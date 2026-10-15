# redis-operator

This is a fork of the `spotahome/redis-operator` repository.

[![Build Status](https://github.com/Saremox/redis-operator/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/Saremox/redis-operator)
[![Go Report Card](https://goreportcard.com/badge/github.com/Saremox/redis-operator)](https://goreportcard.com/report/github.com/Saremox/redis-operator)
[![codecov](https://codecov.io/gh/Saremox/redis-operator/branch/main/graph/badge.svg)](https://codecov.io/gh/Saremox/redis-operator)

The operator creates, configures and manages Redis with automatic failover on Kubernetes. Each Redis deployment is a `RedisFailover` resource.

## Requirements

- Kubernetes version: 1.32 or higher
- Redis version: 6 or higher

CI runs the integration tests on Kubernetes 1.35.8, 1.36.4 and 1.37.0, and the end-to-end test on Kubernetes 1.32.0. Both use Redis 7.2. The soak test runs Redis 6.2, 7.2, 7.4 and 8 and Valkey 7.2, 8 and 9. See [Tested server versions and upgrade paths](#tested-server-versions-and-upgrade-paths).

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

The chart can also apply its CRD before each install and upgrade. Set `crds.upgradeHook.enabled=true`. A Helm hook Job then runs `kubectl apply` with its own ServiceAccount, which can change the RedisFailover CRD. The hook is off by default because of this permission.

In the charts of 4.2.0-rc2, 4.1.2 and older, a namespace with the Pod Security level `restricted` rejects the hook pod. With `imageCredentials.create: true`, the hook pod of these charts can refer to a pull Secret that does not exist. In the charts of 4.2.0-rc1 and 4.1.0, the CRD does not fit in the ConfigMap of the hook. Use the hook only with a later release.

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

### Operator flags

With the Helm chart, set the flags in `image.cli_args`. The chart sets `--listen-address` from `container.port`, and `--watch-auth-secrets` from `watchAuthSecrets`. Do not set `--listen-address` in `image.cli_args`, because the probes use `container.port`. With kubectl or kustomize, add the flags to the `args` of the operator container.

| Flag | Default | Description |
|---|---|---|
| `--concurrency` | `3` | The number of workers that reconcile RedisFailovers in parallel. A value of 0 or less gives 3. |
| `--development` | `false` | Use the file in `--kubeconfig`, not the in-cluster configuration. With this flag, the operator can run outside a cluster. |
| `--disconnect-clients-on-demotion` | `true` | Close the `normal` and `pubsub` client connections of a master when it becomes a replica. See [Usage](#usage). |
| `--enable-pprof` | `false` | Serve the Go profiler. See [Operator metrics](#operator-metrics). |
| `--k8s-cli-burstable-limit` | `100` | The number of requests that the Kubernetes client can send at once without a delay. After such a burst, the client sends at the rate of `--k8s-cli-qps-limit`. With `--k8s-cli-qps-limit` above 0, a value of 0 or less stops the operator at start. |
| `--k8s-cli-qps-limit` | `100` | The maximum mean number of requests in each second that the Kubernetes client sends. The client delays the requests above this rate. A value of 0 gives 5 requests in each second for each API group. A negative value disables the limit. |
| `--kubeconfig` | `$HOME/.kube/config` | The kubeconfig file. The operator reads it only with `--development`. |
| `--listen-address` | `:9710` | The address of the HTTP server for the metrics. |
| `--log-level` | `info` | The log level: `panic`, `fatal`, `error`, `warn`, `info`, `debug` or `trace`. The operator ignores the case and also accepts `warning`. With another value, the operator stops at start with exit code 1. |
| `--metrics-path` | `/metrics` | The HTTP path of the metrics. |
| `--supported-namespaces-regex` | `.*` | Reconcile only the RedisFailovers in a namespace that matches this regular expression. The expression can match a part of the name, so use `^` and `$` for an exact name. The operator still lists and watches the RedisFailovers in all namespaces. A regular expression that is not valid stops the operator at start. |
| `--sync-interval` | `30` | The seconds between two resyncs of all RedisFailovers. A value of 0 or less gives 180. A change of a RedisFailover or of its pods starts a reconcile immediately. Above 300, the metrics cleanup can delete series between two resyncs. |
| `--watch-auth-secrets` | `false` | Apply a change of the Secret in `auth.secretPath` immediately, not at the next resync. It needs `list` and `watch` on all Secrets. See [Redis authentication](#redis-authentication). |

### Operator metrics

The operator serves Prometheus metrics on `--listen-address` (default `:9710`) at `--metrics-path` (default `/metrics`). The flag `--enable-pprof` (default `false`) serves the Go profiler at `/debug/pprof/` on the same address. The profiler has no authentication. Allow only trusted users to reach this address.

### Security notes

The author of a RedisFailover controls the pods that the operator creates: containers, volumes, `hostPath`, `hostNetwork` and service account. Enforce the Pod Security Admission level `restricted`, or an admission policy, in each namespace where users create a RedisFailover.

A RedisFailover in the operator namespace can use the service account of the operator. Install the operator in its own namespace. With Helm, use the options `--namespace` and `--create-namespace`.

Redis and Sentinel do not use TLS. Sentinel has no password. Redis has no password without `auth.secretPath`. Use a NetworkPolicy to limit access to the Redis and Sentinel ports. Allow only your monitoring system to reach the exporter port.

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
**IMPORTANT**: two RedisFailovers in one namespace must not generate the same object name. RedisFailover `X` collides with RedisFailover `s-X` and with RedisFailover `readiness-X`. For example, RedisFailover `foo` generates the shutdown script configmap `rfr-s-foo`, and RedisFailover `s-foo` generates the Redis configmap `rfr-s-foo`. The operator does not change an object that another RedisFailover controls. The RedisFailover that does not control the object does not reconcile, and `status.message` shows the error.

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

`maxmemory` is `percent` (default `75`) of the limit, but at least 32Mi of the limit stays free. The free part must also hold the copy-on-write memory of a fork (`BGSAVE`, full sync). A high `percent` can get a pod OOM-killed under writes. `policy` defaults to `noeviction`.

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

### Pod updates with one Redis pod

With two or more Redis pods, the master role moves to a replica before the master pod stops, except in the cases below. In operator-managed mode, the operator sends `FAILOVER`. With Sentinel, the default shutdown script asks Sentinel for a failover. With `redis.replicas: 1`, no other pod can take the role. Thus a pod replacement, for example after a change of `redis.image`, stops the writes until the new pod is master and Ready. An in-place resize does not replace the pod.

In operator-managed mode, the operator deletes the master pod without a handover when `FAILOVER` is missing. `FAILOVER` is missing in Redis before 6.2 and after a rename of the command. The operator also deletes the master pod without a handover after 3 refusals of `FAILOVER` in a row, for example from an ACL. With Sentinel, the shutdown script asks for no failover when the pause of the writes fails (see [Custom shutdown script](#custom-shutdown-script)). The pause fails on Redis before 6.2 and after a password change. In these cases, the writes fail until a new master takes the role.

The master service `rfrm-<NAME>` sends traffic only to a Ready pod. The default readiness probe of Redis starts 30 seconds after the container starts. Thus, with one pod, the probe delay can make the write outage up to 30 seconds longer. The delay does not protect the data. The probe script `ready.sh` reports a master Ready at once, and a replica only after its first sync with the master.

To shorten the outage, set `redis.customReadinessProbe` with a smaller `initialDelaySeconds`. Keep the command and the other values of the default probe:

```yaml
spec:
  redis:
    replicas: 1
    customReadinessProbe:
      initialDelaySeconds: 5
      timeoutSeconds: 5
      exec:
        command:
          - /bin/sh
          - /redis-readiness/ready.sh
```

Until the operator makes the new pod master, the probe fails. After the promotion, the next probe reports the pod Ready. Kubernetes runs the probe every `periodSeconds` (default 10 seconds).

Do not remove `ready.sh` from the probe. `rfrs-<NAME>` uses the Ready state to send reads only to synced replicas. An election also uses the Ready state to rank the candidates.

With `redis.startupConfigMap` or `redis.customStartupProbe`, the readiness probe starts only after the startup probe succeeds. Then a smaller `initialDelaySeconds` can have no effect.

The probe is part of the pod template. When you set or change `redis.customReadinessProbe`, the operator replaces each Redis pod one time. With one pod, this replacement also stops the writes. Set the probe in the same update as the next image change, so the pod restarts only one time. Without `redis.storage.persistentVolumeClaim`, the new pod starts with no data. See [Persistence](#persistence).

### In-place resize

On Kubernetes 1.33 or later, an update that changes only the container CPU or memory resizes the Redis pods in place. The operator does not recreate them, so Redis does not reload data and the master does not fail over. The operator resizes one pod at a time, replicas first. To lower a memory limit in place, Kubernetes 1.35 is necessary. Set `redis.inPlaceResize: Disabled` to always recreate the pods.

The operator recreates a pod instead of a resize in place in these cases:

- The update changes more than container cpu and memory, adds or removes a request or a limit, or changes the QoS class.
- The kubelet does not support in-place resize, or reports the resize as infeasible.
- The kubelet refuses a memory limit below the current usage. The usage includes the page cache, so a retry also fails and the operator does not wait.
- The Redis process uses more memory (`used_memory_rss`) than the lowered limit minus 32Mi. Redis often does not return the memory that evictions free to the OS.
- The kubelet defers or fails the resize for more than 5 minutes, or does not apply it in 5 minutes.

The operator needs `patch` on `pods/resize` and `get` on `controllerrevisions`, which the chart, the kustomize and the example manifests grant. Without them, the operator recreates the pods. In [mode Auto](#mode-auto) with a limit above the spec, the operator keeps a pod whose revision it cannot read. A recreate would restart Redis for a step.

A ResourceQuota or a LimitRange can reject a resize. The operator then does not recreate the pod, because the same rejection applies to the new pod. The rollout holds, and the status message gives the reason. The rollout continues when the quota or the LimitRange allows the new resources.

### Autoscaling

With `redis.autoscaling`, the operator computes a memory limit for the Redis container from the memory use. Mode `Recommend` shows the limit in the status and changes no pod, StatefulSet, resources or `maxmemory`. Mode `Auto` also applies the limit to the pods in place. See [Mode Auto](#mode-auto) and the example files for [Recommend](example/redisfailover/autoscaling.yaml) and [Auto](example/redisfailover/autoscaling-auto.yaml).

The fields of `redis.autoscaling`:

- `mode`: `Off` (default), `Recommend` or `Auto`. With `bootstrapNode`, validation accepts only `Off`, because the RedisFailover has no master to sample.
- `memoryMax`: the largest limit that the operator recommends. It must not be less than `redis.resources.limits.memory`, which is the lower bound. When the limit is equal to `memoryMax`, the operator recommends no step, and the reason says that the limit is at `memoryMax`.
- `workload`: `Auto` (default), `Store` or `Cache`. `Auto` uses the `maxmemory-policy` of the spec. `allkeys-*` is a cache, and `noeviction` is a store. `volatile-*` is a store until Redis evicts a key.
- `recreateAfter`: the operator does not use it. Validation rejects it with mode `Auto`, because a step never recreates a pod. In the other modes, validation accepts `0s`, or `5m` and more. The value has at most 16 characters, and each number has at most 6 digits. A larger value can overflow and stop the decode of all RedisFailovers.

For a store, the memory request must be equal to the memory limit, or not set. When the request is lower, the node can run out of memory before the pod reaches its limit.

The operator reads `INFO` from the master one time in each minute at most. It compares `used_memory` with `maxmemory`, or with the memory limit when `maxmemory` is `0`. It ignores a sample while the RedisFailover is not `Healthy` and while Redis loads or syncs data. It also ignores the first 10 minutes after a restart. A restart, which changes the `run_id`, starts the count of windows again. When the clock of the operator goes back, the open window starts again.

When `maxmemory` is set, the ratio does not use the memory limit. When `maxmemory` is above the limit, a pod near its limit can show a low ratio. The operator counts an OOM refusal of an ignored sample at the first sample that it does not ignore. A change of the memory limit starts the count of windows again. A lower limit also removes the recommendation, because the operator calculated the step from the higher limit.

A window of 5 minutes is high when its peak is 85 percent or more. The operator recommends a step after 3 consecutive high windows. It recommends a step at once when the use is 95 percent or more, or when Redis refuses a command with an OOM error. A window below 85 percent removes the recommendation. A step is 25 percent of the current limit, rounded up to a multiple of 64Mi, and `memoryMax` at most. For a cache, the operator gives no recommendation.

The rule of 3 windows needs a sample in each window. A window without a sample ends the sequence of high windows. With a `--sync-interval` of 600 seconds or more, a window can end without a sample. Then only the rules for 95 percent and for OOM errors can give a step. With samples more than 11 minutes apart, the operator also discards the state at each sample, and the OOM rule cannot apply. Pod events also start a reconcile, but do not come at regular times.

`status.autoscaling` shows the result. `recommendedMemory` is empty when the operator recommends no change. `highWindows` stops at 3. `evaluatedAt` is the time of the last change of the recommendation or of `highWindows`. Thus a steady instance does not write its status at each window:

```yaml
status:
  autoscaling:
    workload: Store
    recommendedMemory: 1280Mi
    reason: memory use is high in 3 consecutive windows
    evaluatedAt: "2026-10-12T08:15:00Z"
    highWindows: 3
```

To apply a recommendation, set the memory request and limit in `redis.resources` to the recommended value. The operator then resizes the pods as for each other change of the resources.

The operator keeps its state in the ConfigMap `rfa-<NAME>`. In a steady state, it writes the ConfigMap about one time in each window. When the mode changes to `Off`, the operator deletes the ConfigMap, also when a newer operator version wrote the state. After its start, the operator also deletes this ConfigMap of a RedisFailover without autoscaling and without `status.autoscaling`.

The operator discards a state when its last sample is more than 11 minutes before the current time. Thus a state that stays after `Off` gives no recommendation from an earlier time. Kubernetes deletes the ConfigMap together with the RedisFailover.

The operator does not change a ConfigMap `rfa-<NAME>` that another object controls. It does not update a state that a newer operator version wrote, but mode `Off` deletes it. The reason in the status then tells this.

These gauges have the labels `namespace` and `name`:

- `redis_operator_controller_autoscaler_recommended_memory_bytes`: the recommended limit, or `0`.
- `redis_operator_controller_autoscaler_memory_used_ratio`: the use of the last sample.
- `redis_operator_controller_autoscaler_high_windows`: the number of consecutive high windows.
- `redis_operator_controller_autoscaler_effective_memory_bytes`: the memory limit of the redis container. It is the limit of `redis.resources` until the autoscaler applies a larger limit.
- `redis_operator_controller_autoscaler_step_phase`: the phase of the last step: `0` Idle, `1` Applying, `2` Done, `3` Reverted, `4` Held.

The counter `redis_operator_controller_autoscaler_steps_total` has the labels `namespace`, `name` and `result`. It counts the steps that end with the result `done`, `reverted` or `held`.

#### Mode Auto

With `redis.autoscaling.mode: Auto`, the operator applies a recommendation as a step. It resizes the pods in place and never recreates a pod for a step. It only increases the limit. The spec of the RedisFailover stays, and `status.autoscaling.effectiveMemory` shows the limit of the pods when it differs from `redis.resources.limits.memory`.

Validation accepts mode `Auto` only with these settings:

- `redis.maxMemory` is set, and the memory limit is at least 64Mi. A step must raise `maxmemory` with the limit. Otherwise the ratio stays high, and the steps continue up to `memoryMax`.
- `redis.customConfig` does not set `maxmemory`.
- `redis.resources.requests.memory` is equal to the memory limit, or not set.
- `memoryMax` is set and not less than the memory limit.
- `bootstrapNode`, `recreateAfter` and `redis.inPlaceResize: Disabled` are not set.

For a cache, the operator gives no recommendation. Thus mode `Auto` makes no step for the workload `Cache`, or for an `allkeys-*` policy with the workload `Auto`.

The operator also checks these conditions before each step. When one fails, no step starts, and `status.autoscaling.reason` tells why:

- Kubernetes 1.33 or later. In-place resize needs it.
- The operator can get the ControllerRevision of the StatefulSet. Without it, the rollout cannot tell a step from a change of the spec.
- No pod has an annotation of a VerticalPodAutoscaler (`vpaUpdates` or `vpaObservedContainers`). Do not use a VerticalPodAutoscaler for the redis pods.
- The rollout is complete, each replica is in sync, and the RedisFailover is `Healthy`.
- The redis container has no `resizePolicy` with `RestartContainer` for memory, in the StatefulSet and in each pod. Then a resize would restart Redis.

The operator needs `patch` on `pods/resize` and `get` on `controllerrevisions`. The chart, the kustomize and the example manifests grant them. Without `pods/resize`, the first resize fails, and the operator reverts the step.

A step uses the rules of Recommend: 3 consecutive high windows, or at once at 95 percent or after an OOM refusal. A step is 25 percent of the current limit, rounded up to a multiple of 64Mi, and `memoryMax` at most. These limits apply between steps:

- Cooldown: the next step starts 15 minutes after the end of a step at the earliest. After a use of 95 percent or an OOM refusal, the cooldown is 5 minutes.
- Budget: at most 6 steps in 24 hours.
- Back-off: after a revert, the next step waits 1 hour. The wait doubles at each further revert, up to 24 hours. A change of the spec ends the wait.

The operator writes the step to the ConfigMap `rfa-<NAME>` first. The next reconcile writes the larger limit to the StatefulSet. Then the operator resizes the pods in place, one at a time: the replicas first and the master last. The master does not fail over. `maxmemory` follows the smallest pod, so it increases when the master has the larger limit. `status.autoscaling.phase` shows the step:

- `Applying`: the pods get the larger limit.
- `Done`: each pod runs with the larger limit on the update revision.
- `Reverted`: the cluster refused the resize before a pod applied it, for example a ResourceQuota, a LimitRange or a node without enough memory. The operator also reverts a step that no pod applied in 15 minutes, for example because no node can hold the pod. The StatefulSet gets the limit before the step. `retryAfter` shows the end of the back-off.
- `Held`: the cluster refused the resize after a pod applied it. Or the step did not end in 15 minutes, and a revert is not safe. The operator does not lower the limit of a pod, so it keeps the larger limit and starts no further step. The phase changes to `Done` when each pod runs with the larger limit, for example after a pod restart.

A revert is not safe when a pod applied the step, or runs or starts from the template of the step. A missing or terminating pod also blocks a revert, because the StatefulSet can create it from that template. A pod that no node can hold blocks no revert, because it has no data. The operator also holds a step when it cannot remove the resize request of a pod within 30 minutes after the start of the step.

Before the StatefulSet gets the limit before the step, the operator checks the pods again. Then only an existing pod of the step holds the step. A missing pod does not, because a quota can refuse it at the limit of the step.

The reason of `Reverted` and `Held` gives the pod and the refusal. Alert on `redis_operator_controller_autoscaler_step_phase` equal to `4`. In the other phases, the reason also shows a refused resize. An example is a change of the spec while the operator cannot read a revision.

A pod can run with a larger limit than the StatefulSet. This happens when the kubelet applies a resize after a revert, or on Kubernetes 1.33 and 1.34, which cannot lower a limit in place. The operator does not change such a pod. A pod that the StatefulSet created from the template of the last step also keeps its limit. The rollout only moves its revision label. `maxmemory` follows the smallest pod, so the larger pod is safe.

`effectiveMemory` shows the limit of the StatefulSet, so it can be lower than the limit of such a pod. A raise of `memoryMax` keeps this rule. To give such a pod the smaller limit, set `memoryMax` below the limit of the pod, or set mode `Off`. A spec limit at or above the limit of the pod also ends the rule.

A pod that refuses a step stays on its revision, also in mode `Recommend`. The operator does not recreate it in these cases:

- The spec limit is above the limit before the step and below the limit of the step. The template does not change.
- The spec limit is equal to the limit of a step that is `Applying` or `Held`. The step continues. A refusal or the timeout then holds the step, because a revert cannot lower the template.
- The state is lost, not valid, of another owner or of a newer operator. Until the next change of the spec, each raise to the limit of the StatefulSet can be a step.

Changes of the user:

- A spec limit above the effective limit ends the step. The pods get the spec limit as for each change of the resources.
- A spec limit equal to the effective limit does not change the pods. A step that is `Applying` or `Held` continues until each pod runs with that limit.
- A spec limit below the effective limit does not change the pods. The reason then contains `redis.resources.limits.memory is below effectiveMemory`.
- A `memoryMax` below the effective limit lowers the limit of the pods to `memoryMax`. This is a change of the user: Kubernetes 1.35 lowers it in place, and older versions recreate the pods.
- Mode `Recommend` keeps the effective limit, and completes a step that is `Applying` or `Held`. It starts no step. The status shows no phase of an earlier step.
- Mode `Off` gives the pods the spec limit. This lowers the limit as above. To keep the limit, pin it first.
- A ConfigMap `rfa-<NAME>` that is lost or not valid gives the limit of the live StatefulSet. No step starts until the next change of the spec, also when the live limit is the spec limit. After that change, the budget and the back-off start again. The procedure to pin the limit sets the spec limit to the frozen limit. That gives the phase `Held` until each pod runs with that limit. Mode `Recommend` also keeps that limit when `status.autoscaling.effectiveMemory` is set.
- A ConfigMap `rfa-<NAME>` of another owner or of a newer operator version also gives the limit of the live StatefulSet. The operator does not change that ConfigMap and starts no step.
- A state with a last sample more than 11 minutes before the current time gives the limit of the live StatefulSet. A state that stays after mode `Off` thus does not change the pods.

To pin the effective limit, for example before mode `Off` or a downgrade of the operator:

1. Read `status.autoscaling.effectiveMemory`. When it is empty, the pods have the spec limit.
2. Set `redis.resources.requests.memory` and `redis.resources.limits.memory` to that value.
3. When `status.autoscaling.phase` is `Applying` or `Held`, wait until it is `Done`. Mode `Off` recreates a pod that refuses the limit.
4. Set `redis.autoscaling.mode` to `Recommend` or `Off`.
5. Wait until `status.state` is `Healthy`.

An operator version without mode `Auto` rejects such a RedisFailover in validation. It then stops all reconciles of the RedisFailover, also the failover of the operator-managed mode. The 4.1.x CRD removes `redis.autoscaling`, and then the 4.1.x operator gives the pods the spec limit. Thus pin the limit before a downgrade. See the [migration guide](docs/migrations/4.2.0.md).

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

The operator adds its own variables `REDIS_ADDR`, `REDIS_PORT` and `REDIS_USER` to the main Redis container, the init containers and the extra containers. It adds `REDIS_PASSWORD` only with `auth.secretPath`. The operator does not add these variables to the main Sentinel container. In the Redis containers, the variables of the operator have priority over a user variable with the same name.

Do not set `REDIS_PASSWORD` in `redis.env`. Without `auth.secretPath`, the shutdown script and the readiness script use this value as the Redis password.

### Custom Priority Class
To use a custom Kubernetes [Priority Class](https://kubernetes.io/docs/concepts/configuration/pod-priority-preemption/#priorityclass) for the Redis or Sentinel pods, set `priorityClassName` in the `redis` or `sentinel` spec. This field has no default. **Note:** the operator does not create the `PriorityClass` resource.

### Custom Service Account
To use a custom Kubernetes [Service Account](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/) for the Redis or Sentinel pods, set `serviceAccountName` in the `redis` or `sentinel` spec. Without it, the Redis pods use the `default` ServiceAccount, and the Sentinel pods use the `rfs-sa-<NAME>` ServiceAccount that the operator creates. Without it, the pods also do not mount the ServiceAccount token. **Note:** the operator does not create the `ServiceAccount` resource that you specify.

### Custom Pod Annotations
By default, the Sentinel pods have no annotations. The Redis pods have the `redisfailovers.databases.spotahome.com/secret-checksum` annotation. The operator changes it when the password changes, so that the Redis pods restart.

To add pod annotations, set `podAnnotations` in the `redis` or `sentinel` spec. See the [custom annotations example file](example/redisfailover/custom-annotations.yaml).

### Custom Service Annotations
By default, the `rfr-<NAME>` service has the `prometheus.io/scrape`, `prometheus.io/port` and `prometheus.io/path` annotations. This service exists only when `redis.exporter.enabled` is `true`. The other services have no annotations.

To add service annotations, set `serviceAnnotations` in the `redis` or `sentinel` spec. See the [custom annotations example file](example/redisfailover/custom-annotations.yaml).

### Exporter authentication
By default, the exporter ports have no authentication. To allow only clients that know a username and a password, set `REDIS_EXPORTER_BASIC_AUTH_USERNAME` and `REDIS_EXPORTER_BASIC_AUTH_PASSWORD` in `redis.exporter.env` and in `sentinel.exporter.env`. Read both values from a Secret with `secretKeyRef`. The exporter then checks the credentials on all paths. If one value is empty, the exporter does not check credentials.

The `basicAuth` field of a ServiceMonitor sends the credentials. Prometheus Operator and Alloy read the Secret from the namespace of the ServiceMonitor. The `prometheus.io` annotations have no field for credentials. To stop that scrape, set `prometheus.io/scrape: "false"` in `redis.serviceAnnotations`. See the [exporter authentication example file](example/redisfailover/exporter-basic-auth.yaml).

The variables change the pod templates, so the operator replaces the Redis pods, the master last, and rolls the Sentinel pods. A container reads the Secret only when it starts. After you change the Secret, restart the pods that run an exporter. Until then, the exporters use the old password.

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

You can change the `password` key of that Secret in place, add `auth.secretPath`, or remove it. An operator restart during the change needs no manual recovery in most cases when the operator can read and write the Secret `rfr-auth-applied-<NAME>` (see below). The operator first sets the new password on each Redis with `CONFIG SET` and on each Sentinel with `SENTINEL SET`, so replication continues. Then it restarts the Redis pods one at a time. New connections must use the new password immediately. After a removal, Redis refuses `AUTH <password>` with `ERR AUTH <password> called without any password configured`, so clients must not send the password.

After you change or remove `auth.secretPath`, keep the previous Secret until the operator restarts every Redis pod. A pod on the previous pod template reads this Secret when its container restarts. The operator reads it to find the password that such a pod runs with.

By default, the operator finds a change at its next sync (`--sync-interval`, 30s by default). The chart value `watchAuthSecrets: true` (flag `--watch-auth-secrets`) applies a change immediately. It is off by default because it needs `list` and `watch` on secrets in all namespaces, which can read every Secret. With `watchAuthSecrets: true`, the chart grants these verbs. With kustomize, the kubectl manifests or your own ClusterRole, add `list` and `watch` on `secrets` yourself. If the operator cannot list the Secrets, it only logs the error and finds a change at its next sync.

Until a pod restarts, each program that reads the password from the environment of the pod has the old password. The exporter sidecar cannot authenticate, so it reports Redis down. After a change or an addition of the password, the pre-stop `SAVE` fails, and custom probes that use `$REDIS_PASSWORD` also fail. After a removal, `redis-cli` runs the command after the refused `AUTH`, so the pre-stop `SAVE` and probes that use `redis-cli` work.

Until a pod restarts after a change or an addition of the password, the default readiness probe also reports it Ready without a replication check. The probe uses the old password, which Redis refuses. A refused password says nothing about replication, so the probe does not fail the pod for it. During this time, `rfrs-<NAME>` can send reads to a replica that is not synced.

The operator keeps the passwords that the Redis pods run with in the Secret `rfr-auth-applied-<NAME>`. After its start, the operator reads this Secret at the first check of each RedisFailover, also without `auth.secretPath`. After a complete change, the Secret contains only the new password, or empty values after you remove `auth.secretPath`. The RedisFailover owns the Secret, so Kubernetes deletes it with the RedisFailover. Do not change this Secret. The operator does not read or change a Secret with this name that the RedisFailover does not control.

The operator needs `create` and `update` on `secrets` for this Secret. The Helm chart, `manifests/kustomize` and `example/operator` grant these verbs. With these verbs, the operator can change every Secret in the cluster. Without them, the operator logs a warning and keeps the passwords only in memory. If you create a RedisFailover again with the same name before Kubernetes deletes its old Secret, `OwnerReferencesPermissionEnforcement` can refuse the update of the Secret. The operator then logs a warning and writes the Secret at the next password change.

If a Redis pod runs with a password that the operator does not know, the operator cannot switch the pod. This occurs, for example, when the operator cannot read or write `rfr-auth-applied-<NAME>` and restarts between a change and its next check. The RedisFailover then reports `unable to apply the configured password`. Until you recover, the operator does not restart Redis pods and does not start an operator-managed failover. The operator reads the password of a pod on a previous pod template from the Secret that this template names, if that Secret still exists. To recover from a password change:

1. Put the previous password back in the auth Secret (`auth.secretPath`).
2. Wait until the RedisFailover is healthy.
3. Change the password again.

To recover from a removal of `auth.secretPath`:

1. Set `auth.secretPath` to a Secret with the previous password.
2. Wait until the RedisFailover is healthy.
3. Remove `auth.secretPath` again.

The operator reads the password from the Secret and gives it to `redis-server` with `--requirepass` and `--masterauth` from an environment variable.
The password is **not** in the Redis ConfigMap, so it is never visible as plain text in a ConfigMap or in the pod spec. The operator also keeps it in the Secret `rfr-auth-applied-<NAME>`.
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

### Tested server versions and upgrade paths

The soak test runs RedisFailovers with these server images, in [operator-managed mode and in Sentinel mode](docs/logic.md):

| Name | Image |
|---|---|
| Redis 6.2 | `redis:6.2.24-alpine` |
| Redis 7.2 | `redis:7.2.16-alpine` |
| Redis 7.4 | `redis:7.4.11-alpine` |
| Redis 8 | `redis:8.10.2-alpine` |
| Valkey 7.2 | `valkey/valkey:7.2.14-alpine` |
| Valkey 8 | `valkey/valkey:8.1.10-alpine` |
| Valkey 9 | `valkey/valkey:9.1.2-alpine` |

The soak test does not run other versions: Redis 6.0 and 7.0, other patch releases of Redis 7.4 and 8, and Valkey 8.0.

An in-place upgrade is a change of `spec.redis.image` on a RedisFailover that runs. The soak test made 3,775 such changes in a window of about 3.5 days. Each instance had up to 3 Redis pods, a 16 MiB data set and a write ledger. The ledger checks each acknowledged write. The Redis 6.2 row comes from a separate window of about 17.5 hours on two extra instances. All other rows come from the 3.5-day window.

| From | To | Result | Count (operator-managed / Sentinel mode) |
|---|---|---|---|
| Redis 6.2 | Redis 7.2 | works | 43 / 52 |
| Redis 7.2 | Redis 7.4 | works | 284 / 348 |
| Redis 7.4 | Redis 8 | works | 283 / 347 |
| Valkey 7.2 | Valkey 8 | works | 128 / 154 |
| Valkey 8 | Valkey 9 | works | 233 / 286 |
| Redis 7.2 | Valkey 7.2 | works | 128 / 151 |
| Redis 7.2 | Valkey 8 | works | 105 / 131 |
| Redis 7.2 | Valkey 9 | works | 120 / 104 |
| Redis 7.4 | Valkey 7.2 / 8 / 9 | fails safe | 171 / 162 / 154 (operator-managed mode only) |
| Redis 8 | Valkey 7.2 / 8 / 9 | fails safe | 155 / 171 / 160 (operator-managed mode only) |

"Works" means that the upgrade converged: one master, all pods Ready on the new version, status `Healthy`. All 2,802 upgrades with this result in the 3.5-day window converged, and the ledger found no lost acknowledged write.

"Fails safe" means that the rollout stops after the first replica. The new Valkey pod cannot load the data of the old master and stays not Ready. Its log shows `Can't handle RDB format version 12` (data from Redis 7.4) or `Can't handle RDB format version 15` (data from Redis 8). The master and the other replicas keep the old version and accept writes. In all 973 cases, the master stayed writable and the ledger found no lost acknowledged write.

Valkey forked from Redis 7.2.4, so it reads Redis 7.2 data and not the data format of Redis 7.4 or Redis 8. There is no in-place path from Redis 7.4 or Redis 8 to Valkey. [Migrate to Valkey](#migrate-to-valkey) tells how to stop such a rollout.

Two extra instances, one per mode, ran the chain Redis 6.2, 7.2, 7.4 and 8 and then reset. In about 17.5 hours they made 283 upgrades (operator-managed / Sentinel mode):

- Redis 6.2 to Redis 7.2: 43 / 52. Redis 7.2 loads the Redis 6.2 data format, so this step is an ordinary rolling update.
- Redis 7.2 to Redis 7.4: 43 / 52.
- Redis 7.4 to Redis 8: 42 / 51.

All 283 converged, and the ledger found no lost acknowledged write. Only the first step is in the table above. On this chain, an upgrade took 131 s on average (95th percentile 176 s) in operator-managed mode and 166 s (258 s) in Sentinel mode.

In the 3.5-day window, the soak test did not test these changes of `spec.redis.image`:

- Downgrades. [In-place downgrades](#in-place-downgrades) gives the results of a separate window.
- Skipped steps, such as Redis 6.2 to Redis 7.4, Redis 7.2 to Redis 8 or Valkey 7.2 to Valkey 9.
- Redis 6.2 to Valkey.
- Redis 7.4 or Redis 8 to Valkey in Sentinel mode.
- Valkey to Redis.

In Sentinel mode, the Sentinel image changes separately or follows the data image. The soak test ran these cases:

- Data image first, then Sentinel image, from Redis 7.2 through Valkey 7.2, 8 and 9: 447 Sentinel image upgrades, all converged.
- Sentinels on Valkey 9 with Redis 7.2 data, with the Sentinel image flipped between Valkey 9 and Redis 7.2: 320 flips, all converged.
- The Sentinel image follows the data image on the chain Redis 7.2, 7.4 and 8: converged.

The durations are relative figures from a test cluster with nested VMs and local volumes. A data image upgrade took 144 s on average (95th percentile 177 s) in operator-managed mode and 162 s (241 s) in Sentinel mode. Pods of two versions ran side by side for about 90 s.

In the 3.5-day window, the soak test did not cover:

- Node loss, network partitions and a full disk during an upgrade.
- Operator-to-operator upgrades.
- Data sets larger than 16 MiB on the upgrade instances, and more than 5 Redis pods.
- TLS, which the operator does not support. See [Security notes](#security-notes).
- Recovery from a fails-safe Redis-to-Valkey change by a revert of the image. The soak test deletes and recreates the instance instead, which loses the data by design. [Migrate to Valkey](#migrate-to-valkey) describes the revert, and the soak test did not test it.

#### In-place downgrades

An in-place downgrade is a change of `spec.redis.image` to an older server version. The soak test ran downgrades in a separate window of 24 hours, on one instance in operator-managed mode. The instance had 3 Redis pods, a 16 MiB data set and a write ledger.

| From | To | Result | Count (operator-managed mode) |
|---|---|---|---|
| Redis 7.4 | Redis 7.2 | fails safe | 46 |
| Redis 8 | Redis 7.4 | fails safe | 25 |
| Redis 8 | Redis 7.2 | fails safe | 20 |
| Valkey 8 | Valkey 7.2 | works | 70 |
| Valkey 9 | Valkey 8 | works | 23 |
| Valkey 9 | Valkey 7.2 | works | 22 |

"Works" means that the downgrade converged: one master, all pods Ready on the older version, status `Healthy`. A downgrade that fails safe stops the rollout after the first replica. The pod on the older version cannot read the data format of the newer master and stays not Ready. The master and the other replicas keep their version and accept writes. The ledger found no lost acknowledged write in the 206 downgrades.

To stop a downgrade that fails safe, restore the previous `redis.image`. The operator then replaces the unsynced replica, as [Migrate to Valkey](#migrate-to-valkey) describes. The soak test resets the instance after a downgrade that fails safe, so it did not test this revert.

The soak test did not test these downgrades:

- Downgrades to Redis 6.2.
- Downgrades in Sentinel mode.

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
