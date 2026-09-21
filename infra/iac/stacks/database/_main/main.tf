########################################################
# Control-plane database.
#
# The Postgres behind the REST API in the architecture diagram: tenants, DIDs,
# trunks, handoff targets, config sync. Small, config-shaped data — not call
# records, not media, not CDRs at volume.
#
# SEPARATE STACK FROM control-plane, DELIBERATELY. A database outlives the
# application in front of it, and the two want different blast radii: tearing
# down and rebuilding the app tier is routine, and it must not be able to take
# the data with it. Keeping them apart means `make destroy STACK=control-plane`
# cannot touch this.
#
# READ THIS BEFORE SIZING ANYTHING DOWN: Kamailio's admission route fails
# CLOSED. If the control-plane API cannot answer, every inbound INVITE is
# rejected with 503 — see route[ADMISSION] in the Kamailio config, and the
# reasoning there (dispatching an unadmitted call means FreeSWITCH answers a
# number that may belong to no tenant, which on a carrier-connected platform is
# how toll fraud gets paid for).
#
# So this database is in the call-admission path, and a single-AZ instance is a
# single point of failure for the whole platform. Two mitigations, in order of
# value:
#
#   1. THE API MUST CACHE. Tenant/DID/plan data changes on human timescales and
#      is read on every call. An API that holds a warm in-memory cache with a
#      sane TTL turns a database outage into stale-but-working rather than
#      total rejection. That is an application decision and it is the one that
#      actually matters.
#   2. multi_az = true, which roughly doubles the cost and gives automatic
#      failover in 1-2 minutes. Worth it once real traffic depends on this;
#      it is a one-variable change.
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

locals {
  name_prefix = "${var.environment}-intron-sip"

  tags = {
    environment  = var.environment
    project      = "intron-sip"
    team         = "platform"
    managed_with = "terraform"
    stack        = "database"
  }

  # The data tier, built empty by the network stack for exactly this.
  data_subnet_ids = local.network.data_subnet_ids

  # Cross-stack rule by CIDR rather than by security group, the same way the
  # FreeSWITCH ESL port is opened. Referencing control-plane's SG would create
  # a cycle, and the app subnets contain the control plane and nothing else.
  app_subnet_cidrs = [
    for k, v in local.network.subnet_cidrs : v if startswith(k, "app-")
  ]
}
