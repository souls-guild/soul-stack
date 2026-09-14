# ADR-065. core.module.installed — delivery of SoulModule plugins to the Soul host (FetchModule + plugins.soul_modules catalog)

> **Status: amended (accepted, docs-first BEFORE code; implementation — slices S1–S6; amendment 2026-07-03 — auto-synthesis of install steps from `service.yml::modules[]`).** Architect's design, all decisions approved by the user (2026-07-02). Closes open Q No. 5 "where the module registry lives in the Keeper" ([architecture.md → Open questions](../architecture.md#current)) and the design debt [ADR-015 Consequences](0015-core-modules-mvp.md) ("the `core.module.installed` specification is a separate task"). **Amends [ADR-012](0012-keeper-soul-grpc.md) (third RPC `FetchModule`, only-add) / [ADR-020](0020-plugin-infrastructure.md) (`plugins.soul_modules[]` catalog) / [ADR-015](0015-core-modules-mvp.md) (specification fixed).** [ADR-026](0026-sigil.md) (Sigil) — **NOT changed**: allowances, signature, snapshot distribution and host-side verify are reused as-is, no new trust mechanisms.
>
> **Amendment 2026-07-03 (auto-synthesis from `modules[]`)** — see the block of the same name in (e): the canon "only an explicit step, without auto-inject" is **lifted** — Keeper synthesizes Soul-side `core.module.installed` steps from the explicit manifest declaration `service.yml::modules[]`; the validation-hint post-MVP is preserved in the reverse direction.
>
> **Amendment 2026-08-07 (NIM-524)** — see ["`modules[]` declares an address, the synthesized step takes level 1"](#amendment-2026-08-07-nim-524-modules-declares-an-address-the-synthesized-step-takes-level-1): the NIM-377 amendment left the producer emitting the whole two-level name into a `params.name` that forbids the dot, so **every service declaring `modules:` failed at apply**. The declaration stays an address; the synthesizer passes level 1, several entries of one artifact collapse into one install, and a ref disagreement under one alias is now the diagnostic `conflicting_module_ref`.
>
> **Amendment 2026-08-26 (NIM-543)** — see ["the takeover key is address level 1 on BOTH sides"](#amendment-2026-08-26-nim-543-the-takeover-key-is-address-level-1-on-both-sides-and-a-mis-spelled-paramsname-is-caught-offline): NIM-524 reduced the synthesizer's OUTPUT and left its takeover comparison keyed on raw spellings, so an explicit step written `name: community.redis` took over nothing and earned a SECOND install of the same artifact beside it. Both halves now reduce to level 1, and a `params.name` that is not a registration alias is the new offline diagnostic `module_install_name_not_an_alias`.
>
> **Amendment 2026-09-05 (NIM-829)** — see ["a `modules[]` entry declares an ARTIFACT, not one of its objects"](#amendment-2026-09-05-nim-829-a-modules-entry-declares-an-artifact-not-one-of-its-objects): the 2026-08-07 amendment kept the declaration as an address while NIM-765 turned level 2 into an OBJECT inside the one binary, so a service declared one artifact once per object. The entry is now the registration alias; the two-level form is deprecated with `module_name_two_level_deprecated` until 2026-12-01, and `conflicting_module_ref` is retired with it because one row per artifact cannot express two refs for one slot.

**Context.** `core.module.installed` was declared as of [ADR-015](0015-core-modules-mvp.md) as an infrastructure core module for delivering custom module artifacts to a managed host, but neither the byte transport Keeper→Soul nor a registry of SoulModule sources on the Keeper existed — the specification was deferred "until the Soul daemon is implemented". Meanwhile all the adjacent infrastructure is already live: git resolution of the plugin catalog into an FS cache ([`keeper/internal/plugingit`](../../keeper/internal/plugingit), [ADR-026(g)](0026-sigil.md) — but only for keeper-side kinds `cloud_drivers`/`ssh_providers`), Sigil allowances in PG (`plugin_sigils` + persisted `manifest_raw`) and their distribution to the Soul (`SigilSnapshot`/`SigilTrustAnchors`, ReplaceAll), host-side verify ([`shared/pluginhost`](../../shared/pluginhost)). The first real consumer is `community.redis.*` in cloud-provision runs: a freshly created VM must receive the plugin **within the same run** that installs the redis role ([ADR-061](0061-onboarding-await-and-midrun-reresolve.md)/[ADR-063](0063-bootstrap-token-delivery.md)). Exactly two pieces were missing: (1) a channel to transfer the binary's bytes Keeper→Soul; (2) where the Keeper takes those bytes from (open Q No. 5).

## Decision

### (a) Fetch transport — server-streaming RPC `FetchModule`

> **⚠ This is one of TWO transports as of the [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) (NIM-794), not the transport — and the second one is SHIPPED (NIM-796).** A host that can reach the artifact source fetches from it directly; `FetchModule` remains the path for hosts without egress and is **not deprecated**. Everything the section says about `FetchModule` itself — content-addressing, mTLS auth, guard-rails, only-add — is unchanged.

A new **third** RPC in `service Keeper` ([ADR-012(a)](0012-keeper-soul-grpc.md) is extended):

```
service Keeper {
  rpc Bootstrap(BootstrapRequest) returns (BootstrapReply);            // unary, server-only TLS (as before)
  rpc EventStream(stream FromSoul) returns (stream FromKeeper);        // bidi, mTLS (as before)
  rpc FetchModule(PluginFetchRequest) returns (stream PluginChunk);    // server-streaming, mTLS — NEW
}
```

- ⚠ **Narrowed by the [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) to the egress-free path** — it describes what happens when `FetchModule` is the transport, which is no longer always. The stream isolation claimed below is real and unchanged; what the amendment adds is that a separate stream on the **same connection to the Keeper** still shares that connection's bandwidth with run dispatch, which is one of the reasons a host that can reach the source is better off going there.
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
    - { name: redis, source: "git@github.com:souls-guild/redis.git", ref: v1.2.0 }
```

- **Resolution — the existing `plugingit`** (go-git F-fetch → R-nested FS cache `cache_root`, [ADR-026(g)](0026-sigil.md)) reusing all the hardening (scheme-allowlist, size-limits, fail-closed per-entry).
- **Allowance — the existing Sigil flow**: Archon `plugin.allow` → record in `plugin_sigils`.
- **Authority on the wire — sha256 from PG `plugin_sigils`** (Keeper's signature); the FS cache carries only bytes.
- ⚠ **"NO new storage" becomes CONDITIONAL with the [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) (NIM-794).** It still holds — no third store is introduced — but only on the condition that the FS cache holds **N artifacts per slot** rather than one, so that `FetchModule` can still serve every platform a grant covers. The amendment states that condition and what it costs; the claim below is true given it and false without it.
- **NO new storage**: PG = allowances (already exist), FS = bytes (already exist), git = provenance (already exists, [ADR-007](0007-versioning-git-ref.md)).
- **HA:** the FS cache is per-instance. A fetch that lands on a Keeper instance without a materialized slot → on-demand catalog resolution or a rejection with retry (Soul asks again; the policy — S1). Divergences between instances are safe by construction: the served bytes are in any case checked against the allowance's sha256.
- ★ ⚠ **Source-pull is NOT this bullet, and filing it here gets the architecture backwards.** The [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) (NIM-794) is a **different axis**, and the difference is the whole point: in the bullet below the `FetchModule` contract is **kept** and only the Keeper's byte-reading backend changes — the host still calls `FetchModule` and cannot tell. Under source-pull the host **stops calling `FetchModule` at all** and fetches from the artifact source itself. One changes where the Keeper reads bytes from; the other changes whether the Keeper is in the path. The bullet below stands on its own terms and is still not implemented.
- **S3-compatible artifact-store — a post-GA extension BEHIND the fetch abstraction**: the `FetchModule` contract does not change, only the byte-reading backend on the Keeper changes. Noted, NOT implemented in this ADR.

### (c) Semantics of `core.module.installed` (Soul-side, state `installed`)

Addressing: namespace `core`, module `module`, state `installed`; the step is Soul-side (`on:` omitted or coven labels).

| Parameter | Type | Req. | Semantics |
|---|---|---|---|
| `name` | string | **yes** | The **registration alias** of the module family (e.g. `redis`), amended 2026-08-06 — previously the two-level `<namespace>.<name>` read out of the artifact. See the [amendment](#amendment-2026-08-06-nim-377-the-slot-is-named-by-the-alias-and-the-schema-rides-in-the-artifact). |
| `ref` | string | — | **Pin check, NOT version selection**: the active Sigil allowance must be on this ref, otherwise the step is `failed` (`module_not_allowed`). Authority = sha256 of the active allowance; `ref` is the operator's safeguard "I expect exactly this ref". ⚠ **Narrowed by the [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path):** "sha256 of the active allowance" becomes **the sha256 of the row this host selected** from the grant's artifact list. |

⚠ **The Idempotency sentence below is FALSE as of the [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) (NIM-794)**, in one word: there is no longer *the* `binary_sha256` of the active Sigil — there is a **list**, and the comparison is against the entry for this host's platform. The mechanism is otherwise unchanged (compare the installed file's digest, skip the fetch on a match). **⚠ The list is NIM-795 and is not in the tree yet** — the shipped comparison is still against the scalar `BinarySHA256hex`, which is [what is half-wired](#what-is-half-wired-and-what-nim-795-inherits). Original text follows.

**Idempotency:** sha256 of the already installed binary == `binary_sha256` of the active Sigil → `changed=false`, no fetch is performed. **The "all allowed in bulk" scope — NOT in MVP** (a separate option later, on a real request).

### (d) hot-register — MANDATORY in MVP

After a successful install, Soul **re-discovers the module catalog without restarting the daemon** (thread-safe `Rescan` of the custom-module registry). Without this the canonical scenario does not work: `community.redis.*` tasks **in the same run** after the install step would not find the module, and a daemon restart = EventStream break = run break.

**Known MVP limitation:** the Beacon registry (`soul_beacon` plugins, [ADR-030](0030-vigil-oracle.md)) is **NOT rebuilt** on rescan — hot-reload of beacons is a separate post-MVP slice.

### (e) Scenario integration — explicit step, without auto-inject

The operator writes the install step **explicitly** before the first use of the module (symmetry with `core.soul.registered` — the "operator writes explicitly" canon):

```yaml
- module: core.module.installed
  params: { name: redis }   # the ALIAS: level 1, the slot — never `redis.instance`
```

`service.yml::modules[]` — **validation-hint post-MVP**: a render/soul-lint gate "a module is used in tasks → it must be in `modules[]` and have an active Sigil allowance". This is a hint-check, **NOT auto-inject** of an install step.

**Amendment 2026-07-03: auto-synthesis of install steps from `service.yml::modules[]`.** The (e) canon "only an explicit step, without auto-inject" is **lifted**: Keeper synthesizes Soul-side `core.module.installed` steps from the **explicit manifest declaration** `service.yml::modules[]` (`{name, ref}`, [ADR-007](0007-versioning-git-ref.md); format — [service/manifest.md](../service/manifest.md)) — the operator declares the dependency once per service, rather than with a boilerplate step in each scenario. This is NOT the rejected auto-inject "by analysis of used modules without declaration" (see Rejected alternatives): the declaration is explicit, the "operator writes explicitly" canon is honored at the manifest level.

- **Synthesis point** — the scenario-runner, immediately after `include:` expansion (a flat task list) and BEFORE Stratify ([ADR-056](0056-staged-render-passage.md)); symmetrically in Acolyte claim-render (reproduces the run-goroutine plan — plan_index/TaskEvent correlation) and the L0-trial-harness. (check-drift was a third symmetric site until NIM-446 removed it.) The synthesized step is an ordinary plan task: it goes through render→dispatch→TaskEvent, visible in the run-view with the marker name `install <alias> (service manifest)`. The pre-flight/parsing/UI surfaces do NOT mutate the plan.
- **Position** — immediately before the first consumer task (a `module:` task with the `<ns>.<module>.` prefix; a consumer inside a `block:` → insertion before the entire block). A module with no consumers in the plan is NOT synthesized. The synthesis step gets its Passage along the common Stratify axes: as a roster consumer (without `on:`) it automatically rides after the roster-refresh boundary ([ADR-061](0061-onboarding-await-and-midrun-reresolve.md)) — provision-from-zero works without special logic.
- **Params** — `{name: <address level 1 of the entry>, ref: <from the entry>}`: the `ref` pin check (c) is inherited from the manifest.
- **Dedup/takeover** — an explicit `core.module.installed` step naming the same **slot** in the plan disables synthesis for it (the operator controls position/`ref`/`when:` themselves); both sides of that comparison are reduced to address level 1 (amended 2026-08-26, NIM-543). A `params.name` carrying a `${…}` cell — wholly or in part — does not lend itself to literal comparison, so synthesis is not suppressed: a duplicate step is possible, harmless due to idempotency (c).
- **Idempotency** — only module-level (c), by sha256; there is no plan-level skip (Keeper does not keep a registry of what is installed per-host, the roster changes mid-run).
- **No keeper-side fail-fast in MVP** — the absence of a `plugins.soul_modules[]` entry / active Sigil allowance is caught by the Soul-side allow-check (f) fail-closed before a single network byte (`module_not_allowed`); a pre-flight gate before `applying` — together with the validation-hint post-MVP.
- **Push is not affected** — modules travel in bulk ([ADR-020](0020-plugin-infrastructure.md)), there is no EventStream in oneshot. `core.*` in `modules[]` is still forbidden by manifest validation (`core_module_in_modules_list`).
- **The validation-hint post-MVP is preserved** — in the reverse direction: a module is used in tasks but is not declared in `modules[]` (+ there is no active Sigil allowance) → soul-lint/render gate. After synthesis the hint matters more: an undeclared module does not get an install step and fails at runtime.
- **MVP limitation** — consumers are determined by the top-level/`block:` `module:` tasks of the scenario; modules used only inside a destiny (via `apply:`) are not counted as consumers — the operator is left with an explicit step; follow-up together with the validation-hint.

### (f) Sigil verification at install-time — reuse, NO new trust mechanisms

1. **allow-check BEFORE fetch:** no active allowance for the **registration alias** with `kind: soul_module` in the Soul's local Sigil set → the step is `failed` `module_not_allowed` — **before a single network byte**. (⚠ This line said `(namespace, name)` — stale since NIM-438, corrected as a rider by the [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path). The lookup keys on the alias: `soul/internal/coremod/module/installed.go:63`.)
2. fetch by content-address (`FetchModule`). ⚠ **This is the one step the [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) changes, and the change is SHIPPED (NIM-796)** — the fetch goes to the artifact source when the grant names one. Content-addressing is unchanged; the endpoint is not.
3. **full verify before atomic rename:** sha256(downloaded bytes) == `binary_sha256` of the allowance + the Sigil signature is valid against the trust-anchor set + `manifest_sha256` matches. Reuse of [`shared/pluginhost`](../../shared/pluginhost). Failure → `module_verify_failed`, the binary is not materialized.

   ⚠ **"`binary_sha256` of the allowance" in the line above is FALSE as of the [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path)** — the allowance carries a **list**, and the comparison is against this host's selected row. (**⚠ Design, not tree: the list is NIM-795.** What ships verifies against the scalar, which is why a multi-platform grant today installs on exactly one platform and refuses on the rest — [what is half-wired](#what-is-half-wired-and-what-nim-795-inherits).) Both the position of this step (before materialization) and its fail-closed behaviour are unchanged, and are what make an untrusted source safe. The original text stands above, unedited. (This marker is deliberately an unnumbered note, as every other marker in this file is. A marker written as its own `3.` gives the list two items numbered 3, Markdown renumbers it 1–5, and "**(f) step 4**" — cited from "(f)/(c) How the schema reaches the host" below and from the ADR index — then lands on the wrong step.)

4. **The schema document is materialized from `PluginSigil.schema`** (field 9, already carried by `SigilSnapshot`; the field was `manifest_raw` before NIM-438, and field 6 `manifest` is **reserved, never reused**). ⚠ **Amended 2026-08-06** — the second half of this line, "does NOT travel through `FetchModule`", is no longer true: the schema is stamped into the artifact as a trailer, so it necessarily arrives with the bytes as well. See the [amendment](#amendment-2026-08-06-nim-377-the-slot-is-named-by-the-alias-and-the-schema-rides-in-the-artifact).

### (g) Soul cache layout — directory-based

⚠ **Amended 2026-08-06 (NIM-377)** — the slot is `<paths.modules>/<alias>/`, holding one executable and no `manifest.yaml`; see the [amendment](#amendment-2026-08-06-nim-377-the-slot-is-named-by-the-alias-and-the-schema-rides-in-the-artifact). Original text follows.

`<paths.modules>/<ns>-<name>/{manifest.yaml, <name>}` — a single-active slot per `(namespace, name)` pair, written via atomic rename (the binary was `soul-mod-<name>` until NIM-851 dropped the prefix; nothing reads the filename either way). Replaces the early flat schema `soul-mod-<name>-<sha>` (doc-fix [soul/modules.md](../soul/modules.md)). There is deliberately no `commit_sha` axis (as in the keeper-side R-nested) on the Soul: multiple versions side by side are not needed — authority = the active Sigil, "rollback" = revoke+allow of another allowance on the Keeper + a repeated install step.

## MVP boundaries

- **no `absent` state** — cache cleanup via the existing TTL (`cleanup.modules_ttl_days`, [soul/modules.md](../soul/modules.md));
- ~~**no auto-inject** of install steps~~ — **lifted by the 2026-07-03 amendment**: auto-synthesis from the explicit declaration `service.yml::modules[]` (see (e));
- **no beacon-hot-reload** on rescan (see (d));
- **no "all allowed in bulk" scope** (see (c));
- **S3-artifact-store — post-GA** behind the fetch abstraction (see (b)).

## Contract-impact

- **proto** — only-add: RPC `FetchModule` + messages `PluginFetchRequest`/`PluginChunk` ([ADR-012(c)](0012-keeper-soul-grpc.md) forward-compat; fields and file — S1). `proto/plugin/v1/` untouched.
- **config** — additive: `plugins.soul_modules[]` (+ a fetch rate-limit config field, S1); the existing `plugins.*`/`plugin_runtime` fields do not change.
- ~~**PG schema — NOT touched**~~ ⚠ **superseded (NIM-377 / NIM-438, migration 115 — this line said 113 until the [2026-09-04 amendment](#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path); see the [NIM-377 amendment](#amendment-2026-08-06-nim-377-the-slot-is-named-by-the-alias-and-the-schema-rides-in-the-artifact)):** `plugin_sigils` is re-keyed onto `(source, ref)`, gains `alias`, drops `namespace`/`name`/`manifest`, and `manifest_raw` becomes `schema` (`NOT NULL`). Original claim: `plugin_sigils` as-is; `kind: soul_module` is read from the allowance manifest (persisted `manifest_raw`, migration 030).
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
- **S5** — e2e-guard: install step + `redis.*` in one run (regression test of the canonical scenario).
- **S6** — live validation on the cloud-provision Souls (redis) + DoD closure.

## Amends

- **[ADR-012](0012-keeper-soul-grpc.md)** — `service Keeper` extended with a third RPC `FetchModule` (only-add).
- **[ADR-020](0020-plugin-infrastructure.md)** — the `keeper.yml::plugins` catalog extended with `soul_modules[]`; delivery of SoulModule plugins to Soul hosts is formalized (previously catalog resolution — only keeper-side kinds).
- **[ADR-015](0015-core-modules-mvp.md)** — "the `core.module.installed` specification is a separate task" is closed by this ADR.
- **[ADR-026](0026-sigil.md)** — NO changes (cross-ref: `FetchModule` reuses `plugin_sigils` allowances, signature and `shared/pluginhost` verify as-is).

## Amendment (2026-08-06, NIM-377): the slot is named by the alias, and the schema rides in the artifact

Amends (c), (f) and (g). Decisions settled with the user 2026-08-06; the counterparts are [ADR-020](0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name) (the schema is generated, the artifact carries no name) and [ADR-026](0026-sigil.md#amendment-2026-08-06-nim-377-the-registry-keys-on-the-artifact-source-the-signature-is-not-a-control-on-declarations) (the registry keys on the source).

**Nothing about the delivery mechanism changes.** `FetchModule` keeps its contract, its content-addressed serving, its mTLS auth and its guard-rails; the allow-check still runs before a single network byte; verify still fails closed before the atomic rename; hot-register is still mandatory. What changes is how the slot is named and where the schema comes from.

### (g) Soul-side cache layout — keyed by the alias

```
<paths.modules>/
  redis/                     # the REGISTRATION ALIAS, not <ns>-<name>
    <schema document>        #   canonical JSON, from the artifact's trailer
    redis                    #   the single executable delivered by FetchModule
```

- **The slot name is the registration alias.** Previously `<ns>-<name>/`, composed from fields inside the artifact. Those fields are gone ([ADR-020(p)](0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)) and the alias is what the operator chose at registration, so it is both the only name available and the right one: the same artifact registered twice under two aliases occupies two slots and answers at two addresses, with no rebuild.
- **Single-active per alias**, written by atomic rename — unchanged, and the reason is unchanged: authority is the active Sigil grant, "rollback" is revoke+allow of another grant on Keeper plus a repeated install step. There is deliberately still no `commit_sha` axis on the Soul.
- **The executable's filename no longer means anything.** `Manifest.BinaryName()` is gone; discovery takes the one executable in the slot rather than computing a name to look for. A slot with zero or several executables fails closed.
- **`manifest.yaml` is gone from the slot**, replaced by the canonical-JSON schema document.

### (f)/(c) How the schema reaches the host

The schema is stamped into the artifact as a **trailer** ([ADR-020(o)](0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)), which changes one factual claim in (f) step 4: the schema now **does** travel through `FetchModule`, because it is part of the bytes. That is a consequence of the stamping decision, not a new transport.

`PluginSigil.schema` (field 9, renamed from `manifest_raw` in NIM-438) still carries the schema document, and still matters for the same two reasons it always did:

1. **The allow-check happens before the fetch.** The Soul must know what it is permitted to install before downloading anything; the local Sigil set is where that comes from.
2. **`manifest_sha256` is in the signed block.** Verify compares the schema Keeper signed against the schema the artifact carries. With the trailer these are the same bytes by construction rather than by careful agreement between two delivery paths — the S3↔S6 invariant gets easier to hold, not harder.

**Not settled here:** whether the slot keeps the schema as a separate file or the loader re-reads the trailer on each load. Both satisfy everything above. Decided in the wave-2 slot slice.

### `service.yml::modules[]` and the synthesized step

`modules[].name` becomes the registration alias, matching (c)'s `params.name`. The synthesis rules of the 2026-07-03 amendment are otherwise untouched: position before the first consumer, `ref` pin inherited from the manifest entry, dedup against an explicit step naming the same slot, no plan-level idempotency.

The two-level regex on `modules[].name` (`^[a-z][a-z0-9-]*\.[a-z][a-z0-9-]*$`, [service/manifest.md](../service/manifest.md)) and its `destiny.yml::required_modules[]` twin follow the addressing decision, which is **NIM-376** — not fixed here. What this amendment does fix is that the reserved-name list ([ADR-020(s)](0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)) is checked on both surfaces: at registration and in `required_modules:`.

## Amendment (2026-08-07, NIM-524): `modules[]` declares an address, the synthesized step takes level 1

Amends the paragraph directly above, which is the defect. It said `modules[].name` "becomes the registration alias, matching (c)'s `params.name`" **and** left the two-level regex in place — two sentences that cannot both hold. The field is regex-forced to `<alias>.<module>`; `params.name` is regex-forced to a bare alias (`reAlias`, no dot). No string satisfies both, so **every service declaring `modules:` failed at apply on every host**, `examples/service/redis` included. Both ends shipped green: NIM-377 changed the consumer and added a test pinning its refusal of a dotted name, and never touched the producer, which no unit test connected to it.

**The declaration stays an address; the step takes level 1.** `modules[].name` remains two-level `<alias>.<module>` — it is what the service's scenarios write, and a service that named only the alias would be declaring "install this slot" without saying which module it means to call. The synthesizer passes **level 1 alone** into `params.name`, because that is what the step installs: a slot holds an artifact, and level 2 addresses a module inside it. `(c)` is unchanged and correct as written.

This is not the addressing decision NIM-376 defers. That one is whether level 1 *should* be the alias; NIM-377 already made it so, on both the registry key (`<alias>.<module>`) and the cache slot. What this fixes is one producer that kept emitting the pre-NIM-377 shape.

Two consequences of the collapse, both new here:

- **One install per alias, not one per entry.** Several `modules[]` entries of one artifact (`redis.instance`, `redis.acl`) name the same slot; keying the synthesis on the alias, the entries collapse into a single step before the earliest of their consumers. Keying it on the entry would have installed the same artifact twice.
- **`conflicting_module_ref`** — a new validation diagnostic. Two entries under one alias pinning different `ref`s used to be two independent installs; as one slot they are a contradiction the manifest must state, not a race the last writer wins.

The `destiny.yml::required_modules[]` twin needs no change: it synthesizes nothing (soul-lint reads it as a declaration), so it has no producer/consumer pair to disagree. Both lists keep the reserved-name check.

The guard is a property test, not an expected string: the synthesizer's `params.name` is fed to `plugin.ValidAlias` — the same predicate `core.module.installed` applies — so the two ends are pinned to one rule rather than to two copies of an example.

## Amendment (2026-08-26, NIM-543): the takeover key is address level 1 on BOTH sides, and a mis-spelled `params.name` is caught offline

Amends the takeover rule of the 2026-07-03 amendment and adds an offline check. NIM-524 fixed the synthesizer's **output** and left its **input** comparison keyed on raw spellings: the manifest half was reduced to the alias, the explicit-step half was the literal the author typed. The two therefore met only when the author happened to write a bare alias. An author writing `name: community.redis` — the form (e) itself showed until this amendment, and the form every pre-NIM-377 example carries — took over nothing: the synthesizer inserted a **second** install of the same artifact beside theirs, and both steps then failed on the host for the value's own sake. The escape hatch was silently conditional on spelling, and the damage only became visible at apply.

**Both halves reduce to level 1.** The manifest declares `<alias>.<module>`, an explicit step writes a bare alias; the slot each names is address level 1, so that is the only key on which they can agree. A dotted explicit step **is** a takeover — it names one artifact and one slot, and a synthesized second install beside it helps nobody — while remaining an authoring error reported separately. One reduction, `addrLevel1`, is now the single function both ends call.

**`module_install_name_not_an_alias`** — a new validation diagnostic (error, semantic-validate phase) on `core.module.installed`'s `params.name`. The param is a free string in the schema, so before this the whole class was invisible offline: it parsed, linted and passed `service.yml` validation, then failed on every host. The predicate is `plugin.ValidAlias`, the same rule the Soul applies (`soul/internal/coremod/module`, `reAlias`) and the same rule a registration is accepted under — not a local "has no dot" test, which is how the two ends drifted the first time. For a dotted value the hint names the alias the author meant, since "that is not an alias" leaves them guessing which half to delete.

The check reads the value the Soul will read: **raw**, because `reAlias` runs on the untrimmed param, and through the block-scalar forms as well as the plain one. A padded or folded value that looked fine offline would be refused on the host AND miss the takeover key, which is this defect one spelling further along. `name:` written with nothing after it is caught here too — a null is neither a type mismatch nor a missing key, so nothing else offline spoke for it.

A value carrying a `${…}` cell is not judged ([ADR-010](0010-templating.md)), and **partial** interpolation counts: `acme-${ vars.env }` is legal ([templating.md](../templating.md) §5(b)) and renders to an ordinary alias. That is the same predicate the takeover half uses — one function, called from both — because two ends reading `${…}` differently is the defect above pointed the other way.

The rule is keyed on the **base address** `core.module.installed`, never on the state suffix or on the presence of a `name` param: `name` is a param of nine core states and a dot is ordinary in most of them (`nginx.x86_64`, `redis.service`, `/etc/x.conf`). Both the offline check and the takeover recognition carry a negative test pinning that — `core.pkg.installed` with a dotted `name` is neither diagnosed nor read as a takeover.

Two smaller corrections ride along: the synthesizer's own reserved-name skip now reads the shared reserved list rather than a `core.` prefix — `keeper.*` and `soul.*` sit beside `core` on that list and were being synthesized install steps no registration could ever satisfy — and `core.module.installed`'s schema description in `shared/coremanifest`, which still promised `"<namespace>.<name>" (e.g. community.redis)`, now states the alias. That description is what the UI and `soul-lint` show an author, so it was teaching the bug directly.

## Amendment 2026-09-04 (NIM-794): the fetch step goes to the source, and FetchModule stays as the egress-free path

**The Soul half is IMPLEMENTED (NIM-796, `2daf8545`); the keeper half is NOT (NIM-795).** Epic NIM-793. What ships is `soul/internal/coremod/module/source.go` plus its guard suite `installed_source_test.go`: the host selects its platform row, pulls the bytes from the grant's `base_url`, and reports which transport it used. What does not ship is the grant's **wire form** — the signed block still covers one scalar digest, the proto carries no artifact rows, and nothing fills the read-side DTO from the wire (see ["What is half-wired"](#what-is-half-wired-and-what-nim-795-inherits) below, which is the single most important paragraph here for NIM-795). Decisions settled with the user 2026-09-04. The counterparts of the same date are [ADR-026's](0026-sigil.md#amendment-2026-09-04-nim-794-the-grant-carries-a-list-of-artifacts-and-the-bytes-stop-travelling-through-the-keeper) (the grant carries an artifact list — **still design-only**, it describes the signed block) and [ADR-020's](0020-plugin-infrastructure.md#amendment-2026-09-04-nim-794-the-catalog-entry-gains-a-source-kind-and-an-explicit-artifact-list) (the catalog entry gains `source_kind` — **still design-only**, it describes what an Archon writes).

⚠ **This amendment was written before the code and has been corrected against it.** Two of its statements were wrong and are marked where they stood: the platform axis (the shipped selector is better than the one recorded — see ["Platform row selection"](#platform-row-selection--the-hosts-own-soulprint-facts-with-the-running-binary-as-the-fallback)) and the absent size ceiling (shipped, see ["Two safety consequences"](#two-safety-consequences-one-of-them-now-closed)). The decision narrative around them — why an artifact list, why no template, why the source is untrusted, why `FetchModule` stays — is unchanged and was correct.

### Exactly one step of six changes

`applyInstalled` (`soul/internal/coremod/module/installed.go:44-122`) numbers its own steps, and this amendment is best read against that numbering:

1. **allow-check before a single network byte** (`:60-83`) — including confirming `kind: soul_module` from the **grant's schema bytes** (`:75-83`).
2. **idempotency by sha256** of the already installed file (`:90-93`).
3. **fetch by content address** (`:95-101`).
4. **full Sigil verify before materialization** (`:103-108`).
5. **atomic install into the slot** (`:110-114`).
6. **hot-register** (`:116-119`).

Steps 1, 2, 4, 5 and 6 are **transport-independent**: none of them reads where the bytes came from. **Only step 3 changes** — the endpoint the bytes are pulled from. That is the whole of the Soul-side behavioural change, and stating it as a list is deliberate: a reader who takes "the host now downloads from Nexus" as a rewrite of the install path will go looking for work that does not exist, and may move something that must not move.

**This held.** NIM-796 replaced the body of step 3 with one call to `Module.fetch` (`source.go:81-118`) and touched nothing else in the sequence; `sendInstalled` grew the transport keys (below) and `installSlot`/`removeForeignArtifacts` are untouched. The ordering is pinned by a guard rather than by this paragraph: `TestApplyArtifactSourceVerifyRunsBeforeInstall` (`installed_source_test.go:528`) reds if verify is reordered past the install.

**The order is preserved verbatim: rights before network, signature before disk.** That order is not incidental to the change; it is exactly what makes fetching from an untrusted source safe. The allow-check runs before any byte is requested, so an unapproved alias never causes a network call at all; verify runs before materialization, so bytes that fail it never reach the slot. Neither property depends on who served the bytes.

**Why `kind` is read from the grant and not from the artifact.** The source says it in its own comment (`installed.go:75-77`): the kind comes from the grant's schema bytes — *the same bytes the signature covers* — because reading it from the artifact "would mean trusting a file we have not verified yet, and at this point we have not even fetched it." Under source-pull that reasoning gets stronger rather than weaker, and the line must not be re-litigated by an implementer who notices the artifact is now closer to hand.

### Platform row selection — the host's OWN Soulprint facts, with the running binary as the fallback

★ ⚠ **CORRECTED against the shipped code (NIM-796). The decision recorded on 2026-09-04 was `runtime.GOOS`/`runtime.GOARCH` only, and the code is better than it.** Saying so plainly rather than pretending the decision anticipated it: the concern that drove the decision was real, and what shipped answers that concern head-on instead of routing around it. The superseded text is kept below, unedited.

**What ships.** `Module.hostPlatform` (`soul/internal/coremod/module/source.go:315-331`) reads the host's **own Soulprint facts** ([ADR-018](0018-soulprint-typed.md)) and falls back to the running binary — the same primary→fallback shape `util.ResolvePkgMgr` already uses for `core.pkg`:

```go
osName = runtime.GOOS
switch m.facts.OSFamily {
case "":                                   // unreadable os-release, or push mode with no collector
case "debian", "rhel", "alpine", "arch":  osName = "linux"
default:                                   osName = m.facts.OSFamily   // already a GOOS
}
arch = m.facts.Arch; if arch == "" { arch = runtime.GOARCH }
```

**The original reason survives intact — it is the reason for the collapse.** `os.family` is a **distribution** family on Linux where an artifact row names an **operating system**, so matching `{os: linux}` against `family` alone would refuse every row on every Linux host. The code's own comment states exactly that, and answers it: the four Linux families collapse to the single value `linux`, and **any other family is already a GOOS**, because Soulprint fills `family` from `runtime.GOOS` on every system with no `/etc/os-release` to read. The mapping is total in both directions, which is what makes reading the fact safe where reading it naively was not.

**Why the fact is better than the binary's own platform.** `runtime.GOOS`/`runtime.GOARCH` describe the **process**, not the host; the collected fact describes the host. They agree today and the fallback keeps them agreeing when there is no fact, but the value being selected on is now the same one every other host-shaped decision in the run is made from, rather than a second source of truth that could drift from it silently.

**The two supporting reasons recorded on 2026-09-04 are now stale.** They are struck rather than deleted, because a reader who finds them elsewhere should know how each was answered:

- ~~"`module.Deps` carries no facts"~~ — still literally true (`module.go:91-104`) and no longer an argument: the facts do not arrive through `Deps` at all. `Module` gained a `facts` field and a `SetHostFacts` method (`module.go:107-131`), so ~~"`coremod/module` does not implement `util.SoulprintAware`"~~ is simply **false now** — it does, and the ApplyRunner injects the collected Soulprint before `Apply` exactly as it does for `core.pkg` and `core.service` (`soul/internal/runtime/applyrunner.go:1062-1069`).
- ~~"the oneshot path never calls `SetHostFacts`, so facts would work in pull mode and fail in push"~~ — the premise still holds (`soul/cmd/soul/main.go:567` injects, the oneshot runner at `:786` does not) and the conclusion does not, because **the empty-fact case is handled explicitly** rather than left to fail. `case "":` is the first arm of the switch, and `TestApplyArtifactGrantWithoutFactsUsesTheRunningPlatform` (`installed_source_test.go:175`) pins it: a factless host picks its row by the running binary's platform and installs.

**No matching row → a closed refusal.** A grant that covers no row for this host's platform is a step failure, not a fallback to some other row and not an unverified install. The refusal **names what the release does cover** (`selectArtifact`, `source.go:207-215`), sorted so that two runs over one grant produce the same string: "no artifact for linux/arm64" alone leaves the operator unable to tell whether to change the host or the release.

<details>
<summary><strong>Superseded — the axis as recorded on 2026-09-04, before the code (kept verbatim)</strong></summary>

> ### Platform row selection — `runtime.GOOS` / `runtime.GOARCH`, read in the module
>
> **Decided 2026-09-04.** The row is selected by `runtime.GOOS` / `runtime.GOARCH`, read in-process by the module itself.
>
> **It is explicitly NOT `OsFacts.family`**, and this needs saying because the obvious reading of [ADR-018](0018-soulprint-typed.md) is wrong here. `family` is a **distribution** family — `debian / rhel / alpine / windows / darwin` (`proto/keeper/v1/soulprint.proto:57`) — derived on Linux from `/etc/os-release` (`soul/internal/soulprint/systemsource.go:61-77`). It is therefore **never** the string `linux`. A grant row written `{os: linux}` matched against `family` would refuse **every row on every Linux host**: a total failure, not a near miss, and one that would look like a grant problem rather than a selector problem. `runtime.GOOS`/`runtime.GOARCH` yield exactly `linux` / `amd64`, which is also the vocabulary a release's filenames already use.
>
> Two further reasons, each independently sufficient:
>
> - **The Soulprint route needs new plumbing that does not exist.** `soul/internal/coremod/module.Deps` carries `Sigils`, `Anchors`, `ModulesRoot` and `Rescan` — **no facts at all** — and `coremod/module` does not implement `util.SoulprintAware` (the implementors are `core.pkg` and `core.service`). Facts-based selection would mean wiring a new dependency through for a value the process can read from its own runtime.
> - **The oneshot path never calls `SetHostFacts`.** The daemon injects facts at `soul/cmd/soul/main.go:567`; `soul apply` builds its runner at `:786` and does not. A facts-based selection would therefore work in pull mode and fail in push mode, while an in-process `GOOS`/`GOARCH` read works in both.

**Why it was wrong, in one line:** it read "`family` is not a GOOS" as "the fact is unusable", when the fact is usable under a total mapping that costs four case labels — and the plumbing it called absent is the plumbing two other core modules already use.

</details>

### The transport rule is six-way, and Keeper does not stand in for everything

**Shipped (NIM-796), and recorded here because it is exactly the kind of rule an ADR exists to state.** The 2026-09-04 text said only "no matching row → a closed refusal", which is one arm of six. `Module.fetch` (`source.go:64-118`) applies them in this order:

| The grant / the source | Transport | Why |
|---|---|---|
| carries **no artifact rows** | Keeper (`FetchModule`) | exactly as before — this is the egress-free path and the shape every grant has today |
| carries rows, **one matches** this host | the source | the bytes never enter the Keeper cluster |
| carries rows, **none matches** this host | **refusal**, no fallback | not a transport problem: Keeper holds no bytes for that platform either, so a fallback could only turn a clear answer into a vague one |
| the source **did not answer**, an EventStream session exists | Keeper, **with a warning** carrying the source's error | the host that lost its egress, not a mode change |
| the source **did not answer**, no session | **refusal** naming that there was nothing to fall back to | push mode has no stream to ask |
| the source answered with the **wrong bytes**, or the grant's address is **unusable** | **refusal** (`errSourceUnusable`) | see below |

★ **The principle behind the whole table: Keeper can stand in for a source that is down, not for one serving something else, and not for a catalog field it cannot fix.** A fallback on wrong bytes would turn the one signal that a source was tampered with into a warning line under a green run; a fallback on a bad `base_url` or a 404 would hide an operator's typo behind a green run for as long as the catalog says so.

That distinction is why a non-2xx answer is **classified rather than lumped** (`statusError`, `source.go:195-201`): "the source did not answer" and "the source answered no" are different facts, and only the first is Keeper's to stand in for. A 4xx is an answer about *this* request — the path is not there, or the repository will not serve it anonymously — and Keeper cannot make a catalog row right. The two exceptions are the 4xx codes that mean "ask again": `408` and `429` are load, not a verdict. A 5xx is the source being down, which is the case the fallback exists for. `TestApplyArtifactGrantStatusDecidesTheFallback` (`installed_source_test.go:473`) pins the split.

Every one of these refusals reaches the operator as `module_fetch_failed` — step 3's reason code is unchanged, and the transport rule adds no new one.

### Observability: which transport a run used is in the final event

**Shipped (NIM-796).** `sendInstalled` (`installed.go:212-232`) puts on the final event:

- **`fetch_via`** — `source` or `keeper`;
- **`fetch_url`** — the address the bytes came from, present only when they came from an address;
- **`warnings`** — carrying the source's error when Keeper stood in for it.

The reason is stated rather than assumed: a rule that chooses between two transports has to be **answerable afterwards**, from the run record, without re-deriving the choice from the grant and a guess about what the network was doing at the time.

Two deliberate absences. On the idempotent no-op the transport keys are **not emitted at all** — nothing was fetched, and a `fetch_via` there would name a transport this run did not use. On the fall-back-to-Keeper arm there is **no `fetch_url`** — the bytes came from Keeper, and a URL beside `fetch_via=keeper` would name an address these bytes did not come from; the source that was tried is in the warning instead, with the reason it was not used.

### `FetchModule` REMAINS a second legitimate mode

**`FetchModule` is not deprecated, is not transitional, and is not scheduled for removal.** It is the delivery path for hosts with **no egress**, which is a permanent class of host rather than a migration state, and it is the reason this decision could be taken at all without cutting off air-gapped fleets.

This paragraph is written to be un-misreadable on purpose. A later session that reads source-pull as "the new way" and deletes `FetchModule` as dead code **breaks every air-gapped host**, silently at design time and loudly at apply time. If a future ticket proposes removing it, the thing to check is whether the egress-free host class has stopped existing — not whether source-pull covers the hosts in front of you.

### The condition that keeps the second mode honest: N artifacts per slot on the Keeper

Stating this rather than implying it, because the second mode does not actually work without it.

For `FetchModule` to serve a grant that covers N platforms, the **Keeper has to hold N artifacts per slot**. It does not today. `LookupModuleBinary` (`keeper/internal/sigil/lookup.go:34-64`) re-reads the alias's slot and **skips the row unless `slot.BinarySHA256 == sha`** (`:55-61`) — one executable per slot, one digest. With a grant covering N platforms and one executable in the slot, **N−1 fetches can never succeed**, and they fail as `module is not allowed` — a message that points at the grant, which is the wrong place to look.

Three consequences follow, all on the Keeper side:

- **The Keeper downloads all N anyway.** It has to hash them to sign them; there is no signing an artifact list it has not read.
- **`plugins.max_artifact_size_mb` applies N times** rather than once (`keeper/internal/grpc/fetchmodule.go:122-125`, defaulting via `config.DefaultPluginMaxArtifactSizeMB`). The per-artifact ceiling is unchanged; the per-grant total is not.
- **The "single executable in `dist/`" convention becomes kind-scoped.** It is stated as universal at [ADR-020's 2026-08-06 amendment](0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name); an artifact-kind entry with N platform binaries cannot satisfy it. See the [ADR-020 amendment](0020-plugin-infrastructure.md#amendment-2026-09-04-nim-794-the-catalog-entry-gains-a-source-kind-and-an-explicit-artifact-list) of this date.

**(b)'s "NO new storage" holds only on this condition** — no third store is introduced, but the existing FS cache grows an axis.

### The Soul-side slot needs NO change

Said explicitly so that NIM-796 would not "improve" it symmetrically and break discovery — **and it held: NIM-796 did not touch the slot.**

`<paths.modules>/<alias>/` holds **one** executable, and `removeForeignArtifacts` (`soul/internal/coremod/module/installed.go:172-192`) enforces it by deleting every other executable in the slot on install — because "a slot holds exactly ONE ... discovery would refuse it rather than guess which is current" (`:150-155`). The Soul installs **only its own row**, so one executable is exactly right on that side. The N-artifacts condition above is a **Keeper-side** statement about the Keeper's cache; carrying it across to the Soul slot would make discovery ambiguous and the slot would be refused.

### Egress is not a new dependency class

The dependency class "a managed host fetches an artifact from a package repository over the network" is **already shipped** and is not introduced by this decision. A downstream redis service installs `redis-server` from an apt repository declared in its service vars, and the shipped default is a **public internet** repository — `install_package.repo_uri = https://packages.redis.io/deb` at `vars/00-base.yaml:63` of the downstream redis service, a separate repository which this one does not contain. A fleet without direct internet access is already expected to override that map with its own mirror, which is exactly the shape an artifact-kind plugin entry takes.

What is new is therefore not host egress. It is host egress **for executable plugin bytes** — which is precisely why the safety argument rests on the digest gate rather than on the network path ([ADR-026](0026-sigil.md#amendment-2026-09-04-nim-794-the-grant-carries-a-list-of-artifacts-and-the-bytes-stop-travelling-through-the-keeper)). This is encouraging rather than conclusive: it still needs live proof.

### Two safety consequences, one of them now CLOSED

**(i) Verification happens in memory, and an implementation can still lose that. HELD.** The sequence is fetch (`installed.go:98`) → verify (`:106`) → install (`:112`), all on a `[]byte`: **unverified bytes never touch disk.** NIM-796 kept it — the source path reads the body into memory (`io.ReadAll`, `source.go:166`) precisely so that verify runs before anything is materialized, and the size ceiling below is what makes holding it in memory affordable. An implementation that later streams a large artifact to a temp file to avoid holding it gives the property up — unverified executable bytes would sit on the host filesystem, however briefly — and that would be a deliberate trade to be recorded, not a detail. It also gives [ADR-026(g)](0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)'s deferred `noexec`-on-the-slot hardening a **second and much larger host class** than the Keeper it was written for.

**(ii) ⚠ The missing Soul-side size ceiling is CLOSED (NIM-796).** This section recorded it as an open gap for NIM-796 to fill; it was filled in the same change, and the paragraph is corrected rather than left standing.

`maxArtifactBytes` (`source.go:28-38`) bounds the source pull. What shipped, precisely:

- **It is a constant, not a configurable field.** `soul.yml` gained no ceiling of its own. Stating this plainly because the earlier text asked for a field: the value is `config.DefaultPluginMaxArtifactSizeMB` (256 MiB) compiled in.
- **It reuses Keeper's DEFAULT value as its own constant — it is not a shared ceiling.** Keeper's own is configurable (`plugins.max_artifact_size_mb`) and enforced by the sender; Soul has no config to read here. So a cluster that **raised** its ceiling has artifacts this path refuses and `FetchModule` would serve. That asymmetry is deliberate: refusing is the safe direction for a number that bounds an allocation an untrusted source controls, and the fix if it ever bites is a Soul-side knob, not a wider default.
- **The read is `cap+1`, and the reason is a diagnostic one.** Reading exactly the cap cannot tell a file *at* the limit from one *truncated* at it, and a truncated artifact fails verify with a digest mismatch — the diagnostic for tampering, printed over a file that was merely large.
- **Over the cap is `errSourceUnusable`, not a fallback.** A source serving more than the host will accept is a catalog fact Keeper cannot fix.

**`fetchAll` itself is still unbounded, and that remains correct.** `fetchAll` (`installed.go:124-144`) accumulates chunks into a `bytes.Buffer` with no cap of its own; it is safe because its peer is the Keeper, which caps what it will send (`keeper/internal/grpc/fetchmodule.go:122-125`). The ceiling was needed on the path whose peer is **not** the Keeper, and that is where it went.

### The three security decisions the source pull is built on

**Shipped (NIM-796), and recorded because they are exactly what a later reader will ask about.** None of them was in the 2026-09-04 text; each is a decision rather than an implementation detail.

**(a) The HTTP client is `core.url`'s, and so is the timeout.** The source pull builds its client from `util.NewHTTPClient` — the same constructor `core.url` and `core.http` build theirs from — and reads `util.DefaultFetchTimeout` (`soul/internal/coremod/util/httpx.go:30-34`), which `core.url` now reads too instead of its own local copy. The point is not reuse for its own sake: **redirect policy, TLS posture and timeout behaviour cannot diverge between two core modules that fetch**, and a per-module copy of the number is how they would. The client is a `Module` field (`module.go:111-116`) so tests can swap it; production takes the default.

**(b) The SSRF dial guard is lifted for this path, deliberately — and nothing else is.** `sourceClientOpts` sets `AllowPrivate: true` (`source.go:40-49`). This is the one `core.url` default that cannot hold here, and the reason is **where the URL comes from**: a `core.url` task's URL is **run input**, while a `base_url` is **catalog configuration an Archon wrote** and the grant carries. The deployment this exists for is an artifact repository inside the perimeter, answering on an RFC1918 address the dial guard would refuse — so with the guard in place the feature does not work at all in its intended deployment.

Recording the reasoning and not just the fact, because "we turned the SSRF guard off" reads as a weakening and is not one:

- **Every other guard stays, https-only included.** `util.ValidateURL` runs on the joined URL before the request is built (`source.go:143-145`), and it is the same `netguard.ValidateHTTPSURL` the rest of the platform uses — `http://`, `file://` and the rest are refused. The redirect cap and the no-downgrade redirect policy are untouched.
- **The verify still gates the disk.** The guard that actually protects the install is step 4, not the dial guard: bytes are Sigil-verified before they reach the slot, so a source that serves anything else earns a refusal rather than an exec. The dial guard was never what stood between an untrusted source and code execution.
- **The lift is scoped to this client.** It is a package-level `var` for this one call site; no other core module's client changes.

**(c) The path is joined, never interpolated.** `artifactURL` (`source.go:240-281`) concatenates `base_url` and the row's `path` and refuses anything that could steer the address elsewhere, rather than normalizing it. This is the **mechanical half** of "there is deliberately no path template" — argued in [ADR-026's amendment of this date](0026-sigil.md#there-is-deliberately-no-path-template) and restated catalog-side in [ADR-020's](0020-plugin-infrastructure.md#there-is-deliberately-no-path-template): a URL is where executable bytes come from, an expressive template there turns an address into a program, and this is the code that makes the absence of a template enforceable rather than merely conventional.

Refused in the **path**, checked both as written and percent-decoded — because the server is what resolves the path, and `%2e%2e%2f` arrives there as the traversal the literal check just refused:

- an absolute path, or a URL of its own (`://`);
- a `.` or `..` segment;
- a query or a fragment;
- a backslash — Go does not treat it as a separator, so `..\..\x` would pass the segment scan and reach a server that does.

Refused in the **`base_url`**: no host (`https:/repo.internal/...` with one slash parses and would otherwise reach the client as a *transport*-shaped error, which is the shape that falls back to Keeper and hides the typo); embedded credentials (source authentication is deferred by NIM-793, and carrying them would also put a password into `fetch_url`, i.e. into `RunResult` and OTel); a query or fragment (concatenating onto a query fetches the base instead — every row, silently, the same wrong file).

The `base_url` is **parsed, not pattern-matched**: `@` and `?` mean one thing in an authority and another in a path, and a check that cannot tell them apart refuses `https://registry.internal/@souls/plugins` for carrying credentials it does not. `TestArtifactPathIsJoinedNotSteered` (`installed_source_test.go:353`) and `TestApplyArtifactGrantAcceptsAtSignInBaseURLPath` (`:411`) pin both halves.

### Push mode: the transport half opened, the decision half did NOT

The step used to refuse push mode outright, and the message said why: *"FetchModule is unavailable in this run (no EventStream session; push mode is not supported)"*. That was a statement about the **transport**, not about the step — there is no stream in oneshot, so there was nothing to fetch over.

**Shipped, and the split is now visible in the code.** That refusal survives verbatim, but only on the arm it actually describes: `fetchViaStream` (`source.go:120-129`) raises it when Keeper is the transport and there is no session. A grant carrying artifact rows does not reach it — the source pull needs no stream — and `TestApplyArtifactGrantInstallsInPushMode` (`installed_source_test.go:196`) pins that a session-less run installs and reports `fetch_via=source`. The facts gap that would otherwise have blocked oneshot does not block it either, for the reason given above: no fact means the running binary's platform, handled explicitly rather than left to fail.

**⚠ The question that was open is still open, and it is now the ONLY thing blocking push-mode install.** Where the grant set comes from with no EventStream to have delivered a `SigilSnapshot` is untouched by NIM-796, and it is not a theoretical gap: `soul/cmd/soul/main.go:652-653` says in its own comment that **in push mode sigils and anchors are `nil`, so the install step fails closed with `module_not_allowed`** — at step 1, before any transport is chosen. So the honest statement of what shipped is: the **transport** half of the push-mode refusal is gone and guarded; the **grant-provenance** half is what still stops the step, one step earlier and for a different and better reason.

That is the right failure while the question is unanswered — fail-closed at the allow-check is exactly where a host with no grant set should stop. What must not happen is a later change wiring a grant set into oneshot **as a side effect** of noticing the transport now works. Push mode installing plugins is still explicitly **not decided**.

### What is half-wired, and what NIM-795 inherits

★ ⚠ **This is the single most important paragraph here for NIM-795, and it describes a live limitation of what shipped — not a plan.**

The grant's **wire form** belongs to NIM-795, so NIM-796 stopped at the **read-side DTO**:

- `SigilRecord` gained `BaseURL` and `Artifacts []SigilArtifact` (`shared/pluginhost/sigil_verify.go:48-79`). Empty `Artifacts` means the grant names no artifacts and the bytes come from Keeper — which is every grant that exists today.
- **Nothing fills them from the proto.** `PluginSigil` carries no artifact rows, and neither the Soul-side adapter nor the Keeper's lister populates the two fields. They are reachable in tests and from nowhere else.
- **Verify does not read them.** The signed block is still `BuildSigilBlock(source, ref, binary_sha256, schema_sha256)`. Making it cover the row list means changing what Keeper **signs**, and sign and verify are one helper on purpose — so both ends move together or neither does.

**The consequence is a shipped limitation, and it is fail-closed.** Until the signed block covers the row list, a grant installs **only on the platform whose row digest equals `BinarySHA256hex`**. Every other row **fetches, and then fails closed at verify** with a digest mismatch.

That is the safe direction, and it is the reason the fetch comes last in the ordering this amendment opened by defending: a row that is not the signed one costs a wasted download and produces a refusal, never an install. It is nonetheless a real limitation of a multi-platform grant on this tree, so it is recorded here and in [known-limitations.md](../known-limitations.md) rather than left to be discovered as a digest-mismatch on one architecture and not another.

**What NIM-795 has to carry, therefore:** the artifact rows into the proto and into the signed block (DST `soul-stack/sigil/v3`, [ADR-026](0026-sigil.md#the-dst-goes-to-soul-stacksigilv3-and-this-is-forced)), the catalog entry that produces them ([ADR-020](0020-plugin-infrastructure.md#the-catalog-entry)), the keeper-side N-artifacts-per-slot condition stated above, and the two open forks neither ticket may settle silently — what fills `source` for an artifact-kind entry, and N stamped schema trailers against one signed `schema_sha256`.

### Unchanged and reinforced

The rejection of **"bytes over EventStream"** in Rejected alternatives above is unchanged, and this decision strengthens it. The epic rejected Keeper-proxying on the same argument one level up: artifact bytes do not belong in the control plane, and a shared package proxy already exists for them.

## Amendment (2026-09-05, NIM-829): a `modules[]` entry declares an ARTIFACT, not one of its objects

The [2026-08-07 amendment](#amendment-2026-08-07-nim-524-modules-declares-an-address-the-synthesized-step-takes-level-1) settled that the synthesized step takes address **level 1** and left the declaration as an address — "the declaration stays an address (`<alias>.<module>` — what the scenarios write)". That half is now wrong, and it was wrong on the terms the same amendment used: level 2 named *one of the modules that artifact serves* under the address model, and [NIM-765](../naming-rules.md#the-discipline-binding-the-three-levels) replaced it with `<plugin>.<object>.<action>`, where level 2 is an **object inside the one binary**. The address grammar moved; the declaration form did not, and a service listing six objects of `soul-mod-redis` was declaring one artifact six times.

**The entry is the registration alias, and nothing else.** An entry pairs a `name` with a `ref`; a `ref` pins an artifact, `core.module.installed` fetches an artifact, and a slot holds an artifact. The objects arrive with it and were never separately installable — which is why every one of those six rows already collapsed into one install. The row is now what installs, spelled once:

```yaml
modules:
  - { name: redis, ref: v1.0.0 }
```

**`conflicting_module_ref` is a symptom, and it is retired with the form it guards.** That diagnostic ([2026-08-07](#amendment-2026-08-07-nim-524-modules-declares-an-address-the-synthesized-step-takes-level-1)) exists only because the old form lets an author write `redis.cluster ref v1.0.0` beside `redis.user ref v2.0.0` — one binary in two versions, a state with no meaning that the engine has to catch. One row per artifact cannot express it. The general rule, recorded here because it generalises past this field: **when the engine has to guard a state that should not exist, suspect the form rather than a missing guard.** The check comes off with the two-level form and not before — while both forms are accepted a manifest can be written half-migrated, and that is precisely the shape the guard still catches.

**Transition window.** The single-segment form is canonical from this amendment. The two-level form is accepted with the warning `module_name_two_level_deprecated` (one per entry, naming the alias to collapse to) until **2026-12-01**, when it becomes an error and `conflicting_module_ref` is removed with it (NIM-836). Both forms mean the same thing for the window's length: they synthesize the same single install at the same position.

**A rider, and the reason the two forms can be equivalent at all: consumers are matched on the alias.** The synthesizer keyed its consumer map on the whole two-level entry, so `modules: [redis.instance]` under a plan calling `redis.sentinel.present` synthesized **nothing** — a plan's correctness depended on the manifest enumerating every object it touched, which is not a property any author was told to maintain. Both sides now reduce to level 1, matching what the takeover comparison has done since [NIM-543](#amendment-2026-08-26-nim-543-the-takeover-key-is-address-level-1-on-both-sides-and-a-mis-spelled-paramsname-is-caught-offline).

**Nothing loses information.** Level 2 in `service.yml` had exactly one consumer — this synthesis — and it was the wrong key for it. The plugin-params check (NIM-790) takes its object list from the plugin's **own** manifest (`keeper/internal/artifact/module_manifests.go`, keyed `<alias>.<module>` from the registration grant plus the artifact's schema document), never from the service manifest; `ListDependencies` passes the name through to the UI unread. `destiny.yml::required_modules[]` shares the regex and therefore admits the shorter form too — deliberately, and **not** deprecated there: that list carries no `ref` and synthesizes nothing, so naming the object is a more precise statement with no contradictory state behind it. Widening a grammar and deprecating a form are two acts, and only `modules[]` gets the second.
