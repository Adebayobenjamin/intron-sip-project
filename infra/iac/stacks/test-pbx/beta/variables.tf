variable "aws_region" {
  type        = string
  description = "See stacks/network/beta/variables.tf for why eu-central-1."
  default     = "eu-central-1"
}

variable "state_bucket" {
  type        = string
  description = "State bucket, created by the tfstate stack. Read here to configure the remote_state lookups for network and sip-core."
  default     = "intron-sip-tfstate-eu-central-1"
}

variable "aws_profile" {
  type        = string
  description = <<-DESC
    Shared-config profile Terraform authenticates with.

    Named explicitly rather than left to the default credential chain, so a
    bare `terraform plan` in this directory picks the same identity a
    `make plan` does. Also passed to terraform_remote_state, which configures
    its own backend and inherits nothing from the provider block.
  DESC
  default     = "klarity_aws"
}

variable "operator_cidr" {
  type        = string
  description = <<-DESC
    Where Linphone registers from - your laptop's public address. Comma-separate
    for more than one.

      make apply STACK=test-pbx ENV=beta \
        EXTRA='-var=operator_cidr=203.0.113.7/32'

    Find yours with `curl -s ifconfig.me`.

    A STRING, NOT A list(string), AND THAT IS ON PURPOSE. The Makefile expands
    $(EXTRA) unquoted into the recipe, so /bin/sh strips the double quotes out
    of a JSON list before Terraform sees it:

      EXTRA='-var=operator_cidrs=["203.0.113.7/32"]'
        -> terraform apply -var=operator_cidrs=[203.0.113.7/32]
        -> Error: Invalid number literal

    because Terraform then tries to parse 203.0.113.7 as a number. Escaping
    the quotes (\") works and is the kind of thing nobody gets right twice.
    This variable is typed by hand at a prompt, so it takes the shape a human
    can type; beta/main.tf splits it into the list the module wants.

    Contrast carrier_sip_cidrs, which IS a JSON list - it lives in .env and
    reaches Terraform through Secrets Manager as an environment variable,
    never through a shell.

    EMPTY IS THE DEFAULT and means Linphone cannot register, which is the safe
    failure: an extension anyone can register to is a toll-fraud target even on
    a throwaway box.
  DESC
  default     = ""
}
