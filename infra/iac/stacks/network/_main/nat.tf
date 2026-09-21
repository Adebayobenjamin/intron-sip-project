########################################################
# NAT — outbound internet for the private tiers.
#
# WHO NEEDS IT
#   FreeSWITCH: builds from source (git clone + apt + a ~20 min compile), and
#               pulls its SSM agent and AWS CLI the same way.
#   app tier:   docker pull from ECR, and the boot-time Secrets Manager fetch.
#
# Neither needs inbound from the internet, and neither gets it — a NAT gateway
# is one-way by construction.
#
# ONE GATEWAY BY DEFAULT, and that is a real single point of failure: lose the
# AZ holding it and every private subnet loses egress. What survives that is
# the part that matters — calls in progress do not traverse NAT, because SIP
# and RTP both enter through the edge tier and stay inside the VPC. What breaks
# is `docker pull` on a restart, the boot-time secret fetch, and Ansible's SSM
# transport. So a NAT outage blocks deploys and recovery, not traffic.
#
# Set nat_gateway_count = var.az_count in an environment where deploys during
# an AZ failure matter. At ~$0.045/hr plus per-GB, the second one is roughly
# $33/month for that property.
########################################################

resource "aws_eip" "nat" {
  count  = var.nat_gateway_count
  domain = "vpc"

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-nat-${count.index + 1}"
  })
}

resource "aws_nat_gateway" "main" {
  count = var.nat_gateway_count

  allocation_id = aws_eip.nat[count.index].id
  # NAT lives in the PUBLIC subnet of its AZ — it needs a route to the IGW to
  # do its job. Putting it in a private subnet is the classic mistake here and
  # produces a gateway that creates successfully and routes nothing.
  subnet_id = aws_subnet.this["edge-${substr(local.azs[count.index], -1, 1)}"].id

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-nat-${count.index + 1}"
  })

  depends_on = [aws_internet_gateway.main]
}

# Default route out of each private table. index % count means: with one NAT,
# every AZ shares it; with one per AZ, each table gets its own. No branch, and
# no route to rewrite when the count changes.
resource "aws_route" "private_default" {
  for_each = { for idx, az in local.azs : az => idx }

  route_table_id         = aws_route_table.private[each.key].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.main[each.value % var.nat_gateway_count].id
}
