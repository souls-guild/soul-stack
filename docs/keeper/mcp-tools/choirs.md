# Choir - MCP-tools named host topology

Domain section [MCP-tools directory](../mcp-tools.md): Choir domain (`/v1/incarnations/{name}/choirs*`, Choir/Voice topology within incarnation, [ADR-044](../../adr/0044-choir.md)). The source of truth for semantics is [operator-api/choirs.md](../operator-api/choirs.md).

### Choir (0)

Choir does not have **MCP tools** - it is a REST-only domain. There is not a single `keeper.choir.*`-tool in the `keeper/internal/mcp/manifest.go` directory: the topology of hosts inside the incarnation (Choir/Voice) is controlled via the Operator API (`POST/GET/DELETE /v1/incarnations/{name}/choirs*` + voices). MCP symmetry for the Choir domain is not implemented and is not included in the MVP catalog of 72 tools; will appear as a separate PR if necessary (directory extension - only-add, [mcp-tools.md → Future readings and deletions](../mcp-tools.md)).

> **Remark about drift in OpenAPI descriptions.** Text `description` choir operations in [`openapi.yaml`](../openapi.yaml) mention `MCP-tool: keeper.choir.create` / `keeper.choir.delete` / `keeper.choir.add-voice` / `keeper.choir.remove-voice` - these tools in `manifest.go` actually does not exist (REST-only domain). The source of truth for the presence of the tool is `manifest.go`; the descriptions in the spec are outdated and must be edited in a separate task (not included in this mapping inventory).
