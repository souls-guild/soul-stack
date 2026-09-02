# core.cloud

Creating / deleting cloud instances via the CloudDriver plugin (`soul-cloud-*`).
**Keeper-side**, dispatcher `on: keeper` - the step is executed on the Keeper itself, not
on the host (as opposed to Soul-side core). Starting without `on: keeper` is an error
validation scenario. Replaces the earlier "destiny `cloud-provision`" pattern with
`on: keeper`" ([ADR-017](../../../adr/0017-keeper-side-core.md):
this is a keeper-side operation, not a mission package for Soul). Implementation -
[`keeper/internal/coremod/cloud/provisioned.go`](../../../../keeper/internal/coremod/cloud/provisioned.go).

> **★ Author address - `core.cloud.created` / `core.cloud.destroyed`** (base `core.cloud` + state).
> This is what the operator writes in `module:`. Form `core.cloud.provisioned` **NOT
> exists** as task address: registry ([`registry.go`](../../../../keeper/internal/coremod/registry.go))
> divides the address into base (`core.cloud`, goes to `Lookup`) + state (`created`/`destroyed`,
> goes to `ApplyRequest.state`), and `provisioned` is an unknown state (integration test
> catches her as fail). "provisioned" is the historical name of the Go package and the wording of ADR-017,
> is not an author-facing address. The name of this file is left as `core/cloud/` based on the base name.

CloudDriver is called via PluginHost (gRPC-over-stdio plugin `soul-cloud-<provider>`).
SID of the created host = FQDN returned by the provider (`VmInfo.fqdn`); VM without
fqdn - step drops (cannot be used as SID).

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `created` | Request from provider `count` VM; for each - `INSERT` to `souls` (`status: pending`) + `INSERT` to `bootstrap_tokens` (one token per VM). | `changed=true` always (cloud-create is an imperative operation, not idempotent at the module level; repeat creates new VMs). |
| `destroyed` | `PluginHost.Destroy(vm_ids)` at the provider; then cascade with one PG transaction over the registers for the transferred `sids`. | `changed=true` if the provider returned a non-empty list of remote VMs; empty list - `changed=false`. |
| `resized` | `PluginHost.Resize(vm_ids, desired)` at the provider - change of `cpu_cores` / `ram_mb` / `disk_gb` on living VMs. The registries are untouched: a resize changes resources, not identity. | `changed=true` always (converging to the target size is the driver's responsibility; at module level a resize is a changing operation). A VM the driver could not resize lands in `output.errors` and does **not** fail the step - some of the batch may have succeeded. |

## Provider source — params (all three states)

The CloudDriver comes from **exactly one** of two sources (NIM-668); the rules are identical for `created`, `destroyed` and `resized`. Full description with rationale - [keeper/cloud.md → Two sources for the driver](../../../keeper/cloud.md#two-sources-for-the-driver-registry-or-inline).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `provider` | string | one source is required | Name of a row of the `providers` registry (**not** the plugin name); the row carries `type` / `region` / `credentials_ref` / `fqdn_suffix`. |
| `driver` | string | with `credentials` | The CloudDriver plugin alias → `soul-cloud-<driver>` via PluginHost. The inline equivalent of the registry's `type` column - it looks nothing up. |
| `credentials` | string `vault:<mount>/<path>` | with `driver` | **A reference, never a value.** The vault-resolve phase ([ADR-010](../../../adr/0010-templating.md) phase 1) replaces it with the secret map before the module runs, so the module receives an object; a plain string that survived to here is refused (a credential written into a definition, or a reference nothing resolved) and the refusal does not echo it. ★ Write the ref **literally**: vault-resolve walks the raw params before the CEL phase, so `credentials: "${ vars.cloud.credentials }"` produces a ref one phase too late and is refused with its own message. |
| `region` | string | optional, inline only | Merged into the credentials map under the same key the registry path writes, so a driver reads it from one place either way. Absent - a `region` the secret itself carries is left alone. |
| `fqdn_suffix` | string | optional, inline only, `created` | DNS suffix for SID prediction; the inline equivalent of `providers.fqdn_suffix`. Required by `self_onboard: true`. |
| `profile` | string \| object | optional | **string** = name of a row of the `profiles` registry; **object** = the VM spec itself, passed to CloudDriver as-is. Independent of the source above: an inline `driver` may name a registry profile, a registry `provider` may take an inline spec. The manifest declares `map` (the schema DSL has no union type), so the string form has to arrive through an expression - a literal `profile: some-row` is a soul-lint `param_type_mismatch`. |

Refused, each with the reason named:

- `provider` together with `driver`/`credentials` - two sources; and neither of them - no source. Refusals, not a silent preference: the audit event would name one source while the run used the other.
- `driver` without `credentials`, or `credentials` without `driver` - the missing half is named; the step already chose the inline source, so it is not told "no source set".
- `region` or `fqdn_suffix` together with `provider` - in registry mode those are columns of the row, and a step overriding `region` would move a fleet to another data centre through a param the author read as documentation. The error names the column to change.
- `profile` as a string with no `profiles` registry configured - the error names the inline object form as the way out.

## created — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| *(provider source)* | | one source required | See [above](#provider-source--params-all-three-states). |
| `count` | int | optional (default `1`) | How many VMs to create. `< 1` - validation error. |
| `name` | string | optional (required by `self_onboard`) | Base name of the VM batch → `CreateRequest.name`; the driver names VMs `<name>-<index>`. |
| `userdata` | string | optional | Cloud-init blob passed through as-is. Mutually exclusive with `generate_userdata` / `self_onboard`. |
| `generate_userdata` | bool | optional | Keeper renders the userdata itself from `keeper.yml::cloud_init`. |
| `self_onboard` | bool | optional | Per-VM tokens baked into userdata, no delivery step ([Option T](../../../keeper/cloud.md#self-onboard-option-t)). Requires `name` and a FQDN suffix. |

## destroyed — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| *(provider source)* | | one source required | See [above](#provider-source--params-all-three-states). |
| `vm_ids` | array of string | required | Provider-side ID of the VMs to be deleted (the `sid↔vm_id` link is held by the caller). |
| `sids` | array of string | optional | `SID`s for which to cascade in the registries after a successful destroy. If omitted/empty, cascade is not executed. |

## resized — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| *(provider source)* | | one source required | See [above](#provider-source--params-all-three-states). |
| `vm_ids` | array of string | required | Provider-side ID of the VMs to resize. |
| `desired` | object | required | Target shape: `cpu_cores` / `ram_mb` / `disk_gb`; at least one must be `> 0` (all-zero is a meaningless no-op), none may be negative. |
| `allow_downtime` | bool | optional (default `false`) | Consent passed on to the driver: it may reboot the VM to apply the new shape. A driver that would need downtime without it reports the VM as failed rather than rebooting it. |

> Cascade (`destroyed`) is executed **after** a successful `PluginHost.Destroy`:
> if cloud-destroy fails, the registries remain untouched (the host is still "alive"
> from the provider's point of view). Cascade transfers in one PG transaction
> `souls → destroyed`, active `soul_seeds → orphaned`, active
> `bootstrap_tokens → burned`. If `sids` is not empty but cascade-store is not
> configured in the assembly - the step fails with an obvious error.

## Capabilities / side-effects

- **Keeper-side, does not affect the host.** Side-effects - from the cloud provider and in
Keeper registries (Postgres), and not on the Soul host.
- **Creates/deletes cloud VMs** via CloudDriver plugin (external
billing side-effect - real provider instances).
- **`created`:** `INSERT` to `souls` (`status: pending`, `transport: agent`) +
`INSERT` bootstrap token per VM.
- **`destroyed`:** if `sids` is present - cascade transaction over
  `souls`/`soul_seeds`/`bootstrap_tokens`.
- **Writes audit-event** `cloud.provisioned` (if audit-writer is configured):
for `created` - `{action, provider, count, vm_ids}`; for `destroyed` —
`{action, provider, vm_ids, sids, cascade-counts}`. Audit fails a step
(compliance-invariant, event required).

## Security

- **Keeper-side, not Soul-side - `root`/capability semantics are not applicable.** Step
is executed in the Keeper process (`on: keeper`); side-effects - from the cloud provider
(via CloudDriver plugin `soul-cloud-*`) and in Keeper's Postgres registries, not on
host. The module does not have a manifest with `required_capabilities` (keeper-internal operation,
not a host plugin). The launch of such a scenario is regulated by the RBAC operator
([rbac.md](../../../keeper/rbac.md)); the created records `souls` are written with
  `CreatedByAID: null` (keeper-internal action).
- **Real financial side-effect (`created`).** Step creates real VMs
provider is billing. `created` reports `changed=true` always, but a re-run
**converges** rather than duplicating ([ADR-017 amendments 2026-07-24 and
2026-07-26](../../../adr/0017-keeper-side-core.md)) - on both layers:
  - **VM layer (driver):** `CloudDriver.Create` scans the provider for this run's
machines, reuses the live ones, creates only what is missing and returns
`VmInfo` for the whole roster.
  - **Registry layer:** a repeated `create` re-arms the `souls` records its own
earlier attempt left behind (`pending` / `destroyed`, this incarnation's or
unbound) instead of dying on the PK, and passes through - untouched and without
a new bootstrap token - the hosts of this incarnation that are already up
(`connected` / `disconnected`). Records that are `revoked`/`expired`, or belong
to another incarnation, are refused.

  Full table - see [cloud.md → Re-running create](../../../keeper/cloud.md#re-running-create-provision-idempotency-nim-170--nim-189).
- **`destroyed` - destructive cascade operation.** `PluginHost.Destroy(vm_ids)`
physically destroys instances; then (if `sids` is non-empty) one PG transaction
translates `souls → destroyed`, active `soul_seeds → orphaned`,
  `bootstrap_tokens → burned`
  ([`provisioned.go`](../../../../keeper/internal/coremod/cloud/provisioned.go)).
Order protects registries: cascade runs **after** successful cloud-destroy -
if destroy fails, the registries remain untouched (the host is still "alive" with the provider).
The `sid↔vm_id` link is held by the caller - an error in it will lead to destroying the wrong VM,
so the source of `vm_ids`/`sids` must be trusted.
- **Cascade `souls→destroyed` precedes the host-teardown in the destroy run (NIM-56).**
`core.cloud.destroyed` - keeper task; according to the invariant "keeper tasks come FIRST in
her Passage" she removes the VM and cascades `souls→destroyed` BEFORE host-fan-out
host-teardown steps of the same destroy script (for services with Soul-side teardown, for example.
`dragonfly`). Such a host step will be dispatched to an already removed host - in the destroy run
(`TerminalDestroy`) scenario-runner when claiming treats the host taken as OWN
destroy-cascade of this run (`souls.status == 'destroyed'`), as benign-terminal
**`no_match`**, NOT `dispatch_failed`: the barrier counts it towards the success side, and
teardown does not crash in `destroy_failed`. The discriminator is unambiguous - the only writer
status `destroyed` - this cascade transaction (`CascadeDestroy`); any other loss
host from the roster (disconnected / revoked / not-found) remains a failure (**fail-closed**).
- **Plain bootstrap-token in register-output (`created`).** `hosts[].bootstrap_token`
- **plain** one-time token, intentionally in output: cloud-init flow required
pass it to the VM on initial boot (the only time the plain token
visible; in the database - only hash, cannot be restored). Secrecy is maintained
substring filter [`audit.MaskSecrets`](../../../../shared/audit/) (fragment
`token`) on **all** register-outputs (audit-log / OTel / SSE / any
logs). **Any new register-output channel must pass the payload through
`audit.MaskSecrets`; rename the key `bootstrap_token` without checking the filter
you can't** - otherwise one-time token leak.
- **Required audit-event `cloud.provisioned`.** Written for both `created` and
`destroyed`; audit-fail **fails step** (compliance-invariant - destructive/
billing operation should not happen silently). In audit-payload - `provider`,
`vm_ids`, `sids`, cascade counters, but **not** plain tokens.

## Output / register

`created` gives:

| Field | Type | Description |
|---|---|---|
| `hosts` | array of objects | One entry per VM: `{sid, vm_id, primary_ip, attributes?, bootstrap_token}`. A host that was already up when the run started carries `onboarded: true` and **no** `bootstrap_token` - the delivery step skips it ([ADR-017 amendment 2026-07-26](../../../adr/0017-keeper-side-core.md)). |
| `count` | number | Number of VMs in the roster (created plus reused by the driver). |
| `vm_ids` | array of string | Provider-side ID of every VM of the roster - including the ones that were already there. `covenant.yml` writes this into `incarnation.state.provisioned_vm_ids`, so a short list would strand VMs at the provider on day-2 destroy. |
| `action` | string | `created`. |
| `reused` | number | How many `souls` records were taken over from an earlier provision attempt instead of created; `0` on a clean run ([ADR-017 amendment 2026-07-24](../../../adr/0017-keeper-side-core.md)). |
| `existing` | number | How many hosts were already up and were passed through untouched, without a new bootstrap token; `0` on a clean run ([ADR-017 amendment 2026-07-26](../../../adr/0017-keeper-side-core.md)). |

> **WARNING (security).** `hosts[].bootstrap_token` is a **plain** one-time use
> token. It is intentionally in register-output: cloud-init flow is obliged to transfer it to
> VM at initial boot - this is the only time when the plain token is visible
> (only the hash is stored in the database, it cannot be restored). Key privacy
> `bootstrap_token` is held by the substring filter [`audit.MaskSecrets`](../../../../shared/audit/)
> (fragment `token`) on **all** register-outputs (audit-log / OTel / SSE
> / any logs). Any new register-output channel must run payload
> via `audit.MaskSecrets`; You cannot rename a key without checking the filter.

`destroyed` gives:

| Field | Type | Description |
|---|---|---|
| `action` | string | `destroyed`. |
| `vm_ids` | array of string | Actually deleted by the VM provider. |
| `sids` | array of string | Echoes transmitted by `sids`. |
| `destroyed_n` | number | Number of remote VMs. |
| `souls_updated` / `seeds_orphaned` / `tokens_burned` | number | Cascade counters (0 if `sids` is not passed). |

## Example

```yaml
# If necessary, create a VM via CloudDriver. on: keeper required -
# this is a keeper-side core. when:-guard - spawn is optional.
- name: provision
  when: has(input.spawn)
  module: core.cloud.provisioned
  params:
    provider: "${ input.spawn.provider }"
    profile:  "${ input.spawn.profile }"
    count:    "${ input.spawn.count }"
```

(from [`examples/service/example-cloud-bootstrap/scenario/create/main.yml`](../../../../examples/service/example-cloud-bootstrap/scenario/create/main.yml);
example transmits exactly `provider`/`profile`/`count` - this is the full set of params,
which `provisioned.go` validates for `created`).

## See also

- [README.md](../../README.md) - directory of core modules.
- [keeper/modules.md](../../../keeper/modules.md) - regulatory spec for Keeper-side core modules (`on: keeper` manager).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-step-target---on) - `on:`, step manager between the Soul side and the Keeper side.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-017](../../../adr/0017-keeper-side-core.md) — Keeper-side core modules, cascade at `destroyed`.
