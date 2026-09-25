#!/usr/bin/env bash
# Builds the operator from the current checkout as redis-operator:TAG and
# serves it to the kind nodes through the local registry.
#
# Usage: build-image.sh [TAG]   (default TAG: dev)
#
# docker/app/Dockerfile can't be used in the sandbox: its `apk add` steps have
# no route to the package mirrors. Build the binary on the host instead and
# copy it into the same alpine base with the same non-root user.
set -euo pipefail

tag=${1:-dev}
repo=$(git rev-parse --show-toplevel)
here=$(cd "$(dirname "$0")" && pwd)
ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT

(cd "$repo" && CGO_ENABLED=0 go build -o "$ctx/redis-operator" -ldflags "-w" ./cmd/redisoperator)
cat >"$ctx/Dockerfile" <<'EOF'
FROM alpine:latest
COPY redis-operator /usr/local/bin/redis-operator
RUN addgroup -g 1000 rf && adduser -D -u 1000 -G rf rf
USER rf
ENTRYPOINT ["/usr/local/bin/redis-operator"]
EOF
# docker build pulls a missing base image straight from Docker Hub.
"$here/registry.sh" pull alpine:latest
docker build -q -t "redis-operator:$tag" "$ctx" >/dev/null
"$here/registry.sh" push "redis-operator:$tag"
