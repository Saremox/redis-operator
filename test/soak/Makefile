VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= ghcr.io/saremox/redis-operator-soak
LDFLAGS := -s -w -X main.version=$(VERSION)

# kind-e2e settings. OPERATOR_VERSION installs a released chart and image
# instead of building the operator from this checkout. E2E_PROFILE is full
# or versions. DURATION is how many seconds the mutator starts mutations,
# by default 2100 for full and 3000 for versions.
CLUSTER ?= soak
KIND_NODE ?= v1.35.0
OPERATOR_VERSION ?=
E2E_PROFILE ?= full
DURATION ?=

.PHONY: build test lint image kind-up kind-e2e

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/soak ./cmd/soak

test:
	go test ./...

lint:
	golangci-lint run ./...

image:
	docker build -f Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) ../..

kind-up:
	../../.claude/skills/kind-cluster/kind-up.sh $(CLUSTER) $(KIND_NODE)

kind-e2e: build
	CLUSTER=$(CLUSTER) KIND_NODE=$(KIND_NODE) OPERATOR_VERSION=$(OPERATOR_VERSION) \
		E2E_PROFILE=$(E2E_PROFILE) DURATION=$(DURATION) ./e2e/kind-e2e.sh
