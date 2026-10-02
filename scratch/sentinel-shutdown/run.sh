#!/usr/bin/env bash
# Measures acknowledged-write loss when the master pod of a Sentinel-mode
# RedisFailover is deleted gracefully, with the default shutdown script and
# with the same script given the Sentinel address it expects, against a
# forced SENTINEL FAILOVER with the old master still up.
#
# Expects a cluster with the operator installed and the sentloss:ci image
# (writer, verifier, probe.sh, redis-cli) loaded.
set -uo pipefail

ns=sl
rf=sl
OUT=${OUT:-$PWD/out}
mkdir -p "$OUT"
SUM=$OUT/summary.txt
: >"$SUM"
: >"$OUT/ips.txt"

now() { date +%s%3N; }
fmt() { date -u -d @$(($1 / 1000)) +%T; }
log() { echo "$(date -u +%T.%3N) $*" | tee -a "$OUT/driver.log"; }
die() {
  log "FATAL: $*"
  log "last convergence check failed on: $(cat "$OUT/why" 2>/dev/null)"
  k get redisfailover $rf -o jsonpath='{.status}' 2>&1 | tee -a "$OUT/driver.log"; echo
  k get pods -o wide 2>&1 | tee -a "$OUT/driver.log"
  wx probe.sh "$(redis_json | jq -r '.items[].status.podIP // empty' | tr '\n' ' ')" "$(sentinel_ips)" 2>&1 | tee -a "$OUT/driver.log"
  exit 1
}
why() { echo "$*" >"$OUT/why"; return 1; }
k() { kubectl -n $ns "$@"; }
wx() { timeout 30 kubectl -n $ns exec writer -- "$@"; }

redis_json() { k get pods -l app.kubernetes.io/component=redis -o json; }
sentinel_ips() { k get pods -l app.kubernetes.io/component=sentinel -o jsonpath='{.items[*].status.podIP}'; }
record_ips() {
  redis_json | jq -r '.items[] | select(.status.podIP) | "\(.status.podIP) \(.metadata.name)"' >>"$OUT/ips.txt"
  sort -u -o "$OUT/ips.txt" "$OUT/ips.txt"
}
# annotate: adds the pod name to every known pod IP followed by ':'.
annotate() {
  local script
  script=$(awk '{ip=$1; gsub(/\./, "\\.", ip); printf "s/%s:/%s(%s):/g;", ip, $1, $2}' "$OUT/ips.txt")
  sed -E "$script"
}

# converged: 3 ready pods, RF Healthy, one master, replicas linked to it,
# all Sentinels agree, master pod labelled. Prints "pod ip".
converged() {
  local j rips sips out mip m
  j=$(redis_json) || { why "pods list"; return 1; }
  [[ $(jq '.items | length' <<<"$j") == 3 ]] || { why "pod count $(jq '.items | length' <<<"$j")"; return 1; }
  [[ $(jq '[.items[] | select(.metadata.deletionTimestamp == null) | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length' <<<"$j") == 3 ]] || { why "ready pods"; return 1; }
  [[ $(k get redisfailover $rf -o jsonpath='{.status.state}') == Healthy ]] || { why "RF state $(k get redisfailover $rf -o jsonpath='{.status}')"; return 1; }
  rips=$(jq -r '.items[].status.podIP' <<<"$j" | tr '\n' ' ')
  sips=$(sentinel_ips)
  out=$(wx probe.sh "$rips" "$sips" 2>/dev/null) || { why "probe failed"; return 1; }
  [[ $(awk '$1=="R" && $3=="master"' <<<"$out" | wc -l) == 1 ]] || { why "masters: $(tr '\n' ';' <<<"$out")"; return 1; }
  mip=$(awk '$1=="R" && $3=="master" {print $2}' <<<"$out")
  [[ $(awk -v m="$mip" '$1=="R" && $3=="slave" && $4==m && $5=="up"' <<<"$out" | wc -l) == 2 ]] || { why "replica links: $(tr '\n' ';' <<<"$out")"; return 1; }
  [[ $(awk -v m="$mip" '$1=="S" && $3==m' <<<"$out" | wc -l) == 3 ]] || { why "sentinels: $(tr '\n' ';' <<<"$out")"; return 1; }
  [[ $(jq -r --arg ip "$mip" '.items[] | select(.status.podIP == $ip) | .metadata.labels["redisfailovers-role"] // ""' <<<"$j") == master ]] || { why "master $mip not labelled"; return 1; }
  m=$(jq -r --arg ip "$mip" '.items[] | select(.status.podIP == $ip) | .metadata.name' <<<"$j")
  echo "$m $mip"
}
any_master() {
  local ip
  ip=$(wx probe.sh "$(redis_json | jq -r '.items[].status.podIP // empty' | tr '\n' ' ')" "" | awk '$3=="master" {print $2; exit}')
  echo "$(awk -v ip="$ip" '$1==ip {n=$2} END {print n}' "$OUT/ips.txt") $ip"
}
wait_converged() {
  local deadline=$(($(date +%s) + ${1:-300})) r
  while (($(date +%s) < deadline)); do
    if r=$(converged); then echo "$r"; return 0; fi
    sleep 2
  done
  return 1
}

monitor() {
  local j rips sips pods eps probe prev="" cur ts
  while :; do
    j=$(redis_json 2>/dev/null)
    rips=$(jq -r '.items[].status.podIP // empty' <<<"$j" | tr '\n' ' ')
    sips=$(sentinel_ips 2>/dev/null)
    pods=$(jq -r '.items[] | "\(.metadata.name | sub("^rfr-sl-"; ""))=\(.status.podIP // "-")/\(.metadata.labels["redisfailovers-role"] // "-")/\(if .metadata.deletionTimestamp then "T" else "" end)\(if any(.status.conditions[]?; .type == "Ready" and .status == "True") then "R" else "nR" end)"' <<<"$j" | tr '\n' ' ')
    eps=$(k get endpointslices -l kubernetes.io/service-name=rfrm-$rf -o json 2>/dev/null | jq -r '[.items[].endpoints[]? | "\(.addresses[0]):r=\(.conditions.ready),s=\(.conditions.serving),t=\(.conditions.terminating)"] | join(" ")')
    probe=$(wx probe.sh "$rips" "$sips" 2>/dev/null | awk '$1=="R"{printf "%s:%s>%s/%s ", $2, $3, $4, $5} $1=="S"{s=s" "$3} END{printf "| sent=[%s ]", s}')
    cur="pods: $pods| rfrm=[$eps] | $probe"
    ts=$(date -u +%T.%3N)
    echo "$ts $cur" >>"$OUT/monitor.log"
    if [[ $cur != "$prev" ]]; then
      echo "$ts $cur" >>"$OUT/transitions.log"
      prev=$cur
    fi
    sleep 0.5
  done
}

collect() {
  for p in $(k get pods -l app.kubernetes.io/component=sentinel -o name 2>/dev/null); do
    k logs "$p" -c sentinel >"$OUT/sentinel-${p#pod/}.log" 2>/dev/null
  done
  kubectl -n redis-operator logs "$(kubectl -n redis-operator get deploy -o name | head -1)" --tail=-1 >"$OUT/operator.log" 2>/dev/null
  k get events --sort-by=.lastTimestamp >"$OUT/events.txt" 2>/dev/null
  wx cat /tmp/w.log >"$OUT/writer.log" 2>/dev/null
}
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done
  collect
}
trap cleanup EXIT

errors() {
  wx cat /tmp/w.log | awk -v a="$1" -v b="$2" '$1>=a && $1<b && $4=="ERR" {n[$2]++; if (!($2 in f)) f[$2]=$1; l[$2]=$1}
    END {for (m in n) printf "  write errors %s: %d, %.1fs from first to last\n", m, n[m], (l[m]-f[m])/1000}'
}
mark=0
# check TAG: verifies every write acked since the last check on the current master.
check() {
  local tag=$1 r m mip before
  if ! r=$(wait_converged 300); then
    log "$tag: not converged, verifying on any master"
    r=$(any_master)
  fi
  read -r m mip <<<"$r"
  before=$(($(now) - 3000))
  record_ips
  log "$tag: verifying writes acked $(fmt $mark)..$(fmt $before) on master $m ($mip)"
  {
    echo "== $tag: writes acked $(fmt $mark)..$(fmt $before) UTC, verified on $m"
    wx sentloss verify -master "$mip:6379" -log /tmp/w.log -after "$mark" -before "$before" | annotate
    errors "$mark" "$before"
  } | tee -a "$SUM"
  mark=$before
}

# delete_master TAG: graceful delete of the master pod (default grace period).
delete_master() {
  local tag=$1 r m mip uid t0 tgone
  r=$(wait_converged 300) || die "$tag: not converged before the delete"
  read -r m mip <<<"$r"
  record_ips
  uid=$(k get pod "$m" -o jsonpath='{.metadata.uid}')
  k logs -f "$m" -c redis --timestamps >"$OUT/redis-$tag-$m.log" 2>&1 &
  pids+=($!)
  t0=$(now)
  log "$tag: kubectl delete pod $m ($mip)"
  k delete pod "$m" --wait=false >/dev/null
  while [[ $(k get pod "$m" -o jsonpath='{.metadata.uid}' 2>/dev/null) == "$uid" ]]; do sleep 0.5; done
  tgone=$(now)
  log "$tag: old pod $m gone $(((tgone - t0) / 1000))s after the delete"
  echo "$tag: deleted master $m ($mip) at $(fmt $t0) UTC, old pod gone after $(((tgone - t0) / 1000))s" >>"$SUM"
  r=$(wait_converged 300) || die "$tag: not converged after the delete"
  log "$tag: converged, master $r"
  sleep 5
  check "$tag"
}

### Setup
kubectl create namespace $ns >/dev/null
kubectl apply -f - <<EOF
apiVersion: databases.spotahome.com/v1
kind: RedisFailover
metadata:
  name: $rf
  namespace: $ns
spec:
  sentinel:
    enabled: true
    replicas: 3
  redis:
    replicas: 3
EOF
for _ in $(seq 1 60); do k get statefulset rfr-$rf >/dev/null 2>&1 && break; sleep 2; done
k wait --for=jsonpath='{.status.readyReplicas}'=3 statefulset/rfr-$rf --timeout=300s || die "redis not ready"
k wait --for=jsonpath='{.status.readyReplicas}'=3 deployment/rfs-$rf --timeout=300s || die "sentinel not ready"

k run writer --image=sentloss:ci --image-pull-policy=Never --restart=Never --command -- \
  sh -c "exec sentloss write -rfrm rfrm-$rf:6379 -sentinel rfs-$rf:26379 -rate 20 >/tmp/w.log 2>&1" >/dev/null
k wait --for=condition=Ready pod/writer --timeout=120s || die "writer not ready"

r=$(wait_converged 300) || die "RF not converged"
read -r m0 mip0 <<<"$r"
log "converged, master $m0 ($mip0)"
record_ips
monitor &
pids+=($!)

### What the default shutdown script does
replica=$(redis_json | jq -r --arg m "$m0" '.items[] | select(.metadata.name != $m) | .metadata.name' | head -1)
{
  echo "== Default shutdown script"
  echo "enableServiceLinks on $m0: $(k get pod "$m0" -o jsonpath='{.spec.enableServiceLinks}')"
  k exec "$replica" -c redis -- sh -c 'echo "on '"$replica"': RFS_SL_SERVICE_HOST=[${RFS_SL_SERVICE_HOST:-}] RFS_SL_SERVICE_PORT_SENTINEL=[${RFS_SL_SERVICE_PORT_SENTINEL:-}]"'
  echo "sh -x /redis-shutdown/shutdown.sh on replica $replica:"
  k exec "$replica" -c redis -- sh -c 's=$(date +%s); sh -x /redis-shutdown/shutdown.sh; echo "exit=$? after $(($(date +%s) - s))s"' 2>&1 | sed 's/^/  /'
} | tee -a "$SUM"

sleep 15
mark=$(($(now) - 30000))
check baseline

### Graceful master deletions, default shutdown script
for i in 1 2 3; do
  delete_master "DEL$i"
done

### Control: forced SENTINEL FAILOVER with the old master up
r=$(wait_converged 300) || die "CTRL: not converged"
read -r m mip <<<"$r"
record_ips
t0=$(now)
log "CTRL: SENTINEL FAILOVER with master $m ($mip) up -> $(wx redis-cli -h rfs-$rf -p 26379 sentinel failover mymaster)"
echo "CTRL: SENTINEL FAILOVER at $(fmt $t0) UTC, master $m ($mip) left running" >>"$SUM"
sleep 3
deadline=$(($(date +%s) + 300))
until r=$(converged) && [[ ${r#* } != "$mip" ]]; do
  (($(date +%s) < deadline)) || die "CTRL: not converged"
  sleep 2
done
log "CTRL: converged, master $r"
sleep 5
check CTRL

### Same script with the Sentinel address it expects
cm=$(k get statefulset rfr-$rf -o jsonpath='{.spec.template.spec.volumes[?(@.name=="redis-shutdown-config")].configMap.name}')
{
  echo "RFS_SL_SERVICE_HOST=rfs-$rf"
  echo "RFS_SL_SERVICE_PORT_SENTINEL=26379"
  k get configmap "$cm" -o jsonpath='{.data.shutdown\.sh}'
} >"$OUT/shutdown-env.sh"
k create configmap shutdown-env --from-file=shutdown.sh="$OUT/shutdown-env.sh" >/dev/null
log "ROLL: switching to configmap shutdown-env (default script from $cm plus the two variables)"
echo "ROLL: spec.redis.shutdownConfigMap set to shutdown-env; the operator replaces all pods, the master last (old master still runs the default script)" >>"$SUM"
k patch redisfailover $rf --type merge -p '{"spec":{"redis":{"shutdownConfigMap":"shutdown-env"}}}' >/dev/null
deadline=$(($(date +%s) + 360))
until [[ $(redis_json | jq '[.items[] | select(any(.spec.volumes[]; .name == "redis-shutdown-config" and .configMap.name == "shutdown-env"))] | length') == 3 ]] && converged >/dev/null; do
  (($(date +%s) < deadline)) || die "ROLL: rollout did not finish"
  sleep 3
done
log "ROLL: done"
sleep 5
check ROLL

for i in 1 2; do
  delete_master "ENV$i"
done

### Report
collect
{
  echo
  echo "== Lost acknowledged writes (acked / lost)"
  awk '/^== /{tag=$2; sub(":", "", tag)} /^mode=/{split($1, a, "="); split($2, b, "="); split($3, c, "="); v[tag, a[2]] = b[2] "/" c[2]; if (!(tag in seen)) {seen[tag]=1; order[++n]=tag}}
    END {printf "%-10s %-14s %-14s %-14s\n", "phase", "pooled", "fresh", "sentinel"; for (i=1; i<=n; i++) {t=order[i]; printf "%-10s %-14s %-14s %-14s\n", t, v[t,"pooled"], v[t,"fresh"], v[t,"sentinel"]}}' "$SUM"
  echo
  echo "== Pod IPs"
  cat "$OUT/ips.txt"
} | tee -a "$SUM"
