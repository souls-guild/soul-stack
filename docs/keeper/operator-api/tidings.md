# Tiding - endpoints of the registry of notification subscription rules

Domain section [Operator API](../operator-api.md): endpoints `/v1/tidings*` - CRUD registry `tidings` (rules for subscribing to run events, [ADR-052](../../adr/0052-herald-notifications.md), S4). Tiding answers the question "what to respond to → which Herald to deliver"; paired delivery channel - [heralds.md](heralds.md). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/tidings.md](../mcp-tools/tidings.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 5 routes) - in the root [operator-api.md → Tiding (5)](../operator-api.md). The full request/response scheme is [`openapi.yaml`](../openapi.yaml) (`TidingCreateRequest` / `TidingUpdateRequest` / `Tiding` / `TidingListReply` - **source of truth by form**). `tiding.*` - NoSelector. Routes are mounted only when the registry is configured (the same nil-guard as Herald).

### Match model

Dispatcher for each successfully recorded audit event of the run matches the enabled Tiding rules by:

- **`event_types`** - non-empty list of audit-event-types with **area-glob** (`scenario_run.*`) in the scope of runs: `scenario_run.*` / `command_run.*` / `voyage.*` / `cadence.*` + dot `incarnation.drift_checked` and `incarnation.run_completed`. Arbitrary wildcard (`*`, `foo.*.bar`) is prohibited → `422`.
- **filters** `only_failures` / `only_changes` (bool);
- **opt. selectors** `incarnation` / `cadence` / `task` (nullable) - binding to the source of the run. See the separate section "`task` Selector" below.

Each match is assigned a delivery task via `herald` (FK on `heralds.name`).

### Selector `task` - subscription to change a specific task

`task` (string|null, [ADR-052 §l](../../adr/0052-herald-notifications.md)) - opt. subscription selector for a **specific run task** at its address. `null` - without filter.

- **Address** is a value from the task's `register ∪ id` address space (same as the `register`/`id` task's grammar fields in DSL). This is a **rule selector** (which Tiding subscribes to), not the task field itself.
- **Match** - non-empty `task` narrows the triggering **only** to the event `incarnation.run_completed` (per-incarnation result of the run, carries `changed_tasks`). The rule matches if the `changed_tasks` event has an entry with `register == task` OR `id == task`. Any other `event_type` (without `changed_tasks`) does not match the specified `task`.
- **The semantics have "changed"** - the presence of the address in `changed_tasks` already means that the task has changed on at least one host ([ADR-052 §j](../../adr/0052-herald-notifications.md)); a separate `only_changes` is not needed for this. The combination `task` + `only_changes` remains consistent (the match event is not eliminated).

For the `task` rule to receive events at all, there must be `incarnation.run_completed` in `event_types`. Event `incarnation.run_completed` is described in detail in [naming-rules.md → Audit-events](../../naming-rules.md) (per-incarnation result, `status` ∈ `success`/`failed`, carries `changed_tasks` + opt. `cadence_id`).

The `cadence` selector also catches `incarnation.run_completed` when a run is spawned by a Cadence schedule (the event carries `cadence_id`) - this is how constant Tiding with the `cadence` selector subscribes to the results of schedule runs, and not just to the `cadence.*` events themselves.

### Delivery body management (`annotations` / `projection`)

Operator-specified fields that form the body of the webhook delivery ([ADR-052(h)](../../adr/0052-herald-notifications.md)). Available in `Create`/`Update` and present in the answer `Tiding`:

- **`annotations`** (object, optional) - static operator fields (top-level JSON object), merged into the body with the `annotations` key. Non-object at top level (array/scalar) → `422`.
- **`projection`** (array<string>, opt.) — allow-list of payload paths: which event fields will go into the body; empty/omitted - full form. Each path is segments `[a-z0-9_]` through `.` (`summary.succeeded`); empty segment (leading/double/tail point) → `422`.

### Server fields of one-time rules (`ephemeral` / `voyage_id`)

Single-run subscription fields ([ADR-052(g)](../../adr/0052-herald-notifications.md)). **Server, read-only** - present only in the response `Tiding`, not in `Create`/`Update`:

- **`ephemeral`** (bool) - one-time rule. The ephemeral-Tiding operator **does not create** directly: they are materialized by the keeper from the notify block [`POST /v1/voyages`](voyages.md) (`VoyageNotify`) in the same transaction that creates Voyage. Invariant `ephemeral=true ⟺ voyage_id != null`.
- **`voyage_id`** (string|null) — Voyage ID of the one-time rule binding; for a permanent one - `null`.

A permanent rule (created via `POST /v1/tidings`) is always `ephemeral=false`, `voyage_id=null`. Cleaning up orphaned ephemeral rules - background Reaper rule [`purge_orphan_ephemeral_tidings`](../reaper.md) after the run terminal.

### Permanent rule from the schedule form (origin marker Cadence)

Permanent Tiding can be created not only directly through `POST /v1/tidings`, but also **from the `notify[]` block of the schedule form** ([`POST /v1/cadences`](cadences.md), [ADR-052 §m](../../adr/0052-herald-notifications.md)). Such rules carry an internal origin marker `created_from_cadence_id` (= ULID of the generating schedule) - it **is not given in the API response** `Tiding` (server field, not a contract), but defines a cascade: `DELETE /v1/cadences/{id}` atomically demolishes the rules generated by the form (FK `ON DELETE CASCADE`, [ADR-046 §9](../../adr/0046-cadence.md)). The origin marker is orthogonal to the `cadence` filter selector: a manually created rule with the same `cadence` selector (but without the origin marker) is cascaded **not deleted** and lives independently.

### `POST /v1/tidings` - create Tiding

Permission: `tiding.create`. MCP-tool: `keeper.tiding.create`.

**Request `TidingCreateRequest`** (`required: name, herald, event_types`): `{name (^[a-z0-9-]{1,63}$), herald (Herald channel name, FK), event_types (array<string>, non-empty, area-glob), only_failures? (bool), only_changes? (bool), incarnation? (string|null), cadence? (string|null), task? (string|null - register∪id address, see "Task selector"), annotations? (object), projection? (array<string>), enabled? (bool, omitted -> true)}`. There are no `ephemeral`/`voyage_id` fields in the request - server ones (see above).

**Response `201 Tiding`:** `{name, herald, event_types, only_failures, only_changes, incarnation, cadence, task, annotations, projection, ephemeral, voyage_id, enabled, created_at, updated_at, created_by_aid}`.

Errors: `400` (broken JSON / unknown field), `404 not-found` (`herald` does not exist according to FK), `409` (`name` busy), `422 validation-failed` (broken `name`/`event_types`, arbitrary wildcard, `annotations` non-object, broken path `projection`). Audit: `tiding.created`.

### `GET /v1/tidings` - list of Tiding rules

Permission: `tiding.list`. MCP-tool: `keeper.tiding.list`. Query `offset`/`limit`. Sort `updated_at` DESC, `name` ASC. Response `200 TidingListReply` (`{items, offset, limit, total}`).

### `GET /v1/tidings/{name}` - read one rule

Permission: `tiding.read`. MCP-tool: `keeper.tiding.read`. Response `200 Tiding`; `404 not-found` - no entry.

### `PUT /v1/tidings/{name}` — replace the rule (replace semantics)

Permission: `tiding.update`. MCP-tool: `keeper.tiding.update`. **Replace** — the body completely replaces the mutable fields; `name` (PK) immutable. Like Push-Provider/Herald - `PUT` (complete replacement), not `PATCH`.

**Request `TidingUpdateRequest`** (`required: herald, event_types`): `{herald, event_types (array<string>), only_failures?, only_changes?, incarnation? (|null), cadence? (|null), task? (|null), annotations? (object), projection? (array<string>), enabled?}`. Replace semantics: omitted `incarnation`/`cadence`/`task`/`annotations`/`projection` are cleared (omit==clear - FE sends the entire rule, not the delta). There are no `ephemeral`/`voyage_id` fields in the request - server ones.

**Response `200 Tiding`.** Errors: `400`, `404 not-found` (no rule or `herald` by FK does not exist), `422 validation-failed`. Audit: `tiding.updated`.

### `DELETE /v1/tidings/{name}` - delete rule

Permission: `tiding.delete`. MCP-tool: `keeper.tiding.delete`. Response `204`; `404 not-found`. Audit: `tiding.deleted`. (Demolition of the Herald channel cascades away its Tiding subscriptions - Tiding does not have an inverse cascade dependence.)

## See also

- [heralds.md](heralds.md) - paired registry of delivery channels (where to send; webhook, SSRF-guard, signature).
- [mcp-tools/tidings.md](../mcp-tools/tidings.md) - MCP side (`keeper.tiding.*`).
- [ADR-052](../../adr/0052-herald-notifications.md) - Herald/Tiding design (tap over audit-writer, Tiding rules match, scope = run events only).
- [rbac.md](../rbac.md) — permissions directory `tiding.*`.
