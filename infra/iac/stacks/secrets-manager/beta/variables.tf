variable "aws_region" {
  type        = string
  description = "Region. Must match the other stacks — a Secrets Manager container is regional and a cross-region GetSecretValue just fails."
  default     = "eu-west-1"
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
