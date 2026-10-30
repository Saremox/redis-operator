# Controller logic

## Creation pipeline

The Redis-Operator creates Redis Failovers, with all the needed pieces. So, when a event arrives from Kubernetes (add or sync), the following steps are executed:

- Ensure: checks that all the pieces needed are created. It is important to notice that if a change is performed manually on the objects created, the operator will override them. This is done to ensure a healthy status. It will create the following:
  - Redis service (if exporter enabled)
  - Redis configmap
  - Redis shutdown configmap
  - Redis statefulset
  - Sentinel service
  - Sentinel configmap
  - Sentinel deployment
- Check & Heal: will connect to every Redis and Sentinel and will ensure that they are working as they are supposed to do. If this is not the case, it will reconfigure the nodes to move them to the desire state. It will check the following:
  - Number of Redis is equal as the set on the RF spec
  - Number of Sentinel is equal as the set on the RF spec
  - Only one Redis working as a master
  - All Redis slaves have the same master
  - All Redis slaves are connected to the master
  - All Sentinels points to the same Redis master
  - Sentinel has not death nodes
  - Sentinel knows the correct slave number
  - Ensure Redis has the custom configuration set
  - Ensure Sentinel has the custom configuration set

The controller tries to repair most problems. A [split-brain](<https://en.wikipedia.org/wiki/Split-brain_(computing)>) needs a manual fix, except in operator-managed mode when exactly one pod that runs has the master label. Then the controller makes the other masters replicas of that pod. **For a split-brain that it cannot repair, the controller logs an error and waits for a manual fix**.

## Sentinel peers

In Sentinel mode, the operator also checks the peers and the masters of the Sentinels. All RedisFailovers use the master name `mymaster`, and a Sentinel accepts each peer that sends this name. Thus a Sentinel of another RedisFailover at an old IP of an own Sentinel merges the two groups, and they exchange their masters. See [Isolation with a NetworkPolicy](#isolation-with-a-networkpolicy).

The operator reports the Sentinel peers and the masters that are not pods of the RedisFailover:

- The operator reads `SENTINEL MASTER` and `SENTINEL SENTINELS` from each Sentinel that runs, also from a Sentinel in deletion. It reads them before the count of the masters, so two masters do not stop the check.
- A peer IP that is no Sentinel pod is an unknown peer. A master IP that is no Redis pod is an unknown master address. A pod in each phase counts, also in deletion. A loopback or unspecified master address is not unknown.
- A peer is alive when it sent a hello in the last 10 seconds. Only a hello can merge two groups. Without the age of the hello in the reply, a peer without the flags `s_down` and `disconnected` is alive.
- The other Sentinels keep a deleted Sentinel as a peer until their reset. Thus the operator keeps each pod IP for 5 minutes after the last check that found the pod. In that time, the old IP of a deleted pod is not unknown.
- In the 5 minutes, an alive peer at such an IP is unknown when the pod at that IP did not have its run ID. That peer is another Sentinel, which took the IP. Without a known run ID, each alive peer at the IP is unknown. A dead peer at the IP keeps the 5 minutes, because that is the normal state after the deletion of a Sentinel pod.
- The operator learns the run IDs from the alive peers at the IPs of the Sentinel pods. It keeps them as long as the IP of the pod.
- The operator lists only the pods of the RedisFailover. An unknown IP is a pod of another RedisFailover. It can also be an old IP of a deleted pod of this RedisFailover, for example after an operator restart.
- An IP counts when two checks at least 90 seconds apart find it. A hold can delay the reset of a stale peer, for example the hold-off of 25 seconds and a resync of 30 seconds.
- The operator then logs a Warning with the IPs and the Sentinels that know them, and an Info when the IP is gone. The gauges `redis_operator_controller_unknown_sentinel_peers` and `redis_operator_controller_sentinel_unknown_master` show the count.
- A Sentinel without an answer ends nothing in that check. A Sentinel in deletion is an exception, because on a lost node it never answers.
- A reconcile without the check, for example without a Sentinel quorum or in another mode, ends the report and sets the gauges to `0`.

The operator checks that no Sentinel knows a stale peer. A stale peer is a peer IP that no current Sentinel pod has, without the memory of 5 minutes. A Sentinel keeps a deleted Sentinel as a peer until a reset. When a Sentinel of another RedisFailover gets that IP, the two groups merge, and the count of the Sentinels does not change.

- The operator resets only the Sentinels that know the stale peer. A pod in each phase has its IP, also a pod in deletion. Thus a peer is stale only after Kubernetes deleted its pod object.
- The rules of the other resets apply: the hold-off of 25 seconds, the failover hold, and no reset while the master pod stops. The reset starts the hold-off, so the check by count does not reset the Sentinel again in that reconcile.
- The deletion of a pod starts a reconcile, so the reset comes about one second after the deletion. A wait or an error earlier in the reconcile also holds the reset.
- A reset removes a dead peer for all time. A Sentinel of another group that sends hellos comes back in about 2 seconds. A check can find the peer again on the same Sentinel in the 5 minutes after the reset. Then that Sentinel gets no reset while the peer sends hellos.
- After a reset, the Sentinel learns the replicas again at its next `INFO` of the master. In a test with Redis 7.0, this took about 4 seconds. Until then, it cannot promote a replica. When the master stops in that time, that Sentinel never learns the replicas. The operator does not reset without a Redis master, or while the master pod stops.

The operator checks that the Sentinels know no address of another group. When they know one, the operator sends no `SENTINEL MONITOR`, `SENTINEL REMOVE` or `SENTINEL RESET` to the Sentinels. The other group returns each changed Sentinel to its master in about 2 seconds. Thus a change only moves the master of the Sentinels again, and the merge stays.

- An address of another group is an alive unknown peer that came back after a reset, as above. It is also an alive peer at the old IP of a Sentinel pod with a run ID that the pod did not have.
- A peer that came back with a run ID of the pod at its IP is an own Sentinel. Its pod had a forced deletion, and its process still runs. It is no address of another group. The operator keeps the run IDs of an IP while a Sentinel knows an alive peer with one of them, also after the 5 minutes. Such a Sentinel gets one reset in each 5 minutes.
- A peer of another group stays an address of another group until a check of that Sentinel finds it dead or gone. The memory of 5 minutes for the reset and the run IDs does not end it.
- An unknown master address counts when the same Sentinel knows an alive unknown peer. Without such a peer, a monitor of the Redis master ends the state, so the operator heals. For example, a Sentinel can follow the reused IP of an old master to a Redis without Sentinels.
- The master can be down. With another Redis password, the Sentinel cannot authenticate and flags the master of the other group as `s_down`. The entry of a deleted own pod does not count, because it has a run ID of that pod.
- A Sentinel pod can be on a node without an IP in its status. Then a peer at the IP of a pod of the last 5 minutes does not count. The pod can get such an IP, and its Sentinel can join the others before the pod status has the IP. Peers at other IPs still count.
- A Sentinel that monitors `127.0.0.1`, or no master, and knows no alive unknown peer still gets its `SENTINEL MONITOR`. Without it, that Sentinel gives `127.0.0.1` to the clients.
- The operator still sends `SENTINEL SET` for the custom configuration. It still heals the Redis pods, the replicas and the master, because the clients use the Redis data path.
- The operator logs a Warning with the addresses at the start, and an Info at the end. The status is `NotHealthy` and names the IPs. The state ends when a check of all the Sentinels finds no such address. Then the operator resets the Sentinels that still know a stale peer.
- A Sentinel without an answer, or a peer that the item about a pod without an IP hides, keeps the state. It keeps it for at most 60 seconds after the last check that found the addresses. Two resyncs fit in this time, so one failed read does not end the state. A Sentinel in a crash loop does not hold the heal of the others for all time.
- This check does not repair a merge. It stops the changes of the operator that keep the merge. The other group can still promote the wrong pods. To end a merge, isolate the two groups with a NetworkPolicy, and then restart the Sentinel pods of one group. A CNI plugin that tracks connections can keep the open links between the Sentinels, so the hellos can continue until the restart. When the hellos stop, this state ends at the next check after 10 seconds, and the heal resets the peers of the other group.

### Isolation with a NetworkPolicy

The Sentinel port has no password, and all RedisFailovers use the master name `mymaster`. Thus the Sentinels of two RedisFailovers can merge when a pod gets an IP that the Sentinels of the other RedisFailover still know. A merged group can promote the wrong pods, and the pods can lose acknowledged writes. With equal Redis passwords, a Redis pod can replicate from the other RedisFailover and lose all its data.

The checks above make the time for a merge shorter and find a merge. They do not remove the hazard. A master name for each RedisFailover is the planned complete fix. Until then, isolate each RedisFailover with NetworkPolicy rules for ingress and egress. Such rules block the hello messages between the groups. See [NetworkPolicy](../README.md#networkpolicy) for the rules and an example.
