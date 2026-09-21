########################################################
# FARM — FreeSWITCH, private subnet, no public address.
#
# Sized for the source build, not for the call load. The Ansible role compiles
# FreeSWITCH 1.10 from git along with sofia-sip and spandsp; on two vCPUs with
# 4 GB that takes the better part of an hour and is prone to being OOM-killed
# partway through, which presents as a play that fails deep inside a `make`
# with no obvious cause. Four vCPUs and 8 GB makes the build routine.
#
# Call capacity on that shape is not the constraint anyone will hit first —
# the RTP port range is. 16384-32768 is 16,384 ports at two per session, so
# roughly 8,000 sessions and half that in bridged calls, however much CPU is
# spare.
########################################################

resource "aws_instance" "freeswitch" {
  count = var.freeswitch_count

  ami           = data.aws_ami.debian.id
  instance_type = var.freeswitch_instance_type
  subnet_id     = local.sip_subnet_id
  private_ip    = local.freeswitch_private_ips[count.index]

  vpc_security_group_ids = [aws_security_group.freeswitch.id]
  iam_instance_profile   = aws_iam_instance_profile.instance["freeswitch"].name

  key_name = null

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = var.freeswitch_root_volume_size
    encrypted   = true

    # Bigger than the edge's because the source tree stays on the box. That is
    # deliberate rather than untidy: /usr/src/freeswitch is what lets the role
    # build a single missing module in place (`make -C src/mod/mod_foo`)
    # instead of recompiling the whole tree, and it is several GB with the
    # sound and music-on-hold packages alongside it.
    delete_on_termination = true

    tags = merge(local.tags, {
      Name = "${local.name_prefix}-freeswitch-${count.index + 1}-root"
    })
  }

  user_data = templatefile("${path.module}/scripts/vm-bootstrap.sh.tftpl", {
    hostname   = "${var.environment}-freeswitch-${count.index + 1}"
    aws_region = var.aws_region
    role       = "freeswitch"
  })

  user_data_replace_on_change = true

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-freeswitch-${count.index + 1}"
    tier = "sip"
    role = "freeswitch"
    node = count.index + 1
  })
}

########################################################
# Recordings volume — separate EBS, mounted at /mnt/recordings.
#
# Separate from the root volume for one reason: the thing most likely to fill
# it is recordings, and a full root volume takes FreeSWITCH down with it along
# with sshd's replacement (the SSM agent), turning a disk-space problem into a
# box you cannot log into to fix it. On its own volume, the same overflow
# stops recordings and leaves calls and access working.
#
# Sized by UPLOAD LAG, not by call volume — the uploader ships files to S3 and
# deletes them, so this only ever holds what has been written and not yet
# shipped. 100 GB is a lot of that. It is not sized by retention, because
# retention happens in S3.
#
# gp3 at baseline: 3,000 IOPS and 125 MB/s with no provisioning, which is far
# beyond what sequential WAV writes need.
########################################################
resource "aws_ebs_volume" "recordings" {
  count = var.freeswitch_count

  availability_zone = local.primary_az
  size              = var.freeswitch_recordings_volume_size
  type              = "gp3"
  encrypted         = true

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-freeswitch-${count.index + 1}-recordings"
    tier = "sip"
    role = "freeswitch-recordings"
    node = count.index + 1
  })
}

resource "aws_volume_attachment" "recordings" {
  count = var.freeswitch_count

  # Nitro instances ignore the requested name and expose NVMe devices as
  # /dev/nvme[0-9]n1 in attachment order, so the Ansible role must find this
  # volume by its NVMe serial (which carries the EBS volume ID) rather than by
  # path. Naming it here anyway because the API requires a device name and
  # this one is conventional for a secondary volume.
  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.recordings[count.index].id
  instance_id = aws_instance.freeswitch[count.index].id

  # Detach cleanly on destroy. Without this a `terraform destroy` can hang
  # waiting for a volume the instance still has mounted.
  stop_instance_before_detaching = true
}
