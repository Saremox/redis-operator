# Server versions and upgrades

## Default versions

The [defaults file](../api/redisfailover/v1/defaults.go) contains the image versions that the operator deploys.

## Migrate to Valkey

Valkey images ship `redis-server` and `redis-cli`, so a change of `redis.image` and `sentinel.image` to a Valkey image is an ordinary rolling update.

Migrate from Redis 7.2 only. Valkey forked from Redis 7.2 and cannot load the data of Redis 7.4 or later (`Can't handle RDB format version 12`). The first replica on Valkey then never syncs, so the operator does not replace the master. The RedisFailover continues on Redis, but the rollout never completes. To stop the migration, set `redis.image` back to the Redis image. The operator then replaces the unsynced Valkey replica, because that replica has no data to lose.

## Tested server versions and upgrade paths

The soak test runs RedisFailovers with these server images, in [operator-managed mode and in Sentinel mode](logic.md):

| Name | Image |
|---|---|
| Redis 6.2 | `redis:6.2.24-alpine` |
| Redis 7.2 | `redis:7.2.16-alpine` |
| Redis 7.4 | `redis:7.4.11-alpine` |
| Redis 8 | `redis:8.10.2-alpine` |
| Valkey 7.2 | `valkey/valkey:7.2.14-alpine` |
| Valkey 8 | `valkey/valkey:8.1.10-alpine` |
| Valkey 9 | `valkey/valkey:9.1.2-alpine` |

The soak test does not run Redis 6.0 and 7.0, other patch releases of Redis 7.4 and 8, or Valkey 8.0.

An in-place upgrade is a change of `spec.redis.image` on a RedisFailover that runs. The soak test made 3,775 such changes in a window of about 3.5 days. Each instance had up to 3 Redis pods, a 16 MiB data set and a write ledger. The ledger checks each acknowledged write. The Redis 6.2 row comes from a separate window of about 17.5 hours on two extra instances. All other rows come from the 3.5-day window.

| From | To | Result | Count (operator-managed / Sentinel mode) |
|---|---|---|---|
| Redis 6.2 | Redis 7.2 | works | 43 / 52 |
| Redis 7.2 | Redis 7.4 | works | 284 / 348 |
| Redis 7.4 | Redis 8 | works | 283 / 347 |
| Valkey 7.2 | Valkey 8 | works | 128 / 154 |
| Valkey 8 | Valkey 9 | works | 233 / 286 |
| Redis 7.2 | Valkey 7.2 | works | 128 / 151 |
| Redis 7.2 | Valkey 8 | works | 105 / 131 |
| Redis 7.2 | Valkey 9 | works | 120 / 104 |
| Redis 7.4 | Valkey 7.2 / 8 / 9 | fails safe | 171 / 162 / 154 (operator-managed mode only) |
| Redis 8 | Valkey 7.2 / 8 / 9 | fails safe | 155 / 171 / 160 (operator-managed mode only) |

"Works" means that the upgrade converged: one master, all pods Ready on the new version, status `Healthy`. All 2,802 upgrades with this result in the 3.5-day window converged, and the ledger found no lost acknowledged write.

"Fails safe" means that the rollout stops after the first replica. The new Valkey pod cannot load the data of the old master and stays not Ready. Its log shows `Can't handle RDB format version 12` (data from Redis 7.4) or `Can't handle RDB format version 15` (data from Redis 8). The master and the other replicas keep the old version and accept writes. In all 973 cases, the master stayed writable and the ledger found no lost acknowledged write.

Valkey forked from Redis 7.2.4. It reads Redis 7.2 data, but not the data format of Redis 7.4 or Redis 8. There is no in-place path from Redis 7.4 or Redis 8 to Valkey. [Migrate to Valkey](#migrate-to-valkey) tells how to stop such a rollout.

Two extra instances, one per mode, ran the chain Redis 6.2, 7.2, 7.4 and 8 and then reset. In about 17.5 hours they made 283 upgrades (operator-managed / Sentinel mode):

- Redis 6.2 to Redis 7.2: 43 / 52. Redis 7.2 loads the Redis 6.2 data format, so this step is an ordinary rolling update.
- Redis 7.2 to Redis 7.4: 43 / 52.
- Redis 7.4 to Redis 8: 42 / 51.

All 283 converged, and the ledger found no lost acknowledged write. Only the first step is in the table above. On this chain, an upgrade took 131 s on average (95th percentile 176 s) in operator-managed mode. It took 166 s (258 s) in Sentinel mode.

In the 3.5-day window, the soak test did not test these changes of `spec.redis.image`:

- Downgrades. [In-place downgrades](#in-place-downgrades) gives the results of a separate window.
- Skipped steps, such as Redis 6.2 to Redis 7.4, Redis 7.2 to Redis 8 or Valkey 7.2 to Valkey 9.
- Redis 6.2 to Valkey.
- Redis 7.4 or Redis 8 to Valkey in Sentinel mode.
- Valkey to Redis.

In Sentinel mode, the Sentinel image changes separately or follows the data image. The soak test ran these cases:

- Data image first, then Sentinel image, from Redis 7.2 through Valkey 7.2, 8 and 9: 447 Sentinel image upgrades, all converged.
- Sentinels on Valkey 9 with Redis 7.2 data, with the Sentinel image flipped between Valkey 9 and Redis 7.2: 320 flips, all converged.
- The Sentinel image follows the data image on the chain Redis 7.2, 7.4 and 8: converged.

The durations are relative figures from a test cluster with nested VMs and local volumes. A data image upgrade took 144 s on average (95th percentile 177 s) in operator-managed mode and 162 s (241 s) in Sentinel mode. Pods of two versions ran side by side for about 90 s.

In the 3.5-day window, the soak test did not cover:

- Node loss, network partitions and a full disk during an upgrade.
- Operator-to-operator upgrades.
- Data sets larger than 16 MiB on the upgrade instances, and more than 5 Redis pods.
- TLS, which the operator does not support. See [Security notes](install.md#security-notes).
- Recovery from a fails-safe Redis-to-Valkey change by a revert of the image. The soak test deletes and recreates the instance instead, which loses the data by design. [Migrate to Valkey](#migrate-to-valkey) describes the revert, and the soak test did not test it.

### In-place downgrades

An in-place downgrade is a change of `spec.redis.image` to an older server version. The soak test ran downgrades in a separate window of 24 hours, on one instance in operator-managed mode. The instance had 3 Redis pods, a 16 MiB data set and a write ledger.

| From | To | Result | Count (operator-managed mode) |
|---|---|---|---|
| Redis 7.4 | Redis 7.2 | fails safe | 46 |
| Redis 8 | Redis 7.4 | fails safe | 25 |
| Redis 8 | Redis 7.2 | fails safe | 20 |
| Valkey 8 | Valkey 7.2 | works | 70 |
| Valkey 9 | Valkey 8 | works | 23 |
| Valkey 9 | Valkey 7.2 | works | 22 |

"Works" means that the downgrade converged: one master, all pods Ready on the older version, status `Healthy`. A downgrade that fails safe stops the rollout after the first replica. The pod on the older version cannot read the data format of the newer master and stays not Ready. The master and the other replicas keep their version and accept writes. The ledger found no lost acknowledged write in the 206 downgrades.

To stop a downgrade that fails safe, restore the previous `redis.image`. The operator then replaces the unsynced replica, as [Migrate to Valkey](#migrate-to-valkey) describes. The soak test resets the instance after a downgrade that fails safe, so it did not test this revert.

The soak test did not test these downgrades:

- Downgrades to Redis 6.2.
- Downgrades in Sentinel mode.
