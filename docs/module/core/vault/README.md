# core.vault.kv-read

Явное чтение секрета из Vault KV v2 на keeper-стороне с записью audit-event-а.
**Keeper-side**, диспетчер `on: keeper` — шаг исполняется на самом Keeper-е, не
на хосте (в отличие от Soul-side core). Запуск без `on: keeper` — ошибка
валидации scenario. Реализация —
[`keeper/internal/coremod/vault/kvread.go`](../../../../keeper/internal/coremod/vault/kvread.go).

Зачем существует при наличии implicit `${ vault(...) }` в CEL: implicit-vault
дёшев для рендера, но **не** оставляет отдельной записи в audit-trail. Этот модуль
— explicit-форма для случаев, требующих явного audit-event-а `vault.kv-read`
(PCI-DSS, SOC2, compliance-аккуратный код). implicit `${ vault(...) }` при этом
остаётся для render-фазы — это два разных момента.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `read` (verb) | Прочитать секрет из Vault KV v2 и отдать выбранные поля в register-output. | **Никогда** `changed=true`: read-операция, state не мутирует (`changed=false` всегда). |

## read — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Путь секрета в Vault. KV v2 формат `data.data` распаковывается автоматически (`extractKVData`); legacy KV v1 без обёртки — отдаётся как есть. |
| `fields` | array of string | optional | Какие ключи вернуть в `data`. Пусто / не задан → весь payload. Запрошенный, но отсутствующий ключ — **пропускается без ошибки** (audit-event на чтение уже потрачен). |

## Capabilities / side-effects

- **Keeper-side, не трогает хост.** Чтение из Vault на keeper-стороне; на Soul
  ничего не доставляется.
- **Не мутирует state** (read-операция, `changed=false`).
- **Пишет audit-event** `vault.kv-read` (если audit-writer сконфигурирован) с
  payload `{path, fields}` — **только факт чтения**, без значений секретов.
  Audit-фейл валит шаг (иначе обязательный compliance-шаг молча пропадёт).

## Безопасность

- **Keeper-side, не Soul-side — `root`/capability-семантика неприменима.** Чтение
  из Vault идёт в процессе Keeper-а (`on: keeper`); на Soul ничего не доставляется.
  Манифеста с `required_capabilities` у модуля нет (keeper-internal операция, не
  host-плагин). Запуск scenario с этим шагом регулируется RBAC оператора
  ([rbac.md](../../../keeper/rbac.md)).
- **Требует Vault-auth Keeper-а.** Чтение выполняется клиентом
  `keeper/internal/vault` под учёткой Keeper-кластера
  ([`kvread.go`](../../../../keeper/internal/coremod/vault/kvread.go)) — модуль не
  принимает токен/креды в params и не повышает доступ; он читает ровно то, что
  разрешено политикой Keeper-а в Vault.
- **Значения секретов маскируются на выходе.** В register-output значения
  присутствуют (`data.*`) — это смысл чтения, CEL обрабатывает их нормально, — но на
  write-path-е destiny/scenario маскируются через
  [`audit.MaskSecrets`](../../../../shared/audit/) (известные секретные ключи) на
  всех каналах: логи / OTel / UI / отчёты. Маскинг — аспект выхода, не самого
  чтения.
- **Audit-trail — причина существования модуля.** Пишет обязательный audit-event
  `vault.kv-read` с payload `{path, fields}` — **только факт чтения, без значений
  секретов**. Audit-фейл **валит шаг** (иначе обязательный compliance-шаг молча
  пропал бы). Этим модуль отличается от implicit `${ vault(...) }` в CEL, который
  читает дёшево, но отдельной audit-записи не оставляет.
- **Read-only.** `changed=false` всегда — чтение не мутирует state; запрошенный, но
  отсутствующий ключ `fields` пропускается без ошибки (audit-event на чтение уже
  потрачен).

## Output / register

`read` отдаёт в `register.<name>.*`:

| Поле | Тип | Описание |
|---|---|---|
| `path` | string | Эхо запрошенного пути. |
| `data` | object | Извлечённые ключ→значение (после фильтра `fields`). |
| `fields` | array of string | Список ключей в `data` (sorted). |

Плюс стандартные `.changed` (всегда `false`) / `.failed` DSL-ядра.

> **ВНИМАНИЕ (security).** Сами значения секретов в **audit-payload не
> попадают** — фиксируется только `path` + список `fields`. В register-output
> значения присутствуют (`data.*`), но маскируются на write-path-е
> destiny/scenario через [`audit.MaskSecrets`](../../../../shared/audit/) (известные
> секретные ключи) на всех выходах — логи / OTel / UI / отчёты. CEL обрабатывает
> значения нормально; маскинг — на выходе.

## Пример

```yaml
# Явное чтение секрета на keeper-стороне ради audit-event vault.kv-read.
# on: keeper обязателен — это keeper-side core. fields опционален: без него
# вернётся весь payload.
- name: Read DB credentials from Vault (audit-tracked)
  on: keeper
  module: core.vault.kv-read
  register: db_creds
  params:
    path:   secret/data/redis/admin
    fields: [username, password]
```

(минимальный валидный пример: вызова `core.vault.kv-read` в `examples/`
пока нет — см. deferred-заметку ниже).

> **Deferred (backlog).** В `examples/` сейчас нет ни одного вызова
> `core.vault.kv-read` — пример выше составлен как минимальный валидный по
> контракту кода (`path` required, `fields` optional). Замена на ссылку
> на реальный scenario-example отложена до появления соответствующего
> use-case (compliance-аккуратный read с audit-event-ом).

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [keeper/modules.md](../../../keeper/modules.md) — нормативная спека Keeper-side core-модулей (диспетчер `on: keeper`).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-таргет-шага--on) — `on:`, диспетчер шага между Soul-стороной и Keeper-стороной.
- [templating.md](../../../templating.md) — vault-resolve фаза и implicit `${ vault(...) }` в CEL.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-017](../../../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) — Keeper-side core-модули, явный vs implicit vault.
