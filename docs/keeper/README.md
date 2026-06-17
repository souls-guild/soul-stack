# Keeper — индекс

Документация по Keeper — центральному серверу Soul Stack. Keeper хранит реестр Souls и Destiny-каталог, валидирует и раздаёт прогоны, агрегирует Soulprint, выставляет OpenAPI/MCP/gRPC; работает горизонтально масштабируемым stateless-кластером поверх общей Postgres и Redis.

## С чего начать

| Документ | О чём |
|---|---|
| [concept.md](concept.md) | Что такое Keeper: роль, HA-stateless модель, KID, интерфейсы оператора, что входит в `keeper`-бинарь (модуль `keeper.push`). |
| [storage.md](storage.md) | Хранилища Keeper-кластера: что в Postgres (souls, soul_seeds, bootstrap_tokens, Destiny-каталог, incarnation, state_history), что в Redis (heartbeat-кэш, lease на SID, pub/sub, лидерские lease `reaper:leader` / `conductor:leader`). |
| [push.md](push.md) | Push-режим (`keeper.push`): SSH-доставка Destiny без Soul-агента, миграция push↔agent, раскладка `/var/lib/soul-stack/`, алгоритм прогона. |
| [reaper.md](reaper.md) | Жнец: фоновая чистка БД (cleanup-домен), лидерский lease `reaper:leader`, правила (`expire_pending_seeds`, `purge_used_tokens`, …), резерв имени Charon. Cadence-спавн вынесен в Conductor (ADR-048). |
| [conductor.md](conductor.md) | Дирижёр: leader-elected исполнитель Cadence-расписаний, lease `conductor:leader` (независим от Reaper), tick-interval `cadence_scheduler.interval`, default-ON при Redis (footgun-guard), метрики `keeper_conductor_*` ([ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)). |
| [plugins.md](plugins.md) | Plugin-инфраструктура Keeper-а: контракты `CloudDriver` и `SshProvider`, каталог плагинов в `keeper.yml`. |
| [modules.md](modules.md) | Keeper-side core-модули: спецификация `core.soul.registered` (привязка SID к coven-меткам реестра souls), диспетчер Soul-side/Keeper-side через `on:`. |
| [cloud.md](cloud.md) | Cloud-интеграция (`keeper.cloud`): Provider и Profile в Postgres, cloud-create как шаг сценария, безопасность destroy. |
| [augur.md](augur.md) | **Дизайн (имплементации нет)** брокера внешнего доступа Soul (Augur): live-доступ к Vault / Prometheus / ELK во время рендера / apply, две фазы (брокер `delegate=false` / делегация `delegate=true`), реестры `omens` / `rites` в Postgres, нормативный инвариант «master-credential не на Soul» ([ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul)). |
| [rbac.md](rbac.md) | RBAC: роли и permissions, единое применение к OpenAPI / MCP / push. |
| [operator-api.md](operator-api.md) | Operator API: HTTP-эндпоинты Keeper-а (`/v1/*`), conventions, JWT auth, RFC 7807 errors, mapping endpoint ↔ MCP-tool ↔ permission, request/response schemas. Детальные endpoint-секции крупных доменов вынесены в под-папку [`operator-api/`](operator-api/) (парную к [`mcp-tools/`](mcp-tools/)). |
| [run-flavors.md](run-flavors.md) | Сводная таблица entry-point-ов запуска работы на хостах: single-incarnation scenario через agent, батч через Voyage (`kind=scenario` / `kind=command`), single-Errand exec, push по SSH. Decompose «что» (scenario/module) vs «как» (agent/ssh) vs «где» (target). |
| [mcp-tools.md](mcp-tools.md) | MCP-tools каталог: транспорт (MCP-HTTP), auth (JWT Bearer), формат declaration по MCP spec, `_apply_id`-convention для async, mapping ошибок RFC 7807 → MCP-tool error, каталог 72 tool 1:1 с Operator API. Детальные tool-секции вынесены в под-папку [`mcp-tools/`](mcp-tools/). |
| [config.md](config.md) | Полный обход `keeper.yml`: блоки `kid`, `listen`, `postgres`, `redis`, `vault`, `auth`, `otel`, `logging`, `plugins`, `plugin_runtime`, `audit`, `hot_reload`, `reaper`, `cadence_scheduler` (Conductor, ADR-048). Перенесены в БД (отвергаются как `unknown_key`): `rbac` (ADR-028), `services`/`default_destiny_source`/`default_module_source` (ADR-029). Зарезервированные (не нормативные): `reactor`. |
| [prod-setup.md](prod-setup.md) | Прод-развёртывание: отличия от dev, Vault AppRole + persistent + auto-unseal, least-privilege [vault-policy.hcl](../../examples/keeper/vault-policy.hcl), ротация JWT signing-key, гейт recovery-enable. |

## Связанные документы

- [`docs/architecture.md`](../architecture.md) — источник правды по архитектуре:
  - [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) — gRPC bidi + HA Keeper-кластер.
  - [ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper) — раскладка бинарей.
  - [ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres) — Postgres как единственное холодное хранилище.
  - [ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis) — Redis как горячий слой и координация.
  - [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) — версия артефактов через git ref.
  - [Артефакты Soul Stack: что в git, что в БД](../architecture.md#артефакты-soul-stack-что-в-git-что-в-бд) — граница git/PG.
- [`docs/naming-rules.md`](../naming-rules.md) — словарь имён (Keeper, KID, Reaper/Charon, `keeper.push`, …).
- [`docs/soul/`](../soul/README.md) — соседняя папка про `soul`-бинарь: идентичность Soul, модули, хостовый cleanup.
- [`docs/destiny/`](../destiny/README.md) — Destiny как артефакт, который Keeper хранит и раздаёт.
- [`docs/requirements.md`](../requirements.md) — сквозные требования (OpenAPI, MCP, RBAC, Vault, OTel, hot-reload, ротация логов).
- [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) — рабочий пример конфига одного инстанса Keeper-кластера.
