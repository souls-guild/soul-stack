# Synod — MCP-tools управления группами архонов

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.synod.*` (Synod-группы, бандлящие роли). Транспорт, auth, формат tool declaration, async-convention, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api.md → Synod](../operator-api/synods.md) (тела и инварианты — [rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)).

### Synod (8)

Управление **Synod-группами** (группы архонов, бандлящие роли — модель **Архон → Synod → Роли**) через MCP — [ADR-049](../../adr/0049-synod.md#adr-049-synod--группа-архонов), storage в Postgres (`synods` / `synod_operators` / `synod_roles`, [rbac.md → Управление группами архонов](../rbac.md#управление-группами-архонов-synod)). Permission ↔ tool — 1:1: `keeper.synod.<action>` ↔ `synod.<action>`. Бизнес-инварианты (builtin-граница, self-lockout, least-privilege subset) живут в `rbac.Service`; tool — транспорт. Эффективные права инициатора при subset-check = прямые ∪ роли через его Synod-ы.

#### `keeper.synod.create`

Создание Synod-группы (пустой — роли добавляются через `keeper.synod.grant-role`). Permission: `synod.create`. Endpoint: `POST /v1/synods` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Имя группы (kebab-case). |
| `description` | `string` | optional | Человекочитаемое описание группы. |

**Output:** пустой объект (`{}`). Соответствует HTTP `201 Created`.

Ошибки: `synod-already-exists` (`name` занят), `validation-failed` (битый `name`). Least-privilege/self-lockout к create неприменимы — пустая группа прав не выдаёт.

#### `keeper.synod.delete`

Удаление группы (каскадом membership + bundle). Permission: `synod.delete`. Endpoint: `DELETE /v1/synods/{name}` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя группы. |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `synod-not-found`, `synod-builtin` (builtin-группу удалять нельзя, проверка **до** self-lockout), `would-lock-out-cluster` (исчезновение группы снимет последний эффективный `*`).

#### `keeper.synod.list`

Перечисление групп с развёрнутыми ролями (bundle) и членами-AID. Permission: `synod.list`. Endpoint: `GET /v1/synods` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: нет.

**Input:** пустой объект (`{}`).

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `synods` | `array<object>` | Элементы — `{name, description, builtin, roles[], operators[]}`; `roles` / `operators` — non-nil массивы (группа без записей → `[]`), отсортированы детерминированно. |

#### `keeper.synod.update`

Правка **ТОЛЬКО `description`** группы (ADR-049 amend). Permission: `synod.update`. Endpoint: `PATCH /v1/synods/{name}` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: нет. **`name` (PK) immutable** — адресует группу из path, не переименовывается. **builtin РАЗРЕШЁН** к правке (`description` — косметика для UI/аудита, не поведение). **Без subset-check и self-lockout** (`description` прав не выдаёт и не отнимает); снимок enforcer-а не инвалидируется.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Имя группы (PK, не меняется — адресует строку). |
| `description` | `string` (1..1024 символов) | yes | Новое описание (полностью заменяет старое). |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `synod-not-found` (группы нет), `validation-failed` (пустой `description` либо превышение лимита 1024). Audit: `synod.updated`.

#### `keeper.synod.add-operator`

Добавление архона (AID) в группу. Идемпотентно. Permission: `synod.add-operator`. Endpoint: `POST /v1/synods/{name}/operators` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `synod` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Имя группы. |
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID добавляемого Архонта. |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `synod-not-found`, `not-found` (AID не существует), `forbidden` (**least-privilege subset**: член получает весь bundle группы — caller обязан держать все его эффективные права). Self-lockout **нет** — add только расширяет admin-set.

#### `keeper.synod.remove-operator`

Снятие архона (AID) из группы. Permission: `synod.remove-operator`. Endpoint: `DELETE /v1/synods/{name}/operators/{aid}` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `synod` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Имя группы. |
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID снимаемого Архонта. |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `not-found` (пары `(synod, aid)` нет), `would-lock-out-cluster` (**self-lockout**: снятие осиротит последнего админа, чей `*` держится только через эту группу).

#### `keeper.synod.grant-role`

Добавление роли в bundle группы (выдаёт её всем членам). Идемпотентно. Permission: `synod.grant-role`. Endpoint: `POST /v1/synods/{name}/roles` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `synod` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Имя группы. |
| `role` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Имя добавляемой роли. |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `synod-not-found`, `role-not-found` (роли нет), `forbidden` (**least-privilege subset**: роль выдаётся всем членам — caller обязан держать все её эффективные права). Self-lockout **нет**.

#### `keeper.synod.revoke-role`

Снятие роли из bundle группы (у всех членов). Permission: `synod.revoke-role`. Endpoint: `DELETE /v1/synods/{name}/roles/{role_name}` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `synod` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Имя группы. |
| `role` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Имя снимаемой роли. |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

Ошибки: `not-found` (bundle-пары `(synod, role)` нет), `would-lock-out-cluster` (**self-lockout**: снимаемая роль — последняя `*`-дающая роль группы, член держал `*` только через неё).
