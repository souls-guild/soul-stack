# Операционный runbook Soul Stack

Reference-документация для DevOps / SRE, разворачивающих и эксплуатирующих Soul Stack в продакшене. Сосредоточена на **операционных деталях**: что разворачивать, как бэкапить, как масштабировать, что делать при отказе. Не туториал и не повтор архитектуры — для архитектуры см. [`docs/architecture.md`](../architecture.md) с ADR-001…053 ([индекс](../adr/README.md)).

Документация разбита по логическим зонам. Один раздел — одна тема, на которую можно ссылаться отдельно.

| Файл | Зона |
|---|---|
| [deployment.md](deployment.md) | Деплой бинарей: артефакты, системные требования, systemd, multi-keeper за LB, базовая раскатка. |
| [bootstrap-rbac.md](bootstrap-rbac.md) | Bootstrap первого Архонта (`keeper init`), создание дополнительных Архонтов, RBAC. |
| [infra.md](infra.md) | Инфра-зависимости: Postgres (backup/restore/retention/sizing), Redis (что лежит, TTL/eviction), Vault (KV/PKI/SSH/Sigil, ротации). |
| [scaling.md](scaling.md) | Горизонтальное масштабирование Keeper: Conclave, Watchman, Acolyte-пул, Refuse-guard, LB. |
| [upgrade.md](upgrade.md) | Rolling upgrade Keeper / Soul: forward-compat proto, state_schema migrations, откат. |
| [monitoring.md](monitoring.md) | Метрики Prometheus, OTel-трейсы, ключевые алерты, логи. |
| [disaster-recovery.md](disaster-recovery.md) | Восстановление при отказе PG / Redis / Vault / Keeper / полной катастрофе. |
| [recovery-reclaim-apply-runs.md](recovery-reclaim-apply-runs.md) | Включение Reaper-правила `reclaim_apply_runs` в проде (операционализация GATE-1 из ADR-027). |
| [faq.md](faq.md) | Часто встречающиеся проблемы и их триаж (зомби-Souls, висящий applying, drift 422, …). |

## Что не здесь

Эта папка — **только операционные детали** конкретной инсталляции. Архитектурные обоснования, контракты и нормативные спецификации живут в основной документации, см. ниже.

| Тема | Куда смотреть |
|---|---|
| Архитектурные решения (ADR-001…053) | [`docs/architecture.md`](../architecture.md), [индекс ADR](../adr/README.md). |
| Словарь имён | [`docs/naming-rules.md`](../naming-rules.md). |
| Модель угроз (активы, поверхности, остаточные риски, требования к окружению) | [`docs/security/threat-model.md`](../security/threat-model.md). |
| Конфиг `keeper.yml` (типы полей, диагностика парсера) | [`docs/keeper/config.md`](../keeper/config.md). |
| Конфиг `soul.yml` | [`docs/soul/config.md`](../soul/config.md). |
| Прод-развёртывание Keeper (Vault AppRole + persistent + auto-unseal + signing-key) | [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md). |
| Reaper-правила, recovery-enable | [`docs/keeper/reaper.md`](../keeper/reaper.md). |
| RBAC: каталог permissions, scope-фильтры | [`docs/keeper/rbac.md`](../keeper/rbac.md). |
| Operator API (HTTP/JSON), MCP-tools | [`docs/keeper/operator-api.md`](../keeper/operator-api.md), [`docs/keeper/mcp-tools.md`](../keeper/mcp-tools.md). |
| Observability: метрики, namespaces, OTel resource-attrs | [`docs/observability.md`](../observability.md). |
| Audit-pipeline (storage, schema, retention) | [`docs/architecture.md` → ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention). |
| State_schema migrations DSL | [`docs/migrations.md`](../migrations.md). |
| Локальный dev-стек (docker-compose) | [`docs/dev/local-setup.md`](../dev/local-setup.md). |
| Раскладка `deploy/` (Dockerfile / systemd / nfpm) | [`deploy/README.md`](../../deploy/README.md). |

## Принципы операционной модели

Кратко, для понимания контекста при чтении следующих разделов:

- **Stateless-кластер Keeper** ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)). N инстансов поверх общей Postgres ([ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres)) и Redis ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)). У каждого инстанса свой `kid`, уникальный в кластере.
- **Горячее → Redis, холодное → Postgres.** Presence Souls (SID-lease), presence Keeper-инстансов ([Conclave](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)), heartbeat-кэш, лидерские lease, pub/sub — в Redis. Реестры (`souls` / `operators` / `incarnation` / `state_history` / `apply_runs` / `audit_log` / `rbac_*` / `service_registry` / `plugin_sigils`) — в Postgres. Инвариант — для масштаба до 100k VM.
- **Безопасность на первом месте** ([requirements.md](../requirements.md)). default-deny RBAC, JWT с коротким TTL, mTLS Keeper↔Soul, Vault-резолв всех секретов на старте, никакого plaintext-секрета в `*.yml`.
- **Документация впереди кода.** Если runbook и реальность расходятся — сначала правится канон в `docs/`, потом код / процедура.
