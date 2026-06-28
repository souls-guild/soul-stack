# core.vault

Keeper-side core-модуль для работы с Vault KV-секретами. Один модуль (base-имя
`core.vault`, ключ Registry) с диспетчеризацией по **state** (паттерн
`core.cloud` / `core.choir`): author-адрес задачи — base + state.

| Author-адрес | State | Назначение |
|---|---|---|
| `core.vault.kv-read` | `kv-read` (verb) | Явное чтение секрета из Vault KV с записью audit-event-а. |
| `core.vault.kv-present` | `kv-present` | Generate-if-absent: гарантировать существование секретов, сгенерировав недостающие криптослучайным значением по password-policy. |

**Keeper-side**, диспетчер `on: keeper` — оба state исполняются на самом Keeper-е,
не на хосте (в отличие от Soul-side core). Запуск без `on: keeper` — ошибка
валидации scenario. Реализация — [`kvread.go`](../../../../keeper/internal/coremod/vault/kvread.go)
(базовый модуль + `kv-read`), [`kvpresent.go`](../../../../keeper/internal/coremod/vault/kvpresent.go)
(`kv-present`), [`policy.go`](../../../../keeper/internal/coremod/vault/policy.go)
(password-policy генерации).

Зачем эти явные state при наличии implicit `${ vault(...) }` в CEL: implicit-vault
дёшев для рендера, но **не** оставляет отдельной записи в audit-trail и только
читает. `kv-read` — explicit-форма для случаев, требующих audit-event-а
`vault.kv-read` (PCI-DSS, SOC2, compliance-аккуратный код). `kv-present` — write-форма
(генерация секрета на месте), которой у CEL-резолва нет вовсе. implicit
`${ vault(...) }` при этом остаётся для render-фазы — это разные моменты.

---

# core.vault.kv-read

Явное чтение секрета из Vault KV (v1/v2, версия mount-а определяется автоматически)
на keeper-стороне с записью audit-event-а.

## kv-read — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Путь секрета в Vault, **mount-relative и без сегмента `data/`** — клиент сам подставит `data/` для KV v2 (для KV v1 — без него). Указывать `secret/redis/admin`, а не `secret/data/redis/admin` (иначе клиент построит `secret/data/data/redis/admin` и секрет не найдётся). Версия KV mount-а (v1/v2) резолвится в keeper-side `vault.Client` (автоопределение через probe, либо override `vault.kv_version`, см. [config.md → vault](../../../keeper/config.md#vault)); модуль получает уже **плоский payload** и работает на обеих версиях одинаково (для v2 обёртка `data.data` распакована клиентом до передачи в модуль). |
| `fields` | array of string | optional | Какие ключи вернуть в `data`. Пусто / не задан → весь payload. Запрошенный, но отсутствующий ключ — **пропускается без ошибки** (audit-event на чтение уже потрачен). |

## kv-read — capabilities / side-effects

- **Keeper-side, не трогает хост.** Чтение из Vault на keeper-стороне; на Soul
  ничего не доставляется.
- **Не мутирует state** (read-операция, `changed=false`).
- **Пишет audit-event** `vault.kv-read` (если audit-writer сконфигурирован) с
  payload `{path, fields}` — **только факт чтения**, без значений секретов.
  Audit-фейл валит шаг (иначе обязательный compliance-шаг молча пропадёт).

## kv-read — output / register

`kv-read` отдаёт в `register.<name>.*`:

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

## kv-read — пример

```yaml
# Явное чтение секрета на keeper-стороне ради audit-event vault.kv-read.
# on: keeper обязателен — это keeper-side core. fields опционален: без него
# вернётся весь payload.
- name: Read DB credentials from Vault (audit-tracked)
  on: keeper
  module: core.vault.kv-read
  register: db_creds
  params:
    path:   secret/redis/admin
    fields: [username, password]
```

(минимальный валидный пример: вызова `core.vault.kv-read` в `examples/`
пока нет — см. deferred-заметку ниже).

> **Deferred (backlog).** В `examples/` сейчас нет ни одного вызова
> `core.vault.kv-read` — пример выше составлен как минимальный валидный по
> контракту кода (`path` required, `fields` optional). Замена на ссылку
> на реальный scenario-example отложена до появления соответствующего
> use-case (compliance-аккуратный read с audit-event-ом).

---

# core.vault.kv-present

Generate-if-absent для Vault KV-секретов: для каждого target гарантирует, что
указанное поле существует и непусто; отсутствующее — генерирует криптослучайным
значением по описанной автором **password-policy** (длина в символах + алфавит),
присутствующее — оставляет как есть (**не перезатирает**). Author-адрес — base
`core.vault` + state `kv-present`. Парный близнец `kv-read` на **том же** модуле
([ADR-017 amendment 2026-06-28](../../../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)).

Назначение — сервис **сам** генерит недостающие пароли при `create`, оператору не
нужно пред-сеять секреты ручным `vault kv put`. Типично — bootstrap-секреты
сервиса при первом развёртывании (redis: главный пароль + per-user ACL-пароли).

## kv-present — семантика

- Для каждого target читается путь + поле (несуществующий путь — не ошибка, а
  пустой payload: все его поля будут сгенерированы).
- **ОТСУТСТВУЕТ** (поля нет, оно `null` или **пустая строка**) → генерируется
  значение по policy и пишется `WriteKV`. Пустая строка трактуется как «нет»
  (пустой пароль бесполезен).
- **ПРИСУТСТВУЕТ** (непустая строка) → **no-op**, значение не трогается.
- `changed=true` **только** когда что-то реально сгенерировано; если все секреты
  уже были — `changed=false`.
- **Идемпотентно.** Повторный прогон / re-create безопасны: уже существующие
  секреты переиспользуются, новых KV-версий без надобности не создаётся.
- **Несколько target-ов на один путь** (разные поля) сливаются в **один**
  `WriteKV` поверх существующих полей пути (read-merge-write) — соседние поля не
  теряются, лишние KV-версии не плодятся.
- **`destroy` секреты НЕ чистит** — re-create переиспользует те же пароли
  (ротации / удаления секретов этот state не делает; ротация — отдельный сценарий).

## kv-present — params

Step-level `policy` задаёт общий дефолт генерации для всех target-ов без своего
`policy`; per-target `policy` переопределяет его пополям. Отсутствует и там, и там
→ дефолт (`length: 32`, `charset: ascii-printable-safe`).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `policy` | object | optional | Step-level дефолт password-policy (см. ниже). |
| `targets` | array of object | **required, непустой** | Что гарантировать. Пустой / отсутствующий список — ошибка валидации (модулю нечего делать). |
| `targets[].path` | string | required | Vault KV-путь (mount-relative, без `data/` и **без** `#field` — поле задаётся отдельно). Та же конвенция пути, что у `kv-read`. |
| `targets[].field` | string | optional (default `password`) | Имя поля внутри секрета. |
| `targets[].policy` | object | optional | Override password-policy для этого target поверх step-level. |

### password-policy (объект `policy`)

| Поле | Тип | Required / default | Смысл |
|---|---|---|---|
| `length` | int | optional (default `32`) | Итоговая длина пароля **в символах** (не в байтах энтропии). Границы `8..1024`; вне границ — ошибка валидации. |
| `charset` | string (enum) | optional (default `ascii-printable-safe`) | Именованный пресет алфавита: `alphanumeric` / `hex` / `base64url` / `ascii-printable-safe`. **Взаимоисключим с `allowed_chars`.** |
| `allowed_chars` | string | optional | Явный алфавит разрешённых символов (дубликаты схлопываются). Алфавит должен иметь **≥ 2** различных символа. **Взаимоисключим с `charset`.** |

Пресеты `charset`:

| Пресет | Алфавит |
|---|---|
| `alphanumeric` | латиница обоих регистров + цифры (безопасно везде; уже по энтропии на символ). |
| `hex` | строчные hex-цифры `0-9a-f`. |
| `base64url` | url-safe base64 (`-`/`_` вместо `+`/`/`, без `=`). |
| `ascii-printable-safe` (default) | печатный ASCII `0x21..0x7E` **минус** символы, ломающие redis.conf / users.acl / shell-подстановку: пробел, `"`, `'`, `#`, `\`, а также `` ` `` и `$`. Дефолт — пароль не должен ломать целевой конфиг. |

> `charset` и `allowed_chars` нельзя задавать вместе (двусмысленность алфавита) —
> это ошибка валидации. Пустой `allowed_chars` / неизвестный `charset` —
> тоже ошибка.

## kv-present — capabilities / side-effects

- **Keeper-side, не трогает хост.** Запись в Vault на keeper-стороне; на Soul
  ничего не доставляется.
- **Пишет в Vault KV** (`WriteKV`) только пути с реально сгенерированными полями,
  одним merge поверх существующего payload пути.
- **Мутирует state условно:** `changed=true` только при фактической генерации;
  no-op (всё уже было) → `changed=false`.
- **Пишет audit-event** `vault.kv-present` (если audit-writer сконфигурирован)
  **только при `changed=true`** с payload `{paths}` (путь → список
  сгенерированных полей, **без значений**). Audit-фейл валит шаг.

## kv-present — output / register

`kv-present` отдаёт в `register.<name>.*`:

| Поле | Тип | Описание |
|---|---|---|
| `generated` | object | Map `<vault-path>` → **отсортированный список имён** сгенерированных полей. **Без значений.** Пусто, если ничего не сгенерировано. |

Плюс стандартные `.changed` / `.failed` DSL-ядра.

## kv-present — безопасность (★ ADR-010)

- **Сгенерированное значение НИКОГДА не уходит наружу.** Ни в register-output, ни
  в audit-payload, ни в логи, ни в OTel, ни в текст ошибки. Наружу — только
  факт + `path` + **имена** сгенерированных полей
  ([`kvpresent.go`](../../../../keeper/internal/coremod/vault/kvpresent.go),
  эталон `sigil.KeyService.Introduce`). Ошибки `WriteKV` / `ReadKV` несут только
  `path` (инвариант `vault.Client`). Этот инвариант закреплён guard-тестом
  (`kvpresent_test.go::TestPresent_SecurityNoLeak` — рекурсивно проверяет
  отсутствие значения, включая substring, во всём дереве output и payload).
- **crypto/rand, bias-free.** Источник случайности — `crypto/rand` (НЕ
  `math/rand`); индекс символа выбирается равномерно (`rand.Int` — rejection
  sampling, без modulo-перекоса).
- **Keeper-side, не Soul-side — `root`/capability-семантика неприменима.** Генерация
  идёт в процессе Keeper-а (`on: keeper`); манифеста с `required_capabilities` у
  модуля нет (keeper-internal операция, не host-плагин). Запуск scenario с этим
  шагом регулируется RBAC оператора ([rbac.md](../../../keeper/rbac.md)).
- **Требует Vault-auth Keeper-а.** Чтение и запись выполняются клиентом
  `keeper/internal/vault` под учёткой Keeper-кластера — модуль не принимает
  токен/креды в params и не повышает доступ; он пишет ровно туда, куда разрешено
  политикой Keeper-а в Vault.

> **ВНИМАНИЕ (security).** В отличие от `kv-read` (где значения присутствуют в
> register-output и держатся маскингом), `kv-present` **имён значений не несёт
> вовсе** — ни output, ни audit-payload не содержат сгенерированных секретов,
> только пути и имена полей.

## kv-present — пример

Из redis-сценария `create` ([`examples/service/redis/scenario/create/main.yml`](../../../../examples/service/redis/scenario/create/main.yml),
первый шаг тела): генерация главного пароля Redis + per-user ACL-паролей **до**
любого чтения этих секретов через `${ vault(...) }` в render-фазе. policy —
`alphanumeric` / `length 32` (безопасный для `requirepass` / директив ACL алфавит:
спецсимволы ломали бы парсинг).

```yaml
# Сервис сам генерит недостающие пароли (generate-if-absent), оператор не пред-сеет.
# on: keeper обязателен. policy — настоящий YAML-map (не CEL-строка).
- name: Ensure redis passwords exist in Vault (generate if absent)
  on: keeper
  module: core.vault.kv-present
  params:
    policy:
      length: 32
      charset: alphanumeric
    # targets вычисляются из того же essence/input, что и читающие задачи деплоя
    # (drift «что генерим ≡ что читаем» = баг): главный secret/redis/<inc>#password
    # + per-user secret/redis/<inc>/users/<name>#password.
    targets: "${ [{ 'path': 'secret/redis/' + incarnation.name, 'field': 'password' }] + ... }"
```

> В реальном сценарии `targets` — однострочный CEL-`${…}` (а не block-scalar
> `>-`): module.params type-check пропускает CEL-обёртку только для строкового
> скаляра; block-scalar парсился бы как литерал и отверг бы list-param. Список
> юзеров не хардкодится — собирается из `essence.system_acl_users` ∪
> `system_acl_users_sentinel` + `input.users`.

> **Passage-инвариант.** Этот шаг обязан исполниться (запись в Vault) **до**
> render-фазы задач, читающих те же секреты через `${ vault(...) }` (модель
> staged-render [ADR-056](../../../adr/0056-staged-render-passage.md#adr-056-staged-render--прогон-сценария-как-n-упорядоченных-passage-probewhere-реально-работает)). В redis-create
> ребро generate→read несёт roster-ось (refresh-эмиттер + roster-потребление
> деплоя), а не register — поэтому у шага `register` намеренно нет (его результат
> никто не потребляет). Несущий инвариант закреплён guard-тестом
> `keeper/internal/render/redis_create_secrets_passage_test.go`.

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [keeper/modules.md](../../../keeper/modules.md) — нормативная спека Keeper-side core-модулей (диспетчер `on: keeper`).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-таргет-шага--on) — `on:`, диспетчер шага между Soul-стороной и Keeper-стороной.
- [templating.md](../../../templating.md) — vault-resolve фаза и implicit `${ vault(...) }` в CEL.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-017](../../../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) — Keeper-side core-модули; `kv-read` (явный vs implicit vault), `kv-present` (amendment 2026-06-28, generate-if-absent + security-инвариант).
