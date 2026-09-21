########################################################
# Control plane — the app tier.
#
# Two services, co-located on each instance for beta:
#
#   esl-controller   Raw TCP listener on 8022. FreeSWITCH opens a socket PER
#                    CALL and asks "is this routable?" (arrow 3), then keeps it
#                    open for the life of the call under `async full`.
#   control-plane    REST API + Postgres-backed config: tenants, DIDs, trunks,
#                    handoff targets. Queried by Kamailio on inbound INVITE and
#                    by the controller for routing decisions.
#
# Both run as containers from ECR, started by a systemd unit per service. See
# the ecr stack for why images rather than bare binaries.
#
# ONE INSTANCE, NO LOAD BALANCER. This is a small platform and it is built
# like one.
#
# An earlier shape had two instances behind an internal NLB, so a deploy could
# deregister one, drain its ESL sockets, restart, and re-register while the
# other carried calls. That is a real property and it is the only thing the
# second instance bought - so when the count came down to one, the balancer
# had nothing left to do. Failover needs somewhere else to send traffic.
#
# WHAT THAT COSTS, plainly: restarting the controller drops the ESL sockets it
# is holding and those calls end. New calls fail for the restart duration
# either way, because FreeSWITCH's socket application takes a single target and
# does not retry.
#
# WHAT IT REMOVES: a 350s idle-connection reap that AWS does not expose a knob
# for (checked against the live API), security group semantics inverted by
# client-IP preservation, a default that terminates live sockets on a
# health-check flap, a self-deregistration IAM policy, and a drain script. None
# of that complexity was serving one instance.
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
    stack        = "control-plane"
  }

  # Sorted so placement is stable across plans — `keys()` on a map is sorted,
  # but this is built from a comprehension over one, and an unstable order here
  # would reshuffle which instance sits in which subnet on an unrelated change.
  app_subnet_keys = sort([for k, _ in local.network.subnet_ids : k if startswith(k, "app-")])

  # Round-robin across the app subnets. With app_count = 2 and two AZs that is
  # one instance each, which is the point — an AZ failure takes half the tier,
  # not all of it.
  app_placement = [
    for i in range(var.app_count) : {
      subnet_key = local.app_subnet_keys[i % length(local.app_subnet_keys)]
      subnet_id  = local.network.subnet_ids[local.app_subnet_keys[i % length(local.app_subnet_keys)]]
      private_ip = cidrhost(
        local.network.subnet_cidrs[local.app_subnet_keys[i % length(local.app_subnet_keys)]],
        10 + floor(i / length(local.app_subnet_keys))
      )
    }
  ]
}

# Same base image and the same reasoning as sip-core — see the note there.
# Consistency across the fleet is worth more here than picking a marginally
# better base for a tier that only runs Docker.
data "aws_ami" "debian" {
  most_recent = true
  owners      = ["136693071363"]

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
