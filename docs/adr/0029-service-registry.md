# ADR-029. Реестр Service-ов → Postgres

> Закрывает остаток [ADR-028(h)](0028-rbac-storage.md#adr-028-rbac-storage--postgres) (config→БД для прочих managed-сущностей). Тот же паттерн, что у RBAC (ADR-028): managed-каталог уходит из статического `keeper.yml` в Postgres с runtime-снимком + pub/sub-инвалидацией. В отличие от ADR-028 (Фаза 0 — документация), здесь **имплементация выполнена** срезами S1–S4.

- **Контекст.** Реестр Service-ов (`keeper.yml → services[]`) и связанные well-known скаляры (`default_destiny_source`, `default_module_source`) жили в статическом конфиге. Это давало те же дефекты, что у YAML-RBAC до ADR-028:
  - **Per-host расхождение.** stateless-кластер Keeper-ов ([ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)) — каждый инстанс знал только свой `keeper.yml`; добавление Service-а требовало правки файла + reload на каждой ноде.
  - **Нет API-управления.** Service-ы (managed runtime-сущность, симметрично Incarnation/Provider/Profile — все в БД по [ADR-005](0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres)) нельзя было заводить через OpenAPI/MCP, только правкой YAML.
  - **`default_module_source` — мёртвое поле:** объявлено в схеме конфига, но без потребителя (резолв модулей через него не реализован).

- **Решение.**

  **(a) Storage — таблица `service_registry` + key-value `keeper_settings`.**

  | Таблица | Роль |
  |---|---|
  | **`service_registry`** | Каталог Service-ов. `name` PK (kebab-case, совпадает с `service.yml → name`), `git`, `ref` ([ADR-007](0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте): version = git ref), `refresh` (опц. duration авто-fetch, NULL-able), `created_at` / `updated_at`, `created_by_aid` / `updated_by_aid` FK→`operators(aid)` NULL-able (seed / без инициатора-Архонта). |
  | **`keeper_settings`** | Well-known скаляры кластера: `key` PK → `value`, `updated_by_aid` FK→`operators(aid)` NULL-able, `updated_at`. MVP-ключ — `default_destiny_source`. Расширяется новыми ключами без миграции таблицы. |

  **(b) Источник правды — БД, runtime — снимок + инвалидация.** Потребители (scenario-runner: `ServiceRegistry.Resolve`, `DestinySource`) читают не БД per-call, а in-memory снимок `serviceregistry.Holder` (паттерн `rbac.Holder` из [ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)): atomic-снимок `{services map[name]ServiceEntry, default_destiny_source}`, на старте — синхронный build из БД (fatal на ошибке), обновление — TTL-poll (фоновая goroutine; ошибка перечита оставляет прежний снимок + warn) + Redis pub/sub-инвалидация через топик **`service:invalidate`** (envelope `{origin_kid, at}`, self-filter по KID — паттерн `applybus`/`rbac:invalidate`). Геттеры `Resolve(name) (ServiceEntry, bool)` / `DefaultDestinySource() string` — **синхронные** (lock-free atomic.Pointer.Load, без ctx/error): отсутствие Service-а = нормальный `false`, не сбой. Это сохраняет контракт потребителей неизменным при замене источника cfg→БД.

  **(c) Связь с Destiny-каталогом — lazy resolve.** Резолв `apply: { destiny: <name> }` ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)) семантически не меняется: гибрид «per-entry `destiny[].git` override → иначе `default_destiny_source` + `{name}`». Меняется источник скаляра: `default_destiny_source` читается **лениво** из `Holder.DefaultDestinySource()` на каждый резолв (а не копируется в конструктор `DestinySource`) — иначе hot-reload скаляра через `service:invalidate` не доезжал бы до резолва.

  **(d) API/MCP — `service.*`-операции.** Реестр управляется через OpenAPI/MCP (как Архонты в [ADR-014(d)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) и роли в [ADR-028(e)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)): `service.create` / `service.update` / `service.delete` / `service.list`. Имена закреплены propose-and-wait; permission-каталог — [naming-rules.md](../naming-rules.md) / [rbac.md](../keeper/rbac.md).

  **(e) Config hard-cut.** Ключи `services:` / `default_destiny_source:` / `default_module_source:` из `keeper.yml` **удаляются** — поля `KeeperConfig.Services` / `DefaultDestinySource` / `DefaultModuleSource` и тип `config.ServiceRegistryEntry` уходят; ключи начинают отвергаться парсером как `unknown_key` (reflect-walker `shared/config/walk.go`). Легаси-инсталляций нет (продакшена нет) — миграционного слоя YAML→БД не требуется (как в [ADR-028(g)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)). `soul.yml` не затрагивается.

  **(f) `default_module_source` не переносится.** Мёртвое поле без потребителя — упраздняется без замены (не появляется ни в `keeper_settings`, ни в API). Если резолв модулей через шаблон понадобится — заводится отдельным решением как новый well-known ключ `keeper_settings` (расширение без миграции).

- **Обоснование.** Зеркально [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres): соответствие [ADR-005](0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres) (managed runtime-state в Postgres) и [ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) (общий PG → реестр одинаков на всём кластере без раскатки YAML по нодам). Service встаёт в один ряд с Incarnation/Provider/Profile — все managed через API, source of truth — БД, git хранит только код Service-репо (граница git↔Postgres сохранена: в БД координаты `name → git@ref`, не код).

- **Реконсиляция ADR.**
  - [ADR-007](0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) — `ref` Service-а теперь колонка `service_registry.ref`, а не поле `keeper.yml::services[].ref`; принцип «version = git ref» неизменен.
  - [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) — `default_destiny_source` для резолва `apply:destiny` читается из `keeper_settings`, не из `keeper.yml`; гибрид-правило резолва неизменно.
  - [ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml) — реестр Service-ов **исключён** из hot-reload-config-пути: обновление = БД-мутация + `service:invalidate`, не `SIGHUP` / config-swap (как RBAC в ADR-028(d)).
  - [ADR-028(h)](0028-rbac-storage.md#adr-028-rbac-storage--postgres) — остаток scope (config→БД для прочих managed-сущностей) закрыт этим ADR.

- **Consequences.**
  - **Две новые PG-таблицы** `service_registry` / `keeper_settings` ([keeper/storage.md](../keeper/storage.md)).
  - **`service.*`-permissions** ([naming-rules.md](../naming-rules.md), [rbac.md](../keeper/rbac.md)).
  - **Hard-cut config** — удалены `config.ServiceRegistryEntry` / `KeeperConfig.{Services,DefaultDestinySource,DefaultModuleSource}`; три ключа → `unknown_key`. Примеры [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) / [`dev/keeper.dev.yml`](../../dev/keeper.dev.yml) теряют блоки.
  - **Потребители** scenario (`ServiceRegistry` / `DestinySource`) читают `serviceregistry.Holder`; синхронный контракт `Resolve` неизменен.
  - **Operator API / MCP** получают `service.*`-эндпоинты ([operator-api.md](../keeper/operator-api.md), [mcp-tools.md](../keeper/mcp-tools.md)).

- **Trade-offs.**
  - **Снимок + инвалидация (best-effort + TTL-fallback)** — тот же компромисс, что у RBAC ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)) и `Summons` ([ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)): потеря pub/sub-сигнала → нода видит устаревший снимок до следующего TTL-перечита (короткое окно). Service-мутации редки — приемлемо.
  - **`keeper_settings` как нетипизированный key-value** — скаляры хранятся строками, без per-ключ колонок. Цена — нет SQL-типизации значения; выигрыш — новый well-known ключ не требует миграции таблицы. Принято (симметрично RAW-permission в ADR-028).
  - **Hard-cut без YAML→БД-миграции** — продакшена нет, легаси-инсталляций нет; импортёр `services:`-блока не нужен.

- **Срезы реализации.** S1 — storage (миграции `service_registry` + `keeper_settings`, repository, CRUD-Service). S2 — `serviceregistry.Holder` (синхронный снимок Resolve/DefaultDestinySource + Run/WatchInvalidations). S3 — OpenAPI/MCP `service.*`. **S4 — config hard-cut + переключение потребителей на Holder** (этот срез: удаление полей `config`, `scenario.ServiceRegistry`/`DestinySource` на Holder, daemon передаёт Holder вместо cfg).
