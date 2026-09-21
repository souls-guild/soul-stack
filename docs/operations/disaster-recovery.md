# Disaster recovery

Failure scenarios for installation components and recovery procedures. According to the principle "what fails → what is observed → what to do."

Architectural context: Keeper stateless ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)), PG = source of truth ([ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres)), Redis = ephemeral hot layer ([ADR-006](../adr/0006-cache-redis.md)).

## Failure Matrix

| Disclaimer | Severity | Automatic recovery? | Action operator |
|---|---|---|---|
| One of the N Keeper instances | Low | Yes (LB drain, Conclave will update, Souls failback) | Investigate / restart fallen |
| All Keeper instances | High | No (no processes) | Raise one minimum |
| Redis | Average | Partially (presence/lease will be restored upon reconnect) | Raise Redis; existing sessions can continue, new apply sessions will fail in the claim phase |
| Postgres primary (with replica) | Average | Patroni / managed failover makes promote replica | Keeper instances will reconnect to the new primary |
| Postgres without replica | **Critical** | No | Restore from backup |
| Vault sealed / unreachable | High | No (fail-closed) | Raise Vault; existing-sessions are alive, new upadut operations on resolve `vault:` ref |
| A complete disaster (everything died) | Critical | No | Last-known-good restore from backups |

## 1. One Keeper instance has fallen

Symptoms:

- `up{job="keeper",instance="<host>"} == 0` in Prometheus.
- `keeper:instance:<kid>` disappeared from Redis after TTL (~30s).
- LB drain-null backend via health-check fail.
- Souls with streams on this instance received a break → failback to others.

What happened:

- Conclave-presence has gracefully expired or crashed.
- `keeper_reaper_lease_held` for this instance = 0; if he held the lead - re-election through `reaper.lock_ttl` (default 5m).
- Acolyte-claims of this instance remained in the database in the status `claimed`/`dispatched` (if it was in-flight). If `reclaim_apply_runs` is enabled, recovery-scan will relabel `claimed` → `planned` on the next Reaper tick; `dispatched` remains (Soul-reconcile orphanit on reconnect, [ADR-027(g)](../adr/0027-apply-work-queue.md) amend).

Action:

1. Investigate via `journalctl -u keeper -n 200` / Prometheus / OTel-traces.
2. `systemctl start keeper` (if the crash was a one-time crash, systemd `Restart=on-failure` will already restart).
3. After the start, Conclave-presence is written, the instance returns to LB via health-check.
4. Verify `keeper_grpc_streams_active` grows as Souls failback.

## 2. All Keeper instances are unavailable

Symptoms:

- All Souls in `souls.status = 'disconnected'` (via `mark_disconnected` reconcile in PG, will fall behind at `stale_after`).
- Operator API is not available on all `keeper.internal:8080`.
- Prometheus `up{job="keeper"} == 0` everywhere.

What happened:

Possible reasons:
- Network outage in DC.
- Misconfiguration after rolling-upgrade - all instances crashed simultaneously on the new version.
- PG / Redis / Vault outage - all Keepers stopped at retry-loop resolve.
- Hardware incident.

Action:

1. **Diagnose the root cause** before restarting. If they all crashed for the same reason (broken `keeper.yml`, broken Vault), restarting without a fix will not help.
2. Raise **one** instance with a verified version:
   - Check `keeper.yml` locally for syntax. There is **no separate offline validation command** (CLI `keeper` has only `init` / `run` / `version` / `help`; `--check-config` is not implemented, [soulctl](../../soulctl/README.md) keeper config does not validate). Practice: `keeper run --config=/etc/keeper/keeper.yml` on an isolated dev instance - an invalid config crashes at startup with an understandable error; or start with `logging.level=debug` and observe `journalctl -u keeper`.
   - If the version is new, roll back the package to the tested one.
   - `systemctl start keeper` - keep an eye on `journalctl -u keeper -f`.
3. After the start of one instance, Souls will begin to reconnect to it (potential overload - a single instance holds the entire fleet). Raise additional instances as they are ready.
4. After recovery - investigate root cause and patch.

## 3. Redis outage

Symptoms:

- `keeper_reaper_lease_held` = 0 everywhere (leader-lease loss).
- Soul streams can continue to work (TCP is already installed) - but without presence-renewal after ~30s SID-lease will expire, and:
  - `souls.status` through `mark_disconnected` will lag behind (if reconcile with lease-fallback is enabled).
  - Conclave-presence disappears.
  - `apply:summons` pub/sub does not work → new apply will wait for poll-fallback Acolyte (default 2s).
  - `rbac:invalidate` / `service:invalidate` doesn't work → RBAC snapshots / service-registry stale until next TTL-poll.
- `keeper_rbac_invalidations_received_total` rate = 0.

What happened:

Redis is unavailable. Existing in-memory state Keeper instances continue to operate on TTL-poll fallback (RBAC / service-registry), but coordination between instances is lost.

**Acolyte-claim continues to work** - the claim is made through PG `FOR UPDATE SKIP LOCKED`, not through Redis. Simply, without the Summons signal, apply will wait for the poll phase (latency increases).

Action:

1. Raise Redis. If single-node - restore via restart + AOF replay.
2. After Redis-up Keeper instances are automatically re-registered by Conclave, SID-lease is created on the next app message / when reconnecting Souls.
3. Reaper-leader re-elected for `lock_ttl`.

**Backup Redis is not needed** - all keys are naturally recoverable, see [`infra.md` → Backup Redis](infra.md#backup--restore-redis).

## 4. Postgres primary is lost (with replica)

Symptoms:

- Patroni / managed service does automated promote replica.
- Keeper instances receive connection errors for a short time (`keeper_postgres_connection_errors_total`).
- After promote, the pools are recreated and operations are resumed.

Action:

- **If Patroni**: wait for failover to complete (usually 30-60s). Verify via `psql` to new primary.
- **If cloud-managed**: wait for failover (usually 60-120s). Verify via provider console.
- **Watchman can react** to a short PG unavailability → `isolated` → closing streams. Souls failback to the remaining not-watchman instances; after returning PG Watchman removes isolated, Souls return naturally by priority.

After recovery:
- `keeper_postgres_connection_errors_total` rate returns to 0.
- `apply_runs` lines hanging in the claim phase during a failure may lose their lease and be rebranded by the recovery scan (if enabled) - natural behavior.
- `state_history` snapshot during outage time - no (but there should be no runs in this window - Keeper did not respond).

## 5. Postgres primary is lost without replica

Critical Scenario. **Restore procedure:**

1. Stop all Keeper instances.
2. Restore PG from backup (see [`infra.md` → Restore](infra.md)).
3. Clean up Redis (`FLUSHDB`).
4. Raise the Keeper cluster.
5. Souls failback will reconnect.

**Data loss window** = from the last backup to the moment of failure. Minimize via:

- pgBackRest with WAL-archive (PITR accurate to seconds).
- Regular pg_dump (hour / day).

## 6. Vault outage

Symptoms:

- `keeper_vault_read_errors_total{kind="error"}` rate > 0.
- Start of a new Keeper instance - crashed on resolve `postgres.dsn_ref` / `signing_key_ref` / `vault.auth.method=approle` login.
- Already started instances continue to work (in-memory cache of resolved secrets).
- New operations requiring Vault on the fly:
  - `core.vault.kv-read` ([ADR-017](../adr/0017-keeper-side-core.md)) → fail (`ErrVaultKVNotFound` or transport error).
  - CEL `vault(...)` in render → fail script with render-error.
  - Bootstrap of the new Soul → fail (`pki/sign/<role>` is not available).
  - JWT issue for the new Archon → fail.

Action:

1. Raise Vault. If sealed, unseal (auto-unseal prevents this; manual unseal requires a quorum of keys).
2. After up - all operations are resumed. Vault-token renewer ([`docs/keeper/prod-setup.md`](../keeper/prod-setup.md)) will re-request the token.
3. **Verify** - `keeper_vault_read_errors_total` rate returns to 0.

**Sealed Vault - fail-closed by design.** There is no `KEEPER_ALLOW_VAULT_DOWN` flag ("security comes first", [requirements.md](../requirements.md)).

## 7. Complete disaster (PG + Redis + Keeper down)

Hardware incident, DC outage. **Last-known-good restore:**

1. **Raise infrastructure in new DC / on new hosts:**
   - PG from backup (physical pgBackRest is preferable to logical pg_dump - PITR + is faster).
   - Redis - empty, raise (data is ephemeral).
   - Vault from raft-snapshot (see [`infra.md` → Vault backup](infra.md#vault)).

2. **Verify networks** - Keeper hosts see PG / Redis / Vault.

3. **Raise Keeper cluster:**
   ```sh
   for h in keeper-1 keeper-2 keeper-3; do ssh $h systemctl start keeper; done
   ```

4. **Soul-fleet** - will reconnect automatically:
   - SoulSeed certificates in `/etc/soul/seed/` on hosts are alive.
   - `soul.yml::keeper.endpoints` contains the DNS/IP of new Keeper hosts (or old names are resolved to new IPs via DNS).
   - Soul agents themselves retry-request bootstrap → failback by priority → mTLS-handshake to the restored CA.

5. **Verify recovery:**
   - `keeper_grpc_streams_active` total = number of active Souls.
   - `souls.status = 'connected'` in registry `souls` via `mark_disconnected` reconcile (behind `stale_after * 2 = 3min`).
   - `incarnation.status` for all incarnations - in `ready` (if there was no in-flight apply before the disaster; if there was - `applying`/`error_locked`, requires manual triage).
   - SQL: `SELECT count(*) FROM apply_runs WHERE status IN ('planned', 'claimed', 'dispatched')` - must be 0 or close (in-flight apply may freeze during a disaster; see [`faq.md`](faq.md)).

### What is lost in a disaster

- **Apply, which was in process at the time of the disaster** - incarnation remains in `applying` / `error_locked`. The operator restarts the run with a manual action.
- **Audit-events during outage** - no (Keeper did not write). This is natural - there was nothing to audit, Keeper did not respond.
- **State_history snapshot for outage** - no (see above).
- **Live OTel-trace data for outage time** - no (OTel-collector could receive it from surviving Souls, but there are no Keeper-spans).

### RTO / RPO

- **RTO** (recovery time objective): depends on the backup strategy. With pgBackRest PITR - 30 minutes typical.
- **RPO** (recovery point objective): with PITR - seconds to minutes before the moment of disaster; with pg_dump - hour / day.

## 8. Adjustment after recovery

After any crash-recovery, check the status:

### Stuck incarnation

```sql
SELECT name, status, applying_started_at, NOW() - applying_started_at AS stuck_for
FROM incarnation
WHERE status IN ('applying', 'migration_failed', 'error_locked', 'destroying')
ORDER BY applying_started_at;
```

- `applying` older than 15 minutes - owned-by-dead-instance footgun (or valid long run). Cross-check with `apply_runs`.
- `error_locked` - failed run, requires manual resolution (see [ADR-027](../adr/0027-apply-work-queue.md) trade-offs).
- `migration_failed` - failed state_schema migration (see [`upgrade.md` → Rollback state_schema](upgrade.md)).

### Stuck `apply_runs`

```sql
SELECT apply_id, sid, status, claim_at, claim_expires_at, attempt
FROM apply_runs
WHERE status IN ('claimed', 'dispatched')
  AND claim_at < NOW() - INTERVAL '1 hour'
ORDER BY claim_at;
```

- If `reclaim_apply_runs` is enabled, `claimed` will be rebranded automatically.
- `dispatched` - usually closes Soul-reconcile when reconnecting Soul ([ADR-027(g)](../adr/0027-apply-work-queue.md) amend). If Soul also died (post-MVP known-gap), the operator closes it manually:
  ```sql
  UPDATE apply_runs SET status = 'failed', error_summary = 'manual closure after disaster recovery'
  WHERE apply_id = '<ULID>' AND status = 'dispatched';
  ```

### Orphaned Vault keys (if Sigil is used)

If `reap_orphan_vault_keys` is enabled (report-only) - after recovery there may be an increase in `keeper_reaper_rule_purged_total{rule="reap_orphan_vault_keys"}` (orphan detection, not deletion). Investigate via Reaper log; manual removal via `vault delete secret/keeper/sigil-keys/<key_id>` after verifying that the key is indeed orphaned.

## Backup checklist - what should be ready BEFORE a disaster

Without these artifacts, recovery is impossible:

- [ ] PG-backup strategy is configured (pgBackRest or pg_dump cronjob).
- [ ] Vault raft-snapshot is backed up regularly.
- [ ] mTLS material Keeper and CA private - in Vault PKI (restored from Vault-snapshot).
- [ ] Vault unseal-keys / recovery-keys - in a secure location (NOT on the Keeper host).
- [ ] `keeper.yml` / `soul.yml` - in git.
- [ ] DNS records / IP Keeper hosts - documented so that Soul configs can be quickly routed to new hosts.
- [ ] L4-LB config - in git / IaC.
- [ ] Vault AppRole role_id / secret_id - the operator knows where they are (NOT only in Vault).
- [ ] Procedure restore - worked on staging.

## See also

- [`infra.md`](infra.md) — backup / restore PG / Redis / Vault.
- [`docs/architecture.md` → ADR-005 / ADR-006 / ADR-027](../architecture.md) - justifications for fault tolerance.
- [`docs/keeper/reaper.md` → recovery-enable](../keeper/reaper.md) - `reclaim_apply_runs` gate.
- [`faq.md`](faq.md) - typical problems.

## Open questions (runbook)

Checked against code (CLI `keeper`, metrics registry); Below are missing operational amenities that may be needed later:

- **`keeper --check-config`** - separate offline validation subcommand `keeper.yml` **no** (CLI = `init` / `run` / `version` / `help`). Validation practice - `keeper run` on an isolated dev instance (an invalid config crashes at the start), see step 2 above. Candidate for post-MVP convenience.
- **`keeper issue-token`** - there is **no separate subcommand on the manual JWT issue for the existing Archon**. Catastrophic identity recovery - through rotation of signing-key + bootstrap-like process (see [bootstrap-rbac.md → Reset to "single admin"](bootstrap-rbac.md)). Post-MVP candidate.
- **Conclave metrics** - metrics of the form `keeper_conclave_live_count` in the code **no** (Keeper metrics registry - [observability.md](../observability.md)). The viability of instances is monitored by the Redis conclave keys directly and by `journalctl`. Post-MVP candidate.
- **Conclave-deregister on crash** - graceful-shutdown removes the key; crash leaves until TTL. There is **no explicit command `keeper conclave-evict --kid=...` for "I know that the host is definitely dead, don't wait for the TTL" - the operator waits for the TTL to expire or deletes the key from Redis manually. Post-MVP candidate.
