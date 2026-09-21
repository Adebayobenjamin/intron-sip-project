########################################################
# Upstream stacks, read rather than duplicated.
#
# The alternative — passing thirty subnet IDs and secret ARNs down as variables
# from the env wrapper — means the wrapper goes stale silently every time the
# network changes. Remote state re-reads on every plan.
#
# Both backends are configured from variables because this module is
# instantiated per environment and the key differs. The bucket and region do
# not, but hardcoding them here would mean editing the module to move
# environments to a different account.
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

data "terraform_remote_state" "secrets" {
  backend = "s3"

  config = {
    bucket  = var.state_bucket
    key     = "stacks/secrets-manager/${var.environment}/terraform.tfstate"
    region  = var.state_bucket_region
    profile = var.aws_profile
  }
}

locals {
  network = data.terraform_remote_state.network.outputs

  # Keyed by component — see stacks/secrets-manager/_main/main.tf.
  # Read through this rather than building ARNs by hand: Secrets Manager
  # appends a random six-character suffix to every ARN, so a hand-written
  # "arn:...:secret:intron-sip/beta/kamailio" matches nothing and the failure
  # is an AccessDenied naming a resource that looks entirely correct.
  secret_arns = data.terraform_remote_state.secrets.outputs.secret_arns
}
