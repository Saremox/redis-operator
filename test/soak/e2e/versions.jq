# The versions profile: the server version and fork instances of
# deploy/config.yaml, with 2 redis pods each (kind-e2e.sh patches the
# templates), except redis-chain-big: its 256Mi of data make each step too
# slow for the kind job. Seed 291 picks every kind and takes every edge within
# 16 steps, for all 4096 outcomes of the 12 unknown edges.
del(.chaos)
| .observer += {
    convergenceTimeout: "6m",
    # Report only: before the readiness fix (PR #205), the replicas of
    # edge that cannot load the RDB of the master stay Ready.
    replicaReadyWithoutData: false
  }
| .mutation += {
    interval: "10s", jitter: "10s", seed: 291, stopAfter: $stopAfter,
    timeouts: {
      image_upgrade: {base: "3m", perPod: "90s"},
      sentinel_image_upgrade: {base: "3m", perPod: "30s"},
      reset: {base: "3m", perPod: "60s"}
    }
  }
| .instances |= map(select((.chain != null or .version != null) and .name != "redis-chain-big")
    | if .mutations.redisReplicas then .mutations.redisReplicas.max = 2 else . end)
