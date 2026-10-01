#!/usr/bin/env bash
# Runs the soak tester against op-basic and sent-basic on kind and asserts
# from its metrics that every probe path and client style succeeds and every
# invariant holds. Then it kills each instance's master pod and asserts that
# the instances fail over and recover, and reports the outage and violation
# durations it measured.
#
# Environment: CLUSTER, KIND_NODE, OPERATOR_VERSION (empty: build the
# operator from this checkout), DURATION (seconds to let the tester run),
# RECOVERY (seconds the instances get to recover from the master kill).
set -euo pipefail

cluster=${CLUSTER:-soak}
node=${KIND_NODE:-v1.35.0}
duration=${DURATION:-120}
# The replacement pod can't be Ready for 30-40s (the operator's readiness
# probe has a 30s initial delay and the default 10s period), after a
# failover that takes up to 10s (operator failoverTimeout) or ~15s
# (Sentinel down-after 5s, then the election). That is about a minute;
# three minutes leaves room for a slow node without hiding a hang.
recovery=${RECOVERY:-180}
soak=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$soak/../.." && pwd)
skill=$repo/.claude/skills/kind-cluster
export KUBECONFIG=/tmp/kind-$cluster/kubeconfig

instances=(op-basic sent-basic)
declare -A paths=([op-basic]="rfrm" [sent-basic]="sentinel rfrm")
declare -A invariants=(
  [op-basic]="pods one_master master_service replication healthy"
  [sent-basic]="pods one_master master_service replication sentinel_agreement healthy"
)
clients="pooled retrying fresh"

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
docker build -q -f "$soak/Dockerfile" --build-context build="$ctx" -t redis-operator-soak:e2e "$ctx" >/dev/null
"$skill/registry.sh" push redis-operator-soak:e2e

# kustomize only accepts a relative base.
overlay=$soak/bin/kind-e2e
mkdir -p "$overlay"
cat >"$overlay/kustomization.yaml" <<YAML
resources:
  - ../../deploy
images:
  - name: ghcr.io/saremox/redis-operator-soak
    newName: redis-operator-soak
    newTag: e2e
YAML
kubectl apply -k "$overlay"
kubectl -n redis-soak rollout restart deployment/soak
kubectl -n redis-soak rollout status deployment/soak --timeout=180s

echo "--- running for ${duration}s"
sleep "$duration"

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

# probes_ok checks that every path and client style is writable and readable
# and succeeded recently.
probes_ok() {
  local quiet=${1:-} rf path client gauge op last now ok=0
  now=$(date +%s)
  for rf in "${instances[@]}"; do
    for path in ${paths[$rf]}; do
      for client in $clients; do
        local l=("rf=\"$rf\"" "path=\"$path\"" "client=\"$client\"")
        for gauge in writable readable; do
          eq "$(value "redis_soak_$gauge" "${l[@]}")" 1 ||
            { [[ -n $quiet ]] || echo "FAIL: $gauge{rf=$rf,path=$path,client=$client} != 1"; ok=1; }
        done
        for op in set get; do
          last=$(value redis_soak_last_success_timestamp_seconds "${l[@]}" "op=\"$op\"")
          gt "$last" $((now - 10)) ||
            { [[ -n $quiet ]] || echo "FAIL: last_success{rf=$rf,path=$path,client=$client,op=$op} is stale: $last"; ok=1; }
        done
      done
    done
  done
  return $ok
}
# invariants_ok checks that every invariant holds and there is one master.
invariants_ok() {
  local quiet=${1:-} rf inv ok=0
  for rf in "${instances[@]}"; do
    for inv in ${invariants[$rf]}; do
      eq "$(value redis_soak_invariant_ok "rf=\"$rf\"" "invariant=\"$inv\"")" 1 ||
        { [[ -n $quiet ]] || echo "FAIL: invariant_ok{rf=$rf,invariant=$inv} != 1"; ok=1; }
    done
    eq "$(value redis_soak_masters "rf=\"$rf\"")" 1 ||
      { [[ -n $quiet ]] || echo "FAIL: masters{rf=$rf} != 1"; ok=1; }
  done
  return $ok
}

scrape
grep -E '^redis_soak_(build_info|writable|readable|invariant_ok|masters|failovers_total|findings_total|rf_healthy|server_info|replication_lag_bytes)' <<<"$metrics"
kubectl -n redis-soak logs deployment/soak --tail=20

# Probes run once a second; allow for the tester's startup.
min=$((duration * 8 / 10))
check "probes" probes_ok
check "invariants" invariants_ok
for rf in "${instances[@]}"; do
  for path in ${paths[$rf]}; do
    for client in $clients; do
      for op in set get; do
        ok=$(value redis_soak_probe_total "rf=\"$rf\"" "path=\"$path\"" "client=\"$client\"" "op=\"$op\"" 'result="ok"')
        check "probe_total{rf=$rf,path=$path,client=$client,op=$op,result=ok} = $ok < $min" ge "$ok" "$min"
      done
    done
  done
  n=$(series redis_soak_server_info "rf=\"$rf\"" | wc -l)
  check "server_info{rf=$rf} has $n series, want 3" eq "$n" 3
  check "findings_total{rf=$rf} before the master kill" eq "$(series redis_soak_findings_total "rf=\"$rf\"")" ""
done
check "build_info" grep -q '^redis_soak_build_info{operator_version=' <<<"$metrics"
if [[ $fail != 0 ]]; then
  exit 1
fi
echo "PASS: every path and client style probes successfully and every invariant holds"

echo "--- killing the master pods"
before=$metrics
declare -A failovers
for rf in "${instances[@]}"; do
  failovers[$rf]=$(value redis_soak_failovers_total "rf=\"$rf\"")
done
killed_at=$(date +%s)
for rf in "${instances[@]}"; do
  master=$(kubectl -n "$rf" get pods -l redisfailovers-role=master -o name)
  echo "$rf: deleting $master"
  kubectl -n "$rf" delete "$master" --grace-period=0 --force
done

# Wait for a violation to show before waiting for the recovery.
sleep 15
deadline=$((killed_at + recovery))
while :; do
  scrape
  recovered=1
  for rf in "${instances[@]}"; do
    gt "$(value redis_soak_failovers_total "rf=\"$rf\"")" "${failovers[$rf]}" || recovered=0
  done
  if [[ $recovered == 1 ]] && invariants_ok quiet && probes_ok quiet; then
    break
  fi
  if (($(date +%s) > deadline)); then
    echo "FAIL: not recovered ${recovery}s after the master kill"
    probes_ok || true
    invariants_ok || true
    for rf in "${instances[@]}"; do
      echo "failovers_total{rf=$rf}: ${failovers[$rf]} -> $(value redis_soak_failovers_total "rf=\"$rf\"")"
    done
    kubectl -n redis-soak logs deployment/soak --since=5m | grep -v '"level":"DEBUG"' | tail -60
    exit 1
  fi
  sleep 5
done
echo "PASS: recovered $(($(date +%s) - killed_at))s after the master kill"

# Let the restored invariants' and probes' state settle into the metrics,
# then check the probes once more.
sleep 10
scrape
check "probes after the recovery" probes_ok
check "invariants after the recovery" invariants_ok

# delta NAME LABEL... prints how much a series grew since the kill.
delta() {
  local now then
  now=$(value "$@")
  then=$(metrics=$before value "$@")
  awk -v a="$now" -v b="$then" 'BEGIN { printf "%.3f", a - b }'
}
echo "--- measured during the master kill"
printf '%-11s %-9s %-9s %8s %10s\n' rf path client outages seconds
for rf in "${instances[@]}"; do
  for path in ${paths[$rf]}; do
    for client in $clients; do
      l=("rf=\"$rf\"" "path=\"$path\"" "client=\"$client\"")
      printf '%-11s %-9s %-9s %8.0f %10s\n' "$rf" "$path" "$client" \
        "$(delta redis_soak_outage_duration_seconds_count "${l[@]}")" \
        "$(delta redis_soak_outage_duration_seconds_sum "${l[@]}")"
    done
  done
done
printf '\n%-11s %-19s %10s %10s %9s\n' rf invariant violations seconds findings
for rf in "${instances[@]}"; do
  for inv in ${invariants[$rf]}; do
    l=("rf=\"$rf\"" "invariant=\"$inv\"")
    printf '%-11s %-19s %10.0f %10s %9.0f\n' "$rf" "$inv" \
      "$(delta redis_soak_invariant_violation_seconds_count "${l[@]}")" \
      "$(delta redis_soak_invariant_violation_seconds_sum "${l[@]}")" \
      "$(delta redis_soak_findings_total "${l[@]}")"
  done
done
echo
grep -E '^redis_soak_(failovers_total|masters|findings_total|replication_lag_bytes|server_info)' <<<"$metrics"
echo
kubectl -n redis-soak logs deployment/soak --since="$(($(date +%s) - killed_at + 5))s" |
  jq -R -c 'fromjson? | select(.msg | test("outage|invariant|failover|window")) | del(.time, .level, .namespace, .mode)'

if [[ $fail != 0 ]]; then
  exit 1
fi
echo "PASS: both instances failed over and recovered, and every probe succeeds again"
