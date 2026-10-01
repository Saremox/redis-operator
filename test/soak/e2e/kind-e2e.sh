#!/usr/bin/env bash
# Runs the soak tester with the mutator against op-basic and sent-basic on
# kind. After DURATION seconds the mutator stops starting mutations; once
# the last ones have converged, the script asserts from the tester's
# metrics that every enabled kind converged at least once on every
# instance, that no mutation timed out or was rejected, that there were no
# findings, that every invariant holds and that every probe path and client
# style succeeds again. It then reports the convergence time per kind, the
# outages each kill kind caused per path and client style, the pods each
# kind recreated, and any findings.
#
# Environment: CLUSTER, KIND_NODE, OPERATOR_VERSION (empty: build the
# operator from this checkout), DURATION (seconds the mutator starts
# mutations).
set -euo pipefail

cluster=${CLUSTER:-soak}
node=${KIND_NODE:-v1.35.0}
duration=${DURATION:-600}
soak=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$soak/../.." && pwd)
skill=$repo/.claude/skills/kind-cluster
export KUBECONFIG=/tmp/kind-$cluster/kubeconfig
artifacts=$soak/bin/kind-e2e-artifacts

instances=(op-basic sent-basic)
declare -A paths=([op-basic]="rfrm" [sent-basic]="sentinel rfrm")
declare -A invariants=(
  [op-basic]="pods one_master master_service replication healthy"
  [sent-basic]="pods one_master master_service replication sentinel_agreement healthy"
)
# The kinds e2e/config.yaml enables.
declare -A kinds=(
  [op-basic]="redis_replicas redis_resources kill_master kill_replica"
  [sent-basic]="redis_replicas sentinel_replicas kill_master kill_replica kill_sentinel"
)
clients="pooled retrying fresh"
# e2e/config.yaml's observer.convergenceTimeout, plus the longest interval.
convergence=300
interval=20

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
  kubectl apply -f "$soak/e2e/$rf.yaml"
done
for rf in "${instances[@]}"; do
  until kubectl -n "$rf" get statefulset "rfr-$rf" >/dev/null 2>&1; do sleep 2; done
  kubectl -n "$rf" wait --for=jsonpath='{.status.readyReplicas}'=3 "statefulset/rfr-$rf" --timeout=300s
done
until kubectl -n sent-basic get deployment rfs-sent-basic >/dev/null 2>&1; do sleep 2; done
kubectl -n sent-basic wait --for=jsonpath='{.status.readyReplicas}'=3 deployment/rfs-sent-basic --timeout=300s
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
while (($(date +%s) < started + duration)); do
  sleep 60
  scrape
  grep -E '^redis_soak_mutation_total' <<<"$metrics" | sed 's/^redis_soak_//' || true
done
echo "--- waiting for the last mutations to converge"
deadline=$(($(date +%s) + convergence + interval + 60))
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

mkdir -p "$artifacts"
logs >"$artifacts/soak.jsonl"
kubectl -n redis-operator logs deployment/redis-operator --since="$(($(date +%s) - started + 30))s" >"$artifacts/operator.log"
for rf in "${instances[@]}"; do
  kubectl -n "$rf" get events --sort-by=.lastTimestamp >"$artifacts/events-$rf.txt"
done
echo "$metrics" >"$artifacts/metrics.txt"

# probes_ok checks that every path and client style is writable and readable
# and succeeded recently.
probes_ok() {
  local rf path client gauge op last now ok=0
  now=$(date +%s)
  for rf in "${instances[@]}"; do
    for path in ${paths[$rf]}; do
      for client in $clients; do
        local l=("rf=\"$rf\"" "path=\"$path\"" "client=\"$client\"")
        for gauge in writable readable; do
          eq "$(value "redis_soak_$gauge" "${l[@]}")" 1 ||
            { echo "FAIL: $gauge{rf=$rf,path=$path,client=$client} != 1"; ok=1; }
        done
        for op in set get; do
          last=$(value redis_soak_last_success_timestamp_seconds "${l[@]}" "op=\"$op\"")
          gt "$last" $((now - 10)) ||
            { echo "FAIL: last_success{rf=$rf,path=$path,client=$client,op=$op} is stale: $last"; ok=1; }
        done
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
    eq "$(value redis_soak_masters "rf=\"$rf\"")" 1 ||
      { echo "FAIL: masters{rf=$rf} != 1"; ok=1; }
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

echo "--- findings"
jq -c 'select(.finding == true or (.msg | test("timed out"))) | del(.level, .namespace, .mode)' "$artifacts/soak.jsonl"
grep -E '^redis_soak_findings_total' <<<"$metrics" || echo "none"

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

if [[ $fail != 0 ]]; then
  echo "--- evidence (all of it in $artifacts)"
  jq -c 'select(.level == "WARN") | del(.namespace, .mode)' "$artifacts/soak.jsonl" | tail -40
  grep -iE 'error|warn|failover|master' "$artifacts/operator.log" | tail -60 || true
  for rf in "${instances[@]}"; do
    grep -v Normal "$artifacts/events-$rf.txt" | tail -20 || true
  done
  exit 1
fi
echo "PASS: every kind converged on every instance, without findings, and every probe succeeds"
