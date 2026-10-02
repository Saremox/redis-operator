#!/bin/bash

set -eu

chart=charts/redisoperator
# Helm's built-in default Capabilities.KubeVersion is older than this chart's
# kubeVersion requirement, so lint/template fail without an explicit version.
kube_version=$(grep '^kubeVersion:' ${chart}/Chart.yaml | sed -E 's/^kubeVersion:\s*"?>=?([0-9.]+).*/\1/')

echo ">> Testing chart ${chart} against kubeVersion ${kube_version}"

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
