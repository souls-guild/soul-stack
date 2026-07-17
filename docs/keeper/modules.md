# Keeper-side core modules

The vast majority of core modules are **Soul-side** (executed on the host `soul` binary: `pkg`, `file`, `service`, `user`, `exec`, `cmd`, `cron`, ...; see [architecture.md → "Module model"](../architecture.md)). Some of the core modules are **Keeper-side**: they operate on keeper registries (Postgres `souls`+coven, Redis cache, logs) and are executed on the keeper itself. The normative specification of Keeper-side core modules is collected here.

## Soul-side / Keeper-side dispatcher - `on:`

Addressing (`<namespace>.<module>.<state>`) and SoulModule contract are the same for both sides. The difference is **where the step is performed**; this is solved by the scenario key `on:` ([scenario/orchestration.md §3](../scenario/orchestration.md)):

| `on:` | Where is it performed | Suitable for modules |
|---|---|---|
| omitted / `[coven, …]` | on incarnation hosts | Soul-side core (`core.pkg.installed`, `core.file.present`, ...) |
| `keeper` | on the keeper itself | Keeper-side core (`core.soul.registered`, `core.cloud.created` - cloud-create via CloudDriver, `core.bootstrap.delivered` - bootstrap token delivery via SSH, ...) |

Launching Soul-side core module with `on: keeper` - validation error; and vice versa. The ownership of a module by a party is declared in its manifest; `soul-lint` checks statically.

## Registration and dispatch at (`base` + `state`)

Keeper-side core modules are registered in the keeper-side Registry (`keeper/internal/coremod/registry.go`) by **base name** - `<namespace>.<module>` without state suffix: `core.soul`, `core.cloud`, `core.bootstrap`, `core.choir`, `core.vault`, `core.cert`. State comes from the last segment of the task address.

When executing a keeper-side task (`keeper/internal/scenario/keeper_dispatch.go`), the address `module: <namespace>.<module>.<state>` is divided by the function `config.SplitModuleAddr` (a single parser for both sides, the same as the Soul-side runtime) into a pair `(base, state)`:

- `base` (`core.cloud`) goes to `Registry.Lookup` - finds the implementation of `SoulModule`;
- `state` (`created`) is placed in `ApplyRequest.state` and dispatched **inside** the module implementation.

Author-form examples → parsing:

| Task address (`module:`) | Registry-key (`base`) | `ApplyRequest.state` |
|---|---|---|
| `core.soul.registered` | `core.soul` | `registered` |
| `core.cloud.created` / `core.cloud.destroyed` | `core.cloud` | `created` / `destroyed` |
| `core.bootstrap.delivered` | `core.bootstrap` | `delivered` |
| `core.choir.present` / `core.choir.absent` | `core.choir` | `present` / `absent` |
| `core.vault.kv-read` / `core.vault.kv-present` | `core.vault` | `kv-read` / `kv-present` |
| `core.cert.registered` / `core.cert.issued` | `core.cert` | `registered` / `issued` |

Defective address (`SplitModuleAddr` returned `ok=false`: empty, `.state`, `core.`) or `base`, which is not in the Registry - the keeper-task crashes (`failed`-event "unknown keeper-side module"), like Soul-side on an unknown module. Registration of a module in the Registry is conditional based on the presence of its dependency in `coremod.Deps`: `core.choir` is connected only when `ChoirStore` is specified, `core.bootstrap` - only with a full set of SSH-deps (provider card + host-CA + dialer), otherwise the assembly does not carry them, and the step with this module will fall "unknown".

### Audit-trace and per-task alerting

Each keeper-side task writes audit-event `task.executed` (symmetrically to Soul-side handler `TaskEvent`): `sid = keeper` (address of the keeper-target of the run), `correlation_id = apply_id`, `source: keeper_internal`, `payload.status` - name `keeperv1.TaskStatus` (`changed → TASK_STATUS_CHANGED` / `failed → TASK_STATUS_FAILED` / otherwise `TASK_STATUS_OK`). Thanks to this, **task:-Tiding subscription also works for keeper-side addresses** (`on: keeper`): a keeper task with the address `register ∪ id` (including `provision_vm` with `id:` without `register:`) ends up in `changed_tasks` of the terminal event `incarnation.run_completed` and is matched by the task selector ([ADR-052 amend §k/§l](../adr/0052-herald-notifications.md)). Secret hygiene: keeper-side `task.executed` carries only address + status (without `register_data`/output); `error.message` - only on failure and only for non-`no_log` tasks. Operator-SSE keeper-side does not broadcast progress.

### The context of `params:` is `incarnation.state`, but not `soulprint`

Keeper-side task is executed on the keeper itself - it has no hosts. Therefore, `params:` are rendered in a **run-level** context (once per run, not per-host): `input.*` / `essence.*` / `incarnation.*` / `register.*` are available (from previous keeper tasks), but **not** `soulprint.self` / `soulprint.hosts` - access to them in `params` keeper task gives the standard CEL error `no such key` (there are no host facts, and this is correct: the keeper step operates with registries, not facts of a specific VM).

In `incarnation.*` the key **`incarnation.state.<path>`** is available - read-only **pre-run snapshot** `incarnation.state` (the same `stateBefore` for row-lock runs, symmetrically for Soul-side tasks). The snapshot is invariant within the run (fixed once, does not accumulate between passages). This allows the keeper-side task to read facts written by the previous run: for example, `core.cloud.destroyed` in the teardown scenario `destroy` takes `provider`/`vm_ids`/`sids` from `incarnation.state.provisioned_*` written by the create run through `core.cloud.created` ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). If the incarnation does not yet have a state (push/trial without it) - `incarnation.state.<x>` gives `no such key`; defend reading `default(incarnation.state.<path>, …)` where fact may be missing.

## `core.soul.registered`

**The first Keeper-side core module.** Controls Soul's binding (by `SID`) to a set of stable Coven labels in the keeper's registry (tables `souls` + coven, [storage.md](storage.md)). Accepts a **string OR list of SIDs** (registering N created hosts in one step) and optionally carries an **onboarding barrier** `await_online` (blockingly waits for registered Souls to become online) - [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md).

### Addressing and side

- Namespace: `core`. Module: `soul`. State: `registered`.
- Full task name: `module: core.soul.registered`.
- Side: **Keeper-side**. The step **must** carry `on: keeper`.

### State (state form)

`registered` - declarative form: "Soul with the specified `sid` is in the registry and is bound to the specified set of Coven tags." The module is idempotent by design (re-calling with the same set is no-op).

If there is no entry in `souls` for this `sid` yet, the module creates it under `status: pending` (a new host added by the scenario - for example, host branch `add_replica` or after cloud-create via `core.cloud.provisioned`). Creating an entry here is the only side-effect besides updating the coven; the module does not issue bootstrap tokens and does not launch a CSR cycle (this is the responsibility of onboarding, [soul/onboarding.md](../soul/onboarding.md)).

In list form `sid` (see list-SID), the passed set `coven` is applied to **each** list SID; The `await_online` barrier (if specified) aggregates presence over the **entire** set.

### Parameters (`params:`)

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `sid` | string **or** array of string, `format: fqdn` | required | — | `SID` Soul (FQDN) to which the binding is applied. Accepts **single string OR list** ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md), see list-SID). The list in practice comes with the CEL expression `${ register.<provision>.hosts }` (SID list from `core.cloud.provisioned`); literal list `sid: [a, b]` statically `soul-lint` **doesn't** pass (manifest declares `sid` as `string`) - this is a deliberate trade-off, see list-SID. |
| `coven` | array of string, `pattern: "^[a-z][a-z0-9-]*$"`, `min_items: 1`, `unique: true` | required | — | A set of stable Coven tags. At least one tag. When listed, `sid` applies to each SID. |
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
| `replace` | passed (existing, not mentioned, deleted) | empty `coven: []` - **error** (double protection from the footgun "host without root coven incarnation": schema `params.coven` `min_items: 1` + repetition at the semantic level of `mode`; intentional - so that the footgun is caught even when the schema is weakened / the contract is expanded in the future) | yes: calling again with the same set - no-op |
| `remove` | existing\passed | empty `coven: []` or labels that do not exist on the host - **no-op** (no error); removes only actually attached tags | yes: calling again with the same set - no-op |

`replace` with a non-empty `coven` that does not contain the root `incarnation.name` - the module **does not block** at the mode semantic level (the set operation is symmetrical), but this is a user error - footgun. The "host always carries the root coven incarnation" guarantee is invariant at the `souls`+coven table/resolver level (see [storage.md](storage.md), [scenario/orchestration.md §3](../scenario/orchestration.md)), not at the level of an individual module call.

### list-SID — registration+waiting for N hosts in one step

The `sid` parameter accepts a **string OR a list of strings** ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). The target scenario is one create-scenario, which through `core.cloud.provisioned` creates N VMs (their `sid` come as a list in `register.<provision>.hosts`), then with one barrier step `core.soul.registered` registers them and waits for onboarding. The list is more natural than `loop:`: the `await_online` barrier aggregates presence on top of the **total** set of SIDs (general `await_min_count`), rather than launching independent per-iteration barriers.

- The `coven` passed applies to **all** list SIDs (the common set of step Coven labels).
- Single string `sid` remains valid (backwards compatible) - internally normalized to a list of one element.
- **Output form by `sid`:** single line → `register.<name>.sid` line (historical form); list → array. The `coven`/`removed` fields reflect the set of the first SID; `created`/`removed` - cumulative fact; `online`/`pending` - lists of SIDs.

**Manifest-DSL trade-off.** The stripped-down manifest-input DSL ([`shared/coremanifest/soul.yaml`](../../shared/coremanifest/soul.yaml)) does not express the union `string|list`, and changing the declared type `sid` to `list` would break the single-string author form. Therefore `sid` is declared `type: string`: a single literal string passes `soul-lint` as before; **the list comes with the CEL expression** `${ register.<step>.hosts }`, which `soul-lint` skips the type-check ([ADR-010](../adr/0010-templating.md): `${…}`-value is not statically typed). **Literal list `sid: [a, b]` static type-check `soul-lint` does not pass** - acceptable: in practice, the SID list is always from `register.*` (CEL), and runtime accepts both forms.

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
- name: Register the new replica with the cluster coven labels
  on: keeper
  module: core.soul.registered
  register: registered
  params:
    sid:               "{{ input.host.sid }}"
    coven:             ["{{ incarnation.name }}"]
    mode:              append
    refresh_soulprint: true
```

After this step, the registry entry `souls` is created/updated, and scenario-runner will re-resolve the roster before the next Passage (`refresh_soulprint: true`, S3 [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)).

### Example: registration+barrier for N created VMs

Registering a list of SIDs from `core.cloud.provisioned` and blocking wait for onboarding - one step ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)):

```yaml
- name: Register provisioned shards and await onboarding
  on: keeper
  module: core.soul.registered
  register: shards
  params:
    sid:                 "${ register.provision.hosts }"   # list of SIDs from cloud-provision
    coven:               ["${ incarnation.name }"]
    mode:                append
    await_online:        true
    await_timeout:       10m                                # ≤ keeper.yml::max_await_timeout (default 30m)
    await_min_count:     "${ register.provision.count }"    # opt; default = all SIDs
    await_poll_interval: 2s
```

The step first registers all SIDs of the list, then blocks Redis SID-lease. If to `10m` online `< await_min_count` - step `failed` (B1-strict), the run goes to `error_locked`, `register.shards.pending` carries unfinished SIDs.

### Relation to destiny `coven-assign`

The existing destiny `coven-assign` ([examples/destiny/coven-assign/](../../examples/destiny/coven-assign/)) remains as a **thin wrapper** around this module: its `tasks/main.yml` is reduced to a single step `module: core.soul.registered` with `mode: append` (single `sid`, no barrier and without `refresh_soulprint`). `destiny.yml` `coven-assign` (input contract `sid`+`coven`) - compatible, does not change.

When to write a module call directly, and when to write `apply: { destiny: coven-assign }`:

- **Directly `module: core.soul.registered`** is a typical case in the scenario. One step, everything is visible in place, supports all `mode` modes.
- **`apply: { destiny: coven-assign }`** - when there is already an established call through destiny (historical compatible code), or when destiny is used as a self-contained unit with a molecule test and an independent git ref ([ADR-007](../adr/0007-versioning-git-ref.md)). The wrapper fixes `mode: append` - `replace`/`remove` requires a direct call.

## `core.choir.present` / `core.choir.absent`

Editing Voice membership in Choir incarnation (ADR-044): "SID is the Voice of the specified Choir of this incarnation." **Keeper-side**, dispatcher `on: keeper`. Registry key - base `core.choir`; state (`present`/`absent`) comes from the address suffix via `SplitModuleAddr` (see the Registration and Dispatch section). Registered only when `Deps.ChoirStore` is specified - otherwise the step drops to "unknown keeper-side module". Implementation - [`keeper/internal/coremod/choir/member.go`](../../keeper/internal/coremod/choir/member.go).

### Addressing and side

- Namespace: `core`. Module: `choir`. State: `present` (default if state is empty) / `absent`.
- Full task name: `module: core.choir.present` / `module: core.choir.absent`.
- Side: **Keeper-side**. The step **must** carry `on: keeper`.

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
| `role` | string | optional | Voice part in Choir (`present` only). |
| `position` | int (≥ 0) | optional | Voice position (`present` only); negative → `failed`. |

### Output contract (`output:` module)

`present` returns to `register.<name>.*`: `incarnation`, `choir`, `sid`, `state: present`, `added` (bool - whether Voice was added). `absent`: `incarnation`, `choir`, `sid`, `state: absent`, `removed` (bool - whether Voice was removed). Plus standard `.changed` / `.failed` DSL cores.

### S-T5 limitations (not implemented)

- **Cross-incarnation guard** (`param.incarnation` == run incarnation): run-context is not available to the module; the module trusts param `incarnation`, only validating its existence. Hard guard is a separate task (RunContext injection into keeper-dispatch).
- **Roster-growth** (new Voice visible to next run step) - not implemented.

Complete per-module reference - [docs/module/core/choir/README.md](../module/core/choir/README.md).

## `core.cloud.created` / `core.cloud.destroyed`

Creating/deleting VMs via CloudDriver plugin ([ADR-017](../adr/0017-keeper-side-core.md)). **Keeper-side**, dispatcher `on: keeper`. Registry key - base `core.cloud`; state (`created` / `destroyed`, also `resized`) comes from the address suffix. Implementation - [`keeper/internal/coremod/cloud/provisioned.go`](../../keeper/internal/coremod/cloud/provisioned.go). Full flow (Provider/Profile-resolve, credentials Option A, userdata-render, guard-rails destroy) - [cloud.md](cloud.md); per-module reference - [docs/module/core/cloud/README.md](../module/core/cloud/README.md).

### Parameters `created` (`params:`)

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `provider` | string | required | — | Registry line NAME `providers`: keeper resolves it to driver-name (`soul-cloud-<type>`) + plain-credentials from Vault (Option A, [cloud.md → Credentials-flow](cloud.md#credentials-flow)). |
| `profile` | string | optional | — | Registry string NAME `profiles` (**NOT inline-object**, [ADR-017 amendment 2026-06-29](../adr/0017-keeper-side-core.md)); keeper resolves the name in VM-spec params. |
| `count` | int (≥ 1) | optional | `1` | How many VMs to create. |
| `userdata` | string | optional | — | Ready cloud-init blob (legacy / gold-image flow). Mutually exclusive with both `generate_userdata: true` and `self_onboard: true`. |
| `generate_userdata` | bool | optional | `false` | Render userdata from `keeper.yml::cloud_init` - setup **without tokens**, B-flat mode ([cloud.md → Cloud-init bootstrap](cloud.md#cloud-init-bootstrap-mvp)). |
| `name` | string | optional; **required at `self_onboard: true`** | — | Base name of the VM batch → `CreateRequest.name`; The driver names the VM `<name>-<index>`. In conjunction with the Provider registry field `fqdn_suffix` (migration 094) gives the predictable FQDN `<name>-<index>.<fqdn_suffix>` BEFORE create. **The value must match the base name of the VM** - pattern `^[a-z][a-z0-9-]{0,48}[a-z0-9]$` (stricter than `incarnation.NamePattern` `^[a-z0-9][a-z0-9-]{0,62}$`: without start digit and tail hyphen, length ≤ 50). Create-scenarios redis/dragonfly substitute `incarnation.name` here and pre-flight-`assert`-it is the first step of the provision body: the name of the incarnation, which is not suitable as a VM-base (start-digit / tail-hyphen / length 51–63), with `input.provision.enabled=true` is rejected **422 even BEFORE the creation of the incarnation** (not reaching clouddriver); Without provision, the restriction does not apply. |
| `self_onboard` | bool | optional | `false` | Self-onboard "Option T" ([ADR-017(h) amendment 2026-07-01](../adr/0017-keeper-side-core.md)): keeper BEFORE create issues per-VM tokens to the predicted SIDs and bakes them in userdata (`/etc/soul/self-onboard-tokens`, `0600`) - VM onboards itself in one cloud-init cycle, step `core.bootstrap.delivered` not needed. Requires `name` and a non-empty `providers.fqdn_suffix` (otherwise an obvious error); **mutually exclusive with explicit `userdata:`**; `generate_userdata` is implied (the `keeper.yml::cloud_init` block is required). **Plain token is NOT placed in register-output** (there is no `bootstrap_token` key). Failure of create/FQDN reconciliation rolls back inserted souls/tokens (orphan-cleanup). A conscious departure from the security-floor B-flat (single-use tokens, opt-in per-step) - [cloud.md → Self-onboard "Option T"](cloud.md). |

Output `created` (`register.<name>.*`): `hosts[]` (`sid` / `vm_id` / `primary_ip` / `attributes`; in B-flat additionally `bootstrap_token` - plain, the only point of visibility, masked by `audit.MaskSecrets`), `count`, `vm_ids`, `action`. With `self_onboard: true` - plus the sign `self_onboard: true` and **without** `bootstrap_token`. Params `destroyed` (`provider` / `vm_ids` / `sids` + cascade semantics) - [per-module README](../module/core/cloud/README.md) and [cloud.md](cloud.md).

## `core.bootstrap.delivered`

Delivery of per-VM bootstrap token via SSH to newly created VMs ([ADR-063](../adr/0063-bootstrap-token-delivery.md)). **Keeper-side**, dispatcher `on: keeper`. Registry key - base `core.bootstrap`; state `delivered` comes from the address suffix. Implementation - [`keeper/internal/coremod/bootstrap/delivered.go`](../../keeper/internal/coremod/bootstrap/delivered.go).

**Two transports** (`keeper.yml::push.transport`, [ADR-063 amendment Teleport](../adr/0063-bootstrap-token-delivery.md#amendment-teleport-by-name-transport)): **`direct`** (default) - generic `push.Dial` by `primary_ip` via SshProvider plugin (Authorize/Sign + CA-signed host-cert verify from Vault host-CA); **`teleport`** - by-name via Teleport Proxy (target=SID, not IP; transport+auth+host-verify entirely via Teleport identity-file, Authorize/Sign/Vault-host-CA are not used, retry-to-join). Registration is conditional: direct mode requires a full set of SSH dependencies (`BootstrapProviders` + `BootstrapHostCAs` + `BootstrapDial` in `coremod.Deps`), teleport - dialer only; otherwise the step drops "unknown keeper-side module".

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
- Side: **Keeper-side**. The step **must** carry `on: keeper`.

### Parameters (`params:`)

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `hosts` | array of object `{sid, primary_ip, bootstrap_token}` | required | — | List of VMs. In practice, the CEL expression `${ register.<provision>.hosts }` comes in (output `core.cloud.created`). Empty list → `failed`. |
| `ssh_provider` | string | required | — | SshProvider plugin name (`keeper.yml::plugins.ssh_providers[].name`). **★ In `transport: teleport` DOES NOT define a transport** (Authorize/Sign are not called) - the name goes ONLY to audit-payload. |
| `token_path` | string | optional | `/etc/soul/token` | Path to the token file on the VM. |
| `ssh_user` | string | optional | `root` | SSH user. |
| `ssh_port` | int (1..65535) | optional | `22` | sshd TCP port. |
| `start_soul` | bool | optional | `true` | Unit activation after init: `systemctl daemon-reload && systemctl enable soul && systemctl start soul`. `soul init` (step 5) goes regardless of the flag. |
| `install` | bool | optional | `false` | Full-install mode: before the token, put the entire setup via SSH (see "Two modes of operation" above). Only `transport: teleport`; in direct mode → Validate error. Requires a configured block `keeper.yml::cloud_init` (blueprint source, config-reuse). |
| `join_wait_timeout` | int (seconds) | optional | `360` | Host Teleport-join waiting ceiling (retry-with-backoff until a node appears in the cluster); relevant only in `transport: teleport`. Upon expiration, step `failed` (B1-strict). |

### Output contract (`output:` module)

`register.<name>.*`: `hosts[] = {sid, delivered, started}` + `count`. Plus standard `.changed` (always `true` on success) / `.failed` DSL cores. **★ WITHOUT token in output** - the plain token is visible only in the register of the previous step (`core.cloud.created`, key `bootstrap_token`, masked by `audit.MaskSecrets`); it is not here.

### Security

- Token in STDIN, not in argv (step 4); init step (5) carries the literal unexpanded `$(cat <token_path>)` - the token is expanded by the subshell on the VM, not by the keeper. Audit-payload `bootstrap.delivered` — `{action, ssh_provider, count, sids}`, **without tokens**. The error text is masked (`audit.MaskSecrets`) before the `failed`-event. CA-signed host-cert verify is required (empty host-CA → obvious error). fail-closed Authorize.

### MVP Limits (ADR-063)

- One key-based SshProvider, hosts sequentially. Full-install - only `transport: teleport`.
- **★ C1 - cloud-init CA-signed host-key (required-for-live direct-mode, separate slice).** `push.Dial` trusts only host-cert signed by host-CA (TOFU refusal) - fresh VM must have CA-signed host-key, otherwise handshake is rejected: up to C1 live-e2e in **direct** mode will not work (the module is valid in render L0 Trial + unit tests). For `transport: teleport` C1 **not applicable** - host-verify goes through Teleport CA.

## `core.vault.kv-read`

Explicit reading of the secret from Vault KV (v1/v2, mount version is determined automatically) on the keeper side with a mandatory recording of the audit event `vault.kv-read` (ADR-017(b)). **Keeper-side**, dispatcher `on: keeper`. Registry key - base `core.vault`; state `kv-read` (verb) comes from the address suffix. Exists in parallel with implicit `${ vault(...) }` in CEL: the implicit form is cheap to render, but does not leave an audit record; this module is an explicit form for compliance-accurate reading. Read-only (`changed=false` always). Complete per-module reference with params/output/security - [docs/module/core/vault/README.md](../module/core/vault/README.md).

## `core.vault.kv-present`

Generate-if-absent for Vault KV secrets on the keeper side ([ADR-017 amendment 2026-06-28](../adr/0017-keeper-side-core.md)). **Keeper-side**, dispatcher `on: keeper`. The same module as `kv-read`: Registry key - base `core.vault`; state `kv-present` comes from the address suffix. For each target, it guarantees the existence of a non-empty secret field: absent (no field / `null` / empty string) generates a crypto-random value (`crypto/rand`, bias-free) according to the **password-policy** described by the author (length in characters + alphabet `charset`/`allowed_chars`), present - no-op (does not overwrite). `changed=true` only during real generation; idempotent (rerun/re-create are safe). `destroy` does not clear secrets → re-create reuses the same passwords. Purpose - the service itself generates missing passwords when `create`, the operator does not need to manually pre-seed secrets `vault kv put`.

**Security-invariant (ADR-010):** the generated **value** never goes into register-output / audit-payload / log / OTel / error text - only `path` + names of generated fields come out. register-output - `generated` (map path → \[fields]); audit-event `vault.kv-present` (`source: keeper_internal`) is written only with `changed=true`, payload `{paths}` - without values. Complete per-module reference with params (`targets` / `policy`) / output / security - [docs/module/core/vault/README.md](../module/core/vault/README.md#corevaultkv-present).

## `core.cert.registered` / `core.cert.issued`

Tracking of the incarnation's service TLS certs in the **Warrant** registry ([ADR-017 amendment 2026-07-01/2026-07-09](../adr/0017-keeper-side-core.md), [naming-rules.md → Warrant](../naming-rules.md#domain-entities)) — the basis for auto-rotation by the Reaper. **Keeper-side**, dispatcher `on: keeper`. Registry key — base `core.cert`; state (`registered` / `issued`) comes from the address suffix via `SplitModuleAddr`. Registration in `coremod` is conditional on a configured `CertStore` (same pattern as `core.choir`/`core.vault`) — otherwise the step fails with "unknown keeper-side module". This is about the **service** cert (e.g. Redis server TLS), not the Soul agent's identity cert ([SoulSeed](../soul/identity.md), rotated separately).

Two states — by cert source:

| State | What it does | Secret handling |
|---|---|---|
| `registered` | Records an **already issued** cert(s) into Warrant: reads the PEM from Vault by `vault_ref`, extracts `serial`/`fingerprint`/`not_after` from the x509 itself. For certs issued outside the module (e.g. the initial `rotate_tls`, which already placed material in Vault). | The module never touches it (only reads the cert PEM). |
| `issued` | **Mint+enroll**: Keeper generates a keypair+CSR → signs via Vault PKI → writes cert+key to Vault (`secret/<service>/<incarnation>/tls/<kind>`) → records into Warrant. One step "issue and record". | Generated keeper-side, written to Vault, **never leaves** (R2 invariant [ADR-017](../adr/0017-keeper-side-core.md)). |

Without tracking in Warrant the Reaper is **blind to certs** — the step (`registered` or `issued`) must be present in the service's create/`rotate_tls` scenarios. Both are idempotent (same fingerprint → no-op); output/audit carries only non-secret metadata (`kind`/`serial`/`fingerprint`/`not_after`), never the PEM or the secret material.

### Addressing and side

- Namespace `core`, module `cert`, state `registered` / `issued`.
- Full task name: `module: core.cert.registered` / `module: core.cert.issued`.
- Side: **Keeper-side**. The step **must** carry `on: keeper`.

### Parameters (`params:`)

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `incarnation` | string | required | — | Name of the incarnation the certs belong to (FK Warrant → `incarnation`). |
| `auto_rotate` | bool | optional | `true` | Per-cert auto-rotate flag (written to `warrant.auto_rotate`). `true` = the Reaper may rotate the cert (with the service's `certificate_rotation.enable` — this is the "default yes"); `false` = the cert is tracked but not rotated. |
| `certs` | array of object `{kind, vault_ref}` | **`registered` only** | — | List of already issued certs to record. `kind` ∈ `cert`/`key`/`ca`; `vault_ref` — path to the material in Vault. |
| `kind` | string | **`issued` only** | — | Type of the TLS material being issued; determines the write path `secret/<service>/<incarnation>/tls/<kind>`. |

**★ `pki_role` and `scenario` are NOT params.** The Vault PKI signing role (for `issued`) and the rotation scenario name (for the Reaper) are taken from the service manifest `service.yml::certificate_rotation` ([service/manifest.md → Section `certificate_rotation`](../service/manifest.md#certificate_rotation-section)), **not** from the step's `params`. The scenario author does not choose an arbitrary PKI role: `pki_role` comes from the git-reviewed manifest, the PKI-engine mount from `keeper.yml::vault.pki_mount` ([config.md → `vault`](config.md#vault)). Reason — blast-radius ([ADR-017 amendment 2026-07-09](../adr/0017-keeper-side-core.md)).

### Relation to the Reaper rule `rotate_due_certs`

`core.cert.*` only **records** certs into Warrant; auto-rotation of expiring ones is driven by the Reaper rule **`rotate_due_certs`** (Reaper leader, [ADR-017 amendment 2026-07-01](../adr/0017-keeper-side-core.md)): scan `not_after < NOW()+threshold` → CAS `active→rotating` → keeper-side re-sign (Vault PKI role `pki_role` from the manifest) → `WriteKV` → spawn the rotation scenario (`certificate_rotation.scenario`) **for the whole incarnation at once**. Three rotation gates — service `certificate_rotation.enable` × per-cert `auto_rotate` × cluster `keeper.yml::reaper.rules.rotate_due_certs.enabled` (default OFF+dry_run); config-driven source of `scenario`/`pki_role` (Path B, ~60s cache pinned by `service_version`) — [ADR-017 amendment 2026-07-09](../adr/0017-keeper-side-core.md). Retention of removed certs — Reaper rule `purge_old_certs`.

## Auto-synthesis of `core.module.installed` from `service.yml::modules[]`

The step itself `core.module.installed` - **Soul-side** (delivery of the SoulModule plugin to the host: allow-check → `FetchModule` → verify → hot-register; host side - [soul/modules.md](../soul/modules.md), commit - [ADR-065](../adr/0065-core-module-installed.md)). Keeper **synthesizes** such steps into the run plan from the manifest declaration `service.yml::modules[]` (`{name, ref}`, [service/manifest.md](../service/manifest.md)) - the operator declares the dependency once per service, install-boilerplate is not needed in each scenario ([ADR-065 amendment 2026-07-03](../adr/0065-core-module-installed.md)).

**Synthesis point.** Immediately after expanding `include:` (flat task list, [scenario/orchestration.md §6](../scenario/orchestration.md)) and before Stratify - the same in all places that build the run plan: scenario-runner (apply), check-drift (drift-plan ≡ apply-plan, otherwise the synthesis step would be eternal drift), claim-render Acolyte (reproduces the run-goroutine plan - correlation plan_index/TaskEvent) and L0-trial-harness. Pre-flight/parsing/UI-plane surfaces do not mutate.

**What is inserted.** For each record `modules[]`, which has a consumer task in the plan (task `module:` with the prefix `<ns>.<module>.`), a regular plan task with a marker name is synthesized:

```yaml
- name: install community.redis (service manifest)   # synthesis step marker name
  module: core.module.installed
  params: { name: community.redis, ref: v1.2.0 }     # name+ref - from the manifest entry
```

- **Position** - immediately before the first consumer task; consumer inside `block:` → insertion before the entire block. Several synthesis steps before one task - in manifest order.
- **Without `on:`/`where:`** - a regular roster task: stratified as its consumer, incl. goes **after** roster-refresh-border ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)) - provision-from-zero works without special logic.
- **A module without consumers in the plan is NOT synthesized**; `core.*` entries are skipped (they are already prohibited by manifest validation, `core_module_in_modules_list`).
- `ref` in params - **pin-verification** ([ADR-065(c)](../adr/0065-core-module-installed.md)): the active Sigil permit must be on this ref, otherwise step `failed`.
- The synthesis step goes through render → dispatch → TaskEvent like any task and is visible in the run-view by its marker name.

**Takeover - an explicit step disables synthesis.** An explicit `core.module.installed` with the same **literal** `params.name` in plan suppresses the synthesis of this name - the operator controls the position itself, `ref` and `when:`. `${…}`-CEL in `params.name` cannot be compared literally: synthesis will not be suppressed, a double step is possible - harmless (idempotency by sha256: the binary is already installed → `changed=false`, fetch is not executed).

**Idempotency and errors.** Skip is modular only (sha256 of installed binary == sha of active Sigil permission); plan-level skip no - Keeper does not maintain a register of the installed per-host, the roster changes mid-run. The absence of an entry in `plugins.soul_modules[]` / active Sigil-permission catches the Soul-side allow-check of the step (`module_not_allowed`) - like an explicit step; There is no keeper-side pre-flight gate in MVP (together with the validation-hint "the module is used but not declared" - post-MVP).

**MVP limitation.** Consumers are defined by `module:` scenario tasks (top-level and inside `block:`); a module used **only within destiny** (via `apply:`) is not considered a consumer - it still needs an explicit install step. Push is not affected - modules travel there en masse ([ADR-020](../adr/0020-plugin-infrastructure.md)).

## See also

- [architecture.md → Module model](../architecture.md) - general core/custom model, Soul-side vs Keeper-side, SoulModule protocol.
- [architecture.md → Module addressing](../architecture.md) - format `<namespace>.<module>.<state>`.
- [scenario/orchestration.md §3](../scenario/orchestration.md) - `on:`, step manager between the Soul side and the Keeper side.
- [storage.md](storage.md) - `souls` tables, coven binding.
- [cloud.md](cloud.md) - `core.cloud.provisioned` and a border with coven binding (`core.soul.registered` is a separate step).
- [soul/modules.md](../soul/modules.md) — host side of `core.module.installed`: delivery, verify, cache of custom modules.
- [naming-rules.md → Destiny Modules](../naming-rules.md) - a dictionary of names.
