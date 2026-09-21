module "sip_core" {
  source = "../_main"

  environment         = "beta"
  aws_region          = var.aws_region
  state_bucket        = var.state_bucket
  aws_profile         = var.aws_profile
  state_bucket_region = var.aws_region

  ########################################################
  # Trunk access.
  #
  # Empty is the shipped default and an empty list means no calls arrive.
  # Populate via the iac Secrets Manager container:
  #
  #   cd infra/iac && $EDITOR .env
  #   make populate-iac ENV=beta
  #
  # NO EXAMPLE CIDRS HERE ANY MORE, deliberately. This comment used to carry
  # "54.171.127.192/27" and "54.172.60.0/23" as illustrations of the shape,
  # with a warning not to copy them. Both had the wrong prefix length - Twilio
  # publishes /30s at those bases, so copying them would have opened 32 and 512
  # addresses where 4 were meant, and the warning not to copy is exactly the
  # kind nobody reads. An example that is wrong is worse than no example.
  #
  # Current values and their rationale live in iac/.env, next to the value
  # being set. Source of truth is Twilio, which changes them:
  #   https://www.twilio.com/docs/sip-trunking/ip-addresses
  #
  # SIGNALLING ONLY. Media arrives from a different range - the RTP rule in
  # ../_main/security-groups.tf is separate and intentionally wider.
  ########################################################
  carrier_sip_cidrs = var.carrier_sip_cidrs

  ########################################################
  # LiveKit — still empty because nobody has confirmed where Intron's existing
  # self-hosted LiveKit runs. Until this is filled in, the AI leg (arrow 4)
  # does not connect. That is the intended failure; see the variable's
  # description in ../_main/variables.tf for what else has to exist first
  # depending on the answer.
  ########################################################
  livekit_cidrs = var.livekit_cidrs

  ########################################################
  # UDP, because this leg no longer crosses the public internet.
  #
  # It was "tls" when the AI leg dialled Intron's LiveKit in another AWS
  # account, and that was right: our own clinical audio would otherwise have
  # crossed the open internet in the clear.
  #
  # livekit-sip now runs in this VPC (iac/stacks/livekit), reached over private
  # addresses, and its SIP port is open only to this edge's security group. So
  # there is no internet hop left to protect and no certificate to agree
  # between two estates.
  #
  # MUST MATCH livekit_sip_transport in the Ansible inventory, which is what
  # Kamailio's dispatcher actually dials. This decides which port is OPEN; that
  # decides which port is DIALLED. A mismatch presents as calls that set up and
  # then die at media negotiation, reading as a codec problem.
  #
  # Going back to "tls" means changing both, and livekit_srtp_required too.
  ########################################################
  livekit_sip_transport = "udp"

  ########################################################
  # Beta topology: ONE of each.
  #
  # Not prod's shape scaled down — genuinely one edge and one FreeSWITCH node,
  # which is a single point of failure for the platform and is being accepted
  # rather than overlooked. What it still exercises is the part that matters:
  # Kamailio -> FreeSWITCH is real SIP across the VPC, with advertised
  # addresses, Record-Route and a separate media relay. Collapsing the two onto
  # one box was considered and rejected — it turns the signalling path into
  # loopback, and signalling is the class of bug this environment exists to
  # catch.
  #
  # What it stops exercising: failover, and dispatcher behaviour with more than
  # one FreeSWITCH destination.
  ########################################################
  edge_count         = 1
  edge_instance_type = "c6i.large"

  freeswitch_count         = 1
  freeswitch_instance_type = "c6i.xlarge"

  # 100 GB holds a lot of not-yet-uploaded audio. Sized by upload lag, not by
  # retention — retention is an S3 lifecycle policy, once recordings exist.
  freeswitch_recordings_volume_size = 100

  # Recording is planned, not built. Set this to the bucket ARN when the
  # call-recordings stack exists and the FreeSWITCH role gets its
  # s3:PutObject grant with one apply.
  recordings_bucket_arn = ""
}
