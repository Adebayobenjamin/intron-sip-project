########################################################
# SIP core — the Kamailio/RTPEngine edge and the FreeSWITCH farm.
#
# Maps to the "Platform SIP core" box in the architecture diagram. Two tiers,
# and the split between them is the load-bearing idea of the whole design:
#
#   EDGE (public subnet, Elastic IP)
#     Kamailio + RTPEngine on one box. The only thing carriers ever talk to.
#     Kamailio does auth, tenant routing and dispatch; RTPEngine relays media
#     so the FreeSWITCH nodes never appear in an SDP.
#
#   FARM (private subnet, no public address at all)
#     FreeSWITCH as a dumb B2BUA — media anchor, REFER termination, CDRs.
#     Reachable only through the edge. There is no inbound path from the
#     internet, which is the point: a FreeSWITCH with a public IP is a
#     toll-fraud target within hours of booting.
#
# WHAT IS DELIBERATELY NOT HERE
#   The ESL controller and the control-plane API. They are the app tier and
#   live in stacks/control-plane, behind an internal NLB. FreeSWITCH reaches
#   them by DNS name, so a controller redeploy never touches this stack.
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
  name_prefix = "${var.environment}-intron-sip"

  tags = {
    environment  = var.environment
    project      = "intron-sip"
    team         = "platform"
    managed_with = "terraform"
    stack        = "sip-core"
  }

  ########################################################
  # AZ pinning. BOTH TIERS GO IN THE SAME AZ, index 0.
  #
  # Not for latency — inter-AZ RTT inside a region is under a millisecond and
  # irrelevant next to the 80-100ms Lagos-to-Dublin leg. For cost. Every RTP
  # packet on this platform traverses edge <-> farm, and cross-AZ traffic is
  # billed at $0.01/GB in EACH direction. A single concurrent G.711 call is
  # ~64 kbit/s each way before overhead; at a few hundred concurrent calls
  # running all day that is a line item, paid continuously, to buy nothing —
  # because losing the AZ takes out the edge and the farm together anyway
  # while there is one of each.
  #
  # This becomes a real decision when the farm grows past one node and the
  # edge gets a standby. At that point spread the FARM across AZs and keep
  # each edge pinned to one, rather than spreading everything evenly.
  ########################################################
  primary_az        = local.network.availability_zones[0]
  primary_az_letter = substr(local.primary_az, -1, 1)

  edge_subnet_id = local.network.subnet_ids["edge-${local.primary_az_letter}"]
  sip_subnet_id  = local.network.subnet_ids["sip-${local.primary_az_letter}"]

  edge_subnet_cidr = local.network.subnet_cidrs["edge-${local.primary_az_letter}"]
  sip_subnet_cidr  = local.network.subnet_cidrs["sip-${local.primary_az_letter}"]

  ########################################################
  # Static private addresses, derived from the subnet rather than written out.
  #
  # Static because the Ansible inventory names these hosts by IP and the
  # Kamailio dispatcher list is rendered from the same values — an address that
  # moved on instance replacement would mean editing two files and re-running a
  # play to recover from a reboot.
  #
  # cidrhost() rather than literals so changing vpc_cidr in the network stack
  # cannot leave this file pointing at addresses outside the subnet. AWS
  # reserves the first four addresses and the last in every subnet, hence
  # starting at .10 rather than .1.
  ########################################################
  edge_private_ips       = [for i in range(var.edge_count) : cidrhost(local.edge_subnet_cidr, 10 + i)]
  freeswitch_private_ips = [for i in range(var.freeswitch_count) : cidrhost(local.sip_subnet_cidr, 20 + i)]
}

########################################################
# Debian 12.
#
# Same base as the proven Ansible roles this repo's FreeSWITCH build is derived
# from. FreeSWITCH 1.10 is built from source (SignalWire gated their Debian
# packages behind a token), and the toolchain in Debian 12 is the one that
# build is known to complete on. Ubuntu 24.04 ships a newer GCC and OpenSSL 3
# combination that the 1.10 tree needs patching for — not a fight worth having
# on the box that carries the calls.
#
# One cost, paid in user_data: the Debian AMI does NOT ship the SSM agent,
# where the Ubuntu AMIs do. See scripts/vm-bootstrap.sh.
#
# x86_64 rather than Graviton. FreeSWITCH and RTPEngine both build on arm64 and
# c7g would be roughly 20% cheaper, but RTPEngine's kernel forwarding module is
# the unknown and this is not the environment to find out on. Revisit when the
# platform is boring.
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
