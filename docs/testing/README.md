# Тестирование Soul Stack

Индекс уровней тестов. Каждый уровень — отдельный механизм; они не конкурируют,
а добавляются друг к другу. Источник правды по дизайну —
[ADR-023](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage) (Trial)
и [ADR-039](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря) (E2E).

## Уровни

| Уровень | Где | Build-tag | Что покрывает | Прогон |
|---|---|---|---|---|
| **L0** — unit | `<module>/<pkg>/*_test.go` | нет | Чистая логика функций. Без сети / БД / контейнеров. | каждый PR, `make test` |
| **L1** — integration | `<module>/<pkg>/integration_test.go` | `integration` | testcontainers per-package (PG / Redis / Vault), реальные CRUD-вызовы. | каждый PR, `make test-integration` (нужен docker) |
| **L2** — Trial | `examples/destiny/<name>/_trial/`, `examples/service/<name>/scenario/<n>/tests/` | нет | Hermetic prerender + migration-assert на fixtures (`soul-trial`). | каждый PR через `make build` + `soul-trial` |
| **L3a** — E2E fast-loop | `tests/e2e/` | `e2e` | testcontainers (PG/Redis/Vault) + Keeper-процесс in-process + soul-stub. Контракт-тесты apply_runs lifecycle / RBAC / audit / MCP. | каждый PR (когда L3a-imp slice стабилизируется), `make e2e` |
| **L3b** — E2E smoke | `tests/e2e-live/` | `e2e_live` | Real `soul`-binary в privileged Debian-12 контейнере (systemd-PID-1) + Keeper-процесс + mTLS + реальное apply. Flagship-сценарии. | nightly / on-demand; **feature-complete** (5 slices L3b-1..L3b-5 = done): real CSR Bootstrap + `smoke-nginx-live` (apt + systemd) + multi-host `redis-cluster-live` (3 контейнера); `make e2e-live` реально гоняет nightly. L3b-6 (drift-live) — done: `TestL3bDriftLive_HelloWorld` гоняет drift-check на живом soul через реальный `core.file.Plan` (модуль `core.file.present`), а не stub-Plan как L3a |
| **L3c** — E2E k8s | `tests/e2e-k8s/` | `e2e_k8s` | kind-cluster, real K8s-deployment Keeper + Soul + Redis-Cluster + PG. HA-кейсы (Watchman, Toll, leader-failover). | weekly / pre-release, **L3c-1..L3c-5 part A готовы** (single-keeper ping, multi-keeper + Soul CSR Bootstrap, kill-leader failover, Toll degraded-mode); L3c-5 part B (redis-cluster resharding) — каркас + t.Skip до in-cluster git-server-pod |
| **L4** — manual soak | — | — | Ручная пред-релизная проверка под нагрузкой. | пост первой проды |

## Куда писать тесты

- **Чистая функция / классификатор / парсер** → L0 в `<module>/<pkg>/<file>_test.go`.
- **CRUD-слой / БД-миграция / Vault-клиент** → L1 `integration_test.go` с `//go:build integration`.
- **Render-пайплайн destiny / scenario / state-миграция** → L2 Trial-фикстура (`_trial/`).
- **Контракт scenario-runner → apply_runs → audit → metrics** → L3a в `tests/e2e/`.
- **Реальная мутация хоста (filesystem / systemd)** → L3b в `tests/e2e-live/`.
- **HA-сценарий (multi-Keeper / failover)** → L3c в `tests/e2e-k8s/` (когда заработает).

## Связь с CI

- `make check` гонит L0 + L2 (через `lint`). L1 / L3a / L3b / L3c — отдельными
  таргетами по запросу (требуют docker / kind).
- GitHub Actions workflow для регулярного прогона L3a/L3b/L3c — отдельная задача
  ([ADR-039 § 7](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря)),
  не добавляется в текущий slice.

### Pre-release чеклист (docker-зависимое вне `make check`)

`make check` сознательно docker-free, поэтому всё, что требует контейнеров,
из него выпадает и НЕ ловится на каждом PR. Перед релизом эти таргеты
**обязательно прогнать вручную** — иначе stale-фейлы копятся незамеченными
(так уже накопилось 6 stale-фейлов в L1, всплывших только при ручном прогоне):

- **L1 — `make test-integration`** (build-tag `integration`, testcontainers
  PG / Redis / Vault, нужен docker). Покрывает keeper-integration, которую
  docker-free `make check` не достаёт: scenario-dispatch, state-migrate,
  gRPC / EventStream, recovery, topology, Redis-координацию,
  Voyage-оркестрацию. **Самый важный пункт чеклиста.**
- **L3a — `make e2e`** (build-tag `e2e`, testcontainers, нужен docker) —
  контракт apply_runs lifecycle / RBAC / audit / MCP, когда L3a-slice стабилен.
- **L3b — `make e2e-live`** (build-tag `e2e_live`, privileged docker) —
  smoke на реальном `soul`-бинаре; обычно nightly, но прогнать перед релизом.

Разовый флак на старте контейнера (testcontainers infra — например timeout
поднятия Vault) — это не регресс кода: перепрогнать изолированно
затронутый пакет (`go test -tags=integration ./<pkg>/...`), а не откатывать изменения.

## Документы по уровням

- [e2e.md](e2e.md) — нормативная спека L3a harness: формат fixtures/expectations,
  как добавлять новые E2E-кейсы, контракт soul-stub-а. Включает L3b-раздел
  (real-soul-in-container, ссылка на `tests/e2e-live/README.md`) и L3c-раздел
  (kind + bitnami Helm, ссылка на `tests/e2e-k8s/README.md`).

## См. также

- [ADR-023 — Trial и DSL-coverage](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage) — L2.
- [ADR-039 — E2E три уровня](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря) — L3a/L3b/L3c.
- [dev/local-setup.md](../dev/local-setup.md) — testcontainers-go и docker-compose dev-стек (L1-инфра).
