# Sigil-key — endpoints ротации ключей подписи Sigil

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/sigil/keys*` (ротация trust-anchor-**ключей подписи** Sigil, [ADR-026(h)](../../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), R3-S7; реестр `sigil_signing_keys`). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/sigils.md](../mcp-tools/sigils.md). Допуски самих бинарей (allow-list, отдельная зона) — [operator-api/plugins.md](plugins.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 4 роутов) — в корневом [operator-api.md → Sigil-key (4)](../operator-api.md#sigil-key-4--ротация-ключей-подписи-sigil-adr-026h--r3). **3-сегментный MCP-tool `keeper.sigil.key.<verb>` ↔ 2-сегментная permission `sigil.key-<verb>`** (resource `sigil`, hyphenated action). Selector — NoSelector.

| Метод / Path | Permission | MCP-tool |
|---|---|---|
| `POST /v1/sigil/keys` | `sigil.key-introduce` | [`keeper.sigil.key.introduce`](../mcp-tools/sigils.md#keepersigilkeyintroduce) |
| `GET /v1/sigil/keys` | `sigil.key-list` | [`keeper.sigil.key.list`](../mcp-tools/sigils.md#keepersigilkeylist) |
| `POST /v1/sigil/keys/{key_id}/primary` | `sigil.key-set-primary` | [`keeper.sigil.key.set-primary`](../mcp-tools/sigils.md#keepersigilkeyset-primary) |
| `DELETE /v1/sigil/keys/{key_id}` | `sigil.key-retire` | [`keeper.sigil.key.retire`](../mcp-tools/sigils.md#keepersigilkeyretire) |

Подписной приватник пишется в Vault KV (`secret/keeper/sigil-keys/<key_id>`) и **никогда** не возвращается в response/лог. Полные request/response/output-схемы и коды ошибок (`sigil-key-not-found` / `sigil-key-last-active` / `sigil-key-primary` / `sigil-key-concurrent-change`) — на MCP-стороне [mcp-tools/sigils.md → Sigil-key (4)](../mcp-tools/sigils.md#sigil-key-4) (формы 1:1 с HTTP-телами). Все четыре роута монтируются только при сконфигурированном Sigil (`keeper.yml → sigil.signing_key_ref`); при выключенном Sigil блок `/v1/sigil/keys*` **не монтируется** → запрос ловит catch-all → `404 not-found` (`router.go`: `if sigilKeyH != nil`). Мутирующие 3 роута (`introduce`/`set-primary`/`retire`) аудируются ([ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `key-list` — read-only, без audit.
