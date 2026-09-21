########################################################
# Only the network stack. This stack deliberately does NOT read control-plane:
#
#   control-plane -> database would make the app tier un-appliable until the
#   database exists, and database -> control-plane would be a cycle. Neither is
#   needed, because the two are coupled at RUNTIME (a DSN in a secret), not at
#   apply time.
#
# The security group rule that lets the app reach Postgres is written against
# the app subnet CIDRs, which come from the network stack — so the dependency
# graph stays a line: database -> network.
########################################################

data "terraform_remote_state" "network" {
  backend = "s3"

  config = {
    bucket  = var.state_bucket
    key     = "stacks/network/${var.environment}/terraform.tfstate"
    region  = var.state_bucket_region
    profile = var.aws_profile
  }
}

locals {
  network = data.terraform_remote_state.network.outputs
}
