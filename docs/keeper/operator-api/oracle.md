# Oracle — endpoints реестров Vigil / Decree

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/vigils*` + `/v1/decrees*` — CRUD реестров event-driven мониторинга Beacons (Vigil = Soul-side проверка, Decree = правило reactor Portent → match → enqueue scenario, [ADR-030](../../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/oracle.md](../mcp-tools/oracle.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 8 роутов) — в корневом [operator-api.md → Oracle (8)](../operator-api.md#oracle-8--реестры-vigil--decree-event-driven-мониторинг-adr-030). Полная схема request/response — [`openapi.yaml`](../openapi.yaml) (`VigilCreateRequest` / `VigilView` / `VigilListReply` / `DecreeCreateRequest` / `DecreeView` / `DecreeListReply` — **источник правды по форме**). `vigil.*`/`decree.*` — NoSelector. Reactor-флоу (Portent → match Decree → enqueue scenario) этими permission-ами **НЕ управляется** — машинный Soul-инициированный путь; security держится на субъектной привязке Decree ([ADR-030(b)](../../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)).

### `POST /v1/vigils` — создать Vigil

Permission: `vigil.create`. MCP-tool: `keeper.oracle.vigil.create`. Read-only по конструкции (наблюдает, не мутирует хост).

**Request `VigilCreateRequest`** (`required: name, interval, check`):

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (kebab `^[a-z0-9-]{1,63}$`) | yes | Имя Vigil-а. |
| `interval` | `string` (duration) | yes | Частота проверки (`30s`). |
| `check` | `string` | yes | Адрес core-beacon (`core.beacon.file_changed`). |
| `coven` | `array<string>` | no | Субъект-метки coven (**XOR** с `sid`). |
| `sid` | `string` | no | Субъект — один конкретный SID (**XOR** с `coven`). |
| `params` | `object` | no | Параметры проверки; форма зависит от `check` (typed-схема отложена). |
| `enabled` | `boolean` | no | Активна ли проверка. Default `true`. |

**Response `201 VigilView`:** `{name, coven?, sid?, interval, check, params, enabled, created_by_aid?, created_at, updated_at}`.

Ошибки: `400` (битый JSON), `409 vigil-already-exists` (`name` занят), `422 validation-failed` (битый `name`/`interval`/`check`/субъект). Audit: `vigil.created`.

### `GET /v1/vigils` — список Vigil-ов

Permission: `vigil.list`. MCP-tool: `keeper.oracle.vigil.list`. Query `offset`/`limit`. Sort `created_at` DESC, `name` ASC. Response `200 VigilListReply` (`{items, offset, limit, total}`).

### `GET /v1/vigils/{name}` — прочитать Vigil

Permission: `vigil.list` (одно permission покрывает list+get). MCP-tool: `keeper.oracle.vigil.list`. Response `200 VigilView`; `404 not-found` — записи нет.

### `DELETE /v1/vigils/{name}` — удалить Vigil

Permission: `vigil.delete`. MCP-tool: `keeper.oracle.vigil.delete`. Перестаёт раздаваться хостам в `VigilSnapshot`; связанные Decree-ы **НЕ каскадятся**. Response `204`; `404 not-found`. Audit: `vigil.deleted`.

### `POST /v1/decrees` — создать Decree

Permission: `decree.create`. MCP-tool: `keeper.oracle.decree.create`. Default-deny: правило срабатывает только на свой `on_beacon` × субъект × `incarnation_name`.

**Request `DecreeCreateRequest`** (`required: name, on_beacon, incarnation_name, action_scenario`):

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (kebab `^[a-z0-9-]{1,63}$`) | yes | Имя Decree-а. |
| `on_beacon` | `string` (kebab) | yes | Имя Vigil-а, на чей Portent правило реагирует. |
| `incarnation_name` | `string` (`^[a-z0-9][a-z0-9-]{0,62}$`) | yes | Таргет-incarnation реакции (ServiceRef резолвится из неё). |
| `action_scenario` | `string` (`^[a-z][a-z0-9_]*$`) | yes | Named scenario (whitelist; raw-команда отвергнута). |
| `coven` | `array<string>` | no | Субъект-метки coven (**XOR** с `sid`). |
| `sid` | `string` | no | Субъект — один конкретный SID (**XOR** с `coven`). |
| `where` | `string` (CEL) | no | Предикат над `event.data`; compile-проверяется на create. |
| `action_input` | `object` | no | Вход сценария (vault-ref едет как есть). |
| `cooldown` | `string` (duration) | no | Минимальный интервал между срабатываниями per-(decree, subject). |
| `enabled` | `boolean` | no | Активно ли правило. Default `true`. |

**Response `201 DecreeView`:** `{name, on_beacon, where?, coven?, sid?, incarnation_name, action_scenario, action_input, cooldown, enabled, created_by_aid?, created_at, updated_at}`.

Ошибки: `400` (битый JSON), `409 decree-already-exists` (`name` занят), `422 validation-failed` (битый `name`/`on_beacon`/`incarnation_name`/`action_scenario`/субъект/`where`-CEL/`cooldown`). Audit: `decree.created`.

### `GET /v1/decrees` — список Decree-ов

Permission: `decree.list`. MCP-tool: `keeper.oracle.decree.list`. Query `offset`/`limit`. Sort `created_at` DESC, `name` ASC. Response `200 DecreeListReply`.

### `GET /v1/decrees/{name}` — прочитать Decree

Permission: `decree.list`. MCP-tool: `keeper.oracle.decree.list`. Response `200 DecreeView`; `404 not-found`.

### `DELETE /v1/decrees/{name}` — удалить Decree

Permission: `decree.delete`. MCP-tool: `keeper.oracle.decree.delete`. Каскадно чистит cooldown-state (`oracle_fires`, `ON DELETE CASCADE`). Response `204`; `404 not-found`. Audit: `decree.deleted`.

> **Секреты в payload.** В audit Vigil/Decree кладутся `name`/`check`/`interval`/субъект (vigil) и `name`/`on_beacon`/`incarnation`/`scenario`/субъект (decree); `params` / `where`-CEL / `action_input` **НЕ кладутся** (`action_input` может транзитом нести vault-ref).
