variable "environment" {
  type        = string
  description = "Environment name. Prefixes resource names and selects the upstream stacks' state keys."
}

variable "aws_region" {
  type        = string
  description = "Region. Baked into the SSM agent download URL in user_data."
}

variable "state_bucket" {
  type        = string
  description = "S3 bucket holding every stack's state."
}

variable "state_bucket_region" {
  type        = string
  description = "Region of the state bucket."
}

########################################################
# Instances
########################################################

variable "app_count" {
  type        = number
  description = <<-DESC
    App instances.

    ONE. This is a small platform and the second instance only paid for itself
    in combination with a load balancer - it enabled a deregister-drain-restart
    deploy. Without somewhere else to send traffic, a second box is a second
    box to configure and patch for no behavioural gain.

    WHAT ONE COSTS: restarting the controller drops the ESL sockets it holds,
    so calls in progress end. New calls fail for the restart duration whether
    there are one or two instances, because FreeSWITCH's socket application
    takes a single target and does not fail over.

    Going above 1 is not just this number. DNS round-robin across two A records
    is NOT failover - a dead node still receives its share of lookups - so a
    real second instance wants an NLB back in front, and the drain machinery
    with it. Treat this as a 1-or-properly-2 decision.
  DESC
  default     = 1

  validation {
    condition     = var.app_count >= 1
    error_message = "app_count must be at least 1."
  }
}

variable "app_instance_type" {
  type        = string
  description = <<-DESC
    m6i.large — 2 vCPU, 8 GiB.

    Memory over clock speed, because the box runs two containers and the ESL
    controller holds per-call state for every socket it has open. Concurrency
    here is bounded by memory and file descriptors long before CPU.

    NOT a t3/t4g burstable. Those run on CPU credits, and a service that holds
    long-lived sockets under sustained load exhausts them and then gets
    throttled — which presents as call setup latency that degrades over hours
    and recovers overnight, an unusually unpleasant thing to diagnose.
  DESC
  default     = "m6i.large"
}

variable "app_root_volume_size" {
  type        = number
  description = "GB. Mostly the Docker image cache — immutable tags mean every deploy pulls fresh layers and old ones linger until pruned."
  default     = 40
}

########################################################
# Ports
########################################################

variable "esl_listener_port" {
  type        = number
  description = <<-DESC
    Where the ESL controller accepts outbound-ESL sockets from FreeSWITCH.

    Must match the &socket() target in the FreeSWITCH dialplan — see
    freeswitch_esl_outbound_target in the Ansible inventory. 8022 by
    convention, to sit next to FreeSWITCH's own 8021 inbound listener without
    anyone confusing the two.
  DESC
  default     = 8022
}

variable "api_listener_port" {
  type        = number
  description = "Control-plane REST API. Reached by Kamailio for routing lookups and by the ESL controller for decisions."
  default     = 8080
}

variable "app_health_port" {
  type        = number
  description = <<-DESC
    Separate HTTP port both containers expose a health endpoint on.

    Nothing external scrapes it now that the load balancer is gone - the
    Ansible role checks it over loopback after a deploy, and the security group
    leaves it closed. It stays separate from the service ports because a health
    endpoint that shares a port with the ESL listener cannot distinguish "the
    socket accepts" from "the service works", and the first is not worth
    knowing.

    Make the endpoint assert its DEPENDENCIES - that the controller can reach
    the control-plane API and FreeSWITCH's inbound ESL. A check that only
    proves the process is running tells you nothing on the day it matters.
  DESC
  default     = 8088
}

variable "health_check_path" {
  type        = string
  description = <<-DESC
    Path the Ansible role checks over loopback after a deploy.

    Make this endpoint assert the DEPENDENCIES, not just liveness — that the
    controller can reach the control-plane API and FreeSWITCH's inbound ESL. A
    health check that only proves the process is running keeps a wedged node in
    rotation, and every call routed to it is dropped.
  DESC
  default     = "/healthz"
}

########################################################
# ESL socket lifecycle — the settings deploys live or die by
########################################################

########################################################
# DNS
########################################################

variable "internal_dns_zone" {
  type        = string
  description = <<-DESC
    Private hosted zone name. Records created inside it:
      esl.<zone>   the &socket() target in the FreeSWITCH dialplan
      api.<zone>   the control-plane REST API

    `.internal` rather than a subdomain of a real domain — the zone is
    unresolvable outside the VPC by construction, so there is no public record
    to keep in step and nothing to leak.
  DESC
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

variable "dns_ttl" {
  type        = number
  description = <<-DESC
    TTL on the esl/api A records, in seconds.

    Short on purpose. These records point straight at an instance, so they are
    the lever for moving the controller - repointing should take a minute, not
    however long a resolver decided to cache it. The lookup happens per call,
    but it is a VPC resolver query on the same subnet and costs nothing worth
    optimising.
  DESC
  default     = 60
}
