########################################################
# Disposable Asterisk PBX, standing in for a client's contact-centre SBC so
# the human-handoff leg can be exercised before a real one exists.
#
# DESTROY IT WHEN THE REAL SBC ARRIVES. Nothing depends on its outputs and no
# resource here carries prevent_destroy:
#
#   make destroy STACK=test-pbx ENV=beta
#
# Roughly $8/month if left running (t3.micro + 20 GB gp3 + an Elastic IP that
# is only billed while attached to a running instance).
########################################################
module "test_pbx" {
  source = "../_main"

  environment         = "beta"
  aws_region          = var.aws_region
  state_bucket        = var.state_bucket
  aws_profile         = var.aws_profile
  state_bucket_region = var.aws_region

  instance_type = "t3.micro"

  # Where Linphone registers from. Typed as a comma-separated STRING because
  # the Makefile cannot pass a JSON list through the shell intact - see the
  # variable's description. Split here so the module keeps a properly typed
  # list(string) interface.
  #
  # compact() drops the empty element that split() returns for "", which would
  # otherwise become a security group rule with an empty CIDR.
  operator_cidrs = compact(split(",", var.operator_cidr))

  ########################################################
  # MUST MATCH rtp.conf in the Ansible role. The security group opens a range
  # and Asterisk allocates within one; a mismatch is not a config error that
  # reports itself, it is calls that set up cleanly and have no audio.
  ########################################################
  rtp_port_min = 10000
  rtp_port_max = 20000
}
