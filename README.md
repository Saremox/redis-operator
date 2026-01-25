# redis-operator

[![CI](https://github.com/buildio/redis-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/buildio/redis-operator/actions/workflows/ci.yml)
[![E2E Tests](https://github.com/buildio/redis-operator/actions/workflows/e2e.yml/badge.svg)](https://github.com/buildio/redis-operator/actions/workflows/e2e.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/buildio/redis-operator)](https://goreportcard.com/report/github.com/buildio/redis-operator)

Redis Operator creates/configures/manages redis-failovers atop Kubernetes.

This is a fork of `spotahome/redis-operator` → `Saremox/redis-operator` → `buildio/redis-operator`.

## What's New in v1.6.1

**Disable Service Links** ([#3](https://github.com/buildio/redis-operator/issues/3))

v1.6.1 sets `enableServiceLinks: false` on all pods to prevent startup failures in namespaces with many services. Kubernetes by default injects environment variables for every service in the namespace, which can exceed limits and cause pod failures.

## What's New in v1.6.0

**CNPG-style Instance Manager** ([#2](https://github.com/buildio/redis-operator/issues/2))

v1.6.0 introduces an optional instance manager that runs as PID 1 in Redis containers, following the [CloudNativePG model](https://cloudnative-pg.io/documentation/current/instance_manager/) which has proven reliable at scale.

**Features:**
- **RDB tempfile cleanup** - Automatically removes stale `temp-*.rdb` files on startup, preventing disk exhaustion during crash loops
- **Zombie process reaper** - Properly handles SIGCHLD for BGSAVE/BGREWRITEAOF child processes
- **Graceful shutdown** - Timeout escalation (SIGTERM → SIGKILL) for reliable shutdown

**Enable it per-RedisFailover:**
```yaml
apiVersion: databases.spotahome.com/v1
kind: RedisFailover
metadata:
  name: my-redis
spec:
  redis:
    replicas: 3
    instanceManagerImage: ghcr.io/buildio/redis-operator:v1.6.1
  sentinel:
    replicas: 3
```

### Roadmap

| Version | Instance Manager Status | Notes |
|---------|------------------------|-------|
| v1.6.1 | Opt-in via `instanceManagerImage` | Current release |
| v1.7.0 | Enabled by default | Opt-out via `instanceManagerImage: ""` |
| v2.0.0 | Required | Legacy mode removed |

See [Issue #2](https://github.com/buildio/redis-operator/issues/2) for full details on readiness criteria and transition plan.

## Requirements

- Kubernetes: 1.21+
- Redis: 6+ (also supports Valkey 8)

Tested against Kubernetes 1.29, 1.30, 1.31, 1.32, 1.33, 1.34 and Redis 6, 7.

## Quick Start

### Install from GitHub Container Registry

```bash
# Install CRD
kubectl apply --server-side -f https://raw.githubusercontent.com/buildio/redis-operator/main/manifests/databases.spotahome.com_redisfailovers.yaml

# Install operator
helm upgrade --install redis-operator oci://ghcr.io/buildio/redis-operator/charts/redisoperator \
  --namespace redis-operator --create-namespace
```

### Install with Helm

```bash
helm repo add redis-operator https://buildio.github.io/redis-operator
helm repo update
helm install redis-operator redis-operator/redis-operator
```

### Install with kubectl

```bash
REDIS_OPERATOR_VERSION=v1.6.1
kubectl apply --server-side -f https://raw.githubusercontent.com/buildio/redis-operator/${REDIS_OPERATOR_VERSION}/manifests/databases.spotahome.com_redisfailovers.yaml
kubectl apply -f https://raw.githubusercontent.com/buildio/redis-operator/${REDIS_OPERATOR_VERSION}/example/operator/all-redis-operator-resources.yaml
```

### Install with Kustomize

```bash
# Default installation with RBAC, service account, resource limits
kustomize build github.com/buildio/redis-operator/manifests/kustomize/overlays/default?ref=v1.6.1 | kubectl apply -f -

# Minimal installation
kustomize build github.com/buildio/redis-operator/manifests/kustomize/overlays/minimal?ref=v1.6.1 | kubectl apply -f -

# Full installation with Prometheus ServiceMonitor
kustomize build github.com/buildio/redis-operator/manifests/kustomize/overlays/full?ref=v1.6.1 | kubectl apply -f -
```

## Updating

### Update CRD

Helm only manages CRD creation on first install. To update the CRD:

```bash
REDIS_OPERATOR_VERSION=v1.6.1
kubectl replace --server-side -f https://raw.githubusercontent.com/buildio/redis-operator/${REDIS_OPERATOR_VERSION}/manifests/databases.spotahome.com_redisfailovers.yaml
```

Then upgrade the operator:

```bash
helm upgrade redis-operator redis-operator/redis-operator
```

## Usage

### Create a Redis Failover

```bash
kubectl apply -f https://raw.githubusercontent.com/buildio/redis-operator/v1.6.1/example/redisfailover/basic.yaml
```

This creates the following resources:
- `rfr-<NAME>`: Redis StatefulSet and ConfigMap
- `rfs-<NAME>`: Sentinel Deployment, ConfigMap, and Service

**Note:** The RedisFailover name must be ≤48 characters.

### Enable Instance Manager

To use the CNPG-style instance manager for improved reliability:

```yaml
apiVersion: databases.spotahome.com/v1
kind: RedisFailover
metadata:
  name: my-redis
spec:
  redis:
    replicas: 3
    instanceManagerImage: ghcr.io/buildio/redis-operator:v1.6.1
  sentinel:
    replicas: 3
```

When `instanceManagerImage` is set:
1. An init container copies the `redis-instance` binary to a shared volume
2. The main container runs `redis-instance run` as PID 1
3. The instance manager performs cleanup and manages Redis as a child process

### Instance Manager CLI

The `redis-instance` binary provides the following commands:

```bash
# Run as instance manager (PID 1 mode)
redis-instance run --redis-conf /redis/redis.conf --data-dir /data --db-filename dump.rdb

# Standalone cleanup (removes stale RDB files)
redis-instance cleanup --data-dir /data --db-filename dump.rdb

# Dry-run cleanup (show what would be removed)
redis-instance cleanup --data-dir /data --dry-run
```

### Connection

Connect using a [Sentinel-ready client library](https://redis.io/topics/sentinel-clients):

```
url: rfs-<NAME>
port: 26379
master-name: mymaster
```

### Enable Authentication

```bash
kubectl create secret generic redis-auth --from-literal=password=your-password
```

```yaml
apiVersion: databases.spotahome.com/v1
kind: RedisFailover
metadata:
  name: my-redis
spec:
  redis:
    replicas: 3
  sentinel:
    replicas: 3
  auth:
    secretPath: redis-auth
```

### Persistence

Enable persistent storage with a PVC:

```yaml
spec:
  redis:
    storage:
      persistentVolumeClaim:
        metadata:
          name: redis-data
        spec:
          accessModes: [ReadWriteOnce]
          resources:
            requests:
              storage: 10Gi
      keepAfterDeletion: true  # Optional: retain PVCs when RedisFailover is deleted
```

See [persistent-storage.yaml](example/redisfailover/persistent-storage.yaml) for a complete example.

### Custom Configuration

Configure Redis and Sentinel via `customConfig`:

```yaml
spec:
  redis:
    customConfig:
      - maxmemory 2gb
      - maxmemory-policy allkeys-lru
  sentinel:
    customConfig:
      - down-after-milliseconds 5000
```

**Note:** Configuration is applied via `CONFIG SET` at runtime. Do not modify control options like `port`, `bind`, or `dir`.

### Affinity and Tolerations

- [Node Affinity](example/redisfailover/node-affinity.yaml)
- [Pod Anti-Affinity](example/redisfailover/pod-anti-affinity.yaml)
- [Tolerations](example/redisfailover/tolerations.yaml)
- [Topology Spread Constraints](example/redisfailover/topology-spread-contraints.yaml)

### Security Context

- [Pod Security Context](example/redisfailover/security-context.yaml)
- [Container Security Context](example/redisfailover/container-security-context.yaml)

### Bootstrapping

Migrate from an existing Redis instance:

```yaml
spec:
  bootstrapNode:
    host: existing-redis.example.com
    port: "6379"
    allowSentinels: false  # Set true to also create Sentinels pointing to bootstrap node
```

See [bootstrapping.yaml](example/redisfailover/bootstrapping.yaml) for details.

## CI/CD

This project includes comprehensive GitHub Actions workflows:

| Workflow | Triggers | Description |
|----------|----------|-------------|
| CI | Push, PR | Build, lint, unit tests, integration tests, Docker build |
| E2E | PR | Full end-to-end tests in minikube cluster |
| Release | Tags | Multi-arch image build and push to GHCR |

### E2E Tests

The E2E workflow validates:
- Instance manager runs as PID 1
- RDB cleanup works on pod restart
- Redis remains functional after restart

## Development

### Generate CRD

Requires [controller-gen](https://github.com/kubernetes-sigs/controller-tools) v0.20.0+ for Go 1.25+:

```bash
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
make generate-crd
```

### Run Tests

```bash
make ci-unit-test
make ci-integration-test
```

### Build Docker Image

```bash
make image
```

## Cleanup

### Remove Operator

```bash
helm uninstall redis-operator
kubectl delete crd redisfailovers.databases.spotahome.com
```

**Warning:** Deleting the CRD removes all RedisFailover resources and their managed objects.

### Remove Single RedisFailover

```bash
kubectl delete redisfailover <NAME>
```

All managed resources are automatically cleaned up via OwnerReferences.

## Docker Images

Images are published to GitHub Container Registry:

- **Operator & Instance Manager**: `ghcr.io/buildio/redis-operator`
- **Helm Chart**: `oci://ghcr.io/buildio/redis-operator/charts/redisoperator`

## Documentation

- [API Reference](docs/)
- [Examples](example/)
- [GoDoc](https://godoc.org/github.com/buildio/redis-operator)
