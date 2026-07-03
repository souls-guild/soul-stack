# core.soul.registered

Привязка Soul-а (по `SID`) к набору стабильных Coven-меток в реестре keeper-а
(таблицы `souls` + coven). **Keeper-side**, диспетчер `on: keeper` — шаг
исполняется на самом Keeper-е, не на хосте (в отличие от Soul-side core вроде
`core.pkg`/`core.file`). Запуск без `on: keeper` — ошибка валидации scenario.
Реализация — [`keeper/internal/coremod/soul/registered.go`](../../../../keeper/internal/coremod/soul/registered.go).

Если записи в `souls` для этого `sid` ещё нет — модуль создаёт её под
`status: pending` (новый хост, добавленный сценарием — host-ветка `add_replica`
или после cloud-create через `core.cloud.provisioned`). Bootstrap-токены /
SoulSeed модуль **не** выписывает
— это компетенция онбординга.

Принимает **строку ИЛИ список SID** и опционально несёт **барьер онбординга**
`await_online` (блокирующе ждёт, пока зарегистрированные Souls станут online) —
[ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md),
[`await.go`](../../../../keeper/internal/coremod/soul/await.go).

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `registered` | Soul с указанным `sid` находится в реестре и привязан к указанному набору Coven-меток (по `mode`). | `changed=true`, если запись `souls` была создана модулем **или** итоговый набор coven отличается от текущего (порядок-независимая сверка). Набор совпал и запись уже была — `changed=false`. |

## registered — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `sid` | string **или** array of string | required | `SID` Soul-а (FQDN). Каждый валидируется как FQDN (`keepersoul.ValidSID`); невалидный — шаг падает. Принимает одиночную строку **или** список ([ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)) — список обычно приходит CEL-выражением `${ register.<provision>.hosts }`; литеральный список `sid: [a, b]` `soul-lint` статически не проходит (manifest объявляет `string`), см. [list-SID](#list-sid). |
| `coven` | array of string | required | Набор стабильных Coven-меток. Каждая метка валидируется как kebab-case (`keepersoul.ValidCoven`, 1..63 символа); мусор вроде `Prod`/`a_b` — шаг падает. При списочном `sid` применяется к каждому SID. |
| `mode` | string | optional (default `append`) | Стратегия применения набора `coven` к существующим меткам: `append` / `replace` / `remove`. Неизвестное значение — шаг падает. |
| `refresh_soulprint` | bool | optional (default `false`) | **Реализован (S2/S3 [ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)).** `true` — шаг становится passage-определяющей границей (Stratify), после его успеха scenario-runner пере-резолвит roster перед следующим Passage (live-снимок); output `refreshed` эхает флаг. Вместе с `await_online: true` ужесточает барьер до facts-wait (см. [барьер](#барьер-онбординга-await_online)). |
| `await_online` | bool | optional (default `false`) | **Барьер онбординга** ([ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md), [`await.go`](../../../../keeper/internal/coremod/soul/await.go)). `true` — после регистрации всех SID шаг блокирующе ждёт их готовности: online (Redis SID-lease); при `refresh_soulprint: true` — дополнительно записанный первый typed soulprint. Без сконфигурированного presence-checker-а на keeper-е → шаг `failed`. |
| `await_timeout` | duration | **required при `await_online: true`** | Верхняя граница ожидания. Без него при `await_online: true` валидация падает. Ограничен сверху `keeper.yml::max_await_timeout`. |
| `await_min_count` | int | optional (default = число SID) | Минимум online-хостов для успеха. Default — все зарегистрированные SID. Диапазон `0 < await_min_count ≤ len(sids)`. |
| `await_poll_interval` | duration | optional (default `2s`) | Период опроса presence во время барьера. |

### Семантика `mode`

| `mode` | Итоговый набор coven | Поведение по краям |
|---|---|---|
| `append` (default) | существующие ∪ переданные | повтор с тем же набором — no-op (`changed=false`). |
| `replace` | переданные (не упомянутые — удаляются) | пустой `coven: []` — **ошибка** (footgun-защита: хост должен сохранить хотя бы одну метку). |
| `remove` | существующие \ переданные | метки, которых нет на хосте, — пропускаются без ошибки; снимает только реально привязанные. |

## list-SID

`sid` принимает **строку ИЛИ список строк** ([ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)). Целевой случай — один create-scenario создаёт N VM через `core.cloud.provisioned` (их `sid` приходят списком в `register.<provision>.hosts`), а этот шаг регистрирует их и (с `await_online`) ждёт онбординга одним барьером. Переданный `coven` применяется к каждому SID; `await_online`-барьер агрегирует presence поверх всего набора (общий `await_min_count`).

Одиночная строка остаётся валидной (нормализуется в список из одного элемента). **Форма output по входу:** одиночный `sid` → `sid` строкой (историческая форма), список → массивом.

Manifest объявляет `sid` как `type: string` (урезанный DSL не выражает union `string|list`): одиночная литеральная строка проходит `soul-lint`, **список приходит CEL-выражением** `${ register.<step>.hosts }` (не типизируется статически, [ADR-010](../../../adr/0010-templating.md)). Литеральный список `sid: [a, b]` статический type-check `soul-lint` **не** проходит — на практике SID-список всегда из `register.*`, runtime (`StringOrSliceParam`) принимает обе формы.

## Барьер онбординга (`await_online`)

При `await_online: true` ([ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md), [`await.go`](../../../../keeper/internal/coremod/soul/await.go)):

1. Шаг сперва регистрирует все SID (souls+coven, как без барьера).
2. Затем блокирующе поллит готовность с периодом `await_poll_interval` под общим `await_timeout`, пока готовых хостов не станет `≥ await_min_count`.

**Источник «online» — Redis SID-lease** (живой EventStream, `SoulsStreamAlive`), **не** PG `souls.status` (отстающий lifecycle-снимок).

**Facts-wait при `refresh_soulprint: true`** ([ADR-061 amendment 2026-07-02](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)): SID «готов» = online **и** typed soulprint записан в PG (`souls.soulprint_facts IS NOT NULL`). Одного lease мало — render следующего Passage читает `soulprint.self.*`, а запись initial-репорта асинхронна (best-effort, [ADR-018](../../../adr/0018-soulprint-typed.md)): на provision-from-zero гонка давала `render_failed` «no such key». На rerun / `create_from_souls` facts уже в PG → проход на первом опросе, нулевое ожидание. Без `refresh_soulprint` — presence-only, facts не опрашиваются.

**B1-strict:** недобор кворума к таймауту → шаг `failed` → fail-stop прогона → `incarnation.state` не коммитится → `incarnation.status: error_locked`. `register.<name>.pending` несёт не успевшие SID; при facts-wait классы недобора в сообщении разделены — `not online: [sids]` vs `online but factless: [sids]`. Персистентная ошибка опроса (Redis presence / PG facts-чек) или отмена прогона — тоже `failed`. `await_online: true` без presence-checker-а на keeper-е → `failed` (молчаливый success недопустим).

**Потолок `await_timeout`.** `keeper.yml::max_await_timeout` (duration, default `30m`, [`shared/config/keeper.go`](../../../../shared/config/keeper.go)) ограничивает `await_timeout` сверху. `await_timeout` > потолка → шаг `failed` **до** опроса (fail-closed: явная ошибка, не тихое обрезание) — DoS-guard против `await_timeout: 100h`, держащего run-goroutine/Acolyte-воркер.

## Capabilities / side-effects

- **Keeper-side, не трогает хост.** Все side-effect-ы — в реестрах Keeper-а
  (Postgres `souls` + coven), а не в файловой системе/процессах Soul-а.
- **Меняет реестр `souls`:** `UpdateCoven` при изменении набора меток; при
  отсутствии записи — `Insert` новой под `status: pending`, `transport: agent`,
  пустым coven (поля `LastSeenAt`/`CreatedByAID` — `null`: cloud-create /
  scenario-host-add не несут оператора).
- **Не выполняет подпроцессов** и не выписывает bootstrap-токены / SoulSeed.
- **Идемпотентность по конструкции:** лишний `UPDATE` не выполняется, если
  итоговый набор совпадает с текущим (`sameSet`, порядок-независимое сравнение).

## Безопасность

- **Keeper-side, не Soul-side — `root`/capability-семантика неприменима.** Шаг
  исполняется в процессе Keeper-а (диспетчер `on: keeper`), а не `soul`-агентом на
  хосте. Манифеста с `required_capabilities` у модуля нет
  ([`soul.yaml`](../../../../shared/coremanifest/soul.yaml) объявляет только
  states/input) — это keeper-internal операция над Postgres, не host-плагин.
- **Запись в реестр `souls` — привилегированная операция Keeper-а.** Модуль
  создаёт/меняет записи реестра (`Insert`/`UpdateCoven`,
  [`registered.go`](../../../../keeper/internal/coremod/soul/registered.go)):
  добавление хоста под `status: pending` и привязка Coven-меток. Доступ к запуску
  такого scenario регулируется RBAC оператора на уровне scenario-прогона
  ([rbac.md](../../../keeper/rbac.md)) — сам core-модуль отдельного permission не
  объявляет. `CreatedByAID` создаваемой записи — `null` (это keeper-internal
  action: cloud-create / scenario-host-add не несут конкретного Архонта).
- **Валидация входа против инъекции мусора в реестр.** `sid` проверяется как FQDN
  (`keepersoul.ValidSID`), каждая метка `coven` — как kebab-case 1..63
  (`keepersoul.ValidCoven`); невалидное — шаг падает, в реестр не попадает.
  Симметрично API-границе `POST /v1/souls`, чтобы scenario-путь не был «чёрным
  ходом» в обход проверок.
- **Footgun-защита `mode: replace`.** Пустой `coven: []` при `replace` — ошибка:
  хост обязан сохранить хотя бы одну Coven-метку (иначе потерял бы корневой coven
  incarnation и выпал бы из таргетинга).
- **Не выписывает bootstrap-токены и SoulSeed** — это компетенция онбординга, не
  этого модуля; секретов модуль не производит и не раскрывает.
- **DoS-guard барьера онбординга.** `await_timeout` ограничен сверху оператор-
  потолком `keeper.yml::max_await_timeout` (default `30m`, fail-closed): шаг с
  завышенным `await_timeout` отвергается `failed` ДО опроса, а не «тихо
  обрезается». Без потолка зловредный/ошибочный `await_timeout` держал бы
  run-goroutine / Acolyte-воркер занятым (ADR-061).

## Output / register

Модуль отдаёт в `register.<name>.*`:

| Поле | Тип | Описание |
|---|---|---|
| `sid` | string **или** array of string | Эхо входа: **строка** при одиночном `sid`, **массив** при списочном ([list-SID](#list-sid)). |
| `coven` | array of string | **Итоговый** набор coven после применения `mode`, а не переданный аргумент. При списочном `sid` — набор первого SID. |
| `mode` | string | Применённый `mode`. |
| `created` | bool | `true`, если хотя бы одна запись `souls` была создана модулем; `false`, если все уже существовали. |
| `refreshed` | bool | Эхо значения `refresh_soulprint`: `true` ⇒ re-resolve roster перед следующим Passage гарантированно выполнится (S3 [ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md)). |
| `removed` | array of string | Только при `mode: remove`: метки, фактически снятые. Пустой массив иначе. |
| `online` | array of string | **Только при `await_online: true`**: SID, ставшие online (Redis SID-lease) к моменту успеха/таймаута барьера. |
| `pending` | array of string | **Только при `await_online: true`**: SID, не успевшие online к таймауту (диагностика B1-strict). |
| `satisfied` | bool | **Только при `await_online: true`**: достигнут ли `await_min_count` (`true` при успехе; `false` при `failed`). При `refresh_soulprint: true` считается по «готовым» (online **и** soulprint записан). |

Поля `online`/`pending`/`satisfied` присутствуют только при `await_online: true`. Плюс стандартные `.changed` / `.failed` DSL-ядра.

## Пример

```yaml
# Регистрируем нового Soul в реестре incarnation: привязываем к корневому
# coven (mode: append default). on: keeper обязателен — это keeper-side шаг.
- name: Bind new replica to the incarnation root coven
  on: keeper
  module: core.soul.registered
  params:
    sid:   "${ vars.new_sid }"
    coven: ["${ incarnation.name }"]
```

```yaml
# Регистрируем список созданных VM и блокирующе ждём их онбординга одним шагом
# (ADR-061). on: keeper обязателен.
- name: Register provisioned shards and await onboarding
  on: keeper
  module: core.soul.registered
  register: shards
  params:
    sid:           "${ register.provision.hosts }"   # список SID из cloud-provision
    coven:         ["${ incarnation.name }"]
    await_online:  true
    await_timeout: 10m                                # ≤ keeper.yml::max_await_timeout
```

(см. [`examples/destiny/coven-assign/tasks/main.yml`](../../../../examples/destiny/coven-assign/tasks/main.yml) — destiny-обёртка вокруг `core.soul.registered`, и [`examples/service/keeper-register/scenario/create/main.yml`](../../../../examples/service/keeper-register/scenario/create/main.yml) — keeper-side dispatch в scenario)

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [keeper/modules.md](../../../keeper/modules.md) — нормативная спека Keeper-side core-модулей (диспетчер `on: keeper`).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-таргет-шага--on) — `on:`, диспетчер шага между Soul-стороной и Keeper-стороной.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-017](../../../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) — Keeper-side core-модули.
- [ADR-061](../../../adr/0061-onboarding-await-and-midrun-reresolve.md) — барьер онбординга `await_online` (amendment 2026-07-02: + facts-wait при `refresh_soulprint`), list-SID, `refresh_soulprint` re-resolve roster (S2/S3 реализованы).
