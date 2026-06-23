# RBAC и операторы

RBAC встроен в Keeper из коробки ([requirements.md](../requirements.md)) и применяется к **OpenAPI / MCP / push-операциям единообразно**: один и тот же набор политик решает, может ли оператор завести Soul, дёрнуть `push.apply`, прочитать Soulprint конкретного coven-а или создать Provider.

Модель — классическая trio «операторы (Архонты) ↔ роли ↔ permissions». Всё хранится в Postgres ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)): Архонты ([ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) — реестр `operators`, роли и их permissions — таблицы `rbac_roles` / `rbac_role_permissions`, привязка «оператор ↔ роль» (**membership**) — таблица `rbac_role_operators`. Управление — через OpenAPI/MCP (`role.*`-permissions, [§ Каталог permissions](#каталог-permissions)), не правкой YAML.

> **Hard-cut: блок `rbac:` в `keeper.yml` удалён ([ADR-028(g)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)).** До ADR-028 роли / permissions / membership декларировались в `keeper.yml::rbac` — это давало BUG-1 (`keeper init` создаёт Архонта в БД, но membership кладёт в JWT-claim, который enforcer не читает, резолвя его из YAML → bootstrap-Архонт получает `403`). Ключ `rbac:` теперь отвергается парсером как `unknown_key`; типы `config.KeeperRBAC` / `config.RBACRole` / поле `KeeperConfig.RBAC` удалены. Легаси-инсталляций нет, миграции YAML→БД не требуется.

## Storage — три PG-таблицы

RBAC материализован в Postgres ([ADR-028(a)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres), схемы — [storage.md](storage.md)):

| Таблица | Колонки | Роль |
|---|---|---|
| **`rbac_roles`** | `name` PK (kebab-case, CHECK на формат), `description`, `builtin` BOOL, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able | Каталог ролей. `builtin=true` запрещает `role.delete` / `role.update` (встроенная роль — `cluster-admin`, см. [§ Встроенные роли](#встроенные-роли)). `created_by_aid IS NULL` — seed-роли без инициатора-Архонта. |
| **`rbac_role_permissions`** | `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `permission` TEXT, PK `(role_name, permission)` | Permissions роли. `permission` хранится **RAW-строкой** и парсится `ParsePermission` ([§ Формат permissions](#формат-permissions)) — БД строку не интерпретирует. |
| **`rbac_role_operators`** | `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `aid` FK→`operators(aid)`, `granted_at`, `granted_by_aid` FK→`operators(aid)` NULL-able, PK `(role_name, aid)` | **Membership** «роль ↔ оператор». Отсутствие этого слоя раньше было причиной BUG-1 — membership-у негде было персистентно жить так, чтобы его видели и `keeper init`, и enforcer на всех нодах. |

**Термин «FK».** «FK» здесь — **настоящий PG foreign key** (`rbac_role_operators.aid` / `granted_by_aid` / `created_by_aid` → `operators(aid)`). Прежний метафорический «FK» YAML-списка `roles[].operators` — это **membership** (строка `rbac_role_operators`), не ссылка в файле ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres), [naming-rules.md → RBAC](../naming-rules.md#rbac-реестр-ролей-membership-и-термин-fk)).

## Базовая политика

| Понятие | Смысл |
|---|---|
| **`default_policy: deny`** | По умолчанию любое действие, не покрытое явной allow-permission, **запрещено**. Без exception-ов. Это инвариант enforcer-а (не конфиг-поле после hard-cut). |
| **Роль** | Запись `rbac_roles` (`name` kebab-case) + набор permissions (`rbac_role_permissions`) + набор привязанных AID (`rbac_role_operators`). |
| **Permission** | Строка `rbac_role_permissions.permission`: либо `*` (всё), либо `<resource>.<action>` с опциональным фильтром `on <selector>` ([§ Формат permissions](#формат-permissions)). |
| **Membership** | Строка `rbac_role_operators` `(role_name, aid)` — «AID имеет эту роль». |

## Пример (логическая модель)

Роль `db-operator`, привязанная к двум Архонтам, с двумя permissions:

```
rbac_roles:
  name: db-operator        builtin: false   created_by_aid: archon-alice

rbac_role_permissions:
  (db-operator, "incarnation.* on service=redis-cluster,vault-cluster")
  (db-operator, "soul.list")

rbac_role_operators:
  (db-operator, archon-db-01)   granted_by_aid: archon-alice
  (db-operator, archon-db-02)   granted_by_aid: archon-alice
```

`db-operator` может делать любые операции над incarnation-ами сервисов `redis-cluster` и `vault-cluster` и видеть список Souls; всё остальное запрещено. Создаётся через OpenAPI/MCP: `role.create` (роль + permissions) + `role.grant-operator` (membership) — [§ Каталог permissions](#каталог-permissions).

## Формат permissions

Permission-строка — либо `*` (полный доступ, эквивалент cluster-admin), либо двух-уровневое имя `<resource>.<action>` с опциональным селектором `on <selector>`. Формально:

```
permission := "*" | <resource>.<action> ( " on " <selector> )?
resource   := [a-z][a-z0-9-]*
action     := "*" | [a-z][a-z0-9-]*
selector   := <key>=<values> | "regex='" <re2-pattern> "'" | "soulprint='" <cel-predicate> "'"
key        := "service" | "coven" | "incarnation" | "host"
values     := <value> ( "," <value> )*
value      := [a-zA-Z0-9_.-]+
```

Правила, не выражаемые грамматикой:

- **Ровно два сегмента.** `incarnation.create` — валидно; `keeper.incarnation.create` — невалидно (три сегмента; имя такого вида — это **MCP-tool name**, не permission).
- **Wildcard только в `<action>`.** `incarnation.*` — валидно (все действия над `incarnation`). `*.create` — **не** поддерживается в MVP. Полный wildcard — `*` (без точки), отдельный case.
- **Whitespace.** Между `<resource>.<action>` и `on` — ровно один пробел; между ключом и значением селектора — `=` без пробелов; между значениями — `,` без пробелов.
- **Регистр.** Permission-имена, ключи селектора и значения — case-sensitive. Канон — lower-case (`coven=`, не `Coven=`).
- **Kebab-case в action.** Дефис в `<action>` допустим (`operator.issue-token`); пробел и подчёркивание — нет.

Расширение грамматики (новые ключи селектора, wildcards в значениях, новые формы) — отдельный PR в `rbac.md` с обоснованием. Добавление новых ключей — не breaking (старые роли продолжают валидироваться).

## Permission ↔ MCP-tool / OpenAPI endpoint

**Соответствие 1:1.** Каждая permission двух-сегментного формата `<resource>.<action>` контролирует:

- MCP-tool keeper-side с именем `keeper.<resource>.<action>` (4-сегментное имя).
- Соответствующий OpenAPI endpoint (POST `/v1/<resource>/<action>` или подобный).

Пример: permission `incarnation.create` даёт право вызвать MCP-tool `keeper.incarnation.create` или HTTP endpoint `POST /v1/incarnations`. Полное соответствие MCP-tool ↔ OpenAPI endpoint ↔ permission нормировано в [operator-api.md → Mapping endpoint ↔ MCP-tool ↔ permission](operator-api.md#mapping-endpoint--mcp-tool--permission); MCP-сторона (формат tool declaration, транспорт, async-convention, input/output schemas) — в [mcp-tools.md](mcp-tools.md).

## Грамматика селектора

Селектор — однопеременный фильтр `<key>=<v1>,<v2>,…`, где `<key>` — из closed enum:

| Ключ | Семантика | Источник значения при матчинге запроса |
|---|---|---|
| `service=` | Permission ограничен incarnation-ами указанных Service-типов. | **incarnation-операции:** `incarnation.service` (имя Service из git, [architecture.md → Артефакты](../architecture.md#артефакты-soul-stack-что-в-git-что-в-бд)). На create — `service` из тела запроса. |
| `coven=` | Permission ограничен указанными Coven-метками. | **incarnation-операции** (run / destroy / upgrade / get / history): declared `incarnation.covens` ∪ `{incarnation.name}` — env-теги incarnation плюс её имя как корневая Coven-метка ([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги), amendment a). На create — declared `covens` из тела запроса ∪ `{name}`. **soul-операции** (`soul.coven-assign` / `soul.list`): метка хоста из тела/query (`souls.coven[]`). |
| `incarnation=` | Permission ограничен конкретным instance. | `incarnation.name` целевого instance. |
| `host=` | Permission ограничен конкретным хостом. **Источник контекста — path/body мутации** (`soul.issue-token` / `soul.ssh-target-update` / `errand.run`), где SID известен до handler-а. На read-видимости souls (`soul.list` и покрываемые ею get/soulprint/history) `host=`-селектор на gate-этапе НЕ применяется — read гейтится existence-`RequireAction`, сужение по scope делает handler (ADR-047 §г G1, [§ Двухслойная авторизация read-эндпоинтов](#двухслойная-авторизация-read-эндпоинтов-adr-047-г-g1)). | SID хоста ([identity.md](../soul/identity.md)) из path/body мутации. |
| `regex='…'` | Permission ограничен хостами, чей SID/имя матчит RE2-паттерн ([ADR-047](../architecture.md) S2a). | SID/имя хоста — `host`- или `sid`-ключ контекста запроса. |
| `soulprint='…'` | Permission ограничен хостами, чьи факты удовлетворяют CEL-предикату `soulprint.self.*` ([ADR-047](../architecture.md) S2b, [ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)). | Факты хоста (`SoulprintFacts`) — подаёт резолвер list-видимости/target (слайсы S3/S4). |

**Множественные значения** перечисляются через запятую без пробелов (`coven=db,cache`), матчинг — exact-match по каждому значению; OR-логика среди значений одного селектора (`coven=db,cache` = «coven `db` ИЛИ `cache`»).

**regex-ключ (ADR-047 S2a).** Значение — один RE2-паттерн (Go `regexp`, консистентно с UI `compileSidRegex`) в **одинарных кавычках**: `incarnation.run on regex='^web-.*'`. Кавычки обязательны и отделяют regex от `,`-разделителя value-list — запятая внутри regex (`{1,3}`) не рвёт значение; спецсимволы regex не проходят `reSelValue`, поэтому незакавыченная форма отвергается на load. Один паттерн на ключ (multi-regex через `,` неоднозначен с regex-запятой); union нескольких regex набирается несколькими ролями/permission-ами. Паттерн компилируется при load снимка — битый regex фейлит load (как unknown-permission); длина ограничена 256 символами (RE2 без catastrophic-backtracking, cap — страховка). Матчинг — `regexp.MatchString` против `host`/`sid`-контекста; запрос без host/sid-ключа → deny (как exact-ключ без своего ключа). **РЕАЛЬНОЕ применение** regex к видимости list-эндпоинтов и пересечению target-ов — слайсы S3/S4; в S2a regex участвует в `Check`-матчинге (host-context-эндпоинты) и least-privilege subset.

**regex в least-privilege subset.** Покрытие одного regex другим (`^web-` ⊇ `^web-prod-`?) статически неразрешимо в общем случае. MVP — **string-equality fail-closed**: caller вправе выдать regex-permission, только если у него есть `*`, либо матчащая bare-permission (не ограничен по regex), либо матчащая permission с **идентичной** regex-строкой. Иной/более узкий regex → DENY (безопасно). Containment regex не реализуется.

**soulprint-ключ (ADR-047 S2b).** Значение — один CEL-предикат по фактам хоста в **канонической форме `soulprint.self.*`** ([ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)) в **одинарных кавычках**: `incarnation.run on soulprint='soulprint.self.os.family == "debian"'`. Кавычки обязательны и отделяют CEL от `,`-разделителя value-list — пробелы/двойные кавычки/запятые внутри предиката не рвут значение; незакавыченная форма отвергается на load. Один предикат на ключ; union нескольких — несколькими ролями/permission-ами. Предикат компилируется при load снимка через `shared/cel` (sandbox-режим `NewFlowControl`: объявлен `soulprint.self.*`, запрещены `vault()`/`now()`, host-аксессор `soulprint.hosts`/`soulprint.where`) — битый/использующий `vault`/`now`/`state`/`soulprint.hosts` предикат фейлит load (как unknown-permission); длина ограничена 512 символами. **РЕАЛЬНЫЙ CEL-eval** против `SoulprintFacts` хоста — слайсы S3/S4 (видимость list-эндпоинтов / пересечение target-ов): там резолвер подаёт факты. В `Check`-матчинге S2b soulprint-измерение **fail-closed (deny)** — request-context (`map[string]string`) не несёт nested-факты хоста; soulprint участвует в S2b в грамматике, `Purview.SoulprintExprs` и least-privilege subset.

**soulprint в least-privilege subset.** Логический containment одного CEL-предиката другим статически недоказуем. MVP — **string-equality fail-closed** (как regex): caller вправе выдать soulprint-permission, только если у него есть `*`, либо матчащая bare-permission (не ограничен по soulprint), либо матчащая permission с **идентичным** предикатом. Иной/более узкий предикат → DENY.

**Multi-value coven у incarnation-операций.** Источник `coven=` для incarnation — *множество* (declared `covens` ∪ `{name}`), а enforcer оперирует одним значением на ключ. Поэтому permission допускается, если её `coven=`-значение совпадает с **хотя бы одной** меткой из этого множества (OR по кандидатам). Пример: incarnation `redis-prod` с `covens=[prod, dc1]` имеет эффективное coven-множество `{prod, dc1, redis-prod}`; роль `incarnation.run on coven=prod` её матчит, роль `incarnation.run on coven=dev` — нет. Имя incarnation всегда входит в множество как корневая Coven-метка (ADR-008), поэтому роль `incarnation.* on coven=redis-prod` работает даже у incarnation без env-тегов.

> **История (ADR-008 amendment a).** Ранее этот раздел декларировал источником `coven=` для incarnation «`soulprint.self.covens` целевого хоста» — но incarnation-эндпоинты не резолвят хосты на этапе RBAC-гейта (chicken-egg + волатильность), и код приземлял в context только `{incarnation: name}` без `coven`/`service`. Из-за этого роли `incarnation.* on coven=…` / `on service=…` молча НЕ матчили (enforcer на отсутствующий ключ → deny). Источником стали стабильные атрибуты самой incarnation (declared `covens` ∪ `name` + `service`), резолвимые из её строки в Postgres.

**Wildcards (`*`) в значениях запрещены в MVP** — расширение требует отдельного ADR (формы экранирования, семантика для FQDN-имён хостов, поведение пустого значения). Парсер (`reSelValue = ^[a-zA-Z0-9_.-]+$`) отвергает `*` как значение селектора на load снимка, поэтому формы `coven=*` **не существует** как загруженной permission. Для `soul.coven-assign` это значит: unrestricted-scope (любая метка, любой хост) достигается **bare**-permission `soul.coven-assign` (без `on coven=…`) либо полным `*`-permission — не через `coven=*`.

**`namespace=`** (фильтр по namespace плагина) пока не вводим — RBAC на plugin-namespace не входит в MVP-сценарии. Появится при необходимости.

Расширение enum-а ключей — через PR в `rbac.md`; добавление новых ключей не ломает существующие роли (старые правила продолжают валидироваться, новые ключи опциональны).

## Семантика конфликта

- **OR-логика среди allow-permissions.** У Архонта может быть несколько ролей; permission матчится, если **хотя бы одна** roles[].permissions[] этого Архонта удовлетворяет запросу. Конфликт между ролями — union permissions.
- **Нет deny-permissions.** В MVP не поддерживаются явные `deny <permission>`. Любое разрешение задаётся allow-правилом; всё прочее запрещено `default_policy: deny`. Расширение — отдельный ADR при появлении реального сценария «разрешить всё, кроме X».
- **`default_policy: deny`.** Любое действие, не покрытое явной allow-permission, отвергается. Это встроенный инвариант enforcer-а ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) — после hard-cut `rbac:` нет конфиг-поля `default_policy`); `allow`-режим как опция за пределами dev/test не предусмотрен.
- **Union селекторов одной permission.** Если две роли одного оператора дают `incarnation.create on service=foo` и `incarnation.create on service=bar` — эффективный селектор = `service=foo,bar`.

Алгоритм проверки запроса на permission `incarnation.create` с контекстом `{service: redis-cluster, incarnation: redis-prod, …}`:

1. Найти все роли Архонта по membership (`rbac_role_operators` где `aid = <запрашивающий AID>`).
2. Развернуть в плоский список permissions (`rbac_role_permissions` по найденным ролям).
3. Для каждой permission: матчит ли `<resource>.<action>` запрос (с учётом wildcard `*` в `<action>`); если есть селектор — матчит ли он контекст (key есть в контексте, значение из values совпадает).
4. Если **хотя бы одна** permission матчит → allow. Иначе → deny.

Шаги 1–2 идут по **in-memory снимку** enforcer-а, не по живому SQL (см. [§ Как enforcer резолвит](#как-enforcer-резолвит)).

## Как enforcer резолвит

Интерфейс `PermissionChecker.Check` **не ходит в Postgres на каждый запрос** ([ADR-028(d)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)) — он матчит по in-memory снимку `map[AID][]*Role`.

- **Источник снимка — БД.** Снимок строится тремя SELECT-ами по `rbac_roles` ⋈ `rbac_role_permissions` ⋈ `rbac_role_operators` (вместо прежнего парсинга `keeper.yml::rbac`). Permission-строки парсятся `ParsePermission` при построении снимка.
- **Обновление снимка — B2 (реализовано).** Снимок инвалидируется через Redis pub/sub: каждая мутация роли / permissions / membership публикует сигнал в топик **`rbac:invalidate`** (envelope `{origin_kid, at}`), и все ноды перечитывают снимок из БД. **Self-filter по KID**: нода игнорирует собственный сигнал (паттерн `applybus` — публикующая нода уже обновила снимок в той же транзакции мутации). **B1 TTL-poll остаётся fallback**: фоновая goroutine (`rbac.Holder`) перечитывает снимок из БД с фиксированным интервалом (`DefaultRefreshInterval` = 10s) — best-effort страховка на случай недоступного / потерявшего сигнал Redis; на сбой перечита остаётся прежний снимок + warn. Окно устаревания (секунды, до следующего TTL-перечита при потере сигнала) приемлемо: мутации ролей/membership редки.
- **Self-lockout-проверки — из БД, не из снимка.** Инвариант «≥1 активный `*`-admin останется» (см. [§ Встроенные роли](#встроенные-роли)) проверяется **не** по in-memory снимку enforcer-а, а прямым SQL под `SELECT … FOR UPDATE` на `rbac_role_operators` / `rbac_role_permissions` / `operators` в той же транзакции, что и мутация. Снимок устаревает на TTL-окно — решение по нему дало бы staleness-дыру (можно снять последнего админа, если снимок ещё «помнит» уже-revoked-нутого второго); `FOR UPDATE` дополнительно сериализует конкурентные lockout-операции на разных нодах. См. [§ Управление ролями](#управление-ролями-rest--mcp).
- **RBAC вне hot-reload-config-пути.** Снимок перестраивается из БД (Redis pub/sub-инвалидация через топик `rbac:invalidate` + TTL-poll fallback), **не** по `SIGHUP` / config-swap ([ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml), уточнено [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)). Ревокация роли / membership-а (`role.revoke-operator` / `role.delete`) — `DELETE` в БД + перечит снимка, эффект для всех будущих проверок не позднее TTL-окна (отдельно от неотзываемости активного JWT до `exp` — [ADR-014(d)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).
- **`Check`-интерфейс неизменен** — меняется только источник снимка и механизм его обновления, не сигнатура проверки.

## Управление ролями (REST + MCP)

RBAC-CRUD (роли, permissions, membership) управляется через OpenAPI / MCP — Фаза 2 [ADR-028(e)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) (не правкой YAML). Один источник правды — `rbac.Service`: REST-handler-ы (`/v1/roles`) и MCP-tool-ы (`keeper.role.*`) — тонкие транспортные обёртки над ним, бизнес-инварианты (builtin-граница, self-lockout, валидация name/permission) живут в Service.

### REST `/v1/roles`

Шесть эндпоинтов. RBAC-проверка — в middleware (`role.*`-permission без селектора), до handler-а. Ошибки — RFC 7807 ([operator-api.md → Типы ошибок](operator-api.md#типы-ошибок)).

| Метод + путь | Permission | Тело / path | Успех | Коды ошибок |
|---|---|---|---|---|
| `POST /v1/roles` | `role.create` | body `{name, description?, permissions[]}` | `201` (тело пустое) | `403 forbidden` (least-privilege: право вне набора caller-а); `409 role-already-exists`; `422 validation-failed` (битый `name` / `permission`); `400 malformed-request` |
| `GET /v1/roles` | `role.list` | — | `200 {items: [...]}` | `500 internal-error` |
| `DELETE /v1/roles/{name}` | `role.delete` | path `name` | `204` | `404 role-not-found`; `409 role-builtin`; `409 would-lock-out-cluster` |
| `PATCH /v1/roles/{name}/permissions` | `role.update` | path `name` + body `{permissions[]}` (replace) | `204` | `403 forbidden` (least-privilege: добавляемое право вне набора caller-а); `404 role-not-found`; `409 role-builtin`; `409 would-lock-out-cluster`; `422 validation-failed`; `400 malformed-request` |
| `POST /v1/roles/{name}/operators` | `role.grant-operator` | path `name` + body `{aid}` | `204` | `403 forbidden` (least-privilege: роль содержит право вне набора caller-а); `404 role-not-found`; `404 not-found` (AID не существует); `422 validation-failed` (пустой/битый AID); `400 malformed-request` |
| `DELETE /v1/roles/{name}/operators/{aid}` | `role.revoke-operator` | path `name`, `aid` | `204` | `404 not-found` (пары `(name, aid)` нет); `409 would-lock-out-cluster`; `422 validation-failed` (битый path-AID) |

- **`GET /v1/roles` items[]** — `{name, description, builtin, permissions[], operators[]}`; `permissions` / `operators` сериализуются non-nil массивом (`[]`, не `null`).
- **`grant-operator` идемпотентен** — повторная привязка той же пары `(name, aid)` — no-op (`204`).
- **`granted_by_aid`** на grant берётся из JWT-claim caller-а.

### MCP `keeper.role.*`

1:1 с REST: `keeper.role.<action>` ↔ `role.<action>` ↔ один эндпоинт `/v1/roles`. Input-схемы и mapping ошибок RFC 7807 → MCP-tool error — в [mcp-tools/roles.md](mcp-tools/roles.md). Mutating-tool-ы возвращают пустой output-объект (`{}`), `keeper.role.list` — `{roles: [...]}`.

### Инвариант self-lockout: четыре пути

Инвариант [ADR-028(f)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) / [§ Встроенные роли](#встроенные-роли): после операции в кластере обязан остаться **≥1 активный** (`revoked_at IS NULL`) Архонт с эффективным `*`-permission. Четыре мутации могут его нарушить:

| Путь | Когда проверка срабатывает | Что считается «выжившими» |
|---|---|---|
| `operator.revoke` | Отзыв Архонта, держащего `*` ([ADR-013(c)](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)). | Активные `*`-admins, кроме отзываемого AID. |
| `role.delete` | Удаляемая роль даёт `*`. | Активные `*`-admins через **другие** роли (≠ удаляемая). |
| `role.update` | Старый набор роли давал `*`, новый — нет (снятие `*`). | Активные `*`-admins через другие роли. Если новый набор тоже даёт `*` — проверка не нужна. |
| `role.revoke-operator` | Роль даёт `*` и снимается membership. | Активные `*`-admins после исключения ровно пары `(role, aid)` — AID остаётся, если держит `*` ещё через другую роль; роль остаётся для других AID. |

Нарушение → `409 would-lock-out-cluster` (общий problem-type для operator- и role-путей, [naming-rules.md → Error codes](../naming-rules.md#error-codes)). Self-lockout — защита «вниз» (нельзя запереть admin-set). От эскалации «вверх» (нельзя выдать право, которого не имеешь) защищает отдельный [§ Инвариант least-privilege](#инвариант-least-privilege-subset-check); `role.create` / `role.grant-operator` ему подчиняются, хотя self-lockout-у — нет.

**Проверка — из БД под `FOR UPDATE`, не из снимка enforcer-а** (см. [§ Как enforcer резолвит](#как-enforcer-резолвит)). Контрольный SQL берёт row-lock на `rbac_role_operators` / `rbac_role_permissions` / `operators` в той же транзакции, что и мутация: исключает целевую роль/пару из выборки и проверяет, что осталась ≥1 строка. Снимок устаревает на TTL-окно — по нему проверка была бы дырой; `FOR UPDATE` сериализует параллельные lockout-операции (две tx, снимающие `*` разными путями, не могут обе пройти).

### Инвариант least-privilege (subset-check)

Отдельная от self-lockout защита — против **вертикальной эскалации привилегий**. Без неё оператор с `role.create` + `role.grant-operator` (но **без** `*`) мог бы: создать роль с `permissions: ["*"]` → привязать её себе через `role.grant-operator` → стать эффективным cluster-admin. То есть права на *управление ролями* конвертировались бы в *любые* права.

**Инвариант: оператор не может выдать через роль permission, которым не обладает сам.** Три мутации подчиняются ему:

| Путь | Что проверяется против эффективного набора caller-а |
|---|---|
| `role.create` | **каждая** permission новой роли. |
| `role.update` | **каждая ДОБАВЛЯЕМАЯ** permission (которой не было в старом наборе). Удаление прав **не** ограничивается — урезание чужой роли не эскалация. |
| `role.grant-operator` | **каждая** permission **грантящейся роли** (иначе обход: cluster-admin создал мощную роль, suboperator с `role.grant-operator` привязал её себе/другому и поднялся). |

- **Покрытие** — та же семантика implication, что у `Check` ([§ Как enforcer резолвит](#как-enforcer-резолвит)): caller «имеет» permission `P`, если хотя бы одна его permission матчит `P` (с учётом `*` → покрывает всё; `resource.*` → покрывает любой action этого resource; селектор `on key=a,b` → caller обязан покрывать **каждое** значение). Выдать full-wildcard `*` может только владелец `*`.
- **cluster-admin (`*`)** проходит любую такую проверку — его набор покрывает всё.
- **Источник набора caller-а — БД** (та же транзакция мутации, фильтр `operators.revoked_at IS NULL`), не снимок enforcer-а: same-tx-read свежее TTL-снимка. Read-only (без `FOR UPDATE`): subset-check — authorization-гейт, а не consistency-инвариант как self-lockout, поэтому он не добавляет row-lock-ов и не трогает детерминированный lock-порядок self-lockout-ядра (нет deadlock-риска).
- **Bootstrap-грант** (`keeper init`, `granted_by_aid IS NULL`, без caller-Архонта) subset-check не проходит — он привязывает первого Архонта к `cluster-admin` до появления любого субъекта.
- Нарушение → `403 forbidden` (REST `TypeForbidden` / MCP `forbidden`), sentinel `ErrPermissionNotHeld` — отдельный от `ErrPermissionDenied` («нет права на саму операцию», проверяется middleware/tool до Service).

Self-lockout и least-privilege **сосуществуют**: первый запрещает запереть admin-set «вниз», второй — выдать право «вверх». Они проверяют разные вещи и не конфликтуют по порядку.

### Builtin-граница

`cluster-admin` (`builtin=true`, [§ Встроенные роли](#встроенные-роли)):

- `role.delete` / `role.update` над ней — **запрещены** → `409 role-builtin` (проверка builtin идёт **до** self-lockout; builtin важнее).
- `role.grant-operator` / `role.revoke-operator` над ней — **разрешены** (иначе нельзя добавить второго админа или снять ошибочно назначенного), с тем же self-lockout-ом на revoke.

## Управление группами архонов (Synod)

**Synod** ([ADR-049](../adr/0049-synod.md#adr-049-synod--группа-архонов)) — промежуточный уровень модели **Архон → Synod → Роли**: группа архонов, **бандлящая набор ролей**. Вместо того чтобы грантить каждому архону роли по отдельности, оператор собирает группу с нужным bundle ролей и добавляет в неё архонов — все члены автоматически получают весь bundle.

- **Synod своего scope НЕ несёт.** Scope (селекторы `on coven=…` / `on service=…` / `regex` / `soulprint`) живёт на ролях ([Purview](#двухслойная-авторизация-read-эндпоинтов-adr-047-г-g1) / [ADR-047](../adr/0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)); Synod лишь группирует уже-scoped роли. Group-scope ADR-049 не вводит (additive на будущее).
- **Synod плоская** — группа не содержит другие группы (вложенность — additive-расширение).
- **Архон может входить в несколько групп.**

### Эффективные роли = прямые ∪ через Synod

Эффективные роли архона = **прямые** (membership `rbac_role_operators`) **∪** роли через **все его Synod-ы** (`synod_operators` ⋈ `synod_roles`). Объединение собирается при построении in-memory снимка enforcer-а ([§ Как enforcer резолвит](#как-enforcer-резолвит)) — слой матчинга `Check` источник роли не различает (роль через прямой грант и роль через группу эквивалентны). Дубль роли (через прямой грант И группу, либо через две группы) идемпотентен — union множества, не мультимножество.

### Storage — три PG-таблицы (паттерн `rbac_*`)

Реестр Synod материализован тем же паттерном, что `rbac_*` ([миграция 069](../adr/0049-synod.md#adr-049-synod--группа-архонов), схемы — [storage.md](storage.md)):

| Таблица | Колонки | Роль |
|---|---|---|
| **`synods`** | `name` PK (kebab-case, CHECK `^[a-z][a-z0-9-]*$`), `description`, `builtin` BOOL, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able | Каталог групп. `builtin=true` запрещает `synod.delete` (симметрия `rbac_roles.builtin`). |
| **`synod_operators`** | PK `(synod_name, aid)`; `synod_name` FK→`synods(name)` `ON DELETE CASCADE`, `aid` FK→`operators(aid)` `ON DELETE CASCADE`, `added_at`, `added_by_aid` FK→`operators(aid)` NULL-able | **Membership** «Synod ↔ архон». CASCADE с обеих сторон: удаление группы или архона авто-чистит membership. |
| **`synod_roles`** | PK `(synod_name, role_name)`; `synod_name` FK→`synods(name)` `ON DELETE CASCADE`, `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `granted_at`, `granted_by_aid` FK→`operators(aid)` NULL-able | **Bundle** «Synod ↔ роль». CASCADE с обеих сторон: удаление группы чистит bundle, удаление роли снимает её из всех групп. |

### REST `/v1/synods`

Восемь эндпоинтов. RBAC-проверка — в middleware (`synod.*`-permission, NoSelector), до handler-а. Один источник правды — `rbac.Service`: REST-handler-ы и MCP-tool-ы `keeper.synod.*` ([mcp-tools/synods.md](mcp-tools/synods.md)) — тонкие транспортные обёртки. Ошибки — RFC 7807 ([operator-api.md → Типы ошибок](operator-api.md#типы-ошибок)).

| Метод + путь | Permission | Тело / path | Успех | Коды ошибок |
|---|---|---|---|---|
| `POST /v1/synods` | `synod.create` | body `{name, description?}` | `201` (тело пустое) | `409 synod-already-exists`; `422 validation-failed` (пустой/битый `name`); `400 malformed-request` |
| `GET /v1/synods` | `synod.list` | — | `200 {items: [...]}` | `500 internal-error` |
| `PATCH /v1/synods/{name}` | `synod.update` | path `name` + body `{description}` (required, 1..1024 символов) | `204` | `404 synod-not-found`; `422 validation-failed` (пустой `description` / превышение лимита); `400 malformed-request` (битый JSON, неизвестное поле — в т.ч. `name` в теле) |
| `DELETE /v1/synods/{name}` | `synod.delete` | path `name` | `204` | `404 synod-not-found`; `409 synod-builtin`; `409 would-lock-out-cluster` |
| `POST /v1/synods/{name}/operators` | `synod.add-operator` | path `name` + body `{aid}` | `204` | `403 forbidden` (least-privilege: bundle группы содержит право вне набора caller-а); `404 synod-not-found`; `404 not-found` (AID не существует); `422 validation-failed` (пустой/битый AID); `400 malformed-request` |
| `DELETE /v1/synods/{name}/operators/{aid}` | `synod.remove-operator` | path `name`, `aid` | `204` | `404 not-found` (пары `(name, aid)` нет); `409 would-lock-out-cluster`; `422 validation-failed` (битый path-AID) |
| `POST /v1/synods/{name}/roles` | `synod.grant-role` | path `name` + body `{role}` | `204` | `403 forbidden` (least-privilege: роль содержит право вне набора caller-а); `404 synod-not-found`; `404 role-not-found`; `422 validation-failed` (пустой `role`); `400 malformed-request` |
| `DELETE /v1/synods/{name}/roles/{role_name}` | `synod.revoke-role` | path `name`, `role_name` | `204` | `404 not-found` (bundle-пары `(name, role)` нет); `409 would-lock-out-cluster` |

- **`GET /v1/synods` items[]** — `{name, description, builtin, roles[], operators[]}`; `roles` / `operators` сериализуются non-nil массивом (`[]`, не `null`), отсортированы детерминированно.
- **`add-operator` / `grant-role` идемпотентны** — повторное добавление той же пары — no-op (`204`).
- **`added_by_aid` / `granted_by_aid`** берутся из JWT-claim caller-а; у seed-/bootstrap-строк — `NULL`.

### Security-инварианты Synod

Обе защиты RBAC ([§ Инвариант self-lockout](#инвариант-self-lockout-четыре-пути), [§ Инвариант least-privilege](#инвариант-least-privilege-subset-check)) **обязаны учитывать роли через Synod** ([ADR-049 §f](../adr/0049-synod.md#adr-049-synod--группа-архонов)) — иначе через группу обходился бы любой из них. Эффективный `*`-permission архона может приходить **через Synod**, поэтому self-lockout проверяет и Synod-путь; член группы получает весь её bundle, поэтому least-privilege subset проверяет bundle/роль группы.

| Мутация | Защита | Что проверяется |
|---|---|---|
| `synod.add-operator` | **least-privilege subset** | Член получает **весь bundle ролей группы**. Caller обязан держать **все эффективные права** этого bundle (каждая роль развёрнута под своим `default_scope`), иначе `403 forbidden` (`ErrPermissionNotHeld`). Self-lockout **нет** — add только расширяет admin-set. |
| `synod.grant-role` | **least-privilege subset** | Роль выдаётся всем членам. Caller обязан держать **все эффективные права гранящейся роли** (под её `default_scope`), иначе `403 forbidden`. Self-lockout **нет**. Несуществующая роль → `404 role-not-found` (FK-violation, не ложный subset-pass). |
| `synod.delete` | **builtin-граница + self-lockout** | `builtin=true` → `409 synod-builtin` (**первой**, builtin важнее lockout). Если группа бандлит `*`-дающую роль и кто-то держал `*` только через неё → `409 would-lock-out-cluster`. |
| `synod.remove-operator` | **self-lockout** | Снятие отнимает у архона роли группы. Если группа даёт `*` и архон держал его только через неё → `409 would-lock-out-cluster` (исключается ровно пара `(synod, aid)`; `*` через прямой грант / другую группу остаётся). |
| `synod.revoke-role` | **self-lockout** | Снятие отнимает права роли у всех членов. Если снимаемая роль — последняя `*`-дающая роль группы и член держал `*` только через неё → `409 would-lock-out-cluster`. |

Self-lockout-проверки Synod — **из БД под `SELECT … FOR UPDATE`**, не из снимка enforcer-а (как и для role-путей, [§ Как enforcer резолвит](#как-enforcer-резолвит)): детерминированный lock-порядок (группа → её роли → admin-set), исключение целевой пары из Synod-ветки admin-set-probe, проверка «осталась ≥1 строка». Lockout-проверка запускается **только если** группа/роль реально бандлит `*`-дающую роль — иначе admin-set не уменьшается, лишний probe не нужен.

> **Гейт RBAC выше Synod не меняется.** Резолв «эффективные роли = прямые ∪ через Synod» — дополнение snapshot-сборки enforcer-а ([§ Как enforcer резолвит](#как-enforcer-резолвит)); матчинг-слой `Check` / Purview ([ADR-047](../adr/0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)) источник роли не различает и не переписывается.

## Каталог permissions

Полный список permission-имён, валидируемых Keeper-ом в MVP. Имена за пределами этого каталога отвергаются парсером `keeper.yml` с ошибкой `unknown_permission`.

### Operator (5) — [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)

| Permission | Семантика |
|---|---|
| `operator.create` | Создание нового Архонта в реестре `operators` (через OpenAPI/MCP). Первый Архонт создаётся не через эту permission, а через `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)). |
| `operator.revoke` | Установка `revoked_at` для существующего Архонта. Активные JWT продолжают работать до `exp` ([ADR-014(d)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). |
| `operator.issue-token` | Выпуск нового JWT для существующего Архонта (например, оператор потерял токен; другой оператор с этим правом выписывает новый). |
| `operator.list` | Перечисление Архонтов с фильтрами (`auth_method` / `revoked`). Покрывает также single-archon read `GET /v1/operators/{aid}` — паттерн one-permission-on-read, как `soul.list` / `service.list`. Селектор — NoSelector в MVP. |
| `operator.read` | Зарегистрирована forward-only в каталоге; в роутере MVP не используется (route монтирует `operator.list` на оба эндпоинта). Введена, чтобы конфиги ролей могли указывать её без `unknown_permission`. |

### Role (6) — [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)

Управление RBAC (роли, permissions, membership) через OpenAPI/MCP — RBAC-storage в Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`, [§ Storage](#storage--три-pg-таблицы)).

| Permission | Семантика |
|---|---|
| `role.create` | Создание роли (`rbac_roles` + её permissions в `rbac_role_permissions`). Нельзя включить permission вне набора caller-а ([§ Инвариант least-privilege](#инвариант-least-privilege-subset-check)). |
| `role.delete` | Удаление роли (каскадом permissions + membership). **Запрещено** над `builtin=true` (`cluster-admin`) и при нарушении self-lockout-инварианта ([§ Встроенные роли](#встроенные-роли)). |
| `role.list` | Перечисление ролей с их permissions и membership. |
| `role.update` | Изменение permissions роли. **Запрещено** над `builtin=true`; убрать `*` у роли, держащей единственный эффективный `*`, нельзя (self-lockout); добавить permission вне набора caller-а нельзя (least-privilege). |
| `role.grant-operator` | Привязка `(role, aid)` — добавление membership-строки в `rbac_role_operators`. Нельзя грантить роль с permission вне набора caller-а ([§ Инвариант least-privilege](#инвариант-least-privilege-subset-check)). |
| `role.revoke-operator` | Снятие membership-строки. **Запрещено**, если снимает последнего активного AID с эффективным `*` (self-lockout). |

### Synod (8) — [ADR-049](../adr/0049-synod.md#adr-049-synod--группа-архонов)

Управление **Synod-группами** (группы архонов, бандлящие роли — промежуточный уровень модели **Архон → Synod → Роли**, [§ Управление группами архонов](#управление-группами-архонов-synod)). Селектор — **NoSelector** (управление группами — кластер-уровневая операция без scope по coven/host, как `role.*` / `operator.*`; group-scope ADR-049 НЕ вводит). Мутирующие пишут audit, read-only `synod.list` — нет.

| Permission | Семантика | Audit-event |
|---|---|---|
| `synod.create` | Создание Synod-группы (`POST /v1/synods`). Пустая группа прав не выдаёт — least-privilege/self-lockout к create неприменимы (роли добавляются позже через `synod.grant-role`). | `synod.created` |
| `synod.update` | Правка **ТОЛЬКО `description`** группы (`PATCH /v1/synods/{name}`, ADR-049 amend). `name` (PK) **immutable** — rename сознательно не поддержан (нарушал бы инвариант immutable-идентификаторов; симметрия с `rbac_roles.name`). **builtin-граница НЕ применяется** — builtin-группа редактируется (`description` — косметика для UI/аудита, не поведение). **Без subset-check и self-lockout** (`description` прав не выдаёт и не отнимает — оба инварианта неприменимы); снимок enforcer-а не инвалидируется (`description` в матчинг не входит). | `synod.updated` |
| `synod.delete` | Удаление группы (каскадом membership + bundle, `DELETE /v1/synods/{name}`). **Запрещено** над `builtin=true` → `409 synod-builtin` (builtin важнее lockout, проверяется первой); запрещено, если исчезновение группы оставит кластер без эффективного `*`-админа → `409 would-lock-out-cluster` (self-lockout). | `synod.deleted` |
| `synod.list` | Перечисление групп с развёрнутыми ролями (bundle) и членами-AID (`GET /v1/synods`). | — (read-only) |
| `synod.add-operator` | Добавление архона в группу (`POST /v1/synods/{name}/operators`). Идемпотентно. **Под least-privilege subset:** член получает весь bundle ролей группы — caller обязан держать все эффективные права этого bundle, иначе `403 forbidden` ([§ Управление группами архонов](#управление-группами-архонов-synod)). | `synod.operator-added` |
| `synod.remove-operator` | Снятие архона из группы (`DELETE /v1/synods/{name}/operators/{aid}`). **Под self-lockout:** снятие отнимает у архона роли группы (в т.ч. `*`-дающую) — запрещено, если осиротит последнего `*`-админа → `409 would-lock-out-cluster`. | `synod.operator-removed` |
| `synod.grant-role` | Добавление роли в bundle группы (`POST /v1/synods/{name}/roles`). Идемпотентно. **Под least-privilege subset:** роль выдаётся всем членам группы — caller обязан держать все эффективные права роли, иначе `403 forbidden`. | `synod.role-granted` |
| `synod.revoke-role` | Снятие роли из bundle группы (`DELETE /v1/synods/{name}/roles/{role_name}`). **Под self-lockout:** снятие отнимает права роли у всех членов — запрещено, если это последняя `*`-дающая роль группы и кто-то держал `*` только через неё → `409 would-lock-out-cluster`. | `synod.role-revoked` |

### Incarnation (11) — [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) / [scenario/](../scenario/README.md) / [ADR-031](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)

| Permission | Семантика |
|---|---|
| `incarnation.create` | Создание нового instance — запуск сценария `create` сервиса. |
| `incarnation.create-rerun` | Перезапуск сценария `create` из `error_locked` (`POST /v1/incarnations/{name}/rerun-create`, [architecture.md → Атомарность и error_locked](../architecture.md#атомарность-и-error_locked)). Атомарно снимает блок (`state` НЕ трогается — last known-good, snapshot в `state_history`) и тем же действием перезапускает bootstrap (`error_locked → applying` минуя `ready` под одним `FOR UPDATE`). Отдельное право от `incarnation.create` (создание новой инкарнации) и `incarnation.unlock` (снятие блока без перезапуска): rerun требует явного `reason`. Scope ЖЁСТКО ограничен сценарием `create` — для прочих случаев обычный `unlock` + ручной `run`. Селекторы — те же, что у других incarnation-мутаций (`coven=`/`service=`/`incarnation=`). Audit-событие — `incarnation.create_rerun` (НЕ `incarnation.unlocked`). |
| `incarnation.run` | Запуск произвольного сценария (`add_user`, `restart`, любой другой из `scenario/`). |
| `incarnation.get` | Чтение `spec` + `state` + `status` instance. |
| `incarnation.list` | Перечисление инстансов (с фильтрами). |
| `incarnation.history` | Чтение `state_history` instance (snapshot per-change). |
| `incarnation.unlock` | Снятие статуса `error_locked` после ручной разборки последствий частичного сбоя. |
| `incarnation.upgrade` | Перевод instance на новую `state_schema_version` (запуск миграций, [migrations.md](../migrations.md)). |
| `incarnation.destroy` | Удаление instance (с tombstone-периодом для облачных VM, [cloud.md](cloud.md)). |
| `incarnation.check-drift` | Scry on-demand-проверка drift ([ADR-031](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): рендер `scenario/converge/` в `dry_run`-режиме + сборка `DriftReport`. Sync-операция (не async). Селекторы — те же, что у `incarnation.run` (`coven=`/`service=`/`incarnation=`). |
| `incarnation.update-hosts` | Изменение mutable-полей записи incarnation через Operator API. MVP-объём — declared `spec.hosts[]` (`PATCH /v1/incarnations/{name}/hosts`, три mode: replace/append/remove; ADR-008). Селекторы — те же, что у других incarnation-мутаций (`coven=`/`service=`/`incarnation=`). **Deprecated-alias:** прежнее имя `incarnation.update` канонизируется в `incarnation.update-hosts` на load снимка enforcer-а (backcompat — существующие роли с `incarnation.update` продолжают работать, не требуют миграции). |

### Soul (4) — реестр хостов

| Permission | Семантика |
|---|---|
| `soul.create` | Регистрация нового хоста в реестре `souls` (`status: pending`) и выпуск первого bootstrap-токена ([onboarding.md](../soul/onboarding.md)). `transport` (`agent`/`ssh`) обязателен; для `ssh` bootstrap-токен не выпускается. |
| `soul.issue-token` | Повторный выпуск bootstrap-токена для существующей Soul с `transport: agent` (потеря токена, плановая ре-выписка). При уже-активном токене — отказ `409`, если не указан `force=true`. Для `transport: ssh` — `422` (ssh-хост не имеет bootstrap-фазы). **Мутация** — scope-aware gate `RequirePermission`, селектор `host=<sid>` из path (контекст известен до handler-а, в отличие от read-видимости). |
| `soul.list` | Перечисление Souls в реестре (с фильтрами по coven / status / transport). Покрывает также single-soul read `GET /v1/souls/{sid}`, soulprint-read `GET /v1/souls/{sid}/soulprint` и per-host history `GET /v1/souls/{sid}/history` — паттерн one-permission-on-read, как `service.list` / `omen.list` / `vigil.list` / `decree.list`. **Read-видимость авторизуется двухслойно** (ADR-047 §г G1, [§ Двухслойная авторизация read-эндпоинтов](#двухслойная-авторизация-read-эндпоинтов-adr-047-г-g1)): gate `RequireAction` (existence — держит ли `soul.list` в принципе) + сужение по scope в handler (`soulpurview` — coven-pushdown / regex-keyset / soulprint-CEL / `InScope`). **Селектор НЕ применяется на gate-этапе read-роутов** — scope резолвится из строк БД, которых ещё нет; selector-форма (`coven=`/`host=`) у роли всё равно сужает видимость в handler-е, но gate её игнорирует. host-селектор (`host=<sid>`) — это форма для **мутаций** souls (см. `soul.issue-token` / `soul.ssh-target-update`), не для read. |
| `soul.coven-assign` | Массовое назначение/снятие одной Coven-метки на хосты по селектору (`POST /v1/souls/coven`). Coven — холодная PG-метка: чистый UPDATE `souls`, без Redis. **Двухслойная авторизация:** middleware проверяет право вообще + назначаемую метку (гейт b — coven-scoped оператор проходит только для метки в своём `coven=`-scope); service-слой пересекает целевые хосты со scope-ом оператора (гейт a — `souls.coven && scope`). Без обоих гейтов bulk = privilege-escalation (оператор с `coven=dev` навесил бы `prod` на весь флот). |
| `soul.ssh-target-update` | Обновление per-host SSH-реквизитов push-flow (`PUT /v1/souls/{sid}/ssh-target`, ADR-032 amendment 2026-05-26, S7-1). Body: `{ssh_port, ssh_user, soul_path}`. Action — hyphenated (`ssh-target-update`), т.к. permission-грамматика — ровно `<resource>.<action>` (паттерн `sigil.key-introduce`); MCP-tool — 3-сегментный `keeper.soul.ssh-target.update`. **Мутация** — scope-aware gate `RequirePermission`, селектор `host=<sid>` из path. Audit `soul.ssh-target.updated`. |

Селектор `coven=` ([§ Грамматика селектора](#грамматика-селектора)) применим ко всем четырём: ограничивает действие хостами с указанными Coven-метками. Для **мутаций** (`soul.issue-token` / `soul.coven-assign` / `soul.ssh-target-update`) он сужает scope на gate-этапе (scope-aware `Check`, контекст из path/body); у `soul.coven-assign` дополнительно задаёт допустимый набор назначаемых меток и подмножество целевых хостов (scope-intersection). Для **read** (`soul.list` и покрываемые ею get/soulprint/history) coven-/regex-/soulprint-сужение делает handler через `soulpurview` (gate — лишь existence `RequireAction`), см. [§ Двухслойная авторизация read-эндпоинтов](#двухслойная-авторизация-read-эндпоинтов-adr-047-г-g1).

**Пример** — оператор управляет только dev-окружением:

```yaml
roles:
  - name: dev-coven-ops
    permissions:
      - "soul.coven-assign on coven=dev,stage"   # навешивает/снимает только dev|stage, только на dev|stage-хостах
      - "soul.list on coven=dev,stage"
```

С такой ролью `POST /v1/souls/coven {mode: append, label: dev, selector: {all: true}}` затронет лишь хосты с меткой `dev`/`stage`; попытка навесить `label: prod` отвергается `422` (метка вне scope), а хосты вне `dev`/`stage` не попадают в UPDATE.

Будущие кандидаты (`soul.revoke` для отзыва SoulSeed) — вводятся отдельным PR при появлении соответствующих API-операций. `soul.get` сознательно не вводится: single-soul read покрывается `soul.list` (паттерн service/omen/vigil/decree).

### Push (3) — [push.md](push.md)

| Permission | Семантика |
|---|---|
| `push.apply` | SSH-доставка Destiny на хост через модуль `keeper.push`. |
| `push.cleanup` | Чистка `/var/lib/soul-stack/` на хосте при `revoke` или выводе из реестра ([push.md → Cleanup](push.md#cleanup-на-хосте)). |
| `push.read` | Чтение состояния push-прогона (`GET /v1/push/{apply_id}`, Variant C orchestrator). |

### Push-Provider (5) — [push.md → S7-2 migration](push.md#s7-2-migration-to-push_providers-pg-table-2026-05-26)

CRUD реестра Push-Provider-ов — per-provider env-payload params SSH-плагинов push-flow (ADR-032 amendment 2026-05-26, S7-2). Сущность реализована как «SSH Provider» variant of Provider (см. amendment). Селектор — NoSelector (как `provider.*` / `service.*`).

| Permission | Семантика |
|---|---|
| `push-provider.create` | Создать запись в `push_providers` (`POST /v1/push-providers`). Sensitive params (secret_id/token/password/private_key) обязаны быть vault-refs. |
| `push-provider.update` | Заменить params существующей записи (`PUT /v1/push-providers/{name}`, replace-семантика). |
| `push-provider.delete` | Удалить запись (`DELETE /v1/push-providers/{name}`). |
| `push-provider.list` | Перечислить записи (`GET /v1/push-providers`). |
| `push-provider.read` | Прочитать одну запись (`GET /v1/push-providers/{name}`). |

### Errand (3) — [ADR-033](../adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)

| Permission | Семантика |
|---|---|
| `errand.run` | Запуск Errand на Soul через `POST /v1/souls/{sid}/exec` ([ADR-033](../adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)). Селекторы: `host=<sid>` / `coven=<label>`; bare — unrestricted. |
| `errand.cancel` | Отмена in-flight Errand через `DELETE /v1/errands/{errand_id}` (ADR-033 slice E5). Селектор — NoSelector (SID известен только после lookup-а строки errand-а, что несовместимо с pre-handler-middleware-check-ом). |
| `errand.list` | Чтение реестра Errand (`GET /v1/errands` + `GET /v1/errands/{errand_id}`). Read-only. Селекторы фильтруют видимость per-row. |

### Cadence (6) — [ADR-046](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)

CRUD реестра Cadence-расписаний (`cadences`) — расписание, спавнящее обычный [Voyage](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)-прогон по времени ([ADR-046 §7](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)). Селектор — NoSelector в MVP (CRUD оперирует самим реестром расписаний, паттерн `push-provider.*` / `operator.*`); per-name scope — отдельный slice при появлении мульти-тенант-RBAC. Мутирующие пишут audit (`cadence.created` / `cadence.updated` / `cadence.deleted`), read-only `cadence.list` — нет.

| Permission | Семантика |
|---|---|
| `cadence.create` | Создание Cadence-расписания (`POST /v1/cadences`). **Помимо route-level `cadence.create` срабатывает второй уровень guard-а — см. ниже [§ Двухуровневый guard](#cadence-двухуровневый-guard-create).** |
| `cadence.list` | Перечисление Cadence-расписаний (`GET /v1/cadences`) и деталь одного (`GET /v1/cadences/{id}`) — паттерн one-permission-on-read (как `soul.list` / `errand.list`). |
| `cadence.update` | Правка рецепта расписания (`PATCH /v1/cadences/{id}`). **Backcompat:** остаётся валидным грантом и для toggle (`enable`/`disable`) — роли со старым `cadence.update` сохраняют возможность паузы/возобновления (амендмент 2026-06-02). |
| `cadence.delete` | Снятие расписания (`DELETE /v1/cadences/{id}`). История порождённых Voyage сохраняется ([ADR-046 §9](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)). |
| `cadence.enable` | Возобновление расписания без удаления (`POST /v1/cadences/{id}/enable`). Гранулярное право; эндпоинт допускает `cadence.enable` **ИЛИ** `cadence.update` (OR-гейт, амендмент 2026-06-02). |
| `cadence.disable` | Пауза расписания без удаления (`POST /v1/cadences/{id}/disable`). Гранулярное право; эндпоинт допускает `cadence.disable` **ИЛИ** `cadence.update` (OR-гейт). |

`GET /v1/cadences/{id}/runs` (дочерние Voyage расписания, reuse Voyage-DTO) гейтится **`incarnation.history`** (NoSelector), а не `cadence.list` — это чтение истории дочерних прогонов, симметрично остальным history-эндпоинтам.

> Spawn-флоу (Reaper-лидер `spawn_due_cadence` → Insert дочернего Voyage по наступлении `next_run_at`) RBAC-permission **не контролируется** — это автономная инициатива фонового Reaper-правила, не операторский вызов ([ADR-046 §8](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage), audit-source `background` / `archon_aid: NULL`). Авторизация исполнения «зашита» в момент создания (двухуровневый guard ниже) — спавн идёт от имени `created_by_aid` расписания.

#### Cadence: двухуровневый guard на create

Право `cadence.*` управляет самим **расписанием**, но рецепт Cadence спавнит **Voyage**. Если бы для заведения Cadence хватало одного `cadence.create`, оператор без права запускать прогоны мог бы завести расписание, которое запускает их за него — это privilege-escalation-обход RBAC. Поэтому `POST /v1/cadences` проверяется **на двух уровнях** (параллель с двухслойной авторизацией Voyage-create и `soul.coven-assign`):

1. **Route-level (middleware):** `cadence.create` (NoSelector) — право управлять расписаниями вообще. Проверяется до handler-а.
2. **Body-level (handler):** Voyage-permission **по `kind` рецепта** (kind виден только из тела, поэтому проверка не в middleware, а в `CadenceHandler.Create`, parity Voyage-create):
   - `kind: scenario` → требуется **`incarnation.run`**;
   - `kind: command` → требуется **`errand.run`**.

Имена этих Voyage-permission и kind-маппинг — те же, что у разового Voyage-create ([ADR-043 §6](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)). Чтобы завести Cadence, нужны **оба** уровня: и право на управление расписанием, и право запускать то, что расписание будет спавнить. Нарушение второго уровня → `403 forbidden` (problem-detail вида `cadence recipe requires Voyage-permission <resource>.<action> by kind=<kind>`); неизвестный `kind` → `422 validation-failed`. Target в рецепте — выбор из RBAC-скоупа создателя на момент создания (parity [ADR-043 §5](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)).

### Herald / Tiding (10) — [ADR-052](../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)

CRUD реестров уведомлений о событиях прогонов: **Herald** (каналы доставки, `heralds`) и **Tiding** (правила подписки, `tidings`). Селектор — NoSelector (управление каналами/правилами кластер-уровневое, паттерн `push-provider.*` / `omen.*` / `role.*`); per-name scope — отдельный slice при появлении мульти-тенант-RBAC. Мутирующие пишут audit (`herald.created`/`updated`/`deleted` + `tiding.*`), read-only `*.list`/`*.read` — нет.

| Permission | Семантика |
|---|---|
| `herald.create` | Создать Herald-канал (`POST /v1/heralds`). Webhook-config под SSRF-guard; `secret_ref` — vault-ref на signing-token. |
| `herald.read` | Прочитать один Herald-канал (`GET /v1/heralds/{name}`). |
| `herald.list` | Перечислить Herald-каналы (`GET /v1/heralds`). |
| `herald.update` | Заменить mutable-поля канала (`PUT /v1/heralds/{name}`, replace-семантика как Push-Provider). |
| `herald.delete` | Удалить канал (`DELETE /v1/heralds/{name}`); каскадно сносит связанные Tiding-подписки. |
| `tiding.create` | Создать Tiding-правило подписки (`POST /v1/tidings`). `herald` — FK на существующий канал; `event_types` — area-glob в scope прогонов. |
| `tiding.read` | Прочитать одно Tiding-правило (`GET /v1/tidings/{name}`). |
| `tiding.list` | Перечислить Tiding-правила (`GET /v1/tidings`). |
| `tiding.update` | Заменить mutable-поля правила (`PUT /v1/tidings/{name}`, replace). |
| `tiding.delete` | Удалить правило (`DELETE /v1/tidings/{name}`). |

### Provisioning (2) — [ADR-058](../adr/0058-operator-auth-ldap-oidc.md)

Runtime-управление политикой способов **СОЗДАНИЯ** операторов — ключ `provisioning_allowed_methods` в `keeper_settings` (CSV из домена `{user,ldap,oidc}`). Политика гейтит ТОЛЬКО ветку создания оператора (`POST /v1/operators` → `user`; federated auto-provision → `ldap`/`oidc`); существующие операторы логинятся независимо от политики, `bootstrap`/`system` НЕ гейтятся никогда. ОТСУТСТВИЕ ключа = все способы разрешены (back-compat); заданный-но-пустой = config-error (anti-lockout — нельзя запретить ВСЕ способы и залочить заведение операторов). Селектор — **NoSelector** (политика кластер-уровневая, как `operator.*` / `role.*`). `update` пишет audit (`provisioning.policy_changed`), read-only `read` — нет.

| Permission | Семантика |
|---|---|
| `provisioning.read` | Прочитать текущую политику способов создания операторов (`GET /v1/provisioning-policy`). `policy_set=false` → политика не задана (дефолт: всё разрешено). |
| `provisioning.update` | Сменить политику (`PUT /v1/provisioning-policy`, replace-семантика). Пустой список → 422 (anti-lockout); метод вне `{user,ldap,oidc}` → 422. Аудируется (`provisioning.policy_changed`). |

### Audit (1) — [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)

Read-only-доступ к ленте audit-событий (`audit_log`) через `GET /v1/audit` (UI iteration 2). Сам факт чтения audit-таблицы НЕ пишется в audit (избегаем рекурсии — каждый GET удваивал бы таблицу).

| Permission | Семантика |
|---|---|
| `audit.read` | Чтение `audit_log` с фильтрами (`type` multi-value, `source` multi-value, `archon_aid`, `correlation_id`, `started_after`/`started_before`). Селектор — NoSelector в MVP; per-AID/coven-scope на audit-trail — отдельный slice при необходимости. |

### Cloud (2) — [cloud.md](cloud.md)

| Permission | Семантика |
|---|---|
| `provider.create` | Создание Provider record в Postgres (настроенная учётка облака). |
| `profile.create` | Создание Profile record (многоразовый шаблон VM). |

Будущие кандидаты (`provider.list`, `profile.list`, `provider.delete`, `profile.delete`) — вводятся отдельным PR по мере появления соответствующих CRUD-операций.

### Augur (6) — [ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul) / [augur.md](augur.md)

CRUD реестров брокера внешнего доступа Augur (Omen — внешняя система, Rite — grant). OpenAPI / MCP-поверхность стартует как **stub-каталог** ([augur.md](augur.md)); permissions нормированы здесь.

| Permission | Семантика |
|---|---|
| `omen.create` | Создание Omen record в Postgres (`omens`) — внешняя система (vault / prometheus / elk) с vault-ref на master-cred. |
| `omen.list` | Перечисление Omen-ов в реестре. |
| `omen.delete` | Удаление Omen record (каскадно убирает связанные Rite — `rites.omen ON DELETE CASCADE`). |
| `rite.create` | Создание Rite record в Postgres (`rites`) — grant субъекта (coven/sid) на Omen с allow-list и `delegate`. |
| `rite.list` | Перечисление Rite-ов в реестре. |
| `rite.delete` | Удаление Rite record. |

> **Live-fetch от Soul (`AugurRequest`) RBAC-permission не контролируется** — это не операторская операция через OpenAPI / MCP, а машинный запрос Soul-а по gRPC EventStream. Авторизация live-fetch — отдельный механизм Augur-а (Omen + Rite + allow-list по mTLS→SID→covens, [augur.md → Авторизация](augur.md#6-авторизация-keeper-side)), не RBAC-permission Архонта.

### Oracle (6) — [ADR-030](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)

CRUD реестров beacons-контура Oracle (Vigil — Soul-side проверка, Decree — правило reactor). OpenAPI (`POST/GET/DELETE /v1/vigils*` + `/v1/decrees*`) и MCP (`keeper.oracle.vigil.*` / `keeper.oracle.decree.*`) — поверхность реализована (S3). Все шесть проверяются `RequirePermission`-middleware (selector — NoSelector, как `omen.*`/`rite.*`); провал → 403 `forbidden`. Мутирующие (`*.create`/`*.delete`) пишут audit, read-only `*.list` (и get) — нет.

| Permission | Семантика | Audit-event |
|---|---|---|
| `vigil.create` | Создание Vigil record в Postgres (`vigils`) — Soul-side проверка (check — адрес core-beacon + interval + субъект coven XOR sid). | `vigil.created` |
| `vigil.list` | Перечисление Vigil-ов в реестре (и get по имени). | — (read-only) |
| `vigil.delete` | Удаление Vigil record (перестаёт раздаваться в `VigilSnapshot`; Decree-ы не каскадятся). | `vigil.deleted` |
| `decree.create` | Создание Decree record в Postgres (`decrees`) — правило reactor (on_beacon × субъект × incarnation_name → named scenario; опц. where-CEL + cooldown). | `decree.created` |
| `decree.list` | Перечисление Decree-ов в реестре (и get по имени). | — (read-only) |
| `decree.delete` | Удаление Decree record (каскадом чистит cooldown-state `oracle_fires`). | `decree.deleted` |

> **Reactor-флоу (`Portent` → match Decree → enqueue scenario) RBAC-permission не контролируется** — это машинный Soul-инициированный путь по gRPC EventStream, не операторская операция. Защита — субъектная привязка Decree (coven XOR sid) + membership-check + default-deny + whitelist scenario ([ADR-030(b)](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)), не RBAC-permission Архонта.

### Plugin Sigil (3)

[ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) / [plugins.md → Integrity-model](plugins.md#integrity-model). Управление allow-list-ом целостности плагинов **Sigil** — явный допуск Архонтом конкретного бинаря в реестр `plugin_sigils`. OpenAPI-поверхность **реализована** (S4a, `POST/GET/DELETE /v1/plugins/sigils*`); MCP — S4b. Все три проверяются `RequirePermission`-middleware (selector — NoSelector, как `operator.*`/`role.*`); провал → 403 `forbidden`. Мутирующие (`plugin.allow`/`plugin.revoke`) пишут audit (см. ниже), read-only `plugin.list` — нет.

| Permission | Семантика | Audit-event |
|---|---|---|
| `plugin.allow` | Допуск `(namespace, name, ref)` в allow-list `plugin_sigils` — Keeper читает бинарь активного слота кеша через `current`-symlink (R-nested `<ns>-<name>/<commit_sha>/`), считает `sha256`, подписывает и вставляет запись. `ref` — git-verified (Keeper резолвит `source`+`ref` в `commit_sha`-слот через go-git, [ADR-026(g)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)). Человеко-подтверждаемая операция supply-chain-контроля. | `plugin.allowed` |
| `plugin.revoke` | Отзыв ранее допущенной записи из `plugin_sigils` (бинарь перестаёт проходить Sigil-верификацию). | `plugin.revoked` |
| `plugin.list` | Перечисление активных записей allow-list-а `plugin_sigils` (без signature/manifest). | — (read-only) |

> **Верификация Sigil перед seal/exec RBAC-permission не контролируется** — это host-side проверка digest + подписи Keeper-а ([ADR-026(b)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)), а не операторская операция. Прочие plugin-management операции (`plugin.install` / `plugin.update` доставки/кеша) — отдельный PR при появлении соответствующего API.

### Sigil signing keys (4) — [ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) / R3

Ротация trust-anchor-**ключей подписи** Sigil (реестр `sigil_signing_keys`, отдельный от допусков `plugin_sigils`). OpenAPI **реализована** (`POST/GET /v1/sigil/keys`, `POST /v1/sigil/keys/{key_id}/primary`, `DELETE /v1/sigil/keys/{key_id}`) + MCP `keeper.sigil.key.*`. Все четыре — `RequirePermission`-middleware (selector NoSelector, как `plugin.*`); провал → 403. Мутирующие пишут audit, read-only `sigil.key-list` — нет. **Resource `sigil`, action — hyphenated** (`key-introduce`): грамматика permission — ровно `<resource>.<action>` (3-сегментный `sigil.key.introduce` — это MCP-tool, не permission); соответствие `keeper.sigil.key.<verb>` ↔ `sigil.key-<verb>`.

| Permission | Семантика | Audit-event |
|---|---|---|
| `sigil.key-introduce` | Ввод нового ключа подписи: Keeper генерирует ed25519-пару, пишет приватник в Vault KV (`secret/keeper/sigil-keys/<key_id>`), вставляет публичную часть в реестр. Приватник НИКОГДА не в ответе/логе. | `sigil.key-introduced` |
| `sigil.key-set-primary` | Сделать active-ключ primary (новые Sigil-ы подписываются им после cluster reload). | `sigil.key-primary-set` |
| `sigil.key-retire` | Вывод ключа из набора (Soul забывает при следующем `SigilTrustAnchors`). Запрещено для primary напрямую и для последнего active. | `sigil.key-retired` |
| `sigil.key-list` | Перечисление active-ключей подписи (primary первым, без `vault_ref`). | — (read-only) |

### Bootstrap

**Bootstrap-specific permissions отсутствуют.** `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)) действует под admin-bypass: проверяется только инвариант «реестр `operators` пуст» под PG advisory lock, никаких permission-проверок. Первый Архонт получает роль `cluster-admin` (`permissions: ["*"]`).

## Встроенные роли

В MVP закреплена ровно одна встроенная роль:

- **`cluster-admin`** — `permissions: ["*"]`, `builtin=true`. Вставляется **seed-миграцией** (E1, [ADR-028(b)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)) в `rbac_roles` + `rbac_role_permissions` ещё до любого `keeper init`. Привязывается к первому Архонту при `keeper init` — это **membership-строка** `(cluster-admin, <aid>)` в `rbac_role_operators` (фикс BUG-1, [ADR-028(c)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)). Флаг `builtin=true` защищает её от `role.delete` / `role.update`. Дополнительные операторы получают эту роль через `role.grant-operator` (оператором с этим правом).

Остальные роли создаются через OpenAPI/MCP (`role.create` + `role.grant-operator`); специальной встроенной семантики у них нет.

**Инвариант self-lockout ([ADR-028(f)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)).** Нельзя оставить кластер без хотя бы одного активного Архонта (`revoked_at IS NULL`) с эффективным `*`-permission. Проверяется на всех путях, которые могут это нарушить:

- `operator.revoke` — отзыв последнего Архонта с `*` ([ADR-013(c)](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)).
- `role.delete` — удаление роли (`cluster-admin` или иной), дающей единственный путь к `*`.
- `role.update` — снятие permission `*` у роли, через которую держится единственный эффективный `*`.
- `role.revoke-operator` — снятие последнего AID с эффективным `*`.

При попытке API возвращает `409` (`would-lock-out-cluster`, [naming-rules.md → Error codes](../naming-rules.md#error-codes)). Единственный путь восстановления после lockout-а — wipe Postgres + повторный `keeper init`, что неприемлемо для прода — отсюда инвариант.

## Применение

RBAC проверяется **до** выполнения операции, независимо от транспорта:

- **OpenAPI** — на входе HTTP-handler-а, до бизнес-логики.
- **MCP** — на входе MCP-tool, до выполнения.
- **`keeper.push`** — перед открытием SSH-сессии и перед стартом каждого шага Destiny на push-хосте.

Аудит-события пишутся в журнал прогонов ([storage.md](storage.md)) с указанием AID-оператора, ресурса, действия, результата проверки RBAC и (при отказе) причины.

### Двухслойная авторизация read-эндпоинтов (ADR-047 §г G1)

Read-эндпоинты с scoped-видимостью (souls-list/get/soulprint/history; ADR-047) авторизуются **в два слоя**, потому что один gate-слой не может одновременно «пропустить держателя права» и «сузить видимость по scope»:

1. **Gate (existence) — `RequireAction` поверх `HoldsAction`.** Спрашивает только: держит ли оператор `<resource>.<action>` **в принципе**, в любом scope, игнорируя селектор-контекст. Не держит → `403`.
2. **Scope-сужение — handler.** После фетча строк handler урезает выдачу до scope-границы оператора через per-resource резолвер: `keeper/internal/soulpurview` (coven-pushdown в SQL / regex-keyset / soulprint-page-CEL) для списка и `InScope` для одиночного объекта; `keeper/internal/statepredicate` (state-CEL) для инкарнаций. Вне scope: список — пусто, single-get/soulprint/history — `404`/`403`.

**Почему два слоя, а не scope-aware gate.** Scope-aware `Check(aid, resource, action, context)` для **scoped**-permission с пустым контекстом даёт ложный `deny`: селектор-ключ (`coven`/`host`/…) отсутствует в `nil`-контексте read-запроса, а enforcer на отсутствующий ключ возвращает deny (та же механика, что у incarnation-гейтов до ADR-008 amendment a). Это ломало доступ scoped-оператора к **собственному** списку — `RequirePermission(...Selector)` отрезал его ещё **до** handler-а, хотя по своему scope он видеть хосты обязан. Корень: read-эндпоинт **не несёт scope-контекст на gate-этапе** — scope (`coven` хоста, `state` инкарнации) резолвится из строк БД, которых на момент middleware-проверки ещё нет. `HoldsAction` снимает контекст из вопроса (existence, а не applicability), а реальное сужение откладывается в handler, где строки уже зафетчены.

**Мутации остаются на scope-aware `Check`.** issue-token / ssh-target-update / coven-assign по souls несут scope-контекст из path/body (`host=<sid>` из path, `coven=<label>` из body), поэтому продолжают гейтиться scope-aware `RequirePermission` / `RequirePermissionMulti` — для них контекст известен до handler-а и ложного deny нет. Граница: **read** (контекст из строк БД, ещё нет) → `RequireAction`; **мутация** (контекст из path/body) → `RequirePermission`.

### Revoked-семантика на read-путях (ADR-047 §г G1)

`ResolvePurview` стал **revoked-aware**: для отозванного (`operators.revoked_at IS NOT NULL`) оператора возвращает `Purview{Deny:true}` (терминальный флаг, [naming-rules.md → Purview](../naming-rules.md#purview--scope-резолвер-adr-047)) **до** сбора любых scope-измерений — иначе bare `*`-роль revoked-оператора вернула бы `Unrestricted`. Это **единая точка**, отрезающая revoked на всех read-souls-путях, потому что все они деривируются из `ResolvePurview`:

- **gate** — `HoldsAction`→`Deny`→`false`→`403`;
- **list** — `soulpurview.Resolve`→Empty-scope→пустой список;
- **single-get / soulprint / history** — `readScope`→Empty→`InScope`→`false`→`404`.

Per-host **history** (`GET /v1/souls/{sid}/history`) проходит **тот же** handler-`InScope`-gate, что get/soulprint — видимость хоста проверяется до раскрытия timeline, revoked отрезается там же. Это первое реальное использование поля `Purview.Deny` (до G1 — заготовка, всегда `false`).

На read revoked = «нет доступа» (`403`/`404`), а НЕ `401`-паритет scope-aware `Check`-а (который маппит revoked в `401 operator-revoked`): видимость флота не должна различать revoked и no-permission.

## Bootstrap первого Архонта

Закреплено [ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта). Кратко:

- **Имя сущности — Archon** (Архонт). См. [naming-rules.md → Сущности предметной области](../naming-rules.md#сущности-предметной-области).
- **Механизм выпуска первой credential** — administrative subcommand `keeper init`:

  ```bash
  keeper init --archon=archon-alice --config=/etc/keeper/keeper.yml
  ```

  - Команда требует пустого реестра `operators` в Postgres (защита через PG advisory lock + явный `--initialize` флаг для обычного старта Keeper-а до bootstrap-а).
  - Создаёт первого Архонта с указанным **AID** ([Archon ID](../naming-rules.md#идентификаторы)), привязывает к нему роль `cluster-admin` (`permissions: ["*"]`) — пишет membership-строку `(cluster-admin, <aid>)` в `rbac_role_operators` (роль `cluster-admin` уже есть из seed-миграции E1, [ADR-028(b/c)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres); это фикс BUG-1).
  - Выпускает JWT-credential (форма — [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) и кладёт в файл с `mode 0400`. Этот токен — единственный путь оператора в систему после bootstrap-а.
  - При повторном вызове на уже инициализированной БД отказывается.
- **Restart-семантика.** Если `operators` пуст и `--initialize` не указан — Keeper отказывается стартовать с подсказкой запустить `keeper init`. Это защита от случайного re-bootstrap-а после catastrophic wipe Postgres.
- **HA race-condition.** PG advisory lock на этапе `keeper init` исключает гонку N инстансов кластера.

После выпуска первого Архонта все остальные операторы создаются через обычный OpenAPI/MCP с RBAC-проверкой — Архонт с правом `operator.create` заводит новых, в `operators` они получают FK `created_by_aid` на родителя.

## Примеры ролей

Роли заводятся через OpenAPI/MCP (`role.create` для роли + permissions, `role.grant-operator` для membership — [§ Каталог permissions](#каталог-permissions)). Ниже — логическая модель (что лежит в `rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`); permission-строки следуют грамматике [§ Формат permissions](#формат-permissions).

**Простые роли — без селектора:**

```
role: soul-reader
  permissions: ["soul.list", "incarnation.list", "incarnation.get"]
  operators:   ["archon-monitor-01"]

role: cloud-admin
  permissions: ["provider.create", "profile.create"]
  operators:   ["archon-cloud-01"]
```

**Сложная роль — с селекторами:**

```
role: db-operator
  permissions:
    - "incarnation.* on service=redis-cluster,vault-cluster"
    - "incarnation.upgrade on service=redis-cluster,vault-cluster"
    - "push.apply on coven=db,cache"
    - "soul.list"
  operators: ["archon-db-01", "archon-db-02"]
```

`db-operator` может: делать любые операции над incarnation-ами сервисов `redis-cluster` и `vault-cluster` (включая `upgrade`); делать push-apply на хосты с coven `db` или `cache`; видеть список Souls. Прочее — запрещено (`default_policy: deny`).

**Точечная роль — по конкретному instance:**

```
role: redis-prod-only
  permissions:
    - "incarnation.run on incarnation=redis-prod"
    - "incarnation.get on incarnation=redis-prod"
    - "incarnation.history on incarnation=redis-prod"
  operators: ["archon-oncall-01"]
```

Дежурный по `redis-prod` может запускать сценарии и читать состояние только этого instance.

**Per-Coven / per-Service роли (ADR-008 amendment a):**

```
role: prod-operator
  permissions:
    - "incarnation.run on coven=prod"
    - "incarnation.destroy on coven=prod"
  operators: ["archon-prod-oncall"]

role: redis-fleet
  permissions:
    - "incarnation.* on service=redis"
  operators: ["archon-redis-team"]
```

- `prod-operator` может запускать и сносить incarnation-ы, у которых declared `covens` содержит `prod` (либо имя incarnation = `prod`) — env-scope по Coven-метке, независимо от сервиса. incarnation с `covens=[dev]` он не тронет.
- `redis-fleet` может выполнять любые incarnation-операции над incarnation-ами сервиса `redis` (`incarnation.service == "redis"`), независимо от их env-тегов.
- При create правило `coven=prod` не даст создать incarnation с тегом вне `prod`: scope резолвится из declared `covens` тела запроса ∪ `{name}` (см. [§ Грамматика селектора](#грамматика-селектора)).

## См. также

- [architecture.md → ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) — перенос RBAC-storage в Postgres, фикс BUG-1, пакет решений (таблицы / permissions / self-lockout / hard-cut / фазы).
- [operator-api.md](operator-api.md) — OpenAPI-сторона: HTTP endpoints, 1:1 mapping endpoint ↔ permission ↔ MCP-tool.
- [mcp-tools.md](mcp-tools.md) — MCP-сторона: каталог tools, формат declaration, async-convention, error mapping.
- [push.md](push.md) — push под единый RBAC.
- [cloud.md](cloud.md) — RBAC на cloud-операции.
- [storage.md](storage.md) — реестр `operators` и таблицы `rbac_roles` / `rbac_role_permissions` / `rbac_role_operators` в Postgres.
- [architecture.md → Сквозные требования](../architecture.md#сквозные-требования-и-где-они-приземляются).
- [architecture.md → ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) и [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon) — bootstrap первого Архонта, форма credential, реестр `operators`.
- [naming-rules.md → RBAC](../naming-rules.md#rbac-реестр-ролей-membership-и-термин-fk) — таблицы `rbac_*`, permissions `role.*`, разведение термина «FK».
- [requirements.md](../requirements.md) — RBAC как требование из коробки.
