# Copilot Instructions for redis-operator

## Project Overview

This repository is a Kubernetes operator that creates, configures, and manages Redis Failover clusters (Redis + Sentinel) on Kubernetes. It is a fork of `spotahome/redis-operator`.

- **Language**: Go (module: `github.com/saremox/redis-operator`)
- **Go version**: See `go.mod` for the current version
- **Kubernetes API**: Uses `k8s.io/client-go` and the custom CRD `RedisFailover` (group: `databases.spotahome.com/v1`)

## Repository Structure

- `api/redisfailover/v1/` – CRD type definitions and defaulting logic
- `client/` – Generated Kubernetes client code (do not edit manually)
- `cmd/redisoperator/` – Main entry point
- `operator/` – Core operator reconciliation logic
- `service/` – Business logic for managing Redis and Sentinel resources
- `metrics/` – Prometheus metrics
- `mocks/` – Auto-generated mocks (do not edit manually)
- `manifests/` – Kubernetes manifests (CRD YAML, Kustomize overlays)
- `charts/redisoperator/` – Helm chart
- `example/` – Example `RedisFailover` custom resources
- `test/` – Integration tests
- `scripts/` – Build and test helper scripts

## Building and Testing

```bash
# Build the binary
go build -v ./cmd/redisoperator

# Run unit tests
make ci-unit-test
# or directly:
go test $(go list ./... | grep -v /vendor/) -v

# Run integration tests (requires a running Kubernetes cluster)
make ci-integration-test

# Lint
golangci-lint run --timeout=15m

# Helm chart tests
make helm-test
```

## Code Style and Conventions

- Follow standard Go conventions (`gofmt`, `goimports`)
- Use `github.com/sirupsen/logrus` for logging; do not introduce other loggers
- Error handling: wrap errors with context using `fmt.Errorf("...: %w", err)`
- All Kubernetes resource manipulation must go through the service layer (`service/` package), not directly in the operator
- Unit tests use `github.com/stretchr/testify` (assert/require); mock interfaces are in `mocks/` and generated with `go generate`
- Keep generated code (`client/`, `mocks/`) separate from hand-written code; regenerate with `make update-codegen` or `make generate`

## CRD and API Changes

- The `RedisFailover` CRD spec is defined in `api/redisfailover/v1/types.go`
- Default values are set in `api/redisfailover/v1/defaults.go`
- After changing the API types, regenerate the CRD manifest: `make generate-crd`
- After changing the API types, regenerate the client: `make update-codegen`
- Keep backwards compatibility when changing the CRD spec; use optional fields with defaults

## Kubernetes Operator Patterns

- The operator uses the `kooper` framework (`github.com/spotahome/kooper/v2`) for controller/reconciler wiring
- The reconciliation loop is in `operator/redisfailover/`
- All Kubernetes resources created by the operator carry owner references pointing to the `RedisFailover` CR
- Redis Statefulsets use the prefix `rfr-<name>`; Sentinel Deployments use `rfs-<name>`
- The maximum name length for a `RedisFailover` is 48 characters

## Helm Chart

- Chart source is in `charts/redisoperator/`
- CRDs are in `charts/redisoperator/crds/`
- After changing the CRD, copy the updated manifest into `charts/redisoperator/crds/` and `manifests/kustomize/base/`

## CI / Workflow

- CI is defined in `.github/workflows/ci.yaml`
- All PRs must pass: build, lint (golangci-lint), unit tests, integration tests (multi-version Kubernetes matrix), and Helm chart tests
- Docker images are built for `linux/amd64` and `linux/arm64`
