# Role — endpoints RBAC-CRUD

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/roles*` (роли / permissions / membership Архонтов, [ADR-013](../../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) / [ADR-028](../../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/roles.md](../mcp-tools/roles.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 6 роутов + ремарка про replace-семантику `PATCH .../permissions`, NoSelector, audit) — в корневом [operator-api.md → Role (6)](../operator-api.md#role-6--rbac-crud-роли--permissions--membership-adr-013--rbacmd).

**Источник правды по семантике, телам и кодам ошибок** REST `/v1/roles*` (`role-already-exists`, `role-builtin`, `would-lock-out-cluster`) — [rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles) (бизнес-инварианты builtin-границы и self-lockout живут в `rbac.Service`). Этот файл — доменный якорь Operator API; детали тел не дублируются.

Сводно по 6 роутам:

| Метод / Path | Permission | MCP-tool | Семантика |
|---|---|---|---|
| `POST /v1/roles` | `role.create` | [`keeper.role.create`](../mcp-tools/roles.md#keeperrolecreate) | Создать роль + её permissions. |
| `GET /v1/roles` | `role.list` | [`keeper.role.list`](../mcp-tools/roles.md#keeperrolelist) | Список ролей с развёрнутыми permissions/operators. Read-only, без audit. |
| `DELETE /v1/roles/{name}` | `role.delete` | [`keeper.role.delete`](../mcp-tools/roles.md#keeperroledelete) | Удалить роль (каскадом permissions + membership). |
| `PATCH /v1/roles/{name}/permissions` | `role.update` | [`keeper.role.update`](../mcp-tools/roles.md#keeperroleupdate) | **Replace** набора permissions роли (не merge). |
| `POST /v1/roles/{name}/operators` | `role.grant-operator` | [`keeper.role.grant-operator`](../mcp-tools/roles.md#keeperrolegrant-operator) | Привязать Архонта (AID) к роли. Идемпотентно. |
| `DELETE /v1/roles/{name}/operators/{aid}` | `role.revoke-operator` | [`keeper.role.revoke-operator`](../mcp-tools/roles.md#keeperrolerevoke-operator) | Снять membership-строку `(role, aid)`. |

`role.*` — NoSelector (cluster-уровневая операция без coven/host-scope, как `operator.*` / `synod.*`). Мутирующие 5 роутов аудируются (изменение авторизации, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `role.list` — read-only, без audit.
