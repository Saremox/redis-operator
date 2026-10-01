# Soak tester

A long-running tester for redis-operator release candidates. It runs
in-cluster next to an installed operator, probes a fixed set of
RedisFailover instances and exports the results as Prometheus metrics, so an
RC can be judged from a dashboard after hours or days.

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

### Findings

A violation is expected while the instance converges after a change, and a
**finding** otherwise. Whenever the RedisFailover's `metadata.generation`
changes, a convergence window opens. It closes as soon as every invariant
holds, or after `observer.convergenceTimeout` (default 10m). Then:

- A violation that starts while no window is open is a finding.
- A violation that is still open when its window times out becomes a
  finding then.

Each finding increments `findings_total{invariant}` once. The tester also
opens a window when it first sees an instance, since a change may be in
flight.

There is no mutator yet, so windows only open on spec changes made by hand.
A pod killed by chaos doesn't change the generation: today, the failover
after a master pod is killed counts as findings (`pods`, `one_master` and
whatever else it disturbs). That will change once the mutator opens a
window for the kills it makes.

Every transition is logged as a JSON line with `rf`, `namespace`, `mode`
and `invariant`: `invariant violated` with the `reason` and whether it is a
`finding`, and `invariant restored` with its `duration_seconds`. Failovers
are logged as `failover` with the old and new master pod, and windows as
`convergence window opened` / `closed` / `timed out`.

## Running it

```sh
make build test lint      # binary in bin/soak
make image                # ghcr.io/saremox/redis-operator-soak:<git describe>
make kind-e2e             # kind cluster, operator, op-basic, tester, assertions
```

`kind-e2e` reuses `.claude/skills/kind-cluster/`, installs the operator from
this checkout with `charts/redisoperator`, creates `op-basic` and
`sent-basic`, deploys the tester and lets it run for `DURATION` seconds
(default 120). It asserts from `/metrics` that every path and client style
can write and read, that every invariant holds with one master, and that
`server_info` is exported. It then force-deletes each instance's master pod
and asserts that both fail over (`failovers_total` grows), that every
invariant holds again and every probe succeeds again within `RECOVERY`
seconds (default 180), and prints the outage and violation durations it
measured. Set `OPERATOR_VERSION=4.2.0-rc1` to install a released chart and
image instead.

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
