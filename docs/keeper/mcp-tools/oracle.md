# Oracle — MCP-tools реестров Vigil / Decree

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.oracle.vigil.*` / `keeper.oracle.decree.*` (реестры event-driven мониторинга Beacons, [ADR-030](../../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)). Транспорт, auth, формат tool declaration, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api/oracle.md](../operator-api/oracle.md).

### Oracle (6)

4-сегментный tool-name `keeper.oracle.<resource>.<action>` ↔ 2-сегментная permission `<resource>.<action>` (`vigil.create` / `decree.list` / …, selector — NoSelector). Бизнес-логика (валидация `name`/`interval`/`check`/субъект для Vigil; `name`/`on_beacon`/`incarnation`/`scenario`/субъект/`where`-CEL для Decree) живёт в `oracle.Service`; tool — транспорт. Tools доступны только при подключённом реестре; при выключенном вызов возвращает `internal-error` («oracle registry is not configured»). **Reactor-флоу (Portent → match Decree → enqueue) этими tool-ами НЕ управляется** ([rbac.md §Oracle](../rbac.md)).

#### `keeper.oracle.vigil.create`

Создаёт Vigil в `vigils` (Soul-side проверка beacons: `check` — адрес core-beacon + `interval` + субъект `coven` XOR `sid`). Read-only по конструкции. Permission: `vigil.create`. Endpoint: [`POST /v1/vigils`](../operator-api/oracle.md#post-v1vigils--создать-vigil). Async: нет.

**Input** (`required: name, interval, check`): `{name (kebab 1..63), interval (duration), check (core-beacon-адрес), coven? (array, XOR sid), sid? (XOR coven), params? (object), enabled? (default true)}`.

**Output:** `VigilView` — `{name, coven?, sid?, interval, check, params, enabled, created_by_aid?, created_at, updated_at}`.

Ошибки: `vigil-already-exists` (`name` занят), `validation-failed` (битый `name`/`interval`/`check`/субъект).

#### `keeper.oracle.vigil.list`

Перечисление Vigil-ов (sort `created_at` DESC, `name` ASC). Permission: `vigil.list`. Endpoint: [`GET /v1/vigils`](../operator-api/oracle.md#get-v1vigils--список-vigil-ов). Async: нет.

**Input:** `{offset?, limit?}`. **Output:** `VigilListReply` — `{items: array<VigilView>, offset, limit, total}`.

#### `keeper.oracle.vigil.delete`

Удаляет Vigil по имени (перестаёт раздаваться хостам в `VigilSnapshot`; Decree-ы НЕ каскадятся). Permission: `vigil.delete`. Endpoint: [`DELETE /v1/vigils/{name}`](../operator-api/oracle.md#delete-v1vigilsname--удалить-vigil). Async: нет.

**Input:** `{name}`. **Output:** пустой объект (REST-эквивалент — 204). Ошибки: `not-found`.

#### `keeper.oracle.decree.create`

Создаёт Decree (правило reactor): `on_beacon` (Vigil) × субъект (`coven` XOR `sid`) × `incarnation_name` → `action_scenario` (named, whitelist) + опц. `where`-CEL предикат над `event.data` + `cooldown`. Default-deny. Permission: `decree.create`. Endpoint: [`POST /v1/decrees`](../operator-api/oracle.md#post-v1decrees--создать-decree). Async: нет.

**Input** (`required: name, on_beacon, incarnation_name, action_scenario`): `{name (kebab 1..63), on_beacon (kebab), incarnation_name, action_scenario (named, ^[a-z][a-z0-9_]*$), coven? (XOR sid), sid? (XOR coven), where? (CEL), action_input? (object), cooldown? (duration), enabled? (default true)}`.

**Output:** `DecreeView` — `{name, on_beacon, where?, coven?, sid?, incarnation_name, action_scenario, action_input, cooldown, enabled, created_by_aid?, created_at, updated_at}`.

Ошибки: `decree-already-exists` (`name` занят), `validation-failed` (битый `name`/`on_beacon`/`incarnation_name`/`action_scenario`/субъект/`where`-CEL/`cooldown`).

#### `keeper.oracle.decree.list`

Перечисление Decree-ов (sort `created_at` DESC, `name` ASC). Permission: `decree.list`. Endpoint: [`GET /v1/decrees`](../operator-api/oracle.md#get-v1decrees--список-decree-ов). Async: нет.

**Input:** `{offset?, limit?}`. **Output:** `DecreeListReply` — `{items: array<DecreeView>, offset, limit, total}`.

#### `keeper.oracle.decree.delete`

Удаляет Decree по имени; каскадно чистит cooldown-state (`oracle_fires`, `ON DELETE CASCADE`). Permission: `decree.delete`. Endpoint: [`DELETE /v1/decrees/{name}`](../operator-api/oracle.md#delete-v1decreesname--удалить-decree). Async: нет.

**Input:** `{name}`. **Output:** пустой объект (REST-эквивалент — 204). Ошибки: `not-found`.
