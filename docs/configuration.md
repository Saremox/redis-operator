# Configuration of a RedisFailover

## Protect the master from cluster-autoscaler eviction

With `redis.preventMasterEviction: true`, the operator sets the annotation `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` on the master pod.
It sets the value `"true"` on the replica pods. Thus the cluster-autoscaler does not drain the node of the master and does not cause a failover that is not necessary.
The annotation moves with the master. The default is `false`.

When it is `false`, the operator sets the value from `redis.podAnnotations`, or removes the value `"false"`. It keeps a `"true"`,
because a policy or the user can set it.

## Sentinel update strategy and PodDisruptionBudget

Use `sentinel.strategy` to change the update strategy of the Sentinel `Deployment`, for example `rollingUpdate.maxSurge` and `maxUnavailable`.
With required anti-affinity and `replicas == nodes`, the default rolling update cannot schedule a new pod and stops.
This strategy removes an old pod before it creates a new pod:

```yaml
spec:
  sentinel:
    strategy:
      type: RollingUpdate
      rollingUpdate:
        maxSurge: 0
        maxUnavailable: 1
```

The `PodDisruptionBudget` `minAvailable` for each component defaults to `2`, or to `1` when the `replicas` of that component is `2` or less.
To change it, set `redis.podDisruptionBudgetMinAvailable` or `sentinel.podDisruptionBudgetMinAvailable` to an integer or to a percentage string such as `"60%"`.

## Persistence

By default, Redis uses an `emptyDir` volume, so the data is lost when the pod is deleted.

To keep the data, add the full [PersistentVolumeClaim definition](../example/redisfailover/persistent-storage.yaml) under `redis.storage.persistentVolumeClaim`.

**IMPORTANT**: By default, Kubernetes deletes the persistent volume claims when you delete the RedisFailover. To keep them, add `keepAfterDeletion: true` under `redis.storage`. [An example is given](../example/redisfailover/persistent-storage-no-pvc-deletion.yaml). When you add the flag to an existing RedisFailover, the operator removes the owner reference of the RedisFailover from its PVCs. When you remove the flag again, the operator does not add the owner reference back, so these PVCs stay after the RedisFailover is deleted.

The operator removes the owner reference in its next reconcile. Before you delete the RedisFailover, make sure that no PVC shows the owner `RedisFailover`. Replace `<NAME>` with the name of the RedisFailover:

```bash
kubectl get pvc -l app.kubernetes.io/component=redis,app.kubernetes.io/name=<NAME> -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.ownerReferences[*].kind}{"\n"}{end}'
```

To make the volumes larger, increase the storage request in `redis.storage.persistentVolumeClaim`. At each reconcile, the operator resizes the PVCs to the new request. The StorageClass must allow volume expansion (`allowVolumeExpansion: true`). The operator does not make a PVC smaller, and it ignores other changes to the claim. When the API server refuses a resize, the operator only logs the error.

## Node affinity, pod anti-affinity and tolerations

Use node affinity, pod anti-affinity and tolerations to put the pods on isolated groups of nodes. Examples are given for [node affinity](../example/redisfailover/node-affinity.yaml), [pod anti-affinity](../example/redisfailover/pod-anti-affinity.yaml) and [tolerations](../example/redisfailover/tolerations.yaml).

## Topology spread constraints

Use `topologySpreadConstraints` to spread the Redis or Sentinel pods evenly across zones or nodes. See the [topology spread constraints example](../example/redisfailover/topology-spread-contraints.yaml) and the [Kubernetes documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/).

## Custom configurations

You can configure Redis and Sentinel with the `customConfig` option in their spec. It is a list of configuration options and their values. See the [custom config example file](../example/redisfailover/custom-config.yaml).

The operator applies these options with `CONFIG SET` on Redis and `SENTINEL SET mymaster` on Sentinel, so a change does not restart Redis or Sentinel. For this reason, **the configmaps do not change** for this custom configuration. The `customConfig` entries of Redis are not in the `redis.conf` file. To see the actual Redis configuration, use [`redis-cli CONFIG GET *`](https://redis.io/commands/config-get).

**Important**: some Sentinel options need a conversion:

- Configuration on the `sentinel.conf`: `sentinel down-after-milliseconds mymaster 2000`
- Configuration on the `customConfig`: `down-after-milliseconds 2000`

The operator adds `down-after-milliseconds 5000` and `failover-timeout 10000` to the Sentinel `customConfig`, unless `customConfig` sets that option. Thus all the Sentinels use the same timeouts, also after a restart. The operator applies these values to the running Sentinels at the next reconcile, also after an upgrade, without a restart. To use the Sentinel built-in values, set `down-after-milliseconds 30000` and `failover-timeout 180000`. The [4.2.0 migration guide](migrations/4.2.0.md) tells which RedisFailovers get new values on the upgrade to 4.2.0.

**Important 2**: do **NOT** change the options that the operator uses to control Redis and Sentinel, such as `port`, `bind` and `dir`.

Validation rejects a `redis.customCommandRenames` entry for a command that the RedisFailover needs:

- `AUTH`, `CLIENT`, `CONFIG`, `INFO`, `PING`, `PSYNC`, `REPLCONF`, `REPLICAOF` and `SLAVEOF`. The operator, the pod scripts or the replicas send them.
- `EXEC`, `MULTI`, `PUBLISH` and `SUBSCRIBE` when Sentinels run. Sentinel sends them.
- `ACL` when `redis.customConfig` sets `aclfile`. The operator sends `ACL LOAD` to apply the file.

The operator does not reconcile a RedisFailover that fails validation. The status shows `NotHealthy` and the error.

## Managed maxmemory

With `redis.maxMemory`, the operator sets `maxmemory` and `maxmemory-policy` from the memory limit of the Redis container. See the [maxmemory example file](../example/redisfailover/maxmemory.yaml). The memory limit must be at least 64Mi. Otherwise the operator does not manage `maxmemory`, and the status message gives the reason.

`maxmemory` is `percent` (default `75`) of the limit, but at least 32Mi of the limit stays free. The free part must also hold the copy-on-write memory of a fork (`BGSAVE`, full sync). A high `percent` can get a pod OOM-killed under writes. `policy` defaults to `noeviction`.

| Limit | maxmemory |
|---|---|
| 64Mi | 32Mi |
| 96Mi | 64Mi |
| 128Mi | 96Mi |
| 1Gi | 768Mi |

Keys in `customConfig` have priority over the managed values. Validation rejects `replica-ignore-maxmemory no`, because the replicas then evict keys independently of the master. The operator sets `replica-ignore-maxmemory yes` on the running pods.

To move `maxmemory` from `customConfig` to `maxMemory`:

1. Update the CRD and the operator.
2. Add `redis.maxMemory`.
3. Remove `maxmemory` and `maxmemory-policy` from `redis.customConfig`.

When you remove `maxMemory`, the running pods keep their current values until the operator recreates them. An in-place resize does not recreate a pod. To keep the values, set them in `customConfig`.

`maxmemory` follows the smallest Redis pod, because each replica holds the full dataset and the operator can promote any replica. A higher limit applies when all pods run with it. A lower limit applies before the operator replaces the pods.

The operator sets `maxmemory` below the used memory only with an `allkeys-*` policy. A `volatile-*` policy can evict all keys with a TTL and still not fit. With other policies, the operator keeps `maxmemory`, and the status message gives the reason. Until the data fits, the operator does not replace pods with the smaller limit. Pods that Kubernetes recreates for other reasons, for example a node drain, get the smaller limit.

The operator logs each change of `maxmemory` on the master at Info, with the old and the new value, the used memory and the policy. The operator does not read the replicas. It logs a replica only when the value that it sets differs from the value of the master. Thus a replica with a different value can stay without a log line. With `noeviction`, Redis refuses writes when the used memory reaches `maxmemory`.

For small instances, the default `client-output-buffer-limit` for `pubsub` (32mb) and `replica` (256mb) can be larger than the free part of the limit. Lower them with `customConfig`. A replica buffers a full `MULTI`/`EXEC` or `EVAL` before it applies it, so one large batch can cause an OOM kill of a replica.

## Pod updates

A spec change replaces the Redis pods one at a time: the replicas first and the master last. The operator replaces a pod when the previous pod is ready and all replicas are in sync with the master. The operator first replaces an unsynced replica that does not have the current spec, because that replica has no data to lose. A rollout can wait on one pod for more than 10 minutes, for example when a new image cannot load the data of the master. The status message then gives the pod and the reason, for example `rollout waiting on pod rfr-<NAME>-1 for more than 10m: not synced with the master`. The state stays `Healthy`, because the master still serves clients, and the message clears when the rollout continues.

With Sentinel, the master pod waits until the Sentinel Deployment has finished its rollout and no Sentinel pod is `Terminating`. The shutdown script of the master asks a Sentinel for a failover. When that Sentinel stops during the failover, a second failover can lose writes. The status message during the wait is `the master pod replacement waits for the Sentinel rollout`. The wait ends after 10 minutes, so that a Sentinel rollout that does not finish cannot hold the master pod for ever. Then a Warning and the status message give the reason, and the master replacement continues.

The same wait applies when a Sentinel pod is not available outside a rollout, for example a pod in `CrashLoopBackOff`. A restart of the operator starts the 10 minutes again, because the operator keeps the start of the wait in memory.

## Pod updates with one Redis pod

With two or more Redis pods, the master role moves to a replica before the master pod stops, except in the cases below. In operator-managed mode, the operator sends `FAILOVER`. With Sentinel, the default shutdown script asks Sentinel for a failover. With `redis.replicas: 1`, no other pod can take the role. Thus a pod replacement, for example after a change of `redis.image`, stops the writes until the new pod is master and Ready. An in-place resize does not replace the pod.

In operator-managed mode, the operator deletes the master pod without a handover when `FAILOVER` is missing. `FAILOVER` is missing in Redis before 6.2 and after a rename of the command. The operator also deletes the master pod without a handover after 3 refusals of `FAILOVER` in a row, for example from an ACL. With Sentinel, the shutdown script asks for no failover when the pause of the writes fails (see [Custom shutdown script](customization.md#custom-shutdown-script)). The pause fails on Redis before 6.2 and after a password change. In these cases, the writes fail until a new master takes the role.

The master service `rfrm-<NAME>` sends traffic only to a Ready pod. The default readiness probe of Redis starts 30 seconds after the container starts. Thus, with one pod, the probe delay can make the write outage up to 30 seconds longer. The delay does not protect the data. The probe script `ready.sh` reports a master Ready at once, and a replica only after its first sync with the master.

To shorten the outage, set `redis.customReadinessProbe` with a smaller `initialDelaySeconds`. Keep the command and the other values of the default probe:

```yaml
spec:
  redis:
    replicas: 1
    customReadinessProbe:
      initialDelaySeconds: 5
      timeoutSeconds: 5
      exec:
        command:
          - /bin/sh
          - /redis-readiness/ready.sh
```

Until the operator makes the new pod master, the probe fails. After the promotion, the next probe reports the pod Ready. Kubernetes runs the probe every `periodSeconds` (default 10 seconds).

Do not remove `ready.sh` from the probe. `rfrs-<NAME>` uses the Ready state to send reads only to synced replicas. An election also uses the Ready state to rank the candidates.

With `redis.startupConfigMap` or `redis.customStartupProbe`, the readiness probe starts only after the startup probe succeeds. Then a smaller `initialDelaySeconds` can have no effect.

The probe is part of the pod template. When you set or change `redis.customReadinessProbe`, the operator replaces each Redis pod one time. With one pod, this replacement also stops the writes. Set the probe in the same update as the next image change, so the pod restarts only one time. Without `redis.storage.persistentVolumeClaim`, the new pod starts with no data. See [Persistence](#persistence).

## Sentinel failover and write loss

Sentinel does not stop writes on the old master in a failover. If the old master still runs, it accepts writes until Sentinel makes it a replica. With the default timings, this occurs about 10 seconds after the promotion. The old master then copies the data of the new master, and the writes that it acknowledged in that interval are lost. The interval ends earlier if the operator moves the master label or makes the old master a replica first. In a test on kind, a `SENTINEL FAILOVER` lost 11 seconds of writes through `rfrm-<NAME>`.

The default shutdown script reduces this loss when the master pod is deleted. It pauses the writes (`CLIENT PAUSE ... WRITE`) before it requests the failover, so the old master acknowledges no write after the promotion. In 3 deletions on kind without the wait below, no acknowledged write was lost. The new master was ready after about 1 second.

Sentinel promotes the replica with the highest replication offset that it knows. This offset can be 10 seconds old, and Sentinel does not wait until the replica has all writes of the master. Thus, before the failover request, the script waits until each online replica confirms the `master_repl_offset` of the master. Sentinel can still promote a replica that is not online, for example a replica in a full sync.

The replicas confirm once per second, so with healthy replicas the wait adds about 1 second to the write pause. The wait ends at the latest 4 seconds after the start of the script, or 6 seconds when `INFO` does not answer. Then the script requests the failover also when a replica is behind. The script sends the last failover request at most 9 seconds after its start. Thus it has time to make the old master a replica of the new master.

A custom shutdown script that requests a failover without this pause causes the loss. In a test without the pause, 2 deletions lost 8 seconds and 0.1 seconds of writes through `rfrm-<NAME>`. A custom script that uses `REDIS_PASSWORD` has the old password after a password change, until the pod restarts.

The loss can still occur when Sentinel fails over for another reason, for example when the master does not answer but its pod runs.

To lose fewer writes:

- Use a Sentinel client. It gets the new master from Sentinel, and lost 1 to 6 seconds of writes in the tests, not 11 seconds.
- Send `WAIT 1 <timeout>` after an important write, and treat a result of `0` as a failed write. On the old master, `WAIT` returns `0`, because its replicas replicate from the new master. `WAIT` does not undo the write and does not prevent all loss in a failover. It only lets the client detect this case.
- Set `min-replicas-to-write 1` in `redis.customConfig`. The old master then refuses writes when its replicas disconnect. In a test with plain Redis, this reduced the loss from 11 seconds to 0.9 seconds. The master also refuses writes when no replica is connected.

**Known limitation**: the wait for a master pod that stops finds the pod by its `redisfailovers-role=master` label. The operator sets this label only after it counts exactly one master. Sentinel can promote a pod that already stops. An example is a scale-down from 3 to 1 that removes two pods at the same time (`Parallel` pod management). That pod does not have the label yet, so nothing waits for it.
