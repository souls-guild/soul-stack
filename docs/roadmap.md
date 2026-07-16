# Soul Stack — Roadmap (MVP → Prod)

A living plan for the transition from MVP to a full-fledged product solution. Statuses: ✓ ready · 🟡 in progress · ⬜ not started · 💤 Ideas box ([ideas.md](ideas.md)).

> PM updates as we go. Decisions are recorded in ADR ([architecture.md](architecture.md)); here is a plan and status, not a normative source.

> ⚠️ **This file is a historical roadmap, frozen at 2026-05-26.** **`v0.1.0-beta.1`** has since been released (closed beta, private repos `souls-guild`). Some of the statuses below are outdated relative to the actual state: for example, "Cloud parity ✓ ready" means implemented CloudDriver plugins, but **operator-flow cloud-provisioning is not operationally available in beta** (no REST/MCP/UI handles `/v1/providers` / `/v1/profiles`); "push 🟡 in progress" is actually closed as part of the Voyage epic. The current boundaries of beta and GA-readiness are NOT here, but in:
> - [known-limitations.md](known-limitations.md) - what is NOT included in closed beta (source of truth for beta limits).
> - [prod-readiness.md](prod-readiness.md) - GA-gap roadmap (P0/P1/P2 blockers based on audit code results; source of truth based on GA boundaries).
>
> This file is saved as a history of decisions made and tracking MVP→prod; new statuses are not written here.

## Status (2026-05-26, historical)
- MVP feature-complete, release gate closed, HA validated at scale (3 keeper + 9-node real redis-cluster, 0 kernel bugs).
- Code on GitHub - **temporary** repo `co-cy/soul-stack` (will move; module-path and real signature are finalized on permanent repo). Commits to `main`, CI = `make check`.
- **Closed by 2026-05-26:** 6 cloud providers (AWS pilot + GCP/YC/Azure batch-1 + OpenStack/Proxmox batch-2); 3 SshProvider plugins (static / Vault SSH CA / Teleport); drift Slice A+B (Plan pure-read for 17 core modules: 12 stateful covered, 2 deferred Slice C, 3 verbs not-applicable); release-ops completely.
- **Working now:** dispatcher `proxy_jump` support (Teleport via bastion); push S4 (api/mcp facade).

## Committed session decisions
| Topic | Solution |
|---|---|
| env-RBAC environment | hybrid C+D, column `incarnation.covens` (declared env tags + name-as-coven) |
| Cloud | 6 providers, credentials **Option A** (Keeper resolves KV-secret), bootstrap **cloud-init**, pilot **AWS** |
| drift-detection | name **Scry**, **on-demand pilot → background**, `Plan` pure-read, status `drift` informational, report `DriftReport` |
| Shepherd (balancing) | 💤 Ideas box (possibly future releases) |
| UI | **separate artifact** (B), React+TS+Vite, adapt saltgui design-system, TS client from OpenAPI, Soul Stack dictionary; build when the API is almost ready |
| SSH/push | 3 providers (static/Vault SSH CA/Teleport), pilot **static-key** |
| SSH credentials-flow | **Variant B** for Vault SSH CA (the plugin itself in Vault via `vault_access`; `ssh/sign` is an operation, not KV-read; deliberately diverges from cloud-A) ([ADR-020 amendment (j)](adr/0020-plugin-infrastructure.md)) |
| SSH key-ownership | **Keeper-ephemeral** for CA providers (Keeper generates ephemeral keypair per-session, sends only pubkey; private does not leave Keeper; security-first) ([ADR-020 amendment (k)](adr/0020-plugin-infrastructure.md)) |
| SSH params-delivery | env-convention per-plugin(`SOUL_SSH_STATIC_PARAMS` / `SOUL_SSH_VAULT_PARAMS` / `SOUL_SSH_TELEPORT_PARAMS`); generic mechanism deferred post-MVP ([ADR-020 amendment (l)](adr/0020-plugin-infrastructure.md)) |
| state_history retention | **soft-delete / archive according to the config** (not hard-delete); last 50 + always snapshot on version-bump |
| CLI | thin wrapper on top of OpenAPI, later |
| module-registry | discuss; default PG `bytea` |
| archive zip-slip | in-process unpacking from `securejoin` - **closed** (`soul/internal/coremod/archive/`) |
| recovery scripts | deferred until actual request |

---

## Track 0 — Release-ops (T1, path to the first product release)
- ✓ CI (GitHub Actions = `make check` + integration + govulncheck)
- ✓ push code to GitHub
- ✓ Runbook: deploy · backup/restore PG · Vault-setup · scaling Keeper · upgrade procedure
- ✓ govulncheck in `make check`
- ⬜ Branch-protection + required-check (operator in GitHub UI) - on a permanent repo
- ⬜ **On a persistent repo:** finalizing module-path (sed-rename) + real signing key (currently cosign-stub)

## Track 1 - Operator Ergonomics (T2)
- ✓ MCP-tool `keeper.soul.coven-assign`
- ✓ env-RBAC C+D: `service=`/`coven=` in RBAC-context incarnation (OR-Check `RequirePermissionMulti`) + column `incarnation.covens` (migration 046); REST↔MCP parity (7 incarnation MCP-tools - scoped OR-Check, fail-closed) - committed
- ✓ MCP parity of incarnation routes (REST↔MCP scoped OR-Check)
- ✓ bulk-API + follow-ups (`replace`-set + `incarnation=`-selector)
- ⬜ Directory of coven tags (real `CovenLabelValidator`) - closes Q1b
- ⬜ Operator CLI (thin wrapper)
- ⬜ UI (separate SPA, saltgui adaptation) - start when the API is almost ready

## Track 2 - HA / scale
- 💤 Shepherd - balancing with scale-out ([ideas.md](ideas.md))
- ✓ **Batch run - Voyage** ([ADR-043](adr/0043-voyage.md), `kind=scenario` - batch of N incarnations for Legs; `kind=command` - multi-target ad-hoc exec). Absorbed and replaced Tide ([ADR-040](adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) and ErrandRun ([ADR-041](adr/0041-errandrun.md)) - both entities were removed in implementation (Wave 5, migrations 061/062), ADR-040/041 were left as superseded history. Parameters: `batch_size` / `concurrency` / `schedule_at` / `inter_batch_interval` / `on_failure`; registry `voyages` + `voyage_targets` (migration `059`); endpoint `/v1/voyages`; failover via Acolyte-style PG-lease.
- ⬜ advanced: per-task `serial:` (Q24) · per-host `RenderedTask` (Q25) · L7-LB (Q10) — backlog

## Track 3 — Cloud parity (T2)
- ✓ **6 cloud providers implemented.** AWS pilot (`ec83487`) + edge-hardening (`72ff14b`); GCP (`6bfd83c`), Yandex Cloud (`a181d49`), Azure (`c7d93ad`) - batch 1 hyperscaler-like; OpenStack (`a03a404`), Proxmox (`af43665`) - batch-2 divergent (Keystone-auth / clone-model+composite vm_id). Shared SDK framework (`sdk/clouddriver/`) was reused by everyone without edits. credentials-flow Variant A - worked for all 6. [ADR-017 amendment 2026-05-26](adr/0017-keeper-side-core.md).
- ⬜ reconcile-loop (Q17) - after parity
- ⬜ bootstrap-on-VM: cloud-init userdata in main way (Q14)

## Track 4 - Operations / Reliability (T2)
- ✓ **drift-detection (Scry):** Slice A (Plan pure-read + only-add proto + status `drift` + `DriftReport` + on-demand pilot) + Slice B (`scenario/converge/`, `keeper.incarnation.check-drift` REST+MCP, RBAC `incarnation.check-drift`, audit `incarnation.drift_checked`) — ✓ committed. Plan pure-read circulation for 14 remaining core modules (`c2d8181`): **9 modules with `PlanReadSafe`** (group/user/cron/mount/sysctl/archive/line/repo/firewall) — ✓; **2 modules WITHOUT `PlanReadSafe`** (git, url without checksum) - deferred in Slice C; **3 verb modules WITHOUT `PlanReadSafe`** (exec/cmd/http) - correct default-deny. Total 12 stateful are covered, 5 are not covered (2 deferred, 3 verbs n/a). ✓ **Slice C** (background-scan, default OFF guard-rail — `reaper.scry_background.enabled=false`; Reaper-rule `scry_background` + migration of 050 columns `last_drift_check_at`/`last_drift_summary`; throttle via `CountActiveDryRuns()` max_concurrent=10; audit-source `SourceBackground`) - implemented, [ADR-031 amendment 2026-05-26](adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile).
- 🟡 **keeper.push S1-S5 + 3 SshProvider:** **3 SshProvider implemented** - static-key (`4f95ef6`, reference), Vault SSH CA (`3642520`, ephemeral keypair), Teleport (`af27678`, `SignReply.proxy_jump` only-add field 4). MVP solutions are fixed ([ADR-020 amendment 2026-05-26](adr/0020-plugin-infrastructure.md)): credentials Variant B + key-ownership Keeper-ephemeral + params env-convention. ✓ **S1** delivery of soul+modules (SHA-256); ✓ **S2** dispatcher ephemeral-key; 🟡 **S3** alt-Outbound in scenario-runner / dispatcher `proxy_jump` support (Teleport-via-bastion - in progress); 🟡 **S4** OpenAPI/MCP/RBAC/audit facade - Variant C orchestrator committed 2026-05-26 ([ADR-032](adr/0032-push-orchestrator.md)); ✓ **S5** cleanup + live-sshd test.
- ✓ state_history retention: soft-delete/archive by config (Reaper rule)
- ✓ **Recovery - `reclaim_apply_runs` runbook + GATE-1 production-gate (2026-05-26).** Operationalization of GATE-1 ([ADR-027](adr/0027-apply-work-queue.md) amend), not a new ADR. Runbook [`docs/operations/recovery-reclaim-apply-runs.md`](operations/recovery-reclaim-apply-runs.md) - three product gates (fencing-Soul for the fleet + `acolytes > 0` on all Keeper instances + Soul-reconcile S6 for `dispatched` orphans), hot-reload `enabled: true`, validation according to `keeper_reaper_rule_purged_total{rule="reclaim_apply_runs"}` / `keeper_runresult_stale_total` / audit `reaper.reclaim_apply_runs.executed` / alert `dispatched stuck`.
- ⬜ module-registry storage (Q5) - discuss; PG `bytea` default
- ⬜ **Toll (cluster-wide outflow-detector)** - [ADR-038](adr/0038-toll.md) fixed 2026-05-26, implementation with a separate slice (per-instance tollwatcher + Redis-leader aggregation + soft-degraded middleware on POST /v1/incarnations/{name}/scenarios/{scenario} and POST /v1/push/apply).
- ✓ **R2 anchors-TTL** - closed without action 2026-05-26 (Outbound is already working through SoulLease `soul:<sid>:lock` with TTL=30s + refresh=10s + correct failover; phantom recording).

## Track 5 — Security hardening (T2)
- ✓ core.archive zip-slip → in-process `securejoin` (fail-fast on resolved path, symlink within-dest, hardlink/devnode reject, setuid/setgid/sticky mask, zip-bomb size/entries/ratio limits)
- ✓ soul-lint statistical check of `where:`/`on:`-literals

---

## Canon-commit (closed batches)
- **PM 2026-05-25:** ✓ UI (Q20 resolve) · ✓ env-RBAC (ADR-008 amend (a) → IMPLEMENTED C+D) · ✓ drift ([ADR-031](adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile) + Scry/DriftReport in naming-rules + Q15) · ✓ cloud (ADR-017 amend + Q13/Q14) · ✓ state-retention (Q19) · ✓ open Q13/14/15/19/20 updates.
- **PM 2026-05-26:** ✓ ADR-020 amendment (SSH providers - 3 user solutions + MVP set) · ✓ ADR-017 amendment (DONE cloud pairing - 6 providers) · ✓ ADR-031 amendment (Plan circulation for 14 core modules) · ✓ naming-rules sync (`provider_kind` enum for cloud/ssh, 3 SshProvider binaries) · ✓ roadmap status update.
- **PM 2026-05-27 (Vigil S5 closure):** ✓ ADR-030 [amendment 2026-05-26](adr/0030-vigil-oracle.md#amendment-2026-05-26-s5-closure) - three open ends are closed by one wave: V5-1 typed PortentPayload (6 typed-message + custom Struct, deprecation `PortentEvent.data` via 1-release hand-off → hard-cut S5-final), V5-2 `soul_beacon` plugin-kind (4th kind plugin-infra, `KIND_SOUL_BEACON=4`, unary `ValidateVigil`+`Check`, Sigil-verify required), V5-3 `core.beacon.inotify` (7th built-in beacon, Linux-only, fold-adapter, `InotifyPortent` field 14, Darwin/Windows stub). ✓ naming-rules sync (`kind`-enum 4 values, `PortentEvent.payload`-oneof, Vigil description - 7 core-beacon + `soul_beacon`-plugin), plugins.md cross-link, beacon README cross-link. Postponed post-S5: (iv) per-Decree cooldown override · (v) toggle-endpoint re-enable Decree · (vi) metric-threshold-pull beacon (new ADR) · L3b live-harness Vigil/Oracle (M2.5 harness-extension) · inotify `recursive`+`throttle` · `PortentEvent.data` hard-cut

## Open solutions (will be needed before the appropriate phase - DO NOT block ongoing work)
- **Multi-key RBAC selectors** (`coven=X,service=Y` AND): now single-key selector (`coven=` OR `service=`); combined AND is not expressed by a grammar (the structure `Permission.Selector` AND can - it lacks a parsing grammar). Need parser-extension + separate ADR (ADR-008 amend (a) restriction).
- **Destroy guard-rail cross-feature** (⚠ when reviewing destroy-chain): cloud-leaf `core.cloud.provisioned` `destroyed` = immediate TerminateInstances **without** orchestration-guard-rails. Make sure that it is called ONLY after guard-rails `incarnation.destroy` (two-phase `destroying`→`destroy_failed`, [destroy statuses](architecture.md#statuses-destroying-and-destroy_failed)) - otherwise "a typo in `count` erases the prod" (cloud-cascade ADR-017 Consequences erases souls/seeds/tokens).
- **Drift Slice C** (background periodic scan + full lock-strategy with upgrade/destroy + drift for `core.git` / `core.url` without checksum): architecture needed (Reaper-like leader vs separate coroutine; concurrency check-drift × apply/upgrade/destroy; drift signature for `git ls-remote` / `core.url` HEAD mode) + solution. Until the decision - drift only by the on-demand pilot.
- **S3 push↔Acolyte:** push synchronous, Acolyte ([ADR-027](adr/0027-apply-work-queue.md)) asynchronous `apply_runs`-barrier - the junction may require amend ADR-027 (architect marked, check at S3).
- **dispatcher `proxy_jump` support** (Teleport-via-bastion): `soul-ssh-teleport` returns `SignReply.proxy_jump`, dispatcher (`keeper/internal/push`) field IGNORES - `net.Dial` goes directly. Full Teleport flow requires dispatcher proxy_jump support. Working in parallel.
