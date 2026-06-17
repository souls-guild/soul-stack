# Sigil-key — MCP-tools ротации ключей подписи Sigil

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.sigil.key.*` (ротация trust-anchor-**ключей подписи** Sigil, [ADR-026(h)](../../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), R3-S7). Транспорт, auth, формат tool declaration, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api/sigils.md](../operator-api/sigils.md). Допуски самих бинарей (allow-list, отдельная зона) — [mcp-tools/plugins.md](plugins.md).

### Sigil-key (4)

Ротация trust-anchor-**ключей подписи** Sigil ([ADR-026(h)](../../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), R3-S7; реестр `sigil_signing_keys`, отдельный от допусков `plugin_sigils`). 1:1 с REST `POST/GET /v1/sigil/keys` + `POST /v1/sigil/keys/{key_id}/primary` + `DELETE /v1/sigil/keys/{key_id}`. **3-сегментный tool-name `keeper.sigil.key.<verb>` ↔ 2-сегментная permission `sigil.key-<verb>`** (selector NoSelector). Бизнес-логика (key-gen, Vault-write, CRUD, publish `sigil:anchors-changed`) — в `sigil.KeyService`; tool — транспорт. Доступны только при сконфигурированном Sigil; иначе `internal-error` («sigil is not configured»). **Приватник НИКОГДА не в output/логе.**

#### `keeper.sigil.key.introduce`

Keeper генерирует ed25519-пару, пишет приватник в Vault KV (`secret/keeper/sigil-keys/<key_id>`), вставляет публичную часть в реестр как active. `make_primary=true` делает ключ primary. Permission: `sigil.key-introduce`. Endpoint: [`POST /v1/sigil/keys`](../operator-api/sigils.md). Async: нет.

**Input:** `{make_primary?: bool}` (по умолчанию false; тело опционально).

**Output:** `{key_id, pubkey_pem, is_primary, status, introduced_at}` — без приватника. Ошибки: `sigil-key-concurrent-change` (гонка primary, retry). Audit: `sigil.key-introduced`.

#### `keeper.sigil.key.list`

Active-ключи подписи (primary первым, без `vault_ref`). Permission: `sigil.key-list`. Endpoint: [`GET /v1/sigil/keys`](../operator-api/sigils.md). Async: нет.

**Input:** пустой объект. **Output:** `{keys: array<{key_id, is_primary, status, introduced_at}>}`.

#### `keeper.sigil.key.set-primary`

Делает active-ключ primary (новые Sigil-ы подписываются им после cluster reload). Permission: `sigil.key-set-primary`. Endpoint: [`POST /v1/sigil/keys/{key_id}/primary`](../operator-api/sigils.md). Async: нет.

**Input:** `{key_id}` (64-hex). **Output:** пустой объект. Ошибки: `sigil-key-not-found`, `sigil-key-concurrent-change` (гонка primary либо ключ retired), `validation-failed`. Audit: `sigil.key-primary-set`.

#### `keeper.sigil.key.retire`

Выводит ключ из набора (Soul забывает при следующем `SigilTrustAnchors`). Permission: `sigil.key-retire`. Endpoint: [`DELETE /v1/sigil/keys/{key_id}`](../operator-api/sigils.md). Async: нет.

**Input:** `{key_id}` (64-hex). **Output:** пустой объект. Ошибки: `sigil-key-not-found` (active-записи нет), `sigil-key-last-active` (последний active), `sigil-key-primary` (primary напрямую — сперва set-primary другому), `validation-failed`. Audit: `sigil.key-retired`.
