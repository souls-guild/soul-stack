# Changelog

Format — [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Artifact versioning — via git ref ([ADR-007](docs/adr/0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field)), there is no separate `version:` field on Service/Destiny/Module.

## [Unreleased]

### Added

- **Recorded console sessions can be read back**
  ([ADR-0074](docs/adr/0074-interactive-console-pty.md), amendment 2026-07-28).
  Recording became mandatory in the previous change; the artifact was reachable
  only by hand-written SQL. Three read routes now serve it:
  `GET /v1/console/recordings` (paged, filters by host, Archon, kind and time
  window), `GET /v1/console/recordings/{id}` for metadata, and
  `GET /v1/console/recordings/{id}/cast` for the asciicast v2 file itself —
  `application/x-asciicast`, streamed rather than buffered, and replayable with
  `asciinema play`, `agg` or xterm.js.

  **The right is `soul.console`, with the same selectors, and no lighter
  auditor-grade right is minted.** A recording is what an operator typed into a
  root shell and what it printed back — the live session moved in time — so the
  boundary that decides who may watch one has to be the boundary that decided
  who may open one. Otherwise an operator refused `soul.console on host=db-01`
  could read every session anyone ever held on db-01, which is most of what the
  refusal was for.

  Consequence, stated rather than hidden: a **pure auditor** — someone who
  should read the trail without ever holding a shell — cannot be given playback
  without also being given consoles. Withhold it by narrowing the *scope* to the
  hosts they investigate, not by looking for a weaker right; minting one is a
  new entry in a closed catalog and is left to its own decision.

  The gate splits the way the console's own does. A listing names no host, so
  the route can only ask "may this Archon reach consoles at all" and the
  per-host boundary applies to the rows — pushed into SQL for the list, checked
  per object otherwise. **An out-of-scope read answers 404**, identical to an
  unknown id: a 403 would confirm the recording exists and turn the route into
  an oracle for which hosts have been consoled into and by whom.

  Nothing is masked or un-masked on the way out. Masking ran once, when the
  session was recorded, carried across chunk boundaries; the cast is served
  byte-for-byte. Fetching one writes **`console.recording-read`** before the
  first byte leaves, carrying the Archon whose session it was — reading your own
  shell back is routine, reading someone else's is the question an investigation
  asks. The list and metadata routes are not audited: they only say a session
  happened, which `console.opened` already said.

  There is deliberately **no MCP tool** for playback — it would give an agent
  bulk access to the raw content of other operators' shells, and an agent cannot
  watch a replay. A recording of a host since removed from the registry stays
  readable by unrestricted operators and by ones scoped to that host; a
  `coven=`-scoped operator loses it with the host's registry row.

- **The `audit:` block now does what it says** ([ADR-022(i)](docs/adr/0022-audit-pipeline.md),
  amendment 2026-07-27). `audit.enabled: false` used to turn nothing off:
  neither it nor `otel_export` had a consumer anywhere in the write path, so the
  config promised control over the audit trail that did not exist.

  `enabled` is now enforced by a single gate over the shared `audit.Writer`
  where that writer is assembled, so all twenty-odd initiators honor it and a new
  one cannot forget to. `otel_export` switches a dual-write that is now actually
  wired (`auditotel` under `auditmulti`, assembled when `otel.enabled` is set —
  with no OTel stack there is nothing to export to). `retention_days`
  materializes the `purge_audit_old` Reaper rule when none is declared, instead
  of being a number nobody reads.

  **Turning the audit off is itself audited.** `audit.disabled` is written
  in-line ahead of the first event it suppresses, and the reload pair is written
  regardless of the toggle — otherwise the config swap that stops the trail would
  be journaled through an already-closed gate and leave no record of who stopped
  it. `audit.enabled` marks the other end of the window. The block stays out of
  the `SettingsStore` overlay by [ADR-0073(j.2)](docs/adr/0073-keeper-runtime-config-pg.md):
  it is a security gate, so it stays a deliberate edit to a host's `keeper.yml`
  rather than an API call. `keeper init` is not gated — bootstrap of the first
  Archon must leave a record whatever the flag says.

  Both fields are `*bool` now: `audit:` with only `retention_days` set used to
  resolve to "audit off" through the Go zero value, which would have silenced the
  pipeline the moment the flag started being honored.

- **MCP `keeper.soul.run-command` — the non-interactive console**
  ([ADR-0074](docs/adr/0074-interactive-console-pty.md), amendment 2026-07-27).
  An agent needs what an operator needs — run this on that host — but cannot use
  a terminal: a pty merges stdout and stderr onto one fd, echoes what was typed
  and reports only the shell's exit status. So the MCP form is request/response:
  one command line in, split channels and an integer `exit_code` out, masked and
  capped at 64 KiB per channel.

  **It needs `soul.console`, not `errand.run`** — dropping the tty removes the
  echo, not the privilege, and the command is still arbitrary and still runs as
  the Soul daemon's user, typically root. The check is the same scope-aware
  `host=<sid>` one the WebSocket console runs per pane. The Errand stack is only
  the transport, with the module pinned to `core.cmd.shell` and deliberately not
  exposed as an argument. Every run writes `console.command` (`{sid, status}`,
  correlated by `errand_id`) — the command line is not in the payload, exactly as
  `console.opened` holds no keystrokes.

  Known gap, unchanged by this and tracked as NIM-197: `core.cmd.shell` and
  `core.exec.run` are on the Errand runner's hardcoded allow-list, so
  `keeper.soul.errand.run`, `POST /v1/souls/{sid}/exec` and a `kind=command`
  Voyage still reach an arbitrary shell under `errand.run` alone. Until those
  choke-points are aligned, a role that must not hand out shells withholds
  `errand.run` as well as `soul.console`.

- **`introduced_in` engine metadata and the compat cross-check** ([ADR-0076](docs/adr/0076-engine-compat-window.md)).
  A `compat:` window is written by hand and can go stale — declaring `min: 0.1.0`
  while using something that only exists from `0.3.0` promises a keeper that would
  break. Keeper now derives the floor a definition **actually** needs from its
  body (DSL grammar plus the module / state / parameter metadata of the core
  catalog) and compares it with the declared one: `soul-lint` reports
  `compat_floor_too_low` on the manifest that declared the window, and keeper logs
  it at render, where it means another instance of a rolling-upgraded cluster
  would fail this same definition. Deliberately not a run-time block — the
  rendering keeper carries the feature, and refusing a run that would succeed is
  the worse failure. Inference yields a floor only; the ceiling stays the author's
  to declare. `introduced_in` parses on module manifests (module, state and
  parameter level) and is published by `GET /v1/modules`; it names a RELEASED
  version, so features are stamped when a release is cut (see
  [RELEASING.md](RELEASING.md) step c2).

- **Param-level strictness on the agent, and a deprecation policy for module
  params** ([ADR-0076](docs/adr/0076-engine-compat-window.md), amendment
  2026-07-26). A Soul reads a task's params by key, so a key it did not know was
  simply never read: an agent older than the param did its old job and reported
  OK/CHANGED while what the author asked for never happened. It now checks the
  task against the manifest **compiled into that binary** before Apply — and
  before `Plan`, so a `dry_run` cannot answer "no drift" for a param it never
  reads — and rejects an undeclared key with `module.unknown_param` without
  running the module. Core modules are enforced; custom modules **report but do
  not fail** — their manifests were never enforced before and under-declare in
  practice, so gating them would break scenarios that work today.

  The counterpart is that a module contract can now shrink safely: a param is
  never removed outright but marked `deprecated: {since, removed_in, use?}`
  (`removed_in` **exclusive**, like `compat.keeper.max`) and honored for **at
  least 2 minor releases** — deprecated in `X.Y.0`, removable no earlier than
  `X.(Y+2).0`, which is exactly the window an author may declare in `compat:`.
  Authors get a `deprecated_param` **warning**, never an error, naming the
  deadline and the replacement; at `removed_in` the same task text becomes
  `unknown_param`.

  **Upgrade order: souls first, then keeper** — adding a param to a core module
  is now a loud per-host failure on agents that predate it, rather than a silent
  mis-apply. Same order the capability gate already requires.

- `soul-stack-tools` — meta package installing the whole authoring-side CLI set
  (`soulctl` + `soul-lint` + `soul-trial`) in one step. Carries no files itself.
  The `keeper` and `soul` daemons stay separate packages on purpose: a server
  installs only what it runs.

### Security

- **`errand.run` no longer reaches an arbitrary shell on its own**
  ([ADR-0074](docs/adr/0074-interactive-console-pty.md), amendment 2026-07-28;
  [ADR-033](docs/adr/0033-errand.md)). `core.cmd.shell` and `core.exec.run` sit on
  the Errand runner's allow-list, and their declared input *is* a command line —
  so the allow-list bounded the module, not what it carried, and the gate written
  for "one named module with declared params" opened a root shell. That made
  `soul.console` (added a day earlier for exactly this action) a half-control:
  anyone holding `errand.run` walked around it in one call.

  Reaching either module through an Errand now requires **`soul.console` in
  addition to `errand.run`**, at every entry point: `POST /v1/souls/{sid}/exec`,
  MCP `keeper.soul.errand.run`, a `kind=command` Voyage (on every resolved host,
  all-or-nothing), a `kind=command` Cadence recipe, and the Cadence spawn.
  `errand.run` stays necessary. The two rights remain independent in both
  directions — `soul.console` alone still does not open the Errand path, and
  `keeper.soul.run-command` still needs no `errand.run`. **An `ErrandReadSafe`
  module is unaffected**: an ordinary Errand does not acquire a console
  requirement. Cancelling a command Voyage is deliberately not gated — the
  emergency brake must not need a stronger right than the accelerator.

  **This breaks existing grants, so it ships behind a one-minor deprecation
  window.** `console.errand_shell_gate` in `keeper.yml` defaults to `warn`: the
  call proceeds and the would-be denial is recorded. It flips to **`enforce` in
  the next minor**. Before flipping, check two numbers —
  `keeper_rbac_shell_errand_legacy_roles` (roles holding `errand.run` with no
  `soul.console`, named in a WARN line when the set changes) and
  `keeper_rbac_shell_errand_gate_total{result="would_deny"}` (the live, scope-exact
  answer, cut by `surface`). Both at zero for a release means the flip cannot
  break you. The key is a `keeper.yml` edit, not a `SettingsStore` one: it is a
  security gate, and that overlay falls back to the more permissive value after a
  Postgres outage ([ADR-0073(j.2)](docs/adr/0073-keeper-runtime-config-pg.md)).

  **Watch schedules in particular.** Every other entry point answers an operator
  reading an HTTP status; a Cadence recipe written under the old rule would simply
  stop producing runs, on a timer, with nobody looking. Under `enforce` the spawn
  skips rather than errors (an error would stall every other due schedule),
  advances `next_run_at` as an overlap skip does, and writes the new
  **`cadence.skipped_forbidden`** audit event `{cadence_id, scheduled_for, reason,
  module}`. Alert on it.

### Changed

- Package renames, dropping a doubled `soul-`: `soul-stack-soul-lint` →
  **`soul-stack-lint`**, `soul-stack-soul-trial` → **`soul-stack-trial`**. The
  binaries (`soul-lint`, `soul-trial`) are unchanged. Both packages declare
  `Provides`/`Replaces`/`Conflicts` on their old names, so `apt`/`dnf` retire the
  old package on upgrade rather than leaving it orphaned.

- `soul-legion` is now a supported artifact ([ADR-004 Amendment
  2026-07-26](docs/adr/0004-binaries.md)), shipped as **`soul-stack-legion`**, so
  operators can size their own clusters instead of trusting the projection table.
  It gained `--version`, and every environment flag (`--keeper-endpoint`, `--ca`,
  `--pg`, `--vault`, `--openapi`) is now **required with no default** — it used to
  fall back to a developer box (`/tmp/keeper-dev` CA, a localhost DSN, Vault token
  `root`), which is not something a released binary may do.

  It is **not** a black-box benchmark: it writes the stub souls' identity straight
  into the cluster (`souls`/`soul_seeds`) and mints their certs from Vault PKI, so
  it needs cluster database credentials and a PKI-issue token. Run it against a
  bench cluster, never production.

### Fixed

- **Tag-guarded tests are compiled by the gate again.** Nothing built the
  `integration` sources on a normal PR, so they rotted out of sight: the Soul
  failback suite still called `reconnectLoop` with 12 arguments after it grew to
  14 (console wiring, then sd_notify), and `keeper/internal/redis` called
  `NewClient` without the password resolver. Both broke at compile, so
  `make test-integration` could not run those packages at all — and the one error
  it printed looked like a build glitch rather than several suites going missing.

  `make check` now runs **`vet-tags`**: `go vet` under `integration` across the
  workspace, plus `e2e` / `e2e_live` / `e2e_k8s` / `smoke` for the sets that live
  behind their own tags. Vet compiles without running anything, so the gate stays
  docker-free and the container suites stay opt-in. The failback behavior itself
  was intact — only the call had drifted — and those tests pass again.

---

## [v0.1.0-beta.1] — 2026-06-15

First tag of the public beta. One git tag = version of all 7 go.work modules ([ADR-011](docs/adr/0011-go-layout.md)); the version is injected into binaries via `-X main.<var>`, printed by `keeper version` / `soul version` / `soulctl version`. Beta distribution is build-from-source ([CONTRIBUTING.md](CONTRIBUTING.md)); the release procedure is [RELEASING.md](RELEASING.md).

### Highlights of the beta

- **OpenAPI code-first pivot** — the OpenAPI 3.1 source of truth is now a huma aggregator in code (`HumaFullSpecYAML`); `docs/keeper/openapi.yaml` is a derived committed snapshot with a `make check-openapi` drift guard ([ADR-054](docs/adr/0054-openapi-code-first.md)).
- **RapiDoc at `/docs`** — built-in OpenAPI spec rendering in Keeper.
- **Retention** — configurable retention of logs/history (audit, state_history).
- **Version injection** — `git describe` → ldflags `-X main.<var>`, a single version across all binaries ([ADR-011](docs/adr/0011-go-layout.md)).
- **Security hardening ahead of beta** — `govulncheck` supply-chain gate in `make check`, Vault least-privilege policy, edge hardening, retention security review.

### Added
- A fully assembled MVP skeleton of the Keeper cluster, Soul agent, soul-lint, and soulctl CLI.
- Tide+Surge subsystems, Beacons/Vigil-Oracle-Decree, Scry drift-detect, Push (Variant C), Errand, Toll detector.
- 17 Soul-side core modules + 3 Keeper-side core modules (cloud/vault/soul-registered).
- 6 CloudDriver plugins (AWS/GCP/Yandex.Cloud/Azure/OpenStack/Proxmox) and SSH providers static/Vault-CA/Teleport.
- E2E harness L3a/L3b/L3c, runbooks under `docs/operations/`, SBOM/deb/rpm packaging, local CI gate `make check`.
- Load-test harness `soul-legion` (`make stress`) — simulates a fleet from 1k to 25k Souls and runs a read load (24 GET endpoints), a write cycle (create→delete), and a Voyage. Confirmed: Keeper holds linear scaling up to 25k+ concurrent streams at ~0.12 MiB per soul (internal scale-verification tool, not part of the runtime).

### Changed
- The source of Soul presence is a Redis lease; the `souls.status` field no longer filters the target resolver (invariant "hot data → Redis, not PG").
- Soulprint in JSONB/template uses a `snake_case` canon (BUG-A).
- `core.pkg`/`core.service` read facts via Soulprint (BUG-B Variant A), apk-version aligned.
- `apply_runs` marks non-matching hosts as `no_match` (FINDING-01).
- Bulk coven-assign got `mode=replace` and `selector.incarnation`; REST↔MCP parity.
- `POST /v1/voyages/preview` got a separate rate limit (`voyage_preview`, 30 per window / burst 60) and no longer shares a limit with Voyage creation (`voyage_create`, 10/20) — frequent preview requests no longer bump into the creation write limit ([ADR-050](docs/adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api) / [ADR-043](docs/adr/0043-voyage.md#adr-043-voyage--unified-batch-run) amendment).

### Fixed
- Large team-scale Voyages (on the order of 10k hosts) no longer hang at finalization. Previously Redis pub/sub subscribed to a separate channel per applyID, and on a large fleet the number of subscriptions ran into the Redis `maxclients` limit — the Voyage never completed. Keeper now does not spin up a cross-keeper bridge for locally connected Souls and uses a fixed set of sharded event channels (`events:shard:<n>`, K=256) instead of a channel-per-applyID. Voyage on 10k hosts: from "never completes" to ~11.6s ([ADR-006](docs/adr/0006-cache-redis.md#adr-006-cache-and-coordination--redis) amendment).
- §26 audit-log scaling to 100k+ VMs — 5 options considered, decision deferred.
- Tide: cancellation, stop-and-wait, SSE `/v1/tides/{id}/progress` (polling GET exists) — post-MVP.
- Vigil inotify: recursive + throttle — P3.
- ADR-030 S5-final `PortentEvent.data` hard-cut — P3 post-1-prod-release.
- Push S3 (compile-time wire) — no use case, revive on request.
- Multi-cluster federation — rejected, horizontal scale of a single cluster covers the requirements.
- Cloud parity Phase 4 (expansion beyond 6 drivers) — user decision deferred to a following release.
- Shepherd — stream balancing on scale-out, waiting on ADR-006 amend + architect design.
- ADR-018 user-collectors `/etc/soul/soulprint.d/*` — separate ADR.

### Known limitations (beta)
Deliberate beta out-of-scope, not bugs (full list — [docs/known-limitations.md](docs/known-limitations.md)):
- **Cloud provisioning** — CloudDriver plugins and `core.cloud.provisioned` are present, but end-to-end cloud CRUD did not make it into the beta (Cloud parity Phase 4 — next release).
- **MCP cadence** — schedule (Cadence) management via MCP is not yet covered; the path is OpenAPI/soulctl.
- **Audit-log scaling to 100k+ VMs** — on a 100k-VM fleet the audit-log PG-INSERT rate will hit a ceiling; 5 scaling options considered, decision deferred (§26 backlog).
- **No immediate JWT revocation** — operator revocation = `revoked_at`, active JWTs work until `exp` (protection is a short TTL), ([ADR-014](docs/adr/0014-operator-identity.md#adr-014-operator-identity-model-archon)).
- **Push — narrow profile** — `keeper.push` (Variant C) is covered at a basic level; S3 compile-time wire has no use case, extensions on request.
- **External pentest — post-beta** — an independent security audit is planned after the beta, before GA.

### Security
- mTLS Keeper↔Soul per [ADR-012](docs/adr/0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add); the Soul private key never leaves the host (CSR onboarding).
- [ADR-026](docs/adr/0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index) Sigil — a Keeper-signed digest index for all plugins, default-deny when unsigned.
- JWT authentication of operators ([ADR-014](docs/adr/0014-operator-identity.md#adr-014-operator-identity-model-archon)), signing key via Vault KV `secret/keeper/jwt-signing-key`.
- RBAC default-deny, multi-coven AND-merge in Tide ([ADR-040](docs/adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override) fail-closed).
- Augur ([ADR-025](docs/adr/0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access)) — a Keeper-side broker for Soul's external access, default-deny.
- Vault least-privilege policy + recovery-enable procedure (`docs/operations/disaster-recovery.md`).
- The migration-CEL sandbox forbids `vault/now/register/soulprint/essence/input` ([ADR-019](docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

---

## Roadmap → Released — feature-complete MVP

This section covers the actual state of the code and documentation as of 2026-05-27.

### Keeper cluster
- Stateless N-Keeper on top of shared PG/Redis ([ADR-002](docs/adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)); SID lease in Redis ([ADR-006](docs/adr/0006-cache-redis.md#adr-006-cache-and-coordination--redis)).
- Bootstrap of the first Archon via `keeper init --archon=<aid>`, AID validation, partial unique index ([ADR-013](docs/adr/0013-bootstrap-archon.md#adr-013-bootstrap-the-first-archon)).
- Identity registry `operators` + JWT auth, FK on all audit fields ([ADR-014](docs/adr/0014-operator-identity.md#adr-014-operator-identity-model-archon)).
- Config hot-reload with write-back YAML ([ADR-021](docs/adr/0021-hot-reload-config.md#adr-021-hot-reload-of-config-with-write-back-yaml)); Toll leader uses `UpdateConfig` + RWMutex snapshot.
- Conclave (registry of live instances) + Watchman (isolation detection + soul-shedding) + refuse-guard `acolytes=0` warn.
- Toll cluster-wide detector of mass Souls attrition ([ADR-038](docs/adr/0038-toll.md#adr-038-toll--a-cluster-wide-detector-of-mass-souls-attrition)) with hot-reload and webhook diff-recycle.
- Tide + Surge — invocation-time scope chunking, AND-merge target, REPLACE concurrency, abort+continue, per-Surge state commit, Acolyte-style lease for failover ([ADR-040](docs/adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)).
- Reaper — leader via Redis lease, cleans up pending/zombie/expired seeds; soft-delete/archive of state_history.

### Souls (agents)
- gRPC bidi `EventStream` over mTLS, `oneof payload`, thematic `.proto` files, forward-compat only-add ([ADR-012](docs/adr/0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)).
- Typed Soulprint ([ADR-018](docs/adr/0018-soulprint-typed.md#adr-018-soulprint-typed-schema-mvp)): `SoulprintFacts` (Os/Kernel/Cpu/Memory/Network), `pkg_mgr`/`init_system` collected on the Soul side.
- State_schema migration DSL ([ADR-019](docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)): flat (`rename`/`set`/`delete`/`move`) + CEL in `set.value` + structural `foreach`; one PG transaction, snapshot per step.
- Soul-reconcile dispatched-orphan ([ADR-027(g)](docs/adr/0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)); WardRoster in proto.
- The same `soul` binary works in pull (daemon) and push (oneshot) — modules apply the same way.

### Scenario / Destiny
- Coven = stable tags only, role is NOT Coven ([ADR-008](docs/adr/0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only)); declared role only in `incarnation.spec.hosts[].role`.
- Full scenario-DSL set ([ADR-009](docs/adr/0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)): `on:`/`where:`/`serial:`/`run_once:`/`apply:`/`state_changes` + two-level resource resolution.
- Templating engine ([ADR-010](docs/adr/0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)): CEL for YAML expressions, Go text/template + sprig allowlist for files, `${ … }` marker, strict mode, secret masking.
- Top-level `output:` in `destiny.yml`, read via `register:` on the applier task.
- `soulprint.hosts` — scenario-only accessor of the run's hosts with stable facts.
- Scry drift detection ([ADR-031](docs/adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): a pure-read Plan contract over 14 core modules, on-demand check-drift + background `scry_background` (default OFF).
- Trial runner `soul-lint trial` ([ADR-023](docs/adr/0023-trial-test-runner.md#adr-023-test-runner-trial-soul-trial-and-dsl-coverage)) with L0/L1/L2 coverage, `assert.state_after`.

### Modules (core MVP)
- 17 Soul-side core ([ADR-015](docs/adr/0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list) + extensions): `pkg`/`file`/`service`/`user`/`group`/`exec`/`cmd`/`cron`/`mount`/`git`/`archive`/`sysctl`/`firewall`/`http`/`line`/`repo`/`url`; `core.file.rendered` is the only render step.
- 3 Keeper-side core ([ADR-017](docs/adr/0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read)): `core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`.
- Errand pull-ad-hoc exec outside a scenario ([ADR-033](docs/adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-outside-a-scenario)) with E1..E5: HTTP handler, cross-keeper routing, Reaper purge, MCP tools, soulctl commands, `?module=` filter.
- Per-module canon doc at `docs/module/core/<name>/README.md`.

### Plugins (SDK Phase 2)
- A single gRPC-stdio handshake for SoulModule/CloudDriver/SshProvider/soul_beacon ([ADR-020](docs/adr/0020-plugin-infrastructure.md#adr-020-plugin-infrastructure-manifest-format-handshake-lifecycle)).
- Sigil ([ADR-026](docs/adr/0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)) — Keeper-signed digest index, soul-side cache.
- Companion repository `soul-stack-plugins/` + template `soul-mod-template`.
- `soul-lint plugin-init` CLI via `go:embed` for scaffolding a new plugin.
- 3 pilot official plugins: `soul-mod-docker-container`, `soul-mod-nginx-vhost`, `soul-mod-postgres-user` (namespace `official`).

### Beacons / Vigil-Oracle-Decree
- Event-driven monitoring ([ADR-030](docs/adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor)): an edge-triggered check + reactor contour.
- Vigil scheduler on Soul + a 4th plugin kind `soul_beacon` (`port_closed`/`disk_full`/`process_absent`/`http_unhealthy`/`service_down`/`file_changed`).
- Oracle Portent-reactor on Keeper: Vigil/Decree CRUD registries, OpenAPI+MCP, RBAC, audit, scenario-enqueue.
- Circuit-breaker auto-disable of Decree, Oracle+beacon metrics.
- Typed `PortentPayload` (parity with ADR-018 Soulprint), inotify P0/P1, `action=scenario-only`.

### Push (Variant C)
- Multi-host destiny push without incarnation/scenario ([ADR-032](docs/adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-without-incarnationscenario)).
- S0..S7: SSH transport (CA-signed host certs), Deliverer (SHA-256 cache), Cleaner, SshDispatcher, orchestrator, multi-CA, `souls.ssh_target` jsonb, `push_providers` PG table, auto-import legacy, multi-keeper bootstrap in kind, multi-provider routing.
- SSH providers: `soul-ssh-static`, `soul-ssh-vault` (variant B, ephemeral SSH CA), `soul-ssh-teleport` (proxy_jump via bastion).

### Cloud
- 6 CloudDriver plugins: `soul-cloud-aws`/`gcp`/`yc`/`azure`/`openstack`/`proxmox`.
- `core.cloud.provisioned` (`created`/`destroyed`) — keeper-side ([ADR-017](docs/adr/0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read)).
- Cloud-init bootstrap MVP (`keeper/internal/cloudinit/`).
- Credentials flow A + Status/List only-add API.

### Work-queue / Tide
- Acolyte/Ward/Summons work queue ([ADR-027](docs/adr/0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)): PG claim, retry/backoff, recovery-reclaim, S6 Soul-reconcile dispatched-orphan.
- Tide PG schema 055, claim loop, FinalizeWithOwnership, real surge loop, spawn-await, decision-gate ([ADR-040](docs/adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)).
- Audit canon: handler emit, `souls_in_surge`; HTTP/MCP/soulctl REST↔MCP parity.

### Observability
- Prometheus-primary + OTel bridge ([ADR-024](docs/adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)).
- Oracle/beacon/Toll/Acolyte/Ward metrics + `soul /metrics` basic-auth (`password_file`).
- OTel collector + Jaeger in the dev environment.
- Audit pipeline ([ADR-022](docs/adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)): PG + multi-sink + OTel export, GET `/v1/audit`.

### Security
- mTLS Keeper↔Soul, JWT operators, Vault integration (Soul-safe client in `shared/vault`, server-side in `keeper/internal/vault`).
- Sigil plugin trust (default-deny, Keeper-signed digest).
- RBAC default-deny, per-Coven/service scope for incarnation, multi-coven AND fail-closed.
- Augur — a Keeper-side broker for Soul's external access.
- archive zip-slip protection, edge hardening for the AWS pilot, refuse-guard multi-keeper.

### Documentation
- [docs/architecture.md](docs/architecture.md) — 40 ADRs (ADR-001…040, excluding ADR-034/036/037).
- [docs/scenario/](docs/scenario/README.md) — concept/orchestration/tide-state-commit.
- [docs/keeper/](docs/keeper/README.md) — modules/rbac/storage/push/reaper/augur/cloud/mcp-tools/openapi/prod-setup.
- [docs/module/core/<name>/README.md](docs/module/) — per-module documentation (mandatory canon).
- [docs/operations/](docs/operations/README.md) — bootstrap-rbac/deployment/disaster-recovery/scaling/upgrade/monitoring/faq/recovery-reclaim-apply-runs.
- [docs/soul/](docs/soul/README.md) — concept/connection/identity/onboarding/soulprint/modules/config.
- [docs/testing/e2e.md](docs/testing/e2e.md) — L1/L2/L3a/L3b/L3c levels ([ADR-039](docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity)).
- [docs/templating.md](docs/templating.md), [docs/migrations.md](docs/migrations.md), [docs/observability.md](docs/observability.md), [docs/soul-lint.md](docs/soul-lint.md), [docs/roadmap.md](docs/roadmap.md), [docs/ideas.md](docs/ideas.md).
