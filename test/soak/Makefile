VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= ghcr.io/saremox/redis-operator-soak
LDFLAGS := -s -w -X main.version=$(VERSION)

# kind-e2e settings: see e2e/kind-e2e.sh.
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
# dashboard must be JSON with panels. TestMonitoringNames checks the metric
# names of both.
check-monitoring:
	@tmp=$$(mktemp -d) && chmod 755 $$tmp && \
	yq '.spec' deploy/monitoring/prometheusrule.yaml >$$tmp/rules.yaml && cp e2e/rules-test.yaml $$tmp/ && chmod 644 $$tmp/* && \
	docker run --rm -v $$tmp:/r -w /r --entrypoint promtool $(PROMETHEUS_IMAGE) check rules rules.yaml && \
	docker run --rm -v $$tmp:/r -w /r --entrypoint promtool $(PROMETHEUS_IMAGE) test rules rules-test.yaml && \
	jq -e '.panels | length > 0' deploy/monitoring/dashboard.json >/dev/null; rc=$$?; rm -rf $$tmp; exit $$rc
