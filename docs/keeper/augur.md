# Augur — брокер внешнего доступа Soul

> **Статус — MVP-1 реализован (2026-05, в baseline); MVP-2 отложен.** Источник статуса — [ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul) (amendment 2026-06-16). Этот документ нормирует архитектуру **Augur**.
>
> - **MVP-1 (брокер, `delegate=false`) — РЕАЛИЗОВАН:** реестры `omens` / `rites` (миграции 032/033), keeper-side fetch Vault / Prometheus / ELK, Soul-side `core.augur.fetch`, роуты `/v1/augur/*` (`omens` + `rites`).
> - **MVP-2 (делегация, `delegate=true`, минтинг scoped-токенов) — ОТЛОЖЕН:** на уровне кода Rite с `delegate=true` резолвится в `denied`; proto-сообщения `ScopedVaultToken` / `ScopedStaticCred` заведены как контракт, но не заполняются. Порядок реализации относительно прочего backlog-а — за PM/пользователем.

**Augur** — Keeper-side подсистема, дающая Soul-у **живой** (во время рендера / apply) доступ к внешним системам — Vault, Prometheus, ELK — которого не покрывает pre-resolved-модель Soul Stack. Метафора: авгур (оракул) посредничает между смертным и волей богов; здесь Augur посредничает между Soul-ом и внешними системами, не отдавая Soul-у master-credential.

## 1. Зачем Augur — граница с pre-resolved-моделью

Действующая модель доступа к внешним системам — **pre-resolved**: всё, что нужно прогону, резолвится Keeper-side **до** отправки команды на Soul. CEL-фаза (`vault(...)` / `soulprint.*` / `register.*` / `essence.*`) исполняется на Keeper-е, Vault читается на Keeper-е, и Soul получает уже отрендеренный `ApplyRequest` ([ADR-012(d)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)). На Soul нет ни `cel-go`, ни Vault-клиента, ни Vault-токенов.

Эта модель не покрывает случаи, когда значение нужно **в момент исполнения на хосте**, а не на этапе рендера:

- секрет, который должен быть прочитан как можно ближе к использованию (короткоживущий dynamic secret Vault, который протухнет, если зарезолвить его на Keeper-е заранее);
- live-запрос в Prometheus («сколько сейчас реплик в кворуме?») как условие шага apply;
- чтение из ELK-индекса в ходе прогона.

Augur — **forward-looking слой**: он не заменяет pre-resolved-модель и не двигает её границу для обычного рендера. Он добавляет узкий, авторизованный канал «Soul просит внешнее значение прямо сейчас». Pre-resolved остаётся дефолтом; Augur — для того, что pre-resolve по своей природе не может отдать заранее.

## 2. Две фазы (один ADR)

Augur нормируется одним [ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul), но реализуется в две фазы. Граница между фазами — поле `delegate` в [Rite](#42-таблица-rites--grant--policy-mapping) и форма ответа в [`AugurReply`](#52-augurreply-keeper--soul).

### 2.1 MVP-1 — брокер (`delegate=false`)

Soul просит значение **через Keeper**; Keeper сам ходит во внешнюю систему и возвращает значение Soul-у inline.

- Для Vault — Keeper читает KV своим существующим механизмом (`ReadKV`, тот же, что у [`core.vault.kv-read`](modules.md) / implicit `${ vault(...) }`).
- Данные текут **через Keeper** (`AugurReply.inline_data`). На Soul внешний токен/credential **не попадает**.
- Граница [ADR-012(d)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) **не трогается**: внешний доступ остаётся за Keeper-ом, как и для обычного рендера. Меняется только **момент** (live, по запросу Soul-а), не **сторона**.
- AppRole для MVP-1 **не нужен** — Keeper использует уже имеющийся доступ к Vault.

### 2.2 MVP-2 — делегация (`delegate=true`)

Keeper выдаёт Soul-у узкий короткоживущий credential, и Soul ходит во внешнюю систему **напрямую**. Модель заимствована из salt-vault.

- **Vault.** Keeper **минтит** scoped / short-TTL / limited-use Vault-токен (`auth/token/create` под master-AppRole Keeper-а) с политиками/TTL/числом использований из Rite, и отдаёт его Soul-у (`AugurReply.scoped_vault_token`). Soul читает Vault напрямую этим эфемерным токеном. Требует: AppRole + право `auth/token/create` в `keeper/internal/vault` (server-side).
- **Prometheus / ELK.** Vault-style минтинга нет. Делегация = выдача scoped **статического** pre-scoped read-key (`AugurReply.scoped_static_cred`), который оператор заранее положил в Vault и сослался на него в `omens.auth_ref` отдельной [записью](#62-prometheus--elk-delegate-через-pre-scoped-read-key) — это **не** master-cred системы. Soul делает прямой read-only-запрос этим ключом.

## 3. Поток запроса end-to-end

```
Soul                         Keeper (Augur)                       Внешняя система
 │                                │                                       │
 │── AugurRequest ───────────────▶│                                       │
 │   {request_id, apply_id,       │ 1. resolve omen by name               │
 │    omen_name, query}           │ 2. SID ← mTLS peer cert               │
 │                                │ 3. SID → covens (registry)            │
 │                                │ 4. find Rite(omen, coven|sid)         │
 │                                │ 5. query ∈ Rite.allow ?               │
 │                                │ 6. branch by delegate / source_type   │
 │                                │                                       │
 │            delegate=false ─────┤── read KV / query ───────────────────▶│
 │◀── AugurReply.inline_data ─────┤◀── value ─────────────────────────────│
 │                                │                                       │
 │            delegate=true ──────┤── auth/token/create (vault) ──────────▶│
 │◀── AugurReply.scoped_vault_token / scoped_static_cred ─────────────────│
 │── (direct read with ephemeral cred) ──────────────────────────────────▶│
```

SID в запросе **не передаётся** как identity-claim — авторитет идентичности Soul-а это `Subject Alternative Name` mTLS peer cert ([ADR-012(i)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)). `apply_id` в запросе служит корреляцией с прогоном (см. [§8 Audit](#8-audit)).

## 4. Модель данных (Postgres)

Реестр Augur живёт в Postgres, управляется через OpenAPI / MCP — по образцу [Provider / Profile](cloud.md) ([architecture.md → Артефакты](../architecture.md#артефакты-soul-stack-что-в-git-что-в-бд): runtime-state → Postgres, не git). Две таблицы.

### 4.1 Таблица `omens` — реестр внешних систем

**Omen** — внешняя система, к которой Augur посредничает доступ (один Vault-mount, один Prometheus, один ELK-кластер). Аналог [Provider](cloud.md) для облаков.

| Колонка | Тип | Смысл |
|---|---|---|
| `name` | `TEXT PRIMARY KEY` | Имя Omen-а, kebab-case (`vault-prod` / `prom-main` / `elk-logs`). |
| `source_type` | `TEXT` (enum) | Тип внешней системы: `vault` / `prometheus` / `elk`. Descriptive-enum (см. [§7](#7-source_type-enum)). |
| `endpoint` | `TEXT` | URL внешней системы (`https://vault.internal:8200`). |
| `auth_ref` | `TEXT` (vault-ref) | **Всегда** `vault:<mount>/<path>`-ссылка на master-credential Keeper-а (Vault AppRole-secret / pre-scoped read-key). **Master-credential в БД не хранится** — только vault-ref на него. Формат vault-ref — [config.md](config.md) (диагностика `vault_ref_invalid_format`). |

**Инвариант:** `auth_ref` всегда vault-ref. Plaintext-credential в `omens` запрещён — симметрично `metrics.auth.basic.password_ref` ([config.md → metrics](config.md#metrics)) и `provider.credentials_ref` ([cloud.md](cloud.md)).

### 4.2 Таблица `rites` — grant / policy-mapping

**Rite** — grant: разрешение «такой-то субъект может через Augur получить из такого-то Omen-а такие-то значения, в таком-то режиме». Связывает субъект (Coven или конкретный SID) с Omen-ом, allow-list-ом и режимом доставки.

| Колонка | Тип | Смысл |
|---|---|---|
| `id` | `BIGINT` / `UUID` PK | Суррогатный ключ Rite-а. |
| `omen` | `TEXT REFERENCES omens(name) ON DELETE CASCADE` | Omen, к которому относится grant. **CASCADE**: удаление Omen-а удаляет все его Rite-ы (см. [§9 форк](#9-принятые-дизайн-форки)). |
| `coven` | `TEXT NULL` | Субъект-grant по Coven-метке. **XOR** с `sid`. |
| `sid` | `TEXT NULL` | Субъект-grant по конкретному SID. **XOR** с `coven`. |
| `allow` | `JSONB` | Allow-list разрешённых значений. Форма зависит от `source_type` Omen-а (см. ниже). |
| `delegate` | `BOOLEAN NOT NULL DEFAULT false` | `false` — брокер (MVP-1); `true` — делегация (MVP-2). |
| `token_ttl` | `interval` / `TEXT` (duration) `NULL` | **Только для `vault`-Omen с `delegate=true`**: TTL минтуемого scoped-токена. `NULL` для prom / elk. |
| `token_num_uses` | `INT NULL` | **Только для `vault`-Omen с `delegate=true`**: лимит использований минтуемого токена. `NULL` для prom / elk. |

**Субъект — строго XOR.** Ровно одно из `coven` / `sid` непусто (CHECK-constraint). `coven`-Rite применяется ко всем Soul-ам с этой меткой; `sid`-Rite — к одному хосту.

**`allow` по `source_type`:**

| `source_type` | Содержимое `allow` |
|---|---|
| `vault` | `paths` (KV-пути, доступные для чтения) и/или `policies` (Vault-политики, навешиваемые на минтуемый scoped-токен при `delegate=true`). |
| `prometheus` | `queries` (разрешённые запросы / шаблоны запросов). |
| `elk` | `indices` (разрешённые индексы). |

**`token_ttl` / `token_num_uses` — только vault-delegate.** Для prom / elk-делегации Vault-style минтинга нет (выдаётся статический pre-scoped read-key), поэтому оба поля `NULL`. Для `delegate=false` оба поля игнорируются (брокер не минтит токен).

## 5. Транспорт

Augur **не вводит новый RPC**. Он добавляет **два only-add сообщения** в `oneof payload` существующего `EventStream` ([ADR-012(c)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) forward-compat only-add; [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) «один долгоживущий стрим» — соблюдён). Сообщения живут в новом файле `proto/keeper/v1/augur.proto` (тематическая раскладка [ADR-012(b)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add): один файл = одна семантическая ось).

### 5.1 `AugurRequest` (Soul → Keeper)

| Поле | Тип | Смысл |
|---|---|---|
| `request_id` | string | ID запроса, для корреляции `AugurRequest` ↔ `AugurReply` внутри стрима. **Генерируется Soul-ом** (ULID / UUID), **уникален per-stream**. |
| `apply_id` | string | Прогон, в рамках которого Soul делает запрос (correlation для аудита). |
| `omen_name` | string | Имя Omen-а (`omens.name`). |
| `query` | string | Запрос к Omen-у: KV-путь (vault), promQL (prometheus), index-query (elk). Проверяется против `Rite.allow`. |

**`request_id` — генерация и уникальность.** ID запроса генерирует **Soul** (ULID / UUID); он обязан быть **уникален в пределах одного `EventStream`-а**. Назначение — корреляция параллельных Augur-запросов одного apply: в рамках прогона Soul может держать несколько Augur-запросов in-flight одновременно (разные шаги / Omen-ы), и Keeper эхо-возвращает `request_id` в `AugurReply`, чтобы Soul сопоставил ответ с ожиданием. Keeper `request_id` не интерпретирует как identity / авторизацию — только как непрозрачный correlation-ключ.

SID в payload **отсутствует** — берётся из mTLS peer cert ([ADR-012(i)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)).

### 5.2 `AugurReply` (Keeper → Soul)

| Поле | Тип | Смысл |
|---|---|---|
| `request_id` | string | Echo `AugurRequest.request_id`. |
| `status` | enum | `ok` / `denied` / `error`. |
| `result` | `oneof` | При `status=ok` — одно из трёх (см. ниже). |
| `error` | string | При `status=error` / `denied` — диагностика. |

`result` (`oneof`):

| Вариант | Когда | Смысл |
|---|---|---|
| `inline_data` | `delegate=false` (MVP-1, любой `source_type`) | Значение, прочитанное Keeper-ом и переданное Soul-у через Keeper. |
| `scoped_vault_token` | `delegate=true` + `source_type=vault` (MVP-2) | Эфемерный scoped Vault-токен (TTL / num_uses / policies из Rite), которым Soul читает Vault напрямую. |
| `scoped_static_cred` | `delegate=true` + `source_type ∈ {prometheus, elk}` (MVP-2) | Scoped read-only static cred (pre-scoped read-key), которым Soul делает прямой read-only-запрос. |

Forward-compat: новые `result`-варианты добавляются only-add, без reuse field-номеров ([ADR-012(c)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)).

### 5.3 Форма `inline_data` (shape convention)

`inline_data` — `google.protobuf.Struct` (всегда объект на уровне proto). Содержимое нормируется convention-ом по форме исходного результата:

- **Скаляр** (например vault-KV `#field` — одно значение из секрета) — объект `{ "value": <scalar> }`. Скаляр заворачивается в единственный ключ `value`, потому что `Struct` не несёт голый скаляр на верхнем уровне.
- **Map** (vault KV целиком / Prometheus-результат / ELK-ответ) — **натуральный объект** «как есть» (ключи исходной map-ы становятся ключами `Struct`).

**Проекцию `#field` делает Keeper при чтении Omen-а**, не Soul. То есть: если запрос адресует конкретное поле секрета (`#field`-нотация), Keeper читает KV, выбирает нужное поле и возвращает уже спроецированный скаляр в форме `{ "value": <scalar> }`. Soul получает готовое значение и не парсит секрет целиком — это держит инвариант «Soul не видит лишнего» (минимизация секретного материала на Soul-е, [§безопасности](#требование-безопасности-нормативный-инвариант)).

## 6. Авторизация (Keeper-side)

Решение об удовлетворении `AugurRequest` принимает Keeper. Алгоритм:

1. **Omen существует.** `omens` содержит запись с `name == omen_name`. Иначе → `denied`.
2. **SID → covens.** SID берётся из mTLS peer cert; covens резолвятся из registry (`souls.coven[]`, [storage.md](storage.md)).
3. **Rite найден.** Существует Rite с `omen == omen_name` и субъектом, матчащим запрос: либо `rites.sid == SID`, либо `rites.coven ∈ covens(SID)`. Иначе → `denied`.
4. **Query в allow-list.** `query` ∈ `Rite.allow` (по форме `source_type`: путь в `paths`, query в `queries`, index в `indices`). Иначе → `denied`.
5. **Ветвление.** По `Rite.delegate` и `Omen.source_type`:

   | `delegate` | `source_type` | Действие Keeper-а | `AugurReply.result` |
   |---|---|---|---|
   | `false` | любой | прочитать значение сам (vault `ReadKV` / prom-query / elk-query под master-cred) | `inline_data` |
   | `true` | `vault` | заминтить scoped-токен (`auth/token/create`, TTL/num_uses/policies из Rite) | `scoped_vault_token` |
   | `true` | `prometheus` / `elk` | вернуть pre-scoped read-key из `auth_ref` | `scoped_static_cred` |

Любая неуспешная проверка → `AugurReply{status: denied}` + audit-event `augur.access_denied` (см. [§8](#8-audit)).

### 6.1 Vault-delegate — orphan-токен

Минтуемый scoped Vault-токен создаётся как **orphan** (`no_parent=true`): он не привязан к токену Keeper-инстанса и переживает Keeper-restart / failover. Иначе при ротации/рестарте инстанса Keeper-а токены, выданные Soul-ам, отозвались бы вместе с родителем — это сломало бы in-flight-прогоны на хостах. Trade-off и обоснование — [§9](#9-принятые-дизайн-форки).

### 6.2 Prometheus / ELK delegate через pre-scoped read-key

Для prom / elk-делегации в `omens.auth_ref` оператор кладёт vault-ref на **отдельный pre-scoped read-only read-key** (созданный заранее в самой внешней системе и положенный в Vault), а **не** master-credential. Keeper при `delegate=true` отдаёт Soul-у именно этот ключ как `scoped_static_cred`. Master-credential Keeper-а к Soul-у при этом не попадает — инвариант [§безопасности](#требование-безопасности-нормативный-инвариант) соблюдён.

## Требование безопасности (нормативный инвариант)

> **Soul НИКОГДА не получает master-credential внешней системы.** Soul получает только эфемерный scoped-токен (Vault, `delegate=true`) либо scoped read-only static cred (prom / elk, `delegate=true`), либо вообще не получает credential (`delegate=false` — данные приходят inline через Keeper).

Это нормативный инвариант Augur, не рекомендация. Следствия:

- **MVP-1 (`delegate=false`) границу [ADR-012(d)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) НЕ трогает.** Внешний доступ остаётся за Keeper-ом — ровно как для обычного рендера. На Soul нет ни Vault-токена, ни Vault-клиента.
- **MVP-2 (`delegate=true`) — узкое осознанное исключение из [ADR-012(d)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add).** На Soul появляется **минимальный fetch-клиент** (читает Vault эфемерным токеном / делает read-only-запрос в prom-elk pre-scoped-ключом). Но и тогда: master-credential, `cel-go`, sprig-render-контекст и Vault-токены Keeper-а по-прежнему **не** на Soul. Исключение касается ровно одного: узкого live-fetch с эфемерным scoped credential, никогда — master-доступа.

«Безопасность на первом месте» ([requirements.md](../requirements.md)): дефолт `Rite.delegate = false`; делегация — явный осознанный opt-in оператора на конкретный Rite.

## 7. `source_type` enum

Descriptive closed enum в `omens.source_type`:

| Значение | Внешняя система |
|---|---|
| `vault` | HashiCorp Vault (KV; делегация = минтуемый scoped-токен). |
| `prometheus` | Prometheus (live-query; делегация = pre-scoped read-key). |
| `elk` | Elasticsearch / ELK-стек (index-read; делегация = pre-scoped read-key). |

Расширение enum-а — propose-and-wait + PR в этот файл и [naming-rules.md](../naming-rules.md). Augur **не добавляет** нового значения в audit-`source` enum ([§8](#8-audit)) — live-fetch от Soul попадает в существующую категорию `soul_grpc`.

## 8. Audit

Augur-события пишутся в общий audit-pipeline ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention), [storage.md → audit_log](storage.md#таблица-audit_log)). Имена — convention `<area>.<action>` ([naming-rules.md → Audit-events](../naming-rules.md#audit-events)).

| Событие | Категория `source` | `archon_aid` | `correlation_id` | Когда |
|---|---|---|---|---|
| `augur.fetch_brokered` | `soul_grpc` | `NULL` | `apply_id` | MVP-1 брокер прочитал и вернул значение (`delegate=false`). |
| `augur.token_minted` | `soul_grpc` | `NULL` | `apply_id` | MVP-2 заминтил scoped Vault-токен (`delegate=true`, vault). |
| `augur.cred_issued` | `soul_grpc` | `NULL` | `apply_id` | MVP-2 выдал scoped static cred (`delegate=true`, prom / elk). |
| `augur.access_denied` | `soul_grpc` | `NULL` | `apply_id` | Любая проверка [§6](#6-авторизация-keeper-side) провалена. |
| `omen.created` / `omen.revoked` | `api` / `mcp` | AID из JWT | — | CRUD Omen-а через OpenAPI / MCP. |
| `rite.created` / `rite.revoked` | `api` / `mcp` | AID из JWT | — | CRUD Rite-а через OpenAPI / MCP. |

Live-fetch-события (`augur.*`) — категория **`soul_grpc`** ([ADR-022(b)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)): инициатор — Soul (машинный actor, AID нет → `archon_aid: NULL`), `correlation_id = apply_id`. **Нового значения в `source` enum Augur не вводит.** Секрет-значения в audit-payload не пишутся (secret-masking — [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)): логируется факт + Omen + query, не само значение / токен.

## 9. Принятые дизайн-форки

Решения, прошедшие через пользователя + architect (зафиксированы как принятые, не open Q):

| Форк | Решение | Обоснование |
|---|---|---|
| Rite ↔ Omen lifecycle | `ON DELETE CASCADE` | Rite без Omen-а бессмыслен; удаление Omen-а должно атомарно убрать все grant-ы на него (нет orphan-Rite-ов). |
| Vault-токен parentage | orphan (`no_parent=true`) | Scoped-токен должен пережить Keeper-restart / failover, иначе in-flight-прогоны на хостах ломаются при ротации инстанса. Цена — токен не отзывается каскадом при revoke родителя; компенсируется коротким TTL / num_uses из Rite. |
| prom / elk delegate | отдельный pre-scoped read-key в `auth_ref` (не master) | Для prom / elk нет Vault-style минтинга; делегация без раздачи master-cred возможна только через заранее ограниченный read-key. |
| response-wrapping | post-MVP hardening | Vault response-wrapping минтуемого токена усиливает защиту in-transit, но не блокирует MVP-2; вводится отдельной задачей hardening-а. |

## 10. Реконсиляция с действующими ADR

| ADR | Отношение к Augur |
|---|---|
| [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) (один EventStream) | **Соблюдён** — Augur only-add в существующий `oneof`, нового RPC / стрима нет. |
| [ADR-012(c)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) (forward-compat only-add) | `AugurRequest` / `AugurReply` добавляются only-add в `oneof payload`, новый файл `augur.proto`. |
| [ADR-012(d)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) (граница рендера / внешнего доступа) | MVP-1 границу **не трогает**; MVP-2 (`delegate=true`) — узкое осознанное исключение (минимальный fetch-клиент на Soul; master-cred / cel-go / sprig-контекст по-прежнему не на Soul). |
| [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon) (Vault AppRole — post-MVP) | AppRole, ранее post-MVP, становится **зависимостью MVP-2** (минтинг scoped-токена под master-AppRole). |
| [ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) (`core.vault.kv-read`) | **Обобщается Augur-ом.** `core.vault.kv-read` остаётся для render-фазы (pre-resolve, чтение на этапе рендера scenario); Augur — live-доступ при apply. Два разных момента, не дубль. |

## См. также

- [architecture.md → ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul) — фиксация дизайна, 2-фазность, исключение из ADR-012(d).
- [naming-rules.md → Augur / Omen / Rite](../naming-rules.md#augur-вложенные-proto-типы-и-реестры) — словарь имён, source_type enum, proto-имена, RBAC-perms, audit-events, PG-таблицы.
- [storage.md](storage.md) — Postgres-реестры Keeper-а (куда лягут `omens` / `rites`).
- [cloud.md](cloud.md) — образец реестра Provider / Profile в Postgres, managed через API/MCP.
- [modules.md](modules.md) — `core.vault.kv-read` (render-фаза, обобщается Augur-ом для live-доступа).
- [rbac.md](rbac.md) — RBAC-perms (`omen.*` / `rite.*`).
- [operator-api.md](operator-api.md) — OpenAPI-сторона CRUD Omen / Rite (старт как stub-каталог).
- [mcp-tools.md](mcp-tools.md) — MCP-tools для Omen / Rite (старт как stub-каталог).
- [architecture.md → ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) — контракт EventStream (only-add, граница рендера).
- [requirements.md](../requirements.md) — «безопасность на первом месте», интеграция с Vault из коробки.
</content>
</invoke>
