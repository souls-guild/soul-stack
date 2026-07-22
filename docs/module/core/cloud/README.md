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

## created — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `provider` | string | required | Provider name → plugin `soul-cloud-<provider>` via PluginHost. |
| `profile` | object | optional | Provider profile (struct). Passed to CloudDriver as a profile map. |
| `count` | int | optional (default `1`) | How many VMs to create. `< 1` - validation error. |

## destroyed — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `provider` | string | required | Provider name. |
| `vm_ids` | array of string | required | Provider-side ID of the VMs to be deleted (the `sid↔vm_id` link is held by the caller). |
| `sids` | array of string | optional | `SID`s for which to cascade in the registries after a successful destroy. If omitted/empty, cascade is not executed. |

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
provider is billing. `created` **not idempotent** constructively
(`changed=true` always): repeating the step creates **new** VMs rather than checking against
existing. Manage guard replay at scenario level
(`when:`/`changed_when:`) without relying on module idempotency.
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
| `hosts` | array of objects | One entry per VM: `{sid, vm_id, primary_ip, attributes?, bootstrap_token}`. |
| `count` | number | Number of VMs created. |
| `vm_ids` | array of string | Provider-side ID of the created VMs. |
| `action` | string | `created`. |

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
  on: keeper
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
