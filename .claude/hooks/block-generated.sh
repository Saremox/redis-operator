#!/usr/bin/env bash
# PreToolUse for Edit and Write. Generated files change only through their
# generator, so an edit by hand is lost at the next generation and fails
# `make verify-codegen`.
set -euo pipefail
file=$(jq -r '.tool_input.file_path // empty')
case "$file" in
*/zz_generated*.go | */api/redisfailover/v1/zz_generated*)
	target="make generate-deepcopy" ;;
*/manifests/databases.spotahome.com_redisfailovers.yaml | */manifests/kustomize/base/databases.spotahome.com_redisfailovers.yaml | */charts/redisoperator/crds/*)
	target="make generate-crd" ;;
*/client/k8s/clientset/*)
	target="make update-codegen" ;;
*/mocks/*)
	target="make mocks" ;;
*)
	exit 0 ;;
esac
echo "Generated file: $file. Change its source and run \`$target\` instead." >&2
exit 2
