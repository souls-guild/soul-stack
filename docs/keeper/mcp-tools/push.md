# Push — MCP-tools push-режима (SSH-доставка без агента)

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.push.*` (push-прогон Destiny по SSH через `keeper.push`, [ADR-004](../../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)). Транспорт, auth, формат tool declaration, async-convention, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api/push.md](../operator-api/push.md). Полная модель push-режима — [push.md](../push.md).

### Push (2)

#### `keeper.push.apply`

Push-прогон Destiny по SSH. Permission: `push.apply`. Endpoint: [`POST /v1/push/apply`](../operator-api/push.md#post-v1pushapply--push-прогон-destiny-по-ssh). Async: **да**.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `inventory` | `array<string>` (SID) | yes | Target-хосты. |
| `destiny` | `string` (`<name>@<ref>`) | yes | Destiny + git-ref. |
| `input` | `object` | optional | Input destiny. |
| `ssh_provider` | `string` | optional | Имя SshProvider из `keeper.yml`. |
| `cleanup_stale_versions` | `boolean` | optional | Default `false`. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID запуска. |

#### `keeper.push.cleanup`

Чистка `/var/lib/soul-stack/` на хосте. Permission: `push.cleanup`. **Tool без REST-роута** — REST-декларация `/v1/push/cleanup` удалена из спеки 2026-06-10 как мёртвая (в `router.go` роут не монтировался); MCP-tool остаётся в manifest как stub-обвязка. Парный REST-эндпоинт отсутствует; cleanup в push-режиме делается флагом `cleanup_stale_versions` запроса [`POST /v1/push/apply`](../operator-api/push.md#post-v1pushapply--push-прогон-destiny-по-ssh). Async: **да**.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `inventory` | `array<string>` (SID) | yes | Хосты для cleanup. |
| `ssh_provider` | `string` | optional | По аналогии с `push.apply`. |
| `full` | `boolean` | optional | `true` — стереть `/var/lib/soul-stack/` целиком; `false` (default) — только устаревшие версии. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID запуска. |
