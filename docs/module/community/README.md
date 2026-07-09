# Каталог community-плагинов

`community.*` — namespace для third-party плагинов: они **не** встроены в
`soul`-бинарь (в отличие от core-модулей `core.*`) и **не** поставляются командой
Soul Stack официально (в отличие от `official.*`, [official/README.md](../official/README.md)).
Через SoulModule-контракт (gRPC-over-stdio, [ADR-020](../../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle))
такой плагин применяется на хосте наравне с core-модулями.

Бинари — `soul-mod-community-<name>`. Реализация — в
[`examples/module/`](../../../examples/module/) (в этом репозитории — как
референс-пример; собирается отдельным `go.mod`, в `go.work` не входит).

## Реализованные

| Плагин | States | Назначение |
|---|---|---|
| [`community.redis`](redis/README.md) | `command` / `pinged` / `role` / `replica-synced` / `offset-synced` / `config` / `acl` / `cluster` / `replica` / `detached` / `sentinel` (12) | ОСНОВНОЙ интерфейс к **живому Redis** (концепция Ansible-роли `redis`): scenario оркеструет порядок/rolling, плагин исполняет одну операцию над одним инстансом. Backend — `go-redis/v9`. Покрывает hot-reload (`config`/`acl`), сборку и операционную эволюцию кластера (`cluster` create/add-node/remove-node/reshard), live-миграцию между кластерами (`cluster` join-external/failover-takeover/forget-external), репликацию и миграцию из внешнего источника (`replica source_external` → `offset-synced` → `detached`), Sentinel-реконсиляцию. |
| [`community.mongo`](mongo/README.md) | `pinged` / `user` / `command` (3, **PILOT**) | ОСНОВНОЙ интерфейс к **живому MongoDB** (PILOT-срез, концепция Ansible-роли `mongodb`): scenario оркеструет порядок/health-gate, плагин исполняет одну операцию над одним `mongod`-инстансом. Backend — `go-mongo-driver` (не `mongosh`). `user` (createUser/dropUser upsert) заводит первого admin через **localhost-exception** (аналог redis `default_admin` bootstrap: no-auth loopback-коннект, пока admin-БД пуста). PILOT-скоуп — только **standalone**; replica-set / sharded / keyFile / TLS — следующие слайсы. |

## Соглашения (общие для community-плагинов)

- **Capability-минимум.** Плагин объявляет ровно те `required_capabilities`, что ему
  нужны ([ADR-020(f)](../../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)).
  `community.redis` и `community.mongo` — только `network_outbound`, **без**
  `vault_access`: секреты (пароль, PEM) резолвит Keeper в render-фазе и передаёт
  плагину значением
  ([ADR-012](../../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)),
  свой Vault-клиент плагину не нужен.
- **Secret-маскинг.** Secret-поля объявляются `secret: true` + `pattern: "^vault:.*"`
  (маркер vault-источника для аудита); маскинг — выходным слоем `shared/audit`
  ([templating.md §7.4](../../templating.md)). Код-инвариант «секрет не течёт в
  события/логи/ошибки» проверяется guard-тестами (L0).
- **Dry-run.** `PlanReadSafe` ([ADR-031 Scry](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile))
  плагин реализует по своему усмотрению. `community.redis` и `community.mongo` его
  **не** реализуют сознательно: на `dry_run` host применяет default-deny (честный
  «предпросмотр не поддержан», а не ложное «нет дрифта»).

## См. также

- [README.md](../README.md) — каталог модулей (core + ссылки на official/community).
- [official/README.md](../official/README.md) — official-плагины (`official.*`).
- [ADR-016](../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) — parity-стратегия: core-рерайт + community-плагины `soul-mod-*`.
- [ADR-020](../../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle) — plugin-инфраструктура (manifest/handshake/lifecycle).
- [soul/modules.md](../../soul/modules.md) — хостовая сторона: где лежат бинари плагинов, кеш, cleanup.
