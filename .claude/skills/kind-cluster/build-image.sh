#!/usr/bin/env bash
# Builds the operator from the current checkout as redis-operator:TAG and
# serves it to the kind nodes through the local registry.
#
# Usage: build-image.sh [TAG]   (default TAG: dev)
#
# The build stage of docker/app/Dockerfile can't run in the sandbox: its
# `apk add` has no route to the package mirrors. Build the binaries on the
# host with scripts/build.sh instead, and use the runtime stage as it is.
set -euo pipefail

tag=${1:-dev}
repo=$(git rev-parse --show-toplevel)
here=$(cd "$(dirname "$0")" && pwd)
ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT

(cd "$repo" && ./scripts/build.sh >/dev/null)
mkdir -p "$ctx/src"
cp -r "$repo/bin" "$ctx/src/"
awk '/^FROM /{n++} n==2' "$repo/docker/app/Dockerfile" |
  sed 's#COPY --from=build #COPY #' >"$ctx/Dockerfile"
"$here/registry.sh" pull "$(awk '/^FROM /{print $2}' "$ctx/Dockerfile")"
docker build -q -t "redis-operator:$tag" "$ctx" >/dev/null
"$here/registry.sh" push "redis-operator:$tag"
