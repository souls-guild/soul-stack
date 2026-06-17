# ADR-049. Synod — группа архонов

> **Статус: реализован (2026-06-09/10), в baseline.** Имя **Synod** выбрано пользователем (propose-and-wait пройден); дизайн модели — architect. Реализация **полностью закрыта** — бэкенд (миграции `synods` / `synod_operators` / `synod_roles`, дополнение snapshot-сборки enforcer-а, security-инварианты (f), API `/v1/synods*` со всеми permission-ами семьи `synod.*`, включая `PATCH /v1/synods/{name}` для редактирования `description` — amend ниже) и UI. Membership «оператор ↔ роль» теперь трёхуровневый: прямые строки `rbac_role_operators` ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)) ∪ роли через Synod.

**Контекст.** RBAC-модель сейчас двухуровневая: **Архон → Роли** (membership-строки `rbac_role_operators`, [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)). Чтобы выдать N архонам одинаковый набор ролей, оператор гранитит каждому каждую роль вручную — `rbac_role_operators` растёт как `архоны × роли`, а смена «стандартного набора прав команды» требует пройтись по всем её членам. Нет именованной единицы «набор ролей для группы операторов», которую можно завести один раз и добавлять/убирать в неё архонов. Это типовой gap RBAC: между «оператор» и «роль» не хватает **группы операторов**.

**Решение.** Вводится сущность **Synod** (греч. «собрание») — **группа архонов**, бандлящая набор ролей. Модель становится трёхуровневой: **Архон → Synod → Роли**.

> **Имя выбрано пользователем (2026-06-09).** `Conclave` — синоним «собрания», но **занят** реестром живых Keeper-инстансов ([ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis) amendment) — переиспользовать нельзя. `Synod` свободно и держит «греческо-административную» линию рядом с [Archon](0014-operator-identity.md#adr-014-identity-модель-оператора-archon).

**(a) Synod бандлит роли, своего scope НЕ имеет.** Synod несёт **набор ролей** — и только. Собственного `default_scope` у Synod **нет**: scope живёт на ролях ([ADR-047(а)](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор) `rbac_roles.default_scope` + per-perm-селектор) — Synod ничего к scope-резолву не добавляет, он лишь группирует уже-scoped роли. Член Synod получает роли Synod-а ровно с тем scope, который задан на самих ролях. **Group-scope (отдельный scope на Synod, сужающий роли группы) НЕ вводится** — additive-расширение на будущее (см. «Что отложено»), не часть MVP.

**(b) Плоские группы.** Synod **НЕ содержит другие Synod** — вложенности нет (плоская модель). Член Synod — только Архон, не Synod. Транзитивного раскрытия групп в группах нет; эффективные роли считаются за один уровень группировки. Вложенность — additive-расширение на будущее (см. «Что отложено»).

**(c) Архон может входить в несколько Synod.** Membership «архон ↔ Synod» — many-to-many. Эффективные роли архона = **прямые** (`rbac_role_operators`) **∪** роли через **все** его Synod-ы (объединение по `synod_operators` ⋈ `synod_roles`). Дубль роли (через прямой грант И через Synod, либо через два Synod-а) идемпотентен — это союз множеств, не мультимножество.

**(d) Модель данных — три PG-таблицы `synod*`** (паттерн `rbac_*` из [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)):

| Таблица | Роль |
|---|---|
| **`synods`** | Каталог групп. `name` PK (kebab-case, CHECK на формат — как `rbac_roles.name`), `description`, `builtin` BOOL (защита от `synod.delete`, симметрия `rbac_roles.builtin`), `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able (NULL — seed-группы без инициатора-Архонта). |
| **`synod_operators`** | **Membership** «Synod ↔ архон». `synod_name` FK→`synods(name)` `ON DELETE CASCADE`, `aid` FK→`operators(aid)`, `added_at`, `added_by_aid` FK→`operators(aid)` NULL-able, PK `(synod_name, aid)`. |
| **`synod_roles`** | **Bundle** «Synod ↔ роль». `synod_name` FK→`synods(name)` `ON DELETE CASCADE`, `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `granted_at`, `granted_by_aid` FK→`operators(aid)` NULL-able, PK `(synod_name, role_name)`. CASCADE с обеих сторон: удаление Synod чистит свои bundle-строки, удаление роли ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres) `role.delete`) — снимает её из всех Synod-ов. |

Симметрия с RBAC-таблицами полная: `synods` ↔ `rbac_roles` (каталог + `builtin`), `synod_operators` ↔ `rbac_role_operators` (membership), `synod_roles` — новый bundle-уровень.

**(e) Резолв — дополнение snapshot-сборки, матчинг не трогается.** Эффективные роли архона = прямые ∪ через Synod **собираются на этапе сборки in-memory-снимка enforcer-а** ([ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres) `LoadSnapshot`): к трём SELECT-ам (`rbac_roles` ⋈ `rbac_role_permissions` ⋈ `rbac_role_operators`) добавляются два (`synod_operators` ⋈ `synod_roles`), и membership-map `AID → []*Role` **дополняется** ролями через Synod до того, как снимок зафиксирован. Дальше — без изменений: `Check` / `ResolvePurview` / `HoldsAction` ([ADR-047](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)) видят уже-объединённый набор ролей и **матчинг-слой Purview не меняется** — Synod невидим ниже сборки снимка. Инвалидация снимка ([ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres) топик `rbac:invalidate`) — на мутацию Synod/его membership/его bundle тем же путём, что на мутацию роли.

**(f) Security-инвариант — least-privilege и self-lockout ОБЯЗАНЫ учитывать роли через Synod.** Это критическая точка: оба инварианта считают «эффективные права оператора», и оба сломаются, если посчитают только прямые роли.

- **Least-privilege subset-check** ([`subset.go`](../../keeper/internal/rbac/subset.go), [ADR-047 §Грабли](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)): «выдаёшь не шире собственных прав». Если subset-проверка для `synod.grant-role` / `synod.add-operator` (и для обычных `role.grant-operator`) сравнивает выдаваемое право только с **прямыми** ролями инициатора, оператор, чьи права частично приходят через Synod, ложно получит deny **или** — что хуже — проверка «свои права» недосчитает их и пропустит выдачу права, которое инициатор сам не держит напрямую. Эффективные права инициатора в subset-check = прямые ∪ через Synod.
- **Self-lockout-инвариант** ([ADR-013(c)](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)/[ADR-028(f)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)): «нельзя оставить кластер без активного AID с эффективным `*`». `*` теперь может приходить архону **через Synod** (Synod бандлит роль с `*`). Проверка «останется ≥ 1 активный AID с эффективным `*`» обязана разворачивать Synod-membership — иначе `synod.remove-operator` / `synod.revoke-role` / `synod.delete`, снимающие последний путь к `*`, залочат кластер незамеченными. Поэтому к трём путям ADR-028(f) (`role.delete` / `role.update` / `role.revoke-operator`) добавляются Synod-пути: `synod.delete`, `synod.revoke-role`, `synod.remove-operator` — каждый перед мутацией проверяет инвариант с учётом Synod-разворота; иначе `409 would-lock-out-cluster`.

**(g) API/MCP + permission-семейство `synod.*`.** Управление через OpenAPI/MCP (паттерн `role.*` [ADR-028(e)](0028-rbac-storage.md#adr-028-rbac-storage--postgres), `operator.*` [ADR-014(d)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)):

- `synod.create` — создать группу (+ описание).
- `synod.delete` — удалить группу (каскадом membership + bundle; запрещено над `builtin=true`; self-lockout-guard (f)).
- `synod.list` — перечислить группы с членами и ролями.
- `synod.add-operator` — добавить архона в группу (`synod_operators`); под least-privilege subset (f).
- `synod.remove-operator` — убрать архона из группы (self-lockout-guard (f)).
- `synod.grant-role` — добавить роль в bundle группы (`synod_roles`); под least-privilege subset (f).
- `synod.revoke-role` — убрать роль из bundle (self-lockout-guard (f)).

Эндпоинты: `/v1/synods` (`synod.create` POST / `synod.list` GET / `synod.delete` DELETE `{name}`), `/v1/synods/{name}/operators` (`synod.add-operator` POST / `synod.remove-operator` DELETE `{aid}`), `/v1/synods/{name}/roles` (`synod.grant-role` POST / `synod.revoke-role` DELETE `{role_name}`). Permission-семейство `synod.*` — **NoSelector** (управление группами — кластер-уровневая операция, не scoped по coven/host; как `role.*` и `operator.*`). HTTP-фасад / MCP-tool-mapping нормируется отдельной задачей при имплементации (паттерн ADR-028(e)).

**Amendment (2026-06-09, редактирование Synod).** Synod **мутабельный по `description`**: добавляется permission **`synod.update`** и эндпоинт **`PATCH /v1/synods/{name}`** (MCP-tool `keeper.synod.update`), меняющий **ТОЛЬКО `description`**. Семья `synod.*` — 8 permissions. **`name` (PK) IMMUTABLE — переименование сознательно ОТВЕРГНУТО** (симметрия с `rbac_roles.name` / `souls.sid` / `incarnation.name`: rename PK дал бы audit-drift — старое имя в `audit_log` теряет связь со строкой; окно рассинхрона enforcer-снимка — `synod_operators`/`synod_roles` ссылаются на `synod_name`; асимметрию с `role.update`, который тоже не переименовывает роль). `synod.update` — **NoSelector** (как остальная семья); **builtin РАЗРЕШЁН к правке** (`description` — косметика для UI/аудита, не поведение: прав не выдаёт/не отнимает, поэтому builtin-граница `synod.delete` сюда не распространяется); **без subset-check и self-lockout** (description прав не меняет — оба инварианта (f) неприменимы); **без инвалидации снимка** (enforcer-снимок (e) несёт только `name`/роли/membership — `description` в матчинг не входит, авторизация от его правки не меняется). Audit-event **`synod.updated`** (`api`/`mcp`, payload `{name, description}`), параллельно HTTP/MCP-write-path-у (мутация RBAC-топологии аудируется, [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)).

**Обоснование.**
- **Соответствие [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)/[ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres).** Synod ложится тем же паттерном, что RBAC-storage: PG-таблицы с настоящими FK на `operators`/`rbac_roles`, hot-add через API, аудит через FK + audit-events, снимок в enforcer-е + Redis-инвалидация. Никакой новой инфраструктуры — переиспользование снимок-машины [ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres).
- **Безопасность на первом месте.** Группа — управляемая единица отзыва: `synod.remove-operator` снимает весь набор ролей группы с архона одной мутацией (+ Redis-инвалидация, эффект мгновенный на всех нодах). Но та же мощь требует, чтобы оба security-инварианта (f) разворачивали Synod — это явный инвариант ADR, не «деталь имплементации».

**Consequences.**
- **Три новые PG-таблицы** `synods` / `synod_operators` / `synod_roles` ([keeper/storage.md](../keeper/storage.md) дополняется при имплементации).
- **Permission-семейство `synod.*`** (8 permissions, вкл. `synod.update` — amend 2026-06-09) — [naming-rules.md](../naming-rules.md), каталог [rbac.md → Каталог permissions](../keeper/rbac.md#каталог-permissions) (docs-writer).
- **Snapshot-сборка enforcer-а** дополняется двумя SELECT-ами и шагом объединения membership-map; интерфейсы `Check` / `ResolvePurview` / `HoldsAction` **не меняются**.
- **Self-lockout-инвариант** расширяется на `synod.delete` / `synod.revoke-role` / `synod.remove-operator` (f); **least-privilege subset** — на `synod.grant-role` / `synod.add-operator` (f).
- **Audit-events** `synod.created` / `synod.updated` (amend) / `synod.deleted` / `synod.operator-added` / `synod.operator-removed` / `synod.role-granted` / `synod.role-revoked` (категория `api` / `mcp`, [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) — [naming-rules.md → Audit-events](../naming-rules.md#audit-events); нормируются PR-ом при имплементации write-path.

**Связь с ADR.**
- **[ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)** — эффективные роли архона = прямые ∪ через Synod (амендмент).
- **[ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)** — +3 таблицы Synod, snapshot-сборка дополняется групповыми ролями, инвалидация `rbac:invalidate` покрывает мутации Synod (амендмент).
- **[ADR-047](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)** — эффективные роли резолвятся с учётом Synod; group-scope НЕ вводится, scope живёт на ролях (амендмент).
- **[ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)** — self-lockout-инвариант разворачивает Synod-membership (f).

**Отвергнутые альтернативы.**
- **(а) `default_scope` на Synod (group-scope).** Дал бы «сузить роли группы общим scope». Отвергнут в MVP: scope уже живёт на ролях ([ADR-047](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)), второй scope-слой на группе усложняет резолв (пересечение group-scope ∩ role-scope) и матчинг Purview без подтверждённой потребности. Additive — вводится позже без breaking change (новая nullable-колонка `synods.default_scope`).
- **(б) Вложенные Synod (группа в группе).** Транзитивный разворот, риск циклов, более дорогой snapshot-build. Отвергнут в MVP — плоская модель покрывает «набор ролей для команды». Additive при реальном запросе.
- **(в) Хранить membership группы в JWT-claim.** Повторило бы BUG-1 ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)): claim информационен, enforcer резолвит из БД-снимка. Synod-membership — строки `synod_operators`, авторитет — БД, не claim.
- **(г) Переименование Synod (`name` PK mutable).** `synod.update`, меняющий `name`, отвергнут (amend 2026-06-09): rename PK даёт audit-drift (старое имя в `audit_log` теряет связь со строкой), окно рассинхрона enforcer-снимка (`synod_operators`/`synod_roles` ссылаются на `synod_name`) и асимметрию с `role.update` (роль тоже не переименовывается). `synod.update` меняет **только `description`** — name immutable, как у `rbac_roles.name` / `souls.sid` / `incarnation.name`. Если переименование когда-нибудь понадобится — отдельная транзакционная операция (rename + каскадный rewrite FK + audit-trail), не часть `synod.update`.

**Что отложено (post-MVP / backlog, additive).**
- **Group-scope** (`synods.default_scope`, сужающий роли группы) — (а).
- **Вложенные Synod** (группа в группе) — (б).
- Оба расширяемы без breaking change поверх трёх MVP-таблиц.
