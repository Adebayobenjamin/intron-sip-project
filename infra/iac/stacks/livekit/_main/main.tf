########################################################
# LiveKit — our own LiveKit server, in our own account.
#
# WHY THIS STACK EXISTS AT ALL, given Intron already runs LiveKit.
#
# The existing deployment (Calls05, eu-west-2) is a GPU voice worker that dials
# OUT. It has no inbound trunk, no dispatch rule, its SIP proxy trusts a
# hardcoded carrier IP list that does not include our edge, and its SIP path is
# plain UDP with SRTP disabled. Pointing this platform's AI leg at it would
# mean changing all four on a box that is carrying live outbound calls.
#
# So we run our own. The part that makes this cheap is that a LiveKit AGENT IS
# A CLIENT: it opens a WebSocket OUT to whichever server its environment points
# at and waits to be given work. Nothing connects inward to Intron's GPU box.
# Adopting this server is, on their side, a second systemd unit with three
# environment variables changed.
#
#   caller -> our Kamailio -> our livekit-sip -> THIS SERVER
#                                                    ^
#                                                    | outbound WSS
#                                              Intron's agent + GPU (eu-west-2)
#
# WHAT IS DELIBERATELY NOT HERE YET
#   livekit-sip. The first milestone is proving the agent link with a browser
#   and the hosted Agents Playground, which needs no SIP at all. Adding SIP is
#   a second, smaller change on a foundation that is known to work — and it
#   lands in this stack, not a new one.
#
# NO GPU. This box forwards packets; it is an SFU, not a media transcoder. The
# models stay on Intron's T4 in eu-west-2 and nothing here needs to know they
# exist.
########################################################

terraform {
  required_version = ">= 1.10.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

data "aws_caller_identity" "current" {}

locals {
  name_prefix = "${var.environment}-intron-livekit"

  tags = {
    environment  = var.environment
    project      = "intron-sip"
    team         = "platform"
    managed_with = "terraform"
    stack        = "livekit"
  }

  ########################################################
  # Placement. Same AZ index as the SIP core, for the same reason: every RTP
  # packet on the eventual SIP path traverses edge <-> livekit-sip, and
  # cross-AZ traffic is billed per GB in each direction to buy nothing while
  # there is one of everything.
  ########################################################
  primary_az        = local.network.availability_zones[0]
  primary_az_letter = substr(local.primary_az, -1, 1)

  ########################################################
  # THE PUBLIC SUBNET, shared with the SIP edge.
  #
  # LiveKit has to be directly reachable: the browser connects to it, Intron's
  # agent connects to it, and WebRTC media arrives as UDP straight to the
  # instance. A private subnet behind NAT cannot carry that.
  #
  # Reusing the `edge` subnet rather than adding a tier to the network stack is
  # a deliberate trade. The subnet is a routing boundary, not a security one —
  # isolation here comes from the security groups, and this box's group shares
  # no rule with Kamailio's. Adding a `livekit` tier would mean re-applying the
  # network stack to carve new CIDRs out of a VPC that is already carrying
  # calls, which is a much larger blast radius than it sounds.
  ########################################################
  subnet_id   = local.network.subnet_ids["edge-${local.primary_az_letter}"]
  subnet_cidr = local.network.subnet_cidrs["edge-${local.primary_az_letter}"]

  ########################################################
  # Static private addresses, offset to .30 so they cannot collide with the
  # SIP edge's .10+ block in this same subnet.
  #
  # The collision would fail loudly at apply (AWS refuses a duplicate private
  # IP) rather than silently, so this is a convenience rather than a safety
  # net. It exists because the Ansible inventory names this host by private IP
  # and an address that moved on replacement means editing the inventory to
  # recover from a reboot.
  ########################################################
  livekit_private_ips = [for i in range(var.livekit_count) : cidrhost(local.subnet_cidr, 30 + i)]

  # The name Intron's agent, the browser, and eventually our own controller all
  # dial. Assembled once here so the DNS record, the outputs and the Caddy
  # hint in the Ansible inventory cannot drift apart.
  fqdn = "${var.livekit_subdomain}.${var.public_dns_zone}"

  # The second name, same instance, TURN over TLS only. Two names because Caddy
  # demultiplexes 443 on TLS SNI and SNI is a hostname — see the variable.
  turn_fqdn = "${var.livekit_turn_subdomain}.${var.public_dns_zone}"
}

########################################################
# Debian 12, same base as every other box in this repo.
#
# No technical requirement from LiveKit — it ships as containers and would run
# on anything. Consistency is the reason: one bootstrap script, one SSM agent
# install path, one `docker` Ansible role, one set of surprises.
#
# One cost, paid in user_data: the Debian AMI does NOT ship the SSM agent,
# where the Ubuntu images do. See scripts/vm-bootstrap.sh.tftpl.
########################################################
data "aws_ami" "debian" {
  most_recent = true
  owners      = ["136693071363"] # Debian's official AWS account

  filter {
    name   = "name"
    values = ["debian-12-amd64-*"]
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
