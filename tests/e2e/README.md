# tests/e2e — L3a E2E fast-loop pilot

Pilot-каркас L3a E2E-тестирования по [ADR-039](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря).

**Статус:** скелет (структура + типы + контрактные сигнатуры), функциональная
реализация harness-а и soul-stub-а — следующий slice. На текущей фазе
обеспечивает:

- стабильную раскладку каталогов и build-tag (`e2e`),
- компилируемые типы для fixtures/expectations,
- Makefile-таргет `make e2e`,
- canonical pilot-тест (`smoke_nginx_test.go`), который при отсутствии docker
  скипается через `t.Skip("L3a pilot stub: ...")` — но проходит сборку и vet.

## Запуск

```sh
make e2e
# то же, что:
cd tests/e2e && go test -tags=e2e -timeout=10m ./...
```

Без `docker` тесты скипаются (см. [Что НЕ работает](#что-не-работает-в-pilot)
ниже). Сборка и `go vet -tags=e2e ./...` — должны проходить всегда.

## Раскладка

```
tests/e2e/
├── README.md                   # этот файл
├── go.mod                      # отдельный go-модуль (deps не утекают в keeper/soul)
├── harness/                    # reusable Go-helpers (Stack, asserts, fixtures, git)
│   ├── stack.go                # NewStack/Cleanup, конфиг pilot
│   ├── git.go                  # bare git repo per-example в tmp
│   ├── operator.go             # HTTP-клиент к Keeper API с JWT
│   ├── asserts.go              # CheckApplyRunsStatus / CheckIncarnationState / ...
│   └── fixtures.go             # YAML loader для souls/incarnation/secrets
├── internal/
│   └── soulstub/               # fake-Soul helper-пакет (НЕ бинарь, ADR-004)
│       ├── soulstub.go         # gRPC bidi-stream client + scripted ApplyResponse
│       └── doc.go              # обзор контракта soul-stub
├── smoke-nginx/                # fixtures + expectations конкретного теста
│   ├── fixtures/
│   │   ├── souls.yaml          # SID + соответствующий soulprint
│   │   └── stub-responses.yaml # scripted RunResult-ы soul-stub per scenario
│   └── expectations/
│       └── after-create.yaml   # apply_runs.status / incarnation.state / audit / metrics
├── smoke_nginx_test.go         # canonical pilot test
├── hello-world/                # examples-as-tests (см. ниже)
├── noop/
├── coven-probe/
├── long-runner/
└── <name>_test.go              # один файл на example
```

## Examples-as-tests

Каждый каталог `tests/e2e/<example-slug>/` парный к `examples/service/<example>/`
и закрывает happy-path хотя бы одного сценария. Карта:

| Тест | Service | Сценарий | Что покрывает |
|---|---|---|---|
| `smoke_nginx_test.go` | `examples/service/smoke-nginx` | `create` | pilot; install nginx + start service, два task_name. |
| `hello_world_test.go` | `examples/service/hello-world` | `create` | `input.greeting` (required) + мутация `state.greeting_file`. |
| `noop_test.go` | `examples/service/noop` | `create` | минимальный no-op `core.exec.run`; state не мутируется. |
| `coven_probe_test.go` | `examples/service/coven-probe` | `create` | init-маркер `core.file.present`, двойная мутация state (`marker_file`, `last_target`). |
| `long_runner_test.go` | `examples/service/long-runner` | `create` | init-маркер `core.file.present`, двойная мутация state (`last_run`, `runner_marker`). |

Тесты на «продвинутые» сценарии (`mark_a/mark_ab/mark_where`, `stagger/serial_waves`)
требуют multi-host и/или поллинга mid-run — отдельный slice.

## Как добавить новый E2E-кейс

1. Положить service-fixture в [`examples/service/<name>/`](../../examples/service/)
   (формат тот же, что у обычного service — pilot читает «как есть» через git-loader).
2. Создать каталог `tests/e2e/<name>/`:
   - `fixtures/souls.yaml` — список SID-ов + их soulprint;
   - `fixtures/stub-responses.yaml` — что soul-stub отвечает на `ApplyRequest`
     per scenario;
   - `expectations/after-<scenario>.yaml` — ожидаемый state БД / audit / metrics.
3. Создать `tests/e2e/<name>_test.go` с тэгом `//go:build e2e`:
   ```go
   func TestSmoke<Name>(t *testing.T) {
       stack := harness.NewStack(t, harness.Config{
           ExamplePath: "examples/service/<name>",
           Souls:       1,
       })
       defer stack.Cleanup()
       // ...
   }
   ```
4. Прогнать `make e2e`.

## Контракт fixtures/expectations

См. [docs/testing/e2e.md](../../docs/testing/e2e.md) — нормативная спека формата.

**Ключевой инвариант:** значения в `expectations/*.yaml` для status-полей —
только реальные enum-values из Go-кода (например, `apply_runs.status: success`,
не `succeeded` или `done`). Harness валидирует expectations на старте теста
против списка enum-значений (fail-early).

## Что НЕ работает в pilot

- Сетевые testcontainers-вызовы — каркас skeleton, реальные spawn-ы PG/Redis/Vault
  и реальный Keeper-процесс приедут в L3a-implementation slice (отдельная задача
  после стабилизации pilot). Все harness-методы сейчас — `t.Skipf("L3a pilot stub: <reason>")`.
- Soul-stub — открытие gRPC-стрима к Keeper не реализовано (тоже `Skipf` со стороны
  pilot-теста, чтобы не делать ложно-зелёный тест).

См. также:
- [ADR-039](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря) — решение.
- [docs/testing/README.md](../../docs/testing/README.md) — индекс уровней L0/L1/L2/L3a/L3b/L3c.
- [docs/testing/e2e.md](../../docs/testing/e2e.md) — нормативная спека L3a harness.
