# Scenario — спецификация оркестрационного слоя

Этот документ — **нормативная спецификация дельты scenario** поверх DSL-ядра задач destiny. Источник правды при реализации scenario-оркестратора.

**DSL-ядро задач здесь НЕ дублируется.** Все блоки задачи (`module:`, `include:`, `block:`, `params:`, task-level `vars:`, `when:`, `parallel:`, `loop:`, `register:`, `output:`, `no_log:`, `onchanges:`, `onfail:`, `require:`, `changed_when:`, `failed_when:`, `retry:`, `timeout:`), их семантика, барьеры, requisites и шаблонный контекст — **полностью описаны в [destiny/tasks.md](../destiny/tasks.md)** и наследуются scenario как есть. Этот документ описывает **только то, чего у destiny нет**: таргетинг, кросс-хостовую координацию, `apply: { destiny: … }`, запись `incarnation.state`, резолв ресурсов, тесты сценария.

Любой ключ, не описанный здесь и не описанный в [destiny/tasks.md](../destiny/tasks.md), — ошибка валидации scenario.

Связанные документы: [concept.md](concept.md), [destiny/tasks.md](../destiny/tasks.md), [architecture.md → «Targeting и связь хостов»](../architecture.md#targeting-и-связь-хостов), [architecture.md → «Service — структура и manifest»](../architecture.md#service--структура-и-manifest), [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация).

## 1. Файловый формат и раскладка

```
scenario/<name>/
├── main.yml                  # точка входа: name, description, input, state_changes, tasks (inline)
├── <sub>.yml                 # include-соседи (та же структура задач)
├── templates/                # ОПЦ.: шаблоны, используемые шагами этого сценария
├── vars.yml                  # ОПЦ.: scenario-локалы (как destiny vars.yml)
└── tests/                    # ОПЦ.: тесты этого сценария
    └── <case>/
        └── case.yml
```

`main.yml` содержит **inline** `input:`, `state_changes` и `tasks:`. Соседние `*.yml` подключаются через `include:` ([destiny/tasks.md §4](../destiny/tasks.md#4-базовые-блоки)). Раскладка папок (`templates/`, `vars.yml`, `tests/`) — **симметрична destiny** намеренно, без отдельного словаря; двухуровневый резолв — см. §6.

Структура `main.yml` (блоки `name`, `description`, `input`, `state_changes`, `tasks`) — в [architecture.md → «`scenario/<name>/main.yml`»](../architecture.md#scenarionamemainyml--самодостаточная-операция). Блок `input:` — по общему стандарту [docs/input.md](../input.md).

## 2. Дельта scenario относительно DSL-ядра

Поверх задач из [destiny/tasks.md](../destiny/tasks.md) у scenario-задачи появляются дополнительные ключи:

| Блок | Тип | Применим к | Обязательность |
|---|---|---|---|
| `on:` | `keeper` ИЛИ list of coven-id ИЛИ опущен | всем видам задач | опционально (опущен = весь incarnation) |
| `where:` | string (predicate-expr) | всем видам задач | опционально |
| `apply:` | map (`destiny:` + `input:`) | задаче-applier | альтернатива `module:` |
| `serial:` | int (1..M) ИЛИ string `"<N>%"` | module/apply/`block:`-задаче | опционально (опущен = вся ширина таргета) *Гранулярность — per-Passage min-width (в N=1 = per-RUN), см. подраздел §2.2.1 ниже* |
| `run_once:` | bool, default `false` | module/apply/`block:`-задаче | опционально |

Всё остальное в scenario-задаче — ровно то же, что в destiny-задаче, с той же семантикой ([destiny/tasks.md §3–§10](../destiny/tasks.md#3-полный-список-блоков-задачи)). Расхождения с destiny явно перечислены в §6 (резолв ресурсов) и §10 (шаблонный контекст).

### 2.1. `apply:` — вызов destiny

```yaml
- name: Install redis on all cluster hosts
  apply:
    destiny: redis                 # имя destiny из service.yml → destiny:
    input:
      version:  "${ essence.redis_version }"
      password: "${ input.redis_password }"
```

`apply:` — место, где scenario делегирует работу в изолированную destiny. `destiny:` — имя из реестра зависимостей `service.yml` (резолв ref — [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). `input:` — значения, подкладываемые в `input:`-контракт destiny; destiny их валидирует своим контрактом ([destiny/input.md](../destiny/input.md)). Задача с `apply:` — это **applier-задача**; `apply:` и `module:` взаимоисключающи в одной задаче.

#### 2.1.1. `register:` на applier-задаче — чтение результата destiny

Applier-задача может нести унаследованный из DSL-ядра `register: <имя>` ([destiny/tasks.md §8](../destiny/tasks.md#8-requisites--salt-style-зависимости)). В `register.<имя>.*` попадает результат destiny по её декларированному top-level `output:`-контракту ([destiny/output.md](../destiny/output.md)) **плюс** стандартные `.changed` / `.failed` DSL-ядра. Канон доступа — `register.<имя>.<output-поле>`; форма та же, что в `where:`/`changed_when:`/requisites (§4).

```yaml
- name: Reload Redis cluster config on all hosts
  apply:
    destiny: redis-reload
    input: { ... }
  register: reload

- name: Restart only nodes that reported config drift
  on: ["${ incarnation.name }"]
  where: soulprint.self.sid in register.reload.drifted_sids
  module: core.service.restarted
  params: { name: redis-server }
```

Здесь destiny `redis-reload` объявляет в своём top-level `output:` поле `drifted_sids: { type: array, items: { type: string, format: fqdn } }`; scenario читает его через `register.reload.drifted_sids`. Формат `output:`-блока в `destiny.yml` и правила заполнения через task-level `output:` — [destiny/output.md](../destiny/output.md).

Если destiny не объявила top-level `output:` — `register.<имя>.*` содержит только стандартные `.changed`/`.failed`; прикладных полей через `register.<имя>.<поле>` не будет (ошибка валидации при обращении к необъявленному полю).

> Когда писать `apply:`, а когда инлайн `module:` — см. границу-рекомендацию в [concept.md](concept.md) ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). Снятие старого инварианта «scenario только `apply:`» означает: `module:` (включая изменяющие модули) в scenario теперь легален.

### 2.2. Cross-host модель исполнения шага

DSL-ядро ([destiny/tasks.md](../destiny/tasks.md)) описывает исполнение шага
**на одном хосте**. Scenario добавляет ось «хосты таргета». Базовая модель,
поверх которой работают `serial:` и `run_once:`:

- Шаг с таргетом из M хостов (после резолва `on:`+`where:`, см. §3–§4) по
  умолчанию применяется **ко всем M хостам в одной волне** (cross-host
  fan-out), затем — cross-host join перед следующим шагом. Join подчинён
  инварианту barrier/state-commit (§7): `state_changes` коммитятся строго
  после завершения шагов на всех хостах прогона, не по-хостно.
- Порядок хостов в любой пофазной обработке (волны `serial:`, выбор хоста
  для `run_once:`) **детерминирован**: лексикографически по `SID`.
  Недетерминированный порядок запрещён — он ломает воспроизводимость
  разрушительных операций и ассерты топологии в тестах сценария (§6).

Эта ось **ортогональна** `parallel:` и `loop:` DSL-ядра — не путать:

| Механизм | Ось | Источник | Семантика |
|---|---|---|---|
| `parallel:` (tasks.md §6) | треды на ОДНОМ хосте | — | fire-and-forget, flow не ждёт |
| `loop:` (tasks.md §7) | коллекция данных | `input.*` / `vars.*` | повтор шага по элементам |
| `serial:` | ХОСТЫ таргета | резолв `on:`/`where:` | волны по ≤N хостов, волны последовательны |
| `run_once:` | ХОСТЫ таргета | резолв `on:`/`where:` | ровно один (первый по SID) хост таргета |

Комбинируемы: шаг с `loop:` под `serial:` прокатывает весь loop на каждом
хосте волны; `parallel:` внутри хоста работает независимо от волн.

> **Фаза раскрытия `loop:` (нормативно).** `loop:` раскрывается в
> **render-фазе** — одна задача даёт **N `RenderedTask`** по элементам `items:`,
> со сквозными индексами (симметрично `apply: destiny`). Это **не** config-splice
> как `include:` (тот вклеивается в плоский список задач ДО render): `items:` —
> CEL/template-выражение, известное только после CEL-фазы. Раскрытие идёт ПОСЛЕ
> резолва таргета (`on:`/`where:`/`run_once:`), внутри каждого targeted-хоста;
> `serial:`-ширина наследуется всеми итерациями. **Пилот:** `items:` и
> `loop.when:` host-инвариантны (резолвятся один раз на прогон, без `soulprint`);
> host-вариативный `when:`/`items:` через `soulprint` конкретного хоста (per-host
> loop-фильтрация) отложен. См. [destiny/tasks.md §7](../destiny/tasks.md).

#### 2.2.1. `serial:` — волновое (rolling) исполнение

- **Значение.** `N` (целое 1..M) или `"<N>%"` (процент ширины таргета,
  округление вверх, минимум 1). Хосты таргета, отсортированные по `SID`,
  бьются на последовательные волны размера ≤N: внутри волны — параллельно,
  волны — строго последовательно.
- **Применим к** module-, applier- и `block:`-задаче. На `block:`-задаче
  волна катит **весь блок целиком** (все его внутренние шаги) по одному
  набору хостов, прежде чем перейти к следующей волне — это идиома
  «волна = {изменить, проверить здоровье}» (см. §5).
- **Падение в волне (fail-stop, инвариант).** Первый хост, finally-failed
  после исчерпания унаследованного `retry:`, останавливает rolling:
  последующие волны не стартуют. Прямое следствие абсолютного fail-closed
  и §7. Порог толерантности к частичному отказу не предусмотрен (§8, open Q).
- **Взаимодействие с barrier (§7) — инвариант, не опция.** `serial:`
  **не дробит** state-commit. `state_changes` коммитятся **один раз после
  завершения ВСЕХ волн по всем хостам**, никогда по-волново. Частичный
  коммит после успешной волны при падении следующей запрещён — иначе
  `incarnation.state` разойдётся с фактом (см. §7).

> **Гранулярность `serial:` — per-Passage min-width.** Грамматика разрешает писать `serial:` на каждой задаче независимо, а ширина волны — **одна на Passage**: минимальная положительная `serial:`-ширина среди задач **этого Passage** (самое узкое окно). Задачи без `serial:` не сужают окно; если `serial:` не несёт ни одна задача Passage — он идёт одной волной (вся ширина таргета). Причина агрегации — dispatch-модель «один `ApplyRequest` на хост со всеми его задачами скопом» (ADR-012(d)) поверх composite PK `(apply_id, sid, passage)` таблицы `apply_runs`: задачи одного Passage едут хосту одним сообщением, поэтому катить разные задачи Passage разными волнами нельзя. Выбор минимума (а не максимума) — fail-closed: окно сужается до самого узкого намерения автора, blast radius при падении минимален.
>
> **Per-Passage, НЕ per-RUN ([ADR-056](../adr/0056-staged-render-passage.md) §serial, S-2D1).** До стратификации (один проход, N=1) ширина считалась по всему прогону — это **частный случай** per-Passage, **бит-в-бит совпадающий** при N=1. Со staged-render (probe→where даёт >1 Passage) ширина выводится из задач **каждого Passage отдельно**: probe-Passage без `serial:` едет **одной волной**, даже когда последующий Passage несёт `serial:1` — узкое окно одного Passage **не просачивается** в чужой (иначе probe-проход молча поехал бы по одному хосту — destructive throttle). Следствие для автора: задача без собственного `serial:` поедет узкими волнами, только если в **её Passage** есть хоть одна узкая задача (не где угодно в прогоне). 2D `serial`×`passage` — реализован; прежний пилот-рестрикт (serial + staged отвергался) снят. Инварианты fail-stop и barrier/state-commit (§7) сохранены: волны по-прежнему последовательны внутри каждого Passage, state коммитится один раз после **последнего** Passage.

#### 2.2.2. `run_once:` — исполнение на одном хосте таргета

- **Значение.** `bool`, default `false`. При `true` шаг выполняется ровно
  на **одном** хосте — первом по `SID` из резолва `on:`+`where:`.
- **>1 хоста в таргете — норма** (типовой кейс: failover, когда «текущих
  master» по probe может оказаться >1). Шаг идёт на одном детерминированном.
- **0 хостов в таргете.** `run_once:` **не вводит собственной политики**
  пустого/неизвестного таргета — применяется общая семантика §5 (решает
  `failed_when:`/модуль, далее штатная обработка падения шага). Отдельного
  fail-closed-инварианта для `run_once:` нет.
- `serial:` и `run_once:` взаимоисключающи в одной задаче (ошибка
  валидации) — это разные стратегии ширины.

#### 2.2.3. Батчевый прогон (Voyage `kind=scenario`, [ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон))

`serial:` / `run_once:` / `on:` / `where:` фиксируют разбиение и таргет
**на уровне декларации scenario**. Для массовых выкаток на 100k+ souls
оператор может разбить прогон на последовательные батчи (**Leg**) +
сузить таргет по labels/coven/CEL через **invocation-параметры**. Это
второй уровень оркестрации **над** scenario-runner-ом: ADR-009 фиксирует
declaration-level, **Voyage** фиксирует invocation-level.

Пример:

```bash
soulctl incarnation run prod-cache converge \
  --wave-size 10000 \
  --wave-on-failure abort \
  --where 'soulprint.self.os.family == "debian"' \
  --target-coven prod-eu \
  --concurrency 100
```

→ Voyage `kind=scenario` разбивает прогон на Leg-и по инкарнациям, sequential
execution, abort на первом failed Leg. Каждая инкарнация — полноценный
scenario-прогон со своим barrier + state-commit (парность §7).

**Инварианты по отношению к §2.2:**

- **AND-merge target.** `target.coven[]` и `target.where` invocation-а
  **сужают** scenario `on:`/`where:` (пересечение), не расширяют. Оператор
  не может через invocation выйти за рамки декларированного scenario-таргета.
- **REPLACE concurrency.** `concurrency` invocation-а перекрывает scenario
  `serial:` (runtime-knob, не declared invariant).
- **`run_once:` + батч — конфликт.** Сценарий с `run_once:`-шагом
  несовместим с разбиением на waves (`run_once` = ровно один хост таргета,
  разбивать нечего). Валидация отвергает батч-запрос (`422
  validation-failed`).
- **Per-incarnation state-commit.** §7 barrier работает per-инкарнация: state
  коммитится после каждой успешно отработавшей инкарнации Leg-а, не после всего Voyage.

Полный контракт (PG-table `voyages` + `voyage_targets`, back-link `voyage_targets.apply_id → apply_runs(apply_id)`,
failure-policy, RBAC `incarnation.run`, статусы `pending`/`running`/`succeeded`/`failed`/
`partial_failed`/`cancelled`; протухший running-Voyage возвращается Reaper-`reclaim_voyages`
в `pending` для пере-claim) —
[ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон),
HTTP-фасад — `POST /v1/voyages` (`kind=scenario`).
Per-incarnation state-commit (ADR-043 §7/§8) фиксирует state после успешного прогона.

#### 2.2.4. `wait:`/poll-until НЕ вводится — переиспользуется `retry:`+`until:`

Прежний ключ `wait: { condition:, timeout: }` из старых примеров **изъят**
и **не заменяется новым ключом**. Семантика «дождись, пока выражение по
свежему probe станет истинным, до бюджета времени» выражается
унаследованным `retry:`+`until:`
([destiny/tasks.md §9](../destiny/tasks.md#9-прочность-и-контроль-исполнения))
на probe-шаге: `count × delay` задаёт временной бюджет, `until:` — условие
здоровья. Это осознанное решение (не плодить примитив, дублирующий
`retry.until:`); идиома rolling-restart с health-gate — в §5. Любое
вхождение `wait:` в scenario — ошибка валидации; при переписывании старых
примеров `wait: { condition: C, timeout: T }` → probe-шаг с
`retry: { count:, delay:, until: C' }`, где `C'` переписан с удалённого
`soulprint.self.*` на `register.self.*` свежего probe
([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)).

## 3. Таргет шага — `on:`

`on:` — **стабильное место** выполнения шага. Резолвится по Postgres (стабильный слой: реестр хостов incarnation и их covens). Три формы:

| Форма | Семантика |
|---|---|
| **опущен** | весь incarnation: все хосты под корневым coven `${ incarnation.name }` (он подразумевается неявно) |
| `on: keeper` | keeper-сторона: локальная задача на самом keeper-е (cloud-create, vault-resolve, http-call) |
| `on: [coven-a, coven-b]` | пересечение (AND) перечисленных covens; результат **всегда ⊆ хостов incarnation** |

```yaml
# Весь incarnation (on: опущен)
- name: Apply base config everywhere
  apply: { destiny: redis-base, input: { ... } }

# Только keeper
- name: Provision VMs
  on: keeper
  module: core.cloud.provisioned
  state: created
  params: { ... }

# Пересечение стабильных covens, ⊆ incarnation
- name: Tune kernel on bare-metal hosts of this cluster
  on: ["${ incarnation.name }", baremetal]
  apply: { destiny: kernel-tuning, input: { ... } }
```

**Контракт резолвера `on:` (инвариант):**

1. Корневой coven incarnation = `${ incarnation.name }`, назначается keeper-ом автоматически (см. [ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). Опущенный `on:` означает именно его.
2. Список в `on:` — **И/пересечение** covens. Дополнительные стабильные covens (например, `baremetal`, `dc-eu`, `prod`) сужают набор, но **не могут вывести его за пределы хостов incarnation**.
3. **Кросс-incarnation таргетинг запрещён грамматикой.** Резолвер `on:` не может вернуть хост, не принадлежащий текущему incarnation, при любом наборе covens. Это инвариант безопасности (см. [ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)), а не валидация-предупреждение.
4. Роль (master / replica) **никогда не Coven** и в `on:` не участвует. Волатильная роль выражается только через `where:` (см. §4).

## 4. Волатильный предикат — `where:`

`where:` — **волатильный предикат-строка**, отбирающий хосты per-host по результату предыдущего probe-шага (`register:`). Резолвится **по `register:` в рантайме**, не по Postgres.

```yaml
# probe: кто сейчас фактический master (см. §5 — probe-идиома)
- name: Detect actual redis role per host
  module: core.exec.run
  on: ["${ incarnation.name }"]
  register: redis_role
  changed_when: false                                          # probe state не меняет
  params:
    command: "redis-cli role | head -1"

# следующий шаг таргетит по register прошлого probe, per-host
- name: Restart only the current replicas
  on: ["${ incarnation.name }"]
  where: register.redis_role.stdout == 'slave'
  module: core.service.restarted
  params: { name: redis-server }
```

**Двухфазный резолв таргета.** Сначала `on:` сужает множество по Postgres (стабильно), затем `where:` фильтрует получившееся множество **per-host** по `register:` (рантайм). Порядок строгий: `on:` → Postgres, `where:` → `register:`.

> **Реализация probe→where — staged-render (Passage, [ADR-056](../adr/0056-staged-render-passage.md)).** Чтобы `where:` (и `apply: input:`/`params:`/`vars:` следующей задачи) реально видел `register:` предыдущего probe, прогон исполняется как N упорядоченных **Passage** (render→dispatch→barrier→сбор register): задача с `register.X` стратифицируется в Passage **после** probe, эмитящего `X`, и рендерится с уже заполненным register. До завершения раскатки этой фичи на текущем keeper механизм резолвится по мере внедрения (см. слайс-карту [ADR-056](../adr/0056-staged-render-passage.md)).

**`where:` — предикат по двум классам данных.** Предикат `where:`
оперирует (1) **register-данными** предыдущего probe
(`register.<name>.*`/`register.self.*`) — волатильный per-host
результат; и/или (2) **стабильными фактами хоста** (`soulprint.self.*`
— собственные стабильные факты хоста: `sid`, `network.*`, `os.*`,
`covens`; симметрично `register.self.*`). **Инвариант (уточнён, не
ослаблен):** **каждый** `register.<name>`, встречающийся в `where:`,
обязан быть `register:` probe-шага, завершившегося до этого шага
(иначе — ошибка валидации). Предикат, не содержащий **ни одного**
`register.*` (чисто стабильный, например
`soulprint.self.sid == vars.new_sid`), **probe не требует** —
стабильные факты доступны из Postgres-слоя без рантайм-probe. Это не
ослабление register-инварианта (он применяется ровно к
register-ссылкам), а явное разрешение стабильного per-host фильтра
без probe — нужно для таргетинга хоста по его собственному
стабильному `SID` (кейс `add_replica`: новый хост ещё не
probe-абелен). Волатильная роль по-прежнему **только** через probe +
register ([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)
не затронут).

> **Канон ссылки на `register:` — единый префикс.** В `where:`-предикате
> register адресуется **той же формой**, что в
> `changed_when:`/`failed_when:`/`until:`/requisites: `register.<name>.*`
> (`register.self.*` — только свой результат, в
> `changed_when:`/`failed_when:`/`until:`,
> [tasks.md §10](../destiny/tasks.md#10-шаблонный-контекст)).
> Голая форма `<name>.*` в `where:` — **ошибка валидации**. Один
> register-неймспейс на всё DSL-ядро
> ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация),
> без диалектов). Все примеры §4–§5 и пилот `restart` приведены к этой форме.

**`where:` может ссылаться на несколько `register:`.** Предикат-выражение
`where:` вправе использовать register-карты от **разных** предшествующих
probe-шагов (кейс «бывший мастер»:
`where: "register.redis_role_after.stdout == 'slave' and register.redis_role.stdout == 'master'"`).
Инвариант: **каждый** register-id, встречающийся в выражении `where:`,
обязан быть `register:` probe-шага, **завершившегося до** этого шага;
невыполнение для любого из упомянутых register — ошибка валидации
(обобщение правила «`where:` без предшествующего probe — ошибка» с одного
register на N). Per-host join register-карт идёт по ключу хоста (`SID`).
Семантическая оговорка: register от probe, снятых в разные моменты, —
снимки разного времени; сравнение разновременных снимков
(`role_before` vs `role_after`) семантически на ответственности автора
сценария, движок лишь гарантирует join по `SID`.

### `where:`-ключ шага vs `soulprint.where(...)`-функция — РАЗНЫЕ позиции

Это два разных механизма, их нельзя путать:

| | `where:` — ключ шага | `soulprint.where(...)` — функция в выражении |
|---|---|---|
| **Где стоит** | отдельный top-level ключ scenario-задачи | вызов внутри CEL-выражения (в `params:` через `${ … }`, в `apply: input:`, в `when:`/`where:`, …) |
| **Что делает** | отбирает **на каких хостах** выполнить шаг (per-host фильтр таргета) | возвращает **данные** других хостов по стабильному coven (cross-host lookup) |
| **Источник данных** | `register:` предыдущего probe (волатильно, рантайм) | Postgres + горячий слой Redis (стабильные Soulprint-факты) |
| **Когда резолвится** | в фазе резолва таргета шага | при рендере выражения шаблонизатором |

Пример совместного, но раздельного использования:

```yaml
- name: Point replicas at the master
  on: ["${ incarnation.name }"]
  where: register.redis_role.stdout == 'slave'   # КЛЮЧ: только на репликах
  module: core.exec.run
  params:
    command: "redis-cli replicaof ${ soulprint.where(\"incarnation.name in covens\")[0].network.primary_ip } 6379"   # ФУНКЦИЯ: данные хоста
```

> `soulprint.where(<predicate>)` принимает CEL-предикат **статической строкой-литералом** ([templating.md §2.3](../templating.md#23-зарегистрированные-cel-функции-стартовый-минимум)); keyword-стиль (`coven=...`) не используется. Внутри предиката доступны поля элемента (`covens`/`os.*`/`sid`) и внешний контекст (`incarnation.*` и пр.), поэтому «по coven incarnation» пишется как `soulprint.where("incarnation.name in covens")` — **без** динамической склейки строки (`"'" + incarnation.name + "' in covens"` — запрещено, предикат раскрывается на compile-фазе, см. [templating.md §2.3](../templating.md#23-зарегистрированные-cel-функции-стартовый-минимум)). Частый паттерн «по литеральному coven X» — `soulprint.where("'<X>' in covens")`. Глубокая вложенность кавычек — известный footgun, см. [templating.md §8](../templating.md#8-multi-line-cel-и-кавычки): рекомендация выносить такие выражения в `vars:` шага.

> `where:`-ключ — позиция «на каких хостах». `soulprint.where(<predicate>)` — позиция «откуда взять значение». Они независимы; путать их — ошибка чтения, а не альтернатива. (Поскольку Soulprint после [ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) хранит только стабильные факты, `soulprint.where(...)` оперирует стабильным слоем; волатильная роль — исключительно через probe + `where:`-ключ.)

> `soulprint.self.*` в `where:` — собственные стабильные факты
> хоста-кандидата (per-host, стабильный слой), симметрично
> `register.self.*`; не путать с `soulprint.where(...)` (cross-host
> lookup) и `soulprint.hosts` (список всех хостов прогона).

### Миграция: `filter:` изъят

В прежних примерах встречался ключ `filter:` для отбора хостов. **`filter:` изъят полностью** и заменён на `where:`. Любое вхождение `filter:` в scenario — ошибка валидации; при переписывании примеров `filter: <predicate>` → `where: <predicate>`. Прежняя convention под-ковенов `{incarnation.name}-{role}` (например, `coven: {{ incarnation.name }}-master`) — удалена ([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)); вместо неё probe + `where:`.

## 4.1. `soulprint.hosts` — список хостов прогона (scenario-only аксессор)

`soulprint.hosts` — встроенный аксессор **scenario-контекста**: список всех
хостов текущего прогона (хосты incarnation, как их видит резолвер по
Postgres). У каждого элемента — **только стабильные** факты хоста:

| Поле элемента | Тип | Содержание |
|---|---|---|
| `sid` | string | `SID` хоста (FQDN). |
| `role` | string | **declared-роль** из `incarnation.spec.hosts[].role` (`master`/`replica`/…). Это **declared, НЕ actual**: actual-роль волатильна и берётся только живым probe + `where:`-ключом ([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). На `create` (redis ещё не запущен, probe нечего опрашивать) declared — единственный источник топологии. Поле может быть **пустым/`null`** для хостов, привязанных к incarnation **вне declared-spec** (например, host-ветка `add_replica`: существующий Soul привязан к корневому coven, но его роль оператор в `incarnation.spec.hosts[]` не декларировал). Declared-роль отражает **намерение в spec**, не автозаполняется фактом привязки; фактическая роль такого хоста фиксируется в `incarnation.state` сценарным `state_changes`, не в `soulprint.hosts[].role`. Потребитель declared-роли — только bootstrap `create` (probe невозможен); runtime-операции берут роль probe-ом ([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). |
| `network` | map | стабильные сетевые факты хоста (`network.primary_ip`, `network.fqdn`, `network.interfaces[]`) — typed-схема [ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp), полная спека [`docs/soul/soulprint.md → NetworkFacts`](../soul/soulprint.md#networkfacts). Тот же стабильный слой, что отдаёт `soulprint.where(...)`. |
| `os` | map | стабильные ОС-факты хоста (`os.family`, …). |
| `covens` | array of string | стабильные Coven-метки хоста (включая корневой `${ incarnation.name }`). |

`soulprint.hosts.where("<predicate>")` фильтрует список по любому стабильному
атрибуту элемента (`role`, `sid`, `covens`, `network.*`, `os.*`); результат —
снова список с теми же полями (`[0]` для первого элемента, `.size()` /
`size(...)`, индексирование — как у `soulprint.where(...)`). Предикат —
**статический строковый литерал**, раскрываемый на compile-фазе во встроенный
CEL filter-comprehension (не runtime-исполнение строки): динамическая склейка
предиката запрещена, `.where` разрешён только на `soulprint.hosts`/
`soulprint.where(...)`, `.first` не вводится (первый элемент — `[0]`). Полная
механика — [templating.md §2.3](../templating.md#23-зарегистрированные-cel-функции-стартовый-минимум).

**Связь с `soulprint.where(<predicate>)`.** `soulprint.where("'X' in covens")` —
частный случай: тот же список хостов прогона, отфильтрованный по принадлежности
coven `X`. `soulprint.hosts` — полный список без фильтра; `.where(...)`
обобщает фильтр на любой стабильный атрибут (не только coven). Сигнатура и
канон предиката — [templating.md §2.3](../templating.md#23-зарегистрированные-cel-функции-стартовый-минимум):
**predicate-string** (`"'db' in covens"`, `"os.family == 'debian'"`),
**не** keyword-args (`coven=...` — не поддерживается CEL-ом). Источник
данных один — стабильный слой Postgres + горячий слой Redis.

**Scenario-only. destiny напрямую `soulprint.hosts` НЕ видит.** Аксессор
живёт **ровно на том же уровне**, что `soulprint.where(<predicate>)` —
scenario-контекст, НЕ destiny ([destiny/tasks.md §10](../destiny/tasks.md#10-шаблонный-контекст):
кросс-хостовые `soulprint`-запросы — уровень scenario). destiny получает
топологию **только** через явный `apply: { input: { … } }`-проброс и только
если destiny объявила соответствующий ключ в своём input-контракте
([destiny/input.md](../destiny/input.md)). Изоляция destiny по топологии этим **не
меняется**: `soulprint.hosts`/`soulprint.where(...)` в `tasks/main.yml` destiny — ошибка валидации.

> **`soulprint.self.*` в destiny — доступен** (ADR-009/ADR-010 amendment 2026-06-18).
> Граница изоляции destiny проходит по **self vs топология прогона**: стабильный
> self-факт целевого хоста (`soulprint.self.os.arch`, `.os.family`, `.network.*`, …)
> — per-host свойство самого хоста, на котором destiny исполняется, и доступен
> destiny-CEL напрямую; cross-host `soulprint.hosts`/`soulprint.where(...)` —
> по-прежнему только scenario + явный `apply: input:`-проброс.

**`soulprint.hosts` — функция-в-выражении, не таргет-ключ.** Как и
`soulprint.where(...)`, это позиция «откуда взять данные» (в `params:`,
`apply: input:`, `when:`, выражениях), **не** ключ `where:`/`on:` («на каких
хостах»). Роль здесь — **declared**; per-host волатильный таргетинг —
по-прежнему только probe + `where:`-ключ (§4, инвариант
«`where:` без предшествующего probe — ошибка валидации» **не ослаблен**).

**Bootstrap-таргетинг `create` (probe невозможен).** На `create` actual-роли
нет. Per-role шаги `create` НЕ требуют per-role step-таргетинга: шаг идёт
широко (`on:` опущен = весь incarnation, либо `on: [coven]`), а роль
разруливается передачей `soulprint.hosts` в destiny через `apply: input:` —
destiny получает топологию (список ролей + адрес master через
`soulprint.hosts.where("role == 'primary'")[0].network.primary_ip`) и конфигурит
каждый хост по его declared-роли. Это закрывает прежний open Q «cross-host
master discovery вместо под-coven `{incarnation.name}-master`» (см. §8).

## 4.2. `incarnation.host_count` — размер таргета прогона

`incarnation.host_count` — встроенная **scenario-only** переменная
template-контекста, доступная в любом expression-key (`when:`/`changed_when:`/`failed_when:`/`until:`/`where:`) и в строковой интерполяции через `${ … }`.

| Поле | Тип | Семантика |
|---|---|---|
| `incarnation.host_count` | int | Число хостов в таргете прогона **после** резолва `on:` (стабильно по Postgres) и **до** применения `where:` (волатильный фильтр). На probe-шаге, который таргетит весь incarnation, это `size(soulprint.hosts)` для соответствующего прогона. |

**Назначение.** `incarnation.host_count` — размер таргета прогона для выражений, которым нужно знать ширину incarnation: пороги/проценты в собственной логике сценария (`serial: "${ ... }"`-производные расчёты, ассерты топологии в тестах §6, sizing передаваемых в destiny параметров через `apply: input:`).

> **Не для «полноты probe».** Идиома `failed_when: size(register.<probe>) < incarnation.host_count` («упасть, если probe ответили не все хосты») **не используется и неисполнима**: `failed_when:` вычисляется Soul-side per-host, видит только `register:` предыдущих задач и `register.self.*`, но **не** собственный агрегатный `register.<этого-probe>` и **не** cross-host `size(...)` — обращение даёт CEL `no such key`. Полнота probe в защите не нуждается: разрушительную операцию по неполному probe гарантированно отсекает fail-stop барьер staged-render (§5, [ADR-056 §г](../adr/0056-staged-render-passage.md)), а не ручная проверка.

**Доступ в destiny — отсутствует.** Поле — часть `incarnation.*`-namespace, который в destiny не виден ([destiny/tasks.md §10](../destiny/tasks.md#10-шаблонный-контекст)). destiny получает значение, если ей нужно, через `apply: input:`-проброс.

## 4.3. Свёртка агрегатного `register` к одному значению

Probe-шаг с `register: <X>` накапливает per-host **карту** `sid → payload`
(один probe выполняется на каждом хосте таргета, [§4](#4-волатильный-предикат--where)).
Поэтому `register.<X>` — это **карта**, а `register.<X>[<sid>]` — payload одного
хоста (map `{stdout, changed, failed, …}`), **не скаляр**. Распространённая
ошибка — писать `register.<X>.stdout`: на >1 хосте такого ключа нет (`.stdout`
лежит внутри per-host payload-а, а не на самой карте), выражение не разрешится.

Когда нужно свернуть карту к одному значению (типовой кейс — primary discovery:
probe по нескольким существующим хостам, primary печатает свой адрес, реплики —
пусто), **каноническая форма**:

```
register.<X>.map(k, register.<X>[k].stdout).filter(v, v != '')[0]
```

- `map(k, register.<X>[k].stdout)` — comprehension по **ключам** карты (`k` —
  `SID`); для каждого хоста читает `.stdout` его payload-а → список значений.
  Читать `.stdout` элемента **обязательно**: элемент карты — payload-map, не
  строка.
- `.filter(v, v != '')[0]` — первый non-empty: отбрасывает хосты, ответившие
  пусто (на репликах probe primary-адреса печатает пустую строку), берёт первый
  оставшийся.

Пример (`add_replicas`, primary discovery):

```yaml
- name: Detect actual redis primary address on existing hosts
  module: core.exec.run
  on: ["${ incarnation.name }"]
  where: "!(soulprint.self.sid in input.replicas)"
  register: master_addr
  changed_when: false
  params:
    command: "[ \"$(redis-cli role | head -1)\" = master ] && redis-cli config get bind | awk 'NR==2{print $1}' || true"

- name: Point new replicas at the actual primary
  on: ["${ incarnation.name }"]
  where: soulprint.self.sid in input.replicas
  apply:
    destiny: redis-replication-config
    input:
      master_addr: "${ register.master_addr.map(k, register.master_addr[k].stdout).filter(v, v != '')[0] }"
```

> **`.values()` / `.keys()` в текущем движке НЕ доступны.** CEL-окружение Soul
> Stack подключает только `cel.StdLib()` ([templating.md §2.3](../templating.md#23-зарегистрированные-cel-функции-стартовый-минимум));
> расширение `ext.*` (`ext.Lists`/`ext.Strings` и пр.) **не подключено**. Поэтому
> итерация по карте делается **по ключам** через `map(k, …[k])`, а не через
> `.values()`. Соблазнительная форма `register.<X>.values().filter(...)` —
> compile-ошибка «no matching overload». `map`/`filter`/индексирование `[…]` —
> макросы StdLib и работают. (Иначе можно наступить на грабли «эталона»,
> написанного под `ext.Lists`.)

## 5. Probe-идиома и обработка ошибок

**Probe — это обычный scenario-шаг, не спец-конструкция.** Probe = `module: core.exec.run` (или другой read-only модуль) + `register:` + `changed_when: false`. Никакого отдельного вида задачи, никакого спец-инварианта «fail-closed для таргета», никакого нового атрибута.

```yaml
- name: Detect actual redis role per host
  module: core.exec.run
  on: ["${ incarnation.name }"]
  register: redis_role
  changed_when: false                                          # probe state не меняет
  params:
    command: "redis-cli role | head -1"
```

**Успех probe — через семантику модуля (`failed_when:` по `register.self.*`).** Probe-шаг ничем не отличается от обычного: статус хоста определяет модуль (ненулевой exit `core.exec.run` → хост `failed`) либо унаследованный `failed_when:` **по собственному результату** (`register.self.stdout`/`.rc`). Если probe упал на хосте — работает **штатная обработка падения шага** из DSL-ядра (`retry:` / `onfail:` / остановка сценария / `error_locked`).

> **«Полнота probe» НЕ выражается ручной идиомой.** Прежний спека-пример нёс `failed_when: size(register.redis_role) < incarnation.host_count` («упасть, если probe ответили не все хосты»). Эта идиома **изъята** — она физически неисполнима: `failed_when:` вычисляется Soul-side per-host и видит только `register:` предыдущих задач + `register.self.*`; **собственный** агрегатный `register.<этого-probe>` по имени и cross-host `size(...)` ему **недоступны** → CEL `no such key`. Полнота probe не нуждается в ручной проверке — защиту от разрушительной операции по неполному probe даёт fail-stop барьер staged-render (см. footgun ниже).

Обработка ошибок в scenario — **только унаследованные из DSL-ядра** механизмы: `retry:`, `onfail:`, `failed_when:`, `onchanges:` ([destiny/tasks.md §8, §9](../destiny/tasks.md#8-requisites--salt-style-зависимости)). **Новый атрибут не вводится.**

> **Probe и его потребитель — разные Passage ([ADR-056](../adr/0056-staged-render-passage.md)).** Шаг, читающий `register:` probe (через `where:` / `apply: input:` / `params:` / `vars:`), исполняется в **следующем** Passage относительно самого probe (probe и потребитель не могут оказаться в одном Passage). Это и обеспечивает, что `register:` уже собран барьером предыдущего Passage к моменту render-а потребителя. До завершения раскатки staged-render на текущем keeper механизм резолвится по мере внедрения.

> **Footgun: silent-destructive-on-partial — закрыт барьером, а НЕ идиомой.** Опасность: probe, на который не ответила часть хостов, отдаёт неполный `register:`, и следующий шаг с `where:` по этому `register:` применил бы разрушительную операцию (restart, failover) только к «ответившей» части. **Закрытие — fail-stop барьер staged-render ([ADR-056 §г](../adr/0056-staged-render-passage.md)), без ручной проверки полноты:**
>
> - **Хост-кандидат упал в probe-Passage** → барьер этого Passage фиксирует failure → прогон останавливается, **следующий Passage (где стоит `where:`) не стартует**, `incarnation.state` не коммитится → `error_locked` (§7). Разрушительный шаг просто не доходит до dispatch.
> - **Хост терминален, но `register:` неполон** (probe вернулся без нужного ключа для конкретного хоста) → при render-е `where:` следующего Passage обращение к `register.<probe>.*` этого хоста даёт eval-ошибку `no such key` → задача `failed` (ловится штатно, §7), а не молчаливо «не тот таргет».
>
> Ручная идиома полноты (`failed_when: size(...) < incarnation.host_count`) **не нужна и неисполнима** (см. выше и §4.2): safety обеспечивает барьер. review/architect при ревью scenario-спеки и пилота проверяют, что не осталось пути, обходящего fail-stop барьер для destructive-on-failed/partial-probe.

## 6. Двухуровневый резолв ресурсов

`templates/`, `vars.yml`, `tests/`, `include:`-цели сценария резолвятся **двухуровнево**:

1. **Сначала локально:** `scenario/<name>/<kind>/` (например, `scenario/restart/templates/redis.conf.tmpl`).
2. **Затем service-level:** общий ресурс того же `<kind>` на уровне сервиса (`service-<x>/<kind>/`).

**Коллизия имён — shadowing.** Если имя есть и локально, и на service-level — **ближний полностью перекрывает дальний, без merge**. Это консистентно с правилом приоритета task-level `vars:` над file-level в [destiny/tasks.md §9](../destiny/tasks.md#9-прочность-и-контроль-исполнения) (более локальный scope побеждает целиком).

**`../` в синтаксисе запрещён.** Автор сценария **никогда не пишет** относительные пути с `../`. Fallback на service-level делает **движок**, не автор: автор ссылается на ресурс по имени (`template: templates/redis.conf.tmpl` в шаге `core.file.rendered`, `include: replication.yml`), движок ищет сначала локально, потом на service-level. **Resolved-путь печатается в лог apply** и проверяется `soul-lint`-ом (см. backlog в [soul-lint.md](../soul-lint.md)).

#### Раскрытие `include:` — до render, в плоский список

`include:` раскрывается в **плоский список задач ДО фазы render**, на
scenario-loader-слое (между парсингом `main.yml` и CEL-render). Каждая
include-задача заменяется задачами подключённого файла **inline, на их месте**;
вложенные `include:` раскрываются рекурсивно. Render получает уже плоский
список — `include:`-узлов в его входе не остаётся (если узел дошёл до render — это
программная ошибка раскрытия, не «вне pilot-объёма»). Резолв пути — двухуровневый
(локально → service-level, выше); подключённый файл имеет ту же структуру, что
`tasks/main.yml` (top-level список задач).

**Защита от циклов.** `include: a → b → a` (и прямой self-include) **детектируется
по resolved-пути**: повторное вхождение пути в активную цепочку раскрытия —
ошибка `include_cycle` (не бесконечная рекурсия). Глубина цепочки дополнительно
ограничена жёстким потолком (страховка поверх cycle-detection).

**Проброс scope через `include:` (пилот-ограничение).** На текущем слайсе чистый
`include: <file>` (опц. `name:`) splice'ится плоско, без переноса scope. Include-
задача с модификаторами scope/контроля (`vars:` / `loop:` / `when:` / requisites /
`on:` / `where:`) **не раскрывается** — это ошибка `include_modifier_unsupported`
(чтобы scope не терялся молча). Полный проброс (task-level `vars:` на include-
задаче видны задачам подключённого файла; `loop:` на `include:` — повтор файла
N раз) — последующий слайс (см. §8).

> **Это override правила `include:` из [destiny/tasks.md §4](../destiny/tasks.md#4-базовые-блоки).** В destiny `include:` — строго сосед в той же папке `tasks/`, выход за её пределы запрещён. В scenario правило **иное**: `include:` (и резолв `templates/`/`vars.yml`/`tests/`) двухуровневый — локально, затем service-level, fallback делает движок. `tasks.md §4` при этом **не меняется** — там описано поведение для destiny; отличие scenario зафиксировано здесь.

### Тесты сценария

Раскладка: `scenario/<name>/tests/<case>/case.yml`. Формат `case.yml` — `verify:` / `expect:` — **переиспользуется из [destiny/testing.md](../destiny/testing.md)** (отдельного DSL ассерций нет, тот же подход). Дельта scenario:

- **`stand:` расширен на multi-host.** В отличие от одно-хостового destiny-molecule, scenario-тест поднимает **несколько хостов** (топология кластера) — точный формат блока `stand:` для multi-host наследует open Q sandbox из [destiny/testing.md](../destiny/testing.md) (см. §8 ниже).
- **Ассерты на топологию и `incarnation.state`.** Помимо `output:`-проверок шагов, scenario-тест проверяет результирующую топологию (кто master / replica после операции) и содержимое `incarnation.state` после коммита (см. §7).

> Это **расширение open Q про sandbox** из [destiny/testing.md](../destiny/testing.md): multi-host стенд и ассерты на топологию/`state` — не закрытое решение, а явно отмеченное расширение незакрытого вопроса. Не закрывается молча; до решения — declarative-stub `stand:`, как в destiny-molecule.

## 7. Инвариант barrier / state-commit

Коммит `incarnation.state` (применение `state_changes` сценария) — это **cross-host final-barrier**:

1. Сценарий безусловно дожидается завершения **всех** parallel-задач **всех** хостов прогона (расширение final-barrier из [destiny/tasks.md §6](../destiny/tasks.md#6-параллелизм-parallel-true) с одного хоста на cross-host scenario-уровень).
2. Только **после** этого барьера `state_changes` коммитятся в `incarnation.state` (Postgres).
3. Если хоть одна задача хоть на одном хосте finally-failed → `state` **НЕ коммитится** → incarnation переходит в `status: error_locked` ([architecture.md → «Incarnation»](../architecture.md#incarnation--runtime-инстанс-сервиса)).

**Это инвариант, не опция.** Коммит state допустим строго после безусловного cross-host барьера; частичный коммит при частичном фейле запрещён — иначе `incarnation.state` разойдётся с фактическим состоянием хостов. Применяется в т.ч. к probe-footgun из §5: частичный probe → `failed` → барьер фиксирует failure → state не коммитится.

### 7.1. Грамматика `state_changes` — список CRUD-операций

`state_changes` декларирует, **что** сценарий пишет в `incarnation.state` после
барьера, **и откуда берётся значение**. Это **упорядоченный список операций**
(YAML-список, **не** map), каждый элемент — один **CRUD-глагол** (сингуляр):
`set` / `add` / `modify` / `remove` + структурный `foreach`. Грамматика
зафиксирована [ADR-057](../adr/0057-state-changes-crud-verbs.md).

| Глагол | Форма | Семантика |
|---|---|---|
| `set` | `set: <field>` + `value: "${CEL}"` | перезапись поля `incarnation.state.<field>` целиком. |
| `add` | `add: <collection>` + (map: `key:`+`value:` \| list: `value:`+опц.`match:`) + `on_conflict:` | добавить элемент в коллекцию. |
| `modify` | `modify: <collection>` + `match:` + `patch: { <путь>: "${CEL}" }` | патч **всех** подходящих под `match` (all-by-default). |
| `remove` | `remove: <collection>` + `match:` | удалить **всех** подходящих под `match` (all-by-default). |
| `foreach` | `foreach: "${CEL-список\|map}"` + `as: <имя>` + `do: [<глагол…>]` | bulk fan-out N операций. Форма буквально из migration-DSL ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). |

Пустой список (`state_changes: []`) валиден — сценарий state не меняет.

**Порядок и атомарность.** Операции применяются **в порядке объявления**,
последовательно, к **промежуточному** state (каждая видит результат предыдущих).
Вся цепочка — одна PG-транзакция, один `state_history`-snapshot (§7). Фейл любой
операции (eval-ошибка CEL, нарушенный `expect`, `on_conflict: error`) →
`incarnation.state` **не коммитится** → `error_locked` (barrier-инвариант §7 не
ослаблен: операции применяются ПОСЛЕ cross-host барьера).

**Тип коллекции — из `state_schema`** (`service.yml`). `add` в отсутствующее поле
→ материализовать пустую map/list из схемы (форма известна по схеме, даже если
значения ещё нет).

#### CEL-окружение операций

Значение в `value:` / `patch:` и предикат в `match:` — выражения, рендерятся
Keeper-side после барьера (§7) в том же CEL-окружении, что и `params:` задач
([ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), маркер `${ … }`). Литерал без `${ … }` присваивается как есть.
Non-string результат CEL (число/bool/list/map) — по правилам интерполяции
[ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) (вся ячейка = один `${…}` → нативный тип).

**Полный контекст** — `input.*` / `incarnation.*` / `soulprint.self.*` /
`register.*` / `vars.*` / `essence.*`. Поверх него в `match`/`patch`/`value`
действуют **локальные биндинги текущего элемента коллекции**:

| Биндинг | Семантика |
|---|---|
| `elem` | текущий элемент list-коллекции (или скаляр, если list of scalars). |
| `key` / `value` | ключ / значение текущей записи map-коллекции. |

Имя `elem` (не `self`) выбрано во избежание коллизии с per-host `soulprint.self`.

**Семантика per-RUN.** Значения берутся из `input/vars/incarnation/register`
прогона — это per-RUN, **НЕ** per-host union. Если выражение даёт разные значения
per-host (`${ soulprint.self.* }` / `${ register.* }`) — **last-wins по сортировке
`SID`** (детерминированно, как «последняя запись побеждает» в
[output.md](../destiny/output.md)).

#### `set` — перезапись поля

```yaml
state_changes:
  - set: redis_version
    value: "${ input.version }"
  - set: greeting_file
    value: "${ vars.greeting_path }"
  - set: created_at
    value: "static-literal"        # литерал без ${ } — присваивается как есть
```

#### `add` — рост коллекции (идемпотентный по умолчанию)

list-коллекция (опц. `match:` — дедуп-предикат; `on_conflict: skip` = «добавить,
если нет»):

```yaml
state_changes:
  - add: redis_hosts
    value: "${ vars.new_sid }"
    match: "elem == vars.new_sid"
    on_conflict: skip              # DEFAULT: повтор не дублирует
```

map-коллекция (`key:` + `value:`):

```yaml
state_changes:
  - add: redis_users
    key: "${ input.username }"
    value:
      acl:   "${ input.acl }"
      state: "on"
    on_conflict: error             # двойное создание — явная ошибка
```

`on_conflict: skip` (default) `| replace | error` — поведение при коллизии (ключ
map занят / list-`match` уже находит элемент).

#### `modify` / `remove` — патч и удаление по предикату

`match:` — CEL-предикат над элементом; патчатся/удаляются **все** подходящие
(all-by-default). Множественность — свойство предиката, не флаг.

```yaml
state_changes:
  # точечный патч одной записи map
  - modify: redis_users
    match: "key == input.username"
    patch:
      acl:   "${ input.acl }"
      state: "${ input.state }"
  # патч ВСЕХ реплик разом
  - modify: redis_hosts
    match: "elem.role == 'replica'"
    patch:
      config_version: "${ input.version }"
  # удалить хост (с ассертом кратности)
  - remove: redis_hosts
    match: "elem == input.sid"
    expect: one
```

**`expect: one | at_most_one | any`** (DEFAULT `any`) — опц. runtime-ассерт числа
зацепленных `match` элементов в `modify`/`remove`. Кратность ≠ ожидаемой (`one` —
ровно один, `at_most_one` — ноль или один) → `error_locked` **до коммита**.

**Empty-match → no-op** (идемпотентно) для `modify` И `remove`: предикат не
зацепил ничего — операция тихо ничего не делает, не ошибка.

**Предохранитель широкого match** — `soul-lint` выдаёт **WARN** на
константно-истинный предикат (`match: true`) и на **отсутствие** `match:` у
`remove`/`modify` (которое снесло/перепатчило бы всю коллекцию).

#### `foreach` — bulk fan-out

```yaml
state_changes:
  - foreach: "${ input.new_replicas }"
    as: sid
    do:
      - add: redis_hosts
        value: "${ sid }"
        match: "elem == sid"
        on_conflict: skip
```

`foreach`/`as`/`do` — та же структурная форма, что в migration-DSL
([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl),
[migrations.md](../migrations.md)). Внутри `do:` доступны те же глаголы и биндинг
`<as-имя>` к текущему элементу итерации (поверх полного CEL-контекста).

#### `register.*` как источник значения

`value:` / `patch:` / `match:` видят `register.<task>.<поле>` — результат
probe-задачи прогона:

```yaml
state_changes:
  - set: leader_host
    value: "${ register.elect.stdout }"   # из probe-шага register: elect
```

Канал Soul→Keeper: `TaskEvent.register_data` каждой задачи накапливается на
Keeper-стороне в таблице `apply_task_register` (одна строка на `(apply_id, sid,
task_idx)`). **После** cross-host барьера (§7) scenario-runner загружает
register-данные прогона, резолвит `task_idx → register-имя` по своему плану
задач и строит **per-host** register-карту (`sid → register-имя → payload`).
Рендер идёт per-host в том же last-wins-порядке, что и для `soulprint.self.*`:
register адресует **результат именно того хоста**, для которого вычисляется
значение.

`register.*` здесь — **стабильный** post-barrier-снимок (значения уже
зафиксированы фактом успешного apply), не волатильный рантайм-предикат как
`where:` (§5). Хранилище — Postgres (не in-memory): на multi-Keeper-кластере
([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)) `TaskEvent` может прийти не
на тот инстанс, что держит run-goroutine; общая таблица переживает
cross-Keeper-роутинг и не даёт коммитить state по неполной картине register.

Обращение к `register.<task>.*`, для которого хост не дал register-данных
(нет такой probe-задачи на хосте), — eval-ошибка «no such key» → прогон
`error_locked` (как и любое обращение к необъявленному ключу в CEL).

**`no_log` не попадает в state-граф.** Если probe-задача помечена
`no_log: true` ([destiny/tasks.md](../destiny/tasks.md)), её `register` **не
аккумулируется** в per-host register-карту: scenario-runner при резолве
`task_idx → register-имя` пропускает no_log-задачи. Следствие — операция,
ссылающаяся на `register.<no_log-task>.*`, получает «no such key» (прогон
`error_locked`), и чувствительное значение из такой задачи **никогда не
оседает в хранимом `incarnation.state`**. Это защита источника: секрет не
доходит до state физически, а не маскируется постфактум. Маскинг на выходе
наружных GET-каналов (`GET /incarnations`, `/history`) — независимый второй
слой defense-in-depth (см. [keeper/operator-api.md → Secret masking](../keeper/operator-api.md)).

#### Переходный период (deprecated map-форма)

До [ADR-057](../adr/0057-state-changes-crud-verbs.md) `state_changes` была
**map** с ключами `sets` / `appends` / `modifies`:

```yaml
state_changes:        # DEPRECATED-форма (переходный период)
  sets:               # → транслируется в последовательность `set`-элементов
    redis_version: "${ input.version }"
  appends: [redis_hosts]          # был no-op-плейсхолдер (state не рос!)
  modifies: [redis_users.*.acl]   # был no-op-плейсхолдер
```

`sets` — map `<поле>: <CEL>` — был реализован (перезапись поля), эквивалентен
последовательности `set`-элементов нового списка. `appends` / `modifies` —
**декларация-плейсхолдер без источника значения**, движком **не применялись**
(`incarnation.state` не рос: `add_replica`/`add_user`/`update_acl` проходили
успешно, но коллекция не менялась — латентный баг, который чинят глаголы
`add`/`modify`).

**Transit (breaking).** map-форма парсится **один релиз** как DEPRECATED
(dual-parse, `soul-lint` warn): `sets` транслируется в `set`-элементы, непустые
`appends`/`modifies` остаются no-op (поведение не меняется) с warn-ом «перепиши на
`add`/`modify`, иначе state не растёт». В следующем релизе map-форма **удаляется**
(парс старой формы → ошибка валидации).

## 8. Открытые вопросы (расширения, не закрывать молча)

- **Per-task гранулярность `serial:`.** Текущая модель — per-**Passage** min-width
  (см. §2.2.1; staged-render даёт per-task dispatch по оси задач через Passage).
  Истинно per-task волны (каждая задача в Passage — своя ширина) остаются
  отложенными: внутри одного Passage задачи едут хосту одним `ApplyRequest`. Новый
  ADR при реальном запросе.
- **Service-level расположение include-целей.** Двухуровневый резолв (§6)
  ищет include-цель сначала локально (`scenario/<name>/<file>`), затем на
  service-level. Текущая реализация трактует service-level как общий каталог
  `scenario/<file>` (родитель каталогов сценариев). Это рабочий дефолт; точное
  каноническое место общих include-ресурсов сервиса (тот же `scenario/`, или
  отдельный каталог) — не закрытое решение.
- **Полный проброс scope через `include:`.** Сейчас раскрывается только чистый
  `include: <file>`; модификаторы scope/контроля на include-задаче
  (`vars:`/`loop:`/`when:`/requisites/`on:`/`where:`) отвергаются
  (`include_modifier_unsupported`, §6). Семантика проброса (task-level `vars:`
  видны задачам подключённого файла; `loop:` на `include:` — повтор файла) —
  последующий слайс.
- **Multi-host sandbox.** Формат блока `stand:` для multi-host scenario-теста и ассерты на топологию/`incarnation.state` — расширение open Q про sandbox из [destiny/testing.md](../destiny/testing.md). Не закрытое решение.
- **Перенос `role/*.yaml` в destiny-`input:`.** Параметры, ранее зависевшие от роли (essence-слой `role/*` удалён, см. [concept.md](concept.md)), переезжают в destiny через `input:` по probe-роли — отдельная задача имплементации (пилот и батч переписывания примеров).
- **Bootstrap-источник роли на `create` — ЗАКРЫТО.** На `create` redis ещё
  не запущен, probe невозможен → топология (declared-роли + адрес master)
  берётся из `soulprint.hosts` (declared из `incarnation.spec.hosts[].role`,
  см. §4.1) и пробрасывается в destiny через `apply: input:`. Per-role
  step-таргетинг на `create` не вводится; `where:`-инвариант
  (register-only-after-probe) не ослаблен. Прежняя формулировка
  «cross-host master discovery — pending propose-and-wait» снята.

## 9. См. также

- [concept.md](concept.md) — что такое scenario, граница с destiny, declared vs actual роль, role-agnostic essence.
- [destiny/tasks.md](../destiny/tasks.md) — **DSL-ядро задач**, наследуемое scenario целиком (источник правды по `module`/`include`/`block`/`parallel`/`loop`/`register`/requisites/`retry`/`timeout`/`changed_when`/`failed_when`/шаблонный контекст).
- [architecture.md → ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги), [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация).
- [architecture.md → «Targeting и связь хостов»](../architecture.md#targeting-и-связь-хостов) — `on:`/`where:`, контракт резолвера, probe.
- [architecture.md → «Service — структура и manifest»](../architecture.md#service--структура-и-manifest) — раскладка service-репо и `scenario/<name>/main.yml`.
- [destiny/testing.md](../destiny/testing.md) — формат `case.yml`/`verify:`/`expect:`, разграничение scenario-тестов / destiny-molecule / service-smoke.
- [docs/input.md](../input.md) — стандарт `input:` для блока `input:` сценария.
- [soul-lint.md](../soul-lint.md) — backlog статпроверок scenario (`where:`/`on:`-литералы, инлайн-мутация).
