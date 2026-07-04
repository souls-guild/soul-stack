# Operator API — HTTP-фасад Keeper-а

Нормативная спецификация HTTP-эндпоинтов Keeper-а для операторов (Архонтов). Каждый endpoint жёстко привязан 1:1 к permission из [rbac.md → Каталог permissions](rbac.md#каталог-permissions) и к MCP-tool с именем `keeper.<resource>.<action>` ([rbac.md → Permission ↔ MCP-tool / OpenAPI endpoint](rbac.md#permission--mcp-tool--openapi-endpoint)).

Этот документ задаёт **conventions + mapping + ключевые request/response schemas**. Источник правды по **форме** Operator API (paths, тела, схемы) — **Go-типы handler-ов** (huma v2 full-typed, code-first): request/reply описаны Go-struct-ами с huma-тегами в `keeper/internal/api/huma_*.go`, а OpenAPI-спека — **производная** (huma-агрегатор `HumaFullSpecYAML`, см. [§ Полная OpenAPI YAML](#полная-openapi-yaml)). Spec-first-каркас `oapi-codegen` + пакет `keeper/internal/api/oapi` **упразднён** ([ADR-054](../adr/0054-openapi-code-first.md#adr-054-operator-api--разворот-на-code-first-go-типы--openapi-через-huma-v2) заменил [ADR-051](../adr/0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-типы-oapi-codegen-types-only--strict)). Operator API — REST/HTTP, gRPC-сервиса нет. Прежний источник формы `proto/operator/v1` **упразднён** ранее (аменд [ADR-011](../adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)).

## Зачем отдельный фасад

Soul Stack выставляет **три транспорта** наружу, и Operator API — один из них:

| Транспорт | Кто пользователь | Контракт | Listener |
|---|---|---|---|
| **OpenAPI (HTTP/JSON)** — этот документ | Архонты (люди + machine-identity, JWT) | Go-типы huma full-typed (`keeper/internal/api/huma_*.go`); OpenAPI-спека — производная (huma-агрегатор → [`openapi.yaml`](openapi.yaml)) | `listen.openapi.addr` ([config.md](config.md)) |
| **MCP** | LLM-агенты от лица Архонта | те же tools, 1:1 с endpoints | `listen.mcp.addr` ([config.md](config.md)) |
| **gRPC bidi Keeper↔Soul** | `soul`-бинари (mTLS) | `proto/keeper/v1/` ([ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)) | `listen.grpc.bootstrap.addr` (server-only TLS, онбординг) + `listen.grpc.event_stream.addr` (mTLS, долгоживущий стрим) |

Operator API **не пересекается** с Keeper↔Soul gRPC: Souls не ходят в OpenAPI, операторы не ходят в gRPC bidi. Аутентификация и identity-модель тоже разные — JWT/Archon для оператора ([ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) vs mTLS/SoulSeed для Soul ([`../soul/identity.md`](../soul/identity.md)).

## Conventions

| Decision | Value | Rationale |
|---|---|---|
| **URL prefix** | `/v1/` | Короткий, явный. Симметрия с `proto/keeper/v1/` / `proto/plugin/v1/`. Health/meta/docs-эндпоинты (`/healthz`, `/readyz`, `/metrics`, `/openapi.yaml`, `/docs`, `/docs/assets/*`) — **вне** `/v1/`-префикса, чтобы не зависеть от мажорной версии API. |
| **Версионирование** | `v1` в URL; breaking changes — только `/v2/`. Forward-compat only-add внутри `v1` (новые поля — `optional`, удаление полей запрещено). Симметрия с ADR-012(g) для gRPC. |
| **Auth** | `Authorization: Bearer <jwt>` — обязателен для всего `/v1/*`. Формат JWT — [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon). См. [§ Auth](#auth). |
| **Bootstrap-bypass** | `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)) — administrative subcommand на самом keeper-инстансе, **не HTTP**. OpenAPI-фасад в bootstrap-моменте не задействован: первый Архонт создаётся напрямую в Postgres под PG advisory lock, JWT кладётся в файл `mode 0400`. Все последующие операторы — через `POST /v1/operators` с JWT родительского Архонта. |
| **Content-Type request** | `application/json` | Стандарт. |
| **Content-Type response success** | `application/json` | Стандарт. |
| **Content-Type response error** | `application/problem+json` | RFC 7807 Problem Details. См. [§ Error format](#error-format-rfc-7807). |
| **Resource naming в path** | Plural lowercase kebab-case: `/v1/incarnations`, `/v1/operators`, `/v1/souls`, `/v1/push-providers`, `/v1/push`. | Стандарт REST. |
| **JSON field naming** | `snake_case` для всех полей request/response body. Go-типы тел (huma full-typed handler-struct-ы) несут `json:"<snake_case>"`-теги и сериализуются стандартным `encoding/json`; те же теги питают huma-схему производной [`openapi.yaml`](openapi.yaml). | Совпадает с `keeper.yml` / `soul.yml` convention; даёт читаемые JSON, симметричные `components/schemas` в [`openapi.yaml`](openapi.yaml). |
| **Enum serialization** | В JSON API enum-значения — короткие lowercase-формы без family-prefix (`"ready"` / `"connected"` / `"agent"`). Канонический список значений каждого enum задаётся в Go-коде — native enum-каталог `keeper/internal/api/huma_enums.go` (например `IncarnationStatus`); `enum: […]` в производной [`openapi.yaml`](openapi.yaml) — его huma-генерат. Историческая ремарка «proto-константа имела бы вид `INCARNATION_STATUS_READY`» оставлена в описаниях схем для контекста, но в wire-формате не используется. | Короткие формы в JSON — convention этого API; источник перечня — Go-каталог, спека производна. |
| **Schema names** | `CamelCase` для имён схем в `components/schemas` производной [`openapi.yaml`](openapi.yaml) (`OperatorCreateRequest`, `IncarnationGetReply`, `ProblemDetails`); имена схем выводятся huma из одноимённых Go-struct-ов handler-ов (`keeper/internal/api/huma_*.go`). | Стандарт OpenAPI. |
| **ID в path** | `name` для Incarnation (`/v1/incarnations/{name}`), AID для Operator (`/v1/operators/{aid}`, regex `^[a-z0-9][a-z0-9._@-]{1,127}$` из [naming-rules.md → Идентификаторы](../naming-rules.md#идентификаторы); AID может содержать `.`/`@` для email-подобных внешних имён — в path-сегменте они **URL-encoded**, как и FQDN-SID), SID для Soul (FQDN). SID используется в path в `/v1/souls/{sid}/issue-token` (sub-resource action) — FQDN **URL-encoded** в path-сегменте (точки допустимы в path без экранирования; экранируются лишь зарезервированные символы по RFC 3986, которых в валидном FQDN нет). В list-ответе SID отдаётся как есть. Read-by-SID (`GET /v1/souls/{sid}`) остаётся отложенным — нет permission `soul.get`. |
| **Pagination** | Query `offset` (int, ≥0, default `0`) + `limit` (int, 1..1000, default `50`). Ответ list-эндпоинта — `{items: [...], offset, limit, total}`. Cursor-pagination — post-MVP при необходимости. |
| **Async operations** | См. [§ Async operations](#async-operations) ниже. |
| **Status codes** | `200` (sync read/update), `201` (POST resource created), `202` (async accepted), `204` (delete/revoke без body), `400` (malformed JSON/syntactic), `401` (no/invalid JWT), `403` (RBAC deny), `404` (not found), `409` (conflict — `error_locked`, self-lockout invariant), `422` (validation error — semantic), `500` (internal error). |
| **Time format** | ISO-8601 / RFC 3339 в UTC: `"2026-05-20T15:30:00Z"`. |
| **Duration format** | Go-duration string в JSON (`"30s"`, `"24h"`) для symmetry с [config.md](config.md). |
| **Tracing** | Каждый запрос получает OTel-span ([requirements.md](../requirements.md)); `traceparent`/`tracestate` headers пробрасываются. Атрибут `archon.aid=<aid>` пишется после JWT-аутентификации ([ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). |

### Async operations

Эндпоинты, запускающие длительные прогоны (создание/изменение incarnation, push), возвращают `202 Accepted` + body `{"apply_id": "<ULID>"}` (ULID — `proto/keeper/v1/apply.proto → ApplyRequest.apply_id`).

Опрос статуса в MVP — через два эндпоинта:

- `GET /v1/incarnations/{name}` — текущий `status` (`ready`/`applying`/`error_locked`/`migration_failed`/...) и `status_details`.
- `GET /v1/incarnations/{name}/history` — записи `state_history` с полем `apply_id`; запись с конкретным `<ULID>` появляется после успешного коммита. Polling-клиенты могут передать `?apply_id=<ULID>` для прямого поиска row-а конкретного прогона (см. ниже).

Отдельного `/v1/applies/{apply_id}` эндпоинта **в MVP нет** — соответствующих `apply.*` permissions в каталоге [rbac.md](rbac.md#каталог-permissions) тоже нет. Появится отдельной задачей при необходимости.

Симметрично в MCP — см. [mcp-tools.md → `_apply_id`-convention](mcp-tools.md#async-operations-в-mcp).

## Auth

JWT Bearer — единственный auth-метод в MVP ([ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).

```
Authorization: Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJrZWVwZXItZXUtd2VzdC0wMSIsInN1YiI6ImFyY2hvbi1hbGljZSIsImlhdCI6MTcxNjIwNjIwMCwiZXhwIjoxNzE2MjkyNjAwLCJyb2xlcyI6WyJjbHVzdGVyLWFkbWluIl19.<signature>
```

### Claims

| Claim | Тип | Смысл |
|---|---|---|
| `iss` | `string` | `keeper.yml → auth.jwt.issuer` (default — KID инстанса, [config.md → auth](config.md#auth)). |
| `sub` | `string` (AID) | AID Архонта; FK на `operators(aid)` в Postgres. |
| `iat` | `int` (unix-сек) | Время выпуска. |
| `exp` | `int` (unix-сек) | Срок истечения. TTL по умолчанию — `auth.jwt.ttl_default` (`24h`) для обычных, `auth.jwt.ttl_bootstrap` (`720h` = 30 дней) для bootstrap-токена. |
| `roles` | `list<string>` | Список ролей из `keeper.yml → rbac.roles[]`. Источник правды — реестр на момент аутентификации запроса, claim — кеш для быстрых решений. |
| `bootstrap_initial` | `bool` (optional) | `true` для первого Архонта, выпущенного `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)). Используется в аудите. |

### Bootstrap-bypass

Bootstrap первого Архонта **не идёт через OpenAPI**: команда `keeper init --archon=<aid>` выполняется на самом keeper-инстансе оператором с доступом к keeper-хосту. Реестр `operators` пуст → создаётся первый Архонт под PG advisory lock → JWT пишется в файл `mode 0400`. Этот JWT — единственный путь оператора в Operator API после bootstrap. Подробности — [rbac.md → Bootstrap первого Архонта](rbac.md#bootstrap-первого-архонта).

После bootstrap Operator API становится единственным каналом создания дополнительных операторов (`POST /v1/operators` с permission `operator.create`).

### Served-spec `/openapi.yaml` + `/openapi.json` за JWT

`GET /openapi.yaml` (вне `/v1`) с 2026-06-15 требует валидный Bearer-токен — required `RequireJWT`, но без `/v1`-обвязки (RBAC/audit/maxBody/metrics), без permission-check (любой аутентифицированный Архонт читает спеку). Раньше served-spec был публичен; смена усиливает безопасность — анонимный посетитель не может выгрузить полную карту эндпоинтов ([ADR-054 §OpenAPI-вьювер](../adr/0054-openapi-code-first.md#adr-054-operator-api--разворот-на-code-first-go-типы--openapi-через-huma-v2)). Без токена → `401 application/problem+json`. Рядом отдаётся JSON-вариант той же спеки — `GET /openapi.json` (за тем же JWT): его инлайн-рендерит RapiDoc-вьювер `/docs` (RapiDoc `loadSpec` принимает разобранный объект, а не URL — url-фетч не нёс бы наш Bearer и упёрся в 401). `/openapi.yaml` остаётся для людей и тулов. Визуальный вьювер `GET /docs` загружает спеку отдельным `fetch /openapi.json` с Bearer-заголовком (механизм A — см. [§ Health / Meta / Docs](#health--meta--docs)). UI-vendor (soul-stack-web) и `soulctl` не затронуты — они потребляют **committed** [`openapi.yaml`](openapi.yaml), а не live-served.

### Secret masking в логах и трейсах

JWT-токены, возвращаемые в response body `POST /v1/operators` / `POST /v1/operators/{aid}/issue-token`, **не пишутся** в OTel span attributes, access-логи или audit-trail. Маскинг на выходе по правилу:

- Header `Authorization: Bearer <jwt>` — заменяется на `Bearer ***`.
- JSON-поля `jwt`, `token`, `bootstrap_token`, `private_key`, `password`, `credentials_ref`, `secret`, `signing_key`, `signing_key_ref` — заменяются на `"***"` перед записью в любой observability-канал. `signing_key` / `signing_key_ref` — расширение по факту использования suffix-а в `keeper.yml → auth.jwt.signing_key_ref` и симметрично masked-keys в audit-pipeline (`shared/audit.maskedKeys`, ADR-022(k)).
- Vault-ref значения (формат `vault:<path>`) — заменяются на `"vault:***"` (путь скрыт, признак vault-ref сохранён для диагностики).

Конкретная implementation — middleware на OTel-exporter / log-pipeline; нормирование списка masked-полей — отдельная задача security перед релизом.

#### Маскинг `state` / `spec` в GET-ответах (defense-in-depth)

Маскинг распространяется не только на observability-каналы, но и на **сам JSON-ответ** чтения incarnation:

- `GET /v1/incarnations/{name}` и `GET /v1/incarnations` — поля `state` и `spec` прогоняются через `shared/audit.MaskSecrets` перед сериализацией.
- `GET /v1/incarnations/{name}/history` — поля `state_before` и `state_after` прогоняются через тот же маскинг.

Правило — то же, что и для observability (substring-match по sensitive-ключу: `token`/`secret`/`password`/`private_key`/… и vault-ref-маркер `vault:secret/`); чувствительные значения заменяются на `***MASKED***`, несекретные поля и структура объекта сохраняются.

**Маскируется только ответ — в Postgres `state`/`spec` хранятся без изменений** (last known-good для apply / миграций / unlock не должен зависеть от маскинга на чтении). Это второй слой защиты: первый — что чувствительные значения из задач с `no_log: true` физически не попадают в `incarnation.state` (probe-register no_log-задачи не аккумулируется в state-граф, [scenario/orchestration.md §7.1](../scenario/orchestration.md#71-грамматика-state_changes--список-crud-операций)).

> **Важно для клиента:** JWT, возвращаемый в response body `POST /v1/operators` / `POST /v1/operators/{aid}/issue-token`, **отдаётся один раз** и нигде не сохраняется. Оператор обязан надёжно сохранить токен сразу при получении (file `mode 0400` или secret-manager); потерянный токен восстановить нельзя, только выпустить новый через `operator.issue-token` (от другого Архонта с этим правом).

### Self-lockout invariant

Нельзя удалить или ревокнуть последнего Архонта с активной `*`-permission ([rbac.md → Встроенные роли](rbac.md#встроенные-роли)). При попытке через `POST /v1/operators/{aid}/revoke` API возвращает `409 Conflict` с `type: would-lock-out-cluster`.

## Error format (RFC 7807)

Все ошибки `4xx`/`5xx` отдаются как `application/problem+json`:

```json
{
  "type": "https://soul-stack.io/errors/incarnation-locked",
  "title": "Incarnation is in error_locked state",
  "status": 409,
  "detail": "Incarnation 'redis-prod' is locked after failed apply 01HABCDEFGHJKMNPQRSTVWXYZ; use POST /v1/incarnations/{name}/unlock first",
  "instance": "/v1/incarnations/redis-prod/scenarios/restart"
}
```

| Поле | Тип | Смысл |
|---|---|---|
| `type` | `string` (URI) | Стабильный URN ошибки для machine-parsing. Список — [§ Типы ошибок](#типы-ошибок). |
| `title` | `string` | Краткий заголовок (английский, фиксированный для каждого `type`). |
| `status` | `int` | HTTP status code (дублируется для удобства клиента). |
| `detail` | `string` | Человекочитаемое сообщение, может содержать значения (имена, ULID-ы, причины); свободный текст. |
| `instance` | `string` (URI) | Конкретный URI запроса, который привёл к ошибке. |

### Типы ошибок

Все `type` — стабильные URN под доменом `https://soul-stack.io/errors/`. Расширение списка — only-add (новые типы можно добавлять, существующие не переименовывать).

| `type` URN suffix | HTTP | Смысл | Где возникает |
|---|---|---|---|
| `unauthenticated` | 401 | JWT отсутствует, не валиден или истёк. | Любой `/v1/*` endpoint. |
| `forbidden` | 403 | RBAC-проверка не прошла. `detail` содержит требуемую permission и контекст. | Любой `/v1/*` endpoint после JWT-аутентификации. |
| `not-found` | 404 | Ресурс не существует. | Любой endpoint с path-param. |
| `validation-failed` | 422 | Семантическая ошибка валидации: incarnation input не матчит scenario `input:`-схему ([destiny/input](../input.md)); `create_scenario` в `POST /v1/incarnations` пуст при наличии create-сценариев (`detail` начинается с `create_scenario_required:` + перечисление годных) или указывает на сценарий вне create-набора (`create_scenario_invalid:`, см. [operator-api/incarnations.md → Выбор стартового сценария](operator-api/incarnations.md#выбор-стартового-сценария-и-bare-инкарнация)); profile `params` не матчит CloudDriver `profile_schema` ([cloud.md](cloud.md)); запрос `issue-token` для Soul с `transport: ssh` (ssh-хост не имеет bootstrap-фазы — `POST /v1/souls/{sid}/issue-token`). `detail` — путь до конкретного поля или причина. |
| `assert-failed` | 422 | Scenario `assert:`-предикат не прошёл на pre-flight-гейте СОЗДАНИЯ прогона ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)/[ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) amendment 2026-06-23, форма A): roster прогона (connected souls по Coven incarnation) не сходится с инвариантом топологии сценария — напр. cluster size-guard `size(soulprint.hosts) == shards*(1+replicas_per_shard)`. **Incarnation НЕ создаётся**, fail-статус (`error_locked`) НЕ ставится — отказ на этапе модели ДО коммита. `detail` — `message` assert-задачи + текст непрошедшего предиката. Отдельный URN от `validation-failed`: «топология не сходится» ≠ «поле input не матчит схему». | `POST /v1/incarnations` (когда сценарий `create` несёт `assert:`-задачу). |
| `malformed-request` | 400 | Синтаксис JSON / неверные query params. |
| `incarnation-locked` | 409 | Incarnation в `error_locked` — нужен `POST /v1/incarnations/{name}/unlock` перед новым прогоном ([architecture.md → Атомарность и `error_locked`](../architecture.md#атомарность-и-error_locked)). Для `rerun-last` — также «статус не `error_locked`» (нечего перезапускать). | `POST /v1/incarnations/{name}/scenarios/{scenario}`, `DELETE /v1/incarnations/{name}`, `POST /v1/incarnations/{name}/upgrade`, `POST /v1/incarnations/{name}/rerun-last`. |
| `rerun-input-unavailable` | 409 | `rerun-last` не может восстановить input упавшего day-2-прогона (fail-closed): `apply_runs.recipe` недоступен — прогон упал до dispatch (render_failed / no_hosts / pre-flight, терминальная строка записана без рецепта), рецепт вычищен ретеншном Reaper (`purge_apply_runs`) либо legacy-прогон без сохранённого рецепта (`recipe IS NULL`). Отдельный URN от `incarnation-locked` (machine-readable различие «input утрачен → `unlock` + ручной `run` с явным input» от «статус не `error_locked`»). | `POST /v1/incarnations/{name}/rerun-last`. |
| `migration-failed` | 409 | Incarnation в `migration_failed` — нужен ручной разбор state_history ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). | `POST /v1/incarnations/{name}/scenarios/{scenario}`, `POST /v1/incarnations/{name}/upgrade`. |
| `would-lock-out-cluster` | 409 | Операция оставила бы кластер без активного Архонта с эффективным `*`-permission (эффективный `*` может приходить через Synod). | `POST /v1/operators/{aid}/revoke`; role-операции (`DELETE /v1/roles/{name}`, `PATCH /v1/roles/{name}/permissions`, `DELETE /v1/roles/{name}/operators/{aid}`); synod-операции (`DELETE /v1/synods/{name}`, `DELETE /v1/synods/{name}/operators/{aid}`, `DELETE /v1/synods/{name}/roles/{role_name}`). |
| `synod-not-found` | 404 | Synod-группы с указанным `name` нет в реестре `synods` ([ADR-049](../adr/0049-synod.md#adr-049-synod--группа-архонов)). | `PATCH /v1/synods/{name}`, `DELETE /v1/synods/{name}`, `POST /v1/synods/{name}/operators`, `POST /v1/synods/{name}/roles`. |
| `synod-already-exists` | 409 | `name` Synod-группы уже занят в реестре `synods`. | `POST /v1/synods`. |
| `synod-builtin` | 409 | Группа с `builtin=true` — удалять нельзя (проверка **до** self-lockout). | `DELETE /v1/synods/{name}`. |
| `incarnation-already-exists` | 409 | Incarnation с указанным `name` уже создан. | `POST /v1/incarnations`. |
| `operator-already-exists` | 409 | AID уже занят в реестре `operators`. | `POST /v1/operators`. |
| `soul-already-exists` | 409 | SID уже зарегистрирован в реестре `souls`. | `POST /v1/souls`. |
| `bootstrap-token-active` | 409 | У Soul уже есть активный (не сожжённый, не истёкший) bootstrap-токен — инвариант `UNIQUE (sid) WHERE used_at IS NULL`. Повторный выпуск — с `?force=true` (expire старого + reissue). | `POST /v1/souls/{sid}/issue-token`. |
| `service-not-registered` | 422 | `service` в `POST /v1/incarnations` отсутствует в `keeper.yml → services[]` ([config.md → services](config.md#services--default_destiny_source--default_module_source)). |
| `sigil-key-not-found` | 404 | Ключа подписи Sigil с таким `key_id` нет ([ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)). | `POST /v1/sigil/keys/{key_id}/primary`, `DELETE /v1/sigil/keys/{key_id}`. |
| `sigil-key-last-active` | 409 | Нельзя вывести последний active-ключ подписи — набор не должен опустеть. | `DELETE /v1/sigil/keys/{key_id}`. |
| `sigil-key-primary` | 409 | Нельзя вывести primary-ключ напрямую — сперва `POST .../primary` другому active. | `DELETE /v1/sigil/keys/{key_id}`. |
| `sigil-key-concurrent-change` | 409 | Гонка установки primary (one_primary index) либо ключ retired при set-primary; retry. | `POST /v1/sigil/keys`, `POST /v1/sigil/keys/{key_id}/primary`. |
| `omen-already-exists` | 409 | `name` Omen-а уже занят в реестре `omens` ([ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul)). | `POST /v1/augur/omens`. |
| `tempo-exceeded` | 429 | Per-AID rate-limit [Tempo](config.md#tempo) превышен — оператор слишком часто дёргает resolver-тяжёлый write-эндпоинт ([ADR-050](../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)). Ответ несёт заголовок **`Retry-After`** (секунды до пополнения хотя бы одного токена). Лимит per-AID (по `claims.Subject`), не cluster-wide. | `POST /v1/voyages`, `POST /v1/voyages/preview`. |
| `internal-error` | 500 | Незапланированная ошибка. `detail` — generic; полная диагностика только в логах/OTel-trace, который клиент может найти по `traceparent`-header в ответе. |

## Pagination

Применяется ко всем list-эндпоинтам (`GET /v1/incarnations`, `GET /v1/souls`).

**Request — query params:**

| Param | Тип | Default | Смысл |
|---|---|---|---|
| `offset` | `int` (≥0) | `0` | Сдвиг от начала набора. |
| `limit` | `int` (1..1000) | `50` | Размер страницы. `> 1000` → `400 malformed-request`. |

**Response:**

```json
{
  "items": [ /* ... */ ],
  "offset": 0,
  "limit": 50,
  "total": 137
}
```

`total` — общее количество элементов с учётом фильтров (если у endpoint-а есть фильтры). Клиент итерирует, увеличивая `offset` на `limit`, пока `offset + len(items) < total`.

Cursor-pagination не вводим — для admin-API offset/limit достаточно (количество incarnation-ов и souls в MVP-инсталляциях — единицы тысяч). Расширение возможно отдельным PR без breaking change (добавление опционального `cursor`-параметра).

## Mapping endpoint ↔ MCP-tool ↔ permission

Сводная таблица. Источник правды по permissions — [rbac.md → Каталог permissions](rbac.md#каталог-permissions). 1:1 mapping обеспечен по построению — каждое имя permission `<resource>.<action>` детерминирует MCP-tool (`keeper.<resource>.<action>`) и endpoint path/method.

Нормирование MCP-стороны (формат tool declaration, transport, auth, input/output schemas, async-convention, error mapping) — в [mcp-tools.md](mcp-tools.md).

### Health / Meta / Docs

На самом API-фасаде ([`router.go`](../../keeper/internal/api/router.go)) смонтированы **6 meta-роутов**: `/healthz`, `/readyz`, `/openapi.yaml`, `/openapi.json`, `/docs`, `/docs/assets/*`. `/metrics` физически **не на фасаде** — он на выделенном metrics-listener-е (`listen.metrics.addr`, [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)); в таблице ниже приведён для полноты обзора meta-поверхности, но в фасадный роут-счёт не входит.

**Auth-граница (механизм A, [ADR-054 §OpenAPI-вьювер](../adr/0054-openapi-code-first.md#adr-054-operator-api--разворот-на-code-first-go-типы--openapi-через-huma-v2)):**

- **Публичны (без auth):** `/healthz`, `/readyz`, `/docs` (shell-каркас вьювера), `/docs/assets/*` (вшитая статика RapiDoc). Эти роуты не несут данных и не раскрывают API-поверхность.
- **За JWT:** `/openapi.yaml` и `/openapi.json` — сменили security публичный → required Bearer (тот же `RequireJWT`, что и `/v1`, но без `/v1`-обвязки RBAC/audit/maxBody/metrics; mount вне `/v1`). Усиление: **нет анонимной разведки полной API-поверхности**.

| Method | Path | Listener | Auth | Назначение | Response |
|---|---|---|---|---|---|
| `GET` | `/healthz` | API-фасад | публичный | Liveness — процесс жив. | `200 {"status": "ok"}` |
| `GET` | `/readyz` | API-фасад | публичный | Readiness — Postgres + Vault достижимы. | `200 {"status": "ok"}` при ready; `503 {"status": "not_ready", "checks": {...}}` при not-ready. |
| `GET` | `/openapi.yaml` | API-фасад | **JWT (Bearer)** | Self-served OpenAPI 3.1 спека — **runtime-дамп huma-агрегатора из кода** (`servedOpenAPIHandler` → `HumaFullSpecYAML`, кеш собирается один раз). Не embed: committed [`openapi.yaml`](openapi.yaml) — производный снимок для UI-vendor/`soulctl`, served-spec собирается из Go-кода. Без валидного JWT → `401 application/problem+json` (`unauthenticated`). | `200` (`application/yaml` по RFC 9512, 2024 — современнее legacy `text/yaml`). |
| `GET` | `/openapi.json` | API-фасад | **JWT (Bearer)** | Та же self-served OpenAPI 3.1 спека в JSON (`servedOpenAPIJSONHandler`) — для inline-рендера RapiDoc-вьювером `/docs` (`loadSpec` принимает разобранный объект). YAML-вариант `/openapi.yaml` остаётся для людей и тулов. Без валидного JWT → `401 application/problem+json` (`unauthenticated`). | `200 application/json`. |
| `GET` | `/docs` | API-фасад | публичный (shell) | Визуальный OpenAPI-вьювер ([RapiDoc](https://rapidocweb.com/), web-component, go:embed-ассеты) со встроенным full-text-поиском по эндпоинтам (`allow-advanced-search`). Публичный HTML-shell с полем ввода Archon-JWT; сама спека грузится отдельным `fetch /openapi.json` с Bearer и рендерится inline (`loadSpec(объект)` — RapiDoc трактует строку как spec-URL и фетчит её без нашего Bearer). Тот же JWT прокидывается в RapiDoc «Try It» (`setApiKey(bearerAuth, …)`). Токен — в `sessionStorage` (per-tab), Bearer-заголовком (не в URL). Cookies/sessions не вводятся (консистентно [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). См. [`docs_viewer.go`](../../keeper/internal/api/docs_viewer.go). | `200 text/html`. |
| `GET` | `/docs/assets/*` | API-фасад | публичный | Вшитая статика RapiDoc (один файл `rapidoc-min.js`, ~843 КБ, go:embed из `keeper/internal/api/docsassets/`; стили RapiDoc — в Shadow DOM, отдельного CSS нет). Не CDN — air-gapped/офлайн-инсталляции. | `200` (Content-Type по расширению). |
| `GET` | `/metrics` | metrics-listener ([ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)) | опц. basic-auth | Prometheus exposition format. | `200` (`text/plain; version=0.0.4`). См. [config.md → listen.metrics](config.md#listen). |

Health-paths выбраны в стиле kubernetes-convention (`/healthz`, `/readyz`); роут вьювера `/docs` — индустрия-стандартное имя (осознанное исключение из словаря Soul Stack, [naming-rules.md](../naming-rules.md)). Все они явно **нет** под `/v1/`-префиксом — независимы от мажорной версии Operator API.

### Operator (5) — управление Архонтами, [ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) / [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/operators` | `operator.create` | `keeper.operator.create` |
| `GET` | `/v1/operators` | `operator.list` | — (UI iter 2) |
| `GET` | `/v1/operators/{aid}` | `operator.list` | — (UI iter 2) |
| `POST` | `/v1/operators/{aid}/revoke` | `operator.revoke` | `keeper.operator.revoke` |
| `POST` | `/v1/operators/{aid}/issue-token` | `operator.issue-token` | `keeper.operator.issue-token` |

Read-эндпоинты (`GET /v1/operators`, `GET /v1/operators/{aid}`) добавлены в UI iteration 2 (placeholder `/archons-list` / `/archons/:aid`). MCP-tool-симметрия отложена до следующего slice. Read-only — без audit-trail (паттерн `soul.list`).

### Role (6) — RBAC-CRUD (роли / permissions / membership), [ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) / [rbac.md](rbac.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/roles` | `role.create` | `keeper.role.create` |
| `GET` | `/v1/roles` | `role.list` | `keeper.role.list` |
| `DELETE` | `/v1/roles/{name}` | `role.delete` | `keeper.role.delete` |
| `PATCH` | `/v1/roles/{name}/permissions` | `role.update` | `keeper.role.update` |
| `POST` | `/v1/roles/{name}/operators` | `role.grant-operator` | `keeper.role.grant-operator` |
| `DELETE` | `/v1/roles/{name}/operators/{aid}` | `role.revoke-operator` | `keeper.role.revoke-operator` |

1:1 `keeper.role.<action>` ↔ permission `role.<action>` ↔ endpoint. `role.*` — NoSelector (cluster-уровневая операция без coven/host-scope, как `operator.*`/`synod.*`). Источник правды по семантике, телам и кодам ошибок (`role-already-exists`, `role-builtin`, `would-lock-out-cluster`) — [rbac.md → REST `/v1/roles`](rbac.md); MCP-сторона — [mcp-tools/roles.md](mcp-tools/roles.md). Мутирующие 5 роутов аудируются (изменение авторизации, [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `role.list` — read-only, без audit. `PATCH .../permissions` — replace-семантика набора permissions роли (не merge).

### Synod (8) — управление группами архонов, [ADR-049](../adr/0049-synod.md#adr-049-synod--группа-архонов)

Источник правды по семантике, телам и кодам ошибок CRUD — [rbac.md → REST `/v1/synods`](rbac.md#rest-v1synods); MCP-сторона — [mcp-tools/synods.md](mcp-tools/synods.md). `synod.*` — NoSelector.

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/synods` | `synod.create` | `keeper.synod.create` |
| `GET` | `/v1/synods` | `synod.list` | `keeper.synod.list` |
| `PATCH` | `/v1/synods/{name}` | `synod.update` | `keeper.synod.update` |
| `DELETE` | `/v1/synods/{name}` | `synod.delete` | `keeper.synod.delete` |
| `POST` | `/v1/synods/{name}/operators` | `synod.add-operator` | `keeper.synod.add-operator` |
| `DELETE` | `/v1/synods/{name}/operators/{aid}` | `synod.remove-operator` | `keeper.synod.remove-operator` |
| `POST` | `/v1/synods/{name}/roles` | `synod.grant-role` | `keeper.synod.grant-role` |
| `DELETE` | `/v1/synods/{name}/roles/{role_name}` | `synod.revoke-role` | `keeper.synod.revoke-role` |

`PATCH /v1/synods/{name}` (ADR-049 amend) меняет **ТОЛЬКО `description`** группы (body `{description}`, required, 1..1024 символов); `name` (PK) immutable. Коды: `204` (успех), `404 synod-not-found` (группы нет), `422 validation-failed` (пустой `description` / превышение лимита), `400 malformed-request` (битый JSON / неизвестное поле — в т.ч. `name` в теле). builtin-группа редактируется (`description` косметика, без subset/self-lockout). Audit-event `synod.updated`.

### Incarnation (15) — жизненный цикл runtime-инстансов, [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/incarnations` | `incarnation.create` | `keeper.incarnation.create` |
| `POST` | `/v1/incarnations/{name}/rerun-last` | `incarnation.rerun-last` | `keeper.incarnation.rerun-last` |
| `POST` | `/v1/incarnations/{name}/scenarios/{scenario}` | `incarnation.run` | `keeper.incarnation.run` |
| `POST` | `/v1/incarnations/{name}/scenarios/{scenario}/form-prefill` | `incarnation.get` | — (REST only) |
| `GET` | `/v1/incarnations/{name}` | `incarnation.get` | `keeper.incarnation.get` |
| `GET` | `/v1/incarnations` | `incarnation.list` | `keeper.incarnation.list` |
| `GET` | `/v1/incarnations/{name}/history` | `incarnation.history` | `keeper.incarnation.history` |
| `GET` | `/v1/incarnations/{name}/runs` | `incarnation.history` | — (REST only) |
| `GET` | `/v1/incarnations/{name}/runs/{apply_id}` | `incarnation.history` | — (REST only) |
| `POST` | `/v1/incarnations/{name}/unlock` | `incarnation.unlock` | `keeper.incarnation.unlock` |
| `POST` | `/v1/incarnations/{name}/upgrade` | `incarnation.upgrade` | `keeper.incarnation.upgrade` |
| `POST` | `/v1/incarnations/{name}/check-drift` | `incarnation.check-drift` | `keeper.incarnation.check-drift` |
| `DELETE` | `/v1/incarnations/{name}` | `incarnation.destroy` | `keeper.incarnation.destroy` |
| `PATCH` | `/v1/incarnations/{name}/hosts` | `incarnation.update-hosts` | — (REST only) |
| `PUT` | `/v1/incarnations/{name}/traits` | `incarnation.traits-set` | `keeper.incarnation.traits-set` |

`PATCH /v1/incarnations/{name}/hosts` редактирует declared `spec.hosts[]` (UI Hosts editing, [ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). Permission `incarnation.update-hosts` (сужена с прежней `incarnation.update`, PM-decision 2026-06-02; backcompat-alias `incarnation.update` канонизируется в `incarnation.update-hosts` на load снимка), scope-селектор `incScope` (паритет run/upgrade/destroy). Audit `incarnation.hosts_updated` пишет сам handler (payload — old/new snapshot). MCP-tool пока **нет** (REST-only; `manifest.go` не содержит `keeper.incarnation.hosts.update`).

`PUT /v1/incarnations/{name}/traits` целостно заменяет operator-set trait-метки инкарнации (`incarnation.traits` jsonb — источник истины, [ADR-060](../adr/0060-traits.md) R1 slice a), проецируемые sync-hook-ом в `souls.traits` хостов-членов. Permission `incarnation.traits-set` (scope incarnation/coven/service по path-`name`, как `update-hosts`). Audit `incarnation.traits_changed` пишет сам handler (payload — только КЛЮЧИ old/new, не значения). MCP-зеркало — `keeper.incarnation.traits-set`. Заменяет per-soul `POST /v1/souls/traits` (deprecated, см. [Soul](#soul-8--реестр-хостов)). Деталь — [operator-api/incarnations.md → PUT .../traits](operator-api/incarnations.md#put-v1incarnationsnametraits--заменить-trait-метки-инкарнации).

`GET /v1/incarnations/{name}/runs` + `GET …/runs/{apply_id}` — read-view прогонов инкарнации (свёртка `apply_runs` по `apply_id`: агрегатный статус `applying`/`success`/`failed`/`cancelled` + per-host детали с адресом упавшей задачи), под UI «статус выполнения / текущая джоба». Прогон (apply_run) — НЕ Voyage. Permission `incarnation.history` (reuse read-tier: кто видит историю инкарнации, тот видит и её прогоны); гейт — existence-`RequireAction`, per-`{name}` scope — in-handler (вне Purview-scope → `404`, parity History). Read-only, без audit, **REST-only** (MCP-tool-ов нет). Деталь — [operator-api/incarnations.md → GET .../runs](operator-api/incarnations.md#get-v1incarnationsnameruns--список-прогонов-инкарнации).

`POST /v1/incarnations/{name}/scenarios/{scenario}/form-prefill` — day-2 pre-fill UI-формы сценария из `incarnation.state` (только объявленные в схеме `prefill_from_state`-пути, secret-исключение внутри; [input.md → prefill_from_state](../input.md)). Permission `incarnation.get` (reuse: кто читает инкарнацию, тот получает prefill её формы), per-`{name}` scope — in-handler, как у Get/History. Read-резолв, без audit, **REST-only**.

### Runs (2) — глобальный read-view прогонов через все инкарнации

Страница «All Runs» UI: свёртка `apply_runs` по `apply_id` ЧЕРЕЗ ВСЕ инкарнации + сводные счётчики. Прогон (apply_run) — НЕ Voyage (у Voyage свой список `GET /v1/voyages`). Permission `incarnation.history` (reuse read-tier per-incarnation runs, existence-гейт `RequireAction` на chi-группе `/v1/runs`); сужение видимости по Purview ([ADR-047](../adr/0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)) — in-handler, **fail-closed**: пустой/нерезолвящийся scope → пустой список / нулевой агрегат (`200`, не `403` — не палим существование прогонов вне scope; parity souls/stats). Оба роута read-only, без audit, **REST-only** (MCP-tool-ов нет). Endpoint-детали — [operator-api/incarnations.md → GET /v1/runs](operator-api/incarnations.md#get-v1runs--глобальный-список-прогонов).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/runs` | `incarnation.history` | — (UI All Runs) |
| `GET` | `/v1/runs/stats` | `incarnation.history` | — (UI All Runs) |

`GET /v1/runs` (operationId `listRuns`) — страница прогонов, новейшие сверху (`started_at DESC`); query-фильтры `status` (агрегатный `applying`/`success`/`failed`/`cancelled`; невалидный → `422`) и `incarnation` (имя инкарнации-владельца; невалидное → `422`) + пагинация `offset`/`limit` — **cap `limit` = 100** (не общий 1000: глобальная свёртка дороже плоского списка; превышение → `400`). Элемент — форма per-incarnation `RunSummaryEntry` + поле `incarnation` (владелец прогона). `GET /v1/runs/stats` (operationId `getRunsStats`) — счётчики по агрегатному статусу (`total`/`applying`/`success`/`failed`/`cancelled`) в двух корзинах: `all` (за всё время) и `last_24h` (прогоны, стартовавшие за последние 24 часа), в границах того же Purview-scope.

### Soul (8) — реестр хостов

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/souls` | `soul.create` | `keeper.soul.create` |
| `POST` | `/v1/souls/coven` | `soul.coven-assign` | `keeper.soul.coven-assign` |
| `GET` | `/v1/souls` | `soul.list` | `keeper.soul.list` |
| `GET` | `/v1/souls/{sid}` | `soul.list` | — (UI detail-page) |
| `GET` | `/v1/souls/{sid}/soulprint` | `soul.list` | — (UI detail-page) |
| `GET` | `/v1/souls/{sid}/history` | `soul.list` | — (UI timeline) |
| `POST` | `/v1/souls/{sid}/issue-token` | `soul.issue-token` | `keeper.soul.issue-token` |
| `PUT` | `/v1/souls/{sid}/ssh-target` | `soul.ssh-target-update` | `keeper.soul.ssh-target.update` |

Permission `soul.list` покрывает чтение реестра и его деталей: `GET /v1/souls` (список), `GET /v1/souls/{sid}` (single-soul detail), `GET /v1/souls/{sid}/soulprint` (последний typed-Soulprint, [ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)), `GET /v1/souls/{sid}/history` (per-host operation timeline — scenario `apply_runs` + ad-hoc errands). Отдельный `soul.get` сознательно отложен ([rbac.md §Souls](rbac.md)); read-эндпоинты применяют existence-gate (`RequireAction`) + handler-side InScope-фильтр (видимость хоста по Purview, [ADR-047](../adr/0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)). Read-only — без audit.

`PUT /v1/souls/{sid}/ssh-target` (`soul.ssh-target-update`, селектор `host=<sid>`) обновляет per-host SSH-реквизиты push-flow (`souls.ssh_target` jsonb: `ssh_port`/`ssh_user`/`soul_path`, [ADR-032](../adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-без-incarnationscenario) amendment S7-1). 3-сегментный MCP-tool `keeper.soul.ssh-target.update` ↔ 2-сегментная permission `soul.ssh-target-update` (грамматика permission — ровно `<resource>.<action>`). Аудируется (`soul.ssh_target_updated`).

`POST /v1/souls/{sid}/exec` (ad-hoc Errand, permission `errand.run`) учтён в секции [Errand (4)](#errand-4--pull-ad-hoc-exec-вне-scenario-adr-033), не дублируется здесь.

`POST /v1/souls/coven`-семантика, read/ssh-target endpoint-детали — [operator-api/souls.md](operator-api/souls.md). MCP-сторона — [mcp-tools/souls.md](mcp-tools/souls.md).

### Push (2) — [push.md](push.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/push/apply` | `push.apply` | `keeper.push.apply` |
| `GET` | `/v1/push/{apply_id}` | `push.read` | — (REST polling) |

`push.read` — отдельная permission (read-операция не требует mutate-прав `push.apply`); `GET /v1/push/{apply_id}` возвращает состояние записи `push_runs` (in-flight либо terminal). MCP-tool `keeper.push.cleanup` существует в manifest, но **REST-роута `/v1/push/cleanup` нет** — декларация удалена из спеки 2026-06-10 как мёртвая (в `router.go` роут никогда не монтировался); в роут-счёт не входит (см. [mcp-tools/push.md](mcp-tools/push.md)).

### Push-runs (1) — глобальный список push-прогонов

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/push-runs` | `incarnation.history` | — (UI Push-runs page) |

Отдельная зона от `/v1/push/{apply_id}` (тот — per-id detail; этот — список с пагинацией/фильтрами). RBAC reuse `incarnation.history` (push — история incarnation, parity Voyage-list); отдельная permission `push.list` не вводится до запроса оператора. NoSelector. Подключается вместе с push-блоком (`router.go`: `if pushH != nil`).

### Errand (4) — pull-ad-hoc exec вне scenario, [ADR-033](../adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/souls/{sid}/exec` | `errand.run` | `keeper.soul.errand.run` (slice E4) |
| `GET` | `/v1/errands/{errand_id}` | `errand.list` | — (REST polling) |
| `GET` | `/v1/errands` | `errand.list` | `keeper.errand.list` (slice E4) |
| `DELETE` | `/v1/errands/{errand_id}` | `errand.cancel` | `keeper.errand.cancel` (slice E5) |

Sync-первичный flow (server-cap 30s), whitelist модулей Soul-side, cancel-flow (slice E5) и полные request/response — в доменном файле [operator-api/errands.md → Endpoint-секции](operator-api/errands.md#endpoint-секции). Errand НЕ мутирует `incarnation.state` (отдельный реестр `errands`). MCP-сторона — [mcp-tools/errands.md](mcp-tools/errands.md).

### ~~POST /v1/errand-runs (multi-target ad-hoc exec)~~ — superseded

Multi-target Errand под единым ULID. Реализация — slice E6-4.

**Superseded-by-Voyage (удалён в Wave 5).** Multi-target ad-hoc exec теперь — `POST /v1/voyages` с `kind=command` ([ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)). Эндпоинт `/v1/errand-runs` и реестр `errand_runs` реализационно удалены; раздел оставлен как историческая запись, см. [ADR-041](../adr/0041-errandrun.md#adr-041-errandrun--multi-target-обвязка-над-errand).

### Voyage (5) — унифицированный батчевый прогон, [ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/voyages` | `incarnation.run` (kind=scenario) / `errand.run` (kind=command) — RBAC-by-kind | `keeper.voyage.start` |
| `POST` | `/v1/voyages/preview` | `incarnation.run` / `errand.run` — RBAC-by-kind (как create) | — (REST only) |
| `GET` | `/v1/voyages` | `incarnation.history` | `keeper.voyage.list` |
| `GET` | `/v1/voyages/{id}` | `incarnation.history` | `keeper.voyage.get` |
| `DELETE` | `/v1/voyages/{id}` | `incarnation.run` / `errand.run` (cancel pending/scheduled) | `keeper.voyage.cancel` |

**RBAC-by-kind** ([ADR-043 §6](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), fail-closed): permission выбирается по `kind` из тела (виден только после декода body, поэтому проверка живёт в handler-е, а не в middleware-route). Подробности резолва target-а / гибрид-семантики command∩Purview / Tempo-лимита — [operator-api/voyages.md](operator-api/voyages.md).

`GET /v1/voyages/{id}/targets` (All-runs drill, [ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) S5) — read под `incarnation.history`, **REST-only** (MCP-tool нет). В заголовке секции «(5)» он не считается (счётчик ведёт по числу MCP-парных + RBAC-by-kind строк); шесть REST-роутов Voyage = пять выше + `/{id}/targets`.

### Cadence (8) — регулярные запуски (scheduled/recurring Voyage), [ADR-046](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage) / [ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)

Расписания, по времени спавнящие обычный Voyage-прогон (реестр `cadences`). Исполняет триггер [Conductor](conductor.md) (leader-elected подсистема внутри `keeper`, source-of-truth поведению). **REST-only — MCP-tool-ов нет** ([mcp-tools/cadences.md](mcp-tools/cadences.md)). Все восемь роутов монтируются только при сконфигурированном реестре Cadence; при отсутствии (`router.go`: `if cadenceH != nil`) блок `/v1/cadences*` **не монтируется** → `404`. `cadence.*` — NoSelector. Endpoint-детали, формы тел, двухуровневый RBAC — [operator-api/cadences.md](operator-api/cadences.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/cadences` | `cadence.create` (+ Voyage-perm по `kind`) | — (REST only) |
| `GET` | `/v1/cadences` | `cadence.list` | — (REST only) |
| `GET` | `/v1/cadences/{id}` | `cadence.list` | — (REST only) |
| `PATCH` | `/v1/cadences/{id}` | `cadence.update` (+ Voyage-perm по `kind`) | — (REST only) |
| `DELETE` | `/v1/cadences/{id}` | `cadence.delete` | — (REST only) |
| `POST` | `/v1/cadences/{id}/enable` | `cadence.enable` ИЛИ `cadence.update` | — (REST only) |
| `POST` | `/v1/cadences/{id}/disable` | `cadence.disable` ИЛИ `cadence.update` | — (REST only) |
| `GET` | `/v1/cadences/{id}/runs` | `incarnation.history` | — (REST only) |

**Двухуровневый RBAC** (security-критичный fail-closed, [ADR-046 §7](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)): `cadence.create`/`cadence.update` гейтит middleware, но рецепт спавнит Voyage от имени создателя — поэтому на create/patch handler дополнительно требует Voyage-permission по `kind` рецепта (`scenario`→`incarnation.run`, `command`→`errand.run`), иначе Cadence стала бы privilege-escalation-обходом RBAC → `403`. `enable`/`disable` — OR-гейт `cadence.enable|disable` ИЛИ backcompat `cadence.update`. `/runs` (дочерние Voyage) reuse `incarnation.history` (parity Voyage-list). **Floor-лимит:** `interval_seconds < 30` → `422` (суб-30s реакция — через Beacons, [ADR-030](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)). Мутирующие роуты (`create`/`update`/`delete`/`enable`/`disable`) аудируются ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `list`/`get`/`runs` — read-only, без audit.

### Oracle (8) — реестры Vigil / Decree (event-driven мониторинг), [ADR-030](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)

CRUD реестров Beacons: Vigil (Soul-side проверка) и Decree (правило reactor: Portent → match → enqueue scenario). `vigil.*`/`decree.*` — NoSelector. Подключаются только при сконфигурированном Oracle-реестре. Источник правды по семантике, телам, кодам ошибок — [operator-api/oracle.md](operator-api/oracle.md); MCP-сторона — [mcp-tools/oracle.md](mcp-tools/oracle.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/vigils` | `vigil.create` | `keeper.oracle.vigil.create` |
| `GET` | `/v1/vigils` | `vigil.list` | `keeper.oracle.vigil.list` |
| `GET` | `/v1/vigils/{name}` | `vigil.list` | `keeper.oracle.vigil.list` |
| `DELETE` | `/v1/vigils/{name}` | `vigil.delete` | `keeper.oracle.vigil.delete` |
| `POST` | `/v1/decrees` | `decree.create` | `keeper.oracle.decree.create` |
| `GET` | `/v1/decrees` | `decree.list` | `keeper.oracle.decree.list` |
| `GET` | `/v1/decrees/{name}` | `decree.list` | `keeper.oracle.decree.list` |
| `DELETE` | `/v1/decrees/{name}` | `decree.delete` | `keeper.oracle.decree.delete` |

4-сегментный MCP-tool `keeper.oracle.<resource>.<action>` ↔ 2-сегментная permission `<resource>.<action>` (resource `vigil`/`decree`; одно permission покрывает list+get). Reactor-флоу (Portent → match Decree → enqueue) этими permission-ами **НЕ управляется** — это машинный Soul-инициированный путь ([rbac.md §Oracle](rbac.md)). Мутирующие 4 роута (vigil/decree create/delete) аудируются; list/get — read-only, без audit.

### Push-Provider (5) — реестр env-payload params SSH-плагинов push-flow, [ADR-032](../adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-без-incarnationscenario) amendment S7-2

CRUD реестра `push_providers` (per-provider params SSH-плагина; long-term canon вместо `keeper.yml::push.providers[]`). Sensitive params (`secret_id`/`token`/`password`/`private_key`) ОБЯЗАНЫ быть vault-refs. `push-provider.*` — NoSelector. Подключаются только при сконфигурированном реестре. Источник правды по семантике, телам, кодам ошибок — [operator-api/push-providers.md](operator-api/push-providers.md); MCP-сторона — [mcp-tools/push-providers.md](mcp-tools/push-providers.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/push-providers` | `push-provider.create` | `keeper.push-provider.create` |
| `GET` | `/v1/push-providers` | `push-provider.list` | `keeper.push-provider.list` |
| `GET` | `/v1/push-providers/{name}` | `push-provider.read` | `keeper.push-provider.read` |
| `PUT` | `/v1/push-providers/{name}` | `push-provider.update` | `keeper.push-provider.update` |
| `DELETE` | `/v1/push-providers/{name}` | `push-provider.delete` | `keeper.push-provider.delete` |

5 tool-ов 1:1 `keeper.push-provider.<verb>` ↔ permission `push-provider.<verb>` ↔ REST. `read` (одна запись) отделён от `list` — параллель `operator.read`↔`operator.list`. Мутирующие 3 роута (`create`/`update`/`delete`) аудируются; `list`/`read` — read-only, без audit. После commit-а мутации — cluster-wide invalidate через Redis pub/sub `push-providers:changed`.

### Herald (5) — реестр каналов доставки уведомлений о прогонах, [ADR-052](../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)

CRUD реестра `heralds` (канал доставки уведомлений; webhook в MVP). SSRF-контур (https-only + deny приватных IP) взведён по умолчанию, снимается per-Herald opt-out-флагами `config.http_allowed` / `config.allow_private`; `secret_ref` — vault-ref на signing-token (подпись webhook `X-SoulStack-Signature: sha256=<hex>`, HMAC-SHA256). `herald.*` — NoSelector. Подключаются только при сконфигурированном реестре (`router.go`: `if heraldH != nil`). Источник правды по семантике, телам, кодам ошибок — [operator-api/heralds.md](operator-api/heralds.md); MCP-сторона — [mcp-tools/heralds.md](mcp-tools/heralds.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/heralds` | `herald.create` | `keeper.herald.create` |
| `GET` | `/v1/heralds` | `herald.list` | `keeper.herald.list` |
| `GET` | `/v1/heralds/{name}` | `herald.read` | `keeper.herald.read` |
| `PUT` | `/v1/heralds/{name}` | `herald.update` | `keeper.herald.update` |
| `DELETE` | `/v1/heralds/{name}` | `herald.delete` | `keeper.herald.delete` |

5 tool-ов 1:1 `keeper.herald.<verb>` ↔ permission `herald.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/heralds*`. `read` отделён от `list` (параллель `operator.read`↔`operator.list`). Мутирующие 3 роута (`create`/`update`/`delete`) аудируются — audit-события `herald.created` / `herald.updated` / `herald.deleted` ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `list`/`read` — read-only, без audit. `PUT` — replace-семантика (полная замена mutable-полей, не PATCH), как у Push-Provider. После commit-а мутации — cluster-wide invalidate dispatcher-кэша через Redis pub/sub `herald:invalidate`. Терминалы доставки (`herald.delivered` / `herald.failed`) пишет worker, не CRUD-роуты.

### Tiding (5) — реестр правил подписки на уведомления, [ADR-052](../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)

CRUD реестра `tidings` (правило подписки: на какие `event_types` реагировать → каким Herald-ом доставлять). `event_types` — area-glob в scope прогонов (`scenario_run.*` / `command_run.*` / `voyage.*` / `cadence.*` + точечные `incarnation.drift_checked` и `incarnation.run_completed`); произвольный wildcard запрещён. `herald` — FK на существующий Herald. Опц. селектор `task` (адрес `register ∪ id`) подписывает на изменение конкретной задачи и матчит только `incarnation.run_completed` по его `changed_tasks` ([ADR-052 §l](../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)). `tiding.*` — NoSelector. Подключаются только при сконфигурированном реестре. Источник правды по семантике, телам, кодам ошибок — [operator-api/tidings.md](operator-api/tidings.md); MCP-сторона — [mcp-tools/tidings.md](mcp-tools/tidings.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/tidings` | `tiding.create` | `keeper.tiding.create` |
| `GET` | `/v1/tidings` | `tiding.list` | `keeper.tiding.list` |
| `GET` | `/v1/tidings/{name}` | `tiding.read` | `keeper.tiding.read` |
| `PUT` | `/v1/tidings/{name}` | `tiding.update` | `keeper.tiding.update` |
| `DELETE` | `/v1/tidings/{name}` | `tiding.delete` | `keeper.tiding.delete` |

5 tool-ов 1:1 `keeper.tiding.<verb>` ↔ permission `tiding.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/tidings*`. `read` отделён от `list`. Мутирующие 3 роута аудируются — audit-события `tiding.created` / `tiding.updated` / `tiding.deleted`; `list`/`read` — read-only, без audit. `PUT` — replace-семантика (как у Herald). Ссылка на отсутствующий Herald (`herald` FK) на create/update → `404`. Снос Herald-канала каскадно уносит его Tiding-подписки (`tidings.herald ON DELETE CASCADE`). Допустимые `event_types` подписки — из каталога [`GET /v1/event-types`](#self-describing-3--каталоги-прав--event-types-и-эффективные-права-adr-042) (UI фетчит, не хардкод); тот же scope валидирует CRUD Tiding (произвольный wildcard / тип вне scope → `422`).

### Choir (6) — именованная топология хостов внутри инкарнации, [ADR-044](../adr/0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации)

CRUD топологии Choir/Voice внутри инкарнации (`/v1/incarnations/{name}/choirs*`). Choir принадлежит инкарнации → тот же scope-селектор `incarnation`/`service`/`coven` (по path-`{name}`), что у incarnation-мутаций. Подключаются только при сконфигурированном ChoirDB-пуле. **REST-only — MCP-tool-ов нет** ([mcp-tools/choirs.md](mcp-tools/choirs.md)). Источник правды по семантике, телам — [operator-api/choirs.md](operator-api/choirs.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/incarnations/{name}/choirs` | `choir.create` | — (REST only) |
| `GET` | `/v1/incarnations/{name}/choirs` | `choir.list` | — (REST only) |
| `DELETE` | `/v1/incarnations/{name}/choirs/{choir}` | `choir.delete` | — (REST only) |
| `POST` | `/v1/incarnations/{name}/choirs/{choir}/voices` | `choir.add-voice` | — (REST only) |
| `GET` | `/v1/incarnations/{name}/choirs/{choir}/voices` | `choir.list` | — (REST only) |
| `DELETE` | `/v1/incarnations/{name}/choirs/{choir}/voices/{sid}` | `choir.remove-voice` | — (REST only) |

`choir.*` — incarnation-scope (через `IncarnationScopeSelector` по path-`{name}`); Voice-actions hyphenated (`add-voice`/`remove-voice`) по грамматике `<resource>.<action>`. `list` покрывает и список Choir-ов, и список Voice-ов. Mutating-CRUD аудируется (payload пишет сам handler — choir/voice-snapshot доступен только после мутации).

### Self-describing (3) — каталоги прав / event-types и эффективные права, [ADR-042](../adr/0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-в-ui--ui-не-хардкодит-динамические-каталоги)

Само-описывающие read-роуты для permission-aware UI. **Auth-only** (`RequireJWT` на `/v1/*`), **БЕЗ `RequirePermission`** — требование права на чтение списка прав/значений = курица-яйцо (architect-вердикт); `/me/permissions` отдаёт ИМЕННО свои права (AID из claims, не query). **REST-only — MCP-tool-ов нет.** Read-only, без audit (паттерн health/meta).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/permissions` | — (auth-only) | — (REST only) |
| `GET` | `/v1/event-types` | — (auth-only) | — (REST only) |
| `GET` | `/v1/me/permissions` | — (auth-only) | — (REST only) |

`GET /v1/permissions` — машиночитаемый каталог RBAC-permissions (источник — `rbac.catalog.go`), UI фетчит реальные имена для назначения прав роли. `GET /v1/event-types` — машиночитаемый каталог event-types, допустимых для подписки [Tiding](operator-api/tidings.md) (источник — `herald/eventtypes.go`, тот же scope, что валидирует CRUD Tiding); UI Tiding-формы фетчит допустимые типы вместо хардкода. Тело — две группы: `areas` (области area-glob-подписки, готовая форма `<area>.*` — `scenario_run.*`/`command_run.*`/`voyage.*`/`cadence.*`) + `point_events` (точечные типы вне area-glob — `incarnation.drift_checked`/`incarnation.run_completed`). `GET /v1/me/permissions` — эффективные права текущего Архонта (показывать/прятать кнопки). Все три монтируются всегда (статика из пакетов `rbac`/`herald` / снимок enforcer-а, без внешних зависимостей).

### Cloud (8) — реестры Cloud-Provider / Cloud-Profile, [ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) / [cloud.md](cloud.md)

CRUD реестров `providers` (учётка облака) и `profiles` (VM-spec поверх Provider-а) в Postgres, managed через OpenAPI/MCP. `provider.*` / `profile.*` — NoSelector (CRUD оперирует самим реестром, как `service.*` / `push-provider.*`). **Иммутабельность:** `update`-операции **нет** — смена параметров = `delete` + `create` (защита от частичной мутации spec уже-живущих VM), поэтому read-видимость гейтит одна permission `provider.read` / `profile.read`. `credentials_ref` принимает строку `vault:<mount>/<path>`; сами credentials API **НЕ резолвит и НЕ возвращает** (секрет-гигиена). Источник правды по семантике, телам — [cloud.md → Provider и Profile](cloud.md#provider-и-profile-в-postgres); MCP-сторона — [mcp-tools.md → Cloud](mcp-tools.md#cloud-8). Роуты монтируются только при сконфигурированном реестре (`Deps.ProviderSvc` / `Deps.ProfileSvc`).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/providers` | `provider.create` | `keeper.provider.create` |
| `GET` | `/v1/providers` | `provider.read` | `keeper.provider.list` |
| `GET` | `/v1/providers/{name}` | `provider.read` | `keeper.provider.get` |
| `DELETE` | `/v1/providers/{name}` | `provider.delete` | `keeper.provider.delete` |
| `POST` | `/v1/profiles` | `profile.create` | `keeper.profile.create` |
| `GET` | `/v1/profiles` | `profile.read` | `keeper.profile.list` |
| `GET` | `/v1/profiles/{name}` | `profile.read` | `keeper.profile.get` |
| `DELETE` | `/v1/profiles/{name}` | `profile.delete` | `keeper.profile.delete` |

Permission-маппинг: `POST`→`<resource>.create`, `GET`(list + get-`{name}`)→`<resource>.read`, `DELETE`→`<resource>.delete`. Мутирующие 4 роута (create/delete по каждой сущности) аудируются — audit-события `provider.created` / `provider.deleted` / `profile.created` / `profile.deleted` ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `provider.read`/`profile.read` (list + get) — read-only, без audit (audit Profile-create пишет только ключи `params`, не значения). Граничные случаи: `409 provider-already-exists` / `409 profile-already-exists` на дубль `name`; `409 provider-has-profiles` при удалении Provider-а с привязанными Profile-ями (FK `ON DELETE RESTRICT`, миграция 020 — сперва удалить зависимые Profile-и); `422 validation-failed` на ссылку Profile-я на несуществующий Provider (FK) либо битый `name`/`type`/`region`/`credentials_ref`; `404 not-found` на get/delete отсутствующей записи. 3-сегментный MCP-tool `keeper.<resource>.<verb>` ↔ 2-сегментная permission `<resource>.<verb>` (read-tool назван `get`, permission verb — `read`).

### Service (9) — реестр Service-ов (CRUD + git-проекции), [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) / [service/manifest.md](../service/manifest.md)

Реестр `service_registry`: каталог `services[]` переносится из статического `keeper.yml` в managed-через-OpenAPI/MCP PG-таблицу ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)). `service.*` — NoSelector (CRUD оперирует самим реестром). Источник правды по семантике, телам, инвалидации снимка — `serviceregistry.Service`; MCP-сторона — [mcp-tools.md → Service](mcp-tools.md#service-4).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/services` | `service.register` | `keeper.service.register` |
| `GET` | `/v1/services` | `service.list` | `keeper.service.list` |
| `GET` | `/v1/services/{name}` | `service.list` | `keeper.service.list` |
| `PATCH` | `/v1/services/{name}` | `service.update` | `keeper.service.update` |
| `DELETE` | `/v1/services/{name}` | `service.deregister` | `keeper.service.deregister` |
| `GET` | `/v1/services/{name}/refs` | `service.list` | — (REST only) |
| `GET` | `/v1/services/{name}/scenarios` | `service.list` | — (REST only) |
| `GET` | `/v1/services/{name}/state-schema` | `service.list` | — (REST only) |
| `GET` | `/v1/services/{name}/dependencies` | `service.list` | — (REST only) |

Permission-маппинг: `POST`→`service.register`, `GET`(list + get-`{name}`)→`service.list`, `PATCH`→`service.update`, `DELETE`→`service.deregister`. Мутирующие 3 роута (`register`/`update`/`deregister`) аудируются ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); чтения — read-only, без audit. Четыре git-проекции (`/refs` — tag-и+branch-и для Upgrade-modal; `/scenarios` — dropdown Run-modal; `/state-schema` — Schema explorer; `/dependencies` — destiny/module-зависимости) reuse `service.list` (проекции одной Service-записи, без отдельной permission и без MCP-tool-ов); при сбое внешнего git-источника — `502`. Роуты подключаются только при сконфигурированном реестре Service-ов.

**Каталог сценариев `GET /v1/services/{name}/scenarios`** несёт по каждому сценарию поле **`runnable: bool`** — признак «запускаем оператором из Run-формы». Размечается Keeper-ом по канону scenario-пакета (`IsRunnableScenario`), не из манифеста: `create` = `true`, `destroy` = `false` (спец-флоу удаления через `DELETE /v1/incarnations/{name}`), operational-сценарии (вкл. `converge`) = `true`. UI фильтрует Run-форму по `runnable`, а не по хардкоду имён ([ADR-042](../adr/0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-в-ui--ui-не-хардкодит-динамические-каталоги), [architecture.md → Service](../architecture.md#service--структура-и-manifest)).

### Sigil-key (4) — ротация ключей подписи Sigil, [ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) / R3

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/sigil/keys` | `sigil.key-introduce` | `keeper.sigil.key.introduce` |
| `GET` | `/v1/sigil/keys` | `sigil.key-list` | `keeper.sigil.key.list` |
| `POST` | `/v1/sigil/keys/{key_id}/primary` | `sigil.key-set-primary` | `keeper.sigil.key.set-primary` |
| `DELETE` | `/v1/sigil/keys/{key_id}` | `sigil.key-retire` | `keeper.sigil.key.retire` |

3-сегментный MCP-tool `keeper.sigil.key.<verb>` ↔ 2-сегментная permission `sigil.key-<verb>` (resource `sigil`, hyphenated action — грамматика permission ровно `<resource>.<action>`).

### Plugin (3) — Sigil allow-list целостности плагинов, [ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) S4a

Допуск/отзыв/список записей allow-list-а `plugin_sigils` (бинарь плагина проходит Sigil-верификацию только при активном допуске). Отдельная зона от Sigil-key выше: тот про ключи **подписи**, этот — про допуски самих бинарей. `plugin.*` — NoSelector. Источник правды по семантике, телам и `sha256`-вычислению — [plugins.md → Integrity-model](plugins.md#integrity-model); MCP-сторона — [mcp-tools.md → Plugin](mcp-tools.md#plugin-3).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/plugins/sigils` | `plugin.allow` | `keeper.plugin.allow` |
| `GET` | `/v1/plugins/sigils` | `plugin.list` | `keeper.plugin.list` |
| `DELETE` | `/v1/plugins/sigils/{namespace}/{name}/{ref}` | `plugin.revoke` | `keeper.plugin.revoke` |

Мутирующие 2 роута (`allow`/`revoke`) аудируются (supply-chain-мутации, [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `plugin.list` — read-only, без audit. Роуты монтируются только при сконфигурированном Sigil (`keeper.yml → sigil.signing_key_ref`); при выключенном Sigil блок `/v1/plugins/sigils*` **не монтируется** — запрос ловит catch-all → `404 not-found` (`router.go`: `if sigilH != nil`).

### Module-catalog (3) — read-only-каталог модулей для UI Run→Command, [ADR-042](../adr/0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-в-ui--ui-не-хардкодит-динамические-каталоги)

Машиночитаемый каталог core-модулей (doc-data из core-registry) + активных plugin-допусков для поиска модуля в UI Run→Command. **REST-only — MCP-tool-ов нет.** Reuse permission `service.list` (read-only-каталог, новая permission не заводится). `form-prep` — резолвер source-каталогов UI-формы модуля (`incarnation_hosts`/`choir` → живые SID-ы автокомплита), reuse под-прогонной permission `incarnation.run`.

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/modules` | `service.list` | — (REST only) |
| `GET` | `/v1/modules/{name}` | `service.list` | — (REST only) |
| `POST` | `/v1/modules/{name}/form-prep` | `incarnation.run` | — (REST only) |

Все три read-only/резолв-роута — без audit (паттерн `service.list` / `soul.list`). Selector — NoSelector (каталог глобальный, резолв cluster-wide по `souls`).

### Augur (7) — реестры Omen / Rite, [ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul) / [augur.md](augur.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/augur/omens` | `omen.create` | `keeper.augur.omen.create` |
| `GET` | `/v1/augur/omens` | `omen.list` | `keeper.augur.omen.list` |
| `GET` | `/v1/augur/omens/{name}` | `omen.list` | `keeper.augur.omen.list` |
| `DELETE` | `/v1/augur/omens/{name}` | `omen.delete` | `keeper.augur.omen.delete` |
| `POST` | `/v1/augur/rites` | `rite.create` | `keeper.augur.rite.create` |
| `GET` | `/v1/augur/rites` | `rite.list` | `keeper.augur.rite.list` |
| `DELETE` | `/v1/augur/rites/{id}` | `rite.delete` | `keeper.augur.rite.delete` |

4-сегментный MCP-tool `keeper.augur.<resource>.<action>` ↔ 2-сегментная permission `<resource>.<action>` (resource `omen`/`rite`). Live-fetch от Soul (`AugurRequest`) этими permission-ами НЕ управляется ([rbac.md §Augur](rbac.md)).

### Audit (1) — read-only-лента audit-events, [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/audit` | `audit.read` | — (UI iter 2) |

Read-only-лента событий `audit_log` для UI iteration 2 (placeholder `/audit`). Read-эндпоинт сам в audit-trail НЕ пишется (избегаем рекурсии — каждый GET удваивал бы таблицу). MCP-tool-симметрия отложена.

**Итого: 4 health/meta на API-фасаде (`/healthz`, `/readyz`, `/openapi.yaml`, `/openapi.json`) + 132 endpoint-а под permissions/auth-only** (Operator 5 + Audit 1 + Role 6 + Synod 8 + Incarnation 15 + Runs 2 + Choir 6 + Soul 8 + Errand 4 + Plugin 3 + Sigil-key 4 + Service 9 + Module-catalog 3 + Self-describing 3 + Augur 7 + Oracle 8 + Push 2 + Push-runs 1 + Push-Provider 5 + Cloud 8 + Herald 5 + Tiding 5 + Voyage 6 + Cadence 8) **= 132 роута в этой таблице.** `/metrics` в этот фасадный счёт **не входит** — Prometheus-эндпоинт вынесен на отдельный metrics-listener (`listen.metrics.addr`, [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)), в `router.go` фасада не монтируется. (Voyage «(6)» — пять MCP-парных/RBAC-by-kind строк + шестой REST-роут `GET /v1/voyages/{id}/targets`, read/REST-only. Augur — 7 роутов: 4 omen + 3 rite. Cloud — 8 роутов: 4 provider (create/list/get/delete) + 4 profile, реализованы и смонтированы.)

> **Несведённые роуты (TODO — секции ещё не написаны).** В [`router.go`](../../keeper/internal/api/router.go) смонтированы, но в таблицы выше пока не сведены: `GET /v1/souls/stats` (агрегат Souls Overview, `soul.list`), `POST /v1/souls/traits` (bulk trait-assign, `soul.traits-assign` — **deprecated**, заменён `PUT /v1/incarnations/{name}/traits`, ADR-060), `GET`/`PUT /v1/provisioning-policy` (`provisioning.read`/`provisioning.update`, [ADR-058](../adr/0058-operator-auth-ldap-oidc.md) Часть B), `GET /v1/herald-types` (auth-only каталог типов Herald-канала, [ADR-042](../adr/0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-в-ui--ui-не-хардкодит-динамические-каталоги)-паттерн), `GET /v1/cluster` (HA-топология Keeper-кластера, existence-gate `soul.list`). Логин-роуты `/auth/ldap/login` + `/auth/oidc/{login,callback}` — вне `/v1` (публичный вход до JWT, parity `/healthz`; ADR-058) и в `/v1`-роут-счёт не входят. Формы и семантика — в производной [`openapi.yaml`](openapi.yaml); в счётчик выше эти роуты не включены.

> **Tool-счёт vs роут-счёт — легитимно разные множества.** Заголовки секций в [mcp-tools.md](mcp-tools.md) считают **MCP-tools**, а здесь — **REST-роуты**; один домен может нести больше REST-роутов, чем MCP-tool-ов. Сверка с кодом (`router.go` + `keeper/internal/mcp/manifest.go`):
>
> - **Incarnation** — REST «(15)» (router) vs MCP «(11)» (`manifest.go`). Разница НЕ ошибка: четыре REST-only-роута без MCP-tool-а — `PATCH /v1/incarnations/{name}/hosts` (tool `keeper.incarnation.hosts.update` в `manifest.go` отсутствует), `POST …/scenarios/{scenario}/form-prefill` (UI-резолв), `GET …/runs` + `GET …/runs/{apply_id}` (read-view прогонов под UI); остальные 11 REST-роутов (включая `PUT .../traits` ↔ `keeper.incarnation.traits-set`, ADR-060) имеют MCP-парность. Заголовок mcp-tools.md «(11)» считает MCP-tools, разбор — в [mcp-tools/incarnations.md](mcp-tools/incarnations.md). Глобальные `GET /v1/runs` + `/v1/runs/stats` (секция Runs) — тоже REST-only.
> - **Voyage** — REST «(5)» (без `/targets`) vs MCP «(4)» (`preview` REST-only). Шестой REST-роут `/targets` и `preview` — read/REST-only, MCP-tool-а не имеют.
> - **Cadence / Choir / Self-describing** — MCP-tool-ов **нет вовсе** (`manifest.go` их не содержит): заголовки этих секций ведут по числу REST-роутов; стаб-файлы [mcp-tools/cadences.md](mcp-tools/cadences.md) / [mcp-tools/choirs.md](mcp-tools/choirs.md) фиксируют «(0)».

## Endpoint-секции

Ключевые endpoints разобраны с полным request/response schema. Для остальных — таблица полей без полного JSON-примера (форма примера выводится по аналогии).

### Operator endpoints

Вынесены в доменный файл — [operator-api/operator.md → Endpoint-секции](operator-api/operator.md#endpoint-секции): `POST /v1/operators` (создать Архонта + первый JWT), `POST /v1/operators/{aid}/revoke` (отзыв), `POST /v1/operators/{aid}/issue-token` (перевыпуск JWT), `GET /v1/operators` (список), `GET /v1/operators/{aid}` (detail). MCP-сторона — [mcp-tools/operator.md](mcp-tools/operator.md).

### Audit endpoints

Вынесены в доменный файл — [operator-api/audit.md → Endpoint-секции](operator-api/audit.md#endpoint-секции): `GET /v1/audit` (read-only-лента событий `audit_log`, фильтры `type`/`source`/`archon_aid`/`correlation_id`/`started_after`/`started_before` + pagination). MCP-сторона — [mcp-tools/audit.md](mcp-tools/audit.md) (MCP-tool-симметрия отложена).

### Incarnation endpoints

Вынесены в доменный файл — [operator-api/incarnations.md → Endpoint-секции](operator-api/incarnations.md#endpoint-секции): `POST /v1/incarnations` (создать instance — выбор стартового сценария через `create_scenario`, либо bare-инкарнация если сервис без create-сценариев), `POST …/rerun-last` (перезапуск последнего упавшего сценария из `error_locked`), `POST …/scenarios/{scenario}` (произвольный сценарий), `GET …/{name}` (spec+state+status), `GET /v1/incarnations` (список), `GET …/history` (журнал state), `GET …/runs` + `GET …/runs/{apply_id}` (read-view прогонов: список + per-host детали), `POST …/unlock`, `POST …/upgrade`, `GET …/upgrade-paths` (пути апгрейда — [ADR-0068](../adr/0068-service-upgrade-v2.md)), `POST …/check-drift` (Scry), `DELETE …/{name}` (destroy), плюс два superseded-by-Voyage Tide-раздела (историческая запись). Там же — глобальные `GET /v1/runs` + `GET /v1/runs/stats` (страница «All Runs», [§ Runs (2)](#runs-2--глобальный-read-view-прогонов-через-все-инкарнации)). MCP-сторона — [mcp-tools/incarnations.md](mcp-tools/incarnations.md).

### Soul endpoints

Вынесены в доменный файл — [operator-api/souls.md → Endpoint-секции](operator-api/souls.md#endpoint-секции): `POST /v1/souls` (регистрация + bootstrap-токен), `POST /v1/souls/{sid}/issue-token` (перевыпуск bootstrap-токена), `GET /v1/souls` (список), плюс `POST /v1/souls/coven` (bulk Coven-метки). MCP-сторона — [mcp-tools/souls.md](mcp-tools/souls.md).

### Augur endpoints

Вынесены в доменный файл — [operator-api/augur.md → Endpoint-секции](operator-api/augur.md#endpoint-секции): `POST /v1/augur/omens` (создать Omen), `GET /v1/augur/omens` (список), `GET /v1/augur/omens/{name}` (прочитать), `DELETE /v1/augur/omens/{name}` (удалить), `POST /v1/augur/rites` (создать Rite), `GET /v1/augur/rites` (список Rite-ов Omen-а), `DELETE /v1/augur/rites/{id}` (удалить). Полная модель брокера — [augur.md](augur.md). MCP-сторона — [mcp-tools/augur.md](mcp-tools/augur.md).

### Push endpoints

Вынесены в доменный файл — [operator-api/push.md → Endpoint-секции](operator-api/push.md#endpoint-секции): `POST /v1/push/apply` (push-прогон Destiny по SSH), `GET /v1/push/{apply_id}` (состояние push-прогона), `GET /v1/push-runs` (глобальный список push-прогонов). Полная модель push-режима — [push.md](push.md). MCP-сторона — [mcp-tools/push.md](mcp-tools/push.md).

### Cloud endpoints

CRUD реестров Cloud-Provider / Cloud-Profile — `POST/GET/DELETE /v1/providers*` + `POST/GET/DELETE /v1/profiles*` (без `PUT`/`PATCH`: Provider/Profile иммутабельны, смена параметров = `delete` + `create`). Provider-тело — `name`/`type`/`region`/`credentials_ref` (`credentials_ref` — строка `vault:<mount>/<path>`, секрет API не резолвит и не возвращает); Profile-тело — `name`/`provider` (FK на Provider)/`params`/`cloud_init`. Источник правды по семантике, телам, граничным случаям (`409` дубль-`name`, `409 provider-has-profiles` FK RESTRICT, `422` ссылка на несуществующий Provider) и Credentials-flow — [cloud.md → Provider и Profile](cloud.md#provider-и-profile-в-postgres). MCP-сторона — [mcp-tools.md → Cloud](mcp-tools.md#cloud-8).

### Voyage endpoints

Вынесены в доменный файл — [operator-api/voyages.md → Endpoint-секции](operator-api/voyages.md#endpoint-секции): `POST /v1/voyages` (создать Voyage, RBAC-by-kind, target-резолв, command∩Purview, Tempo) и `POST /v1/voyages/preview` (dry-resolve scope без создания). MCP-сторона — [mcp-tools/voyages.md](mcp-tools/voyages.md).

### Cadence endpoints

Вынесены в доменный файл — [operator-api/cadences.md → Endpoint-секции](operator-api/cadences.md#endpoint-секции): `POST /v1/cadences` (создать расписание, двухуровневый RBAC), `GET /v1/cadences` (список), `GET …/{id}` (деталь), `PATCH …/{id}` (обновить), `DELETE …/{id}` (снять), `POST …/{id}/enable|disable` (toggle), `GET …/{id}/runs` (дочерние Voyage). Поведение исполнителя расписаний — [conductor.md](conductor.md). **MCP-стороны нет** ([mcp-tools/cadences.md](mcp-tools/cadences.md)).

### Oracle endpoints

Вынесены в доменный файл — [operator-api/oracle.md → Endpoint-секции](operator-api/oracle.md#endpoint-секции): `POST/GET/DELETE /v1/vigils*` (Soul-side проверки beacons) + `POST/GET/DELETE /v1/decrees*` (правила reactor). MCP-сторона — [mcp-tools/oracle.md](mcp-tools/oracle.md).

### Push-Provider endpoints

Вынесены в доменный файл — [operator-api/push-providers.md → Endpoint-секции](operator-api/push-providers.md#endpoint-секции): `POST/GET/PUT/DELETE /v1/push-providers*` (env-payload params SSH-плагинов push-flow; sensitive — vault-refs). MCP-сторона — [mcp-tools/push-providers.md](mcp-tools/push-providers.md).

### Herald endpoints

Вынесены в доменный файл — [operator-api/heralds.md → Endpoint-секции](operator-api/heralds.md#endpoint-секции): `POST/GET/PUT/DELETE /v1/heralds*` (каналы доставки уведомлений о прогонах; webhook + SSRF-guard + `secret_ref` vault-ref + `X-SoulStack-Signature`). MCP-сторона — [mcp-tools/heralds.md](mcp-tools/heralds.md).

### Tiding endpoints

Вынесены в доменный файл — [operator-api/tidings.md → Endpoint-секции](operator-api/tidings.md#endpoint-секции): `POST/GET/PUT/DELETE /v1/tidings*` (правила подписки `event_types` → Herald). MCP-сторона — [mcp-tools/tidings.md](mcp-tools/tidings.md).

### Choir endpoints

Вынесены в доменный файл — [operator-api/choirs.md → Endpoint-секции](operator-api/choirs.md#endpoint-секции): `POST/GET/DELETE /v1/incarnations/{name}/choirs*` + voices (именованная топология хостов внутри инкарнации). **MCP-стороны нет** ([mcp-tools/choirs.md](mcp-tools/choirs.md)).

## Полная OpenAPI YAML

Полная OpenAPI 3.1 YAML-спека: [`openapi.yaml`](openapi.yaml) — **производный снимок** формы Operator API (paths, методы, тела, схемы), а не источник правды. Источник правды — **huma-агрегатор в Go-коде** (`HumaFullSpecYAML` / `buildFullOpenAPISpec` в `keeper/internal/api/huma_full_spec.go`), собирающий единую 3.1-спеку runtime-дампом huma-операций всех доменов ([ADR-054](../adr/0054-openapi-code-first.md#adr-054-operator-api--разворот-на-code-first-go-типы--openapi-через-huma-v2)). Committed [`openapi.yaml`](openapi.yaml) обновляется `make gen-openapi` (запись дампа) и предназначен для UI-vendor + git-ревью; `make check-openapi` — drift-guard (committed-снимок байт-в-байт == huma-дамп). Тот же дамп отдаёт `GET /openapi.yaml` self-serving endpoint (через `servedOpenAPIHandler`, без embed-копии). Спека используется и для генерации client SDK / TS-типов на стороне UI.

Разделение источников:
- **Go-типы handler-ов** (`keeper/internal/api/huma_*.go`) — форма: paths, methods, request/response schemas, имена схем, JSON-поля, enum-значения (через native enum-каталог `huma_enums.go`);
- производная [`openapi.yaml`](openapi.yaml) — снимок этой формы для внешних потребителей;
- этот документ — нормирование транспорта и семантики: status codes, permissions, conventions, mapping endpoint ↔ MCP-tool ↔ permission, граничные случаи.

Соответствие markdown ↔ код ↔ YAML: CamelCase-имена схем из этого документа выводятся huma из одноимённых Go-struct-ов handler-ов и попадают в `components.schemas.<Name>` производной YAML; snake_case JSON-поля — `json:"…"`-теги этих struct-ов. Enum-значения в коде и в JSON — короткие формы (`ready`) по правилу [§ Conventions → Enum serialization](#conventions).

## См. также

- [rbac.md](rbac.md) — каталог permissions, грамматика селекторов, Bootstrap первого Архонта.
- [mcp-tools.md](mcp-tools.md) — MCP-сторона каталога: транспорт, auth, формат tool declaration, `_apply_id`-convention, error mapping.
- [config.md → `listen.openapi.addr`](config.md#listen) — bind-адрес фасада. [config.md → `auth`](config.md#auth) — JWT-подпись.
- [push.md](push.md) — модель push-режима, источник правды по `POST /v1/push/apply`.
- [cloud.md](cloud.md) — Provider/Profile-семантика и Credentials-flow (REST-роуты `/v1/providers` / `/v1/profiles` реализованы и смонтированы, см. [§ Cloud](#cloud-8--реестры-cloud-provider--cloud-profile-adr-017--cloudmd)).
- [plugins.md](plugins.md) — `profile_schema` / `params_schema` плагинов, используется в `422 validation-failed`.
- [storage.md](storage.md) — реестр `operators`, `souls`, `incarnation`, `state_history` в Postgres.
- [`../architecture.md → ADR-013`](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) — Bootstrap первого Архонта.
- [`../architecture.md → ADR-014`](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon) — identity-модель, JWT-claims.
- [`../architecture.md → ADR-019`](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) — миграции state_schema, цикл upgrade.
- [`../architecture.md → Incarnation`](../architecture.md#incarnation--runtime-инстанс-сервиса) — структура записи, `state_history`, `error_locked`-семантика.
- [`../naming-rules.md`](../naming-rules.md) — словарь имён (Archon, AID, KID, SID).
- [`../requirements.md`](../requirements.md) — OpenAPI и MCP как сквозные требования.
