# core.cloud

Создание / удаление cloud-инстансов через CloudDriver-плагин (`soul-cloud-*`).
**Keeper-side**, диспетчер `on: keeper` — шаг исполняется на самом Keeper-е, не
на хосте (в отличие от Soul-side core). Запуск без `on: keeper` — ошибка
валидации scenario. Заменяет более ранний паттерн «destiny `cloud-provision` с
`on: keeper`» ([ADR-017](../../../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read):
это keeper-side операция, не пакет задач для Soul). Реализация —
[`keeper/internal/coremod/cloud/provisioned.go`](../../../../keeper/internal/coremod/cloud/provisioned.go).

> **★ Author-адрес — `core.cloud.created` / `core.cloud.destroyed`** (base `core.cloud` + state).
> Именно это пишет оператор в `module:`. Форма `core.cloud.provisioned` **НЕ
> существует** как адрес задачи: реестр ([`registry.go`](../../../../keeper/internal/coremod/registry.go))
> делит адрес на base (`core.cloud`, идёт в `Lookup`) + state (`created`/`destroyed`,
> идёт в `ApplyRequest.state`), а `provisioned` — неизвестный state (integration-тест
> ловит её как fail). «provisioned» — историческое имя Go-пакета и формулировка ADR-017,
> не author-facing адрес. Имя этого файла оставлено `core/cloud/` по base-имени.

CloudDriver вызывается через PluginHost (gRPC-over-stdio плагин `soul-cloud-<provider>`).
SID создаваемого хоста = FQDN, который вернул провайдер (`VmInfo.fqdn`); VM без
fqdn — шаг падает (нельзя использовать как SID).

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `created` | Запросить у провайдера `count` VM; на каждую — `INSERT` в `souls` (`status: pending`) + `INSERT` в `bootstrap_tokens` (один токен на VM). | `changed=true` всегда (cloud-create — императивная операция, не идемпотентна на уровне модуля; повтор создаёт новые VM). |
| `destroyed` | `PluginHost.Destroy(vm_ids)` у провайдера; затем cascade одной PG-транзакцией над реестрами для переданных `sids`. | `changed=true`, если провайдер вернул непустой список удалённых VM; пустой список — `changed=false`. |

## created — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `provider` | string | required | Имя провайдера → плагин `soul-cloud-<provider>` через PluginHost. |
| `profile` | object | optional | Профиль провайдера (struct). Передаётся в CloudDriver как map профиля. |
| `count` | int | optional (default `1`) | Сколько VM создать. `< 1` — ошибка валидации. |

## destroyed — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `provider` | string | required | Имя провайдера. |
| `vm_ids` | array of string | required | Provider-side ID удаляемых VM (связку `sid↔vm_id` держит caller). |
| `sids` | array of string | optional | `SID`-ы, для которых выполнить cascade в реестрах после успешного destroy. Если опущен / пуст — cascade не выполняется. |

> Cascade (`destroyed`) выполняется **после** успешного `PluginHost.Destroy`:
> если cloud-destroy провалился, реестры остаются нетронутыми (хост ещё «жив»
> с точки зрения провайдера). Cascade одной PG-транзакцией переводит
> `souls → destroyed`, активные `soul_seeds → orphaned`, активные
> `bootstrap_tokens → burned`. Если `sids` непуст, но cascade-store не
> сконфигурирован в сборке — шаг падает с явной ошибкой.

## Capabilities / side-effects

- **Keeper-side, не трогает хост.** Side-effect-ы — у cloud-провайдера и в
  реестрах Keeper-а (Postgres), а не на Soul-хосте.
- **Создаёт / удаляет облачные VM** через CloudDriver-плагин (внешний
  биллинговый side-effect — реальные инстансы провайдера).
- **`created`:** `INSERT` в `souls` (`status: pending`, `transport: agent`) +
  `INSERT` bootstrap-токена на каждую VM.
- **`destroyed`:** при наличии `sids` — cascade-транзакция над
  `souls`/`soul_seeds`/`bootstrap_tokens`.
- **Пишет audit-event** `cloud.provisioned` (если audit-writer сконфигурирован):
  для `created` — `{action, provider, count, vm_ids}`; для `destroyed` —
  `{action, provider, vm_ids, sids, cascade-counts}`. Audit-фейл валит шаг
  (compliance-инвариант, событие обязательно).

## Безопасность

- **Keeper-side, не Soul-side — `root`/capability-семантика неприменима.** Шаг
  исполняется в процессе Keeper-а (`on: keeper`); side-effect-ы — у cloud-провайдера
  (через CloudDriver-плагин `soul-cloud-*`) и в Postgres-реестрах Keeper-а, не на
  хосте. Манифеста с `required_capabilities` у модуля нет (keeper-internal операция,
  не host-плагин). Запуск такого scenario регулируется RBAC оператора
  ([rbac.md](../../../keeper/rbac.md)); создаваемые записи `souls` пишутся с
  `CreatedByAID: null` (keeper-internal action).
- **Реальный финансовый side-effect (`created`).** Шаг создаёт настоящие VM у
  провайдера — это биллинг. `created` **не идемпотентен** конструктивно
  (`changed=true` всегда): повтор шага создаёт **новые** VM, а не сверяется с
  существующими. Управляйте повтором guard-ом на уровне scenario
  (`when:`/`changed_when:`), не полагаясь на идемпотентность модуля.
- **`destroyed` — деструктивная cascade-операция.** `PluginHost.Destroy(vm_ids)`
  физически уничтожает инстансы; затем (при непустом `sids`) одна PG-транзакция
  переводит `souls → destroyed`, активные `soul_seeds → orphaned`,
  `bootstrap_tokens → burned`
  ([`provisioned.go`](../../../../keeper/internal/coremod/cloud/provisioned.go)).
  Порядок защищает реестры: cascade выполняется **после** успешного cloud-destroy —
  при провале destroy реестры остаются нетронутыми (хост ещё «жив» у провайдера).
  Связку `sid↔vm_id` держит caller — ошибка в ней приведёт к destroy не той VM,
  поэтому источник `vm_ids`/`sids` должен быть доверенным.
- **Cascade `souls→destroyed` предшествует host-teardown в destroy-прогоне (NIM-56).**
  `core.cloud.destroyed` — keeper-задача; по инварианту «keeper-задачи идут ПЕРВЫМИ в
  своём Passage» она снимает VM и каскадит `souls→destroyed` РАНЬШЕ, чем host-fan-out
  host-teardown-шаги того же destroy-сценария (у сервисов с Soul-side teardown, напр.
  `dragonfly`). Такой host-шаг диспатчится на уже снятый хост — в destroy-прогоне
  (`TerminalDestroy`) scenario-runner при claim трактует хост, снятый СОБСТВЕННЫМ
  destroy-каскадом этого прогона (`souls.status == 'destroyed'`), как benign-терминал
  **`no_match`**, НЕ `dispatch_failed`: барьер засчитывает его на success-сторону, и
  teardown не валится в `destroy_failed`. Дискриминатор однозначен — единственный писатель
  статуса `destroyed` — эта cascade-транзакция (`CascadeDestroy`); любое другое выпадение
  хоста из roster-а (disconnected / revoked / not-found) остаётся отказом (**fail-closed**).
- **Plain bootstrap-token в register-output (`created`).** `hosts[].bootstrap_token`
  — **plain** одноразовый токен, намеренно в output: cloud-init flow обязан
  передать его на VM при первичной загрузке (единственный момент, когда plain-токен
  виден; в БД — только hash, восстановить нельзя). Секретность держится
  substring-фильтром [`audit.MaskSecrets`](../../../../shared/audit/) (фрагмент
  `token`) на **всех** выходах register-output (audit-log / OTel / SSE / любые
  логи). **Любой новый канал вывода register-output обязан прогонять payload через
  `audit.MaskSecrets`; переименовывать ключ `bootstrap_token` без проверки фильтра
  нельзя** — иначе one-time token leak.
- **Обязательный audit-event `cloud.provisioned`.** Пишется и для `created`, и для
  `destroyed`; audit-фейл **валит шаг** (compliance-инвариант — деструктивная/
  биллинговая операция не должна пройти молча). В audit-payload — `provider`,
  `vm_ids`, `sids`, cascade-счётчики, но **не** plain-токены.

## Output / register

`created` отдаёт:

| Поле | Тип | Описание |
|---|---|---|
| `hosts` | array of object | По одной записи на VM: `{sid, vm_id, primary_ip, attributes?, bootstrap_token}`. |
| `count` | number | Число созданных VM. |
| `vm_ids` | array of string | Provider-side ID созданных VM. |
| `action` | string | `created`. |

> **ВНИМАНИЕ (security).** `hosts[].bootstrap_token` — это **plain** одноразовый
> токен. Он намеренно в register-output: cloud-init flow обязан передать его на
> VM при первичной загрузке — это единственный момент, когда plain-токен виден
> (в БД хранится только hash, восстановить нельзя). Секретность ключа
> `bootstrap_token` держится substring-фильтром [`audit.MaskSecrets`](../../../../shared/audit/)
> (фрагмент `token`) на **всех** выходах register-output (audit-log / OTel / SSE
> / любые логи). Любой новый канал вывода register-output обязан прогонять payload
> через `audit.MaskSecrets`; переименовывать ключ без проверки фильтра нельзя.

`destroyed` отдаёт:

| Поле | Тип | Описание |
|---|---|---|
| `action` | string | `destroyed`. |
| `vm_ids` | array of string | Фактически удалённые провайдером VM. |
| `sids` | array of string | Эхо переданных `sids`. |
| `destroyed_n` | number | Число удалённых VM. |
| `souls_updated` / `seeds_orphaned` / `tokens_burned` | number | Cascade-счётчики (0, если `sids` не передан). |

## Пример

```yaml
# Если нужно — создаём VM через CloudDriver. on: keeper обязателен —
# это keeper-side core. when:-guard — spawn опционален.
- name: provision
  on: keeper
  when: has(input.spawn)
  module: core.cloud.provisioned
  params:
    provider: "${ input.spawn.provider }"
    profile:  "${ input.spawn.profile }"
    count:    "${ input.spawn.count }"
```

(из [`examples/service/example-cloud-bootstrap/scenario/create/main.yml`](../../../../examples/service/example-cloud-bootstrap/scenario/create/main.yml);
пример передаёт ровно `provider`/`profile`/`count` — это полный набор params,
которые `provisioned.go` валидирует для `created`).

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [keeper/modules.md](../../../keeper/modules.md) — нормативная спека Keeper-side core-модулей (диспетчер `on: keeper`).
- [scenario/orchestration.md §3](../../../scenario/orchestration.md#3-таргет-шага--on) — `on:`, диспетчер шага между Soul-стороной и Keeper-стороной.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-017](../../../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) — Keeper-side core-модули, cascade при `destroyed`.
