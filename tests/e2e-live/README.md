# tests/e2e-live — L3b real-soul-in-container (ADR-039)

L3b smoke-loop с **реальным** `soul`-бинарём в Linux-контейнере (Debian-12
systemd-PID-1). В отличие от L3a (`tests/e2e/`, soul-stub helper-пакет), L3b:

- использует реальный `soul`-binary внутри privileged-контейнера с systemd-PID-1;
- проходит реальный CSR Bootstrap-flow (CSR → `Keeper.Bootstrap` → leaf-cert);
- реальный `core.pkg.installed name=nginx` ставит реальный пакет;
- реальный `core.service.running name=nginx` запускает реальный сервис через systemd.

## Запуск

```sh
# Pre-requisite — cross-compile linux-binary для mount-а в контейнер
make build-linux

# Полный прогон L3b
make e2e-live

# Или один тест напрямую
SOUL_BIN_LINUX=$(pwd)/soul/bin/soul-linux-amd64 \
  go test -tags=e2e_live -run TestSmokeNginxLive ./...
```

Требует **docker** с поддержкой privileged-контейнеров (cgroup-mount + systemd-PID-1).

### WSL2 + Docker-Desktop

На native-Linux соул-контейнер дозванивается к keeper-у через
`host.docker.internal` (host-gateway) — дефолт, ничего настраивать не нужно.

На **WSL2 + Docker-Desktop** контейнеры живут в DD-VM, а keeper-процесс —
в WSL2-дистре (разные network-namespace). Из контейнера `host.docker.internal`
резолвится в DD-VM-шлюз (`192.168.65.254`), где keeper НЕ слушает → bootstrap
падает на `connection refused`. Реальный WSL2-хост-IP из контейнера достижим —
прокинь его через `E2E_KEEPER_HOST` (харнес пропишет его в `soul.yml`-эндпоинт,
ExtraHosts и **TLS-SAN** keeper-серта):

```sh
make build-linux
cd tests/e2e-live
E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') \
  go test -tags=e2e_live -run TestL3bSmokeNginxLive -timeout 25m
```

Без `E2E_KEEPER_HOST` поведение не меняется (CI-дефолт `host.docker.internal`).

## Frequency

- **L3a** (fast-loop) — каждый PR через `make e2e` (~30–60 сек на тест).
- **L3b** (smoke-loop) — **nightly cron / on-demand** (~5–15 мин на тест).
- **L3c** (k8s-loop) — weekly / pre-release.

## Раскладка

```
tests/e2e-live/
├── README.md
├── go.mod                          # отдельный go-модуль (deps не утекают)
├── dockerfiles/
│   └── debian-12.Dockerfile        # privileged systemd-PID-1 base-image
├── harness/                        # копия L3a-harness + L3b-specific helpers
│   ├── stack.go                    # NewStack — PG/Redis/Vault/keeper
│   ├── bootstrap.go                # IssueBootstrapToken (L3b-2)
│   ├── container.go                # SoulContainer + SpawnSoulContainer (L3b-2)
│   ├── asserts.go                  # keeper-side + container-side asserts
│   ├── expectations.go             # LoadExpectations / AssertExpectations (L3b-5)
│   ├── operator.go                 # HTTP-клиент Operator API
│   ├── vault.go                    # InitVaultTestSecrets / IssueKeeperServerCert
│   ├── config_builder.go           # buildKeeperYAML
│   ├── probe.go                    # waitForReady
│   ├── git.go                      # SetupGitRepo (L3b-3)
│   └── drift.go                    # CheckDrift → DriftReport (L3b-6)
├── smoke-nginx-live/
│   └── expectations/
│       └── after-create.yaml
└── drift_live_test.go              # TestL3bDriftLive_HelloWorld (L3b-6)
```

## Slices

L3b реализуется итеративно. Карта slice-ов (architect-консультация `a0af3d90ec118aafd`):

| Slice | Содержание | Статус |
|---|---|---|
| **L3b-1** | Раскладка + Dockerfile + `make build-linux` + `harness.NewStack` skeleton (PG/Redis/Vault/keeper, БЕЗ soul-container). | done |
| **L3b-2** | Real Bootstrap-flow: `IssueBootstrapToken` + `SpawnSoulContainer` (privileged Debian-12 + soul-binary mount + CSR Bootstrap RPC). | done |
| **L3b-3** | First L3b-example `smoke-nginx-live` (реально ставит nginx через apt + systemctl start). | done |
| **L3b-4** | Container-side asserts (`AssertHostPkgInstalled` / `AssertHostServiceActive` / `AssertHostFileExists` / `AssertHostFileContent`). | done |
| **L3b-5** | Multi-host (`redis-cluster-live` с 3 soul-контейнерами) + YAML expectations loader (`harness.LoadExpectations` / `Stack.AssertExpectations`). | done |
| **L3b-6** | Drift-live (`drift_live_test.go` + `harness/drift.go`): check-drift на живом soul через реальный `core.file.Plan` (модуль `core.file.present`), а не stub-Plan как L3a. | done |

## Тесты

| Тест | Souls | Что проверяет |
|---|---|---|
| `TestL3bBootstrap_OneSoul` | 1 | Real CSR Bootstrap-flow + `souls.status=connected` после `SpawnSoulContainer`. |
| `TestL3bSmokeNginxLive_InstallAndStart` | 1 | Реальный `apt install nginx` + `systemctl start nginx` + `core.file.rendered` site-config. Использует `harness.LoadExpectations` + `Stack.AssertExpectations`. |
| `TestL3bRedisClusterLive_ThreeNode` | 3 | Multi-host: установка redis на 3 контейнера + формирование Redis Cluster (`redis-cli --cluster create --cluster-replicas 0`); independent check `cluster_state:ok`. |
| `TestL3bDriftLive_HelloWorld` | 1 | Drift-check на живом soul: create hello-world (`core.file.present` greeting-файл) → clean baseline → out-of-band мутация файла → `CheckDrift` видит `drifted=1` через реальный `core.file.Plan` → re-apply → `CheckDrift` снова clean. Ловит регресс реального Plan (в отличие от stub-Plan L3a). |

## Expectations YAML loader (L3b-5)

`harness.LoadExpectations(t, path)` → `*Expectations` парсит YAML c
`apply_runs` / `incarnation_state` / `audit_events` / `metrics` / `host_state`
(strict-mode `KnownFields(true)` — опечатки в ключах ловятся на старте).
`Stack.AssertExpectations(t, exp, applyID, incName)` применяет весь набор
проверок одним вызовом (включая per-soul container-side ассерты по `host_state`).

Формат полностью симметричен L3a (см. [docs/testing/e2e.md](../../docs/testing/e2e.md))
плюс новая секция `host_state` — список per-soul ожиданий
(packages/services/files). Резолв `host_state[].soul` (по FQDN) →
`Stack.SoulContainers[i]` — внутри loader-а, caller не работает с индексами.

## Почему harness-код дублируется с L3a

Architect-вердикт `a0af3d90ec118aafd`: harness-пакет `tests/e2e/harness/`
расположен под Go-internal-правилами недоступен из `tests/e2e-live/` (другой
module-root). Дублирование приемлемо потому, что L3a/L3b — **независимые
test-frequencies** с разными контрактами (stub vs real soul). После
стабилизации обоих — экстракция общего ядра в `tests/e2e-shared/` отдельным
slice-ом.

## Source of truth

- [ADR-039 § E2E три уровня](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря).
- Architect-консультация `a0af3d90ec118aafd` (5-slice карта L3b).
- [docs/testing/e2e.md](../../docs/testing/e2e.md) — L3a-канон fixtures/expectations
  (L3b-формат симметричный + container-side ожидания).
