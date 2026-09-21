output "test_pbx_instance_id" {
  description = "EC2 instance ID. ansible_aws_ssm_instance_id in the inventory - it changes on every replacement."
  value       = aws_instance.test_pbx.id
}

output "test_pbx_private_ip" {
  description = "Private address. ansible_host in the inventory - Ansible reaches this box over SSM, not over the public path."
  value       = aws_instance.test_pbx.private_ip
}

output "test_pbx_public_ip" {
  description = <<-DESC
    The Elastic IP.

    TWO CONSUMERS, and they must agree:
      - test_pbx_public_ip in the Ansible inventory, which Asterisk renders as
        external_media_address / external_signaling_address. Wrong here and
        calls connect with no audio.
      - the handoff target you seed for the tenant, as
        sip:agent@<this address>.
  DESC
  value       = aws_eip.test_pbx.public_ip
}

output "handoff_target_uri" {
  description = "Paste into the tenant's handoff_targets row. This is the whole point of the stack."
  value       = "sip:agent@${aws_eip.test_pbx.public_ip}"
}

output "test_pbx_security_group_id" {
  description = "Security group. Nothing references it today - the edge is admitted by public /32, on purpose."
  value       = aws_security_group.test_pbx.id
}

output "ansible_inventory_hint" {
  description = "Paste into inventories/<env>/hosts.yml after any apply that replaced the instance."
  value       = <<-EOT
    test_pbx:
      hosts:
        ${var.environment}-test-pbx-1:
          ansible_host: ${aws_instance.test_pbx.private_ip}
          ansible_aws_ssm_instance_id: ${aws_instance.test_pbx.id}
          # Asterisk cannot see this address - the OS holds only the private
          # one. It is rendered into pjsip.conf as external_media_address and
          # external_signaling_address; get it wrong and the call sets up
          # cleanly with no audio.
          test_pbx_public_ip: ${aws_eip.test_pbx.public_ip}
  EOT
}
