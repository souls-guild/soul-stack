# Plugin — endpoints Sigil allow-list plugin integrity

Domain section [Operator API](../operator-api.md): endpoints `/v1/plugins/sigils*` (admission/revocation/list of allow-list entries `plugin_sigils` - the plugin binary passes Sigil verification only with active admission, [ADR-026](../../adr/0026-sigil.md) S4a). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). The source of truth for semantics, bodies and `sha256`-computation is [plugins.md → Integrity-model](../plugins.md#integrity-model). MCP side - [mcp-tools/plugins.md](../mcp-tools/plugins.md). Rotation of the **signature** keys themselves (separate zone) - [operator-api/sigils.md](sigils.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 3 routes) - in the root [operator-api.md → Plugin (3)](../operator-api.md). A separate area from Sigil-key: that one is about the **signature keys**, this one is about the permissions of the binaries themselves. `plugin.*` - NoSelector.

| Method/Path | Permission | MCP-tool |
|---|---|---|
| `POST /v1/plugins/sigils` | `plugin.allow` | [`keeper.plugin.allow`](../mcp-tools/plugins.md#keeperpluginallow) |
| `GET /v1/plugins/sigils` | `plugin.list` | [`keeper.plugin.list`](../mcp-tools/plugins.md#keeperpluginlist) |
| `DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}` | `plugin.revoke` | [`keeper.plugin.revoke`](../mcp-tools/plugins.md#keeperpluginrevoke) |

Full request/response/output-schemes and error codes (`plugin-not-in-cache` / `sigil-already-active` / `sigil-not-found` / `validation-failed`) - on the MCP side [mcp-tools/plugins.md → Plugin (3)](../mcp-tools/plugins.md#plugin-3) (forms 1:1 with HTTP bodies). Mutating 2 routes (`allow`/`revoke`) are audited (supply-chain-mutations, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `plugin.list` - read-only, no audit. Routes are mounted only when Sigil is configured (`keeper.yml → sigil.signing_key_ref`); when Sigil is turned off, the block `/v1/plugins/sigils*` **is not mounted** → the request is caught by catch-all → `404 not-found` (`router.go`: `if sigilH != nil`).
