########################################################
# Instance role. Two grants and no more:
#
#   SSM core          the only way in - no SSH key, like every other box here
#   ansible transport the S3 bucket the aws_ssm connection plugin rides through
#
# NO SECRETS-READ. The agent's SIP password is the only credential this box
# needs, and it is rendered by Ansible rather than fetched at service start.
# That is a deliberate departure from the platform tiers: those fetch at boot
# so a rotated credential is picked up by `systemctl restart`, which matters
# for something carrying real calls. This is a test rig whose password is
# rotated by destroying it.
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

resource "aws_iam_role" "test_pbx" {
  name               = local.name_prefix
  assume_role_policy = data.aws_iam_policy_document.ec2_assume_role.json

  tags = merge(local.tags, { Name = local.name_prefix })
}

resource "aws_iam_instance_profile" "test_pbx" {
  name = local.name_prefix
  role = aws_iam_role.test_pbx.name

  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "ssm_core" {
  role       = aws_iam_role.test_pbx.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "ansible_transport" {
  name = "ansible-ssm-transport"
  role = aws_iam_role.test_pbx.id

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
