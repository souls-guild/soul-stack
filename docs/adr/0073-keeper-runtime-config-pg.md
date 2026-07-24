# ADR-0073. Keeper runtime-config → Postgres — the `SettingsStore` overlay

- **Status.** Active.

- **Context.** Reload-able Keeper parameters live in `keeper.yml` on every Keeper VM. Keeper is a stateless, horizontally-scalable cluster ([ADR-002](0002-transport-grpc-ha.md)) — changing a threshold means editing the file on each instance and sending each one a `SIGHUP`, and nothing stops the files from drifting apart. [ADR-021(f)](0021-hot-reload-config.md) deliberately deferred cross-host coordination ("cluster-wide reload by event via Redis pub/sub … will be added post-MVP on the first real request"); this ADR is that request.

  The mechanism does not need inventing — it already exists twice. [ADR-028](0028-rbac-storage.md) moved RBAC and [ADR-029](0029-service-registry.md) the Service registry out of `keeper.yml` into Postgres with `rbac:invalidate` / `service:invalidate` pub/sub. The untyped `keeper_settings` table (migration `035_create_keeper_settings`, `key TEXT PRIMARY KEY` under `CHECK (key ~ '^[a-z][a-z0-9_]*$')` — so dots cannot appear in a key — plus `value TEXT NOT NULL`) grows without migrations, and `serviceregistry.Holder` already provides an `atomic.Pointer` snapshot + a 10s TTL-poll + `WatchInvalidations`.

  What the reconnaissance changed: **both pilot consumers read the file snapshot, not the PG Holder.** Toll reconfigures from a `config.Store.OnReload` callback (`keeper/cmd/keeper/daemon.go:3247` → `applyTollReload`, `:3546`); Tempo re-reads per request (`d.store.Get().Tempo.ResolvedVoyageCreate()`, `:4300`). Wiring each consumer to the Holder would mean rewriting every consumer and then maintaining two config-read dialects side by side. The source switch therefore belongs **in the config layer, below the consumers**.

- **Decision.**

  **(a) `SettingsStore` — the name of the subsystem.** The cluster-wide store of Keeper runtime settings in Postgres together with its overlay onto the file config. Name assigned by the user via propose-and-wait; recorded in [`docs/naming-rules.md`](../naming-rules.md). `SettingsStore` owns the `cfg_*` rows of `keeper_settings`, the field-registry, the write-gate and the overlay hook; it does **not** own the ADR-029 well-known keys (`default_destiny_source`, `provisioning_allowed_methods`), which keep their own consumers.

  **(b) Three layers with a fixed precedence: built-in default < file < Postgres.** The effective value of a key is `pg ?? file ?? default` — Postgres wins over `keeper.yml`, and `keeper.yml` wins over the default compiled into `shared/config`. A key with no row in `keeper_settings` is not "empty": the file layer simply shows through. The layering is per key, not per block — `toll.threshold` may come from Postgres while `toll.window_size` comes from the file.

  **★ The target end-state: `keeper.yml` shrinks to the bootstrap floor.** The overlay is not meant to stay a thin patch over a full config file — the direction is that **everything that can live in Postgres does**, and the file keeps only what physically cannot. Each migration phase moves keys out of the file, so for a migrated key the resolution is in practice `default < Postgres`: the file simply stops mentioning it. What necessarily remains in `keeper.yml`, and why:

  | Stays in the file | Why it cannot move |
  |---|---|
  | `postgres.dsn_ref` | Chicken-and-egg — it *is* the way to reach the store. |
  | `vault.addr` / `vault.auth.*` / mounts | Hard-required ([ADR-053](0053-dependency-tiers.md)) and read before Postgres — the Postgres DSN itself is a vault-ref. |
  | `redis.*` | Needed for the invalidation channel and presence; also read before Postgres. |
  | `kid` | Per-instance identity. A cluster-wide value would be a contradiction in terms. |
  | `listen.*` (+ their TLS) | Per-instance network surface, bound once at startup; each host binds its own address, so a shared value would be wrong even if it were hot. |
  | `logging.*` | Must work *before* Postgres — otherwise a Postgres failure could not be reported at all. |
  | `hot_reload.*` | Governs the reload mechanism itself (a race on the signal handler / watch). |
  | `auth.jwt.signing_key_ref`, `metrics.auth.*` | Security-critical and loaded once at startup; excluded from a fail-soft overlay by (j.2). |

  Everything outside that residue is a migration candidate, phased by whether a live hot-apply path already exists for it (see the ‡ group in Non-goals).

  **(c) The overlay hook lives in `shared/config.Store`, late-binding.** The pattern is `Store.SetAuditWriter` (`shared/config/store.go:282`): the binary builds the Store first (before Postgres is up — Vault → pool → migrations is a deliberate order), then injects the overlay source once it exists. A nil hook = today's behavior exactly, so `soul` and `soul-lint` are untouched and `soul.yml` is out of scope. The **merge policy and the field-registry live in `keeper`**, not in `shared` — [ADR-011](0011-go-layout.md) forbids a `shared → keeper` import, so `shared/config` only knows how to call an injected source and apply what it returns.

  **(d) The merge is applied to the in-memory Document, and the merged result goes through the full validation pipeline.** Concretely: read the file → parse to a `Document` → apply one `PatchKeeper(doc, yamlPath, value)` per overlay key → re-serialize → run `LoadKeeperFromBytes` over the merged bytes → atomic swap. The merged config is thus validated by exactly the same parse → schema-validate → semantic-validate pipeline as a plain file reload ([ADR-021(c)](0021-hot-reload-config.md)), including cross-field semantic checks that only make sense on the merged result. The alternative — patching the already-parsed struct after validation — was rejected: it would need a second, weaker validation dialect that never sees cross-field constraints.

  **★ Implementation prerequisite — create-on-write in `shared/config`.** Paths are goccy `PathString` (`$.toll.threshold`, `$.tempo.voyage_create.rate`), and `PatchKeeper` today returns `ErrPathNotFound` for a path absent from the file — create-on-write is explicitly deferred in the package. That is not an edge case here: optional blocks are legal and common (`toll:` absent means "enabled with defaults"), so an operator who never touched a block would be unable to override any key inside it. **The overlay therefore requires create-on-write, and it must land before or with the pilot.** The alternative — falling back to a struct-level patch for absent paths only — is rejected: it would reintroduce exactly the second validation dialect that (d) exists to avoid, and it would do so only on the least-tested code path.

  **★ Invariant: the overlay never touches the file on disk.** The patched Document is in-memory only; write-back ([ADR-021(d)](0021-hot-reload-config.md)) stays a file-path mechanism. A reload-able value set through the API is written to Postgres, **never** back into `keeper.yml` — otherwise Postgres values would leak into the file layer and the precedence in (b) would become unobservable.

  **(e) Key namespace — a reserved `cfg_` prefix inside `keeper_settings`.** Overlay keys are `cfg_<flattened yaml path>`: `cfg_toll_threshold` → `toll.threshold`, `cfg_tempo_voyage_create_rate` → `tempo.voyage_create.rate`. One table, no new migration; the prefix keeps the overlay namespace disjoint from the ADR-029 well-known keys — a real concern, since `default_destiny_source` was itself a top-level `keeper.yml` key before the ADR-029 hard-cut, and an un-prefixed overlay would have collided with it.

  The key ↔ YAML-path mapping is **an explicit entry in a Go field-registry** `{key, yamlPath, parse, validate, default, appliesTo}`, not a mechanical dot→underscore transliteration: flattening is ambiguous (`a.b_c` and `a_b.c` both give `a_b_c`). A guard test asserts that keys are unique, carry the `cfg_` prefix, satisfy the migration-035 CHECK, and resolve to YAML paths that actually exist in `KeeperConfig`.

  **(f) No seeding from the file — Postgres holds only explicit operator intent.** On first start nothing is copied from `keeper.yml` into `keeper_settings`; an absent row means the file layer wins (b). Consequences: no silent per-host drift (whichever instance started first cannot freeze *its* file into the cluster), no write on the startup path, and `DELETE` of a row is a clean revert to the file value. This **supersedes the insert-if-absent seed** sketched in the NIM-126 design plan.

  Discoverability, which the seed was meant to provide, is an API concern instead: the settings read endpoint returns the **effective** value plus `source ∈ {default, file, pg}` per key, computed from the live overlay of the answering instance. That is strictly more informative than a seeded table, which would show a value without saying whether anyone ever chose it.

  **(g) Invalidation protocol — reuse `service:invalidate`, with a mandatory idempotence guard.** A mutation commits to Postgres and the mutating node publishes the existing `{origin_kid, at}` envelope on the `service:invalidate` channel (self-filtered by KID); the other nodes re-read the overlay and re-merge. The TTL-poll (`serviceregistry.DefaultRefreshInterval`, 10s) is **not** removed — Redis pub/sub has no persistence, so a lost message is picked up by the next poll; pub/sub only shortens the typical propagation to milliseconds. The channel already covers `keeper_settings` by construction (`SetSetting` publishes on it today), so no new channel and no new plumbing are introduced.

  **The idempotence guard is load-bearing, not an optimization:** re-merge, compare against the live snapshot, and swap **only on a real difference**. Without it the 10s TTL-poll alone would fire a config swap — with every `OnReload` consumer reconfiguring and an audit event written — every ten seconds. With it, an unrelated `service.create` invalidation resolves to a no-op, which is precisely what makes channel reuse free.

  **(h) Degradation — fail-soft on the read path, last-good always.**

  | Failure | Behavior |
  |---|---|
  | Postgres unreachable **at startup** | Start on the pure file base + `WARN`. **Not fatal** — a deliberate divergence from `serviceregistry.NewHolder`, which *is* fatal on a broken provisioning policy: that policy is a security gate, a runtime tunable is not. |
  | Postgres unreachable **at runtime** | Keep the last-good overlay snapshot + `WARN` + metric. Never fall back to the file mid-flight (that would silently *raise* limits an operator had lowered). |
  | Redis unreachable | Degrade to plain TTL-poll — already the `Holder.WatchInvalidations` contract. |
  | A row fails parse/validate on the read path | Reject the **whole** overlay snapshot (all-or-nothing, never a partial merge), keep last-good, `WARN` + `config.reload_failed`. |

  **Break-glass `KEEPER_CONFIG_SOURCE=file`** — an environment variable (it must work before the config is parsed and without a live Postgres): the instance ignores the overlay entirely and runs off `keeper.yml`. This is the escape hatch for a *valid but harmful* value that has bricked the cluster, which the write-gate cannot catch by construction.

  **(i) Write-gate — validate before publish, fail-closed.** A mutation runs the field-registry `parse` + `validate` + **range bounds** before the row is written; a rejection is a `422` with `keeper_settings` unchanged. Range bounds are mandatory precisely because a type validator is happy to accept `reaper_interval=1ms` or `tempo rate=1e9`.

  **Operator surface — API, UI and RBAC are part of the contract, not a follow-up.** Settings are managed through the Operator API (and MCP), rendered as an editable form in the web UI, and gated by their own RBAC permission family, so an operator can be granted the right to edit instance settings without any other cluster-admin power. Two things follow:

  - **The field-registry is published as a backend catalog** rather than re-implemented on the front end. [ADR-042](0042-backend-driven-ui.md) forbids hardcoding dynamic catalogs in the UI; the working precedent is `GET /v1/herald-types` with `HeraldFieldSpec`/`FieldKind` ([ADR-052](0052-herald-notifications.md)) — a single source that both validates on the write path and describes the form to render. The settings catalog has the same shape: per key its type, range bounds, default, the current **effective** value and its `source ∈ {default, file, pg}`. A newly admitted key then shows up in the UI with no front-end change, and the validator and the form cannot drift apart.
  - **A dedicated permission family**, distinct from `service.*`: editing a cluster-wide runtime tunable is a different privilege from registering a Service, even though both live in `keeper_settings`. Mutations carry their own `<area>.<action>` audit event, following the `provisioning.policy_changed` precedent ([ADR-058(i)](0058-operator-auth-ldap-oidc.md)).

  Exact endpoint paths, the permission-family name and the audit-event name are **propose-and-wait** ([ADR-029(d)](0029-service-registry.md)) and get fixed in the implementation tickets.

  **(j) Admission — enumerated per phase, growing toward the floor.** The set of overlay keys is explicit (a key is in the field-registry or it does not exist), but it is *meant to grow*: each phase moves more of `keeper.yml` into Postgres until only the residue of (b) is left. What gates a key is not caution about the mechanism but these four properties:

  1. it is marked reload-able-without-restart in the per-block table of [`docs/keeper/config.md`](../keeper/config.md);
  2. it is an **operational tunable, not a security gate**. Authn/authz, RBAC, and the provisioning policy keep their own paths ([ADR-028](0028-rbac-storage.md) / [ADR-029](0029-service-registry.md)) — the overlay is fail-soft (h), and a fail-soft security control degrades toward the *more permissive* file value, which is unacceptable;
  3. its value is a **scalar** in this phase — structural reload-able values (`toll.per_coven_thresholds`, `toll.webhook`, `reaper.rules`) stay file-only; JSON-in-TEXT is deferred;
  4. it belongs to neither the bootstrap floor (anything needed *before* Postgres: config path, Vault, the Postgres DSN itself, Redis, listeners and their TLS, `kid`, logging, `hot_reload.*`) nor the require-restart class ([ADR-021(e)](0021-hot-reload-config.md)) — both are excluded by construction, the first by the chicken-and-egg problem and the second because a restart is needed anyway.

  Pilot set (the two parameters that already have a live hot-reload consumer): `cfg_toll_threshold`, `cfg_tempo_voyage_create_rate`, `cfg_tempo_voyage_create_burst`.

  **(k) Audit — the existing events, `source: keeper_internal`, no enum extension.** An overlay-driven swap emits `config.reload_succeeded` (or `config.reload_failed`) with `source: keeper_internal` and `archon_aid: NULL`: on the receiving node the swap has no initiating Archon — it is an autonomous Keeper reaction to a cluster event, which is exactly what `keeper_internal` denotes ([ADR-022(b)](0022-audit-pipeline.md), the `source` enum is closed and its extension is propose-and-wait; **this ADR does not extend it**). The operator's identity is recorded once, on the mutating node, by the settings write-path event (i).

  The overlay path **can** populate `changed_paths` honestly — the field-registry knows exactly which keys differ — even though the generic file↔file diff is still deferred in `shared/config`. Combined with the idempotence guard (g), an event is written only when something actually changed, so the audit trail is a truthful record of propagation rather than a heartbeat.

- **Rationale.** The overlay sits in the config layer because that is the only place where one change buys hot-sync for *every* reload-able parameter and *every* existing consumer: `Store.Get()` and `Store.OnReload` keep their contracts, so Toll, Tempo, the Reaper and everything else downstream need no edits. Storage, invalidation and snapshot machinery are reused wholesale from [ADR-028](0028-rbac-storage.md) / [ADR-029](0029-service-registry.md), consistent with [ADR-005](0005-storage-postgres.md) (managed runtime state in Postgres) and [ADR-006](0006-cache-redis.md) (Redis for cross-instance coordination). The direction of travel follows from what each store is good at: a per-VM file is the right home for the handful of values needed *before* the cluster exists, and the wrong home for anything an operator tunes while it runs — so the file converges on the bootstrap floor (b), and Postgres becomes the place where a cluster-wide setting is read, changed and audited. Until a key makes that trip the layering keeps both readable, which is what makes the migration incremental rather than a flag day.

- **ADR reconciliation.**
  - [ADR-021](0021-hot-reload-config.md) — **amended by this ADR.** (f) "cross-host coordination deferred post-MVP" is closed for the reload-able class; the write-back target for an overlay key becomes Postgres rather than YAML (d); the per-host trade-off is superseded for those keys.
  - [ADR-029(g)](0029-service-registry.md) — the well-known `keeper_settings` key list gains a whole reserved namespace (`cfg_*`) rather than individual keys; the "new key without a table migration" property is unchanged.
  - [ADR-022(b)](0022-audit-pipeline.md) — **not** amended: the overlay reuses `source: keeper_internal` (k).
  - [ADR-011](0011-go-layout.md) — merge policy and field-registry live in `keeper`; `shared/config` only exposes the injection point (c).
  - [ADR-053](0053-dependency-tiers.md) — Postgres and Redis remain mandatory; the overlay adds no new dependency tier, and its degradation (h) is bounded by last-good plus the file base.

- **Non-goals / deferred.**
  - **`config_history` + rollback by timestamp** — still deferred ([ADR-021(i)](0021-hot-reload-config.md)); the audit trail plus git-blame on the file remains the history story.
  - **The ‡ hot-apply group** (`acolytes`, `cadence_scheduler.enabled`, `tempo.enabled`, `toll.enabled`, `watchman_*`, worker-pool sizes) — these require restart because no code spins pools up/down or toggles a subsystem live, not because of where the value is stored. Moving them to Postgres would not make them hot; that is a separate "hot-apply lifecycle" effort. It is, however, the main thing standing between the pilot and the end-state of (b), so the migration phases and the hot-apply work should be planned together. **Until a key has a live apply path, it must be flagged `requires_restart` in the catalog** — otherwise the UI would happily accept an edit that silently does nothing until the next restart, which is worse than not offering the field at all.
  - **Typed columns** in `keeper_settings` — rejected in favor of the untyped table plus the Go field-registry, which is what keeps new keys migration-free.
  - **Structural (non-scalar) overlay values**, canary / staged rollout, and the `soul.yml` side — all out of scope.

- **Trade-offs.**
  - **Double parse per merge.** Patch-then-reparse (d) costs a second parse of the config. Accepted: reloads are rare — the idempotence guard (g) means the work happens only on a real change — and the alternative is a second validation dialect that would eventually drift from the first.
  - **Two places to look for one value.** An operator now has to ask "file or Postgres?". Mitigated by the read endpoint reporting `source` per key (f) and by Postgres holding *only* deliberate overrides, so an unexplained value is by definition a file value.
  - **Fail-soft can serve a stale-but-safe value.** After a Postgres outage a restarted instance runs on the file base (h), which may be more permissive than the operator's override. Accepted, and bounded by admission rule (j.2): the overlay is closed to security gates, so the exposure is limited to operational tunables. The alternative — refusing to start without Postgres — would turn a tunables store into a new hard dependency for the whole cluster.
  - **Channel reuse couples two subsystems.** Service-registry CRUD wakes the overlay and vice versa. Accepted because the idempotence guard reduces the false wake-up to a comparison, and a dedicated channel would duplicate publisher, subscriber and self-filter for no behavioral gain.
