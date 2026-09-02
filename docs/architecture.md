# Soul Stack Architecture

The document is the single source of truth for top-level architecture. If the solution here and in the code differ, the document is updated first, then the code. Changes via explicit ADR blocks below.

## Contents

- [System purpose](#system-purpose)
- [Decisions made (ADR-001…039)](#resolved-decisions)
- [Topology](#topology)
- [Soul Life Cycle and Soul Registry](#soul-life-cycle-and-soul-registry)
- [Soul connection: priority and failback](#soul-connection-priority-and-failback)
- [Push mode (`keeper.push`)](#push-mode-keeperpush)
- [Module model](#module-model)
- [Plugin infrastructure](#plugin-infrastructure)
- [Soul Stack artifacts: what's in git, what's in the database](#soul-stack-artifacts-whats-in-git-whats-in-the-database)
- [Destiny: Entry Contract and Validation](#destiny-entry-contract-and-validation)
- [Service - structure and manifest](#service---structure-and-manifest)
- [Service vars: assembly pipeline](#service-vars-the-assembly-pipeline)
- [Incarnation — runtime service instance](#incarnation--runtime-service-instance)
- [Targeting and host communication](#targeting-and-host-communication)
- [Versioning and migration state_schema](#versioning-and-state_schema-migrations)
- [Cloud integration via `keeper.cloud`](#cloud-integration-via-keepercloud)
- [Reaper](#reaper)
- [Delivery of SoulSeed token to host](#delivery-of-soulseed-token-to-the-host)
- [End-to-end installation scenario](#end-to-end-installation-scenario)
- [Top Level Data Flow](#top-level-data-flow)
- [End-to-End Requirements](#end-to-end-requirements-and-where-they-land)
- [Open questions](#open-questions)

Accompanying documents:
- [docs/README.md](README.md) - index of all documentation.
- [docs/naming-rules.md](naming-rules.md) - dictionary of names.
- [docs/requirements.md](requirements.md) - product requirements.
- [../CLAUDE.md](../CLAUDE.md) - guide for AI agents and a summary of solutions.

## System purpose

Soul Stack is a configuration management system with its own dictionary of names ([docs/naming-rules.md](naming-rules.md)) and its own architecture. Applicable for:

- declarative description of the desired state of the hosts (**Destiny**),
- collecting facts about hosts (**Soulprint**),
- storing parameters and secrets (**service vars**),
- remote execution and run check.

Two delivery models are supported:

- **pull model.** The daemon **Soul** is installed on the host, it keeps a long-lived gRPC stream to the Keeper and applies commands as they arrive.
- **push model.** Keeper itself goes to the host via SSH (`keeper.push`) and applies Destiny without an agent. Used for one-time tasks and hosts where a Soul agent is not desired.

## Resolved decisions

Each decision is formulated as ADR: context, choice, rationale, key trade-off. The solution can only be changed by editing the corresponding block. ADR-008 (Coven - stable tags) and ADR-009 (scenario - complete DSL tasks) have been supplemented with the specification in [`docs/scenario/`](scenario/README.md).

### [ADR-001. Implementation language: Go](adr/0001-language-go.md)

Moved to [`docs/adr/0001-language-go.md`](adr/0001-language-go.md). Go as the language of all system binaries: static compilation, mature SDKs for the entire stack (Vault / OTel / gRPC / MCP / k8s), low entry threshold for contributors; trade-off - GC and runtime weight are higher than Rust.

### [ADR-002. Transport Keeper ↔ Souls - gRPC bidirectional stream over mTLS, Keeper HA cluster](adr/0002-transport-grpc-ha.md)

Moved to [`docs/adr/0002-transport-grpc-ha.md`](adr/0002-transport-grpc-ha.md). Bidirectional gRPC stream over mTLS, initiated by Soul; Keeper - a horizontally scalable stateless cluster on top of a shared Postgres/Redis (KID per instance); The Soul client keeps a fallback-list of endpoints. Amendments: presence - Redis-derived (SID-lease, not PG); soul-shedding (Watchman resets streams when isolated); Shepherd - balancing with scale-out (PLANNED/backlog).

### [ADR-003. Destiny format is YAML with typed schema (CUE/JSON Schema)](adr/0003-destiny-format.md)

Moved to [`docs/adr/0003-destiny-format.md`](adr/0003-destiny-format.md). The source of truth is YAML + typed schema (JSON Schema → CUE); templating in a separate safe phase before validation (engines - [ADR-010](adr/0010-templating.md)); strict division "render → validate → apply".

### [ADR-004. Binary layout - `keeper`, `soul`, `soul-lint`; push mode - module inside `keeper`](adr/0004-binaries.md)

Moved to [`docs/adr/0004-binaries.md`](adr/0004-binaries.md). Four separate binaries (`keeper` / `soul` / `soul-lint` / `soul-trial`); push - module `keeper.push` inside `keeper`. No mixing of Keeper and Soul roles in one binary (subcommand within a role is acceptable); the main operator interface is OpenAPI and MCP, CLI is a thin wrapper. Amendment: push Variant C (`POST /v1/push/apply` - ad-hoc multi-host orchestrator).

### [ADR-005. Keeper state store - Postgres](adr/0005-storage-postgres.md)

Moved to [`docs/adr/0005-storage-postgres.md`](adr/0005-storage-postgres.md). Postgres is the only cold storage of the Keeper cluster state (Souls registry, certificate history, Destiny catalog, logs, operator artifacts); no embedded KV. Target scale: tens to thousands of Souls; The SQLite option for small installations is possible, but not an obligation.

### [ADR-006. Cache and coordination - Redis](adr/0006-cache-redis.md)

Moved to [`docs/adr/0006-cache-redis.md`](adr/0006-cache-redis.md). Redis - heartbeat cache (a), lease on SID (b), pub/sub between Keeper instances (c), leader for background tasks/Reaper (d). Amendments: presence Souls - derivative SID-lease + `mark_disconnected` lease-aware bidirectional; presence of Keeper instances - Conclave registry (role e, refuse-startup-guard); Conclave load snapshot for Shepherd (PLANNED/backlog).

### [ADR-007. Versioning of artifacts is done through git ref, not through a field in the manifest](adr/0007-versioning-git-ref.md)

Moved to [`docs/adr/0007-versioning-git-ref.md`](adr/0007-versioning-git-ref.md). Artifact version (Service / Destiny / Module / Plugin) = git ref, there is no `version:` field in manifests; dependencies - strictly `ref:` (tag or branch), without semver-range. Exceptions (not "artifact versions"): `protocol_version`, `service_version`, Go modules inside a repo (shared semver tag). The state-schema version left the manifest entirely and is now derived from the migration ladder ([ADR-019](adr/0019-state-migration-dsl.md), NIM-735).

### [ADR-008. Coven - stable logical tags only](adr/0008-coven-stable-tags.md)

Moved to [`docs/adr/0008-coven-stable-tags.md`](adr/0008-coven-stable-tags.md). Coven - only stable logical tags (cluster / project / environment / data center); convention `{incarnation.name}-{role}` deleted; role NOT Coven (declared in spec / actual via probe); essence role-agnostic. Amendments: environment = special case Coven (first-class `Environment` rejected, per-Coven RBAC-scope implemented); cross-incarnation was lifted at the Voyage layer; Choir ≠ coven. **[2026-07-17 (NIM-124): `incarnation.name` is NO LONGER a Coven — incarnation membership is a first-class relation `incarnation_membership`; the `on:` resolver / RBAC / bulk-select decouple from `coven==name`; `on: ["${ incarnation.name }"]` is a validation error.](adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)**

### [ADR-009. Scenario - a complete DSL of destiny tasks; border with destiny - recommendation](adr/0009-scenario-dsl.md)

Moved to [`docs/adr/0009-scenario-dsl.md`](adr/0009-scenario-dsl.md). The "scenario without `module:`" invariant has been removed completely: scenario receives the entire DSL core of destiny tasks (the source of truth is `docs/destiny/tasks.md`) + orchestration delta (`on:`/`where:`/`apply:`; `state_changes` was part of it until [ADR-084](#adr-084-explicit-state-capture--corestateverb-replaces-end-of-run-state_changes) removed it); destiny/scenario boundary - recommendation (three "yes": reuse / molecule-idempotency / isolability); barrier/state-commit - invariant (cross-host final-barrier, `error_locked` for finally-failed). Amendments: Tide (invocation-time scope chunking, absorbed by Voyage); §7 per-incarnation state-commit in Voyage; Choir - additive `choirs[]`; intra-host concurrency - the task core's key is `async:`, `parallel:` reserved for a future group-with-join ([ADR-0075](adr/0075-intra-host-async-tasks.md)).

### [ADR-010. Template engine: CEL for YAML expressions, Go text/template for files](adr/0010-templating.md)

Moved to [`docs/adr/0010-templating.md`](adr/0010-templating.md). Two engines with a strict file boundary: CEL (google/cel-go) - all YAML expressions (top-level expression keys without wrapper, interpolation through the `${ … }` marker); Go text/template + sprig-allowlist - render files `templates/<path>.tmpl` via `core.file.rendered`. CEL sandbox-by-design; `.j2` → `.tmpl`; `soulprint.hosts.where(...)` - compile-time rewrite in native CEL-comprehension. Full spec - [docs/templating.md](templating.md).

### [ADR-011. Go code layout: go.work with modules on the sides](adr/0011-go-layout.md)

Moved to [`docs/adr/0011-go-layout.md`](adr/0011-go-layout.md). Option B - `go.work` with seven modules on the sides (`proto/` / `proto/plugin/` / `shared/` / `sdk/` / `keeper/` / `soul/` / `soul-lint/`); Soul isolation is guaranteed by the compiler (`soul/go.mod` does not require keeper); committed generated Go; shared semver tags; server-side drivers in `<binary>/internal/`, not in `shared/`. Amendment: abolition of `proto/operator/v1`. Operator API form - **Go-types huma code-first** ([ADR-054](adr/0054-openapi-code-first.md)), derivative spec; oapi-codegen-framework ([ADR-051](adr/0051-operator-api-codegen.md)) and package `keeper/internal/api/oapi/` demolished (2026-06-13).

### [ADR-012. Keeper↔Soul gRPC contract: one EventStream with oneof, Keeper-side render, forward-compat only-add](adr/0012-keeper-soul-grpc.md)

Moved to [`docs/adr/0012-keeper-soul-grpc.md`](adr/0012-keeper-soul-grpc.md). One `service Keeper` with two RPCs - unary `Bootstrap` (server-only TLS, separate listener) and long-lived bidi `EventStream` (mTLS) with `oneof payload`; thematic layout `.proto` in `proto/keeper/v1/`; forward-compat only-add (breaking - via `v2/`); render border - by external access (CEL params/vault - Keeper, text/template-COMPUTE + flow-control CEL - Soul); `RunResult` — final run report; SID in payload is echo, authority is mTLS peer cert. Amendment: `WardRoster` (Soul-reconcile, FromSoul field 8).

### [ADR-013. Bootstrap of the first Archon](adr/0013-bootstrap-archon.md)

Moved to [`docs/adr/0013-bootstrap-archon.md`](adr/0013-bootstrap-archon.md). Bootstrap of the first Archon (entity name - Archon, identifier - AID): administrative subcommand `keeper init --archon=<aid>` under PG advisory lock creates the first Archon with the role `cluster-admin`, releases JWT to the file `mode 0400`; restart-failure if `operators` is empty without `--initialize`; self-lockout-invariant to the last `*`-permission.

### [ADR-014. Operator identity model (Archon)](adr/0014-operator-identity.md)

Moved to [`docs/adr/0014-operator-identity.md`](adr/0014-operator-identity.md). Registry `operators` in Postgres (mandatory, with real FK), credential - JWT (signing key from Vault KV), identifier AID (charset `[a-z0-9._@-]`, prefix `archon-` removed by amendment); creation/revocation lifecycle via OpenAPI/MCP; near-instant revocation via RBAC snapshot (amendment).

### [ADR-015. Core MVP modules: exact list](adr/0015-core-modules-mvp.md)

Moved to [`docs/adr/0015-core-modules-mvp.md`](adr/0015-core-modules-mvp.md). 18 Soul-side core modules (`core.pkg`/`core.file`/`core.directory`/`core.service`/`core.user`/`core.group`/`core.exec`/`core.cmd`/`core.cron`/`core.mount`/`core.git`/`core.archive`/`core.sysctl`/`core.url`/`core.line`/`core.repo`/`core.firewall`/`core.http`) + 3 Keeper-side (`core.soul.registered`/`core.cloud.provisioned`/`core.vault.kv-read`); `core.template`/`core.copy` are NOT deliberately highlighted; `core.line`/`core.repo`/`core.firewall`/`core.http` accepted post-facto (in-place/read-probe MVP). Amendment 2026-07-17: `core.file.directory` split out into the standalone `core.directory` (`present`/`absent`, hard rename); `core.service` gains `disabled`/`masked`.

### [ADR-016. Parity strategy and Soul Stack license](adr/0016-parity-license.md)

Moved to [`docs/adr/0016-parity-license.md`](adr/0016-parity-license.md). License - **BSL 1.1** for the core (this repo) and frontend (`soul-stack-web`): fair-code, Change License Apache 2.0, Change Date 2 years (Amendment 2026-07-09); SDK/examples/plugins - Apache 2.0; enterprise features - separate commercial license. The parity strategy is a hybrid without a wrapper: core MVP is our Go rewrite, exotic is community plugins via SDK; a GPLv3 Python-runtime wrapper is prohibited. Amendments: Plugin SDK Phase 2 (10 official `soul-mod-*`, namespace `official`, template mechanism); fair-code/BSL (license).

### [ADR-017. Keeper-side core modules expanded: `core.cloud.provisioned`, `core.vault.kv-read`](adr/0017-keeper-side-core.md)

Moved to [`docs/adr/0017-keeper-side-core.md`](adr/0017-keeper-side-core.md). Two keeper-side core modules (`on: keeper`): `core.cloud.provisioned` (`created`/`destroyed` via `CloudDriver` plugin, cascade with destroy) replaces the "destiny `cloud-provision`" pattern; `core.vault.kv-read` (`read`) - explicit audit-accurate reading of Vault KV during rendering. Amendments: cloud credentials-flow (Variant A) + 6 implemented providers + cloud-init bootstrap.

### [ADR-018. Soulprint typed MVP scheme](adr/0018-soulprint-typed.md)

Moved to [`docs/adr/0018-soulprint-typed.md`](adr/0018-soulprint-typed.md). Typed `SoulprintFacts` (sub-messages `OsFacts`/`KernelFacts`/`CpuFacts`/`MemoryFacts`/`NetworkFacts`) instead of `google.protobuf.Struct`-stub (deprecated, wire-compat); `os.pkg_mgr`/`os.init_system` collected by Soul Agent; canonical CEL form `soulprint.self.<path>`; `covens`/`choirs` — Keeper projection, not a fact. Amendments: pkg_mgr/init_system hybrid, `choirs`-fact, `typed_facts` byte-passthrough on REST.

### [ADR-019. State_schema migration DSL](adr/0019-state-migration-dsl.md)

Moved to [`docs/adr/0019-state-migration-dsl.md`](adr/0019-state-migration-dsl.md). Grammar flat (`rename`/`set`/`delete`/`move`) + CEL in `set.value` through `${ … }` + structural `foreach`; migration-CEL sandbox(ban `vault`/`now`/`register`/`soulprint`/`essence`/`input`); forward-only; atomic PG transaction per chain; tests `migrations/<NNN>_<slug>/tests/`. A step's place in the ladder is stated exactly once — the version is derived from the ladder, and a generated `schema.lock` catches a `state_schema` edited without a matching step (NIM-735).

### [ADR-020. Plugin infrastructure: manifest, handshake, lifecycle format](adr/0020-plugin-infrastructure.md)

Moved to [`docs/adr/0020-plugin-infrastructure.md`](adr/0020-plugin-infrastructure.md). A unified infrastructure of three kinds of plugins (`soul_module`/`cloud_driver`/`ssh_provider`): static `manifest.yaml` (offline validation `soul-lint`), JSON-handshake with a magic prefix → gRPC-over-unix-socket, `protocol_version` ↔ `proto/plugin/vN/`, one-shot lifecycle, closed-enum `required_capabilities` / `side_effects`, file-permissions instead of mTLS. Amendments: SDK Phase 2, SshProvider-set + credentials-flow.
### [ADR-021. Hot-reload config with write-back YAML](adr/0021-hot-reload-config.md)

Moved to [`docs/adr/0021-hot-reload-config.md`](adr/0021-hot-reload-config.md). Two ways of changing (file-edit `SIGHUP` / API-MCP with write-back YAML round-trip); validation-pipeline (parse→schema→semantic→atomic swap); per-host without cross-host; audit-events `config.reload_succeeded`/`config.reload_failed`; `shared/config` for three binaries; reload-able vs require-restart general principle.

### [ADR-022. Audit-pipeline: storage, schema, retention](adr/0022-audit-pipeline.md)

Moved to [`docs/adr/0022-audit-pipeline.md`](adr/0022-audit-pipeline.md). Postgres table `audit_log` (ULID PK, `source` closed-enum 5 values, `correlation_id` ULID, `payload` jsonb); retention via Reaper rule `purge_audit_old`; OTel dual-write (opt); write-path initiators; block `audit:` in `keeper.yml`; `GET /v1/audit` + `audit.read` is a separate task.

### [ADR-023. Test runner Trial (`soul-trial`) and DSL-coverage](adr/0023-trial-test-runner.md)

Moved to [`docs/adr/0023-trial-test-runner.md`](adr/0023-trial-test-runner.md). Entity Trial, binary `soul-trial`, metric trial coverage; levels L0 render-only / L1 migration / L2 single-host docker (implemented, test-only) / L3 multi-host (deferred); `CoverageSink` to `shared/cel`; layout `keeper/cmd/soul-trial`; the case.yml format extends the migration standard.

### [ADR-024. Observability: Prometheus-primary + OTel-bridge](adr/0024-observability.md)

Moved to [`docs/adr/0024-observability.md`](adr/0024-observability.md). Prometheus-primary (`/metrics` pull) + OTel-bridge for traces and opt. push metrics; namespace prefixes `keeper_*` / `soul_*`; OTel resource-attrs `service.name` + `soulstack.kid` / `soulstack.sid`.

### [ADR-025. Augur — Keeper-side broker for external access Soul](adr/0025-augur.md)

Moved to [`docs/adr/0025-augur.md`](adr/0025-augur.md). Keeper-side broker for Soul live access to external systems (Omen-registry / Rite-grant); MVP-1 broker (`delegate=false`, data via Keeper) → MVP-2 delegation (scoped Vault token / static read-cred); transport only-add `AugurRequest`/`AugurReply` to EventStream; invariant "Soul never receives master-credential". Design, no implementation.

### [ADR-026. Sigil - plugin integrity (Keeper-signed digest index)](adr/0026-sigil.md)

Moved to [`docs/adr/0026-sigil.md`](adr/0026-sigil.md). Keeper-signed allow-list of plugins (registry `plugin_sigils`): an Archon explicitly allows an artifact `(source, ref) → sha256` under a registration `alias` (re-keyed off `(namespace, name, ref)` by NIM-377/438 — the artifact declares no name), Keeper signs the block with the schema document attached, and the host verifies digest+signature BEFORE exec (replaces TOFU first-load). ★ The signature makes the declarations **non-forgeable, not enforced** — `capabilities`/`side_effects` are disclosure for the operator's approval, and the one real control is the approved digest. Git-verified `ref` (go-git F-fetch), multi-anchor signature key rotation (`sigil_signing_keys`), replace semantics snapshot/anchors.

### [ADR-027. Execution model apply - work-queue + claim (Acolyte-pool, Ward-claim)](adr/0027-apply-work-queue.md)

Moved to [`docs/adr/0027-apply-work-queue.md`](adr/0027-apply-work-queue.md). Execution of apply on work-queue + claim: Acolyte-pool, Ward-claim (`FOR UPDATE SKIP LOCKED`), Summons (`apply:summons`), just-in-time render for claim, Reaper recovery scan (`reclaim_apply_runs`), two-way attempt-fencing, single-winner state-commit; Phase 0/1/2 + GATE-1 (deliver-once, lifecycle `planned→claimed→dispatched→terminal`) implemented, Phase 3 distributed serial postponed; amendments Conclave/Watchman/refuse-guard, Voyage back-link, Tide-spawn.

### [ADR-028. RBAC-storage → Postgres](adr/0028-rbac-storage.md)

Moved to [`docs/adr/0028-rbac-storage.md`](adr/0028-rbac-storage.md). Transfer of RBAC-storage (roles, permissions, membership) from `keeper.yml` to Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`), fix BUG-1 (init writes membership to the database, not to the JWT-claim); enforcer on an in-memory snapshot from the database + Redis invalidation; includes Amendment ADR-049 (Synod).

### [ADR-029. Service registry → Postgres](adr/0029-service-registry.md)

Moved to [`docs/adr/0029-service-registry.md`](adr/0029-service-registry.md). The Service registry (`keeper.yml → services[]`) and well-known scalars (`default_destiny_source`) are transferred to Postgres (`service_registry` + key-value `keeper_settings`), runtime - snapshot `serviceregistry.Holder` + `service:invalidate`; config hard-cut three keys; closes the balance [ADR-028(h)](adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres).

### [ADR-030. Vigil + Oracle - event-driven monitoring (beacons + reactor)](adr/0030-vigil-oracle.md)

Moved to [`docs/adr/0030-vigil-oracle.md`](adr/0030-vigil-oracle.md). Event-driven circuit: Vigil (Soul-side read-only check) → Portent (edge-triggered event, only-add in EventStream) → Oracle (Keeper-side reactor-router) → Decree (default-deny rule, action=named-scenario-only). Mandatory invariants loop-prevention (cooldown+circuit-breaker) and security. Amendment S5: typed PortentPayload, `soul_beacon` plugin-kind, inotify-beacon; feature-complete.

### [ADR-031. Scry — drift-detection (declarative dry-run reconcile)](adr/0031-scry-drift.md)

Moved to [`docs/adr/0031-scry-drift.md`](adr/0031-scry-drift.md). **The drift-detection circuit was REMOVED on 2026-08-05 (NIM-446)**: no `check-drift` endpoint or MCP tool, no `DriftReport`, no background `scry_background` scan, no `incarnation.drift_checked` audit event, no `last_drift_*` columns. What the ADR still governs: `Plan` no-op→pure-read in all core modules (the SoulModule contract, exercised by Errand), the only-add proto fields `PlanEvent.changed` / `ApplyRequest.dry_run`, and the informational status `drift` — now written only by a legacy upgrade (see the closing amendment for why it is not orphaned).

### [ADR-032. Push-orchestrator (Variant C) - multi-host destiny push without incarnation/scenario.](adr/0032-push-orchestrator.md)

Moved to [`docs/adr/0032-push-orchestrator.md`](adr/0032-push-orchestrator.md). Multi-host parallel rollout of destiny to a list of SIDs without scenario/incarnation/state: `POST /v1/push/apply`, table `push_runs`, statuses (including `partial_failed`), narrow render-context, best-effort recovery via Reaper; does not mutate `incarnation.state`. Amendments: S6 pilot wire-up → S7-1…S7-4 PG-canon (`souls.ssh_target` / `push_providers` / multi-CA / auto-import) + runtime re-spawn + P2 multi-provider routing.

### [ADR-033. Errand — pull-ad-hoc exec outside scenario.](adr/0033-errand.md)

Moved to [`docs/adr/0033-errand.md`](adr/0033-errand.md). Pull-ad-hoc exec of a single module through an existing Soul-agent (NOT scenario/apply/incarnation-bound): whitelist (`core.cmd.shell`/`core.exec.run` + marker `ErrandReadSafe`), sync/async-hybrid (`POST /v1/souls/{sid}/exec`), does not mutate `incarnation.state`, area `errand.*`, only-add `ErrandRequest`/`ErrandResult`/`CancelErrand`, table `errands`. Amendments: E5 cancel, Voyage `command`-kind recycle infra.

### [ADR-035. Distribution split — core (API+CLI) vs web (UI).](adr/0035-distribution-split.md)

Moved to [`docs/adr/0035-distribution-split.md`](adr/0035-distribution-split.md). Distribution separation: core = only Go artifacts (`keeper`/`soul`/`soul-lint`/`soulctl`/proto/sdk/shared), web — separate companion repo `soul-stack-web`; contract core↔web = OpenAPI (read-only); independent release cycles; the operator can work without UI (CLI+MCP+OpenAPI). `ui/` is removed from core. **Amended [ADR-055](#adr-055-embed-ui-bundle---optional-single-binary-keeper-with-ui-on-ui):** deferred embed-compat-shim is activated as an optional default-ON embed UI on `/ui` (single-binary onboarding beta) - companion-source-of-truth and toolchain-split are preserved, only the "no embed UI-assets" invariant is deployed (now the assembled artifact is allowed).

### [ADR-038. Toll — cluster-wide detector of mass outflow of Souls.](adr/0038-toll.md)

Moved to [`docs/adr/0038-toll.md`](adr/0038-toll.md). Passive cluster-level detector of Souls mass outflow (rate-of-disconnect by gRPC events, sliding 60s, threshold 20% of baseline): per-instance `tollwatcher` + Redis-leader aggregation, soft-degraded mode (503 on write-API, read/destroy/Errand available), asymmetric hysteresis, warmup-immunity; DOES NOT close streams (this is Watchman). Amendment: webhook (generic/pagerduty/slack) + per-coven thresholds + hot-reload.

### [ADR-039. E2E testing - three levels without new dictionary entity](adr/0039-e2e-testing.md)

Moved to [`docs/adr/0039-e2e-testing.md`](adr/0039-e2e-testing.md). L3 e2e without a new dictionary entity, via build-tag Go-tests: L3a fast-loop (soul-stub helper + testcontainers + real Keeper process, every-PR), L3b smoke-loop (real `soul` binary in a container, nightly), L3c k8s-loop (kind-cluster, HA-cases, weekly); fixtures+expectations in YAML, assertion-values = code enums. Amendment: L3a-impl particulars.

### [ADR-040. Tide — invocation-time scope chunking + target-override](adr/0040-tide.md)

Moved to [`docs/adr/0040-tide.md`](adr/0040-tide.md). **Superseded by [ADR-043 (Voyage)](adr/0043-voyage.md), 2026-05-29** (absorbed by `kind=scenario` mode: Surge → Leg, per-Surge state-commit → per-incarnation; implementation removed in Wave 5 - migration `061`, packages `tideorch`/`tide`, `/v1/tides`, audit `tide.*`). Initial fixation - invocation-time chunking scenario-run into successive Surge waves (Tide/Surge entities) + AND-merge target-override (scope narrowing only) + concurrency-override; PG-table `tides`, claim+lease failover (Acolyte-style), snapshot-scope `target_resolved_souls`.

### [ADR-041. ErrandRun — multi-target binding over Errand.](adr/0041-errandrun.md)

Moved to [`docs/adr/0041-errandrun.md`](adr/0041-errandrun.md). **Superseded by [ADR-043 (Voyage)](adr/0043-voyage.md), 2026-05-29** (absorbed by `kind=command` mode; implementation removed in Wave 5 - migration `062`, packages `errandrun`/`errandrunorch`, `/v1/errand-runs`, audit `errand_run.*` → `command_run.*`). Initial commit - multi-target binding over N ad-hoc `Errand` (common ULID, AND-merge target, concurrency-cap, cancel-all; registry `errand_runs`). Single [Errand](#adr-033-errand--pull-ad-hoc-exec-outside-scenario) (`POST /v1/souls/{sid}/exec`) is NOT deleted, sugar remains.

### [ADR-042. Backend-driven dynamic data in UI - UI does not hardcode dynamic directories.](adr/0042-backend-driven-ui.md)

Moved to [`docs/adr/0042-backend-driven-ui.md`](adr/0042-backend-driven-ui.md). The UI does not hardcode dynamic catalogs (RBAC permission catalog, module-catalog, status enums, selector keys) - the backend gives them catalog endpoints (identifiers + machine metadata, human-label/i18n on the UI with fallback to identifier); the border "does not affect the acceptance of the request by the backend and the backend-side does not grow." Enters `GET /v1/permissions`.

### [ADR-043. Voyage - unified batch run.](adr/0043-voyage.md)

Moved to [`docs/adr/0043-voyage.md`](adr/0043-voyage.md). Voyage is a single top-level entity of a batch run (batch unit - Leg), absorbing Tide ([ADR-040](adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) + ErrandRun ([ADR-041](adr/0041-errandrun.md)) + classic scenario-run: discriminator `kind` (`scenario` - batch N incarnations, per-incarnation state-commit B1 | `command` - batch N hosts, state is not touched), tables `voyages`/`voyage_targets`, target selection from RBAC scope (not invocation-override), RBAC-by-`kind`, audit `scenario_run.*`/`command_run.*`, failover claim+lease (`reclaim_voyages` default-ON); includes amendments two `batch_mode` (`barrier`/`window`) + batch strategies + string `batch`/`max_failures` + `POST /v1/voyages/preview`.

### [ADR-044. Choir - named topology of hosts within the incarnation](adr/0044-choir.md)

Moved to [`docs/adr/0044-choir.md`](adr/0044-choir.md). Choir - first-class named group of hosts within one incarnation (topological "party"), Voice - SID membership in Choir (triple `(incarnation_name, choir_name, sid)`): three DIFFERENT layers (membership/coven/Choir are not duplicated), Choir absorbed `spec.hosts[].role` and, since the amendment 2026-07-30 (NIM-330), `voice.role` is its ONLY source (the spec fallback and the field are removed), separate PG tables `incarnation_choirs`/`incarnation_choir_voices` (NOT `incarnation.state`), E1 targeting via `where:` + accessors `soulprint.*.choirs`, keeper-side core module `core.choir`; implemented (S-T2…S-T6 part(1b)). Amendments: precedence-role + multi-Choir-conflict; scenario-driven layout of hosts by batch + NULL-vs-default semantics (Choir not set → `NULL` in the database / empty `choirs[]`, NOT default group - empty state is more honest than default; per-shard layout in scenario deferred to mongo, redis cluster lives in one coven `incarnation.name`); **2026-07-30 (NIM-330)** `incarnation.spec.hosts[]` REMOVED whole together with `PATCH /v1/incarnations/{name}/hosts`, the rights `incarnation.update-hosts`/`incarnation.update` and the audit event `incarnation.hosts_updated` - "unset → empty role" becomes the only rule, and a role is declared in a scenario by `core.choir.present` (`on: keeper`) or day-2 by `POST .../choirs/{choir}/voices`.

## Topology

```
                          ┌─────────────────────────┐
                          │      Operator / CI      │
                          │  OpenAPI · MCP · gRPC   │
                          │ (CLI is a thin wrapper) │
                          └─────────────┬───────────┘
                                        │  mTLS
                                        ▼
   ┌──────────────────────────────────────────────────────────────┐
   │                 Keeper cluster (HA, stateless)                │
   │     ┌────────┐   ┌────────┐   ┌────────┐                     │
   │     │ keeper │   │ keeper │   │ keeper │   …  N instances     │
   │     │  K1    │   │  K2    │   │  KN    │                     │
   │     │ + push │   │ + push │   │ + push │                     │
   │     └───┬────┘   └───┬────┘   └───┬────┘                     │
   │         │            │            │                          │
   │         └────────────┼────────────┘                          │
   │                      │                                       │
   │            ┌─────────┴──────────┐                            │
   │            ▼                    ▼                            │
   │      ┌──────────┐         ┌─────────────┐                    │
   │      │  Redis   │         │  Postgres   │                    │
   │      │ cache /  │         │  souls,     │                    │
   │      │ lease /  │         │  soul_seeds,│                    │
   │      │ pub-sub  │         │  destiny,…  │                    │
   │      └──────────┘         └─────────────┘                    │
   └─────┬─────────────────────────────────────┬──────────────────┘
         │ pull (Soul initiates)                │ push (Keeper initiates)
         │ gRPC bidi + mTLS                     │ SSH (Vault SSH CA / static / Teleport — open Q)
         ▼                                      ▼
   ┌──────────┐                          ┌──────────┐
   │   soul   │                          │   host   │      Managed hosts:
   │ (daemon, │                          │ (no      │      same entry in the
   │  agent   │                          │  agent,  │      `souls` registry,
   │  mode)   │                          │  push)   │      different transport.
   └──────────┘                          └──────────┘

In parallel:
   ┌──────────────┐
   │  soul-lint   │  offline validation of Destiny + service vars
   │  (CI / dev)  │  on the developer side, without Keeper
   └──────────────┘
```

### Roles of binaries

- **`keeper`.** Central server. Stores the Souls registry (Postgres), RBAC policies, validates and renders Destiny, issues Souls commands, aggregates Soulprint and run results, sets gRPC + OpenAPI + MCP. Contains module `keeper.push` (SSH delivery). Integrates with Vault for service-vars secrets and for CA (issuing SoulSeed certificates).
- **`soul`.** Agent daemon on the managed host; runs as a service and runs continuously. Raises gRPC bidi stream to Keeper, executes received commands, collects Soulprint, and sends results. No Keeper server code, no outgoing traffic except to your Keeper and to explicitly allowed resources. There is no local admin endpoint in MVP ([open Q No. 8](#current) - post-MVP); admin operations on Soul-host in MVP - SIGHUP for hot-reload `soul.yml` + local shell access to logs/metrics.
- **`soul-lint`.** Offline linter for Destiny and service vars. Parses, renders, validates according to the schema, runs static analysis (non-existent modules, dependency cycles, typos in Soulprint targets). Runs locally and in CI, does not require Keeper or a network.

## Soul Life Cycle and Soul Registry

Soul's identity is built on three architecturally fixed entities:

- **SID = Host FQDN** (not UUID). This automatically gives dedup when the agent is reinstalled on the same host; the downside is that renaming the FQDN means migration (old Soul → new, in-place rename is not supported).
- **SoulSeed** - a pair (mTLS certificate + private key) with which Soul is authenticated when connecting to Keeper. Released via CSR upon first connection (the private key never leaves the host), regularly rotated via live stream. The database stores **only `fingerprint`**, without PEM and private keys; The main protection is the CA private key in Vault.
- **Coven** - an arbitrary label/tag for logical association of Souls (by data center, by role, by environment). Used in RBAC, Destiny targeting, potentially in balancer routing (open Q "LB-1").

The Soul registry in Postgres is divided into three tables: `souls` (one entry per host, statuses `pending` / `connected` / `disconnected` / `revoked`), `bootstrap_tokens` (one-time onboarding tokens - invariant `UNIQUE (sid) WHERE used_at IS NULL`, The plain token is not stored in the database, only `token_hash`), `soul_seeds` (certificate history - invariant `UNIQUE (sid) WHERE status='active'`).

Full table schemas, status transition diagram, SQL transaction for presenting a bootstrap token, burning algorithm on the Soul side, operator recommendations (`mode 0400`, systemd `LoadCredential=`), SoulSeed rotation procedure, revoke and host renaming - in [`docs/soul/identity.md`](soul/identity.md) and [`docs/soul/onboarding.md`](soul/onboarding.md). The corresponding registries in Postgres are also described in [`docs/keeper/storage.md`](keeper/storage.md).

## Soul connection: priority and failback

Applies to **agent** mode (`transport: agent`). Push mode uses a different model - Keeper itself initiates an SSH session to the host, see [Push mode](#push-mode-keeperpush).

In the Soul config, a list of Keeper endpoints with a numeric field `priority` is specified: a smaller number is more preferable (like DNS MX, systemd, ip route), the default is `priority: 1`. Between priorities - sequentially (1 → 2 → 3); within one priority - sequentially with order randomization (shuffle on each attempt): this loads the endpoints evenly and is easier for correct implementation than a race of parallel handshake.

Failback (return to a more preferred priority after switching down) - maximum once per `failback.interval`, with a random jitter shift `±spray` against herd effect. The base interval is maintained. Guarantee - at any moment Soul maintains exactly one active stream to one Keeper; zero-downtime switching (a new stream is opened, then the old one is closed).

YAML config of block `keeper:` (endpoints, retry, failback), full specification of parameters and step-by-step algorithm - in [`docs/soul/connection.md`](soul/connection.md). The location of this block in general `soul.yml` is in [`docs/soul/config.md`](soul/config.md).

## Push mode (`keeper.push`)

Host management **without installing a Soul agent**: Keeper goes to the host via SSH, performs Destiny steps, takes the results, leaves nothing permanent except for the changes that Destiny describes. Push mode is a **module inside `keeper`** (ADR-004), not a separate binary: the server function (RBAC, auditing, issuing SSH credentials via Vault) logically sits in Keeper.

Properties important at the architectural level:

- **Unified registry.** Push host - record in the same table `souls` with `transport: ssh`; push↔agent migration - changing one field, the history is not lost. The SoulSeed table is not used for push hosts (there is no mTLS identity - its role is played by the SSH side).
- **Same `soul` binary.** Same artifact as the pull daemon, run one-time as `soul apply` (stdin = rendered `ApplyRequest` as protojson - `apply_id` + `RenderedTask[]` after Keeper-side render phases, ADR-012(d); stdout = NDJSON stream `TaskEvent` + final `RunResult` as protojson; exit 0 for `RunResult.status==success`, 1 otherwise). Raw Destiny and service vars do not reach the push host - Keeper renders on its own, Soul does not resolve Vault.
- **SHA-256 cache on the host.** The binary and modules are cached in `/var/lib/soul-stack/{bin,modules}/`; a repeated run does not download anything.
- **SSH authentication - pluggable provider.** Contract `SshProvider` (Vault SSH CA / static key / Teleport - all fit under it), a specific set of required implementations - [open Q SSH-2](#current).

Full analysis (model, push↔agent migration, SSH authentication, operator interface `POST /v1/push/apply`, running algorithm, layout `/var/lib/soul-stack/`, key properties) - in [`docs/keeper/push.md`](keeper/push.md). The normative specification of the HTTP façade and request/response schemas is in [`docs/keeper/operator-api.md`](keeper/operator-api.md). Host layout and cache are in [`docs/soul/modules.md`](soul/modules.md). The `SshProvider` contract and plugin directory are in [`docs/keeper/plugins.md`](keeper/plugins.md).

## Module model

This section applies to both **pull** and **push** transports. This is a single model: the `soul` binary applies Destiny steps, the step execution models are the same regardless of how the binary ended up on the host.

### Structure

- **Core modules** - statically built into the `soul` binary. Cover the vast majority of Destiny: the exact list is fixed [ADR-015](#adr-015-core-mvp-modules-exact-list) – 18 Soul-side (`pkg`/`file`/`directory`/`service`/`user`/`group`/`exec`/`cmd`/`cron`/`mount`/`git`/`archive`/`sysctl`/`url`/`line`/`repo`/`firewall`/`http`; `directory` split from `file` per Amendment 2026-07-17, `service` also gains `disabled`/`masked`) + 3 Keeper-sides (`soul.registered`/`cloud.provisioned`/`vault.kv-read`, the last two are [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read)). They work always, everywhere, and do not require additional delivery. By addressing, all built-in modules live in namespace `core`. Files from templates are rendered by `core.file.rendered` (see [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files)) - a separate module `core.template` is NOT allocated.
- **Custom modules** - executable artifacts under `/var/lib/soul-stack/modules/<alias>/`, one executable per slot. The `soul` binary runs one as a sub-process over the stdio protocol (see below), naming the module it wants as a **subcommand**; one artifact serves several modules. By addressing they live under their registration alias (`redis`, `acme`, `community`, …).

> **Soul-side vs Keeper-side core modules.** The vast majority of core modules (`pkg`, `file`, `service`, `user`, `exec`, `template`, …) are **Soul-side**: executed on the host `soul`-binary. Some of the core modules are **Keeper-side**: they operate on the keeper's registries (Postgres souls+coven, Redis cache, logs) and are executed on the keeper itself. The first Keeper-side core is `core.soul.registered` (SID binding to coven tags of the souls registry; full specification is [`docs/keeper/modules.md`](keeper/modules.md)). **The side is decided by the ADDRESS** — the two core registries are disjoint — and [ADR-0087](adr/0087-task-side-derived-from-module-address.md) makes the routing derive from it, retiring `on: keeper` on a core address; implemented in NIM-749, so `on:` now means only "which covens" ([`docs/scenario/orchestration.md §3`](scenario/orchestration.md)). The addressing (`<namespace>.<module>.<state>`) and the SoulModule contract are the same for both parties.

When applying Destiny step `soul`:
1. parses the module name according to the scheme `<namespace>.<module>.<state>` (see "Addressing modules");
2. for a built-in core module calls the implementation of `<module>.<state>` directly, in the process;
3. otherwise takes the single executable in the alias's slot directory; no slot - validation error (`soul-lint` catches this before running);
4. launches a sub-process, transfers state and parameters via gRPC-stdio, reads events as a stream (see "Modules Protocol").

### Module addressing

The module is addressed in three levels, through a dot:

```
<alias>.<module>.<state>
```

| Level | Meaning | Examples |
|---|---|---|
| **alias** | The **registration alias** — the name the *operator* gave the artifact in `keeper.yml::plugins.*[].name`. Also the unit of distribution and caching (see [module-collections.md](module-collections.md)). | `core` (built into `soul`), `redis`, `acme`, `community` |
| **module** | The control object is "what the module is about." (`pkg`, `service`). | `pkg`, `file`, `service`, `user`, `exec`, `template`, `haproxy` |
| **state** | The desired state is "how the object should be." Declarative noun (not an imperative verb). | `installed`, `absent`, `latest`, `present`, `running`, `stopped`, `restarted`, `enabled` |

> **Level 1 is the operator's name, not the publisher's** ([ADR-020(p)](adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name), NIM-377). The artifact carries no `namespace:`, no `name:` and no self-identity of any kind: the same bytes registered as `redis` answer `redis.acl.present`, and registered as `redis-community` answer `redis-community.acl.present`, with no rebuild. Two publishers of one subject cannot collide, because the operator names both. `core` is [reserved](naming-rules.md#reserved-namespace-names) and cannot be claimed.
>
> **The full addressing model is [NIM-376](adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name), a separate open ticket** — reserved-name enforcement across both surfaces, the `required_modules:` grammar and the alias↔source relationship are fixed there, not here.

Addressing **declarative**: the third level is a *desired state*, not an *action*. `core.pkg.installed` reads "package installed" - not "install package". This fits better with the philosophy of destiny ("what should be"), gives a natural language to scenario writers.

**Each state is a separate input schema of the module.** `core.pkg.installed` accepts `name`, `version`; `core.pkg.absent` - only `name`; the parameters do not intersect. The module manifest declares its full list of states and the parameter scheme for each, see "Module Manifest" below. Linter catches:
- unknown module state → error;
- wrong set `params:` for a specific state → error;
- divergent parameter types → error.

**Non-stateful modules.** Modules that do not have a natural "state" (`exec`, `cmd`) use the third-level **verb form**: `core.exec.run`, `core.cmd.shell`. This is a deliberate exception to the declarative rule: for a statement that has no state, it is useless to invent "this is the command I want in a neglected state." The module manifest marks the state form or verb form, the linter checks this.

**The third level is required.** The entry `core.pkg` without `state` is a validation error, no defaults. Explicit is better than implicit: the operator does not have to "guess" what the default is.

**`required_modules:` in destiny.yml is a declaration of only custom modules** in a two-level form (`<alias>.<module>`): "this destiny requires that the family of custom modules `acme.haproxy`, `acme.myapp`, ..." be available on the host. All state forms inside these modules are available automatically. Specific state instances are called by a 3-level form in `tasks/main.yml` (see ["Task Structure in `tasks:`"](#destiny-task-structure) below).

**Core modules are not listed in `required_modules:`** - they are statically built into the `soul` binary and are always available. If destiny uses only `core.*`, the `required_modules:` block is omitted entirely.

**Example (`tasks/main.yml` destiny with custom modules - top-level task list, without wrapper):**

```yaml
# destiny-<name>/destiny.yml declares required_modules:
#   required_modules: [acme.haproxy, acme.myapp]

# destiny-<name>/tasks/main.yml:
- name: Install redis-server package
  module: core.pkg.installed                # core: in required_modules: do not write
  params: { name: redis-server, ref: v7.2.4 }

- name: Render /etc/redis/redis.conf from template
  module: core.file.rendered                    # ADR-010: .tmpl renderer does core.file.rendered
  params:
    path: /etc/redis/redis.conf
    template: templates/redis.conf.tmpl
    vars:
      maxmemory: "${ input.maxmemory }"
    mode: "0640"

- name: Ensure redis-server is running and enabled at boot
  module: core.service.running
  params: { name: redis-server, enabled: true }

- name: Restart redis-server
  module: core.service.restarted
  when: input.action == 'restart'
  params: { name: redis-server }

- name: Reload haproxy after config change
  module: acme.haproxy.reloaded               # custom: declared in required_modules:
  params: { config_path: /etc/haproxy/haproxy.cfg }
```

### [ADR-045. Param-DSL modules - typed input fields for the UI form Run Command](adr/0045-param-dsl.md)

Moved to [`docs/adr/0045-param-dsl.md`](adr/0045-param-dsl.md). Brings modular input-DSL (`plugin.InputParamDef` / `shared/coremanifest`) closer to scenario/destiny `input:` - `enum` / `format: sid` + `source:` / `pattern` / `items` under `list`/`map` - to UI Run Command built a typed form for a module (core + plugin), without introducing a third DSL.

### Destiny task structure

The contents of destiny live in **`destiny-<name>/tasks/main.yml`** as a top-level YAML task list (without the `tasks:` / `steps:` wrapper), and not in `destiny.yml` itself. Root `destiny.yml` - manifest only (`name`, `description`, `input`, opt. `required_modules`); `tasks/main.yml` - entry point, with the ability to connect `include: <file>.yml` neighbors inside the same folder `tasks/`, or one subdirectory down (`include: <dir>/<file>.yml`).

One list element is a call to one module with parameters and optional binding. Task fields (`name`, `module`, `params`, `when`, `register`, `include`; a task-level `output:` is refused - `output_unsupported`, see §9 there), task naming convention (capital letter, imperative, English) and rules `include:` - fixed in **[`docs/destiny/tasks.md`](destiny/tasks.md)**. The architectural section here does not duplicate the field table, so that there is no drift between two sources.

Destiny folder layout and format `destiny.yml` - in [`docs/destiny/manifest.md`](destiny/manifest.md). For a complete overview of the concept of destiny, see [`docs/destiny/`](destiny/README.md).

### Module protocol - gRPC over stdio (HashiCorp-style)

Model **B (gRPC-stdio)**: same technique as Terraform providers, Vault plugins, Packer plugins. The standard format for the handshake line, lifecycle and versioning is [ADR-020](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format); specification file - [`docs/keeper/plugins.md`](keeper/plugins.md). General view:

- When started, the module prints to stdout a handshake string with the protocol version and the address of the local socket (Unix socket, for example).
- Next, `soul` and the module communicate via gRPC via a socket. Service on the module side:

  ```protobuf
  service SoulModule {
    rpc Validate(ValidateRequest)       returns (ValidateReply);
    rpc Plan(PlanRequest)               returns (stream PlanEvent);
    rpc Apply(ApplyRequest)             returns (stream ApplyEvent);
  }
  ```

  The plugin's schema document is generated and stamped into the artifact, normatively described in [ADR-020(o)](adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name); there is no `Manifest()` RPC — the host must be able to read the schema without executing an unapproved binary.

- gRPC-stream provides native progress reporting for long operations (`PlanEvent`, `ApplyEvent`).
- The module ends with a graceful shutdown signal under the same contract.

### Module schema document

A module declares itself as a **`module.Def` value in Go**, and the **schema document** — canonical JSON — is *generated* from it, stamped into the artifact and written to `dist/schema.json` ([ADR-020(n)/(o)](adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name), NIM-377). **There is no hand-written `manifest.yaml`.** The format is one shape for every kind (`soul_module` / `cloud_driver` / `ssh_provider` / `soul_beacon`) with a `kind:` discriminator; the normative source is [`docs/keeper/plugins.md → Schema document`](keeper/plugins.md#schema-document), which carries the field tables and the authoring example — deliberately not duplicated here.

Two properties matter at this level:

- **The artifact names no subject.** No `namespace:`, no `name:` — level 1 comes from registration (see [Module addressing](#module-addressing) above), and level 2 is the module's own name inside the artifact's `modules[]`.
- **The document is readable without executing the artifact.** Keeper reads it at `plugin.allow`, when the binary is not yet approved; running it to ask what it is would defeat the gate. It is stamped as a trailer and found by seeking from the end of the file.

The document is used by `soul-lint` for **local** validation of Destiny: unknown modules, unknown module state, wrong parameters for a specific state — without launching the module itself ([ADR-009](adr/0009-scenario-dsl.md), [ADR-020](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format)).

**`required_capabilities` / `side_effects` are disclosure to the operator before approval, not controls** — nothing enforces them; the one real control is that the operator approves a specific sha256 and the host refuses to exec a differing digest ([ADR-020(r)](adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)).

### Languages and module assembly

- **Native (Go, Rust, C/C++)** is the recommended path. One static binary, compact (5–25 MB), without runtime dependencies. For Go, we supply a first-class SDK (`soulstack/sdk-go`) with an implemented handshake and protocol - the author of the module only has to write the business logic.
- **Python** - supported via bundler (PyInstaller, **Nuitka recommended**). Module size 15–80 MB depending on dependencies. Heavy dependencies (`pandas`, `numpy`, ML stack) for config management modules are an indicator of overcomplicated design.
- **Node.js / Ruby / other interpreted languages** - technically they work (gRPC-stdio is a pure protocol), but the first-class SDK is not supplied; collect via pkg / Tebako / analogues. Files are larger (40–120 MB).

Soul Stack accepts "an executable that does a gRPC-stdio handshake." What's under the hood is the choice of the module author.

### Host behavior and cleanup

`/var/lib/soul-stack/{bin,modules}/` layout, SHA-256 module cache, pull behavior (the daemon pulls a custom module via `core.module.installed`) and push (Keeper transfers all registered modules en masse), local cache clearing via TTL, separate operation `keeper.push.cleanup` when revoke the host - collected in [`docs/soul/modules.md`](soul/modules.md).

Limit of responsibility: Reaper on the Keeper side works **only on Postgres** and does not access hosts via SSH - otherwise you would have to give it SSH rights to all the Souls (bad for blast radius). Host cleaning is the task of the Soul daemon (pull) or `keeper.push` itself (push).

### [ADR-046. Cadence - regular launches (scheduled/recurring Voyage)](adr/0046-cadence.md)

Moved to [`docs/adr/0046-cadence.md`](adr/0046-cadence.md). Cadence is a first-class schedule entity (table `cadences`, model b - survives runs), which in time **spawns** the regular [Voyage](adr/0043-voyage.md) (back-link `voyages.cadence_id`): repetition rule `interval`|`cron`, three `overlap_policy` (`skip`/`queue`/`parallel`), anchored recalculation `next_run_at` + anti-storm missed-slot, two-level RBAC-guard (`cadence.*` + Voyage-permission by `kind`), audit `cadence.*` (`source: background`); executor - [Conductor](adr/0048-conductor.md) (amendment 2026-06-02); includes floor min-period 30s (Pass B).

### [ADR-047. Purview - scoped RBAC visibility of nodes (role default_scope + extended selector)](adr/0047-purview.md)

Moved to [`docs/adr/0047-purview.md`](adr/0047-purview.md). Purview - scoped RBAC node visibility resolver (`ResolveScope`/`ResolvePurview`/`HoldsAction`): dimensions coven/regex/soulprint/state, role `default_scope` + per-perm override, default-deny with `*` exception, scoped visibility souls/incarnations-list (S3b) and target ∩ Purview for Voyage command path (S4, security-fix); fail-closed-invariant. Includes Amendment ADR-049 (Synod).

### [ADR-048. Conductor — leader-elected executor of Cadence schedules](adr/0048-conductor.md)

Moved to [`docs/adr/0048-conductor.md`](adr/0048-conductor.md). Conductor is a separate leader-elected keeper-side subsystem (Redis-lease `conductor:leader`, generic `leaderloop`, its own adaptive tick-interval `cadence_scheduler`, independent of `reaper.interval`), executing the spawn of due-[Cadence](adr/0046-cadence.md)-schedules; removal of spawn from the Reaper rule `spawn_due_cadence` (Reaper loses `action: spawn`, returns to a clean cleanup domain), spawn semantics are preserved verbatim, audit-source remains `background`, default-ON if Redis is present; includes amendment "Adaptive interval" (profile "Quiet" + floor min-period).

### [ADR-049. Synod - group of archons](adr/0049-synod.md)

Moved to [`docs/adr/0049-synod.md`](adr/0049-synod.md). Synod is a group of archons that bundles a set of roles (model **Archon → Synod → Roles**); three PG tables `synods` / `synod_operators` / `synod_roles`, effective roles = direct ∪ via Synod assembled in an enforcer snapshot assembly; security invariants (least-privilege + self-lockout) deploy Synod; permission-family `synod.*` (8, incl. `synod.update`); amendit [ADR-014](adr/0014-operator-identity.md) / [ADR-028](adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) / [ADR-047](adr/0047-purview.md).

### [ADR-050. Tempo — per-AID rate-limiting write-API](adr/0050-tempo.md)

Moved to [`docs/adr/0050-tempo.md`](adr/0050-tempo.md). Tempo - per-AID frequency limiter for operator calls to resolver-heavy write endpoints (MVP coverage `POST /v1/voyages` + `/v1/voyages/preview`, single bucket `voyage_create`): token-bucket in Redis (Lua, key `tempo:<aid>:<bucket>`), fail-OPEN with Redis-down (realized trade-off, like Toll), 429 + `Retry-After` + problem-type `tempo-exceeded`, default `rate: 10 / burst: 20`, hot-reload config block `tempo:`; the third anti-DoS layer next to body-limit and [Toll](#adr-038-toll--cluster-wide-detector-of-mass-outflow-of-souls).

### [ADR-051. Operator API codegen: OpenAPI → Go-types (oapi-codegen), types-only → strict](adr/0051-operator-api-codegen.md)

Moved to [`docs/adr/0051-operator-api-codegen.md`](adr/0051-operator-api-codegen.md). **SUPERSEDED [ADR-054](#adr-054-operator-api---pivot-to-code-first-go-types--openapi-via-huma-v2); implementation demolished 2026-06-13 (HEAD `fde65bf`).** Described spec-first: form source = `openapi.yaml`, Go types were generated from it by oapi-codegen v2 (`types-only`, package `keeper/internal/api/oapi`, `types.gen.go`), `proto/operator/v1` deprecated (amend ADR-011); type-alias pattern + converter-at-the-border (categories A–D, byte-passthrough JSONB D), "zero wire-change" invariant, spec downgrade to `3.0.3`, Phase 2 (strict) - "bridge". **Framework removed** (oapi package / `oapi_strict.go` / source manuscript / `gen-api`/`check-gen-api`), form source is now huma Go types ([ADR-054](adr/0054-openapi-code-first.md)).

### [ADR-052. Herald + Tiding - notifications about running events](adr/0052-herald-notifications.md)

Moved to [`docs/adr/0052-herald-notifications.md`](adr/0052-herald-notifications.md). **Herald** - notification delivery channel (PG registry `heralds`: `type`-enum `webhook` in MVP, `config` JSONB, `secret_ref` vault-ref), **Tiding** - subscription rule (PG registry `tidings`: `event_types[]` with area-glob `scenario_run.*`, filters `only_failures`/`only_changes`, selectors `incarnation`/`cadence`, FK on Herald). Scope MVP - Run Events ONLY (`scenario_run.*`/`command_run.*`/`voyage.*`/`incarnation.run_completed`/`cadence.*`); Host beacon events via [Oracle](adr/0030-vigil-oracle.md) - rejected (separate entry). Mechanics - tap (multi-writer decorator on top of `audit.Writer`, point [ADR-022(f)](adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) → notification-dispatcher → at-least-once webhook-delivery of claim-queue worker (parity [VoyageWorker](adr/0043-voyage.md)), retry+backoff, attempt statuses in Redis (hot→Redis), terminals `herald.delivered`/`herald.failed` in audit. Security invariants: https-only + deny private IPs (opt-out `allow_private`, pattern `core.url`) + deny redirect (`util.CheckRedirect`) + timeout; `secret_ref` vault-ref only; payload without resolved secrets + `MaskSecrets`. Contracts additive: OpenAPI `/v1/heralds`+`/v1/tidings` (greenfield full-strict, [ADR-051 S6](adr/0051-operator-api-codegen.md)), RBAC `herald.*`/`tiding.*`, MCP `keeper.herald.*`/`keeper.tiding.*`, UI tab "Notifications".

### [ADR-053. Infrastructure dependency tiers](adr/0053-dependency-tiers.md)

Moved to [`docs/adr/0053-dependency-tiers.md`](adr/0053-dependency-tiers.md). Classification ADR: the mandatory contour of the Keeper cluster is **three components PostgreSQL + Redis + Vault**, all three **fail-fast** at the start. Vault hard-required at three points: vault client at start (`setupVault` → `NewClient` → `Ping`), JWT signing-key (auth operators, [ADR-014](adr/0014-operator-identity.md)), souls-PKI (release/rotation of SoulSeed mTLS via Vault PKI). OPTIONAL-with-degradation (the feature is clearly disabled, Keeper does not crash): Sigil signing-key (fail-closed), Augur (default-deny), Herald `secret_ref` (without signature), push host-CA (push disabled), metrics basic-auth, OTel-export. Rule for NEW features: new mandatory dependencies - only through the user's decision; optional ones are required to degrade clearly. Rejected: no-Vault mode (file/env auth-key + built-in CA - breaks the security premise "secrets do not materialize on the Keeper cluster disk"; CA-private on disk/in PG, in multi-keeper distribution of private among nodes; rejected by the user) and SecretProvider abstraction (premature). Operations note: mandatory Vault ≠ heavy cluster - single-binary with file-storage comparable to Redis (recipe in [infra.md](adr/../operations/infra.md)).

### [ADR-054. Operator API - pivot to code-first (Go-types → OpenAPI) via huma v2](adr/0054-openapi-code-first.md)

Moved to [`docs/adr/0054-openapi-code-first.md`](adr/0054-openapi-code-first.md). Replaces [ADR-051](#adr-051-operator-api-codegen-openapi--go-types-oapi-codegen-types-only--strict): Operator API form source inverted to **Go-types → OpenAPI** (code-first, huma v2 + humachi), derivative spec. **FULL-TYPED** handlers (typed `Body`/output + extracted domain `XTyped`); validation `required`/`enum`/`unknown→400` - native huma from struct tags; security saved (manual mount to chi group with RBAC/Audit linkage, global problem+json-override, per-domain audit-guard; middleware-audit domains - option B `UseMiddleware`). Tiers pattern: full-typed write, PATCH-presence via `Optional[T]`, read-with-typed-query (`400/422`-contract saved). **IMPLEMENTATION COMPLETED 2026-06-13 (HEAD `fde65bf`)**: all ~19 domains handler-native, served-spec = runtime huma-dump (`HumaFullSpecYAML`, aggregator `huma_full_spec.go`), committed `docs/keeper/openapi.yaml` - derived (`make gen-openapi` / drift-guard `make check-openapi`), native enum directory `huma_enums.go`; oapi-codegen-framework and source-manuscript demolished. **Amendment 2026-06-15:** the visual OpenAPI viewer `GET /docs` (Stoplight Elements, go:embed-assets, mechanism A is a public shell framework, the spec for JWT) was raised above the derived spec, and `GET /openapi.yaml` was replaced by security public → for JWT (no anonymous intelligence of the API surface; UI-vendor/`soulctl` take the committed file). Meta routes - [operator-api.md → Health / Meta / Docs](keeper/operator-api.md#health--meta--docs).

### [ADR-055. Embed UI bundle - optional single-binary keeper with UI on `/ui`](adr/0055-embed-ui-bundle.md)

Moved to [`docs/adr/0055-embed-ui-bundle.md`](adr/0055-embed-ui-bundle.md). **Amends [ADR-035](#adr-035-distribution-split--core-apicli-vs-web-ui):** Enables deferred embed-compat-shim as an optional **default-ON** embed UI for beta (single-binary onboarding), NOT a split distribution rollout. Keeper, through `go:embed`, carries the vendored UI build snapshot (`soul-stack-web`) to `keeper/internal/webui/assets/` (folder `assets/`, NOT `dist/` - the gitignore rule `dist/` would silently eat the bundle) and distributes it on the route `/ui` + `/ui/*` with SPA-fallback to `index.html`. Static **public** (parity `/docs`; `/v1` remains with JWT+RBAC+default-deny), toggle `web_ui_enabled` (`*bool`, `nil`→default-ON, explicit `false`→opt-out), shares listener `:8080` (NO new ports/systemd), binary growth ~+2–5 MB. Source-of-truth UI = companion-repo `soul-stack-web`; in core - only the collected artifact, sync `scenarios/sync-webui.sh` + drift-guard `make check-webui` (skip without companion, tracing plugin-template). Clarification of the ADR-035 "no HTML/CSS/TS" invariant: **sources** UI is still prohibited, **assembled static artifact** is allowed (as RapiDoc bundle `/docs`). The use case for go:embed-statics is [ADR-054](#adr-054-operator-api---pivot-to-code-first-go-types--openapi-via-huma-v2).

### [ADR-056. Staged-render - running the scenario as N ordered Passages](adr/0056-staged-render-passage.md)

Moved to [`docs/adr/0056-staged-render-passage.md`](adr/0056-staged-render-passage.md). The scenario run is executed as **N ordered Passages** (run phase = render → dispatch → barrier → register collection). Implements the promise of the canon [orchestration.md §4/§5](scenario/orchestration.md) probe→where: the task reading `register.X` (in `where:`/`apply: input:`/`params:`/`vars:`), is stratified in Passage **after** the probe issuing `X` (topological N-stage); render of the next Passage substitutes the per-host register of the previous ones. Closes doc-drift "keeper renders one up-front pass BEFORE probe → `where:` sees empty register." **`incarnation.state` commits ONCE after the last Passage** - barrier/state-commit-invariant §7 is not split; Passage is a task axis, not a commit axis. Reuse: `loadRegisterByHost`/`apply_task_register`/`SelectTaskRegistersByApplyID` (post-barrier register) as Passage-loop; `evalWhere`/`renderParams` already accept `in.Register`. Contract: proto only-add `passage` to `ApplyRequest`/`TaskEvent`/`RunResult`; PG-PK per-passage (option is fixed in S1); N `RunResult` to `(apply_id, sid, passage)`; old Soul under staged scenario → explicit-reject (`render_failed`, no hangup). **Amends [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation)** (per-task `serial:`/§8 → per-task dispatch via Passage), **[ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)** (dispatch "one ApplyRequest per host" → "N per host via Passage"), **[ADR-027](#adr-027-execution-model-apply---work-queue--claim-acolyte-pool-ward-claim)** (reclaim/Ward-claim granularity → per-passage). Closes [open Q No. 24](#open-questions) (per-task granularity `serial:`). Slice map S0–S5.

### [ADR-057. `state_changes` - ordered list of CRUD verbs](adr/0057-state-changes-crud-verbs.md)

> ⚠ **The verbs survived, the block did not.** [ADR-084](#adr-084-explicit-state-capture--corestateverb-replaces-end-of-run-state_changes) retired the scenario-level `state_changes:` section; each verb below is now an address of its own (`core.state.set` / `.add` / `.modify` / `.remove` / …) applied by the same engine at the step that names it. Read this entry for what a verb MEANS, not for where it is written.

Moved to [`docs/adr/0057-state-changes-crud-verbs.md`](adr/0057-state-changes-crud-verbs.md). The scenario's `state_changes` becomes an **ordered list of operations** (YAML list, not map). Each element is one **CRUD verb** (singular): **`set`** (rewriting the entire field, replaces the previous `sets`-map), **`add`** (add an element to the collection: map - `key:`+`value:`, list - `value:`+opt. `match:`; `on_conflict: skip|replace|error`, default `skip` = idempotent "add if not"), **`modify`** (`match:` + `patch:` - patch ALL matching, all-by-default), **`remove`** (`match:` - remove ALL matching), **`foreach`/`as`/`do`** (bulk fan-out, form literally from migration-DSL [ADR-019](#adr-019-state_schema-migration-dsl)). **Multiplicity is expressed by a `match` predicate** (CEL over the element), not by handles/flags; CEL bindings `elem` (list element/scalar), `key`/`value` (map record) on top of the full sets context (`input`/`incarnation`/`soulprint.self`/`register`/`vars`/`essence`). Opt. `expect: one|at_most_one|any` (default `any`) — runtime assertion of multiplicity `modify`/`remove` (hooked ≠ expected → `error_locked` before commit). Fuses: soul-lint WARN on constant-true/absent `match`; empty-match → no-op (idempotency). Operations are applied in the order of declaration to the intermediate state, one PG transaction, one `state_history`-snapshot, any fail → `error_locked` (barrier/state-commit-invariant [§7](scenario/orchestration.md) is not weakened). Collection type - from `state_schema`; per-RUN semantics (last-wins by SID, NOT per-host union). Fixes **latent bug**: `appends`/`modifies` ([ADR-009 §7.1](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation)) were no-op placeholders without a value source - `incarnation.state` did not grow (`add_replica`/`add_user`/`update_acl`). **`remove` (state_changes) ≠ `delete` (migration-DSL)** - intentionally different names (remove collection element vs remove schema path). Not entered: `clear`/`rename`/`move`/`upsert`/positional remove/paired `*_one`/`*_all`/flag `all:`. Transit (breaking): map form (`sets`/`appends`/`modifies`) is parsed for one release as DEPRECATED (dual-parse + soul-lint warn), then deleted. **Amends [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation)** (Grammar §7.1). **Amended by [ADR-084](#adr-084-explicit-state-capture--corestateverb-replaces-end-of-run-state_changes):** the verbs survive (extended with `present`/`append`/`unset`), the `state_changes:` block does not — a write is a `core.state.<verb>` keeper task landing at its own step, and `foreach` is dropped because a step has `loop:`. Normative spec - [`docs/scenario/orchestration.md §7.1`](scenario/orchestration.md#71-the-capture-verbs).

### [ADR-058. Federated Operator Authentication (Archon) - LDAP + OAuth2/OIDC](adr/0058-operator-auth-ldap-oidc.md)

Moved to [`docs/adr/0058-operator-auth-ldap-oidc.md`](adr/0058-operator-auth-ldap-oidc.md). Federated-login operators: external IdP **validated on Keeper** (LDAP search-bind `go-ldap/v3` / OIDC authorization-code flow with mandatory **PKCE (S256)**, discovery+JWKS-validation `id_token` via `go-oidc/v3`), then identity **mapped** to registry `operators` (AID) + RBAC roles from groups, after which an **internal JWT** is issued to the existing `jwt.Issuer` (ADR-014) in the HttpOnly+Secure+SameSite=Strict cookie `soul_session`. Auth-middleware/RBAC/MCP/OpenAPI remain JWT-based and **do not change**. Public endpoints outside `/v1`: `POST /auth/ldap/login`, `GET /auth/oidc/{login,callback}`; OIDC flow-state - cluster-shared on Redis (state→nonce/PKCE-verifier, single-use GETDEL, TTL 5m). Provisioning-policy `provisioning_allowed_methods` + `/v1/provisioning-policy` (perm `provisioning.read`/`provisioning.update`, problem `provisioning-method-disabled`); auto-provision writes the line `operators` (`created_via='ldap'`\|`'oidc'`, `created_by_aid=NULL`). Extension `auth_method` enum (`ldap`/`oidc`, only-add, migration 083); field `operators.created_via` + bootstrap index relax on `WHERE created_via='bootstrap'` (migrations 084/085) + seeding `archon-system` (086); config blocks `auth.ldap`/`auth.oidc`/`auth.rate_limit` (secrets - Vault `*_ref`, TLS-required). Anti-bruteforce login endpoints - LoginGuard (per-IP/per-username throttle+lockout over Redis, fail-closed, problem `auth-throttled`). **Amends [ADR-014](#adr-014-operator-identity-model-archon)** (`auth_method`/`created_via`), **[ADR-013](#adr-013-bootstrap-of-the-first-archon)** (bootstrap invariant on `created_via`), **[ADR-029](#adr-029-service-registry--postgres)**.

### [ADR-059. Pluggable audit sink — PG / Kafka / off](adr/0059-audit-sink-pluggable.md)

Moved to [`docs/adr/0059-audit-sink-pluggable.md`](adr/0059-audit-sink-pluggable.md). **Status proposed / deferred - design-only, no code in beta** (post-beta plan; synchronous PG-recording audit on 100k VM does not scale - user decision to upload to Kafka under the toggle switch, without in-app preview). Backend audit-upload - selecting an **implementation** of the existing abstraction `shared/audit.Writer` (the new interface is not introduced; `MultiWriter`-tap [Herald](adr/0052-herald-notifications.md) is saved), insertion - `setupAudit` in [`keeper/cmd/keeper/daemon.go`](../keeper/cmd/keeper/daemon.go). Config `keeper.yml → audit.sink: pg | kafka | off` (default **`pg`** = current PG-`audit_log` [ADR-022](adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention), mandatory circuit [ADR-053](adr/0053-dependency-tiers.md) is intact; `kafka` - new sink with `brokers`/`topic`/`acks=all`, secrets vault-ref; `off` - conscious no-unloading, intelligent log). Both implementations in `keeper/internal` - `shared` remain **pgx-free and Kafka-free** ([ADR-011](adr/0011-go-layout.md)). Guarantees: at-least-once `acks=all`, degradation when Kafka is unavailable - **fail-closed** (audit compliance is critical, events cannot be lost), downstream deduplication by `audit_id` (ULID PK). Write guarantee - two candidates (open): transactional outbox (stronger, but PG-write remains) vs direct-producer + durable-fallback. Tier: Kafka is strictly **OPTIONAL-with-degradation**, NOT 4th required ([ADR-053](adr/0053-dependency-tiers.md)). Switching sink - **restart-required** (pattern `web_ui_enabled` [ADR-055](adr/0055-embed-ui-bundle.md)). Replaces the Redis-Stream version of the audit-scaling backlog; batched-INSERT remains (cheaper alternative). **Hard dependency BEFORE implementation (open question):** `changed_tasks`/`incarnation.run_completed` ([ADR-052 §k](adr/0052-herald-notifications.md)) and `GET /v1/audit` today derive data using an SQL query for `audit_log` in PG - with `sink: kafka` without PG they silently break (Herald-notifications + audit-reading); an alternative source of events is needed (in-memory run-aggregate / hybrid sink / Kafka projection). Working name `audit sink`; thematic **Chronicle** is an alternative candidate, NOT recorded in [naming-rules.md](naming-rules.md). **Amends [ADR-022](adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)** (index: PG remains default + source-of-truth FOR THE TIME) **/ [ADR-053](adr/0053-dependency-tiers.md)** (OPTIONAL string).

### [ADR-060. Trait — operator-set key-value labels (host and incarnation)](adr/0060-traits.md)

Moved to [`docs/adr/0060-traits.md`](adr/0060-traits.md). **Trait** - operator-set key-value of label (scalar\|list value, e.g. `namespace: dba-ns` / `owners: [alice, bob]` / `product: aboba`), **separate axis next to flat Coven** (Option B; extension `souls.coven` to key-value rejected - would break [ADR-008](#adr-008-coven---stable-logical-tags-only) - flat label semantics, RBAC scope-pushdown `$1 = ANY(coven)` ([ADR-047](adr/0047-purview.md)) and predicates `'x' in soulprint.self.covens`). **R1 (2026-06-25): Trait RELOCATED per-soul → per-incarnation.** Source of truth - `incarnation.traits jsonb NOT NULL DEFAULT '{}'` + GIN (migration `088`, mirror `incarnation.covens`/`046`), operator-set in `incarnation.spec.traits` on create. Projected **MATERIALIZED** to `souls.traits` member hosts via **sync-hook** (`incarnation.SyncTraitsToHosts`, reuses `soul.BulkReplaceTraits`; sidebar - incarnation-create + bind host `core.soul.registered`) - **the hook is REMOVED by ADR-080, see the tail of this stub**. `souls.traits` (`087`) REMAINS as the host's **own** axis: read layer is reused without changes - projection `soulprint.self.traits` (registry, like `covens`/`choirs`, [ADR-018](#adr-018-soulprint-typed-mvp-scheme); `SoulprintFacts` proto and registration contract Soul→Keeper **do not change**), `topology.HostFacts.Traits`, soul-lint whitelist `traits`. Targeting - `where: soulprint.self.traits.<key>` (`soulprint` declared CEL `DynType`, [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files) - the key is resolved dynamically, without AST-rewrite): `traits.namespace == 'dba-ns'` / `'alice' in traits.owners`. **Transitional state:** per-soul bulk-write `POST /v1/souls/traits` (+ MCP `soul.traits-assign`) still works on `souls.traits` directly, BUT is overwritten by the projection at the next sync `incarnation.traits` - expected to relocate per-soul bulk → per-incarnation (+ deprecate, next slice). **RBAC-scope for traits on the incarnation dimension - UNLOCKED** R1 (implementation - next slice). **Amends [ADR-008](#adr-008-coven---stable-logical-tags-only) / [ADR-018](#adr-018-soulprint-typed-mvp-scheme).** **[ADR-080](adr/0080-label-inheritance-union.md) (2026-07-27, NIM-121) SUPERSEDES the R1 relocation** - the materialized projection is removed and Trait becomes an axis on BOTH levels; **[NIM-281](adr/0008-coven-stable-tags.md#amendment-2026-08-05-nim-281-a-label-is-never-inherited) (2026-08-05) then revokes the read-time union ADR-080 put in its place**, so the two levels are simply independent: `incarnation.traits` labels the incarnation, `souls.traits` labels the host, and neither reaches the other. See the ADR-080 stub below.

### [ADR-061. Single-run provision→onboarding→role: onboarding-await + mid-run re-resolve roster](adr/0061-onboarding-await-and-midrun-reresolve.md)

Moved to [`docs/adr/0061-onboarding-await-and-midrun-reresolve.md`](adr/0061-onboarding-await-and-midrun-reresolve.md). **One create-scenario** deploys an N-shard cluster from "nothing": provision N VM (`core.cloud.provisioned`, `on: keeper`, [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read)) → waiting for onboarding of created Souls → mid-run roster growth → applying redis role to already online hosts. Closes the blocker "`soulprint.hosts` - snapshot at the start, mid-run does not grow; `refresh_soulprint` is ignored; there is no onboarding barrier." Two abilities on existing `core.soul.registered` (**NOT** new module - user decision: barrier adjacent to registration; separate `core.soul.online` rejected as an extra entity). **(1) onboarding-await:** new input flags `await_online` (bool) / `await_timeout` (duration, required-when `await_online`) / `await_min_count` (int, opt, default = number of registered SIDs) / `await_poll_interval` (duration, opt, ~2s) - after register+coven the step blockingly polls the **Redis SID-lease** (`keeper/internal/redis/SoulsStreamAlive`, source of truth online - NOT PG `souls.status`, [ADR-006](#adr-006-cache-and-coordination---redis)) to `await_min_count`/timeout; **B1-strict** (online < min to timeout → step `failed` → fail-stop → `incarnation.state` not committed → `error_locked`); output `register.<name>` added `online[]`/`pending[]`/`satisfied`; ceiling `keeper.yml::max_await_timeout` (DoS-guard, fail-closed - exceeding → `failed`, not silent cutting). **list-SID:** `params.sid` accepts a string OR a list (the barrier aggregates presence across all SIDs in one step). **(2) mid-run re-resolve:** flag `refresh_soulprint` has been revived (was a stub `refreshed: false`) - after the success of the scenario-runner step, the incarnation roster will be re-solved before the NEXT Passage; **monotonous growth** (+hosts only, deleting mid-run is prohibited). The stability roster invariant is weakened: "stable within the Passage" (not the entire run). barrier/state-commit-invariant [ADR-009 §7](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) **NOT** weakened (state is committed once after the last Passage). **Stratify:** `refresh_soulprint: true` makes the task a passage-defining boundary ([ADR-056](adr/0056-staged-render-passage.md), symmetrical to the probe-emitter) - consumers `soulprint.hosts`/`on: [incarnation.name]`/`soulprint.self.*` leave for Passage strictly AFTER. **HA:** provision scenarios are recommended to be run via Voyage ([ADR-043](adr/0043-voyage.md), recovery closed); standalone staged-recovery of a long barrier - open. **S1 (`await_online`) implemented; S2 (Stratify-border) / S3 (actual re-resolve in run.go) - the contract is fixed, implementation is in separate slices.** **Amends [ADR-009 §7](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) / [ADR-056](adr/0056-staged-render-passage.md) / [ADR-006](#adr-006-cache-and-coordination---redis) / [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read).**

### [ADR-062. Named input types - reusable named input schemes via `types:` + `$type`](adr/0062-input-types.md)

Moved to [`docs/adr/0062-input-types.md`](adr/0062-input-types.md). Reused named input schemas instead of duplicating inline-`object` between service scenarios. Section **`types:`** in the service-level file `service/<name>/types.yml` — map `<PascalCase>` → schema in **the same** InputSchema-DSL ([docs/input.md](input.md)); reference **`$type: <Name>`** as a standalone field OR `items: {$type: <Name>}` for an array. Resolve **service-level** (NOT local-per-scenario, NOT cross-service); MVP = object + array-of-type + nesting type→type with **mandatory cycle-detection**; WITHOUT scalar-alias/generics/cross-service. Errors `input_type_unknown` / `input_type_cycle` / `input_type_duplicate` / `input_type_ref_conflict`. DTO `/v1/scenarios` resolves `$type` **backend-side BEFORE projection** (UI receives expanded inline schema) + annotation **`x-type: <Name>`** (forward-compat UI widget). **Replaces the unrealized work `$ref`/`schemas/`** - it has been removed from this document and [service/manifest.md](service/manifest.md). **Amends affected by `$ref` in [ADR-003](#adr-003-destiny-format-is-yaml-with-typed-schema-cuejson-schema) / [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation).**

### [ADR-063. core.bootstrap.issued / delivered — keeper-side onboarding tokens and SSH delivery](adr/0063-bootstrap-token-delivery.md)

Moved to [`docs/adr/0063-bootstrap-token-delivery.md`](adr/0063-bootstrap-token-delivery.md). The keeper-side base module now exposes two public states. **`core.bootstrap.issued`** atomically creates/checks `pending`, `transport=agent` Souls and issues one fresh token per ready-made VM SID without invoking CloudDriver; a repeat invalidates the prior unused token, while any already onboarded identity is refused. **`core.bootstrap.delivered`** consumes per-host output from either `issued` or `core.cloud.created`, installs if requested, writes the token through STDIN, redeems it with guarded `soul init`, and optionally starts the unit. Teleport dials by SID and therefore does not require `primary_ip`; direct SSH still does. Plaintext exists only in the current run register under the sensitive key `bootstrap_token`; state/audit/OTel/SSE/log projections never contain it. Batch issuance is transactional and delivery is B1-strict with SID diagnostics. Audit events are separate: `bootstrap.issued` and `bootstrap.delivered`, neither carries tokens. **Amends [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read) / [ADR-061](#adr-061-single-run-provisiononboardingrole-onboarding-await--mid-run-re-resolve-roster) / [ADR-015](adr/0015-core-modules-mvp.md).**

### [ADR-064. Secret write-path - receiving a plaintext secret from the operator, writing to Vault keeper-side](adr/0064-secret-write-path.md)

Moved to [`docs/adr/0064-secret-write-path.md`](adr/0064-secret-write-path.md). Dual-mode for receiving a secret in Herald- and Provider-CRUD: the operator passes **`secret`** (plaintext) **XOR** **`secret_ref`** (vault-path, current behavior) - to the UI radio "value / path". With plaintext Keeper **itself** writes the secret to Vault along the deterministic path `secret/<domain>/<entity>/<field>` (`vault.Client.WriteKV` - the same as `sigil.Introduce` / cert `issueMaterial`) → in Postgres puts only the internal ref `vault:<path>#<field>` (as sigil/warrant); plaintext **does not persist anywhere** (Vault only). With `secret_ref` - behavior is the same as now (Keeper does not write to Vault). **Generalization of the already existing keeper-side write-path** (`sigil.Introduce` / cert `issueMaterial` / `core.vault.kv-present`) to a new case - receiving plaintext **FROM the operator** (not generated by the system), not a new infra-code. **★ Security trade-off (conscious relaxation):** plaintext over the wire operator →Keeper breaks the premise "the secret does not leave the Vault" ([requirements.md](requirements.md)); relaxation for the sake of UX **confirmed by the user (2026-07-01)** WITH mandatory mitigation blockers: **(a)** TLS transport is required; **(b)** strict masking of all sinks (logs/audit/OTel/UI) + guard leak tests - field `secret` under `shared/audit.sensitiveKeyRe`, but **huma-request-body is NOT auto-masked** → explicit audit of body logging points; **(c)** plaintext non-persist; **(d)** vault-policy Keeper with write prefixes `secret/herald/*`/`secret/provider/*`. `update` rewrites to a stable path (idempotent). RBAC - **reuses** `herald.create`/`provider.create` (no new permission). Name - **DevOps field `secret`**, without new dictionary pattern. Scope MVP = **Herald** (webhook-secret + channel-token) + **Provider** (cloud creds); TLS-PEM operator **deferred** (ref in essence → pulls essence→PG migration). Rejected: pattern-name `Consign`/`Entrust`; `oneof`-contract form; permission `secret.write`; ULID-immutable path. **Status: accepted, implementation pending** (fixation of the decision in a document). **Amends [ADR-052](#adr-052-herald--tiding---notifications-about-running-events) / [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read).**

### [ADR-065. core.module.installed - delivery of SoulModule plugins to the Soul host (FetchModule + plugins.soul_modules)](adr/0065-core-module-installed.md)

Moved to [`docs/adr/0065-core-module-installed.md`](adr/0065-core-module-installed.md). Canonical channel for delivering custom modules (`soul-mod-*`) to the Soul host; **closes open Q No. 5** "where the registry of modules in Keeper lives." **Transport** - the third RPC in `service Keeper`: server-streaming **`FetchModule(PluginFetchRequest) returns (stream PluginChunk)`** (the same mTLS-listener as EventStream; artifact bytes travel **in a separate HTTP/2 stream** and do not choke the control-plane; **content-addressed** - Keeper gives ONLY bytes whose sha256 is in the active Sigil-admission `kind: soul_module`; authorization - mTLS peer-cert; guard-rails: `plugins.max_artifact_size_mb` + rate-limit parallel fetch per-SID; only-add by [ADR-012(c)](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)). **Byte register** - config-directory **`plugins.soul_modules[]`** (`{name, source, ref}`, symmetry `cloud_drivers`/`ssh_providers`) + existing plugingit-resolve into FS cache + Sigil-tolerance (review [ADR-026](#adr-026-sigil---plugin-integrity-keeper-signed-digest-index) without changes); **NO new repository** - PG = permissions (authority sha256), FS = bytes, git = origin ([ADR-007](adr/0007-versioning-git-ref.md)); HA - per-instance FS cache, miss slot → on-demand resolve or failure with retry; S3-artifact-store is a post-GA extension behind the fetch abstraction. **Params `core.module.installed`** (Soul-side, state `installed`): `name` (required, `<ns>.<name>`), `ref` (opt, **pin-reconciliation** - active tolerance must be on this ref, otherwise `failed`; NOT version selection); idempotency - sha256 of the existing binary == sha of the active Sigil → `changed=false`. **hot-register is MANDATORY in MVP** (thread-safe Rescan without restarting the daemon - otherwise `community.redis.*` in the SAME run after the install step does not work; restart = break EventStream = break of run); The beacon registry is NOT rebuilt during rescan (post-MVP). **Scenario** — amendment 2026-07-03: Keeper **synthesizes** Soul-side install steps from the explicit manifest declaration `service.yml::modules[]` (scenario-runner, after expanding include BEFORE Stratify; insertion before the first consumer of the module; explicit step `core.module.installed` with the same literal `params.name` disables synthesis - operator controls position/ref/when itself); validation-hint post-MVP is saved in the reverse direction (the module is used in tasks, but is not declared in `modules[]`). **Sigil-verify install-time:** allow-check BEFORE fetch (no tolerance → `module_not_allowed` to single network byte) + full verify (signature + `manifest_sha256`) before atomic rename; manifest is materialized from `PluginSigil.manifest_raw` (not via fetch); review `shared/pluginhost`, NO new trust mechanisms. Soul cache layout is **catalog** `<paths.modules>/<ns>-<name>/{manifest.yaml, soul-mod-<name>}`. TaskError-reasons: `module_not_allowed` / `module_fetch_failed` / `module_verify_failed`. MVP boundaries: without `absent`-state (TTL-cleanup), ~~without auto-inject~~ (removed by amendment 2026-07-03 - auto-synthesis from `modules[]`), without beacon-hot-reload. **Amends [ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) / [ADR-020](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format) / [ADR-015](#adr-015-core-mvp-modules-exact-list).** **Amendment 2026-08-06 (NIM-377):** the slot above is stale — it is named by the **registration alias** (`<paths.modules>/<alias>/`), holds one executable whose filename means nothing, and carries the canonical-JSON schema document in place of `manifest.yaml`; the schema rides in the artifact as a stamped trailer, so it now does travel through `FetchModule`. **Amendment 2026-08-07 (NIM-524):** `params.name` is address **level 1** — the bare alias naming the slot, never the `<alias>.<module>` of the `modules[]` entry. The synthesizer had been passing the whole two-level name into a field that forbids the dot, so every service declaring `modules:` failed at apply on every host; several entries of one artifact now collapse into ONE install, and a `ref` disagreement under one alias is the diagnostic `conflicting_module_ref`. **Amendment 2026-08-26 (NIM-543):** the takeover comparison reduces **both** halves to level 1 — it had been keyed on the manifest alias against the step's raw literal, so an explicit step spelled `name: community.redis` took over nothing and earned a second install of the same artifact beside it; a `params.name` that is not a registration alias is now the offline diagnostic `module_install_name_not_an_alias` instead of a failure on every host.

### [ADR-066. Onboarding via Teleport on platforms without cloud-init userdata - environment restrictions and work profiles](adr/0066-teleport-onboarding-profile.md)

Moved to [`docs/adr/0066-teleport-onboarding-profile.md`](adr/0066-teleport-onboarding-profile.md). **Deployment-profile solution on top of existing mechanics** ([ADR-063](adr/0063-bootstrap-token-delivery.md) Teleport-transport/full-install/init-phase, [ADR-061](#adr-061-single-run-provisiononboardingrole-onboarding-await--mid-run-re-resolve-roster) await-barrier, [ADR-017(h)](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read) cloud-init/self-onboard) - their contracts do not change; records the restrictions of the external environment and standard onboarding profiles for it. **Environment restrictions (live-tested):** (1) `ci_user_data` is disabled on the platform side **at the namespace level** (matrix: all authorizing keys × dev/stage+prod clouds × user-key/service-account → `ci_user_data is not allowed for this user` BEFORE checking rights) → both userdata paths (B-flat and self-onboard T) are not available; (2) direct SSH keeper→VM closed → `transport: direct` unavailable; (3) access to VM - only Teleport, with Proxy behind a public L7-TLS balancer, VM behind NAT. **Production profile: full-install via `core.bootstrap.delivered` `transport: teleport`** with mandatory environment requirements: **bot-identity (Teleport Machine ID) without `pin_source_ip`** - identity from the interactive `tsh login` carries PinnedIP (OID `1.3.9999.1.9` = src-IP of the check-out machine; keeper with a different src-IP → access-denied) + MFA/TTL restrictions on the interactive session; `alpn_upgrade: true` + private LB root in the system trust of the keeper process; working **external IP** on the VM for Teleport-enroll (`set_external_ip: true` in profile + driver-probe is waiting for external activation). History of 403 diagnosis: first attributed to PinnedIP, actual code wall - ALPN `NextProtos` h2-first (fix `NextProtos=nil` in dial_teleport.go); PinnedIP remains an objective product-limitation identity. **Live-proven by an E2E create run** (VM from scratch → external IP → enroll → automatic onboarding → redis deployment 77 tasks → `ready`). **Demo/dev-profile (bastion):** local keeper + enrolled-VM as bastion (`tsh ssh -R` reverse tunnel; Teleport agent binds `0.0.0.0` itself) + onboarding soul external Teleport shell-exec from the operator's machine (src-IP = pin); keeper-cert must carry the IP bastion in the SAN, endpoint soul.yml - IP not FQDN; **borders: demo/dev, not prod**. **Rejected:** `core.teleport.shell` (plan B: external binary by Sigil [ADR-026](#adr-026-sigil---plugin-integrity-keeper-signed-digest-index), epic ~15 files, does NOT solve PinnedIP - left only as fallback); waiting for `ci_user_data` to be enabled as a blocker (profiles coexist - when enabled, userdata self-onboard T will work without code changes).

### [ADR-067. Mandatory log-shipping (Vector) - log plane of data services](adr/0067-vector-log-shipping.md)

Moved to [`docs/adr/0067-vector-log-shipping.md`](adr/0067-vector-log-shipping.md). **Vector** (`vectordotdev/vector`) - **PUSH log plane**, installed in all data services (redis/dragonfly/...) **required** ("like a node-exporter"), an invariant of the service, not an option. **Adds** node-exporter/redis_exporter (metrics, pull) - three independent observability layers (host metrics / Redis metrics / logs), do not intersect. **Design - clone of the node-exporter standard** (stateful branch [`production-conventions.md`](destiny/production-conventions.md)): new standalone-destiny [`vector`](../examples/destiny/vector/destiny.yml) - `core.url.fetched` (release-tarball GitHub, **mandatory `sha256`** fail-closed) → `core.archive.extracted` → **stable system account** (NOT `DynamicUser`: persistent disk-buffer `data_dir` is undergoing a restart, needs a fixed uid) + `data_dir` 0700 / `config_dir` 0750 → `core.file.present` binary + `core.file.rendered` `vector.yaml` (sources/sinks) + hardened systemd-unit + `core.service.running`/`restarted` (onchanges config/unit/binary); arch = Rust triplet (`soulprint.self.os.arch` amd64/arm64 → x86_64/aarch64). **Does not introduce a new core module** - compiled from existing ones ([ADR-015](adr/0015-core-modules-mvp.md)). **Embedding - unconditional `apply: destiny vector` at the end of `create`** (after the deploy-branch and exporters, without `when:`-gate): the entire contract in **essence** (contract A - author-context, versions/checksum/sink are hidden from the Run-form, `covenant`/`form` are not touched); per-service difference is only `vector_log_sources`. **sink - Option A** (essence per-incarnation: `vector_sink_type` loki/elasticsearch/vector/console, default `console` - without external infrastructure / `vector_sink_endpoint` / `vector_sink_auth_ref`); `sink_auth_ref` — Vault-ref/value, Soul-side resolve, **does NOT settle in state** (symmetry `tls.*_ref`; in unit via `Environment=VECTOR_SINK_TOKEN`, not in `vector.yaml` on disk). **B** (`keeper.yml` globally) / **C** (hybrid) - follow-up. **state read-model** `logging.vector_*` (without `auth_ref`) + bump `state_schema` + per-service migration (redis `013_to_014` v13→v14, forward-only, `has()`-guard). Name `vector` is upstream (node-exporter use case), in [naming-rules](naming-rules.md). **Amends [ADR-024](#adr-024-observability-prometheus-primary--otel-bridge)** (adds the push log plane as a third dimension next to the pull metrics + OTel traces). Related (NOT amend): node-exporter standard (pattern clone, does not have its own ADR; corrects the phantom "ADR-064 node-exporter" design 2026-07-01 - real [ADR-064](#adr-064-secret-write-path---receiving-a-plaintext-secret-from-the-operator-writing-to-vault-keeper-side) = secret-write-path), [ADR-015](#adr-015-core-mvp-modules-exact-list) / [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files) / [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation).

### [ADR-070. Secret reveal-path — disclosure of the plaintext secret of the incarnation to the operator under RBAC rights](adr/0070-secret-reveal-path.md)

Moved to [`docs/adr/0070-secret-reveal-path.md`](adr/0070-secret-reveal-path.md). **READ-double [ADR-064](adr/0064-secret-write-path.md)** (secret write-path): ADR-064 accepts the plaintext secret **FROM** the operator and writes to the Vault keeper-side, this ADR is the reverse direction: it gives the plaintext **BACK** to the operator under explicit permission. **Declarative registry `revealable_secrets[]`** in the manifest `service.yml` (generic, NOT redis hardcode in the kernel): entry `{id, label, enumerate: state.<array>, vault_ref: "secret/…/{incarnation}/…/{key}#field"}` - the service itself declares that its incarnations are disclosed. **Restricted placeholders `{incarnation}`/`{key}`** - literal substitution of validated values, **not CEL** (less attack-surface than computable language in the path to the secret): `key` is obliged ∈ enumerate-array **current** state (anti-arbitrariness), manifest version is always `incarnation.ServiceVersion` (anti version-craft), the result is run through `vault.ParseRef` (traversal-guard `..`). **Endpoints:** `POST /v1/incarnations/{name}/secrets/reveal` `{secret_id, key}` → `{value}` (self-audit `incarnation.secret_revealed` - `{name, secret_id, key, path}` **WITHOUT value**) + discovery `GET /v1/incarnations/{name}/secrets/revealable` → `{items: [{secret_id, label, state_path, keys}]}` (READ, no audit). Authorized expansion → 200-DTO **past `MaskSecrets`** (the only point where the value exits the domain). **★ Security trade-off** (mirror ADR-064: plaintext Keeper→wire operator, TLS) WITH mandatory mitigation blockers: RBAC-gate `incarnation.view-secrets` (scope `coven=`/`service=`/`incarnation=`, fail-closed **404** outside scope) / fact audit WITHOUT value + leak-guard tests for each sink / no body-logging (plaintext with response body only) / key-in-state / traversal-guard / vault-policy read-prefix (`secret/data/redis/*`). The right **`incarnation.view-secrets`** is a new scoped right ([ADR-047](adr/0047-purview.md)), **strictly more privileged than `incarnation.get`**; MCP-tool **no** (REST-only, like `form-prefill`). Rejected: redis-specific hardcode endpoint (in favor of generic registry); CEL in `vault_ref` (in favor of limited placeholders); reuse `incarnation.get`. Deferred (post-MVP): singleton secrets without `enumerate` (admin-password `secret/redis/{incarnation}#password`); the live manifest `community.redis` carries a follow-up section in the module repo. Implemented **NIM-74**. **Amends [ADR-064](adr/0064-secret-write-path.md) / [ADR-047](adr/0047-purview.md).** **Amendment 2026-08-19 (NIM-698, [ADR-0083](#adr-083-a-secret-is-a-declared-state-field--the-author-never-writes-a-vault-path)):** the `revealable_secrets` registry is **deleted** — both endpoints, the right `incarnation.view-secrets`, the audit event, the prefix-allowlist and the system-floor backstop stay exactly as described above, but the path is no longer authored: a secret is declared as a `state_schema` field carrying `type: secret`, and reveal **derives** `secret/<service>/<incarnation>/<state-field>/<key>#<property>` (or `…/<state-field>#value` for a scalar) from `(service, incarnation, field, key)`. `secret_id` becomes `<field>` / `<field>.<property>`, the discovery item gains `collection: bool`, the diagnostic `vault_ref_not_service_scoped` goes with the field it fenced, and the deferred singleton case is solved for free by the scalar form.

### [ADR-072. Host-Utilization — lightweight host-utilization telemetry over the presence channel](adr/0072-host-utilization.md)

Moved to [`docs/adr/0072-host-utilization.md`](adr/0072-host-utilization.md). **Live host utilization** (CPU%/load/mem/disk/uptime) for the operator when opening an incarnation — "is the instance choking **right now**" — **without deploying Prometheus**. A third cheap **push layer** on top of the Soul→Keeper presence stream, independent of the **static** Soulprint ([ADR-018](adr/0018-soulprint-typed.md), refresh 5m, `soulprint.self.*` targeting facts) and the expensive pull-node-exporter; precedent of an independent observability layer — [ADR-067](adr/0067-vector-log-shipping.md). **Transport — Variant B:** new `FromSoul.host_utilization = 10` (message `HostUtilization`, file `proto/keeper/v1/utilization.proto`); alternative A (reserved fields 8-14 in `SoulprintFacts`) rejected — semantic violation (static→live), slow 5m cadence, pollutes the `soulprint` CEL namespace; only-add per [ADR-012(c)](adr/0012-keeper-soul-grpc.md). **Economical pulse** — 30s default / floor 10s (anti-DoS clamp+warn), single-writer via `handleSession`. **Redis-only storage** (hot data, not PG — [ADR-006](adr/0006-cache-redis.md)): latest Hash `soul:<sid>:util` + TTL 3x interval + list-ring `soul:<sid>:util:win` (`LPUSH`/`LTRIM`, N=60 sparklines) — **not RedisTimeSeries** (portable to `redis:7-alpine` and DragonFly, which lack the module). **Invariants:** liveness does NOT depend on utilization (authority — lease [ADR-006](adr/0006-cache-redis.md), graceful degrade when data is missing); freshness — the API returns `stale` (stale data is never presented as fresh, mirroring the Soulprint `collected_at`/`received_at` skew pattern); authenticity — SID comes only from the mTLS peer cert, NEVER from the payload. **API:** `GET /v1/souls/{sid}/telemetry` (latest + window + freshness) + `GET /v1/incarnations/{name}/telemetry` (aggregate across hosts `coven && ARRAY[name]`). **Defers:** delivering config to the agent + essence-override + collector toggles (**NIM-87**), the web HostsTab panel (**NIM-88**), collector extensibility (only-add new fields). **Amends [ADR-024](adr/0024-observability.md)** (a lightweight utilization layer alongside pull metrics / OTel traces / [ADR-067](adr/0067-vector-log-shipping.md) push logs). Layer name — `Host-Utilization`, proto message — `HostUtilization`, in [naming-rules](naming-rules.md). **Amendment 2026-07-18 (NIM-127):** +network throughput (`net_rx_bps`/`net_tx_bps`/`net_err_ps`, aggregate physical-NIC rate) + inode per-mount (`DiskUtilization.inodes_used`/`inodes_total`) + collector `net`; server-side worst-case `IncarnationRollup`; two-tier read UX (soul-page Overview strip + `Utilization` tab, incarnation curated columns + rollup); disk-IO deferred.

### [ADR-0073. Keeper runtime-config → Postgres — the `SettingsStore` overlay](adr/0073-keeper-runtime-config-pg.md)

Moved to [`docs/adr/0073-keeper-runtime-config-pg.md`](adr/0073-keeper-runtime-config-pg.md). **Reload-able Keeper parameters move out of per-VM `keeper.yml` into the `keeper_settings` table** and hot-sync across the stateless HA cluster ([ADR-002](adr/0002-transport-grpc-ha.md)) without a restart — the "first real request" that [ADR-021(f)](adr/0021-hot-reload-config.md) was waiting for; the storage + invalidation machinery is reused wholesale from [ADR-028](adr/0028-rbac-storage.md) / [ADR-029](adr/0029-service-registry.md). **Three layers, resolved per key:** built-in default < Postgres < `keeper.yml` (**amended 2026-07-27, NIM-141**: a value written into a host's own file outranks the cluster one, and the catalog reports the shadowed `cluster_value` so the override is visibly, not silently, ignored). **★ The target end-state is a minimal `keeper.yml`** — everything that can live in Postgres does, and the file keeps only the bootstrap floor: `postgres.dsn_ref` / `vault.*` / `redis.*` (chicken-and-egg — the DSN is itself a vault-ref), `kid` and `listen.*` (per-instance by definition), `logging.*` (must work *before* Postgres, or a Postgres failure could not be reported), `hot_reload.*` (governs the mechanism itself), plus the security-critical `auth.jwt.signing_key_ref` / `metrics.auth.*`. **The operator surface is part of the contract, not a follow-up:** Operator API / MCP + an editable web-UI form + a dedicated RBAC permission family (distinct from `service.*` — editing a runtime tunable is not registering a Service), with the field-registry **published as a backend catalog** ([ADR-042](adr/0042-backend-driven-ui.md); the `GET /v1/herald-types` + `HeraldFieldSpec` shape) carrying per key its type, range bounds, default, current **effective** value and `source ∈ {default, file, pg}` — one source that both validates writes and describes the form, so a newly admitted key appears in the UI with no front-end change, and a key with no live apply path is flagged `requires_restart` instead of silently accepting a no-op edit. **The overlay is injected into `shared/config.Store`** (late-binding, the `SetAuditWriter` pattern) rather than into the consumers — Toll reconfigures from `OnReload` and Tempo re-reads `Get()` per request, both off the *file* snapshot, so switching the source **below** them costs zero consumer edits; the merge policy and field-registry live in `keeper` ([ADR-011](adr/0011-go-layout.md) forbids `shared → keeper`). **Merge = patch the in-memory Document, then re-run the full parse → schema → semantic pipeline** ([ADR-021(c)](adr/0021-hot-reload-config.md)) — no second, weaker validation dialect; ★ the patched Document **never reaches disk**, and write-back for an overlay key targets Postgres, never `keeper.yml`. **Implementation prerequisite** — create-on-write in `shared/config` (`PatchKeeper` returns `ErrPathNotFound` for a path absent from the file, and optional blocks like `toll:` are legal, so a never-touched block would otherwise be un-overridable). **Namespace** — a reserved `cfg_*` prefix inside `keeper_settings`, disjoint from the [ADR-029(g)](adr/0029-service-registry.md) well-known keys (`default_destiny_source` was itself a former top-level YAML key); the key↔YAML-path mapping is an explicit field-registry entry, not a mechanical dot→underscore transliteration (which is ambiguous). **No seeding of file values into Postgres:** an absent row means the layer below shows through, `DELETE` is a clean revert, and no instance can freeze *its* file into the cluster; discoverability comes from a read endpoint returning the effective value + `source ∈ {default, file, pg}`. **Invalidation** reuses the `service:invalidate` channel (envelope `{origin_kid, at}`, self-filter by KID) with the TTL-poll as fallback, plus a **mandatory idempotence guard** — swap only on a real difference, without which the 10s poll would fire a config reload every 10s. **Fail-soft:** unreachable Postgres at startup → run on the file base + WARN (**not** fatal, a deliberate divergence from `serviceregistry.NewHolder`), at runtime → keep last-good; a single bad row rejects the **whole** overlay snapshot; break-glass `KEEPER_CONFIG_SOURCE=file` ignores the overlay entirely. **Write-gate** — parse + validate + range bounds before publish (422, row unwritten). **Admission is a closed set**, not "everything reload-able": reload-able ∧ **not a security gate** (the overlay is fail-soft, and a fail-soft security control degrades toward the more permissive value) ∧ scalar ∧ outside the bootstrap floor and the require-restart class. Audit reuses `config.reload_*` with `source: keeper_internal` — **the closed `source` enum is NOT extended**. Deferred: `config_history`/rollback, the ‡ hot-apply group, typed columns, structural values, the `soul.yml` side. Subsystem name — `SettingsStore`, in [naming-rules](naming-rules.md). **Amends [ADR-021](adr/0021-hot-reload-config.md)**, extends [ADR-029(g)](adr/0029-service-registry.md).

### [ADR-0074. Interactive console — live PTY sessions on a managed host](adr/0074-interactive-console-pty.md)

Moved to [`docs/adr/0074-interactive-console-pty.md`](adr/0074-interactive-console-pty.md). A **third execution mode** beside [Errand](adr/0033-errand.md) and [Voyage](adr/0043-voyage.md): a living tty on the host, so that `top`, `vim`, a `cd` that persists and a `^C` that reaches the foreground job all behave — programs test `isatty` and switch to full-screen, line editing and job control, which no amount of streaming an exec's output can imitate. **A console is deliberately NOT an Errand.** An Errand is request/response — one named module with declared params, one final blob capped at 64 KiB, a known end — and can therefore be checked against a module allow-list **before** it runs; a console is an open-ended bidirectional byte stream whose commands are not knowable in advance, running as the Soul daemon's user, typically **root**. Reusing `errand.run` would let the most privileged action in the system inherit the gate written for the least, so the console gets its own runner (`consolerunner`, never `errandrunner`), its own Keeper-side session manager, its own right and its own audit events. **Transport — only-add, no new RPC:** the `console_*` messages ride the existing bidi `EventStream` `oneof payload` (`FromKeeper` **13-16** / `FromSoul` **11-13**, new file `proto/keeper/v1/console.proto`, numbers frozen by a descriptor guard) per [ADR-012(c)](adr/0012-keeper-soul-grpc.md); the single genuinely new transport is the operator's **WebSocket `GET /v1/console`** — the only WebSocket in Keeper, justified by need rather than taste (SSE is one-directional and cannot carry keystrokes back, and a POST per keypress is not a terminal), authenticated through the `bearer.<jwt>` subprotocol because a browser `WebSocket` cannot set headers, which also keeps the token out of access logs, `Referer` and history, and deliberately **absent from the OpenAPI spec** since an upgrade replaces the response with a raw socket and has no body to model. **The right is `soul.console`, strictly stronger than `errand.run` and independent in both directions**, with the existing `host=` / `coven=` selectors and **no new selector keys** (narrowing beyond them goes through the Purview dimensions, [ADR-047 §S4](adr/0047-purview.md)). It is **checked twice**, because the target host is not in the URL: an **existence-gate** before the WebSocket upgrade (403 before a socket exists) and a scope-aware `host=<sid>` check per `open` frame (a session-scoped error that leaves the socket's other panes live). ★ The upgrade gate **must not** be a scope-aware check with a nil context — an absent scope dimension fails closed, so it would deny precisely the `host=`-scoped roles the feature exists to serve ([ADR-047 §g G1](adr/0047-purview.md)); pinned by a guard test. ★ Recorded consequence: `soul.*` covers `soul.console`, the same widening `incarnation.*` took when `incarnation.view-secrets` landed ([ADR-0070](adr/0070-secret-reveal-path.md)) — a role that must not hand out shells enumerates actions instead of using the wildcard. **Two ceilings, because host and operator policy answer different questions:** the host decides whether an interactive shell may run on it at all (`console:` in `soul.yml` — `enabled`, `max_sessions` 8), while how many terminals one Archon may hold is something no single host can judge (a wall over 30 hosts is one session on each) and lives in `console:` of `keeper.yml` (`max_sessions_per_archon` 30, `max_sessions_global` 256, `idle_timeout` 30m). **Two non-negotiable invariants:** **kill-on-disconnect** on both halves — a pty never outlives the EventStream that authorized it and a session never outlives its operator socket, with teardown escalating SIGHUP → hang up the pty master → sweep the terminal **session** (an interactive shell turns job control on and puts each job in its own process group, so a group-only kill would orphan the `top` someone left running) — and **lost output is always visible**, since bounded queues drop chunks under flood but never lifecycle frames, reporting `dropped_bytes` plus a metric on each side: a silently spliced ANSI stream would show the operator a screen that never existed. **Audit records the fact of a session independently of its content** (`console.opened` / `console.closed` with `archon_aid`, `sid` and the session id) — who opened a shell where must survive every degradation of the recording path. **Session recording is mandatory before the console is on by default**, in an enforceable form: the plane is opt-in and off unless configured, and once recording lands (NIM-145) it is on for every session and **not disableable per-session**. **★ Landed 2026-07-27 (NIM-145):** enforced by construction rather than by default — `console.NewHub` refuses to build without a recorder and `keeper.yml` has **no** `console.recording.enabled` key (absence pinned by a guard test); one interception point, the **Hub**, above both carriers and the cross-instance bridge, with the order **record → deliver** in each direction, so no byte reaches an operator or a shell unrecorded and a chunk is recorded before backpressure can drop it; format **asciicast v2** in Postgres (migration 104), because Keeper is stateless and a local file is a recording exactly one machine can read; retention baked into `ttl_at` and swept by `purge_old_console_recordings`, since a mandatory recorder with no purge is a disk-growth bug with an audit story attached; ★ **masking carried across chunk boundaries**, because a pty echoes keystrokes one byte at a time and per-chunk masking would mask nothing exactly where an operator types a credential path — and it masks the reference only, never the whole chunk; fail-closed at each point it can act (no recording → no `ConsoleOpen` dispatched; a store that breaks mid-session closes even an idle shell; `run-command` is not dispatched, and unrecordable output is not returned). **★ Playback landed 2026-07-28 (NIM-148):** three read routes (`GET /v1/console/recordings`, `…/{id}`, `…/{id}/cast` — asciicast v2 as `application/x-asciicast`, streamed, and unlike the WebSocket present in the OpenAPI spec). **The right is `soul.console` with the same selectors and no lighter auditor-grade right is minted** — a recording is the session's content moved in time, so whoever may watch it must be whoever may open one; conceded and left open: a pure auditor therefore cannot get playback without shells, and the fix is a narrower scope rather than a weaker right. The check splits as (c) does — existence gate at the route (a listing names no host), purview per row — resolved through `ResolvePurview` and never `Check`, whose context map does not see a role's `default_scope`; the list narrows in SQL, out-of-scope answers the same 404 an unknown id gets, and the `souls` join is LEFT with the host dimension reading the recording's own `sid`, so deleting a host cannot delete its evidence. Nothing is masked or un-masked on read — the cast is byte-for-byte what the recorder wrote. Audit `console.recording-read` on the cast route only, written before the first byte. No MCP twin, deliberately. **★ An approval-gate on opening a console in a production Coven is REJECTED for R5**, bounded by an explicit re-open condition rather than closed: there is nothing to attach "production" to — a [Coven](adr/0008-coven-stable-tags.md) is a stable label on `souls.coven[]`, with no coven entity, no table and no environment classification anywhere in the model — the approval engine it would reuse (NIM-113) is an unbuilt draft, and a coarser preventive control already exists in the **scope of the right itself**: an operator who must not open production consoles simply does not hold `soul.console on coven=prod`, and obtaining it is a role grant, which means a second human, the least-privilege subset check and an audit record. What is conceded is that a standing grant is a standing key, so the posture is **detective-leaning** — stated rather than implied — and the decision is revisited as an amendment when either Coven classification gains a home or NIM-113 ships an approval engine. Two cheaper preventive measures are noted as candidates and deliberately not built here: a mandatory `reason` on open (the `incarnation.rerun-last` precedent) and a [Herald](adr/0052-herald-notifications.md) notification on `console.opened`. Impl — NIM-142..147. **Amendment (2026-07-27, NIM-147 — the non-interactive console).** The MCP surface for command execution leaves Deferred as **`keeper.soul.run-command`**: an agent needs the same thing an operator does — run this on that host — but cannot use a terminal, since a pty merges stdout and stderr onto one fd, echoes what was typed, wraps everything in ANSI and reports only the shell's exit status. So the MCP form is request/response. **The right stays `soul.console` with selector `host=<sid>`, and no lighter right is minted:** dropping the tty removes the echo, not the privilege — the command line is still arbitrary, still unknowable before it runs, still executed as the Soul daemon's user. The two-gate split does not arise there because the SID is an argument, so one scope-aware `Check` has its context from the start. The **transport** is the Errand stack with the module **pinned to `core.cmd.shell`** and not exposed as an argument — a deliberate split between what authorizes and what carries, because only request/response yields split channels, an integer exit code, the 64 KiB cap with truncation flags and async escalation. Audit adds **`console.command`** alongside the transport's `errand.*` chain: `errand.invoked` says a module ran, this says an arbitrary command ran and that `soul.console` authorized it — the command line itself is not in the payload, exactly as `console.opened` holds no keystrokes. Masking is applied on this boundary too rather than inherited, with its bound stated rather than implied: on a free-form byte stream only the content layer (vault provenance) can fire, since recognizing a plaintext credential would require knowing what the command was going to do. **Known gap, not closed here:** `core.cmd.shell` and `core.exec.run` are on the Errand runner's hardcoded allow-list, so `keeper.soul.errand.run`, `POST /v1/souls/{sid}/exec` and a `kind=command` Voyage still reach an arbitrary shell under `errand.run` alone — the allow-list constrains the module, not the command line it carries. Aligning those choke-points with `soul.console` is RBAC-breaking for existing grants and is tracked as NIM-197. **Amends [ADR-033](adr/0033-errand.md)**, extends [ADR-012(c)](adr/0012-keeper-soul-grpc.md).

### [ADR-0075. Intra-host task concurrency — `async:` tasks and named barriers](adr/0075-intra-host-async-tasks.md)

Moved to [`docs/adr/0075-intra-host-async-tasks.md`](adr/0075-intra-host-async-tasks.md). Two tasks on one host that do not depend on each other still run one after the other: the apply cycle is a strict loop over `ApplyRequest.tasks[]`, and requisite gating, fail-stop and register accumulation are all written against that order — rendering twenty templates costs the sum of twenty renders, not the longest one. `parallel:` **looked** like the answer: a declared task key, parsed into `Task.Parallel`, specified in full in [destiny/tasks.md §6](destiny/tasks.md). **Nothing ever executed it** — both render guards reject it with `ErrUnsupportedDSL`, the config validator rejects it on a `block:`, and `RenderedTask` carries no field for it, so it could not reach a Soul even with the guards lifted. A specification with no implementation, and **no consumer anywhere in `examples/`**, made this the last cheap moment to choose between two incompatible models: fire-and-forget (start and move on, wait later at a barrier) versus a join block (a container of N tasks run concurrently, then a barrier). Neither is a reading of the other. **Chosen: fire-and-forget, spelled `async: true`** — a flag on an ordinary task, explicitly **not** a grouping mechanism, so adjacent `async:` neighbours do not "execute together". **The name states the semantics**: what is being introduced is asynchrony — start now, collect later at a point of your choosing — while a concurrent **group with a join** remains a plausible future construct with genuinely different behaviour (a bounded set, a group-scoped outcome, a group-scoped error boundary), for which **`parallel:` is reserved as an unrecognized key**; spelling asynchrony `parallel:` would spend the better word on the weaker meaning and force a rename later with consumers in the field, whereas renaming now costs nothing. **Three barriers, and the explicit one is primary:** `require: [<register>, …]` / `require: all`, because it is the form a reader, a linter and a diff can all see — the plan states what waits for what; an implicit reference to an async task's `register.<name>`; and the final barrier at the end of the run, which is required behaviour, not an option (any async task ending failed fails the run). Ordering and condition stay separate keys combining by AND: `require:` answers *when*, `onchanges:`/`onfail:`/`when:` answer *whether*. **A failure is observed where it is awaited, and siblings are never cancelled.** A failed async task writes `failed` into its register and does not break the main flow at the moment it fails; the flow learns exactly where it reaches for it, and there the ordinary fail-stop engages (run irreversibly `FAILED`, subsequent ordinary tasks skipped, only the `onfail:` rescue tail runs). In-flight tasks run to completion: cancel-on-first-failure would make the set of tasks that actually ran depend on scheduling timing and leave a host in a state no one declared. The accepted cost is that a doomed run may keep working as long as its slowest in-flight task. **★ Two architectural boundaries are stated in the specification rather than discovered at run time, and both correct §6 as written.** First, a register reference is a **local** wait only when it sits in `when`/`changed_when`/`failed_when`/`until`/`onchanges`/`onfail`/`require` (Soul-side, inside one `ApplyRequest`), and a **full** plan-wide barrier when it sits in `where`/`vars`/`params`/`apply.input`/`output`/`loop.items`/`loop.when` — those resolve Keeper-side before dispatch and move the reader into the next Passage ([ADR-056](adr/0056-staged-render-passage.md)), so the next Passage waits for the whole previous one on every host; §6's own worked example (`output: { ping: "${ register.ping.stdout }" }`) sat in the second class, meaning the example illustrating a local barrier was in fact a full join. Second, **an async task never outlives its Passage**: the unit of dispatch is one `ApplyRequest` per host per Passage, closed by its `RunResult`, so "background until the end of the destiny" holds only on an unstratified plan (`Count == 1`), where the two coincide — which is why the distinction stays invisible until a probe splits the run. **No concurrency limit in the DSL.** Real fan-out is small (a handful of renders, a handful of fetches), so a mandatory limit would be ceremony around a non-problem, and a per-task field would invite tuning a number the author cannot evaluate — the author knows the plan, not the machine it lands on. The ceiling lives where the knowledge is: host-side `async.max_concurrent` in `soul.yml`, unlimited unless set, mirroring the console's host-side `max_sessions` ([ADR-0074](adr/0074-interactive-console-pty.md)); above it a task waits for a slot and is never dropped, and a run is correct at any setting, only slower. **★ Safety against shared OS resources is the author's, stated concretely rather than generically**, because the most obvious use of concurrency is precisely the one that does not pay off: `core.pkg.installed` takes a single `name`, so N packages are N tasks, and on Debian/Ubuntu apt is invoked with `-o DPkg::Lock::Timeout=300` — concurrent installs do not fail, they **queue on the dpkg lock**, giving the original serial duration plus lock-wait overhead and outright failures once the queue exceeds the timeout, while `dnf`/`yum`/`apk` are invoked without a wait flag and surface contention as errors instead. The real win is independent-path work — `core.file.rendered`, `core.url.fetched`, read-only probes. A declared per-module concurrency class is deferred, not dismissed: the closed `side_effects` resource enum exists only in the **plugin** manifest and the 18 statically-built core modules have no manifest at all, so a resource-aware validator is its own ADR and its own sweep. **Wire — three only-add fields on `RenderedTask`** ([ADR-012(c)](adr/0012-keeper-soul-grpc.md), numbers **18-20**): `async`, `require_idx`, `require_all`, with `require:` register names resolved into task indices **Keeper-side** (Variant A, the same path as `onchanges_idx`/`onfail_idx`/`aggregate_of`, including the global→local remap and the `-1` sentinel). This closes the standing gap where `require:` was parsed and reference-checked but reached no runtime at all. **Determinism is preserved where it is observable**: an async task's own gating is evaluated in the main flow at its position in the plan, before launch, so *whether* a task runs never depends on timing — only *when* it finishes does; register correlation on Keeper is order-insensitive by construction (upsert on `(apply_id, sid, plan_index)`). What is given up is the apply log's plan order, since `TaskEvent`s interleave — events carry `plan_index` and the UI orders by it rather than by arrival. Rejected: the join block as the R5 construct (reserved under `parallel:`, not dropped); keeping the spelling `parallel:` for asynchrony; implicit grouping of adjacent neighbours; cancel-on-first-failure by default; a per-task DSL concurrency field; serializing modules by resource inside the runner. Impl — NIM-149 (this ADR + spec) · NIM-150 (proto + render) · NIM-151 (runner) · NIM-152 (validator + `soul-lint`). **Amends [ADR-009](adr/0009-scenario-dsl.md).**

### [ADR-0076. Engine compatibility — a declared keeper version window per entity](adr/0076-engine-compat-window.md)

Moved to [`docs/adr/0076-engine-compat-window.md`](adr/0076-engine-compat-window.md). An explicit **service↔engine compatibility contract**: a definition is rendered by **keeper** and applied by **soul-agent**, which are versioned independently, and today a mismatch is only discovered by its symptoms — an opaque `render: DSL construct outside the pilot scope` on the keeper side and, in the dangerous case, an **old Soul silently ignoring a new parameter** of a known module (the task reports OK/CHANGED with no effect). **Primary layer — an author-declared window `compat: keeper: {min, max}` on `service.yml` AND on every `destiny.yml`**: per-entity, because a destiny is a separate git artifact pinned at its own ref ([ADR-007](adr/0007-versioning-git-ref.md)) and only its author knows the range it was tested against. The effective requirement is the **intersection — the narrowest wins** (max-of-mins / min-of-maxes); an empty intersection is an authoring error caught at lint/registration; a missing block is unbounded, so existing services need no migration. **Grammar:** half-open `[min, max)`, plain `MAJOR.MINOR.PATCH`, no range operators — a free-form range string (`">=0.1.0 <0.3.0"`) was rejected because arbitrary constraints are **not closed under intersection**, and the effective window has to be displayable in the API/UI. **Comparison:** the keeper version is normalized (a leading `v`, the git-describe distance `-<N>-g<sha>` and `-dirty` are build metadata, not precedence) and compared by its **release core** — so a pre-release is enforced as its release (strict semver puts `0.1.0-beta.1` below `0.1.0`, and under naive matching the entire public beta would fail every window), and a `make build` standing past a tag is enforced as the release it is based on, which is exactly where a mismatch should surface on the dev stand. The check is skipped **only when there is no version to compare** — the un-injected `0.0.0-dev` sentinel or a bare commit hash — and then **loudly warned**, never silently passed: `0.0.0-dev` is valid semver sorting below every declared `min`, so treating it as a real version would break plain `go build`/`go run`. **Enforcement lives on the keeper instance that renders** — the cluster is stateless and horizontally scaled ([ADR-002](adr/0002-transport-grpc-ha.md)), so instances run mixed versions during a rolling upgrade and a registration-time check says nothing about the renderer; the render gate is fail-closed with reason `keeper_version_unsupported`, symmetric to the existing `soul_passage_unsupported`. **Secondary layer — a cross-check, not a replacement:** generalized Soul capabilities ([ADR-056](adr/0056-staged-render-passage.md) §S5, the per-host fail-closed gate that closes the silent-ignore) plus `introduced_in` metadata letting keeper compute the floor a plan actually needs — but **inference can only ever produce a floor, never a ceiling**, which is precisely why the declaration is primary; a declared floor below the inferred one is a lint/registration diagnostic and deliberately **not** a run-time block (blocking a run that would succeed is the worse failure). **The rendered fact is stamped into state** — `apply_runs` gains the keeper/soul versions and the incarnation carries the effective window at render time — never into the definition, keeping the [ADR-007](adr/0007-versioning-git-ref.md) boundary intact in both directions. The bootstrap paradox is accepted and bounded: a keeper older than this release rejects `compat:` as a generic `unknown_key` — fail-closed but unversioned — and the strict manifest walker is deliberately not relaxed to buy a nicer message. Impl — NIM-159..163; the keeper-side deprecation policy lands later as an amendment (NIM-163). **Amends [ADR-056](adr/0056-staged-render-passage.md).**

### [ADR-078. Derived roles — `parent_role`, attenuation and cascade](adr/0078-rbac-derived-roles.md)

Moved to [`docs/adr/0078-rbac-derived-roles.md`](adr/0078-rbac-derived-roles.md). A role may name another role in **`rbac_roles.parent_role`** (self-FK, [migration 102](../keeper/migrations/102_rbac_roles_parent_role.up.sql)); the named role is its **ceiling**. Replaces the copy-a-role-and-narrow-it habit, whose copies silently keep pointing at the parent's OLD scope. **Storage = reference + delta** — the delta is not a new field but the role's existing `default_scope` ([ADR-047](adr/0047-purview.md)) reinterpreted: absolute on a plain role, an **attenuating delta** on a derived one, so a single formula covers both — `effective_scope(r) = effective_scope(parent) AND default_scope(r)`, with a plain role's parent side being the unrestricted top. The scope grammar has no `NOT`, so conjunction can only narrow: **attenuation of scope is structural**. **Variant B** — a child may also hold **fewer** permissions: `effective_perms(r) = own_perms(r) ∩ effective_perms(parent)`, reusing the containment predicate of [`subset.go`](../keeper/internal/rbac/subset.go). The intersection is where the security lives: a child's rows are **NOT** an implicit copy of the parent's (that would widen every descendant whenever the parent gains a permission), while removal from the parent cascades **down** at the next snapshot build — so "a child never exceeds its parent" holds at the **decision** layer, not only at write time. **Resolution is flattened once at snapshot build** (`NewEnforcerFromSnapshot`), never walked per request; cascade needs no new machinery — it rides the `rbac:invalidate` rebuild that already carries every role change ([ADR-028(d)](adr/0028-rbac-storage.md)). **The parent is ONE named role** (not the union of the caller's roles — wider than any single one; multi-parent rejected). **Graph guards live in the schema** so they hold for every write path: no self-parent, no cycles, chain capped at **4 roles** (mirrors the ADR-047 scope-nesting cap), re-parenting checked from both ends (ancestors above + the subtree already below); re-checked in Go at snapshot build, where a broken graph builds **no** enforcer — the existing [`Holder`](../keeper/internal/rbac/holder.go) degradation, reached by the same path as an unparseable permission (a TTL refresh keeps the previous enforcer and warns; at startup the Keeper refuses to come up), a stale-but-coherent catalog beating a partial one. **Deleting a parent that still has children is REFUSED** (`ON DELETE RESTRICT` / `ErrRoleHasChildren`) — every alternative changes rights implicitly and two do so upward: clearing the parent turns the child's delta into an absolute scope and drops the parent's narrowing (a **widening** = escalation, the same reasoning as [migration 100](../keeper/migrations/100_rbac_drop_pattern_selectors.up.sql)), re-rooting to the grandparent widens by definition, cascading the delete strips membership behind the self-lockout check. **The write-time floor is preserved, not replaced**: a derived role must satisfy `child ⊆ parent` **AND** caller-holds-parent (`subset.go` untouched). Self-lockout still counts only a **bare** `*`; once chains resolve, a `*` inside a derived role is capped by its parent and must be excluded from that count (an obligation on NIM-180). **J1 ([NIM-179](adr/0078-rbac-derived-roles.md)) is deliberately inert at the decision layer** — `parent_role` is stored and carried into the snapshot and the role catalog, but nothing consults it on a permission check yet; an unresolved parent leaves a child narrower than intended, never wider, so the gate lands without opening a window. Follow-ups: NIM-180 flattening + the extended subset check, NIM-181 API, NIM-182 web. **Amends [ADR-028](adr/0028-rbac-storage.md) / [ADR-047](adr/0047-purview.md) / [ADR-049](adr/0049-synod.md).**

### [ADR-079. Composed incarnation name — `name_template` in the create scenario](adr/0079-incarnation-name-template.md)

Moved to [`docs/adr/0079-incarnation-name-template.md`](adr/0079-incarnation-name-template.md). A create scenario may declare a top-level **`name_template`** — a `${ … }` template over its own `input:` — and the Keeper composes the incarnation name from the **resolved** input **before the row is inserted**, on the shared [`ResolveCreatePlan`](../keeper/internal/scenario/create_scenarios.go) path that `POST /v1/incarnations` and `keeper.incarnation.create` already take. Replaces hand-assembled free text that in practice always follows a house convention (`{name}-{project}-{subproject}-redis-{service_type}`) yet drifts every time it is retyped — and the name is a `TEXT PRIMARY KEY` with no rename, so a typo costs a destroy+recreate. Cheap and contained for two reasons: the name is **no longer a targeting label** ([ADR-008](adr/0008-coven-stable-tags.md) made it the root Coven tag, NIM-124 superseded that part — it is not injected into the Soulprint and `on: ["${incarnation.name}"]` is rejected), so composition ripples nowhere near `on:`/covens; and everything a composition needs is already in hand before the insert, so **no new run phase and no DB migration**. **The sandbox is input-only** — each block compiles against the same narrow cel-go env as `required_when`/`validate:` ([`input_required_when.go`](../shared/config/input_required_when.go)), making `essence`/`soulprint`/`register`/`vault()`/`now()` undeclared-reference compile errors: a name is a pure function of the request, reproducible from it alone. Unlike general interpolation ([ADR-010 §5(a)](adr/0010-templating.md)) a single block does **not** yield a native type — a name is a string, so every block is stringified and a list/map is an error. **`name` becomes optional in the API and mutually exclusive with a template**: sending it against a composing scenario is 422 `name_not_composable`, deliberately neither silently ignored (which would let the RBAC `incarnation=<name>` dimension be checked against one name while a different one is inserted) nor an override (which would defeat the convention the template exists to enforce). **Overflow refuses, it never truncates** — four components plus literal text pass the 63-character ceiling easily, so the realistic failure answers 422 `composed_name_invalid` quoting the composed string and its length; a truncated name would be a *different* immutable identity under a name nobody asked for. **Name components are write-once identity**: composition runs only on create, `incarnation.spec.input` is never rewritten, so a later run with a different `project` renames nothing — a distinction the create form has to make visible. **soul-lint** catches the whole class statically ([`name_template.go`](../shared/config/name_template.go)): `name_template_input_unknown` (ERROR, modelled on `form_field_unknown` — such a template fails for *every* operator), `name_template_invalid` (outside the sandbox, or index form `input['x']` which hides the component name from static analysis, mirroring `vars[...]`), `name_template_too_long` (literal skeleton alone over 63), plus WARNINGs `name_template_constant` and `name_template_ignored`; under `extends:` the reference check is gated post-merge exactly like `form:`, since the effective input exists only after the covenant merge. **Known limitation, stated not implied:** a *scoped* operator cannot create a templated incarnation on either surface — a nameless request yields an empty RBAC context set, which `RequirePermissionMulti` admits only for bare/`*` roles (MCP behaves the same via a nil context); the direction is fail-closed, and scoping a create whose name is unknown until the service snapshot resolves needs its own decision. Fully opt-in — a service without the key behaves bit-for-bit as before. Impl — NIM-177 (the web half — hiding the free-text field and drawing a live preview with a character count — is a follow-up). **Amends [ADR-009](adr/0009-scenario-dsl.md) / [ADR-010](adr/0010-templating.md).**

## Plugin infrastructure

Soul Stack has three categories of extensions: **Destiny modules**, **cloud providers**, and **SSH push providers**. All three use **single plugin infrastructure** - the same handshake mechanism, protocol, requirements for the artifact. Only the service contract (gRPC service) that the plugin implements changes.

Regulatory fixation of the manifest format, handshake lines, lifecycle, capabilities and side_effects - [ADR-020](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format). The full spec for plugin authors and the host side is [`docs/keeper/plugins.md`](keeper/plugins.md) (the manifest format is the same for all three kinds; `SoulModule` specifics are [`docs/soul/modules.md`](soul/modules.md)).

### [ADR-080. Coven and Trait — one label world: inheritance by membership, union at read (REVERTED)](adr/0080-label-inheritance-union.md)

Moved to [`docs/adr/0080-label-inheritance-union.md`](adr/0080-label-inheritance-union.md). ⛔ **REVERTED IN FULL 2026-08-05 ([NIM-281](adr/0008-coven-stable-tags.md#amendment-2026-08-05-nim-281-a-label-is-never-inherited)) — inheritance does not exist.** An operator label — a **Coven** tag or a **Trait** pair — lives **only where it was attached**, on a host (`souls.coven` / `souls.traits`) or on an incarnation (`incarnation.covens` / `incarnation.traits`), and it **stays there**: nothing is copied between the two levels and nothing is unioned when they are read. A host's labels are its own columns, full stop; belonging to an incarnation attaches nothing, so the only way a host gets one is an operator attaching it (`POST /v1/souls/coven` / `POST /v1/souls/traits`). Reaching an incarnation's hosts is a **membership** question, spelled `incarnation=<name>` and answered from `incarnation_membership` alone — inside a scenario it needs no spelling at all, since a run is already scoped to its incarnation and `on:` omitted means every member. The incarnation-side predicate loses its `name = ANY($x)` arm to match: a coven scope selects an incarnation by `covens &&` only, because a name is an **identity**, not a label. Every reader was reverted together — the RBAC predicate (the correlated `EXISTS` over membership is gone; the `coven`/`trait` dimensions of [ADR-047](adr/0047-purview.md) read columns and stay fully pushed down to SQL), targeting (`soulprint.self.covens`/`.traits`, `soulprint.hosts[].*`, the topology roster, the push inventory and the Voyage target filter), bulk selector and gate (a), push routing Level 2, the Oracle/Vigil subject, the Augur Rite subject, and the trial harness fixtures. **What is NOT restored:** the materialized Trait projection of [ADR-060](adr/0060-traits.md) R1 — `SyncTraitsToHosts` and both of its hooks stay removed, and migration [`106`](../keeper/migrations/106_prune_projected_soul_traits.up.sql) stays applied; with no read-time union left, that prune is a real deletion of labels no operator ever attached, which is the intended end state. **What survives untouched:** the coven overlay of service vars — its axis is `incarnation.covens` selecting layers of the incarnation's OWN config ([ADR-0082](adr/0082-service-vars.md)), and it never read a host label. **Consequence to plan for:** visibility **narrows** for deployed roles scoped `coven=` / `trait.<key>=` — they see only the hosts carrying the label themselves; and `push.coven_default_providers` entries that matched by inheritance fall through to the cluster default, a silent change of SSH perimeter. Audit each before upgrading and tag the hosts by hand. **Known narrowing (NIM-280):** a Decree's and a Rite's subject is `coven` XOR `sid`, so "the members of incarnation X" cannot be expressed as a subject at all — tag those hosts and bind to the tag. Impl — NIM-281 (reverting NIM-121 / NIM-224 / NIM-249 / NIM-251 / the NIM-248 overlay wording). **Reverts its amendments to [ADR-008](#adr-008-coven---stable-logical-tags-only) / [ADR-060](#adr-060-trait--operator-set-key-value-labels-host-and-incarnation) / [ADR-047](adr/0047-purview.md).**

### [ADR-081. Roster at create — a scenario declares which input carries its hosts](adr/0081-roster-at-create.md)

Moved to [`docs/adr/0081-roster-at-create.md`](adr/0081-roster-at-create.md). A create scenario that deploys onto hosts somebody already onboarded (`create_from_souls`) had no way to be **given** them: `POST /v1/incarnations` accepted no hosts, membership was bound afterwards ([ADR-008](#adr-008-coven---stable-logical-tags-only) amendment / NIM-209), and the bootstrap run resolves its roster from `incarnation_membership` **at start** — so it aborted `no_hosts` before its first task, and a scenario cannot bind its own roster from the inside. The scenario now **declares** which of its `input:` fields carries the roster, with a third variant of the [ADR-044](adr/0044-choir.md) S-T1 source discriminator: `source: { roster: true }`. The key does double duty — for the UI it is the SID catalog (**free onboarded souls the caller may see**; the incarnation-scoped variants have no incarnation to resolve against on a create form), and for Keeper it is the statement that this input **is the composition** of the incarnation, bound into `incarnation_membership` after the insert and **BEFORE** `runner.Start`. Which field carries it is the author's choice (`config.RosterInputField`), at most one per block. **The create contract is unchanged** — the roster rides inside `input`, so `required` / `min_items` / `format: sid` and a `validate:` size rule against the declared topology all come from the ordinary input gate, turning a render-time `error_locked` into a request-path 422. **The order IS the decision:** screen before the insert (a refusal leaves no half-made incarnation), bind after it (the FK needs the row), run last; a bind that fails infrastructurally answers 500 with **no run**, leaving an empty ready incarnation the operator repairs with `POST .../members`. Authorization reuses the bind route's two gates verbatim — `incarnation.bind-member` ANDed over every declared coven (otherwise create is the way around NIM-209's gate (a)) plus every SID inside the caller's `soul.list` purview, all-or-nothing, buckets ordered unknown→422 / out-of-scope→**403** / not-connected→422 — through one screening function shared by REST and MCP. **`input.hosts` is a journal, membership is the truth:** the input records what the incarnation was created ON, and the two diverge legitimately once the Hosts tab edits the roster; later runs read the relation. It is deliberately **not** written to `spec.hosts`, which declares host ROLES ([ADR-008](#adr-008-coven---stable-logical-tags-only)) and would read plausible while binding nothing. The catalog is `GET /v1/souls` with `sid_prefix` (literal, LIKE metacharacters escaped) and a repeatable `coven` (ANY-of), narrowed by **`status=connected` and nothing else** — the two tighter filters this began with are both wrong and were dropped after the first live review: an incarnation's declared covens reach a host only by inheritance once it BELONGS to it ([ADR-080](adr/0080-label-inheritance-union.md), so a candidate cannot carry them), and "unassigned" contradicts M:N membership (a host legitimately serves several incarnations). The RBAC scope, not a guess about occupancy, is what keeps other people's hosts out. Reused instead of extending form-prep because that endpoint is addressed per module and the souls list is **already** `soul.list`-scoped, so "the picker cannot show a SID the caller could not otherwise see" holds by construction rather than by a second copy of the rule (the failure class of NIM-148 / NIM-202/203). Rejected: a `hosts[]` field in the create body (core contract for every service, when the scenario is what knows whether it is given hosts or produces them), writing `spec.hosts`, a scenario binding itself, a separate `requires_roster` flag (the field's presence **is** the flag — and it retired the web form's guess by scenario NAME), operator-assigned master/replica roles (`cluster_topology` already covers it; otherwise the plugin lays roles out by sorted SID). Impl — NIM-371. **Amends [ADR-008](#adr-008-coven---stable-logical-tags-only) / [ADR-044](adr/0044-choir.md) / [ADR-045](adr/0045-param-dsl.md).**

### [ADR-082. Service vars replace Essence — one `vars` namespace, `incarnation.spec.essence` removed](adr/0082-service-vars.md)

Moved to [`docs/adr/0082-service-vars.md`](adr/0082-service-vars.md). A service repository's default parameters move from `essence/` into **`vars/`**, the CEL root `essence.*` merges into **`vars.*`**, the package `keeper/internal/essence` becomes `servicevars`, and **`incarnation.spec.essence` is removed with no replacement**. The two namespaces were separated by exactly one property — essence was overridable from outside, a `vars` local by definition is not ([`docs/destiny/vars.md`](destiny/vars.md)) — and the override is what goes: `spec.essence` had two live readers (`scenario/state.go`, `grpc/events_telemetry.go`, both through `specEssence()`) and **no writer at all** (no field in `IncarnationCreateRequest`; the only write of the key anywhere is the fixture of the unit test that reads it back), while being the last key blocking the drop of the `incarnation.spec` column (NIM-408). **A fleet overrides a service's defaults by FORKING the service repo** and re-pinning its `ServiceRef` ([ADR-007](#adr-007-versioning-of-artifacts-is-done-through-git-ref-not-through-a-field-in-the-manifest)) — the ansible-role model, zero new machinery; the redis keys commented "the operator overrides this in `spec.essence`" (eighteen comments over a couple of dozen keys: fleet-wide facts pinned to a single instance's row, plus values deliberately kept out of the Run form) do not move at all, only the way to override them does. Rejected: a replacement column `incarnation.essence_override`, `keeper_settings` ([ADR-0073](adr/0073-keeper-runtime-config-pg.md)) for the fleet group — it needs a `settings.*` CEL root, and adding an entity is the opposite of the point (still available later, on top, changing nothing) — and relocating the per-incarnation keys into `input:` behind a collapsed `form:` section, which would park **desired** constants in `state`, a projection of **actual**. **The priority ladder becomes one flat stack:** `<service>/vars/*.yaml` → destiny `vars.yml` → `block:` → task, with no operator rung (and no scenario file rung — `scenario/<name>/vars.yml` is documented but read by nothing); a layer may reference the layers **below** it, since a task var used to reach the service layer as `${ essence.X }` and would otherwise fail on the same expression spelled `${ vars.X }`, while sideways stays refused; the merge's one real cost is silent shadowing (a task var taking over a service var's name), to be covered by a soul-lint WARNING `vars_shadows_service_var` (NIM-416, not built yet) on the model of the existing `vars_collision`. It also pays for part of itself — the transit hops `essence.conf_dir → compute.conf_dir → task vars.conf_dir`, which existed only to cross the namespace boundary, collapse (redis, dragonfly, mongo). **`essence/os/` and `essence/coven/` are deleted** (implemented, used by zero shipped examples and zero external destiny repos) and **`vars/_stack.yaml` becomes real** — it was documented as working in [`docs/service/manifest.md`](service/manifest.md) and in the section above while the code read `// Convention-based ordering (no _stack.yaml)`; conditionality gets ONE mechanism instead of two, and the [ADR-080](#adr-080-coven-and-trait--one-label-world-inheritance-by-membership-union-at-read) guard proving that an incarnation's tag reaches its members' parameters (NIM-248) is **retargeted onto a `foreach:` step, not deleted**. The step context is `incarnation.*` (covens included) / accumulated `vars.*` / the `foreach:` binding, and **nothing else**: the draft's `host` root is dropped (it duplicated the incarnation's labels on one axis and misreported the keeper context, which has no host, on the other), and `soulprint.self` is refused outright — a service's vars resolve **once per run** and are handed to every host, so a step keyed on one host's facts would silently apply that host's answer to the whole roster (a mixed debian/rhel roster being the obvious case). The layer is host-invariant by construction. Host-dependent behaviour belongs where the render already is per-host — `where:` on a task, a task's own `vars:`/`params:` over `soulprint.self.*`, a `.tmpl` — and **not** in `apply: input:`, which resolves on `targeted[0]` and hands one set of values to a destiny's whole roster. Per-host service vars were weighed and deferred: doing them honestly means refusing `vars.*` in `compute:` and in `apply: input:` and rewriting the 141 sites that read it there, a train of its own. `os/<family>.yaml` therefore has no replacement — it was used by zero shipped services and already answered with `hosts[0]`'s family for all of them. Corollary: the incarnation's covens must be read on every resolving path, so the runner's `FOR UPDATE` read and the telemetry query now select the column they used to omit. New per-step **`strategy: deep|replace`**: redis documents `install_package` as "the whole map that an override replaces" while `mergeInto` recurses, so overriding `repo_uri` alone kept the base's `gpg_key_url` — an intent the mechanism could not express. Without a `_stack.yaml` the order is every `*.yaml`/`*.yml` directly inside `vars/`, sorted lexically (subdirectories are **not** walked → `vars_dir_nested`), and the base file is **`00-base.yaml`, not `_default.yaml`** — a rename forced BY that scheme rather than one fixing a defect in the old one (the old resolver read `_default.yaml` through a hard-coded constant and never sorted anything), because once the directory listing is the order the name carries the precedence, and `_` is `0x5F`, between `Z` and `a`: `00-base.yaml` < `Base.yaml` < `_default.yaml` < `base.yaml`. **Two Struct payloads that cross the wire lose the `essence` key** — `RenderedTask.flow_context` ([ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)) becomes `{input, vars, incarnation, self}` and `core.file.rendered`'s `render_context` ([ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files)) becomes `{vars, self, role}` (plus its conditional `input`); no proto field number moves (only-add intact), but a Soul from before this train finds nothing under `essence`, stated here rather than left to a field report, and cheap in practice since **no `.tmpl` in `examples/` reads `.essence`**. The name asymmetry across the `apply:` boundary is documented head-on instead of removed (in a scenario `vars` = service vars + locals, in a destiny only its own `vars.yml`; isolation untouched) — that same asymmetry already cost `apply_when_dynamic_unsupported`, where different names merely camouflaged it. **Essence retires from the dictionary** ([naming-rules.md](naming-rules.md) entries rewritten to forward here, not deleted). Impl — NIM-412…NIM-416, with the column drop in NIM-408. **Amends [ADR-008](#adr-008-coven---stable-logical-tags-only) / [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) / [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files) / [ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add).**

### [ADR-083. A secret is a declared state field — the author never writes a Vault path](adr/0083-declared-secret-state-fields.md)

Moved to [`docs/adr/0083-declared-secret-state-fields.md`](adr/0083-declared-secret-state-fields.md). A service author declares a secret **where the data already lives** — as a field in `state_schema` carrying `type: secret` — and Keeper derives its Vault path from `(service, incarnation, field, key)`; every authoring channel into the service's own namespace is closed. The problem was never that `revealable_secrets` ([ADR-070](#adr-070-secret-reveal-path--disclosure-of-the-plaintext-secret-of-the-incarnation-to-the-operator-under-rbac-rights)) was wrong, but that it was the **fourth** hand-written copy of one path: a redis service spells `secret/{service}/{incarnation}/users/{key}#password` in `service.yml`, again in prose in `types.yml`, and again in seventeen `${ vault(...) }` call sites across three scenarios — with a comment, not a mechanism, holding the four in agreement. **The declaration:** `type: secret` on a scalar field, or on a property inside `items` next to the `key:` naming which sibling property is the collection's identity, and nothing about how a value is minted — that policy travels inside the `SecretRequest` written at the call site, so it lives in one place rather than two that must be reconciled; the derived path is `secret/<service>/<incarnation>/<state-field>/<key>#<property>` for a collection and `secret/<service>/<incarnation>/<state-field>#value` for a scalar, every segment validated against the [ADR-064](#adr-064-secret-write-path---receiving-a-plaintext-secret-from-the-operator-writing-to-vault-keeper-side) grammar `^[a-zA-Z0-9_-]+$` and **failing closed** — `<key>` is operator-influenced data, so a `/` or a `..` inside a user's name must never become a path segment. `revealable_secrets` is **deleted**: the reveal endpoints, the `incarnation.view-secrets` right and the `incarnation.secret_revealed` audit event are untouched, what goes is the author-written `vault_ref` that fed them, and the `vault_ref_not_service_scoped` diagnostic goes with it because the escalation class it fenced is no longer expressible. A new CEL function **`generate_secret({...})`** returns an opaque **`SecretRequest`** marker rather than a value, so no plaintext exists at render time and none can leak through a register, a log line or a diff (CEL has no keyword arguments and no `=` token, so a map argument is what keeps the names at the call site). The single write is the keeper-side module **`core.state.present`**, which reads the state field, mints the properties the step asked for with `generate_secret({…})` and whose derived path is still empty, writes them to their derived paths and returns the **effective** state in its register — *a generator's output is a candidate, a writer's output is the truth*: present-semantics is what makes a second run keep the first run's password instead of minting one the writer discards while a consumer has already configured the target with it. A register also crosses the `include:` boundary **downwards and transitively** — expansion threads each level's declared `register:` names into the bodies it splices in — so a `_common/` partial can quote the writer; sibling branches stay blind to each other, and the reverse direction (a main file naming a register declared inside an included body) stays a per-file error, because a conditional include is dropped as a whole group at render. To let a Soul-side task read that, the keeper register is **unioned into** every per-host bucket instead of remaining the empty-bucket fallback [ADR-056](#adr-056-staged-render---running-the-scenario-as-n-ordered-passages) deliberately withheld (duplicate register names are already a load error, so the union is unambiguous by construction); `incarnation.state` stays the frozen pre-run snapshot it is. A secret in a register rides as a **reference**, which closes the plaintext window through `apply_task_register` for free rather than by adding a purge. **The fence is on the namespace, not the spelling:** an author-written path under `<mount>/<service>/` is rejected in all four spellings — `${ vault(...) }`, a `vault:` ref in `params:`, and the `path:` of `core.vault.kv-read` / `core.vault.kv-present` — while all four survive **outside** that prefix, where the cross-namespace read class (a shared TLS CA, another service's credential) is real and has no replacement yet. Per-field **`secret: true` on module output** retires **`no_log`**, which is all-or-nothing and set by the task author rather than by the module that knows its own output shape — too coarse and unreliable at once; removed, not deprecated. The four channels it covered are answered separately, and two of them honestly narrower: `params:` by the per-cell seal, `output:`/`register_data` by per-field masking of the observable copy (the live register keeps the value), `state_changes` by §1 + §6 instead of the source-side register drop that also broke the register chain, and **`error.message` not at all per task** — what stands there is the write-path vault-ref masking plus the SSE floor that withholds it for every failed task. The generation policy grammar is the existing one (`length` / `charset` / `allowed_chars`), lifted out of the `core.vault` module into shared code so both callers parse one definition. Rejected: a generator module owning the store (a second state manager beside `incarnation.state`, with the storage path invisible to the author), `generate_secret()` returning plaintext, per-passage state refresh, a new `secret.*` CEL root, character-class composition rules, entropy-based sizing, and deleting `vault()` outright. **Breaking, deliberately** — derived paths do not match what existing incarnations use, and no migration is provided: this lands before the release. Impl — NIM-698; rotation is a separate ticket, and explicit state capture landed as [ADR-084](#adr-084-explicit-state-capture--corestateverb-replaces-end-of-run-state_changes) (NIM-699), which **amends §4**: the address becomes `core.state.set` (one state per [ADR-057](#adr-057-state_changes---ordered-list-of-crud-verbs) verb), the declared-secret rule moves off the verb onto the property — every verb keeps an existing value and mints only what is absent — and the write lands at the step instead of at an end-of-run commit — which also reverses two lines above: `incarnation.state` no longer stays the frozen pre-run snapshot, and per-passage state refresh is no longer rejected, because the read-modify-write verbs only mean anything if a write in the same run is observable. **Amends [ADR-070](#adr-070-secret-reveal-path--disclosure-of-the-plaintext-secret-of-the-incarnation-to-the-operator-under-rbac-rights) / [ADR-064](#adr-064-secret-write-path---receiving-a-plaintext-secret-from-the-operator-writing-to-vault-keeper-side) / [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) / [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files) / [ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) / [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read) / [ADR-056](#adr-056-staged-render---running-the-scenario-as-n-ordered-passages).** **This summary is the ADR as first decided; it has been amended four times since — read them in the ADR.** The address became `core.state.set` and the declared-secret rule moved off the verb onto the property (2026-08-25, NIM-699); the namespace fence gained a second axis (2026-08-26, NIM-706); the declaration moved to the input dialect (2026-09-01, NIM-741); and **a missing value is not minted on its own** — the mint follows the `generate_secret({…})` request, so `core.state.present` over a populated field fails closed instead of minting (2026-09-02, NIM-746).


### [ADR-084. Explicit state capture — `core.state.<verb>` replaces end-of-run `state_changes`](adr/0084-explicit-state-capture.md)

Moved to [`docs/adr/0084-explicit-state-capture.md`](adr/0084-explicit-state-capture.md). A scenario's state is written **where the scenario says so**, by a keeper-side step, and the write lands **at that step** rather than at an end-of-run commit. The `state_changes:` block is retired: it was a second grammar for the thing tasks already do, evaluated in its own CEL context, invisible to `when:` / `register:` / `onfail:`, and committed only after every host had finished — so a run that died half-way recorded nothing it had in fact done, and a task could not read what an earlier task in the same run had just written. **The address is the verb:** one base module `core.state` carries seven states — `set` / `present` / `add` / `append` / `modify` / `remove` / `unset` — the [ADR-057](adr/0057-state-changes-crud-verbs.md) verb set, applied by the same engine the retired block used, so a verb cannot mean two different things depending on which path wrote it; a param the verb does not take (a `patch:` handed to a `set`) is an authoring error rather than a silent drop. **Secret resolution is orthogonal to the verb** — the rule that made [ADR-083](#adr-083-a-secret-is-a-declared-state-field--the-author-never-writes-a-vault-path) §4 name its module `present` moves onto the *property*: on `type: secret` every verb keeps an existing Vault value and mints only what is absent, so `core.state.set` overwrites the field's ordinary content and still does not rotate a live credential, while `present` now answers the separate question of whether the incoming value reaches the field at all. Deliberate rotation stays inexpressible and gets its own decision. **State accumulates within a run** — each capture is committed under the run that produced it, so a half-way failure keeps what it had already captured. Rejected: keeping `state_changes:` beside the verbs (two grammars for one write, and the ordering question stays unanswered); a single `core.state.write` taking the verb as a parameter (a verb is not data — the address is what a linter, a diff and a reader all key on); and compare-and-swap `expect:` on `set`, deferred because ADR-057's `expect` counts match cardinality while a whole-field `set` has no `match:`, making it a new mechanism rather than an extension. **Amends [ADR-083](#adr-083-a-secret-is-a-declared-state-field--the-author-never-writes-a-vault-path) §4 / [ADR-057](#adr-057-state_changes---ordered-list-of-crud-verbs) / [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) / [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read).** Impl — NIM-699.

### [ADR-085. A registry entity carries an immutable `id` and a mutable `label`](adr/0085-entity-id-and-label.md)

Moved to [`docs/adr/0085-entity-id-and-label.md`](adr/0085-entity-id-and-label.md). One field, `name`, is asked to do two jobs that pull apart: an **identifier** that must never move — the `TEXT PRIMARY KEY` of twelve registry tables, segment 2 of every derived Vault path ([ADR-083](#adr-083-a-secret-is-a-declared-state-field--the-author-never-writes-a-vault-path) §1), the CEL root `incarnation.name`, 49 `{name}` OpenAPI path templates and the value of the RBAC `incarnation=` dimension — and a **caption** an operator wants to edit. The identifier wins everywhere, so the caption is not expressible at all, and putting one in the identifier is not a cosmetic mistake. **Decision: two fields.** **`id`** is the identifier: one grammar for every registry, `^[a-z0-9][a-z0-9-]{0,62}$` (incarnation's existing pattern, unifying the four dialects on disk and closing `serviceregistry`'s `^[a-z][a-z0-9-]*$` having **no upper bound at all** while its value is a Vault path segment; a leading digit stays legal, so the live `9redis` fixture keeps its premise), **immutable**, set once at registration. **`label`** is the caption: free text, capitals allowed, mutable at any time, non-unique, optional, display only, seeded at registration with a Title-cased default derived from the git path. ★ **The invariant that makes the split worth anything: `label` participates in NOTHING derived** — no Vault path, no RBAC scope, no snapshot directory, no `incarnation.<…>` in CEL, no selector, no resolver. Only under it is *"I changed the label and nothing moved"* a guarantee rather than a hope. **Immutability is mechanics, and it is two separate arguments that must not be merged.** A **rename orphans** because `SecretField.VaultPath` joins its segments verbatim (`shared/config/secret_field.go:153-188`; the only check is `^[a-zA-Z0-9_-]+$`, capitals pass, nothing folds case, Vault KV is case-sensitive) and **nothing in the tree rewrites a Vault path when an identifier changes — no rename operation exists anywhere** — so a changed id derives a *different* path and the old value becomes unreachable with no error raised; the sharpest instance is `<mount>/herald/<entity>/<field>`, which takes that segment straight from the registry row, one hop with no state schema in between ([ADR-064](#adr-064-secret-write-path---receiving-a-plaintext-secret-from-the-operator-writing-to-vault-keeper-side)). Separately, **replace-not-merge** (`WriteKV` calls `Put`, not `Patch`) is what makes a *collision* destructive — that is the ADR-083 / NIM-706 reserved-namespace argument and **not** the rename argument. The honest framing: all of these are **already** immutable de facto (every PK is `name TEXT PRIMARY KEY`, no surface offers a rename, [ADR-079](adr/0079-incarnation-name-template.md) records it for incarnation), so this decision does not introduce immutability — it **names** it and adds the escape hatch that was missing. **`id` means "code word", not "opaque identifier"**: kebab, human-chosen, legible in a URL, a Vault segment and a CEL expression, like `SID` and `AID`; where the platform wants a genuinely opaque handle it uses a **surrogate** and says so (`apply_id` / `errand_id` / `AuditEvent.id` are ULIDs, `Rite.id` is a sequence number), and `augur.Rite.ID int64` sitting inside a renamed registry is carved out in prose rather than renamed here. **"label" now names two things and they never meet** — a *matching label* is a Coven tag or a Trait pair ([ADR-008](#adr-008-coven---stable-logical-tags-only) / NIM-281, "a label is never inherited"), an entity's *`label` field* is a caption nothing derived reads; the selector metavariable is re-spelled `coven=<coven-tag>` across the docs so the two senses never share a line. **Scope is platform-wide**, not the eight `NamePattern` registries: all twelve `name TEXT PRIMARY KEY` tables convert (`rbac_roles`, `synods` and the choir FK columns included) and all 49 `{name}` path templates become `{id}` — a half-rename at the URL layer is the same defect that was rejected at the wire and DB boundary, surviving in the layer an operator reads. `service.yml` carries neither field: the manifest key `name:` is **deleted**, the same shape of decision [ADR-007](#adr-007-versioning-of-artifacts-is-done-through-git-ref-not-through-a-field-in-the-manifest) made for `version:`. **`operators.display_name` → `label`**; `aid` stays that registry's id, and its deliberately wider grammar is the one recorded exception. **CEL reach** — `incarnation.name` → `incarnation.id`, `name_template:` → `id_template:`, `composes_name` → `composes_id`, `name_not_composable` → `id_not_composable` — breaks every service repository, so the engine accepts both roots for a **compatibility window**, `soul-lint` warns with a line address, then the old root is dropped; ⚠ that window is a **different axis** from [ADR-0076](adr/0076-engine-compat-window.md)'s keeper-version window per entity. Three consequences rated major: **a fence surface is lost** (the manifest load was one of ADR-083's four reserved-namespace enforcement surfaces — not a security regression, the derivation still refuses a colliding path, but the author stops finding out at authoring time); **a stale CEL root is a runtime failure, not a compile error** (`incarnation` is DynType in all three envs, and one of them is flow-control evaluated **on the host**, so a stale `when:` dies mid-run after earlier tasks applied — the class [ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)'s 2026-08-03 amendment recorded for `essence`, which makes soul-lint's warning load-bearing rather than a courtesy); and **consumer breakage** across `soul-stack-web` (codegen + routing, silently — core `make check` cannot see it), `soulctl`, the derived [`openapi.yaml`](keeper/openapi.yaml), the MCP tools, every service repository, `keeper/migrations` and `soul-stack-plugins`. **Two names are deliberately NOT decided here** and need propose-and-wait first: the permission gating the label mutation and its audit event, both in closed catalogs. Impl — NIM-728 … NIM-733 (epic NIM-725; this ADR is NIM-727; the manifest-key removal is NIM-726). **Amends [ADR-079](adr/0079-incarnation-name-template.md) / [ADR-083](#adr-083-a-secret-is-a-declared-state-field--the-author-never-writes-a-vault-path) / [ADR-029](adr/0029-service-registry.md) / [ADR-064](#adr-064-secret-write-path---receiving-a-plaintext-secret-from-the-operator-writing-to-vault-keeper-side) / [ADR-008](#adr-008-coven---stable-logical-tags-only) / [ADR-047](adr/0047-purview.md) / [ADR-081](adr/0081-roster-at-create.md) / [ADR-082](#adr-082-service-vars-replace-essence--one-vars-namespace-incarnationspecessence-removed) / [ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) / [ADR-025](adr/0025-augur.md) / [ADR-030](adr/0030-vigil-oracle.md) / [ADR-052](adr/0052-herald-notifications.md) / [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read) / [ADR-032](adr/0032-push-orchestrator.md) / [ADR-044](adr/0044-choir.md) / [ADR-014](adr/0014-operator-identity.md).**

### [ADR-087. A task's side is derived from its module address, not declared by `on:`](adr/0087-task-side-derived-from-module-address.md)

Moved to [`docs/adr/0087-task-side-derived-from-module-address.md`](adr/0087-task-side-derived-from-module-address.md). `on:` is overloaded: for a Soul task it is a coven filter, while `keeper` is a magic scalar in the same key meaning "do not send this to hosts at all" — one key, two unrelated jobs, and the second is a routing verdict the platform already knows. **Decision: routing is derived from the module address**; `on:` returns to one meaning — a list of covens — and the scalar `keeper` on a **core** address becomes an error. The derivation is unambiguous because the two core registries are **disjoint** (seven keeper-side bases against twenty-one Soul-side ones, no name in both), and routing today is the single line `task.On.(string) == "keeper"`. The refusal matrix is a table rather than prose, because an omitted `on:` now means two different things depending on the address; `on: keeper` on a Soul-side address gets its own diagnostic, separate from redundancy, since the two are opposite mistakes with opposite fixes. ★ **A base absent from `shared/coremanifest` has no side** — every new diagnostic stays silent for it and routing falls back to the written `on:`; without that rule `on: keeper` on `core.cert.registered` would be a false error on working code. The offline answer is not a new constant set: `shared/coremanifest` already lists both sides in one table, and the side becomes a field on `sdk/schema.Module`, subsuming three ad-hoc address constants. A plugin module declares **`side: keeper | soul`, default `soul`, per module** — the absent key and `side: soul` are indistinguishable **by design, permanently**, because documents are signed and stored. ⚠ On a plugin the field is **accepted and inert** until NIM-688 lands the executor, and the failure stays loud rather than becoming a silent skip. Consequences: ★ the **Passage stratifier is the blocker, not a validator** (`onTargetsRoster` is literally `on == nil`, so dropping the key silently re-stratifies the keeper tasks standing after a `refresh_soulprint` emitter — four example trees today, but **the hazard is silence, not volume**: Passage boundaries move with no diagnostic anywhere — and turns the refresh emitter into its own consumer; it must become side-derived in the same move); the **`when:` hole is worse than "ignored"** (a dynamic `when:` on a keeper-side module that omits `on: keeper` reaches the agent, is evaluated before the module lookup, and when false on every host yields a **green run with the keeper-side effect absent**); and the break is taken **in one release with no transition window** (a warn-then-error window was offered and declined), so an unswept external service fork simply stops loading. Rejected: `runs_on:`, a `Document`-level `side`, a transition window, and folding this into ADR-009/ADR-017 as amendments. Status: **accepted, not implemented** — epic NIM-747; this ADR is NIM-748; implementation NIM-749 / NIM-750. **Amends [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) / [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read) / [ADR-020](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format) / [ADR-084](#adr-084-explicit-state-capture--corestateverb-replaces-end-of-run-state_changes).**

### [ADR-086. One schema dialect — `state_schema` is written in the input DSL](adr/0086-one-schema-dialect.md)

Moved to [`docs/adr/0086-one-schema-dialect.md`](adr/0086-one-schema-dialect.md) — the first ADR authored without a number and stamped at squash-merge, per the convention decided 2026-09-01 (see [docs/adr/README.md](adr/README.md)). A service author writes two schemas, `input:` and `state_schema`, and they were written in **two different dialects** — the platform's own input DSL on one side, JSON Schema on the other. [ADR-062](adr/0062-input-types.md) rejected `$ref` precisely to avoid *"a second schema DSL alongside our own input DSL (a divergence of `properties`/`required` semantics, the `type` vocabulary, `input_*` error codes)"*, and then left that divergence standing in the one place it was already shipping. **Decision: `state_schema` becomes a map `<field name>` → schema, like `input:`.** The `type: object` / `properties:` wrapper and the list `required: [names]` are refused **at the root**; requiredness is `required: true` on the field. ★ Root-only — a **nested** object field still declares `type: object` with its fields under `properties:`; what leaves the **whole** dialect, `types.yml` and nested objects included, is the list form of `required:`. The vocabulary becomes snake_case, which is **alignment rather than invention** — the input DSL already spells these that way (`shared/config/input_schema.go:322-342`): `additionalProperties` → `additional_properties`, `minimum` → `min`, `minLength`/`maxLength` → `min_length`/`max_length`, `minItems`/`maxItems` → `min_items`/`max_items`, `exclusiveMinimum` → `exclusive_min`; `patternProperties` is refused outright (no counterpart, zero authored uses). Two capabilities follow from the shared dialect. **`$type` may carry the node's own `properties:` — in `state_schema` only** — which is a **widening of an existing closed overlay set**, not a carve-out: `applyRefOverlay` already overlays `description`/`required`/`required_when` (`shared/config/input_types.go:430`) while `input_type_ref_conflict` refuses the closed set `{type, properties, items}` (`input_types.go:100`); `properties` moves from the second list to the first, `type` and `items` stay refused everywhere, and the merge is [ADR-009](adr/0009-scenario-dsl.md)'s `extends:` covenant **by reference** — add-only, shallow, fail-closed on a collision (`input_type_ref_overlay_conflict`). And **`type: secret` becomes legal in `types.yml`**: *a property with `type: secret` in a shared type is not asked for on input — the platform mints it; in `state_schema` it means a declared secret*. **The input half of that was deferred and is resolved and built since 2026-09-02 (NIM-751)** — such a property is stripped from the projected operator form, is never required and takes no default, and a value supplied anyway is refused as `input_secret_type_not_writable`; the render seal deliberately stays provenance-based on `secret: true`, because a value refused at the gate reaches no cell to mask. The secret node's grammar stays closed to `type`/`key`/`label` — the shared input keys (`default`, `enum`, `pattern`, `min_length`/`max_length`, `secret`, `prefill_from_state`, `required_when`, and `description`, that last one because `label` already carries the caption) are refused on it — and `required: true` on a `type: secret` property is refused as `secret_field_required`, since no state instance can satisfy it. **Breaking, no transition window.** The old envelope is refused **by name** (`state_schema_legacy_json_schema_form`) rather than left to parse, because read as the new dialect it is two state fields called `type` and `properties`, which moves every real field a level down and loses every declared secret with no error raised; the list form is refused as `input_required_list_removed`. The open question §14 recorded — `validateObjectSchema` demanding `properties` on a `type: object` even when `additional_properties` carries a schema, which left the corpus's map-shaped state fields (`redis_config`, `sysctl_settings`, `redis_sentinel.master_settings` and `.settings`) with no expressible form — was **resolved by NIM-742 as the ADR's candidate 1**: an object describes its contents through `properties` OR `additional_properties`, and the bare `false` counts as neither (`shared/config/input_schema.go:1155-1172`). ⚠ §14's own text in the ADR file still reads as open; amending it is NIM-741's. Status: **implemented** — NIM-742/743/751; the corpus rewrite NIM-744 is outstanding — epic NIM-740, this ADR NIM-741, implementation NIM-742 (engine, including the fail-open masking walk), NIM-743 (`soul-lint list-secret-paths`), NIM-744 (`examples/` and the WB redis service). **Amends [ADR-003](#adr-003-destiny-format-is-yaml-with-typed-schema-cuejson-schema) / [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) / [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files) / [ADR-062](#adr-062-named-input-types---reusable-named-input-schemes-via-types--type) / [ADR-083](#adr-083-a-secret-is-a-declared-state-field--the-author-never-writes-a-vault-path).**

### General mechanism

- A plugin is a separate executable file, supplied as an independent artifact (its own git repo, its own release pipeline, its own versions).
- Launched by the host process (`soul`-binary or `keeper`) as a sub-process (one-shot, [ADR-020(d)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format)).
- At startup, prints **handshake-string** to stdout - JSON in one line with the magic field `"soul_stack":"plugin-v1"` ([ADR-020(b)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format)).
- Then the host and plugin communicate via **gRPC via Unix domain socket** in the host-managed directory (`/var/run/soul-stack/plugins/` or `/var/run/soul-stack-keeper/plugins/`, mode `0700`).
- The plugin ends with graceful shutdown via SIGTERM (grace 10s → SIGKILL).
- Borrows the "one-line handshake → gRPC-over-socket" model from `hashicorp/go-plugin`, but **not** their code/format/MPL-2.0-license ([ADR-020(b)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format), [ADR-016](#adr-016-parity-strategy-and-soul-stack-license)).

### Three service contracts

| Contract | Who is the host | Who is the plugin | Destination |
|---|---|---|---|
| **`SoulModule`** | `soul`-binary | one executable in the alias-named slot | Implements Destiny steps: `Validate` / `Plan` / `Apply` (see Module Model). |
| **`CloudDriver`** | `keeper` | `soul-cloud-<provider>` | Creates/deletes/polls VMs in the cloud: `Schema` / `Validate` / `Create` / `Destroy` / `Status` / `List`. |
| **`SshProvider`** | `keeper` | `soul-ssh-<provider>` | Provides SSH credentials for `keeper.push`: `Sign` / `Authorize` (Vault SSH CA, static-key, Teleport - all fit into this contract). |

### Benefits of a single infrastructure

- One SDK per language (Go / Rust / Python) covers all three types of plugins.
- One method of distribution and caching (artifact-store + cache in master using SHA-256).
- One configuration method (manifest + JSON Schema parameters).
- Third parties can release their plugins (cloud provider for a niche cloud, custom module for a specific company) without modifying the Soul Stack core.

### Plugin directory in keeper config

Plugin registries (`modules`, `cloud_drivers`, `ssh_providers`) live in `keeper.yml` - Keeper resolves sources at startup, checks out `ref:`, pulls binaries into artifact-cache. The plugin version is always git ref (tag or branch), without semver-range, see [ADR-007](#adr-007-versioning-of-artifacts-is-done-through-git-ref-not-through-a-field-in-the-manifest).

The format of the block `plugins:` with all keys is in [`docs/keeper/config.md`](keeper/config.md). Contracts `CloudDriver` and `SshProvider` on the part of Keeper - in [`docs/keeper/plugins.md`](keeper/plugins.md). `SoulModule` (host = `soul`) - in [`docs/soul/modules.md`](soul/modules.md).

## Soul Stack artifacts: what's in git, what's in the database

A clear boundary between **code** (static, versioned with git tags, reviewed via PR) and **runtime-state** (specific instances, mutations via API/MCP, source of truth - Postgres):

| Artifact | Type | Where it lives | Managed as |
|---|---|---|---|
| **Service** | Definition (service type) | git, separate repo for the service | git tag → registry in master |
| **Destiny** | Definition (atomic brick) | git, separate repo on destiny | git tag → transitively via service.yml |
| **Module** (`soul-mod-*`, `soul-cloud-*`, `soul-ssh-*`) | Definition + binary | git sources + artifact-cache in master | release in git → master pulls binary |
| **Profile** | Runtime config | **Postgres** | API/MCP CRUD |
| **Provider** | Runtime config | **Postgres** | API/MCP CRUD |
| **Coven** | Runtime state | **Postgres** | API/MCP, or synchronized from incarnation |
| **Incarnation** | Runtime state (spec + state + status) | **Postgres** | API/MCP CRUD |
| **Soul** | Runtime state | **Postgres** | bootstrap via CSR, lifecycle via scenario |

Git is good for code: versioning, reviewing, history, branching. Bad for runtime-state: drift between git and fact, synchronization by polling, difficult to do atomic mutations. Therefore, everything that **changes during operation** lives in Postgres and is controlled via the API. Git remains for **code and definitions**.

Optional: the operator can export the incarnation to YAML and commit it to git **as a backup or audit-snapshot**, but this is not the primary path and is not a required feature.

## Destiny: entry contract and validation

Brief summary for the architectural context. Full analysis - in **[`docs/destiny/`](destiny/README.md)** (format `destiny.yml` and `tasks/main.yml`, task fields, `input:`-contract, testing).

- **The block format `input:`** is a general standard **[`docs/input.md`](input.md)**, applies equally to destiny, scenario and module manifest. Any DSL extension is propose-and-wait → edit [`input.md`](input.md) → then everything else.
- **Destiny-specific block `input:`** (where the difference between `input.<name>` and `params:` module is validated, as available in templates) - in **[`docs/destiny/input.md`](destiny/input.md)**.
- **Two rounds of validation.** Keeper upon scenario invocation (fail fast, zero traffic to hosts) + Soul before apply (defense in depth against desynchronization). Details and role of `soul-lint` are in [`docs/destiny/input.md`](destiny/input.md).
- **`soul-lint`** checks statically: well-formed `input:`-block, literals in `when:` against `enum:`. It does not see specific runtime values; it is not linked to the Keeper runtime path ([ADR-004](adr/0004-binaries.md)).
- **Mini-open Q:** name of the block of the same name `params:` on the call-site of the scenario (`apply: { destiny: …, params: { … } }`) and `input:` destiny. Whether to agree on them is a separate issue.

## Service - structure and manifest

Service is a **service type** (Redis HA, PostgreSQL, Vector-collector). One service - one git repo, with its own versions, with a mandatory manifest and a set of scenarios.

### Repository layout

```
redis/
├── service.yml                         # manifest: state/host schemas, destiny, modules (no name: — ADR-0083/NIM-726) (version = git tag, see ADR-007)
├── vars/                               # the service's default parameters (see "Service vars: the assembly pipeline")
│   ├── _stack.yaml                     # OPTIONAL: declarative build pipeline
│   ├── _default.yaml                   # baseline for all incarnation
│   ├── coven/
│   │   ├── prod.yaml
│   │   └── dev.yaml
│   └── os/
│       ├── ubuntu.yaml
│       └── debian.yaml
├── scenario/                           # auto-discover from directory, directory name = scenario name
│   ├── _create/                        # OPT: shared bodies of the create family; a `_`/`.` name is NOT a scenario
│   │   ├── provision.yml               # `include: _create/provision.yml` from any scenario of the service
│   │   └── deploy.yml
│   ├── create/
│   │   ├── main.yml                    # entry point: input + tasks (all inline)
│   │   ├── standalone.yml              # reusable mode blocks (include from main.yml)
│   │   ├── templates/                  # OPTS: scenario-local templates (two-level resolve)
│   │   └── tests/                      # OPT: scenario tests (see scenario/orchestration.md)
│   ├── add_node/
│   │   └── main.yml
│   ├── remove_node/
│   │   └── main.yml
│   ├── reshard/
│   │   └── main.yml
│   └── restart/
│       └── main.yml
├── types.yml                           # OPTS: reused named input types (section types:, $type-link - ADR-062)
├── migrations/                         # the state_schema ladder; its top IS the version
│   ├── 002_split_system_and_operator_users/
│   │   ├── main.yml                    # description + transform
│   │   └── tests/                      # migration tests (see migrations.md)
│   └── schema.lock                     # GENERATED by `make schema-stamp`
└── ...
```

Each folder `scenario/<name>/` is a separate operation (CRUD-style) on the service — except one whose name starts with `_` or `.`, which is by convention a home for bodies several scenarios share and is never discovered as a scenario, main.yml or no main.yml. `main.yml` - scenario entry point: contains **inline** `input` and `tasks` (a state write is one of those tasks, [ADR-084](#adr-084-explicit-state-capture--corestateverb-replaces-end-of-run-state_changes)). Neighboring `*.yml` are sub-tasks, included through `include:` into `main.yml`; a target may also sit one level down (`_create/provision.yml`) or at the service level `scenario/` — full resolution rules in [`docs/scenario/orchestration.md §6`](scenario/orchestration.md). There is no need to list the scenarios in `service.yml` - keeper finds them with auto-discovery based on the directory structure.

### `service.yml` - manifest

> **Implementation status.** The `state_schema` below is written in the **input DSL**, the form
> agreed on 2026-09-01 ([ADR. One schema dialect](adr/0086-one-schema-dialect.md)) and **not built
> yet**: today's parser refuses this form outright — it still reads the JSON Schema envelope
> (`type: object` + `properties:` + a root `required: [names]`), and `validateStateSchema`
> (`shared/config/service.go:500-543`) demands a root `type: object`, emitting
> `state_schema_root_not_object` without one (`:509`, `:522`, `:534`). The misparse runs the other
> way: it is the **old** envelope read by the **new** parser that becomes two state fields called
> `type` and `properties`, which is why the old form will be refused by name
> (`state_schema_legacy_json_schema_form`) rather than left to parse. Implementation is NIM-742 (engine
> and the masking walk), NIM-743 (`soul-lint list-secret-paths`), NIM-744 (`examples/` and the WB
> redis service — the corpus, including `examples/service/redis/service.yml`, is still in the old
> form). The rest of the manifest is current.

```yaml
# No `name:` — a service is named at registration, not here (NIM-726).
# No `state_schema_version:` either — the version is the top of the `migrations/` ladder (NIM-735);
# the linter still requires the key until NIM-736 ships.

# Structure of incarnation.state in the database. A map <field name> → schema, the SAME
# dialect as `input:` — no `type: object`/`properties:` wrapper at the root, requiredness
# is `required: true` on the field, vocabulary is snake_case (ADR. One schema dialect).
state_schema:
  redis_type:
    type: string
    required: true
    enum: [sentinel, cluster]
  redis_version:
    type: string
  # redis_config — the opaque merged redis.conf map: `type: object` whose value shape is
  # carried by `additional_properties:` and which declares no `properties:` of its own.
  # ⚠ That shape has NO expressible form yet — an open question the ADR records rather
  # than answers (validateObjectSchema demands `properties:` unconditionally). Same for
  # sysctl_settings and redis_sentinel's two setting maps. See
  # adr/0086-one-schema-dialect.md before writing one.
  redis_users:                        # a TYPED array since migration 005_to_006 — not a map
    type: array
    items:
      type: object                    # ★ a NESTED object still declares type/properties
      additional_properties: false    # was additionalProperties
      properties:
        name:  { type: string, required: true }   # was a sibling `required: [name, perms, state]`
        perms: { type: string, required: true }
        state: { type: string, required: true, enum: [on, off] }
        password:                     # the value lives in Vault, never in state (ADR-0083 §1)
          type: secret
          key: name                   # the sibling property that addresses this element's secret
          label: "Redis user password"
  redis_hosts:
    type: array
    items:
      type: object
      properties:
        sid:  { type: string, required: true }
        role: { type: string, required: true, enum: [primary, replica, sentinel] }

# Dependency artifacts - ref: git tag or branch (see ADR-007).
# No semver-range - exact ref and nothing more.
destiny:
  - { name: redis, ref: v1.0.0 }      # mode-agnostic brick: install + render redis.conf
  # cloud-create is NOT a destiny dependency: this is a scenario step `core.cloud.provisioned`
  # (on: keeper, CloudDriver plugin), see ADR-017.

modules:                              # custom modules
  - { name: community.redis, ref: v1.0.0 }  # live Redis runtime (CONFIG SET, ACL, cluster, sentinel)

# OPTIONAL: Incarnation lifecycle policy (no block = both true).
lifecycle:
  auto_create: true                   # POST /v1/incarnations immediately runs scenario create
  auto_destroy: true                  # deletion runs the destroy teardown scenario (by allow_destroy)
```

Scenarios and their details are not mentioned in service.yml - keeper finds them from the contents of the `scenario/` directory.

`lifecycle:` - optional block of service incarnation life cycle policy:

- `auto_create: bool` (default `true`) — `POST /v1/incarnations` automatically starts scenario `create`; `false` - the incarnation is created in `ready` without running, the operator launches `create` manually from the Run form.
- `auto_destroy: bool` (default `true`) - deleting an incarnation launches the teardown scenario `destroy` according to the usual logic `allow_destroy`; `false` - deletion is always direct, without teardown, priority over `allow_destroy`.

Missing block = both `true` (backcompat).

**Scenario-conventions and lifecycle-set.** Lifecycle-set (`LifecycleScenarioNames`) = only `create` / `destroy` - specialized scenario-kinds of the corresponding life cycle phases. `converge` is an **operational** scenario-kind (launched by the usual `run` Apply-reconcile; its second role as a dry-run target left with NIM-446), it is derived from the lifecycle set (see [Amendment ADR-031 2026-06-10](#adr-031-scry--drift-detection-declarative-dry-run-reconcile)). The scenario directory (`GET /v1/services/{name}/scenarios`) carries the field `runnable: bool` - the sign "launched by the operator from the Run form": `create` = `true`, `destroy` = `false` (special deletion flow via `DELETE /v1/incarnations/{name}`), operational (incl. `converge`) = `true`. The UI filters the Run form by `runnable`, not by name hardcode ([ADR-042](adr/0042-backend-driven-ui.md)).

> `service.yml` carries **neither** version. The service version is the git-tag under which the file itself is committed (see [ADR-007](#adr-007-versioning-of-artifacts-is-done-through-git-ref-not-through-a-field-in-the-manifest)). The state-schema version is a **different** concept (version of the state data structure) and is not written down anywhere either — it is the top of the `migrations/` ladder, see section below ["Versioning and migrations state_schema"](#versioning-and-state_schema-migrations).

### `scenario/<name>/main.yml` - self-contained operation

The complete regulatory specification of the orchestration layer (`on:`/`where:`, probe-idiom, two-level resource resolution, tests, barrier/state-commit) is [`docs/scenario/`](scenario/README.md). DSL task core (`module:`, `include:`, `block:`, `async:`, `loop:`, `register:`, requisites, `retry:`, `timeout:`, `changed_when:`/`failed_when:`) scenario inherits entirely from [`docs/destiny/tasks.md`](destiny/tasks.md) - after [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) the "scenario only `apply:`" invariant has been removed. Below is an illustration of the format.

> **Implementation status.** The `input:` block's dialect is current — what is **not built** is the
> removal of the object-level list form `required: [names]`, which leaves the whole DSL and is
> written below as `required: true` per property ([ADR. One schema
> dialect](adr/0086-one-schema-dialect.md), refused as `input_required_list_removed`;
> implementation NIM-742, corpus migration NIM-744). Both forms parse today; only one will.

```yaml
name: create
description: Initial bootstrap of Redis HA cluster

# Typed scenario input - validated before run.
# Block format is docs/input.md standard.
input:
  redis_version:
    type: string
    pattern: "^[0-9]+\\.[0-9]+\\.[0-9]+$"
  redis_users:
    type: object
    required: true
    additional_properties:
      type: object
      properties:
        acl:   { type: string, required: true }   # was a sibling `required: [acl, state]`
        state: { type: string, required: true, enum: [on, off] }
  redis_password:
    type: string
    required: true
    secret: true
    pattern: "^vault:.*"              # a link to Vault is required
  spawn:                              # optional: for cloud-create
    type: object
    properties:
      provider: { type: string }
      profile:  { type: string }
      count:    { type: integer, min: 3, max: 6 }

# Steps - the module address decides the side; on: selects covens ([coven,...] / omitted)
tasks:
  - name: provision
    when: input.spawn != null
    module: core.cloud.provisioned    # keeper-side core (ADR-017) — the address routes it
    state: created
    params:
      provider: "${ input.spawn.provider }"
      profile:  "${ input.spawn.profile }"
      count:    "${ input.spawn.count }"

  - name: install-redis
    # on: omitted = the whole incarnation (all member hosts)
    apply:
      destiny: redis
      input:
        version:  "${ vars.redis_version }"
        password: "${ input.redis_password }"

  - include: replication.yml

  # What the scenario writes to incarnation.state - a keeper-side step standing
  # where the value becomes known (ADR-0084). The state suffix IS the verb:
  # set/present/add/append/modify/remove/unset (the ADR-057 verb set).
  # scenario/orchestration.md §7.1.
  - name: record what we deployed
    module: core.state.set
    params:
      field: redis_version
      value: "${ input.redis_version }"

  - name: record the configured users
    module: core.state.set
    params:
      field: redis_users
      value: "${ input.users }"
```

The `on:` key decides where the step is executed: `keeper` - locally on the keeper, `[coven, …]` - intersection of covens (⊆ incarnation), omitted - the entire incarnation. Volatile per-host filter based on `register:` of the previous probe - key `where:` ([ADR-008](adr/0008-coven-stable-tags.md)). One scenario mixes keeper and host steps in a linear flow, in the same task language ([ADR-009](adr/0009-scenario-dsl.md)). It is one linear task language for both keeper and host steps. Normative semantics - [`docs/scenario/orchestration.md`](scenario/orchestration.md).

The `input:` block validates the scenario input parameters before running (according to the [docs/input.md](input.md) standard). A write to `incarnation.state` is a **step**, not a section: `module: core.state.<verb>` (keeper-side by its address, no `on:` key), where the state suffix is the verb — `set`/`present`/`add`/`append`/`modify`/`remove`/`unset` ([ADR-0084](#adr-084-explicit-state-capture--corestateverb-replaces-end-of-run-state_changes), the [ADR-057](#adr-057-state_changes---ordered-list-of-crud-verbs) verb set). It declares **what** field is written and **from where** the value comes (CEL `${ … }` in `params:`), it stands where the value becomes known, and it lands **at that step** rather than at an end-of-run commit; plurality — through a `match:` predicate. Normative — [scenario/orchestration.md §7.1](scenario/orchestration.md#71-the-capture-verbs).

**Reusable named types** - for composite schemas found in multiple service scenarios ([ADR-062](adr/0062-input-types.md)). The type is declared in the service-level file `service/<name>/types.yml` (section `types:`, same input-DSL), and the scenario refers to it with the directive `$type`:

```yaml
# service/<name>/types.yml
types:
  AclUser:
    type: object                      # a declared type is a schema NODE, so it keeps type/properties
    additional_properties: false
    properties:
      name:  { type: string, required: true, pattern: "^[a-zA-Z0-9_-]+$" }
      perms: { type: string, required: true }
      state: { type: string, required: true, enum: [on, off] }
```

> **Implementation status.** The per-property `required: true` above is the agreed form and is **not
> built**: the object-level `required: [name, perms, state]` still parses today. The catalog is now
> shared by both contracts — one `AclUser` can serve a scenario's `input:` and a `state_schema`
> field at once — which is what the [one-schema-dialect ADR](adr/0086-one-schema-dialect.md) buys,
> together with `type: secret` becoming legal in a shared type (the **input** side of that is
> decided and built — NIM-751: off the form, never required, a supplied value refused) and `$type`
> accepting the referring node's
> own `properties:` in `state_schema` only, merged add-only and fail-closed. Implementation
> NIM-742; corpus migration NIM-744.

```yaml
# scenario/add_user/main.yml
input:
  user:
    $type: AclUser            # single object of declared type

# scenario/create/main.yml
input:
  users:
    type: array
    items:
      $type: AclUser          # array of declared type
    min_items: 1
```

`$type` resolves service-level at the input stage (with cycle-detection); We keep the types inline in one scenario, and only include reused ones in `types.yml`. The previous provision `$ref` for an external JSON-Schema file in `schemas/` **cancelled** (not implemented, replaced by named types) - see [ADR-062](#adr-062-named-input-types---reusable-named-input-schemes-via-types--type) and [docs/input.md → "Reused named types"](input.md#reusable-named-types-types--type).

## Service vars: the assembly pipeline

A service's own default parameter values live in `<service>/vars/` and read in CEL as `vars.*`. Regulatory spec — [ADR-0082](adr/0082-service-vars.md); the authoring view — [`docs/service/manifest.md` → Service vars](service/manifest.md#service-vars).

### Lexical default (without `_stack.yaml`)

Every `*.yaml` / `*.yml` **directly inside** `vars/`, sorted lexically, deep-merged in that order. Subdirectories are not walked — `soul-lint` reports `vars_dir_nested` rather than letting a layer sit somewhere the resolver will never look.

The base file is **`00-base.yaml`**. The name is load-bearing: once the directory listing IS the order, the filename carries the precedence, and a numeric prefix leaves room to insert a layer between two others without renaming either. (`_` sorts at `0x5F`, between `Z` and `a`, so the old `_default.yaml` would land in the middle of an alphabetical set rather than at its head.)

> **Service vars are role-agnostic** ([ADR-008](adr/0008-coven-stable-tags.md)) — there is no `role/<Y>.yaml` stage. Role-dependent parameters move to destiny and are passed through `input:` after the probe (see [`docs/scenario/concept.md`](scenario/concept.md)).

> **There is no operator rung.** Nothing overrides these from outside: no field on the incarnation, no API, no successor to one. A fleet that needs different defaults **forks the service repo** and re-pins its `ServiceRef` ([ADR-007](adr/0007-versioning-git-ref.md)) — the ansible-role model. What an operator supplies is `input:`, and only `input:`.

### `_stack.yaml` — the declarative pipeline

When a layer is conditional, iterated, or computed, write `vars/_stack.yaml`. Present, it replaces the lexical order entirely; `_stack.yaml` itself is never a layer.

```yaml
# vars/_stack.yaml
#
# Available at every step, and NOTHING else:
#   incarnation - { name, service, service_version, covens, traits }
#   vars        - the layers accumulated by the steps before this one
#   <as>        - the foreach binding, inside a foreach step

stack:
  # 1. The base — always
  - file: 00-base.yaml

  # 2. Iterate over the incarnation's coven labels
  - foreach: "${ incarnation.covens }"
    as: coven_name
    file: "coven-${ coven_name }.yaml"
    optional: true                     # skip silently when the file is absent

  # 3. Depends on a value an earlier step already put in
  - file: "env-${ vars.env }.yaml"
    when: vars.env != null
    optional: true

  # 4. A conditional inline block, no separate file
  - inline:
      redis_maxmemory_percent: 60
    when: vars.redis_maxmemory_percent == null

  # 5. Replace a map wholesale instead of merging into it
  - file: 90-mirror.yaml
    strategy: replace
```

**`soulprint` is refused outright** — it is not declared in that environment, so naming it is a compile error rather than a silently empty map. Service vars resolve **once per run** and the result is handed to every host; a step keyed on one host's facts would apply that host's answer to the whole roster, which a mixed debian/rhel roster makes obvious. `input`, `register` and `compute` are refused for the same structural reason. Host-dependent behaviour belongs where the render already is per-host: `where:` on a task, a task's own `vars:`/`params:` over `soulprint.self.*`, or a `.tmpl`.

### Pipeline operators

| Operator | Purpose |
|---|---|
| `file: <path>` | Include a file from `vars/`; the path is a template. Escaping `vars/` is refused. |
| `inline: <map>` | Include a set of values without a separate file. |
| `when: <expr>` | Inclusion condition (boolean expression). |
| `optional: true` | Do not fail when the `file:` is absent. |
| `foreach: <list>` + `as: <name>` | Repeat the step per element, binding `<name>` inside it. |
| `strategy: deep\|replace` | How this step merges. `deep` (default) recurses into nested maps; `replace` swaps the whole value. Also settable per key with `_strategy` inside a layer file. |

Between steps the accumulated `vars.*` is re-evaluated, so a later step can reference a key an earlier one contributed.

The decoder is **strict**: an unknown key in a step is an error, not a silent drop. `whn:` would otherwise remove a gate and `strateg:` would quietly deep-merge — both changing what gets applied to a host while the file still looked right.

### Where the merge continues

The service's resolved `vars/` is the **bottom** of one flat `vars.*` namespace: `<service>/vars/*.yaml` → destiny `vars.yml` → `block:` → task, each layer overriding the one below and able to reference it. Across an `apply:` boundary the name survives but the meaning does not — inside a destiny pass `vars.*` is that destiny's own `vars.yml` and nothing else ([`docs/destiny/vars.md`](destiny/vars.md)).

## Incarnation — runtime service instance

Incarnation is a specific **instance** of a service in reality (one Redis cluster, one PostgreSQL cluster). Stored in Postgres, managed via API/MCP.

### Record structure

| Field | Type | Meaning |
|---|---|---|
| `incarnation_id` | UUID | primary key |
| `name` | text UNIQUE | incarnation name (global PK). **Not** a Coven — membership lives in `incarnation_membership` ([ADR-008 amendment 2026-07-17](adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)) |
| `service` | text | service name |
| `service_version` | text | pin version (git-tag) of the service under which incarnation runs |
| `state_schema_version` | integer | version of state_schema under which state is structured |
| `state` | jsonb | current structured configuration, by `state_schema` service |
| `status` | enum | `provisioning` / `ready` / `applying` / `error_locked` / `migration_failed` / `drift` / `destroying` / `destroy_failed` |
| `status_details` | jsonb NULL | error details if `status` locking |
| `created_by_aid` | text FK on `operators(aid)` | who created |
| `created_at`, `updated_at` | timestamptz | audit |

### `state_history` — state change log

Separate table, snapshot for each successful state change:

| Field | Type | Meaning |
|---|---|---|
| `history_id` | UUID PK | |
| `incarnation_id` | UUID FK | |
| `scenario` | text | what scenario led to the change |
| `state_before` | jsonb | state up to |
| `state_after` | jsonb | state after |
| `changed_by_aid` | text FK on `operators(aid)` | who initiated |
| `at` | timestamptz | when |

This gives: rollback to any previous state, audit of "who added user X and when," compliance reports.

### Atomicity and `error_locked`

**The scenario does not write to the database until it has run on all target hosts**. If apply failed partially (for example, `add_user` passed on 2 out of 3 hosts), the state in the database **is not updated**, incarnation goes to `status: error_locked` with `status_details: { failed_hosts: [...], partial_changes: {...} }`. Any subsequent scenario for this incarnation is rejected until express permission from the operator.

Permission:
- `keeper.incarnation.unlock name=X reason="manual cleanup verified"` - the operator assumes that he has verified the fact on the hosts.
- Special recovery scenario - service can declare it to automate typical cases (clean up, roll back, forward).
- `keeper.incarnation.rerun-last name=X reason="..."` (REST: `POST /v1/incarnations/{name}/rerun-last`, permission `incarnation.rerun-last`) - atomically removes `error_locked` (state is not touched, last-known-good, snapshot in `state_history`) and with the same action restarts **last fallen scenario** incarnations: this is the create option (`create`/`create_from_souls`) for a bootstrap file or any day-2 scenario. For day-2, input is restored from the `apply_runs.recipe` failed run; if the recipe is unavailable (retention/legacy) - `409` fail-closed "Unlock + manual start". Under one `FOR UPDATE`: transition `error_locked → applying` bypassing `ready` - eliminates the window in which a concurrent run would slip into the vacated `ready`. The restart starts in the "reserved `applying`" (`RunSpec.FromLocked`) mode: the runner does NOT transit the status again, but verifies that the line in `applying` is fail-closed, another status rejects the start without applying the scenario over the intercepted recovery line. The "operator explicitly confirmed" invariant is preserved by the mandatory `reason` + confirm in the UI; audit event `incarnation.rerun_last` (does NOT reuse `incarnation.unlocked`). For a bare incarnation without a fixed scenario and with an inaccessible recipe, the usual `unlock` + manual repeated run remains.

This is a deliberate design choice: partial applications do not quietly pass and drift does not silently accumulate. Soul Stack is fail-fast, with a clear point of operator responsibility.

### Statuses `destroying` and `destroy_failed`

Teardown incarnation (`keeper.incarnation.destroy`) goes through two statuses (implemented, `keeper/internal/incarnation`, CHECK `incarnation_status_valid` - migrations 005 + 031 + 036):

- **`destroying`** (S-D1) - the operator initiated destroy: teardown was launched through scenario `destroy` (S-D2b) followed by `DELETE` lines (S-D3). This is **not the terminal of the line itself** - if successful, the line is deleted (single-winner `DELETE … WHERE status='destroying' RETURNING`, see ADR-027(j)), if teardown fails, it goes to `destroy_failed`. From `destroying` all other operations (`run` / `upgrade` / repeated `destroy`) are rejected fail-closed; The force-path removes the line immediately. Recovery of "dangling `destroying`" (dead owner) - the same single-winner `DELETE`, see ADR-027(j).
- **`destroy_failed`** (S-D2a) — teardown (scenario `destroy`) crashed on hosts: instance **not deleted**, `state` remains last-known-good (teardown works with hosts, not with jsonb-state). A terminal that requires operator intervention: from it the operator repeats destroy, force-destroys or removes in `ready`. Separate status (and **not** `error_locked`), because the semantics and recovery paths are different - partial teardown failure ≠ partial apply failure.

`migration_failed` — terminal of failed state_schema migration ([Versioning and state_schema migrations](#versioning-and-state_schema-migrations), ADR-019): `ROLLBACK`, state is not saved, incarnation is locked; same value in CHECK-constraint and `ValidStatus` (`keeper/internal/incarnation`).

Current implemented enum (`ValidStatus` + CHECK `incarnation_status_valid`) = `ready` / `applying` / `error_locked` / `migration_failed` / `destroying` / `destroy_failed`. **`drift`** is entered into `ValidStatus` + CHECK migration according to [ADR-031(d)](#adr-031-scry--drift-detection-declarative-dry-run-reconcile) - **informational, NOT blocking** status (remediation = normal apply from `drift` → `ready`). Since NIM-446 removed the Scry circuit its only writer is the legacy branch of `incarnation.upgrade`: the DB state moved ahead of the hosts. **`provisioning`** in the table - **post-MVP** (phase not yet implemented): in the enum directory above, but not yet accepted by the code - will appear in `ValidStatus`/CHECK when the corresponding phase is implemented.

### Operator API

```
keeper.incarnation.create name=X service=Y input={...}      # run create scenario
keeper.incarnation.run    name=X scenario=add_user input={...}  # any other scenario
keeper.incarnation.get    name=X                            # spec + state + status
keeper.incarnation.list   filter={...}                      # list of instances
keeper.incarnation.history name=X                           # state_history
keeper.incarnation.unlock name=X reason=...                 # removing error_locked
keeper.incarnation.upgrade name=X to_version=v2.0           # transition to a new version of service
keeper.incarnation.destroy name=X                           # deletion
```

The same set is available through MCP-tools. The operator looks at the state object, sees "here are the users of the redis cluster", adds one - this is `incarnation.run scenario=add_user`.

## Targeting and host communication

Rewritten under [ADR-008](#adr-008-coven---stable-logical-tags-only). The full regulatory targeting specification is [`docs/scenario/orchestration.md`](scenario/orchestration.md); here is the architectural summary.

### Coven - stable logical tags

Coven - **only stable** logical tags (cluster / project / environment / data center / hardware type). `incarnation.name` is **not** a Coven ([ADR-008 amendment 2026-07-17](adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)): incarnation **membership** is a first-class relation `incarnation_membership(incarnation_name, sid)`, not the fact that a host carries a coven equal to the incarnation name. Coven and membership are two separate axes.

**There are no more sub-covens by role (`{incarnation.name}-{role}`)** - convention removed ([ADR-008](adr/0008-coven-stable-tags.md)). The role (master / replica) **not Coven**: it is volatile (failover) and is not suitable for a stable label. Stable covens (for example, `baremetal`, `prod`) are assigned declaratively via incarnation; the operator does not make separate "tag host" API calls.

### `on:` - stable step target

Scenario step target - key **`on:`**, resolved by Postgres (stable layer):

```yaml
# Entire incarnation (on: omitted - all member hosts via the membership relation)
- name: Apply base config everywhere
  apply: { destiny: redis-base, input: { ... } }

# Local task on the keeper itself (cloud-create, vault-resolve, http-call)
- name: Provision VMs
  module: core.cloud.provisioned
  state: created
  params: { ... }

# Intersection (AND) of stable covens, always ⊆ members
- name: Tune kernel on bare-metal hosts of this cluster
  on: [baremetal]                  # incarnation scope is implicit (roster is membership-scoped)
  apply: { destiny: kernel-tuning, input: { ... } }
```

**Resolver contract** (invariant): the omitted `on:` = all **members** (via `incarnation_membership`); `on: ["${ incarnation.name }"]` is a **validation error** (`incarnation.name` is not a Coven); a list in `on:` - AND/intersection of stable covens, result **always ⊆ members**; **cross-incarnation targeting is prohibited by construction** (enforced by the membership roster); role in `on:` is not involved. Completely - [`docs/scenario/orchestration.md §3`](scenario/orchestration.md).

### `where:` - volatile role via probe + register

The volatile role (who is now master) is not stored anywhere stably. The scenario puts a **probe step** (`module: core.exec.run` + `register:` + `changed_when: false` + `failed_when:` for completeness), then targets the next step with the key **`where:`** - a volatile predicate on `register:` of this probe, per-host:

```yaml
- name: Detect actual redis role per host
  module: core.exec.run                # on: omitted = all member hosts
  register: redis_role
  changed_when: false
  params: { command: "redis-cli role | head -1" }

- name: Restart only the current replicas
  where: register.redis_role.stdout == 'slave'   # on: omitted = all members
  module: core.service.restarted
  params: { name: redis-server }
```

Two-phase resolve: `on:` → Postgres (stable), `where:` → `register:` (runtime). After failover - just a new probe; no cache, role collector or volatile Soulprint facts ([ADR-008](adr/0008-coven-stable-tags.md)). `where:`-step key and `soulprint.where(...)`-function in the expression - **different positions** (on which hosts vs where to get the data), detailed in [`docs/scenario/orchestration.md §4`](scenario/orchestration.md). The former `filter:` from the old examples was removed and replaced with `where:`.

In the template context of the scenario, the following are always available:
- `incarnation.name` - instance name.
- `input` - parameters passed to the scenario.
- `vars` — one flat namespace: the service's resolved [`vars/`](#service-vars-the-assembly-pipeline) at the bottom, then the scenario's `vars:`, a `block:`'s, and the task's.
- `state` - current state from the database (for scenarios that read the existing state).
- `soulprint.hosts` - list of run hosts with stable facts (`sid`/`role`(declared)/`network`/`os`/`covens`); `.where("<predicate>")` filters by CEL predicate string. A shortened form of the same request is `soulprint.where("<predicate>")` (for example, `soulprint.where("'X' in covens")` instead of `soulprint.hosts.where("'X' in covens")`). Scenario-only; receive destiny topology only through explicit `apply: input:`. Regulatory - [`docs/scenario/orchestration.md §4.1`](scenario/orchestration.md).

> **Difference from the destiny template context.** Both spell it `vars.*`, and the name means different things on the two sides of an `apply:` boundary ([ADR-0082](adr/0082-service-vars.md)). In a destiny, `vars.*` is [that destiny's own `vars.yml`](destiny/vars.md) and nothing else — it is isolated and receives only what came in `input:`. In a scenario, `vars.*` additionally holds the service's resolved `vars/` underneath the scenario's own locals, which is why a scenario has values to put into a destiny's `input:` when it calls `apply:`. The asymmetry is stated rather than hidden: it is the same one that forced `apply_when_dynamic_unsupported`.

### Host communication via Soulprint

When a host needs data from another host (cross-host data access), the `soulprint.where(<predicate>)` function is used on the **stable** layer (the CEL predicate is a static string literal):

```yaml
master_addr: "${ soulprint.hosts[0].network.primary_ip }"
```

All members of the run are simply `soulprint.hosts` (the accessor is incarnation-scoped); a stable-coven filter is `soulprint.where("'<X>' in covens")`. `incarnation.name` is not a Coven, so `soulprint.where("incarnation.name in covens")` is removed ([ADR-008 amendment 2026-07-17](adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)). The request goes to Postgres + Redis hot layer. Soulprint after [ADR-008](#adr-008-coven---stable-logical-tags-only) stores **only stable** facts, so `soulprint.where(...)` operates on a stable layer; volatile role (who is now master) - exclusively through probe + `where:`-key, not through Soulprint. The predicate `.where(...)` is a static string literal, expanded at the compile phase into the native CEL filter-comprehension (not runtime; dynamic merging of the predicate is prohibited, the first element is `[0]`, see [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files)). Cross-host master discovery - through the accessor `soulprint.hosts` (`soulprint.hosts.where("role == 'primary'")[0].network.primary_ip`), the declared role is taken from the host's Choir Voice ([ADR-044 amendment 2026-07-30](adr/0044-choir.md#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role)), a probe is defined on the runtime master (as in `restart`); normative - [`docs/scenario/orchestration.md §4.1`](scenario/orchestration.md) (the former open Q is closed there, see [§8](scenario/orchestration.md)).

## Versioning and state_schema migrations

> We are only talking about the **structure version `incarnation.state`** - this is not a "service version". The version of the service itself as an artifact is the git tag under which it is committed (see [ADR-007](#adr-007-versioning-of-artifacts-is-done-through-git-ref-not-through-a-field-in-the-manifest)). The state-schema version stays a separate entity, needed **only** for jsonb-state migrations to the database, and coexists with the service's git tags independently — but it is no longer a manifest field: it lives in the service repo as the top of the `migrations/` ladder, and in Postgres as the `incarnation.state_schema_version` column recording where a given incarnation's state sits.

Service developer changes `state_schema` between versions: adds a field, renames it, changes the structure of nested objects. Existing incarnations in the database store state according to the **old scheme** - you need to migrate.

### The ladder `migrations/` and the derived version

A migration step is a directory `migrations/<NNN>_<slug>/` holding `main.yml` and its `tests/`. The number is the version the step leads to; the "from" is derived — the ladder is forward-only and goes by one.

In the service repo:
```
migrations/
  002_split_system_and_operator_users/
    main.yml          # description + transform; no from_version, no to_version
    tests/
      splits-system-and-operator-users.yml
  schema.lock         # GENERATED
```

The state-schema version is not stored anywhere — it is the top of the ladder. An empty `migrations/` means version 1. `service.yml` carries no `state_schema_version:` key; writing one is an error (NIM-735, [ADR-019](#adr-019-state_schema-migration-dsl)) — planned, NIM-736, not implemented yet: until then the linter still **requires** the key and refuses a manifest without it.

`schema.lock` is generated next to the ladder: `version` (the top of the ladder at stamp time) and `fingerprint` (a hash of the **parsed and canonicalized** `state_schema`, never of the file text — a comment would break a text hash). Written by `make schema-stamp` and read by `soul-lint` on the **service repo's** own `make validate` (wherever that repo runs `soul-lint validate-service`) — both targets live in the service repository, not in this core repo. Layout and lock are specified in [`docs/migrations.md`](migrations.md); the tooling itself is not built yet (NIM-737).

The bundled services under `examples/` still carry the pre-NIM-735 flat form (`<NNN>_to_<MMM>.yml` beside `<NNN>_to_<MMM>/tests/`); they are relocated by NIM-738.

In the incarnation table there is a field `state_schema_version`, fixed upon creation.

### DSL migrations

Full regulatory specification - [`docs/migrations.md`](migrations.md), solution commit - [ADR-019](#adr-019-state_schema-migration-dsl).

MVP grammar: flat (`rename`/`set`/`delete`/`move`) + CEL expressions in `set:` values via `${ … }` marker + structural `foreach` for iteration over collections. Conditional `if:` key - deferred until the first real request (extension without breaking change). Complex scenarios not covered by the grammar are not supported in MVP (escape module `state.migrate` was rejected by [ADR-019(e)](#adr-019-state_schema-migration-dsl) as not from the dictionary; candidate `core.incarnation.state-migrate` - if necessary, a separate ADR).

Example (the same step to version 2, rewritten for MVP grammar):

```yaml
# migrations/002_<slug>/main.yml
description: redis_users turns from an array of strings to a map with acl and state

transform:
  - rename: { from: state.redis_users, to: state.redis_users_legacy_v1 }

  - foreach: "${ state.redis_users_legacy_v1 }"
    as: user_name
    do:
      - set:
          path: "state.redis_users.${ user_name }"
          value:
            acl: "off ~* &* +@all"
            state: "off"

  - delete: { path: state.redis_users_legacy_v1 }
```

Execution context of migration-CEL: `state.*` (mutable) and `<as-name>` inside `foreach.do[*]` are available. Prohibited: `vault(...)`, `now()`, `register.*`, `soulprint.*`, `vars.*`, `input.*` - migration = pure function from the old state, side-effect-free.

### Upgrade - an explicit operator step through the UI

Migration **does not run automatically when applying scenario**. This is an explicit operator operation:

```
keeper.incarnation.upgrade name=X to_version=v2.0
```

What happens in the UI/CLI:
1. Service is connected to master, visible in the UI with known versions (master sees git tags).
2. incarnation displays the current `service_version` and available updates.
3. The operator selects `v2.0`, clicks "upgrade".
4. Keeper in one transaction: runs the ladder steps `002_<slug>`, `003_<slug>` in a chain, updates `state_schema_version` and `service_version`, writes a snapshot to `state_history` with the mark `migration`.
5. If the migration fails - `status: migration_failed`, no changes in state are saved, incarnation is locked, the operator investigates.

After a successful upgrade, the scenarios continue to work with the new schema. The old one remains as a legacy snapshot in `state_history`.

### Compatibility scenarios

The scenario is tied to the **service version** under which it is launched. After upgrade incarnation to a new service-version, old scenarios are no longer called - the operator works with new ones. This is intentional: mixing scenarios of different versions is the way to drift.

## Cloud integration via `keeper.cloud`

Dynamic VM creation is implemented as a **cloud-create-scenario step** with `on: keeper` via the CloudDriver plugin. Service does not know the specifics of clouds - it knows "the step of creating a VM with parameters is needed," Keeper selects a driver and executes it.

Git/DB boundary: **Provider** (configured cloud account) and **Profile** (reusable VM template) live in Postgres, controlled via API/MCP - this is a runtime config, not code. A service's default parameters live in its git repo's `vars/` and are not overridable from outside — a fleet that needs different ones forks the service repo ([ADR-0082](adr/0082-service-vars.md)). The profile parameters are validated against `profile_schema`, which CloudDriver publishes via RPC `Schema()`.

Destroy operations are protected by a mandatory set of guard-rails (tombstone period with `tombstone_ttl`, confirm-flag, storage protection, audit) - one typo in `count` should not erase the rest.

Full breakdown of Provider/Profile, scenario step `core.cloud.provisioned`, coven binding and destroy security - in [`docs/keeper/cloud.md`](keeper/cloud.md). The `CloudDriver` contract and plugin directory are in [`docs/keeper/plugins.md`](keeper/plugins.md).

## Reaper

Background task inside `keeper`, cleaning the database from garbage and maintaining registry invariants. Not a separate binary. Works only on one Keeper instance at a time - the leader is selected via Redis-lease. Valid **only on Postgres**: it does not go to hosts via SSH, host-based module cache cleaning is described separately in [`docs/soul/modules.md`](soul/modules.md).

If the scope in the future grows beyond the scope of the cleanup (table migrations, transfer of archive records, GC of cold data) - reserve the name **Charon** for a broader process; so far there is only one name - **Reaper**.

Properties, rules (`expire_pending_seeds`, `purge_used_tokens`, `purge_souls`, `purge_old_seeds`, `mark_disconnected`), complete YAML config and metrics - in [`docs/keeper/reaper.md`](keeper/reaper.md).

## Delivery of SoulSeed token to the host

"The operator generated a bootstrap token → the token ended up in a file on the VM where `soul` will start" - this is the operational step between issuing the token and starting Soul. In itself, it is not dictated by Soul Stack: the method of physical delivery is the choice of the operator. The target path is through `keeper.push` (unified audit, RBAC, logs in Keeper); SSH/SCP, CI/CD pipeline and cloud-init are allowed as alternatives.

The protections that ensure the security of a token for any delivery are short TTL (24h by default), one-time use (burning on the first CSR), binding to a specific SID. For additional protections beyond this, see the open question "SoulSeed Token Leakage" in [Open Questions](#current).

Full list of delivery scenarios, permission requirements and file mode, recommendation for systemd `LoadCredential=` - in [`docs/soul/onboarding.md`](soul/onboarding.md).

## End-to-end installation scenario

Reference path from empty infrastructure to managed Souls. Each step is linked to a section where it is described in detail.

1. **Postgres and Redis are raised** - external dependencies of the Keeper cluster (see ADR-005, ADR-006).
2. **A Keeper cluster is being raised** - one or more `keeper` instances on top of a common PG+Redis. The KID of each instance is fixed in the config.
3. **Bootstrap of the first Archon** - the operator launches `keeper init --archon=<aid>` on the Keeper host. The command under PG advisory lock checks that the registry `operators` is empty, creates the first Archon with the role `cluster-admin` (`permissions: ["*"]`), issues a JWT token (TTL 30 days) and puts it in the file `mode 0400`. Before this step, Keeper refuses to start with operators=empty without `--initialize`. See [ADR-013](#adr-013-bootstrap-of-the-first-archon) and [ADR-014](#adr-014-operator-identity-model-archon).
4. **The Archon writes out the remaining operators** - through OpenAPI/MCP with `Authorization: Bearer <jwt>`, ordinary Archons with limited rights under Covens are created (FK `created_by_aid` in the registry `operators`).
5. **Operator adds Soul** - via OpenAPI/MCP `keeper`: specifies SID (FQDN), desired `transport` (`agent` or `ssh`), Coven tags. The entry in `souls` appears in status `pending`.
6. **For `transport: agent`:** the operator receives a short **bootstrap token** (not a certificate, not a key - only a one-time use token). Delivers to the VM along with the `soul` binary - the target path via `keeper.push` (the same SSH mechanism as for agentless-Destiny, but with the "deploy pull agent" task); alternative paths are cloud-init, regular SSH (see "Delivering a SoulSeed token to the host").
7. **Soul starts on VM:**
   - reads a token from a file,
   - generates a private key locally (never leaves the host),
   - generates CSR,
   - connects to Keeper at the address from the config,
   - presents the token and CSR,
   - receives SoulSeed (signed certificate),
   - puts it next to the private key, burns the token,
   - opens a gRPC bidi stream with SoulSeed as an mTLS identity.
8. **The entry in `souls` goes to `connected`**, the active seed appears in `soul_seeds`.
9. **Next is the usual work:** Keeper pushes Destiny on the stream, Soul applies, reports back with events. SoulSeed rotates once a week on a live stream without the participation of an operator.
10. **For `transport: ssh`:** steps 6-8 are skipped. The operator immediately does `POST /v1/push/apply` ([request/response normalization](keeper/operator-api/push.md)) with inventory and Destiny, Keeper goes to the host via SSH, executes it, takes the results. Soul agent is not installed on the host.
11. **Reaper** periodically cleans the registry of debris (see "Reaper").

## Top level data flow

1. The operator writes Destiny and service vars in git, runs `soul-lint` locally and in CI (render → schema validation → static analysis). Without the green `soul-lint` nothing goes outside.
2. Destiny reaches Keeper via OpenAPI or MCP. Keeper checks RBAC, re-renders and validates, puts it in the Destiny (Postgres) registry.
3. **Pull:** Keeper pushes the command "apply such and such Destiny with such and such parameters" to the corresponding Souls (agent transport) on the live gRPC stream.
   **Push:** Keeper for each target host (`transport: ssh`) raises an SSH session through the selected provider, performs the steps, takes the result.
4. Soul (or push session) applies, reports events (start, step, success/failure).
5. Keeper aggregates the result, exposes it externally via OpenAPI/MCP, publishes metrics and traces (OTel).
6. Soulprint is collected by Soul periodically and on demand, stored in Keeper (Postgres), available RBAC-filtered.

## End-to-end requirements and where they land

| Requirement (from [docs/requirements.md](requirements.md)) | Where it lives | Notes |
|---|---|---|
| Metrics | All three binaries | Normalized [ADR-024](adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge): Prometheus-primary (pull `/metrics`), namespace prefixes `keeper_*` / `soul_*`. OTLP-push metrics - optional bridge. Spec - [observability.md](observability.md). |
| OpenTelemetry | All three binaries | Normalized [ADR-024](adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge): OTel-bridge for traces (end-to-end operator → Keeper → Soul via gRPC metadata) + opt. push metrics; resource-attrs `service.name` + `soulstack.kid` / `soulstack.sid`. Spec - [observability.md](observability.md). |
| Hot-reload config + rewrite to disk | All three binaries | The mechanism is standardized [ADR-021](#adr-021-hot-reload-config-with-write-back-yaml): file-edit (SIGHUP) + API/MCP with write-back YAML, validation pipeline parse → schema → semantic → atomic swap, audit-events `config.reload_succeeded` / `config.reload_failed`. History - git-blame + audit (DB table `config_history` deferred). |
| Log rotation | All three binaries | Built-in by default, without dependence on external logrotate. |
| Vault | Keeper (full: service-vars secrets, CA for SoulSeed, SSH provider); Soul (short-lived token client only) | Soul should not have the right to read another service's secrets. |
| RBAC | Keeper | Applies to OpenAPI, MCP, push operations uniformly. |
| MCP | Keeper | Keeper - MCP server; primary operator interface on par with OpenAPI. |
| OpenAPI | Keeper | gRPC-Gateway or connect-go on top of the same contract; primary operator interface. |
| Postgres | Keeper cluster | See ADR-005. Source of truth. |
| Redis | Keeper cluster | See ADR-006. Heartbeat cache, lease, pub/sub, leader for Reaper. |
| Security | Everything, especially Soul | Principle of least privilege, minimum Soul surface, mTLS mandatory, no PEM/private key in the database. |

## Open questions

Divided into those closed in previous rounds (we don't save them for history - see git log) and current ones.

### Current

1. ~~**Bootstrap of the first operator ("Creator").**~~ **Closed ADR-013 + ADR-014:** entity name - **Archon** (Archon), identifier - **AID** (kebab-case); mechanism - command `keeper init --archon=<aid>`; credential form - JWT (Vault KV signing key, MVP); registry `operators` in Postgres; restart semantics - failure without `--initialize`; HA race - PG advisory lock.
2. **Operator client - CLI form.** Primary interface - OpenAPI and MCP (ADR-004), CLI is acceptable as a thin wrapper. Open: will it be (separate binary / `keeper` subcommand in client mode / only third-party tools on top of the API), and whether it is necessary to supply an official CLI as part of the release.
3. ~~**SSH-2. SSH providers for `keeper.push`.**~~ **Closed [ADR-020 amendment (2026-05-26)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format):** MVP set - 3 providers, all committed and working: `soul-ssh-static` (`4f95ef6`), `soul-ssh-vault` (`3642520`, Vault SSH CA), `soul-ssh-teleport` (`af27678`, `SignReply.proxy_jump` field 4). Solutions for three general mechanics are fixed: credentials-flow - Option B for CA providers (the plugin itself in Vault via `vault_access`; diverges from cloud-Variant A deliberately - `ssh/sign` is an operation, not KV-read); key-ownership - Keeper-ephemeral (the private does not leave Keeper, security-first); params-delivery - env-convention per-plugin (`SOUL_SSH_*_PARAMS`). Open - **dispatcher `proxy_jump` support** (Teleport-via-bastion): the pilot is applicable to hosts with direct SSH accessibility, a separate slice is in progress.
5. **Sub-questions of the module model.** The model itself is fixed. Open inside:
   - ~~exact set of core modules in MVP~~ **closed ADR-015**: 17 Soul-side (`pkg`/`file`/`service`/`user`/`group`/`exec`/`cmd`/`cron`/`mount`/`git`/`archive`/`sysctl`/`url`/`line`/`repo`/`firewall`/`http`) + 3 Keeper-side (`soul.registered`/`cloud.provisioned`/`vault.kv-read`, the last two are ADR-017);
   - ~~where the module registry lives in Keeper - Postgres `bytea` / separate artifact store (S3-compatible) / keeper file system~~ **closed [ADR-065](adr/0065-core-module-installed.md):** no new storage - PG `plugin_sigils` = permissions (authority sha256), keeper FS cache = bytes(git-directory-resolve `plugins.soul_modules[]`), git = origin(ADR-007); delivery to Soul - server-streaming RPC `FetchModule` + core module `core.module.installed`; S3-artifact-store - post-GA extension behind the fetch abstraction;
   - ~~format and location of the module manifest - separate `manifest.yaml` next to the binary vs the first gRPC method `Manifest()`~~ **closed ADR-020(a):** static `manifest.yaml` in the root of the plugin repo and next to the binary; RPC `Manifest()` is not included in MVP;
   - ~~exact stdio-handshake protocol version and format (probably like `hashicorp/go-plugin`)~~ **closed ADR-020(b/c):** JSON on one line with magic prefix field `"soul_stack":"plugin-v1"`; `protocol_version` is duplicated in manifest and handshake; correspondence `protocol_version: N` ↔ `proto/plugin/vN/`. Full spec - [`docs/keeper/plugins.md`](keeper/plugins.md);
   - ~~module versioning policy and compatibility with different versions of `SoulModule` API~~ **closed ADR-020(c):** forward-compat only-add inside `proto/plugin/vN/`, host holds `SupportedProtocolVersions` (MVP `[1]`), hard fail on mismatch;
   - **optimization for later:** mandatory declaration of `required_modules: [haproxy, myapp]` in Destiny - will allow you to transfer only the necessary modules instead of all of them.
6. ~~**Soulprint: schema and extensions.**~~ **Closed ADR-018 (MVP typed schema only).** Fields: `sid`/`hostname`/`os`(family/distro/version/codename/arch/pkg_mgr/init_system)/`kernel`(version/release)/`cpu`(count/model/vendor)/`memory`(total_mb/available_mb/swap_mb)/`network`(primary_ip/fqdn/interfaces[]). The canonical CEL form is `soulprint.self.<path>`. Covens - Keeper-registry-projection, not in Soul-side facts. The user-collectors mechanism (open Q No. 22) - **remains open** (requires decisions on sandbox/permissions/collector format, not just schema).
7. ~~**Compatible version of the protobuf contract.**~~ **Closed ADR-012:** forward-compat only-add inside `proto/keeper/v1/` (never delete fields, do not reuse field numbers; breaking changes - only through a new package `v2/`). Keeper can be upgraded separately from Souls.
8. **Local admin endpoint on Soul.** **Delayed post-MVP.** In MVP admin operations on Soul-host: `SIGHUP` for hot-reload `soul.yml`, local shell access to logs / `journalctl` / `metrics`-listener for observability, centralized rollout config via CI / SSH. Local HTTP/MCP listener on Soul (status / force-resync / dump Soulprint without Keeper / API-driven config mutation) - if really necessary (separate ADR, propose-and-wait for transport: HTTP vs Unix socket vs FromKeeper command).
10. **LB-1. Balancing by SID/Coven labels.** Is L4-LB sufficient (any Keeper will serve any Soul, Coven - only in application logic), or is L7-aware LB / Keeper's own routing-prefix needed. Deferred by user decision.
11. **Leakage of SoulSeed tokens before use.** Additional protections beyond TTL+disposability (binding to IP/CIDR, requirement of cloud-metadata proof, manual approval). Put aside in the ideas box.
12. **Mechanics of checks `last_seen_at`.** Now updated for any message on the stream; explicit keepalive pings are deferred. If this is not enough to accurately determine "alive/not alive," we select an explicit ping mechanism.
13. ~~**List of cloud providers for MVP.**~~ **Closed [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read) amendment (2026-05-25, implemented by 2026-05-26):** first set - **6 official** providers (AWS / GCP / Azure / Yandex Cloud / Proxmox / OpenStack; binaries `soul-cloud-{aws,gcp,azure,yc,proxmox,openstack}`); **vSphere** - community / deferred. AWS - pilot (reference). By 2026-05-26, all 6 are committed and working: AWS (`ec83487`+`72ff14b`), GCP (`6bfd83c`), YC (`a181d49`), Azure (`c7d93ad`), OpenStack (`a03a404`), Proxmox (`af43665`) - see [ADR-017 amendment 2026-05-26 (j)/(k)/(l)](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read).
14. **Bootstrap the soul agent on the new VM.** **The recommended path is cloud-init userdata** ([ADR-017](adr/0017-keeper-side-core.md) amendment (h)): `CreateRequest.userdata` carries a blob that deploys soul on the new VM; per-VM-token-in-userdata is postponed (chicken-egg: SID is assigned after create), in MVP - general batch-blob + onboarding via CSR. Alternatives (`keeper.push` after create / gold image with soul in AMI) remain possible. Open: formalization of path selection for specific scenarios.
15. ~~**Drift detection.**~~ **Closed by [ADR-031](#adr-031-scry--drift-detection-declarative-dry-run-reconcile), then WITHDRAWN (NIM-446, 2026-08-05).** Scry shipped (on-demand `check-drift` + a default-OFF background scan) and was cut whole after it went unused; the user's decision at the NIM-435 acceptance was to remove it everywhere, UI and backend. Drift detection is therefore **not** a capability Soul Stack currently offers, and the open question is open again should a real request return. What the ADR left behind is the machinery, not the feature: `Plan` → pure-read with machine `changed`, only-add `PlanEvent.changed` + `ApplyRequest.dry_run` (Errand uses both), and the informational status `drift` for a legacy upgrade.
16. **Recovery scenarios.** Service can declare a special recovery scenario for typical cases of partial failure (`error_locked`). Format, obligatory nature, naming convention.
17. **Reconcile-loop for cloud-incarnation.** Background process "declared count vs actual VM count" - install immediately or MVP only manual `incarnation.upgrade/scale`.
18. ~~**DSL migrations state_schema.**~~ **Closed ADR-019:** flat (`rename`/`set`/`delete`/`move`) + CEL value expressions + structured `foreach` (MVP). Conditional `if:` - deferred until the first real request (extension without breaking change). Forward-only, no escape module is entered. Full spec - [`docs/migrations.md`](migrations.md).
19. ~~**`state_history` retention.**~~ **Solved (2026-05-25):** **soft-delete / archiving by config, NOT hard-delete.** Store the last **N=50** snapshots on incarnation + **ALWAYS** snapshot for every `state_schema`-version-bump (migrations remain recoverable). "Excess" images are **archived** (archive table or `archived_at` flag - config-knob), not physically erased (forensic > GC). Cleans/archives **Reaper rule**. Implementation - Track 4 ([roadmap.md](roadmap.md)), not yet done.
20. ~~**UI Keeper.**~~ **Resolved (2026-05-25): UI is a separate artifact (SPA), NOT embedded in `keeper`.** Stack - React + TypeScript + Vite + TanStack Query + React Hook Form + Zod + lucide. Design-system - a custom set of CSS tokens/components for the Soul Stack brand/dictionary. TS client + Zod types **generated from [`docs/keeper/openapi.yaml`](keeper/openapi.yaml)** (not written by hand). All UI vocabulary is strictly according to [naming-rules.md](naming-rules.md) (Keeper / Souls / Coven / Soulprint / Destiny / Archon). Start of development - when the API reaches almost complete readiness. Plan - [roadmap.md](roadmap.md).
21. **`host_schema` in `service.yml` (a collection of ideas).** Declaration of the expected host topology for Incarnation: list of roles (`required_roles`) and restrictions on the number of hosts in each role (`role_constraints.<role>.count: { eq | gte | lte }`), possibly extendable to Soulprint filters (minimum CPU/RAM/OS). What would result: an early failure when creating an Incarnation with the wrong number of roles (before cloud-create), checking the declared topology (Choir Voices, [ADR-044](adr/0044-choir.md)) against the declared scheme before running, hints in the UI/MCP. Postponed until real scenarios show which checks pay off - without them the field will turn into decorative. NB: step targeting - `on:`/`where:` ([`docs/scenario/orchestration.md §4`](scenario/orchestration.md), [§4.1](scenario/orchestration.md)), role **not** Coven ([ADR-008](adr/0008-coven-stable-tags.md)); static check of `on:`/`where:` literals - backlog `soul-lint` ([soul-lint.md → B2](soul-lint.md#b2-statistical-check-of-where--and-on-literals)), not part of this clause.
22. **`soulprint.collectors` in `soul.yml` (a collection of ideas).** Explicit declaration of a set of Soulprint collectors in the agent config: built-in groups (`core` - os/kernel/network/memory/cpu/hostname, `systemd` - units) and a directory of custom detectors (`custom_dir: /etc/soul/soulprint.d/`). Now only `refresh_interval` remains in `soul.yml`; the actual set of facts is internal default. Related to open question 6 (general layout of Soulprint and extensions), but deals specifically with the form of the config on the host: is a toggle switch needed at all, or are collectors always enabled and custom detectors picked up from a fixed path by convention.
23. ~~**Event-driven circuit.**~~ **Closed [ADR-030](#adr-030-vigil--oracle---event-driven-monitoring-beacons--reactor):** beacons-circuit introduced by entities **Vigil** (Soul-side check, read-only) → **Portent** (event, only-add `EventStream`-oneof) → **Oracle** (Keeper reactor router) → **Decree** (reactor rule, default-deny, action = scenario-only via work-queue [ADR-027](#adr-027-execution-model-apply---work-queue--claim-acolyte-pool-ward-claim)). The uncommitted clause `SoulBeacon` has been replaced with final names; community checks - via plugin-kind `soul_beacon` (S5). A long-running on-host/Keeper engine is **not** introduced by this ADR - it remains deferred.
24. ~~**Per-task granularity `serial:`.**~~ **Closed [ADR-056](#adr-056-staged-render---running-the-scenario-as-n-ordered-passages) (staged-render).** MVP was per-RUN min-width (wave width one per run = minimum positive `serial:`-width among tasks, [`docs/scenario/orchestration.md §2.2.1`](scenario/orchestration.md)), because per-task dispatch (multiple `ApplyRequest` / `apply_runs` lines per host) is not implemented. ADR-056 introduces **Passage** - run as N ordered stages (render→dispatch→barrier→register) along the task axis: per-task dispatch is now implemented as N `ApplyRequest` per host by Passage (amend [ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) dispatch-model + only-add `passage` + PG-PK per-passage). The main driver of the ADR-056 is the implementation of probe→where (`register` in `where:`/`apply:input` of the following tasks), which the canon orchestration.md §4/§5 already promises; per-task `serial:` is a consequence of the same staged model.
25. **Per-host `RenderedTask` dispatch (host-variable flow-control predicates on multi-host).** **render_context.self - closed by Option A** (per-host render context, `RenderContextBySID`): per-host `.self` now resolves for `templates/*.tmpl` and `core.file.rendered` in both branches dispatcher (inline tasks and Acolyte pool) - each host receives its own `soulprint.self.*` in the render file. **Remains open** (full Option B - per-host dispatch of the entire `RenderedTask`): host-variable `params:`, flow-control predicates (`when:`/`changed_when:`/`failed_when:`) and `loop:` with a link to `soulprint.self` on the multi-host target. Pilot keeps these cases under fail-closed guard: they are only allowed with a single-host target; on multi-host, the render crashes with an obvious error ([`docs/templating.md §4`](templating.md)) - guard catches an unsupported case with an obvious error, not silent-wrong. **Full Option B deferred** (decision 2026-06-29: Option A is sufficient). Will require a separate ADR (removing host invariance `params:` + changing the Keeper↔Soul dispatch boundary). **Resume condition** is the first service that needs `${ soulprint.self.* }` in module-`params:` OR `when: soulprint.self.*` on a multi-host target WITHOUT the ability to put self-variability in `.tmpl`. Until then, Option A (per-host `.self` for render files + fail-closed guards for the rest) is sufficient.
26. **Audit-log scaling for bulk runs (100k+ hosts).** Currently `audit_log` is a PG table with INSERT per-event. One Tide run on a 100k VM with wave=1000 gives ~200k records (`tide.started` ×1 + `surge_started`/`surge_completed` ×100 each + per-apply_run `apply.dispatched`/`apply.completed` ×100k×2). Bottlenecks: INSERT-throughput PG (~5-10k INSERT/s), table size (~50MB/run → 250GB/year at 100 runs/week), full-scan reads for UI without partitioning. **Decision postponed to the backlog of future releases** (user decision 2026-05-27). Possible options for consideration: (A) semantic filter "no per-SID events if there is a Tide-aggregate" (-99% volume, source of truth - `apply_runs` table); (B) partitioning by month + retention 90/365 days; (C) hot/cold split (PG for UI, S3/parquet for long-term); (D) batched INSERT; (E) async sink via Redis Stream → separate writer. Up to 10k VM - the current audit works without optimization; During pilot runs, evaluate the actual load and choose an option.
27. **UI i18n - runtime-discoverable list of languages (`manifest.json`).** UI (companion-repo `soul-stack-web`) uses hybrid lazy-load: default language `ru` bundled inline, the rest (`en`+) - static `public/locales/<lang>/<ns>.json`, fetched via `i18next-http-backend` (not Keeper - these are static SPA assets, they go with the static front host). The list of available languages is now **hardcoded** in `SUPPORTED_LANGS` (TS-literal) - gives type safety `type Lang` + build-time completeness validation via ns-key-sync test. **User decision 2026-05-27: leave hardcoded, change with releases.** Idea for the future (piggy bank): add a list of languages to `/locales/manifest.json`, read by the tag in runtime → adding a language without rebuilding JS, even for the switch (the translation command drops the folder + line in manifest). Trade-off: +1 network request at start, loss of TS literal type `Lang`, no build-time guarantee of completeness of translations (broken language is caught only by the user in runtime). Entering in real translation-workflow / 3+ community languages is a non-breaking additive.

Each of these items will either become a new ADR here, or (if they grow) into their own document in [docs/](.).
