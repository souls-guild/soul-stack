# Эксплуатация: рабочий цикл оператора

Этот гайд — следующий шаг после [first-service.md](first-service.md). Там ты собрал сервис, зарегистрировал его и создал инкарнацию; здесь ты её **эксплуатируешь**: повторно прогоняешь, проверяешь дрейф, таргетишь часть флота, апгрейдишь версию, масштабируешь и разбираешь инциденты.

Это task-oriented гайд: каждый раздел — конкретная задача «как сделать X» с реальной командой. Полная нормативная грамматика — по ссылкам, а runbook-уровень эксплуатации (HA, восстановление, sizing) — в [docs/operations/](../operations/README.md). Этот гайд — мост между ними: что оператор делает в обычный день, не уходя в SRE-детали.

## Предпосылки

Всё из [first-service.md](first-service.md): рабочий Keeper, токен Архонта в `TOKEN`, инкарнация `hello-demo` под coven `demo`. CLI `soulctl` — тонкая обёртка над Operator API ([ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)); где возможности нет в CLI — показан прямой вызов API.

> **О реальных covens.** dev-стенд из [getting-started.md](../getting-started.md) поднимает флот под coven `demo`. В примерах таргетинга ниже фигурируют `prod` / `staging` / `dev` — это иллюстрация многосредового флота из 9 душ: covens — просто стабильные метки, которые ты присваиваешь хостам при онбординге ([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). На своём стенде подставляй `demo`.

## 1. Повторный прогон / обновление состояния

Создание инкарнации запускает сценарий `create` один раз. Чтобы прогнать сценарий повторно (тот же `create` с новым `input` или другую операцию сервиса) на **существующей** инкарнации — `soulctl incarnation run`:

```sh
# повторить create с новым greeting (re-apply: хосты сойдутся к новому состоянию)
soulctl incarnation run hello-demo create --input '{"greeting":"hi again"}' --wait
```

`--wait` поллит статус до завершения apply и печатает финальный `status` + `history_id`. Без `--wait` команда возвращает `apply_id` сразу (операция асинхронная). Флаги:

- `--input '<json>'` — input сценария (валидируется Keeper-ом против `input:`-контракта **до** запуска);
- `--dry-run` — прогнать сценарий в dry-run-режиме без мутаций (см. раздел 2 — это и есть механика Scry);
- `--wait-timeout 5m` — потолок ожидания для `--wait`.

Под капотом это `POST /v1/incarnations/{name}/scenarios/{scenario}` ([operator-api/incarnations.md](../keeper/operator-api/incarnations.md)). Изменить input инкарнации — это и есть повторный прогон с новым `--input`: state перепишется только после успеха на **всех** хостах (cross-host barrier, [orchestration.md → §7](../scenario/orchestration.md#7-инвариант-barrier--state-commit)), иначе инкарнация уходит в `error_locked` (раздел 6).

Посмотреть, что записалось в state, и историю прогонов:

```sh
soulctl incarnation get hello-demo          # spec / state / status / covens
soulctl incarnation history hello-demo      # state_history: apply_id / scenario / кто запустил
```

Сводка всех способов запуска работы (одиночный run / батч через Voyage / push) — [run-flavors.md](../keeper/run-flavors.md).

## 2. Проверка дрейфа (check-drift / Scry)

**Что такое drift.** Между прогонами кто-то мог изменить хост руками, другой системой или сервис сам переписал свой конфиг — и до следующего apply об этом никто не узнает. **Scry** ([ADR-031](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) отвечает на вопрос «применился бы хоть один ресурс заново, если прогнать reconcile прямо сейчас». Нормативно: **drift = dry-run reconcile показал бы `changed=true` хотя бы на одном ресурсе** по **декларированным** ресурсам. Это declarative drift (как Salt `test=True` / Ansible `--check`), а **не** полный дамп хоста: Scry видит расхождение только по тому, что объявлено в сценарии, неуправляемые ресурсы ему невидимы by design.

**Что нужно от сервиса.** Drift-проверка читает `scenario/converge/main.yml` — это «желаемое состояние» сервиса. В [first-service.md](first-service.md) этот сценарий уже лежит рядом с `create` (раздел «Раскладка service-репо»). Если файла нет — `check-drift` вернёт `422 ErrConvergeMissing` и инкарнацию не тронет ([faq.md](../operations/faq.md#check-drift-возвращает-422-errconvergemissing)).

**Как запустить:**

```sh
soulctl incarnation check-drift hello-demo
```

Параметры `converge` авто-резолвятся: для каждого входа значение берётся из `--input`-override → `incarnation.state.<имя>` → default схемы (это даёт «auto-drift из state» без ручной передачи). Команда синхронная, таймаут CLI — 5 минут (Soul обходит весь сценарий в Plan-режиме). API-эквивалент — `POST /v1/incarnations/{name}/check-drift`.

**Как читать результат.** CLI печатает summary и таблицу по хостам:

```
incarnation: hello-demo
scenario:    converge
checked_at:  2026-06-16T12:30:00Z
summary:     drifted=1 clean=3 unsupported=0 failed=0

SID                STATUS    TASKS_DRIFTED
host-01.internal   drifted   1/2
host-02.internal   clean     0/2
```

Per-host `status`: `clean` (всё совпадает), `drifted` (хотя бы один ресурс показал `changed`), `unsupported` (модуль без read-safe-Plan — например verb-модули `core.exec.run`/`core.cmd.shell`, у них нет «желаемого состояния», это норма, не ошибка), `failed` (Plan упал). За полным per-task-разбором (`idx`/`module`/`action`/`changed`/`message`) бери `-o json`.

**Что делать с дрейфом.** Статус `drift` на инкарнации **информационный, не блокирующий** ([ADR-031(d)](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): он не запирает инкарнацию. Remediation — обычный apply: прогони `converge` как operational-сценарий, хосты сойдутся к декларации, статус вернётся в `ready`:

```sh
soulctl incarnation run hello-demo converge --wait
```

(`converge` — это operational-сценарий с двойной ролью: как `run` он реально сводит хосты к состоянию, как target `check-drift` — делает declarative dry-run, [ADR-031 amendment 2026-06-10](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile).) Если предпочитаешь повторить исходную операцию — `run hello-demo create --input ...` (раздел 1). Фоновый периодический скан есть, но по умолчанию выключен (`reaper.scry_background.enabled=false`) — включается осознанным решением на проде.

## 3. Таргетинг по флоту

По умолчанию `run` без `on:` в шаге бьёт по **всему incarnation** — все хосты под корневым coven `${ incarnation.name }`. Чтобы прогнать только на части флота, есть два механизма ([orchestration.md → §3–§4](../scenario/orchestration.md#3-таргет-шага--on)):

**Стабильный таргет — `on:` (covens).** В сценарии шаг таргетится на пересечение стабильных covens, ⊆ incarnation:

```yaml
tasks:
  - name: Применить только на prod-хостах
    module: core.file.present
    on: [prod]            # 4 хоста с coven prod из 9 во флоте
    params: { path: /tmp/marker, content: "prod only" }
```

Covens — стабильные логические метки хоста (окружение / ЦОД / проект), роль хоста **не Coven** ([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). Какие covens у душ — видно в `soulctl souls list` (или `GET /v1/souls`).

**Волатильный предикат — `where:` (late-binding по фактам).** Per-host фильтр, который вычисляется в момент прогона по фактам хоста (`soulprint.self.*`) или по результату probe-шага (`register.*`). Это и есть late-binding: новый хост, появившийся в coven к моменту прогона, автоматически попадает под предикат, его не надо перечислять:

```yaml
tasks:
  - name: Только на Debian-семействе
    module: core.pkg.present
    where: "soulprint.self.os.family == 'debian'"
    params: { name: nginx }
```

Различие позиций `where:`-ключа шага и функции `soulprint.where(...)` — [orchestration.md → §4](../scenario/orchestration.md#where-ключ-шага-vs-soulprintwhere-функция--разные-позиции). Полный список фактов для предикатов (os-family, pkg_mgr, primary_ip, …) — [soulprint.md](../soul/soulprint.md).

> **Кросс-incarnation таргетинг запрещён грамматикой** — `on:`/`where:` бьют только по хостам своей инкарнации. Данные о других хостах прогона — через `soulprint.hosts` ([orchestration.md → §4.1](../scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор)).

## 4. Апгрейд версии сервиса

Версия сервиса — это git-ref (tag или branch), под которым закоммичены его файлы; поля `version:` в манифесте нет ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). Апгрейд инкарнации на новую версию — `POST /v1/incarnations/{name}/upgrade` с целевым `to_version` (этой операции в `soulctl` нет — через API):

```sh
curl -s -X POST http://127.0.0.1:8080/v1/incarnations/hello-demo/upgrade \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"to_version": "v2.0.0"}'
```

Ответ `202 Accepted` с `apply_id` — операция асинхронная. `to_version` — любой git-ref (tag `v2.0.0` или branch `main`); если он совпадает с текущей версией инкарнации, Keeper вернёт ошибку «апгрейдить нечего».

**Когда меняется `state_schema_version`.** Если новая версия сервиса подняла `state_schema_version` (breaking-изменение структуры `incarnation.state`), upgrade применит миграции `migrations/<NNN>_to_<MMM>.yml` атомарно одной PG-транзакцией: при сбое — rollback и статус `migration_failed` (раздел 6). Это **явный шаг оператора**, не lazy; `state_history` хранит snapshot per-change для восстановления. Бэкап перед апгрейдом со сменой схемы, откат и форму миграций — [operations/upgrade.md → State_schema migrations](../operations/upgrade.md#state_schema-migrations) и нормативная грамматика DSL ([migrations.md](../migrations.md), [ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

Доступные ref-ы для апгрейда (теги + ветки сервиса) — `GET /v1/services/{name}/refs`.

## 5. Масштаб — добавить хост в coven

Добавить душу во флот — это онбординг нового Soul (как в [getting-started.md → Шаг 6](../getting-started.md#шаг-6-онбордить-один-soul)): зарегистрировать хост с нужными covens, выпустить bootstrap-токен, применить `soul init` на хосте. На dev-стенде это делает `make dev-souls` (переподнимает флот по реестру БД, covens сохраняются).

Ключевое для эксплуатации: **новый хост подхватывается late-binding автоматически**. Если ты онбордил хост в coven `prod`, то:

- следующий прогон со `where:`/`on: [prod]` (раздел 3) включит его без правки сценария — таргет резолвится в момент run;
- регулярные прогоны по расписанию (**Cadence**, [ADR-046](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)) и event-driven реакции (**Vigil**, [ADR-030](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)) накроют его на следующем тике — оба резолвят target на момент срабатывания, а не на момент настройки.

То есть после онбординга нового хоста ничего в сервисах/расписаниях править не надо — достаточно правильных covens у души. Sizing флота, Acolyte-пул под нагрузку, целевой масштаб 100k VM, балансировка при scale-out — [operations/scaling.md](../operations/scaling.md).

## 6. Инциденты — `error_locked` и разлочка

Если apply упал хотя бы на одном хосте, инкарнация уходит в **`error_locked`** и блокируется: новые прогоны на неё не пройдут, пока ты явно её не разлочишь. Это by design — чтобы расхождение не накапливалось тихо ([architecture.md → Атомарность и error_locked](../architecture.md#атомарность-и-error_locked)). Близкие блокирующие статусы: `migration_failed` (упала state-миграция при upgrade, раздел 4), `destroy_failed` (упал destroy).

**Где смотреть, что случилось:**

```sh
soulctl incarnation get hello-demo          # status: error_locked
soulctl incarnation history hello-demo      # последние прогоны (кто/когда/scenario)
# аудит-журнал операции:
curl -s "http://127.0.0.1:8080/v1/audit?correlation_id=<apply_id>" \
  -H "Authorization: Bearer $TOKEN"
```

**Как разлочить.** `POST /v1/incarnations/{name}/unlock` с обязательным полем `reason` (этой команды в `soulctl` нет — через API):

```sh
curl -s -X POST http://127.0.0.1:8080/v1/incarnations/hello-demo/unlock \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"reason": "проверил host-01 вручную, файл создан, причина была в полном диске — почищено"}'
```

- **`reason` обязателен и ограничен 500 символами** (`ReasonMaxLen`, считается в Unicode-рунах — кириллица влезает полностью). Пустой или длиннее — `422`. Это записывается в аудит, поэтому пиши по делу: что проверил и почему безопасно снимать лок.
- Ответ `200` возвращает `previous_status` (`error_locked`) и новый `status`. После разлочки инкарнация снова принимает прогоны — но **разлочка не чинит хост**: сначала устрани причину, потом разлочь и прогони сценарий заново (раздел 1).

Триаж типовых проблем (`apply` висит в `applying`, souls в `disconnected`, `409 already applying`) — [operations/faq.md](../operations/faq.md). Зависшие инкарнации/`apply_runs` после катастрофы и восстановление — [operations/disaster-recovery.md → Корректировка после восстановления](../operations/disaster-recovery.md#8-корректировка-после-восстановления).

## 7. Что наблюдать

Метрики Prometheus — на `:9090/metrics`, namespace `keeper_*` (Keeper-side) и `soul_*` (Soul-side). В обычной эксплуатационной практике следи за немногим:

- **`keeper_grpc_streams_active`** — сколько душ онлайн; падение ниже ожидаемого числа = часть флота отвалилась (сверь с `souls.status = 'disconnected'`).
- **Apply failure rate** — `rate(keeper_scenario_runs_total{result="failed"}[15m]) / rate(keeper_scenario_runs_total[15m])`; рост = прогоны фейлятся, инкарнации уходят в `error_locked`.
- **`keeper_reaper_lease_held`** — должен быть ровно `1` по кластеру; `0` = чистка БД встала, `>1` = split-brain.
- **`keeper_conductor_lease_held`** — то же для Conductor-лидера ([ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)); Cadence-расписания спавнятся планировщиком (`0` = расписания не спавнят Voyage).

OTel-трейсы покрывают путь прогона end-to-end. Полный список метрик, готовые алерты (Critical/Warning/Info), дашборды и где смотреть трейсы — [operations/monitoring.md](../operations/monitoring.md).

## 8. Что дальше

- **Больше операций сервиса** — добавляй сценарии `scenario/<op>/main.yml`, см. [first-service.md → Что дальше](first-service.md#10-что-дальше).
- **Оркестрация прогона** (`serial:` / `run_once:` / probe-идиома, батч через Voyage) — [orchestration.md](../scenario/orchestration.md), [run-flavors.md](../keeper/run-flavors.md).
- **Эксплуатация кластера** (HA, rolling upgrade Keeper/Soul-флота, disaster recovery, sizing) — [operations/](../operations/README.md).
- **RBAC и Архонты** (роли, scoped-видимость, permission `incarnation.unlock`/`upgrade`/`check-drift`) — [operations/bootstrap-rbac.md](../operations/bootstrap-rbac.md), [keeper/rbac.md](../keeper/rbac.md).
