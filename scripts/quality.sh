#!/usr/bin/env bash
# Prints the Code Quality score and the Testing score, both from 0 to 100.
#
# The scores are penalty based and normalised to the size of the code, so
# more code, more tests or more comments never raise them. The target is
# 85-95. There is no gate above 95, because a higher number comes from code
# that nobody wants to read.
#
#   scripts/quality.sh                  print the scores
#   scripts/quality.sh --check          also fail when a score is more than 2
#                                       points below .quality/baseline.json
#   scripts/quality.sh --write-baseline write .quality/baseline.json
#   QUALITY_RACE=1 scripts/quality.sh   also run `go test -race`
#
# Needs golangci-lint, deadcode, jq and awk. Run `make install-quality-tools`.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

mode="${1:-}"
baseline=.quality/baseline.json
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Code that a person writes and reads: no tests, mocks, generated code or
# separate modules.
prod_files() {
	git ls-files '*.go' | grep -vE '_test\.go$|^mocks/|^client/|^test/|zz_generated' || true
}
test_files() {
	git ls-files '*_test.go' | grep -v '^test/' || true
}
lines() { xargs cat | wc -l; }

prod_lines=$(prod_files | lines)
test_lines=$(test_files | lines)

# --- Code Quality: lint, complexity, duplication, dead code, file size ---
golangci-lint run --output.json.path="$work/lint.json" --output.text.path=/dev/null ./... || true
count_linter() { jq "[.Issues[] | select(.FromLinter == \"$1\")] | length" "$work/lint.json"; }
cognitive=$(count_linter gocognit)
clones=$(count_linter dupl)
layering=$(count_linter depguard)
other_lint=$(jq "[.Issues[] | select(.FromLinter | IN(\"gocognit\", \"dupl\", \"depguard\") | not)] | length" "$work/lint.json")
# Test doubles in production packages are not dead code.
dead=$(deadcode ./... 2>/dev/null | grep -cvE 'dummy\.go|mocks/|zz_generated|client/k8s|register\.go' || true)
big_files=$(prod_files | xargs wc -l | awk '$2 != "total" && $1 > 600' | wc -l)

# penalty(weight, count, cap): the count is scaled to a code base of 20k lines.
penalty() { awk -v w="$1" -v n="$2" -v cap="$3" -v loc="$prod_lines" \
	'BEGIN { p = w * n * 20000 / loc; print (p > cap ? cap : p) }'; }
quality=$(awk \
	-v a="$(penalty 0.5 "$cognitive" 10)" -v b="$(penalty 0.5 "$clones" 10)" \
	-v c="$(penalty 0.25 "$other_lint" 15)" -v d="$(penalty 0.25 "$dead" 10)" \
	-v e="$(penalty 1 "$big_files" 5)" -v f="$(awk -v n="$layering" 'BEGIN { p = 2 * n; print (p > 10 ? 10 : p) }')" \
	'BEGIN { printf "%.1f", 100 - a - b - c - d - e - f }')

# --- Testing: coverage, package floor, test size, hygiene, races ---
if [ ! -f coverage.out ]; then
	make --no-print-directory ci-unit-test-coverage >/dev/null
fi
# Per package: statements and covered statements, without generated code.
coverage_table=$(grep -v '^mode:' coverage.out | grep -v zz_generated | awk '{
	split($1, a, ":"); file = a[1]; sub(/\/[^\/]*$/, "", file)
	total[file] += $2; if ($3 > 0) covered[file] += $2
} END { for (p in total) printf "%s %d %d\n", p, total[p], covered[p] + 0 }')
coverage=$(echo "$coverage_table" | awk '{ t += $2; c += $3 } END { printf "%.1f", 100 * c / t }')
packages_ok=$(echo "$coverage_table" | awk '$2 >= 20 && $3 / $2 >= 0.75 { ok++ } $2 >= 20 { n++ } END { printf "%.3f", ok / n }')
sleeps=$(test_files | xargs grep -h 'time\.Sleep' | wc -l)
skips=$(test_files | xargs grep -h 't\.Skip' | wc -l)

race_points=0
race_available=0
race_note="skipped (set QUALITY_RACE=1)"
if [ "${QUALITY_RACE:-}" = 1 ]; then
	race_available=10
	if go test -race -count=1 ./api/... ./log/... ./metrics/... ./operator/... ./service/k8s/... >/dev/null 2>&1; then
		race_points=10
		race_note=clean
	else
		race_note=FAILED
	fi
fi
testing=$(awk -v cov="$coverage" -v ok="$packages_ok" -v tl="$test_lines" -v pl="$prod_lines" \
	-v sl="$sleeps" -v sk="$skips" -v rp="$race_points" -v ra="$race_available" 'BEGIN {
	# Coverage earns no more points above 90%, so nobody needs to chase it.
	c = 40 * (cov >= 90 ? 1 : cov / 90)
	f = 10 * ok
	r = tl / pl
	s = r <= 1.5 ? 25 : (r >= 3 ? 0 : 25 * (3 - r) / 1.5)
	h = 15 - 0.4 * sl - 0.15 * sk; if (h < 0) h = 0
	printf "%.1f", 100 * (c + f + s + h + rp) / (90 + ra)
}')

printf 'Code Quality: %s / 100\n' "$quality"
printf '  cognitive complexity > 20: %s | clones: %s | other lint findings: %s | unreachable funcs: %s\n' \
	"$cognitive" "$clones" "$other_lint" "$dead"
printf '  production files > 600 lines: %s | layering findings: %s\n' "$big_files" "$layering"
printf 'Testing: %s / 100\n' "$testing"
printf '  coverage (no generated code): %s%% | packages with 75%% or more: %s | test lines per production line: %s\n' \
	"$coverage" "$packages_ok" "$(awk -v t="$test_lines" -v p="$prod_lines" 'BEGIN { printf "%.2f", t / p }')"
printf '  time.Sleep in tests: %s | t.Skip: %s | race detector: %s\n' "$sleeps" "$skips" "$race_note"
if awk -v q="$quality" -v t="$testing" 'BEGIN { exit !(q > 97 || t > 97) }'; then
	echo "A score above 97 is a sign of over-engineering, not of quality. Check for needless code."
fi

json=$(jq -n --argjson q "$quality" --argjson t "$testing" '{code_quality: $q, testing: $t}')
case "$mode" in
--write-baseline)
	mkdir -p "$(dirname "$baseline")"
	echo "$json" >"$baseline"
	echo "Wrote $baseline"
	;;
--check)
	status=0
	for key in code_quality testing; do
		now=$(echo "$json" | jq ".$key")
		was=$(jq ".$key" "$baseline")
		if awk -v n="$now" -v w="$was" 'BEGIN { exit !(n < w - 2) }'; then
			echo "FAIL: $key is $now, more than 2 points below the baseline $was."
			status=1
		fi
	done
	exit $status
	;;
esac
