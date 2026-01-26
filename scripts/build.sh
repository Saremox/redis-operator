#!/usr/bin/env bash

set -o errexit
set -o nounset

if [[ ! -z ${TARGETOS+x} ]] && [[ ! -z ${TARGETARCH+x} ]];
then
    echo "Building ${TARGETOS}/${TARGETARCH} release..."
    export GOOS=${TARGETOS}
    export GOARCH=${TARGETARCH}
    binary_ext=-${TARGETOS}-${TARGETARCH}
else
    echo "Building native release..."
fi

ldf_cmp="-w -extldflags '-static'"
f_ver="-X main.Version=${VERSION:-dev}"

# Build the operator binary
echo "Building redis-operator binary at ./bin/redis-operator"
CGO_ENABLED=0 go build -o ./bin/redis-operator --ldflags "${ldf_cmp} ${f_ver}" ./cmd/redisoperator