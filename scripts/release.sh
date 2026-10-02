#!/bin/bash
#
# Usage:
#   scripts/release.sh set-version <version>
#   scripts/release.sh check-version <version>
#   scripts/release.sh chart-values <chart-dir> <repository> <version>
#
# A release workflow runs after its tag exists, so it cannot change the files
# at the tag. Users pin a tag in the raw manifest URLs and in the kustomize
# `?ref=`, so these files must carry the operator version before the tag.
# Run `set-version` and commit the result, then push the tag. The release
# workflow runs `check-version` and stops the release if a file disagrees.

set -eu

version_files=(
    example/operator/all-redis-operator-resources.yaml
    example/operator/operator.yaml
    manifests/kustomize/base/deployment.yaml
    manifests/kustomize/components/version/kustomization.yaml
    charts/redisoperator/Chart.yaml
)

usage() {
    sed -n '3,6p' "$0" | sed 's/^# *//' >&2
    exit 2
}

validate_version() {
    if [[ ! "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$ ]]; then
        echo "Version '$1' is not X.Y.Z or X.Y.Z-rcN." >&2
        exit 2
    fi
}

# Writes the file with the version set to stdout.
with_version() {
    local file=$1 version=$2
    sed -E \
        -e "s|(image: ghcr\.io/saremox/redis-operator:).*|\1${version}|" \
        -e "s|^(\s*newTag:).*|\1 \"${version}\"|" \
        -e "s|^(\s*app\.kubernetes\.io/version:).*|\1 \"${version}\"|" \
        -e "s|^version:.*|version: ${version}|" \
        -e "s|^appVersion:.*|appVersion: \"${version}\"|" \
        "$file"
}

set_version() {
    local version=$1 file tmp
    for file in "${version_files[@]}"; do
        tmp=$(mktemp)
        with_version "$file" "$version" >"$tmp"
        cat "$tmp" >"$file"
        rm -f "$tmp"
    done
}

check_version() {
    local version=$1 file failed=0
    for file in "${version_files[@]}"; do
        if ! with_version "$file" "$version" | diff -u --label "$file" --label "$file (${version})" "$file" -; then
            failed=1
        fi
    done
    if [ "$failed" -ne 0 ]; then
        echo "The files above do not carry version ${version}." >&2
        echo "Run 'scripts/release.sh set-version ${version}', commit the result, and tag that commit." >&2
        exit 1
    fi
    echo "All files carry version ${version}."
}

# Sets only the operator image in the chart values. The upgrade hook image
# (crds.upgradeHook.image) must keep kubectl, which the operator image does
# not have.
chart_values() {
    local chart=$1 repository=$2 version=$3
    sed -i -E \
        -e "/^image:/,/^[^[:space:]#]/{
            s|^(  repository:).*|\1 ${repository}|
            s|^(  tag:).*|\1 \"${version}\"|
        }" \
        "${chart}/values.yaml"
}

root=$(dirname "$0")/..

case "${1:-}" in
set-version)
    [ $# -eq 2 ] || usage
    validate_version "$2"
    cd "$root"
    set_version "$2"
    ;;
check-version)
    [ $# -eq 2 ] || usage
    validate_version "$2"
    cd "$root"
    check_version "$2"
    ;;
chart-values)
    [ $# -eq 4 ] || usage
    validate_version "$4"
    chart_values "$2" "$3" "$4"
    ;;
*)
    usage
    ;;
esac
