# ADR-065. core.module.installed — delivery of SoulModule plugins to the Soul host (FetchModule + plugins.soul_modules catalog)

> **Status: amended (accepted, docs-first BEFORE code; implementation — slices S1–S6; amendment 2026-07-03 — auto-synthesis of install steps from `service.yml::modules[]`).** Architect's design, all decisions approved by the user (2026-07-02). Closes open Q No. 5 "where the module registry lives in the Keeper" ([architecture.md → Open questions](../architecture.md#current)) and the design debt [ADR-015 Consequences](0015-core-modules-mvp.md) ("the `core.module.installed` specification is a separate task"). **Amends [ADR-012](0012-keeper-soul-grpc.md) (third RPC `FetchModule`, only-add) / [ADR-020](0020-plugin-infrastructure.md) (`plugins.soul_modules[]` catalog) / [ADR-015](0015-core-modules-mvp.md) (specification fixed).** [ADR-026](0026-sigil.md) (Sigil) — **NOT changed**: allowances, signature, snapshot distribution and host-side verify are reused as-is, no new trust mechanisms.
>
> **Amendment 2026-07-03 (auto-synthesis from `modules[]`)** — see the block of the same name in (e): the canon "only an explicit step, without auto-inject" is **lifted** — Keeper synthesizes Soul-side `core.module.installed` steps from the explicit manifest declaration `service.yml::modules[]`; the validation-hint post-MVP is preserved in the reverse direction.

**Context.** `core.module.installed` was declared as of [ADR-015](0015-core-modules-mvp.md) as an infrastructure core module for delivering custom modules (`soul-mod-*`) to a managed host, but neither the byte transport Keeper→Soul nor a registry of SoulModule sources on the Keeper existed — the specification was deferred "until the Soul daemon is implemented". Meanwhile all the adjacent infrastructure is already live: git resolution of the plugin catalog into an FS cache ([`keeper/internal/plugingit`](../../keeper/internal/plugingit), [ADR-026(g)](0026-sigil.md) — but only for keeper-side kinds `cloud_drivers`/`ssh_providers`), Sigil allowances in PG (`plugin_sigils` + persisted `manifest_raw`) and their distribution to the Soul (`SigilSnapshot`/`SigilTrustAnchors`, ReplaceAll), host-side verify ([`shared/pluginhost`](../../shared/pluginhost)). The first real consumer is `community.redis.*` in cloud-provision runs: a freshly created VM must receive the plugin **within the same run** that installs the redis role ([ADR-061](0061-onboarding-await-and-midrun-reresolve.md)/[ADR-063](0063-bootstrap-token-delivery.md)). Exactly two pieces were missing: (1) a channel to transfer the binary's bytes Keeper→Soul; (2) where the Keeper takes those bytes from (open Q No. 5).

## Decision

### (a) Fetch transport — server-streaming RPC `FetchModule`

A new **third** RPC in `service Keeper` ([ADR-012(a)](0012-keeper-soul-grpc.md) is extended):

```
service Keeper {
  rpc Bootstrap(BootstrapRequest) returns (BootstrapReply);            // unary, server-only TLS (as before)
  rpc EventStream(stream FromSoul) returns (stream FromKeeper);        // bidi, mTLS (as before)
  rpc FetchModule(PluginFetchRequest) returns (stream PluginChunk);    // server-streaming, mTLS — NEW
}
```

- **The same mTLS listener as EventStream** (Bootstrap stays on its own server-only TLS listener, [ADR-012(f)](0012-keeper-soul-grpc.md)); no new port/listener. Artifact bytes travel over a **separate HTTP/2 stream**, NOT through EventStream — megabytes of the binary do not choke the control-plane (the apply/presence message queue is not blocked).
- **Content-addressed.** Keeper serves **only** bytes whose `sha256` is present in an **active** `plugin_sigils` allowance with `kind: soul_module` (the kind is read from the allowance's persisted manifest, no new PG columns). A request for an unknown/revoked digest → rejection.
- **Authorization — mTLS peer-cert (SoulSeed)**, like EventStream; SID — from the SAN ([ADR-012(i)](0012-keeper-soul-grpc.md)). There is no operator RBAC — Soul is not an operator.
- **Guard-rails:** size ceiling — the existing `plugins.max_artifact_size_mb` ([ADR-026(g)](0026-sigil.md)); rate-limit of parallel fetches per-SID (protection against storms; the config field name — at S1 implementation).
- **Forward-compat — only-add** ([ADR-012(c)](0012-keeper-soul-grpc.md)): a new method + new messages, existing ones untouched. An old Soul does not call the method; a new Soul against an old Keeper gets `Unimplemented` → the install step fails `module_fetch_failed` (explicit-reject, not a hang). The exact field set of `PluginFetchRequest` (content-address `binary_sha256` + `namespace`/`name` for slot lookup and diagnostics) and its placement in the thematic layout of `.proto` files ([ADR-012(b)](0012-keeper-soul-grpc.md)) — slice S1.

### (b) Byte registry — the `plugins.soul_modules[]` catalog, NO new storage (closes open Q No. 5)

`keeper.yml::plugins` is extended with a third kind of entries — **`soul_modules[]`** (`{name, source, ref}`), symmetric to `cloud_drivers`/`ssh_providers` ([keeper/plugins.md → Plugin directory](../keeper/plugins.md#plugin-directory-in-keeperyml)):

```yaml
plugins:
  soul_modules:
    - { name: redis, source: "git@github.com:souls-guild/soul-mod-community-redis.git", ref: v1.2.0 }
```

- **Resolution — the existing `plugingit`** (go-git F-fetch → R-nested FS cache `cache_root`, [ADR-026(g)](0026-sigil.md)) reusing all the hardening (scheme-allowlist, size-limits, fail-closed per-entry).
- **Allowance — the existing Sigil flow**: Archon `plugin.allow` → record in `plugin_sigils`.
- **Authority on the wire — sha256 from PG `plugin_sigils`** (Keeper's signature); the FS cache carries only bytes.
- **NO new storage**: PG = allowances (already exist), FS = bytes (already exist), git = provenance (already exists, [ADR-007](0007-versioning-git-ref.md)).
- **HA:** the FS cache is per-instance. A fetch that lands on a Keeper instance without a materialized slot → on-demand catalog resolution or a rejection with retry (Soul asks again; the policy — S1). Divergences between instances are safe by construction: the served bytes are in any case checked against the allowance's sha256.
- **S3-compatible artifact-store — a post-GA extension BEHIND the fetch abstraction**: the `FetchModule` contract does not change, only the byte-reading backend on the Keeper changes. Noted, NOT implemented in this ADR.

### (c) Semantics of `core.module.installed` (Soul-side, state `installed`)

Addressing: namespace `core`, module `module`, state `installed`; the step is Soul-side (`on:` omitted or coven labels).

| Parameter | Type | Req. | Semantics |
|---|---|---|---|
| `name` | string | **yes** | Full plugin name `<namespace>.<name>` (e.g. `community.redis`). |
| `ref` | string | — | **Pin check, NOT version selection**: the active Sigil allowance must be on this ref, otherwise the step is `failed` (`module_not_allowed`). Authority = sha256 of the active allowance; `ref` is the operator's safeguard "I expect exactly this ref". |

**Idempotency:** sha256 of the already installed binary == `binary_sha256` of the active Sigil → `changed=false`, no fetch is performed. **The "all allowed in bulk" scope — NOT in MVP** (a separate option later, on a real request).

### (d) hot-register — MANDATORY in MVP

After a successful install, Soul **re-discovers the module catalog without restarting the daemon** (thread-safe `Rescan` of the custom-module registry). Without this the canonical scenario does not work: `community.redis.*` tasks **in the same run** after the install step would not find the module, and a daemon restart = EventStream break = run break.

**Known MVP limitation:** the Beacon registry (`soul_beacon` plugins, [ADR-030](0030-vigil-oracle.md)) is **NOT rebuilt** on rescan — hot-reload of beacons is a separate post-MVP slice.

### (e) Scenario integration — explicit step, without auto-inject

The operator writes the install step **explicitly** before the first use of the module (symmetry with `core.soul.registered` — the "operator writes explicitly" canon):

```yaml
- module: core.module.installed
  params: { name: community.redis }
```

`service.yml::modules[]` — **validation-hint post-MVP**: a render/soul-lint gate "a module is used in tasks → it must be in `modules[]` and have an active Sigil allowance". This is a hint-check, **NOT auto-inject** of an install step.

**Amendment 2026-07-03: auto-synthesis of install steps from `service.yml::modules[]`.** The (e) canon "only an explicit step, without auto-inject" is **lifted**: Keeper synthesizes Soul-side `core.module.installed` steps from the **explicit manifest declaration** `service.yml::modules[]` (`{name, ref}`, [ADR-007](0007-versioning-git-ref.md); format — [service/manifest.md](../service/manifest.md)) — the operator declares the dependency once per service, rather than with a boilerplate step in each scenario. This is NOT the rejected auto-inject "by analysis of used modules without declaration" (see Rejected alternatives): the declaration is explicit, the "operator writes explicitly" canon is honored at the manifest level.

- **Synthesis point** — the scenario-runner, immediately after `include:` expansion (a flat task list) and BEFORE Stratify ([ADR-056](0056-staged-render-passage.md)); symmetrically in check-drift ([ADR-031](0031-scry-drift.md)), Acolyte claim-render (reproduces the run-goroutine plan — plan_index/TaskEvent correlation) and the L0-trial-harness. The synthesized step is an ordinary plan task: it goes through render→dispatch→TaskEvent, visible in the run-view with the marker name `install <ns>.<module> (service manifest)`. The pre-flight/parsing/UI surfaces do NOT mutate the plan.
- **Position** — immediately before the first consumer task (a `module:` task with the `<ns>.<module>.` prefix; a consumer inside a `block:` → insertion before the entire block). A module with no consumers in the plan is NOT synthesized. The synthesis step gets its Passage along the common Stratify axes: as a roster consumer (without `on:`) it automatically rides after the roster-refresh boundary ([ADR-061](0061-onboarding-await-and-midrun-reresolve.md)) — provision-from-zero works without special logic.
- **Params** — `{name: <from the entry>, ref: <from the entry>}`: the `ref` pin check (c) is inherited from the manifest.
- **Dedup/takeover** — an explicit `core.module.installed` step with the same literal `params.name` in the plan disables synthesis for that name (the operator controls position/`ref`/`when:` themselves). A CEL expression in an explicit step's `params.name` does not lend itself to literal comparison — a duplicate step is possible, harmless due to idempotency (c).
- **Idempotency** — only module-level (c), by sha256; there is no plan-level skip (Keeper does not keep a registry of what is installed per-host, the roster changes mid-run).
- **No keeper-side fail-fast in MVP** — the absence of a `plugins.soul_modules[]` entry / active Sigil allowance is caught by the Soul-side allow-check (f) fail-closed before a single network byte (`module_not_allowed`); a pre-flight gate before `applying` — together with the validation-hint post-MVP.
- **Push is not affected** — modules travel in bulk ([ADR-020](0020-plugin-infrastructure.md)), there is no EventStream in oneshot. `core.*` in `modules[]` is still forbidden by manifest validation (`core_module_in_modules_list`).
- **The validation-hint post-MVP is preserved** — in the reverse direction: a module is used in tasks but is not declared in `modules[]` (+ there is no active Sigil allowance) → soul-lint/render gate. After synthesis the hint matters more: an undeclared module does not get an install step and fails at runtime.
- **MVP limitation** — consumers are determined by the top-level/`block:` `module:` tasks of the scenario; modules used only inside a destiny (via `apply:`) are not counted as consumers — the operator is left with an explicit step; follow-up together with the validation-hint.

### (f) Sigil verification at install-time — reuse, NO new trust mechanisms

1. **allow-check BEFORE fetch:** no active allowance `(namespace, name)` with `kind: soul_module` in the Soul's local Sigil set → the step is `failed` `module_not_allowed` — **before a single network byte**.
2. fetch by content-address (`FetchModule`).
3. **full verify before atomic rename:** sha256(downloaded bytes) == `binary_sha256` of the allowance + the Sigil signature is valid against the trust-anchor set + `manifest_sha256` matches. Reuse of [`shared/pluginhost`](../../shared/pluginhost). Failure → `module_verify_failed`, the binary is not materialized.
4. **The manifest is materialized from `PluginSigil.manifest_raw`** (already carried by `SigilSnapshot`) — the manifest does NOT travel through `FetchModule`.

### (g) Soul cache layout — directory-based

`<paths.modules>/<ns>-<name>/{manifest.yaml, soul-mod-<name>}` — a single-active slot per `(namespace, name)` pair, written via atomic rename. Replaces the early flat schema `soul-mod-<name>-<sha>` (doc-fix [soul/modules.md](../soul/modules.md)). There is deliberately no `commit_sha` axis (as in the keeper-side R-nested) on the Soul: multiple versions side by side are not needed — authority = the active Sigil, "rollback" = revoke+allow of another allowance on the Keeper + a repeated install step.

## MVP boundaries

- **no `absent` state** — cache cleanup via the existing TTL (`cleanup.modules_ttl_days`, [soul/modules.md](../soul/modules.md));
- ~~**no auto-inject** of install steps~~ — **lifted by the 2026-07-03 amendment**: auto-synthesis from the explicit declaration `service.yml::modules[]` (see (e));
- **no beacon-hot-reload** on rescan (see (d));
- **no "all allowed in bulk" scope** (see (c));
- **S3-artifact-store — post-GA** behind the fetch abstraction (see (b)).

## Contract-impact

- **proto** — only-add: RPC `FetchModule` + messages `PluginFetchRequest`/`PluginChunk` ([ADR-012(c)](0012-keeper-soul-grpc.md) forward-compat; fields and file — S1). `proto/plugin/v1/` untouched.
- **config** — additive: `plugins.soul_modules[]` (+ a fetch rate-limit config field, S1); the existing `plugins.*`/`plugin_runtime` fields do not change.
- **PG schema — NOT touched**: `plugin_sigils` as-is; `kind: soul_module` is read from the allowance manifest (persisted `manifest_raw`, migration 030).
- **UI / soulctl / MCP / plugin-SDK — not affected**: the Sigil allow/revoke/list surface already exists ([ADR-026](0026-sigil.md)); plugin authors need do nothing.
- **TaskError reasons** (open catalog, [naming-rules.md → Error codes](../naming-rules.md#error-codes)): `module_not_allowed` / `module_fetch_failed` / `module_verify_failed`.

## Rejected alternatives

- **Bytes over EventStream** (a chunk message in `oneof payload`). Rejected: megabytes of the artifact in the control-plane stream block the apply/presence message queue; a separate HTTP/2 stream on the same connection gives isolation for free.
- **A new byte storage** (PG `bytea` / a mandatory artifact-store). Rejected: the bytes already lie in the git resolver's FS cache, the allowances are already in PG; a third storage is a duplicate without benefit, and a mandatory artifact-store would break the mandatory dependency tier [ADR-053](0053-dependency-tiers.md). S3 — a post-GA option behind the fetch abstraction.
- **Daemon restart instead of hot-register.** Rejected: a restart = EventStream break = break of the current run — the install step and the module consumer cannot live in one run.
- **Auto-inject of the install step by analysis of used modules WITHOUT declaration.** Rejected: hidden magic against the "operator writes explicitly" canon. Synthesis from the explicit declaration `service.yml::modules[]` (amendment 2026-07-03, see (e)) is NOT this case: the operator declares the dependency explicitly at the manifest level; the earlier wording "only an explicit step" is lifted by the amendment, the validation-hint post-MVP is preserved in the reverse direction.
- **`ref` as version selection.** Rejected: the source of truth for "which binary is allowed" is the sha256 of the active Sigil allowance, not a task parameter; `ref` in params is only a pin check.

## Slices

- **S0** — this document (ADR + amendments + naming + doc-fix).
- **S1** — proto `FetchModule`/`PluginFetchRequest`/`PluginChunk` (`make gen`) + keeper-side handler (content-addressed serving by `plugin_sigils`, mTLS-auth, rate-limit per-SID, size-cap).
- **S2** — config catalog `plugins.soul_modules[]` + resolution of SoulModule entries with the existing `plugingit` (reuse).
- **S3** — Soul-side `core.module.installed`: allow-check → fetch → verify → atomic rename into the directory slot; idempotency by sha256.
- **S4** — hot-register: thread-safe `Rescan` of the Soul daemon's custom-module registry.
- **S5** — e2e-guard: install step + `community.redis.*` in one run (regression test of the canonical scenario).
- **S6** — live validation on the cloud-provision Souls (redis) + DoD closure.

## Amends

- **[ADR-012](0012-keeper-soul-grpc.md)** — `service Keeper` extended with a third RPC `FetchModule` (only-add).
- **[ADR-020](0020-plugin-infrastructure.md)** — the `keeper.yml::plugins` catalog extended with `soul_modules[]`; delivery of SoulModule plugins to Soul hosts is formalized (previously catalog resolution — only keeper-side kinds).
- **[ADR-015](0015-core-modules-mvp.md)** — "the `core.module.installed` specification is a separate task" is closed by this ADR.
- **[ADR-026](0026-sigil.md)** — NO changes (cross-ref: `FetchModule` reuses `plugin_sigils` allowances, signature and `shared/pluginhost` verify as-is).
