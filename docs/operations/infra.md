# Infra-dependencies: Postgres, Redis, Vault

Soul Stack exploits three required external dependencies ([ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres) / [ADR-006](../adr/0006-cache-redis.md) / [requirements.md](../requirements.md)). The production installation must raise them **separately** from the Keeper cluster - Soul Stack does not manage them, only consumes them.

This documentation area focuses on the **operational part**: what to backup, how to restore, what settings are critical for Keeper to work correctly. The architectural "why" is in the corresponding ADRs.

## Postgres

Source of truth Keeper cluster. Stateless Keeper instances write here everything that survives the restart: registries (`souls` / `operators` / `rbac_*` / `service_registry` / ...), `incarnation.state` + `state_history`, `apply_runs`, `audit_log`, `plugin_sigils`. The full catalog of tables is [`docs/keeper/storage.md`](../keeper/storage.md).

### Version and mode

| Parameter | Minimum | Recommended |
|---|---|---|
| PostgreSQL version | 14 | 16 (LTS window longer) |
| Connection-encryption | TLS (`sslmode=require`) - required in prod | `sslmode=verify-full` with server-cert from internal PKI |
| Search path | default `public` | default `public` |
| Encoding | `UTF8` | `UTF8` |
| Locale | `C` / `en_US.UTF-8` | `en_US.UTF-8` (for text search) |

DSN is transmitted via Vault KV (`postgres.dsn_ref: vault:secret/keeper/postgres`, field `dsn`) - plaintext-DSN in `keeper.yml` is prohibited by the parser ([`docs/keeper/config.md` → postgres](../keeper/config.md#postgres)).

### HA topology

| Solution | When |
|---|---|
| **Single instance** + regular backup | dev/staging. Cont **not recommended** - single-point-of-failure. |
| **Patroni** (streaming replication + automated failover via etcd/consul/raft) | Prod on-premise. Promotion of replicas when the primary fails is automatic, Keeper instances are reconnected (pool `postgres.pool.min/max`). |
| **Managed**: RDS PostgreSQL (Multi-AZ), Cloud SQL (regional), Yandex Managed PG, Azure Database (zone-redundant) | Prod in the cloud. Failover is managed by the provider. |

**Promote lines - outside Soul Stack** ([ADR-027(k)](../adr/0027-apply-work-queue.md)). Keeper does not manage its database topology - if the PG-primary failover is done by Patroni / managed service, Keeper instances reconnect to a new primary (the pool is recreated). Keeper instances isolated from PG - Watchman ([scaling.md](scaling.md)) will reset their Soul streams → Souls will failback to healthy instances.

### Connection pool

```yaml
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 5, max: 50 }
```

Total flow in PG = `pool.max × N_keeper_instances`. For example, a cluster of 3 instances with `pool.max: 50` potentially holds **150** active connections to the PG. PG `max_connections` should be higher with a reserve for backup tools + third-party clients (minimum `150 × 1.5 = 225`, default PG 100 - too little).

### Table size (estimate)

Calculation for target scale ([mentioned many times in ADR](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) - invariant "100k VM"):

| Table | Lines per installation | Line Size | Volume (approx.) |
|---|---|---|---|
| `souls` | ~ number of managed hosts (1k…100k) | ~200 V | 200 KB … 20 MB |
| `soul_seeds` | ~3-5× lines `souls` (SoulSeed rotation history) | ~200 V | ~ 100 MB per 100k VM |
| `operators` + `rbac_*` | tens of lines | small | <1 MB |
| `service_registry` | tens of lines | small | <1 MB |
| `incarnation` | ~ number of deployed services (tens-hundreds) | ~ 5-50 KB (jsonb `spec`/`state`) | up to hundreds of MB |
| `state_history` | active snapshots × incarnation; **archive_state_history** keeps keep_last_n=50 on incarnation | ~ 5-50 KB | up to GB on large installations |
| `apply_runs` | per-(apply_id, sid) per run; retention 30d (`purge_apply_runs`) | ~500 V | hundreds of MB during active use |
| `apply_task_register` | per-(apply_id, sid, task_idx); retention 1h (`purge_apply_task_register`) | jsonb register_data, variable | 10s-100s MB |
| `audit_log` | all Archon actions + Reaper + push + cloud + soul_grpc; retention 365d (`purge_audit_old`) | ~ 1 KB + jsonb payload | GBs during active use |
| `plugin_sigils` | units to tens of records | ~ 1 KB | <1 MB |

**Disk sizing**: 20-50 GB is enough for a typical installation of up to 10k VM; 100k VM - plan 100-200 GB with a reserve for audit-log for the year.

`audit_log` is the main consumer of volume during growth. Partitioning by `created_at` (BRIN index or declarative partitioning by month) - post-MVP extension ([ADR-022 trade-offs](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)), without breaking.

### Backup / Restore

**Source of truth is all in PG**, so regular backup is critical. Restoring from a backup returns the cluster to the moment of the snapshot.

#### Logical backup (pg_dump)

For compatibility with CI/cold-archive tools:

```sh
PGPASSWORD=$(vault kv get -field=password secret/keeper/postgres-backup) \
pg_dump \
  --host=pg.internal \
  --username=keeper_backup \
  --format=custom \
  --compress=9 \
  --file=/backup/keeper-$(date -u +%Y%m%dT%H%M%SZ).dump \
  keeper
```

- `custom` format (binary, compressed) - faster recovery, supports `pg_restore -j`.
- Separate read-only user `keeper_backup` (`GRANT pg_read_all_data`) - do not use app-user `keeper`.
- **Regularity**: hourly for active installation, daily for static installation.
- **Retention**: 30 days locally + 1 year in cold archive (S3 / GCS / Yandex Object Storage) - corresponds to `audit_log.purge_audit_old.max_age` by default.

#### Physical backup (pgBackRest / barman)

Recommended for production installation - WAL-archive + point-in-time recovery (PITR):

```ini
# /etc/pgbackrest/pgbackrest.conf (sample configuration)
[global]
repo1-path=/var/lib/pgbackrest
repo1-retention-full=7
repo1-retention-archive=7

[keeper]
pg1-host=pg.internal
pg1-port=5432
pg1-database=keeper
pg1-user=postgres
```

```sh
# Full backup once a day
pgbackrest --stanza=keeper backup --type=full
# Incremental - every hour
pgbackrest --stanza=keeper backup --type=incr
```

Advantage - **PITR**: restore to a specific timestamp before the incident (for example, to a random `DELETE` via API). Without PITR, the operator loses everything after the last snapshot.

#### Restore procedure

1. **Stop the entire Keeper cluster** - otherwise the restored PG will be written on top (split state):
   ```sh
   for h in keeper-1 keeper-2 keeper-3; do ssh $h systemctl stop keeper; done
   ```
2. **Restore PG** from the selected backup:
   - Physical (pgBackRest):
     ```sh
     pgbackrest --stanza=keeper --type=time --target="2026-05-26 14:30:00" restore
     ```
   - Boolean (pg_dump):
     ```sh
     dropdb keeper && createdb keeper
     pg_restore -d keeper -j 4 /backup/keeper-2026-05-26.dump
     ```
3. **Clear Redis** - cached presence Souls / Conclave / SID-lease refers to the pre-restore state and may desync:
   ```sh
   redis-cli FLUSHDB
   ```
It's safe - all Redis keys are ephemeral (presence will be restored when reconnecting Souls), see [§ Redis](#redis).
4. **Raise the Keeper cluster** back:
   ```sh
   for h in keeper-1 keeper-2 keeper-3; do ssh $h systemctl start keeper; done
   ```
5. **Souls will reconnect themselves** (failback-loop in `soul.yml::keeper.endpoints`). After reconnection, SID-lease is restored, `souls.status` comes to `connected` via `mark_disconnected`-reconcile (bidirectional, ADR-006 amendment).
6. **Verify**: `keeper_grpc_streams_active` (Prometheus) on each instance should grow to the expected number of Souls. `apply_runs.status = 'running'` lines before recovery - may hang in `dispatched`/`claimed`; recovery-scan Reaper will pick them up (if `reclaim_apply_runs` is enabled) or the administrator will restart them manually.

#### What is **not** restored from PG backup

- **Vault secrets** (JWT signing-key, DSN, SoulSeed CA private, Sigil signing-key) - separate Vault backup (see [§ Vault](#vault)).
- **Redis state** — by design (ephemeral, restored naturally).
- **mTLS material on hosts** (`/etc/keeper/tls/`, `/var/lib/soul-stack/seed/`) - backup via standard file-backup tools or rotate again via Vault PKI after restore.

### Retention and housekeeping

Keeper cleans its tables itself via Reaper rules ([`docs/keeper/reaper.md`](../keeper/reaper.md)):

| Table | Rule | Default `max_age` |
|---|---|---|
| `bootstrap_tokens` (pending) | `expire_pending_seeds` | 24h |
| `bootstrap_tokens` (used) | `purge_used_tokens` | 90d |
| `souls` (disconnected/expired) | `purge_souls` | 30d |
| `soul_seeds` (superseded/expired/revoked) | `purge_old_seeds` | 90d |
| `audit_log` | `purge_audit_old` | 365d |
| `apply_runs` (terminal) | `purge_apply_runs` | 30d |
| `apply_task_register` (after terminal) | `purge_apply_task_register` | 1h (grace) |
| `state_history` (active) | `archive_state_history` (`soft_delete`) | `keep_last_n: 50` on incarnation; migration snapshots are not archived |

Additionally manually (outside of Reaper):

- **`VACUUM ANALYZE`** — autovacuum in PG is enabled by default; for large tables (`audit_log`, `state_history`) during long-term operation, you can run `VACUUM FULL` in the maintenance window for defragmentation.
- **Soft-deleted `state_history` (archived_at IS NOT NULL)** - Keeper does not physically delete them. If space runs out, use a separate bulk unloader in S3 + DELETE (see [ADR-Q19](../architecture.md)).

## Redis

Hot layer and coordination bus of the Keeper cluster. **Ephemeral storage** - all keys are naturally restored when reconnecting Souls and extending lease-renew goroutines.

### Version and mode

| Parameter | Minimum | Recommended |
|---|---|---|
| Redis version | 6.2 | 7.x (improved ACL, additional commands) |
| Topology | single instance + AOF (`redis.mode: standalone`) | Sentinel (recommended HA on-premise) or Cluster (horizontal scaling) - see [§ HA Redis](#ha-redis) |
| Authentication | password via `redis.password_ref` to `keeper.yml` (vault-ref or plaintext) | vault-ref `vault:secret/keeper/redis` (resolved from Vault) + user ACL with minimal command set (post-MVP) |
| Persistence | AOF (everysec) or RDB-snapshot | AOF - superfluous: data is ephemeral, will be restored via reconnect |
| TLS | optional | preferably Redis ⇄ Keeper over TLS, especially cross-DC |

Redis password (`redis.password_ref`) supports both forms (see [config.md → redis](../keeper/config.md#redis)):

- **vault-ref** `vault:secret/keeper/redis[#field]` - resolved from Vault by the keeper-vault-client at start (as `postgres.dsn_ref` / `auth.jwt.signing_key_ref`). Default KV secret field - `password`; the other field is selected with the suffix `#field`. This is the recommended product form.
- **plaintext-string** - works as is (dev/tests without Vault-fixture).
- **empty** - connect to Redis without a password.

Recorded below in Vault KV `secret/keeper/redis` (bootstrap step, rotation table) - **valid link**: just specify `password_ref: vault:secret/keeper/redis` in `keeper.yml`.

### What's in Redis

| Key prefix | What | TTL | When is it updated |
|---|---|---|---|
| `soul:<sid>:lock` | SID-lease (which Keeper instance holds the EventStream of this Soul) | 30s | renew every 10s renew-goroutine handler; goes out at `Release` (graceful) or TTL (crash). |
| `keeper:instance:<kid>` | [Conclave](../adr/0006-cache-redis.md) - presence of Keeper instance | 30s (`DefaultConclaveTTL`) | renew every 10s (`DefaultConclaveRenewInterval`); filmed on graceful-shutdown. |
| `reaper:leader` | Reaper's leadership lease | `reaper.lock_ttl` (default 5m) | renew is re-elected by TTL; one live-Reaper per cluster. |
| `apply:summons` | pub/sub-signal "planned tasks have appeared" ([ADR-027](../adr/0027-apply-work-queue.md)) | without TTL (pub/sub) | Ephemeral signal; the loss is compensated by poll-fallback Acolytes. |
| `events:shard:<n>` | cluster-routing apply/run events (`TaskEvent`/`RunResult`/`ErrandResult`) between Keeper instances ([ADR-006(p.1)](../adr/0006-cache-redis.md), `applybus`) | without TTL (pub/sub) | Ephemeral; fixed set of 256 shards (`n = fnv32a(apply_id) % 256`), bridge per-shard. Event loss is acceptable (fire-and-forget SSE). |
| `sigil:anchors-changed` | pub/sub-signal "trust-anchor-set updated" ([ADR-026(h)](../adr/0026-sigil.md)) | no TTL | Ephemeral; the loss is compensated by TTL-fallback-reread (`sigil_anchors_reload_interval`, default 30s). |
| `rbac:invalidate` | pub/sub-signal "RBAC snapshot updated" ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)) | no TTL | TTL-poll fallback on the `Holder` side. |
| `service:invalidate` | pub/sub-signal "Service registry has been updated" ([ADR-029](../adr/0029-service-registry.md)) | no TTL | TTL-poll fallback. |
| `cancel:<apply_id>` | cluster-wide cancel-run signal ([cluster-wide cancel](../keeper/storage.md)) | short TTL | at `POST /v1/incarnations/{name}/cancel`. |

All keys have a **fallback mechanism** in the code ([ADR-006](../adr/0006-cache-redis.md), [ADR-027](../adr/0027-apply-work-queue.md), [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)): loss of a pub/sub signal is covered by TTL-poll, loss of lease is covered by TTL and re-election.

### Parameters

```redis.conf
# Persistence is lightweight, only needed to restart Redis without losing the lease window.
appendonly yes
appendfsync everysec

# Memory - no hard limit, but it's safe to limit so as not to crowd out the OS:
maxmemory 1gb
maxmemory-policy noeviction
```

**Eviction policy is `noeviction`**, not `allkeys-lru`. Rationale: Each Redis key in Soul Stack has **its own TTL** and **semantic fallback** in the code; eviction LRU would supplant lease mid-life (split-brain Reaper / double handler of one SID). `noeviction` + `maxmemory` is enough with a large margin - the total volume of all keys per 100k VM:

- `soul:<sid>:lock` × 100k = ~10 MB
- `keeper:instance:<kid>` × tens = <1 KB
- `apply:summons` / `events:shard:*` / `cancel:*` - pub/sub, not stored in Redis (broadcast only)
- pub/sub channels - not stored, broadcast only

**1 GB `maxmemory` with plenty of headroom** for target scale.

### HA Redis

The Keeper client supports all three topologies **natively** via `redis.mode` (slot-routing for Cluster, master-discovery for Sentinel - on the client side, without an external proxy). The complete normative scheme of block fields is [config.md → redis](../keeper/config.md#redis).

| Solution | `redis.mode` | Required fields | When |
|---|---|---|---|
| **Single instance** + AOF | `standalone` (default) | `addr` | dev/staging, small installations, one node. |
| **Sentinel** (1 master + N replica + Sentinel quorum) | `sentinel` | `master_name` + `sentinels[]` | **Recommended HA path for typical on-premise** - automatic failover; single-master, simpler and safer than Cluster. |
| **Cluster** (sharded, 3+ master + replicas) | `cluster` | `nodes[]` | Horizontal scaling for a large volume of keys. See the pub/sub cover below. |
| **Managed**: ElastiCache, Cloud Memorystore, Yandex Managed Redis | `standalone` (behind a single endpoint) or `cluster`/`sentinel` in the form of a managed service | by selected `mode` | Prod in the cloud. |

Cluster validated at mega-reception 2026-05-25 (3 keeper + 9-node real redis-cluster): leader-failover Reaper by TTL without split-brain, presence Souls / Conclave, real run scenarios.

!!! note "Recommendation: Sentinel for generic HA"
For typical on-premise HA, choose **Sentinel** rather than Cluster. Sentinel is a single-master with automatic failover: easier to operate, without sharding and slot-rebalance, and removes the caveat about cluster pub/sub (below). Choose Cluster consciously - when the volume of keys has outgrown one master.

!!! warning "Cluster pub/sub: broadcast on very large fleets"
In `cluster` mode Redis-pub/sub is a classic broadcast (the message reaches all nodes in the cluster). This is **correct** (coordination signals get through), but on **very large fleets** broadcast does not remove the well-known bottleneck pub/sub-load. Sharded pub/sub - GA plan. For standard scale and for on-premise-HA via Sentinel, this caveat is not relevant.

### Backup / restore Redis

**Not needed** in a normal operating model. All keys are recoverable:

- SID-lease — renew-goroutine handler; on reconnect Soul, the lease is created anew.
- Conclave - at the start of the Keeper instance, `RegisterInstance` writes presence again.
- Reaper-leader - re-election by TTL.
- pub/sub - ephemeral, loss is compensated by fallback.

**Infrastructure restoration** - raise a new Redis, send the `keeper.yml::redis` block there (`addr` for `standalone`, `sentinels[]`/`nodes[]` for `sentinel`/`cluster`) via hot-reload or systemd reload. Souls continues to work, presence is re-fixed in the new Redis for TTL (~30s).

## Vault

Storage of **all secrets** of the installation: PG DSN, JWT signing-key, mTLS PKI (Keeper-side + SoulSeed), SSH-CA (for `keeper.push` via Vault SSH provider), Sigil signing-key, Essence-secrets of services, credentials of cloud-drivers. **Required dependency** ([requirements.md](../requirements.md), [ADR-014](../adr/0014-operator-identity.md)).

Vault prod configuration (AppRole + persistent backend + auto-unseal + least-privilege policy) is described in detail in [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md). Here is the operational part.

### Engines Keeper

| Engine | Mount | What is stored | Usage |
|---|---|---|---|
| KV (v1/v2) | `secret/` | `keeper/postgres` (DSN), `keeper/jwt-signing-key`, `keeper/redis` (password), `keeper/sigil-signing-key`, `keeper/sigil-keys/<key_id>` (R3 multi-anchor), Essence-service secrets | resolve `vault:` ref in the config and in CEL `vault(...)`. The KV mount version is detected automatically (probe), v1 and v2 work; Provisioning below raises v2 as the recommended default. **Sigil multi-anchor (R3, `sigil-keys/<key_id>`) - list/metadata operations, require KV v2.** |
| PKI | `pki/` (or `pki/soulstack/`) | Root + intermediate CA for SoulSeed mTLS | `Bootstrap`-RPC signs Soul Agent CSR via `pki/sign/<pki_role>` |
| SSH | `ssh/` (optional) | SSH CA for `keeper.push` via `soul-ssh-vault` provider | `keeper.push` requests a signed SSH cert for a specific host before an SSH session |
| Transit | (optional) | JWT signing without key export | post-MVP, see [ADR-014(b)](../adr/0014-operator-identity.md) |

!!! note "TLS to Vault: custom-CA bundle is not parameterized in the config (post-MVP)"
HTTPS to Vault (`vault.addr: https://…`) works **if the Vault server certificate clings to the system trust-store** of the Keeper host (or is passed to the Vault SDK through standard environment variables, e.g. `VAULT_CACERT`). There is currently **no** a separate field for a custom CA-bundle for Vault in `keeper.yml` - the client is built through `vaultapi.DefaultConfig()`. Custom-CA parameterization for Vault - post-MVP; For now, add CA Vault to the host's system trust-store.

### Bootstrap Vault for Keeper

Minimum set of operations - provisioning secrets and engines:

```sh
export VAULT_ADDR=https://vault.internal:8200
export VAULT_TOKEN=<root-or-admin-token>

# KV - the Keeper client works with both v1 and v2 (the version is detected automatically).
# v2 recommended (versioning + metadata; Sigil multi-anchor list/metadata requires v2)
# and in dev-mode it is active by default on mount `secret`.
vault secrets enable -version=2 -path=secret kv 2>/dev/null || true

# Write down mandatory Keeper secrets
vault kv put secret/keeper/postgres \
  dsn="postgres://keeper:<password>@pg.internal:5432/keeper?sslmode=verify-full"
# Redis: password is read from Vault (keeper.yml::redis.password_ref:
# vault:secret/keeper/redis). The default field of the KV secret is `password`.
vault kv put secret/keeper/redis password="<redis-password>"
vault kv put secret/keeper/jwt-signing-key signing_key="$(openssl rand -base64 32)"

# PKI engine for SoulSeed
vault secrets enable -path=pki/soulstack pki
vault secrets tune -max-lease-ttl=87600h pki/soulstack
vault write -field=certificate pki/soulstack/root/generate/internal \
    common_name="Soul Stack SoulSeed CA" ttl=87600h > /tmp/ca.crt
vault write pki/soulstack/roles/soul-seed \
    allowed_domains="example.com,internal" \
    allow_subdomains=true \
    max_ttl=720h

# AppRole for Keeper (cont.)
vault auth enable approle
vault policy write keeper-prod /path/to/vault-policy.hcl   # see examples/keeper/vault-policy.hcl
vault write auth/approle/role/keeper-prod \
    token_policies=keeper-prod \
    secret_id_ttl=720h token_ttl=1h token_max_ttl=24h
vault read auth/approle/role/keeper-prod/role-id           # role_id → keeper.yml
vault write -f auth/approle/role/keeper-prod/secret-id     # secret_id → /etc/keeper/vault-secret-id mode 0400
```

The full policy template with commented paths is [`examples/keeper/vault-policy.hcl`](../../examples/keeper/vault-policy.hcl). AppRole Details - [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md).

### Lightweight Vault for small installations

Vault is a required dependency ([ADR-053](../adr/0053-dependency-tiers.md)), but this **does not mean "a heavy Vault cluster is needed"**. For a small installation, one Vault process with file-storage is enough - operationally this is comparable to using Redis (one binary, local directory for data).

```hcl
# /etc/vault/vault.hcl — single-binary, file-storage
storage "file" { path = "/var/lib/vault/data" }
listener "tcp" {
  address     = "127.0.0.1:8200"
  tls_disable = false              # cont: real TLS, not disable
}
api_addr = "https://vault.internal:8200"
```

First run - `vault operator init` (save unseal-keys and root-token in a safe place), then `vault operator unseal` with the required number of keys. Next are the secrets and engines from the Bootstrap Vault for Keeper section above.

**dev-mode (`vault server -dev`) is not suitable for sale** - it keeps data only in memory and **loses it on every restart** (and also unseals itself and listens via HTTP). Suitable only for local experiments. For sales - **file- or raft-storage + explicit unseal** (in production - [auto-unseal](#backup--restore-vault), otherwise each restart requires manual entry of a quorum of keys).

Details of single-binary-configuration, raft-storage and auto-unseal are in [official Vault documentation](https://developer.hashicorp.com/vault/docs/configuration); prod setting for Keeper - [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md).

### Backup / restore Vault

Vault backs up the **storage backend**, not the binary itself. Depends on the selected backend:

- **Raft** (recommended): `vault operator raft snapshot save /backup/vault-$(date -u +%Y%m%dT%H%M%SZ).snap`. The snapshot includes all KVs, policies, tokens, engines configuration.
- **Consul / etcd**: backup of the corresponding storage according to their tools.
- **Cloud-managed (HCP Vault, AWS Secrets Manager, ...)**: backup is managed by the provider.

**Rotation of unseal-keys / recovery-keys** - according to the organization's security policy, usually annually. See [Vault docs → Key rotation](https://developer.hashicorp.com/vault/docs/concepts/rotation).

**Auto-unseal** ([ADR in prod-setup](../keeper/prod-setup.md)) is required in prod - otherwise, each Vault restart requires manual unseal with a quorum of keys. Cloud KMS (AWS KMS / GCP KMS / Azure Key Vault) or HSM are standard options.

### Rotation of secrets

| Secret | How often | Procedure |
|---|---|---|
| JWT signing-key (`secret/keeper/jwt-signing-key`) | By compromise / by policy (every 6-12 months) | `vault kv put` → `systemctl reload keeper` → reissue of all JWTs (old ones are invalid). See [§ Rotation of signing-key in prod-setup.md](../keeper/prod-setup.md). |
| PG password (`secret/keeper/postgres`) | By policy (once every 90 days) | `ALTER USER keeper PASSWORD …` in PG → `vault kv put secret/keeper/postgres dsn=…` → `systemctl reload keeper` (the pool is recreated with a new DSN). The atomicity window is short-term `connection failed` while the pool is being recreated. |
| Redis password (`secret/keeper/redis`) | By politics | `CONFIG SET requirepass …` in Redis → `vault kv put secret/keeper/redis password=…` (if `redis.password_ref` is vault-ref) → `systemctl reload keeper`. The Redis password resolution from Vault has been implemented - `password_ref: vault:secret/keeper/redis` is valid. |
| SoulSeed (mTLS-cert Soul) | Regularly (TTL `pki/soulstack/roles/soul-seed`, up to 30d) | automatically via live stream ([`docs/soul/onboarding.md`](../soul/onboarding.md)). |
| Vault AppRole `secret_id` | By TTL `secret_id_ttl` (default 720h in our template) | `vault write -f auth/approle/role/keeper-prod/secret-id` → rewrite `/etc/keeper/vault-secret-id` (mode 0400) → `systemctl reload keeper` (or restart - reload-able auth block? see [config.md hot-reload](../keeper/config.md#hot-reload)). |
| Vault root-token | Upon completion of bootstrap | `vault token revoke <root-token>` - after configuration, AppRole is not needed; for plurpose `keeper init` has already worked. |
| Sigil signing-key (`secret/keeper/sigil-signing-key`) | By compromise (rarely) | R3 multi-anchor allows graceful rotation: add a new key → re-broadcast trust-anchors → Retire the old one; See [ADR-026(h)](../adr/0026-sigil.md). |

**Sealed Vault → Keeper fail-closed.** When sealed, Vault Keeper will neither resolve `vault:` ref (DSN/signing-key/passwords) nor release SoulSeed via PKI. Existing sessions (with already resolved secrets and an active PG pool) continue to work; new operations that require Vault fail. This is part of the "safety first" invariant - there is no `KEEPER_ALLOW_VAULT_DOWN` flag and there are no plans to do so.

## See also

- [`docs/keeper/storage.md`](../keeper/storage.md) - complete catalog of Postgres tables + Redis keys.
- [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md) — Vault AppRole, least-privilege policy, persistent + auto-unseal, JWT signing-key rotation.
- [`docs/keeper/reaper.md`](../keeper/reaper.md) — Reaper rules (retention).
- [`docs/architecture.md` → ADR-005 / ADR-006 / ADR-022 / ADR-026 / ADR-029](../architecture.md) - justifications.
- [`examples/keeper/vault-policy.hcl`](../../examples/keeper/vault-policy.hcl) - policy template.
- [`disaster-recovery.md`](disaster-recovery.md) - complete disaster scenarios (PG+Redis+Vault+Keeper).
