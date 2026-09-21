########################################################
# ECR — image repositories for the backend services.
#
# The app tier runs containers on EC2: Ansible installs Docker and a systemd
# unit per service, and the unit pulls a tagged image from here at start. So a
# deploy is "push a tag, restart the unit" — nothing is built on the box, and a
# rollback is the previous tag.
#
# WHY IMAGES ON EC2 RATHER THAN A BARE BINARY. Two things, both about the ESL
# controller specifically. It holds TCP sockets for the life of a call, so a
# deploy has to drain rather than restart; having the old and new versions as
# distinct, addressable artifacts is what makes a drain-and-swap possible
# instead of an in-place overwrite. And the ECR pull authenticates with the
# instance profile, so there is no artifact credential to distribute.
########################################################

terraform {
  required_version = ">= 1.10.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

locals {
  tags = {
    environment  = var.environment
    project      = "intron-sip"
    team         = "platform"
    managed_with = "terraform"
    stack        = "ecr"
  }

  repositories = {
    esl-controller = "Outbound-ESL listener. FreeSWITCH opens a socket per call and asks it for a routing decision."
    control-plane  = "REST API + Postgres-backed config: tenants, DIDs, trunks, handoff targets."
  }
}

resource "aws_ecr_repository" "this" {
  for_each = local.repositories

  name = "intron-sip/${each.key}"

  ########################################################
  # IMMUTABLE TAGS. A tag, once pushed, cannot be moved.
  #
  # This is the setting that makes the systemd-unit deploy model trustworthy.
  # With mutable tags, `docker pull intron-sip/esl-controller:v1.4.2` can
  # return different bytes on two instances pulled minutes apart, and two
  # controller nodes end up running different code with nothing to indicate it.
  # It also makes rollback meaningless — the tag you are rolling back to may
  # have been overwritten.
  #
  # The cost is that CI cannot push `latest` or re-push a tag after a failed
  # deploy. That is the intended friction: push a new tag.
  ########################################################
  image_tag_mutability = "IMMUTABLE"

  # Scan on push. Free, and the findings land in ECR where the deploy can look
  # at them. Does not block a pull — this is information, not a gate.
  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = merge(local.tags, {
    Name      = each.key
    component = each.key
  })
}

########################################################
# Expiry. Immutable tags mean images accumulate forever otherwise — one per
# build, and CI builds on every merge.
#
# Two rules, and the ORDER MATTERS because ECR applies the lowest rulePriority
# that matches and stops:
#
#   1. Keep the last 30 release-tagged images. Release tags are what production
#      runs and what a rollback reaches for.
#   2. Expire anything else after 14 days — feature branches, PR builds.
#
# Rule 1 first, so a release image is never caught by the age rule.
########################################################
resource "aws_ecr_lifecycle_policy" "this" {
  for_each = aws_ecr_repository.this

  repository = each.value.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Keep the last 30 release images"
        selection = {
          tagStatus     = "tagged"
          tagPrefixList = var.release_tag_prefixes
          countType     = "imageCountMoreThan"
          countNumber   = 30
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "Expire everything else after 14 days"
        selection = {
          tagStatus   = "any"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 14
        }
        action = { type = "expire" }
      },
    ]
  })
}
