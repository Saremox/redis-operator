# Controller logic

## Creation pipeline

The operator creates each RedisFailover and the objects that it needs. These events start a reconcile of a RedisFailover:

- An add, an update or a delete of the RedisFailover.
- A resync every `--sync-interval` seconds (default 30).
- An event of one of its pods.
- With `--watch-auth-secrets`, an event of the Secret in `auth.secretPath`.
- The end of the operator-managed failover wait, `sentinel.failoverTimeout`. No pod event comes at that time.

Each reconcile does these steps in this order:

1. Delete: if the RedisFailover has a deletion timestamp, the operator removes its metrics and in-memory state, then removes its finalizer. Nothing else runs. The operator also removes the metrics and the state of a RedisFailover that is gone without a deletion timestamp.
2. Finalizer: the operator adds its finalizer. With it, the operator sees the deletion timestamp before the object is gone.
3. Skip: if the annotation `redisfailovers.databases.spotahome.com/skip-reconcile` is `"true"`, the operator stops here. Use it to repair a cluster by hand.
4. Validate: `Validate()` rejects an invalid spec and sets the defaults.
5. Ensure: the operator creates or updates the objects below. It overwrites a manual change of these objects:
   - Redis service (if exporter enabled)
   - Redis master service and Redis replica service
   - Redis shutdown configmap
   - Redis readiness configmap
   - Redis configmap
   - Redis statefulset, and its PodDisruptionBudget unless `redis.disablePodDisruptionBudget` is true
   - Sentinel service, configmap and deployment. Only when Sentinel is on and, in bootstrap mode, `allowSentinels` is true. Otherwise, the operator deletes them. With them, the operator also creates a Sentinel PodDisruptionBudget unless `sentinel.disablePodDisruptionBudget` is true, and a Sentinel service account unless `sentinel.serviceAccountName` is set.
   - When a flag disables a PodDisruptionBudget or `sentinel.serviceAccountName` is set, the operator deletes the object that it created before. An old PodDisruptionBudget would block node drains. The operator deletes only an object that the RedisFailover owns. It deletes the ServiceAccount when no Sentinel pod uses it, because the old ReplicaSet cannot create a pod without it. Without the RBAC verb, the operator logs a warning and keeps the object.
6. Check & Heal: connects to every Redis and Sentinel and moves them to the desired state. First, it applies a changed password, because every later check authenticates. Then it uses one mode, described below.

## Check & Heal modes

Operator-managed mode and Sentinel mode need a quorum (a majority) of the pods to run, not the full number in the spec. A Pending pod does not block the heal of the others while the running pods are a majority. With 2 replicas, one Pending pod blocks the heal.

A pod rollout replaces one stale pod in each reconcile: the replicas first, the master last. Before each replacement, the operator waits until the last replaced pod is ready and each replica on the new revision is in sync. A stale replica that is not in sync has no data to lose, so the operator replaces it first and does not wait for its sync. When only the container resources change and the kubelet allows it, the operator resizes the pod in place. With `redis.maxMemory`, a lowered memory limit holds the rollout until the `maxmemory` and the memory in use of the master fit the new limit.

### Operator-managed mode

This is the default mode (`sentinel.enabled` not set or `false`). There is no Sentinel, so the operator elects the master itself. It checks:

- A quorum of Redis pods runs.
- Only one Redis works as a master. An old master that did not answer during a failover can come back as a second master. If exactly one running pod has the master label, the other masters become replicas of that pod, because the label shows the master that the operator elected. Otherwise the operator reports `multiple masters detected, fix manually`.
- No master: the operator promotes the best replica, or the oldest pod if it finds no replica to promote. Before the promotion, it waits in these cases. The status is `NotHealthy` while it waits.
  - The old master pod is still stopping. That master can still take writes.
  - No pod answers as master, and a Ready pod does not answer. That pod can be the master in a short stall.
  - The master pod does not answer. It gets `sentinel.failoverTimeout` (default 10s) from the first missed check. The operator keeps that time in a pod annotation, so an operator restart keeps the deadline.
- The master does not answer, or is not a master: the operator waits `sentinel.failoverTimeout` as above, then promotes the best replica. Without a replica to promote, it does not promote the oldest pod. The status message is then `no healthy replica available for failover`.
- All Redis replicas replicate from the master.
- Redis has the custom configuration and the managed `maxmemory`.
- Stale Redis pods get the new statefulset revision.

The best replica is a synced replica first, then the replica with the highest replication offset, then a Ready pod. This choice loses the fewest writes.

### Sentinel mode

This mode is on when `sentinel.enabled: true`. Sentinel does the failover. The operator repairs what Sentinel cannot. It checks:

- A quorum of Redis pods and a quorum of Sentinel pods run.
- Only one Redis works as a master.
- No master: if the old master pod is still stopping, the operator waits. That master can still take writes. Its shutdown script asks Sentinel for a failover only when it can pause the writes (Redis 6.2 or later). Otherwise Sentinel fails over after `down-after-milliseconds`. Then the operator sets the oldest pod as master in these cases: one Redis, no Sentinel quorum, or all Redis replicate from localhost (first boot). When no Sentinel can fail over, the operator promotes the best replica, as in operator-managed mode. Otherwise it waits for the Sentinel failover. The status is `NotHealthy` while it waits.
  - Sentinel promotes only a replica that it knows, and it learns the replicas from its master. After a `SENTINEL RESET` while the master stops, the Sentinels know no replica. Then they stay without a master.
  - The operator promotes only when all Sentinels of the spec run and answer, and each Sentinel flags its master as down. A Sentinel must not monitor a Redis pod or know one as replica. The address 127.0.0.1 of a new Sentinel needs no down flag, because no Redis listens there. Otherwise a Sentinel can still fail over, or the old master can still answer, and a promotion gives two masters.
  - Before the promotion, the operator counts the masters and checks for a stopping master again. A new master in that time gives two masters.
- All Redis replicas replicate from the master.
- Redis has the custom configuration and the managed `maxmemory`.
- Stale Redis pods get the new statefulset revision. The operator deletes the master pod only when every Sentinel knows a quorum of the replicas. Otherwise, Sentinel has no replica to promote.
- While the master pod stops, the operator does not check or reset the Sentinels. A reset at that time can leave Sentinel with no replica to promote. The status is `NotHealthy` while it waits.
- All Sentinels monitor the same Redis master.
- Each Sentinel knows the correct number of Sentinels and replicas. If not, the operator resets that Sentinel. A pod that does not run, for example a Pending pod, is no reason for a reset, because a reset does not add it.
- Sentinel has the custom configuration.

### Bootstrap mode

This mode is on when `bootstrapNode` is set, in either of the modes above. It waits until all Redis pods run. All Redis pods replicate from the external bootstrap node. If Sentinel is on and `bootstrapNode.allowSentinels` is true, the Sentinels monitor that node.

The controller tries to repair most problems. A [split-brain](<https://en.wikipedia.org/wiki/Split-brain_(computing)>) needs a manual fix, except in operator-managed mode when exactly one master has the master label. **For a split-brain that it cannot repair, the controller logs an error and waits for a manual fix**.
