# Operator — MCP-tools управления Архонтами

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.operator.*` (создание / отзыв / выпуск JWT Архонтов). Транспорт, auth, формат tool declaration, async-convention, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api.md → Operator](../operator-api/operator.md).

### Operator (3)

#### `keeper.operator.create`

Создание нового Архонта. Permission: `operator.create`. Endpoint: [`POST /v1/operators`](../operator-api/operator.md#post-v1operators--создать-архонта). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID нового Архонта. |
| `display_name` | `string` | yes | Человекочитаемое имя. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `aid` | `string` | AID созданного Архонта. |
| `display_name` | `string` | Зеркало input. |
| `created_at` | `string` (RFC 3339) | Время создания. |
| `created_by_aid` | `string` | AID Архонта, выполнившего вызов. |
| `jwt` | `string` | Выпущенный JWT, **отдаётся один раз**. |

#### `keeper.operator.revoke`

Отзыв Архонта. Permission: `operator.revoke`. Endpoint: [`POST /v1/operators/{aid}/revoke`](../operator-api/operator.md#post-v1operatorsaidrevoke--отозвать-архонта). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `aid` | `string` | yes | AID Архонта, отзываемого. |
| `reason` | `string` | no | Свободный текст причины для audit-trail (фиксируется в `payload.reason` audit-event `operator.revoked`, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). |

**Output:** пустой объект (`{}`). Соответствует HTTP `204 No Content`.

#### `keeper.operator.issue-token`

Выпуск нового JWT для существующего Архонта. Permission: `operator.issue-token`. Endpoint: [`POST /v1/operators/{aid}/issue-token`](../operator-api/operator.md#post-v1operatorsaidissue-token--выпустить-новый-jwt). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `aid` | `string` | yes | AID. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `aid` | `string` | AID. |
| `jwt` | `string` | Новый JWT. |
| `expires_at` | `string` (RFC 3339) | Срок истечения. Не путать с JWT-claim `exp` (unix-сек) внутри самого токена. |
