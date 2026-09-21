# Cadence — endpoints of regular launches (scheduled/recurring Voyage)

Domain section [Operator API](../operator-api.md): endpoints `/v1/cadences*` - schedules that spawn the usual [Voyage](voyages.md)-run ([ADR-046](../../adr/0046-cadence.md), registry `cadences`). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). Schedule executor behavior (due-fetch, `overlap_policy`, recalculation `next_run_at`, adaptive poll) - [conductor.md](../conductor.md) (source-of-truth behavior). **There is no MCP side** - Cadence does not have MCP tools installed ([mcp-tools/cadences.md](../mcp-tools/cadences.md)).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 8 routes) - in the root [operator-api.md → Cadence (8)](../operator-api.md). The full request/response scheme is [`openapi.yaml`](../openapi.yaml) (`CadenceCreateRequest` / `CadencePatchRequest` / `Cadence` / `CadenceCreateReply` / `CadenceEnabledReply` / `CadenceListReply` - **source of truth in form**). Below is the normative semantics of behavior on which the contract is based.

Cadence is a line in `cadences` with a "recipe" for the run (the same set of fields as [`VoyageCreateRequest`](voyages.md): `kind`/`scenario_name`|`module`/`target`/`input`/batch settings) + repetition rule (`schedule_kind` `interval`|`cron`) + `overlap_policy`. Executes the trigger [Conductor](../conductor.md) - leader-elected subsystem inside `keeper`; The spawned Voyage is included in the regular Voyage-lifecycle.

### Two-level RBAC (security-critical fail-closed)

The `cadence.*` right controls the schedule itself, but the recipe spawns Voyage on behalf of the creator ([ADR-046 §7](../../adr/0046-cadence.md)). Therefore, on **creation** (and on editing a target/recipe), the **second level** is in effect - Voyage-permission according to `kind` recipe (`scenario`→`incarnation.run`, `command`→`errand.run`, [ADR-043 §6](../../adr/0043-voyage.md)): otherwise Cadence would become privilege-escalation-bypass RBAC. `kind` is visible only from the body → the second gate lives inside `CadenceHandler.Create`/`.Patch` (the first, `cadence.create`/`cadence.update`, gates middleware-route).

For `kind=scenario`, in addition to bare-check, **per-target coven-scope-check** (parity `VoyageHandler.createScenario`) works: the resolved target (its `covens ∪ {name}`) must be in the creator's RBAC scope on each incarnation - otherwise the scoped Archon "run on `coven=A`" would create Cadence on `coven=B` (outside scope) and background spawn would execute outside scope. `kind=command` - bare-check `errand.run` (per-host selectors are deferred post-MVP, parity Voyage).

PATCH carries the same two-level guard: it changes `target`/`scenario_name`, so without the scoped guard, the Archon would create a Cadence on the allowed `coven=A` and PATCH redirect the target to `coven=B`. `kind` in PATCH **does not change** (change `kind` = delete + create).

### `POST /v1/cadences` - create Cadence

Permission: `cadence.create` (middleware) **+** Voyage-permission by `kind` (handler, see above). MCP-tool: no. Async: no (schedule creation is synchronous; run spawning is delayed, conducted by Conductor).

**Request `CadenceCreateRequest`** (`required: name, schedule_kind, overlap_policy, kind, target`):

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | The human-readable name of the schedule. |
| `schedule_kind` | `string` (`interval`/`cron`) | yes | Type of repetition rule (`interval_seconds` XOR `cron_expr`). |
| `interval_seconds` | `integer` (≥30) | for interval | Period. Minimum **30s** (floor limit, ADR-046 Pass B); `<30` → `422`. For a reaction faster than 30s - Beacons (Vigil/Oracle, [ADR-030](../../adr/0030-vigil-oracle.md)). |
| `cron_expr` | `string` | for cron | Standard 5-field cron expression (UTC). |
| `overlap_policy` | `string` (`skip`/`queue`/`parallel`) | yes | Overlay behavior (the previous child is not yet terminal). |
| `kind` | `string` (`scenario`/`command`) | yes | Recipe-run type. |
| `scenario_name` | `string` | for scenario | Required for `kind=scenario`; prohibited for `command`. |
| `module` | `string` | for command | Required for `kind=command`; prohibited for `scenario`. |
| `target` | `VoyageTarget` | yes | Run target (resolves at **spawn**, not at creation). |
| `input` | `object` | no | Run parameters. **NOT logged** (invariant A [ADR-027](../../adr/0027-apply-work-queue.md)). |
| `batch` | `string` | no | Leg size: `N` units / `N%` (1..100) from spawn-scope. Mutually exclusive with `batch_size`/`batch_percent` → `422 voyage_batch_spec_conflict`. |
| `max_failures` | `string` | no | Failure threshold: `N` absolute / `N%` from spawn-scope run units. Mutually exclusive with `fail_threshold` → `422`. |
| `batch_size` / `batch_percent` | `integer` | no | **DEPRECATED** (use `batch`). |
| `fail_threshold` | `integer` | no | **DEPRECATED** (use `max_failures`). |
| `concurrency` | `integer` (≥1) | no | Parallelism inside Leg (barrier) / window width (window). |
| `batch_mode` | `string` (`barrier`/`window`) | no | Batch mode (`NULL` ⇒ `barrier`). |
| `inter_batch_interval_ms` / `inter_unit_interval_ms` | `integer` (≥0) | no | Pauses between Legs (barrier) / per-unit (window). |
| `require_alive` | `boolean` | no | Presence filter for living people on the scope resolution (kind=command). Default `false`. |
| `on_failure` | `string` (`abort`/`continue`) | no | Behavior upon failure of Leg. |
| `enabled` | `boolean` | no | Is the schedule included? Default **`true`** (default-ON; `false` → pause). |
| `notify` | `array<VoyageNotify>` | no | Subscriptions to notifications about runs of THIS schedule. The shape of the element is the same [`VoyageNotify`](voyages.md) as that of the one-time `voyage.notify`. See ["Notifications `notify[]`"](#notifications-notify---permanent-tiding-from-the-schedule-form) below. |

Percentages `batch`/`max_failures` (format `N%`) **do not resolve to absolute on create** - Cadence spawn-scope is unknown on creation; the percentage goes into the `batch_percent`/`fail_threshold_percent` columns and resolves to the spawn-scope when Voyage spawns.

### Notifications `notify[]` - permanent Tiding from the schedule form

`notify` ([ADR-052 §m](../../adr/0052-herald-notifications.md), opt.) - list of subscriptions to notifications about runs of **this** schedule. The shape of each element is the same [`VoyageNotify`](voyages.md) (`herald` + opt. `on`/`only_failures`/`only_changes`/`annotations`/`projection`) as the one-time `voyage.notify`; validation and RBAC are reused. The difference from the Voyage form is in the nature of the rule being created:

- **Permanent, not ephemeral.** Unlike `voyage.notify` (one-time rule `ephemeral=true` for one run), each element of `cadence.notify` is materialized by the keeper into a **permanent** Tiding (`ephemeral=false`), which listens to schedule runs and further - while the schedule lives.
- **Binding by schedule ULID.** The rule carries the `cadence` selector (filter "send only about runs of this schedule") + internal origin marker `created_from_cadence_id = cadences.id` (stable ULID-PK, rename-safe - schedule name is mutable via PATCH). The auto rule name is `<name>-notify[-N]` (deterministic suffix).
- **Atomicity.** Insert of rules goes in **the same PG transaction** that creates Cadence: either Cadence + all rules, or nothing (FK/name collision/validation rolls back the entire `POST`).
- **Cascade when deleting.** `DELETE /v1/cadences/{id}` cascades the rules generated by the form ([ADR-046 §9](../../adr/0046-cadence.md), `tidings.created_from_cadence_id ON DELETE CASCADE`). Manually created Tidings with the same `cadence` selector (but `created_from_cadence_id = NULL`) **do not touch** - the origin marker is orthogonal to the filter selector.
- **RBAC `herald.read`.** The initiator must have permission `herald.read` for **each** specified delivery channel - otherwise `403` (you cannot "suspend" delivery through someone else's Herald). Non-existent channel → `422`.
- **Cap 64 channels.** The length of `notify[]` is limited to **64** channels per schedule (exceeding → `422` before transaction opens).

`notify=[]`/omitted ⇒ schedule without notifications (one transaction with a single Insert Cadence).

**Response `201 CadenceCreateReply`** (`required: cadence_id, name, enabled, location`) + header `Location: /v1/cadences/{id}`:

```json
{
  "cadence_id": "01HABCDEFGHJKMNPQRSTVWXYZ",
  "name": "nightly-converge",
  "enabled": true,
  "next_run_at": "2026-06-11T00:00:00Z",
  "location": "/v1/cadences/01HABCDEFGHJKMNPQRSTVWXYZ"
}
```

`next_run_at` is calculated during creation (a pure function from the schedule; for `enabled=false` it is also calculated - the series does not "stick", it starts when enabled). `created_by_aid = JWT.sub`. Audit: `cadence.created`.

**Errors:** `400` (invalid JSON); `401` `unauthenticated` / `operator-revoked-token` (AID revoked); `403 forbidden` (two-level RBAC deny: no Voyage-permission by `kind`, or target outside scope for scenario, **or no `herald.read` per channel from `notify[]`**); `404 not-found` (explicit incarnation of target does not exist, scenario); `422 validation-failed` (invalid recipe/schedule: XOR interval/cron, enum `overlap_policy`/`kind`/`batch_mode`/`on_failure`, `kind`↔`scenario_name`/`module`, broken cron, batch-spec conflict, empty resolution `cadence_empty_target`, **floor limit `interval_seconds < 30`**, **non-existent Herald channel in `notify[]` / `notify[]` > 64 channels**); `500` (store/enforcer not configured / DB failed).

### `GET /v1/cadences` — list of Cadence schedules

Permission: `cadence.list`. MCP-tool: no. Query: `enabled` (`true` → enabled only; `false`/omitted → all), `kind` (exact `scenario`/`command`) + `offset`/`limit` ([§ Pagination](../operator-api.md#pagination)). Sort `created_at` DESC. Response `200 CadenceListReply` (`{items, offset, limit, total}`). `input` of the recipe in the list-issue **not given** (invariant A ADR-027).

### `GET /v1/cadences/{id}` - Cadence part

Permission: `cadence.list`. MCP-tool: no. `id` - ULID. Response `200 Cadence` (recipe + schedule + `next_run_at`/`last_run_at` + audit metadata; `input` NOT given). `404 cadence_not_found` (no entry); `422 validation-failed` (`id` is not a ULID).

### `PATCH /v1/cadences/{id}` - update Cadence

Permission: `cadence.update` (middleware) **+** two-level guard (handler, see above). MCP-tool: no. Read-modify-write: specified fields are overwritten, omitted fields are retained. Request `CadencePatchRequest` (all fields are optional; `kind` is missing - does not change). When changing the schedule (`schedule_kind`/`interval_seconds`/`cron_expr`), `next_run_at` is recalculated. Response `200 Cadence`. Audit: `cadence.updated`.

**Errors:** `400` (invalid JSON); `403` (two-level RBAC deny on post-patch target); `404 cadence_not_found`; `422 validation-failed` (invalid recipe/schedule, batch-spec conflict, **floor-limit** - including when transferring the schedule to `interval`); `500` (DB failure).

### `POST /v1/cadences/{id}/enable` | `POST /v1/cadences/{id}/disable` — toggle schedules

Permission: **`cadence.enable` OR `cadence.update`** (enable) / **`cadence.disable` OR `cadence.update`** (disable) - OR gate `RequireAnyPermission` (backcompat: roles with old `cadence.update` retain toggle, [ADR-046 amendment 2026-06-02](../../adr/0046-cadence.md)). MCP-tool: no. Lightweight toggle without rewriting the recipe. Response `200 CadenceEnabledReply` (`{cadence_id, enabled}`). Audit: `cadence.updated`. `404 cadence_not_found`; `422` (`id` is not a ULID).

### `GET /v1/cadences/{id}/runs` - Voyage child schedules

Permission: **`incarnation.history`** (read runtime states of runs, parity Voyage-list - not `cadence.*`). MCP-tool: no. Drill "schedule → its runs" (`voyages WHERE cadence_id=$1`, reuse [Voyage-DTO](voyages.md)). Query: `status` (multi-value `?status=X&status=Y`, OR semantics) + `offset`/`limit`. Response `200 VoyageListReply`. `404 cadence_not_found` (if Cadence does not exist, an empty list is indistinguishable from a non-existent id, so existence-probe). `422 validation-failed` (`id` is not a ULID / broken `status` filter).

> **Note (performer behavior).** With `DELETE /v1/cadences/{id}` (`cadence.delete`), the spawned Voyages **remain** (FK `voyages.cadence_id ON DELETE SET NULL` - children's history and manual runs are preserved), but the permanent Tidings from the block `notify[]` **are demolished in a cascade** (FK `tidings.created_from_cadence_id ON DELETE CASCADE`, [ADR-046 §9](../../adr/0046-cadence.md)); manually created rules with the same `cadence` selector (`created_from_cadence_id = NULL`) are not affected. Spawn semantics (due-fetch, three `overlap_policy`, anchored-recalculation `next_run_at`, missed-slot anti-storm, authorship of the child Voyage from `created_by_aid` Cadence with audit `source: background`) are carried out by [Conductor](../conductor.md), and not by these endpoints.
