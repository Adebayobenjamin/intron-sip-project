########################################################
# Apply this SECOND, right after tfstate.
#
# It is on the Makefile's NO_IAC_SECRET_STACKS list because it creates the
# container that list exists to work around: every other stack is wrapped in
# `aws-env-run intron-sip/beta/iac`, and that fails if the container is not
# there yet.
#
# After the first apply, populate it:
#   cd infra/iac && cp .env.example .env && $EDITOR .env
#   make populate-iac ENV=beta TOKEN=<6-digit>
########################################################

module "secrets" {
  source = "../_main"

  environment = "beta"
}
