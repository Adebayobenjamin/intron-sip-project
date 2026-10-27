variable "aws_region" {
  type        = string
  description = "See stacks/network/beta/variables.tf for why eu-west-1 and not af-south-1."
  default     = "eu-west-1"
}

variable "state_bucket" {
  type        = string
  description = "State bucket, created by the tfstate stack. Read here to configure the remote_state lookups for network and secrets-manager."
  default     = "intron-sip-tfstate-eu-west-1"
}

variable "carrier_sip_cidrs" {
  type        = list(string)
  description = <<-DESC
    Supplied at plan time from the iac Secrets Manager container as
    TF_VAR_carrier_sip_cidrs, so carrier ranges can change without a commit.

    Defaults to empty. See the long note in ../_main/variables.tf — the short
    version is that empty means no calls arrive, and 0.0.0.0/0 means scanners
    arrive too.
  DESC
  default     = []
}

variable "livekit_cidrs" {
  type        = list(string)
  description = "Also from the iac container, as TF_VAR_livekit_cidrs. Empty until the LiveKit location is confirmed."
  default     = []
}

variable "aws_profile" {
  type        = string
  description = <<-DESC
    Shared-config profile Terraform authenticates with, from ~/.aws/credentials.

    Named explicitly in the provider block rather than left to the default
    credential chain, so a bare `terraform plan` in this directory picks the
    same identity a `make plan` does. An auth model that only works through the
    Makefile breaks the first time someone debugs a stack directly — and it
    fails by reaching for whatever AWS_PROFILE happens to be set, which on a
    laptop with several accounts configured is how changes land in the wrong
    one.

    NOTE the two places this value also appears and why they differ:
      - backend "s3"       a LITERAL, because backend blocks cannot
                           interpolate. Override with
                           `terraform init -backend-config=profile=<other>`.
      - terraform_remote_state
                           passed through as a variable. A remote_state data
                           source configures its own backend and inherits
                           NOTHING from the provider block.

    The profile carries no region (see ~/.aws/config), which is fine for
    Terraform — region comes from var.aws_region and from the backend literal —
    but means any bare `aws` CLI call needs AWS_REGION set. The Makefile
    exports it.
  DESC
  default     = "intron_aws"
}
