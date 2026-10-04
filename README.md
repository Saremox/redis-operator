# redis-operator

This is a fork of the `spotahome/redis-operator` repository.

[![Build Status](https://github.com/Saremox/redis-operator/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/Saremox/redis-operator)
[![Go Report Card](https://goreportcard.com/badge/github.com/Saremox/redis-operator)](https://goreportcard.com/report/github.com/Saremox/redis-operator)
[![codecov](https://codecov.io/gh/Saremox/redis-operator/branch/main/graph/badge.svg)](https://codecov.io/gh/Saremox/redis-operator)

The operator creates, configures and manages Redis with automatic failover on Kubernetes. Each Redis deployment is a `RedisFailover` resource.

## Requirements

- Kubernetes version: 1.32 or higher
- Redis version: 6 or higher

CI runs the integration tests on Kubernetes 1.35.8, 1.36.4 and 1.37.0, and the end-to-end test on Kubernetes 1.32.0. Both use Redis 7.2.

## Operator deployment on Kubernetes

Deploy the operator before you create a RedisFailover.
You can deploy it with the [kubectl manifests](example/operator), with [Kustomize](manifests/kustomize) or with the [Helm chart](charts/redisoperator).

### Install with the Helm chart

Install the chart from the Helm repository:

```
helm repo add redis-operator https://Saremox.github.io/redis-operator
helm repo update
helm install redis-operator redis-operator/redis-operator
```

The chart is also an OCI artifact next to the operator image. You can install it without a Helm repository:

```
helm install redis-operator oci://ghcr.io/saremox/redis-operator/charts/redis-operator --version <release-tag>
```

Each release tag, for example `4.0.0`, publishes the operator image and the Helm chart with the same version.
A release candidate, for example `4.0.0-rc1`, is a GitHub pre-release with the same artifacts.
Helm ignores release candidates unless you give `--version` or `--devel`.

#### Update the Helm chart

Read the [release notes](https://github.com/Saremox/redis-operator/releases) before an upgrade. If a release needs manual steps, its notes start with a warning that links to a guide in [docs/migrations](docs/migrations).

Helm installs the CRD only at the first `helm install`. `helm upgrade` does not change the CRD. To update the CRD, replace it with kubectl:

```
REDIS_OPERATOR_VERSION=<release-tag>
kubectl replace -f https://raw.githubusercontent.com/Saremox/redis-operator/${REDIS_OPERATOR_VERSION}/manifests/databases.spotahome.com_redisfailovers.yaml
```

The chart can also apply its CRD before each install and upgrade. Set `crds.upgradeHook.enabled=true`. A Helm hook Job then runs `kubectl apply` with its own ServiceAccount, which can change the RedisFailover CRD. The hook is off by default because of this permission. The charts of 4.2.0-rc2 and earlier run the hook with the operator image, which has no `kubectl`. Use the hook only with a later release.

```
helm upgrade redis-operator redis-operator/redis-operator
```

The CRD enables the `status` subresource, so a status write does not change the `metadata.generation` of a RedisFailover. An operator and a CRD from different releases cannot write the status. With the new CRD, the API server ignores the status writes of an older operator. With the old CRD, the status writes of the new operator fail. The RedisFailovers keep their status during the upgrade.

When you upgrade from a release without the `status` subresource, update the CRD and the operator together. `helm upgrade` updates the CRD only with `crds.upgradeHook.enabled=true`. A ClusterRole that you maintain yourself needs `get` and `patch` on `redisfailovers/status`. See the [4.2.0 migration guide](docs/migrations/4.2.0.md).

### Install with kubectl

Install the CRD and the operator with kubectl:

```
REDIS_OPERATOR_VERSION=<release-tag>
kubectl create -f https://raw.githubusercontent.com/Saremox/redis-operator/${REDIS_OPERATOR_VERSION}/manifests/databases.spotahome.com_redisfailovers.yaml
kubectl apply -f https://raw.githubusercontent.com/Saremox/redis-operator/${REDIS_OPERATOR_VERSION}/example/operator/all-redis-operator-resources.yaml
```

The manifest creates a Deployment with the name `redisoperator`. The manifest works only in the `default` namespace, because its ClusterRoleBinding and its ServiceMonitor name this namespace.

The manifests at a release tag deploy the operator image of that tag. The release workflow stops a release when they do not agree. In tags 4.2.0-rc2, 4.1.2 and older, the manifests deploy a different operator image. For these tags, set the image after the install:

```
kubectl set image deployment/redisoperator app=ghcr.io/saremox/redis-operator:${REDIS_OPERATOR_VERSION}
```

The manifest also contains a `ServiceMonitor`. Without the Prometheus Operator CRDs, `kubectl apply` reports an error for this resource. It still creates the other resources.

### Install with kustomize

You can change the kustomize setup of this repository with [components](https://kubectl.docs.kubernetes.io/guides/config_management/components/).
The overlays are presets for the most common use cases.

The `default` overlay installs the operator with the RBAC, the service account and default resource limits. The CRD is 1.1 MB, so a client-side `kubectl apply` fails on it. The reason is the 256 KiB limit for the annotations of an object. Apply the overlay server-side:

```shell
kustomize build github.com/Saremox/redis-operator/manifests/kustomize/overlays/default | kubectl apply --server-side -f -
```

The overlays install the operator in the `default` namespace, because the ClusterRoleBinding must name the namespace of the ServiceAccount. The `redis-operator.yaml` file of each GitHub release contains the `default` overlay. Apply this file also with `--server-side`. To use a different namespace, set `namespace:` in your own `kustomization.yaml`, as in the example below. `kubectl apply -n <namespace>` does not work with these files.

Run only one operator in a cluster. Each operator takes the leader lease in its own namespace, so two operators in different namespaces both change the RedisFailovers. When you move the operator to a different namespace, delete the old operator Deployment.

The `minimal` overlay is the `default` overlay without the resource limits. It also creates the RBAC and the service account. To use your own RBAC or service account, use the `base` and the [components](manifests/kustomize/components) in your own kustomization.

The `full` overlay adds a metrics Service and a Prometheus ServiceMonitor to the `default` overlay.

Pin the operator version in your configuration. Without a pin, you get each change of the development branch:

```shell
kustomize build github.com/Saremox/redis-operator/manifests/kustomize/overlays/default?ref=<release-tag> | kubectl apply --server-side -f -
```

The `?ref=<release-tag>` also pins the operator image. In tags 4.2.0-rc2, 4.1.2 and older, the overlays deploy a different operator image. Most of these tags deploy `ghcr.io/saremox/redis-operator:v1.4.0`. In tag 4.0.0, the `default` and `minimal` overlays deploy `ghcr.io/buildio/redis-operator:4.0.0`. For these tags, add the image to your `kustomization.yaml`. Only tag 4.0.0 needs the first entry:

```yaml
images:
  - name: ghcr.io/buildio/redis-operator
    newName: ghcr.io/saremox/redis-operator
    newTag: <release-tag>
  - name: ghcr.io/saremox/redis-operator
    newTag: <release-tag>
```

To change the resource limits, the labels or the namespace, write your own `kustomization.yaml` file:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: redis-operator

labels:
- pairs:
    app: redis-operator

resources:
  - github.com/Saremox/redis-operator/manifests/kustomize/overlays/full
```

See the manifests in [manifests/kustomize](manifests/kustomize) for more information.

### Operator metrics

The operator serves Prometheus metrics on `--listen-address` (default `:9710`) at `--metrics-path` (default `/metrics`). The flag `--enable-pprof` (default `false`) serves the Go profiler at `/debug/pprof/` on the same address. A heap profile can contain the Redis passwords.

## Usage

When the operator runs, you can create, update and delete RedisFailover resources.

To deploy a new RedisFailover, create a [specification](example/redisfailover/basic.yaml):

```
REDIS_OPERATOR_VERSION=<release-tag>
kubectl create -f https://raw.githubusercontent.com/Saremox/redis-operator/${REDIS_OPERATOR_VERSION}/example/redisfailover/basic.yaml
```

By default, Sentinel is disabled and the operator manages the failover. Set `spec.sentinel.enabled: true` to deploy the Sentinel resources. Use `spec.sentinel.failoverTimeout` to tune the failover of the operator.

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

### Protect the master from cluster-autoscaler eviction

With `redis.preventMasterEviction: true`, the operator sets the annotation `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` on the master pod.
It sets the value `"true"` on the replica pods. Thus the cluster-autoscaler does not drain the node of the master and does not cause a failover that is not necessary.
The annotation moves with the master. The default is `false`.

When it is `false`, the operator sets the value from `redis.podAnnotations`, or removes the value `"false"`. It keeps a `"true"`,
because a policy or the user can set it.

### Sentinel update strategy and PodDisruptionBudget

Use `sentinel.strategy` to change the update strategy of the Sentinel `Deployment`, for example `rollingUpdate.maxSurge` and `maxUnavailable`.
With required anti-affinity and `replicas == nodes`, the default rolling update cannot schedule a new pod and stops.
This strategy removes an old pod before it creates a new pod:

```yaml
spec:
  sentinel:
    strategy:
      type: RollingUpdate
      rollingUpdate:
        maxSurge: 0
        maxUnavailable: 1
```

The `PodDisruptionBudget` `minAvailable` for each component defaults to `2`, or to `1` when the `replicas` of that component is `2` or less.
To change it, set `redis.podDisruptionBudgetMinAvailable` or `sentinel.podDisruptionBudgetMinAvailable` to an integer or to a percentage string such as `"60%"`.

### Persistence

By default, Redis uses an `emptyDir` volume, so the data is lost when the pod is deleted.

To keep the data, add the full [PersistentVolumeClaim definition](example/redisfailover/persistent-storage.yaml) under `redis.storage.persistentVolumeClaim`.

**IMPORTANT**: By default, Kubernetes deletes the persistent volume claims when you delete the RedisFailover. To keep them, add `keepAfterDeletion: true` under `redis.storage`. [An example is given](example/redisfailover/persistent-storage-no-pvc-deletion.yaml). When you add the flag to an existing RedisFailover, the operator removes the owner reference of the RedisFailover from its PVCs. When you remove the flag again, the operator does not add the owner reference back, so these PVCs stay after the RedisFailover is deleted.

The operator removes the owner reference in its next reconcile. Before you delete the RedisFailover, make sure that no PVC shows the owner `RedisFailover`. Replace `<NAME>` with the name of the RedisFailover:

```bash
kubectl get pvc -l app.kubernetes.io/component=redis,app.kubernetes.io/name=<NAME> -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.ownerReferences[*].kind}{"\n"}{end}'
```

To make the volumes larger, increase the storage request in `redis.storage.persistentVolumeClaim`. At each reconcile, the operator resizes the PVCs to the new request. The StorageClass must allow volume expansion (`allowVolumeExpansion: true`). The operator does not make a PVC smaller, and it ignores other changes to the claim. When the API server refuses a resize, the operator only logs the error.

### Node affinity, pod anti-affinity and tolerations

Use node affinity, pod anti-affinity and tolerations to put the pods on isolated groups of nodes. Examples are given for [node affinity](example/redisfailover/node-affinity.yaml), [pod anti-affinity](example/redisfailover/pod-anti-affinity.yaml) and [tolerations](example/redisfailover/tolerations.yaml).

### Topology spread constraints

Use `topologySpreadConstraints` to spread the Redis or Sentinel pods evenly across zones or nodes. See the [topology spread constraints example](example/redisfailover/topology-spread-contraints.yaml) and the [Kubernetes documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/).

### Custom configurations

You can configure Redis and Sentinel with the `customConfig` option in their spec. It is a list of configuration options and their values. See the [custom config example file](example/redisfailover/custom-config.yaml).

The operator applies these options with `CONFIG SET` on Redis and `SENTINEL SET mymaster` on Sentinel, so a change does not restart Redis or Sentinel. For this reason, **the configmaps do not change** for this custom configuration. The `customConfig` entries of Redis are not in the `redis.conf` file. To see the actual Redis configuration, use [`redis-cli CONFIG GET *`](https://redis.io/commands/config-get).

**Important**: some Sentinel options need a conversion:

- Configuration on the `sentinel.conf`: `sentinel down-after-milliseconds mymaster 2000`
- Configuration on the `customConfig`: `down-after-milliseconds 2000`

The operator adds `down-after-milliseconds 5000` and `failover-timeout 10000` to the Sentinel `customConfig`, unless `customConfig` sets that option. Thus all the Sentinels use the same timeouts, also after a restart. The operator applies these values to the running Sentinels at the next reconcile, also after an upgrade, without a restart. To use the Sentinel built-in values, set `down-after-milliseconds 30000` and `failover-timeout 180000`. The [4.2.0 migration guide](docs/migrations/4.2.0.md) tells which RedisFailovers get new values on the upgrade to 4.2.0.

**Important 2**: do **NOT** change the options that the operator uses to control Redis and Sentinel, such as `port`, `bind` and `dir`.

Validation rejects a `redis.customCommandRenames` entry for a command that the RedisFailover needs:

- `AUTH`, `CLIENT`, `CONFIG`, `INFO`, `PING`, `PSYNC`, `REPLCONF`, `REPLICAOF` and `SLAVEOF`. The operator, the pod scripts or the replicas send them.
- `EXEC`, `MULTI`, `PUBLISH` and `SUBSCRIBE` when Sentinels run. Sentinel sends them.
- `ACL` when `redis.customConfig` sets `aclfile`. The operator sends `ACL LOAD` to apply the file.

The operator does not reconcile a RedisFailover that fails validation. The status shows `NotHealthy` and the error.

### Managed maxmemory

With `redis.maxMemory`, the operator sets `maxmemory` and `maxmemory-policy` from the memory limit of the Redis container. See the [maxmemory example file](example/redisfailover/maxmemory.yaml). The memory limit must be at least 64Mi. Otherwise the operator does not manage `maxmemory`, and the status message gives the reason.

`maxmemory` is `percent` (default `75`) of the limit, but at least 32Mi of the limit stays free. `policy` defaults to `noeviction`.

| Limit | maxmemory |
|---|---|
| 64Mi | 32Mi |
| 96Mi | 64Mi |
| 128Mi | 96Mi |
| 1Gi | 768Mi |

Keys in `customConfig` have priority over the managed values. Validation rejects `replica-ignore-maxmemory no`, because the replicas then evict keys independently of the master. The operator sets `replica-ignore-maxmemory yes` on the running pods.

To move `maxmemory` from `customConfig` to `maxMemory`:

1. Update the CRD and the operator.
2. Add `redis.maxMemory`.
3. Remove `maxmemory` and `maxmemory-policy` from `redis.customConfig`.

When you remove `maxMemory`, the running pods keep their current values until the operator recreates them. An in-place resize does not recreate a pod. To keep the values, set them in `customConfig`.

`maxmemory` follows the smallest Redis pod, because each replica holds the full dataset and the operator can promote any replica. A higher limit applies when all pods run with it. A lower limit applies before the operator replaces the pods.

The operator sets `maxmemory` below the used memory only with an `allkeys-*` policy. A `volatile-*` policy can evict all keys with a TTL and still not fit. With other policies, the operator keeps `maxmemory`, and the status message gives the reason. Until the data fits, the operator does not replace pods with the smaller limit. Pods that Kubernetes recreates for other reasons, for example a node drain, get the smaller limit.

For small instances, the default `client-output-buffer-limit` for `pubsub` (32mb) and `replica` (256mb) can be larger than the free part of the limit. Lower them with `customConfig`. A replica buffers a full `MULTI`/`EXEC` or `EVAL` before it applies it, so one large batch can cause an OOM kill of a replica.

### Pod updates

A spec change replaces the Redis pods one at a time: the replicas first and the master last. The operator replaces a pod when the previous pod is ready and all replicas are in sync with the master. The operator first replaces an unsynced replica that does not have the current spec, because that replica has no data to lose. A rollout can wait on one pod for more than 10 minutes, for example when a new image cannot load the data of the master. The status message then gives the pod and the reason, for example `rollout waiting on pod rfr-<NAME>-1 for more than 10m: not synced with the master`. The state stays `Healthy`, because the master still serves clients, and the message clears when the rollout continues.

### In-place resize

On Kubernetes 1.33 or later, an update that changes only the container CPU or memory resizes the Redis pods in place. The operator does not recreate them, so Redis does not reload data and the master does not fail over. The operator resizes one pod at a time, replicas first. To lower a memory limit in place, Kubernetes 1.35 is necessary. Set `redis.inPlaceResize: Disabled` to always recreate the pods.

The operator recreates a pod instead of a resize in place in these cases:

- The update changes more than container cpu and memory, adds or removes a request or a limit, or changes the QoS class.
- The kubelet does not support in-place resize, or reports the resize as infeasible.
- The kubelet refuses a memory limit below the current usage. The usage includes the page cache, so a retry also fails and the operator does not wait.
- The kubelet defers or fails the resize for more than 5 minutes, or does not apply it in 5 minutes.

The operator needs `patch` on `pods/resize` and `get` on `controllerrevisions`, which the chart, the kustomize and the example manifests grant. Without them, the operator recreates the pods.

### Custom shutdown script

By default, the operator gives each Redis pod a shutdown script. The script makes Redis `SAVE` its data before it stops. When Sentinel runs and the pod is the master, the script first pauses the writes and asks Sentinel to fail over. Thus Sentinel moves the master immediately and does not wait for `down-after-milliseconds`. The Redis pods have no service links, so the script finds Sentinel through the Service name `rfs-<NAME>` on port 26379.

After the failover, the script makes the old master a replica of the new master. The pause prevents the loss of writes that the old master acknowledged, because the clients get `READONLY` instead. If the pause fails, the script does not ask for a failover. Redis 7 and later then pause the writes when they stop and wait for their replicas, and Sentinel fails over after `down-after-milliseconds`. The pause fails on Redis before 6.2 and after a password change. After a password change, the `REDIS_PASSWORD` variable of a pod keeps the old password until the pod restarts.

The script waits a maximum of 12 seconds for the new master. Thus the script ends inside the default 30-second grace period, and the `SAVE` can run. After a password change, the `SAVE` of the script also fails. Then Redis saves its data when it stops only if `save` points are set. The default `redis.conf` sets `save 900 1` and `save 300 10`.

To replace the default script, create a ConfigMap with a `shutdown.sh` key in the namespace of the RedisFailover. Set its name in `redis.shutdownConfigMap`. See the [shutdown example file](example/redisfailover/custom-shutdown.yaml). A custom script replaces all of the default script, also the write pause. Without the pause, a failover can lose writes, see [Sentinel failover and write loss](#sentinel-failover-and-write-loss).

### Sentinel failover and write loss

Sentinel does not stop writes on the old master in a failover. If the old master still runs, it accepts writes until Sentinel makes it a replica. With the default timings, this occurs about 10 seconds after the promotion. The old master then copies the data of the new master, and the writes that it acknowledged in that interval are lost. The interval ends earlier if the operator moves the master label or makes the old master a replica first. In a test on kind, a `SENTINEL FAILOVER` lost 11 seconds of writes through `rfrm-<NAME>`.

The default shutdown script prevents this loss when the master pod is deleted, because it pauses the writes (`CLIENT PAUSE ... WRITE`) before it requests the failover. In 3 deletions on kind, no acknowledged write was lost, and the new master was ready after about 1 second.

A custom shutdown script that requests a failover without this pause causes the loss. In a test without the pause, 2 deletions lost 8 seconds and 0.1 seconds of writes through `rfrm-<NAME>`. A custom script that uses `REDIS_PASSWORD` has the old password after a password change, until the pod restarts.

The loss can still occur when Sentinel fails over for another reason, for example when the master does not answer but its pod runs.

To lose fewer writes:

- Use a Sentinel client. It gets the new master from Sentinel, and lost 1 to 6 seconds of writes in the tests, not 11 seconds.
- Send `WAIT 1 <timeout>` after an important write, and treat a result of `0` as a failed write. On the old master, `WAIT` returns `0`, because its replicas replicate from the new master. `WAIT` does not undo the write and does not prevent all loss in a failover. It only lets the client detect this case.
- Set `min-replicas-to-write 1` in `redis.customConfig`. The old master then refuses writes when its replicas disconnect. In a test with plain Redis, this reduced the loss from 11 seconds to 0.9 seconds. The master also refuses writes when no replica is connected.

**Known limitation**: the wait for a master pod that stops finds the pod by its `redisfailovers-role=master` label. The operator sets this label only after it counts exactly one master. Sentinel can promote a pod that already stops. An example is a scale-down from 3 to 1 that removes two pods at the same time (`Parallel` pod management). That pod does not have the label yet, so nothing waits for it.

### Custom SecurityContext

By default, the operator runs the pods as user and group `1000` with `runAsNonRoot: true`, `fsGroup: 1000` and the `RuntimeDefault` seccomp profile.
Set `securityContext` in the `redis` or `sentinel` spec to change the pod security context, for example to run as a specific user.
See the [SecurityContext example file](example/redisfailover/security-context.yaml) and the Kubernetes documentation about [security context](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/).

The operator merges a custom `securityContext` with its defaults. A field that you set has priority, and a field that you do not set keeps its default. For example, a `securityContext` with only `runAsUser` keeps `fsGroup` and `runAsNonRoot`.

### Custom containerSecurityContext at container level

By default, the operator runs the containers as user and group `1000` with `runAsNonRoot: true`, drops `ALL` capabilities, and sets `privileged: false`, `allowPrivilegeEscalation: false` and `readOnlyRootFilesystem: true`.
Set `containerSecurityContext` in the `redis` or `sentinel` spec to change the container security context.
For example, you can add capabilities or allow writes to the root file system.
See the [ContainerSecurityContext example file](example/redisfailover/container-security-context.yaml) and the [available keys](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.20/#securitycontext-v1-core).

The operator merges a custom `containerSecurityContext` with its defaults. A field that you set has priority, and a field that you do not set keeps its default. For example, `allowPrivilegeEscalation: false` stays if you do not set it. A custom `capabilities` replaces the default `capabilities` fully. Add `drop: [ALL]` to it yourself.

### Custom command

By default, the operator starts Redis and Sentinel with this command and the configuration file:

- Redis: `redis-server /redis/redis.conf`
- Sentinel: `redis-server /redis/sentinel.conf --sentinel`

To change the command, set `command` in the `redis` or `sentinel` spec. See the [custom command example file](example/redisfailover/custom-command.yaml).

**Important**: a custom `redis.command` replaces the default command. With `auth.secretPath`, the default command passes `--requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD"` to `redis-server`. The password is not in `redis.conf`, so a custom command must pass these flags itself. Otherwise Redis starts without a password. Kubernetes does not expand `$REDIS_PASSWORD` in a command, so use a shell:

```yaml
command:
  - sh
  - -c
  - exec redis-server /redis/redis.conf --requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD"
```

### Custom environment variables

Use `redis.env` and `sentinel.env` to add environment variables (standard Kubernetes `EnvVar` entries) to the main Redis and Sentinel containers.

The operator adds its own variables `REDIS_ADDR`, `REDIS_PORT` and `REDIS_USER` to the main Redis container, the init containers and the extra containers. It adds `REDIS_PASSWORD` only with `auth.secretPath`. The operator does not add these variables to the Sentinel containers. In the Redis containers, the variables of the operator have priority over a user variable with the same name.

Do not set `REDIS_PASSWORD` in `redis.env`. Without `auth.secretPath`, the shutdown script and the readiness script use this value as the Redis password.

### Custom Priority Class
To use a custom Kubernetes [Priority Class](https://kubernetes.io/docs/concepts/configuration/pod-priority-preemption/#priorityclass) for the Redis or Sentinel pods, set `priorityClassName` in the `redis` or `sentinel` spec. This field has no default. **Note:** the operator does not create the `PriorityClass` resource.

### Custom Service Account
To use a custom Kubernetes [Service Account](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/) for the Redis or Sentinel pods, set `serviceAccountName` in the `redis` or `sentinel` spec. Without it, the Redis pods use the `default` ServiceAccount, and the Sentinel pods use the `rfs-sa-<NAME>` ServiceAccount that the operator creates. **Note:** the operator does not create the `ServiceAccount` resource that you specify.

### Custom Pod Annotations
By default, the Sentinel pods have no annotations. The Redis pods have the `redisfailovers.databases.spotahome.com/secret-checksum` annotation. The operator changes it when the password changes, so that the Redis pods restart.

To add pod annotations, set `podAnnotations` in the `redis` or `sentinel` spec. See the [custom annotations example file](example/redisfailover/custom-annotations.yaml).

### Custom Service Annotations
By default, the `rfr-<NAME>` service has the `prometheus.io/scrape`, `prometheus.io/port` and `prometheus.io/path` annotations. This service exists only when `redis.exporter.enabled` is `true`. The other services have no annotations.

To add service annotations, set `serviceAnnotations` in the `redis` or `sentinel` spec. See the [custom annotations example file](example/redisfailover/custom-annotations.yaml).

### Control of label propagation
By default, the operator copies all labels of the RedisFailover to the resources that it creates.
These labels are not always under your control, for example when a GitOps tool manages them.
A change of these labels can fail on immutable resources such as PodDisruptionBudgets.
To select the labels that the operator copies, set the `labelWhitelist` option in the spec.

Without `labelWhitelist`, or with an empty list, the operator copies all labels.

Each item in the list is a regular expression. See the [label propagation example](example/redisfailover/control-label-propagation.yaml) and the [syntax reference](https://github.com/google/re2/wiki/Syntax).

To copy no labels, use a regular expression that matches no label.

NOTE: The operator always adds the labels that it needs to the resources. These are the labels:
```
app.kubernetes.io/component
app.kubernetes.io/managed-by
app.kubernetes.io/name
app.kubernetes.io/part-of
redisfailovers-role
redisfailovers.databases.spotahome.com/name
```


### ExtraVolumes and ExtraVolumeMounts

Use `extraVolumes` and `extraVolumeMounts` in `spec.redis` and `spec.sentinel` to mount more volumes, for example with configuration files or Secrets. Typical uses are:
- Secrets for a sidecar that backs up the RDB files
- Users with their Secrets and ACLs, for an init container that creates more users
- Configuration files that merge with the Redis configuration
- Failover scripts for more operations

```
---
apiVersion: v1
kind: Secret
metadata:
  name: foo
  namespace: exm
type: Opaque
stringData:
  password: MWYyZDFlMmU2N2Rm
---
apiVersion: databases.spotahome.com/v1
kind: RedisFailover
metadata:
  name: foo
  namespace: exm
spec:
  sentinel:
    enabled: true
    replicas: 3
    extraVolumes:
    - name: foo
      secret:
        secretName: foo
        optional: false
    extraVolumeMounts:
    - name: foo
      mountPath: "/etc/foo"
      readOnly: true
  redis:
    replicas: 3
    extraVolumes:
    - name: foo
      secret:
        secretName: foo
        optional: false
    extraVolumeMounts:
    - name: foo
      mountPath: "/etc/foo"
      readOnly: true
```



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

To enable authentication:

1. Create a Secret with a `password` key:

   ```
   echo -n "pass" > password
   kubectl create secret generic redis-auth --from-file=password
   ```

2. Set `auth.secretPath` to the name of the Secret:

   ```yaml
   apiVersion: databases.spotahome.com/v1
   kind: RedisFailover
   metadata:
     name: redisfailover
   spec:
     sentinel:
       enabled: true
       replicas: 3
     redis:
       replicas: 1
     auth:
       secretPath: redis-auth
   ```

It is safe to change the `password` key of that Secret in place, to add `auth.secretPath`, or to remove it. The operator first sets the new password on each Redis with `CONFIG SET` and on each Sentinel with `SENTINEL SET`, so replication continues. Then it restarts the Redis pods one at a time. New connections must use the new password immediately.

By default, the operator finds a change at its next sync (`--sync-interval`, 30s by default). The chart value `watchAuthSecrets: true` (flag `--watch-auth-secrets`) applies a change immediately. It is off by default because it needs `list` and `watch` on secrets in all namespaces, which can read every Secret. With `watchAuthSecrets: true`, the chart grants these verbs. With kustomize, the kubectl manifests or your own ClusterRole, add `list` and `watch` on `secrets` yourself. If the operator cannot list the Secrets, it only logs the error and finds a change at its next sync.

Until a pod restarts, each program that reads the password from the environment of the pod has the old password. The exporter sidecar cannot authenticate, the pre-stop `SAVE` fails, and custom probes that use `$REDIS_PASSWORD` also fail.

Until a pod restarts, the default readiness probe also reports the pod Ready without a replication check. The probe uses the old password, which Redis refuses. A refused password says nothing about replication, so the probe does not fail the pod for it. During this time, `rfrs-<NAME>` can send reads to a replica that is not synced.

The operator knows the old password only from memory. If it restarted between the change and its next check, it cannot switch the pods. The RedisFailover then reports `unable to apply the configured password`. To recover:

1. Put the previous password back in the Secret.
2. Wait until the RedisFailover is healthy.
3. Change the password again.

The operator reads the password from the Secret and gives it to `redis-server` with `--requirepass` and `--masterauth` from an environment variable.
The password is **not** in the Redis ConfigMap, so it is never visible as plain text in a ConfigMap or in the pod spec.
A custom `redis.command` must pass these flags itself.

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

The [defaults file](api/redisfailover/v1/defaults.go) contains the image versions that the operator deploys.

### Migrate to Valkey

Valkey images ship `redis-server` and `redis-cli`, so a change of `redis.image` and `sentinel.image` to a Valkey image is an ordinary rolling update.

Migrate from Redis 7.2 only. Valkey forked from Redis 7.2 and cannot load the data of Redis 7.4 or later (`Can't handle RDB format version 12`). The first replica on Valkey then never syncs, so the operator does not replace the master. The RedisFailover continues on Redis, but the rollout never completes. To stop the migration, set `redis.image` back to the Redis image. The operator then replaces the unsynced Valkey replica, because that replica has no data to lose.

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

For more information, see the [docs folder](docs).
