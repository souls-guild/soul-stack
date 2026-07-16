# MCP-tools - Keeper-tools directory for LLM agents

Regulatory specification of the **MCP-tools directory** that the Keeper cluster publishes to listener `listen.mcp.addr` ([config.md → listen](config.md#listen)). The directory is a declarative wrapper over [Operator API](operator-api.md): each MCP-tool strictly corresponds to one HTTP endpoint `/v1/*` and one permission ([rbac.md → Permission ↔ MCP-tool / OpenAPI endpoint](rbac.md#permission--mcp-tool--openapi-endpoint)).

**The source of truth in semantics** is [operator-api.md](operator-api.md). This document describes:

- transport and auth MCP sides;
- tool declaration format according to MCP spec;
- async-convention `_apply_id`;
- mapping RFC 7807 errors → MCP-tool error;
- directory 89 tool with input/output schemas;
- that **not** published as MCP-tool;
- SSE event-payloads format for `GET /mcp/events?apply_id=<ULID>`.

Document addressed to:

- authors of LLM agents and MCP host applications (Claude Code, IDE plugins) connecting to the Keeper MCP server;
- Keeper developers implementing MCP handlers;
- `soul-lint` and similar tools that validate scripts for calling tools.

## Transport and auth

| Decision | Value | Rationale |
|---|---|---|
| **Transport** | MCP-HTTP (Streamable HTTP) - current stable revision of MCP spec. | Cross-platform, server model without stdio/SSE restrictions. |
| **Listener** | `listen.mcp.addr` is a separate HTTP listener for the Keeper cluster ([config.md → listen](config.md#listen)). | Mandatory listener according to the end-to-end requirement "embedded MCP" ([requirements.md](../requirements.md)). |
| **Auth** | `Authorization: Bearer <jwt>` is the same JWT as in the Operator API ([operator-api.md → Auth](operator-api.md#auth), [ADR-014](../adr/0014-operator-identity.md)). The MCP client sends header when connecting. | Single identity model: one Archon, one token, one RBAC. We do not create a second auth chain. |
| **Bootstrap-bypass** | Not applicable. MCP tools always require JWT; The first Archon is released through `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md)), not through MCP. | Symmetry with Operator API. |
| **Naming** | `keeper.<resource>.<action>` (4-segment, dots as separators). | Recorded in [rbac.md](rbac.md#permission--mcp-tool--openapi-endpoint) and [operator-api.md](operator-api.md#mapping-endpoint--mcp-tool--permission). |
| **Input naming** | `snake_case` for all input fields. | Matches the JSON body of the HTTP endpoint. |
| **Output naming** | `snake_case` for business fields + top-level `_apply_id` for async operations (the underscore prefix distinguishes MCP-convention from business-data). | See [§ Async operations in MCP](#async-operations-in-mcp). |
| **Pagination** | `offset` (int, ≥0, default `0`) + `limit` (int, 1..1000, default `50`). Output list-tools: `{items, offset, limit, total}`. | Symmetry with [operator-api.md → Pagination](operator-api.md#pagination). |
| **Source of truth on semantics** | [operator-api.md](operator-api.md). | MCP-tools do not duplicate business logic. |
| **Tracing** | Each MCP call receives an OTel-span with the attribute `archon.aid=<aid>` (from JWT `sub`) and `mcp.tool=<name>`. | Symmetry with Operator API ([operator-api.md → Conventions](operator-api.md#conventions)). |
| **Secret masking** | The JWT in the output (`jwt`-field) is written once to the result of the tool; in logs/OTel - masked according to the same rules as in the Operator API ([operator-api.md → Secret masking](operator-api.md)). | Single rule across transports. |

Details MCP transport / handshake / session lifecycle - in the current [MCP spec](https://spec.modelcontextprotocol.io/); this document does not duplicate them.

## Tool declaration format

Each MCP-tool is published according to the MCP spec with the following fields:

| Field | Type | Meaning |
|---|---|---|
| `name` | `string` | 4-segment name `keeper.<resource>.<action>` (dots - separators). |
| `description` | `string` | Brief description of the operation, 1-3 sentences for LLM. Full semantics is in operator-api.md. |
| `inputSchema` | `object` | JSON Schema draft 2020-12, describes input parameters. Required fields in `required: [...]`. Additional fields are not allowed (`additionalProperties: false`). |
| `outputSchema` | `object` | JSON Schema draft 2020-12 for structured output. For async-tools it contains `_apply_id: string`. |

### Example: `keeper.incarnation.create`

```json
{
  "name": "keeper.incarnation.create",
"description": "Create a new Incarnation: run the selected startup script of the specified Service, create an entry in Postgres. Asynchronous operation - returns _apply_id; query status via keeper.incarnation.get / keeper.incarnation.history.",
  "inputSchema": {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "type": "object",
    "additionalProperties": false,
    "required": ["name", "service"],
    "properties": {
      "name": {
        "type": "string",
        "pattern": "^[a-z][a-z0-9-]*$",
"description": "The name of the new instance, the root Coven label."
      },
      "service": {
        "type": "string",
"description": "Service name from keeper.yml → services[].name."
      },
      "create_scenario": {
        "type": "string",
        "pattern": "^[a-z][a-z0-9_]*$",
"description": "Name of the starting script (scenario with create: true). Empty: the service offers create scripts → selection is required (validation-failed with a list of them); service without create scripts → bare incarnation (ready without running)."
      },
      "input": {
        "type": "object",
"description": "Input for the selected startup script, validated against its input schema.",
        "default": {}
      }
    }
  },
  "outputSchema": {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "type": "object",
    "additionalProperties": false,
    "required": ["_apply_id", "incarnation"],
    "properties": {
"_apply_id": { "type": "string", "description": "Launch ULID." },
"incarnation": { "type": "string", "description": "The name of the created instance." }
    }
  }
}
```

Each of the 89 tools in the directory is declared using this template. Full compliance of input/output schema with HTTP endpoint fields - see [operator-api.md → Endpoint sections](operator-api.md).

**Enum serialization.** The MCP server uses the same enum mapping (short snake_case values without family-prefix: `"ready"`, `"connected"`, `"agent"`, ...) as the HTTP API - see [operator-api.md → Conventions → Enum serialization](operator-api.md#conventions). Full proto-constants (`INCARNATION_STATUS_READY`, ...) are not forwarded to MCP input/output.

## Async operations in MCP

There are no HTTP status codes in the MCP protocol - `202 Accepted + body {apply_id}` Operator API in MCP is displayed as a **structured output** tool with the top-level field `_apply_id` (underscore-prefix distinguishes MCP-convention from business-data).

List of async-tools: `keeper.incarnation.create`, `keeper.incarnation.rerun-last`, `keeper.incarnation.run`, `keeper.incarnation.upgrade`, `keeper.incarnation.destroy`, `keeper.push.apply`, `keeper.push.cleanup`. `keeper.soul.errand.run` - sync-by-default (server-cap 30s), if exceeded, returns `async=true` from `status=running`; poll via `keeper.errand.get`.

**Status Poll:**

- `keeper.incarnation.get` → reads `status` / `status_details` instance.
- `keeper.incarnation.history` → returns records `state_history` with field `apply_id`; the element with `apply_id == <ULID>` appears after a successful commit.

There is **no separate tool `keeper.apply.get` in MVP** - symmetry with the Operator API ([operator-api.md → Async operations](operator-api.md#async-operations)).

## Errors

RFC 7807 ProblemDetails Operator API ([operator-api.md → Error format](operator-api.md#error-format-rfc-7807)) is displayed in MCP-tool error as follows:

| RFC 7807 field | MCP-tool error field | Conversion |
|---|---|---|
| `type` (URI suffix under `https://soul-stack.io/errors/`) | `code` | Take suffix URN: `https://soul-stack.io/errors/incarnation-locked` → `code: "incarnation-locked"`. |
| `title` | — | Not forwarded separately (short text, duplicates `code`). |
| `status` | — | HTTP status code in MCP is not applicable; The MCP client does not parse it. |
| `detail` | `message` | Free text from `detail` ProblemDetails. |
| `instance` | `data.instance` | Failed request URI (`/v1/...`); useful for auditing. The `data` MCP error field can contain an arbitrary structured-payload. |

Full list of error codes - stable URN suffixes from [operator-api.md → Error types](operator-api.md):

| Code | When occurs in MCP |
|---|---|
| `unauthenticated` | JWT is missing/not valid/expired. |
| `forbidden` | RBAC check failed. `message` contains the required permission. |
| `not-found` | The resource does not exist. |
| `validation-failed` | Semantic input validation error. |
| `malformed-request` | Invalid JSON/incorrect query params. |
| `incarnation-locked` | Incarnation in `error_locked` - call `keeper.incarnation.unlock` before a new run. In `keeper.incarnation.rerun-last` - the status is not `error_locked` (nothing to restart). |
| `rerun-input-unavailable` | `keeper.incarnation.rerun-last` (day-2-path) cannot restore the input of a failed run: recipe `apply_runs.recipe` cleaned up by retention Reaper / legacy run without a recipe (fail-closed) - remove the block with the usual `unlock` and run the script manually with an explicit input. Separate code from `incarnation-locked` (REST `TypeRerunInputUnavailable`, 409): machine-readable distinguishing "input lost" from "status not `error_locked`". |
| `migration-failed` | Incarnation in `migration_failed` - manual parsing of state_history is required. |
| `would-lock-out-cluster` | The operation would leave the cluster without an active Archon with an effective `*`-permission. Occurs in `keeper.operator.revoke` (recall of the last `*`-Archon), in role-operations `keeper.role.delete` / `keeper.role.update` / `keeper.role.revoke-operator` (see [§ Role](#role-6)) and in synod-operations `keeper.synod.delete` / `keeper.synod.remove-operator` / `keeper.synod.revoke-role` (effective `*` may come via Synod, see [§ Synod](#synod-8)). |
| `role-not-found` | The role with the specified `name` is missing from `rbac_roles` (`keeper.role.delete` / `keeper.role.update` / `keeper.role.grant-operator` / `keeper.role.revoke-operator`; `keeper.synod.grant-role` over a non-existent role). |
| `role-already-exists` | `name` role is occupied (`keeper.role.create`). |
| `role-builtin` | The role with `builtin=true` (`cluster-admin`) - `keeper.role.delete` / `keeper.role.update` above it is prohibited. `grant-operator` / `revoke-operator` over the builtin role are allowed. |
| `synod-not-found` | The synod group with the specified `name` is missing in `synods` (`keeper.synod.update` / `keeper.synod.delete` / `keeper.synod.add-operator` / `keeper.synod.grant-role`). |
| `synod-already-exists` | Group `name` is busy (`keeper.synod.create`). |
| `synod-builtin` | The group with `builtin=true` - `keeper.synod.delete` above it is prohibited. |
| `incarnation-already-exists` | An Incarnation with the specified `name` has already been created. |
| `operator-already-exists` | AID is already busy. |
| `soul-already-exists` | The SID is already registered in the registry `souls`. |
| `bootstrap-token-active` | Soul already has an active bootstrap token - re-release with `force: true` (`keeper.soul.issue-token`). |
| `plugin-not-in-cache` | The active plugin slot `(namespace, name)` is not in the host cache (no `current`-symlink / broken slot, `keeper.plugin.allow`). |
| `sigil-already-active` | There is already an active permit for `(namespace, name, ref)` (`keeper.plugin.allow`). |
| `sigil-not-found` | There is no active allow-list entry on `(namespace, name, ref)` (`keeper.plugin.revoke`). |
| `sigil-key-not-found` | There is no signing key with this `key_id` (`keeper.sigil.key.set-primary` / `keeper.sigil.key.retire`). |
| `sigil-key-last-active` | The last active signature key cannot be displayed - the set must not be empty (`keeper.sigil.key.retire`). |
| `sigil-key-primary` | You cannot display the primary key directly - first set-primary to another active (`keeper.sigil.key.retire`). |
| `sigil-key-concurrent-change` | Primary installation race or retired key with set-primary; retry(`keeper.sigil.key.introduce` / `set-primary`). |
| `service-already-exists` | `name` Service is busy in the registry `service_registry` (`keeper.service.register`). |
| `service-not-registered` | `service` is missing from `keeper.yml → services[]`. |
| `omen-already-exists` | `name` Omen is busy in the registry `omens` (`keeper.augur.omen.create`). |
| `provider-already-exists` | `name` Provider is busy in the registry `providers` (`keeper.provider.create`). |
| `profile-already-exists` | `name` Profile - I am busy in the registry `profiles` (`keeper.profile.create`). |
| `provider-has-profiles` | The removal of the Provider is blocked - it is referenced by the Profile (`keeper.provider.delete`; FK `ON DELETE RESTRICT`). |
| `errand-not-cancellable` | Errand is already in terminal status - there is nothing to cancel (`keeper.errand.cancel`, ADR-033 slice E5). |
| `internal-error` | Unplanned error; full diagnostics - in OTel-trace. |

> Unknown-but-valid script in `keeper.incarnation.run` - **not** call error: tool returns `_apply_id` (async-accepted), run then goes to `error_locked` (`scenario_load_failed`), status is polled via `keeper.incarnation.get`. Symmetrically [operator-api/incarnations.md → `POST …/scenarios/{scenario}`](operator-api/incarnations.md).

Extending the code list - only-add symmetrically Operator API.

## Catalog 89 MCP-tool

1:1 with HTTP endpoints from [operator-api.md → Mapping endpoint ↔ MCP-tool ↔ permission](operator-api.md#mapping-endpoint--mcp-tool--permission). For each tool: input schema (short table of fields), output schema, cross-link to the endpoint section of operator-api.md as a source of truth for semantics.

Field names input - 1:1 with JSON body HTTP endpoint; The paths in output are the same as in the HTTP response. Async-tools are marked in the **Async** column.

### Operator (3)

Moved to a domain file - [mcp-tools/operator.md](mcp-tools/operator.md): `keeper.operator.create`, `keeper.operator.revoke`, `keeper.operator.issue-token`. The source of truth for semantics is [operator-api/operator.md](operator-api/operator.md).

### Role (6)

Moved to a domain file - [mcp-tools/roles.md](mcp-tools/roles.md): `keeper.role.create`, `keeper.role.delete`, `keeper.role.list`, `keeper.role.update`, `keeper.role.grant-operator`, `keeper.role.revoke-operator`. The source of truth for semantics is [operator-api/roles.md](operator-api/roles.md) (bodies and invariants are [rbac.md → REST `/v1/roles`](rbac.md#rest-v1roles)).

### Synod (8)

Moved to a domain file - [mcp-tools/synods.md](mcp-tools/synods.md): `keeper.synod.create`, `keeper.synod.delete`, `keeper.synod.list`, `keeper.synod.update`, `keeper.synod.add-operator`, `keeper.synod.remove-operator`, `keeper.synod.grant-role`, `keeper.synod.revoke-role`. The source of truth for semantics is [operator-api/synods.md](operator-api/synods.md) (bodies and invariants are [rbac.md → REST `/v1/synods`](rbac.md#rest-v1synods)).

### Incarnation (11)

Moved to a domain file - [mcp-tools/incarnations.md](mcp-tools/incarnations.md): `keeper.incarnation.create`, `keeper.incarnation.rerun-last`, `keeper.incarnation.run`, `keeper.incarnation.get`, `keeper.incarnation.list`, `keeper.incarnation.history`, `keeper.incarnation.unlock`, `keeper.incarnation.upgrade`, `keeper.incarnation.check-drift`, `keeper.incarnation.destroy`, `keeper.incarnation.traits-set` - eleven tools with MCP pairing to REST routes [operator-api.md → Incarnation (17)](operator-api.md). Six REST-only routes do not have an MCP tool: `PATCH /v1/incarnations/{name}/hosts`, `POST …/scenarios/{scenario}/form-prefill`, `GET …/runs`, `GET …/runs/{apply_id}`, `POST …/secrets/reveal`, `GET …/secrets/revealable`; global `GET /v1/runs` + `/v1/runs/stats` ([operator-api.md → Runs (2)](operator-api.md)) - also REST-only. The source of truth for semantics is [operator-api/incarnations.md](operator-api/incarnations.md).

### Soul (5)

Moved to a domain file - [mcp-tools/souls.md](mcp-tools/souls.md): `keeper.soul.create`, `keeper.soul.issue-token`, `keeper.soul.coven-assign`, `keeper.soul.list`, `keeper.soul.ssh-target.update`. The source of truth for semantics is [operator-api/souls.md](operator-api/souls.md). Read registry routes (`GET /v1/souls/{sid}`, `/soulprint`, `/history`) - REST-only (no MCP tools).

### Plugin (3)

Moved to a domain file - [mcp-tools/plugins.md](mcp-tools/plugins.md): `keeper.plugin.allow`, `keeper.plugin.revoke`, `keeper.plugin.list`. The source of truth for semantics is [operator-api/plugins.md](operator-api/plugins.md) (Integrity-model details are [plugins.md → Integrity-model](plugins.md#integrity-model)). Rotation of the **signature keys** (separate zone) - [mcp-tools/sigils.md](mcp-tools/sigils.md).

### Sigil-key (4)

Moved to a domain file - [mcp-tools/sigils.md](mcp-tools/sigils.md): `keeper.sigil.key.introduce`, `keeper.sigil.key.list`, `keeper.sigil.key.set-primary`, `keeper.sigil.key.retire`. The source of truth for semantics is [operator-api/sigils.md](operator-api/sigils.md). Tolerances of the binaries themselves (allow-list) - [mcp-tools/plugins.md](mcp-tools/plugins.md).

### Service (4)

Service registry `service_registry` (ADR-028 RBAC-storage pattern: directory `services[]` is transferred from static `keeper.yml` to managed-via-OpenAPI/MCP PG-table). 1:1 with REST `POST/GET/PATCH/DELETE /v1/services*` and permission (`keeper.service.<action>` ↔ `service.<action>`, selector - NoSelector, like `operator.*`/`role.*`). Business logic (validation `name`/`git`/`ref`/`refresh`, cluster-wide snapshot validation after commit) lives in `serviceregistry.Service`; tool - transport. Tools are only available when the registry is connected; when disabled, the call returns `internal-error` ("service registry is not configured").

#### `keeper.service.register`

Registers a Service in `service_registry`: git-source service-repo + `ref` (version = git ref, [ADR-007](../adr/0007-versioning-git-ref.md)) + opt. auto-`refresh`. Permission: `service.register`. Endpoint: [`POST /v1/services`](operator-api.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Service name (kebab-case `^[a-z][a-z0-9-]*$`). |
| `git` | `string` | yes | git source service repo (URL; no secret). |
| `ref` | `string` | yes | git ref (tag/branch) - version of the Service. |
| `refresh` | `string` | no | duration auto-refresh(`5m`); omitted - no auto-refresh. |

**Output:** `ServiceView` — `{name, git, ref, refresh?, created_by_aid?, updated_by_aid?, created_at, updated_at}`.

Errors: `service-already-exists` (`name` busy), `not-found` (creator's AID missing from `operators`), `validation-failed` (broken `name`/`git`/`ref`/`refresh`). Audit: `service.registered`.

#### `keeper.service.update`

Replaces mutable fields of a Service record (`git`/`ref`/`refresh`, replace semantics); `name` - key, does not change. Permission: `service.update`. Endpoint: [`PATCH /v1/services/{name}`](operator-api.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Service name (record key). |
| `git` | `string` | yes | New git source. |
| `ref` | `string` | yes | New git ref. |
| `refresh` | `string` | no | duration auto-refresh (`5m`). |

**Output:** `ServiceView`.

Errors: `not-found` (there is no record or the editor's AID is missing in `operators`), `validation-failed` (broken `git`/`ref`/`refresh`). Audit: `service.updated`.

#### `keeper.service.list`

List of registered Services (sort `name` ASC). Permission: `service.list`. Endpoint: [`GET /v1/services`](operator-api.md). Async: no.

**Input:** empty object.

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `services` | `array<ServiceView>` | Items - `{name, git, ref, refresh?, created_by_aid?, updated_by_aid?, created_at, updated_at}`. |

#### `keeper.service.deregister`

Removes a Service entry from `service_registry` by name. Permission: `service.deregister`. Endpoint: [`DELETE /v1/services/{name}`](operator-api.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Service name. |

**Output:** empty object (REST equivalent - 204 No Content).

Errors: `not-found` (no entry). Audit: `service.deregistered`.

### Augur (6)

Moved to a domain file - [mcp-tools/augur.md](mcp-tools/augur.md): `keeper.augur.omen.create`, `keeper.augur.omen.list`, `keeper.augur.omen.delete`, `keeper.augur.rite.create`, `keeper.augur.rite.list`, `keeper.augur.rite.delete`. The source of truth for semantics is [operator-api/augur.md](operator-api/augur.md). **Live-fetch from Soul (`AugurRequest`) is NOT controlled by these tools** ([rbac.md §Augur](rbac.md)).

### Oracle (6)

Moved to a domain file - [mcp-tools/oracle.md](mcp-tools/oracle.md): `keeper.oracle.vigil.create`, `keeper.oracle.vigil.list`, `keeper.oracle.vigil.delete`, `keeper.oracle.decree.create`, `keeper.oracle.decree.list`, `keeper.oracle.decree.delete`. The source of truth for semantics is [operator-api/oracle.md](operator-api/oracle.md). **Reactor flow (Portent → match Decree → enqueue) is NOT controlled by these tools** ([rbac.md §Oracle](rbac.md)).

### Errand (4)

Moved to a domain file - [mcp-tools/errands.md](mcp-tools/errands.md): `keeper.soul.errand.run`, `keeper.errand.list`, `keeper.errand.get`, `keeper.errand.cancel`. The source of truth for semantics is [operator-api/errands.md](operator-api/errands.md).

### Voyage (4)

Moved to a domain file - [mcp-tools/voyages.md](mcp-tools/voyages.md): `keeper.voyage.start`, `keeper.voyage.get`, `keeper.voyage.list`, `keeper.voyage.cancel`. `POST /v1/voyages/preview` - REST-only (no MCP-tool). The source of truth for semantics is [operator-api/voyages.md](operator-api/voyages.md).

### Push (2)

Moved to a domain file - [mcp-tools/push.md](mcp-tools/push.md): `keeper.push.apply`, `keeper.push.cleanup`. The source of truth for semantics is [operator-api/push.md](operator-api/push.md).

### Cloud (8)

CRUD registries of Cloud-Providers (`providers`) and Cloud-Profiles (`profiles`, ADR-017, [cloud.md → Provider and Profile](cloud.md)). **Implemented** (REST + MCP, one source of truth `provider.Service` / `profile.Service`): four tools per entity - `create` / `list` / `get` / `delete`. **`update`-tool-but no** - Provider/Profile are immutable (change parameters = `delete` + `create`, protection against partial mutation spec of living VMs); therefore read-visibility gates one permission `provider.read` / `profile.read` (pattern `operator.list`↔`read`). Selector - NoSelector. Tools are only available when the registry is connected; when disabled, the call returns `internal-error`. Async: no. The source of truth for semantics is [cloud.md](cloud.md), permission directory is [rbac.md → Cloud](rbac.md#cloud-6--cloudmd).

`credentials_ref` (Provider only) is stored and returned as **path** `vault:<mount>/<path>` - the credentials themselves are NOT resolved or returned by the API; The path is also written in audit (not a secret).

#### `keeper.provider.create`

Creating a Provider. Permission: `provider.create`. Endpoint: `POST /v1/providers`. Audit: `provider.created`.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (kebab-case) | yes | Provider name. |
| `type` | `string` (kebab-case) | yes | CloudDriver plugin name (`soul-cloud-<type>`). |
| `region` | `string` | yes | Region/zone. |
| `credentials_ref` | `string` (`vault:<path>`) | yes | Vault-ref to credentials (path, no secret). |

**Output:** `{name, type, region, credentials_ref, created_at (RFC 3339), created_by_aid?}` - mirror input + server-side labels.

Errors: `provider-already-exists` (`409`, double `name`); `validation-failed` (broken `name`/`type`/`region`/`credentials_ref`).

#### `keeper.provider.list`

Enumeration of Providers (paged). Permission: `provider.read`. Endpoint: `GET /v1/providers`.

**Input:** `{offset?, limit?}` (`limit` default `100`). **Output:** `{items: [...], offset, limit, total}`.

#### `keeper.provider.get`

Reading one Provider by name. Permission: `provider.read`. Endpoint: `GET /v1/providers/{name}`.

**Input:** `{name}`. **Output:** `providerViewOut`. Errors: `not-found`.

#### `keeper.provider.delete`

Removing Provider. Permission: `provider.delete`. Endpoint: `DELETE /v1/providers/{name}`. Audit: `provider.deleted`.

**Input:** `{name}`. **Output:** empty object. Errors: `not-found`; `provider-has-profiles` (`409` - Provider is referenced by Profiles, FK `ON DELETE RESTRICT`; first delete dependent Profiles).

#### `keeper.profile.create`

Creating a Profile. Permission: `profile.create`. Endpoint: `POST /v1/profiles`. Audit: `profile.created`.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (kebab-case) | yes | Profile name. |
| `provider` | `string` | yes | Name of the registered Provider (FK). |
| `params` | `object` | optional | VM parameters (freeform jsonb; validated against the `profile_schema` CloudDriver plugin on the scenario layer, not in CRUD). |
| `cloud_init` | `string` | optional | Raw cloud-init userdata. |

**Output:** `{name, provider, params, cloud_init?, created_at, created_by_aid?}`.

Errors: `profile-already-exists` (`409`, double `name`); `validation-failed` (`422` - link to a non-existent Provider (FK) or broken `name`/`provider`).

#### `keeper.profile.list`

Enumeration of Profiles (paged; optional filter by Provider). Permission: `profile.read`. Endpoint: `GET /v1/profiles`.

**Input:** `{provider?, offset?, limit?}`. **Output:** `{items: [...], offset, limit, total}`.

#### `keeper.profile.get`

Reading one Profile by name. Permission: `profile.read`. Endpoint: `GET /v1/profiles/{name}`.

**Input:** `{name}`. **Output:** `profileViewOut`. Errors: `not-found`.

#### `keeper.profile.delete`

Deleting Profile. Permission: `profile.delete`. Endpoint: `DELETE /v1/profiles/{name}`. Audit: `profile.deleted`.

**Input:** `{name}`. **Output:** empty object. Errors: `not-found`.

### Push-Provider (5)

Moved to a domain file - [mcp-tools/push-providers.md](mcp-tools/push-providers.md): `keeper.push-provider.create`, `keeper.push-provider.update`, `keeper.push-provider.delete`, `keeper.push-provider.list`, `keeper.push-provider.read`. The source of truth for semantics is [operator-api/push-providers.md](operator-api/push-providers.md). Sensitive params (`secret_id`/`token`/`password`/`private_key`) MUST be vault-refs.

### Herald (5)

Moved to a domain file - [mcp-tools/heralds.md](mcp-tools/heralds.md): `keeper.herald.create`, `keeper.herald.update`, `keeper.herald.delete`, `keeper.herald.list`, `keeper.herald.read`. The source of truth for semantics is [operator-api/heralds.md](operator-api/heralds.md). Run notification delivery channels ([ADR-052](../adr/0052-herald-notifications.md)); webhook + SSRF-guard (https-only/deny-private by default), `secret_ref` - vault-ref to signing-token (signature `X-SoulStack-Signature: sha256=<hex>`).

### Tiding (5)

Moved to a domain file - [mcp-tools/tidings.md](mcp-tools/tidings.md): `keeper.tiding.create`, `keeper.tiding.update`, `keeper.tiding.delete`, `keeper.tiding.list`, `keeper.tiding.read`. The source of truth for semantics is [operator-api/tidings.md](operator-api/tidings.md). Subscription rules (`event_types` area-glob in scope runs → Herald); `herald` - FK to existing Herald.

### Cadence (0) and Choir (0) - REST-only

Domains **Cadence** (`/v1/cadences*`, [ADR-046](../adr/0046-cadence.md)) and **Choir** (`/v1/incarnations/{name}/choirs*`, [ADR-044](../adr/0044-choir.md)) **do not have MCP tools** - `manifest.go` does not contain them. Stub files record the absence: [mcp-tools/cadences.md](mcp-tools/cadences.md), [mcp-tools/choirs.md](mcp-tools/choirs.md). These domains are managed only through the Operator API ([operator-api/cadences.md](operator-api/cadences.md), [operator-api/choirs.md](operator-api/choirs.md)).

## What is NOT published as MCP-tool

### Health / Meta endpoints

`/healthz`, `/readyz`, `/metrics`, `/openapi.yaml` - **not MCP-tools**. The LLM agent should not pull healthcheck/metrics; their consumers are orchestrators, monitoring, documentation. Access - directly via HTTP to `listen.openapi.addr` / `listen.metrics.addr` without auth (metrics - on a separate listener, see [config.md → listen](config.md#listen)).

### Bootstrap of the first Archon

`keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md)) - administrative subcommand, executed on the keeper host by an operator with shell access. MCP-tool like `keeper.bootstrap.init` **no**: the first Archon is created when the registry `operators` is empty; MCP access requires JWT, which has not yet been released. Bootstrap-bypass through MCP is not introduced deliberately - this will reduce the security boundary.

### Future reads and deletes

Directory 89 tool - 1:1 with MVP directory permissions from [rbac.md → Directory permissions](rbac.md). Reads/deletions that are deferred until the corresponding permissions (`operator.get`, `soul.get`, etc.) are available are added to this directory in one PR with the extension `rbac.md` + `operator-api.md` + of this document. Cloud-CRUD (`provider.*` / `profile.*`) - no longer deferred: fully implemented (create/list/get/delete per entity, see [§ Cloud](#cloud-8)).

## SSE event payloads

`GET /mcp/events?apply_id=<ULID>` sends a Server-Sent Events stream with typed apply-event-payloads published by the in-memory bus [`keeper/internal/applybus`](https://github.com/souls-guild/soul-stack/tree/main/keeper/internal/applybus). The bus connects publishers (Keeper-side handlers of EventStream-payloads [`TaskEvent`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/grpc/events_taskevent.go) / [`RunResult`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/grpc/events_runresult.go) and, in the future, scenario-runners) with SSE-subscribers.

This section is a regulatory fixation of the SSE-frame and payload format; code publishers and handler are at the source of truth in semantics (see links above).

### SSE frame

Each apply event is broadcast by one SSE frame:

```
event: <kind>
id: <apply_id>
data: <json payload>

```

- `event:` - name `EventKind` (see below), stable snake-case with dot separator.
- `id:` - `apply_id` events (ULID); The SSE client sees it in `MessageEvent.lastEventId`.
- `data:` — one line JSON-payload (without hyphens); the structure depends on `kind` (see § Per-kind schema).
- Terminates with an empty line according to SSE-spec.

### Common payload fields

All payloads are a JSON object with three required keys:

| Field | Type | Description |
|---|---|---|
| `apply_id` | string ULID | Id apply-run; duplicates SSE-frame `id:`-line (subscriber can rely on any). |
| `kind` | string (closed enum) | Event type (see below); duplicates SSE-frame `event:`-line. |
| `sid` | string FQDN | Soul is the initiator of the event. Payload source: Soul-side TaskEvent/RunResult, mTLS peer cert upon delivery EventStream - authoritative SID. |

**What is not in the SSE payload:**

- Fields `at` (post timestamp) in JSON-payload **no** - it lives in the internal structure of `applybus.Event.At` and is used only for logging/diagnostics. The SSE client takes the timestamp from itself when receiving an event or puts it on the keeper-side scenario-runner side in `state_changes`/audit-log (source - Postgres `audit_log.created_at`).
- The `register_data` (TaskEvent) fields in the SSE payload are **not**. This is a deliberate simplification: register-data can be large and/or contain secrets - for the audit chain it is written in `audit_log.payload.register_data` (run through [`audit.MaskSecrets`](https://github.com/souls-guild/soul-stack/tree/main/shared/audit)); The SSE client that monitors the progress of the run does not need it.

### Event kinds (closed enum)

| `kind` | When is it published | Source |
|---|---|---|
| `apply.started` | The Apply run has started. Reserved for keeper-side scenario-runner; in M0.7.c there is **no** publisher, but the SSE client must recognize `kind` for forward-compat. | scenario-runner (post-MVP). |
| `task.executed` | One task within the run has completed (any `TaskStatus`: OK / FAILED / CANCELLED / SKIPPED). | [`handleTaskEvent`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/grpc/events_taskevent.go) when receiving `TaskEvent` from Soul. |
| `apply.completed` | The run completed successfully (`RUN_STATUS_SUCCESS`). | [`handleRunResult`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/grpc/events_runresult.go). |
| `apply.failed` | The run failed (`RUN_STATUS_FAILED` / `RUN_STATUS_ERROR_LOCKED` / any failure not attributed to `cancelled`). | `handleRunResult`. |
| `apply.cancelled` | Run canceled (`RUN_STATUS_CANCELLED`). | `handleRunResult`. |

Enum extension - only-add: new kinds are added here and in [`applybus.EventKind`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/applybus/bus.go) with one PR, without renaming existing ones.

### Per-kind schema

#### `apply.started`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "apply.started",
  "sid": "host-01.example.com"
}
```

No additional fields. Reserved for the scenario-runner - in M0.7.c the publisher does not issue such events, but the SSE client should not drop the stream when it encounters a kind.

#### `task.executed`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "task.executed",
  "sid": "host-01.example.com",
  "task_idx": 3,
  "task_status": "TASK_STATUS_FAILED",
  "error": {
    "code": "module.failed",
    "module": "core.pkg"
  }
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `task_idx` | integer (≥0) | yes | The task index in the `RenderedTask[]` apply-run. |
| `task_status` | string | yes | The full name of the enum constant `TaskStatus` from proto (`TASK_STATUS_OK` / `TASK_STATUS_FAILED` / `TASK_STATUS_CANCELLED` / ...). When expanding the Soul-side enum, new values go to the payload "as is" - the SSE client must treat unknown values as the `failed` analogue for UX, and not crash. |
| `error` | object | optional | Filled only when `task_status` ≠ OK. Structure - `{code, module}` (subset of `keeperv1.ModuleError`). **`message` (task stderr) is NOT published on SSE** (BUG-3 floor): stderr of a fallen task may carry a plaintext secret (especially `no_log: true` task), which `MaskSecrets` does not catch according to vault-ref; the `no_log` flag lives in the run-goroutine, but SSE-publish (grpc layer) on multi-Keeper does not know it (ADR-002, ADR-012(d)). The operator receives the detailed safe reason via `status_details` / `GET /v1/incarnations/<name>` (there no_log is suppressed + double `MaskSecrets`, see `scenario.failureReason`). `code`/`module` carry triage without body stderr. |

#### `apply.completed`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "apply.completed",
  "sid": "host-01.example.com",
  "run_status": "RUN_STATUS_SUCCESS",
  "state_changes": {
    "users": [{"name": "alice", "action": "added"}]
  }
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `run_status` | string | yes | The full name of the enum constant `RunStatus` from proto. For `apply.completed` - always `RUN_STATUS_SUCCESS`. |
| `state_changes` | object | optional | State delta calculated by the Soul-side scenario-runner and passed to `RunResult.state_changes`. JSON object (decoded from `google.protobuf.Struct`). May be absent if scenario does not modify state. |

#### `apply.failed`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "apply.failed",
  "sid": "host-01.example.com",
  "run_status": "RUN_STATUS_FAILED"
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `run_status` | string | yes | `RUN_STATUS_FAILED` / `RUN_STATUS_ERROR_LOCKED` / any other non-success / non-cancelled. The SSE client must recognize a specific sub-status by this field. |

Field `state_changes` for `apply.failed` **not published**: state is not overwritten for an error run (see `commitRunState`), and it is prohibited to give a partial delta outside. Per-task diagnostics are collected by the client from previous `task.executed` events with the `error` field.

#### `apply.cancelled`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "apply.cancelled",
  "sid": "host-01.example.com",
  "run_status": "RUN_STATUS_CANCELLED"
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `run_status` | string | yes | Always `RUN_STATUS_CANCELLED`. |

`state_changes` is missing for the same reason as for `apply.failed`.

### Lifecycle: subscribe semantics

- **In-memory only.** In M0.7.c, the bus is an in-memory single-Keeper, without persistence: the event is delivered only to subscribers existing at the time of `Publish`. Late subscriber (connected AFTER the event was published) will not receive an already sent event** - there is no replay in M0.7.c **.
- **Subscribe-then-call.** The correct order for the client is to first subscribe `GET /mcp/events?apply_id=<ULID>`, wait for `200 OK` + the first `:keepalive\n\n` from the server, and only then call the async-tool (`tools/call keeper.incarnation.create` / `keeper.incarnation.run` / ...). Otherwise, there is a risk of missing early `task.executed` events of small runs.
- **Buffer overflow → drop-oldest.** Per-subscriber buffer - 64 events ([`applybus.SubscriberBufferSize`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/applybus/bus.go)). When overflowing (slow client), the bus drops the oldest event and writes `warn` to slog - the publisher is never blocked. The client SHOULD read the stream without delay; The newest-oldest order guarantee remains.
- **Connection close → auto-unsubscribe.** Closing an SSE connection (client-side `EventSource.close()` or transport disconnect) cancels the HTTP-request-context; the bus detects `ctx.Done()`, removes the subscriber from the map and closes the channel. An explicit unsubscribe call is not required.

### Heartbeat

Every 30 seconds (`sseHeartbeatInterval` in [`keeper/internal/mcp/sse.go`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/mcp/sse.go)) the server writes to stream SSE-comment-line:

```
:keepalive

```

This is **not an event** (no `event:`/`data:` fields); reverse-proxy (nginx, AWS ALB) and browser `EventSource` respect the comment-line, do not deliver it to the JS handler and do not close the connection due to idle-timeout. Frequency is not configurable in M0.7.c.

### Auth and RBAC

- SSE-handler requires the same JWT as `POST /mcp` (see § Transport and auth). The auth error is returned HTTP `401` / `400` with JSON-error-body (not SSE format) - the client has not yet subscribed, there is no point in opening a stream only to immediately close it.
- There is no separate RBAC-permission for `/mcp/events` in M0.7.c: any authorized Archon can subscribe to his `apply_id`. The detail check "can this AID know the state of a specific apply_id" is deferred to the scenario-runner (mapping `apply_id → archon_aid` will be part of the runner-table).

### Cluster-mode (cross-instance routing)

In a horizontally scalable cluster (ADR-002), the Soul of the run can be connected to Keeper instance B, while the SSE-subscriber `GET /mcp/events?apply_id=X` hangs on Keeper instance A. Cross-instance routing of applybus events **implemented** via Redis pub/sub ([ADR-006(c.1)](https://github.com/souls-guild/soul-stack/blob/main/docs/adr/0006-cache-redis.md)): publisher broadcasts the event to the **sharded channel `events:shard:<n>`**, where `n = fnv32a(apply_id) % 256` (fixed set of K=256 shards). Each Keeper holds a Redis-bridge **per-shard** (not per-applyID): the first Subscribe of any applyID of a given shard raises one subscription to the shard channel, the remaining applyIDs of the same shard reuse it. Forward-loop filters incoming events by `envelope.apply_id` and distributes only to local subscribers of the corresponding applyID - the collision of two runs into one shard (frequency ≈ 1/K) does not mix their payloads. Sticky-session on LB is not required: subscriber on any instance will receive run events from any other. Implementation - [`keeper/internal/redis/applybus.go`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/redis/applybus.go) (`ApplyBusChannel`/`ApplyBusShardIndex`/`ApplyBusShardCount`) + per-shard `bridges` and `deliverFromCluster` filter in [`keeper/internal/applybus/bus.go`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/applybus/bus.go).

### Client examples

#### curl

```bash
APPLY_ID=$(curl -sS -X POST https://keeper.example.com/mcp \
  -H "Authorization: Bearer ${KEEPER_JWT}" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{
    "name":"keeper.incarnation.run",
    "arguments":{"name":"prod-app","scenario":"deploy","input":{"version":"v1.2.3"}}
  }}' | jq -r '.result.structuredContent._apply_id')

curl -N -sS https://keeper.example.com/mcp/events?apply_id=${APPLY_ID} \
  -H "Authorization: Bearer ${KEEPER_JWT}" \
  -H "Accept: text/event-stream"
```

#### JavaScript (EventSource)

The standard `EventSource` does not support custom headers - use [`@microsoft/fetch-event-source`](https://github.com/Azure/fetch-event-source) (or an analogue) to pass `Authorization`:

```javascript
import { fetchEventSource } from '@microsoft/fetch-event-source';

await fetchEventSource(`/mcp/events?apply_id=${applyId}`, {
  headers: { Authorization: `Bearer ${jwt}` },
  onmessage(ev) {
    const payload = JSON.parse(ev.data);
    switch (ev.event) {
      case 'task.executed':
        console.log(`[${payload.task_idx}] ${payload.task_status}`,
                    payload.error ?? '');
        break;
      case 'apply.completed':
        console.log('done', payload.state_changes);
        break;
      case 'apply.failed':
      case 'apply.cancelled':
        console.error(ev.event, payload.run_status);
        break;
    }
  },
  onerror(err) { throw err; },
});
```

## See also

- [operator-api.md](operator-api.md) - source of truth for semantics, request/response schemas, error codes.
- [rbac.md → Permissions directory](rbac.md) and [rbac.md → Permission ↔ MCP-tool / OpenAPI endpoint](rbac.md#permission--mcp-tool--openapi-endpoint) - permissions directory and 1:1 mapping.
- [config.md → `listen.mcp.addr`](config.md#listen) — bind address of the MCP listener. [config.md → `auth`](config.md#auth) - JWT signature.
- [`../architecture.md → ADR-013`](../adr/0013-bootstrap-archon.md) - Bootstrap of the first Archon (outside MCP).
- [`../architecture.md → ADR-014`](../adr/0014-operator-identity.md) — operator identity model, JWT-claims.
- [`../requirements.md`](../requirements.md) - "embedded MCP" as an end-to-end requirement.
- [MCP spec](https://spec.modelcontextprotocol.io/) - transport / handshake / session lifecycle details.
