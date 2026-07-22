# Reaper

A background task inside the `keeper` binary that cleans the DB of garbage and maintains registry invariants. **Not a separate binary.**

The name **Charon** is **reserved** in case the Reaper's scope expands beyond cleanup (table migrations, moving archival records, GC of cold layers). For now there is one name — Reaper. See [naming-rules.md](../naming-rules.md).

> **Reaper = the cleanup domain, scheduling belongs to Conductor.** Spawning [Cadence](../naming-rules.md) schedules ([ADR-046](../adr/0046-cadence.md)) is **NOT** a Reaper rule. The S0 design of ADR-046 planned the Reaper rule `spawn_due_cadence` (`action: spawn`), but [ADR-048](../adr/0048-conductor.md) (2026-06-02) moved schedule execution into a separate leader-elected subsystem **[Conductor](../naming-rules.md)** with its own lease `conductor:leader` and tick-interval (`cadence_scheduler.interval`, ~15–30s) — the Reaper cleanup domain (`interval` 1h) and the Cadence scheduling domain have a different natural rhythm. Therefore the Reaper `action` stays a cleanup set (`expire`/`delete`/`set_status`/`report`/`soft_delete`) **without `spawn`**, and the list of rules below does not contain `spawn_due_cadence`. When Cadence spawning is implemented (ADR-048 slices C1–C4) — it lives in Conductor, not here.

## Properties

- Lives inside `keeper`. Not a separate binary.
- Works **only on one Keeper instance at a time**: the leader is elected via the Redis lease `reaper:leader` with TTL = `lock_ttl` ([ADR-006 → (d)](../adr/0006-cache-redis.md)).
- A loop by `interval` (default `1h`). Dry-run is supported.
- **Metrics per rule:** see [Metrics](#metrics) below.
- Any rule is disabled pointwise via `enabled: false`, **without redeploying the binary** (config hot-reload is a cross-cutting requirement, see [requirements.md](../requirements.md)).

## Boundary: Postgres (+ read-only Vault metadata), not hosts

The Reaper works **over Postgres**; the only exception is the cross-store rule `reap_orphan_vault_keys` (see [Rules](#rules)), which **only reads** the metadata layer of Vault KV (`list` of names + `created_time`, without reading secret values and without deletion). It does not go to hosts over SSH and does not clean local files. This deliberately keeps the Reaper "at the database", not "at the hosts" — otherwise it would need SSH rights to all Souls, which is bad from a blast-radius standpoint. The Reaper's Vault access is limited to a read-only metadata path of the Sigil signing key set (the Vault policy is in the rule description below).

Host-side cleanup of `/var/lib/soul-stack/{bin,modules}/` is arranged separately:

- **pull mode** — the `soul` daemon cleans the local cache on its own schedule. See [`../soul/modules.md`](../soul/modules.md).
- **push mode** — optional cleanup in the same `keeper.push` SSH session. See [push.md](push.md).

> The blast-radius rationale "Reaper at the database, not at the hosts" and the read-only access to Vault metadata are discussed as a security boundary in [`../security/threat-model.md`](../security/threat-model.md).

### What Reaper does NOT clean (self-cleans via cascade)

- **`oracle_circuit`** (the Oracle circuit-breaker, [ADR-030(a)](../adr/0030-vigil-oracle.md)) — a per-decree fixed-window trip counter. There is **no** separate Reaper rule: the table self-cleans via the FK `decree → decrees(name) ON DELETE CASCADE` — a row lives exactly as long as the Decree lives, and goes away with it.
  - **Known-op: re-enable a failed (tripped) Decree (MVP)** = **delete + recreate**. The circuit-breaker auto-`disable`s a rule that spun into a loop (`decrees.enabled=false`); to bring it back into service with a **clean window**, the operator deletes the Decree (`DELETE /v1/decrees/{name}`) and creates it anew (`POST /v1/decrees`). The cascade on delete carries away the `oracle_circuit` row, so the recreated Decree starts with a `fire_count` from zero (rather than immediately at the threshold). A toggle endpoint (`enabled` without recreation) — a separate effort.

## Configuration

A full example of the `reaper:` block from `keeper.yml`:

```yaml
reaper:
  enabled: true
  interval: 1h          # how often the Reaper wakes up
  dry_run: false        # true — only count, delete nothing (for audit)
  batch_size: 500       # how many records per pass (protection against long transactions)
  lock_ttl: 5m          # TTL of the leader's Redis lease

  rules:
    expire_pending_seeds:
      enabled: true
      max_age: 24h      # a pending bootstrap token with an expired expires_at — delete
      action: delete

    purge_used_tokens:
      enabled: true
      max_age: 90d      # burned/expired bootstrap tokens — delete (audit is kept separately)
      action: delete

    purge_souls:
      enabled: true
      statuses: [disconnected, expired]
      max_age: 30d      # a Soul record with no life for N days — delete
      action: delete

    purge_old_seeds:
      enabled: true
      statuses: [superseded, expired, revoked]
      max_age: 90d      # certificate history older than this — delete
      action: delete

    mark_disconnected:
      enabled: true
      stale_after: 90s  # last_seen_at older than N + no live stream → disconnected
      action: set_status
      target_status: disconnected

    purge_audit_old:
      enabled: true
      max_age: 365d     # audit_log records older than this — delete; alias to keeper.yml → audit.retention_days
      action: delete

    purge_apply_runs:
      enabled: true
      max_age: 30d      # completed apply runs older than this — delete (apply-history retention)
      action: delete

    purge_voyages:
      enabled: true
      max_age: 30d      # completed Voyage runs (history) older than this — delete; the window is ALIGNED with purge_apply_runs (the "voyage → apply_runs" drill)
      action: delete

    purge_push_runs:
      enabled: true
      max_age: 30d      # completed push runs (success/partial_failed/failed/cancelled) older than this — delete; the window is ALIGNED with purge_apply_runs; do NOT confuse with purge_orphan_push_runs (zombies)
      action: delete

    purge_incarnation_archive:
      enabled: true
      max_age: 365d     # archive of destroyed incarnations (incarnation_archive): rows with archived_at older than this — delete; a compliance window
      action: delete

    purge_state_history_archive:
      enabled: true
      max_age: 365d     # archive of the state_history journal of destroyed incarnations (state_history_archive): rows with archived_at older than this — delete; a compliance window
      action: delete

    purge_archived_state_history:
      enabled: true
      max_age: 365d     # soft-deleted rows of the LIVE state_history (archived_at IS NOT NULL) older than this — physically delete; a compliance window
      action: delete

    purge_apply_task_register:
      enabled: true
      max_age: 1h       # register data of a completed run older than the grace — delete (transient run-state)
      action: delete

    reclaim_apply_runs:
      enabled: false    # DISABLED BY DEFAULT — see the warning below
      stale_after: 1m   # a formal lease timeout; NOT part of the predicate
      action: set_status
      target_status: planned

    reclaim_voyages:
      enabled: true     # ENABLED BY DEFAULT (path-defaulting) — works even when the key is ABSENT; see below
      stale_after: 1m   # a formal lease timeout; NOT part of the predicate (the lease is baked into claim_expires_at)
      action: set_status
      target_status: pending

    reconcile_orphan_applying:
      enabled: true     # ENABLED BY DEFAULT (path-defaulting, like reclaim_voyages) — works even when the key is ABSENT; see below
      stale_after: 90s  # PART of the predicate (cutoff = NOW()-stale_after); parity with mark_disconnected
      action: set_status
      target_status: ready

    reap_orphan_vault_keys:
      enabled: false    # DISABLED BY DEFAULT — report-only, requires Vault + list rights
      max_age: 24h      # grace by the age of the Vault secret against the Introduce race
      action: report    # report-only: only counts/meters/logs, deletes NOTHING

    archive_state_history:
      enabled: true
      keep_last_n: 50                 # how many of the newest active snapshots to keep per incarnation
      keep_version_bump_snapshots: true  # never archive snapshots of state_schema migration steps
      action: soft_delete             # soft-delete: mark `archived_at = NOW()`, do NOT physically delete

    scry_background:
      enabled: false                  # DISABLED BY DEFAULT — opt-in, ADR-031 Slice C
      interval: 6h                    # the recommended cadence of the background scan
      max_concurrent_in_flight: 10    # the upper bound of concurrent dry_run runs (cluster-wide)
      min_interval_per_incarnation: 0 # 0 = no lower throttle; ORDER BY last_drift_check_at NULLS FIRST round-robins on its own
      action: report                  # informational: counts → incarnation.last_drift_summary, no deletions

    purge_orphan_ephemeral_tidings:
      enabled: true     # OFF without enabled:true (map-driven, like reclaim_apply_runs — an explicit key is needed)
      max_age: 5m       # grace AFTER the Voyage terminal; PART of the predicate (like purge_apply_task_register)
      action: delete
```

> **`reap_orphan_vault_keys` is disabled by default** — it is a **report-only** cross-store reconcile: the rule finds orphaned Sigil signing private keys in Vault (`secret/keeper/sigil-keys/<key_id>` without a row in the `sigil_signing_keys` registry) and **only** counts/meters/logs them. It **deletes nothing from Vault** and **does not read secret values** (private keys) — it takes only the names (`list`) and `created_time` (metadata). It makes sense to enable it only where Vault is configured and a list-/read-policy is granted on the metadata path of the key set (see the rule description in [Rules](#rules)). With Vault disabled/unconfigured the rule degrades (logs a fail and is skipped), not interfering with the others.

> **Known behavior (alert-noise):** metadata misses of the orphan scan (a secret deleted between `list` and the read of `created_time`) are counted in the common `keeper_vault_read_errors_total{kind=notfound}` — with the rule enabled the dashboard may get slightly noisier; this is expected.

> **`reclaim_apply_runs` is disabled by default and MUST NOT be enabled in prod until attempt-fencing is rolled out to the Soul agents** ([ADR-027(g)](../adr/0027-apply-work-queue.md), Phase 2 / S-P2.2). Recovery returns a stale Ward to `planned`; a live Acolyte re-claims it and sends a **second** `ApplyRequest` to the host. Without a Soul-side guard by `attempt` epoch, the Soul will not cut off the stale duplicate from the former owner — resulting in **two applies on one host**. Set `enabled: true` only after fencing-Soul is fully rolled out. This is exactly the mechanism of the invariant "recovery not in prod before fencing".

> **`reclaim_voyages` is ENABLED by default via path-defaulting** ([ADR-043 §8](../adr/0043-voyage.md), 2026-05-30). This is the **inverse default** relative to `reclaim_apply_runs` (which is map-driven: absence of the key in `reaper.rules` = OFF, requires an explicit `enabled: true`). The mechanism is **path-defaulting in `reaper.dispatch`**: the rule executes when the key `reclaim_voyages` is **absent** from `reaper.rules` **OR** when there is an explicit `enabled: true`; it is disabled **only** by an explicit `reclaim_voyages.enabled: false`. The rule returns an orphaned running Voyage (the owner died before finalization, the lease expired) back to `pending` (attempt++) — another Keeper instance re-claims it and finishes the leg from the saved `current_batch_index`. **Why ON by default:** the target scale is up to 100k Souls, Keeper is a horizontally scalable stateless cluster ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)), where restart/replacement of an instance (deploy, OOM, auto-scale) is a routine regular event → orphaned running Voyages are regular → recovery must not depend on whether the operator wrote the rule into `reaper.rules`. **Why it is safe (unlike `reclaim_apply_runs`):** Voyage finalization goes under a CAS-ownership guard (`voyage.Finalize` writes the terminal `WHERE claimed_by_kid = $2`) — a stale worker that lost the lease catches `ErrLeaseLost`, a double-commit is impossible (**exactly-once at the level of the top-level Voyage**; per-leg apply inherits apply-fencing [ADR-027](../adr/0027-apply-work-queue.md)). Correctness requirement: the worker's renew interval ≪ the lease TTL ([ADR-043 §8(d)](../adr/0043-voyage.md)).

> **Voyage-orphan-lock-release (the re-run seam): a reclaimed VoyageWorker removes the orphaned `incarnation.status='applying'` before re-run** ([ADR-027(l)](../adr/0027-apply-work-queue.md)). When `reclaim_voyages` returned an orphaned `kind=scenario` Voyage to `pending` and another Keeper instance re-claims it, the VoyageWorker before re-running the leg encounters `incarnation.status='applying'` left by the **crashed previous owner** (which died before finalization, the single-winner state-commit `applying`→terminal did not run). The VoyageWorker **removes ITS OWN orphaned `applying`** (only the one belonging to the Voyage it reclaimed) and continues the re-run. **Without this seam a reclaimed Voyage hangs** — the re-run runs into "incarnation already applying" and cannot start the leg, i.e. the `reclaim_voyages` recovery would spin idle (the Voyage returned to `pending`, but is never finished). **This seam is ALWAYS ENABLED together with `reclaim_voyages`** (part of its re-run path), **NOT behind a separate opt-in** — unlike `reclaim_apply_runs` (default-OFF, map-driven, requires an explicit gate). This is by design: `reclaim_voyages` is itself default-ON (path-defaulting, see the block above), and without orphan-lock-release Voyage recovery is inoperative → the seam has no switch of its own.
>
> **The double-apply class of Voyage-orphan-release = the same acceptable class as `reclaim_apply_runs`.** The seam removes `applying` and finishes the leg → the per-leg apply of this Voyage inherits apply-fencing ([ADR-027](../adr/0027-apply-work-queue.md)). On a **network partition** (the still-live but partitioned former owner keeps running its apply while our re-run sends a second) a **double dispatch of the job** may reach the host. Protection from **corruption of `incarnation.state`** comes not from "one apply per host" but from two barriers: (1) **gate-1 attempt-fencing of RunResult** — the former owner's stale `RunResult` is cut off on receipt by `apply_runs.attempt` (the epoch grew on re-claim), a corrupt state commit does not pass in the race; (2) **module idempotency** — a repeat dispatch of the same job to the host must not change the result if the module is written correctly. **The operator must know:** Voyage recovery, being **default-ON**, **already carries this double-apply class in prod** — unlike `reclaim_apply_runs`, where the same class arrives only after an explicit gated enable. Full operator explanation — [recovery-reclaim-apply-runs.md → Voyage-orphan-release](../operations/recovery-reclaim-apply-runs.md).

> **Known limitation: `GET /v1/voyages?status=running` shows temporarily-orphaned runs.** The `status` filter returns the value of the **raw PG column `voyages.status` without a lease overlay**. Therefore an orphaned Voyage (`claim_expires_at < NOW()`, the owner died, but `reclaim_voyages` has not yet picked it up on the nearest tick) falls into the `?status=running` selection — it stays `running` in the DB during the **orphanhood window ≈ `reaper.interval`** (from lease expiry to the nearest reaper tick). This is **by-design, not a bug**: with `reclaim_voyages` default-ON the window is short, no data is lost (the Voyage will be picked up and finished from the saved `current_batch_index`). Unlike [`GET /v1/souls`](../soul/identity.md), where presence is derived via a Redis-lease overlay on top of the `souls.status` snapshot, **the Voyage read path has no lease overlay** — this is a conscious debt. To distinguish live ownership from orphaned, look at `claim_expires_at` in the detail response of [`GET /v1/voyages/{id}`](#) (`claim_expires_at < NOW()` ⇒ the owner is dead, waiting for reclaim; `>= NOW()` ⇒ the lease is alive). A candidate for elimination — an overlay in the read-path projection (following the soul presence-overlay model) on an actual operator request.

> **Known limitation: reclaim of a `command` Voyage → at-least-once semantics of the child Errand on failover.** See [ADR-043 §8(d)](../adr/0043-voyage.md) (recommendation: idempotent command modules `core.cmd.shell` / `core.exec.run`).

> **`reconcile_orphan_applying` is ENABLED by default via path-defaulting** ([ADR-027(m)](../adr/0027-apply-work-queue.md), a recovery-completeness backstop). The default mechanism is **identical to `reclaim_voyages`**: the rule executes when the key `reconcile_orphan_applying` is **absent** from `reaper.rules` **OR** when there is an explicit `enabled: true`; it is disabled **only** by an explicit `reconcile_orphan_applying.enabled: false`. **What it closes:** a direct (standalone, not under a Voyage) `incarnation.run` sets `incarnation.status='applying'` in `lockRun`; if the Keeper owner of the run dies before the terminal — the lock hangs **forever**. The Voyage path is closed by `reclaim_voyages` + Voyage-orphan-lock-release ([ADR-027(l)](../adr/0027-apply-work-queue.md)) via the back-link `voyage_targets.apply_id`, but a direct run has no `voyage_targets` row — the back-link is structurally unreachable; `reclaim_apply_runs` does not reach it either (it reclaims a stale `claimed` Ward in `apply_runs`, whereas the applying-lock is a separate flag on the `incarnation` row). **Two-phase detection:** (1) SQL candidates — stale applying rows (`status='applying' AND applying_since < NOW()-stale_after AND applying_by_kid IS NOT NULL`) via the partial index `incarnation_applying_scan_idx`; (2) presence check — for each candidate `InstanceAlive(applying_by_kid)` in the Conclave: alive → skip (the run is really going), dead → release, presence-check error → fail-safe skip (a Redis flap ⇒ do NOT declare it dead). Release — the idempotent `ReleaseApplyingOrphan` (FENCING-1 no-live-rival + single-winner CAS `applying → ready` inside). **`stale_after` is PART of the predicate** (cutoff = `NOW()-stale_after`, unlike the lease arguments of the reclaim rules), default **90s** (parity with `mark_disconnected` — the same class "the owner has been silent for a long time", the presence check finishes the decision). **Presence-gate = no-op without a live Conclave:** the rule proves the owner's death only via `InstanceAlive`; with Redis unavailable the candidate's presence check fails → fail-safe skip (a live run is not disrupted). Per-row audit `reaper.reconcile_orphan_applying.executed` on each release. **Known-gap:** rows with NULL `applying_by_kid` (legacy/pre-082 or the rerun-last micro-window between the `UnlockForRerun`-tx ↔ the epoch-write-tx) the rule does NOT reclaim — without a presence witness of the owner's death, release is unsafe; such an orphaned lock is removed by the operator manually via `POST /v1/incarnations/{name}/unlock`. **Default-ON is safe:** the presence-gate (`InstanceAlive`=false is mandatory) + FENCING-1 + single-winner CAS do not allow releasing a live lock; residual double-apply (a network partition of the live owner) is the same acceptable class as `reclaim_apply_runs` / Voyage-orphan-release, under the protection of attempt-fencing of `RunResult` + module idempotency. Operator explanation — [recovery-reclaim-apply-runs.md → standalone-orphan reconcile](../operations/recovery-reclaim-apply-runs.md).

### Rule structure

Each value in the `reaper.rules` map is an object with the fields below. The map key (`expire_pending_seeds`, `purge_souls`, …) is the rule name; it simultaneously identifies **which table** the rule works over (the binding is fixed in [Rules](#rules) below). The semantics of `max_age` / `stale_after` (which field the age is counted from) also depend on the table and are normalized per-rule.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | `bool` | `true` | Enable/disable the rule pointwise, without redeploying the binary. |
| `action` | `enum{expire, delete, set_status, report, soft_delete}` | — | What to do with the records that satisfy the condition. `expire` — mark `expired`; `delete` — delete the row; `set_status` — move to `target_status`; `report` — **report-only**: only count/log the finding, change nothing (used by the cross-store reconcile rule `reap_orphan_vault_keys`); `soft_delete` — mark `archived_at = NOW()`, physical deletion is forbidden (used by the rule `archive_state_history`, ADR-Q19 retention). A closed enum; extension — propose-and-wait, not freeform. |
| `max_age` | `duration` | — | The age of a record (from a table field, see [Rules](#rules)), after which the rule fires. Mandatory for `action: expire` / `action: delete`. |
| `stale_after` | `duration` | — | The time since the last `last_seen_at` (or the table equivalent), after which the rule fires. Mandatory for `action: set_status`. |
| `statuses` | `list<enum>` | — | A filter — apply the rule only to records in the specified statuses (optional, see the mandatoriness table below). The allowed values depend on the table (see the cross-link in [Rules](#rules)). |
| `target_status` | `enum` | — | The target status for `action: set_status`. The allowed values depend on the table. |
| `keep_last_n` | `integer` | `50` | Only for `action: soft_delete`. How many of the newest active snapshots to keep per unit (incarnation for `archive_state_history`). Positive; `0` or less is a configuration error (a zero keep = "archive everything"). |
| `keep_version_bump_snapshots` | `bool` | `true` | Only for `action: soft_delete` of the rule `archive_state_history`. `true` — snapshots of state_schema migration steps (`scenario='migration'`) are NEVER archived, regardless of `keep_last_n`; a restorable anchor for schema recovery on an ADR-019 rollback. `false` — the rule archives them on par with regular ones (an explicit operator opt-out). |
| `max_concurrent_in_flight` | `integer` | `10` | Only for the rule `scry_background` (ADR-031 Slice C). A cluster-wide upper bound of concurrent background dry_run runs: `<= 0` mutes the rule without removing `enabled`. |
| `min_interval_per_incarnation` | `duration` | `0` (no throttle) | Only for the rule `scry_background`. The minimum interval between background scans of one incarnation; `0` or empty — no lower bound (the natural round-robin gives ORDER BY `last_drift_check_at NULLS FIRST`). |

**Conditional mandatoriness by `action`:**

| `action` | Mandatory fields | Optional fields |
|---|---|---|
| `expire` | `max_age` | `enabled`, `statuses` |
| `delete` | `max_age` | `enabled`, `statuses` |
| `set_status` | `stale_after`, `target_status` | `enabled`, `statuses` |
| `report` | `max_age` | `enabled` |
| `soft_delete` | — | `enabled`, `keep_last_n`, `keep_version_bump_snapshots` |

The allowed values of `statuses` and `target_status` are **not normalized in this document** — they depend on the table and are defined in [`../soul/identity.md`](../soul/identity.md) (`souls` / `bootstrap_tokens` / `soul_seeds` and their statuses) and [storage.md](storage.md). The `keeper.yml` parser rejects a value not in the enum of a specific rule's table with the error `unknown_status`.

### Rules

| Rule | What it works over | `action` | Mandatory fields | What it does |
|---|---|---|---|---|
| `expire_pending_seeds` | `bootstrap_tokens` (see [storage.md](storage.md), [`../soul/identity.md`](../soul/identity.md)) | `delete` | `max_age` (+ `enabled`, `statuses` opt.) | Deletes unused (`used_at IS NULL`) bootstrap tokens with an expired `expires_at` — older than `max_age` beyond the expiry moment (default 24h). An expired pending token cannot be used (`Burn` rejects it), there is no point keeping it further; the long-term audit of creation lives in `audit_log`. `bootstrap_tokens` has no `status` column, so historically `action: expire` (marking `expired`) is not applicable on this table — the rule's actual semantics is `delete`. |
| `purge_used_tokens` | `bootstrap_tokens` | `delete` | `max_age` (+ `enabled`, `statuses` opt.) | Deletes burned / expired tokens older than `max_age`. The long-term audit is kept separately. |
| `purge_souls` | `souls` | `delete` | `max_age`, `statuses` (+ `enabled` opt.) | Deletes Soul records in `disconnected` / `expired` older than `max_age` (default 30d). |
| `purge_old_seeds` | `soul_seeds` | `delete` | `max_age`, `statuses` (+ `enabled` opt.) | Deletes certificate history in `superseded` / `expired` / `revoked` older than `max_age` (default 90d). |
| `mark_disconnected` | `souls` | `set_status` | `stale_after`, `target_status` (+ `enabled`, `statuses` opt.) | **A bidirectional lease-aware reconcile** of the `souls.status` snapshot ([ADR-006(a)](../adr/0006-cache-redis.md)). (1) `connected` → `disconnected`: `last_seen_at` older than `stale_after` (default 90s) **and** no live Redis SID-lease. (2) `disconnected` → `connected`: a live SID-lease (the Soul is really online — a reconnect grabbed the lease, but the snapshot stayed `disconnected`). Each direction is two-phase: select PG candidates, verify against the lease `soul:<sid>:lock`, mark. Thus an idle Soul (PG `last_seen_at` stale, but the stream is alive) is not falsely marked disconnected, **and** the snapshot is not latched into `disconnected` after the first "break+sweep" (a reconnect fixes the snapshot via reconcile, since eventstream presence is not written to PG, and a Bootstrap-RPC of an already-onboarded Soul does not fire). Without configured Redis (single-instance dev) — a fallback to the pure-SQL **one-directional** `mark_disconnected` (migration 014), where a stale `last_seen_at` ⇔ no stream by construction (one instance), and there is no latch. |
| `purge_audit_old` | `audit_log` ([storage.md → `audit_log` table](storage.md#audit_log-table), [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) | `delete` | `max_age` (+ `enabled` opt.) | Deletes `audit_log` records older than `max_age` (default 365d, counted from `created_at`). `max_age` is an alias to `keeper.yml → audit.retention_days` ([config.md → audit](config.md#audit)); on a value mismatch the `keeper.yml` parser rejects the config with `audit_retention_mismatch`. |
| `purge_apply_runs` | `apply_runs` (migration 018, the registry of the scenario-runner's apply runs) | `delete` | `max_age` (+ `enabled` opt.) | Deletes **completed** apply runs (`status` ∈ `success` / `failed` / `cancelled` and `finished_at IS NOT NULL`) older than `max_age` (default 30d, counted from `finished_at`). Runs in `running` are **never deleted** — these are in-progress/hung runs, their triage is a separate mechanism. |
| `purge_voyages` | `voyages` ([ADR-043 §8](../adr/0043-voyage.md), migration 059; the SQL function `purge_voyages` — migration 075, [ADR-046 §79](../adr/0046-cadence.md)) | `delete` | `max_age` (+ `enabled` opt.) | **Retention of the growing Voyage run history.** Deletes **completed** Voyages (`status` ∈ `succeeded` / `failed` / `partial_failed` / `cancelled` and `finished_at IS NOT NULL`) older than `max_age` (default **30d**, counted from `finished_at`). `voyages` is the only run-history table without its own retention: every manual run and every Cadence spawn adds a row, growth without a ceiling. Runs in `scheduled` / `pending` / `running` are **never deleted** — these are unfinished/in-progress runs (their terminal is set by VoyageWorker.Finalize or `reclaim_voyages`, not the Reaper). **Do not confuse with active Cadence schedules:** the rule cleans only the Voyage history, it does **not** touch the `cadences` / `incarnation_choirs` tables (the active declared topology) — deleting them would be data corruption and remains an operator action ([ADR-046 §9](../adr/0046-cadence.md)). **Cascade:** `voyage_targets` (the run's Leg rows) are carried away by `ON DELETE CASCADE` (migration 059). The soft links `voyage_targets.apply_id` / `errand_id` (to `apply_runs` / `errands`) and `tidings.voyage_id` (ephemeral) are **NOT** FKs to `voyages`; purge does not delete them and does not leave broken references (apply_runs is cleaned by `purge_apply_runs`, errands by `purge_old_errands`, ephemeral Tidings are removed earlier by the rule `purge_orphan_ephemeral_tidings`). `voyages.cadence_id → cadences ON DELETE SET NULL` (migration 066): purge of the children does not touch the schedule. **Correlation invariant:** the default window (30d) is **aligned with `purge_apply_runs`** so that the "voyage → its apply_runs" drill does not lose one of the sides (a voyage is deleted while its apply_runs are still needed for correlation — or vice versa); changing one window without the other is a desync. The SQL function parameter is named `batch_limit` (not `batch_size`) because the column `voyages.batch_size` (the run's batch size) would give `ambiguous` in `LIMIT`. |
| `purge_push_runs` | `push_runs` (migration 051; the SQL function `purge_push_runs` — migration 076, [push.md](push.md)) | `delete` | `max_age` (+ `enabled` opt.) | **Retention of the growing run history of push runs.** Deletes **completed** push runs (`status` ∈ `success` / `partial_failed` / `failed` / `cancelled` and `finished_at IS NOT NULL`) older than `max_age` (default **30d**, counted from `finished_at`). `push_runs` is a run-history table of the same class as `apply_runs` / `voyages`: every `keeper.push` run adds a row, growth without a ceiling. Runs in `pending` / `running` are **never deleted** — they are terminalized by executeAsync or (if a zombie) by the rule `purge_orphan_push_runs`. **Do not confuse with `purge_orphan_push_runs`** (TTL 1h, `set_status`/`cancelled` for hung in-flight) — this rule removes already **completed** history, that one — terminalizes orphaned active ones. **No cascade:** per-host results lie inline in `push_runs.summary` (jsonb), there are no child FKs to `push_runs` (migration 051). **Correlation invariant:** the default window (30d) is **aligned with `purge_apply_runs`** so that the "push-run → its per-host summary" drill does not lose the tail of run-history tables of one class; changing one window without the other is a desync. |
| `purge_incarnation_archive` | `incarnation_archive` (migration 039; the SQL function — migration 077) | `delete` | `max_age` (+ `enabled` opt.) | **Retention of the archive of destroyed incarnations.** Deletes `incarnation_archive` rows with `archived_at` older than `max_age` (default **365d**, counted from `archived_at`). The archive is a historical-compliance snapshot of the incarnation snapshot at the moment of destroy (what was there before deletion), so the default window is **deliberately more conservative** than the run-history windows (30d): a year of retention for audit. There are no child FKs to the archive (migration 039) — no cascade. The operator tunes `max_age` to their compliance requirements. |
| `purge_state_history_archive` | `state_history_archive` (migration 039; the SQL function — migration 077) | `delete` | `max_age` (+ `enabled` opt.) | **Retention of the archive of the state_history journal of destroyed incarnations.** Deletes `state_history_archive` rows with `archived_at` older than `max_age` (default **365d**, counted from `archived_at`). Parity with `purge_incarnation_archive` — the same compliance window of 365d; both tables (`incarnation_archive` + `state_history_archive`) are written in one transaction on destroy. There are no child FKs. **Do not confuse with `purge_archived_state_history`** (the live `state_history`, not the archive table). |
| `purge_archived_state_history` | `state_history` (migration 006; the `archived_at` column from 048; the SQL function — migration 077) | `delete` | `max_age` (+ `enabled` opt.) | **Physical removal of soft-deleted snapshots from the LIVE `state_history`.** Deletes `state_history` rows with `archived_at IS NOT NULL` (marked earlier by the rule `archive_state_history`) older than `max_age` (default **365d**, counted from `archived_at`). **Do not confuse with `archive_state_history`** (migration 049): that rule **only sets** the soft-delete flag (`archived_at = NOW()`) beyond the `keep_last_n` latest snapshots, **this one** — finally removes the already-marked ones after the compliance window expires, offloading the live table. Active snapshots (`archived_at IS NULL`) are **not touched**. Thus soft-delete (archive) and physical removal (this purge) are spread out in time: the operator can export the archival snapshots before their physical deletion. |
| `purge_apply_task_register` | `apply_task_register` (migration 022, the accumulator of the run tasks' register data) | `delete` | `max_age` (+ `enabled` opt.) | Deletes register rows of runs in a **terminal** status (`apply_runs.status` ∈ `success` / `failed` / `cancelled` and `finished_at IS NOT NULL`) older than `max_age` (default **1h**, counted from `finished_at`). Here `max_age` semantically = **grace** after the run's completion: register is needed by the scenario-runner only up to the cross-host barrier, beyond which it is transient plaintext (potentially with secrets). The register of an **active** (`running`) run is **never deleted** — the criterion "terminal + grace", rather than a TTL by `created_at`, guarantees this independently of the run duration. See also: the FK `ON DELETE CASCADE` cleans the register cascade-wise together with the apply_run itself (the rule `purge_apply_runs`, 30d) — this rule removes the register **earlier**, shortening the plaintext-storage window. |
| `reap_orphan_vault_keys` | Vault KV `secret/keeper/sigil-keys/<key_id>` ↔ the registry `sigil_signing_keys` (migration 037, ADR-026(h)) | `report` | `max_age` (+ `enabled` opt.) | **Report-only cross-store reconcile** ([ADR-026(h)](../adr/0026-sigil.md)). Finds **orphaned** Sigil signing private keys — the secrets `secret/keeper/sigil-keys/<key_id>` in Vault, for which there is **no row** in `sigil_signing_keys` in any status (**both `active` and `retired` count as alive** — a retired private key is needed to verify previously signed Sigils). An orphan arises, for example, when `Introduce` wrote the private key to Vault, but the PG insert after it failed: the keyservice deliberately does **not** do reverse-cleanup. The rule **only counts/meters/logs** the finding — **deletes nothing** and **does not read secret values** (private keys): it takes only the names (`list`) + `created_time` (metadata). Here `max_age` semantically = **grace** by the age of the Vault secret (default **24h**, counted from `created_time`): it cuts off the race with `Introduce`, where the Vault write outruns the PG commit — a fresh secret may still get a row in the registry, so anything younger than the grace is **not written** into orphans. `batch_size` limits the number of metadata round-trips per pass. Requires configured Vault and the Vault policy `path "secret/metadata/keeper/sigil-keys/*" { capabilities = ["list", "read"] }` — **without `delete`, without reading the data path** `secret/data/...`. In the absence of a Vault client the rule degrades (logs a fail, the pass continues). Disabled by default (see the warning in [Configuration](#configuration)). The metric `keeper_reaper_rule_purged_total{rule="reap_orphan_vault_keys"}` = the **number of detected orphans** (for report-only, "purged" = "detected", nothing deleted). |
| `scry_background` | `incarnation` (migration 050: the `last_drift_check_at` / `last_drift_summary` columns) + `apply_runs` (the work-queue dry_run) | `report` | `max_concurrent_in_flight` (opt., default 10), `min_interval_per_incarnation` (opt., default 0) | **Background periodic drift scanning** ([ADR-031](../architecture.md) Slice C, pilot). The iterator selects incarnations in the statuses `ready`/`drift` without an active apply run (`NOT IN apply_runs WHERE finished_at IS NULL`) and in the order `last_drift_check_at NULLS FIRST` (new ones scanned first). For each: a short FOR UPDATE-tx checks that the status is still `ready`/`drift` (protection against a race with an operator Run), then calls the same `scenario.Runner.CheckDrift` as on-demand Slice B (`POST /v1/incarnations/{name}/check-drift`): dispatches dry_run `converge` on all hosts via the work-queue (the Acolyte renders and sends `ApplyRequest{dry_run:true}`, the Soul calls `mod.Plan`), waits for the barrier, collects a DriftReport. **Counts-only in the background**: the full DriftReport is NOT saved — only the counts aggregates are written to `incarnation.last_drift_summary` (`{hosts_drifted, hosts_clean, hosts_unsupported, hosts_failed, total_hosts, scanned_at}`) and `last_drift_check_at = NOW()`; the full on-demand report from Slice B is returned directly in the response. `max_concurrent_in_flight` limits the concurrent background dry_run runs cluster-wide (the counter — `SELECT count(*) FROM apply_runs WHERE recipe->>'dry_run'='true' AND finished_at IS NULL`). `min_interval_per_incarnation > 0` additionally cuts off a repeat scan of the same incarnation before its time. The audit event `incarnation.drift_checked` is written with `source: background`, `archon_aid: NULL`. **Default OFF — opt-in**: the rule is absent from the base config, the operator enables it explicitly. No need to keep it together with Slice B as a dependency: both use the same pipeline, but independently. |
| `archive_state_history` | `state_history` (migration 006, the `archived_at` column from 048) | `soft_delete` | `keep_last_n` (opt., default 50), `keep_version_bump_snapshots` (opt., default true) | **Soft-delete retention of the state_history journal** ([ADR-Q19](../architecture.md), PM-decision 2026-05). Marks `archived_at = NOW()` on active `state_history` snapshots beyond the `keep_last_n` latest per incarnation (by `at DESC`); physical deletion is forbidden — soft-deleted snapshots remain in the table (optionally for an external bulk exporter). With `keep_version_bump_snapshots: true` (default) snapshots of state_schema migration steps (`scenario='migration'`) are NEVER archived — a restorable anchor for schema recovery on an ADR-019 rollback. `batch_size` limits the number marked per pass; drain to 0 — a sequence of loop passes. The SQL function `archive_state_history(integer, boolean, integer)` (migration 049). Reading history via [`HistorySelectByName`] excludes soft-deleted by default (`archived_at IS NULL`); the Operator API can enable inclusion of archival snapshots via the flag `include_archived=true`. |
| `reclaim_apply_runs` | `apply_runs` (migration 025, the Ward-claim columns) | `set_status` | `stale_after`, `target_status` (+ `enabled` opt.) | **Recovery scan of stale Wards** ([ADR-027(i)](../adr/0027-apply-work-queue.md), Phase 2). Returns zombie claims — the jobs of a dead Acolyte (`status` ∈ `claimed` / `running` with an expired `claim_expires_at < NOW()`) — back to `planned` for re-claim, resetting `claim_by_kid` / `claim_at` / `claim_expires_at`. **`attempt` is NOT reset** — the fencing epoch is incremented by the next claim, and the Soul-guard cuts off a stale `ApplyRequest`. **Closes the "hung applying" hole** (a dead owner does not finalize the run). Uses the partial index `apply_runs_claim_scan_idx`. `stale_after`/`target_status` are formal fields of the action schema; they are **not part** of the SQL predicate (recovery compares `claim_expires_at < NOW()` directly, the actual lease is baked into `claim_expires_at` at Ward capture). `target_status` is always `planned`. **Enabled ONLY with attempt-fencing on the Soul agents** (see the warning in [Configuration](#configuration)), otherwise it will double the apply. |
| `reclaim_voyages` | `voyages` ([ADR-043 §8](../adr/0043-voyage.md), migration 059 — `voyages_claim_scan_idx`) | `set_status` | `stale_after`, `target_status` (+ `enabled` opt.) | **Recovery scan of stale Voyage claims** ([ADR-043 §8 item 7](../adr/0043-voyage.md)). Returns an orphaned Voyage with an expired lease (`status='running' AND claim_expires_at < NOW()`) back to `pending` for re-claim by another Keeper instance, resetting `claimed_by_kid` / `last_renewed_at` / `claim_expires_at`. **`attempt` is incremented** (fencing-epoch parity with `reclaim_apply_runs`); `current_batch_index` is NOT touched — Keeper-B on pickup reads progress from this field and continues from the same Leg. Return to `pending` (NOT to the original `scheduled`): by the time it was `running`, `schedule_at` had certainly arrived, the row must be immediately pickable. Uses the partial index `voyages_claim_scan_idx` (`WHERE status='running'`) + `FOR UPDATE SKIP LOCKED` (protection against a race with a concurrent claim/renew). `stale_after`/`target_status` are formal fields of the action schema; they are **not part** of the SQL predicate (the actual lease is baked into `claim_expires_at` at capture via voyage.ClaimNext; the predicate compares `claim_expires_at < NOW()` directly). `target_status` is always `pending`. Audit event `voyage.reclaimed { voyage_id, last_renewed_at, attempt_after }` per-row (kind-agnostic — a single event for `scenario`/`command` Voyages). **Enabled by default via path-defaulting** (the inverse default relative to `reclaim_apply_runs`; works when the key is absent, disabled only by an explicit `enabled: false`) — see the warning in [Configuration](#configuration). A double-commit is cut off by the CAS-ownership guard `voyage.Finalize` (`WHERE claimed_by_kid=$2` → `ErrLeaseLost` for a stale worker; exactly-once on the top-level Voyage). |
| `reconcile_orphan_applying` | `incarnation` (migration 082, the applying-epoch columns `applying_apply_id`/`applying_attempt`/`applying_by_kid`/`applying_since` + the partial index `incarnation_applying_scan_idx`) | `set_status` | `stale_after`, `target_status` (+ `enabled` opt.) | **Release of an orphaned `incarnation.status='applying'` lock of a direct (standalone) scenario run** ([ADR-027(m)](../adr/0027-apply-work-queue.md)). Closes the seam symmetric to Voyage-orphan-lock-release ([ADR-027(l)](../adr/0027-apply-work-queue.md)), but for a direct `incarnation.run` (which has no back-link `voyage_targets`). **Two-phase:** (1) SQL candidates — stale applying rows (`status='applying' AND applying_since < NOW()-stale_after AND applying_by_kid IS NOT NULL`) via the partial index `incarnation_applying_scan_idx`; (2) presence — for each `InstanceAlive(applying_by_kid)` in the Conclave: alive → skip, dead → release via the idempotent `ReleaseApplyingOrphan` (FENCING-1 no-live-rival + single-winner CAS `applying → ready`), presence-check error → fail-safe skip. **`stale_after` is PART of the SQL predicate** (cutoff = `NOW()-stale_after`, unlike the lease arguments of the reclaim rules), default **90s** (parity with `mark_disconnected`). `target_status` is always `ready`. Audit event `reaper.reconcile_orphan_applying.executed` per-row (`{incarnation, prev_kid, apply_id}`). **Enabled by default via path-defaulting** (like `reclaim_voyages`; disabled only by an explicit `enabled: false`) — see the warning in [Configuration](#configuration). **Known-gap:** NULL `applying_by_kid` (legacy/pre-082, the rerun-last micro-window) is NOT reclaimed — a manual `unlock`. **Presence-gate = no-op without a live Conclave** (no live Redis ⇒ the candidate's presence check fails ⇒ fail-safe skip of the candidate, not a no-op of the whole rule). |
| `purge_old_errands` | `errands` (migration 052, [ADR-033](../adr/0033-errand.md)) | `delete` | `max_age` (+ `enabled` opt.) | `DELETE FROM errands WHERE ttl_at < NOW()`. Default TTL **7d** (`errands.ttl_at = started_at + 7d`); the field is baked in by the dispatcher on INSERT (`errand.TTLDefault`). The rule's `max_age` parameter is **not part** of the SQL predicate — the TTL is baked into the row, the argument is kept for compatibility with the common duration-runner and as a documented override for future migrations of the ttl logic. The index `errands_ttl_idx` (migration 052) makes the condition cheap-scanable. |
| `purge_orphan_ephemeral_tidings` | `tidings` (migration 072 — the partial index `tidings_ephemeral_voyage_idx` `WHERE ephemeral`, [ADR-052(g)](../adr/0052-herald-notifications.md)) | `delete` | `max_age` (+ `enabled` opt.) | **Removal of orphaned ephemeral Tidings** ([ADR-052(g)](../adr/0052-herald-notifications.md) amendment N2). Deletes **one-shot** rules (`ephemeral=true`) whose Voyage is either **in a terminal** (`status` ∈ `succeeded`/`failed`/`partial_failed`/`cancelled` and `finished_at` older than the grace) or **does not exist** (the `voyages` row is deleted). Here `max_age` semantically = **grace AFTER the terminal** of the Voyage and **is part** of the SQL predicate (parity with `purge_apply_task_register`: max_age-as-grace), default **5m**. The grace is a correctness condition, not cosmetics: the dispatcher matches a terminal event against an ephemeral rule asynchronously (a tap-consumer through a bounded channel), removal before the tap-consumer's window would delete the rule **before** the run-completion notification goes out → the terminal notification would be lost. One `DELETE` in one statement via the partial index (there are few ephemeral rules — dozens per in-flight runs; permanent rules do not fall into the scan), `batch_size` does not apply. Permanent Tidings (`ephemeral=false`) the rule does **not** touch. The default semantics is **OFF without `enabled: true`** (map-driven, like `reclaim_apply_runs` — not the path-defaulting of `reclaim_voyages`); in the absence of a wired-up herald stack the rule degrades (warn + skip), not interfering with the others. |

The exact thresholds (`max_age`, `stale_after`) are tuned to the installation via hot-reload.

`mark_disconnected` reconciles the `souls.status` snapshot with the fact of the Redis SID-lease **in both directions**; each direction is two-phase (lease-aware, [ADR-006(a)](../adr/0006-cache-redis.md)).

**The disconnect direction** (`connected` → `disconnected`):

1. **Candidates by PG.** The SQL function `select_disconnect_candidates(stale_after, batch)` (migration 043) selects `connected` souls with `last_seen_at` older than `stale_after`. A throttled flush from the EventStream handler (no more than once per `stale_after / 3`, see [storage.md → Redis — hot layer and coordination](storage.md#redis--hot-layer-and-coordination)) keeps `last_seen_at` fresh while traffic goes over the stream — this is the first barrier.
2. **Verification against the Redis SID-lease.** For each candidate the Purger checks for a live lease `soul:<sid>:lock` ([`SoulStreamAlive`](storage.md#redis--hot-layer-and-coordination)). The lease is held by the handler's renewal goroutine while the stream is alive, and disappears on a normal Release or by TTL after a crash. A candidate with a live lease is **excluded** — this closes the hole with an **idle Soul**: a host that sends only soulprint once per `refresh_interval` could have a stale `last_seen_at` within `stale_after` (not a single app message in the window), but its stream is alive — and it would be falsely marked disconnected without the lease check.

Those that survived both phases (a stale `last_seen_at` **and** no lease) are marked `disconnected` via `mark_disconnected_sids(text[])` (migration 043).

**The reconnect direction** (`disconnected` → `connected`):

1. **Candidates by PG.** The SQL function `select_reconnect_candidates(batch)` (migration 043) selects `disconnected` souls of **any** `last_seen_at` — without a duration predicate: onlineness is decided by a live lease, not by the freshness of the PG snapshot (an idle Soul on a live stream holds the lease, but `last_seen_at` may have gone stale).
2. **Verification against the Redis SID-lease.** A candidate with a **live** lease is really online (a reconnect grabbed the lease, but the snapshot stayed `disconnected`) → return to `connected` via `mark_connected_sids(text[])` (migration 043, the guard `status='disconnected'` protects `revoked`/`destroyed` from being overwritten). Without this direction the snapshot would latch into `disconnected` forever after the first sweep: a reconnect raises the lease, but no one would move the row (eventstream presence is not written to PG, a Bootstrap-RPC of an already-onboarded Soul does not fire).

An error of the Redis check of a specific SID is fail-safe in **both** directions: the Soul is **not** marked either `disconnected` or `connected` (a live stream is more important than the timeliness of the snapshot; the next pass will retry).

**Presence (online/offline) is derived from the Redis SID-lease, NOT from `souls.status`** ([ADR-006(a)](../adr/0006-cache-redis.md)). The authority of "Soul online" is a live lease `soul:<sid>:lock`; it is exactly what the [target resolver](../scenario/orchestration.md) reads (two-phase: SQL candidates by Coven + non-terminal/non-onboarding status → filtering by a live lease). There is **no** synchronous write of presence to PG on connect/disconnect of the EventStream (it would be a hot-path on 100k VMs).

Therefore `mark_disconnected` is now **a lazy bidirectional reconciliation of the PG snapshot `souls.status`** (for the Operator API "last known"), and **not a source of presence**. The `souls.status` snapshot lags the fact (online is already visible to the resolver via the lease, while the row is still `disconnected`) — this is acceptable: the rule brings the snapshot to the fact in the background in both directions, the run does not depend on it. Without configured Redis (single-instance dev / unit) the rule degrades to the former pure-SQL **one-directional** `mark_disconnected` (migration 014), where a stale PG `last_seen_at` ⇔ no live stream by construction (one instance) and there is no `disconnected` latch — a reconnect immediately makes `last_seen_at` fresh; there the resolver also derives presence from the SQL snapshot (`status='connected'`). A normal teardown no longer writes PG presence (the lease goes out on Release/TTL — see [`../soul/identity.md` → Soul statuses](../soul/identity.md)).

The "Mandatory fields" column in this table is actual usage, not a separate norm: for `purge_souls` / `purge_old_seeds` `statuses` is mandatory because without a status filter `delete` would remove live records. The rule-grammar-level norm is the "Conditional mandatoriness by `action`" table above.

## Enabling recovery (recovery-enable)

> **Operational guide:** [`docs/operations/recovery-reclaim-apply-runs.md`](../operations/recovery-reclaim-apply-runs.md) — a step-by-step runbook (prod gates, hot-reload, metric validation, rollback). This section is the normative fixing of the gate; the runbook applies it in prod.

`reclaim_apply_runs` is **disabled** by default and cannot simply be flipped to `enabled: true` — it is an operational step under a gate ([ADR-027(i)](../adr/0027-apply-work-queue.md) + the amend "GATE-1: deliver-once recovery", 2026-05-25). After GATE-1 the gate is **softened**: the lease invariant is relaxed, an Acolyte lease-renew for the recovery boundary is no longer needed. **Validated at scale** by the mega-acceptance of 2026-05-25 (3 keeper + a 9-node real redis-cluster, `acolytes: 2`, `reclaim_apply_runs` enabled, fencing-Soul rolled out): no double apply, leader-failover works (re-election of the Reaper leader by TTL without split-brain), recovery returns stale Wards. That is, the gate condition "fencing rolled out" (step 1) is satisfied by deploying the current `soul` build (fencing is already in the binary). Safe enabling — step by step:

> ⚠️ **WARN: do NOT enable `reclaim_apply_runs` with `acolytes: 0`.** This is a **coupled pair**: `reclaim_apply_runs` and `acolytes > 0`. With `acolytes: 0` (the prod default, Acolyte is opt-in) the jobs are executed by the old synchronous path and written directly to `running`; on this non-fenced path reclaim on its own is **unsafe** — a re-claim without epoch protection will double the apply. Narrowing reclaim to `claimed` means the Reaper anyway does **not** recover `running` rows of the old path (this is not a regression: recovery of the old path is the in-memory run-goroutine of the owner instance, not a Reaper reclaim). Enabling reclaim makes sense **only** together with a raised Acolyte pool (`acolytes > 0`) and a rolled-out fencing-Soul. This is a known-gap from amend (e)/(f).

**Step 1. Roll out fencing-Soul across all Souls.** All `soul` agents must carry attempt-fencing (gate-1 attempt-fencing — already in the code: a Soul-guard by `ApplyRequest.attempt` on execution + an echo `RunResult.attempt` for the epoch-check on receipt). Without this, a re-claim of a stale Ward will send a **second** `ApplyRequest` to the host, and a non-fenced Soul will not cut off the stale duplicate from the former owner → two applies on one host. Roll out the updated `soul` binary to all Souls **before** enabling the rule.

**Step 2. Make sure `acolyte_lease > max-RENDER`.** After GATE-1, reclaim takes only a Ward in the `claimed` phase (render/claim, **before** handoff to the host); a live long apply sits in `dispatched`, which reclaim does not touch. Therefore it is enough that the lease survives the render phase — the **default `acolyte_lease: 30s` is ok** (see [config.md → `acolytes`](config.md#acolytes)). The old requirement `acolyte_lease > max(one-host apply time)` and the Acolyte lease-renew are **removed** (the gate was relaxed after GATE-1, amend (e)). Only check that `acolytes > 0` (see the WARN above).

**Step 3. Flip `reclaim_apply_runs.enabled` to `true`.** Only after steps 1–2 — a config hot-reload (`enabled: true` without redeploying the binary). After enabling, watch via metrics that recovery does not double the apply: a spike in `keeper_runresult_stale_total` (the epoch-check on receipt cuts off a stale `RunResult`) is expected and means fencing works; growth in the number of repeat applies on the same hosts without a stale cutoff is a signal that fencing is not rolled out across all Souls (go back to step 1).

```yaml
reaper:
  rules:
    reclaim_apply_runs:
      enabled: true     # only after steps 1–2 and with acolytes > 0
      stale_after: 1m   # a formal lease timeout; NOT part of the predicate
      action: set_status
      target_status: planned
```

Before enabling in prod — a separate architect re-review of the coupling (as fixed in [ADR-027](../adr/0027-apply-work-queue.md) amend).

**Soul-also-dead — known-gap (MVP).** If Keeper and Soul both died **after** the job was handed off, the row will hang in `dispatched` (`RunResult` will not arrive, reclaim does not touch it — `dispatched` is not reclaimed by design). In the MVP this is a documented known-gap: the operator restarts the run manually. Closure — post-MVP via Soul-reconcile (the Soul on reconnect reports the actually-led `apply_id`s, Keeper terminalizes the orphaned `dispatched`). A Reaper dispatched-timeout is deliberately **not** done (terminal-by-timeout without confirmation from the Soul is unsafe — amend (g)).

## Metrics

Registered in Keeper's Prometheus registry only when `reaper.enabled: true` (if `false` — the collectors are not published at all, cardinality-safe). Implementation — [`keeper/internal/reaper/metrics.go`](../../keeper/internal/reaper/metrics.go), called from `keeper/cmd/keeper/main.go`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_reaper_rule_executions_total` | counter | `rule` | The number of runs of a rule over the whole uptime of a keeper instance. Incremented both at `purged=0` and on an error — this is "how many times the rule was invoked", not "how many times it fired". |
| `keeper_reaper_rule_purged_total` | counter | `rule` | The sum of processed records (deleted by `delete` / moved to a new status by `set_status` / marked by `expire`). For `action: report` (`reap_orphan_vault_keys`) — the number of **detected** orphans (nothing deleted; "purged" here = "detected"). Not incremented on `dispatch_error`. |
| `keeper_reaper_rule_duration_seconds` | histogram | `rule` | The duration of one run of a rule. `_count{rule}` matches `keeper_reaper_rule_executions_total{rule}`. |
| `keeper_reaper_dispatch_errors_total` | counter | `rule` | The number of dispatch errors (the Purger returned an error, PG/Redis unavailable, etc.). On a trigger `executions_total{rule}` is also incremented, `purged_total{rule}` — not. |
| `keeper_reaper_lease_held` | gauge | — | `1` if this instance holds the Redis lease `reaper:leader`, otherwise `0`. One gauge per keeper instance. Cluster-wide invariant: `sum(keeper_reaper_lease_held) == 1`. |

`rule` — the canonical rule name from the [Rules](#rules) table above (`expire_pending_seeds` / `purge_used_tokens` / `purge_souls` / `purge_old_seeds` / `mark_disconnected` / `purge_audit_old` / `purge_apply_runs` / `purge_voyages` / `purge_push_runs` / `purge_incarnation_archive` / `purge_state_history_archive` / `purge_archived_state_history` / `purge_apply_task_register` / `reclaim_apply_runs` / `reclaim_voyages` / `reconcile_orphan_applying` / `reap_orphan_vault_keys` / `archive_state_history` / `scry_background` / `purge_old_errands` / `purge_orphan_ephemeral_tidings`). Extension of the closed enum — via propose-and-wait together with a new `keeper.yml::reaper.rules.<name>`.

For `scry_background`, `keeper_reaper_rule_purged_total{rule="scry_background"}` = the **number of incarnations processed in the current tick** (goroutines launched; including incarnations for which `ErrConvergeMissing` — the very fact of launching the check); the per-host-drift counters live in `incarnation.last_drift_summary` (read via `GET /v1/incarnations/{name}`), not in Prometheus.

## See also

- [storage.md](storage.md) — the tables the Reaper works over.
- [push.md](push.md), [`../soul/modules.md`](../soul/modules.md) — host-side cleanup (a different topic, a different mechanism).
- [config.md](config.md) → the `reaper:` block in `keeper.yml`.
- [prod-setup.md](prod-setup.md) → prod deployment of Keeper (the Vault policy for `reap_orphan_vault_keys`, the recovery-enable gate).
- [`../soul/identity.md`](../soul/identity.md) — `souls` / `soul_seeds` / `bootstrap_tokens` and their statuses.
- [architecture.md → Reaper](../architecture.md).
- [naming-rules.md](../naming-rules.md) — Reaper and the reserved Charon.
