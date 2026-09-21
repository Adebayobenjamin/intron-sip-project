resource "aws_security_group" "test_pbx" {
  name        = local.name_prefix
  description = "Disposable Asterisk test PBX. Stands in for a client contact-centre SBC."
  vpc_id      = local.network.vpc_id

  tags = merge(local.tags, { Name = local.name_prefix })

  lifecycle {
    create_before_destroy = true
  }
}

########################################################
# SIP from the edge, BY PUBLIC ADDRESS.
#
# cidr_ipv4 on the edge's Elastic IP rather than a referenced_security_group_id
# on the edge SG, and the difference is the whole point of this stack.
#
# An SG reference would match traffic arriving on the PRIVATE path - the two
# boxes are in the same VPC, so that path exists and is the one AWS would use
# if the edge addressed this box by its private IP. The handoff would then work
# in testing without ever crossing a NAT boundary, and the first real client
# SBC would expose every address-rewriting bug at once.
#
# Matching the public /32 means the only traffic admitted is traffic that went
# out through the internet gateway and came back, which is what a real SBC
# sees.
########################################################
resource "aws_vpc_security_group_ingress_rule" "sip_from_edge" {
  security_group_id = aws_security_group.test_pbx.id
  description       = "SIP from the Intron edge, over its public address"
  cidr_ipv4         = local.edge_public_cidr
  from_port         = var.sip_port
  to_port           = var.sip_port
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-sip-edge" })
}

########################################################
# SIP from the operator, so Linphone can REGISTER.
#
# Separate from the rule above because it is a different thing with a different
# risk: the edge rule admits calls, this one admits registration, and an
# extension that anyone can register to is a toll-fraud target.
#
# for_each over a list that DEFAULTS TO EMPTY. No operator CIDR means Linphone
# cannot register, which is the correct default - see the variable.
########################################################
resource "aws_vpc_security_group_ingress_rule" "sip_from_operator" {
  for_each = toset(var.operator_cidrs)

  security_group_id = aws_security_group.test_pbx.id
  description       = "SIP registration from the operator (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.sip_port
  to_port           = var.sip_port
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-sip-operator" })
}

########################################################
# SIP over TCP, from the operator only.
#
# NOT A SECOND WAY IN FOR THE EDGE - the edge's rule above is UDP and stays
# that way. This exists because the operator's network drops outbound UDP to
# 5060 entirely: a capture on this box logged other UDP traffic while three
# probes from the operator's machine produced zero packets. Blocking SIP on
# 5060/udp is ordinary on consumer and mobile ISPs.
#
# Same port, different protocol, and the blanket blocks are usually written
# for UDP. If this is dropped too, the answer is TLS on 5061, which a
# middlebox cannot distinguish from HTTPS.
#
# Scoped to operator_cidrs for the same reason the UDP registration rule is:
# an extension anyone can register to is a toll-fraud target, and adding a
# transport must not quietly widen who can reach it.
########################################################
resource "aws_vpc_security_group_ingress_rule" "sip_tcp_from_operator" {
  for_each = toset(var.operator_cidrs)

  security_group_id = aws_security_group.test_pbx.id
  description       = "SIP registration over TCP from the operator (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.sip_port
  to_port           = var.sip_port
  ip_protocol       = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-sip-tcp-operator" })
}

########################################################
# RTP, and this one is 0.0.0.0/0 for the same reason the edge's is.
#
# Media does not arrive from the signalling address. The caller's audio is
# relayed by RTPEngine on the edge, the agent's comes from wherever Linphone
# happens to be - a laptop on a phone network, behind carrier-grade NAT, with
# an address nobody can enumerate in advance. Pinning this produces one-way
# audio that reproduces intermittently.
#
# The range is the control: 10000-20000 and nothing else, and Asterisk only
# forwards on ports it allocated for a call it knows about.
########################################################
resource "aws_vpc_security_group_ingress_rule" "rtp" {
  security_group_id = aws_security_group.test_pbx.id
  description       = "RTP media. Source cannot be enumerated - see the note in security-groups.tf."
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = var.rtp_port_min
  to_port           = var.rtp_port_max
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-rtp" })
}

# Egress: unrestricted. Declaring a security group without inline egress blocks
# makes Terraform strip AWS's default allow-all, so this rule is required
# rather than decorative - the box needs apt, SSM and Secrets Manager.
resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.test_pbx.id
  description       = "All outbound"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"

  tags = merge(local.tags, { Name = "${local.name_prefix}-egress" })
}
