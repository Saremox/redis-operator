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
check_tree=$(mktemp -d)
trap 'rm -rf "${release_chart}" "${check_tree}"' EXIT
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
version=$(sed -n 's/^version: //p' ${chart}/Chart.yaml)
./scripts/release.sh check-version "${version}"

# A version field that the check cannot find must fail the check. If not, a
# release with a renamed image repository pins no operator version.
tar -cf - scripts/release.sh example/operator manifests/kustomize ${chart}/Chart.yaml \
    | tar -xf - -C "${check_tree}"
"${check_tree}/scripts/release.sh" check-version "${version}" >/dev/null
sed -i 's|ghcr.io/saremox/redis-operator:|ghcr.io/example/redis-operator:|' \
    "${check_tree}/example/operator/operator.yaml"
if "${check_tree}/scripts/release.sh" check-version "${version}" >/dev/null 2>&1; then
    echo "check-version passed with a renamed image in example/operator/operator.yaml." >&2
    exit 1
fi

helm lint ${chart}
helm template ${chart} --kube-version ${kube_version}

# The Deployment, the ServiceAccount and the binding subject must use the same
# name. Otherwise the pod refers to a ServiceAccount that does not exist.
echo ">> Testing serviceAccount.name=custom with serviceAccount.create=true"
out=$(helm template ${chart} --kube-version ${kube_version} \
  --set serviceAccount.create=true --set serviceAccount.name=custom)
fail=0
grep -A2 '^kind: ServiceAccount$' <<<"${out}" | grep -qx '  name: custom' \
  || { echo "FAIL: ServiceAccount is not named custom" >&2; fail=1; }
grep -A1 -- '- kind: ServiceAccount$' <<<"${out}" | grep -qx '    name: custom' \
  || { echo "FAIL: ClusterRoleBinding subject is not custom" >&2; fail=1; }
grep -qx '      serviceAccountName: custom' <<<"${out}" \
  || { echo "FAIL: Deployment serviceAccountName is not custom" >&2; fail=1; }
[ ${fail} -eq 0 ]

echo "> Chart OK"
