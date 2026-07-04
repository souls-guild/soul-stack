// Package api — HTTP-фасад Operator API Keeper-а
// (M0.6a: framework + auth + health/meta; M0.6b/c добавит endpoints).
//
// Изолирует choice роутера (chi) от остального keeper-кода: внешний код
// зависит только от типа [Server] и [Deps].
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/keeper/internal/toll"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// Deps — внешние зависимости HTTP-сервера. M0.6b добавляет JWTIssuer
// (выпуск токенов через `POST /v1/operators` и `issue-token`),
// AuditWriter (запись events после прошедшей RBAC-проверку операции),
// RBAC (permission-check middleware + handler-side ClusterAdmins/RolesOf),
// OperatorPool (handler-доступ к реестру с BeginTx для self-lockout-tx)
// и TTLDefault (TTL JWT, выпускаемых API).
//
// RBAC передаётся как [RBACProvider] — общая поверхность для
// [rbac.Enforcer] и [rbac.Holder]; production wire-up в `keeper run` даёт
// Holder, который перевывает Enforcer на каждый Reload-swap Store-а
// (hot-reload `rbac:`-блока, ADR-021 + docs/keeper/config.md).
//
// Incarnation/Soul/Push/Cloud-handlers — M0.6c+. M0.6c-1: IncarnationDB
// добавлен; scenario-execution / migrate-executor — M0.6c-2/3.
type Deps struct {
	JWTVerifier *jwt.Verifier
	JWTIssuer   handlers.JWTIssuer
	PGPinger    health.Pinger
	RedisPinger health.Pinger
	VaultPinger health.Pinger
	AuditWriter audit.Writer
	RBAC        RBACProvider

	// RBACSvc — бизнес-логика RBAC-CRUD (роли / permissions / membership)
	// для будущих role.*-эндпоинтов (Slice 2a). ОТЛИЧАЕТСЯ от RBAC выше:
	// тот — read-only enforcer-поверхность (Check / ClusterAdmins / RolesOf
	// для middleware и operator-handler-а), этот — мутирующий CRUD-фасад
	// ([rbac.Service]). При nil role.*-роуты просто не подключаются (Slice
	// 1.5 поле прокидывает, роуты регистрирует Slice 2a).
	RBACSvc *rbac.Service

	// SigilSvc — бизнес-логика Sigil allow-list-а (plugin.allow/revoke/list,
	// ADR-026 S4a). При nil plugin.*-роуты не подключаются (production-wire-up
	// в `keeper run` передаёт *sigil.Service поверх Signer+Store+кеша host-а;
	// unit/integration-тесты без Sigil остаются валидными). Симметрично
	// [RBACSvc].
	SigilSvc *sigil.Service

	// SigilKeySvc — бизнес-логика ротации trust-anchor-ключей подписи Sigil
	// (sigil.key-introduce/retire/list/set-primary, ADR-026(h) R3-S7). При nil
	// sigil/keys-роуты не подключаются (production-wire-up при включённом Sigil
	// передаёт *sigil.KeyService). Симметрично [SigilSvc].
	SigilKeySvc *sigil.KeyService

	// ServiceSvc — бизнес-логика реестра Service-ов (service.register/update/
	// list/deregister, ADR-028-паттерн RBAC-storage). При nil service.*-роуты
	// не подключаются (production-wire-up в `keeper run` передаёт тот же
	// *serviceregistry.Service, что несёт S2-invalidate-хук; unit/integration-
	// тесты без реестра остаются валидными). Симметрично [RBACSvc] / [SigilSvc].
	ServiceSvc *serviceregistry.Service

	// ServiceRefs — TTL-кеш git-ls-remote-листинга tag/branch для
	// `GET /v1/services/{name}/refs` (UI Upgrade-modal dropdown). Опционален:
	// при nil /refs-эндпоинт отвечает 500 (фича не сконфигурирована); сам
	// service-CRUD остаётся работоспособным. Production-wire-up в `keeper run`
	// передаёт *serviceregistry.RefsCache поверх artifact.RefsListerFunc(
	// artifact.ListRefs).
	ServiceRefs handlers.ServiceRefsLister

	// ServiceScenarios — TTL-кеш listing-а scenario из материализованного
	// снапшота git-репо Service-а для `GET /v1/services/{name}/scenarios` (UI
	// Run-modal dropdown). Опционален: при nil /scenarios-эндпоинт отвечает 500
	// (фича не сконфигурирована); сам service-CRUD остаётся работоспособным.
	// Production-wire-up в `keeper run` передаёт *serviceregistry.ScenariosCache
	// поверх ScenarioListerFunc, разрешающего `(name,gitURL,ref)` через
	// *artifact.ServiceLoader.Load → artifact.ListScenarios.
	ServiceScenarios handlers.ServiceScenarioLister

	// ServiceStateSchema — TTL-кеш listing-а state_schema-метаданных
	// (`state_schema_version` + опц. декларация структуры state + цепочка
	// миграций) из материализованного снапшота git-репо Service-а для
	// `GET /v1/services/{name}/state-schema` (UI Schema explorer).
	// Опционален: при nil /state-schema-эндпоинт отвечает 500 (фича не
	// сконфигурирована); сам service-CRUD остаётся работоспособным.
	// Production-wire-up в `keeper run` передаёт *serviceregistry.StateSchemaCache
	// поверх StateSchemaListerFunc, разрешающего `(name,gitURL,ref)` через
	// *artifact.ServiceLoader.Load → artifact.ListStateSchema.
	ServiceStateSchema handlers.ServiceStateSchemaLister

	// ServiceDependencies — TTL-кеш listing-а git-зависимостей (destiny/modules
	// из `service.yml`) для `GET /v1/services/{name}/dependencies` (UI Service
	// Detail). Опционален: при nil /dependencies-эндпоинт отвечает 500 (фича не
	// сконфигурирована); сам service-CRUD остаётся работоспособным.
	// Production-wire-up в `keeper run` передаёт *serviceregistry.DependenciesCache
	// поверх DependenciesListerFunc, разрешающего `(name,gitURL,ref)` через
	// *artifact.ServiceLoader.Load → artifact.ListDependencies.
	ServiceDependencies handlers.ServiceDependenciesLister

	// AugurSvc — management-логика реестра Augur (omen.create/list/delete +
	// rite.create/list/delete, ADR-025). При nil augur.*-роуты не подключаются
	// (production-wire-up в `keeper run` передаёт тот же *augur.Service, что MCP).
	// Симметрично [ServiceSvc]. НЕ путать с Augur-брокером (resolve/broker) —
	// тот живёт в EventStream-пути, не в Operator API.
	AugurSvc *augur.Service

	// OracleSvc — management-логика реестров Oracle (vigil.create/list/delete +
	// decree.create/list/delete, ADR-030 beacons). При nil vigil.*/decree.*-роуты
	// не подключаются (production-wire-up в `keeper run` передаёт тот же
	// *oracle.Service, что MCP). Симметрично [AugurSvc]. НЕ путать с
	// reactor-роутером (match/enqueue) — тот живёт в EventStream-пути.
	OracleSvc *oracle.Service

	OperatorDB    handlers.OperatorPool
	IncarnationDB handlers.IncarnationDB
	SoulDB        handlers.SoulPool
	TTLDefault    time.Duration

	// ChoirDB — CRUD-поверхность реестра Choir/Voice (ADR-044, S-T3). При nil
	// choir.*-роуты не подключаются (паттерн PushProviderSvc).
	// Production-wire-up в `keeper run` передаёт тот же *pgxpool.Pool, что и
	// IncarnationDB (Choir-таблицы лежат в той же БД).
	ChoirDB handlers.ChoirDB

	// SoulPresence — lease-overlay presence для `GET /v1/souls` (ADR-006(a)):
	// поле `status` ответа List/Get деривируется из живого Redis SID-lease, а не
	// отдаётся как лениво-сверяемый PG-снимок `souls.status` (иначе
	// переподключившийся Soul висит `disconnected` до следующего тика Reaper-а).
	// Опционален: при nil overlay выключен (single-instance dev / unit без Redis),
	// отдаётся PG-снимок. Production-wire-up в `keeper run` передаёт обёртку над
	// тем же Redis-клиентом, что и topology-резолверу (keeperredis.SoulsStreamAlive).
	SoulPresence handlers.SoulPresence

	// SoulStatsStaleFn — провайдер порога «протухшего» last_seen_at для
	// stale_count в `GET /v1/souls/stats` (тот же mark_disconnected.stale_after
	// Reaper-а). Функция читает СВЕЖИЙ конфиг (hot-reload) на каждом запросе,
	// симметрично TempoVoyageCreateLimits. nil → регистрация подставляет дефолт
	// (90s, parity reaper.defaultMarkDisconnectedStale) — валидно для unit-тестов.
	SoulStatsStaleFn func() time.Duration

	// ClusterRegistry / ClusterLeaderReader / SelfKID — зависимости `GET /v1/cluster`
	// (HA-топология из Conclave + Reaper-лидер). Опциональны: при nil ClusterRegistry
	// роут не монтируется (single-Keeper dev без Redis — cluster-view не нужен).
	// Production-wire-up в `keeper run` передаёт обёртки над тем же Redis-клиентом,
	// что у Conclave-renewal (keeperredis.LiveKIDs / ReadInstanceMeta /
	// PeekLeaseHolder(reaper.LeaderLeaseKey)); SelfKID = soulstack.kid из конфига.
	ClusterRegistry     handlers.ClusterRegistry
	ClusterLeaderReader handlers.ClusterLeaderReader
	SelfKID             string

	// ScenarioRunner / ServiceRegistry — опциональны: при обоих non-nil
	// `POST /v1/incarnations` запускает scenario `create` (production);
	// при nil — Create остаётся stub-ом (insert row, без apply).
	ScenarioRunner  handlers.ScenarioStarter
	ServiceRegistry handlers.ServiceResolver

	// ScenarioDestroyer — опционален: нужен `DELETE /v1/incarnations/{name}`
	// (async-teardown scenario `destroy` в TerminalDestroy, S-D2b). Отдельное
	// поле от ScenarioRunner — узкий интерфейс [handlers.DestroyStarter]
	// (StartDestroy), хотя production-wire-up передаёт тот же *scenario.Runner.
	// При nil Destroy отвечает 500 (endpoint не сконфигурирован).
	ScenarioDestroyer handlers.DestroyStarter

	// ScenarioDrift — опционален: нужен `POST /v1/incarnations/{name}/check-drift`
	// (Scry on-demand-пилот, ADR-031). Узкий интерфейс [handlers.DriftChecker]
	// (CheckDrift + MarkDriftStatus), production-wire-up передаёт тот же
	// *scenario.Runner. При nil check-drift отвечает 500.
	ScenarioDrift handlers.DriftChecker

	// ServiceLoader — опционален: нужен `POST /v1/incarnations/{name}/upgrade`
	// (материализация снапшота целевого service-ref-а + сборка
	// migration-chain). При nil Upgrade отвечает 500. Production-wire-up
	// передаёт *artifact.ServiceLoader.
	ServiceLoader handlers.ServiceSnapshotLoader

	// PushRun — multi-host push-orchestrator (Variant C, ADR-004 push-flow +
	// docs/keeper/push.md). При nil push.*-роуты не подключаются (паттерн
	// SigilSvc/AugurSvc/OracleSvc): keeper стартует без SSH-плагинов, и
	// `POST /v1/push/apply` / `GET /v1/push/{apply_id}` остаются 404.
	// Production-wire-up в daemon собирает PushRun через setupPushOrchestrator
	// (после поднятия SshDispatcher из push S1+S5).
	PushRun *pushorch.PushRun

	// PushProviderSvc — бизнес-логика CRUD реестра Push-Provider-ов
	// (push-provider.create/update/delete/list/read, ADR-032 amendment 2026-05-26,
	// S7-2). При nil push-provider.*-роуты не подключаются (паттерн ServiceSvc/
	// AugurSvc). Production-wire-up в `keeper run` передаёт *pushprovider.Service
	// поверх pgxpool.Pool + Redis-publisher (push-providers:changed).
	PushProviderSvc *pushprovider.Service

	// HeraldSvc — бизнес-логика CRUD реестров Herald (каналы) / Tiding (правила)
	// уведомлений (herald.*/tiding.*, ADR-052, S4). При nil herald.*/tiding.*-
	// роуты не подключаются (паттерн PushProviderSvc/AugurSvc). Production-wire-up
	// в `keeper run` передаёт *herald.Service поверх pgxpool.Pool + dispatcher-
	// инвалидатор + Redis-publisher (herald:invalidate).
	HeraldSvc *herald.Service

	// ProviderSvc / ProfileSvc — operator-facing CRUD реестров Cloud-Provider-ов
	// (`providers`) и Cloud-Profile-ей (`profiles`, ADR-017, docs/keeper/cloud.md).
	// При nil соответствующие provider.*/profile.*-роуты не подключаются (паттерн
	// PushProviderSvc/AugurSvc). credentials_ref отдаётся как vault-путь, секрет
	// не резолвится. БЕЗ Redis-publisher: Cloud-Provider/Profile читаются on-demand
	// на scenario-слое (`core.cloud.provisioned`), не hot-reload-ятся.
	ProviderSvc *provider.Service
	ProfileSvc  *profile.Service

	// ErrandDispatcher / ErrandStore — pull-ad-hoc Errand contour (ADR-033).
	// При nil обоих errand.*-роуты не подключаются (паттерн PushRun). Wire-up
	// — setupErrandDispatcher (после setupGRPCEventStream: Outbound нужен
	// dispatcher-у). dispatcher и store создаются вместе из одного PG-pool-а
	// и единого ApplyBus-а, поэтому передаются обе ссылки одной зоной (а не
	// один общий Service-объект): Dispatcher несёт write-path (Insert+Mark+
	// audit), Store читает (Get/List). Симметрично RBACProvider/CovenScoper
	// для SoulHandler (две роли вокруг одного pool-а).
	ErrandDispatcher *errand.Dispatcher
	ErrandStore      *errand.Store

	// VoyageDB / VoyageScenarioResolver / VoyageCommandResolver — Voyage contour
	// (ADR-043, S5): unified батчевый прогон (kind=scenario|command). При nil
	// VoyageDB voyage.*-роуты не подключаются (паттерн ErrandRunStore).
	// VoyageDB — тот же *pgxpool.Pool, что несёт IncarnationDB (таблицы
	// voyages/voyage_targets в той же БД). Резолверы:
	//   - VoyageScenarioResolver → имена инкарнаций (production: NewVoyage
	//     ScenarioPGResolver(d.pool));
	//   - VoyageCommandResolver → SID-snapshot (production: NewVoyageCommandPG
	//     Resolver(d.pool)).
	// При nil любого резолвера — соответствующая kind-ветка create отвечает 500.
	VoyageDB               handlers.VoyageStore
	VoyageScenarioResolver handlers.VoyageScenarioResolver
	VoyageCommandResolver  handlers.VoyageCommandResolver
	// VoyageMaxScope — верхний лимит размера резолвнутого scope одного Voyage
	// (DoS-guard S-med-3). 0 → безлимит. Источник — cfg.Voyage.ResolvedMaxScope().
	VoyageMaxScope int
	// VoyageMaxBatchSize — верхний предел размера батча/окна одного Voyage
	// (DoS-guard S-W4). 0 → без предела. Источник — cfg.Voyage.ResolvedMaxBatchSize().
	VoyageMaxBatchSize int

	// CadenceDB — CRUD-поверхность реестра Cadence-расписаний (`cadences`,
	// ADR-046, S4). При nil cadence.*-роуты не подключаются (паттерн VoyageDB).
	// Тот же *pgxpool.Pool, что несёт VoyageDB/IncarnationDB (таблица cadences и
	// back-link voyages.cadence_id в той же БД). Двухуровневый RBAC-by-kind
	// (ADR-046 §7) использует тот же enforcer (RBAC).
	CadenceDB handlers.CadenceStore

	// CadencePollFloorSeconds — нижний предел периода interval-Cadence (floor-лимит,
	// ADR-046 Pass B): create/update с `interval_seconds < floor` → 422. ЕДИНЫЙ
	// источник с адаптивным опросом Conductor — `cfg.CadenceScheduler.ResolvedPollFloor()`
	// (не хардкод 30 в двух местах). 0 → floor-проверка выключена (dev/тест).
	CadencePollFloorSeconds int

	// AuditReader — read-side `audit_log` для `GET /v1/audit` (UI iteration 2).
	// При nil audit-роут не подключается (паттерн PushRun/Errand). Production-
	// wire-up передаёт *auditpg.NewReader(pgPool) — тот же pool, что несёт
	// auditWriter (writer + reader живут поверх одной таблицы, разделены
	// только direction-ом для type-safety).
	AuditReader *auditpg.Reader

	// MetricsHTTP — keeper_http_*-инструментация `/v1/*` (registered
	// поверх *obs.Registry через [obs.RegisterHTTPMetrics]). При nil
	// HTTP-метрики не собираются (см. router.go) — допустимо в unit-тестах.
	//
	// Сам `/metrics`-эндпоинт здесь НЕ обслуживается: он вынесен на
	// выделенный listener (`listen.metrics.addr`, ADR-024) в
	// keeper/cmd/keeper; openapi-роутер только инструментирует /v1/*.
	MetricsHTTP *obs.HTTPMetrics

	// ModuleCatalogPlugins — поверхность чтения активных plugin-допусков для
	// module-catalog (`GET /v1/modules`, UI Run→Command module-search).
	// Опционален: при nil каталог отдаёт только core-модули (статическая doc-
	// таблица всегда доступна), plugin-секция пуста. Production-wire-up в
	// `keeper run` передаёт адаптер поверх sigil-store (ListActive → ManifestRaw).
	// Сам `/v1/modules`-роут подключается ВСЕГДА (core-каталог не требует
	// внешних зависимостей), в отличие от opt-in plugin.*-роутов.
	ModuleCatalogPlugins handlers.ModuleCatalogPlugins
	// ModuleFormPrepH — резолвер source-каталогов UI-формы модуля (ADR-045 S3).
	// Pre-built в daemon-е поверх pgxpool (паттерн VoyageH); nil → роут
	// /v1/modules/{name}/form-prep не монтируется (drift-test держит allowlist).
	ModuleFormPrepH *handlers.ModuleFormPrepHandler

	// TollDegraded — Toll cluster-detector read-флаг (ADR-038). Middleware на
	// blocked-routes (POST scenarios/run, POST push/apply) на каждом запросе
	// проверяет его через IsDegraded и блокирует с 503 + Retry-After при
	// взведённом флаге. При nil — middleware не навешивается (single-instance/
	// dev без Redis: блокировка не нужна, флаг никем не выставляется).
	TollDegraded toll.DegradedReader

	// TempoLimiter — Tempo per-AID rate-limiter (ADR-050) для resolver-тяжёлого
	// `POST /v1/voyages`. При nil (нет Redis / Tempo disabled) middleware
	// passthrough — точечная навеска на роут просто пропускает запросы. Production
	// wire-up передаёт *redis.TokenBucket поверх живого Redis-клиента.
	TempoLimiter apimiddleware.RateLimiter

	// TempoMetrics — keeper_tempo_*-counters (ADR-050(g)). nil-safe (nil →
	// emit no-op). Production wire-up передаёт *TempoMetrics из metrics-registry.
	TempoMetrics apimiddleware.RateLimitMetrics

	// TempoVoyageCreateLimits — провайдер живых rate/burst bucket-а voyage-create
	// (hot-reload, ADR-050(f)/ADR-021): читает config.Store snapshot на каждом
	// запросе. nil → дефолты [config.DefaultTempoVoyageCreate*] (резолвится при
	// сборке роутера). Используется лишь при non-nil TempoLimiter.
	TempoVoyageCreateLimits func() apimiddleware.RateLimitLimits

	// TempoVoyagePreviewLimits — провайдер живых rate/burst bucket-а voyage-preview
	// (hot-reload, ADR-050(f)/ADR-021 + amendment 2026-06-17 — отдельный bucket).
	// Читает config.Store snapshot на каждом запросе. nil → дефолты
	// [config.DefaultTempoVoyagePreview*] (резолвится при сборке роутера).
	// Используется лишь при non-nil TempoLimiter.
	TempoVoyagePreviewLimits func() apimiddleware.RateLimitLimits

	// WebUIEnabled — резолвнутый тоггл встроенного UI на маршруте `/ui`
	// (ADR-055): true → статика go:embed монтируется (публично, ВНЕ /v1, parity
	// /docs); false → /ui не подключается. Резолвится daemon-ом из
	// [config.KeeperConfig.WebUIMounted] (default-ON: nil-config → true).
	// Zero-value (false) у вызывающих, не выставивших поле (unit-тесты без UI),
	// — осознанный «не монтировать»: /ui для них не нужен, mount требует embed-
	// дерева. Внешнего бэкенда тоггл не требует (UI вшит в бинарь).
	WebUIEnabled bool

	// LDAPAuth — федеративная LDAP-аутентификация операторов (ADR-058,
	// POST /auth/ldap/login). При nil endpoint не монтируется (opt-in-домен,
	// паттерн pushH/errandH): keeper.yml::auth.ldap не задан → способ логина
	// недоступен, Keeper стартует (ADR-053 OPTIONAL-tier). daemon собирает поле
	// при наличии auth.ldap (резолв bind_password_ref/ca_ref из Vault).
	LDAPAuth *LDAPAuthDeps

	// OIDCAuth — федеративная OIDC-аутентификация операторов (ADR-058 стадия 2,
	// GET /auth/oidc/{login,callback}). При nil эндпоинты не монтируются (opt-in,
	// как LDAPAuth): keeper.yml::auth.oidc не задан → способ недоступен, Keeper
	// стартует (ADR-053 OPTIONAL-tier). daemon собирает поле при наличии auth.oidc
	// И живого Redis (flow-state store cluster-shared): без Redis OIDC недоступен.
	OIDCAuth *OIDCAuthDeps

	// LoginGuard — anti-bruteforce-примитив публичных login-эндпоинтов (ADR-058(g),
	// HIGH-3): per-IP+per-username throttle + lockout. Реализуется *redis.LoginGuard.
	// nil (нет Redis) → login-эндпоинты без throttle (passthrough, как Tempo при
	// nil-limiter). daemon собирает при живом Redis. Используется только при
	// смонтированных /auth-роутах (non-nil LDAPAuth/OIDCAuth).
	LoginGuard apimiddleware.LoginGuard

	// LoginLimitCfg — статические параметры anti-bruteforce-лимита (резолв из
	// config.KeeperAuth.ResolvedLoginRateLimit()). Читается один раз на сборке
	// middleware (login редок, не hot-path).
	LoginLimitCfg apimiddleware.AuthLoginLimitConfig

	// ProvisioningPolicyReader — read-снимок политики provisioning_allowed_methods
	// для GET /v1/provisioning-policy (ADR-058 Часть B). Реализуется
	// *serviceregistry.Holder (cluster-консистентный atomic-снимок). PUT пишет через
	// [ServiceSvc].SetSetting. provisioning-policy-роуты монтируются только при
	// non-nil ProvisioningPolicyReader И non-nil ServiceSvc (нужны оба: чтение +
	// запись). nil → группа не подключается (unit-тесты без serviceregistry).
	ProvisioningPolicyReader handlers.ProvisioningPolicyReader
}

// RBACProvider — общая поверхность rbac-сервиса, нужная и middleware-у
// (Check / HoldsAction) и handler-у (ClusterAdmins / RolesOf). Реализуют
// [rbac.Enforcer] (static snapshot, для unit-тестов) и [rbac.Holder]
// (hot-reload-aware wrapper над [config.Store], production).
//
// ActionHolder (ADR-047 §г G1) — existence-gate read-эндпоинтов
// ([apimiddleware.RequireAction]): read-souls-роуты гейтятся «держит ли оператор
// soul.list В ПРИНЦИПЕ», сужение по scope делает handler после фетча строк.
type RBACProvider interface {
	apimiddleware.PermissionChecker
	apimiddleware.ActionHolder
	handlers.RBACSource
	handlers.PurviewResolver
	handlers.PermissionsLister
}

// Server — обёртка над http.Server с pre-computed listener-ом и логгером.
// Конструктор не привязывается к порту до Start, чтобы NewServer был
// дёшев и не мог завершиться race-условием bind-а.
//
// Поле addr защищено mu — Start обновляет его actual-адресом (важно
// при `:0` для тестов), Addr() читает; без mu Go-race-detector ловит
// write-vs-read goroutine boundary.
type Server struct {
	srv        *http.Server
	configAddr string

	// operatorHandler удерживается ссылкой, чтобы caller (keeper/cmd/keeper)
	// мог получить inner [operator.Service] через [Server.OperatorService]
	// и переиспользовать его в MCP-listener-е — один источник правды для
	// бизнес-логики Operator-CRUD (M0.7, PM-decision delegation.md #6).
	operatorHandler *handlers.OperatorHandler

	mu     sync.Mutex
	addr   string
	logger *slog.Logger
}

// OperatorService возвращает [operator.Service], инкапсулированный в
// inner OperatorHandler-е сервера. Используется wire-up-ом MCP listener-а
// в keeper/cmd/keeper для переиспользования single-source-of-truth-логики
// (delegation.md PM-decision #6). Возвращает nil только если NewServer
// не успел/не смог построить handler — production-path всегда non-nil.
func (s *Server) OperatorService() *operator.Service {
	if s.operatorHandler == nil {
		return nil
	}
	return s.operatorHandler.Service()
}

// maxHeaderBytes — лимит на размер HTTP-headers (request-line + headers).
// stdlib default — 1 MiB; для Operator API такого размера headers не
// бывает (Bearer JWT ~1 KiB), 16 KiB закрывает DoS-вектор «огромные
// headers».
const maxHeaderBytes = 16 * 1024

// v1RequestBodyLimit — лимит на body запросов под `/v1/*`. Operator-endpoints
// MVP принимают компактный JSON (POST /v1/operators ~200 байт, revoke ~80,
// issue-token — пустой body); 1 MiB закрывает DoS «многогигабайтный
// payload» с большим запасом для будущих incarnation-endpoint-ов
// (essence-yaml в spec.fragments + module-list). Превышение → MaxBytesError
// при Decode → 400 problem+json (TypeMalformedRequest).
const v1RequestBodyLimit = 1 << 20

// maxBodyMiddleware оборачивает Request.Body в [http.MaxBytesReader],
// ограничивая объём считываемых байт. Применяется на /v1/* (см. router.go).
// http.Server сам body не лимитирует — нужно явно (anti-DoS, см. RFC 9110
// §10.2).
func maxBodyMiddleware(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// NewServer собирает HTTP-сервер. Возвращает error на invalid-cfg
// (пустой addr) или nil deps (JWT verifier обязателен — без него
// /v1/* теряет аутентификацию, что нарушает требование RFC 7807-фасада).
//
// http.Server timeouts:
//
//   - ReadHeaderTimeout=5s — защита от Slowloris;
//   - ReadTimeout=30s — для POST body (incarnation create / migration);
//   - WriteTimeout=60s — list endpoints могут возвращать сотни записей;
//   - IdleTimeout=120s — keep-alive (matches default LB defaults);
//   - MaxHeaderBytes=16 KiB — anti-DoS на больших headers.
//
// Эти значения — стартовые; конфиг-driven tuning — M0.6+ (отдельный
// блок keeper.yml::listen.openapi.timeouts).
//
// Лимит на body — [v1RequestBodyLimit], применяется через
// [maxBodyMiddleware] на `/v1/*` (см. router.go).
func NewServer(cfg config.KeeperListenSimple, deps Deps, logger *slog.Logger) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("api: listen.openapi.addr is empty")
	}
	if deps.JWTVerifier == nil {
		return nil, errors.New("api: JWTVerifier is required")
	}
	if logger == nil {
		return nil, errors.New("api: logger is required")
	}
	if deps.RBAC == nil {
		return nil, errors.New("api: RBAC enforcer is required")
	}
	if deps.AuditWriter == nil {
		return nil, errors.New("api: AuditWriter is required")
	}
	if deps.OperatorDB == nil {
		return nil, errors.New("api: OperatorDB is required")
	}
	if deps.IncarnationDB == nil {
		return nil, errors.New("api: IncarnationDB is required")
	}
	if deps.SoulDB == nil {
		return nil, errors.New("api: SoulDB is required")
	}
	if deps.JWTIssuer == nil {
		return nil, errors.New("api: JWTIssuer is required")
	}
	if deps.TTLDefault <= 0 {
		return nil, errors.New("api: TTLDefault must be positive")
	}

	healthH := health.NewHandler(health.Deps{
		PG:    deps.PGPinger,
		Redis: deps.RedisPinger,
		Vault: deps.VaultPinger,
	})
	opH := handlers.NewOperatorHandler(deps.OperatorDB, deps.JWTIssuer, deps.RBAC, deps.TTLDefault, logger)
	// Гейт политики provisioning_allowed_methods на POST /v1/operators (ADR-058
	// Часть B): тот же снимок политики (Holder), что и для GET-эндпоинта.
	// ProvisioningPolicyReader (Holder) реализует и узкий gate-интерфейс
	// (ProvisioningMethodAllowed) — выставляем через type-assert. nil-reader / не-
	// Holder → gate не выставлен (back-compat: CreateTyped пропускает).
	if gate, ok := deps.ProvisioningPolicyReader.(handlers.ProvisioningGate); ok && gate != nil {
		opH.SetProvisioningGate(gate)
	}
	incH := handlers.NewIncarnationHandler(deps.IncarnationDB, deps.ScenarioRunner, deps.ScenarioDestroyer, deps.ScenarioDrift, deps.ServiceRegistry, deps.ServiceLoader, deps.AuditWriter, deps.RBAC, logger)
	// refs-lister (тот же ls-remote-кеш, что у ServiceHandler) для дешёвого режима
	// GET .../upgrade-paths (ADR-0068 §6); late-binding, конструктор не расширяем.
	incH.SetServiceRefs(deps.ServiceRefs)
	soulH := handlers.NewSoulHandler(deps.SoulDB, deps.RBAC, deps.SoulPresence, logger)

	// clusterH опционален: при nil ClusterRegistry `GET /v1/cluster` не монтируется
	// (single-Keeper dev без Redis — cluster-view не нужен). self_health использует
	// те же PG/Redis/Vault-pingers, что `/readyz` (health.Check — единый источник).
	var clusterH *handlers.ClusterHandler
	if deps.ClusterRegistry != nil {
		clusterH = handlers.NewClusterHandler(
			deps.ClusterRegistry, deps.ClusterLeaderReader,
			health.Deps{PG: deps.PGPinger, Redis: deps.RedisPinger, Vault: deps.VaultPinger},
			deps.SelfKID, logger)
	}

	// roleH опционален: при nil RBACSvc role.*-роуты не подключаются (Slice
	// 1.5 прокидывает поле, production-wire-up в `keeper run` передаёт
	// *rbac.Service). NewServer не требует RBACSvc — unit/integration-тесты
	// без RBAC-CRUD остаются валидными.
	var roleH *handlers.RoleHandler
	if deps.RBACSvc != nil {
		roleH = handlers.NewRoleHandler(deps.RBACSvc, logger)
	}

	// synodH опционален: при nil RBACSvc synod.*-роуты не подключаются (ADR-049).
	// Тот же *rbac.Service, что у roleH (Synod-методы — на нём).
	var synodH *handlers.SynodHandler
	if deps.RBACSvc != nil {
		synodH = handlers.NewSynodHandler(deps.RBACSvc, logger)
	}

	// sigilH опционален: при nil SigilSvc plugin.*-роуты не подключаются
	// (production-wire-up в `keeper run` передаёт *sigil.Service). Симметрично
	// roleH.
	var sigilH *handlers.SigilHandler
	if deps.SigilSvc != nil {
		sigilH = handlers.NewSigilHandler(deps.SigilSvc, logger)
	}

	// sigilKeyH опционален: при nil SigilKeySvc sigil/keys-роуты не подключаются
	// (production-wire-up при включённом Sigil передаёт *sigil.KeyService).
	// Симметрично sigilH.
	var sigilKeyH *handlers.SigilKeyHandler
	if deps.SigilKeySvc != nil {
		sigilKeyH = handlers.NewSigilKeyHandler(deps.SigilKeySvc, logger)
	}

	// serviceH опционален: при nil ServiceSvc service.*-роуты не подключаются
	// (production-wire-up в `keeper run` передаёт *serviceregistry.Service).
	// Симметрично roleH / sigilH.
	var serviceH *handlers.ServiceHandler
	if deps.ServiceSvc != nil {
		serviceH = handlers.NewServiceHandler(deps.ServiceSvc, deps.ServiceRefs, deps.ServiceScenarios, deps.ServiceStateSchema, deps.ServiceDependencies, logger)
	}

	// provisioningPolicyH опционален: GET читает снимок политики (Holder), PUT
	// пишет её через тот же ServiceSvc.SetSetting (+ cluster-invalidate). Нужны
	// оба — при nil любого provisioning-policy-роуты не монтируются (ADR-058 Часть B).
	var provisioningPolicyH *handlers.ProvisioningPolicyHandler
	if deps.ProvisioningPolicyReader != nil && deps.ServiceSvc != nil {
		provisioningPolicyH = handlers.NewProvisioningPolicyHandler(deps.ProvisioningPolicyReader, deps.ServiceSvc, logger)
	}

	// augurH опционален: при nil AugurSvc augur.*-роуты не подключаются
	// (production-wire-up в `keeper run` передаёт *augur.Service). Симметрично
	// serviceH.
	var augurH *handlers.AugurHandler
	if deps.AugurSvc != nil {
		augurH = handlers.NewAugurHandler(deps.AugurSvc, logger)
	}

	// oracleH опционален: при nil OracleSvc vigil.*/decree.*-роуты не
	// подключаются (production-wire-up передаёт *oracle.Service). Симметрично
	// augurH.
	var oracleH *handlers.OracleHandler
	if deps.OracleSvc != nil {
		oracleH = handlers.NewOracleHandler(deps.OracleSvc, logger)
	}

	// pushH опционален: при nil PushRun push.*-роуты не подключаются
	// (production-wire-up в `keeper run` передаёт *pushorch.PushRun, если
	// SshDispatcher сконфигурирован — см. setupPushOrchestrator). Симметрично
	// oracleH/augurH.
	var pushH *handlers.PushHandler
	if deps.PushRun != nil {
		pushH = handlers.NewPushHandler(deps.PushRun, logger)
	}

	// errandH опционален: при nil ErrandDispatcher / ErrandStore errand.*-роуты
	// не подключаются (паттерн pushH/sigilH). Production-wire-up передаёт обе
	// ссылки одной волной из setupErrandDispatcher. Конструктор тонкий: nil-
	// dispatcher/store зануляются в handler-е, роутер при nil errandH в обход
	// regist-ит routing-блок целиком (router.go).
	var errandH *handlers.ErrandHandler
	if deps.ErrandDispatcher != nil && deps.ErrandStore != nil {
		errandH = handlers.NewErrandHandler(deps.ErrandDispatcher, deps.ErrandStore, logger)
	}

	// auditH опционален: при nil AuditReader audit-роут не подключается (паттерн
	// errandH/pushH). Production-wire-up передаёт *auditpg.NewReader(pgPool) —
	// тот же pool, что несёт auditWriter.
	var auditH *handlers.AuditHandler
	if deps.AuditReader != nil {
		auditH = handlers.NewAuditHandler(deps.AuditReader, logger)
	}

	// pushProviderH опционален: при nil PushProviderSvc push-provider.*-роуты не
	// подключаются (паттерн serviceH/augurH/oracleH).
	var pushProviderH *handlers.PushProviderHandler
	if deps.PushProviderSvc != nil {
		pushProviderH = handlers.NewPushProviderHandler(deps.PushProviderSvc, logger)
	}

	// heraldH опционален: при nil HeraldSvc herald.*/tiding.*-роуты не подключаются
	// (паттерн pushProviderH). Один handler обслуживает оба реестра (Herald + Tiding).
	var heraldH *handlers.HeraldHandler
	if deps.HeraldSvc != nil {
		heraldH = handlers.NewHeraldHandler(deps.HeraldSvc, logger)
	}

	// providerH / profileH опциональны: при nil соответствующего Svc provider.*/
	// profile.*-роуты не подключаются (паттерн pushProviderH). Cloud-CRUD (ADR-017).
	var providerH *handlers.ProviderHandler
	if deps.ProviderSvc != nil {
		providerH = handlers.NewProviderHandler(deps.ProviderSvc, logger)
	}
	var profileH *handlers.ProfileHandler
	if deps.ProfileSvc != nil {
		profileH = handlers.NewProfileHandler(deps.ProfileSvc, logger)
	}

	// moduleCatalogH монтируется ВСЕГДА: core-каталог (`GET /v1/modules`) не
	// требует внешних зависимостей (статическая doc-таблица). ModuleCatalogPlugins
	// опционален — при nil plugin-секция каталога пуста.
	moduleCatalogH := handlers.NewModuleCatalogHandler(deps.ModuleCatalogPlugins, logger)

	// permCatalogH монтируется ВСЕГДА: каталог RBAC-permissions (`GET /v1/permissions`)
	// — статика из пакета rbac, без внешних зависимостей. Auth-only (без
	// RequirePermission, см. router.go).
	permCatalogH := handlers.NewPermissionCatalogHandler(logger)

	// eventTypeCatalogH монтируется ВСЕГДА: каталог event-types для подписки Tiding
	// (`GET /v1/event-types`, ADR-052(b)) — статика из пакета herald (источник правды
	// тот же, что валидирует CRUD). Auth-only (без RequirePermission, см. router.go).
	eventTypeCatalogH := handlers.NewEventTypeCatalogHandler(logger)

	// heraldTypeCatalogH монтируется ВСЕГДА: каталог типов Herald-канала и их
	// config-полей (`GET /v1/herald-types`, ADR-052 amendment) — статика из пакета
	// herald (источник тот же, что валидирует CRUD). Auth-only (без RequirePermission).
	heraldTypeCatalogH := handlers.NewHeraldTypeCatalogHandler(logger)

	// meH монтируется ВСЕГДА: эффективные права текущего Архонта
	// (`GET /v1/me/permissions`) резолвятся из RBAC-снимка (deps.RBAC non-nil
	// гарантирован выше). Auth-only (без RequirePermission, см. router.go).
	meH := handlers.NewMyPermissionsHandler(deps.RBAC, logger)

	// choirH опционален: при nil ChoirDB choir.*-роуты не подключаются (паттерн
	// pushProviderH). AuditWriter — тот же, что у incarnation-handler-а
	// (handler-side mutating-events choir.created/deleted/voice_*).
	var choirH *handlers.ChoirHandler
	if deps.ChoirDB != nil {
		choirH = handlers.NewChoirHandler(deps.ChoirDB, deps.AuditWriter, logger)
	}

	// voyageH опционален: при nil VoyageDB voyage.*-роуты не подключаются (паттерн
	// errandRunH). enforcer (deps.RBAC) — для in-handler RBAC-by-kind
	// guard-а (ADR-043 §6); IncarnationDB — per-incarnation scope-check scenario-
	// create-а. Резолверы — scenario→имена инкарнаций, command→SID-snapshot.
	var voyageH *handlers.VoyageHandler
	if deps.VoyageDB != nil {
		voyageH = handlers.NewVoyageHandler(
			deps.VoyageDB,
			deps.VoyageScenarioResolver,
			deps.VoyageCommandResolver,
			deps.IncarnationDB,
			deps.RBAC,
			// scoper: target ∩ Purview command-пути (ADR-047 S4). FOOTGUN: scoper
			// ОБЯЗАН быть non-nil в проде — при nil command-путь падает в cluster-
			// wide резолв (silent scope-leak: scoped-Архонт запустит command на
			// чужом coven). Зануление этого аргумента ловят e2e в
			// voyage_scope_integration_test.go (#2/#4/#6 станут красными).
			deps.RBAC,
			deps.AuditWriter,
			// tidingInvalidator: тот же *herald.Service (single source of truth),
			// что REST/MCP используют для CRUD Herald/Tiding. После commit
			// voyage-tx с ephemeral-notify сбрасывает TTL-снимок dispatcher-а
			// (ADR-052(g) race-fix). nil (dev без herald) → no-op, деградация на
			// TTL-сходимость.
			deps.HeraldSvc,
			deps.VoyageMaxScope,
			deps.VoyageMaxBatchSize,
			logger,
		)
	}

	// cadenceH опционален: при nil CadenceDB cadence.*-роуты не подключаются
	// (паттерн voyageH). enforcer (deps.RBAC) — для двухуровневого RBAC-by-kind
	// guard-а (ADR-046 §7); scenarioResolver/IncarnationDB — per-target coven-
	// scope-check рецепта kind=scenario (те же экземпляры, что у voyageH —
	// security-паритет create/patch Cadence ↔ create Voyage); AuditWriter —
	// handler-side mutating-events cadence.created/updated/deleted.
	var cadenceH *handlers.CadenceHandler
	if deps.CadenceDB != nil {
		// tidingInvalidator: тот же *herald.Service, что REST/MCP используют для CRUD
		// Herald/Tiding — после tx-создания Cadence с notify-правилами сбрасывает
		// TTL-снимок dispatcher-а (ADR-052 §m, parity voyageH). nil (dev без herald)
		// → no-op, деградация на TTL-сходимость.
		cadenceH = handlers.NewCadenceHandler(deps.CadenceDB, deps.VoyageScenarioResolver, deps.IncarnationDB, deps.RBAC, deps.AuditWriter, deps.HeraldSvc, deps.CadencePollFloorSeconds, logger)
	}

	// Tempo voyage-create/preview limits-провайдеры: при nil от вызывающего
	// (unit-тесты) деградируем к дефолтам config — middleware читает их на каждом
	// запросе. preview — отдельный bucket с собственными дефолтами (ADR-050
	// amendment 2026-06-17).
	tempoVoyageCreateLimits := deps.TempoVoyageCreateLimits
	if tempoVoyageCreateLimits == nil {
		tempoVoyageCreateLimits = func() apimiddleware.RateLimitLimits {
			rate, burst := config.DefaultTempoVoyageCreateRate, config.DefaultTempoVoyageCreateBurst
			return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
		}
	}
	tempoVoyagePreviewLimits := deps.TempoVoyagePreviewLimits
	if tempoVoyagePreviewLimits == nil {
		tempoVoyagePreviewLimits = func() apimiddleware.RateLimitLimits {
			rate, burst := config.DefaultTempoVoyagePreviewRate, config.DefaultTempoVoyagePreviewBurst
			return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
		}
	}

	handler := buildRouter(deps.JWTVerifier, healthH, opH, incH, soulH, roleH, synodH, sigilH, sigilKeyH, serviceH, provisioningPolicyH, augurH, oracleH, pushH, pushProviderH, providerH, profileH, errandH, voyageH, cadenceH, auditH, choirH, heraldH, moduleCatalogH, deps.ModuleFormPrepH, permCatalogH, eventTypeCatalogH, heraldTypeCatalogH, meH, deps.RBAC, deps.AuditWriter, deps.MetricsHTTP, deps.TollDegraded, deps.TempoLimiter, deps.TempoMetrics, tempoVoyageCreateLimits, tempoVoyagePreviewLimits, deps.WebUIEnabled, deps.LDAPAuth, deps.OIDCAuth, deps.LoginGuard, deps.LoginLimitCfg, deps.SoulStatsStaleFn, clusterH, logger)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	return &Server{
		srv:             srv,
		configAddr:      cfg.Addr,
		operatorHandler: opH,
		addr:            cfg.Addr,
		logger:          logger,
	}, nil
}

// Start привязывается к addr, начинает обслуживать запросы, и
// блокируется до тех пор, пока ctx не отменится или Serve не вернёт
// fatal-error. При cancel ctx делает graceful shutdown через [Shutdown].
//
// Используется паттерн «listen first, serve second»: net.Listen
// синхронный — если порт занят, ошибка возвращается до того, как
// goroutine стартанёт. Это даёт caller-у понятный fail-fast при
// конфликте портов.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.configAddr)
	if err != nil {
		return fmt.Errorf("api: listen %q: %w", s.configAddr, err)
	}
	// Фактический адрес может отличаться от requested (например, при
	// `:0` ядро выдаёт ephemeral port — это нужно для integration-тестов).
	actual := ln.Addr().String()
	s.mu.Lock()
	s.addr = actual
	s.mu.Unlock()
	s.logger.Info("operator API listening", slog.String("addr", actual))

	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("operator API received shutdown signal")
		shutErr := s.Shutdown(context.Background())
		// После Shutdown Serve-goroutine завершается (вернёт
		// ErrServerClosed). Дожидаем её и логируем нестандартный exit
		// (panic-recovery / accept-loop crash) отдельным WARN-ом,
		// чтобы такой случай не растворился в логах.
		select {
		case serveErr := <-errCh:
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				s.logger.Warn("operator API Serve returned non-ErrServerClosed after shutdown",
					slog.Any("error", serveErr),
				)
			}
		case <-time.After(2 * time.Second):
			s.logger.Warn("operator API Serve did not exit within 2s after shutdown — leak suspected")
		}
		return shutErr
	case err := <-errCh:
		return err
	}
}

// Addr возвращает фактический адрес, на котором слушает сервер. До
// первого вызова Start возвращает значение из cfg (как есть), после —
// resolved-адрес (для `:0` — конкретный port).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Shutdown инициирует graceful-stop с 10s grace-периодом. Каллер обычно
// не вызывает это напрямую — Start делает Shutdown сам при ctx.Done().
func (s *Server) Shutdown(ctx context.Context) error {
	shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("api: shutdown: %w", err)
	}
	s.logger.Info("operator API stopped")
	return nil
}
