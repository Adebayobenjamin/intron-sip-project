# ansible/Makefile — drive playbooks against SSM-only EC2 hosts.
#
# Parallels iac/Makefile, including how it authenticates: the klarity_aws
# shared-config profile, named explicitly rather than inherited from whatever
# AWS_PROFILE happens to be set.
#
# Usage (from this directory):
#   make play   PLAYBOOK=freeswitch ENV=beta
#   make ping   TARGET=freeswitch   ENV=beta
#   make list-instances             ENV=beta
#   make galaxy                                        (collections, no AWS)
#
# Recipes are single-line (no .ONESHELL) so this works on macOS's stock GNU
# make 3.81. ansible-playbook runs from THIS directory so ansible.cfg is
# auto-discovered — no `cd` needed.

PLAYBOOK     ?=
TARGET       ?= all
ENV          ?= beta
TAGS         ?=
LIMIT        ?=
EXTRA_VARS   ?=
AWS_REGION   ?= eu-central-1

########################################################
# CREDENTIALS - the klarity_aws profile, same as Terraform.
#
# Ansible does not depend on this being exported: the inventory sets
# ansible_aws_ssm_profile, so a bare `ansible-playbook` authenticates the same
# way a `make play` does. It is exported anyway for the plain `aws` calls in
# the recipes below, which have no inventory to read.
#
# AWS_REGION is NOT optional: the klarity_aws profile carries no region in
# ~/.aws/config, so without it every CLI call fails with NoRegion.
#
# NO MFA WRAPPER. This used to route every target through bin/with-mfa, now
# deleted. Two reasons it is gone. It is incompatible with naming a profile - an explicit
# profile beats environment credentials, so the MFA session would be silently
# ignored while appearing to be in use. And the identity behind klarity_aws has
# NO MFA DEVICE (checked with `aws iam list-mfa-devices`), so GetSessionToken
# could never have succeeded; the placeholder MFA_ARN meant every target here
# failed before Ansible started.
#
# If MFA is ever enforced, fix it in ~/.aws/config rather than here: an
# assume-role profile with `mfa_serial` makes the SDK prompt and cache the
# session itself.
########################################################
AWS_PROFILE  ?= klarity_aws

BIN_DIR  := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../bin)

INV_DIR := inventories/$(ENV)

# macOS Python aborts forked subprocesses by default — Ansible's worker pool
# then comes up "dead" before sending any message back, and all you see is
# `A worker was found in a dead state` with no further detail. Opting out of
# fork safety lets the workers actually run. No-op on Linux.
export OBJC_DISABLE_INITIALIZE_FORK_SAFETY := YES

# Propagated to ansible and to the aws CLI calls in the recipes below.
export AWS_PROFILE AWS_REGION

INV_NEEDED := play ping
ifneq ($(filter $(INV_NEEDED),$(MAKECMDGOALS)),)
  ifeq ($(wildcard $(INV_DIR)/),)
    $(error inventory directory does not exist: $(INV_DIR))
  endif
endif

ifeq ($(MAKECMDGOALS),play)
  ifeq ($(PLAYBOOK),)
    $(error PLAYBOOK is required. Example: make play PLAYBOOK=freeswitch ENV=beta)
  endif
  ifeq ($(wildcard playbooks/$(PLAYBOOK).yml),)
    $(error playbook does not exist: playbooks/$(PLAYBOOK).yml)
  endif
endif

# Built so they are empty when unset — no stray --tags='' reaching ansible.
ANSIBLE_OPTS := $(if $(TAGS),--tags $(TAGS),) $(if $(LIMIT),--limit $(LIMIT),) $(if $(EXTRA_VARS),--extra-vars $(EXTRA_VARS),)

.PHONY: play check ping list-instances shell galaxy boto-deps lint syntax-check list help

# `-b` (become) elevates to root via sudo on the host. Every role expects it.
play:
	ansible-playbook -i $(INV_DIR) playbooks/$(PLAYBOOK).yml -b $(ANSIBLE_OPTS)

# Dry run. Worth knowing the limit: --check cannot predict tasks whose
# behaviour depends on an earlier task's result, and this repo has several
# (the FreeSWITCH module detection, the recordings volume discovery). Treat a
# clean check as "nothing obviously wrong", not as a plan.
check:
	ansible-playbook -i $(INV_DIR) playbooks/$(PLAYBOOK).yml -b --check --diff $(ANSIBLE_OPTS)

# Smoke test. `ping` is the Ansible module, not ICMP — it proves SendCommand
# plus the S3 transport bucket work end to end, which is the thing that
# actually breaks.
ping:
	ansible -i $(INV_DIR) $(TARGET) -m ping

# Interactive shell on a host. TARGET must name ONE host.
#   make shell TARGET=beta-freeswitch-1 ENV=beta
shell:
	aws ssm start-session --region $(AWS_REGION) \
	  --target $$(ansible-inventory -i $(INV_DIR) --host $(TARGET) | python3 -c 'import json,sys; print(json.load(sys.stdin)["ansible_aws_ssm_instance_id"])')

# Discover the instance IDs that go into inventories/$(ENV)/hosts.yml.
#
# Prefer `make output STACK=<stack> ENV=$(ENV) EXTRA="-raw ansible_inventory_hint"`
# from iac/ — it emits paste-ready YAML. This is the fallback for when you need
# to know what SSM can actually SEE, which is a different question: an instance
# missing here has an agent problem, not an inventory problem.
list-instances:
	@aws ssm describe-instance-information \
	  --region $(AWS_REGION) \
	  --filters "Key=PingStatus,Values=Online" \
	  --query "InstanceInformationList[].[InstanceId, ComputerName, IPAddress, PingStatus]" \
	  --output table

# One-time setup. No AWS credentials needed — galaxy hits public artifact stores.
galaxy:
	ansible-galaxy collection install -r requirements.yml

# Install boto3/botocore into the ANSIBLE-BUNDLED Python. Homebrew's ansible
# ships a venv at $(brew --prefix ansible)/libexec/bin/python, and a plain
# `pip install` from your shell PATH lands in a different interpreter that the
# connection plugin cannot see — which fails as "boto3 is not installed" while
# `python3 -c 'import boto3'` works fine.
#
# Re-run after `brew upgrade ansible`: the upgrade wipes the bundled venv.
boto-deps:
	"$$(brew --prefix ansible)/libexec/bin/python" -m pip install --upgrade boto3 botocore

syntax-check:
	@for f in playbooks/*.yml; do \
	  echo "==> $$f"; \
	  ansible-playbook -i $(INV_DIR) "$$f" --syntax-check; \
	done

lint:
	ansible-lint playbooks/ roles/

list:
	@echo "Playbooks:"
	@ls playbooks/*.yml | sed 's|playbooks/||; s|\.yml||; s|^|  PLAYBOOK=|'
	@echo
	@echo "Envs:"
	@find inventories -maxdepth 1 -mindepth 1 -type d | sed 's|inventories/||; s|^|  ENV=|'

help:
	@echo "First run, in order:"
	@echo "  make galaxy                                        # collections"
	@echo "  make boto-deps                                     # boto3 into ansible's python"
	@echo "  brew install --cask session-manager-plugin         # required by the SSM plugin"
	@echo "  make ping TARGET=all ENV=beta      # prove the path works"
	@echo
	@echo "Then:"
	@echo "  make play PLAYBOOK=site ENV=beta"
	@echo
	@echo "Optional flags on play/check:"
	@echo "  TAGS=install,config   LIMIT=beta-freeswitch-1   EXTRA_VARS='key=value'"
