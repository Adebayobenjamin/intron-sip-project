########################################################
# Security groups.
#
# Written with the per-rule resources (aws_vpc_security_group_ingress_rule)
# rather than inline ingress/egress blocks. Two reasons that matter in
# practice: a plan shows exactly which rule changed instead of re-rendering
# the whole group, and adding a carrier CIDR no longer produces a diff that
# touches every other rule in the group.
#
# NOTE ON CROSS-STACK RULES. The FreeSWITCH ESL port is opened to the APP
# SUBNET CIDR, not to the control-plane's security group. Referencing an SG
# owned by another stack creates a cycle — control-plane needs this stack's
# SG for its own rules — and the usual escapes (a shared security-group stack,
# or rules defined in whichever stack applies second) both trade a clear
# dependency for an unclear one. The app subnets contain the control plane and
# nothing else, so the precision lost is the difference between "the control
# plane" and "the subnets the control plane is alone in".
########################################################

########################################################
# EDGE — Kamailio + RTPEngine. The only tier carriers can reach.
########################################################
resource "aws_security_group" "edge" {
  name        = "${local.name_prefix}-edge"
  description = "Kamailio SIP signalling + RTPEngine media relay. Internet-facing."
  vpc_id      = local.network.vpc_id

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-edge"
    tier = "edge"
  })

  # Rules are separate resources, so replacing this group would orphan them.
  # create_before_destroy makes the replacement order work if a name or
  # description change ever forces one.
  lifecycle {
    create_before_destroy = true
  }
}

########################################################
# SIP signalling from carriers.
#
# Scoped to var.carrier_sip_cidrs, and the default for that variable is an
# EMPTY LIST on purpose. An empty list means no calls arrive, which is obvious
# within a minute of testing. The alternative default — 0.0.0.0/0 — means calls
# arrive and so does everything else: SIP scanners find a 5060 responder within
# hours, and the first thing they try is toll fraud through it. Loudly broken
# beats quietly exposed.
#
# UDP and TCP both, because carriers differ and a trunk that fails over from
# UDP to TCP mid-incident should not also hit a firewall.
########################################################
resource "aws_vpc_security_group_ingress_rule" "edge_sip_udp" {
  for_each = toset(var.carrier_sip_cidrs)

  security_group_id = aws_security_group.edge.id
  description       = "SIP/UDP from carrier ${each.value}"
  cidr_ipv4         = each.value
  from_port         = 5060
  to_port           = 5060
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-sip-udp" })
}

resource "aws_vpc_security_group_ingress_rule" "edge_sip_tcp" {
  for_each = toset(var.carrier_sip_cidrs)

  security_group_id = aws_security_group.edge.id
  description       = "SIP/TCP from carrier ${each.value}"
  cidr_ipv4         = each.value
  from_port         = 5060
  to_port           = 5060
  ip_protocol       = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-sip-tcp" })
}

resource "aws_vpc_security_group_ingress_rule" "edge_sips_tcp" {
  for_each = toset(var.carrier_sip_cidrs)

  security_group_id = aws_security_group.edge.id
  description       = "SIP-TLS from carrier ${each.value}"
  cidr_ipv4         = each.value
  from_port         = var.sips_port
  to_port           = var.sips_port
  ip_protocol       = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-sips" })
}

########################################################
# RTP — and this one is 0.0.0.0/0, which looks alarming and is correct.
#
# Media does not come from the signalling address. A carrier signals from its
# SBC and then sources RTP from a media gateway in a different range, often a
# different provider entirely, and for mobile-originated calls the far end may
# be a handset behind carrier-grade NAT with an address nobody can enumerate in
# advance. Pinning this to the signalling CIDRs produces one-way audio that
# reproduces intermittently and looks like a codec problem for a week.
#
# What actually protects this range is RTPEngine, not the security group: it
# only forwards on ports it has allocated for a call it was told about via the
# ng protocol, and it drops everything else. An unsolicited packet to a port in
# this range hits a closed socket.
#
# The range is the real control. Keep it tight — 30000-40000 is 10,000 ports,
# two per call, so ~5,000 concurrent legs. Widening it to the whole ephemeral
# range would expose ports the OS also hands out to outbound connections.
########################################################
resource "aws_vpc_security_group_ingress_rule" "edge_rtp" {
  security_group_id = aws_security_group.edge.id
  description       = "RTP/RTCP media. Source cannot be enumerated - see the note in security-groups.tf."
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = var.rtpengine_port_min
  to_port           = var.rtpengine_port_max
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-rtp" })
}

# Return path from the farm. FreeSWITCH signals back to Kamailio (responses,
# BYE, REFER) and sends media to RTPEngine.
resource "aws_vpc_security_group_ingress_rule" "edge_sip_from_farm" {
  security_group_id            = aws_security_group.edge.id
  description                  = "SIP/UDP back from the FreeSWITCH farm"
  referenced_security_group_id = aws_security_group.freeswitch.id
  from_port                    = 5060
  to_port                      = 5060
  ip_protocol                  = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-sip-farm" })
}

resource "aws_vpc_security_group_ingress_rule" "edge_rtp_from_farm" {
  security_group_id            = aws_security_group.edge.id
  description                  = "RTP from the FreeSWITCH farm to the relay"
  referenced_security_group_id = aws_security_group.freeswitch.id
  from_port                    = var.rtpengine_port_min
  to_port                      = var.rtpengine_port_max
  ip_protocol                  = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-rtp-farm" })
}

########################################################
# NO CONTROL-INTERFACE RULE. There was one here opening Kamailio's xhttp port
# to the app subnets, so the control plane could drive outbound originate.
#
# Originate is now controller -> FreeSWITCH directly over ESL, and Kamailio has
# no control listener at all. The rule is deleted rather than left in place
# pointing at a closed port: an ingress rule that still exists reads as an
# intentional path, and the next person to want a control channel will wire to
# it instead of asking why it is there.
#
# The app tier still reaches the edge for SIP - that is the egress path, and it
# is covered by edge_sip_from_farm above.
########################################################

# RTPEngine's ng control port gets NO rule. RTPEngine is co-located with
# Kamailio on this box, so the ng protocol never leaves loopback. If the two
# are ever split onto separate instances, this is the rule that has to appear
# — and the symptom of forgetting it is every call setting up with no media.

# Egress: unrestricted. Declaring a security group without inline egress blocks
# makes Terraform strip AWS's default allow-all, so this rule is required, not
# decorative. The edge needs to reach carriers on ports they choose, the farm,
# Secrets Manager, SSM, and the package mirrors — an allowlist here would be a
# long list that gets widened under pressure during an incident.
resource "aws_vpc_security_group_egress_rule" "edge_all" {
  security_group_id = aws_security_group.edge.id
  description       = "All outbound"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-egress" })
}

########################################################
# FARM — FreeSWITCH. No public address, no internet-reachable path.
########################################################
resource "aws_security_group" "freeswitch" {
  name        = "${local.name_prefix}-freeswitch"
  description = "FreeSWITCH B2BUA farm. Private only - reachable via the edge and the app tier."
  vpc_id      = local.network.vpc_id

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-freeswitch"
    tier = "sip"
  })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "fs_sip_udp" {
  security_group_id            = aws_security_group.freeswitch.id
  description                  = "SIP/UDP from the Kamailio edge (dispatcher)"
  referenced_security_group_id = aws_security_group.edge.id
  from_port                    = 5060
  to_port                      = 5060
  ip_protocol                  = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-fs-sip-udp" })
}

resource "aws_vpc_security_group_ingress_rule" "fs_sip_tcp" {
  security_group_id            = aws_security_group.freeswitch.id
  description                  = "SIP/TCP from the Kamailio edge"
  referenced_security_group_id = aws_security_group.edge.id
  from_port                    = 5060
  to_port                      = 5060
  ip_protocol                  = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-fs-sip-tcp" })
}

resource "aws_vpc_security_group_ingress_rule" "fs_rtp_from_edge" {
  security_group_id            = aws_security_group.freeswitch.id
  description                  = "RTP from the RTPEngine relay"
  referenced_security_group_id = aws_security_group.edge.id
  from_port                    = var.freeswitch_rtp_port_min
  to_port                      = var.freeswitch_rtp_port_max
  ip_protocol                  = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-fs-rtp-edge" })
}

########################################################
# Inbound ESL — the controller connecting IN to FreeSWITCH on 8021 to issue
# commands and subscribe to events. This is the opposite direction from the
# &socket() path in the diagram's arrow 3, and both exist:
#
#   outbound ESL   FS  -> controller:8022   per call, "is this routable?"
#   inbound  ESL   ctl -> FS:8021           originate, uuid_*, event stream
#
# The one in this rule is the second. The first needs no rule here at all —
# it leaves FreeSWITCH as an outbound connection.
########################################################
resource "aws_vpc_security_group_ingress_rule" "fs_esl_inbound" {
  for_each = toset(local.app_subnet_cidrs)

  security_group_id = aws_security_group.freeswitch.id
  description       = "Inbound ESL from the app tier (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.freeswitch_esl_inbound_port
  to_port           = var.freeswitch_esl_inbound_port
  ip_protocol       = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-fs-esl" })
}

resource "aws_vpc_security_group_ingress_rule" "fs_xmlrpc" {
  for_each = toset(local.app_subnet_cidrs)

  security_group_id = aws_security_group.freeswitch.id
  description       = "mod_xml_rpc from the app tier (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.freeswitch_xmlrpc_port
  to_port           = var.freeswitch_xmlrpc_port
  ip_protocol       = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-fs-xmlrpc" })
}

########################################################
# LiveKit — arrow 4, the AI leg.
#
# FreeSWITCH bridges the B-leg through Kamailio to livekit-sip, so the
# signalling hop is edge <-> livekit-sip and the media hop is
# RTPEngine <-> livekit-sip.
#
# INTERIM: OVER THE PUBLIC INTERNET. LiveKit is in a different AWS account, so
# for now this is an ordinary SIP trunk between two public addresses — exactly
# what any carrier interconnect is. livekit_cidrs holds LiveKit's PUBLIC egress
# addresses today.
#
# The design survives the migration because only the CIDRs change. When the two
# accounts are peered (or a tunnel exists), put the PRIVATE ranges in this same
# variable and remove nothing else — no rule is restructured, no address is
# hardcoded anywhere but here and in the Ansible inventory. See docs/LIVEKIT.md.
#
# WHY THIS LEG IS ENCRYPTED AND THE CARRIER LEG IS NOT.
# The carrier leg is plain UDP because that is what ITSPs interconnect with, and
# it traverses a comparatively short, commercially-accountable path. This leg is
# our own traffic crossing the open internet between two accounts we control,
# and it carries clinical conversation audio. Plain RTP is readable by anyone on
# the path. So the default here is SIP over TLS with SRTP media, and dropping to
# UDP is a deliberate, named choice rather than the shape it happens to have.
#
# Empty livekit_cidrs means the AI leg does not connect — the correct failure. A
# guessed CIDR that happens to be routable is worse.
########################################################
locals {
  # One of these is the empty set. Splitting here rather than branching inside
  # each rule keeps the plan readable: you see exactly which transport is open.
  livekit_tls_cidrs = var.livekit_sip_transport == "tls" ? toset(var.livekit_cidrs) : toset([])
  livekit_udp_cidrs = var.livekit_sip_transport == "udp" ? toset(var.livekit_cidrs) : toset([])
}

resource "aws_vpc_security_group_ingress_rule" "edge_sips_from_livekit" {
  for_each = local.livekit_tls_cidrs

  security_group_id = aws_security_group.edge.id
  description       = "SIP-TLS from livekit-sip (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.sips_port
  to_port           = var.sips_port
  ip_protocol       = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-sips-livekit" })
}

resource "aws_vpc_security_group_ingress_rule" "edge_sip_from_livekit" {
  for_each = local.livekit_udp_cidrs

  security_group_id = aws_security_group.edge.id
  description       = "SIP/UDP from livekit-sip (${each.value}) - UNENCRYPTED signalling"
  cidr_ipv4         = each.value
  from_port         = 5060
  to_port           = 5060
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-sip-livekit" })
}

# Media, either transport. SRTP rides the same UDP ports as RTP — encryption
# changes the payload, not the port allocation — so this rule is identical
# whether the leg is encrypted or not, and RTPEngine remains the thing that
# actually decides which packets are forwarded.
resource "aws_vpc_security_group_ingress_rule" "edge_rtp_from_livekit" {
  for_each = toset(var.livekit_cidrs)

  security_group_id = aws_security_group.edge.id
  description       = "RTP/SRTP from livekit-sip (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.rtpengine_port_min
  to_port           = var.rtpengine_port_max
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-edge-rtp-livekit" })
}

resource "aws_vpc_security_group_egress_rule" "freeswitch_all" {
  security_group_id = aws_security_group.freeswitch.id
  description       = "All outbound - package mirrors and the source build, Secrets Manager, SSM, and the outbound ESL socket to the controller"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"

  tags = merge(local.tags, { Name = "${local.name_prefix}-fs-egress" })
}

locals {
  # The app tier's CIDRs, for the cross-stack rules above. Read from the
  # network stack rather than the control-plane stack so the dependency stays
  # one-directional: control-plane -> sip-core -> network.
  app_subnet_cidrs = [
    for k, v in local.network.subnet_cidrs : v if startswith(k, "app-")
  ]
}
