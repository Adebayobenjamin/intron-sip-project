########################################################
# App-tier instance profile.
#
# Reads TWO Secrets Manager containers, not one, because both services run on
# this box: intron-sip/<env>/esl-controller and intron-sip/<env>/control-plane.
# Co-location is a beta decision — when they split onto separate tiers, this
# splits into two roles and each keeps one container.
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

resource "aws_iam_role" "app" {
  name               = "${local.name_prefix}-app"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume_role.json

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-app"
    tier = "app"
  })
}

resource "aws_iam_instance_profile" "app" {
  name = "${local.name_prefix}-app"
  role = aws_iam_role.app.name

  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "app_ssm_core" {
  role       = aws_iam_role.app.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy_attachment" "app_cloudwatch_agent" {
  role       = aws_iam_role.app.name
  policy_arn = "arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy"
}

resource "aws_iam_role_policy" "app_secrets_read" {
  name = "secrets-read"
  role = aws_iam_role.app.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "secretsmanager:GetSecretValue",
        "secretsmanager:DescribeSecret",
      ]
      # Trailing "*" matches the random six-character suffix Secrets Manager
      # appends to every ARN. Without it, GetSecretValue fails with an
      # AccessDenied naming a resource string that looks exactly correct.
      Resource = [
        "${local.secret_arns["esl-controller"]}*",
        "${local.secret_arns["control-plane"]}*",
      ]
    }]
  })
}

########################################################
# ECR pull.
#
# TWO STATEMENTS, and the split is not cosmetic. GetAuthorizationToken has no
# resource to scope to — it is an account-level call that mints a docker login
# token — so it must sit at "*". Everything that actually reads image data is
# scoped to this project's repositories, so a compromised app instance cannot
# enumerate or pull images from any other repository in the account.
#
# Collapsing both into one "*" statement is the common shortcut and it throws
# that boundary away for one fewer block.
########################################################
resource "aws_iam_role_policy" "app_ecr_pull" {
  name = "ecr-pull"
  role = aws_iam_role.app.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "EcrAuthToken"
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      },
      {
        Sid    = "EcrPullProjectRepos"
        Effect = "Allow"
        Action = [
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer",
          "ecr:BatchCheckLayerAvailability",
          "ecr:DescribeImages",
        ]
        Resource = values(local.ecr.repository_arns)
      },
    ]
  })
}

########################################################
# Ansible's SSM transport bucket, owned by sip-core.
#
# These instances are Ansible-managed too — the systemd units, the env-file
# templates and the Docker install all come from a play — so they need the same
# access the SIP boxes have.
########################################################
resource "aws_iam_role_policy" "app_ansible_transport" {
  name = "ansible-ssm-transport"
  role = aws_iam_role.app.id

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

########################################################
# NO LOAD-BALANCER PERMISSIONS.
#
# This role previously carried elasticloadbalancing:Register/DeregisterTargets
# scoped to two target groups, so the box could drain itself before a restart.
# With no NLB there is nothing to deregister from, and the policy is gone
# rather than left inert - a permission that grants nothing is still a
# permission someone has to reason about later.
########################################################
