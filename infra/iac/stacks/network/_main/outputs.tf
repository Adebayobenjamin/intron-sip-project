########################################################
# Consumed by sip-core and control-plane via terraform_remote_state.
#
# Subnet IDs are exposed BOTH as a per-tier map and as flat per-tier lists.
# The map is keyed "<tier>-<az-letter>" and is what you want when placement
# matters (pinning the edge and the FreeSWITCH farm to one AZ); the lists are
# what an RDS subnet group or a load balancer wants.
########################################################

output "vpc_id" {
  description = "VPC every stack attaches to."
  value       = aws_vpc.main.id
}

output "vpc_cidr" {
  description = "VPC CIDR. Used downstream for intra-VPC security group rules that are genuinely VPC-wide."
  value       = aws_vpc.main.cidr_block
}

output "availability_zones" {
  description = "AZs in the order this stack assigned them. Index 0 is the AZ the media path is pinned to."
  value       = local.azs
}

output "subnet_ids" {
  description = "All subnets, keyed <tier>-<az-letter> (e.g. edge-a, sip-a). Use this when AZ placement matters."
  value       = { for k, v in aws_subnet.this : k => v.id }
}

output "edge_subnet_ids" {
  description = "Public subnets. Kamailio + RTPEngine, and the NAT gateways."
  value       = [for k, v in local.subnets : aws_subnet.this[k].id if v.tier == "edge"]
}

output "app_subnet_ids" {
  description = "Private subnets for the ESL controller, the control-plane API, and the internal NLB."
  value       = [for k, v in local.subnets : aws_subnet.this[k].id if v.tier == "app"]
}

output "sip_subnet_ids" {
  description = "Private subnets for the FreeSWITCH farm."
  value       = [for k, v in local.subnets : aws_subnet.this[k].id if v.tier == "sip"]
}

output "data_subnet_ids" {
  description = "Private subnets reserved for RDS / ElastiCache. Empty of instances today."
  value       = [for k, v in local.subnets : aws_subnet.this[k].id if v.tier == "data"]
}

output "subnet_cidrs" {
  description = "Subnet CIDRs keyed the same as subnet_ids. Handy for a security group rule that has to name a range rather than a peer group."
  value       = { for k, v in aws_subnet.this : k => v.cidr_block }
}

output "nat_public_ips" {
  description = <<-DESC
    Elastic IPs the NAT gateways egress from.

    Worth knowing even though no SIP leaves this way: anything in the private
    tiers that calls an external API Intron has allowlisted — a carrier's REST
    API, an LLM provider — appears as one of these addresses.
  DESC
  value       = aws_eip.nat[*].public_ip
}

output "private_route_table_ids" {
  description = "Per-AZ private route tables, keyed by AZ name. A future peering or Transit Gateway attachment to the LiveKit network adds its routes here."
  value       = { for az, rt in aws_route_table.private : az => rt.id }
}
