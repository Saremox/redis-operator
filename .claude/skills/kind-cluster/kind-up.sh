#!/usr/bin/env bash
# Creates a kind cluster that works inside the Claude Code cloud sandbox and
# prepares it for this repo's integration tests.
#
# Usage: kind-up.sh NAME [NODE_VERSION] [SUBNET]
#   NAME          cluster name; state goes to /tmp/kind-NAME
#   NODE_VERSION  kindest/node tag (default v1.35.0)
#   SUBNET        second octet of the pod subnet 10.SUBNET.0.0/16 (default 244).
#                 Use a different one per cluster when running several.
set -euo pipefail

name=${1:?usage: kind-up.sh NAME [NODE_VERSION] [SUBNET]}
version=${2:-v1.35.0}
subnet=${3:-244}
dir=/tmp/kind-$name
repo=$(git rev-parse --show-toplevel)
here=$(cd "$(dirname "$0")" && pwd)
pod_cidr=10.$subnet.0.0/16
node=$name-control-plane
mkdir -p "$dir"

if ! docker info >/dev/null 2>&1; then
  (dockerd >/tmp/dockerd.log 2>&1 &)
  for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
fi

# The sandbox runs cgroup v1, which kubelet >= 1.35 refuses by default, and
# its kernel rejects negative oom_score_adj values, which containerd sets
# on every pod sandbox unless restricted.
cat >"$dir/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  podSubnet: $pod_cidr
  serviceSubnet: 10.$((subnet + 1)).0.0/16
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri"]
    restrict_oom_score_adj = true
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: KubeletConfiguration
    failCgroupV1: false
EOF

"$here/registry.sh" pull "kindest/node:$version"
kind create cluster --name "$name" --image "kindest/node:$version" \
  --config "$dir/kind.yaml" --kubeconfig "$dir/kubeconfig" --wait 180s
export KUBECONFIG=$dir/kubeconfig

# The node can't reach any registry: its inherited HTTPS_PROXY is the
# sandbox's 127.0.0.1 proxy. Serve images from a local registry that
# mirrors docker.io and quay.io, so pods with pullPolicy Always work too.
"$here/registry.sh" connect "$name"
images=$(grep -oE '"[^"]+:[^"]+"' "$repo/api/redisfailover/v1/defaults.go" | tr -d '"' | sort -u)
for img in $images ${EXTRA_IMAGES:-}; do
  "$here/registry.sh" push "$img"
done

# The integration tests talk to Redis pod IPs directly. The sandbox has no
# `ip` binary, so borrow the node image's.
node_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$node")
docker run --rm --net=host --privileged --entrypoint ip "kindest/node:$version" \
  route replace "$pod_cidr" via "$node_ip"

kubectl apply --server-side -f "$repo/manifests/databases.spotahome.com_redisfailovers.yaml"

echo
echo "export KUBECONFIG=$dir/kubeconfig"
