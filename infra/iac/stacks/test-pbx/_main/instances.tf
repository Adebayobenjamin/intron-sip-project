resource "aws_instance" "test_pbx" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = var.instance_type
  subnet_id     = local.subnet_id

  vpc_security_group_ids = [aws_security_group.test_pbx.id]
  iam_instance_profile   = aws_iam_instance_profile.test_pbx.name

  # SSM only, like every other box in this repo. No key pair exists to attach.
  key_name = null

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required" # IMDSv2
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    volume_size = var.root_volume_size
    volume_type = "gp3"
    encrypted   = true

    tags = merge(local.tags, { Name = "${local.name_prefix}-root" })
  }

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-1"
    tier = "test-pbx"
  })

  # NO prevent_destroy, deliberately - this stack is meant to be torn down.
  # Every other instance in this repo carries one.
}

########################################################
# Elastic IP.
#
# Needed for two reasons, and the second is the one that matters:
#
#   1. Linphone has to reach a stable address to register.
#   2. It is what makes this box EXTERNAL to the edge. The edge addresses it by
#      this IP, so the traffic leaves through the internet gateway and returns,
#      and the media path crosses a NAT boundary exactly as it would to a real
#      client SBC.
#
# Asterisk cannot discover this address - the OS only ever sees the private
# one, same as Kamailio and RTPEngine - so it is passed into the Ansible role
# as external_media_address / external_signaling_address. Without those,
# Asterisk puts the private IP in its SDP and the call connects with no audio.
########################################################
resource "aws_eip" "test_pbx" {
  domain = "vpc"

  tags = merge(local.tags, { Name = "${local.name_prefix}-1" })
}

resource "aws_eip_association" "test_pbx" {
  instance_id   = aws_instance.test_pbx.id
  allocation_id = aws_eip.test_pbx.id
}
