########################################################
# Upstream stacks. The dependency runs one way and only one way:
#
#   control-plane -> sip-core -> network
#                 -> secrets-manager
#                 -> ecr
#
# sip-core deliberately does NOT read this stack. Its FreeSWITCH security group
# opens the ESL port to the app SUBNET CIDRs rather than to this stack's
# security group, precisely so the two do not form a cycle. See the note at the
# top of sip-core/_main/security-groups.tf.
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

data "terraform_remote_state" "sip_core" {
  backend = "s3"

  config = {
    bucket  = var.state_bucket
    key     = "stacks/sip-core/${var.environment}/terraform.tfstate"
    region  = var.state_bucket_region
    profile = var.aws_profile
  }
}

data "terraform_remote_state" "ecr" {
  backend = "s3"

  config = {
    bucket  = var.state_bucket
    key     = "stacks/ecr/${var.environment}/terraform.tfstate"
    region  = var.state_bucket_region
    profile = var.aws_profile
  }
}

locals {
  network  = data.terraform_remote_state.network.outputs
  sip_core = data.terraform_remote_state.sip_core.outputs
  ecr      = data.terraform_remote_state.ecr.outputs

  secret_arns = data.terraform_remote_state.secrets.outputs.secret_arns
}
