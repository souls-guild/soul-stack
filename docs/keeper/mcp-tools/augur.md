# Augur — MCP-tools реестров Omen / Rite

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.augur.omen.*` / `keeper.augur.rite.*` (реестры внешних систем и grant-ов брокера Augur, [ADR-025](../../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul), [augur.md](../augur.md)). Транспорт, auth, формат tool declaration, async-convention, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api/augur.md](../operator-api/augur.md).

### Augur (6)

Реестры Augur — Omen (внешняя система) и Rite (grant) ([ADR-025](../../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul), [augur.md](../augur.md)). 4-сегментный tool-name `keeper.augur.<resource>.<action>` ↔ 2-сегментная permission `<resource>.<action>` (`omen.create` / `rite.list` / …, selector — NoSelector). Бизнес-логика (валидация `name`/`source_type`/`auth_ref`, XOR-субъект, allow-shape по `source_type`, token-поля только для vault-delegate) живёт в `augur.Service`; tool — транспорт. Tools доступны только при подключённом реестре; при выключенном вызов возвращает `internal-error` («augur registry is not configured»). **Live-fetch от Soul (`AugurRequest`) этими tool-ами НЕ управляется** — это машинный gRPC-запрос, не операторская операция ([rbac.md §Augur](../rbac.md)).

#### `keeper.augur.omen.create`

Создаёт Omen в `omens`: внешняя система (`vault`/`prometheus`/`elk`) + `endpoint` + `auth_ref` (vault-ref на master-cred, **не** сам секрет — [augur.md §4.1](../augur.md#41-таблица-omens--реестр-внешних-систем)). Permission: `omen.create`. Endpoint: [`POST /v1/augur/omens`](../operator-api/augur.md#post-v1auguromens--создать-omen). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя Omen-а (kebab-case `^[a-z0-9-]{1,63}$`). |
| `source_type` | `string` | yes | `vault` / `prometheus` / `elk`. |
| `endpoint` | `string` | yes | URL внешней системы (не секрет). |
| `auth_ref` | `string` | yes | vault-ref `vault:<mount>/<path>` на master-credential. |

**Output:** `OmenView` — `{name, source_type, endpoint, auth_ref, created_by_aid?, created_at}`.

Ошибки: `omen-already-exists` (`name` занят), `validation-failed` (битый `name`/`source_type`/`endpoint`/`auth_ref`). Audit: `omen.created` (payload `{name, source_type, endpoint, auth_ref, created_by_aid}` — значения секретов НЕ кладутся).

#### `keeper.augur.omen.list`

Перечисление Omen-ов (sort `created_at` DESC, `name` ASC). Permission: `omen.list`. Endpoint: [`GET /v1/augur/omens`](../operator-api/augur.md#get-v1auguromens--список-omen-ов). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `offset` | `integer` | no | Смещение пагинации (≥ 0). |
| `limit` | `integer` | no | Размер страницы (≥ 1). |

**Output:** `{omens: array<OmenView>, total}`.

#### `keeper.augur.omen.delete`

Удаляет Omen по имени; каскадно убирает связанные Rite-ы (`ON DELETE CASCADE`). Permission: `omen.delete`. Endpoint: [`DELETE /v1/augur/omens/{name}`](../operator-api/augur.md#delete-v1auguromensname--удалить-omen). Async: нет.

**Input:** `{name}`.

**Output:** пустой объект (REST-эквивалент — 204 No Content).

Ошибки: `not-found` (записи нет). Audit: `omen.revoked` (payload `{name}`).

#### `keeper.augur.rite.create`

Создаёт Rite (grant): субъект (`coven` **XOR** `sid`) × `omen` → `allow`-list + `delegate` + опц. `token_ttl`/`token_num_uses` (только vault-delegate). Permission: `rite.create`. Endpoint: [`POST /v1/augur/rites`](../operator-api/augur.md#post-v1augurrites--создать-rite). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `omen` | `string` | yes | Omen, к которому относится grant. |
| `coven` | `string` | no | Субъект по Coven-метке (XOR с `sid`). |
| `sid` | `string` | no | Субъект по конкретному SID (XOR с `coven`). |
| `allow` | `object` | yes | Allow-list; форма по `source_type` Omen-а (vault `{paths?,policies?}` / prometheus `{queries}` / elk `{indices}`). |
| `delegate` | `boolean` | no | `false` — брокер; `true` — делегация. |
| `token_ttl` | `string` | no | TTL минтуемого scoped-токена; только vault-delegate. |
| `token_num_uses` | `integer` | no | Лимит использований токена; только vault-delegate. |

**Output:** `RiteView` — `{id, omen, coven?, sid?, allow, delegate, token_ttl?, token_num_uses?, created_by_aid?, created_at}`.

Ошибки: `not-found` (Omen не существует), `validation-failed` (нарушение XOR / битый `allow` / token-поля). Audit: `rite.created` (payload `{id, omen, subject, delegate, created_by_aid}` — `allow`-list НЕ кладётся).

#### `keeper.augur.rite.list`

Перечисление Rite-ов одного Omen-а (фильтр `omen` обязателен; sort `created_at` DESC, `id` ASC). Permission: `rite.list`. Endpoint: [`GET /v1/augur/rites`](../operator-api/augur.md#get-v1augurrites--список-rite-ов-omen-а). Async: нет.

**Input:** `{omen}` (обязательно).

**Output:** `{rites: array<RiteView>}`.

#### `keeper.augur.rite.delete`

Удаляет Rite по суррогатному `id`. Permission: `rite.delete`. Endpoint: [`DELETE /v1/augur/rites/{id}`](../operator-api/augur.md#delete-v1augurritesid--удалить-rite). Async: нет.

**Input:** `{id}` (положительное целое).

**Output:** пустой объект (REST-эквивалент — 204 No Content).

Ошибки: `not-found` (записи нет). Audit: `rite.revoked` (payload `{id}`).
