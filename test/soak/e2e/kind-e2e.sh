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
# With E2E_PROFILE=versions it runs only the server version and fork
# instances of e2e/config-versions.yaml instead, which the tester creates
# from their templates, and asserts that every ok edge ends ok, that no
# version change failed unsafely, and that the chains completed and reset;
# it reports every edge with its mixed window, outages and losses, how the
# unknown edges failed, the Valkey compatibility checks and the resets.
#
# With E2E_PROFILE=chaos it runs the chaos lane of e2e/config-chaos.yaml on
# a control plane and two workers, against four small instances: it
# installs UPGRADE_FROM, upgrades to UPGRADE_TO and back, restarts the
# operator and drains each worker. It asserts that every action converged
# with no change in flight, that no restart or upgrade lost writes, nor a
# drain of the instance on volumes, and that the dashboard and alerts of
# deploy/monitoring work against a Prometheus and a Grafana in the
# cluster. It reports the time without an operator, the CRD hook, the
# drains with their blocked evictions, and every action's outages and
# losses.
#
# Environment: CLUSTER, KIND_NODE, SUBNET (the pod subnet's second octet),
# OPERATOR_VERSION (empty: build the operator from this checkout),
# E2E_PROFILE (full, versions or chaos), DURATION (seconds the mutator
# starts mutations, default 2100, 3000 for versions, 1500 for chaos), and
# for chaos UPGRADE_FROM and UPGRADE_TO: git refs built from source
# (default 4.2.0-rc2 and origin/main), or with OPERATOR_VERSION a released
# tag to upgrade from (required) and to OPERATOR_VERSION.
set -euo pipefail

cluster=${CLUSTER:-soak}
node=${KIND_NODE:-v1.35.0}
subnet=${SUBNET:-244}
profile=${E2E_PROFILE:-full}
soak=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$soak/../.." && pwd)
skill=$repo/.claude/skills/kind-cluster
export KUBECONFIG=/tmp/kind-$cluster/kubeconfig
artifacts=$soak/bin/kind-e2e-artifacts

operator_invariants="pods one_master master_service replication replica_ready_without_data config healthy oom_killed"
sentinel_invariants="pods one_master master_service replication sentinel_agreement replica_ready_without_data config healthy oom_killed"
clients="pooled retrying fresh follower"
# Every instance of both profiles, whose namespaces a run starts afresh.
all_instances=(op-basic sent-basic op-maxmem op-noevict op-full sent-full toggle bootstrap
  redis-chain redis-chain-sent migrate migrate-sent edge valkey-op valkey-sent mixed-sent
  chaos-op chaos-pvc chaos-sent)
case $profile in
full)
  config=$soak/e2e/config.yaml
  duration=${DURATION:-2100}
  instances=(op-basic sent-basic op-maxmem op-noevict op-full sent-full toggle bootstrap)
  # toggle's follow its mode at the end, see mode_of.
  declare -A paths=([op-basic]="rfrm" [sent-basic]="sentinel rfrm" [op-maxmem]="rfrm" [op-noevict]="rfrm"
    [op-full]="rfrm" [sent-full]="sentinel rfrm" [bootstrap]="rfrs")
  declare -A invariants=(
    [op-basic]=$operator_invariants [sent-basic]=$sentinel_invariants
    [op-maxmem]=$operator_invariants [op-noevict]=$operator_invariants
    [op-full]=$operator_invariants [sent-full]=$sentinel_invariants
    [bootstrap]="pods one_master master_service replica_ready_without_data config healthy oom_killed"
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
  # Changes that must lose no write, and the instances with volumes, whose
  # graceful kills and scale-downs must lose none either.
  lossless_kinds="password_rotate auth_add auth_remove sentinel_toggle password_rotate_offline"
  pvc_instances="op-full"
  # e2e/config.yaml's observer.convergenceTimeout, plus the longest
  # interval and a data verification; scenario C pauses every mutator for
  # up to another convergence timeout.
  convergence=480
  interval=80
  ;;
versions)
  config=$soak/e2e/config-versions.yaml
  duration=${DURATION:-3000}
  instances=(redis-chain redis-chain-sent migrate migrate-sent edge valkey-op valkey-sent mixed-sent)
  declare -A paths=([redis-chain]="rfrm" [redis-chain-sent]="sentinel rfrm" [migrate]="rfrm" [migrate-sent]="sentinel rfrm"
    [edge]="rfrm" [valkey-op]="rfrm" [valkey-sent]="sentinel rfrm" [mixed-sent]="sentinel rfrm")
  declare -A invariants=(
    [redis-chain]=$operator_invariants [redis-chain-sent]=$sentinel_invariants
    [migrate]=$operator_invariants [migrate-sent]=$sentinel_invariants
    [edge]=$operator_invariants [valkey-op]=$operator_invariants
    [valkey-sent]=$sentinel_invariants [mixed-sent]=$sentinel_invariants
  )
  # The kinds e2e/config-versions.yaml enables, which must converge;
  # image_upgrade resets at the end of a chain. edge's image changes don't
  # converge and are judged by their edges instead.
  declare -A kinds=(
    [redis-chain]="image_upgrade reset redis_replicas redis_resources"
    [redis-chain-sent]="image_upgrade reset"
    [migrate]="image_upgrade reset"
    [migrate-sent]="image_upgrade sentinel_image_upgrade reset"
    [edge]="reset"
    [valkey-op]="redis_replicas redis_resources redis_memory kill_master kill_master_force kill_replica"
    [valkey-sent]="password_rotate kill_master kill_sentinel sentinel_replicas"
    [mixed-sent]="kill_master kill_replica kill_sentinel"
  )
  # The chains, and the instances with volumes, whose rollovers along ok
  # edges must lose no write.
  chains="redis-chain redis-chain-sent migrate migrate-sent edge"
  pvc_instances="redis-chain migrate migrate-sent"
  # Its image_upgrade timeout for 2 pods and a reset, plus the longest
  # interval and a data verification.
  convergence=720
  interval=80
  ;;
chaos)
  config=$soak/e2e/config-chaos.yaml
  duration=${DURATION:-1500}
  instances=(chaos-op chaos-pvc chaos-sent mixed-sent)
  declare -A paths=([chaos-op]="rfrm" [chaos-pvc]="rfrm" [chaos-sent]="sentinel rfrm" [mixed-sent]="sentinel rfrm")
  declare -A invariants=(
    [chaos-op]=$operator_invariants [chaos-pvc]=$operator_invariants
    [chaos-sent]=$sentinel_invariants [mixed-sent]=$sentinel_invariants
  )
  declare -A kinds=([chaos-op]="" [chaos-pvc]="" [chaos-sent]="" [mixed-sent]="sentinel_image_flip")
  chaos_kinds="operator_restart operator_upgrade node_drain"
  # Restarts and upgrades must lose no write anywhere, drains none on the
  # instance on volumes.
  pvc_instances="chaos-pvc"
  # An upgrade's three waits of the chaos timeout, plus the longest
  # interval and a data verification.
  convergence=900
  interval=120
  ;;
*)
  echo "E2E_PROFILE must be full, versions or chaos" >&2
  exit 1
  ;;
esac

# kind_up_workers creates the cluster as the skill's kind-up.sh does, with
# two workers to drain, and a directory every node mounts as /shared, for
# volumes that move with their pods.
kind_up_workers() {
  local dir=/tmp/kind-$cluster n host img
  mkdir -p "$dir/shared"
  chmod 0777 "$dir/shared"
  if ! docker info >/dev/null 2>&1; then
    (dockerd >/tmp/dockerd.log 2>&1 &)
    for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
  fi
  local node_config="  kubeadmConfigPatches:
  - |
    kind: KubeletConfiguration
    failCgroupV1: false
  extraMounts:
  - hostPath: $dir/shared
    containerPath: /shared"
  cat >"$dir/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  podSubnet: 10.$subnet.0.0/16
  serviceSubnet: 10.$((subnet + 1)).0.0/16
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri"]
    restrict_oom_score_adj = true
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
$node_config
- role: worker
$node_config
- role: worker
$node_config
EOF
  "$skill/registry.sh" pull "kindest/node:$node"
  kind create cluster --name "$cluster" --image "kindest/node:$node" \
    --config "$dir/kind.yaml" --kubeconfig "$dir/kubeconfig" --wait 180s
  # registry.sh makes the local registry the control plane's mirror only.
  "$skill/registry.sh" connect "$cluster"
  for n in $(kind get nodes --name "$cluster"); do
    for host in docker.io quay.io; do
      docker exec "$n" mkdir -p "/etc/containerd/certs.d/$host"
      printf '[host."http://kind-registry:5000"]\n  capabilities = ["pull", "resolve"]\n' |
        docker exec -i "$n" cp /dev/stdin "/etc/containerd/certs.d/$host/hosts.toml"
    done
  done
  for img in $(grep -oE '"[^"]+:[^"]+"' "$repo/api/redisfailover/v1/defaults.go" | tr -d '"' | sort -u); do
    "$skill/registry.sh" push "$img"
  done
  # A StorageClass whose volumes live in /shared: a pod moves to another
  # node with its volume, as on network storage. It is a clone of kind's
  # local-path provisioner in its shared file system mode.
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

# build_version builds the operator of a git ref as redis-operator:src-<sha>,
# serves it through the local registry, and packages its chart.
build_version() {
  local name=$1 ref=$2 src sha
  sha=$(git -C "$repo" rev-parse --short "$ref^{commit}")
  src=$(mktemp -d)
  git -C "$repo" archive "$sha" | tar -x -C "$src"
  mkdir -p "$src/out"
  (cd "$src" && CGO_ENABLED=0 go build -o "$src/out/redis-operator" -ldflags "-w" ./cmd/redisoperator)
  # As the skill's build-image.sh does: docker/app/Dockerfile can't install
  # packages in the sandbox.
  cat >"$src/out/Dockerfile" <<'EOF'
FROM alpine:latest
COPY redis-operator /usr/local/bin/redis-operator
RUN addgroup -g 1000 rf && adduser -D -u 1000 -G rf rf
USER rf
ENTRYPOINT ["/usr/local/bin/redis-operator"]
EOF
  "$skill/registry.sh" pull alpine:latest
  docker build -q -t "redis-operator:src-$sha" "$src/out" >/dev/null
  "$skill/registry.sh" push "redis-operator:src-$sha"
  helm package "$src/charts/redisoperator" --destination "$src/out" >/dev/null
  mv "$src"/out/redis-operator-*.tgz "$charts/chart-$name.tgz"
  version_name[$name]=$ref
  version_chart[$name]=chart-$name.tgz
  version_version[$name]=""
  version_image[$name]=redis-operator:src-$sha
  rm -rf "$src"
}

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  if [[ $profile == chaos ]]; then
    kind_up_workers
  else
    "$skill/kind-up.sh" "$cluster" "$node" "$subnet"
  fi
fi

echo "--- operator"
if [[ $profile == chaos ]]; then
  # Both versions of the upgrade; the first is installed, with the chart's
  # CRD hook. The operator, like the tester, runs on the control plane,
  # which no drain picks.
  declare -A version_name version_chart version_version version_image
  charts=$soak/bin/kind-e2e-charts
  rm -rf "$charts"
  mkdir -p "$charts"
  if [[ -z ${OPERATOR_VERSION:-} ]]; then
    build_version from "${UPGRADE_FROM:-4.2.0-rc2}"
    build_version to "${UPGRADE_TO:-origin/main}"
  else
    for v in "from ${UPGRADE_FROM:?UPGRADE_FROM is the release to upgrade from}" "to $OPERATOR_VERSION"; do
      read -r name tag <<<"$v"
      version_name[$name]=$tag
      version_chart[$name]=oci://ghcr.io/saremox/redis-operator/charts/redis-operator
      version_version[$name]=$tag
      version_image[$name]=ghcr.io/saremox/redis-operator:$tag
    done
  fi
  chart=${version_chart[from]}
  [[ $chart == oci://* ]] || chart=$charts/$chart
  cat >"$charts/values.yaml" <<'EOF'
nodeSelector:
  node-role.kubernetes.io/control-plane: ""
tolerations:
  - {key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}
crds:
  upgradeHook:
    enabled: true
EOF
  # The hook's kubectl, which the nodes pull through the registry.
  "$skill/registry.sh" push rancher/kubectl:v1.36.2
  helm upgrade --install redis-operator "$chart" ${version_version[from]:+--version "${version_version[from]}"} \
    --namespace redis-operator --create-namespace -f "$charts/values.yaml" \
    --set image.repository="${version_image[from]%:*}" --set image.tag="${version_image[from]##*:}" --wait
elif [[ -z ${OPERATOR_VERSION:-} ]]; then
  "$skill/build-image.sh" soak-e2e
  helm upgrade --install redis-operator "$repo/charts/redisoperator" \
    --namespace redis-operator --create-namespace \
    --set image.repository=redis-operator --set image.tag=soak-e2e --wait
else
  helm upgrade --install redis-operator oci://ghcr.io/saremox/redis-operator/charts/redis-operator \
    --version "$OPERATOR_VERSION" --namespace redis-operator --create-namespace --wait
fi

echo "--- instances"
# An earlier run's mutations leave the instances changed: start afresh,
# and without the other profile's, which the node has no room for.
kubectl -n redis-soak delete deployment soak --ignore-not-found --wait
kubectl delete namespace "${all_instances[@]}" --ignore-not-found --wait
if [[ $profile != full ]]; then
  # The tester creates the instances from their templates; the node pulls
  # every version's image from the local registry.
  for img in $(yq -r '.versions[].image' "$config" | sort -u); do
    "$skill/registry.sh" push "$img"
  done
fi
# The tester's RoleBindings need every namespace, the other profile's too.
for ns in "${all_instances[@]}"; do
  kubectl create namespace "$ns"
done
for rf in "${instances[@]}"; do
  [[ $profile != full ]] && break
  if [[ $rf == bootstrap ]]; then
    # It replicates from op-basic's master through its Service.
    until kubectl -n op-basic get service rfrm-op-basic >/dev/null 2>&1; do sleep 2; done
    host=$(kubectl -n op-basic get service rfrm-op-basic -o jsonpath='{.spec.clusterIP}')
    sed "s/BOOTSTRAP_HOST/$host/" "$soak/e2e/$rf.yaml" | kubectl apply -f -
    continue
  fi
  kubectl apply -f "$soak/e2e/$rf.yaml"
done
# wait_instances waits until every instance runs and is Healthy.
wait_instances() {
  local rf replicas
  for rf in "${instances[@]}"; do
    until kubectl -n "$rf" get statefulset "rfr-$rf" >/dev/null 2>&1; do sleep 2; done
    replicas=$(kubectl -n "$rf" get redisfailover "$rf" -o jsonpath='{.spec.redis.replicas}')
    kubectl -n "$rf" wait --for=jsonpath='{.status.readyReplicas}'="$replicas" "statefulset/rfr-$rf" --timeout=300s
  done
  for rf in "${instances[@]}"; do
    [[ ${paths[$rf]} == *sentinel* ]] || continue
    until kubectl -n "$rf" get deployment "rfs-$rf" >/dev/null 2>&1; do sleep 2; done
    kubectl -n "$rf" wait --for=jsonpath='{.status.readyReplicas}'=3 "deployment/rfs-$rf" --timeout=300s
  done
  for rf in "${instances[@]}"; do
    kubectl -n "$rf" wait --for=jsonpath='{.status.state}'=Healthy "redisfailover/$rf" --timeout=300s
  done
}
[[ $profile == full ]] && wait_instances

if [[ $profile == chaos ]]; then
  echo "--- Prometheus and Grafana"
  # Prometheus scrapes the tester from its start, and loads the alerts.
  kubectl apply -f "$soak/e2e/monitoring.yaml"
  yq '.spec' "$soak/deploy/monitoring/prometheusrule.yaml" >"$charts/rules.yaml"
  cat >"$charts/prometheus.yml" <<'EOF'
global:
  scrape_interval: 5s
  evaluation_interval: 5s
rule_files: [/etc/prometheus/rules.yaml]
scrape_configs:
  - job_name: redis-soak
    honor_labels: true
    static_configs:
      - targets: [soak.redis-soak.svc:9090]
EOF
  kubectl -n soak-monitoring create configmap prometheus --from-file="$charts/prometheus.yml" --from-file="$charts/rules.yaml"
  for img in prom/prometheus:v3.7.3 grafana/grafana:12.2.1; do
    "$skill/registry.sh" push "$img"
  done
fi

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
rm -rf "$overlay"
mkdir -p "$overlay"
sed "s/^  stopAfter: .*/  stopAfter: ${duration}s/" "$config" >"$overlay/config.yaml"
templates=()
for t in "$(dirname "$config")"/rf-*.yaml; do
  [[ $profile == full ]] && break
  cp "$t" "$overlay/"
  templates+=("$(basename "$t")")
done
extra_resources=""
patches=""
if [[ $profile == chaos ]]; then
  # The upgrade's versions, with the charts built here next to the config.
  for v in from to; do
    yq -y -i "(.chaos.upgrade.versions[] | select(.name == \"$v\")) |= (.name = \"${version_name[$v]}\"
      | .chart = \"${version_chart[$v]}\" | .version = \"${version_version[$v]}\" | .image = \"${version_image[$v]}\")" "$overlay/config.yaml"
    if [[ ${version_chart[$v]} != oci://* ]]; then
      cp "$charts/${version_chart[$v]}" "$overlay/"
      templates+=("${version_chart[$v]}")
    fi
  done
  # The mutator's rights in the chaos instances' namespaces, and the
  # tester on the control plane, which no drain picks.
  for ns in chaos-op chaos-pvc chaos-sent; do
    yq -y -s ".[0] | .metadata.namespace = \"$ns\"" "$soak/deploy/rbac-instances.yaml"
    echo ---
  done >"$overlay/rbac-chaos-instances.yaml"
  extra_resources="  - rbac-chaos-instances.yaml"
  patches="patches:
  - target: {kind: Deployment, name: soak}
    patch: |-
      - {op: add, path: /spec/template/spec/nodeSelector, value: {node-role.kubernetes.io/control-plane: \"\"}}
      - {op: add, path: /spec/template/spec/tolerations, value: [{key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}]}"
fi
cat >"$overlay/kustomization.yaml" <<YAML
resources:
  - ../../deploy
$extra_resources
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
$(for t in "${templates[@]}"; do echo "      - $t"; done)
$patches
YAML
# Server-side: with the chaos profile's charts, the ConfigMap is too large
# for a client-side apply's last-applied annotation.
kubectl apply --server-side --force-conflicts -k "$overlay"
kubectl -n redis-soak rollout status deployment/soak --timeout=180s
started=$(date +%s)
[[ $profile != full ]] && wait_instances

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
# Every mutator, and the chaos lane, logs when it stopped.
stops=$(yq '[.instances[] | select(.mutations.kinds)] | length' "$config")
yq -e '.chaos.kinds' "$config" >/dev/null 2>&1 && stops=$((stops + 1))
until (($(logs | jq -s '[.[] | select(.msg == "mutator stopped" or .msg == "chaos stopped")] | length') == stops)); do
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
if [[ $profile == chaos ]]; then
  for ns in redis-operator soak-monitoring kube-system; do
    kubectl -n "$ns" get events --sort-by=.lastTimestamp >"$artifacts/events-$ns.txt"
  done
fi
kubectl -n redis-operator logs deployment/redis-operator --since="$(($(date +%s) - started + 30))s" >"$artifacts/operator.log"
for rf in "${instances[@]}"; do
  kubectl -n "$rf" get events --sort-by=.lastTimestamp >"$artifacts/events-$rf.txt"
done
echo "$metrics" >"$artifacts/metrics.txt"

# toggle's paths and invariants follow the mode it ended in.
if [[ $profile != full ]]; then
  :
elif [[ $(kubectl -n toggle get redisfailover toggle -o jsonpath='{.spec.sentinel.enabled}') == true ]]; then
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

echo "--- auth windows (outages that started during each auth change: result it started with, seconds)"
# The follower switches to the new password at once: its auth outage is how
# long the operator took to apply the change. The other styles keep their
# open connections, or take the Secret's password for new ones. Outages
# that start closed or timeout are the pods rolling onto the Secret.
jq -rs '
  def ts: capture("^(?<s>[^.Z]+)(?<f>\\.[0-9]+)?") | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | tonumber);
  (map(select(.msg == "outage started") | . + {t: (.time | ts)})) as $os
  | (map(select(.msg == "outage ended") | . + {s: ((.time | ts) - .duration_seconds)})) as $oe
  | (map(select(.msg == "mutating"))) as $starts
  | .[] | select(.msg == "mutation done" and (.kind | test("^(password_rotate|auth_add|auth_remove|password_rotate_offline)$"))) | . as $d
  | ([$starts[] | select(.rf == $d.rf and .step == $d.step)] | first) as $m
  | ($m.time | ts) as $from | ($d.time | ts) as $to
  | [$oe[] | select(.rf == $d.rf and .s >= $from and .s <= $to) | . as $e
     | . + {result: ([$os[] | select(.rf == $e.rf and .path == $e.path and .client == $e.client and .t >= $e.s - 0.1 and .t <= $e.s + 3)] | first | .result)}]
  | group_by([.path, .client])[]
  | [$d.rf, $d.step, $d.kind, $d.redis_replicas, .[0].path, .[0].client,
     (map("\(.result):\(.duration_seconds * 10 | round / 10)") | join(" "))]
  | @tsv' "$artifacts/soak.jsonl" | table rf step kind replicas path client outages

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

echo "--- bootstrap during op-basic's master changes (seconds)"
# Its link breaks while op-basic fails over; rfrm-op-basic's ClusterIP then
# leads it to the new master.
jq -rs '
  def ts: capture("^(?<s>[^.Z]+)(?<f>\\.[0-9]+)?") | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | tonumber);
  (map(select(.rf == "bootstrap" and .msg == "invariant restored" and .invariant == "one_master") | . + {s: ((.time | ts) - .duration_seconds)})) as $down
  | (map(select(.rf == "bootstrap" and .msg == "outage ended") | . + {s: ((.time | ts) - .duration_seconds)})) as $reads
  | (map(select(.rf == "op-basic" and .msg == "mutating"))) as $starts
  | .[] | select(.rf == "op-basic" and .msg == "mutation done" and (.kind | test("^(kill_master|kill_master_force|redis_replicas)$"))) | . as $d
  | ([$starts[] | select(.step == $d.step)] | first.time | ts) as $from | (($d.time | ts) + 30) as $to
  | [$down[] | select(.s >= $from and .s <= $to)] as $dn
  | select(($dn | length) > 0 or ($d.kind | test("^kill_")))
  | [$d.step, $d.kind, $d.redis_replicas, $d.params,
     ($dn | map(.duration_seconds | . * 10 | round / 10 | tostring) | join(" ") | if . == "" then "-" else . end),
     ([$reads[] | select(.s >= $from and .s <= $to)] | length)]
  | @tsv' "$artifacts/soak.jsonl" | table step kind replicas params link_down rfrs_outages

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

if [[ $profile == versions ]]; then
  echo "--- version transitions (seconds; outages and their longest per path, of any client style)"
  # Each version change with its mixed window (until the reset deleted the
  # instance for one that didn't converge), its losses and the outages that
  # started while it ran.
  jq -rs '
    def ts: capture("^(?<s>[^.Z]+)(?<f>\\.[0-9]+)?") | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | tonumber);
    (map(select(.msg == "mutating"))) as $starts
    | (map(select(.msg == "mutation done"))) as $dones
    | (map(select(.msg == "mixed versions"))) as $mixed
    | (map(select(.msg == "outage ended") | . + {s: ((.time | ts) - .duration_seconds)})) as $oe
    | .[] | select(.msg == "version transition") | . as $v
    | ([$starts[] | select(.rf == $v.rf and .step == $v.step)] | first) as $m
    | ([$dones[] | select(.rf == $v.rf and .step == $v.step)] | first) as $d
    | ($m.time | ts) as $from | ($v.time | ts) as $to
    | ([$mixed[] | select(.rf == $v.rf and .from == $v.from and .to == $v.to and (.time | ts) >= $from)] | first) as $mx
    | [$oe[] | select(.rf == $v.rf and .s >= $from and .s <= $to)] as $outs
    | [$v.rf, $v.step, $v.kind, "\($v.from) -> \($v.to)", $v.expect, $v.result, ($d.duration_seconds | round),
       (if $mx == null then "-" else ($mx.duration_seconds | round) end), $v.lost, ($outs | length),
       ($outs | group_by(.path) | map("\(.[0].path)=\(map(.duration_seconds) | max | . * 10 | round / 10)") | join(" ") | if . == "" then "-" else . end)]
    | @tsv' "$artifacts/soak.jsonl" | table rf step kind edge expect result seconds mixed lost outages longest >"$artifacts/transitions.txt"
  cat "$artifacts/transitions.txt"
  grep -E '^redis_soak_(version_transition_total|version_mixed_seconds_(sum|count))' <<<"$metrics" | sed 's/^redis_soak_//'

  echo "--- version changes that didn't converge: what they left behind"
  jq -c 'select(.msg == "version transition" and .result != "ok")
    | {rf, step, edge: "\(.from) -> \(.to)", expect, result, lost, master, master_version, master_writable, reasons, pods}' \
    "$artifacts/soak.jsonl"

  echo "--- resets (seconds)"
  jq -r 'select(.msg == "mutation done" and .kind == "reset")
    | [.rf, .step, .result, (.duration_seconds | round), .params] | @tsv' "$artifacts/soak.jsonl" |
    table rf step result seconds params
  jq -r 'select(.msg == "reset phase done") | [.rf, .time, .phase, (.duration_seconds | round)] | @tsv' "$artifacts/soak.jsonl" |
    table rf time phase seconds

  echo "--- Valkey compatibility"
  # The operator execs redis-server and redis-cli by name in the pod
  # commands, probes and scripts; the Valkey images ship them as links.
  for rf in "${instances[@]}"; do
    kubectl -n "$rf" get pods -l app.kubernetes.io/part-of=redis-failover \
      -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.labels.app\.kubernetes\.io/component} {.spec.containers[0].image}{"\n"}{end}' |
      while read -r pod component image; do
        [[ $image == *valkey* ]] || continue
        container=redis
        [[ $component == sentinel ]] && container=sentinel
        printf '%s/%s %s: ' "$rf" "$pod" "$image"
        kubectl -n "$rf" exec "$pod" -c "$container" -- sh -c \
          'printf "redis-server -> %s, redis-cli -> %s, pid 1: %s\n" "$(readlink -f "$(command -v redis-server)")" "$(readlink -f "$(command -v redis-cli)")" "$(tr "\0" " " </proc/1/cmdline)"' 2>&1 |
          sed -E 's/--requirepass [^ ]+|--masterauth [^ ]+/<auth>/g'
      done
  done
  grep -E '^redis_soak_server_info' <<<"$metrics" | sed 's/^redis_soak_//'
  echo "probe results other than ok on Valkey instances:"
  grep -E '^redis_soak_probe_total\{[^}]*rf="(valkey-op|valkey-sent|mixed-sent)"' <<<"$metrics" | grep -v 'result="ok"' | sed 's/^redis_soak_//' || echo "  none"
  echo "probe and container events on Valkey instances:"
  for rf in valkey-op valkey-sent mixed-sent; do
    grep -E 'Unhealthy|BackOff|Failed' "$artifacts/events-$rf.txt" | sed "s/^/  $rf: /" | tail -10 || true
  done
  grep -iE 'level=error.*(valkey-op|valkey-sent|mixed-sent)|(valkey-op|valkey-sent|mixed-sent).*level=error' "$artifacts/operator.log" | tail -10 || true

  echo "--- known operator issues seen"
  # In operator mode, a graceful master rollover can promote the restarted
  # old master (the same pod name, a new UID) instead of a Ready replica:
  # GetBestReplicaForPromotion falls back to the highest offset without
  # checking readiness.
  jq -rs '
    def ts: capture("^(?<s>[^.Z]+)(?<f>\\.[0-9]+)?") | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | tonumber);
    (map(select(.msg == "mutating"))) as $starts
    | .[] | select(.msg == "failover" and .from == .to and .event != "reset") | . as $f | ($f.time | ts) as $t
    | ([$starts[] | select(.rf == $f.rf and (.time | ts) <= $t)] | last) as $m
    | "old master promoted: \($f.rf) \($f.from) at \($f.time), during step \($m.step // "-") \($m.kind // "-") \($m.params // "")"' \
    "$artifacts/soak.jsonl"
  grep -E 'Selected replica .* for promotion' "$artifacts/operator.log" | tail -10 || true
  # A scale-down that removes the master can promote a replica too early:
  # checker.go's case 1 of checkAndHealOperatorManagedMode skips
  # masterPodStopping.
  jq -rs '
    (map(select(.msg == "mutation done")) | map({key: "\(.rf)/\(.step)", value: .}) | from_entries) as $m
    | .[] | select(.msg == "data verified" and .failover and .event == "redis_replicas") | . as $v | ($m["\(.rf)/\(.step)"] // {}) as $d
    | "scale-down removed the master: \(.rf) step \(.step) \($d.params // "") lost \(.lost)"' "$artifacts/soak.jsonl"
fi

if [[ $profile == chaos ]]; then
  echo "--- chaos actions (seconds)"
  jq -r 'select(.lane == "chaos" and (.msg == "chaos done" or .msg == "chaos skipped"))
    | [.step, .kind, .result, (.duration_seconds | round), (.error // .reason // "" | gsub("\n"; "; "))] | @tsv' \
    "$artifacts/soak.jsonl" | table step kind result seconds error
  jq -r 'select(.lane == "chaos" and .msg == "chaos") | [.time, .step, .kind, (.in_flight // [] | join(",") | if . == "" then "-" else . end)] | @tsv' \
    "$artifacts/soak.jsonl" | table started step kind in_flight

  echo "--- operator restarts (seconds)"
  # Without an operator: from the old leader's deletion until the new one
  # acquired the Lease.
  jq -r 'select(.msg == "operator restarted")
    | [.step, .from, .to, (.operator_down_seconds * 10 | round / 10), (.converge_seconds | round)] | @tsv' \
    "$artifacts/soak.jsonl" | table step old_leader new_leader without_operator converged

  echo "--- operator upgrades (seconds)"
  jq -r 'select(.msg == "operator upgraded" or .msg == "helm upgrade failed" or .msg == "operator not upgraded")
    | [.step, "\(.from) -> \(.to)", .msg, (.helm_seconds | round), .hook, (.hook_seconds * 10 | round / 10),
       (.operator_down_seconds // 0 | . * 10 | round / 10), (.converge_seconds // 0 | round)] | @tsv' \
    "$artifacts/soak.jsonl" | table step upgrade result helm hook hook_s without_operator converged
  echo "the CRD before and after each upgrade's hook:"
  jq -r 'select(.msg == "operator upgraded" or .msg == "helm upgrade failed") | "  \(.from) -> \(.to): \(.crd_before) -> \(.crd_after)"' \
    "$artifacts/soak.jsonl"
  echo "the hook's Job and pod events:"
  grep -E 'crds-upgrade' "$artifacts/events-redis-operator.txt" | sed 's/^/  /' || true
  jq -r 'select(.msg == "helm upgrade failed") | .output' "$artifacts/soak.jsonl"

  echo "--- node drains (seconds)"
  jq -r 'select(.msg == "node drained" or .msg == "node uncordoned")
    | [.step, .node, .msg, (.evict_seconds // 0 | round), (.converge_seconds | round), (.pdb_blocked // "-")] | @tsv' \
    "$artifacts/soak.jsonl" | table step node phase evicted converged pdb_blocked
  echo "evictions a PodDisruptionBudget blocked:"
  jq -r 'select(.msg == "eviction blocked" or .msg == "eviction unblocked" or .msg == "eviction still blocked at the drain timeout")
    | [.step, .node, "\(.pod_namespace)/\(.pod)", .msg, (.blocked_seconds // 0 | . * 10 | round / 10)] | @tsv' \
    "$artifacts/soak.jsonl" | table step node pod event blocked
  grep -E '^redis_soak_chaos_' <<<"$metrics" | sed 's/^redis_soak_//'

  echo "--- outages per chaos action (seconds; per path and client style)"
  jq -rs '[.[] | select(.msg == "outage ended" and (.event | test("^(operator_restart|operator_upgrade|node_drain)$")))]
    | group_by([.rf, .event, .path, .client])[]
    | [.[0].rf, .[0].event, .[0].path, .[0].client, length, (map(.duration_seconds) | add * 10 | round / 10),
       (map(.duration_seconds) | max * 10 | round / 10)] | @tsv' "$artifacts/soak.jsonl" |
    table rf event path client outages total max
  jq -rs '[.[] | select(.msg == "outage started" and (.event | test("^(operator_restart|operator_upgrade|node_drain)$")))]
    | group_by([.rf, .event, .result])[] | [.[0].rf, .[0].event, .[0].result, length] | @tsv' "$artifacts/soak.jsonl" |
    table rf event first_result outages

  echo "--- data verified after chaos actions"
  jq -r 'select(.msg == "data verified" and (.event | test("^(operator_restart|operator_upgrade|node_drain|reset)$")))
    | [.rf, .event, .step, .failover, .recent, .older, .fill, .lost] | @tsv' "$artifacts/soak.jsonl" |
    table rf event step failover recent older fill lost
  jq -c 'select(.msg == "lost writes") | del(.level, .namespace, .mode)' "$artifacts/soak.jsonl"

  echo "--- Sentinel image flips (seconds)"
  jq -r 'select(.msg == "mutation done" and .kind == "sentinel_image_flip") | [.rf, .step, .result, (.duration_seconds | round), .params] | @tsv' \
    "$artifacts/soak.jsonl" | table rf step result seconds params
  jq -r 'select(.msg == "mixed versions") | [.rf, .from, .to, .sentinel, (.duration_seconds | round)] | @tsv' "$artifacts/soak.jsonl" |
    table rf from to sentinel mixed_seconds

  echo "--- replica_ready_without_data"
  jq -c 'select(.invariant == "replica_ready_without_data" and (.msg == "invariant violated" or .msg == "invariant restored"))
    | {time, rf, msg, finding, reason, duration_seconds}' "$artifacts/soak.jsonl"

  echo "--- dashboard and alerts"
  # Through port-forwards: Prometheus has scraped the tester since its start.
  kubectl -n soak-monitoring port-forward svc/prometheus 19090:9090 >/dev/null 2>&1 &
  pf_prometheus=$!
  kubectl -n soak-monitoring port-forward svc/grafana 13000:3000 >/dev/null 2>&1 &
  pf_grafana=$!
  sleep 5
  monitoring=0
  python3 "$soak/e2e/check-monitoring.py" http://127.0.0.1:19090 http://127.0.0.1:13000 $(($(date +%s) - started)) "${instances[@]}" \
    >"$artifacts/monitoring.txt" 2>&1 || monitoring=$?
  kill "$pf_prometheus" "$pf_grafana"
  # Without the panels that returned data for an instance.
  grep -vE '\[[a-z-]+\]: [0-9]+ series$' "$artifacts/monitoring.txt" || true
fi

# verified_after_mutations prints the mutations the data wasn't verified
# after, as their kind or as a reset.
verified_after_mutations() {
  jq -rs '(map(select(.msg == "data verified")) | map("\(.rf)/\(.step)/\(.event)")) as $v
    | .[] | select(.msg == "mutation done") | . as $d
    | select($v | (index("\($d.rf)/\($d.step)/\($d.kind)") or index("\($d.rf)/\($d.step)/reset")) | not)
    | "\(.rf)/\(.step)/\(.kind)"' \
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
    (map(select(.msg == "data verified" and (.event | test("^(failover|reset|operator_restart|operator_upgrade|node_drain)$"))))) as $fv
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
if [[ $profile == full ]]; then
  check "oom_rejections_total{rf=op-noevict} = 0" gt "$(value redis_soak_oom_rejections_total 'rf="op-noevict"')" 0
  check "the bootstrap was never verified" gt "$(series redis_soak_ledger_verified_total 'rf="bootstrap"' | awk '{ s += $1 } END { print s + 0 }')" 0
  n=$(series redis_soak_lost_writes_total 'rf="bootstrap"' | awk '{ s += $1 } END { print s + 0 }')
  check "op-basic's writes missing on the bootstrap: $n" eq "$n" 0
  lossy=$(lossy_events | xargs)
  check "writes lost by lossless events: $lossy" eq "$lossy" ""
  race=$(race_losses | xargs)
  [[ -z $race ]] || echo "KNOWN RACE (excluded): a scale-down removed the master and lost writes: $race"
  check "evicted_keys_total{rf=op-maxmem} = 0" gt "$(value redis_soak_evicted_keys_total 'rf="op-maxmem"')" 0
else
  # Every edge was taken; every ok edge ended ok; none failed unsafely.
  while read -r from to expect; do
    l=("from=\"$from\"" "to=\"$to\"")
    n=$(series redis_soak_version_transition_total "${l[@]}" | awk '{ s += $1 } END { print s + 0 }')
    check "edge $from -> $to was never taken" gt "$n" 0
    [[ $expect == ok ]] || continue
    n=$(series redis_soak_version_transition_total "${l[@]}" | wc -l)
    m=$(series redis_soak_version_transition_total "${l[@]}" 'result="ok"' | wc -l)
    check "ok edge $from -> $to ended other than ok" eq "$n" "$m"
  done < <(yq -r '.edges[] | [.from, .to, .expect] | join(" ")' "$config")
  n=$(series redis_soak_version_transition_total 'result="failed_unsafe"' | awk '{ s += $1 } END { print s + 0 }')
  check "version changes failed unsafely: $n" eq "$n" 0
  # Rollovers along ok edges on volumes are graceful and must lose nothing.
  lossy=$(jq -r --arg pvc "$pvc_instances" 'select(.msg == "version transition" and .expect == "ok" and .lost > 0) | . as $v
    | select($pvc | split(" ") | index($v.rf)) | "\(.rf)/\(.step)/\(.from)->\(.to)=\(.lost)"' "$artifacts/soak.jsonl" | xargs)
  check "writes lost along ok edges on volumes: $lossy" eq "$lossy" ""
fi
if [[ $profile == chaos ]]; then
  for kind in $chaos_kinds; do
    l=("kind=\"$kind\"")
    check "chaos_total{kind=$kind,result=converged} = 0" ge "$(value redis_soak_chaos_total "${l[@]}" 'result="converged"')" 1
    for result in timeout failed; do
      n=$(value redis_soak_chaos_total "${l[@]}" "result=\"$result\"")
      check "chaos_total{kind=$kind,result=$result} = $n" eq "$n" 0
    done
  done
  inflight=$(jq -r 'select(.msg == "chaos" and (.in_flight // [] | length) > 0) | "\(.step)/\(.kind):\(.in_flight | join(","))"' \
    "$artifacts/soak.jsonl" | xargs)
  check "changes in flight when a chaos action started: $inflight" eq "$inflight" ""
  # Restarts and upgrades never touch a redis pod; a drain's evictions are
  # graceful, and the volumes move with the pods.
  lossy=$(jq -r --arg pvc "$pvc_instances" 'select(.msg == "data verified" and .lost > 0) | . as $v
    | select(.event == "operator_restart" or .event == "operator_upgrade" or (.event == "node_drain" and ($pvc | split(" ") | index($v.rf))))
    | "\(.rf)/\(.step)/\(.event)=\(.lost)"' "$artifacts/soak.jsonl" | xargs)
  check "writes lost by chaos actions that must lose none: $lossy" eq "$lossy" ""
  n=$(jq -s '[.[] | select(.msg == "data verified" and (.event | test("^(operator_restart|operator_upgrade|node_drain)$")))] | length' "$artifacts/soak.jsonl")
  check "the data was never verified after a chaos action" gt "$n" 0
  check "the dashboard and alerts" eq "$monitoring" 0
fi
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
if [[ $profile == chaos ]]; then
  echo "PASS: every chaos action converged with no change in flight, without findings, every probe succeeds, no restart, upgrade or drain on volumes lost writes, and the dashboard and alerts check out"
  exit 0
fi
if [[ $profile == versions ]]; then
  echo "PASS: every edge was taken, every ok edge ended ok, none failed unsafely, every chain reset and every kind converged, without findings, every probe succeeds, and no rollover along an ok edge on volumes lost writes"
  exit 0
fi
echo "PASS: every kind converged on every instance, without findings, every probe succeeds, the data was verified after every mutation and failover, and no lossless event lost writes"
