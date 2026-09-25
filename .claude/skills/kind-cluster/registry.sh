#!/usr/bin/env bash
# Local registry that the kind nodes use as their docker.io and quay.io
# mirror.
#
# Usage: registry.sh connect CLUSTER   start the registry, make it CLUSTER's mirror
#        registry.sh push IMAGE        copy IMAGE from the host into it
#        registry.sh pull IMAGE        pull IMAGE to the host, via a mirror if needed
set -euo pipefail

cmd=${1:-}
arg=${2:?usage: registry.sh connect CLUSTER | push IMAGE | pull IMAGE}
reg=kind-registry

# Repository path without the registry host, as containerd asks the mirror
# for it: redis:7 -> library/redis:7, quay.io/a/b:1 -> a/b:1.
repo_path() {
  local first=${1%%/*}
  if [[ $1 != */* ]]; then
    echo "library/$1"
  elif [[ $first == *.* || $first == *:* ]]; then
    echo "${1#*/}"
  else
    echo "$1"
  fi
}

# Pulls IMAGE unless the host has it. quay.io is blocked and Docker Hub may
# rate limit, so fall back to Docker Hub's copy and mirror.gcr.io.
ensure_local() {
  local img=$1 path src
  docker image inspect "$img" >/dev/null 2>&1 && return
  path=$(repo_path "$img")
  for src in "$img" "$path" "mirror.gcr.io/$path"; do
    if docker pull -q "$src" >/dev/null 2>&1; then
      [[ $src == "$img" ]] || docker tag "$src" "$img"
      return
    fi
  done
  echo "failed to pull $img" >&2
  return 1
}

case $cmd in
connect)
  if ! docker inspect "$reg" >/dev/null 2>&1; then
    ensure_local registry:2
    docker run -d --restart=always --name "$reg" -p 127.0.0.1:5001:5000 registry:2 >/dev/null
  fi
  docker network connect kind "$reg" 2>/dev/null || true
  # Plain HTTP, so containerd doesn't send it through the unreachable
  # HTTPS proxy.
  for host in docker.io quay.io; do
    docker exec "$arg-control-plane" mkdir -p "/etc/containerd/certs.d/$host"
    printf '[host."http://%s:5000"]\n  capabilities = ["pull", "resolve"]\n' "$reg" |
      docker exec -i "$arg-control-plane" cp /dev/stdin "/etc/containerd/certs.d/$host/hosts.toml"
  done
  ;;
pull)
  ensure_local "$arg"
  ;;
push)
  img=$arg
  path=$(repo_path "$img")
  ensure_local "$img"
  docker tag "$img" "localhost:5001/$path"
  docker push -q --platform linux/amd64 "localhost:5001/$path" >/dev/null
  echo "$img -> $reg/$path"
  ;;
*)
  echo "usage: registry.sh connect CLUSTER | push IMAGE | pull IMAGE" >&2
  exit 1
  ;;
esac
