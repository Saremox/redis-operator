# Controller logic

## Creation pipeline

The operator creates each RedisFailover and the objects that it needs. These events start a reconcile of a RedisFailover:

- An add, an update or a delete of the RedisFailover.
- A resync every `--sync-interval` seconds (default 30).
- An event of one of its pods.
- With `--watch-auth-secrets`, an event of the Secret in `auth.secretPath`.
- The end of the operator-managed failover wait, `sentinel.failoverTimeout`. No pod event comes at that time.

Each reconcile does these steps in this order:

1. Delete: if the RedisFailover has a deletion timestamp, the operator removes its metrics and in-memory state, then removes its finalizer. Nothing else runs. The operator also removes the metrics and the state of a RedisFailover that is gone before a reconcile sees its deletion timestamp. This happens after a manual removal of the finalizer.
2. Finalizer: the operator adds its finalizer. With it, the operator sees the deletion timestamp before the object is gone.
3. Skip: if the annotation `redisfailovers.databases.spotahome.com/skip-reconcile` is `"true"`, the operator stops here. Use it to repair a cluster by hand.
4. Validate: `Validate()` rejects an invalid spec and sets the defaults.
5. Autoscaler state: with `redis.autoscaling.mode` `Recommend` or `Auto`, the operator reads its state from the ConfigMap `rfa-<NAME>`. The state can hold a memory limit of the redis pods above `redis.resources`, and Ensure writes that limit to the statefulset. Thus an error of this read stops the reconcile before Ensure. Without autoscaling and without `status.autoscaling`, the operator reads the ConfigMap one time after its start, and deletes a ConfigMap that it finds.

   After a step, the spec limit can be smaller than the pods. Thus the limit of the live statefulset is a floor, in each mode with autoscaling. Without the statefulset, the largest applied limit of the redis pods gives the floor. A lower limit needs a lower `memoryMax`, a revert of the template step, or a lower spec limit. The template must then have the spec limit of the last Ensure, which the statefulset annotation `redisfailovers.databases.spotahome.com/spec-memory-limit` holds.

   The floor costs one more read of the statefulset in each reconcile. The operator owns the annotation. A larger value counts as a lower spec limit of the user.

   A state below the floor counts as lost, for example a restored older ConfigMap. A state that is lost, not valid, of another owner or of a newer operator gives the floor. A state with a larger limit and without a sample in the last 11 minutes also gives the limit of the live statefulset. A revert is the exception.

   A lost or invalid state also lost its budget, its back-off and its step. The ConfigMap, or a phase or an effective limit in `status.autoscaling`, shows such a loss. Step 8 then does not use the sample of the state in memory. Step 8 then freezes the limit, also at the spec limit, and no step starts until the next change of the spec.

   After that change, a pod can still refuse the lost step, and a later spec limit at the frozen limit does not change the template. Thus a frozen limit above the spec limit becomes a `Held` step to it. The step ends as `Done` when each pod runs with that limit.
6. Ensure: the operator creates or updates the objects below. It overwrites a manual change of these objects:
   - Redis service (if exporter enabled)
   - Redis master service and Redis replica service
   - Redis shutdown configmap
   - Redis readiness configmap
   - Redis configmap
   - Redis statefulset, and its PodDisruptionBudget unless `redis.disablePodDisruptionBudget` is true
   - Sentinel service, configmap and deployment. Only when Sentinel is on and, in bootstrap mode, `allowSentinels` is true. Otherwise, the operator deletes them. With them, the operator also creates a Sentinel PodDisruptionBudget unless `sentinel.disablePodDisruptionBudget` is true, and a Sentinel service account unless `sentinel.serviceAccountName` is set.
   - When a flag disables a PodDisruptionBudget or `sentinel.serviceAccountName` is set, the operator deletes the object that it created before. An old PodDisruptionBudget would block node drains. The operator deletes only an object that the RedisFailover owns. It deletes the ServiceAccount when no Sentinel pod uses it, because the old ReplicaSet cannot create a pod without it. Without the RBAC verb, the operator logs a warning and keeps the object.
7. Check & Heal: connects to every Redis and Sentinel and moves them to the desired state. First, it applies a changed password, because every later check authenticates. It keeps the passwords that the Redis pods run with in the Secret `rfr-auth-applied-<NAME>`, so that it can apply a change after a restart. Then, when not all Redis pods run, it deletes a stale pod that the scheduler cannot place. Then it uses one mode. The sections below describe both.
8. Autoscaler: after the checks, the operator uses the state of step 5. At most one time in each minute, it reads `INFO` from the master that the checks found, and writes the state when it changes. It sets `status.autoscaling` and the autoscaler metrics. An autoscaler error after step 5 gives a warning in the log, but does not change the state of the RedisFailover.
9. Autoscaler step: in mode `Auto`, the operator decides on a step from the rollout result of step 7. It starts a step only on a settled RedisFailover. The state is `Healthy`, all pods are Ready on the update revision, and each replica is in sync. The rollout does not wait.

   The operator writes the step to the ConfigMap before Ensure can change the statefulset. A failed write starts no step. After a start or a revert, the operator starts the next reconcile at once, because the controller does not watch the ConfigMap.

   The same step ends a step: done, reverted or held. Mode `Recommend` also ends a step of an earlier mode `Auto` that is `Applying` or `Held`, and starts no step.
10. Status: the operator writes the status, if it changed.

## Check & Heal modes

Operator-managed mode and Sentinel mode need a quorum (a majority) of the pods to run, not the full number in the spec. A Pending pod does not block the heal of the others while the running pods are a majority. With 2 replicas, one Pending pod blocks the heal.

A pod rollout replaces one stale pod in each reconcile: the replicas first, the master last. Before each replacement, the operator waits until the last replaced pod is ready and each replica on the new revision is in sync. A stale replica that is not in sync has no data to lose. For this reason, the operator replaces such a replica first and does not wait for its sync. When only the container resources change and the kubelet allows it, the operator resizes the pod in place. With `redis.maxMemory`, a lowered memory limit holds the rollout until the `maxmemory` and the memory in use of the master fit the new limit.

While the statefulset status does not show the generation of its template, the rollout waits. Until then, the status names the revision before the change, and the pods on the latest template look stale.

In mode `Auto`, a step of the autoscaler changes only the memory limit of the redis container. The rollout resizes such a pod in place and never recreates it. This is also true when the cluster refuses the resize or the operator cannot read a revision. It keeps the refusal for step 9, and the status shows it. A change of the spec in the same revision takes the normal rollout.

The rule does not depend on the mode or the phase, so it also applies in mode `Recommend`. A revision that raises only the memory limit to the effective limit is a step when the effective limit is above the spec limit. A raise of the spec below the effective limit does not change the template, so a pod from before it keeps the rule. These cases also keep a pod:

- The spec limit is at the limit of a step that is `Applying` or `Held`. The template does not change, so the step continues. A refusal or the timeout then holds the step, because a revert cannot lower the template.
- The state is lost, not valid, frozen, of another owner or of a newer operator. Then each raise to the limit of the live statefulset can be a step, also at the spec limit. A pod on a revision with a larger limit up to `memoryMax` can be a pod of a step, so it keeps its limit. A lower spec limit of the user ends this rule. A freeze ends at the next change of the spec.

A revert to the limit before the step happens only while no pod applied the step. Also, no pod may run or start from the template of the step. No pod may be absent or stop, because the StatefulSet can create it from that template. The revert first removes the annotation of the open resize request from the pods. Otherwise the operator holds the step and does not lower the limit of a pod. It also holds the step when it cannot remove an annotation within 30 minutes after the start of the step.

Before Ensure applies a revert, step 5 lists the pods again, also after an operator outage. A pod of the step that exists then holds the step. An absent pod or an absent statefulset does not. A pod that the StatefulSet created from the template of the last step keeps its larger limit. The rollout only moves its revision label. A raise of `memoryMax` keeps this rule. Only a `memoryMax` below that limit, a spec limit at or above that limit, or mode `Off` ends it.

When the spec of a pod requests the limit of a step since a revert, the next step writes the request time again. Otherwise a deferred resize times out at once. Before Kubernetes 1.35, the API server refuses a limit below the pod spec. Thus such a pod keeps its request when a change of the spec gives another limit, and the rollout holds at it. A spec limit at or above the limit of a done step ends the step, so a later lower spec limit takes the normal rollout.

A pod on the update revision that keeps an annotation of a resize request loses it, because nothing else removes it. A resize request makes a pod stale, also on the update revision. When such a pod has the resources of the template, or a larger memory limit, the rollout only removes the request. An example is a request that a second operator replica writes again after a revert. Without in-place resize, the rollout still moves a pod of the last step to the update revision. A step does not start when the redis container restarts on a memory resize.

A stale pod stays Pending when its `PodScheduled` condition is `False` with the reason `Unschedulable`. The `OnDelete` StatefulSet does not replace such a pod. The operator deletes such a pod when none of its containers started and the StatefulSet status shows the current generation. It does this before the check of the pods that run, which ends the reconcile with 1 or 2 replicas and in bootstrap mode. The operator does not wait for a quorum or for the last replacement, because the pod has no data in memory and serves no client. Its PVC stays, and the StatefulSet creates the pod again on the new revision, so the operator does not delete it again.

### Operator-managed mode

This is the default mode (`sentinel.enabled` not set or `false`). There is no Sentinel, so the operator elects the master itself. It checks:

- A quorum of Redis pods runs.
- Only one Redis works as a master. An old master that did not answer during a failover can come back as a second master. If exactly one running pod has the master label, the other masters become replicas of that pod, because the label shows the elected master. Otherwise the operator reports `multiple masters detected, fix manually`.
- No master: the operator promotes the best replica, or the oldest pod if it finds no replica to promote. Before the promotion, it waits in these cases. The status is `NotHealthy` while it waits.
  - The old master pod is still stopping. That master can still take writes.
  - No pod answers as master, and a Ready pod does not answer. That pod can be the master in a short stall.
  - The master pod does not answer. It gets `sentinel.failoverTimeout` (default 10s) from the first missed check. The operator keeps that time in a pod annotation, so an operator restart keeps the deadline.
- The master does not answer, or is not a master: the operator waits `sentinel.failoverTimeout` as above, then promotes the best replica. Without a replica to promote, it does not promote the oldest pod. The status message is then `no healthy replica available for failover`.
- All Redis replicas replicate from the master.
- Redis has the custom configuration and the managed `maxmemory`.
- Stale Redis pods get the new statefulset revision. The operator does not delete a stale master pod that has replicas. First, it hands the master role over to the best synced replica with the Redis `FAILOVER` command:
  1. The master pauses the writes and waits until the replica has its whole replication stream. Clients that write in this time wait.
  2. The master becomes a replica of that replica, and the replica becomes the master. In the normal case, no write that the old master acknowledged is lost. The exceptions are below.
  3. The operator gives the master label to the new master, and makes the other pods its replicas.
  4. The old master is then a stale replica. A later reconcile replaces it.

  The catch-up takes 2s at most, and a synced replica usually catches up in less than 1s. If the replica does not catch up in 2s, Redis aborts the failover, and the master continues. The role change after the catch-up has no Redis time limit, and usually takes milliseconds. If the failover does not end 3s after the command, the operator sends `FAILOVER ABORT`, and the old master continues with all its data. A replica that took the role at the same time has no client writes. The check for multiple masters makes it a replica again.

  A replica that restarts in place loads its RDB file. That file holds the replication ID of the master, so the replica can take the role with older data. Thus the operator reads the run ID of the replica every 100ms during the failover. If the replica does not answer, or has a new run ID, the operator sends `FAILOVER ABORT` at once. A short network error also ends the handover, and the next attempt comes after the retry wait.

  After a completed role change, the operator reads the run ID of the new master once more. A new run ID, or no answer, gives a log message. The roles changed, so this check cannot prevent the loss of writes. The labels change as for any other handover.

  After an abort of a handover, by Redis or by the operator, the next attempt comes after 30s. The wait doubles up to 5 minutes. An abort never deletes the master, so the rollout waits until the replica takes the role. The status message gives the reason. The metric `redis_operator_controller_redis_checks_total` with the indicator `MASTER_HANDOVER_ABORTED` and the status `UNHEALTHY` counts the aborts.

  At the start of each reconcile, the operator aborts a `FAILOVER` that runs on the pod labelled master. In this mode, only the rollout sends `FAILOVER`, and it waits for the end in the same reconcile. Thus such a failover has no watcher, for example after an operator restart. The operator also aborts a `FAILOVER` that a user sends by hand. This abort has no retry wait, and the metric does not count it. When two pods have the master label, the operator skips this abort, and the checks of the Redis roles run. The check waits 500ms for each step. A master that answers more slowly is skipped, and a later reconcile aborts the failover.

  These cases can still lose acknowledged writes:
  - The replica restarts in place, and takes the role before the next read of its run ID. The old master then copies the older data of the replica, and loses the writes after the last RDB file of the replica. The restart must end in less than 100ms, and the sync must start at once. Redis 6.2 syncs from disk by default, and it starts at once. On Redis 7 and Valkey, a `customConfig` can also start it at once: `repl-diskless-sync no`, `repl-diskless-sync-delay 0`, or `repl-diskless-sync-max-replicas` of 1 or more. With the defaults, the sync waits 5s, and the next read of the run ID aborts the failover first.
  - The operator restarts during the role change, or cannot read the old master until the end of the wait. Then nothing reads the run ID of the replica until the next reconcile. A replica that restarts in place in that time can take the role as above. The next reconcile aborts a failover that still runs.
  - At the start of a reconcile in a role change, the operator cannot read the old master, but the count of masters can. Then the operator can elect another replica, and the old master can become a master again. Writes that clients send to the old master until the next check for multiple masters are lost.

  The operator deletes the master pod, and elects a master after the pod stops, in these cases:
  - `redis.replicas: 1`.
  - Redis before 6.2, which has no `FAILOVER` command, or a `customCommandRenames` entry that disables it.
  - The master refused 3 attempts in a row. A master can see its replicas at an address other than the pod IP. Examples are a service mesh sidecar, NAT and `replica-announce-ip`. An ACL that denies `FAILOVER` has the same result.

  The rollout replaces one pod at a time. With `redis.replicas: 3`, it takes three pod restarts and three initial syncs, one after the other.

The best replica is a synced replica first, then the replica with the highest replication offset, then a Ready pod. This choice loses the fewest writes.

### Sentinel mode

This mode is on when `sentinel.enabled: true`. Sentinel does the failover. The operator repairs what Sentinel cannot. It checks:

- A quorum of Redis pods and a quorum of Sentinel pods run.
- The operator reports the Sentinel peers and the masters that are not pods of the RedisFailover. It changes nothing for them. All RedisFailovers use the master name `mymaster`, and a Sentinel accepts each peer that sends this name. Thus a Sentinel of another RedisFailover at an old IP of an own Sentinel merges the two groups, and they exchange their masters.
  - The operator reads `SENTINEL MASTER` and `SENTINEL SENTINELS` from each running Sentinel, also from a Sentinel in deletion. It reads them before the count of the masters, so two masters do not stop the check.
  - A peer IP that is no Sentinel pod is an unknown peer. A master IP that is no Redis pod is an unknown master address. A pod in each phase counts, also in deletion. A loopback or unspecified master address is not unknown.
  - The other Sentinels keep a deleted Sentinel as a peer until their reset. Thus the operator keeps each pod IP for 5 minutes after the last check that found the pod. In that time, the old IP of a deleted pod is not unknown.
  - The operator lists only the pods of the RedisFailover. Thus an unknown IP is a pod of another RedisFailover, or an old IP of a deleted pod of this RedisFailover, for example after an operator restart.
  - An IP counts when two checks at least 90 seconds apart find it. A reset can come 55 seconds after the deletion of a peer: the hold-off of 25 seconds and a resync of 30 seconds.
  - The operator then logs a Warning with the IPs and the Sentinels that know them, and an Info when the IP is gone.
  - A Sentinel without an answer ends nothing in that check. A Sentinel in deletion is an exception, because on a lost node it never answers.
  - A reconcile without the check, for example without a Sentinel quorum or in another mode, ends the report and sets the gauges to `0`.
  - The gauges are in [Operator metrics](../README.md#operator-metrics).
- Only one Redis works as a master.
- No master: if the old master pod is still stopping, the operator waits. That master can still take writes. Its shutdown script asks Sentinel for a failover only when it can pause the writes (Redis 6.2 or later). Otherwise Sentinel fails over after `down-after-milliseconds`. Then the operator sets the oldest pod as master in these cases: one Redis, no Sentinel quorum, or all Redis replicate from localhost (first boot). When no Sentinel can fail over, the operator promotes the best replica, as in operator-managed mode. Otherwise it waits for the Sentinel failover. The status is `NotHealthy` while it waits.
  - Sentinel promotes only a replica that it knows, and it learns the replicas from its master. After a `SENTINEL RESET` while the master stops, the Sentinels know no replica. Then they stay without a master.
  - The operator promotes only when all Sentinels of the spec run and answer, and each Sentinel flags its master as down. A Sentinel must not monitor a Redis pod or know one as replica. The address 127.0.0.1 of a new Sentinel needs no down flag, because no Redis listens there. Otherwise a Sentinel can still fail over, or the old master can still answer, and a promotion gives two masters.
  - Before the promotion, the operator counts the masters and checks for a stopping master again. A new master in that time gives two masters.
- One master, but the Sentinels give another address as the master: the operator waits. It changes no role label, no replica and no Sentinel. The status is `NotHealthy` while it waits.
  - A Sentinel leader can promote a replica and stop before it tells the other Sentinels. The other Sentinels then fail over the old master to another replica. That failover discards the writes that the first promoted replica accepted.
  - The operator reads `SENTINEL get-master-addr-by-name` from each running Sentinel, also from a Sentinel in deletion. A Sentinel in deletion can still vote until it stops.
  - It waits when a quorum of the Sentinels gives the same address, also when other Sentinels give the Redis master. That quorum can fail over the old master later. A loopback address, for example the 127.0.0.1 of a new Sentinel, and 0.0.0.0 do not count.
  - The address does not have to be a pod. After a forced delete, or when the pod gets a new IP, the Sentinels can still fail over the old address. Then the wait lasts until the Sentinels switch to the Redis master, or until the bound.
  - A Sentinel without a readable answer can be one more vote for an address that at least one other Sentinel gives. Sentinels without an answer alone do not cause a wait. When the operator cannot list the Sentinel pods, it does not change the roles in that reconcile.
  - The wait ends after `down-after-milliseconds` plus `failover-timeout` (default 15s). Then the operator heals as usual.
  - The bound is the largest `down-after-milliseconds` plus the largest `failover-timeout`, of the Sentinel `customConfig` and of the running Sentinels. The Sentinels get a changed `customConfig` only after the wait, and one Sentinel can flag the master down late while another leads the failover. The operator sets the `customConfig` part of the bound at the start of the wait and keeps it.
  - In each check of the wait, the operator reads the timeouts of each Sentinel pod that did not give them yet. It knows a pod by its UID, because a new Sentinel pod can get the IP of an old one. A new value can only make the bound longer. A Sentinel that never answers stays unknown, and the wait ends at the bound of the known values.
  - Another master or another address that the Sentinels give starts a new wait. A reconcile that skips the check, for example after a read error or with the skip annotation, ends the wait. The next check starts a new wait.
  - In the wait, the master Service can have no endpoint, because the old master pod stops.
  - After the operator elects a master, it does not wait for that master, also in later reconciles. At the bound from the election, the operator logs the end of the wait and heals. The election needs Sentinels that know no replica pod and monitor no pod, so they cannot fail over the old address. They give the old address until the operator monitors the elected master. Another Redis master, Sentinels that give the elected master, a reconcile that skips the check and a restart of the operator end this rule. Then the operator waits as for any other master.
    - With `redis.replicas: 1`, the same applies after the operator sets the only pod as master. Other pods stay only while the StatefulSet removes them. The election already gives the master label to that pod, so a wait does not protect its writes.
  - Limits of the wait:
    - A change of `sentinel.replicas` during a wait changes the quorum before the added Sentinels join the others. Then the operator can heal, although the other Sentinels have a quorum for the stale master.
    - A Sentinel in deletion gives a vote, but the operator does not read its timeouts. When they are longer than the known values, the heal runs at the bound that the operator knows.
    - The wait compares the IP of the master, not the port. A Sentinel that gives the correct IP with a wrong port agrees, and the next reconcile repairs its monitor.
- All Redis replicas replicate from the master.
- Redis has the custom configuration and the managed `maxmemory`.
- Stale Redis pods get the new statefulset revision. The operator deletes the master pod only when every Sentinel knows a quorum of the replicas. Otherwise, Sentinel has no replica to promote.
  - The master pod also waits until the Sentinel Deployment has finished its rollout and no Sentinel pod is `Terminating`. The shutdown script of the master sends `SENTINEL FAILOVER` through the Sentinel Service. When the Sentinel that accepts it stops, no other Sentinel knows the promoted replica. A second failover can then lose the writes of that replica. The operator checks again after 5 seconds. The wait ends after 10 minutes, and a new generation of the Deployment starts it again.
- While the master pod stops, the operator does not check or reset the Sentinels. A reset at that time can leave Sentinel with no replica to promote. The status is `NotHealthy` while it waits.
- All Sentinels monitor the same Redis master.
  - While a Sentinel has the flag `failover_in_progress`, the operator sends no `SENTINEL MONITOR` and no `SENTINEL RESET`. These commands end the failover in that Sentinel. The Sentinels can then return to the old master and fail over a second time. The promoted replica then loses its writes. The status is `NotHealthy` while the operator waits.
  - `SENTINEL SET`, for the custom configuration and the password, does not end a failover. The operator sends it during the wait.
  - A Sentinel has the flag while it tries to fail over: as a candidate in the election, and as the leader. Thus the operator reads the flag of each Sentinel before the first change. Just before each change, it reads the flag of the target Sentinel again. A reconcile with one change thus reads each Sentinel and then the target. A reconcile without a change reads no flag.
  - A Sentinel that does not answer gets no change. The reply `No such master with that name`, for example after `SENTINEL REMOVE`, shows no failover. That Sentinel gets the heal at once.
  - Another error, for example an ACL error or a malformed reply, does not show the state. That Sentinel gets no change for 10 seconds after the first error, and the status is `NotHealthy`. Then the operator heals it, so the hold is never permanent.
  - The wait ends after the `failover-timeout` of the Sentinel (default 10s). Then the operator heals the Sentinels, so a failover that does not end does not stop the heal.
  - The flag can stay for two `failover-timeout` periods: one to wait for the promotion and one to reconfigure the replicas. The wait ends after one period on purpose. In the second period, the leader already announces the new master to the other Sentinels.
  - A read of all the Sentinels that finds another Sentinel, another master or an earlier failover state starts a new wait. The master of a Sentinel stays the old master until the failover ends. Thus a second failover of the same Sentinel gets its own wait.
  - The read of one Sentinel before a change does not start a new wait. When it finds a failover that the wait does not cover, only that Sentinel gets no change in that reconcile. Thus the heal waits at most one `failover-timeout` from the first read of a failover. A candidate loses the flag at the end of the election, so it does not hold its own change for long.
- Each Sentinel knows the correct number of Sentinels and replicas. If not, the operator resets that Sentinel. A pod that does not run, for example a Pending pod, is no reason for a reset, because a reset does not add it.
- After a reset or a new monitor, the Sentinel needs some seconds to find the other Sentinels and the replicas again. A failover cannot start in that time, so the operator does not reset it again in the next 25 seconds. The operator keeps this time in memory only, so a restart of the operator ends the wait.
- Sentinel has the custom configuration.
- A Sentinel that does not answer does not stop the heal of the other Sentinels. The status is then `NotHealthy`.

### Bootstrap mode

This mode is on when `bootstrapNode` is set, in either of the modes above. It waits until all Redis pods run. All Redis pods replicate from the external bootstrap node. If Sentinel is on and `bootstrapNode.allowSentinels` is true, the Sentinels monitor that node. The operator heals these Sentinels as in Sentinel mode. The reset hold-off and the rule for a Sentinel that does not answer also apply.

The controller tries to repair most problems. A [split-brain](<https://en.wikipedia.org/wiki/Split-brain_(computing)>) needs a manual fix, except in operator-managed mode when exactly one master has the master label. **For a split-brain that it cannot repair, the controller logs an error and waits for a manual fix**.
