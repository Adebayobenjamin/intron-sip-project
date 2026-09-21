########################################################
# EDGE — Kamailio + RTPEngine, co-located.
#
# ONE BOX, TWO DAEMONS. The architecture diagram draws them inside a single
# "Kamailio VM" and that is what this builds. Co-location is the right call
# here for a reason beyond saving an instance: Kamailio drives RTPEngine over
# the ng control protocol on every offer/answer, and keeping that on loopback
# removes a network hop from the call-setup path and a security group from the
# list of things that can silently break media.
#
# The cost is that they share a CPU budget, and they stress it differently:
# Kamailio is latency-sensitive on a small number of packets, RTPEngine is
# throughput-bound on a great many. When media volume starts affecting SIP
# response times, splitting them is the fix — and the rule that has to appear
# at that point is the ng port, noted in security-groups.tf.
########################################################

resource "aws_instance" "edge" {
  count = var.edge_count

  ami           = data.aws_ami.debian.id
  instance_type = var.edge_instance_type
  subnet_id     = local.edge_subnet_id
  private_ip    = local.edge_private_ips[count.index]

  vpc_security_group_ids = [aws_security_group.edge.id]
  iam_instance_profile   = aws_iam_instance_profile.instance["edge"].name

  # No key_name. There is no SSH key in this repo, no port 22 rule in the edge
  # security group, and no bastion. Access is SSM Session Manager or nothing —
  # attaching a key pair here would create a second path in that nothing else
  # in this repo defends.
  key_name = null

  ########################################################
  # IMDSv2 required, not optional.
  #
  # This instance holds an IAM role that can read Kamailio's credentials, and
  # it terminates traffic from the public internet. IMDSv1 answers an
  # unauthenticated GET, so any SSRF in anything listening here hands over
  # role credentials. Requiring the token turns that into a PUT the same class
  # of bug generally cannot make.
  #
  # hop_limit stays at 1: the metadata service should not be reachable from a
  # container on this host, and nothing here runs one.
  ########################################################
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = var.edge_root_volume_size
    encrypted   = true

    # Delete with the instance. Nothing durable lives on the edge — Kamailio's
    # state is registrations (in Valkey or rebuilt by re-REGISTER) and its
    # config comes from Ansible plus Secrets Manager. A surviving orphan
    # volume here would be a cost and a stale-credential liability, not a
    # backup.
    delete_on_termination = true

    tags = merge(local.tags, {
      Name = "${local.name_prefix}-edge-${count.index + 1}-root"
    })
  }

  user_data = templatefile("${path.module}/scripts/vm-bootstrap.sh.tftpl", {
    hostname   = "${var.environment}-edge-${count.index + 1}"
    aws_region = var.aws_region
    role       = "edge"
  })

  # Changing user_data forces a replacement, which for the edge means the
  # Elastic IP detaches and reattaches — a brief outage, not a lost address.
  # Deliberately NOT ignored: unlike the hybrid-activation setup this is
  # derived from, nothing rotates in here, so a user_data diff means someone
  # actually changed the bootstrap and should see the replacement coming.
  user_data_replace_on_change = true

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-edge-${count.index + 1}"
    tier = "edge"
    role = "kamailio-rtpengine"
    node = count.index + 1
  })
}

########################################################
# Elastic IP — the address carriers allowlist.
#
# THE INSTANCE NEVER SEES THIS ADDRESS. AWS maps it 1:1 at the VPC edge; the
# OS only ever has the private 10.70.0.x address on its single NIC. That is
# fine for TCP and catastrophic for SIP if unhandled, because SIP carries
# addresses in the payload:
#
#   Kamailio   must set `advertise` on its listen directive, and Record-Route
#              with the public address, or the Via/Contact it emits point at
#              10.70.0.10 and the carrier's ACK and BYE go nowhere.
#   RTPEngine  must run with --advertised-address = this EIP, or the SDP it
#              rewrites offers a private address and the call sets up with
#              no audio in either direction.
#
# Both are wired from the Ansible inventory (see kamailio_public_ip in
# ansible/inventories/beta/group_vars/all.yml), which is why this is an output
# of this stack rather than something Ansible discovers on the box — it CANNOT
# be discovered on the box.
#
# Separate resource from the instance so it survives replacement: the address
# is on a carrier allowlist, and changing it means a ticket with the ITSP and
# an outage that lasts as long as they take to action it.
########################################################
resource "aws_eip" "edge" {
  count  = var.edge_count
  domain = "vpc"

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-edge-${count.index + 1}"
    tier = "edge"
    node = count.index + 1
  })

  # An EIP released by accident is gone — AWS will not give the same address
  # back, and every carrier allowlist referencing it is now wrong. This is the
  # one resource in the stack worth making inconvenient to destroy.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_eip_association" "edge" {
  count = var.edge_count

  instance_id   = aws_instance.edge[count.index].id
  allocation_id = aws_eip.edge[count.index].id
}
