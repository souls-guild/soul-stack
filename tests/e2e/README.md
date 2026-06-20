# tests/e2e — L3a контракт-тир (fast-loop)

L3a-уровень E2E-тестирования по [ADR-039](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря).

**Статус:** harness рабочий. Каждый тест поднимает изолированный стенд через
testcontainers (PG / Redis / Vault), запускает **реальный `keeper`-процесс**
(sub-process настоящего бинаря, не in-process импорт), делает реальный
`keeper init` (bootstrap первого Архонта), и открывает к нему **live
gRPC-mTLS-стримы soul-stub-ов**. Контракт Keeper↔протокол проверяется на живой
инфраструктуре; единственное, что симулируется, — сам apply на хосте: soul-stub
отвечает **scripted RunResult-ом** вместо реального исполнения задач.

## Три тира E2E (ADR-039)

| Тир | Каталог | build-tag | Soul | Что доказывает |
|---|---|---|---|---|
| **L3a** контракт | `tests/e2e/` | `e2e` | soul-**stub** (scripted RunResult) | контракт Keeper↔протокол: lifecycle apply_runs, dispatch-маршрутизация, staged-render (probe→where), state-commit, audit/metrics — на живых PG/Redis/Vault/keeper, но без реального apply на хосте. |
| **L3b** live | `tests/e2e-live/` | `e2e_live` | **реальный** `soul`-бинарь в privileged Debian-systemd-контейнере | реальный CSR-handshake + реальный apply на хосте (apt install, systemctl, файлы) — см. [tests/e2e-live/README.md](../e2e-live/README.md). |
| **L3c** k8s | `tests/e2e-k8s/` | `e2e_k8s` | реальный `soul` в kind-кластере | прод-подобный деплой (weekly/pre-release). |

L3a отвечает на вопрос «правильно ли Keeper рендерит Destiny, маршрутизирует
dispatch по живым стримам, агрегирует RunResult, коммитит state и пишет
audit/metrics». L3b отвечает на вопрос «правильно ли реальный модуль реально
меняет хост». Разделение по контракту даёт fast-loop (L3a — секунды-минуты,
каждый PR) против smoke-loop (L3b — минуты, nightly).

## Запуск

```sh
make e2e
# то же, что:
cd tests/e2e && go test -tags=e2e -timeout=10m ./...
```

Pre-flight в `harness.NewStack`:

- **keeper-бинарь** обязателен. Источник — env `KEEPER_BIN`, иначе дефолтная
  сборка `keeper/bin/keeper` (`make build`). Без бинаря тест Skip-ается ДО spawn-а
  testcontainers (чтобы разработчик без сборки не ждал 5-минутный таймаут).
- **docker** обязателен для testcontainers. Если docker есть, а спавн PG/Redis/Vault
  падает — тест фейлится явно (разработчик запросил E2E намеренно, отсутствие
  docker = fail, не skip).

Сборка и `go vet -tags=e2e ./...` проходят всегда (отдельный go-модуль
`tests/e2e/go.mod` — deps testcontainers не утекают в `keeper/`/`soul/`).

## Архитектура harness-а

`harness.Stack` — единица изоляции: один тест = один Stack = свой PG/Redis/Vault +
свой Keeper-процесс + N soul-stub-ов. `NewStack` блокируется до полной готовности
(PG healthy → Vault test-secrets → keeper init → keeper run отвечает на `/readyz`
→ soul-stub-ы pre-auth-зарегистрированы).

Что делает `NewStack` (`harness/stack.go`):

1. `startPostgres` / `startRedis` / `startVault` — testcontainers (`postgres:16-alpine`,
   `redis:7-alpine`, `hashicorp/vault:1.15` dev-mode).
2. `InitVaultTestSecrets` — заводит в Vault PKI-mount + JWT signing-key (симметрично
   prod-provision). `IssueKeeperServerCert` — выпускает server-cert keeper-листенеров.
3. `buildKeeperYAML` — рендерит `keeper.yml` в tmpDir с resolved-endpoints инфры.
4. `runKeeperInit` — `keeper init --archon=archon-test --credential-out=...`; JWT
   первого Архонта читается из credential-файла в `Stack.JWT`.
5. `startKeeperRun` — `keeper run` как sub-process; stdout/stderr форвардятся в
   `t.Log`; поллинг `/readyz` (60s).
6. `RegisterSoulPreAuth` на каждый `Config.Souls` — вставляет SID в реестр `souls`
   и выпускает mTLS client-cert (нужен для последующего открытия стрима).

Архитектурные инварианты (ADR-039 Amendment 2026-05-26):

- harness НЕ импортирует `keeper/internal/*` (Go internal-rules);
- все DB-операции — direct SQL через `pgx`;
- все Vault-операции — direct HTTP API (`harness/vault.go`);
- Keeper-процесс — sub-process реального бинаря.

### soul-stub

`Stack.ConnectSoulStub(t, i)` (`harness/soul_stub.go`) открывает **live
EventStream gRPC-mTLS-стрим** i-го pre-auth Soul-а к Keeper-у. Это превращает
«строку в `souls` со `status=connected`» в реальный стрим: на session-open Keeper
захватывает Redis SID-lease, и dispatch (Apply/Errand) маршрутизируется в локальный
Outbound этого SID-а. Без открытого стрима dispatch вернёт `ErrSoulNotConnected`
(apply → `orphaned`). `ConnectSoulStub` ждёт `HelloReply` — гарантию, что lease
захвачен и dispatch смаршрутизируется.

Stub (`internal/soulstub/`) на `ApplyRequest` отвечает scripted `RunResult`-ом:

- `LoadApplyScript` / `SetApplyDefaultSuccess` — success-ответы per-task / по
  умолчанию;
- `SetTaskRegister(taskName, data)` — per-host `register`-данные (`TaskEvent.RegisterData`,
  echo passage) для staged-render probe→where;
- `SetErrandStatus` — ветка ErrandResult;
- `Messages()` — записанные FromKeeper-фреймы (для ассертов: какие ApplyRequest и
  с каким `passage` пришли).

### Helpers Stack-а

Драйверы (`harness/stack.go`):

- `CreateIncarnation` / `CreateIncarnationWithApply` / `CreateIncarnationRaw` —
  POST `/v1/incarnations` через Operator API (с поллингом транзиентного 422
  «service not registered», пока serviceregistry.Holder греет снимок).
- `RunScenario(inc, scenario, input)` — POST скан-сценария, возвращает `apply_id`.
- `SeedIncarnationReady` — прямой INSERT ready-incarnation с baseline state (для
  мутирующих сценариев, чей `create` в L3a недоступен — cloud-spawn / declared-role
  / probe на ещё-не-запущенном хосте, например redis-cluster).
- `WaitApplySuccess` — блокируется до `apply_runs.status=success` у всех строк прогона.
- `WaitIncarnationReady` — блокируется до `incarnation.status=ready` (state_changes
  коммитятся ПОСЛЕ барьера всех хостов; ждать надо именно `ready`, не только success).

Фикстура-хелперы: `RegisterService`, `MaterializeDestinies`, `SeedSoulprint`,
`AddSoulToCoven` (roster по `incarnation.name ∈ souls.coven[]`, ADR-008),
`SeedVaultKV`.

Ассерты (`harness/asserts.go`):

- `AssertApplyRunsStatus(applyID, expected)`;
- `AssertIncarnationState(name, expectedSubset)` — subset-сверка `incarnation.state`;
- `AssertAuditEvent(eventType, expectedPayload)`;
- `AssertMetricGE(metric, minimum)` — Prometheus-эндпоинт keeper-а.

**Ключевой инвариант:** значения status-полей в ассертах — только реальные
enum-values из Go-кода (`apply_runs.status: success`, не `succeeded`/`done`).
`TestValidApplyRunsStatus_*` гарантирует, что список валидных значений не
разъехался с Go-enum.

## Раскладка

```
tests/e2e/
├── README.md                  # этот файл
├── go.mod / go.sum            # отдельный go-модуль (testcontainers-deps изолированы)
├── harness/                   # рабочий L3a-harness
│   ├── stack.go               # NewStack / Cleanup / драйверы (Create/Run/Wait*)
│   ├── soul_stub.go           # ConnectSoulStub + LoadApplyScript
│   ├── asserts.go             # AssertApplyRunsStatus / IncarnationState / AuditEvent / MetricGE
│   ├── vault.go               # InitVaultTestSecrets / IssueKeeperServerCert / SeedVaultKV
│   ├── cert.go                # RegisterSoulPreAuth + mTLS client-cert
│   ├── config_builder.go      # buildKeeperYAML
│   ├── git.go / destiny.go    # bare git-repo per-service + MaterializeDestinies
│   ├── fixtures.go            # YAML loader для souls/soulprint
│   ├── drift.go / errand.go   # drift-check + errand-driver
│   ├── oracle.go / probe.go   # Vigil/Oracle event-driven helpers
│   └── operator.go            # HTTP-клиент Operator API с JWT
├── internal/
│   └── soulstub/              # fake-Soul helper-пакет (НЕ бинарь, ADR-004)
├── smoke-nginx/  hello-world/  noop/  coven-probe/  long-runner/
│   └── fixtures/ + expectations/   # per-example service-fixtures
├── redis/  redis-cluster-live/  redis-monitored/  redis-sentinel/
├── staged-failover/           # WIP-сервис под staged-render proof (НЕ examples/**)
├── oracle_typed_portent/
└── <name>_test.go             # один файл на кейс
```

## Что покрыто (L3a-тесты)

| Тест | Souls | Что доказывает |
|---|---|---|
| `TestSmokeNginx_InstallAndStart` | 1 | pilot: `create` smoke-nginx, lifecycle apply_runs + state-commit. |
| `TestE2EServiceHelloWorld_Create` | 1 | required `input.greeting` + мутация `state.greeting_file`. |
| `TestE2EServiceNoop_Create` | 1 | минимальный no-op `core.exec.run`, state не мутируется. |
| `TestE2EServiceCovenProbe_Create` | 1 | `core.file.present` init-маркер + двойная мутация state. |
| `TestE2EServiceLongRunner_Create` | 1 | `core.file.present` + двойная мутация state. |
| `TestE2EStagedFailover_2Passage` | 2 | **staged-render probe→where** (ADR-056): Passage 0 probe всем, Passage 1 `where: register.role=='master'` ТОЛЬКО на master. |
| `TestE2EServiceRedisCluster_UpdateAcl` | 3 | redis-cluster `update_acl` на PG-seeded ready-incarnation: probe-роли per-host (3 хоста) + ACL-apply only-master + state.redis_users patch + scoped-vault достижимость. |
| `TestE2EServiceRedisClusterLive_*` (`Create`/`AddUser`/`UpdateConfig`/`UpdateAcl`/`AddReplica`/`RemoveReplica`/`Reshard`) | 1–3 | redis-cluster-live на core-модулях: lifecycle мутирующих сценариев. |
| `TestE2EServiceRedis_*` (`Create`/`AddAclUser`/`UpdateConfig`/`AddReplicas`/`*NodeExporter`) | 1+ | redis-сервис + node-exporter destiny. |
| `TestE2EServiceRedisMonitored_Create` / `TestE2EServiceRedisSentinel_Create` | | redis + monitoring / sentinel-топология. |
| `TestE2EKeeperSideDispatch_CovenRegistered` | | keeper-side core `core.soul.registered` (диспетчер `on: keeper`). |
| `TestE2EOracleTypedPortent_*` / `TestOracle_FileChanged_FiresScenario` / `TestL3b_VigilDecreeOracleFlow_Smoke` | | Vigil/Oracle event-driven: portent → fired scenario. |
| `TestDrift_CheckDrift_DriftedAndClean` | 1 | drift-check (stub-Plan; реальный Plan — в L3b). |
| `TestSoulHistory_AggregatesScenarioAndErrand` | | агрегация истории scenario+errand по SID. |
| `TestIncarnationCreate_MissingRequiredInput_422` | | негативный: sync-валидация required-input. |
| `TestValidApplyRunsStatus_*` | — | guard: enum-значения `apply_runs.status` не разъехались с Go. |

## Как добавить L3a-кейс

1. Положить service-fixture в [`examples/service/<name>/`](../../examples/service/)
   (формат обычного service — harness читает «как есть» через git-loader).
   WIP-сервис под proof конкретного механизма — рядом в `tests/e2e/<name>/`
   (как `staged-failover/`), чтобы не засорять examples.
2. Создать `tests/e2e/<name>_test.go` с тегом `//go:build e2e`:
   ```go
   func TestE2EService<Name>_Create(t *testing.T) {
       stack := harness.NewStack(t, harness.Config{
           ExamplePath: "examples/service/<name>",
           Souls:       1,
       })
       defer stack.Cleanup()

       stack.RegisterService(t, "<name>", "examples/service/<name>")
       stub := stack.ConnectSoulStub(t, 0)
       stub.SetApplyDefaultSuccess(true)

       inc, applyID := stack.CreateIncarnationWithApply(t, "<inc>", "<name>@main", input)
       stack.WaitApplySuccess(t, applyID, 60)
       stack.WaitIncarnationReady(t, inc, 30)
       stack.AssertIncarnationState(t, inc, expected)
   }
   ```
3. Прогнать `make e2e`.

## Source of truth

- [ADR-039](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря) — три уровня E2E.
- [docs/testing/README.md](../../docs/testing/README.md) — индекс L0/L1/L2/L3a/L3b/L3c.
- [docs/testing/e2e.md](../../docs/testing/e2e.md) — нормативная спека формата fixtures/expectations.
- [tests/e2e-live/README.md](../e2e-live/README.md) — L3b live-тир.
