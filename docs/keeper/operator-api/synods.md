# Synod — endpoints управления группами архонов

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/synods*` (Synod-группы — бандлят роли по модели **Архон → Synod → Роли**, [ADR-049](../../adr/0049-synod.md#adr-049-synod--группа-архонов)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/synods.md](../mcp-tools/synods.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 8 роутов + ремарка про `PATCH /v1/synods/{name}` правку только `description`, NoSelector) — в корневом [operator-api.md → Synod (8)](../operator-api.md#synod-8--управление-группами-архонов-adr-049).

**Источник правды по семантике, телам и кодам ошибок** CRUD `/v1/synods*` (`synod-already-exists`, `synod-not-found`, `synod-builtin`, `would-lock-out-cluster`) — [rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods) (бизнес-инварианты builtin-границы, self-lockout, least-privilege subset живут в `rbac.Service`). Эффективные права инициатора при subset-check = прямые ∪ роли через его Synod-ы.

Сводно по 8 роутам:

| Метод / Path | Permission | MCP-tool | Семантика |
|---|---|---|---|
| `POST /v1/synods` | `synod.create` | [`keeper.synod.create`](../mcp-tools/synods.md#keepersynodcreate) | Создать группу (пустую — роли добавляются grant-role). |
| `GET /v1/synods` | `synod.list` | [`keeper.synod.list`](../mcp-tools/synods.md#keepersynodlist) | Список групп с развёрнутыми ролями и членами-AID. |
| `PATCH /v1/synods/{name}` | `synod.update` | [`keeper.synod.update`](../mcp-tools/synods.md#keepersynodupdate) | Правка **ТОЛЬКО `description`** (ADR-049 amend); `name` (PK) immutable; builtin разрешён. |
| `DELETE /v1/synods/{name}` | `synod.delete` | [`keeper.synod.delete`](../mcp-tools/synods.md#keepersynoddelete) | Удалить группу (каскадом membership + bundle). |
| `POST /v1/synods/{name}/operators` | `synod.add-operator` | [`keeper.synod.add-operator`](../mcp-tools/synods.md#keepersynodadd-operator) | Добавить архона в группу. Идемпотентно. |
| `DELETE /v1/synods/{name}/operators/{aid}` | `synod.remove-operator` | [`keeper.synod.remove-operator`](../mcp-tools/synods.md#keepersynodremove-operator) | Снять архона из группы. |
| `POST /v1/synods/{name}/roles` | `synod.grant-role` | [`keeper.synod.grant-role`](../mcp-tools/synods.md#keepersynodgrant-role) | Добавить роль в bundle (всем членам). Идемпотентно. |
| `DELETE /v1/synods/{name}/roles/{role_name}` | `synod.revoke-role` | [`keeper.synod.revoke-role`](../mcp-tools/synods.md#keepersynodrevoke-role) | Снять роль из bundle (у всех членов). |

`synod.*` — NoSelector. `PATCH /v1/synods/{name}` (ADR-049 amend) меняет **ТОЛЬКО `description`** (body `{description}`, required, 1..1024 символов); `name` (PK) immutable. Коды: `204` (успех), `404 synod-not-found`, `422 validation-failed` (пустой/превышение `description`), `400 malformed-request` (битый JSON / неизвестное поле, в т.ч. `name` в теле). builtin-группа редактируется (`description` косметика, без subset/self-lockout). Audit-event `synod.updated`.
