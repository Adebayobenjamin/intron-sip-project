variable "environment" {
  type        = string
  description = "Environment name. Prefixes resource names and selects the network stack's state key."
}

variable "aws_region" {
  type        = string
  description = "Region. Must match the network stack - an RDS instance lives in the VPC's region."
}

variable "aws_profile" {
  type        = string
  description = "Shared-config profile for the terraform_remote_state lookup. See stacks/network/beta/variables.tf."
}

variable "state_bucket" {
  type        = string
  description = "S3 bucket holding every stack's state."
}

variable "state_bucket_region" {
  type        = string
  description = "Region of the state bucket."
}

########################################################
# Engine
########################################################

variable "engine_major_version" {
  type        = string
  description = <<-DESC
    Postgres MAJOR version, as a bare number. Also forms the parameter group
    family (postgres<N>).

    Major only, not x.y: AWS picks the current minor at creation and
    auto_minor_version_upgrade keeps it patched. Pinning a full minor pins to
    one that eventually stops being offered, and the result is a confusing plan
    diff months later on a stack nobody has touched.

    16 rather than the newest available. This is the database in the call
    admission path, and a well-worn major with years of extension and driver
    support is worth more here than new features nothing in the control plane
    uses. Raising it is a deliberate exercise with allow_major_version_upgrade,
    not something to drift into.
  DESC
  default     = "16"
}

variable "instance_class" {
  type        = string
  description = <<-DESC
    db.t4g.micro - 2 burstable vCPU, 1 GiB, Graviton.

    Burstable is defensible HERE and was not for the app tier, and the reason
    is the workload shape. This holds configuration: tenants, DIDs, trunks,
    handoff targets. Reads are small point lookups, writes are human-initiated,
    and the API in front of it should be caching. That is a workload that idles
    and accumulates CPU credits rather than spending them.

    IT STOPS BEING DEFENSIBLE if the API does not cache and every inbound
    INVITE becomes a query. At a few calls per second that is still nothing; at
    a few hundred it will exhaust credits and then throttle, which presents as
    call setup latency that degrades over hours and recovers overnight.

    db.t4g.small (2 GiB, ~$27/mo) is the next step, and moving to it is a
    variable change plus a reboot in the maintenance window.
  DESC
  default     = "db.t4g.micro"
}

variable "db_name" {
  type        = string
  description = "Initial database created inside the instance."
  default     = "control_plane"
}

variable "master_username" {
  type        = string
  description = <<-DESC
    Master user.

    NOT "postgres" - that is the first name any credential-stuffing attempt
    tries, and there is no cost to choosing another. The password is generated
    and held by RDS (see manage_master_user_password in rds.tf), never by
    Terraform.

    The application should NOT use this account for day-to-day work. Create a
    least-privilege role in the first migration and give the app that.
  DESC
  default     = "intron_admin"
}

variable "db_port" {
  type        = number
  description = "Postgres port. Also what the security group opens to the app subnets."
  default     = 5432
}

########################################################
# Storage
########################################################

variable "allocated_storage" {
  type        = number
  description = "GB. The gp3 floor - this is configuration data, not call records."
  default     = 20
}

variable "max_allocated_storage" {
  type        = number
  description = <<-DESC
    GB ceiling for RDS storage autoscaling.

    RDS grows the volume automatically and NEVER shrinks it, so this is the
    number that matters and the floor is not. It exists so an unexpected growth
    pattern degrades into a larger bill rather than into a full disk at 3am.
  DESC
  default     = 100
}

########################################################
# Availability and retention
########################################################

variable "multi_az" {
  type        = bool
  description = <<-DESC
    Standby in a second AZ with automatic failover.

    FALSE for beta, and that is a single point of failure for CALL ADMISSION,
    not just for the API - Kamailio fails closed, so a database outage rejects
    every inbound call with 503. See the long note at the top of main.tf.

    Roughly doubles the instance cost. Turn it on when real traffic depends on
    this; the more valuable fix first is an API-side cache, which converts the
    outage into stale-but-working.
  DESC
  default     = false
}

variable "backup_retention_days" {
  type        = number
  description = <<-DESC
    Automated backup retention. Any value above 0 also enables point-in-time
    recovery to any second within the window.

    PITR is the part that matters. The realistic failure for a control-plane
    database is not a lost instance - it is a bad migration or a DELETE without
    a WHERE clause, and only PITR helps with those.
  DESC
  default     = 7
}

variable "deletion_protection" {
  type        = bool
  description = <<-DESC
    Blocks deletion at the AWS API level.

    True means `terraform destroy` on this stack FAILS until someone sets this
    false and applies that change first. Two deliberate steps to delete the
    control plane's data, which is the right amount of friction for the one
    stack here holding state that cannot be rebuilt from code.
  DESC
  default     = true
}

variable "slow_query_log_ms" {
  type        = number
  description = <<-DESC
    Log any statement slower than this, in milliseconds.

    500 is deliberately aggressive for a database serving point lookups. This
    sits in the call-admission path, so a query drifting to 800ms surfaces as
    call setup latency long before it surfaces as an error - and without the
    log there is no record of which query it was.
  DESC
  default     = 500
}
