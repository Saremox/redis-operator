# Customization

## Custom shutdown script

The operator gives the Redis container a preStop hook in two cases: with Sentinel, and with a custom `redis.shutdownConfigMap`. The hook runs the shutdown script, and the script makes Redis `SAVE` its data before it stops. When Sentinel runs and the pod is the master, the script first pauses the writes, waits for the replicas, and asks Sentinel to fail over. Thus Sentinel moves the master immediately and does not wait for `down-after-milliseconds`. The Redis pods have no service links, so the script finds Sentinel through the Service name `rfs-<NAME>` on port 26379.

After the failover, the script makes the old master a replica of the new master. The pause prevents the loss of writes that the old master acknowledged, because the clients get `READONLY` instead. If the pause fails, the script does not ask for a failover. Redis 7 and later then pause the writes when they stop and wait for their replicas, and Sentinel fails over after `down-after-milliseconds`. The pause fails on Redis before 6.2 and after a password change. After a password change, the `REDIS_PASSWORD` variable of a pod keeps the old password until the pod restarts.

The script waits a maximum of 12 seconds for the new master. Thus the script ends inside the default 30-second grace period, and the `SAVE` can run. After a password change, the `SAVE` of the script also fails. Then Redis saves its data when it stops only if `save` points are set. The default `redis.conf` sets `save 900 1` and `save 300 10`.

Without Sentinel and without a custom `redis.shutdownConfigMap`, the container has no preStop hook. The script would only send `SAVE`, and that save is not needed. `redis-server` is PID 1, and the default `redis.conf` sets save points. Redis thus saves its data itself when it gets SIGTERM. The ConfigMap and its mount at `/redis-shutdown` stay in both modes, so that a user who runs the script by hand still finds it.

If `redis.customConfig` sets `save ""`, Redis writes no RDB when it stops. The preStop `SAVE` was the only save at stop time. A pod replacement then loses every write since the last RDB, unless `appendonly yes` is set. To keep a save at stop, add `shutdown-on-sigterm save` to `customConfig` on Redis 7 or later, or keep a save point.

To replace the default script, create a ConfigMap with a `shutdown.sh` key in the namespace of the RedisFailover. Set its name in `redis.shutdownConfigMap`. See the [shutdown example file](../example/redisfailover/custom-shutdown.yaml). A custom script replaces all of the default script, also the write pause. The container keeps the preStop hook with a custom script, also without Sentinel. Without the pause, a failover can lose writes, see [Sentinel failover and write loss](configuration.md#sentinel-failover-and-write-loss).

## Custom SecurityContext

By default, the operator runs the pods as user and group `1000` with `runAsNonRoot: true`, `fsGroup: 1000` and the `RuntimeDefault` seccomp profile.
Set `securityContext` in the `redis` or `sentinel` spec to change the pod security context, for example to run as a specific user.
See the [SecurityContext example file](../example/redisfailover/security-context.yaml) and the Kubernetes documentation about [security context](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/).

The operator merges a custom `securityContext` with its defaults. A field that you set has priority, and a field that you do not set keeps its default. For example, a `securityContext` with only `runAsUser` keeps `fsGroup` and `runAsNonRoot`.

## Custom containerSecurityContext at container level

By default, the operator runs the containers as user and group `1000` with `runAsNonRoot: true`, drops `ALL` capabilities, and sets `privileged: false`, `allowPrivilegeEscalation: false` and `readOnlyRootFilesystem: true`.
Set `containerSecurityContext` in the `redis` or `sentinel` spec to change the container security context.
For example, you can add capabilities or allow writes to the root file system.
See the [ContainerSecurityContext example file](../example/redisfailover/container-security-context.yaml) and the [available keys](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.20/#securitycontext-v1-core).

The operator merges a custom `containerSecurityContext` with its defaults. A field that you set has priority, and a field that you do not set keeps its default. For example, `allowPrivilegeEscalation: false` stays if you do not set it. A custom `capabilities` replaces the default `capabilities` fully. Add `drop: [ALL]` to it yourself.

## Custom command

By default, the operator starts Redis and Sentinel with this command and the configuration file:

- Redis: `redis-server /redis/redis.conf`
- Sentinel: `redis-server /redis/sentinel.conf --sentinel`

To change the command, set `command` in the `redis` or `sentinel` spec. See the [custom command example file](../example/redisfailover/custom-command.yaml).

**Important**: a custom `redis.command` replaces the default command. With `auth.secretPath`, the default command passes `--requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD"` to `redis-server`. The password is not in `redis.conf`, so a custom command must pass these flags itself. Otherwise Redis starts without a password. Kubernetes does not expand `$REDIS_PASSWORD` in a command, so use a shell:

```yaml
command:
  - sh
  - -c
  - exec redis-server /redis/redis.conf --requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD"
```

## Custom environment variables

Use `redis.env` and `sentinel.env` to add environment variables (standard Kubernetes `EnvVar` entries) to the main Redis and Sentinel containers.

The operator adds its own variables `REDIS_ADDR`, `REDIS_PORT` and `REDIS_USER` to the main Redis container, the init containers and the extra containers. It adds `REDIS_PASSWORD` only with `auth.secretPath`. The operator does not add these variables to the main Sentinel container. In the Redis containers, the variables of the operator have priority over a user variable with the same name.

Do not set `REDIS_PASSWORD` in `redis.env`. Without `auth.secretPath`, the shutdown script and the readiness script use this value as the Redis password.

## Custom Priority Class
To use a custom Kubernetes [Priority Class](https://kubernetes.io/docs/concepts/configuration/pod-priority-preemption/#priorityclass) for the Redis or Sentinel pods, set `priorityClassName` in the `redis` or `sentinel` spec. This field has no default. **Note:** the operator does not create the `PriorityClass` resource.

## Custom Service Account
To use a custom Kubernetes [Service Account](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/) for the Redis or Sentinel pods, set `serviceAccountName` in the `redis` or `sentinel` spec. Without it, the Redis pods use the `default` ServiceAccount, and the Sentinel pods use the `rfs-sa-<NAME>` ServiceAccount that the operator creates. Without it, the pods also do not mount the ServiceAccount token. **Note:** the operator does not create the `ServiceAccount` resource that you specify.

## Custom Pod Annotations
By default, the Sentinel pods have no annotations. The Redis pods have the `redisfailovers.databases.spotahome.com/secret-checksum` annotation. The operator changes it when the password changes, so that the Redis pods restart.

To add pod annotations, set `podAnnotations` in the `redis` or `sentinel` spec. See the [custom annotations example file](../example/redisfailover/custom-annotations.yaml).

## Custom Service Annotations
By default, the `rfr-<NAME>` service has the `prometheus.io/scrape`, `prometheus.io/port` and `prometheus.io/path` annotations. This service exists only when `redis.exporter.enabled` is `true`. The other services have no annotations.

To add service annotations, set `serviceAnnotations` in the `redis` or `sentinel` spec. See the [custom annotations example file](../example/redisfailover/custom-annotations.yaml).

## Exporter authentication
By default, the exporter ports have no authentication. To allow only clients that know a username and a password, set `REDIS_EXPORTER_BASIC_AUTH_USERNAME` and `REDIS_EXPORTER_BASIC_AUTH_PASSWORD` in `redis.exporter.env` and in `sentinel.exporter.env`. Read both values from a Secret with `secretKeyRef`. The exporter then checks the credentials on all paths. If one value is empty, the exporter does not check credentials.

The `basicAuth` field of a ServiceMonitor sends the credentials. Prometheus Operator and Alloy read the Secret from the namespace of the ServiceMonitor. The `prometheus.io` annotations have no field for credentials. To stop that scrape, set `prometheus.io/scrape: "false"` in `redis.serviceAnnotations`. See the [exporter authentication example file](../example/redisfailover/exporter-basic-auth.yaml).

The variables change the pod templates, so the operator replaces the Redis pods, the master last, and rolls the Sentinel pods. A container reads the Secret only when it starts. After you change the Secret, restart the pods that run an exporter. Until then, the exporters use the old password.

## Control of label propagation
By default, the operator copies all labels of the RedisFailover to the resources that it creates.
These labels are not always under your control, for example when a GitOps tool manages them.
A change of these labels can fail on immutable resources such as PodDisruptionBudgets.
To select the labels that the operator copies, set the `labelWhitelist` option in the spec.

Without `labelWhitelist`, or with an empty list, the operator copies all labels.

Each item in the list is a regular expression. See the [label propagation example](../example/redisfailover/control-label-propagation.yaml) and the [syntax reference](https://github.com/google/re2/wiki/Syntax).

To copy no labels, use a regular expression that matches no label.

NOTE: The operator always adds the labels that it needs to the resources. These are the labels:
```
app.kubernetes.io/component
app.kubernetes.io/managed-by
app.kubernetes.io/name
app.kubernetes.io/part-of
redisfailovers-role
redisfailovers.databases.spotahome.com/name
```


## ExtraVolumes and ExtraVolumeMounts

Use `extraVolumes` and `extraVolumeMounts` in `spec.redis` and `spec.sentinel` to mount more volumes, for example with configuration files or Secrets. Typical uses are:
- Secrets for a sidecar that copies the RDB files
- Users with their Secrets and ACLs, for an init container that creates more users
- Configuration files that merge with the Redis configuration
- Failover scripts for more operations

```
---
apiVersion: v1
kind: Secret
metadata:
  name: foo
  namespace: exm
type: Opaque
stringData:
  password: MWYyZDFlMmU2N2Rm
---
apiVersion: databases.spotahome.com/v1
kind: RedisFailover
metadata:
  name: foo
  namespace: exm
spec:
  sentinel:
    enabled: true
    replicas: 3
    extraVolumes:
    - name: foo
      secret:
        secretName: foo
        optional: false
    extraVolumeMounts:
    - name: foo
      mountPath: "/etc/foo"
      readOnly: true
  redis:
    replicas: 3
    extraVolumes:
    - name: foo
      secret:
        secretName: foo
        optional: false
    extraVolumeMounts:
    - name: foo
      mountPath: "/etc/foo"
      readOnly: true
```
