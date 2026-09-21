# Backend services — build, test, and push to ECR.
#
# Deploys are NOT driven from here. Pushing a tag is half of it; the other half
# is bumping image_tag in infra/ansible/inventories/<env>/group_vars/control_plane.yml
# and running the control-plane play. `make deploy-hint` prints the second half.

SHELL := /bin/bash
.DEFAULT_GOAL := help

SERVICES    := control-plane esl-controller

AWS_PROFILE ?= klarity_aws
AWS_REGION  ?= eu-central-1
AWS_ACCOUNT ?= 023585529698
ECR_REGISTRY := $(AWS_ACCOUNT).dkr.ecr.$(AWS_REGION).amazonaws.com

# TAG names exactly one set of bytes, forever: the ECR repositories are created
# with IMMUTABLE tags, so a tag cannot be moved or re-pushed. That is what
# makes a rollback meaningful and what stops two nodes running different code
# under one name. The cost is that a failed deploy needs a NEW tag, not a
# re-push of the old one.
TAG ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')

## help: list targets
.PHONY: help
help:
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
	@echo ""
	@echo "Current TAG: $(TAG)"

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

## build: compile both binaries for the app tier (linux/amd64)
.PHONY: build
build: $(addprefix build-,$(SERVICES))

# GOARCH is pinned rather than inherited. The app tier is x86_64; a binary
# built on Apple silicon runs perfectly on the laptop and not at all on the
# instance.
.PHONY: build-%
build-%:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
	  -trimpath \
	  -ldflags="-s -w -X main.version=$(TAG)" \
	  -o bin/$* ./cmd/$*
	@echo "built bin/$* ($(TAG), linux/amd64)"

## run-control-plane: run locally against a .env file
.PHONY: run-control-plane
run-control-plane:
	@set -a && [ -f .env ] && source .env; set +a; go run ./cmd/control-plane

## run-esl-controller: run locally against a .env file
.PHONY: run-esl-controller
run-esl-controller:
	@set -a && [ -f .env ] && source .env; set +a; go run ./cmd/esl-controller

# ---------------------------------------------------------------------------
# Quality
# ---------------------------------------------------------------------------

## test: unit tests
.PHONY: test
test:
	go test ./...

## test-race: unit tests with the race detector
.PHONY: test-race
test-race:
	go test -race ./...

## test-integration: tests needing a real Postgres. Each creates its own database
.PHONY: test-integration
test-integration:
	go test -tags integration ./...

## cover: test coverage summary
.PHONY: cover
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

## vet: go vet
.PHONY: vet
vet:
	go vet ./...

## fmt: format and tidy
.PHONY: fmt
fmt:
	gofmt -w $(GO_FILES)
	go mod tidy

## check: what CI runs
.PHONY: check
check: vet test

# ---------------------------------------------------------------------------
# Images
# ---------------------------------------------------------------------------

## docker: build both images
.PHONY: docker
docker: $(addprefix docker-,$(SERVICES))

.PHONY: docker-%
docker-%:
	docker build \
	  --platform linux/amd64 \
	  --build-arg VERSION=$(TAG) \
	  -f Dockerfile.$* \
	  -t $(ECR_REGISTRY)/intron-sip/$*:$(TAG) \
	  .

## ecr-login: authenticate docker against ECR
.PHONY: ecr-login
ecr-login:
	aws ecr get-login-password --profile $(AWS_PROFILE) --region $(AWS_REGION) \
	  | docker login --username AWS --password-stdin $(ECR_REGISTRY)

## push: build and push both images. Requires TAG to be a version, not 'dev'
.PHONY: push
push: guard-tag ecr-login docker
	@for s in $(SERVICES); do \
	  docker push $(ECR_REGISTRY)/intron-sip/$$s:$(TAG); \
	done
	@$(MAKE) --no-print-directory deploy-hint

# Refuse to push a tag that says nothing about what is in it. An immutable
# registry makes `dev` a permanent squatter on a name that can never be reused.
.PHONY: guard-tag
guard-tag:
	@if [ "$(TAG)" = "dev" ] || [[ "$(TAG)" == *-dirty ]]; then \
	  echo "refusing to push TAG=$(TAG)"; \
	  echo "tags are immutable — commit, then: make push TAG=v0.1.0"; \
	  exit 1; \
	fi

## deploy-hint: print the Ansible half of a deploy
.PHONY: deploy-hint
deploy-hint:
	@echo ""
	@echo "Pushed $(TAG). To deploy:"
	@echo "  1. set image_tag: $(TAG) for both services in"
	@echo "     infra/ansible/inventories/beta/group_vars/control_plane.yml"
	@echo "  2. cd infra/ansible && make play PLAYBOOK=control-plane ENV=beta"

## clean: remove build output
.PHONY: clean
clean:
	rm -rf bin coverage.out
