# core.soul.registered

Linking Soul (by `SID`) to a set of stable Coven marks in the keeper registry
(tables `souls` + coven). **Keeper-side**, dispatcher `on: keeper` - step
is executed on Keeper itself, not on the host (unlike Soul-side core like
`core.pkg`/`core.file`). Launch without `on: keeper` - scenario validation error.
Implementation - [`keeper/internal/coremod/soul/registered.go`](../../../../keeper/internal/coremod/soul/registered.go).

If there is no entry in `souls` for this `sid` yet, the module creates it under
`status: pending` (new host added by the script - host branch `add_replica`
or after cloud-create via `core.cloud.provisioned`). Bootstrap tokens /
SoulSeed module **doesn't** write out
is an onboarding competency.

Accepts a **string OR list of SIDs** and optionally carries an **onboarding barrier**
`await_online` (blockingly waiting for registered Souls to become online) —
[ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md),
[`await.go`](../../../../keeper/internal/coremod/soul/await.go).

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `registered` | Soul with the specified `sid` is in the registry and is tied to the specified set of Coven tags (by `mode`). | `changed=true` if the record `souls` was created by the module **or** the resulting coven set is different from the current one (order-independent reconciliation). The set matched and there was already an entry - `changed=false`. |

## registered — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `sid` | string **or** array of string | required | `SID` Soul (FQDN). Each is validated as an FQDN (`keepersoul.ValidSID`); invalid - the step falls. Accepts a single string **or** list ([ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)) - the list usually comes with the CEL expression `${ register.<provision>.hosts }`; literal list `sid: [a, b]` `soul-lint` fails statically (manifest declares `string`), see [list-SID](#list-sid). |
| `coven` | array of string | required | A set of stable Coven tags. Each label is validated as kebab-case (`keepersoul.ValidCoven`, 1..63 characters); garbage like `Prod`/`a_b` - the step decreases. When listed, `sid` applies to each SID. |
| `mode` | string | optional (default `append`) | The strategy for applying the `coven` set to existing labels is: `append` / `replace` / `remove`. Unknown value - step drops. |
| `refresh_soulprint` | bool | optional (default `false`) | **Implemented (S2/S3 [ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)).** `true` - the step becomes a passage-defining boundary (Stratify), after its success, the scenario-runner will re-resolve the roster before the next Passage (live snapshot); output `refreshed` echoes the flag. Together with `await_online: true`, it tightens the barrier to facts-wait (see barrier). |
| `await_online` | bool | optional (default `false`) | **Onboarding barrier** ([ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md), [`await.go`](../../../../keeper/internal/coremod/soul/await.go)). `true` - after registering all SIDs, the step waits blocking for them to be ready: online (Redis SID-lease); with `refresh_soulprint: true` - additionally recorded first typed soulprint. Without a configured presence checker on the keeper → step `failed`. |
| `await_timeout` | duration | **required at `await_online: true`** | Upper limit of expectation. Without it, validation fails at `await_online: true`. Bounded at the top by `keeper.yml::max_await_timeout`. |
| `await_min_count` | int | optional (default = SID number) | Minimum online hosts for success. Default - all registered SIDs. Range `0 < await_min_count ≤ len(sids)`. |
| `await_poll_interval` | duration | optional (default `2s`) | Presence polling period during the barrier. |

### Semantics `mode`

| `mode` | Final coven set | Edge behavior |
|---|---|---|
| `append` (default) | existing ∪ transferred | repeat with the same set - no-op (`changed=false`). |
| `replace` | transferred (not mentioned - deleted) | empty `coven: []` - **error** (footgun protection: host must save at least one tag). |
| `remove` | existing\transferred | tags that are not on the host are skipped without error; Only removes those who are really attached. |

## list-SID

`sid` accepts a **string OR a list of strings** ([ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)). The target case is that one create-scenario creates N VMs via `core.cloud.provisioned` (their `sid` come as a list in `register.<provision>.hosts`), and this step registers them and (with `await_online`) waits for onboarding with one barrier. The `coven` passed applies to each SID; `await_online`-barrier aggregates presence over the entire set (general `await_min_count`).

A single string remains valid (normalized to a list of one element). **Input output form:** single `sid` → `sid` by string (historical form), list → array.

Manifest declares `sid` as `type: string` (the stripped-down DSL does not express the union `string|list`): a single literal string passes `soul-lint`, **list comes as a CEL expression** `${ register.<step>.hosts }` (not statically typed, [ADR-010](../../../adr/0010-templating.md)). Literal list `sid: [a, b]` static type-check `soul-lint` **doesn't** pass - in practice the SID list is always from `register.*`, runtime (`StringOrSliceParam`) takes both forms.

## Onboarding barrier (`await_online`)

For `await_online: true` ([ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md), [`await.go`](../../../../keeper/internal/coremod/soul/await.go)):

1. The step first registers all SIDs (souls+coven, as without barrier).
2. Then blocking pollutes readiness with a period of `await_poll_interval` under the general `await_timeout`, until there are `≥ await_min_count` ready hosts.

**Source "online" - Redis SID-lease** (live EventStream, `SoulsStreamAlive`), **not** PG `souls.status` (lagging lifecycle snapshot).

**Facts-wait at `refresh_soulprint: true`** ([ADR-061 amendment 2026-07-02](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)): SID "ready" = online **and** typed soulprint written in PG (`souls.soulprint_facts IS NOT NULL`). One lease is not enough - the render of the next Passage reads `soulprint.self.*`, and the recording of the initial report is asynchronous (best-effort, [ADR-018](../../../adr/0018-soulprint-typed.md)): on provision-from-zero the race gave `render_failed` "no such key". On rerun / `create_from_souls` facts already in PG → pass on the first poll, zero wait. Without `refresh_soulprint` - presence-only, facts are not polled.

**B1-strict:** lack of quorum by timeout → step `failed` → fail-stop run → `incarnation.state` is not committed → `incarnation.status: error_locked`. `register.<name>.pending` carries failed SIDs; with facts-wait, the classes of shortfall in the message are separated - `not online: [sids]` vs `online but factless: [sids]`. Persistent polling error (Redis presence / PG facts check) or run cancellation - also `failed`. `await_online: true` without a presence-checker on the keeper → `failed` (silent success is not allowed).

**Ceiling `await_timeout`.** `keeper.yml::max_await_timeout` (duration, default `30m`, [`shared/config/keeper.go`](../../../../shared/config/keeper.go)) limits `await_timeout` from above. `await_timeout` > ceiling → step `failed` **to** polling (fail-closed: obvious error, not silent truncation) - DoS-guard against `await_timeout: 100h` holding run-goroutine/Acolyte-worker.

## Capabilities / side-effects

- **Keeper-side, does not touch the host.** All side-effects are in Keeper registries
(Postgres `souls` + coven), and not in Soul's file system/processes.
- **Changes the registry `souls`:** `UpdateCoven` when the set of labels changes; at
no entry - `Insert` new under `status: pending`, `transport: agent`,
empty coven (fields `LastSeenAt`/`CreatedByAID` - `null`: cloud-create /
scenario-host-add do not carry an operator).
- **Does not execute subprocesses** and does not issue bootstrap tokens / SoulSeed.
- **Idempotency by design:** the extra `UPDATE` is not executed if
the resulting set is the same as the current one (`sameSet`, order-independent comparison).

## Security

- **Keeper-side, not Soul-side - `root`/capability semantics are not applicable.** Step
is executed in the Keeper process (`on: keeper` dispatcher), and not by the `soul` agent on
host. The module does not have a manifest with `required_capabilities`
([`soul.yaml`](../../../../shared/coremanifest/soul.yaml) declares only
states/input) is a keeper-internal operation on Postgres, not a host plugin.
- **Writing to the registry `souls` is a privileged Keeper operation.** Module
creates/modifies registry entries (`Insert`/`UpdateCoven`,
  [`registered.go`](../../../../keeper/internal/coremod/soul/registered.go)):
adding a host under `status: pending` and binding Coven tags. Access to launch
such a scenario is regulated by the RBAC operator at the level of the scenario run
([rbac.md](../../../keeper/rbac.md)) - the core module itself does not have a separate permission
announces. `CreatedByAID` of the created entry is `null` (this is keeper-internal
action: cloud-create / scenario-host-add do not carry a specific Archon).
- **Login validation against garbage injection into the registry.** `sid` is validated as FQDN
(`keepersoul.ValidSID`), each label `coven` - as kebab-case 1..63
(`keepersoul.ValidCoven`); invalid - the step falls and is not included in the register.
Symmetrically to the API boundary `POST /v1/souls` so that the scenario path is not "black"
move" bypassing checks.
- **Footgun protection `mode: replace`.** Empty `coven: []` with `replace` - error:
the host must save at least one Coven label (otherwise it would lose the root coven
incarnation and would fall out of targeting).
- **Does not issue bootstrap tokens and SoulSeed** - this is the competence of onboarding, not
this module; The module does not produce or reveal secrets.
- **DoS-guard of the onboarding barrier.** `await_timeout` is limited from above by the operator-
ceiling `keeper.yml::max_await_timeout` (default `30m`, fail-closed): step with
overpriced `await_timeout` is rejected `failed` BEFORE the survey, not "quietly"
cut off." Without the ceiling, the malicious/erroneous `await_timeout` would hold
run-goroutine / Acolyte-worker busy (ADR-061).

## Output / register

The module outputs to `register.<name>.*`:

| Field | Type | Description |
|---|---|---|
| `sid` | string **or** array of string | Input echo: **string** for single `sid`, **array** for list ([list-SID](#list-sid)). |
| `coven` | array of string | **The final** set of covens after applying `mode`, not the passed argument. For list `sid` — dialing the first SID. |
| `mode` | string | Applied `mode`. |
| `created` | bool | `true` if at least one `souls` record was created by the module; `false` if all already existed. |
| `refreshed` | bool | Echo the value `refresh_soulprint`: `true` ⇒ re-resolve roster before the next Passage is guaranteed to execute (S3 [ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)). |
| `removed` | array of string | Only with `mode: remove`: marks actually cleared. Empty array otherwise. |
| `online` | array of string | **Only with `await_online: true`**: SIDs that have become online (Redis SID-lease) at the time of barrier success/timeout. |
| `pending` | array of string | **Only for `await_online: true`**: SIDs that did not time out online (diagnosis B1-strict). |
| `satisfied` | bool | **Only for `await_online: true`**: whether `await_min_count` is reached (`true` if successful; `false` if `failed`). When `refresh_soulprint: true` is considered "ready" (online **and** soulprint recorded). |

The `online`/`pending`/`satisfied` fields are only present with `await_online: true`. Plus standard `.changed` / `.failed` DSL cores.

## Example

```yaml
# Register the new Soul in the incarnation registry: bind it to the root
# coven (mode: append default). on: keeper is required - this is a keeper-side step.
- name: Bind new replica to the incarnation root coven
  on: keeper
  module: core.soul.registered
  params:
    sid:   "${ vars.new_sid }"
    coven: ["${ incarnation.name }"]
```

```yaml
# Register the list of created VMs and blockingly wait for them to be onboarded in one step
# (ADR-061). on: keeper is required.
- name: Register provisioned shards and await onboarding
  on: keeper
  module: core.soul.registered
  register: shards
  params:
    sid:           "${ register.provision.hosts }"   # list of SIDs from cloud-provision
    coven:         ["${ incarnation.name }"]
    await_online:  true
    await_timeout: 10m                                # ≤ keeper.yml::max_await_timeout
```

(see [`examples/destiny/coven-assign/tasks/main.yml`](../../../../examples/destiny/coven-assign/tasks/main.yml) - destiny-wrapper around `core.soul.registered`, and [`examples/service/keeper-register/scenario/create/main.yml`](../../../../examples/service/keeper-register/scenario/create/main.yml) - keeper-side dispatch in scenario)

## See also

- [README.md](../../README.md) - directory of core modules.
- [keeper/modules.md](../../../keeper/modules.md) - regulatory spec for Keeper-side core modules (`on: keeper` manager).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-step-target---on) - `on:`, step manager between the Soul side and the Keeper side.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-017](../../../adr/0017-keeper-side-core.md) — Keeper-side core modules.
- [ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md) - onboarding barrier `await_online` (amendment 2026-07-02: + facts-wait at `refresh_soulprint`), list-SID, `refresh_soulprint` re-resolve roster (S2/S3 implemented).
