terraform {
  required_version = ">= 1.10.0"

  backend "s3" {
    bucket       = "intron-sip-tfstate-eu-west-1"
    key          = "stacks/database/beta/terraform.tfstate"
    region       = "eu-west-1"
    encrypt      = true
    use_lockfile = true
    # Literal, not var.aws_profile: a backend block cannot
    # interpolate. Override per-invocation with
    # `terraform init -backend-config=profile=<other>`.
    profile = "intron_aws"
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

  default_tags {
    tags = {
      project      = "intron-sip"
      environment  = "beta"
      managed_with = "terraform"
    }
  }
}
