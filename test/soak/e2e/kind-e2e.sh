#!/usr/bin/env bash
# Runs the soak tester on kind for one profile, and asserts the results from
# its metrics. Each profile is a jq program over deploy/config.yaml:
# e2e/full.jq (default), e2e/versions.jq and e2e/chaos.jq. The logs, the
# events and the last scrape stay in bin/kind-e2e-artifacts.
#
# Environment:
# - CLUSTER, KIND_NODE, SUBNET: the kind cluster, see kind-up.sh.
# - E2E_PROFILE: full, versions or chaos.
# - DURATION: how many seconds the tester starts mutations. Default 2100,
#   3000 for versions, 1500 for chaos.
# - OPERATOR_VERSION: a released version to install. Empty builds the
#   operator from this checkout.
# - UPGRADE_FROM, UPGRADE_TO (chaos only): the git refs to build and upgrade
#   between (default 4.2.0-rc2 and origin/main). With OPERATOR_VERSION,
#   UPGRADE_FROM is the release to upgrade from.
set -euo pipefail
shopt -s nullglob

cluster=${CLUSTER:-soak}
profile=${E2E_PROFILE:-full}
soak=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$soak/../.." && pwd)
skill=$repo/.claude/skills/kind-cluster
ns=redis-soak-instances
export KUBECONFIG=/tmp/kind-$cluster/kubeconfig
artifacts=$soak/bin/kind-e2e-artifacts
# kustomize only accepts a relative base, and files below the overlay.
overlay=$soak/bin/kind-e2e
case $profile in
full) duration=${DURATION:-2100} ;;
versions) duration=${DURATION:-3000} ;;
chaos) duration=${DURATION:-1500} ;;
*)
  echo "E2E_PROFILE must be full, versions or chaos" >&2
  exit 1
  ;;
esac
rm -rf "$overlay" "$artifacts"
mkdir -p "$overlay" "$artifacts"

# A failure keeps the cluster state in the artifacts, and prints the state
# of each RedisFailover and the last operator log lines into the job log.
on_exit() {
  local rc=$?
  ((rc == 0)) && return
  [[ -f $KUBECONFIG ]] || return
  kubectl get redisfailovers,pods -A -o wide >"$artifacts/state.txt" 2>&1 || true
  kubectl get events -A --sort-by=.lastTimestamp >"$artifacts/events.txt" 2>&1 || true
  kubectl -n redis-operator logs deployment/redis-operator --tail=5000 >"$artifacts/operator.log" 2>&1 || true
  kubectl -n redis-soak logs deployment/soak >"$artifacts/soak.jsonl" 2>&1 || true
  kubectl -n "$ns" get redisfailovers -o custom-columns=NAME:.metadata.name,STATE:.status.state,MESSAGE:.status.message || true
  tail -n 60 "$artifacts/operator.log" || true
}
trap on_exit EXIT

# shared_path makes the StorageClass shared-path: a clone of the kind
# local-path provisioner whose volumes are in /shared, which every node
# mounts. A pod then moves to another node with its volume, as on network
# storage.
shared_path() {
  kubectl -n local-path-storage get configmap local-path-config -o json |
    jq '.metadata = {name: "shared-path-config", namespace: "local-path-storage"}
      | .data["config.json"] = "{\"sharedFileSystemPath\": \"/shared\"}"' | kubectl apply -f -
  kubectl -n local-path-storage get deployment local-path-provisioner -o json |
    jq '.metadata = {name: "shared-path-provisioner", namespace: "local-path-storage"} | del(.status)
      | .spec.selector.matchLabels.app = "shared-path-provisioner"
      | .spec.template.metadata.labels.app = "shared-path-provisioner"
      | .spec.template.spec.containers[0].command += ["--provisioner-name", "rancher.io/shared-path", "--configmap-name", "shared-path-config"]
      | .spec.template.spec.volumes[0].configMap.name = "shared-path-config"' | kubectl apply -f -
  kubectl apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: shared-path
provisioner: rancher.io/shared-path
volumeBindingMode: Immediate
reclaimPolicy: Delete
EOF
  kubectl -n local-path-storage rollout status deployment/shared-path-provisioner --timeout=180s
  # As in most clusters, a drain takes CoreDNS down one pod at a time.
  kubectl -n kube-system create poddisruptionbudget coredns --selector=k8s-app=kube-dns --min-available=1
}

# build_version builds the operator of a git ref, serves its image through
# the local registry, and packages its chart. It prints the version as JSON
# for chaos.upgrade.versions. build-image.sh cannot build an older ref: the
# runtime stage of its Dockerfile installs packages, which the sandbox
# cannot reach.
build_version() {
  local name=$1 ref=$2 src sha
  sha=$(git -C "$repo" rev-parse --short "$ref^{commit}")
  src=$(mktemp -d)
  git -C "$repo" archive "$sha" | tar -x -C "$src"
  (cd "$src" && CGO_ENABLED=0 go build -o out/redis-operator -ldflags "-w" ./cmd/redisoperator)
  printf 'FROM alpine:latest\nCOPY redis-operator /usr/local/bin/\nUSER 1000:1000\nENTRYPOINT ["/usr/local/bin/redis-operator"]\n' >"$src/out/Dockerfile"
  "$skill/registry.sh" pull alpine:latest >&2
  docker build -q -t "redis-operator:src-$sha" "$src/out" >/dev/null
  "$skill/registry.sh" push "redis-operator:src-$sha" >&2
  helm package "$src/charts/redisoperator" --destination "$src/out" >/dev/null
  mv "$src"/out/redis-operator-*.tgz "$overlay/chart-$name.tgz"
  rm -rf "$src"
  jq -n --arg n "$ref" --arg c "chart-$name.tgz" --arg i "redis-operator:src-$sha" '{name: $n, chart: $c, image: $i}'
}

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  if [[ $profile == chaos ]]; then
    WORKERS=2 SHARED=/tmp/kind-$cluster/shared "$skill/kind-up.sh" "$cluster" "${KIND_NODE:-v1.35.0}" "${SUBNET:-244}"
    shared_path
  else
    "$skill/kind-up.sh" "$cluster" "${KIND_NODE:-v1.35.0}" "${SUBNET:-244}"
  fi
fi

echo "--- operator"
versions=[]
if [[ $profile == chaos ]]; then
  # The two versions of the upgrade; the first is installed, with the CRD
  # hook. The operator, like the tester, runs on the control plane, which no
  # drain picks.
  if [[ -z ${OPERATOR_VERSION:-} ]]; then
    a=$(build_version from "${UPGRADE_FROM:-4.2.0-rc2}")
    b=$(build_version to "${UPGRADE_TO:-origin/main}")
    versions=$(jq -s . <<<"$a $b")
  else
    versions=$(jq -n --arg from "${UPGRADE_FROM:?UPGRADE_FROM is the release to upgrade from}" --arg to "$OPERATOR_VERSION" \
      '[$from, $to] | map({name: ., chart: "oci://ghcr.io/saremox/redis-operator/charts/redis-operator", version: ., image: "ghcr.io/saremox/redis-operator:\(.)"})')
  fi
  chart=$(jq -r '.[0].chart' <<<"$versions")
  [[ $chart == oci://* ]] || chart=$overlay/$chart
  version=$(jq -r '.[0].version // ""' <<<"$versions")
  image=$(jq -r '.[0].image' <<<"$versions")
  # The kubectl of the CRD hook.
  "$skill/registry.sh" push rancher/kubectl:v1.36.2
  helm upgrade --install redis-operator "$chart" ${version:+--version "$version"} --namespace redis-operator --create-namespace \
    --set image.repository="${image%:*}" --set image.tag="${image##*:}" --wait -f - <<'YAML'
crds: {upgradeHook: {enabled: true}}
nodeSelector: {node-role.kubernetes.io/control-plane: ""}
tolerations: [{key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}]
YAML
elif [[ -z ${OPERATOR_VERSION:-} ]]; then
  "$skill/build-image.sh" soak-e2e
  helm upgrade --install redis-operator "$repo/charts/redisoperator" --namespace redis-operator --create-namespace \
    --set image.repository=redis-operator --set image.tag=soak-e2e --wait
else
  # kind-up.sh installs the CRD of this checkout, and helm does not replace
  # an existing CRD.
  helm show crds oci://ghcr.io/saremox/redis-operator/charts/redis-operator --version "$OPERATOR_VERSION" |
    kubectl apply --server-side --force-conflicts -f -
  helm upgrade --install redis-operator oci://ghcr.io/saremox/redis-operator/charts/redis-operator \
    --version "$OPERATOR_VERSION" --namespace redis-operator --create-namespace --wait
fi

echo "--- tester"
yq -y --arg stopAfter "${duration}s" --arg versions "$versions" -f "$soak/e2e/$profile.jq" "$soak/deploy/config.yaml" >"$overlay/config.yaml"
case $profile in
# The node has room for the version instances only with 2 pods each.
versions) yq -y '.spec.redis.replicas = 2' "$soak/deploy/instances.yaml" >"$overlay/instances.yaml" ;;
chaos) yq -y 'if .metadata.name == "op-full" then .spec.redis.storage.persistentVolumeClaim.spec.storageClassName = "shared-path" else . end' \
  "$soak/deploy/instances.yaml" >"$overlay/instances.yaml" ;;
*) cp "$soak/deploy/instances.yaml" "$overlay/" ;;
esac
# The nodes pull every image through the local registry.
for img in $(yq -r '.versions[]?.image' "$overlay/config.yaml" | sort -u); do
  "$skill/registry.sh" push "$img"
done
# The image reuses the final stage of the Dockerfile with the binary that
# `make build` made on the host. The tag changes with the binary, because
# the node keeps an image that it has.
ctx=$(mktemp -d)
mkdir -p "$ctx/out"
cp "$soak/bin/soak" "$ctx/out/soak"
tag=e2e-$(sha256sum "$soak/bin/soak" | cut -c1-12)
docker build -q -f "$soak/Dockerfile" --build-context build="$ctx" -t "redis-operator-soak:$tag" "$ctx" >/dev/null
rm -rf "$ctx"
"$skill/registry.sh" push "redis-operator-soak:$tag"
{
  echo "resources: [../../deploy]"
  [[ $profile == chaos ]] && echo "components: [../../deploy/chaos]"
  cat <<YAML
images:
  - {name: ghcr.io/saremox/redis-operator-soak, newName: redis-operator-soak, newTag: $tag}
configMapGenerator:
  - name: soak-config
    namespace: redis-soak
    behavior: replace
    files: [$(cd "$overlay" && echo config.yaml instances.yaml chart-*.tgz | tr ' ' ,)]
YAML
  # The tester runs on the control plane, which no drain picks.
  [[ $profile == chaos ]] && cat <<'YAML'
patches:
  - target: {kind: Deployment, name: soak}
    patch: |-
      - {op: add, path: /spec/template/spec/nodeSelector, value: {node-role.kubernetes.io/control-plane: ""}}
      - {op: add, path: /spec/template/spec/tolerations, value: [{key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}]}
YAML
} >"$overlay/kustomization.yaml"
# Start afresh: an earlier run leaves its instances changed.
kubectl -n redis-soak delete deployment soak --ignore-not-found --wait
kubectl delete namespace "$ns" --ignore-not-found --wait
# Server-side: with the charts, the ConfigMap is too large for the
# last-applied annotation of a client-side apply.
kubectl apply --server-side --force-conflicts -k "$overlay"
kubectl -n redis-soak rollout status deployment/soak --timeout=180s
started=$(date +%s)

logs() {
  kubectl -n redis-soak logs deployment/soak --since="$(($(date +%s) - started + 30))s" | jq -R -c 'fromjson?'
}
echo "--- mutating for ${duration}s"
while (($(date +%s) < started + duration)); do
  sleep 300
  echo "$(logs | jq -s '[.[] | select(.msg == "mutation done" or .msg == "chaos done")] | length') mutations and chaos actions done"
done
# Every mutator, and the chaos lane, logs when it stopped. The last change
# can take more than one convergence timeout, for example scenario C or an
# upgrade and back.
stops=$(yq '([.instances[] | select(.mutations.kinds)] | length) + (if .chaos.kinds then 1 else 0 end)' "$overlay/config.yaml")
deadline=$(($(date +%s) + 2400))
until (($(logs | jq -s '[.[] | select(.msg == "mutator stopped" or .msg == "chaos stopped")] | length') == stops)); do
  if (($(date +%s) > deadline)); then
    echo "FAIL: the mutators did not stop"
    exit 1
  fi
  sleep 10
done
# Let the probes and the observer catch up with the last mutation.
sleep 15
metrics=$(kubectl get --raw /api/v1/namespaces/redis-soak/services/soak:metrics/proxy/metrics | grep '^redis_soak_')
logs >"$artifacts/soak.jsonl"
echo "$metrics" >"$artifacts/metrics.txt"
kubectl -n redis-operator logs deployment/redis-operator --since="$(($(date +%s) - started + 30))s" >"$artifacts/operator.log"
kubectl -n "$ns" get events --sort-by=.lastTimestamp >"$artifacts/events.txt"

echo "--- assertions"
fail=0
# expect DESCRIPTION [AWK_OPTION...] PROGRAM fails if the awk PROGRAM prints
# a series of the scrape that breaks the rule. l(NAME) is the value of the
# label NAME.
expect() {
  local msg=$1 bad
  shift
  bad=$(awk "${@:1:$#-1}" 'function l(n) { return match($0, "[{,]" n "=\"[^\"]*\"") ? substr($0, RSTART + length(n) + 3, RLENGTH - length(n) - 4) : "" }
    '"${!#}" <<<"$metrics")
  if [[ -n $bad ]]; then
    echo "FAIL: $msg:"
    sed 's/^/  /' <<<"$bad"
    fail=1
  fi
}
now=$(date +%s)
expect "every path and client style writes and reads" '/^redis_soak_(writable|readable)\{/ && $NF != 1'
expect "every probe succeeded recently" -v now="$now" '/^redis_soak_last_success_timestamp_seconds\{/ && $NF < now - 30'
expect "every invariant holds" '/^redis_soak_invariant_ok\{/ && $NF != 1'
expect "no findings" '/^redis_soak_findings_total\{/ && $NF > 0'
expect "no event that must lose no write lost one" '/^redis_soak_unexpected_lost_writes_total\{/ && $NF > 0'
# A bootstrapping instance, which has the rfrs path, has no master.
expect "one master" '/^redis_soak_readable\{/ && l("path") == "rfrs" { boot[l("rf")] = 1 }
  /^redis_soak_masters\{/ { m[l("rf")] = $NF }
  END { for (rf in m) if (m[rf] != (rf in boot ? 0 : 1)) print rf, m[rf] }'
# Every enabled kind converged, and none timed out or was rejected. A
# version change along an edge that can fail is judged by the edge below.
judged=$(yq -r '.instances[] | select(.chain.expect and (.chain.expect | index("ok") | not)) | .name' "$overlay/config.yaml" | paste -sd'|' -)
expect "every kind converged, none timed out or was rejected" -v judged="^(${judged:-none})$" '
  /^redis_soak_mutation_total\{/ && !(l("rf") ~ judged && l("kind") == "image_upgrade") {
    k = l("rf") " " l("kind")
    if (l("result") == "converged") { n[k] += 0; if ($NF > 0) ok[k] = 1 }
    if (l("result") ~ /^(timeout|rejected)$/ && $NF > 0) print
  }
  END { for (k in n) if (!(k in ok)) print k, "never converged" }'
expect "a master kill failed over" '/^redis_soak_mutation_total\{/ && l("kind") == "kill_master" && l("result") == "converged" && $NF > 0 { killed[l("rf")] = 1 }
  /^redis_soak_failovers_total\{/ { f[l("rf")] = $NF }
  END { for (rf in killed) if (f[rf] == 0) print rf }'
expect "a burst past maxmemory was rejected" '/^redis_soak_mutation_total\{/ && l("kind") == "fill_burst" && l("result") == "converged" && $NF > 0 { burst[l("rf")] = 1 }
  /^redis_soak_oom_rejections_total\{/ { oom[l("rf")] = $NF }
  END { for (rf in burst) if (oom[rf] == 0) print rf }'
expect "the data of every instance was verified" '/^redis_soak_ledger_verified_total\{/ { v[l("rf")] += $NF }
  END { for (rf in v) if (v[rf] == 0) print rf }'
case $profile in
versions)
  # Every edge was taken, every ok edge ended ok, and none failed unsafely.
  edges=$(yq -r '.edges[] | "\(.from) \(.to) \(.expect)"' "$overlay/config.yaml")
  expect "every edge was taken, and ended as expected" -v edges="$edges" '
    BEGIN { n = split(edges, es, "\n"); for (i = 1; i <= n; i++) { split(es[i], f, " "); want[f[1] " " f[2]] = f[3] } }
    /^redis_soak_version_transition_total\{/ && $NF > 0 {
      e = l("from") " " l("to")
      taken[e] = 1
      if (l("result") == "failed_unsafe" || want[e] == "ok" && l("result") != "ok") print
    }
    END { for (k in want) if (!(k in taken)) print k, "never taken" }'
  ;;
chaos)
  expect "every chaos kind converged, none timed out or failed" '/^redis_soak_chaos_total\{/ {
      if (l("result") == "converged") { n[l("kind")] += 0; if ($NF > 0) ok[l("kind")] = 1 }
      if (l("result") ~ /^(timeout|failed)$/ && $NF > 0) print
    }
    END { for (k in n) if (!(k in ok)) print k, "never converged" }'
  inflight=$(jq -r 'select(.msg == "chaos" and (.in_flight // [] | length) > 0) | "\(.step) \(.kind): \(.in_flight | join(","))"' "$artifacts/soak.jsonl")
  [[ -z $inflight ]] || {
    echo "FAIL: changes in flight when a chaos action started: $inflight"
    fail=1
  }
  ;;
esac

if ((fail)); then
  echo "--- evidence (all of it in $artifacts)"
  jq -c 'select(.level == "WARN" or .level == "ERROR") | del(.namespace, .mode)' "$artifacts/soak.jsonl" | tail -40
  grep -iE 'level=(error|warning)' "$artifacts/operator.log" | tail -40 || true
  exit 1
fi
echo "PASS: $profile"
