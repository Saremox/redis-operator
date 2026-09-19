VERSION := 4.0.0

# Name of this service/application
SERVICE_NAME := redis-operator

# Docker image name for this project - derived from git remote
# Extracts owner/repo from git@github.com:owner/repo.git or https://github.com/owner/repo.git
IMAGE_NAME := $(shell git remote get-url origin 2>/dev/null | sed -E 's|.*github.com[:/]||; s|\.git$$||' | tr '[:upper:]' '[:lower:]')
ifeq ($(IMAGE_NAME),)
  IMAGE_NAME := local/$(SERVICE_NAME)
endif

# Repository url for this project
REPOSITORY := ghcr.io/$(IMAGE_NAME)

# Shell to use for running scripts
SHELL := $(shell which bash)

# Get docker path or an empty string
DOCKER := $(shell command -v docker)

# Get the main unix group for the user running make (to be used by docker-compose later)
GID := $(shell id -g)

# Get the unix user id for the user running make (to be used by docker-compose later)
UID := $(shell id -u)

# Commit hash from git
COMMIT=$(shell git rev-parse HEAD)
GITTAG_COMMIT := $(shell git rev-list --tags --max-count=1)
GITTAG := $(shell git describe --abbrev=0 --tags ${GITTAG_COMMIT} 2>/dev/null || true)

# Branch from git
BRANCH=$(shell git rev-parse --abbrev-ref HEAD)

TAG := $(GITTAG)
ifneq ($(COMMIT), $(GITTAG_COMMIT))
    TAG := $(COMMIT)
endif

ifneq ($(shell git status --porcelain),)
    TAG := $(TAG)-dirty
endif


PROJECT_PACKAGE := github.com/saremox/redis-operator
CODEGEN_IMAGE := ghcr.io/slok/kube-code-generator:v1.27.0
PORT := 9710

# CMDs
UNIT_TEST_CMD := go test `go list ./... | grep -v /vendor/` -v
# Packages that actually contain test files (in-package or external "_test"
# packages), respecting build tags (e.g. excludes test/integration, which is
# gated behind the "integration" build tag). Coverage is scoped to these so
# generated/no-test packages (client/k8s clientset, mocks, cmd/*, ...) aren't
# fed through the coverage instrumentation, which otherwise trips
# `go: no such tool "covdata"` on toolchains that don't ship it.
UNIT_TEST_COVERAGE_PKGS_CMD := go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v /vendor/
UNIT_TEST_COVERAGE_CMD := go test `$(UNIT_TEST_COVERAGE_PKGS_CMD)` -v -coverprofile=coverage.out -covermode=atomic
GO_GENERATE_CMD := go generate `go list ./... | grep -v /vendor/`
# -timeout raised from go test's 10m default: this package now runs two
# real-cluster tests back to back (sentinel-managed creation, and an
# operator-managed creation-plus-rollout scenario), and together they can
# comfortably exceed 10m against a minikube runner without either being slow
# on its own.
GO_INTEGRATION_TEST_CMD := go test `go list ./... | grep test/integration` -v -tags='integration' -timeout=30m
MOCKS_CMD := go generate ./mocks

# environment dirs
DEV_DIR := docker/development
APP_DIR := docker/app

# workdir
WORKDIR := /go/src/github.com/saremox/redis-operator

# The default action of this Makefile is to build the development docker image
.PHONY: default
default: build

# Run the development environment in non-daemonized mode (foreground)
.PHONY: docker-build
docker-build: deps-development
	docker build \
		--build-arg uid=$(UID) \
		-t $(REPOSITORY)-dev:latest \
		-t $(REPOSITORY)-dev:$(COMMIT) \
		-f $(DEV_DIR)/Dockerfile \
		.

# Run a shell into the development docker image
.PHONY: shell
shell: docker-build
	docker run -ti --rm -v ~/.kube:/.kube:ro -v $(PWD):$(WORKDIR) -u $(UID):$(UID) --name $(SERVICE_NAME) -p $(PORT):$(PORT) $(REPOSITORY)-dev /bin/bash

# Build redis-failover executable file
.PHONY: build
build: docker-build
	docker run -ti --rm -v $(PWD):$(WORKDIR) -u $(UID):$(UID) --name $(SERVICE_NAME) $(REPOSITORY)-dev ./scripts/build.sh

# Run the development environment in the background
.PHONY: run
run: docker-build
	docker run -ti --rm -v ~/.kube:/.kube:ro -v $(PWD):$(WORKDIR) -u $(UID):$(UID) --name $(SERVICE_NAME) -p $(PORT):$(PORT) $(REPOSITORY)-dev ./scripts/run.sh

# Build the production image based on the public one
.PHONY: image
image: deps-development
	docker build \
	-t $(SERVICE_NAME) \
	-t $(REPOSITORY):latest \
	-t $(REPOSITORY):$(COMMIT) \
	-t $(REPOSITORY):$(BRANCH) \
	-f $(APP_DIR)/Dockerfile \
	.

.PHONY: image-release
image-release:
	docker buildx build \
	--platform linux/amd64,linux/arm64,linux/arm/v7 \
	--label "org.opencontainers.image.source=https://github.com/saremox/redis-operator" \
 	--label "org.opencontainers.image.description=Redis Failover Operator" \
 	--label "org.opencontainers.image.licenses=Apache-2.0" \
	--push \
	--build-arg VERSION=$(TAG) \
	-t $(REPOSITORY):latest \
	-t $(REPOSITORY):$(COMMIT) \
	-t $(REPOSITORY):$(TAG) \
	-f $(APP_DIR)/Dockerfile \
	.

.PHONY: testing
testing: image
	docker push $(REPOSITORY):$(BRANCH)

.PHONY: tag
tag:
	git tag $(VERSION)

.PHONY: publish
publish:
	@COMMIT_VERSION="$$(git rev-list -n 1 $(VERSION))"; \
	docker tag $(REPOSITORY):"$$COMMIT_VERSION" $(REPOSITORY):$(VERSION)
	docker push $(REPOSITORY):$(VERSION)
	docker push $(REPOSITORY):latest

.PHONY: release
release: tag image-release

# Test stuff in dev
.PHONY: unit-test
unit-test: docker-build
	docker run -ti --rm -v $(PWD):$(WORKDIR) -u $(UID):$(UID) --name $(SERVICE_NAME) $(REPOSITORY)-dev /bin/sh -c '$(UNIT_TEST_CMD)'

.PHONY: ci-unit-test
ci-unit-test:
	$(UNIT_TEST_CMD)

# Same unit tests as ci-unit-test, but also produces coverage.out for
# uploading to Codecov. Used by the CI unit-test job.
.PHONY: ci-unit-test-coverage
ci-unit-test-coverage:
	$(UNIT_TEST_COVERAGE_CMD)

.PHONY: ci-integration-test
ci-integration-test:
	$(GO_INTEGRATION_TEST_CMD)

.PHONY: integration-test
integration-test:
	./scripts/integration-tests.sh

.PHONY: helm-test
helm-test:
	./scripts/helm-tests.sh

# Run all tests
.PHONY: test
test: ci-unit-test ci-integration-test helm-test

.PHONY: go-generate
go-generate: docker-build
	docker run -ti --rm -v $(PWD):$(WORKDIR) -u $(UID):$(UID) --name $(SERVICE_NAME) $(REPOSITORY)-dev /bin/sh -c '$(GO_GENERATE_CMD)'

.PHONY: generate
generate: go-generate

.PHONY: mocks
mocks: docker-build
	docker run -ti --rm -v $(PWD):$(WORKDIR) -u $(UID):$(UID) --name $(SERVICE_NAME) $(REPOSITORY)-dev /bin/sh -c '$(MOCKS_CMD)'

.PHONY: deps-development
# Test if the dependencies we need to run this Makefile are installed
deps-development:
ifndef DOCKER
	@echo "Docker is not available. Please install docker"
	@exit 1
endif

# Generate the typed clientset (client/k8s/clientset). DeepCopy used to come
# out of this same Docker-based generator too, but that moved to
# generate-deepcopy (controller-gen, no Docker needed) since it's the part
# that actually goes stale in practice - client-gen output only changes when
# the RedisFailover API's shape itself changes, which is rare.
.PHONY: update-codegen
update-codegen:
	@echo ">> Generating client code for Kubernetes CRD types..."
	docker run --rm -it \
	-v $(PWD):/go/src/$(PROJECT_PACKAGE) \
	-e PROJECT_PACKAGE=$(PROJECT_PACKAGE) \
	-e CLIENT_GENERATOR_OUT=$(PROJECT_PACKAGE)/client/k8s \
	-e APIS_ROOT=$(PROJECT_PACKAGE)/api \
	-e GROUPS_VERSION="redisfailover:v1" \
	-e GENERATION_TARGETS="client" \
	$(CODEGEN_IMAGE)

# Generate CRD using controller-gen (requires controller-gen v0.20.0+ for Go 1.25+)
# Install: go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
.PHONY: generate-crd
generate-crd:
	controller-gen crd paths=./api/... output:crd:dir=./manifests
	cp -f manifests/databases.spotahome.com_redisfailovers.yaml manifests/kustomize/base/
	cp -f manifests/databases.spotahome.com_redisfailovers.yaml charts/redisoperator/crds/

# Generate DeepCopy methods for the API types (zz_generated.deepcopy.go).
# Same controller-gen binary as generate-crd - no Docker required, which is
# what makes this (unlike update-codegen) safe to run from verify-codegen and
# the pre-commit hook in every contributor's environment.
.PHONY: generate-deepcopy
generate-deepcopy:
	controller-gen object paths=./api/...

# Everything controller-gen can produce without Docker. update-codegen
# (client-gen) and mocks are separate: still Docker-based, change far less
# often, and aren't covered by verify-codegen or the pre-commit hook.
.PHONY: generate-api
generate-api: generate-deepcopy generate-crd

# Fails if the API types changed without regenerating DeepCopy - i.e.
# `make generate-deepcopy`'s output doesn't match what's committed. Run by
# CI (see .github/workflows/ci.yaml) and the pre-commit hook (see
# .githooks/pre-commit); both call this instead of duplicating the check.
#
# Deliberately does NOT also verify the CRD manifest generate-crd produces:
# that embeds the full schema of every corev1 type RedisFailover references
# (PodSpec, Volume, ...), which shifts on any controller-gen or k8s.io/api
# version bump regardless of whether RedisFailover's own fields changed -
# gating commits on that would fail for reasons unrelated to the change
# being made. generate-crd stays a manually-run step.
.PHONY: verify-codegen
verify-codegen: generate-deepcopy
	@git diff --exit-code -- api/ || \
		(echo ""; \
		echo "Generated DeepCopy code is out of date."; \
		echo "Run 'make generate-deepcopy' and commit the result."; \
		exit 1)

# One-time setup per clone: git hooks under .git/hooks aren't version
# controlled, so this points git at the versioned ones in .githooks instead.
.PHONY: install-hooks
install-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks installed from .githooks/. verify-codegen now runs before each commit that touches api/."
