# ADR-030. Vigil + Oracle — event-driven monitoring (beacons + reactor)

> **This is a contract + design; there is no implementation of the event-loop.** Slice S0 (this ADR) introduces ONLY the normative design, the proto contract (`proto/keeper/v1/beacon.proto` + only-add into the `EventStream` oneof) and the names in [naming-rules.md](../naming-rules.md). The Soul-scheduler, Oracle-router, Postgres registries, migrations, OpenAPI/MCP, and safety mechanisms are the subsequent slices S1–S5 (see below). Closes [open Q №23](../architecture.md#open-questions) and the backlog item [ADR-016(d)](0016-parity-license.md) (event-driven loop). The unfixed proposal `SoulBeacon` (open Q №23) is replaced by this ADR with the final names.

- **Context.** Soul Stack is batch declarative: a run is initiated by an operator (a scenario via API/MCP) or a scheduler; the loop "the host state changed → an automatic reaction" is absent. There is no built-in event loop — host triggers ("a file changed / a pid disappeared / a metric threshold was reached") and a reactor rule ("event → action") are absent. This blocks self-healing scenarios (a service went down → restart it with a scenario), reaction to drift, and event-driven automation in general. Open Q №23 deferred the loop until a concrete request; the request has appeared. The entity names in [naming-rules.md](../naming-rules.md) were not fixed — propose-and-wait is needed.
- **Entities (propose-and-wait passed, entered into [naming-rules.md](../naming-rules.md)).**
  - **Vigil** — a Soul-side check (beacon definition): "what to observe and how often". **Read-only by construction** — a Vigil does NOT mutate the host, it only observes and raises an event. The source of truth is the Postgres registry `vigils` (managed via OpenAPI/MCP, toggle + RBAC; symmetric to Augur `omens`/`rites` [ADR-025](0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access)). The body of the check — **built-in core beacons** (`core.beacon.file_changed` / `core.beacon.service_down` / …) + the **plugin-kind `soul_beacon`** for community checks (S5). The set of active Vigils is resolved by the host's covens and travels to it via `VigilSnapshot`.
  - **Portent** — a beacon event (Soul → Keeper). It travels only-add in the `FromSoul.oneof` of the existing `EventStream`. The payload is `google.protobuf.Struct` in the MVP (a typed payload schema is deferred to a separate ADR, like Soulprint in [ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-schema-mvp)). **Edge-triggered** — an event is raised on a **state change**, not on every check tick (otherwise a storm + loops). The SID in the payload is an echo (the authority is the mTLS peer cert, [ADR-012(i)](0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)).
  - **Oracle** — a Keeper-side reactor-router. Receives a Portent → matches against the Decree registry → enqueues a named scenario into the work-queue ([ADR-027](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)). No apply execution of its own — only routing into the existing work-queue.
  - **Decree** — a reactor rule (Postgres registry; managed via OpenAPI/MCP). **Default-deny** (like a Rite [ADR-025](0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access)): no matching Decree → the event triggers no action. Subject binding — `coven` **XOR** `sid` (like a Rite). **A mandatory `incarnation_name`**: a scenario operates on `incarnation.state` (ADR-009), so a Decree explicitly carries the incarnation over which the action runs — guessing it from the subject coven/sid is not allowed (a host carries several incarnation labels; guessing = an amplification vector from a compromised Soul). `ServiceRef` is resolved **from** the incarnation (ADR-007 — the git-ref is not duplicated in the Decree); the reaction is enqueued against the **single SID sender** of the Portent (not against the whole coven); before enqueue — a **membership-check** (the subjectSID must belong to the Decree's incarnation, otherwise skip — fail-closed, no fire/audit written). The optional `where` predicate — a **keeper-local CEL sandbox** with the single root `event` (a narrow predicate over an untrusted payload; `shared/cel` is not extended, a deny-list as in migration-CEL ADR-019 in spirit). **Action = ONLY a named scenario** (whitelist) via the work-queue; a raw command is rejected (see the security invariant).
- **Transport — only-add, no new RPC.** A new file `proto/keeper/v1/beacon.proto`; `PortentEvent` is added into `FromSoul.oneof payload`, `VigilSnapshot` — into `FromKeeper.oneof payload` ([ADR-012(c)](0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) forward-compat only-add; [ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) "one stream" is honored). `VigilSnapshot` carries `repeated VigilDef vigils` (`name` / `interval` / `check` / `params`) and is applied by the Soul as **ReplaceAll** (like `SigilSnapshot` / `SigilTrustAnchors` [ADR-026(h)](0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)): the full active set for the SID replaces the local one, a Vigil that is absent the Soul-scheduler stops. Minimally extensible (only-add later).
- **MANDATORY INVARIANTS (normative, not optional).**
  - **(a) Loop-prevention.** A triad: a **cooldown** on the Decree (the minimal interval between firings of a single rule) + an **idempotent declarative scenario-action** (a repeated run converges to the same state and quenches the cause of the event) + a **circuit-breaker** (N firings within a window → auto-`disable` the Decree + alert). The edge-triggered Portent (an event on a state change, not on a tick) is the first barrier against a storm.
    - **Normative circuit-breaker mechanism (S4 Part 1).** Storage — the Postgres table `oracle_circuit(decree PK, window_start TIMESTAMPTZ, fire_count INT)` with FK `decree → decrees(name) ON DELETE CASCADE` (a per-decree **fixed-window** counter; the cooldown ADR-030(a) quenches per-(decree, subject), the circuit-breaker counts a rule's firings IN TOTAL). The increment — an **atomic UPSERT** (`INSERT … ON CONFLICT (decree) DO UPDATE` with a CASE window-reset when `window_start <= now - window`, `RETURNING fire_count`): a single statement under a row-lock serializes the read-modify-write — **cluster-safe** without advisory locks (concurrent increments from different Keeper instances are not lost). Called in `evaluateDecree` AFTER a successful enqueue+RecordFire (only an actually enqueued firing is counted). At `fire_count >= max_fires` — trip: `UPDATE decrees SET enabled=false WHERE name=$1 AND enabled=true`; `RowsAffected==1` = **single-winner** (exactly one instance under a concurrent trip writes the metric `keeper_oracle_circuit_tripped_total` + audit `decree.circuit_tripped` + a warn log). Thresholds — global in `keeper.yml`: `oracle_circuit_max_fires` (default 5, **`0` = breaker OFF**, an escape-hatch — `BumpCircuit` is not called) and `oracle_circuit_window` (default 10m); a per-Decree override — a separate pass. **Re-enabling a failed Decree (MVP)** = delete+recreate: the `ON DELETE CASCADE` cascade cleans up `oracle_circuit`, the recreated Decree starts with a clean window (a toggle-endpoint — a separate pass). Self-cleaning via the cascade, WITHOUT a separate Reaper rule. Migration `042_create_oracle_circuit`.
  - **(b) Security.** **Default-deny Decree** (no rule → no action). **Whitelist = scenario-only** — an action can be ONLY a registered named scenario; **a raw command is rejected** as an RCE vector: a compromised Soul could otherwise raise a Portent launching an arbitrary command on the Keeper-managed loop. Commands are available only via `core.exec.run` **inside** a scenario (the same path as an ordinary run, under the same guarantees). **The subject binding** of a Decree (`coven` XOR `sid`) limits which hosts can trigger a rule at all. **RBAC** — separate permissions for managing Vigil / Decree. **Audit** — the category `soul_grpc` ([ADR-022(b)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)), the Oracle firing event (the class name — the `oracle.fired` family, normalized at S3). **A beacon event = an untrusted input**: a Soul may be compromised, so the Oracle treats a Portent as input from a potentially hostile party (default-deny + subject binding + scenario-only — layers of defense).
- **Difference from Augur ([ADR-025](0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access)).** Augur = a **pull of an external value DURING an apply** (request/reply, the Soul asks — the Keeper answers synchronously within the run). Vigil/Oracle = a **push of an internal event-loop on the Soul's schedule** (fire-and-forget, the Soul observes on its own and sends a Portent, the reaction is asynchronous via the work-queue). They do not overlap in purpose; the only thing in common is the transport (both are only-add into the `EventStream` oneof).
- **ADR reconciliation.**
  - [ADR-012(c)](0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) — honored literally: only-add `PortentEvent` (FromSoul, field 7) + `VigilSnapshot` (FromKeeper, field 9), new field numbers are not reused, there is no new RPC. The ReplaceAll-snapshot pattern is adopted from [ADR-026(h)](0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index).
  - [ADR-025](0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access) — orthogonal (pull/apply-time vs push/scheduled); a shared transport oneof, different messages. Default-deny + the subject `coven` XOR `sid` of a Decree mirror a Rite.
  - [ADR-027](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim) — the Oracle does not execute an apply itself, but enqueues a scenario into the existing work-queue (`apply_runs` + `Summons`); an Acolyte picks it up as an ordinary run. No second execution loop.
  - [ADR-016(d)](0016-parity-license.md) — the event-driven backlog is closed; community checks — via the `soul_beacon` plugin-kind (the 4th kind of plugin infrastructure, S5), the implementation is under Sigil integrity ([ADR-026](0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)).
- **Consequences.**
  - **A new file `proto/keeper/v1/beacon.proto`** (`PortentEvent` / `VigilDef` / `VigilSnapshot`) + committed `proto/gen/go/keeper/v1/beacon.pb.go`; only-add into the `keeper.proto` oneof.
  - **Names in [naming-rules.md](../naming-rules.md)** — Vigil / Portent / Oracle / Decree (the dictionary), `PortentEvent` (FromSoul) / `VigilSnapshot` (FromKeeper) (proto Keeper↔Soul), `soul_beacon` as the future 4th plugin-kind.
  - **Future slices introduce** (NOT in this ADR): the Soul-scheduler + core-beacon (S1), the Oracle-router + the Postgres registries `vigils`/`decrees` + migrations (S2), OpenAPI/MCP CRUD for Vigil/Decree + RBAC perms + audit (S3), the safety mechanisms cooldown/circuit-breaker/metrics (S4), inotify + `soul_beacon` plugins + Sigil integration + typed-payload (S5).
- **Trade-offs.**
  - **Struct-payload in the MVP** (not typed) — like Soulprint before [ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-schema-mvp): faster to introduce the loop, the cost is no static payload/params schema. A typed schema — a separate ADR once the set of core beacons stabilizes.
  - **Scenario-only action (no raw command)** — we sacrifice the flexibility of "the reactor immediately executes a command" for eliminating the RCE vector. Commands remain available via `core.exec.run` in a scenario — one level of indirection more, but under the same RBAC/audit/idempotency guarantees.
  - **Push model (fire-and-forget) vs pull (Augur)** — two different loops with a shared transport; the cost is two mental models of events, the gain is that each fits its use case without stretching one over both.
- **Implementation slices.** **S0 (this slice)** — ADR + proto contract + names (pilot). S1 — Soul-scheduler + 1–2 core beacons (edge-triggered). S2 — Oracle-router + the Decree registry + migration + enqueueing a scenario into the work-queue. S3 — OpenAPI/MCP CRUD for Vigil/Decree + RBAC + audit. S4 — safety (cooldown / circuit-breaker / metrics). S5 — post-MVP (inotify / `soul_beacon` plugins + Sigil / typed-payload). Pilot = S0 + S1 + S2.

#### Amendment 2026-05-26 (S5 closure)

Vigil S5 closes three open ends of ADR-030 in one wave (commits `634b818` +
V5-2/V5-3 + V5-4 docs-amend). After S5 — ADR-030 is feature-complete; (iv)
per-Decree cooldown override + (v) toggle-endpoint re-enable of a Decree + (vi)
metric-threshold-pull beacon — separate slices once a concrete request
appears.

**S5 PM-decisions.**

1. **Typed PortentPayload (V5-1).** 6 typed messages (`FileChangedPortent` /
   `ServiceDownPortent` / `PortClosedPortent` / `DiskFullPortent` /
   `ProcessAbsentPortent` / `HttpUnhealthyPortent`) + `custom` Struct in
   the `oneof payload` (fields 7..13). Parity with the typed Soulprint Facts
   ([ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-schema-mvp)). The field `PortentEvent.data`
   (Struct, field 2) → `[deprecated=true]` physically remains; the Soul-side
   1-release transitional period fills BOTH branches (data + typed); a hard-cut
   of `data` in S5-final (post-1-release, parity with the push S7-decision).

2. **`soul_beacon` plugin-kind (V5-2).** The 4th plugin-kind in the plugin
   infrastructure (after `SoulModule` / `CloudDriver` / `SshProvider`). Unary RPCs
   `ValidateVigil` + `Check` (NOT a stream — preserves
   [ADR-020(d)](0020-plugin-infrastructure.md#adr-020-plugin-infrastructure-manifest-format-handshake-lifecycle)
   one-shot lifecycle). The plugin is spawned Soul-side via
   `soul/internal/pluginhost` (parity with keeper-host). Sigil-trust verify on
   the Soul host via the already-delivered `SigilTrustAnchors` snapshot
   ([ADR-026(h)](0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)).
   Vigil routing via the namespace in `VigilDef.check`: `core.beacon.*` →
   built-in, everything else → plugin discovery.

3. **inotify — the 7th core beacon (V5-3).** Linux-only (build-tag), the kernel
   inotify syscall (`golang.org/x/sys/unix`). Fold-adapter: a background
   goroutine reads the inotify-fd, accumulates events over the tick window, `Check`
   returns `state='quiet'/'events'` + payload `{path, events[], count}`.
   Darwin/Windows a stub impl with the error `platform not supported`.
   `recursive=true` is deferred (a source of bugs in the MVP); the `throttle` param
   is accept-and-ignore (forward-compat). `InotifyPortent` typed (field 14).

**S5 invariants.**

- All 7 core beacons + the plugin-beacon are edge-triggered (state-change Portent),
  not level-triggered.
- The plugin-beacon Sigil-verify is mandatory (default-deny on a mismatch).
- Typed-payload + legacy Struct are both valid for the CEL-Decree-where (a 1-release
  transitional period).
- inotify Linux-only inv: running a Vigil on non-Linux → the error
  `platform not supported` in the logs, the scheduler skips the tick, the Vigil-row
  stays `state=unknown`.

**S5 ADR cross-links.**

- [ADR-018](0018-soulprint-typed.md#adr-018-soulprint-typed-schema-mvp) (typed Soulprint) —
  a pattern reference for the typed PortentPayload.
- [ADR-020](0020-plugin-infrastructure.md#adr-020-plugin-infrastructure-manifest-format-handshake-lifecycle)
  (plugin infrastructure) — the 4th kind `soul_beacon` (`KIND_SOUL_BEACON=4`).
  `ManifestSpec.params_schema` is reused for the beacon config.
- [ADR-026](0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)
  (Sigil-trust) — the anchors snapshot is already delivered to the Soul host,
  reused without new machinery.
- [ADR-012](0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)
  (only-add proto) — `PortentEvent.data` deprecated but physically remains;
  oneof field-numbers are strictly only-add.

**What is deferred post-S5.**

- (iv) Per-Decree cooldown override.
- (v) Toggle-endpoint re-enable of a Decree.
- (vi) Metric-threshold-pull beacon — a separate loop (a new ADR).
- L3b live tests for Vigil/Oracle — require the harness `CreateVigil` /
  `CreateDecree` / `WaitForOracleFires` (the M2.5 harness-extension slice).
- inotify `recursive` + `throttle` — a separate slice on request.
- `PortentEvent.data` hard-cut in S5-final after 1 production release
  (parity with the push S7-decision).
