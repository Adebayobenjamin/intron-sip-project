########################################################
# App tier security group.
#
# ONE GROUP, because there is no load balancer in front. FreeSWITCH and
# Kamailio connect straight to the instance, so the rules name the real
# sources and mean exactly what they say.
#
# That is a simplification worth noting, because the NLB version of this file
# had a trap in it: with client-IP preservation on, an instance's security
# group is evaluated against the ORIGINAL source rather than the balancer, so
# "allow from the NLB's group" - the instinct from ALB setups - produced a
# target group that never went healthy and traffic that never arrived, with
# nothing logged on either side. No balancer, no trap.
########################################################

resource "aws_security_group" "app" {
  ########################################################
  # name_prefix, NOT name - and the two cannot be swapped freely.
  #
  # Security group names are unique per VPC, and create_before_destroy means
  # Terraform builds the replacement while the original still exists. With a
  # fixed `name` those two rules collide and the apply fails with
  # InvalidGroup.Duplicate naming a group Terraform is itself about to delete.
  #
  # That is not hypothetical: it happened here, triggered by nothing more than
  # editing this group's description. A description change forces replacement,
  # replacement runs create-before-destroy, and the create hits the name that
  # is still in use.
  #
  # name_prefix lets AWS append a unique suffix, so the two can coexist for the
  # seconds it takes to move the rules across. The cost is a
  # non-deterministic name in the console; the `Name` tag below carries the
  # readable one, which is what the console actually displays.
  #
  # create_before_destroy itself is load-bearing: the rules are separate
  # resources and the instance references this group, so a destroy-first
  # replacement would need the instance to briefly have no security group,
  # which AWS does not allow.
  ########################################################
  name_prefix = "${local.name_prefix}-app-"
  description = "ESL controller + control-plane API. Private, no load balancer."
  vpc_id      = local.network.vpc_id

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-app"
    tier = "app"
  })

  lifecycle {
    create_before_destroy = true
  }
}

########################################################
# Outbound ESL - arrow 3. FreeSWITCH opens a socket here per call and holds it
# for the life of the call.
#
# Referenced by security group, not CIDR: both stacks are in one VPC and
# sip-core exports its group id, so this is the precise form. The reverse
# direction (FreeSWITCH's ESL port, 8021) is opened over there by app subnet
# CIDR instead, because referencing THIS group from that stack would make the
# two stacks depend on each other.
########################################################
resource "aws_vpc_security_group_ingress_rule" "app_esl_from_freeswitch" {
  security_group_id            = aws_security_group.app.id
  description                  = "Outbound-ESL sockets from the FreeSWITCH farm"
  referenced_security_group_id = local.sip_core.freeswitch_security_group_id
  from_port                    = var.esl_listener_port
  to_port                      = var.esl_listener_port
  ip_protocol                  = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-app-esl-fs" })
}

# Kamailio also opens a socket for the outbound originate path.
resource "aws_vpc_security_group_ingress_rule" "app_esl_from_edge" {
  security_group_id            = aws_security_group.app.id
  description                  = "ESL sockets from the Kamailio edge (originate path)"
  referenced_security_group_id = local.sip_core.edge_security_group_id
  from_port                    = var.esl_listener_port
  to_port                      = var.esl_listener_port
  ip_protocol                  = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-app-esl-edge" })
}

# Kamailio's admission lookup on every inbound INVITE.
resource "aws_vpc_security_group_ingress_rule" "app_api_from_edge" {
  security_group_id            = aws_security_group.app.id
  description                  = "Routing lookups from Kamailio"
  referenced_security_group_id = local.sip_core.edge_security_group_id
  from_port                    = var.api_listener_port
  to_port                      = var.api_listener_port
  ip_protocol                  = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-app-api-edge" })
}

# The ESL controller calls the API. Same box today, so this is loopback and
# needs no rule at all - but it is here because with app_count > 1 the
# controller on one node may reach the API on another, and discovering that
# the hard way means debugging a timeout rather than reading a rule.
resource "aws_vpc_security_group_ingress_rule" "app_api_from_app" {
  security_group_id            = aws_security_group.app.id
  description                  = "API, from the controller on a peer app instance"
  referenced_security_group_id = aws_security_group.app.id
  from_port                    = var.api_listener_port
  to_port                      = var.api_listener_port
  ip_protocol                  = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-app-api-app" })
}

########################################################
# NO RULE FOR THE HEALTH PORT, deliberately.
#
# Nothing external health-checks this any more - that was the load balancer's
# job. The Ansible role checks it over loopback (127.0.0.1) after a deploy, so
# the port stays exposed by the container and closed at the security group.
#
# If monitoring is added later and needs to scrape it, that is when this rule
# appears - scoped to whatever does the scraping, not to the VPC.
########################################################

resource "aws_vpc_security_group_egress_rule" "app_all" {
  security_group_id = aws_security_group.app.id
  description       = "All outbound: ECR pulls, Secrets Manager, SSM, inbound-ESL to FreeSWITCH on 8021, Postgres, and the LiveKit API"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"

  tags = merge(local.tags, { Name = "${local.name_prefix}-app-egress" })
}
