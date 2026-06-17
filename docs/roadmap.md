# Soul Stack — Roadmap (MVP → Prod)

Живой план перехода от MVP к полноценному прод-решению. Статусы: ✓ готово · 🟡 в работе · ⬜ не начато · 💤 Ящик идей ([ideas.md](ideas.md)).

> Обновляется PM по ходу. Решения фиксируются в ADR ([architecture.md](architecture.md)); здесь — план и статус, не нормативный источник.

## Статус (2026-05-26)
- MVP feature-complete, релиз-гейт закрыт, HA провалидирован на масштабе (3 keeper + 9-node real redis-cluster, 0 багов ядра).
- Код на GitHub — **временный** репо `co-cy/soul-stack` (переедет; module-path и реальная подпись финализируются на постоянном репо). Коммиты в `main`, CI = `make check`.
- **К 2026-05-26 закрыто:** 6 cloud-провайдеров (AWS пилот + GCP/YC/Azure батч-1 + OpenStack/Proxmox батч-2); 3 SshProvider-плагина (static / Vault SSH CA / Teleport); drift Slice A+B (Plan pure-read на 17 core-модулей: 12 stateful покрыты, 2 deferred Slice C, 3 verbs not-applicable); release-ops полностью.
- **В работе сейчас:** dispatcher `proxy_jump` support (Teleport через bastion); push S4 (api/mcp фасад).

## Зафиксированные решения сессии
| Тема | Решение |
|---|---|
| env-RBAC окружения | гибрид C+D, колонка `incarnation.covens` (declared env-теги + name-as-coven) |
| Cloud | 6 провайдеров, credentials **Вариант A** (Keeper резолвит KV-секрет), bootstrap **cloud-init**, пилот **AWS** |
| drift-detection | имя **Scry**, **on-demand пилот → фон**, `Plan` pure-read, статус `drift` информационный, отчёт `DriftReport` |
| Shepherd (балансировка) | 💤 Ящик идей (возможно следующие релизы) |
| UI | **отдельный артефакт** (B), React+TS+Vite, адаптировать saltgui design-system, TS-клиент из OpenAPI, словарь Soul Stack; строим когда API почти готов |
| SSH/push | 3 провайдера (static/Vault SSH CA/Teleport), пилот **static-key** |
| SSH credentials-flow | **Variant B** для Vault SSH CA (плагин сам в Vault через `vault_access`; `ssh/sign` — операция, не KV-read; расходится с cloud-A сознательно) ([ADR-020 amendment (j)](adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)) |
| SSH key-ownership | **Keeper-ephemeral** для CA-провайдеров (Keeper генерит ephemeral keypair per-session, шлёт только pubkey; приватник не покидает Keeper; security-first) ([ADR-020 amendment (k)](adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)) |
| SSH params-delivery | env-convention per-plugin (`SOUL_SSH_STATIC_PARAMS` / `SOUL_SSH_VAULT_PARAMS` / `SOUL_SSH_TELEPORT_PARAMS`); generic-механизм отложен post-MVP ([ADR-020 amendment (l)](adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)) |
| state_history retention | **soft-delete / архив по конфигу** (не hard-delete); last 50 + всегда snapshot на version-bump |
| CLI | тонкий враппер поверх OpenAPI, позже |
| module-registry | обсудить; дефолт PG `bytea` |
| archive zip-slip | in-process распаковка с `securejoin` — **закрыто** (`soul/internal/coremod/archive/`) |
| recovery-сценарии | отложено до реального запроса |

---

## Track 0 — Release-ops (Т1, путь к первому прод-релизу)
- ✓ CI (GitHub Actions = `make check` + integration + govulncheck)
- ✓ push кода на GitHub
- ✓ Runbook: deploy · backup/restore PG · Vault-setup · scaling Keeper · upgrade-процедура
- ✓ govulncheck в `make check`
- ⬜ Branch-protection + required-check (оператор в GitHub UI) — на постоянном репо
- ⬜ **На постоянном репо:** финализация module-path (sed-rename) + реальный ключ подписи (сейчас cosign-stub)

## Track 1 — Эргономика операторов (Т2)
- ✓ MCP-tool `keeper.soul.coven-assign`
- ✓ env-RBAC C+D: `service=`/`coven=` в RBAC-context incarnation (OR-Check `RequirePermissionMulti`) + колонка `incarnation.covens` (миграция 046); REST↔MCP паритет (7 incarnation MCP-tools — scoped OR-Check, fail-closed) — закоммичено
- ✓ MCP-паритет incarnation-роутов (REST↔MCP scoped OR-Check)
- ✓ bulk-API + follow-ups (`replace`-набор + `incarnation=`-селектор)
- ⬜ Справочник coven-меток (реальный `CovenLabelValidator`) — закрывает Q1b
- ⬜ Operator CLI (тонкий враппер)
- ⬜ UI (отдельный SPA, saltgui-адаптация) — старт когда API почти готов

## Track 2 — HA / масштаб
- 💤 Shepherd — балансировка при scale-out ([ideas.md](ideas.md))
- ✓ **Батчевый прогон — Voyage** ([ADR-043](adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), `kind=scenario` — батч N инкарнаций по Leg-ам; `kind=command` — multi-target ad-hoc exec). Поглотил и заменил Tide ([ADR-040](adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) и ErrandRun ([ADR-041](adr/0041-errandrun.md#adr-041-errandrun--multi-target-обвязка-над-errand)) — обе сущности реализационно удалены (Wave 5, миграции 061/062), ADR-040/041 оставлены как superseded-история. Параметры: `batch_size` / `concurrency` / `schedule_at` / `inter_batch_interval` / `on_failure`; реестр `voyages` + `voyage_targets` (migration `059`); эндпоинт `/v1/voyages`; failover через Acolyte-style PG-lease.
- ⬜ advanced: per-task `serial:` (Q24) · per-host `RenderedTask` (Q25) · L7-LB (Q10) — backlog

## Track 3 — Cloud parity (Т2)
- ✓ **6 cloud-провайдеров реализованы.** AWS пилот (`ec83487`) + edge-hardening (`72ff14b`); GCP (`6bfd83c`), Yandex Cloud (`a181d49`), Azure (`c7d93ad`) — батч-1 hyperscaler-like; OpenStack (`a03a404`), Proxmox (`af43665`) — батч-2 расходящиеся (Keystone-auth / clone-model+composite vm_id). Shared SDK-каркас (`sdk/clouddriver/`) переиспользован всеми без правок. credentials-flow Variant A — сработал на всех 6. [ADR-017 amendment 2026-05-26](adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read).
- ⬜ reconcile-loop (Q17) — после parity
- ⬜ bootstrap-on-VM: cloud-init userdata основным путём (Q14)

## Track 4 — Операции / надёжность (Т2)
- ✓ **drift-detection (Scry):** Slice A (Plan pure-read + only-add proto + статус `drift` + `DriftReport` + on-demand-пилот) + Slice B (`scenario/converge/`, `keeper.incarnation.check-drift` REST+MCP, RBAC `incarnation.check-drift`, audit `incarnation.drift_checked`) — ✓ закоммичено. Plan-тираж pure-read на 14 остальных core-модулей (`c2d8181`): **9 модулей с `PlanReadSafe`** (group/user/cron/mount/sysctl/archive/line/repo/firewall) — ✓; **2 модуля БЕЗ `PlanReadSafe`** (git, url без checksum) — отложено в Slice C; **3 verb-модуля БЕЗ `PlanReadSafe`** (exec/cmd/http) — correct default-deny. Итого 12 stateful покрыты, 5 без покрытия (2 deferred, 3 verbs n/a). ✓ **Slice C** (background-scan, default OFF guard-rail — `reaper.scry_background.enabled=false`; Reaper-rule `scry_background` + миграция 050 колонки `last_drift_check_at`/`last_drift_summary`; throttle через `CountActiveDryRuns()` max_concurrent=10; audit-source `SourceBackground`) — реализован, [ADR-031 amendment 2026-05-26](adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile).
- 🟡 **keeper.push S1-S5 + 3 SshProvider:** **3 SshProvider реализованы** — static-key (`4f95ef6`, reference), Vault SSH CA (`3642520`, ephemeral keypair), Teleport (`af27678`, `SignReply.proxy_jump` only-add field 4). MVP-решения зафиксированы ([ADR-020 amendment 2026-05-26](adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)): credentials Variant B + key-ownership Keeper-ephemeral + params env-convention. ✓ **S1** доставка soul+модулей (SHA-256); ✓ **S2** dispatcher ephemeral-key; 🟡 **S3** alt-Outbound в scenario-runner / dispatcher `proxy_jump` support (Teleport-через-bastion — в работе); 🟡 **S4** OpenAPI/MCP/RBAC/audit фасад — Variant C orchestrator зафиксирован 2026-05-26 ([ADR-032](adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-без-incarnationscenario)); ✓ **S5** cleanup + live-sshd тест.
- ✓ state_history retention: soft-delete/архив по конфигу (Reaper-правило)
- ✓ **Recovery — `reclaim_apply_runs` runbook + GATE-1 production-gate (2026-05-26).** Операционализация GATE-1 ([ADR-027](adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) amend), не новый ADR. Runbook [`docs/operations/recovery-reclaim-apply-runs.md`](operations/recovery-reclaim-apply-runs.md) — три прод-гейта (fencing-Soul по флоту + `acolytes > 0` на всех Keeper-инстансах + Soul-reconcile S6 для `dispatched`-сирот), hot-reload `enabled: true`, валидация по `keeper_reaper_rule_purged_total{rule="reclaim_apply_runs"}` / `keeper_runresult_stale_total` / audit `reaper.reclaim_apply_runs.executed` / алерт `dispatched stuck`.
- ⬜ module-registry storage (Q5) — обсудить; PG `bytea` дефолт
- ⬜ **Toll (cluster-wide отток-detector)** — [ADR-038](adr/0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls) зафиксирован 2026-05-26, имплементация отдельным slice-ом (per-instance tollwatcher + Redis-leader агрегация + soft-degraded middleware на POST /v1/incarnations/{name}/scenarios/{scenario} и POST /v1/push/apply).
- ✓ **R2 anchors-TTL** — закрыт без действий 2026-05-26 (Outbound уже работает через SoulLease `soul:<sid>:lock` с TTL=30s + refresh=10s + правильным failover; фантом-запись).

## Track 5 — Security hardening (Т2)
- ✓ core.archive zip-slip → in-process `securejoin` (fail-fast по резолвнутому пути, symlink within-dest, hardlink/devnode reject, setuid/setgid/sticky mask, zip-bomb лимиты size/entries/ratio)
- ✓ soul-lint статпроверка `where:`/`on:`-литералов

---

## Канон-фиксация (закрытые батчи)
- **PM 2026-05-25:** ✓ UI (Q20 resolve) · ✓ env-RBAC (ADR-008 amend (a) → РЕАЛИЗОВАНО C+D) · ✓ drift ([ADR-031](adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile) + Scry/DriftReport в naming-rules + Q15) · ✓ cloud (ADR-017 amend + Q13/Q14) · ✓ state-retention (Q19) · ✓ open Q13/14/15/19/20 updates.
- **PM 2026-05-26:** ✓ ADR-020 amendment (SSH-провайдеры — 3 решения пользователя + MVP-набор) · ✓ ADR-017 amendment (cloud-парность DONE — 6 провайдеров) · ✓ ADR-031 amendment (Plan-тираж на 14 core-модулей) · ✓ naming-rules sync (`provider_kind` enum для cloud/ssh, 3 SshProvider-бинаря) · ✓ roadmap status update.
- **PM 2026-05-27 (Vigil S5 closure):** ✓ ADR-030 [amendment 2026-05-26](adr/0030-vigil-oracle.md#amendment-2026-05-26-s5-closure) — три open ends закрыты одной волной: V5-1 typed PortentPayload (6 typed-message + custom Struct, deprecation `PortentEvent.data` через 1-release hand-off → hard-cut S5-final), V5-2 `soul_beacon` plugin-kind (4-й kind plugin-инфры, `KIND_SOUL_BEACON=4`, unary `ValidateVigil`+`Check`, Sigil-verify обязателен), V5-3 `core.beacon.inotify` (7-й встроенный beacon, Linux-only, fold-adapter, `InotifyPortent` field 14, Darwin/Windows stub). ✓ naming-rules sync (`kind`-enum 4 значения, `PortentEvent.payload`-oneof, Vigil-описание — 7 core-beacon + `soul_beacon`-plugin), plugins.md cross-link, beacon README cross-link. Отложено пост-S5: (iv) per-Decree cooldown override · (v) toggle-endpoint re-enable Decree · (vi) metric-threshold-pull beacon (новый ADR) · L3b live-harness Vigil/Oracle (M2.5 harness-extension) · inotify `recursive`+`throttle` · `PortentEvent.data` hard-cut.

## Открытые решения (понадобятся до соответствующей фазы — НЕ блокируют текущую работу)
- **Multi-key RBAC-селекторы** (`coven=X,service=Y` AND): сейчас селектор single-key (`coven=` ИЛИ `service=`); комбинированный AND грамматикой не выражается (структура `Permission.Selector` AND умеет — не хватает грамматики разбора). Нужен parser-extension + отдельный ADR (ADR-008 amend (a) ограничение).
- **Destroy guard-rail cross-feature** (⚠ при ревью destroy-цепочки): cloud-leaf `core.cloud.provisioned` `destroyed` = немедленный TerminateInstances **без** orchestration-guard-rails. Убедиться, что он вызывается ТОЛЬКО после guard-rails `incarnation.destroy` (двухфазный `destroying`→`destroy_failed`, [статусы destroy](architecture.md#статусы-destroying-и-destroy_failed)) — иначе «опечатка в `count` стирает прод» (cloud-cascade ADR-017 Consequences стирает souls/seeds/tokens).
- **Drift Slice C** (фоновый периодический скан + полноценный lock-strategy с upgrade/destroy + drift для `core.git` / `core.url` без checksum): нужна архитектура (Reaper-подобный лидер vs отдельная корутина; конкурентность check-drift × apply/upgrade/destroy; drift-сигнатура для `git ls-remote` / `core.url` HEAD-режим) + решение. До решения — drift только on-demand-пилотом.
- **S3 push↔Acolyte:** push синхронен, Acolyte ([ADR-027](adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)) асинхронный `apply_runs`-барьер — стык может потребовать amend ADR-027 (architect пометил, проверяем при S3).
- **dispatcher `proxy_jump` support** (Teleport-через-bastion): `soul-ssh-teleport` возвращает `SignReply.proxy_jump`, dispatcher (`keeper/internal/push`) поле ИГНОРИРУЕТ — `net.Dial` идёт напрямую. Полный Teleport-флоу требует dispatcher proxy_jump support. В работе параллельно.
