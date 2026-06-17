# Audit — endpoint read-only-ленты audit-events

Доменная секция [Operator API](../operator-api.md): эндпоинт `GET /v1/audit` (чтение ленты `audit_log`, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/audit.md](../mcp-tools/audit.md) (MCP-tool-симметрия отложена).

## Endpoint-секции

Mapping endpoint ↔ permission (таблица 1 роута) — в корневом [operator-api.md → Audit (1)](../operator-api.md#audit-1--read-only-лента-audit-events-adr-022).

### `GET /v1/audit` — лента audit-events

Permission: `audit.read`. Read-only, **БЕЗ audit-trail на читателя** (избегаем рекурсии — каждый GET удваивал бы таблицу). MCP-tool — нет (UI iteration 2, placeholder `/audit`).

**Query-параметры:**

| Поле | Тип | Обязательность | Смысл |
|---|---|---|---|
| `type` | `string[]` (multi-value) | optional | `?type=X&type=Y` — exact-match OR (`event_type IN (X,Y)`). |
| `source` | `string[]` (multi-value, signal/api/mcp/keeper_internal/soul_grpc/background) | optional | `?source=api&source=mcp` — exact-match OR. |
| `archon_aid` | `string` | optional | AID инициатора (exact match). |
| `correlation_id` | `string` (ULID) | optional | Цепочка связанных событий одного flow (exact match). |
| `started_after` | `string` (RFC 3339) | optional | `created_at >= started_after` (включающая). |
| `started_before` | `string` (RFC 3339) | optional | `created_at <= started_before` (включающая). |
| `offset` / `limit` | `int` | optional | Стандартная пагинация ([§ Pagination](../operator-api.md#pagination)). |

**Response `200 AuditEventListReply`:** `{items: AuditEvent[], offset, limit, total}`. Сортировка `created_at DESC`. `payload` — свободный jsonb (форма зависит от `type`); секреты замаскированы на write-path (`audit.MaskSecrets`).
