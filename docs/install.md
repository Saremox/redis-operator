# Operator deployment on Kubernetes

Deploy the operator before you create a RedisFailover.
You can deploy it with the [kubectl manifests](../example/operator), with [Kustomize](../manifests/kustomize) or with the [Helm chart](../charts/redisoperator).

## Install with the Helm chart

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

### Update the Helm chart

Read the [release notes](https://github.com/Saremox/redis-operator/releases) before an upgrade. If a release needs manual steps, its notes start with a warning that links to a guide in [docs/migrations](migrations).

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

When you upgrade from a release without the `status` subresource, update the CRD and the operator together. `helm upgrade` updates the CRD only with `crds.upgradeHook.enabled=true`. A ClusterRole that you maintain yourself needs `get` and `patch` on `redisfailovers/status`. See the [4.2.0 migration guide](migrations/4.2.0.md).

## Install with kubectl

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

## Install with kustomize

You can change the kustomize setup of this repository with [components](https://kubectl.docs.kubernetes.io/guides/config_management/components/).
The overlays are presets for the most common use cases.

The `default` overlay installs the operator with the RBAC, the service account and default resource limits. The CRD is 1.1 MB, so a client-side `kubectl apply` fails on it. The reason is the 256 KiB limit for the annotations of an object. Apply the overlay server-side:

```shell
kustomize build github.com/Saremox/redis-operator/manifests/kustomize/overlays/default | kubectl apply --server-side -f -
```

The overlays install the operator in the `default` namespace, because the ClusterRoleBinding must name the namespace of the ServiceAccount. The `redis-operator.yaml` file of each GitHub release contains the `default` overlay. Apply this file also with `--server-side`. To use a different namespace, set `namespace:` in your own `kustomization.yaml`, as in the example below. `kubectl apply -n <namespace>` does not work with these files.

Run only one operator in a cluster. Each operator takes the leader lease in its own namespace, so two operators in different namespaces both change the RedisFailovers. When you move the operator to a different namespace, delete the old operator Deployment.

The `minimal` overlay is the `default` overlay without the resource limits. It also creates the RBAC and the service account. To use your own RBAC or service account, use the `base` and the [components](../manifests/kustomize/components) in your own kustomization.

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

See the manifests in [manifests/kustomize](../manifests/kustomize) for more information.

## Operator flags

With the Helm chart, set the flags in `image.cli_args`. The chart sets `--listen-address` from `container.port`, and `--watch-auth-secrets` from `watchAuthSecrets`. Do not set `--listen-address` in `image.cli_args`, because the probes use `container.port`. With kubectl or kustomize, add the flags to the `args` of the operator container.

| Flag | Default | Description |
|---|---|---|
| `--concurrency` | `3` | The number of workers that reconcile RedisFailovers in parallel. A value of 0 or less gives 3. |
| `--development` | `false` | Use the file in `--kubeconfig`, not the in-cluster configuration. With this flag, the operator can run outside a cluster. |
| `--disconnect-clients-on-demotion` | `true` | Close the `normal` and `pubsub` client connections of a master when it becomes a replica. See [Usage](../README.md#usage). |
| `--enable-pprof` | `false` | Serve the Go profiler. See [Operator metrics](#operator-metrics). |
| `--k8s-cli-burstable-limit` | `100` | The number of requests that the Kubernetes client can send at once without a delay. After such a burst, the client sends at the rate of `--k8s-cli-qps-limit`. With `--k8s-cli-qps-limit` above 0, a value of 0 or less stops the operator at start. |
| `--k8s-cli-qps-limit` | `100` | The maximum mean number of requests in each second that the Kubernetes client sends. The client delays the requests above this rate. A value of 0 gives 5 requests in each second for each API group. A negative value disables the limit. |
| `--kubeconfig` | `$HOME/.kube/config` | The kubeconfig file. The operator reads it only with `--development`. |
| `--listen-address` | `:9710` | The address of the HTTP server for the metrics. |
| `--log-level` | `info` | The log level: `panic`, `fatal`, `error`, `warn`, `info`, `debug` or `trace`. The operator ignores the case and also accepts `warning`. With another value, the operator stops at start with exit code 1. |
| `--metrics-path` | `/metrics` | The HTTP path of the metrics. |
| `--supported-namespaces-regex` | `.*` | Reconcile only the RedisFailovers in a namespace that matches this regular expression. The expression can match a part of the name, so use `^` and `$` for an exact name. The operator still lists and watches the RedisFailovers in all namespaces. A regular expression that is not valid stops the operator at start. |
| `--sync-interval` | `30` | The seconds between two resyncs of all RedisFailovers. A value of 0 or less gives 180. A change of a RedisFailover or of its pods starts a reconcile immediately. Above 300, the metrics cleanup can delete series between two resyncs. |
| `--watch-auth-secrets` | `false` | Apply a change of the Secret in `auth.secretPath` immediately, not at the next resync. It needs `list` and `watch` on all Secrets. See [Redis authentication](authentication.md#redis-authentication). |

## Operator metrics

The operator serves Prometheus metrics on `--listen-address` (default `:9710`) at `--metrics-path` (default `/metrics`). The flag `--enable-pprof` (default `false`) serves the Go profiler at `/debug/pprof/` on the same address. The profiler has no authentication. Allow only trusted users to reach this address.

In Sentinel mode, these gauges have the labels `namespace` and `name`:

- `redis_operator_controller_unknown_sentinel_peers`: the number of peer IPs that the Sentinels know and that no Sentinel pod of the RedisFailover has.
- `redis_operator_controller_sentinel_unknown_master`: `1` when a Sentinel monitors a master IP that no Redis pod of the RedisFailover has, else `0`.

The operator counts an IP after two checks at least 90 seconds apart find it. No pod of the RedisFailover had that IP in the last 5 minutes. An alive peer with a run ID that the deleted Sentinel pod at its IP did not have counts without the 5 minutes. A value above `0` can show the Sentinels of two RedisFailovers in one group, because all RedisFailovers use the master name `mymaster`. It can also show an old IP of a deleted pod of this RedisFailover. See [Sentinel mode](logic.md#sentinel-mode).

## Security notes

The author of a RedisFailover controls the pods that the operator creates: containers, volumes, `hostPath`, `hostNetwork` and service account. Enforce the Pod Security Admission level `restricted`, or an admission policy, in each namespace where users create a RedisFailover.

A RedisFailover in the operator namespace can use the service account of the operator. Install the operator in its own namespace. With Helm, use the options `--namespace` and `--create-namespace`.

Redis and Sentinel do not use TLS. Sentinel has no password. Redis has no password without `auth.secretPath`. Use a NetworkPolicy to limit access to the Redis and Sentinel ports. Allow only your monitoring system to reach the exporter port.

### NetworkPolicy

We recommend NetworkPolicy rules for ingress and egress that isolate each RedisFailover from the other RedisFailovers and from other workloads. This is a best practice, not a guarantee. The owner of this project runs such rules in production, with strong isolation between the namespaces and in each namespace.

The Sentinel port has no password, and all RedisFailovers use the master name `mymaster`. Thus the Sentinels of two RedisFailovers can merge when a pod gets an IP that the Sentinels of the other RedisFailover still know. A merged group can promote the wrong pods, and the pods can lose acknowledged writes. With equal Redis passwords, a Redis pod can replicate from the other RedisFailover and lose all its data. NetworkPolicy rules block the hello messages between the groups.

The checks of the operator make the time for a merge shorter and report a merge, but they do not remove the hazard. See [Sentinel mode](logic.md#sentinel-mode). A master name for each RedisFailover is the planned complete fix.

Allow only this traffic. `<NAME>` is the name of the RedisFailover.

| Pods | Ingress port | Allow ingress from |
| --- | --- | --- |
| Sentinel | `26379` | The Sentinel and the Redis pods of the same RedisFailover, the operator pods, and the clients that use Sentinel. |
| Redis | `redis.port` (default `6379`) | The Redis and the Sentinel pods of the same RedisFailover, the operator pods, and the clients. |
| Redis exporter | the exporter port (default `9121`) | Only the system that collects the metrics. |
| Sentinel exporter | the exporter port (default `9355`) | Only the system that collects the metrics. |

- Egress of the Redis and the Sentinel pods: only to the Redis and Sentinel ports of the same RedisFailover, and to DNS. In bootstrap mode, also to the bootstrap node.
- The DNS rule depends on the cluster. The example allows the pods `k8s-app: kube-dns` in `kube-system` on port 53. With NodeLocal DNSCache, the pods use a node-local address. On OpenShift, the DNS pods are in `openshift-dns` and listen on port 5353. Make sure that the rule allows your DNS. Without DNS, the shutdown script cannot find the Sentinels and asks for no failover. Then each deletion of the master pod gives a failover after `down-after-milliseconds`.
- The operator connects from its pod to the Redis port and the Sentinel port of each pod. When the operator namespace has egress rules, also allow this traffic and the Kubernetes API.
- The Redis and the Sentinel pods do not use the Kubernetes API. Without `serviceAccountName`, the operator sets `automountServiceAccountToken: false`.
- The shutdown script of Redis asks the Sentinel Service for the failover. A NetworkPolicy applies to the pod IPs behind the Service, so the rule for the Sentinel pods allows it. The DNS rule allows the lookup of the Service name.
- The liveness probe of Redis and the probes of Sentinel connect to the IP of their own pod. The rule for the pods of the same RedisFailover allows this. The readiness probe of Redis connects to `localhost`.
- Select the pods with the labels that the operator sets: `app.kubernetes.io/part-of: redis-failover`, `app.kubernetes.io/name: <NAME>` and `app.kubernetes.io/component: redis` or `sentinel`. A selector without `app.kubernetes.io/component` selects both.
- The rules need a CNI plugin that enforces NetworkPolicy. Without such a plugin, Kubernetes accepts the rules and enforces nothing.
- A pod selector does not select pods with `hostNetwork: true`, so the rules do not isolate such pods.
- A CNI plugin that tracks connections can keep the connections that were open before you applied the rules. To end a merge of two groups, restart the Sentinel pods of one group after you apply the rules.

See the [NetworkPolicy example file](../example/redisfailover/networkpolicy.yaml).
