########################################################
# Apply LAST. It reads network, secrets-manager, sip-core and ecr through
# remote state, and a plan against a missing upstream fails with a message
# about an output key rather than about the missing stack.
########################################################

module "control_plane" {
  source = "../_main"

  environment         = "beta"
  aws_region          = var.aws_region
  state_bucket        = var.state_bucket
  aws_profile         = var.aws_profile
  state_bucket_region = var.aws_region

  ########################################################
  # ONE instance, no load balancer. See ../_main/main.tf for what that costs
  # (a restart drops in-flight ESL sockets) and what it removes (the whole NLB
  # surface: idle reap, client-IP inversion, drain script, self-deregister IAM).
  ########################################################
  app_count         = 1
  app_instance_type = "m6i.large"

  # 8022 must match the &socket() target the FreeSWITCH dialplan is rendered
  # with; 8080 must match routing_service_url in the Ansible inventory. Both
  # flow from this stack's outputs into that inventory rather than being typed
  # twice.
  esl_listener_port = 8022
  api_listener_port = 8080
  app_health_port   = 8088

  # Short TTL: these A records point straight at the instance, so they are the
  # lever for moving the controller.
  dns_ttl = 60

  internal_dns_zone = "beta.intron.internal"
}
