########################################################
# Repositories are NOT env-scoped — see the note on `environment` in
# ../_main/variables.tf. Applying this from a second environment's directory
# would try to create repositories that already exist.
#
# When a second environment appears, delete its ecr wrapper and have it read
# this stack's outputs through remote state instead.
########################################################

module "ecr" {
  source = "../_main"

  environment = "beta"
}
