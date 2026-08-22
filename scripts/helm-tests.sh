#!/bin/bash

set -eu

chart=charts/redisoperator
# Helm's built-in default Capabilities.KubeVersion is older than this chart's
# kubeVersion requirement, so lint/template fail without an explicit version.
kube_version=$(grep '^kubeVersion:' ${chart}/Chart.yaml | sed -E 's/^kubeVersion:\s*"?>=?([0-9.]+).*/\1/')

echo ">> Testing chart ${chart} against kubeVersion ${kube_version}"

helm lint ${chart}
helm template ${chart} --kube-version ${kube_version}

echo "> Chart OK"
