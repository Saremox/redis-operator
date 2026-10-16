# The full profile: the instances of deploy/config.yaml without a server
# version and without autoscale, with a short mutation interval. The seed
# picks each kind of each instance within its first nine steps. An auth kind
# counts only when auth is on or off as the kind needs. A step of the
# autoscaler needs minutes, so the autoscale instances are left out.
del(.chaos, .versions, .edges, .mutation.timeouts)
| .mutation += {interval: "10s", jitter: "10s", seed: 2646446, stopAfter: $stopAfter}
| .instances |= map(select(.chain == null and .version == null and .autoscale == null))
