# FAQ - typical problems and triage

The most common operational situations and their quick analysis. Details - according to cross-links in the architectural / regulatory documentation.

## "Souls in `disconnected`, although the processes are alive"

**Symptoms.** In the registry `souls` through the Operator API the host is in `disconnected`, but `systemctl status soul` on the host says `active (running)`, and `soul` logs do not show a stream break.

**Root.** `souls.status` - **lazy snapshot** for Operator API, not presence source ([ADR-006(a)](../adr/0006-cache-redis.md) amendment). The authority of "Soul online" is a live SID-lease in Redis. Snapshot lags behind the fact and must be pulled up by the `mark_disconnected` Reaper rule (bidirectional reconcile, including `disconnected → connected` for the case when the lease is alive and the snapshot is behind).

**Triage:**

1. Check that the `mark_disconnected` rule is **enabled** in `keeper.yml::reaper.rules.mark_disconnected.enabled: true`.
2. Check that the Reaper leader is working at all: `sum(keeper_reaper_lease_held) == 1`. If 0 - no one does a reconcile, the snapshot is patched.
3. Check SID-lease in Redis directly:
   ```sh
   redis-cli EXISTS soul:host-01.example.com:lock
   # (integer) 1 — lease is available, Soul is really online
   ```
4. If there is a lease and a snapshot `disconnected` — wait for the next Reaper cycle (`reaper.interval`, default 1h). For acceleration - `keeper.yml::reaper.interval: 5m` via hot-reload.
5. If there is no lease and the process is alive, Soul is not connected to Keeper (TCP / mTLS / SoulSeed problem). Check Soul logs for connection errors.

See also: [`docs/keeper/reaper.md` → `mark_disconnected`](../keeper/reaper.md), [`docs/architecture.md` → ADR-006 amendment](../adr/0006-cache-redis.md).

## "Apply hangs in `applying` for 30 minutes+"

**Symptoms.** `GET /v1/incarnations/{name}` shows `status: applying`, hangs for a long time. The run seemed to have to end.

**Possible reasons:**

### Case 1: `acolytes: 0` in HA cluster (footgun)

Run created on keeper-A, but Soul agent on keeper-B stream. Keeper-A holds in-memory run-goroutine, waits for `RunResult`; `RunResult` comes to keeper-B (the one who holds EventStream Soul); keeper-B does not know what to do (there is no owner in memory), silently ignores it. incarnation hangs in `applying`.

> **Auto-recovery if the owner of the run is DEAD.** If the keeper owner of the direct `incarnation.run` crashes (and not just silently ignored `RunResult`), the orphaned `applying`-lock removes the Reaper rule `reconcile_orphan_applying` (default-ON, presence-gated, [reaper.md](../keeper/reaper.md)): through ≈`stale_after`+presence check it will return incarnation to `ready` without manual SQL. The manual steps below are only needed when the owner is **alive** (`applying_by_kid` still in Conclave) - for example silent-ignore with `acolytes:0` on a living keeper-B - or when the epoch is unknown (`applying_by_kid IS NULL`, see known-gap in reaper.md).

**Verify via SQL:**

```sql
-- Find strung-up incarnation + owner epoch (migration 082)
SELECT name, applying_since, applying_by_kid, started_by_aid FROM incarnation
WHERE status = 'applying' AND NOW() - applying_since > '15 minutes';

-- Find apply_id and its hosts
SELECT apply_id, sid, status FROM apply_runs
WHERE apply_id = (SELECT apply_id FROM apply_runs ORDER BY created_at DESC LIMIT 1);

-- If status = 'success' for all, this is case 1; incarnation did not close at `acolytes: 0`
```

**Action:**

1. Check that Refuse-guard did not work (`acolytes: 0` + multi-keeper → should have refused to start; if it started, then `allow_unsafe_single_path_multi_keeper: true`).
2. Commit `acolytes: > 0` to `keeper.yml` of all instances - this is not a reload-able change, it requires a restart of the Keeper cluster ([scaling.md](scaling.md)).
3. Close stuck run. Endpoint `POST /v1/incarnations/{name}/unlock` is **not suitable** here - it only removes `error_locked` (`409`, if the status is `applying`, [operator-api/incarnations.md → unlock](../keeper/operator-api/incarnations.md)). For the hanging `applying` live owner there is no API transition - the last resort is direct SQL:
   ```sql
   UPDATE incarnation SET status = 'ready' WHERE name = '<name>' AND status = 'applying';
   ```
(If the owner of the run is dead, there is no need to do this, `reconcile_orphan_applying` will release the lock itself, see sidebar above.)
4. Investigate via audit-log - who posted `allow_unsafe_single_path_multi_keeper` and when.

### Case 2: `claimed`/`dispatched` Ward after instance crash

Acolyte branded the task, the instance died. With `reclaim_apply_runs` enabled - recovery will pick it up. Without - freeze.

**Verify:**

```sql
SELECT apply_id, sid, status, claim_by_kid, claim_at, claim_expires_at, attempt
FROM apply_runs WHERE status IN ('claimed', 'dispatched');
```

**Action:** enable `reclaim_apply_runs` according to the procedure in [`docs/keeper/reaper.md` → recovery-enable](../keeper/reaper.md). Requires fencing-Soul rollout (available in the code) + `acolytes > 0`. Before switching on - architect re-review (see ADR-027 amend).

### Case 3: long run with `serial:` barrier and slow host

If the scenario uses `serial:` - Acolyte brands the entire serial-blok with one worker, maintaining a barrier. One slow host can take out the entire run.

**Verify:** OTel traces (`scenario.run` span from child `apply.run` to each host - find the slow one).

**Action:** optimize the scenario or accept that the run is long-running.

## "check-drift returns `422 ErrConvergeMissing`"

**Symptoms.** `POST /v1/incarnations/{name}/check-drift` returns 422 from `"type": "/errors/converge-missing"`.

**Root.** The service does not support drift detection - there is no `scenario/converge/main.yml` file in the service repo ([ADR-031 Slice B](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)).

**Action:** add `scenario/converge/main.yml` to the service repo, which implements an idempotent check of the current state (a typical destiny-style scenario). After merge + service-ref bump - check-drift will become available.

See [`docs/architecture.md` → ADR-031 Slice B](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile).

## "check-drift returns `422 ErrDriftInputMissing`"

**Symptoms.** `POST /v1/incarnations/{name}/check-drift` returns 422 from `"type": "/errors/drift-input-missing"`.

**Root.** Converge-scenario requires an input parameter that cannot be auto-resolved from `incarnation.state` (there is no such name) and there is no value in the override-body of the request.

**Action:**

- Or pass override in the body of the request:
  ```sh
  curl -X POST .../check-drift -d '{"<param-name>": "<value>"}'
  ```
- Either change converge-scenario so that the input parameter has default or is taken from state with a different name.

## "`Holder.Refresh` errors / RBAC snapshot stale"

**Symptoms.** Alert `time() - keeper_rbac_snapshot_last_success_timestamp_seconds > 300`. Perhaps `keeper_rbac_snapshot_rebuild_errors_total{kind=...}` is growing.

**Root `kind=load`.** DB unavailable / SELECT error in `rbac_*` tables. Investigate PG-connectivity.

**Root `kind=parse`.** Invalid permission in `rbac_role_permissions.permission` (out of sync with directory versions). Investigate latest entries in `rbac_role_permissions`:

```sql
SELECT role_name, permission FROM rbac_role_permissions ORDER BY role_name;
```

Find permission that does not match the grammar ([`docs/keeper/rbac.md` → Format permissions](../keeper/rbac.md)).

**Action:**

- `kind=load`: repair PG.
- `kind=parse`: remove / correct invalid permission via `role.update` (or direct SQL after analysis).

After fix - `keeper_rbac_invalidations_received_total` rate increases (via pub/sub `rbac:invalidate`), snapshot is rebuilt.

## "Vault unreachable - what will fall?"

**Symptoms.** `keeper_vault_read_errors_total{kind="error"}` is growing. Apply starts to fall during the render phase.

**What keeps working:**

- Existing EventStream streams (TCP is already installed, the in-memory cache of resolved secrets is alive).
- Reaper, Conclave, Watchman.
- Operator API on read operations that do not require Vault (`GET /v1/operators`, `GET /v1/incarnations` without `state_history`-inclusion of secrets).

**What will fall:**

- Start of a new Keeper instance - at resolution `postgres.dsn_ref`. If there is an AppRole login, it will not work without Vault.
- Any apply that requires `vault(...)` in CEL or `${ vault:... }` in template is render fail.
- Bootstrap of the new Soul - PKI is not available.
- `operator.issue-token` - JWT signing-key from Vault may already be cached, so it may work; but `auth.jwt.signing_key_ref` resolve has already been done at the start of Keeper - it should work.

**Action:** raise Vault. Existing sessions will survive (token-renewer will interrupt and try to re-issue the token; if Vault returns quickly, users may not notice at all).

## "Reaper does not work - `keeper_reaper_lease_held` is 0 everywhere"

**Symptoms.** Metric 0 on all instances, `apply_runs` / `audit_log` / `souls` are starting to grow.

**Root.** No one is holding Redis-lease `reaper:leader`. Reasons:

1. Redis is not available (see [§ Redis outage](#redis-outage)).
2. `reaper.enabled: false` on all instances.
3. Bug in Reaper-loop (restarting the instance should help).

**Triage:**

1. `redis-cli GET reaper:leader` - should return `<kid>` if someone is the leader.
2. If nil and Redis is alive - investigate `journalctl -u keeper -n 200 | grep -i reaper`.
3. Check that `reaper.enabled: true` is in `keeper.yml`.

**Action:**

- Restart any Keeper instance - the next cycle should take lease (`SET reaper:leader <kid> NX EX <lock_ttl>`).
- If all instances were launched in `reaper.enabled: false` - change via hot-reload.

## "Soul drops from `bootstrap token already used`"

**Symptoms.** When starting Soul Agent for the first time on a new host: `error: bootstrap token already used`.

**Root.** The Bootstrap token is one-time use - after successful onboarding, `bootstrap_tokens.used_at` is set, the repeated request is rejected ([`docs/soul/onboarding.md`](../soul/onboarding.md), [`docs/soul/identity.md`](../soul/identity.md)).

Possible reasons:

1. The token is actually used - the Soul agent has already boarded, file `/etc/soul/seed/soul.crt` exists.
2. Race condition - two processes are trying to run bootstrap in parallel.

**Action:**

1. If `/etc/soul/seed/soul.crt` exists, bootstrap has already passed; delete the file `bootstrap-token` (it is no longer needed) and restart `soul`.
2. If you need to re-onboard, issue a new token via the Operator API:
   ```sh
   curl -X POST https://keeper.internal:8080/v1/souls/host-01.example.com/issue-token \
     -H "Authorization: Bearer $(cat /etc/keeper/archon.jwt)"
   ```

Place a new token in `/etc/soul/bootstrap-token` (mode 0400).

## "Hot-reload `keeper.yml` has fallen"

**Symptoms.** `systemctl reload keeper` worked without an error, but the audit-event `config.reload_failed` appeared, the metric tells us.

**Root.** Invalid config in the new `keeper.yml` (syntax / parser diagnostics / incorrect value in the reload-able block).

**Keeper behavior:** old config snapshot **remains active** ([ADR-021](../adr/0021-hot-reload-config.md)) - Keeper will not break, will continue to work on the old configuration.

**Action:**

1. `journalctl -u keeper -n 50 --no-pager` — find a specific parser error (diagnostic name from [`docs/keeper/config.md`](../keeper/config.md)).
2. Fix `keeper.yml`.
3. `systemctl reload keeper` again. Audit-event `config.reload_succeeded` - verify.

## "Soul stream ends every 30 seconds"

**Symptoms.** `keeper_grpc_streams_active` fluctuates, `soul_eventstream_reconnects_total` rate is high.

**Possible reasons:**

1. **L4-LB resets due to idle timeout.** gRPC keepalive should keep the connection, but if the LB is aggressive, it breaks. Check `timeout server 24h` in haproxy (see [`scaling.md` → L4-LB](scaling.md)).
2. **NAT / firewall** between Soul and Keeper - the state of the NAT tables expires, the connection is terminated.
3. **gRPC keepalive** server/client side misconfigured.

**Triage:**

1. On Soul host - `ss -t -o state established | grep keeper-endpoint-port` shows live TCP connections and timer keepalive.
2. On Keeper - `keeper_grpc_messages_total{direction="from_soul"}` rate - are there any app messages about the stream? If 0, and the stream is still active, Soul only keeps a keepalive, there is no app traffic.

**Action:** configure LB/NAT under gRPC long-running streams (`timeout server 24h`+; NAT keepalive: increase TCP keepalive timeout on hosts).

## "Souls do a lot of reconnects after rolling-restart Keeper"

**Symptoms.** With rolling-restart `soul_eventstream_reconnects_total` rate spikes, then returns to normal.

**This is normal behavior.** Soul agents are redirected via failback to new/different instances. The spike lasts for seconds to minutes. After rolling-upgrade is completed, stream-counts re-distribute.

**If the spike is delayed** (>5 minutes) - investigate:

- Conclave-presence of new instances does not appear in Redis - restart prereqs failed.
- `keeper.yml` of the new instance is invalid - fail-stop at startup.
- L4-LB does not return a new instance to the backend (health-check fails).

See [`upgrade.md` → Rolling upgrade Keeper](upgrade.md#rolling-upgrade-keeper).

## "`incarnation.run` returns `409 incarnation already applying`"

**Symptoms.** Request `POST /v1/incarnations/{name}/run` returns 409.

**Root.** Atomicity of the apply model ([architecture.md → Atomicity and error_locked](../architecture.md)): You cannot run a new apply on an incarnation until the previous one has completed.

**Action:**

1. Check status: `GET /v1/incarnations/{name}` → `status: applying`. If so, wait for completion.
2. If it hangs for a long time, see § Apply hangs in applying.
3. If the status is `error_locked` - investigate the last `state_history.changed_by_aid` and the reason; usually requires a manual solution (see [ADR-027 trade-offs](../adr/0027-apply-work-queue.md)).

## "`POST /v1/operators` returns 409 `would lock out`"

**Symptoms.** Attempting to revoke an Archon returns 409 with `would lock out the cluster`.

**Root.** Invariant [ADR-013(c)](../adr/0013-bootstrap-archon.md) - you cannot delete the last statement with `*`-permission.

**Action:** first create another Archon with `cluster-admin`, then revoke the old one. See [`bootstrap-rbac.md` → Self-lockout protection](bootstrap-rbac.md).

## "Sigil verify failed - the plugin does not start"

**Symptoms.** Apply using community plugin (`soul-mod-*` / `soul-cloud-*` / `soul-ssh-*`) crashes with `sigil verify failed`. Audit-event `plugin.verify_failed`.

**Root.** The plugin's Sigil signature did not match - either the plugin is not allowed through `plugin.allow`, or has been revoked (`revoked_at`), or the SHA-256 of the binary does not match the entry in `plugin_sigils`.

**Action:**

1. Verify entry in `plugin_sigils`:
   ```sql
   SELECT namespace, name, ref, sha256, revoked_at FROM plugin_sigils
   WHERE namespace = 'cloud' AND name = 'soul-cloud-aws' AND revoked_at IS NULL;
   ```
2. Verify SHA-256 of the actual binary:
   ```sh
   sha256sum /var/lib/soul-stack-keeper/plugins/cloud/soul-cloud-aws/<commit_sha>/soul-cloud-aws
   ```
3. If there is a match, verify the trust-anchor set on Soul (re-broadcast may not have reached). See [`docs/observability.md` → keeper_sigil_anchors_last_delivered](../observability.md).
4. If the plugin has been updated and the SHA has changed, you need to explicitly allow the new one via the Operator API (`plugin.allow`).

See [ADR-026](../adr/0026-sigil.md), [`docs/keeper/plugins.md` → Integrity-model](../keeper/plugins.md).

## See also

- [`disaster-recovery.md`](disaster-recovery.md) - scenarios of complete component failure.
- [`monitoring.md`](monitoring.md) - what metrics to look at in each scenario.
- [`docs/architecture.md`](../architecture.md) - justifications for all the described invariants.
- [`docs/keeper/reaper.md`](../keeper/reaper.md) - Reaper rules (including `reclaim_apply_runs`).
- [`docs/keeper/rbac.md`](../keeper/rbac.md) — RBAC.
