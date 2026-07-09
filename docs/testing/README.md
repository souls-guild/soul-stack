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
| **L4** — manual soak + cloud live-run | — | — | Ручная пред-релизная проверка под нагрузкой **+ повторяемый облачный оркестратор** `scripts/e2e-cloud/` (create / операционные / destroy через Operator API keeper'а на VM, teleport), см. [e2e-cloud.md](e2e-cloud.md). | пост первой проды / on-demand |

## Куда писать тесты

- **Чистая функция / классификатор / парсер** → L0 в `<module>/<pkg>/<file>_test.go`.
- **CRUD-слой / БД-миграция / Vault-клиент** → L1 `integration_test.go` с `//go:build integration`.
- **Render-пайплайн destiny / scenario / state-миграция** → L2 Trial-фикстура (`_trial/`).
- **Контракт scenario-runner → apply_runs → audit → metrics** → L3a в `tests/e2e/`.
- **Реальная мутация хоста (filesystem / systemd)** → L3b в `tests/e2e-live/`.
- **HA-сценарий (multi-Keeper / failover)** → L3c в `tests/e2e-k8s/` (когда заработает).
- **Живой облачный операционный сценарий / create-destroy на постоянном keeper** → `scripts/e2e-cloud/`
  (не Go-harness, bash поверх Operator API через teleport; см. [e2e-cloud.md](e2e-cloud.md)).

## Связь с CI

- `make check` гонит L0 + L2 (через `lint`). L1 / L3a / L3b / L3c — отдельными
  таргетами по запросу (требуют docker / kind).
- Перед батч-коммитом **крупной** фичи — обязательный локальный live-гейт
  `make e2e-live-gate` (курируемое L3b-подмножество, docker; см.
  [Локальный live-гейт крупных фич](#локальный-live-гейт-крупных-фич-make-e2e-live-gate)).
  `make check` его не включает.
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

## Локальный live-гейт крупных фич (`make e2e-live-gate`)

`e2e-live-gate` — **обязательный локальный live-прогон перед батч-коммитом каждой
крупной фичи**. Это курируемое подмножество L3b (~15-25 мин, нужен docker), а не
весь `make e2e-live` (тот остаётся nightly / pre-release). Смысл гейта — доказать
на реальном `soul`-бинаре, что ключевая механика жива, до того как правки уйдут в
коммит.

**Что прогоняет** (build-tag `e2e_live`: реальный `soul` в privileged Debian-12
контейнере + Keeper-процесс на хосте + mTLS + живое apply). Маска —
`TestL3bModuleDeliveryLive|TestL3bSmokeNginxLive|TestL3bPluginChannel`:

- **Механика доставки `SoulModule`** (fixture `tests/e2e-live/module-delivery-live`,
  `TestL3bModuleDeliveryLive_*`) — флагманская проверка, ради которой гейт заведён:
  синтез шага установки модуля из `service.yml::modules[]` по
  [ADR-065](../adr/0065-core-module-installed.md) → `FetchModule` с Keeper →
  Sigil-verify подписи → hot-register в реестре Soul → живое исполнение
  доставленного модуля против реального redis.
- **Базовый apply-смок nginx** (`TestL3bSmokeNginxLive_*`): `core.pkg` apt-install +
  `core.service` systemd-start на живом хосте — проверка, что обычный apply не сломан.
- **Smoke plugin-канала** (`TestL3bPluginChannel_*`): каталог модулей + allow-механика
  gRPC-stdio канала плагинов.

**Когда обязателен.** Перед батч-коммитом фичи, подходящей под критерий «крупная» —
те же триггеры, что эскалируют на architect: затронуто **>5 файлов** ИЛИ правятся
**ключевые узлы** (контракт Keeper↔Soul, plugin-инфраструктура, render/dispatch-пайплайн,
`state_schema`, шаблонизатор). Мелкая правка гейт не требует. `make check` гейт
**не** включает (он docker-free); полный `make e2e-live` — nightly / pre-release.

**Как запускать:**

```bash
make e2e-live-gate
```

Таргет сам собирает нативный `keeper` (`make build` — harness запускает Keeper на
хосте) и linux-`soul` (`make build-linux` — mount в контейнер); плагин
`community.redis` собирает сам тест. `E2E_KEEPER_HOST` (IP, по которому
soul-контейнер дозванивается до Keeper-на-хосте) **автодетектится таргетом** через
`hostname -I`. На **WSL2** это критично: `localhost` из контейнера не виден, нужен
LAN-IP. Переопределить вручную — `make e2e-live-gate E2E_KEEPER_HOST=<ip>`.

**Запускать изолированно**, без параллельной docker/build-нагрузки: L3b-тесты
поднимают docker-контейнеры (keeper + PG + Redis + Vault + soul) и на WSL2
чувствительны к конкурентной docker-нагрузке — при параллельном запуске с другой
тяжёлой docker/build-работой Vault-контейнер может не подняться (connection
refused). При флаке поднятия контейнеров (инфраструктура, не регресс кода) —
перезапустить гейт.

**Что НЕ покрывает** (это стенд / облако / ФАЗА 2 / L3c-k8s, не локальный гейт):
облачный provision (`CloudDriver`), Nexus-binary `install_method`,
exporter / vector-destinies (egress-мониторинг), sentinel- и cluster-топологии redis,
multi-keeper HA.

**Redis-create локально НЕ покрыт.** Канонический `TestL3bRedisLive_CreateStandalone`
имеет **безусловный `t.Skip`** — то есть create redis-сервиса локально не
проверяется вообще. Это осознанное дизайн-решение (architect): локальный гейт
гарантирует **только механику доставки модулей**, а redis-паритет (input
`version`=Nexus-enum, `install_method`=binary, sentinel- / cluster-топологии)
обеспечивается **стендовыми live-прогонами (ФАЗА 2)** против реального
Redis / DragonFly, а не локальным docker-гейтом. Будущему читателю: это НЕ пробел
покрытия. Механику доставки модулей гейт проверяет через отдельную лёгкую fixture
`tests/e2e-live/module-delivery-live`, не через полный redis-create.

## Документы по уровням

- [e2e.md](e2e.md) — нормативная спека L3a harness: формат fixtures/expectations,
  как добавлять новые E2E-кейсы, контракт soul-stub-а. Включает L3b-раздел
  (real-soul-in-container, ссылка на `tests/e2e-live/README.md`) и L3c-раздел
  (kind + bitnami Helm, ссылка на `tests/e2e-k8s/README.md`).
- [e2e-cloud.md](e2e-cloud.md) — runbook облачного live-E2E оркестратора
  (`scripts/e2e-cloud/`): bash поверх Operator API через teleport, два мира keeper
  (`local` / `tsh`), suites create / create-destroy / operations, ассерты по apply_run,
  формат отчёта и exit-коды. L4-adjacent, вне ephemeral-инварианта L3a/L3b.

## См. также

- [ADR-023 — Trial и DSL-coverage](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage) — L2.
- [ADR-039 — E2E три уровня](../adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря) — L3a/L3b/L3c.
- [dev/local-setup.md](../dev/local-setup.md) — testcontainers-go и docker-compose dev-стек (L1-инфра).
