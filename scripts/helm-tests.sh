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

# A patch on an older release line must not move latest or the major tag.
floating_tags() {
    printf '4.1.2\n4.2.0-rc2\n4.2.0\n' | ./scripts/release.sh floating "$1" | tr '\n' ' '
}
for want in "4.1.3:latest=false major=false minor=true " "4.2.1:latest=true major=true minor=true " \
    "4.3.0-rc1:latest=false major=false minor=false "; do
    got=$(floating_tags "${want%%:*}")
    if [ "${got}" != "${want#*:}" ]; then
        echo "Floating tags of ${want%%:*}: '${got}', want '${want#*:}'." >&2
        exit 1
    fi
done

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

# Prints "sa <namespace>/<name>" for each ServiceAccount and "subject
# <namespace>/<name>" for each ServiceAccount subject in kustomize output.
service_accounts() {
    awk '
        function item_end() {
            if (ikind == "ServiceAccount") print "subject " ins "/" iname
            ikind = iname = ins = ""
        }
        function doc_end() {
            item_end()
            if (kind == "ServiceAccount") print "sa " ns "/" name
            kind = name = ns = ""; insub = inmeta = 0
        }
        /^---/ { doc_end(); next }
        insub && /^- / { item_end(); sub(/^- /, "  ") }
        insub && /^  [a-zA-Z]+: / { if ($1 == "kind:") ikind = $2; if ($1 == "name:") iname = $2; if ($1 == "namespace:") ins = $2; next }
        insub { item_end(); insub = 0 }
        /^[a-z]/ { inmeta = 0 }
        /^subjects:$/ { insub = 1; next }
        /^metadata:$/ { inmeta = 1; next }
        /^kind: / { kind = $2 }
        inmeta && /^  name: / { name = $2 }
        inmeta && /^  namespace: / { ns = $2 }
        END { doc_end() }'
}

# The API server rejects a ServiceAccount subject without a namespace.
fail=0
for dir in manifests/kustomize/overlays/*/; do
  echo ">> Testing kustomize ${dir}"
  refs=$(kubectl kustomize "${dir}" 2>/dev/null | service_accounts)
  grep -q '^subject ' <<<"${refs}" || { echo "FAIL: ${dir} binds no ServiceAccount" >&2; fail=1; }
  while read -r _ ref; do
    [ "${ref#/}" = "${ref}" ] || { echo "FAIL: ${dir}: subject ${ref#/} has no namespace" >&2; fail=1; }
    grep -qx "sa ${ref}" <<<"${refs}" || { echo "FAIL: ${dir}: no ServiceAccount ${ref}" >&2; fail=1; }
  done < <(grep '^subject ' <<<"${refs}")
done

[ ${fail} -eq 0 ]

echo "> Manifests OK"
