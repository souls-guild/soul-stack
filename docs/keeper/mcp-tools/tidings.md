# Tiding - MCP-tools for registering notification subscription rules

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.tiding.*` (registry CRUD `tidings`, [ADR-052](../../adr/0052-herald-notifications.md), S4). Transport, auth, tool declaration format, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics, bodies, error codes is [operator-api/tidings.md](../operator-api/tidings.md).

### Tiding (5)

5 tools 1:1 `keeper.tiding.<verb>` ↔ permission `tiding.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/tidings*` (selector - NoSelector). `event_types` — area-glob (`scenario_run.*`) in the scope of runs; `herald` - FK to existing Herald. Same `HeraldSvc` / nil-guard as herald-tools; when the registry is turned off - `internal-error`.

#### `keeper.tiding.create`

Creates a PERMANENT Tiding subscription rule: for which `event_types` (area-glob `scenario_run.*` in the scope of runs - `scenario_run` / `command_run` / `voyage` / `cadence` + point `incarnation.drift_checked` and `incarnation.run_completed`) react → which Herald to deliver. Filters `only_failures` / `only_changes`, opt. selectors `incarnation` / `cadence` / `task`, delivery body control `annotations` / `projection` ([ADR-052(h)](../../adr/0052-herald-notifications.md)). Permission: `tiding.create`. Endpoint: [`POST /v1/tidings`](../operator-api/tidings.md). Async: no.

`task` (string|null, [ADR-052 §l](../../adr/0052-herald-notifications.md)) - opt. Subscription selector for a specific task at `register ∪ id`. A non-empty `task` narrows the match only to `incarnation.run_completed`, whose `changed_tasks` has an entry with `register == task` OR `id == task` (see [operator-api/tidings.md → "Task selector"](../operator-api/tidings.md)). To trigger the `task` rule in `event_types`, you need `incarnation.run_completed`.

**Input** (`required: name, herald, event_types`): `{name (^[a-z0-9-]{1,63}$), herald (FK to Herald), event_types (array<string>, area-glob), only_failures?, only_changes?, incarnation? (|null), cadence? (|null), task? (|null), annotations? (object), projection? (array<string>), enabled? (omitted -> true)}`. Fields `ephemeral`/`voyage_id` - server fields ([ADR-052(g)](../../adr/0052-herald-notifications.md)): are not accepted for input, the one-time rule materializes the keeper from the Voyage notify block.

**Output:** `Tiding` - `{name, herald, event_types, only_failures, only_changes, incarnation, cadence, task, annotations, projection, ephemeral, voyage_id, enabled, created_at, updated_at, created_by_aid}` (`ephemeral`/`voyage_id` - read-only; permanent rule has `ephemeral=false`, `voyage_id=null`). Errors: `tiding-already-exists` (`name` busy), `not-found` (`herald` does not exist), `validation-failed` (broken `name`/`event_types`, arbitrary wildcard, `annotations` non-object, broken path `projection`).

#### `keeper.tiding.update`

Replaces the mutable fields of the Tiding rule (replace semantics; `name` is the key). Permission: `tiding.update`. Endpoint: [`PUT /v1/tidings/{name}`](../operator-api/tidings.md). Async: no.

**Input** (`required: name, herald, event_types`): `{name, herald, event_types, only_failures?, only_changes?, incarnation? (|null), cadence? (|null), task? (|null), annotations? (object), projection? (array<string>), enabled?}` (replace: omitted `incarnation`/`cadence`/`task`/`annotations`/`projection` are cleared - omit==clear; `ephemeral`/`voyage_id` are not accepted for input - server ones). **Output:** `Tiding`. Errors: `not-found` (no rule or `herald` by FK does not exist).

#### `keeper.tiding.delete`

Removes a Tiding rule by name. Permission: `tiding.delete`. Endpoint: [`DELETE /v1/tidings/{name}`](../operator-api/tidings.md). Async: no.

**Input:** `{name}`. **Output:** empty object (REST equivalent - 204). Errors: `not-found`.

#### `keeper.tiding.list`

Enumeration of Tiding rules (sort `updated_at` DESC, `name` ASC). Permission: `tiding.list`. Endpoint: [`GET /v1/tidings`](../operator-api/tidings.md). Async: no.

**Input:** `{offset?, limit?}`. **Output:** `{items: array<Tiding>, offset, limit, total}`.

#### `keeper.tiding.read`

Reads one Tiding rule by name. Permission: `tiding.read`. Endpoint: [`GET /v1/tidings/{name}`](../operator-api/tidings.md). Async: no.

**Input:** `{name}`. **Output:** `Tiding`. Errors: `not-found`.
