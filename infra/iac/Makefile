# iac/Makefile — drive any stack from this directory.
#
# Recipes are single-line (no .ONESHELL) so this works on macOS's stock GNU
# make 3.81. The `cd` happens inside the recipe's subshell — your interactive
# pwd stays at iac/ before and after the run.
#
# Usage (from this directory):
#   make plan    STACK=network ENV=beta
#   make apply   STACK=network ENV=beta
#   make init    STACK=network ENV=beta
#   make destroy STACK=network ENV=beta
#
# Targets that don't touch AWS:
#   make fmt
#   make validate STACK=network ENV=beta
#   make list
#
# Override via env or `make VAR=...`:
#   AWS_PROFILE  Shared-config profile Terraform authenticates with
#   AWS_REGION   Region for the aws CLI calls this Makefile makes

STACK      ?=
ENV        ?= beta
AWS_REGION ?= eu-central-1

########################################################
# CREDENTIALS — the klarity_aws shared-config profile.
#
# Terraform does not depend on this being exported: every provider block, every
# backend block and every terraform_remote_state lookup names the profile
# explicitly, so a bare `terraform plan` inside a stack directory authenticates
# the same way a `make plan` does. That is deliberate — an auth model that only
# works through the Makefile is one that breaks the first time someone debugs a
# stack directly.
#
# It is exported anyway, for the `aws` CLI calls in the recipes below
# (aws-env-run, env-file-to-sm), which have no provider block to read.
#
# AWS_REGION is exported for the same reason and is NOT optional: the
# klarity_aws profile carries no region in ~/.aws/config, so without this every
# CLI call fails with NoRegion. Terraform itself is unaffected — its region
# comes from var.aws_region and from the literal in each backend block.
#
# NO MFA WRAPPER. This used to route every target through bin/with-mfa, which
# fetched STS session credentials and unset AWS_PROFILE. That is INCOMPATIBLE
# with naming a profile in the provider block: an explicit `profile` overrides
# environment credentials, so the MFA session would be silently ignored while
# appearing to be in use.
#
# If the klarity_aws identity ever gains a RequireMFA policy, the fix is in
# ~/.aws/config, not here — add an assume-role profile with `mfa_serial` and
# point AWS_PROFILE at it. The AWS SDK prompts for the token and caches the
# session itself. bin/with-mfa hand-rolled that and has been deleted: nothing
# called it once Ansible moved to the profile too, and the SDK does it better.
########################################################
AWS_PROFILE ?= klarity_aws

BIN_DIR     := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../bin)
AWS_ENV_RUN := $(BIN_DIR)/aws-env-run
STACK_DIR   := stacks/$(STACK)/$(ENV)

export AWS_PROFILE AWS_REGION

# Stacks that do NOT pull from the iac secret. Two reasons to be on this list:
#   - bootstrapping the secret store or the state bucket itself
#     (chicken-and-egg: tfstate, secrets-manager)
#   - having no per-env TF_VAR_* values to inject
#
# Every other stack gets intron-sip/<env>/iac loaded into its environment via
# aws-env-run, so TF_VAR_* values land in the process and not on disk.
NO_IAC_SECRET_STACKS := tfstate secrets-manager
ifeq ($(filter $(STACK),$(NO_IAC_SECRET_STACKS)),)
  WRAP_AWS_ENV := $(AWS_ENV_RUN) intron-sip/$(ENV)/iac --
else
  WRAP_AWS_ENV :=
endif

# Targets that need STACK pointed at a real directory.
STACK_TARGETS := init plan apply destroy validate import state output

# Only validate STACK + path for targets that need them — fmt and list don't.
ifneq ($(filter $(STACK_TARGETS),$(MAKECMDGOALS)),)
  ifeq ($(STACK),)
    $(error STACK is required. Example: make apply STACK=network ENV=beta)
  endif
  ifeq ($(wildcard $(STACK_DIR)/),)
    $(error stack directory does not exist: $(STACK_DIR))
  endif
endif

.PHONY: init plan apply destroy import state output fmt fmt-check lint validate list populate-iac populate-ansible populate preflight help

########################################################
# preflight — run before Stage 0 of docs/ROLLOUT.md.
#
# Checks the things that produce confusing failures LATER rather than here:
# a Terraform older than 1.10 parses `use_lockfile` and silently does not lock;
# a profile that does not resolve fails inside a provider with an error about
# the resource it was creating; a region mismatch between the Makefile and a
# backend block splits state across two regions without complaint.
#
# Deliberately read-only. It creates nothing and costs nothing.
########################################################
preflight:
	@echo "== tooling =="
	@terraform version | head -1
	@printf "terraform >= 1.10 (use_lockfile): "; \
	  tfv=$$(terraform version -json | python3 -c 'import json,sys;print(json.load(sys.stdin)["terraform_version"])'); \
	  python3 -c "import sys;from itertools import zip_longest;v=[int(x) for x in '$$tfv'.split('.')[:2]];print('OK ($$tfv)' if (v[0],v[1])>=(1,10) else 'TOO OLD ($$tfv) — locking will silently no-op')"
	@printf "aws cli: "; aws --version 2>&1 | head -1
	@echo
	@echo "== identity =="
	@printf "AWS_PROFILE=$(AWS_PROFILE)  AWS_REGION=$(AWS_REGION)\n"
	@aws sts get-caller-identity --output table
	@echo
	@echo "== region reachable =="
	@aws ec2 describe-availability-zones --query 'AvailabilityZones[].ZoneName' --output text
	@echo
	@echo "== region agrees with the backend blocks =="
	@backend_regions=$$(grep -rh 'region  *= *"' stacks/*/beta/backend.tf | tr -d ' "' | cut -d= -f2 | sort -u); \
	  echo "backend blocks use: $$backend_regions"; \
	  if [ "$$backend_regions" != "$(AWS_REGION)" ]; then \
	    echo "MISMATCH — Makefile says $(AWS_REGION). State would land in a different region than the resources."; \
	  else echo "OK"; fi
	@echo
	@echo "== state bucket =="
	@bucket=$$(grep -h 'bucket *= *"intron-sip-tfstate' stacks/network/beta/backend.tf | tr -d ' "' | cut -d= -f2); \
	  if aws s3api head-bucket --bucket "$$bucket" 2>/dev/null; then \
	    echo "$$bucket exists — Stage 0 already applied"; \
	  else \
	    echo "$$bucket does not exist yet — start at Stage 0"; fi
	@echo
	@echo "== decisions preflight CANNOT make for you =="
	@echo "  [ ] vpc_cidr does not overlap the network LiveKit runs in  (not blocking -"
	@echo "      the AI leg runs over the internet for now - but it is what makes the"
	@echo "      later move to VPC peering possible. See docs/LIVEKIT.md)"
	@echo "  [ ] carrier_sip_cidrs populated in .env                    (no calls until then)"
	@echo "  [ ] livekit_cidrs + livekit_sip_host set                   (no AI leg until then)"

# EXTRA passes args AFTER the terraform subcommand, e.g.
#   make apply STACK=sip-core ENV=beta EXTRA="-replace='...' -auto-approve"
# TF_ARGS still passes args BEFORE the subcommand (rare — `terraform -chdir`).
init:
	cd $(STACK_DIR) && $(WRAP_AWS_ENV) terraform $(if $(TF_ARGS),$(TF_ARGS) ,) init $(EXTRA)

plan:
	cd $(STACK_DIR) && $(WRAP_AWS_ENV) terraform $(if $(TF_ARGS),$(TF_ARGS) ,) plan $(EXTRA)

apply:
	cd $(STACK_DIR) && $(WRAP_AWS_ENV) terraform $(if $(TF_ARGS),$(TF_ARGS) ,) apply $(EXTRA)

destroy:
	cd $(STACK_DIR) && $(WRAP_AWS_ENV) terraform $(if $(TF_ARGS),$(TF_ARGS) ,) destroy $(EXTRA)

output:
	cd $(STACK_DIR) && $(WRAP_AWS_ENV) terraform output $(EXTRA)

# Adopt a resource that exists in the cloud but not in state. Pass the address
# and id via EXTRA (quote the address — indexes contain brackets):
#   make import STACK=network ENV=beta \
#     EXTRA="'aws_route_table_association.private[0]' subnet-abc/rtb-xyz"
import:
	cd $(STACK_DIR) && $(WRAP_AWS_ENV) terraform $(if $(TF_ARGS),$(TF_ARGS) ,) import $(EXTRA)

# Raw `terraform state` access. The subcommand and its args go in EXTRA:
#   make state STACK=network ENV=beta EXTRA="list"
#
# Mainly for entries whose real-world object is already gone, where a destroy
# cannot work because the provider can no longer reach it. `state rm` makes
# Terraform forget the entry; it does NOT delete anything.
state:
	cd $(STACK_DIR) && $(WRAP_AWS_ENV) terraform $(if $(TF_ARGS),$(TF_ARGS) ,) state $(EXTRA)

# Lint helpers - no AWS calls, so no credentials needed at all.
fmt:
	terraform fmt -recursive stacks/

########################################################
# lint - catches what `validate` and `plan` cannot.
#
# Security group descriptions accept a restricted character set, and it is the
# EC2 API that enforces it, not the Terraform schema. So an em dash or an
# apostrophe passes validate, passes plan, and fails MID-APPLY with a
# half-built stack. That is exactly how it went the first time.
########################################################
lint: fmt-check
	@python3 scripts/lint-sg-descriptions.py stacks

fmt-check:
	@terraform fmt -recursive -check stacks/ && echo "terraform fmt: OK"

validate:
	cd $(STACK_DIR) && terraform init -backend=false -input=false >/dev/null && terraform validate

# Populate the iac SM secret from the local iac/.env file. Run from iac/.
# The container must already exist (apply the secrets-manager stack first).
populate-iac:
	@test -f .env || { echo "iac/.env not found in $(CURDIR) — nothing to populate" >&2; exit 1; }
	$(BIN_DIR)/env-file-to-sm intron-sip/$(ENV)/iac .env

populate-ansible:
	@test -f ../ansible/.env || { echo "ansible/.env not found at ../ansible/.env" >&2; exit 1; }
	$(BIN_DIR)/env-file-to-sm intron-sip/$(ENV)/ansible ../ansible/.env

populate: populate-iac populate-ansible

# Show available stacks + their per-env subdirs.
list:
	@find stacks -maxdepth 2 -mindepth 2 -type d ! -name '_main' \
	  | sed 's|stacks/||' \
	  | awk -F/ '{printf "  STACK=%-20s ENV=%s\n", $$1, $$2}'

help:
	@echo "Apply order from an empty account:"
	@echo "  1. STACK=tfstate         (local state; creates the S3 state bucket)"
	@echo "  2. STACK=secrets-manager (creates the SM containers, incl. .../iac)"
	@echo "  3. STACK=network         (VPC, subnets, NAT, endpoints)"
	@echo "  4. STACK=ecr             (image repos for the backend services)"
	@echo "  5. STACK=sip-core        (Kamailio+RTPEngine edge, FreeSWITCH farm)"
	@echo "  6. STACK=database        (RDS Postgres for the control plane)"
	@echo "  7. STACK=control-plane   (ESL controller + REST API, internal NLB)"
	@echo "  8. STACK=livekit         (LiveKit server for the AI leg; needs 5 for the SSM bucket)"
	@echo
	@echo "After 6:  ../bin/db-dsn-to-sm beta   (writes DATABASE_URL into the secret)"
	@echo "After 8:  make output STACK=livekit ENV=beta EXTRA=\"livekit_zone_nameservers\""
	@echo "          then have the parent domain delegate to them. Nothing in that"
	@echo "          stack works until the delegation exists - no cert, no agent."
	@echo
	@echo "Each:  make init STACK=<s> ENV=beta"
	@echo "       make plan STACK=<s> ENV=beta"
	@echo "       make apply STACK=<s> ENV=beta"
