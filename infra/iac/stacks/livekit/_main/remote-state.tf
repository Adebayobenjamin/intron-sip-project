########################################################
# Upstream stacks, read rather than duplicated.
#
# Same shape as sip-core and control-plane — see the note in
# ../../sip-core/_main/remote-state.tf for why remote state rather than
# variables passed down from the env wrapper.
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

########################################################
# sip-core, for the Ansible SSM transport bucket ONLY.
#
# That bucket lives in sip-core because it was the first stack with
# Ansible-managed hosts (see the note in its ansible-transport.tf). This stack
# grants its own instance role the same access rather than creating a second
# bucket — the alternative is two buckets with the same lifecycle rules and a
# 50% chance of configuring the connection plugin against the wrong one.
#
# THE ORDERING CONSEQUENCE: sip-core must be applied before this stack. That
# is already true of the rollout order and is asserted here only by this
# lookup failing with a message naming the key.
########################################################
data "terraform_remote_state" "sip_core" {
  backend = "s3"

  config = {
    bucket  = var.state_bucket
    key     = "stacks/sip-core/${var.environment}/terraform.tfstate"
    region  = var.state_bucket_region
    profile = var.aws_profile
  }
}

locals {
  network  = data.terraform_remote_state.network.outputs
  sip_core = data.terraform_remote_state.sip_core.outputs

  # Keyed by component — see stacks/secrets-manager/_main/main.tf. Read through
  # this rather than building ARNs by hand: Secrets Manager appends a random
  # six-character suffix to every ARN, so a hand-written
  # "arn:...:secret:intron-sip/beta/livekit" matches nothing and the failure is
  # an AccessDenied naming a resource that looks entirely correct.
  secret_arns = data.terraform_remote_state.secrets.outputs.secret_arns
}
