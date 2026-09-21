########################################################
# S3 bucket — Ansible SSM connection-plugin transport.
#
# `community.aws.aws_ssm` is the connection plugin every playbook in this repo
# runs over. It uses SSM SendCommand for control flow, but module payloads,
# rendered templates and fetch-back results ride through this bucket: SSM
# command parameters cap at ~100 KB, which cannot fit a Python module.
#
# WHAT IS IN IT AT ANY MOMENT
# Seconds-to-minutes of Ansible operational scratch — module Python, and
# rendered configs carrying ${VAR} PLACEHOLDERS ONLY. Resolved secrets never
# pass through here; they go Secrets Manager -> VM directly at service start
# (fetch-and-render-secrets.sh). That is the reason the templates are deployed
# as .tpl files and substituted on the box rather than rendered by Jinja with
# real values.
#
# WHY IT LIVES IN sip-core rather than its own stack: sip-core is applied
# before control-plane and is the stack with the most Ansible-managed hosts.
# control-plane reads the name out of this stack's remote state and grants its
# own role the same access. If that ever needs to change, the bucket name is
# operator-shaped (no service prefix) so a `terraform state mv` hoists it out
# without renaming anything.
########################################################

resource "aws_s3_bucket" "ansible_ssm" {
  bucket = "intron-ansible-ssm-${var.environment}-${var.aws_region}"

  # The bucket only ever holds ephemeral scratch. The lifecycle rule below
  # purges most of it, but interrupted plays leave stragglers — force_destroy
  # lets `terraform destroy` empty and delete in one shot instead of failing
  # with BucketNotEmpty and needing a separate `aws s3 rm --recursive`.
  force_destroy = true

  tags = merge(local.tags, {
    Name      = "intron-ansible-ssm-${var.environment}"
    component = "ansible-ssm-transport"
  })
}

resource "aws_s3_bucket_public_access_block" "ansible_ssm" {
  bucket = aws_s3_bucket.ansible_ssm.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# SSE-S3 (AES256), not KMS. The plugin makes hundreds of small Put/Get/Delete
# calls per playbook run; KMS adds a per-call round trip and a per-call charge
# to every one of them, and there are no resolved secrets in here to justify
# key-level access control.
resource "aws_s3_bucket_server_side_encryption_configuration" "ansible_ssm" {
  bucket = aws_s3_bucket.ansible_ssm.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Objects here are write-once, read-once, delete-on-cleanup. Versions would
# be pure noise and pure cost.
resource "aws_s3_bucket_versioning" "ansible_ssm" {
  bucket = aws_s3_bucket.ansible_ssm.id

  versioning_configuration {
    status = "Disabled"
  }
}

# Belt and braces. The plugin deletes its own files at task end; interrupted
# runs (SIGKILL, dropped network, operator ctrl-C) leak. Seven days is well
# past the longest expected run — the FreeSWITCH source build is the outlier
# at around twenty minutes.
resource "aws_s3_bucket_lifecycle_configuration" "ansible_ssm" {
  bucket = aws_s3_bucket.ansible_ssm.id

  rule {
    id     = "purge-orphans"
    status = "Enabled"

    filter {}

    expiration {
      days = 7
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

locals {
  # Bucket-wide rather than prefix-scoped. The plugin writes under a
  # per-invocation UUID prefix, and pinning the policy to that layout would
  # couple this stack to an implementation detail the upstream collection is
  # free to change — the symptom of which is every playbook failing at the
  # first task with an S3 AccessDenied.
  ansible_ssm_bucket_actions = [
    "s3:GetObject",
    "s3:PutObject",
    "s3:DeleteObject",
    "s3:ListBucket",
    "s3:GetBucketLocation",
  ]
}
