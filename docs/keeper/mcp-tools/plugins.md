# Plugin — MCP-tools Sigil allow-list plugin integrity

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.plugin.*` (admission/revocation/list of allow-list entries `plugin_sigils`, [ADR-026](../../adr/0026-sigil.md) S4a). Transport, auth, tool declaration format, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics, bodies and `sha256`-computation is [plugins.md → Integrity-model](../plugins.md#integrity-model); REST side - [operator-api/plugins.md](../operator-api/plugins.md). Rotation of the **signature** keys themselves (separate zone) - [mcp-tools/sigils.md](sigils.md).

### Plugin (3)

Sigil allow-list plugin integrity ([ADR-026](../../adr/0026-sigil.md)). 1:1 with REST `POST/GET/DELETE /v1/plugins/sigils*` and permission (`keeper.plugin.<action>` ↔ `plugin.<action>`, selector - NoSelector, like `operator.*`/`role.*`). `ref` - git-verified (Keeper resolves `source`+`ref` into `commit_sha` slot via go-git, option A/F-fetch, [ADR-026(g)](../../adr/0026-sigil.md)): does not participate in slot lookup (read active slot via `current`), integrity authority — `sha256` + Keeper signature. Tools are only available when Sigil is configured (`keeper.yml → sigil.signing_key_ref`); with Sigil disabled, the call returns `internal-error` ("sigil is not configured").

#### `keeper.plugin.allow`

Allowing `(namespace, name, ref)` in the allow-list `plugin_sigils`: Keeper reads the active cache slot binary via `current`-symlink (R-nested `<ns>-<name>/<commit_sha>/`), reads `sha256`, signs and inserts the entry. Permission: `plugin.allow`. Endpoint: [`POST /v1/plugins/sigils`](../operator-api/plugins.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `namespace` | `string` | yes | Namespace of the plugin (kebab-case + dots/underscore; no slashes and `..`). |
| `name` | `string` | yes | Plugin name. |
| `ref` | `string` | yes | Operator-asserted tolerance label (tag-ref of the form `v1.0.0`). Branch-ref with a slash is not supported in MVP. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `namespace`, `name`, `ref` | `string` | Echo input. |
| `sha256` | `string` | SHA-256 (hex) of the accepted binary. |

Errors: `plugin-not-in-cache` (the plugin is not in the host's cache), `sigil-already-active` (there is already active permission for `(namespace, name, ref)`), `validation-failed` (broken three). Audit: `plugin.allowed`.

#### `keeper.plugin.revoke`

Revocation of active clearance `(namespace, name, ref)` from `plugin_sigils` (the binary no longer passes Sigil verification). Permission: `plugin.revoke`. Endpoint: [`DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}`](../operator-api/plugins.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `namespace` | `string` | yes | Namespace of the plugin. |
| `name` | `string` | yes | Plugin name. |
| `ref` | `string` | yes | Revoked clearance label. |

**Output:** empty object (REST equivalent - 204 No Content).

Errors: `sigil-not-found` (no active record), `validation-failed` (broken three). Audit: `plugin.revoked`.

#### `keeper.plugin.list`

Listing the active (not revoked) entries of the allow-list `plugin_sigils`, new ones first. Without `signature`/`manifest` (crypto stuff/large JSONB). Permission: `plugin.list`. Endpoint: [`GET /v1/plugins/sigils`](../operator-api/plugins.md). Async: no.

**Input:** empty object.

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `sigils` | `array<SigilView>` | Items - `{namespace, name, ref, sha256, allowed_by_aid, allowed_at, revoked_at}`. |
