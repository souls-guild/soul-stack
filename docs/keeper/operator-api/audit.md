# Audit — endpoint of the read-only audit-events tape

Domain section [Operator API](../operator-api.md): endpoint `GET /v1/audit` (read tape `audit_log`, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/audit.md](../mcp-tools/audit.md) (MCP-tool symmetry deferred).

## Endpoint sections

Mapping endpoint ↔ permission (route table 1) - in the root [operator-api.md → Audit (1)](../operator-api.md).

### `GET /v1/audit` — audit-events feed

Permission: `audit.read`. Read-only, **WITHOUT audit-trail per reader** (we avoid recursion - each GET would double the table). MCP-tool - no (UI iteration 2, placeholder `/audit`).

**Query parameters:**

| Field | Type | Obligation | Meaning |
|---|---|---|---|
| `type` | `string[]` (multi-value) | optional | `?type=X&type=Y` — exact-match OR (`event_type IN (X,Y)`). |
| `source` | `string[]` (multi-value, signal/api/mcp/keeper_internal/soul_grpc/background) | optional | `?source=api&source=mcp` — exact-match OR. |
| `archon_aid` | `string` | optional | Initiator AID (exact match). |
| `correlation_id` | `string` (ULID) | optional | A chain of related events of one flow (exact match). |
| `started_after` | `string` (RFC 3339) | optional | `created_at >= started_after` (inclusive). |
| `started_before` | `string` (RFC 3339) | optional | `created_at <= started_before` (inclusive). |
| `offset` / `limit` | `int` | optional | Standard pagination ([§ Pagination](../operator-api.md#pagination)). |

**Response `200 AuditEventListReply`:** `{items: AuditEvent[], offset, limit, total}`. Sort `created_at DESC`. `payload` - free jsonb (form depends on `type`); secrets are masked on write-path (`audit.MaskSecrets`).
