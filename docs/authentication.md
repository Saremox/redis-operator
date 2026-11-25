# Redis authentication

## Redis authentication

To enable authentication:

1. Create a Secret with a `password` key:

   ```
   echo -n "pass" > password
   kubectl create secret generic redis-auth --from-file=password
   ```

2. Set `auth.secretPath` to the name of the Secret:

   ```yaml
   apiVersion: databases.spotahome.com/v1
   kind: RedisFailover
   metadata:
     name: redisfailover
   spec:
     sentinel:
       enabled: true
       replicas: 3
     redis:
       replicas: 1
     auth:
       secretPath: redis-auth
   ```

You can change the `password` key of that Secret in place, add `auth.secretPath`, or remove it. An operator restart during the change needs no manual recovery in most cases when the operator can read and write the Secret `rfr-auth-applied-<NAME>` (see below). The operator first sets the new password on each Redis with `CONFIG SET` and on each Sentinel with `SENTINEL SET`, so replication continues. Then it restarts the Redis pods one at a time. New connections must use the new password immediately. After a removal, Redis refuses `AUTH <password>` with `ERR AUTH <password> called without any password configured`, so clients must not send the password.

After you change or remove `auth.secretPath`, keep the previous Secret until the operator restarts every Redis pod. A pod on the previous pod template reads this Secret when its container restarts. The operator reads it to find the password that such a pod runs with.

By default, the operator finds a change at its next sync (`--sync-interval`, 30s by default). The chart value `watchAuthSecrets: true` (flag `--watch-auth-secrets`) applies a change immediately. It is off by default because it needs `list` and `watch` on secrets in all namespaces, which can read every Secret. With `watchAuthSecrets: true`, the chart grants these verbs. With kustomize, the kubectl manifests or your own ClusterRole, add `list` and `watch` on `secrets` yourself. If the operator cannot list the Secrets, it only logs the error and finds a change at its next sync.

Until a pod restarts, each program that reads the password from the environment of the pod has the old password. The exporter sidecar cannot authenticate, so it reports Redis down. After a change or an addition of the password, the pre-stop `SAVE` fails, and custom probes that use `$REDIS_PASSWORD` also fail. After a removal, `redis-cli` runs the command after the refused `AUTH`, so the pre-stop `SAVE` and probes that use `redis-cli` work.

Until a pod restarts after a change or an addition of the password, the default readiness probe also reports it Ready without a replication check. The probe uses the old password, which Redis refuses. A refused password says nothing about replication, so the probe does not fail the pod for it. During this time, `rfrs-<NAME>` can send reads to a replica that is not synced.

The operator keeps the passwords that the Redis pods run with in the Secret `rfr-auth-applied-<NAME>`. After its start, the operator reads this Secret at the first check of each RedisFailover, also without `auth.secretPath`. After a complete change, the Secret contains only the new password, or empty values after you remove `auth.secretPath`. The RedisFailover owns the Secret, so Kubernetes deletes it with the RedisFailover. Do not change this Secret. The operator does not read or change a Secret with this name that the RedisFailover does not control.

The operator needs `create` and `update` on `secrets` for this Secret. The Helm chart, `manifests/kustomize` and `example/operator` grant these verbs. With these verbs, the operator can change every Secret in the cluster. Without them, the operator logs a warning and keeps the passwords only in memory. If you create a RedisFailover again with the same name before Kubernetes deletes its old Secret, `OwnerReferencesPermissionEnforcement` can refuse the update of the Secret. The operator then logs a warning and writes the Secret at the next password change.

If a Redis pod runs with a password that the operator does not know, the operator cannot switch the pod. This occurs, for example, when the operator cannot read or write `rfr-auth-applied-<NAME>` and restarts between a change and its next check. The RedisFailover then reports `unable to apply the configured password`. Until you recover, the operator does not restart Redis pods and does not start an operator-managed failover. The operator reads the password of a pod on a previous pod template from the Secret that this template names, if that Secret still exists. To recover from a password change:

1. Put the previous password back in the auth Secret (`auth.secretPath`).
2. Wait until the RedisFailover is healthy.
3. Change the password again.

To recover from a removal of `auth.secretPath`:

1. Set `auth.secretPath` to a Secret with the previous password.
2. Wait until the RedisFailover is healthy.
3. Remove `auth.secretPath` again.

The operator reads the password from the Secret and gives it to `redis-server` with `--requirepass` and `--masterauth` from an environment variable.
The password is **not** in the Redis ConfigMap, so it is never visible as plain text in a ConfigMap or in the pod spec. The operator also keeps it in the Secret `rfr-auth-applied-<NAME>`.
A custom `redis.command` must pass these flags itself.
