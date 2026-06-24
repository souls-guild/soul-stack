# Known limitations — что не входит в бету

Закрытая малая бета рассчитана на единицы операторов и флот до сотен хостов. Этот документ честно перечисляет, чего в бете **нет** или что работает с ограничениями, — чтобы бета-тестер не упёрся в это молча и не принял отсутствие фичи за баг.

Каждый пункт — со ссылкой на канон (ADR / runbook), где описано «как будет» или «почему отложено». Дизайн фиксируется в ADR раньше кода; «отложено» означает «решение принято, кода в бете нет».

## Cloud-provisioning — НЕ в бете

Бета работает с **существующими хостами**: оператор сам поднимает VM/железо и онбордит Soul ([getting-started.md → Шаг 6](getting-started.md#шаг-6-онбордить-один-soul)). Динамическое создание VM из Soul Stack в бету не входит.

- **Provider и Profile** (учётка облака + шаблон VM) — концепция есть, хранятся в Postgres, но **REST-роуты `POST /v1/providers` / `POST /v1/profiles` отложены** — cloud-CRUD не реализован ([keeper/cloud.md → Provider и Profile](keeper/cloud.md#provider-и-profile-в-postgres), [operator-api.md → Cloud](keeper/operator-api.md)). Управления Provider/Profile через REST / MCP / UI в бете **нет**.
- Шаг сценария `core.cloud.provisioned` (`on: keeper`, вызов CloudDriver-плагина) спроектирован ([ADR-017](adr/0017-keeper-side-core.md)), но без сконфигурированных Provider/Profile использовать его в бете нельзя.

Хотите динамический provisioning — это пост-бета. Сейчас: создайте хост вне Soul Stack, затем `POST /v1/souls` + `soul init`.

## MCP не покрывает все домены

Первичный интерфейс оператора — REST (OpenAPI) и MCP. Но MCP-симметрия с REST неполная — часть доменов доступна **только через REST/UI**:

- **Cadence** (расписания регулярных прогонов, [ADR-046](adr/0046-cadence.md)) — **MCP-tool-ов нет** ([operator-api/cadences.md](keeper/operator-api/cadences.md)). Создание/изменение расписаний — только через REST `/v1/cadences*` или Web-UI.
- **Audit-чтение** (`GET /v1/audit`) — MCP-симметрия отложена ([operator-api/audit.md](keeper/operator-api/audit.md)).
- **Choir** (топология хостов внутри incarnation) и **Module-catalog** (`/v1/modules`) — REST-only по дизайну ([operator-api.md → Choir / Module-catalog](keeper/operator-api.md)).

Часть MCP-tool-ов заведена как **stub**: они помечены `status=stub` в манифесте и возвращают честный `not_implemented` (не падают, не делают вид, что отработали). В бете-stub:

- **`keeper.soul.list`** — `not_implemented`. Для списка Souls в бете используйте REST `GET /v1/souls` (фильтры `coven` / `status` / `transport` + pagination). Полное MCP-покрытие — пост-бета (M2).
- **`keeper.push.cleanup`** — `not_implemented`. REST-аналога в бете нет (push выполняется через `keeper.push.apply`). Отложено пост-бета.

Если автоматизируете через MCP — сверяйтесь с [keeper/mcp-tools.md](keeper/mcp-tools.md): там перечислены реально заведённые MCP-tool-ы. Отсутствие tool-а или его `stub`-статус — не баг, а граница покрытия беты.

## Audit-scaling — рассчитан на малую бету

`audit_log` — главный потребитель объёма Postgres при росте флота: на целевом масштабе 100k VM объём прогонов упирается в INSERT-rate и размер таблицы. Для **малой беты** (до сотен хостов) это не проблема, штатной retention хватает (`purge_audit_old`, default 365 дней, [operations/infra.md → Retention](operations/infra.md#retention-и-housekeeping)).

Что отложено до пост-беты (не нужно для малого флота):

- **Партиционирование `audit_log`** по `created_at` (declarative partitioning / BRIN) — расширение, не breaking ([ADR-022](adr/0022-audit-pipeline.md)).
- **Pluggable audit-sink (Kafka-выгрузка)** — **спроектирован, в бете не реализован** ([ADR-059](adr/0059-audit-sink-pluggable.md), proposed / deferred). На целевом масштабе backend audit-выгрузки становится выбираемым (`audit.sink: pg | kafka | off`, default `pg`); Kafka-sink (at-least-once `acks=all`, fail-closed, дедуп downstream по `audit_id`) снимает PG-write-нагрузку, остаётся строго опциональным (обязательный контур PG+Redis+Vault не меняется, [ADR-053](adr/0053-dependency-tiers.md)). **Вытесняет вариант Redis-Stream-буферизации** (Kafka покрывает ту же ось write-throughput полноценнее; Redis — hot-слой, не долговременный audit-буфер). Перед реализацией требует развязки зависимости: `changed_tasks`/`GET /v1/audit` сегодня деривят данные из `audit_log` в PG ([ADR-059](adr/0059-audit-sink-pluggable.md) open question).
- **Hot-cold / batched-INSERT** аудита для крупных флотов — backlog следующих релизов. **batched-INSERT остаётся** более дешёвой альтернативой на оси write-throughput (батч-flush PG-sink-а без новой инфраструктуры), не вытесняется Kafka-sink-ом.

Если планируете флот в тысячи+ хостов — это вне профиля беты; следите за размером `audit_log` и `apply_runs` ([operations/infra.md → Размер таблиц](operations/infra.md#размер-таблиц-приблизительная-оценка)).

## Прочие границы беты

### Supply-chain: подпись образов отложена

`make sign` — documented stub (печатает причину и завершается успешно). Реальная cosign/sigstore-подпись Docker-образов требует registry + OIDC-identity из CI, которых у локального репозитория нет ([deploy/README.md → Подпись образов](../deploy/README.md)). SBOM (`make sbom`) при этом работает.

### Внешний pentest — не проводился (внутренний gate достаточен для беты)

Независимый внешний pentest на момент беты **не выполнялся**. Граница гарантий держится на внутреннем security-gate: deep ИБ-аудит 2026-06-12 (0 critical/high), threat-model, чистый `govulncheck` по всем модулям и security-ревалидация OpenAPI-пивота (PASS) — состав и обоснование в [security/threat-model.md → Статус внешнего аудита / pentest](security/threat-model.md#статус-внешнего-аудита--pentest). Решение от 2026-06-15: для закрытой малой беты этого достаточно; внешний независимый pentest запланирован пост-бета / перед GA.

### Identity оператора: только JWT

Форма credential Архонта в бете — **JWT** (HS256, signing-key из Vault). mTLS-cert-форма и transit-подпись JWT — пост-MVP, расширение через `auth_method` enum без breaking changes ([ADR-014](adr/0014-operator-identity.md), [operations/bootstrap-rbac.md → Machine-identity](operations/bootstrap-rbac.md#machine-identity-ci--scripts)).

**Немедленный отзыв всех живых JWT** в бете отсутствует: после `revoke` Архонта его активные токены работают до `exp`. Аварийный отзыв — только через ротацию signing-key ([operations/bootstrap-rbac.md → Аварийный отзыв](operations/bootstrap-rbac.md#аварийный-отзыв-всех-jwt--ротация-signing-key)). Защита — короткий `ttl_default`.

### Push (agentless по SSH) — узкий профиль

`keeper.push` (доставка Destiny по SSH без агента) работает, но без host-CA / `ssh_providers` это no-op ([ADR-053 → optional-with-degradation](adr/0053-dependency-tiers.md)). Профиль беты — pull (агент-демон `soul`), push используйте только при настроенном SSH-provider-е.

### Served OpenAPI описывает полную поверхность продукта

Часть ручек относится к опциональным доменам, которые монтируются только при включении соответствующей фичи в конфиге Keeper (например push/SSH-доставка — при настроенных `plugins.ssh_providers` + `push.host_ca_ref`; Sigil/sigil-keys — при включённом allow-list плагинов). Если фича не включена на конкретном инстансе, ручка вернёт `404 "no such endpoint"` — в том числе при попытке вызвать её из /docs (RapiDoc «Try It»). Это ожидаемо: спека = стабильный контракт всего продукта, доступность ручки зависит от конфигурации деплоя (pull-only инсталляция без push/Sigil — штатный режим). Авторитетный список опциональных/feature-gated доменов — `pathAllowlist` в `keeper/internal/api/openapi_drift_test.go` (защищён guard-тестом `TestFullSpec_CoversAllRoutes`).

### Recovery прерванных прогонов — выключено по умолчанию

`reclaim_apply_runs` (Reaper подбирает зависшие после краша Keeper-инстанса прогоны) **выключен** в конфиге по умолчанию — включается только после раскатки fencing-Soul + `acolytes>0` ([operations/deployment.md → keeper.yml](operations/deployment.md#конфиг-keeperyml--обязательный-минимум), [keeper/reaper.md](keeper/reaper.md)). Для малой single-keeper-беты это не требуется; зависший прогон оператор перезапускает вручную.

### UI `/oracle/fires` — заглушка

Страница `/oracle/fires` в Web-UI — placeholder с явным WIP-сообщением: backend `GET /v1/oracle/fires` не реализован (таблица `oracle_fires` уже есть, query-фаза отложена). Просмотр срабатываний Decree в бете — через **Audit Log** с фильтром `type=decree.fired`. Отложено пост-бета (Oracle query-phase).

### Sentinel-Redis: без нативного master-discovery

`redis.addr` принимает один TCP-адрес. Redis Sentinel с автоматическим master-discovery нативно не поддержан — раскатка через TCP-прокси на динамический master ([operations/infra.md → HA Redis](operations/infra.md#ha-redis)). Single-instance и Redis Cluster поддержаны. Для малой беты — single-instance + AOF.

## См. также

- [getting-started.md](getting-started.md) — quickstart, путь онбординга существующего хоста.
- [operations/](operations/README.md) — прод-runbook (deployment / infra / scaling / disaster-recovery).
- [architecture.md → Открытые вопросы](architecture.md#открытые-вопросы) — развилки, ещё не закрытые ADR.
