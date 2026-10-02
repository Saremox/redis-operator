# redis-operator

This is a fork of the `spotahome/redis-operator` repository.

[![Build Status](https://github.com/Saremox/redis-operator/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/Saremox/redis-operator)
[![Go Report Card](https://goreportcard.com/badge/github.com/Saremox/redis-operator)](https://goreportcard.com/report/github.com/Saremox/redis-operator)
[![codecov](https://codecov.io/gh/Saremox/redis-operator/branch/main/graph/badge.svg)](https://codecov.io/gh/Saremox/redis-operator)

Redis Operator creates/configures/manages redis-failovers atop Kubernetes.

## Requirements

Kubernetes version: 1.32 or higher
Redis version: 6 or higher

CI runs the integration tests on Kubernetes 1.35.8, 1.36.4 and 1.37.0, and the end-to-end test on Kubernetes 1.32.0. Both use Redis 7.2.

## Operator deployment on Kubernetes

To create Redis failovers inside a Kubernetes cluster, the operator has to be deployed.
It can be done with plain old [deployment](example/operator), using [Kustomize](manifests/kustomize) or with the provided [Helm chart](charts/redisoperator).

### Using the Helm chart

From the root folder of the project, execute the following:

```
helm repo add redis-operator https://Saremox.github.io/redis-operator
helm repo update
helm install redis-operator redis-operator/redis-operator
```

Alternatively, the chart is also published as an OCI artifact alongside the
operator image, so it can be installed without adding a repo:

```
helm install redis-operator oci://ghcr.io/saremox/redis-operator/charts/redis-operator --version <release-tag>
```

Every release tag (e.g. `4.0.0`) publishes the operator image and the Helm
chart together with the same version, so the two can never drift apart.
Release candidates (e.g. `4.0.0-rc1`) are published the same way as GitHub
pre-releases; Helm skips them by default unless you pass `--version` or `--devel`.

#### Update helm chart

Read the [release notes](https://github.com/Saremox/redis-operator/releases) before an upgrade. If a release needs manual steps, its notes start with a warning that links to a guide in [docs/migrations](docs/migrations).

Helm chart only manages the creation of CRD in the first installation. To update the CRD, you will need to apply it directly.

```
REDIS_OPERATOR_VERSION=<release-tag>
kubectl replace -f https://raw.githubusercontent.com/Saremox/redis-operator/${REDIS_OPERATOR_VERSION}/manifests/databases.spotahome.com_redisfailovers.yaml
```

The chart can also apply its CRD before each install and upgrade. Set `crds.upgradeHook.enabled=true`. A Helm hook Job then runs `kubectl apply` with its own ServiceAccount, which can change the RedisFailover CRD. The hook is off by default because of this permission. The charts of 4.2.0-rc2 and earlier run the hook with the operator image, which has no `kubectl`. Use the hook only with a later release.

```
helm upgrade redis-operator redis-operator/redis-operator
```

The CRD enables the `status` subresource, so status updates no longer bump an RF's `metadata.generation`. When upgrading from a release without it, update the CRD and the operator together (with Helm, kubectl or kustomize): an older operator's status writes are dropped against the new CRD, and the new operator's status writes fail against the old one. Existing RFs keep their status.
### Using kubectl

To create the operator, you can directly create it with kubectl:

```
REDIS_OPERATOR_VERSION=<release-tag>
kubectl create -f https://raw.githubusercontent.com/Saremox/redis-operator/${REDIS_OPERATOR_VERSION}/manifests/databases.spotahome.com_redisfailovers.yaml
kubectl apply -f https://raw.githubusercontent.com/Saremox/redis-operator/${REDIS_OPERATOR_VERSION}/example/operator/all-redis-operator-resources.yaml
```

This will create a deployment named `redisoperator`.

The manifests at a release tag deploy the operator image of that tag. The release workflow stops a release when they do not agree. In tags 4.2.0-rc2, 4.1.2 and older, the manifests deploy an older operator image. For these tags, set the image after the install:

```
kubectl set image deployment/redisoperator app=ghcr.io/saremox/redis-operator:${REDIS_OPERATOR_VERSION}
```

The manifest also contains a `ServiceMonitor` and a `PodMonitor`. Without the Prometheus Operator CRDs, `kubectl apply` reports an error for these two resources. It still creates the other resources.

### Using kustomize

The kustomize setup included in this repo is highly customizable using [components](https://kubectl.docs.kubernetes.io/guides/config_management/components/),
but it also comes with a few presets (in the form of overlays) supporting the most common use cases.

To install the operator with default settings and every necessary resource (including RBAC, service account, default resource limits, etc.), install the `default` overlay:

```shell
kustomize build github.com/Saremox/redis-operator/manifests/kustomize/overlays/default
```

The `minimal` overlay is the `default` overlay without the resource limits. It also creates the RBAC and the service account. To use your own RBAC or service account, use the `base` and the [components](manifests/kustomize/components) in your own kustomization.

Finally, you can install the `full` overlay if you want everything this operator has to offer, including Prometheus ServiceMonitor resources.

It's always a good practice to pin the version of the operator in your configuration to make sure you are not surprised by changes on the latest development branch:

```shell
kustomize build github.com/Saremox/redis-operator/manifests/kustomize/overlays/default?ref=<release-tag>
```

The `?ref=<release-tag>` also pins the operator image. In tags 4.2.0-rc2, 4.1.2 and older, the overlays deploy operator `v1.4.0`. For these tags, add the image to your `kustomization.yaml`:

```yaml
images:
  - name: ghcr.io/saremox/redis-operator
    newTag: <release-tag>
```

You can create your own config by creating a `kustomization.yaml` file
(for example, to apply custom resource limits, to add custom labels or to customize the namespace):

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

Take a look at the manifests inside [manifests/kustomize](manifests/kustomize) for more details.

## Usage

Once the operator is deployed inside a Kubernetes cluster, a new API will be accessible, so you'll be able to create, update and delete redisfailovers.

To deploy a new redis-failover, a [specification](example/redisfailover/basic.yaml) has to be created:

```
REDIS_OPERATOR_VERSION=<release-tag>
kubectl create -f https://raw.githubusercontent.com/Saremox/redis-operator/${REDIS_OPERATOR_VERSION}/example/redisfailover/basic.yaml
```

Starting with `4.0.0`, Sentinel is disabled by default and failover is managed by the operator. Set `spec.sentinel.enabled: true` to deploy Sentinel resources, or use `spec.sentinel.failoverTimeout` to tune operator-managed failover.

This redis-failover will be managed by the operator, resulting in the following elements created inside Kubernetes:

- `rfr-<NAME>`: Redis configmap
- `rfr-<NAME>`: Redis statefulset
- `rfr-<NAME>`: Redis service (if redis-exporter is enabled)
- `rfrm-<NAME>`: Redis master service
- `rfrs-<NAME>`: Redis slave service
- `rfr-s-<NAME>`: Redis shutdown script configmap (if `redis.shutdownConfigMap` is not set)
- `rfr-readiness-<NAME>`: Redis readiness script configmap
- `rfr-<NAME>`: Redis PodDisruptionBudget (if `redis.disablePodDisruptionBudget` is not `true`)
- `rfs-<NAME>`: Sentinel configmap (if Sentinels run, see below)
- `rfs-<NAME>`: Sentinel deployment (if Sentinels run, see below)
- `rfs-<NAME>`: Sentinel service (if Sentinels run, see below)
- `rfs-<NAME>`: Sentinel PodDisruptionBudget (if Sentinels run and `sentinel.disablePodDisruptionBudget` is not `true`)
- `rfs-sa-<NAME>`: Sentinel service account (if Sentinels run and `sentinel.serviceAccountName` is not set)

Sentinels run when `spec.sentinel.enabled` is `true`. With `bootstrapNode`, they also need `bootstrapNode.allowSentinels: true`.

**NOTE**: `NAME` is the named provided when creating the RedisFailover.
**IMPORTANT**: the name of the redis-failover to be created cannot be longer than 48 characters, due to prepend of redis/sentinel identification and statefulset limitation.

### Protect the master from cluster-autoscaler eviction

Setting `redis.preventMasterEviction: true` makes the operator annotate the current master pod with
`cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` (and mark slaves `"true"`), so the
cluster-autoscaler will not drain the node running the master and trigger an avoidable failover. The
annotation follows the master as it moves. Defaults to `false` (no annotation is managed).

### Sentinel update strategy and PodDisruptionBudget

The sentinel `Deployment` update strategy can be overridden via `sentinel.strategy` (e.g. to set
`rollingUpdate.maxSurge`/`maxUnavailable`). This helps when required anti-affinity plus
`replicas == nodes` would otherwise deadlock the default rolling update:

```yaml
spec:
  sentinel:
    strategy:
      type: RollingUpdate
      rollingUpdate:
        maxSurge: 1
        maxUnavailable: 0
```

The `PodDisruptionBudget` `minAvailable` for each component defaults to `2` (or `1` when that
component's `replicas <= 2`). Override it per component with `redis.podDisruptionBudgetMinAvailable` /
`sentinel.podDisruptionBudgetMinAvailable` (an integer or percentage string such as `"60%"`).

### Persistence

The operator can add persistence to Redis data. By default, an `emptyDir` will be used, so the data is not saved.

To have persistence, a `PersistentVolumeClaim` usage is allowed. The full [PVC definition has to be added](example/redisfailover/persistent-storage.yaml) to the Redis Failover Spec under the `Storage` section.

**IMPORTANT**: By default, the persistent volume claims will be deleted when the Redis Failover is. If this is not the expected usage, a `keepAfterDeletion` flag can be added under the `storage` section of Redis. [An example is given](example/redisfailover/persistent-storage-no-pvc-deletion.yaml).

### NodeAffinity and Tolerations

You can use NodeAffinity and Tolerations to deploy Pods to isolated groups of Nodes. Examples are given for [node affinity](example/redisfailover/node-affinity.yaml), [pod anti-affinity](example/redisfailover/pod-anti-affinity.yaml) and [tolerations](example/redisfailover/tolerations.yaml).

## Topology Spread Constraints

You can use the `topologySpreadConstraints` to ensure the pods of a type(redis or sentinel) are evenly distributed across zones/nodes. Examples are for using [topology spread constraints](example/redisfailover/topology-spread-contraints.yaml). Further document on how `topologySpreadConstraints` work could be found [here](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/).

### Custom configurations

It is possible to configure both Redis and Sentinel. This is done with the `customConfig` option inside their spec. It is a list of configurations and their values. This example is given in the [custom config example file](example/redisfailover/custom-config.yaml).

To have the ability of this configuration to be changed "on the fly," without the need of reload the redis/sentinel processes, the operator will apply them with calls to the redises/sentinels, using `config set` or `sentinel set mymaster` respectively. Because of this, **no changes on the configmaps** will appear regarding this custom configuration and the entries of `customConfig` from Redis spec will not be written on `redis.conf` file. To verify the actual Redis configuration use [`redis-cli CONFIG GET *`](https://redis.io/commands/config-get).

**Important**: in the Sentinel options, there are some "conversions" to be made:

- Configuration on the `sentinel.conf`: `sentinel down-after-milliseconds mymaster 2000`
- Configuration on the `customConfig`: `down-after-milliseconds 2000`

**Important 2**: do **NOT** change the options used for control the redis/sentinel such as `port`, `bind`, `dir`, etc.

### Managed maxmemory

With `redis.maxMemory` the operator sets `maxmemory` and `maxmemory-policy` from the redis container's memory limit, see the [maxmemory example file](example/redisfailover/maxmemory.yaml). It requires a memory limit of at least 64Mi; otherwise `maxmemory` is not managed and the reason is in the status message.

`maxmemory` is `percent` (default `75`) of the limit, keeping at least 32Mi free. `policy` defaults to `noeviction`.

| Limit | maxmemory |
|---|---|
| 64Mi | 32Mi |
| 96Mi | 64Mi |
| 128Mi | 96Mi |
| 1Gi | 768Mi |

Keys set in `customConfig` take precedence; `replica-ignore-maxmemory no` is rejected, as replicas would evict on their own, and running pods are set to `yes`. To migrate, update the CRD and the operator, add `maxMemory`, then remove `maxmemory` and `maxmemory-policy` from `customConfig`. Removing `maxMemory` leaves the running pods at their current values until they are recreated, which an in-place resize does not do; set them in `customConfig` to keep them.

`maxmemory` follows the smallest redis pod, as replicas hold the whole dataset and any of them can be promoted: a raised limit applies once every pod runs with it, a lowered one before the pods are replaced. `maxmemory` is only lowered below the memory in use under an `allkeys-*` policy, as `volatile-*` could evict every key with a TTL and still not fit; otherwise it is kept and the reason is in the status message. Until the data fits, the operator does not replace pods with the smaller limit. Pods recreated for other reasons, e.g. a node drain, get the smaller limit anyway.

For small instances, the default `client-output-buffer-limit` for `pubsub` (32mb) and `replica` (256mb) can exceed the free part of the limit; lower them with `customConfig`. Replicas buffer a whole `MULTI`/`EXEC` or `EVAL` before applying it, so one large batch can get a replica OOM-killed.

### Pod updates

A changed spec replaces the redis pods one at a time, replicas first and the master last, each once the previous one is ready and every replica is synced with the master. The operator replaces an unsynced replica that is not on the current spec first, because that replica has no data to lose. When the rollout waits on the same pod for more than 10 minutes, e.g. on a new image that can't load the master's data, the status message names the pod and the reason, e.g. `rollout waiting on pod rfr-<NAME>-1 for more than 10m: not synced with the master`. The state stays `Healthy`, as the master still serves, and the message clears once the rollout moves on.

### In-place resize

On Kubernetes 1.33 or later, an update that only changes container cpu or memory resizes the redis pods in place instead of recreating them, so no data is reloaded and the master does not fail over. Pods are resized one at a time, replicas first. Lowering a memory limit in place needs Kubernetes 1.35. Set `redis.inPlaceResize: Disabled` to always recreate the pods.

The operator recreates a pod instead of a resize in place in these cases:

- The update changes more than container cpu and memory, adds or removes a request or a limit, or changes the QoS class.
- The kubelet does not support in-place resize, or reports the resize as infeasible.
- The kubelet refuses a memory limit below the current usage. The usage includes the page cache, so a retry also fails and the operator does not wait.
- The kubelet defers or fails the resize for more than 5 minutes, or does not apply it in 5 minutes.

The operator needs `patch` on `pods/resize` and `get` on `controllerrevisions`, which the chart, the kustomize and the example manifests grant. Without them, the operator recreates the pods.

### Custom shutdown script

By default, the operator gives each redis pod a shutdown script. The script makes redis `SAVE` its data before it stops. When Sentinel runs and the pod is the master, the script first asks Sentinel to fail over. Thus Sentinel moves the master immediately and does not wait for `down-after-milliseconds`. The redis pods have no service links, so the script finds Sentinel through the Service name `rfs-<NAME>` on port 26379.

During the failover, the script pauses the writes on the old master, and then makes it a replica of the new master. This prevents the loss of writes that the old master acknowledged, because the clients get `READONLY` instead. The pause needs Redis 6.2 or later. On an earlier version, the script continues without the pause. The script waits a maximum of 12 seconds for the new master. Thus the script ends inside the default 30-second grace period, and the `SAVE` can run.

This behavior is configurable, creating a configmap and indicating to use it. An example about how to use this option can be found in the [shutdown example file](example/redisfailover/custom-shutdown.yaml).

**Important**: the configmap has to be in the same namespace. The configmap has to have a `shutdown.sh` data, containing the script.

### Sentinel failover and write loss

Sentinel does not stop writes on the old master in a failover. If the old master still runs, it accepts writes until Sentinel makes it a replica. With the default timings, this occurs about 10 seconds after the promotion. The old master then copies the data of the new master, and the writes that it acknowledged in that interval are lost. The interval ends earlier if the operator moves the master label or makes the old master a replica first. In a test on kind, a `SENTINEL FAILOVER` lost 11 seconds of writes through `rfrm-<NAME>`.

The default shutdown script prevents this loss when the master pod is deleted. Before it requests the failover, it pauses the writes on the master (`CLIENT PAUSE ... WRITE`). After the promotion, it makes the old master a replica of the new master. Thus clients get an error, not an acknowledgement that is lost. In 3 deletions on kind, no acknowledged write was lost, and the new master was ready after about 1 second. The pause needs Redis 6.2 or later. On an older Redis, the script requests the failover without the pause.

A custom shutdown script that requests a failover without this pause causes the loss. In a test without the pause, 2 deletions lost 8 seconds and 0.1 seconds of writes through `rfrm-<NAME>`.

The loss can still occur when Sentinel fails over for another reason, for example when the master stops to answer but its pod continues to run.

To lose fewer writes:

- Use a Sentinel client. It gets the new master from Sentinel, and lost 1 to 6 seconds of writes in the tests, not 11 seconds.
- Send `WAIT 1 <timeout>` after an important write, and treat a result of `0` as a failed write. On the old master, `WAIT` returns `0`, because its replicas replicate from the new master. `WAIT` does not undo the write and does not prevent all loss in a failover. It only lets the client detect this case.
- Set `min-replicas-to-write 1` in `redis.customConfig`. The old master then refuses writes when its replicas disconnect. In a test with plain Redis, this reduced the loss from 11 seconds to 0.9 seconds. The master also refuses writes when no replica is connected.

**Known limitation**: the wait for a master pod that stops finds the pod by its `redisfailovers-role=master` label. The operator sets this label only after it counts exactly one master. Sentinel can promote a pod that already stops. An example is a scale-down from 3 to 1 that removes two pods at the same time (`Parallel` pod management). That pod does not have the label yet, so nothing waits for it.

### Custom SecurityContext

By default, the operator runs the pods as user and group `1000` with `runAsNonRoot: true`, `fsGroup: 1000` and the `RuntimeDefault` seccomp profile.
If you need the containers to run as a specific user (or provide any other PodSecurityContext options), then you can specify a custom `securityContext` in the
`redisfailover` object. See the [SecurityContext example file](example/redisfailover/security-context.yaml) for an example. You can visit kubernetes documentation for detailed docs about [security context](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/)

A custom `securityContext` is merged on top of the operator defaults: fields you set win, and any field you leave unset keeps its default (e.g. setting only `runAsUser` no longer clears `fsGroup`/`runAsNonRoot`).

### Custom containerSecurityContext at container level

By default, the operator runs the containers as user and group `1000` with `runAsNonRoot: true`, drops `ALL` capabilities, and sets `privileged: false`, `allowPrivilegeEscalation: false` and `readOnlyRootFilesystem: true`.
If you need the containers to run with specific capabilities or with a writable root file system (or provide any other securityContext options), then you can specify a custom `containerSecurityContext` in the
`redisfailover` object. See the [ContainerSecurityContext example file](example/redisfailover/container-security-context.yaml) for an example. Keys available under containerSecurityContext are detailed [here](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.20/#securitycontext-v1-core)

A custom `containerSecurityContext` is merged on top of the operator defaults: fields you set win, and any field you leave unset keeps its default (e.g. the dropped `ALL` capabilities and `allowPrivilegeEscalation: false` are retained unless you override them).

### Custom command

By default, redis and sentinel will be called with the basic command, giving the configuration file:

- Redis: `redis-server /redis/redis.conf`
- Sentinel: `redis-server /redis/sentinel.conf --sentinel`

If necessary, this command can be changed with the `command` option inside redis/sentinel spec. An example can be found in the [custom command example file](example/redisfailover/custom-command.yaml).

**Important**: a custom `redis.command` replaces the default command. With `auth.secretPath`, the default command passes `--requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD"` to `redis-server`. The password is not in `redis.conf`, so a custom command must pass these flags itself. Otherwise Redis starts without a password. Kubernetes does not expand `$REDIS_PASSWORD` in a command, so use a shell:

```yaml
command:
  - sh
  - -c
  - exec redis-server /redis/redis.conf --requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD"
```

### Custom environment variables

Extra environment variables can be injected into the redis and sentinel **main** containers via
`redis.env` / `sentinel.env` (standard Kubernetes `EnvVar` entries). The operator's own variables
(`REDIS_ADDR`, `REDIS_PORT`, `REDIS_USER`, `REDIS_PASSWORD`) always take precedence, so a
user-supplied variable that reuses one of those names cannot override it.

### Custom Priority Class
To use a custom Kubernetes [Priority Class](https://kubernetes.io/docs/concepts/configuration/pod-priority-preemption/#priorityclass) for Redis and/or Sentinel pods, you can set the `priorityClassName` in the redis/sentinel spec, this attribute has no default and depends on the specific cluster configuration. **Note:** the operator doesn't create the referenced `Priority Class` resource.

### Custom Service Account
To use a custom Kubernetes [Service Account](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/) for Redis and/or Sentinel pods, you can set the `serviceAccountName` in the redis/sentinel spec. If not specified, the Redis pods use the `default` Service Account, and the Sentinel pods use the `rfs-sa-<NAME>` Service Account that the operator creates. **Note:** the operator doesn't create the referenced `Service Account` resource.

### Custom Pod Annotations
By default, the Sentinel pods have no annotations. The Redis pods have the `redisfailovers.databases.spotahome.com/secret-checksum` annotation. The operator changes it when the password changes, so that the Redis pods restart.

To apply custom pod Annotations, you can provide the `podAnnotations` option inside redis/sentinel spec. An example can be found in the [custom annotations example file](example/redisfailover/custom-annotations.yaml).
### Custom Service Annotations
By default, the `rfr-<NAME>` service has the `prometheus.io/scrape`, `prometheus.io/port` and `prometheus.io/path` annotations. The other services have no annotations.

To apply custom service Annotations, you can provide the `serviceAnnotations` option inside redis/sentinel spec. An example can be found in the [custom annotations example file](example/redisfailover/custom-annotations.yaml).

### Control of label propagation.
By default, the operator will propagate all labels on the CRD down to the resources that it creates.
This can be problematic if the labels on the CRD are not fully under your own control, for example when they are being managed by a gitops operator.
Changes to those labels can fail on immutable resources such as PodDisruptionBudgets.
To control which labels the operator propagates to the resources it creates, you can modify the `labelWhitelist` option in the spec.

By default, specifying no whitelist or an empty whitelist will cause all labels to still be copied as not to break backwards compatibility.

Items in the array should be regular expressions, see [here](example/redisfailover/control-label-propagation.yaml) as an example of how they can be used and
[here](https://github.com/google/re2/wiki/Syntax) for a syntax reference.

The whitelist can also be used as a form of blacklist by specifying a regular expression that will not match any label.

NOTE: The operator will always add the labels it requires for operation to resources.  These are the following:
```
app.kubernetes.io/component
app.kubernetes.io/managed-by
app.kubernetes.io/name
app.kubernetes.io/part-of
redisfailovers-role
redisfailovers.databases.spotahome.com/name
```


### ExtraVolumes and ExtraVolumeMounts

If the user chooses to have extra volumes creates and mounted, he could use the `extraVolumes` and `extraVolumeMounts`, in `spec.redis` of the CRD. This allows users to mount the extra configurations or secrets to be used. A typical use case for this might be
- Secrets that sidecars might use to back up of RDBs
- Extra users and their secrets and acls that could use the initContainers to create multiple users
- Extra Configurations that could merge on top of the existing configurations
- To pass failover scripts for addition for additional operations

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



## Connection to the created Redis Failovers

To connect to the redis-failover and use it, you can either connect through Sentinel or directly to the Redis master service, depending on the failover mode.

When `spec.sentinel.enabled: true`, a [Sentinel-ready](https://redis.io/topics/sentinel-clients) library has to be used. The connection parameters are the following:

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

Reads can also go to the replicas through `rfrs-<NAME>`. A replica is ready, and so behind that service, only while it has the master's data. It is not ready during a full sync, until its first sync since it started has completed (a replica that can't load the master's RDB format never gets there), and once its link to the master has been down for longer than the failover can take: 60 seconds plus `spec.sentinel.failoverTimeout` without Sentinel, or plus Sentinel's `down-after-milliseconds` and `failover-timeout` (from `spec.sentinel.customConfig`) with it. So the replicas stay ready through a failover.

### Enabling redis auth

To enable auth, create a secret with a password field:

```
echo -n "pass" > password
kubectl create secret generic redis-auth --from-file=password

## example config
apiVersion: databases.spotahome.com/v1
kind: RedisFailover
metadata:
  name: redisfailover
spec:
  sentinel:
    replicas: 3
  redis:
    replicas: 1
  auth:
    secretPath: redis-auth
```
You need to set secretPath as the secret name which is created before.

Rotating the password (updating the `password` key of that same Secret in place), adding `auth.secretPath` or removing it is safe. The operator first switches every running Redis, and the Sentinels, to the new password in place with `CONFIG SET`, which keeps replication up, and then restarts the Redis pods one at a time onto the Secret. New connections need the new password right away.

Until a pod restarts, whatever reads the password from its environment keeps the old one: the exporter sidecar can't authenticate, the pre-stop `SAVE` fails, and so do custom probes using `$REDIS_PASSWORD`.

Until a pod restarts, the default readiness probe also reports the pod Ready without a replication check. The probe uses the old password, which Redis refuses. A refused password says nothing about replication, so the probe does not fail the pod for it. During this time, `rfrs-<NAME>` can send reads to a replica that is not synced.

The operator knows the old password only from memory. If it restarted between the change and its next check, it can't switch the pods. The RedisFailover then reports `unable to apply the configured password`. Put the previous password back in the Secret, wait for the RedisFailover to become healthy, then change it again.

The password is read from that Secret and passed to `redis-server` via `--requirepass`/`--masterauth`
sourced from an environment variable; it is **not** written into the redis ConfigMap, so it never
appears in plaintext in a ConfigMap or in the pod spec. A custom `redis.command` must pass these flags itself.

### Bootstrapping from pre-existing Redis Instance(s)
If you are wanting to migrate off of a pre-existing Redis instance, you can provide a `bootstrapNode` to your `RedisFailover` resource spec.

This `bootstrapNode` can be configured as follows:
|       Key      | Type         | Description                                                                                                                                                                               | Example File                                                                                 |
|:--------------:|--------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| host           | **required** | The IP of the target Redis address or the ClusterIP of a pre-existing Kubernetes Service targeting Redis pods                                                                             | [bootstrapping.yaml](example/redisfailover/bootstrapping.yaml)                               |
| port           | _optional_   | The Port that the target Redis address is listening to. Defaults to `6379`.                                                                                                               | [bootstrapping-with-port.yaml](example/redisfailover/bootstrapping-with-port.yaml)           |
| allowSentinels | _optional_   | Allow the Operator to also create the specified Sentinel resources and point them to the target Node/Port. This only takes effect when `spec.sentinel.enabled` is `true`. By default, the Sentinel resources will **not** be created when bootstrapping. | [bootstrapping-with-sentinels.yaml](example/redisfailover/bootstrapping-with-sentinels.yaml) |

#### What is Bootstrapping?
When a `bootstrapNode` is provided, the Operator will always set all the defined Redis instances to replicate from the provided `bootstrapNode` host value.
This allows for defining a `RedisFailover` that replicates from an existing Redis instance to ease cutover from one instance to another.

**Note: by default, the Redis instances get `replica-priority 0`, so that Sentinel never promotes them to `master`. A `replica-priority` entry in `redis.customConfig` replaces this value.**

Depending on the configuration provided, the Operator will launch the `RedisFailover` in two bootstrapping states: without sentinels and with sentinels.

#### Default Bootstrapping Mode (Without Sentinels)
By default, if the `RedisFailover` resource defines a valid `bootstrapNode`, **only the redis instances will be created**.
This allows for ease of bootstrapping from an existing `RedisFailover` instance without the Sentinels intermingling with each other.

#### Bootstrapping With Sentinels
When `allowSentinels` is provided and `spec.sentinel.enabled` is `true`, the Operator will also create the defined Sentinel resources. These sentinels will be configured to point to the provided
`bootstrapNode` as their monitored master.

### Default versions

The image versions deployed by the operator can be found on the [defaults file](api/redisfailover/v1/defaults.go).

### Migrating to Valkey

Valkey images ship `redis-server` and `redis-cli`, so switching `redis.image` and `sentinel.image` to a Valkey image is an ordinary rolling update.

Migrate from Redis 7.2 only. Valkey forked from Redis 7.2 and can't load the data of Redis 7.4 or later (`Can't handle RDB format version 12`). The first replica on Valkey then never syncs, so the operator doesn't replace the master: the RedisFailover keeps running on Redis, but the rollout never completes. To end the migration, revert `redis.image`. The operator then replaces the unsynced Valkey replica, because that replica has no data to lose.
## Cleanup

### Operator and CRD

If you want to delete the operator from your Kubernetes cluster, the operator deployment should be deleted.

Also, the CRD has to be deleted. Deleting CRD automatically will delete all redis failover custom resources and their managed resources:

```
kubectl delete crd redisfailovers.databases.spotahome.com
```

### Single Redis Failover

Thanks to Kubernetes' `OwnerReference`, all the objects created from a redis-failover will be deleted after the custom resource is.

```
kubectl delete redisfailover <NAME>
```

## Docker Images

### Redis Operator

* [Redis Operator Image](https://github.com/Saremox/redis-operator/pkgs/container/redis-operator)

## Documentation

For the code documentation, you can look up on the [GoDoc](https://godoc.org/github.com/Saremox/redis-operator).

Also, you can check information on the [docs folder more deeply](docs).
