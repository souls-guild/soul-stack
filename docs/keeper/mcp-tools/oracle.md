# Oracle - MCP-tools Vigil / Decree registries

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.oracle.vigil.*` / `keeper.oracle.decree.*` (event-driven monitoring registries Beacons, [ADR-030](../../adr/0030-vigil-oracle.md)). Transport, auth, tool declaration format, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api/oracle.md](../operator-api/oracle.md).

### Oracle (6)

4-segment tool-name `keeper.oracle.<resource>.<action>` ↔ 2-segment permission `<resource>.<action>` (`vigil.create` / `decree.list` / …, selector - NoSelector). Business logic (validation `name`/`interval`/`check`/subject for Vigil; `name`/`on_beacon`/`incarnation`/`scenario`/subject/`where`-CEL for Decree) lives in `oracle.Service`; tool - transport. Tools are only available when the registry is connected; when disabled, the call returns `internal-error` ("oracle registry is not configured"). **Reactor flow (Portent → match Decree → enqueue) is NOT controlled by these tools** ([rbac.md §Oracle](../rbac.md)).

#### `keeper.oracle.vigil.create`

Creates a Vigil at `vigils` (Soul-side check beacons: `check` - core-beacon address + `interval` + subject `coven` XOR `sid`). Read-only by design. Permission: `vigil.create`. Endpoint: [`POST /v1/vigils`](../operator-api/oracle.md). Async: no.

**Input** (`required: name, interval, check`): `{name (kebab 1..63), interval (duration), check (core-beacon address), coven? (array, XOR sid), sid? (XOR coven), params? (object), enabled? (default true)}`.

**Output:** `VigilView` — `{name, coven?, sid?, interval, check, params, enabled, created_by_aid?, created_at, updated_at}`.

Errors: `vigil-already-exists` (`name` busy), `validation-failed` (broken `name`/`interval`/`check`/subject).

#### `keeper.oracle.vigil.list`

Enumeration of Vigils (sort `created_at` DESC, `name` ASC). Permission: `vigil.list`. Endpoint: [`GET /v1/vigils`](../operator-api/oracle.md). Async: no.

**Input:** `{offset?, limit?}`. **Output:** `VigilListReply` — `{items: array<VigilView>, offset, limit, total}`.

#### `keeper.oracle.vigil.delete`

Deletes Vigil by name (stops distributing to hosts in `VigilSnapshot`; Decrees do NOT cascade). Permission: `vigil.delete`. Endpoint: [`DELETE /v1/vigils/{name}`](../operator-api/oracle.md). Async: no.

**Input:** `{name}`. **Output:** empty object (REST equivalent - 204). Errors: `not-found`.

#### `keeper.oracle.decree.create`

Creates Decree (reactor rule): `on_beacon` (Vigil) × subject (`coven` XOR `sid`) × `incarnation_name` → `action_scenario` (named, whitelist) + opt. `where`-CEL predicate over `event.data` + `cooldown`. Default-deny. Permission: `decree.create`. Endpoint: [`POST /v1/decrees`](../operator-api/oracle.md). Async: no.

**Input** (`required: name, on_beacon, incarnation_name, action_scenario`): `{name (kebab 1..63), on_beacon (kebab), incarnation_name, action_scenario (named, ^[a-z][a-z0-9_]*$), coven? (XOR sid), sid? (XOR coven), where? (CEL), action_input? (object), cooldown? (duration), enabled? (default true)}`.

**Output:** `DecreeView` — `{name, on_beacon, where?, coven?, sid?, incarnation_name, action_scenario, action_input, cooldown, enabled, created_by_aid?, created_at, updated_at}`.

Errors: `decree-already-exists` (`name` busy), `validation-failed` (broken `name`/`on_beacon`/`incarnation_name`/`action_scenario`/subject/`where`-CEL/`cooldown`).

#### `keeper.oracle.decree.list`

Enumeration of Decrees (sort `created_at` DESC, `name` ASC). Permission: `decree.list`. Endpoint: [`GET /v1/decrees`](../operator-api/oracle.md). Async: no.

**Input:** `{offset?, limit?}`. **Output:** `DecreeListReply` — `{items: array<DecreeView>, offset, limit, total}`.

#### `keeper.oracle.decree.delete`

Removes Decree by name; cascade clears cooldown-state (`oracle_fires`, `ON DELETE CASCADE`). Permission: `decree.delete`. Endpoint: [`DELETE /v1/decrees/{name}`](../operator-api/oracle.md). Async: no.

**Input:** `{name}`. **Output:** empty object (REST equivalent - 204). Errors: `not-found`.
