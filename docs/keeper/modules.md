# Keeper-side core modules

The vast majority of core modules are **Soul-side** (executed on the host `soul` binary: `pkg`, `file`, `service`, `user`, `exec`, `cmd`, `cron`, ...; see [architecture.md → "Module model"](../architecture.md)). Some of the core modules are **Keeper-side**: they operate on keeper registries (Postgres `souls`+coven, Redis cache, logs) and are executed on the keeper itself. The normative specification of Keeper-side core modules is collected here.

## Soul-side / Keeper-side dispatcher - `on:`

Addressing (`<namespace>.<module>.<state>`) and SoulModule contract are the same for both sides. The difference is **where the step is performed**; this is solved by the scenario key `on:` ([scenario/orchestration.md §3](../scenario/orchestration.md)):

| `on:` | Where is it performed | Suitable for modules |
|---|---|---|
| omitted / `[coven, …]` | on incarnation hosts | Soul-side core (`core.pkg.installed`, `core.file.present`, ...) |
| **omitted** (keeper-side module address) | on the keeper itself | Keeper-side core (`core.soul.registered`, `core.bootstrap.issued` - tokens for ready-made VMs, `core.bootstrap.delivered` - token delivery via SSH, ...) |

**The side is the module's, and a task does not restate it** (NIM-747). The six keeper-side core base addresses - `core.bootstrap` / `core.cert` / `core.choir` / `core.soul` / `core.state` / `core.vault` - are disjoint from the twenty-one Soul-side ones, so the address alone routes the step. `on: keeper` on one of them is a validation error (`on_keeper_redundant`), and a Soul-side address is a host task however it is written. The catalog is one list, `shared/coremanifest`, read by both `soul-lint` and the render pipeline; a plugin declares its own side in its schema document (`side: keeper | soul`, default `soul`), and since NIM-758 the Keeper **routes by it** - see [Keeper-side plugin modules](#keeper-side-plugin-modules) below. `on: keeper` on a PLUGIN address stays legal regardless: a scenario cannot read the artifact's document, so unlike a core address it has nothing to derive the side from.

★ **`core.cloud` has left this list, and `side: keeper` is what replaced it** (epic NIM-757, decided 2026-09-01; removed in **NIM-761**). Every CloudDriver already *was* a plugin, so the separate contract is gone and a cloud driver is an ordinary SoulModule plugin declaring `side: keeper` - which is exactly the field above. The precondition, that the Keeper could execute such a plugin at all (**NIM-758**, earlier **NIM-688**), is closed in both halves: `applyKeeperTask` falls back from the `coremod.Registry` to the discovered plugins, and `Host.SpawnSoulModule` may start a `soul_module` artifact that declares the keeper side (the kind-agnostic `Host.Spawn` refuses every kind but `ssh_provider`, so that gate has no way around it). Full decision - [ADR-017 amendment 2026-09-01](../adr/0017-keeper-side-core.md#amendment-2026-09-01-nim-757-the-clouddriver-contract-is-removed--a-cloud-driver-is-an-ordinary-plugin).

## Registration and dispatch at (`base` + `state`)

Keeper-side core modules are registered in the keeper-side Registry (`keeper/internal/coremod/registry.go`) by **base name** - `<namespace>.<module>` without state suffix: `core.soul`, `core.bootstrap`, `core.choir`, `core.vault`, `core.state`, `core.cert`. State comes from the last segment of the task address.

When executing a keeper-side task (`keeper/internal/scenario/keeper_dispatch.go`), the address `module: <namespace>.<module>.<state>` is divided by the function `config.SplitModuleAddr` (a single parser for both sides, the same as the Soul-side runtime) into a pair `(base, state)`:

- `base` (`core.vault`) goes to `Registry.Lookup` - finds the implementation of `SoulModule`;
- `state` (`created`) is placed in `ApplyRequest.state` and dispatched **inside** the module implementation.

Author-form examples → parsing:

| Task address (`module:`) | Registry-key (`base`) | `ApplyRequest.state` |
|---|---|---|
| `core.soul.registered` | `core.soul` | `registered` |
| `core.bootstrap.issued` / `core.bootstrap.delivered` | `core.bootstrap` | `issued` / `delivered` |
| `core.choir.present` / `core.choir.absent` | `core.choir` | `present` / `absent` |
| `core.vault.kv-read` / `core.vault.kv-present` | `core.vault` | `kv-read` / `kv-present` |
| `core.state.set` / `.present` / `.add` / `.append` / `.modify` / `.remove` / `.unset` | `core.state` | the address suffix, one per [ADR-057](../adr/0057-state-changes-crud-verbs.md) verb |
| `core.cert.registered` / `core.cert.issued` | `core.cert` | `registered` / `issued` |

Defective address (`SplitModuleAddr` returned `ok=false`: empty, `.state`, `core.`) - the keeper-task crashes, like Soul-side on an unknown module. A `base` that is not in the Registry is **not** the end of the lookup since NIM-758: it is tried against the keeper-side plugin modules next (see [Keeper-side plugin modules](#keeper-side-plugin-modules)), and only an address neither knows gives the `failed`-event "unknown keeper-side module". Core is asked first, so a plugin registered under a core address cannot shadow the module that really runs. Registration of a module in the Registry is conditional based on the presence of its dependency in `coremod.Deps`: `core.choir` is connected only when `ChoirStore` is specified, `core.state` only when the Vault client is. `core.bootstrap` is present when either its Postgres issuer or its delivery dependencies exist; production always wires the issuer. An unavailable state fails explicitly (`issuer not configured` / `dialer not configured`) instead of borrowing dependencies from another state.

### Audit-trace and per-task alerting

Each keeper-side task writes audit-event `task.executed` (symmetrically to Soul-side handler `TaskEvent`): `sid = keeper` (address of the keeper-target of the run), `correlation_id = apply_id`, `source: keeper_internal`, `payload.status` - name `keeperv1.TaskStatus` (`changed → TASK_STATUS_CHANGED` / `failed → TASK_STATUS_FAILED` / otherwise `TASK_STATUS_OK`). Thanks to this, **task:-Tiding subscription also works for keeper-side addresses**: a keeper task with the address `register ∪ id` (including `provision_vm` with `id:` without `register:`) ends up in `changed_tasks` of the terminal event `incarnation.run_completed` and is matched by the task selector ([ADR-052 amend §k/§l](../adr/0052-herald-notifications.md)). Secret hygiene: keeper-side `task.executed` carries only address + status (without `register_data`/output); `error.message` - only on failure (nothing suppresses it per task since [ADR-0083](../adr/0083-declared-secret-state-fields.md) §8 removed `no_log:`; the write-path vault-ref masking still applies). Operator-SSE keeper-side does not broadcast progress.

### The context of `params:` is `incarnation.state`, but not `soulprint`

Keeper-side task is executed on the keeper itself - it has no hosts. Therefore, `params:` are rendered in a **run-level** context (once per run, not per-host): `input.*` / `vars.*` / `incarnation.*` / `register.*` are available (from previous keeper tasks), but **not** `soulprint.self` / `soulprint.hosts` - access to them in `params` keeper task fails the render (there are no host facts, and this is correct: the keeper step operates with registries, not facts of a specific VM). The two fail differently: `soulprint.self.<path>` gives the standard CEL `no such key`, while `soulprint.hosts` is refused by name - `construct soulprint.hosts (scenario-only; not available in destiny pass) not yet implemented (pilot)`.

In `incarnation.*` the key **`incarnation.state.<path>`** is available - read-only **pre-run snapshot** `incarnation.state` (the same `stateBefore` for row-lock runs, symmetrically for Soul-side tasks). The snapshot is invariant within the run (fixed once, does not accumulate between passages). This allows the keeper-side task to read facts written by the previous run: for example, a teardown step in the `destroy` scenario takes the resource identifiers from `incarnation.state.*` written by the create run. If the incarnation does not yet have a state (push/trial without it) - `incarnation.state.<x>` gives `no such key`; defend reading `default(incarnation.state.<path>, …)` where fact may be missing.

### Flow control on a keeper-side task

A keeper-side task is executed by the keeper's own scenario runner, which walks the plan in order and evaluates no flow-control predicate. Everything a Soul-side runner answers - `when:` / `changed_when:` / `failed_when:` / `onchanges:` / `onfail:` / `retry:`+`until:` - rides the `RenderedTask` for symmetry and is read by nobody keeper-side. The keys whose being dropped would change what actually runs are therefore refused outright rather than accepted and ignored:

| Key on a keeper-side task | Answer |
|---|---|
| `async:` | **Refused** (`async_on_keeper_invalid`) - Soul-side task concurrency, no meaning off a Soul ([destiny/tasks.md §6](../destiny/tasks.md)). |
| `loop:` · `apply:` | **Refused** - a keeper task is module-only in the pilot. A capture over a runtime-sized collection is written out one step per element ([NIM-709](../adr/0084-explicit-state-capture.md)). |
| `block:` | **Refused** (`block_on_keeper_invalid`) - a block fans its children out over the run's hosts, and the keeper is not one of them. Refused at **both** levels: on the block, and on a keeper-side task nested inside one - the child needs no `on:` to be caught, since its module address is what decides. Keeper-side tasks go flat, in the scenario's own task list. |
| `when:` **static** (`input.` / `vars.` / `incarnation.`) | **Honoured.** The keeper settles it at render, before the task is routed keeper-side: false collapses the step to a skip placeholder, true renders it normally. This is the working form. |
| `when:` reading `register.*` / `soulprint.*` | **Refused** (`when_on_keeper_dynamic_unsupported`, [ADR-0084](../adr/0084-explicit-state-capture.md) F-D) - see below. |
| `require:` | **Accepted, redundant** - the keeper runs its tasks in plan order, so the barrier is already satisfied; threading it keeps its names under the unknown-register check. |
| `register:` · `id:` | **Accepted** - the task's register is accumulated under the keeper target and is readable by a **later keeper task** (`register.*` in the run-level context above). |

**A `when:` that reads `register.*` or `soulprint.*` is an error**, not a slow path. `when:` is a Soul-side predicate: it is evaluated in the Soul's own flow-control sandbox, and a keeper task never reaches a Soul. Until it was refused, the key was accepted and dropped on the floor - the file said the step was conditional and the step ran every time. Since [ADR-0084](../adr/0084-explicit-state-capture.md) made `core.state.<verb>` the only writer of incarnation state, the step that runs every time is the step that **writes state**, which is why this is an ERROR at parse (`soul-lint`, with a line and a column) with a fail-closed backstop at render.

Two replacements, both working today:

```yaml
# 1. The condition moves INSIDE the value - evaluated at render, in the keeper
#    env, where a PREVIOUS keeper task's register is bound.
- name: Record the provisioning outcome
  module: core.state.set
  params:
    field: tier
    value: "${ register.provision.changed ? 'fresh' : 'reused' }"

# 2. The step must not run AT ALL - then it belongs on the Soul side, where
#    `when:` is evaluated.
- name: Warm the cache only where the probe found it cold
  when: register.probe.stdout == 'cold'
  module: core.exec.run
  params: { cmd: warm-cache }
```

A keeper task's `register.*` context holds **keeper** tasks only. A capture that reaches for a **host** task's register does not silently read an empty value - the run aborts (`error_locked`, the field left unwritten, the abort reason named). Reaching a host-derived value into `incarnation.state` is not expressible today; the value has to come from a keeper task.

## Keeper-side plugin modules

**A keeper-side module does not have to be built in** (NIM-758, closing NIM-688). A task address the core Registry does not know is looked up among the plugins this Keeper discovered and allow-listed, and a module whose schema document declares **`side: keeper`** ([ADR-0087](../adr/0087-task-side-derived-from-module-address.md), [plugins.md → per-module fields](plugins.md#per-module-fields)) executes locally, through the same gRPC-over-stdio infrastructure the Soul host uses. The SoulModule contract is one contract for both sides ([ADR-009](../adr/0009-scenario-dsl.md)/[ADR-017](../adr/0017-keeper-side-core.md)); only the place of execution differs, and a keeper-side plugin therefore follows every rule this page states for a keeper-side core module — run-level `params:` context, one `apply_runs` row per Passage, the same `register:` accumulation and audit trail.

The address is the plugin's, not the core namespace's: `<alias>.<module>.<state>`, where level 1 is the **registration alias** the operator chose ([plugins.md → registration alias](plugins.md#registration-alias)). `wb-cloud.vm.created` resolves to `(base=wb-cloud.vm, state=created)` exactly as a core address does.

**Three answers, and the difference between the last two is the point:**

| The address is… | Answer |
|---|---|
| a discovered plugin declaring `side: keeper` | executed on the Keeper |
| a discovered plugin declaring `side: soul` (or omitting the key) | `plugin module "<base>" declares side=soul and does not execute on the keeper: a keeper-side step needs 'side: keeper' in the module's schema document` |
| known to neither registry | `unknown keeper-side module "<addr>"` — unchanged, and **loud**, never a silent skip |

A Soul-side module addressed by a keeper-side step must not be answered "unknown module" (the module exists; the author would go hunting a typo that is not there) and must not quietly leave for a host (nothing would run it there either — the step is keeper-side because there are no hosts yet).

`on: keeper` on a plugin address **stays legal and is still required**. Unlike a core address, whose side the catalog in `shared/coremanifest` settles offline, a scenario cannot read a plugin's schema document — the artifact lives in the Keeper's cache, not in the service repo — so the address alone does not tell the render pipeline where the step goes. That is the boundary [ADR-0087](../adr/0087-task-side-derived-from-module-address.md)(f) drew, and NIM-758 does not move it.

### The trust boundary, and the masking that follows from it

⚠ **This is the most privileged way to execute anything the platform has** — a foreign binary inside the Keeper's process tree rather than on a host. It stands at the door the neighbouring kinds already stand at, plus one bolt, and none of them is weakened:

- the artifact is in `keeper.yml::plugins.soul_modules` (the catalog filter — an undeclared slot is not discovered; ⚠ that entry gains `source_kind` with the [2026-09-04 amendment](../adr/0020-plugin-infrastructure.md#amendment-2026-09-04-nim-794-the-catalog-entry-gains-a-source-kind-and-an-explicit-artifact-list), NIM-794, not implemented — the filter itself is unchanged, and whether a `side: keeper` plugin's bytes are also source-pulled is recorded as **open** in [known-limitations.md](../known-limitations.md));
- its declared capabilities pass `plugin_runtime.allowed_capabilities`;
- its sha256 matches an **active Sigil grant**, verified before the exec ([ADR-026](../adr/0026-sigil.md)) — the one real control;
- and its module declares `side: keeper`. That declaration lives INSIDE the schema document, which the Sigil seal signs together with the binary's digest ([ADR-026(c)](../adr/0026-sigil.md)), so it cannot be flipped without breaking the signature. The check sits in two places — the registry never hands back a Soul-side module, and `Host.SpawnSoulModule` refuses to fork one — because a single gate here would be one too few. The kind-agnostic `Host.Spawn` keeps refusing `soul_module` outright, so it is not a way around either.

Nothing confines the process once it starts; that bound is the same one [ADR-020](../adr/0020-plugin-infrastructure.md) states for every kind, and it is why the digest, not the disclosure, is the control.

★ **Secrets reach such a plugin as PARAMS** (epic NIM-757, decided 2026-09-01): it has no Vault access of its own and never gets one. The consequence is a masking obligation on this path — a module that echoes what it was handed would turn its own message into a leak, and a failed task's message is observable four ways (audit `task.executed` inside `error`, `apply_runs.error_summary`, the run's abort reason, and the operator's view of each). So a keeper task's result message is run through the **per-cell seal** before it becomes any of them: every **string** value the task's params carry at or beneath a sealed path ([ADR-010](../adr/0010-templating.md) §7.4 — the cells whose expression read `${ vault(…) }`, a secret input, or a sealed register) is replaced by `***MASKED***`, and the rest of the diagnostic is left readable. *At or beneath*, because a sealed cell often resolves to a subtree rather than a string — a bare `vault:<mount>/<path>` with no `#field` hands back the whole KV map — and masking only the top would mask nothing at all on exactly that shape. The write-path maskers cannot do this job: `MaskSecrets` catches a sensitive key NAME or a `vault:` ref, and a resolved credential quoted mid-sentence is neither.

**What this does not cover, stated rather than implied:**

- a credential the module invents at runtime, or one written as a plaintext literal in the scenario — the render never saw it as a secret, and it is not distinguishable from any other text;
- ⚠ **a secret that reaches the params through a `vars:` hop.** `params: { pw: "${ vars.db_pw }" }` is NOT sealed even when `vars.db_pw` is itself `${ vault(…) }`: the seal's `SealedVars` / `SealedCompute` sources are never populated by anything in the tree, so the detector sees an ordinary `vars.*` read. This is a predating gap in the seal ([ADR-010](../adr/0010-templating.md) §7.4), not one this path opened — but routing a secret through `vars:` is the commonest idiom in `examples/`, so in practice it is the largest bound on this masking;
- a **non-string** scalar at a sealed path. A numeric secret is `***MASKED***` in a payload (which masks the cell whatever its type) and legible in a message. Widening the walk is not the fix — `87654321` is a plausible substring of a byte count or a timestamp, and masking numbers out of free text is a decision of its own;
- a **fragment** of a mixed-interpolation cell: `url: "postgres://u:${ vault('s#pw') }@h/db"` seals and collects the whole rendered URL, so a module reporting `password rejected: hunter2` is not masked — the secret is a substring of what was collected, not equal to it;
- the module's **`output`**, which is not the message. A keeper task's `register:` is written to `apply_task_register` verbatim on purpose — it is the value the next task reads through `register.<name>.<field>`, and redacting it would break the chain rather than protect it. ⚠ **`secret: true` on an output field does not change that write.** What the declaration buys keeper-side is that the register NAME is sealed ([ADR-0083](../adr/0083-declared-secret-state-fields.md) §8), so a later cell reading `${ register.<n>.<f> }` is masked where *it* becomes observable. A plugin that echoes a credential into its `output` still lands it in the register row in plaintext.

**And the cost of the rule, since it is paid on every message:** a sealed cell contributes the non-secret siblings of its subtree too — a `creds` map from Vault brings `user`, `host` and `port` along with the password — and the seal set is run-wide, so those are redacted from *every* keeper task's message in the run, core modules included. `await timeout for ***` instead of a hostname is the price of not leaking the password beside it.

## `core.soul.registered`

**The first Keeper-side core module.** Binds a Soul (by `SID`) as a **member of the run's incarnation** and optionally assigns a set of **real stable Coven tags** in the keeper's registry (tables `souls` + `incarnation_membership` + coven, [storage.md](storage.md)). **Membership is set implicitly** from the current run's incarnation (cross-incarnation binding is forbidden by the grammar, so the target is unambiguous) — it lives in the first-class relation `incarnation_membership`, **not** in `souls.coven[]` ([ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)). Accepts a **string OR list of SIDs** (registering N created hosts in one step) and optionally carries an **onboarding barrier** `await_online` (blockingly waits for registered Souls to become online) - [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md).

### Addressing and side

- Namespace: `core`. Module: `soul`. State: `registered`.
- Full task name: `module: core.soul.registered`.
- Side: **Keeper-side**, derived from the module address. The step carries **no** `on:` key - writing `on: keeper` on it is an error (`on_keeper_redundant`, NIM-747).

### State (state form)

`registered` - declarative form: "Soul with the specified `sid` is in the registry, is a **member** of the run's incarnation, and carries the specified set of stable Coven tags (if any)." The module is idempotent by design (re-calling with the same set is no-op).

If there is no entry in `souls` for this `sid` yet, the module creates it under `status: pending` (a new host added by the scenario - for example, host branch `add_replica`, or after a `side: keeper` plugin created the VM). Side-effects: the `souls` entry (if new), the **membership row** in `incarnation_membership` for the run's incarnation, and the optional stable-coven update. The module does not issue bootstrap tokens and does not launch a CSR cycle (this is the responsibility of onboarding, [soul/onboarding.md](../soul/onboarding.md)).

In list form `sid` (see list-SID), membership and the passed set `coven` are applied to **each** list SID; The `await_online` barrier (if specified) aggregates presence over the **entire** set.

### Parameters (`params:`)

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `sid` | string **or** array of string, `format: fqdn` | required | — | `SID` Soul (FQDN) to which the binding is applied. Accepts **single string OR list** ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md), see list-SID). The list in practice comes with the CEL expression `${ register.<provision>.hosts }` (SID list from the step that created the VMs); literal list `sid: [a, b]` statically `soul-lint` **doesn't** pass (manifest declares `sid` as `string`) - this is a deliberate trade-off, see list-SID. |
| `coven` | array of string, `pattern: "^[a-z][a-z0-9-]*$"`, `unique: true` | optional | `[]` | A set of **real stable** Coven tags (cluster / project / environment / datacenter). **May be empty** — membership no longer lives in `coven[]`, so a bind needs no coven tag. When listed, `sid` applies to each SID. |
| `mode` | string, `enum: [append, replace, remove]` | optional | `append` | Strategy for applying the `coven` set to existing labels (see below). |
| `refresh_soulprint` | boolean | optional | `false` | **Implemented (S2/S3 [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)).** `true` - the step becomes a passage-defining boundary (Stratify), after its success, the scenario-runner will re-resolve the roster before the next Passage (live snapshot); output `refreshed` echoes the value of the flag. **Together with `await_online: true`** further tightens the barrier: SID is only counted when online **and** typed soulprint is written to PG (see facts-wait, amendment 2026-07-02). |
| `await_online` | boolean | optional | `false` | **Onboarding Barrier** ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). `true` - after recording `souls`+coven for all SIDs, the step **blocking** waits for the registered Souls to be ready. Readiness: online by **Redis SID-lease** (live EventStream, **not** PG `souls.status`); with `refresh_soulprint: true` - additionally recorded first typed soulprint (`souls.soulprint_facts`). Requires a configured presence checker on the keeper: `await_online: true` without it → step `failed`. |
| `await_timeout` | duration | **required at `await_online: true`** | — | The upper limit of the barrier expectation. **Required** for `await_online: true` - without it, validation falls (the barrier should not hang forever). The top is limited by `keeper.yml::max_await_timeout` (see ceiling). |
| `await_min_count` | int | optional | number of registered SIDs | Minimum online hosts for barrier success. Default - **all** registered SIDs (`len(sids)`). Valid range: `0 < await_min_count ≤ len(sids)`. |
| `await_poll_interval` | duration | optional | `2s` | Presence (Redis SID-lease) polling period during the barrier. |

### Semantics `mode`

| `mode` | The final set of coven at `sid` | Edge behavior | Idempotency |
|---|---|---|---|
| `append` (default) | existing ∪ passed | empty intersecting set → no-op | yes: calling again with the same `coven` does not change anything |
| `replace` | passed (existing, not mentioned, deleted) | empty `coven: []` - **valid no-op-to-empty** (clears all stable tags); membership is untouched | yes: calling again with the same set - no-op |
| `remove` | existing\passed | empty `coven: []` or labels that do not exist on the host - **no-op** (no error); removes only actually attached tags | yes: calling again with the same set - no-op |

Since membership now lives in `incarnation_membership` and **not** in `souls.coven[]` ([ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)), **no coven operation can sever a host from its incarnation**. The former footgun guard ("a host must always carry the root coven incarnation": `params.coven` `min_items: 1` + the `mode: replace` empty-set error) is therefore **removed** — `coven[]` holds only real stable tags and may be emptied safely; the coven axis stops carrying two meanings at once.

### list-SID — registration+waiting for N hosts in one step

The `sid` parameter accepts a **string OR a list of strings** ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). The target scenario is one create-scenario, which through a `side: keeper` plugin creates N VMs (their `sid` come as a list in `register.<provision>.hosts`), then with one barrier step `core.soul.registered` registers them and waits for onboarding. The list is more natural than `loop:`: the `await_online` barrier aggregates presence on top of the **total** set of SIDs (general `await_min_count`), rather than launching independent per-iteration barriers.

- The `coven` passed applies to **all** list SIDs (the common set of step Coven labels).
- Single string `sid` remains valid (backwards compatible) - internally normalized to a list of one element.
- **Output form by `sid`:** single line → `register.<name>.sid` line (historical form); list → array. The `coven`/`removed` fields reflect the set of the first SID; `created`/`removed` - cumulative fact; `online`/`pending` - lists of SIDs.

**Manifest-DSL trade-off.** The stripped-down manifest-input DSL ([`soul` module](../../shared/coremanifest/mod_soul.go)) does not express the union `string|list`, and changing the declared type `sid` to `list` would break the single-string author form. Therefore `sid` is declared `type: string`: a single literal string passes `soul-lint` as before; **the list comes with the CEL expression** `${ register.<step>.hosts }`, which `soul-lint` skips the type-check ([ADR-010](../adr/0010-templating.md): `${…}`-value is not statically typed). **Literal list `sid: [a, b]` static type-check `soul-lint` does not pass** - acceptable: in practice, the SID list is always from `register.*` (CEL), and runtime accepts both forms.

### Onboarding barrier (`await_online`)

With `await_online: true` the step works in two stages ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)):

1. First, regular registration (souls+coven, as without a barrier) for **all** SIDs.
2. Then a **blocking** readiness poll with a period of `await_poll_interval` under a general timeout of `await_timeout` until the number of ready hosts among the registered SIDs reaches `await_min_count`.

**Source of truth "online" - Redis SID-lease** (live EventStream-lease, [ADR-006(a)](../adr/0006-cache-redis.md)), **NOT** PG `souls.status`. PG status - lifecycle snapshot, lags behind the real state of the stream; lease is a constructively authoritative sign that the agent is in touch (the same source as the presence filter of the target resolver and lease-aware Reaper). The barrier does not consider the online host until the actual stream.

**Facts-wait at `refresh_soulprint: true`** ([ADR-061 amendment 2026-07-02](../adr/0061-onboarding-await-and-midrun-reresolve.md)). SID "ready" = **online (lease) And the typed soulprint is written to PG** (`souls.soulprint_facts IS NOT NULL`). One lease is not enough: the render of the next Passage reads `soulprint.self.*`, and Soul sends an initial report when connecting **best-effort** ([ADR-018](../adr/0018-soulprint-typed.md)) - its recording is asynchronous, on provision-from-zero the barrier and render are completed in one second (race → `render_failed` "no such key"). On rerun / `create_from_souls` facts for a long time in PG → the barrier passes at the very first survey, zero wait. Without `refresh_soulprint`, the barrier remains presence-only (facts are not polled).

**B1-strict (failure-semantics).** If `< await_min_count` is ready for `await_timeout`, the step ends **`failed`** → fail-stop run → `incarnation.state` **does not commit** → `incarnation.status: error_locked`. A partially onboarded set of Souls does not "leak" into the role-use: either a quorum has been reached, or an explicit fail with diagnostics (`pending[]` in the message and in the output; with facts-wait, the classes of shortage are divided - `not online: [sids]` vs `online but factless: [sids]`, so that the race of the first report is distinguishable from the failed onboarding). Persistent polling error (Redis presence or PG facts check not available) - also `failed`, not a "blind" success; cancel run (context-cancel) - `failed`.

Request `await_online: true` without a configured presence checker on the keeper ends with `failed` (silent success in the absence of a presence source is not allowed).

#### Ceiling `await_timeout` (`max_await_timeout`)

DoS-guard, fail-closed. The field `keeper.yml::max_await_timeout` (duration, default `30m` - [`DefaultMaxAwaitTimeout`](../../shared/config/keeper.go)) limits the top to `await_timeout`. If the step specifies `await_timeout` **more** than the ceiling, step `failed` **before** any polling (an obvious error, and **not** a silent cut to the ceiling: the hidden change in the stated behavior is rejected). This protects the cluster from a DoS scenario (a malicious/buggy `await_timeout: 100h` would keep the run-goroutine/Acolyte-worker busy). The ceiling is read hot-reload-aware (from the current snapshot `keeper.yml` for each `Apply`); empty/invalid/`≤0` config value → default `30m`.

> **HA.** Single-binary provisioning run with a long `await_online` barrier is vulnerable to instance crash (blocking poll holds run-goroutine). Provision→onboarding→role scenarios are recommended to be driven through **Voyage** ([ADR-043](../adr/0043-voyage.md)), where recovery is closed (an orphaned claim will be rebranded by another worker, [ADR-027(l)](../adr/0027-apply-work-queue.md)). Standalone staged-recovery of a long barrier is an open risk ([ADR-056 §S4](../adr/0056-staged-render-passage.md)).

### Output contract (`output:` module)

The module returns in `register.<name>.*` (scheme that falls into applier-`register:` or `register:` of a regular module task):

| Field | Type | Description |
|---|---|---|
| `sid` | string **or** array of string | `SID` to which the action was applied. **String** for a single `sid`, **array** for a list (the form mirrors the input, list-SID). |
| `coven` | array of string | **The final** set of coven labels on the host after applying `mode` (not the passed argument set). For a list `sid` — the set of the first SID. |
| `mode` | string | Applied `mode` (echo `params.mode`, convenient for template composition). |
| `created` | boolean | `true` if at least one entry in `souls` was created by the module; `false` if all already existed. |
| `refreshed` | boolean | Echo the value `refresh_soulprint`: `true` ⇒ scenario-runner is guaranteed to re-resolve the roster before the next Passage (S3 [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). |
| `removed` | array of string | **Only with `mode: remove`**: marks that were actually removed. Empty array if no-op (or mode ≠ `remove`). |
| `online` | array of string | **Only with `await_online: true`**: SIDs that have become online (Redis SID-lease) at the time of barrier success/timeout. |
| `pending` | array of string | **Only with `await_online: true`**: SIDs that did not have time to become online by the timeout (B1-strict-failure diagnostics). |
| `satisfied` | boolean | **Only for `await_online: true`**: whether `await_min_count` has been reached (for `refresh_soulprint: true` - according to "ready": online **and** soulprint recorded). If successful, `true`; in case of `failed` failure - `false` (fields `online`/`pending` carry diagnostics; fact classes - in the failed message of the step). |

The fields `online`/`pending`/`satisfied` are present in output **only** when `await_online: true` is specified; without a barrier they do not exist. Plus standard `.changed` / `.failed` DSL cores ([destiny/tasks.md §8](../destiny/tasks.md)).

### Example call from a scenario

```yaml
- name: Bind the new replica to the incarnation
  module: core.soul.registered
  register: registered
  params:
    sid:               "{{ input.host.sid }}"
    # coven: optional stable tags (e.g. [prod, dc1]); membership is set implicitly
    mode:              append
    refresh_soulprint: true
```

After this step, the registry entry `souls` is created/updated, and scenario-runner will re-resolve the roster before the next Passage (`refresh_soulprint: true`, S3 [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)).

### Example: registration+barrier for N created VMs

Registering a list of SIDs produced by an earlier keeper step and blocking wait for onboarding - one step ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)):

```yaml
- name: Register provisioned shards and await onboarding
  module: core.soul.registered
  register: shards
  params:
    sid:                 "${ register.provision.hosts }"   # list of SIDs from cloud-provision
    # coven: optional stable tags; membership is set implicitly from the run's incarnation
    mode:                append
    await_online:        true
    await_timeout:       10m                                # ≤ keeper.yml::max_await_timeout (default 30m)
    await_min_count:     "${ register.provision.count }"    # opt; default = all SIDs
    await_poll_interval: 2s
```

The step first registers all SIDs of the list, then blocks Redis SID-lease. If to `10m` online `< await_min_count` - step `failed` (B1-strict), the run goes to `error_locked`, `register.shards.pending` carries unfinished SIDs.

### Coven tags and the retired destiny `coven-assign`

After [ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation), assigning Coven tags is **pure stable-tag management** — it assigns/updates the real stable Coven tags of an already-bound host and is **no longer a membership act** (assigning a tag does not make a host a member; membership is conferred by the bind, handled implicitly by `core.soul.registered` during onboarding/create).

**Write the module call directly**, in the scenario's own task list:

```yaml
- name: Assign the stable coven tags
  module: core.soul.registered
  params:
    sid:   "${ input.sid }"
    coven: "${ input.coven }"
    mode:  append
```

There used to be a destiny wrapper for this (`examples/destiny/coven-assign/`), and it was **removed in NIM-749 because it could never have run**. A destiny is rendered per host and dispatched to a Soul, whose registry has no `core.soul` module: nothing in the destiny render path diverts a keeper-side address (`guardDestinyTask` looks at the discriminator, not at the side), so the step reached a host as an unknown module. Reading the side off the module address made that statable, and it is now refused offline as `keeper_module_in_destiny`. **A keeper-side module cannot be wrapped in a destiny at all** — a destiny is Soul-side by construction, so the scenario task list is the only place such a step belongs.

## `core.choir.present` / `core.choir.absent`

Editing Voice membership in Choir incarnation (ADR-044): "SID is the Voice of the specified Choir of this incarnation." **Keeper-side**, routed by its module address (NIM-747 - the task carries no `on:` key). Registry key - base `core.choir`; state (`present`/`absent`) comes from the address suffix via `SplitModuleAddr` (see the Registration and Dispatch section). Registered only when `Deps.ChoirStore` is specified - otherwise the step drops to "unknown keeper-side module". Implementation - [`keeper/internal/coremod/choir/member.go`](../../keeper/internal/coremod/choir/member.go).

### Addressing and side

- Namespace: `core`. Module: `choir`. State: `present` (default if state is empty) / `absent`.
- Full task name: `module: core.choir.present` / `module: core.choir.absent`.
- Side: **Keeper-side**, derived from the module address. The step carries **no** `on:` key - writing `on: keeper` on it is an error (`on_keeper_redundant`, NIM-747).

### State (state form)

| State | Action | Idempotency |
|---|---|---|
| `present` (default) | `AddVoice` - SID becomes the Voice of the Choir. | Voice already exists (`ErrVoiceExists`) → `changed=false`, not an error. |
| `absent` | `RemoveVoice` - membership is canceled. | Voice no (`ErrVoiceNotFound`) → `changed=false`, not an error. |

Before mutation, the module validates the existence of incarnation (`IncarnationExists`): absent → `failed`. The membership invariant (Voice only for a SID that is already a member of the incarnation, ADR-044) is implemented in choir-CRUD (`AddVoice → ErrNotMembers`) and is not duplicated here; `ErrNotMembers` → `failed`-event (the run goes to onfail / `error_locked`).

### Parameters (`params:`)

| Parameter | Type | Required | Description |
|---|---|---|---|
| `incarnation` | string | required | The name of the incarnation to which Choir belongs. Checks for existence. |
| `choir` | string | required | Choir's name. Validated by `ValidChoirName`; garbage → `failed`. |
| `sid` | string | required | `SID` host-Voice (FQDN). Validated by `ValidSID`; invalid → `failed`. |
| `role` | string | optional | The host's **declared role** within the Choir (`present` only) - kebab-case, 1..63. Since [ADR-044 amendment 2026-07-30](../adr/0044-choir.md#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role) (NIM-330) this is the ONLY way to declare a role: `incarnation.spec.hosts[].role` and its `PATCH .../hosts` endpoint are gone, so a bootstrap-`create` that needs roles writes them with this step (keeper-side by its address) before any task reads `soulprint.hosts[].role`. Omitted → SQL `NULL` = "no declared role", NOT a default group. |
| `position` | int (≥ 0) | optional | Voice position (`present` only); negative → `failed`. |

### Output contract (`output:` module)

`present` returns to `register.<name>.*`: `incarnation`, `choir`, `sid`, `state: present`, `added` (bool - whether Voice was added). `absent`: `incarnation`, `choir`, `sid`, `state: absent`, `removed` (bool - whether Voice was removed). Plus standard `.changed` / `.failed` DSL cores.

### S-T5 limitations (not implemented)

- **Cross-incarnation guard** (`param.incarnation` == run incarnation): run-context is not available to the module; the module trusts param `incarnation`, only validating its existence. Hard guard is a separate task (RunContext injection into keeper-dispatch).
- **Roster-growth** (new Voice visible to next run step) - not implemented.

Complete per-module reference - [docs/module/core/choir/README.md](../module/core/choir/README.md).

## `core.bootstrap.issued`

Keeper-side issuance of bootstrap tokens for **ready-made VM FQDN/SIDs**, before delivery. Registry key is `core.bootstrap`; state `issued` comes from the public address. Implementation: [`keeper/internal/coremod/bootstrap/issued.go`](../../keeper/internal/coremod/bootstrap/issued.go) and transactional PG backend [`issuer_pg.go`](../../keeper/internal/coremod/bootstrap/issuer_pg.go). Complete per-module reference: [docs/module/core/bootstrap](../module/core/bootstrap/README.md).

Input `sids` is a required, non-empty, unique list of canonical SID/FQDN values. The whole batch is a single Postgres transaction. A free SID becomes a `pending`, `transport=agent` Soul; an existing `pending`/`expired` agent Soul is re-armed and gets a fresh token. Any prior unused token is invalidated, including an expired one. `revoked`, `destroyed`, and `transport=ssh` are refused fail-closed. A failure identifies the SID and rolls back the complete batch.

A `connected`/`disconnected` Soul already owns an identity, and what happens to it depends on **whose host it is** ([ADR-063 amendment 2026-09-04](../adr/0063-bootstrap-token-delivery.md#amendment-2026-09-04--issuance-converges-over-a-host-this-run-already-onboarded-nim-780)). A host of **this run** — member of the run's incarnation, or of no incarnation yet — is **converged over**: passed through with no token and no write of any kind, reported as `{sid, onboarded: true}`. That is what lets a `create` which died *after* onboarding be repeated to completion, the case the cloud plugin's own idempotence made reachable. A host belonging to **another** incarnation is an identity takeover and is still refused, still rolling the whole batch back; an unknown incarnation (a call outside a run) can claim only unbound rows. The ownership rule is `EnsureProvisionable`'s, shared rather than restated (`keepersoul.OwnedByRun`).

Output `register.<name>.hosts[] = {sid, bootstrap_token, expires_at, created, reissued}` plus `count` / `created` / `reissued` / `skipped` / `action: issued`. A converged host keeps its slot in `hosts[]` carrying only `{sid, onboarded: true}` — `core.bootstrap.delivered` skips exactly that shape, and dropping the entry instead would leave a fully converged re-run failing on delivery's empty-list refusal. **A scenario that re-maps `hosts` between the two steps must carry `onboarded` through**, or a converged entry arrives as a host with no token. Every successful repeat over an eligible SID returns new plaintext and makes the previous unused token unusable. The plaintext exists only in the current run register for per-host delivery; Postgres stores only SHA-256. The `bootstrap_token` key is masked on audit/OTel/SSE/log surfaces and must not be projected to `incarnation.state`. Audit `bootstrap.issued` carries only `{action,count,created,reissued,skipped,sids}`, converged hosts included in `sids`.

Canonical ready-made VM chain: `core.bootstrap.issued` → `core.bootstrap.delivered` (`install: true`, `transport: teleport`) → `core.soul.registered` (`await_online: true`, normally `refresh_soulprint: true`).

## `core.bootstrap.delivered`

Delivery of per-VM bootstrap token via SSH to newly created VMs ([ADR-063](../adr/0063-bootstrap-token-delivery.md)). **Keeper-side**, routed by its module address (NIM-747 - the task carries no `on:` key). Registry key - base `core.bootstrap`; state `delivered` comes from the address suffix. Implementation - [`keeper/internal/coremod/bootstrap/delivered.go`](../../keeper/internal/coremod/bootstrap/delivered.go).

**Two transports** (`keeper.yml::push.transport`, [ADR-063 amendment Teleport](../adr/0063-bootstrap-token-delivery.md#amendment-teleport-by-name-transport)): **`direct`** (default) - generic `push.Dial` by required `primary_ip` via SshProvider plugin (Authorize/Sign + CA-signed host-cert verify from Vault host-CA); **`teleport`** - by-name via Teleport Proxy (target=SID, not IP; `primary_ip` is optional; transport+auth+host-verify entirely via Teleport identity-file, Authorize/Sign/Vault-host-CA are not used, retry-to-join). Delivery dependencies are state-specific: direct needs providers + host CAs + dialer, Teleport needs its dialer.

**Two operating modes** ([ADR-063 amendment full-install](../adr/0063-bootstrap-token-delivery.md)): **token-only** (default) - cloud-init has already installed setup, only token + redeem is delivered; **full-install** (`install: true`, only `transport: teleport`) - the module first installs the ENTIRE setup (keeper-ca.pem → soul.yml → soul.service → curl soul binary) in steps `soulinstall.RenderInstallScript` - the same shared-blueprint as cloud-init userdata - then token /redeem/start. For platforms where the provider does not accept userdata.

**Closes BUG#2 cloud-provision.** Before ADR-063, the scenario carried a stub address `keeper.push.applied`, which keeper-side does not exist (this is an audit-event of a Destiny push run, not a module) - the created VM ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)) did not receive a token, the barrier `await_online` did not typed presence, the run went to `error_locked`.

### Design A1 - "thin delivery" + init phase

cloud-init (B-flat, [ADR-017(h)](../adr/0017-keeper-side-core.md)) has already installed a soul binary + CA + systemd-unit on the VM (but **intentionally NOT a token** - userdata is logged by the provider). The module places a token, **redeems it** (`soul init` is the only mechanism for creating SoulSeed; there is no soul-side "pickup" of the token file, [ADR-063 amendment init-phase](../adr/0063-bootstrap-token-delivery.md)) and optionally activates the unit. Per-host stream (**sequentially**):

1. `SshProvider.Authorize(host, user)` — deny interrupts delivery to connect (**fail-closed**).
2. ephemeral ed25519-keypair + `SshProvider.Sign(pubkey)` → `ssh.AuthMethod`s (reuses `push.NewEphemeralEd25519` + `push.AuthMethodsFromSign`). The private key does not leave Keeper.
3. `push.Dial` → `Session` (CA-signed host-cert verify, same path as `SshDispatcher.SendApply`).
4. `session.Run("install -d -m 0700 /etc/soul && umask 077 && cat > <token_path> && chmod 0400 <token_path>", tokenBytes)` - **★ token in STDIN, NOT in argv** (otherwise it will leak to `ps`/audit/journald on VM).
5. `session.Run("test -e /var/lib/soul-stack/seed/current/cert.pem || SOUL_BOOTSTRAP_TOKEN=\"$(cat <token_path>)\" /usr/local/bin/soul init --config /etc/soul/soul.yml", nil)` — redeem the token (CSR→Bootstrap-RPC→SoulSeed). Guard by seed-cert = idempotency(single-use token); the literal `$(cat …)` is expanded by the subshell on the VM - the token is not in the keeper's argv. Executes regardless of `start_soul`.
6. if `start_soul` is `session.Run("systemctl daemon-reload && systemctl enable soul && systemctl start soul", nil)` (parity with cloud-init runcmd: daemon-reload picks up a fresh unit in install mode, enable survives VM reboot).

**B1-strict:** any host error (Authorize-deny / connect-fail / write-fail / init-fail / start-fail) → step `failed` → state not committed → `error_locked`.

### Addressing and side

- Namespace `core`, module `bootstrap`, state `delivered`.
- Full task name: `module: core.bootstrap.delivered`.
- Side: **Keeper-side**, derived from the module address. The step carries **no** `on:` key - writing `on: keeper` on it is an error (`on_keeper_redundant`, NIM-747).

### Parameters (`params:`)

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `hosts` | array of object `{sid, bootstrap_token, primary_ip?}` | required | — | List from `${ register.<issue>.hosts }` (`core.bootstrap.issued`) or the register of a `side: keeper` plugin that created the hosts. `primary_ip` is required in direct transport and optional in Teleport, which dials by `sid`. Empty list → `failed`. An entry marked `onboarded: true` — by `core.bootstrap.issued` (NIM-780) or by whatever produced the list — carries no token and is skipped; that flag is the only exemption for a missing token. A skipped host needs **no `primary_ip` on either transport**, since it is never dialed: the requirement is settled after the flag, so the issuance shape `{sid, onboarded: true}` passes on `direct` too. |
| `ssh_provider` | string | required | — | SshProvider plugin name (`keeper.yml::plugins.ssh_providers[].name`). **★ In `transport: teleport` DOES NOT define a transport** (Authorize/Sign are not called) - the name goes ONLY to audit-payload. |
| `token_path` | string | optional | `/etc/soul/token` | Path to the token file on the VM. |
| `ssh_user` | string | optional | `root` | SSH user. |
| `ssh_port` | int (1..65535) | optional | `22` | sshd TCP port. |
| `start_soul` | bool | optional | `true` | Unit activation after init: `systemctl daemon-reload && systemctl enable soul && systemctl start soul`. `soul init` (step 5) goes regardless of the flag. |
| `install` | bool | optional | `false` | Full-install mode: before the token, put the entire setup via SSH (see "Two modes of operation" above). Only `transport: teleport`; in direct mode → Validate error. Requires a configured block `keeper.yml::cloud_init` (blueprint source, config-reuse). |
| `join_wait_timeout` | duration string (legacy: int seconds) | optional | `15m` | Host Teleport-join waiting ceiling (retry-with-backoff until a node appears in the cluster); relevant only in `transport: teleport`. Upon expiration, step `failed` (B1-strict). |

### Output contract (`output:` module)

`register.<name>.*`: `hosts[] = {sid, delivered, started}` + `count` + `skipped`. A host that was already onboarded is reported as `{sid, delivered: false, started: false, onboarded: true}` and counted in `skipped` (`0` on a clean run); `count` stays the total number of hosts. Plus standard `.changed` (always `true` on success) / `.failed` DSL cores. **★ WITHOUT token in output** - the plain token is visible only in the register of the issuing step (key `bootstrap_token`, masked by `audit.MaskSecrets`); it is not here.

### Security

- Token in STDIN, not in argv (step 4); init step (5) carries the literal unexpanded `$(cat <token_path>)` - the token is expanded by the subshell on the VM, not by the keeper. Audit-payload `bootstrap.delivered` — `{action, ssh_provider, count, sids}`, **without tokens**. The error text is masked (`audit.MaskSecrets`) before the `failed`-event. CA-signed host-cert verify is required (empty host-CA → obvious error). fail-closed Authorize.

### MVP Limits (ADR-063)

- One key-based SshProvider, hosts sequentially. Full-install - only `transport: teleport`.
- **★ C1 - cloud-init CA-signed host-key (required-for-live direct-mode, separate slice).** `push.Dial` trusts only host-cert signed by host-CA (TOFU refusal) - fresh VM must have CA-signed host-key, otherwise handshake is rejected: up to C1 live-e2e in **direct** mode will not work (the module is valid in render L0 Trial + unit tests). For `transport: teleport` C1 **not applicable** - host-verify goes through Teleport CA.

## `core.vault.kv-read`

Explicit reading of the secret from Vault KV (v1/v2, mount version is determined automatically) on the keeper side with a mandatory recording of the audit event `vault.kv-read` (ADR-017(b)). **Keeper-side**, routed by its module address (NIM-747 - the task carries no `on:` key). Registry key - base `core.vault`; state `kv-read` (verb) comes from the address suffix. Exists in parallel with implicit `${ vault(...) }` in CEL: the implicit form is cheap to render, but does not leave an audit record; this module is an explicit form for compliance-accurate reading. Read-only (`changed=false` always). Complete per-module reference with params/output/security - [docs/module/core/vault/README.md](../module/core/vault/README.md).

## `core.vault.kv-present`

Generate-if-absent for Vault KV secrets on the keeper side ([ADR-017 amendment 2026-06-28](../adr/0017-keeper-side-core.md)). **Keeper-side**, routed by its module address (NIM-747 - the task carries no `on:` key). The same module as `kv-read`: Registry key - base `core.vault`; state `kv-present` comes from the address suffix. For each target, it guarantees the existence of a non-empty secret field: absent (no field / `null` / empty string) generates a crypto-random value (`crypto/rand`, bias-free) according to the **password-policy** described by the author (length in characters + alphabet `charset`/`allowed_chars`), present - no-op (does not overwrite). `changed=true` only during real generation; idempotent (rerun/re-create are safe). `destroy` does not clear secrets → re-create reuses the same passwords. Purpose - the service itself generates missing passwords when `create`, the operator does not need to manually pre-seed secrets `vault kv put`.

**Security-invariant (ADR-010):** the generated **value** never goes into register-output / audit-payload / log / OTel / error text - only `path` + names of generated fields come out. register-output - `generated` (map path → \[fields]); audit-event `vault.kv-present` (`source: keeper_internal`) is written only with `changed=true`, payload `{paths}` - without values. Complete per-module reference with params (`targets` / `policy`) / output / security - [docs/module/core/vault/README.md](../module/core/vault/README.md#corevaultkv-present).

## `core.state.<verb>`

The write point of a service state field ([ADR-0084](../adr/0084-explicit-state-capture.md)). **Keeper-side**, routed by its module address (NIM-747 - the task carries no `on:` key). Registry key — base `core.state`; the address suffix **is the verb**. Registered only when the Vault client is configured (`Deps.Vault`), the same pattern as `core.choir` / `core.cert` — otherwise the step fails with "unknown keeper-side module".

The field lands in `incarnation.state` **at the step**, under the run that produced it, not in an end-of-run commit. A later task in the same run reads what an earlier one wrote, and a run that dies half-way leaves what it had already captured instead of nothing.

### The verbs

| Address | What happens to the field |
|---|---|
| `core.state.set` | Overwrite it. |
| `core.state.present` | Write it only if it has no value yet; an existing one wins. |
| `core.state.add` | Idempotently add one element to a collection, by identity (`key:` for a map, `match:` for a list). |
| `core.state.append` | Append one element to a list, no identity check. |
| `core.state.modify` | Patch every element matching `match:`. |
| `core.state.remove` | Drop every element matching `match:`. |
| `core.state.unset` | Drop the field itself. |

These are the [ADR-057](../adr/0057-state-changes-crud-verbs.md) verbs, applied by the same engine the retired `state_changes` used (`keeper/internal/stateop`) — a verb cannot mean two different things depending on which path wrote it. An address outside the table is refused (`unknown keeper-side module state`), and so is a param the verb does not take: a `patch:` handed to a `set` is an authoring mistake, and dropping it silently would write the field without the change the author asked for.

**A declared secret is orthogonal to the verb — a mint is not.** On a property declared `type: secret` in the service `state_schema` ([ADR-0083](../adr/0083-declared-secret-state-fields.md) §4), every verb behaves identically in the half that matters: an existing Vault value is **kept**, and the register carries a `vault:` reference rather than plaintext. `core.state.set` overwrites the field's ordinary content and still does not rotate a live credential. Deliberate rotation is not expressible here by design — its own decision, its own ticket (NIM-700). What no verb does is mint because a value is *missing*: the mint comes from the `generate_secret({…})` request in the value the step proposes, and a derived path holding nothing, resolved from a value carrying no request, is a **failure** — "has no value yet and none was requested" — not a fresh password.

`core.state.present` answers a different question — whether the incoming value reaches the field at all — and that answer feeds back into the secret resolve. Over a field that already holds a value it discards the proposal and resolves the **stored** value instead, so the register quotes what won and nothing is minted for a write that was thrown away. The stored element carries no request (declared secrets are stripped on the way into state, [ADR-0083](../adr/0083-declared-secret-state-fields.md) §4), so `present` over a populated field **fails closed** on a property whose Vault value is gone: "is stored but was never minted in Vault -- core.state.present keeps the stored value and cannot mint one for it". Three outcomes on an empty derived path, one of them a mint — the table and the cites are in [ADR-0083](../adr/0083-declared-secret-state-fields.md), amendment 2026-09-02.

The practical edge is a schema change that re-points a derived path: it does **not** self-heal. A day-2 step re-resolving a stored record breaks loudly; a create-class step carrying `generate_secret({…})` mints silently at the new path, and the running service still authenticates with the old one ([ADR-019](../adr/0019-state-migration-dsl.md), amendment 2026-09-01 §7).

```yaml
- name: capture the users we just created
  module: core.state.add
  register: redis_users
  params:
    field: redis_users
    key: "${ compute.new_user.name }"
    value: "${ compute.new_user }"
    on_conflict: skip
```

### Parameters (`params:`)

| Param | Type | Verbs | Required | Meaning |
|---|---|---|---|---|
| `field` | string | all | yes | The **top-level** property of the service `state_schema` this task writes. Not a path, not a nested field — a name that is not a top-level property is an error. |
| `value` | any | `set`, `present`, `add`, `append` | yes | The proposed value: the whole field for `set` / `present`, one element for `add` / `append`. Secret properties may hold a `SecretRequest` produced by [`generate_secret()`](../templating.md#23-registered-cel-functions-starting-minimum); everything else is ordinary data. |
| `key` | string | `add` | no | The element's identity in a **map** field: the key it is stored under. On a **list** field it is an error, not a fallback — the list spelling is `match:`, and the two are mutually exclusive (which one the engine reads is decided by the field's kind, so a task carrying both would have one of them silently ignored). |
| `match` | expression | `add`, `modify`, `remove` | no | Per-element predicate, evaluated by the render pipeline's own CEL environment — the module never builds one of its own. On `add` it is the element's identity in a **list** field (bindings `elem` and `value`); omitted there, identity is deep equality of the whole element. On `modify` / `remove` it selects which elements to touch (binding `elem`), and **omitting it does not mean "every element"** — an empty predicate matches nothing, so the step is a no-op (fail-safe: a forgotten line deletes nothing). To mean every element, write `match: "true"` and take the `state_wide_match` warning, which exists to make that choice visible. |
| `on_conflict` | `skip` \| `replace` \| `error` | `add` | no (`skip`) | What to do when an element with that identity is already there. |
| `patch` | map | `modify` | yes | The properties to overwrite on each matching element. |
| `expect` | `any` \| `one` \| `at_most_one` | `modify`, `remove` | no (`any`) | How many elements the match is allowed to hit, checked **before** mutating. |

An enum param is checked at the module, not passed through: `on_conflict: replce` would otherwise fall to the engine's default (`skip`) and silently keep the old element.

The **service** and **incarnation** of the run are not parameters — they travel on the module context, because they are two segments of a derived path and an author must not be able to name them (the same rule the removed `core.cloud` followed for its incarnation). Both missing is a **failure, not a default**: the owner is what makes the path unforgeable, so the module fails closed rather than derive a path with an empty segment. The same applies to an unavailable `state_schema`.

A `SecretRequest` sitting in a position no declared property claims is an **error**, checked before anything is written. A request nobody resolves would travel on into `incarnation.state` as ordinary data and look like a password was asked for when nothing minted one.

### Output contract (`output:` module)

| Key | Verbs | Meaning |
|---|---|---|
| `field` | all | The state field this task wrote, echoed for diagnostics. |
| `effective` | `set`, `present`, `add`, `append` | **The effective value** — what the step resolved, existing secrets included, not what the caller proposed — with every secret property replaced by its `vault:` reference. This is what consumers read: `${ register.<name>.effective }`. A verb carrying no `value:` reports no `effective` at all: the key is **absent**, not null, so "this verb writes no value" cannot be mistaken for "resolution produced nothing". |
| `generated` | all | The derived Vault paths whose secret this run **minted**, sorted, flat form (`<mount>/<service>/…#<field>`, no `vault:` prefix). Paths, never values. |

*A generator's output is a candidate, a writer's output is the truth.* Only the writer knows which value won, so only the writer can be quoted — a consumer that re-derives the value from its own `generate_secret()` call would configure the target with a password the writer discarded.

`changed=true` when the run minted a secret **or** the stored field is not what it was. Reporting only the mint would call a real state change a no-op; reporting only the field would miss a mint into a collection whose visible content did not move. `onchanges:` hung off this register therefore fires on either.

A secret in the register rides as a **reference**, not as plaintext ([ADR-0083](../adr/0083-declared-secret-state-fields.md) §6) — which is why plaintext never reaches `apply_task_register` and needs no purge. The render boundary resolves the reference for the cell that consumes it, and the seal detector marks such a register (`SealSources.SealedRegisters`) so the consuming cell is masked in `status_details`.

### Security

Derived, never authored: [`shared/config.SecretField`](../../shared/config/secret_field.go) builds the path from (service, incarnation, state field, key) and every segment is checked against the [ADR-064](../adr/0064-secret-write-path.md) grammar `^[a-zA-Z0-9_-]+$`, **failing closed** — `<key>` is operator-influenced data, so a `/`, a `.` or a `..` inside a user's name must never become a path segment. The corollary is the fence: an author-written path under `<mount>/<service>/` is refused in every spelling ([ADR-0083](../adr/0083-declared-secret-state-fields.md) §7, [templating.md §2.3](../templating.md#23-registered-cel-functions-starting-minimum)).

The audit event is `vault.kv-present`, **reused rather than renamed**: the fact recorded is the one that module already records — a secret was ensured present at these paths — and splitting one fact across two names would leave an operator having to filter on both. Written only when something was minted, payload `{paths, state_field, service, incarnation}`, no values. Every failure is a failed **event** rather than a gRPC error, so the run enters `onfail` / `error_locked` like any other task.

## `core.cert.registered` / `core.cert.issued`

Tracking of the incarnation's service TLS certs in the **Warrant** registry ([ADR-017 amendment 2026-07-01/2026-07-09](../adr/0017-keeper-side-core.md), [naming-rules.md → Warrant](../naming-rules.md#domain-entities)) — the basis for auto-rotation by the Reaper. **Keeper-side**, routed by its module address (NIM-747 - the task carries no `on:` key). Registry key — base `core.cert`; state (`registered` / `issued`) comes from the address suffix via `SplitModuleAddr`. Registration in `coremod` is conditional on a configured `CertStore` (same pattern as `core.choir`/`core.vault`) — otherwise the step fails with "unknown keeper-side module". This is about the **service** cert (e.g. Redis server TLS), not the Soul agent's identity cert ([SoulSeed](../soul/identity.md), rotated separately).

Two states — by cert source:

| State | What it does | Secret handling |
|---|---|---|
| `registered` | Records an **already issued** cert(s) into Warrant: reads the PEM from Vault by `vault_ref`, extracts `serial`/`fingerprint`/`not_after` from the x509 itself. For certs issued outside the module (e.g. the initial `rotate_tls`, which already placed material in Vault). | The module never touches it (only reads the cert PEM). |
| `issued` | **Mint+enroll**: Keeper generates a keypair+CSR → signs via Vault PKI → writes cert+key to Vault (`secret/<service>/<incarnation>/tls/<kind>`) → records into Warrant. One step "issue and record". | Generated keeper-side, written to Vault, **never leaves** (R2 invariant [ADR-017](../adr/0017-keeper-side-core.md)). |

Without tracking in Warrant the Reaper is **blind to certs** — the step (`registered` or `issued`) must be present in the service's create/`rotate_tls` scenarios. Both are idempotent (same fingerprint → no-op); output/audit carries only non-secret metadata (`kind`/`serial`/`fingerprint`/`not_after`), never the PEM or the secret material.

### Addressing and side

- Namespace `core`, module `cert`, state `registered` / `issued`.
- Full task name: `module: core.cert.registered` / `module: core.cert.issued`.
- Side: **Keeper-side**, derived from the module address. The step carries **no** `on:` key - writing `on: keeper` on it is an error (`on_keeper_redundant`, NIM-747).

### Parameters (`params:`)

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `incarnation` | string | required | — | Name of the incarnation the certs belong to (FK Warrant → `incarnation`). |
| `auto_rotate` | bool | optional | `true` | Per-cert auto-rotate flag (written to `warrant.auto_rotate`). `true` = the Reaper may rotate the cert (with the service's `certificate.rotate.enable` — this is the "default yes"); `false` = the cert is tracked but not rotated. |
| `certs` | array of object `{kind, vault_ref}` | **`registered` only** | — | List of already issued certs to record. `kind` ∈ `cert`/`key`/`ca`; `vault_ref` — path to the material in Vault. |
| `kind` | string | **`issued` only** | — | Type of the TLS material being issued; determines the write path `secret/<service>/<incarnation>/tls/<kind>`. |

**★ `pki_role` and `scenario` are NOT params.** The Vault PKI signing role (for `issued`) and the rotation scenario name (for the Reaper) are taken from the service manifest `service.yml::certificate` — `pki_role` on the section itself, `scenario` under `rotate:` ([service/manifest.md → Section `certificate`](../service/manifest.md#certificate-section)) — **not** from the step's `params`. The scenario author does not choose an arbitrary PKI role: `pki_role` comes from the git-reviewed manifest, the PKI-engine mount from `keeper.yml::vault.pki_mount` ([config.md → `vault`](config.md#vault)). Reason — blast-radius ([ADR-017 amendment 2026-07-09](../adr/0017-keeper-side-core.md)).

### Relation to the Reaper rule `rotate_due_certs`

`core.cert.*` only **records** certs into Warrant; auto-rotation of expiring ones is driven by the Reaper rule **`rotate_due_certs`** (Reaper leader, [ADR-017 amendment 2026-07-01](../adr/0017-keeper-side-core.md)): scan `not_after < NOW()+threshold` → CAS `active→rotating` → keeper-side re-sign (Vault PKI role `pki_role` from the manifest) → `WriteKV` → spawn the rotation scenario (`certificate.rotate.scenario`) **for the whole incarnation at once**. Three rotation gates — service `certificate.rotate.enable` × per-cert `auto_rotate` × cluster `keeper.yml::reaper.rules.rotate_due_certs.enabled` (default OFF+dry_run); config-driven source of `scenario`/`pki_role` (Path B, ~60s cache pinned by `service_version`) — [ADR-017 amendment 2026-07-09](../adr/0017-keeper-side-core.md). Retention of removed certs — Reaper rule `purge_old_certs`.

## Auto-synthesis of `core.module.installed` from `service.yml::modules[]`

The step itself `core.module.installed` - **Soul-side** (delivery of the SoulModule plugin to the host: allow-check → `FetchModule` → verify → hot-register; host side - [soul/modules.md](../soul/modules.md), commit - [ADR-065](../adr/0065-core-module-installed.md); ⚠ the `FetchModule` link gains a sibling with the [2026-09-04 amendment](../adr/0065-core-module-installed.md#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) — a host with egress pulls from the artifact source, `FetchModule` stays for hosts without it; NIM-794, not implemented, and **synthesis is unaffected either way**). Keeper **synthesizes** such steps into the run plan from the manifest declaration `service.yml::modules[]` (`{name, ref}`, [service/manifest.md](../service/manifest.md)) - the operator declares the dependency once per service, install-boilerplate is not needed in each scenario ([ADR-065 amendment 2026-07-03](../adr/0065-core-module-installed.md)).

**Synthesis point.** Immediately after expanding `include:` (flat task list, [scenario/orchestration.md §6](../scenario/orchestration.md)) and before Stratify - the same in all places that build the run plan: scenario-runner (apply), claim-render Acolyte (reproduces the run-goroutine plan - correlation plan_index/TaskEvent) and L0-trial-harness. Pre-flight/parsing/UI-plane surfaces do not mutate.

**What is inserted.** For each record `modules[]`, which has a consumer task in the plan (task `module:` with the prefix `<alias>.<module>.`), a regular plan task with a marker name is synthesized:

```yaml
- name: install community (service manifest)   # synthesis step marker name
  module: core.module.installed
  params: { name: community, ref: v1.2.0 }     # the ALIAS + the entry's ref
```

**`params.name` is address level 1, not the manifest entry.** `modules[].name` is `<alias>.<module>`; `core.module.installed` installs an artifact into the slot the alias names and has nothing to do with level 2, so it rejects a dotted value (`soul/internal/coremod/module`, `reAlias`). NIM-377 renamed level 1 from an artifact-declared namespace to an operator-chosen alias and left the synthesizer passing both levels through — every service declaring `modules:` then failed at apply on every host, with the producer and the consumer each carrying passing tests of their own (NIM-524).

- **Position** - immediately before the first consumer task; consumer inside `block:` → insertion before the entire block. Several synthesis steps before one task - in manifest order.
- **One step per alias.** Two entries served by one artifact (`redis.instance`, `redis.sentinel`) install once, before the **earlier** of their consumers — they name one slot, and installing before the later one would leave the earlier consumer unresolvable. `service.yml` validation requires such entries to agree on `ref` (`conflicting_module_ref`).
- **Without `on:`/`where:`** - a regular roster task: stratified as its consumer, incl. goes **after** roster-refresh-border ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)) - provision-from-zero works without special logic.
- **A module without consumers in the plan is NOT synthesized**; entries under a **reserved** name (`core`, `keeper`, `soul`, … — `plugin.ReservedNames`) are skipped: no registration can hold one, so no install could ever satisfy them. Manifest validation already forbids them (`core_module_in_modules_list`); the skip is the defensive half of the same rule, for a manifest re-read from storage.
- `ref` in params - **pin-verification** ([ADR-065(c)](../adr/0065-core-module-installed.md)): the active Sigil permit must be on this ref, otherwise step `failed`.
- The synthesis step goes through render → dispatch → TaskEvent like any task and is visible in the run-view by its marker name.

**Takeover - an explicit step disables synthesis.** An explicit `core.module.installed` naming that **slot** suppresses synthesis for it - the operator controls the position itself, `ref` and `when:`. Both halves of the comparison are reduced to address level 1, so a step written `name: redis` takes over the `community` slot just as `name: community` does - it is one artifact either way (NIM-543). The dotted spelling is still wrong and is reported offline as `module_install_name_not_an_alias`; what changed is that it no longer silently earns a second install beside the operator's own. a `params.name` carrying a `${…}` cell — wholly or in part — cannot be compared literally: synthesis will not be suppressed, a double step is possible - harmless (idempotency by sha256: the binary is already installed → `changed=false`, fetch is not executed).

**Idempotency and errors.** Skip is modular only (sha256 of installed binary == sha of active Sigil permission); plan-level skip no - Keeper does not maintain a register of the installed per-host, the roster changes mid-run. The absence of an entry in `plugins.soul_modules[]` / active Sigil-permission catches the Soul-side allow-check of the step (`module_not_allowed`) - like an explicit step; There is no keeper-side pre-flight gate in MVP (together with the validation-hint "the module is used but not declared" - post-MVP). ⚠ **The idempotency compare gains an arity with the [2026-09-04 amendment](../adr/0065-core-module-installed.md#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) (NIM-794, not implemented):** a grant carries a **list** of per-platform digests rather than one, so "sha of the active Sigil permission" becomes the sha of the row matching the host's **own Soulprint facts** — `os.family`, with the four Linux distro families collapsed to `linux`, and `os.arch` — **falling back to the running binary's platform** where a fact is missing (`Module.hostPlatform`, `soul/internal/coremod/module/source.go:315-331`; an earlier draft said `runtime.GOOS`/`runtime.GOARCH` and is superseded). The mechanism is otherwise the same, and none of the synthesis, takeover or error behaviour above moves.

**MVP limitation.** Consumers are defined by `module:` scenario tasks (top-level and inside `block:`); a module used **only within destiny** (via `apply:`) is not considered a consumer - it still needs an explicit install step. Push is not affected - modules travel there en masse ([ADR-020](../adr/0020-plugin-infrastructure.md)).

## See also

- [architecture.md → Module model](../architecture.md) - general core/custom model, Soul-side vs Keeper-side, SoulModule protocol.
- [architecture.md → Module addressing](../architecture.md) - format `<namespace>.<module>.<state>`.
- [scenario/orchestration.md §3](../scenario/orchestration.md) - `on:`, step manager between the Soul side and the Keeper side.
- [storage.md](storage.md) - `souls` tables, coven binding.
- [soul/modules.md](../soul/modules.md) — host side of `core.module.installed`: delivery, verify, cache of custom modules.
- [naming-rules.md → Destiny Modules](../naming-rules.md) - a dictionary of names.
