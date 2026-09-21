output "test_pbx_instance_id" {
  description = "ansible_aws_ssm_instance_id in the inventory."
  value       = module.test_pbx.test_pbx_instance_id
}

output "test_pbx_private_ip" {
  description = "ansible_host in the inventory."
  value       = module.test_pbx.test_pbx_private_ip
}

output "test_pbx_public_ip" {
  description = "The Elastic IP. Rendered into pjsip.conf as external_media_address - wrong here means no audio."
  value       = module.test_pbx.test_pbx_public_ip
}

output "handoff_target_uri" {
  description = "Paste into the tenant's handoff_targets row."
  value       = module.test_pbx.handoff_target_uri
}

output "ansible_inventory_hint" {
  description = "Paste into inventories/beta/hosts.yml after any apply that replaced the instance."
  value       = module.test_pbx.ansible_inventory_hint
}
