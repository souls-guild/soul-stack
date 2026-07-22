# Voyage - MCP tools for unified batch runs

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.voyage.*` (batch N incarnations by scenario / N hosts by command, [ADR-043](../../adr/0043-voyage.md)). Transport, auth, tool declaration format, async convention, error mapping - in the root [mcp-tools.md](../mcp-tools.md). Source of truth for semantics - [operator-api.md → Voyage](../operator-api/voyages.md).

### Voyage (4)

Four tools 1:1 with REST `POST /v1/voyages` + `GET /v1/voyages{,/{id}}` + `DELETE /v1/voyages/{id}` ([operator-api/voyages.md](../operator-api/voyages.md)). `POST /v1/voyages/preview` - **REST-only** (no MCP tool). Permission - **RBAC-by-kind** on create/cancel (`scenario`→`incarnation.run`, `command`→`errand.run`, security-critical fail-closed guard inside the handler), `incarnation.history` on read. Tools are available only when the VoyageWorker stack is up; otherwise `internal-error` ("voyage orchestrator is not configured").

#### `keeper.voyage.start`

Creates a Voyage - a unified batch run. `kind=scenario`: apply named scenario to a set of INCARNATIONS (target `incarnations[]` ∪ `service`/`coven`; per-incarnation state commit). `kind=command`: execute a whitelisted module on a set of HOSTS (target `sids`/`coven`/`where`, AND-merge; state is not affected). Permission: **RBAC-by-kind**. Endpoint: [`POST /v1/voyages`](../operator-api/voyages.md). Async: **yes** (202 + `voyage_id`; progress - polling `keeper.voyage.get`).

**Input** (`required: kind, target`):

| Field | Type | Required | Meaning |
|---|---|---|---|
| `kind` | `string` (enum `scenario`/`command`) | yes | Run type. |
| `scenario_name` | `string` | for scenario | Scenario name (required for `kind=scenario`). |
| `module` | `string` | for command | Soul-side whitelisted module address (required for `kind=command`). |
| `input` | `object` | optional | Run parameters (**NOT logged** in audit). |
| `target` | `object` | yes | Target: `incarnations[]` / `service` / `sids[]` / `where` (CEL, ≤4 KiB) / `coven[]`. scenario uses `incarnations`/`service`/`coven` (any-of env tag); command - `sids`/`coven` (AND) / `where`. |
| `batch` | `string` | optional | Batch size `N` units or `N%` (1..100) from scope. Mutually exclusive with `batch_size` (mixing → `voyage_batch_spec_conflict`). |
| `max_failures` | `string` | optional | The failure threshold is `N` absolute or `N%` from run units. Mutually exclusive with `fail_threshold`. |
| `batch_size` | `integer` (≥1) | optional | **DEPRECATED** (use `batch`). Leg size; `null` → entire run in one Leg. |
| `concurrency` | `integer` (1..500) | optional | `0`/missing → default `50`. |
| `dry_run` | `boolean` | optional | Run dry-run. |
| `schedule_at` | `string` (date-time) | optional | Delayed start → `status=scheduled`. |
| `inter_batch_interval_ms` | `integer` (≥0) | optional | Pause between Legs (ms). |
| `on_failure` | `string` (enum `abort`/`continue`) | optional | Behavior upon failure of Leg. |

**Output** (`required: voyage_id, kind, scope_size, status, location`):

| Field | Type | Meaning |
|---|---|---|
| `voyage_id` | `string` (ULID) | ID Voyage. |
| `kind` | `string` (enum) | Echo input. |
| `scope_size` | `integer` | Number of resolved units. |
| `status` | `string` (enum `pending`/`scheduled`) | `scheduled` when `schedule_at` is specified, otherwise `pending`. |
| `location` | `string` | REST path for get/poll (`/v1/voyages/<id>`). |

Errors (like REST): `forbidden` (RBAC-by-kind deny / explicit foreign host in command / incarnation outside scope in scenario), `not-found` (explicit incarnation does not exist, scenario), `validation-failed` (invalid `kind` / empty `scenario_name`/`module` by kind / no target / broken SID/coven/name / `where` > 4 KiB / empty resolve `voyage_empty_target` / scope > `voyage.max_scope` / batch-spec conflict), `tempo-exceeded` (rate-limit bucket `voyage_create`). See hybrid semantics command∩Purview - [operator-api/voyages.md](../operator-api/voyages.md).

#### `keeper.voyage.get`

Reads Voyage snapshot by ULID (detail + summary). Permission: `incarnation.history`. Endpoint: [`GET /v1/voyages/{id}`](../operator-api/voyages.md). Async: no.

**Input:** `{voyage_id}` (ULID, required).

**Output `VoyageView`** (`required: voyage_id, kind, status, scope_size, total_batches, current_batch_index, started_by_aid, created_at`):

| Field | Type | Meaning |
|---|---|---|
| `voyage_id` | `string` (ULID) | PK. |
| `kind` | `string` | `scenario` / `command`. |
| `status` | `string` | `scheduled`/`pending`/`running`/`succeeded`/`failed`/`partial_failed`/`cancelled`. |
| `scope_size` | `integer` | Scope size. |
| `total_batches` | `integer` | Number of Legs. |
| `current_batch_index` | `integer` | Current Leg. |
| `started_by_aid` | `string` | FK on `operators(aid)`. |
| `created_at` | `string` (date-time) | When inserted. |

Errors: `not-found` (`voyage_id` does not exist).

#### `keeper.voyage.list`

Enumeration of Voyage runs with filters `kind`/`status` (multi-value) and pagination (sort `created_at` DESC). Permission: `incarnation.history`. Endpoint: [`GET /v1/voyages`](../operator-api/voyages.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `kind` | `string` (enum) | optional | Filter by type. |
| `status` | `array<string>` (enum) | optional | Multi-value filter by status. |
| `offset` | `integer` (≥0) | optional | Pagination. |
| `limit` | `integer` (1..1000) | optional | Pagination. |

**Output:** `{items: array<object>, offset, limit, total}` (elements - form `VoyageView`).

#### `keeper.voyage.cancel`

Cancels Voyage (`pending`/`scheduled` → `cancelled`). Running-abortion - post-MVP. Permission: **RBAC-by-kind** (as `keeper.voyage.start`). Endpoint: [`DELETE /v1/voyages/{id}`](../operator-api/voyages.md). Async: no.

**Input:** `{voyage_id}` (ULID, required).

**Output:** `{voyage_id, status}`, where `status` = `cancelled`.

Errors: `not-found` (`voyage_id` does not exist), `errand-not-cancellable` (Voyage is already `running`/in terminal status - there is nothing to cancel).
