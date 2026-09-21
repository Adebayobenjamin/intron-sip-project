output "livekit_instance_ids" {
  description = "EC2 instance IDs. What Ansible addresses this host by (ansible_aws_ssm_instance_id) - they change on every replacement."
  value       = aws_instance.livekit[*].id
}

output "livekit_private_ips" {
  description = "Private addresses. ansible_host in the inventory, and what livekit-sip will dial when it lands in this stack."
  value       = aws_instance.livekit[*].private_ip
}

output "livekit_public_ips" {
  description = <<-DESC
    Elastic IPs.

    NOT what anything should be configured with. The name is the interface -
    see livekit_url. This is here for firewall allowlists and for confirming
    that the A record points where it should.
  DESC
  value       = aws_eip.livekit[*].public_ip
}

output "livekit_fqdn" {
  description = "The public name. Caddy requests a certificate for exactly this string, and it must match LIVEKIT_URL on both sides."
  value       = local.fqdn
}

output "livekit_turn_fqdn" {
  description = "TURN over TLS, same instance. Rendered into livekit.yaml as turn.domain - a mismatch means clients are handed a TURN address that does not resolve."
  value       = local.turn_fqdn
}

output "livekit_url" {
  description = <<-DESC
    THE VALUE TO HAND INTRON, and to put in this platform's esl-controller
    secret as LIVEKIT_URL.

    wss:// rather than https:// - the agent SDK and the browser client both
    open a WebSocket. An https:// URL works for REST API calls and fails at
    the point the agent tries to register, with an error about the protocol
    rather than about the scheme.
  DESC
  value       = "wss://${local.fqdn}"
}

output "livekit_zone_id" {
  description = "Route53 zone ID, for anything that later adds a record here - a livekit-sip name, or an ACME DNS-01 challenge."
  value       = aws_route53_zone.public.zone_id
}

output "livekit_zone_nameservers" {
  description = <<-DESC
    THE MANUAL STEP. Give these four names to whoever manages the parent
    domain, to be added as NS records for this subdomain.

    Nothing works until that delegation exists: no resolution, no ACME
    challenge, no certificate, no agent connection. The symptom is a TLS
    error, which points at everything except DNS.

    Confirm with:  dig +short NS <zone> @1.1.1.1
  DESC
  value       = aws_route53_zone.public.name_servers
}

output "livekit_security_group_id" {
  description = "LiveKit SG. Referenced when livekit-sip lands, so the edge can reach it without widening anything to the internet."
  value       = aws_security_group.livekit.id
}

########################################################
# Inventory fragment, paste-ready.
#
# Same pattern as sip-core and control-plane: the instance ID changes on every
# replacement, and hand-editing the inventory from a plan output is how a
# stale ID gets in. A stale ID fails with a message about an instance not
# being found, which points at SSM rather than at the file.
########################################################
output "ansible_inventory_hint" {
  description = "Paste into inventories/<env>/hosts.yml after any apply that replaced the instance."

  ########################################################
  # Built with join() rather than a heredoc, and that is not a style choice.
  #
  # A <<-EOT heredoc strips the indentation of its least-indented line, and a
  # %{for~} directive line participates in that calculation while emitting no
  # output. The result is YAML whose nesting is off by the width of the
  # directive - host keys over-indented, their children under-indented - which
  # is invalid and pastes into an inventory that fails to parse.
  #
  # It looks right in the template and wrong only in the rendered output, which
  # is the worst place to find it. Explicit strings cannot drift that way.
  ########################################################
  value = join("\n", concat(
    [
      "    livekit:",
      "      hosts:",
    ],
    flatten([for idx, id in aws_instance.livekit[*].id : [
      "        ${var.environment}-livekit-${idx + 1}:",
      "          ansible_host: ${aws_instance.livekit[idx].private_ip}",
      "          ansible_aws_ssm_instance_id: ${id}",
      # The instance never sees this address - AWS maps it 1:1 at the VPC edge.
      # LiveKit discovers it via STUN at startup; this copy is what lets the
      # play assert the two agree rather than trusting the discovery.
      "          livekit_public_ip: ${aws_eip.livekit[idx].public_ip}",
    ]]),
  ))
}
