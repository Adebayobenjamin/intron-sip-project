########################################################
# Reads only. This stack creates nothing anything else depends on, which is
# what makes it safe to destroy at any time.
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

# For the edge's Elastic IP. The security group admits that /32 and nothing
# else on the SIP port, which is both the access control and the thing that
# forces the handoff leg onto the public path - see main.tf.
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
}
