########################################################
# Instance role.
#
# Three grants, and nothing else:
#
#   SSM Session Manager        the only way onto this box
#   Secrets Manager            its own container, intron-sip/<env>/livekit
#   the Ansible SSM bucket     module payloads and rendered templates
#
# Scoped to one secret container deliberately. The API secret on this box
# signs every token that grants access to a room; a role that could also read
# the database URL or the ESL password would turn one compromised container
# into a platform-wide incident.
########################################################

data "aws_iam_policy_document" "ec2_assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "livekit" {
  name               = "${local.name_prefix}-server"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume_role.json

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-server"
    tier = "livekit"
  })
}

resource "aws_iam_instance_profile" "livekit" {
  name = "${local.name_prefix}-server"
  role = aws_iam_role.livekit.name

  tags = local.tags
}

# Session Manager. This is the ONLY way onto this box — there is no SSH key in
# this repo and no port 22 rule in the security group. Detaching this policy
# locks everyone out permanently; the recovery is to replace the instance.
resource "aws_iam_role_policy_attachment" "ssm_core" {
  role       = aws_iam_role.livekit.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

########################################################
# Secrets Manager - this box's own container only.
#
# Consumed at service start by /usr/local/bin/fetch-and-render-secrets.sh from
# the unit's ExecStartPre, so a rotated API secret is picked up by `systemctl
# restart` with no Ansible run and nothing resolved ever reaches this repo,
# the S3 transport bucket, or Terraform state.
#
# The trailing "*" is load-bearing. Secrets Manager appends a random
# six-character suffix to every ARN; without it GetSecretValue fails with an
# AccessDenied naming a resource string that looks exactly correct.
########################################################
resource "aws_iam_role_policy" "secrets_read" {
  name = "secrets-read"
  role = aws_iam_role.livekit.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "secretsmanager:GetSecretValue",
        "secretsmanager:DescribeSecret",
      ]
      Resource = "${local.secret_arns["livekit"]}*"
    }]
  })
}

########################################################
# Ansible SSM transport bucket, owned by sip-core.
#
# The aws_ssm connection plugin ships module payloads and rendered templates
# through S3 because SSM command parameters cap around 100 KB, which cannot
# fit a Python module. Without this grant every play against this host fails
# on the first task with an S3 AccessDenied, which reads as a broken
# connection plugin rather than a missing policy.
########################################################
resource "aws_iam_role_policy" "ansible_transport" {
  name = "ansible-ssm-transport"
  role = aws_iam_role.livekit.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:ListBucket",
        "s3:GetBucketLocation",
      ]
      Resource = [
        local.sip_core.ansible_ssm_bucket_arn,
        "${local.sip_core.ansible_ssm_bucket_arn}/*",
      ]
    }]
  })
}
