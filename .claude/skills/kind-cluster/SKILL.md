---
name: kind-cluster
description: Create a kind Kubernetes cluster inside the Claude Code cloud sandbox and run this repo's integration tests or a Helm-installed operator against it. Use when a change needs e2e testing on a real API server.
---

# kind cluster in the cloud sandbox

A plain `kind create cluster` fails in the sandbox, and a working cluster
still cannot pull images. The scripts here solve these problems.

## Create

```sh
.claude/skills/kind-cluster/kind-up.sh e2e v1.35.0 244
export KUBECONFIG=/tmp/kind-e2e/kubeconfig
```

The script:

1. Starts `dockerd` if it does not run.
2. Writes a kind config with two sandbox fixes:
   - `failCgroupV1: false`: the sandbox is cgroup v1, and kubelet >= 1.35
     refuses to start on it.
   - containerd `restrict_oom_score_adj = true`: the kernel rejects negative
     `oom_score_adj`, so every pod sandbox would fail with `can't get final
     child's PID from pipe: EOF`. This affects every node version, 1.34 too.
3. Makes a local registry, `kind-registry`, the node's mirror for docker.io
   and quay.io (`registry.sh`).
   - The node cannot reach a registry, because its `HTTPS_PROXY` points at
     the 127.0.0.1 proxy of the sandbox.
   - `kind load` is not sufficient: the operator default is `imagePullPolicy:
     Always`, so preloaded images still fail with `ImagePullBackOff`.
4. Copies the images from `api/redisfailover/v1/defaults.go`, plus any in
   `EXTRA_IMAGES`, into the registry.
   - quay.io is blocked from the host too, so `registry.sh` falls back to the
     Docker Hub and `mirror.gcr.io` copies.
   - Add another image any time with `registry.sh push IMAGE`.
5. Adds a host route to the pod subnet: the integration tests connect to
   Redis pod IPs.
6. Applies the RedisFailover CRD.

## Several clusters at once

Give each cluster its own name and subnet octet, for example `a … 181` and
`b … 185`. A cluster uses `10.SUBNET.0.0/16` for pods and
`10.SUBNET+1.0.0/16` for services, where SUBNET is the third argument.
Keep the octets at least 2 apart. Overlapping pod subnets make the host
routes collide. The registry is shared. The machine has 4 CPUs, so two
single-node clusters is the practical limit.

## Integration tests

```sh
go test ./test/integration/... -tags integration -v -timeout 30m
```

They run the operator in-process against `$KUBECONFIG`.

## Operator image and Helm

```sh
.claude/skills/kind-cluster/build-image.sh pr     # redis-operator:pr
helm upgrade --install redis-operator ./charts/redisoperator \
  --set image.repository=redis-operator --set image.tag=pr --wait
```

- `docker/app/Dockerfile` fails here, because `apk add` cannot reach the
  package mirrors. `build-image.sh` builds the binaries on the host and uses
  the runtime stage of the Dockerfile.
- If helm is missing, install it with
  `GOBIN=/usr/local/bin go install helm.sh/helm/v3/cmd/helm@v3.19.0`.
- `.github/workflows/e2e.yml` has a RedisFailover manifest and checks that
  you can use again.
- Do not run the integration tests while a Helm-installed operator runs:
  both operators would reconcile the RedisFailovers of the tests.

## Debugging a failed create

`kind-up.sh` does not pass `--retain` to kind, so kind deletes a failed
node. To keep the node and read its logs:

1. Copy the `kind create cluster` command from `kind-up.sh`.
2. Add `--retain` and `--config /tmp/kind-NAME/kind.yaml`, and run it.
3. Read `docker exec NAME-control-plane journalctl -u kubelet --no-pager`.
4. Read `docker exec NAME-control-plane journalctl -u containerd --no-pager`.

## Delete the cluster

```sh
kind delete cluster --name e2e
docker rm -f kind-registry    # once no cluster needs it
```

This leaves the host route behind. It is harmless, and a new cluster
replaces it.
