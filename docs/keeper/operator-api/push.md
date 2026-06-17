# Push — endpoints push-режима (SSH-доставка без агента)

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/push/*` (push-прогон Destiny по SSH через `keeper.push`, [ADR-004](../../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). Полная модель push-режима (orchestrator, cleanup, SshProvider) — [push.md](../push.md). MCP-сторона — [mcp-tools/push.md](../mcp-tools/push.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 2 роутов) — в корневом [operator-api.md → Push (2)](../operator-api.md#push-2--pushmd).

| Метод / Path | Permission | MCP-tool |
|---|---|---|
| `POST /v1/push/apply` | `push.apply` | [`keeper.push.apply`](../mcp-tools/push.md#keeperpushapply) |
| `GET /v1/push/{apply_id}` | `push.read` | — (REST polling) |

> **NB.** MCP-tool `keeper.push.cleanup` существует в manifest, но REST-роута `/v1/push/cleanup` **нет** — декларация удалена из спеки 2026-06-10 как мёртвая (в `router.go` роут никогда не монтировался). Cleanup в push-режиме делается в той же SSH-сессии флагом `cleanup_stale_versions` запроса `POST /v1/push/apply` (см. ниже) — отдельного REST-эндпоинта чистки нет. Полная модель cleanup — [push.md → Cleanup](../push.md#cleanup-на-хосте).

### `POST /v1/push/apply` — push-прогон Destiny по SSH

Permission: `push.apply`. MCP-tool: `keeper.push.apply`. Полное описание модели — [push.md](../push.md).

**Request `PushApplyRequest`:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `inventory` | `list<string>` | yes | Список SID (FQDN) target-хостов. Хосты должны существовать в реестре `souls` с `transport: ssh`. |
| `destiny` | `string` (git-ref form `<name>@<ref>`) | yes | Ссылка на Destiny — `<name>` из `default_destiny_source` ([config.md](../config.md#services--default_destiny_source--default_module_source)) + git-tag/branch. |
| `input` | `object` | optional | Input для destiny (см. [destiny/input](../../input.md)). |
| `ssh_provider` | `string` | optional | Имя SshProvider из `keeper.yml → plugins.ssh_providers[].name` ([push.md → Аутентификация](../push.md#аутентификация-ssh--pluggable-provider)). По умолчанию — первый зарегистрированный. |
| `cleanup_stale_versions` | `bool` | optional | Удалить устаревшие версии `soul`-бинаря/модулей в той же SSH-сессии ([push.md → Cleanup](../push.md#cleanup-на-хосте)). Default `false`. |

```json
{
  "inventory": ["redis-push-01.example.com", "redis-push-02.example.com"],
  "destiny": "redis-base@v1.4.0",
  "input": { "redis_password": "vault:secret/redis/prod#password" },
  "ssh_provider": "vault-ssh"
}
```

**Response `202 Accepted`:**

```json
{ "apply_id": "01HABCDEFGHJKMNPQRSTVWXYZ" }
```

**Errors:** `403 forbidden` (RBAC может фильтровать по `coven=` инвентори-хостов), `404 not-found` (SID отсутствует в реестре), `422 validation-failed`.

### `GET /v1/push/{apply_id}` — состояние push-прогона

Permission: `push.read`. MCP-tool: пока без отдельного tool-а (опрос ведётся через REST). Полная модель — [push.md](../push.md). Read-endpoint Variant C orchestrator-а: возвращает текущее состояние записи `push_runs` (in-flight либо terminal).

**Response `200 OK` `PushApplyView`:**

| Поле | Тип | Смысл |
|---|---|---|
| `apply_id` | `string` (ULID) | Идентификатор прогона. |
| `inventory_sids` | `list<string>` | Список SID, переданных в `POST /v1/push/apply`. |
| `destiny_ref` | `string` | `<name>@<ref>` запроса. |
| `ssh_provider` | `string` | Имя SshProvider (опционально); пусто = registry-default. |
| `input` | `object` | Input destiny (как пришёл в запросе). |
| `cleanup_stale` | `bool` | Флаг `cleanup_stale_versions` запроса. |
| `status` | `string` (enum) | `pending` / `running` / `success` / `partial_failed` / `failed` / `cancelled`. |
| `started_at` | `string` (RFC3339) | Время приёма запроса. |
| `finished_at` | `string` (RFC3339), opt | Время финализации (отсутствует пока `pending`/`running`). |
| `started_by_aid` | `string` (AID), opt | Архонт-инициатор. |
| `summary` | `object`, opt | Per-host исходы прогона (см. ниже). |

`summary` (присутствует ТОЛЬКО для терминальных статусов либо при `cancelled`):

```json
{
  "hosts": [
    {"sid": "redis-push-01.example.com", "status": "success"},
    {"sid": "redis-push-02.example.com", "status": "failed", "error": "run_status=failed"}
  ],
  "total": 2,
  "success_count": 1,
  "fail_count": 1
}
```

Для `cancelled` (Reaper purge_orphan_push_runs) — дополнительно `summary.orphan_purged: true` + `summary.reason`.

**Errors:** `404 not-found` (apply_id отсутствует в реестре `push_runs`), `422 validation-failed` (битый apply_id).
