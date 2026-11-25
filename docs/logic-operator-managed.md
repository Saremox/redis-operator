# Check and heal in operator-managed mode

The shared parts of the check and heal logic are in [logic.md](logic.md).

## Operator-managed mode

This is the default mode (`sentinel.enabled` not set or `false`). There is no Sentinel, so the operator elects the master itself. It checks:

- At least one Redis pod runs. A quorum is not needed, because the operator alone decides the master role.
- Only one Redis works as a master. An old master that did not answer during a failover can come back as a second master. If exactly one running pod has the master label, the other masters become replicas of that pod, because the label shows the elected master. Otherwise the operator reports `multiple masters detected, fix manually`.
- No master: the operator promotes the best replica, or the oldest pod if it finds no replica to promote. Before the promotion, it waits in these cases. The status is `NotHealthy` while it waits.
  - The old master pod is still stopping. That master can still take writes.
  - No pod answers as master, and a Ready pod does not answer. That pod can be the master in a short stall.
  - The master pod does not answer. It gets `sentinel.failoverTimeout` (default 10s) from the first missed check. The operator keeps that time in a pod annotation, so an operator restart keeps the deadline.
  - The running pods are below a quorum, and a pod is not Ready. That pod can hold newer data on its volume, and it answers no `INFO` yet. It holds the election until it is `sentinel.failoverTimeout` old. A pod that the scheduler cannot place passes that age and then holds the election no longer.
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
