# Облачный live-E2E оркестратор (`scripts/e2e-cloud/`)

Runbook повторяемого live-прогона Soul Stack против **постоянного** keeper'а на
облачной VM. Оператор одной командой гонит create / операционные / destroy-сценарии
(redis и далее DragonFly) через Operator API и получает отчёт с ассертами.
Переиспользуемо для регрессий и pre-release проверки.

Уровневый контекст — [testing/README.md](README.md); дизайн уровней L3a/L3b/L3c —
[ADR-039](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря).

## Что это и чем НЕ является

Оркестратор — это **bash поверх Operator API keeper'а**, а не Go-harness. Он не
поднимает стенд сам: keeper и Souls уже живут на VM, оркестратор только дёргает
HTTP-роуты (`POST /v1/incarnations`, `POST .../scenarios/{scenario}`,
`DELETE ...`), опрашивает прогон до терминала и ассертит результат. Единственная
сетевая граница — функция `keeper_api` (`lib/keeper-api.sh`); вся классификация /
опрос / ассерты / отчёт чисты и покрыты docker-free guard-тестами
(`test/guard.sh`, таргет `make check-e2e-cloud`).

⚠️ **Это НЕ `make e2e-live` / `make e2e-live-gate`.** Те — локальный docker-гейт
(**L3b**): реальный `soul`-бинарь в privileged-контейнере, ephemeral-стек через
testcontainers. Локальный гейт **сознательно НЕ покрывает облако**: cloud-provision
(`CloudDriver`), Nexus-`install_method`, sentinel- / cluster-топологии redis,
multi-keeper — всё это стендовая территория (см.
[testing/README.md → что НЕ покрывает live-гейт](README.md#локальный-live-гейт-крупных-фич-make-e2e-live-gate)).
Облачный оркестратор — **L4-adjacent operator-tooling**: живой прогон против
персистентного keeper'а, вне ephemeral-инварианта L3a/L3b.

`make e2e-cloud` **не входит** в `make check` (требует облака и teleport,
симметрично `e2e` / `e2e-live`). В `check` входит только docker-free
`check-e2e-cloud` (guard несущей логики).

## Два мира keeper (важно — невзаимозаменяемы)

Оркестратор ходит к keeper'у двумя способами; выбор — переменная `EXEC_MODE`.
Миры отличаются endpoint'ом, JWT и CA — перепутать нельзя.

| | LOCAL dev-стенд | CLOUD native-keeper |
|---|---|---|
| `EXEC_MODE` | `local` | `tsh` (дефолт) |
| Где keeper | локальная машина (`/tmp/keeper-dev`) | VM (`192.168.2.3`) |
| Как Souls подключены | reverse-туннель (`ssh -R`) | нативно, на самой VM |
| Доступ к API | напрямую curl на `$KEEPER_API` (`:8080`) | только `tsh ssh` на VM → curl `localhost:8080` |
| JWT оператора | `$JWT_FILE` (`/tmp/keeper-dev/archon-alice.jwt`) | `$REMOTE_JWT` на VM (`/opt/soul-stack/archon-cloud.jwt`) |
| Кто читает JWT | локальная оболочка | код **на VM** (`cat $REMOTE_JWT`) |

В `tsh`-мире тело POST передаётся на VM **base64 через env** (`BODY_B64` в
`bash -s`), чтобы не тонуть во вложенном квотинге; teleport-шум
(`WARNING`/`self-signed`/…) фильтруется. JWT в `local`-мире и JWT/CA в `tsh`-мире
принадлежат **разным** keeper'ам — токен одного мира к другому не подойдёт.

Какой bring-up-скрипт к какому миру — см. [канонические списки шагов](#канонические-списки-bring-up).

## Предусловия (гейтит preflight)

`preflight` (`lib/preflight.sh`) — жёсткий гейт **до касания облака**: при провале
runbook выходит с кодом **2**, ничего в keeper не отправив. Печатает чеклист ✓/✗.
Проверяет только **наличие**, ничего не собирает:

- `jq` и `curl` в `PATH`;
- при `EXEC_MODE=local` — читаемость `$JWT_FILE`;
- при `EXEC_MODE=tsh` — `tsh` в `PATH`, **свежий teleport-login** (`tsh status` != 0
  → «сделай `tsh login`»), резолв proxy-хоста;
- при непустом `$E2E_BRINGUP_STEPS` — исполняемость каждого шага в `$SCRIPTS_DIR`
  **и** наличие пред-собранных артефактов в `$ARTIFACTS_DIR`
  (`soul-cloud-wb-linux` / `soul-mod-redis` / `mod-manifest.yaml` — переопределимо
  через `$E2E_ARTIFACTS`). Артефакты оркестратор **проверяет, но не собирает** —
  собери их заранее.

**Креды — только из env или из `/root/.env` на VM (внутри локальных скриптов).
НИКОГДА не из `~/.zsh_wb`.**

## Локальный toolkit `.pm/scripts/` и bring-up

Оркестратор (`scripts/e2e-cloud/`) коммитится в git. **Облачный bring-up остаётся
локальным** — скрипты `.pm/scripts/*` в git не попадают (`.gitignore`): это
WB-специфика (teleport-пути `/mnt/c/...`, VM `192.168.2.3`, чтение `/root/.env`).
Раннер ссылается на них только в рантайме по имени из `$SCRIPTS_DIR`.

Оператор объявляет свой bring-up упорядоченным списком имён в `$E2E_BRINGUP_STEPS`.
`lib/bringup.sh` прогоняет их по очереди, лог каждого шага — в
`$LOG_DIR/<step>.log`, стоп при первом ненулевом коде возврата. Последовательность
**не хардкодится** — два мира keeper требуют разных наборов. Ниже — канонические
списки как документация (сами скрипты — в локальном `.pm/scripts/`).

### Канонические списки bring-up

**Local-track** (`EXEC_MODE=local`, dev-стенд + Souls через reverse-туннель):

```
E2E_BRINGUP_STEPS="restore-after-reboot onboard-e2e distribute-plugin-e2e"
```

- `restore-after-reboot` — поднять `/tmp/keeper-dev` (PG / Vault / Redis / keeper) после ребута;
- `onboard-e2e` — онбординг Soul через reverse-туннель;
- `distribute-plugin-e2e` — доставить `soul-mod-redis` на локально-подключённый Soul.

**Cloud-track** (`EXEC_MODE=tsh`, native-keeper на VM):

```
E2E_BRINGUP_STEPS="deploy-keeper deploy-service batch-onboard distribute-plugin autoprov-run poll-autoprov"
```

- `deploy-keeper` — выкатить / перезапустить keeper на VM;
- `deploy-service` — залить service-репо (`example-cloud-bootstrap`);
- `batch-onboard` — онбординг флота Souls на VM;
- `distribute-plugin` — доставить `soul-mod-redis` + `soul-cloud-wb`;
- `autoprov-run` / `poll-autoprov` — запустить cloud-provision и дождаться VM.

Оба списка — пример. Порядок и состав задаёт оператор; пустой `$E2E_BRINGUP_STEPS`
= bring-up пропущен (keeper уже готов).

## Запуск

```bash
make e2e-cloud SUITE=create-destroy
# либо напрямую:
bash scripts/e2e-cloud/runbook.sh <create|create-destroy|operations>
```

Сухой прогон без сети — `DRY_RUN=1`: печатает последовательность вызовов и генерит
отчёт-скелет на синтетических ответах. Полезно проверить параметры и порядок
шагов перед реальным прогоном.

```bash
DRY_RUN=1 make e2e-cloud SUITE=operations SCENARIO=add_user SCENARIO_INPUT='{"name":"alice"}'
```

### Ключевые параметры (env, дефолты)

| Переменная | Дефолт | Назначение |
|---|---|---|
| `EXEC_MODE` | `tsh` | `local` (прямой curl) / `tsh` (curl на VM через teleport) |
| `KEEPER_API` | `http://127.0.0.1:8080` | endpoint для `EXEC_MODE=local` |
| `TSH_NODE` | `root@soul-keeper-1.$FQDN_SUFFIX` | teleport-нода VM (для `tsh`) |
| `FQDN_SUFFIX` | `fedorovstepan2-dev.vm.xc.clv3` | суффикс FQDN стенда |
| `TELEPORT_HOME` | `/mnt/c/Users/stf20/.tsh` | каталог teleport-identity |
| `SSL_CERT_FILE` | (unset) | CA-bundle для teleport, если нужен |
| `REMOTE_JWT` | `/opt/soul-stack/archon-cloud.jwt` | JWT оператора **на VM** (`tsh`) |
| `REMOTE_KEEPER_API` | `http://localhost:8080` | endpoint keeper'а изнутри VM |
| `JWT_FILE` | `/tmp/keeper-dev/archon-alice.jwt` | JWT оператора локально (`local`) |
| `AID` | `archon-alice` | оператор в шапке отчёта |
| `INCARNATION` | `redis-auto` | имя инкарнации |
| `SERVICE` | `example-cloud-bootstrap` | service инкарнации |
| `CREATE_SCENARIO` | `create` | create-сценарий (пусто = дефолт service'а) |
| `PROVIDER` / `PROFILE` | `wb-prod` / `redis-debian-12` | cloud-провайдер / профиль (шапка отчёта) |
| `COVENS` | (пусто) | covens при create (CSV → JSON-массив) |
| `E2E_CREATE_INPUT` | (пусто) | `input`-JSON для create |
| `E2E_CREATE_MODE` | `engine` | `engine` (POST создаёт) / `script` (создаёт bring-up, движок ловит latest `apply_id` из `/runs`) |
| `SCENARIO` / `SCENARIO_INPUT` | (пусто) | операция: одиночный сценарий + его `input`-JSON |
| `SCENARIOS` | (пусто) | операция: `;`-список (`name` или `name::<json>`) |
| `STATE_ASSERT_PATH` / `STATE_ASSERT_EXPECTED` | (пусто) | операция: опц. ассерт state-поля после сценария |
| `ALLOW_DESTROY` | `true` | destroy без teardown (`allow_destroy=true`) |
| `HEALTHY_TERMINAL` | `ready` | здоровый терминал `incarnation.status` |
| `SCRIPTS_DIR` | `.pm/scripts` | каталог локальных bring-up-скриптов |
| `ARTIFACTS_DIR` | `/opt/soul-stack` | каталог пред-собранных артефактов |
| `E2E_BRINGUP_STEPS` | (пусто) | упорядоченный список шагов bring-up |
| `REPORT_DIR` | `.pm/e2e-reports` | каталог отчётов |
| `LOG_DIR` | `$REPORT_DIR/logs` | логи bring-up-шагов |
| `POLL_INTERVAL` / `POLL_MAX` | `30` / `40` | опрос прогона: интервал (с) и максимум итераций |
| `DRY_RUN` | `0` | `1` — печать вызовов без сети |
| `INSECURE_TLS` | `0` | `1` — `curl -k` (self-signed на стенде) |

### Suites

- **`create`** — `[bring-up] → create → poll → assert`. `E2E_CREATE_MODE=engine`
  шлёт `POST /v1/incarnations`; `=script` — создание делают bring-up-скрипты, движок
  подхватывает последний `apply_id` через `GET /runs?limit=1`. Затем опрос до
  терминала, `assert_run_success`, вторичный ассерт `incarnation.status==ready`.
  Если `POST` вернул 202 **без** `apply_id` (`lifecycle.auto_create:false` — bare
  инкарнация без прогона) — шаг помечается SKIP и проверяется только статус `ready`.
- **`create-destroy`** — идемпотентный полный цикл. **Pre-clean:** если инкарнация
  уже есть и залочена (`error_locked` / `migration_failed`) — `unlock` + destroy +
  ждать исчезновения; затем `create`-suite; затем `DELETE` и подтверждение сноса.
  **Повторный прогон обязан пройти без ручных вмешательств** — это критерий приёмки.
- **`operations`** — generic-движок `run_scenario`: `POST /scenarios/{scenario}` → poll →
  `assert_run_success`. Одиночный сценарий (`$SCENARIO` + `$SCENARIO_INPUT`) или
  `;`-список (`$SCENARIOS`, элемент `name` или `name::<json>`). Имя сценария —
  любое service-defined (`add_user` / `update_config` / `restart` / `rotate_tls` /
  cluster-ops). Опц. после одиночного успешного сценария — ассерт state-поля
  (`$STATE_ASSERT_PATH` == `$STATE_ASSERT_EXPECTED`).

Пример операции:

```bash
EXEC_MODE=tsh INCARNATION=redis-auto \
  SCENARIO=add_user SCENARIO_INPUT='{"name":"alice","acl":"~* +@read"}' \
  STATE_ASSERT_PATH='.state.users[]?.name' STATE_ASSERT_EXPECTED=alice \
  make e2e-cloud SUITE=operations
```

## Что ассертит

**Успех прогона — по apply_run, не по HTTP.** `POST` возвращает **202 Accepted** —
это лишь «принято», а не «применено». Движок опрашивает
`GET /v1/incarnations/{name}/runs/{apply_id}` (`RunDetailReply`) до терминала:

- Агрегат `.status` ∈ `applying | success | failed | cancelled` (источник истины —
  `keeper/internal/applyrun/applyrun.go`). `classify_status`:
  `success → PASS`, `failed|cancelled → FAIL`, `applying → CONTINUE`. Неизвестный
  статус → `CONTINUE` (безопасно: дойдёт до timeout, а не ложно засчитает успех).
- **`assert_run_success` = `.status=="success"` И каждый `.hosts[].status` ∈
  {`success`, `no_match`}.** `no_match` — benign-терминал: сценарий, нацеленный на
  подмножество (напр. `add_user` на master → реплики `no_match`), считается успехом
  — так же, как агрегат бэкенда (`applyrun.AggregateRunStatus`). На фейле в stderr
  печатается форензика реально упавших хостов: `failed_task_idx` /
  `failed_plan_index` / `error_summary`.

**Вторичный ассерт — `incarnation.status`.** `GET /v1/incarnations/{name}` →
`.status`; здоровый терминал — **только `ready`** (enum из
`keeper/internal/api/huma_enums.go`: `applying | destroy_failed | destroying |
drift | error_locked | migration_failed | provisioning | ready`).

**Destroy** ассертится по исчезновению: `GET /{name}` → **404** (надёжнее статуса
teardown-прогона — покрывает и `allow_destroy=true` без teardown).

**Операционный state** (опц.) — `assert_state_field` извлекает jq-путь из `.state` и
сравнивает с ожидаемым (contains-семантика для стримов вроде `.state.users[]?.name`).

Подтверждённые роуты Operator API (все под `/v1`, `Authorization: Bearer <jwt>`):

| Операция | Роут |
|---|---|
| create | `POST /v1/incarnations` → 202 `IncarnationCreateReply` |
| операция (generic) | `POST /v1/incarnations/{name}/scenarios/{scenario}` → 202 `IncarnationRunReply` |
| destroy | `DELETE /v1/incarnations/{name}?allow_destroy=<bool>` → 202 `IncarnationDestroyReply` |
| unlock | `POST /v1/incarnations/{name}/unlock` → 200 |
| статус прогона | `GET /v1/incarnations/{name}/runs/{apply_id}` → `RunDetailReply` |
| список прогонов | `GET /v1/incarnations/{name}/runs` → `RunSummaryEntry[]` |
| get / state | `GET /v1/incarnations/{name}` → `.state` / `.status` / `.status_details` |

## Отчёт

Отчёт пишется **инкрементально** в `.pm/e2e-reports/<дата>-<suite>.md`
(`<дата>` = UTC `YYYY-MM-DD`; каталог gitignored). Каждая строка добавляется сразу
— при аборте отчёт не пустой. Логи bring-up-шагов — рядом в `logs/<step>.log`.

Структура:

- **Шапка «Окружение»** — suite, exec_mode (+ пометка DRY-RUN), endpoint,
  incarnation / service, provider / profile, canon (short-SHA core), operator (aid),
  bring-up steps.
- **Таблица «Шаги»** — колонки:
  `# | шаг / сценарий | apply_id | старт (UTC) | длит,с | http | run_status | assert | итог`.
  Итог каждого шага — `PASS` / `FAIL` / `SKIP`.
- **Сводка** — счётчики PASS / FAIL / SKIP, RESULT, exit-code.

Как читать: `apply_id` из строки → `GET /runs/{apply_id}` для полной форензики;
`run_status` = агрегат прогона; колонка `assert` = вердикт ассерта (`run=success` /
`hosts!=success` / `got!=ready` / `инкарнация снесена (404)` / …).

**Exit-коды** (ими же завершается `runbook.sh`):

| Код | Значение |
|---|---|
| `0` | все шаги PASS |
| `1` | провал ассерта или прогона (`failed` / `cancelled` / хост не success / timeout опроса) |
| `2` | провал preflight или инфраструктуры (нет `jq`/`tsh`, протух teleport, нет артефакта, неизвестный suite) |

## Troubleshooting

- **`apply_id` из 202 пустой.** Норма для `lifecycle.auto_create:false` (bare
  инкарнация — create-suite помечает SKIP и проверяет только `ready`) и для
  `E2E_CREATE_MODE=script` (создаёт скрипт → движок берёт latest из
  `GET /runs?limit=1`). Если ждали прогон, а его нет — проверь, что service вообще
  запускает create-сценарий.
- **202, но прогон «не виден» при опросе (http != 200).** `GET /runs/{apply_id}`
  какое-то время может отдавать не-200, пока прогон материализуется — движок это
  терпит и опрашивает дальше до `POLL_MAX`. **202 ≠ успех**: без успешного опроса
  шаг не засчитывается.
- **Стейл-путь к артефактам.** Единый `$ARTIFACTS_DIR` (`/opt/soul-stack`) для всех
  артефактов; не разноси по разным каталогам — preflight проверяет именно его.
  Артефакты **пред-собери сам**, оркестратор их не собирает.
- **Два JWT / CA перепутаны.** `local`-мир и `tsh`-мир — разные keeper'ы с разными
  JWT и CA. Токен `archon-alice.jwt` не подойдёт облачному keeper'у и наоборот —
  401/403 обычно означает «взял JWT не того мира».
- **`no_match`-хосты в отчёте.** Не ошибка: сценарий на подмножество (master-only)
  оставляет остальные хосты `no_match`, ассерт считает это успехом (как бэкенд).
  FAIL даёт только реальный `failed` / `cancelled` хост — его форензика в stderr.
- **`tsh status` протух → exit 2.** Teleport-identity истекает; preflight ловит это
  до касания облака. Обнови `tsh login` и перезапусти.
- **Повторный `create-destroy` спотыкается о залипшую инкарнацию.** Не должно:
  pre-clean снимает `error_locked` / `migration_failed` через `unlock` и сносит
  остаток перед create. Если всё же залипло — снести вручную рецептом из
  applying-zombie-cleanup, затем перезапустить.

## См. также

- [testing/README.md](README.md) — индекс уровней тестирования (L0–L4) и границы
  локального live-гейта.
- [testing/e2e.md](e2e.md) — нормативная спека L3a Go-harness (контракт keeper↔soul).
- [ADR-039](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря)
  — E2E три уровня + amendment про облачный оркестратор.
