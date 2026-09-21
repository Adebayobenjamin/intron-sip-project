variable "environment" {
  type        = string
  description = "Environment name. Prefixes every resource and selects the remote state keys."
}

variable "aws_region" {
  type        = string
  description = "Region. Must match the rest of the platform - this box talks SIP to the edge, and cross-region would add latency to the one path being measured."
}

variable "aws_profile" {
  type        = string
  description = "Shared-config profile. Passed through to terraform_remote_state, which configures its own backend and inherits nothing from the provider block."
}

variable "state_bucket" {
  type        = string
  description = "Bucket holding the other stacks' state, read via terraform_remote_state."
}

variable "state_bucket_region" {
  type        = string
  description = "Region of the state bucket."
}

variable "instance_type" {
  type        = string
  description = <<-DESC
    Asterisk answering one call at a time for a human to pick up. t3.micro is
    ample and free-tier eligible; this is a test rig, not a platform tier.
  DESC
  default     = "t3.micro"
}

variable "root_volume_size" {
  type        = number
  description = "GB. Asterisk from a package holds nothing durable."
  default     = 20
}

variable "operator_cidrs" {
  type        = list(string)
  description = <<-DESC
    Where the Linphone softphone registers FROM. Your laptop's public address,
    as a /32.

    EMPTY IS THE DEFAULT AND MEANS LINPHONE CANNOT REGISTER, which is the safe
    failure: an extension reachable from the whole internet is a toll-fraud
    target, and an exposed 5060 is found by scanners within hours. That is true
    even on a throwaway box - the fraud is charged to whoever owns the trunk it
    can reach.

    Find yours with `curl -s ifconfig.me`. If it changes often, prefer
    re-running apply over widening this to 0.0.0.0/0.
  DESC
  default     = []
}

variable "rtp_port_min" {
  type        = number
  description = "Bottom of Asterisk's RTP range. Must match rtp.conf in the Ansible role - a mismatch means calls connect with no audio."
  default     = 10000
}

variable "rtp_port_max" {
  type        = number
  description = "Top of Asterisk's RTP range. See rtp_port_min."
  default     = 20000
}

variable "sip_port" {
  type        = number
  description = "SIP signalling port this PBX listens on."
  default     = 5060
}
