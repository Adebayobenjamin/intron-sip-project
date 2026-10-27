terraform {
  required_version = ">= 1.10.0"

  # use_lockfile is S3-native state locking (Terraform >= 1.10) — a .tflock
  # object written with a conditional put, no DynamoDB table involved. On an
  # older CLI this parses and silently does nothing, so check
  # `terraform version` before trusting concurrent applies not to race.
  backend "s3" {
    bucket = "intron-sip-tfstate-eu-west-1"
    key    = "stacks/network/beta/terraform.tfstate"
    region = "eu-west-1"
    # Literal, not var.aws_profile: a backend block cannot
    # interpolate. Override per-invocation with
    # `terraform init -backend-config=profile=<other>`.
    profile      = "intron_aws"
    encrypt      = true
    use_lockfile = true
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region  = var.aws_region
  profile = var.aws_profile

  # Applied to every taggable resource in the stack. Per-resource tags in
  # _main merge on top of these rather than replacing them.
  default_tags {
    tags = {
      project      = "intron-sip"
      environment  = "beta"
      managed_with = "terraform"
    }
  }
}
