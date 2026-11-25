#!/usr/bin/env bash
# PostToolUse for Edit and Write. A formatted file keeps the diff small and
# keeps the CI gofmt step green.
set -euo pipefail
file=$(jq -r '.tool_input.file_path // empty')
case "$file" in
*.go) gofmt -w "$file" ;;
esac
