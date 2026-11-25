
# redis-operator

This is a fork of the `spotahome/redis-operator` repository.

[![Build Status](https://github.com/Saremox/redis-operator/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/Saremox/redis-operator)
[![Go Report Card](https://goreportcard.com/badge/github.com/Saremox/redis-operator)](https://goreportcard.com/report/github.com/Saremox/redis-operator)
[![codecov](https://codecov.io/gh/Saremox/redis-operator/branch/main/graph/badge.svg)](https://codecov.io/gh/Saremox/redis-operator)

The operator creates, configures and manages Redis with automatic failover on Kubernetes. Each Redis deployment is a `RedisFailover` resource.

## Requirements

- Kubernetes version: 1.32 or higher
- Redis version: 6 or higher

CI runs the integration tests on Kubernetes 1.35.8, 1.36.4 and 1.37.0. It runs the end-to-end test on Kubernetes 1.32.0. Both use Redis 7.2. The soak test runs Redis 6.2, 7.2, 7.4 and 8 and Valkey 7.2, 8 and 9. See [Tested server versions and upgrade paths](docs/versions.md#tested-server-versions-and-upgrade-paths).

## Operator deployment on Kubernetes

Moved to [Operator deployment on Kubernetes](docs/install.md#operator-deployment-on-kubernetes).

### Install with the Helm chart

Moved to [Install with the Helm chart](docs/install.md#install-with-the-helm-chart).

#### Update the Helm chart

Moved to [Update the Helm chart](docs/install.md#update-the-helm-chart).

### Install with kubectl

Moved to [Install with kubectl](docs/install.md#install-with-kubectl).

### Install with kustomize

Moved to [Install with kustomize](docs/install.md#install-with-kustomize).

### Operator flags

Moved to [Operator flags](docs/install.md#operator-flags).

### Operator metrics

Moved to [Operator metrics](docs/install.md#operator-metrics).

### Security notes

Moved to [Security notes](docs/install.md#security-notes).

#### NetworkPolicy

Moved to [NetworkPolicy](docs/install.md#networkpolicy).

## Usage

When the operator runs, you can create, update and delete RedisFailover resources.

To deploy a new RedisFailover, create a [specification](example/redisfailover/basic.yaml):

```
REDIS_OPERATOR_VERSION=<release-tag>
kubectl create -f https://raw.githubusercontent.com/Saremox/redis-operator/${REDIS_OPERATOR_VERSION}/example/redisfailover/basic.yaml
```

The example `basic.yaml` enables Sentinel. Without `spec.sentinel.enabled: true`, Sentinel is disabled and the operator manages the failover, as in [`minimum-sentinelless.yaml`](example/redisfailover/minimum-sentinelless.yaml). Set `spec.sentinel.enabled: true` to deploy the Sentinel resources. Use `spec.sentinel.failoverTimeout` to tune the failover of the operator.

`spec.sentinel.failoverTimeout` (default `10s`, `0s` disables it) is the time that the operator waits for a master that does not answer while its pod runs. The wait prevents a failover after a short stall, for example a GC pause, because a failover makes all replicas resync and can lose writes.

The wait starts at the first missed check. The operator records this time on the master pod in the `redisfailovers.databases.spotahome.com/unreachable-since` annotation, so an operator restart or a new leader keeps the deadline. `status.message` shows the wait.

The operator does not replace a master that answers no check while its pod is ready, because a promotion can then give two masters. When the pod becomes not ready, the operator promotes a replica after the timeout. When the operator finds no master, for example because its pod is gone, it elects a master immediately. A master pod in deletion gets a wait while it is ready, because it can still accept writes.

An old master that did not answer during a failover can come back as a second master. The operator then makes it a replica of the pod labelled master, because that label shows the master that the operator elected. If the elected master also does not answer when the old master comes back, the operator can move the label back to the old master. Then the writes since the failover are lost.

The operator also closes the client connections of the old master, because after `REPLICAOF` their writes fail with `READONLY` until the clients connect again. The flag `--disconnect-clients-on-demotion=false` keeps them open. When the label does not identify one master, the status shows `multiple masters detected, fix manually`.

For each RedisFailover, the operator creates these resources in Kubernetes:

- `rfr-<NAME>`: Redis configmap
- `rfr-<NAME>`: Redis statefulset
- `rfr-<NAME>`: Redis service (if `redis.exporter.enabled` is `true`)
- `rfrm-<NAME>`: Redis master service
- `rfrs-<NAME>`: Redis replica service
- `rfr-s-<NAME>`: Redis shutdown script configmap (if `redis.shutdownConfigMap` is not set)
- `rfr-readiness-<NAME>`: Redis readiness script configmap
- `rfr-<NAME>`: Redis PodDisruptionBudget (if `redis.disablePodDisruptionBudget` is not `true`)
- `rfs-<NAME>`: Sentinel configmap (if Sentinels run, see below)
- `rfs-<NAME>`: Sentinel deployment (if Sentinels run, see below)
- `rfs-<NAME>`: Sentinel service (if Sentinels run, see below)
- `rfs-<NAME>`: Sentinel PodDisruptionBudget (if Sentinels run and `sentinel.disablePodDisruptionBudget` is not `true`)
- `rfs-sa-<NAME>`: Sentinel service account (if Sentinels run and `sentinel.serviceAccountName` is not set)

Sentinels run when `spec.sentinel.enabled` is `true`. With `bootstrapNode`, they also need `bootstrapNode.allowSentinels: true`.

**NOTE**: `NAME` is the name of the RedisFailover.
**IMPORTANT**: the name of a RedisFailover can have a maximum of 48 characters. The operator adds a Redis or Sentinel prefix to the name, and a StatefulSet name has a length limit.
**IMPORTANT**: two RedisFailovers in one namespace must not generate the same object name. RedisFailover `X` collides with RedisFailover `s-X` and with RedisFailover `readiness-X`. For example, RedisFailover `foo` generates the shutdown script configmap `rfr-s-foo`, and RedisFailover `s-foo` generates the Redis configmap `rfr-s-foo`. The operator does not change an object that another RedisFailover controls. The RedisFailover that does not control the object does not reconcile, and `status.message` shows the error.

### Protect the master from cluster-autoscaler eviction

Moved to [Protect the master from cluster-autoscaler eviction](docs/configuration.md#protect-the-master-from-cluster-autoscaler-eviction).

### Sentinel update strategy and PodDisruptionBudget

Moved to [Sentinel update strategy and PodDisruptionBudget](docs/configuration.md#sentinel-update-strategy-and-poddisruptionbudget).

### Persistence

Moved to [Persistence](docs/configuration.md#persistence).

### Node affinity, pod anti-affinity and tolerations

Moved to [Node affinity, pod anti-affinity and tolerations](docs/configuration.md#node-affinity-pod-anti-affinity-and-tolerations).

### Topology spread constraints

Moved to [Topology spread constraints](docs/configuration.md#topology-spread-constraints).

### Custom configurations

Moved to [Custom configurations](docs/configuration.md#custom-configurations).

### Managed maxmemory

Moved to [Managed maxmemory](docs/configuration.md#managed-maxmemory).

### Pod updates

Moved to [Pod updates](docs/configuration.md#pod-updates).

### Pod updates with one Redis pod

Moved to [Pod updates with one Redis pod](docs/configuration.md#pod-updates-with-one-redis-pod).

### In-place resize

Moved to [In-place resize](docs/autoscaling.md#in-place-resize).

### Autoscaling

Moved to [Autoscaling](docs/autoscaling.md#autoscaling).

#### Mode Auto

Moved to [Mode Auto](docs/autoscaling.md#mode-auto).

### Custom shutdown script

Moved to [Custom shutdown script](docs/customization.md#custom-shutdown-script).

### Sentinel failover and write loss

Moved to [Sentinel failover and write loss](docs/configuration.md#sentinel-failover-and-write-loss).

### Custom SecurityContext

Moved to [Custom SecurityContext](docs/customization.md#custom-securitycontext).

### Custom containerSecurityContext at container level

Moved to [Custom containerSecurityContext at container level](docs/customization.md#custom-containersecuritycontext-at-container-level).

### Custom command

Moved to [Custom command](docs/customization.md#custom-command).

### Custom environment variables

Moved to [Custom environment variables](docs/customization.md#custom-environment-variables).

### Custom Priority Class

Moved to [Custom Priority Class](docs/customization.md#custom-priority-class).

### Custom Service Account

Moved to [Custom Service Account](docs/customization.md#custom-service-account).

### Custom Pod Annotations

Moved to [Custom Pod Annotations](docs/customization.md#custom-pod-annotations).

### Custom Service Annotations

Moved to [Custom Service Annotations](docs/customization.md#custom-service-annotations).

### Exporter authentication

Moved to [Exporter authentication](docs/customization.md#exporter-authentication).

### Control of label propagation

Moved to [Control of label propagation](docs/customization.md#control-of-label-propagation).

### ExtraVolumes and ExtraVolumeMounts

Moved to [ExtraVolumes and ExtraVolumeMounts](docs/customization.md#extravolumes-and-extravolumemounts).

## Connection to a RedisFailover

Connect through Sentinel or directly to the Redis master service. The failover mode tells which one to use.

When `spec.sentinel.enabled: true`, use a [Sentinel-ready](https://redis.io/topics/sentinel-clients) client library with these connection parameters:

```
url: rfs-<NAME>
port: 26379
master-name: mymaster
```

When Sentinel is disabled, connect directly to the master service:

```
url: rfrm-<NAME>
port: <redis-port> # defaults to 6379
```

Clients can also read from the replicas through `rfrs-<NAME>`. A replica is ready, and thus behind that service, only while it has the data of the master. A replica is not ready in these cases:

- During a full sync.
- Until its first sync after its start is complete. A replica that cannot load the RDB format of the master never completes this sync.
- When its link to the master is down for longer than a failover can take. Without Sentinel, this time is 60 seconds plus `spec.sentinel.failoverTimeout`. With Sentinel, it is 60 seconds plus the Sentinel `down-after-milliseconds` and `failover-timeout` (from `spec.sentinel.customConfig`).

Thus the replicas stay ready during a failover.

### Redis authentication

Moved to [Redis authentication](docs/authentication.md#redis-authentication).

### Bootstrap from a different Redis instance
To migrate from a different Redis instance, set `bootstrapNode` in the spec of your `RedisFailover` resource.

These are the fields of `bootstrapNode`:

|       Key      | Type         | Description                                                                                                                                                                               | Example File                                                                                 |
|:--------------:|--------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| host           | **required** | The IP address of the Redis instance, or the ClusterIP of a Kubernetes Service for the Redis pods                                                                                         | [bootstrapping.yaml](example/redisfailover/bootstrapping.yaml)                               |
| port           | _optional_   | The port of the Redis instance. Defaults to `6379`.                                                                                                                                       | [bootstrapping-with-port.yaml](example/redisfailover/bootstrapping-with-port.yaml)           |
| allowSentinels | _optional_   | Lets the operator also create the Sentinel resources and point them to the `host` and `port`. This applies only when `spec.sentinel.enabled` is `true`. By default, the operator does **not** create the Sentinel resources in bootstrap mode. | [bootstrapping-with-sentinels.yaml](example/redisfailover/bootstrapping-with-sentinels.yaml) |

#### Bootstrap mode
With a `bootstrapNode`, the operator always makes all Redis instances replicas of the `bootstrapNode` host.
Thus a `RedisFailover` can replicate from a different Redis instance, and you can move the clients from one instance to the other.

**Note: by default, the Redis instances get `replica-priority 0`, so that Sentinel never promotes them to `master`. A `replica-priority` entry in `redis.customConfig` replaces this value.**

The operator starts the `RedisFailover` in one of two bootstrap modes: without Sentinels or with Sentinels.

#### Default bootstrap mode (without Sentinels)
By default, with a valid `bootstrapNode`, the operator creates **only the Redis instances**.
Thus the Sentinels of the new `RedisFailover` do not interfere with the Sentinels of the source `RedisFailover`.

#### Bootstrap mode with Sentinels
When `allowSentinels` is `true` and `spec.sentinel.enabled` is `true`, the operator also creates the Sentinel resources. These Sentinels monitor the
`bootstrapNode` as their master.

### Default versions

Moved to [Default versions](docs/versions.md#default-versions).

### Migrate to Valkey

Moved to [Migrate to Valkey](docs/versions.md#migrate-to-valkey).

### Tested server versions and upgrade paths

Moved to [Tested server versions and upgrade paths](docs/versions.md#tested-server-versions-and-upgrade-paths).

#### In-place downgrades

Moved to [In-place downgrades](docs/versions.md#in-place-downgrades).

## Cleanup

### Operator and CRD

Each RedisFailover has the finalizer `redisfailovers.databases.spotahome.com/finalizer`. Only the operator removes it, while it runs. Without the operator, the deletion of a RedisFailover and of the CRD does not complete.

To remove the operator and the CRD:

1. Delete all RedisFailovers:

   ```
   kubectl delete redisfailovers --all --all-namespaces
   ```

2. Wait until this command shows no RedisFailover:

   ```
   kubectl get redisfailovers --all-namespaces
   ```

3. Delete the operator, for example with `helm uninstall redis-operator`.
4. Delete the CRD:

   ```
   kubectl delete crd redisfailovers.databases.spotahome.com
   ```

If the operator is already deleted, remove its finalizer manually. This command keeps the finalizers of other controllers:

```
kubectl get redisfailover <NAME> -n <NAMESPACE> -o json \
  | jq '.metadata.finalizers -= ["redisfailovers.databases.spotahome.com/finalizer"]' \
  | kubectl replace -f -
```

PVCs with `keepAfterDeletion` stay after the deletion. Delete them manually when you do not need the data.

### Single RedisFailover

Kubernetes deletes all the objects that the operator created for a RedisFailover after the RedisFailover, because of their `OwnerReference`. PVCs with `keepAfterDeletion` stay. The operator must run to remove the finalizer.

```
kubectl delete redisfailover <NAME>
```

## Docker Images

### Redis Operator

* [Redis Operator Image](https://github.com/Saremox/redis-operator/pkgs/container/redis-operator)

## Documentation

For the code documentation, see [pkg.go.dev](https://pkg.go.dev/github.com/saremox/redis-operator).

For more information, see the [docs folder](docs):

- [Install and operate the operator](docs/install.md): Helm, kubectl, kustomize, flags, metrics, NetworkPolicy.
- [Configure a RedisFailover](docs/configuration.md): persistence, scheduling, custom configuration, `maxmemory`, pod updates.
- [Resize and autoscaling](docs/autoscaling.md): in-place resize and the memory autoscaler.
- [Customization](docs/customization.md): shutdown script, security context, command, environment, annotations, volumes.
- [Redis authentication](docs/authentication.md).
- [Server versions and upgrades](docs/versions.md): tested versions, Valkey, in-place downgrades.
- [Failover logic](docs/logic.md) and [development](docs/development.md).
