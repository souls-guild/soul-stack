# Keeper-side core-модули

Подавляющее большинство core-модулей — **Soul-side** (исполняются на хосте `soul`-бинарём: `pkg`, `file`, `service`, `user`, `exec`, `cmd`, `cron`, …; см. [architecture.md → «Модель модулей»](../architecture.md#модель-модулей)). Часть core-модулей — **Keeper-side**: оперируют реестрами keeper-а (Postgres `souls`+coven, Redis-кэш, журналы) и исполняются на самом keeper-е. Здесь собрана нормативная спецификация Keeper-side core-модулей.

## Диспетчер Soul-side / Keeper-side — `on:`

Адресация (`<namespace>.<module>.<state>`) и контракт SoulModule для обеих сторон один и тот же. Разница — **где исполняется шаг**; это решает scenario-ключ `on:` ([scenario/orchestration.md §3](../scenario/orchestration.md#3-таргет-шага--on)):

| `on:` | Где исполняется | Подходит модулям |
|---|---|---|
| опущен / `[coven, …]` | на хостах incarnation | Soul-side core (`core.pkg.installed`, `core.file.present`, …) |
| `keeper` | на самом keeper-е | Keeper-side core (`core.soul.registered`, `core.cloud.created` — cloud-create через CloudDriver, `core.bootstrap.delivered` — доставка bootstrap-токена по SSH, …) |

Запуск Soul-side core-модуля с `on: keeper` — ошибка валидации; и наоборот. Принадлежность модуля стороне декларируется в его манифесте; `soul-lint` сверяет статически.

## Регистрация и диспетчеризация по адресу (`base` + `state`)

Keeper-side core-модули регистрируются в keeper-side Registry (`keeper/internal/coremod/registry.go`) по **базовому имени** — `<namespace>.<module>` без state-суффикса: `core.soul`, `core.cloud`, `core.bootstrap`, `core.choir`, `core.vault`. State приходит из последнего сегмента адреса задачи.

При исполнении keeper-side задачи (`keeper/internal/scenario/keeper_dispatch.go`) адрес `module: <namespace>.<module>.<state>` делится функцией `config.SplitModuleAddr` (единый разборщик для обеих сторон, тот же что у Soul-side runtime) на пару `(base, state)`:

- `base` (`core.cloud`) идёт в `Registry.Lookup` — находит реализацию `SoulModule`;
- `state` (`created`) кладётся в `ApplyRequest.state` и диспетчится **внутри** реализации модуля.

Примеры author-формы → разбор:

| Адрес задачи (`module:`) | Registry-ключ (`base`) | `ApplyRequest.state` |
|---|---|---|
| `core.soul.registered` | `core.soul` | `registered` |
| `core.cloud.created` / `core.cloud.destroyed` | `core.cloud` | `created` / `destroyed` |
| `core.bootstrap.delivered` | `core.bootstrap` | `delivered` |
| `core.choir.present` / `core.choir.absent` | `core.choir` | `present` / `absent` |
| `core.vault.kv-read` | `core.vault` | `kv-read` |

Бракованный адрес (`SplitModuleAddr` вернул `ok=false`: пустой, `.state`, `core.`) или `base`, которого нет в Registry, — keeper-задача падает (`failed`-событие «unknown keeper-side module»), как Soul-side на неизвестный модуль. Регистрация модуля в Registry условна по наличию его зависимости в `coremod.Deps`: `core.choir` подключается только при заданном `ChoirStore`, `core.bootstrap` — только при полном наборе SSH-deps (provider-карта + host-CA + dialer), иначе сборка их не несёт, и шаг с этим модулем упадёт «unknown».

### Audit-след и per-task алертинг

Каждая keeper-side задача пишет audit-event `task.executed` (симметрично Soul-side handler-у `TaskEvent`): `sid = keeper` (адрес keeper-target-а прогона), `correlation_id = apply_id`, `source: keeper_internal`, `payload.status` — имя `keeperv1.TaskStatus` (`changed → TASK_STATUS_CHANGED` / `failed → TASK_STATUS_FAILED` / иначе `TASK_STATUS_OK`). Благодаря этому **task:-подписка Tiding работает и на keeper-side адреса** (`on: keeper`): keeper-задача с адресом `register ∪ id` (включая `provision_vm` с `id:` без `register:`) попадает в `changed_tasks` терминального события `incarnation.run_completed` и матчится task-селектором ([ADR-052 amend §k/§l](../adr/0052-herald-notifications.md#amend-kl-t4-fix-2026-06-12-changed_tasks-и-task-подписка-покрывают-keeper-side-задачи)). Секрет-гигиена: keeper-side `task.executed` несёт только адрес + status (без `register_data`/output); `error.message` — лишь на провале и только для не-`no_log` задач. Operator-SSE keeper-side прогресс не транслирует.

### Контекст `params:` — `incarnation.state`, но не `soulprint`

Keeper-side задача исполняется на самом keeper-е — хостов у неё нет. Поэтому `params:` рендерятся в **рун-уровневом** контексте (один раз на прогон, не per-host): доступны `input.*` / `essence.*` / `incarnation.*` / `register.*` (от предыдущих keeper-задач), но **не** `soulprint.self` / `soulprint.hosts` — обращение к ним в `params` keeper-задачи даёт штатную CEL-ошибку `no such key` (фактов хоста нет, и это правильно: keeper-шаг оперирует реестрами, а не фактами конкретной VM).

В `incarnation.*` доступен ключ **`incarnation.state.<path>`** — read-only **pre-run снимок** `incarnation.state` (тот же `stateBefore` под row-lock прогона, симметрично Soul-side задачам). Снимок инвариантен в пределах прогона (фиксируется один раз, не накапливается между passages). Это позволяет keeper-side задаче читать факты, записанные предыдущим прогоном: например, `core.cloud.destroyed` в teardown-сценарии `destroy` берёт `provider`/`vm_ids`/`sids` из `incarnation.state.provisioned_*`, записанных create-прогоном через `core.cloud.created` ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). Если у инкарнации ещё нет state (push/trial без него) — `incarnation.state.<x>` даёт `no such key`; защищай чтение `default(incarnation.state.<path>, …)` там, где факт может отсутствовать.

## `core.soul.registered`

**Первый Keeper-side core-модуль.** Управляет привязкой Soul-а (по `SID`) к набору стабильных Coven-меток в реестре keeper-а (таблицы `souls` + coven, [storage.md](storage.md)). Принимает **строку ИЛИ список SID** (регистрация N созданных хостов одним шагом) и опционально несёт **барьер онбординга** `await_online` (блокирующе ждёт, пока зарегистрированные Souls станут online) — [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md).

### Адресация и сторона

- Namespace: `core`. Module: `soul`. State: `registered`.
- Полное имя задачи: `module: core.soul.registered`.
- Сторона: **Keeper-side**. Шаг **обязан** нести `on: keeper`.

### Состояние (state-форма)

`registered` — декларативная форма: «Soul с указанным `sid` находится в реестре и привязан к указанному набору Coven-меток». Модуль идемпотентен по конструкции (повторный вызов с тем же набором — no-op).

Если записи в `souls` для этого `sid` ещё нет — модуль создаёт её под `status: pending` (новый хост, добавленный сценарием — например, host-ветка `add_replica` или после cloud-create через `core.cloud.provisioned`). Создание записи здесь — единственный side-effect помимо обновления coven; модуль не выписывает bootstrap-токены и не запускает CSR-цикл (это компетенция онбординга, [soul/onboarding.md](../soul/onboarding.md)).

При списочной форме `sid` (см. [list-SID](#list-sid--регистрацияожидание-n-хостов-одним-шагом)) переданный набор `coven` применяется к **каждому** SID списка; `await_online`-барьер (если задан) агрегирует presence поверх **всего** набора.

### Параметры (`params:`)

| Параметр | Тип | Обязательность | Default | Описание |
|---|---|---|---|---|
| `sid` | string **или** array of string, `format: fqdn` | required | — | `SID` Soul-а (FQDN), к которому применяется привязка. Принимает **одиночную строку ИЛИ список** ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md), см. [list-SID](#list-sid--регистрацияожидание-n-хостов-одним-шагом)). Список на практике приходит CEL-выражением `${ register.<provision>.hosts }` (SID-список от `core.cloud.provisioned`); литеральный список `sid: [a, b]` статически `soul-lint` **не** проходит (manifest объявляет `sid` как `string`) — это осознанный trade-off, см. [list-SID](#list-sid--регистрацияожидание-n-хостов-одним-шагом). |
| `coven` | array of string, `pattern: "^[a-z][a-z0-9-]*$"`, `min_items: 1`, `unique: true` | required | — | Набор стабильных Coven-меток. Минимум одна метка. При списочном `sid` применяется к каждому SID. |
| `mode` | string, `enum: [append, replace, remove]` | optional | `append` | Стратегия применения набора `coven` к существующим меткам (см. ниже). |
| `refresh_soulprint` | boolean | optional | `false` | **Pending (слайсы S2/S3 [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)).** Намерение: после успешного шага пере-резолвить roster прогона, чтобы следующий Passage видел новые online-хосты. Сейчас флаг **валидируется как известный** (не ошибка), но фактический mid-run re-resolve **не реализован** — output всегда `refreshed: false`. Не полагаться на работающий re-resolve до S3. |
| `await_online` | boolean | optional | `false` | **Барьер онбординга** ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). `true` — после записи `souls`+coven для всех SID шаг **блокирующе** ждёт, пока зарегистрированные Souls станут online. Источник «online» — **Redis SID-lease** (живой EventStream), **не** PG `souls.status`. Требует сконфигурированного presence-checker-а на keeper-е: `await_online: true` без него → шаг `failed`. |
| `await_timeout` | duration | **required при `await_online: true`** | — | Верхняя граница ожидания барьера. **Обязателен** при `await_online: true` — без него валидация падает (барьер не должен висеть вечно). Сверху ограничен `keeper.yml::max_await_timeout` (см. [потолок](#потолок-await_timeout-max_await_timeout)). |
| `await_min_count` | int | optional | число регистрируемых SID | Минимум online-хостов для успеха барьера. Default — **все** зарегистрированные SID (`len(sids)`). Допустимый диапазон: `0 < await_min_count ≤ len(sids)`. |
| `await_poll_interval` | duration | optional | `2s` | Период опроса presence (Redis SID-lease) во время барьера. |

### Семантика `mode`

| `mode` | Итоговый набор coven у `sid` | Поведение по краям | Идемпотентность |
|---|---|---|---|
| `append` (default) | существующие ∪ переданные | пустой пересекающийся набор → no-op | да: повторный вызов с тем же `coven` ничего не меняет |
| `replace` | переданные (existing, не упомянутые, удаляются) | пустой `coven: []` — **ошибка** (двойная защита от footgun-а «хост без корневого coven incarnation»: схема `params.coven` `min_items: 1` + повтор на уровне семантики `mode`; намеренно — чтобы footgun ловился и при ослаблении схемы / расширении контракта в будущем) | да: повторный вызов с тем же набором — no-op |
| `remove` | существующие \ переданные | пустой `coven: []` или метки, которых нет на хосте — **no-op** (без ошибки); снимает только реально привязанные метки | да: повторный вызов с тем же набором — no-op |

`replace` с непустым `coven`, не содержащим корневой `incarnation.name` — модуль **не блокирует** на уровне семантики mode (set операция симметрична), но это пользовательская ошибка-footgun. Гарантия «хост всегда несёт корневой coven incarnation» — invariant на уровне `souls`+coven таблицы / резолвера (см. [storage.md](storage.md), [scenario/orchestration.md §3](../scenario/orchestration.md#3-таргет-шага--on)), не на уровне отдельного вызова модуля.

### list-SID — регистрация+ожидание N хостов одним шагом

Параметр `sid` принимает **строку ИЛИ список строк** ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). Целевой сценарий — один create-scenario, который через `core.cloud.provisioned` создаёт N VM (их `sid` приходят списком в `register.<provision>.hosts`), затем одним шагом-барьером `core.soul.registered` регистрирует их и ждёт онбординга. Список естественнее `loop:`: барьер `await_online` агрегирует presence поверх **всего** набора SID (общий `await_min_count`), а не запускает независимые per-iteration барьеры.

- Переданный `coven` применяется ко **всем** SID списка (общий набор Coven-меток шага).
- Одиночная строка `sid` остаётся валидной (обратная совместимость) — внутренне нормализуется в список из одного элемента.
- **Форма output по `sid`:** одиночная строка → `register.<name>.sid` строкой (историческая форма); список → массивом. Поля `coven`/`removed` отражают набор первого SID; `created`/`removed`-факт совокупный; `online`/`pending` — списки SID.

**Manifest-DSL trade-off.** Урезанный manifest-input DSL ([`shared/coremanifest/soul.yaml`](../../shared/coremanifest/soul.yaml)) не выражает union `string|list`, а смена объявленного типа `sid` на `list` сломала бы одиночно-строковую author-форму. Поэтому `sid` объявлен `type: string`: одиночная литеральная строка проходит `soul-lint` как раньше; **список приходит CEL-выражением** `${ register.<step>.hosts }`, которое `soul-lint` пропускает мимо type-check-а ([ADR-010](../adr/0010-templating.md): `${…}`-значение статически не типизируется). **Литеральный список `sid: [a, b]` статический type-check `soul-lint` не проходит** — приемлемо: на практике SID-список всегда из `register.*` (CEL), а runtime принимает обе формы.

### Барьер онбординга (`await_online`)

При `await_online: true` шаг работает в два этапа ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)):

1. Сперва — обычная регистрация (souls+coven, как без барьера) для **всех** SID.
2. Затем — **блокирующий** опрос presence с периодом `await_poll_interval` под общим таймаутом `await_timeout`, пока число online-хостов среди регистрируемых SID не достигнет `await_min_count`.

**Источник истины «online» — Redis SID-lease** (живой EventStream-lease, [ADR-006(a)](../adr/0006-cache-redis.md)), **НЕ** PG `souls.status`. PG-статус — lifecycle-снимок, отстаёт от реального состояния стрима; lease — конструктивно авторитетный признак, что агент на связи (тот же источник, что presence-фильтр таргет-резолвера и lease-aware Reaper). Барьер не считает хост online до фактического стрима.

**B1-strict (failure-семантика).** Если к `await_timeout` online `< await_min_count` — шаг завершается **`failed`** → fail-stop прогона → `incarnation.state` **не коммитится** → `incarnation.status: error_locked`. Частично-онбордившийся флот не «протекает» в роль-применение: либо набран кворум, либо явный fail с диагностикой (`pending[]` в сообщении и в output). Персистентная Redis-ошибка опроса (источник presence недоступен) — тоже `failed`, не «слепой» успех; отмена прогона (context-cancel) — `failed`.

Запрос `await_online: true` без сконфигурированного presence-checker-а на keeper-е завершается `failed` (молчаливый success при отсутствии источника presence недопустим).

#### Потолок `await_timeout` (`max_await_timeout`)

DoS-guard, fail-closed. Поле `keeper.yml::max_await_timeout` (duration, default `30m` — [`DefaultMaxAwaitTimeout`](../../shared/config/keeper.go)) ограничивает сверху `await_timeout`. Если шаг задаёт `await_timeout` **больше** потолка — шаг `failed` **до** любого опроса (явная ошибка, а **не** тихое обрезание до потолка: скрытое изменение заявленного поведения отвергнуто). Это защищает кластер от сценария-DoS (зловредный/ошибочный `await_timeout: 100h` держал бы run-goroutine / Acolyte-воркер занятым). Потолок читается hot-reload-aware (из текущего snapshot `keeper.yml` на каждый `Apply`); пустое/невалидное/`≤0` значение конфига → дефолт `30m`.

> **HA.** Single-binary провижн-прогон с долгим `await_online`-барьером уязвим к крашу инстанса (блокирующий poll держит run-goroutine). Provision→онбординг→роль сценарии рекомендуется гнать через **Voyage** ([ADR-043](../adr/0043-voyage.md)), где recovery закрыт (осиротевший claim переклеймит другой воркер, [ADR-027(l)](../adr/0027-apply-work-queue.md)). Standalone staged-recovery долгого барьера — открытый риск ([ADR-056 §S4](../adr/0056-staged-render-passage.md)).

### Выходной контракт (`output:` модуля)

Модуль возвращает в `register.<имя>.*` (схема, попадающая в applier-`register:` либо в `register:` обычной module-задачи):

| Поле | Тип | Описание |
|---|---|---|
| `sid` | string **или** array of string | `SID`, к которому применилось действие. **Строка** при одиночном `sid`, **массив** при списочном (форма зеркалит вход, [list-SID](#list-sid--регистрацияожидание-n-хостов-одним-шагом)). |
| `coven` | array of string | **Итоговый** набор coven-меток на хосте после применения `mode` (а не переданный набор-аргумент). При списочном `sid` — набор первого SID. |
| `mode` | string | Применённый `mode` (эхо `params.mode`, удобно при шаблонной композиции). |
| `created` | boolean | `true`, если хотя бы одна запись в `souls` была создана модулем; `false`, если все уже существовали. |
| `refreshed` | boolean | **Всегда `false`** в текущем срезе (mid-run re-resolve — pending S3, см. `refresh_soulprint`). Поле эхо-выдаётся для стабильной формы output. |
| `removed` | array of string | **Только при `mode: remove`**: метки, которые были фактически сняты. Пустой массив, если no-op (или mode ≠ `remove`). |
| `online` | array of string | **Только при `await_online: true`**: SID, ставшие online (Redis SID-lease) к моменту успеха/таймаута барьера. |
| `pending` | array of string | **Только при `await_online: true`**: SID, не успевшие стать online к таймауту (диагностика B1-strict-провала). |
| `satisfied` | boolean | **Только при `await_online: true`**: достигнут ли `await_min_count`. При успехе `true`; при `failed`-провале — `false` (поля `online`/`pending` несут диагностику). |

Поля `online`/`pending`/`satisfied` присутствуют в output **только** когда задан `await_online: true`; без барьера их нет. Плюс стандартные `.changed` / `.failed` DSL-ядра ([destiny/tasks.md §8](../destiny/tasks.md#8-requisites--salt-style-зависимости)).

### Пример вызова из сценария

```yaml
- name: Register the new replica with the cluster coven labels
  on: keeper
  module: core.soul.registered
  register: registered
  params:
    sid:               "{{ input.host.sid }}"
    coven:             ["{{ incarnation.name }}"]
    mode:              append
    refresh_soulprint: true
```

После этого шага запись в реестре `souls` создана/обновлена. (Mid-run re-resolve roster через `refresh_soulprint` — pending S3, [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md): сейчас следующий Passage старого roster ещё не растёт автоматически.)

### Пример: регистрация+барьер для N созданных VM

Регистрация списка SID от `core.cloud.provisioned` и блокирующее ожидание онбординга — один шаг ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)):

```yaml
- name: Register provisioned shards and await onboarding
  on: keeper
  module: core.soul.registered
  register: shards
  params:
    sid:                 "${ register.provision.hosts }"   # список SID из cloud-provision
    coven:               ["${ incarnation.name }"]
    mode:                append
    await_online:        true
    await_timeout:       10m                                # ≤ keeper.yml::max_await_timeout (default 30m)
    await_min_count:     "${ register.provision.count }"    # опц.; default = все SID
    await_poll_interval: 2s
```

Шаг сперва регистрирует все SID списка, затем блокирующе поллит Redis SID-lease. Если к `10m` online `< await_min_count` — шаг `failed` (B1-strict), прогон уходит в `error_locked`, `register.shards.pending` несёт не успевшие SID.

### Отношение к destiny `coven-assign`

Существующая destiny `coven-assign` ([examples/destiny/coven-assign/](../../examples/destiny/coven-assign/)) остаётся как **тонкая обёртка** вокруг этого модуля: её `tasks/main.yml` сводится к одному шагу `module: core.soul.registered` с `mode: append` (одиночный `sid`, без барьера и без `refresh_soulprint`). `destiny.yml` `coven-assign` (input-контракт `sid`+`coven`) — совместим, не меняется.

Когда писать вызов модуля напрямую, а когда `apply: { destiny: coven-assign }`:

- **Напрямую `module: core.soul.registered`** — типовой случай в сценарии. Один шаг, всё видно на месте, поддерживает все `mode`-режимы.
- **`apply: { destiny: coven-assign }`** — когда уже есть устоявшийся вызов через destiny (исторический совместимый код), либо когда destiny используется как self-contained unit с molecule-тестом и независимым git-ref-ом ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). Обёртка фиксирует `mode: append` — для `replace`/`remove` нужен прямой вызов.

## `core.choir.present` / `core.choir.absent`

Правка членства Voice-а в Choir-е инкарнации (ADR-044): «SID является Voice-ом указанного Choir-а данной инкарнации». **Keeper-side**, диспетчер `on: keeper`. Registry-ключ — base `core.choir`; state (`present`/`absent`) приходит из суффикса адреса через `SplitModuleAddr` (см. раздел «Регистрация и диспетчеризация»). Регистрируется только при заданном `Deps.ChoirStore` — иначе шаг падает «unknown keeper-side module». Реализация — [`keeper/internal/coremod/choir/member.go`](../../keeper/internal/coremod/choir/member.go).

### Адресация и сторона

- Namespace: `core`. Module: `choir`. State: `present` (default при пустом state) / `absent`.
- Полное имя задачи: `module: core.choir.present` / `module: core.choir.absent`.
- Сторона: **Keeper-side**. Шаг **обязан** нести `on: keeper`.

### Состояние (state-форма)

| State | Действие | Идемпотентность |
|---|---|---|
| `present` (default) | `AddVoice` — SID становится Voice-ом Choir-а. | Voice уже есть (`ErrVoiceExists`) → `changed=false`, не ошибка. |
| `absent` | `RemoveVoice` — членство снимается. | Voice-а нет (`ErrVoiceNotFound`) → `changed=false`, не ошибка. |

Перед мутацией модуль валидирует существование инкарнации (`IncarnationExists`): отсутствует → `failed`. Инвариант членства (Voice только для SID, уже являющегося членом инкарнации, ADR-044) реализован в choir-CRUD (`AddVoice → ErrNotMembers`) и здесь не дублируется; `ErrNotMembers` → `failed`-событие (прогон уходит в onfail / `error_locked`).

### Параметры (`params:`)

| Параметр | Тип | Обязательность | Описание |
|---|---|---|---|
| `incarnation` | string | required | Имя инкарнации, которой принадлежит Choir. Проверяется на существование. |
| `choir` | string | required | Имя Choir-а. Валидируется `ValidChoirName`; мусор → `failed`. |
| `sid` | string | required | `SID` хоста-Voice (FQDN). Валидируется `ValidSID`; невалидный → `failed`. |
| `role` | string | optional | Партия Voice-а в Choir-е (только `present`). |
| `position` | int (≥ 0) | optional | Позиция Voice-а (только `present`); отрицательная → `failed`. |

### Выходной контракт (`output:` модуля)

`present` отдаёт в `register.<имя>.*`: `incarnation`, `choir`, `sid`, `state: present`, `added` (bool — был ли Voice добавлен). `absent`: `incarnation`, `choir`, `sid`, `state: absent`, `removed` (bool — был ли Voice снят). Плюс стандартные `.changed` / `.failed` DSL-ядра.

### Ограничения S-T5 (не реализовано)

- **Cross-incarnation guard** (`param.incarnation` == инкарнация прогона): run-context модулю недоступен; модуль доверяет param `incarnation`, лишь валидируя его существование. Жёсткий guard — отдельная задача (RunContext-инъекция в keeper-dispatch).
- **Roster-growth** (новый Voice виден следующему шагу прогона) — не реализовано.

Полный per-module справочник — [docs/module/core/choir/README.md](../module/core/choir/README.md).

## `core.bootstrap.delivered`

Тонкая доставка per-VM bootstrap-токена по SSH на свежесозданные cloud-init-VM ([ADR-063](../adr/0063-bootstrap-token-delivery.md)). **Keeper-side**, диспетчер `on: keeper`. Registry-ключ — base `core.bootstrap`; state `delivered` приходит из суффикса адреса. Регистрируется только при полном наборе SSH-зависимостей (`BootstrapProviders` + `BootstrapHostCAs` + `BootstrapDial` в `coremod.Deps`) — иначе шаг падает «unknown keeper-side module». Реализация — [`keeper/internal/coremod/bootstrap/delivered.go`](../../keeper/internal/coremod/bootstrap/delivered.go).

**Закрывает BUG#2 cloud-provision.** До ADR-063 scenario нёс адрес-заглушку `keeper.push.applied`, которой keeper-side не существует (это audit-event push-прогона Destiny, не модуль) — созданная VM ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)) не получала токен, барьер `await_online` не набирал presence, прогон уходил в `error_locked`.

### Дизайн A1 — «тонкая доставка»

cloud-init (B-flat, [ADR-017(h)](../adr/0017-keeper-side-core.md)) уже поставил на VM soul-бинарь + CA + systemd-unit (но **намеренно НЕ токен** — userdata логируется провайдером). Модуль кладёт **ТОЛЬКО** токен и опционально запускает soul-агент. Поток per-host (**последовательно**):

1. `SshProvider.Authorize(host, user)` — deny прерывает доставку до connect-а (**fail-closed**).
2. ephemeral ed25519-keypair + `SshProvider.Sign(pubkey)` → `ssh.AuthMethod`-ы (переиспользует `push.NewEphemeralEd25519` + `push.AuthMethodsFromSign`). Приватник не покидает Keeper.
3. `push.Dial` → `Session` (CA-signed host-cert verify, тот же путь, что `SshDispatcher.SendApply`).
4. `session.Run("install -d -m 0700 /etc/soul && umask 077 && cat > <token_path> && chmod 0400 <token_path>", tokenBytes)` — **★ токен в STDIN, НЕ в argv** (иначе утечёт в `ps`/audit/journald на VM).
5. если `start_soul` — `session.Run("systemctl start soul", nil)`.

**B1-strict:** ошибка любого хоста (Authorize-deny / connect-fail / write-fail / start-fail) → шаг `failed` → state не коммитится → `error_locked`.

### Адресация и сторона

- Namespace `core`, module `bootstrap`, state `delivered`.
- Полное имя задачи: `module: core.bootstrap.delivered`.
- Сторона: **Keeper-side**. Шаг **обязан** нести `on: keeper`.

### Параметры (`params:`)

| Параметр | Тип | Обязательность | Default | Описание |
|---|---|---|---|---|
| `hosts` | array of object `{sid, primary_ip, bootstrap_token}` | required | — | Список VM. На практике приходит CEL-выражением `${ register.<provision>.hosts }` (выход `core.cloud.created`). Пустой список → `failed`. |
| `ssh_provider` | string | required | — | Имя SshProvider-плагина (`keeper.yml::plugins.ssh_providers[].name`). |
| `token_path` | string | optional | `/etc/soul/token` | Путь файла токена на VM. |
| `ssh_user` | string | optional | `root` | SSH-пользователь. |
| `ssh_port` | int (1..65535) | optional | `22` | TCP-порт sshd. |
| `start_soul` | bool | optional | `true` | `systemctl start soul` после доставки. |

### Выходной контракт (`output:` модуля)

`register.<имя>.*`: `hosts[] = {sid, delivered, started}` + `count`. Плюс стандартные `.changed` (всегда `true` при успехе) / `.failed` DSL-ядра. **★ БЕЗ токена в output** — plain-токен виден только в register предыдущего шага (`core.cloud.created`, ключ `bootstrap_token`, маскируется `audit.MaskSecrets`); здесь его нет.

### Безопасность

- Токен в STDIN, не в argv (шаг 4). Audit-payload `bootstrap.delivered` — `{action, ssh_provider, count, sids}`, **без токенов**. Текст ошибки маскируется (`audit.MaskSecrets`) перед `failed`-event. CA-signed host-cert verify обязателен (пустой host-CA → явная ошибка). fail-closed Authorize.

### Границы MVP (ADR-063)

- Один key-based SshProvider, только токен, хосты последовательно.
- **★ C1 — cloud-init CA-signed host-key (required-для-live, СЛЕДУЮЩИЙ слайс).** `push.Dial` доверяет только host-cert, подписанному host-CA (отказ от TOFU) — свежая VM обязана иметь CA-signed host-key, иначе handshake реджектится. До C1 модуль валиден на render (L0 Trial) + unit-тестах, но live-e2e не пройдёт.

## `core.vault.kv-read`

Явное чтение секрета из Vault KV (v1/v2, версия mount-а определяется автоматически) на keeper-стороне с обязательной записью audit-event-а `vault.kv-read` (ADR-017(b)). **Keeper-side**, диспетчер `on: keeper`. Registry-ключ — base `core.vault`; state `kv-read` (verb) приходит из суффикса адреса. Существует параллельно с implicit `${ vault(...) }` в CEL: implicit-форма дёшева для рендера, но не оставляет audit-записи; этот модуль — explicit-форма для compliance-аккуратного чтения. Read-only (`changed=false` всегда). Полный per-module справочник с params/output/security — [docs/module/core/vault/README.md](../module/core/vault/README.md).

## См. также

- [architecture.md → Модель модулей](../architecture.md#модель-модулей) — общая модель core/custom, Soul-side vs Keeper-side, протокол SoulModule.
- [architecture.md → Адресация модулей](../architecture.md#адресация-модулей) — формат `<namespace>.<module>.<state>`.
- [scenario/orchestration.md §3](../scenario/orchestration.md#3-таргет-шага--on) — `on:`, диспетчер шага между Soul-стороной и Keeper-стороной.
- [storage.md](storage.md) — таблицы `souls`, привязка coven.
- [cloud.md](cloud.md) — `core.cloud.provisioned` и граница с coven-привязкой (`core.soul.registered` — отдельный шаг).
- [naming-rules.md → Модули Destiny](../naming-rules.md#модули-destiny) — словарь имён.
