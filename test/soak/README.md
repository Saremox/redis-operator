# Soak tester

A long-running tester for redis-operator release candidates. It runs
in-cluster next to an installed operator, probes a fixed set of
RedisFailover instances and exports the results as Prometheus metrics, so an
RC can be judged from a dashboard after hours or days.

It is a separate Go module that builds against the operator's API through a
`replace` to the repository root, so a tester built from a tag always matches
that tag's spec.

## What it does today

For every configured instance it probes the master through the
`rfrm-<name>` Service, once per `probe.interval`, with three client styles:

- `pooled`: one long-lived go-redis client, like an application.
- `retrying`: the same with go-redis's default retries, like an application
  that didn't tune its client.
- `fresh`: a new connection for every probe, like a newly started pod.

Each probe is a `SET soak:<rf>:rfrm:<client>:seq <n>` followed by a `GET` of
the same key. Except for `retrying`, go-redis retries are off, so every
failure is counted. `retrying` retries within the same `probe.timeout`.

## Running it

```sh
make build test lint      # binary in bin/soak
make image                # ghcr.io/saremox/redis-operator-soak:<git describe>
make kind-e2e             # kind cluster, operator, op-basic, tester, assertions
```

`kind-e2e` reuses `.claude/skills/kind-cluster/`, installs the operator from
this checkout with `charts/redisoperator`, creates `op-basic`, deploys the
tester, lets it run for `DURATION` seconds (default 120) and asserts from
`/metrics` that every client style can write and read through `rfrm`. Set
`OPERATOR_VERSION=4.2.0-rc1` to install a released chart and image instead.

### Pointing it at an RC

1. Install the RC:
   `helm install redis-operator oci://ghcr.io/saremox/redis-operator/charts/redis-operator --version <tag> -n redis-operator --create-namespace`.
2. Create the instances listed in the config.
3. Set the tester image to the same tag and apply the manifests:

   ```sh
   cd deploy && kustomize edit set image ghcr.io/saremox/redis-operator-soak:<tag>
   kubectl apply -k deploy
   ```

The Role in `deploy/rbac.yaml` must be in the operator's namespace and name
its Deployment, matching `operator` in `deploy/config.yaml`.

## Configuration

See [`config.example.yaml`](config.example.yaml). `deploy/config.yaml` is the
config the manifests ship.

## Metrics

Served on `:9090/metrics`; `/healthz` answers `ok`. Per-instance series carry
`rf`, `namespace` and `mode` (`operator` or `sentinel`).

| Metric | Type | Extra labels |
|---|---|---|
| `redis_soak_probe_total` | counter | `path`, `client`, `op` (`set`/`get`), `result` |
| `redis_soak_probe_duration_seconds` | histogram | `path`, `client`, `op` |
| `redis_soak_writable`, `redis_soak_readable` | gauge (0/1) | `path`, `client` |
| `redis_soak_last_success_timestamp_seconds` | gauge | `path`, `client`, `op` |
| `redis_soak_outage_duration_seconds` | histogram | `path`, `client` |
| `redis_soak_build_info` | gauge | `operator_version`, `tester_version` |

`result` is one of `ok`, `timeout`, `refused`, `readonly`, `loading`, `auth`,
`oom`, `masterdown`, `noreplicas`, `dns`, `closed` and `other`. Server errors
are classified by their leading error code, never by the message, so Redis and
Valkey count the same way. `closed` is a connection closed or reset by the
server, such as the operator disconnecting clients from a demoted master.

An outage runs from the first failed probe to the next successful one, per
path and client style. `operator_version` is the image tag of the operator
Deployment, re-read every 30 seconds.
