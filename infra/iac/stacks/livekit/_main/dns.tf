########################################################
# Public hosted zone, and the one A record in it.
#
# THIS IS THE ONLY PUBLIC DNS IN THE REPO. Everything else resolves inside the
# VPC through beta.intron.internal, a private zone that is unreachable from
# outside by construction. This has to be public for a reason that is worth
# being clear about: the two clients that matter are a browser on somebody's
# laptop and an agent process in a different AWS account. Neither is in our
# VPC, so neither can resolve a private name, and Let's Encrypt cannot
# validate one either.
#
# A SUBDOMAIN, NOT THE APEX. quiverlearn.com is managed somewhere else. This
# stack takes ownership of one level below it, so Terraform can add and change
# records here without any access to, or risk of clobbering, the parent.
########################################################

resource "aws_route53_zone" "public" {
  name    = var.public_dns_zone
  comment = "Public zone for ${var.environment} LiveKit. Delegated from the parent domain - see livekit_zone_nameservers."

  tags = merge(local.tags, {
    Name = var.public_dns_zone
  })
}

########################################################
# A record, straight to the Elastic IP. No load balancer, no alias.
#
# An ALB in front would cost around 20 dollars a month and solve half the
# problem: it terminates HTTPS, but WebRTC media is UDP 50000-60000 direct to
# the instance and an ALB cannot carry that at all. Caddy on the box does the
# TLS for free, which is also what the existing Intron deployment does.
#
# TTL 60 rather than the usual 300. This record is the recovery path when the
# instance is replaced: the EIP reattaches, but if it ever does not, a short
# TTL is the difference between a five-minute fix and waiting out a cache.
########################################################
resource "aws_route53_record" "livekit" {
  zone_id = aws_route53_zone.public.zone_id
  name    = local.fqdn
  type    = "A"
  ttl     = 60
  records = aws_eip.livekit[*].public_ip
}

########################################################
# The TURN name. SAME ADDRESS, different hostname.
#
# Not redundancy — a demultiplexing key. TURN/TLS and the LiveKit WebSocket
# both have to answer on 443, since 443 is the one port a restrictive network
# reliably allows. Caddy separates them on TLS SNI, and SNI carries a hostname,
# so two services on one port need two names.
#
# Caddy requests a certificate for this name too. If the A record is missing
# the ACME challenge for it fails, and Caddy's failure is for the WHOLE
# certificate batch — so a missing TURN record takes the main name's
# certificate down with it. Both records or neither.
########################################################
resource "aws_route53_record" "livekit_turn" {
  zone_id = aws_route53_zone.public.zone_id
  name    = local.turn_fqdn
  type    = "A"
  ttl     = 60
  records = aws_eip.livekit[*].public_ip
}

########################################################
# WHAT TERRAFORM CANNOT DO HERE, and it blocks everything downstream.
#
# Creating this zone does not make the name resolve. The parent domain's
# operator has to add four NS records pointing at the nameservers in this
# stack's `livekit_zone_nameservers` output. Until that happens:
#
#   the name does not resolve from anywhere
#   Caddy's ACME HTTP-01 challenge fails, so there is no certificate
#   Intron's agent cannot connect, and reports a TLS error
#
# The TLS error is the trap. It points at certificates, which points at Caddy,
# which is working fine and waiting on a name that does not exist. Check
# delegation FIRST:
#
#   dig +short NS <public_dns_zone> @1.1.1.1
#
# against a resolver that is not your own, and expect the four names below.
# An empty answer means the delegation has not been done, or has not
# propagated - give it an hour before assuming it is wrong.
########################################################
