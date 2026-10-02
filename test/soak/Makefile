VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= ghcr.io/saremox/redis-operator-soak
LDFLAGS := -s -w -X main.version=$(VERSION)

# kind-e2e settings:
# - OPERATOR_VERSION installs a released chart and image instead of an
#   operator built from this checkout.
# - E2E_PROFILE is full, versions or chaos.
# - DURATION is how many seconds the mutator starts mutations. Defaults:
#   2100 for full, 3000 for versions and 1500 for chaos.
# - SUBNET is the second octet of the pod subnet.
# - chaos upgrades between the git refs UPGRADE_FROM and UPGRADE_TO, which
#   it builds. With OPERATOR_VERSION, it upgrades from the release
#   UPGRADE_FROM to OPERATOR_VERSION.
CLUSTER ?= soak
KIND_NODE ?= v1.35.0
SUBNET ?= 244
OPERATOR_VERSION ?=
E2E_PROFILE ?= full
DURATION ?=
UPGRADE_FROM ?=
UPGRADE_TO ?=
PROMETHEUS_IMAGE ?= prom/prometheus:v3.7.3

.PHONY: build test lint image kind-up kind-e2e check-monitoring

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
	CLUSTER=$(CLUSTER) KIND_NODE=$(KIND_NODE) SUBNET=$(SUBNET) OPERATOR_VERSION=$(OPERATOR_VERSION) \
		E2E_PROFILE=$(E2E_PROFILE) DURATION=$(DURATION) UPGRADE_FROM=$(UPGRADE_FROM) UPGRADE_TO=$(UPGRADE_TO) \
		./e2e/kind-e2e.sh

# promtool checks the alerts and runs e2e/rules-test.yaml on them. The
# dashboard must be JSON with panels.
check-monitoring:
	@tmp=$$(mktemp -d) && chmod 755 $$tmp && \
	yq '.spec' deploy/monitoring/prometheusrule.yaml >$$tmp/rules.yaml && cp e2e/rules-test.yaml $$tmp/ && chmod 644 $$tmp/* && \
	docker run --rm -v $$tmp:/r -w /r --entrypoint promtool $(PROMETHEUS_IMAGE) check rules rules.yaml && \
	docker run --rm -v $$tmp:/r -w /r --entrypoint promtool $(PROMETHEUS_IMAGE) test rules rules-test.yaml && \
	jq -e '.panels | length > 0' deploy/monitoring/dashboard.json >/dev/null; rc=$$?; rm -rf $$tmp; exit $$rc
