# Oracle - Vigil / Decree registry endpoints

Domain section [Operator API](../operator-api.md): endpoints `/v1/vigils*` + `/v1/decrees*` - CRUD registries event-driven monitoring Beacons (Vigil = Soul-side check, Decree = rule reactor Portent → match → enqueue scenario, [ADR-030](../../adr/0030-vigil-oracle.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/oracle.md](../mcp-tools/oracle.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 8 routes) - in the root [operator-api.md → Oracle (8)](../operator-api.md). The full request/response scheme is [`openapi.yaml`](../openapi.yaml) (`VigilCreateRequest` / `VigilView` / `VigilListReply` / `DecreeCreateRequest` / `DecreeView` / `DecreeListReply` - **source of truth in form**). `vigil.*`/`decree.*` - NoSelector. Reactor flow (Portent → match Decree → enqueue scenario) by these permissions is **NOT controlled** - machine Soul-initiated path; security is based on the Decree subject binding ([ADR-030(b)](../../adr/0030-vigil-oracle.md)).

### `POST /v1/vigils` - create Vigil

Permission: `vigil.create`. MCP-tool: `keeper.oracle.vigil.create`. Read-only by design (observes, does not mutate the host).

**Request `VigilCreateRequest`** (`required: name, interval, check`):

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (kebab `^[a-z0-9-]{1,63}$`) | yes | Vigil's name. |
| `interval` | `string` (duration) | yes | Check frequency (`30s`). |
| `check` | `string` | yes | Core-beacon address (`core.beacon.file_changed`). |
| `coven` | `array<string>` | no | Subject-labels coven (**XOR** with `sid`). |
| `sid` | `string` | no | The subject is one specific SID (**XOR** with `coven`). |
| `params` | `object` | no | Scan parameters; form depends on `check` (typed schema deferred). |
| `enabled` | `boolean` | no | Whether the check is active. Default `true`. |

**Response `201 VigilView`:** `{name, coven?, sid?, interval, check, params, enabled, created_by_aid?, created_at, updated_at}`.

Errors: `400` (broken JSON), `409 vigil-already-exists` (`name` busy), `422 validation-failed` (broken `name`/`interval`/`check`/subject). Audit: `vigil.created`.

### `GET /v1/vigils` - list of Vigils

Permission: `vigil.list`. MCP-tool: `keeper.oracle.vigil.list`. Query `offset`/`limit`. Sort `created_at` DESC, `name` ASC. Response `200 VigilListReply` (`{items, offset, limit, total}`).

### `GET /v1/vigils/{name}` — read Vigil

Permission: `vigil.list` (one permission covers list+get). MCP-tool: `keeper.oracle.vigil.list`. Response `200 VigilView`; `404 not-found` - no entry.

### `DELETE /v1/vigils/{name}` - remove Vigil

Permission: `vigil.delete`. MCP-tool: `keeper.oracle.vigil.delete`. Stops distributing to hosts in `VigilSnapshot`; connected Decrees **DO NOT cascade**. Response `204`; `404 not-found`. Audit: `vigil.deleted`.

### `POST /v1/decrees` - create Decree

Permission: `decree.create`. MCP-tool: `keeper.oracle.decree.create`. Default-deny: the rule is triggered only on its `on_beacon` × subject × `incarnation_name`.

**Request `DecreeCreateRequest`** (`required: name, on_beacon, incarnation_name, action_scenario`):

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (kebab `^[a-z0-9-]{1,63}$`) | yes | Decree's name. |
| `on_beacon` | `string` (kebab) | yes | The name of the Vigil whose Portent rule responds. |
| `incarnation_name` | `string` (`^[a-z0-9][a-z0-9-]{0,62}$`) | yes | Target-incarnation of the reaction (ServiceRef resolves from it). |
| `action_scenario` | `string` (`^[a-z][a-z0-9_]*$`) | yes | Named scenario (whitelist; raw command rejected). |
| `coven` | `array<string>` | no | Subject-labels coven (**XOR** with `sid`). |
| `sid` | `string` | no | The subject is one specific SID (**XOR** with `coven`). |
| `where` | `string` (CEL) | no | Predicate over `event.data`; compile - checked on create. |
| `action_input` | `object` | no | Script input (vault-ref goes as is). |
| `cooldown` | `string` (duration) | no | Minimum interval between triggers per-(decree, subject). |
| `enabled` | `boolean` | no | Is the rule active? Default `true`. |

**Response `201 DecreeView`:** `{name, on_beacon, where?, coven?, sid?, incarnation_name, action_scenario, action_input, cooldown, enabled, created_by_aid?, created_at, updated_at}`.

Errors: `400` (broken JSON), `409 decree-already-exists` (`name` busy), `422 validation-failed` (broken `name`/`on_beacon`/`incarnation_name`/`action_scenario`/subject/`where`-CEL/`cooldown`). Audit: `decree.created`.

### `GET /v1/decrees` - list of Decrees

Permission: `decree.list`. MCP-tool: `keeper.oracle.decree.list`. Query `offset`/`limit`. Sort `created_at` DESC, `name` ASC. Response `200 DecreeListReply`.

### `GET /v1/decrees/{name}` — read Decree

Permission: `decree.list`. MCP-tool: `keeper.oracle.decree.list`. Response `200 DecreeView`; `404 not-found`.

### `DELETE /v1/decrees/{name}` - remove Decree

Permission: `decree.delete`. MCP-tool: `keeper.oracle.decree.delete`. Cascade cleans cooldown-state (`oracle_fires`, `ON DELETE CASCADE`). Response `204`; `404 not-found`. Audit: `decree.deleted`.

> **Secrets in payload.** Audit Vigil/Decree contains `name`/`check`/`interval`/subject (vigil) and `name`/`on_beacon`/`incarnation`/`scenario`/subject (decree); `params` / `where`-CEL / `action_input` **NOT put** (`action_input` may carry vault-ref in transit).
