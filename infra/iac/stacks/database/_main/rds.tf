########################################################
# Subnet group — the data tier, across both AZs.
#
# RDS REQUIRES TWO AZs in a subnet group even for a single-AZ instance. That is
# why the network stack builds `data-a` and `data-b` and leaves them empty:
# discovering the requirement at database-creation time means going back to
# reshape the VPC.
########################################################
resource "aws_db_subnet_group" "main" {
  name        = "${local.name_prefix}-db"
  description = "Control-plane Postgres. Data tier, no route to the internet."
  subnet_ids  = local.data_subnet_ids

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-db"
  })
}

########################################################
# Security group.
#
# ONE INGRESS RULE, from the app subnets, on 5432. Nothing else reaches this:
# not the edge, not the FreeSWITCH farm, not a laptop. The data subnets have no
# route to an internet gateway, and `publicly_accessible = false` below means
# RDS does not give it a public endpoint either.
#
# To run a migration or a psql session, go through an app instance over SSM.
# That is deliberate friction: a database reachable from anywhere convenient is
# reachable from anywhere.
########################################################
resource "aws_security_group" "db" {
  name        = "${local.name_prefix}-db"
  description = "Control-plane Postgres. Reachable from the app tier only."
  vpc_id      = local.network.vpc_id

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-db"
    tier = "data"
  })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "db_from_app" {
  for_each = toset(local.app_subnet_cidrs)

  security_group_id = aws_security_group.db.id
  description       = "Postgres from the app tier (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.db_port
  to_port           = var.db_port
  ip_protocol       = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-db-app" })
}

# No egress rules beyond this. RDS is a destination; it does not initiate
# connections anywhere this platform cares about. Declaring a security group
# without inline egress makes Terraform strip AWS's default allow-all, which is
# the behaviour wanted here.
resource "aws_vpc_security_group_egress_rule" "db_none" {
  security_group_id = aws_security_group.db.id
  description       = "Explicitly none. RDS does not initiate outbound connections."
  cidr_ipv4         = "127.0.0.1/32"
  ip_protocol       = "-1"

  tags = merge(local.tags, { Name = "${local.name_prefix}-db-egress" })
}

########################################################
# Parameter group.
#
# Exists for ONE setting worth having from day one: slow-query logging. This
# database sits in the call-admission path, so a query that starts taking 800ms
# shows up as call setup latency long before it shows up as an error, and
# without this there is no record of which query it was.
#
# A custom parameter group also means later changes do not require creating one
# under time pressure — several parameters are static and need a reboot, which
# is a worse thing to discover mid-incident.
########################################################
resource "aws_db_parameter_group" "main" {
  name        = "${local.name_prefix}-pg${var.engine_major_version}"
  family      = "postgres${var.engine_major_version}"
  description = "Control-plane Postgres ${var.engine_major_version}"

  parameter {
    name  = "log_min_duration_statement"
    value = tostring(var.slow_query_log_ms)
  }

  # Log every connection attempt that fails. Cheap, and the first thing wanted
  # when the app cannot reach the database is whether it is being refused or
  # never arriving at all.
  parameter {
    name  = "log_connections"
    value = "1"
  }

  parameter {
    name  = "log_disconnections"
    value = "1"
  }

  tags = local.tags

  # The name carries the major version, so a major upgrade means a new group.
  # create_before_destroy lets that happen without a window where the instance
  # references a group that is being deleted.
  lifecycle {
    create_before_destroy = true
  }
}

########################################################
# The instance.
########################################################
resource "aws_db_instance" "main" {
  identifier = "${local.name_prefix}-db"

  engine = "postgres"
  # MAJOR VERSION ONLY. With allow_major_version_upgrade left false, AWS picks
  # the current minor at creation and auto_minor_version_upgrade keeps it
  # patched inside the maintenance window. Pinning a full x.y here means
  # pinning to a minor that stops being available and produces a confusing
  # plan diff months later.
  engine_version              = var.engine_major_version
  allow_major_version_upgrade = false
  auto_minor_version_upgrade  = true

  instance_class = var.instance_class
  db_name        = var.db_name
  port           = var.db_port

  ########################################################
  # THE PASSWORD IS NOT IN TERRAFORM STATE.
  #
  # manage_master_user_password hands the credential to RDS, which generates it
  # and stores it in its own Secrets Manager secret. Terraform never sees the
  # value, so it never lands in state — including in every historical version
  # sitting in the state bucket.
  #
  # The alternative (a `password` argument fed from a variable) puts the
  # plaintext in state permanently, which contradicts the rule the rest of this
  # repo follows: Terraform owns the secret CONTAINER, never the value.
  #
  # CONSEQUENCE: the application does not read this secret directly. An
  # operator composes a DSN from it once with bin/db-dsn-to-sm, which writes
  # DATABASE_URL into intron-sip/<env>/control-plane. The app then reads one
  # container, as it already does for everything else, and needs no extra IAM
  # grant or code path. Re-run that helper if the password is ever rotated.
  ########################################################
  manage_master_user_password = true
  username                    = var.master_username

  ########################################################
  # Storage. gp3 at the 20 GB floor — this is configuration data (tenants,
  # DIDs, trunks), not call records, and it will not grow on its own.
  #
  # max_allocated_storage enables autoscaling so an unexpected growth pattern
  # does not become an outage at 3am. RDS grows the volume and never shrinks
  # it, so the ceiling is the thing that matters, not the floor.
  ########################################################
  storage_type          = "gp3"
  allocated_storage     = var.allocated_storage
  max_allocated_storage = var.max_allocated_storage
  storage_encrypted     = true

  db_subnet_group_name   = aws_db_subnet_group.main.name
  vpc_security_group_ids = [aws_security_group.db.id]
  parameter_group_name   = aws_db_parameter_group.main.name

  # No public endpoint, and the data subnets have no route to an internet
  # gateway either. Two independent reasons this is unreachable from outside
  # the VPC, which is the right number for a database.
  publicly_accessible = false

  ########################################################
  # Availability. Single-AZ for beta - see the long note in main.tf about this
  # being in the call-admission path, and why an API-side cache matters more
  # than doubling the instance cost.
  ########################################################
  multi_az = var.multi_az

  ########################################################
  # Backups.
  #
  # 7 days of automated backups also enables point-in-time recovery to any
  # second in that window, which is the thing that actually saves you: the
  # realistic failure here is not a lost instance, it is a bad migration or a
  # DELETE without a WHERE clause.
  #
  # Windows are in UTC and chosen for Lagos (WAT, UTC+1): backup at 02:00-03:00
  # UTC is 03:00-04:00 local, and maintenance follows it on Sunday. Both sit
  # well outside any plausible call volume.
  ########################################################
  backup_retention_period = var.backup_retention_days
  backup_window           = "02:00-03:00"
  maintenance_window      = "sun:03:30-sun:04:30"
  copy_tags_to_snapshot   = true

  ########################################################
  # Deletion protection ON, and a final snapshot on the way out.
  #
  # Means `terraform destroy` on this stack FAILS until someone sets
  # deletion_protection = false and applies that first. Two deliberate steps to
  # delete the control plane's data, which is the correct amount of friction.
  ########################################################
  deletion_protection       = var.deletion_protection
  skip_final_snapshot       = false
  final_snapshot_identifier = "${local.name_prefix}-db-final-${var.environment}"

  ########################################################
  # Performance Insights, 7-day retention. FREE at that retention, and it is
  # the difference between "the API got slow" and knowing which query did it.
  # Turning it on later requires a reboot on some instance classes; turning it
  # on now costs nothing.
  ########################################################
  performance_insights_enabled          = true
  performance_insights_retention_period = 7

  # postgresql = the server log, where log_min_duration_statement output lands.
  # upgrade = major/minor upgrade logs, useful when auto minor upgrade does
  # something unexpected in the maintenance window.
  enabled_cloudwatch_logs_exports = ["postgresql", "upgrade"]

  # Apply changes in the maintenance window rather than immediately. A
  # parameter change that forces a reboot should not do it the moment someone
  # runs apply - especially while this sits in the call-admission path.
  apply_immediately = false

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-db"
  })

  lifecycle {
    ignore_changes = [
      # AWS picks the current minor at creation and bumps it in the maintenance
      # window. Without this, every plan after an auto minor upgrade shows a
      # spurious diff trying to put it back.
      engine_version,
      # RDS rotates the managed secret on its own schedule. The ARN is stable;
      # the version is not, and Terraform should not chase it.
      master_user_secret_kms_key_id,
    ]
  }
}
