########################################################
# Apply BEFORE control-plane.
#
# Not a Terraform dependency - the two are coupled at runtime by a DSN in a
# secret, not at apply time - but applying it first means the app tier comes up
# with a database to talk to rather than crash-looping on a missing
# DATABASE_URL.
########################################################

module "database" {
  source = "../_main"

  environment         = "beta"
  aws_region          = var.aws_region
  aws_profile         = var.aws_profile
  state_bucket        = var.state_bucket
  state_bucket_region = var.aws_region

  engine_major_version = "16"
  instance_class       = "db.t4g.micro"

  # Configuration data, not call records. 20 GB floor, autoscaling to 100 so an
  # unexpected growth pattern becomes a bigger bill and not a full disk.
  allocated_storage     = 20
  max_allocated_storage = 100

  ########################################################
  # Single-AZ for beta. This is a single point of failure for CALL ADMISSION,
  # because Kamailio fails closed - a database outage rejects every inbound
  # call with 503, not just the API.
  #
  # Accepted deliberately. The cheaper and more valuable fix is an API-side
  # cache of tenant/DID/plan data, which turns the outage into
  # stale-but-working. multi_az doubles the instance cost and is the right call
  # once real traffic depends on this.
  ########################################################
  multi_az = false

  # 7 days of backups, which also buys point-in-time recovery - the thing that
  # actually helps with the realistic failure here, a bad migration.
  backup_retention_days = 7

  # `terraform destroy` fails until this is flipped false and applied. Correct
  # friction for the one stack holding state that cannot be rebuilt from code.
  deletion_protection = true
}
