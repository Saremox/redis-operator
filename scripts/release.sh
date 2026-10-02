#!/bin/bash
#
# Usage:
#   scripts/release.sh set-version <version>
#   scripts/release.sh check-version <version>
#   scripts/release.sh chart-values <chart-dir> <repository> <version>
#   <published release tags> | scripts/release.sh floating <version>
#
# A release workflow runs after its tag exists, so it cannot change the files
# at the tag. Users pin a tag in the raw manifest URLs and in the kustomize
# `?ref=`, so these files must carry the operator version before the tag.
# Run `set-version` and commit the result, then push the tag. The release
# workflow runs `check-version` and stops the release if a file disagrees.

set -eu

# The fields that must carry the version, as "file|pattern". A substitution
# that matches nothing changes nothing, so without this list a removed or
# renamed field passes the check.
version_fields=(
    "example/operator/all-redis-operator-resources.yaml|image: ghcr\.io/saremox/redis-operator:"
    "example/operator/operator.yaml|image: ghcr\.io/saremox/redis-operator:"
    "manifests/kustomize/base/deployment.yaml|image: ghcr\.io/saremox/redis-operator:"
    "manifests/kustomize/components/version/kustomization.yaml|^\s*newTag:"
    "manifests/kustomize/components/version/kustomization.yaml|^\s*app\.kubernetes\.io/version:"
    "charts/redisoperator/Chart.yaml|^version:"
    "charts/redisoperator/Chart.yaml|^appVersion:"
)
mapfile -t version_files < <(printf '%s\n' "${version_fields[@]%%|*}" | uniq)

usage() {
    sed -n '3,7p' "$0" | sed 's/^# *//' >&2
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

require_fields() {
    local field missing=0
    for field in "${version_fields[@]}"; do
        if ! grep -qE "${field#*|}" "${field%%|*}"; then
            echo "${field%%|*}: no line matches '${field#*|}'." >&2
            missing=1
        fi
    done
    if [ "$missing" -ne 0 ]; then
        echo "Put the field back, or update version_fields and with_version in scripts/release.sh." >&2
        exit 1
    fi
}

set_version() {
    local version=$1 file tmp
    require_fields
    for file in "${version_files[@]}"; do
        tmp=$(mktemp)
        with_version "$file" "$version" >"$tmp"
        cat "$tmp" >"$file"
        rm -f "$tmp"
    done
}

check_version() {
    local version=$1 file failed=0
    require_fields
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

# Prints, for GITHUB_OUTPUT, which floating tags a release moves: latest, the
# major tag and the major.minor tag. A release moves a tag only when it is the
# highest full release that the tag covers. Otherwise a patch on an older line
# moves the tag back. Reads the tags of the published releases on stdin.
floating() {
    local version=$1 major minor rest releases
    releases=$( (cat; echo "$version") | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -u -V)
    IFS=. read -r major minor rest <<<"$version"
    highest_is() {
        [ "$(grep -E "^$1" <<<"$releases" | tail -n1)" = "$version" ] && echo true || echo false
    }
    echo "latest=$(highest_is '')"
    echo "major=$(highest_is "${major}\.")"
    echo "minor=$(highest_is "${major}\.${minor}\.")"
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
floating)
    [ $# -eq 2 ] || usage
    validate_version "$2"
    floating "$2"
    ;;
*)
    usage
    ;;
esac
