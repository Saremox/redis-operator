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
  because the configuration keeps room for a fork (see
  [Mutations](#mutations));
- a version change that fails unsafely (see [Server versions](#server-versions));
- a reset that does not complete within two times its timeout;
- a Ready replica that never completed a sync;
- a step of the autoscaler that breaks a rule of mode Auto, also inside a
  window (see [Autoscaler mode Auto](#autoscaler-mode-auto)).

These events are also not expected, and have their own alerts:

- a write lost in an event that must lose none (see [Data](#data));
- a mutation or chaos action that times out or fails;
- an `auth` outage of the `follower` client for more than 2 minutes.

The tester measures this behaviour, but it is not a failure:

- outages and violations inside a window;
- writes lost to asynchronous replication in a forced kill or a failover;
- `failed_safe` version changes;
- `OOM` rejections under `noeviction`;
- evictions under `allkeys-*`.

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

Redis 6.2 does not wait for its replicas on SIGTERM. For an instance on
Redis 6.2, the tester does not require a lossless result for a mutation or
a `node_drain` that stops a redis pod. It counts such a loss in
`redis_soak_lost_writes_total`.

The ledger keys must never be evicted, because the tester cannot tell an
eviction from a loss. Config validation rejects a ledger on an instance
that can run under `allkeys-*`, and a ledger under `volatile-*` without
`fill.ttl`.

A bootstrapping instance (`bootstrap.source`) is read-only. Its
verification reads a sample of the source writes from the source master.
Then it reads the sample from each pod, after the pod reaches the
replication offset that the master had at that read. The tester does not
compare offsets of different replication IDs: a pod that did not sync with
a new source master yet can be far ahead in the old stream.

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

### A full master under noeviction

The operator sets `maxmemory` below the used memory only with an
`allkeys-*` policy. With other policies, it keeps `maxmemory`. Thus a
master under `noeviction` refuses writes with `OOM` when its data reaches
`maxmemory`, for example after `fill_burst`, a load or a lower percent.
Nothing frees the memory until a change of the data, the policy, the
percent or the limit. This is by design, and not a finding.

The master is full when its policy is `noeviction` and `used_memory` minus
`mem_not_counted_for_evict` is at least `maxmemory` minus a margin. The
margin is 1 percent of `maxmemory`, 1Mi at least, because the used memory
changes between the refused write and the `INFO` of the observer. The
observer reads these fields in its `INFO` of each pod.

A write probe that fails with `OOM` while the master is full sets
`redis_soak_expected_not_writable{reason="noeviction_full"}` to 1, and adds
the probe interval to `redis_soak_expected_not_writable_seconds_total`.
`RedisSoakNotWritable` ignores such a client. Another error of the probe
is not expected, also while the master is full.

A full state is expected for `observer.noevictionFullLimit`, 30 minutes by
default. After that time, it counts one time as the finding
`noeviction_full`. Then `RedisSoakNotWritable` fires again, because the
fill, the ledger and the mutations that need writes stop. Only a round
that shows a master that is not full ends the state. A round without a
single master, for example after an `INFO` timeout, keeps the time.

The finding `maxmemory_below_used` counts a `maxmemory` of a pod that
decreased between two rounds. It counts only a value below the used memory
of that pod plus the margin, under a policy that is not `allkeys-*`. Both
findings count also inside a window, as `oom_killed` does.

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

  The tester records the recovery path of each converged run in the log
  field `recovery_path` and in `redis_soak_no_master_recovery_total{path}`.
  The path never changes the result. The tester uses two signals:
  - The status message. From before the reset until the convergence, the
    tester watches the `status.message` of the RedisFailover. When no
    Sentinel can fail over, the operator promotes a replica. After
    `PromoteBestReplica` succeeds, it sets the message `Sentinel knew no
    replica to fail over to, the operator promoted` and the pod name. The log
    field `operator_message` tells if the tester saw this message.
  - The epoch. The tester reads `config-epoch` of the master from each
    Sentinel before the reset and after the convergence, and logs the change
    as `config_epoch`. `SENTINEL RESET` keeps the epoch.

  The paths are:
  - `operator`: the operator promoted a replica during the window. The tester
    saw the message, or the epoch fell. The operator makes each Sentinel
    monitor the new master with `SENTINEL MONITOR`, which sets the epoch to 0.
  - `sentinel`: the tester saw no message, and the epoch rose. A Sentinel
    failover raises the epoch.
  - `unknown`: all other cases. The epoch did not change, it rose on one
    Sentinel and fell on another, or the tester could not read it.

  The message wins over the epoch. A run with the message and a risen epoch
  is `operator`, also if a Sentinel failover ran too. `config_epoch` shows the
  rise. The epoch can also fall when the operator monitors a lagging Sentinel
  again after a Sentinel failover. Then the change usually reads `mixed` or
  `unchanged`, and the path is `unknown`.

  The operator sets no message when it elects a master without a Sentinel
  quorum, or when every pod has `127.0.0.1` as master. Both cases use
  `SetOldestAsMaster`. These elections leave a trace only if the epoch was
  above 0 before. The epoch is 0 on a new instance and after an election by
  the operator. Thus `unknown` does not show that the operator did not run.

  The watch can end before the message arrives, for example if the API server
  closes it. Then the tester sees no message.
- `password_rotate`, `auth_remove` and `password_rotate_offline` can time
  out with the status `unable to apply the configured password`, for example
  after an operator restart. Then the tester puts the previous password back,
  because a stuck instance hides all other signals of the run. After
  `auth_remove`, it sets `auth.secretPath` again, and the Secret still has
  the previous password. After the other kinds, it writes the previous
  password to the Secret. If the instance becomes `Healthy` within one more
  convergence timeout, the result is `stuck_restored`, else `timeout`. The
  findings of the stuck time count. Without a known previous password, the
  tester does not put it back.
- `password_rotate_offline` is scenario C. It stops the operator, changes
  the password, and starts the operator with its replicas. The operator
  keeps the passwords of the redis pods in the Secret
  `rfr-auth-applied-<name>`, so it applies the password without help.
- `password_rotate_offline_forget` is scenario C without this Secret. The
  tester deletes the Secret while the operator is stopped. It then expects
  `unable to apply the configured password`, sets the previous password,
  expects `Healthy`, and changes the password again. An operator that does
  not keep this Secret gets stuck in both kinds. Then
  `password_rotate_offline` ends `stuck_restored`.
- Both scenario C kinds stop all other mutations while they run. They
  always start the operator again.
- `reset` deletes the RedisFailover, its volumes and its auth Secret, and
  creates it again from its template.

A result is `converged`, `timeout`, `stuck_restored` (see
`password_rotate`), `rejected` or `skipped`. `rejected` means that the API
refused the change. `skipped` means that the mutation cannot apply now, for
example a replica kill on an instance with one pod. A version change that
does not converge gets its judgement below, not `timeout`.

The mutator mutates only a quiet instance. `redis_soak_mutation_stalled_seconds`
shows the seconds that the mutator waits for one. It is 0 while the instance is
quiet and while a mutation runs.

### Server versions

`versions` names each server version, pinned to an exact patch tag.
`edges` is the transition graph, with the expectation `ok`, `fail` or
`unknown` of each edge. A downgrade never expects `ok`, because an older
server may not load a newer RDB. The main graph has no downgrade, so a chain
goes back to its start with a `reset`. The `chain` of an instance is the part
of the graph that it moves through.

An edge can have a `group`. A chain takes only the edges of its own `group`,
and a chain without a `group` takes only the edges without one. Thus a group
does not change the other instances.

The instances `redis-chain-62` and `redis-chain-62-sent` (with Sentinels)
start on `redis-6.2`, the oldest Redis version in the graph. They move
through `redis-7.2` and `redis-7.4` to `redis-8`, and then reset on
`redis-6.2`. The edge `redis-6.2 -> redis-7.2` is `ok`, because Redis 7.2
reads the RDB data of Redis 6.2.

The group `skip` has four `ok` edges that leave out a version, for example
`redis-6.2 -> redis-8`. The newer server reads the older RDB data. The
instance `skip` takes them, on volumes. The group `downgrade` has six
`unknown` edges, for example `redis-8 -> redis-7.4`. The tester does not
know if the older server loads the data, so a safe failure and a convergence
both pass. The instance `downgrade` takes them. Both instances start on each
of their start versions in turn.

The instance `edge-sent` takes the same edges as `edge`, with Sentinels. The
Sentinels change image after the data image, so a stuck data change leaves
them as they are. The instance `redis-chain-big` is `redis-chain` with 256Mi
of data and a memory limit of 640Mi, for the fork and the full sync of each
step.

`image_upgrade` follows an edge from the current version. The tester
observes a change along an `unknown` or `fail` edge for the timeout of the
kind. If a pod on the new version logs a failed load of the data, the
tester observes the change for at most one more minute. The timeout of
the kind still applies. It then judges the change:

- `ok`: it converged.
- `failed_safe`: it did not converge, but the rollout stopped. The single
  master still runs the old version, accepts writes, and lost no
  acknowledged write. At most one pod on the new image is not Ready or
  fails to load the data, and it is not the master. This rule does not apply
  to a Sentinel image change. If the Sentinels did not change image, each of
  them still reports this master.
- `failed_unsafe`: all other cases. This is a finding.

A change that did not converge is reset immediately.

The tester keeps a version change in its own process. A tester that starts
during a stuck change adopts it when the instance shows this state:

- Exactly one redis pod runs the image of the spec. It is not Ready, it does
  not replicate, and its log says that it cannot load the data.
- All other redis pods are Ready and run one other configured image. The
  master is one of them.
- The chain has an `unknown` or `fail` edge from the version of the other pods
  to the new one.

The tester reads the instance at each observer interval until it finds this
state, the instance is quiet, or the deadline passes. The deadline is the
convergence timeout minus the grace, the verification bound and one minute of
margin, counted from the start. The grace is one minute, and the verification
bound is two minutes.

After the grace, the tester takes the mutation lock and reads the instance
again. The state must still hold. The tester then judges the change by the rules
above. The master must accept writes, the ledger must show no lost write, and
each Sentinel that kept its image must report the master. If the judgement is
`failed_safe`, it logs `adopted=true` and resets the instance.

The tester never adopts a change along an `ok` edge, because a stuck replica
there is a finding. It adopts no other state, and the observer counts findings
after the convergence timeout. If one of the checks fails, for example a Sentinel
does not answer, the tester does not adopt the change. The instance then stays
stalled, and `redis_soak_mutation_stalled_seconds` shows this. Adoption runs
once per tester start, so one failed read brings back the stall. An adopted
change has no `redis_soak_version_mixed_seconds` sample. Its ledger check
covers only the writes since the start.

Adoption has these limits:

- Adoption can wait for the mutation lock. Another instance can run an
  exclusive mutation, for example `password_rotate_offline`, while adoption
  polls or waits.
  Later phases of that mutation can outlast the startup window, and findings
  can appear. Adoption still resets the instance afterwards.
- A tester can start between the spec patch and the first pod deletion by the
  operator. The startup window then closes at the first round, because all
  invariants hold. Adoption does not run. This gap is shorter than the start of
  the tester, but it exists.

### Autoscaler mode Auto

An instance with `autoscale` in the config gets a judge of the steps of the
memory autoscaler. Its template must set `redis.autoscaling.mode: Auto`,
`maxMemory` with `noeviction`, and a memory request equal to the limit. The
instances `auto` and `auto-sent` (with Sentinels) do this. They need
Kubernetes 1.33 or later.

The rules of the operator are constants, so the tester cannot make a step
come sooner. The operator samples the master one time in each minute. A use
of 95 percent of `maxmemory` gives a step at the next sample. The instance
must be `Healthy`, the cooldown must be over, and the budget of 6 steps in
24 hours must allow it. Thus a load of 97 percent of `maxmemory` gives a step
after about 1 to 2 minutes.

Mode `Off` and a lost state start the budget again, and the user paths lower
the limit below `memoryMax`. With the shipped weights, expect about one step
in each hour. The counter `redis_soak_autoscale_steps_total` shows the real
rate.

The template sets `maxMemory.percent` to 35. Thus the load fits two times in
the limit, plus the operator reserve of 32Mi, for a fork. The fill is 10
percent of `maxmemory`, so that a user path can lower the limit to the
spec limit.

The kinds, and what each kind may do to the redis pods:

| Kind | Change | May |
|------|--------|-----|
| `autoscale_load` | Write keys up to 97 percent of `maxmemory`, wait at most 3 minutes for a step, and delete the keys. | nothing |
| `autoscale_kill_master` | The load, and a graceful delete of the master while the step is `Applying`. | recreate the killed pod, hand over |
| `autoscale_kill_replica` | The load, and a graceful delete of a replica while the step is `Applying`, as a node drain does. | recreate the killed pod |
| `autoscale_limit` | Set the memory limit and request of the spec to a value in `autoscale.limit`. | lower: recreate, shrink; higher: recreate |
| `autoscale_memory_max` | Set `memoryMax` to a value in `autoscale.memoryMax`. | below the largest pod limit: recreate, shrink; else nothing |
| `autoscale_mode` | Change the mode in the cycle `Auto`, `Recommend`, `Off`. | `Off`: recreate, shrink |
| `autoscale_state_lost` | Delete the state ConfigMap `rfa-<name>`. | nothing |

A kill kind that sees no step `Applying` within the 3 minutes is `skipped`.
The other mutation kinds, a chaos action, a stopped operator and a change of
the generation by hand may do all of it.

An autoscale kind converges when no step is `Applying`, the rollout is
complete, and no resize is pending. Each pod must also run the limit of its
own spec, and at least the limit of the StatefulSet. `maxmemory` must follow,
and the instance must be `Healthy`. Mode `Off` also needs an empty
`status.autoscaling` and each pod on the spec limit. `autoscale_state_lost`
also needs a state ConfigMap that the operator wrote again, because the
operator does not watch the ConfigMap. A kind that may not recreate a pod
must lose no write.

Each violation of a judge counts one time in `redis_soak_findings_total`,
also inside a window, as `oom_killed` does:

- `autoscale_step_size`: a step raises the limit to one target: 25 percent
  more, rounded up to a multiple of 64Mi, and `memoryMax` at most. A smaller
  or a larger step is a violation. Because the operator rounds up, a small
  limit gets a larger step: 64Mi goes to 128Mi. The status shows the limit
  of the step one reconcile after the start. Thus the check waits until
  `effectiveMemory` moves in `Applying`, or until the step is `Done` or
  `Held`. A revert, and a user change of the spec limit or of `memoryMax` in the step,
  give no check.
- `autoscale_step_timeout`: a step leaves `Applying` within 18 minutes after
  `lastStep`. This is the timeout of 15 minutes, the minute of `lastStep`, and
  2 minutes for the reconcile that ends the step. Without a `lastStep` that
  parses, the limit is 17 minutes after the first round in `Applying`. After
  this time, the step keeps no window open, so the window of a mutation can
  time out.
- `autoscale_step_rate`: a step starts at least 5 minutes after the end of
  the last step. At most 6 steps start in 24 hours. After a revert,
  `retryAfter` is at least the back-off of 1 hour, which doubles at each
  further revert up to 24 hours. No step starts before `retryAfter`, unless
  the generation changed.
- `autoscale_restart` and `autoscale_handover`: while a step is `Applying`
  or `Held`, no redis container restarts, no redis pod is recreated, and the
  master does not change.
- `autoscale_shrink`: the applied limit of a pod does not decrease.
- `autoscale_maxmemory`: the `maxmemory` of a pod is not above its limit.
- `autoscale_status`: each field of `status.autoscaling` has a value that
  the operator writes. A phase is `Idle`, `Applying`, `Done`, `Reverted`,
  `Held` or empty. `lastStep` is a time, not after the round, and not before
  the last step. `Applying` and `Reverted` have a `lastStep`.
  `effectiveMemory` is empty or a positive quantity. A field counts again
  only when its value changes.

The other judges then use a safe default. An unknown phase is no step in
progress, and the size check skips an `effectiveMemory` that is not valid.
Without a `lastStep` that parses, the rate checks skip the step. A step
timeout never counts from later than the first round in `Applying`. A
`Reverted` step without a `retryAfter` that parses is an `autoscale_step_rate`
violation, and the back-off counts from the revert that the tester saw. An
empty `status.autoscaling` is mode `Off` or a lost state, and starts the
budget again. An unknown pod limit or `maxmemory` keeps the last known
limit and gives no `autoscale_maxmemory` check.

At its start, the tester adopts the step of the status. Only a step in
`Applying` has its first result still to come, because `Held` is a result.
A `Done` step gives the cooldown from `lastStep`. A `Reverted` step gives
the back-off up to `retryAfter`, or 1 hour after `lastStep` without a
`retryAfter` that parses, which is an `autoscale_step_rate` violation. The
judge follows the phase also without `lastStep`, because the held step of a
lost state has none. `lastStep` has a resolution of a minute, so `Applying`
after `Done`, `Reverted` or `Held` is a further step, also with the same
`lastStep`.

A user path that may do the change excuses it. The excuse covers the round
of the user path and the next round, because a change can show one round
late. For `autoscale_maxmemory`, a user path that may shrink a pod excuses
it, because the operator lowers `maxmemory` one reconcile later.

The status gives its times in full minutes. Each rate check uses the latest
time that the minute allows. It also adds a slack of 30 seconds for the
clocks and the age of a read. Thus only a sure violation is a finding. The
status does not tell an urgent step from a sustained one. Thus the cooldown
check uses the urgent cooldown of 5 minutes for each step.

A step keeps a convergence window open while it is `Applying`, and in the
round of its end. The operator sets `maxmemory` one reconcile after the last
resize, so `config` can fail for a short time. `redis_soak_autoscale_steps_total`
counts each step at its start, `started`, and at its first end: `done`,
`reverted` or `held`.

The judges, the planner and the convergence of each kind have unit tests
with fakes: `go test ./internal/autoscale/ ./internal/observer/ ./internal/mutator/`.

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

The versions profile leaves out `redis-chain-big`. Its 256Mi of data make
each step and each reset too slow for the kind job. The full profile leaves
out the instances with `autoscale`, because a step of the autoscaler takes
minutes. No profile runs them.

The versions profile runs 3600 seconds by default. The instance `downgrade`
needs 16 steps in that time. This holds only if each stuck change logs that
it could not load the data, because the change then ends 1 minute later. A
silent stuck change waits 6 minutes, about 9 minutes with its reset. In that
case, set `DURATION=4500`. The last needed edge starts at step 15, about 4000
seconds in.

The script keeps the logs of the tester and the operator, the events and
the last scrape in `bin/kind-e2e-artifacts/`. The workflow
`.github/workflows/soak-e2e.yml` runs it on demand against a release.

## Alerts and dashboard

`deploy/monitoring/prometheusrule.yaml` has these alerts. "Outside a
window" is `redis_soak_window_open == 0`.

- `RedisSoakNotWritable`: a path is not writable for 1 minute outside a
  window. The expected `OOM` of a full master does not fire this alert (see
  [A full master under noeviction](#a-full-master-under-noeviction)).
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

## Delays of the tester

A delay of the tester itself can make the reads of all instances time out
in one round. These logs and metrics tell such a delay from a fault of an
instance. No alert uses them.

- `observer round slow`: a round took more than 1 second, or started more
  than 1 second after its tick or after the end of the round before it.
  The fields give the time of the API reads, the pod reads and the checks.
  The other rounds log `observer round` at debug level, which the tester
  does not enable. When a read of the API fails, `kube_seconds` holds its
  time, `read_seconds` is 0, and the histograms record the round.
- `observer read slow`: a read of a pod failed or took more than 1 second.
  The field `phase` tells where a read failed:
  - `dial`: the tester made no connection.
  - `reply`: the tester made the connection, but no reply came.
  - `context`: the tester stopped.

  go-redis returns `context deadline exceeded` when the dial did not end
  before the read timeout. It returns `i/o timeout` when the reply did not
  come.
- `soak process stalled`: a goroutine with a tick of 100 milliseconds did
  not run for more than 1 second. `redis_soak_process_stalls_total` counts
  these stalls.
- `redis_soak_observer_round_seconds` and
  `redis_soak_observer_round_lateness_seconds` record each round.
- `go_sched_latencies_seconds` and `go_gc_pauses_seconds` record the delays
  of the Go scheduler and of the GC. client_golang puts them in larger
  buckets, and the highest finite bucket is 0.117 seconds. Thus they show
  only that a delay was longer than about 0.1 seconds. The stall detector
  and the round histograms give the length of the delay.

## Limits

- The autoscale judges read the status of the RedisFailover, not the
  metrics of the operator. The tester does not change the image during a
  step. It does not cause a revert or a held step on purpose, for example
  with a ResourceQuota.
- The tester does not check two documented operator limits: until a redis
  pod restarts after a password change, its exporter and its pre-stop
  `SAVE` use the old password; and an operator that restarts between a
  Secret change and its next check needs the recovery of scenario C.
- `config` cannot check Sentinel keys that `SENTINEL MASTER` does not
  report, for example `auth-pass`, nor `requirepass`, `masterauth` and
  `aclfile`, which the operator sets itself.
- On node-local volumes, an evicted pod can start again only on its node.
  Thus a drain cannot converge before the uncordon.
- The tester never reverts `spec.redis.image` to the old image after a
  failed-safe Redis to Valkey change: it resets. Once, a revert by hand
  repaired a stuck replica (3 pods, 16 MiB). The operator recreated the
  replica on the old image. The replica was Ready after about 50 seconds, and
  the replication synced. The operator deleted no data. The tester does not
  test this path.
- The tester does not compare `currentRevision` of the StatefulSet: for
  `OnDelete`, the controller never advances it.
- Run one tester for each cluster. Two testers write the same keys.
