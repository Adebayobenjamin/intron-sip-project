########################################################
# Private hosted zone.
#
# THE WHOLE POINT: FreeSWITCH's dialplan names a DNS record, not an address.
#
#   <action application="socket" data="esl.beta.intron.internal:8022 async full"/>
#
# That indirection is what makes the app tier replaceable. It is worth being
# precise about the cost of not having it: a hardcoded IP in that dialplan that
# goes stale drops every inbound call at answer, and the only symptom is calls
# ending immediately with nothing in the controller's log, because the
# controller was never reached.
#
# `.internal` rather than a subdomain of a real domain: this zone is
# unreachable and unresolvable from outside the VPC by construction, so there
# is nothing to leak and no public record to keep in step.
########################################################

resource "aws_route53_zone" "internal" {
  name    = var.internal_dns_zone
  comment = "Private zone for ${var.environment}. Resolvable only inside the VPC."

  vpc {
    vpc_id = local.network.vpc_id
  }

  tags = merge(local.tags, {
    Name = var.internal_dns_zone
  })

  # Associating another VPC later (a peered LiveKit VPC, for instance) is done
  # with aws_route53_zone_association from the OTHER side, not by adding a vpc
  # block here - Terraform would fight the association on every plan.
  lifecycle {
    ignore_changes = [vpc]
  }
}

########################################################
# A records straight to the app instances. NO LOAD BALANCER.
#
# WHY THERE IS NO NLB HERE, since the obvious question about a raw-TCP service
# is "what fronts it":
#
# A load balancer would buy two things - health-checked failover, and a
# deregister-drain-restart deploy. Both need somewhere else to send traffic,
# and at app_count = 1 there is nowhere. Failover value is zero. The drain
# reduces to "in-flight calls finish while new calls fail anyway", which is a
# thin return for a component that also inverts security group semantics
# (client IP preservation), terminates live sockets on a health-check flap by
# default, and INTRODUCES a 350s idle-connection reap whose configuration knob
# AWS does not actually expose - verified against the live API, not assumed.
#
# So FreeSWITCH connects straight to the instance. No middlebox means no idle
# reap, no source-address rewriting, and no drain script.
#
# WHAT IS GIVEN UP, stated plainly: a restart of the controller drops the ESL
# sockets it is holding, and those calls end. New calls fail for the duration
# either way. Revisit when a second app instance earns its keep - at which
# point an NLB becomes worth its complexity again, because then there IS
# somewhere else to send traffic.
#
# THE PRIVATE IP IS STATIC (see main.tf - cidrhost off the app subnet), so
# instance replacement does not move this record. TTL is deliberately short so
# that repointing it, when that day comes, takes a minute rather than an hour.
########################################################
resource "aws_route53_record" "esl" {
  zone_id = aws_route53_zone.internal.zone_id
  name    = "esl.${var.internal_dns_zone}"
  type    = "A"
  ttl     = var.dns_ttl

  # All app instances. With one, this is a single A record; with more it
  # becomes round-robin, which is NOT failover - a dead node still gets its
  # share of lookups. That is the point at which a load balancer returns.
  records = aws_instance.app[*].private_ip
}

resource "aws_route53_record" "api" {
  zone_id = aws_route53_zone.internal.zone_id
  name    = "api.${var.internal_dns_zone}"
  type    = "A"
  ttl     = var.dns_ttl

  records = aws_instance.app[*].private_ip
}
