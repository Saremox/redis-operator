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

# The development container runs as this user, so the files that it writes belong to you.
UID := $(shell id -u)

# Commit hash from git
COMMIT=$(shell git rev-parse HEAD)

# Branch from git
BRANCH=$(shell git rev-parse --abbrev-ref HEAD)

PROJECT_PACKAGE := github.com/saremox/redis-operator
CODEGEN_IMAGE := ghcr.io/slok/kube-code-generator:v1.27.0
CONTROLLER_GEN_VERSION := v0.20.1
CRD_MANIFESTS := manifests/databases.spotahome.com_redisfailovers.yaml \
	manifests/kustomize/base/databases.spotahome.com_redisfailovers.yaml \
	charts/redisoperator/crds/databases.spotahome.com_redisfailovers.yaml
PORT := 9710

# CMDs
UNIT_TEST_CMD := go test `go list ./... | grep -v /vendor/` -v
# Coverage uses only the packages with test files. A package without tests,
# for example the generated clientset or the mocks, makes `go test -cover`
# fail with `go: no such tool "covdata"` on toolchains without that tool.
# The list obeys build tags, so it does not include test/integration.
UNIT_TEST_COVERAGE_PKGS_CMD := go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v /vendor/
UNIT_TEST_COVERAGE_CMD := go test `$(UNIT_TEST_COVERAGE_PKGS_CMD)` -v -coverprofile=coverage.out -covermode=atomic
GO_GENERATE_CMD := go generate `go list ./... | grep -v /vendor/`
# The waits in the rollout test add up to more than 10 minutes, the default
# timeout of go test. With a higher timeout, a slow wait fails with its own
# error, not with a panic of go test.
GO_INTEGRATION_TEST_CMD := go test `go list ./... | grep test/integration` -v -tags='integration' -timeout=30m
MOCKS_CMD := go generate ./mocks

# environment dirs
DEV_DIR := docker/development
APP_DIR := docker/app

# workdir
WORKDIR := /go/src/github.com/saremox/redis-operator

# The default target builds the binaries in the development container.
.PHONY: default
default: build

# Build the development container image.
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

# Build ./bin/redis-operator and ./bin/redis-instance in the development container.
.PHONY: build
build: docker-build
	docker run -ti --rm -v $(PWD):$(WORKDIR) -u $(UID):$(UID) --name $(SERVICE_NAME) $(REPOSITORY)-dev ./scripts/build.sh

# Build the operator and run it in the foreground against ~/.kube/config.
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

.PHONY: testing
testing: image
	docker push $(REPOSITORY):$(BRANCH)

# A release is not a make target. Use scripts/release.sh and push a tag. The
# workflow .github/workflows/release.yml then builds and publishes the release.

# Test stuff in dev
# The development image has no redis-server, so the service/redis tests skip.
# The warning comes after the test output, where the reader sees it.
REDIS_SERVER_WARNING := command -v redis-server >/dev/null || echo "WARNING: redis-server is not on the PATH, so the service/redis tests were skipped. Run make ci-unit-test on a host with redis-server."
.PHONY: unit-test
unit-test: docker-build
	docker run -ti --rm -v $(PWD):$(WORKDIR) -u $(UID):$(UID) --name $(SERVICE_NAME) $(REPOSITORY)-dev /bin/sh -c '$(UNIT_TEST_CMD); status=$$?; $(REDIS_SERVER_WARNING); exit $$status'

.PHONY: ci-unit-test
ci-unit-test:
	$(UNIT_TEST_CMD)

# Runs the tests of ci-unit-test and writes coverage.out. The CI unit-test job
# sends this file to Codecov.
.PHONY: ci-unit-test-coverage
ci-unit-test-coverage:
	$(UNIT_TEST_COVERAGE_CMD)

.PHONY: ci-integration-test
ci-integration-test:
	$(GO_INTEGRATION_TEST_CMD)

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

# Generate the typed clientset in client/k8s/clientset. The clientset changes
# only when the shape of the RedisFailover API changes. DeepCopy comes from
# generate-deepcopy.
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

# controller-gen version the committed DeepCopy code and CRD manifests are
# generated with. verify-codegen compares byte for byte, so CI and the
# pre-commit hook need this exact version.
.PHONY: install-controller-gen
install-controller-gen:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

# Generate CRD using controller-gen (install it with `make install-controller-gen`)
.PHONY: generate-crd
generate-crd:
	controller-gen crd paths=./api/... output:crd:dir=./manifests
	cp -f manifests/databases.spotahome.com_redisfailovers.yaml manifests/kustomize/base/
	cp -f manifests/databases.spotahome.com_redisfailovers.yaml charts/redisoperator/crds/

# Generate the DeepCopy methods (zz_generated.deepcopy.go) with controller-gen.
# This target needs no Docker, so verify-codegen and the pre-commit hook can
# run it in each environment.
.PHONY: generate-deepcopy
generate-deepcopy:
	controller-gen object paths=./api/...

# All the code that controller-gen generates. update-codegen and mocks use
# Docker and change less frequently. verify-codegen and the pre-commit hook do
# not check them.
.PHONY: generate-api
generate-api: generate-deepcopy generate-crd

# Fails if the output of `make generate-api` is different from the committed
# files. CI (.github/workflows/ci.yaml) and the pre-commit hook
# (.githooks/pre-commit) call this target, so the check is in one place.
#
# The CRD embeds the schema of every corev1 type RedisFailover references
# (PodSpec, Volume, ...), so a k8s.io/api bump in go.mod changes it too.
# Such a bump needs `make generate-crd` as well, or the API server prunes
# the new fields from RedisFailovers.
.PHONY: verify-codegen
verify-codegen: generate-deepcopy generate-crd
	@git diff --exit-code -- api/ || \
		(echo ""; \
		echo "Generated DeepCopy code is out of date."; \
		echo "Run 'make generate-deepcopy' and commit the result."; \
		exit 1)
	@git diff --exit-code -- $(CRD_MANIFESTS) || \
		(echo ""; \
		echo "Generated CRD manifests are out of date."; \
		echo "Run 'make generate-crd' with controller-gen $(CONTROLLER_GEN_VERSION) and commit the result."; \
		exit 1)

# Run once for each clone. Git does not version .git/hooks, so this target
# tells git to use the versioned hooks in .githooks.
.PHONY: install-hooks
install-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks installed from .githooks/. verify-codegen now runs before each commit that touches api/ or go.mod."
