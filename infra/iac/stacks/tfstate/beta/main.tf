########################################################
# Terraform state backend — bootstrap.
#
# DO NOT CREATE THIS BUCKET BY HAND. It is not just a bucket: it carries
# versioning (the only thing that makes a truncated state file recoverable), a
# lifecycle rule, SSE, Block Public Access and a TLS-only bucket policy. A
# hand-made bucket has none of those, nothing records that it should, and the
# omission is invisible until the day you need to roll state back and find
# there are no previous versions.
#
# THE CHICKEN AND EGG, AND HOW IT IS RESOLVED.
# This stack creates the bucket every other stack stores state in, so at the
# moment it first runs there is nowhere to put its own state. It therefore
# applies against a LOCAL backend — and then adopts the bucket it just made:
#
#   # 1. Create the bucket. State lands in ./terraform.tfstate.
#   cd infra/iac/stacks/tfstate/beta
#   terraform init && terraform apply
#
#   # 2. Uncomment the backend block below, then migrate into it.
#   terraform init -migrate-state      # answers "yes" to copy existing state
#
#   # 3. The local file is now a copy, not the source of truth. Delete it.
#   rm -f terraform.tfstate terraform.tfstate.backup
#
# After step 2 this stack is stored in the bucket it manages, like every other
# stack, and there is no local state file to lose or to remember not to commit.
# Step 2 is safe because the bucket exists by then; running it before step 1 is
# the only ordering that fails.
#
# The self-reference is fine in normal operation and dangerous in exactly one:
# `terraform destroy` here would try to delete the bucket holding its own
# state. `prevent_destroy = true` on the bucket below is what stops that.
#
# No DynamoDB lock table. Terraform 1.10+ supports S3-native locking via
# `use_lockfile = true` — a .tflock object next to the state, written with an
# S3 conditional put. One less resource, one less thing to pay for, and a stale
# lock is cleared with `terraform force-unlock` exactly as it was with DynamoDB.
#
# There is no Makefile target for this stack: the Makefile wraps every other
# stack in aws-env-run, which reads the iac secret — and that does not exist
# yet either. Credentials come from the klarity_aws profile named in the
# provider block below, so nothing needs to be exported first.
########################################################

terraform {
  required_version = ">= 1.10.0"

  ########################################################
  # UNCOMMENT AFTER THE FIRST APPLY, then `terraform init -migrate-state`.
  # See the sequence in the header above.
  #
  # `key` deliberately follows the same stacks/<stack>/<env> convention as
  # everything else — this stack is not special once it has a home.
  ########################################################
  backend "s3" {
    bucket       = "intron-sip-tfstate-eu-central-1"
    key          = "stacks/tfstate/beta/terraform.tfstate"
    region       = "eu-central-1"
    encrypt      = true
    use_lockfile = true
    profile      = "klarity_aws"
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
    tags = local.tags
  }
}

locals {
  tags = {
    environment  = var.environment
    project      = "intron-sip"
    managed_with = "terraform"
    stack        = "tfstate"
  }
}

resource "aws_s3_bucket" "tfstate" {
  bucket = var.state_bucket_name

  # No force_destroy. Losing this bucket means losing the record of every
  # resource in the account — the one place where making destruction
  # inconvenient is the correct call.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_public_access_block" "tfstate" {
  bucket = aws_s3_bucket.tfstate.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Versioning is what makes a corrupted or truncated state recoverable — restore
# the previous object version and carry on. Non-negotiable for a state bucket.
resource "aws_s3_bucket_versioning" "tfstate" {
  bucket = aws_s3_bucket.tfstate.id

  versioning_configuration {
    status = "Enabled"
  }
}

# SSE-S3 rather than KMS. State files DO contain sensitive values (any resource
# attribute marked sensitive is still stored in cleartext in state), so this is
# not "nothing worth encrypting" — it is that KMS buys key-level access control
# we are not going to use, at the cost of a second permission surface that
# breaks `terraform init` in confusing ways when it drifts. Switch to a CMK
# when there is a real reason to gate state reads separately from bucket reads.
resource "aws_s3_bucket_server_side_encryption_configuration" "tfstate" {
  bucket = aws_s3_bucket.tfstate.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
    bucket_key_enabled = true
  }
}

# Old state versions accumulate forever otherwise — one object version per
# apply, per stack. 90 days is well past any realistic "roll back to last
# week's state" window, and the objects are small enough that the cost is
# rounding error either way.
resource "aws_s3_bucket_lifecycle_configuration" "tfstate" {
  bucket = aws_s3_bucket.tfstate.id

  rule {
    id     = "expire-noncurrent-state"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = 90
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

# Reject any write that isn't TLS. S3 is HTTPS by default and nothing here
# speaks plaintext, but this closes the door rather than trusting every future
# client to pick the right endpoint.
resource "aws_s3_bucket_policy" "tfstate_tls_only" {
  bucket = aws_s3_bucket.tfstate.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DenyInsecureTransport"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource = [
        aws_s3_bucket.tfstate.arn,
        "${aws_s3_bucket.tfstate.arn}/*",
      ]
      Condition = {
        Bool = { "aws:SecureTransport" = "false" }
      }
    }]
  })
}
