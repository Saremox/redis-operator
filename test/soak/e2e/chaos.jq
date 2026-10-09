# The chaos profile: the chaos lane on a control plane and two workers, with
# op-basic, sent-basic, op-full on volumes that move with the pods, and
# mixed-sent, whose Sentinels flip between Valkey 9 and Redis 7.2. Seed 54
# does an operator restart, an upgrade and back, a drain of the second
# worker, an upgrade again, and a drain of the first worker. Each action
# starts 60-90s after the previous one converged. $versions are the two
# operator versions that kind-e2e.sh built or names.
.observer.convergenceTimeout = "5m"
| .mutation += {interval: "90s", jitter: "30s", seed: 54, stopAfter: $stopAfter}
| .chaos += {
    kinds: {operator_restart: 1, operator_upgrade: 1, node_drain: 1},
    interval: "60s", jitter: "30s", timeout: "5m",
    drain: {hold: "30s", timeout: "3m"}
  }
| .chaos.upgrade.versions = ($versions | fromjson)
| .instances |= map(select(.name | IN("op-basic", "sent-basic", "op-full", "mixed-sent"))
    | if .name == "mixed-sent" then .mutations = {kinds: {sentinel_image_flip: 1}, sentinelImages: .mutations.sentinelImages}
      else del(.mutations) end)
