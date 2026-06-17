# Plugin — MCP-tools Sigil allow-list целостности плагинов

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.plugin.*` (допуск/отзыв/список записей allow-list-а `plugin_sigils`, [ADR-026](../../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) S4a). Транспорт, auth, формат tool declaration, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике, телам и `sha256`-вычислению — [plugins.md → Integrity-model](../plugins.md#integrity-model); REST-сторона — [operator-api/plugins.md](../operator-api/plugins.md). Ротация самих ключей **подписи** (отдельная зона) — [mcp-tools/sigils.md](sigils.md).

### Plugin (3)

Sigil allow-list целостности плагинов ([ADR-026](../../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)). 1:1 с REST `POST/GET/DELETE /v1/plugins/sigils*` и permission (`keeper.plugin.<action>` ↔ `plugin.<action>`, selector — NoSelector, как `operator.*`/`role.*`). `ref` — git-verified (Keeper резолвит `source`+`ref` в `commit_sha`-слот через go-git, вариант A/F-fetch, [ADR-026(g)](../../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)): в lookup слота не участвует (читается активный слот через `current`), authority целостности — `sha256` + подпись Keeper-а. Tools доступны только при сконфигурированном Sigil (`keeper.yml → sigil.signing_key_ref`); при выключенном Sigil вызов возвращает `internal-error` («sigil is not configured»).

#### `keeper.plugin.allow`

Допуск `(namespace, name, ref)` в allow-list `plugin_sigils`: Keeper читает бинарь активного слота кеша через `current`-symlink (R-nested `<ns>-<name>/<commit_sha>/`), считает `sha256`, подписывает и вставляет запись. Permission: `plugin.allow`. Endpoint: [`POST /v1/plugins/sigils`](../operator-api/plugins.md). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `namespace` | `string` | yes | Namespace плагина (kebab-case + точки/подчёркивание; без слешей и `..`). |
| `name` | `string` | yes | Имя плагина. |
| `ref` | `string` | yes | Operator-asserted метка допуска (tag-ref вида `v1.0.0`). Branch-ref со слешем в MVP не поддержан. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `namespace`, `name`, `ref` | `string` | Эхо input. |
| `sha256` | `string` | SHA-256 (hex) допущенного бинаря. |

Ошибки: `plugin-not-in-cache` (плагина нет в кеше host-а), `sigil-already-active` (активный допуск на `(namespace, name, ref)` уже есть), `validation-failed` (битая тройка). Audit: `plugin.allowed`.

#### `keeper.plugin.revoke`

Отзыв активного допуска `(namespace, name, ref)` из `plugin_sigils` (бинарь перестаёт проходить Sigil-верификацию). Permission: `plugin.revoke`. Endpoint: [`DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}`](../operator-api/plugins.md). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `namespace` | `string` | yes | Namespace плагина. |
| `name` | `string` | yes | Имя плагина. |
| `ref` | `string` | yes | Метка отзываемого допуска. |

**Output:** пустой объект (REST-эквивалент — 204 No Content).

Ошибки: `sigil-not-found` (активной записи нет), `validation-failed` (битая тройка). Audit: `plugin.revoked`.

#### `keeper.plugin.list`

Перечисление активных (не отозванных) записей allow-list-а `plugin_sigils`, новые первыми. Без `signature`/`manifest` (крипто-материал / крупный JSONB). Permission: `plugin.list`. Endpoint: [`GET /v1/plugins/sigils`](../operator-api/plugins.md). Async: нет.

**Input:** пустой объект.

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `sigils` | `array<SigilView>` | Элементы — `{namespace, name, ref, sha256, allowed_by_aid, allowed_at, revoked_at}`. |
