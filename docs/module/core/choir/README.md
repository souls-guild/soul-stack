# core.choir.present / core.choir.absent

Editing Voice's membership in the Choir incarnation (ADR-044): declared entity -
"SID is the Voice of the specified Choir of this incarnation" (declared part
choir). **Keeper-side**, dispatcher `on: keeper` - the step is executed on the actual
Keeper, not on the host (unlike Soul-side core like `core.pkg`/`core.file`).
Launch without `on: keeper` - scenario validation error. Implementation -
[`keeper/internal/coremod/choir/member.go`](../../../../keeper/internal/coremod/choir/member.go).

Author form of task address - `core.choir.present` / `core.choir.absent`
(base `core.choir` + state, symmetrical `core.file.present`/`core.file.absent`
Soul-side). State comes to the module in `ApplyRequest.state` via `SplitModuleAddr`
and dispatched within the implementation (see
[keeper/modules.md → "Registration and dispatching"](../../../keeper/modules.md)).
The module is registered in the keeper-side Registry **only if there is a** dependency
`Deps.ChoirStore`; in a build without choir scripts the step drops to "unknown keeper-side"
module" (like any unconnected one).

## States

| State | Action | Idempotency (when `changed=true`) |
|---|---|---|
| `present` (default if state is empty) | `AddVoice` - SID becomes the Voice of the Choir. | `changed=true` if Voice is added. Voice already exists (`ErrVoiceExists`) → `changed=false`, not an error. |
| `absent` | `RemoveVoice` - membership is canceled. | `changed=true` if Voice is removed. There is no voice (`ErrVoiceNotFound`) → `changed=false`, not an error. |

Unknown state (not `present`/`absent`) - step drops. Before mutation module
validates the existence of incarnation (`IncarnationExists`); missing - step
falls (otherwise `absent` on the typo of the incarnation name would have been quietly returned
`ErrVoiceNotFound`).

## present — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `incarnation` | string | required | The name of the incarnation to which Choir belongs. Checks for existence. |
| `choir` | string | required | Choir's name. Validated by `ValidChoirName`; garbage - the step falls. |
| `sid` | string | required | `SID` host-Voice (FQDN). Validated by `ValidSID`; invalid - the step falls. |
| `role` | string | optional | Voice part in Choir. |
| `position` | int (≥ 0) | optional | Voice position. Negative - the step decreases. |

## absent — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `incarnation` | string | required | Incarnation name. Checks for existence. |
| `choir` | string | required | Choir's name. Validated by `ValidChoirName`. |
| `sid` | string | required | `SID` host-Voice (FQDN). Validated by `ValidSID`. |

`role` / `position` for `absent` are not used (withdrawal - in threes
`incarnation`/`choir`/`sid`).

## Capabilities / side-effects

- **Keeper-side, does not touch the host.** All side-effects are in the Keeper registry
(Postgres `incarnation_choir_voices`), not on Soul's filesystem/processes.
- **Changes the Choir membership registry:** `AddVoice` (`present`) / `RemoveVoice`
(`absent`) via `Store` (cont. - `choir.NewPGStore`). This is a membership edit.
Voice, not the creation of the Choir itself or the incarnation.
- **Does not execute subprocesses** and does not deliver anything to Soul.
- **Idempotency by design:** repeated `present` already-Voice
(`ErrVoiceExists`) and `absent` missing Voice (`ErrVoiceNotFound`) -
`changed=false`, not an error.

## Security

- **Keeper-side, not Soul-side - `root`/capability semantics are not applicable.** Step
is executed in the Keeper process (`on: keeper` dispatcher), and not by the `soul` agent
on the host. The module does not have a manifest with `required_capabilities` - this is
keeper-internal operation on Postgres, not a host plugin.
- **Edit membership is a privileged Keeper operation.** Launch access
scenario with this step is regulated by the RBAC operator at the level of the scenario run
([rbac.md](../../../keeper/rbac.md)); the core module itself does not have a separate permission
announces.
- **Membership invariant (ADR-044) is reused, not duplicated.** Voice can
add only for a SID that is already a member of the incarnation - this guarantees
choir-CRUD (`AddVoice → ErrNotMembers`), not this module. `ErrNotMembers` →
`failed`-event (the run goes to onfail / `error_locked`).
- **Login validation against garbage injection into the registry.** `choir` is checked
`ValidChoirName`, `sid` - `ValidSID` (FQDN); invalid - the step falls, to the register
doesn't hit. The existence of an incarnation is verified before mutation.

## Limitations (S-T5, future - not implemented)

- **Cross-incarnation guard** (`param.incarnation` == incarnation of the current
run): run-context is not available to the module (ADR-044 / architect A1). Module
trusts param `incarnation` and only validates its existence. Hard
guard is a separate task (RunContext injection into keeper-dispatch).
- **Roster-growth** (new Voice visible to next run step) - not implemented.

## Output / register

`present` returns to `register.<name>.*`:

| Field | Type | Description |
|---|---|---|
| `incarnation` | string | Echo `params.incarnation`. |
| `choir` | string | Echo `params.choir`. |
| `sid` | string | Echo `params.sid`. |
| `state` | string | `present`. |
| `added` | bool | `true` if Voice was added; `false` if it already existed. |

`absent` gives:

| Field | Type | Description |
|---|---|---|
| `incarnation` | string | Echo `params.incarnation`. |
| `choir` | string | Echo `params.choir`. |
| `sid` | string | Echo `params.sid`. |
| `state` | string | `absent`. |
| `removed` | bool | `true` if Voice was removed; `false` if it was not there. |

Plus standard `.changed` / `.failed` DSL cores.

## Contact soulprint

After `present`/`absent` the host's Choir membership is mapped to CEL as
registry projection `soulprint.self.choirs` and `soulprint.hosts[].choirs`
(mirror `covens`, source - `incarnation_choir_voices`, not collected fact
`SoulprintFacts`; see
[soul/soulprint.md → "Soulprint border ↔ souls-registry"](../../../soul/soulprint.md)).
Roster-growth within one run has not yet been implemented (see restrictions
above): the new Voice is visible to subsequent runs.

## Example

```yaml
# Add a host to the Choir 'replicas' incarnations (present default).
# on: keeper is required - this is a keeper-side step.
- name: Add the new replica to the replicas choir
  on: keeper
  module: core.choir.present
  params:
    incarnation: "${ incarnation.name }"
    choir:       replicas
    sid:         "${ vars.new_sid }"
    role:        replica
```

```yaml
# Removing membership when a host is removed from a role.
- name: Remove the host from the replicas choir
  on: keeper
  module: core.choir.absent
  params:
    incarnation: "${ incarnation.name }"
    choir:       replicas
    sid:         "${ input.target_sid }"
```

> **Deferred (backlog).** Call `core.choir.present`/`absent` to `examples/`
> not yet - the examples above are compiled as minimal valid code under the contract.
> Replacement with a link to a real scenario-example has been postponed until it becomes available
> corresponding use-case.

## See also

- [README.md](../../README.md) - directory of core modules.
- [keeper/modules.md](../../../keeper/modules.md) - regulatory spec of Keeper-side core modules (dispatcher `on: keeper`, parsing `base`+`state`).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-step-target---on) - `on:`, step manager between the Soul side and the Keeper side.
- [soul/soulprint.md](../../../soul/soulprint.md) - registry projection `soulprint.self.choirs` / `soulprint.hosts[].choirs`.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
