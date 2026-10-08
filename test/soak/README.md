# Soak tester

The soak tester tests a redis-operator release candidate (RC) for hours or
days. It runs in the cluster next to the installed operator. It changes a
set of RedisFailover instances continuously, probes them as applications
do, and checks their state. It exports the results as Prometheus metrics,
so that alerts and a dashboard can judge the RC.

The tester is a separate Go module. It builds against the operator API
through a `replace` of the repository root. Thus a tester built from a tag
always uses the spec of that tag.

## What an RC must not do

A change of an instance, for example a failover, causes violations while
the instance converges. The tester expects them inside a convergence
window. It counts these events in `redis_soak_findings_total`, and
`RedisSoakFinding` fires:

- a violation of an invariant outside a window;
- a violation that is still open when its window times out;
- an OOM kill of a redis or Sentinel container, also inside a window,
  because the configuration keeps room for a fork of the data (see
  [Mutations](#mutations));
- a version change that fails unsafely (see [Server versions](#server-versions));
- a reset that does not complete within two times its timeout;
- a Ready replica that never completed a sync.

These events are also not expected, and have their own alerts:

- a write lost in an event that must lose none (see [Data](#data));
- a mutation or chaos action that times out or fails;
- an `auth` outage of the `follower` client for more than 2 minutes.

The tester measures this behaviour, but it is not a failure: outages and
violations inside a window, writes lost to asynchronous replication in a
forced kill or a failover, `failed_safe` version changes, `OOM` rejections
under `noeviction`, and evictions under `allkeys-*`.

## How the tester works

### Probes

The tester writes and reads a key on the master of each instance every
`probe.interval`, through each path of its current mode:

- `sentinel`: a go-redis failover client that asks the Sentinels for the
  master, as a Sentinel-aware application does;
- `rfrm`: the `rfrm-<name>` Service;
- `rfrs`: the `rfrs-<name>` Service, read-only, for a bootstrapping
  instance, because all its pods are replicas.

The paths follow `sentinel.enabled` of the RedisFailover. The tester adds
the `sentinel` path only when all Sentinels are Ready and agree on the
master.

Each path has four client styles, because applications hold their
connections in different ways:

- `pooled`: one long-lived client.
- `retrying`: `pooled` with the default go-redis retries.
- `fresh`: a new client for each probe, as a new pod.
- `follower`: `pooled`, but the tester replaces the client when the auth
  Secret changes. Its `auth` failures show how long the operator takes to
  apply a password change.

Go-redis retries are off for all styles except `retrying`, so that each
failure counts. The tester classifies a server error by its error code,
not by its message, so that Redis and Valkey errors count the same. Every
tenth write also sends `WAIT 1`, to sample how many replicas acknowledge a
write. An outage starts at the first failed probe, and counts for the
mutation or chaos action that ran then.

When the RedisFailover has `spec.auth.secretPath`, each connection reads
the current password from the Secret. Passwords are never in logs, metrics
or errors.

### Data

For each instance with `data`, the tester does these steps:

1. The filler keeps `used_memory` at `fill.percent` of `maxmemory`, or at
   `fill.sizeMi` without `maxmemory`. Each value comes from its key, so the
   tester can verify any key without stored state.
2. The ledger (`data.ledger`) writes keys continuously and records each
   acknowledged write.
3. A verification reads from the master pod all ledger writes since the
   previous verification, a sample of older writes and a sample of fill
   keys. Each missing or wrong key counts once in
   `redis_soak_lost_writes_total{event}`. The tester then deletes the
   ledger keys that it no longer needs.

A verification runs after each mutation and chaos action, after each
failover outside a mutation, and every 10 minutes. A failover that loses
the data by design is a `reset`: the only pod without a volume was
replaced, or the RedisFailover was recreated.

Asynchronous replication can lose writes in a failover. Thus most losses
are a value to monitor. The tester decides for each verification whether
the event must lose no write, and counts such losses in
`redis_soak_unexpected_lost_writes_total`. These events must lose none:

- auth changes and Sentinel changes, because they stop each redis pod
  gracefully at most, and Redis waits for its replicas on SIGTERM;
- operator restarts and upgrades, because they do not touch a redis pod;
- on volumes: a graceful master kill, a scale-down, a drain and a version
  change along an `ok` edge;
- for a bootstrapping instance: each source write that a pod that caught up
  does not have.

Redis 6.2 does not wait for its replicas on SIGTERM. On an instance that runs
Redis 6.2, the tester does not require a lossless result for an event that
stops a redis pod. It counts such a loss in `redis_soak_lost_writes_total`.

The ledger keys must never be evicted, because the tester cannot tell an
eviction from a loss. Config validation rejects a ledger on an instance
that can run under `allkeys-*`, and a ledger under `volatile-*` without
`fill.ttl`.

A bootstrapping instance (`bootstrap.source`) is read-only. Its
verification reads a sample of the source writes from the source master,
and then from each pod when the pod caught up with the replication stream
of that master. The tester does not compare offsets of different
replication IDs: a pod that did not sync with a new source master yet can
be far ahead in the old stream.

### Invariants

Every `observer.interval`, the observer reads the RedisFailover, its pods
and the `rfrm` EndpointSlices, sends `INFO` to each pod, and checks these
invariants:

- `pods`: the redis and Sentinel pods of the spec exist and are Ready.
- `one_master`: exactly one redis pod reports the master role. A pod that
  does not answer has no role. For a bootstrapping instance, each pod
  replicates the stream of the current source master.
- `master_service`: the `rfrm` EndpointSlices hold only the master. For a
  bootstrapping instance, they hold no address.
- `replication`: each other redis pod replicates the master with its link
  up.
- `sentinel_agreement`: each Sentinel reports the master.
- `healthy`: `status.state` is `Healthy`.
- `config`: each pod runs the `customConfig` of the spec, and the
  `maxmemory` settings that the operator sets. The tester calculates them
  with its own code and with the helpers of `api/redisfailover/v1`, never
  with the operator code under test.
- `oom_killed`: no container reports an `OOMKilled` termination since the
  tester started.
- `replica_ready_without_data`: no Ready replica has a link that was never
  up. Kubernetes sends reads to such a pod, but it has no data. Set
  `observer.replicaReadyWithoutData: false` to only report it, for an
  operator without the readiness fix of PR #205.

The invariants follow the current mode. When an invariant does not apply,
for example `sentinel_agreement` after Sentinel is off, the tester removes
its series.

### Convergence windows

A mutation opens a window before the mutator applies it. The window closes
when the mutation converged, all invariants hold, and at least 15 seconds
passed. Without this minimum, a change that the operator did not see yet
can look converged. The window times out after
`observer.convergenceTimeout`, or after the entry of the kind in
`mutation.timeouts`.

A change of `metadata.generation` outside a mutation also opens a window,
for example a manual change. A disturbance outside the instance keeps a
window open: a stopped operator, a chaos action, or a source of a
bootstrapping instance that is mutated or converges. The tester also opens a window when
it first sees an instance.

### Mutations

For each instance with `mutations.kinds`, the mutator applies one mutation
at a time. It waits until the instance is quiet: no window is open and all
invariants hold. It picks a kind by weight from `mutation.seed`, the
instance name and the step, so that you can replay a logged step. Then it
waits `mutation.interval` plus up to `mutation.jitter`.

The kinds are in `internal/config/mutations.go`, and the convergence
signal of each kind is in `internal/mutator`. Some kinds need an
explanation:

- `kill_master` and `kill_master_force` are two kinds, because a forced
  delete skips the preStop hook, and the master cannot wait for its
  replicas. Their losses and convergence times are different.
- `redis_resources` changes memory only within limits that hold two times
  the data plus 32Mi, because a full sync forks the master. On a
  `maxMemory` instance, only `redis_memory` changes memory.
- With `redis_memory` and `maxmemory_percent`, the smallest limit must
  hold two times the data plus 32Mi. The data is `fill.percent` of
  `maxmemory`, or all of it with `fill_burst`. The operator reserve of
  32Mi bounds the data, not the fork.
- `redis_memory` can go below the memory that the data needs. Under
  `noeviction` and `volatile-*`, the operator then keeps `maxmemory` and
  reports this in `status.message`. Under `allkeys-*`, it applies the
  limit and the server evicts keys.
- `sentinel_reset_kill_master` sends `SENTINEL RESET *` to every Sentinel,
  and then deletes the master pod gracefully. Each Sentinel learns its
  replicas again at its next `INFO` of the master. The recovery path
  differs: a Sentinel failover, or an election by the operator. Any path
  that ends with one master and the status `Healthy` passes. A master that
  stays absent is a timeout.

  The kind runs only on Sentinel instances, and the mutator skips it while
  the instance has one redis pod. Its convergence timeout is
  `observer.convergenceTimeout`, as for `kill_master`. If the shutdown
  script gets no failover, it releases its write pause, and the master
  accepts writes until Redis gets `SIGTERM`. Redis 7 and later then pause
  writes and wait for the replicas, up to `shutdown-timeout`. A write is
  lost only if a replica lags, so the tester does not require a lossless
  result.
- `password_rotate_offline` is scenario C. It stops the operator, changes
  the password, starts the operator with its replicas, expects
  `unable to apply the configured password`, sets the previous password,
  expects `Healthy`, and changes the password again. It stops all other
  mutations while it runs. It always starts the operator again.
- `reset` deletes the RedisFailover, its volumes and its auth Secret, and
  creates it again from its template.

A result is `converged`, `timeout`, `rejected` (the API refused the
change) or `skipped` (the mutation cannot apply now, for example a replica
kill on an instance with one pod). A version change that does not converge
gets its judgement below, not `timeout`.

### Server versions

`versions` names each server version, pinned to an exact patch tag.
`edges` is the transition graph, with the expectation `ok`, `fail` or
`unknown` of each edge. Downgrades are not edges, because an older server
cannot load a newer RDB. Thus a chain goes back to its start with a
`reset`. The `chain` of an instance is the part of the graph that it moves
through.

The instances `redis-chain-62` and `redis-chain-62-sent` (with Sentinels)
start on `redis-6.2`, the oldest Redis version in the graph. They move
through `redis-7.2` and `redis-7.4` to `redis-8`, and then reset on
`redis-6.2`. The edge `redis-6.2 -> redis-7.2` is `ok`, because Redis 7.2
reads the RDB data of Redis 6.2.

`image_upgrade` follows an edge from the current version. The tester
observes a change along an `unknown` or `fail` edge for the timeout of the
kind, or for one more minute after a pod on the new version logged that it
could not load the data. It then judges the change:

- `ok`: it converged.
- `failed_safe`: it did not converge, but the single master still runs the
  old version, accepts writes, and lost no acknowledged write.
- `failed_unsafe`: all other cases. This is a finding.

A change that did not converge is reset immediately.

### Chaos

The chaos lane (`chaos.kinds`) does one action at a time on all instances
together: `operator_restart`, `operator_upgrade` (helm, to each other
version in `chaos.upgrade.versions` and back) and `node_drain`. It waits
until all mutations are complete and all instances are quiet. It keeps all
instances in a window while the action runs, and verifies their data
after it.

## Run the tester against an RC

1. Install the RC:
   `helm install redis-operator oci://ghcr.io/saremox/redis-operator/charts/redis-operator --version <tag> -n redis-operator --create-namespace`.
2. Set the tester image to the same tag. `release.yml` pushes
   `ghcr.io/saremox/redis-operator-soak:<tag>` for each tag. The CRD only
   adds fields, so a newer tester can test an older RC.

   ```sh
   cd deploy && kustomize edit set image ghcr.io/saremox/redis-operator-soak:<tag>
   ```

3. Apply the manifests: `kubectl apply -k deploy`. The tester creates each
   instance of `deploy/config.yaml` from `deploy/instances.yaml` in the
   namespace `redis-soak-instances`.
4. If you have a Prometheus Operator, apply the ServiceMonitor, the alerts
   and the dashboard: `kubectl apply -k deploy/monitoring`. Add the label
   that your Prometheus selects, if it selects by a label.

To start again from new instances, delete the namespace
`redis-soak-instances` and apply the manifests again.

The config entry point is `deploy/config.yaml`. The doc comments of the
fields in `internal/config` explain each option and its default.

The Role in `deploy/rbac.yaml` must be in the operator namespace and must
name the operator Deployment, as `operator` in `deploy/config.yaml` does.

### Enable the chaos lane

The chaos lane is off in `deploy/config.yaml`, and its rights are in the
opt-in kustomize component `deploy/chaos`. helm can create the
ClusterRoles of the chart only with `escalate` and `bind`, so the component
grants them. **This is equal to cluster-admin.** Use the component only on
a cluster that exists for the soak test.

1. Add `components: [chaos]` to `deploy/kustomization.yaml`.
2. Add the chaos kinds to `deploy/config.yaml`.
3. For `operator_upgrade`, list two releases in `chaos.upgrade.versions`.
   The installed release must be the first one. A chart before 4.2.0-rc2
   cannot run its CRD hook, because its ConfigMap is larger than 1MiB.

## Run the kind e2e

```sh
make build test lint
make check-monitoring                 # promtool on the alerts and their tests
make kind-e2e                         # E2E_PROFILE=full
make kind-e2e E2E_PROFILE=versions    # the server version instances
make kind-e2e E2E_PROFILE=chaos       # the chaos lane on three nodes
```

`kind-e2e` uses `.claude/skills/kind-cluster/`. It installs the operator
of this checkout, or the release `OPERATOR_VERSION`. Each profile is a jq
program in `e2e/` over `deploy/config.yaml`. After the mutations, the
script asserts these conditions from the metrics:

- every path and client style writes and reads, and every invariant holds;
- every enabled kind converged, and none timed out or was rejected;
- no finding and no unexpected lost write occurred;
- versions: every edge was taken, and every `ok` edge ended `ok`;
- chaos: every action converged, and no action found a change in progress.

The script keeps the logs of the tester and the operator, the events and
the last scrape in `bin/kind-e2e-artifacts/`. The workflow
`.github/workflows/soak-e2e.yml` runs it on demand against a release.

## Alerts and dashboard

`deploy/monitoring/prometheusrule.yaml` has these alerts. "Outside a
window" is `redis_soak_window_open == 0`.

- `RedisSoakNotWritable`: a path is not writable for 1 minute outside a
  window.
- `RedisSoakMasters`: an instance has no master or more than one outside a
  window.
- `RedisSoakFinding`: a finding.
- `RedisSoakLostWritesLossless`: an unexpected lost write.
- `RedisSoakMutationTimeout`, `RedisSoakChaosTimeout`: a change did not
  converge in time, or failed.
- `RedisSoakFollowerAuthOutage`: the operator did not apply a password
  change within 2 minutes.
- `RedisSoakVersionTransitionFailed`: a version change along an `ok` edge
  did not converge.
- `RedisSoakVersionTransitionUnsafe`: a version change failed unsafely.

The series of the counters start at 0, so that `increase()` shows the
first event. The dashboard `deploy/monitoring/dashboard.json` shows the
findings and the unexpected losses of the run, and for each instance its
probes, invariants, window and mutations. Read a red part of `writable`
together with the window: inside a window, it is the cost of the change.

## Limits

- The tester does not check two documented operator limits: until a redis
  pod restarts after a password change, its exporter and its pre-stop
  `SAVE` use the old password; and an operator that restarts between a
  Secret change and its next check needs the recovery of scenario C.
- `config` cannot check Sentinel keys that `SENTINEL MASTER` does not
  report, for example `auth-pass`, nor `requirepass`, `masterauth` and
  `aclfile`, which the operator sets itself.
- On node-local volumes, an evicted pod can start again only on its node.
  Thus a drain cannot converge before the uncordon.
- The tester does not compare `currentRevision` of the StatefulSet: for
  `OnDelete`, the controller never advances it.
- Run one tester for each cluster. Two testers write the same keys.
