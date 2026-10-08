# The versions profile: the server version and fork instances of
# deploy/config.yaml, with 2 redis pods each (kind-e2e.sh patches the
# templates), except redis-chain-big, whose 256Mi of data does not fit the
# kind node. Seed 549 picks every kind of every instance and takes every edge
# within 16 steps, also if the changes along the unknown edges all converge.
# The last edge is valkey-9 -> valkey-8 of the instance downgrade.
del(.chaos)
| .observer += {
    convergenceTimeout: "6m",
    # Report only: before the readiness fix (PR #205), the replicas of
    # edge that cannot load the RDB of the master stay Ready.
    replicaReadyWithoutData: false
  }
| .mutation += {
    interval: "10s", jitter: "10s", seed: 549, stopAfter: $stopAfter,
    timeouts: {
      image_upgrade: {base: "3m", perPod: "90s"},
      sentinel_image_upgrade: {base: "3m", perPod: "30s"},
      reset: {base: "3m", perPod: "60s"}
    }
  }
| .instances |= map(select((.chain != null or .version != null) and .name != "redis-chain-big")
    | if .mutations.redisReplicas then .mutations.redisReplicas.max = 2 else . end)
