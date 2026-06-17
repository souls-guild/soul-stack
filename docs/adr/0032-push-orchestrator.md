# ADR-032. Push-orchestrator (Variant C) — multi-host destiny push без incarnation/scenario.

**Контекст.** push-режим ([ADR-004](0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)) изначально описывал «доставить soul-бинарь + ad-hoc apply через SSH». MVP+ потребовал multi-host параллельную выкатку destiny на список SID без обвязки scenario/incarnation/state (ad-hoc операции, оператор-driven, не часть soul-lifecycle).

**Решение.**
1. **Endpoint `POST /v1/push/apply`** (см. [docs/keeper/operator-api.md:806-837](../keeper/operator-api.md)):
   - Body: `{inventory_sids[], destiny_ref ("<name>@<ref>"), ssh_provider, input, cleanup_stale}`.
   - Response: `202 + {apply_id}` (ULID).
   - Async-модель: best-effort параллельная выкатка, без cross-host barrier.
2. **Storage — отдельная таблица `push_runs`** (миграция 051, не пересекается с `apply_runs`):
   - Поля: `apply_id` PK, `inventory_sids` text[], `destiny_ref`, `ssh_provider`, `input` jsonb, `cleanup_stale` bool, `status`, `started_at`, `finished_at`, `started_by_aid` FK→operators, `started_by_kid`, `summary` jsonb.
   - Партиальные индексы WHERE status IN ('pending','running').
3. **Статусы push_runs.status:** `pending` | `running` | `succeeded` | `partial_failed` | `failed`.
   - `partial_failed` — часть хостов прошла, часть упала; вид «успех» по факту, оператор сам решает retry на упавшие.
4. **RBAC permissions (новые):** `push.apply` (запуск), `push.read` (чтение `GET /v1/push/{apply_id}`).
5. **Render-context для push:**
   - Доступно: `input.*`, `soulprint.self.*`, `vault(...)`.
   - Недоступно: `register.*`, `incarnation.state.*`, `essence.*`, `soulprint.hosts` — fail-render с явной ошибкой (push не имеет scenario/incarnation/cross-host runtime).
   - Механизм: синтетический one-task scenario + `destinyIsolated=true` в render.Pipeline (см. architect-вердикт push S4).
6. **Recovery — best-effort через статус + Reaper.**
   - При рестарте Keeper-а pending/running push-прогоны → orphans.
   - Reaper-rule `purge_orphan_push_runs` (TTL по умолчанию 1h): orphan → status=`failed`, summary={orphan_purged: true}.
   - Без сложной cross-keeper lease-модели (push — oneshot, проще fail-fast).
7. **Audit-events:** `push.applied` (start), `push.completed`/`push.failed`/`push.partial_failed` (finish). См. [naming-rules.md](../naming-rules.md#audit-events).
8. **Симметрия с pull.** Аналог `POST /v1/souls/{sid}/exec` для pull-ad-hoc (одиночные core-команды через Soul-стрим) — отдельная сущность Errand, ADR-033 в работе.

**Инварианты.**
- Push НЕ читает/мутирует `incarnation.state` (нет incarnation в контуре).
- Render-pipeline переиспользуется через синтетический scenario; **не** вводится `RenderDestinyStandalone` (drift-риск).
- `DestinyResolver` для push отдельный (`pushorch.pushDestinyResolver`), не пересекается с scenario-side `DestinySource` (тот привязан к service.yml).
- `topology.LoadByInventory(sids)` — новая only-add точка, без incarnation-фазы и declared-role.

**Отвергнутые альтернативы.**
- (а) Reuse `apply_runs` для push — отвергнуто, lifecycle апликации scenario и push разнятся (нет state_changes, нет attempt-fencing, нет barrier-а).
- (б) `RenderDestinyStandalone` как отдельный публичный API в render/ — отвергнуто (drift-риск, две точки входа в CEL+template).
- (в) Push через Acolyte work-queue — отложено (вне scope MVP); push — oneshot, work-queue избыточен.

**Что отложено.**
- Token-renewal Vault при долгих push-прогонах (>100 хостов): отдельный slice.
- Кэш destiny-tree между прогонами: `DestinyLoader` уже несёт snapshot-кэш; если упрётся в git-clone latency — отдельный кейс.
- Cancel push (по `apply_id`): не входит в MVP, отдельный slice.

**Amendment (2026-05-26, S6 pilot wire-up SshDispatcher).** ADR-032 целится в production-форму push-orchestrator-а с PG-table push_providers, souls.ssh_target jsonb и push.host_ca_refs[] (multi-CA, multi-provider routing) — это S7, отдельный slice. До закрытия S7 «production» включается **pilot-формой** через 3 inline-поля в `keeper.yml::push:`:

- **`push.targets[]`** — per-SID SSH-реквизиты (host=SID, ssh_port/ssh_user/soul_path с дефолтами 22/root/`/usr/local/bin/soul`). Без миграции `souls.ssh_target jsonb` — pilot. SID без записи → `ErrTargetNotConfigured` на резолве в SshDispatcher (fail-closed).
- **`push.providers[]`** — per-provider `params` (opaque-форма), сериализуется в JSON и инжектится в env `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` плагина при spawn (ADR-020 amendment l). Без PG-table `push_providers` — pilot.
- **`push.host_ca_ref`** — single Vault-ref на PEM public host-CA (поле `public_key`). Single-CA (multi-CA `host_ca_refs[]` — S7). Plaintext-inline-PEM отвергнут (`vault_ref_invalid` в schema-фазе).

`setupPushDispatchers` (`keeper/cmd/keeper/daemon.go`) поднимает single-provider single-CA SshDispatcher на первом дискаверенном SshProvider-плагине; multi-provider routing по `push_runs.ssh_provider` отложен на S7. Spawn делается один раз при старте, plugin-handle закрывается в LIFO ДО Redis/Pool. Нормативная грамматика блока — [docs/keeper/config.md → push](../keeper/config.md#push); описание wire-up — [docs/keeper/push.md → Wire-up SshDispatcher в daemon](../keeper/push.md#wire-up-sshdispatcher-в-daemon-s6-pilot-2026-05-26).

Pilot **сознательно** не вводит новые сущности (двойной реестр `push.ssh_providers[]`, `private_key_vault_path`, multi-CA list, multi-provider routing) — это полностью покрывается S7-каноном и не упрощается долгосрочно. До S7 pilot — единственный канон; после S7 inline-формы `push.targets[]` / `push.providers[]` отвергаются как `unknown_key` (deprecation окно фиксируется в S7 amendment).

**Amendment (2026-05-26, S7-1 wire-up `souls.ssh_target jsonb` как PG-canon).** Первый slice S7: per-host SSH-реквизиты переезжают из inline-`push.targets[]` в реестр `souls`. Изменения:

- **Колонка `souls.ssh_target jsonb`** (миграция 053) + CHECK `souls_ssh_target_shape` (typed shape: integer/text/text). NULL-семантика поля целиком означает «target не настроен».
- **Operator API:** `PUT /v1/souls/{sid}/ssh-target` с body `{ssh_port, ssh_user, soul_path}`. Permission `soul.ssh-target-update` (с селектором `host=<sid>`), audit `soul.ssh-target.updated`. MCP-tool: `keeper.soul.ssh-target.update` (3-сегментный паттерн `keeper.sigil.key.<verb>` ↔ permission `sigil.key-<verb>`).
- **Резолвер push-flow:** `PGFallbackTargetResolver` (PG-first; `souls.ssh_target` → дефолты port=22/user=root/soul-path=/usr/local/bin/soul на опущенных полях). PG-row.ssh_target IS NULL → проверка флага `push.allow_legacy_push_targets` (default `false`): false → `ErrTargetNotConfigured`; true → одноразовый WARN deprecation + fallback на `ConfigTargetResolver` поверх `push.targets[]`.
- **PM-decisions S7-1:** (1) push-providers/conduit как «SSH Provider» variant of Provider (S7-2, не эта slice); (2) deprecation policy `keeper.yml::push.targets[]` — 1 release WARN → hard-cut; (3) audit-event имя `soul.ssh-target.updated`; (4) permission `soul.ssh-target-update`; (5) JSON-shape типизированный, валидируется CHECK; (6) **нет auto-import** keeper.yml targets[] в PG в S7-1 (отложено в S7-4, требует explicit consent + idempotency); (7) priority PG > keeper.yml (DB — source of truth).
- **Что НЕ в S7-1:** push_providers PG-table (S7-2), multi-CA `push.host_ca_refs[]` (S7-3), auto-import legacy targets (S7-4).

**Amendment (2026-05-26, S7-2 wire-up `push_providers` PG-table как PG-canon).** Второй slice S7: per-provider env-payload params SSH-плагинов переезжают из inline-`push.providers[]` (pilot S6 / S7-1) в PG-таблицу. Изменения:

- **Таблица `push_providers`** (миграция 054): PK `name TEXT` (regex `^[a-z][a-z0-9-]{0,62}$` — env-var-name-safe для трансляции в `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS`), `params JSONB`, `created_by_aid` NOT NULL REFERENCES operators, `updated_at`/`updated_by_aid` для аудита триажа.
- **Operator API:** 5 CRUD-эндпоинтов `POST/GET/PUT/DELETE /v1/push-providers[/{name}]` + `GET /v1/push-providers` (list). 5 permissions `push-provider.{create,update,delete,list,read}`. Audit-events `push-provider.created` / `.updated` / `.deleted` (payload `{name, params_keys}` без values — фиксирует мутацию без раскрытия секретов). MCP-tools `keeper.push-provider.{create,update,delete,list,read}` (5 новых tools, total catalog 53→58).
- **Sensitive params (PM-decision S7-2 #5):** ключи `secret_id` / `token` / `password` / `private_key` ОБЯЗАНЫ быть vault-refs (`vault:<path>`) — валидация на service-слое (`pushprovider.Service.validateSensitive`), 422 при plaintext.
- **Резолвер push-flow:** `PGFallbackProviderResolver` (`keeper/internal/push/provider_pg.go`) на старте `setupPushDispatchers` резолвит env-payload params для дискаверенного SSH-плагина: PG-first → fallback `keeper.yml::push.providers[]` под флагом `push.allow_legacy_push_providers` (default false; one-shot WARN deprecation). При отсутствии записи в PG и выключенном legacy — плагин стартует без env-payload (как pilot S6).
- **Hot-reload (PM-decision S7-2 #6):** spawn-on-change через Redis pub/sub `push-providers:changed` (`keeper/internal/redis/pushproviderchanged.go`); REST/MCP-мутация публикует, каждая нода подписана через `SubscribePushProvidersChanged`. Subscription-инфраструктура поднята; фактический re-spawn SSH-плагина с новым env-payload — отдельный slice follow-up (текущая S7-2-реализация только логирует приходящие сообщения).
- **PM-decisions S7-2:** (1) Extend Provider entity (Cloud Provider + SSH Provider — два домена одной сущности; разные таблицы и permission-области); (2) audit-event naming `push-provider.created/updated/deleted` (kebab single-section); (3) Redis topic `push-providers:changed`; (4) deprecation 1 release WARN → hard-cut; (5) sensitive params как vault-refs; (6) hot-reload = spawn-on-change.
- **Что НЕ в S7-2:** auto-import legacy `keeper.yml::push.providers[]` в PG (S7-4), per-host invalidation (multi-provider routing slice), capabilities manifest `sensitive_keys[]` плагина (пост-MVP).

**Amendment (2026-05-26, S7-3 multi-CA `push.host_ca_refs[]`).** Третий slice S7: single Vault-ref `push.host_ca_ref` → массив структур `push.host_ca_refs[{ref, name}]` для verify host-keys через SSH. Изменения:

- **Грамматика:** `push.host_ca_refs[]` (`KeeperPushCARef = {ref: vault-ref, name: kebab-case}`). Каждый `ref` — vault-ref-only (plaintext-PEM запрещён, симметрия с singular); `name` — operator-defined, используется как label-значение в `keeper_push_host_ca_used_total{ca_name=...}` и в diag-сообщениях. Имена в наборе обязаны быть уникальны.
- **Backward-compat singular:** `push.host_ca_ref` остаётся под 1-release WARN deprecation window (как S7-1/S7-2). При заполненном singular и пустом `host_ca_refs[]` daemon auto-adapt-ит singular в singleton `HostCARefs[0]` c auto-name `default` и пишет одноразовый WARN. Одновременное присутствие singular + plural отвергается schema-фазой (`mutually_exclusive_keys`).
- **Verify logic:** `ssh.CertChecker.IsHostAuthority` делает OR-проверку по всем загруженным CA (закрывается `hostCertCallback(cas, onMatch)` в [`keeper/internal/push/session.go`](../../keeper/internal/push/session.go)). Host-cert, подписанный любым CA из набора → доверенный; иначе reject (отказ от TOFU). При матче callback `onMatch(caName)` инкрементирует `keeper_push_host_ca_used_total{ca_name=...}` + debug-лог.
- **Per-provider CA-override — ОТЛОЖЕНО.** В MVP all-providers-trust-all-CAs: один набор `host_ca_refs[]` действует на все SSH-handshake-и (target + proxy_jump). Отдельный CA на provider/route — пост-MVP open Q после S7.
- **PM-decisions S7-3:** (1) формат — массив структур (не плоский массив строк) для извлечения `name` под label-значение; (2) backward-compat singular auto-adapt в singleton + WARN; (3) mutually exclusive singular+plural; (4) verify через `CertChecker.IsHostAuthority` OR-loop; (5) per-provider CA-override отложен (all-trust-all в MVP); (6) deprecation policy — 1 release WARN (как S7-1/S7-2).
- **Что НЕ в S7-3:** per-provider CA-override / per-route CA (отложено, open Q post-S7), auto-import legacy targets (S7-4).

**Amendment (2026-05-26, S7-4 auto-import legacy on start + deprecation timeline).** Финальный slice S7: закрывает migration loop — pilot и canon coexist под прежними флагами, а явный opt-in переносит inline-блоки в PG-источники одним проходом на старте. Изменения:

- **Грамматика:** два новых поля в `keeper.yml::push:`:
  - `push.auto_import_legacy_targets: false` (default) — opt-in one-shot миграция `push.targets[]` → `souls.ssh_target` jsonb;
  - `push.auto_import_legacy_providers: false` (default) — аналогично для `push.providers[]` → PG-таблица `push_providers`.
- **One-shot семантика.** [push.AutoImporter] (`keeper/internal/push/auto_import.go`) запускается шагом `runLegacyAutoImport` в pipeline после `setupPushProviderSvc` и ДО `setupAPIServer`. Идемпотентно: уже импортированные SID-ы / provider-имена пропускаются (PG canonical-источник не перезаписывается), повторный старт без новых записей в `keeper.yml` — no-op. Отсутствующая `souls`-row для config-target SID — WARN-skip (не fatal: soul ещё не зарегистрирован, импорт реквизита бессмыслен; оператор позже PUT-нёт через `/v1/souls/{sid}/ssh-target`). PG read/write fail — `errSetupFailed` (оператор включил флаг, не должен молча получить «половина импортирована»).
- **Audit-events** (новая `source` enum: `config_bootstrap`, system-action, `archon_aid: NULL`):
  - `soul.ssh-target.imported_from_config` — per-row, payload `{sid, ssh_port, ssh_user, soul_path}`;
  - `push-provider.imported_from_config` — per-row, payload `{name, params_keys}` (значения sensitive ключей в audit НЕ пишутся, симметрия с `push-provider.created`).
- **System-AID `archon-system`.** Импортированные `push_providers`-строки несут `created_by_aid='archon-system'` (отделить от Архонт-create-ов). FK на `operators(aid)` обязывает row `archon-system` существовать до первого auto-import-а; миграция reserved-AID — отдельный slice (pilot предполагает оператора, добавляющего её руками либо bootstrap-семантика следующего slice-а).
- **Deprecation timeline (финал):**
  - **S7 wave (текущий commit):** pilot + canon coexist. PG-first резолверы (`PGFallbackTargetResolver` / `PGFallbackProviderResolver`); yml fallback под `push.allow_legacy_push_targets` / `push.allow_legacy_push_providers` (default false); auto-import под `push.auto_import_legacy_targets` / `push.auto_import_legacy_providers` (default false). 1-release WARN при использовании любого legacy-fallback / при auto-adapt singular host_ca_ref.
  - **S8 (после следующего prod-release):** hard-cut. `keeper.yml::push.targets[]` / `push.providers[]` / singular `host_ca_ref` → `unknown_key` на schema-фазе. `allow_legacy_push_*` / `auto_import_legacy_*` флаги удаляются. PG-резолверы упрощаются до PG-only (без `Fallback`/`AllowLegacy` полей).
- **Operator-driven migration path (если auto-import не подходит):** `soulctl souls ssh-target set <sid> --port … --user … --soul-path …` (S7-1) и `soulctl push-providers create <name> --params=…` (S7-2). Оба пути идемпотентны и аудируются (`soul.ssh-target.updated` / `push-provider.created`).
- **PM-decisions S7-4:** (1) auto-import — opt-in (default false, explicit operator-consent); (2) one-shot семантика + idempotent (ON CONFLICT / PG-row presence check); (3) audit-events `soul.ssh-target.imported_from_config` + `push-provider.imported_from_config`; (4) новая `source` enum `config_bootstrap` (отделить от `keeper_internal` semantically); (5) deprecation 1-release WARN → S8 hard-cut; (6) system-AID `archon-system` для импортированных строк.
- **Что НЕ в S7-4:** S8 hard-cut (отдельный slice после prod-release); миграция reserved-AID `archon-system` (отдельный slice). TODO `push-providers:changed` re-spawn SshDispatcher plugin-handle — закрыт ADR-032 amendment 2026-05-27 ниже (S7-2 closure).

**Amendment (2026-05-27, S7-2 closure: runtime re-spawn SshDispatcher plugin-handle).** Закрывает TODO от S7-2 «фактический re-spawn SSH-плагина с новым env-payload». S7-2 поднимала subscription-инфраструктуру + логировала приходящие `push-providers:changed`-сообщения; этот amendment подключает фактический re-spawn. Изменения:

- **`SshDispatcher.RefreshProvider(ctx, providerName)`** ([`keeper/internal/push/dispatcher.go`](../../keeper/internal/push/dispatcher.go)) — public-метод, под `d.mu.Lock` подменяющий внутренний `Deps.Provider`/`Deps.ProviderCloser` на свежий plugin-handle. Сверка имени: single-provider pilot хранит ровно одного провайдера (`Deps.ProviderName`), сообщения про чужой плагин игнорируются (no-op без ошибки); пустое имя (массовая инвалидация) трактуется как «свой». Sentinel `push.ErrRespawnNotSupported` — когда `Deps.Respawner == nil` (unit-тесты / single-instance dev без CRUD-инвалидации).
- **`push.ProviderRespawner`** — узкий интерфейс `RespawnProvider(ctx, name, oldCloser) (SshProvider, io.Closer, error)`. Реализация `pushProviderRespawner` ([`keeper/cmd/keeper/push_respawn.go`](../../keeper/cmd/keeper/push_respawn.go)) хранит `*pluginhost.Host` + `[]pluginhost.Discovered` + `*push.PGFallbackProviderResolver`: находит discovered по имени, закрывает старый handle, резолвит свежие params (PG-first → legacy-fallback), spawn-ит новый с `WithEnv(SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS)`, оборачивает в `pluginhost.SshProviderPlugin`. push-пакет независим от pluginhost/PG — поверхность respawner-а узкая.
- **Daemon-listener** (`runPushProviderInvalidationListener`, [`keeper/cmd/keeper/daemon.go`](../../keeper/cmd/keeper/daemon.go)) — заменена TODO-логирующая реализация на реальный вызов `pushSshDispatcher.RefreshProvider`. Ошибка `ErrRespawnNotSupported` логируется как WARN (не наш плагин / нет respawner-а); реальный spawn-fail — ERROR + dispatcher переходит в degraded state до следующей мутации/рестарта. Listener не падает на ошибке — продолжает читать канал.
- **Concurrency-инвариант:** RefreshProvider держит `sync.RWMutex` Lock на всём re-spawn-цикле; SendApply/Cleanup на горячем пути берут snapshot ссылки через `provider()` под RLock и доезжают до конца сессии без блокировки. Старый handle жив до момента, когда respawner вызывает его Close (синхронно внутри RefreshProvider); идущий прогон сам держит gRPC-conn открытым, `BasePlugin.Close` идемпотентен.
- **Тесты:** unit `keeper/internal/push/refresh_test.go` (happy-path / wrong-name no-op / empty-name mass-invalidate / spawn-fail degraded state / mutex concurrency); integration `keeper/internal/push/refresh_integration_test.go` (под `-tags=integration`, miniredis-фикстура): publish → subscription → listener → RefreshProvider → respawner round-trip.
- **PM-decisions amendment 2026-05-27:** (1) shape pilot оставлен — `SshDispatcher` хранит ровно ОДНОГО провайдера (single-provider pilot), не map[name]; multi-provider routing — отдельный slice пост-S7, расширение без breaking change через `map[string]providerEntry`; (2) Respawner — отдельный интерфейс в push-пакете, конкретная реализация в `keeper/cmd/keeper/` (где живут зависимости pluginhost+resolver); (3) listener не падает на ошибках — best-effort с структурированным логом; (4) RWMutex (Read для горячего пути SendApply, Write только на re-spawn) — чтобы re-spawn не задерживал идущие прогоны.
- **Что НЕ в этом amendment-е:** multi-provider routing (несколько SshDispatcher-ов на одной ноде, каждый под свой `pluginName`); per-host invalidation (текущий канал — кластеро-wide); graceful drain old plugin-handle до Close (старые in-flight RPC доезжают через свой snapshot, явного await-а нет — pluginhost-side conn закрывается при Close, незавершённые RPC получат cancelled, но это не наш hot path — SendApply/Cleanup RPC к плагину короткие).

**Amendment (2026-05-27, P2 Multi-provider routing).** Закрывает «multi-provider routing» из known-gap S7-2 closure: keeper держит ОДНОВРЕМЕННО несколько SshProvider-плагинов и per-SID маршрутизирует SID-ы между ними. Изменения:

- **Dispatcher single → map.** `push.Deps.Provider/ProviderName/ProviderCloser/Respawner` (single-provider pilot) удалены; вместо них `Deps.Providers map[string]ProviderEntry` (`ProviderEntry = {Provider, Closer}`). `SendApply`/`Cleanup` получают `providerName` от caller-а и делают lookup в карте под RLock; неизвестное имя → `push.ErrProviderUnknown`. `SshDispatcher.RefreshProvider(name)` подменяет одну запись под Lock без эффекта на остальные; `name=""` (mass invalidate) → re-spawn всех. Старые тесты single-provider формы переписаны на map-based wire-up через helper `testProviderName + map[string]ProviderEntry{...}`.
- **Selector R1 (3-tier resolve)** в [`keeper/internal/push/router.go::PGRouter`](../../keeper/internal/push/router.go) (`ProviderRouter` interface, `RouteFor(ctx, sid) (name, source, err)`). Уровни:
  - **Level 1:** `souls.ssh_target.ssh_provider` — optional поле в shape-CHECK `souls_ssh_target_shape` (миграция 056 расширяет 053 регексом `^[a-z][a-z0-9-]{0,62}$` для optional `ssh_provider`). Source `soul`.
  - **Level 2:** `push.coven_default_providers: { <coven>: <provider_name> }` — карта в keeper.yml. Tiebreak при множественном coven-match Soul-а — **алфавитный порядок ковенов** (детерминизм). Source `coven`.
  - **Level 3:** `push.cluster_default_provider` — cluster fallback. Source `cluster`.
  - Все три пусты → `ErrProviderNotRouted` → fail per-host (status=`error`, error_code=`provider_not_routed`). **БЕЗ provider-chain fallback** (security-инвариант: auth-perimeter разных providers разный, silent fallback ломает trust).
- **α-compat** REST/MCP-`POST /v1/push/apply::ssh_provider`: при непустом поле — per-job preset применяется ко ВСЕМ SID-ам, router НЕ вызывается. Source трактуется как `soul`. При пустом — router-резолв per-SID.
- **Eager spawn.** `setupPushDispatchers` spawn-ит ВСЕ дискаверенные SshProvider-плагины при старте Keeper-а (UX-предсказуемость + plugin-start cost — единовременный). Spawn-fail любого плагина → `errSetupFailed` (оператор явно объявил его в каталоге).
- **Audit/Metrics.** Routing-decision per-SID **НЕ** пишется отдельным audit-event-ом (избыточный шум). Реальный SshProvider сохраняется в `push_runs.summary.hosts[sid].ssh_provider`. Counter `keeper_push_provider_routed_total{provider, decision_source}` (low cardinality ~N\_providers × 3) — на каждом успешном RouteFor + на α-compat preset-пути.
- **Hot-reload** Level 2/3: `RouterConfigSource.Snapshot()` тянет свежий `*KeeperConfig` через `config.Store.Get()` на каждый RouteFor (атомарный read, no PGRouter rebuild).
- **Operator surface:**
  - Per-SID: extended `PUT /v1/souls/{sid}/ssh-target` body + `keeper.soul.ssh-target.update` MCP-tool + `soulctl souls ssh-target set --ssh-provider=<name>`.
  - Bulk per-coven: `soulctl souls ssh-target bulk-set --coven=<name> --ssh-provider=<name>` (client-side fan-out поверх list+PUT; server-side bulk-route не вводится).
  - Cluster / per-coven default: правка `keeper.yml::push.{coven_default_providers,cluster_default_provider}` + SIGHUP / API-reload.
- **PM-decisions amendment 2026-05-27 (P2):** (1) selector R1 — 3-tier (per-SID → per-coven → cluster), без β/γ-вариантов в MVP; (2) fail-per-host БЕЗ provider-chain (security-first); (3) audit-summary-only (без отдельного per-routing event-а); (4) eager spawn (не lazy); (5) α-compat preset перебивает routing; (6) bulk операторская команда — client-side fan-out поверх list+PUT.
- **Что НЕ в P2:** soul-label-selector `souls.attributes` (новая сущность; propose-and-wait, отложено); per-job inline `routing_rules` (γ-variant per-prog override карты; post-MVP); provider-chain fallback; lazy spawn; soul-level per-incarnation override.
