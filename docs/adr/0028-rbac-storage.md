# ADR-028. RBAC-storage → Postgres

> **Это дизайн, имплементации нет (Фаза 0 — документация).** ADR фиксирует перенос RBAC-хранилища из `keeper.yml` в Postgres и закрывает BUG-1 (init не пишет membership туда, откуда его читает enforcer). Кода новой схемы (миграции `rbac_*`, БД-резолв enforcer-а, init-membership) в репозитории нет — поэтапная реализация Фаза 0 → 4 ниже.

- **Контекст.** Сейчас RBAC-модель расщеплена между двумя хранилищами:
  - **Архонты** живут в Postgres (реестр `operators`, [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).
  - **Роли, их permissions и привязка «оператор ↔ роль»** декларируются в блоке `rbac:` файла `keeper.yml` ([rbac.md](../keeper/rbac.md), [config.md](../keeper/config.md)): `roles[].name` / `roles[].permissions` / `roles[].operators`.

  Это даёт **BUG-1 — самоблокировку первого Архонта**. `keeper init` ([ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)) создаёт первого Архонта в Postgres (`operators`-запись) и кладёт ему в JWT-claim `roles: ["cluster-admin"]` ([ADR-014(b)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). Но enforcer резолвит membership «AID → роли» **не из JWT-claim-а и не из БД, а из `keeper.yml::rbac.roles[].operators`** — куда `keeper init` ничего не пишет (он не редактирует YAML на диске). Результат: только что выпущенный bootstrap-Архонт при первом же API/MCP-вызове получает `403` — у enforcer-а нет ни одной роли, привязанной к его AID. Cluster-admin, созданный для разблокировки кластера, заблокирован сам. JWT-claim `roles` оказался **информационным**, а не авторитетным источником membership-а.

  Корень бага — **отсутствие персистентного слоя membership-а**: «оператор ↔ роль» нигде не материализована как запись, которую видят и `keeper init`, и enforcer на всех нодах кластера. YAML-список `roles[].operators` видит только тот инстанс, чей `keeper.yml` его содержит, и только после ручной правки файла + reload. Параллельно YAML-модель противоречит [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) («identity в Postgres ради hot-add + аудит + работающих FK»): half identity (Архонты) в БД, half (membership + роли) в файле.

  Решение пользователя — **вынести весь RBAC (роли, permissions, membership) в Postgres**, оставив `operators` как есть. propose-and-wait по всем именам пройден.

- **Решение.**

  **(a) Storage — три PG-таблицы с префиксом `rbac_*`.**

  | Таблица | Роль |
  |---|---|
  | **`rbac_roles`** | Каталог ролей. `name` PK (kebab-case, CHECK на формат), `description`, `builtin BOOL` (защита от `role.delete` / `role.update`), `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able (NULL — seed-роли без инициатора-Архонта). |
  | **`rbac_role_permissions`** | Permissions роли. `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `permission TEXT` (хранится **RAW-строкой**, парсится существующим `ParsePermission` — [rbac.md → Формат permissions](../keeper/rbac.md#формат-permissions)), PK `(role_name, permission)`. |
  | **`rbac_role_operators`** | **Membership** «роль ↔ оператор» — то, чего не хватало (причина BUG-1). `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `aid` FK→`operators(aid)`, `granted_at`, `granted_by_aid` FK→`operators(aid)` NULL-able, PK `(role_name, aid)`. |

  Permission хранится сырой строкой (не разложенной на колонки) — парсинг и матчинг остаются в Go (`ParsePermission`), БД таблицу не интерпретирует. Это сохраняет грамматику [rbac.md](../keeper/rbac.md) без изменений.

  **(b) cluster-admin — seed-миграция (E1) + `builtin`.** Seed-миграция вставляет роль `cluster-admin` с `builtin=true` и единственный permission `*` в `rbac_role_permissions`. `builtin=true` запрещает `role.delete` / `role.update` над ней (встроенная роль не редактируется и не удаляется — см. (e)). Это **E1** — роль с её permissions существует в БД до любого `keeper init`.

  **(c) `keeper init` пишет только membership (фикс BUG-1).** В своей advisory-lock-транзакции ([ADR-013(e)](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)) `keeper init` после создания `operators`-записи первого Архонта добавляет **одну строку membership-а** в `rbac_role_operators`: `(cluster-admin, <aid>)`. Роль и её permissions уже есть из seed-а (b). Это и есть фикс BUG-1: membership живёт там же, откуда его читает enforcer (БД), а не в JWT-claim-е, который enforcer не читал. JWT-claim `roles` ([ADR-014(b)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) перестаёт быть источником membership-а — авторитет membership-а исключительно `rbac_role_operators`.

  **(d) Enforcer — in-memory снимок из БД + B2-инвалидация.** Интерфейс `PermissionChecker.Check` **не меняется** — `Check` по-прежнему НЕ ходит в Postgres на каждый запрос. Меняется источник снимка: вместо парсинга `keeper.yml::rbac` enforcer строит снимок `map[AID][]*Role` тремя SELECT-ами (`rbac_roles` ⋈ `rbac_role_permissions` ⋈ `rbac_role_operators`). Обновление снимка — **B2 (реализовано)**: in-memory кэш + Redis pub/sub-инвалидация через топик **`rbac:invalidate`** (мутация роли/membership на любой ноде публикует envelope `{origin_kid, at}` → все ноды перечитывают снимок; **self-filter по KID** — публикующая нода игнорирует собственный сигнал, паттерн `applybus`) + TTL-fallback (периодический перечит на случай потери сигнала, по аналогии с `Summons` poll-fallback [ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). Это снимает старую зависимость RBAC от hot-reload-config-пути ([ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)): RBAC больше не перечитывается по `SIGHUP` / config-swap, а реагирует на БД-мутацию.

  **(e) API/MCP-permissions — четыре role-операции + membership-пара.** RBAC становится управляемым через OpenAPI/MCP (как Архонты в [ADR-014(d)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)), а не правкой YAML:
  - `role.create` — создать роль (+ её permissions).
  - `role.delete` — удалить роль (каскадом permissions + membership; запрещено над `builtin=true`).
  - `role.list` — перечислить роли с permissions и membership.
  - `role.update` — изменить permissions роли (запрещено над `builtin=true`).
  - `role.grant-operator` — добавить membership `(role, aid)` в `rbac_role_operators`.
  - `role.revoke-operator` — убрать membership.

  Имена закреплены пользователем (propose-and-wait пройден), вносятся в [naming-rules.md](../naming-rules.md) и каталог [rbac.md → Каталог permissions](../keeper/rbac.md#каталог-permissions).

  **(f) Self-lockout-инвариант расширяется на новые пути.** Инвариант [ADR-013(c)](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) / [rbac.md](../keeper/rbac.md) «нельзя оставить кластер без активного Архонта с эффективным `*`» теперь проверяется на трёх новых путях:
  - `role.delete` над `cluster-admin` (или любой ролью, дающей единственный путь к `*`);
  - `role.update`, убирающий permission `*` у роли, через которую держится единственный эффективный `*`;
  - `role.revoke-operator`, снимающий последнего активного AID с эффективным `*`.

  Все три перед мутацией проверяют «после операции останется ≥ 1 активный (`revoked_at IS NULL`) AID с эффективным `*`-permission»; иначе `409 would-lock-out-cluster` ([naming-rules.md → Error codes](../naming-rules.md#error-codes)).

  **(g) Config hard-cut.** Блок `rbac:` из `keeper.yml` **удаляется** — типы `config.KeeperRBAC` / `config.RBACRole` / поле `KeeperConfig.RBAC` уходят, ключ `rbac:` начинает отвергаться парсером как `unknown_key` ([naming-rules.md → Error codes](../naming-rules.md#error-codes)). Легаси-инсталляций нет (кода RBAC-резолва ещё нет — Фаза 0), миграционного слоя YAML→БД не требуется. `soul.yml` не затрагивается — у Soul нет ни PG, ни RBAC.

  **(h) Scope.** Этот ADR — **только RBAC**. Перенос остальных секций `keeper.yml` в БД (`services[]` / `default_*_source` и т.п.) — **НЕ** в этом решении; кандидат на будущий отдельный ADR (config→БД для прочих managed-сущностей), здесь не закрепляется. *(Закрыто [ADR-029](0029-service-registry.md#adr-029-реестр-service-ов--postgres): реестр Service-ов + `default_destiny_source` перенесены в Postgres тем же паттерном.)*

- **Обоснование.**
  - **Соответствие [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon).** «Identity в Postgres → hot-add + аудит + работающие FK» теперь распространяется на весь RBAC: membership и роли получают настоящие PG FK (`rbac_role_operators.aid` → `operators(aid)`, `created_by_aid` / `granted_by_aid` → `operators`), hot-add роли/привязки через API без правки файлов, аудит мутаций через FK + audit-events.
  - **Соответствие [ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) (stateless-кластер).** Общий Postgres виден всем нодам — membership одинаков на всём кластере без раскатки YAML по инстансам. YAML-модель давала per-host расхождение (один инстанс знает роль, другой нет до правки его файла).
  - **Безопасность на первом месте.** Ревокация роли / привязки — `DELETE` в БД + Redis-инвалидация, эффект мгновенный на всех нодах. В YAML-модели отзыв требовал правки файла + reload на каждом инстансе. Это устраняет окно «оператор уже не должен иметь доступ, но файл ещё не раскатан». (Отдельно от неотзываемости активного JWT до `exp` — [ADR-014(d)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon); membership-DELETE снимает роль для всех будущих проверок немедленно.)

- **Реконсиляция ADR.**
  - [ADR-013(c)](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) — `keeper init` пишет membership в `rbac_role_operators` (БД), не в YAML; прежняя формулировка «включение AID в `roles[].operators` через FK» уточнена: membership = строка `rbac_role_operators`.
  - [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) — RBAC-storage перенесён в БД; разведён термин «FK»: настоящий PG FK (`created_by_aid`, `rbac_role_operators.aid`) vs прежний метафорический «FK» YAML-списка `roles[].operators` (теперь — **membership**-строка в `rbac_role_operators`). JWT-claim `roles` перестаёт быть источником membership-а.
  - [ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml) — RBAC **исключён** из hot-reload-config-пути: reload RBAC = БД-мутация + Redis-инвалидация (d), не `SIGHUP` / config-swap. (Раздел [ADR-021 Consequences](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml) упоминал `keeper/internal/rbac.Holder` как eager-rebuild-consumer config-reload-а — после ADR-028 RBAC-holder перестраивается из БД-инвалидации, не из config-callback-а.)

- **Consequences.**
  - **Три новые PG-таблицы** `rbac_roles` / `rbac_role_permissions` / `rbac_role_operators` ([keeper/storage.md](../keeper/storage.md) дополняется).
  - **Шесть новых permissions** `role.create` / `role.delete` / `role.list` / `role.update` / `role.grant-operator` / `role.revoke-operator` ([naming-rules.md](../naming-rules.md), [rbac.md → Каталог permissions](../keeper/rbac.md#каталог-permissions)).
  - **Расширение self-lockout-инварианта** на `role.delete` / `role.update` / `role.revoke-operator` (f).
  - **Hard-cut config** — удаление `config.KeeperRBAC` / `config.RBACRole` / `KeeperConfig.RBAC`; `rbac:` → `unknown_key`. Пример [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) теряет блок `rbac:`.
  - **Enforcer** перестраивается под БД-снимок + B2-инвалидация; интерфейс `PermissionChecker.Check` неизменен.
  - **Operator API / MCP** получают `role.*`-эндпоинты ([operator-api.md](../keeper/operator-api.md), [mcp-tools.md](../keeper/mcp-tools.md)) — нормирование HTTP-фасада / MCP-tool-mapping отдельной задачей при имплементации.
  - **Audit-events** `role.created` / `role.deleted` / `role.updated` / `role.operator_granted` / `role.operator_revoked` — добавляются в [naming-rules.md → Audit-events](../naming-rules.md#audit-events) при имплементации write-path (категория `api` / `mcp`, [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); в этом ADR имена не закрепляются нормативно.

- **Поэтапность.**
  - **Фаза 0 — документация** (этот ADR + amend ADR-013/014/021 + naming-rules + rbac.md). Кода нет.
  - **Фаза 1 — миграции `rbac_*` + seed cluster-admin (E1) + enforcer на БД-снимок (без B2) + init пишет membership.** Закрывает **BUG-1**.
  - **Фаза 2 — Operator API / MCP `role.*` + расширенный self-lockout (f).**
  - **Фаза 3 — Redis pub/sub-инвалидация снимка (B2) — реализовано** (топик `rbac:invalidate`, envelope `{origin_kid, at}`, self-filter по KID; TTL-poll сохранён как fallback).
  - **Фаза 4 — удалить `config.KeeperRBAC` / `RBACRole` / `KeeperConfig.RBAC`, `rbac:` → `unknown_key` (g).**

- **Trade-offs.**
  - **Permission хранится RAW-строкой, не разложен на колонки.** БД не валидирует и не матчит permission (это делает `ParsePermission` в Go). Цена — нельзя SQL-фильтровать по `<resource>`/`<action>`; выигрыш — грамматика [rbac.md](../keeper/rbac.md) не дублируется в схему БД, расширение грамматики не требует миграции таблицы. Принято.
  - **B2-инвалидация (Redis pub/sub) — best-effort + TTL-fallback.** Потеря сигнала → нода видит устаревший снимок до следующего TTL-перечита (короткое окно). Тот же компромисс, что у `Summons` ([ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)); приемлемо для RBAC (мутации ролей редки, окно мало). Строгая немедленная согласованность всех нод — отвергнута как избыточная.
  - **Hard-cut без YAML→БД-миграции.** Принято осознанно — кода RBAC-резолва ещё нет (Фаза 0), легаси-инсталляций нет. Если бы инсталляции были — потребовался бы одноразовый импортёр `rbac:`-блока; здесь не нужен.

- **Amendment (2026-06-09, Synod — группа архонов [ADR-049](0049-synod.md#adr-049-synod--группа-архонов)).** Между «оператор» и «роль» вводится промежуточный уровень — **[Synod](0049-synod.md#adr-049-synod--группа-архонов)** (группа архонов, бандлящая роли). RBAC-storage расширяется **тремя PG-таблицами** тем же паттерном `rbac_*`:
  - **`synods`** — каталог групп (`name` PK kebab-CHECK, `description`, `builtin`, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able). Симметрия `rbac_roles`.
  - **`synod_operators`** — membership «Synod ↔ архон» (`synod_name` FK→`synods(name)` CASCADE, `aid` FK→`operators(aid)`, `added_at`, `added_by_aid` FK→`operators(aid)` NULL-able, PK `(synod_name, aid)`). Симметрия `rbac_role_operators`.
  - **`synod_roles`** — bundle «Synod ↔ роль» (`synod_name` FK→`synods(name)` CASCADE, `role_name` FK→`rbac_roles(name)` CASCADE, `granted_at`, `granted_by_aid` FK→`operators(aid)` NULL-able, PK `(synod_name, role_name)`). CASCADE с обеих сторон.

  **Snapshot-сборка enforcer-а (d) дополняется** двумя SELECT-ами (`synod_operators` ⋈ `synod_roles`): membership-map `AID → []*Role` объединяет прямые роли (`rbac_role_operators`) с ролями через все Synod-ы архона **до фиксации снимка**. Интерфейс `PermissionChecker.Check` и матчинг-слой **не меняются** — Synod невидим ниже сборки снимка. Redis-инвалидация (топик `rbac:invalidate`) покрывает мутации Synod/его membership/его bundle тем же путём, что мутации роли. **Self-lockout-инвариант (f) расширяется** на Synod-пути (`synod.delete` / `synod.revoke-role` / `synod.remove-operator`): проверка «останется ≥ 1 активный AID с эффективным `*`» разворачивает Synod-membership (эффективный `*` может приходить через Synod). Полная фиксация — [ADR-049](0049-synod.md#adr-049-synod--группа-архонов).
