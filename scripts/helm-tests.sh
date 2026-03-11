#!/bin/bash

set -eu

chart=charts/redisoperator

echo ">> Testing chart ${chart}"

helm lint ${chart}
helm template test ${chart} >/dev/null
helm template test ${chart} --set serviceAccount.name=custom-sa >/dev/null
helm template test ${chart} --set serviceAccount.create=false --set serviceAccount.name=existing-sa >/dev/null
helm template test ${chart} --set imageCredentials.create=true --set imageCredentials.registry=ghcr.io --set imageCredentials.username=tester --set imageCredentials.password=secret --set imageCredentials.email=tester@example.com >/dev/null
helm template test ${chart} --set imageCredentials.existsSecrets[0]=foo --set imageCredentials.existsSecrets[1]=bar >/dev/null
helm template test ${chart} --set operator.logLevel=debug --set operator.supportedNamespacesRegex=prod-.* --set operator.extraArgs[0]=--concurrency=5 >/dev/null

echo "> Chart OK"
