# ADR-0068. Апгрейд инкарнаций до новой версии сервиса — `upgrade/`-папка + upgrade-paths API

> **Статус: accepted (реализован — NIM-34, влит на канон 2026-07-05).** Дизайн утверждён пользователем (2026-07-03). Расширяет существующий `POST /v1/incarnations/{name}/upgrade` (сегодня — смена пина + state-миграции [ADR-019](0019-state-migration-dsl.md) → `drift`) опциональной оркестрацией перехода на хостах. **Amends [ADR-019](0019-state-migration-dsl.md)** (upgrade получает вторую фазу — host-side upgrade-сценарий) **/ [ADR-009](0009-scenario-dsl.md)** (второй канал авто-дискавери сценариев: каталог `upgrade/`). Related: [ADR-007](0007-versioning-git-ref.md) (версия = git-ref), [ADR-057](0057-state-changes-crud-verbs.md) (истина day-2 = `incarnation.state`), [ADR-065](0065-core-module-installed.md) (симметрия `create: true` / self-describing манифест), [ADR-043](0043-voyage.md) (массовый апгрейд — future work). Impl — тикет NIM-34.

## Контекст

Версионирование Soul Stack уже полностью ref-based ([ADR-007](0007-versioning-git-ref.md)): версия сервиса — git-tag, поле `version:` в манифестах запрещено. Пин конкретной инкарнации — `incarnation.service_version` — **захватывается на create** из резолвнутого `service_registry.ref` ([`keeper/internal/api/handlers/incarnation_typed.go:141`](../../keeper/internal/api/handlers/incarnation_typed.go), [`keeper/internal/mcp/incarnation_create.go:179`](../../keeper/internal/mcp/incarnation_create.go)) и **форсится вместо HEAD реестра во все run/render-пути** ([`scenario/run.go:297`](../../keeper/internal/scenario/run.go), [`scenario/checkdrift.go:288`](../../keeper/internal/scenario/checkdrift.go), [`scenario/render_host.go:128`](../../keeper/internal/scenario/render_host.go), [`render/cel_render.go:161`](../../keeper/internal/render/cel_render.go), [`render/dispatch.go:152`](../../keeper/internal/render/dispatch.go), teardown — [`incarnation/destroy_prepare.go:92`](../../keeper/internal/incarnation/destroy_prepare.go)). Кеш артефактов sha1-адресуемый → версии сосуществуют. Выбор версии на create/day-2-формах запрещён (**анти-version-craft** инвариант: формы всегда на `inc.ServiceVersion`, [`api/huma_incarnation_formprefill.go:23`](../../keeper/internal/api/huma_incarnation_formprefill.go), [`api/handlers/incarnation_formprefill.go:53`](../../keeper/internal/api/handlers/incarnation_formprefill.go)).

**Апгрейд сегодня.** `POST /v1/incarnations/{name}/upgrade` с `to_version` ([`api/handlers/incarnation_typed.go:449`](../../keeper/internal/api/handlers/incarnation_typed.go) `UpgradeTyped`, роут [`api/huma_incarnation.go:118`](../../keeper/internal/api/huma_incarnation.go)): `PrepareUpgrade` резолвит целевой снапшот, ловит no-op/downgrade, собирает цепочку state-миграций ([`incarnation/upgrade_prepare.go:74`](../../keeper/internal/incarnation/upgrade_prepare.go)) → `UpgradeStateSchema`/`upgradeTx` одной PG-tx применяет миграции + меняет `service_version` + ставит `status=drift` ([`incarnation/crud.go:1505`](../../keeper/internal/incarnation/crud.go), финальный UPDATE — `crud.go:1635`). **Хосты при этом не трогаются**: upgrade заканчивается в информационном `drift` (ADR-031, amendment 2026-06-27, `crud.go:1443` `upgrade-pending-apply`), а раскатку новой версии оператор доводит **обычным apply** вручную. Возвращаемый `apply_id` — ULID upgrade-операции для истории миграции, а **не** реальный Runner-прогон.

Пробел: для нетривиальных переходов между версиями (смена схемы деплоя, переезд данных, изменение раскладки) «оператор сам накатит apply» — недостаточно. Нужен **специализированный, версия-к-версии, оркестрируемый переход**, запускаемый апгрейдом, но при этом не пугающий оператора в обычных day-2-списках и удобный для ретрая. Это расширение и фиксирует ADR.

## Решение

### 1. Что НЕ делаем (границы, сохранённые инварианты)

- **Semver-парсинг тегов — нет.** Резолвер версий остаётся `git checkout <ref>` ([ADR-007](0007-versioning-git-ref.md)); никаких range/`>=`.
- **Double-pin commit-sha — нет.** Пин = git-ref, как есть.
- **Выбор версии при create — нет.** Анти-version-craft инвариант сохраняется: create/day-2-формы всегда на `inc.ServiceVersion`. Единственное легальное место смены версии — **действие upgrade** (и его discovery — §6), а не создание.

### 2. Upgrade-сценарий опционален

Апгрейд работает и без единого upgrade-сценария. Если для перехода `from→to` upgrade-сценарий не найден — поведение **ровно сегодняшнее** (§5, ветвь legacy): смена пина + state-миграции [ADR-019](0019-state-migration-dsl.md) + `drift`. Upgrade-сценарий — это **дополнительная** способность автора сервиса, не обязательство.

### 3. Upgrade-сценарий живёт в отдельной папке `upgrade/<slug>/`, self-describing `from:`

Файлы апгрейд-сценариев — в **отдельном top-level каталоге сервис-репо** `upgrade/<slug>/main.yml` в дереве **НОВОЙ** версии, НЕ в общем `scenario/`. Причины: (а) в day-2-списках сценариев они пугали бы оператора («что это за операция, можно ли её запускать руками?»); (б) отдельная папка даёт естественное разделение и удобный ретрай.

Манифест `upgrade/<slug>/main.yml` — **self-describing**: top-level поле `from:` — список исходных версий-тегов, из которых этот сценарий умеет апгрейдить:

```yaml
# upgrade/v2/main.yml  (в дереве версии v2.x)
from: ["v1.0.0", "v1.2.0"]
description: Переход redis sentinel→cluster при апгрейде на v2
# ... обычные задачи сценария: module: / apply: / state_changes / on: / where: ...
```

- **Симметрия с `create: true`** ([ADR-065](0065-core-module-installed.md)-стиль self-describing манифеста): как `create: true` объявляет «этот сценарий годен как стартовый», так `from: [...]` объявляет «этот сценарий годен как переход из этих версий». Дискриминатор живёт **в самом файле сценария**, а не в реестре.
- **Блока `upgrade:` в `service.yml` НЕТ.** Работает канон авто-дискавери [ADR-009](0009-scenario-dsl.md): сценарии не перечисляются в манифесте, keeper находит их сканом каталога. upgrade/-сценарии — второй такой каталог рядом со `scenario/`.
- **upgrade/ не показывается в обычных day-2-списках** (`GET /v1/services/{name}/scenarios`, [`api/handlers/service.go:481`](../../keeper/internal/api/handlers/service.go)) — они видны только через upgrade-контур (§6).

### 4. Направление «`from` в новой версии», а не «`to` в старой»

`from:` объявляется в **новой** версии (куда апгрейдим), перечисляя старые версии-источники. Обоснование:

- **Теги immutable** ([ADR-007](0007-versioning-git-ref.md)): в момент выпуска `v1.0.0` будущие версии неизвестны — задекларировать «`to: v2`» в старом теге физически нельзя, тег уже заморожен. Новая версия же знает все свои валидные источники.
- **Симметрия с forward-only** [ADR-019](0019-state-migration-dsl.md): миграции пишутся вперёд (`<NNN>_to_<MMM>`, from<to), апгрейд — тоже forward. `upgrade/` наследует ту же ось «новое знает про старое».

### 5. Апгрейд — 2 ветви по факту наличия upgrade-сценария для `from→to`

Резолв: в фазе `PrepareUpgrade` (до смены пина, пока `inc.ServiceVersion` = старая версия) просканировать `upgrade/*/main.yml` **целевого** снапшота и найти сценарий, чей `from:` содержит текущий `inc.ServiceVersion`.

- **found** → сменить пин + прогнать state-миграции ([ADR-019](0019-state-migration-dsl.md), существующий `upgradeTx`) + **автозапуск** найденного upgrade-сценария через `Runner.Start` ([`scenario/runner.go:25`](../../keeper/internal/scenario/runner.go), `RunSpec` — [`scenario/scenario.go:211`](../../keeper/internal/scenario/scenario.go)) с `ServiceRef`, запиненным на **новый** `to_version`. Upgrade-сценарий раскатывает переход на хостах и своими `state_changes` доводит `incarnation.state`. Успех → `ready`. **Падение → `error_locked` → разблокировка через `rerun-last`** (как обычный сценарий: `UnlockForRerun` → `RunSpec.FromLocked`, [`scenario/scenario.go:225`](../../keeper/internal/scenario/scenario.go)); ретрай гоняет тот же upgrade-сценарий против уже-поднятого пина.
- **not-found** → **legacy**: только смена пина + state-миграции [ADR-019](0019-state-migration-dsl.md) + **WARN** в лог/ответ + `drift` (ровно сегодняшнее поведение). Оператор доводит раскаткой обычным apply.

**★ Fail-closed (422 на незадекларированный переход) — ОТБРОШЕН осознанно.** Каталог `upgrade/` **наследуется вниз по git-дереву патчей**: тег `v2.0.1` несёт те же `upgrade/v2/` (with `from: [v1.*]`), что и `v2.0.0`. Запрет undeclared-переходов ломал бы невинные патч-апгрейды `v2.0.0→v2.0.1`, для которых никто не станет писать `from: [v2.0.0]`. Поэтому «нет upgrade-сценария → падаем» неверно; корректно «нет upgrade-сценария → legacy-путь». Плата: незадекларированный «большой» переход тихо уйдёт в legacy без host-оркестрации — это ловит WARN + upgrade-paths (§6), не 422.

Транзакционная граница: state-миграция ([ADR-019](0019-state-migration-dsl.md), одна PG-tx) остаётся атомарной и коммитится ПЕРВОЙ; upgrade-сценарий — отдельный последующий Runner-прогон. Если сценарий упал — пин уже поднят и state уже мигрирован; это консистентно с forward-only + `error_locked` + `rerun-last` (ретрай не откатывает пин, догоняет раскатку). Found-ветвь при этом обязана **зарезервировать `applying`** и передать управление Runner-у вместо финального `drift` (impl-развилка §Scope: `upgradeTx` в found-режиме ставит `applying`, а не `drift`).

### 6. Discoverability — keeper перечисляет, а не оператор: `GET /v1/incarnations/{name}/upgrade-paths`

Возражение к §4 «`from` в новой версии не даёт видимости, куда переходить» снимается тем, что **версии перечисляет keeper**, а не оператор наизусть.

**Выбранный путь — incarnation-scoped: `GET /v1/incarnations/{name}/upgrade-paths`** (обоснование выбора — ниже).

- **Без `?to=` — дёшево**: перечисление тегов реестра сервиса (реюз `ls-remote`, тот же источник, что `GET /v1/services/{name}/refs` → `ListRefsTyped`, [`api/handlers/service.go:430`](../../keeper/internal/api/handlers/service.go)) с пометкой `is_current` (тег == текущий пин инкарнации). **Направление (forward/downgrade) в дешёвом режиме НЕ вычисляется**: [ADR-007](0007-versioning-git-ref.md) запрещает semver-парсинг имён тегов — по именам направление недостоверно, поэтому точное направление / found-legacy / миграции вынесены в `?to=`.
- **С `?to=<ref>` — on-demand анализ конкретной цели**: `direction` — **четыре значения** (`no-op` / `downgrade` / `forward` / `same-schema` — ref-bump без смены схемы); `mode` (found vs legacy §5) считается только для forward/same-schema; применяемые state-миграции [ADR-019](0019-state-migration-dsl.md) (реюз `LoadMigrationChain` из `PrepareUpgrade`). Дорогой per-target анализ не гоняется на весь список — только по запросу. **Битая цепочка миграций — не HTTP-ошибка (422), а `200` с `reachable: false` + `unreachable_reason`**: preview-эндпоинт отдаёт недостижимую цель как ДАННЫЕ (UI рисует серой), достижимые цели — `reachable: true`.
- UI рисует выпадашку «на что и как могу обновиться» (метка found/legacy у цели анализируется on-demand через `?to=`).

**Почему incarnation-scoped, а не `/v1/services/{name}/upgrade-paths`:** ответ «куда и КАК могу перейти» неизбежно опирается на **`from` = текущий пин конкретной инкарнации** (`inc.ServiceVersion`) — у сервиса нет единой «текущей версии», у каждой инкарнации свой пин. Обе дорогие части анализа — сопоставление `from:` upgrade-сценария и расчёт применимых state-миграций — требуют `inc.ServiceVersion` + `inc.state_schema_version`, известных серверу только per-incarnation. Плюс симметрия с действием, которому upgrade-paths предшествует: `POST /v1/incarnations/{name}/upgrade` тоже incarnation-scoped, и UI Upgrade-modal открывается из инкарнации. Service-scoped вариант заставил бы клиента слать `from` query-параметром (избыточно — сервер его знает — и хрупко). Существующий `GET /v1/services/{name}/refs` (service-scoped сырое перечисление тегов, уже помечен «парный /refs для Upgrade-modal», [`api/handlers/service.go:483`](../../keeper/internal/api/handlers/service.go)) остаётся низкоуровневым строительным блоком, поверх которого upgrade-paths добавляет incarnation-контекст.

Permission — реюз `incarnation.upgrade` (та же операция, read-грань) либо `incarnation.get`; выбор — на impl (§Scope). READ, без audit.

> *Амендмент 2026-07-04 (NIM-34 impl): §6 сверен с реализацией.* Дешёвый режим сведён к пометке `is_current` — направление по именам тегов НЕ вычисляется ([ADR-007](0007-versioning-git-ref.md)). `?to=` даёт `direction` ∈ {`no-op`, `downgrade`, `forward`, `same-schema`}, а недостижимую по битой цепочке миграций цель сигналит `reachable: false` + `unreachable_reason` (200, не 422). Wire-контракт — [operator-api/incarnations.md](../keeper/operator-api/incarnations.md); enum-словарь — [naming-rules.md](../naming-rules.md#upgrade-v2-каталог-upgrade-ключ-from-upgrade-paths).

### 7. Канон input — граница: input НЕ мигрируется

`spec.input` = **write-once рецепт создания** (используется `rerun-last` по create-ветке + аудит), истина желаемого состояния = `incarnation.state` (amendment [ADR-057](0057-state-changes-crud-verbs.md), day-2 читает `state`, не `input`/`essence`). **При апгрейде `input` НЕ мигрируется** и НЕ является источником day-2: он — pre-state seed момента create, а не желаемое состояние. Upgrade-сценарий работает с `incarnation.state` (читает развёрнутый факт, пишет через `state_changes`) и `essence` целевой версии — как любой day-2-сценарий. Явная граница: апгрейд может изменить форму `state` (через [ADR-019](0019-state-migration-dsl.md)-миграции) и содержимое (через `state_changes` upgrade-сценария), но `spec.input` остаётся историческим слепком create и не переписывается.

### 8. Non-goals MVP

- **Авто-чейнинг `v1→v3`** (композиция цепочки upgrade-сценариев через промежуточные версии) — нет; MVP резолвит один прямой `from→to`.
- **glob/semver в `from:`** — нет; `from:` — точный список immutable-тегов.
- **Массовый апгрейд скоупа** — отдельный тикет NIM-35 через Voyage `kind=upgrade` ([ADR-043](0043-voyage.md)); здесь только упоминается как future work.

## Что уже существует vs что предстоит построить (scope NIM-34)

**Существует и переиспользуется:**
- `POST /v1/incarnations/{name}/upgrade` + `UpgradeTyped`/`PrepareUpgrade`/`UpgradeStateSchema`/`upgradeTx` (смена пина + [ADR-019](0019-state-migration-dsl.md)-миграции + `drift`).
- `Runner.Start` + `RunSpec` (запуск сценария), `error_locked` + `rerun-last` (`FromLocked`) — вся machinery ветви found уже есть.
- `ls-remote` перечисление тегов (`ListRefsTyped`) и `LoadMigrationChain` — строительные блоки upgrade-paths.
- Авто-дискавери сценариев + self-describing флаг (`create: true` → `Create *bool`, [`artifact/scenarios.go:150`](../../keeper/internal/artifact/scenarios.go)) — образец для `from:`.

**Предстоит построить:**
1. **Сканер `upgrade/*/main.yml`** — новый каталог рядом со `scenario/`. Сейчас `ListScenarios` жёстко сканирует `scenario/*` (`scenarioDir="scenario"`, [`artifact/scenarios.go:22`](../../keeper/internal/artifact/scenarios.go)), а Runner грузит `scenario/%s/main.yml` (`scenarioMainFile`, [`scenario/scenario.go:48`](../../keeper/internal/scenario/scenario.go)). Нужен второй discovery-путь + парс top-level `from: []string` + запуск upgrade-сценария Runner-ом по `upgrade/`-префиксу (не «дописать поле», а новый путь загрузки).
2. **Резолв upgrade-сценария для `from→to`** в `PrepareUpgrade`: скан `upgrade/` целевого снапшота, матч `from:` ⊇ `inc.ServiceVersion`.
3. **Found-ветвь в upgrade-флоу**: `upgradeTx` в found-режиме резервирует `applying` (не `drift`) → `Runner.Start(upgrade-сценарий, ref=to)`; not-found → сегодняшний `drift` + WARN.
4. **Эндпоинт `GET /v1/incarnations/{name}/upgrade-paths`** (+ `?to=`): дешёвый список тегов + on-demand per-target анализ (found/legacy + state-миграции).
5. **UI**: выпадашка upgrade-paths (companion-репо, вне core-скоупа NIM-34).

## ⚠️ Обнаруженные несоответствия / принятые из-за них решения

1. **Каталог назван `upgrade/<slug>/`, а не `migrate/` — осознанно, во избежание тройной перегрузки слова «migrate».** В кодовой базе оно уже занято дважды: (а) `scenario/migrate_cluster/` — **существующий create-сценарий** миграции **ДАННЫХ** с внешнего redis-кластера через нативную репликацию (`create: true`, виден в day-2, [`examples/service/redis/scenario/migrate_cluster/main.yml`](../../examples/service/redis/scenario/migrate_cluster/main.yml)); (б) `migrations/<NNN>_to_<MMM>` — структурные state_schema-миграции [ADR-019](0019-state-migration-dsl.md). Третья ось «версионный апгрейд» под корнем «migrate» путалась бы с обеими. **Решение (2026-07-03): каталог `upgrade/<slug>/`** — симметрия с действием `POST /v1/incarnations/{name}/upgrade` и эндпоинтом upgrade-paths, ноль коллизий. Три термина (`upgrade/` версия, `scenario/migrate_cluster/` данные, `migrations/` схема) развести в [naming-rules.md](../naming-rules.md).
2. **`from` занят в другом смысле**: `artifact.StateSchemaMigration.From int` ([`artifact/state_schema.go:41`](../../keeper/internal/artifact/state_schema.go)) — from-версия [ADR-019](0019-state-migration-dsl.md)-цепочки (целое). Новое top-level `from: []string` upgrade-манифеста — список git-тегов. Коллизия по имени, не по структуре; отметить в доке / развести именами полей в парсере.
3. **Текущий upgrade НЕ запускает Runner-прогон** — заканчивается в `drift` (`crud.go:1640`), а возвращаемый `apply_id` — ULID upgrade-**операции** для истории миграции, НЕ scenario-прогон. Found-ветвь вводит настоящий Runner-прогон. Нужно решить: тот же `apply_id` на обе фазы (migration-tx + upgrade-run) или два разных — иначе `GET .../runs/{apply_id}` и триаж истории будут неоднозначны. (Симметрия: сегодня `writeUpgradeDriftHistory` пишет zero-diff запись `upgrade-pending-apply` под тем же `apply_id`.)
4. **Анти-version-craft инвариант** ([`api/huma_incarnation_formprefill.go:23`](../../keeper/internal/api/huma_incarnation_formprefill.go)) upgrade-paths НЕ нарушает (инвариант про create/day-2-формы, всегда на `inc.ServiceVersion`), но ADR должен явно зафиксировать: upgrade-paths + действие upgrade — **единственный** легальный контур смены версии; выбор версии на create по-прежнему запрещён.

## Consequences

- Апгрейд из «смена пина + drift» превращается в two-phase: структурная миграция ([ADR-019](0019-state-migration-dsl.md)) + опциональная host-оркестрация (upgrade-сценарий). Обратная совместимость полная: сервис без `upgrade/` работает как сегодня (ветвь legacy).
- Второй канал авто-дискавери сценариев (`upgrade/`) — amendment [ADR-009](0009-scenario-dsl.md); `from:` пополняет [naming-rules.md](../naming-rules.md).
- Новый роут `GET /v1/incarnations/{name}/upgrade-paths` — OpenAPI/MCP-поверхность (docs-writer, huma-native).
- Каталог `upgrade/` требует тестовой конвенции (симметрично `scenario/<name>/tests/`, `migrations/<NNN>_to_<MMM>/tests/`).
- Разведение перегруженного «migrate» в доке/naming — обязательный побочный эффект (см. несоответствие 1).

## Отвергнутые альтернативы

- **`to:` в старой версии** — невозможно из-за immutable-тегов (§4).
- **Fail-closed 422 на undeclared-переход** — ломает патч-апгрейды (§5, ★).
- **Каталог `migrate/`** — слово трижды перегружено (см. несоответствие 1); выбран `upgrade/`.
- **Service-scoped `/v1/services/{name}/upgrade-paths`** — `from` инкарнации серверу известен, гонять его query-параметром избыточно и хрупко (§6).
- **upgrade-сценарии в общем `scenario/`** с дискриминатором — пугают оператора в day-2-списках, ломают удобство ретрая/разделения (§3).
- **Массовый/цепочечный апгрейд в MVP** — вынесен в future work (§8, NIM-35/[ADR-043](0043-voyage.md)).

## Future work

- **NIM-35**: массовый апгрейд скоупа через Voyage `kind=upgrade` ([ADR-043](0043-voyage.md)).
- Авто-чейнинг `v1→v2→v3`, glob/semver в `from:` — при реальном запросе, без breaking change.
