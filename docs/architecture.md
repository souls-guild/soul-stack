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
- [Essence: assembly pipeline](#essence-assembly-pipeline)
- [Incarnation — runtime service instance](#incarnation--runtime-service-instance)
- [Targeting and host communication](#targeting-and-host-communication)
- [Versioning and migration state_schema](#versioning-and-state_schema-migrations)
- [Cloud integration via `keeper.cloud`](#cloud-integration-via-keepercloud)
- [Reaper](#reaper)
- [Delivery of SoulSeed token to host](#delivery-of-soulseed-token-to-the-host)
- [End-to-end installation script](#end-to-end-installation-script)
- [Top Level Data Flow](#top-level-data-flow)
- [End-to-End Requirements](#end-to-end-requirements-and-where-they-land)
- [Open questions](#open-questions)

Accompanying documents:
- [docs/README.md](README.md) - index of all documentation.
- [docs/naming-rules.md](naming-rules.md) - dictionary of names.
- [docs/requirements.md](requirements.md) - product requirements.
- [../CLAUDE.md](../CLAUDE.md) - guide for AI agents and a summary of solutions.

## System purpose

Soul Stack is a configuration management system, ideologically close to SaltStack, but with its own dictionary of names ([docs/naming-rules.md](naming-rules.md)) and its own architecture. Applicable for:

- declarative description of the desired state of the hosts (**Destiny**),
- collecting facts about hosts (**Soulprint**),
- storing parameters and secrets (**Essence**),
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

Moved to [`docs/adr/0007-versioning-git-ref.md`](adr/0007-versioning-git-ref.md). Artifact version (Service / Destiny / Module / Plugin) = git ref, there is no `version:` field in manifests; dependencies - strictly `ref:` (tag or branch), without semver-range. Exceptions (not "artifact versions"): `state_schema_version`, `protocol_version`, `service_version`, Go modules inside a repo (shared semver tag).

### [ADR-008. Coven - stable logical tags only](adr/0008-coven-stable-tags.md)

Moved to [`docs/adr/0008-coven-stable-tags.md`](adr/0008-coven-stable-tags.md). Coven - only stable logical tags (cluster / project / environment / data center); `incarnation.name` remains the root Coven label; convention `{incarnation.name}-{role}` deleted; role NOT Coven (declared in spec / actual via probe); essence role-agnostic. Amendments: environment = special case Coven (first-class `Environment` rejected, per-Coven RBAC-scope implemented); cross-incarnation was lifted at the Voyage layer; Choir ≠ coven.

### [ADR-009. Scenario - a complete DSL of destiny tasks; border with destiny - recommendation](adr/0009-scenario-dsl.md)

Moved to [`docs/adr/0009-scenario-dsl.md`](adr/0009-scenario-dsl.md). The "scenario without `module:`" invariant has been removed completely: scenario receives the entire DSL core of destiny tasks (the source of truth is `docs/destiny/tasks.md`) + orchestration delta (`on:`/`where:`/`apply:`/`state_changes`); destiny/scenario boundary - recommendation (three "yes": reuse / molecule-idempotency / isolability); barrier/state-commit - invariant (cross-host final-barrier, `error_locked` for finally-failed). Amendments: Tide (invocation-time scope chunking, absorbed by Voyage); §7 per-incarnation state-commit in Voyage; Choir - additive `choirs[]`.

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

Moved to [`docs/adr/0015-core-modules-mvp.md`](adr/0015-core-modules-mvp.md). 17 Soul-side core modules (`core.pkg`/`core.file`/`core.service`/`core.user`/`core.group`/`core.exec`/`core.cmd`/`core.cron`/`core.mount`/`core.git`/`core.archive`/`core.sysctl`/`core.url`/`core.line`/`core.repo`/`core.firewall`/`core.http`) + 3 Keeper-side (`core.soul.registered`/`core.cloud.provisioned`/`core.vault.kv-read`); `core.template`/`core.copy` are NOT deliberately highlighted; `core.line`/`core.repo`/`core.firewall`/`core.http` accepted post-facto (in-place/read-probe MVP).

### [ADR-016. Parity strategy with SaltStack/Ansible and Soul Stack license](adr/0016-parity-license.md)

Moved to [`docs/adr/0016-parity-license.md`](adr/0016-parity-license.md). License - Apache 2.0 (open core / freemium: enterprise features in separate repo). The parity strategy is a hybrid without a wrapper: core MVP is our Go rewrite, exotic is community plugins via SDK; wrapper Ansible is prohibited (GPLv3 + Python-runtime). Amendments: Plugin SDK Phase 2 (10 official `soul-mod-*`, namespace `official`, template mechanism).

### [ADR-017. Keeper-side core modules expanded: `core.cloud.provisioned`, `core.vault.kv-read`](adr/0017-keeper-side-core.md)

Moved to [`docs/adr/0017-keeper-side-core.md`](adr/0017-keeper-side-core.md). Two keeper-side core modules (`on: keeper`): `core.cloud.provisioned` (`created`/`destroyed` via `CloudDriver` plugin, cascade with destroy) replaces the "destiny `cloud-provision`" pattern; `core.vault.kv-read` (`read`) - explicit audit-accurate reading of Vault KV during rendering. Amendments: cloud credentials-flow (Variant A) + 6 implemented providers + cloud-init bootstrap.

### [ADR-018. Soulprint typed MVP scheme](adr/0018-soulprint-typed.md)

Moved to [`docs/adr/0018-soulprint-typed.md`](adr/0018-soulprint-typed.md). Typed `SoulprintFacts` (sub-messages `OsFacts`/`KernelFacts`/`CpuFacts`/`MemoryFacts`/`NetworkFacts`) instead of `google.protobuf.Struct`-stub (deprecated, wire-compat); `os.pkg_mgr`/`os.init_system` collected by Soul Agent; canonical CEL form `soulprint.self.<path>`; `covens`/`choirs` — Keeper projection, not a fact. Amendments: pkg_mgr/init_system hybrid, `choirs`-fact, `typed_facts` byte-passthrough on REST.

### [ADR-019. State_schema migration DSL](adr/0019-state-migration-dsl.md)

Moved to [`docs/adr/0019-state-migration-dsl.md`](adr/0019-state-migration-dsl.md). Grammar flat (`rename`/`set`/`delete`/`move`) + CEL in `set.value` through `${ … }` + structural `foreach`; migration-CEL sandbox(ban `vault`/`now`/`register`/`soulprint`/`essence`/`input`); forward-only; atomic PG transaction per chain; tests `migrations/<NNN>_to_<MMM>/tests/`.

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

Moved to [`docs/adr/0026-sigil.md`](adr/0026-sigil.md). Keeper-signed allow-list of plugins (registry `plugin_sigils`): Archon explicitly allows `(namespace, name, ref) → sha256`, Keeper signs the block with attached manifest, host verifies digest+signature BEFORE exec (replaces TOFU first-load). Git-verified `ref` (go-git F-fetch), multi-anchor signature key rotation (`sigil_signing_keys`), replace semantics snapshot/anchors.

### [ADR-027. Execution model apply - work-queue + claim (Acolyte-pool, Ward-claim)](adr/0027-apply-work-queue.md)

Moved to [`docs/adr/0027-apply-work-queue.md`](adr/0027-apply-work-queue.md). Execution of apply on work-queue + claim: Acolyte-pool, Ward-claim (`FOR UPDATE SKIP LOCKED`), Summons (`apply:summons`), just-in-time render for claim, Reaper recovery scan (`reclaim_apply_runs`), two-way attempt-fencing, single-winner state-commit; Phase 0/1/2 + GATE-1 (deliver-once, lifecycle `planned→claimed→dispatched→terminal`) implemented, Phase 3 distributed serial postponed; amendments Conclave/Watchman/refuse-guard, Voyage back-link, Tide-spawn.

### [ADR-028. RBAC-storage → Postgres](adr/0028-rbac-storage.md)

Moved to [`docs/adr/0028-rbac-storage.md`](adr/0028-rbac-storage.md). Transfer of RBAC-storage (roles, permissions, membership) from `keeper.yml` to Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`), fix BUG-1 (init writes membership to the database, not to the JWT-claim); enforcer on an in-memory snapshot from the database + Redis invalidation; includes Amendment ADR-049 (Synod).

### [ADR-029. Service registry → Postgres](adr/0029-service-registry.md)

Moved to [`docs/adr/0029-service-registry.md`](adr/0029-service-registry.md). The Service registry (`keeper.yml → services[]`) and well-known scalars (`default_destiny_source`) are transferred to Postgres (`service_registry` + key-value `keeper_settings`), runtime - snapshot `serviceregistry.Holder` + `service:invalidate`; config hard-cut three keys; closes the balance [ADR-028(h)](adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres).

### [ADR-030. Vigil + Oracle - event-driven monitoring (beacons + reactor)](adr/0030-vigil-oracle.md)

Moved to [`docs/adr/0030-vigil-oracle.md`](adr/0030-vigil-oracle.md). Event-driven circuit (Salt beacons+reactor-analogue): Vigil (Soul-side read-only check) → Portent (edge-triggered event, only-add in EventStream) → Oracle (Keeper-side reactor-router) → Decree (default-deny rule, action=named-scenario-only). Mandatory invariants loop-prevention (cooldown+circuit-breaker) and security. Amendment S5: typed PortentPayload, `soul_beacon` plugin-kind, inotify-beacon; feature-complete.

### [ADR-031. Scry — drift-detection (declarative dry-run reconcile)](adr/0031-scry-drift.md)

Moved to [`docs/adr/0031-scry-drift.md`](adr/0031-scry-drift.md). Read-only declarative drift detection subsystem: `Plan` no-op→pure-read in all core modules, only-add `PlanEvent.changed` / `ApplyRequest.dry_run`, information status `drift` (not blocking), `DriftReport`, default-deny for community modules without read-safe. On-demand pilot (`check-drift`) → background scan. Amendments: Slice B/C implemented, Plan circulation 14 modules, `converge` → operational scenario-kind (2026-06-10).

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

Moved to [`docs/adr/0043-voyage.md`](adr/0043-voyage.md). Voyage is a single top-level entity of a batch run (batch unit - Leg), absorbing Tide ([ADR-040](adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) + ErrandRun ([ADR-041](adr/0041-errandrun.md)) + classic scenario-run: discriminator `kind` (`scenario` - batch N incarnations, per-incarnation state-commit B1 | `command` - batch N hosts, state is not touched), tables `voyages`/`voyage_targets`, target selection from RBAC scope (not invocation-override), RBAC-by-`kind`, audit `scenario_run.*`/`command_run.*`, failover claim+lease (`reclaim_voyages` default-ON); includes amendments two `batch_mode` (`barrier`/`window`) + Salt-level batch strategies + string `batch`/`max_failures` + `POST /v1/voyages/preview`.

### [ADR-044. Choir - named topology of hosts within the incarnation](adr/0044-choir.md)

Moved to [`docs/adr/0044-choir.md`](adr/0044-choir.md). Choir - first-class named group of hosts within one incarnation (topological "party"), Voice - SID membership in Choir (triple `(incarnation_name, choir_name, sid)`): three DIFFERENT layers (membership/coven/Choir are not duplicated), Choir absorbs `spec.hosts[].role` (`voice.role` precedence), separate PG tables `incarnation_choirs`/`incarnation_choir_voices` (NOT `incarnation.state`), E1 targeting via `where:` + accessors `soulprint.*.choirs`, keeper-side core module `core.choir`; implemented (S-T2…S-T6 part(1a)). Amendments: precedence-role + multi-Choir-conflict; scenario-driven layout of hosts by batch + NULL-vs-default semantics (Choir not set → `NULL` in the database / empty `choirs[]`, NOT default group - empty state is more honest than default; per-shard layout in scenario deferred to mongo, redis cluster lives in one coven `incarnation.name`).

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
   │  soul-lint   │  offline validation of Destiny + Essence
   │  (CI / dev)  │  on the developer side, without Keeper
   └──────────────┘
```

### Roles of binaries

- **`keeper`.** Central server. Stores the Souls registry (Postgres), RBAC policies, validates and renders Destiny, issues Souls commands, aggregates Soulprint and run results, sets gRPC + OpenAPI + MCP. Contains module `keeper.push` (SSH delivery). Integrates with Vault for Essence (secrets) and for CA (issuing SoulSeed certificates).
- **`soul`.** Agent daemon on the managed host; runs as a service and runs continuously. Raises gRPC bidi stream to Keeper, executes received commands, collects Soulprint, and sends results. No Keeper server code, no outgoing traffic except to your Keeper and to explicitly allowed resources. There is no local admin endpoint in MVP ([open Q No. 8](#current) - post-MVP); admin operations on Soul-host in MVP - SIGHUP for hot-reload `soul.yml` + local shell access to logs/metrics.
- **`soul-lint`.** Offline linter for Destiny and Essence. Parses, renders, validates according to the schema, runs static analysis (non-existent modules, dependency cycles, typos in Soulprint targets). Runs locally and in CI, does not require Keeper or a network.

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
- **Same `soul` binary.** Same artifact as the pull daemon, run one-time as `soul apply` (stdin = rendered `ApplyRequest` as protojson - `apply_id` + `RenderedTask[]` after Keeper-side render phases, ADR-012(d); stdout = NDJSON stream `TaskEvent` + final `RunResult` as protojson; exit 0 for `RunResult.status==success`, 1 otherwise). Raw Destiny/Essence does not reach the push host - Keeper renders on its own, Soul does not resolve Vault.
- **SHA-256 cache on the host.** The binary and modules are cached in `/var/lib/soul-stack/{bin,modules}/`; a repeated run does not download anything.
- **SSH authentication - pluggable provider.** Contract `SshProvider` (Vault SSH CA / static key / Teleport - all fit under it), a specific set of required implementations - [open Q SSH-2](#current).

Full analysis (model, push↔agent migration, SSH authentication, operator interface `POST /v1/push/apply`, running algorithm, layout `/var/lib/soul-stack/`, key properties) - in [`docs/keeper/push.md`](keeper/push.md). The normative specification of the HTTP façade and request/response schemas is in [`docs/keeper/operator-api.md`](keeper/operator-api.md). Host layout and cache are in [`docs/soul/modules.md`](soul/modules.md). The `SshProvider` contract and plugin directory are in [`docs/keeper/plugins.md`](keeper/plugins.md).

## Module model

This section applies to both **pull** and **push** transports. This is a single model: the `soul` binary applies Destiny steps, the step execution models are the same regardless of how the binary ended up on the host.

### Structure

- **Core modules** - statically built into the `soul` binary. Cover the vast majority of Destiny: the exact list is fixed [ADR-015](#adr-015-core-mvp-modules-exact-list) – 17 Soul-side (`pkg`/`file`/`service`/`user`/`group`/`exec`/`cmd`/`cron`/`mount`/`git`/`archive`/`sysctl`/`url`/`line`/`repo`/`firewall`/`http`) + 3 Keeper-sides (`soul.registered`/`cloud.provisioned`/`vault.kv-read`, the last two are [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read)). They work always, everywhere, and do not require additional delivery. By addressing, all built-in modules live in namespace `core`. Files from templates are rendered by `core.file.rendered` (see [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files)) - a separate module `core.template` is NOT allocated.
- **Custom modules** - separate executable files `soul-mod-<name>`, located in `/var/lib/soul-stack/modules/`. The `soul` binary runs them as a sub-process with the stdio protocol (see below). By addressing they live in the namespace of their collection (`wb`, `community`, ...).

> **Soul-side vs Keeper-side core modules.** The vast majority of core modules (`pkg`, `file`, `service`, `user`, `exec`, `template`, …) are **Soul-side**: executed on the host `soul`-binary. Some of the core modules are **Keeper-side**: they operate on the keeper's registries (Postgres souls+coven, Redis cache, logs) and are executed on the keeper itself. The first Keeper-side core is `core.soul.registered` (SID binding to coven tags of the souls registry; full specification is [`docs/keeper/modules.md`](keeper/modules.md)). Dispatcher - scenario key `on:` ([`docs/scenario/orchestration.md §3`](scenario/orchestration.md)): for Soul-side core `on:` is omitted or contains coven tags; for Keeper-side core `on: keeper`. The addressing (`<namespace>.<module>.<state>`) and the SoulModule contract are the same for both parties.

When applying Destiny step `soul`:
1. parses the module name according to the scheme `<namespace>.<module>.<state>` (see "Addressing modules");
2. for a built-in core module calls the implementation of `<module>.<state>` directly, in the process;
3. otherwise looks for the file `soul-mod-<module>` in the collection modules directory; no file - validation error (`soul-lint` catches this before running);
4. launches a sub-process, transfers state, parameters and Essence via gRPC-stdio, reads events as a stream (see "Modules Protocol").

### Module addressing

The module is addressed in three levels, through a dot:

```
<namespace>.<module>.<state>
```

| Level | Meaning | Examples |
|---|---|---|
| **namespace** | A collection is a unit of distribution and trust (see [module-collections.md](module-collections.md)). Author/publisher prefix. | `core` (built into `soul`), `wb`, `community` |
| **module** | The control object is "what the module is about." Equivalent to Salt's state module without a function (`pkg`, `service`). | `pkg`, `file`, `service`, `user`, `exec`, `template`, `haproxy` |
| **state** | The desired state is "how the object should be." Declarative noun (not an imperative verb). | `installed`, `absent`, `latest`, `present`, `running`, `stopped`, `restarted`, `enabled` |

Addressing **declarative**: the third level is a *desired state*, not an *action*. `core.pkg.installed` reads "package installed" - not "install package". This fits better with the philosophy of destiny ("what should be"), gives a natural language to script writers and coincides with what Salt does.

**Each state is a separate input schema of the module.** `core.pkg.installed` accepts `name`, `version`; `core.pkg.absent` - only `name`; the parameters do not intersect. The module manifest declares its full list of states and the parameter scheme for each, see "Module Manifest" below. Linter catches:
- unknown module state → error;
- wrong set `params:` for a specific state → error;
- divergent parameter types → error.

**Non-stateful modules.** Modules that do not have a natural "state" (`exec`, `cmd`) use the third-level **verb form**: `core.exec.run`, `core.cmd.shell`. This is a deliberate exception to the declarative rule: for a statement that has no state, it is useless to invent "this is the command I want in a neglected state." The module manifest marks the state form or verb form, the linter checks this.

**The third level is required.** The entry `core.pkg` without `state` is a validation error, no defaults. Explicit is better than implicit: the operator does not have to "guess" what the default is.

**`required_modules:` in destiny.yml is a declaration of only custom modules** in a two-level form (`<namespace>.<module>`): "this destiny requires that the family of custom modules `wb.haproxy`, `wb.myapp`, ..." be available on the host. All state forms inside these modules are available automatically. Specific state instances are called by a 3-level form in `tasks/main.yml` (see ["Task Structure in `tasks:`"](#destiny-task-structure) below).

**Core modules are not listed in `required_modules:`** - they are statically built into the `soul` binary and are always available. If destiny uses only `core.*`, the `required_modules:` block is omitted entirely.

**Example (`tasks/main.yml` destiny with custom modules - top-level task list, without wrapper):**

```yaml
# destiny-<name>/destiny.yml declares required_modules:
#   required_modules: [wb.haproxy, wb.myapp]

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
  module: wb.haproxy.reloaded               # custom: declared in required_modules:
  params: { config_path: /etc/haproxy/haproxy.cfg }
```

### [ADR-045. Param-DSL modules - typed input fields for the UI form Run Command](adr/0045-param-dsl.md)

Moved to [`docs/adr/0045-param-dsl.md`](adr/0045-param-dsl.md). Brings modular input-DSL (`plugin.InputParamDef` / `shared/coremanifest`) closer to scenario/destiny `input:` - `enum` / `format: sid` + `source:` / `pattern` / `items` under `list`/`map` - to UI Run Command built a typed form for a module (core + plugin), without introducing a third DSL.

### Destiny task structure

The contents of destiny live in **`destiny-<name>/tasks/main.yml`** as a top-level YAML task list (without the `tasks:` / `steps:` wrapper), and not in `destiny.yml` itself. Root `destiny.yml` - manifest only (`name`, `description`, `input`, opt. `required_modules`); `tasks/main.yml` - entry point, with the ability to connect `include: <file>.yml` neighbors inside the same folder `tasks/`.

One list element is a call to one module with parameters and optional binding. Task fields (`name`, `module`, `params`, `when`, `register`, `output`, `no_log`, `include`), task naming convention (capital letter, imperative, English) and rules `include:` - fixed in **[`docs/destiny/tasks.md`](destiny/tasks.md)**. The architectural section here does not duplicate the field table, so that there is no drift between two sources.

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

  The plugin manifest is static `manifest.yaml`, normatively described in [ADR-020(a)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format); RPC `Manifest()` is not included in MVP.

- gRPC-stream provides native progress reporting for long operations (`PlanEvent`, `ApplyEvent`).
- The module ends with a graceful shutdown signal under the same contract.

### Module manifest

Each module declares itself in the **static `manifest.yaml`** in the root of the plugin repo and next to the binary in the host cache ([ADR-020(a)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format)). The format is the same for three kinds (`soul_module` / `cloud_driver` / `ssh_provider`) with `kind:` discriminator; regulatory source is [ADR-020(e)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format) and [`docs/keeper/plugins.md`](keeper/plugins.md). Example for `SoulModule`:

```yaml
kind: soul_module                   # discriminator (ADR-020(e))
protocol_version: 1                 # plugin protocol version (compat flag, not module version - see ADR-007, ADR-020(c))
namespace: wb                       # collection
name: haproxy                       # module name inside the collection

required_capabilities: [run_as_root]  # closed enum (ADR-020(f))
side_effects:                          # strict contract (ADR-020(g))
  - { service: haproxy }
  - { file: /etc/haproxy/haproxy.cfg }

spec:                                  # kind-specific block for soul_module
  # List of supported states (or verb forms for non-stateful modules).
  # Each state has its own input schema (DSL - see section
  # "Destiny: Input Contract and Validation").
  states:
    running:
      input:
        name:    { type: string, required: true }
        enabled: { type: boolean, default: true }
      description: HAProxy is running and enabled in systemd.
    stopped:
      input:
        name: { type: string, required: true }
    restarted:
      input:
        name: { type: string, required: true }
```

The manifest is used by `soul-lint` for **local** validation of Destiny: we catch unknown modules, unknown module state, incorrect parameters for a specific state, capabilities-mismatch with host-policy - without launching the module itself ([ADR-009](adr/0009-scenario-dsl.md), [ADR-020](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format)).

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

Moved to [`docs/adr/0052-herald-notifications.md`](adr/0052-herald-notifications.md). **Herald** - notification delivery channel (PG registry `heralds`: `type`-enum `webhook` in MVP, `config` JSONB, `secret_ref` vault-ref), **Tiding** - subscription rule (PG registry `tidings`: `event_types[]` with area-glob `scenario_run.*`, filters `only_failures`/`only_changes`, selectors `incarnation`/`cadence`, FK on Herald). Scope MVP - Run Events ONLY (`scenario_run.*`/`command_run.*`/`voyage.*`/`incarnation.drift_checked`/`cadence.*`); Host beacon events via [Oracle](adr/0030-vigil-oracle.md) - rejected (separate entry). Mechanics - tap (multi-writer decorator on top of `audit.Writer`, point [ADR-022(f)](adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) → notification-dispatcher → at-least-once webhook-delivery of claim-queue worker (parity [VoyageWorker](adr/0043-voyage.md)), retry+backoff, attempt statuses in Redis (hot→Redis), terminals `herald.delivered`/`herald.failed` in audit. Security invariants: https-only + deny private IPs (opt-out `allow_private`, pattern `core.url`) + deny redirect (`util.CheckRedirect`) + timeout; `secret_ref` vault-ref only; payload without resolved secrets + `MaskSecrets`. Contracts additive: OpenAPI `/v1/heralds`+`/v1/tidings` (greenfield full-strict, [ADR-051 S6](adr/0051-operator-api-codegen.md)), RBAC `herald.*`/`tiding.*`, MCP `keeper.herald.*`/`keeper.tiding.*`, UI tab "Notifications".

### [ADR-053. Infrastructure dependency tiers](adr/0053-dependency-tiers.md)

Moved to [`docs/adr/0053-dependency-tiers.md`](adr/0053-dependency-tiers.md). Classification ADR: the mandatory contour of the Keeper cluster is **three components PostgreSQL + Redis + Vault**, all three **fail-fast** at the start. Vault hard-required at three points: vault client at start (`setupVault` → `NewClient` → `Ping`), JWT signing-key (auth operators, [ADR-014](adr/0014-operator-identity.md)), souls-PKI (release/rotation of SoulSeed mTLS via Vault PKI). OPTIONAL-with-degradation (the feature is clearly disabled, Keeper does not crash): Sigil signing-key (fail-closed), Augur (default-deny), Herald `secret_ref` (without signature), push host-CA (push disabled), metrics basic-auth, OTel-export. Rule for NEW features: new mandatory dependencies - only through the user's decision; optional ones are required to degrade clearly. Rejected: no-Vault mode (file/env auth-key + built-in CA - breaks the security premise "secrets do not materialize on the Keeper cluster disk"; CA-private on disk/in PG, in multi-keeper distribution of private among nodes; rejected by the user) and SecretProvider abstraction (premature). Operations note: mandatory Vault ≠ heavy cluster - single-binary with file-storage comparable to Redis (recipe in [infra.md](adr/../operations/infra.md)).

### [ADR-054. Operator API - pivot to code-first (Go-types → OpenAPI) via huma v2](adr/0054-openapi-code-first.md)

Moved to [`docs/adr/0054-openapi-code-first.md`](adr/0054-openapi-code-first.md). Replaces [ADR-051](#adr-051-operator-api-codegen-openapi--go-types-oapi-codegen-types-only--strict): Operator API form source inverted to **Go-types → OpenAPI** (code-first, huma v2 + humachi), derivative spec. **FULL-TYPED** handlers (typed `Body`/output + extracted domain `XTyped`); validation `required`/`enum`/`unknown→400` - native huma from struct tags; security saved (manual mount to chi group with RBAC/Audit linkage, global problem+json-override, per-domain audit-guard; middleware-audit domains - option B `UseMiddleware`). Tiers pattern: full-typed write, PATCH-presence via `Optional[T]`, read-with-typed-query (`400/422`-contract saved). **IMPLEMENTATION COMPLETED 2026-06-13 (HEAD `fde65bf`)**: all ~19 domains handler-native, served-spec = runtime huma-dump (`HumaFullSpecYAML`, aggregator `huma_full_spec.go`), committed `docs/keeper/openapi.yaml` - derived (`make gen-openapi` / drift-guard `make check-openapi`), native enum directory `huma_enums.go`; oapi-codegen-framework and source-manuscript demolished. **Amendment 2026-06-15:** the visual OpenAPI viewer `GET /docs` (Stoplight Elements, go:embed-assets, mechanism A is a public shell framework, the spec for JWT) was raised above the derived spec, and `GET /openapi.yaml` was replaced by security public → for JWT (no anonymous intelligence of the API surface; UI-vendor/`soulctl` take the committed file). Meta routes - [operator-api.md → Health / Meta / Docs](keeper/operator-api.md#health--meta--docs).

### [ADR-055. Embed UI bundle - optional single-binary keeper with UI on `/ui`](adr/0055-embed-ui-bundle.md)

Moved to [`docs/adr/0055-embed-ui-bundle.md`](adr/0055-embed-ui-bundle.md). **Amends [ADR-035](#adr-035-distribution-split--core-apicli-vs-web-ui):** Enables deferred embed-compat-shim as an optional **default-ON** embed UI for beta (single-binary onboarding), NOT a split distribution rollout. Keeper, through `go:embed`, carries the vendored UI build snapshot (`soul-stack-web`) to `keeper/internal/webui/assets/` (folder `assets/`, NOT `dist/` - the gitignore rule `dist/` would silently eat the bundle) and distributes it on the route `/ui` + `/ui/*` with SPA-fallback to `index.html`. Static **public** (parity `/docs`; `/v1` remains with JWT+RBAC+default-deny), toggle `web_ui_enabled` (`*bool`, `nil`→default-ON, explicit `false`→opt-out), shares listener `:8080` (NO new ports/systemd), binary growth ~+2–5 MB. Source-of-truth UI = companion-repo `soul-stack-web`; in core - only the collected artifact, sync `scripts/sync-webui.sh` + drift-guard `make check-webui` (skip without companion, tracing plugin-template). Clarification of the ADR-035 "no HTML/CSS/TS" invariant: **sources** UI is still prohibited, **assembled static artifact** is allowed (as RapiDoc bundle `/docs`). The use case for go:embed-statics is [ADR-054](#adr-054-operator-api---pivot-to-code-first-go-types--openapi-via-huma-v2).

### [ADR-056. Staged-render - running the script as N ordered Passages](adr/0056-staged-render-passage.md)

Moved to [`docs/adr/0056-staged-render-passage.md`](adr/0056-staged-render-passage.md). The script run is executed as **N ordered Passages** (run phase = render → dispatch → barrier → register collection). Implements the promise of the canon [orchestration.md §4/§5](scenario/orchestration.md) probe→where: the task reading `register.X` (in `where:`/`apply: input:`/`params:`/`vars:`), is stratified in Passage **after** the probe issuing `X` (topological N-stage); render of the next Passage substitutes the per-host register of the previous ones. Closes doc-drift "keeper renders one up-front pass BEFORE probe → `where:` sees empty register." **`incarnation.state` commits ONCE after the last Passage** - barrier/state-commit-invariant §7 is not split; Passage is a task axis, not a commit axis. Reuse: `loadRegisterByHost`/`apply_task_register`/`SelectTaskRegistersByApplyID` (post-barrier register) as Passage-loop; `evalWhere`/`renderParams` already accept `in.Register`. Contract: proto only-add `passage` to `ApplyRequest`/`TaskEvent`/`RunResult`; PG-PK per-passage (option is fixed in S1); N `RunResult` to `(apply_id, sid, passage)`; old Soul under staged script → explicit-reject (`render_failed`, no hangup). **Amends [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation)** (per-task `serial:`/§8 → per-task dispatch via Passage), **[ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)** (dispatch "one ApplyRequest per host" → "N per host via Passage"), **[ADR-027](#adr-027-execution-model-apply---work-queue--claim-acolyte-pool-ward-claim)** (reclaim/Ward-claim granularity → per-passage). Closes [open Q No. 24](#open-questions) (per-task granularity `serial:`). Slice map S0–S5.

### [ADR-057. `state_changes` - ordered list of CRUD verbs](adr/0057-state-changes-crud-verbs.md)

Moved to [`docs/adr/0057-state-changes-crud-verbs.md`](adr/0057-state-changes-crud-verbs.md). The script's `state_changes` becomes an **ordered list of operations** (YAML list, not map). Each element is one **CRUD verb** (singular): **`set`** (rewriting the entire field, replaces the previous `sets`-map), **`add`** (add an element to the collection: map - `key:`+`value:`, list - `value:`+opt. `match:`; `on_conflict: skip|replace|error`, default `skip` = idempotent "add if not"), **`modify`** (`match:` + `patch:` - patch ALL matching, all-by-default), **`remove`** (`match:` - remove ALL matching), **`foreach`/`as`/`do`** (bulk fan-out, form literally from migration-DSL [ADR-019](#adr-019-state_schema-migration-dsl)). **Multiplicity is expressed by a `match` predicate** (CEL over the element), not by handles/flags; CEL bindings `elem` (list element/scalar), `key`/`value` (map record) on top of the full sets context (`input`/`incarnation`/`soulprint.self`/`register`/`vars`/`essence`). Opt. `expect: one|at_most_one|any` (default `any`) — runtime assertion of multiplicity `modify`/`remove` (hooked ≠ expected → `error_locked` before commit). Fuses: soul-lint WARN on constant-true/absent `match`; empty-match → no-op (idempotency). Operations are applied in the order of declaration to the intermediate state, one PG transaction, one `state_history`-snapshot, any fail → `error_locked` (barrier/state-commit-invariant [§7](scenario/orchestration.md) is not weakened). Collection type - from `state_schema`; per-RUN semantics (last-wins by SID, NOT per-host union). Fixes **latent bug**: `appends`/`modifies` ([ADR-009 §7.1](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation)) were no-op placeholders without a value source - `incarnation.state` did not grow (`add_replica`/`add_user`/`update_acl`). **`remove` (state_changes) ≠ `delete` (migration-DSL)** - intentionally different names (remove collection element vs remove schema path). Not entered: `clear`/`rename`/`move`/`upsert`/positional remove/paired `*_one`/`*_all`/flag `all:`. Transit (breaking): map form (`sets`/`appends`/`modifies`) is parsed for one release as DEPRECATED (dual-parse + soul-lint warn), then deleted. **Amends [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation)** (Grammar §7.1). Regulatory spec - [`docs/scenario/orchestration.md §7.1`](scenario/orchestration.md).

### [ADR-058. Federated Operator Authentication (Archon) - LDAP + OAuth2/OIDC](adr/0058-operator-auth-ldap-oidc.md)

Moved to [`docs/adr/0058-operator-auth-ldap-oidc.md`](adr/0058-operator-auth-ldap-oidc.md). Federated-login operators: external IdP **validated on Keeper** (LDAP search-bind `go-ldap/v3` / OIDC authorization-code flow with mandatory **PKCE (S256)**, discovery+JWKS-validation `id_token` via `go-oidc/v3`), then identity **mapped** to registry `operators` (AID) + RBAC roles from groups, after which an **internal JWT** is issued to the existing `jwt.Issuer` (ADR-014) in the HttpOnly+Secure+SameSite=Strict cookie `soul_session`. Auth-middleware/RBAC/MCP/OpenAPI remain JWT-based and **do not change**. Public endpoints outside `/v1`: `POST /auth/ldap/login`, `GET /auth/oidc/{login,callback}`; OIDC flow-state - cluster-shared on Redis (state→nonce/PKCE-verifier, single-use GETDEL, TTL 5m). Provisioning-policy `provisioning_allowed_methods` + `/v1/provisioning-policy` (perm `provisioning.read`/`provisioning.update`, problem `provisioning-method-disabled`); auto-provision writes the line `operators` (`created_via='ldap'`\|`'oidc'`, `created_by_aid=NULL`). Extension `auth_method` enum (`ldap`/`oidc`, only-add, migration 083); field `operators.created_via` + bootstrap index relax on `WHERE created_via='bootstrap'` (migrations 084/085) + seeding `archon-system` (086); config blocks `auth.ldap`/`auth.oidc`/`auth.rate_limit` (secrets - Vault `*_ref`, TLS-required). Anti-bruteforce login endpoints - LoginGuard (per-IP/per-username throttle+lockout over Redis, fail-closed, problem `auth-throttled`). **Amends [ADR-014](#adr-014-operator-identity-model-archon)** (`auth_method`/`created_via`), **[ADR-013](#adr-013-bootstrap-of-the-first-archon)** (bootstrap invariant on `created_via`), **[ADR-029](#adr-029-service-registry--postgres)**.

### [ADR-059. Pluggable audit sink — PG / Kafka / off](adr/0059-audit-sink-pluggable.md)

Moved to [`docs/adr/0059-audit-sink-pluggable.md`](adr/0059-audit-sink-pluggable.md). **Status proposed / deferred - design-only, no code in beta** (post-beta plan; synchronous PG-recording audit on 100k VM does not scale - user decision to upload to Kafka under the toggle switch, without in-app preview). Backend audit-upload - selecting an **implementation** of the existing abstraction `shared/audit.Writer` (the new interface is not introduced; `MultiWriter`-tap [Herald](adr/0052-herald-notifications.md) is saved), insertion - `setupAudit` in [`keeper/cmd/keeper/daemon.go`](../keeper/cmd/keeper/daemon.go). Config `keeper.yml → audit.sink: pg | kafka | off` (default **`pg`** = current PG-`audit_log` [ADR-022](adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention), mandatory circuit [ADR-053](adr/0053-dependency-tiers.md) is intact; `kafka` - new sink with `brokers`/`topic`/`acks=all`, secrets vault-ref; `off` - conscious no-unloading, intelligent log). Both implementations in `keeper/internal` - `shared` remain **pgx-free and Kafka-free** ([ADR-011](adr/0011-go-layout.md)). Guarantees: at-least-once `acks=all`, degradation when Kafka is unavailable - **fail-closed** (audit compliance is critical, events cannot be lost), downstream deduplication by `audit_id` (ULID PK). Write guarantee - two candidates (open): transactional outbox (stronger, but PG-write remains) vs direct-producer + durable-fallback. Tier: Kafka is strictly **OPTIONAL-with-degradation**, NOT 4th required ([ADR-053](adr/0053-dependency-tiers.md)). Switching sink - **restart-required** (pattern `web_ui_enabled` [ADR-055](adr/0055-embed-ui-bundle.md)). Replaces the Redis-Stream version of the audit-scaling backlog; batched-INSERT remains (cheaper alternative). **Hard dependency BEFORE implementation (open question):** `changed_tasks`/`incarnation.run_completed` ([ADR-052 §k](adr/0052-herald-notifications.md)) and `GET /v1/audit` today derive data using an SQL query for `audit_log` in PG - with `sink: kafka` without PG they silently break (Herald-notifications + audit-reading); an alternative source of events is needed (in-memory run-aggregate / hybrid sink / Kafka projection). Working name `audit sink`; thematic **Chronicle** is an alternative candidate, NOT recorded in [naming-rules.md](naming-rules.md). **Amends [ADR-022](adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)** (index: PG remains default + source-of-truth FOR THE TIME) **/ [ADR-053](adr/0053-dependency-tiers.md)** (OPTIONAL string).

### [ADR-060. Trait — operator-set key-value of labels on incarnation (relocated from Soul)](adr/0060-traits.md)

Moved to [`docs/adr/0060-traits.md`](adr/0060-traits.md). **Trait** - operator-set key-value of label (scalar\|list value, e.g. `namespace: dba-ns` / `owners: [alice, bob]` / `product: aboba`), **separate axis next to flat Coven** (Option B; extension `souls.coven` to key-value rejected - would break [ADR-008](#adr-008-coven---stable-logical-tags-only) - flat label semantics, RBAC scope-pushdown `$1 = ANY(coven)` ([ADR-047](adr/0047-purview.md)) and predicates `'x' in soulprint.self.covens`). **R1 (2026-06-25): Trait RELOCATED per-soul → per-incarnation.** Source of truth - `incarnation.traits jsonb NOT NULL DEFAULT '{}'` + GIN (migration `088`, mirror `incarnation.covens`/`046`), operator-set in `incarnation.spec.traits` on create. Projected **MATERIALIZED** to `souls.traits` member hosts via **sync-hook** (`incarnation.SyncTraitsToHosts`, reuses `soul.BulkReplaceTraits`; sidebar - incarnation-create + bind host `core.soul.registered`). `souls.traits` (`087`) REMAINS as **projection target**: read layer is reused without changes - projection `soulprint.self.traits` (registry, like `covens`/`choirs`, [ADR-018](#adr-018-soulprint-typed-mvp-scheme); `SoulprintFacts` proto and registration contract Soul→Keeper **do not change**), `topology.HostFacts.Traits`, soul-lint whitelist `traits`. Targeting - `where: soulprint.self.traits.<key>` (`soulprint` declared CEL `DynType`, [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files) - the key is resolved dynamically, without AST-rewrite): `traits.namespace == 'dba-ns'` / `'alice' in traits.owners`. **Transitional state:** per-soul bulk-write `POST /v1/souls/traits` (+ MCP `soul.traits-assign`) still works on `souls.traits` directly, BUT is overwritten by the projection at the next sync `incarnation.traits` - expected to relocate per-soul bulk → per-incarnation (+ deprecate, next slice). **RBAC-scope for traits on the incarnation dimension - UNLOCKED** R1 (implementation - next slice). **Amends [ADR-008](#adr-008-coven---stable-logical-tags-only) / [ADR-018](#adr-018-soulprint-typed-mvp-scheme).**

### [ADR-061. Single-run provision→onboarding→role: onboarding-await + mid-run re-resolve roster](adr/0061-onboarding-await-and-midrun-reresolve.md)

Moved to [`docs/adr/0061-onboarding-await-and-midrun-reresolve.md`](adr/0061-onboarding-await-and-midrun-reresolve.md). **One create-scenario** deploys an N-shard cluster from "nothing": provision N VM (`core.cloud.provisioned`, `on: keeper`, [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read)) → waiting for onboarding of created Souls → mid-run roster growth → applying redis role to already online hosts. Closes the blocker "`soulprint.hosts` - snapshot at the start, mid-run does not grow; `refresh_soulprint` is ignored; there is no onboarding barrier." Two abilities on existing `core.soul.registered` (**NOT** new module - user decision: barrier adjacent to registration; separate `core.soul.online` rejected as an extra entity). **(1) onboarding-await:** new input flags `await_online` (bool) / `await_timeout` (duration, required-when `await_online`) / `await_min_count` (int, opt, default = number of registered SIDs) / `await_poll_interval` (duration, opt, ~2s) - after register+coven the step blockingly polls the **Redis SID-lease** (`keeper/internal/redis/SoulsStreamAlive`, source of truth online - NOT PG `souls.status`, [ADR-006](#adr-006-cache-and-coordination---redis)) to `await_min_count`/timeout; **B1-strict** (online < min to timeout → step `failed` → fail-stop → `incarnation.state` not committed → `error_locked`); output `register.<name>` added `online[]`/`pending[]`/`satisfied`; ceiling `keeper.yml::max_await_timeout` (DoS-guard, fail-closed - exceeding → `failed`, not silent cutting). **list-SID:** `params.sid` accepts a string OR a list (the barrier aggregates presence across all SIDs in one step). **(2) mid-run re-resolve:** flag `refresh_soulprint` has been revived (was a stub `refreshed: false`) - after the success of the scenario-runner step, the incarnation roster will be re-solved before the NEXT Passage; **monotonous growth** (+hosts only, deleting mid-run is prohibited). The stability roster invariant is weakened: "stable within the Passage" (not the entire run). barrier/state-commit-invariant [ADR-009 §7](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) **NOT** weakened (state is committed once after the last Passage). **Stratify:** `refresh_soulprint: true` makes the task a passage-defining boundary ([ADR-056](adr/0056-staged-render-passage.md), symmetrical to the probe-emitter) - consumers `soulprint.hosts`/`on: [incarnation.name]`/`soulprint.self.*` leave for Passage strictly AFTER. **HA:** provision scripts are recommended to be run via Voyage ([ADR-043](adr/0043-voyage.md), recovery closed); standalone staged-recovery of a long barrier - open. **S1 (`await_online`) implemented; S2 (Stratify-border) / S3 (actual re-resolve in run.go) - the contract is fixed, implementation is in separate slices.** **Amends [ADR-009 §7](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) / [ADR-056](adr/0056-staged-render-passage.md) / [ADR-006](#adr-006-cache-and-coordination---redis) / [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read).**

### [ADR-062. Named input types - reusable named input schemes via `types:` + `$type`](adr/0062-input-types.md)

Moved to [`docs/adr/0062-input-types.md`](adr/0062-input-types.md). Reused named input schemas instead of duplicating inline-`object` between service scripts. Section **`types:`** in the service-level file `service/<name>/types.yml` — map `<PascalCase>` → schema in **the same** InputSchema-DSL ([docs/input.md](input.md)); reference **`$type: <Name>`** as a standalone field OR `items: {$type: <Name>}` for an array. Resolve **service-level** (NOT local-per-scenario, NOT cross-service); MVP = object + array-of-type + nesting type→type with **mandatory cycle-detection**; WITHOUT scalar-alias/generics/cross-service. Errors `input_type_unknown` / `input_type_cycle` / `input_type_duplicate` / `input_type_ref_conflict`. DTO `/v1/scenarios` resolves `$type` **backend-side BEFORE projection** (UI receives expanded inline schema) + annotation **`x-type: <Name>`** (forward-compat UI widget). **Replaces the unrealized work `$ref`/`schemas/`** - it has been removed from this document and [service/manifest.md](service/manifest.md). **Amends affected by `$ref` in [ADR-003](#adr-003-destiny-format-is-yaml-with-typed-schema-cuejson-schema) / [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation).**

### [ADR-063. core.bootstrap.delivered — keeper-side delivery of bootstrap token via SSH](adr/0063-bootstrap-token-delivery.md)

Moved to [`docs/adr/0063-bootstrap-token-delivery.md`](adr/0063-bootstrap-token-delivery.md). New keeper-side core module **`core.bootstrap.delivered`** (`on: keeper`) for thin delivery of per-VM bootstrap token via SSH to newly created cloud-init-VMs. **Closes BUG#2 cloud-provision**: stub-address `keeper.push.applied` keeper-side did not exist (no such module) → created VM ([ADR-061](adr/0061-onboarding-await-and-midrun-reresolve.md)) did not receive a token → CSR onboarding did not start → barrier `await_online` did not gain presence → `error_locked`. **Design A1 "thin delivery":** cloud-init (B-flat, [ADR-017(h)](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read)) has already installed soul-binary+CA+systemd-unit; module puts **ONLY** token (`/etc/soul/token`, umask 077, chmod 0400, **★ token in STDIN, not in argv** - otherwise it will leak to `ps`/audit on VM) + opt. `systemctl start soul`. Per-host flow (**sequentially**): `SshProvider.Authorize` (deny → fail-closed) → ephemeral ed25519-keypair + `Sign` → `push.Dial` (CA-signed host-cert verify) → write token (STDIN) → opt. start. **B1-strict** (any host error → step `failed` → state not committed → `error_locked`). Params: `hosts` (`${ register.<provision>.hosts }` from `core.cloud.created`), `ssh_provider`, opt. `token_path`/`ssh_user`/`ssh_port`/`start_soul`. Output `register.<name>.hosts[]={sid,delivered,started}`+`count` — **WITHOUT token**; audit `bootstrap.delivered` - `{action, ssh_provider, count, sids}` without tokens (parallel to cloud.provisioned-masking). **Reuses** [`keeper/internal/push`](keeper/push.md) (ephemeral keypair / Sign / Dial / Session - same path as `SshDispatcher.SendApply`). Registration in `coremod` is **conditional** (like `core.choir` with `ChoirStore`): with a full set of SSH-deps (provider card + host-CA from Vault + dialer), otherwise the "unknown keeper-side module" step is dropped. **MVP limits:** one key-based SshProvider, token only, hosts sequentially. **★ C1 - cloud-init CA-signed host-key (required-for-live, NEXT slice):** `push.Dial` trusts only host-cert signed by host-CA (TOFU refusal) - a fresh VM must have a CA-signed host-key, otherwise the handshake is rejected and delivery fails on connect; up to C1, the module is valid in render (L0 Trial) + unit tests, but live-e2e will not pass. **Amends [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read) / [ADR-061](#adr-061-single-run-provisiononboardingrole-onboarding-await--mid-run-re-resolve-roster) / [ADR-015](adr/0015-core-modules-mvp.md).**

### [ADR-064. Secret write-path - receiving a plaintext secret from the operator, writing to Vault keeper-side](adr/0064-secret-write-path.md)

Moved to [`docs/adr/0064-secret-write-path.md`](adr/0064-secret-write-path.md). Dual-mode for receiving a secret in Herald- and Provider-CRUD: the operator passes **`secret`** (plaintext) **XOR** **`secret_ref`** (vault-path, current behavior) - to the UI radio "value / path". With plaintext Keeper **itself** writes the secret to Vault along the deterministic path `secret/<domain>/<entity>/<field>` (`vault.Client.WriteKV` - the same as `sigil.Introduce` / cert `issueMaterial`) → in Postgres puts only the internal ref `vault:<path>#<field>` (as sigil/warrant); plaintext **does not persist anywhere** (Vault only). With `secret_ref` - behavior is the same as now (Keeper does not write to Vault). **Generalization of the already existing keeper-side write-path** (`sigil.Introduce` / cert `issueMaterial` / `core.vault.kv-present`) to a new case - receiving plaintext **FROM the operator** (not generated by the system), not a new infra-code. **★ Security trade-off (conscious relaxation):** plaintext over the wire operator →Keeper breaks the premise "the secret does not leave the Vault" ([requirements.md](requirements.md)); relaxation for the sake of UX **confirmed by the user (2026-07-01)** WITH mandatory mitigation blockers: **(a)** TLS transport is required; **(b)** strict masking of all sinks (logs/audit/OTel/UI) + guard leak tests - field `secret` under `shared/audit.sensitiveKeyRe`, but **huma-request-body is NOT auto-masked** → explicit audit of body logging points; **(c)** plaintext non-persist; **(d)** vault-policy Keeper with write prefixes `secret/herald/*`/`secret/provider/*`. `update` rewrites to a stable path (idempotent). RBAC - **reuses** `herald.create`/`provider.create` (no new permission). Name - **DevOps field `secret`**, without new dictionary pattern. Scope MVP = **Herald** (webhook-secret + channel-token) + **Provider** (cloud creds); TLS-PEM operator **deferred** (ref in essence → pulls essence→PG migration). Rejected: pattern-name `Consign`/`Entrust`; `oneof`-contract form; permission `secret.write`; ULID-immutable path. **Status: accepted, implementation pending** (fixation of the decision in a document). **Amends [ADR-052](#adr-052-herald--tiding---notifications-about-running-events) / [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read).**

### [ADR-065. core.module.installed - delivery of SoulModule plugins to the Soul host (FetchModule + plugins.soul_modules)](adr/0065-core-module-installed.md)

Moved to [`docs/adr/0065-core-module-installed.md`](adr/0065-core-module-installed.md). Canonical channel for delivering custom modules (`soul-mod-*`) to the Soul host; **closes open Q No. 5** "where the registry of modules in Keeper lives." **Transport** - the third RPC in `service Keeper`: server-streaming **`FetchModule(PluginFetchRequest) returns (stream PluginChunk)`** (the same mTLS-listener as EventStream; artifact bytes travel **in a separate HTTP/2 stream** and do not choke the control-plane; **content-addressed** - Keeper gives ONLY bytes whose sha256 is in the active Sigil-admission `kind: soul_module`; authorization - mTLS peer-cert; guard-rails: `plugins.max_artifact_size_mb` + rate-limit parallel fetch per-SID; only-add by [ADR-012(c)](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)). **Byte register** - config-directory **`plugins.soul_modules[]`** (`{name, source, ref}`, symmetry `cloud_drivers`/`ssh_providers`) + existing plugingit-resolve into FS cache + Sigil-tolerance (review [ADR-026](#adr-026-sigil---plugin-integrity-keeper-signed-digest-index) without changes); **NO new repository** - PG = permissions (authority sha256), FS = bytes, git = origin ([ADR-007](adr/0007-versioning-git-ref.md)); HA - per-instance FS cache, miss slot → on-demand resolve or failure with retry; S3-artifact-store is a post-GA extension behind the fetch abstraction. **Params `core.module.installed`** (Soul-side, state `installed`): `name` (required, `<ns>.<name>`), `ref` (opt, **pin-reconciliation** - active tolerance must be on this ref, otherwise `failed`; NOT version selection); idempotency - sha256 of the existing binary == sha of the active Sigil → `changed=false`. **hot-register is MANDATORY in MVP** (thread-safe Rescan without restarting the daemon - otherwise `community.redis.*` in the SAME run after the install step does not work; restart = break EventStream = break of run); The beacon registry is NOT rebuilt during rescan (post-MVP). **Scenario** — amendment 2026-07-03: Keeper **synthesizes** Soul-side install steps from the explicit manifest declaration `service.yml::modules[]` (scenario-runner, after expanding include BEFORE Stratify; insertion before the first consumer of the module; explicit step `core.module.installed` with the same literal `params.name` disables synthesis - operator controls position/ref/when itself); validation-hint post-MVP is saved in the reverse direction (the module is used in tasks, but is not declared in `modules[]`). **Sigil-verify install-time:** allow-check BEFORE fetch (no tolerance → `module_not_allowed` to single network byte) + full verify (signature + `manifest_sha256`) before atomic rename; manifest is materialized from `PluginSigil.manifest_raw` (not via fetch); review `shared/pluginhost`, NO new trust mechanisms. Soul cache layout is **catalog** `<paths.modules>/<ns>-<name>/{manifest.yaml, soul-mod-<name>}`. TaskError-reasons: `module_not_allowed` / `module_fetch_failed` / `module_verify_failed`. MVP boundaries: without `absent`-state (TTL-cleanup), ~~without auto-inject~~ (removed by amendment 2026-07-03 - auto-synthesis from `modules[]`), without beacon-hot-reload. **Amends [ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) / [ADR-020](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format) / [ADR-015](#adr-015-core-mvp-modules-exact-list).**

### [ADR-066. Onboarding via Teleport on platforms without cloud-init userdata - environment restrictions (WB) and work profiles](adr/0066-teleport-onboarding-profile.md)

Moved to [`docs/adr/0066-teleport-onboarding-profile.md`](adr/0066-teleport-onboarding-profile.md). **Deployment-profile solution on top of existing mechanics** ([ADR-063](adr/0063-bootstrap-token-delivery.md) Teleport-transport/full-install/init-phase, [ADR-061](#adr-061-single-run-provisiononboardingrole-onboarding-await--mid-run-re-resolve-roster) await-barrier, [ADR-017(h)](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read) cloud-init/self-onboard) - their contracts do not change; records the restrictions of the external environment and standard onboarding profiles for it. **WB environment restrictions (live-tested 2026-07-01/02):** (1) `ci_user_data` is disabled on the WB side **at the namespace level** (matrix: all authorizing keys × dev/stage+prod clouds × user-key/service-account → `ci_user_data is not allowed for this user` BEFORE checking rights) → both userdata paths (B-flat and self-onboard T) are not available; (2) direct SSH keeper→VM closed → `transport: direct` unavailable; (3) access to VM - only Teleport, with Proxy behind a public L7-TLS balancer, VM behind NAT. **Production profile: full-install via `core.bootstrap.delivered` `transport: teleport`** with mandatory environment requirements: **bot-identity (Teleport Machine ID) without `pin_source_ip`** - identity from the interactive `tsh login` carries PinnedIP (OID `1.3.9999.1.9` = src-IP of the check-out machine; keeper with a different src-IP → access-denied) + MFA/TTL restrictions on the interactive session; `alpn_upgrade: true` + private LB root in the system trust of the keeper process; working **external IP** on the VM for Teleport-enroll (`set_external_ip: true` in profile + driver-probe is waiting for external activation). History of 403 diagnosis: first attributed to PinnedIP, actual code wall - ALPN `NextProtos` h2-first (fix `NextProtos=nil` in dial_teleport.go); PinnedIP remains an objective product-limitation identity. **Live-proven by run-23 E2E create** (2026-07-02: VM from scratch → external IP → enroll → automatic onboarding → redis deployment 77 tasks → `ready`). **Demo/dev-profile (bastion):** local keeper + enrolled-VM as bastion (`tsh ssh -R` reverse tunnel; Teleport agent binds `0.0.0.0` itself) + onboarding soul external Teleport shell-exec from the operator's machine (src-IP = pin); keeper-cert must carry the IP bastion in the SAN, endpoint soul.yml - IP not FQDN; **borders: demo/dev, not prod**. **Rejected:** `core.teleport.shell` (plan B: external binary by Sigil [ADR-026](#adr-026-sigil---plugin-integrity-keeper-signed-digest-index), epic ~15 files, does NOT solve PinnedIP - left only as fallback); waiting for `ci_user_data` to be enabled as a blocker (profiles coexist - when enabled, userdata self-onboard T will work without code changes).

### [ADR-067. Mandatory log-shipping (Vector) - log plane of data services](adr/0067-vector-log-shipping.md)

Moved to [`docs/adr/0067-vector-log-shipping.md`](adr/0067-vector-log-shipping.md). **Vector** (`vectordotdev/vector`) - **PUSH log plane**, installed in all data services (redis/dragonfly/...) **required** ("like a node-exporter"), an invariant of the service, not an option. **Adds** node-exporter/redis_exporter (metrics, pull) - three independent observability layers (host metrics / Redis metrics / logs), do not intersect. **Design - clone of the node-exporter standard** (stateful branch [`production-conventions.md`](destiny/production-conventions.md)): new standalone-destiny [`vector`](../examples/destiny/vector/destiny.yml) - `core.url.fetched` (release-tarball GitHub, **mandatory `sha256`** fail-closed) → `core.archive.extracted` → **stable system account** (NOT `DynamicUser`: persistent disk-buffer `data_dir` is undergoing a restart, needs a fixed uid) + `data_dir` 0700 / `config_dir` 0750 → `core.file.present` binary + `core.file.rendered` `vector.yaml` (sources/sinks) + hardened systemd-unit + `core.service.running`/`restarted` (onchanges config/unit/binary); arch = Rust triplet (`soulprint.self.os.arch` amd64/arm64 → x86_64/aarch64). **Does not introduce a new core module** - compiled from existing ones ([ADR-015](adr/0015-core-modules-mvp.md)). **Embedding - unconditional `apply: destiny vector` at the end of `create`** (after the deploy-branch and exporters, without `when:`-gate): the entire contract in **essence** (contract A - author-context, versions/checksum/sink are hidden from the Run-form, `covenant`/`form` are not touched); per-service difference is only `vector_log_sources`. **sink - Option A** (essence per-incarnation: `vector_sink_type` loki/elasticsearch/vector/console, default `console` - without external infrastructure / `vector_sink_endpoint` / `vector_sink_auth_ref`); `sink_auth_ref` — Vault-ref/value, Soul-side resolve, **does NOT settle in state** (symmetry `tls.*_ref`; in unit via `Environment=VECTOR_SINK_TOKEN`, not in `vector.yaml` on disk). **B** (`keeper.yml` globally) / **C** (hybrid) - follow-up. **state read-model** `logging.vector_*` (without `auth_ref`) + bump `state_schema` + per-service migration (redis `013_to_014` v13→v14, forward-only, `has()`-guard). Name `vector` is upstream (node-exporter use case), in [naming-rules](naming-rules.md). **Amends [ADR-024](#adr-024-observability-prometheus-primary--otel-bridge)** (adds the push log plane as a third dimension next to the pull metrics + OTel traces). Related (NOT amend): node-exporter standard (pattern clone, does not have its own ADR; corrects the phantom "ADR-064 node-exporter" design 2026-07-01 - real [ADR-064](#adr-064-secret-write-path---receiving-a-plaintext-secret-from-the-operator-writing-to-vault-keeper-side) = secret-write-path), [ADR-015](#adr-015-core-mvp-modules-exact-list) / [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files) / [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation).

### [ADR-070. Secret reveal-path — disclosure of the plaintext secret of the incarnation to the operator under RBAC rights](adr/0070-secret-reveal-path.md)

Moved to [`docs/adr/0070-secret-reveal-path.md`](adr/0070-secret-reveal-path.md). **READ-double [ADR-064](adr/0064-secret-write-path.md)** (secret write-path): ADR-064 accepts the plaintext secret **FROM** the operator and writes to the Vault keeper-side, this ADR is the reverse direction: it gives the plaintext **BACK** to the operator under explicit permission. **Declarative registry `revealable_secrets[]`** in the manifest `service.yml` (generic, NOT redis hardcode in the kernel): entry `{id, label, enumerate: state.<array>, vault_ref: "secret/…/{incarnation}/…/{key}#field"}` - the service itself declares that its incarnations are disclosed. **Restricted placeholders `{incarnation}`/`{key}`** - literal substitution of validated values, **not CEL** (less attack-surface than computable language in the path to the secret): `key` is obliged ∈ enumerate-array **current** state (anti-arbitrariness), manifest version is always `incarnation.ServiceVersion` (anti version-craft), the result is run through `vault.ParseRef` (traversal-guard `..`). **Endpoints:** `POST /v1/incarnations/{name}/secrets/reveal` `{secret_id, key}` → `{value}` (self-audit `incarnation.secret_revealed` - `{name, secret_id, key, path}` **WITHOUT value**) + discovery `GET /v1/incarnations/{name}/secrets/revealable` → `{items: [{secret_id, label, state_path, keys}]}` (READ, no audit). Authorized expansion → 200-DTO **past `MaskSecrets`** (the only point where the value exits the domain). **★ Security trade-off** (mirror ADR-064: plaintext Keeper→wire operator, TLS) WITH mandatory mitigation blockers: RBAC-gate `incarnation.view-secrets` (scope `coven=`/`service=`/`incarnation=`, fail-closed **404** outside scope) / fact audit WITHOUT value + leak-guard tests for each sink / no body-logging (plaintext with response body only) / key-in-state / traversal-guard / vault-policy read-prefix (`secret/data/redis/*`). The right **`incarnation.view-secrets`** is a new scoped right ([ADR-047](adr/0047-purview.md)), **strictly more privileged than `incarnation.get`**; MCP-tool **no** (REST-only, like `form-prefill`). Rejected: redis-specific hardcode endpoint (in favor of generic registry); CEL in `vault_ref` (in favor of limited placeholders); reuse `incarnation.get`. Deferred (post-MVP): singleton secrets without `enumerate` (admin-password `secret/redis/{incarnation}#password`); the live manifest `community.redis` carries a follow-up section in the module repo. Implemented **NIM-74**. **Amends [ADR-064](adr/0064-secret-write-path.md) / [ADR-047](adr/0047-purview.md).**

## Plugin infrastructure

Soul Stack has three categories of extensions: **Destiny modules**, **cloud providers**, and **SSH push providers**. All three use **single plugin infrastructure** - the same handshake mechanism, protocol, requirements for the artifact. Only the service contract (gRPC service) that the plugin implements changes.

Regulatory fixation of the manifest format, handshake lines, lifecycle, capabilities and side_effects - [ADR-020](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format). The full spec for plugin authors and the host side is [`docs/keeper/plugins.md`](keeper/plugins.md) (the manifest format is the same for all three kinds; `SoulModule` specifics are [`docs/soul/modules.md`](soul/modules.md)).

### General mechanism

- A plugin is a separate executable file, supplied as an independent artifact (its own git repo, its own release pipeline, its own versions).
- Launched by the host process (`soul`-binary or `keeper`) as a sub-process (one-shot, [ADR-020(d)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format)).
- At startup, prints **handshake-string** to stdout - JSON in one line with the magic field `"soul_stack":"plugin-v1"` ([ADR-020(b)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format)).
- Then the host and plugin communicate via **gRPC via Unix domain socket** in the host-managed directory (`/var/run/soul-stack/plugins/` or `/var/run/soul-stack-keeper/plugins/`, mode `0700`).
- The plugin ends with graceful shutdown via SIGTERM (grace 10s → SIGKILL).
- Borrows the "one-line handshake → gRPC-over-socket" model from `hashicorp/go-plugin`, but **not** their code/format/MPL-2.0-license ([ADR-020(b)](#adr-020-plugin-infrastructure-manifest-handshake-lifecycle-format), [ADR-016](#adr-016-parity-strategy-with-saltstackansible-and-soul-stack-license)).

### Three service contracts

| Contract | Who is the host | Who is the plugin | Destination |
|---|---|---|---|
| **`SoulModule`** | `soul`-binary | `soul-mod-<name>` | Implements Destiny steps: `Validate` / `Plan` / `Apply` (see Module Model). |
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
- **Two rounds of validation.** Keeper upon script invocation (fail fast, zero traffic to hosts) + Soul before apply (defense in depth against desynchronization). Details and role of `soul-lint` are in [`docs/destiny/input.md`](destiny/input.md).
- **`soul-lint`** checks statically: well-formed `input:`-block, literals in `when:` against `enum:`. It does not see specific runtime values; it is not linked to the Keeper runtime path ([ADR-004](adr/0004-binaries.md)).
- **Mini-open Q:** name of the block of the same name `params:` on the call-site of the script (`apply: { destiny: …, params: { … } }`) and `input:` destiny. Whether to agree on them is a separate issue.

## Service - structure and manifest

Service is a **service type** (Redis HA, PostgreSQL, Vector-collector). One service - one git repo, with its own versions, with a mandatory manifest and a set of scripts.

### Repository layout

```
redis/
├── service.yml                         # manifest: name, state/host schemas, destiny, modules (version = git tag, see ADR-007)
├── essence/                            # parameters in the hierarchy (see "Essence: assembly pipeline")
│   ├── _stack.yaml                     # OPTIONAL: declarative build pipeline
│   ├── _default.yaml                   # baseline for all incarnation
│   ├── coven/
│   │   ├── prod.yaml
│   │   └── dev.yaml
│   └── os/
│       ├── ubuntu.yaml
│       └── debian.yaml
├── scenario/                           # auto-discover from directory, directory name = script name
│   ├── create/
│   │   ├── main.yml                    # entry point: input + state_changes + tasks (all inline)
│   │   ├── standalone.yml              # reusable mode blocks (include from main.yml)
│   │   ├── templates/                  # OPTS: scenario-local templates (two-level resolve)
│   │   ├── vars.yml                    # OPTS: scenario-locals
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
├── migrations/                         # migrating state_schema between versions
│   ├── 001_to_002.yml
│   └── 001_to_002/
│       └── tests/                      # migration tests (see migrations.md)
└── ...
```

Each folder `scenario/<name>/` is a separate operation (CRUD-style) on the service. `main.yml` - script entry point: contains **inline** `input`, `state_changes` and `tasks`. Neighboring `*.yml` are sub-tasks, included through `include:` into `main.yml`. There is no need to list the scripts in `service.yml` - keeper finds them with auto-discovery based on the directory structure.

### `service.yml` - manifest

```yaml
name: redis
state_schema_version: 2               # incarnation.state structure version, not service version (see ADR-007)

# Structure of incarnation.state in the database
state_schema:
  type: object
  required: [redis_type, redis_config]
  properties:
    redis_type: { type: string, enum: [standalone, sentinel, cluster, sentinel_only] }
    redis_version: { type: string }
    redis_config:
      type: object
      additionalProperties: true
    redis_users:                      # map username → {perms, state}
      type: object
      additionalProperties:
        type: object
        required: [perms, state]
        properties:
          perms: { type: string }
          state: { type: string, enum: [on, off] }
    redis_hosts:
      type: array
      items:
        type: object
        properties:
          sid:  { type: string }
          role: { type: string, enum: [primary, replica, sentinel] }

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
  auto_destroy: true                  # deletion runs the destroy teardown script (by allow_destroy)
```

Scripts and their details are not mentioned in service.yml - keeper finds them from the contents of the `scenario/` directory.

`lifecycle:` - optional block of service incarnation life cycle policy:

- `auto_create: bool` (default `true`) — `POST /v1/incarnations` automatically starts scenario `create`; `false` - the incarnation is created in `ready` without running, the operator launches `create` manually from the Run form.
- `auto_destroy: bool` (default `true`) - deleting an incarnation launches the teardown script `destroy` according to the usual logic `allow_destroy`; `false` - deletion is always direct, without teardown, priority over `allow_destroy`.

Missing block = both `true` (backcompat).

**Scenario-conventions and lifecycle-set.** Lifecycle-set (`LifecycleScenarioNames`) = only `create` / `destroy` - specialized scenario-kinds of the corresponding life cycle phases. `converge` is an **operational** scenario-kind (launched by the usual `run` Apply-reconcile + dry-run target `check-drift`), it is derived from the lifecycle set (see [Amendment ADR-031 2026-06-10](#adr-031-scry--drift-detection-declarative-dry-run-reconcile)). The script directory (`GET /v1/services/{name}/scenarios`) carries the field `runnable: bool` - the sign "launched by the operator from the Run form": `create` = `true`, `destroy` = `false` (special deletion flow via `DELETE /v1/incarnations/{name}`), operational (incl. `converge`) = `true`. The UI filters the Run form by `runnable`, not by name hardcode ([ADR-042](adr/0042-backend-driven-ui.md)).

> The top-level `version:` field in `service.yml` is intentionally missing: the service version is the git-tag under which the file itself is committed (see [ADR-007](#adr-007-versioning-of-artifacts-is-done-through-git-ref-not-through-a-field-in-the-manifest)). `state_schema_version` is a **different** concept (version of the state data structure), needed for migrations, see section below ["Versioning and migrations state_schema"](#versioning-and-state_schema-migrations).

### `scenario/<name>/main.yml` - self-contained operation

The complete regulatory specification of the orchestration layer (`on:`/`where:`, probe-idiom, two-level resource resolution, tests, barrier/state-commit) is [`docs/scenario/`](scenario/README.md). DSL task core (`module:`, `include:`, `block:`, `parallel:`, `loop:`, `register:`, requisites, `retry:`, `timeout:`, `changed_when:`/`failed_when:`) scenario inherits entirely from [`docs/destiny/tasks.md`](destiny/tasks.md) - after [ADR-009](#adr-009-scenario---a-complete-dsl-of-destiny-tasks-border-with-destiny---recommendation) the "scenario only `apply:`" invariant has been removed. Below is an illustration of the format.

```yaml
name: create
description: Initial bootstrap of Redis HA cluster

# Typed script input - validated before run.
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
        acl:   { type: string }
        state: { type: string, enum: [on, off] }
      required: [acl, state]
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

# What does the script write to incarnation.state after a successful apply.
# state_changes - ordered list of CRUD verbs (ADR-057): set/add/modify/
# remove + foreach. set - rewrite the entire field; value from CEL ${ ... }
# (rendered by Keeper-side, scenario/orchestration.md §7.1). Context:
# input/incarnation/soulprint.self/register/vars/essence.
state_changes:
  - set: redis_version
    value: "${ input.redis_version }"
  - set: redis_users
    value: "${ input.users }"
  - set: redis_config
    value: "${ input.config }"
  - set: redis_hosts
    value: "${ input.hosts }"

# Steps - everyone knows where to execute (on: keeper / on: [coven,...] / on: omitted)
tasks:
  - name: provision
    on: keeper                        # on the keeper itself, via CloudDriver
    when: input.spawn != null
    module: core.cloud.provisioned    # keeper-side core (ADR-017)
    state: created
    params:
      provider: "${ input.spawn.provider }"
      profile:  "${ input.spawn.profile }"
      count:    "${ input.spawn.count }"

  - name: install-redis
    on: ["${ incarnation.name }"]     # the whole incarnation (you could have omitted on:)
    apply:
      destiny: redis
      input:
        version:  "${ essence.redis_version }"
        password: "${ input.redis_password }"

  - include: replication.yml
```

The `on:` key decides where the step is executed: `keeper` - locally on the keeper, `[coven, …]` - intersection of covens (⊆ incarnation), omitted - the entire incarnation. Volatile per-host filter based on `register:` of the previous probe - key `where:` ([ADR-008](adr/0008-coven-stable-tags.md)). One script mixes keeper and host steps in a linear flow, in the same task language ([ADR-009](adr/0009-scenario-dsl.md)). This is similar to Salt Orchestration + State, but without two different languages and without two places. Normative semantics - [`docs/scenario/orchestration.md`](scenario/orchestration.md).

The `input:` block validates the script input parameters before running (according to the [docs/input.md](input.md) standard). `state_changes` - **ordered list of CRUD verbs** (`set`/`add`/`modify`/`remove` + `foreach`, [ADR-057](#adr-057-state_changes---ordered-list-of-crud-verbs)): declares **what** the script writes to `incarnation.state` on success and **from where** the value is taken (CEL `${ … }`, rendered by the keeper after the barrier); plurality - through `match`-predicate. Regulatory - [scenario/orchestration.md §7.1](scenario/orchestration.md).

**Reusable named types** - for composite schemas found in multiple service scenarios ([ADR-062](adr/0062-input-types.md)). The type is declared in the service-level file `service/<name>/types.yml` (section `types:`, same input-DSL), and the script refers to it with the directive `$type`:

```yaml
# service/<name>/types.yml
types:
  AclUser:
    type: object
    additional_properties: false
    required: [name, perms, state]
    properties:
      name:  { type: string, pattern: "^[a-zA-Z0-9_-]+$" }
      perms: { type: string }
      state: { type: string, enum: [on, off] }
```

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

`$type` resolves service-level at the input stage (with cycle-detection); We keep the types inline in one script, and only include reused ones in `types.yml`. The previous provision `$ref` for an external JSON-Schema file in `schemas/` **cancelled** (not implemented, replaced by named types) - see [ADR-062](#adr-062-named-input-types---reusable-named-input-schemes-via-types--type) and [docs/input.md → "Reused named types"](input.md#reusable-named-types-types--type).

## Essence: assembly pipeline

The service parameters are not a flat list, but a **hierarchical assembly** in the spirit of Salt `top.sls` / PillarStack: at each step, already accumulated data is available, the next step can rely on them. This gives conditional inclusions, iteration and dynamic values.

### Convention-based default (without `_stack.yaml`)

If the operator does not write `_stack.yaml`, keeper applies the default order:

1. `_default.yaml` — baseline.
2. `os/<soulprint.os.family>.yaml` - OS family (if the file exists).
3. For each Coven tag of the current host: `coven/<label>.yaml` (if the file exists).
4. On top of everything - `incarnation.spec` from the operator (via API).

This is enough for most services.

> **Essence role-agnostic** ([ADR-008](adr/0008-coven-stable-tags.md)). Stages `role/<host.role>.yaml` in the pipeline **no** - essence is not layered by role. Role-dependent parameters move to destiny and are passed through `input:` via the probe role (see [`docs/scenario/concept.md`](scenario/concept.md)). The assembly order is `default → os → coven → incarnation.spec`.

### `_stack.yaml` - declarative pipeline

When conditions, iteration or calculated values are needed, the operator writes `_stack.yaml`:

```yaml
# essence/_stack.yaml
#
# Available at every step:
#   soulprint - host facts (os, kernel, network, custom facts)
#   incarnation — { name, service, scenario, ... }
#   host - { sid, covens } for the current host (role is NOT available here - ADR-008)
#   vars - already accumulated essence from previous steps

stack:
  # 1. Baseline - always
  - file: _default.yaml

  # 2. OS family
  - file: "os/${ soulprint.self.os.family }.yaml"
    optional: true                     # skip silently if file is not present

  # 3. Iterate over all coven labels of the host
  - foreach: "${ host.covens }"
    as: coven_name
    file: "coven/${ coven_name }.yaml"
    optional: true

  # 4. Depends on the ALREADY collected value (PillarStack-style)
  - file: "env/${ vars.env }.yaml"
    when: vars.env != null
    optional: true

  # 5. Conditional inline block - without a separate file
  - inline:
      redis_maxmemory: "${ int(soulprint.self.memory.total_mb * 0.6) }mb"
    when: vars.redis_maxmemory == null
```

### Pipeline operators

| Operator | Destination |
|---|---|
| `file: <path>` | Include file; path is a template with variables. |
| `inline: <map>` | Include a set of variables without a separate file. |
| `when: <expr>` | The inclusion condition (Boolean expression). |
| `optional: true` | Don't crash if file `file:` does not exist. |
| `foreach: <list>` + `as: <name>` | Iteration: The step is repeated for each element, the variable `<name>` is available internally. |

Between steps, keeper re-evaluates the available variables so that a later step can reference the `vars.X` that came from the file included earlier.

### Final merge with incarnation.spec

After passing the pipeline, the keeper makes the final deep-merge: `effective_essence = merge(stack_result, incarnation.spec)`. Spec operator is the strongest override (it interrupts everything that the hierarchy has collected).

The **original `incarnation.spec`** (not merged) is stored in the database - for auditing it is clear what exactly the operator has redefined.

## Incarnation — runtime service instance

Incarnation is a specific **instance** of a service in reality (one Redis cluster, one PostgreSQL cluster). Stored in Postgres, managed via API/MCP.

### Record structure

| Field | Type | Meaning |
|---|---|---|
| `incarnation_id` | UUID | primary key |
| `name` | text UNIQUE | name incarnation, also known as root Coven label |
| `service` | text | service name |
| `service_version` | text | pin version (git-tag) of the service under which incarnation runs |
| `state_schema_version` | integer | version of state_schema under which state is structured |
| `spec` | jsonb | what the operator declared (input for the last successful create/update) |
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

**The script does not write to the database until it has run on all target hosts**. If apply failed partially (for example, `add_user` passed on 2 out of 3 hosts), the state in the database **is not updated**, incarnation goes to `status: error_locked` with `status_details: { failed_hosts: [...], partial_changes: {...} }`. Any subsequent script for this incarnation is rejected until express permission from the operator.

Permission:
- `keeper.incarnation.unlock name=X reason="manual cleanup verified"` - the operator assumes that he has verified the fact on the hosts.
- Special recovery script - service can declare it to automate typical cases (clean up, roll back, forward).
- `keeper.incarnation.rerun-last name=X reason="..."` (REST: `POST /v1/incarnations/{name}/rerun-last`, permission `incarnation.rerun-last`) - atomically removes `error_locked` (state is not touched, last-known-good, snapshot in `state_history`) and with the same action restarts **last fallen script** incarnations: this is the create option (`create`/`create_from_souls`) for a bootstrap file or any day-2 script. For day-2, input is restored from the `apply_runs.recipe` failed run; if the recipe is unavailable (retention/legacy) - `409` fail-closed "Unlock + manual start". Under one `FOR UPDATE`: transition `error_locked → applying` bypassing `ready` - eliminates the window in which a concurrent run would slip into the vacated `ready`. The restart starts in the "reserved `applying`" (`RunSpec.FromLocked`) mode: the runner does NOT transit the status again, but verifies that the line in `applying` is fail-closed, another status rejects the start without applying the script over the intercepted recovery line. The "operator explicitly confirmed" invariant is preserved by the mandatory `reason` + confirm in the UI; audit event `incarnation.rerun_last` (does NOT reuse `incarnation.unlocked`). For a bare incarnation without a fixed script and with an inaccessible recipe, the usual `unlock` + manual repeated run remains.

This is behavior that is **not present in Ansible**: there partial applications quietly pass, drift accumulates. We have a fail-fast with a clear point of operator responsibility.

### Statuses `destroying` and `destroy_failed`

Teardown incarnation (`keeper.incarnation.destroy`) goes through two statuses (implemented, `keeper/internal/incarnation`, CHECK `incarnation_status_valid` - migrations 005 + 031 + 036):

- **`destroying`** (S-D1) - the operator initiated destroy: teardown was launched through scenario `destroy` (S-D2b) followed by `DELETE` lines (S-D3). This is **not the terminal of the line itself** - if successful, the line is deleted (single-winner `DELETE … WHERE status='destroying' RETURNING`, see ADR-027(j)), if teardown fails, it goes to `destroy_failed`. From `destroying` all other operations (`run` / `upgrade` / repeated `destroy`) are rejected fail-closed; The force-path removes the line immediately. Recovery of "dangling `destroying`" (dead owner) - the same single-winner `DELETE`, see ADR-027(j).
- **`destroy_failed`** (S-D2a) — teardown (scenario `destroy`) crashed on hosts: instance **not deleted**, `state` remains last-known-good (teardown works with hosts, not with jsonb-state). A terminal that requires operator intervention: from it the operator repeats destroy, force-destroys or removes in `ready`. Separate status (and **not** `error_locked`), because the semantics and recovery paths are different - partial teardown failure ≠ partial apply failure.

`migration_failed` — terminal of failed state_schema migration ([Versioning and state_schema migrations](#versioning-and-state_schema-migrations), ADR-019): `ROLLBACK`, state is not saved, incarnation is locked; same value in CHECK-constraint and `ValidStatus` (`keeper/internal/incarnation`).

Current implemented enum (`ValidStatus` + CHECK `incarnation_status_valid`) = `ready` / `applying` / `error_locked` / `migration_failed` / `destroying` / `destroy_failed`. **`drift`** is entered into `ValidStatus` + CHECK migration according to [ADR-031](#adr-031-scry--drift-detection-declarative-dry-run-reconcile) (Scry) - **informational, NOT blocking** status (remediation = normal apply from `drift` → `ready`); implementation in operation (on-demand pilot). **`provisioning`** in the table - **post-MVP** (phase not yet implemented): in the enum directory above, but not yet accepted by the code - will appear in `ValidStatus`/CHECK when the corresponding phase is implemented.

### Operator API

```
keeper.incarnation.create name=X service=Y input={...}      # run create script
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

Coven - **only stable** logical tags (cluster / project / environment / data center / hardware type). When an incarnation is created, its name becomes the **root Coven label** of all its hosts:

- `incarnation.name = test-cache-redis-cl-dev` → coven `test-cache-redis-cl-dev`.

**There are no more sub-covens by role (`{incarnation.name}-{role}`)** - convention removed ([ADR-008](adr/0008-coven-stable-tags.md)). The role (master / replica) **not Coven**: it is volatile (failover) and is not suitable for a stable label. Covens are assigned automatically by **keeper**; additional stable covens (for example, `baremetal`, `prod`) are assigned declaratively via incarnation, the operator does not make separate "tag host" API calls.

### `on:` - stable step target

Script step target - key **`on:`**, resolved by Postgres (stable layer):

```yaml
# Entire incarnation (on: omitted - root coven implied)
- name: Apply base config everywhere
  apply: { destiny: redis-base, input: { ... } }

# Local task on the keeper itself (cloud-create, vault-resolve, http-call)
- name: Provision VMs
  on: keeper
  module: core.cloud.provisioned
  state: created
  params: { ... }

# Intersection (AND) of stable covens, always ⊆ incarnation hosts
- name: Tune kernel on bare-metal hosts of this cluster
  on: ["${ incarnation.name }", baremetal]
  apply: { destiny: kernel-tuning, input: { ... } }
```

**Resolver contract** (invariant): list in `on:` - AND/intersection covens; result **always ⊆ hosts incarnation**; **cross-incarnation targeting is prohibited by the grammar** (security invariant); role in `on:` is not involved. Completely - [`docs/scenario/orchestration.md §3`](scenario/orchestration.md).

### `where:` - volatile role via probe + register

The volatile role (who is now master) is not stored anywhere stably. The script puts a **probe step** (`module: core.exec.run` + `register:` + `changed_when: false` + `failed_when:` for completeness), then targets the next step with the key **`where:`** - a volatile predicate on `register:` of this probe, per-host:

```yaml
- name: Detect actual redis role per host
  module: core.exec.run
  on: ["${ incarnation.name }"]
  register: redis_role
  changed_when: false
  failed_when: size(register.redis_role) < incarnation.host_count
  params: { command: "redis-cli role | head -1" }

- name: Restart only the current replicas
  on: ["${ incarnation.name }"]
  where: register.redis_role.stdout == 'slave'
  module: core.service.restarted
  params: { name: redis-server }
```

Two-phase resolve: `on:` → Postgres (stable), `where:` → `register:` (runtime). After failover - just a new probe; no cache, role collector or volatile Soulprint facts ([ADR-008](adr/0008-coven-stable-tags.md)). `where:`-step key and `soulprint.where(...)`-function in the expression - **different positions** (on which hosts vs where to get the data), detailed in [`docs/scenario/orchestration.md §4`](scenario/orchestration.md). The former `filter:` from the old examples was removed and replaced with `where:`.

In the template context of the script, the following are always available:
- `incarnation.name` - instance name.
- `input` - parameters passed to the script.
- `essence` — merged essence (default + spec) after passing [pipeline](#essence-assembly-pipeline).
- `state` - current state from the database (for scripts that read the existing state).
- `soulprint.hosts` - list of run hosts with stable facts (`sid`/`role`(declared)/`network`/`os`/`covens`); `.where("<predicate>")` filters by CEL predicate string. A shortened form of the same request is `soulprint.where("<predicate>")` (for example, `soulprint.where("'X' in covens")` instead of `soulprint.hosts.where("'X' in covens")`). Scenario-only; receive destiny topology only through explicit `apply: input:`. Regulatory - [`docs/scenario/orchestration.md §4.1`](scenario/orchestration.md).

> **Difference from destiny template context.** In destiny, `vars.*` means [destiny locales from `vars.yml`](destiny/vars.md), and `essence.*` is **absent**: destiny is isolated, receiving only what came in `input:`. In scenario, on the contrary, `essence.*` is directly accessible (hence scenario puts values in `input:` destiny when `apply:` is called), but destiny-`vars` is not visible - it is local to a specific destiny.

### Host communication via Soulprint

When a host needs data from another host (what is done in Salt via Mine, in Ansible via `hostvars[X]`), the `soulprint.where(<predicate>)` function is used on the **stable** layer (the CEL predicate is a static string literal):

```yaml
master_addr: "${ soulprint.where(\"incarnation.name in covens\")[0].network.primary_ip }"
```

The request goes to Postgres + Redis hot layer. Soulprint after [ADR-008](#adr-008-coven---stable-logical-tags-only) stores **only stable** facts, so `soulprint.where(...)` operates on a stable layer; volatile role (who is now master) - exclusively through probe + `where:`-key, not through Soulprint. The predicate `.where(...)` is a static string literal, expanded at the compile phase into the native CEL filter-comprehension (not runtime; dynamic merging of the predicate is prohibited, the first element is `[0]`, see [ADR-010](#adr-010-template-engine-cel-for-yaml-expressions-go-texttemplate-for-files)). Cross-host master discovery - through the accessor `soulprint.hosts` (`soulprint.hosts.where("role == 'primary'")[0].network.primary_ip`), the declared role is taken from `incarnation.spec.hosts[].role`, a probe is defined on the runtime master (as in `restart`); normative - [`docs/scenario/orchestration.md §4.1`](scenario/orchestration.md) (the former open Q is closed there, see [§8](scenario/orchestration.md)).

## Versioning and state_schema migrations

> We are only talking about the **structure version `incarnation.state`** - this is not a "service version". The version of the service itself as an artifact is the git tag under which it is committed (see [ADR-007](#adr-007-versioning-of-artifacts-is-done-through-git-ref-not-through-a-field-in-the-manifest)). `state_schema_version` is a separate entity, needed **only** for jsonb-state migrations to the database, and coexists with the service's git tags independently.

Service developer changes `state_schema` between versions: adds a field, renames it, changes the structure of nested objects. Existing incarnations in the database store state according to the **old scheme** - you need to migrate.

### `state_schema_version` and directory `migrations/`

In service.yml:
```yaml
state_schema_version: 2
```

In the service repo:
```
migrations/
├── 001_to_002.yml
└── 002_to_003.yml
```

In the incarnation table there is a field `state_schema_version`, fixed upon creation.

### DSL migrations

Full regulatory specification - [`docs/migrations.md`](migrations.md), solution commit - [ADR-019](#adr-019-state_schema-migration-dsl).

MVP grammar: flat (`rename`/`set`/`delete`/`move`) + CEL expressions in `set:` values via `${ … }` marker + structural `foreach` for iteration over collections. Conditional `if:` key - deferred until the first real request (extension without breaking change). Complex scenarios not covered by the grammar are not supported in MVP (escape module `state.migrate` was rejected by [ADR-019(e)](#adr-019-state_schema-migration-dsl) as not from the dictionary; candidate `core.incarnation.state-migrate` - if necessary, a separate ADR).

Example (the same `001_to_002`, rewritten for MVP grammar):

```yaml
# migrations/001_to_002.yml
from_version: 1
to_version: 2
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

Execution context of migration-CEL: `state.*` (mutable) and `<as-name>` inside `foreach.do[*]` are available. Prohibited: `vault(...)`, `now()`, `register.*`, `soulprint.*`, `essence.*`, `input.*` - migration = pure function from the old state, side-effect-free.

### Upgrade - an explicit operator step through the UI

Migration **does not run automatically when applying script**. This is an explicit operator operation:

```
keeper.incarnation.upgrade name=X to_version=v2.0
```

What happens in the UI/CLI:
1. Service is connected to master, visible in the UI with known versions (master sees git tags).
2. incarnation displays the current `service_version` and available updates.
3. The operator selects `v2.0`, clicks "upgrade".
4. Keeper in one transaction: runs migrations `001_to_002`, `002_to_003` in a chain, updates `state_schema_version` and `service_version`, writes a snapshot to `state_history` with the mark `migration`.
5. If the migration fails - `status: migration_failed`, no changes in state are saved, incarnation is locked, the operator investigates.

After a successful upgrade, the scripts continue to work with the new schema. The old one remains as a legacy snapshot in `state_history`.

### Compatibility scenarios

The script is tied to the **service version** under which it is launched. After upgrade incarnation to a new service-version, old scripts are no longer called - the operator works with new ones. This is intentional: mixing scripts of different versions is the way to drift.

## Cloud integration via `keeper.cloud`

Dynamic VM creation is implemented as a **cloud-create-script step** with `on: keeper` via the CloudDriver plugin. Service does not know the specifics of clouds - it knows "the step of creating a VM with parameters is needed," Keeper selects a driver and executes it.

Git/DB boundary: **Provider** (configured cloud account) and **Profile** (reusable VM template) live in Postgres, controlled via API/MCP - this is a runtime config, not code. The Default-essence of a service in git acts as a substrate, the operator is overridden in the spec incarnation. The profile parameters are validated against `profile_schema`, which CloudDriver publishes via RPC `Schema()`.

Destroy operations are protected by a mandatory set of guard-rails (tombstone period with `tombstone_ttl`, confirm-flag, storage protection, audit) - one typo in `count` should not erase the rest.

Full breakdown of Provider/Profile, script step `core.cloud.provisioned`, coven binding and destroy security - in [`docs/keeper/cloud.md`](keeper/cloud.md). The `CloudDriver` contract and plugin directory are in [`docs/keeper/plugins.md`](keeper/plugins.md).

## Reaper

Background task inside `keeper`, cleaning the database from garbage and maintaining registry invariants. Not a separate binary. Works only on one Keeper instance at a time - the leader is selected via Redis-lease. Valid **only on Postgres**: it does not go to hosts via SSH, host-based module cache cleaning is described separately in [`docs/soul/modules.md`](soul/modules.md).

If the scope in the future grows beyond the scope of the cleanup (table migrations, transfer of archive records, GC of cold data) - reserve the name **Charon** for a broader process; so far there is only one name - **Reaper**.

Properties, rules (`expire_pending_seeds`, `purge_used_tokens`, `purge_souls`, `purge_old_seeds`, `mark_disconnected`), complete YAML config and metrics - in [`docs/keeper/reaper.md`](keeper/reaper.md).

## Delivery of SoulSeed token to the host

"The operator generated a bootstrap token → the token ended up in a file on the VM where `soul` will start" - this is the operational step between issuing the token and starting Soul. In itself, it is not dictated by Soul Stack: the method of physical delivery is the choice of the operator. The target path is through `keeper.push` (unified audit, RBAC, logs in Keeper); ansible-role, SSH/SCP, CI/CD pipeline and cloud-init are allowed as alternatives.

The protections that ensure the security of a token for any delivery are short TTL (24h by default), one-time use (burning on the first CSR), binding to a specific SID. For additional protections beyond this, see the open question "SoulSeed Token Leakage" in [Open Questions](#current).

Full list of delivery scenarios, permission requirements and file mode, recommendation for systemd `LoadCredential=` - in [`docs/soul/onboarding.md`](soul/onboarding.md).

## End-to-end installation script

Reference path from empty infrastructure to managed Souls. Each step is linked to a section where it is described in detail.

1. **Postgres and Redis are raised** - external dependencies of the Keeper cluster (see ADR-005, ADR-006).
2. **A Keeper cluster is being raised** - one or more `keeper` instances on top of a common PG+Redis. The KID of each instance is fixed in the config.
3. **Bootstrap of the first Archon** - the operator launches `keeper init --archon=<aid>` on the Keeper host. The command under PG advisory lock checks that the registry `operators` is empty, creates the first Archon with the role `cluster-admin` (`permissions: ["*"]`), issues a JWT token (TTL 30 days) and puts it in the file `mode 0400`. Before this step, Keeper refuses to start with operators=empty without `--initialize`. See [ADR-013](#adr-013-bootstrap-of-the-first-archon) and [ADR-014](#adr-014-operator-identity-model-archon).
4. **The Archon writes out the remaining operators** - through OpenAPI/MCP with `Authorization: Bearer <jwt>`, ordinary Archons with limited rights under Covens are created (FK `created_by_aid` in the registry `operators`).
5. **Operator adds Soul** - via OpenAPI/MCP `keeper`: specifies SID (FQDN), desired `transport` (`agent` or `ssh`), Coven tags. The entry in `souls` appears in status `pending`.
6. **For `transport: agent`:** the operator receives a short **bootstrap token** (not a certificate, not a key - only a one-time use token). Delivers to the VM along with the `soul` binary - the target path via `keeper.push` (the same SSH mechanism as for agentless-Destiny, but with the "deploy pull agent" task); alternative paths are Ansible-role, cloud-init, regular SSH (see "Delivering a SoulSeed token to the host").
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

1. The operator writes Destiny/Essence in git, runs `soul-lint` locally and in CI (render → schema validation → static analysis). Without the green `soul-lint` nothing goes outside.
2. Destiny reaches Keeper via OpenAPI or MCP. Keeper checks RBAC, re-renders and validates, puts it in the Destiny (Postgres) registry.
3. **Pull:** Keeper pushes the command "apply such and such Destiny with such and such Essence" to the corresponding Souls (agent transport) on the live gRPC stream.
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
| Vault | Keeper (full: Essence, CA for SoulSeed, SSH provider); Soul (short-lived token client only) | Soul should not have the right to read other people's Essence. |
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
8. **Local admin endpoint on Soul.** **Delayed post-MVP.** In MVP admin operations on Soul-host: `SIGHUP` for hot-reload `soul.yml`, local shell access to logs / `journalctl` / `metrics`-listener for observability, centralized rollout config via CI / Ansible / SSH. Local HTTP/MCP listener on Soul (status / force-resync / dump Soulprint without Keeper / API-driven config mutation) - if really necessary (separate ADR, propose-and-wait for transport: HTTP vs Unix socket vs FromKeeper command).
10. **LB-1. Balancing by SID/Coven labels.** Is L4-LB sufficient (any Keeper will serve any Soul, Coven - only in application logic), or is L7-aware LB / Keeper's own routing-prefix needed. Deferred by user decision.
11. **Leakage of SoulSeed tokens before use.** Additional protections beyond TTL+disposability (binding to IP/CIDR, requirement of cloud-metadata proof, manual approval). Put aside in the ideas box.
12. **Mechanics of checks `last_seen_at`.** Now updated for any message on the stream; explicit keepalive pings are deferred. If this is not enough to accurately determine "alive/not alive," we select an explicit ping mechanism.
13. ~~**List of cloud providers for MVP.**~~ **Closed [ADR-017](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read) amendment (2026-05-25, implemented by 2026-05-26):** first set - **6 official** providers (AWS / GCP / Azure / Yandex Cloud / Proxmox / OpenStack; binaries `soul-cloud-{aws,gcp,azure,yc,proxmox,openstack}`); **vSphere** - community / deferred. AWS - pilot (reference). By 2026-05-26, all 6 are committed and working: AWS (`ec83487`+`72ff14b`), GCP (`6bfd83c`), YC (`a181d49`), Azure (`c7d93ad`), OpenStack (`a03a404`), Proxmox (`af43665`) - see [ADR-017 amendment 2026-05-26 (j)/(k)/(l)](#adr-017-keeper-side-core-modules-expanded-corecloudprovisioned-corevaultkv-read).
14. **Bootstrap the soul agent on the new VM.** **The recommended path is cloud-init userdata** ([ADR-017](adr/0017-keeper-side-core.md) amendment (h)): `CreateRequest.userdata` carries a blob that deploys soul on the new VM; per-VM-token-in-userdata is postponed (chicken-egg: SID is assigned after create), in MVP - general batch-blob + onboarding via CSR. Alternatives (`keeper.push` after create / gold image with soul in AMI) remain possible. Open: formalization of path selection for specific scenarios.
15. ~~**Drift detection.**~~ **Closed [ADR-031](#adr-031-scry--drift-detection-declarative-dry-run-reconcile) (Scry):** declarative drift = "dry-run reconcile would show `changed=true` on at least one declared resource" (like Salt `test=True`/Ansible `--check`, NOT a full host dump). Mechanism - **Scry** subsystem (read-only): MVP on-demand (`keeper.incarnation.check-drift`) → background scan (Reaper-like leader). `Plan` → pure-read with machine `changed`; only-add `PlanEvent.changed` + `ApplyRequest.dry_run`; status **`drift`** informational (remediation = normal apply); report **`DriftReport`**; community module without read-safe → default-deny. The design has been accepted, implementation is in progress (on-demand pilot).
16. **Recovery scripts.** Service can declare a special recovery script for typical cases of partial failure (`error_locked`). Format, obligatory nature, naming convention.
17. **Reconcile-loop for cloud-incarnation.** Background process "declared count vs actual VM count" - install immediately or MVP only manual `incarnation.upgrade/scale`.
18. ~~**DSL migrations state_schema.**~~ **Closed ADR-019:** flat (`rename`/`set`/`delete`/`move`) + CEL value expressions + structured `foreach` (MVP). Conditional `if:` - deferred until the first real request (extension without breaking change). Forward-only, no escape module is entered. Full spec - [`docs/migrations.md`](migrations.md).
19. ~~**`state_history` retention.**~~ **Solved (2026-05-25):** **soft-delete / archiving by config, NOT hard-delete.** Store the last **N=50** snapshots on incarnation + **ALWAYS** snapshot for every `state_schema`-version-bump (migrations remain recoverable). "Excess" images are **archived** (archive table or `archived_at` flag - config-knob), not physically erased (forensic > GC). Cleans/archives **Reaper rule**. Implementation - Track 4 ([roadmap.md](roadmap.md)), not yet done.
20. ~~**UI Keeper.**~~ **Resolved (2026-05-25): UI is a separate artifact (SPA), NOT embedded in `keeper`.** Stack - React + TypeScript + Vite + TanStack Query + React Hook Form + Zod + lucide (mirror of internal reference salt-manager). Design-system - **adaptation of `saltgui-design-system`** (CSS tokens/components, re-skin for the Soul Stack brand/dictionary). TS client + Zod types **generated from [`docs/keeper/openapi.yaml`](keeper/openapi.yaml)** (not written by hand). All UI vocabulary is strictly according to [naming-rules.md](naming-rules.md) (Keeper / Souls / Coven / Soulprint / Destiny / Archon, NOT SaltStack minion/grain/pillar). Start of development - when the API reaches almost complete readiness. Plan - [roadmap.md](roadmap.md).
21. **`host_schema` in `service.yml` (a collection of ideas).** Declaration of the expected host topology for Incarnation: list of roles (`required_roles`) and restrictions on the number of hosts in each role (`role_constraints.<role>.count: { eq | gte | lte }`), possibly extendable to Soulprint filters (minimum CPU/RAM/OS). What would result: an early failure when creating an Incarnation with the wrong number of roles (before cloud-create), checking the declared topology (`incarnation.spec.hosts[].role`, [ADR-008](#adr-008-coven---stable-logical-tags-only)) with the declared scheme before running, hints in the UI/MCP. Postponed until real scenarios show which checks pay off - without them the field will turn into decorative. NB: step targeting - `on:`/`where:` ([`docs/scenario/orchestration.md §4`](scenario/orchestration.md), [§4.1](scenario/orchestration.md)), role **not** Coven ([ADR-008](adr/0008-coven-stable-tags.md)); static check of `on:`/`where:` literals - backlog `soul-lint` ([soul-lint.md → B2](soul-lint.md#b2-statistical-check-of-where--and-on-literals)), not part of this clause.
22. **`soulprint.collectors` in `soul.yml` (a collection of ideas).** Explicit declaration of a set of Soulprint collectors in the agent config: built-in groups (`core` - os/kernel/network/memory/cpu/hostname, `systemd` - units) and a directory of custom detectors (`custom_dir: /etc/soul/soulprint.d/`). Now only `refresh_interval` remains in `soul.yml`; the actual set of facts is internal default. Related to open question 6 (general layout of Soulprint and extensions), but deals specifically with the form of the config on the host: is a toggle switch needed at all, or are collectors always enabled and custom detectors picked up from a fixed path by convention.
23. ~~**Event-driven circuit (Salt beacons / engines equivalent).**~~ **Closed [ADR-030](#adr-030-vigil--oracle---event-driven-monitoring-beacons--reactor):** beacons-circuit introduced by entities **Vigil** (Soul-side check, read-only) → **Portent** (event, only-add `EventStream`-oneof) → **Oracle** (Keeper reactor router) → **Decree** (reactor rule, default-deny, action = scenario-only via work-queue [ADR-027](#adr-027-execution-model-apply---work-queue--claim-acolyte-pool-ward-claim)). The uncommitted clause `SoulBeacon` has been replaced with final names; community checks - via plugin-kind `soul_beacon` (S5). The Engines equivalent (long-running on the host/Keeper) is **not** introduced by this ADR - it remains deferred.
24. ~~**Per-task granularity `serial:`.**~~ **Closed [ADR-056](#adr-056-staged-render---running-the-script-as-n-ordered-passages) (staged-render).** MVP was per-RUN min-width (wave width one per run = minimum positive `serial:`-width among tasks, [`docs/scenario/orchestration.md §2.2.1`](scenario/orchestration.md)), because per-task dispatch (multiple `ApplyRequest` / `apply_runs` lines per host) is not implemented. ADR-056 introduces **Passage** - run as N ordered stages (render→dispatch→barrier→register) along the task axis: per-task dispatch is now implemented as N `ApplyRequest` per host by Passage (amend [ADR-012](#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) dispatch-model + only-add `passage` + PG-PK per-passage). The main driver of the ADR-056 is the implementation of probe→where (`register` in `where:`/`apply:input` of the following tasks), which the canon orchestration.md §4/§5 already promises; per-task `serial:` is a consequence of the same staged model.
25. **Per-host `RenderedTask` dispatch (host-variable flow-control predicates on multi-host).** **render_context.self - closed by Option A** (per-host render context, `RenderContextBySID`): per-host `.self` now resolves for `templates/*.tmpl` and `core.file.rendered` in both branches dispatcher (inline tasks and Acolyte pool) - each host receives its own `soulprint.self.*` in the render file. **Remains open** (full Option B - per-host dispatch of the entire `RenderedTask`): host-variable `params:`, flow-control predicates (`when:`/`changed_when:`/`failed_when:`) and `loop:` with a link to `soulprint.self` on the multi-host target. Pilot keeps these cases under fail-closed guard: they are only allowed with a single-host target; on multi-host, the render crashes with an obvious error ([`docs/templating.md §4`](templating.md)) - guard catches an unsupported case with an obvious error, not silent-wrong. **Full Option B deferred** (decision 2026-06-29: Option A is sufficient). Will require a separate ADR (removing host invariance `params:` + changing the Keeper↔Soul dispatch boundary). **Resume condition** is the first service that needs `${ soulprint.self.* }` in module-`params:` OR `when: soulprint.self.*` on a multi-host target WITHOUT the ability to put self-variability in `.tmpl`. Until then, Option A (per-host `.self` for render files + fail-closed guards for the rest) is sufficient.
26. **Audit-log scaling for bulk runs (100k+ hosts).** Currently `audit_log` is a PG table with INSERT per-event. One Tide run on a 100k VM with wave=1000 gives ~200k records (`tide.started` ×1 + `surge_started`/`surge_completed` ×100 each + per-apply_run `apply.dispatched`/`apply.completed` ×100k×2). Bottlenecks: INSERT-throughput PG (~5-10k INSERT/s), table size (~50MB/run → 250GB/year at 100 runs/week), full-scan reads for UI without partitioning. **Decision postponed to the backlog of future releases** (user decision 2026-05-27). Possible options for consideration: (A) semantic filter "no per-SID events if there is a Tide-aggregate" (-99% volume, source of truth - `apply_runs` table); (B) partitioning by month + retention 90/365 days; (C) hot/cold split (PG for UI, S3/parquet for long-term); (D) batched INSERT; (E) async sink via Redis Stream → separate writer (parity SaltStack returners). Up to 10k VM - the current audit works without optimization; During pilot runs, evaluate the actual load and choose an option.
27. **UI i18n - runtime-discoverable list of languages (`manifest.json`).** UI (companion-repo `soul-stack-web`) uses hybrid lazy-load: default language `ru` bundled inline, the rest (`en`+) - static `public/locales/<lang>/<ns>.json`, fetched via `i18next-http-backend` (not Keeper - these are static SPA assets, they go with the static front host). The list of available languages is now **hardcoded** in `SUPPORTED_LANGS` (TS-literal) - gives type safety `type Lang` + build-time completeness validation via ns-key-sync test. **User decision 2026-05-27: leave hardcoded, change with releases.** Idea for the future (piggy bank): add a list of languages to `/locales/manifest.json`, read by the tag in runtime → adding a language without rebuilding JS, even for the switch (the translation command drops the folder + line in manifest). Trade-off: +1 network request at start, loss of TS literal type `Lang`, no build-time guarantee of completeness of translations (broken language is caught only by the user in runtime). Entering in real translation-workflow / 3+ community languages is a non-breaking additive.

Each of these items will either become a new ADR here, or (if they grow) into their own document in [docs/](.).
