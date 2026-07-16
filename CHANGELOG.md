# Changelog

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/).
Версионирование артефактов — через git ref ([ADR-007](docs/adr/0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field)), отдельного `version:` поля у Service/Destiny/Module нет.

## [Unreleased]

Задел после `v0.1.0-beta.1`. Пока пусто.

---

## [v0.1.0-beta.1] — 2026-06-15

Первый тег закрытой беты. Один git-тег = версия всех 7 модулей go.work ([ADR-011](docs/adr/0011-go-layout.md)); версия инъектится в бинари через `-X main.<var>`, печатается командой `keeper version` / `soul version` / `soulctl version`. Дистрибуция беты — build-from-source ([CONTRIBUTING.md](CONTRIBUTING.md)), процедура выпуска — [RELEASING.md](RELEASING.md).

### Ключевое в бете

- **OpenAPI code-first пивот** — источник правды OpenAPI 3.1 теперь huma-агрегатор в коде (`HumaFullSpecYAML`); `docs/keeper/openapi.yaml` — производный committed-снимок с drift-guard'ом `make check-openapi` ([ADR-054](docs/adr/0054-openapi-code-first.md)).
- **RapiDoc на `/docs`** — встроенная отрисовка OpenAPI-спеки в Keeper.
- **Retention** — настраиваемое удержание журналов/истории (audit, state_history).
- **Version-инъекция** — `git describe` → ldflags `-X main.<var>`, единая версия всех бинарей ([ADR-011](docs/adr/0011-go-layout.md)).
- **Security-харднинг до беты** — `govulncheck` supply-chain-гейт в `make check`, Vault least-privilege policy, edge-hardening, retention security-обзор.

### Added
- Полностью собранный MVP-каркас Keeper-кластера, Soul-агента, soul-lint и soulctl-CLI.
- Подсистемы Tide+Surge, Beacons/Vigil-Oracle-Decree, Scry drift-detect, Push (Variant C), Errand, Toll detector.
- 17 Soul-side core-модулей + 3 Keeper-side core (cloud/vault/soul-registered).
- 6 CloudDriver-плагинов (AWS/GCP/Yandex.Cloud/Azure/OpenStack/Proxmox) и SSH-провайдеры static/Vault-CA/Teleport.
- E2E-харнесс L3a/L3b/L3c, runbook'и `docs/operations/`, SBOM/deb/rpm-пакетирование, локальный CI-гейт `make check`.
- Нагрузочный харнес `soul-legion` (`make stress`) — симулирует флот от 1k до 25k Souls и прогоняет read-нагрузку (24 GET-ручки), write-цикл (create→delete) и Voyage. Подтверждено: Keeper держит линейный масштаб до 25k+ одновременных стримов при ~0.12 MiB на душу (внутренний инструмент проверки масштаба, не часть рантайма).

### Changed
- Источник presence Soul — Redis-lease, поле `souls.status` больше не фильтрует таргет-резолвер (инвариант «горячее → Redis, не PG»).
- Soulprint в JSONB/template — `snake_case`-канон (BUG-A).
- `core.pkg`/`core.service` читают факт через Soulprint (BUG-B Вариант A), apk-version выровнен.
- `apply_runs` помечает нецелевые хосты как `no_match` (FINDING-01).
- Bulk coven-assign получил `mode=replace` и `selector.incarnation`; REST↔MCP паритет.
- `POST /v1/voyages/preview` получил отдельный лимит запросов (`voyage_preview`, 30 в окне / 60 пик) и больше не делит лимит с созданием Voyage (`voyage_create`, 10/20) — частые preview-запросы не упираются в write-лимит создания ([ADR-050](docs/adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api) / [ADR-043](docs/adr/0043-voyage.md#adr-043-voyage--unified-batch-run) amendment).

### Fixed
- Большие командные Voyage (порядка 10k хостов) больше не подвисают на финализации. Раньше Redis pub/sub подписывался на отдельный канал для каждого applyID, и на крупном флоте число подписок упиралось в лимит Redis `maxclients` — Voyage не завершался. Теперь Keeper не поднимает cross-keeper-мост для локально подключённых Souls и использует фиксированный набор шардированных каналов событий (`events:shard:<n>`, K=256) вместо канала-на-applyID. Voyage на 10k хостов: с «не завершается» до ~11.6 с ([ADR-006](docs/adr/0006-cache-redis.md#adr-006-cache-and-coordination--redis) amendment).
- §26 audit-log scaling до 100k+ VM — 5 вариантов рассмотрены, решение отложено.
- Tide: cancellation, stop-and-wait, SSE `/v1/tides/{id}/progress` (есть polling GET) — post-MVP.
- Vigil inotify: recursive + throttle — P3.
- ADR-030 S5-final `PortentEvent.data` hard-cut — P3 post-1-prod-release.
- Push S3 (compile-time wire) — без use-case, revive по запросу.
- Multi-cluster federation — отвергнуто, horizontal scale одного кластера покрывает требования.
- Cloud parity Фаза 4 (расширение за пределы 6 драйверов) — решение пользователя в следующем релизе.
- Shepherd — балансировка стримов при scale-out, ждёт amend ADR-006 + architect-дизайн.
- ADR-018 user-collectors `/etc/soul/soulprint.d/*` — отдельный ADR.

### Known limitations (бета)
Осознанный out-of-scope беты, не баги (полный список — [docs/known-limitations.md](docs/known-limitations.md)):
- **Cloud-provisioning** — CloudDriver-плагины и `core.cloud.provisioned` присутствуют, но end-to-end cloud-CRUD не вошёл в бету (Cloud parity Фаза 4 — следующий релиз).
- **MCP-cadence** — управление расписаниями (Cadence) через MCP пока не покрыто; путь — OpenAPI/soulctl.
- **Audit-log scaling до 100k+ VM** — на флоте 100k VM PG-INSERT-rate журнала аудита упрётся в потолок; 5 вариантов масштабирования рассмотрены, решение отложено (§26 backlog).
- **Нет немедленного отзыва JWT** — ревокация оператора = `revoked_at`, активные JWT работают до `exp` (защита — короткий TTL), ([ADR-014](docs/adr/0014-operator-identity.md#adr-014-operator-identity-model-archon)).
- **Push — узкий профиль** — `keeper.push` (Variant C) покрыт базово; S3 compile-time wire без use-case, расширения по запросу.
- **Внешний pentest — пост-бета** — независимый аудит безопасности запланирован после беты, до GA.

### Security
- mTLS Keeper↔Soul по [ADR-012](docs/adr/0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add); приватник Soul никогда не покидает хост (CSR-онбординг).
- [ADR-026](docs/adr/0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index) Sigil — Keeper-signed digest-индекс для всех плагинов, default-deny при отсутствии подписи.
- JWT-аутентификация операторов ([ADR-014](docs/adr/0014-operator-identity.md#adr-014-operator-identity-model-archon)), подписка через Vault KV `secret/keeper/jwt-signing-key`.
- RBAC default-deny, multi-coven AND-merge в Tide ([ADR-040](docs/adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override) fail-closed).
- Augur ([ADR-025](docs/adr/0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access)) — Keeper-side брокер внешнего доступа Soul, default-deny.
- Vault least-privilege policy + recovery-enable процедура (`docs/operations/disaster-recovery.md`).
- Migration-CEL sandbox запрещает `vault/now/register/soulprint/essence/input` ([ADR-019](docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

---

## Roadmap → Released — feature-complete MVP

Раздел покрывает фактическое состояние кода и документации на 2026-05-27.

### Keeper cluster
- Stateless N-Keeper поверх общей PG/Redis ([ADR-002](docs/adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)); SID-lease в Redis ([ADR-006](docs/adr/0006-cache-redis.md#adr-006-cache-and-coordination--redis)).
- Bootstrap первого Архонта через `keeper init --archon=<aid>`, AID-валидация, partial unique index ([ADR-013](docs/adr/0013-bootstrap-archon.md#adr-013-bootstrap-the-first-archon)).
- Identity-реестр `operators` + JWT auth, FK на все аудит-поля ([ADR-014](docs/adr/0014-operator-identity.md#adr-014-operator-identity-model-archon)).
- Hot-reload конфига с write-back YAML ([ADR-021](docs/adr/0021-hot-reload-config.md#adr-021-hot-reload-of-config-with-write-back-yaml)); Toll-leader использует `UpdateConfig` + RWMutex-snapshot.
- Conclave (реестр живых инстансов) + Watchman (изоляция-детект + soul-shedding) + refuse-guard `acolytes=0` warn.
- Toll cluster-wide detector массового оттока Souls ([ADR-038](docs/adr/0038-toll.md#adr-038-toll--a-cluster-wide-detector-of-mass-souls-attrition)) с hot-reload и webhook diff-recycle.
- Tide + Surge — invocation-time scope chunking, AND-merge target, REPLACE concurrency, abort+continue, per-Surge state-commit, Acolyte-style lease для failover ([ADR-040](docs/adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)).
- Reaper — лидер через Redis-lease, чистит pending/zombie/expired seeds; soft-delete/архив state_history.

### Souls (agents)
- gRPC bidi `EventStream` поверх mTLS, `oneof payload`, тематические `.proto`-файлы, forward-compat only-add ([ADR-012](docs/adr/0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)).
- Typed Soulprint ([ADR-018](docs/adr/0018-soulprint-typed.md#adr-018-soulprint-typed-schema-mvp)): `SoulprintFacts` (Os/Kernel/Cpu/Memory/Network), `pkg_mgr`/`init_system` собираются на стороне Soul.
- State_schema migration DSL ([ADR-019](docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)): плоский (`rename`/`set`/`delete`/`move`) + CEL в `set.value` + structural `foreach`; одна PG-транзакция, snapshot per-step.
- Soul-reconcile dispatched-orphan ([ADR-027(g)](docs/adr/0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)); WardRoster в proto.
- Один и тот же `soul`-бинарь работает в pull (демон) и push (oneshot) — модули применяются одинаково.

### Scenario / Destiny
- Coven = только стабильные теги, роль НЕ Coven ([ADR-008](docs/adr/0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only)); declared-роль только в `incarnation.spec.hosts[].role`.
- Scenario-DSL полный set ([ADR-009](docs/adr/0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)): `on:`/`where:`/`serial:`/`run_once:`/`apply:`/`state_changes` + двухуровневый резолв ресурсов.
- Шаблонизатор ([ADR-010](docs/adr/0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)): CEL для YAML-выражений, Go text/template + sprig allowlist для файлов, маркер `${ … }`, strict-mode, secret-masking.
- Top-level `output:` в `destiny.yml`, читается через `register:` на applier-задаче.
- `soulprint.hosts` scenario-only аксессор хостов прогона со стабильными фактами.
- Scry drift-detection ([ADR-031](docs/adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): Plan-контракт pure-read на 14 core-модулей, on-demand check-drift + фоновый `scry_background` (default OFF).
- Trial-runner `soul-lint trial` ([ADR-023](docs/adr/0023-trial-test-runner.md#adr-023-test-runner-trial-soul-trial-and-dsl-coverage)) с L0/L1/L2-coverage, `assert.state_after`.

### Modules (core MVP)
- 17 Soul-side core ([ADR-015](docs/adr/0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list) + расширения): `pkg`/`file`/`service`/`user`/`group`/`exec`/`cmd`/`cron`/`mount`/`git`/`archive`/`sysctl`/`firewall`/`http`/`line`/`repo`/`url`; `core.file.rendered` — единственный шаг рендера.
- 3 Keeper-side core ([ADR-017](docs/adr/0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read)): `core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`.
- Errand pull-ad-hoc exec вне scenario ([ADR-033](docs/adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-outside-a-scenario)) с E1..E5: HTTP handler, cross-keeper routing, Reaper purge, MCP tools, soulctl-команды, `?module=` фильтр.
- Per-модульный canon-doc в `docs/module/core/<name>/README.md`.

### Plugins (SDK Phase 2)
- Единый gRPC-stdio handshake для SoulModule/CloudDriver/SshProvider/soul_beacon ([ADR-020](docs/adr/0020-plugin-infrastructure.md#adr-020-plugin-infrastructure-manifest-format-handshake-lifecycle)).
- Sigil ([ADR-026](docs/adr/0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)) — Keeper-signed digest-index, soul-side cache.
- Companion-репозиторий `soul-stack-plugins/` + template `soul-mod-template`.
- `soul-lint plugin-init` CLI через `go:embed` для скаффолдинга нового плагина.
- 3 pilot официальных плагинов: `soul-mod-docker-container`, `soul-mod-nginx-vhost`, `soul-mod-postgres-user` (namespace `official`).

### Beacons / Vigil-Oracle-Decree
- Event-driven мониторинг ([ADR-030](docs/adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor)): SaltStack beacons+reactor analog.
- Vigil scheduler на Soul + 4-й plugin-kind `soul_beacon` (`port_closed`/`disk_full`/`process_absent`/`http_unhealthy`/`service_down`/`file_changed`).
- Oracle Portent-reactor на Keeper: Vigil/Decree CRUD-реестры, OpenAPI+MCP, RBAC, audit, scenario-enqueue.
- Circuit-breaker auto-disable Decree, метрики Oracle+beacon.
- Typed `PortentPayload` (parity с ADR-018 Soulprint), inotify P0/P1, `action=scenario-only`.

### Push (Variant C)
- Multi-host destiny push без incarnation/scenario ([ADR-032](docs/adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-without-incarnationscenario)).
- S0..S7: SSH-транспорт (CA-signed host certs), Deliverer (SHA-256 cache), Cleaner, SshDispatcher, orchestrator, multi-CA, `souls.ssh_target` jsonb, `push_providers` PG-таблица, auto-import legacy, multi-keeper bootstrap в kind, multi-provider routing.
- SSH-провайдеры: `soul-ssh-static`, `soul-ssh-vault` (variant B, ephemeral SSH CA), `soul-ssh-teleport` (proxy_jump через bastion).

### Cloud
- 6 CloudDriver-плагинов: `soul-cloud-aws`/`gcp`/`yc`/`azure`/`openstack`/`proxmox`.
- `core.cloud.provisioned` (`created`/`destroyed`) — keeper-side ([ADR-017](docs/adr/0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read)).
- Cloud-init bootstrap MVP (`keeper/internal/cloudinit/`).
- Credentials-flow A + Status/List only-add API.

### Work-queue / Tide
- Acolyte/Ward/Summons work-queue ([ADR-027](docs/adr/0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)): PG-claim, retry/backoff, recovery-reclaim, S6 Soul-reconcile dispatched-orphan.
- Tide PG schema 055, claim-loop, FinalizeWithOwnership, real surge-loop, spawn-await, decision-gate ([ADR-040](docs/adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)).
- Audit canon: handler emit, `souls_in_surge`; HTTP/MCP/soulctl REST↔MCP паритет.

### Observability
- Prometheus-primary + OTel-bridge ([ADR-024](docs/adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)).
- Метрики Oracle/beacon/Toll/Acolyte/Ward + `soul /metrics` basic-auth (`password_file`).
- OTel-collector + Jaeger в dev-окружении.
- Audit-pipeline ([ADR-022](docs/adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)): PG + multi-sink + OTel-export, GET `/v1/audit`.

### Security
- mTLS Keeper↔Soul, JWT-операторы, Vault integration (Soul-safe клиент в `shared/vault`, server-side в `keeper/internal/vault`).
- Sigil plugin trust (default-deny, Keeper-signed digest).
- RBAC default-deny, per-Coven/service scope для incarnation, multi-coven AND fail-closed.
- Augur Keeper-side брокер внешнего доступа Soul.
- archive zip-slip защита, edge-hardening AWS-пилота, refuse-guard multi-keeper.

### Documentation
- [docs/architecture.md](docs/architecture.md) — 40 ADR (ADR-001…040, без ADR-034/036/037).
- [docs/scenario/](docs/scenario/README.md) — concept/orchestration/tide-state-commit.
- [docs/keeper/](docs/keeper/README.md) — modules/rbac/storage/push/reaper/augur/cloud/mcp-tools/openapi/prod-setup.
- [docs/module/core/<name>/README.md](docs/module/) — per-модульная документация (обязательный canon).
- [docs/operations/](docs/operations/README.md) — bootstrap-rbac/deployment/disaster-recovery/scaling/upgrade/monitoring/faq/recovery-reclaim-apply-runs.
- [docs/soul/](docs/soul/README.md) — concept/connection/identity/onboarding/soulprint/modules/config.
- [docs/testing/e2e.md](docs/testing/e2e.md) — L1/L2/L3a/L3b/L3c уровни ([ADR-039](docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity)).
- [docs/templating.md](docs/templating.md), [docs/migrations.md](docs/migrations.md), [docs/observability.md](docs/observability.md), [docs/soul-lint.md](docs/soul-lint.md), [docs/roadmap.md](docs/roadmap.md), [docs/ideas.md](docs/ideas.md).
