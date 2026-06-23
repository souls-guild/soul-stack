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

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `registered` | Soul с указанным `sid` находится в реестре и привязан к указанному набору Coven-меток (по `mode`). | `changed=true`, если запись `souls` была создана модулем **или** итоговый набор coven отличается от текущего (порядок-независимая сверка). Набор совпал и запись уже была — `changed=false`. |

## registered — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `sid` | string | required | `SID` Soul-а (FQDN). Валидируется как FQDN (`keepersoul.ValidSID`); невалидный — шаг падает. |
| `coven` | array of string | required | Набор стабильных Coven-меток. Каждая метка валидируется как kebab-case (`keepersoul.ValidCoven`, 1..63 символа); мусор вроде `Prod`/`a_b` — шаг падает. |
| `mode` | string | optional (default `append`) | Стратегия применения набора `coven` к существующим меткам: `append` / `replace` / `remove`. Неизвестное значение — шаг падает. |
| `refresh_soulprint` | bool | optional | Принимается и валидируется, но в MVP **игнорируется** (scenario-runner keeper-стороны ещё не интегрирован). В output всегда `refreshed: false` — поле эхо-выдаётся для стабильной формы input-схемы. |

### Семантика `mode`

| `mode` | Итоговый набор coven | Поведение по краям |
|---|---|---|
| `append` (default) | существующие ∪ переданные | повтор с тем же набором — no-op (`changed=false`). |
| `replace` | переданные (не упомянутые — удаляются) | пустой `coven: []` — **ошибка** (footgun-защита: хост должен сохранить хотя бы одну метку). |
| `remove` | существующие \ переданные | метки, которых нет на хосте, — пропускаются без ошибки; снимает только реально привязанные. |

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

## Output / register

Модуль отдаёт в `register.<name>.*`:

| Поле | Тип | Описание |
|---|---|---|
| `sid` | string | Эхо `params.sid`. |
| `coven` | array of string | **Итоговый** набор coven после применения `mode` (sorted), а не переданный аргумент. |
| `mode` | string | Применённый `mode`. |
| `created` | bool | `true`, если запись `souls` была создана модулем; `false`, если уже существовала. |
| `refreshed` | bool | Всегда `false` в MVP (см. `refresh_soulprint`). |
| `removed` | array of string | Только при `mode: remove`: метки, фактически снятые. Пустой массив иначе. |

Плюс стандартные `.changed` / `.failed` DSL-ядра.

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

(см. [`examples/destiny/coven-assign/tasks/main.yml`](../../../../examples/destiny/coven-assign/tasks/main.yml) — destiny-обёртка вокруг `core.soul.registered`, и [`examples/service/keeper-register/scenario/create/main.yml`](../../../../examples/service/keeper-register/scenario/create/main.yml) — keeper-side dispatch в scenario)

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [keeper/modules.md](../../../keeper/modules.md) — нормативная спека Keeper-side core-модулей (диспетчер `on: keeper`).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-таргет-шага--on) — `on:`, диспетчер шага между Soul-стороной и Keeper-стороной.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-017](../../../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) — Keeper-side core-модули.
