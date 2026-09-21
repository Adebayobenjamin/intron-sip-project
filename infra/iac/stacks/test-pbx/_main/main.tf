########################################################
# TEST PBX — a stand-in for a client's contact-centre SBC.
#
# THIS IS A TEST RIG AND IS BUILT LIKE ONE. It exists to be the far end of a
# human handoff so that leg can be exercised before a real client SBC is
# available: FreeSWITCH bridges out, Kamailio's route[EGRESS] relays, and this
# box rings a Linphone softphone.
#
# It is DISPOSABLE ON PURPOSE. Nothing here carries prevent_destroy, nothing
# else depends on its outputs, and `make destroy STACK=test-pbx ENV=beta`
# should always be safe. Tear it down when the real SBC arrives.
#
# ---------------------------------------------------------------------------
# WHY IT IS REACHED OVER THE PUBLIC INTERNET, despite living in this VPC
#
# The instance sits in an edge (public) subnet with its own Elastic IP, and the
# edge reaches it by that PUBLIC address. Traffic therefore leaves through the
# internet gateway and comes back, and this box sees the edge's Elastic IP as
# the source — exactly what a real client SBC would see.
#
# That is the entire point. A test target on a private address would not
# exercise the thing most likely to be wrong: RTPEngine advertising
# {{ public }} in the SDP while the media actually has to traverse a NAT
# boundary. Put this on 10.70.x and the handoff leg "works" in testing and
# fails the first time a real SBC is on the other end.
#
# The security group rule below therefore matches the edge's PUBLIC /32, NOT
# the edge security group. Referencing the SG would silently route the traffic
# privately and defeat the exercise.
# ---------------------------------------------------------------------------
#
# UBUNTU, NOT DEBIAN, and that is forced rather than chosen: Asterisk was
# dropped from Debian 12 (bookworm), so `apt install asterisk` has no candidate
# there. Ubuntu carries it in universe. Every other box in this repo is Debian;
# this one is the exception and the reason is packaging, not preference.
#
# Ubuntu's AMIs also ship the SSM agent, so this stack needs no bootstrap
# script to install it the way the Debian boxes do.
########################################################

data "aws_caller_identity" "current" {}

locals {
  name_prefix = "${var.environment}-intron-test-pbx"

  tags = {
    stack     = "test-pbx"
    component = "test-pbx"
    # Loud on purpose. Anyone auditing spend or resources should be able to see
    # at a glance that this is scaffolding rather than platform.
    lifecycle_note = "disposable test rig - safe to destroy"
  }

  # First edge subnet. Public, so the instance can hold an Elastic IP and be
  # reached from outside the VPC.
  subnet_id = local.network.edge_subnet_ids[0]

  # The edge's Elastic IP, as a /32. This is what the security group admits and
  # what forces the traffic onto the public path - see the header.
  edge_public_cidr = "${local.sip_core.edge_public_ips[0]}/32"
}

########################################################
# Ubuntu 24.04 LTS, Canonical's official account.
########################################################
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }

  filter {
    name   = "root-device-type"
    values = ["ebs"]
  }
}
