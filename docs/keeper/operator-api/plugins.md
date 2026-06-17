# Plugin — endpoints Sigil allow-list целостности плагинов

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/plugins/sigils*` (допуск/отзыв/список записей allow-list-а `plugin_sigils` — бинарь плагина проходит Sigil-верификацию только при активном допуске, [ADR-026](../../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) S4a). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). Источник правды по семантике, телам и `sha256`-вычислению — [plugins.md → Integrity-model](../plugins.md#integrity-model). MCP-сторона — [mcp-tools/plugins.md](../mcp-tools/plugins.md). Ротация самих ключей **подписи** (отдельная зона) — [operator-api/sigils.md](sigils.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 3 роутов) — в корневом [operator-api.md → Plugin (3)](../operator-api.md#plugin-3--sigil-allow-list-целостности-плагинов-adr-026-s4a). Отдельная зона от Sigil-key: тот про ключи **подписи**, этот — про допуски самих бинарей. `plugin.*` — NoSelector.

| Метод / Path | Permission | MCP-tool |
|---|---|---|
| `POST /v1/plugins/sigils` | `plugin.allow` | [`keeper.plugin.allow`](../mcp-tools/plugins.md#keeperpluginallow) |
| `GET /v1/plugins/sigils` | `plugin.list` | [`keeper.plugin.list`](../mcp-tools/plugins.md#keeperpluginlist) |
| `DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}` | `plugin.revoke` | [`keeper.plugin.revoke`](../mcp-tools/plugins.md#keeperpluginrevoke) |

Полные request/response/output-схемы и коды ошибок (`plugin-not-in-cache` / `sigil-already-active` / `sigil-not-found` / `validation-failed`) — на MCP-стороне [mcp-tools/plugins.md → Plugin (3)](../mcp-tools/plugins.md#plugin-3) (формы 1:1 с HTTP-телами). Мутирующие 2 роута (`allow`/`revoke`) аудируются (supply-chain-мутации, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `plugin.list` — read-only, без audit. Роуты монтируются только при сконфигурированном Sigil (`keeper.yml → sigil.signing_key_ref`); при выключенном Sigil блок `/v1/plugins/sigils*` **не монтируется** → запрос ловит catch-all → `404 not-found` (`router.go`: `if sigilH != nil`).
