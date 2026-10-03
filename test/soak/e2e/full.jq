# The full profile: the instances of deploy/config.yaml without a server
# version, with a short mutation interval. The seed picks each kind of each
# instance within its first seven steps, and within eleven on op-full.
del(.chaos, .versions, .edges, .mutation.timeouts)
| .mutation += {interval: "10s", jitter: "10s", seed: 104782, stopAfter: $stopAfter}
| .instances |= map(select(.chain == null and .version == null))
