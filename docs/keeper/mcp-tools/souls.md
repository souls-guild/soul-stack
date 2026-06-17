# Soul — MCP-tools реестра хостов

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.soul.*` (регистрация хоста, bootstrap-токены, bulk-назначение Coven-меток). Транспорт, auth, формат tool declaration, async-convention, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api.md → Soul](../operator-api/souls.md).

### Soul (5)

`keeper.soul.list` остаётся транспортом будущего M2-реестра (в `manifest.go` помечен stub); остальные четыре — Implemented. Каталог `manifest.go` содержит пять Soul-tool-ов (`create` / `issue-token` / `coven-assign` / `list` / `ssh-target.update`). Read-роуты реестра (`GET /v1/souls/{sid}`, `/soulprint`, `/history`) — **REST-only** (MCP-tool-ов нет, покрыты permission `soul.list`).

#### `keeper.soul.create`

Регистрация нового хоста в реестре `souls` + выпуск первого bootstrap-токена (для `transport: agent`). Permission: `soul.create`. Endpoint: [`POST /v1/souls`](../operator-api/souls.md#post-v1souls--зарегистрировать-soul). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `sid` | `string` (FQDN) | yes | SID нового хоста. |
| `transport` | `string` (enum `agent`/`ssh`) | yes | Способ управления хостом. |
| `covens` | `array<string>` | optional | Coven-метки хоста. Default `[]`. |
| `note` | `string` | optional | Свободный комментарий к записи. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `sid` | `string` | SID созданной записи. |
| `transport` | `string` (enum) | Зеркало input. |
| `covens` | `array<string>` | Зеркало input. |
| `status` | `string` (enum) | Всегда `pending`. |
| `registered_at` | `string` (RFC 3339) | Время создания. |
| `created_by_aid` | `string` | AID Архонта, выполнившего вызов. |
| `bootstrap_token` | `string` (optional) | Plain bootstrap-токен; присутствует только для `transport: agent`. **Отдаётся один раз**, маскируется в логах. |
| `expires_at` | `string` (RFC 3339, optional) | Срок истечения токена; вместе с `bootstrap_token`. Wire-ключ — `expires_at` (синхронен REST `SoulCreateReply` и `openapi.yaml`; legacy-имя `token_expires_at` не используется). |

#### `keeper.soul.issue-token`

Перевыпуск bootstrap-токена для существующей Soul (`transport: agent`). Permission: `soul.issue-token`. Endpoint: [`POST /v1/souls/{sid}/issue-token`](../operator-api/souls.md#post-v1soulssidissue-token--перевыпустить-bootstrap-токен). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `sid` | `string` (FQDN) | yes | SID Soul (echo path-param HTTP-стороны). |
| `force` | `boolean` | optional | Default `false`. `true` — истечь активный токен и выпустить новый. При `false` и наличии активного токена tool возвращает `bootstrap-token-active` error. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `sid` | `string` | SID. |
| `bootstrap_token` | `string` | Новый plain bootstrap-токен. **Отдаётся один раз**, маскируется в логах. |
| `expires_at` | `string` (RFC 3339) | Срок истечения нового токена. Wire-ключ — `expires_at` (синхронен REST и `openapi.yaml`). |

Для Soul с `transport: ssh` tool возвращает `validation-failed` error (ssh-хост не имеет bootstrap-фазы).

#### `keeper.soul.coven-assign`

Массовое назначение Coven-меток: добавить ОДНУ метку (`mode: append`) / снять (`mode: remove`) либо ЗАМЕНИТЬ (`mode: replace`) набор Coven-меток целиком на хостах под `selector` ∩ coven-scope оператора. Coven — холодная PG-метка (чистый UPDATE `souls`). Permission: `soul.coven-assign`. Endpoint: [`POST /v1/souls/coven`](../operator-api/souls.md#post-v1soulscoven--bulk-назначение-coven-меток). Async: нет.

**Scope-intersection (security).** Применяется двойная проверка, идентичная REST: (a) целевые хосты ⊆ coven-scope оператора (предикат `coven && ARRAY[scope]`); (b) назначаемая метка ∈ scope (permission-гейт `RBAC.Check` с селектором `coven=<label>` + service-проверка). Для `replace` гейт (b) проходит по КАЖДОЙ метке набора: оператор с scope `dev` не может навесить `prod` через `labels: [dev, prod]` (отказ `forbidden`). Оператор с `soul.coven-assign on coven=dev` не может навесить/снять метку `prod` (отказ `forbidden`) и не затронет хосты вне `dev`. Bare/`*`-permission снимает оба ограничения. Без этой проверки MCP стал бы обходом REST-защиты (privilege-escalation), поэтому MCP-путь выполняет её на тех же service-функциях, что REST.

**Input (XOR `label` ↔ `labels` по mode):**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `mode` | `string` (enum `append`/`remove`/`replace`) | yes | Добавить (`append`) / снять (`remove`) одну метку либо заменить (`replace`) набор целиком. |
| `label` | `string` (Coven-метка) | для append/remove | Назначаемая/снимаемая метка. Запрещён для `replace`. |
| `labels` | `array<string>` (Coven-метки) | для replace | Набор меток для `replace` (может быть пустым = «снять все»). Каждая метка обязана быть в coven-scope оператора. Запрещён для `append`/`remove`. |
| `selector` | `object` | yes | Таргет хостов (∩ scope); минимум один критерий. Комбинации соединяются AND. |
| `selector.all` | `boolean` | optional | Весь реестр (без host-фильтра). |
| `selector.sids` | `array<string>` | optional | Точечный список SID. |
| `selector.coven` | `string` | optional | Хосты, у которых УЖЕ есть эта метка. |
| `selector.incarnation` | `string` (incarnation-имя) | optional | Хосты этой incarnation (имя incarnation как корневая Coven-метка, ADR-008). |
| `selector.status` | `string` (enum) | optional | Фильтр по статусу `souls`. |
| `dry_run` | `boolean` | optional | Default `false`. `true` — вернуть `matched` без UPDATE. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `mode` | `string` | Зеркало input. |
| `label` | `string` | Применённая метка для `append`/`remove` (зеркало input). |
| `labels` | `array<string>` | Применённый набор меток для `replace` (зеркало input). |
| `matched` | `integer` | Хосты под `selector` ∩ scope. |
| `changed` | `integer` | Фактически изменённые строки (0 для `dry_run`). |
| `status` | `string` (enum `completed`/`partial`) | `partial` — часть чанков закоммичена, затем фейл (закоммиченное идемпотентно до-повторяется оператором, не откатывается). |
| `dry_run` | `boolean` | Зеркало input. |

Пустой селектор (ни `all`, ни `sids`/`coven`/`incarnation`/`status`), метка/любая метка набора вне coven-scope, или XOR-нарушение (`label`+`labels` вместе / `label` без `labels` для replace / наоборот) → `validation-failed` / `forbidden` error.

#### `keeper.soul.list`

Перечисление Souls. Permission: `soul.list`. Endpoint: [`GET /v1/souls`](../operator-api/souls.md#get-v1souls--список-souls). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `coven` | `string` или `array<string>` | optional | Фильтр по coven-метке (exact-match по любому значению из `souls.coven[]`); множественный — массив значений. |
| `status` | `string` (enum) | optional | `pending` / `connected` / `disconnected` / `expired`. |
| `transport` | `string` (enum) | optional | `agent` / `ssh`. |
| `offset` | `integer` | optional | Default `0`. |
| `limit` | `integer` | optional | Default `50`. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `items` | `array<SoulListEntry>` | Элементы — `{sid, transport, status, covens, last_seen_at, last_seen_by_kid, registered_at}`. |
| `offset`, `limit`, `total` | `integer` | Pagination. |

#### `keeper.soul.ssh-target.update`

Обновляет per-host SSH-реквизиты push-flow (`souls.ssh_target` jsonb: `ssh_port`/`ssh_user`/`soul_path`, [ADR-032](../../adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-без-incarnationscenario) amendment 2026-05-26, S7-1). Source-of-truth для `PGFallbackTargetResolver`; `keeper.yml::push.targets[]` — legacy fallback под флагом `push.allow_legacy_push_targets`. Permission: `soul.ssh-target-update`; селектор `host=<sid>`. Endpoint: [`PUT /v1/souls/{sid}/ssh-target`](../operator-api/souls.md). Async: нет.

3-сегментный MCP-tool `keeper.soul.ssh-target.update` ↔ 2-сегментная permission `soul.ssh-target-update` (грамматика permission — ровно `<resource>.<action>`; параллель `keeper.sigil.key.introduce` ↔ `sigil.key-introduce`).

**Input** (`required: sid, ssh_port, ssh_user, soul_path`):

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `sid` | `string` (FQDN, echo path-param) | yes | SID хоста. |
| `ssh_port` | `integer` (1..65535) | yes | SSH-порт. |
| `ssh_user` | `string` (≥1) | yes | SSH-пользователь. |
| `soul_path` | `string` (абс. Unix-путь `^/.+`) | yes | Путь к `soul`-бинарю на хосте. |

**Output:** `{sid, ssh_target: {ssh_port, ssh_user, soul_path}}`. Ошибки: `not-found` (SID отсутствует в реестре `souls`).
