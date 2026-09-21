########################################################
# Instance profiles — one per tier.
#
# NOTE FOR ANYONE COMING FROM THE HETZNER SETUP: this is dramatically simpler
# than hybrid activations, and the simplification is worth naming because it
# removes a whole class of failure. There is no aws_ssm_activation, no
# activation code baked into user_data, no time_rotating on a 25-day cadence,
# and no `lifecycle.ignore_changes = [user_data]` to stop that rotation from
# churning running VMs. An EC2 instance assumes its profile through the
# metadata service; the credentials rotate themselves and cannot expire.
#
# The other consequence: Ansible addresses these hosts by `i-...` instance ID,
# not `mi-...`. Same connection plugin, same inventory shape.
#
# SCOPING. Each role reads exactly one Secrets Manager container — its own.
# A compromised FreeSWITCH box cannot read Kamailio's credentials or the
# control plane's database URL. That boundary is the reason the containers are
# split per component rather than being one big blob.
########################################################

locals {
  # EVERY tier that gets an instance role, profile, SSM access, the Ansible
  # transport grant and the CloudWatch agent policy.
  #
  # THIS SET IS NOT ABOUT SECRETS, and conflating the two is a trap worth
  # naming. It used to be a single map of tier -> secret component, and both
  # the role itself and the secrets policy were built from it. Removing a tier
  # because it stopped needing a secret would therefore have destroyed its IAM
  # ROLE AND INSTANCE PROFILE - taking SSM with them, which is the only way in
  # to a box with no SSH key. The instance would have gone unreachable, and
  # Ansible could not have put it back.
  instance_tiers = toset(["edge", "freeswitch"])

  # tier -> the Secrets Manager component it may read. A SUBSET of the above:
  # a tier absent here gets no secrets-read policy and keeps everything else.
  #
  # THE EDGE IS ABSENT ON PURPOSE. Kamailio holds no credentials (admission
  # moved to the controller, originate moved to ESL) and RTPEngine never did,
  # so the edge needs no Secrets Manager access at all.
  #
  # Restoring a Kamailio secret means putting the entry back here AND the
  # unit's ExecStartPre. With the hook but not the policy, the unit fails to
  # start on an AccessDenied raised inside ExecStartPre.
  secret_readers = {
    freeswitch = "freeswitch"
  }
}

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

resource "aws_iam_role" "instance" {
  for_each = local.instance_tiers

  name               = "${local.name_prefix}-${each.key}"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume_role.json

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-${each.key}"
    tier = each.key
  })
}

resource "aws_iam_instance_profile" "instance" {
  for_each = local.instance_tiers

  name = "${local.name_prefix}-${each.key}"
  role = aws_iam_role.instance[each.key].name

  tags = local.tags
}

# Session Manager. This is the ONLY way onto these boxes — there is no SSH key
# anywhere in this repo and no security group rule for port 22. Detaching this
# policy locks everyone out of the instance permanently; the recovery is to
# replace it.
resource "aws_iam_role_policy_attachment" "ssm_core" {
  for_each = local.instance_tiers

  role       = aws_iam_role.instance[each.key].name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

########################################################
# Secrets Manager — each tier reads its own container and nothing else.
#
# Consumed at service start by /usr/local/bin/fetch-and-render-secrets.sh,
# called from the unit's ExecStartPre. Boot-time rather than deploy-time on
# purpose: a rotated credential is picked up by `systemctl restart`, with no
# Ansible run and no rendered secret ever written to the repo, to Ansible's S3
# transport bucket, or to Terraform state.
#
# The trailing "-*" is load-bearing. Secrets Manager appends a random
# six-character suffix to every ARN, and without the wildcard GetSecretValue
# fails with an AccessDenied that names a resource string looking exactly right.
########################################################
resource "aws_iam_role_policy" "secrets_read" {
  for_each = local.secret_readers

  name = "secrets-read"
  role = aws_iam_role.instance[each.key].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "secretsmanager:GetSecretValue",
        "secretsmanager:DescribeSecret",
      ]
      Resource = "${local.secret_arns[each.value]}*"
    }]
  })
}

########################################################
# Ansible's SSM transport bucket. See ansible-transport.tf for what rides
# through it and why bucket-wide rather than prefix-scoped.
########################################################
resource "aws_iam_role_policy" "ansible_transport" {
  for_each = local.instance_tiers

  name = "ansible-ssm-transport"
  role = aws_iam_role.instance[each.key].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = local.ansible_ssm_bucket_actions
      Resource = [aws_s3_bucket.ansible_ssm.arn, "${aws_s3_bucket.ansible_ssm.arn}/*"]
    }]
  })
}

########################################################
# FreeSWITCH only: write call recordings to S3.
#
# Gated on the bucket ARN being supplied, because the recordings bucket is not
# built yet — it belongs to a stack that does not exist until recording is a
# requirement rather than a plan. Wiring the policy now means the day it does
# exist, this is one variable and an apply.
#
# PutObject only. The uploader writes and never reads; a compromised
# FreeSWITCH node should not be able to enumerate or download other tenants'
# call audio, which is the whole risk this scoping exists to contain.
########################################################
resource "aws_iam_role_policy" "freeswitch_recordings_write" {
  count = var.recordings_bucket_arn == "" ? 0 : 1

  name = "recordings-write"
  role = aws_iam_role.instance["freeswitch"].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["s3:PutObject", "s3:AbortMultipartUpload"]
      Resource = "${var.recordings_bucket_arn}/*"
    }]
  })
}

########################################################
# CloudWatch agent — metrics and log shipping from both tiers.
#
# There is no other telemetry path off these boxes: they have no public
# address, nothing scrapes them, and `journalctl` over an SSM session is a
# debugging tool rather than monitoring. Without this the first sign of a
# FreeSWITCH node being out of memory during a source rebuild is calls
# failing.
#
# CloudWatchAgentServerPolicy is broader than ideal (PutMetricData is
# unscopable, and it grants ssm:GetParameter for agent config). Accepted: the
# actions it grants cannot read data or mutate infrastructure.
########################################################
resource "aws_iam_role_policy_attachment" "cloudwatch_agent" {
  for_each = local.instance_tiers

  role       = aws_iam_role.instance[each.key].name
  policy_arn = "arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy"
}
