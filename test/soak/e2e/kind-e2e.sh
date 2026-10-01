#!/usr/bin/env bash
# Runs the soak tester against op-basic on kind and asserts from its metrics
# that the rfrm probes succeed for every client style.
#
# Environment: CLUSTER, KIND_NODE, OPERATOR_VERSION (empty: build the
# operator from this checkout), DURATION (seconds to let the tester run).
set -euo pipefail

cluster=${CLUSTER:-soak}
node=${KIND_NODE:-v1.35.0}
duration=${DURATION:-120}
soak=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$soak/../.." && pwd)
skill=$repo/.claude/skills/kind-cluster
export KUBECONFIG=/tmp/kind-$cluster/kubeconfig

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

echo "--- op-basic"
kubectl apply -f "$soak/e2e/op-basic.yaml"
until kubectl -n op-basic get statefulset rfr-op-basic >/dev/null 2>&1; do sleep 2; done
kubectl -n op-basic wait --for=jsonpath='{.status.readyReplicas}'=3 statefulset/rfr-op-basic --timeout=300s
kubectl -n op-basic wait --for=jsonpath='{.status.state}'=Healthy redisfailover/op-basic --timeout=300s

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

metrics=$(kubectl get --raw /api/v1/namespaces/redis-soak/services/soak:metrics/proxy/metrics)
grep -E '^redis_soak_(build_info|writable|readable|probe_total|last_success_timestamp_seconds|outage_duration_seconds_count)' <<<"$metrics"
kubectl -n redis-soak logs deployment/soak --tail=20

# Probes run once a second; allow for the tester's startup.
min=$((duration * 8 / 10))
now=$(date +%s)
fail=0
# series NAME LABEL... prints the value of op-basic's rfrm series NAME that
# has all the given labels.
series() {
  local name=$1 line l
  shift
  while read -r line; do
    for l in 'rf="op-basic"' 'path="rfrm"' "$@"; do
      [[ $line == *"$l"* ]] || continue 2
    done
    echo "${line##* }"
  done < <(grep -F "$name{" <<<"$metrics" || true)
}
for client in pooled retrying fresh; do
  for gauge in writable readable; do
    v=$(series "redis_soak_$gauge" "client=\"$client\"")
    [[ $v == 1 ]] || { echo "FAIL: $gauge{client=$client} = ${v:-missing}"; fail=1; }
  done
  for op in set get; do
    ok=$(series redis_soak_probe_total "client=\"$client\"" "op=\"$op\"" 'result="ok"')
    [[ ${ok:-0} -ge $min ]] || { echo "FAIL: probe_total{client=$client,op=$op,result=ok} = ${ok:-0} < $min"; fail=1; }
    last=$(series redis_soak_last_success_timestamp_seconds "client=\"$client\"" "op=\"$op\"")
    awk -v l="${last:-0}" -v n="$now" 'BEGIN { exit !(n - l < 10) }' ||
      { echo "FAIL: last_success{client=$client,op=$op} is stale: ${last:-missing}"; fail=1; }
  done
done
grep -q '^redis_soak_build_info{operator_version=' <<<"$metrics" || { echo "FAIL: no build_info"; fail=1; }

if [[ $fail != 0 ]]; then
  exit 1
fi
echo "PASS: rfrm probes succeed for every client style"
