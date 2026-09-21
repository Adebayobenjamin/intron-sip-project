variable "aws_region" {
  type        = string
  description = "See stacks/network/beta/variables.tf for the region rationale."
  default     = "eu-central-1"
}

variable "aws_profile" {
  type        = string
  description = "Shared-config profile Terraform authenticates with. See stacks/network/beta/variables.tf."
  default     = "klarity_aws"
}

variable "state_bucket" {
  type        = string
  description = "State bucket, created by the tfstate stack."
  default     = "intron-sip-tfstate-eu-central-1"
}
