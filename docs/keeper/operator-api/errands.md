# Errand — endpoints pull-ad-hoc exec вне scenario

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/souls/{sid}/exec` + `/v1/errands*` (ad-hoc exec одиночного модуля на Soul, [ADR-033](../../adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/errands.md](../mcp-tools/errands.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 4 роутов) — в корневом [operator-api.md → Errand (4)](../operator-api.md#errand-4--pull-ad-hoc-exec-вне-scenario-adr-033).

| Метод / Path | Permission | MCP-tool |
|---|---|---|
| `POST /v1/souls/{sid}/exec` | `errand.run` (selector `host=<sid>`) | [`keeper.soul.errand.run`](../mcp-tools/errands.md#keepersoulerrandrun) |
| `GET /v1/errands/{errand_id}` | `errand.list` | — (REST polling) |
| `GET /v1/errands` | `errand.list` | [`keeper.errand.list`](../mcp-tools/errands.md#keepererrandlist) |
| `DELETE /v1/errands/{errand_id}` | `errand.cancel` | [`keeper.errand.cancel`](../mcp-tools/errands.md#keepererrandcancel) |

#### `POST /v1/souls/{sid}/exec` — запуск Errand на хосте

Permission: `errand.run` (селектор `host=<sid>`, [rbac.md §Errand](../rbac.md)). MCP-tool: `keeper.soul.errand.run`. Path-param: `sid` (FQDN, URL-encoded).

Pull-ad-hoc exec одиночного модуля на конкретном Soul через mTLS EventStream. Errand **НЕ мутирует** `incarnation.state` — это отдельный реестр `errands`. Whitelist модулей — Soul-side defense-in-depth: жёсткий список `core.cmd.shell` / `core.exec.run` либо marker-интерфейс `ErrandReadSafe` в `sdk/module/`.

**Sync-первичный flow (server-cap 30s):** `200` + `ErrandResult`, если терминал получен до cap; иначе `202` + `{errand_id}` + `Location: /v1/errands/{errand_id}`, продолжение в background до `timeout_seconds` (max 300s) → `ErrandStatus.TIMED_OUT`.

**Request:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `module` | `string` | yes | Адрес модуля `core.<class>.<state>` либо `core.cmd.shell` / `core.exec.run` (whitelist Soul-side). |
| `input` | `object` | optional | Input модуля (форма зависит от модуля). |
| `timeout_seconds` | `int` (1..300) | optional | Полный timeout. Default `30`. |
| `dry_run` | `bool` | optional | `true` → Soul зовёт `mod.Plan` (только read-safe модули). |

**Response (`ErrandResult` / `ErrandStatus`):** `status` ∈ `running` / `success` / `failed` / `timed_out` / `module_not_allowed`; `exit_code` (NULL для read-safe non-shell); `stdout`/`stderr` (маскированный вывод, cap 64 KiB) + `*_truncated`-флаги; `duration_ms`; `error_message` (маскированная причина FAILED/TIMED_OUT/MODULE_NOT_ALLOWED); `output` (структурный output read-safe модулей, для shell/exec отсутствует).

**Errors:** `404 not-found` (Soul не подключён к кластеру), `422 validation-failed` (пустой `module`, `timeout_seconds` вне [1, 300]).

#### `GET /v1/errands/{errand_id}` / `GET /v1/errands` — чтение Errand-ов

Permission: `errand.list` (read-permission покрывает list+get). MCP-tool: `keeper.errand.list` (list); detail — REST polling.

`GET /v1/errands` — фильтры `sid` / `status` / `started_after` (RFC 3339) + pagination ([§ Pagination](../operator-api.md#pagination)). `GET /v1/errands/{errand_id}` — строка по ULID (форма как у `POST .../exec` ответа + `started_by_aid`, `started_at`, `finished_at`).

**Errors:** `404 not-found` (`errand_id` не существует).

#### `DELETE /v1/errands/{errand_id}` — отмена in-flight Errand

Permission: `errand.cancel`. MCP-tool: `keeper.errand.cancel`. Path-param: `errand_id` (ULID).

Cancel-flow (slice E5, best-effort): `DELETE /v1/errands/{errand_id}` отправляет `CancelErrand` Soul-у через EventStream-канал — Soul-side `errandrunner` отменяет ctx активной Run-горутины, та возвращает `ErrandResult{status: CANCELLED}` тем же каналом, applybus-receiver на Keeper переводит строку `errands` в `status=cancelled`. Финальный статус оператор смотрит через `GET /v1/errands/{errand_id}` (poll).

**Response:** `204 No Content` при успешной отправке сигнала. **Errors:** `409 errand-not-cancellable` (Errand уже в терминальном статусе), `404 not-found` (`errand_id` неизвестен либо целевой Soul не подключён).
