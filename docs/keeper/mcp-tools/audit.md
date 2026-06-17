# Audit — MCP-tools read-only-ленты audit-events

Доменная секция [каталога MCP-tools](../mcp-tools.md): чтение ленты `audit_log` ([ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). Транспорт, auth, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api/audit.md](../operator-api/audit.md).

### Audit (0)

MCP-tool-а для ленты audit-events **в MVP нет**. REST-эндпоинт `GET /v1/audit` (permission `audit.read`) реализован для UI iteration 2 (placeholder `/audit`); MCP-симметрия отложена до следующего slice — добавится одним PR с расширением [rbac.md → Каталог permissions](../rbac.md#каталог-permissions) + [operator-api/audit.md](../operator-api/audit.md) + этого файла (паттерн [§ Будущие чтения и удаления](../mcp-tools.md#будущие-чтения-и-удаления)).

Семантика, query-параметры и форма ответа — [operator-api/audit.md → `GET /v1/audit`](../operator-api/audit.md#get-v1audit--лента-audit-events).
