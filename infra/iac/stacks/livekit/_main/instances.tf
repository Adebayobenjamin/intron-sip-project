########################################################
# The LiveKit server.
#
# Runs, as containers under systemd (see ansible/roles/livekit):
#
#   livekit-server   the SFU and its API. Where rooms live.
#   redis            LiveKit's required backing store, single-node
#   caddy            TLS termination, and the ACME client that gets the cert
#
# NO GPU, and the reason is worth stating because "AI voice agent" suggests
# otherwise: LiveKit forwards RTP between participants. It does not decode,
# mix, transcode or run a model. The STT/TTS work happens on Intron's T4 in
# eu-west-2, in a process that connects here as an ordinary client.
########################################################

resource "aws_instance" "livekit" {
  count = var.livekit_count

  ami           = data.aws_ami.debian.id
  instance_type = var.livekit_instance_type
  subnet_id     = local.subnet_id
  private_ip    = local.livekit_private_ips[count.index]

  vpc_security_group_ids = [aws_security_group.livekit.id]
  iam_instance_profile   = aws_iam_instance_profile.livekit.name

  # No SSH key anywhere in this repo, no port 22 rule in the group above, and
  # no bastion. Attaching a key pair here would open a second path in that
  # nothing else in this repo defends.
  key_name = null

  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"

    ########################################################
    # HOP LIMIT 2, matching the app tier and differing from the SIP edge.
    #
    # This box runs Docker. A container's traffic to 169.254.169.254 crosses
    # the host bridge, which costs one IP hop, so a limit of 1 drops the
    # packet before it reaches IMDS. The symptom is the container failing to
    # find AWS credentials while `aws sts get-caller-identity` on the host
    # works perfectly, which sends people to IAM for an afternoon.
    #
    # The trade is that a compromised container can reach the host's instance
    # credentials. That is why the role attached below reads exactly one
    # secret container and nothing else.
    ########################################################
    http_put_response_hop_limit = 2
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = var.livekit_root_volume_size
    encrypted   = true

    ########################################################
    # Deleted with the instance, and one thing is lost when that happens:
    # Caddy's certificate store in /var/lib/caddy.
    #
    # That is acceptable rather than overlooked. Caddy re-issues on first boot
    # from the ACME account it recreates, and the cost is a few seconds of
    # startup. What it is NOT acceptable to do is replace this instance
    # repeatedly in a short window — Let's Encrypt rate-limits to 5 identical
    # certificates per week, and hitting that locks the name out for days with
    # an error that mentions nothing about instance replacement.
    ########################################################
    delete_on_termination = true

    tags = merge(local.tags, {
      Name = "${local.name_prefix}-${count.index + 1}-root"
    })
  }

  user_data = templatefile("${path.module}/scripts/vm-bootstrap.sh.tftpl", {
    hostname   = "${var.environment}-livekit-${count.index + 1}"
    aws_region = var.aws_region
    role       = "livekit"
  })

  # A user_data change replaces the instance, which detaches and reattaches the
  # Elastic IP. Deliberately not ignored: nothing rotates in that script, so a
  # diff means somebody actually changed the bootstrap and should see the
  # replacement coming in the plan.
  user_data_replace_on_change = true

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-${count.index + 1}"
    tier = "livekit"
    role = "livekit-server"
    node = count.index + 1
  })
}

########################################################
# Elastic IP.
#
# THE INSTANCE NEVER SEES THIS ADDRESS — AWS maps it 1:1 at the VPC edge and
# the OS only ever holds the private 10.70.x.x address. WebRTC carries
# addresses in its payload the same way SIP does, so LiveKit has to be told:
# `use_external_ip: true` in livekit.yaml makes it discover this address via
# STUN at startup and advertise it in ICE candidates.
#
# Get that wrong and the failure is the one this repo keeps meeting from
# different directions: signalling is clean, both participants appear in the
# room, and there is no audio.
#
# WHY NO prevent_destroy, unlike the edge EIP.
# The edge address is on carrier allowlists — losing it means a ticket with
# the ITSP and an outage lasting as long as they take. This address is reached
# through a DNS name that this stack also owns, so replacing it is an A record
# update and a certificate re-issue. Inconvenient, not expensive.
########################################################
resource "aws_eip" "livekit" {
  count  = var.livekit_count
  domain = "vpc"

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-${count.index + 1}"
    tier = "livekit"
    node = count.index + 1
  })
}

resource "aws_eip_association" "livekit" {
  count = var.livekit_count

  instance_id   = aws_instance.livekit[count.index].id
  allocation_id = aws_eip.livekit[count.index].id
}
