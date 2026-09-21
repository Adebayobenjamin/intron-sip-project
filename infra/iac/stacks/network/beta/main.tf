########################################################
# beta — the only environment today.
#
# The diagram calls this platform Beta, and there is deliberately no dev/prod
# split yet. The cost of that is real and worth stating: there is nowhere to
# test a Kamailio config change or a FreeSWITCH rebuild except the environment
# carrying traffic. When that becomes unacceptable, copy this directory to
# stacks/network/dev, change `environment` and `vpc_cidr`, and the module
# already supports it.
########################################################

module "network" {
  source = "../_main"

  environment = "beta"
  aws_region  = var.aws_region

  # 10.70.0.0/16 — see the variable's description. This has to be confirmed
  # against wherever Intron's existing LiveKit runs BEFORE the first apply.
  vpc_cidr = var.vpc_cidr

  # Two AZs of subnets, one AZ of instances. See the variable descriptions in
  # _main — the subnets are free and an RDS subnet group needs two.
  az_count = 2

  # One NAT. Blocks deploys during an AZ failure, not calls. ~$33/month buys
  # the second one when that trade stops being acceptable.
  nat_gateway_count = 1

  # Redundant while there is a NAT gateway. Flip together with
  # nat_gateway_count, not before — see endpoints.tf.
  enable_ssm_interface_endpoints = false
}
