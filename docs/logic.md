# Controller logic

## Creation pipeline

The Redis-Operator creates Redis Failovers, with all the needed pieces. When an event arrives from Kubernetes (add or sync), the operator does these steps in this order:

1. Delete: if the RedisFailover has a deletion timestamp, the operator removes its metrics and in-memory state, then removes its finalizer. Nothing else runs.
2. Finalizer: the operator adds its finalizer. Without it, the operator never sees a delete and cannot clean up.
3. Skip: if the annotation `redisfailovers.databases.spotahome.com/skip-reconcile` is `"true"`, the operator stops here. Use it to repair a cluster by hand.
4. Validate: `Validate()` rejects an invalid spec and sets the defaults.
5. Ensure: checks that all the pieces needed are created. If a change is performed manually on the objects created, the operator will override them. This is done to ensure a healthy status. It will create the following:
   - Redis service (if exporter enabled)
   - Redis master service and Redis slave service
   - Redis shutdown configmap
   - Redis readiness configmap
   - Redis configmap
   - Redis statefulset, and its PodDisruptionBudget unless `redis.disablePodDisruptionBudget` is true
   - Sentinel service, configmap and deployment. Only when Sentinel is on and, in bootstrap mode, `allowSentinels` is true. Otherwise, the operator deletes them. With them, the operator also creates a Sentinel PodDisruptionBudget unless `sentinel.disablePodDisruptionBudget` is true, and a Sentinel service account unless `sentinel.serviceAccountName` is set.
   - When a flag disables a PodDisruptionBudget or `sentinel.serviceAccountName` is set, the operator deletes the object that it created before. An old PodDisruptionBudget would block node drains.
6. Check & Heal: connects to every Redis and Sentinel and moves them to the desired state. First, it applies a changed password, because every later check authenticates. Then it uses one mode, described below.

## Check & Heal modes

Operator-managed mode and Sentinel mode need a quorum (a majority) of the pods to run, not the full number in the spec. A Pending pod does not block the heal of the others while the running pods are a majority. With 2 replicas, one Pending pod blocks the heal.

A pod rollout updates one stale pod in each reconcile: the replicas first, the master last. It waits until all replicas are in sync.

### Operator-managed mode

This is the default since 4.0 (`sentinel.enabled` not set or `false`). There is no Sentinel, so the operator elects the master itself. It checks:

- A quorum of Redis pods runs.
- Only one Redis works as a master.
- No master: if the old master pod is still stopping, the operator waits, because that master can still take writes. Otherwise it promotes the replica with the highest replication offset, to lose the least data. If it cannot read the offsets, it promotes the oldest pod.
- The master does not answer, or is not a master: the operator promotes the replica with the highest replication offset.
- All Redis slaves replicate from the master.
- Redis has the custom configuration and the managed `maxmemory`.
- Stale Redis pods get the new statefulset revision.

### Sentinel mode

This mode is on when `sentinel.enabled: true`. Sentinel does the failover. The operator repairs what Sentinel cannot. It checks:

- A quorum of Redis pods and a quorum of Sentinel pods run.
- Only one Redis works as a master.
- No master: the operator sets the oldest pod as master if there is one Redis, if the Sentinels have no quorum, or if all Redis replicate from localhost (first boot). Otherwise it waits for the Sentinel failover.
- All Redis slaves replicate from the master.
- Redis has the custom configuration and the managed `maxmemory`.
- Stale Redis pods get the new statefulset revision. The operator deletes the master pod only when every Sentinel knows a quorum of the slaves. Otherwise, Sentinel has no replica to promote.
- All Sentinels monitor the same Redis master.
- Each Sentinel knows the correct number of Sentinels and slaves. If not, the operator resets that Sentinel.
- Sentinel has the custom configuration.

### Bootstrap mode

This mode is on when `bootstrapNode` is set, in either of the modes above. It waits until all Redis pods run. All Redis pods replicate from the external bootstrap node. If Sentinel is on and `bootstrapNode.allowSentinels` is true, the Sentinels monitor that node.

Most of the problems that may occur will be treated and tried to fix by the controller, except the case that there are a [split-brain](<https://en.wikipedia.org/wiki/Split-brain_(computing)>). **If happens to be a split-brain, an error will be logged waiting for manual fix**.
