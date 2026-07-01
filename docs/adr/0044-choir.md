# ADR-044. Choir — именованная топология хостов внутри инкарнации

> **Статус: реализовано (S-T2…S-T6 part(1a)).** Migration `060_create_choirs.up.sql` (таблицы `incarnation_choirs` + `incarnation_choir_voices`), пакеты `keeper/internal/choir` (CRUD) и `keeper/internal/topology` (резолвер роли Voice → declared → null fallback), REST `/v1/incarnations/{name}/choirs/*` с `RequirePermissionMulti`, RBAC permission `choir` (`keeper/internal/rbac/catalog.go`), OpenAPI-эндпоинты `/choirs` + схемы Choir/Voice, keeper-side core-модуль `core.choir` (`keeper/internal/coremod/choir`) + wire-up `ChoirDB`, B3 integration-тест NULL-role Voice (`topology/choir_nullrole_integration_test.go`). Остаток: S-T1 (типизированный input `source:`/`format: sid`) и S-T6-остаток (`on: [choir]` + формальный deprecate `spec.hosts[].role`) — открыты (slice-карта в конце ADR).

**Контекст.** К 2026-05-29 «кто и как стоит внутри инкарнации» размазано по двум разным механизмам без явной топологической оси:
- **membership** («какие хосты вообще принадлежат инкарнации») = `incarnation.name` в `souls.coven[]` (уже работает; резолвер `on:` опущен → весь incarnation через предикат `$1 = ANY(coven)`);
- **declared-роль** хоста (`incarnation.spec.hosts[].role`) — единственное место декларированной топологии, проектировалось под bootstrap-`create`, где probe невозможен ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)).

Чего нет: **именованной позиции хоста внутри инкарнации** — аналога Ansible host-group `[redis_nodes_1]` / `[haproxy_frontends]`. Оператор не может декларативно сказать «эти три SID — партия `redis_primary`, эти два — `redis_replica`» и затем таргетировать по этой партии. `spec.hosts[].role` даёт только одномерный ярлык на хост, без именованной группы как сущности, без CRUD, без таргетинга `where:`.

**Решение (зафиксировано пользователем 2026-05-29).** Ввести first-class сущность **Choir** — именованная группа хостов внутри одной инкарнации (топологическая «партия хора»), и **Voice** — членство конкретного SID в конкретном Choir.

1. **Три РАЗНЫХ слоя — не дублировать.** Choir не схлопывается ни с membership, ни с coven:
   - **membership** = `incarnation.name` в `souls.coven[]` (как было; не трогаем);
   - **coven** = стабильные логические теги ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги): кластер / проект / окружение / ЦОД);
   - **Choir** = именованная позиция хоста ВНУТРИ инкарнации. **Choir ≠ coven** (иначе это вернуло бы удалённый под-coven `{incarnation.name}-{role}` — прямой конфликт [ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)) и **≠ membership** (Choir — подмножество-роль внутри уже-членов, а не сам факт членства).

2. **Choir поглощает `spec.hosts[].role` (locked).** Declared-роль становится атрибутом членства — `voice.role`. `soulprint.hosts[].role` питается из Choir (через Voice), а не из `spec.hosts[].role`. `incarnation.spec.hosts[].role` — **вырожденный случай / deprecated**: остаётся для wire/spec-совместимости и bootstrap-`create`, но единственным источником declared-топологии становится Choir. Один источник правды declared-топологии — устраняется двойственность «роль в spec.hosts vs. где-то ещё».

3. **Мультиинкарнационность (locked).** Один хост (один SID) может входить в **несколько инкарнаций одновременно** — это допустимо и уже поддержано (`souls.coven[]` — массив; хост A может нести и `service-haproxy`, и `redis`, обе инкарнации содержат A). Voice привязан к **тройке `(incarnation_name, choir_name, sid)`**, поэтому один SID легально является Voice в Choir-ах **разных** инкарнаций одновременно. **Инвариант:** Voice создаётся только для SID, который **уже член этой инкарнации** (его `souls.coven[]` содержит `incarnation.name`) — Voice не подменяет membership, а уточняет позицию внутри него.

4. **Источник правды — отдельные PG-таблицы + CRUD-API, НЕ `incarnation.state` (эскиз; реализация — S-T2).** Финальные имена колонок и SQL фиксируются в S-T2 (пилот не падает). На S-T0 — каркас:
   - **`incarnation_choirs`** — declared-группа: `incarnation_name` (FK), `choir_name`, `description`, `min_size` / `max_size` (опц. ограничения размера партии), `created_by_aid` (FK `operators(aid)`), тайминги.
   - **`incarnation_choir_voices`** — членство SID в Choir: `incarnation_name`, `choir_name` (FK на пару выше), `sid` (FK `souls`), `role` (nullable — поглощённая declared-роль), `position` (nullable — порядковый индекс внутри партии, напр. seed-узел), `added_by_aid` (FK `operators(aid)`), тайминги.
   - **Почему не `incarnation.state`.** `state` коммитится **только под cross-host barrier** ([ADR-009 §7](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)) — топологию там нельзя поправить без полного прогона. `state` остаётся для **actual-результата** (кто фактически master после apply). **declared-топология (Choir) ≠ actual-state**: Choir декларирует «как ДОЛЖНО стоять», `state` фиксирует «как стало». Их смешение вернуло бы проблему волатильности из [ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги).

5. **Таргетинг — подход E1 (locked default).** Choir — **стабильный per-host факт** (как coven), доступный в `where:`-предикатах и в scenario-аксессорах `soulprint.hosts[].choirs` / `soulprint.self.choirs` (additive поле `choirs[]`, список имён Choir-ов хоста в текущей инкарнации). **`on: [choir]` в MVP НЕ вводится** — резолвер `on:` знает только coven-метки ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)), его контракт не трогаем. Сужение по Choir делается через `where:` (`'redis_primary' in soulprint.self.choirs`), что симметрично probe-роли из ADR-008, но без probe (Choir стабилен). `on: [choir]` — опциональное расширение S-T6.

6. **Правка топологии — гибрид.** Два пути изменения Choir/Voice, симметрично уже существующим механизмам:
   - **CRUD-API вне прогона** — как `soul.coven-assign` / `PATCH /hosts` (оператор правит топологию напрямую через OpenAPI/MCP);
   - **keeper-side core-модуль (правка-в-сценарии, `on: keeper`)** — диспетчер keeper-side core уже реализован ([ADR-015](0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список), [ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read), [docs/keeper/modules.md](../keeper/modules.md)). Это и есть отложенный ранее `core.incarnation.member`-паттерн. **Имя модуля — propose-and-wait при S-T5, в этом ADR НЕ закрепляется.**

7. **Отложенный membership-эпик НЕ запускается отдельно.** Ранее обсуждавшийся отдельный эпик «управление членством хостов в инкарнации» **функционально поглощается Choir**: membership остаётся `souls.coven[]`, а именованная позиция внутри — Choir/Voice. Отдельной сущности под membership не вводим.

8. **Типизированный input — отдельный ранний слайс (S-T1, независим).** Под выбор Choir-партий в сценариях нужен типизированный input-слой: дескриптор поля с источником-каталогом (напр. список SID инкарнации) + формат-валидатором `sid` + лимитами `min_items` / `max_items`. Имена ключей (кандидаты `source:` / `format: sid`) — **propose-and-wait при S-T1**, в этом ADR НЕ закрепляются (под-вопрос остаётся открытым).

9. **Slice-карта (инкрементальная раскатка).**

| Слайс | Содержание | Пилот не падает |
|---|---|---|
| **S-T0** ✅ DONE | Этот ADR + amendments + словарь (документация). | да |
| **S-T1** _(остаток)_ | Типизированный input (`source:` + `format: sid` + `min_items`/`max_items`; имена ключей propose-and-wait). Независим от Choir-таблиц. | да |
| **S-T2** ✅ DONE | Таблицы `incarnation_choirs` / `incarnation_choir_voices` (migration 060) + CRUD-API (`keeper/internal/choir`). | да |
| **S-T3** ✅ DONE | RBAC `choir.*` (`keeper/internal/rbac/catalog.go`) + audit `choir.*` + OpenAPI (роуты `RequirePermissionMulti`). | да |
| **S-T4** ✅ DONE | Резолвер `choirs[]` в `soulprint.hosts` / `soulprint.self` (`keeper/internal/topology`, E1-таргетинг через `where:`). | да |
| **S-T5** ✅ DONE | keeper-side core-модуль правки топологии **`core.choir`** (`keeper/internal/coremod/choir`) + wire-up `ChoirDB` — реализован, имя закреплено (под-вопрос закрыт). | да |
| **S-T6 part(1a)** ✅ DONE | Поглощение `spec.hosts[].role` в коде: резолвер берёт role из Voice с fallback на spec (см. amendment ниже). | да |
| **S-T6** _(опц., остаток)_ | `on: [choir]` (резолвер `on:`) + формальный deprecate `spec.hosts[].role`. | да |

**Отвергнутые альтернативы.**
- **(а) Choir = coven-метка.** Вернуло бы удалённый под-coven `{incarnation.name}-{role}` ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)) — прямой конфликт. Coven — глобальная стабильная ось; Choir — внутри-инкарнационная.
- **(б) Choir в `incarnation.state`.** state коммитится только под barrier ([ADR-009 §7](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)) — топологию нельзя править без прогона; смешивает declared с actual (пункт 4).
- **(в) `on: [choir]` сразу в MVP.** Расширяет контракт резолвера `on:` (знает только coven). E1 (`where:`-таргетинг) даёт ту же выразительность без правки резолвера; `on: [choir]` отложен до S-T6.
- **(г) Отдельный membership-эпик параллельно.** Дублировал бы Choir; поглощён (пункт 7).

**Открытые под-вопросы (propose-and-wait, НЕ закреплены этим ADR).**
- Имена ключей типизированного input (`source:` / `format: sid`) — при S-T1.
- ~~Имя keeper-side core-модуля правки топологии — при S-T5.~~ **Закрыто (S-T5):** модуль — `core.choir` (author-формы `core.choir.present` / `core.choir.absent`, симметрия present/absent остальных core-модулей).
- RBAC permission-имена семейства `choir.*` — при S-T3.

**Amendment (2026-05-29, precedence role + multi-Choir конфликт — S-T6 part(1a)).** Реализация поглощения declared-роли в резолвере (`keeper/internal/topology`) фиксирует два правила, не специфицированных ранее в пункте 2:

- **(a) Precedence role:** `voice.role` (из `incarnation_choir_voices`) **>** `spec.hosts[].role`. Резолвер для каждого хоста roster-а берёт role из Voice; `spec.hosts[].role` остаётся **fallback-ом** для хостов БЕЗ Voice (bootstrap-`create`, wire-совместимость) и для Voice с пустой/`NULL` role. `voice.role` nullable (миграция 060 — `TEXT` без `NOT NULL`): опущенная роль пишется как SQL `NULL` и трактуется как «нет роли» → fallback на spec (не ошибка).
- **(b) Multi-Choir конфликт role:** `HostFacts.Role` — скаляр, но один SID легально является Voice-ом в нескольких Choir-ах **одной** инкарнации с разными непустыми role. Детерминированное правило: берётся role из **первого по сортировке `choir_name` Choir-а с непустой role** (`ORDER BY choir_name ASC` в SQL-выборке Voice-ов; Choir-ы с пустой/`NULL` role пропускаются) + **WARN-лог** о конфликте (SID, выбранный/конфликтующий Choir и role). Если role пусты во всех Choir-ах SID — fallback на spec (пункт (a)). Скаляр-семантика role хоста и порядок имён внутри `soulprint.hosts[].choirs` тем самым детерминированы.

**Amendment (2026-06-30, scenario-driven раскладка + NULL-vs-default semantics).** Фиксирует источник Choir-присвоения и поведение при отсутствии присвоения — под mongo-use-case (6–9 VM разных ролей: шарды / координаторы / управляющие узлы деплоятся раздельно по группам). Ранее (пункты 4, 6) декларировался только механизм CRUD/keeper-side, но не источник-декларация в сценарии сервиса и не семантика пустого присвоения.

- **(a) Scenario-driven источник (declared в сценарии сервиса).** Раскладка хостов по Choir-группам **описывается прямо в сценарии сервиса**: сценарий декларативно говорит «этот хост → в такую-то партию» (аналог того, как Ansible-роль раскладывает хосты по host-group при деплое). Присвоение — **опциональное, per-host**. Это уточняет пункт 6: помимо day-2 правки (CRUD-API вне прогона / keeper-side `core.choir` в сценарии) сценарий-декларация — **первичный** способ разложить топологию при деплое сервиса. **Механика per-shard / per-role раскладки в сценарии (какой ключ сценария описывает Choir хоста, форма декларации) в этом ADR НЕ закрепляется — propose-and-wait при реализации.**

- **(b) Не задан → `NULL` в БД, НЕ дефолтная группа (locked).** Если для хоста Choir/Voice **не задан** (сценарий не разложил хост по партии и оператор не создал Voice вручную) — в `incarnation_choir_voices` для этого SID **нет строки**, `soulprint.self.choirs` / `soulprint.hosts[].choirs` для него **пустой список**, а роль (`HostFacts.Role`) резолвится fallback-ом на `spec.hosts[].role` либо остаётся пустой (по amendment 2026-05-29 (a)). **Хост НЕ попадает ни в какую «дефолтную»/«стандартную» партию по умолчанию** — пустое состояние сохраняется как есть.
  - **Обоснование:** пустое состояние **честнее** дефолта — оно не навязывает хосту группу, которую оператор не выбирал, и не создаёт иллюзию declared-топологии там, где её нет. Дефолтная партия скрыла бы факт «этот хост нигде не размечен» и потребовала бы зарезервированного имени-партии (лишняя сущность, конфликт с реальными именами Choir-ов сервиса). NULL-семантика симметрична `voice.role` (пункт (a) amendment 2026-05-29: опущенная роль = SQL `NULL` = «нет роли», не дефолт) и `soulprint.hosts[].role` (может быть `null` для хостов вне declared-spec, [ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)).
  - **Частичное присвоение.** Часть хостов с Choir, часть без → размеченные живут в своих партиях, остальные — `NULL` (пустой `choirs[]`). Смешанное состояние — норма, не ошибка.

- **(c) Статус реализации: per-shard/per-role раскладка — DEFERRED до mongo.** Scenario-driven раскладка по партиям по-настоящему нужна для **mongo** (раздельный деплой шардов / координаторов / управляющих узлов). Для **redis** Choir используется «для удобства», раскладка per-shard **не требуется**: сейчас redis cluster живёт в одной партии = один Coven `incarnation.name` ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги), roster хостов инкарнации без разбиения по Choir). Этот amendment фиксирует **semantics** (scenario-driven источник + NULL-vs-default); **реализация scenario-декларации раскладки по партиям — planned/deferred** (post-redis, к появлению mongo-сервиса). Инфраструктура Choir/Voice (таблицы, CRUD, `core.choir`, резолвер `choirs[]`) уже реализована (S-T2…S-T6 part(1a)) и покрывает day-2 присвоение; недостающая часть — именно scenario-декларация при деплое.
