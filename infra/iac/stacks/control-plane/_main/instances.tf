########################################################
# App instances.
#
# Plain aws_instance with a count, not an Auto Scaling Group. That is a
# deliberate choice and worth the sentence:
#
# An ASG's value is replacing instances automatically and scaling on a metric.
# Neither helps here yet. Automatic replacement of a node holding live ESL
# sockets is a liability rather than a feature — the sockets go with it — and
# scaling on CPU is meaningless for a service whose load is "number of calls in
# progress", a metric that does not exist until the controller publishes it.
#
# Static instances also keep them addressable by the Ansible inventory the same
# way every other box in this repo is, with one access model and one playbook
# shape. Revisit when there is a real concurrency metric to scale on.
########################################################

resource "aws_instance" "app" {
  count = var.app_count

  ami           = data.aws_ami.debian.id
  instance_type = var.app_instance_type
  subnet_id     = local.app_placement[count.index].subnet_id
  private_ip    = local.app_placement[count.index].private_ip

  vpc_security_group_ids = [aws_security_group.app.id]
  iam_instance_profile   = aws_iam_instance_profile.app.name

  # No SSH key anywhere in this repo. SSM Session Manager or nothing.
  key_name = null

  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"

    ########################################################
    # HOP LIMIT 2, NOT 1 — and this is the one place in the repo it differs.
    #
    # This tier runs Docker. A container's traffic to 169.254.169.254 is
    # forwarded by the host's bridge, which costs one IP hop, so a limit of 1
    # means the packet is dropped before reaching IMDS. The symptom is the
    # container failing to find AWS credentials while `aws sts
    # get-caller-identity` on the host works perfectly — which sends people
    # looking at IAM for an afternoon.
    #
    # The trade is real: hop limit 2 is also what lets a compromised container
    # reach the host's instance credentials. That is why the role attached to
    # this profile is scoped as tightly as it is (two secret containers, this
    # project's ECR repos, these two target groups) rather than relying on
    # containers being unable to reach IMDS at all.
    ########################################################
    http_put_response_hop_limit = 2
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = var.app_root_volume_size
    encrypted   = true

    # Sized for the Docker image cache more than anything else. Immutable tags
    # mean every deploy pulls a new image and the old layers linger until
    # something prunes them.
    delete_on_termination = true

    tags = merge(local.tags, {
      Name = "${local.name_prefix}-app-${count.index + 1}-root"
    })
  }

  user_data = templatefile("${path.module}/scripts/vm-bootstrap.sh.tftpl", {
    hostname   = "${var.environment}-app-${count.index + 1}"
    aws_region = var.aws_region
    role       = "app"
  })

  user_data_replace_on_change = true

  tags = merge(local.tags, {
    Name = "${local.name_prefix}-app-${count.index + 1}"
    tier = "app"
    role = "esl-controller,control-plane"
    node = count.index + 1
  })
}
