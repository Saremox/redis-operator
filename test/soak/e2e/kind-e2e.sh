#!/usr/bin/env bash
# Runs the soak tester with the mutator, the filler and the ledger against
# op-basic, sent-basic, op-maxmem, op-noevict, op-full, sent-full, toggle
# and bootstrap on kind. After DURATION seconds the mutator stops starting
# mutations; once the last ones have converged, the script asserts from the
# tester's metrics and logs that every enabled kind converged at least once
# on every instance, that no mutation timed out or was rejected, that there
# were no findings (OOM kills included), that every invariant holds, that
# every probe path and client style succeeds again, that op-noevict
# rejected writes with OOM and op-maxmem evicted keys, that the data was
# verified after every mutation and failover, and that no write was lost
# by an auth change, a Sentinel toggle, or a graceful event on op-full's
# volumes. It then reports the convergence time per kind, the outages per
# mutation, path and client style, the auth windows, scenario C's phases,
# the bootstrap's lag, the pods each kind recreated, the lost writes per
# instance and event, and any findings.
#
# Environment: CLUSTER, KIND_NODE, OPERATOR_VERSION (empty: build the
# operator from this checkout), DURATION (seconds the mutator starts
# mutations, default 2100).
set -euo pipefail

cluster=${CLUSTER:-soak}
node=${KIND_NODE:-v1.35.0}
duration=${DURATION:-2100}
soak=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$soak/../.." && pwd)
skill=$repo/.claude/skills/kind-cluster
export KUBECONFIG=/tmp/kind-$cluster/kubeconfig
artifacts=$soak/bin/kind-e2e-artifacts

instances=(op-basic sent-basic op-maxmem op-noevict op-full sent-full toggle bootstrap)
operator_invariants="pods one_master master_service replication config healthy oom_killed"
sentinel_invariants="pods one_master master_service replication sentinel_agreement config healthy oom_killed"
# toggle's follow its mode at the end, see mode_of.
declare -A paths=([op-basic]="rfrm" [sent-basic]="sentinel rfrm" [op-maxmem]="rfrm" [op-noevict]="rfrm"
  [op-full]="rfrm" [sent-full]="sentinel rfrm" [bootstrap]="rfrs")
declare -A invariants=(
  [op-basic]=$operator_invariants [sent-basic]=$sentinel_invariants
  [op-maxmem]=$operator_invariants [op-noevict]=$operator_invariants
  [op-full]=$operator_invariants [sent-full]=$sentinel_invariants
  [bootstrap]="pods one_master master_service config healthy oom_killed"
)
# The kinds e2e/config.yaml enables.
declare -A kinds=(
  [op-basic]="redis_replicas redis_resources kill_master kill_master_force kill_replica"
  [sent-basic]="redis_replicas sentinel_replicas kill_master kill_master_force kill_replica kill_sentinel"
  [op-maxmem]="redis_memory maxmemory_policy maxmemory_percent kill_replica"
  [op-noevict]="redis_memory maxmemory_policy maxmemory_percent fill_burst"
  [op-full]="password_rotate auth_remove auth_add password_rotate_offline kill_master kill_master_force redis_replicas"
  [sent-full]="password_rotate kill_master kill_sentinel sentinel_replicas"
  [toggle]="sentinel_toggle kill_master"
  [bootstrap]="redis_replicas kill_replica"
)
clients="pooled retrying fresh follower"
# Changes that must lose no write, and the instances with volumes, whose
# graceful kills and scale-downs must lose none either.
lossless_kinds="password_rotate auth_add auth_remove sentinel_toggle password_rotate_offline"
pvc_instances="op-full"
# e2e/config.yaml's observer.convergenceTimeout, plus the longest interval
# and a data verification; scenario C pauses every mutator for up to
# another convergence timeout.
convergence=300
interval=80

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  "$skill/kind-up.sh" "$cluster" "$node"
fi

echo "--- operator"
if [[ -z ${OPERATOR_VERSION:-} ]]; then
  "$skill/build-image.sh" soak-e2e
  helm upgrade --install redis-operator "$repo/charts/redisoperator" \
    --namespace redis-operator --create-namespace \
    --set image.repository=redis-operator --set image.tag=soak-e2e --wait
else
  helm upgrade --install redis-operator oci://ghcr.io/saremox/redis-operator/charts/redis-operator \
    --version "$OPERATOR_VERSION" --namespace redis-operator --create-namespace --wait
fi

echo "--- instances"
# An earlier run's mutations leave the instances changed: start afresh.
kubectl -n redis-soak delete deployment soak --ignore-not-found --wait
kubectl delete namespace "${instances[@]}" --ignore-not-found --wait
for rf in "${instances[@]}"; do
  if [[ $rf == bootstrap ]]; then
    # It replicates from op-basic's master through its Service.
    until kubectl -n op-basic get service rfrm-op-basic >/dev/null 2>&1; do sleep 2; done
    host=$(kubectl -n op-basic get service rfrm-op-basic -o jsonpath='{.spec.clusterIP}')
    sed "s/BOOTSTRAP_HOST/$host/" "$soak/e2e/$rf.yaml" | kubectl apply -f -
    continue
  fi
  kubectl apply -f "$soak/e2e/$rf.yaml"
done
for rf in "${instances[@]}"; do
  until kubectl -n "$rf" get statefulset "rfr-$rf" >/dev/null 2>&1; do sleep 2; done
  replicas=$(kubectl -n "$rf" get redisfailover "$rf" -o jsonpath='{.spec.redis.replicas}')
  kubectl -n "$rf" wait --for=jsonpath='{.status.readyReplicas}'="$replicas" "statefulset/rfr-$rf" --timeout=300s
done
for rf in sent-basic sent-full; do
  until kubectl -n "$rf" get deployment "rfs-$rf" >/dev/null 2>&1; do sleep 2; done
  kubectl -n "$rf" wait --for=jsonpath='{.status.readyReplicas}'=3 "deployment/rfs-$rf" --timeout=300s
done
for rf in "${instances[@]}"; do
  kubectl -n "$rf" wait --for=jsonpath='{.status.state}'=Healthy "redisfailover/$rf" --timeout=300s
done

echo "--- tester"
# The image reuses the Dockerfile's final stage with the binary `make build`
# made on the host, since a Go build inside docker has no module proxy
# access in every environment this runs in.
ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT
mkdir -p "$ctx/out"
cp "$soak/bin/soak" "$ctx/out/soak"
# The tag changes with the binary: the node keeps an image it has, so a
# fixed tag would run the previous build.
tag=e2e-$(sha256sum "$soak/bin/soak" | cut -c1-12)
docker build -q -f "$soak/Dockerfile" --build-context build="$ctx" -t "redis-operator-soak:$tag" "$ctx" >/dev/null
"$skill/registry.sh" push "redis-operator-soak:$tag"

# kustomize only accepts a relative base, and files below the overlay.
overlay=$soak/bin/kind-e2e
mkdir -p "$overlay"
sed "s/^  stopAfter: .*/  stopAfter: ${duration}s/" "$soak/e2e/config.yaml" >"$overlay/config.yaml"
cat >"$overlay/kustomization.yaml" <<YAML
resources:
  - ../../deploy
images:
  - name: ghcr.io/saremox/redis-operator-soak
    newName: redis-operator-soak
    newTag: $tag
configMapGenerator:
  - name: soak-config
    namespace: redis-soak
    behavior: replace
    files:
      - config.yaml
YAML
kubectl apply -k "$overlay"
kubectl -n redis-soak rollout status deployment/soak --timeout=180s
started=$(date +%s)

scrape() {
  metrics=$(kubectl get --raw /api/v1/namespaces/redis-soak/services/soak:metrics/proxy/metrics)
}
# series NAME LABEL... prints the values of the series NAME that have all
# the given labels.
series() {
  local name=$1 line l
  shift
  while read -r line; do
    for l in "$@"; do
      [[ $line == *"$l"* ]] || continue 2
    done
    echo "${line##* }"
  done < <(grep -F "$name{" <<<"$metrics" || true)
}
# value NAME LABEL... prints the single series' value, or 0.
value() {
  local v
  v=$(series "$@")
  echo "${v:-0}"
}
fail=0
check() {
  local msg=$1
  shift
  "$@" || { echo "FAIL: $msg"; fail=1; }
}
eq() { [[ $1 == "$2" ]]; }
ge() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a >= b) }'; }
gt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a > b) }'; }
# table HEADER... prints tab-separated lines as aligned columns.
table() {
  {
    local IFS=$'\t'
    echo "$*"
    cat
  } | awk -F'\t' '
    { for (i = 1; i <= NF; i++) { c[NR, i] = $i; if (length($i) > w[i]) w[i] = length($i) } if (NF > nf) nf = NF }
    END { for (r = 1; r <= NR; r++) { for (i = 1; i < nf; i++) printf("%-" w[i] + 2 "s", c[r, i]); print c[r, nf] } }'
}
logs() {
  kubectl -n redis-soak logs deployment/soak --since="$(($(date +%s) - started + 30))s" | jq -R -c 'fromjson?'
}

echo "--- mutating for ${duration}s"
# The bootstrap's lag behind op-basic's master, sampled every 20s.
lag_samples=$artifacts/bootstrap-lag.txt
mkdir -p "$artifacts"
: >"$lag_samples"
sample_lag() {
  grep -E '^redis_soak_replication_lag_bytes\{[^}]*rf="bootstrap"' <<<"$metrics" |
    sed -E 's/.*pod="([^"]+)".* ([0-9.e+]+)$/\1 \2/' | sed "s/^/$(date +%s) /" >>"$lag_samples" || true
}
minute=0
while (($(date +%s) < started + duration)); do
  sleep 20
  scrape
  sample_lag
  if ((++minute % 3 == 0)); then
    grep -E '^redis_soak_mutation_total' <<<"$metrics" | sed 's/^redis_soak_//' || true
  fi
done
echo "--- waiting for the last mutations to converge"
deadline=$(($(date +%s) + 2 * convergence + interval + 60))
until (($(logs | jq -s '[.[] | select(.msg == "mutator stopped")] | length') == ${#instances[@]})); do
  if (($(date +%s) > deadline)); then
    echo "FAIL: the mutators didn't stop"
    fail=1
    break
  fi
  sleep 10
done
# Let the probes and the observer catch up with the last mutation.
sleep 15
scrape

logs >"$artifacts/soak.jsonl"
kubectl -n redis-operator logs deployment/redis-operator --since="$(($(date +%s) - started + 30))s" >"$artifacts/operator.log"
for rf in "${instances[@]}"; do
  kubectl -n "$rf" get events --sort-by=.lastTimestamp >"$artifacts/events-$rf.txt"
done
echo "$metrics" >"$artifacts/metrics.txt"

# toggle's paths and invariants follow the mode it ended in.
if [[ $(kubectl -n toggle get redisfailover toggle -o jsonpath='{.spec.sentinel.enabled}') == true ]]; then
  paths[toggle]="sentinel rfrm"
  invariants[toggle]=$sentinel_invariants
else
  paths[toggle]="rfrm"
  invariants[toggle]=$operator_invariants
fi

# probes_ok checks that every path and client style is writable and readable
# and succeeded recently; the bootstrap's rfrs is read-only.
probes_ok() {
  local rf path client gauge op last now ok=0 gauges ops
  now=$(date +%s)
  for rf in "${instances[@]}"; do
    for path in ${paths[$rf]}; do
      gauges="writable readable" ops="set get"
      [[ $path == rfrs ]] && gauges=readable ops=get
      for client in $clients; do
        local l=("rf=\"$rf\"" "path=\"$path\"" "client=\"$client\"")
        for gauge in $gauges; do
          eq "$(value "redis_soak_$gauge" "${l[@]}")" 1 ||
            { echo "FAIL: $gauge{rf=$rf,path=$path,client=$client} != 1"; ok=1; }
        done
        for op in $ops; do
          last=$(value redis_soak_last_success_timestamp_seconds "${l[@]}" "op=\"$op\"")
          gt "$last" $((now - 10)) ||
            { echo "FAIL: last_success{rf=$rf,path=$path,client=$client,op=$op} is stale: $last"; ok=1; }
        done
        [[ $path == rfrs ]] && continue
        gt "$(value redis_soak_probe_total "${l[@]}" 'op="wait"' 'result="ok"')" 0 ||
          { echo "FAIL: no WAIT succeeded on rf=$rf,path=$path,client=$client"; ok=1; }
      done
    done
  done
  return $ok
}
# invariants_ok checks that every invariant holds and there is one master.
invariants_ok() {
  local rf inv ok=0
  for rf in "${instances[@]}"; do
    for inv in ${invariants[$rf]}; do
      eq "$(value redis_soak_invariant_ok "rf=\"$rf\"" "invariant=\"$inv\"")" 1 ||
        { echo "FAIL: invariant_ok{rf=$rf,invariant=$inv} != 1"; ok=1; }
    done
    # A bootstrapping instance has no master of its own.
    local masters=1
    [[ $rf == bootstrap ]] && masters=0
    eq "$(value redis_soak_masters "rf=\"$rf\"")" $masters ||
      { echo "FAIL: masters{rf=$rf} != $masters"; ok=1; }
  done
  return $ok
}

echo "--- mutations"
jq -r 'select(.msg == "mutation done" or .msg == "mutation skipped")
  | [.rf, .step, .kind, .result, (.duration_seconds // 0 | . * 10 | round / 10), (.pods_recreated // "-"), (.params // .reason)] | @tsv' \
  "$artifacts/soak.jsonl" | table rf step kind result seconds recreated params
echo
grep -E '^redis_soak_(mutation_total|pods_recreated_total)' <<<"$metrics" | sed 's/^redis_soak_//'

echo "--- convergence per kind (seconds; at least minDwell, in observer ticks)"
jq -rs '[.[] | select(.msg == "mutation done" and .result == "converged")] | group_by([.rf, .kind])[]
  | [.[0].rf, .[0].kind, length, (map(.duration_seconds) | min | round), (map(.duration_seconds) | add / length | round),
     (map(.duration_seconds) | max | round)] | @tsv' "$artifacts/soak.jsonl" |
  table rf kind converged min mean max

echo "--- outages by the mutation they started in (seconds)"
# An outage is attributed to the mutation of its instance that was in
# progress when its first probe failed.
jq -rs '
  def ts: capture("^(?<s>[^.Z]+)(?<f>\\.[0-9]+)?") | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | tonumber);
  (map(select(.msg == "mutating"))) as $starts
  | (map(select(.msg == "mutation done"))) as $dones
  | [.[] | select(.msg == "outage ended") | . as $o
     | (($o.time | ts) - $o.duration_seconds) as $start
     | ([$starts[] | select(.rf == $o.rf and (.time | ts) <= $start)] | last) as $m
     | ([$dones[] | select($m != null and .rf == $o.rf and .step == $m.step)] | first) as $d
     | {rf: $o.rf, path: $o.path, client: $o.client, d: $o.duration_seconds,
        kind: (if $m != null and ($d == null or ($d.time | ts) >= $start) then $m.kind else "none" end)}]
  | group_by([.rf, .kind, .path, .client])[]
  | [.[0].rf, .[0].kind, .[0].path, .[0].client, length, (map(.d) | add * 10 | round / 10), (map(.d) | max * 10 | round / 10)]
  | @tsv' "$artifacts/soak.jsonl" | table rf kind path client outages total max

echo "--- auth windows (seconds of outage that started during each auth change)"
# The follower switches to the new password at once: its outage is how
# long the operator took to apply the change. The other styles keep their
# open connections, or take the Secret's password for new ones.
jq -rs '
  def ts: capture("^(?<s>[^.Z]+)(?<f>\\.[0-9]+)?") | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | tonumber);
  (map(select(.msg == "outage started"))) as $os
  | (map(select(.msg == "outage ended"))) as $oe
  | (map(select(.msg == "mutating"))) as $starts
  | .[] | select(.msg == "mutation done" and (.kind | test("^(password_rotate|auth_add|auth_remove|password_rotate_offline)$"))) | . as $d
  | ([$starts[] | select(.rf == $d.rf and .step == $d.step)] | first) as $m
  | ($m.time | ts) as $from | ($d.time | ts) as $to
  | [$oe[] | select(.rf == $d.rf and ((.time | ts) - .duration_seconds) >= $from and ((.time | ts) - .duration_seconds) <= $to)] as $outs
  | ($outs | group_by([.path, .client])[]) as $g
  | [$d.rf, $d.step, $d.kind, $d.redis_replicas, $g[0].path, $g[0].client, ($g | length),
     ($g | map(.duration_seconds) | add * 10 | round / 10), ($g | map(.duration_seconds) | max * 10 | round / 10),
     ([$os[] | select(.rf == $d.rf and .path == $g[0].path and .client == $g[0].client and (.time | ts) >= $from and (.time | ts) <= $to) | .result] | unique | join(","))]
  | @tsv' "$artifacts/soak.jsonl" | table rf step kind replicas path client outages total max results

echo "--- scenario C phases (seconds)"
jq -r 'select(.msg == "scenario phase done" or .msg == "scenario phase failed")
  | [.rf, .step, .phase, .phase_name, (.duration_seconds * 10 | round / 10), (if .msg == "scenario phase failed" then .error else "ok" end)] | @tsv' \
  "$artifacts/soak.jsonl" | table rf step phase name seconds result

echo "--- Sentinel toggles: outages on the paths that stay (seconds)"
jq -rs '
  def ts: capture("^(?<s>[^.Z]+)(?<f>\\.[0-9]+)?") | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | tonumber);
  (map(select(.msg == "outage ended"))) as $oe
  | (map(select(.msg == "mutating"))) as $starts
  | .[] | select(.msg == "mutation done" and .kind == "sentinel_toggle") | . as $d
  | ([$starts[] | select(.rf == $d.rf and .step == $d.step)] | first) as $m
  | ($m.time | ts) as $from | ($d.time | ts) as $to
  | [$oe[] | select(.rf == $d.rf and .path == "rfrm" and ((.time | ts) - .duration_seconds) >= $from and ((.time | ts) - .duration_seconds) <= $to)] as $outs
  | [$d.step, $d.params, ($d.duration_seconds | round), ($outs | length),
     ($outs | group_by(.client) | map("\(.[0].client)=\(map(.duration_seconds) | add * 10 | round / 10)") | join(" "))]
  | @tsv' "$artifacts/soak.jsonl" | table step change converge_s outages per_client
jq -c 'select(.msg == "path added" or .msg == "path removed") | {time, rf, msg, path}' "$artifacts/soak.jsonl" | grep toggle || true

echo "--- bootstrap: lag behind op-basic's master (bytes) and verifications"
awk '{ n[$2]++; s[$2] += $3; if ($3 > m[$2]) m[$2] = $3; l[$2] = $3 }
  END { for (p in n) printf("%s samples=%d mean=%.0f max=%.0f last=%.0f\n", p, n[p], s[p] / n[p], m[p], l[p]) }' "$lag_samples" | sort
jq -r 'select(.rf == "bootstrap" and (.msg == "data verified" or .msg == "source writes missing on a pod"))
  | [.time, .msg, .event, .step, .keys, .pods, .lost, (.duration_seconds // 0 | . * 10 | round / 10)] | @tsv' "$artifacts/soak.jsonl" |
  table time msg event step keys pods lost seconds
jq -c 'select(.rf == "bootstrap" and .invariant != null and .msg == "invariant violated") | {time, invariant, finding, reason}' "$artifacts/soak.jsonl"

echo "--- findings"
jq -c 'select(.finding == true or (.msg | test("timed out"))) | del(.level, .namespace, .mode)' "$artifacts/soak.jsonl"
grep -E '^redis_soak_findings_total' <<<"$metrics" || echo "none"

echo "--- data verifications"
# Each verification with the mutation it followed: its params (graceful or
# force for kills), the redis replicas then, and whether the master changed.
jq -rs '
  (map(select(.msg == "mutation done")) | map({key: "\(.rf)/\(.step)", value: .}) | from_entries) as $m
  | .[] | select(.msg == "data verified") | ($m["\(.rf)/\(.step)"] // {}) as $d
  | [.rf, .event, (if .step > 0 then .step else "-" end), .failover, .recent, .older, .fill, .lost,
     ($d.redis_replicas // "-"), ($d.params // "-")] | @tsv' "$artifacts/soak.jsonl" |
  table rf event step failover recent older fill lost replicas params
jq -c 'select(.msg == "lost writes") | del(.level, .namespace, .mode)' "$artifacts/soak.jsonl"

echo "--- lost writes by instance and event"
# Kills apart by how the pod was deleted, and whether it was the only
# pod; scale-downs apart by whether they removed the master. A reset is
# the kill of the only pod without a volume.
jq -rs '
  (map(select(.msg == "mutation done")) | map({key: "\(.rf)/\(.step)", value: .}) | from_entries) as $m
  | [.[] | select(.msg == "data verified") | ($m["\(.rf)/\(.step)"] // {}) as $d
     | {rf, lost, event: (.event
         + (if .event == "reset" then " (" + $d.kind + ")" else "" end)
         + (if $d.kind == "kill_replica" or $d.kind == "kill_sentinel" then " " + ($d.params | capture("\\((?<h>[a-z]+)\\)").h) else "" end)
         + (if ($d.kind // "" | test("^kill_")) and $d.redis_replicas == 1 then ", only pod" else "" end)
         + (if $d.kind == "redis_replicas" and .failover then ", master removed" else "" end))}]
  | group_by([.rf, .event])[] | [.[0].rf, .[0].event, length, (map(.lost) | add)] | @tsv' "$artifacts/soak.jsonl" |
  table rf event verifications lost >"$artifacts/lost-writes.txt"
cat "$artifacts/lost-writes.txt"
# Redis 7 waits for its replicas on SIGTERM: a graceful kill, or a
# scale-down that removes the master, should lose nothing.
awk '$NF > 0 && (/^[^ ]+ +kill_master / && !/only pod/ || /graceful/ && !/only pod/ || /master removed/) { print "NOTABLE: writes lost on a graceful path: " $0 }' \
  "$artifacts/lost-writes.txt"
grep -E '^redis_soak_(lost_writes_total|ledger_verified_total)' <<<"$metrics" | sed 's/^redis_soak_//'

echo "--- memory, OOM and evictions"
grep -E '^redis_soak_(dataset_keys|used_memory_bytes|maxmemory_bytes|oom_rejections_total|evicted_keys_total|wait_acked_replicas)' <<<"$metrics" |
  sed 's/^redis_soak_//'
jq -c 'select(.msg == "burst rejected" or .msg == "burst done" or .msg == "data filled") | del(.level, .namespace, .mode)' "$artifacts/soak.jsonl"
grep -F 'op="wait"' <<<"$metrics" | grep -E '^redis_soak_probe_total' | grep -v 'result="ok"' | sed 's/^redis_soak_//' || true

echo "--- config invariant"
grep -E '^redis_soak_invariant_ok.*invariant="(config|oom_killed)"' <<<"$metrics" | sed 's/^redis_soak_//'
jq -rs '[.[] | select(.invariant == "config" and .msg == "invariant violated")] | group_by(.rf)[]
  | [.[0].rf, length, (map(select(.finding)) | length)] | @tsv' "$artifacts/soak.jsonl" | table rf violations findings
grep -oE 'maxmemory (kept at|lowered to) [^"]*' "$artifacts/operator.log" | sort | uniq -c | sort -rn | head -20 || true

# verified_after_mutations prints the mutations the data wasn't verified
# after, as their kind or as a reset.
verified_after_mutations() {
  jq -rs '(map(select(.msg == "data verified")) | map("\(.rf)/\(.step)")) as $v
    | .[] | select(.msg == "mutation done") | "\(.rf)/\(.step)" as $k | select($v | index($k) | not) | "\($k)/\(.kind)"' \
    "$artifacts/soak.jsonl"
}
# lossy_events prints the verifications that lost writes they must not:
# after an auth change or a Sentinel toggle anywhere, and after a graceful
# kill or a scale-down on an instance with volumes. A scale-down that
# removed the master is the known operator race (checker.go, case 1 of
# checkAndHealOperatorManagedMode skips masterPodStopping) and reported
# apart.
lossy_events() {
  jq -rs --arg kinds "$lossless_kinds" --arg pvc "$pvc_instances" '
    (map(select(.msg == "mutation done")) | map({key: "\(.rf)/\(.step)", value: .}) | from_entries) as $m
    | .[] | select(.msg == "data verified" and .lost > 0) | . as $v | ($m["\(.rf)/\(.step)"] // {}) as $d
    | select(($kinds | split(" ") | index($d.kind // ""))
        or (($pvc | split(" ") | index($v.rf)) and ($d.kind == "kill_master" or ($d.kind == "redis_replicas" and ($v.failover | not)))))
    | "\(.rf)/\(.step)/\($d.kind)=\(.lost)"' "$artifacts/soak.jsonl"
}
# race_losses prints the scale-downs on volumes that removed the master and
# lost writes: the known race, excluded from lossy_events.
race_losses() {
  jq -rs --arg pvc "$pvc_instances" '
    (map(select(.msg == "mutation done")) | map({key: "\(.rf)/\(.step)", value: .}) | from_entries) as $m
    | .[] | select(.msg == "data verified" and .lost > 0 and .failover) | . as $v | ($m["\(.rf)/\(.step)"] // {}) as $d
    | select(($pvc | split(" ") | index($v.rf)) and $d.kind == "redis_replicas") | "\(.rf)/\(.step)=\(.lost)"' "$artifacts/soak.jsonl"
}
# verified_after_failovers prints the failovers outside a mutation the data
# wasn't verified after.
verified_after_failovers() {
  jq -rs '
    def ts: capture("^(?<s>[^.Z]+)(?<f>\\.[0-9]+)?") | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | tonumber);
    (map(select(.msg == "data verified" and (.event == "failover" or .event == "reset")))) as $fv
    | (map(select(.msg == "mutating"))) as $starts
    | (map(select(.msg == "mutation done"))) as $dones
    | .[] | select(.msg == "failover") | . as $f | ($f.time | ts) as $t
    | ([$starts[] | select(.rf == $f.rf and (.time | ts) <= $t)] | last) as $m
    | ([$dones[] | select($m != null and .rf == $f.rf and .step == $m.step)] | first) as $d
    | select(($m != null and ($d == null or ($d.time | ts) >= $t)) | not)
    | select([$fv[] | select(.rf == $f.rf and (.time | ts) >= $t)] | length == 0)
    | "\($f.rf)@\($f.time)"' "$artifacts/soak.jsonl"
}

echo "--- assertions"
check "probes" probes_ok
check "invariants" invariants_ok
for rf in "${instances[@]}"; do
  for kind in ${kinds[$rf]}; do
    l=("rf=\"$rf\"" "kind=\"$kind\"")
    check "mutation_total{rf=$rf,kind=$kind,result=converged} = 0" \
      ge "$(value redis_soak_mutation_total "${l[@]}" 'result="converged"')" 1
    for result in timeout rejected; do
      n=$(value redis_soak_mutation_total "${l[@]}" "result=\"$result\"")
      check "mutation_total{rf=$rf,kind=$kind,result=$result} = $n" eq "$n" 0
    done
  done
  if gt "$(value redis_soak_mutation_total "rf=\"$rf\"" 'kind="kill_master"' 'result="converged"')" 0; then
    check "failovers_total{rf=$rf} = 0 after killing the master" \
      gt "$(value redis_soak_failovers_total "rf=\"$rf\"")" 0
  fi
  for n in $(series redis_soak_findings_total "rf=\"$rf\""); do
    check "findings_total{rf=$rf} = $n" eq "$n" 0
  done
  replicas=$(kubectl -n "$rf" get redisfailover "$rf" -o jsonpath='{.spec.redis.replicas}')
  n=$(series redis_soak_server_info "rf=\"$rf\"" | wc -l)
  check "server_info{rf=$rf} has $n series, want $replicas" eq "$n" "$replicas"
done
check "build_info" grep -q '^redis_soak_build_info{operator_version=' <<<"$metrics"
check "oom_rejections_total{rf=op-noevict} = 0" gt "$(value redis_soak_oom_rejections_total 'rf="op-noevict"')" 0
check "the bootstrap was never verified" gt "$(series redis_soak_ledger_verified_total 'rf="bootstrap"' | awk '{ s += $1 } END { print s + 0 }')" 0
n=$(series redis_soak_lost_writes_total 'rf="bootstrap"' | awk '{ s += $1 } END { print s + 0 }')
check "op-basic's writes missing on the bootstrap: $n" eq "$n" 0
lossy=$(lossy_events | xargs)
check "writes lost by lossless events: $lossy" eq "$lossy" ""
race=$(race_losses | xargs)
[[ -z $race ]] || echo "KNOWN RACE (excluded): a scale-down removed the master and lost writes: $race"
check "evicted_keys_total{rf=op-maxmem} = 0" gt "$(value redis_soak_evicted_keys_total 'rf="op-maxmem"')" 0
unverified=$(verified_after_mutations | xargs)
check "data not verified after mutations: $unverified" eq "$unverified" ""
unverified=$(verified_after_failovers | xargs)
check "data not verified after failovers: $unverified" eq "$unverified" ""

if [[ $fail != 0 ]]; then
  echo "--- evidence (all of it in $artifacts)"
  jq -c 'select(.level == "WARN") | del(.namespace, .mode)' "$artifacts/soak.jsonl" | tail -40
  grep -iE 'error|warn|failover|master' "$artifacts/operator.log" | tail -60 || true
  for rf in "${instances[@]}"; do
    grep -v Normal "$artifacts/events-$rf.txt" | tail -20 || true
  done
  kubectl -n redis-operator get events --sort-by=.lastTimestamp | tail -20 || true
  exit 1
fi
echo "PASS: every kind converged on every instance, without findings, every probe succeeds, the data was verified after every mutation and failover, and no lossless event lost writes"
