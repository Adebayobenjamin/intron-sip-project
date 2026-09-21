########################################################
# VPC endpoints
########################################################

########################################################
# S3 — GATEWAY endpoint. Free, and it pays for itself immediately.
#
# Three things in this platform pull large objects from S3 over what would
# otherwise be the NAT gateway, at $0.045/GB of NAT processing plus egress:
#
#   - ECR image layers. ECR stores layers in S3; `docker pull` on the app tier
#     fetches them from an S3 endpoint, not from the ECR API. A few hundred MB
#     per deploy, per instance.
#   - Ansible's SSM connection plugin, which ships every module payload and
#     rendered template through its transport bucket.
#   - FreeSWITCH call recordings on the way out, once that path exists.
#
# A gateway endpoint is a route-table entry, not an ENI — no hourly charge, no
# per-GB charge, nothing to size. There is no argument for not having it.
########################################################
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.main.id
  service_name      = "com.amazonaws.${var.aws_region}.s3"
  vpc_endpoint_type = "Gateway"

  # Every private table, plus the public one. The public subnets reach S3 via
  # the IGW perfectly well, but routing them through the endpoint keeps the
  # traffic off the edge instances' Elastic IPs — which matters because those
  # addresses are on carrier allowlists and their egress profile should stay
  # boring and predictable.
  route_table_ids = concat(
    [for rt in aws_route_table.private : rt.id],
    [aws_route_table.public.id],
  )

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-s3"
  })
}

########################################################
# SSM interface endpoints — OFF by default.
#
# SSM is the only way into these boxes (no SSH, no port 22 anywhere), so the
# agent's reachability is load-bearing: lose it and you cannot get a shell on a
# FreeSWITCH node to find out why it is not taking calls.
#
# With a NAT gateway present the agent reaches SSM over it and these endpoints
# are pure duplication, at 3 endpoints × $0.01/hr × AZ ≈ $22/month for two AZs.
# They earn that back in exactly one situation, and it is a situation worth
# naming: the NAT gateway's AZ fails, private egress dies, and the boxes you
# need to debug drop off SSM at the same moment. Interface endpoints are
# per-AZ ENIs and keep working.
#
# So: leave this false while a single NAT is acceptable, and flip it together
# with nat_gateway_count when it stops being. Turning it on later is a plan and
# an apply — nothing on the instances changes, because the agent resolves the
# same hostnames either way and private DNS silently redirects them.
#
# All three are required. ssmmessages carries the Session Manager data channel
# and ec2messages the older command channel; enabling only `ssm` produces an
# agent that registers and then cannot hold a session, which reads as a
# network problem rather than a missing endpoint.
########################################################
locals {
  ssm_endpoint_services = var.enable_ssm_interface_endpoints ? toset([
    "ssm",
    "ssmmessages",
    "ec2messages",
  ]) : toset([])
}

resource "aws_security_group" "vpc_endpoints" {
  count = var.enable_ssm_interface_endpoints ? 1 : 0

  name        = "${local.name_prefix}-vpc-endpoints"
  description = "HTTPS from inside the VPC to the SSM interface endpoints"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTPS from anything in the VPC"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  # No egress rules: an endpoint ENI is a destination, never an initiator.
  # Terraform's default is to remove the implicit allow-all egress once any
  # rule set is declared, which is the behaviour we want here.
  egress {
    description = "Explicitly none"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["127.0.0.1/32"]
  }

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-vpc-endpoints"
  })
}

resource "aws_vpc_endpoint" "ssm" {
  for_each = local.ssm_endpoint_services

  vpc_id            = aws_vpc.main.id
  service_name      = "com.amazonaws.${var.aws_region}.${each.value}"
  vpc_endpoint_type = "Interface"

  # app + sip tiers only. The edge tier has a public route and does not need
  # an ENI in every subnet it happens to occupy.
  subnet_ids = [
    for k, v in local.subnets : aws_subnet.this[k].id
    if contains(["app", "sip"], v.tier)
  ]

  security_group_ids = [aws_security_group.vpc_endpoints[0].id]

  # The whole point: the agent keeps calling ssm.<region>.amazonaws.com and
  # Route53 resolves it to the ENI instead of the public address. Nothing on
  # the instance is reconfigured.
  private_dns_enabled = true

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-${each.value}"
  })
}
