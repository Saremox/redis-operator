# Soak tester

A long-running tester for redis-operator release candidates. It runs
in-cluster next to an installed operator, keeps mutating a fixed set of
RedisFailover instances while it probes and observes them, and exports the
results as Prometheus metrics, so an RC can be judged from a dashboard after
hours or days.

It is a separate Go module that builds against the operator's API through a
`replace` to the repository root, so a tester built from a tag always matches
that tag's spec.

## What it does today

### Probes

For every configured instance it probes the master once per
`probe.interval`, through every path the instance's mode has:

- `sentinel` (Sentinel instances only): the way a Sentinel-aware client
  connects. go-redis's failover client asks the Sentinels behind
  `rfs-<name>:26379` for `mymaster`, and follows their failover
  announcements.
- `rfrm` (every instance): the `rfrm-<name>` Service.

Each path is probed with three client styles:

- `pooled`: one long-lived go-redis client, like an application.
- `retrying`: the same with go-redis's default retries, like an application
  that didn't tune its client.
- `fresh`: a new client for every probe, like a newly started pod. On the
  `sentinel` path, that includes asking a Sentinel for the master.

Each probe is a `SET soak:<rf>:<path>:<client>:seq <n>` followed by a `GET`
of the same key. Except for `retrying`, go-redis retries are off, so every
failure is counted. `retrying` retries within the same `probe.timeout`.

### Invariants

Every `observer.interval` (default 5s), the observer reads the
RedisFailover, its pods and the `rfrm-<name>` EndpointSlices, sends `INFO`
to every redis pod by its IP, and asks every Sentinel pod for
`get-master-addr-by-name mymaster`. It then checks:

| Invariant | Holds when |
|---|---|
| `pods` | There are `spec.redis.replicas` redis pods and all are Ready. With Sentinel, there are also `spec.sentinel.replicas` Sentinel pods, all Ready. |
| `one_master` | Exactly one redis pod reports the master role in `INFO replication`. |
| `master_service` | The `rfrm-<name>` EndpointSlices hold exactly one ready address, the master's. |
| `replication` | Every other redis pod is a replica of the master's IP and port, with `master_link_status:up`. |
| `sentinel_agreement` | (Sentinel instances only) every Sentinel reports the master's IP and port. |
| `healthy` | The RedisFailover's `status.state` is `Healthy`. |

The mode the invariants follow is the RedisFailover's `sentinel.enabled`,
with the operator's defaults applied. Redis and Valkey are read the same way:
the roles `master`/`primary` and `slave`/`replica` are both accepted.

A pod that doesn't answer `INFO` has no role, so an unreachable master
violates `one_master`, and an unreachable replica violates `replication`.
`master_service`, `replication` and `sentinel_agreement` can't hold without
a single master, so they are violated whenever `one_master` is.

### Mutations

For every instance with a `mutations` section, the mutator changes the
instance one mutation at a time:

1. It waits until the instance is quiet: no convergence window open and
   every invariant holding.
2. It picks a kind by weight, and the kind's parameters, from the seed
   and the instance's step. Each instance and step has its own random
   source, so a step logged with its seed can be replayed on its own.
3. It opens a convergence window and applies the mutation.
4. It waits for the window to close or time out, and records the result.
5. It sleeps `mutation.interval` plus up to `mutation.jitter`.

| Kind | Mutation | Converged when |
|---|---|---|
| `redis_replicas` | Merge-patches `spec.redis.replicas` to another value in `redisReplicas`. | The `rfr-<name>` StatefulSet has the new `spec.replicas`, has observed its generation, and has that many replicas, all ready and on its update revision; so many redis pods exist, all Ready. |
| `sentinel_replicas` | Merge-patches `spec.sentinel.replicas` to another value in `sentinelReplicas`. | The `rfs-<name>` Deployment has observed its generation and has that many replicas, all ready, updated and available; so many Sentinel pods exist, all Ready. |
| `redis_resources` | Merge-patches the cpu, memory or both of the redis container's requests and limits to other values within `resources`. Only values the RedisFailover already sets are changed, so the set of requests and limits stays the same, and a change that would alter the QoS class is skipped: the operator can resize the pods in place. | Every redis pod runs the new values: the kubelet reports them in `status.containerStatuses[].resources` (or, if it doesn't, the pod spec has them and no resize is in flight), and no pod has a pending `resize-requested-at` annotation. The StatefulSet is converged as for `redis_replicas`. |
| `kill_master` | Deletes the pod labelled `redisfailovers-role=master`, if it is the one the observer last saw as the single master. | A new pod of the same name is Ready. |
| `kill_replica` | Deletes a pod labelled `redisfailovers-role=slave`. | A new pod of the same name is Ready. |
| `kill_sentinel` | Deletes a Sentinel pod. | The pod is gone and the Deployment is converged as for `sentinel_replicas`. |

Kills delete with a UID precondition, gracefully or, with a probability
of `forceDeleteProbability`, with `GracePeriodSeconds=0`.

The StatefulSet's `currentRevision` isn't compared: the operator's
StatefulSets use `OnDelete`, for which the controller never advances it.

Each mutation is logged as `mutating` and then `mutation done` (or
`mutation skipped`) with `rf`, `namespace`, `mode`, `kind`, `seed`, `step`,
`params` (`old -> new`, or the pod deleted and how), `result`,
`duration_seconds` and `pods_recreated`.

`result` is one of:

- `converged`: the window closed.
- `timeout`: the window timed out after `observer.convergenceTimeout`.
- `rejected`: the API refused the patch or the delete. Its window closes
  once the invariants hold.
- `skipped`: the mutation couldn't be applied, e.g. a replica kill on an
  instance with one redis pod, or a resource change that would alter
  the QoS class. Nothing was changed and no window was opened.

### Findings

A violation is expected while the instance converges after a change, and a
**finding** otherwise. Convergence windows open in two ways:

- **A mutation** opens a window before it is applied. The window stays open
  until the mutation's own convergence signal holds (see Mutations), every
  invariant holds (`healthy` included) and `mutation.minDwell` (default
  15s) has passed, or until `observer.convergenceTimeout` (default 10m).
  The dwell keeps a change the operator hasn't picked up yet from being
  taken as converged.
- **A change of the RedisFailover's `metadata.generation`** that the
  tester didn't make, e.g. a spec change by hand, opens a window that
  closes as soon as every invariant holds, or after the convergence
  timeout. The operator itself bumps the generation at times, too.

Then:

- A violation that starts while no window is open is a finding.
- A violation that is still open when its window times out becomes a
  finding then.

Each finding increments `findings_total{invariant}` once. The tester also
opens a window when it first sees an instance, since a change may be in
flight.

Pod kills open a window like every mutation, so the failover after a
master kill isn't a finding. A pod killed by anything else, or a failover
nobody asked for, is.

Every transition is logged as a JSON line with `rf`, `namespace`, `mode`
and `invariant`: `invariant violated` with the `reason` and whether it is a
`finding`, and `invariant restored` with its `duration_seconds`. Failovers
are logged as `failover` with the old and new master pod, and windows as
`convergence window opened` / `closed` / `timed out`; a mutation's window
that timed out also logs why the mutation hadn't converged.

## Running it

```sh
make build test lint      # binary in bin/soak
make image                # ghcr.io/saremox/redis-operator-soak:<git describe>
make kind-e2e             # kind cluster, operator, instances, mutating tester, assertions
```

`kind-e2e` reuses `.claude/skills/kind-cluster/`, installs the operator from
this checkout with `charts/redisoperator`, recreates `op-basic` and
`sent-basic` from scratch, and deploys the tester with
[`e2e/config.yaml`](e2e/config.yaml): the mutator on, a 10-20s interval and
seed 629, which picks every enabled kind within each instance's first five
steps. The mutator starts mutations for `DURATION` seconds (default 600).
Once the last ones have converged, it asserts from `/metrics` that:

- every enabled kind converged at least once on every instance, and none
  timed out or was rejected;
- `findings_total` stayed 0;
- every invariant holds, with one master, and `server_info` has a series
  per redis pod;
- every path and client style can write and read again;
- each instance whose master was killed failed over.

It prints every mutation, the convergence time per kind, the outages per
path and client style attributed to the mutation they started in, the
pods each kind recreated, and any findings or timed-out windows. The
tester's and the operator's logs, the instances' events and the last
scrape are kept in `bin/kind-e2e-artifacts/`. Set
`OPERATOR_VERSION=4.2.0-rc1` to install a released chart and image instead.

### Pointing it at an RC

1. Install the RC:
   `helm install redis-operator oci://ghcr.io/saremox/redis-operator/charts/redis-operator --version <tag> -n redis-operator --create-namespace`.
2. Create the instances listed in the config.
3. Set the tester image to the same tag and apply the manifests:

   ```sh
   cd deploy && kustomize edit set image ghcr.io/saremox/redis-operator-soak:<tag>
   kubectl apply -k deploy
   ```

The Role in `deploy/rbac.yaml` must be in the operator's namespace and name
its Deployment, matching `operator` in `deploy/config.yaml`. A ClusterRole
lets the observer read pods, EndpointSlices and RedisFailovers in the
instances' namespaces.

The mutator's rights (patch RedisFailovers, delete pods, read StatefulSets
and Deployments) are granted only in the instances' namespaces: the
`redis-soak-mutator` ClusterRole holds the rules once, and
`deploy/rbac-instances.yaml` binds it with a RoleBinding in every instance
namespace. Keep that file in step with the instances in `config.yaml`, and
create the namespaces before applying the manifests.

## Configuration

See [`config.example.yaml`](config.example.yaml). `deploy/config.yaml` is the
config the manifests ship.

## Metrics

Served on `:9090/metrics`; `/healthz` answers `ok`. Per-instance series carry
`rf`, `namespace` and `mode` (`operator` or `sentinel`).

| Metric | Type | Extra labels |
|---|---|---|
| `redis_soak_probe_total` | counter | `path`, `client`, `op` (`set`/`get`), `result` |
| `redis_soak_probe_duration_seconds` | histogram | `path`, `client`, `op` |
| `redis_soak_writable`, `redis_soak_readable` | gauge (0/1) | `path`, `client` |
| `redis_soak_last_success_timestamp_seconds` | gauge | `path`, `client`, `op` |
| `redis_soak_outage_duration_seconds` | histogram | `path`, `client` |
| `redis_soak_invariant_ok` | gauge (0/1) | `invariant` |
| `redis_soak_invariant_violation_seconds` | histogram | `invariant` |
| `redis_soak_findings_total` | counter | `invariant` |
| `redis_soak_masters` | gauge | |
| `redis_soak_failovers_total` | counter | |
| `redis_soak_replication_lag_bytes` | gauge | `pod` |
| `redis_soak_rf_healthy` | gauge (0/1) | |
| `redis_soak_server_info` | gauge | `pod`, `server` (`redis`/`valkey`), `version` |
| `redis_soak_build_info` | gauge | `operator_version`, `tester_version` |
| `redis_soak_mutation_total` | counter | `kind`, `result` (`converged`/`timeout`/`rejected`/`skipped`) |
| `redis_soak_mutation_converge_seconds` | histogram | `kind` |
| `redis_soak_pods_recreated_total` | counter | `kind` |
| `redis_soak_mutation_in_progress` | gauge (0/1) | `kind` |

`result` is one of `ok`, `timeout`, `refused`, `readonly`, `loading`, `auth`,
`oom`, `masterdown`, `noreplicas`, `dns`, `closed` and `other`. Server errors
are classified by their leading error code, never by the message, so Redis and
Valkey count the same way. `closed` is a connection closed or reset by the
server, such as the operator disconnecting clients from a demoted master.

An outage runs from the first failed probe to the next successful one, per
path and client style.

`masters` is the number of redis pods reporting the master role: 1 is right,
0 is an instance without a master and 2 or more a split brain.
`failovers_total` counts changes of the master pod (by pod UID, so a master
that is replaced by a new pod with the same name counts too).
`replication_lag_bytes` is the master's `master_repl_offset` minus each
replica's `slave_repl_offset`, for the replicas of the current master; the
two `INFO` replies are a few milliseconds apart, so it is floored at 0.
`server_info` comes from `INFO server` (`valkey_version`, `server_name` and
`redis_version`) and shows pods running different servers or versions.
The per-pod series exist only while the pod answers `INFO`, and disappear
with the pod. `operator_version` is the image tag of the operator
Deployment, re-read every 30 seconds.

`mutation_converge_seconds` runs from applying a converged mutation to its
window closing. The window is judged on observer ticks and stays open for
at least `minDwell`, so the values are at least `minDwell` and step by
`observer.interval`; its buckets reach up to the convergence timeout.
`pods_recreated_total` counts the redis pods a mutation replaced by a new
pod of the same name, by UID. A kill counts the pod it deleted; a
`redis_resources` mutation that counts any was not done in place.
