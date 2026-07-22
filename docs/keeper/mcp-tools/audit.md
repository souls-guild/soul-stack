# Audit - MCP-tools read-only tapes audit-events

Domain section [MCP-tools directory](../mcp-tools.md): reading tape `audit_log` ([ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). Transport, auth, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api/audit.md](../operator-api/audit.md).

### Audit (0)

There is no MCP-tool for the audit-events tape **in MVP**. REST endpoint `GET /v1/audit` (permission `audit.read`) implemented for UI iteration 2 (placeholder `/audit`); MCP symmetry is postponed until the next slice - added by one PR with the extension [rbac.md → Permissions directory](../rbac.md) + [operator-api/audit.md](../operator-api/audit.md) + of this file (pattern [§ Future reads and deletions](../mcp-tools.md)).

Semantics, query parameters and response form - [operator-api/audit.md → `GET /v1/audit`](../operator-api/audit.md).
