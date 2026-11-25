---
name: quality-review
description: Measure the Code Quality score and the Testing score of this repository and decide what to fix first. Use before and after a cleanup, or when CI reports a drop in a score.
---

# Quality review

The scores come from `scripts/quality.sh`. They are deterministic, so two runs on the same commit give the same numbers.

## Run

1. `make install-quality-tools` once. It installs the pinned `golangci-lint`, `deadcode` and `govulncheck`.
2. `scripts/quality.sh`. It takes about 30 seconds when `coverage.out` exists, and a few minutes when it must run the tests first.
3. `QUALITY_RACE=1 scripts/quality.sh` adds the race detector.

## Read the result

- The Code Quality score loses points for functions with high cognitive complexity, clones, other lint findings, unreachable functions, files over 600 lines and layering findings.
- The Testing score loses points for coverage under 90%, packages under 75%, many test lines for each production line, `time.Sleep` and `t.Skip` in tests, and a failed race run.
- The target is 85-95. Do not raise a score above 95. A change that only adds code, tests or comments to raise a number is wrong. Delete or merge code instead.

## Fix in this order

1. Layering findings: they break the rule that only `service/k8s` calls Kubernetes.
2. Unreachable functions: delete them, and run `make mocks` if an interface shrinks.
3. Complexity and clones: extract one function or one table, not a framework.
4. Test size: merge copies into table tests, and delete a test that only repeats the code under test.

Update `.quality/baseline.json` with `scripts/quality.sh --write-baseline` only in a commit that explains why the score changed.
