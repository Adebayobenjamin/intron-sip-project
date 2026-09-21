variable "environment" {
  type        = string
  description = "Environment name. Prefixes resource names and selects the upstream stacks' state keys."
}

variable "aws_region" {
  type        = string
  description = "Region. Must match the provider — it is baked into the SSM agent download URL in user_data."
}

variable "state_bucket" {
  type        = string
  description = "S3 bucket holding every stack's state. Read by the terraform_remote_state lookups in remote-state.tf."
}

variable "state_bucket_region" {
  type        = string
  description = "Region of the state bucket. Usually the same as aws_region, but kept separate so the two can diverge without editing remote-state.tf."
}

########################################################
# Carrier / trunk access
########################################################

variable "carrier_sip_cidrs" {
  type        = list(string)
  description = <<-DESC
    Source ranges allowed to send SIP signalling to the edge on 5060/5061.

    DEFAULTS TO EMPTY, and that is the safe default rather than an oversight.
    Empty means no calls arrive — obvious within a minute of testing. The
    tempting alternative, 0.0.0.0/0, means calls arrive and so does everything
    else: an open 5060 is found by scanners within hours of first responding,
    and the first thing they try is toll fraud.

    POPULATE FROM THE CARRIER, NOT FROM MEMORY. Both sources in the
    architecture diagram publish their ranges and both change them:

      Twilio Elastic SIP Trunking — the signalling ranges for the region the
        trunk is homed in, from Twilio's "SIP Trunking IP addresses" doc.
        Region matters: a trunk homed in Dublin does not signal from the same
        addresses as one homed in Ashburn.

      Client BYOT trunks — whatever the client's own SBC signals from. One
        entry per client, added when they are onboarded.

    THIS IS SIGNALLING ONLY. Do not add media ranges here — RTP arrives from
    addresses that cannot be enumerated, which is why that rule is separate
    and deliberately wide. See the note above aws_vpc_security_group_ingress_rule.edge_rtp.

    Supplied through the iac Secrets Manager container as TF_VAR_carrier_sip_cidrs
    so the list can be updated without a commit.
  DESC
  default     = []
}

variable "livekit_cidrs" {
  type        = list(string)
  description = <<-DESC
    Ranges livekit-sip reaches the edge from, for the AI leg (arrow 4).

    INTERIM: LiveKit lives in a different AWS account, so these are its PUBLIC
    egress addresses and the leg is an ordinary internet SIP trunk. Use /32s of
    the actual addresses — an Elastic IP if livekit-sip has one, otherwise the
    NAT gateway address its egress appears as.

    IF livekit-sip SITS BEHIND A NAT GATEWAY WITH NO ELASTIC IP, inbound SIP
    and media cannot reach it at all and this half of the config is moot. It
    needs a stable, directly-reachable public address and its own media ports
    open. That is a LiveKit-side prerequisite, not something this stack can
    arrange.

    MIGRATING LATER IS A CHANGE TO THIS VARIABLE AND NOTHING ELSE. Once the
    accounts are peered, replace the public /32s with the private subnet CIDRs;
    no rule is restructured. The Ansible side is the matching one-liner
    (livekit_sip_host). See docs/LIVEKIT.md for the full sequence and for the
    tunnel variant if LiveKit turns out not to be on AWS.

    Leaving it empty means the AI leg does not connect, which is the right
    failure. A guessed range that happens to be routable is worse than one
    that is obviously absent.
  DESC
  default     = []
}

variable "livekit_sip_transport" {
  type        = string
  description = <<-DESC
    Transport for the LiveKit signalling leg: "tls" or "udp".

    DEFAULTS TO TLS, and that differs from the carrier leg on purpose. Carriers
    interconnect over plain UDP because that is the industry norm and the path
    is short and commercially accountable. This leg is our own traffic crossing
    the OPEN INTERNET between two accounts, and it carries clinical
    conversation audio — plain RTP on that path is readable by anyone who sits
    on it.

    "tls" opens 5061/tcp instead of 5060/udp and expects SRTP media. Both ends
    need it: confirm the livekit-sip version in use supports TLS transport and
    SRTP before switching this, because a mismatch presents as calls that set up
    and then fail at media negotiation rather than as an obvious config error.

    "udp" is available for debugging — a tcpdump of plain SIP is enormously
    easier to read than a TLS session — and for the case where LiveKit is
    reached over a tunnel that already encrypts. Choosing it for an internet
    path is a decision to send audio in the clear.
  DESC
  default     = "tls"

  validation {
    condition     = contains(["tls", "udp"], var.livekit_sip_transport)
    error_message = "livekit_sip_transport must be \"tls\" or \"udp\"."
  }
}

variable "sips_port" {
  type        = number
  description = "SIP over TLS. Used by both the carrier rules and the LiveKit leg when livekit_sip_transport is \"tls\"."
  default     = 5061
}

########################################################
# Edge tier — Kamailio + RTPEngine
########################################################

variable "edge_count" {
  type        = number
  description = <<-DESC
    Edge instances.

    ONE for beta, and that is a single point of failure for the entire
    platform: lose it and every call drops and no new call can arrive. Named
    rather than hidden because the fix is not just raising this number —
    two Kamailio nodes need the Elastic IP to move between them (a health-
    checked reassociation, or a Network Load Balancer in front), and
    registration state has to be shared rather than held per-node.

    Raising this to 2 without that work gives two boxes, one of which receives
    nothing.
  DESC
  default     = 1
}

variable "edge_instance_type" {
  type        = string
  description = <<-DESC
    c6i.large — 2 vCPU, 4 GiB, up to 12.5 Gbps burst.

    The thing this tier is actually bound by is PACKETS PER SECOND, not
    bandwidth: RTPEngine relays 50 small packets/second per direction per call,
    so a few hundred concurrent calls is tens of thousands of pps of pure
    per-packet overhead, on frames far too small to reach the bandwidth cap.

    c6in.large is the upgrade path when that starts to bite — same vCPU count,
    substantially higher packet-per-second allowance, roughly 25% more money.
    Move to it when RTPEngine starts showing packet loss that does not
    correlate with CPU being busy, which is the signature of hitting the
    instance's pps allowance rather than running out of compute.
  DESC
  default     = "c6i.large"
}

variable "edge_root_volume_size" {
  type        = number
  description = "GB. Kamailio and RTPEngine come from packages and hold nothing durable; 30 is comfortable."
  default     = 30
}

# kamailio_xhttp_port is DELETED. It sized the security group rule for
# Kamailio's xhttp control listener, which the control plane used to drive
# outbound originate. Originate is now controller -> FreeSWITCH over ESL, so
# there is no listener, no rule, and nothing for this to configure.
#
# Removed rather than left with no reference: an unused variable with a
# plausible description is indistinguishable from one that is wired up, and
# the next person to need a control channel sets it and wonders why nothing
# opens.

variable "rtpengine_port_min" {
  type        = number
  description = <<-DESC
    Bottom of RTPEngine's media port range. Must match rtpengine_port_min in
    the Ansible inventory — the security group opens a range and RTPEngine
    allocates within one, and a mismatch means calls set up cleanly with no
    audio.
  DESC
  default     = 30000
}

variable "rtpengine_port_max" {
  type        = number
  description = "Top of RTPEngine's media port range. 10,000 ports at two per call is roughly 5,000 concurrent legs."
  default     = 40000
}

########################################################
# FreeSWITCH farm
########################################################

variable "freeswitch_count" {
  type        = number
  description = <<-DESC
    FreeSWITCH nodes.

    ONE for beta. What that gives up, stated plainly: Kamailio's dispatcher
    list holds a single destination, so nothing in this environment exercises
    how it distributes or fails over between nodes. That is a real class of bug
    this environment cannot catch.

    Raising it is genuinely just this number plus an Ansible run — the
    dispatcher list is rendered from the inventory, and FreeSWITCH nodes hold
    no shared state.
  DESC
  default     = 1
}

variable "freeswitch_instance_type" {
  type        = string
  description = <<-DESC
    c6i.xlarge — 4 vCPU, 8 GiB.

    Sized for the SOURCE BUILD rather than the call load. The role compiles
    FreeSWITCH, sofia-sip and spandsp from git; on 2 vCPU / 4 GB that build
    takes close to an hour and gets OOM-killed often enough to be the first
    thing to suspect when a play dies inside a `make` with no clear error.
  DESC
  default     = "c6i.xlarge"
}

variable "freeswitch_root_volume_size" {
  type        = number
  description = "GB. Holds the /usr/src/freeswitch build tree (several GB with sounds and MoH), which stays on the box so single modules can be rebuilt in place."
  default     = 50
}

variable "freeswitch_recordings_volume_size" {
  type        = number
  description = <<-DESC
    GB for the separate recordings volume at /mnt/recordings.

    Sized by UPLOAD LAG, not retention — the uploader ships to S3 and deletes,
    so this only holds what has been written and not yet shipped. Retention
    lives in S3. Separate from root so a recordings overflow cannot take down
    FreeSWITCH and the SSM agent along with it.
  DESC
  default     = 100
}

variable "freeswitch_rtp_port_min" {
  type        = number
  description = "Bottom of FreeSWITCH's RTP range (vanilla default). Must match the Ansible inventory."
  default     = 16384
}

variable "freeswitch_rtp_port_max" {
  type        = number
  description = "Top of FreeSWITCH's RTP range. 16,384 ports at two per session is the real concurrency ceiling on a node, ahead of CPU or memory."
  default     = 32768
}

variable "freeswitch_esl_inbound_port" {
  type        = number
  description = "Event Socket listener on FreeSWITCH. The controller connects IN here to originate and subscribe to events — the opposite direction from the per-call &socket()."
  default     = 8021
}

variable "freeswitch_xmlrpc_port" {
  type        = number
  description = "mod_xml_rpc. Second control path into FreeSWITCH, used for originate when ESL is not convenient."
  default     = 8085
}

variable "recordings_bucket_arn" {
  type        = string
  description = <<-DESC
    ARN of the call-recordings bucket, if one exists. Empty disables the
    FreeSWITCH role's s3:PutObject policy entirely.

    Empty today: recording is planned, not built. Wiring the policy behind a
    variable now means the day the bucket exists this is one value and an
    apply, rather than a new IAM policy written under time pressure.
  DESC
  default     = ""
}

variable "aws_profile" {
  type        = string
  description = <<-DESC
    Shared-config profile for the terraform_remote_state lookups in
    remote-state.tf.

    Required rather than optional: a remote_state data source configures its
    own backend and inherits nothing from the root module's provider. Without
    it the lookup falls back to the default credential chain and fails to read
    state the provider itself can reach — an error that names the state bucket
    and not the credentials.
  DESC
}
