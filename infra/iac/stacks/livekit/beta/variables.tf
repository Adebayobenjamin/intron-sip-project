variable "aws_region" {
  type        = string
  description = "See stacks/network/beta/variables.tf for why eu-central-1 and not af-south-1."
  default     = "eu-central-1"
}

variable "state_bucket" {
  type        = string
  description = "State bucket, created by the tfstate stack. Read here to configure the remote_state lookups for network, secrets-manager and sip-core."
  default     = "intron-sip-tfstate-eu-central-1"
}

variable "aws_profile" {
  type        = string
  description = <<-DESC
    Shared-config profile Terraform authenticates with, from ~/.aws/credentials.

    Named explicitly in the provider block rather than left to the default
    credential chain, so a bare `terraform plan` in this directory picks the
    same identity a `make plan` does.

    NOTE the two places this value also appears and why they differ:
      - backend "s3"       a LITERAL, because backend blocks cannot
                           interpolate. Override with
                           `terraform init -backend-config=profile=<other>`.
      - terraform_remote_state
                           passed through as a variable. A remote_state data
                           source configures its own backend and inherits
                           NOTHING from the provider block.
  DESC
  default     = "klarity_aws"
}
