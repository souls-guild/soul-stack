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
	keeperauth "github.com/souls-guild/soul-stack/keeper/internal/auth"
	keeperldap "github.com/souls-guild/soul-stack/keeper/internal/auth/ldap"
	keeperoidc "github.com/souls-guild/soul-stack/keeper/internal/auth/oidc"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/certissue"
	"github.com/souls-guild/soul-stack/keeper/internal/certpolicy"
	"github.com/souls-guild/soul-stack/keeper/internal/cloudinit"
	"github.com/souls-guild/soul-stack/keeper/internal/conductor"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	coremodcert "github.com/souls-guild/soul-stack/keeper/internal/coremod/cert"
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
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/secretwrite"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/keeper/internal/toll"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
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

// errSetupFailed -- sentinel error for a setupX step: means "stderr has
// already been printed inside the step, the orchestrator only needs to exit
// with exitError". Every setupX prints its own meaningful message (as the
// former runDaemon did before `return exitError`), so the orchestrator does
// NOT print the error again -- it only maps it to an exit code.
var errSetupFailed = errors.New("keeper run: setup step failed")

// cleanupStack -- LIFO stack of daemon cleanup functions. Replaces the
// scattered `defer`s in the former monolithic runDaemon: every setupX method
// registers its teardown via push (NOT via its own defer -- that would fire
// on exit from setupX, not from runDaemon, breaking graceful shutdown).
// runLIFO is invoked by the orchestrator's single defer and reproduces the
// order of the former defers one-to-one (last registered -- first executed).
type cleanupStack struct{ fns []func() }

func (c *cleanupStack) push(fn func()) { c.fns = append(c.fns, fn) }

func (c *cleanupStack) runLIFO() {
	for i := len(c.fns) - 1; i >= 0; i-- {
		c.fns[i]()
	}
}

// daemon -- accumulated wiring state for `keeper run`. Fields are grouped by
// subsystem; each setupX method reads the already-filled fields of previous
// steps and writes its own. Adding a new dependency thus means a new field +
// a read inside its own method, with no change to the orchestrator (see the
// refactor motivation).
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

	// --- service-registry (Service/keeper_settings registry in PG, ADR-029) ---
	// serviceHolder -- read-only in-memory snapshot of the registry (TTL-poll +
	// pub/sub invalidation); scenario consumers (serviceRegistry/
	// destinySource, S4) read from it. serviceSvc -- CRUD facade (OpenAPI/MCP, S3).
	serviceHolder *serviceregistry.Holder
	serviceSvc    *serviceregistry.Service
	// serviceRefs -- TTL cache of the git-ls-remote tag/branch listing for
	// `GET /v1/services/{name}/refs` (UI Upgrade-modal dropdown). Per-keeper,
	// not cluster-wide (refs are read-only; lag between instances does not
	// break registry consistency).
	serviceRefs *serviceregistry.RefsCache

	// serviceScenarios -- TTL cache of the scenario listing from a
	// materialized snapshot of the Service's git repo for
	// `GET /v1/services/{name}/scenarios` (UI Run-modal dropdown). Per-keeper,
	// not cluster-wide -- read-only listing.
	serviceScenarios *serviceregistry.ScenariosCache

	// serviceStateSchema -- TTL cache of state_schema metadata (version +
	// declared schema + migrations metadata) from a materialized snapshot of
	// the Service's git repo for `GET /v1/services/{name}/state-schema` (UI
	// Schema explorer). Per-keeper, not cluster-wide -- read-only listing
	// (parity with serviceScenarios).
	serviceStateSchema *serviceregistry.StateSchemaCache

	// serviceDependencies -- TTL cache of git dependencies (destiny/modules
	// from `service.yml`) from a materialized snapshot of the Service's git
	// repo for `GET /v1/services/{name}/dependencies` (UI Service Detail).
	// Per-keeper, not cluster-wide -- read-only listing (parity with
	// serviceStateSchema).
	serviceDependencies *serviceregistry.DependenciesCache

	// serviceDirectives -- TTL cache of the valid redis.conf directive
	// catalog by version (essence.redis_directives) from a snapshot of the
	// Service's git repo for `GET /v1/services/{name}/directives` (UI
	// redis_settings editor). Per-keeper, not cluster-wide -- read-only
	// catalog (parity with serviceDependencies).
	serviceDirectives *serviceregistry.DirectivesCache

	// serviceCertPolicy — TTL-кеш секции `certificate_rotation:` манифеста Service-а
	// (parity с serviceStateSchema); питает certPolicyResolver. Per-keeper, read-only.
	serviceCertPolicy *serviceregistry.CertPolicyCache

	// certPolicyResolver — резолвер эффективной cert-rotation-политики инкарнации
	// (incarnation → пиновый снапшот сервиса → секция certificate_rotation). Общий
	// вход reaper.CertRotator (кого ротировать) и core.cert.issued (роль подписи).
	certPolicyResolver *certpolicy.Resolver

	// serviceTelemetry — TTL-кеш дефолтного (per-service, без essence) host-vitals
	// telemetry-конфига из манифеста снапшота git-репо Service-а для
	// `GET /v1/services/{name}/telemetry` (UI-редактор, ADR-042/072). Per-keeper,
	// не cluster-wide — read-only конфиг (parity с serviceDirectives).
	serviceTelemetry *serviceregistry.TelemetryCache

	// --- augur (ADR-025) ---
	// augurSvc -- management CRUD for the Omen / Rite registries (OpenAPI/MCP).
	// DIFFERS from the Augur broker (keepergrpc.AugurDeps in EventStream): that
	// resolves AugurRequest from a Soul, this is operator-facing record management.
	augurSvc *keeperaugur.Service

	// --- oracle (ADR-030 beacons) ---
	// oracleSvc -- management CRUD for the Vigil / Decree registries (OpenAPI/MCP).
	// DIFFERS from the reactor router (oracleScenarioEnqueuer in EventStream):
	// that resolves a Portent from a Soul, this is operator-facing record management.
	oracleSvc *oracle.Service

	// --- sigil (ADR-026) ---
	// nil = Sigil disabled (no sigil.signing_key_ref): plugin.*-routes/tools
	// are not registered (rbacSvc pattern). Constructor is nil-safe (see setupSigil).
	sigilSvc *sigil.Service
	// sigilKeySvc -- operator-facing rotation of Sigil signing trust-anchor
	// keys (ADR-026(h), R3-S7): introduce (key-gen+Vault-write) / retire /
	// set-primary / list. nil when Sigil is disabled (like sigilSvc). Filled
	// in setupSigil; read by setupAPIServer / setupMCPServer /
	// setupSigilInvalidation (publisher).
	sigilKeySvc *sigil.KeyService
	// sigilAnchors -- the SET of Sigil signing trust anchors (ADR-026(h), R3
	// multi-anchor) for the keeper-host to verify ITS OWN plugins (ADR-026(f),
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	sigilAnchors []ed25519.PublicKey
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	sigilAnchorSource *trustAnchorHolder
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	sigilHost *pluginhost.Host
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	sigilKeyMetrics *sigil.KeyMetrics

	// --- audit ---
	auditWriter audit.Writer

	// --- herald (ADR-052) ---
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	heraldDispatcher *herald.Dispatcher
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	heraldTap *herald.NotificationTap
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	heraldDeliveryMetrics *herald.DeliveryMetrics
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// in-process invalidate (heraldDispatcher) + cross-keeper Redis-publisher.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	heraldSvc *herald.Service
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	heraldInvalidation *keeperredis.HeraldInvalidateSubscription
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	providerSvc *provider.Service
	profileSvc  *profile.Service

	// --- metrics ---
	metricsReg      *obs.Registry
	httpMetrics     *obs.HTTPMetrics
	grpcMetrics     *keepergrpc.GRPCMetrics
	scenarioMetrics *scenario.ScenarioMetrics
	renderMetrics   *render.RenderMetrics
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	augurMetrics *keeperaugur.BrokerMetrics

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	oracleMetrics *oracle.OracleMetrics

	// --- scenario deps ---
	serviceLoader    *artifact.ServiceLoader
	topologyResolver *topology.Resolver
	essenceResolver  *essence.Resolver
	renderPipeline   *render.Pipeline
	serviceRegistry  *scenario.ServiceRegistry
	destinySource    *scenario.DestinySource
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	coreModules *coremod.Registry

	// --- push orchestrator (Variant C, docs/keeper/push.md) ---
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushDestinyLoader *artifact.DestinyLoader
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushDiscoveredSsh []pluginhost.Discovered
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushPluginHost *pluginhost.Host
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushSshPlugin *pluginhost.SshProviderPlugin
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// config-backed targets/providers + single Vault host-CA).
	pushDispatcher pushorch.SshDispatcher
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushSshDispatcher *push.SshDispatcher
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushCleaner pushorch.Cleaner
	// Keeper daemon runtime wiring note.
	// renderPipeline + topologyResolver + pushDestinyLoader + serviceHolder +
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushRun *pushorch.PushRun
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushProviderSvc *pushprovider.Service
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushProviderInvalidation *keeperredis.PushProvidersChangedSubscription
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pushMetrics *push.Metrics

	// --- redis / apply-bus ---
	redisClient *keeperredis.Client
	applyBus    *applybus.EventBus

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	conclaveInstances prometheus.Gauge

	// --- grpc ---
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	streamManager  *keepergrpc.StreamManager
	outbound       *keepergrpc.Outbound
	scenarioRunner *scenario.Runner

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	watchmanMetrics *watchmanMetrics

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	tollMetrics *toll.Metrics
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// `toll.enabled: false`) → eventstream notifyTollDisconnect no-op.
	tollWatcher *toll.Watcher
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	tollDegradedReader toll.DegradedReader
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	tollLeader *toll.Leader
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	tollWebhookCfg *config.KeeperTollWebhook

	// --- tempo (per-AID rate-limiter write-API, ADR-050) ---
	// tempoMetrics — keeper_tempo_allowed_total / keeper_tempo_rejected_total
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	tempoMetrics *api.TempoMetrics
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// (hot-reload, ADR-050(f)).
	tempoLimiter *keeperredis.TokenBucket

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	loginGuard *keeperredis.LoginGuard

	// --- acolyte (ADR-027) ---
	acolytePool *acolyte.Pool

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	voyageReclaimer *reaper.VoyageReclaimer

	// --- errand (ADR-033) ---
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	errandStore      *errand.Store
	errandDispatcher *errand.Dispatcher

	// --- voyage (ADR-043, S1) ---
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	voyagePoolStarted bool

	// Keeper daemon runtime wiring note.
	apiServer *api.Server
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	initialize = initialize || envTruthy("KEEPER_INITIALIZE")
	d.initialize = initialize

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupObservabilityEarly(ctx context.Context) error {
	cfg := d.cfg
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	logger, logLevel := shlog.NewWithLevel(shlog.FromKeeper(cfg.Logging))
	d.logger = logger

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.store.OnReload(func(_, newCfg *config.KeeperConfig) {
		if newCfg != nil {
			logLevel.Set(newCfg.Logging.Level)
		}
	})

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if cfg.HotReload.SignalEnabled() {
		reloadCh := config.WatchSIGHUP(ctx, d.store)
		go config.LogReloads(reloadCh, logger)
		logger.Info("keeper run: SIGHUP config reload enabled")
	} else {
		logger.Info("keeper run: SIGHUP config reload disabled (hot_reload.enable_signal=false)")
	}
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupVault(ctx context.Context) error {
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	vc, err := keepervault.NewClient(ctx, d.cfg.Vault)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: vault client: %v\n", err)
		return errSetupFailed
	}
	d.vc = vc
	logger := d.logger

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	renewerCtx, renewerCancel := context.WithCancel(ctx)
	tokenRenewer, err := vc.StartTokenRenewer(renewerCtx, logger)
	if err != nil {
		renewerCancel()
		fmt.Fprintf(os.Stderr, "keeper run: vault token renewer: %v\n", err)
		return errSetupFailed
	}
	// Keeper daemon runtime wiring note.
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
	// Keeper daemon runtime wiring note.
	d.cleanups.push(renewerCancel)
	return nil
}

// setupStorage — PG pool + Ping + ResolveDSN + Apply migrations. pool
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupOperatorBootstrapGuard(ctx context.Context) error {
	n, err := operator.CountNonSystem(ctx, d.pool)
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

// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupRBAC(ctx context.Context) error {
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	rbacHolder, err := rbac.NewHolder(ctx, rbac.PoolSource{DB: d.pool}, rbac.DefaultRefreshInterval, d.logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build RBAC enforcer: %v\n", err)
		return errSetupFailed
	}
	go rbacHolder.Run(ctx)
	d.rbacHolder = rbacHolder

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	rbacSvc, err := rbac.NewService(rbac.ServiceDeps{Pool: d.pool, Logger: d.logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build RBAC service: %v\n", err)
		return errSetupFailed
	}
	d.rbacSvc = rbacSvc
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupServiceRegistry(ctx context.Context) error {
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	holder, err := serviceregistry.NewHolder(ctx, serviceregistry.PoolSource{DB: d.pool}, serviceregistry.DefaultRefreshInterval, d.logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build service-registry holder: %v\n", err)
		return errSetupFailed
	}
	go holder.Run(ctx)
	d.serviceHolder = holder

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// (setupServiceRegistryInvalidation).
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: d.pool, Logger: d.logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build service-registry service: %v\n", err)
		return errSetupFailed
	}
	d.serviceSvc = svc

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.serviceRefs = serviceregistry.NewRefsCache(
		artifact.RefsListerFunc(artifact.ListRefs),
		0, // Keeper daemon runtime wiring note.
	)

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	augurSvc, err := keeperaugur.NewService(keeperaugur.ServiceDeps{Pool: d.pool, Logger: d.logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build augur service: %v\n", err)
		return errSetupFailed
	}
	d.augurSvc = augurSvc

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupAudit(_ context.Context) error {
	auditWriter := audit.Writer(auditpg.NewWriter(d.pool))

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.store.SetAuditWriter(auditWriter)
	return nil
}

// Keeper daemon runtime wiring note.
// cloud-adapter + coremod.Default. Discovery best-effort.
func (d *daemon) setupCoreModules(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// PluginHost: NewHost + Discover + FilterByCatalog. Discovery
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	sigilLookup := pluginhost.NewSigilLookupAdapter(sigilRecordLister{store: sigil.NewPGStore(d.pool)}, logger)
	pluginHost, err := pluginhost.NewHost(cfg.PluginRuntime, d.sigilAnchors, sigilLookup)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build plugin host: %v\n", err)
		return errSetupFailed
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.sigilHost = pluginHost
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.pushPluginHost = pluginHost
	cacheRoot := pluginCacheRoot(cfg.Plugins)

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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
				// Keeper daemon runtime wiring note.
				// Keeper daemon runtime wiring note.
				d.pushDiscoveredSsh = append(d.pushDiscoveredSsh, dd)
			}
		}
	}
	cloudAdapter, err := cloud.NewPluginAdapter(pluginHost, discoveredCloud)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build cloud plugin adapter: %v\n", err)
		return errSetupFailed
	}
	// Keeper daemon runtime wiring note.
	// `generate_userdata: true` (ADR-017(h) amendment 2026-05-27, B-flat).
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	userdataProvider := &cloudInitProvider{store: d.store, resolver: cloudinit.NewResolver(d.vc)}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var bootstrapTransport string
	var bootstrapDial push.Dialer
	if cfg.Push != nil && cfg.Push.Transport == config.PushTransportTeleport {
		td, terr := buildBootstrapTeleportDialer(cfg.Push)
		if terr != nil {
			fmt.Fprintf(os.Stderr, "keeper run: build bootstrap teleport dialer: %v\n", terr)
			return errSetupFailed
		}
		bootstrapTransport = config.PushTransportTeleport
		bootstrapDial = td
		logger.Info("keeper run: bootstrap token delivery transport = teleport (by-name)",
			slog.String("proxy_addr", cfg.Push.Teleport.ProxyAddr),
			slog.String("cluster", cfg.Push.Teleport.Cluster))
	}

	coreReg := coremod.Default(coremod.Deps{
		SoulStore: coremodsoul.NewPGStore(d.pool),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		SoulPresence: lazySoulPresence{d: d},
		MaxAwaitTimeout: func() string {
			if cfg := d.store.Get(); cfg != nil {
				return cfg.MaxAwaitTimeout
			}
			return "" // Keeper daemon runtime wiring note.
		},
		PluginHost: cloudAdapter,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		CloudResolver: cloud.NewCredentialsResolverPG(cloud.NewProviderReaderPG(d.pool), cloud.NewProfileReaderPG(d.pool), d.vc),
		CloudSouls:    cloud.NewSoulPG(d.pool),
		CloudTokens:   cloud.NewTokenPG(d.pool, cloud.DefaultBootstrapTokenTTL),
		CloudCascade:  cloud.NewCascadePG(d.pool),
		CloudUserdata: userdataProvider,
		Vault:         d.vc,
		Audit:         d.auditWriter,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ChoirStore: coremodchoir.NewPGStore(d.pool),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		CertStore: coremodcert.NewPGStore(d.pool),
		KID:       cfg.KID,
		// Cert* — state `core.cert.issued` (NIM-99): общий signer/writer/csrgen с
		// reaper.CertRotator. CertPolicy — лениво (резолвер строится в setupScenarioDeps
		// ПОСЛЕ этого шага); PKIMount — hot-reload keeper.yml snapshot.
		CertSigner:      certPKISignerAdapter{vc: d.vc},
		CertVaultWriter: d.vc,
		CertPolicy:      lazyCertPolicy{d: d},
		CertCSRGen:      certServiceCSRGen,
		CertPKIMount: func() string {
			if c := d.store.Get(); c != nil {
				return c.Vault.PKIMount
			}
			return ""
		},
		// `core.bootstrap.delivered` teleport-режим (ADR-063 amendment): dialer
		// из keeper.yml::push.teleport. nil/"" → direct, и т.к. direct-набор
		// (providers/host-CA) тут не заполнен, модуль не регистрируется.
		BootstrapTransport: bootstrapTransport,
		BootstrapDial:      bootstrapDial,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		BootstrapInstall: userdataProvider,
	})
	logger.Info("keeper run: core modules registered",
		slog.Int("count", len(coreReg.Names())),
		slog.Any("cloud_providers", cloudAdapter.Providers()))
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.coreModules = coreReg
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (p *cloudInitProvider) GenerateUserdataSelfOnboard(ctx context.Context, tokens map[string]string) (string, error) {
	cfg := p.store.Get()
	if cfg == nil {
		return "", fmt.Errorf("cloud_init: keeper config snapshot is nil")
	}
	resolved, err := p.resolver.Resolve(ctx, cfg.CloudInit)
	if err != nil {
		return "", err
	}
	return cloudinit.GenerateUserdataSelfOnboard(resolved, tokens)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (p *cloudInitProvider) Resolve(ctx context.Context) (cloudinit.Config, error) {
	cfg := p.store.Get()
	if cfg == nil {
		return cloudinit.Config{}, fmt.Errorf("cloud_init: keeper config snapshot is nil")
	}
	return p.resolver.Resolve(ctx, cfg.CloudInit)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func buildBootstrapTeleportDialer(p *config.KeeperPush) (push.Dialer, error) {
	if p.Teleport == nil {
		return nil, fmt.Errorf("push.transport=teleport requires push.teleport block")
	}
	return push.NewTeleportDialer(push.TeleportDialerConfig{
		ProxyAddr:      p.Teleport.ProxyAddr,
		IdentityFile:   p.Teleport.IdentityFile,
		Cluster:        p.Teleport.Cluster,
		UseSystemTrust: p.Teleport.UseSystemTrust,
		AlpnUpgrade:    p.Teleport.AlpnUpgrade,
	})
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type moduleCatalogPlugins struct {
	store sigil.Store
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupMetricsRegistry(_ context.Context) error {
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	metricsReg := obs.NewRegistry()
	d.metricsReg = metricsReg
	d.httpMetrics = obs.RegisterHTTPMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.grpcMetrics = keepergrpc.RegisterGRPCMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.scenarioMetrics = scenario.RegisterScenarioMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.renderMetrics = render.RegisterRenderMetrics(metricsReg)
	// keeper_mask_regex_fallback_total + process-global audit.SetSealHooks —
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	setupMaskMetrics(metricsReg, d.logger)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.vc.SetMetrics(keepervault.RegisterVaultMetrics(metricsReg))
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.rbacHolder.SetMetrics(rbac.RegisterRBACMetrics(metricsReg))
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.serviceHolder.SetMetrics(serviceregistry.RegisterRegistryMetrics(metricsReg))
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.augurMetrics = keeperaugur.RegisterBrokerMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.oracleMetrics = oracle.RegisterOracleMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.conclaveInstances = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "keeper_conclave_instances",
		Help: "Current number of live keeper instances in Conclave (presence registry in Redis).",
	})
	metricsReg.Registerer().MustRegister(d.conclaveInstances)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.watchmanMetrics = registerWatchmanMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// ADR-038): per-instance Watcher (counter disconnects + warmup/graceful
	// skipped) + cluster-level Leader (gauge cluster_degraded + gauge
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.tollMetrics = toll.RegisterMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.tempoMetrics = api.RegisterTempoMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	heraldMetrics := herald.RegisterDispatcherMetrics(metricsReg)
	d.heraldTap.SetMetrics(heraldMetrics)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.heraldDeliveryMetrics = herald.RegisterDeliveryMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.pushMetrics = push.RegisterMetrics(metricsReg)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if d.sigilKeySvc != nil {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		d.sigilKeyMetrics = sigil.RegisterKeyMetrics(metricsReg)
		d.sigilKeySvc.SetMetrics(d.sigilKeyMetrics)
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		d.sigilKeySvc.PrimeActiveGauge(context.Background())
	}
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupMetricsListener(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupOTel(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupScenarioDeps(_ context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.serviceLoader = artifact.NewServiceLoader(serviceCacheRoot(cfg), logger)

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.serviceScenarios = serviceregistry.NewScenariosCache(
		serviceregistry.ScenarioListerFunc(func(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error) {
			art, err := d.serviceLoader.Load(ctx, artifact.ServiceRef{Name: name, Git: gitURL, Ref: ref})
			if err != nil {
				return nil, err
			}
			return artifact.ListScenarios(art.LocalDir, logger)
		}),
		0, // Keeper daemon runtime wiring note.
	)

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.serviceStateSchema = serviceregistry.NewStateSchemaCache(
		serviceregistry.StateSchemaListerFunc(func(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error) {
			art, err := d.serviceLoader.Load(ctx, artifact.ServiceRef{Name: name, Git: gitURL, Ref: ref})
			if err != nil {
				return nil, err
			}
			return artifact.ListStateSchema(art.LocalDir, logger)
		}),
		0, // Keeper daemon runtime wiring note.
	)

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.serviceDependencies = serviceregistry.NewDependenciesCache(
		serviceregistry.DependenciesListerFunc(func(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error) {
			art, err := d.serviceLoader.Load(ctx, artifact.ServiceRef{Name: name, Git: gitURL, Ref: ref})
			if err != nil {
				return nil, err
			}
			return artifact.ListDependencies(art.LocalDir, logger)
		}),
		0, // Keeper daemon runtime wiring note.
	)

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.serviceDirectives = serviceregistry.NewDirectivesCache(
		serviceregistry.DirectiveListerFunc(func(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error) {
			art, err := d.serviceLoader.Load(ctx, artifact.ServiceRef{Name: name, Git: gitURL, Ref: ref})
			if err != nil {
				return nil, err
			}
			dirs, err := artifact.LoadDirectiveCatalog(art.LocalDir, "")
			if err != nil {
				return nil, err
			}
			return &artifact.DirectiveCatalog{SHA1: art.SHA1, Directives: dirs}, nil
		}),
		0, // Keeper daemon runtime wiring note.
	)

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.

	// TTL-кеш дефолтного host-vitals telemetry-конфига для
	// `GET /v1/services/{name}/telemetry` (ADR-042/072). Lister грузит снапшот через
	// d.serviceLoader.Load → эффективные манифест-дефолты `telemetry:` (essence=nil →
	// чистый per-service дефолт) + SHA1 снапшота (ETag). Parity с serviceDirectives.
	d.serviceTelemetry = serviceregistry.NewTelemetryCache(
		serviceregistry.TelemetryListerFunc(func(ctx context.Context, name, gitURL, ref string) (*serviceregistry.TelemetryCatalog, error) {
			art, err := d.serviceLoader.Load(ctx, artifact.ServiceRef{Name: name, Git: gitURL, Ref: ref})
			if err != nil {
				return nil, err
			}
			var mt *config.TelemetryConfig
			if art.Manifest != nil {
				mt = art.Manifest.Telemetry
			}
			return &serviceregistry.TelemetryCatalog{
				SHA1:      art.SHA1,
				Telemetry: essence.ResolveEffectiveTelemetry(mt, nil),
			}, nil
		}),
		0, // 0 → дефолтный TelemetryTTL
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
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.serviceRegistry = scenario.NewServiceRegistry(d.serviceHolder)
	// serviceCertPolicy + certPolicyResolver (NIM-99): TTL-кеш секции
	// certificate_rotation манифеста (parity с прочими service*-кешами) и резолвер
	// эффективной политики инкарнации. Питают reaper.CertRotator (кого ротировать) и
	// core.cert.issued (роль PKI-подписи из манифеста).
	d.serviceCertPolicy = serviceregistry.NewCertPolicyCache(
		serviceregistry.CertPolicyListerFunc(func(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error) {
			return d.serviceLoader.LoadCertPolicy(ctx, artifact.ServiceRef{Name: name, Git: gitURL, Ref: ref})
		}), 0)
	d.certPolicyResolver = certpolicy.NewResolver(d.pool, d.serviceRegistry, d.serviceCertPolicy)
	// Источник destiny-артефактов для apply:destiny (ADR-009): git-URL —
	// default_destiny_source + {name} (читается ЛЕНИВО из serviceHolder, чтобы
	// hot-reload скаляра доезжал), ref — service.yml::destiny[].
	destinyLoader := artifact.NewDestinyLoader(destinyCacheRoot(cfg), logger)
	d.destinySource = scenario.NewDestinySource(destinyLoader, d.serviceHolder)
	return nil
}

// setupPushOrchestrator — multi-host push-orchestrator (Variant C,
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupPushOrchestrator(_ context.Context) error {
	d.pushDestinyLoader = artifact.NewDestinyLoader(destinyCacheRoot(d.cfg), d.logger)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	return nil
}

// setupPushDispatchers — wire-up SshDispatcher (S6 pilot + S7-1 PG-canon,
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
//	`push.allow_legacy_push_targets` (1-release WARN deprecation window);
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
//	`push.providers[].params` (ADR-020 amendment l, env-convention);
//
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupPushDispatchers(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if cfg.Plugins == nil || len(cfg.Plugins.SSHProviders) == 0 {
		logger.Info("keeper run: push dispatcher disabled (plugins.ssh_providers[] not declared) - /v1/push/* and MCP keeper.push.apply will return 'not configured'")
		return nil
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if len(d.pushDiscoveredSsh) == 0 {
		logger.Warn("keeper run: push dispatcher disabled (no discovered SshProvider plugins in cache) - /v1/push/* unavailable")
		return nil
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if cfg.Push == nil || (cfg.Push.HostCARef == "" && len(cfg.Push.HostCARefs) == 0) {
		logger.Warn("keeper run: push dispatcher disabled (push.host_ca_refs[] / host_ca_ref not set) - /v1/push/* unavailable; configure keeper.yml::push.host_ca_refs[] to enable")
		return nil
	}

	hostCARefs := cfg.Push.HostCARefs
	if len(hostCARefs) == 0 {
		// Keeper daemon runtime wiring note.
		logger.Warn("keeper run: push.host_ca_ref deprecated (S7-3 ADR-032 amendment 2026-05-26); auto-adapted into host_ca_refs[0] with name='default'. Replace with push.host_ca_refs[{ref, name}] before hard-cut.",
			slog.String("singular_ref", cfg.Push.HostCARef))
		hostCARefs = []config.KeeperPushCARef{{
			Ref:  cfg.Push.HostCARef,
			Name: config.DefaultHostCAName,
		}}
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	hostAuthorities, err := push.LoadHostCAs(ctx, d.vc, hostCARefs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: push dispatcher resolve host_ca_refs: %v\n", err)
		return errSetupFailed
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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
			fmt.Fprintln(os.Stderr, "keeper run: push dispatcher: discovered SshProvider without manifest (discovery programming error)")
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

		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		closer := sshPlugin
		d.cleanups.push(func() {
			if cerr := closer.Close(); cerr != nil {
				logger.Warn("keeper run: push SshProvider plugin close returned error",
					slog.String("plugin", pluginName), slog.Any("error", cerr))
			}
		})
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if len(providers) > 0 {
		for _, entry := range providers {
			if e, ok := entry.Closer.(*pluginhost.SshProviderPlugin); ok {
				d.pushSshPlugin = e
				break
			}
		}
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// push.allow_legacy_push_targets (1-release WARN deprecation window,
	// [ADR-032 amendment 2026-05-26]).
	configResolver := push.NewConfigTargetResolver(cfg.Push.Targets)
	targetResolver := &push.PGFallbackTargetResolver{
		Reader:      push.NewPGTargetReader(d.pool),
		Fallback:    configResolver,
		AllowLegacy: cfg.Push.AllowLegacyPushTargets,
		Logger:      logger.With(slog.String("component", "push-target-resolver")),
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// 2026-05-27, S7-2 closure).
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) runPushProviderInvalidationListener(sub *keeperredis.PushProvidersChangedSubscription, logger *slog.Logger) {
	for ev := range sub.Channel() {
		logger.Info("keeper run: push-providers:changed received",
			slog.String("provider", ev.Name),
			slog.Time("at", ev.At))
		if d.pushSshDispatcher == nil {
			continue
		}
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type daemonPushProviderPublisher struct {
	redis *keeperredis.Client
}

func (p *daemonPushProviderPublisher) PublishPushProvidersChanged(ctx context.Context, providerName string) error {
	_, err := keeperredis.PublishPushProvidersChanged(ctx, p.redis, providerName)
	return err
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) buildSecretWriter() (*secretwrite.Writer, error) {
	return secretwrite.NewWriter(d.vc, d.cfg.Vault.KVMount)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) acceptPlaintextSecrets() bool {
	return d.cfg.SecretIngest != nil && d.cfg.SecretIngest.AcceptPlaintext
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupHeraldSvc(ctx context.Context) error {
	logger := d.logger
	var redis herald.RedisInvalidator
	if d.redisClient != nil {
		redis = &daemonHeraldInvalidator{redis: d.redisClient}
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var inv herald.Invalidator
	if d.heraldDispatcher != nil {
		inv = d.heraldDispatcher
	}
	secretWriter, err := d.buildSecretWriter()
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build herald secret writer: %v\n", err)
		return errSetupFailed
	}
	svc, err := herald.NewService(herald.ServiceDeps{
		Pool:            d.pool,
		Invalidator:     inv,
		Redis:           redis,
		Logger:          logger.With(slog.String("component", "herald-svc")),
		SecretWriter:    secretWriter,
		AcceptPlaintext: d.acceptPlaintextSecrets(),
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
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupCloudCRUD(_ context.Context) error {
	secretWriter, err := d.buildSecretWriter()
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build provider secret writer: %v\n", err)
		return errSetupFailed
	}
	provSvc, err := provider.NewService(provider.ServiceDeps{
		Pool:            d.pool,
		SecretWriter:    secretWriter,
		AcceptPlaintext: d.acceptPlaintextSecrets(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build provider service: %v\n", err)
		return errSetupFailed
	}
	d.providerSvc = provSvc

	profSvc, err := profile.NewService(d.pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build profile service: %v\n", err)
		return errSetupFailed
	}
	d.profileSvc = profSvc

	d.logger.Info("keeper run: cloud CRUD services ready (ADR-017)")
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) runHeraldInvalidationListener(sub *keeperredis.HeraldInvalidateSubscription, logger *slog.Logger) {
	for ev := range sub.Channel() {
		logger.Debug("keeper run: herald:invalidate received",
			slog.String("name", ev.Name), slog.Time("at", ev.At))
		// Keeper daemon runtime wiring note.
		d.heraldDispatcher.InvalidateRules()
	}
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type daemonHeraldInvalidator struct {
	redis *keeperredis.Client
}

func (p *daemonHeraldInvalidator) PublishHeraldInvalidate(ctx context.Context, name string) error {
	_, err := keeperredis.PublishHeraldInvalidate(ctx, p.redis, name)
	return err
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func buildPushSpawnOptsFromParams(pluginName string, params map[string]any) ([]pluginhost.SpawnOption, string, error) {
	if len(params) == 0 {
		return nil, "", nil
	}
	payload, err := json.Marshal(params)
	if err != nil {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		return nil, "", fmt.Errorf("marshal push.providers[%q].params: %w", pluginName, err)
	}
	envName := pushParamsEnvName(pluginName)
	return []pluginhost.SpawnOption{pluginhost.WithEnv([]string{envName + "=" + string(payload)})}, envName, nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) finalizePushOrchestrator(_ context.Context) error {
	if d.pushDispatcher == nil {
		d.logger.Warn("keeper run: push orchestrator disabled (SshDispatcher not configured) - /v1/push/* and MCP keeper.push.apply will return 'not configured'")
		return nil
	}
	if d.topologyResolver == nil {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		fmt.Fprintln(os.Stderr, "keeper run: push orchestrator wire-up: topologyResolver is nil (programmer error in step order)")
		return errSetupFailed
	}

	// P2 W-3 multi-provider routing: PGRouter (3-tier per-SID → per-coven →
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// PublishErrand), d.applyBus (subscribe-waiter), d.redisClient (LeaseLookup —
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var lookup errand.LeaseLookup
	if d.redisClient != nil {
		lookup = errandLeaseLookup{rc: d.redisClient}
	}

	disp, err := errand.NewDispatcher(errand.Deps{
		Store:       store,
		Outbound:    d.outbound,
		Publisher:   d.outbound, // Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type errandLeaseLookup struct{ rc *keeperredis.Client }

func (l errandLeaseLookup) ReadHolder(ctx context.Context, sid string) (string, error) {
	return keeperredis.ReadSoulLeaseHolder(ctx, l.rc, sid)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type errandApplyBusBridge struct{ bus *applybus.EventBus }

func (b errandApplyBusBridge) Subscribe(ctx context.Context, applyID string) <-chan applybus.Event {
	return b.bus.Subscribe(ctx, applyID)
}

func (b errandApplyBusBridge) SubscribeWithBridge(ctx context.Context, applyID string, wantBridge bool) <-chan applybus.Event {
	return b.bus.SubscribeWithBridge(ctx, applyID, wantBridge)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupGRPCBootstrap(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// (ADR-012(b)).
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func redisConfigured(r config.KeeperRedis) bool {
	switch r.Mode {
	case keeperredis.ModeSentinel:
		return len(r.Sentinels) > 0
	case keeperredis.ModeCluster:
		return len(r.Nodes) > 0
	default: // Keeper daemon runtime wiring note.
		return r.Addr != ""
	}
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupRedis(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if redisConfigured(cfg.Redis) {
		rc, err := keeperredis.NewClient(ctx, keeperredis.Config{
			Mode:                cfg.Redis.Mode,
			Addr:                cfg.Redis.Addr,
			PasswordRef:         cfg.Redis.PasswordRef,
			MasterName:          cfg.Redis.MasterName,
			Sentinels:           cfg.Redis.Sentinels,
			Nodes:               cfg.Redis.Nodes,
			SentinelPasswordRef: cfg.Redis.SentinelPasswordRef,
		}, d.vc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "keeper run: redis client: %v\n", err)
			return errSetupFailed
		}
		d.redisClient = rc
		d.cleanups.push(func() { _ = rc.Close() })
	} else {
		logger.Warn("keeper run: redis disabled (redis block has no addr/sentinels/nodes) — cluster-mode routing / SoulLease / heartbeat-cache disabled")
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.applyBus = applybus.NewBusWithRedis(logger, d.redisClient, cfg.KID)
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupHeraldDelivery(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	if d.heraldDispatcher == nil {
		// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	d.heraldDispatcher.SetQueue(herald.NewRedisDeliveryQueue(backend, logger))

	workers := cfg.Herald.ResolvedWorkers()
	if workers <= 0 {
		logger.Info("herald: delivery workers disabled (herald.workers=0) — jobs queued but not delivered")
		return nil
	}

	timeout := heraldDeliveryTimeout(cfg)

	// Keeper daemon runtime wiring note.
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
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
type heraldReaderFunc func(ctx context.Context, name string) (*herald.Herald, error)

func (f heraldReaderFunc) HeraldByName(ctx context.Context, name string) (*herald.Herald, error) {
	return f(ctx, name)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupConclave(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	if d.redisClient == nil {
		logger.Warn("keeper run: conclave disabled (redis unavailable) — instance presence registry off, soul-shedding refuse-guard inert")
		return nil
	}

	ttl := keeperredis.DefaultConclaveTTL
	renewEvery := keeperredis.DefaultConclaveRenewInterval

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	meta := conclaveMeta(cfg.KID)

	if err := keeperredis.RegisterInstance(ctx, d.redisClient, cfg.KID, meta, ttl, true); err != nil {
		if errors.Is(err, keeperredis.ErrConclaveKIDTaken) {
			logger.Warn("keeper run: conclave KID collision - another keeper instance already registered with the same kid (configuration error?), registering over it",
				slog.String("kid", cfg.KID))
			// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.cleanups.push(func() {
		relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer relCancel()
		if err := keeperredis.DeregisterInstance(relCtx, d.redisClient, cfg.KID); err != nil {
			logger.Warn("conclave deregister failed (instance key will expire by TTL)",
				slog.String("kid", cfg.KID), slog.Any("error", err))
		}
	})
	// Keeper daemon runtime wiring note.
	d.cleanups.push(func() {
		select {
		case <-renewDone:
		case <-time.After(5 * time.Second):
			logger.Warn("conclave renewal goroutine did not stop within 5s after shutdown — leak suspected")
		}
	})
	// Keeper daemon runtime wiring note.
	d.cleanups.push(renewCancel)

	go d.runConclaveRenewal(renewCtx, ttl, renewEvery, meta, renewDone)
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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
				// Keeper daemon runtime wiring note.
				// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// setupOperatorBootstrapGuard. Opt-out: cfg.AllowUnsafeSinglePathMultiKeeper
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupConclaveRefuseGuard(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if cfg.Acolytes > 0 || d.redisClient == nil {
		return nil
	}

	live, err := keeperredis.CountLive(ctx, d.redisClient)
	if err != nil {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		logger.Warn("keeper run: conclave refuse-guard - failed to list live instances, guard skipped (fail-open)",
			slog.Any("error", err))
		return nil
	}

	allowUnsafe := cfg.AllowUnsafeSinglePathMultiKeeper || envTruthy("KEEPER_ALLOW_UNSAFE_MULTI_KEEPER")
	switch decideConclaveSinglePath(cfg.Acolytes, live, allowUnsafe) {
	case conclaveSinglePathRefuse:
		fmt.Fprintln(os.Stderr, conclaveRefuseMessage(live))
		return errSetupFailed
	case conclaveSinglePathWarn:
		logger.Warn("keeper run: multi-keeper + acolytes=0 - refuse suppressed by explicit opt-out (allow_unsafe_single_path_multi_keeper); run may hang in applying under cross-keeper routing (ADR-027)",
			slog.Int("conclave_live", live),
			slog.String("self_kid", cfg.KID))
	case conclaveSinglePathOK:
		// Keeper daemon runtime wiring note.
	}
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupRBACInvalidation(ctx context.Context) error {
	// --- rbac-wiring (B2 = B1 + pub/sub, ADR-028(d)) ---
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if d.redisClient != nil {
		d.rbacSvc.SetInvalidator(rbacInvalidator{redis: d.redisClient, kid: d.cfg.KID, logger: d.logger})
		go d.rbacHolder.WatchInvalidations(ctx, rbacInvalidationSource{redis: d.redisClient, kid: d.cfg.KID, logger: d.logger})
	}
	// --- /rbac-wiring ---
	return nil
}

// setupOperatorInvalidation — JWT immediate revoke (ADR-014 Amendment
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupServiceRegistryInvalidation(ctx context.Context) error {
	if d.redisClient != nil {
		d.serviceSvc.SetInvalidator(serviceInvalidator{redis: d.redisClient, kid: d.cfg.KID, logger: d.logger})
		go d.serviceHolder.WatchInvalidations(ctx, serviceInvalidationSource{redis: d.redisClient, kid: d.cfg.KID, logger: d.logger})
	}
	return nil
}

// setupGRPCEventStream — gRPC EventStream listener (M2.2 + M2.5): StreamManager,
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// scenarioRunner.Shutdown + drain listener.
func (d *daemon) setupGRPCEventStream(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var summons scenario.SummonsPublisher
	if d.redisClient != nil {
		summons = summonsPublisher{redis: d.redisClient, kid: cfg.KID}
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var leaseOwner scenario.LeaseOwnerChecker
	if d.redisClient != nil {
		leaseOwner = leaseOwnerChecker{rc: d.redisClient}
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var passageCap scenario.PassageCapabilityChecker
	if d.redisClient != nil {
		passageCap = passageCapChecker{rc: d.redisClient}
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		Vault: d.vc,
		Audit: d.auditWriter,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ApplyBus:       d.applyBus,
		AuditReader:    auditpg.NewReader(d.pool),
		InputDenyPaths: cfg.Vault.InputDenyPaths,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		AcolyteEnabled: cfg.Acolytes > 0,
		KID:            cfg.KID,
		Summons:        summons,
		LeaseOwner:     leaseOwner,
		PassageCap:     passageCap,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		MaxAwaitTimeoutFn: func() time.Duration {
			if cfg := d.store.Get(); cfg != nil {
				return cfg.ResolvedMaxAwaitTimeout()
			}
			return config.DefaultMaxAwaitTimeout
		},
	})
	d.scenarioRunner = scenarioRunner
	d.cleanups.push(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutCancel()
		if err := scenarioRunner.Shutdown(shutCtx); err != nil {
			logger.Warn("scenario runner shutdown returned error", slog.Any("error", err))
		}
	})

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	lastSeenFlushInterval := reaper.ResolveMarkDisconnectedStale(cfg.Reaper) / 3

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var sigilStore keepergrpc.SigilStore
	if d.sigilSvc != nil {
		sigilStore = sigil.NewPGStore(d.pool)
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var moduleBinaries keepergrpc.ModuleBinarySource
	if d.sigilSvc != nil {
		moduleBinaries = d.sigilSvc
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var trustAnchors keepergrpc.TrustAnchorSource
	if d.sigilAnchorSource != nil {
		trustAnchors = d.sigilAnchorSource
	}

	// Oracle-handler (ADR-030 S2, beacons reactor): PortentEvent → match Decree →
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// destroy/upgrade-prepare (d.serviceRegistry). where-CEL — sandbox-evaluator
	// Keeper daemon runtime wiring note.
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
		ModuleBinaries:        moduleBinaries,
		ModuleFetchMaxBytes:   cfg.Plugins.ResolvedMaxArtifactSize(),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		VigilSource: keepergrpc.NewVigilSource(d.pool),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Connect-time broadcast эффективного telemetry-конфига host-vitals
		// (ADR-072, NIM-87): резолв per-SID (souls→incarnation→service-artifact-
		// манифест `telemetry:` + essence-override) поверх общего pool + реестра
		// сервисов (git-координаты по имени) + Service-загрузчика + essence-
		// резолвера. Нет инкарнации → broadcast скип (Soul на soul-local каденсе).
		TelemetrySource: keepergrpc.NewTelemetrySource(d.pool, d.serviceRegistry, d.serviceLoader, d.essenceResolver, logger),
		// Toll cluster-detector hook (ADR-038): на каждом выходе EventStream-
		// handler-а (Recv-error / ctx-cancel) вызывается NotifyDisconnect. При
		// выключенном Toll d.tollWatcher = nil → handler-side hook no-op (см.
		// notifyTollDisconnect в eventstream.go).
		TollNotifier: tollNotifierOrNil(d.tollWatcher),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		Oracle: &keepergrpc.OracleDeps{
			DB:          d.pool,
			Where:       oracleWhere,
			Enqueuer:    oracleEnqueuer,
			AuditWriter: d.auditWriter,
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			Metrics: d.oracleMetrics,
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
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
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		Augur: &keepergrpc.AugurDeps{
			DB:          d.pool,
			Vault:       d.vc,
			Egress:      keeperaugur.NewEgressClient(),
			AuditWriter: d.auditWriter,
			Outbound:    outbound,
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupWatchman(ctx context.Context) error {
	logger := d.logger

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pingers := []watchman.NamedPinger{
		{Name: "postgres", Pinger: poolPinger{d.pool}},
	}
	if d.redisClient != nil {
		pingers = append(pingers, watchman.NamedPinger{Name: "redis", Pinger: d.redisClient})
	}
	probe, err := watchman.NewDepsProbe(pingers...)
	if err != nil {
		// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	d.watchmanMetrics.SetIsolated(false)

	logger.Info("keeper run: watchman started (isolation-detect + soul-shedding)",
		slog.Duration("interval", watchmanInterval(d.cfg)),
		slog.Int("fail_threshold", watchmanFailThreshold(d.cfg)),
		slog.Bool("redis_in_probe", d.redisClient != nil))

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupToll(ctx context.Context) error {
	logger := d.logger

	// Keeper daemon runtime wiring note.
	enabled := true
	if d.cfg.Toll != nil && d.cfg.Toll.Enabled != nil {
		enabled = *d.cfg.Toll.Enabled
	}
	// Keeper daemon runtime wiring note.
	if !enabled {
		d.tollDegradedReader = toll.NoopDegradedReader{}
		logger.Info("keeper run: toll disabled (toll.enabled=false)")
		return nil
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if d.redisClient == nil {
		d.tollDegradedReader = toll.NoopDegradedReader{}
		logger.Info("keeper run: toll disabled (no Redis client)")
		return nil
	}

	// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// d.tollWatcher → ZADD).
	publisher := &keeperRedisTollPublisher{client: d.redisClient}
	watcher, err := toll.NewWatcher(toll.Config{KID: d.cfg.KID, WarmupDelay: warmup}, publisher, d.tollMetrics, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build toll watcher: %v\n", err)
		return errSetupFailed
	}
	d.tollWatcher = watcher
	d.tollDegradedReader = &keeperRedisTollDegradedReader{client: d.redisClient}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	baselineReader, err := toll.NewPGBaselineReader(d.pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build toll baseline: %v\n", err)
		return errSetupFailed
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	notifier := buildTollWebhookNotifier(cfgToll.Webhook, d.vc, logger)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// / `degraded_ttl` / `clear_grace` / `per_coven_thresholds` / `webhook.*`
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// `POST /v1/voyages`).
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupTempo(_ context.Context) error {
	logger := d.logger

	// Keeper daemon runtime wiring note.
	if !d.cfg.Tempo.TempoEnabled() {
		logger.Info("keeper run: tempo disabled (tempo.enabled=false)")
		return nil
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func tollNotifierOrNil(w *toll.Watcher) keepergrpc.TollNotifier {
	if w == nil {
		return nil
	}
	return w
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func tempoLimiterOrNil(tb *keeperredis.TokenBucket) apimiddleware.RateLimiter {
	if tb == nil {
		return nil
	}
	return tb
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupLoginGuard(_ context.Context) error {
	logger := d.logger
	if d.cfg.Auth == nil {
		return nil // Keeper daemon runtime wiring note.
	}
	if !d.cfg.Auth.LoginRateLimitEnabled() {
		logger.Info("keeper run: auth login rate-limit disabled (auth.rate_limit.enabled=false)")
		return nil
	}
	if d.redisClient == nil {
		logger.Info("keeper run: auth login rate-limit disabled (no Redis client) — login endpoints without throttle")
		return nil
	}
	guard, err := keeperredis.NewLoginGuard(d.redisClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build login guard: %v\n", err)
		return errSetupFailed
	}
	d.loginGuard = guard
	rate, burst, threshold, window, backoff := d.cfg.Auth.ResolvedLoginRateLimit()
	logger.Info("keeper run: auth login rate-limit active (/auth/*)",
		slog.Float64("rate", rate), slog.Int("burst", burst),
		slog.Int("lockout_threshold", threshold),
		slog.Duration("lockout_window", window),
		slog.Duration("lockout_backoff", backoff))
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func loginGuardOrNil(g *keeperredis.LoginGuard) apimiddleware.LoginGuard {
	if g == nil {
		return nil
	}
	return g
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) loginLimitCfg() apimiddleware.AuthLoginLimitConfig {
	rate, burst, threshold, window, backoff := d.cfg.Auth.ResolvedLoginRateLimit()
	return apimiddleware.AuthLoginLimitConfig{
		Rate:             rate,
		Burst:            burst,
		LockoutThreshold: threshold,
		LockoutWindow:    window,
		LockoutBackoff:   backoff,
	}
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type keeperRedisTollSortedSetReader struct {
	client *keeperredis.Client
}

func (r *keeperRedisTollSortedSetReader) CountInWindow(ctx context.Context, fromUnix, toUnix int64) (int64, error) {
	return keeperredis.TollCountInWindow(ctx, r.client, fromUnix, toUnix)
}

func (r *keeperRedisTollSortedSetReader) TrimBelow(ctx context.Context, beforeUnix int64) error {
	return keeperredis.TollTrimBelow(ctx, r.client, beforeUnix)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// PerCovenThresholds.
func (r *keeperRedisTollSortedSetReader) CountByCovenInWindow(ctx context.Context, fromUnix, toUnix int64) (map[string]int64, error) {
	return keeperredis.TollCountByCovenInWindow(ctx, r.client, fromUnix, toUnix)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func cloneTollWebhookCfg(in *config.KeeperTollWebhook) *config.KeeperTollWebhook {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
//   - threshold/window/degraded_ttl/clear_grace + per_coven_thresholds —
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var notifier toll.Notifier
	if tollWebhookCfgChanged(d.tollWebhookCfg, cfgToll.Webhook) {
		notifier = buildTollWebhookNotifier(cfgToll.Webhook, d.vc, logger)
		d.tollWebhookCfg = cloneTollWebhookCfg(cfgToll.Webhook)
		logger.Info("toll hot-reload: webhook notifier recycled",
			slog.Bool("enabled", cfgToll.Webhook != nil && cfgToll.Webhook.Enabled))
	} else {
		// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) tollCurrentNotifier() toll.Notifier {
	if d.tollLeader == nil {
		return nil
	}
	return d.tollLeader.CurrentNotifier()
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type keeperRedisTollDegradedWriter struct {
	client *keeperredis.Client
}

func (w *keeperRedisTollDegradedWriter) SetDegraded(ctx context.Context, holder string, ttl time.Duration) error {
	return keeperredis.TollSetDegraded(ctx, w.client, holder, ttl)
}

func (w *keeperRedisTollDegradedWriter) ClearDegraded(ctx context.Context) error {
	return keeperredis.TollClearDegraded(ctx, w.client)
}

// Keeper daemon runtime wiring note.
// keeperredis.TollIsDegraded.
type keeperRedisTollDegradedReader struct {
	client *keeperredis.Client
}

func (r *keeperRedisTollDegradedReader) IsDegraded(ctx context.Context) (bool, error) {
	return keeperredis.TollIsDegraded(ctx, r.client)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// (d.outbound + streamManager).
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupSigilInvalidation(ctx context.Context) error {
	if d.sigilSvc == nil {
		return nil
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	go runAnchorsReloadTicker(ctx, sigilAnchorsReloadInterval(d.cfg), d.reloadAnchors)
	d.logger.Info("keeper run: sigil anchors TTL-fallback reload enabled",
		slog.Duration("interval", sigilAnchorsReloadInterval(d.cfg)))

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if d.redisClient == nil {
		return nil
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	sigilStore := sigil.NewPGStore(d.pool)

	d.sigilSvc.SetInvalidator(sigilInvalidator{redis: d.redisClient, logger: d.logger})
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if d.sigilKeySvc != nil {
		d.sigilKeySvc.SetPublisher(sigilAnchorsPublisher{redis: d.redisClient, logger: d.logger})
	}

	go d.watchSigilInvalidations(ctx, sigilStore)
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	go d.watchAnchorsChanged(ctx)
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) watchSigilInvalidations(ctx context.Context, store keepergrpc.SigilStore) {
	sub, err := keeperredis.SubscribeSigilInvalidate(ctx, d.redisClient, d.logger)
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Warn("sigil: cluster invalidation subscription did not start, keeping connect-time broadcast",
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
				// Keeper daemon runtime wiring note.
				// Keeper daemon runtime wiring note.
				return
			}
			d.rebroadcastActiveSigils(ctx, store)
		}
	}
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) rebroadcastActiveSigils(ctx context.Context, store keepergrpc.SigilStore) {
	recs, err := store.ListActive(ctx)
	if err != nil {
		d.logger.Warn("sigil: re-broadcast list failed — skipping (connect-time broadcast protects)",
			slog.Any("error", err))
		return
	}
	d.outbound.RebroadcastSigils(ctx, keepergrpc.SigilRecordsToProto(recs))
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// rbac.Holder.Run TTL-poll + Summons poll-fallback ADR-027).
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) watchAnchorsChanged(ctx context.Context) {
	sub, err := keeperredis.SubscribeAnchorsChanged(ctx, d.redisClient, d.logger)
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Warn("sigil: anchors-changed subscription did not start, anchor set will not hot-reload",
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) reloadAnchors(ctx context.Context) {
	if d.sigilSvc == nil {
		return
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type trustAnchorHolder struct {
	pems atomic.Pointer[[]string]
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (h *trustAnchorHolder) set(pems []string) {
	cp := make([]string, len(pems))
	copy(cp, pems)
	h.pems.Store(&cp)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// setupCoreModules): `keeper.yml::plugins.cache_root` → env-override →
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupSigil(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	if cfg.Sigil == nil || cfg.Sigil.SigningKeyRef == "" {
		logger.Info("keeper run: sigil disabled (sigil.signing_key_ref is empty) — plugin.allow/revoke/list not registered")
		return nil
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	signer, err := d.buildSigilSigner(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build sigil signer: %v\n", err)
		return errSetupFailed
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.sigilAnchors = signer.AnchorSet()

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	pemSet, err := signer.AnchorSetPEM()
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: derive sigil anchor set PEM: %v\n", err)
		return errSetupFailed
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
//	active);
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	d.logger.Info("keeper run: sigil signing key registry empty — falling back to cfg.signing_key_ref (single-anchor)")
	signingKey, err := sigil.LoadSigningKey(ctx, d.vc, d.cfg.Sigil.SigningKeyRef)
	if err != nil {
		return nil, fmt.Errorf("load cfg signing key: %w", err)
	}
	return sigil.NewSigner(signingKey)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupAPIServer(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var redisPinger health.Pinger
	if d.redisClient != nil {
		redisPinger = d.redisClient
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	var soulPresence handlers.SoulPresence
	if d.redisClient != nil {
		soulPresence = topologyLeaseChecker{rc: d.redisClient}
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	ldapAuth, err := d.setupLDAPAuth(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: setup LDAP auth: %v\n", err)
		return errSetupFailed
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	oidcAuth, err := d.setupOIDCAuth(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: setup OIDC auth: %v\n", err)
		return errSetupFailed
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
		ServiceDirectives:   d.serviceDirectives,
		ServiceTelemetry:    d.serviceTelemetry,
		AugurSvc:            d.augurSvc,
		OracleSvc:           d.oracleSvc,
		OperatorDB:          d.pool,
		IncarnationDB:       d.pool,
		SoulDB:              d.pool,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ApplyBus: d.applyBus,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ChoirDB: d.pool,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		SoulPresence: soulPresence,
		// UtilizationReader — host-vitals из Redis для telemetry-эндпоинтов
		// (NIM-86). Value-адаптер (не typed-nil interface): при nil-Redis
		// (single-Keeper dev) внутренний nil-guard отдаёт stale/empty.
		UtilizationReader: utilizationReader{rc: d.redisClient},
		// SoulStatsStaleFn — hot-reload порог disconnect-а для stale_count в
		// GET /v1/souls/stats (тот же mark_disconnected.stale_after, что flush
		// last_seen_at и Reaper): читаем свежий cfg-снимок на каждом запросе.
		SoulStatsStaleFn: func() time.Duration {
			return reaper.ResolveMarkDisconnectedStale(d.store.Get().Reaper)
		},
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ClusterRegistry:     clusterRegistryOrNil(d),
		ClusterLeaderReader: clusterLeaderReaderOrNil(d),
		SelfKID:             cfg.KID,
		TTLDefault:          d.ttlDefault,
		MetricsHTTP:         d.httpMetrics,
		ScenarioRunner:      d.scenarioRunner,
		ScenarioDestroyer:   d.scenarioRunner,
		ScenarioDrift:       d.scenarioRunner,
		ServiceRegistry:     d.serviceRegistry,
		ServiceLoader:       d.serviceLoader,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		VaultClient:     d.vc,
		PushRun:         d.pushRun,
		PushProviderSvc: d.pushProviderSvc,
		HeraldSvc:       d.heraldSvc,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ProviderSvc:      d.providerSvc,
		ProfileSvc:       d.profileSvc,
		ErrandDispatcher: d.errandDispatcher,
		ErrandStore:      d.errandStore,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// VoyageHandler (enforcer=d.rbacHolder, IncarnationDB=d.pool).
		VoyageDB:               d.pool,
		VoyageScenarioResolver: handlers.NewVoyageScenarioPGResolver(d.pool),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		VoyageCommandResolver: handlers.NewVoyageCommandPGResolverWithPresence(d.pool, soulPresence),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		VoyageMaxScope: cfg.Voyage.ResolvedMaxScope(),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		VoyageMaxBatchSize: cfg.Voyage.ResolvedMaxBatchSize(),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		CadenceDB: d.pool,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		CadencePollFloorSeconds: int(cfg.CadenceScheduler.ResolvedPollFloor().Seconds()),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		AuditReader: auditpg.NewReader(d.pool),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// passthrough).
		TollDegraded: d.tollDegradedReader,
		// Tempo per-AID rate-limiter `POST /v1/voyages` (ADR-050). limiter nil
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		TempoLimiter: tempoLimiterOrNil(d.tempoLimiter),
		TempoMetrics: d.tempoMetrics,
		TempoVoyageCreateLimits: func() apimiddleware.RateLimitLimits {
			rate, burst := d.store.Get().Tempo.ResolvedVoyageCreate()
			return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
		},
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		TempoVoyagePreviewLimits: func() apimiddleware.RateLimitLimits {
			rate, burst := d.store.Get().Tempo.ResolvedVoyagePreview()
			return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
		},
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// setupSigilInvalidation.
		ModuleCatalogPlugins: moduleCatalogPluginsOrNil(d),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ModuleFormPrepH: handlers.NewModuleFormPrepHandler(handlers.NewFormPrepPGResolver(d.pool), d.logger),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		WebUIEnabled: cfg.WebUIMounted(),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		LDAPAuth: ldapAuth,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		OIDCAuth: oidcAuth,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		LoginGuard:    loginGuardOrNil(d.loginGuard),
		LoginLimitCfg: d.loginLimitCfg(),
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ProvisioningPolicyReader: d.serviceHolder,
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build HTTP server: %v\n", err)
		return errSetupFailed
	}
	d.apiServer = srv
	return nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Vault (bind_password_ref → field `password`; tls.ca_ref → field `ca`),
// Keeper daemon runtime wiring note.
func (d *daemon) setupLDAPAuth(ctx context.Context) (*api.LDAPAuthDeps, error) {
	if d.cfg.Auth == nil || d.cfg.Auth.LDAP == nil {
		return nil, nil
	}
	l := d.cfg.Auth.LDAP

	bindPassword, err := readVaultField(ctx, d.vc, l.BindPasswordRef, "password")
	if err != nil {
		return nil, fmt.Errorf("auth.ldap.bind_password_ref: %w", err)
	}
	var caPEM []byte
	if l.TLS.CARef != "" {
		ca, err := readVaultField(ctx, d.vc, l.TLS.CARef, "ca")
		if err != nil {
			return nil, fmt.Errorf("auth.ldap.tls.ca_ref: %w", err)
		}
		caPEM = []byte(ca)
	}

	timeout := 0
	if l.Timeout != "" {
		dur, perr := config.ParseDuration(l.Timeout)
		if perr != nil {
			return nil, fmt.Errorf("auth.ldap.timeout: %w", perr)
		}
		timeout = int(dur / time.Second)
	}

	authn, err := keeperldap.New(keeperldap.Config{
		URL:                l.URL,
		StartTLS:           l.StartTLS,
		TLSCA:              caPEM,
		InsecureSkipVerify: l.TLS.InsecureSkipVerify,
		BindMode:           keeperldap.BindMode(l.BindMode),
		BindDN:             l.BindDN,
		BindPassword:       bindPassword,
		BaseDN:             l.BaseDN,
		UserFilter:         l.UserFilter,
		GroupFilter:        l.GroupFilter,
		GroupAttr:          l.GroupAttr,
		AIDAttr:            l.AIDAttr,
		TimeoutSeconds:     timeout,
	}, d.logger)
	if err != nil {
		return nil, err
	}

	mapper := keeperauth.NewMapper(keeperauth.MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: l.GroupRoleMap,
		DB:           d.pool,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		Tx:    d.pool,
		Audit: d.auditWriter,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ProvisioningGate: d.serviceHolder,
		Logger:           d.logger,
	})

	return &api.LDAPAuthDeps{
		Authenticator: authn,
		Mapper:        mapper,
		Issuer:        d.issuer,
		TTL:           d.ttlDefault,
		Audit:         d.auditWriter,
		Logger:        d.logger,
	}, nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
const oidcFlowTTL = 5 * time.Minute

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type oidcFlowStoreAdapter struct {
	store *keeperredis.OIDCFlowStore
}

func (a oidcFlowStoreAdapter) Save(ctx context.Context, state string, fs keeperoidc.FlowState) error {
	return a.store.Save(ctx, state, keeperredis.OIDCFlowState{Nonce: fs.Nonce, CodeVerifier: fs.CodeVerifier})
}

func (a oidcFlowStoreAdapter) Consume(ctx context.Context, state string) (keeperoidc.FlowState, error) {
	rs, err := a.store.Consume(ctx, state)
	if err != nil {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		if errors.Is(err, keeperredis.ErrOIDCFlowNotFound) {
			return keeperoidc.FlowState{}, keeperoidc.ErrFlowNotFound
		}
		return keeperoidc.FlowState{}, err
	}
	return keeperoidc.FlowState{Nonce: rs.Nonce, CodeVerifier: rs.CodeVerifier}, nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupOIDCAuth(ctx context.Context) (*api.OIDCAuthDeps, error) {
	if d.cfg.Auth == nil || d.cfg.Auth.OIDC == nil {
		return nil, nil
	}
	if d.redisClient == nil {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		d.logger.Warn("auth.oidc configured, but Redis is unavailable - OIDC login not mounted (flow-state store requires Redis)")
		return nil, nil
	}
	o := d.cfg.Auth.OIDC

	var clientSecret string
	if o.ClientSecretRef != "" {
		s, err := readVaultField(ctx, d.vc, o.ClientSecretRef, "client_secret")
		if err != nil {
			return nil, fmt.Errorf("auth.oidc.client_secret_ref: %w", err)
		}
		clientSecret = s
	}
	var caPEM []byte
	if o.TLS.CARef != "" {
		ca, err := readVaultField(ctx, d.vc, o.TLS.CARef, "ca")
		if err != nil {
			return nil, fmt.Errorf("auth.oidc.tls.ca_ref: %w", err)
		}
		caPEM = []byte(ca)
	}

	flowStore, err := keeperredis.NewOIDCFlowStore(d.redisClient, oidcFlowTTL)
	if err != nil {
		return nil, fmt.Errorf("auth.oidc flow store: %w", err)
	}

	authn, err := keeperoidc.New(ctx, keeperoidc.Config{
		Issuer:       o.Issuer,
		ClientID:     o.ClientID,
		ClientSecret: clientSecret,
		RedirectURL:  o.RedirectURL,
		Scopes:       o.Scopes,
		TLSCA:        caPEM,
		AIDClaim:     o.AIDClaim,
		GroupsClaim:  o.GroupsClaim,
	}, oidcFlowStoreAdapter{store: flowStore}, d.logger)
	if err != nil {
		return nil, err
	}

	mapper := keeperauth.NewMapper(keeperauth.MapperConfig{
		Method:       operator.AuthMethodOIDC,
		GroupRoleMap: o.GroupRoleMap,
		DB:           d.pool,
		// Keeper daemon runtime wiring note.
		Tx:    d.pool,
		Audit: d.auditWriter,
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		ProvisioningGate: d.serviceHolder,
		Logger:           d.logger,
	})

	return &api.OIDCAuthDeps{
		Authenticator: authn,
		Mapper:        mapper,
		Issuer:        d.issuer,
		TTL:           d.ttlDefault,
		Audit:         d.auditWriter,
		Logger:        d.logger,
	}, nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func readVaultField(ctx context.Context, vc *keepervault.Client, ref, field string) (string, error) {
	if vc == nil {
		return "", fmt.Errorf("vault client is nil")
	}
	if ref == "" {
		return "", fmt.Errorf("ref is empty")
	}
	path, err := keepervault.ParseRef(ref)
	if err != nil {
		return "", err
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return "", fmt.Errorf("read vault %q: %w", path, err)
	}
	if v, ok := kv[field].(string); ok && v != "" {
		return v, nil
	}
	// Keeper daemon runtime wiring note.
	var only string
	count := 0
	for _, raw := range kv {
		if s, ok := raw.(string); ok && s != "" {
			only = s
			count++
		}
	}
	if count == 1 {
		return only, nil
	}
	return "", fmt.Errorf("field %q not found (or ambiguous) in vault %q", field, path)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupMCPServer(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if cfg.Listen.MCP.Addr != "" {
		mcpHandler, err := mcp.NewHandler(mcp.HandlerDeps{
			OperatorSvc: d.apiServer.OperatorService(),
			RBAC:        d.rbacHolder,
			RBACRoles:   d.rbacSvc,
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			SigilSvc: d.sigilSvc,
			// Keeper daemon runtime wiring note.
			// api.Deps.SigilKeySvc (single source of truth, R3-S7). nil → sigil.key.*-
			// Keeper daemon runtime wiring note.
			SigilKeySvc: d.sigilKeySvc,
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			ServiceSvc: d.serviceSvc,
			// Keeper daemon runtime wiring note.
			// api.Deps.AugurSvc (single source of truth, ADR-025). nil →
			// Keeper daemon runtime wiring note.
			AugurSvc: d.augurSvc,
			// Keeper daemon runtime wiring note.
			// api.Deps.OracleSvc (single source of truth, ADR-030). nil →
			// Keeper daemon runtime wiring note.
			OracleSvc:   d.oracleSvc,
			AuditWriter: d.auditWriter,
			Logger:      logger,
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			IncarnationDB:     d.pool,
			ScenarioRunner:    d.scenarioRunner,
			ScenarioDestroyer: d.scenarioRunner,
			ScenarioDrift:     d.scenarioRunner,
			ServiceRegistry:   d.serviceRegistry,
			ServiceLoader:     d.serviceLoader,
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			SoulDB: d.pool,
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			PurviewResolver: d.rbacHolder,
			// Keeper daemon runtime wiring note.
			// api.Deps.PushRun (single source of truth, Variant C orchestrator).
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			PushRun: d.pushRun,

			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// ADR-032 amendment 2026-05-26, S7-2).
			PushProviderSvc: d.pushProviderSvc,

			// Keeper daemon runtime wiring note.
			// api.Deps.HeraldSvc (single source of truth, ADR-052 S4).
			HeraldSvc: d.heraldSvc,

			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			ProviderSvc: d.providerSvc,
			ProfileSvc:  d.profileSvc,

			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// E2 wire-up). nil → keeper.soul.errand.run / keeper.errand.*
			// Keeper daemon runtime wiring note.
			ErrandDispatcher: d.errandDispatcher,
			ErrandStore:      d.errandStore,

			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			VoyageDB:               d.pool,
			VoyageScenarioResolver: handlers.NewVoyageScenarioPGResolver(d.pool),
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupAcolyte(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	if cfg.Acolytes <= 0 {
		logger.Info("acolyte: pool disabled (keeper.acolytes is 0) — run-goroutine path remains")
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		logger.Warn("acolyte: run-goroutine mode (acolytes=0) is single-keeper-only — for an HA cluster (N>1 keepers) set keeper.acolytes>0 (ADR-027)")
		return nil
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	claimRunner := scenario.NewClaimRunner(scenario.ClaimDeps{
		Deps: scenario.Deps{
			Loader:   d.serviceLoader,
			Topology: d.topologyResolver,
			Essence:  d.essenceResolver,
			Render:   d.renderPipeline,
			Outbound: d.outbound,
			Destiny:  d.destinySource,
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	acolyteCtx, acolyteCancel := context.WithCancel(ctx)
	d.cleanups.push(acolyteCancel)
	d.cleanups.push(func() {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type reaperExecutor struct {
	*reaper.Purger
	*reaper.VaultReconciler
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type sigilKeyIDsReader struct{ pool *pgxpool.Pool }

func (r sigilKeyIDsReader) ListAllKeyIDs(ctx context.Context) (map[string]struct{}, error) {
	return sigil.ListAllKeyIDs(ctx, r.pool)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type soulLeaseChecker struct{ rc *keeperredis.Client }

func (c soulLeaseChecker) SoulStreamAlive(ctx context.Context, sid string) (bool, error) {
	return keeperredis.SoulStreamAlive(ctx, c.rc, sid)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type topologyLeaseChecker struct{ rc *keeperredis.Client }

func (c topologyLeaseChecker) SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error) {
	return keeperredis.SoulsStreamAlive(ctx, c.rc, sids)
}

// utilizationReader адаптирует Redis-клиент под host-vitals-ридер telemetry-
// эндпоинтов (NIM-86, [handlers.UtilizationReader]). Зеркало topologyLeaseChecker;
// nil-Redis (dev/unit без Redis) → stale/empty, не паникует.
type utilizationReader struct{ rc *keeperredis.Client }

func (u utilizationReader) ReadUtilization(ctx context.Context, sid string) (keeperredis.UtilizationSnapshot, bool, error) {
	if u.rc == nil {
		return keeperredis.UtilizationSnapshot{}, false, nil
	}
	return keeperredis.ReadUtilization(ctx, u.rc, sid)
}

func (u utilizationReader) ReadUtilizationWindow(ctx context.Context, sid string, limit int) ([]keeperredis.UtilizationPoint, error) {
	if u.rc == nil {
		return nil, nil
	}
	return keeperredis.ReadUtilizationWindow(ctx, u.rc, sid, limit)
}

// clusterRegistryAdapter адаптирует Redis-клиент под read-поверхность Conclave
// для `GET /v1/cluster` ([handlers.ClusterRegistry]). Узкая склейка ради изоляции
// api-пакета от keeperredis (handler зависит от интерфейса, не от клиента).
type clusterRegistryAdapter struct{ rc *keeperredis.Client }

func (a clusterRegistryAdapter) LiveKIDs(ctx context.Context) ([]string, error) {
	return keeperredis.LiveKIDs(ctx, a.rc)
}

func (a clusterRegistryAdapter) InstanceMeta(ctx context.Context, kid string) (string, bool, error) {
	return keeperredis.ReadInstanceMeta(ctx, a.rc, kid)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type clusterLeaderReaderAdapter struct{ rc *keeperredis.Client }

func (a clusterLeaderReaderAdapter) ReaperLeaderHolder(ctx context.Context) (string, bool, error) {
	return keeperredis.PeekLeaseHolder(ctx, a.rc, reaper.LeaderLeaseKey)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func clusterRegistryOrNil(d *daemon) handlers.ClusterRegistry {
	if d.redisClient == nil {
		return nil
	}
	return clusterRegistryAdapter{rc: d.redisClient}
}

func clusterLeaderReaderOrNil(d *daemon) handlers.ClusterLeaderReader {
	if d.redisClient == nil {
		return nil
	}
	return clusterLeaderReaderAdapter{rc: d.redisClient}
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func mcpSoulPresence(d *daemon) handlers.SoulPresence {
	if d.redisClient == nil {
		return nil
	}
	return topologyLeaseChecker{rc: d.redisClient}
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// topologyLeaseChecker (keeperredis.SoulsStreamAlive).
type lazySoulPresence struct{ d *daemon }

func (p lazySoulPresence) SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error) {
	if p.d.redisClient == nil {
		return nil, errors.New("presence unavailable: Redis not configured (await_online barrier requires SID-lease)")
	}
	return keeperredis.SoulsStreamAlive(ctx, p.d.redisClient, sids)
}

// lazyCertPolicy — резолвер cert-rotation-политики для `core.cert.issued`,
// читающий d.certPolicyResolver ЛЕНИВО: setupCoreModules собирает coremod.Deps ДО
// setupScenarioDeps, где резолвер конструируется. nil-резолвер → явная ошибка
// (шаг issued failed), а не паника.
type lazyCertPolicy struct{ d *daemon }

func (l lazyCertPolicy) Resolve(ctx context.Context, name string) (certpolicy.Policy, error) {
	if l.d.certPolicyResolver == nil {
		return certpolicy.Policy{}, fmt.Errorf("cert policy resolver not ready")
	}
	return l.d.certPolicyResolver.Resolve(ctx, name)
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// leaseOwnerChecker.
type passageCapChecker struct{ rc *keeperredis.Client }

func (c passageCapChecker) SoulsLackingPassage(ctx context.Context, sids []string) ([]string, error) {
	return keeperredis.SoulsLackingCapability(ctx, c.rc, sids, config.CapabilityPassage)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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
		logger.Warn("voyageorch: renew_interval >= lease_ttl - lease may expire between renew ticks",
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

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//   - ScenarioSpawner: incarnation.SelectByName → ServiceRegistry.Resolve →
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// incarnation.ReleaseApplyingOrphan (apply_id-match + CAS).
	orphanReleaser := &voyageOrphanLockReleaser{pool: d.pool}
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
		logger.Warn("voyageorch: errandDispatcher is nil - kind=command Voyages will fail-closed (CommandSpawner not configured)")
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
			OrphanReleaser:  orphanReleaser,
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

// Keeper daemon runtime wiring note.

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type voyageServiceResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type voyageScenarioSpawner struct {
	runner   *scenario.Runner
	reader   incarnation.ExecQueryRower
	resolver voyageServiceResolver
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (s *voyageScenarioSpawner) SpawnScenarioRun(ctx context.Context, voyageID, incarnationName, scenarioName string, input []byte, startedByAID string, cadenceID *string) (string, error) {
	inc, err := incarnation.SelectByName(ctx, s.reader, incarnationName)
	if err != nil {
		return "", fmt.Errorf("voyage scenario spawner: select incarnation %q: %w", incarnationName, err)
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		CadenceID: cadenceID,
	}
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if voyageID != "" {
		spec.VoyageID = &voyageID
	}
	if err := s.runner.Start(ctx, spec); err != nil {
		return "", fmt.Errorf("voyage scenario spawner: runner.Start: %w", err)
	}
	return applyID, nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
//   - FENCING-3 (self-ownership): voyage.VerifyOwnership(voyage_id, kid, attempt)
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//   - FENCING-1 + single-winner: incarnation.ReleaseApplyingOrphan
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type voyageOrphanLockReleaser struct {
	pool *pgxpool.Pool
}

func (r *voyageOrphanLockReleaser) ReleaseOrphanLock(ctx context.Context, voyageID, incarnationName string, attempt int, kid, orphanApplyID string) (bool, error) {
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if err := voyage.VerifyOwnership(ctx, r.pool, voyageID, kid, attempt); err != nil {
		if errors.Is(err, voyage.ErrLeaseLost) {
			return false, nil
		}
		return false, fmt.Errorf("voyage orphan-releaser: verify ownership: %w", err)
	}

	// Keeper daemon runtime wiring note.
	historyID := audit.NewULID()
	if err := incarnation.ReleaseApplyingOrphan(ctx, r.pool, incarnationName, orphanApplyID, historyID); err != nil {
		if errors.Is(err, incarnation.ErrOrphanLockNotReleased) {
			return false, nil
		}
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			// Keeper daemon runtime wiring note.
			return false, nil
		}
		return false, fmt.Errorf("voyage orphan-releaser: release applying-orphan: %w", err)
	}
	return true, nil
}

// voyagePgIncarnationAwaiter — production [voyageorch.IncarnationAwaiter]: poll
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type voyagePgIncarnationAwaiter struct {
	db           applyrun.ExecQueryRower
	pollInterval time.Duration
	logger       *slog.Logger
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (a *voyagePgIncarnationAwaiter) pollOutcome(ctx context.Context, applyID string) (voyageorch.TargetOutcome, bool) {
	statuses, err := applyrun.SelectStatusesByApplyID(ctx, a.db, applyID)
	if err != nil {
		a.logger.Warn("voyageorch: await poll failed",
			slog.String("apply_id", applyID), slog.Any("error", err))
		return "", false
	}
	if len(statuses) == 0 {
		return "", false // Keeper daemon runtime wiring note.
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
			return "", false // Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type voyageCommandSpawner struct {
	bridge *errandRunSpawnerBridge
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// standalone (parity single-SID /exec, ADR-033).
func (s *voyageCommandSpawner) SpawnCommand(ctx context.Context, voyageID, sid, module, startedByAID string, input []byte) (string, string, error) {
	_ = voyageID // Keeper daemon runtime wiring note.
	errandID, status, _, err := s.bridge.SpawnErrand(ctx, "", sid, module, startedByAID, input)
	return errandID, status, err
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
type errandTerminalSource interface {
	Get(ctx context.Context, errandID string) (*errand.Row, error)
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
type errandRunSpawnerBridge struct {
	dispatcher     *errand.Dispatcher
	terminalSource errandTerminalSource
	pollInterval   time.Duration
	clock          func() time.Time
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// status-string, errorCode, err).
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//   - StatusSuccess        → status=success
//   - StatusFailed / StatusModuleNotAllowed / StatusTimedOut / StatusCancelled →
//
// Keeper daemon runtime wiring note.
func (b *errandRunSpawnerBridge) SpawnErrand(ctx context.Context, runID, sid, module, startedByAID string, input []byte) (string, string, string, error) {
	inputMap := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &inputMap); err != nil {
			return "", "failed", "input_decode_error", fmt.Errorf("errandrun spawner: decode input: %w", err)
		}
	}

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	_ = runID // Keeper daemon runtime wiring note.
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
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		status, errorCode, awErr := b.awaitTerminal(ctx, res.ErrandID)
		return res.ErrandID, status, errorCode, awErr
	}
	status, errorCode := classifyErrandStatus(res.Status)
	if status == "" {
		return res.ErrandID, "failed", "unknown_status", fmt.Errorf("errandrun spawner: unknown status %q", res.Status)
	}
	return res.ErrandID, status, errorCode, nil
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (b *errandRunSpawnerBridge) awaitTerminal(ctx context.Context, errandID string) (string, string, error) {
	if b.terminalSource == nil {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		return "failed", "async_escalation", nil
	}
	poll := b.pollInterval
	if poll <= 0 {
		poll = time.Second
	}
	deadline := b.now().Add(time.Duration(errand.DefaultTimeoutSeconds)*time.Second + 5*time.Second)

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if st, ec, done := b.pollTerminal(ctx, errandID); done {
		return st, ec, nil
	}
	for {
		select {
		case <-ctx.Done():
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
			// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (b *errandRunSpawnerBridge) CancelErrand(ctx context.Context, errandID string) error {
	if errandID == "" {
		return nil
	}
	if err := b.dispatcher.Cancel(ctx, errand.CancelRequest{ErrandID: errandID}); err != nil {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		if errors.Is(err, errand.ErrErrandTerminal) {
			return nil
		}
		return err
	}
	return nil
}

// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupReaper(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	//
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if cfg.Reaper != nil && cfg.Reaper.Enabled && d.redisClient != nil {
		rc := d.redisClient

		reaperCtx, reaperCancel := context.WithCancel(ctx)
		reaperDone := make(chan struct{})

		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		d.cleanups.push(func() {
			select {
			case <-reaperDone:
			case <-time.After(5 * time.Second):
				logger.Warn("reaper did not stop within 5s after shutdown — leak suspected")
			}
		})

		// Keeper daemon runtime wiring note.
		d.cleanups.push(reaperCancel)

		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		reaperMetrics := reaper.RegisterReaperMetrics(d.metricsReg)

		// Keeper daemon runtime wiring note.
		// VaultReconciler (cross-store report-only reap_orphan_vault_keys).
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		var vaultDep reaper.VaultKVLister
		if d.vc != nil {
			vaultDep = d.vc
		}
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		executor := &reaperExecutor{
			Purger:          reaper.NewPurgerWithLease(d.pool, soulLeaseChecker{rc: rc}, logger),
			VaultReconciler: reaper.NewVaultReconciler(vaultDep, sigilKeyIDsReader{pool: d.pool}, logger, nil),
		}

		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		var scryDeps *reaper.ScryDeps
		if d.scenarioRunner != nil && d.serviceRegistry != nil {
			scryDeps = &reaper.ScryDeps{
				Pool:         d.pool,
				DriftChecker: d.scenarioRunner,
				Services:     d.serviceRegistry,
				Audit:        d.auditWriter,
			}
		}

		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		var oldErrandsPurger *reaper.ErrandsPurger
		if d.pool != nil {
			oldErrandsPurger = reaper.NewErrandsPurger(d.pool, logger)
		}

		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		if d.pool != nil {
			d.voyageReclaimer = reaper.NewVoyageReclaimer(d.pool, d.auditWriter, logger)
		}

		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		var certRotator *reaper.CertRotator
		if d.pool != nil && d.vc != nil {
			certRotator = reaper.NewCertRotator(d.pool, reaper.CertRotatorDeps{
				Signer: certPKISignerAdapter{vc: d.vc},
				Vault:  d.vc,
				CSRGen: certServiceCSRGen,
				Cfg:    func() reaper.CertRotatorConfig { return resolveCertRotatorConfig(d.store.Get(), logger) },
				Policy: d.certPolicyResolver,
				Audit:  d.auditWriter,
				Logger: logger,
				KID:    cfg.KID,
			})
		}

		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		var orphanEphemeralTidingsPurger *reaper.EphemeralTidingsPurger
		if d.pool != nil {
			orphanEphemeralTidingsPurger = reaper.NewEphemeralTidingsPurger(d.pool, logger)
		}

		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		var orphanApplyingReconciler *reaper.OrphanApplyingReconciler
		if d.pool != nil {
			orphanApplyingReconciler = reaper.NewOrphanApplyingReconciler(d.pool, rc, d.auditWriter, logger)
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
			OrphanApplying:         orphanApplyingReconciler,
			CertRotator:            certRotator,
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

// certPKISignerAdapter адаптирует *keepervault.Client к certissue.Signer:
// vault.SignCSR возвращает *vault.SignedCertificate, certissue ждёт *certissue.SignedCert
// (certissue не импортирует vault-пакет ради типа результата — см. certissue/issue.go).
// Общий signer для reaper.CertRotator и core.cert.issued.
type certPKISignerAdapter struct{ vc *keepervault.Client }

func (a certPKISignerAdapter) SignCSR(ctx context.Context, mount, role, csrPEM string) (*certissue.SignedCert, error) {
	signed, err := a.vc.SignCSR(ctx, mount, role, csrPEM)
	if err != nil {
		return nil, err
	}
	return &certissue.SignedCert{
		CertificatePEM: signed.CertificatePEM,
		CAChainPEM:     signed.CAChainPEM,
		SerialNumber:   signed.SerialNumber,
		NotAfter:       signed.NotAfter,
	}, nil
}

// certServiceCSRGen генерит keypair+CSR (keeper-side, R2) через
// vault.GenerateServiceCSR. Пакет-функция (не inline-замыкание): один источник для
// reaper.CertRotator и core.cert.issued (coremod.Deps.CertCSRGen).
func certServiceCSRGen(cn string, dns []string) ([]byte, []byte, error) {
	g, gerr := keepervault.GenerateServiceCSR(keepervault.CSRParams{CommonName: cn, DNSNames: dns})
	if gerr != nil {
		return nil, nil, gerr
	}
	return g.PrivateKeyPEM, g.CSRPEM, nil
}

// resolveCertRotatorConfig собирает политику правила rotate_due_certs из свежего
// keeper.yml snapshot (вызывается на каждом тике — hot-reload). rotate_threshold/
// rotate_jitter парсятся через config.ParseDuration (convention `<N>d`, как у всех
// reaper-правил), НЕ через stdlib time.ParseDuration — иначе `30d` не распарсится,
// Threshold схлопнется в 0 и правило МОЛЧА не будет ротировать при enabled:true
// (тихий security-сбой). Невалидный формат не глотается: warn-лог + поле остаётся
// нулевым (правило инертно), симметрично runDurationRule.
func resolveCertRotatorConfig(cfg *config.KeeperConfig, logger *slog.Logger) reaper.CertRotatorConfig {
	out := reaper.CertRotatorConfig{}
	if cfg == nil {
		return out
	}
	out.DefaultPKIMount = cfg.Vault.PKIMount
	if cfg.Reaper == nil || cfg.Reaper.Rules == nil {
		return out
	}
	rule, ok := cfg.Reaper.Rules["rotate_due_certs"]
	if !ok {
		return out
	}
	if rule.RotateThreshold != "" {
		if d, err := config.ParseDuration(rule.RotateThreshold); err == nil {
			out.Threshold = d
		} else if logger != nil {
			logger.Warn("reaper.rotate_due_certs: invalid rotate_threshold, rule will not rotate",
				slog.String("raw", rule.RotateThreshold), slog.Any("error", err))
		}
	}
	if rule.RotateJitter != "" {
		if d, err := config.ParseDuration(rule.RotateJitter); err == nil {
			out.JitterWindow = d
		} else if logger != nil {
			logger.Warn("reaper.rotate_due_certs: invalid rotate_jitter, jitter disabled",
				slog.String("raw", rule.RotateJitter), slog.Any("error", err))
		}
	}
	if rule.MaxRotationsPerTick != nil {
		out.MaxRotationsPerTick = *rule.MaxRotationsPerTick
	}
	return out
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// (cardinality-safe, parity setupReaper). Cleanup — conductorCancel +
// conductorDone-wait (parity reaper).
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func (d *daemon) setupConductor(ctx context.Context) error {
	cfg := d.cfg
	logger := d.logger

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	if d.redisClient == nil || !cfg.CadenceScheduler.CadenceSchedulerEnabled() {
		logger.Info("keeper run: conductor disabled (no Redis or cadence_scheduler.enabled: false)")
		return nil
	}
	if d.pool == nil {
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		logger.Warn("keeper run: conductor skipped - no PG pool")
		return nil
	}
	rc := d.redisClient

	conductorCtx, conductorCancel := context.WithCancel(ctx)
	conductorDone := make(chan struct{})

	// Keeper daemon runtime wiring note.
	d.cleanups.push(func() {
		select {
		case <-conductorDone:
		case <-time.After(5 * time.Second):
			logger.Warn("conductor did not stop within 5s after shutdown — leak suspected")
		}
	})
	// Keeper daemon runtime wiring note.
	d.cleanups.push(conductorCancel)

	// Keeper daemon runtime wiring note.
	conductorMetrics := conductor.RegisterConductorMetrics(d.metricsReg)

	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
	// Keeper daemon runtime wiring note.
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
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		//
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
		// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
func conductorSchedulerCfg(cfg *config.KeeperConfig) *config.KeeperCadenceScheduler {
	if cfg == nil {
		return nil
	}
	return cfg.CadenceScheduler
}

// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
//
// Keeper daemon runtime wiring note.
// Keeper daemon runtime wiring note.
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

// Keeper daemon runtime wiring note.
type cadencePoolFetcher struct{ pool *pgxpool.Pool }

func (f cadencePoolFetcher) SelectMinPeriod(ctx context.Context) (cadence.MinPeriod, error) {
	return cadence.SelectMinPeriod(ctx, f.pool)
}
