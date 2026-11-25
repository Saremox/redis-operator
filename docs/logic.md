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

Sentinel mode needs a quorum (a majority) of the pods to run, not the full number in the spec. A Pending pod does not block the heal of the others while the running pods are a majority. With 2 replicas, one Pending pod blocks the heal in that mode. Operator-managed mode needs one running Redis pod, because the operator alone decides the master role.

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

A stale pod stays Pending when its `PodScheduled` condition is `False` with the reason `Unschedulable`. The `OnDelete` StatefulSet does not replace such a pod. The operator deletes such a pod when none of its containers started and the StatefulSet status shows the current generation. It does this before the check of the pods that run. In Sentinel mode that check ends the reconcile with 1 or 2 replicas and in bootstrap mode. In operator-managed mode it ends the reconcile only when no Redis pod runs. The operator does not wait for a quorum or for the last replacement, because the pod has no data in memory and serves no client. Its PVC stays, and the StatefulSet creates the pod again on the new revision, so the operator does not delete it again.

### Operator-managed mode

Moved to [Check and heal in operator-managed mode](logic-operator-managed.md).

### Sentinel mode

Moved to [Check and heal in Sentinel mode](logic-sentinel.md).

### Bootstrap mode

This mode is on when `bootstrapNode` is set, in either of the modes above. It waits until all Redis pods run. All Redis pods replicate from the external bootstrap node. If Sentinel is on and `bootstrapNode.allowSentinels` is true, the Sentinels monitor that node. The operator heals these Sentinels as in Sentinel mode. The reset hold-off and the rule for a Sentinel that does not answer also apply.

The controller tries to repair most problems. A [split-brain](<https://en.wikipedia.org/wiki/Split-brain_(computing)>) needs a manual fix, except in operator-managed mode when exactly one master has the master label. **For a split-brain that it cannot repair, the controller logs an error and waits for a manual fix**.
