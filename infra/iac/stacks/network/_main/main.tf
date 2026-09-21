########################################################
# Network — the one VPC everything else lands in.
#
# Owns: VPC, subnets, IGW, NAT, route tables, VPC endpoints.
# Owns nothing with an instance in it — sip-core and control-plane look this
# stack up through remote state and place their own resources.
#
# WHY ONE VPC AND NOT ONE PER TIER
# --------------------------------
# Media. RTP between the Kamailio/RTPEngine edge and the FreeSWITCH farm is the
# bulk of the traffic on this platform, and anything that puts a VPC boundary
# between them turns that into peering or Transit Gateway traffic — billed per
# GB in both directions, on top of the per-AZ charge. One VPC, one AZ for the
# media path, and the only per-GB charge is the one AWS cannot avoid.
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

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  name_prefix = "${var.environment}-intron-sip"

  tags = {
    environment  = var.environment
    project      = "intron-sip"
    team         = "platform"
    managed_with = "terraform"
    stack        = "network"
  }

  # First N AZs in the region, in the order AWS returns them. Pinned into
  # outputs so downstream stacks agree on which AZ is "index 0" — the media
  # path depends on the edge and the FreeSWITCH farm landing in the SAME AZ,
  # and an AZ list that reordered between applies would quietly break that.
  azs = slice(data.aws_availability_zones.available.names, 0, var.az_count)

  ########################################################
  # Subnet layout. netnum == third octet, deliberately: reading 10.70.20.0/24
  # off a host and knowing it is a FreeSWITCH box is worth more than the
  # density a tighter packing would buy. A /16 has room for 256 of these.
  #
  #   edge   0,  1   public   Kamailio + RTPEngine. Needs an EIP: SIP signalling
  #                           and RTP both carry the public address in-band, so
  #                           this tier cannot sit behind NAT.
  #   app   10, 11   private  ESL controller + control-plane API, and the
  #                           internal NLB that fronts the ESL listener.
  #   sip   20, 21   private  FreeSWITCH farm. No public address at all —
  #                           every packet in reaches it via the edge.
  #   data  30, 31   private  RDS / ElastiCache. Empty today; the control plane's
  #                           Postgres lands here, not in `app`, so a database
  #                           subnet group exists before it is needed.
  ########################################################
  subnet_tiers = {
    edge = { netnum_base = 0, public = true }
    app  = { netnum_base = 10, public = false }
    sip  = { netnum_base = 20, public = false }
    data = { netnum_base = 30, public = false }
  }

  # Flattened tier × AZ product — the shape for_each wants.
  subnets = merge([
    for tier, cfg in local.subnet_tiers : {
      for idx, az in local.azs :
      "${tier}-${substr(az, -1, 1)}" => {
        tier       = tier
        az         = az
        az_index   = idx
        public     = cfg.public
        cidr_block = cidrsubnet(var.vpc_cidr, 8, cfg.netnum_base + idx)
      }
    }
  ]...)
}
