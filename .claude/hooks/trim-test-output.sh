#!/usr/bin/env bash
# PreToolUse for Bash. A verbose `go test -v` run prints thousands of lines
# for passing tests. The filter drops the passing lines and keeps failures,
# panics and the package summary, so the context stays small. pipefail keeps
# the exit code of go test. Only a plain `go test` command is rewritten.
set -euo pipefail
cmd=$(jq -r '.tool_input.command // empty')
case "$cmd" in
"go test "*) ;;
*) exit 0 ;;
esac
case "$cmd" in
*"|"* | *">"* | *"&&"* | *";"* | *'$('* | *'`'*) exit 0 ;;
esac
filter='^(=== (RUN|PAUSE|CONT|NAME)|\s*--- PASS|PASS$)'
new="set -o pipefail; $cmd 2>&1 | grep -Ev '$filter'"
jq -n --arg c "$new" '{hookSpecificOutput: {hookEventName: "PreToolUse", updatedInput: {command: $c}}}'
