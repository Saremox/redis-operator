#!/bin/bash

set -eu

chart=charts/redisoperator
# Helm's built-in default Capabilities.KubeVersion is older than this chart's
# kubeVersion requirement, so lint/template fail without an explicit version.
kube_version=$(grep '^kubeVersion:' ${chart}/Chart.yaml | sed -E 's/^kubeVersion:\s*"?>=?([0-9.]+).*/\1/')

echo ">> Testing chart ${chart} against kubeVersion ${kube_version}"

# The release workflow sets the operator image in a copy of the chart. The
# CRD upgrade hook must keep its own image, because it runs kubectl.
hook_image() {
    helm template "$1" --kube-version ${kube_version} --set crds.upgradeHook.enabled=true \
        | grep -A1 'name: crds-upgrade$' | sed -n 's/^ *image: //p'
}
release_chart=$(mktemp -d)
trap 'rm -rf "${release_chart}"' EXIT
cp -r ${chart} "${release_chart}/"
./scripts/release.sh chart-values "${release_chart}/redisoperator" ghcr.io/example/redis-operator 9.9.9
want=$(hook_image ${chart})
got=$(hook_image "${release_chart}/redisoperator")
if [ -z "${want}" ] || [ "${got}" != "${want}" ]; then
    echo "Release chart: upgrade hook image is '${got}', want '${want}'." >&2
    exit 1
fi
helm template "${release_chart}/redisoperator" --kube-version ${kube_version} \
    | grep -q 'image: "ghcr.io/example/redis-operator:9.9.9"' \
    || { echo "Release chart: operator image is not ghcr.io/example/redis-operator:9.9.9." >&2; exit 1; }

# Find a version bump that misses a file before a release tag exists.
./scripts/release.sh check-version "$(sed -n 's/^version: //p' ${chart}/Chart.yaml)"

helm lint ${chart}
helm template ${chart} --kube-version ${kube_version}

echo "> Chart OK"
