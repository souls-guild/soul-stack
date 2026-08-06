# Incarnation - MCP-tools for the life cycle of runtime instances

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.incarnation.*` (creating / running scripts / reading / unlock / upgrade / destroy / traits-set / membership bind-member / unbind-member / members). Transport, auth, tool declaration format, async-convention `_apply_id`, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api.md → Incarnation](../operator-api/incarnations.md).

### Incarnation (11)

#### `keeper.incarnation.create`

Creating an instance - launching the selected starting script (or a bare incarnation if the service does not have create scripts). Permission: `incarnation.create`. Endpoint: [`POST /v1/incarnations`](../operator-api/incarnations.md). Async: **yes** (for bare - sync, without running).

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (kebab-case) | yes | Name of the new instance. |
| `service` | `string` | yes | Service name. |
| `covens` | `array<string>` | optional | Declared env-Coven-tags ([ADR-008](../../adr/0008-coven-stable-tags.md) amendment a). |
| `traits` | `object` | optional | Operator-set trait incarnation marks (key → `scalar`\|`list of scalars`, [ADR-060](../../adr/0060-traits.md)). Placed in `incarnation.traits` and nowhere else - member hosts are not touched ([NIM-281](../../adr/0008-coven-stable-tags.md#amendment-2026-08-05-nim-281-a-label-is-never-inherited)). Day-2 replacement - `keeper.incarnation.traits-set`. |
| `create_scenario` | `string` | conditional | The name of the starting script (scenario with `create: true`). Required, if the service offers ≥1 create script (empty → `validation-failed` with a list of valid ones); value out of set → `validation-failed`. A service without create scripts → gives a bare incarnation. Details - [operator-api/incarnations.md → Selecting a starting script](../operator-api/incarnations.md). |
| `input` | `object` | optional | Input of the selected startup script (validated against its `input:` schema). |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `_apply_id` | `string` (ULID) | Launch ID. |
| `incarnation` | `string` | The name of the created instance. |

#### `keeper.incarnation.rerun-last`

Restarting the **last crashed** script from `error_locked`: REST mirror [`POST /v1/incarnations/{name}/rerun-last`](../operator-api/incarnations.md). Permission: `incarnation.rerun-last`. Async: **yes**.

Under one `FOR UPDATE` removes the block (`state` DOES NOT touch - last known-good, snapshot in `state_history`) and with the same action restarts **last fallen script** incarnations - bootstrap (`create`/..., if creation failed) OR day-2 operation (`add_user`/...) — with the saved input of the failed run (`error_locked → applying` bypassing `ready`). Input is restored from `incarnation.spec.input` (create-path) or from the failed run recipe (`apply_runs.recipe.input`, day-2-path), not from defaults. Difference from `keeper.incarnation.unlock`: it only removes the block, rerun removes and restarts the fallen script in one action. Works only from `error_locked`; status is not `error_locked` → `incarnation-locked`, input of the failed run is not available (the run fell to dispatch and the recipe was not written / the recipe was cleared by retention / legacy run, fail-closed) → separate code `rerun-input-unavailable` ([mcp-tools.md → Errors](../mcp-tools.md#errors)). Status poll - `keeper.incarnation.get`. Audit event - `incarnation.rerun_last` (NOT `incarnation.unlocked`).

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Name instance. |
| `reason` | `string` (1..500 characters) | yes | Free text for audit-trail (payload `incarnation.rerun_last`). |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID of the restarted run. |
| `incarnation` | `string` | Name instance. |
| `scenario` | `string` | The name of the restarted (last crashed) script is bootstrap `create`/… or day-2 `add_user`/…. |

Errors: `not-found` (incarnation does not exist), `incarnation-locked` (status not `error_locked`), `rerun-input-unavailable` (input of the failed day-2 run is not available - the run fell to dispatch and the recipe was not written / the recipe was cleared by retention / legacy run without a recipe), `validation-failed` (empty `reason` / `reason` longer than 500 characters / broken `name` / incarnation service not registered), `internal-error` (runner not configured / transaction / run).

#### `keeper.incarnation.run`

Run a custom script on an existing instance. Permission: `incarnation.run`. Endpoint: [`POST /v1/incarnations/{name}/scenarios/{scenario}`](../operator-api/incarnations.md). Async: **yes**.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Name instance. |
| `scenario` | `string` | yes | Script name from `scenario/<name>/`. |
| `input` | `object` | optional | Input script. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `_apply_id` | `string` (ULID) | Launch ID. |
| `incarnation` | `string` | Name instance. |
| `scenario` | `string` | Scenario name. |

#### `keeper.incarnation.get`

Read spec + state + status. Permission: `incarnation.get`. Endpoint: [`GET /v1/incarnations/{name}`](../operator-api/incarnations.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Name instance. |

**Output:** schema `IncarnationGetReply` — fields `name`, `service`, `service_version`, `state_schema_version`, `spec` (object), `state` (object), `status` (enum), `status_details` (object\|null), `created_by_aid`, `created_at`, `updated_at`. Details - [operator-api.md → IncarnationGetReply](../operator-api/incarnations.md).

#### `keeper.incarnation.list`

Enumeration of instances. Permission: `incarnation.list`. Endpoint: [`GET /v1/incarnations`](../operator-api/incarnations.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `service` | `string` | optional | Filter by service. |
| `status` | `string` (enum) | optional | Filter by status: `provisioning` / `ready` / `applying` / `error_locked` / `migration_failed` / `drift` / `destroying`. |
| `offset` | `integer` (≥0) | optional | Default `0`. |
| `limit` | `integer` (1..1000) | optional | Default `50`. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `items` | `array<IncarnationGetReply>` | The elements are the same form as in `keeper.incarnation.get`. |
| `offset`, `limit`, `total` | `integer` | Pagination. |

#### `keeper.incarnation.history`

Log `state_history`. Permission: `incarnation.history`. Endpoint: [`GET /v1/incarnations/{name}/history`](../operator-api/incarnations.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Name instance. |
| `offset` | `integer` | optional | Default `0`. |
| `limit` | `integer` | optional | Default `50`. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `items` | `array<StateHistoryEntry>` | Items - `{history_id, scenario, state_before, state_after, changed_by_aid, apply_id, at}`. |
| `offset`, `limit`, `total` | `integer` | Pagination. |

#### `keeper.incarnation.unlock`

Removing `error_locked`. Permission: `incarnation.unlock`. Endpoint: [`POST /v1/incarnations/{name}/unlock`](../operator-api/incarnations.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Name instance. |
| `reason` | `string` (1..500 characters) | yes | Free text for audit. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `name` | `string` | Name instance. |
| `previous_status` | `string` (enum) | Typically `error_locked`. |
| `status` | `string` (enum) | Typically `ready`. |
| `unlocked_by_aid` | `string` | AID that performed unlock. |
| `unlocked_at` | `string` (RFC 3339) | Time. |

#### `keeper.incarnation.upgrade`

Transfer to new `state_schema_version` + change `service_version`. Permission: `incarnation.upgrade`. Endpoint: [`POST /v1/incarnations/{name}/upgrade`](../operator-api/incarnations.md). Async: **yes**.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Name instance. |
| `to_version` | `string` (git-ref service) | yes | Target version of the service. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `_apply_id` | `string` (ULID) | Migration start ID. |

#### `keeper.incarnation.destroy`

Demolition instance. Permission: `incarnation.destroy`. Endpoint: [`DELETE /v1/incarnations/{name}`](../operator-api/incarnations.md). Async: **yes**. Operator-facing `allow_destroy` is mapped to internal `force` (force↔allow_destroy unification): `false` — destroy via teardown script `destroy`; `true` - demolition without teardown.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Name instance. |
| `allow_destroy` | `boolean` | yes | Mandatory confirmation flag (mapped in internal `force`). `false` - destroy via teardown script `destroy`; there is no script `destroy` in the service snapshot → `validation-failed`. `true` - demolition without teardown (force, DELETE lines directly). |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `_apply_id` | `string` (ULID) | Launch ID. |

#### `keeper.incarnation.traits-set`

Complete replacement of operator-set trait incarnation marks. Permission: `incarnation.traits-set`. Endpoint: [`PUT /v1/incarnations/{name}/traits`](../operator-api/incarnations.md). Async: **no** (sync - one UPDATE on the incarnation row, compact summary response).

Replaces `incarnation.traits` (jsonb - source of truth, [ADR-060](../../adr/0060-traits.md) R1 slice a) whole: empty/omitted `traits` = clear labels. One tx `FOR UPDATE`; nothing is written to a host row and no member host inherits the set ([NIM-281](../../adr/0008-coven-stable-tags.md#amendment-2026-08-05-nim-281-a-label-is-never-inherited)). RBAC - body-scoped OR-Check by coven/service-scope incarnation (a coven-scope matches the declared `covens` only; the incarnation's own name is the `incarnation=` dimension, not a coven - REST mirror). Per-host counterpart - [`keeper.soul.traits-assign`](souls.md) (first-class). Audit event - `incarnation.traits_changed` (trait-**KEYS** only, not values).

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Name instance. |
| `traits` | `object` | optional | Full set of trait marks: key → `scalar` (`string`/`number`/`boolean`) OR `list of scalars`. Replace semantics; empty/omitted = clear. Nested object/array → `validation-failed`. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `incarnation` | `string` | Name instance. |
| `keys` | `array<string>` | Sorted trait-**KEYS** after replacement (values ​​are NOT echoed - secret hygiene). |

**Errors:** `validation-failed` (broken `name` / invalid key / embedded trait value), `not-found` (incarnation does not exist), `forbidden` (there is no `incarnation.traits-set` in the scope of incarnation), `internal-error`.

#### `keeper.incarnation.bind-member`

Binds already-onboarded, **connected** Souls to the incarnation's roster. Permission: `incarnation.bind-member`. Endpoint: [`POST /v1/incarnations/{name}/members`](../operator-api/incarnations.md). Async: **no**.

The operator half of membership ([ADR-008 amendment 2026-07-28](../../adr/0008-coven-stable-tags.md), NIM-209): the relation is otherwise written only by `core.soul.registered` inside a scenario run, which makes a create scenario over a ready roster unreachable. Idempotent — a re-bind writes nothing and reports the SIDs under `already_member`.

**RBAC — two gates, both required.** (a) body-scoped OR-Check over the incarnation's coven/service scope (the REST middleware mirror); (b) **every** target SID inside the caller's soul visibility (`soul.list` purview), all-or-nothing. Gate (b) is shared with REST through the same domain screening — an MCP tool that skipped it would be a bypass of a REST-only check. Details — [rbac.md → § Incarnation membership](../rbac.md).

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Name instance. |
| `sids` | `array<string>` | yes | SIDs (FQDN) to bind, 1..200. Each must exist, be `connected` and lie inside the caller's soul scope. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `incarnation` | `string` | Name instance. |
| `bound` | `array<string>` | SIDs written by THIS call (sorted). |
| `already_member` | `array<string>` | SIDs that were members already — the idempotent part (sorted). |

**Errors:** `validation-failed` (broken `name`/SID, empty or oversized `sids`, unknown SID, host not `connected`), `not-found` (incarnation does not exist), `forbidden` (no `incarnation.bind-member` in the incarnation's scope, **or** a SID outside the caller's soul scope), `internal-error`.

#### `keeper.incarnation.unbind-member`

Removes a host from the roster — it stops being a target of every FUTURE run. Permission: `incarnation.unbind-member`. Endpoint: [`DELETE /v1/incarnations/{name}/members/{sid}`](../operator-api/incarnations.md). Async: **no**.

Idempotent: unbinding a non-member succeeds with `removed: false`; a SID absent from the registry is likewise a no-op (the FK cascade already removed its memberships). Same two gates as `bind-member`.

**Input:** `name` (`string`, yes) — name instance; `sid` (`string`, yes) — SID (FQDN) of the host to unbind.

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `incarnation` | `string` | Name instance. |
| `sid` | `string` | The host. |
| `removed` | `boolean` | `false` when the SID was not a member — the call is idempotent. |

**Errors:** `validation-failed` (broken `name`/`sid`), `not-found`, `forbidden`, `internal-error`.

#### `keeper.incarnation.members`

Lists the incarnation's roster. Permission: `incarnation.get` (the roster needs no right of its own). Endpoint: [`GET /v1/incarnations/{name}/members`](../operator-api/incarnations.md). Async: **no**.

**Input:** `name` (`string`, yes).

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `incarnation` | `string` | Name instance. |
| `items` | `array<object>` | `{sid, status, bound_at, bound_by_aid?}` — `status` is the HOST's lifecycle status at read time; `bound_by_aid` is absent for a keeper-internal bind. |
| `total` | `integer` | Number of members visible to THIS operator, not the size of the whole roster (narrowed to the caller's soul scope). |

**Errors:** `validation-failed` (broken `name`), `not-found`, `forbidden`, `internal-error`.
