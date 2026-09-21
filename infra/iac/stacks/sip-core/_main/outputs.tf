########################################################
# Outputs.
#
# Two audiences. control-plane reads the machine-shaped ones through remote
# state; an operator reads `ansible_inventory_hint` and pastes it.
########################################################

output "primary_az" {
  description = "The AZ both tiers are pinned to. Anything added to the media path belongs here too — see the note in main.tf."
  value       = local.primary_az
}

########################################################
# EDGE
########################################################
output "edge_instance_ids" {
  description = "EC2 instance IDs. These are what Ansible addresses hosts by (ansible_aws_ssm_instance_id) — they change on every replacement."
  value       = aws_instance.edge[*].id
}

output "edge_private_ips" {
  description = "Private addresses. What Kamailio binds to, and what the FreeSWITCH nodes send responses back to."
  value       = aws_instance.edge[*].private_ip
}

output "edge_public_ips" {
  description = <<-DESC
    Elastic IPs. THE ADDRESSES TO GIVE CARRIERS.

    Also the values Kamailio's `advertise` and RTPEngine's --advertised-address
    must carry. The instance cannot discover these itself — AWS maps them 1:1
    at the VPC edge and the OS only ever sees the private address — so they
    are read from here into the Ansible inventory. Getting this wrong produces
    calls that set up and have no audio.
  DESC
  value       = aws_eip.edge[*].public_ip
}

output "edge_security_group_id" {
  description = "Edge SG. Referenced by control-plane so its NLB and instances can be reached from, and reach, the edge."
  value       = aws_security_group.edge.id
}

########################################################
# FREESWITCH
########################################################
output "freeswitch_instance_ids" {
  description = "EC2 instance IDs for the Ansible inventory."
  value       = aws_instance.freeswitch[*].id
}

output "freeswitch_private_ips" {
  description = "Private addresses. These populate Kamailio's dispatcher list — the farm has no other address."
  value       = aws_instance.freeswitch[*].private_ip
}

output "freeswitch_security_group_id" {
  description = "FreeSWITCH SG. control-plane references it to allow the outbound ESL connections that arrive at its NLB."
  value       = aws_security_group.freeswitch.id
}

output "freeswitch_recordings_volume_ids" {
  description = <<-DESC
    EBS volume IDs for the recordings volumes.

    Needed by the Ansible role, not just for the record: Nitro instances expose
    EBS as NVMe devices in attachment order and ignore the requested
    /dev/sdf, so the role finds this volume by matching the NVMe serial
    against the volume ID with the leading "vol-" dash stripped.
  DESC
  value       = aws_ebs_volume.recordings[*].id
}

########################################################
# ANSIBLE
########################################################
output "ansible_ssm_bucket_name" {
  description = "Set ansible_aws_ssm_bucket_name to this in inventories/<env>/group_vars/all.yml. control-plane also reads it, to grant its own instance role the same access."
  value       = aws_s3_bucket.ansible_ssm.id
}

output "ansible_ssm_bucket_arn" {
  description = "For control-plane's instance role policy."
  value       = aws_s3_bucket.ansible_ssm.arn
}

output "ansible_inventory_hint" {
  description = <<-DESC
    Paste-ready inventory fragment for ansible/inventories/<env>/hosts.yml.

    Regenerate this after ANY apply that replaced an instance —
    `make output STACK=sip-core ENV=beta TOKEN=...` — because the instance IDs
    are how the connection plugin routes tasks, and a stale one fails with a
    message about the instance not being found rather than about the inventory.

    Note the two address-like fields per host and keep them distinct:
      ansible_host                 the private IP, used INSIDE rendered configs
                                   (what Kamailio binds to, what the dispatcher
                                   list contains)
      ansible_aws_ssm_instance_id  how Ansible REACHES the box. Not routable
                                   from a laptop; it goes via the SSM data plane.
  DESC
  value = yamlencode({
    all = {
      children = {
        edge = {
          hosts = {
            for i, inst in aws_instance.edge :
            "${var.environment}-edge-${i + 1}" => {
              ansible_host                = inst.private_ip
              ansible_aws_ssm_instance_id = inst.id
              kamailio_node_id            = i + 1
              # The EIP this node advertises. Per-host rather than in
              # group_vars because with more than one edge node each has its
              # own, and a group-level value would silently give both the same.
              kamailio_public_ip = aws_eip.edge[i].public_ip
            }
          }
        }
        freeswitch = {
          hosts = {
            for i, inst in aws_instance.freeswitch :
            "${var.environment}-freeswitch-${i + 1}" => {
              ansible_host                = inst.private_ip
              ansible_aws_ssm_instance_id = inst.id
              freeswitch_node_id          = i + 1
              recordings_volume_id        = aws_ebs_volume.recordings[i].id
            }
          }
        }
      }
    }
  })
}

output "operator_iam_policy_snippet" {
  description = <<-DESC
    Statements the OPERATOR's own IAM identity needs so
    `ansible-playbook` can drive these hosts.

    NOT NEEDED while that identity holds AdministratorAccess. This exists for
    a restricted operator - the minimum an Ansible operator actually requires.

    Not attachable from here — that identity lives outside this stack — so it
    is emitted for pasting into the operator policy.

    Two statements rather than one, and the split is the point: S3 is scoped to
    the transport bucket, while SSM sits at "*" because SendCommand and
    StartSession take instance ARNs and pinning them would couple the operator
    policy to inventory churn. The SSM statement's blast radius is still
    narrow — none of those actions can touch IAM, networking, or data. They let
    the holder run commands on instances already enrolled in SSM, which is the
    entire job description of an Ansible operator.
  DESC
  value = jsonencode([
    {
      Sid    = "AnsibleSSMSessionAndCommand"
      Effect = "Allow"
      Action = [
        "ssm:StartSession",
        "ssm:TerminateSession",
        "ssm:ResumeSession",
        "ssm:DescribeSessions",
        "ssm:GetConnectionStatus",
        "ssm:DescribeInstanceInformation",
        "ssm:SendCommand",
        "ssm:GetCommandInvocation",
        "ssm:ListCommandInvocations",
      ]
      Resource = "*"
    },
    {
      Sid      = "AnsibleSSMTransportBucket"
      Effect   = "Allow"
      Action   = local.ansible_ssm_bucket_actions
      Resource = [aws_s3_bucket.ansible_ssm.arn, "${aws_s3_bucket.ansible_ssm.arn}/*"]
    },
  ])
}
