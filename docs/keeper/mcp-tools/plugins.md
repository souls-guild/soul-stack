# Plugin — MCP-tools Sigil allow-list plugin integrity

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.plugin.*` (admission/revocation/list of allow-list entries `plugin_sigils`, [ADR-026](../../adr/0026-sigil.md) S4a). Transport, auth, tool declaration format, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics, bodies and `sha256`-computation is [plugins.md → Integrity-model](../plugins.md#integrity-model); REST side - [operator-api/plugins.md](../operator-api/plugins.md). Rotation of the **signature** keys themselves (separate zone) - [mcp-tools/sigils.md](sigils.md).

### Plugin (3)

Sigil allow-list plugin integrity ([ADR-026](../../adr/0026-sigil.md)). 1:1 with REST `POST/GET/DELETE /v1/plugins/sigils*` and permission (`keeper.plugin.<action>` ↔ `plugin.<action>`, selector - NoSelector, like `operator.*`/`role.*`). `ref` - git-verified (Keeper resolves `source`+`ref` into `commit_sha` slot via go-git, option A/F-fetch, [ADR-026(g)](../../adr/0026-sigil.md)): does not participate in slot lookup (read active slot via `current`), integrity authority — `sha256` + Keeper signature. Tools are only available when Sigil is configured (`keeper.yml → sigil.signing_key_ref`); with Sigil disabled, the call returns `internal-error` ("sigil is not configured").

#### `keeper.plugin.allow`

Allowing an artifact in the allow-list `plugin_sigils`: the body is `{alias, source, ref}` — `source`+`ref` are the signed artifact identity, `alias` is address level 1 and is **not** signed (NIM-377 / NIM-438). Keeper reads the active cache slot binary via the `current` symlink (R-nested `<alias>/<commit_sha>/`), reads `sha256`, signs and inserts the entry. Permission: `plugin.allow`. Endpoint: [`POST /v1/plugins/sigils`](../operator-api/plugins.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `alias` | `string` | yes | Registration alias — address level 1, the operator's choice and the cache slot to read (`^[a-z][a-z0-9-]{0,62}$`, not on the [reserved list](../../naming-rules.md#reserved-namespace-names)). NOT signed. |
| `source` | `string` | yes | The artifact source the approval is ON: the git remote for a `kind: git` entry, the publication `base_url` for `kind: artifact`. SIGNED. For the artifact kind it must equal the address the Keeper actually fetched the release from, or the call is refused. |
| `ref` | `string` | yes | Operator-asserted version label (tag-ref of the form `v1.0.0`). Branch-ref with a slash is not supported in MVP. SIGNED. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `alias`, `source`, `ref` | `string` | Echo input. |
| `kind` | `string` | Source kind the slot was resolved by: `git` or `artifact`. |
| `artifacts` | `array<object>` | The approved release: `{os, arch, path, sha256}` per platform, canonically ordered. **A release, not a hash** — one `plugin.allow` confirms every platform variant under one signature (NIM-793). A `kind: git` grant carries exactly one row with empty `os`/`arch`/`path`: that source declares no platform, so its single binary answers for every one. |

Errors: `plugin-not-in-cache` (the plugin is not in the host's cache), `sigil-already-active` (there is already an active permission for `(source, ref)`, or the alias is taken), `validation-failed` (malformed alias/source/ref, or a `source` that is not where the release was fetched from). Audit: `plugin.allowed`, payload `{alias, source, ref, kind, artifact_sha256[], allowed_by_aid}`.

#### `keeper.plugin.revoke`

Revocation of an active clearance from `plugin_sigils` by its registration alias (the binary no longer passes Sigil verification). Permission: `plugin.revoke`. Endpoint: [`DELETE /v1/plugins/sigils/{alias}`](../operator-api/plugins.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `alias` | `string` | yes | The registration to un-register. It identifies exactly one live grant. |

**Output:** empty object (REST equivalent - 204 No Content).

Errors: `sigil-not-found` (no active record), `validation-failed` (malformed alias). Audit: `plugin.revoked`.

#### `keeper.plugin.list`

Listing the active (not revoked) entries of the allow-list `plugin_sigils`, new ones first. Without `signature`/`manifest` (crypto stuff/large JSONB). Permission: `plugin.list`. Endpoint: [`GET /v1/plugins/sigils`](../operator-api/plugins.md). Async: no.

**Input:** empty object.

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `sigils` | `array<SigilView>` | Items - `{alias, source, ref, kind, artifacts[], allowed_by_aid, allowed_at, revoked_at}`. `artifacts` is the WHOLE approved release, not this Keeper's platform: an operator auditing the allow-list has to see every digest the approval covers. |
