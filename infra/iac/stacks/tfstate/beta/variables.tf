variable "aws_region" {
  type        = string
  description = "Region the state bucket lives in. Must match the `region` in every other stack's backend block."
  default     = "eu-central-1"
}

variable "environment" {
  type        = string
  description = "Environment name. One environment today (beta); the bucket is shared if more are added, separated by key prefix."
  default     = "beta"
}

variable "state_bucket_name" {
  type        = string
  description = <<-DESC
    Globally-unique S3 bucket name for Terraform state.

    Not env-suffixed: one bucket holds every environment, separated by the
    `key` prefix in each stack's backend block (stacks/<stack>/<env>/...).
    A per-env bucket would mean a second bootstrap for every new environment
    and buys nothing — the blast radius of the bucket is the account either way.
  DESC
  default     = "intron-sip-tfstate-eu-central-1"
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

    THIS STACK IS THE SIMPLE CASE: it keeps state locally (it creates the
    bucket the others use), so there is no backend block and no remote_state
    lookup. The provider block is the only consumer.

    Every other stack has two more places the profile has to be named — a
    LITERAL in backend "s3" because backend blocks cannot interpolate, and a
    pass-through into terraform_remote_state because a remote_state data source
    configures its own backend and inherits nothing from the provider. See
    ../../network/beta/variables.tf.

    The profile carries no region (see ~/.aws/config), which is fine for
    Terraform — region comes from var.aws_region — but means any bare `aws` CLI
    call needs AWS_REGION set. The Makefile exports it.
  DESC
  default     = "klarity_aws"
}
