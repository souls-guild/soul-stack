# Errand — MCP-tools pull-ad-hoc exec вне scenario

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.soul.errand.run` / `keeper.errand.*` (ad-hoc exec одиночного модуля на Soul, [ADR-033](../../adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)). Транспорт, auth, формат tool declaration, async-convention, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api.md → Errand](../operator-api/errands.md).

### Errand (4)

Pull-ad-hoc exec одиночного модуля на Soul через mTLS EventStream ([ADR-033](../../adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)). 1:1 с REST `POST /v1/souls/{sid}/exec` + `GET /v1/errands{,/{errand_id}}` + `DELETE /v1/errands/{errand_id}`. Бизнес-логика (validate, INSERT errands-row, send/wait, mask+cap stdout/stderr, async-escalation, cancel-signal, audit) — в `errand.Dispatcher` / `Store`; tool — транспорт. Доступны только при поднятом errand-стеке; иначе `internal-error` («errand orchestrator is not configured»).

#### `keeper.soul.errand.run`

Запуск Errand на конкретном Soul. Permission: `errand.run`, селектор `host=<sid>` ([rbac.md §Errand](../rbac.md)). Endpoint: [`POST /v1/souls/{sid}/exec`](../operator-api/errands.md#post-v1soulssidexec--запуск-errand-на-хосте). Async: **возможен** (server-cap 30s; при превышении `async=true` со `status=running`, дальше poll через `keeper.errand.get`).

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `sid` | `string` (regex SID) | yes | FQDN целевого Soul. |
| `module` | `string` | yes | Адрес модуля `core.<class>.<state>` либо `core.cmd.shell` / `core.exec.run` (whitelist на Soul-side). |
| `input` | `object` | optional | Input модуля (форма зависит от модуля). |
| `timeout_seconds` | `integer` (1..300) | optional | Полный timeout. Default 30. |
| `dry_run` | `boolean` | optional | `true` → Soul зовёт `mod.Plan` (только read-safe модули). |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `errand_id` | `string` (ULID) | ID запуска. |
| `sid`, `module` | `string` | Зеркало input. |
| `status` | `string` | `running` / `success` / `failed` / `timed_out` / `module_not_allowed`. |
| `async` | `boolean` | `true` → server-cap превышен, дожимай через `keeper.errand.get`. |
| `exit_code` | `integer` | Exit-код verb-модуля (NULL для read-safe non-shell). |
| `stdout`, `stderr` | `string` | Маскированный вывод (cap 64 KiB). |
| `stdout_truncated`, `stderr_truncated` | `boolean` | Превышение cap. |
| `duration_ms` | `integer` | Длительность Errand-а на Soul-side. |
| `error_message` | `string` | Маскированная причина FAILED/TIMED_OUT/MODULE_NOT_ALLOWED. |
| `output` | `object` | Структурный output read-safe модулей; для shell/exec отсутствует. |

Ошибки: `not-found` (Soul не подключён к кластеру), `validation-failed` (пустой sid/module, `timeout_seconds` вне [1, 300]).

#### `keeper.errand.list`

Перечисление Errand-ов с фильтрацией и pagination. Permission: `errand.list`. Endpoint: [`GET /v1/errands`](../operator-api/errands.md#get-v1errandserrand_id--get-v1errands--чтение-errand-ов). Async: нет. Read-only.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `sid` | `string` (regex SID) | optional | Фильтр по целевому Soul. |
| `status` | `string` (enum статусов) | optional | Фильтр по статусу. |
| `started_after` | `string` (RFC 3339) | optional | Фильтр по `started_at > value`. |
| `offset` | `integer` (≥0) | optional | Pagination. |
| `limit` | `integer` (1..1000) | optional | Default 50. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `items` | `array<object>` | Список строк (форма — как у `keeper.errand.get`). |
| `offset`, `limit`, `total` | `integer` | Pagination. |

#### `keeper.errand.get`

Состояние Errand-а по ULID. Permission: `errand.list` (read-permission покрывает и list, и get). Endpoint: [`GET /v1/errands/{errand_id}`](../operator-api/errands.md#get-v1errandserrand_id--get-v1errands--чтение-errand-ов). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `errand_id` | `string` (ULID) | yes | ID запуска. |

**Output:** см. поля `keeper.soul.errand.run` (без `async`); добавляется `started_by_aid`, `started_at`, `finished_at` (только для терминалов).

Ошибки: `not-found` (errand_id не существует).

#### `keeper.errand.cancel`

Отменяет in-flight Errand (slice E5). Permission: `errand.cancel`. Endpoint: [`DELETE /v1/errands/{errand_id}`](../operator-api/errands.md#delete-v1errandserrand_id--отмена-in-flight-errand). Async: нет. Best-effort: Keeper отправляет `CancelErrand` Soul-у через EventStream, Soul-side `errandrunner` отменяет ctx → возвращает `ErrandResult{CANCELLED}`. Финальный статус оператор смотрит через `keeper.errand.get` (poll).

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `errand_id` | `string` (ULID) | yes | ID запуска. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `errand_id` | `string` | Эхо входа. |
| `cancelled` | `boolean` | `true` — cancel-сигнал отправлен. |

Ошибки: `not-found` (errand_id не существует ИЛИ Soul не подключён), `errand-not-cancellable` (Errand уже в терминальном статусе — нечего отменять).
