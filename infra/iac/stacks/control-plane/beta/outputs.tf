output "app_instance_ids" {
  value = module.control_plane.app_instance_ids
}

output "app_private_ips" {
  value = module.control_plane.app_private_ips
}

output "app_security_group_id" {
  value = module.control_plane.app_security_group_id
}

output "esl_target" {
  description = "Put this in freeswitch_esl_outbound_target in the Ansible inventory."
  value       = module.control_plane.esl_target
}

output "api_url" {
  description = "Put this in routing_service_url in the Ansible inventory."
  value       = module.control_plane.api_url
}

output "internal_zone_id" {
  value = module.control_plane.internal_zone_id
}

output "ansible_inventory_hint" {
  value = module.control_plane.ansible_inventory_hint
}
