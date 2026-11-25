# Resize and autoscaling

## In-place resize

On Kubernetes 1.33 or later, an update that changes only the container CPU or memory resizes the Redis pods in place. The operator does not recreate them, so Redis does not reload data and the master does not fail over. The operator resizes one pod at a time, replicas first. To lower a memory limit in place, Kubernetes 1.35 is necessary. Set `redis.inPlaceResize: Disabled` to always recreate the pods.

The operator recreates a pod instead of a resize in place in these cases:

- The update changes more than container cpu and memory, adds or removes a request or a limit, or changes the QoS class.
- The kubelet does not support in-place resize, or reports the resize as infeasible.
- The kubelet refuses a memory limit below the current usage. The usage includes the page cache, so a retry also fails and the operator does not wait.
- The Redis process uses more memory (`used_memory_rss`) than the lowered limit minus 32Mi. Redis often does not return the memory that evictions free to the OS.
- The kubelet defers or fails the resize for more than 5 minutes, or does not apply it in 5 minutes.

The operator needs `patch` on `pods/resize` and `get` on `controllerrevisions`, which the chart, the kustomize and the example manifests grant. Without them, the operator recreates the pods. In [mode Auto](#mode-auto) with a limit above the spec, the operator keeps a pod whose revision it cannot read. A recreate would restart Redis for a step.

A ResourceQuota or a LimitRange can reject a resize. The operator then does not recreate the pod, because the same rejection applies to the new pod. The rollout holds, and the status message gives the reason. The rollout continues when the quota or the LimitRange allows the new resources.

## Autoscaling

With `redis.autoscaling`, the operator computes a memory limit for the Redis container from the memory use. Mode `Recommend` shows the limit in the status and changes no pod, StatefulSet, resources or `maxmemory`. Mode `Auto` also applies the limit to the pods in place. See [Mode Auto](#mode-auto) and the example files for [Recommend](../example/redisfailover/autoscaling.yaml) and [Auto](../example/redisfailover/autoscaling-auto.yaml).

The fields of `redis.autoscaling`:

- `mode`: `Off` (default), `Recommend` or `Auto`. With `bootstrapNode`, validation accepts only `Off`, because the RedisFailover has no master to sample.
- `memoryMax`: the largest limit that the operator recommends. It must not be less than `redis.resources.limits.memory`, which is the lower bound. When the limit is equal to `memoryMax`, the operator recommends no step, and the reason says that the limit is at `memoryMax`.
- `workload`: `Auto` (default), `Store` or `Cache`. `Auto` uses the `maxmemory-policy` of the spec. `allkeys-*` is a cache, and `noeviction` is a store. `volatile-*` is a store until Redis evicts a key.
- `recreateAfter`: the operator does not use it. Validation rejects it with mode `Auto`, because a step never recreates a pod. In the other modes, validation accepts `0s`, or `5m` and more. The value has at most 16 characters, and each number has at most 6 digits. A larger value can overflow and stop the decode of all RedisFailovers.

For a store, the memory request must be equal to the memory limit, or not set. When the request is lower, the node can run out of memory before the pod reaches its limit.

The operator reads `INFO` from the master one time in each minute at most. It compares `used_memory` with `maxmemory`, or with the memory limit when `maxmemory` is `0`. It ignores a sample while the RedisFailover is not `Healthy` and while Redis loads or syncs data. It also ignores the first 10 minutes after a restart. A restart, which changes the `run_id`, starts the count of windows again. When the clock of the operator goes back, the open window starts again.

When `maxmemory` is set, the ratio does not use the memory limit. When `maxmemory` is above the limit, a pod near its limit can show a low ratio. The operator counts an OOM refusal of an ignored sample at the first sample that it does not ignore. A change of the memory limit starts the count of windows again. A lower limit also removes the recommendation, because the operator calculated the step from the higher limit.

A window of 5 minutes is high when its peak is 85 percent or more. The operator recommends a step after 3 consecutive high windows. It recommends a step at once when the use is 95 percent or more, or when Redis refuses a command with an OOM error. A window below 85 percent removes the recommendation. A step is 25 percent of the current limit, rounded up to a multiple of 64Mi, and `memoryMax` at most. For a cache, the operator gives no recommendation.

The rule of 3 windows needs a sample in each window. A window without a sample ends the sequence of high windows. With a `--sync-interval` of 600 seconds or more, a window can end without a sample. Then only the rules for 95 percent and for OOM errors can give a step. With samples more than 11 minutes apart, the operator also discards the state at each sample, and the OOM rule cannot apply. Pod events also start a reconcile, but do not come at regular times.

`status.autoscaling` shows the result. `recommendedMemory` is empty when the operator recommends no change. `highWindows` stops at 3. `evaluatedAt` is the time of the last change of the recommendation or of `highWindows`. Thus a steady instance does not write its status at each window:

```yaml
status:
  autoscaling:
    workload: Store
    recommendedMemory: 1280Mi
    reason: memory use is high in 3 consecutive windows
    evaluatedAt: "2026-10-12T08:15:00Z"
    highWindows: 3
```

To apply a recommendation, set the memory request and limit in `redis.resources` to the recommended value. The operator then resizes the pods as for each other change of the resources.

The operator keeps its state in the ConfigMap `rfa-<NAME>`. In a steady state, it writes the ConfigMap about one time in each window. When the mode changes to `Off`, the operator deletes the ConfigMap, also when a newer operator version wrote the state. After its start, the operator also deletes this ConfigMap of a RedisFailover without autoscaling and without `status.autoscaling`.

The operator discards a state when its last sample is more than 11 minutes before the current time. Thus a state that stays after `Off` gives no recommendation from an earlier time. Kubernetes deletes the ConfigMap together with the RedisFailover.

The operator does not change a ConfigMap `rfa-<NAME>` that another object controls. It does not update a state that a newer operator version wrote, but mode `Off` deletes it. The reason in the status then tells this.

These gauges have the labels `namespace` and `name`:

- `redis_operator_controller_autoscaler_recommended_memory_bytes`: the recommended limit, or `0`.
- `redis_operator_controller_autoscaler_memory_used_ratio`: the use of the last sample.
- `redis_operator_controller_autoscaler_high_windows`: the number of consecutive high windows.
- `redis_operator_controller_autoscaler_effective_memory_bytes`: the memory limit of the redis container. It is the limit of `redis.resources` until the autoscaler applies a larger limit.
- `redis_operator_controller_autoscaler_step_phase`: the phase of the last step: `0` Idle, `1` Applying, `2` Done, `3` Reverted, `4` Held.

The counter `redis_operator_controller_autoscaler_steps_total` has the labels `namespace`, `name` and `result`. It counts each step one time, at its first result: `done`, `reverted` or `held`.

### Mode Auto

With `redis.autoscaling.mode: Auto`, the operator applies a recommendation as a step. It resizes the pods in place and never recreates a pod for a step. It only increases the limit. The spec of the RedisFailover stays, and `status.autoscaling.effectiveMemory` shows the limit of the pods when it differs from `redis.resources.limits.memory`.

Validation accepts mode `Auto` only with these settings:

- `redis.maxMemory` is set, and the memory limit is at least 64Mi. A step must raise `maxmemory` with the limit. Otherwise the ratio stays high, and the steps continue up to `memoryMax`.
- `redis.customConfig` does not set `maxmemory`.
- `redis.resources.requests.memory` is equal to the memory limit, or not set.
- `memoryMax` is set and not less than the memory limit.
- `bootstrapNode`, `recreateAfter` and `redis.inPlaceResize: Disabled` are not set.

For a cache, the operator gives no recommendation. Thus mode `Auto` makes no step for the workload `Cache`, or for an `allkeys-*` policy with the workload `Auto`.

The operator also checks these conditions before each step. When one fails, no step starts, and `status.autoscaling.reason` tells why:

- Kubernetes 1.33 or later. In-place resize needs it.
- The operator can get the ControllerRevision of the StatefulSet. Without it, the rollout cannot tell a step from a change of the spec.
- No pod has an annotation of a VerticalPodAutoscaler (`vpaUpdates` or `vpaObservedContainers`). Do not use a VerticalPodAutoscaler for the redis pods.
- The rollout is complete, each replica is in sync, and the RedisFailover is `Healthy`.
- The redis container has no `resizePolicy` with `RestartContainer` for memory, in the StatefulSet and in each pod. Then a resize would restart Redis.

The operator needs `patch` on `pods/resize` and `get` on `controllerrevisions`. The chart, the kustomize and the example manifests grant them. Without `pods/resize`, the first resize fails, and the operator reverts the step.

A step uses the rules of Recommend: 3 consecutive high windows, or at once at 95 percent or after an OOM refusal. A step is 25 percent of the current limit, rounded up to a multiple of 64Mi, and `memoryMax` at most. These limits apply between steps:

- Cooldown: the next step starts 15 minutes after the end of a step at the earliest. After a use of 95 percent or an OOM refusal, the cooldown is 5 minutes.
- Budget: at most 6 steps in 24 hours.
- Back-off: after a revert, the next step waits 1 hour. The wait doubles at each further revert, up to 24 hours. A change of the spec ends the wait.

The operator writes the step to the ConfigMap `rfa-<NAME>` first. The next reconcile writes the larger limit to the StatefulSet. Then the operator resizes the pods in place, one at a time: the replicas first and the master last. The master does not fail over. `maxmemory` follows the smallest pod, so it increases when the master has the larger limit. `status.autoscaling.phase` shows the step:

- `Applying`: the pods get the larger limit.
- `Done`: each pod runs with the larger limit on the update revision.
- `Reverted`: the cluster refused the resize before a pod applied it, for example a ResourceQuota, a LimitRange or a node without enough memory. The operator also reverts a step that no pod applied in 15 minutes, for example because no node can hold the pod. The StatefulSet gets the limit before the step. `retryAfter` shows the end of the back-off.
- `Held`: the cluster refused the resize after a pod applied it. Or the step did not end in 15 minutes, and a revert is not safe. The operator does not lower the limit of a pod, so it keeps the larger limit and starts no further step. The phase changes to `Done` when each pod runs with the larger limit, for example after a pod restart.

A revert is not safe when a pod applied the step, or runs or starts from the template of the step. A pod that is absent or stops also blocks a revert, because the StatefulSet can create it from that template. A pod that no node can hold blocks no revert, because it has no data. The operator also holds a step when it cannot remove the resize request of a pod within 30 minutes after the start of the step.

Before the StatefulSet gets the limit before the step, the operator checks the pods again. Then only a pod of the step that exists holds the step. An absent pod does not, because a quota can refuse it at the limit of the step.

The reason of `Reverted` and `Held` gives the refusal that ended the step. A later refusal does not change that reason. Alert on `redis_operator_controller_autoscaler_step_phase` equal to `4`. In the other phases, the reason also shows a refused resize. An example is a change of the spec while the operator cannot read a revision.

A pod can run with a larger limit than the StatefulSet. This happens when the kubelet applies a resize after a revert. It also happens on Kubernetes 1.33 and 1.34, which cannot lower a limit in place. The operator does not change such a pod. A pod that the StatefulSet created from the template of the last step also keeps its limit. The rollout only moves its revision label. `maxmemory` follows the smallest pod, so the larger pod is safe.

The spec of a pod can also request the limit of a step after a revert. The operator then removes only the resize request, and the pod keeps the request. Before Kubernetes 1.35, the API server refuses a lower limit in the pod spec. Thus a later change of the limit holds the rollout at such a pod, and the reason shows the pod. The operator does not recreate it.

`effectiveMemory` shows the limit of the StatefulSet, so it can be lower than the limit of such a pod. A raise of `memoryMax` keeps this rule. A `memoryMax` below the limit of the pod, a spec limit at or above it, or mode `Off` ends the rule. A pod that the rollout already moved to the update revision keeps its limit until it restarts. To give such a pod the limit of the StatefulSet, delete the pod.

A pod that refuses a step stays on its revision, also in mode `Recommend`. The operator does not recreate it in these cases:

- The spec limit is above the limit before the step and below the limit of the step. The template does not change.
- The spec limit is equal to the limit of a step that is `Applying` or `Held`. The step continues. A refusal or the timeout then holds the step, because a revert cannot lower the template.
- The state is lost, not valid, behind the StatefulSet, of another owner or of a newer operator. Until the next change of the spec, each raise to the limit of the StatefulSet can be a step. A pod with a larger limit up to `memoryMax` also keeps its limit, unless the user lowers the spec limit.

Changes of the user:

- A spec limit above the effective limit ends the step. The pods get the spec limit as for each change of the resources.
- A spec limit equal to the effective limit does not change the pods. A step that is `Applying` or `Held` continues until each pod runs with that limit. A step that is done ends, so a later lower spec limit gives each pod the lower limit.
- A spec limit below the effective limit does not change the pods. The reason then contains `redis.resources.limits.memory is below effectiveMemory`.
- A `memoryMax` below the effective limit lowers the limit of the pods to `memoryMax`. This is a change of the user: Kubernetes 1.35 lowers it in place, and older versions recreate the pods.
- Mode `Recommend` keeps the effective limit, and completes a step that is `Applying` or `Held`. It starts no step. The status shows no phase of an earlier step.
- Mode `Off` gives the pods the spec limit. This lowers the limit as above. To keep the limit, pin it first.
- A ConfigMap `rfa-<NAME>` that is lost, not valid or behind the StatefulSet gives the limit of the live StatefulSet. An example of a state behind the StatefulSet is a restored older copy. No step starts until the next change of the spec, also when the live limit is the spec limit. That change gives the phase `Held` to a frozen limit above the spec limit, until each pod runs with that limit. A spec limit above the frozen limit ends this step. After the step, the budget and the back-off start again.
- A ConfigMap `rfa-<NAME>` of another owner or of a newer operator version also gives the limit of the live StatefulSet. The operator does not change that ConfigMap and starts no step.
- A state with a last sample more than 11 minutes before the current time gives the limit of the live StatefulSet. A state that stays after mode `Off` thus does not change the pods.

The operator lowers the limit of the StatefulSet only after a user change or for the revert of its own step. Before each Ensure, it reads the limit of the live StatefulSet, also in mode `Recommend`. Without the StatefulSet, for example after a delete with `--cascade=orphan`, the largest redis pod gives the limit. The user changes are a lower `memoryMax`, and a lower spec limit while the StatefulSet has the earlier spec limit.

The annotation `redisfailovers.databases.spotahome.com/spec-memory-limit` of the StatefulSet holds the spec limit of the last Ensure. The operator owns this annotation, so do not change it. A larger value counts as a lower spec limit of the user, and the StatefulSet then gets the spec limit. The pods keep their limit until they restart.

Without autoscaling, a reconcile reads the StatefulSet less often. Mode `Recommend` reads it one time more in each reconcile. Mode `Auto` reads it three times more, and lists the pods one time.

To pin the effective limit, for example before mode `Off` or a downgrade of the operator:

1. Read `status.autoscaling.effectiveMemory`. When it is empty, the pods have the spec limit.
2. Set `redis.resources.requests.memory` and `redis.resources.limits.memory` to that value.
3. When `status.autoscaling.phase` is `Applying` or `Held`, wait until it is `Done`. Mode `Off` recreates a pod that refuses the limit. When the phase stays `Held`, set the spec limit back to the value before step 2. This ends the step, and the pods of the step keep their limit. Then find the cause of the refusal, or delete the pod that refuses the limit.
4. Set `redis.autoscaling.mode` to `Recommend` or `Off`.
5. Wait until `status.state` is `Healthy`.

An operator version without mode `Auto` rejects such a RedisFailover in validation. It then stops all reconciles of the RedisFailover, also the failover of the operator-managed mode. The 4.1.x CRD removes `redis.autoscaling`, and then the 4.1.x operator gives the pods the spec limit. Thus pin the limit before a downgrade. See the [migration guide](migrations/4.2.0.md).
