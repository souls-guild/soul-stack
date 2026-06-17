# core.choir.present / core.choir.absent

Правка членства Voice-а в Choir-е инкарнации (ADR-044): декларируемая сущность —
«SID является Voice-ом указанного Choir-а данной инкарнации» (declared-партия
хора). **Keeper-side**, диспетчер `on: keeper` — шаг исполняется на самом
Keeper-е, не на хосте (в отличие от Soul-side core вроде `core.pkg`/`core.file`).
Запуск без `on: keeper` — ошибка валидации scenario. Реализация —
[`keeper/internal/coremod/choir/member.go`](../../../../keeper/internal/coremod/choir/member.go).

Author-форма адреса задачи — `core.choir.present` / `core.choir.absent`
(base `core.choir` + state, симметрично `core.file.present`/`core.file.absent`
Soul-side). State приходит модулю в `ApplyRequest.state` через `SplitModuleAddr`
и диспетчится внутри реализации (см.
[keeper/modules.md → «Регистрация и диспетчеризация»](../../../keeper/modules.md#регистрация-и-диспетчеризация-по-адресу-base--state)).
Модуль регистрируется в keeper-side Registry **только при наличии** зависимости
`Deps.ChoirStore`; в сборке без choir-сценариев шаг падает «unknown keeper-side
module» (как любой не подключённый).

## States

| State | Действие | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` (default при пустом state) | `AddVoice` — SID становится Voice-ом Choir-а. | `changed=true`, если Voice добавлен. Voice уже есть (`ErrVoiceExists`) → `changed=false`, не ошибка. |
| `absent` | `RemoveVoice` — членство снимается. | `changed=true`, если Voice снят. Voice-а нет (`ErrVoiceNotFound`) → `changed=false`, не ошибка. |

Неизвестный state (не `present`/`absent`) — шаг падает. До мутации модуль
валидирует существование инкарнации (`IncarnationExists`); отсутствует — шаг
падает (иначе `absent` на опечатке имени инкарнации тихо вернул бы
`ErrVoiceNotFound`).

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `incarnation` | string | required | Имя инкарнации, которой принадлежит Choir. Проверяется на существование. |
| `choir` | string | required | Имя Choir-а. Валидируется `ValidChoirName`; мусор — шаг падает. |
| `sid` | string | required | `SID` хоста-Voice (FQDN). Валидируется `ValidSID`; невалидный — шаг падает. |
| `role` | string | optional | Партия Voice-а в Choir-е. |
| `position` | int (≥ 0) | optional | Позиция Voice-а. Отрицательная — шаг падает. |

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `incarnation` | string | required | Имя инкарнации. Проверяется на существование. |
| `choir` | string | required | Имя Choir-а. Валидируется `ValidChoirName`. |
| `sid` | string | required | `SID` хоста-Voice (FQDN). Валидируется `ValidSID`. |

`role` / `position` для `absent` не используются (снятие — по тройке
`incarnation`/`choir`/`sid`).

## Capabilities / side-effects

- **Keeper-side, не трогает хост.** Все side-effect-ы — в реестре Keeper-а
  (Postgres `incarnation_choir_voices`), не в файловой системе/процессах Soul-а.
- **Меняет реестр Choir-членства:** `AddVoice` (`present`) / `RemoveVoice`
  (`absent`) через `Store` (прод — `choir.NewPGStore`). Это правка членства
  Voice-а, не создание самого Choir-а или инкарнации.
- **Не выполняет подпроцессов** и не доставляет ничего на Soul.
- **Идемпотентность по конструкции:** повторный `present` уже-Voice-а
  (`ErrVoiceExists`) и `absent` отсутствующего Voice-а (`ErrVoiceNotFound`) —
  `changed=false`, не ошибка.

## Безопасность

- **Keeper-side, не Soul-side — `root`/capability-семантика неприменима.** Шаг
  исполняется в процессе Keeper-а (диспетчер `on: keeper`), а не `soul`-агентом
  на хосте. Манифеста с `required_capabilities` у модуля нет — это
  keeper-internal операция над Postgres, не host-плагин.
- **Правка членства — привилегированная операция Keeper-а.** Доступ к запуску
  scenario с этим шагом регулируется RBAC оператора на уровне scenario-прогона
  ([rbac.md](../../../keeper/rbac.md)); сам core-модуль отдельного permission не
  объявляет.
- **Инвариант членства (ADR-044) переиспользуется, не дублируется.** Voice можно
  добавить только для SID, уже являющегося членом инкарнации — это гарантирует
  choir-CRUD (`AddVoice → ErrNotMembers`), не данный модуль. `ErrNotMembers` →
  `failed`-событие (прогон уходит в onfail / `error_locked`).
- **Валидация входа против инъекции мусора в реестр.** `choir` проверяется
  `ValidChoirName`, `sid` — `ValidSID` (FQDN); невалидное — шаг падает, в реестр
  не попадает. Существование инкарнации проверяется до мутации.

## Ограничения (S-T5, future — не реализовано)

- **Cross-incarnation guard** (`param.incarnation` == инкарнация текущего
  прогона): run-context модулю недоступен (ADR-044 / architect A1). Модуль
  доверяет param `incarnation` и лишь валидирует его существование. Жёсткий
  guard — отдельная задача (RunContext-инъекция в keeper-dispatch).
- **Roster-growth** (новый Voice виден следующему шагу прогона) — не реализовано.

## Output / register

`present` отдаёт в `register.<name>.*`:

| Поле | Тип | Описание |
|---|---|---|
| `incarnation` | string | Эхо `params.incarnation`. |
| `choir` | string | Эхо `params.choir`. |
| `sid` | string | Эхо `params.sid`. |
| `state` | string | `present`. |
| `added` | bool | `true`, если Voice был добавлен; `false`, если уже существовал. |

`absent` отдаёт:

| Поле | Тип | Описание |
|---|---|---|
| `incarnation` | string | Эхо `params.incarnation`. |
| `choir` | string | Эхо `params.choir`. |
| `sid` | string | Эхо `params.sid`. |
| `state` | string | `absent`. |
| `removed` | bool | `true`, если Voice был снят; `false`, если его не было. |

Плюс стандартные `.changed` / `.failed` DSL-ядра.

## Связь с soulprint

После `present`/`absent` Choir-членство хоста проецируется в CEL как
registry-проекция `soulprint.self.choirs` и `soulprint.hosts[].choirs`
(зеркало `covens`, источник — `incarnation_choir_voices`, не collected-факт
`SoulprintFacts`; см.
[soul/soulprint.md → «Граница Soulprint ↔ souls-registry»](../../../soul/soulprint.md#граница-soulprint--souls-registry)).
Roster-growth в пределах одного прогона пока не реализован (см. ограничения
выше): новый Voice виден последующим прогонам.

## Пример

```yaml
# Добавляем хост в Choir 'replicas' инкарнации (present default).
# on: keeper обязателен — это keeper-side шаг.
- name: Add the new replica to the replicas choir
  on: keeper
  module: core.choir.present
  params:
    incarnation: "${ incarnation.name }"
    choir:       replicas
    sid:         "${ vars.new_sid }"
    role:        replica
```

```yaml
# Снятие членства при выводе хоста из роли.
- name: Remove the host from the replicas choir
  on: keeper
  module: core.choir.absent
  params:
    incarnation: "${ incarnation.name }"
    choir:       replicas
    sid:         "${ input.target_sid }"
```

> **Deferred (backlog).** Вызова `core.choir.present`/`absent` в `examples/`
> пока нет — примеры выше составлены как минимальные валидные по контракту кода.
> Замена на ссылку на реальный scenario-example отложена до появления
> соответствующего use-case.

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [keeper/modules.md](../../../keeper/modules.md) — нормативная спека Keeper-side core-модулей (диспетчер `on: keeper`, разбор `base`+`state`).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-таргет-шага--on) — `on:`, диспетчер шага между Soul-стороной и Keeper-стороной.
- [soul/soulprint.md](../../../soul/soulprint.md) — registry-проекция `soulprint.self.choirs` / `soulprint.hosts[].choirs`.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
