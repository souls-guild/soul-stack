# ADR-052. Herald + Tiding — notifications about run events

> **Status: canon before code (S0, 2026-06-11).** The names **Herald** / **Tiding** and the design (PG registries, tap-decorator over audit-writer, at-least-once webhook delivery, scope = run events only) were chosen/approved by the user (propose-and-wait passed); design — architect. Slices S1–S5 are the implementation. No code/migrations/OpenAPI for this ADR yet.

**Context.** A run ([Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run) `kind=scenario`/`command`, [Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage)-spawn, drift check [Scry](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) is recorded in the audit log ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) and in metrics, but **signals nowhere outward**: an operator learns about a failed nightly-converge or a failed command-Voyage only by opening UI / Grafana / `GET /v1/audit`. There is no full-fledged push channel "a run finished / failed — notify an external system." The only existing webhook ([Toll](0038-toll.md#adr-038-toll--a-cluster-wide-detector-of-mass-souls-attrition) amendment) is narrowly scoped (cluster-degraded set/cleared), configurable only via `keeper.yml` (no UI / RBAC / per-rule subscription), and does not cover run events. A **first-class notification entity** is needed: a delivery channel + a subscription rule, managed via API/MCP/UI under RBAC, reacting to run audit events.

**Decision (locked by the user 2026-06-11).** Two entities are introduced:

- **Herald** — a **delivery channel** for notifications (where to send). The name — "herald," continuing the "voice/heraldic" line alongside [Choir](0044-choir.md#adr-044-choir--named-host-topology-within-an-incarnation)/[Voice](0044-choir.md#adr-044-choir--named-host-topology-within-an-incarnation)/[Oracle](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor).
- **Tiding** — a **subscription rule** (what to react to → via which Herald). The name — "tiding," pairs with Herald.

Metaphor: Herald is the messenger, Tiding is the news he carries. The model — **run event → Tiding match → delivery via Herald**.

**(a) Entities and registries (Postgres, managed via OpenAPI/MCP, Omen/Decree pattern).**

- **`heralds`** — the channel registry. `name` PK (kebab-case, CHECK on format — like `omens.name`), `type` closed-enum (**`webhook`** in MVP; `slack`/`email` — additive post-MVP), `config` JSONB (for `webhook`: `url`, optional `headers`), `secret_ref` (vault-ref — channel secret, e.g. signing-token; **NOT** stored in PG cleartext, pattern `omens.auth_ref` / `core.url`), `enabled` BOOL, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able.
- **`tidings`** — the subscription-rule registry. `name` PK (kebab-case), `event_types` TEXT[] (audit event-types with support for scopes of the form `scenario_run.*` — an area-glob, not an arbitrary wildcard), filters `only_failures` BOOL / `only_changes` BOOL, optional selectors `incarnation` / `cadence` (binding to a run source), `herald` FK→`heralds(name)` (`ON DELETE CASCADE` — deleting a channel takes its subscriptions with it), `enabled` BOOL, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able.

**(b) MVP scope — run events ONLY.** `event_types` is limited to run scopes: `scenario_run.*` / `command_run.*` / `voyage.*` (kind-agnostic) / `incarnation.drift_checked` / `incarnation.run_completed` (a point type, amend §k — a terminal for a single incarnation's scenario-run) / `cadence.*`. These are keeper-internal events ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) categories `api`/`mcp`/`keeper_internal`/`background`), all passing through the audit-writer. **Host beacon events** ([Portent](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor) from Souls) are **NOT** in this scope — see "Rejected alternatives (a)."

**(c) Mechanics — a tap over the audit-writer.** The integration point is a **multi-writer decorator over `audit.Writer`** (a point provided for by [ADR-022(f)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)): after the event is **successfully** written to PG (order "audit fact first, then notification" — a notification never lies about an unrecorded event), the tap hands the event to the **notification-dispatcher**. The dispatcher matches the event against enabled Tiding rules (by `event_types` area-glob + `only_failures`/`only_changes` filters + `incarnation`/`cadence` selectors) and, on each match, enqueues a **delivery job**. The tap is best-effort relative to the main audit-write: a dispatcher failure does NOT roll back the audit record (audit is primary).

**(d) Delivery — at-least-once via a claim-queue worker.** Delivery is implemented by a **claim-queue worker** (parity with [VoyageWorker](0043-voyage.md#adr-043-voyage--unified-batch-run) / [ADR-027(d)](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim): `FOR UPDATE SKIP LOCKED` + PG-lease + `attempt++`): a job is claimed, delivered, and on failure — **retried with backoff**. Semantics — **at-least-once** (user decision: a rare duplicate notification is acceptable — as with command-[Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run); exactly-once is not required). **Delivery-attempt statuses — Redis** (hot data: hot→Redis invariant, [ADR-006](0006-cache-redis.md#adr-006-cache-and-coordination--redis); **NOT** written synchronously to PG on every attempt). **Terminal outcomes** of delivery — audit events `herald.delivered` / `herald.failed` (a permanent trail in the log, not hot).

**(e) Security — mandatory invariants.** A webhook = an outbound HTTP call to an operator-supplied URL → an **SSRF vector**. The guard rail, armed by default (pattern `core.url` / [ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list), [ADR-016](0016-parity-license.md#adr-016-parity-strategy-with-saltstackansible-and-the-soul-stack-license) "safe default + auditable opt-out"):

- **https-only by default** — `http://` URLs only via an explicit per-Herald opt-out with a warning.
- **deny private IPs by default** — a dial into loopback/RFC1918/link-local/metadata is blocked by the actually resolved IP; `allow_private` — an explicit opt-out (as with `core.url`).
- **redirect prohibited** — reuse `netguard.NewCheckRedirect` (downgrade/redirect block); reuse `netguard.ValidateEndpoint` for URL validation + `netguard.GuardedDialContext` (`Resolver`/`DefaultResolver`) to deny private IPs by the resolved address — a keeper-side shared guard `shared/netguard` (the same one `core.url`/`core.http` sits on top of on the Soul side; precedent for keeper-reuse — `keeper/internal/augur/egress.go`).
- **timeout** on delivery (don't hang on a slow/malicious endpoint).
- **`secret_ref` — vault-ref only** (master-cred / signing-token not in PG cleartext; the vault-ref is masked in errors via `shared/audit.MaskSecrets`).
- **The notification payload carries NO resolved secrets** — `input`/vault-resolved values are not placed into the payload (**invariant A** [ADR-027](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim), just as `scenario_run.started`/`command_run.invoked` do not carry `input`); the payload passes through `shared/audit.MaskSecrets`.

**(f) Contracts — all additive.**

- **OpenAPI** — `/v1/heralds` + `/v1/tidings` CRUD (greenfield endpoints → written **full-strict** + `strictDecodeProbe`, pattern [ADR-051 amendment S6 d](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-types-oapi-codegen-types-only--strict): typed `request.Body` + typed-response + a manual `unknown→400` probe of the buffered body).
- **RBAC** — permission families `herald.{create,read,list,update,delete}` + `tiding.{create,read,list,update,delete}` (catalog-driven, [ADR-042](0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-in-the-ui--the-ui-does-not-hardcode-dynamic-catalogs)).
- **Audit** — CRUD families `herald.created`/`herald.updated`/`herald.deleted` + `tiding.created`/`tiding.updated`/`tiding.deleted` (area `api`/`mcp`) + delivery terminals `herald.delivered`/`herald.failed` (area `keeper_internal` — worker-initiated).
- **MCP** — `keeper.herald.*` / `keeper.tiding.*` (parity with `keeper.augur.omen.*` / `keeper.oracle.decree.*`).
- **UI** — a "Notifications" tab (parity with the Vigils/Decrees/Cadences pages; companion-repo `soul-stack-web`).

**Rationale.**

- **A tap over the audit-writer, not a separate event bus.** The audit log is already the single normalized point for all keeper-side run events ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); hanging a multi-writer decorator (a point provided for by ADR-022(f)) reuses the existing write-path and doesn't spawn a parallel event stream. "Audit fact in PG first, then notification" — a notification can never precede/lie about an unrecorded event.
- **at-least-once + claim-queue parity with Voyage.** Failover-resilient delivery reuses a proven claim-pattern ([ADR-027(d)](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)/[VoyageWorker](0043-voyage.md#adr-043-voyage--unified-batch-run)); no new infrastructure. A duplicate notification is an acceptable price for a delivery guarantee (user decision).
- **hot→Redis for attempts.** In-flight attempt statuses are volatile (presence/hot→Redis invariant, [ADR-006](0006-cache-redis.md#adr-006-cache-and-coordination--redis)); a synchronous PG write on every delivery attempt would violate the invariant and load PG. The terminal goes into audit (permanent, auditable).
- **Security first.** SSRF-guard / https-only / vault-ref / payload-masking are mandatory invariants, not "implementation details"; a webhook to an operator-supplied URL without a guard is a textbook SSRF.

**Consequences.**

- **Postgres migration** — tables `heralds` / `tidings` (S1).
- **Domain CRUD** — the `keeper/internal` Herald/Tiding layer (S1).
- **tap-decorator + notification-dispatcher** — multi-writer over `audit.Writer` + Tiding-rule matching (S2; without delivery).
- **webhook delivery** — claim-queue worker + SSRF-guard (reuse `shared/netguard`: `netguard.ValidateEndpoint` / `netguard.NewCheckRedirect` / `netguard.GuardedDialContext`+`Resolver`) + retry/backoff + Redis attempt statuses + audit `herald.delivered`/`herald.failed` (S3).
- **OpenAPI `/v1/heralds` + `/v1/tidings`** (full-strict greenfield) + **RBAC** `herald.*`/`tiding.*` + **MCP** `keeper.herald.*`/`keeper.tiding.*` (S4) — [naming-rules.md](../naming-rules.md), [keeper/openapi.yaml](../keeper/openapi.yaml), [keeper/operator-api.md](../keeper/operator-api.md), [keeper/rbac.md](../keeper/rbac.md), [keeper/mcp-tools.md](../keeper/mcp-tools.md) (docs-writer).
- **UI tab "Notifications"** (S5, companion-repo).
- **Names** Herald / Tiding / `type`-enum / permissions / audit-events / REST paths / MCP / tab — [naming-rules.md](../naming-rules.md).

**Rejected alternatives.**

- **(a) `notify` as a [Decree](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor) action ([Oracle](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor)).** Oracle listens to [Portent](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor) — beacon events of **hosts** from Souls (an untrusted input), and its action = a named-scenario (whitelisted via the work queue). **Run events are keeper-internal and do NOT pass through Oracle** — Oracle simply doesn't see them (it's on the Portent flow, not the audit flow). On top of that, the Decree subject model (`coven` XOR `sid` + mandatory `incarnation_name`) doesn't fit "a run event" (a scenario_run event's subject is a Voyage/incarnation, not a single SID-sender), and action=notify doesn't fit into the "action = ONLY a named scenario" whitelist. The split is fixed: **Oracle = reaction to host beacon events; Herald/Tiding = reaction to keeper-internal run events**. Notifications on beacon events are a separate future effort (see "Deferred").
- **(b) Config-only webhook in `keeper.yml`** (as with the [Toll](0038-toll.md#adr-038-toll--a-cluster-wide-detector-of-mass-souls-attrition) webhook). No UI tab, no RBAC management, no per-rule subscription — against the order ("a first-class managed entity with UI/RBAC"). Herald/Tiding are PG-managed via API/MCP/UI, not an inline config.

**Relation to ADRs.**

- **[ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)** — the tap = a multi-writer decorator over `audit.Writer` (point ADR-022(f)); audit is primary, notification is secondary.
- **[ADR-043](0043-voyage.md#adr-043-voyage--unified-batch-run)** / **[ADR-046](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage)** / **[ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)** — sources of run events (`scenario_run.*`/`command_run.*`/`voyage.*`/`cadence.*`/`incarnation.drift_checked`).
- **[ADR-027](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)** — the claim-queue delivery pattern (at-least-once, lease, `attempt++`) + invariant A (payload without resolved secrets).
- **[ADR-006](0006-cache-redis.md#adr-006-cache-and-coordination--redis)** — hot→Redis for attempt statuses.
- **[ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list)** / **[ADR-016](0016-parity-license.md#adr-016-parity-strategy-with-saltstackansible-and-the-soul-stack-license)** — SSRF-guard / https-only / the `core.url` opt-out pattern; keeper-side webhook delivery reuses the shared `shared/netguard` (`netguard.ValidateEndpoint`/`netguard.NewCheckRedirect`/`netguard.GuardedDialContext`), the same guard that Soul-side `core.url`/`core.http` sits on.
- **[ADR-025](0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access)** — the `secret_ref`/`auth_ref` vault-ref pattern (secret not in PG cleartext) + a PG-managed registry via API/MCP.
- **[ADR-030](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor)** — Oracle/Decree (domain split: host beacon events vs run events).
- **[ADR-051](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-types-oapi-codegen-types-only--strict)** — the greenfield endpoints `/v1/heralds`/`/v1/tidings` are written full-strict + `strictDecodeProbe` (amendment S6 d).
- **[ADR-042](0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-in-the-ui--the-ui-does-not-hardcode-dynamic-catalogs)** — the `herald.*`/`tiding.*` permission catalog is fetched by the UI, not hardcoded.

**Slice map (Herald/Tiding epic).**

- **S0** — canon (this ADR + names in naming-rules). _(current slice)_
- **S1** — PG migration (`heralds`/`tidings`) + domain CRUD.
- **S2** — the tap (multi-writer decorator over the audit-writer + notification-dispatcher: Tiding-rule matching), **without** delivery.
- **S3** — webhook delivery: claim-queue worker + SSRF-guard + retry/backoff + Redis statuses + audit `herald.delivered`/`herald.failed`.
- **S4** — OpenAPI `/v1/heralds`+`/v1/tidings` (full-strict) + RBAC `herald.*`/`tiding.*` + MCP `keeper.herald.*`/`keeper.tiding.*`.
- **S5** — UI tab "Notifications" (companion-repo).

**Deferred post-MVP.**

- **Herald types `slack` / `email`** — additive new values of the `heralds.type` enum + per-type config/delivery, without a breaking change.
- **Delivery dedup** (exactly-once) — at-least-once is sufficient (a rare duplicate is acceptable); dedup — upon real demand.
- **Per-channel rate-limit** on delivery (protection against a storm of notifications to a single Herald) — a separate slice.
- **Notifications on host beacon events** ([Portent](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor)/[Oracle](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor)) — a separate future effort (the Oracle/Portent domain ≠ the run audit domain, see "Rejected (a)").

---

## Amendment (2026-06-11, "one-off notifications on a Run + a flexible body")

An extension of Herald/Tiding at the user's request (decisions made 2026-06-11, design — architect). Everything is **additive**: none of the existing fields/contracts change semantics. Implemented in slices N1–N4 (map at the end of the block).

**Context.** [Tiding](#adr-052-herald--tiding--notifications-about-run-events) MVP is a permanent subscription rule for a class of events. It doesn't cover two requests: (1) "notify about **exactly this** run I'm launching right now" — a one-off subscription to a specific [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run) that leaves no litter in the rule registry; (2) a flexible webhook delivery body — adding static operator fields and/or narrowing the payload to needed paths, without reworking the receiver for the full form.

### (g) Ephemeral Tiding — a one-off rule bound to a run.

**Not a new entity — a flag.** [Tiding](#adr-052-herald--tiding--notifications-about-run-events) gets two new fields:

- **`ephemeral`** BOOL DEFAULT `false` — a marker of a one-off rule.
- **`voyage_id`** — a selector binding to a specific run ([Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run)). For `ephemeral` rules — mandatory (the rule matches only events of this run); for ordinary rules — `NULL`.

An ephemeral Tiding is **NOT a registry entity for the operator**: its lifecycle is tied to the parent [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run) run (initiator = the run). It's stored as a row in `tidings` (`ephemeral=true` + `voyage_id`), but is **removed** from the Notifications tab — the operator does not manage it as a standalone rule. Visibility of a one-off rule and its deliveries is contextual, on the Voyage/Run detail page (a run-notifications section), not in the general [Tiding](#adr-052-herald--tiding--notifications-about-run-events) list.

**Creation — atomically by keeper from a `notify` block in [VoyageCreateRequest](0043-voyage.md#adr-043-voyage--unified-batch-run).** An optional **`notify`** block (additive) is added to `VoyageCreateRequest`: which [Herald](#adr-052-herald--tiding--notifications-about-run-events) channel + filters/body (the same fields as a permanent Tiding: `only_failures`/`only_changes`/`annotations`/`projection`). When the block is present, keeper, in **the same transaction** that creates the [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run) itself, creates an ephemeral Tiding with the `voyage_id` of the new run. Atomicity by construction rules out the race "the run finished before the rule was created in the DB": either both records are in one tx, or neither. **But tx atomicity guarantees only that the rule EXISTS in the DB, not its visibility to the dispatcher's TTL cache** — that needs explicit invalidation (the invariant below).

**Dispatcher cache invalidation — two-tier, mandatory (invariant).** **ANY path creating a Tiding rule** — including a direct insert of an ephemeral Tiding into the voyage transaction from the `notify` block, **bypassing `herald.Service`** — **MUST** after commit invalidate the dispatcher's snapshot of enabled rules on two tiers: (1) **in-process** `InvalidateRules()` on the local instance; (2) **cross-keeper** publish on the **`herald:invalidate`** channel ([ADR-006](0006-cache-redis.md#adr-006-cache-and-coordination--redis) pub/sub). Without this, a fast run (shorter than the dispatcher's rule-snapshot TTL, default **15s**) dispatches the terminal event against a stale cache — and the one-off rule silently misses. **Cross-keeper publish is mandatory**, not merely in-process, because: an ephemeral Tiding is created on one keeper, but finalizing the run and dispatching its terminal event can be done by **another** instance of the stateless cluster ([Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run)orch `ClaimNext`) — that instance must receive the invalidation via Redis. This is the same `herald:invalidate` + TTL-poll mechanism used for ordinary Tidings ([rejected alternative below](#adr-052-herald--tiding--notifications-about-run-events)); a direct insert into the voyage tx is not exempt from it.

**RBAC.** An Archon creating a run with a `notify` block must hold **`herald.read`** on the specified channel (a guard in the [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run) create handler — one cannot subscribe a notification to a channel one has no access to). The semantics of `herald.read` (rather than `tiding.create`) is a deliberate choice: an ephemeral Tiding is not managed by the operator as a standalone registry entity — it's a derivative of the run; the gate is access to the receiving channel.

**Listing.** `GET /v1/tidings` by default does **NOT return** `ephemeral` rules ([ADR-042](0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-in-the-ui--the-ui-does-not-hardcode-dynamic-catalogs) dumb frontend — the operator's registry doesn't show them); an optional query parameter `include_ephemeral=true` for debugging.

**Cleanup.** An ephemeral Tiding is removed by the Reaper rule `purge_orphan_ephemeral_tidings` on the predicate "the run is terminal" with a grace period (grace is **mandatory** for delivery correctness: the dispatcher is asynchronous, a synchronous removal on the terminal would race ahead of the terminal notification). Voyages in PG are not currently deleted or archived — cascading cleanup via `ON DELETE CASCADE` FK will only become possible once run retention/archiving is introduced (a separate feature, [ADR-046](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) `purge_voyages` — unimplemented); until then, Reaper-purge is the only mechanism. The earlier phrasing about "removing the subscription on the [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run) terminal" was inaccurate — there's no such step in the code.

### (h) A flexible webhook-delivery body — `annotations` + `projection`.

[Tiding](#adr-052-herald--tiding--notifications-about-run-events) (including ephemeral, and the `notify` block) gets two body-control fields:

- **`annotations`** JSONB — static operator fields (arbitrary key→value). Merged into the webhook-delivery body under a **new top-level key `annotations`** (see (i)). Purpose — tag a notification with receiver context (`team: ops`, `severity: high`, a runbook link, etc.).
- **`projection`** TEXT[] — an allow-list of paths from the event payload. Non-empty → the body carries only the payload subset at these paths; **empty = the current full form** of the payload (backward-compat by default).

**Both are computed in the delivery worker (claim-queue, off-path).** The hot-path invariant is preserved: **the dispatcher remains "match + enqueue a copy"** ([ADR-052(c)](#adr-052-herald--tiding--notifications-about-run-events)) — it touches neither `annotations` nor `projection`. Merging `annotations` and applying `projection` to a payload copy happens in the worker while assembling `webhookPayload`, outside the audit-writer tap path.

**Templating engines — decision locked.**

- **Go text/template — REJECTED.** A different layer ([ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files) keeps text/template strictly for rendering `templates/<path>.tmpl` files, not for a runtime notification body), plus a DoS surface (an operator-supplied template executed by the worker on every delivery).
- **CEL interpolation in the body — DEFERRED.** Would require a separate CEL sandbox (parity with the migration-CEL sandbox [ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)) + a secret-hygiene audit (a CEL expression could pull more out of the context than the masked payload). Introduced as a separate slice upon real demand; `annotations`+`projection` in MVP cover "add static data" and "narrow" without computation.

**Secret hygiene is unchanged.** `annotations` is operator static data (not computed from the event context, secrets don't end up in it except by the operator's own hand). `projection` is a subset of an **already-masked** payload (after `shared/audit.MaskSecrets`, [ADR-052(e)](#adr-052-herald--tiding--notifications-about-run-events)) — narrowing cannot expose more than what was already in the payload copy. Invariant A ([ADR-027](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)) remains in force.

### (i) The webhook contract — the `annotations` key (additive).

The body of the webhook POST ([ADR-052(d)](#adr-052-herald--tiding--notifications-about-run-events), `webhookPayload`: `event_type`/`occurred_at`/`herald`/`tiding`/`payload`) gets an **optional** top-level key **`annotations`** (an object from `Tiding.annotations`; absent when annotations are empty). Additive for external receivers: existing integrations reading `event_type`/`payload`/… don't break.

### Rejected / deferred (Amendment).

- **Per-task alerting ("changes on task X")** — deferred at the time of the N-epic; **activated by a separate design effort** (see Amendment §j below). The chosen path is confirmed: a per-task changed breakdown **in the run's terminal event**, not a subscription to a per-task stream; the [Tiding](#adr-052-herald--tiding--notifications-about-run-events) scope on `task.executed` is **NOT** expanded (the cardinality of host-level `task.executed` stays outside Tiding). Task addressing — the stable address space `register ∪ id` (§j).
- **`notify` instructions in `voyages` JSONB without an entity** (storing a webhook instruction directly in the run row, without an ephemeral Tiding) — REJECTED: this would put a PG lookup into `voyages` on the dispatcher's hot tap path (on every audit event — reading instructions from the run row). An ephemeral Tiding fits into the dispatcher's existing snapshot of enabled rules (`herald:invalidate` + TTL-poll), leaving the hot path untouched.

### Slice map (Amendment N).

- **N0** — canon (this Amendment + additions in naming-rules). _(current slice)_
- **N1** — PG + domain: fields `ephemeral`/`voyage_id`/`annotations`/`projection` in `tidings` (migration + domain layer).
- **N2** — a `notify` block in [VoyageCreateRequest](0043-voyage.md#adr-043-voyage--unified-batch-run) + atomic creation of an ephemeral Tiding (one tx with [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run)) + `herald.read` guard + cleanup (Reaper rule `purge_orphan_ephemeral_tidings`, grace period).
- **N3** — the worker: merging `annotations` + applying `projection` to a payload copy (off-path, while assembling `webhookPayload`).
- **N4** — UI: a "Notify" block in RunWizard (radio pattern like [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run)↔[Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage)) + `annotations`/`projection` fields in Notifications CRUD (companion-repo); visibility of one-off rules — on the Voyage/Run detail page (not in the Notifications tab); default-hide listing on the backend (`include_ephemeral` debug).

---

## Amendment (2026-06-12, "an alert on a change to a specific task — addressing")

### (j) The "task X changed" subscription — the address space `register ∪ id`.

Per-task changed alerting (deferred in the N-epic, see "Rejected / deferred" above) has been **activated** by user decision. This subsection fixes **only the addressing** of a task in a subscription; the breakdown mechanics and the dispatcher are the next slices.

**The task addressing key = the union `register ∪ id`.** A subscription "alert when **this specific** task changed state" addresses the task by a **stable string address**:

- if the task has a `register:`, the address is its `register` name (the task is already addressable, [ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) §8);
- if there's no `register:`, the address is given by the optional field `id:` ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) amendment 2026-06-12).

`register` and `id` are **one address space** (a single format `^[a-z][a-z0-9_]*$`, mutually exclusive on a single task). The address is **stable**: it's set in the scenario/destiny YAML by the author, doesn't depend on `task_idx` (which shifts when the task list is edited) — hence suitable as a long-lived subscription selector.

**What is NOT in this amendment.** The contract of the run's terminal event (exactly how the per-task changed breakdown lands in the payload), resolving the address in `RenderedTask`, the subscription format in [Tiding](#adr-052-herald--tiding--notifications-about-run-events)/`notify`, and the dispatcher binding — are **next slices**, not fixed here. The invariant "Tiding scope on host-level `task.executed` is NOT expanded" ([ADR-052 "Rejected / deferred"](#rejected--deferred-amendment)) remains **in force**: per-task addressing goes through the run terminal, not through a subscription to a per-task stream.

The grammar and validation of the `id:` field — [ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) (amendment 2026-06-12), [`docs/destiny/tasks.md §3`](../destiny/tasks.md#3-complete-list-of-task-blocks).

### (k) The run terminal event `incarnation.run_completed` carries `changed_tasks`.

Section §j fixed task **addressing** (`register ∪ id`); §k fixes the **contract of the terminal event** through which the per-task changed breakdown reaches the subscriber. Decision: the per-incarnation outcome of a scenario-run is emitted as a **new audit event `incarnation.run_completed`** (rather than expanding the subscription scope to host-level `task.executed` — the "Rejected / deferred" invariant remains in force).

**Event name and shape.** `incarnation.run_completed` is the **per-incarnation** outcome of one scenario-run: one event per incarnation-run, **not per-host** (written by [`scenario.Runner`](../keeper/modules.md) after the barrier on a successful terminal of a regular run, alongside the state commit). It must be distinguished from **`run.completed`**: that is a per-host `RunResult` from a Soul (`source: soul_grpc`); `incarnation.run_completed` is a keeper-internal rollup (`source: keeper_internal`, `archon_aid` column NULL, `correlation_id = apply_id`). The aggregation level is **per-incarnation scenario-run, NOT summed over [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run)** (the voyage-orchestrator is untouched; the voyage outcome is carried by `scenario_run.*`/`command_run.*`).

**Payload:** `{incarnation, scenario, apply_id, status, changed_tasks, cadence_id?, voyage_id?}`, where `status` ∈ {`success`, `failed`} and `changed_tasks` is an array of records `{idx, name, register, id, module, changed_hosts, total_hosts}` for tasks that changed on at least one host (a task with no changes does **not** appear in the array). `cadence_id` and `voyage_id` are optional keys (see amend §k below).

**The source of `changed` is the audit log, not a new table.** The changed fact is already recorded for every `(apply_id, sid, task_idx)` by a `task.executed` event with `payload.status`. `changed = TASK_STATUS_CHANGED`. On the terminal, the Runner reads the journal aggregate (`correlation_id = apply_id`, `event_type = task.executed`, `payload->>'status' = 'TASK_STATUS_CHANGED'`) and takes only the **addressing fields** `(sid, task_idx)` — payload values (`register_data`/params/error) are **not read**. Task metadata (`name`/`register`/`id`/`module`) is pulled from the run's in-memory `[]RenderedTask`. **Secret hygiene:** `changed_tasks` carries only metadata + counters, not a single payload value.

**Loop collapse by address.** A single source task with `loop:` unrolls into N `RenderedTask`s with contiguous `task_idx`s, but the address (`register ∪ id`, §j) is **the same** across all iterations. The collapse is by address: one `changed_tasks` record per address, `idx` is a representative (the first iteration). Counters are the **union of unique `sid`s**, not a sum over `idx`: `total_hosts = |union TargetSIDs over all idx of the address|` (the number of target hosts after `on:`/`where:`, **not the whole roster and not a sum over iterations** — otherwise a loop would inflate the denominator to M×K instead of M); `changed_hosts = |union sid with status CHANGED over these idx|`.

**Non-addressable tasks** (no `register` and no `id`) are **included** in `changed_tasks` with an empty address (for completeness of "what and where changed"); each such task is grouped by its own `idx` (not collapsed with other non-addressable tasks). One cannot subscribe to them (the address is a field for subscribing, not for the record's existence), but "what changed" stays complete.

**What is NOT in this amendment.** The subscription format in [Tiding](#adr-052-herald--tiding--notifications-about-run-events)/`notify` for a specific task address and the dispatcher binding of `changed_tasks` to a channel — the **next slice** (T4). The event name — [naming-rules.md → Audit events](../naming-rules.md).

#### Amend §k (T4-0, 2026-06-12): `status: failed`, `cadence_id`, a point scope.

The T4a/T4b foundation extends the §k contract by three points (the success-branch behavior doesn't change):

- **The event is also emitted on a TERMINAL FAILURE of a regular run** — the same name `incarnation.run_completed` with `status: failed` (NOT a separate name; `error_locked` collapses into `failed`, no sub-statuses are spawned — the `task.executed`/`run.completed` pattern, filtering by `payload.status`). Emission point — after `lockIncarnation` (transition to `error_locked`), symmetric with the success branch. `changed_tasks` on failure: **partial** on a late abort (`dispatch_failed`/`register_load_failed`/… — whatever managed to become CHANGED before the failure) or **empty** on an early abort (`no_hosts`/`render_failed`/… — before render, no `tasks` yet). The payload shape is identical to success.
  - **`TerminalDestroy` gate:** a teardown-run failure (scenario `destroy`) does **NOT** emit `incarnation.run_completed` — destroy has its own terminal (`incarnation.destroy_completed` / `.destroy_failed`).
  - **Single-winner:** the failure event is emitted **only by the instance whose `lockIncarnation` actually wrote the terminal**. On a recovery takeover (the row already moved out of `applying` by another committer → `ErrAlreadyFinalized`), the losing instance does **not** write the event — protection against a duplicate event per run, symmetric with the success branch (where the losing commit returns before emission).
- **The optional `cadence_id`** in the payload — present **only** when the run was spawned by a [Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) schedule (a child [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run), `voyages.cadence_id`). A manual run does **not** carry the key (conservative, like the drift payload). This lets a permanent Tiding rule with a cadence selector catch the **results** of scheduled runs, not only the spawned/skipped events (T4b).
- **[Tiding](#adr-052-herald--tiding--notifications-about-run-events) subscription scope.** `incarnation.run_completed` is added to scope §(b) as a **point** type (alongside `incarnation.drift_checked`). The whole `incarnation.*` area is **NOT** opened up to scope (it carries CRUD/lifecycle noise) — only this one point type is admitted.

#### Amend §k (visibility, 2026-06-12): optional `voyage_id` + the `payload_voyage` audit filter.

`incarnation.run_completed` is a **per-incarnation** event with `correlation_id = apply_id` (one run of one incarnation). The Voyage detail page in the UI shows "what and where changed" across the whole voyage, but it reads audit by `voyage_id`, not by each incarnation's `apply_id` — without a back-link to the voyage, run events can't be collected from it. Decision (path B):

- **The optional `voyage_id`** in the payload — present **only** when the run was spawned by the [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run) orchestrator (the production `ScenarioSpawner` forwards `voyages.voyage_id` into `RunSpec.VoyageID`). Direct scenario-run paths (`create`/`rerun-create`/`destroy` and their MCP analogs) call `scenario.Runner` directly, bypassing the voyage orchestrator, → they do **not** carry the key (symmetric with `cadence_id`). `voyage_id` and `cadence_id` are orthogonal: a child [Voyage](0043-voyage.md#adr-043-voyage--unified-batch-run) of a schedule carries both.
- **The `payload_voyage` audit filter.** `GET /v1/audit` accepts an optional query param `payload_voyage` → an exact match on `payload->>'voyage_id'` (a parameterized placeholder, following the `payload_herald` pattern). The Voyage detail page fetches the voyage's per-incarnation run events with this filter.
- **What's not included:** the Voyage detail UI (rendering "what and where changed") — the next slice.

### (l) Backend match T4: a [Tiding](#adr-052-herald--tiding--notifications-about-run-events) task selector + a cadence selector on `incarnation.run_completed` (T4-match, 2026-06-12).

§j fixed task addressing, §k fixed the terminal event contract. §l fixes the **dispatcher match**: a rule subscribing to a specific task, and extending the cadence selector to schedule-run results (backend for both T4a/T4b branches — UI is a separate future effort).

**The [Tiding](#adr-052-herald--tiding--notifications-about-run-events) task selector.** The field `tidings.task` (TEXT, nullable; migration 073) — an optional selector for subscribing to a SPECIFIC task of a run by its address (`register ∪ id`, §j). `NULL` = no filter (S1 behavior). A non-empty value → the rule matches **only** `incarnation.run_completed` events whose `changed_tasks` contains a record with `register == task` OR `id == task` (`herald.matchTask`, symmetric with `matchIncarnation`/`matchCadence`). **The "presence = changed" semantics:** a record in `changed_tasks` exists only for a task that changed on at least one host (§k), so the task selector is **self-sufficient** — it needs no separate "did it change" check. The empty address of a non-addressable task (`register==""` && `id==""`) is **not** matched by a non-empty selector (domain normalization `""→nil` + defense-in-depth in `matchTask`). Any other `event_type` (no `changed_tasks`) → no match.

**`only_changes` × `incarnation.run_completed`.** For consistency when combining the task selector with `only_changes`: `hasChanges(incarnation.run_completed)` = `len(changed_tasks) > 0`. The task selector is self-sufficient on its own, but if an operator combines it with `only_changes`, that must not silently drop a matching event; on a failure with no changes (empty `changed_tasks`) — `false`.

**The cadence selector catches schedule-run results.** `eventCadence` is extended with the `incarnation.run_completed` case → `payload.cadence_id` (alongside `cadence.spawned`/`cadence.skipped_overlap`). A permanent [Tiding](#adr-052-herald--tiding--notifications-about-run-events) with a cadence selector now also catches the **results** of runs spawned by a [Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) schedule (via the optional `cadence_id` payload key, amend §k), not just the `cadence.*` events themselves. A manual run doesn't carry `cadence_id` → no match (conservative).

**Surface.** The `task` field is threaded through the domain (`Tiding.Task`), CRUD (insert/update/select), the OpenAPI schemas `Tiding`/`TidingCreateRequest`/`TidingUpdateRequest` (nullable string), the REST handler (omit==clear on a PUT-replace, like `annotations`/`projection`), the MCP tools (`keeper.tiding.create`/`update`/`read`). **Not included:** UI (the Notifications tab) — a next effort.

#### Amend §l (T4-cadence-fix, 2026-06-12): the cadence selector catches schedule-run results at the Voyage TERMINAL.

§l (T4b) extended `eventCadence` with the `incarnation.run_completed` case. This is **insufficient** for a typical cadence-Tiding rule `event_types=[scenario_run.*]` (or `command_run.*`): such a rule is subscribed to the **Voyage terminal** (the aggregated outcome of the schedule spawn), while `cadence_id` was only carried by the per-incarnation `incarnation.run_completed`. This produced a disjunction: the `scenario_run.*` event didn't carry `cadence_id` (`matchCadence → false`), while `incarnation.run_completed` carried `cadence_id` but didn't fall under the `scenario_run.*` pattern (`matchEventType → false`). Result — a cadence-notify rule **matched no event at all** (a QA blocker).

**The fix (Variant b, architect).** The Voyage terminal `scenario_run.*`/`command_run.*` carries `cadence_id` when the Voyage was spawned by a schedule:

- `voyageorch.emitFinalized` places `cadence_id` in the run terminal payload when `run.CadenceID != nil` (the claim selects `voyages.cadence_id`). nil-guarded symmetrically with `scenario.Runner.emitRunCompleted`: a manual Voyage doesn't carry the key.
- `eventCadence` is extended with a **parallel** case for Voyage terminals (`scenario_run.completed`/`.failed`/`.partial_failed` + `command_run.*`) → `payload.cadence_id`. The `incarnation.run_completed` case is **retained** (needed by the §l task selector, which matches only it).

**What this gives.** A cadence-Tiding `event_types=[scenario_run.*], cadence=<ULID>` now catches **ONE** aggregated notification per schedule spawn (the Voyage terminal), instead of scattering across per-incarnation `incarnation.run_completed` events. Additive (an audit-payload change, not a proto/wire contract). The task selector (§l) is **unaffected**: `matchTask` still matches only `incarnation.run_completed` (the Voyage terminal carries no `changed_tasks`).

#### Amend §k/§l (T4-fix, 2026-06-12): `changed_tasks` and the task subscription cover keeper-side tasks.

§k/§l assume that `task.executed` is recorded for **every** changed task in a run. Initially this was only true for **Soul-side** tasks: the handler [`handleTaskEvent`](../keeper/modules.md) emitted `task.executed` from a Soul's `TaskEvent` payload. **Keeper-side tasks** (`on: keeper` — `core.cloud.provisioned`/`core.vault.kv-read`/`core.soul.registered`, [docs/keeper/modules.md](../keeper/modules.md)) run in-process (`scenario.dispatchKeeperTasks`), no `TaskEvent` goes over the network — so `task.executed` was **not** emitted for them. Consequence: a changed keeper task silently dropped out of the `changed_tasks` collapse (§k), and the task subscription (§l) with its address was **dead** (its `changed/total` counters never counted it).

**The fix (Variant A): `task.executed` is emitted by BOTH sides.** `scenario.dispatchKeeperTasks` writes `task.executed` for **every** keeper task, symmetrically with the Soul-side handler: `sid = keeper` (the address of the run's keeper target, [naming-rules.md](../naming-rules.md)), `correlation_id = apply_id`, `source: keeper_internal`, `payload.status` = the `keeperv1.TaskStatus` name (`changed → TASK_STATUS_CHANGED`, `failed → TASK_STATUS_FAILED`, otherwise `TASK_STATUS_OK`) — the same string the collapse filters on for CHANGED. The payload shape is shared with the Soul side (a single builder `audit.BuildTaskExecutedPayload`) so the two emission points don't drift apart. **Secret hygiene:** the keeper-side `task.executed` carries only addressing fields + status (no `register_data`/output/params); `error.message` — only on failure and only for non-`no_log` (for `no_log` it's suppressed with the `suppressed: "no_log"` marker, as on the Soul side). Keeper-side progress is not broadcast on the operator SSE (only written to audit) — `applybus.Publish` is not called for keeper tasks.

**What this gives.** A changed keeper task with an address `register ∪ id` (a typical `provision_vm` with `id:` and no `register`) now lands in the terminal event's `changed_tasks` (`changed_hosts`/`total_hosts` are counted by `sid = keeper`), and a task [Tiding](#adr-052-herald--tiding--notifications-about-run-events) subscription on its address works. The `changed_tasks` contract (§k) and the task selector (§l) **do not change** — only the set of `task.executed` sources is extended (both sides instead of just Soul-side).

**Rejected (Variant B):** the source of changed = `apply_task_register` instead of `task.executed`. Rejected by the architect: it doesn't cover tasks **without** `register` (and the key case behind the bug is `provision_vm` with `id:` and no `register:`), and it diverges from the source already fixed in §k.

### Slice map (the "alert on task X" epic).

- **T1** — the task field `id:` (struct field `Task.ID`, YAML `id`), grammar/validation ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) amendment, [tasks.md §3](../destiny/tasks.md#3-complete-list-of-task-blocks)).
- **T2** — uniqueness of the `register ∪ id` address space (a config validator: both on one task is prohibited, a duplicate address within a scenario is prohibited).
- **T3** — resolving `id:` in `RenderedTask` + a per-task changed collapse (from the audit_log aggregate, loop-collapse by address) + the terminal event `incarnation.run_completed` (§k).
- **T4-0** — the T4a/T4b foundation (amend §k): `status: failed` on the terminal failure + optional `cadence_id` + a point scope for `incarnation.run_completed` in a Tiding subscription.
- **T4-match (backend)** — dispatcher match for both branches (§l): the task selector `tidings.task` (`matchTask`, migration 073) + a `hasChanges` case for `incarnation.run_completed` + the cadence selector on schedule-run results (`eventCadence` → `cadence_id`). Surface: domain/CRUD/OpenAPI/REST/MCP.
- **T4-fix (backend)** — keeper-side tasks (`on: keeper`) land in `changed_tasks`/the task subscription: `task.executed` is also emitted from `scenario.dispatchKeeperTasks` (amend §k/§l, Variant A). A shared payload builder `audit.BuildTaskExecutedPayload` for both emission points.
- **T4-cadence-fix (backend)** — a cadence-notify rule `event_types=[scenario_run.*]` catches schedule-run results at the Voyage terminal (amend §l, Variant b): `emitFinalized` carries `cadence_id` when `run.CadenceID != nil` + `eventCadence` a parallel case on `scenario_run.*`/`command_run.*`. Delivery guard test (the full dispatcher path). _(current slice)_
- **T4a (UI)** — the Notifications tab: subscribing to a task address (the `task` field) in the Tiding/`notify` form.
- **T4b (UI)** — displaying the cadence binding of schedule-run results (backend closed in T4-match).

## Amendment (2026-06-12, "notifications in the Cadence recurring-schedule form")

### (m) A permanent Tiding from the `notify` block in the [Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) form.

[`CadenceCreateRequest`](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) gets an optional **`notify`** block (additive, the same [`VoyageNotify`](0043-voyage.md#adr-043-voyage--unified-batch-run) shape: `herald`/`on`/`only_failures`/`only_changes`/`annotations`/`projection`). Unlike `voyage.notify` ([§g](#g-ephemeral-tiding--a-one-off-rule-bound-to-a-run), `ephemeral=true` for ONE run), `cadence.notify` creates a **PERMANENT** [Tiding](#adr-052-herald--tiding--notifications-about-run-events) (`ephemeral=false`) that outlives individual runs and reacts to every schedule spawn.

**Binding — by ULID, not by name (rename-safe).** A permanent rule is bound to a [Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) by its **`cadences.id`** (a ULID PK, stable), not by `name` (mutable via PATCH). The binding is set in TWO `tidings` columns:
- **`cadence`** (the existing subscription selector) ← `cadences.id`: the rule filters "notify only about runs of THIS schedule";
- **`created_from_cadence_id`** (a new column, migration 074) ← `cadences.id`: an **ORIGIN marker** — "this rule was created from the schedule form." FK on `cadences(id)` **ON DELETE CASCADE**.

**Why a separate origin marker instead of reusing the `cadence` selector.** The `cadence` selector can also be set on a Tiding an operator created **manually** (subscribing themselves to a schedule's runs). Cascade deletion when a [Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) is removed ([ADR-046 §9](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage)) must remove **only** the rules born from the form, and NOT touch manually created ones with the same selector. Hence origin is a column orthogonal to the selector, with FK CASCADE.

**Creation — atomically in one tx (parity with §g).** When a `notify` block is present, keeper, in **the same transaction** that creates the `cadences` row, inserts the permanent Tidings via a direct `herald.InsertTiding` (Cadence first — the FK is `tidings.created_from_cadence_id → cadences(id)`). Any failure (FK / a Tiding PK-name collision / validation) rolls back the whole `POST /v1/cadences` — neither the [Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) nor the rules are created.

**Auto-rule name — deterministically unique.** `<cadence-name>-notify` for a single `notify` element; with several — a `-2`/`-3`/… suffix (`Tiding.Name` is a PK, collision is not allowed). The human-readable schedule name is coerced to `NamePattern` (`^[a-z0-9-]{1,63}$`) and truncated with headroom for suffixes ([naming-rules.md](../naming-rules.md)).

**Dispatcher cache invalidation — two-tier, mandatory (the same invariant as [§g](#g-ephemeral-tiding--a-one-off-rule-bound-to-a-run)).** Permanent rules are inserted via a direct `herald.InsertTiding` **bypassing the `herald.Service` CRUD** (and its invalidation), so after `commit`, when `notify` is present, keeper **must** two-tier invalidate the dispatcher's snapshot of enabled rules (in-process `InvalidateRules()` + cross-keeper publish `herald:invalidate`). Without this, a fast/cross-keeper spawn (a [Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) spawn can happen on any keeper of the stateless cluster) dispatches the terminal against a stale TTL snapshot (15s) — the rule silently misses.

**RBAC (parity with §g).** An Archon creating a [Cadence](0046-cadence.md#adr-046-cadence--recurring-runs-scheduledrecurring-voyage) with a `notify` block must hold **`herald.read`** on EVERY specified channel (one cannot subscribe a notification to a channel without access) — a guard in the create handler before the tx is opened. A nonexistent channel → 422 (not an FK-500 on insert).

**Lifecycle — cascade (ADR-046 §9).** `DELETE /v1/cadences/{id}` removes the associated permanent rules via the `created_from_cadence_id ON DELETE CASCADE` FK. Rules with the same `cadence` selector but `created_from_cadence_id = NULL` (manually created) survive the schedule's removal.

**What is NOT in this amendment.** A UI "Notify" block in the Cadence form (companion-repo) and an MCP tool for creating a Cadence with `notify` — separate slices. PATCH-editing/deleting auto-rules via the schedule form — deferred (the rules are manageable via ordinary Tiding CRUD; the form only creates).

### Slice map (the "notifications in the Cadence form" epic).

- **C1 (backend)** — migration 074 (`tidings.created_from_cadence_id` + FK CASCADE) + domain/CRUD (the `CreatedFromCadenceID` field) + `cadence.notify` in `CadenceCreateRequest` + a tx refactor of `Create` (Insert Cadence + InsertTiding of permanent rules in one tx + invalidation) + the `herald.read` guard + ULID binding. _(current slice)_
- **C2 (UI)** — a "Notify" block in the Cadence form (companion-repo), parity with RunWizard.
- **C3 (MCP)** — `notify` in the Cadence-creation MCP tool.

---

## Amendment (2026-07-01, "channel-types — 6 new channel types across two transport classes"; recorded retrospectively 2026-07-03)

**User decision 2026-07-01** ("6 Herald types OK"; design — architect; implemented, merge `f3739d03`, migration 091). Retiring the NIM-18 debt — recorded retrospectively.

### (n) Extending the `heralds.type` enum — six new types on top of the MVP `webhook`.

The closed-enum `heralds.type` (§a) is extended **only-add** with six types (the resulting enum — 7 values): `webhook` (MVP, as before) + **`telegram` / `slack` / `mattermost` / `discord` / `custom`** (HTTP class) + **`email`** (SMTP class). Existing `webhook` rows are unaffected (migration 091 recreates the `heralds_type_enum` CHECK with the expanded set). This closes "Deferred post-MVP → Herald types `slack`/`email`" from the base ADR — and goes wider than originally stated (plus a messenger family and a generic `custom`).

### (o) A two-class transport model — `channelDriver` (HTTP) + a separate SMTP axis.

- **HTTP class** (`webhook`/`telegram`/`slack`/`mattermost`/`discord`/`custom`): each type is a **`channelDriver`** ([keeper/internal/herald/channel.go](../../keeper/internal/herald/channel.go)) with three responsibilities — `validateConfig` (CRUD validation of config against a field descriptor, Vault is not read), `secretRequired` (whether the type uses the top-level `secret_ref`), `resolveDelivery` (assembling an **`httpDelivery`** at delivery time: URL + method + body + headers + SSRF-opt-out flags + an optional HMAC signing-key). **A single SSRF guard rail by construction:** the driver ONLY builds `httpDelivery`; the guard (`shared/netguard`, §e) and `client.Do` are called by `DeliveryWorker.deliver` itself — a new HTTP type cannot bypass the guard. For messengers' fixed public endpoints, the opt-out flags (`httpAllowed`/`allowPrivate`) are always `false`.
- **SMTP class** (`email`): **NOT** a `channelDriver` (no httpDelivery/HTTP transport) — a separate `net/smtp` axis ([email.go](../../keeper/internal/herald/email.go)), its own branch in `DeliveryWorker.deliver`, its own SSRF guard by resolved IP.
- The names `channelDriver` / `httpDelivery` / `HeraldFieldSpec` / `FieldKind` are locked by the user (propose-and-wait passed) — [naming-rules.md → Modules and subsystems inside keeper](../naming-rules.md#modules-and-subsystems-inside-keeper).

### (p) Body semantics and secret placement by type.

- **Bodies:** `webhook`/`custom` send the structured `webhookPayload` (§d + `annotations` §i); the `telegram`/`slack`/`mattermost`/`discord` messengers get a **human-readable text digest** (their receivers don't parse our payload); `email` — an RFC5322 message. `custom` = an arbitrary HTTP endpoint with a **fixed** `webhookPayload` body (there is NO operator-supplied `body_template` — the templating engine in the body was already rejected, §h), a configurable method, and an optional `header_secret_ref` in `Authorization`.
- **Secrets:** the top-level `secret_ref` (§a) is used **only by `webhook`** (an HMAC signature of the body, `X-SoulStack-Signature: sha256=<hex>`); for the other types, the credential (bot-token / SMTP password / auth-header) is a **vault-ref field INSIDE `config`** (a descriptor with `Secret=true` ⟹ `Kind=vault_ref`). The §e invariant "secret not in PG cleartext" is preserved for all types.

### (q) A type catalog — `GET /v1/herald-types`, a single source.

The canonical `channelDrivers` registry (+ `email`) is the **single source of the type set**: from it are derived (1) a generic config validator by `HeraldFieldSpec` descriptors (`validateBySpec`), (2) the huma-enum API, (3) the PG CHECK (091), (4) the catalog endpoint **`GET /v1/herald-types`** (`TypeCatalog` → `HeraldTypeDescriptor{Type, Fields, SecretRequired}`; auth-only, no separate permission). The UI builds the per-type form from the catalog, not hardcoded ([ADR-042](0042-backend-driven-ui.md)); the catalog and validator cannot drift apart (one source), the three external spots (CHECK/huma-enum/catalog) are checked against `AllHeraldTypes` by a guard test. Adding a new HTTP type = one entry in `channelDrivers` + a CHECK migration + a huma-enum entry.

**Contract impact:** everything additive (§f) — an only-add enum extension, a new read-only endpoint `/v1/herald-types`, the webhook contract body (§d/§i) unchanged — the Tiding/dispatcher/worker mechanics (§c/§d) are unaffected; only the per-type delivery resolution is extended.
