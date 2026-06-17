# Role — MCP-tools RBAC-CRUD

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.role.*` (роли / permissions / membership Архонтов). Транспорт, auth, формат tool declaration, async-convention, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api.md → Role](../operator-api/roles.md) (тела и инварианты — [rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)).

### Role (6)

Управление RBAC (роли, permissions, membership) через MCP — [ADR-028](../../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres), storage в Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`, [rbac.md → Storage](../rbac.md#storage--три-pg-таблицы)). Permission ↔ tool — 1:1: `keeper.role.<action>` ↔ `role.<action>`. Бизнес-инварианты (builtin-граница, self-lockout) живут в `rbac.Service`; tool — транспорт ([rbac.md → Управление ролями](../rbac.md#управление-ролями-rest--mcp)).

#### `keeper.role.create`

Создание роли + её permissions. Permission: `role.create`. Endpoint: `POST /v1/roles` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Имя роли (kebab-case). |
| `description` | `string` | optional | Человекочитаемое описание роли. |
| `permissions` | `array<string>` | yes | Permission-строки `<resource>.<action>` (+ опц. ` on <selector>`), [rbac.md → Формат permissions](../rbac.md#формат-permissions). |

**Output:** пустой объект (`{}`). Соответствует HTTP `201 Created`.

Ошибки: `role-already-exists` (`name` занят), `validation-failed` (битый `name` или `permission`).

#### `keeper.role.delete`

Удаление роли (каскадом permissions + membership). Permission: `role.delete`. Endpoint: `DELETE /v1/roles/{name}` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя роли. |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `role-not-found`, `role-builtin` (builtin-роль удалять нельзя), `would-lock-out-cluster` (удаление снимет последний эффективный `*`).

#### `keeper.role.list`

Перечисление ролей с развёрнутыми permissions и назначенными Архонтами. Permission: `role.list`. Endpoint: `GET /v1/roles` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: нет.

**Input:** пустой объект (`{}`).

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `roles` | `array<object>` | Элементы — `{name, description, builtin, permissions[], operators[]}`; `permissions` / `operators` — non-nil массивы (роль без записей → `[]`). |

#### `keeper.role.update`

Замена набора permissions роли (replace-семантика). Permission: `role.update`. Endpoint: `PATCH /v1/roles/{name}/permissions` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя роли. |
| `permissions` | `array<string>` | yes | Новый набор permissions (полностью заменяет существующий). |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `role-not-found`, `role-builtin`, `would-lock-out-cluster` (снятие последнего `*`), `validation-failed` (битый `permission`).

#### `keeper.role.grant-operator`

Привязка Архонта (AID) к роли — добавление membership-строки `(role, aid)`. Идемпотентно. Permission: `role.grant-operator`. Endpoint: `POST /v1/roles/{name}/operators` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `role` | `string` | yes | Имя роли. |
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID привязываемого Архонта. |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `not-found` (роль или AID не существуют). Self-lockout-проверки **нет**: grant только расширяет admin-set, кластер запереть не может. Над builtin-ролью разрешено.

#### `keeper.role.revoke-operator`

Снятие membership-строки `(role, aid)`. Permission: `role.revoke-operator`. Endpoint: `DELETE /v1/roles/{name}/operators/{aid}` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `role` | `string` | yes | Имя роли. |
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID снимаемого Архонта. |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `not-found` (пары `(role, aid)` нет), `would-lock-out-cluster` (снятие последнего эффективного `*`). Над builtin-ролью разрешено.
