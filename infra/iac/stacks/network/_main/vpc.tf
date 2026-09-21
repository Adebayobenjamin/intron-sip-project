########################################################
# VPC + subnets + routing
########################################################

resource "aws_vpc" "main" {
  cidr_block = var.vpc_cidr

  # Both required for the private Route53 zone the control-plane stack creates
  # (esl.<env>.intron.internal). Without enable_dns_hostnames a private hosted
  # zone associates fine and then resolves nothing from inside the VPC, which
  # surfaces as FreeSWITCH failing to open its &socket() with a DNS error and
  # every inbound call dropping at answer.
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-vpc"
  })
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-igw"
  })
}

resource "aws_subnet" "this" {
  for_each = local.subnets

  vpc_id            = aws_vpc.main.id
  cidr_block        = each.value.cidr_block
  availability_zone = each.value.az

  # Deliberately false even on the public subnets. The edge instances get an
  # Elastic IP explicitly in the sip-core stack, because a carrier allowlist is
  # pinned to that address — an auto-assigned public IP changes on every stop/
  # start, and the failure is an ITSP silently rejecting INVITEs from an
  # address it does not recognise. Making it explicit means a box can never
  # acquire a public address by accident.
  map_public_ip_on_launch = false

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-${each.key}"
    tier = each.value.tier
  })
}

########################################################
# Public routing — one table shared by every public subnet.
########################################################
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-public"
  })
}

resource "aws_route" "public_default" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.main.id
}

resource "aws_route_table_association" "public" {
  for_each = { for k, v in local.subnets : k => v if v.public }

  subnet_id      = aws_subnet.this[each.key].id
  route_table_id = aws_route_table.public.id
}

########################################################
# Private routing — ONE TABLE PER AZ, not one shared table.
#
# Each private table's default route points at the NAT gateway in its own AZ
# when there is one. With a single NAT (the beta default) every table points at
# the same gateway and this looks like pointless duplication; it stops looking
# that way the moment nat_gateway_count goes to 2, because then the tables are
# already the right shape and adding HA is a variable change rather than a
# routing rewrite.
#
# The AZ-local pairing matters for cost as well as availability: cross-AZ
# traffic to a NAT gateway is billed on top of the NAT's own per-GB processing.
########################################################
resource "aws_route_table" "private" {
  for_each = toset(local.azs)

  vpc_id = aws_vpc.main.id

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-private-${substr(each.value, -1, 1)}"
  })
}

resource "aws_route_table_association" "private" {
  for_each = { for k, v in local.subnets : k => v if !v.public }

  subnet_id      = aws_subnet.this[each.key].id
  route_table_id = aws_route_table.private[each.value.az].id
}
