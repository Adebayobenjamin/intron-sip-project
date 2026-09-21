########################################################
# Secrets Manager containers.
#
# TERRAFORM OWNS THE CONTAINER, NOT THE VALUE. Every secret below is created
# empty; the string is written out-of-band with bin/env-file-to-sm or
# `aws secretsmanager put-secret-value`. That split is the whole point: a
# secret value passed through Terraform is stored in cleartext in state
# forever, including in every historical version in the state bucket.
#
# Consequence to know about: `terraform plan` will never tell you a secret is
# empty or stale. The services do — at boot, loudly, because
# fetch-and-render-secrets.sh fails closed and takes the systemd unit down
# with it rather than starting a daemon with an unsubstituted ${PLACEHOLDER}
# in its config.
#
# WHY A SEPARATE STACK rather than one container per owning stack:
# intron-sip/<env>/iac has to exist before any stack that reads TF_VAR_*
# values through bin/aws-env-run can run at all, which includes every stack
# that would otherwise own its own container. This one is on the Makefile's
# NO_IAC_SECRET_STACKS list for exactly that reason, and once it is here the
# rest may as well be too — one place to look for the list of credentials
# this platform has.
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
    stack        = "secrets-manager"
  }

  ########################################################
  # One entry per container. `description` is not decoration — it is the only
  # record of which keys a container is expected to hold, and it is what the
  # next person reads when a service will not start.
  #
  # recovery_window_in_days = 7 on all of them. Deleting a secret is
  # reversible for a week, which is the right shape for a credential: long
  # enough to notice, short enough that a rotated-away secret actually goes.
  ########################################################
  secrets = {
    iac = {
      component   = "terraform"
      description = <<-DESC
        TF_VAR_* values for Terraform, loaded into the process by
        bin/aws-env-run so they never sit in a tfvars file on disk. Read by
        the OPERATOR, not by any instance — no VM role is granted access.

        Expected keys (add as stacks need them):
          TF_VAR_carrier_sip_ips   JSON list of ITSP signalling CIDRs
          TF_VAR_alarm_email       Where operational alarms go
      DESC
    }

    ansible = {
      component   = "ansible"
      description = <<-DESC
        Operator-side values for ansible-playbook runs. Same shape and same
        audience as the iac container — read by bin/aws-env-run on a laptop,
        never by a VM.
      DESC
    }

    # NO kamailio CONTAINER. Kamailio reads no credentials at all:
    # CONTROL_PLANE_API_TOKEN went when admission moved into the ESL controller
    # (in-process, against Postgres), and FREESWITCH_ESL_PASSWORD went when
    # originate became controller -> FreeSWITCH over ESL. Nothing in
    # kamailio.cfg referenced either.
    #
    # Removed WITH its consumers rather than left empty - the secrets.cfg
    # template, the import_file, the unit's ExecStartPre hook and the edge
    # role's secrets-read IAM policy all went in the same change. A container
    # that still exists is a thing the next person populates.
    #
    # recovery_window_in_days = 7 below applies: the deleted secret is
    # restorable for a week, and the only value it held
    # (FREESWITCH_ESL_PASSWORD) is duplicated in the freeswitch and
    # esl-controller containers anyway.

    freeswitch = {
      component   = "freeswitch"
      description = <<-DESC
        FreeSWITCH runtime secrets. Fetched at service start by
        /usr/local/bin/fetch-and-render-secrets.sh via the FreeSWITCH instance
        profile.

        Expected keys:
          FREESWITCH_ESL_PASSWORD      Event Socket auth. The ESL controller
                                       uses this for its INBOUND connections
                                       (controller -> FS :8021).
          FREESWITCH_XMLRPC_USER       HTTP Basic user for mod_xml_rpc
          FREESWITCH_XMLRPC_PASSWORD   HTTP Basic password for mod_xml_rpc

        FREESWITCH_ESL_PASSWORD MUST MATCH the value in the esl-controller
        container exactly. TWO copies now, not three - the kamailio container
        is gone, because Kamailio no longer originates and so no longer needs
        the ESL password. Nothing keeps the remaining two in step; rotate both
        together or inbound ESL fails auth with a message naming neither.
      DESC
    }

    esl-controller = {
      component   = "esl-controller"
      description = <<-DESC
        ESL controller runtime config + secrets. Rendered into the service's
        env file at start by the app-service systemd unit.

        Expected keys:
          FREESWITCH_ESL_PASSWORD   must equal the freeswitch container's copy
          DATABASE_URL              the controller reads and writes Postgres
                                    DIRECTLY - admission and every call-state
                                    write. Written by bin/db-dsn-to-sm, which
                                    merges into this container rather than
                                    overwriting it.
          LIVEKIT_API_KEY           for room creation on the LiveKit side
          LIVEKIT_API_SECRET
          LIVEKIT_URL

        NO CONTROL_PLANE_API_URL / CONTROL_PLANE_API_TOKEN. The controller does
        not call the control plane at all any more - both went away when it
        got a database credential of its own. That also removes a copy of a
        shared token nothing kept in step.
      DESC
    }

    control-plane = {
      component   = "control-plane"
      description = <<-DESC
        Control-plane REST API runtime config + secrets.

        Expected keys:
          DATABASE_URL              Postgres DSN (RDS; the data tier is built
                                    but empty today)
          CONTROL_PLANE_API_TOKEN   operator/API callers only. NOT presented by
                                    Kamailio or the ESL controller any more -
                                    neither calls this API
          JWT_SIGNING_KEY           for operator/API auth
          TWILIO_ACCOUNT_SID        Elastic SIP trunk management for the POC
          TWILIO_AUTH_TOKEN
      DESC
    }

    livekit = {
      component   = "livekit"
      description = <<-DESC
        LiveKit server credentials, for the box in stacks/livekit.

        Expected keys:
          LIVEKIT_API_KEY      key identifier. Conventionally starts "API"
          LIVEKIT_API_SECRET   the signing secret. 32+ random bytes

        THE SAME PAIR LIVES IN THREE PLACES and nothing keeps them in step:

          this container            the server itself, validating tokens
          intron-sip/<env>/esl-controller
                                    our controller, signing tokens to create
                                    rooms
          Intron's agent .env       their worker, in a different AWS account,
                                    registering over wss://

        Rotating means updating all three. Miss the third and their agent
        stops picking up jobs with an authentication error on their side and
        nothing at all on ours - the call lands in a room and sits there with
        no agent, which reads as a model problem rather than a credential one.

        THIS IS THE ONLY THING PROTECTING THE SERVER. Its security group is
        open to the internet on purpose (the clients are a browser and a
        process behind somebody else's NAT), so the token signature is the
        entire access control. Generate it with `openssl rand -base64 32`,
        not by hand.
      DESC
    }
  }
}

resource "aws_secretsmanager_secret" "this" {
  for_each = local.secrets

  name                    = "intron-sip/${var.environment}/${each.key}"
  description             = each.value.description
  recovery_window_in_days = 7

  tags = merge(local.tags, {
    component = each.value.component
  })
}
