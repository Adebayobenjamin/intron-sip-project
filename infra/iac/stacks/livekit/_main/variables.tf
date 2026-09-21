variable "environment" {
  type        = string
  description = "Environment name. Prefixes every resource and selects the remote-state keys this stack reads."
}

variable "aws_region" {
  type        = string
  description = "See stacks/network/beta/variables.tf for why eu-central-1 and not af-south-1."
}

variable "aws_profile" {
  type        = string
  description = <<-DESC
    Shared-config profile Terraform authenticates with.

    Passed through to the terraform_remote_state lookups as well as the
    provider — a remote_state data source configures its OWN backend and
    inherits nothing from the provider block. Miss that and the provider
    authenticates fine while a state lookup falls back to the default
    credential chain, erroring with a message about the bucket rather than the
    credentials.
  DESC
}

variable "state_bucket" {
  type        = string
  description = "State bucket, created by the tfstate stack. Read here to configure the remote_state lookups."
}

variable "state_bucket_region" {
  type        = string
  description = "Region the state bucket lives in. Separate from aws_region so state can stay put if a stack moves."
}

########################################################
# Instances
########################################################

variable "livekit_count" {
  type        = number
  description = <<-DESC
    Number of LiveKit server instances.

    ONE, and more than one is not a matter of raising this number. LiveKit
    scales horizontally only with a shared Redis and a node-aware routing
    setup; two instances sharing nothing would put a caller and their agent in
    the same-named room on different servers, which presents as a call that
    connects and hears silence. Redis runs on this box today for exactly the
    single-node case.
  DESC
  default     = 1

  validation {
    condition     = var.livekit_count == 1
    error_message = "Multi-node LiveKit needs a shared Redis and node-aware routing, neither of which this stack builds. See the variable description."
  }
}

variable "livekit_instance_type" {
  type        = string
  description = <<-DESC
    LiveKit is a Selective Forwarding Unit — it relays RTP packets between
    participants and does not decode, mix or transcode them. So this is a
    network-bound workload with a small CPU cost per stream, not a media
    workload, and it needs no GPU: the STT/TTS models stay on Intron's GPU
    box in eu-west-2 and reach this server as an ordinary WebRTC client.

    t3.small is sized for proving the integration and a handful of concurrent
    calls. The thing to watch before real volume is NETWORK, not CPU — t3
    baseline bandwidth is modest and burst credits are finite.
  DESC
  default     = "t3.small"
}

variable "livekit_root_volume_size" {
  type        = number
  description = "Root volume, GB. Holds the OS, container images and Caddy's certificate store. Nothing here grows with call volume — LiveKit records nothing locally."
  default     = 20
}

########################################################
# DNS
########################################################

variable "public_dns_zone" {
  type        = string
  description = <<-DESC
    Public hosted zone this stack CREATES and owns, e.g. intron.quiverlearn.com.

    A SUBDOMAIN, not the apex. quiverlearn.com is managed elsewhere, and this
    stack takes ownership of one level below it so Terraform can manage records
    without touching whatever holds the parent.

    THE DELEGATION IS MANUAL AND IT IS THE STEP THAT BLOCKS EVERYTHING ELSE.
    Creating the zone here does nothing on its own — the parent zone's operator
    must add the four NS records this stack outputs. Until they do, the name
    does not resolve, Caddy cannot complete the ACME HTTP-01 challenge, there
    is no certificate, and Intron's agent cannot connect over wss://. The
    failure reads as a TLS error, which points nowhere near DNS.

    Verify with:  dig +short NS <this zone>
    against a resolver that is not your own, and expect the four names in
    `livekit_zone_nameservers`.
  DESC
}

variable "livekit_subdomain" {
  type        = string
  description = <<-DESC
    Host part of the LiveKit name, prepended to public_dns_zone.

    This is the string that ends up in THREE places outside this repo, and they
    must agree: Caddy's certificate request on the box, LIVEKIT_URL in Intron's
    agent environment, and LIVEKIT_URL in this platform's esl-controller
    secret. It is a DNS name rather than the Elastic IP precisely so the
    address can change without a conversation with Intron.
  DESC
  default     = "livekit-beta"
}

variable "livekit_turn_subdomain" {
  type        = string
  description = <<-DESC
    A SECOND name, for the same instance, used only by TURN over TLS.

    WHY TWO NAMES FOR ONE BOX. Both TURN/TLS and the LiveKit WebSocket have to
    answer on 443, because 443 is the one port a locked-down network reliably
    permits. Caddy tells them apart by TLS SNI, and SNI is a HOSTNAME - so
    demultiplexing requires two hostnames. One name means TURN/TLS cannot be
    offered at all.

    That matters more here than it looks. The fallback chain is direct UDP,
    then TCP, then TURN; a caller inside a hospital or corporate network is
    frequently on the last rung. Losing it presents as a call that connects,
    shows both participants, and carries no audio - for some callers and not
    others, which is the hardest version of that bug to chase.
  DESC
  default     = "livekit-beta-turn"
}

########################################################
# Access
########################################################

variable "livekit_client_cidrs" {
  type        = list(string)
  description = <<-DESC
    Who may reach the signalling and media ports.

    DEFAULTS TO THE WHOLE INTERNET, and unlike carrier_sip_cidrs in sip-core
    that is the correct default rather than a dangerous one. The clients here
    are a browser on somebody's laptop and an agent process in a different AWS
    account behind a NAT gateway whose address nobody controls. There is no
    stable list to write.

    WHAT PROTECTS THIS BOX IS NOT THE SECURITY GROUP. It is that every LiveKit
    API call and every room join requires a JWT signed with the API secret.
    An unauthenticated connection gets a 401 and nothing else. That is a
    different security model from the SIP edge, where the allowlist IS the
    control, and the difference is worth being explicit about rather than
    discovering when someone "tightens" this to match.

    Narrow it if the client set ever becomes knowable — a fixed office range
    plus Intron's egress IP, for instance.
  DESC
  default     = ["0.0.0.0/0"]
}

variable "rtc_udp_port_min" {
  type        = number
  description = <<-DESC
    Low end of the WebRTC media range.

    MUST MATCH rtc.port_range_start in the rendered livekit.yaml. Terraform
    opens what the daemon binds to, and the two are configured in different
    places — a mismatch presents as a call that connects, shows both
    participants, and carries no audio. Nothing in any log names a port.
  DESC
  default     = 50000
}

variable "rtc_udp_port_max" {
  type        = number
  description = "High end of the WebRTC media range. Must match rtc.port_range_end. See rtc_udp_port_min."
  default     = 60000
}

variable "rtc_tcp_port" {
  type        = number
  description = <<-DESC
    WebRTC over TCP, for clients on networks that block UDP entirely.

    Worth opening even though it is the slow path: without it a caller behind a
    restrictive corporate firewall fails to establish media at all, and the
    symptom is indistinguishable from the server being down.
  DESC
  default     = 7881
}

variable "turn_udp_port" {
  type        = number
  description = "LiveKit's embedded TURN server. The last-resort relay when a client can reach neither the UDP range nor the TCP port directly."
  default     = 3478
}

variable "turn_relay_port_min" {
  type        = number
  description = <<-DESC
    Low end of the TURN RELAY range, and a separate thing from turn_udp_port.

    3478 is where a client ASKS for a relay. The relay itself is then allocated
    on a port in this range, and the media flows there - so opening 3478 alone
    produces a TURN server that accepts the request and then relays to a port
    nothing can reach.

    THE FAILURE IS PARTIAL, WHICH IS WHY IT SURVIVES TESTING. Only clients that
    actually fall back to TURN are affected - behind a symmetric NAT or a
    firewall that blocks the direct media range. Everyone testing from a normal
    network takes the direct path, sees working audio, and concludes TURN is
    fine. The people it fails for are the ones inside a hospital or corporate
    network, which on this platform is the population that matters most.

    MUST MATCH turn.relay_range_start in the rendered livekit.yaml. Found by
    reading the server's own startup log, which prints the range it chose:
      Starting TURN server  turn.relay_range_start: 30000 ...
  DESC
  default     = 30000
}

variable "turn_relay_port_max" {
  type        = number
  description = "High end of the TURN relay range. Must match turn.relay_range_end. See turn_relay_port_min."
  default     = 40000
}

########################################################
# livekit-sip — the bridge between our SIP platform and LiveKit rooms.
#
# EVERYTHING BELOW IS PRIVATE, and that is the whole reason this stack exists
# rather than dialling Intron's LiveKit across the internet. The only thing
# that sends SIP here is our own Kamailio, in this VPC, so these rules take
# the EDGE SECURITY GROUP as their source rather than a CIDR - let alone
# 0.0.0.0/0 like the WebRTC ports above.
#
# The consequence worth stating: SIP and RTP never leave the VPC, so the
# SIP-TLS certificate question and livekit_srtp_required both disappear. They
# were only ever needed because the leg used to cross the public internet.
########################################################

variable "enable_sip" {
  type        = bool
  description = <<-DESC
    Whether to open the SIP and RTP ports, and run livekit-sip.

    Separate from the rest of the stack because the browser path was proven
    first, deliberately: it establishes that DNS, TLS, the WebSocket and the
    agent link all work with no SIP involved. When a call later fails, that
    ordering is what tells you which half to look at.
  DESC
  default     = true
}

variable "sip_port" {
  type        = number
  description = <<-DESC
    Where livekit-sip listens for INVITEs from our Kamailio.

    5060 is fine here, unlike on Intron's box where livekit-sip had to move to
    5160 because OpenSIPS already held 5060 on the same host. Nothing else on
    this instance binds it.

    MUST MATCH sip_port in the rendered sip.yaml, and livekit_sip_port in the
    Ansible inventory, which is what Kamailio's dispatcher dials.
  DESC
  default     = 5060
}

variable "sip_rtp_port_min" {
  type        = number
  description = <<-DESC
    Low end of the RTP range livekit-sip uses for SIP-side media.

    A DIFFERENT RANGE FROM rtc_udp_port_min. That one carries WebRTC media to
    browsers and agents; this one carries plain RTP to and from RTPEngine on
    the edge. Two media paths meet inside livekit-sip, and conflating their
    port ranges in the security group produces audio that works in one
    direction only.

    MUST MATCH rtp_port in the rendered sip.yaml.
  DESC
  default     = 10000
}

variable "sip_rtp_port_max" {
  type        = number
  description = "High end of the SIP-side RTP range. Must match rtp_port in sip.yaml. See sip_rtp_port_min."
  default     = 20000
}
