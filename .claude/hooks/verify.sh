#!/usr/bin/env bash
# Stop hook. It checks the files that the session changed, so it takes
# seconds, not minutes. The full tests and the quality score stay in CI.
# Exit 2 sends the findings back to Claude. Claude Code stops the loop after
# 8 blocked stops in a row, so the message must say what to fix.
set -uo pipefail
cd "${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel)}"
export PATH="$PATH:$(go env GOPATH 2>/dev/null)/bin"

# A second stop in the same turn means the first block did not help.
[ "$(jq -r '.stop_hook_active // false')" = true ] && exit 0

changed=$(git status --porcelain | awk '{print $NF}')
fail=0

dirs=$(echo "$changed" | grep '\.go$' | grep -v '^test/soak/' | xargs -r -n1 dirname | sort -u | sed 's|^|./|')
if [ -n "$dirs" ] && command -v golangci-lint >/dev/null; then
	# shellcheck disable=SC2086
	out=$(golangci-lint run --new-from-rev=HEAD $dirs 2>&1) || { echo "$out" >&2; fail=1; }
fi

docs=$(echo "$changed" | grep '\.md$' | grep -v '^docs/migrations/')
if [ -n "$docs" ] && [ -f .vale.ini ] && command -v vale >/dev/null; then
	# shellcheck disable=SC2086
	out=$(vale --minAlertLevel=error $docs 2>&1) || { echo "$out" >&2; fail=1; }
fi

if [ "$fail" = 1 ]; then
	echo "Fix the findings above, then stop again." >&2
	exit 2
fi
