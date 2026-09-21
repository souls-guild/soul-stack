# Cadence - MCP-tools for regular launches

Domain section [MCP-tools directory](../mcp-tools.md): Cadence domain (`/v1/cadences*`, regular scheduled Voyage launches, [ADR-046](../../adr/0046-cadence.md)). The source of truth for semantics is [operator-api/cadences.md](../operator-api/cadences.md).

### Cadence (0)

Cadence does not have **MCP tools** - REST-only domain. There is not a single `keeper.cadence.*`-tool in the `keeper/internal/mcp/manifest.go` directory: schedules are managed via the Operator API (`POST/GET/PATCH/DELETE /v1/cadences*` + `enable`/`disable` + `runs`). If an LLM operator needs a periodic run, he creates Voyage directly ([mcp-tools/voyages.md](voyages.md)) or contacts a human operator for the Cadence establishment via UI/REST.

MCP symmetry for the Cadence domain is not implemented and is not included in the MVP catalog of 72 tools; will appear as a separate PR if necessary (directory extension - only-add, [mcp-tools.md → Future readings and deletions](../mcp-tools.md)).
