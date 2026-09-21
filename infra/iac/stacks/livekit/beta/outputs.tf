########################################################
# Re-exported for the operator, and for the inventory.
#
# The two that get used most, in the order they are needed:
#
#   make output STACK=livekit ENV=beta EXTRA="livekit_zone_nameservers"
#     -> the four NS records the parent domain's operator has to add. Nothing
#        else in this stack works until they exist.
#
#   make output STACK=livekit ENV=beta EXTRA="-raw livekit_url"
#     -> the wss:// URL that goes to Intron and into the esl-controller secret.
########################################################

output "livekit_instance_ids" {
  value = module.livekit.livekit_instance_ids
}

output "livekit_private_ips" {
  value = module.livekit.livekit_private_ips
}

output "livekit_public_ips" {
  description = "Elastic IPs. For allowlists and for checking the A record - configure things with the name, not this."
  value       = module.livekit.livekit_public_ips
}

output "livekit_fqdn" {
  value = module.livekit.livekit_fqdn
}

output "livekit_turn_fqdn" {
  value = module.livekit.livekit_turn_fqdn
}

output "livekit_url" {
  description = "Hand this to Intron, and put it in intron-sip/beta/esl-controller as LIVEKIT_URL."
  value       = module.livekit.livekit_url
}

output "livekit_zone_id" {
  value = module.livekit.livekit_zone_id
}

output "livekit_zone_nameservers" {
  description = "THE MANUAL STEP. NS records for the parent domain to delegate this subdomain. Nothing resolves until these are in place."
  value       = module.livekit.livekit_zone_nameservers
}

output "livekit_security_group_id" {
  value = module.livekit.livekit_security_group_id
}

output "ansible_inventory_hint" {
  description = "Paste into inventories/beta/hosts.yml after any apply that replaced the instance."
  value       = module.livekit.ansible_inventory_hint
}
