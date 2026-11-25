# redis-operator

A Kubernetes operator that creates and heals Redis failover clusters (`RedisFailover` resource). By default the operator elects the master. With `sentinel.enabled: true`, Redis Sentinel does it. This is a fork of `spotahome/redis-operator`, so the API group stays `databases.spotahome.com`.

## Layout

- `api/redisfailover/v1`: CRD types, defaults and validation.
- `operator/redisfailover`: the controller and the check and heal logic. `service/` here holds the generators, the checker and the healer.
- `service/k8s`, `service/redis`: all calls to the Kubernetes API and to Redis.
- `cmd/redisoperator`, `cmd/instance`: the operator binary and the `redis-instance` binary that runs in each Redis pod.
- `log`, `metrics`, `version`: logrus wrapper, Prometheus metrics, version string.
- `mocks`, `client`, `api/**/zz_generated*`, the CRD copies in `manifests/` and `charts/*/crds/`: generated. Change the source, then run the make target.
- `test/integration`: needs a cluster. `test/soak`: separate Go module for long release-candidate tests.
- `docs`: read the file for your topic only. `docs/logic.md` explains the shared failover logic, with `docs/logic-operator-managed.md` and `docs/logic-sentinel.md` for each mode. `docs/development.md` lists the make targets, `docs/writing.md` the writing rules, and `docs/install.md`, `docs/configuration.md`, `docs/autoscaling.md`, `docs/customization.md`, `docs/authentication.md` and `docs/versions.md` the user guide.

## Rules and their reasons

- Call the Kubernetes API only through `service/k8s`. The service layer is the one place that records metrics and wraps errors. The controller files `controller.go`, `factory.go` and `leaderelection.go` are the exception.
- Wrap errors with `fmt.Errorf("...: %w", err)`. Return an error or log it, not both. The reconcile loop logs what you return.
- Log with logrus through `log.Logger`. `cmd/instance` writes to the container output on purpose.
- Ask before you change failover behaviour, the CRD schema or a default value. A change there reaches production clusters.
- Do not add features, tests, files, docs or refactors that nobody asked for. Fewer lines are easier to read. Add a test for new behaviour, and prefer a table test to a copy.
- Write comments, docs and commit messages as `docs/writing.md` says: the reason, not a repeat of the code.

## API and resource facts

- The `RedisFailover` spec is in `api/redisfailover/v1/types.go`. Defaults are in `defaults.go`. `Validate()` in `validate.go` sets them, so it changes the spec.
- After a change of the types, run `make generate-api`, then `git add api/ manifests/ charts/redisoperator/crds/`, then `make verify-codegen`. The check compares with the staged files. Run `make update-codegen` only when the shape of the API changes.
- Keep the CRD backward compatible. Add optional fields with defaults.
- Each object that the operator creates has an owner reference to the `RedisFailover`. The exception is a PVC with `storage.keepAfterDeletion: true`.
- Redis StatefulSets are named `rfr-<name>` and Sentinel Deployments `rfs-<name>`. A `RedisFailover` name has at most 48 characters, so the generated names stay valid.

## Check your work

- `make ci-unit-test` runs the unit tests. Some tests skip without `redis-server`.
- `make verify-codegen` checks the generated API code. Run `make generate-api` after a change in `api/redisfailover/v1`.
- `scripts/quality.sh` prints the Code Quality score and the Testing score. The target is 85-95. A higher score is not a goal.
- `make ci-integration-test` needs a cluster. In the cloud sandbox, use the `kind-cluster` skill.

Hooks in `.claude/hooks` stop edits to generated files, format Go files, and lint the changed packages before a session ends. Keep working until everything that was asked is done. Stop to ask only when you cannot go on, or before a risky step. Do not change the model or the effort in the middle of a task. Do not start reviewer subagents unless asked.
