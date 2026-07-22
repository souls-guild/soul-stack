// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
//
//	keeper init    --archon=<aid> [--config=<path>] [--credential-out=<path>] [--display-name=<name>]
//	keeper run     [--config=<path>] [--initialize]
//	keeper version
//	keeper help
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstrap"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	keeperpg "github.com/souls-guild/soul-stack/keeper/internal/pg"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	shlog "github.com/souls-guild/soul-stack/shared/log"
	"github.com/souls-guild/soul-stack/shared/obs"
)

const defaultConfigPath = "/etc/keeper/keeper.yml"

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
var version = "0.0.0-dev"

// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
//	2 — usage-error (bad flags, unknown command).
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "init":
		os.Exit(runInit(args))
	case "run":
		os.Exit(runDaemon(args))
	case "version", "--version", "-v":
		printVersion(os.Stdout)
		os.Exit(exitOK)
	case "help", "--help", "-h":
		printUsage(os.Stdout)
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "keeper: unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, `keeper — Soul Stack Keeper.

Usage:
  keeper <command> [flags]

Commands:
  init     Bootstrap the first Archon (ADR-013). Requires empty operators registry.
  run      Run the Keeper daemon (M0.5c: stub — apply migrations, wait for signal).
  version  Print keeper version and Go runtime.
  help     Show this message.

Run "keeper <command> --help" for command-specific flags.`)
}

// Keeper command runtime helper note.
//
//	keeper <version> (go<goversion>)
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func printVersion(w *os.File) {
	fmt.Fprintf(w, "keeper %s (%s)\n", version, runtime.Version())
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
func runInit(args []string) int {
	var (
		archonAID   string
		configPath  string
		credOut     string
		displayName string
	)
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&archonAID, "archon", "", "first Archon AID (required, e.g. archon-alice)")
	fs.StringVar(&configPath, "config", defaultConfigPath, "keeper.yml path")
	fs.StringVar(&credOut, "credential-out", "", "path to write JWT token (default <user-cache>/keeper/bootstrap-<aid>.token or /var/lib/keeper/bootstrap-<aid>.token)")
	fs.StringVar(&displayName, "display-name", "", "display name (default = ArchonAID)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: keeper init --archon=<aid> [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// Keeper command runtime helper note.
		return exitUsage
	}
	if archonAID == "" {
		fmt.Fprintln(os.Stderr, "keeper init: --archon is required")
		fs.Usage()
		return exitUsage
	}
	if !operator.ValidAID(archonAID) {
		fmt.Fprintf(os.Stderr, "keeper init: invalid AID %q (must match %s)\n", archonAID, operator.AIDPattern)
		return exitUsage
	}

	ctx, cancel := signalContext()
	defer cancel()

	cfg, _, diags, err := config.LoadKeeper(configPath, config.ValidateOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper init: load config %q: %v\n", configPath, err)
		return exitError
	}
	if diag.HasErrors(diags) {
		fmt.Fprintf(os.Stderr, "keeper init: config %q has errors:\n", configPath)
		for _, d := range diags {
			if d.Level == diag.LevelError {
				fmt.Fprintf(os.Stderr, "  - %s [%s]: %s\n", d.Phase, d.Code, d.Message)
			}
		}
		return exitError
	}
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	logger := shlog.New(shlog.FromKeeper(cfg.Logging))
	if cfg.Auth == nil || cfg.Auth.JWT == nil {
		fmt.Fprintln(os.Stderr, "keeper init: auth.jwt block is required in keeper.yml")
		return exitError
	}
	ttlBootstrap, err := parseTTL(cfg.Auth.JWT.TTLBootstrap, "auth.jwt.ttl_bootstrap", 720*time.Hour)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper init: %v\n", err)
		return exitError
	}
	jwtIssuerName := cfg.Auth.JWT.Issuer
	if jwtIssuerName == "" {
		jwtIssuerName = cfg.KID
	}
	if jwtIssuerName == "" {
		fmt.Fprintln(os.Stderr, "keeper init: cannot derive JWT issuer (both auth.jwt.issuer and kid are empty)")
		return exitError
	}

	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	vc, err := keepervault.NewClient(ctx, cfg.Vault)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper init: vault client: %v\n", err)
		return exitError
	}

	pool, err := keeperpg.NewPool(ctx, cfg.Postgres, vc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper init: pg pool: %v\n", err)
		return exitError
	}
	defer pool.Close()
	if err := keeperpg.Ping(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "keeper init: pg ping: %v\n", err)
		return exitError
	}

	// Keeper command runtime helper note.
	dsn, err := keeperpg.ResolveDSN(ctx, vc, cfg.Postgres.DSNRef)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper init: resolve DSN: %v\n", err)
		return exitError
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		fmt.Fprintf(os.Stderr, "keeper init: apply migrations: %v\n", err)
		return exitError
	}

	auditWriter := auditpg.NewWriter(pool)

	bootCfg := bootstrap.Config{
		ArchonAID:        archonAID,
		DisplayName:      displayName,
		TTLBootstrap:     ttlBootstrap,
		Pool:             pool,
		VaultClient:      vc,
		SigningKeyRef:    cfg.Auth.JWT.SigningKeyRef,
		IssuerFactory:    issuerFactory(jwtIssuerName),
		AuditWriter:      auditWriter,
		CredentialOutput: credOut,
	}
	res, err := bootstrap.Init(ctx, bootCfg)
	if err != nil {
		switch {
		case errors.Is(err, bootstrap.ErrAlreadyInitialized):
			fmt.Fprintln(os.Stderr, "keeper init: keeper already initialized (operators registry not empty)")
			return exitError

		case errors.Is(err, bootstrap.ErrAuditWriteFailed):
			// Keeper command runtime helper note.
			// Keeper command runtime helper note.
			logger.Warn("bootstrap: audit write failed AFTER operator insert committed — manual reconciliation required",
				slog.String("aid", archonAID),
				slog.Any("error", err),
			)
			fmt.Fprintln(os.Stderr,
				"keeper init: audit write failed after operator was committed.\n"+
					"        Operator is in the registry, but audit_log has no record.\n"+
					"        Manually insert an audit-event for operator.created or contact ops.")
			return exitError

		case errors.Is(err, bootstrap.ErrTokenFileWriteFailed):
			// Keeper command runtime helper note.
			// Keeper command runtime helper note.
			// Keeper command runtime helper note.
			// Keeper command runtime helper note.
			// Keeper command runtime helper note.
			fmt.Fprintf(os.Stderr,
				"keeper init: token file write failed (%v).\n"+
					"        Operator is committed and audit recorded; only the JWT file is missing.\n"+
					"        WARNING: token printed below — treat as COMPROMISED and rotate ASAP.\n"+
					"        Intended path was: %s\n"+
					"---BEGIN TOKEN---\n%s\n---END TOKEN---\n",
				err, res.CredentialPath, res.Token)
			return exitError

		default:
			fmt.Fprintf(os.Stderr, "keeper init: %v\n", err)
			return exitError
		}
	}

	logger.Info("bootstrap complete",
		slog.String("aid", archonAID),
		slog.String("credential_path", res.CredentialPath),
		slog.String("correlation_id", res.CorrelationID),
		slog.String("audit_id", res.AuditID),
	)
	fmt.Fprintf(os.Stdout, "Bootstrap complete. Token written to %s\n", res.CredentialPath)
	return exitOK
}

// runDaemon — `keeper run` (M0.6b).
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
func runDaemon(args []string) int {
	ctx, cancel := signalContext()
	defer cancel()

	d := &daemon{cleanups: &cleanupStack{}}
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	defer d.cleanups.runLIFO()

	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	if code, ok := d.setupConfig(args); !ok {
		return code
	}

	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	steps := []func(context.Context) error{
		d.setupObservabilityEarly,
		d.setupVault,
		d.setupStorage,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupSigil,
		d.setupOperatorBootstrapGuard,
		d.setupJWT,
		d.setupRBAC,
		d.setupServiceRegistry,
		d.setupAudit,
		d.setupCoreModules,
		d.setupMetricsRegistry,
		d.setupMetricsListener,
		d.setupOTel,
		d.setupScenarioDeps,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupPushOrchestrator,
		// setupPushDispatchers — pilot wire-up SshDispatcher (S6, 2026-05-26):
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupPushDispatchers,
		d.setupGRPCBootstrap,
		d.setupRedis,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// setupAudit (dispatcher/auditWriter), setupMetricsRegistry
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupHeraldDelivery,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupPushProviderSvc,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupHeraldSvc,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupCloudCRUD,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.runLegacyAutoImport,
		d.setupConclave,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupConclaveRefuseGuard,
		d.setupRBACInvalidation,
		d.setupServiceRegistryInvalidation,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupToll,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupTempo,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupLoginGuard,
		d.setupGRPCEventStream,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.finalizePushOrchestrator,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupErrandDispatcher,
		d.setupWatchman,
		d.setupSigilInvalidation,
		d.setupAPIServer,
		// setupOperatorInvalidation — JWT immediate revoke (ADR-014 Amendment
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupOperatorInvalidation,
		d.setupMCPServer,
		d.setupAcolyte,
		// Keeper command runtime helper note.
		// d.pool + scenarioRunner/serviceRegistry/errandDispatcher (production
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupVoyageWorker,
		d.setupReaper,
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		d.setupConductor,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return exitError
		}
	}

	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	if err := d.apiServer.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: HTTP server: %v\n", err)
		return exitError
	}

	d.logger.Info("keeper run: shutdown complete")
	return exitOK
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
type poolPinger struct{ pool pgxPool }

func (p poolPinger) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Keeper command runtime helper note.
// Keeper command runtime helper note.
type pgxPool interface {
	Ping(ctx context.Context) error
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func parseTTL(raw, fieldName string, def time.Duration) (time.Duration, error) {
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", fieldName, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", fieldName, raw)
	}
	return d, nil
}

// minExchangeTTL is a floor on auth.jwt.exchange_ttl: a lower value is raised
// to it (protection from a misconfig that would make the cookie->Bearer exchange pointless).
const minExchangeTTL = 1 * time.Minute

// clampExchangeTTL raises d to minExchangeTTL if it is below the floor.
func clampExchangeTTL(d time.Duration) time.Duration {
	if d < minExchangeTTL {
		return minExchangeTTL
	}
	return d
}

// envTruthy reads an env variable as a boolean flag via [strconv.ParseBool]
// (accepts 1/t/T/true/TRUE etc). An empty or invalid string -> false:
// an env-override must not "accidentally" enable a mode due to a typo/garbage.
func envTruthy(name string) bool {
	v := os.Getenv(name)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false
	}
	return b
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func guardOperatorsRegistry(n int64, initialize bool) (proceed bool, refuseMsg string, pending bool) {
	if n == 0 && !initialize {
		return false,
			"keeper run: operators registry is empty — refusing to start.\n" +
				"        Run `keeper init --archon=<aid>` to bootstrap the first Archon,\n" +
				"        or pass `--initialize` to start in bootstrap-pending mode.",
			false
	}
	if n == 0 {
		return true, "", true
	}
	return true, "", false
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
type conclaveSinglePathDecision int

const (
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	conclaveSinglePathOK conclaveSinglePathDecision = iota
	// Keeper command runtime helper note.
	conclaveSinglePathRefuse
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	conclaveSinglePathWarn
)

// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func decideConclaveSinglePath(acolytes, liveCount int, allowUnsafe bool) conclaveSinglePathDecision {
	if acolytes > 0 || liveCount <= 1 {
		return conclaveSinglePathOK
	}
	if allowUnsafe {
		return conclaveSinglePathWarn
	}
	return conclaveSinglePathRefuse
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func conclaveRefuseMessage(liveCount int) string {
	return fmt.Sprintf("keeper run: multi-keeper detected (%d live Keeper instances in Conclave) with keeper.acolytes=0 - refusing to start.\n"+
		"        Run-goroutine path (acolytes: 0) is single-keeper-only: apply on one Keeper with a Soul\n"+
		"        on another Keeper's stream can hang in applying forever (ADR-027). Set keeper.acolytes>0\n"+
		"        for HA cluster, or keeper.allow_unsafe_single_path_multi_keeper: true\n"+
		"        (env KEEPER_ALLOW_UNSAFE_MULTI_KEEPER=true) as an explicit single-keeper-behind-LB opt-out.", liveCount)
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
const metricsPasswordField = "password"

// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func resolveMetricsBasicAuth(ctx context.Context, vc *keepervault.Client, m *config.KeeperMetrics) (*obs.BasicAuth, error) {
	if m == nil || m.Auth == nil || m.Auth.Basic == nil || !m.Auth.Basic.Enabled {
		return nil, nil
	}
	b := m.Auth.Basic
	if vc == nil {
		return nil, fmt.Errorf("metrics basic-auth enabled but vault client is nil")
	}
	path, err := keepervault.ParseRef(b.PasswordRef)
	if err != nil {
		// Keeper command runtime helper note.
		// Keeper command runtime helper note.
		return nil, fmt.Errorf("metrics.auth.basic.password_ref: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read metrics basic-auth password from vault: %w", err)
	}
	raw, ok := kv[metricsPasswordField]
	if !ok {
		return nil, fmt.Errorf("vault secret for metrics basic-auth has no %q field", metricsPasswordField)
	}
	pass, ok := raw.(string)
	if !ok || pass == "" {
		return nil, fmt.Errorf("vault field %q for metrics basic-auth is empty or not a string", metricsPasswordField)
	}
	return &obs.BasicAuth{Username: b.Username, Password: pass}, nil
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
func otelEndpoint(o *config.KeeperOTel) string {
	if o == nil {
		return ""
	}
	return o.Endpoint
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
func issuerFactory(issuerName string) func(signingKey []byte) (bootstrap.JWTIssuer, error) {
	return func(signingKey []byte) (bootstrap.JWTIssuer, error) {
		return keeperjwt.NewIssuer(signingKey, issuerName)
	}
}

// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func pluginCacheRoot(p *config.KeeperPlugins) string {
	if p != nil && p.CacheRoot != "" {
		return p.CacheRoot
	}
	if v := os.Getenv("KEEPER_PLUGIN_CACHE_DIR"); v != "" {
		return v
	}
	return pluginhost.DefaultCacheRoot
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
const defaultPluginWorkRoot = "/var/lib/soul-stack-keeper/plugin-src"

// Keeper command runtime helper note.
// Keeper command runtime helper note.
//  2. env `KEEPER_PLUGIN_WORK_DIR` — dev/CI-override;
//  3. [defaultPluginWorkRoot].
func pluginWorkRoot(p *config.KeeperPlugins) string {
	if p != nil && p.WorkRoot != "" {
		return p.WorkRoot
	}
	if v := os.Getenv("KEEPER_PLUGIN_WORK_DIR"); v != "" {
		return v
	}
	return defaultPluginWorkRoot
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
const defaultServiceCacheRoot = "/var/lib/soul-stack-keeper/services"

// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func serviceCacheRoot(_ *config.KeeperConfig) string {
	if v := os.Getenv("KEEPER_SERVICE_CACHE_DIR"); v != "" {
		return v
	}
	return defaultServiceCacheRoot
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
const defaultDestinyCacheRoot = "/var/lib/soul-stack-keeper/destiny"

// Keeper command runtime helper note.
// [serviceCacheRoot]: env `KEEPER_DESTINY_CACHE_DIR` → [defaultDestinyCacheRoot].
func destinyCacheRoot(_ *config.KeeperConfig) string {
	if v := os.Getenv("KEEPER_DESTINY_CACHE_DIR"); v != "" {
		return v
	}
	return defaultDestinyCacheRoot
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func acolyteLease(cfg *config.KeeperConfig) time.Duration {
	if cfg.AcolyteLease == "" {
		return config.DefaultAcolyteLease
	}
	d, err := config.ParseDuration(cfg.AcolyteLease)
	if err != nil || d <= 0 {
		return config.DefaultAcolyteLease
	}
	return d
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func acolyteBatch(cfg *config.KeeperConfig) int {
	if cfg.AcolyteBatch <= 0 {
		return config.DefaultAcolyteBatch
	}
	return cfg.AcolyteBatch
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
func acolytePollInterval(cfg *config.KeeperConfig) time.Duration {
	if cfg.AcolytePollInterval == "" {
		return config.DefaultAcolytePollInterval
	}
	d, err := config.ParseDuration(cfg.AcolytePollInterval)
	if err != nil || d <= 0 {
		return config.DefaultAcolytePollInterval
	}
	return d
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func acolyteDrainGrace(cfg *config.KeeperConfig) time.Duration {
	if cfg.AcolyteDrainGrace == "" {
		return config.DefaultAcolyteDrainGrace
	}
	d, err := config.ParseDuration(cfg.AcolyteDrainGrace)
	if err != nil || d <= 0 {
		return config.DefaultAcolyteDrainGrace
	}
	return d
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// `voyage.workers: N > 0`.
func voyageWorkers(cfg *config.KeeperConfig) int {
	if cfg.Voyage == nil || cfg.Voyage.Workers <= 0 {
		return 0
	}
	return cfg.Voyage.Workers
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// (60s, parity ErrandRun).
func voyageLeaseTTL(cfg *config.KeeperConfig) time.Duration {
	if cfg.Voyage == nil || cfg.Voyage.LeaseTTL == "" {
		return config.DefaultVoyageLeaseTTL
	}
	d, err := config.ParseDuration(cfg.Voyage.LeaseTTL)
	if err != nil || d <= 0 {
		return config.DefaultVoyageLeaseTTL
	}
	return d
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// [config.DefaultVoyageLeaseRenewInterval] (20s = ~1/3 LeaseTTL).
func voyageLeaseRenewInterval(cfg *config.KeeperConfig) time.Duration {
	if cfg.Voyage == nil || cfg.Voyage.LeaseRenewInterval == "" {
		return config.DefaultVoyageLeaseRenewInterval
	}
	d, err := config.ParseDuration(cfg.Voyage.LeaseRenewInterval)
	if err != nil || d <= 0 {
		return config.DefaultVoyageLeaseRenewInterval
	}
	return d
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// [config.DefaultVoyagePollInterval] (5s).
func voyagePollInterval(cfg *config.KeeperConfig) time.Duration {
	if cfg.Voyage == nil || cfg.Voyage.PollInterval == "" {
		return config.DefaultVoyagePollInterval
	}
	d, err := config.ParseDuration(cfg.Voyage.PollInterval)
	if err != nil || d <= 0 {
		return config.DefaultVoyagePollInterval
	}
	return d
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func sigilAnchorsReloadInterval(cfg *config.KeeperConfig) time.Duration {
	if cfg.SigilAnchorsReloadInterval == "" {
		return config.DefaultSigilAnchorsReloadInterval
	}
	d, err := config.ParseDuration(cfg.SigilAnchorsReloadInterval)
	if err != nil || d <= 0 {
		return config.DefaultSigilAnchorsReloadInterval
	}
	return d
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func oracleCircuitMaxFires(cfg *config.KeeperConfig) int {
	if cfg.OracleCircuitMaxFires == nil {
		return config.DefaultOracleCircuitMaxFires
	}
	return *cfg.OracleCircuitMaxFires
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func oracleCircuitWindow(cfg *config.KeeperConfig) time.Duration {
	if cfg.OracleCircuitWindow == "" {
		return config.DefaultOracleCircuitWindow
	}
	d, err := config.ParseDuration(cfg.OracleCircuitWindow)
	if err != nil || d <= 0 {
		return config.DefaultOracleCircuitWindow
	}
	return d
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func watchmanInterval(cfg *config.KeeperConfig) time.Duration {
	if cfg.WatchmanInterval == "" {
		return config.DefaultWatchmanInterval
	}
	d, err := config.ParseDuration(cfg.WatchmanInterval)
	if err != nil || d <= 0 {
		return config.DefaultWatchmanInterval
	}
	return d
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func watchmanFailThreshold(cfg *config.KeeperConfig) int {
	if cfg.WatchmanFailThreshold <= 0 {
		return config.DefaultWatchmanFailThreshold
	}
	return cfg.WatchmanFailThreshold
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
type summonsPublisher struct {
	redis *keeperredis.Client
	kid   string
}

func (p summonsPublisher) PublishSummons(ctx context.Context) error {
	_, err := keeperredis.PublishSummons(ctx, p.redis, p.kid)
	return err
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
const rbacInvalidatePublishTimeout = time.Second

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
type rbacInvalidator struct {
	redis  *keeperredis.Client
	kid    string
	logger *slog.Logger
}

func (i rbacInvalidator) Invalidate(_ context.Context) {
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	ctx, cancel := context.WithTimeout(context.Background(), rbacInvalidatePublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishRBACInvalidate(ctx, i.redis, i.kid); err != nil {
		i.logger.Warn("rbac: cluster-invalidate publish failed", slog.Any("error", err))
	}
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
type rbacInvalidationSource struct {
	redis  *keeperredis.Client
	kid    string
	logger *slog.Logger
}

func (s rbacInvalidationSource) Watch(ctx context.Context, onInvalidate func()) error {
	sub, err := keeperredis.SubscribeRBACInvalidate(ctx, s.redis, s.kid, s.logger)
	if err != nil {
		return err
	}
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-sub.Channel():
			if !ok {
				// Keeper command runtime helper note.
				// Keeper command runtime helper note.
				return nil
			}
			onInvalidate()
		}
	}
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
const serviceInvalidatePublishTimeout = time.Second

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
type serviceInvalidator struct {
	redis  *keeperredis.Client
	kid    string
	logger *slog.Logger
}

func (i serviceInvalidator) Invalidate(_ context.Context) {
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	ctx, cancel := context.WithTimeout(context.Background(), serviceInvalidatePublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishServiceInvalidate(ctx, i.redis, i.kid); err != nil {
		i.logger.Warn("serviceregistry: cluster-invalidate publish failed", slog.Any("error", err))
	}
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
type serviceInvalidationSource struct {
	redis  *keeperredis.Client
	kid    string
	logger *slog.Logger
}

func (s serviceInvalidationSource) Watch(ctx context.Context, onInvalidate func()) error {
	sub, err := keeperredis.SubscribeServiceInvalidate(ctx, s.redis, s.kid, s.logger)
	if err != nil {
		return err
	}
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-sub.Channel():
			if !ok {
				// Keeper command runtime helper note.
				// Keeper command runtime helper note.
				return nil
			}
			onInvalidate()
		}
	}
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
const sigilInvalidatePublishTimeout = time.Second

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
type sigilInvalidator struct {
	redis  *keeperredis.Client
	logger *slog.Logger
}

func (i sigilInvalidator) Invalidate(_ context.Context) {
	// Keeper command runtime helper note.
	// Keeper command runtime helper note.
	ctx, cancel := context.WithTimeout(context.Background(), sigilInvalidatePublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishSigilInvalidate(ctx, i.redis); err != nil {
		i.logger.Warn("sigil: cluster-invalidate publish failed", slog.Any("error", err))
	}
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
type sigilAnchorsPublisher struct {
	redis  *keeperredis.Client
	logger *slog.Logger
}

func (p sigilAnchorsPublisher) Publish(_ context.Context) {
	// Keeper command runtime helper note.
	ctx, cancel := context.WithTimeout(context.Background(), sigilInvalidatePublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishAnchorsChanged(ctx, p.redis); err != nil {
		p.logger.Warn("sigil: anchors-changed publish failed", slog.Any("error", err))
	}
}

// Keeper command runtime helper note.
// Keeper command runtime helper note.
// Keeper command runtime helper note.
//
// Keeper command runtime helper note.
// Keeper command runtime helper note.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
