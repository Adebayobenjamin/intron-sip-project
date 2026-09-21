########################################################
# LiveKit security group.
#
# READ THIS BEFORE TIGHTENING ANYTHING HERE.
#
# Every rule below is open to var.livekit_client_cidrs, which defaults to
# 0.0.0.0/0. That looks wrong next to sip-core, where an empty allowlist is the
# safe default and 0.0.0.0/0 invites toll fraud. The difference is real and it
# is about what the control actually is:
#
#   SIP edge      Anyone who can send a packet can attempt a call. The source
#                 allowlist IS the authentication, because SIP has no other.
#   LiveKit       Every connection presents a JWT signed with the API secret.
#                 No token, no room, no API call — a 401 and a closed socket.
#
# And the client set here is genuinely unknowable: a browser on a laptop, and
# Intron's agent behind a NAT gateway in an account we do not control.
#
# The thing that would make this dangerous is a leaked API secret, not an open
# port. Guard the secret.
#
# SECURITY GROUP DESCRIPTIONS: AWS accepts only a restricted character set, and
# enforces it in the EC2 API rather than the Terraform schema — so an em dash
# or an apostrophe passes `validate` and `plan`, then fails MID-APPLY. Plain
# ASCII only below, and `make lint` catches the rest.
########################################################

resource "aws_security_group" "livekit" {
  name        = "${local.name_prefix}-server"
  description = "LiveKit server - WebRTC signalling and media"
  vpc_id      = local.network.vpc_id

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-server"
    tier = "livekit"
  })

  # Rules are separate resources, not inline blocks. Inline rules are
  # authoritative for the whole group, so a second stack adding one would have
  # its rule silently removed on the next apply here.
  lifecycle {
    create_before_destroy = true
  }
}

########################################################
# 443/tcp - HTTPS, WSS, and TURN over TLS, all through Caddy.
#
# THE ONE PORT THAT MATTERS. Three different things ride it, demultiplexed by
# Caddy on TLS SNI:
#
#   the browser loading a token and opening its WebSocket
#   Intron's agent registering over wss:// and staying connected
#   TURN/TLS for clients that can reach nothing else
#
# Closing this to a narrow range is what breaks the agent link first, and the
# error Intron would see is a connection timeout with no server-side log.
########################################################
resource "aws_vpc_security_group_ingress_rule" "https" {
  for_each = toset(var.livekit_client_cidrs)

  security_group_id = aws_security_group.livekit.id
  description       = "HTTPS and WSS via Caddy (${each.value})"
  cidr_ipv4         = each.value
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-https" })
}

########################################################
# 80/tcp - ACME HTTP-01 challenge. NOT application traffic.
#
# Caddy provisions and renews the Let's Encrypt certificate over this port and
# serves nothing else on it. It has to be open to the INTERNET rather than to
# livekit_client_cidrs, because the challenge comes from Let's Encrypt's
# validation servers, whose addresses are deliberately unpublished and change.
#
# Close it and the certificate issues once (if it already has), then fails to
# renew 60 days later. The failure lands on a Saturday, presents as every
# client dropping simultaneously, and nothing in LiveKit's log mentions a
# certificate.
########################################################
resource "aws_vpc_security_group_ingress_rule" "acme_http" {
  security_group_id = aws_security_group.livekit.id
  # No apostrophe in "Let's Encrypt" here on purpose - the EC2 API rejects it
  # and does so MID-APPLY, after validate and plan both pass. make lint catches
  # it; this comment is so it does not come back.
  description = "ACME HTTP-01 challenge - must be open to the internet, ACME validation source addresses are unpublished"
  cidr_ipv4   = "0.0.0.0/0"
  from_port   = 80
  to_port     = 80
  ip_protocol = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-acme" })
}

########################################################
# WebRTC media - UDP, the normal path.
#
# MUST MATCH rtc.port_range_start/end in livekit.yaml. Terraform opens what the
# daemon binds to and the two live in different files; a mismatch is a call
# that connects, lists both participants, and has no audio. Neither side logs
# anything naming a port.
########################################################
resource "aws_vpc_security_group_ingress_rule" "rtc_udp" {
  for_each = toset(var.livekit_client_cidrs)

  security_group_id = aws_security_group.livekit.id
  description       = "WebRTC media UDP (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.rtc_udp_port_min
  to_port           = var.rtc_udp_port_max
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-rtc-udp" })
}

########################################################
# WebRTC media - TCP fallback, for clients whose network blocks UDP.
#
# Slower and worse, and the difference between a working call and none at all
# for anyone on a locked-down corporate network.
########################################################
resource "aws_vpc_security_group_ingress_rule" "rtc_tcp" {
  for_each = toset(var.livekit_client_cidrs)

  security_group_id = aws_security_group.livekit.id
  description       = "WebRTC media TCP fallback (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.rtc_tcp_port
  to_port           = var.rtc_tcp_port
  ip_protocol       = "tcp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-rtc-tcp" })
}

########################################################
# TURN over UDP. The relay of last resort.
#
# TURN/TLS is NOT here: it rides 443 above, routed by Caddy on SNI. That is the
# same arrangement Intron's existing deployment uses, and it means a client on
# a network that permits only 443 still gets media.
########################################################
resource "aws_vpc_security_group_ingress_rule" "turn_udp" {
  for_each = toset(var.livekit_client_cidrs)

  security_group_id = aws_security_group.livekit.id
  description       = "TURN over UDP (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.turn_udp_port
  to_port           = var.turn_udp_port
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-turn-udp" })
}

########################################################
# TURN RELAY range. A DIFFERENT THING from the TURN port above.
#
# 3478 is where a client asks for a relay allocation. The relay is then handed
# out on a port in THIS range and the media flows there. Open 3478 alone and
# TURN accepts the request, allocates, and relays to a port the security group
# drops - so the negotiation succeeds and the audio does not arrive.
#
# WHY THIS SURVIVED THE FIRST ROUND OF TESTING. Only clients that actually fall
# back to TURN are affected. Anyone on a normal network takes the direct media
# path above, hears working audio, and concludes TURN is configured. It fails
# exactly for the callers behind a symmetric NAT or a restrictive corporate
# firewall - the hospital case this leg exists to serve.
#
# MUST MATCH turn.relay_range_start/end in livekit.yaml. The server prints the
# range it chose at startup, which is how this gap was found:
#   Starting TURN server  turn.relay_range_start: 30000, turn.relay_range_end: 40000
########################################################
resource "aws_vpc_security_group_ingress_rule" "turn_relay" {
  for_each = toset(var.livekit_client_cidrs)

  security_group_id = aws_security_group.livekit.id
  description       = "TURN relay allocations (${each.value})"
  cidr_ipv4         = each.value
  from_port         = var.turn_relay_port_min
  to_port           = var.turn_relay_port_max
  ip_protocol       = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-turn-relay" })
}

########################################################
# livekit-sip: SIP signalling, from OUR EDGE ONLY.
#
# READ THE SOURCE. Every rule above is open to the internet because its
# clients are a browser and a process behind somebody else's NAT. This one is
# the opposite: the only thing that ever sends an INVITE here is our own
# Kamailio, in this VPC, so the source is the EDGE SECURITY GROUP.
#
# A security group as a source rather than a CIDR is deliberate - the edge
# instance can be replaced, and its private address with it, without this rule
# going stale. The rule follows the role, not the address.
#
# This is also what removes the SIP-TLS question entirely. The leg used to
# cross the public internet between two AWS accounts, which is why
# livekit_sip_transport was "tls" and livekit_srtp_required was true. Inside
# one VPC, with a source restricted to one security group, plain UDP is the
# same choice every carrier interconnect makes and there is no certificate to
# negotiate between estates.
########################################################
resource "aws_vpc_security_group_ingress_rule" "sip_from_edge" {
  count = var.enable_sip ? 1 : 0

  security_group_id            = aws_security_group.livekit.id
  description                  = "SIP from the Kamailio edge - VPC only, never the internet"
  referenced_security_group_id = local.sip_core.edge_security_group_id
  from_port                    = var.sip_port
  to_port                      = var.sip_port
  ip_protocol                  = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-sip" })
}

########################################################
# livekit-sip: RTP, also from the edge only.
#
# A DIFFERENT RANGE FROM THE WebRTC RULES ABOVE, and the distinction matters.
# Two media paths meet inside livekit-sip:
#
#   RTPEngine (edge)  <-- plain RTP, this range -->  livekit-sip
#   livekit-sip       <-- WebRTC, rtc_udp range -->  livekit-server
#
# Sharing one range between them in the security group gives audio that works
# in one direction and not the other, which is a long afternoon.
#
# MUST MATCH rtp_port in the rendered sip.yaml.
########################################################
resource "aws_vpc_security_group_ingress_rule" "sip_rtp_from_edge" {
  count = var.enable_sip ? 1 : 0

  security_group_id            = aws_security_group.livekit.id
  description                  = "SIP-side RTP from RTPEngine on the edge"
  referenced_security_group_id = local.sip_core.edge_security_group_id
  from_port                    = var.sip_rtp_port_min
  to_port                      = var.sip_rtp_port_max
  ip_protocol                  = "udp"

  tags = merge(local.tags, { Name = "${local.name_prefix}-sip-rtp" })
}

########################################################
# NO PORT 22 RULE, and no key_name on the instance. SSM Session Manager or
# nothing, the same as every other box in this repo.
########################################################

########################################################
# Egress - everything.
#
# Needed for: the Debian package mirrors and Docker Hub at build time, Secrets
# Manager and SSM continuously, Let's Encrypt for ACME, and the STUN lookup
# LiveKit uses to discover its own public address at startup.
#
# That last one is easy to miss. use_external_ip makes LiveKit query a public
# STUN server on boot; with egress closed it falls back to the private address,
# advertises 10.70.x.x in its ICE candidates, and every call connects with no
# media. Same class of failure as the SIP advertised-address trap, reached a
# completely different way.
########################################################
resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.livekit.id
  description       = "All outbound - packages, Secrets Manager, SSM, ACME, and the STUN lookup LiveKit needs to learn its own public address"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"

  tags = merge(local.tags, { Name = "${local.name_prefix}-egress" })
}
