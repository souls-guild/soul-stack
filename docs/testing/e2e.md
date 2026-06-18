# L3a E2E — harness и формат fixtures/expectations

Нормативная спека L3a fast-loop E2E-тестирования ([ADR-039](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря)).
Источник правды по реализации — каталог [`tests/e2e/`](../../tests/e2e/) (Go-код)
и этот документ (формат YAML-fixtures и expectations + контракт harness-методов).

## Что это

L3a поднимает изолированный E2E-стенд на один тест: PG + Redis + Vault через
testcontainers + Keeper-процесс (in-process) + N soul-stub-ов (helper-пакет,
не бинарь). Тест запускает scenario через Operator API, soul-stub отвечает
scripted `RunResult`-ом, тест ассертит `apply_runs` / `incarnation.state` /
audit / metrics.

**L3a НЕ тестирует** реальное apply на хосте (для этого — L3b real-soul-in-container).
L3a — контракт-тест keeper-стороны (scenario-runner ↔ apply_runs lifecycle ↔
audit ↔ Prometheus).

## Запуск

```sh
make e2e
# либо напрямую:
cd tests/e2e && go test -tags=e2e -timeout=10m ./...
```

Без docker тесты скипаются (testcontainers spawn возвращает ошибку). Сборка
(`go build -tags=e2e ./tests/e2e/...`) и vet (`go vet -tags=e2e ./tests/e2e/...`)
работают всегда — это инвариант ADR-039: pilot должен оставаться
compile-clean даже без docker.

## Раскладка

```
tests/e2e/
├── go.mod                          # отдельный go-модуль (deps не утекают)
├── README.md
├── harness/                        # reusable Go-helpers
├── internal/soulstub/              # fake-Soul helper-пакет
├── <example-slug>/                 # fixtures + expectations конкретного теста
│   ├── fixtures/
│   │   ├── souls.yaml              # SID + соответствующий soulprint + статус
│   │   └── stub-responses.yaml     # scripted RunResult-ы soul-stub-а
│   └── expectations/
│       └── after-<scenario>.yaml   # apply_runs / state / audit / metrics
└── <example-slug>_test.go          # Go-test с //go:build e2e
```

Service-fixture (то, что Keeper загружает через git) лежит **в `examples/service/<name>/`**,
а не в `tests/e2e/<name>/`. Это инвариант ADR-039 (5): «examples остаются
доменно-чистыми; тестовая обвязка живёт в `tests/e2e/<name>/`».

## Формат fixtures

### `fixtures/souls.yaml`

Список soul-stub-ов, которые harness регистрирует у Keeper-а перед прогоном.

```yaml
- sid: soul-a.example.com
  status: connected                  # значение souls.status; обычно "connected"
  covens: [smoke]                    # членство в Coven для where:-таргетинга
  soulprint:                         # содержимое soulprint_facts (см. docs/soul/soulprint.md)
    os:
      family: debian
      distro: debian
      version: "12"
      pkg_mgr: apt
      init_system: systemd
    hostname: soul-a
```

| Ключ | Тип | Обяз. | Описание |
|---|---|---|---|
| `sid` | string | да | FQDN-имя Soul-а ([naming-rules.md → SID](../naming-rules.md)). |
| `status` | string | да | Целевой `souls.status` (обычно `connected`). |
| `covens` | []string | нет | Coven-метки для where:-фильтров; пусто = только incarnation-membership. |
| `soulprint` | map | нет | Содержимое `soulprint_facts` (форма — typed-схема SoulprintFacts, см. [docs/soul/soulprint.md](../soul/soulprint.md)). |

### `fixtures/stub-responses.yaml`

Скрипт ответов soul-stub-а: на каждую задачу из scenario stub отвечает
соответствующим `RunResult`.

```yaml
scenarios:
  <scenario-name>:
    apply_responses:
      - task_name: "Install nginx package"
        run_result:
          status: success
          state_changes:
            packages:
              - nginx: installed
```

| Ключ | Тип | Обяз. | Описание |
|---|---|---|---|
| `scenarios` | map | да | Карта scenario-name → script. Ключи совпадают с `name:` в `scenario/<name>/main.yml`. |
| `<scenario>.apply_responses[]` | list | да | Список scripted ответов. Matching по `task_name` (не по индексу — порядок может различаться). |
| `apply_responses[].task_name` | string | да | Полное `name:` задачи из destiny/scenario. |
| `apply_responses[].run_result.status` | string | да | Реальное значение enum `keeper/internal/applyrun.Status` (например `success`, `failed`). Harness валидирует fail-early. |
| `apply_responses[].run_result.state_changes` | map | нет | Произвольный jsonb-payload, кладётся в `state_changes`-поле `FromSoul.RunResult`. |

### `expectations/after-<scenario>.yaml`

Post-apply ожидания. Harness читает БД / audit / metrics после `WaitApplySuccess`
и сравнивает с этим файлом.

```yaml
apply_runs:
  status: success                            # реальное значение enum

incarnation_state:                            # deep-subset проверка jsonb
  nginx_package: nginx
  nginx_service: nginx

audit_events:                                 # presence-проверка (минимум одна строка)
  - type: incarnation.created                 # реальный EventType (shared/audit/event_types.go)
    payload:
      incarnation: test-nginx

metrics:                                      # Prometheus-expression → constraint-строка
  keeper_scenario_runs_total{result="ok"}: ">= 1"
```

> Имена audit-event-ов и метрик в expectations — **реальные значения из Go-кода**, не выдуманные. Источник правды по audit-типам — [`shared/audit/event_types.go`](../../shared/audit/event_types.go) (например `incarnation.created`, `incarnation.scenario_started`, `incarnation.run_completed`; событий `scenario.applied` / `scenario.completed` в коде НЕТ). Источник по метрикам прогона — [`keeper/internal/scenario/metrics.go`](../../keeper/internal/scenario/metrics.go): метрика называется `keeper_scenario_runs_total{result="ok"|"error"|"locked"}` (метрики `keeper_apply_runs_total` в коде НЕТ).

| Ключ | Семантика |
|---|---|
| `apply_runs.status` | Ровное значение `apply_runs.status` на момент терминала. Только реальные enum-values: `planned`, `claimed`, `running`, `dispatched`, `success`, `failed`, `cancelled`, `orphaned`, `no_match`. |
| `incarnation_state` | Deep-subset проверка `incarnation.state` jsonb. Указанные ключи обязаны быть равны (для скаляров) / содержать subset (для map-ов). Лишние ключи в БД допустимы. |
| `audit_events[]` | Presence-проверка: для каждой строки в expectation в `audit_log` обязана быть хотя бы одна строка с `type=<type>` и `payload`, содержащим `<payload>` subset-ом. Cardinality не проверяется. |
| `metrics{<expression>}` | Prometheus-выражение → constraint-строка (`>= N`, `== N`, `> N`). Harness скрейпит `/metrics` Keeper-а, парсит и применяет constraint. |

## Source-of-truth для enum-значений

[ADR-039(4)](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря):
значения статусов в expectations — это **реальные enum-values** из Go-кода
keeper-а.

Harness валидирует `apply_runs.status` на старте теста через
`harness.CheckApplyRunsStatusValid`. Список разрешённых значений — в
[`tests/e2e/harness/asserts.go`](../../tests/e2e/harness/asserts.go) (карта
`validApplyRunsStatus`). Источник — [`keeper/internal/applyrun/applyrun.go`](../../keeper/internal/applyrun/applyrun.go)
(const-ы `StatusSuccess` etc.); drift ловится тестом, сверяющим карту с Go-enum.

## Контракт harness

```go
import "github.com/souls-guild/soul-stack/tests/e2e/harness"

func TestX(t *testing.T) {
    stack := harness.NewStack(t, harness.Config{
        ExamplePath: "examples/service/<name>",
        Souls:       1,
    })
    defer stack.Cleanup()

    inc := stack.CreateIncarnation(t, "test-x", "<service>@main", map[string]any{...})
    applyID := stack.RunScenario(t, inc, "<scenario>", map[string]any{...})

    stack.WaitApplySuccess(t, applyID, 60)

    stack.AssertApplyRunsStatus(t, applyID, "success")
    stack.AssertIncarnationState(t, inc, map[string]any{...})
    stack.AssertAuditEvent(t, "incarnation.created", map[string]any{...})
    stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}
```

| Метод | Контракт |
|---|---|
| `NewStack(t, Config) *Stack` | Поднимает PG/Redis/Vault через testcontainers + Keeper-процесс + N soul-stub-ов. Блокируется до `keeper /readyz=200` и регистрации всех Souls. |
| `Stack.Cleanup()` | Graceful-shutdown всего стенда + удаление tmp-каталогов. Idempotent. |
| `Stack.CreateIncarnation(t, name, serviceRef, spec)` | Создаёт incarnation через `POST /v1/incarnations` Operator API. Возвращает имя. |
| `Stack.RunScenario(t, inc, scenario, input)` | Запускает scenario через `POST /v1/incarnations/<name>/scenarios/<scenario>`. Возвращает apply_id. |
| `Stack.WaitApplySuccess(t, applyID, timeoutSec)` | Polling `apply_runs.status` до терминала или timeout. Фейлит тест, если статус != success. |
| `Stack.AssertApplyRunsStatus(...)` | Прямой SELECT по apply_runs с валидацией ожидаемого статуса через `validApplyRunsStatus`. |
| `Stack.AssertIncarnationState(...)` | SELECT `incarnation.state` jsonb + deep-subset сравнение. |
| `Stack.AssertAuditEvent(...)` | SELECT `audit_log` WHERE type=... + payload subset check. |
| `Stack.AssertMetricGE(...)` | GET `<keeper>/metrics`, парсинг Prometheus-text, eval expression-а из ключа. |

## Контракт soul-stub

См. [`tests/e2e/internal/soulstub/`](../../tests/e2e/internal/soulstub/).

- `New(sid, keeperGRPCAddr) *Stub` — конструктор.
- `Stub.LoadScript(perScenario)` — загрузка scripted-ответов из распарсенного `stub-responses.yaml`.
- `Stub.Open()` — открыть bidi-stream к Keeper-у (mTLS-handshake, регистрация SID), блокируется до Close или разрыва.
- `Stub.Close()` — graceful-shutdown.
- `Stub.Messages() []Message` — все принятые от Keeper-а payload-ы для последующих ассертов.

## L3a canonical config & helpers

Конкретные канонические параметры, на которых harness и smoke-test поднимают
изолированный стенд (см. также [ADR-039 Amendment 2026-05-26](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря)).

| Параметр | Канон | Источник правды |
|---|---|---|
| Spawn инфры | testcontainers-go (PG/Redis/Vault); Keeper — sub-process реального `keeper`-бинаря | ADR-039 Amendment §1 |
| Vault KV path для JWT | `secret/keeper/jwt-signing-key`, поле `signing_key` | `dev/keeper.dev.yml::auth.jwt.signing_key_ref` |
| Vault KV формат signing-key | **HS256: 32B random base64-encoded string** (НЕ Ed25519 PEM) | `keeper/internal/jwt/issuer.go::minSigningKeyBytes`, `dev/provision.sh:88-92` |
| Vault PKI mount | `pki/`, role `soul-seed`, root CA TTL 87600h | `dev/provision.sh` шаги 3-5 |
| Keeper TLS cert | Vault-issued leaf, SAN `127.0.0.1`+`localhost`, TTL 24h | `harness.IssueKeeperServerCert` |
| Sigil signing-key | НЕ pre-seed; `KeyService` introduces runtime | `keeper/internal/sigil/keyservice.go` |
| Bootstrap первого Архонта | `keeper init --archon=archon-test --config=<path> --credential-out=<tmp>/jwt` | `keeper/cmd/keeper/main.go:99-261` |
| Env keeper run | `SOUL_STACK_ALLOW_FILE_REPOS=1` обязателен для service-loader-а с file://-URL | ADR-039 Amendment §5 |
| Soul-stub pre-auth | Vault PKI leaf + direct SQL INSERT в `souls`/`soul_seeds`; `bootstrap.Bootstrap` минуется | ADR-039 Amendment §6 |
| harness ↔ keeper границы | harness НЕ импортирует `keeper/internal/*` (Go internal-rules); все DB-ops — direct SQL, Vault-ops — direct HTTP API | ADR-039 Amendment §1 |

**Drift-инвариант.** Drift между harness-овским CRUD-каркасом и реальным
schema-set в `keeper/migrations/` синхронизируется вручную при schema-change.
Минимальный набор затрагиваемых таблиц у harness — `souls`, `soul_seeds`,
`operators`, `apply_runs`, `incarnation`, `audit_log`. Источник правды — Go-
структуры и миграции в `keeper/internal/`; harness-CRUD дублируется по
необходимости (через replace в `go.mod` сейчас не тащится — internal-rules
запрещают import из тестового модуля).

**Note про HS256 vs Ed25519.** До L3a-impl v3 в моём ТЗ ошибочно фигурировал
Ed25519 PEM как формат signing-key — это противоречит `jwt/issuer.go`. Реальный
формат — HMAC-256 ключ ≥32 байта; `bootstrap.extractSigningKey` сначала пробует
base64-decode, при ошибке принимает строку как raw-bytes (см. `keeper/internal/
bootstrap/keeper_init.go::extractSigningKey`). harness кладёт `signing_key` как
base64-encoded string из 32 случайных байт (`crypto/rand` → `base64.StdEncoding`),
полностью симметрично `openssl rand -base64 32` в `dev/provision.sh:90`.

## Статус реализации L3a

L3a-harness **реализован полностью** (фаза v3, pilot-stub снят): `NewStack`
делает реальный spawn инфры через testcontainers (PG / Redis / Vault), поднимает
реальный `keeper`-процесс sub-process-ом, открывает реальный gRPC-стрим из
soul-stub-а, грузит fixtures/expectations YAML loader-ом и выполняет реальные
`Assert*`-методы.

`t.Skip` остаётся ровно в двух случаях, означающих «E2E невозможен в этой среде»,
а не «not implemented»:

- нет keeper-бинаря (`KEEPER_BIN` не задан и `make build` не выполнен) — skip ДО
  spawn-а, чтобы разработчик без сборки не ловил 5-минутный таймаут;
- нет docker — testcontainers вернёт ошибку spawn-а; в этом случае тест **фейлится
  явно** (E2E запрошен сознательно, отсутствие docker — fail, не skip).

Сборка/vet с `-tags=e2e` обязаны проходить всегда (инвариант ADR-039 — compile-clean
без docker).

## L3b (real-soul-in-container) — `tests/e2e-live/`

Состояние: **L3b-1..L3b-5 закрыты** (вся 5-slice карта L3b завершена).
Smoke-тесты:

- `TestL3bBootstrap_OneSoul` — реальный CSR Bootstrap-flow + `souls.status=connected`.
- `TestL3bSmokeNginxLive_InstallAndStart` — реальный `apt install nginx` +
  `systemctl start` + `core.file.rendered` (single-host).
- `TestL3bRedisClusterLive_ThreeNode` — multi-host (3 soul-контейнера) +
  Redis Cluster через `redis-cli --cluster create`.

Build-tag: `e2e_live`. Запуск: `make e2e-live` (требует `make build-linux` +
docker с privileged-режимом). Frequency: on-demand (pre-tag gate в
[RELEASING.md](../../RELEASING.md); ежедневный nightly-cron отключён — manual-only).

**WSL2.** На WSL2 контейнер не дозванивается до keeper через
`host.docker.internal` — прокинь реальный хост-IP через `E2E_KEEPER_HOST`
(harness пропишет его в `soul.yml`-эндпоинт). Рецепт:

```sh
E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') make e2e-live
```

Без `E2E_KEEPER_HOST` поведение не меняется (CI-дефолт `host.docker.internal`).
Полный quickstart — [`tests/e2e-live/README.md`](../../tests/e2e-live/README.md).

Отличия от L3a:

- Вместо soul-stub helper-пакета — реальный `soul`-binary в privileged Debian-12
  контейнере с systemd-PID-1 (см. `tests/e2e-live/dockerfiles/debian-12.Dockerfile`).
- Реальный CSR Bootstrap-flow (CSR → `Keeper.Bootstrap` RPC → leaf-cert) вместо
  pre-auth-регистрации в БД.
- Реальный `core.pkg.installed` / `core.service.running` мутируют состояние
  внутри контейнера (apt-install + systemctl).

### YAML expectations loader (L3b-5)

`tests/e2e-live/harness/expectations.go` экспортирует
`LoadExpectations(t, path) *Expectations` и `Stack.AssertExpectations(t, exp,
applyID, incName)`. Формат YAML — расширение L3a-`ExpectationsAfter`
(см. выше) плюс секция `host_state[]` для per-soul container-side ассертов:

```yaml
host_state:
  - soul: soul-live-a.example.com     # FQDN, должен совпадать с Stack.SoulContainers[i].SID
    packages:
      nginx: installed                # → AssertHostPkgInstalled
    services:
      nginx: active                   # → AssertHostServiceActive
    files:
      - path: /etc/nginx/...          # → AssertHostFileExists
        contains: "server_name"       # → AssertHostFileContent (опционально)
```

Loader работает в strict-mode (`yaml.Decoder.KnownFields(true)`) — лишние
ключи (опечатки) ловятся на старте теста, не на assert-фазе. `metrics`-секция
поддерживает constraint-выражения `>= N` / `<= N` / `> N` / `< N` / `== N`
или голое число (трактуется как `>= N`); в MVP реализован только `>=`,
остальные операторы → fail с понятным сообщением (расширение без breaking
change YAML-формата).

L3b-карта slice-ов и подробности — `tests/e2e-live/README.md`.

## L3c (kind-cluster, real K8s) — `tests/e2e-k8s/`

L3c — самый медленный E2E-уровень: поверх **реального** Kubernetes-кластера
(kind) с инфрой (PG / Redis / Vault) через bitnami Helm-чарты и Keeper / Soul
как раздельные k8s-ресурсы (Deployment / StatefulSet). Покрывает HA-кейсы,
недоступные в L3a/L3b: leader-failover при kill-е keeper-пода, Watchman-
rebalance при scale Soul-replicas, Toll-rate-limit под нагрузкой.

Build-tag: `e2e_k8s`. Запуск: `make e2e-k8s` (требует docker + kind CLI в PATH).
Frequency: **weekly / pre-release**.

### Архитектура

- kind-cluster per-test (`soul-stack-e2e-<test>-<unix-nanos>`) — изолирует
  параллельные прогоны.
- Инфра — bitnami Helm-чарты (`bitnami/postgresql` / `bitnami/redis` /
  `bitnami/vault`), репо подключается runtime через `helm repo add` с retry
  (PM-decision: vendoring чартов в репо отложено).
- Keeper-image: base `gcr.io/distroless/static` (PM-decision).
- Soul-pod: privileged systemd-PID-1 (PM-decision: parity с L3b
  `tests/e2e-live/dockerfiles/debian-12.Dockerfile`).
- Harness-код **дублируется** с L3a/L3b (Go-internal-rules; экстракция
  в `tests/e2e-shared/` — отдельный slice после L3c-1..L3c-5).

### L3c slice-карта

| Slice | Содержание | Статус |
|---|---|---|
| **L3c-1** | Раскладка + Dockerfile-placeholder + `make e2e-k8s` + `harness.NewCluster` (kind через Go-API) + `TestL3cKindUp_Empty`. | done |
| **L3c-2** | `Stack.DeployInfra` (bitnami Helm PG/Redis/Vault) + `Stack.DeployKeeper` (single keeper-pod, distroless, `--initialize` mode) + Vault PKI/JWT/DSN seed через port-forward + `TestL3cKeeperPing_Single` (port-forward keeper:8080 → /readyz=200). | done |
| **L3c-3** | `Stack.DeployKeeper` replicas: 3 (HA) + `Stack.DeploySoul` (StatefulSet privileged systemd-PID-1) + `TestL3cMultiKeeper_BootstrapAndConnect`. | done |
| **L3c-4** | HA-failover-сценарий: `TestL3cMultiKeeper_KillLeader` (per-pod KID через init-container + reaper.enabled с lock_ttl=15s + kubectl delete pod лидера → новый лидер в `GET reaper:leader` → Deployment self-heal → Soul реконнект). | done |
| **L3c-5** | Toll degraded-mode test (`TestL3cToll_DegradedMode`: 5 souls → kill 3 grace=0 → `cluster:degraded` flag → POST scenario=503 + Retry-After + audit `cluster.degraded_set`) + Redis-cluster resharding (parity с мега-тестом 2026-05-25; каркас `TestL3cRedisCluster_Resharding` с t.Skip до in-cluster git-server-pod infra — propose-and-wait по имени `git-server-pod`). | done (part A) / deferred (part B) |

L3c-полная карта и quickstart — [`tests/e2e-k8s/README.md`](../../tests/e2e-k8s/README.md).

## Examples-as-tests (L3a coverage extension)

L3a-pilot `smoke-nginx` зафиксировал pattern; дальше pattern тиражируется на
существующие `examples/service/<name>/` happy-path-сценарии. Каждый Go-тест
рядом с harness-ом (`tests/e2e/<example-slug>_test.go`) парный с каталогом
fixtures+expectations `tests/e2e/<example-slug>/`. Service-fixture не дублируется
в `tests/e2e/` — Keeper читает его из `examples/service/<name>/` через git-loader
([ADR-039](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря) §5).

Текущее покрытие:

| Тест | Service | Сценарий | Покрывает |
|---|---|---|---|
| `smoke_nginx_test.go` | `smoke-nginx` | `create` | pilot; nginx install + service start. |
| `hello_world_test.go` | `service-hello-world` | `create` | `input.greeting` (required) + мутация `state.greeting_file`. |
| `noop_test.go` | `service-noop` | `create` | no-op `core.exec.run`, state не мутируется. |
| `coven_probe_test.go` | `service-coven-probe` | `create` | init-маркер, двойная мутация state (`marker_file`, `last_target`). |
| `long_runner_test.go` | `service-long-runner` | `create` | init-маркер, двойная мутация state (`last_run`, `runner_marker`). |

«Продвинутые» сценарии (`mark_a/mark_ab/mark_where` coven-таргетинга,
`stagger/serial_waves` long-runner-а, redis-композиции с Vault + destiny-deps,
sentinel multi-host) требуют multi-host фикстуры / poll mid-run / реальной
Vault-подложки — отдельный slice, не входит в текущее L3a-расширение.

## См. также

- [ADR-039](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря) — решение.
- [README уровней тестов](README.md) — индекс L0/L1/L2/L3a/L3b/L3c.
- [`tests/e2e/README.md`](../../tests/e2e/README.md) — L3a quickstart.
- [`tests/e2e-live/README.md`](../../tests/e2e-live/README.md) — L3b quickstart.
- [`tests/e2e-k8s/README.md`](../../tests/e2e-k8s/README.md) — L3c quickstart.
