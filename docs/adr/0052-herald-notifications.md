# ADR-052. Herald + Tiding — уведомления о событиях прогонов

> **Статус: канон ДО кода (S0, 2026-06-11).** Имена **Herald** / **Tiding** и дизайн (PG-реестры, tap-декоратор поверх audit-writer, at-least-once webhook-доставка, scope = только события прогонов) выбраны/утверждены пользователем (propose-and-wait пройден); дизайн — architect. Слайсы S1–S5 — реализация. Кода/миграций/OpenAPI по этому ADR пока нет.

**Контекст.** Прогон ([Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) `kind=scenario`/`command`, [Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)-спавн, drift-проверка [Scry](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) фиксируется в audit-журнале ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) и в метриках, но **наружу никуда не сигналит**: оператор узнаёт об упавшем nightly-converge или провалившемся command-Voyage только зайдя в UI / Grafana / `GET /v1/audit`. Полноценного push-канала «прогон завершился / упал — сообщи во внешнюю систему» нет. Единственный существующий webhook ([Toll](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls) amendment) узко-предметен (cluster-degraded set/cleared), конфигурируется только через `keeper.yml` (нет UI / RBAC / per-правило-подписки) и не покрывает события прогонов. Нужна **первоклассная сущность уведомлений**: канал доставки + правило подписки, управляемые через API/MCP/UI под RBAC, реагирующие на audit-события прогонов.

**Решение (зафиксировано пользователем 2026-06-11).** Вводятся две сущности:

- **Herald** — **канал доставки** уведомлений (куда слать). Имя — «вестник», продолжает «голосовую/вестническую» линию рядом с [Choir](0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации)/[Voice](0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации)/[Oracle](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor).
- **Tiding** — **правило подписки** (на что реагировать → каким Herald-ом). Имя — «весть», парно к Herald.

Метафора: Herald — гонец, Tiding — весть, которую он несёт. Модель — **событие прогона → матч Tiding → доставка через Herald**.

**(a) Сущности и реестры (Postgres, managed через OpenAPI/MCP, паттерн Omen/Decree).**

- **`heralds`** — реестр каналов. `name` PK (kebab-case, CHECK на формат — как `omens.name`), `type` closed-enum (**`webhook`** в MVP; `slack`/`email` — additive post-MVP), `config` JSONB (для `webhook`: `url`, опц. `headers`), `secret_ref` (vault-ref — секрет канала, напр. signing-token; **НЕ** хранится в PG cleartext, паттерн `omens.auth_ref` / `core.url`), `enabled` BOOL, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able.
- **`tidings`** — реестр правил подписки. `name` PK (kebab-case), `event_types` TEXT[] (audit event-types с поддержкой областей вида `scenario_run.*` — area-glob, не произвольный wildcard), фильтры `only_failures` BOOL / `only_changes` BOOL, опц. селекторы `incarnation` / `cadence` (привязка к источнику прогона), `herald` FK→`heralds(name)` (`ON DELETE CASCADE` — снос канала уносит его подписки), `enabled` BOOL, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able.

**(b) Scope MVP — ТОЛЬКО события прогонов.** `event_types` ограничен областями прогона: `scenario_run.*` / `command_run.*` / `voyage.*` (kind-agnostic) / `incarnation.drift_checked` / `incarnation.run_completed` (точечный, amend §k — терминал scenario-run одной инкарнации) / `cadence.*`. Это keeper-internal-события ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) категории `api`/`mcp`/`keeper_internal`/`background`), все проходящие через audit-writer. **Beacon-события хостов** ([Portent](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor) от Souls) в этот scope **НЕ входят** — см. «Отвергнутые альтернативы (а)».

**(c) Механика — tap поверх audit-writer.** Точка интеграции — **multi-writer декоратор над `audit.Writer`** (точка, предусмотренная [ADR-022(f)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)): после **успешной** записи события в PG (порядок «сначала аудит-факт, потом уведомление» — уведомление не врёт о незаписанном событии) tap отдаёт событие **notification-dispatcher**-у. Dispatcher матчит событие против включённых Tiding-правил (по `event_types`-area-glob + фильтрам `only_failures`/`only_changes` + селекторам `incarnation`/`cadence`) и на каждый матч ставит **задание на доставку**. Tap — best-effort относительно основного audit-write: сбой dispatcher-а НЕ откатывает audit-запись (audit — primary).

**(d) Доставка — at-least-once через claim-queue worker.** Доставка реализуется **claim-queue worker**-ом (parity [VoyageWorker](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) / [ADR-027(d)](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim): `FOR UPDATE SKIP LOCKED` + PG-lease + `attempt++`): задание клеймится, доставляется, при сбое — **retry с backoff**. Семантика — **at-least-once** (решение пользователя: редкий дубль уведомления допустим — как у command-[Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон); exactly-once не требуется). **Статусы попыток доставки — Redis** (hot-данные: инвариант hot→Redis, [ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis); **НЕ** пишутся синхронно в PG на каждую попытку). **Терминальные исходы** доставки — audit-события `herald.delivered` / `herald.failed` (постоянный след в журнале, не hot).

**(e) Security — обязательные инварианты.** Webhook = исходящий HTTP-вызов на оператор-заданный URL → **SSRF-вектор**. Контур, взведённый по умолчанию (паттерн `core.url` / [ADR-015](0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список), [ADR-016](0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) «безопасный default + аудируемый opt-out»):

- **https-only по умолчанию** — `http://` URL только явным per-Herald opt-out с warning.
- **deny приватных IP по умолчанию** — dial в loopback/RFC1918/link-local/metadata блокируется по фактически резолвнутому IP; `allow_private` — явный opt-out (как `core.url`).
- **запрет redirect** — reuse `netguard.NewCheckRedirect` (downgrade/redirect-блок); reuse `netguard.ValidateEndpoint` для валидации URL + `netguard.GuardedDialContext` (`Resolver`/`DefaultResolver`) для deny приватных IP по резолвнутому адресу — keeper-side общий guard `shared/netguard` (тот же, поверх которого Soul-side `core.url`/`core.http`; прецедент keeper-reuse — `keeper/internal/augur/egress.go`).
- **timeout** на доставку (не висеть на медленном/злом endpoint-е).
- **`secret_ref` — только vault-ref** (master-cred / signing-token не в PG cleartext; vault-ref маскируется в ошибках через `shared/audit.MaskSecrets`).
- **Payload уведомления НЕ несёт resolved-секреты** — `input`/vault-резолвленные значения в payload не кладутся (**инвариант A** [ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim), как `scenario_run.started`/`command_run.invoked` не кладут `input`); payload проходит `shared/audit.MaskSecrets`.

**(f) Контракты — все additive.**

- **OpenAPI** — `/v1/heralds` + `/v1/tidings` CRUD (greenfield-эндпоинты → пишутся **full-strict** + `strictDecodeProbe`, паттерн [ADR-051 amendment S6 d](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-типы-oapi-codegen-types-only--strict): typed `request.Body` + typed-response + ручная `unknown→400`-проба забуференного тела).
- **RBAC** — permission-семейства `herald.{create,read,list,update,delete}` + `tiding.{create,read,list,update,delete}` (catalog-driven, [ADR-042](0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-в-ui--ui-не-хардкодит-динамические-каталоги)).
- **Audit** — CRUD-семейства `herald.created`/`herald.updated`/`herald.deleted` + `tiding.created`/`tiding.updated`/`tiding.deleted` (область `api`/`mcp`) + терминалы доставки `herald.delivered`/`herald.failed` (область `keeper_internal` — worker-инициированные).
- **MCP** — `keeper.herald.*` / `keeper.tiding.*` (parity `keeper.augur.omen.*` / `keeper.oracle.decree.*`).
- **UI** — вкладка «Notifications» (parity страниц Vigils/Decrees/Cadences; companion-repo `soul-stack-web`).

**Обоснование.**

- **Tap поверх audit-writer, не отдельный event-bus.** Audit-журнал уже — единая нормированная точка всех keeper-side событий прогона ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); навешивание multi-writer декоратора (точка, предусмотренная ADR-022(f)) переиспользует существующий write-path, не плодит параллельный поток событий. «Сначала audit-факт в PG, потом уведомление» — уведомление не может опередить/соврать о незаписанном событии.
- **at-least-once + claim-queue parity Voyage.** Failover-resilient доставка переиспользует доказанный claim-pattern ([ADR-027(d)](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)/[VoyageWorker](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)); никакой новой инфраструктуры. Дубль уведомления — приемлемая цена за гарантию доставки (решение пользователя).
- **hot→Redis для попыток.** Статусы in-flight-попыток волатильны (инвариант presence/hot→Redis, [ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)); синхронный PG-write на каждую попытку доставки нарушил бы инвариант и нагрузил PG. Терминал — в audit (постоянный, аудируемый).
- **Безопасность на первом месте.** SSRF-guard / https-only / vault-ref / payload-masking — обязательные инварианты, не «детали имплементации»; webhook на оператор-заданный URL без guard-а — классический SSRF.

**Consequences.**

- **Миграция Postgres** — таблицы `heralds` / `tidings` (S1).
- **Domain CRUD** — `keeper/internal` слой Herald/Tiding (S1).
- **tap-декоратор + notification-dispatcher** — multi-writer поверх `audit.Writer` + матч Tiding-правил (S2; без доставки).
- **webhook-доставка** — claim-queue worker + SSRF-guard (reuse `shared/netguard`: `netguard.ValidateEndpoint` / `netguard.NewCheckRedirect` / `netguard.GuardedDialContext`+`Resolver`) + retry/backoff + Redis-статусы попыток + audit `herald.delivered`/`herald.failed` (S3).
- **OpenAPI `/v1/heralds` + `/v1/tidings`** (full-strict greenfield) + **RBAC** `herald.*`/`tiding.*` + **MCP** `keeper.herald.*`/`keeper.tiding.*` (S4) — [naming-rules.md](../naming-rules.md), [keeper/openapi.yaml](../keeper/openapi.yaml), [keeper/operator-api.md](../keeper/operator-api.md), [keeper/rbac.md](../keeper/rbac.md), [keeper/mcp-tools.md](../keeper/mcp-tools.md) (docs-writer).
- **UI-вкладка «Notifications»** (S5, companion-repo).
- **Имена** Herald / Tiding / `type`-enum / permissions / audit-events / REST-пути / MCP / вкладка — [naming-rules.md](../naming-rules.md).

**Отвергнутые альтернативы.**

- **(а) `notify` как [Decree](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)-action ([Oracle](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)).** Oracle слушает [Portent](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor) — beacon-события **хостов** от Souls (недоверенный вход), и его action = named-scenario (whitelist через work-queue). **События прогонов keeper-internal и через Oracle НЕ проходят** — Oracle их попросту не видит (он на Portent-флоу, не на audit-флоу). К тому же subject-модель Decree (`coven` XOR `sid` + обязательный `incarnation_name`) не ложится на «событие прогона» (у scenario_run-события субъект — Voyage/инкарнации, не один SID-отправитель), а action=notify не вписывается в whitelist «action = ТОЛЬКО named scenario». Разграничение зафиксировано: **Oracle = реакция на beacon-события хостов; Herald/Tiding = реакция на keeper-internal-события прогонов**. Уведомления на beacon-события — отдельный заход (см. «Отложено»).
- **(в) Config-only webhook в `keeper.yml`** (как [Toll](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)-webhook). Нет UI-вкладки, нет RBAC-управления, нет per-правило-подписки — против заказа («первоклассная управляемая сущность с UI/RBAC»). Herald/Tiding — PG-managed через API/MCP/UI, не inline-конфиг.

**Связь с ADR.**

- **[ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)** — tap = multi-writer декоратор над `audit.Writer` (точка ADR-022(f)); audit — primary, уведомление вторично.
- **[ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)** / **[ADR-046](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)** / **[ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)** — источники событий прогона (`scenario_run.*`/`command_run.*`/`voyage.*`/`cadence.*`/`incarnation.drift_checked`).
- **[ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)** — claim-queue паттерн доставки (at-least-once, lease, `attempt++`) + инвариант A (payload без resolved-секретов).
- **[ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)** — hot→Redis для статусов попыток.
- **[ADR-015](0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)** / **[ADR-016](0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack)** — SSRF-guard / https-only / opt-out-паттерн `core.url`; keeper-side webhook-доставка переиспользует общий `shared/netguard` (`netguard.ValidateEndpoint`/`netguard.NewCheckRedirect`/`netguard.GuardedDialContext`), тот же guard под Soul-side `core.url`/`core.http`.
- **[ADR-025](0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul)** — паттерн `secret_ref`/`auth_ref` vault-ref (секрет не в PG cleartext) + PG-managed реестр через API/MCP.
- **[ADR-030](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)** — Oracle/Decree (разграничение доменов: beacon-события хостов vs события прогонов).
- **[ADR-051](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-типы-oapi-codegen-types-only--strict)** — greenfield-эндпоинты `/v1/heralds`/`/v1/tidings` пишутся full-strict + `strictDecodeProbe` (amendment S6 d).
- **[ADR-042](0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-в-ui--ui-не-хардкодит-динамические-каталоги)** — permission-каталог `herald.*`/`tiding.*` фетчится UI, не хардкодится.

**Slice-карта (эпик Herald/Tiding).**

- **S0** — канон (этот ADR + имена в naming-rules). _(текущий слайс)_
- **S1** — миграция PG (`heralds`/`tidings`) + domain CRUD.
- **S2** — tap (multi-writer декоратор поверх audit-writer + notification-dispatcher: матч Tiding-правил), **без** доставки.
- **S3** — webhook-доставка: claim-queue worker + SSRF-guard + retry/backoff + Redis-статусы + audit `herald.delivered`/`herald.failed`.
- **S4** — OpenAPI `/v1/heralds`+`/v1/tidings` (full-strict) + RBAC `herald.*`/`tiding.*` + MCP `keeper.herald.*`/`keeper.tiding.*`.
- **S5** — UI-вкладка «Notifications» (companion-repo).

**Отложено post-MVP.**

- **Herald-типы `slack` / `email`** — additive новые значения `heralds.type`-enum + per-type config/доставка, без breaking change.
- **Dedup доставки** (exactly-once) — at-least-once достаточен (редкий дубль допустим); dedup — при реальной потребности.
- **Per-channel rate-limit** доставки (защита от шторма уведомлений на один Herald) — отдельным слайсом.
- **Уведомления на beacon-события хостов** ([Portent](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)/[Oracle](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)) — отдельный заход (Oracle/Portent-домен ≠ audit-домен прогонов, см. «Отвергнутые (а)»).

---

## Amendment (2026-06-11, «разовые уведомления в Run + гибкое тело»)

Расширение Herald/Tiding по запросу пользователя (решения приняты 2026-06-11, дизайн — architect). Всё **additive**: ни одно из существующих полей/контрактов не меняет семантику. Реализуется слайсами N1–N4 (карта — в конце блока).

**Контекст.** [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов) MVP — постоянное правило подписки на класс событий. Двух запросов он не покрывает: (1) «уведоми про **именно этот** прогон, который я запускаю сейчас» — разовая подписка на конкретный [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), не оставляющая мусора в реестре правил; (2) гибкое тело webhook-доставки — добавить статические поля оператора и/или сузить payload до нужных путей, не правя приёмник под полную форму.

### (g) Ephemeral Tiding — разовое правило, привязанное к прогону.

**Не новая сущность — флаг.** [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов) получает два поля:

- **`ephemeral`** BOOL DEFAULT `false` — признак разового правила.
- **`voyage_id`** — селектор привязки к конкретному прогону ([Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)). Для `ephemeral` правил — обязателен (правило матчит только события этого прогона); у обычных правил — `NULL`.

Ephemeral-Tiding — **НЕ реестровая сущность для оператора**: lifecycle привязан к породившему [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)-прогону (инициатор = прогон). Хранится строкой в `tidings` (`ephemeral=true` + `voyage_id`), но из вкладки Notifications **убрана** — оператор не управляет ею как самостоятельным правилом. Видимость разового правила и его доставок — контекстно на Voyage/Run detail (секция уведомлений прогона), не в общем списке [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов).

**Создание — атомарно keeper-ом из блока `notify` в [VoyageCreateRequest](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон).** В `VoyageCreateRequest` добавляется опциональный блок **`notify`** (additive): какой [Herald](#adr-052-herald--tiding--уведомления-о-событиях-прогонов)-канал + фильтры/тело (те же поля, что у постоянного Tiding: `only_failures`/`only_changes`/`annotations`/`projection`). При наличии блока keeper в **той же транзакции**, что создаёт сам [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), создаёт ephemeral-Tiding с `voyage_id` нового прогона. Атомарность by construction исключает гонку «прогон завершился раньше, чем создано правило в БД»: либо обе записи в одной tx, либо ни одной. **Но атомарность tx гарантирует только НАЛИЧИЕ правила в БД, а НЕ его видимость TTL-кэшу dispatcher-а** — для этого нужна явная инвалидация (инвариант ниже).

**Инвалидация кэша dispatcher-а — двухуровневая, обязательна (инвариант).** **ЛЮБОЙ путь, создающий Tiding-правило** — включая прямую вставку ephemeral-Tiding в voyage-транзакцию из блока `notify`, **минуя `herald.Service`** — **ОБЯЗАН после commit инвалидировать снимок enabled-правил dispatcher-а двухуровнево**: (1) **in-process** `InvalidateRules()` на локальном инстансе; (2) **cross-keeper** publish на канал **`herald:invalidate`** ([ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis) pub/sub). Без этого быстрый прогон (короче TTL снимка правил dispatcher-а, default **15s**) диспетчит терминальное событие против устаревшего кэша — и разовое правило молча промахивается. **Cross-keeper publish обязателен**, а не достаточно in-process: ephemeral-Tiding создаётся на одном keeper, а финализировать прогон и диспетчить его терминальное событие может **другой** инстанс stateless-кластера ([Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)orch `ClaimNext`) — тот инстанс обязан получить инвалидацию через Redis. Это тот же механизм `herald:invalidate` + TTL-poll, что и для обычных Tiding ([отвергнутый вариант ниже](#adr-052-herald--tiding--уведомления-о-событиях-прогонов)); прямая вставка в voyage-tx из него не освобождена.

**RBAC.** Архонт, создающий прогон с блоком `notify`, обязан держать **`herald.read`** на указанный канал (guard в create-обработчике [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) — нельзя подписать уведомление на канал, к которому нет доступа). Семантика `herald.read` (а не `tiding.create`) выбрана сознательно: ephemeral-Tiding не управляется оператором как самостоятельная сущность реестра — это деривация прогона; гейт — доступ к каналу-получателю.

**Listing.** `GET /v1/tidings` по умолчанию **НЕ отдаёт** `ephemeral`-правила ([ADR-042](0042-backend-driven-ui.md#adr-042-backend-driven-dynamic-data-в-ui--ui-не-хардкодит-динамические-каталоги) тупой фронт — реестр оператора их не показывает); опциональный query-параметр `include_ephemeral=true` для отладки.

**Очистка.** Ephemeral-Tiding сносится Reaper-правилом `purge_orphan_ephemeral_tidings` по предикату «прогон в терминале» с grace-периодом (grace **обязателен** по корректности доставки: dispatcher асинхронный, синхронный снос на финале опередил бы терминальное уведомление). Voyages в PG сейчас не удаляются и не архивируются — каскадная очистка через FK `ON DELETE CASCADE` станет возможна только с вводом retention/архивации прогонов (отдельная фича, [ADR-046](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage) `purge_voyages` — нереализован); до этого Reaper-purge — единственный механизм. Прежняя формулировка про «снос подписки на терминале [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)» была неточна — в коде такого шага нет.

### (h) Гибкое тело webhook-доставки — `annotations` + `projection`.

[Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов) (включая ephemeral, и блок `notify`) получает два поля управления телом:

- **`annotations`** JSONB — статические поля оператора (произвольные ключ→значение). Мержатся в тело webhook-доставки **новым верхнеуровневым ключом `annotations`** (см. (i)). Назначение — пометить уведомление контекстом приёмника (`team: ops`, `severity: high`, ссылка на runbook и т.п.).
- **`projection`** TEXT[] — allow-list путей из payload события. Непусто → в тело кладётся только подмножество payload по этим путям; **пусто = текущая полная форма** payload (backward-compat по умолчанию).

**Оба вычисляются в worker-е доставки (claim-queue, off-path).** Инвариант горячего пути сохраняется: **dispatcher остаётся «match + enqueue копии»** ([ADR-052(c)](#adr-052-herald--tiding--уведомления-о-событиях-прогонов)) — он не трогает ни `annotations`, ни `projection`. Merge `annotations` и применение `projection` к payload-копии происходят в worker-е при сборке `webhookPayload`, вне tap-пути audit-writer.

**Шаблонные движки — решение зафиксировано.**

- **Go text/template — ОТВЕРГНУТ.** Чужой слой ([ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) держит text/template строго на рендере файлов `templates/<path>.tmpl`, не на runtime-теле уведомлений), плюс DoS-поверхность (оператор-заданный шаблон, исполняемый worker-ом на каждой доставке).
- **CEL-интерполяция в теле — ОТЛОЖЕНА.** Потребует отдельного CEL-sandbox (parity migration-CEL-sandbox [ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)) + аудита секрет-гигиены (CEL-выражение могло бы вытащить из контекста больше, чем замаскированный payload). Вводится отдельным слайсом при реальной потребности; `annotations`+`projection` MVP покрывают «добавить статику» и «сузить» без вычислений.

**Секрет-гигиена не меняется.** `annotations` — статика оператора (не вычисляется из контекста события, секреты в неё не попадают иначе как руками оператора). `projection` — подмножество **уже-замаскированного** payload (после `shared/audit.MaskSecrets`, [ADR-052(e)](#adr-052-herald--tiding--уведомления-о-событиях-прогонов)) — сужение не может раскрыть больше, чем уже было в payload-копии. Инвариант A ([ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)) в силе.

### (i) Webhook-контракт — `annotations`-ключ (additive).

Тело webhook-POST-а ([ADR-052(d)](#adr-052-herald--tiding--уведомления-о-событиях-прогонов), `webhookPayload`: `event_type`/`occurred_at`/`herald`/`tiding`/`payload`) дополняется **опциональным** верхнеуровневым ключом **`annotations`** (объект из `Tiding.annotations`; отсутствует при пустых annotations). Additive для внешних приёмников: существующие интеграции, читающие `event_type`/`payload`/…, не ломаются.

### Отвергнуто / отложено (Amendment).

- **Per-task алертинг («изменения в таске X»)** — был отложен на момент N-эпика; **активирован отдельным дизайн-заходом** (см. Amendment §j ниже). Путь решения подтверждён: per-task changed-разбивка **в терминальном событии прогона**, не подписка на пер-таск-поток; scope [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов) на `task.executed` **НЕ расширяется** (кардинальность host-level `task.executed` остаётся вне Tiding). Адресация таски — стабильное адресное пространство `register ∪ id` (§j).
- **`notify`-инструкции в `voyages` JSONB без сущности** (хранить webhook-инструкцию прямо в строке прогона, без ephemeral-Tiding) — ОТВЕРГНУТ: это завело бы PG-lookup в `voyages` на горячий tap-путь dispatcher-а (на каждое audit-событие — чтение инструкций из строки прогона). Ephemeral-Tiding ложится в существующий снимок enabled-правил dispatcher-а (`herald:invalidate` + TTL-poll), горячий путь не трогает.

### Slice-карта (Amendment N).

- **N0** — канон (этот Amendment + дополнения в naming-rules). _(текущий слайс)_
- **N1** — PG + domain: поля `ephemeral`/`voyage_id`/`annotations`/`projection` в `tidings` (миграция + domain-слой).
- **N2** — блок `notify` в [VoyageCreateRequest](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) + атомарное создание ephemeral-Tiding (одна tx с [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)) + `herald.read`-guard + cleanup (Reaper-правило `purge_orphan_ephemeral_tidings`, grace-период).
- **N3** — worker: merge `annotations` + применение `projection` к payload-копии (off-path, при сборке `webhookPayload`).
- **N4** — UI: блок «Notify» в RunWizard (radio-паттерн как [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)↔[Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)) + поля `annotations`/`projection` в Notifications-CRUD (companion-repo); видимость разовых — на Voyage/Run detail (не в Notifications-вкладке); listing default-hide бэкендный (`include_ephemeral` debug).

---

## Amendment (2026-06-12, «алерт на изменение конкретной таски — адресация»)

### (j) Подписка «таска X изменила» — адресное пространство `register ∪ id`.

Per-task changed-алертинг (отложенный в N-эпике, см. «Отвергнуто / отложено» выше) **активирован** решением пользователя. Этот под-раздел фиксирует **только адресацию** таски в подписке; механика разбивки и dispatcher — следующие слайсы.

**Ключ адресации таски = объединение `register ∪ id`.** Подписка «алерт, когда **эта конкретная** таска изменила state» адресует таску по **стабильному строковому адресу**:

- если у таски есть `register:` — адрес = её `register`-имя (таска уже адресуема, [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) §8);
- если `register:` нет — адрес задаётся опциональным полем `id:` ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) amendment 2026-06-12).

`register` и `id` — **одно адресное пространство** (один формат `^[a-z][a-z0-9_]*$`, взаимоисключимы на одной таске). Адрес **стабилен**: он задан в YAML scenario/destiny автором, не зависит от `task_idx` (которое плывёт при правке списка задач) — поэтому пригоден как долгоживущий селектор подписки.

**Что НЕ в этом amendment.** Контракт терминального события прогона (как именно per-task changed-разбивка попадает в payload), резолв адреса в `RenderedTask`, формат подписки в [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов)/`notify` и dispatcher-привязка — **следующие слайсы**, не зафиксированы здесь. Инвариант «scope Tiding на host-level `task.executed` НЕ расширяется» ([ADR-052 «Отвергнуто / отложено»](#отвергнуто--отложено-amendment)) **в силе**: адресация per-task идёт через терминал прогона, не через подписку на пер-таск-поток.

Грамматика и валидация поля `id:` — [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) (amendment 2026-06-12), [`docs/destiny/tasks.md §3`](../destiny/tasks.md#3-полный-список-блоков-задачи).

### (k) Терминальное событие прогона `incarnation.run_completed` несёт `changed_tasks`.

Раздел §j зафиксировал **адресацию** таски (`register ∪ id`); §k фиксирует **контракт терминального события**, через которое per-task changed-разбивка попадает к подписчику. Решение: per-incarnation итог scenario-run эмитится **новым audit-событием `incarnation.run_completed`** (а не расширением scope подписки на host-level `task.executed` — инвариант §«Отвергнуто / отложено» в силе).

**Имя и форма события.** `incarnation.run_completed` — **per-incarnation** итог одного scenario-run: одно событие на инкарнацию-прогон, **не per-host** (его пишет [`scenario.Runner`](../keeper/modules.md) после барьера на успешном финале обычного прогона, рядом с commit state). Развести с **`run.completed`** обязательно: то — per-host `RunResult` от Soul-а (`source: soul_grpc`); `incarnation.run_completed` — keeper-internal свёртка (`source: keeper_internal`, `archon_aid` колонка NULL, `correlation_id = apply_id`). Уровень сбора — **per-incarnation scenario-run, НЕ суммарно по [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)** (voyage-orchestrator не трогается; voyage-итог несут `scenario_run.*`/`command_run.*`).

**Payload:** `{incarnation, scenario, apply_id, status, changed_tasks, cadence_id?, voyage_id?}`, где `status` ∈ {`success`, `failed`} и `changed_tasks` — массив записей `{idx, name, register, id, module, changed_hosts, total_hosts}` по задачам, изменившимся хотя бы на одном хосте (таска без изменений в массив **не** попадает). `cadence_id` и `voyage_id` — опциональные ключи (см. amend §k ниже).

**Источник `changed` — журнал аудита, не новая таблица.** Changed-факт уже фиксируется на каждую `(apply_id, sid, task_idx)` событием `task.executed` с `payload.status`. `changed = TASK_STATUS_CHANGED`. Runner на финале читает агрегат journal-а (`correlation_id = apply_id`, `event_type = task.executed`, `payload->>'status' = 'TASK_STATUS_CHANGED'`) и берёт **только адресные поля** `(sid, task_idx)` — payload-значения (`register_data`/params/error) **не читаются**. Метаданные задачи (`name`/`register`/`id`/`module`) добираются из in-memory `[]RenderedTask` прогона. **Секрет-гигиена:** `changed_tasks` несёт только метаданные + счётчики, ни одного payload-значения.

**Loop-свёртка по адресу.** Одна исходная задача с `loop:` разворачивается в N `RenderedTask` со сквозными `task_idx`, но адрес (`register ∪ id`, §j) у всех итераций **один**. Свёртка — по адресу: одна запись `changed_tasks` на адрес, `idx` — репрезентативный (первая итерация). Счётчики — **union уникальных `sid`**, не сумма по `idx`: `total_hosts = |union TargetSIDs по всем idx адреса|` (число таргет-хостов после `on:`/`where:`, **не весь roster и не сумма по итерациям** — иначе loop раздул бы знаменатель M×K вместо M); `changed_hosts = |union sid со статусом CHANGED по этим idx|`.

**Неадресуемые задачи** (нет `register` и нет `id`) **включаются** в `changed_tasks` с пустым адресом (полнота «сколько и где изменилось»); каждая такая задача группируется по своему `idx` (с чужими неадресуемыми не схлопывается). Подписаться на них нельзя (адрес — поле для подписки, не для существования записи), но «что изменилось» остаётся полным.

**Что НЕ в этом amendment.** Формат подписки в [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов)/`notify` на конкретный адрес таски и dispatcher-привязка `changed_tasks` к каналу — **следующий слайс** (T4). Имя события — [naming-rules.md → Audit-events](../naming-rules.md).

#### Amend §k (T4-0, 2026-06-12): `status: failed`, `cadence_id`, точечный scope.

Фундамент T4a/T4b расширяет контракт §k тремя пунктами (поведение success-ветки не меняется):

- **Событие эмитится и на ТЕРМИНАЛЬНОМ ПРОВАЛЕ обычного прогона** — то же имя `incarnation.run_completed` со `status: failed` (НЕ отдельное имя; `error_locked` сворачивается в `failed`, под-статусы не плодим — паттерн `task.executed`/`run.completed`, фильтрация по `payload.status`). Точка эмиссии — после `lockIncarnation` (перевод в `error_locked`), симметрично success-ветке. `changed_tasks` на провале: **частичный** при позднем abort (`dispatch_failed`/`register_load_failed`/… — что успело CHANGED до падения) либо **пустой** при раннем abort (`no_hosts`/`render_failed`/… — до render, `tasks` ещё нет). Форма payload идентична успеху.
  - **Гейт `TerminalDestroy`:** провал teardown-прогона (scenario `destroy`) `incarnation.run_completed` **НЕ** эмитит — у destroy свой терминал (`incarnation.destroy_completed` / `.destroy_failed`).
  - **Single-winner:** провальное событие эмитит **только тот инстанс, чей `lockIncarnation` реально записал терминал**. При recovery-перехвате (строку уже вывел из `applying` другой коммиттер → `ErrAlreadyFinalized`) проигравший событие **не** пишет — защита от дубля события на прогон, симметрично success-ветке (где проигравший commit возвращается до эмиссии).
- **Опциональный `cadence_id`** в payload — присутствует **только** когда прогон спавнен [Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)-расписанием (дочерний [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), `voyages.cadence_id`). Ручной прогон ключ **не** несёт (консервативно, как drift-payload). Это позволяет постоянному Tiding-правилу с cadence-селектором ловить **результаты** прогонов расписания, а не только spawned/skipped-события (T4b).
- **Scope подписки [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов).** `incarnation.run_completed` добавлен в scope §(b) **точечным** типом (рядом с `incarnation.drift_checked`). Область `incarnation.*` целиком в scope **НЕ** открывается (несёт CRUD/lifecycle-шум) — допущен только этот один точечный тип.

#### Amend §k (visibility, 2026-06-12): опциональный `voyage_id` + audit-фильтр `payload_voyage`.

`incarnation.run_completed` — **per-incarnation** событие с `correlation_id = apply_id` (один прогон одной инкарнации). [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) detail на UI показывает «что и где изменилось» по всему вояжу, но читает audit по `voyage_id`, а не по `apply_id` каждой инкарнации — без back-link-а на вояж run-события из него не собрать. Решение (путь B):

- **Опциональный `voyage_id`** в payload — присутствует **только** когда прогон спавнен [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)-orchestrator-ом (production `ScenarioSpawner` пробрасывает `voyages.voyage_id` в `RunSpec.VoyageID`). Прямые пути scenario-run (`create`/`rerun-create`/`destroy` и их MCP-аналоги) вызывают `scenario.Runner` напрямую, минуя voyage-orchestrator, → ключ **не** несут (симметрия с `cadence_id`). `voyage_id` и `cadence_id` ортогональны: дочерний [Voyage](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) расписания несёт оба.
- **Audit-фильтр `payload_voyage`.** `GET /v1/audit` принимает опц. query-param `payload_voyage` → exact-match по `payload->>'voyage_id'` (параметризованный плейсхолдер, по образцу `payload_herald`). Voyage detail фетчит per-incarnation run-события вояжа этим фильтром.
- **Что НЕ входит:** UI Voyage detail (рендер «что и где изменилось») — следующий слайс.

### (l) Backend-матч T4: task-селектор [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов) + cadence-селектор на `incarnation.run_completed` (T4-match, 2026-06-12).

§j зафиксировал адресацию таски, §k — контракт терминального события. §l фиксирует **dispatcher-матч**: подписку правила на конкретную задачу и расширение cadence-селектора на результаты прогонов расписания (backend обоих веток T4a/T4b — UI отдельным заходом).

**Task-селектор [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов).** Поле `tidings.task` (TEXT, nullable; миграция 073) — опц. селектор подписки на КОНКРЕТНУЮ задачу прогона по её адресу (`register ∪ id`, §j). `NULL` = без фильтра (поведение S1). Непустое значение → правило матчит **только** `incarnation.run_completed`, в `changed_tasks` которого есть запись с `register == task` ИЛИ `id == task` (`herald.matchTask`, симметрично `matchIncarnation`/`matchCadence`). **Семантика «присутствие = изменилась»:** запись в `changed_tasks` существует только для задачи, изменившейся хотя бы на одном хосте (§k), поэтому task-селектор **самодостаточен** — отдельной проверки «изменилась» не требует. Пустой адрес неадресуемой задачи (`register==""` && `id==""`) непустым селектором **не** матчится (domain-нормализация `""→nil` + defence-in-depth в `matchTask`). Любой иной `event_type` (нет `changed_tasks`) → не матч.

**`only_changes` × `incarnation.run_completed`.** Для консистентности комбинации task-селектора с `only_changes`: `hasChanges(incarnation.run_completed)` = `len(changed_tasks) > 0`. Task-селектор сам по себе самодостаточен, но если оператор скомбинирует его с `only_changes`, тот не должен молча отсеять матчевое событие; на провале без изменений (пустой `changed_tasks`) — `false`.

**Cadence-селектор ловит результаты прогонов расписания.** `eventCadence` расширен случаем `incarnation.run_completed` → `payload.cadence_id` (рядом с `cadence.spawned`/`cadence.skipped_overlap`). Постоянное [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов) с cadence-селектором теперь ловит и **результаты** прогонов, заспавненных [Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)-расписанием (через опц. `cadence_id` payload, amend §k), а не только сами `cadence.*`-события. Ручной прогон `cadence_id` не несёт → не матч (консервативно).

**Поверхность.** Поле `task` проброшено в domain (`Tiding.Task`), CRUD (insert/update/select), OpenAPI-схемы `Tiding`/`TidingCreateRequest`/`TidingUpdateRequest` (nullable string), REST-handler (omit==clear на PUT-replace, как `annotations`/`projection`), MCP-tools (`keeper.tiding.create`/`update`/`read`). **Не входит:** UI (вкладка Notifications) — следующий заход.

#### Amend §l (T4-cadence-fix, 2026-06-12): cadence-селектор ловит результаты прогонов расписания на Voyage-ТЕРМИНАЛЕ.

§l (T4b) расширил `eventCadence` случаем `incarnation.run_completed`. Этого **недостаточно** для типового cadence-Tiding-правила `event_types=[scenario_run.*]` (или `command_run.*`): такое правило подписано на **Voyage-терминал** (агрегированный итог спавна расписания), а cadence_id нёс только per-incarnation `incarnation.run_completed`. Получалось дизъюнктно: `scenario_run.*` событие cadence_id не несло (`matchCadence → false`), а `incarnation.run_completed` нёс cadence_id, но не попадал под `scenario_run.*`-паттерн (`matchEventType → false`). Итог — cadence-notify правило **не матчило ни одно событие** (QA-blocker).

**Фикс (Вариант б, architect).** Voyage-терминал `scenario_run.*`/`command_run.*` несёт `cadence_id`, когда Voyage спавнен расписанием:

- `voyageorch.emitFinalized` кладёт `cadence_id` в payload терминала прогона при `run.CadenceID != nil` (claim селектит `voyages.cadence_id`). nil-guarded симметрично `scenario.Runner.emitRunCompleted`: ручной Voyage ключ не несёт.
- `eventCadence` расширен **параллельным** case на Voyage-терминалы (`scenario_run.completed`/`.failed`/`.partial_failed` + `command_run.*`) → `payload.cadence_id`. Case `incarnation.run_completed` **сохранён** (нужен task-селектору §l, который матчит только его).

**Что это даёт.** cadence-Tiding `event_types=[scenario_run.*], cadence=<ULID>` ловит **ОДНО** агрегированное уведомление на спавн расписания (Voyage-терминал), а не рассыпается на per-incarnation `incarnation.run_completed`. Additive (audit-payload, не proto/wire-контракт). Task-селектор (§l) **не затронут**: `matchTask` по-прежнему матчит только `incarnation.run_completed` (Voyage-терминал `changed_tasks` не несёт).

#### Amend §k/§l (T4-fix, 2026-06-12): `changed_tasks` и task-подписка покрывают keeper-side задачи.

§k/§l предполагают, что `task.executed` фиксируется на **каждую** изменившуюся задачу прогона. Изначально это было верно только для **Soul-side** задач: handler [`handleTaskEvent`](../keeper/modules.md) эмитил `task.executed` по `TaskEvent`-payload-у от Soul-а. **Keeper-side задачи** (`on: keeper` — `core.cloud.provisioned`/`core.vault.kv-read`/`core.soul.registered`, [docs/keeper/modules.md](../keeper/modules.md)) исполняются in-process (`scenario.dispatchKeeperTasks`), `TaskEvent` по сети не идёт — и `task.executed` для них **не эмитился**. Следствие: changed-keeper-задача молча выпадала из `changed_tasks` свёртки (§k), а task-подписка (§l) с её адресом была **мёртвой** (счётчики `changed/total` её не считали).

**Фикс (Вариант A): `task.executed` эмитируется ОБЕИМИ сторонами.** `scenario.dispatchKeeperTasks` пишет `task.executed` для **каждой** keeper-задачи симметрично Soul-side handler-у: `sid = keeper` (адрес keeper-target-а прогона, [naming-rules.md](../naming-rules.md)), `correlation_id = apply_id`, `source: keeper_internal`, `payload.status` = имя `keeperv1.TaskStatus` (`changed → TASK_STATUS_CHANGED`, `failed → TASK_STATUS_FAILED`, иначе `TASK_STATUS_OK`) — та же строка, по которой свёртка фильтрует CHANGED. Форма payload общая с Soul-side (единый сборщик `audit.BuildTaskExecutedPayload`), чтобы не разъехалась между двумя точками эмиссии. **Секрет-гигиена:** keeper-side `task.executed` несёт только адресные поля + status (без `register_data`/output/params); `error.message` — только на провале и только для не-`no_log` (для `no_log` подавляется маркером `suppressed: "no_log"`, как Soul-side). Keeper-side прогресс на operator-SSE не транслируется (только запись в audit) — `applybus.Publish` для keeper-задач не вызывается.

**Что это даёт.** changed-keeper-задача с адресом `register ∪ id` (типичный `provision_vm` с `id:` без `register`) теперь попадает в `changed_tasks` терминального события (`changed_hosts`/`total_hosts` считаются по `sid = keeper`), и task-подписка [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов) на её адрес — рабочая. Контракт `changed_tasks` (§k) и task-селектор (§l) **не меняются** — расширяется только множество источников `task.executed` (обе стороны вместо одной Soul-side).

**Отвергнуто (Вариант B):** источник changed = `apply_task_register` вместо `task.executed`. Отвергнут architect-ом: не покрывает задачи **без** `register` (а ключевой кейс бага — `provision_vm` с `id:` без `register:`), и расходится с уже зафиксированным §k-источником.

### Slice-карта (эпик «алерт на таску X»).

- **T1** — поле задачи `id:` (struct-поле `Task.ID`, yaml `id`), грамматика/валидация ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) amendment, [tasks.md §3](../destiny/tasks.md#3-полный-список-блоков-задачи)).
- **T2** — uniqueness адресного пространства `register ∪ id` (config-валидатор: оба на одной таске запрещены, дубль адреса в scenario запрещён).
- **T3** — резолв `id:` в `RenderedTask` + per-task свёртка changed (из агрегата audit_log, loop-свёртка по адресу) + терминальное событие `incarnation.run_completed` (§k).
- **T4-0** — фундамент T4a/T4b (amend §k): `status: failed` на терминальном провале + опц. `cadence_id` + точечный scope `incarnation.run_completed` в подписке Tiding.
- **T4-match (backend)** — dispatcher-матч обеих веток (§l): task-селектор `tidings.task` (`matchTask`, миграция 073) + `hasChanges`-case для `incarnation.run_completed` + cadence-селектор на результаты прогонов расписания (`eventCadence` → `cadence_id`). Поверхность: domain/CRUD/OpenAPI/REST/MCP.
- **T4-fix (backend)** — keeper-side задачи (`on: keeper`) попадают в `changed_tasks`/task-подписку: `task.executed` эмитируется и из `scenario.dispatchKeeperTasks` (amend §k/§l, Вариант A). Общий сборщик payload `audit.BuildTaskExecutedPayload` для обеих точек эмиссии.
- **T4-cadence-fix (backend)** — cadence-notify правило `event_types=[scenario_run.*]` ловит результаты прогонов расписания на Voyage-терминале (amend §l, Вариант б): `emitFinalized` несёт `cadence_id` при `run.CadenceID != nil` + `eventCadence` параллельный case на `scenario_run.*`/`command_run.*`. Guard-тест доставки (полный dispatcher-путь). _(текущий слайс)_
- **T4a (UI)** — вкладка Notifications: подписка на адрес таски (поле `task`) в форме Tiding/`notify`.
- **T4b (UI)** — отображение cadence-привязки результатов прогонов расписания (backend закрыт в T4-match).

## Amendment (2026-06-12, «уведомления в форме регулярного расписания Cadence»)

### (m) Постоянный Tiding из блока `notify` формы [Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage).

[`CadenceCreateRequest`](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage) получает опциональный блок **`notify`** (additive, та же [`VoyageNotify`](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)-форма: `herald`/`on`/`only_failures`/`only_changes`/`annotations`/`projection`). В отличие от `voyage.notify` ([§g](#g-ephemeral-tiding--разовое-правило-привязанное-к-прогону), `ephemeral=true` на ОДИН прогон), `cadence.notify` создаёт **ПОСТОЯННЫЙ** [Tiding](#adr-052-herald--tiding--уведомления-о-событиях-прогонов) (`ephemeral=false`), переживающий отдельные прогоны и реагирующий на каждый спавн расписания.

**Привязка — по ULID, а не имени (rename-safe).** Постоянное правило привязывается к [Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage) по её **`cadences.id`** (ULID-PK, стабильный), не по `name` (мутабельно через PATCH). Привязка проставляется в ДВЕ колонки `tidings`:
- **`cadence`** (существующий селектор подписки) ← `cadences.id`: правило фильтрует «слать только про прогоны ЭТОГО расписания»;
- **`created_from_cadence_id`** (новая колонка, миграция 074) ← `cadences.id`: **маркер ПРОИСХОЖДЕНИЯ** «правило создано из формы расписания». FK на `cadences(id)` **ON DELETE CASCADE**.

**Зачем отдельный origin-маркер, а не переиспользование селектора `cadence`.** Селектор `cadence` может стоять и на **вручную** заведённом операторе Tiding-е (он сам подписался на прогоны расписания). Каскад-удаление при сносе [Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage) ([ADR-046 §9](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)) обязан сносить **только** правила, рождённые формой, и НЕ трогать руками заведённые с тем же селектором. Поэтому происхождение — ортогональная селектору колонка с FK CASCADE.

**Создание — атомарно одной tx (parity §g).** При наличии блока `notify` keeper в **той же транзакции**, что создаёт строку `cadences`, вставляет постоянные Tiding-и прямым `herald.InsertTiding` (Cadence сначала — FK `tidings.created_from_cadence_id → cadences(id)`). Любой сбой (FK / коллизия PK-имени Tiding / валидация) откатывает весь `POST /v1/cadences` — нет ни [Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage), ни правил.

**Имя автоправила — детерминированно-уникальное.** `<cadence-name>-notify` для единственного `notify`-элемента; при нескольких — суффикс `-2`/`-3`/… (`Tiding.Name` — PK, коллизия недопустима). Человекочитаемое имя расписания приводится к `NamePattern` (`^[a-z0-9-]{1,63}$`) и усекается с запасом под суффиксы ([naming-rules.md](../naming-rules.md)).

**Инвалидация кэша dispatcher-а — двухуровневая, обязательна (тот же инвариант [§g](#g-ephemeral-tiding--разовое-правило-привязанное-к-прогону)).** Постоянные правила вставлены прямым `herald.InsertTiding` **минуя `herald.Service`-CRUD** (и его инвалидацию), поэтому после `commit` при наличии `notify` keeper **обязан** двухуровнево инвалидировать снимок enabled-правил dispatcher-а (in-process `InvalidateRules()` + cross-keeper publish `herald:invalidate`). Без этого быстрый/cross-keeper спавн (спавн [Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage) идёт на любом keeper-е stateless-кластера) диспетчит терминал против устаревшего TTL-снимка (15s) — правило молча промахивается.

**RBAC (parity §g).** Архонт, создающий [Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage) с блоком `notify`, обязан держать **`herald.read`** на КАЖДЫЙ указанный канал (нельзя подписать уведомление на канал без доступа) — guard в create-обработчике до открытия tx. Несуществующий канал → 422 (а не FK-500 при insert).

**Lifecycle — каскад (ADR-046 §9).** `DELETE /v1/cadences/{id}` сносит связанные постоянные правила через FK `created_from_cadence_id ON DELETE CASCADE`. Правила с тем же `cadence`-селектором, но `created_from_cadence_id = NULL` (вручную созданные), переживают снос расписания.

**Что НЕ в этом amendment.** UI-блок «Notify» в форме Cadence (companion-repo) и MCP-tool создания Cadence с `notify` — отдельные слайсы. PATCH-редактирование/удаление автоправил через форму расписания — отложено (правила управляемы через обычный Tiding-CRUD; форма только создаёт).

### Slice-карта (эпик «уведомления в форме Cadence»).

- **C1 (backend)** — миграция 074 (`tidings.created_from_cadence_id` + FK CASCADE) + domain/CRUD (поле `CreatedFromCadenceID`) + `cadence.notify` в `CadenceCreateRequest` + tx-рефактор `Create` (Insert Cadence + InsertTiding постоянных правил одной tx + инвалидация) + `herald.read`-guard + ULID-привязка. _(текущий слайс)_
- **C2 (UI)** — блок «Notify» в форме Cadence (companion-repo), parity RunWizard.
- **C3 (MCP)** — `notify` в MCP-tool создания Cadence.
