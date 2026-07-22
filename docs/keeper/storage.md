# Keeper cluster storage: Postgres + Redis

Keeper is stateless by design ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)). Everything that survives an instance restart lives outside the binary: **Postgres** for cold state and **Redis** for the hot layer with coordination.

## Postgres — cold storage (source of truth)

Fixed by [ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres). No embedded KV is used in any form — only PG.

| Registry / table | What it stores | Details |
|---|---|---|
| `souls` | Registry of all Soul records in the system (both agent and push hosts): `sid`, `transport`, `status`, `coven[]`, `registered_at`, `last_seen_at`, `last_seen_by_kid`, operator audit. | [`../soul/identity.md → souls registry`](../soul/identity.md). |
| `soul_seeds` | History of the Soul's mTLS certificates: only `fingerprint`, **without PEM and private keys**. Statuses `active` / `superseded` / `expired` / `revoked`. | [`../soul/identity.md → soul_seeds registry`](../soul/identity.md). |
| `bootstrap_tokens` | One-time onboarding tokens: `token_hash` (SHA-256), TTL, `used_at`, `used_by_kid`. The plain token is **not stored** in the DB. Invariant: `UNIQUE (sid) WHERE used_at IS NULL`. | [`../soul/identity.md → bootstrap_tokens registry`](../soul/identity.md) and [`../soul/onboarding.md`](../soul/onboarding.md) — the lifecycle. |
| `operators` | Registry of Archons: `aid` (PK, kebab-case), `display_name`, `auth_method` (`jwt`/`mtls`/`combined`), `created_at`, `created_by_aid` (FK to `operators(aid)`, `NULL` only for the first Archon - an invariant via a partial unique index), `revoked_at`, `metadata` (jsonb). The `created_by_aid` / `changed_by_aid` FK fields in other tables reference this. | [architecture.md → ADR-014](../adr/0014-operator-identity.md), [rbac.md](rbac.md). |
| `service_registry` | Registry of Services (service type → git coordinates): `name` (PK, kebab-case), `git`, `ref` ([ADR-007](../adr/0007-versioning-git-ref.md): version = git ref), `refresh` (optional auto-fetch duration), operator audit (`created_by_aid` / `updated_by_aid` FK to `operators(aid)`). Moved out of `keeper.yml → services:` ([ADR-029](../adr/0029-service-registry.md)); managed via the `service.*` API/MCP ([operator-api.md](operator-api.md)). The runtime reads an in-memory snapshot (`serviceregistry.Holder`, TTL-poll + pub/sub invalidation `service:invalidate`). | [architecture.md → ADR-029](../adr/0029-service-registry.md), [config.md](config.md). |
| `keeper_settings` | Well-known cluster scalars (`key` PK → `value`), moved out of `keeper.yml`. MVP key: `default_destiny_source` (the default Destiny git-URL template, see [config.md](config.md)). Managed via the `service.*` API/MCP, read by the same `serviceregistry.Holder`. | [architecture.md → ADR-029](../adr/0029-service-registry.md), [config.md](config.md). |
| Destiny catalog | Resolved and validated Service / Destiny / Module artifacts by the `ref:`s from the `service_registry` registry (Service) and `service.yml → destiny[]` (Destiny), git-URL - `service_registry.git` / `keeper_settings.default_destiny_source` ([ADR-029](../adr/0029-service-registry.md)). The registry is in the DB, git is the source. | [architecture.md → Soul Stack artifacts](../architecture.md). |
| `incarnation` | Runtime instances of services: `spec`, `state`, `status`, `service_version`, `state_schema_version`. **Applying-epoch** columns (migration 082, [ADR-027(m)](../adr/0027-apply-work-queue.md)) — all NULLABLE, turn the bare `status='applying'` flag into the run owner's inline epoch, by which the Reaper rule `reconcile_orphan_applying` distinguishes a live run from an orphaned lock: `applying_apply_id` (the `apply_id` of the run holding the lock), `applying_attempt` (the run's fencing epoch, parity with `apply_runs.attempt`), `applying_by_kid` (the KID of the owning Keeper — a presence check of this KID in the Conclave via `InstanceAlive` distinguishes "run in progress" / "owner dead"), `applying_since` (the moment the lock was taken - the rule looks for stale candidates by age `applying_since < NOW()-stale_after`). All four are written by a SINGLE UPDATE atomically with `status='applying'` in `lockRun` (there is no applying-without-epoch window); cleared at the run terminal. NULL-epoch (legacy/pre-082 rows) the rule does NOT reclaim (fail-safe). The `incarnation_applying_scan_idx` index (partial, `ON (status, applying_since) WHERE status='applying'`) keeps the Reaper scan of stale candidates narrow. Managed via API/MCP. | [architecture.md → Incarnation](../architecture.md), [reaper.md → `reconcile_orphan_applying`](reaper.md#rules). |
| `state_history` | Snapshot per change for `incarnation.state`: `state_before` / `state_after`, initiator, time. Retention - [open Q No. 19](../architecture.md). | [architecture.md → `state_history`](../architecture.md). |
| `apply_runs` | Correlation `apply_id` ↔ incarnation/scenario: one row per `(apply_id, sid)` of the run's host fan-out. `status` (`planned`/`claimed`/`dispatched`/`running`/`success`/`failed`/`cancelled`/`no_match`/`orphaned`), `error_summary`, `started_by_aid`, `cancel_requested` (the cluster-wide Cancel flag, see below), the **Ward-claim** columns `claim_by_kid` / `claim_at` / `claim_expires_at` / `attempt` ([ADR-027](../adr/0027-apply-work-queue.md)). **`apply_runs` is orchestrator-agnostic — it does NOT carry a back-link column to the parent run** (a direct `incarnation.run` lives without a Voyage). The link to Voyage is inverse: `voyage_targets.apply_id → apply_runs(apply_id)` is written in the orchestrator's table ([ADR-043](../adr/0043-voyage.md), see the `voyages` + `voyage_targets` row below). _(The sketched back-link `apply_runs.run_id`, like the former `apply_runs.tide_id`/`surge_index` to the `tides` table, are absent from the schema: `tide_id` was removed in Wave 5 together with the `tides` table, migration `061`; `run_id` was never implemented — the link is inverted.)_ On dispatch the scenario-runner writes the row as `planned` and sends a Summons; the Acolyte claims `planned → claimed` via `FOR UPDATE SKIP LOCKED` (`attempt++` — the fencing epoch), resolves+renders just-in-time and moves `claimed → dispatched` **before** sending the `ApplyRequest` (a deliver-once intent marker, [ADR-027 amend GATE-1](../adr/0027-apply-work-queue.md)); the RunResult handler moves it to a terminal. A stale `claimed` (after GATE-1 reclaim narrowed to `status = 'claimed'`, not `running`/`dispatched`: `status = 'claimed' AND claim_expires_at < NOW`) is returned to `planned` by the Reaper leader's recovery scan — closing the "under-delivered render" gap. Cleaned by the Reaper rule `purge_apply_runs`. **Lifecycle: `planned → claimed → dispatched → terminal`** (`running` is a vestigial status of the old synchronous path `dispatchWave` at `acolytes:0`, not used in the Acolyte flow; `dispatched` is NOT reclaimed - after handing off to the Soul a repeat `SendApply` = double apply). The terminals **`no_match`** (a non-target roster host on the Acolyte path: `on:`/`where:` filtered out all tasks → a benign terminal, the barrier counts it on the success side, the incarnation is `ready` — the FINDING-01 fix; **residual dialect**: the old `acolytes:0` path does not create non-target rows at all, the Acolyte path writes the whole roster with `no_match`) and **`orphaned`** (S6 Soul-reconcile, [ADR-027(g)](../adr/0027-apply-work-queue.md): a `dispatched` row not confirmed by the Soul in `WardRoster` on reconnect → barrier fail → `error_locked`). | [architecture.md → ADR-027](../adr/0027-apply-work-queue.md), [architecture.md → ADR-009](../adr/0009-scenario-dsl.md), [reaper.md](reaper.md), [§ Cluster-wide Cancel](#cluster-wide-cancel-of-a-run). |
| `apply_task_register` | Accumulator of the run tasks' register data for `state_changes.sets`: one row per `(apply_id, sid, task_idx)` with `register_data` (jsonb). The handler writes from `TaskEvent.register_data`; after the barrier the scenario-runner reads per-host and resolves `task_idx → register name`. FK to `apply_runs(apply_id, sid)` ON DELETE CASCADE (cleaned cascade-wise together with the run by the `purge_apply_runs` rule). Transient run-state with potential secrets in `register_data`: cleaned more aggressively by a separate Reaper rule `purge_apply_task_register` (default grace 1h after the apply_run terminal, [reaper.md](reaper.md)). | [architecture.md → ADR-009](../adr/0009-scenario-dsl.md), [scenario/orchestration.md §7.1](../scenario/orchestration.md). |
| `voyages` + `voyage_targets` | Registry of Voyage runs (the unified batch, [ADR-043](../adr/0043-voyage.md); the sketch names `runs`/`run_targets` were refined to `voyages`/`voyage_targets` in migration `059`): `voyages` — `voyage_id` (PK), `kind` (`scenario`/`command`), target/`batch_size`/`concurrency`/`schedule_at`/`inter_batch_interval`/`on_failure`, `status` (`scheduled`/`pending`/`running`/`succeeded`/`failed`/`partial_failed`/`cancelled`), the **PG-based failover claim** (`claimed_by_kid` / `last_renewed_at` / `claim_expires_at` / `attempt` - a parallel of the Ward-claim from `apply_runs`); `voyage_targets` — the batch units (Leg) with `batch_index` and a back-link to the child run: `apply_id` (`kind=scenario`, FK meaning → `apply_runs(apply_id)`) or `errand_id` (`kind=command` → `errands(errand_id)`), the partial-UNIQUE indexes (migration `063`) give "one apply_id/errand_id → at most one row". The link direction is **inverted relative to the sketch**: the back-link lives here, not in `apply_runs` (which is orchestrator-agnostic). Picked up by the `VoyageWorker` pool; a stale running Voyage is returned to `pending` by the Reaper rule `reclaim_voyages`. **Replaced the tables removed in Wave 5: `tides` (migration `061`, absorbed by Tide [ADR-040](../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) and `errand_runs` (migration `062`, ErrandRun [ADR-041](../adr/0041-errandrun.md)).** | [architecture.md → ADR-043](../adr/0043-voyage.md), [reaper.md](reaper.md). |
| Run journals | Events of every pull/push apply (what, where, by whom, result). With an RBAC filter. | [push.md](push.md), [rbac.md](rbac.md). |
| Cloud runtime | `Provider`, `Profile`, the registry of created VMs. Managed via API/MCP. | [cloud.md](cloud.md). [architecture.md → Cloud integration](../architecture.md). |
| RBAC policies | Roles, operators, permissions. | [rbac.md](rbac.md). |
| `audit_log` | The Keeper cluster audit trail: `audit_id` (ULID PK), `created_at`, `event_type` (`<area>.<action>`), `source` (closed enum: `signal` / `api` / `mcp` / `keeper_internal` / `soul_grpc`), `archon_aid` (nullable FK to `operators(aid)`), `correlation_id` (ULID, opt), `payload` (jsonb). Written by the `shared/audit` helper by all of Keeper's write-path initiators; cleaned by the Reaper rule `purge_audit_old`. Schema — [§ `audit_log` table](#audit_log-table). | [architecture.md → ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention), [reaper.md](reaper.md), [config.md → audit](config.md#audit). |
| `plugin_sigils` | Keeper-signed allow-list of admitted plugin binaries (`soul-mod-*` / `soul-cloud-*` / `soul-ssh-*`): replaces TOFU. Key `(namespace, name, ref)` (type / binary name / version git-ref; active partial-unique — one active record per triple, re-allow after revoke = a new INSERT) → `sha256` (hex, lowercase, 64) of the admitted binary; `signature` (bytea - Keeper's raw signature ed25519/ECDSA) over the signed block `(… , binary_sha256, manifest)`; `manifest` (jsonb — stitched into the signed block so that `side_effects` / `capabilities` cannot be forged). Lifecycle: `allowed_by_aid` (NOT NULL) / `allowed_at` — an Archon explicitly admitted it via OpenAPI/MCP → `revoked_at` / `revoked_by_aid` (NULL) — revoked. FK to `operators(aid)`: `allowed_by_aid` RESTRICT (the author of an active allowance cannot be deleted), `revoked_by_aid` SET NULL. Schema - [§ `plugin_sigils` table](#plugin_sigils-table). | [architecture.md → ADR-026](../adr/0026-sigil.md), [plugins.md → Integrity-model](plugins.md#integrity-model). |

The connection is set in [config.md](config.md) → the `postgres:` block (`dsn_ref` from Vault, pool size).

### `audit_log` table

Full normalization — [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention). Schema:

```sql
CREATE TABLE audit_log (
  audit_id        TEXT        PRIMARY KEY,                       -- ULID (sortable timestamp prefix + random component; global uniqueness without cross-host coordination)
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  event_type      TEXT        NOT NULL,                          -- '<area>.<action>', catalog: docs/naming-rules.md → Audit-events
  source          TEXT        NOT NULL,                          -- closed enum: signal | api | mcp | keeper_internal | soul_grpc
  archon_aid      TEXT        REFERENCES operators(aid) ON DELETE SET NULL,  -- nullable: NULL for source IN ('signal', 'keeper_internal'); on operator deletion the audit trail is preserved via SET NULL
  correlation_id  TEXT,                                           -- ULID, optional; reuse apply_id for source='soul_grpc'
  payload         JSONB       NOT NULL DEFAULT '{}'::jsonb       -- kind-specific payload, masked per operator-api.md → Secret masking
);

CREATE INDEX idx_audit_event_type   ON audit_log (event_type, created_at);
CREATE INDEX idx_audit_aid          ON audit_log (archon_aid, created_at) WHERE archon_aid IS NOT NULL;
CREATE INDEX idx_audit_correlation  ON audit_log (correlation_id)         WHERE correlation_id IS NOT NULL;
```

Partitioning by `created_at` (for example, a BRIN index or PG declarative partitioning by months) is a post-MVP extension as volume grows; not breaking (the Reaper rule `purge_audit_old` then replaces the batch DELETE with DROP PARTITION).

All of Keeper's write-path initiators (the Operator API HTTP middleware, the MCP handler, Reaper, the hot-reload pipeline, `keeper.cloud`, `keeper.push`, bootstrap, the Soul gRPC event forwarder) write through the common helper `shared/audit` — which also handles secret masking ([operator-api.md → Secret masking](operator-api.md)) and the optional OTel dual-write (`keeper.yml → audit.otel_export`). Reading for `GET /v1/audit` (a separate Operator API extension task) — a standard SQL query with an RBAC filter.

### `plugin_sigils` table

The Sigil registry — the Keeper-signed allow-list of admitted plugin binaries ([ADR-026](../adr/0026-sigil.md), [plugins.md → Integrity-model](plugins.md#integrity-model)). A record appears only when an Archon **explicitly** admits a plugin via OpenAPI/MCP; this replaces the TOFU semantics of "the host itself decides to trust". Schema:

```sql
CREATE TABLE plugin_sigils (
  id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  namespace       TEXT        NOT NULL,                          -- plugin type: cloud / ssh / mod
  name            TEXT        NOT NULL,                          -- binary name: soul-cloud-hetzner etc.
  ref             TEXT        NOT NULL,                          -- git-ref of the version (ADR-007)
  sha256          TEXT        NOT NULL,                          -- digest of the admitted binary (hex, lowercase, 64)
  signature       BYTEA       NOT NULL,                          -- Keeper's signature (ed25519/ECDSA) over the signed block; raw bytes, no base64
  manifest        JSONB       NOT NULL,                          -- stitched into the signed block (ADR-026(c)) → side_effects/capabilities are not forgeable
  allowed_by_aid  TEXT        NOT NULL REFERENCES operators(aid),               -- who admitted it; default NO ACTION (effectively RESTRICT) — the author of an active allowance cannot be deleted
  allowed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  revoked_at      TIMESTAMPTZ,                                   -- NULL = active; NOT NULL = allowance revoked (soft, for audit)
  revoked_by_aid  TEXT        REFERENCES operators(aid) ON DELETE SET NULL,     -- the revocation history survives operator deletion

  CONSTRAINT plugin_sigils_sha256_format CHECK (sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX plugin_sigils_allowed_by_aid_idx ON plugin_sigils (allowed_by_aid);
-- Invariant: at most one ACTIVE record per (namespace, name, ref); this same
-- index covers the lookup during verify. Precedent — bootstrap_tokens (migration 008).
CREATE UNIQUE INDEX plugin_sigils_active_idx ON plugin_sigils (namespace, name, ref) WHERE revoked_at IS NULL;
```

**The `signature BYTEA` choice.** An ed25519/ECDSA signature is raw binary bytes of fixed (ed25519 — 64 bytes) length. `BYTEA` stores them directly, without the overhead of base64 encoding (`text`) and without the risk of an encoding mismatch between the write path (S2a/S3) and the verify path (S6). The exact format of the signed block — slice S3.

**Lifecycle.** `allowed` (an Archon admitted it — audit-event `plugin.allowed`, [naming-rules.md → the `plugin.*` area](../naming-rules.md)) → optionally `revoked` (`revoked_at`/`revoked_by_aid` — audit-event `plugin.revoked`). Revocation is soft: the row remains for audit, activity is determined by `revoked_at IS NULL`. Uniqueness — partial-unique over active records (`plugin_sigils_active_idx`): there is always exactly one active record per triple `(namespace, name, ref)`, and **re-allow after revoke creates a NEW record** (a clean INSERT) — the revocation history is preserved, the previous `sha256`/`signature`/`allowed_by_aid` are not overwritten. A Sigil verification failure before seal/exec on a host — audit-event `plugin.verify_failed`. The registry CRUD, signing and verify — slices S2a/S3/S6, not part of this migration.

## Redis — hot layer and coordination

Fixed by [ADR-006](../adr/0006-cache-redis.md). PG = source of truth, Redis = the derived hot layer and coordination bus.

| Role | What lies there | Why |
|---|---|---|
| **(a) Heartbeat cache** | `last_seen_at` and `last_seen_by_kid` of Souls. The EventStream handler writes the real-time value to Redis on every app message, while it flushes PG-`souls.last_seen_at` **throttled** — no more than once per `mark_disconnected.stale_after / 3` (default 30s) per SID. The "list of active Souls" is read from a Set/HSET in Redis, not from PG. | Removes the UPDATE storm on `souls.last_seen_at` on every stream message, while still keeping the PG snapshot fresh so that the Reaper rule `mark_disconnected` does not mark a live stream disconnected. |
| **(b) SID lease** | `SET sid:lock <kid> NX EX <ttl>` with renewal. Which Keeper holds the active bidi stream to a given Soul. | On a break the TTL expires, and the next Keeper freely takes it. Without its own consensus. |
| **(c) Pub/Sub** | Signals between Keeper instances: "a new Soul came in", "Destiny updated", "SoulSeed revocation". | Getting away from polling Postgres. |
| **(d) Leader lease** | `reaper:leader` — which of the Keepers is currently the Reaper; `conductor:leader` — which one executes Cadence schedules ([Conductor](conductor.md), [ADR-048](../adr/0048-conductor.md)). The keys are **independent** — the leaders may be on different instances. | Each single-executor subsystem (Reaper / Conductor) works on one instance at a time — see [reaper.md](reaper.md), [conductor.md](conductor.md). |

Caching of rendered Destiny / Soulprint is **deferred** until real load appears — not part of the MVP ([ADR-006](../adr/0006-cache-redis.md), last item).

The connection is set in [config.md](config.md) → the `redis:` block (`addr`, `password_ref` from Vault).

## Cluster-wide Cancel of a run

A scenario run is executed **by an in-memory run-goroutine of a single** Keeper instance (the one that accepted the run request, [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)). A local cancel (`Runner.Cancel`) cancels only the `runCtx` of that goroutine. If the operator called Cancel on instance **Keeper-B**, while the run-goroutine lives on **Keeper-A** — the local cancel does not reach it (G1).

The mechanism is a **PG flag** `apply_runs.cancel_requested` (migration 024), multi-instance-safe, riding on the already existing barrier polling:

- **Setting the flag (any instance).** `Runner.RequestCancel` sets `cancel_requested = true` on all still-non-terminal rows of the run in the guard window (`UPDATE apply_runs SET cancel_requested = true WHERE apply_id = $1 AND status IN ('planned','claimed','running')`). This works from any Keeper instance — the flag lives in the shared Postgres, not in memory. The window includes `planned`/`claimed` (cancelling BEFORE `SendApply` is safe: before sending the `ApplyRequest` the Acolyte checks `cancel_requested` and does not send the apply). **Known-gap (ADR-027 amend GATE-1(f)):** `dispatched` is NOT included in the guard — by design in the pilot: Keeper does not send `CancelApply` to Souls already handed off to (best-effort cancel — a separate layer). A run with a dispatched row completes normally, and the barrier sees its terminal by `apply_id`; cluster-wide cancel of an already-handed-off run — a separate sub-slice (guard += `dispatched` together with `CancelApply`-emit), reconciled with Soul-reconcile (Q2).
- **Reading the flag (owner instance).** The run-goroutine in the barrier polling (`waitBarrier`) already polls `apply_runs` every `poll_interval` (default 200ms); the same query reads `cancel_requested`. Seeing the flag, it interrupts the barrier and goes into the same abort path as a local ctx-Cancel.
- **Fast local path.** If the run-goroutine lives on the same instance that accepted the Cancel, `RequestCancel` additionally pokes the local `Runner.Cancel` — the cancel fires immediately, without waiting for the barrier tick. The poll period = the upper bound on the latency of a cross-Keeper cancel.

**The cancel semantics** are identical to a local Cancel: the incarnation goes into `error_locked` (`status_details.reason = dispatch_failed`), the state does not change — the operator removes the lock via `POST /v1/incarnations/{name}/unlock` after analyzing the consequences ([operator-api/incarnations.md → unlock](operator-api/incarnations.md)). To Souls already sent to, Keeper in the pilot does not send `CancelApply` (best-effort cancel — a separate layer via `Outbound.SendCancel`).

**Idempotency and races:**

- A repeated Cancel (double-cancel) is a no-op: the flag `true → true`, the local `Cancel` a second time does not find the goroutine.
- Cancel of an **already completed** run (terminal `status`) is a no-op: the guard `status IN ('planned','claimed','running')` does not touch terminal (or `dispatched`) rows, there is no local goroutine (0 rows affected → `RequestCancel` returns `found=false`).
- The flag does not interfere with normal completion: success/failed rows do not carry `cancel_requested=true` (it is set only in the `planned`/`claimed`/`running` guard window), and if the flag did manage to be set in the gap before RunResult arrives — the barrier sees it before classify and cancels, which is exactly the required Cancel behavior.

## The git ↔ Postgres boundary

This is a system invariant, important when working with any artifact ([architecture.md → Soul Stack artifacts](../architecture.md)):

- **Git** — code and definitions: Service, Destiny, Module. Versioning via git ref ([ADR-007](../adr/0007-versioning-git-ref.md)), review via PR, history via `git log`.
- **Postgres** — runtime state: Soul, SoulSeed, Incarnation, Coven, Provider, Profile, the **Service registry** (`service_registry` + the `keeper_settings` scalars, [ADR-029](../adr/0029-service-registry.md)). Mutations via OpenAPI/MCP, the source of truth is the DB. (The Service repository code itself stays in git — the DB holds only the coordinates `name → git@ref`.)

Optional export of an incarnation to YAML for backup/audit — this is **not** the primary path and not a mandatory feature.

## See also

- [config.md](config.md) — the `postgres:` and `redis:` blocks in `keeper.yml`.
- [reaper.md](reaper.md) — the Reaper cleans the Postgres tables, the leader — via a Redis lease.
- [`../soul/identity.md`](../soul/identity.md) — details of `souls`, `soul_seeds`, `bootstrap_tokens` and onboarding.
- [architecture.md → ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres), [ADR-006](../adr/0006-cache-redis.md).
- [architecture.md → Soul Stack artifacts](../architecture.md).
