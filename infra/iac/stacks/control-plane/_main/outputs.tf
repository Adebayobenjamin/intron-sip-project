output "app_instance_ids" {
  description = "EC2 instance IDs for the Ansible inventory."
  value       = aws_instance.app[*].id
}

output "app_private_ips" {
  description = "Private addresses of the app instances."
  value       = aws_instance.app[*].private_ip
}

output "app_security_group_id" {
  value = aws_security_group.app.id
}

output "esl_target" {
  description = <<-DESC
    THE VALUE THAT GOES IN THE FREESWITCH DIALPLAN.

    Set freeswitch_esl_outbound_target to this in the Ansible inventory:

      <action application="socket" data="<this> async full"/>

    A wrong or stale value here drops every inbound call at answer, and the
    only symptom is calls ending immediately with nothing in the controller's
    log — because the controller was never reached.
  DESC
  value       = "${aws_route53_record.esl.name}:${var.esl_listener_port}"
}

output "api_url" {
  description = "Control-plane API base URL. Used by Kamailio's routing lookup and by the ESL controller."
  value       = "http://${aws_route53_record.api.name}:${var.api_listener_port}"
}

output "internal_zone_id" {
  description = "Private hosted zone. Needed if a peered VPC (LiveKit, when its location is confirmed) has to resolve these names — associate from that side with aws_route53_zone_association."
  value       = aws_route53_zone.internal.zone_id
}

output "ansible_inventory_hint" {
  description = <<-DESC
    Paste-ready inventory fragment for the app tier. Regenerate after any apply
    that replaced an instance — the instance IDs are how the SSM connection
    plugin routes tasks.
  DESC
  value = yamlencode({
    all = {
      children = {
        control_plane = {
          hosts = {
            for i, inst in aws_instance.app :
            "${var.environment}-app-${i + 1}" => {
              ansible_host                = inst.private_ip
              ansible_aws_ssm_instance_id = inst.id
              app_node_id                 = i + 1
            }
          }
        }
      }
    }
  })
}
