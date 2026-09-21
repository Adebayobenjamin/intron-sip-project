########################################################
# Re-exported for control-plane's remote_state lookup and for the operator.
#
# `make output STACK=sip-core ENV=beta TOKEN=... EXTRA="-raw ansible_inventory_hint"`
# prints the inventory fragment to paste after any apply that replaced an
# instance.
########################################################

output "primary_az" {
  value = module.sip_core.primary_az
}

output "edge_instance_ids" {
  value = module.sip_core.edge_instance_ids
}

output "edge_private_ips" {
  value = module.sip_core.edge_private_ips
}

output "edge_public_ips" {
  description = "Give these to the carrier. Also what Kamailio advertises and RTPEngine rewrites SDP to."
  value       = module.sip_core.edge_public_ips
}

output "edge_security_group_id" {
  value = module.sip_core.edge_security_group_id
}

output "freeswitch_instance_ids" {
  value = module.sip_core.freeswitch_instance_ids
}

output "freeswitch_private_ips" {
  value = module.sip_core.freeswitch_private_ips
}

output "freeswitch_security_group_id" {
  value = module.sip_core.freeswitch_security_group_id
}

output "freeswitch_recordings_volume_ids" {
  value = module.sip_core.freeswitch_recordings_volume_ids
}

output "ansible_ssm_bucket_name" {
  value = module.sip_core.ansible_ssm_bucket_name
}

output "ansible_ssm_bucket_arn" {
  value = module.sip_core.ansible_ssm_bucket_arn
}

output "ansible_inventory_hint" {
  value = module.sip_core.ansible_inventory_hint
}

output "operator_iam_policy_snippet" {
  value = module.sip_core.operator_iam_policy_snippet
}
