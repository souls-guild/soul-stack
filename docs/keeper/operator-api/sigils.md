# Sigil-key — endpoints for rotation of Sigil signature keys

Domain section [Operator API](../operator-api.md): endpoints `/v1/sigil/keys*` (rotation of trust-anchor-**signature keys** Sigil, [ADR-026(h)](../../adr/0026-sigil.md), R3-S7; registry `sigil_signing_keys`). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/sigils.md](../mcp-tools/sigils.md). Tolerances of the binaries themselves (allow-list, separate zone) - [operator-api/plugins.md](plugins.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 4 routes) - in the root [operator-api.md → Sigil-key (4)](../operator-api.md). **3-segment MCP-tool `keeper.sigil.key.<verb>` ↔ 2-segment permission `sigil.key-<verb>`** (resource `sigil`, hyphenated action). Selector - NoSelector.

| Method/Path | Permission | MCP-tool |
|---|---|---|
| `POST /v1/sigil/keys` | `sigil.key-introduce` | [`keeper.sigil.key.introduce`](../mcp-tools/sigils.md#keepersigilkeyintroduce) |
| `GET /v1/sigil/keys` | `sigil.key-list` | [`keeper.sigil.key.list`](../mcp-tools/sigils.md#keepersigilkeylist) |
| `POST /v1/sigil/keys/{key_id}/primary` | `sigil.key-set-primary` | [`keeper.sigil.key.set-primary`](../mcp-tools/sigils.md#keepersigilkeyset-primary) |
| `DELETE /v1/sigil/keys/{key_id}` | `sigil.key-retire` | [`keeper.sigil.key.retire`](../mcp-tools/sigils.md#keepersigilkeyretire) |

The subscription private is written to Vault KV (`secret/keeper/sigil-keys/<key_id>`) and is **never** returned to the response/log. Full request/response/output schemes and error codes (`sigil-key-not-found` / `sigil-key-last-active` / `sigil-key-primary` / `sigil-key-concurrent-change`) are on the MCP side [mcp-tools/sigils.md → Sigil-key (4)](../mcp-tools/sigils.md#sigil-key-4) (1:1 forms with HTTP bodies). All four routes are mounted only with Sigil configured (`keeper.yml → sigil.signing_key_ref`); when Sigil is turned off, the block `/v1/sigil/keys*` **is not mounted** → the request is caught by catch-all → `404 not-found` (`router.go`: `if sigilKeyH != nil`). Mutating 3 routes (`introduce`/`set-primary`/`retire`) are audited ([ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `key-list` - read-only, no audit.
