variable "environment" {
  type        = string
  description = "Environment name. Prefixes every resource name."
}

variable "aws_region" {
  type        = string
  description = "Region. Used to build VPC endpoint service names — must match the provider's region."
}

variable "vpc_cidr" {
  type        = string
  description = <<-DESC
    CIDR for the one VPC everything lands in. A /16 — the subnet layout uses
    the third octet as a readable tier marker (see locals.subnet_tiers) and
    anything tighter forces that to become arithmetic.

    MUST NOT OVERLAP the network Intron's existing self-hosted LiveKit runs in.
    FreeSWITCH bridges its B-leg to livekit-sip (arrow 4 in the architecture
    diagram), so those two networks have to be routable to each other — by
    peering, Transit Gateway, or a tunnel, depending on where LiveKit actually
    lives. Overlapping ranges make that impossible to fix later without
    renumbering one side, which for a VPC means rebuilding it.

    CONFIRM THE LIVEKIT RANGE BEFORE THE FIRST APPLY. This is the one decision
    in the repo that is genuinely expensive to reverse.
  DESC
  default     = "10.70.0.0/16"
}

variable "az_count" {
  type        = number
  description = <<-DESC
    How many AZs to build subnets in.

    Two, even though beta runs its media path entirely in one. The subnets cost
    nothing empty, an RDS subnet group requires two AZs to exist at all, and
    the alternative — discovering that when the control plane's Postgres is
    being created — means coming back to reshape the VPC.

    Instance PLACEMENT is a separate decision and lives in sip-core: the edge
    and the FreeSWITCH farm are pinned to the same AZ so RTP never crosses one.
  DESC
  default     = 2
}

variable "nat_gateway_count" {
  type        = number
  description = <<-DESC
    NAT gateways, each in the public subnet of a distinct AZ.

    1 for beta. See nat.tf for what that does and does not put at risk — the
    short version is that it blocks deploys and recovery during an AZ failure,
    not calls in progress.
  DESC
  default     = 1

  validation {
    condition     = var.nat_gateway_count >= 1 && var.nat_gateway_count <= var.az_count
    error_message = "nat_gateway_count must be between 1 and az_count — each gateway needs its own AZ's public subnet."
  }
}

variable "enable_ssm_interface_endpoints" {
  type        = bool
  description = <<-DESC
    Create interface endpoints for ssm / ssmmessages / ec2messages.

    False while there is one NAT gateway and a NAT outage is an acceptable
    debugging outage. See the long note in endpoints.tf — flip this at the same
    time as nat_gateway_count, not before.
  DESC
  default     = false
}
