# Conductor / Дирижёр

Фоновая leader-elected подсистема внутри `keeper`-бинаря — исполнитель [Cadence](../naming-rules.md#сущности-предметной-области)-расписаний ([ADR-046](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)): по своему тику отбирает созревшие расписания и спавнит обычный [Voyage](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)-прогон. **Не отдельный бинарь** (как и Reaper). Полная фиксация дизайна — [ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний); этот документ — справочник по поведению и конфигу.

> **Conductor исполняет, Cadence хранит, Reaper чистит.** Cadence — это строка в Postgres-таблице `cadences` с «рецептом» прогона + правилом повторения ([ADR-046](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)). Conductor только **исполняет триггер** — спавн-семантика Cadence (due-выборка, три `overlap_policy`, пересчёт `next_run_at`) принадлежит Cadence и в Conductor перенесена **без изменений**. Спавн исторически (S0-дизайн ADR-046) планировался Reaper-правилом `spawn_due_cadence` (`action: spawn`), но [ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний) (2026-06-02) вынес исполнение в отдельную подсистему: cleanup-домен Reaper (`reaper.interval` 1h) и scheduling-домен Cadence требуют разного естественного ритма (~15–30s). После выноса Reaper снова чисто cleanup-домен ([reaper.md](reaper.md)).

## Свойства

- Живёт внутри `keeper`. Не отдельный бинарь.
- Работает **только на одном Keeper-инстансе одновременно**: лидер выбирается через Redis-lease `conductor:leader` с TTL = `lock_ttl` ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)). Lease **независим** от `reaper:leader` — лидер Conductor и лидер Reaper могут быть на разных инстансах.
- Сидит на generic-примитиве lease-loop-а (`keeper/internal/leaderloop`, общий с Reaper) — два независимых lease-ключа, две независимых leader-election.
- **Адаптивный шаг опроса** (cron-модель, [ADR-048 «Adaptive interval»](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)): тик НЕ фиксированный. Перед каждым тиком лидер пересчитывает шаг из enabled-реестра Cadence — `clamp(min(периоды enabled-расписаний), poll_floor, poll_ceiling)`; пустой реестр enabled-правил → `poll_idle` (опрос не вхолостую). Независим от `reaper.interval` (1h). Подробно — [Адаптивный шаг опроса](#адаптивный-шаг-опроса).
- **Default-ON при наличии Redis** (footgun-guard, см. [Default-ON и деградация без Redis](#default-on-и-деградация-без-redis)).
- `poll_floor` / `poll_ceiling` / `poll_idle` / `lock_ttl` (и backcompat-alias `interval`) — hot-reload без передеплоя бинаря (сквозное требование, см. [requirements.md](../requirements.md)). `enabled` читается на старте (см. [Конфиг](#конфиг)).
- Метрики `keeper_conductor_*` — см. [Метрики](#метрики).

## Что Conductor делает на тике

Когда инстанс держит lease `conductor:leader`, на каждом тике он исполняет ту же due-выборку и спавн, что нормированы в [ADR-046 §4/§5](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage):

1. `SELECT … FROM cadences WHERE enabled AND next_run_at <= NOW() FOR UPDATE SKIP LOCKED` — отбор созревших расписаний.
2. Для каждой due-строки **в одной PG-транзакции**: применить `overlap_policy` (`skip` / `queue` / `parallel`) → при разрешённом спавне `INSERT` в `voyages` / `voyage_targets` с `cadence_id` → пересчёт `next_run_at` (cron-парсер для `cron`, anchored `next_run_at + interval_seconds` для `interval`) → `last_run_at = NOW()`.

   **Anchor пересчёта — плановый слот, не `NOW()`.** Для `interval` следующий запуск считается от **прежнего планового `next_run_at`** (того, по которому строка стала due), а не от фактического `last_run_at`/`NOW()` — иначе тик-дрейф (Conductor сработал чуть позже срока) накапливался бы и слоты «уезжали». Anchoring к плановой сетке делает интервал drift-free. См. [ADR-046 §4](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage).

   **Missed-slot после простоя (anti-storm).** Если Keeper простаивал и пропущено несколько слотов, `next_run_at` переводится на **первый будущий слот сетки строго после `NOW()`** — один catch-up-спавн за текущий due, **без** доспавна каждого пропущенного слота. Симметрично для `interval` (наматывать `+ interval_seconds`, пока слот не станет `> NOW()`) и для `cron` (следующий cron-слот после `NOW()`).
3. Спавнутый Voyage входит в обычный Voyage-lifecycle (`pending` → claim VoyageWorker-ом → `running` → terminal); Conductor его дальше **не ведёт** (one-shot спавн).

`FOR UPDATE SKIP LOCKED` + single-executor-лидерство дают **ровно-один-спавн на тик** без гонки между Keeper-инстансами.

**Авторство и audit.** Дочерний Voyage спавнится «от имени» создателя Cadence (`voyages.started_by_aid` наследует `created_by_aid` Cadence). Audit-event спавна/пропуска (`cadence.spawned` / `cadence.skipped_overlap`) пишется с **`source: background`** и `archon_aid: NULL` — это автономная фоновая инициатива keeper-а, а не операторский вызов. Источник `background` **сохраняется после переезда в Conductor** (новый source `scheduler` НЕ вводится, см. [ADR-048 → ADR-022](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)). Каталог event-types — [naming-rules.md → Audit-events](../naming-rules.md#audit-events).

## Адаптивный шаг опроса

Шаг опроса Conductor **не фиксированный** ([ADR-048 «Adaptive interval»](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)). Перед каждым тиком лидер выводит шаг из enabled-реестра Cadence и зажимает его в коридор `[poll_floor, poll_ceiling]` профиля «Спокойный» (30s / 60s / 120s по дефолту):

```
derivedMinPeriod = min(периоды всех enabled Cadence)   # interval-правила несут interval_seconds; cron-правила — вклад 60s
шаг = clamp(derivedMinPeriod, poll_floor, poll_ceiling)
если enabled-реестр пуст → шаг = poll_idle
```

Как считается `derivedMinPeriod` (реализация — [`cadence.MinPeriod.DerivedMinPeriod`](../../keeper/internal/cadence/crud.go), [`conductor.AdaptivePollInterval`](../../keeper/internal/conductor/poll.go)):

- **interval-правила** дают `MIN(interval_seconds)` по enabled-строкам (один лёгкий aggregate-SELECT `SELECT MIN(interval_seconds), bool_or(schedule_kind='cron') FROM cadences WHERE enabled`, partial-индекс `cadences_enabled_interval_idx`).
- **cron-правила** не несут `interval_seconds` (NULL, в MIN не попадают), но cron-гранулярность — минута: при наличии хоть одного enabled cron-правила вклад — фиксированные **60s**. Чтобы не промахнуться мимо ближайшего cron-слота, Conductor при cron-правилах опрашивается не реже раза в минуту.
- Берётся **более частый** из двух: `min(MIN(interval_seconds), 60s при наличии cron)`.
- **Пустой enabled-реестр** (ни interval-, ни cron-правил): `derivedMinPeriod` не определён — Conductor опрашивается с `poll_idle` (lazy-baseline). Сигнал «пусто» несёт тот же MIN-запрос — отдельного Redis-канала нет.

Зачем коридор:

- **`poll_floor` (30s)** — нижняя граница: частое расписание (`interval=5s`) НЕ заставит Conductor молотить PG каждые 5 секунд. Это и есть defence-in-depth backstop к floor-лимиту минимального периода Cadence (см. ниже) — даже если суб-floor строка обошла write-path и DB-CHECK, опрос всё равно не опустится ниже 30s.
- **`poll_ceiling` (60s)** — верхняя граница: редкое расписание (`interval=1h`) не растягивает опрос так, чтобы единственной страховкой от пропуска слота оставался missed-slot-механизм anchored-пересчёта.
- **`poll_idle` (120s)** — пустой реестр: спавнить нечего, опрашиваем реже обычного коридора, не нагружая PG вхолостую.

**Failover-safe by construction.** Шаг — чистая функция от текущего enabled-реестра в PG, пересчитывается на каждом тике с лидера. Новый лидер после failover не несёт in-memory-состояния опроса: тот же реестр → тот же шаг. Не-лидеры aggregate-SELECT не выполняют (его зовёт только держатель lease).

**Деградация на ошибке fetch.** Если MIN-запрос упал (PG-glitch), лидер НЕ падает — fallback на `poll_ceiling` (нечастый край коридора, не floor — чтобы не молотить PG в шторм) + warn в лог. Следующий тик повторяет запрос.

**Почему не event-driven.** Реактивный (push-уведомление «расписание созрело») не дал бы выигрыша: downstream-цепочка (claim задания Acolyte-пулом + EventStream к Soul-у + apply на хосте) throttle-ит точность сама. Cadence — грубый ритм («раз в N минут/часов»), а реактивный суб-30s домен принадлежит [Beacons](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor) (Vigil/Portent/Oracle, ADR-030), не Cadence.

## Floor минимального периода Cadence

Минимальный период interval-Cadence — **30s** ([ADR-046](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage), Pass B). Создание или изменение Cadence с `interval_seconds < 30` отвергается с **HTTP 422** и текстом «минимальный период Cadence — 30s; для реакции быстрее 30s используйте Beacons (Vigil/Oracle, ADR-030)». cron-Cadence под floor не попадают (cron-гранулярность — минута, ≥ 60s).

Три уровня защиты (defence-in-depth):

1. **Write-path validate** ([`cadence.ValidateIntervalFloor`](../../keeper/internal/cadence/crud.go), `POST`/`PATCH /v1/cadences`) — дружелюбная 422 до PG. Floor берётся из того же config-ключа `cadence_scheduler.poll_floor`, что и нижняя граница опроса (единый минимум, не хардкод `30` в двух местах).
2. **DB-CHECK `cadences_interval_seconds_floor`** (`interval_seconds IS NULL OR interval_seconds >= 30`, миграция 068) — инвариант на уровне таблицы, ловит запись в обход API.
3. **Pre-flight data-guard** в миграции 068 — перед `ADD CONSTRAINT` проверяет, нет ли уже строк с суб-30 `interval_seconds` (например dev-стенд со старым 10s-расписанием), и `RAISE EXCEPTION` с понятным текстом, если нашлись. Не тихий UPDATE: молча менять период чужого расписания недопустимо — оператор сам решает (поднять до 30s / перевести в cron / удалить).

Floor-лимит и нижняя граница опроса `poll_floor` совпадают по значению (30s) и по источнику не случайно: суб-30s период бессмысленен (downstream его не отработает точнее), а реактивный домен — Beacons.

## Default-ON и деградация без Redis

Conductor включён **по умолчанию, если настроен Redis** (lease-лидерство возможно). Это **footgun-guard**: Cadence без работающего планировщика молча не спавнит Voyage — оператор создал расписание, а оно «мёртвое» без видимой ошибки. Поэтому планировщик активен из коробки на любой Redis-инсталляции.

- **Выключение Cadence — per-Cadence** через `enabled: false` самой строки расписания ([ADR-046 §3](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)), не глобальным гашением Conductor.
- **Глобальное гашение** — явный `cadence_scheduler.enabled: false` (оператор сознательно отключает планировщик целиком на этом инстансе).
- **Без Redis** (single-instance dev без Redis) leader-election невозможна — Conductor **деградирует так же, как Reaper-лидер на одиночном инстансе** (не поднимается; метрики `keeper_conductor_*` не публикуются).

## Конфиг

Блок `cadence_scheduler:` в `keeper.yml` (опциональный — при отсутствии действуют дефолты + default-ON при Redis):

```yaml
cadence_scheduler:
  enabled: true        # nil/опущено → ON при настроенном Redis (footgun-guard); false → OFF
  poll_floor: 30s      # нижняя граница адаптивного шага опроса (профиль «Спокойный»)
  poll_ceiling: 60s    # верхняя граница адаптивного шага опроса
  poll_idle: 120s      # шаг опроса при пустом enabled-реестре Cadence
  lock_ttl: 5m         # TTL Redis-lease conductor:leader
  # interval: 60s      # backcompat-alias poll_ceiling (см. ниже); новые конфиги пишут poll_*
```

| Поле | Тип | Default | Hot-reload | Смысл |
|---|---|---|---|---|
| `cadence_scheduler.enabled` | `bool` (опц., tri-state) | `nil` → ON при Redis | нет (читается на старте) | Включение Conductor. **Опущено / `null`** → default-ON при наличии Redis (footgun-guard [ADR-048 §5](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)); явный **`false`** → Conductor не поднимается; явный **`true`** → поднимается (требует Redis для lease-лидерства, как и Reaper). Выключение отдельного расписания — per-Cadence `enabled: false` (ADR-046), не здесь. |
| `cadence_scheduler.poll_floor` | `duration` | `30s` | да | Нижняя граница адаптивного шага опроса (см. [Адаптивный шаг опроса](#адаптивный-шаг-опроса)). Совпадает с floor-лимитом минимального периода Cadence (тот же ключ — единый источник 30s). **Абсолютный минимум**: `< 30s` → конфиг-ошибка `value_out_of_range` на старте (суб-30s период бессмысленен, реактивный домен — Beacons). Пустое/невалидное → дефолт. Перечитывается на каждом тике из свежего Store-снимка. |
| `cadence_scheduler.poll_ceiling` | `duration` | `60s` | да | Верхняя граница адаптивного шага опроса: редкое расписание (`interval=1h`) не растягивает опрос так, чтобы missed-slot-механизм стал единственной страховкой. Инвариант `poll_floor ≤ poll_ceiling` (иначе `value_out_of_range`). Пустое/невалидное → дефолт. Hot-reload. |
| `cadence_scheduler.poll_idle` | `duration` | `120s` | да | Шаг опроса при **пустом enabled-реестре** Cadence (спавнить нечего — опрашиваем реже коридора, не молотим PG вхолостую). Инвариант `poll_idle ≥ poll_ceiling` (иначе `value_out_of_range`: idle не должен быть чаще обычного опроса). Пустое/невалидное → дефолт. Hot-reload. |
| `cadence_scheduler.interval` | `duration` | — (alias) | да | **Backcompat-alias** `poll_ceiling`. До амендмента 2026-06-07 был фиксированным периодом тика; теперь шаг адаптивный, а `interval` оставлен только ради старых `keeper.yml`. Если задан и `poll_ceiling` **не** задан → `poll_ceiling = max(interval, poll_floor)` (clamp вверх до floor). Суб-floor `interval` (например dev-конфиг с `5s`) **не роняет конфиг**: поднимается до floor с WARNING (`value_clamped`, текст про Beacons для суб-30s). Если задан и `interval`, и `poll_ceiling` — побеждает `poll_ceiling` (alias игнорируется). Новые конфиги пишут `poll_*`, не `interval`. |
| `cadence_scheduler.lock_ttl` | `duration` | `5m` | да | TTL Redis-lease `conductor:leader` ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)). Parity `reaper.lock_ttl`: достаточно большой, чтобы пережить временный stall лидера, достаточно короткий для быстрого failover. renew идёт на `lock_ttl/3`. Пустое/`0`/невалидное → дефолт. Применяется между re-acquire lease-а. |

Формат `poll_floor` / `poll_ceiling` / `poll_idle` / `interval` / `lock_ttl` валидируется в semantic-фазе парсера (`checkDuration`, как `reaper.interval` / `acolyte_*`); невалидный duration отвергает конфиг на старте, диапазон (`>0`) добивается дефолтом. Взаимный порядок коридора (`poll_floor ≥ 30s ≤ poll_ceiling ≤ poll_idle`) проверяется по **резолвнутым** значениям (с учётом alias-clamp), поэтому ловит и неявные нарушения через `interval`.

> **Прежняя dev-рекомендация `interval: 5s` отменена.** При floor 30s суб-30s опрос недостижим by design. Для частого ритма ставьте `poll_floor`/`poll_ceiling` к 30–60s; для реакции быстрее 30s — это не задача Cadence, используйте [Beacons](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor) (Vigil/Oracle, ADR-030).

> **`enabled` — не hot-reload.** Включение/выключение Conductor читается при старте инстанса (в отличие от `poll_floor` / `poll_ceiling` / `poll_idle` / `lock_ttl`, которые перечитываются на лету). Чтобы погасить или поднять планировщик, нужен рестарт инстанса. Это сознательно: hot-toggle подсистемы (поднять/убить goroutine + lease на лету) — отдельная сложность, не нужная для штатной эксплуатации (per-Cadence `enabled` покрывает оперативное управление расписаниями без рестарта).

## Метрики

Регистрируются в Prometheus-registry Keeper-а **только в ветке поднятого Conductor** (default-ON при Redis и не `enabled: false`) — если Conductor не поднят, collectors не публикуются вовсе (cardinality-safe, parity Reaper). Реализация — [`keeper/internal/conductor/metrics.go`](../../keeper/internal/conductor/metrics.go), wire-up из `keeper/cmd/keeper/daemon.go`.

| Метрика | Тип | Метки | Смысл |
|---|---|---|---|
| `keeper_conductor_lease_held` | gauge | — | `1` если этот инстанс держит Redis-lease `conductor:leader`, иначе `0`. Один gauge на keeper-инстанс. Cluster-wide инвариант: `sum(keeper_conductor_lease_held) == 1` (при ровно одном лидере). Независим от `keeper_reaper_lease_held` — держатели могут различаться. |
| `keeper_conductor_spawn_executions_total` | counter | — | Число тиков спавна Conductor-лидера за uptime инстанса. Инкрементируется на каждый тик, **независимо** от того, нашлись ли due-расписания. Сравнение со `spawned_total` показывает «эффективность»: много тиков при нулевом спавне = расписаний нет либо все `skip`/`queue`. |
| `keeper_conductor_spawned_total` | counter | — | Число Voyage, **реально заспавненных** из созревших Cadence. `skip`/`queue`-тики (политика не дала спавн) сюда не идут — это «сколько прогонов создано», parity affected-семантики Reaper. |
| `keeper_conductor_spawn_errors_total` | counter | — | Число ошибок тика спавна (Spawner вернул error: PG-сбой, резолв target и т.п.). Выделено из `spawn_executions_total`, чтобы алертилось без histogram-а. |
| `keeper_conductor_spawn_duration_seconds` | histogram | — | Длительность одного тика спавна (`Spawner.Run`). Buckets `0.005…30s` (parity reaper-rule-duration): типичный тик — единицы-десятки ms (SELECT due + per-row insert), верх 30s ловит аномально долгий тик в отдельный bucket. `_count` совпадает с `spawn_executions_total`. |

**Dashboard / alert-ориентиры** ([ADR-048 §5](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)): «лидер жив» (`sum(keeper_conductor_lease_held) == 1`), «спавн идёт по графику» (ненулевой `spawned_total` при наличии активных расписаний), всплеск `spawn_errors_total` — нештатная ситуация.

## См. также

- [operator-api/cadences.md](operator-api/cadences.md) — Operator API расписаний (`/v1/cadences*`): создание/правка/toggle/runs Cadence, которые этот Conductor исполняет.
- [reaper.md](reaper.md) — Reaper / Жнец: cleanup-домен (spawn-правила **нет**, оно здесь).
- [config.md](config.md) → блок `cadence_scheduler:` в `keeper.yml`.
- [ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний) — дизайн Conductor (обоснование, отвергнутые варианты).
- [ADR-046](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage) — Cadence: модель расписания, due-выборка, `overlap_policy`, пересчёт `next_run_at`.
- [ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) — Voyage: что именно спавнит Conductor.
- [ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis) — Redis-lease, single-executor, leader-election.
- [naming-rules.md → Модули и подсистемы внутри `keeper`](../naming-rules.md#модули-и-подсистемы-внутри-keeper) — Conductor в словаре.
- [observability.md → Keeper · Conductor](../observability.md#keeper--conductor-исполнитель-cadence-adr-048) — метрики в общем каталоге.
