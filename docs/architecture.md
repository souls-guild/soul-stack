# Архитектура Soul Stack

Документ — единственный источник правды по верхнеуровневой архитектуре. Если решение здесь и в коде расходятся — сначала обновляется документ, потом код. Изменения через явные ADR-блоки ниже.

## Содержание

- [Назначение системы](#назначение-системы)
- [Принятые решения (ADR-001…039)](#принятые-решения)
- [Топология](#топология)
- [Жизненный цикл Soul и реестр душ](#жизненный-цикл-soul-и-реестр-душ)
- [Подключение Soul: priority и failback](#подключение-soul-priority-и-failback)
- [Push-режим (`keeper.push`)](#push-режим-keeperpush)
- [Модель модулей](#модель-модулей)
- [Plugin-инфраструктура](#plugin-инфраструктура)
- [Артефакты Soul Stack: что в git, что в БД](#артефакты-soul-stack-что-в-git-что-в-бд)
- [Destiny: входной контракт и валидация](#destiny-входной-контракт-и-валидация)
- [Service — структура и manifest](#service--структура-и-manifest)
- [Essence: pipeline сборки](#essence-pipeline-сборки)
- [Incarnation — runtime-инстанс сервиса](#incarnation--runtime-инстанс-сервиса)
- [Targeting и связь хостов](#targeting-и-связь-хостов)
- [Versioning и миграции state_schema](#versioning-и-миграции-state_schema)
- [Cloud-интеграция через `keeper.cloud`](#cloud-интеграция-через-keepercloud)
- [Reaper / Жнец](#reaper--жнец)
- [Доставка SoulSeed-токена на хост](#доставка-soulseed-токена-на-хост)
- [End-to-end сценарий установки](#end-to-end-сценарий-установки)
- [Поток данных верхнего уровня](#поток-данных-верхнего-уровня)
- [Сквозные требования](#сквозные-требования-и-где-они-приземляются)
- [Открытые вопросы](#открытые-вопросы)

Сопровождающие документы:
- [docs/README.md](README.md) — индекс всей документации.
- [docs/naming-rules.md](naming-rules.md) — словарь имён.
- [docs/requirements.md](requirements.md) — продуктовые требования.
- [../CLAUDE.md](../CLAUDE.md) — гайд для AI-агентов и краткая сводка решений.

## Назначение системы

Soul Stack — система управления конфигурациями (configuration management), идейно близкая к SaltStack, но со своим словарём имён ([docs/naming-rules.md](naming-rules.md)) и собственной архитектурой. Применяется для:

- декларативного описания желаемого состояния хостов (**Destiny**),
- сбора фактов о хостах (**Soulprint**),
- хранения параметров и секретов (**Essence**),
- удалённого исполнения и проверки прогона.

Поддерживаются две модели доставки:

- **pull-модель.** На хосте установлен демон **Soul**, он держит долгоживущий gRPC-стрим к Keeper-у и применяет команды по мере поступления.
- **push-модель.** Keeper сам ходит на хост по SSH (`keeper.push`) и применяет Destiny без агента. Используется для одноразовых задач и хостов, где Soul-агент нежелателен.

## Принятые решения

Каждое решение сформулировано как ADR: контекст, выбор, обоснование, ключевой trade-off. Менять решение можно только через правку соответствующего блока. ADR-008 (Coven — стабильные теги) и ADR-009 (scenario — полная DSL задач) дополнены спецификацией в [`docs/scenario/`](scenario/README.md).

### [ADR-001. Язык реализации — Go](adr/0001-language-go.md)

Вынесен в [`docs/adr/0001-language-go.md`](adr/0001-language-go.md). Go как язык всех бинарей системы: статическая компиляция, зрелые SDK под весь стек (Vault / OTel / gRPC / MCP / k8s), низкий порог входа для контрибьюторов; trade-off — GC и runtime-вес выше Rust.

### [ADR-002. Транспорт Keeper ↔ Souls — gRPC bidirectional stream поверх mTLS, HA-кластер Keeper](adr/0002-transport-grpc-ha.md)

Вынесен в [`docs/adr/0002-transport-grpc-ha.md`](adr/0002-transport-grpc-ha.md). Bidirectional gRPC-стрим поверх mTLS, инициирует Soul; Keeper — горизонтально масштабируемый stateless-кластер поверх общей Postgres/Redis (KID на инстанс); Soul-клиент держит fallback-list endpoints. Amendments: presence — Redis-derived (SID-lease, не PG); soul-shedding (Watchman сбрасывает стримы при изоляции); Shepherd — балансировка при scale-out (PLANNED/backlog).

### [ADR-003. Формат Destiny — YAML с типизированной схемой (CUE/JSON Schema)](adr/0003-destiny-format.md)

Вынесен в [`docs/adr/0003-destiny-format.md`](adr/0003-destiny-format.md). Источник истины — YAML + типизированная схема (JSON Schema → CUE); шаблонизация отдельной безопасной фазой перед валидацией (движки — [ADR-010](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)); жёсткое разделение «render → validate → apply».

### [ADR-004. Раскладка бинарей — `keeper`, `soul`, `soul-lint`; push-режим — модуль внутри `keeper`](adr/0004-binaries.md)

Вынесен в [`docs/adr/0004-binaries.md`](adr/0004-binaries.md). Четыре отдельных бинаря (`keeper` / `soul` / `soul-lint` / `soul-trial`); push — модуль `keeper.push` внутри `keeper`. Никакого смешения ролей Keeper и Soul в одном бинаре (subcommand внутри роли допустим); основной интерфейс оператора — OpenAPI и MCP, CLI — тонкая обёртка. Amendment: push Variant C (`POST /v1/push/apply` — ad-hoc multi-host orchestrator).

### [ADR-005. Хранилище состояния Keeper — Postgres](adr/0005-storage-postgres.md)

Вынесен в [`docs/adr/0005-storage-postgres.md`](adr/0005-storage-postgres.md). Postgres — единственное холодное хранилище состояния Keeper-кластера (реестр Souls, история сертификатов, Destiny-каталог, журналы, операторские артефакты); никаких embedded KV. Целевой масштаб — десятки–тысячи Souls; SQLite-вариант для мелких инсталляций — возможный, но не обязательство.

### [ADR-006. Кэш и координация — Redis](adr/0006-cache-redis.md)

Вынесен в [`docs/adr/0006-cache-redis.md`](adr/0006-cache-redis.md). Redis — heartbeat-кэш (a), lease на SID (b), pub/sub между Keeper-инстансами (c), лидер для фоновых задач/Reaper (d). Amendments: presence Souls — производная SID-lease + `mark_disconnected` lease-aware двунаправленный; presence Keeper-инстансов — реестр Conclave (роль e, refuse-startup-guard); снимок нагрузки в Conclave для Shepherd (PLANNED/backlog).

### [ADR-007. Версионирование артефактов — через git ref, а не через поле в манифесте](adr/0007-versioning-git-ref.md)

Вынесен в [`docs/adr/0007-versioning-git-ref.md`](adr/0007-versioning-git-ref.md). Версия артефакта (Service / Destiny / Module / Plugin) = git ref, поля `version:` в манифестах нет; зависимости — строго `ref:` (tag или branch), без semver-range. Исключения (не «версии артефакта»): `state_schema_version`, `protocol_version`, `service_version`, Go-модули внутри репо (совместный semver-тег).

### [ADR-008. Coven — только стабильные логические теги](adr/0008-coven-stable-tags.md)

Вынесен в [`docs/adr/0008-coven-stable-tags.md`](adr/0008-coven-stable-tags.md). Coven — только стабильные логические теги (кластер / проект / окружение / ЦОД); `incarnation.name` остаётся корневой Coven-меткой; convention `{incarnation.name}-{role}` удалена; роль НЕ Coven (declared в spec / actual через probe); essence role-agnostic. Amendments: окружение = частный случай Coven (first-class `Environment` отвергнут, per-Coven RBAC-scope реализован); cross-incarnation снят на Voyage-слое; Choir ≠ coven.

### [ADR-009. Scenario — полная DSL задач destiny; граница с destiny — рекомендация](adr/0009-scenario-dsl.md)

Вынесен в [`docs/adr/0009-scenario-dsl.md`](adr/0009-scenario-dsl.md). Инвариант «scenario без `module:`» снят полностью: scenario получает всё DSL-ядро задач destiny (источник правды — `docs/destiny/tasks.md`) + оркестрационную дельту (`on:`/`where:`/`apply:`/`state_changes`); граница destiny/scenario — рекомендация (три «да»: переиспользование / molecule-идемпотентность / изолируемость); barrier/state-commit — инвариант (cross-host final-barrier, `error_locked` при finally-failed). Amendments: Tide (invocation-time scope chunking, поглощён Voyage); §7 per-incarnation state-commit в Voyage; Choir — additive `choirs[]`.

### [ADR-010. Шаблонизатор: CEL для YAML-выражений, Go text/template для файлов](adr/0010-templating.md)

Вынесен в [`docs/adr/0010-templating.md`](adr/0010-templating.md). Два движка со строгой границей по файлу: CEL (google/cel-go) — все YAML-выражения (top-level expression-ключи без обёртки, интерполяция через маркер `${ … }`); Go text/template + sprig-allowlist — рендер файлов `templates/<path>.tmpl` через `core.file.rendered`. CEL sandbox-by-design; `.j2` → `.tmpl`; `soulprint.hosts.where(...)` — compile-time rewrite в native CEL-comprehension. Полная спека — [docs/templating.md](templating.md).

### [ADR-011. Раскладка Go-кода: go.work с модулями по сторонам](adr/0011-go-layout.md)

Вынесен в [`docs/adr/0011-go-layout.md`](adr/0011-go-layout.md). Вариант B — `go.work` с семью модулями по сторонам (`proto/` / `proto/plugin/` / `shared/` / `sdk/` / `keeper/` / `soul/` / `soul-lint/`); изоляция Soul гарантируется компилятором (`soul/go.mod` не require keeper); committed generated Go; совместные semver-теги; server-side драйверы в `<binary>/internal/`, не в `shared/`. Amendment: упразднение `proto/operator/v1`. Форма Operator API — **Go-типы huma code-first** ([ADR-054](#adr-054-operator-api--разворот-на-code-first-go-типы--openapi-через-huma-v2)), спека производна; oapi-codegen-каркас ([ADR-051](#adr-051-operator-api-codegen-openapi--go-типы-oapi-codegen-types-only--strict)) и пакет `keeper/internal/api/oapi/` снесены (2026-06-13).

### [ADR-012. Контракт Keeper↔Soul gRPC: один EventStream с oneof, Keeper-side рендер, forward-compat only-add](adr/0012-keeper-soul-grpc.md)

Вынесен в [`docs/adr/0012-keeper-soul-grpc.md`](adr/0012-keeper-soul-grpc.md). Один `service Keeper` с двумя RPC — unary `Bootstrap` (server-only TLS, отдельный listener) и долгоживущий bidi `EventStream` (mTLS) с `oneof payload`; тематическая раскладка `.proto` в `proto/keeper/v1/`; forward-compat only-add (breaking — через `v2/`); граница рендера — по внешнему доступу (CEL params/vault — Keeper, text/template-COMPUTE + flow-control CEL — Soul); `RunResult` — финальный отчёт прогона; SID в payload — echo, авторитет — mTLS peer cert. Amendment: `WardRoster` (Soul-reconcile, FromSoul field 8).

### [ADR-013. Bootstrap первого Архонта](adr/0013-bootstrap-archon.md)

Вынесен в [`docs/adr/0013-bootstrap-archon.md`](adr/0013-bootstrap-archon.md). Bootstrap первого Архонта (имя сущности — Archon, идентификатор — AID): administrative subcommand `keeper init --archon=<aid>` под PG advisory lock создаёт первого Архонта с ролью `cluster-admin`, выпускает JWT в файл `mode 0400`; restart-отказ при пустом `operators` без `--initialize`; self-lockout-инвариант на последний `*`-permission.

### [ADR-014. Identity-модель оператора (Archon)](adr/0014-operator-identity.md)

Вынесен в [`docs/adr/0014-operator-identity.md`](adr/0014-operator-identity.md). Реестр `operators` в Postgres (обязательный, с настоящими FK), credential — JWT (signing key из Vault KV), идентификатор AID (charset `[a-z0-9._@-]`, префикс `archon-` снят amendment-ом); жизненный цикл создание/ревокация через OpenAPI/MCP; near-instant revocation через RBAC-снимок (amendment).

### [ADR-015. Core-модули MVP: точный список](adr/0015-core-modules-mvp.md)

Вынесен в [`docs/adr/0015-core-modules-mvp.md`](adr/0015-core-modules-mvp.md). 17 Soul-side core-модулей (`core.pkg`/`core.file`/`core.service`/`core.user`/`core.group`/`core.exec`/`core.cmd`/`core.cron`/`core.mount`/`core.git`/`core.archive`/`core.sysctl`/`core.url`/`core.line`/`core.repo`/`core.firewall`/`core.http`) + 3 Keeper-side (`core.soul.registered`/`core.cloud.provisioned`/`core.vault.kv-read`); `core.template`/`core.copy` сознательно НЕ выделяются; `core.line`/`core.repo`/`core.firewall`/`core.http` приняты пост-факто (in-place / read-probe MVP).

### [ADR-016. Стратегия parity с SaltStack/Ansible и лицензия Soul Stack](adr/0016-parity-license.md)

Вынесен в [`docs/adr/0016-parity-license.md`](adr/0016-parity-license.md). Лицензия — Apache 2.0 (open core / freemium: enterprise-фичи в отдельных репо). Стратегия parity — гибрид без wrapper-а: core MVP — наш Go-рерайт, экзотика — community-плагины через SDK; wrapper Ansible запрещён (GPLv3 + Python-runtime). Amendments: Plugin SDK Фаза 2 (10 official `soul-mod-*`, namespace `official`, template-механизм).

### [ADR-017. Keeper-side core-модули расширены: `core.cloud.provisioned`, `core.vault.kv-read`](adr/0017-keeper-side-core.md)

Вынесен в [`docs/adr/0017-keeper-side-core.md`](adr/0017-keeper-side-core.md). Два keeper-side core-модуля (`on: keeper`): `core.cloud.provisioned` (`created`/`destroyed` через `CloudDriver`-плагин, cascade при destroy) заменяет паттерн «destiny `cloud-provision`»; `core.vault.kv-read` (`read`) — явное аудит-аккуратное чтение Vault KV при рендере. Amendments: cloud credentials-flow (Variant A) + 6 реализованных провайдеров + cloud-init bootstrap.

### [ADR-018. Soulprint typed-схема MVP](adr/0018-soulprint-typed.md)

Вынесен в [`docs/adr/0018-soulprint-typed.md`](adr/0018-soulprint-typed.md). Typed `SoulprintFacts` (sub-messages `OsFacts`/`KernelFacts`/`CpuFacts`/`MemoryFacts`/`NetworkFacts`) вместо `google.protobuf.Struct`-stub (deprecated, wire-compat); `os.pkg_mgr`/`os.init_system` собирает Soul-агент; каноническая CEL-форма `soulprint.self.<path>`; `covens`/`choirs` — Keeper-проекция, не факт. Amendments: гибрид pkg_mgr/init_system, `choirs`-факт, `typed_facts` byte-passthrough на REST.

### [ADR-019. State_schema migration DSL](adr/0019-state-migration-dsl.md)

Вынесен в [`docs/adr/0019-state-migration-dsl.md`](adr/0019-state-migration-dsl.md). Грамматика плоский (`rename`/`set`/`delete`/`move`) + CEL в `set.value` через `${ … }` + структурный `foreach`; migration-CEL sandbox (запрет `vault`/`now`/`register`/`soulprint`/`essence`/`input`); forward-only; атомарная PG-транзакция на цепочку; тесты `migrations/<NNN>_to_<MMM>/tests/`.

### [ADR-020. Plugin-инфраструктура: формат manifest, handshake, lifecycle](adr/0020-plugin-infrastructure.md)

Вынесен в [`docs/adr/0020-plugin-infrastructure.md`](adr/0020-plugin-infrastructure.md). Единая инфраструктура трёх kind-ов плагинов (`soul_module`/`cloud_driver`/`ssh_provider`): статический `manifest.yaml` (offline-валидация `soul-lint`), JSON-handshake с магическим префиксом → gRPC-over-unix-socket, `protocol_version` ↔ `proto/plugin/vN/`, one-shot lifecycle, closed-enum `required_capabilities` / `side_effects`, file-permissions вместо mTLS. Amendments: SDK Фаза 2, SshProvider-набор + credentials-flow.
### [ADR-021. Hot-reload конфига с write-back YAML](adr/0021-hot-reload-config.md)

Вынесен в [`docs/adr/0021-hot-reload-config.md`](adr/0021-hot-reload-config.md). Два пути изменения (file-edit `SIGHUP` / API-MCP с write-back YAML round-trip); validation-pipeline (parse→schema→semantic→atomic swap); per-host без cross-host; audit-events `config.reload_succeeded`/`config.reload_failed`; `shared/config` для трёх бинарей; reload-able vs require-restart общий принцип.

### [ADR-022. Audit-pipeline: storage, schema, retention](adr/0022-audit-pipeline.md)

Вынесен в [`docs/adr/0022-audit-pipeline.md`](adr/0022-audit-pipeline.md). Postgres-таблица `audit_log` (ULID PK, `source` closed-enum 5 значений, `correlation_id` ULID, `payload` jsonb); retention через Reaper-правило `purge_audit_old`; OTel dual-write (opt); write-path инициаторы; блок `audit:` в `keeper.yml`; `GET /v1/audit` + `audit.read` — отдельной задачей.

### [ADR-023. Тест-раннер Trial (`soul-trial`) и DSL-coverage](adr/0023-trial-test-runner.md)

Вынесен в [`docs/adr/0023-trial-test-runner.md`](adr/0023-trial-test-runner.md). Сущность Trial, бинарь `soul-trial`, метрика trial coverage; уровни L0 render-only / L1 migration / L2 single-host docker (реализован, test-only) / L3 multi-host (отложен); `CoverageSink` в `shared/cel`; раскладка `keeper/cmd/soul-trial`; формат case.yml расширяет migration-эталон.

### [ADR-024. Observability: Prometheus-primary + OTel-bridge](adr/0024-observability.md)

Вынесен в [`docs/adr/0024-observability.md`](adr/0024-observability.md). Prometheus-primary (`/metrics` pull) + OTel-bridge для трейсов и опц. push метрик; namespace-префиксы `keeper_*` / `soul_*`; OTel resource-attrs `service.name` + `soulstack.kid` / `soulstack.sid`.

### [ADR-025. Augur — Keeper-side брокер внешнего доступа Soul](adr/0025-augur.md)

Вынесен в [`docs/adr/0025-augur.md`](adr/0025-augur.md). Keeper-side брокер live-доступа Soul к внешним системам (Omen-реестр / Rite-grant); MVP-1 брокер (`delegate=false`, данные через Keeper) → MVP-2 делегация (scoped Vault-токен / static read-cred); транспорт only-add `AugurRequest`/`AugurReply` в EventStream; инвариант «Soul никогда не получает master-credential». Дизайн, имплементации нет.

### [ADR-026. Sigil — целостность плагинов (Keeper-signed digest-индекс)](adr/0026-sigil.md)

Вынесен в [`docs/adr/0026-sigil.md`](adr/0026-sigil.md). Keeper-signed allow-list плагинов (реестр `plugin_sigils`): Архонт явно допускает `(namespace, name, ref) → sha256`, Keeper подписывает блок с пришитым manifest, host верифицирует digest+подпись ДО exec (заменяет TOFU first-load). Git-verified `ref` (go-git F-fetch), multi-anchor ротация ключей подписи (`sigil_signing_keys`), replace-семантика snapshot/anchors.

### [ADR-027. Модель исполнения apply — work-queue + claim (Acolyte-пул, Ward-claim)](adr/0027-apply-work-queue.md)

Вынесен в [`docs/adr/0027-apply-work-queue.md`](adr/0027-apply-work-queue.md). Исполнение apply на work-queue + claim: Acolyte-пул, Ward-claim (`FOR UPDATE SKIP LOCKED`), Summons (`apply:summons`), just-in-time render при claim, recovery-скан Reaper (`reclaim_apply_runs`), двусторонний attempt-fencing, single-winner state-commit; Phase 0/1/2 + GATE-1 (deliver-once, lifecycle `planned→claimed→dispatched→terminal`) реализованы, Phase 3 distributed serial отложен; amendments Conclave/Watchman/refuse-guard, Voyage back-link, Tide-spawn.

### [ADR-028. RBAC-storage → Postgres](adr/0028-rbac-storage.md)

Вынесен в [`docs/adr/0028-rbac-storage.md`](adr/0028-rbac-storage.md). Перенос RBAC-storage (роли, permissions, membership) из `keeper.yml` в Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`), фикс BUG-1 (init пишет membership в БД, а не в JWT-claim); enforcer на in-memory снимке из БД + Redis-инвалидация; включает Amendment ADR-049 (Synod).

### [ADR-029. Реестр Service-ов → Postgres](adr/0029-service-registry.md)

Вынесен в [`docs/adr/0029-service-registry.md`](adr/0029-service-registry.md). Реестр Service-ов (`keeper.yml → services[]`) и well-known скаляры (`default_destiny_source`) переносятся в Postgres (`service_registry` + key-value `keeper_settings`), runtime — снимок `serviceregistry.Holder` + `service:invalidate`; config hard-cut трёх ключей; закрывает остаток [ADR-028(h)](adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres).

### [ADR-030. Vigil + Oracle — event-driven мониторинг (beacons + reactor)](adr/0030-vigil-oracle.md)

Вынесен в [`docs/adr/0030-vigil-oracle.md`](adr/0030-vigil-oracle.md). Event-driven контур (Salt beacons+reactor-аналог): Vigil (Soul-side read-only проверка) → Portent (edge-triggered событие, only-add в EventStream) → Oracle (Keeper-side reactor-роутер) → Decree (default-deny правило, action=named-scenario-only). Обязательные инварианты loop-prevention (cooldown+circuit-breaker) и security. Amendment S5: typed PortentPayload, `soul_beacon` plugin-kind, inotify-beacon; feature-complete.

### [ADR-031. Scry — drift-detection (declarative dry-run reconcile)](adr/0031-scry-drift.md)

Вынесен в [`docs/adr/0031-scry-drift.md`](adr/0031-scry-drift.md). Read-only подсистема declarative drift-детекта: `Plan` no-op→pure-read во всех core-модулях, only-add `PlanEvent.changed` / `ApplyRequest.dry_run`, информационный статус `drift` (не блокирующий), `DriftReport`, default-deny для community-модулей без read-safe. On-demand-пилот (`check-drift`) → фоновый скан. Amendments: Slice B/C реализованы, Plan-тираж 14 модулей, `converge` → operational scenario-kind (2026-06-10).

### [ADR-032. Push-orchestrator (Variant C) — multi-host destiny push без incarnation/scenario.](adr/0032-push-orchestrator.md)

Вынесен в [`docs/adr/0032-push-orchestrator.md`](adr/0032-push-orchestrator.md). Multi-host параллельная выкатка destiny на список SID без scenario/incarnation/state: `POST /v1/push/apply`, таблица `push_runs`, статусы (вкл. `partial_failed`), узкий render-context, best-effort recovery через Reaper; не мутирует `incarnation.state`. Amendments: S6 pilot wire-up → S7-1…S7-4 PG-canon (`souls.ssh_target` / `push_providers` / multi-CA / auto-import) + runtime re-spawn + P2 multi-provider routing.

### [ADR-033. Errand — pull-ad-hoc exec вне scenario.](adr/0033-errand.md)

Вынесен в [`docs/adr/0033-errand.md`](adr/0033-errand.md). Pull-ad-hoc exec одиночного модуля через уже стоящий Soul-agent (НЕ scenario/apply/incarnation-bound): whitelist (`core.cmd.shell`/`core.exec.run` + marker `ErrandReadSafe`), sync/async-гибрид (`POST /v1/souls/{sid}/exec`), не мутирует `incarnation.state`, область `errand.*`, only-add `ErrandRequest`/`ErrandResult`/`CancelErrand`, таблица `errands`. Amendments: E5 cancel, Voyage `command`-kind реюзит инфру.

### [ADR-035. Distribution split — core (API+CLI) vs web (UI).](adr/0035-distribution-split.md)

Вынесен в [`docs/adr/0035-distribution-split.md`](adr/0035-distribution-split.md). Разделение дистрибуции: core = только Go-артефакты (`keeper`/`soul`/`soul-lint`/`soulctl`/proto/sdk/shared), web — отдельный companion-репо `soul-stack-web`; контракт core↔web = OpenAPI (read-only); независимые релизные циклы; оператор может работать без UI (CLI+MCP+OpenAPI). `ui/` удаляется из core. **Amended [ADR-055](#adr-055-embed-ui-bundle--опциональный-single-binary-keeper-с-ui-на-ui):** отложенный embed-compat-shim активирован как опциональный default-ON embed UI на `/ui` (бета single-binary onboarding) — companion-source-of-truth и toolchain-split сохранены, разворачивается только инвариант «никакого embed UI-assets» (теперь собранный артефакт допущен).

### [ADR-038. Toll — cluster-wide detector массового оттока Souls.](adr/0038-toll.md)

Вынесен в [`docs/adr/0038-toll.md`](adr/0038-toll.md). Passive cluster-level detector массового оттока Souls (rate-of-disconnect по gRPC-событиям, sliding 60s, threshold 20% от baseline): per-instance `tollwatcher` + Redis-leader агрегация, soft-degraded mode (503 на write-API, read/destroy/Errand доступны), asymmetric hysteresis, warmup-immunity; НЕ закрывает стримы (это Watchman). Amendment: webhook (generic/pagerduty/slack) + per-coven thresholds + hot-reload.

### [ADR-039. E2E-тестирование — три уровня без новой сущности словаря](adr/0039-e2e-testing.md)

Вынесен в [`docs/adr/0039-e2e-testing.md`](adr/0039-e2e-testing.md). L3 e2e без новой сущности словаря, через build-tag Go-tests: L3a fast-loop (soul-stub helper + testcontainers + реальный Keeper-процесс, every-PR), L3b smoke-loop (real `soul`-бинарь в контейнере, nightly), L3c k8s-loop (kind-cluster, HA-кейсы, weekly); fixtures+expectations в YAML, assertion-значения = code enums. Amendment: L3a-impl particulars.

### [ADR-040. Tide — invocation-time scope chunking + target-override](adr/0040-tide.md)

Вынесен в [`docs/adr/0040-tide.md`](adr/0040-tide.md). **Superseded by [ADR-043 (Voyage)](adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), 2026-05-29** (поглощён режимом `kind=scenario`: Surge → Leg, per-Surge state-commit → per-incarnation; реализация удалена в Wave 5 — migration `061`, пакеты `tideorch`/`tide`, `/v1/tides`, audit `tide.*`). Исходная фиксация — invocation-time chunking scenario-прогона на последовательные Surge-волны (сущности Tide/Surge) + AND-merge target-override (только сужение scope) + concurrency-override; PG-таблица `tides`, claim+lease failover (Acolyte-style), snapshot-scope `target_resolved_souls`.

### [ADR-041. ErrandRun — multi-target обвязка над Errand.](adr/0041-errandrun.md)

Вынесен в [`docs/adr/0041-errandrun.md`](adr/0041-errandrun.md). **Superseded by [ADR-043 (Voyage)](adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), 2026-05-29** (поглощён режимом `kind=command`; реализация удалена в Wave 5 — migration `062`, пакеты `errandrun`/`errandrunorch`, `/v1/errand-runs`, audit `errand_run.*` → `command_run.*`). Исходная фиксация — multi-target обвязка над N ad-hoc `Errand` (общий ULID, AND-merge target, concurrency-cap, cancel-all; реестр `errand_runs`). Single [Errand](#adr-033-errand--pull-ad-hoc-exec-вне-scenario) (`POST /v1/souls/{sid}/exec`) НЕ удалён, остаётся sugar.

### [ADR-042. Backend-driven dynamic data в UI — UI не хардкодит динамические каталоги.](adr/0042-backend-driven-ui.md)

Вынесен в [`docs/adr/0042-backend-driven-ui.md`](adr/0042-backend-driven-ui.md). UI не хардкодит динамические каталоги (RBAC permission-каталог, module-catalog, enum-ы статусов, ключи селекторов) — backend отдаёт их каталог-эндпоинтами (идентификаторы + машинные метаданные, human-label/i18n на UI с fallback на идентификатор); граница «не влияет на принимаемость запроса backend-ом и не растёт backend-side». Вводит `GET /v1/permissions`.

### [ADR-043. Voyage — унифицированный батчевый прогон.](adr/0043-voyage.md)

Вынесен в [`docs/adr/0043-voyage.md`](adr/0043-voyage.md). Voyage — единая top-level сущность батчевого прогона (единица батча — Leg), поглощающая Tide ([ADR-040](adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) + ErrandRun ([ADR-041](adr/0041-errandrun.md#adr-041-errandrun--multi-target-обвязка-над-errand)) + classic scenario-run: дискриминатор `kind` (`scenario` — батч N инкарнаций, per-incarnation state-commit B1 | `command` — батч N хостов, state не трогается), таблицы `voyages`/`voyage_targets`, выбор цели из RBAC-скоупа (не invocation-override), RBAC-by-`kind`, audit `scenario_run.*`/`command_run.*`, failover claim+lease (`reclaim_voyages` default-ON); включает amendments два `batch_mode` (`barrier`/`window`) + Salt-уровневые батч-стратегии + строковые `batch`/`max_failures` + `POST /v1/voyages/preview`.

### [ADR-044. Choir — именованная топология хостов внутри инкарнации](adr/0044-choir.md)

Вынесен в [`docs/adr/0044-choir.md`](adr/0044-choir.md). Choir — first-class именованная группа хостов внутри одной инкарнации (топологическая «партия»), Voice — членство SID в Choir (тройка `(incarnation_name, choir_name, sid)`): три РАЗНЫХ слоя (membership/coven/Choir не дублируются), Choir поглощает `spec.hosts[].role` (`voice.role` precedence), отдельные PG-таблицы `incarnation_choirs`/`incarnation_choir_voices` (НЕ `incarnation.state`), E1-таргетинг через `where:` + аксессоры `soulprint.*.choirs`, keeper-side core-модуль `core.choir`; реализовано (S-T2…S-T6 part(1a)), включает amendment precedence-role + multi-Choir-конфликт.

## Топология

```
                          ┌──────────────────────────┐
                          │       Оператор / CI      │
                          │  OpenAPI · MCP · gRPC    │
                          │   (CLI — тонкая обёртка) │
                          └──────────────┬───────────┘
                                         │  mTLS
                                         ▼
   ┌──────────────────────────────────────────────────────────────┐
   │                Keeper-кластер (HA, stateless)                │
   │     ┌────────┐   ┌────────┐   ┌────────┐                     │
   │     │ keeper │   │ keeper │   │ keeper │   …  N инстансов    │
   │     │  K1    │   │  K2    │   │  KN    │                     │
   │     │ + push │   │ + push │   │ + push │                     │
   │     └───┬────┘   └───┬────┘   └───┬────┘                     │
   │         │            │            │                          │
   │         └────────────┼────────────┘                          │
   │                      │                                       │
   │            ┌─────────┴──────────┐                            │
   │            ▼                    ▼                            │
   │      ┌──────────┐         ┌─────────────┐                    │
   │      │  Redis   │         │  Postgres   │                    │
   │      │ cache /  │         │  souls,     │                    │
   │      │ lease /  │         │  soul_seeds,│                    │
   │      │ pub-sub  │         │  destiny,…  │                    │
   │      └──────────┘         └─────────────┘                    │
   └─────┬─────────────────────────────────────┬──────────────────┘
         │ pull (Soul инициирует)              │ push (Keeper инициирует)
         │ gRPC bidi + mTLS                    │ SSH (Vault SSH CA / static / Teleport — open Q)
         ▼                                     ▼
   ┌──────────┐                          ┌──────────┐
   │   soul   │                          │   host   │      Управляемые хосты:
   │ (демон,  │                          │ (без     │      одна и та же запись
   │ агентский│                          │  агента, │      в реестре `souls`,
   │ режим)   │                          │  push)   │      разный transport.
   └──────────┘                          └──────────┘

   Параллельно:
   ┌──────────────┐
   │  soul-lint   │   офлайн-валидация Destiny + Essence
   │  (CI / dev)  │   на стороне разработчика, без Keeper
   └──────────────┘
```

### Роли бинарей

- **`keeper`.** Центральный сервер. Хранит реестр Souls (Postgres), политики RBAC, валидирует и рендерит Destiny, раздаёт команды Souls, агрегирует Soulprint и результаты прогонов, выставляет gRPC + OpenAPI + MCP. Содержит модуль `keeper.push` (SSH-доставка). Интегрируется с Vault для Essence (секретов) и для CA (выпуск SoulSeed-сертификатов).
- **`soul`.** Демон-агент на управляемом хосте; запускается как служба и работает постоянно. Поднимает gRPC bidi-стрим к Keeper, исполняет полученные команды, собирает Soulprint, отдаёт результаты. Никакого серверного кода Keeper, никакого исходящего трафика, кроме как к своему Keeper и к явно разрешённым ресурсам. Локальный admin-эндпоинт в MVP отсутствует ([open Q №8](#текущие) — post-MVP); admin-операции на Soul-host в MVP — SIGHUP для hot-reload `soul.yml` + локальный shell-доступ к логам/метрикам.
- **`soul-lint`.** Офлайн-линтер для Destiny и Essence. Разбирает, рендерит, валидирует по схеме, прогоняет статический анализ (несуществующие модули, циклы зависимостей, опечатки в Soulprint-таргетах). Запускается локально и в CI, не требует Keeper и сети.

## Жизненный цикл Soul и реестр душ

Идентичность Soul строится на трёх сущностях, фиксированных архитектурно:

- **SID = FQDN хоста** (не UUID). Это автоматически даёт дедуп при переустановке агента на тот же хост; обратная сторона — переименование FQDN означает миграцию (старая Soul → новая, in-place rename не поддерживается).
- **SoulSeed** — пара (mTLS-сертификат + приватный ключ), которой Soul аутентифицируется при подключении к Keeper. Выпускается через CSR при первом подключении (приватный ключ никогда не покидает хост), регулярно ротируется по живому стриму. В БД хранится **только `fingerprint`**, без PEM и приватных ключей; главная защита — приватный ключ CA в Vault.
- **Coven** — произвольная метка/тег для логического объединения Souls (по ЦОДу, по роли, по окружению). Используется в RBAC, таргетинге Destiny, потенциально в маршрутизации балансировщика (open Q «LB-1»).

Реестр Soul в Postgres разнесён на три таблицы: `souls` (одна запись на хост, статусы `pending` / `connected` / `disconnected` / `revoked`), `bootstrap_tokens` (одноразовые токены онбординга — инвариант `UNIQUE (sid) WHERE used_at IS NULL`, plain-токен в БД не хранится, только `token_hash`), `soul_seeds` (история сертификатов — инвариант `UNIQUE (sid) WHERE status='active'`).

Полные схемы таблиц, диаграмма переходов статусов, SQL-транзакция предъявления bootstrap-токена, алгоритм сжигания на стороне Soul-а, рекомендации оператору (`mode 0400`, systemd `LoadCredential=`), процедура ротации SoulSeed, revoke и переименование хоста — в [`docs/soul/identity.md`](soul/identity.md) и [`docs/soul/onboarding.md`](soul/onboarding.md). Соответствующие реестры в Postgres также описаны в [`docs/keeper/storage.md`](keeper/storage.md).

## Подключение Soul: priority и failback

Применимо к **агентскому** режиму (`transport: agent`). Push-режим использует другую модель — Keeper сам инициирует SSH-сессию к хосту, см. [Push-режим](#push-режим-keeperpush).

В конфиге Soul-а задан список endpoint-ов Keeper-а с числовым полем `priority`: меньшее число — более предпочтительно (как DNS MX, systemd, ip route), по умолчанию `priority: 1`. Между приоритетами — последовательно (1 → 2 → 3); внутри одного приоритета — последовательно с рандомизацией порядка (shuffle на каждой попытке): это равномерно нагружает endpoints и проще для корректной реализации, чем гонка параллельных handshake-ей.

Failback (возврат на более предпочтительный приоритет после переключения вниз) — максимум один раз за `failback.interval`, со случайным jitter-сдвигом `±spray` против стадного эффекта. Базовый интервал сохраняется. Гарантия — в любой момент Soul держит ровно один активный стрим к одному Keeper-у; переключения zero-downtime (новый стрим открыт, затем старый закрыт).

YAML-конфиг блока `keeper:` (endpoints, retry, failback), полная спецификация параметров и алгоритм по шагам — в [`docs/soul/connection.md`](soul/connection.md). Расположение этого блока в общем `soul.yml` — в [`docs/soul/config.md`](soul/config.md).

## Push-режим (`keeper.push`)

Управление хостами **без установки Soul-агента**: Keeper по SSH ходит на хост, выполняет шаги Destiny, забирает результаты, ничего постоянного не оставляет кроме изменений, которые Destiny и описывает. Push-режим — это **модуль внутри `keeper`-а** (ADR-004), не отдельный бинарь: серверная функция (RBAC, аудит, выпуск SSH-credentials через Vault) логично сидит в Keeper-е.

Свойства, важные на архитектурном уровне:

- **Единый реестр.** Push-хост — запись в той же таблице `souls` с `transport: ssh`; миграция push↔agent — смена одного поля, история не теряется. SoulSeed-таблица для push-хостов не используется (нет mTLS-идентичности — её роль играет SSH-сторона).
- **Один и тот же `soul`-бинарь.** Тот же артефакт, что и pull-демон, запускается одноразово как `soul apply` (stdin = отрендеренный `ApplyRequest` как protojson — `apply_id` + `RenderedTask[]` после Keeper-side фаз рендера, ADR-012(d); stdout = NDJSON-поток `TaskEvent` + финальный `RunResult` как protojson; exit 0 при `RunResult.status==success`, 1 иначе). Сырой Destiny/Essence на push-хост не попадает — Keeper рендерит у себя, Soul не резолвит Vault.
- **Кеш по SHA-256 на хосте.** Бинарь и модули кешируются в `/var/lib/soul-stack/{bin,modules}/`; повторный прогон ничего не докачивает.
- **SSH-аутентификация — pluggable provider.** Контракт `SshProvider` (Vault SSH CA / static key / Teleport — все вписываются под него), конкретный набор обязательных реализаций — [open Q SSH-2](#текущие).

Полный разбор (модель, миграция push↔agent, аутентификация SSH, интерфейс оператора `POST /v1/push/apply`, алгоритм прогона, раскладка `/var/lib/soul-stack/`, ключевые свойства) — в [`docs/keeper/push.md`](keeper/push.md). Нормативная спецификация HTTP-фасада и request/response schemas — в [`docs/keeper/operator-api.md`](keeper/operator-api.md). Хостовая раскладка и кеш — в [`docs/soul/modules.md`](soul/modules.md). Контракт `SshProvider` и каталог плагинов — в [`docs/keeper/plugins.md`](keeper/plugins.md).

## Модель модулей

Раздел применим к обоим транспортам — **pull** и **push**. Это единая модель: `soul`-бинарь применяет Destiny-шаги, модели исполнения шагов одинаковы независимо от того, как бинарь оказался на хосте.

### Структура

- **Core-модули** — статически встроены в `soul`-бинарь. Покрывают подавляющее большинство Destiny: точный список зафиксирован [ADR-015](#adr-015-core-модули-mvp-точный-список) — 17 Soul-side (`pkg`/`file`/`service`/`user`/`group`/`exec`/`cmd`/`cron`/`mount`/`git`/`archive`/`sysctl`/`url`/`line`/`repo`/`firewall`/`http`) + 3 Keeper-side (`soul.registered`/`cloud.provisioned`/`vault.kv-read`, последние два — [ADR-017](#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)). Работают всегда, везде, не требуют дополнительной доставки. По адресации все встроенные модули живут в namespace `core`. Рендер файлов из шаблонов делает `core.file.rendered` (см. [ADR-010](#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)) — отдельный модуль `core.template` НЕ выделяется.
- **Custom-модули** — отдельные исполняемые файлы `soul-mod-<имя>`, лежат в `/var/lib/soul-stack/modules/`. `soul`-бинарь запускает их как sub-process с stdio-протоколом (см. ниже). По адресации живут в namespace своей коллекции (`wb`, `community`, …).

> **Soul-side vs Keeper-side core-модули.** Подавляющее большинство core-модулей (`pkg`, `file`, `service`, `user`, `exec`, `template`, …) — **Soul-side**: исполняются на хосте `soul`-бинарём. Часть core-модулей — **Keeper-side**: оперируют реестрами keeper-а (Postgres souls+coven, Redis-кэш, журналы) и исполняются на самом keeper-е. Первый Keeper-side core — `core.soul.registered` (привязка SID к coven-меткам реестра souls; полная спецификация — [`docs/keeper/modules.md`](keeper/modules.md)). Диспетчер — scenario-ключ `on:` ([`docs/scenario/orchestration.md §3`](scenario/orchestration.md#3-таргет-шага--on)): для Soul-side core `on:` опущен либо содержит coven-метки; для Keeper-side core `on: keeper`. Адресация (`<namespace>.<module>.<state>`) и контракт SoulModule для обеих сторон один и тот же.

При apply Destiny-шага `soul`:
1. парсит имя модуля по схеме `<namespace>.<module>.<state>` (см. «Адресация модулей»);
2. для встроенного core-модуля вызывает реализацию `<module>.<state>` напрямую, в процессе;
3. иначе ищет файл `soul-mod-<module>` в каталоге модулей коллекции; нет файла — ошибка валидации (`soul-lint` ловит это до прогона);
4. запускает sub-process, передаёт состояние, параметры и Essence через gRPC-stdio, читает события стримом (см. «Протокол модулей»).

### Адресация модулей

Модуль адресуется тремя уровнями, через точку:

```
<namespace>.<module>.<state>
```

| Уровень | Смысл | Примеры |
|---|---|---|
| **namespace** | Коллекция — единица дистрибуции и доверия (см. [module-collections.md](module-collections.md)). Префикс автора/издателя. | `core` (встроены в `soul`), `wb`, `community` |
| **module** | Объект управления — «о чём модуль». Эквивалент Salt-овского state-модуля без функции (`pkg`, `service`). | `pkg`, `file`, `service`, `user`, `exec`, `template`, `haproxy` |
| **state** | Желаемое состояние — «как объект должен быть». Декларативное имя (не императивный глагол). | `installed`, `absent`, `latest`, `present`, `running`, `stopped`, `restarted`, `enabled` |

Адресация **декларативная**: третий уровень — это *желаемое состояние*, не *действие*. `core.pkg.installed` читается как «пакет установлен» — не «установи пакет». Это лучше ложится на философию destiny («какое должно быть»), даёт естественный язык авторам сценариев и совпадает с тем, как делает Salt.

**Каждое состояние — отдельный input schema модуля.** `core.pkg.installed` принимает `name`, `version`; `core.pkg.absent` — только `name`; параметры не пересекаются. Манифест модуля декларирует свой полный список состояний и схему параметров для каждого, см. «Манифест модуля» ниже. Линтер ловит:
- незнакомое состояние модуля → ошибка;
- неверный набор `params:` для конкретного состояния → ошибка;
- расходящиеся типы параметров → ошибка.

**Не stateful модули.** Модули, у которых нет естественного «состояния» (`exec`, `cmd`), используют **verb-форму третьего уровня**: `core.exec.run`, `core.cmd.shell`. Это сознательное исключение из declarative-правила: для оператора, у которого нет состояния, бесполезно изобретать «эту команду я хочу в состоянии запущенности». Манифест модуля помечает state-форму или verb-форму, линтер это проверяет.

**Третий уровень обязателен.** Запись `core.pkg` без `state` — ошибка валидации, без умолчаний. Явное лучше неявного: оператор не должен «угадывать», что подразумевается по умолчанию.

**`required_modules:` в destiny.yml — декларация только custom-модулей** в двухуровневой форме (`<namespace>.<module>`): «эта destiny требует, чтобы на хосте была доступна семья custom-модулей `wb.haproxy`, `wb.myapp`, …». Все state-формы внутри этих модулей доступны автоматически. Конкретные state-инстансы вызываются 3-уровневой формой в `tasks/main.yml` (см. ниже [«Структура задачи в `tasks:`»](#структура-задачи-destiny)).

**Core-модули в `required_modules:` не перечисляются** — они статически встроены в `soul`-бинарь и всегда доступны. Если destiny использует только `core.*` — блок `required_modules:` опускается целиком.

**Пример (`tasks/main.yml` destiny с custom-модулями — top-level список задач, без обёртки):**

```yaml
# destiny-<name>/destiny.yml объявляет required_modules:
#   required_modules: [wb.haproxy, wb.myapp]

# destiny-<name>/tasks/main.yml:
- name: Install redis-server package
  module: core.pkg.installed                # core: в required_modules: не пишем
  params: { name: redis-server, ref: v7.2.4 }

- name: Render /etc/redis/redis.conf from template
  module: core.file.rendered                    # ADR-010: рендер .tmpl делает core.file.rendered
  params:
    path: /etc/redis/redis.conf
    template: templates/redis.conf.tmpl
    vars:
      maxmemory: "${ input.maxmemory }"
    mode: "0640"

- name: Ensure redis-server is running and enabled at boot
  module: core.service.running
  params: { name: redis-server, enabled: true }

- name: Restart redis-server
  module: core.service.restarted
  when: input.action == 'restart'
  params: { name: redis-server }

- name: Reload haproxy after config change
  module: wb.haproxy.reloaded               # custom: объявлен в required_modules:
  params: { config_path: /etc/haproxy/haproxy.cfg }
```

### [ADR-045. Param-DSL модулей — типизированные input-поля для UI-формы Run Command](adr/0045-param-dsl.md)

Вынесен в [`docs/adr/0045-param-dsl.md`](adr/0045-param-dsl.md). Сближает модульную input-DSL (`plugin.InputParamDef` / `shared/coremanifest`) со scenario/destiny `input:` — `enum` / `format: sid` + `source:` / `pattern` / `items` под `list`/`map` — чтобы UI Run Command строил типизированную форму под модуль (core + plugin), не вводя третью DSL.

### Структура задачи destiny

Содержимое destiny живёт в **`destiny-<name>/tasks/main.yml`** как top-level YAML-список задач (без обёртки `tasks:` / `steps:`), а не в самом `destiny.yml`. Корень `destiny.yml` — только манифест (`name`, `description`, `input`, опц. `required_modules`); `tasks/main.yml` — точка входа, с возможностью подключать `include: <file>.yml` соседей внутри той же папки `tasks/`.

Один элемент списка — вызов одного модуля с параметрами и необязательной обвязкой. Поля задачи (`name`, `module`, `params`, `when`, `register`, `output`, `no_log`, `include`), соглашение об именовании задач (заглавная буква, императив, английский) и правила `include:` — закреплены в **[`docs/destiny/tasks.md`](destiny/tasks.md)**. Архитектурный раздел тут не дублирует таблицу полей, чтобы не было drift-а двух источников.

Раскладка папки destiny и формат `destiny.yml` — в [`docs/destiny/manifest.md`](destiny/manifest.md). Полный обзор концепции destiny — в [`docs/destiny/`](destiny/README.md).

### Протокол модулей — gRPC over stdio (HashiCorp-style)

Модель **B (gRPC-stdio)**: тот же приём, что у Terraform providers, Vault plugins, Packer plugins. Нормативный формат handshake-строки, lifecycle и версионирования — [ADR-020](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle); файл спецификации — [`docs/keeper/plugins.md`](keeper/plugins.md). Общий вид:

- При запуске модуль печатает на stdout handshake-строку с версией протокола и адресом локального сокета (Unix socket, например).
- Дальше `soul` и модуль общаются по gRPC через сокет. Сервис на стороне модуля:

  ```protobuf
  service SoulModule {
    rpc Validate(ValidateRequest)       returns (ValidateReply);
    rpc Plan(PlanRequest)               returns (stream PlanEvent);
    rpc Apply(ApplyRequest)             returns (stream ApplyEvent);
  }
  ```

  Манифест плагина — статический `manifest.yaml`, нормативно описан в [ADR-020(a)](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle); RPC `Manifest()` в MVP не вводится.

- gRPC-stream даёт нативный progress-репортинг для длинных операций (`PlanEvent`, `ApplyEvent`).
- Модуль завершается graceful shutdown сигналом по тому же контракту.

### Манифест модуля

Каждый модуль декларирует себя в **статическом `manifest.yaml`** в корне репо плагина и рядом с бинарём в кеше host-а ([ADR-020(a)](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Формат един для трёх kind-ов (`soul_module` / `cloud_driver` / `ssh_provider`) с `kind:`-дискриминатором; нормативный источник — [ADR-020(e)](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle) и [`docs/keeper/plugins.md`](keeper/plugins.md). Пример для `SoulModule`:

```yaml
kind: soul_module                   # дискриминатор (ADR-020(e))
protocol_version: 1                 # версия plugin-протокола (compat-флаг, не версия модуля — см. ADR-007, ADR-020(c))
namespace: wb                       # коллекция
name: haproxy                       # имя модуля внутри коллекции

required_capabilities: [run_as_root]  # closed enum (ADR-020(f))
side_effects:                          # strict-контракт (ADR-020(g))
  - { service: haproxy }
  - { file: /etc/haproxy/haproxy.cfg }

spec:                                  # kind-specific блок для soul_module
  # Список поддерживаемых состояний (или verb-форм для non-stateful модулей).
  # Каждое состояние имеет собственный input schema (DSL — см. раздел
  # «Destiny: входной контракт и валидация»).
  states:
    running:
      input:
        name:    { type: string, required: true }
        enabled: { type: boolean, default: true }
      description: HAProxy запущен и включён в systemd.
    stopped:
      input:
        name: { type: string, required: true }
    restarted:
      input:
        name: { type: string, required: true }
```

Манифест используется `soul-lint`-ом для **локальной** валидации Destiny: ловим неизвестные модули, неизвестное состояние модуля, неправильные параметры для конкретного состояния, capabilities-mismatch с host-policy — без запуска самого модуля ([ADR-009](#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация), [ADR-020](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)).

### Языки и сборка модуля

- **Native (Go, Rust, C/C++)** — рекомендуемый путь. Один статический бинарь, компактный (5–25 MB), без рантайм-зависимостей. Для Go поставляем first-class SDK (`soulstack/sdk-go`) с реализованным handshake-ом и протоколом — автору модуля остаётся написать только бизнес-логику.
- **Python** — поддерживается через bundler (PyInstaller, **рекомендуется Nuitka**). Размер модуля 15–80 MB в зависимости от зависимостей. Тяжёлые зависимости (`pandas`, `numpy`, ML-стек) для модулей конфиг-менеджмента — индикатор переусложнения дизайна.
- **Node.js / Ruby / другие interpreted-языки** — технически работают (gRPC-stdio это чистый протокол), но first-class SDK не поставляется; собирать через pkg / Tebako / аналоги. Файлы крупнее (40–120 MB).

Soul Stack принимает «исполняемый файл, который делает gRPC-stdio handshake». Что под капотом — выбор автора модуля.

### Поведение на хосте и cleanup

Раскладка `/var/lib/soul-stack/{bin,modules}/`, кеш модулей по SHA-256, поведение в pull (демон стягивает custom-модуль через `core.module.installed`) и в push (Keeper передаёт все зарегистрированные модули скопом), локальная чистка кеша по TTL, отдельная операция `keeper.push.cleanup` при revoke хоста — собрано в [`docs/soul/modules.md`](soul/modules.md).

Граница ответственности: Reaper / Жнец на стороне Keeper-а работает **только над Postgres** и на хосты по SSH не ходит — иначе пришлось бы давать ему SSH-права на весь парк (плохо по blast radius). Хостовая чистка — задача Soul-демона (pull) либо самого `keeper.push` (push).

### [ADR-046. Cadence — регулярные запуски (scheduled/recurring Voyage)](adr/0046-cadence.md)

Вынесен в [`docs/adr/0046-cadence.md`](adr/0046-cadence.md). Cadence — первоклассная сущность расписания (таблица `cadences`, модель b — переживает прогоны), которая по времени **спавнит** обычный [Voyage](adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) (back-link `voyages.cadence_id`): правило повторения `interval`|`cron`, три `overlap_policy` (`skip`/`queue`/`parallel`), anchored-пересчёт `next_run_at` + anti-storm missed-slot, двухуровневый RBAC-guard (`cadence.*` + Voyage-permission по `kind`), audit `cadence.*` (`source: background`); исполнитель — [Conductor](adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний) (amendment 2026-06-02); включает floor мин-периода 30s (Pass B).

### [ADR-047. Purview — scoped RBAC-видимость узлов (role default_scope + расширенный селектор)](adr/0047-purview.md)

Вынесен в [`docs/adr/0047-purview.md`](adr/0047-purview.md). Purview — резолвер scoped RBAC-видимости узлов (`ResolveScope`/`ResolvePurview`/`HoldsAction`): измерения coven/regex/soulprint/state, role `default_scope` + per-perm override, default-deny с `*`-исключением, scoped-видимость souls/incarnations-list (S3b) и target ∩ Purview для command-пути Voyage (S4, security-fix); fail-closed-инвариант. Включает Amendment ADR-049 (Synod).

### [ADR-048. Conductor — leader-elected исполнитель Cadence-расписаний](adr/0048-conductor.md)

Вынесен в [`docs/adr/0048-conductor.md`](adr/0048-conductor.md). Conductor — отдельная leader-elected keeper-side подсистема (Redis-lease `conductor:leader`, generic `leaderloop`, свой адаптивный tick-interval `cadence_scheduler`, независимый от `reaper.interval`), исполняющая спавн due-[Cadence](adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)-расписаний; вынос спавна из Reaper-правила `spawn_due_cadence` (Reaper теряет `action: spawn`, возвращается к чистому cleanup-домену), spawn-семантика сохранена дословно, audit-source остаётся `background`, default-ON при наличии Redis; включает amendment «Adaptive interval» (профиль «Спокойный» + floor мин-периода).

### [ADR-049. Synod — группа архонов](adr/0049-synod.md)

Вынесен в [`docs/adr/0049-synod.md`](adr/0049-synod.md). Synod — группа архонов, бандлящая набор ролей (модель **Архон → Synod → Роли**); три PG-таблицы `synods` / `synod_operators` / `synod_roles`, эффективные роли = прямые ∪ через Synod собираются в snapshot-сборке enforcer-а; security-инварианты (least-privilege + self-lockout) разворачивают Synod; permission-семейство `synod.*` (8, вкл. `synod.update`); амендит [ADR-014](adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon) / [ADR-028](adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) / [ADR-047](adr/0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор).

### [ADR-050. Tempo — per-AID rate-limiting write-API](adr/0050-tempo.md)

Вынесен в [`docs/adr/0050-tempo.md`](adr/0050-tempo.md). Tempo — per-AID ограничитель частоты обращений оператора к resolver-тяжёлым write-эндпоинтам (MVP-охват `POST /v1/voyages` + `/v1/voyages/preview`, единый bucket `voyage_create`): token-bucket в Redis (Lua, ключ `tempo:<aid>:<bucket>`), fail-OPEN при Redis-down (осознанный trade-off, как Toll), 429 + `Retry-After` + problem-type `tempo-exceeded`, дефолт `rate: 10 / burst: 20`, hot-reload config-блок `tempo:`; третий anti-DoS-слой рядом с body-limit и [Toll](#adr-038-toll--cluster-wide-detector-массового-оттока-souls).

### [ADR-051. Operator API codegen: OpenAPI → Go-типы (oapi-codegen), types-only → strict](adr/0051-operator-api-codegen.md)

Вынесен в [`docs/adr/0051-operator-api-codegen.md`](adr/0051-operator-api-codegen.md). **SUPERSEDED [ADR-054](#adr-054-operator-api--разворот-на-code-first-go-типы--openapi-через-huma-v2); реализация снесена 2026-06-13 (HEAD `fde65bf`).** Описывал spec-first: источник формы = `openapi.yaml`, Go-типы из неё генерировал oapi-codegen v2 (`types-only`, пакет `keeper/internal/api/oapi`, `types.gen.go`), `proto/operator/v1` упразднён (аменд ADR-011); паттерн type-alias + конвертер-на-границе (категории A–D, byte-passthrough JSONB D), инвариант «ноль wire-change», даунгрейд спеки до `3.0.3`, Фаза 2 (strict) — «мост». **Каркас удалён** (oapi-пакет / `oapi_strict.go` / рукопись-источник / `gen-api`/`check-gen-api`), источник формы теперь — Go-типы huma ([ADR-054](#adr-054-operator-api--разворот-на-code-first-go-типы--openapi-через-huma-v2)).

### [ADR-052. Herald + Tiding — уведомления о событиях прогонов](adr/0052-herald-notifications.md)

Вынесен в [`docs/adr/0052-herald-notifications.md`](adr/0052-herald-notifications.md). **Herald** — канал доставки уведомлений (PG-реестр `heralds`: `type`-enum `webhook` в MVP, `config` JSONB, `secret_ref` vault-ref), **Tiding** — правило подписки (PG-реестр `tidings`: `event_types[]` с area-glob `scenario_run.*`, фильтры `only_failures`/`only_changes`, селекторы `incarnation`/`cadence`, FK на Herald). Scope MVP — ТОЛЬКО события прогонов (`scenario_run.*`/`command_run.*`/`voyage.*`/`incarnation.drift_checked`/`cadence.*`); beacon-события хостов через [Oracle](adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor) — отвергнуто (отдельный заход). Механика — tap (multi-writer декоратор поверх `audit.Writer`, точка [ADR-022(f)](adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) → notification-dispatcher → at-least-once webhook-доставка claim-queue worker-ом (parity [VoyageWorker](adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)), retry+backoff, статусы попыток в Redis (hot→Redis), терминалы `herald.delivered`/`herald.failed` в audit. Security-инварианты: https-only + deny приватных IP (opt-out `allow_private`, паттерн `core.url`) + запрет redirect (`util.CheckRedirect`) + timeout; `secret_ref` только vault-ref; payload без resolved-секретов + `MaskSecrets`. Контракты additive: OpenAPI `/v1/heralds`+`/v1/tidings` (greenfield full-strict, [ADR-051 S6](adr/0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-типы-oapi-codegen-types-only--strict)), RBAC `herald.*`/`tiding.*`, MCP `keeper.herald.*`/`keeper.tiding.*`, UI-вкладка «Notifications».

### [ADR-053. Tier-ы инфраструктурных зависимостей](adr/0053-dependency-tiers.md)

Вынесен в [`docs/adr/0053-dependency-tiers.md`](adr/0053-dependency-tiers.md). Классификационный ADR: обязательный контур Keeper-кластера — **три компонента PostgreSQL + Redis + Vault**, все три **fail-fast** на старте. Vault hard-required в трёх точках: vault-клиент на старте (`setupVault` → `NewClient` → `Ping`), JWT signing-key (auth операторов, [ADR-014](adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)), souls-PKI (выпуск/ротация SoulSeed mTLS через Vault PKI). OPTIONAL-with-degradation (фича выключается внятно, Keeper не падает): Sigil signing-key (fail-closed), Augur (default-deny), Herald `secret_ref` (без подписи), push host-CA (push выключен), metrics basic-auth, OTel-экспорт. Правило для НОВЫХ фич: новые обязательные зависимости — только через решение пользователя; опциональные обязаны деградировать внятно. Отвергнуто: без-Vault режим (file/env auth-ключ + встроенный CA — ломает security-предпосылку «секреты на диск Keeper-кластера не материализуются»; CA-приватник на диске/в PG, в multi-keeper разнос приватника по нодам; отвергнут пользователем) и SecretProvider-абстракция (преждевременна). Operations-нота: обязательный Vault ≠ тяжёлый кластер — single-binary с file-storage сопоставим с Redis (рецепт в [infra.md](adr/../operations/infra.md#лёгкий-vault-для-малых-инсталляций)).

### [ADR-054. Operator API — разворот на code-first (Go-типы → OpenAPI) через huma v2](adr/0054-openapi-code-first.md)

Вынесен в [`docs/adr/0054-openapi-code-first.md`](adr/0054-openapi-code-first.md). Заменяет [ADR-051](#adr-051-operator-api-codegen-openapi--go-типы-oapi-codegen-types-only--strict): источник формы Operator API инвертирован на **Go-типы → OpenAPI** (code-first, huma v2 + humachi), спека производна. **FULL-TYPED** handler-ы (typed `Body`/output + извлечённая доменная `XTyped`); валидация `required`/`enum`/`unknown→400` — нативно huma из struct-тегов; security сохранён (ручной mount в chi-группу с RBAC/Audit-навеской, глобальный problem+json-override, per-домен audit-guard; middleware-audit домены — вариант B `UseMiddleware`). Tier-ы pattern: full-typed write, PATCH-presence через `Optional[T]`, read-with-typed-query (`400/422`-контракт сохранён). **Реализация ЗАВЕРШЕНА 2026-06-13 (HEAD `fde65bf`)**: все ~19 доменов handler-native, served-spec = runtime huma-dump (`HumaFullSpecYAML`, агрегатор `huma_full_spec.go`), committed `docs/keeper/openapi.yaml` — производный (`make gen-openapi` / drift-guard `make check-openapi`), native enum-каталог `huma_enums.go`; oapi-codegen-каркас и рукопись-источник снесены. **Amendment 2026-06-15:** над производной спекой поднят визуальный OpenAPI-вьювер `GET /docs` (Stoplight Elements, go:embed-ассеты, механизм A — публичный shell-каркас, спека за JWT), а `GET /openapi.yaml` сменил security публичный → за JWT (нет анонимной разведки API-поверхности; UI-vendor/`soulctl` берут committed-файл). Meta-роуты — [operator-api.md → Health / Meta / Docs](keeper/operator-api.md#health--meta--docs).

### [ADR-055. Embed UI bundle — опциональный single-binary keeper с UI на `/ui`](adr/0055-embed-ui-bundle.md)

Вынесен в [`docs/adr/0055-embed-ui-bundle.md`](adr/0055-embed-ui-bundle.md). **Amends [ADR-035](#adr-035-distribution-split--core-apicli-vs-web-ui):** активирует отложенный embed-compat-shim как опциональный **default-ON** embed UI для беты (single-binary onboarding), НЕ разворот разделения дистрибуции. Keeper через `go:embed` несёт завендоренный build-снапшот UI (`soul-stack-web`) в `keeper/internal/webui/assets/` (папка `assets/`, НЕ `dist/` — gitignore-правило `dist/` молча съело бы бандл) и раздаёт его на маршруте `/ui` + `/ui/*` с SPA-fallback на `index.html`. Статика **публична** (parity `/docs`; `/v1` остаётся за JWT+RBAC+default-deny), тоггл `web_ui_enabled` (`*bool`, `nil`→default-ON, явный `false`→opt-out), делит listener `:8080` (новых портов/systemd НЕТ), прирост бинаря ~+2–5 МБ. Source-of-truth UI = companion-репо `soul-stack-web`; в core — только собранный артефакт, синк `scripts/sync-webui.sh` + drift-guard `make check-webui` (skip без companion, калька plugin-template). Уточнение инварианта ADR-035 «никаких HTML/CSS/TS»: **исходники** UI по-прежнему запрещены, **собранный статический артефакт** допускается (как RapiDoc-бандл `/docs`). Прецедент go:embed-статики — [ADR-054](#adr-054-operator-api--разворот-на-code-first-go-типы--openapi-через-huma-v2).

### [ADR-056. Staged-render — прогон сценария как N упорядоченных Passage](adr/0056-staged-render-passage.md)

Вынесен в [`docs/adr/0056-staged-render-passage.md`](adr/0056-staged-render-passage.md). Прогон сценария исполняется как **N упорядоченных Passage** (фаза прогона = render → dispatch → barrier → сбор register). Реализует обещанный каноном [orchestration.md §4/§5](scenario/orchestration.md#4-волатильный-предикат--where) probe→where: задача, читающая `register.X` (в `where:`/`apply: input:`/`params:`/`vars:`), стратифицируется в Passage **после** probe, эмитящего `X` (топологический N-stage); render следующего Passage подставляет per-host register предыдущих. Закрывает doc-drift «keeper рендерит один up-front проход ДО probe → `where:` видит пустой register». **`incarnation.state` коммитится ОДИН раз после последнего Passage** — barrier/state-commit-инвариант §7 не дробится; Passage — ось задач, не ось коммита. Reuse: `loadRegisterByHost`/`apply_task_register`/`SelectTaskRegistersByApplyID` (post-barrier register) как Passage-loop; `evalWhere`/`renderParams` уже принимают `in.Register`. Контракт: proto only-add `passage` в `ApplyRequest`/`TaskEvent`/`RunResult`; PG-PK per-passage (вариант фиксируется в S1); N `RunResult` на `(apply_id, sid, passage)`; старый Soul под staged-сценарием → explicit-reject (`render_failed`, не зависание). **Amends [ADR-009](#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)** (per-task `serial:`/§8 → per-task dispatch через Passage), **[ADR-012](#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)** (dispatch «один ApplyRequest на хост» → «N на хост по Passage»), **[ADR-027](#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)** (reclaim/Ward-claim гранулярность → per-passage). Закрывает [open Q №24](#открытые-вопросы) (per-task гранулярность `serial:`). Слайс-карта S0–S5.

### [ADR-057. `state_changes` — упорядоченный список CRUD-глаголов](adr/0057-state-changes-crud-verbs.md)

Вынесен в [`docs/adr/0057-state-changes-crud-verbs.md`](adr/0057-state-changes-crud-verbs.md). `state_changes` сценария становится **упорядоченным списком операций** (YAML-список, не map). Каждый элемент — один **CRUD-глагол** (сингуляр): **`set`** (перезапись поля целиком, заменяет прежний `sets`-map), **`add`** (добавить элемент в коллекцию: map — `key:`+`value:`, list — `value:`+опц. `match:`; `on_conflict: skip|replace|error`, default `skip` = идемпотентно «добавить если нет»), **`modify`** (`match:` + `patch:` — патч ВСЕХ подходящих, all-by-default), **`remove`** (`match:` — удалить ВСЕХ подходящих), **`foreach`/`as`/`do`** (bulk fan-out, форма буквально из migration-DSL [ADR-019](#adr-019-state_schema-migration-dsl)). **Множественность выражается `match`-предикатом** (CEL над элементом), не ручками/флагами; CEL-биндинги `elem` (элемент list/скаляр), `key`/`value` (запись map) поверх полного sets-контекста (`input`/`incarnation`/`soulprint.self`/`register`/`vars`/`essence`). Опц. `expect: one|at_most_one|any` (default `any`) — runtime-ассерт кратности `modify`/`remove` (зацепил ≠ ожидаемого → `error_locked` до коммита). Предохранители: soul-lint WARN на константно-истинный/отсутствующий `match`; empty-match → no-op (идемпотентность). Операции применяются в порядке объявления к промежуточному state, одна PG-транзакция, один `state_history`-snapshot, фейл любой → `error_locked` (barrier/state-commit-инвариант [§7](scenario/orchestration.md#7-инвариант-barrier--state-commit) не ослаблен). Тип коллекции — из `state_schema`; семантика per-RUN (last-wins по SID, НЕ per-host union). Чинит **латентный баг**: `appends`/`modifies` ([ADR-009 §7.1](#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)) были no-op-плейсхолдерами без источника значения — `incarnation.state` не рос (`add_replica`/`add_user`/`update_acl`). **`remove` (state_changes) ≠ `delete` (migration-DSL)** — намеренно разные имена (вынуть элемент коллекции vs снести путь схемы). Не вводятся: `clear`/`rename`/`move`/`upsert`/позиционный remove/парные `*_one`/`*_all`/флаг `all:`. Transit (breaking): map-форма (`sets`/`appends`/`modifies`) парсится один релиз как DEPRECATED (dual-parse + soul-lint warn), затем удаляется. **Amends [ADR-009](#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)** (грамматика §7.1). Нормативная спека — [`docs/scenario/orchestration.md §7.1`](scenario/orchestration.md#71-грамматика-state_changes--список-crud-операций).

### [ADR-058. Федеративная аутентификация операторов (Archon) — LDAP + OAuth2/OIDC](adr/0058-operator-auth-ldap-oidc.md)

Вынесен в [`docs/adr/0058-operator-auth-ldap-oidc.md`](adr/0058-operator-auth-ldap-oidc.md). Federated-login операторов: внешний IdP **валидируется на Keeper-е** (LDAP search-bind `go-ldap/v3` / OIDC authorization-code flow с обязательным **PKCE (S256)**, discovery+JWKS-валидация `id_token` через `go-oidc/v3`), затем identity **маппится** на реестр `operators` (AID) + RBAC-роли из групп, после чего выпускается **внутренний JWT** существующим `jwt.Issuer` (ADR-014) в HttpOnly+Secure+SameSite=Strict cookie `soul_session`. Auth-middleware/RBAC/MCP/OpenAPI остаются JWT-based и **не меняются**. Публичные эндпоинты вне `/v1`: `POST /auth/ldap/login`, `GET /auth/oidc/{login,callback}`; OIDC flow-state — cluster-shared на Redis (state→nonce/PKCE-verifier, single-use GETDEL, TTL 5m). Provisioning-policy `provisioning_allowed_methods` + `/v1/provisioning-policy` (perm `provisioning.read`/`provisioning.update`, problem `provisioning-method-disabled`); auto-provision пишет строку `operators` (`created_via='ldap'`\|`'oidc'`, `created_by_aid=NULL`). Расширение `auth_method` enum (`ldap`/`oidc`, only-add, миграция 083); поле `operators.created_via` + релакс bootstrap-индекса на `WHERE created_via='bootstrap'` (миграции 084/085) + посев `archon-system` (086); конфиг-блоки `auth.ldap`/`auth.oidc`/`auth.rate_limit` (секреты — Vault `*_ref`, TLS-required). Anti-bruteforce login-эндпоинтов — LoginGuard (per-IP/per-username throttle+lockout поверх Redis, fail-closed, problem `auth-throttled`). **Amends [ADR-014](#adr-014-identity-модель-оператора-archon)** (`auth_method`/`created_via`), **[ADR-013](#adr-013-bootstrap-первого-архонта)** (bootstrap-инвариант на `created_via`), **[ADR-029](#adr-029-реестр-service-ов--postgres)**.

## Plugin-инфраструктура

В Soul Stack три категории расширений: **модули Destiny**, **cloud-провайдеры** и **SSH-провайдеры для push-режима**. Все три используют **единую plugin-инфраструктуру** — один и тот же handshake-механизм, протокол, requirements к артефакту. Меняется только service-контракт (gRPC-сервис), который плагин реализует.

Нормативная фиксация формата manifest, handshake-строки, lifecycle, capabilities и side_effects — [ADR-020](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle). Полная спека для авторов плагинов и host-стороны — [`docs/keeper/plugins.md`](keeper/plugins.md) (manifest формат един для всех трёх kind-ов; `SoulModule`-специфика — [`docs/soul/modules.md`](soul/modules.md)).

### Общий механизм

- Плагин — отдельный исполняемый файл, поставляемый как самостоятельный артефакт (свой git-репо, свой релизный pipeline, свои версии).
- Запускается host-процессом (`soul`-бинарь или `keeper`) как sub-process (one-shot, [ADR-020(d)](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)).
- При старте печатает в stdout **handshake-строку** — JSON в одну строку с магическим полем `"soul_stack":"plugin-v1"` ([ADR-020(b)](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)).
- Дальше host и плагин общаются по **gRPC через Unix domain socket** в host-managed директории (`/var/run/soul-stack/plugins/` или `/var/run/soul-stack-keeper/plugins/`, mode `0700`).
- Плагин завершается graceful shutdown по SIGTERM (grace 10s → SIGKILL).
- Заимствуется модель «one-line handshake → gRPC-over-socket» от `hashicorp/go-plugin`, но **не** их код/формат/MPL-2.0-лицензия ([ADR-020(b)](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle), [ADR-016](#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack)).

### Три service-контракта

| Контракт | Кто host | Кто плагин | Назначение |
|---|---|---|---|
| **`SoulModule`** | `soul`-бинарь | `soul-mod-<name>` | Реализует шаги Destiny: `Validate` / `Plan` / `Apply` (см. «Модель модулей»). |
| **`CloudDriver`** | `keeper` | `soul-cloud-<provider>` | Создаёт/удаляет/опрашивает VM в облаке: `Schema` / `Validate` / `Create` / `Destroy` / `Status` / `List`. |
| **`SshProvider`** | `keeper` | `soul-ssh-<provider>` | Поставляет SSH-credentials для `keeper.push`: `Sign` / `Authorize` (Vault SSH CA, static-key, Teleport — все вписываются под этот контракт). |

### Преимущества единой инфраструктуры

- Один SDK на язык (Go / Rust / Python) покрывает все три типа плагинов.
- Один способ распространения и кеширования (артефакт-store + кеш в master по SHA-256).
- Один способ конфигурирования (manifest + JSON Schema параметров).
- Третьи стороны могут выпускать свои плагины (cloud-провайдер для нишевого облака, custom-модуль конкретной компании) без модификации ядра Soul Stack.

### Каталог плагинов в keeper-конфиге

Реестры плагинов (`modules`, `cloud_drivers`, `ssh_providers`) живут в `keeper.yml` — Keeper при старте резолвит источники, делает checkout по `ref:`, тянет бинари в artifact-cache. Версия плагина — всегда git ref (tag или branch), без semver-range, см. [ADR-007](#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте).

Формат блока `plugins:` со всеми ключами — в [`docs/keeper/config.md`](keeper/config.md). Контракты `CloudDriver` и `SshProvider` со стороны Keeper-а — в [`docs/keeper/plugins.md`](keeper/plugins.md). `SoulModule` (host = `soul`) — в [`docs/soul/modules.md`](soul/modules.md).

## Артефакты Soul Stack: что в git, что в БД

Чёткая граница между **кодом** (статика, версионируется git-tag-ами, ревьюится через PR) и **runtime-state** (конкретные инстансы, мутации через API/MCP, source of truth — Postgres):

| Артефакт | Тип | Где живёт | Управляется как |
|---|---|---|---|
| **Service** | Definition (тип сервиса) | git, отдельный репо на сервис | git tag → реестр в master |
| **Destiny** | Definition (атомарный кирпичик) | git, отдельный репо на destiny | git tag → транзитивно через service.yml |
| **Module** (`soul-mod-*`, `soul-cloud-*`, `soul-ssh-*`) | Definition + бинарь | git исходники + artifact-cache в master | релиз в git → master тянет бинарь |
| **Profile** | Runtime config | **Postgres** | API/MCP CRUD |
| **Provider** | Runtime config | **Postgres** | API/MCP CRUD |
| **Coven** | Runtime state | **Postgres** | API/MCP, либо синхронизируется из incarnation |
| **Incarnation** | Runtime state (spec + state + status) | **Postgres** | API/MCP CRUD |
| **Soul** | Runtime state | **Postgres** | bootstrap через CSR, lifecycle через scenario |

Git хорош для кода: версионирование, ревью, история, ветвление. Плох для runtime-state: drift между git и фактом, синхронизация поллингом, сложно делать atomic mutations. Поэтому всё, что **меняется во время эксплуатации**, живёт в Postgres и управляется через API. Git остаётся для **кода и определений**.

Опционально: оператор может экспортировать incarnation в YAML и закоммитить в git **как backup или audit-snapshot**, но это не primary path и не обязательная фича.

## Destiny: входной контракт и валидация

Краткая сводка для архитектурного contex-та. Полный разбор — в **[`docs/destiny/`](destiny/README.md)** (формат `destiny.yml` и `tasks/main.yml`, поля задачи, `input:`-контракт, тестирование).

- **Формат блока `input:`** — общий стандарт **[`docs/input.md`](input.md)**, применяется одинаково к destiny, scenario и манифесту модуля. Любое расширение DSL — propose-and-wait → правка [`input.md`](input.md) → потом всё остальное.
- **Destiny-специфика блока `input:`** (где валидируется, как доступен в шаблонах, отличие `input.<name>` от `params:` модуля) — в **[`docs/destiny/input.md`](destiny/input.md)**.
- **Два раунда валидации.** Keeper при инвокации сценария (fail fast, нулевой трафик к хостам) + Soul перед apply (defense in depth против рассинхронизации). Подробности и роль `soul-lint` — в [`docs/destiny/input.md`](destiny/input.md).
- **`soul-lint`** проверяет статически: well-formed `input:`-блок, литералы в `when:` против `enum:`. Конкретных runtime-значений не видит, в runtime-путь Keeper-а не завязан ([ADR-004](#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)).
- **Мини-open Q:** имя одноимённого блока `params:` на call-site сценария (`apply: { destiny: …, params: { … } }`) и `input:` destiny. Согласовать ли их — отдельный пункт.

## Service — структура и manifest

Service — это **тип сервиса** (Redis HA, PostgreSQL, Vector-collector). Один service — один git-репо, со своими версиями, с обязательным manifest и набором сценариев.

### Раскладка репозитория

```
redis-cluster/
├── service.yml                         # манифест: name, state/host schemas, destiny, modules (версия = git tag, см. ADR-007)
├── essence/                            # параметры в иерархии (см. «Essence: pipeline сборки»)
│   ├── _stack.yaml                     # ОПЦИОНАЛЬНО: декларативный pipeline сборки
│   ├── _default.yaml                   # baseline для всех incarnation
│   ├── coven/
│   │   ├── prod.yaml
│   │   └── dev.yaml
│   └── os/
│       ├── ubuntu.yaml
│       └── debian.yaml
├── scenario/                           # auto-discover из директории, имя папки = имя сценария
│   ├── create/
│   │   ├── main.yml                    # точка входа: input + state_changes + tasks (всё inline)
│   │   ├── install.yml                 # переиспользуемые блоки (include из main.yml)
│   │   ├── templates/                  # ОПЦ.: scenario-локальные шаблоны (двухуровневый резолв)
│   │   ├── vars.yml                    # ОПЦ.: scenario-локалы
│   │   ├── tests/                      # ОПЦ.: тесты сценария (см. scenario/orchestration.md)
│   │   └── replication.yml
│   ├── add_user/
│   │   └── main.yml
│   ├── update_acl/
│   │   └── main.yml
│   ├── add_replica/
│   │   └── main.yml
│   └── restart/
│       └── main.yml
├── schemas/                            # ОПЦИОНАЛЬНО: переиспользуемые JSON Schema-файлы
│   └── user.yaml
├── migrations/                         # миграции state_schema между версиями
│   ├── 001_to_002.yml
│   └── 002_to_003.yml
└── tests/
    ├── smoke.yml
    └── chaos.yml
```

Каждая папка `scenario/<name>/` — отдельная операция (CRUD-style) над сервисом. `main.yml` — точка входа сценария: содержит **inline** `input`, `state_changes` и `tasks`. Соседние `*.yml` — sub-tasks, инклудятся через `include:` в `main.yml`. Перечислять сценарии в `service.yml` не нужно — keeper находит их auto-discover-ом по структуре каталога.

### `service.yml` — манифест

```yaml
name: redis-cluster
state_schema_version: 2               # версия структуры incarnation.state, не версия сервиса (см. ADR-007)

# Структура incarnation.state в БД
state_schema:
  type: object
  required: [redis_version, redis_users, redis_config, redis_hosts]
  properties:
    redis_version: { type: string }
    redis_users:
      type: object
      additionalProperties:
        type: object
        required: [acl, state]
        properties:
          acl:   { type: string }
          state: { type: string, enum: [on, off] }
    redis_config:
      type: object
      additionalProperties: { type: string }
    redis_hosts:
      type: array
      items:
        type: object
        properties:
          sid:  { type: string }
          role: { type: string, enum: [primary, replica] }

# Артефакты-зависимости — ref: git tag или branch (см. ADR-007).
# Никаких semver-range — точный ref и ничего больше.
destiny:
  - { name: redis,                    ref: v2.0.0 }
  - { name: redis-replication-config, ref: v1.0.0 }
  # cloud-create — НЕ destiny-зависимость: это шаг scenario `core.cloud.provisioned`
  # (on: keeper, CloudDriver-плагин), см. ADR-017.

modules:                              # custom-модули
  - { name: redis-failover, ref: v1.2.0 }

# ОПЦИОНАЛЬНО: политика жизненного цикла инкарнаций (отсутствие блока = оба true).
lifecycle:
  auto_create: true                   # POST /v1/incarnations сразу запускает scenario create
  auto_destroy: true                  # удаление запускает teardown-сценарий destroy (по allow_destroy)
```

Сценарии и их детали в service.yml не упоминаются — keeper находит их по содержимому каталога `scenario/`.

`lifecycle:` — опциональный блок политики жизненного цикла инкарнаций сервиса:

- `auto_create: bool` (default `true`) — `POST /v1/incarnations` автоматически запускает scenario `create`; `false` — инкарнация создаётся в `ready` без прогона, оператор запускает `create` вручную из Run-формы.
- `auto_destroy: bool` (default `true`) — удаление инкарнации запускает teardown-сценарий `destroy` по обычной логике `allow_destroy`; `false` — удаление всегда прямое, без teardown, приоритет над `allow_destroy`.

Отсутствие блока = оба `true` (backcompat).

**Сценарии-конвенции и lifecycle-набор.** Lifecycle-набор (`LifecycleScenarioNames`) = только `create` / `destroy` — специализированные scenario-kind-ы соответствующих фаз жизненного цикла. `converge` — **operational** scenario-kind (запускаемый обычным `run`-ом Apply-reconcile + dry-run target `check-drift`), он выведен из lifecycle-набора (см. [Amendment ADR-031 2026-06-10](#adr-031-scry--drift-detection-declarative-dry-run-reconcile)). Каталог сценариев (`GET /v1/services/{name}/scenarios`) несёт поле `runnable: bool` — признак «запускаем оператором из Run-формы»: `create` = `true`, `destroy` = `false` (спец-флоу удаления через `DELETE /v1/incarnations/{name}`), operational (вкл. `converge`) = `true`. UI фильтрует Run-форму по `runnable`, не по хардкоду имён ([ADR-042](#adr-042-backend-driven-dynamic-data-в-ui--ui-не-хардкодит-динамические-каталоги)).

> Поле `version:` верхнего уровня в `service.yml` намеренно отсутствует: версия сервиса — это git-tag, под которым закоммичен сам файл (см. [ADR-007](#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). `state_schema_version` — это **другая** концепция (версия структуры данных state), нужна для миграций, см. ниже раздел [«Versioning и миграции state_schema»](#versioning-и-миграции-state_schema).

### `scenario/<name>/main.yml` — самодостаточная операция

Полная нормативная спецификация оркестрационного слоя (`on:`/`where:`, probe-идиома, двухуровневый резолв ресурсов, тесты, barrier/state-commit) — [`docs/scenario/`](scenario/README.md). DSL-ядро задач (`module:`, `include:`, `block:`, `parallel:`, `loop:`, `register:`, requisites, `retry:`, `timeout:`, `changed_when:`/`failed_when:`) scenario наследует целиком из [`docs/destiny/tasks.md`](destiny/tasks.md) — после [ADR-009](#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) инвариант «scenario только `apply:`» снят. Ниже — иллюстрация формата.

```yaml
name: create
description: Initial bootstrap of Redis HA cluster

# Типизированный вход сценария — валидируется до прогона.
# Формат блока — стандарт docs/input.md.
input:
  redis_version:
    type: string
    pattern: "^[0-9]+\\.[0-9]+\\.[0-9]+$"
  redis_users:
    type: object
    required: true
    additional_properties:
      type: object
      properties:
        acl:   { type: string }
        state: { type: string, enum: [on, off] }
      required: [acl, state]
  redis_password:
    type: string
    required: true
    secret: true
    pattern: "^vault:.*"              # обязательно ссылка на Vault
  spawn:                              # опционально: для cloud-create
    type: object
    properties:
      provider: { type: string }
      profile:  { type: string }
      count:    { type: integer, min: 3, max: 6 }

# Что сценарий пишет в incarnation.state после успешного apply.
# state_changes — упорядоченный список CRUD-глаголов (ADR-057): set/add/modify/
# remove + foreach. set — перезапись поля целиком; значение из CEL ${ … }
# (рендерится Keeper-side, scenario/orchestration.md §7.1). Контекст:
# input/incarnation/soulprint.self/register/vars/essence.
state_changes:
  - set: redis_version
    value: "${ input.redis_version }"
  - set: redis_users
    value: "${ input.users }"
  - set: redis_config
    value: "${ input.config }"
  - set: redis_hosts
    value: "${ input.hosts }"

# Шаги — каждый знает, где исполняется (on: keeper / on: [coven,…] / on: опущен)
tasks:
  - name: provision
    on: keeper                        # на самом keeper-е, через CloudDriver
    when: input.spawn != null
    module: core.cloud.provisioned    # keeper-side core (ADR-017)
    state: created
    params:
      provider: "${ input.spawn.provider }"
      profile:  "${ input.spawn.profile }"
      count:    "${ input.spawn.count }"

  - name: install-redis
    on: ["${ incarnation.name }"]     # весь incarnation (можно было опустить on:)
    apply:
      destiny: redis
      input:
        version:  "${ essence.redis_version }"
        password: "${ input.redis_password }"

  - include: replication.yml
```

Ключ `on:` решает, где выполняется шаг: `keeper` — локально на keeper-е, `[coven, …]` — пересечение covens (⊆ incarnation), опущен — весь incarnation. Волатильный per-host фильтр по `register:` предыдущего probe — ключ `where:` ([ADR-008](#adr-008-coven--только-стабильные-логические-теги)). Один сценарий смешивает keeper- и хост-шаги в линейном flow, в одном языке задач ([ADR-009](#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). Это аналог Salt Orchestration + State, но без двух разных языков и без двух мест. Нормативная семантика — [`docs/scenario/orchestration.md`](scenario/orchestration.md).

Блок `input:` валидирует входные параметры сценария до прогона (по стандарту [docs/input.md](input.md)). `state_changes` — **упорядоченный список CRUD-глаголов** (`set`/`add`/`modify`/`remove` + `foreach`, [ADR-057](#adr-057-state_changes--упорядоченный-список-crud-глаголов)): декларирует, **что** сценарий пишет в `incarnation.state` при успехе и **откуда** берётся значение (CEL `${ … }`, рендерится keeper-ом после барьера); множественность — через `match`-предикат. Нормативно — [scenario/orchestration.md §7.1](scenario/orchestration.md#71-грамматика-state_changes--список-crud-операций).

**Опциональный `$ref`** — для очень больших или переиспользуемых схем:

```yaml
input:
  $ref: "../../schemas/user.yaml"
```

Папка `schemas/` появляется в репо только при реальной потребности — если схема в одном сценарии, держим её inline.

## Essence: pipeline сборки

Параметры сервиса не плоский список, а **иерархическая сборка** в духе Salt `top.sls` / PillarStack: на каждом шаге доступны уже накопленные данные, следующий шаг может на них опираться. Это даёт условные включения, итерацию и динамические значения.

### Convention-based default (без `_stack.yaml`)

Если оператор `_stack.yaml` не пишет, keeper применяет порядок по умолчанию:

1. `_default.yaml` — baseline.
2. `os/<soulprint.os.family>.yaml` — семейство ОС (если файл есть).
3. Для каждой Coven-метки текущего хоста: `coven/<метка>.yaml` (если файл есть).
4. Поверх всего — `incarnation.spec` от оператора (через API).

Этого хватает для большинства сервисов.

> **Essence role-agnostic** ([ADR-008](#adr-008-coven--только-стабильные-логические-теги)). Ступени `role/<host.role>.yaml` в pipeline-е **нет** — essence не слоится по роли. Роль-зависимые параметры переезжают в destiny и передаются через `input:` по probe-роли (см. [`docs/scenario/concept.md`](scenario/concept.md)). Порядок сборки — `default → os → coven → incarnation.spec`.

### `_stack.yaml` — декларативный pipeline

Когда нужны условия, итерация или вычисляемые значения — оператор пишет `_stack.yaml`:

```yaml
# essence/_stack.yaml
#
# На каждом шаге доступны:
#   soulprint   — факты хоста (os, kernel, network, custom-факты)
#   incarnation — { name, service, scenario, ... }
#   host        — { sid, covens } для текущего хоста (роль здесь НЕ доступна — ADR-008)
#   vars        — уже накопленный essence из предыдущих шагов

stack:
  # 1. Baseline — всегда
  - file: _default.yaml

  # 2. Семейство ОС
  - file: "os/${ soulprint.self.os.family }.yaml"
    optional: true                     # пропустить молча, если файла нет

  # 3. Итерация по всем coven-меткам хоста
  - foreach: "${ host.covens }"
    as: coven_name
    file: "coven/${ coven_name }.yaml"
    optional: true

  # 4. Зависит от УЖЕ собранного значения (PillarStack-style)
  - file: "env/${ vars.env }.yaml"
    when: vars.env != null
    optional: true

  # 5. Условный inline-блок — без отдельного файла
  - inline:
      redis_maxmemory: "${ int(soulprint.self.memory.total_mb * 0.6) }mb"
    when: vars.redis_maxmemory == null
```

### Операторы pipeline

| Оператор | Назначение |
|---|---|
| `file: <path>` | Включить файл; путь — шаблон с переменными. |
| `inline: <map>` | Включить набор переменных без отдельного файла. |
| `when: <expr>` | Условие включения (булево выражение). |
| `optional: true` | Не падать, если файл по `file:` не существует. |
| `foreach: <list>` + `as: <name>` | Итерация: шаг повторяется для каждого элемента, переменная `<name>` доступна внутри. |

Между шагами keeper делает re-evaluate доступных переменных, поэтому в более позднем шаге можно ссылаться на `vars.X`, попавший из файла, включённого ранее.

### Финальный merge с incarnation.spec

После прохождения pipeline-а keeper делает финальный deep-merge: `effective_essence = merge(stack_result, incarnation.spec)`. Spec оператора — самый сильный override (он перебивает всё, что собрала иерархия).

В БД хранится **исходный `incarnation.spec`** (не merged) — для аудита видно, что именно оператор переопределил.

## Incarnation — runtime-инстанс сервиса

Incarnation — конкретный **экземпляр** сервиса в реальности (один Redis-кластер, один PostgreSQL-кластер). Хранится в Postgres, управляется через API/MCP.

### Структура записи

| Поле | Тип | Смысл |
|---|---|---|
| `incarnation_id` | UUID | первичный ключ |
| `name` | text UNIQUE | имя incarnation, оно же корневая Coven-метка |
| `service` | text | имя service-а |
| `service_version` | text | пин-версия (git-tag) service-а, под которой работает incarnation |
| `state_schema_version` | integer | версия state_schema, под которой структурирован state |
| `spec` | jsonb | то, что задекларировал оператор (input для последнего успешного create/update) |
| `state` | jsonb | текущая структурированная конфигурация, по `state_schema` сервиса |
| `status` | enum | `provisioning` / `ready` / `applying` / `error_locked` / `migration_failed` / `drift` / `destroying` / `destroy_failed` |
| `status_details` | jsonb NULL | детали ошибки, если `status` локирующий |
| `created_by_aid` | text FK на `operators(aid)` | кто создал |
| `created_at`, `updated_at` | timestamptz | аудит |

### `state_history` — журнал изменений state

Отдельная таблица, snapshot на каждое успешное изменение state:

| Поле | Тип | Смысл |
|---|---|---|
| `history_id` | UUID PK | |
| `incarnation_id` | UUID FK | |
| `scenario` | text | какой сценарий привёл к изменению |
| `state_before` | jsonb | состояние до |
| `state_after` | jsonb | состояние после |
| `changed_by_aid` | text FK на `operators(aid)` | кто инициировал |
| `at` | timestamptz | когда |

Это даёт: откат на любое предыдущее состояние, аудит «кто и когда добавил юзера X», compliance-отчёты.

### Атомарность и `error_locked`

**Сценарий не пишет в БД, пока не отработал на всех целевых хостах**. Если apply упал частично (например, `add_user` прошёл на 2 из 3 хостов), state в БД **не обновляется**, incarnation переходит в `status: error_locked` с `status_details: { failed_hosts: [...], partial_changes: {...} }`. Любой следующий сценарий для этого incarnation отклоняется до явного разрешения от оператора.

Разрешение:
- `keeper.incarnation.unlock name=X reason="manual cleanup verified"` — оператор берёт на себя, что он проверил факт на хостах.
- Специальный recovery-сценарий — service может объявить его, чтобы автоматизировать типовые случаи (дочистить, откатить, переслать).
- `keeper.incarnation.rerun-create name=X reason="..."` (REST: `POST /v1/incarnations/{name}/rerun-create`, permission `incarnation.create-rerun`) — атомарно снимает `error_locked` (state не трогается, last-known-good, snapshot в `state_history`) и тем же действием перезапускает scenario `create`. Под одним `FOR UPDATE`: переход `error_locked → applying` минуя `ready` — исключает окно, в котором конкурентный прогон проскочил бы в освободившийся `ready`. Перезапуск `create` стартует в режиме «по зарезервированному `applying`» (`RunSpec.FromLocked`): runner НЕ транзитит статус повторно, а верифицирует, что строка именно в `applying` — fail-closed, иной статус отклоняет старт, не применяя `create` поверх перехваченной recovery строки. Инвариант «оператор явно подтвердил» сохраняется обязательным `reason` + confirm в UI; audit-событие `incarnation.create_rerun` (НЕ переиспользует `incarnation.unlocked`). Scope ЖЁСТКО ограничен сценарием `create` (rerun bootstrap-а); для прочих случаев — обычный `unlock` + ручной повторный run.

Это поведение, которого **нет в Ansible**: там частичные применения тихо проходят, drift копится. У нас — фейл-фаст с явной точкой ответственности оператора.

### Статусы `destroying` и `destroy_failed`

Teardown incarnation (`keeper.incarnation.destroy`) проходит через два статуса (реализовано, `keeper/internal/incarnation`, CHECK `incarnation_status_valid` — миграции 005 + 031 + 036):

- **`destroying`** (S-D1) — оператор инициировал destroy: запущен teardown через scenario `destroy` (S-D2b) с последующим `DELETE` строки (S-D3). Это **не терминал самой строки** — при успехе строка удаляется (single-winner `DELETE … WHERE status='destroying' RETURNING`, см. ADR-027(j)), при фейле teardown переходит в `destroy_failed`. Из `destroying` все остальные операции (`run` / `upgrade` / повторный `destroy`) отвергаются fail-closed; force-путь сносит строку немедленно. Recovery «висячего `destroying`» (мёртвый владелец) — тот же single-winner `DELETE`, см. ADR-027(j).
- **`destroy_failed`** (S-D2a) — teardown (scenario `destroy`) упал на хостах: инстанс **не удалён**, `state` остался last-known-good (teardown работает с хостами, не с jsonb-state). Терминал, требующий вмешательства оператора: из него оператор повторяет destroy, force-сносит или снимает в `ready`. Отдельный статус (а **не** `error_locked`), потому что семантика и пути восстановления различны — частичный teardown-фейл ≠ частичный apply-фейл.

`migration_failed` — терминал упавшей миграции state_schema ([Versioning и миграции state_schema](#versioning-и-миграции-state_schema), ADR-019): `ROLLBACK`, state не сохраняется, incarnation залочена; то же значение в CHECK-constraint и `ValidStatus` (`keeper/internal/incarnation`).

Текущий реализованный enum (`ValidStatus` + CHECK `incarnation_status_valid`) = `ready` / `applying` / `error_locked` / `migration_failed` / `destroying` / `destroy_failed`. **`drift`** вводится в `ValidStatus` + CHECK-миграцию по [ADR-031](#adr-031-scry--drift-detection-declarative-dry-run-reconcile) (Scry) — **информационный, НЕ блокирующий** статус (remediation = обычный apply из `drift` → `ready`); реализация в работе (on-demand-пилот). **`provisioning`** в таблице — **пост-MVP** (фаза ещё не имплементирована): в каталоге enum выше есть, но кодом пока не принимается — появится в `ValidStatus`/CHECK при имплементации соответствующей фазы.

### API оператора

```
keeper.incarnation.create name=X service=Y inputs={...}     # запуск сценария create
keeper.incarnation.run    name=X scenario=add_user inputs={...}  # любой другой сценарий
keeper.incarnation.get    name=X                            # spec + state + status
keeper.incarnation.list   filter={...}                      # список инстансов
keeper.incarnation.history name=X                           # state_history
keeper.incarnation.unlock name=X reason=...                 # снятие error_locked
keeper.incarnation.upgrade name=X to_version=v2.0           # переход на новую версию service
keeper.incarnation.destroy name=X                           # удаление
```

Тот же набор доступен через MCP-tools. Оператор смотрит state-объект, видит «вот юзеры redis-кластера», добавляет одного — это `incarnation.run scenario=add_user`.

## Targeting и связь хостов

Переписано под [ADR-008](#adr-008-coven--только-стабильные-логические-теги). Полная нормативная спецификация таргетинга — [`docs/scenario/orchestration.md`](scenario/orchestration.md); здесь — архитектурный summary.

### Coven — стабильные логические теги

Coven — **только стабильные** логические теги (кластер / проект / окружение / ЦОД / тип железа). Когда incarnation создаётся, его имя становится **корневой Coven-меткой** всех его хостов:

- `incarnation.name = test-cache-redis-cl-dev` → coven `test-cache-redis-cl-dev`.

**Под-ковенов по роли (`{incarnation.name}-{role}`) больше нет** — convention удалена ([ADR-008](#adr-008-coven--только-стабильные-логические-теги)). Роль (master / replica) **не Coven**: она волатильна (failover) и не годится в стабильную метку. Coven назначаются **keeper-ом автоматически**; дополнительные стабильные covens (например, `baremetal`, `prod`) присваиваются декларативно через incarnation, оператор не делает отдельных API-вызовов «тегни хост».

### `on:` — стабильный таргет шага

Таргет шага сценария — ключ **`on:`**, резолвится по Postgres (стабильный слой):

```yaml
# Весь incarnation (on: опущен — корневой coven подразумевается)
- name: Apply base config everywhere
  apply: { destiny: redis-base, input: { ... } }

# Локальная задача на самом keeper-е (cloud-create, vault-resolve, http-call)
- name: Provision VMs
  on: keeper
  module: core.cloud.provisioned
  state: created
  params: { ... }

# Пересечение (AND) стабильных covens, всегда ⊆ хостов incarnation
- name: Tune kernel on bare-metal hosts of this cluster
  on: ["${ incarnation.name }", baremetal]
  apply: { destiny: kernel-tuning, input: { ... } }
```

**Контракт резолвера** (инвариант): список в `on:` — И/пересечение covens; результат **всегда ⊆ хостов incarnation**; **кросс-incarnation таргетинг запрещён грамматикой** (инвариант безопасности); роль в `on:` не участвует. Полностью — [`docs/scenario/orchestration.md §3`](scenario/orchestration.md#3-таргет-шага--on).

### `where:` — волатильная роль через probe + register

Волатильная роль (кто сейчас master) не хранится нигде стабильно. Сценарий ставит **probe-шаг** (`module: core.exec.run` + `register:` + `changed_when: false` + `failed_when:` для полноты), затем таргетит следующий шаг ключом **`where:`** — волатильным предикатом по `register:` этого probe, per-host:

```yaml
- name: Detect actual redis role per host
  module: core.exec.run
  on: ["${ incarnation.name }"]
  register: redis_role
  changed_when: false
  failed_when: size(register.redis_role) < incarnation.host_count
  params: { command: "redis-cli role | head -1" }

- name: Restart only the current replicas
  on: ["${ incarnation.name }"]
  where: register.redis_role.stdout == 'slave'
  module: core.service.restarted
  params: { name: redis-server }
```

Двухфазный резолв: `on:` → Postgres (стабильно), `where:` → `register:` (рантайм). После failover — просто новый probe; никакого кэша, коллектора роли или волатильных Soulprint-фактов ([ADR-008](#adr-008-coven--только-стабильные-логические-теги)). `where:`-ключ шага и `soulprint.where(...)`-функция в выражении — **разные позиции** (на каких хостах vs откуда взять данные), подробно разведено в [`docs/scenario/orchestration.md §4`](scenario/orchestration.md#4-волатильный-предикат--where). Прежний `filter:` из старых примеров — изъят, заменён на `where:`.

В template-контексте сценария всегда доступны:
- `incarnation.name` — имя инстанса.
- `input` — параметры, переданные в сценарий.
- `essence` — merged essence (default + spec) после прохождения [pipeline-а](#essence-pipeline-сборки).
- `state` — текущий state из БД (для сценариев, которые читают существующее состояние).
- `soulprint.hosts` — список хостов прогона со стабильными фактами (`sid`/`role`(declared)/`network`/`os`/`covens`); `.where("<predicate>")` фильтрует по CEL-предикату-строке. Сокращённая форма того же запроса — `soulprint.where("<predicate>")` (например, `soulprint.where("'X' in covens")` вместо `soulprint.hosts.where("'X' in covens")`). Scenario-only; destiny получает топологию только через явный `apply: input:`. Нормативно — [`docs/scenario/orchestration.md §4.1`](scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор).

> **Отличие от destiny template-контекста.** В destiny `vars.*` означает [destiny-локалы из `vars.yml`](destiny/vars.md), а `essence.*` **отсутствует**: destiny изолирована, получает только то, что пришло в `input:`. В scenario, наоборот, `essence.*` доступен напрямую (отсюда же scenario подкладывает значения в `input:` destiny при `apply:`-вызове), а destiny-`vars` не виден — он локален для конкретной destiny.

### Связь хостов через Soulprint

Когда хосту нужны данные другого хоста (то, что в Salt делается через Mine, в Ansible — через `hostvars[X]`), используется функция `soulprint.where(<predicate>)` по **стабильному** слою (CEL-предикат — статический строковый литерал):

```yaml
master_addr: "${ soulprint.where(\"incarnation.name in covens\")[0].network.primary_ip }"
```

Запрос идёт в Postgres + горячий слой Redis. Soulprint после [ADR-008](#adr-008-coven--только-стабильные-логические-теги) хранит **только стабильные** факты, поэтому `soulprint.where(...)` оперирует стабильным слоем; волатильная роль (кто сейчас master) — исключительно через probe + `where:`-ключ, не через Soulprint. Предикат `.where(...)` — статический строковый литерал, раскрываемый на compile-фазе в нативный CEL filter-comprehension (не runtime; динамическая склейка предиката запрещена, первый элемент — `[0]`, см. [ADR-010](#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)). Cross-host master discovery — через аксессор `soulprint.hosts` (`soulprint.hosts.where("role == 'primary'")[0].network.primary_ip`), declared-роль берётся из `incarnation.spec.hosts[].role`, на runtime master определяется probe (как в `restart`); нормативно — [`docs/scenario/orchestration.md §4.1`](scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор) (прежний open Q закрыт там же, см. [§8](scenario/orchestration.md#8-открытые-вопросы-расширения-не-закрывать-молча)).

## Versioning и миграции state_schema

> Тут речь только про **версию структуры `incarnation.state`** — это не «версия сервиса». Версия самого сервиса как артефакта — это git tag, под которым он закоммичен (см. [ADR-007](#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). `state_schema_version` — отдельная сущность, нужна **только** для миграций jsonb-state в БД, и сосуществует с git-tag-ами сервиса независимо.

Service-разработчик меняет `state_schema` между версиями: добавляет поле, переименовывает, меняет структуру вложенных объектов. Существующие incarnation в БД хранят state по **старой схеме** — нужно мигрировать.

### `state_schema_version` и каталог `migrations/`

В service.yml:
```yaml
state_schema_version: 2
```

В service-репо:
```
migrations/
├── 001_to_002.yml
└── 002_to_003.yml
```

В incarnation-таблице — поле `state_schema_version`, фиксируется при создании.

### DSL миграций

Полная нормативная спецификация — [`docs/migrations.md`](migrations.md), фиксация решения — [ADR-019](#adr-019-state_schema-migration-dsl).

Грамматика MVP: плоский (`rename`/`set`/`delete`/`move`) + CEL-выражения в значениях `set:` через маркер `${ … }` + структурный `foreach` для итераций по коллекциям. Условный `if:`-ключ — отложен до первого реального запроса (расширение без breaking change). Сложные сценарии, не покрываемые грамматикой, — в MVP не поддерживаются (escape-модуль `state.migrate` отвергнут [ADR-019(e)](#adr-019-state_schema-migration-dsl) как не из словаря; кандидат `core.incarnation.state-migrate` — при необходимости отдельным ADR).

Пример (тот же `001_to_002`, переписанный под MVP-грамматику):

```yaml
# migrations/001_to_002.yml
from_version: 1
to_version: 2
description: redis_users превращается из массива строк в map с acl и state

transform:
  - rename: { from: state.redis_users, to: state.redis_users_legacy_v1 }

  - foreach: "${ state.redis_users_legacy_v1 }"
    as: user_name
    do:
      - set:
          path: "state.redis_users.${ user_name }"
          value:
            acl: "off ~* &* +@all"
            state: "off"

  - delete: { path: state.redis_users_legacy_v1 }
```

Контекст исполнения migration-CEL: доступно `state.*` (мутируемое) и `<as-name>` внутри `foreach.do[*]`. Запрещено: `vault(...)`, `now()`, `register.*`, `soulprint.*`, `essence.*`, `input.*` — миграция = чистая функция от старого state, side-effect-free.

### Upgrade — явный шаг оператора через UI

Миграция **не запускается автоматически при apply сценария**. Это явная операция оператора:

```
keeper.incarnation.upgrade name=X to_version=v2.0
```

Что происходит в UI/CLI:
1. Service подключён в master, виден в UI с известными версиями (master видит git-tag-и).
2. У incarnation отображается текущая `service_version` и доступные обновления.
3. Оператор выбирает `v2.0`, нажимает «upgrade».
4. Keeper в одной транзакции: прогоняет миграции `001_to_002`, `002_to_003` цепочкой, обновляет `state_schema_version` и `service_version`, пишет snapshot в `state_history` с пометкой `migration`.
5. Если миграция упала — `status: migration_failed`, никаких изменений в state не сохраняется, incarnation залочена, оператор разбирается.

После успешного upgrade сценарии продолжают работать с новой схемой. Старая остаётся как легаси-снапшот в `state_history`.

### Совместимость scenarios

Сценарий привязан к **версии service-а**, под которой запускается. После upgrade incarnation на новый service-version старые сценарии больше не вызываются — оператор работает с новыми. Это намеренно: смешивать сценарии разных версий — путь в drift.

## Cloud-интеграция через `keeper.cloud`

Динамическое создание VM реализовано как **cloud-create-шаг сценария** с `on: keeper` через CloudDriver-плагин. Service не знает специфики облаков — он знает «нужен шаг создания VM с параметрами», Keeper выбирает driver и исполняет.

Граница git/БД: **Provider** (настроенная учётка облака) и **Profile** (многоразовый шаблон VM) живут в Postgres, управляются через API/MCP — это runtime-конфиг, не код. Default-essence сервиса в git выступает подложкой, оператор переопределяет в spec incarnation. Параметры профиля валидируются против `profile_schema`, который CloudDriver публикует через RPC `Schema()`.

Destroy-операции защищены обязательным набором guard-rails (tombstone period с `tombstone_ttl`, confirm-flag, storage protection, аудит) — одна опечатка в `count` не должна стирать прод.

Полный разбор Provider/Profile, шага сценария `core.cloud.provisioned`, привязки coven и безопасности destroy — в [`docs/keeper/cloud.md`](keeper/cloud.md). Контракт `CloudDriver` и каталог плагинов — в [`docs/keeper/plugins.md`](keeper/plugins.md).

## Reaper / Жнец

Фоновая задача внутри `keeper`, чистящая БД от мусора и поддерживающая инварианты реестра. Не отдельный бинарь. Работает только на одном Keeper-инстансе одновременно — лидер выбирается через Redis-lease. Действует **только над Postgres**: на хосты по SSH не ходит, хостовая чистка кеша модулей описана отдельно в [`docs/soul/modules.md`](soul/modules.md).

Если scope в будущем разрастётся за рамки cleanup-а (миграции таблиц, перенос архивных записей, GC холодных данных) — резервируем имя **Charon** для более широкого процесса; пока имя одно — **Reaper / Жнец**.

Свойства, правила (`expire_pending_seeds`, `purge_used_tokens`, `purge_souls`, `purge_old_seeds`, `mark_disconnected`), полный YAML-конфиг и метрики — в [`docs/keeper/reaper.md`](keeper/reaper.md).

## Доставка SoulSeed-токена на хост

«Оператор сгенерировал bootstrap-токен → токен оказался в файле на ВМ, где запустится `soul`» — это операционный шаг между выпиской токена и стартом Soul-а. Сам по себе он не диктуется Soul Stack-ом: способ физической доставки — выбор оператора. Целевой путь — через `keeper.push` (единый аудит, RBAC, логи в Keeper-е); ansible-role, SSH/SCP, CI/CD pipeline и cloud-init допускаются как альтернативы.

Защиты, на которых стоит безопасность токена при любой доставке — короткий TTL (24h по умолчанию), одноразовость (сжигание при первом CSR), привязка к конкретному SID. Дополнительные защиты сверх этого — см. open-вопрос «Утечка SoulSeed-токенов» в [Открытых вопросах](#текущие).

Полный список сценариев доставки, требования к правам и mode файла, рекомендация по systemd `LoadCredential=` — в [`docs/soul/onboarding.md`](soul/onboarding.md).

## End-to-end сценарий установки

Эталонный путь от пустой инфраструктуры до управляемого парка хостов. Каждый шаг ссылается на раздел, где он расписан подробно.

1. **Поднимается Postgres и Redis** — внешние зависимости Keeper-кластера (см. ADR-005, ADR-006).
2. **Поднимается Keeper-кластер** — один или несколько `keeper`-инстансов поверх общей PG+Redis. KID каждого инстанса фиксируется в конфиге.
3. **Bootstrap первого Архонта** — оператор запускает `keeper init --archon=<aid>` на хосте Keeper-а. Команда под PG advisory lock проверяет, что реестр `operators` пуст, создаёт первого Архонта с ролью `cluster-admin` (`permissions: ["*"]`), выпускает JWT-токен (TTL 30 дней) и кладёт в файл `mode 0400`. До этого шага Keeper отказывается стартовать с операторами=пусто без `--initialize`. См. [ADR-013](#adr-013-bootstrap-первого-архонта) и [ADR-014](#adr-014-identity-модель-оператора-archon).
4. **Архонт выписывает остальных операторов** — через OpenAPI/MCP с `Authorization: Bearer <jwt>` создаются обычные Архонты с ограниченными правами по Coven-ам (FK `created_by_aid` в реестре `operators`).
5. **Оператор добавляет Soul** — через OpenAPI/MCP `keeper`-а: указывает SID (FQDN), желаемый `transport` (`agent` или `ssh`), Coven-метки. Запись в `souls` появляется в статусе `pending`.
6. **Для `transport: agent`:** оператор получает короткий **bootstrap-токен** (не сертификат, не ключ — только токен на одноразовое использование). Доставляет на ВМ вместе с `soul`-бинарём — целевой путь через `keeper.push` (тот же SSH-механизм, что и для агентless-Destiny, но с задачей «развернуть pull-агента»); альтернативные пути — Ansible-role, cloud-init, обычный SSH (см. «Доставка SoulSeed-токена на хост»).
7. **Soul стартует на ВМ:**
   - читает токен из файла,
   - локально генерирует приватный ключ (никогда не покидает хост),
   - формирует CSR,
   - подключается к Keeper-у по адресу из конфига,
   - предъявляет токен и CSR,
   - получает SoulSeed (подписанный сертификат),
   - кладёт его рядом с приватным ключом, сжигает токен,
   - открывает gRPC bidi-стрим уже с SoulSeed как mTLS-идентичностью.
8. **Запись в `souls` переходит в `connected`**, в `soul_seeds` появляется активный seed.
9. **Дальше — обычная работа:** Keeper пушит Destiny по стриму, Soul применяет, отчитывается событиями. SoulSeed ротируется раз в неделю по живому стриму без участия оператора.
10. **Для `transport: ssh`:** шаги 6–8 пропускаются. Оператор сразу делает `POST /v1/push/apply` ([нормирование request/response](keeper/operator-api/push.md#post-v1pushapply--push-прогон-destiny-по-ssh)) с inventory и Destiny, Keeper по SSH ходит на хост, выполняет, забирает результаты. Soul-агент на хост не ставится.
11. **Жнец** периодически чистит реестр от мусора (см. «Reaper / Жнец»).

## Поток данных верхнего уровня

1. Оператор пишет Destiny/Essence в git, прогоняет `soul-lint` локально и в CI (рендер → валидация по схеме → статический анализ). Без зелёного `soul-lint` ничего наружу не уезжает.
2. Destiny попадает в Keeper через OpenAPI или MCP. Keeper проверяет RBAC, повторно рендерит и валидирует, кладёт в реестр Destiny (Postgres).
3. **Pull:** Keeper пушит соответствующим Souls (агентский transport) команду «применить такую-то Destiny с такой-то Essence» по живому gRPC-стриму.
   **Push:** Keeper для каждого целевого хоста (`transport: ssh`) поднимает SSH-сессию через выбранный провайдер, выполняет шаги, забирает результат.
4. Soul (или push-сессия) применяет, отчитывается событиями (start, step, success/failure).
5. Keeper агрегирует результат, выставляет наружу через OpenAPI/MCP, публикует метрики и трейсы (OTel).
6. Soulprint собирается Soul периодически и по требованию, складируется в Keeper (Postgres), доступен RBAC-фильтрованно.

## Сквозные требования и где они приземляются

| Требование (из [docs/requirements.md](requirements.md)) | Где живёт | Заметки |
|---|---|---|
| Метрики | Все три бинаря | Нормировано [ADR-024](adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge): Prometheus-primary (pull `/metrics`), namespace-префиксы `keeper_*` / `soul_*`. OTLP-push метрик — опциональный мост. Спека — [observability.md](observability.md). |
| OpenTelemetry | Все три бинаря | Нормировано [ADR-024](adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge): OTel-bridge для трейсов (сквозные оператор → Keeper → Soul через gRPC-метаданные) + опц. push метрик; resource-attrs `service.name` + `soulstack.kid` / `soulstack.sid`. Спека — [observability.md](observability.md). |
| Hot-reload конфига + перезапись на диск | Все три бинаря | Механизм нормирован [ADR-021](#adr-021-hot-reload-конфига-с-write-back-yaml): file-edit (SIGHUP) + API/MCP с write-back YAML, validation pipeline parse → schema → semantic → atomic swap, audit-events `config.reload_succeeded` / `config.reload_failed`. История — git-blame + audit (БД-таблица `config_history` отложена). |
| Ротация логов | Все три бинаря | Встроенная по умолчанию, без зависимости от внешнего logrotate. |
| Vault | Keeper (полная: Essence, CA для SoulSeed, SSH-провайдер); Soul (только клиент короткоживущих токенов) | Soul не должен иметь прав читать чужие Essence. |
| RBAC | Keeper | Применяется к OpenAPI, MCP, push-операциям единообразно. |
| MCP | Keeper | Keeper — MCP-сервер; первичный интерфейс оператора наравне с OpenAPI. |
| OpenAPI | Keeper | gRPC-Gateway или connect-go поверх того же контракта; первичный интерфейс оператора. |
| Postgres | Keeper-кластер | См. ADR-005. Source of truth. |
| Redis | Keeper-кластер | См. ADR-006. Heartbeat-кэш, lease, pub/sub, лидер для Reaper. |
| Безопасность | Все, особенно Soul | Принцип наименьших привилегий, минимальная поверхность Soul, mTLS обязательный, никакого PEM/private key в БД. |

## Открытые вопросы

Разделены на закрытые в предыдущих раундах (для истории не сохраняем — см. git log) и текущие.

### Текущие

1. ~~**Bootstrap первого оператора («Создатель»).**~~ **Закрыт ADR-013 + ADR-014:** имя сущности — **Archon** (Архонт), идентификатор — **AID** (kebab-case); механизм — команда `keeper init --archon=<aid>`; форма credential — JWT (Vault KV signing key, MVP); реестр `operators` в Postgres; restart-семантика — отказ без `--initialize`; HA race — PG advisory lock.
2. **Клиент оператора — форма CLI.** Первичный интерфейс — OpenAPI и MCP (ADR-004), CLI допустим как тонкая обёртка. Открыто: будет ли он (отдельный бинарь / подкоманда `keeper` в клиентском режиме / только сторонние тулзы поверх API), и нужна ли поставка официального CLI в составе релиза.
3. ~~**SSH-2. SSH-провайдеры для `keeper.push`.**~~ **Закрыт [ADR-020 amendment (2026-05-26)](#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle):** MVP-набор — 3 провайдера, все закоммичены и работают: `soul-ssh-static` (`4f95ef6`), `soul-ssh-vault` (`3642520`, Vault SSH CA), `soul-ssh-teleport` (`af27678`, `SignReply.proxy_jump` field 4). Решения по трём общим механикам зафиксированы: credentials-flow — Вариант B для CA-провайдеров (плагин сам в Vault через `vault_access`; расходится с cloud-Variant A сознательно — `ssh/sign` это операция, не KV-read); key-ownership — Keeper-ephemeral (приватник не покидает Keeper, security-first); params-delivery — env-convention per-plugin (`SOUL_SSH_*_PARAMS`). Открыто — **dispatcher `proxy_jump` support** (Teleport-через-bastion): пилот применим к хостам с прямой SSH-доступностью, отдельный слайс в работе.
5. **Под-вопросы модели модулей.** Сама модель закреплена. Открыто внутри:
   - ~~точный набор core-модулей в MVP~~ **закрыт ADR-015**: 17 Soul-side (`pkg`/`file`/`service`/`user`/`group`/`exec`/`cmd`/`cron`/`mount`/`git`/`archive`/`sysctl`/`url`/`line`/`repo`/`firewall`/`http`) + 3 Keeper-side (`soul.registered`/`cloud.provisioned`/`vault.kv-read`, последние два — ADR-017);
   - где живёт реестр модулей в Keeper-е — Postgres `bytea` / отдельный artifact store (S3-compatible) / файловая система keeper-а;
   - ~~формат и место манифеста модуля — отдельный `manifest.yaml` рядом с бинарём vs первый gRPC-метод `Manifest()`~~ **закрыт ADR-020(a):** статический `manifest.yaml` в корне репо плагина и рядом с бинарём; RPC `Manifest()` в MVP не вводится;
   - ~~точная версия и формат stdio-handshake-протокола (вероятно, как у `hashicorp/go-plugin`)~~ **закрыт ADR-020(b/c):** JSON в одну строку с магическим префикс-полем `"soul_stack":"plugin-v1"`; `protocol_version` дублируется в manifest и handshake; соответствие `protocol_version: N` ↔ `proto/plugin/vN/`. Полная спека — [`docs/keeper/plugins.md`](keeper/plugins.md);
   - ~~политика версионирования модулей и совместимости с разными версиями `SoulModule` API~~ **закрыт ADR-020(c):** forward-compat only-add внутри `proto/plugin/vN/`, host держит `SupportedProtocolVersions` (MVP `[1]`), hard fail при mismatch;
   - **оптимизация на потом:** обязательное декларирование `required_modules: [haproxy, myapp]` в Destiny — позволит передавать только нужные модули вместо всех.
6. ~~**Soulprint: схема и расширения.**~~ **Закрыт ADR-018 (только typed-схема MVP).** Поля: `sid`/`hostname`/`os`(family/distro/version/codename/arch/pkg_mgr/init_system)/`kernel`(version/release)/`cpu`(count/model/vendor)/`memory`(total_mb/available_mb/swap_mb)/`network`(primary_ip/fqdn/interfaces[]). Каноническая CEL-форма — `soulprint.self.<path>`. Covens — Keeper-registry-проекция, не в Soul-side фактах. Механизм user-collectors (open Q №22) — **остаётся открытым** (требует решений по sandbox/правам/format коллектора, не только schema).
7. ~~**Совместимая версия protobuf-контракта.**~~ **Закрыт ADR-012:** forward-compat only-add внутри `proto/keeper/v1/` (никогда не удалять поля, не reuse field-номера; breaking changes — только через новый пакет `v2/`). Keeper можно апгрейдить отдельно от Souls.
8. **Локальный admin-эндпоинт на Soul.** **Отложено post-MVP.** В MVP admin-операции на Soul-host: `SIGHUP` для hot-reload `soul.yml`, локальный shell-доступ к логам / `journalctl` / `metrics`-listener для observability, централизованный rollout конфига через CI / Ansible / SSH. Локальный HTTP/MCP listener на Soul (status / force-resync / dump Soulprint без Keeper / API-driven config mutation) — при реальной необходимости (отдельный ADR, propose-and-wait по транспорту: HTTP vs Unix socket vs FromKeeper-команда).
10. **LB-1. Балансировка по SID/Coven меткам.** Достаточно ли L4-LB (любой Keeper обслужит любого Soul, Coven — только в прикладной логике), или нужен L7-aware LB / собственный routing-prefix Keeper. Отложено по решению пользователя.
11. **Утечка SoulSeed-токенов до использования.** Дополнительные защиты сверх TTL+одноразовости (привязка к IP/CIDR, требование cloud-metadata-доказательства, ручное approval). Отложено в копилку идей.
12. **Механика чеков `last_seen_at`.** Сейчас обновляется на любое сообщение по стриму; явные keepalive-пинги отложены. Если этого окажется недостаточно для точного определения «жив/не жив» — выбираем явный механизм пинга.
13. ~~**Список cloud-провайдеров для MVP.**~~ **Закрыт [ADR-017](#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) amendment (2026-05-25, реализовано к 2026-05-26):** первый набор — **6 официальных** провайдеров (AWS / GCP / Azure / Yandex Cloud / Proxmox / OpenStack; бинари `soul-cloud-{aws,gcp,azure,yc,proxmox,openstack}`); **vSphere** — community / отложен. AWS — пилот (reference). К 2026-05-26 все 6 закоммичены и работают: AWS (`ec83487`+`72ff14b`), GCP (`6bfd83c`), YC (`a181d49`), Azure (`c7d93ad`), OpenStack (`a03a404`), Proxmox (`af43665`) — см. [ADR-017 amendment 2026-05-26 (j)/(k)/(l)](#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read).
14. **Bootstrap soul-агента на новой VM.** **Рекомендованный путь — cloud-init userdata** ([ADR-017](#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) amendment (h)): `CreateRequest.userdata` несёт blob, разворачивающий soul на новой VM; per-VM-токен-в-userdata отложен (chicken-egg: SID присваивается после create), в MVP — общий batch-blob + онбординг через CSR. Альтернативы (`keeper.push` после create / gold image с soul в AMI) остаются возможны. Открыто: формализация выбора пути под конкретные сценарии.
15. ~~**Drift detection.**~~ **Закрыт [ADR-031](#adr-031-scry--drift-detection-declarative-dry-run-reconcile) (Scry):** declarative drift = «dry-run reconcile показал бы `changed=true` хотя бы на одном декларированном ресурсе» (как Salt `test=True`/Ansible `--check`, НЕ полный дамп хоста). Механизм — подсистема **Scry** (read-only): MVP on-demand (`keeper.incarnation.check-drift`) → фоновый скан (Reaper-подобный лидер). `Plan` → pure-read с машинным `changed`; only-add `PlanEvent.changed` + `ApplyRequest.dry_run`; статус **`drift`** информационный (remediation = обычный apply); отчёт **`DriftReport`**; community-модуль без read-safe → default-deny. Дизайн принят, реализация в работе (on-demand-пилот).
16. **Recovery-сценарии.** Service может объявить специальный сценарий-восстановитель для типовых случаев частичного фейла (`error_locked`). Формат, обязательность, конвенция именования.
17. **Reconcile-loop для cloud-incarnation.** Фоновый процесс «declared count vs actual VM count» — закладываем сразу или MVP только manual `incarnation.upgrade/scale`.
18. ~~**DSL миграций state_schema.**~~ **Закрыт ADR-019:** плоский (`rename`/`set`/`delete`/`move`) + CEL-выражения в значениях + структурный `foreach` (MVP). Условный `if:` — отложен до первого реального запроса (расширение без breaking change). Forward-only, escape-модуль не вводится. Полная спека — [`docs/migrations.md`](migrations.md).
19. ~~**`state_history` retention.**~~ **Решён (2026-05-25):** **soft-delete / архивирование по конфигу, НЕ hard-delete.** Хранить последние **N=50** снимков на incarnation + **ВСЕГДА** snapshot на каждый `state_schema`-version-bump (миграции остаются восстановимыми). «Лишние» снимки **архивируются** (архив-таблица либо `archived_at`-флаг — конфиг-knob), физически не стираются (forensic > GC). Чистит/архивирует **Reaper-правило**. Реализация — Track 4 ([roadmap.md](roadmap.md)), ещё не сделано.
20. ~~**UI Keeper-а.**~~ **Решён (2026-05-25): UI — отдельный артефакт (SPA), НЕ embedded в `keeper`.** Стек — React + TypeScript + Vite + TanStack Query + React Hook Form + Zod + lucide (зеркало внутреннего референса salt-manager). Design-system — **адаптация `saltgui-design-system`** (CSS-токены/компоненты, re-skin под бренд/словарь Soul Stack). TS-клиент + Zod-типы **генерятся из [`docs/keeper/openapi.yaml`](keeper/openapi.yaml)** (не пишутся руками). Вся лексика UI — строго по [naming-rules.md](naming-rules.md) (Keeper / Souls / Coven / Soulprint / Destiny / Archon, НЕ SaltStack minion/grain/pillar). Старт разработки — когда API дойдёт до почти-полной готовности. План — [roadmap.md](roadmap.md).
21. **`host_schema` в `service.yml` (копилка идей).** Декларация ожидаемой топологии хостов для Incarnation: список ролей (`required_roles`) и ограничения на количество хостов в каждой роли (`role_constraints.<role>.count: { eq | gte | lte }`), возможно расширяемое до фильтров по Soulprint (минимум CPU/RAM/ОС). Что давало бы: ранний отказ при создании Incarnation с неверным числом ролей (до cloud-create), сверка declared-топологии (`incarnation.spec.hosts[].role`, [ADR-008](#adr-008-coven--только-стабильные-логические-теги)) с объявленной схемой до прогона, подсказки в UI/MCP. Отложено до того момента, как реальные сценарии покажут, какие именно проверки окупаются — без них поле превратится в декоративное. NB: таргетинг шагов — `on:`/`where:` ([`docs/scenario/orchestration.md §4`](scenario/orchestration.md#4-волатильный-предикат--where), [§4.1](scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор)), роль **не** Coven ([ADR-008](#adr-008-coven--только-стабильные-логические-теги)); статическая проверка `on:`/`where:`-литералов — backlog `soul-lint` ([soul-lint.md → B2](soul-lint.md#b2-статпроверка-where--и-on-литералов)), не часть этого пункта.
22. **`soulprint.collectors` в `soul.yml` (копилка идей).** Явное декларирование набора коллекторов Soulprint в конфиге агента: встроенные группы (`core` — os/kernel/network/memory/cpu/hostname, `systemd` — units) и каталог пользовательских детекторов (`custom_dir: /etc/soul/soulprint.d/`). Сейчас в `soul.yml` остаётся только `refresh_interval`; фактический набор фактов — внутренний дефолт. Связано с открытым вопросом 6 (общая схема Soulprint и расширения), но касается конкретно формы конфига на хосте: нужен ли вообще тумблер, или коллекторы всегда включены, а пользовательские детекторы подхватываются из фиксированного пути по соглашению.
23. ~~**Event-driven контур (Salt beacons / engines-эквивалент).**~~ **Закрыт [ADR-030](#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor):** beacons-контур введён сущностями **Vigil** (Soul-side проверка, read-only) → **Portent** (событие, only-add `EventStream`-oneof) → **Oracle** (Keeper reactor-роутер) → **Decree** (правило reactor, default-deny, action = scenario-only через work-queue [ADR-027](#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). Незафиксированное предложение `SoulBeacon` заменено финальными именами; community-проверки — через plugin-kind `soul_beacon` (S5). Engines-эквивалент (long-running на хосте/Keeper-е) этим ADR **не** вводится — остаётся отложенным.
24. ~~**Per-task гранулярность `serial:`.**~~ **Закрыт [ADR-056](#adr-056-staged-render--прогон-сценария-как-n-упорядоченных-passage) (staged-render).** MVP был per-RUN min-width (ширина волны одна на прогон = минимальная положительная `serial:`-ширина среди задач, [`docs/scenario/orchestration.md §2.2.1`](scenario/orchestration.md)), потому что per-task dispatch (несколько `ApplyRequest` / строк `apply_runs` на хост) не реализован. ADR-056 вводит **Passage** — прогон как N упорядоченных этапов (render→dispatch→barrier→register) по оси задач: per-task dispatch теперь реализуется как N `ApplyRequest` на хост по Passage (amend [ADR-012](#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) dispatch-модели + only-add `passage` + PG-PK per-passage). Главный драйвер ADR-056 — реализация probe→where (`register` в `where:`/`apply:input` следующих задач), которую канон orchestration.md §4/§5 уже обещает; per-task `serial:` — следствие той же staged-модели.
25. **Per-host `RenderedTask` dispatch (host-вариативные flow-control-предикаты на multi-host).** Отложен, потребует отдельного ADR (снятие host-инвариантности params + изменение dispatch-границы Keeper↔Soul). Сейчас в pilot fail-closed guard: flow-control-предикат (`when:`/`changed_when:`/`failed_when:`) со ссылкой на `soulprint.self` допустим только при single-host таргете; на multi-host рендер падает с явной ошибкой ([`docs/templating.md §4`](templating.md#4-соотношение-фаз-обработки-yaml)).
26. **Audit-log scaling для массовых прогонов (100k+ хостов).** Сейчас `audit_log` — PG-таблица с INSERT per-event. Один Tide-прогон на 100k VM с wave=1000 даёт ~200k записей (`tide.started` ×1 + `surge_started`/`surge_completed` ×100 каждое + per-apply_run `apply.dispatched`/`apply.completed` ×100k×2). Узкие места: INSERT-throughput PG (~5-10k INSERT/s), размер таблицы (~50MB/прогон → 250GB/год при 100 прогонов/неделя), full-scan чтения для UI без партиционирования. **Решение отложено в backlog следующих релизов** (решение пользователя 2026-05-27). Возможные варианты на рассмотрение: (A) semantic filter «no per-SID events если есть Tide-агрегат» (-99% объёма, источник правды — `apply_runs` table); (B) partitioning by month + retention 90/365 дней; (C) hot/cold split (PG для UI, S3/parquet для long-term); (D) batched INSERT; (E) async sink через Redis Stream → отдельный writer (parity SaltStack returners). До 10k VM — текущий audit без оптимизации работает; на пилотных прогонах оценить реальную нагрузку и выбрать вариант.
27. **UI i18n — runtime-discoverable список языков (`manifest.json`).** UI (companion-repo `soul-stack-web`) использует hybrid lazy-load: default-язык `ru` bundled inline, остальные (`en`+) — static `public/locales/<lang>/<ns>.json`, фетчатся через `i18next-http-backend` (не Keeper — это static-ассеты SPA, едут со static-хостом фронта). Список доступных языков сейчас **захардкожен** в `SUPPORTED_LANGS` (TS-литерал) — даёт типобезопасность `type Lang` + build-time валидацию полноты через ns-key-sync тест. **Решение пользователя 2026-05-27: оставить захардкоженным, менять с релизами.** Идея на будущее (копилка): вынести список языков в `/locales/manifest.json`, читаемый тоглом в рантайме → добавление языка без ребилда JS даже для переключателя (translation-команда дропает папку + строку в manifest). Trade-off: +1 сетевой запрос на старте, потеря TS-литерал-типа `Lang`, нет build-time-гарантии полноты переводов (битый язык ловится только пользователем в рантайме). Вводить при реальном translation-workflow / 3+ community-языках — non-breaking добавка.

Каждый из этих пунктов превратится либо в новый ADR здесь, либо (если разрастётся) в собственный документ в [docs/](.).
