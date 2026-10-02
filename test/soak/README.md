# Soak tester

The soak tester tests redis-operator release candidates (RCs) for hours or
days. It runs in the cluster next to an installed operator. It changes a
fixed set of RedisFailover instances continuously, and probes and observes
them. It exports the results as Prometheus metrics, so you can judge an RC
from a dashboard.

The tester is a separate Go module. It builds against the operator API
through a `replace` of the repository root, so a tester built from a tag
always uses the spec of that tag.

## What the tester does

### Probes

The tester probes the master of each configured instance once per
`probe.interval`, through each path of the current mode of the instance:

- `sentinel` (Sentinel mode only): the go-redis failover client asks the
  Sentinels behind `rfs-<name>:26379` for `mymaster` and follows their
  failover announcements, as a Sentinel-aware application does.
- `rfrm` (all instances except a bootstrapping instance): the
  `rfrm-<name>` Service.
- `rfrs` (bootstrapping instances only, read-only): the `rfrs-<name>`
  Service, because all pods are replicas.

The paths follow `sentinel.enabled` of the RedisFailover, not the config.
The tester adds `sentinel` when all Sentinels are Ready and agree on the
master after a change to Sentinel mode. It removes `sentinel` and its
series when the mode is no longer Sentinel. It also removes `sentinel` when
a `reset` recreates the RedisFailover, because the new Sentinels are behind
a new Service. A path that the tester adds gets new probers.

The tester probes each path with four client styles:

- `pooled`: one long-lived go-redis client, as an application uses.
- `retrying`: `pooled` with the default go-redis retries, as an application
  with an untuned client.
- `fresh`: a new client for each probe, as a new pod. On the `sentinel`
  path, this includes the query to a Sentinel for the master.
- `follower`: `pooled`, but the tester replaces the client when the auth
  Secret changes, as an application that reads its Secret again. The
  `auth` failures of `follower` show how long the operator takes to apply
  a password change.

Each probe is a `SET soak:<rf>:<path>:<client>:seq <n>` and then a `GET` of
the same key. Go-redis retries are off for all styles except `retrying`, so
each failure counts. `retrying` retries within the same `probe.timeout`.
When the sequence number is a multiple of `probe.waitEvery` (default 10)
and the `SET` succeeded, the probe also sends
`WAIT 1 <probe.waitTimeout>` (default: half of `probe.timeout`). The tester
records it as `op="wait"`, and the number of replicas that acknowledged it
as `wait_acked_replicas`. On `rfrs`, a probe only reads the key that the
pooled `rfrm` probe of the source instance writes. A missing key is not a
failure, because the write can still be on its way to the replica.

### Auth

When the RedisFailover has `spec.auth.secretPath`, each connection of the
tester authenticates: the probes, the observer, the mutator checks, the
filler, the ledger, the verifications and the burst. The Sentinels need no
password. The tester reads the password from the Secret through an
informer on the Secrets of the instance namespace. It reads `secretPath`
from the RedisFailover. The go-redis credentials provider gives each new
connection the current password. Open connections keep their
authentication, because Redis keeps them when its password changes.
Passwords are never in logs, metrics or errors.

### Data

For each instance with a `data` section:

- **The filler** keeps the `used_memory` of the master at `fill.percent` of
  `maxmemory`, or at `fill.sizeMi` without `maxmemory`. It adds data again
  after evictions and resets. It writes `soak:<rf>:fill:<n>` through `rfrm`
  in pipelines of `fill.batch` keys, at a maximum of `fill.keysPerSecond`,
  with an optional `fill.ttl`. Each value comes from its key only, so the
  tester can verify any key without stored state.
- **The ledger** (`data.ledger`) writes `soak:<rf>:ledger:<seq>` at
  `writesPerSecond`. It records each acknowledged seq as compact ranges.
- **A verification** reads these keys directly from the master pod:
  - all ledger writes acknowledged since the previous verification;
  - a sample of `ledger.sampleKeys` of the writes that the previous
    verification checked. If the sample lost a key, the verification reads
    all of them, so that later events do not get this loss;
  - a sample of `fill.sampleKeys` fill keys. A missing fill key counts only
    if it cannot be evicted or expired: no TTL and no `allkeys-*` policy.
    After a fill loss, the tester forgets the older fill keys, because the
    sample cannot tell how many more are lost.

  Each missing or wrong key counts once in `lost_writes_total{event}`, and
  the tester logs it as `lost writes` with the seq ranges. The tester then
  deletes and forgets the ledger keys that are older than the previous
  verification. This limits the memory of the tester and the number of
  keys in the instance.

Verifications run at these times:

- after each mutation (`event` is the kind);
- after each failover that the observer sees outside a mutation
  (`failover`);
- every `ledger.verifyInterval` without one of these (`periodic`).

A failover that loses the data by design is a `reset`. This occurs when a
pod that never held the data replaces the only pod of an instance without
a PersistentVolumeClaim, or when the RedisFailover is recreated. A failover
to a pod that was a replica with its link up, for example after a
scale-down, is a `failover`, and its losses count. A `reset` mutation
forgets all writes from before the recreation, so its verification counts
only the later writes. The tester logs each verification as
`data verified`.

Asynchronous replication can lose writes, so `lost_writes_total` is a value
to monitor, not a finding.

The ledger keys must never be evicted, because the tester cannot tell their
loss from an eviction. Config validation rejects a ledger on an instance
that can run under `allkeys-*`: its `maxMemoryPolicy`, or a policy that
`maxmemory_policy` can set. It also rejects a ledger under `volatile-*`
without `fill.ttl`, so that only fill keys can be evicted. Instances under
`allkeys-*` run the filler only. The ledger also stops while the master
runs an `allkeys-*` policy that a person set.

`oom_rejections_total` counts the fill, ledger and burst writes that the
server rejects with `OOM`. At startup, the tester deletes the ledger and
burst keys of an earlier run, because it can never verify or delete them.

A **bootstrapping instance** (`bootstrap.source`) is read-only: it has no
filler, no ledger and no write probes. Its verification does these steps:

1. It takes a sample of `bootstrap.sampleKeys` of the source ledger writes
   acknowledged since the last verification of the source. If there are
   none, it waits for some.
2. It reads these keys from the source master, and records the
   `master_replid` and `master_repl_offset` of that master.
3. It reads the keys that the source master holds from each pod, when the
   pod caught up within 30s: the same `master_replid`, the link up, and a
   `slave_repl_offset` at least as far.

The tester does not compare offsets of different replication IDs. After a
new source master starts, a pod that did not sync with it yet can be far
ahead in the old stream. Each key that a caught-up pod does not have
counts in `lost_writes_total`. The verification runs after each mutation
of the instance and every `bootstrap.verifyInterval` (default 1m).

### Invariants

Every `observer.interval` (default 5s), the observer reads the
RedisFailover, its pods and the `rfrm-<name>` EndpointSlices. It sends
`INFO` to each redis pod at its IP, and
`SENTINEL get-master-addr-by-name mymaster` to each Sentinel pod. It then
checks these invariants:

| Invariant | Holds when |
|---|---|
| `pods` | `spec.redis.replicas` redis pods exist and all are Ready. In Sentinel mode, also `spec.sentinel.replicas` Sentinel pods exist and all are Ready. |
| `one_master` | Exactly one redis pod reports the master role in `INFO replication`. Bootstrapping instance: each pod replicates from `bootstrapNode` with `master_link_status:up`, on the stream (`master_replid`) of the current source master. |
| `master_service` | The `rfrm-<name>` EndpointSlices hold exactly one ready address: the address of the master. Bootstrapping instance: no address, because the operator labels no pod as master. |
| `replication` | Each other redis pod is a replica of the IP and port of the master, with `master_link_status:up`. Not checked for a bootstrapping instance. |
| `sentinel_agreement` | (Sentinel mode only) Each Sentinel reports the IP and port of the master. |
| `healthy` | The `status.state` of the RedisFailover is `Healthy`. |
| `config` | `CONFIG GET` on each redis pod with an IP returns what `spec.redis.customConfig` sets. In Sentinel mode, `SENTINEL MASTER mymaster` on each Sentinel returns what `spec.sentinel.customConfig` sets. With `spec.redis.maxMemory`, `maxmemory` and `maxmemory-policy` are the values that the operator sets (see below). |
| `oom_killed` | No container of a redis or Sentinel pod reports an `OOMKilled` termination. |
| `replica_ready_without_data` | No Ready redis pod is a replica whose link is down and was never up since the server started (`master_link_down_since_seconds:-1`). Such a pod never completed a sync, and `rfrs-<name>` sends reads to it without the data of the master. |

If `observer.replicaReadyWithoutData` is `false`, `replica_ready_without_data`
is never a finding, and an instance can be quiet while it is violated. The
tester still evaluates it, exports it as `invariant_ok` and logs it. The readiness
probe of an operator before PR #205 passes such a replica, for example a
Valkey replica that cannot read a Redis 7.4 or 8 RDB. Thus the versions
profile sets it to `false`.

The invariants follow the mode of the RedisFailover: `sentinel.enabled` and
`bootstrapNode`, with the operator defaults applied. When an invariant does
not apply to the mode, for example `sentinel_agreement` after Sentinel is
off, the tester forgets its `invariant_ok` series and any open violation.
The tester accepts the Redis and the Valkey role names: `master`/`primary`
and `slave`/`replica`.

`config` compares `customConfig` as the operator applies it: a later entry
for a key wins, case does not matter, and memory sizes compare by bytes.
It also checks the operator defaults, for example `replica-priority`. It
does not check `requirepass`, `masterauth` and `aclfile`, because the
operator sets them itself or loads them with `ACL LOAD`. It cannot check
Sentinel keys that `SENTINEL MASTER` does not report, for example
`auth-pass`.

For `maxmemory`, `maxmemory-policy` and `replica-ignore-maxmemory`, `config`
calculates the expected values with the operator helpers in
`api/redisfailover/v1` (`MaxMemoryFor`, `CustomConfigSets`,
`ManagedMaxMemoryError`), so that the tester and the operator use the same
rules. If the status message keeps `maxmemory` (`maxmemory kept at X: ...`),
each pod must run `X`.

A pod that does not answer `INFO` has no role. Thus an unreachable master
violates `one_master`, and an unreachable replica violates `replication`.
`master_service`, `replication` and `sentinel_agreement` need a single
master, so a violation of `one_master` also violates them.

### Mutations

For each instance with a `mutations` section, the mutator applies one
mutation at a time:

1. It waits until the instance is quiet: no convergence window is open and
   all invariants hold.
2. It picks a kind by weight, and the parameters of the kind, from the seed
   and the step of the instance. Each instance and step has its own random
   source, so you can replay a logged step from its seed.
3. It takes the global lock. The lock is shared for most kinds, and
   exclusive for `password_rotate_offline`, because that kind stops the
   operator for all instances. An exclusive mutation waits until all other
   mutations are complete, and all other mutators wait until it is
   complete. The mutator then checks again that its instance is quiet.
4. It opens a convergence window and applies the mutation.
5. It waits until the window closes or times out, and records the result.
6. It waits for `mutation.interval` plus up to `mutation.jitter`.

| Kind | Mutation | Converged when |
|---|---|---|
| `redis_replicas` | Merge-patches `spec.redis.replicas` to a different value in `redisReplicas`. | The `rfr-<name>` StatefulSet has the new `spec.replicas` and observed its generation. It has that number of replicas, all ready and on its update revision. That number of redis pods exist, all Ready. |
| `sentinel_replicas` | Merge-patches `spec.sentinel.replicas` to a different value in `sentinelReplicas`. | The `rfs-<name>` Deployment observed its generation and has that number of replicas, all ready, updated and available. That number of Sentinel pods exist, all Ready. |
| `redis_resources` | Merge-patches the cpu, the memory or both of the redis container requests and limits to different values in `resources`. It changes only values that the RedisFailover already sets, and skips a change to the QoS class, so that the operator can resize the pods in place. | Each redis pod runs the new values, as the kubelet reports them, and no resize is pending. The StatefulSet converged as for `redis_replicas`. |
| `kill_master` | Deletes the pod with the label `redisfailovers-role=master` gracefully, if the observer last saw it as the single master. | A new pod with the same name is Ready. |
| `kill_master_force` | Same as `kill_master`, with `GracePeriodSeconds=0`. | Same as `kill_master`. |
| `kill_replica` | Deletes a pod with the label `redisfailovers-role=slave`. | A new pod with the same name is Ready. |
| `kill_sentinel` | Deletes a Sentinel pod. | The pod is gone and the Deployment converged as for `sentinel_replicas`. |
| `redis_memory` | Merge-patches the memory limit to a different value in `redisMemory` (Mi), and the memory request by the same ratio, so the QoS class does not change. The range can go below the memory that the data needs. | `maxmemory` is the value that the operator sets (as for `config`). Each pod runs the new values (as for `redis_resources`). Without an `allkeys-*` policy, this alternative is also converged: the status keeps `maxmemory` because the new target is too small for the data, all pods are ready and no pod shrank. |
| `maxmemory_policy` | Merge-patches `spec.redis.maxMemory.policy` to a different value in `maxMemoryPolicies`. | The spec has the change. Each redis pod is ready and runs the `maxmemory` settings that the operator sets. |
| `maxmemory_percent` | Merge-patches `spec.redis.maxMemory.percent` to a different value in `maxMemoryPercent` (10-95). | Same as `maxmemory_policy`. |
| `fill_burst` | (`noeviction` only) Writes `soak:<rf>:burst:<n>` past `maxmemory` until the server rejects writes with `OOM`. Continues to write for `fillBurstHold`, then deletes the burst keys. | The burst is complete, all redis pods are ready, and `rfrm` accepts a write again. |
| `password_rotate` | (With auth) Sets the `password` key of the Secret that `spec.auth.secretPath` names to a new random password. | Each redis pod accepts the new password and runs the new `secret-checksum` of the pod template. The StatefulSet converged as for `redis_replicas`. In Sentinel mode, each Sentinel sees the master without `s_down`, `o_down` or `disconnected` flags, and sees all replicas. The RedisFailover is `Healthy`. |
| `auth_add` | (Without auth) Creates or updates the Secret `authSecret` (default `<name>-auth`) with a new random password. Then merge-patches `spec.auth.secretPath` to it. | Same as `password_rotate`. |
| `auth_remove` | (With auth) Removes `spec.auth.secretPath` with a merge patch. | Same as `password_rotate`, and each pod needs no password. |
| `password_rotate_offline` | (With auth; scenario C) Runs 7 phases: (1) scales the operator to 0 and waits until its pod is gone, (2) rotates the password, (3) scales the operator to 1, (4) expects `NotHealthy` with `unable to apply the configured password`, (5) sets the previous password again, (6) expects `Healthy` with all pods on that password, (7) rotates again. Each expectation waits up to `observer.convergenceTimeout`. | Same as `password_rotate`, for the last password. |
| `image_upgrade` | (Chain instances) Merge-patches `spec.redis.image` along an edge of the chain from the current version. Also changes `spec.sentinel.image` if the Sentinels of the chain follow. The random source of the step picks the edge. At the end of the chain, it does a `sentinel_image_upgrade` while separate Sentinels are behind, and then a `reset`. | Each redis pod (and each changed Sentinel pod) runs the new image and reports its server and version in `INFO server`. The StatefulSet (and the Deployment) converged. The RedisFailover is `Healthy`. See [Versions](#versions) for edges that can fail. |
| `sentinel_image_upgrade` | (Chains with `sentinel: separate`) Merge-patches `spec.sentinel.image` along an edge from the Sentinel version towards the version of the redis image. When the Sentinels run that version, it does an `image_upgrade`. | Same as `image_upgrade`, for the Sentinel pods. |
| `reset` | (Instances with a template) Deletes the RedisFailover and waits until it, its StatefulSet, its Sentinel Deployment and its pods are gone. Deletes its PersistentVolumeClaims and the auth Secret that its template names. Creates the RedisFailover and the Secret again on the next start version of the chain, with a new password. | The RedisFailover has a new UID. Each pod runs the start version (as for `image_upgrade`). The RedisFailover is `Healthy`, and the filler reached its target again. |
| `sentinel_image_flip` | (Sentinel mode only) Merge-patches only `spec.sentinel.image` to a different value in `sentinelImages`, for example between a Redis and a Valkey version for a mixed pair such as `mixed-sent`. It is not an edge of the graph, because Sentinels hold no data. Its mixed window counts as `version_mixed_seconds{component="sentinel"}`. | Each Sentinel pod runs the new image and reports its version in `INFO server`. The Deployment and the StatefulSet converged. The RedisFailover is `Healthy`. |
| `sentinel_toggle` | Merge-patches `spec.sentinel.enabled` to the other value. | On: the Sentinel Deployment converged as for `sentinel_replicas`, and the `rfs-<name>` Service and ConfigMap exist. Off: the Deployment, the Service, the ConfigMap and all Sentinel pods are gone. In both cases, all redis pods are Ready and the RedisFailover is `Healthy`. |

`redis_memory`, `maxmemory_policy`, `maxmemory_percent` and `fill_burst`
need `maxMemoryPolicy`. Under `allkeys-*`, `redis_memory` expects that the
operator applies the limit and decreases `maxmemory`, and that the server
evicts keys. Under `noeviction` and `volatile-*`, a target below the data
makes the operator keep `maxmemory`, report this in `.status.message` and
stop the rollout.

On instances with data, `redis_resources` changes memory only where this
is safe for the data:

- On `maxMemoryPolicy` instances, it can change only cpu. Their data follows
  the limit, and only `redis_memory` knows what the operator does then.
- On other instances, the data is a fixed `fill.sizeMi`. The memory limit
  range must start at two times that value plus 32Mi. A full sync forks the
  master, and the copy-on-write pages can double the data. The 32Mi is the
  reserve of the operator.

The mutator waits until the filler filled the data once before its first
mutation.

`kill_master` and `kill_master_force` are two kinds, not one kind with a
label, because their convergence times and losses are different. A forced
delete removes the pod object immediately and skips the preStop hook. The
master then gets only the 2s kubelet minimum between SIGTERM and SIGKILL,
so it cannot wait for its replicas, and its replacement starts
immediately. The `kind` label already splits `mutation_total`,
`mutation_converge_seconds`, `pods_recreated_total` and
`lost_writes_total`, and a new label must be on all other series too.

Kills delete with a UID precondition. Replica and Sentinel kills use
`GracePeriodSeconds=0` with the probability `forceDeleteProbability`. The
tester verifies a master kill of the only pod of an instance without a
PersistentVolumeClaim as a `reset`.

The auth kinds never log a password: `params` names the Secret.
`password_rotate_offline` logs each phase as `scenario phase done` (or
`scenario phase failed`) with `phase`, `phase_name` and
`duration_seconds`. Phase 8 is the convergence of the last rotation. If an
expectation does not occur within its limit, the result is `timeout`. The
scenario always runs to the end, so it never leaves the operator stopped.

A bootstrapping instance can run only `redis_replicas`, `redis_resources`
and `kill_replica`.

The tester does not compare the `currentRevision` of the StatefulSet. The
operator StatefulSets use `OnDelete`, and for `OnDelete` the controller
never advances `currentRevision`.

The mutator logs each mutation as `mutating` and then `mutation done` (or
`mutation skipped`), with `kind`, `seed`, `step`, `params` and `result`.

`result` is one of these values:

- `converged`: the window closed.
- `timeout`: the window timed out after the mutator applied the mutation.
  The timeout is `observer.convergenceTimeout` or the `mutation.timeouts`
  entry of the kind. For a version change along an edge that can fail, see
  [Versions](#versions); its `version transition` log line judges it.
- `rejected`: the API refused the patch, the delete or the Secret change.
  The window closes when the invariants hold.
- `skipped`: the mutator could not apply the mutation, for example a
  replica kill on an instance with one redis pod, or a resource change
  that alters the QoS class. The mutator changed nothing and opened no
  window.

### Instances from templates

The tester owns an instance with a `template`. The template is a
RedisFailover manifest next to the config file, in the same ConfigMap. At
startup, the tester creates the instance from the template if the instance
does not exist. It also creates the auth Secret with a random password if
the template names one and the Secret does not exist. `version` and
`sentinelVersion` set the redis and Sentinel images by version name. A
chain instance starts on its first start version. A `reset` recreates the
instance completely. The namespace must exist.

### Versions

`versions` names each server version in the rotation, pinned to an exact
patch tag (`redis:7.2.16-alpine`, `valkey/valkey:9.1.2-alpine`). Labels use
these names, never tags. `edges` is the transition graph: `from`, `to` and
`expect` (`ok`, `fail` or `unknown`). Downgrades are not edges, because an
older server cannot load a newer RDB. Thus a chain goes back to its start
with a `reset`. Config validation rejects unknown versions, floating tags,
self-edges, downgrades within a server and duplicates.

The `chain` of an instance is the part of the graph that the instance
moves through:

- `start`: the start versions, one for each reset, in turn.
- `versions`: the versions that the instance can run.
- `expect`: the expectations of the edges that it takes (default: all).
- `sentinel`: `follow` changes the Sentinel image with the data image.
  `separate` leaves the Sentinel image to `sentinel_image_upgrade`, which
  follows the data image. Empty keeps the Sentinel image of the template.

Each start must have an edge, and each version must be reachable from a
start.

A version change along an `ok` edge converges as in
[Mutations](#mutations), within the timeout of the kind. The tester
observes a change along an `unknown` or `fail` edge until it converges, for
a maximum of the `timeout` of the edge (default: the timeout of the kind).
If a pod on the new version logs that it could not load the data
(`rollout stuck`), the tester observes for one more minute only. It then
judges the change and keeps the window open. If the change did not
converge, the tester resets the instance immediately, and the reset takes
the window. Thus the stuck pods are not findings. Each change gets one of
these results:

- `ok`: it converged.
- `failed_safe`: it did not converge, but the rollout stopped and kept the
  data. The single master still runs the old version and accepts a write to
  its pod. Its log has no line that says that it could not load the data.
  The verification found no lost acknowledged write.
- `failed_unsafe`: all other cases. This is a finding of
  `version_transition`.

The tester logs the result as `version transition`. For a change that did
not converge, the log line also has the master and its version, whether it
accepts writes, the `reasons`, and the state of each pod of the changed
image. The pod state includes the log line about the data load (for
example `Can't handle RDB format version 12`) from its current or previous
container.

The mixed window starts when the first pod runs the new image and reports
the new version in `INFO server`. It ends when no pod runs the old image,
or when a reset deleted the instance. The tester samples it every
`observer.interval` and logs it as `mixed versions`. The outages and lost
writes during a change belong to the mutation:

- the outages that started between its `mutating` and its
  `version transition` log lines;
- `lost_writes_total{event="image_upgrade"}` (or `sentinel_image_upgrade`),
  and `reset` for the reset after it.

### Chaos

The chaos lane (`chaos` in the config) does cluster-level actions, one at
a time, on all instances together. Every `chaos.interval` plus up to
`chaos.jitter`, it picks a kind by weight from `mutation.seed` and its
step. It stops at `mutation.stopAfter`. For each action, the lane does
these steps:

1. It takes the global lock exclusively. It waits until all mutations are
   complete, and all mutators wait until the action is complete.
2. It waits until all instances are quiet, for a maximum of
   `chaos.timeout`. It logs the instances that are not quiet as
   `in_flight`. The lock must leave none.
3. It marks the data of each instance as in a change. It disturbs all
   instances while the action runs, so the observers keep their windows
   open, as for scenario C.
4. It runs the action. The action waits for its own convergence, each wait
   for a maximum of `chaos.timeout`.
5. It verifies the data of each instance. The `event` is the kind, or
   `reset` for an instance whose only redis pod, without a volume, the
   action deleted.

| Kind | Action | Converged when |
|---|---|---|
| `operator_restart` | Deletes the operator pods gracefully. | A new Ready pod holds the operator Lease (`operator.lease`). Then, without the disturbance, all instances are quiet: all invariants hold, `healthy` included. |
| `operator_upgrade` | Runs `helm upgrade` of `chaos.upgrade.release` to each other version in `chaos.upgrade.versions` in turn, and back to the version that the operator ran. Each upgrade uses `--reset-then-reuse-values`, the image of the version as `image.repository` and `image.tag`, `chaos.upgrade.set`, and `--wait`. helm runs the pre-upgrade CRD hook of the chart first, and fails the upgrade if the hook fails. | For each upgrade: helm succeeded, the Deployment rolled out the image of the version and has no other pods, and a pod on that image holds the Lease. Then all instances are quiet. |
| `node_drain` | Picks a Ready, schedulable node of `chaos.drain.nodeSelector` (default: all nodes except the control plane), never the node of the tester. Cordons it and evicts all pods that `kubectl drain --ignore-daemonsets` evicts, all at the same time. While a PodDisruptionBudget refuses an eviction, it tries again every 2s, for a maximum of `chaos.drain.timeout`. It keeps the node cordoned for `chaos.drain.hold`, then uncordons it. | Drained: no such pod is on the node, and all invariants of all instances hold on the other nodes. After the uncordon: all instances are quiet again. |

The time without an operator, for a restart or an upgrade, starts when the
old leader is deleted. It ends when a new leader acquires the Lease
(`chaos_operator_down_seconds`).

An upgrade logs `operator upgraded` for each version, with the helm time,
the result and duration of the CRD hook, and the CRD generation before and
after the hook. The hook result (`succeeded`, `failed` or `not seen`) comes
from its Job, which helm deletes when the hook succeeds. A drain logs
`eviction blocked` and `eviction unblocked` for each pod
(`chaos_eviction_blocked_seconds`), then `node drained` and
`node uncordoned`.

`result` (`chaos_total`, logged as `chaos done`) is one of these values:

- `converged`.
- `timeout`: the operator or an instance did not converge in time, or a
  budget still blocked an eviction at the drain timeout.
- `failed`: an API call or helm failed.
- `skipped`: no operator pod, no node to drain, or the operator runs no
  configured version.

`chaos_converge_seconds` starts at the delete of a restart, the start of
each upgrade, or the cordon of a drain. It ends when all instances
converged. It does not include the hold of a drain.

On node-local volumes (the default StorageClass of kind, local disks), an
evicted pod can start again only on its node. Thus a drain cannot converge
before the uncordon, and its first wait times out.

### Findings

A violation is expected while the instance converges after a change.
Otherwise, it is a **finding**. A convergence window opens in one of these
ways:

- **A mutation** opens a window before the mutator applies it. The window
  closes when the convergence signal of the mutation holds (see
  [Mutations](#mutations)), all invariants hold (`healthy` included), and
  `mutation.minDwell` (default 15s) passed after the mutator applied the
  mutation. The dwell prevents a converged result before the operator sees
  the change. The window times out after `observer.convergenceTimeout`
  (default 10m), or after the `mutation.timeouts` entry of the kind:
  `base` plus `perPod` for each redis pod. A change of
  `metadata.generation` in the window does not extend or restart it. An
  operator before PR #207 has no status subresource, so each of its status
  updates increases the generation.
- **A change of `metadata.generation`** outside the window of a mutation
  opens a window, for example a manual spec change. The window closes when
  all invariants hold, or at the convergence timeout. A later change
  restarts it.
- **A disturbance outside the instance** keeps a window open while it
  continues. The tester restarts the window at each check (for the window
  of a mutation: its timeout). These are the disturbances:
  - `password_rotate_offline` stopped the operator.
  - A chaos action disturbs the cluster (all instances).
  - For a bootstrapping instance: the source is in a window or has no
    single master, because the replication link breaks when the source
    fails over.

  The window then closes when all invariants hold. For a bootstrapping
  instance, a change of the source master also opens a window. The link
  of its pods to the old master can break after the source converged.
  `one_master` holds again when all pods replicate the stream of the new
  master.

The tester judges violations with these rules:

- A violation that starts while no window is open is a finding.
- A violation that is still open when its window times out becomes a
  finding at that time.

Each finding increments `findings_total{invariant}` once. An OOM kill is a
finding of `oom_killed` also inside a window: the 32Mi reserve of the
operator exists so that no data size that the operator allows causes an
OOM kill. The tester also opens a window when it first sees an instance,
because a change can be in progress.

Pod kills open a window as all mutations do, so the failover after a
master kill is not a finding. A pod that something else killed, or a
failover that nobody requested, is a finding.

The tester does not check known, documented operator limitations:

- Until a redis pod restarts after a password change, its exporter sidecar
  and its pre-stop `SAVE` use the old password.
- If the operator restarts between a Secret change and its next check, it
  reports `unable to apply the configured password` until the documented
  recovery. `password_rotate_offline` runs this recovery.

The tester logs each violation as `invariant violated` (with `finding`)
and `invariant restored`, each failover as `failover`, and each window as
`convergence window opened`, `closed` or `timed out`. When the window of a
mutation times out, the log line also tells why the mutation did not
converge.

## Run the tester

```sh
make build test lint      # binary in bin/soak
make image                # ghcr.io/saremox/redis-operator-soak:<git describe>
make kind-e2e             # kind cluster, operator, instances, mutating tester, assertions
make kind-e2e E2E_PROFILE=versions   # the server version and fork instances only
make kind-e2e E2E_PROFILE=chaos      # the chaos lane on three nodes, with Prometheus and Grafana
make check-monitoring                # promtool on the alerts, and their unit tests
```

`kind-e2e` uses `.claude/skills/kind-cluster/`. It installs the operator
from this checkout with `charts/redisoperator`, recreates its instances,
and deploys the tester with [`e2e/config.yaml`](e2e/config.yaml):

| Instance | Mode | Storage, auth | maxMemory | Data | Mutations |
|---|---|---|---|---|---|
| `op-basic` | operator | emptyDir | none | 32Mi, ledger | `redis_replicas`, `redis_resources`, `kill_master`, `kill_master_force`, `kill_replica` |
| `sent-basic` | Sentinel | emptyDir | none | 32Mi, ledger | `redis_replicas`, `sentinel_replicas`, `kill_master`, `kill_master_force`, `kill_replica`, `kill_sentinel` |
| `op-maxmem` | operator | emptyDir | `allkeys-lru`, 75% of 256Mi | 90% of `maxmemory`, filler only | `redis_memory`, `maxmemory_policy` (`allkeys-*`), `maxmemory_percent`, `kill_replica` |
| `op-noevict` | operator | emptyDir | `noeviction`, 75% of 192Mi | 70% of `maxmemory` (fill TTL 1h), ledger | `redis_memory`, `maxmemory_policy` (`noeviction`, `volatile-*`), `maxmemory_percent`, `fill_burst` |
| `op-full` | operator | PVC (default StorageClass of kind), auth; exporter, `preventMasterEviction` | none | 16Mi, ledger | `password_rotate`, `auth_remove`, `auth_add`, `password_rotate_offline`, `kill_master`, `kill_master_force`, `redis_replicas` (1-2) |
| `sent-full` | Sentinel | emptyDir, auth; port 6380, both exporters, redis and Sentinel `customConfig` | none | 16Mi, ledger | `password_rotate`, `kill_master`, `kill_sentinel`, `sentinel_replicas` |
| `toggle` | operator at first | emptyDir, auth | none | 16Mi, ledger | `sentinel_toggle`, `kill_master` |
| `bootstrap` | operator, `bootstrapNode` = ClusterIP of `rfrm-op-basic` | emptyDir | none | read-only | `redis_replicas`, `kill_replica` |

The convergence timeout of the observer is 8m. It is longer than the 5m
that the operator retries an in-place resize that the kubelet refuses (for
example a memory limit below the page cache in use) before it recreates the
pod. The mutator runs with an interval of 10-20s and seed 24080. This seed
picks each enabled kind within the first eight steps of each instance, and
within the first twelve steps of `op-full`. On `op-full`, it rotates the
password with three pods, runs scenario C on step 5 and restarts the single
pod gracefully on step 8. It kills the only pod of `op-basic` (a `reset`)
and the master of `toggle` in both modes. It starts mutations for
`DURATION` seconds (default 2100). When the last mutations converged, the
script asserts these conditions from `/metrics` and the tester log:

- Each enabled kind converged at least once on each instance, and no kind
  timed out or was rejected.
- `findings_total` stayed 0, OOM kills included.
- All invariants hold, `config` and `oom_killed` included, with one master
  (none for the bootstrapping instance). `server_info` has a series for each
  redis pod.
- Each path and client style can write and read again, and `WAIT`
  succeeded on each. The `rfrs` path of the bootstrapping instance can read
  again.
- Each instance whose master was killed failed over.
- `op-noevict` rejected writes with `OOM`, and `op-maxmem` evicted keys.
- The tester verified the data after each mutation, and after each failover
  outside a mutation. It verified the bootstrapping instance, and that
  instance had all writes of `op-basic`.
- No write was lost by `password_rotate`, `auth_add`, `auth_remove`,
  `sentinel_toggle` or `password_rotate_offline`. No write was lost by a
  graceful `kill_master` (a restart of a single pod included) or a
  scale-down on the volumes of `op-full`. The script does not include a
  scale-down that removed the master, and reports it separately. This is
  an operator race: case 1 of `checkAndHealOperatorManagedMode` in
  `checker.go` promoted a replica without a check of `masterPodStopping`.
  PR #210 fixes it.

The script then prints a report: the mutations and their convergence
times, the outages for each mutation, path and client style, the auth
windows, the phases of scenario C, the bootstrap lag, the verifications,
the lost writes, the data metrics and any findings. A `follower` `auth`
outage in an auth window is the delay of the operator. The report flags a
loss on a graceful path as `NOTABLE`, because Redis 7 waits for its
replicas on SIGTERM.

The script keeps the logs of the tester and the operator, the events of the
instances, the lag samples of the bootstrapping instance and the last scrape
in `bin/kind-e2e-artifacts/`. To install a released chart and image
instead, set `OPERATOR_VERSION=4.2.0-rc2`.

### The versions profile

`E2E_PROFILE=versions` runs only the version and fork instances of
[`e2e/config-versions.yaml`](e2e/config-versions.yaml). The node (4 CPUs,
16GiB) can hold these instances only without other instances, so the script
first deletes the namespaces of the other profile. The tester creates the
instances from the `rf-*.yaml` templates next to the config. Each instance
has 2 redis pods, 8Mi of data and a ledger at 10 writes/s. The script first
pushes the image of each version into the local registry, because the node
cannot pull images and the operator pulls with `Always`.

| Instance | Mode | Storage, auth | Versions | Mutations |
|---|---|---|---|---|
| `redis-chain` | operator | PVC, auth | Redis 7.2 -> 7.4 -> 8, reset | `image_upgrade`, `redis_replicas` (1-2), `redis_resources` (cpu) |
| `redis-chain-sent` | Sentinel | emptyDir, auth | the same, Sentinels follow | `image_upgrade` |
| `migrate` | operator | PVC, auth | Redis 7.2 -> Valkey 7.2, 8 or 9 -> ... -> Valkey 9, reset | `image_upgrade` |
| `migrate-sent` | Sentinel | PVC, auth | the same; data first, then the Sentinels | `image_upgrade`, `sentinel_image_upgrade` |
| `edge` | operator | emptyDir | Redis 7.4 or 8 in turn, the unknown edges into Valkey, reset | `image_upgrade` |
| `valkey-op` | operator | PVC, auth; exporter, `allkeys-lfu` 50% filled | Valkey 9 | `redis_replicas`, `redis_resources` (cpu), `redis_memory`, `kill_master`, `kill_master_force`, `kill_replica` |
| `valkey-sent` | Sentinel | emptyDir, auth | Valkey 8 data and Sentinels | `password_rotate`, `kill_master`, `kill_sentinel`, `sentinel_replicas` |
| `mixed-sent` | Sentinel | emptyDir | Redis 7.2 data, Valkey 9 Sentinels | `kill_master`, `kill_replica`, `kill_sentinel` |

The timeout of `image_upgrade` is 3m plus 90s for each redis pod. The
timeout of a reset is 3m plus 1m for each pod. The tester judges a change
along an unknown edge one minute after the first replaced pod could not
load the data. Seed 9 takes each edge of the graph within the first 14
steps of `edge`, and each chain completes and resets early.

The script asserts these conditions:

- Each edge was taken, and each ok edge ended `ok`.
- No change failed unsafely, and each chain reset.
- Each other kind converged without timeouts.
- There were no findings, all invariants hold, and each probe path and
  client style works.
- No rollover along an ok edge lost writes on the instances with volumes.

The script reports each edge with its convergence time, mixed window,
losses and outages (`transitions.txt` in the artifacts). It also reports
what the changes that did not converge left behind, and the known operator
issues that it saw.

### The chaos profile

`E2E_PROFILE=chaos` runs the chaos lane of
[`e2e/config-chaos.yaml`](e2e/config-chaos.yaml) on a control plane and
two workers. The `kind-up.sh` script of the skill makes one node, so the
script makes the cluster the same way with workers added. Each node mounts
one host directory as `/shared`. A clone of the kind local-path provisioner
serves the `shared-path` StorageClass from it. Thus a pod moves to a
different node with its volume, as on network storage. CoreDNS gets a
budget of one, as in most clusters.

The script builds the operator from source two times: `UPGRADE_FROM`
(default `4.2.0-rc2`) and `UPGRADE_TO` (default `origin/main`). The sandbox
cannot pull from ghcr.io, so the script serves each image through the local
registry and packages each chart into the ConfigMap of the tester. It
installs the first version with the CRD hook. With `OPERATOR_VERSION`, both
are released OCI charts and images: `UPGRADE_FROM` and `OPERATOR_VERSION`.
The operator, the tester, Prometheus and Grafana run on the control plane,
which no drain picks.

| Instance | Mode | Storage, auth | Mutations |
|---|---|---|---|
| `chaos-op` | operator | emptyDir, 2 pods | none |
| `chaos-pvc` | operator | `shared-path` volumes, auth, 3 pods: its budget lets one pod go at a time | none |
| `chaos-sent` | Sentinel | emptyDir, 2 pods, 3 Sentinels | none |
| `mixed-sent` | Sentinel | emptyDir, Redis 7.2 and Valkey 9 Sentinels | `sentinel_image_flip` between Valkey 9 and Redis 7.2 |

Each instance has 8Mi of data and a ledger at 10 writes/s. Seed 54 does
these actions, each one 60-90s after the previous one converged: an
operator restart, an upgrade and back, a drain of the second worker, an
upgrade again, and a drain of the first worker.

The script asserts these conditions:

- Each kind converged, and no kind timed out or failed.
- No action found a change in progress.
- There were no findings, all invariants hold, and each path and client
  style works.
- `sentinel_image_flip` converged.
- No restart or upgrade lost a write, and no drain of `chaos-pvc` lost a
  write.

The script reports each action, the time without an operator, the CRD
hook, the blocked evictions, the outages, the verifications and the flips.

The script then checks `deploy/monitoring` against the Prometheus and
Grafana that it deployed before the tester. `e2e/check-monitoring.py`
requires these conditions:

- Each alert rule loads healthy, and no alert fired.
- Each alert, panel, template variable and annotation expression evaluates
  without error.
- Each metric that they select has data.
- Each panel expression returns data for each instance where its series
  have data. For an expression that ends in a filter like `> 0`, the check
  uses the expression before the filter.

It imports the dashboard into Grafana and runs each panel query through
the Prometheus data source of Grafana. `make check-monitoring` runs
`promtool check rules` and the alert unit tests in
[`e2e/rules-test.yaml`](e2e/rules-test.yaml). The unit tests make each
alert fire, and make sure that it does not fire inside a window.

### Use the tester with an RC

1. Install the RC:
   `helm install redis-operator oci://ghcr.io/saremox/redis-operator/charts/redis-operator --version <tag> -n redis-operator --create-namespace`.
2. Create the namespace of each instance in the config.
3. Create the instances that have no `template`. The tester creates the
   other instances from their templates. `deploy/kustomization.yaml` puts
   the templates into the ConfigMap of the config.
4. Set the tester image to the same tag and apply the manifests:

   ```sh
   cd deploy && kustomize edit set image ghcr.io/saremox/redis-operator-soak:<tag>
   kubectl apply -k deploy
   ```

   `release.yml` pushes `ghcr.io/saremox/redis-operator-soak:<tag>` for each
   tag, built against the spec of that tag. The CRD only adds fields, so a
   tester of a newer tag can test an older RC.
5. If you have a Prometheus Operator and the dashboard sidecar of Grafana,
   apply the ServiceMonitor, the alerts and the dashboard:
   `kubectl apply -k deploy/monitoring`.
   - The ServiceMonitor keeps the `namespace` and `pod` labels of the
     tester. They identify the namespace and the redis pods of an
     instance, not of the tester.
   - Without the sidecar, import `deploy/monitoring/dashboard.json`
     manually.
   - If your Prometheus selects PrometheusRules and ServiceMonitors by a
     label, for example `release`, add that label.

To test an upgrade between two RCs, use the `deploy/chaos` component (see
below). Enable `operator_upgrade` in `deploy/config.yaml` and list the two
RCs as OCI charts with their images. The first RC must be the installed
one:

```yaml
chaos:
  kinds: {operator_restart: 2, operator_upgrade: 1, node_drain: 1}
  upgrade:
    set: [crds.upgradeHook.enabled=true]
    versions:
      - name: 4.2.0-rc2
        chart: oci://ghcr.io/saremox/redis-operator/charts/redis-operator
        version: 4.2.0-rc2
        image: ghcr.io/saremox/redis-operator:4.2.0-rc2
      - name: 4.2.0-rc3
        chart: oci://ghcr.io/saremox/redis-operator/charts/redis-operator
        version: 4.2.0-rc3
        image: ghcr.io/saremox/redis-operator:4.2.0-rc3
```

If the cluster cannot pull them, set `chart` to a chart archive next to the
config, for example `helm package charts/redisoperator` of each tag, in the
ConfigMap of the config. Set `image` to a copy that the nodes can pull. The
chaos profile does this. A chart before PR #195 (4.2.0-rc1) cannot run its
CRD hook. Its ConfigMap holds the 1.09MB CRD YAML, but a ConfigMap holds a
maximum of 1MiB. Thus, with `crds.upgradeHook.enabled`, each upgrade to such
a chart fails. Upgrade between 4.2.0-rc2 and later versions, or disable the
hook for such a version.

The Role in `deploy/rbac.yaml` must be in the operator namespace and must
name the operator Deployment, as `operator` in `deploy/config.yaml` does.

The chaos lane is off in `deploy/config.yaml`, and its rights are opt-in.
The kustomize component `deploy/chaos` grants rights on these objects:

- in the operator namespace: the operator pods, the Lease and the helm
  release;
- in the cluster: the ClusterRoles of the chart, the CRD, nodes and
  evictions.

helm can create the ClusterRoles of the chart only if it holds the rights
that they grant, or `escalate` and `bind`. The component grants `escalate`
and `bind`. **This is equal to cluster-admin.** Use the component only on
a cluster that exists for the soak test. To enable the lane:

1. Add `components: [chaos]` to `deploy/kustomization.yaml`, or to your own
   kustomization over `deploy`.
2. Add the chaos kinds to `config.yaml`.

A ClusterRole lets the observer read pods, EndpointSlices and
RedisFailovers in the instance namespaces.

The mutator has its rights only in the instance namespaces. The
`redis-soak-mutator` ClusterRole holds the rules once, and
`deploy/rbac-instances.yaml` binds it with a RoleBinding in each instance
namespace. It can read and change Secrets, because the password of each
client comes from a Secret. Keep `deploy/rbac-instances.yaml` aligned with
the instances in `config.yaml`, and create the namespaces before you apply
the manifests. `password_rotate_offline` scales the operator through the
`scale` subresource of its Deployment, which the Role in the operator
namespace allows.

## Read the dashboard

`deploy/monitoring/dashboard.json` ("Redis operator soak") has a global
row, then one row for each instance of the `Instance` variable. In the
global row, the findings panel must be empty, and `build_info` shows the
operator version (the image tag of its Deployment), so an upgrade shows
there. Chaos actions are annotations on each panel. For the paths, client
styles and labels, see [Probes](#probes) and [Metrics](#metrics).

Read a red part of `writable` together with the window and the `event` of
the outage. Inside a window, it is the cost of the change that caused it.
Outside a window, the alerts report it.

## Findings and expected behaviour

**Findings** are what an RC must not do. Each finding counts in
`findings_total` and fires `RedisSoakFinding`. A finding is a violation
outside a window, a violation still open at the window timeout, an OOM
kill, a `failed_unsafe` version change, or `replica_ready_without_data`
(unless it is off). See [Findings](#findings).

These are also not expected, and have their own alerts:

- a mutation or chaos action that timed out or failed;
- writes lost in an event that must lose none: auth changes, Sentinel
  toggles and flips, operator restarts and upgrades, drains of pods on
  volumes;
- a `follower` `auth` outage longer than 2m.

This **expected** behaviour is measured, but it is not a finding:

- violations and outages inside a window, for example a failover after a
  master kill or a drain. `fresh` and `retrying` clients recover from a
  failover sooner than `pooled` clients, because the server closes the
  connections of a demoted master;
- writes lost to asynchronous replication in a forced kill or a failover,
  and all writes of a `reset`;
- `failed_safe` version changes;
- `OOM` rejections under `noeviction`, and evictions under `allkeys-*`;
- the documented auth limitations (see [Findings](#findings));
- a drain that a PodDisruptionBudget blocks until the pods that it waits
  for are Ready on a different node.

## Known operator issues

The tester found these issues in 4.2.0-rc2. Each issue has a fix PR.

| Issue | Seen as | Fix | Status |
|---|---|---|---|
| A graceful master rollover in operator mode can promote the restarted old master (the same pod with a new UID) instead of a Ready replica. `GetBestReplicaForPromotion` falls back to the highest offset without a readiness check. | Outages of 40-46s on `rfrm`; a `failover` from a pod to itself. | PR #203, `claude/fix-promote-ready-replica` | open |
| A replica that never completed a sync is Ready, because `ready.sh` fails only a full sync in progress. Example: a Valkey replica that cannot read a Redis 7.4/8 RDB. `rfrs` then serves empty reads. | `replica_ready_without_data`. | PR #205, `claude/fix-readiness-link-down` | merged |
| A scale-down that removes the master in operator mode promotes a replica while the old master still accepts writes. Case 1 of `checkAndHealOperatorManagedMode` does not check `masterPodStopping`. | Writes lost by `redis_replicas` on volumes. | PR #210, `claude/fix-scale-down-master-race` | merged |
| A memory limit decrease below the page cache in use: the kubelet refuses the in-place resize, and the operator waits 5m for each pod before it recreates the pod. | `redis_memory` takes more than 18m on 3 pods. | PR #204, `claude/fix-resize-below-usage` | open |
| A changed password applies only at the next resync (30s), because nothing watches the auth Secret. | `follower` `auth` outages of up to 30s. | PR #212, `claude/watch-auth-secret` | open |
| The CRD has no status subresource, so each status update increases `metadata.generation`. | A window opened by each status change. | PR #207, `claude/crd-status-subresource` | merged |
| A rollout that cannot continue, for example onto a server that cannot load the data, stays `Healthy` with an empty message. | Stuck `unknown` edges that only the tester reports. | PR #206, `claude/report-stalled-rollout` | merged |
| Before PR #195 (in 4.2.0-rc2), the CRD hook of the chart cannot install: its ConfigMap holds the 1.09MB CRD YAML. | Each `helm upgrade` to 4.2.0-rc1 with `crds.upgradeHook.enabled` fails. | PR #195 | merged |

## Configuration

See [`config.example.yaml`](config.example.yaml). `deploy/config.yaml` is
the config that the manifests ship.

## Metrics

The tester serves metrics on `:9090/metrics`. `/healthz` answers `ok`.
Per-instance series have the labels `rf`, `namespace` and `mode`
(`operator` or `sentinel`). `mode` is the configured mode, not the current
mode, so the series of an instance stay the same when `sentinel_toggle`
changes its mode.

| Metric | Type | Extra labels |
|---|---|---|
| `redis_soak_probe_total` | counter | `path` (`sentinel`/`rfrm`/`rfrs`), `client` (`pooled`/`retrying`/`fresh`/`follower`), `op` (`set`/`get`/`wait`), `result` |
| `redis_soak_probe_duration_seconds` | histogram | `path`, `client`, `op` |
| `redis_soak_writable`, `redis_soak_readable` | gauge (0/1) | `path`, `client` |
| `redis_soak_last_success_timestamp_seconds` | gauge | `path`, `client`, `op` |
| `redis_soak_outage_duration_seconds` | histogram | `path`, `client`, `event` (the chaos kind or the mutation kind of the instance when the outage started, or `none`) |
| `redis_soak_invariant_ok` | gauge (0/1) | `invariant` |
| `redis_soak_invariant_violation_seconds` | histogram | `invariant` |
| `redis_soak_findings_total` | counter | `invariant` (also `oom_killed` and `version_transition`) |
| `redis_soak_window_open` | gauge (0/1) | |
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
| `redis_soak_wait_acked_replicas` | gauge | |
| `redis_soak_dataset_keys` | gauge | |
| `redis_soak_used_memory_bytes`, `redis_soak_maxmemory_bytes` | gauge | |
| `redis_soak_oom_rejections_total` | counter | |
| `redis_soak_evicted_keys_total` | counter | |
| `redis_soak_lost_writes_total` | counter | `event` (a mutation or chaos kind, `failover`, `reset` or `periodic`) |
| `redis_soak_ledger_verified_total` | counter | `event` |
| `redis_soak_version_transition_total` | counter | `from`, `to` (version names), `expect`, `result` (`ok`/`failed_safe`/`failed_unsafe`) |
| `redis_soak_version_mixed_seconds` | histogram | `from`, `to`, `component` (`redis`/`sentinel`) |
| `redis_soak_chaos_total` | counter | `kind`, `result` (`converged`/`timeout`/`failed`/`skipped`); no instance labels |
| `redis_soak_chaos_converge_seconds` | histogram | `kind` |
| `redis_soak_chaos_in_progress` | gauge (0/1) | `kind` |
| `redis_soak_chaos_operator_down_seconds` | histogram | `kind` |
| `redis_soak_chaos_eviction_blocked_seconds` | histogram | |

The probe `result` is one of `ok`, `timeout`, `refused`, `readonly`,
`loading`, `auth`, `oom`, `masterdown`, `noreplicas`, `dns`, `closed` and
`other`. The tester classifies server errors by their leading error code,
never by the message, so Redis and Valkey errors count the same. `closed`
is a connection that the server closed or reset, for example when the
operator disconnects the clients of a demoted master.

An outage starts at the first failed probe and ends at the next successful
probe, for each path and client style. It counts for the `event` that ran
when it started. When a path is removed, the tester deletes its series.

`masters` is the number of redis pods that report the master role. 1 is
correct, 0 is an instance without a master, and 2 or more is a split
brain. `failovers_total` counts changes of the master pod by pod UID, so it
also counts a master that a new pod with the same name replaces.

`replication_lag_bytes` is the `master_repl_offset` of the master minus the
`slave_repl_offset` of each replica of the current master. The two `INFO`
replies are a few milliseconds apart, so the minimum value is 0. For a
bootstrapping instance, the lag is behind the source master, for the pods
on its stream.

`server_info` comes from `INFO server` (`valkey_version`, `server_name` and
`redis_version`), and shows pods that run different servers or versions.
The per-pod series exist only while the pod answers `INFO`, and the tester
deletes them when the pod is gone. `operator_version` is the image tag of
the operator Deployment. The tester reads it again every 30 seconds.

`window_open` is 1 while the convergence window of the instance is open. A
violation is then expected, not a finding. In the alerts, "outside a
window" is `window_open == 0`. `findings_total`, `mutation_total`,
`chaos_total` and `lost_writes_total` start at 0 for each invariant, kind,
result and event that the instance can have, so that `increase()` shows the
first increment.

`mutation_converge_seconds` starts when the mutator applies a converged
mutation and ends when its window closes. The observer judges the window on
its ticks and keeps it open for at least `minDwell`, so the values are at
least `minDwell` and increase in steps of `observer.interval`.
`pods_recreated_total` counts the redis pods that a mutation replaced with
a new pod of the same name. If a `redis_resources` mutation counts a pod,
the resize was not in place.

`dataset_keys`, `used_memory_bytes` and `maxmemory_bytes` come from the
`INFO` of the master. `evicted_keys_total` adds the `evicted_keys` deltas
from `INFO stats` of each redis pod. A new `run_id` starts a new counter,
and a pod that the tester first sees after its start counts from 0. Thus
restarts and failovers do not lose or double-count evictions.
`wait_acked_replicas` is the reply of the last sampled `WAIT 1`, from any
path and client style. `ledger_verified_total` counts verifications,
including the fill-only verifications of instances without a ledger.
