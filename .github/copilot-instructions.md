# Copilot Instructions for redis-operator

## Project Overview

This repository is a Kubernetes operator that creates, configures, and manages Redis Failover clusters on Kubernetes. By default, the operator manages failover itself. With `sentinel.enabled: true`, Redis Sentinel does the failover. It is a fork of `spotahome/redis-operator`.

- **Language**: Go (module: `github.com/saremox/redis-operator`)
- **Go version**: See `go.mod` for the current version
- **Kubernetes API**: Uses `k8s.io/client-go` and the custom CRD `RedisFailover` (group: `databases.spotahome.com`, version: `v1`)

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

# Run integration tests. They need a Kubernetes cluster with the RedisFailover CRD,
# and a host route to the pod IPs.
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
- Write documentation, comments and pull request descriptions as `docs/writing.md` says (ASD-STE100: the why, short sentences, no fluff)

## CRD and API Changes

- The `RedisFailover` CRD spec is defined in `api/redisfailover/v1/types.go`
- `Validate()` in `api/redisfailover/v1/validate.go` sets the defaults. `api/redisfailover/v1/defaults.go` holds the default values
- After a change of the API types, regenerate the DeepCopy code and the CRD manifests: `make generate-api`. Stage the changes: `git add api/ manifests/ charts/redisoperator/crds/`. Then run `make verify-codegen`, the same check that CI runs. It compares with the staged files
- After changing the API types, regenerate the client: `make update-codegen`
- Keep backwards compatibility when changing the CRD spec; use optional fields with defaults

## Kubernetes Operator Patterns

- The controller is built directly on client-go informers and a workqueue (`operator/redisfailover/controller.go`)
- The reconciliation loop is in `operator/redisfailover/`
- Each Kubernetes object that the operator creates has an owner reference to the `RedisFailover`. The exception is a PVC with `storage.keepAfterDeletion: true`
- Redis Statefulsets use the prefix `rfr-<name>`; Sentinel Deployments use `rfs-<name>`
- The maximum name length for a `RedisFailover` is 48 characters

## Helm Chart

- Chart source is in `charts/redisoperator/`
- CRDs are in `charts/redisoperator/crds/`
- `make generate-crd` (part of `make generate-api`) copies the CRD manifest into `charts/redisoperator/crds/` and `manifests/kustomize/base/`. Do not edit these copies by hand

## CI / Workflow

- CI is defined in `.github/workflows/ci.yaml` and `.github/workflows/e2e.yml`. The `ci.yaml` file calls `e2e.yml`
- A PR must pass these jobs: build, lint (golangci-lint), verify-codegen, unit tests, integration tests (multi-version Kubernetes matrix), Helm chart tests, the Docker build, and the e2e test (minikube, operator-managed mode)
- The `changes` job in `ci.yaml` lists the paths that each job reads. The workflow skips a job when the PR changes none of its paths, for example a PR that changes only documentation. When a job reads a new path, add the path to the filter of the job
- Docker images are built for `linux/amd64` and `linux/arm64`
