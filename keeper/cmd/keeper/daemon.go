package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/souls-guild/soul-stack/keeper/internal/acolyte"
	"github.com/souls-guild/soul-stack/keeper/internal/api"
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	keeperaugur "github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/cloudinit"
	"github.com/souls-guild/soul-stack/keeper/internal/conductor"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	coremodchoir "github.com/souls-guild/soul-stack/keeper/internal/coremod/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	coremodsoul "github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	keepergrpc "github.com/souls-guild/soul-stack/keeper/internal/grpc"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/mcp"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	keeperpg "github.com/souls-guild/soul-stack/keeper/internal/pg"
	"github.com/souls-guild/soul-stack/keeper/internal/plugingit"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/keeper/internal/toll"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/internal/voyageorch"
	"github.com/souls-guild/soul-stack/keeper/internal/watchman"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	shlog "github.com/souls-guild/soul-stack/shared/log"
	"github.com/souls-guild/soul-stack/shared/obs"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// errSetupFailed — sentinel-ошибка шага setupX: означает «stderr уже
// напечатан внутри шага, оркестратору осталось только выйти с exitError».
// Каждый setupX печатает осмысленное сообщение сам (как делал прежний
// runDaemon перед `return exitError`), поэтому оркестратор НЕ печатает
// ошибку повторно — лишь маппит её в exit-code.
var errSetupFailed = errors.New("keeper run: setup step failed")

// cleanupStack — LIFO-стек cleanup-функций daemon-а. Заменяет россыпь
// `defer`-ов внутри прежнего монолитного runDaemon: каждый setupX-метод
// регистрирует свой teardown через push (а НЕ через свой defer — тот сработал
// бы при выходе из setupX, а не из runDaemon, и сломал бы graceful shutdown).
// runLIFO дёргается единственным defer-ом оркестратора и воспроизводит
// порядок прежних defer-ов один-в-один (последний зарегистрированный —
// первый исполняется).
type cleanupStack struct{ fns []func() }

func (c *cleanupStack) push(fn func()) { c.fns = append(c.fns, fn) }

func (c *cleanupStack) runLIFO() {
	for i := len(c.fns) - 1; i >= 0; i-- {
		c.fns[i]()
	}
}

// daemon — накапливаемое состояние wiring-а `keeper run`. Поля группируются по
// подсистемам; каждый setupX-метод читает уже-заполненные поля предыдущих
// шагов и пишет свои. Так добавление новой зависимости = новое поле + чтение
// внутри своего метода, без правки оркестратора (см. мотивацию рефактора).
type daemon struct {
	cleanups *cleanupStack

	// --- config ---
	initialize    bool
	store         *config.Store[config.KeeperConfig]
	cfg           *config.KeeperConfig
	jwtIssuerName string

	// --- observability (early: logger) ---
	logger *slog.Logger

	// --- vault ---
	vc *keepervault.Client

	// --- storage ---
	pool *pgxpool.Pool

	// --- jwt ---
	verifier   *keeperjwt.Verifier
	issuer     *keeperjwt.Issuer
	ttlDefault time.Duration

	// --- rbac ---
	rbacHolder *rbac.Holder
	rbacSvc    *rbac.Service

	// --- service-registry (реестр Service-ов/keeper_settings в PG, ADR-029) ---
	// serviceHolder — read-only in-memory снимок реестра (TTL-poll + pub/sub-
	// инвалидация); из него читают потребители scenario (serviceRegistry/
	// destinySource, S4). serviceSvc — CRUD-фасад (OpenAPI/MCP, S3).
	serviceHolder *serviceregistry.Holder
	serviceSvc    *serviceregistry.Service
	// serviceRefs — TTL-кеш git-ls-remote-листинга tag/branch для
	// `GET /v1/services/{name}/refs` (UI Upgrade-modal dropdown). Per-keeper,
	// не cluster-wide (refs read-only; отставание между инстансами не нарушает
	// консистентность реестра).
	serviceRefs *serviceregistry.RefsCache

	// serviceScenarios — TTL-кеш scenario-listing-а из материализованного
	// снапшота git-репо Service-а для `GET /v1/services/{name}/scenarios` (UI
	// Run-modal dropdown). Per-keeper, не cluster-wide — read-only listing.
	serviceScenarios *serviceregistry.ScenariosCache

	// serviceStateSchema — TTL-кеш state_schema-метаданных (version +
	// declared schema + migrations metadata) из материализованного снапшота
	// git-репо Service-а для `GET /v1/services/{name}/state-schema` (UI
	// Schema explorer). Per-keeper, не cluster-wide — read-only listing
	// (parity с serviceScenarios).
	serviceStateSchema *serviceregistry.StateSchemaCache

	// serviceDependencies — TTL-кеш git-зависимостей (destiny/modules из
	// `service.yml`) из материализованного снапшота git-репо Service-а для
	// `GET /v1/services/{name}/dependencies` (UI Service Detail). Per-keeper,
	// не cluster-wide — read-only listing (parity с serviceStateSchema).
	serviceDependencies *serviceregistry.DependenciesCache

	// --- augur (ADR-025) ---
	// augurSvc — management-CRUD реестров Omen / Rite (OpenAPI/MCP). ОТЛИЧАЕТСЯ
	// от Augur-брокера (keepergrpc.AugurDeps в EventStream): тот резолвит
	// AugurRequest от Soul-а, этот — operator-facing управление записями.
	augurSvc *keeperaugur.Service

	// --- oracle (ADR-030 beacons) ---
	// oracleSvc — management-CRUD реестров Vigil / Decree (OpenAPI/MCP).
	// ОТЛИЧАЕТСЯ от reactor-роутера (oracleScenarioEnqueuer в EventStream): тот
	// резолвит Portent от Soul-а, этот — operator-facing управление записями.
	oracleSvc *oracle.Service

	// --- sigil (ADR-026) ---
	// nil = Sigil выключен (нет sigil.signing_key_ref): plugin.*-routes/tools
	// не регистрируются (паттерн rbacSvc). Конструктор nil-safe (см. setupSigil).
	sigilSvc *sigil.Service
	// sigilKeySvc — operator-facing ротация trust-anchor-ключей подписи Sigil
	// (ADR-026(h), R3-S7): introduce (key-gen+Vault-write) / retire / set-primary /
	// list. nil при выключенном Sigil (как sigilSvc). Заполняется в setupSigil;
	// читают setupAPIServer / setupMCPServer / setupSigilInvalidation (publisher).
	sigilKeySvc *sigil.KeyService
	// sigilAnchors — НАБОР trust-anchor-ов подписи Sigil (ADR-026(h), R3
	// multi-anchor) для keeper-host verify СВОИХ плагинов (ADR-026(f),
	// S6-keeper-verify). Заполняется в setupSigil из Signer.AnchorSet() (все
	// active-ключи подписи); пустой при выключенном Sigil → keeper-host плагины
	// fail-closed (no_trust_anchor). OR-проверка по набору даёт безразрывную
	// ротацию ключа. Читается в setupCoreModules (идёт после setupSigil).
	sigilAnchors []ed25519.PublicKey
	// sigilAnchorSource — «живой» holder набора PEM-якорей для connect-time
	// broadcast SigilTrustAnchors (ADR-026(h), R3-S6) И bootstrap-reply (R3-S7,
	// architect af7d). setupSigil ставит стартовый набор, watcher
	// `sigil:anchors-changed` обновляет его после runtime-ротации; EventStream-
	// handler и Bootstrap-handler читают свежий набор при каждом connect-е/онбординге.
	// nil → Sigil выключен (broadcast/bootstrap-pubkey no-op).
	sigilAnchorSource *trustAnchorHolder
	// sigilHost — keeper-host (verify СВОИХ плагинов), сохранён из setupCoreModules,
	// чтобы watcher `sigil:anchors-changed` мог атомарно обновить его verify-набор
	// (Host.SigilAnchors.SetAnchors) при runtime-ротации без рестарта.
	sigilHost *pluginhost.Host
	// sigilKeyMetrics — keeper_sigil_*-дескриптор (gauge active-ключей + re-broadcast
	// набора якорей). Тот же экземпляр, что инжектится в sigilKeySvc; сохранён в
	// daemon, чтобы reloadAnchors фиксировал re-broadcast-наблюдаемость
	// (ObserveAnchorsRebroadcast: счётчик проходов + delivered последней раздачи,
	// ADR-026(h), R3 known-gap). nil при выключенном Sigil / до wire-up registry —
	// все Observe* no-op (nil-safe методы KeyMetrics).
	sigilKeyMetrics *sigil.KeyMetrics

	// --- audit ---
	auditWriter audit.Writer

	// --- herald (ADR-052) ---
	// heraldDispatcher — notification-dispatcher: матч событий прогона против
	// включённых Tiding-правил (S2). nil при сбое сборки tap-а (fail-open).
	heraldDispatcher *herald.Dispatcher
	// heraldTap — notification-tap поверх audit-writer (multi-writer
	// декоратор). Метрики прокидываются late-binding в setupMetricsRegistry;
	// Close — в cleanup-стеке. nil при fail-open.
	heraldTap *herald.NotificationTap
	// heraldDeliveryMetrics — keeper_herald_delivery_*-дескриптор claim-queue
	// worker-а доставки (ADR-052(d), S3). Регистрируется в setupMetricsRegistry,
	// инжектится в DeliveryWorker в setupHeraldDelivery (после setupRedis). nil-
	// safe методы — при отсутствии Redis (доставка деградирует) не инжектится.
	heraldDeliveryMetrics *herald.DeliveryMetrics
	// heraldSvc — CRUD-фасад реестров heralds/tidings (S4): один источник правды
	// для REST (api.Deps.HeraldSvc) и MCP (HandlerDeps.HeraldSvc). Несёт
	// in-process invalidate (heraldDispatcher) + cross-keeper Redis-publisher.
	// Собирается в setupHeraldSvc (после setupAudit — нужен heraldDispatcher — и
	// setupRedis — нужен publisher); ДО setupAPIServer/setupMCPServer.
	heraldSvc *herald.Service
	// heraldInvalidation — подписка на `herald:invalidate` (cross-keeper сброс
	// снимка dispatcher-кэша). nil при выключенном Redis.
	heraldInvalidation *keeperredis.HeraldInvalidateSubscription

	// --- metrics ---
	metricsReg      *obs.Registry
	httpMetrics     *obs.HTTPMetrics
	grpcMetrics     *keepergrpc.GRPCMetrics
	scenarioMetrics *scenario.ScenarioMetrics
	renderMetrics   *render.RenderMetrics
	// augurMetrics — keeper_augur_*-дескриптор брокера AugurRequest. Регистрируется
	// в setupMetricsRegistry, инжектится в keepergrpc.AugurDeps.Metrics в
	// setupGRPCEventStream (идёт позже по steps). nil при выключенной
	// observability невозможен (registry поднимается всегда), но nil-safe ObserveX
	// держит инвариант.
	augurMetrics *keeperaugur.BrokerMetrics

	// oracleMetrics — keeper_oracle_*-дескриптор reactor-роутера Oracle (ADR-030
	// S4). Регистрируется в setupMetricsRegistry, инжектится в
	// keepergrpc.OracleDeps.Metrics в setupGRPCEventStream (паттерн augurMetrics).
	oracleMetrics *oracle.OracleMetrics

	// --- scenario deps ---
	serviceLoader    *artifact.ServiceLoader
	topologyResolver *topology.Resolver
	essenceResolver  *essence.Resolver
	renderPipeline   *render.Pipeline
	serviceRegistry  *scenario.ServiceRegistry
	destinySource    *scenario.DestinySource
	// coreModules — keeper-side core-модули (ADR-017), собранные в setupCoreModules.
	// Передаётся scenario-runner-у только в run-goroutine-пути для локального
	// исполнения задач с `on: keeper` (docs/keeper/modules.md): keeper-задачи в
	// pilot исполняет run-goroutine (dispatchKeeperTasks), Acolyte-claim путь их
	// не исполняет (groupByHost пропускает plan.Keeper). nil до setupCoreModules
	// → keeper-side задача отвергается scenario-runner-ом.
	coreModules *coremod.Registry

	// --- push orchestrator (Variant C, docs/keeper/push.md) ---
	// pushDestinyLoader — git-loader destiny-репозиториев для pushDestinyResolver.
	// Тот же тип, что scenario.NewDestinySource ниже потребляет (artifact.DestinyLoader),
	// шарим объект на процесс.
	pushDestinyLoader *artifact.DestinyLoader
	// pushDiscoveredSsh — дискаверенные SshProvider-плагины (filtered by catalog
	// `plugins.ssh_providers[]`). Заполняется в setupCoreModules одной discover-
	// волной с cloud-плагинами, потребляется в setupPushDispatchers для Spawn.
	// nil/пустой → push выключен (см. setupPushDispatchers).
	pushDiscoveredSsh []pluginhost.Discovered
	// pushPluginHost — keeper-host, разделяемый с setupCoreModules (тот же объект),
	// сохранён для setupPushDispatchers: spawn SshProvider-плагина с env-payload
	// (ADR-020 amendment l, S6 pilot). nil при выключенном Sigil → setupPushDispatchers
	// возвращает skip (push выключен), что симметрично гейту core-модулей.
	pushPluginHost *pluginhost.Host
	// pushSshPlugin — long-living handle SshProvider-плагина (S6 pilot, single-provider).
	// Spawn делается один раз в setupPushDispatchers и держится до shutdown (cleanup
	// Closes plugin в LIFO ДО Redis/Pool). nil при выключенной push-инфраструктуре.
	pushSshPlugin *pluginhost.SshProviderPlugin
	// pushDispatcher — *push.SshDispatcher из push S1+S5 (ADR-004 push-flow).
	// nil → push-orchestrator не поднимается (api.Deps.PushRun=nil →
	// /v1/push/*-роуты не подключаются; keeper.push.apply tool возвращает
	// «не сконфигурировано»). Wire-up — setupPushDispatchers (S6, pilot-path:
	// config-backed targets/providers + single Vault host-CA).
	pushDispatcher pushorch.SshDispatcher
	// pushSshDispatcher — конкретный *push.SshDispatcher, тот же объект, что
	// pushDispatcher, но удерживается отдельно для вызова RefreshProvider из
	// invalidation-listener-а (узкий pushorch.SshDispatcher не несёт этот
	// метод — он чисто dispatch-поверхность). nil при выключенной push-
	// инфраструктуре.
	pushSshDispatcher *push.SshDispatcher
	// pushCleaner — *push.SshDispatcher тот же, что pushDispatcher, доступ только
	// по узкому интерфейсу Cleanup (cleanup_stale_versions=true after success).
	pushCleaner pushorch.Cleaner
	// pushRun — orchestrator, собранный в setupPushOrchestrator поверх
	// renderPipeline + topologyResolver + pushDestinyLoader + serviceHolder +
	// pushDispatcher. nil при отсутствии pushDispatcher (ssh-плагины не
	// подняты — см. doc выше).
	pushRun *pushorch.PushRun
	// pushProviderSvc — CRUD-фасад реестра push_providers (ADR-032 amendment
	// 2026-05-26, S7-2). Поднимается всегда (PG-таблица доступна), даже если
	// push-dispatcher выключен: оператор должен иметь возможность настроить
	// провайдеров до включения push-инфраструктуры. nil только если pool не
	// создан (не должно случиться в production-path).
	pushProviderSvc *pushprovider.Service
	// pushProviderInvalidation — Redis pub/sub subscription на
	// `push-providers:changed`. Используется для cluster-wide уведомления о
	// мутации (re-spawn плагина на ближайшем RPC, PM-decision S7-2 #6). nil
	// при выключенном Redis. Cleanup закрывает goroutine.
	pushProviderInvalidation *keeperredis.PushProvidersChangedSubscription
	// pushMetrics — keeper_push_*-дескрипторы (S7-3 multi-CA: counter матчей
	// host-CA с разрезом по `ca_name`). Регистрируется в setupMetricsRegistry
	// безусловно (registry всегда поднят); инжектится в push.Deps.Metrics в
	// setupPushDispatchers. nil-safe методы — push выключен / unit-тесты без
	// observability оставляют дескриптор не-инжектённым.
	pushMetrics *push.Metrics

	// --- redis / apply-bus ---
	redisClient *keeperredis.Client
	applyBus    *applybus.EventBus

	// --- conclave (реестр живых Keeper-инстансов, ADR-006 amend, soul-shedding S1) ---
	// conclaveInstances — gauge числа живых keeper-инстансов в Conclave
	// (keeper_conclave_instances). Обновляется renewal-goroutine после каждого
	// renew-тика по LiveKIDs. nil при выключенной observability / отсутствии
	// Redis — все Set no-op (nil-safe Gauge).
	conclaveInstances prometheus.Gauge

	// --- grpc ---
	// streamManager — реестр активных EventStream-стримов этого инстанса. Сохранён
	// на daemon, чтобы Watchman (soul-shedding S2) мог принудительно закрыть все
	// локальные стримы (CloseAll) при устойчивой изоляции инстанса.
	streamManager  *keepergrpc.StreamManager
	outbound       *keepergrpc.Outbound
	scenarioRunner *scenario.Runner

	// --- watchman (изоляция-детект + soul-shedding S2) ---
	// watchmanMetrics — keeper_watchman_*-дескриптор (gauge isolated + счётчик
	// streams_shed). Регистрируется в setupMetricsRegistry, инжектится в
	// watchman.New в setupWatchman (паттерн conclaveInstances). nil-safe.
	watchmanMetrics *watchmanMetrics

	// --- toll (cluster-wide detector массового оттока Souls, ADR-038) ---
	// tollMetrics — keeper_toll_*-дескриптор + keeper_cluster_degraded gauge.
	// Регистрируется в setupMetricsRegistry, инжектится в toll.NewWatcher /
	// toll.NewLeader в setupToll. nil-safe методы.
	tollMetrics *toll.Metrics
	// tollWatcher — per-instance disconnect-наблюдатель (без goroutine, hook
	// EventStream-cleanup-а). nil при выключенном Toll (нет Redis, или
	// `toll.enabled: false`) → eventstream notifyTollDisconnect no-op.
	tollWatcher *toll.Watcher
	// tollDegradedReader — read-флаг cluster:degraded для api-middleware. При
	// выключенном Toll — реализация-noop (всегда false → middleware passthrough).
	tollDegradedReader toll.DegradedReader
	// tollLeader — cluster-leader goroutine Toll-а. Сохраняется на daemon, чтобы
	// hot-reload-подписка (см. setupToll) могла позвать [toll.Leader.UpdateConfig]
	// на каждый успешный config-swap. nil при выключенном Toll.
	tollLeader *toll.Leader
	// tollWebhookCfg — snapshot конфига webhook-канала на момент построения
	// notifier-а. Используется hot-reload-подпиской для diff-а: webhook
	// пересоздаётся только при изменении URLRef/Format/Timeout/Enabled, чтобы
	// частые reload-ы с неизменными webhook-полями не дёргали Vault-резолв.
	tollWebhookCfg *config.KeeperTollWebhook

	// --- tempo (per-AID rate-limiter write-API, ADR-050) ---
	// tempoMetrics — keeper_tempo_allowed_total / keeper_tempo_rejected_total
	// {endpoint}. Регистрируется в setupMetricsRegistry безусловно (паттерн
	// tollMetrics), инжектится в api.Deps.TempoMetrics. nil-safe.
	tempoMetrics *api.TempoMetrics
	// tempoLimiter — Redis token-bucket Tempo. Конструируется в setupTempo
	// ТОЛЬКО при наличии Redis (без Redis → nil → middleware passthrough,
	// ADR-050(a)+(b)). Инжектится в api.Deps.TempoLimiter. Сам limiter
	// stateless — rate/burst читаются на каждом запросе из config.Store
	// (hot-reload, ADR-050(f)).
	tempoLimiter *keeperredis.TokenBucket

	// --- acolyte (ADR-027) ---
	acolytePool *acolyte.Pool

	// voyageReclaimer — wired-up в setupReaper (Reaper-правило reclaim_voyages,
	// ADR-043 S4). Зависит только от d.pool; правило default-ON через
	// path-defaulting в reaper.dispatch (ADR-043 §8) — оператор выключает
	// `reclaim_voyages: { enabled: false }`. Сохраняется для диагностики/тестов.
	voyageReclaimer *reaper.VoyageReclaimer

	// --- errand (ADR-033) ---
	// errandStore / errandDispatcher — pull-ad-hoc Errand contour. Собираются в
	// setupErrandDispatcher после setupGRPCEventStream (d.outbound нужен
	// dispatcher-у) и ДО setupAPIServer (API.Deps читает обе ссылки). При
	// отсутствии Redis dispatcher деградирует на local-only routing (single-
	// keeper dev): holder lease-а неизвестен → пробуем напрямую через outbound.
	errandStore      *errand.Store
	errandDispatcher *errand.Dispatcher

	// --- voyage (ADR-043, S1) ---
	// voyagePoolStarted — флаг, что setupVoyageWorker реально поднял pool
	// (cfg.Voyage != nil && workers > 0). На S1 нет API-эндпоинтов /v1/runs
	// (S5) — флаг существует ради parity-диагностики и будущего gate.
	voyagePoolStarted bool

	// --- api server (.Start дёргается оркестратором последним) ---
	apiServer *api.Server
}

// setupConfig — флаги + env, загрузка Store, диагностика, снимок cfg и derive
// JWT issuer. Особая exit-семантика (флаг-парс → exitUsage, ранние
// config-ошибки → stderr напрямую, logger ещё нет) оставлена вне общего
// паттерна: метод сам печатает в stderr и сигналит вид ошибки через exitCode.
func (d *daemon) setupConfig(args []string) (exitCode int, ok bool) {
	var (
		configPath string
		initialize bool
	)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&configPath, "config", defaultConfigPath, "keeper.yml path")
	fs.BoolVar(&initialize, "initialize", false, "allow start with empty operators registry (bootstrap-pending mode)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: keeper run [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage, false
	}
	// ADR-013(d): bootstrap-pending режим включается флагом `--initialize`
	// ЛИБО env `KEEPER_INITIALIZE=true` (для контейнер/CI-окружений, где
	// флаг неудобен). Env читаем как truthy-OR: пустая/невалидная строка →
	// false, флаг остаётся приоритетным источником «истины-вверх».
	initialize = initialize || envTruthy("KEEPER_INITIALIZE")
	d.initialize = initialize

	// Reaper-runner требует Store[KeeperConfig] для hot-reload-аware
	// чтения (ADR-021 / M0.3). Используем тот же путь, что api/health —
	// один LoadKeeperStore покрывает оба consumer-а; Store.Get() даёт
	// текущий снимок для остальной wire-up-логики.
	store, diags, err := config.LoadKeeperStore(configPath, config.ValidateOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: load config %q: %v\n", configPath, err)
		return exitError, false
	}
	if diag.HasErrors(diags) {
		fmt.Fprintf(os.Stderr, "keeper run: config %q has errors:\n", configPath)
		for _, dg := range diags {
			if dg.Level == diag.LevelError {
				fmt.Fprintf(os.Stderr, "  - %s [%s]: %s\n", dg.Phase, dg.Code, dg.Message)
			}
		}
		return exitError, false
	}
	cfg := store.Get()
	if cfg == nil {
		fmt.Fprintln(os.Stderr, "keeper run: config snapshot is nil after successful load (unexpected)")
		return exitError, false
	}
	d.store = store
	d.cfg = cfg

	if cfg.Auth == nil || cfg.Auth.JWT == nil {
		fmt.Fprintln(os.Stderr, "keeper run: auth.jwt block is required in keeper.yml")
		return exitError, false
	}
	jwtIssuerName := cfg.Auth.JWT.Issuer
	if jwtIssuerName == "" {
		jwtIssuerName = cfg.KID
	}
	if jwtIssuerName == "" {
		fmt.Fprintln(os.Stderr, "keeper run: cannot derive JWT issuer (both auth.jwt.issuer and kid are empty)")
		return exitError, false
	}
	d.jwtIssuerName = jwtIssuerName
	return exitOK, true
}

// setupObservabilityEarly — logger + level-handle, подписка на hot-reload
// `logging.level` и SIGHUP-watcher. Подписки ставятся до подъёма тяжёлых
// зависимостей, чтобы level применялся даже на ранних reload-ах.
func (d *daemon) setupObservabilityEarly(ctx context.Context) error {
	cfg := d.cfg
	// Logger строится после успешной загрузки/валидации cfg, чтобы читать
	// logging-ротацию из keeper.yml (ранние config-ошибки выше уходят в
	// stderr напрямую, не через logger). NewWithLevel возвращает level-handle:
	// на hot-reload (`logging.level`, ADR-021) меняем порог логирования без
	// пере-создания writer-а (file/format/rotation — restart-required).
	logger, logLevel := shlog.NewWithLevel(shlog.FromKeeper(cfg.Logging))
	d.logger = logger

	// Hot-reload `logging.level` (ADR-021): на каждый успешный Store-swap
	// двигаем порог логирования по новому снимку. Подписку ставим до подъёма
	// тяжёлых зависимостей — level применяется даже на ранних reload-ах.
	// Остальные logging.*-поля (file/format/rotation) restart-required —
	// их не трогаем (см. docs/keeper/config.md → таблица).
	d.store.OnReload(func(_, newCfg *config.KeeperConfig) {
		if newCfg != nil {
			logLevel.Set(newCfg.Logging.Level)
		}
	})

	// SIGHUP-reload (ADR-021(b)). Watcher ведёт ОТДЕЛЬНЫЙ signal-канал —
	// SIGHUP не попадает в signalContext (SIGINT/SIGTERM), поэтому reload не
	// путается с shutdown. Запускается только если hot_reload.enable_signal
	// (default true); при false — file-edit reload отключён. write-back на
	// диск тут не происходит (read-only по отношению к файлу).
	if cfg.HotReload.SignalEnabled() {
		reloadCh := config.WatchSIGHUP(ctx, d.store)
		go config.LogReloads(reloadCh, logger)
		logger.Info("keeper run: SIGHUP config reload enabled")
	} else {
		logger.Info("keeper run: SIGHUP config reload disabled (hot_reload.enable_signal=false)")
	}
	return nil
}

// setupVault — vault-клиент (до pool: postgres.dsn_ref может быть vault-ref) и
// фоновый token-renewer под собственным renewerCtx. Cleanup-пара (cancel →
// Stop-wait) регистрируется в том же относительном порядке, что и прежние
// vault-defer'ы.
func (d *daemon) setupVault(ctx context.Context) error {
	// Vault-client поднимается до pg.NewPool — `postgres.dsn_ref`
	// может быть vault-ref.
	vc, err := keepervault.NewClient(ctx, d.cfg.Vault)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: vault client: %v\n", err)
		return errSetupFailed
	}
	d.vc = vc
	logger := d.logger

	// Auto-renew vault-токена в фоне, пока процесс жив. Для non-renewable
	// токена (root/static dev) watcher не стартует — keeper работает дальше.
	//
	// Под своим renewerCtx (производный от parent-а) — тем же приёмом, что и
	// reaper-runner ниже. Stop() блокируется на выходе goroutine, а goroutine
	// выходит только по отмене своего ctx. Если бы Stop зависел от внешнего
	// ctx, любой fatal-`return exitError` ниже (DSN/migrations/operators/...)
	// подвесил бы shutdown: внешний ctx ещё жив, cleanup Stop() (LIFO раньше
	// renewerCancel) ждал бы вечно. renewerCancel выполняется первым и
	// разблокирует Stop независимо от пути выхода.
	//
	// Cleanup-стек LIFO. Целевой порядок выполнения:
	//
	//  1. renewerCancel()        — сигнализируем goroutine остановиться.
	//  2. <-Stop() (с timeout-ом) — ждём её реального выхода; 5s + warn про
	//     возможный leak, иначе зависший watcher.Stop() при недоступном
	//     Vault на shutdown подвесил бы exit.
	renewerCtx, renewerCancel := context.WithCancel(ctx)
	tokenRenewer, err := vc.StartTokenRenewer(renewerCtx, logger)
	if err != nil {
		renewerCancel()
		fmt.Fprintf(os.Stderr, "keeper run: vault token renewer: %v\n", err)
		return errSetupFailed
	}
	// 2. Ждём выхода renewer-goroutine.
	d.cleanups.push(func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			tokenRenewer.Stop()
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.Warn("vault token renewer did not stop within 5s after shutdown — leak suspected")
		}
	})
	// 1. Отменяем renewerCtx (зарегистрирован позже → LIFO выполнит первым).
	d.cleanups.push(renewerCancel)
	return nil
}

// setupStorage — PG pool + Ping + ResolveDSN + Apply migrations. pool
// поднимается после vault-клиента (DSN-резолв), миграции — до любого чтения
// таблиц (guard / RBAC-схема зависят от applied migrations).
func (d *daemon) setupStorage(ctx context.Context) error {
	pool, err := keeperpg.NewPool(ctx, d.cfg.Postgres, d.vc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: pg pool: %v\n", err)
		return errSetupFailed
	}
	d.pool = pool
	d.cleanups.push(pool.Close)
	if err := keeperpg.Ping(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: pg ping: %v\n", err)
		return errSetupFailed
	}

	dsn, err := keeperpg.ResolveDSN(ctx, d.vc, d.cfg.Postgres.DSNRef)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: resolve DSN: %v\n", err)
		return errSetupFailed
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: apply migrations: %v\n", err)
		return errSetupFailed
	}
	return nil
}

// setupOperatorBootstrapGuard — Count operators + restart-семантика ADR-013(d)
// (пустой реестр без --initialize → отказ старта). Зависит от applied
// migrations.
func (d *daemon) setupOperatorBootstrapGuard(ctx context.Context) error {
	n, err := operator.Count(ctx, d.pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: count operators: %v\n", err)
		return errSetupFailed
	}
	proceed, refuseMsg, pending := guardOperatorsRegistry(n, d.initialize)
	if !proceed {
		fmt.Fprintln(os.Stderr, refuseMsg)
		return errSetupFailed
	}
	if pending {
		d.logger.Info("keeper run: ready to bootstrap, no operators yet (bootstrap-pending mode)")
	} else {
		d.logger.Info("keeper run: ready", slog.Int64("operators", n))
	}
	return nil
}

// setupJWT — signing-key из Vault, Verifier/Issuer и ttl_default.
func (d *daemon) setupJWT(ctx context.Context) error {
	signingKey, err := bootstrap.LoadSigningKey(ctx, d.vc, d.cfg.Auth.JWT.SigningKeyRef)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: load signing key: %v\n", err)
		return errSetupFailed
	}
	verifier, err := keeperjwt.NewVerifier(signingKey, d.jwtIssuerName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build JWT verifier: %v\n", err)
		return errSetupFailed
	}
	issuer, err := keeperjwt.NewIssuer(signingKey, d.jwtIssuerName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build JWT issuer: %v\n", err)
		return errSetupFailed
	}
	d.verifier = verifier
	d.issuer = issuer

	ttlDefault, err := parseTTL(d.cfg.Auth.JWT.TTLDefault, "auth.jwt.ttl_default", 24*time.Hour)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: %v\n", err)
		return errSetupFailed
	}
	d.ttlDefault = ttlDefault
	return nil
}

// setupRBAC — read-only enforcer-снимок (rbac.Holder + фоновый Run) и
// CRUD-фасад rbac.Service. Pub/sub-инвалидация подключается отдельно в
// setupRBACInvalidation (после redisClient).
func (d *daemon) setupRBAC(ctx context.Context) error {
	// RBAC enforcer обёрнут в [rbac.Holder], строящий снимок из БД
	// (ADR-028(d) — RBAC-storage перенесён в Postgres). Initial-build падает
	// fatal на недоступной/битой RBAC-схеме (миграции 026/027 уже применены
	// выше). Стратегия обновления Фаза 1 = B1 (TTL-poll): фоновая goroutine
	// перечитывает снимок каждые rbac.DefaultRefreshInterval; ошибка перечита
	// оставляет прежний enforcer + warn (БД-сбой не превращает всех в
	// default-deny). Redis pub/sub-инвалидация (B2) — Фаза 3.
	rbacHolder, err := rbac.NewHolder(ctx, rbac.PoolSource{DB: d.pool}, rbac.DefaultRefreshInterval, d.logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build RBAC enforcer: %v\n", err)
		return errSetupFailed
	}
	go rbacHolder.Run(ctx)
	d.rbacHolder = rbacHolder

	// RBAC-CRUD-фасад (ADR-028 Фаза 2): мутирующая бизнес-логика role.* —
	// один экземпляр на процесс, шарится REST (api.Deps.RBACSvc) и MCP
	// (mcp.HandlerDeps.RBACRoles). ОТЛИЧАЕТСЯ от rbacHolder выше: тот —
	// read-only enforcer-снимок (Check/ClusterAdmins/RolesOf), этот — CRUD
	// над БД под FOR UPDATE. role.*-роуты/tools подключает Slice 2a/2b.
	rbacSvc, err := rbac.NewService(rbac.ServiceDeps{Pool: d.pool, Logger: d.logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build RBAC service: %v\n", err)
		return errSetupFailed
	}
	d.rbacSvc = rbacSvc
	return nil
}

// setupServiceRegistry — in-memory снимок реестра Service-ов/keeper_settings
// (serviceregistry.Holder + фоновый Run) и CRUD-фасад serviceregistry.Service
// (реестр Service-ов перенесён из статического keeper.yml в PG, ADR-029, паттерн
// setupRBAC). Pub/sub-инвалидация подключается отдельно в
// setupServiceRegistryInvalidation (после redisClient).
//
// Holder — единственный источник реестра для потребителей scenario
// (serviceRegistry/destinySource в setupScenarioDeps читают его Resolve/
// DefaultDestinySource, S4). transport-фасад (OpenAPI/MCP) для serviceSvc — S3.
func (d *daemon) setupServiceRegistry(ctx context.Context) error {
	// Снимок реестра обёрнут в [serviceregistry.Holder], строящий его из БД
	// (миграции 034/035 уже применены в setupStorage). Initial-build падает
	// fatal на недоступной/битой схеме. Стратегия обновления: TTL-poll
	// (фоновая goroutine перечитывает снимок каждые
	// serviceregistry.DefaultRefreshInterval; ошибка перечита оставляет прежний
	// снимок + warn) + Redis pub/sub-инвалидация (setupServiceRegistryInvalidation).
	holder, err := serviceregistry.NewHolder(ctx, serviceregistry.PoolSource{DB: d.pool}, serviceregistry.DefaultRefreshInterval, d.logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build service-registry holder: %v\n", err)
		return errSetupFailed
	}
	go holder.Run(ctx)
	d.serviceHolder = holder

	// CRUD-фасад реестра (S1): один экземпляр на процесс. ОТЛИЧАЕТСЯ от holder
	// выше: тот — read-only снимок (Resolve/DefaultDestinySource), этот — CRUD
	// над БД. transport-роуты/tools подключает S3; invalidate-хук — S2
	// (setupServiceRegistryInvalidation).
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: d.pool, Logger: d.logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build service-registry service: %v\n", err)
		return errSetupFailed
	}
	d.serviceSvc = svc

	// TTL-кеш git-ls-remote ответа для `/v1/services/{name}/refs` (UI). Lister —
	// прямой artifact.ListRefs (go-git ListContext, без клонирования). TTL по
	// умолчанию (RefsTTL=60s).
	d.serviceRefs = serviceregistry.NewRefsCache(
		artifact.RefsListerFunc(artifact.ListRefs),
		0, // 0 → дефолтный RefsTTL
	)

	// Augur management-CRUD реестров Omen / Rite (ADR-025, OpenAPI/MCP). Тот же
	// pool. Брокер-сторона (резолв AugurRequest) поднимается отдельно в gRPC-
	// wire-up (keepergrpc.AugurDeps), здесь — только operator-facing управление.
	augurSvc, err := keeperaugur.NewService(keeperaugur.ServiceDeps{Pool: d.pool, Logger: d.logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build augur service: %v\n", err)
		return errSetupFailed
	}
	d.augurSvc = augurSvc

	// Oracle management-CRUD реестров Vigil / Decree (ADR-030 beacons, OpenAPI/
	// MCP). Тот же pool. Reactor-сторона (резолв Portent → match Decree →
	// enqueue) поднимается отдельно в gRPC-wire-up (oracleScenarioEnqueuer),
	// здесь — только operator-facing управление. Where — отдельный
	// WhereEvaluator для compile-проверки where-CEL Decree-а на create (тот же
	// sandbox-env, что reactor использует на горячем пути).
	oracleWhereCheck, err := oracle.NewWhereEvaluator()
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build oracle where-evaluator: %v\n", err)
		return errSetupFailed
	}
	oracleSvc, err := oracle.NewService(oracle.ServiceDeps{Pool: d.pool, Where: oracleWhereCheck, Logger: d.logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build oracle service: %v\n", err)
		return errSetupFailed
	}
	d.oracleSvc = oracleSvc
	return nil
}

// setupAudit — PG audit-writer (опц. обёрнутый в multi-writer с herald
// notification-tap) + late-binding в Store (с этого момента каждый Store.Reload
// пишет config.reload_* в audit_log).
func (d *daemon) setupAudit(_ context.Context) error {
	auditWriter := audit.Writer(auditpg.NewWriter(d.pool))

	// Herald notification-tap (ADR-052(c)): multi-writer декоратор над
	// PG-writer-ом. Tap получает событие ПОСЛЕ успешной PG-записи и матчит его
	// против включённых Tiding-правил (notification-dispatcher), не влияя на
	// исход audit-записи (best-effort, ADR-022(f)).
	//
	// Сборка безотказна: NewDispatcher/NewNotificationTap ошибок не возвращают —
	// чистая in-memory-инициализация (PGRuleSource — ленивый адаптер над d.pool,
	// запрос к tidings отложен до первого матча и best-effort; LogDeliveryQueue —
	// без доставки в S2), поэтому heraldTap всегда non-nil. Fail-open-ветка
	// (деградация на голый PG-writer при сбое сборки) появится, если конструкторы
	// станут возвращать ошибку (S3+ с реальной доставкой). Метрики dispatcher-а —
	// late-binding в setupMetricsRegistry (тот идёт ПОСЛЕ setupAudit по init-order,
	// как vault/rbac SetMetrics).
	dispatcher := herald.NewDispatcher(herald.DispatcherConfig{
		Source: herald.PGRuleSource{DB: d.pool},
		Queue:  &herald.LogDeliveryQueue{Logger: d.logger},
		Logger: d.logger,
	})
	tap := herald.NewNotificationTap(dispatcher, d.logger, 0)
	d.heraldDispatcher = dispatcher
	d.heraldTap = tap
	d.cleanups.push(tap.Close)
	auditWriter = audit.NewMultiWriter(auditWriter, d.logger, tap)

	d.auditWriter = auditWriter

	// Audit-эмиссия config.reload_* (ADR-021(g), ADR-022(j)). Store создаётся
	// выше (до подъёма pool/writer — порядок init выверен Vault→pool→
	// migrations→writer), поэтому audit-writer инъектируется late-binding:
	// с этого момента каждый Store.Reload пишет config.reload_succeeded/
	// config.reload_failed в audit_log.
	d.store.SetAuditWriter(auditWriter)
	return nil
}

// setupCoreModules — keeper-side core-модули (ADR-017): PluginHost discovery +
// cloud-adapter + coremod.Default. Discovery best-effort.
func (d *daemon) setupCoreModules(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper-side core-модули (ADR-017, docs/keeper/modules.md): три модуля,
	// диспетчер `on: keeper`. Wire-up scenario-runner-а в gRPC EventStream —
	// отдельная задача (M2.5); сейчас Registry собирается заранее, чтобы при
	// его подключении не пришлось менять main и rewire-ить зависимости.
	//
	// PluginHost: NewHost + Discover + FilterByCatalog. Discovery
	// best-effort — отсутствие cache-root или пустой каталог не fatal,
	// только означает «cloud-плагины не подключены, `core.cloud.provisioned`
	// в проде вернёт ошибку spawn/unknown provider». StubHost не подсовываем
	// — adapter с пустым списком провайдеров даст внятную ошибку «unknown
	// provider", а stub дал бы загадочный ErrPluginHostNotImplemented.
	//
	// Sigil verify-tract keeper-host-а (ADR-026(f)/(h), S6-keeper-verify): набор
	// trust-anchor-ов — d.sigilAnchors (active-ключи подписи, которыми keeper
	// подписывает допуски; пустой при выключенном Sigil → плагины fail-closed по
	// no_trust_anchor — интенсионально, G-sigil-5: оператор с cloud/ssh обязан
	// настроить Sigil + allow). OR-проверка по набору даёт безразрывную ротацию
	// ключа. lookup — адаптер над реестром plugin_sigils (читается напрямую из
	// d.pool, не EventStream). Core-модули статические, Spawn-verify не проходят
	// — этим не затронуты.
	sigilLookup := pluginhost.NewSigilLookupAdapter(sigilRecordLister{store: sigil.NewPGStore(d.pool)}, logger)
	pluginHost, err := pluginhost.NewHost(cfg.PluginRuntime, d.sigilAnchors, sigilLookup)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build plugin host: %v\n", err)
		return errSetupFailed
	}
	// Сохраняем keeper-host: watcher `sigil:anchors-changed` (setupSigilInvalidation)
	// атомарно обновляет его verify-набор (Host.SigilAnchors.SetAnchors) при
	// runtime-ротации ключей подписи без рестарта (ADR-026(h), R3-S6).
	d.sigilHost = pluginHost
	// Тот же keeper-host шарится с setupPushDispatchers (S6 pilot wire-up
	// SshDispatcher): spawn SshProvider-плагина идёт через него же, чтобы Sigil-
	// verify, capability-allowlist и socket-dir-настройки были одни на процесс.
	d.pushPluginHost = pluginHost
	cacheRoot := pluginCacheRoot(cfg.Plugins)

	// git-резолв каталога плагинов в commit_sha-слоты (ADR-026 F-fetch, A1-S1):
	// наполняет кеш ДО Discover/FilterByCatalog. Per-entry ошибки fail-closed —
	// warnings, не валят старт (Keeper поднимается без сломанного плагина).
	resolver := plugingit.NewResolver(cacheRoot, pluginWorkRoot(cfg.Plugins),
		cfg.Plugins.ResolvedFetchTimeout(),
		cfg.Plugins.ResolvedMaxArtifactSize(), cfg.Plugins.ResolvedMaxCloneSize(), logger)
	if slots, rwarns, rerr := resolver.ResolveCatalog(ctx, cfg.Plugins); rerr != nil {
		logger.Warn("keeper run: plugin git resolve skipped", slog.Any("error", rerr))
	} else {
		for _, w := range rwarns {
			logger.Warn("keeper run: plugin git resolve warning", slog.String("detail", w))
		}
		logger.Info("keeper run: plugins resolved into cache", slog.Int("count", len(slots)))
	}

	var discoveredCloud []pluginhost.Discovered
	if found, warns, derr := pluginhost.Discover(cacheRoot); derr != nil {
		logger.Warn("keeper run: plugin discovery skipped",
			slog.String("cache_root", cacheRoot),
			slog.Any("error", derr))
	} else {
		for _, w := range warns {
			logger.Warn("keeper run: plugin discovery warning", slog.String("detail", w))
		}
		filtered, fwarns := pluginhost.FilterByCatalog(found, cfg.Plugins)
		for _, w := range fwarns {
			logger.Warn("keeper run: plugin catalog mismatch", slog.String("detail", w))
		}
		for _, dd := range filtered {
			if dd.Manifest == nil {
				continue
			}
			switch dd.Manifest.Kind {
			case pluginhost.KindCloudDriver:
				discoveredCloud = append(discoveredCloud, dd)
			case pluginhost.KindSSHProvider:
				// SshProvider-плагины уходят в setupPushDispatchers (S6 pilot).
				// Здесь только индексируем по kind — Spawn делается позже.
				d.pushDiscoveredSsh = append(d.pushDiscoveredSsh, dd)
			}
		}
	}
	cloudAdapter, err := cloud.NewPluginAdapter(pluginHost, discoveredCloud)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build cloud plugin adapter: %v\n", err)
		return errSetupFailed
	}
	// CloudUserdata — рендер cloud-init userdata для scenario-параметра
	// `generate_userdata: true` (ADR-017(h) amendment 2026-05-27, B-flat).
	// Обёртка читает текущий KeeperConfig snapshot через d.store.Get() — hot-
	// reload подхватывается каждым новым cloud-create-шагом без рестарта.
	// nil-блок keeper.yml::cloud_init допустим: scenario без generate_userdata
	// работает без изменений; явный generate_userdata: true тогда вернёт
	// понятную ошибку «не сконфигурирован».
	userdataProvider := &cloudInitProvider{store: d.store, resolver: cloudinit.NewResolver(d.vc)}

	coreReg := coremod.Default(coremod.Deps{
		SoulStore:  coremodsoul.NewPGStore(d.pool),
		PluginHost: cloudAdapter,
		// CloudResolver — A-flow: Provider-реестр (PG) + Vault → driver-имя +
		// plain-credentials. d.vc всегда не-nil после setupVault (тот валит
		// старт при ошибке), как и для core.vault.kv-read ниже.
		CloudResolver: cloud.NewCredentialsResolverPG(cloud.NewProviderReaderPG(d.pool), d.vc),
		CloudSouls:    cloud.NewSoulPG(d.pool),
		CloudTokens:   cloud.NewTokenPG(d.pool, cloud.DefaultBootstrapTokenTTL),
		CloudCascade:  cloud.NewCascadePG(d.pool),
		CloudUserdata: userdataProvider,
		Vault:         d.vc,
		Audit:         d.auditWriter,
		// ChoirStore — `core.choir` (ADR-044): AddVoice/RemoveVoice над
		// incarnation_choir_voices через choir-CRUD (S-T2) + проверка
		// существования инкарнации. Тот же d.pool, что у остальных PG-adapter-ов.
		ChoirStore: coremodchoir.NewPGStore(d.pool),
	})
	logger.Info("keeper run: core modules registered",
		slog.Int("count", len(coreReg.Names())),
		slog.Any("cloud_providers", cloudAdapter.Providers()))
	// Передаём scenario-runner-у (только run-goroutine-путь): задачи с
	// `on: keeper` исполняются локально на инстансе через этот Registry.
	// Acolyte-claim путь keeper-задачи не исполняет (groupByHost их пропускает).
	d.coreModules = coreReg
	return nil
}

// cloudInitProvider — обёртка cloudinit.Resolver+GenerateUserdata под интерфейс
// cloud.UserdataProvider (ADR-017(h) amendment 2026-05-27, B-flat). На каждом
// GenerateUserdata-вызове читает текущий config.Store snapshot — hot-reload
// keeper.yml::cloud_init подхватывается следующим cloud-create-шагом без
// рестарта (через d.store.Get).
type cloudInitProvider struct {
	store    *config.Store[config.KeeperConfig]
	resolver *cloudinit.Resolver
}

func (p *cloudInitProvider) GenerateUserdata(ctx context.Context) (string, error) {
	cfg := p.store.Get()
	if cfg == nil {
		return "", fmt.Errorf("cloud_init: keeper config snapshot is nil")
	}
	resolved, err := p.resolver.Resolve(ctx, cfg.CloudInit)
	if err != nil {
		return "", err
	}
	return cloudinit.GenerateUserdata(resolved)
}

// sigilRecordLister проецирует реестр plugin_sigils (sigil.Store.ListActive)
// в verify-форму shared/pluginhost.SigilRecord для keeper-host verify-tract-а
// (ADR-026(f), S6-keeper-verify). Маппинг живёт здесь (call-site), а не в
// keeper/internal/pluginhost: тот пакет нельзя заставить импортировать
// keeper/internal/sigil (sigil уже импортирует pluginhost — был бы import-цикл).
//
// Manifest = sigil.Sigil.ManifestRaw — byte-exact СЫРЫЕ байты manifest.yaml,
// над которыми поставлена подпись (КАНОН для verify), НЕ JSONB-проекция
// Manifest: re-хеш на verify идёт именно над этими байтами (S3↔S6-инвариант).
type sigilRecordLister struct {
	store sigil.Store
}

func (l sigilRecordLister) ListActive(ctx context.Context) ([]*sharedhost.SigilRecord, error) {
	recs, err := l.store.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*sharedhost.SigilRecord, 0, len(recs))
	for _, s := range recs {
		out = append(out, &sharedhost.SigilRecord{
			Namespace:       s.Namespace,
			Name:            s.Name,
			Ref:             s.Ref,
			BinarySHA256hex: s.SHA256,
			Signature:       s.Signature,
			Manifest:        s.ManifestRaw,
		})
	}
	return out, nil
}

// moduleCatalogPlugins проецирует активные записи plugin_sigils
// (sigil.Store.ListActive) в plugin-секцию module-catalog (`GET /v1/modules`).
// Manifest = sigil.Sigil.ManifestRaw — byte-exact сырые байты manifest.yaml
// (handler парсит из них params; та же форма, что и sigilRecordLister, но в
// handler-формат [handlers.PluginCatalogEntry]).
type moduleCatalogPlugins struct {
	store sigil.Store
}

// moduleCatalogPluginsOrNil возвращает plugin-lister каталога ИЛИ нетипизированный
// nil (НЕ typed-nil): handler проверяет `plugins != nil` для gate-а plugin-секции,
// typed-nil прошёл бы проверку и упал бы на ListActive. При выключенном Sigil
// (d.sigilSvc==nil) реестра plugin_sigils нет → каталог отдаёт только core.
func moduleCatalogPluginsOrNil(d *daemon) handlers.ModuleCatalogPlugins {
	if d.sigilSvc == nil {
		return nil
	}
	return moduleCatalogPlugins{store: sigil.NewPGStore(d.pool)}
}

func (l moduleCatalogPlugins) ActivePlugins(ctx context.Context) ([]handlers.PluginCatalogEntry, error) {
	recs, err := l.store.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]handlers.PluginCatalogEntry, 0, len(recs))
	for _, s := range recs {
		out = append(out, handlers.PluginCatalogEntry{
			Namespace:   s.Namespace,
			Name:        s.Name,
			Ref:         s.Ref,
			ManifestRaw: s.ManifestRaw,
		})
	}
	return out, nil
}

// setupMetricsRegistry — dedicated Prometheus registry + регистрация всех
// per-подсистемных метрик (http/grpc/scenario/render/vault). Один экземпляр
// на процесс. RegisterReaperMetrics НЕ здесь — оно условное в ветке Reaper на
// тот же registry.
func (d *daemon) setupMetricsRegistry(_ context.Context) error {
	// Observability-стек: dedicated Prometheus registry с go/process-
	// collectors (компонент-агностичный core). keeper_http_*-метрики
	// регистрируются отдельно поверх него. Один экземпляр на keeper-процесс,
	// шарится между middleware на /v1/* (инструментация) и выделенным
	// metrics-listener-ом (exposition).
	metricsReg := obs.NewRegistry()
	d.metricsReg = metricsReg
	d.httpMetrics = obs.RegisterHTTPMetrics(metricsReg)
	// keeper_grpc_*-метрики EventStream-подсистемы регистрируются на тот же
	// registry; дескриптор шарится между Outbound (dispatch) и
	// EventStream-handler-ом (streams/messages). Безусловно — EventStream-
	// listener поднимается всегда (в отличие от опционального Reaper-а).
	d.grpcMetrics = keepergrpc.RegisterGRPCMetrics(metricsReg)
	// keeper_scenario_*-метрики scenario-runner-а — на тот же registry;
	// дескриптор инжектится в scenario.Deps.Metrics (один Registry на процесс).
	d.scenarioMetrics = scenario.RegisterScenarioMetrics(metricsReg)
	// keeper_render_*-метрики render-пайплайна — на тот же registry; дескриптор
	// инжектится в render.NewPipeline ниже (горячий путь CEL+template-рендера).
	d.renderMetrics = render.RegisterRenderMetrics(metricsReg)
	// keeper_vault_*-метрики чтения KV — на тот же registry; vc поднят выше
	// (до создания registry, т.к. нужен для DSN-резолва), поэтому метрики
	// подключаются сеттером SetMetrics здесь, после регистрации.
	d.vc.SetMetrics(keepervault.RegisterVaultMetrics(metricsReg))
	// keeper_rbac_*-метрики RBAC-подсистемы — на тот же registry; rbacHolder
	// поднят в setupRBAC ДО создания registry (init-order), поэтому метрики
	// подключаются сеттером SetMetrics здесь, после регистрации (паттерн vault).
	d.rbacHolder.SetMetrics(rbac.RegisterRBACMetrics(metricsReg))
	// keeper_serviceregistry_*-метрики снимка реестра Service-ов — на тот же
	// registry; serviceHolder поднят в setupServiceRegistry ДО создания registry
	// (init-order), метрики подключаются сеттером SetMetrics здесь (паттерн rbac).
	d.serviceHolder.SetMetrics(serviceregistry.RegisterRegistryMetrics(metricsReg))
	// keeper_augur_*-метрики брокера AugurRequest — на тот же registry; дескриптор
	// сохраняется в d.augurMetrics и инжектится в keepergrpc.AugurDeps.Metrics в
	// setupGRPCEventStream (идёт позже по steps, паттерн grpcMetrics/scenarioMetrics).
	d.augurMetrics = keeperaugur.RegisterBrokerMetrics(metricsReg)
	// keeper_oracle_*-метрики reactor-роутера Oracle (ADR-030 S4) — на тот же
	// registry; дескриптор сохраняется в d.oracleMetrics и инжектится в
	// keepergrpc.OracleDeps.Metrics в setupGRPCEventStream (паттерн augurMetrics).
	d.oracleMetrics = oracle.RegisterOracleMetrics(metricsReg)
	// keeper_conclave_instances — gauge числа живых keeper-инстансов в Conclave
	// (реестр presence в Redis, ADR-006 amend, soul-shedding S1). Обновляется
	// renewal-goroutine-ой setupConclave (идёт позже, после setupRedis) по
	// LiveKIDs. Регистрируется безусловно (registry всегда поднят); при
	// отсутствии Redis renewal-goroutine не стартует → gauge остаётся 0.
	d.conclaveInstances = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "keeper_conclave_instances",
		Help: "Текущее число живых keeper-инстансов в Conclave (presence-реестр в Redis).",
	})
	metricsReg.Registerer().MustRegister(d.conclaveInstances)
	// keeper_watchman_*-метрики (изоляция-детект + soul-shedding S2): gauge
	// isolated (1/0) + счётчик закрытых shedding-ом стримов. Регистрируется
	// безусловно (registry всегда поднят); инжектится в watchman.New в
	// setupWatchman (паттерн conclaveInstances).
	d.watchmanMetrics = registerWatchmanMetrics(metricsReg)
	// keeper_toll_*-метрики + keeper_cluster_degraded (Toll cluster-detector,
	// ADR-038): per-instance Watcher (counter disconnects + warmup/graceful
	// skipped) + cluster-level Leader (gauge cluster_degraded + gauge
	// leader_active). Регистрируется безусловно (registry всегда поднят);
	// инжектится в toll.NewWatcher / toll.NewLeader в setupToll (паттерн
	// watchmanMetrics). При выключенном Toll (отсутствие Redis или
	// `toll.enabled: false`) метрики остаются на 0 — это валидный сигнал
	// «детектор не активен», без двоякости.
	d.tollMetrics = toll.RegisterMetrics(metricsReg)
	// keeper_tempo_*-метрики (Tempo per-AID rate-limiter, ADR-050(g)): counters
	// allowed/rejected с разрезом по endpoint (= bucket-имя voyage_create), БЕЗ
	// aid-лейбла (кардинальность). Регистрируется безусловно (registry всегда
	// поднят); инжектится в api.Deps.TempoMetrics в setupAPIServer (паттерн
	// tollMetrics). При выключенном Tempo (нет Redis / enabled=false) counters
	// остаются на 0 — валидный сигнал «лимитер не активен».
	d.tempoMetrics = api.RegisterTempoMetrics(metricsReg)
	// keeper_herald_*-метрики notification-dispatcher/tap-а (ADR-052(c)): drop-
	// counter переполнения буфера tap-а + dispatch/matches/errors. tap собран в
	// setupAudit ДО создания registry (init-order: setupAudit идёт раньше
	// setupMetricsRegistry), поэтому метрики прокидываются сеттером SetMetrics
	// здесь (паттерн vault/rbac). heraldTap всегда non-nil (сборка в setupAudit
	// безотказна — см. там); SetMetrics всё равно nil-safe на случай S3+, когда
	// сборка сможет фейлиться и оставить heraldTap == nil.
	heraldMetrics := herald.RegisterDispatcherMetrics(metricsReg)
	d.heraldTap.SetMetrics(heraldMetrics)
	// keeper_herald_delivery_*-метрики claim-queue worker-а доставки (ADR-052(d),
	// S3): attempts/succeeded/failed/retries по herald-каналу. Регистрируется
	// безусловно (registry всегда поднят); инжектится в DeliveryWorker в
	// setupHeraldDelivery (после setupRedis). При отсутствии Redis (доставка
	// деградирует) counters остаются на 0.
	d.heraldDeliveryMetrics = herald.RegisterDeliveryMetrics(metricsReg)
	// keeper_push_*-метрики (S7-3 multi-CA: counter матчей host-CA с разрезом
	// по `ca_name`). Регистрируется безусловно (registry всегда поднят);
	// инжектится в push.Deps.Metrics в setupPushDispatchers (паттерн
	// watchmanMetrics/tollMetrics). При выключенном push дескриптор остаётся
	// зарегистрированным, но никто не пишет — counter остаётся на 0.
	d.pushMetrics = push.RegisterMetrics(metricsReg)
	// keeper_sigil_signing_keys_active — gauge active-ключей подписи (R3-S7);
	// sigilKeySvc поднят в setupSigil ДО registry (init-order), метрики — сеттером
	// (паттерн vault/rbac). nil при выключенном Sigil. Стартовое значение
	// проставляет первая мутация (afterMutation); при выключенном — gauge остаётся 0.
	if d.sigilKeySvc != nil {
		// Один дескриптор keeper_sigil_* шарится: gauge active-ключей обновляет
		// KeyService (afterMutation), re-broadcast-наблюдаемость (счётчик проходов
		// + delivered) — daemon из reloadAnchors. Сохраняем в d.sigilKeyMetrics,
		// чтобы оба писали в ту же серию.
		d.sigilKeyMetrics = sigil.RegisterKeyMetrics(metricsReg)
		d.sigilKeySvc.SetMetrics(d.sigilKeyMetrics)
		// Стартовое значение gauge: читаем active-ключи сейчас (одноразово),
		// чтобы метрика была актуальна до первой ротации. Best-effort.
		d.sigilKeySvc.PrimeActiveGauge(context.Background())
	}
	return nil
}

// setupMetricsListener — выделенный `/metrics` listener на listen.metrics.addr
// (ADR-024) с опциональным basic-auth. Cleanup — graceful Shutdown.
func (d *daemon) setupMetricsListener(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// `/metrics` — выделенный listener на `listen.metrics.addr` (ADR-024,
	// PM-decision Slice 1): эндпоинт снят с openapi-роутера, чтобы scrape
	// шёл на отдельный порт (обычно 9090) без auth-chain Operator API.
	// keeper_http_*/keeper_reaper_* (последние регистрируются ниже на тот же
	// registry) экспонируются здесь же — registry один.
	//
	// Опц. basic-auth: при metrics.auth.basic.enabled пароль резолвится тем
	// же keeper-vault-клиентом (что читает signing-key) из password_ref;
	// иначе auth=nil (открытый эндпоинт). Резолв — на keeper-стороне, helper
	// получает готовые креды (ADR-011: shared/obs не тянет vault).
	metricsAuth, err := resolveMetricsBasicAuth(ctx, d.vc, cfg.Metrics)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: resolve metrics basic-auth: %v\n", err)
		return errSetupFailed
	}
	metricsSrv, err := obs.ServeMetrics(cfg.Listen.Metrics.Addr, d.metricsReg, metricsAuth)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: start metrics listener: %v\n", err)
		return errSetupFailed
	}
	logger.Info("keeper run: metrics listener up",
		slog.String("addr", metricsSrv.Addr()),
		slog.Bool("basic_auth", metricsAuth != nil))
	d.cleanups.push(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := metricsSrv.Shutdown(shutCtx); err != nil {
			logger.Warn("metrics listener shutdown returned error", slog.Any("error", err))
		}
	})
	return nil
}

// setupOTel — OTel-провайдер (ADR-024), service.name="keeper". Cleanup —
// Shutdown (no-op-провайдер при otel.enabled=false → единообразный teardown).
func (d *daemon) setupOTel(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// OTel-провайдер (ADR-024): service.name="keeper" + кастомный
	// soulstack.kid из конфига. Trace-export при otel.enabled+endpoint;
	// иначе no-op-провайдер (cleanup Shutdown единообразен). Setup один раз
	// за процесс — otel.* restart-required, hot-reload его не трогает.
	otelProvider, err := obs.SetupOTel(ctx, obs.OTelConfig{
		Enabled:       cfg.OTel != nil && cfg.OTel.Enabled,
		Endpoint:      otelEndpoint(cfg.OTel),
		ServiceName:   "keeper",
		ResourceAttrs: map[string]string{"soulstack.kid": cfg.KID},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: setup OTel: %v\n", err)
		return errSetupFailed
	}
	d.cleanups.push(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := otelProvider.Shutdown(shutCtx); err != nil {
			logger.Warn("OTel provider shutdown returned error", slog.Any("error", err))
		}
	})
	return nil
}

// setupScenarioDeps — зависимости scenario-runner-а (git-loader, topology- и
// essence-резолверы, CEL-engine, render-pipeline, service-registry, destiny-
// source). Сам Runner собирается в setupGRPCEventStream (после Outbound).
func (d *daemon) setupScenarioDeps(_ context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// scenario-runner-зависимости (M2.x slice .g): git-loader, topology- и
	// essence-резолверы, render-pipeline. Сам Runner собирается ниже (после
	// Outbound — он dispatch-ит ApplyRequest через него). api.NewServer тоже
	// собирается ниже, чтобы инжектить Runner + ServiceRegistry в Create-handler.
	d.serviceLoader = artifact.NewServiceLoader(serviceCacheRoot(cfg), logger)

	// TTL-кеш scenario-listing-а для `/v1/services/{name}/scenarios` (UI). Lister
	// под кешем грузит снапшот через d.serviceLoader.Load(ServiceRef{...}) →
	// artifact.ListScenarios(snapshot.LocalDir). TTL по умолчанию (ScenariosTTL=60s).
	// Создаётся ПОСЛЕ d.serviceLoader — переиспользует общий cacheRoot.
	d.serviceScenarios = serviceregistry.NewScenariosCache(
		serviceregistry.ScenarioListerFunc(func(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error) {
			art, err := d.serviceLoader.Load(ctx, artifact.ServiceRef{Name: name, Git: gitURL, Ref: ref})
			if err != nil {
				return nil, err
			}
			return artifact.ListScenarios(art.LocalDir, logger)
		}),
		0, // 0 → дефолтный ScenariosTTL
	)

	// TTL-кеш state_schema-метаданных для `/v1/services/{name}/state-schema`
	// (UI Schema explorer). Lister грузит снапшот через d.serviceLoader.Load →
	// artifact.ListStateSchema(snapshot.LocalDir). Parity с serviceScenarios:
	// тот же loader, тот же TTL (StateSchemaTTL=60s).
	d.serviceStateSchema = serviceregistry.NewStateSchemaCache(
		serviceregistry.StateSchemaListerFunc(func(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error) {
			art, err := d.serviceLoader.Load(ctx, artifact.ServiceRef{Name: name, Git: gitURL, Ref: ref})
			if err != nil {
				return nil, err
			}
			return artifact.ListStateSchema(art.LocalDir, logger)
		}),
		0, // 0 → дефолтный StateSchemaTTL
	)

	// TTL-кеш git-зависимостей для `/v1/services/{name}/dependencies`
	// (UI Service Detail). Lister грузит снапшот через d.serviceLoader.Load →
	// artifact.ListDependencies(snapshot.LocalDir). Parity с serviceStateSchema:
	// тот же loader, тот же TTL (DependenciesTTL=60s).
	d.serviceDependencies = serviceregistry.NewDependenciesCache(
		serviceregistry.DependenciesListerFunc(func(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error) {
			art, err := d.serviceLoader.Load(ctx, artifact.ServiceRef{Name: name, Git: gitURL, Ref: ref})
			if err != nil {
				return nil, err
			}
			return artifact.ListDependencies(art.LocalDir, logger)
		}),
		0, // 0 → дефолтный DependenciesTTL
	)

	// topologyResolver собирается ниже, в setupGRPCEventStream: его presence-фаза
	// (Variant A, ADR-006(a)) деривирует «Soul online» из живого Redis SID-lease,
	// а d.redisClient поднимается только в setupRedis (после этого шага).
	d.essenceResolver = essence.NewResolver(logger)
	celEngine, err := cel.New(cel.WithVault(d.vc))
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build CEL engine: %v\n", err)
		return errSetupFailed
	}
	d.renderPipeline = render.NewPipeline(d.vc, celEngine, logger, d.renderMetrics)
	// Реестр Service-ов и скаляр default_destiny_source перенесены в Postgres
	// (ADR-029): источник правды — БД, потребители читают runtime-снимок
	// serviceHolder (TTL-poll + pub/sub-инвалидация, поднят в setupServiceRegistry).
	// Resolve — синхронный, lock-free; hot-reload реестра/скаляра прозрачен.
	d.serviceRegistry = scenario.NewServiceRegistry(d.serviceHolder)
	// Источник destiny-артефактов для apply:destiny (ADR-009): git-URL —
	// default_destiny_source + {name} (читается ЛЕНИВО из serviceHolder, чтобы
	// hot-reload скаляра доезжал), ref — service.yml::destiny[].
	destinyLoader := artifact.NewDestinyLoader(destinyCacheRoot(cfg), logger)
	d.destinySource = scenario.NewDestinySource(destinyLoader, d.serviceHolder)
	return nil
}

// setupPushOrchestrator — multi-host push-orchestrator (Variant C,
// docs/keeper/push.md). Зависит от setupScenarioDeps (renderPipeline +
// serviceHolder через destinySource resolveURL-семантику) — в pipeline шагов
// идёт сразу после setupScenarioDeps.
//
// Поднимает:
//   - pushDestinyLoader — отдельный git-loader, шарящий тот же destinyCacheRoot,
//     что scenario.NewDestinySource (один кеш-снапшот на коммит, scenario- и
//     push-пути дёргают одно и то же дерево);
//   - topologyResolver-bridge — нужен LoadByInventory, поэтому setupGRPCEventStream
//     должен быть после setupPushOrchestrator. Однако topologyResolver сейчас
//     создаётся в setupGRPCEventStream (там SoulLeaseChecker). В этом slice
//     setupPushOrchestrator только подготавливает loader+template; финальный
//     pushRun собирается в setupGRPCEventStream после поднятия topologyResolver
//     (новый под-шаг finalizePushOrchestrator ниже).
//
// pushDispatcher остаётся nil в этом slice — wire-up SshDispatcher (handshake/
// sigil-verify/GC через pluginhost, TargetResolver/HostKeyAuthority/Deliverer/
// Cleaner) — отдельный slice setupPushDispatchers (ждёт policy-решения по
// источнику ssh-target по SID). При nil pushDispatcher pushRun не собирается
// (api.Deps.PushRun=nil → роуты не подключаются).
func (d *daemon) setupPushOrchestrator(_ context.Context) error {
	d.pushDestinyLoader = artifact.NewDestinyLoader(destinyCacheRoot(d.cfg), d.logger)
	// pushDispatcher/pushCleaner намеренно остаются nil: SshDispatcher-wire-up
	// — отдельный slice (см. doc выше). При появлении dispatcher-а его в это
	// поле подставит setupPushDispatchers (либо daemon-конструктор для тестов).
	return nil
}

// setupPushDispatchers — wire-up SshDispatcher (S6 pilot + S7-1 PG-canon,
// [ADR-032 amendment 2026-05-26]). Поднимает:
//
//  1. PGFallbackTargetResolver: PG-first резолв ssh_target по PK souls.sid +
//     optional fallback на keeper.yml::push.targets[] под флагом
//     `push.allow_legacy_push_targets` (1-release WARN deprecation window);
//  2. host-CA из Vault по `push.host_ca_ref` (PEM SSH public key, поле
//     `public_key`); fail-fast на ошибке резолва;
//  3. one-shot Spawn первого дискаверенного SshProvider-плагина (single-
//     provider pilot; multi-provider routing — S7) с env-payload params из
//     `push.providers[].params` (ADR-020 amendment l, env-convention);
//  4. SshDispatcher с ShaDeliverer/ShaCleaner (S1/S5).
//
// Gate-условия (fail-open, без ошибки старта): пустой `plugins.ssh_providers[]`,
// нет дискаверенных SshProvider-плагинов, отсутствие `push`-блока или
// `push.host_ca_ref` → push выключен, WARN в лог; `/v1/push/*` и
// `keeper.push.apply` вернут «не сконфигурировано» (api.Deps.PushRun=nil).
//
// Fail-closed условия (errSetupFailed): `push.host_ca_ref` задан, но Vault
// недоступен / поле отсутствует / битый PEM — оператор предписал push, не
// поднимаем тихо без host-CA (любой connect провалится с невнятной ошибкой).
//
// Cleanup: spawned SshProvider-плагин закрывается в LIFO ДО Redis/Pool —
// плагин держит unix-socket с keeper-host-стороны, разумно прибрать первым.
func (d *daemon) setupPushDispatchers(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	// Gate 1: каталог SshProvider-ов пуст → push отключён (нет плагинов, чтобы
	// аутентифицировать SSH-handshake). Это нормальный режим для pull-only
	// инсталляций — без ошибки старта.
	if cfg.Plugins == nil || len(cfg.Plugins.SSHProviders) == 0 {
		logger.Info("keeper run: push dispatcher disabled (plugins.ssh_providers[] не объявлены) — /v1/push/* и MCP keeper.push.apply вернут 'не сконфигурировано'")
		return nil
	}

	// Gate 2: после Discover/FilterByCatalog в setupCoreModules не осталось
	// SshProvider-плагинов (битый кеш / mismatch имён). WARN — оператор уже
	// видел warning из FilterByCatalog; здесь даём explicit-сообщение «push
	// поэтому не поднялся».
	if len(d.pushDiscoveredSsh) == 0 {
		logger.Warn("keeper run: push dispatcher disabled (нет дискаверенных SshProvider-плагинов в кеше) — /v1/push/* недоступен")
		return nil
	}

	// Gate 3: блок `push:` или host-CA отсутствует → push не сконфигурирован
	// оператором (ssh_providers могут быть в каталоге для будущего использования).
	// Без host-CA dispatcher не поднимается — это согласовано с security policy
	// (CA-signed host-cert verify обязателен). S7-3 ввёл multi-CA `host_ca_refs[]`;
	// устаревший singular `host_ca_ref` остаётся под 1-release WARN deprecation
	// window: при заполненном singular и пустом плюрале — auto-adapt singular в
	// singleton с auto-name `default` + одноразовый WARN.
	if cfg.Push == nil || (cfg.Push.HostCARef == "" && len(cfg.Push.HostCARefs) == 0) {
		logger.Warn("keeper run: push dispatcher disabled (push.host_ca_refs[] / host_ca_ref не заданы) — /v1/push/* недоступен; настройте keeper.yml::push.host_ca_refs[] для включения")
		return nil
	}

	hostCARefs := cfg.Push.HostCARefs
	if len(hostCARefs) == 0 {
		// S7-3 backward-compat: singular auto-adapt в singleton.
		logger.Warn("keeper run: push.host_ca_ref deprecated (S7-3 ADR-032 amendment 2026-05-26); auto-adapted в host_ca_refs[0] с name='default'. Замените на push.host_ca_refs[{ref, name}] до hard-cut.",
			slog.String("singular_ref", cfg.Push.HostCARef))
		hostCARefs = []config.KeeperPushCARef{{
			Ref:  cfg.Push.HostCARef,
			Name: config.DefaultHostCAName,
		}}
	}

	// Fail-fast: host-CA задан, но не резолвится. push-флоу без него
	// бессмыслен (любой connect провалится), молча отключать нельзя — оператор
	// явно объявил push.
	hostAuthorities, err := push.LoadHostCAs(ctx, d.vc, hostCARefs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: push dispatcher resolve host_ca_refs: %v\n", err)
		return errSetupFailed
	}

	// P2 W-2 multi-provider: eager spawn ВСЕХ дискаверенных SshProvider-плагинов.
	// Каждый плагин получает свои env-payload params (PG-first резолв через
	// push_providers, optional legacy fallback на keeper.yml::push.providers[]).
	// Шейринг одного respawner-а и одного providerResolver-а на все плагины —
	// они опираются только на (имя плагина, текущая PG-row).
	providerResolver := &push.PGFallbackProviderResolver{
		Reader:      push.NewPGPushProviderReader(d.pool),
		Fallback:    push.NewLegacyConfigProvidersFallback(cfg.Push.Providers),
		AllowLegacy: cfg.Push.AllowLegacyPushProviders,
		Logger:      logger.With(slog.String("component", "push-provider-resolver")),
	}

	providers := make(map[string]push.ProviderEntry, len(d.pushDiscoveredSsh))
	spawnedPluginNames := make([]string, 0, len(d.pushDiscoveredSsh))
	for _, dd := range d.pushDiscoveredSsh {
		if dd.Manifest == nil {
			fmt.Fprintln(os.Stderr, "keeper run: push dispatcher: discovered SshProvider без manifest (программная ошибка discovery)")
			return errSetupFailed
		}
		pluginName := dd.Manifest.Name

		resolvedParams, resolveErr := providerResolver.ResolveParams(ctx, pluginName)
		if resolveErr != nil && !errors.Is(resolveErr, push.ErrPushProviderNotConfigured) {
			fmt.Fprintf(os.Stderr, "keeper run: push dispatcher resolve push_providers %q: %v\n", pluginName, resolveErr)
			return errSetupFailed
		}
		spawnOpts, _, optErr := buildPushSpawnOptsFromParams(pluginName, resolvedParams)
		if optErr != nil {
			fmt.Fprintf(os.Stderr, "keeper run: push dispatcher build env-payload %q: %v\n", pluginName, optErr)
			return errSetupFailed
		}

		plugin, err := d.pushPluginHost.Spawn(ctx, dd, spawnOpts...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "keeper run: push dispatcher spawn %s: %v\n", dd.Manifest.Address(), err)
			return errSetupFailed
		}
		sshPlugin, err := pluginhost.NewSshProviderPlugin(plugin)
		if err != nil {
			_ = plugin.Close()
			fmt.Fprintf(os.Stderr, "keeper run: push dispatcher wrap %s: %v\n", dd.Manifest.Address(), err)
			return errSetupFailed
		}
		providers[pluginName] = push.ProviderEntry{Provider: sshPlugin, Closer: sshPlugin}
		spawnedPluginNames = append(spawnedPluginNames, pluginName)

		// Cleanup в LIFO: каждый spawned-плагин закрывается отдельной функцией,
		// порядок не критичен (sharedhost.BasePlugin.Close идемпотентен).
		closer := sshPlugin
		d.cleanups.push(func() {
			if cerr := closer.Close(); cerr != nil {
				logger.Warn("keeper run: push SshProvider plugin close returned error",
					slog.String("plugin", pluginName), slog.Any("error", cerr))
			}
		})
	}

	// d.pushSshPlugin сохраняем как «первый из карты» для backward-compat
	// диагностики (LegacyAccessor). Не используется в hot path.
	if len(providers) > 0 {
		for _, entry := range providers {
			if e, ok := entry.Closer.(*pluginhost.SshProviderPlugin); ok {
				d.pushSshPlugin = e
				break
			}
		}
	}

	// S7-1 wire-up: PG-first резолвер ssh_target поверх souls.ssh_target jsonb;
	// keeper.yml::push.targets[] доступен как fallback под флагом
	// push.allow_legacy_push_targets (1-release WARN deprecation window,
	// [ADR-032 amendment 2026-05-26]).
	configResolver := push.NewConfigTargetResolver(cfg.Push.Targets)
	targetResolver := &push.PGFallbackTargetResolver{
		Reader:      push.NewPGTargetReader(d.pool),
		Fallback:    configResolver,
		AllowLegacy: cfg.Push.AllowLegacyPushTargets,
		Logger:      logger.With(slog.String("component", "push-target-resolver")),
	}
	// P2 W-2: respawner поднимается одним экземпляром на весь набор; находит
	// discovered по имени и обновляет конкретную запись карты.
	respawner := newPushProviderRespawner(d.pushPluginHost, d.pushDiscoveredSsh, providerResolver,
		logger.With(slog.String("component", "push-provider-respawner")))
	dispatcher, err := push.NewSshDispatcher(push.Deps{
		Providers:       providers,
		Respawner:       respawner,
		Targets:         targetResolver,
		Souls:           push.NewPGSoulLookup(d.pool),
		HostAuthorities: hostAuthorities,
		Metrics:         d.pushMetrics,
		Deliverer:       push.NewShaDeliverer(),
		Cleaner:         push.NewShaCleaner(),
		Logger:          logger.With(slog.String("component", "push-dispatcher")),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: push dispatcher build: %v\n", err)
		return errSetupFailed
	}
	d.pushDispatcher = dispatcher
	d.pushCleaner = dispatcher
	d.pushSshDispatcher = dispatcher

	logger.Info("keeper run: push dispatcher ready (P2 multi-provider + S7-1 PG-canon + S6 legacy fallback + S7-3 multi-CA)",
		slog.Any("providers", spawnedPluginNames),
		slog.Int("legacy_targets", len(cfg.Push.Targets)),
		slog.Bool("allow_legacy_push_targets", cfg.Push.AllowLegacyPushTargets),
		slog.Bool("allow_legacy_push_providers", cfg.Push.AllowLegacyPushProviders),
		slog.Int("host_authorities", len(hostAuthorities)),
		slog.String("cluster_default_provider", cfg.Push.ClusterDefaultProvider),
		slog.Int("coven_default_providers", len(cfg.Push.CovenDefaultProviders)))
	return nil
}

// setupPushProviderSvc — CRUD-фасад реестра push_providers + Redis-publisher
// для cluster-wide invalidate (ADR-032 amendment 2026-05-26, S7-2).
//
// Поднимается всегда, даже если push-dispatcher выключен: оператор должен
// иметь возможность настроить провайдеров до включения push-инфраструктуры
// (REST /v1/push-providers / MCP keeper.push-provider.* работают независимо).
//
// Publisher: при non-nil d.redisClient — реальный (REST/MCP-мутация публикует
// в `push-providers:changed`, любая нода re-spawn-ит плагин на ближайшем
// RPC). При nil — NopPublisher (single-instance dev: spawn-on-change работает
// только локально через рестарт).
//
// Subscriber: при non-nil d.redisClient поднимает goroutine-listener; cluster-
// wide уведомления приходят сюда (сейчас только логируются — фактический
// re-spawn плагина внутри SshDispatcher — отдельный slice). При nil — skip.
func (d *daemon) setupPushProviderSvc(ctx context.Context) error {
	logger := d.logger
	var publisher pushprovider.RedisPublisher
	if d.redisClient != nil {
		publisher = &daemonPushProviderPublisher{redis: d.redisClient}
	}
	svc, err := pushprovider.NewService(pushprovider.ServiceDeps{
		Pool:      d.pool,
		Publisher: publisher,
		Logger:    logger.With(slog.String("component", "push-provider-svc")),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build push-provider service: %v\n", err)
		return errSetupFailed
	}
	d.pushProviderSvc = svc

	if d.redisClient != nil {
		sub, err := keeperredis.SubscribePushProvidersChanged(ctx, d.redisClient,
			logger.With(slog.String("component", "push-provider-invalidation")))
		if err != nil {
			// Subscription-сбой не критичен: CRUD работает, просто spawn-on-
			// change не реагирует на cluster-wide мутации (только локальные).
			logger.Warn("keeper run: subscribe push-providers:changed failed; cluster-wide invalidate not active",
				slog.Any("error", err))
		} else {
			d.pushProviderInvalidation = sub
			go d.runPushProviderInvalidationListener(sub, logger)
			d.cleanups.push(func() {
				if cerr := sub.Close(); cerr != nil {
					logger.Warn("keeper run: push-provider invalidation subscription close",
						slog.Any("error", cerr))
				}
			})
		}
	}

	logger.Info("keeper run: push-provider service ready (S7-2)",
		slog.Bool("redis_publisher", d.redisClient != nil),
		slog.Bool("redis_subscription", d.pushProviderInvalidation != nil))
	return nil
}

// runLegacyAutoImport — opt-in one-shot миграция inline-`keeper.yml::push`-
// блоков в PG-источники (ADR-032 amendment 2026-05-26, S7-4).
//
// Gate: оба флага `push.auto_import_legacy_targets` и
// `push.auto_import_legacy_providers` false → no-op. При хотя бы одном true
// поднимает [push.AutoImporter] поверх `d.pool` и проходит соответствующий блок
// один раз. Идемпотентность — на уровне импортёра (PG-row уже есть → skip).
//
// Failure-семантика: PG read/write fail → errSetupFailed. Оператор явно
// включил флаг, не должен молча получить «половина импортирована, половина
// нет» — пусть починит PG и перезапустит (повтор подхватит остаток).
// audit-write fail внутри импорта — best-effort (см. AutoImporter.writeAudit).
//
// Порядок: после [setupPushProviderSvc] (нужен подтверждённый PG-state +
// d.auditWriter); ДО setupAPIServer (REST `/v1/push-providers` уже сможет
// показать импортированные строки). Не зависит от setupPushDispatchers
// (push-dispatcher может быть выключен — auto-import всё равно подготовит PG
// под будущее включение push-flow).
func (d *daemon) runLegacyAutoImport(ctx context.Context) error {
	cfg := d.cfg
	if cfg.Push == nil {
		return nil
	}
	if !cfg.Push.AutoImportLegacyTargets && !cfg.Push.AutoImportLegacyProviders {
		return nil
	}
	logger := d.logger.With(slog.String("component", "push-auto-import"))

	targetsRW := push.NewPGTargetReadWriter(d.pool)
	providersRW := push.NewPGProviderReadWriter(d.pool)
	importer, err := push.NewAutoImporter(push.AutoImporterDeps{
		TargetReader:   targetsRW,
		TargetWriter:   targetsRW,
		ProviderReader: providersRW,
		ProviderWriter: providersRW,
		Auditor:        d.auditWriter,
		Logger:         logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build push auto-importer: %v\n", err)
		return errSetupFailed
	}
	if err := importer.ImportLegacyOnStart(ctx, *cfg.Push); err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: %v\n", err)
		return errSetupFailed
	}
	logger.Info("keeper run: S7-4 legacy auto-import completed",
		slog.Bool("auto_import_targets", cfg.Push.AutoImportLegacyTargets),
		slog.Bool("auto_import_providers", cfg.Push.AutoImportLegacyProviders))
	return nil
}

// runPushProviderInvalidationListener — фоновая goroutine, читающая
// invalidate-сообщения из Redis (`push-providers:changed`) и делегирующая
// фактический re-spawn `SshDispatcher.RefreshProvider` (ADR-032 amendment
// 2026-05-27, S7-2 closure).
//
// Семантика:
//   - pushSshDispatcher == nil → push-флоу не поднят (gate в
//     setupPushDispatchers), просто дренируем канал, чтобы Close прошёл
//     корректно. Без RefreshProvider никакая повторная подписка смысла не
//     имеет.
//   - push.ErrRespawnNotSupported / wrong-name → noop (не наш плагин или
//     respawner не сконфигурирован); WARN-лог, продолжаем.
//   - реальная ошибка spawn-а → ERROR-лог, dispatcher остаётся в degraded
//     state; следующая мутация / рестарт keeper-а починят.
func (d *daemon) runPushProviderInvalidationListener(sub *keeperredis.PushProvidersChangedSubscription, logger *slog.Logger) {
	for ev := range sub.Channel() {
		logger.Info("keeper run: push-providers:changed received",
			slog.String("provider", ev.Name),
			slog.Time("at", ev.At))
		if d.pushSshDispatcher == nil {
			continue
		}
		// Без request-ctx у listener-а: используем фоновой context. Spawn
		// плагина не должен висеть бесконечно — pluginhost.Host имеет
		// собственный StartupTimeout, поэтому отдельный тайм-аут здесь не
		// добавляем (любой hang будет видим в логах handshake-а).
		if err := d.pushSshDispatcher.RefreshProvider(context.Background(), ev.Name); err != nil {
			if errors.Is(err, push.ErrRespawnNotSupported) {
				logger.Warn("keeper run: push provider re-spawn not supported (no respawner configured)",
					slog.String("provider", ev.Name))
				continue
			}
			logger.Error("keeper run: push provider re-spawn failed",
				slog.String("provider", ev.Name),
				slog.Any("error", err))
		}
	}
}

// daemonPushProviderPublisher — bridge между pushprovider.RedisPublisher и
// keeperredis.PublishPushProvidersChanged. Тонкая адаптация, чтобы
// pushprovider-пакет не зависел напрямую от keeperredis.
type daemonPushProviderPublisher struct {
	redis *keeperredis.Client
}

func (p *daemonPushProviderPublisher) PublishPushProvidersChanged(ctx context.Context, providerName string) error {
	_, err := keeperredis.PublishPushProvidersChanged(ctx, p.redis, providerName)
	return err
}

// setupHeraldSvc — CRUD-фасад реестров heralds/tidings + двухуровневая
// инвалидация снимка Tiding-правил dispatcher-а (ADR-052, S4).
//
// In-process invalidate — d.heraldDispatcher (собран в setupAudit): мутация на
// этой ноде мгновенно сбрасывает её кэш. Cross-keeper — Redis-publisher
// (`herald:invalidate`): другая нода по подписке дёргает свой InvalidateRules.
//
// Поднимается всегда (как setupPushProviderSvc): оператор управляет каналами/
// правилами независимо от доставки (S3). heraldDispatcher может быть nil
// (fail-open ветка setupAudit) — тогда in-process invalidate деградирует на
// TTL-сходимость (NewService подменит nil-Invalidator no-op-ом).
//
// Порядок: ПОСЛЕ setupAudit (d.heraldDispatcher) и setupRedis (d.redisClient для
// publisher); ДО setupAPIServer / setupMCPServer (читают d.heraldSvc).
func (d *daemon) setupHeraldSvc(ctx context.Context) error {
	logger := d.logger
	var redis herald.RedisInvalidator
	if d.redisClient != nil {
		redis = &daemonHeraldInvalidator{redis: d.redisClient}
	}
	// nil-Invalidator (heraldDispatcher==nil) NewService подменит no-op-ом —
	// явный nil интерфейса не передаём (typed-nil-pitfall): только если non-nil.
	var inv herald.Invalidator
	if d.heraldDispatcher != nil {
		inv = d.heraldDispatcher
	}
	svc, err := herald.NewService(herald.ServiceDeps{
		Pool:        d.pool,
		Invalidator: inv,
		Redis:       redis,
		Logger:      logger.With(slog.String("component", "herald-svc")),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build herald service: %v\n", err)
		return errSetupFailed
	}
	d.heraldSvc = svc

	if d.redisClient != nil {
		sub, err := keeperredis.SubscribeHeraldInvalidate(ctx, d.redisClient,
			logger.With(slog.String("component", "herald-invalidation")))
		if err != nil {
			// Subscription-сбой не критичен: CRUD работает, кросс-нодовая
			// сходимость деградирует на TTL-poll (DefaultRuleCacheTTL).
			logger.Warn("keeper run: subscribe herald:invalidate failed; cluster-wide invalidate not active",
				slog.Any("error", err))
		} else {
			d.heraldInvalidation = sub
			go d.runHeraldInvalidationListener(sub, logger)
			d.cleanups.push(func() {
				if cerr := sub.Close(); cerr != nil {
					logger.Warn("keeper run: herald invalidation subscription close",
						slog.Any("error", cerr))
				}
			})
		}
	}

	logger.Info("keeper run: herald service ready (S4)",
		slog.Bool("redis_publisher", d.redisClient != nil),
		slog.Bool("redis_subscription", d.heraldInvalidation != nil),
		slog.Bool("in_process_invalidate", d.heraldDispatcher != nil))
	return nil
}

// runHeraldInvalidationListener — фоновая goroutine, читающая invalidate-
// сообщения из Redis (`herald:invalidate`) и сбрасывающая снимок правил
// dispatcher-а этой ноды (cross-keeper-сходимость, ADR-052 S4).
func (d *daemon) runHeraldInvalidationListener(sub *keeperredis.HeraldInvalidateSubscription, logger *slog.Logger) {
	for ev := range sub.Channel() {
		logger.Debug("keeper run: herald:invalidate received",
			slog.String("name", ev.Name), slog.Time("at", ev.At))
		// nil-safe: heraldDispatcher может быть nil (fail-open setupAudit).
		d.heraldDispatcher.InvalidateRules()
	}
}

// daemonHeraldInvalidator — bridge между herald.RedisInvalidator и
// keeperredis.PublishHeraldInvalidate. Тонкая адаптация, чтобы herald-пакет не
// зависел напрямую от keeperredis.
type daemonHeraldInvalidator struct {
	redis *keeperredis.Client
}

func (p *daemonHeraldInvalidator) PublishHeraldInvalidate(ctx context.Context, name string) error {
	_, err := keeperredis.PublishHeraldInvalidate(ctx, p.redis, name)
	return err
}

// buildPushSpawnOpts собирает [pluginhost.SpawnOption] для SshProvider-плагина:
// при наличии записи `push.providers[].name == pluginName` сериализует `params`
// в JSON и кладёт в env-переменную `SOUL_SSH_<UPPER_SNAKE(pluginName)>_PARAMS`
// (ADR-020 amendment l). Возвращает (opts, envName, error); envName пуст, если
// для плагина не было записи или params пуст (диагностика в логе).
//
// Legacy-форма для совместимости с unit-тестами push_dispatchers_test.go;
// новый код использует [buildPushSpawnOptsFromParams] поверх PG-резолва
// (S7-2 wire-up).
func buildPushSpawnOpts(providers []config.KeeperPushProvider, pluginName string) ([]pluginhost.SpawnOption, string, error) {
	var params map[string]any
	for _, p := range providers {
		if p.Name == pluginName {
			params = p.Params
			break
		}
	}
	return buildPushSpawnOptsFromParams(pluginName, params)
}

// buildPushSpawnOptsFromParams — резолв-agnostic форма
// [buildPushSpawnOpts]: получает уже резолвенные `params` и собирает
// env-payload. Используется S7-2 wire-up-ом после PGFallbackProviderResolver.
func buildPushSpawnOptsFromParams(pluginName string, params map[string]any) ([]pluginhost.SpawnOption, string, error) {
	if len(params) == 0 {
		return nil, "", nil
	}
	payload, err := json.Marshal(params)
	if err != nil {
		// Не должно случаться на провалидированных map[string]any (YAML-decoder
		// гарантирует JSON-совместимые типы), но держим инвариант.
		return nil, "", fmt.Errorf("marshal push.providers[%q].params: %w", pluginName, err)
	}
	envName := pushParamsEnvName(pluginName)
	return []pluginhost.SpawnOption{pluginhost.WithEnv([]string{envName + "=" + string(payload)})}, envName, nil
}

// pushParamsEnvName преобразует имя плагина в env-имя `SOUL_SSH_<UPPER_SNAKE>_PARAMS`
// (ADR-020 amendment l). Маппинг: `vault-bastion` → `SOUL_SSH_VAULT_BASTION_PARAMS`.
// Допускаются буквы/цифры/дефис в kebab-case (валидируется schema-фазой
// kind:ssh_provider manifest); прочие символы маппятся в `_` defensively.
func pushParamsEnvName(pluginName string) string {
	var b strings.Builder
	b.Grow(len("SOUL_SSH__PARAMS") + len(pluginName))
	b.WriteString("SOUL_SSH_")
	for _, r := range pluginName {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteString("_PARAMS")
	return b.String()
}

// finalizePushOrchestrator собирает *pushorch.PushRun из ранее подготовленных
// зависимостей. Вынесен в отдельный шаг (после setupGRPCEventStream), потому
// что topologyResolver появляется в setupGRPCEventStream вместе с SoulLeaseChecker.
//
// Gate: при отсутствии pushDispatcher pushRun остаётся nil — push.*-роуты/tool
// не подключатся (api.Deps.PushRun=nil → router пропускает блок /v1/push, MCP
// keeper.push.apply возвращает «не сконфигурировано»).
func (d *daemon) finalizePushOrchestrator(_ context.Context) error {
	if d.pushDispatcher == nil {
		d.logger.Warn("keeper run: push orchestrator disabled (SshDispatcher не сконфигурирован) — /v1/push/* и MCP keeper.push.apply вернут 'не сконфигурировано'")
		return nil
	}
	if d.topologyResolver == nil {
		// Программная ошибка порядка шагов: finalizePushOrchestrator
		// зарегистрирован ПОСЛЕ setupGRPCEventStream (там создаётся
		// topologyResolver). Эта проверка — fail-fast на refactor-регрессию.
		fmt.Fprintln(os.Stderr, "keeper run: push orchestrator wire-up: topologyResolver is nil (programmer error in step order)")
		return errSetupFailed
	}

	// P2 W-3 multi-provider routing: PGRouter (3-tier per-SID → per-coven →
	// cluster-default). RouterConfigSource — hot-reload-aware adapter над
	// config.Store: на каждый Reload снимок CovenDefaultProviders /
	// ClusterDefaultProvider обновляется без пересоздания PGRouter-а.
	routerCfgSrc := newPushRouterConfigSource(d.store)
	router, err := push.NewPGRouter(push.NewPGRouterReader(d.pool), routerCfgSrc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build push router: %v\n", err)
		return errSetupFailed
	}

	run, err := pushorch.NewPushRun(pushorch.Deps{
		Store:           pushorch.NewStore(d.pool),
		Topology:        d.topologyResolver,
		Render:          d.renderPipeline,
		DestinyLoader:   d.pushDestinyLoader,
		Template:        d.serviceHolder,
		Dispatcher:      d.pushDispatcher,
		Cleaner:         d.pushCleaner,
		Router:          router,
		ProviderMetrics: d.pushMetrics,
		Audit:           d.auditWriter,
		Logger:          d.logger,
		KID:             d.cfg.KID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build push orchestrator: %v\n", err)
		return errSetupFailed
	}
	d.pushRun = run
	d.logger.Info("keeper run: push orchestrator ready",
		slog.String("kid", d.cfg.KID))
	return nil
}

// setupErrandDispatcher — pull-ad-hoc Errand contour (ADR-033, slice E2).
// Собирает Store над `errands`-table + Dispatcher поверх Outbound/ApplyBus,
// делает однократный Replay (recovery scan) осиротевших running-Errand-ов
// этого инстанса.
//
// Зависимости: d.pool (Store), d.outbound (Local SendErrand + remote
// PublishErrand), d.applyBus (subscribe-waiter), d.redisClient (LeaseLookup —
// при nil routing деградирует на local-only). d.auditWriter обязателен.
//
// Шаг ставится после setupGRPCEventStream (d.outbound и d.applyBus уже
// заполнены) и ДО setupAPIServer (api.Deps.ErrandDispatcher / ErrandStore
// читаются на сборке HTTP-сервера).
func (d *daemon) setupErrandDispatcher(ctx context.Context) error {
	if d.outbound == nil {
		fmt.Fprintln(os.Stderr, "keeper run: errand dispatcher wire-up: outbound is nil (programmer error in step order)")
		return errSetupFailed
	}
	if d.applyBus == nil {
		fmt.Fprintln(os.Stderr, "keeper run: errand dispatcher wire-up: applyBus is nil (programmer error in step order)")
		return errSetupFailed
	}

	store := errand.NewStore(d.pool)

	// LeaseLookup — при наличии Redis. Single-keeper dev без Redis получает
	// nil → dispatcher идёт local-only (Outbound.SendErrand, NotConnected
	// если стрима нет).
	var lookup errand.LeaseLookup
	if d.redisClient != nil {
		lookup = errandLeaseLookup{rc: d.redisClient}
	}

	disp, err := errand.NewDispatcher(errand.Deps{
		Store:       store,
		Outbound:    d.outbound,
		Publisher:   d.outbound, // тот же Outbound реализует обе поверхности
		LeaseLookup: lookup,
		ApplyBus:    errandApplyBusBridge{bus: d.applyBus},
		Logger:      d.logger,
		Audit:       d.auditWriter,
		KID:         d.cfg.KID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build errand dispatcher: %v\n", err)
		return errSetupFailed
	}
	d.errandStore = store
	d.errandDispatcher = disp

	// Recovery scan однократно при старте: переводим осиротевшие running-
	// Errand-ы этого KID в timed_out (background-горутина умерла вместе с
	// процессом — ErrandResult-а больше не будет). Дефолт grace = 25 мин
	// (server-cap 300s × 5), параметризация через ReplayOptions при появлении
	// reaper-конфига `reaper.errands.*` (slice E4).
	if n, rerr := disp.Replay(ctx, errand.ReplayOptions{}); rerr != nil {
		d.logger.Warn("keeper run: errand replay failed (non-fatal, continuing startup)",
			slog.Any("error", rerr))
	} else if n > 0 {
		d.logger.Info("keeper run: errand replay swept orphan running errands",
			slog.Int("count", n))
	}

	d.logger.Info("keeper run: errand dispatcher ready",
		slog.String("kid", d.cfg.KID),
		slog.Bool("cluster_routing", lookup != nil))
	return nil
}

// errandLeaseLookup — production-обёртка LeaseLookup поверх Redis. Симметрично
// topologyLeaseChecker / leaseOwnerChecker (тонкие адаптеры из daemon.go для
// сужения зависимости пакета).
type errandLeaseLookup struct{ rc *keeperredis.Client }

func (l errandLeaseLookup) ReadHolder(ctx context.Context, sid string) (string, error) {
	return keeperredis.ReadSoulLeaseHolder(ctx, l.rc, sid)
}

// errandApplyBusBridge — адаптер *applybus.EventBus к узкой поверхности
// errand.ApplyBus (только Subscribe). Сужает зависимость dispatcher-а: тесты
// мокают через interface, прод-инжектит общий applybus.EventBus (тот же, что
// для apply-флоу). Channel-тип хранится в applybus.Event, поэтому никакого
// маппинга — прямой проброс.
type errandApplyBusBridge struct{ bus *applybus.EventBus }

func (b errandApplyBusBridge) Subscribe(ctx context.Context, applyID string) <-chan applybus.Event {
	return b.bus.Subscribe(ctx, applyID)
}

func (b errandApplyBusBridge) SubscribeWithBridge(ctx context.Context, applyID string, wantBridge bool) <-chan applybus.Event {
	return b.bus.SubscribeWithBridge(ctx, applyID, wantBridge)
}

// setupGRPCBootstrap — gRPC Bootstrap listener (M2.1.b.2), server-only TLS на
// отдельном порту. Cleanup — drain goroutine (15s).
func (d *daemon) setupGRPCBootstrap(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// gRPC Bootstrap listener (M2.1.b.2). Server-only TLS на отдельном
	// порту: у Soul-а до онбординга ещё нет SoulSeed-сертификата
	// (ADR-012(b)).
	//
	// listen.grpc.bootstrap.addr — обязательный по schema-фазе; здесь
	// проверять не нужно. NewBootstrapServer падает с error на пустой
	// addr/TLS — это runtime config bug, exit 1.
	grpcDone := make(chan struct{})
	bootstrapDeps := keepergrpc.BootstrapDeps{
		Pool:        d.pool,
		VaultClient: d.vc,
		AuditWriter: d.auditWriter,
		KID:         cfg.KID,
		PKIMount:    cfg.Vault.PKIMount,
		PKIRole:     cfg.Vault.PKIRole,
		Metrics:     d.grpcMetrics,
	}
	// Sigil trust-anchor-ы для Soul (ADR-026(h), R3-S7, architect af7d): ЖИВОЙ
	// источник набора (тот же holder, что connect-time broadcast и watcher
	// ротации обновляет), а не снимок старта. setupSigil переставлен выше этого
	// шага → d.sigilAnchorSource заполнен (nil при выключенном Sigil → reply без
	// pubkey, bootstrap обратносовместим). typed-nil-guard: nil-holder в
	// interface-поле дал бы non-nil interface; ставим только при живом holder-е.
	if d.sigilAnchorSource != nil {
		bootstrapDeps.SigilAnchorSource = d.sigilAnchorSource
	}
	grpcSrv, err := keepergrpc.NewBootstrapServer(cfg.Listen.GRPC.Bootstrap, bootstrapDeps, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build gRPC bootstrap server: %v\n", err)
		return errSetupFailed
	}
	go func() {
		defer close(grpcDone)
		if err := grpcSrv.Start(ctx); err != nil {
			logger.Error("gRPC Bootstrap listener stopped with error", slog.Any("error", err))
		}
	}()
	d.cleanups.push(func() {
		select {
		case <-grpcDone:
		case <-time.After(15 * time.Second):
			logger.Warn("gRPC Bootstrap listener did not stop within 15s after shutdown — leak suspected")
		}
	})
	return nil
}

// setupRedis — Redis-клиент (nil-fallback при пустом addr) + apply-events bus.
// Клиент общий для Outbound / EventStream / SoulLease / Reaper. rbac-pub/sub
// подключается отдельно (setupRBACInvalidation).
func (d *daemon) setupRedis(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Redis-клиент поднимается до Outbound / EventStream, потому что
	// cluster-mode routing (ADR-002 HA) и SoulLease используют один и
	// тот же клиент. Reaper-блок ниже переиспользует его же. При
	// `cfg.Redis.Addr == ""` (dev без Redis) клиент остаётся nil —
	// Outbound деградирует до single-instance lookup, EventStream
	// поднимается без SoulLease / heartbeat-кэша / cluster-subscribe.
	if cfg.Redis.Addr != "" {
		rc, err := keeperredis.NewClient(ctx, keeperredis.Config{
			Addr:        cfg.Redis.Addr,
			PasswordRef: cfg.Redis.PasswordRef,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "keeper run: redis client: %v\n", err)
			return errSetupFailed
		}
		d.redisClient = rc
		d.cleanups.push(func() { _ = rc.Close() })
	} else {
		logger.Warn("keeper run: redis disabled (listen.redis.addr is empty) — cluster-mode routing / SoulLease / heartbeat-cache disabled")
	}

	// Apply-events bus (M0.7.c) — общий между EventStream-handler-ом
	// (publisher TaskEvent/RunResult) и MCP SSE-handler-ом (subscriber).
	// Cluster-mode (ADR-006(c), M2.6): при наличии Redis-клиента и KID
	// шина дополнительно публикует события в `apply:<applyID>` и
	// подписывается на чужие через [keeperredis.SubscribeApplyEvent],
	// поэтому SSE-подписчик на Keeper-A получает события от publisher-а
	// на Keeper-B. При redisClient=nil — single-Keeper-fallback.
	d.applyBus = applybus.NewBusWithRedis(logger, d.redisClient, cfg.KID)
	return nil
}

// heraldQueueAdapter — адаптер [redis.HeraldDeliveryQueue] под herald.QueueBackend
// (узкий контракт без импорта redis-пакета в сигнатурах herald-API). Конвертирует
// *redis.ClaimedJob → *herald.ClaimedJob и прокидывает mini-reaper-callback.
type heraldQueueAdapter struct {
	q *keeperredis.HeraldDeliveryQueue
}

func (a heraldQueueAdapter) Enqueue(ctx context.Context, payload []byte) error {
	return a.q.Enqueue(ctx, payload)
}

func (a heraldQueueAdapter) Claim(ctx context.Context, blockTimeout time.Duration) (*herald.ClaimedJob, error) {
	c, err := a.q.Claim(ctx, blockTimeout)
	if err != nil || c == nil {
		return nil, err
	}
	return &herald.ClaimedJob{Payload: c.Payload, JobID: c.JobID}, nil
}

func (a heraldQueueAdapter) SetLease(ctx context.Context, jobID string, ttl time.Duration) error {
	return a.q.SetLease(ctx, jobID, ttl)
}

func (a heraldQueueAdapter) Ack(ctx context.Context, jobID string, payload []byte) error {
	return a.q.Ack(ctx, jobID, payload)
}

func (a heraldQueueAdapter) Requeue(ctx context.Context, jobID string, oldPayload, newPayload []byte) error {
	return a.q.Requeue(ctx, jobID, oldPayload, newPayload)
}

func (a heraldQueueAdapter) RequeueExpired(ctx context.Context, parse func([]byte) (string, bool)) (int, error) {
	return a.q.RequeueExpired(ctx, parse)
}

// setupHeraldDelivery — claim-queue worker-ы реальной webhook-доставки уведомлений
// (ADR-052(d), S3). Late-binding подменяет fallback-LogDeliveryQueue dispatcher-а
// (собран в setupAudit ДО Redis) на RedisDeliveryQueue, поднимает N worker-ов +
// mini-reaper осиротевших job-ов.
//
// Fail-open: при отсутствии Redis (d.redisClient==nil) доставка деградирует —
// dispatcher остаётся с LogDeliveryQueue (job-ы логируются, не доставляются),
// keeper НЕ падает (ADR-052(d) wiring). Так же при herald.workers==0 (явный
// opt-out): очередь подключается (Enqueue копит job-ы в Redis), но worker-ы не
// поднимаются.
//
// Зависит от setupRedis (redisClient), setupAudit (heraldDispatcher + auditWriter),
// setupMetricsRegistry (heraldDeliveryMetrics) и setupVault (d.vc — резолв
// signing-token-ов secret_ref). Cleanup — отмена runCtx + WaitGroup-ожидание.
func (d *daemon) setupHeraldDelivery(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	if d.heraldDispatcher == nil {
		// setupAudit не собрал dispatcher (fail-open ветка S3+) — доставки нет.
		return nil
	}
	if d.redisClient == nil {
		logger.Warn("herald: delivery degraded — Redis disabled; notifications matched but not delivered")
		return nil
	}

	rq, err := keeperredis.NewHeraldDeliveryQueue(d.redisClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: herald delivery queue: %v\n", err)
		return errSetupFailed
	}
	backend := heraldQueueAdapter{q: rq}

	// Late-binding: dispatcher теперь кладёт job-ы в Redis-очередь вместо лог-noop.
	d.heraldDispatcher.SetQueue(herald.NewRedisDeliveryQueue(backend, logger))

	workers := cfg.Herald.ResolvedWorkers()
	if workers <= 0 {
		logger.Info("herald: delivery workers disabled (herald.workers=0) — jobs queued but not delivered")
		return nil
	}

	timeout := heraldDeliveryTimeout(cfg)

	// Heralds-reader: замыкание над SelectHeraldByName (узкая поверхность реестра).
	heralds := heraldReaderFunc(func(rctx context.Context, name string) (*herald.Herald, error) {
		return herald.SelectHeraldByName(rctx, d.pool, name)
	})

	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	d.cleanups.push(func() {
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			logger.Warn("herald: delivery workers did not stop within 15s after shutdown — leak suspected")
		}
	})
	d.cleanups.push(runCancel)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		w := &herald.DeliveryWorker{
			Queue:   backend,
			Heralds: heralds,
			KV:      d.vc,
			Audit:   d.auditWriter,
			Logger:  logger,
			Metrics: d.heraldDeliveryMetrics,
			Timeout: timeout,
		}
		wg.Add(1)
		go func(worker *herald.DeliveryWorker) {
			defer wg.Done()
			if err := worker.Run(runCtx); err != nil {
				logger.Error("herald: delivery worker stopped with error", slog.Any("error", err))
			}
		}(w)
	}
	// Mini-reaper осиротевших job-ов (одна горутина на инстанс).
	wg.Add(1)
	go func() {
		defer wg.Done()
		herald.RunDeliveryReaper(runCtx, backend, herald.DefaultReaperInterval, logger)
	}()
	go func() {
		wg.Wait()
		close(runDone)
	}()

	logger.Info("herald: delivery workers started",
		slog.Int("workers", workers),
		slog.Duration("delivery_timeout", timeout))
	return nil
}

// heraldReaderFunc — функциональный адаптер под herald.HeraldReader.
type heraldReaderFunc func(ctx context.Context, name string) (*herald.Herald, error)

func (f heraldReaderFunc) HeraldByName(ctx context.Context, name string) (*herald.Herald, error) {
	return f(ctx, name)
}

// heraldDeliveryTimeout резолвит общий таймаут webhook-POST-а из
// `keeper.herald.delivery_timeout`; пусто/некорректно → herald.DefaultDeliveryTimeout.
func heraldDeliveryTimeout(cfg *config.KeeperConfig) time.Duration {
	raw := config.DefaultHeraldDeliveryTimeout
	if cfg.Herald != nil && cfg.Herald.DeliveryTimeout != "" {
		raw = cfg.Herald.DeliveryTimeout
	}
	d, err := config.ParseDuration(raw)
	if err != nil || d <= 0 {
		return herald.DefaultDeliveryTimeout
	}
	return d
}

// setupConclave — регистрация presence-записи этого keeper-инстанса в Conclave
// (реестр живых инстансов в Redis, ADR-006 amend, soul-shedding S1) + renewal-
// goroutine, продлевающая её. Подключается после setupRedis (нужен redisClient)
// и setupConfig (нужен cfg.KID).
//
// Питает S2/S3 (отдельные слайсы): refuse-guard «я не один» (CountLive > 1) и
// soul-shedding (есть куда уходить). Сам S1 = только реестр + count + wiring;
// потребителей presence ещё нет — gauge keeper_conclave_instances даёт раннюю
// наблюдаемость числа живых инстансов.
//
// Redis==nil (dev / single-instance без Redis) — no-op + warn: presence-реестр
// требует общего Redis между инстансами; без него «кластера» нет (defensive,
// в проде Redis обязателен).
//
// Cleanup-стек LIFO (тот же приём, что vault-renewer / reaper):
//
//  1. renewCancel()           — сигнализируем renewal-goroutine остановиться.
//  2. <-renewDone (с timeout) — ждём её реального выхода.
//  3. DeregisterInstance      — удаляем presence-ключ (graceful; на crash —
//     TTL-expiry). Detached-ctx: shutdown-ctx может быть уже отменён.
//
// Регистрация на старте — requireUnique=true: коллизия KID (два keeper-процесса
// с одинаковым `kid` в конфиге) логируется как WARN и регистрация продолжается
// безусловным SET (presence своего KID — инвариант, не борьба за лидерство).
func (d *daemon) setupConclave(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	if d.redisClient == nil {
		logger.Warn("keeper run: conclave disabled (redis unavailable) — instance presence registry off, soul-shedding refuse-guard inert")
		return nil
	}

	ttl := keeperredis.DefaultConclaveTTL
	renewEvery := keeperredis.DefaultConclaveRenewInterval

	// Лёгкие метаданные для диагностики: started_at + KID. json.Marshal на
	// контролируемых значениях не падает; ошибку трактуем fail-safe — пишем
	// голый KID (presence-ключ важнее, чем его value-форма).
	meta := conclaveMeta(cfg.KID)

	if err := keeperredis.RegisterInstance(ctx, d.redisClient, cfg.KID, meta, ttl, true); err != nil {
		if errors.Is(err, keeperredis.ErrConclaveKIDTaken) {
			logger.Warn("keeper run: conclave KID collision — another keeper instance уже зарегистрирован с тем же kid (ошибка конфигурации?), регистрируюсь поверх",
				slog.String("kid", cfg.KID))
			// Безусловная перезапись: presence своего KID — инвариант.
			if err2 := keeperredis.RegisterInstance(ctx, d.redisClient, cfg.KID, meta, ttl, false); err2 != nil {
				fmt.Fprintf(os.Stderr, "keeper run: conclave register (overwrite): %v\n", err2)
				return errSetupFailed
			}
		} else {
			fmt.Fprintf(os.Stderr, "keeper run: conclave register: %v\n", err)
			return errSetupFailed
		}
	}
	logger.Info("keeper run: conclave registered (instance presence active)",
		slog.String("kid", cfg.KID),
		slog.Duration("ttl", ttl),
		slog.Duration("renew_interval", renewEvery))

	renewCtx, renewCancel := context.WithCancel(ctx)
	renewDone := make(chan struct{})

	// 3. Deregister (зарегистрирован раньше → LIFO выполнит последним): удаляем
	//    presence-ключ. Detached-ctx с timeout-ом — shutdown-ctx может быть уже
	//    отменён, а Redis на shutdown-е может быть недоступен (тогда crash-fallback
	//    на TTL-expiry).
	d.cleanups.push(func() {
		relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer relCancel()
		if err := keeperredis.DeregisterInstance(relCtx, d.redisClient, cfg.KID); err != nil {
			logger.Warn("conclave deregister failed (instance ключ истечёт по TTL)",
				slog.String("kid", cfg.KID), slog.Any("error", err))
		}
	})
	// 2. Ждём выхода renewal-goroutine.
	d.cleanups.push(func() {
		select {
		case <-renewDone:
		case <-time.After(5 * time.Second):
			logger.Warn("conclave renewal goroutine did not stop within 5s after shutdown — leak suspected")
		}
	})
	// 1. Отменяем renewCtx (зарегистрирован позже → LIFO выполнит первым).
	d.cleanups.push(renewCancel)

	go d.runConclaveRenewal(renewCtx, ttl, renewEvery, meta, renewDone)
	return nil
}

// runConclaveRenewal — periodic renew presence-ключа (по образцу
// eventStreamHandler.renewLeaseLoop). На каждый тик продлевает TTL; если ключ
// исчез (пропущенные renew после длинной паузы / внешний DEL) — пере-создаёт
// presence (restart-safe: инстанс молча не выпадает из кластера). После каждого
// успешного refresh-а обновляет gauge keeper_conclave_instances числом живых
// (LiveKIDs) — best-effort, ошибка SCAN-а не роняет renewal-loop.
func (d *daemon) runConclaveRenewal(ctx context.Context, ttl, every time.Duration, meta string, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok, err := keeperredis.RenewInstance(ctx, d.redisClient, d.cfg.KID, ttl)
			if err != nil {
				if ctx.Err() == nil {
					d.logger.Warn("conclave: renew failed", slog.String("kid", d.cfg.KID), slog.Any("error", err))
				}
				continue
			}
			if !ok {
				// Ключ истёк (длинная пауза) — пере-создаём presence без NX
				// (это наш KID, конкуренции нет).
				if rerr := keeperredis.RegisterInstance(ctx, d.redisClient, d.cfg.KID, meta, ttl, false); rerr != nil {
					if ctx.Err() == nil {
						d.logger.Warn("conclave: re-register after key expiry failed",
							slog.String("kid", d.cfg.KID), slog.Any("error", rerr))
					}
					continue
				}
				d.logger.Info("conclave: presence re-registered after key expiry", slog.String("kid", d.cfg.KID))
			}
			d.observeConclaveLive(ctx)
		}
	}
}

// observeConclaveLive обновляет gauge keeper_conclave_instances числом живых
// инстансов (LiveKIDs). Best-effort: ошибка SCAN-а логируется debug-ом и
// gauge не трогается (держит прежнее значение). nil-gauge — no-op.
func (d *daemon) observeConclaveLive(ctx context.Context) {
	if d.conclaveInstances == nil {
		return
	}
	n, err := keeperredis.CountLive(ctx, d.redisClient)
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Debug("conclave: count live failed (gauge not updated)", slog.Any("error", err))
		}
		return
	}
	d.conclaveInstances.Set(float64(n))
}

// conclaveMeta собирает лёгкие presence-метаданные (`{started_at, kid}`) для
// диагностики. Fail-safe: на ошибке Marshal (не ожидается на контролируемых
// значениях) возвращает голый KID — presence-ключ важнее формы его value.
func conclaveMeta(kid string) string {
	b, err := json.Marshal(struct {
		StartedAt string `json:"started_at"`
		KID       string `json:"kid"`
	}{
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		KID:       kid,
	})
	if err != nil {
		return kid
	}
	return string(b)
}

// setupConclaveRefuseGuard — refuse-guard soul-shedding (Finding-A, ADR-027(h)):
// при `acolytes == 0` (run-goroutine-путь, single-keeper-only) и присутствии
// ДРУГИХ живых Keeper-инстансов в Conclave (`CountLive > 1`) Keeper по дефолту
// ОТКАЗЫВАЕТСЯ стартовать — иначе apply на одном Keeper-е c Soul-ом на стриме
// другого навсегда зависнет в `applying`. Подключается СРАЗУ после setupConclave
// (нужна собственная presence-запись в выборке) и до подъёма EventStream/Acolyte:
// чинить конфиг оператор должен ДО приёма Soul-стримов.
//
// Источник числа живых — keeperredis.CountLive (тот же Conclave-SCAN, что питает
// gauge keeper_conclave_instances). Conclave-ключи TTL 30s → только что умерший
// инстанс может ещё числиться (stale-окно); для startup-refuse это приемлемо —
// оператор видит ошибку и чинит конфиг. На ошибке чтения Conclave НЕ блокируем
// старт (best-effort, как dispatch-WARN): guard fail-open, runtime-сетка
// (dispatch-WARN + Watchman) остаётся.
//
// Refuse — НЕ panic: чёткое stderr-сообщение + errSetupFailed (exit 1), как
// setupOperatorBootstrapGuard. Opt-out: cfg.AllowUnsafeSinglePathMultiKeeper
// ЛИБО env `KEEPER_ALLOW_UNSAFE_MULTI_KEEPER` (truthy-OR) превращает refuse в
// громкий WARN и продолжает старт (осознанный single-keeper-за-LB выбор).
//
// Redis==nil / acolytes>0 — fast-path no-op: без общего Redis «кластера» нет
// (Conclave inert), а work-queue-режим (acolytes>0) cross-keeper-зависанием не
// страдает (туда guard не относится).
func (d *daemon) setupConclaveRefuseGuard(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	// acolytes>0 — work-queue ADR-027, cross-keeper-зависания нет: guard неуместен.
	// Redis==nil — Conclave выключен (нет реестра живых), «я не один» не определить.
	if cfg.Acolytes > 0 || d.redisClient == nil {
		return nil
	}

	live, err := keeperredis.CountLive(ctx, d.redisClient)
	if err != nil {
		// Best-effort: чтение Conclave упало — не валим старт (fail-open).
		// Runtime-сетка (dispatch-WARN cross-keeper + Watchman) остаётся.
		logger.Warn("keeper run: conclave refuse-guard — не удалось перечислить живые инстансы, guard пропущен (fail-open)",
			slog.Any("error", err))
		return nil
	}

	allowUnsafe := cfg.AllowUnsafeSinglePathMultiKeeper || envTruthy("KEEPER_ALLOW_UNSAFE_MULTI_KEEPER")
	switch decideConclaveSinglePath(cfg.Acolytes, live, allowUnsafe) {
	case conclaveSinglePathRefuse:
		fmt.Fprintln(os.Stderr, conclaveRefuseMessage(live))
		return errSetupFailed
	case conclaveSinglePathWarn:
		logger.Warn("keeper run: multi-keeper + acolytes=0 — refuse подавлен явным opt-out (allow_unsafe_single_path_multi_keeper); прогон может зависнуть в applying при cross-keeper-маршрутизации (ADR-027)",
			slog.Int("conclave_live", live),
			slog.String("self_kid", cfg.KID))
	case conclaveSinglePathOK:
		// liveCount<=1 при Redis!=nil&&acolytes==0: единственный инстанс — штатно.
	}
	return nil
}

// setupRBACInvalidation — cluster-wide RBAC-инвалидация поверх TTL-poll-а (B2,
// ADR-028(d)). Подключается после setupRedis (нужен redisClient) и setupRBAC
// (нужны rbacSvc/rbacHolder). При redisClient==nil — чистый TTL-poll.
func (d *daemon) setupRBACInvalidation(ctx context.Context) error {
	// --- rbac-wiring (B2 = B1 + pub/sub, ADR-028(d)) ---
	// Cluster-wide RBAC-инвалидация поверх TTL-poll-а (rbacHolder.Run выше).
	// rbacSvc после успешной role-мутации PUBLISH-ит в `rbac:invalidate`,
	// остальные ноды по SUBSCRIBE near-instant перечитывают снимок из БД.
	// TTL-poll остаётся fallback-ом (потеря pub/sub-сообщения → подхватит
	// тик rbacHolder.Run). Late-binding: redisClient поднят только сейчас
	// (после NewService/NewHolder), поэтому publish/subscribe подключаются
	// здесь. При redisClient==nil (dev без Redis) — чистый TTL-poll.
	if d.redisClient != nil {
		d.rbacSvc.SetInvalidator(rbacInvalidator{redis: d.redisClient, kid: d.cfg.KID, logger: d.logger})
		go d.rbacHolder.WatchInvalidations(ctx, rbacInvalidationSource{redis: d.redisClient, kid: d.cfg.KID, logger: d.logger})
	}
	// --- /rbac-wiring ---
	return nil
}

// setupOperatorInvalidation — JWT immediate revoke (ADR-014 Amendment
// 2026-05-27). operator.Service после успешного Revoke PUBLISH-ит в тот же
// топик `rbac:invalidate`, что и role-мутации (ADR-028(d)). Подписчиком на
// инвалидацию выступает d.rbacHolder (подписку запускает setupRBACInvalidation
// раньше по steps) — он перечитает Snapshot из БД, и Snapshot.Revoked
// пополнится свежей revoked-строкой → Enforcer.Check вернёт ErrOperatorRevoked
// → middleware вернёт 401 на любой запрос ревокнутого Архонта.
//
// Идёт ПОСЛЕ setupAPIServer: operator.Service создаётся внутри NewServer
// (NewOperatorHandler). При redisClient==nil — no-op (single-Keeper/dev,
// fallback на TTL-poll).
func (d *daemon) setupOperatorInvalidation(_ context.Context) error {
	if d.redisClient == nil || d.apiServer == nil {
		return nil
	}
	opSvc := d.apiServer.OperatorService()
	if opSvc == nil {
		return nil
	}
	opSvc.SetInvalidator(rbacInvalidator{redis: d.redisClient, kid: d.cfg.KID, logger: d.logger})
	return nil
}

// setupServiceRegistryInvalidation — cluster-wide инвалидация реестра Service-ов
// поверх TTL-poll-а (S2, паттерн setupRBACInvalidation). Подключается после
// setupRedis (нужен redisClient) и setupServiceRegistry (нужны serviceSvc/
// serviceHolder). При redisClient==nil — чистый TTL-poll.
//
// serviceSvc после успешной CRUD-мутации PUBLISH-ит в `service:invalidate`,
// остальные ноды по SUBSCRIBE near-instant перечитывают снимок из БД. TTL-poll
// остаётся fallback-ом (потеря pub/sub-сообщения → подхватит тик
// serviceHolder.Run). Late-binding: redisClient поднят только сейчас (после
// NewService/NewHolder), поэтому publish/subscribe подключаются здесь.
func (d *daemon) setupServiceRegistryInvalidation(ctx context.Context) error {
	if d.redisClient != nil {
		d.serviceSvc.SetInvalidator(serviceInvalidator{redis: d.redisClient, kid: d.cfg.KID, logger: d.logger})
		go d.serviceHolder.WatchInvalidations(ctx, serviceInvalidationSource{redis: d.redisClient, kid: d.cfg.KID, logger: d.logger})
	}
	return nil
}

// setupGRPCEventStream — gRPC EventStream listener (M2.2 + M2.5): StreamManager,
// Outbound, scenario-runner и сам EventStreamServer. Жёсткая цепочка
// Outbound→scenarioRunner→EventStream оставлена одним методом. Cleanup —
// scenarioRunner.Shutdown + drain listener.
func (d *daemon) setupGRPCEventStream(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// gRPC EventStream listener (M2.2 + M2.5). mTLS, отдельный порт от
	// Bootstrap (ADR-012(b)). StreamManager + Outbound поднимаются вместе
	// с listener-ом — это keeper-внутренние компоненты, не listener-ы
	// отдельных протоколов. SeedRotationDeps вшивает Vault PKI + Outbound,
	// чтобы handler `SeedRotationRequest` мог выпустить новый seed и
	// отправить SeedRotationReply обратно по тому же стриму.
	streamManager := keepergrpc.NewStreamManager(logger)
	d.streamManager = streamManager
	outbound, err := keepergrpc.NewOutbound(keepergrpc.OutboundDeps{
		Manager:     streamManager,
		AuditWriter: d.auditWriter,
		Logger:      logger,
		Redis:       d.redisClient,
		KID:         cfg.KID,
		Metrics:     d.grpcMetrics,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build outbound: %v\n", err)
		return errSetupFailed
	}
	d.outbound = outbound

	// scenario-runner (M2.x slice .g): singleton-orchestrator прогонов.
	// Dispatch-ит ApplyRequest через Outbound, поэтому собирается после него.
	// Инжектится в Create-handler (запуск scenario `create`) + graceful
	// Shutdown в cleanup-цепочке (LIFO: после listener-ов, до pool.Close).
	// Summons-publisher нового пути dispatch-а (ADR-027(a)): non-nil ТОЛЬКО
	// при живом Redis. nil → publishSummons внутри runner-а no-op, planned-
	// задания подхватит poll-fallback Acolyte-пула (best-effort-ускорение).
	var summons scenario.SummonsPublisher
	if d.redisClient != nil {
		summons = summonsPublisher{redis: d.redisClient, kid: cfg.KID}
	}

	// Multi-keeper-guard старого dispatch-пути (footgun acolytes=0): чекер
	// владельца SID-lease. non-nil ТОЛЬКО при живом Redis (без координации
	// нечем определить владельца стрима, guard вырождается). На чужом KID-
	// владельце dispatchWave печатает WARN о возможном зависании прогона в
	// applying (см. scenario.LeaseOwnerChecker).
	var leaseOwner scenario.LeaseOwnerChecker
	if d.redisClient != nil {
		leaseOwner = leaseOwnerChecker{rc: d.redisClient}
	}

	// Staged-гейт passage-capability (ADR-056 §S5): чекер «какие таргет-хосты НЕ
	// анонсировали passage». non-nil ТОЛЬКО при живом Redis (presence-источник
	// capability — heartbeat-Hash в Redis). nil → staged-прогон (N>1 Passage)
	// отвергается fail-closed (нельзя подтвердить поддержку без presence).
	var passageCap scenario.PassageCapabilityChecker
	if d.redisClient != nil {
		passageCap = passageCapChecker{rc: d.redisClient}
	}

	// Топология с lease-aware presence (ADR-006(a)): «Soul online» деривируется
	// из живого Redis SID-lease, не из снимка `souls.status`. d.redisClient уже
	// поднят (setupRedis отработал перед этим шагом). nil-Redis (single-Keeper
	// dev без координации) → резолвер деградирует на SQL-presence-снимок,
	// симметрично reaper-у.
	var topologyLease topology.SoulLeaseChecker
	if d.redisClient != nil {
		topologyLease = topologyLeaseChecker{rc: d.redisClient}
	}
	d.topologyResolver = topology.NewResolver(d.pool, topologyLease, logger)

	scenarioRunner := scenario.NewRunner(scenario.Deps{
		Loader:        d.serviceLoader,
		Topology:      d.topologyResolver,
		Essence:       d.essenceResolver,
		Render:        d.renderPipeline,
		Outbound:      outbound,
		Destiny:       d.destinySource,
		KeeperModules: d.coreModules,
		DB:            d.pool,
		Logger:        logger,
		Metrics:       d.scenarioMetrics,
		// Scoped-резолв `vault:`-ref в operator-input (docs/input.md →
		// «vault_scope»): тот же vault-клиент, что у render-pipeline + audit
		// для security-trail + config-расширение hard deny-list.
		Vault:          d.vc,
		Audit:          d.auditWriter,
		AuditReader:    auditpg.NewReader(d.pool),
		InputDenyPaths: cfg.Vault.InputDenyPaths,
		// Cutover-флаг исполнения apply (ADR-027, Phase 1.4.2): при keeper.acolytes>0
		// dispatch пишет planned-задания + Summons (исполняет Acolyte-пул), иначе
		// прямой Insert(running)+SendApply (старый путь). KID — origin Summons-сигнала.
		AcolyteEnabled: cfg.Acolytes > 0,
		KID:            cfg.KID,
		Summons:        summons,
		LeaseOwner:     leaseOwner,
		PassageCap:     passageCap,
	})
	d.scenarioRunner = scenarioRunner
	d.cleanups.push(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutCancel()
		if err := scenarioRunner.Shutdown(shutCtx); err != nil {
			logger.Warn("scenario runner shutdown returned error", slog.Any("error", err))
		}
	})

	// Throttle PG-flush-а `last_seen_at` выводим из reaper-порога disconnect:
	// stale_after / 3, чтобы snapshot обновлялся заведомо чаще, чем Reaper
	// метит живой стрим disconnected (ADR-006(a), миграция 014).
	lastSeenFlushInterval := reaper.ResolveMarkDisconnectedStale(cfg.Reaper) / 3

	// Connect-time broadcast печатей доверия плагинов (ADR-026, S6). Источник —
	// тот же реестр plugin_sigils поверх общего pool-а, что и sigil-service
	// (setupSigil идёт раньше в run-цепочке). Gate по d.sigilSvc: Sigil выключен
	// → sigilStore остаётся nil → broadcast no-op. NewPGStore — не второй
	// источник правды, а stateless-адаптер той же таблицы/pool-а (read-only
	// ListActive); полный sigil.Store не экспонируется наружу Service-ом.
	var sigilStore keepergrpc.SigilStore
	if d.sigilSvc != nil {
		sigilStore = sigil.NewPGStore(d.pool)
	}

	// Connect-time broadcast набора trust-anchor-ов (ADR-026(h), R3-S6): «живой»
	// holder, заполненный в setupSigil и обновляемый watcher-ом ротации. typed-nil-
	// guard: при выключенном Sigil holder == nil → передаём nil-интерфейс (иначе
	// non-nil интерфейс с nil-указателем), broadcast набора no-op.
	var trustAnchors keepergrpc.TrustAnchorSource
	if d.sigilAnchorSource != nil {
		trustAnchors = d.sigilAnchorSource
	}

	// Oracle-handler (ADR-030 S2, beacons reactor): PortentEvent → match Decree →
	// постановка named-scenario в work-queue. ServiceRef резолвится ИЗ таргет-
	// incarnation Decree-а (РЕШЕНИЕ #1 вариант b) тем же резолвером, что
	// destroy/upgrade-prepare (d.serviceRegistry). where-CEL — sandbox-evaluator
	// над event.data (keeper-local, без vault/now/soulprint).
	oracleWhere, err := oracle.NewWhereEvaluator()
	if err != nil {
		return fmt.Errorf("oracle where-evaluator: %w", err)
	}
	oracleEnqueuer := &oracleScenarioEnqueuer{
		db:       d.pool,
		resolver: d.serviceRegistry,
		summons:  summonsPublisher{redis: d.redisClient, kid: cfg.KID},
		logger:   logger,
	}

	eventStreamDone := make(chan struct{})
	eventStreamSrv, err := keepergrpc.NewEventStreamServer(cfg.Listen.GRPC.EventStream, keepergrpc.EventStreamDeps{
		SeedDB:                d.pool,
		SoulDB:                d.pool,
		Redis:                 d.redisClient,
		AuditWriter:           d.auditWriter,
		KID:                   cfg.KID,
		Manager:               streamManager,
		ApplyBus:              d.applyBus,
		ApplyRunDB:            d.pool,
		Metrics:               d.grpcMetrics,
		LastSeenFlushInterval: lastSeenFlushInterval,
		SigilStore:            sigilStore,
		TrustAnchors:          trustAnchors,
		// Connect-time broadcast active-набора Vigil (ADR-030, beacons-контур S2,
		// ReplaceAll). Источник — реестр vigils + souls поверх общего pool-а
		// (covens хоста резолвятся из souls, набор — по sid ∪ covens). Доставка
		// VigilSnapshot не зависит от Oracle-handler-а (Portent-реакции) — Vigil
		// раздаются всегда, даже без сконфигурированного Oracle.
		VigilSource: keepergrpc.NewVigilSource(d.pool),
		// Toll cluster-detector hook (ADR-038): на каждом выходе EventStream-
		// handler-а (Recv-error / ctx-cancel) вызывается NotifyDisconnect. При
		// выключенном Toll d.tollWatcher = nil → handler-side hook no-op (см.
		// notifyTollDisconnect в eventstream.go).
		TollNotifier: tollNotifierOrNil(d.tollWatcher),
		// Oracle-handler (PortentEvent → match Decree → постановка scenario в
		// work-queue, ADR-030 S2). DB — общий pool (decrees/oracle_fires +
		// souls для covens субъекта). Enqueuer резолвит ServiceRef ИЗ таргет-
		// incarnation (РЕШЕНИЕ #1 вариант b) и пишет planned-задание планировщик-
		// путём (ADR-027). nil-handler больше нет — Portent-реакции включены.
		Oracle: &keepergrpc.OracleDeps{
			DB:          d.pool,
			Where:       oracleWhere,
			Enqueuer:    oracleEnqueuer,
			AuditWriter: d.auditWriter,
			// keeper_oracle_*-метрики: дескриптор зарегистрирован в
			// setupMetricsRegistry (раньше по steps), паттерн augurMetrics.
			Metrics: d.oracleMetrics,
			// circuit-breaker (ADR-030(a), beacons S4): пороги авто-disable
			// Decree-а. max_fires==0 → breaker OFF (escape-hatch); пусто-поле
			// → дефолт 5 (резолв в oracleCircuitMaxFires/Window, стиль acolyte_*).
			CircuitMaxFires: oracleCircuitMaxFires(cfg),
			CircuitWindow:   oracleCircuitWindow(cfg),
		},
		SeedRotation: &keepergrpc.SeedRotationDeps{
			Pool:        d.pool,
			VaultClient: d.vc,
			AuditWriter: d.auditWriter,
			Outbound:    outbound,
			KID:         cfg.KID,
			PKIMount:    cfg.Vault.PKIMount,
			PKIRole:     cfg.Vault.PKIRole,
		},
		// Augur-брокер (ADR-025, augur.md): резолв доступа + брокер
		// vault/prometheus/elk (delegate=false). DB — общий pool (omens/rites +
		// souls для covens), Vault — тот же клиент, что у render-pipeline /
		// core.vault.kv-read (vault-broker + чтение prom/elk-credential по
		// auth_ref), Egress — SSRF-guarded HTTP-клиент для prom/elk (исходящий
		// HTTP к НЕдоверенному endpoint-у Omen-а), Outbound — тот же, что у
		// SeedRotation (reply по тому же стриму).
		Augur: &keepergrpc.AugurDeps{
			DB:          d.pool,
			Vault:       d.vc,
			Egress:      keeperaugur.NewEgressClient(),
			AuditWriter: d.auditWriter,
			Outbound:    outbound,
			// keeper_augur_*-метрики + augur.request span: дескриптор
			// зарегистрирован в setupMetricsRegistry (раньше по steps).
			Metrics: d.augurMetrics,
		},
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build gRPC event_stream server: %v\n", err)
		return errSetupFailed
	}
	go func() {
		defer close(eventStreamDone)
		if err := eventStreamSrv.Start(ctx); err != nil {
			logger.Error("gRPC EventStream listener stopped with error", slog.Any("error", err))
		}
	}()
	d.cleanups.push(func() {
		select {
		case <-eventStreamDone:
		case <-time.After(15 * time.Second):
			logger.Warn("gRPC EventStream listener did not stop within 15s after shutdown — leak suspected")
		}
	})
	return nil
}

// setupWatchman — изоляция-детект + soul-shedding S2 (Watchman). Фоновая
// goroutine: периодически пингует PG+Redis (те же зависимости, что `/readyz`),
// и при устойчивой изоляции (debounce по watchman_fail_threshold) принудительно
// закрывает ВСЕ локальные EventStream-стримы (streamManager.CloseAll) — Souls
// уходят на живой Keeper по failback-list-у. Подключается после
// setupGRPCEventStream (нужен d.streamManager) и setupRedis (redisClient для
// probe). Cleanup-стек LIFO (как conclave/vault-renewer): cancel → join.
//
// Probe-зависимости — те же Pinger-ы, что `/readyz` (PG обязателен; Redis —
// только при живом клиенте, dev-fallback без Redis даёт probe только по PG).
// Redis==nil НЕ выключает Watchman: PG-изоляция тоже причина увести Souls.
func (d *daemon) setupWatchman(ctx context.Context) error {
	logger := d.logger

	// Те же зависимости, что `/readyz` (см. setupAPIServer): PG обязателен,
	// Redis — только при живом клиенте (typed-nil-guard: nil-интерфейс, иначе
	// probe пинговал бы nil-ресивер). Vault в probe Watchman НЕ включаем: он
	// опционален для обслуживания EventStream-стримов (lease/seed-auth идут через
	// PG+Redis), а его недоступность не означает изоляцию инстанса от флота.
	pingers := []watchman.NamedPinger{
		{Name: "postgres", Pinger: poolPinger{d.pool}},
	}
	if d.redisClient != nil {
		pingers = append(pingers, watchman.NamedPinger{Name: "redis", Pinger: d.redisClient})
	}
	probe, err := watchman.NewDepsProbe(pingers...)
	if err != nil {
		// Невозможно при заполненном pool-е (PG всегда есть), но держим инвариант.
		fmt.Fprintf(os.Stderr, "keeper run: build watchman probe: %v\n", err)
		return errSetupFailed
	}

	wm, err := watchman.New(probe, d.streamManager, watchman.Config{
		Interval:      watchmanInterval(d.cfg),
		FailThreshold: watchmanFailThreshold(d.cfg),
	}, d.watchmanMetrics, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build watchman: %v\n", err)
		return errSetupFailed
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	watchDone := make(chan struct{})

	// Стартовое значение gauge: инстанс при старте считается здоровым (0).
	d.watchmanMetrics.SetIsolated(false)

	logger.Info("keeper run: watchman started (isolation-detect + soul-shedding)",
		slog.Duration("interval", watchmanInterval(d.cfg)),
		slog.Int("fail_threshold", watchmanFailThreshold(d.cfg)),
		slog.Bool("redis_in_probe", d.redisClient != nil))

	// Cleanup LIFO: 2) ждём выхода probe-loop-а; 1) отменяем его ctx (зарегистрирован
	// позже → LIFO выполнит первым).
	d.cleanups.push(func() {
		select {
		case <-watchDone:
		case <-time.After(5 * time.Second):
			logger.Warn("watchman did not stop within 5s after shutdown — leak suspected")
		}
	})
	d.cleanups.push(watchCancel)

	go func() {
		defer close(watchDone)
		wm.Run(watchCtx)
	}()
	return nil
}

// setupToll — cluster-wide detector массового оттока Soul-ов (Toll, ADR-038).
//
// Поднимает три компонента, gate-нутые наличием Redis (single-instance/dev без
// Redis → весь Toll выключен, hook EventStream-а no-op, middleware passthrough):
//
//  1. Per-instance [toll.Watcher] — пассивный объект, в EventStream-cleanup-е
//     зовётся NotifyDisconnect (через узкий [grpc.TollNotifier]).
//  2. Cluster-leader [toll.Leader] — фоновая goroutine, Redis-lease
//     `cluster:toll:leader`, агрегирует sorted-set и set/clear cluster:degraded.
//  3. [toll.DegradedReader] — read-only для api-middleware (см. router.go).
//
// Порядок: ПОСЛЕ setupGRPCEventStream (нужен EventStreamDeps, но wire-up Toll-а
// в EventStream идёт через сохранённую ссылку d.tollWatcher, которую API-сервер
// и daemon-finalize читают позже) и ПОСЛЕ setupRedis. Перед setupAPIServer
// (api.Deps читает d.tollDegradedReader).
//
// Явное выключение через `keeper.toll.enabled: false` — настройка для dev /
// отладки: Watcher не собирается, Leader не стартует, DegradedReader = noop.
// При nil блока (опущен в keeper.yml) → enabled по дефолту (true).
func (d *daemon) setupToll(ctx context.Context) error {
	logger := d.logger

	// Default-on: блок опущен → Toll включён с дефолтами.
	enabled := true
	if d.cfg.Toll != nil && d.cfg.Toll.Enabled != nil {
		enabled = *d.cfg.Toll.Enabled
	}
	// Gate-1: явный opt-out.
	if !enabled {
		d.tollDegradedReader = toll.NoopDegradedReader{}
		logger.Info("keeper run: toll disabled (toll.enabled=false)")
		return nil
	}
	// Gate-2: нет Redis — Toll бессмыслен (sorted-set / lease / degraded-flag
	// все в Redis). middleware-degraded остаётся как noop (passthrough).
	if d.redisClient == nil {
		d.tollDegradedReader = toll.NoopDegradedReader{}
		logger.Info("keeper run: toll disabled (no Redis client)")
		return nil
	}

	// Резолв параметров (опущенные поля → дефолты из shared/config).
	cfgToll := d.cfg.Toll
	if cfgToll == nil {
		cfgToll = &config.KeeperToll{}
	}
	threshold := cfgToll.Threshold
	if threshold <= 0 {
		threshold = config.DefaultTollThreshold
	}
	window := tollDurationOrDefault(cfgToll.WindowSize, config.DefaultTollWindow)
	degradedTTL := tollDurationOrDefault(cfgToll.DegradedTTL, config.DefaultTollDegradedTTL)
	clearGrace := tollDurationOrDefault(cfgToll.ClearGrace, config.DefaultTollClearGrace)
	leaseTTL := tollDurationOrDefault(cfgToll.LeaseTTL, config.DefaultTollLeaseTTL)
	warmup := tollDurationOrDefault(cfgToll.WarmupDelay, config.DefaultTollWarmup)

	// Per-instance Watcher — собирается всегда при enabled+Redis. Source-of-truth
	// для disconnect-публикаций (EventStream-handler зовёт NotifyDisconnect через
	// d.tollWatcher → ZADD).
	publisher := &keeperRedisTollPublisher{client: d.redisClient}
	watcher, err := toll.NewWatcher(toll.Config{KID: d.cfg.KID, WarmupDelay: warmup}, publisher, d.tollMetrics, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build toll watcher: %v\n", err)
		return errSetupFailed
	}
	d.tollWatcher = watcher
	d.tollDegradedReader = &keeperRedisTollDegradedReader{client: d.redisClient}

	// Cluster-leader Loop: Redis-lease agg-aggregator. Запускается в отдельной
	// goroutine, cleanup LIFO (как watchman).
	baselineReader, err := toll.NewPGBaselineReader(d.pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build toll baseline: %v\n", err)
		return errSetupFailed
	}
	// Опц. webhook notifier (ADR-038 amendment 2026-05-27, extensions): nil при
	// выключенном/опущенном блоке. Best-effort: ошибка построения — Warn, leader
	// поднимается без notifier-а (alert-out отказан, audit + gauge остаются).
	notifier := buildTollWebhookNotifier(cfgToll.Webhook, d.vc, logger)
	// Per-coven thresholds (ADR-038 amendment, extensions): копия map-ы из
	// конфига, чтобы leader не разделял мутируемое состояние с hot-reload-ом.
	perCovenThresholds := copyPerCovenThresholds(cfgToll.PerCovenThresholds)
	leader, err := toll.NewLeader(toll.LeaderConfig{
		KID:                d.cfg.KID,
		LeaseTTL:           leaseTTL,
		WindowSize:         window,
		Threshold:          threshold,
		DegradedTTL:        degradedTTL,
		ClearGrace:         clearGrace,
		BaselineCacheTTL:   window,
		PerCovenThresholds: perCovenThresholds,
		Notifier:           notifier,
	}, toll.LeaderDeps{
		Lease:          &keeperRedisTollLeaseAcquirer{client: d.redisClient},
		SortedSet:      &keeperRedisTollSortedSetReader{client: d.redisClient},
		DegradedWriter: &keeperRedisTollDegradedWriter{client: d.redisClient},
		Baseline:       baselineReader,
		Audit:          d.auditWriter,
		Metrics:        d.tollMetrics,
		Logger:         logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build toll leader: %v\n", err)
		return errSetupFailed
	}
	d.tollLeader = leader
	d.tollWebhookCfg = cloneTollWebhookCfg(cfgToll.Webhook)

	// Hot-reload подписка ([ADR-021]): mutation `toll.threshold` / `window_size`
	// / `degraded_ttl` / `clear_grace` / `per_coven_thresholds` / `webhook.*`
	// применяются на лету через [toll.Leader.UpdateConfig]. Restart-required
	// поля (lease_ttl / warmup_delay / enabled) тихо игнорируются — Leader
	// уже стартовал с прежними значениями.
	d.store.OnReload(func(_, newCfg *config.KeeperConfig) {
		d.applyTollReload(newCfg, logger)
	})

	leaderCtx, leaderCancel := context.WithCancel(ctx)
	leaderDone := make(chan struct{})
	logger.Info("keeper run: toll cluster-detector started (per-instance watcher + leader-election attempt)",
		slog.String("kid", d.cfg.KID),
		slog.Float64("threshold", threshold),
		slog.Duration("window", window),
		slog.Duration("lease_ttl", leaseTTL),
		slog.Duration("warmup", warmup))

	// Cleanup LIFO: 2) ждём выхода leader-loop-а; 1) отменяем его ctx (зарегистрирован
	// позже → LIFO выполнит первым).
	d.cleanups.push(func() {
		select {
		case <-leaderDone:
		case <-time.After(5 * time.Second):
			logger.Warn("toll leader did not stop within 5s after shutdown — leak suspected")
		}
	})
	d.cleanups.push(leaderCancel)

	go func() {
		defer close(leaderDone)
		leader.Run(leaderCtx)
	}()
	return nil
}

// setupTempo — Tempo per-AID rate-limiter resolver-тяжёлых write-эндпоинтов
// (ADR-050). Конструирует Redis token-bucket-limiter, который инжектится в
// api.Deps.TempoLimiter (setupAPIServer навешивает middleware точечно на
// `POST /v1/voyages`).
//
// Gate-цепочка (как setupToll):
//   - `tempo.enabled: false` → limiter не конструируется (явный opt-out);
//   - нет Redis-клиента → limiter не конструируется (limiter живёт в Redis;
//     без него middleware passthrough — fail-OPEN, ADR-050(a)+(b)).
//
// При обоих gate-ах d.tempoLimiter остаётся nil → api.Deps.TempoLimiter nil →
// middleware no-op. rate/burst НЕ резолвятся здесь — limiter stateless, лимиты
// читаются на каждом запросе провайдером из config.Store (hot-reload, ADR-050(f));
// см. api.Deps.TempoVoyageCreateLimits в setupAPIServer.
//
// Порядок: ПОСЛЕ setupRedis (нужен d.redisClient), ДО setupAPIServer (api.Deps
// читает d.tempoLimiter).
func (d *daemon) setupTempo(_ context.Context) error {
	logger := d.logger

	// Gate-1: явный opt-out.
	if !d.cfg.Tempo.TempoEnabled() {
		logger.Info("keeper run: tempo disabled (tempo.enabled=false)")
		return nil
	}
	// Gate-2: нет Redis — limiter бессмыслен (token-bucket живёт в Redis).
	// middleware при nil-limiter → passthrough (fail-OPEN, ADR-050(b)).
	if d.redisClient == nil {
		logger.Info("keeper run: tempo disabled (no Redis client)")
		return nil
	}

	limiter, err := keeperredis.NewTokenBucket(d.redisClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build tempo limiter: %v\n", err)
		return errSetupFailed
	}
	d.tempoLimiter = limiter
	createRate, createBurst := d.cfg.Tempo.ResolvedVoyageCreate()
	previewRate, previewBurst := d.cfg.Tempo.ResolvedVoyagePreview()
	logger.Info("keeper run: tempo rate-limiter active (POST /v1/voyages + /v1/voyages/preview)",
		slog.Float64("voyage_create_rate", createRate),
		slog.Int("voyage_create_burst", createBurst),
		slog.Float64("voyage_preview_rate", previewRate),
		slog.Int("voyage_preview_burst", previewBurst))
	return nil
}

// tollNotifierOrNil — typed-nil-guard для интерфейса [keepergrpc.TollNotifier].
// Передача *toll.Watcher==nil напрямую в interface-поле даёт «non-nil interface
// с nil-underlying» (классический Go-gotcha) — handler-side nil-проверка не
// сработает. Helper возвращает «настоящий nil-интерфейс» при nil-watcher.
func tollNotifierOrNil(w *toll.Watcher) keepergrpc.TollNotifier {
	if w == nil {
		return nil
	}
	return w
}

// tempoLimiterOrNil — typed-nil-guard для интерфейса [apimiddleware.RateLimiter].
// При выключенном Tempo (нет Redis / enabled=false) d.tempoLimiter == nil;
// передача *keeperredis.TokenBucket(nil) напрямую в interface-поле дала бы
// non-nil interface (RateLimit-фабрика не распознала бы passthrough). Helper
// возвращает «настоящий nil-интерфейс» при nil-limiter (паттерн tollNotifierOrNil).
func tempoLimiterOrNil(tb *keeperredis.TokenBucket) apimiddleware.RateLimiter {
	if tb == nil {
		return nil
	}
	return tb
}

// tollDurationOrDefault — парсит config-строку duration-формата с fallback на
// дефолт. Стиль `watchmanInterval`-резолвера (semantic-фаза уже отвергла
// невалидный формат, остаётся отрезать пустое/<=0).
func tollDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := config.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// keeperRedisTollPublisher — адаптер [toll.Publisher] поверх keeperredis-
// примитивов. Тонкий: один Publish = один ZADD.
type keeperRedisTollPublisher struct {
	client *keeperredis.Client
}

func (p *keeperRedisTollPublisher) PublishDisconnect(ctx context.Context, sid, kid, coven string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	member := toll.EncodeDisconnect(sid, kid, coven, at)
	return keeperredis.PublishTollDisconnect(ctx, p.client, member, at.Unix())
}

// keeperRedisTollSortedSetReader — адаптер [toll.SortedSetReader] поверх
// keeperredis-примитивов. ZCOUNT + ZREMRANGEBYSCORE.
type keeperRedisTollSortedSetReader struct {
	client *keeperredis.Client
}

func (r *keeperRedisTollSortedSetReader) CountInWindow(ctx context.Context, fromUnix, toUnix int64) (int64, error) {
	return keeperredis.TollCountInWindow(ctx, r.client, fromUnix, toUnix)
}

func (r *keeperRedisTollSortedSetReader) TrimBelow(ctx context.Context, beforeUnix int64) error {
	return keeperredis.TollTrimBelow(ctx, r.client, beforeUnix)
}

// CountByCovenInWindow — реализация [toll.CovenAwareReader] (ADR-038
// amendment 2026-05-27, per-coven thresholds). Один extra round-trip
// (ZRANGEBYSCORE), вызывается leader-ом только при заданном
// PerCovenThresholds.
func (r *keeperRedisTollSortedSetReader) CountByCovenInWindow(ctx context.Context, fromUnix, toUnix int64) (map[string]int64, error) {
	return keeperredis.TollCountByCovenInWindow(ctx, r.client, fromUnix, toUnix)
}

// buildTollWebhookNotifier собирает [toll.WebhookNotifier] по cfg блока
// `toll.webhook` (ADR-038 amendment 2026-05-27, extensions). Возвращает nil
// при выключенном / отсутствующем блоке. Ошибки построения — Warn-лог,
// notifier nil (alert-канал не поднимается, audit + gauge остаются).
//
// vault-параметр обязателен при URLRef с префиксом `vault:` (валидация в
// toll.NewWebhookNotifier); при inline-URL — игнорируется.
func buildTollWebhookNotifier(cfg *config.KeeperTollWebhook, vault toll.VaultReader, logger *slog.Logger) toll.Notifier {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	timeout := tollDurationOrDefault(cfg.Timeout, config.DefaultTollWebhookTimeout)
	notifier, err := toll.NewWebhookNotifier(toll.WebhookConfig{
		URLRef:  cfg.URLRef,
		Format:  cfg.Format,
		Timeout: timeout,
	}, vault, logger)
	if err != nil {
		logger.Warn("keeper run: toll webhook notifier disabled (build failed)",
			slog.Any("error", err))
		return nil
	}
	logger.Info("keeper run: toll webhook notifier enabled",
		slog.String("format", cfg.Format),
		slog.Duration("timeout", timeout))
	return notifier
}

// copyPerCovenThresholds — defensive-copy map из конфига перед инжектом в
// [toll.LeaderConfig]. Hot-reload может пересоздать d.cfg.Toll; leader должен
// работать со снимком, не разделяя ссылку с config-layer-ом.
func copyPerCovenThresholds(in map[string]float64) map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// cloneTollWebhookCfg — defensive-copy блока webhook для diff-проверки на
// hot-reload-е (см. [daemon.applyTollReload]). nil остаётся nil — Webhook —
// опц. блок, и nil == "выключено / опущено".
func cloneTollWebhookCfg(in *config.KeeperTollWebhook) *config.KeeperTollWebhook {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// tollWebhookCfgChanged — точечный diff двух snapshot-ов webhook-блока. true,
// если хотя бы одно поле различается ИЛИ один из аргументов nil-а другой —
// нет. Используется в [daemon.applyTollReload], чтобы пересобирать
// [toll.WebhookNotifier] (Vault-резолв, http.Client) только при реальной
// mutation, не на каждый reload.
func tollWebhookCfgChanged(a, b *config.KeeperTollWebhook) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return a.Enabled != b.Enabled ||
		a.URLRef != b.URLRef ||
		a.Format != b.Format ||
		a.Timeout != b.Timeout
}

// applyTollReload — runtime-apply нового config-snapshot-а на Leader. Вызывается
// из reload-callback-а (см. [setupToll]).
//
// Поведение:
//   - threshold/window/degraded_ttl/clear_grace + per_coven_thresholds —
//     резолв с дефолтами (как в setupToll) → [toll.Leader.UpdateConfig].
//   - webhook.* — recycle notifier-а ТОЛЬКО при изменении (см. [tollWebhookCfgChanged]).
//     Recycle = [buildTollWebhookNotifier] заново (Vault-резолв при URLRef-ref —
//     отложен на первый Notify, как и до reload-а; здесь только пересоздание).
//   - error от UpdateConfig (валидация невалидного блока) → Warn-лог, старые
//     значения leader-а сохраняются. Аналогично [Store.Reload]-семантике:
//     reload не должен ломать работающую подсистему.
//
// Hot-reload-таблица в [docs/keeper/config.md → toll] фиксирует, какие именно
// поля reload-able vs restart-required.
func (d *daemon) applyTollReload(newCfg *config.KeeperConfig, logger *slog.Logger) {
	if d.tollLeader == nil || newCfg == nil {
		return
	}
	cfgToll := newCfg.Toll
	if cfgToll == nil {
		cfgToll = &config.KeeperToll{}
	}
	threshold := cfgToll.Threshold
	if threshold <= 0 {
		threshold = config.DefaultTollThreshold
	}
	window := tollDurationOrDefault(cfgToll.WindowSize, config.DefaultTollWindow)
	degradedTTL := tollDurationOrDefault(cfgToll.DegradedTTL, config.DefaultTollDegradedTTL)
	clearGrace := tollDurationOrDefault(cfgToll.ClearGrace, config.DefaultTollClearGrace)

	// Webhook-recycle: пересобираем notifier только при diff-е (см.
	// [tollWebhookCfgChanged]). Старая ссылка GC-ится; http.Client держит
	// только idle-conn pool — явного Close не требуется.
	var notifier toll.Notifier
	if tollWebhookCfgChanged(d.tollWebhookCfg, cfgToll.Webhook) {
		notifier = buildTollWebhookNotifier(cfgToll.Webhook, d.vc, logger)
		d.tollWebhookCfg = cloneTollWebhookCfg(cfgToll.Webhook)
		logger.Info("toll hot-reload: webhook notifier recycled",
			slog.Bool("enabled", cfgToll.Webhook != nil && cfgToll.Webhook.Enabled))
	} else {
		// Без diff-а — оставляем текущий notifier (читаем под RLock).
		notifier = d.tollCurrentNotifier()
	}

	if err := d.tollLeader.UpdateConfig(toll.LeaderConfig{
		KID:                d.cfg.KID,
		WindowSize:         window,
		Threshold:          threshold,
		DegradedTTL:        degradedTTL,
		ClearGrace:         clearGrace,
		PerCovenThresholds: copyPerCovenThresholds(cfgToll.PerCovenThresholds),
		Notifier:           notifier,
	}); err != nil {
		logger.Warn("toll hot-reload: UpdateConfig failed — keeping previous values",
			slog.Any("error", err))
		return
	}
	logger.Info("toll hot-reload: applied",
		slog.Float64("threshold", threshold),
		slog.Duration("window", window),
		slog.Duration("degraded_ttl", degradedTTL),
		slog.Duration("clear_grace", clearGrace),
		slog.Int("per_coven_count", len(cfgToll.PerCovenThresholds)))
}

// tollCurrentNotifier — возвращает текущий [toll.Notifier] из Leader. Нужен в
// applyTollReload, когда webhook-блок не менялся и пересоздавать notifier не
// нужно: передаём в UpdateConfig тот же pointer, что уже стоит.
func (d *daemon) tollCurrentNotifier() toll.Notifier {
	if d.tollLeader == nil {
		return nil
	}
	return d.tollLeader.CurrentNotifier()
}

// keeperRedisTollDegradedWriter — адаптер toll-internal degradedWriter поверх
// keeperredis. Toll-package интерфейс не экспортирован (`degradedWriter` —
// внутренний); адаптер удовлетворяет ему по signature.
type keeperRedisTollDegradedWriter struct {
	client *keeperredis.Client
}

func (w *keeperRedisTollDegradedWriter) SetDegraded(ctx context.Context, holder string, ttl time.Duration) error {
	return keeperredis.TollSetDegraded(ctx, w.client, holder, ttl)
}

func (w *keeperRedisTollDegradedWriter) ClearDegraded(ctx context.Context) error {
	return keeperredis.TollClearDegraded(ctx, w.client)
}

// keeperRedisTollDegradedReader — адаптер [toll.DegradedReader] поверх
// keeperredis.TollIsDegraded.
type keeperRedisTollDegradedReader struct {
	client *keeperredis.Client
}

func (r *keeperRedisTollDegradedReader) IsDegraded(ctx context.Context) (bool, error) {
	return keeperredis.TollIsDegraded(ctx, r.client)
}

// keeperRedisTollLeaseAcquirer — адаптер [toll.LeaseAcquirer] поверх
// keeperredis.Acquire. Транслирует sentinel-ы keeperredis-а в toll-овые:
// ErrLeaseTaken/ErrLeaseLost — общая поверхность Leader-а (он сам не
// импортирует keeperredis).
type keeperRedisTollLeaseAcquirer struct {
	client *keeperredis.Client
}

func (a *keeperRedisTollLeaseAcquirer) Acquire(ctx context.Context, key, holder string, ttl time.Duration) (toll.Lease, error) {
	lease, err := keeperredis.Acquire(ctx, a.client, key, holder, ttl)
	if err != nil {
		if errors.Is(err, keeperredis.ErrLeaseTaken) {
			return nil, toll.ErrLeaseTaken
		}
		return nil, err
	}
	return &keeperRedisTollLease{lease: lease}, nil
}

// keeperRedisTollLease — адаптер [toll.Lease] поверх *keeperredis.Lease.
type keeperRedisTollLease struct {
	lease *keeperredis.Lease
}

func (l *keeperRedisTollLease) Renew(ctx context.Context) error {
	if err := l.lease.Renew(ctx); err != nil {
		if errors.Is(err, keeperredis.ErrLeaseLost) {
			return toll.ErrLeaseLost
		}
		return err
	}
	return nil
}

func (l *keeperRedisTollLease) Release(ctx context.Context) error {
	return l.lease.Release(ctx)
}

// setupSigilInvalidation — cluster-wide Sigil-re-broadcast поверх connect-time
// broadcast-а (ADR-026, S6c, паттерн setupRBACInvalidation). Подключается после
// setupSigil (нужен d.sigilSvc), setupRedis (redisClient) и setupGRPCEventStream
// (d.outbound + streamManager).
//
// sigilSvc после успешного Allow/Revoke PUBLISH-ит в `sigil:invalidate`. КАЖДАЯ
// нода (включая мутирующую) по SUBSCRIBE перечитывает active-набор из БД и
// re-broadcast-ит его своим подключённым Soul-ам через [Outbound.RebroadcastSigils]
// — иначе Soul на другой ноде работает с устаревшим кешем допусков.
// Connect-time broadcast остаётся fallback-ом (потеря pub/sub-сообщения →
// подхватит следующий reconnect Soul-а).
//
// TTL-fallback тик re-read набора trust-anchor-ключей (ADR-026(h), R3 known-gap)
// поднимается НЕЗАВИСИМО от Redis — он-то и самоисцеляет пропущенный
// `sigil:anchors-changed`-сигнал (см. ниже).
//
// Gate-ы: Sigil выключен (d.sigilSvc==nil) → ничего не поднимается. Redis
// отсутствует (redisClient==nil, single-Keeper/dev) → cluster-wide pub/sub-
// watcher-ы (`sigil:invalidate` / `sigil:anchors-changed`) не поднимаются,
// работают только connect-time broadcast + TTL-fallback тик.
func (d *daemon) setupSigilInvalidation(ctx context.Context) error {
	if d.sigilSvc == nil {
		return nil
	}

	// TTL-fallback тик re-read набора якорей (ADR-026(h), R3 known-gap, образец
	// rbac.Holder.Run / Summons poll-fallback). Поднимается НЕЗАВИСИМО от Redis:
	// при пропуске pub/sub-сигнала `sigil:anchors-changed` (потеря сообщения /
	// reconnect) отставшая нода самоисцеляется за интервал; при выключенном Redis
	// (single-instance / dev) — единственный путь, по которому runtime-ротация
	// доезжает без рестарта. reloadAnchors idempotent/fail-safe (на ошибке re-build
	// не трогает состояние) — двойной reload (тик + pub/sub) безопасен. Goroutine
	// завершается по ctx.Done() (тот же ctx, что у watcher-ов; LIFO-cleanup
	// родительского ctx гасит её на shutdown).
	go runAnchorsReloadTicker(ctx, sigilAnchorsReloadInterval(d.cfg), d.reloadAnchors)
	d.logger.Info("keeper run: sigil anchors TTL-fallback reload enabled",
		slog.Duration("interval", sigilAnchorsReloadInterval(d.cfg)))

	// Cluster-wide re-broadcast и pub/sub-watcher-ы поднимаются только при Redis
	// (single-Keeper/dev без Redis обходится TTL-тиком выше + connect-time broadcast).
	if d.redisClient == nil {
		return nil
	}

	// Тот же stateless-адаптер реестра plugin_sigils, что и connect-time
	// broadcast (ListActive поверх общего pool-а) — не второй источник правды.
	sigilStore := sigil.NewPGStore(d.pool)

	d.sigilSvc.SetInvalidator(sigilInvalidator{redis: d.redisClient, logger: d.logger})
	// Anchors-publisher для ротации ключей подписи (R3-S7): KeyService после
	// Introduce/SetPrimary/Retire шлёт `sigil:anchors-changed` → watcher ниже
	// re-load-ит набор по кластеру. nil-publisher (redisClient==nil) — мутация
	// доедет на рестарте (set-at-start в setupSigil); тут redisClient гарантирован
	// gate-ом выше.
	if d.sigilKeySvc != nil {
		d.sigilKeySvc.SetPublisher(sigilAnchorsPublisher{redis: d.redisClient, logger: d.logger})
	}

	go d.watchSigilInvalidations(ctx, sigilStore)
	// Второй watcher — cluster-wide reload набора trust-anchor-ов подписи (ADR-026(h),
	// R3-S6): на сигнал `sigil:anchors-changed` (ротация ключей подписи на любой
	// ноде) каждая нода re-load-ит Signer/набор и re-broadcast-ит SigilTrustAnchors
	// своим Soul-ам. Отдельный канал от `sigil:invalidate` (тот про допуски
	// plugin_sigils, этот — про ключи их подписи).
	go d.watchAnchorsChanged(ctx)
	return nil
}

// watchSigilInvalidations держит подписку на `sigil:invalidate` до ctx.Done() и
// на каждый сигнал перечитывает active-набор из БД и re-broadcast-ит его всем
// локальным стримам. Ошибка подписки логируется (warn) и НЕ роняет daemon —
// Sigil продолжает раздаваться connect-time broadcast-ом.
func (d *daemon) watchSigilInvalidations(ctx context.Context, store keepergrpc.SigilStore) {
	sub, err := keeperredis.SubscribeSigilInvalidate(ctx, d.redisClient, d.logger)
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Warn("sigil: подписка на cluster-инвалидацию не поднялась, остаётся connect-time broadcast",
				slog.Any("error", err))
		}
		return
	}
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-sub.Channel():
			if !ok {
				// Канал закрыт (фатальная ошибка подписки) — выходим, раздача
				// деградирует на connect-time broadcast.
				return
			}
			d.rebroadcastActiveSigils(ctx, store)
		}
	}
}

// rebroadcastActiveSigils читает active-набор допусков из БД и раздаёт его всем
// Soul-ам, висящим локально на этом инстансе. Best-effort: ошибка ListActive
// логируется и пропускается (connect-time broadcast и Soul-side fail-closed
// verify защищают), per-stream сбои обрабатываются внутри [Outbound.RebroadcastSigils].
func (d *daemon) rebroadcastActiveSigils(ctx context.Context, store keepergrpc.SigilStore) {
	recs, err := store.ListActive(ctx)
	if err != nil {
		d.logger.Warn("sigil: re-broadcast list failed — skipping (connect-time broadcast protects)",
			slog.Any("error", err))
		return
	}
	d.outbound.RebroadcastSigils(ctx, keepergrpc.SigilRecordsToProto(recs))
}

// runAnchorsReloadTicker — TTL-fallback тик re-read набора trust-anchor-ключей
// подписи (ADR-026(h), R3 known-gap): периодически (каждые interval) выполняет
// reload (= [daemon.reloadAnchors]) до отмены ctx. Блокирующий — caller запускает
// в отдельной goroutine.
//
// Закрывает best-effort-природу канала `sigil:anchors-changed`: если нода
// ПРОПУСТИЛА pub/sub-сигнал (потеря сообщения / reconnect подписки), она держала
// бы старый набор якорей до рестарта — fail-open-окно при Retire старого ключа.
// Периодический re-read самоисцеляет пропуск за интервал (образец
// rbac.Holder.Run TTL-poll + Summons poll-fallback ADR-027).
//
// reload idempotent/fail-safe (reloadAnchors на ошибке re-build не трогает
// состояние; на успехе — атомарный swap), поэтому двойной reload (этот тик +
// pub/sub-watcher) безопасен. interval <= 0 (дефолтнут резолвером до 30s) —
// guard: тик не поднимается (защита от busy-loop). reload вынесен параметром,
// чтобы тик тестировался без реальных Vault/PG/Outbound-deps.
func runAnchorsReloadTicker(ctx context.Context, interval time.Duration, reload func(context.Context)) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reload(ctx)
		}
	}
}

// watchAnchorsChanged держит подписку на `sigil:anchors-changed` до ctx.Done() и
// на каждый сигнал выполняет [daemon.reloadAnchors] (re-build Signer + обновить
// keeper-host verify-набор + holder connect-time broadcast + re-broadcast набора
// своим Soul-ам). Ошибка подписки логируется (warn) и НЕ роняет daemon — набор
// продолжит раздаваться connect-time broadcast-ом со старым значением (до
// следующего TTL-fallback тика [runAnchorsReloadTicker] или рестарта).
func (d *daemon) watchAnchorsChanged(ctx context.Context) {
	sub, err := keeperredis.SubscribeAnchorsChanged(ctx, d.redisClient, d.logger)
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Warn("sigil: подписка на anchors-changed не поднялась, набор якорей не будет hot-reload-иться",
				slog.Any("error", err))
		}
		return
	}
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-sub.Channel():
			if !ok {
				return
			}
			d.reloadAnchors(ctx)
		}
	}
}

// reloadAnchors — «keeper Signer hot-reload» (ADR-026(h), R3-S6): по сигналу
// `sigil:anchors-changed` (ротация ключей подписи) перечитывает active-набор из
// БД и атомарно применяет его без рестарта:
//
//  1. re-build Signer ([daemon.buildSigilSigner]: новый primary подписывает,
//     anchors = публичные ключи всех active-ключей);
//  2. подмена подписывающего Signer-а в sigil-service ([sigil.Service.SetSigner])
//     — новые Allow подписываются свежим primary;
//  3. обновление verify-набора keeper-host-а ([Host.SigilAnchors.SetAnchors]) —
//     keeper-host верифицирует СВОИ плагины против свежих якорей;
//  4. обновление holder-а connect-time broadcast (свежеподключённый Soul получит
//     актуальный набор);
//  5. re-broadcast SigilTrustAnchors всем локальным Soul-ам ([Outbound.RebroadcastTrustAnchors])
//     — подключённые Soul-ы near-instant переключаются на свежий набор.
//
// Best-effort и fail-safe: на ошибке re-build (Vault недоступен, битый реестр)
// логируем warn и НЕ трогаем текущее состояние (старый Signer/набор продолжают
// работать — целостность не нарушается, повтор подхватится следующим сигналом
// либо рестартом). Частичного применения нет: всё после успешного re-build-а.
func (d *daemon) reloadAnchors(ctx context.Context) {
	if d.sigilSvc == nil {
		return
	}
	// In-process span на runtime-ротацию trust-anchor-ключей (re-build Signer +
	// обновление verify-наборов + re-broadcast). Атрибуты БЕЗ секретов: число
	// активных якорей и сколько Soul-ов получили re-broadcast — операционные
	// counts (не label-cardinality, не материал ключа). При OTel disabled tracer
	// no-op — Start/End бесплатны.
	ctx, span := sigil.Tracer().Start(ctx, sigil.SpanRotation)
	defer span.End()

	signer, err := d.buildSigilSigner(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "build_signer_failed")
		d.logger.Warn("sigil: anchors reload skipped — re-build signer failed (keeping current set)",
			slog.Any("error", err))
		return
	}
	pemSet, err := signer.AnchorSetPEM()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "derive_anchor_pem_failed")
		d.logger.Warn("sigil: anchors reload skipped — derive anchor set PEM failed (keeping current set)",
			slog.Any("error", err))
		return
	}

	d.sigilSvc.SetSigner(signer)
	if d.sigilHost != nil {
		d.sigilHost.SigilAnchors.SetAnchors(signer.AnchorSet())
	}
	if d.sigilAnchorSource != nil {
		d.sigilAnchorSource.set(pemSet)
	}
	delivered := d.outbound.RebroadcastTrustAnchors(ctx, pemSet)
	// Re-broadcast-наблюдаемость (ADR-026(h), R3-S7 Retire-инвариант): счётчик
	// проходов + delivered последней раздачи. Оператор видит по
	// keeper_sigil_anchors_last_delivered, что новый набор разошёлся подключённым
	// Soul-ам, ПЕРЕД Retire старого ключа. nil-safe при выключенной observability.
	d.sigilKeyMetrics.ObserveAnchorsRebroadcast(delivered)
	span.SetAttributes(
		attribute.Int("active_anchors", len(pemSet)),
		attribute.Int("rebroadcast_souls", delivered),
	)
	d.logger.Info("sigil: trust-anchors hot-reloaded (multi-anchor rotation)",
		slog.Int("active_anchors", len(pemSet)),
		slog.Int("rebroadcast_souls", delivered),
	)
}

// trustAnchorHolder — атомарно-сменяемый снимок набора trust-anchor-ов подписи
// Sigil в PEM-форме (ADR-026(h), R3-S6). Реализует [keepergrpc.TrustAnchorSource]:
// connect-time broadcast читает свежий набор при каждом подключении Soul-а, а
// watcher `sigil:anchors-changed` подменяет его целиком после runtime-ротации
// ключей подписи. atomic.Pointer на immutable-слайс: lock-free чтение в
// connect-time-пути, атомарная замена в watcher-е.
type trustAnchorHolder struct {
	pems atomic.Pointer[[]string]
}

// set атомарно заменяет набор PEM-якорей (ReplaceAll). Копирует заголовок слайса:
// caller волен переиспользовать буфер.
func (h *trustAnchorHolder) set(pems []string) {
	cp := make([]string, len(pems))
	copy(cp, pems)
	h.pems.Store(&cp)
}

// AnchorSetPEM возвращает текущий снимок набора (read-only; слайс не мутируется
// по месту — set подменяет указатель целиком). nil-holder / неинициализированный
// → nil (пустой набор; Soul fail-closed по no_trust_anchor).
func (h *trustAnchorHolder) AnchorSetPEM() []string {
	if h == nil {
		return nil
	}
	p := h.pems.Load()
	if p == nil {
		return nil
	}
	return *p
}

// setupSigil — bizness-логика Sigil allow-list-а (ADR-026, plugin.allow/revoke/
// list). Feature-flag по конфигу: Signer строится из Vault ТОЛЬКО при заданном
// `sigil.signing_key_ref`; иначе d.sigilSvc остаётся nil — plugin.*-routes (REST)
// и plugin.*-tools (MCP) не регистрируются (паттерн rbacSvc / MCP-listener,
// keeper стартует нормально).
//
// Помещён после setupStorage (pool → Store) и setupVault (vc → Signer), ПЕРЕД
// setupGRPCBootstrap: bootstrap-сервер читает d.sigilPubKeyPEM (trust-anchor
// Soul-у, ADR-026 S6). Позже d.sigilSvc читают setupAPIServer/setupMCPServer.
// Состояния не держит, teardown не нужен (ed25519-Signer in-memory, PG-доступ
// через общий pool).
//
// cacheRoot — [pluginCacheRoot] (тот же источник, что keeper-side core-модули в
// setupCoreModules): `keeper.yml::plugins.cache_root` → env-override →
// [pluginhost.DefaultCacheRoot]. Отдельного config-ключа под Sigil-кеш нет —
// слот плагина один на host (вариант C, single-slot), Sigil и core-модули
// читают тот же кеш.
func (d *daemon) setupSigil(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	if cfg.Sigil == nil || cfg.Sigil.SigningKeyRef == "" {
		logger.Info("keeper run: sigil disabled (sigil.signing_key_ref is empty) — plugin.allow/revoke/list not registered")
		return nil
	}

	// Multi-anchor Signer (R3, ADR-026(h)): источник истины — реестр
	// sigil_signing_keys (primary подписывает, anchors = публичные ключи всех
	// active). Пустой реестр (свежий кластер до первого Introduce) → fallback на
	// одиночный ключ из cfg.signing_key_ref, чтобы НЕ ломать текущий путь (S2-).
	// Fallback — работа от cfg как single-anchor, БЕЗ авто-seed строки в реестр:
	// seed первого ключа — операторская ротационная операция S7 (Introduce через
	// API), а не молчаливый side-effect старта; auto-seed размывал бы аудит
	// (introduced_by_aid) и гонял бы конкурирующие keeper-инстансы наперегонки за
	// вставку. Так старый одиночный путь продолжает жить, новый multi-key путь
	// включается, как только оператор ввёл ключ через API.
	signer, err := d.buildSigilSigner(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build sigil signer: %v\n", err)
		return errSetupFailed
	}

	// Набор trust-anchor-ов для keeper-host verify СВОИХ плагинов (ADR-026(h),
	// R3 multi-anchor): публичные ключи всех active-ключей подписи (включая
	// primary). OR-проверка по набору даёт безразрывную ротацию ключа. Читается
	// в setupCoreModules при сборке keeper-host-а.
	d.sigilAnchors = signer.AnchorSet()

	// PEM-набор всех active-якорей (ADR-026(h), R3-S6/S7): едет Soul-у в bootstrap
	// (multi-anchor, set>single, через живой holder) и connect-time broadcast-ом
	// SigilTrustAnchors. Деривация здесь же — fail-fast на ошибке Marshal-а.
	pemSet, err := signer.AnchorSetPEM()
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: derive sigil anchor set PEM: %v\n", err)
		return errSetupFailed
	}
	// «Живой» holder набора PEM для connect-time broadcast И bootstrap-reply
	// (R3-S7, architect af7d): стартовый набор — сейчас, runtime-ротация подменит
	// его в watcher-е (setupSigilInvalidation).
	d.sigilAnchorSource = &trustAnchorHolder{}
	d.sigilAnchorSource.set(pemSet)

	svc, err := sigil.NewService(sigil.ServiceDeps{
		Signer: signer,
		Store:  sigil.NewPGStore(d.pool),
		Slots:  sigil.NewCacheSlotReader(pluginCacheRoot(cfg.Plugins)),
		Logger: logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build sigil service: %v\n", err)
		return errSetupFailed
	}

	// Operator-facing ротация ключей подписи (R3-S7): key-gen + Vault-write +
	// keys.go CRUD. Vault-путь приватников — отдельный mount `<key>-keys/<key_id>`
	// (развязка с одиночным sigil-signing-key). Publisher (anchors-changed) и
	// Metrics (gauge active-ключей) подключаются late-binding-ом в
	// setupSigilInvalidation / setupMetricsRegistry (Redis/registry поднимаются позже).
	keySvc, err := sigil.NewKeyService(sigil.KeyServiceDeps{
		Pool:          d.pool,
		Vault:         d.vc,
		VaultKeyMount: sigilKeyVaultMount(cfg.Sigil),
		Logger:        logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build sigil key service: %v\n", err)
		return errSetupFailed
	}
	d.sigilKeySvc = keySvc
	d.sigilSvc = svc
	logger.Info("keeper run: sigil enabled (plugin allow-list active)",
		slog.Int("active_anchors", len(signer.AnchorSet())))
	return nil
}

// buildSigilSigner строит подписывающий Signer для Sigil по правилу
// «реестр-первым, cfg-fallback» (R3, ADR-026(h)):
//
//   - есть active-ключи в sigil_signing_keys → multi-anchor Signer
//     ([sigil.LoadSigner]: primary подписывает, anchors = публичные ключи всех
//     active);
//   - реестр пуст → single-anchor Signer из cfg.signing_key_ref
//     ([sigil.NewSigner]) — старый одиночный путь не ломается.
//
// Auto-seed первого ключа в реестр СОЗНАТЕЛЬНО не делается (это операторская
// ротационная операция S7, см. комментарий в setupSigil). Reload набора якорей
// по событию ротации — S6 (load-at-start достаточно для S3).
//
// Условие caller-а: вызывается только при заданном cfg.Sigil.SigningKeyRef
// (Sigil включён), поэтому fallback-ветка всегда имеет валидный ref.
func (d *daemon) buildSigilSigner(ctx context.Context) (*sigil.Signer, error) {
	keys, err := sigil.ListActiveKeys(ctx, d.pool)
	if err != nil {
		return nil, fmt.Errorf("list active sigil signing keys: %w", err)
	}
	if len(keys) > 0 {
		signer, err := sigil.LoadSigner(ctx, d.vc, keys)
		if err != nil {
			return nil, fmt.Errorf("load multi-anchor signer from registry: %w", err)
		}
		d.logger.Info("keeper run: sigil signer from key registry (multi-anchor)",
			slog.Int("active_keys", len(keys)))
		return signer, nil
	}

	// Реестр пуст — fallback на одиночный cfg-ключ (миграция со старого
	// одиночного пути; bootstrap первого ключа в реестр — S7 через API).
	d.logger.Info("keeper run: sigil signing key registry empty — falling back to cfg.signing_key_ref (single-anchor)")
	signingKey, err := sigil.LoadSigningKey(ctx, d.vc, d.cfg.Sigil.SigningKeyRef)
	if err != nil {
		return nil, fmt.Errorf("load cfg signing key: %w", err)
	}
	return sigil.NewSigner(signingKey)
}

// sigilKeyVaultMount выводит корень Vault-пути приватников ключей подписи
// (R3-S7) из mount-а `sigil.signing_key_ref`: `<mount>/keeper/sigil-keys`. Так
// введённые ключи живут в том же KV-mount-е, что одиночный signing-key
// (одинаковая Vault-политика), но в отдельном подпути (развязка ротации).
// Не-парсящийся ref (schema-фаза это уже отбила, но caller вне config-load
// возможен) → пакетный default sigil.defaultSigilKeyMount (`secret/keeper/sigil-keys`,
// передаётся пустой строкой).
func sigilKeyVaultMount(s *config.KeeperSigil) string {
	if s == nil || s.SigningKeyRef == "" {
		return ""
	}
	logical, err := keepervault.ParseRef(s.SigningKeyRef)
	if err != nil {
		return ""
	}
	mount := logical
	if i := strings.IndexByte(logical, '/'); i > 0 {
		mount = logical[:i]
	}
	return mount + "/keeper/sigil-keys"
}

// setupAPIServer — Operator API HTTP-сервер. RedisPinger передаётся только при
// живом клиенте (typed-nil-guard). srv.Start() дёргается оркестратором
// последним (блокирующий main loop), вне «setup».
func (d *daemon) setupAPIServer(_ context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// RedisPinger для `/readyz`: передаём как health.Pinger ТОЛЬКО при живом
	// клиенте. Без явной ветки `*keeperredis.Client(nil)` в интерфейсе даёт
	// typed-nil (интерфейс != nil) — readiness вызвал бы Ping на nil-ресивере.
	// При redisClient==nil (dev-fallback, Redis отключён) check пропускается,
	// как и Vault-check при отсутствии Vault.
	var redisPinger health.Pinger
	if d.redisClient != nil {
		redisPinger = d.redisClient
	}

	// soulPresence — lease-overlay presence для GET /v1/souls (см. api.Deps).
	// Тот же адаптер batch SID-lease EXISTS, что и у topology-резолвера; гейтим
	// non-nil Redis-клиентом (typed-nil интерфейс != nil ломал бы overlay).
	var soulPresence handlers.SoulPresence
	if d.redisClient != nil {
		soulPresence = topologyLeaseChecker{rc: d.redisClient}
	}

	srv, err := api.NewServer(cfg.Listen.OpenAPI, api.Deps{
		JWTVerifier:         d.verifier,
		JWTIssuer:           d.issuer,
		PGPinger:            poolPinger{d.pool},
		RedisPinger:         redisPinger,
		VaultPinger:         d.vc,
		AuditWriter:         d.auditWriter,
		RBAC:                d.rbacHolder,
		RBACSvc:             d.rbacSvc,
		SigilSvc:            d.sigilSvc,
		SigilKeySvc:         d.sigilKeySvc,
		ServiceSvc:          d.serviceSvc,
		ServiceRefs:         d.serviceRefs,
		ServiceScenarios:    d.serviceScenarios,
		ServiceStateSchema:  d.serviceStateSchema,
		ServiceDependencies: d.serviceDependencies,
		AugurSvc:            d.augurSvc,
		OracleSvc:           d.oracleSvc,
		OperatorDB:          d.pool,
		IncarnationDB:       d.pool,
		SoulDB:              d.pool,
		// ChoirDB — реестр Choir/Voice (ADR-044, S-T3). Тот же *pgxpool.Pool, что
		// и IncarnationDB (Choir-таблицы в той же БД): wire-up монтирует
		// `/v1/incarnations/{name}/choirs` (при nil роуты gated-off).
		ChoirDB: d.pool,
		// SoulPresence — lease-overlay presence для GET /v1/souls (ADR-006(a)):
		// поле `status` деривируется из живого Redis SID-lease, не из лениво-
		// сверяемого PG-снимка `souls.status` (иначе переподключившийся Soul висит
		// disconnected до следующего тика Reaper-а). Тот же адаптер и Redis-клиент,
		// что у topology-резолвера (batch SID-lease EXISTS). nil-Redis (single-Keeper
		// dev) → overlay выключен, PG-снимок отдаётся как есть.
		SoulPresence:      soulPresence,
		TTLDefault:        d.ttlDefault,
		MetricsHTTP:       d.httpMetrics,
		ScenarioRunner:    d.scenarioRunner,
		ScenarioDestroyer: d.scenarioRunner,
		ScenarioDrift:     d.scenarioRunner,
		ServiceRegistry:   d.serviceRegistry,
		ServiceLoader:     d.serviceLoader,
		PushRun:           d.pushRun,
		PushProviderSvc:   d.pushProviderSvc,
		HeraldSvc:         d.heraldSvc,
		ErrandDispatcher:  d.errandDispatcher,
		ErrandStore:       d.errandStore,
		// VoyageDB / резолверы — Voyage contour (ADR-043, S5). Тот же
		// *pgxpool.Pool, что несёт IncarnationDB (voyages/voyage_targets в той же
		// БД). Scenario-резолвер → имена инкарнаций (incarnations[] ∪ service/
		// coven-фильтр); command-резолвер → SID-snapshot (AND-merge, parity
		// ErrandRun). RBAC-by-kind + per-incarnation scope-check делает сам
		// VoyageHandler (enforcer=d.rbacHolder, IncarnationDB=d.pool).
		VoyageDB:               d.pool,
		VoyageScenarioResolver: handlers.NewVoyageScenarioPGResolver(d.pool),
		// require_alive (ADR-043 amendment §5) использует тот же presence-lease-
		// чекер, что GET /v1/souls overlay; soulPresence=nil (dev без Redis) →
		// require_alive деградирует на SQL-presence.
		VoyageCommandResolver: handlers.NewVoyageCommandPGResolverWithPresence(d.pool, soulPresence),
		// VoyageMaxScope — верхний лимит размера резолвнутого scope одного Voyage
		// (DoS-guard scopeExceedsCap в voyage.go). Дефолт 10000, явный 0 = безлимит
		// (ResolvedMaxScope в shared/config). Без этого wire-up cap=zero-value=0 и
		// защита на REST-пути мертва (MCP-путь заполняется тем же значением).
		VoyageMaxScope: cfg.Voyage.ResolvedMaxScope(),
		// VoyageMaxBatchSize — верхний предел размера батча/окна (DoS-guard S-W4,
		// batchSizeExceedsCap в voyage.go). Дефолт 10000, явный 0 = без предела.
		VoyageMaxBatchSize: cfg.Voyage.ResolvedMaxBatchSize(),
		// CadenceDB — реестр Cadence-расписаний (`cadences`, ADR-046 S4). Тот же
		// *pgxpool.Pool, что несёт VoyageDB/IncarnationDB. Двухуровневый RBAC-by-kind
		// делает сам CadenceHandler (enforcer=d.rbacHolder).
		CadenceDB: d.pool,
		// CadencePollFloorSeconds — floor-лимит периода interval-Cadence (ADR-046
		// Pass B): create/update с interval_seconds < floor → 422. ЕДИНЫЙ источник
		// с адаптивным опросом Conductor — cadence_scheduler.poll_floor (тот же
		// ResolvedPollFloor, что в conductorPollInterval), не хардкод 30.
		CadencePollFloorSeconds: int(cfg.CadenceScheduler.ResolvedPollFloor().Seconds()),
		// AuditReader — read-side `audit_log` для GET /v1/audit (UI iter 2).
		// Тот же pool, что несёт d.auditWriter; writer и reader разделены
		// только direction-ом для type-safety.
		AuditReader: auditpg.NewReader(d.pool),
		// Toll cluster-detector — middleware-блокировка blocked-routes при
		// cluster:degraded (ADR-038). При выключенном Toll d.tollDegradedReader
		// уже выставлен в NoopDegradedReader (всегда false → middleware
		// passthrough).
		TollDegraded: d.tollDegradedReader,
		// Tempo per-AID rate-limiter `POST /v1/voyages` (ADR-050). limiter nil
		// (нет Redis / tempo.enabled=false) → middleware passthrough; tempoLimiterOrNil
		// гасит typed-nil-gotcha (передача *TokenBucket==nil в interface-поле дала бы
		// non-nil interface). Лимиты читаются на каждом запросе из config.Store
		// (hot-reload, ADR-050(f)) — провайдер ниже.
		TempoLimiter: tempoLimiterOrNil(d.tempoLimiter),
		TempoMetrics: d.tempoMetrics,
		TempoVoyageCreateLimits: func() apimiddleware.RateLimitLimits {
			rate, burst := d.store.Get().Tempo.ResolvedVoyageCreate()
			return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
		},
		// Отдельный bucket voyage_preview (ADR-050 amendment 2026-06-17): preview
		// и create НЕ делят квоту. Тот же hot-reload-провайдер из config.Store.
		TempoVoyagePreviewLimits: func() apimiddleware.RateLimitLimits {
			rate, burst := d.store.Get().Tempo.ResolvedVoyagePreview()
			return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
		},
		// ModuleCatalogPlugins — plugin-секция module-catalog (GET /v1/modules).
		// nil при выключенном Sigil (d.sigilSvc==nil → нет реестра plugin_sigils):
		// каталог тогда отдаёт только core-модули (статическая doc-таблица всегда
		// доступна). NewPGStore — узкий read-адаптер ListActive, как sigilStore в
		// setupSigilInvalidation.
		ModuleCatalogPlugins: moduleCatalogPluginsOrNil(d),
		// ModuleFormPrepH — резолвер source-каталогов UI-формы модуля (ADR-045
		// S3) поверх pgxpool (паттерн VoyageCommandPGResolver(d.pool)).
		ModuleFormPrepH: handlers.NewModuleFormPrepHandler(handlers.NewFormPrepPGResolver(d.pool), d.logger),
		// WebUIEnabled — тоггл встроенного UI `/ui` (ADR-055). Резолв *bool →
		// bool: default-ON (nil-config → true), явный web_ui_enabled: false →
		// /ui не монтируется. UI вшит в бинарь (go:embed) — внешнего бэкенда не
		// требует, в отличие от Tempo/Toll.
		WebUIEnabled: cfg.WebUIMounted(),
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build HTTP server: %v\n", err)
		return errSetupFailed
	}
	d.apiServer = srv
	return nil
}

// setupMCPServer — MCP listener (M0.7.a). Поднимается только при заданном
// listen.mcp.addr. Использует тот же operator.Service, что HTTP-сервер
// (srv.OperatorService()), поэтому идёт после setupAPIServer. Cleanup — drain.
func (d *daemon) setupMCPServer(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// MCP listener (M0.7.a). Поднимается только если в конфиге задан
	// `listen.mcp.addr` — для совместимости с keeper.yml без mcp-блока
	// (минимум один listener — schema.go валидирует на уровне cfg.Listen).
	// Использует тот же [operator.Service], что HTTP — single source of
	// truth для бизнес-логики Operator-CRUD (delegation.md PM-decision #6).
	//
	// goroutine стартует параллельно с srv.Start ниже; lifecycle привязан
	// к тому же ctx (на SIGTERM оба listener-а делают graceful shutdown
	// независимо друг от друга).
	if cfg.Listen.MCP.Addr != "" {
		mcpHandler, err := mcp.NewHandler(mcp.HandlerDeps{
			OperatorSvc: d.apiServer.OperatorService(),
			RBAC:        d.rbacHolder,
			RBACRoles:   d.rbacSvc,
			// SigilSvc — тот же *sigil.Service, что REST прокидывает в
			// api.Deps.SigilSvc (single source of truth). nil при выключенном
			// Sigil → plugin.*-tools диспатчатся, но вернут internal-error
			// «не сконфигурировано» (паттерн RBACRoles).
			SigilSvc: d.sigilSvc,
			// SigilKeySvc — тот же *sigil.KeyService, что REST прокидывает в
			// api.Deps.SigilKeySvc (single source of truth, R3-S7). nil → sigil.key.*-
			// tools вернут «не сконфигурировано».
			SigilKeySvc: d.sigilKeySvc,
			// ServiceSvc — тот же *serviceregistry.Service, что REST прокидывает в
			// api.Deps.ServiceSvc (single source of truth, несёт S2-invalidate-
			// хук). nil → service.*-tools вернут «не сконфигурировано».
			ServiceSvc: d.serviceSvc,
			// AugurSvc — тот же *augur.Service, что REST прокидывает в
			// api.Deps.AugurSvc (single source of truth, ADR-025). nil →
			// augur.*-tools вернут «не сконфигурировано».
			AugurSvc: d.augurSvc,
			// OracleSvc — тот же *oracle.Service, что REST прокидывает в
			// api.Deps.OracleSvc (single source of truth, ADR-030). nil →
			// oracle.*-tools вернут «не сконфигурировано».
			OracleSvc:   d.oracleSvc,
			AuditWriter: d.auditWriter,
			Logger:      logger,
			// Те же значения, что идут в api.NewServer → IncarnationHandler
			// (single source of truth, без дубля конструирования).
			IncarnationDB:     d.pool,
			ScenarioRunner:    d.scenarioRunner,
			ScenarioDestroyer: d.scenarioRunner,
			ScenarioDrift:     d.scenarioRunner,
			ServiceRegistry:   d.serviceRegistry,
			ServiceLoader:     d.serviceLoader,
			// Тот же pool, что REST прокидывает в SoulHandler (SoulDB: d.pool
			// в setupAPIServer) — single source of truth для soul-онбординга.
			SoulDB: d.pool,
			// Тот же rbac.Holder, что REST прокидывает scoper-ом в NewSoulHandler
			// (single source of truth) — scope-граница для bulk keeper.soul.coven-
			// assign. Реализует handlers.PurviewResolver.
			PurviewResolver: d.rbacHolder,
			// PushRun — тот же *pushorch.PushRun, что REST прокидывает в
			// api.Deps.PushRun (single source of truth, Variant C orchestrator).
			// nil при выключенной push-инфраструктуре — keeper.push.apply
			// вернёт «не сконфигурировано» (паттерн SigilSvc).
			PushRun: d.pushRun,

			// PushProviderSvc — тот же *pushprovider.Service, что REST
			// прокидывает в api.Deps.PushProviderSvc (single source of truth,
			// ADR-032 amendment 2026-05-26, S7-2).
			PushProviderSvc: d.pushProviderSvc,

			// HeraldSvc — тот же *herald.Service, что REST прокидывает в
			// api.Deps.HeraldSvc (single source of truth, ADR-052 S4).
			HeraldSvc: d.heraldSvc,

			// ErrandDispatcher / ErrandStore — те же экземпляры, что REST
			// прокидывает в api.Deps.* (single source of truth, ADR-033 slice
			// E2 wire-up). nil → keeper.soul.errand.run / keeper.errand.*
			// вернут «errand orchestrator is not configured».
			ErrandDispatcher: d.errandDispatcher,
			ErrandStore:      d.errandStore,

			// Voyage contour для keeper.voyage.{start,list,get,cancel}. Те же
			// экземпляры, что REST прокидывает в api.Deps (single source of truth,
			// ADR-043 S5). RBAC-by-kind делает сам VoyageHandler.
			VoyageDB:               d.pool,
			VoyageScenarioResolver: handlers.NewVoyageScenarioPGResolver(d.pool),
			// require_alive (ADR-043 amendment §5) использует тот же presence-lease-
			// чекер, что REST; d.redisClient=nil (dev без Redis) → деградация на
			// SQL-presence.
			VoyageCommandResolver: handlers.NewVoyageCommandPGResolverWithPresence(d.pool, mcpSoulPresence(d)),
			VoyageMaxScope:        cfg.Voyage.ResolvedMaxScope(),
			VoyageMaxBatchSize:    cfg.Voyage.ResolvedMaxBatchSize(),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "keeper run: build MCP handler: %v\n", err)
			return errSetupFailed
		}
		mcpSrv, err := mcp.NewServer(cfg.Listen.MCP, mcp.ServerDeps{
			JWTVerifier: d.verifier,
			Handler:     mcpHandler,
			Bus:         d.applyBus,
			ApplyAccess: mcp.NewApplyAccessPG(d.pool),
			RBAC:        d.rbacHolder,
			Logger:      logger,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "keeper run: build MCP server: %v\n", err)
			return errSetupFailed
		}
		mcpDone := make(chan struct{})
		go func() {
			defer close(mcpDone)
			if err := mcpSrv.Start(ctx); err != nil {
				logger.Error("MCP listener stopped with error", slog.Any("error", err))
			}
		}()
		d.cleanups.push(func() {
			select {
			case <-mcpDone:
			case <-time.After(15 * time.Second):
				logger.Warn("MCP listener did not stop within 15s after shutdown — leak suspected")
			}
		})
	} else {
		logger.Info("keeper run: MCP listener disabled (listen.mcp.addr is empty)")
	}
	return nil
}

// setupAcolyte — пул воркеров исполнения apply (ADR-027, Acolyte). Feature-flag:
// поднимается ТОЛЬКО при cfg.Acolytes > 0; иначе no-op (прежний run-goroutine-
// путь scenario-runner-а остаётся). Помещён после setupGRPCEventStream (нужен
// d.outbound) и setupScenarioDeps (нужны render-deps), перед setupReaper.
//
// Cutover (Phase 1.4.4): claim-callback пула = [scenario.ClaimRunner.Claim] над
// теми же deps, что scenario.Runner (Loader/Topology/Essence/render-pipeline/
// Outbound/Vault/Audit/DB) — БЕЗ вторых экземпляров. Summons-подписчик пула
// (wake на planned-задание) — адаптер над [keeperredis.SubscribeSummons] при
// живом Redis; иначе чистый poll-fallback (пул это поддерживает).
//
// Cleanup — graceful-drain пула Acolyte через pool.Shutdown (ADR-027 Phase 2):
//
//  1. pool.Shutdown — beginDrain (стоп новых claim-ов) → ожидание in-flight в
//     пределах grace → claimCancel (отмена claim-ctx у не успевших; их Ward
//     остаётся в БД для recovery, ADR-027(i)).
//  2. acolyteCancel — страховка после Shutdown (гасит worker-loop по timeout-у).
func (d *daemon) setupAcolyte(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	if cfg.Acolytes <= 0 {
		logger.Info("acolyte: pool disabled (keeper.acolytes is 0) — run-goroutine path remains")
		// Startup-advisory (footgun multi-keeper + acolytes=0): run-goroutine-путь
		// держит владение прогоном in-memory ЭТОГО инстанса, поэтому корректен
		// только для single-keeper. В HA-кластере (N>1 живых Keeper на общих PG/
		// Redis) apply, чей Soul на стриме ДРУГОГО инстанса, зависнет в applying
		// (RunResult уйдёт владельцу стрима, здешний barrier его не увидит).
		// Per-dispatch WARN ловит конкретный случай (warnCrossKeeperDispatch);
		// это — одноразовое предупреждение на старте про режим в целом.
		logger.Warn("acolyte: run-goroutine mode (acolytes=0) is single-keeper-only — for an HA cluster (N>1 keepers) set keeper.acolytes>0 (ADR-027)")
		return nil
	}

	// Summons-подписчик пула: non-nil ТОЛЬКО при живом Redis. Адаптер над
	// keeperredis.SubscribeSummons; callback = pool.Notify (wake воркера).
	// При redisClient==nil — оставляем nil: пул работает на чистом poll-
	// fallback-е (Summons-ускорения нет, задания не теряются).
	var summons acolyte.SummonsSubscriber
	if d.redisClient != nil {
		summons = func(subCtx context.Context, onSignal func()) (io.Closer, error) {
			return keeperredis.SubscribeSummons(subCtx, d.redisClient, onSignal, logger)
		}
	}

	pool, err := acolyte.NewPool(acolyte.Config{
		Workers:      cfg.Acolytes,
		PollInterval: acolytePollInterval(cfg),
		DrainGrace:   acolyteDrainGrace(cfg),
	}, acolyte.Deps{
		Logger:  logger,
		Summons: summons,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build acolyte pool: %v\n", err)
		return errSetupFailed
	}
	d.acolytePool = pool

	// Claim-callback = scenario.ClaimRunner над теми же deps, что scenario.Runner
	// (переиспользуем уже созданные daemon-поля — НЕ вторые экземпляры). Lease/
	// Batch — параметры захвата Ward из keeper.yml (acolyte_lease/acolyte_batch),
	// дефолты совпадают с прежними хардкод-значениями (ADR-027).
	claimRunner := scenario.NewClaimRunner(scenario.ClaimDeps{
		Deps: scenario.Deps{
			Loader:   d.serviceLoader,
			Topology: d.topologyResolver,
			Essence:  d.essenceResolver,
			Render:   d.renderPipeline,
			Outbound: d.outbound,
			Destiny:  d.destinySource,
			// KeeperModules НЕ прокидывается в claim-путь: задачи `on: keeper`
			// исполняет только run-goroutine (dispatchKeeperTasks), claim-путь
			// идёт через groupByHost, где plan.Keeper пропускается. Поле в claim-
			// Deps осталось бы непотребляемым.
			DB:             d.pool,
			Logger:         logger,
			Metrics:        d.scenarioMetrics,
			Vault:          d.vc,
			Audit:          d.auditWriter,
			InputDenyPaths: cfg.Vault.InputDenyPaths,
		},
		KID:   cfg.KID,
		Lease: acolyteLease(cfg),
		Batch: acolyteBatch(cfg),
	})
	pool.SetClaim(claimRunner.Claim)

	// Под своим acolyteCtx (производный от parent-а), чтобы cleanup мог
	// отменить пул независимо от пути выхода из runDaemon.
	//
	// Graceful-drain пула Acolyte (ADR-027 Phase 2) ЦЕЛИКОМ внутри
	// pool.Shutdown: beginDrain (стоп новых claim-ов) → ожидание in-flight в
	// пределах grace → claimCancel (отмена claim-ctx у не успевших). Поэтому
	// Shutdown обязан отработать ДО acolyteCancel — иначе отмена worker-ctx
	// (acolyteCtx, родитель claimCtx) пре-отменила бы claim-ctx и сожгла grace.
	// Cleanup-стек LIFO, целевой порядок выполнения:
	//  1. pool.Shutdown — зарегистрирован позже → LIFO выполнит первым.
	//  2. acolyteCancel — зарегистрирован раньше → LIFO позже (страховка:
	//     гасит worker-loop, если Shutdown вернулся по timeout-у).
	acolyteCtx, acolyteCancel := context.WithCancel(ctx)
	d.cleanups.push(acolyteCancel)
	d.cleanups.push(func() {
		// Таймаут shutdown-ctx с запасом над grace+hard-stop пула: при
		// конфиге с большим acolyte_drain_grace не обрезаем drain раньше срока.
		shutTimeout := acolyteDrainGrace(cfg) + 10*time.Second
		shutCtx, shutCancel := context.WithTimeout(context.Background(), shutTimeout)
		defer shutCancel()
		if err := pool.Shutdown(shutCtx); err != nil {
			logger.Warn("acolyte pool shutdown returned error", slog.Any("error", err))
		}
	})

	pool.Start(acolyteCtx)
	return nil
}

// reaperExecutor — составной исполнитель правил Reaper-а: embed [*reaper.Purger]
// (чистые pgx-DELETE-правила) + [*reaper.VaultReconciler] (cross-store
// report-only reap_orphan_vault_keys). Методы обоих промотятся, тип целиком
// удовлетворяет [reaper.PurgerAPI] без явных обёрток. Живёт здесь (cmd/keeper),
// т.к. это wiring-склейка двух исполнителей под один интерфейс runner-а.
type reaperExecutor struct {
	*reaper.Purger
	*reaper.VaultReconciler
}

// sigilKeyIDsReader адаптирует pgxpool.Pool под [reaper] live-keys-reader:
// резолвит авторитетный набор живых key_id (все статусы) через sigil.ListAllKeyIDs.
// Узкая склейка ради изоляции reaper-пакета от sigil-пакета (reaper не импортит
// sigil напрямую — зависимость инвертирована через интерфейс).
type sigilKeyIDsReader struct{ pool *pgxpool.Pool }

func (r sigilKeyIDsReader) ListAllKeyIDs(ctx context.Context) (map[string]struct{}, error) {
	return sigil.ListAllKeyIDs(ctx, r.pool)
}

// soulLeaseChecker адаптирует Redis-клиент под reaper-проверку «жив ли
// EventStream к SID» (lease-aware mark_disconnected, ADR-006(a)). Узкая
// склейка ради изоляции reaper-пакета от keeperredis-пакета (reaper зависит
// от интерфейса, не от клиента напрямую).
type soulLeaseChecker struct{ rc *keeperredis.Client }

func (c soulLeaseChecker) SoulStreamAlive(ctx context.Context, sid string) (bool, error) {
	return keeperredis.SoulStreamAlive(ctx, c.rc, sid)
}

// topologyLeaseChecker адаптирует Redis-клиент под presence-фильтр
// таргет-резолвера (batch SID-lease EXISTS, ADR-006(a) Variant A). Узкая
// склейка ради изоляции topology-пакета от keeperredis-пакета (резолвер
// зависит от интерфейса [topology.SoulLeaseChecker], не от клиента напрямую).
type topologyLeaseChecker struct{ rc *keeperredis.Client }

func (c topologyLeaseChecker) SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error) {
	return keeperredis.SoulsStreamAlive(ctx, c.rc, sids)
}

// mcpSoulPresence — presence-lease-чекер для Voyage command-резолвера на
// MCP-пути (require_alive, ADR-043 amendment §5). Гейтим non-nil Redis-клиентом
// (typed-nil интерфейс != nil деградировал бы фильтр некорректно); nil →
// require_alive падает на SQL-presence. Симметричен soulPresence в setupAPIServer.
func mcpSoulPresence(d *daemon) handlers.SoulPresence {
	if d.redisClient == nil {
		return nil
	}
	return topologyLeaseChecker{rc: d.redisClient}
}

// leaseOwnerChecker адаптирует Redis-клиент под multi-keeper-guard старого
// dispatch-пути scenario-runner-а (footgun acolytes=0): возвращает KID-владельца
// SID-lease. Узкая склейка ради изоляции scenario-пакета от keeperredis (runner
// зависит от интерфейса [scenario.LeaseOwnerChecker], не от клиента напрямую) —
// тот же приём, что topologyLeaseChecker.
type leaseOwnerChecker struct{ rc *keeperredis.Client }

func (c leaseOwnerChecker) SoulLeaseOwner(ctx context.Context, sid string) (string, bool, error) {
	return keeperredis.SoulLeaseOwner(ctx, c.rc, sid)
}

// passageCapChecker адаптирует Redis-клиент под staged-гейт scenario-runner-а
// (ADR-056 §S5): возвращает подмножество SID-ов, НЕ анонсировавших passage-
// capability. Узкая склейка ради изоляции scenario-пакета от keeperredis (runner
// зависит от интерфейса [scenario.PassageCapabilityChecker]) — тот же приём, что
// leaseOwnerChecker.
type passageCapChecker struct{ rc *keeperredis.Client }

func (c passageCapChecker) SoulsLackingPassage(ctx context.Context, sids []string) ([]string, error) {
	return keeperredis.SoulsLackingCapability(ctx, c.rc, sids, config.CapabilityPassage)
}

// setupVoyageWorker — pool VoyageWorker-ов (ADR-043, S1). Feature-flag
// config-gated OFF ПО УМОЛЧАНИЮ: поднимается ТОЛЬКО при voyageWorkers(cfg) > 0
// (cfg.Voyage != nil И workers > 0). Отличие от setupErrandRunWorker:
// отсутствие блока voyage → pool НЕ поднимается (а не дефолт-1) — S1 даёт
// фундамент рядом со старыми путями, не меняя текущее поведение dev-конфига.
//
// На S1 worker исполняет NOOP-заглушку (voyageorch.executeVoyage: лог +
// finalize succeeded); реальный scenario/command прогон — S2/S3. Помещён сразу
// после setupErrandRunWorker и перед setupReaper (parity worker-pool-ов).
//
// Cleanup — отмена voyageCtx + WaitGroup-ожидание выхода всех worker-goroutine-ов.
// renewLoop останавливается на ctx.Done. Явный ReleaseLease не делаем; протухший
// lease подберёт Reaper-правило reclaim_voyages (тираж — пост-S1).
func (d *daemon) setupVoyageWorker(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	workers := voyageWorkers(cfg)
	if workers <= 0 {
		logger.Info("voyageorch: pool disabled (keeper.voyage not configured or workers=0)")
		return nil
	}

	leaseTTL := voyageLeaseTTL(cfg)
	renewInterval := voyageLeaseRenewInterval(cfg)
	pollInterval := voyagePollInterval(cfg)

	if renewInterval >= leaseTTL {
		logger.Warn("voyageorch: renew_interval >= lease_ttl — lease может истекать между renew-тиками",
			slog.Duration("renew_interval", renewInterval),
			slog.Duration("lease_ttl", leaseTTL),
		)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan struct{})

	d.cleanups.push(func() {
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			logger.Warn("voyageorch: workers did not stop within 15s after shutdown — leak suspected")
		}
	})
	d.cleanups.push(runCancel)

	// Production DI-адаптеры исполнения (ADR-043 S5). Конструируются ВСЕГДА при
	// workers>0 (worker.validate fail-closed-ит scenario/command-ветку без них).
	//   - ScenarioSpawner: incarnation.SelectByName → ServiceRegistry.Resolve →
	//     scenario.Runner.Start (reuse incarnation.Run-пути); вернёт applyID.
	//   - IncarnationAwaiter: poll applyrun.SelectStatusesByApplyID до терминала.
	//   - CommandSpawner: обёртка над тем же errandRunSpawnerBridge, что E6-3
	//     (reuse errand.Dispatcher).
	if d.scenarioRunner == nil || d.serviceRegistry == nil {
		fmt.Fprintln(os.Stderr, "keeper run: voyageorch wire-up: scenarioRunner/serviceRegistry is nil (programmer error in step order)")
		return errSetupFailed
	}
	scenarioSpawner := &voyageScenarioSpawner{
		runner:   d.scenarioRunner,
		reader:   d.pool,
		resolver: d.serviceRegistry,
	}
	incarnationAwaiter := &voyagePgIncarnationAwaiter{
		db:           d.pool,
		pollInterval: pollInterval,
		logger:       logger,
	}
	var commandSpawner voyageorch.CommandSpawner
	if d.errandDispatcher != nil {
		commandSpawner = &voyageCommandSpawner{
			bridge: &errandRunSpawnerBridge{
				dispatcher:     d.errandDispatcher,
				terminalSource: d.errandStore,
				pollInterval:   time.Second,
				clock:          time.Now,
			},
		}
	} else {
		logger.Warn("voyageorch: errandDispatcher is nil — kind=command Voyage-и будут fail-closed (CommandSpawner не сконфигурирован)")
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		w := &voyageorch.VoyageWorker{
			KID:             cfg.KID,
			Pool:            d.pool,
			LeaseTTL:        leaseTTL,
			RenewInterval:   renewInterval,
			PollInterval:    pollInterval,
			Logger:          logger,
			ScenarioSpawner: scenarioSpawner,
			ScenarioAwaiter: incarnationAwaiter,
			CommandSpawner:  commandSpawner,
			Audit:           d.auditWriter,
		}
		wg.Add(1)
		go func(worker *voyageorch.VoyageWorker) {
			defer wg.Done()
			if err := worker.Run(runCtx); err != nil {
				logger.Error("voyageorch: worker stopped with error",
					slog.String("kid", cfg.KID),
					slog.Any("error", err),
				)
			}
		}(w)
	}
	go func() {
		wg.Wait()
		close(runDone)
	}()

	d.voyagePoolStarted = true
	logger.Info("voyageorch: pool started",
		slog.Int("workers", workers),
		slog.Duration("lease_ttl", leaseTTL),
		slog.Duration("renew_interval", renewInterval),
		slog.Duration("poll_interval", pollInterval),
	)
	return nil
}

// --- Voyage DI-адаптеры (ADR-043 S5 production wire-up) ---

// voyageServiceResolver — узкая поверхность [scenario.ServiceRegistry.Resolve]
// для scenario-Spawner-а (git-координаты service-репо по имени сервиса).
type voyageServiceResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// cadenceScenarioResolver / cadenceCommandResolver — адаптеры handlers-PG-
// резолверов под cadence.ScenarioResolver / cadence.CommandResolver (ADR-046 §4).
// Conductor (ADR-048) пере-резолвит target рецепта Cadence just-in-time через тот
// же резолв, что POST /v1/voyages (snapshot фиксируется на момент спавна). Тонкая
// обёртка: переупаковывает плоские аргументы в VoyageScenarioFilter /
// VoyageCommandFilter.
type cadenceScenarioResolver struct {
	inner *handlers.VoyageScenarioPGResolver
}

func (r cadenceScenarioResolver) ResolveIncarnations(ctx context.Context, incarnations []string, service, coven string) ([]string, error) {
	return r.inner.ResolveIncarnations(ctx, handlers.VoyageScenarioFilter{
		Incarnations: incarnations,
		Service:      service,
		Coven:        coven,
	})
}

type cadenceCommandResolver struct {
	inner *handlers.VoyageCommandPGResolver
}

func (r cadenceCommandResolver) ResolveSIDs(ctx context.Context, sids, covens []string, where string, requireAlive bool) ([]string, error) {
	return r.inner.ResolveSIDs(ctx, handlers.VoyageCommandFilter{
		SIDs:         sids,
		Covens:       covens,
		Where:        where,
		RequireAlive: requireAlive,
	})
}

// voyageScenarioSpawner — production [voyageorch.ScenarioSpawner]: спавн одного
// per-incarnation scenario-run-а. Резолвит ServiceRef (incarnation.SelectByName
// → ServiceRegistry.Resolve) и запускает scenario.Runner.Start — ровно тот же
// путь, что incarnation.Run-handler (classic single-run). applyID генерируется
// здесь (ULID), как в handler-е, и возвращается для back-link/await.
//
// Start async (возвращается сразу, прогон живёт в своей goroutine); терминал
// доезжает через apply_runs — его ждёт [voyagePgIncarnationAwaiter].
type voyageScenarioSpawner struct {
	runner   *scenario.Runner
	reader   incarnation.ExecQueryRower
	resolver voyageServiceResolver
}

// SpawnScenarioRun реализует [voyageorch.ScenarioSpawner]. Input приходит
// jsonb-байтами (voyages.input) — десериализуем в map[string]any для RunSpec.
func (s *voyageScenarioSpawner) SpawnScenarioRun(ctx context.Context, voyageID, incarnationName, scenarioName string, input []byte, startedByAID string, cadenceID *string) (string, error) {
	inc, err := incarnation.SelectByName(ctx, s.reader, incarnationName)
	if err != nil {
		return "", fmt.Errorf("voyage scenario spawner: select incarnation %q: %w", incarnationName, err)
	}
	// error_locked — fast-fail (parity incarnation.Run probe); lockRun под
	// FOR UPDATE — авторитет, но дешёвый отказ до спавна goroutine.
	if inc.Status == incarnation.StatusErrorLocked {
		return "", fmt.Errorf("voyage scenario spawner: incarnation %q is error_locked", incarnationName)
	}
	ref, ok := s.resolver.Resolve(inc.Service)
	if !ok {
		return "", fmt.Errorf("voyage scenario spawner: service %q is not registered", inc.Service)
	}

	var inputMap map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &inputMap); err != nil {
			return "", fmt.Errorf("voyage scenario spawner: decode input: %w", err)
		}
	}

	applyID := audit.NewULID()
	spec := scenario.RunSpec{
		ApplyID:         applyID,
		IncarnationName: incarnationName,
		ServiceRef:      ref,
		ScenarioName:    scenarioName,
		Input:           inputMap,
		StartedByAID:    startedByAID,
		// CadenceID — back-link на Cadence (voyages.cadence_id, ADR-046 §2),
		// проброшенный воркером из run.CadenceID. nil ⇒ ручной Voyage; populated
		// ⇒ дочерний Voyage расписания → incarnation.run_completed несёт cadence_id
		// (T4b, постоянное Tiding-правило с cadence-селектором).
		CadenceID: cadenceID,
	}
	// VoyageID — back-link на породивший Voyage (voyages.voyage_id, ADR-043):
	// этот prod-spawner — единственный путь scenario-run через voyage-
	// orchestrator, поэтому voyage_id здесь ВСЕГДА заполнен (run.VoyageID — PK
	// voyages). incarnation.run_completed несёт его (ADR-052 amend §k) для
	// visibility-фетча per-incarnation run-событий на Voyage detail. Прямые пути
	// (create/rerun/destroy) сюда не идут — там VoyageID остаётся nil.
	if voyageID != "" {
		spec.VoyageID = &voyageID
	}
	if err := s.runner.Start(ctx, spec); err != nil {
		return "", fmt.Errorf("voyage scenario spawner: runner.Start: %w", err)
	}
	return applyID, nil
}

// voyagePgIncarnationAwaiter — production [voyageorch.IncarnationAwaiter]: poll
// applyrun.SelectStatusesByApplyID до терминала всех хостов инкарнации (без
// applybus-subscribe, чистый PG-poll: на одну инкарнацию poll-латентность
// приемлема, source of truth — PG).
type voyagePgIncarnationAwaiter struct {
	db           applyrun.ExecQueryRower
	pollInterval time.Duration
	logger       *slog.Logger
}

// Await блокируется до терминала apply_run-а либо ctx.Done. Маппит per-host
// статусы в [voyageorch.TargetOutcome] (succeeded/failed/cancelled/no_match).
func (a *voyagePgIncarnationAwaiter) Await(ctx context.Context, applyID string) (voyageorch.TargetOutcome, error) {
	poll := a.pollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	if outcome, done := a.pollOutcome(ctx, applyID); done {
		return outcome, nil
	}
	for {
		select {
		case <-ctx.Done():
			return voyageorch.OutcomeCancelled, ctx.Err()
		case <-ticker.C:
			if outcome, done := a.pollOutcome(ctx, applyID); done {
				return outcome, nil
			}
		}
	}
}

// pollOutcome — один PG-poll: terminal всех хостов → агрегированный outcome.
// done=false при not-found/running/ошибке (caller продолжит poll).
func (a *voyagePgIncarnationAwaiter) pollOutcome(ctx context.Context, applyID string) (voyageorch.TargetOutcome, bool) {
	statuses, err := applyrun.SelectStatusesByApplyID(ctx, a.db, applyID)
	if err != nil {
		a.logger.Warn("voyageorch: await poll failed",
			slog.String("apply_id", applyID), slog.Any("error", err))
		return "", false
	}
	if len(statuses) == 0 {
		return "", false // Insert apply_run-а ещё не виден pool-ом — ждём.
	}
	var anyFailed, anyCancelled, allBenign = false, false, true
	for _, s := range statuses {
		switch s.Status {
		case applyrun.StatusSuccess, applyrun.StatusNoMatch:
			// benign
		case applyrun.StatusCancelled:
			anyCancelled, allBenign = true, false
		case applyrun.StatusFailed, applyrun.StatusOrphaned:
			anyFailed, allBenign = true, false
		default:
			return "", false // non-terminal — ждём.
		}
	}
	switch {
	case anyFailed:
		return voyageorch.OutcomeFailed, true
	case anyCancelled:
		return voyageorch.OutcomeCancelled, true
	case allBenign:
		return voyageorch.OutcomeSucceeded, true
	default:
		return voyageorch.OutcomeFailed, true
	}
}

// voyageCommandSpawner — production [voyageorch.CommandSpawner]: тонкая обёртка
// над тем же [errandRunSpawnerBridge], что E6-3 ErrandRun (reuse errand.Dispatcher).
// Контракт SpawnCommand отличается от bridge.SpawnErrand только отсутствием
// error_code в возврате — отбрасываем его (voyageorch его не использует).
type voyageCommandSpawner struct {
	bridge *errandRunSpawnerBridge
}

// SpawnCommand реализует [voyageorch.CommandSpawner]. Блокируется до терминала
// Errand-а (или ctx.Done).
//
// Back-link Voyage→Errand фиксируется в voyage_targets.errand_id (его
// проставляет сам VoyageWorker через MarkTargetRunning); сам Errand
// standalone (parity single-SID /exec, ADR-033).
func (s *voyageCommandSpawner) SpawnCommand(ctx context.Context, voyageID, sid, module, startedByAID string, input []byte) (string, string, error) {
	_ = voyageID // back-link трекается в voyage_targets, не в errands.
	errandID, status, _, err := s.bridge.SpawnErrand(ctx, "", sid, module, startedByAID, input)
	return errandID, status, err
}

// errandTerminalSource — узкая read-поверхность над `errands`-таблицей для
// await-фазы spawn-bridge-а: poll-fallback статуса по errand_id. *errand.Store
// удовлетворяет автоматически (метод Get). Интерфейс держит bridge
// unit-тестируемым без подъёма PG.
type errandTerminalSource interface {
	Get(ctx context.Context, errandID string) (*errand.Row, error)
}

// errandRunSpawnerBridge — обёртка над existing single-SID errand.Dispatcher
// под контракт [voyageorch.CommandSpawner] (через voyageCommandSpawner).
// SpawnErrand вызывает Dispatcher.Dispatch и блокируется до terminal-статуса;
// CancelErrand — прямой Dispatcher.Cancel (best-effort signal по ADR-033 E5).
//
// Await-механика. Dispatcher имеет sync-окно ServerCap (≤30s, ADR-033 §3): если
// Errand завершается дольше, Dispatch возвращает Async=true (background-горутина
// Dispatcher-а ДОДОЖИДАЕТСЯ результата и пишет terminal в `errands`-строку).
// Voyage command-у нужен РЕАЛЬНЫЙ результат, не sync-window-усечение, поэтому при
// Async=true bridge поллит `errands`-строку через terminalSource до терминала
// (subscribe тут не нужен — Dispatcher уже владеет applybus-подпиской, а строка
// обновляется его же background-горутиной). Дедлайн poll-а = errand timeout + grace.
//
// Timeout per-Errand — берём дефолтный (errand.DefaultTimeoutSeconds = 30s, MVP).
type errandRunSpawnerBridge struct {
	dispatcher     *errand.Dispatcher
	terminalSource errandTerminalSource
	pollInterval   time.Duration
	clock          func() time.Time
}

// SpawnErrand сериализует Input (json-bytes из ErrandRun) обратно в
// map[string]any, вызывает Dispatch, ждёт terminal. Возвращает (errandID,
// status-string, errorCode, err).
//
// Семантика статусов после Dispatch:
//   - DispatchResult.Async == true → Dispatcher escalate-нул sync-окно; bridge
//     ДОДОЖИДАЕТСЯ реального терминала через poll `errands`-строки
//     (awaitTerminal). Команда на хост уже ушла — это НОРМА, не ошибка.
//   - StatusSuccess        → status=success
//   - StatusFailed / StatusModuleNotAllowed / StatusTimedOut / StatusCancelled →
//     соответствующая строка.
func (b *errandRunSpawnerBridge) SpawnErrand(ctx context.Context, runID, sid, module, startedByAID string, input []byte) (string, string, string, error) {
	inputMap := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &inputMap); err != nil {
			return "", "failed", "input_decode_error", fmt.Errorf("errandrun spawner: decode input: %w", err)
		}
	}

	// AID-пропагация: errands.started_by_aid — NOT NULL + FK на operators(aid)
	// (migration 052). Прокидываем AID инициатора ErrandRun-а (errand_runs.
	// started_by_aid, валидный operator); без него insert errand-строки падает
	// по errands_started_by_aid_fk (SQLSTATE 23503) → spawn_error. Этот же AID
	// проставляется в Dispatcher.audit-event errand.invoked.
	_ = runID // single-SID Errand — без back-link на multi-target обвязку.
	res, err := b.dispatcher.Dispatch(ctx, errand.DispatchRequest{
		SID:          sid,
		Module:       module,
		Input:        inputMap,
		StartedByAID: startedByAID,
	})
	if err != nil {
		return res.ErrandID, "failed", classifyDispatchErr(err), err
	}
	if res.Async {
		// Sync-окно Dispatcher-а истекло, результат продолжает писаться его
		// background-горутиной в `errands`-строку. Поллим до терминала.
		status, errorCode, awErr := b.awaitTerminal(ctx, res.ErrandID)
		return res.ErrandID, status, errorCode, awErr
	}
	status, errorCode := classifyErrandStatus(res.Status)
	if status == "" {
		return res.ErrandID, "failed", "unknown_status", fmt.Errorf("errandrun spawner: unknown status %q", res.Status)
	}
	return res.ErrandID, status, errorCode, nil
}

// awaitTerminal поллит `errands`-строку до достижения terminal-статуса
// (poll-fallback, source of truth — PG, который наполняет background-горутина
// Dispatcher-а). Дедлайн = DefaultTimeoutSeconds + grace (sync-окно уже
// истекло, поэтому реальный остаток ≤ полного errand-timeout-а). При истечении
// дедлайна или ctx.Done возвращает failed с диагностическим error_code — строка
// в БД продолжит финализироваться независимо (sweep / поздний result), но
// orchestrator не должен висеть.
func (b *errandRunSpawnerBridge) awaitTerminal(ctx context.Context, errandID string) (string, string, error) {
	if b.terminalSource == nil {
		// Wire-up без terminalSource (не должно случаться в production) —
		// деградируем на прежнее поведение: считаем failed.
		return "failed", "async_escalation", nil
	}
	poll := b.pollInterval
	if poll <= 0 {
		poll = time.Second
	}
	deadline := b.now().Add(time.Duration(errand.DefaultTimeoutSeconds)*time.Second + 5*time.Second)

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	// Первый poll сразу — строка могла стать terminal между Dispatch-escalation
	// и входом сюда.
	if st, ec, done := b.pollTerminal(ctx, errandID); done {
		return st, ec, nil
	}
	for {
		select {
		case <-ctx.Done():
			// Caller-ctx отменён (abort-policy / shutdown). Не failed «по сути» —
			// orchestrator сам пометит cancelled через CancelErrand-путь; здесь
			// возвращаем cancelled, чтобы Summary не считал это провалом.
			return "cancelled", "cancelled", nil
		case <-ticker.C:
			if st, ec, done := b.pollTerminal(ctx, errandID); done {
				return st, ec, nil
			}
			if b.now().After(deadline) {
				return "failed", "await_timeout", nil
			}
		}
	}
}

// pollTerminal делает один Get и проверяет terminal-статус. done=false при
// not-found / running / ошибке (caller продолжит poll).
func (b *errandRunSpawnerBridge) pollTerminal(ctx context.Context, errandID string) (string, string, bool) {
	row, err := b.terminalSource.Get(ctx, errandID)
	if err != nil || row == nil {
		return "", "", false
	}
	if row.Status == errand.StatusRunning {
		return "", "", false
	}
	status, errorCode := classifyErrandStatus(row.Status)
	if status == "" {
		return "", "", false
	}
	return status, errorCode, true
}

func (b *errandRunSpawnerBridge) now() time.Time {
	if b.clock != nil {
		return b.clock()
	}
	return time.Now()
}

// classifyErrandStatus проецирует errand.Status в (orchestrator-status, error_code).
// Пустой status-строка → неизвестный статус (caller обрабатывает отдельно).
func classifyErrandStatus(s errand.Status) (string, string) {
	switch s {
	case errand.StatusSuccess:
		return "success", ""
	case errand.StatusModuleNotAllowed:
		return "module_not_allowed", "module_not_allowed"
	case errand.StatusTimedOut:
		return "timed_out", "timed_out"
	case errand.StatusCancelled:
		return "cancelled", "cancelled"
	case errand.StatusFailed:
		return "failed", "errand_failed"
	default:
		return "", ""
	}
}

// CancelErrand — прямой проксирующий вызов Dispatcher.Cancel. Bridge не знает
// RequestedBy AID (cancel инициирует orchestrator при abort-policy, не оператор);
// audit-event `errand.cancelled` пишется Dispatcher-ом с source=api/aid="" —
// это допустимо для orchestrator-инициированного cancel.
func (b *errandRunSpawnerBridge) CancelErrand(ctx context.Context, errandID string) error {
	if errandID == "" {
		return nil
	}
	if err := b.dispatcher.Cancel(ctx, errand.CancelRequest{ErrandID: errandID}); err != nil {
		// Idempotent: уже terminal — не ошибка (orchestrator вызывает cancel
		// best-effort, race с собственным terminal Errand-а допустим).
		if errors.Is(err, errand.ErrErrandTerminal) {
			return nil
		}
		return err
	}
	return nil
}

// classifyDispatchErr — короткий machine-readable error_code для Summary.
func classifyDispatchErr(err error) string {
	switch {
	case errors.Is(err, errand.ErrSoulNotConnected):
		return "soul_not_connected"
	case errors.Is(err, errand.ErrSIDEmpty), errors.Is(err, errand.ErrModuleEmpty), errors.Is(err, errand.ErrTimeoutOutOfRange):
		return "invalid_request"
	default:
		return "spawn_error"
	}
}

// setupReaper — Reaper-loop (опциональный, требует Redis для lease-лидерства).
// RegisterReaperMetrics — только в этой ветке (cardinality-safe). Cleanup —
// reaperCancel + reaperDone-wait (раньше redisClient.Close по LIFO).
func (d *daemon) setupReaper(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Reaper-loop поднимается параллельно с API-сервером. Под своим
	// reaperCtx (производный от parent-а), чтобы можно было отменить
	// его в cleanup-е, гарантируя порядок shutdown-а независимо от того,
	// каким путём мы выходим из runDaemon (нормальный SIGTERM или
	// fatal-error от srv.Start).
	//
	// Cleanup-стек LIFO. Целевой порядок выполнения:
	//
	//  1. reaperCancel()              — сигнализируем reaper-у остановиться.
	//  2. <-reaperDone (с timeout-ом) — ждём, пока goroutine реально вышла.
	//  3. redisClient.Close()         — закрывается cleanup-ом выше (LIFO),
	//                                   после всех Redis-потребителей.
	//
	// Redis-клиент общий для Outbound / EventStream / SoulLease /
	// Reaper — поднят выше. Reaper требует non-nil Redis (lease-based
	// лидерство); при отсутствии Redis в конфиге Reaper не стартует,
	// даже если `reaper.enabled=true`.
	if cfg.Reaper != nil && cfg.Reaper.Enabled && d.redisClient != nil {
		rc := d.redisClient

		reaperCtx, reaperCancel := context.WithCancel(ctx)
		reaperDone := make(chan struct{})

		// 2. Ждём завершения reaper-goroutine. 5s — щедрая граница:
		//    внутри Run максимум 2s на Release-attempt.
		d.cleanups.push(func() {
			select {
			case <-reaperDone:
			case <-time.After(5 * time.Second):
				logger.Warn("reaper did not stop within 5s after shutdown — leak suspected")
			}
		})

		// 1. Отменяем reaperCtx (зарегистрирован позже → LIFO выполнит первым).
		d.cleanups.push(reaperCancel)

		// Per-rule метрики Reaper-а регистрируем только в этой ветке —
		// если reaper.enabled=false, collectors не публикуются вовсе
		// (cardinality-safe; см. keeper/internal/reaper/metrics.go).
		reaperMetrics := reaper.RegisterReaperMetrics(d.metricsReg)

		// Составной исполнитель: Purger (чистые pgx-DELETE-правила) +
		// VaultReconciler (cross-store report-only reap_orphan_vault_keys).
		// Оба embed-нуты — методы промотятся, reaperExecutor удовлетворяет
		// reaper.PurgerAPI. d.vc может быть nil (Vault не настроен): тогда
		// reap_orphan_vault_keys деградирует (0, error) и логируется как fail,
		// прочие правила работают. clock=nil → time.Now (тесты подменяют).
		// d.vc — конкретный *keepervault.Client; при nil передаём НАСТОЯЩИЙ
		// nil-интерфейс (иначе typed-nil спрятал бы degrade-ветку
		// VaultReconciler-а, где сравнивается vault == nil).
		var vaultDep reaper.VaultKVLister
		if d.vc != nil {
			vaultDep = d.vc
		}
		// Purger lease-aware: reaper-ветка стартует только при non-nil Redis
		// (lease-лидерство), поэтому SID-lease-проверка `mark_disconnected`
		// (ADR-006(a)) всегда доступна — idle-Soul на живом стриме не метится
		// disconnected ложно.
		executor := &reaperExecutor{
			Purger:          reaper.NewPurgerWithLease(d.pool, soulLeaseChecker{rc: rc}, logger),
			VaultReconciler: reaper.NewVaultReconciler(vaultDep, sigilKeyIDsReader{pool: d.pool}, logger, nil),
		}

		// Scry-deps (ADR-031 Slice C): фоновое drift-правило живёт в пакете
		// reaper, но зависит от scenario.Runner (CheckDrift/MarkDriftStatus) и
		// service-registry-резолвера. Собираем единый блок прямо здесь, чтобы
		// не плодить ещё одного setup-step-а. nil-ScryDeps допустим — правило
		// просто тихо пропускается, остальные работают.
		var scryDeps *reaper.ScryDeps
		if d.scenarioRunner != nil && d.serviceRegistry != nil {
			scryDeps = &reaper.ScryDeps{
				Pool:         d.pool,
				DriftChecker: d.scenarioRunner,
				Services:     d.serviceRegistry,
				Audit:        d.auditWriter,
			}
		}

		// OldErrands — реализация `purge_old_errands` (ADR-033). Зависит только
		// от d.pool, который к этому моменту уже инициализирован. nil-pool
		// сценарий не предусмотрен (PG обязателен для Keeper-а), но для
		// единообразия с прочими опц.-deps оставляем условие.
		var oldErrandsPurger *reaper.ErrandsPurger
		if d.pool != nil {
			oldErrandsPurger = reaper.NewErrandsPurger(d.pool, logger)
		}

		// VoyageReclaim — реализация `reclaim_voyages` (ADR-043 S4). Зависит
		// только от d.pool; правило default-ON через path-defaulting в
		// reaper.dispatch (ADR-043 §8), поэтому безусловный wire-up обязателен —
		// без него default-ON-правило деградировало бы в warn+skip.
		if d.pool != nil {
			d.voyageReclaimer = reaper.NewVoyageReclaimer(d.pool, d.auditWriter, logger)
		}

		// OrphanEphemeralTidings — реализация `purge_orphan_ephemeral_tidings`
		// (ADR-052(g) N2). Снос осиротевших ephemeral-Tiding-ов с grace после
		// терминала Voyage. Зависит только от d.pool; правило map-driven (OFF без
		// явного enabled: true в reaper.rules).
		var orphanEphemeralTidingsPurger *reaper.EphemeralTidingsPurger
		if d.pool != nil {
			orphanEphemeralTidingsPurger = reaper.NewEphemeralTidingsPurger(d.pool, logger)
		}

		runner, err := reaper.NewRunner(reaper.Deps{
			Purger:                 executor,
			Redis:                  rc,
			Store:                  d.store,
			Holder:                 cfg.KID,
			Logger:                 logger,
			Metrics:                reaperMetrics,
			Scry:                   scryDeps,
			OldErrands:             oldErrandsPurger,
			VoyageReclaim:          d.voyageReclaimer,
			OrphanEphemeralTidings: orphanEphemeralTidingsPurger,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "keeper run: build reaper: %v\n", err)
			return errSetupFailed
		}
		go func() {
			defer close(reaperDone)
			if err := runner.Run(reaperCtx); err != nil {
				logger.Error("reaper stopped with error", slog.Any("error", err))
			}
		}()
	} else {
		logger.Info("keeper run: reaper disabled in config")
	}
	return nil
}

// setupConductor — Conductor-loop (ADR-048): leader-elected исполнитель Cadence-
// расписаний под своим lease `conductor:leader`, независимым от reaper-lease.
// Default-ON при наличии Redis (footgun-guard ADR-048 §5: Cadence без планировщика
// молча не спавнит Voyage); явный `cadence_scheduler.enabled: false` гасит. Своя
// частота (`cadence_scheduler.interval`, ~15s), не связанная с reaper.interval
// (1h). Метрики keeper_conductor_* регистрируются только в этой ветке
// (cardinality-safe, parity setupReaper). Cleanup — conductorCancel +
// conductorDone-wait (parity reaper).
//
// switchover-безопасность (ADR-048 §3, C4): Reaper в этом же изменении ПЕРЕСТАЛ
// исполнять `spawn_due_cadence`, Conductor НАЧАЛ. Окна двойного/нулевого спавна
// нет — даже если на миг тикали оба, FOR UPDATE SKIP LOCKED отдал бы due-строку
// лишь одному исполнителю, а advance next_run_at в той же tx убрал бы её из due
// для другого.
func (d *daemon) setupConductor(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	// Conductor требует non-nil Redis (lease-лидерство), как и Reaper: без Redis
	// leader-election невозможна (single-instance dev без Redis деградирует).
	// Default-ON резолвится через CadenceSchedulerEnabled (nil/не задано → ON);
	// явный false → не поднимаем.
	if d.redisClient == nil || !cfg.CadenceScheduler.CadenceSchedulerEnabled() {
		logger.Info("keeper run: conductor disabled (нет Redis или cadence_scheduler.enabled: false)")
		return nil
	}
	if d.pool == nil {
		// PG обязателен для Keeper-а; defensive-skip ради единообразия с прочими
		// PG-зависимыми подсистемами.
		logger.Warn("keeper run: conductor пропущен — нет PG-пула")
		return nil
	}
	rc := d.redisClient

	conductorCtx, conductorCancel := context.WithCancel(ctx)
	conductorDone := make(chan struct{})

	// 2. Ждём завершения conductor-goroutine (parity reaper-cleanup).
	d.cleanups.push(func() {
		select {
		case <-conductorDone:
		case <-time.After(5 * time.Second):
			logger.Warn("conductor did not stop within 5s after shutdown — leak suspected")
		}
	})
	// 1. Отменяем conductorCtx (зарегистрирован позже → LIFO выполнит первым).
	d.cleanups.push(conductorCancel)

	// Метрики Conductor — только в этой ветке (cardinality-safe).
	conductorMetrics := conductor.RegisterConductorMetrics(d.metricsReg)

	// Spawner — concrete CadenceSpawner (переехал в conductor, C3). Резолверы —
	// те же PG-резолверы, что POST /v1/voyages (пере-резолв target рецепта
	// just-in-time при спавне). Спавн с source: background (ADR-048 §4).
	spawner := conductor.NewCadenceSpawner(
		d.pool,
		cadenceScenarioResolver{inner: handlers.NewVoyageScenarioPGResolver(d.pool)},
		cadenceCommandResolver{inner: handlers.NewVoyageCommandPGResolver(d.pool)},
		d.auditWriter,
		logger,
	)

	sch, err := conductor.New(conductor.Config{
		Holder:  cfg.KID,
		Redis:   rc,
		Logger:  logger,
		Spawner: spawner,
		// Hot-reload: коридор опроса/lock_ttl перечитываются на каждом тике/
		// re-acquire из свежего Store-снимка (parity reaper). nil-cfg (невалидный
		// reload) → дефолт через nil-safe резолверы.
		//
		// Адаптивный шаг (ADR-048 «Adaptive interval»): clamp(derivedMinPeriod,
		// poll_floor, poll_ceiling); пустой enabled-реестр → poll_idle. IntervalFn
		// stateless — зовётся только с лидера (leaderloop), пересчитывает шаг из PG
		// каждый тик, поэтому новый лидер после failover не несёт in-memory
		// состояния опроса. Снимок config (floor/ceiling/idle) читается на каждом
		// resolve → hot-reload смены коридора видна со следующего тика.
		IntervalFn:    d.conductorPollInterval(ctx),
		LockTTLFn:     func() time.Duration { return conductorSchedulerCfg(d.store.Get()).ResolvedLockTTL() },
		Metrics:       conductorMetrics,
		OnLeaseChange: conductorMetrics.SetLeaseHeld,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build conductor: %v\n", err)
		return errSetupFailed
	}
	go func() {
		defer close(conductorDone)
		if err := sch.Run(conductorCtx); err != nil {
			logger.Error("conductor stopped with error", slog.Any("error", err))
		}
	}()
	return nil
}

// conductorSchedulerCfg извлекает блок cadence_scheduler из (возможно nil)
// Store-снимка. nil-cfg (невалидный reload) → nil-блок: резолверы Conductor-а
// nil-safe и подставят дефолты.
func conductorSchedulerCfg(cfg *config.KeeperConfig) *config.KeeperCadenceScheduler {
	if cfg == nil {
		return nil
	}
	return cfg.CadenceScheduler
}

// conductorPollInterval строит адаптивную IntervalFn Conductor (ADR-048 «Adaptive
// interval»): шаг опроса = clamp(derivedMinPeriod, poll_floor, poll_ceiling);
// пустой enabled-реестр Cadence → poll_idle. Вся логика — в pure
// [conductor.AdaptivePollInterval]; здесь только связывание с config-снимком
// (hot-reload коридора) и PG-пулом.
//
// ctx — родительский conductorCtx: отменяется при shutdown, обрывает
// derivedMinPeriod-запрос вместе с tick-loop-ом.
func (d *daemon) conductorPollInterval(ctx context.Context) func() time.Duration {
	corridor := func() conductor.PollCorridor {
		cs := conductorSchedulerCfg(d.store.Get())
		return conductor.PollCorridor{
			Floor:   cs.ResolvedPollFloor(),
			Ceiling: cs.ResolvedPollCeiling(),
			Idle:    cs.ResolvedPollIdle(),
		}
	}
	fetcher := cadencePoolFetcher{pool: d.pool}
	return func() time.Duration {
		return conductor.AdaptivePollInterval(ctx, corridor, fetcher, d.logger)
	}
}

// cadencePoolFetcher адаптирует pgxpool.Pool к [conductor.MinPeriodFetcher].
type cadencePoolFetcher struct{ pool *pgxpool.Pool }

func (f cadencePoolFetcher) SelectMinPeriod(ctx context.Context) (cadence.MinPeriod, error) {
	return cadence.SelectMinPeriod(ctx, f.pool)
}
