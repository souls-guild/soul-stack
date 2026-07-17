// Package main is the entrypoint for the `soul` binary under ADR-004 / ADR-011 / ADR-012.
//
// Subcommand router on stdlib `flag`:
//
//	soul init  --token=<bootstrap-token> [--config=<path>] [--sid=<sid>]
//	soul run                            [--config=<path>]
//	soul apply                          [--config=<path>]
//	soul help
//
// `init` — one-shot bootstrap cycle (ADR-012(b)): generate key+CSR →
// unary Bootstrap RPC to Keeper → write SoulSeed to disk. Bootstrap token
// comes from --token OR env SOUL_BOOTSTRAP_TOKEN (flag wins over env).
// The env form is preferred: the flag shows up in `ps` and shell history, env doesn't.
//
// `run` — long-lived daemon loop: load SoulSeed → discover custom plugins
// → build Registry (core + custom) → dial EventStream to Keeper → recv-loop
// + dispatch ApplyRequest → ApplyRunner → send TaskEvent / RunResult.
// Reconnect on disconnect — internal loop with exponential backoff.
//
// `apply` — push-oneshot mode (ADR-004): reads a rendered ApplyRequest
// (protojson) from stdin → builds Registry (core + custom) → ApplyRunner →
// writes a stream of TaskEvent + final RunResult to stdout as NDJSON (protojson).
// exit 0 on RunResult.status==success, 1 otherwise. No SoulSeed/mTLS required —
// trust comes from the authenticated Archon + SSH channel from Keeper.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	shlog "github.com/souls-guild/soul-stack/shared/log"
	"github.com/souls-guild/soul-stack/shared/obs"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/augur"
	"github.com/souls-guild/soul-stack/soul/internal/beacon"
	soulbootstrap "github.com/souls-guild/soul-stack/soul/internal/bootstrap"
	"github.com/souls-guild/soul-stack/soul/internal/coremod"
	installmod "github.com/souls-guild/soul-stack/soul/internal/coremod/module"
	coremodutil "github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	soulgrpc "github.com/souls-guild/soul-stack/soul/internal/grpc"
	"github.com/souls-guild/soul-stack/soul/internal/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/runtime"
	"github.com/souls-guild/soul-stack/soul/internal/runtime/errandrunner"
	"github.com/souls-guild/soul-stack/soul/internal/seed"
	"github.com/souls-guild/soul-stack/soul/internal/sigilcache"
	"github.com/souls-guild/soul-stack/soul/internal/soulprint"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// envBootstrapToken is the env var carrying the bootstrap token for `soul init`,
// a safer alternative to --token (the flag is visible in `ps`/shell history).
const envBootstrapToken = "SOUL_BOOTSTRAP_TOKEN"

const (
	defaultConfigPath = "/etc/soul/soul.yml"
	// defaultSoulMetricsListen is the default loopback address for `/metrics`
	// (docs/soul/config.md → metrics.listen). Loopback keeps the metrics port
	// from facing outward — the main protection for Soul metrics in this slice (auth deferred).
	defaultSoulMetricsListen = "127.0.0.1:9091"
)

// leaseHeldBackoffCap is a modest reconnect-backoff cap for the lease-held branch
// (Dial rejected with AlreadyExists: the SID lease still holds a live holder, see
// soulgrpc.IsLeaseHeld). Deliberately small and NOT configurable: after a keeper
// crash presence expires in ~30s, after which force-release frees the lease — Soul
// must reconnect within a few seconds, not wait out the general
// transport cap (keeper.retry.backoff.max, tens of seconds). Not promoted to a
// config key: it's an internal recovery-latency invariant, not an operator tunable.
const leaseHeldBackoffCap = 3 * time.Second

// soulVersion is reported in Hello.soul_version and BootstrapRequest.soul_version
// for auditing. The default is for `go run`/IDE builds; release builds
// overwrite it via the linker with `-ldflags "-X ...soulVersion=<ver>"`
// (see Makefile, VERSION variable). It's a `var`, not a `const`, because
// `-X` can only inject into package-level string variables.
var soulVersion = "0.0.0-dev"

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
	case "apply":
		os.Exit(runApply(args))
	case "help", "--help", "-h":
		printUsage(os.Stdout)
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "soul: unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `soul — Soul Stack agent.

Usage:
  soul <command> [flags]

Commands:
  init     Bootstrap SoulSeed (ADR-012(b)). Generates RSA key + CSR, calls
           Keeper Bootstrap RPC, writes cert.pem/key.pem/ca.pem to paths.seed.
  run      Run the Soul daemon: connect to Keeper EventStream, dispatch
           ApplyRequest to core/custom modules.
  apply    Push-oneshot (ADR-004): read a rendered ApplyRequest (protojson)
           from stdin, apply it, stream TaskEvent + RunResult as NDJSON to
           stdout. Exit 0 on success, 1 otherwise. No SoulSeed required.
  help     Show this message.

Run "soul <command> --help" for command-specific flags.`)
}

// resolveInitToken picks the bootstrap token by precedence: explicit --token
// wins over env SOUL_BOOTSTRAP_TOKEN (flag overrides). Empty --token → env.
// Both empty → error (at least one source is required). The env form is safer:
// --token is visible in `ps`/shell history, env isn't.
func resolveInitToken(flagToken string) (string, error) {
	if flagToken != "" {
		return flagToken, nil
	}
	if envToken := os.Getenv(envBootstrapToken); envToken != "" {
		return envToken, nil
	}
	return "", fmt.Errorf("soul init: provide token via --token or %s", envBootstrapToken)
}

// runInit parses flags, brings up dependencies (config), calls
// bootstrap.Run, and prints the result.
func runInit(args []string) int {
	var (
		configPath string
		token      string
		sid        string
	)
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&configPath, "config", defaultConfigPath, "soul.yml path")
	fs.StringVar(&token, "token", "", "bootstrap token issued by Keeper (or env "+envBootstrapToken+"; env is safer — flag is visible in ps/history)")
	fs.StringVar(&sid, "sid", "", "SID override (precedence: --sid > config.sid > os.Hostname lowercased)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: soul init --token=<bootstrap-token> [flags]")
		fmt.Fprintln(os.Stderr, "  Token may also be supplied via env "+envBootstrapToken+" (safer: not exposed in ps/shell history).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	token, err := resolveInitToken(token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		fs.Usage()
		return exitUsage
	}

	ctx, cancel := signalContext()
	defer cancel()

	cfg, err := loadSoulConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul init: %v\n", err)
		return exitError
	}
	logger := shlog.New(shlog.FromSoul(cfg.Logging))

	// The bootstrap phase hits bootstrap_port; hosts are tried in priority
	// order, no in-group shuffle — bootstrap is one-shot, order is
	// deterministic (spray only applies to the EventStream client). Failback
	// doesn't apply to bootstrap (one-shot). See docs/soul/connection.md.
	endpoints := make([]string, 0, len(cfg.Keeper.Endpoints))
	for _, ep := range orderedByPriority(cfg.Keeper.Endpoints) {
		endpoints = append(endpoints, ep.BootstrapAddr())
	}

	timeout, err := parseHandshakeTimeout(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul init: %v\n", err)
		return exitError
	}

	// --sid flag > config.sid > os.Hostname (resolved below in bootstrap.Run).
	if sid == "" {
		sid = cfg.SID
	}

	res, err := soulbootstrap.Run(ctx, soulbootstrap.Config{
		SID:              sid,
		Token:            token,
		SeedDir:          cfg.Paths.Seed,
		KeeperCA:         cfg.Keeper.TLS.CA,
		Endpoints:        endpoints,
		HandshakeTimeout: timeout,
		SoulVersion:      soulVersion,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul init: %v\n", err)
		return exitError
	}

	logger.Info("soul init: bootstrap complete",
		slog.String("sid", res.SID),
		slog.String("endpoint", res.Endpoint),
		slog.String("kid", res.KID),
		slog.Time("not_after", res.NotAfter),
		slog.String("seed_dir", res.SeedDir),
	)
	fmt.Fprintf(os.Stdout, "Bootstrap complete. SoulSeed written to %s\n", res.SeedDir)
	return exitOK
}

// runDaemon — `soul run`.
//
// Lifecycle:
//  1. Load config + load SoulSeed (cert/key/ca).
//  2. Discover custom plugins under paths.modules (warnings logged, not fatal).
//  3. Build Registry: coremod.Default() — single source for MVP.
//     (custom modules go through pluginhost — wired up for discovery,
//     dispatch to them is Plugin.d / M2.3).
//  4. Reconnect loop: Dial → recv-loop → disconnect → backoff → retry.
//     SIGINT/SIGTERM interrupt the loop.
func runDaemon(args []string) int {
	var configPath string
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&configPath, "config", defaultConfigPath, "soul.yml path")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: soul run [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	ctx, cancel := signalContext()
	defer cancel()

	// Store instead of the flat loadSoulConfig (ADR-021 hot-reload): gives a snapshot
	// for startup wire-up and is re-read on SIGHUP. reconnect-loop and the
	// soulprint ticker read store.Get() at point of use, so the
	// next iteration sees updated keeper.retry/failback + soulprint.refresh_interval.
	store, diags, err := config.LoadSoulStore(configPath, config.ValidateOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: load config %q: %v\n", configPath, err)
		return exitError
	}
	if diag.HasErrors(diags) {
		fmt.Fprintf(os.Stderr, "soul run: config %q has errors:\n", configPath)
		for _, d := range diags {
			if d.Level == diag.LevelError {
				fmt.Fprintf(os.Stderr, "  - %s [%s]: %s\n", d.Phase, d.Code, d.Message)
			}
		}
		return exitError
	}
	cfg := store.Get()
	if cfg == nil {
		fmt.Fprintln(os.Stderr, "soul run: config snapshot is nil after successful load (unexpected)")
		return exitError
	}
	logger, logLevel := shlog.NewWithLevel(shlog.FromSoul(cfg.Logging))

	// Hot-reload `logging.level` (ADR-021): shift the log threshold to the
	// new snapshot on every successful Store swap. file/format/rotation are
	// restart-required (docs/soul/config.md), left untouched.
	store.OnReload(func(_, newCfg *config.SoulConfig) {
		if newCfg != nil {
			logLevel.Set(newCfg.Logging.Level)
		}
	})

	// SIGHUP reload (ADR-021(b)): separate signal channel inside WatchSIGHUP,
	// SIGHUP doesn't get mixed up with SIGINT/SIGTERM from signalContext. Only
	// runs when hot_reload.enable_signal (default true). Push mode (soul apply)
	// is unaffected by hot-reload — it's one-shot. No audit on the Soul side
	// (no audit_log DB), reload is only logged.
	if cfg.HotReload.SignalEnabled() {
		reloadCh := config.WatchSIGHUP(ctx, store)
		go config.LogReloads(reloadCh, logger)
		logger.Info("soul run: SIGHUP config reload enabled")
	} else {
		logger.Info("soul run: SIGHUP config reload disabled (hot_reload.enable_signal=false)")
	}

	// SoulSeed — load before everything else: if bootstrap hasn't run,
	// there's no point continuing. Clear message: "run soul init".
	seedPaths := seed.PathsIn(cfg.Paths.Seed)
	seedMaterial, err := seed.Load(cfg.Paths.Seed)
	if err != nil {
		if errors.Is(err, seed.ErrIncomplete) {
			fmt.Fprintln(os.Stderr,
				"soul run: SoulSeed not found — run `soul init --token=<token>` first.\n"+
					"        expected files in: "+cfg.Paths.Seed)
			return exitError
		}
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}

	// Sigil-verify trust-anchor set (ADR-026(h), R3 multi-anchor): parse
	// sigil_pubkey.pem from the seed — it may carry several concatenated PEM
	// blocks (multi-anchor for seamless signing-key rotation). Empty (Sigil
	// disabled on Keeper) → empty set: any custom-plugin verify
	// fail-closed on no_trust_anchor. Broken PEM → refuse to start (no silent
	// fail-open). Core modules are static — don't go through verify, unaffected.
	sigilAnchors, err := seed.ParseSigilPubKeys(seedMaterial.SigilPubKeyPEM)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}

	// The Sigil cache (ADR-026, S6a) lives at Soul's runtime level — created once
	// here, outside the reconnect loop, so plugin grants survive an EventStream
	// disconnect and re-establishment. Keeper broadcasts PluginSigil on
	// connect; the recv-loop in handleSession stores them here. Verify
	// against the cache is S6b (via SigilLookupAdapter in pluginhost.Host).
	sigils := sigilcache.New()

	// Custom-modules discovery + lazy-spawn wire-up (ADR-020(d): one-shot per
	// Apply). Warnings are non-fatal: core MVP stays functional without
	// custom plugins. The Host itself is created even with an empty plugin list —
	// it's cheap and simplifies the code path when paths.modules is unset. Trust-anchor
	// + cache adapter are threaded into Host for fail-closed Sigil-verify of plugins.
	registry, anchorSet, beaconLookup, err := buildRegistry(cfg, logger, "soul run", sigilAnchors, pluginhost.NewSigilLookupAdapter(sigils))
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}

	// The EventStream phase hits event_stream_port; priority + spray-shuffle
	// still apply (docs/soul/connection.md).
	endpoints := make([]soulgrpc.Endpoint, 0, len(cfg.Keeper.Endpoints))
	for _, ep := range cfg.Keeper.Endpoints {
		endpoints = append(endpoints, soulgrpc.Endpoint{Addr: ep.EventStreamAddr(), Priority: ep.Priority})
	}
	if len(endpoints) == 0 {
		fmt.Fprintln(os.Stderr, "soul run: keeper.endpoints is empty in soul.yml")
		return exitError
	}

	handshakeTimeout, err := parseHandshakeTimeout(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}

	// Per-endpoint retry (keeper.retry.max_attempts) + a flat inter-attempt pause
	// = reuse backoff.initial/jitter (no new config keys). backoff here is
	// only needed for the static ClientConfig build; reconnectLoop reads its own
	// snapshot from the store per iteration (hot-reload).
	clientBackoff, err := loadBackoff(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}
	// SID resolution: config.sid > os.Hostname (lowercased). Lowercasing mirrors
	// bootstrap.Run (bootstrap.go) — otherwise a host with a MixedCase hostname would get
	// different SIDs on init vs run. `soul run` has no --sid flag (unlike init).
	sid := cfg.SID
	if sid == "" {
		host, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "soul run: detect hostname: %v\n", err)
			return exitError
		}
		sid = strings.ToLower(strings.TrimSpace(host))
	}

	// Observability stack (ADR-024). The Registry is shared between the `/metrics`
	// exposition handler and subsystem instrumentation (apply cycle / EventStream
	// client / soulprint collector). Register soul_*-collectors right away — the
	// handles are threaded into the subsystems below. docs/observability.md §4.0: the
	// collector lives next to its subsystem, registration happens on the soul-registry.
	reg := obs.NewRegistry()
	applyMetrics := runtime.RegisterApplyMetrics(reg)
	eventStreamMetrics := soulgrpc.RegisterEventStreamMetrics(reg)
	soulprintMetrics := soulprint.RegisterSoulprintMetrics(reg)
	beaconMetrics := beacon.RegisterBeaconMetrics(reg)
	errandMetrics := errandrunner.Register(reg)

	// `/metrics` — listener on cfg.Metrics.Listen (loopback 127.0.0.1:9091
	// by default). Optional HTTP Basic-auth via metrics.basic_auth: the password
	// is read from a file on disk (Soul has no vault client, ADR-012). Without
	// basic-auth, the endpoint's only protection is the loopback bind. Optional: with
	// metrics.enabled=false the listener isn't started.
	if cfg.Metrics != nil && cfg.Metrics.Enabled {
		// Default loopback address (docs/soul/config.md → metrics.listen)
		// for when the operator enables metrics but doesn't set listen — the
		// config parser doesn't inject defaults, apply it here.
		metricsListen := cfg.Metrics.Listen
		if metricsListen == "" {
			metricsListen = defaultSoulMetricsListen
		}
		metricsAuth, err := resolveSoulMetricsBasicAuth(cfg.Metrics.BasicAuth)
		if err != nil {
			fmt.Fprintf(os.Stderr, "soul run: resolve metrics basic-auth: %v\n", err)
			return exitError
		}
		metricsSrv, err := obs.ServeMetrics(metricsListen, reg, metricsAuth)
		if err != nil {
			fmt.Fprintf(os.Stderr, "soul run: start metrics listener: %v\n", err)
			return exitError
		}
		logger.Info("soul run: metrics listener up",
			slog.String("addr", metricsSrv.Addr()),
			slog.Bool("basic_auth", metricsAuth != nil))
		defer func() {
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutCancel()
			if err := metricsSrv.Shutdown(shutCtx); err != nil {
				logger.Warn("metrics listener shutdown returned error", slog.Any("error", err))
			}
		}()
	} else {
		logger.Info("soul run: metrics listener disabled (metrics.enabled=false or unset)")
	}

	// OTel provider (ADR-024): service.name="soul" + custom soulstack.sid.
	// Trace export when otel.enabled+endpoint; no-op otherwise. Set up once per
	// process — otel.* is restart-required (hot-reload doesn't touch it).
	otelProvider, err := obs.SetupOTel(ctx, obs.OTelConfig{
		Enabled:       cfg.OTel != nil && cfg.OTel.Enabled,
		Endpoint:      soulOTelEndpoint(cfg.OTel),
		ServiceName:   "soul",
		ResourceAttrs: map[string]string{"soulstack.sid": sid},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: setup OTel: %v\n", err)
		return exitError
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := otelProvider.Shutdown(shutCtx); err != nil {
			logger.Warn("OTel provider shutdown returned error", slog.Any("error", err))
		}
	}()

	client, err := soulgrpc.NewClient(soulgrpc.ClientConfig{
		Endpoints:          endpoints,
		SeedCert:           seedPaths.Cert,
		SeedKey:            seedPaths.Key,
		CAPath:             seedPaths.CA,
		HandshakeTimeout:   handshakeTimeout,
		SoulVersion:        soulVersion,
		SID:                sid,
		MaxRecvMsgSize:     cfg.Keeper.ResolvedMaxApplySize(),
		MaxAttempts:        resolveMaxAttempts(cfg),
		InterAttemptDelay:  clientBackoff.initial,
		InterAttemptJitter: clientBackoff.jitter,
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}

	runner := runtime.NewApplyRunner(registry, applyMetrics)

	// Errand runner (ADR-033): pull-based ad-hoc exec of a single module. Same
	// Registry as applyrunner — core + plugin via CompositeRegistry. Per-
	// process: stateless, survives reconnect/swap (like ApplyRunner). Concurrent-
	// safe (Errands aren't serialized on Soul, unlike apply, ADR-012(a)).
	errandRunner := errandrunner.New(registry, logger, errandMetrics)

	// Beacon scheduler (ADR-030 S1) — per-process, like ApplyRunner: the set of
	// Vigils and their last-state survive reconnect/swap. Harmless without Vigils
	// (does nothing until the first VigilSnapshot). The active set is delivered via
	// handleSession (ReplaceAll), raised Portents go out through the same session's writer-loop.
	scheduler := beacon.NewScheduler(beacon.SchedulerConfig{
		Registry: beaconLookup,
		SID:      sid,
		Logger:   logger,
		Metrics:  beaconMetrics,
	})
	defer scheduler.Stop()

	// Startup validation of keeper.retry/failback/soulprint.refresh_interval —
	// fail-fast on a broken config at startup. After startup these values
	// are re-read from store.Get() per iteration (reconnect-loop /
	// soulprint ticker), so SIGHUP reload updates them without a restart.
	if _, err := loadBackoff(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}
	if _, err := loadFailback(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}
	if _, err := loadSoulprintInterval(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}

	sp := soulprintPusher{
		collector: soulprint.NewCollector(soulprint.NewSystemSource(), soulprintMetrics),
		sid:       sid,
		// interval isn't fixed here: handleSession reads the current
		// soulprint.refresh_interval from the store at the start of each session (hot-reload).
	}

	// Soulprint facts → core modules (Variant A, ADR-018(b)): collect a host
	// snapshot once at startup and inject it into ApplyRunner. core.pkg/core.service read
	// pkg_mgr/init_system from the facts (primary), which fixes the crash on alpine with
	// openrc-soulprint without openrc-tools (BUG-B) and gives a single source-of-truth with
	// CEL `soulprint.self.os.*`. The facts are periodically re-collected for Keeper
	// (soulprintPusher), but for backend selection the startup snapshot suffices: host
	// pkg_mgr/init_system don't change over the process lifetime.
	runner.SetHostFacts(hostFactsFromSoulprint(sp.collector.Collect(ctx, sid)))

	logger.Info("soul run: ready", slog.String("sid", sid), slog.Int("endpoints", len(endpoints)))
	reconnectLoop(ctx, store, client, runner, errandRunner, sp, eventStreamMetrics, sigils, anchorSet, scheduler, logger)
	logger.Info("soul run: shutdown complete")
	return exitOK
}

// buildRegistry assembles the module Registry (core + custom) — shared code for pull
// and push. core is always available; custom discovery walks cfg.Paths.Modules,
// warnings are non-fatal. logPrefix distinguishes the call site in logs
// ("soul run" / "soul apply").
//
// anchors + sigils are DI for Sigil verify (ADR-026(h), R3 multi-anchor) in
// pluginhost.Host: custom plugins go through fail-closed verify before Spawn. In
// pull (`soul run`) we thread through the trust-anchor SET from the seed and the
// runtime-cache adapter; in push (`soul apply`) — an empty set and nil lookup (push has
// no Sigil broadcast cache), custom plugins fail-closed, core MVP works (static
// modules don't go through verify).
//
// The second return value is the Host's own atomic anchor-set holder ([sharedhost.AnchorSet]):
// the recv-loop (`soul run`) swaps its contents on the [keeperv1.SigilTrustAnchors]
// runtime message (R3-S6, ReplaceAll), and the same holder is read by the verify
// phase at Spawn. push (`soul apply`) ignores the holder (no broadcast channel).
//
// The third return value is a composite beacon Lookup (ADR-030 V5-2): core-beacon
// (static) + plugin-beacon from the same discovered set (kind=soul_beacon is a
// separate registry layered on pluginhost.Host.SpawnBeacon).
func buildRegistry(cfg *config.SoulConfig, logger *slog.Logger, logPrefix string, anchors []ed25519.PublicKey, sigils sharedhost.SigilLookup) (*runtime.CompositeRegistry, *sharedhost.AnchorSet, beacon.BeaconLookup, error) {
	host, err := pluginhost.NewHost(cfg.PluginRuntime, anchors, sigils)
	if err != nil {
		return nil, nil, nil, err
	}

	var discovered []pluginhost.Discovered
	if cfg.Paths.Modules != "" {
		found, warnings, err := pluginhost.Discover(cfg.Paths.Modules)
		if err != nil {
			logger.Warn(logPrefix+": plugin discovery skipped",
				slog.String("modules_root", cfg.Paths.Modules),
				slog.Any("error", err))
		} else {
			for _, w := range warnings {
				logger.Warn(logPrefix+": plugin discovery warning", slog.String("detail", w))
			}
			discovered = found
		}
	}

	pluginReg := runtime.NewPluginRegistry(
		runtime.PluginHostSpawner{Host: host},
		discovered,
		logger,
	)
	// Name conflicts `<namespace>.<name>` between core and custom resolve in
	// favor of core (protects against a custom plugin shadowing core.*). Names
	// are logged for audit; repeats on every hot-register.
	var core *coremod.Registry
	logShadowedByCore := func() {
		for _, name := range pluginReg.Names() {
			if _, clash := core.Lookup(name); clash {
				logger.Warn(logPrefix+": custom module shadowed by core",
					slog.String("module", name))
			}
		}
	}
	// core.module (ADR-065) gets the same Sigil set/anchors/cache root
	// used to verify custom plugins; in push mode sigils/anchors are nil →
	// the install step fail-closed module_not_allowed. Rescan is a hot-register
	// (ADR-065(d)) after a successful install; the beacon registry is NOT
	// rebuilt in the process (MVP limitation of ADR-065, hot-reload for soul_beacon is post-MVP).
	core = coremod.Default(installmod.Deps{
		Sigils:      sigils,
		Anchors:     host.SigilAnchors,
		ModulesRoot: cfg.Paths.Modules,
		Rescan: func() {
			warnings, err := pluginReg.Rescan(cfg.Paths.Modules)
			if err != nil {
				logger.Warn(logPrefix+": plugin rescan failed",
					slog.String("modules_root", cfg.Paths.Modules),
					slog.Any("error", err))
				return
			}
			for _, w := range warnings {
				logger.Warn(logPrefix+": plugin discovery warning", slog.String("detail", w))
			}
			logShadowedByCore()
			logger.Info(logPrefix+": modules rescanned",
				slog.Int("custom", len(pluginReg.Names())))
		},
	})
	logShadowedByCore()
	logger.Info(logPrefix+": modules registered",
		slog.Int("core", len(core.Names())),
		slog.Int("custom", len(pluginReg.Names())),
	)

	// Beacon composite lookup (ADR-030 V5-2): plugin-beacon is an out-of-core
	// registry of the same shape, layered on the same pluginhost.Host (SpawnBeacon
	// method). discovered is filtered by kind=soul_beacon inside NewPluginRegistry.
	sharedDiscovered := make([]sharedhost.Discovered, len(discovered))
	for i, d := range discovered {
		sharedDiscovered[i] = d
	}
	beaconPluginReg := beacon.NewPluginRegistry(beaconHostAdapter{host: host}, sharedDiscovered, logger)
	logger.Info(logPrefix+": beacon plugins registered",
		slog.Int("custom_beacons", len(beaconPluginReg.Names())),
	)
	beaconLookup := beacon.NewCompositeRegistry(beacon.Default(), beaconPluginReg, logger)

	return runtime.NewCompositeRegistry(core, pluginReg), host.SigilAnchors, beaconLookup, nil
}

// beaconHostAdapter is a narrow bridge *pluginhost.Host → beacon.PluginBeaconSpawner.
// Adapts SpawnBeacon's return type (*pluginhost.BeaconPlugin) to
// beacon.PluginBeaconSession (a narrow interface) so the beacon package doesn't
// import pluginhost (minimizes coupling; mirrors
// runtime.PluginHostSpawner for SoulModule).
type beaconHostAdapter struct {
	host *pluginhost.Host
}

func (a beaconHostAdapter) SpawnBeacon(ctx context.Context, d sharedhost.Discovered) (beacon.PluginBeaconSession, error) {
	p, err := a.host.SpawnBeacon(ctx, d)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// runApply — `soul apply` (push-oneshot, ADR-004).
//
// Lifecycle:
//  1. (opt.) Load config — only for custom-module paths (paths.modules) and
//     plugin_runtime; SoulSeed/keeper-endpoints aren't needed in push.
//  2. Read stdin → protojson.Unmarshal → ApplyRequest (apply_id + RenderedTask[]).
//  3. Build Registry: core + custom (same as `run`).
//  4. ApplyRunner.Run with NDJSONSink → stdout: stream of TaskEvent + RunResult.
//  5. exit 0 on RunResult.status==SUCCESS, otherwise 1.
//
// Same proto semantics and the same ApplyRunner as pull — only the
// transport differs (stdin/stdout instead of EventStream).
func runApply(args []string) int {
	var configPath string
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&configPath, "config", defaultConfigPath, "soul.yml path (optional; for paths/plugin_runtime, SoulSeed not required)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: soul apply [flags]\n\nReads a rendered ApplyRequest (protojson) from stdin, applies it, and writes\nTaskEvent + RunResult as NDJSON to stdout.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	ctx, cancel := signalContext()
	defer cancel()

	// Config is optional: on load failure (no file / push host without
	// soul.yml) proceed with an empty cfg — core modules are enough, custom discovery
	// just won't run. No hard error here, unlike run/init.
	cfg, cfgErr := loadSoulConfig(configPath)
	if cfgErr != nil {
		cfg = &config.SoulConfig{}
	}
	logger := shlog.New(shlog.FromSoul(cfg.Logging))
	if cfgErr != nil {
		logger.Warn("soul apply: config unavailable, using core modules only",
			slog.String("config", configPath),
			slog.Any("error", cfgErr))
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul apply: read stdin: %v\n", err)
		return exitError
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		fmt.Fprintln(os.Stderr, "soul apply: empty stdin — expected a protojson ApplyRequest")
		return exitUsage
	}

	req := &keeperv1.ApplyRequest{}
	if err := protojson.Unmarshal(raw, req); err != nil {
		fmt.Fprintf(os.Stderr, "soul apply: parse ApplyRequest (protojson): %v\n", err)
		return exitError
	}

	// Push (`soul apply`) has no Sigil broadcast cache/channel and doesn't load a
	// seed — verify runs with nil trust-anchor/lookup, so the anchor holder
	// (2nd return value) isn't needed either. Custom plugins fail-closed
	// (no_trust_anchor/no_sigil); core MVP is unaffected.
	registry, _, _, err := buildRegistry(cfg, logger, "soul apply", nil, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul apply: %v\n", err)
		return exitError
	}

	runner := runtime.NewApplyRunner(registry, nil)
	sink := runtime.NewNDJSONSink(os.Stdout)

	logger.Info("soul apply: start",
		slog.String("apply_id", req.GetApplyId()),
		slog.Int("tasks", len(req.GetTasks())),
	)
	// ApplyRunner sends TaskEvents and the final RunResult to sink. An error here
	// only means an I/O failure writing to stdout (task business errors travel via
	// TaskEvent/RunResult). The final RunResult wasn't written in that case, and we
	// can't determine a status — bail out with an error.
	if err := runner.Run(ctx, req, sink); err != nil {
		fmt.Fprintf(os.Stderr, "soul apply: %v\n", err)
		return exitError
	}

	// Exit code follows the run's final status. Run already wrote the RunResult to
	// stdout; the status is needed both by Keeper and as the SSH session's return code.
	if sink.LastStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		logger.Info("soul apply: finished", slog.String("status", sink.LastStatus().String()))
		return exitError
	}
	logger.Info("soul apply: finished", slog.String("status", "success"))
	return exitOK
}

// reconnectLoop is the outer loop: Dial → handleSession → backoff → repeat.
//
// Each Dial failure increases delay exponentially (capped at backoff.max);
// a successful Dial resets delay. Stops when ctx is cancelled (SIGTERM).
//
// keeper.retry.backoff and keeper.failback are read from store.Get() on every
// iteration (hot-reload, ADR-021): SIGHUP reload updates them without a restart.
// The store snapshot has already passed semantic validation (an invalid duration
// is rejected at the reload phase), so resolveBackoff/resolveFailback here are
// best-effort — on a parse error they return defaults + warn, they don't panic.
func reconnectLoop(ctx context.Context, store *config.Store[config.SoulConfig], client *soulgrpc.Client, runner *runtime.ApplyRunner, errandRunner *errandrunner.Runner, sp soulprintPusher, metrics *soulgrpc.EventStreamMetrics, sigils *sigilcache.Cache, anchors *sharedhost.AnchorSet, scheduler *beacon.Scheduler, logger *slog.Logger) {
	delay := resolveBackoff(store, logger).initial
	// The first iteration is the initial connect; every subsequent Dial attempt is a
	// reconnect (after a disconnect or a failed dial). soul_eventstream_
	// reconnects_total counts re-establishment attempts only, not the initial one.
	firstAttempt := true
	for ctx.Err() == nil {
		if !firstAttempt {
			metrics.IncReconnects()
		}
		firstAttempt = false
		b := resolveBackoff(store, logger)
		sess, err := client.Dial(ctx)
		if err != nil {
			// lease-held (every endpoint returned AlreadyExists, the SID lease still holds a
			// live/unexpired holder after a keeper crash) is a soft failure: Dial succeeded
			// at the transport level, only the session was rejected. Cap backoff at the modest
			// leaseHeldBackoffCap so Soul reconnects within seconds of
			// presence expiring (force-release), instead of hammering surviving keepers the whole
			// window or waiting out the inflated general transport cap (keeper.retry.backoff.max). The general cap
			// for transport failures is left alone — exponential up to max is appropriate there.
			backoffCap := b.max
			leaseHeld := soulgrpc.IsLeaseHeld(err)
			if leaseHeld {
				backoffCap = leaseHeldBackoffCap
				if delay > backoffCap {
					delay = backoffCap
				}
			}
			logger.Warn("soul run: dial failed, will retry",
				slog.Duration("delay", delay),
				slog.Bool("lease_held", leaseHeld),
				slog.Any("error", err),
			)
			if !sleepCtx(ctx, withJitter(delay, b.jitter)) {
				return
			}
			delay = nextDelay(delay, backoffCap)
			continue
		}
		// Successful dial — reset backoff to the current initial.
		delay = b.initial
		metrics.SetConnected(true)
		handleSession(ctx, store, client, sess, runner, errandRunner, sp, sigils, anchors, scheduler, logger)
		// handleSession returned = session closed (clean EOF or error).
		// The next iteration will try Dial again.
		metrics.SetConnected(false)
	}
}

// resolveBackoff reads keeper.retry.backoff from the current store snapshot.
// On a nil snapshot / parse error — defaults + warn (see reconnectLoop).
func resolveBackoff(store *config.Store[config.SoulConfig], logger *slog.Logger) backoffParams {
	cfg := store.Get()
	if cfg == nil {
		return backoffParams{initial: 1 * time.Second, max: 30 * time.Second, jitter: true}
	}
	b, err := loadBackoff(cfg)
	if err != nil {
		logger.Warn("soul run: invalid keeper.retry.backoff in reloaded config, using defaults", slog.Any("error", err))
		return backoffParams{initial: 1 * time.Second, max: 30 * time.Second, jitter: true}
	}
	return b
}

// resolveFailback reads keeper.failback from the current store snapshot.
// On a nil snapshot / parse error — defaults + warn.
func resolveFailback(store *config.Store[config.SoulConfig], logger *slog.Logger) failbackParams {
	def := failbackParams{enabled: true, interval: 1 * time.Hour, spray: 10 * time.Minute}
	cfg := store.Get()
	if cfg == nil {
		return def
	}
	fb, err := loadFailback(cfg)
	if err != nil {
		logger.Warn("soul run: invalid keeper.failback in reloaded config, using defaults", slog.Any("error", err))
		return def
	}
	return fb
}

// resolveSoulprintInterval reads soulprint.refresh_interval from the current
// store snapshot. On a nil snapshot / parse error — default 5m + warn.
func resolveSoulprintInterval(store *config.Store[config.SoulConfig], logger *slog.Logger) time.Duration {
	const def = 5 * time.Minute
	cfg := store.Get()
	if cfg == nil {
		return def
	}
	d, err := loadSoulprintInterval(cfg)
	if err != nil {
		logger.Warn("soul run: invalid soulprint.refresh_interval in reloaded config, using default", slog.Any("error", err))
		return def
	}
	return d
}

// parseTrustAnchorSet parses the trust-anchor set from the runtime
// [keeperv1.SigilTrustAnchors] message (R3-S6): each `pubkey_pem` element is a single
// SPKI "PUBLIC KEY" PEM block (as written by keeper-side sigil.Signer.AnchorSetPEM). Each
// block is parsed with the same [seed.ParseSigilPubKeys] used for the seed's
// anchor set (bootstrap, R3-S4): byte-identical form, shared parser.
//
// fail-closed: any broken block → error for the whole set (caller does NOT swap the
// holder, keeps the previous valid set). Empty input → empty set, no
// error (Sigil disabled on Keeper is a valid state).
func parseTrustAnchorSet(pems []string) ([]ed25519.PublicKey, error) {
	if len(pems) == 0 {
		return nil, nil
	}
	out := make([]ed25519.PublicKey, 0, len(pems))
	for i, p := range pems {
		keys, err := seed.ParseSigilPubKeys([]byte(p))
		if err != nil {
			return nil, fmt.Errorf("anchor %d: %w", i, err)
		}
		// Each element = exactly one SPKI block; ParseSigilPubKeys allows
		// concatenation (returns N), but the SigilTrustAnchors contract is one block per
		// entry. Accept whatever parses (defensive against concatenation).
		out = append(out, keys...)
	}
	return out, nil
}

// recvResult is the outcome of a single stream read. Passed from the reader goroutine
// to handleSession's select-loop.
type recvResult struct {
	msg *keeperv1.FromKeeper
	err error
}

// handleSession is the recv-loop for one session. Accepts FromKeeper, dispatches
// ApplyRequest to ApplyRunner.Run. Returns on io.EOF / Recv error / cancel.
//
// When failback is enabled, a failbackLoop runs alongside it, periodically
// trying to move up to a more preferred priority. On success the current
// session is closed and replaced with a new one (zero-downtime: the new one is open before
// the old one closes). The failback goroutine stops when handleSession
// exits.
func handleSession(ctx context.Context, store *config.Store[config.SoulConfig], client *soulgrpc.Client, sess *soulgrpc.StreamSession, runner *runtime.ApplyRunner, errandRunner *errandrunner.Runner, sp soulprintPusher, sigils *sigilcache.Cache, anchors *sharedhost.AnchorSet, scheduler *beacon.Scheduler, logger *slog.Logger) {
	// failback and soulprint.refresh_interval are read from the store at the start of
	// each session (hot-reload, ADR-021): a new session after reconnect/swap sees
	// current values. Within a session they're fixed — a change applies
	// at the next reconnect (sub-second latency isn't needed here).
	fb := resolveFailback(store, logger)
	sp.interval = resolveSoulprintInterval(store, logger)

	// failbackCtx lives for exactly one failback-loop attempt. On swap, the
	// previous cancel is called before creating a new one — this keeps the
	// number of active failback goroutines ≤ 1. The defer below calls the
	// current cancel on exit.
	var (
		failbackCtx    context.Context
		failbackCancel context.CancelFunc
	)
	stopFailback := func() {
		if failbackCancel != nil {
			failbackCancel()
			failbackCancel = nil
		}
	}
	defer stopFailback()

	// WardRoster (Soul-reconcile, ADR-027(g), S6): the FIRST app message after
	// handshake — a snapshot of tracked apply runs (ReplaceAll). Sent BEFORE any
	// other app message so Keeper's sweep of orphaned dispatched rows
	// happens before this SID's RunResults/TaskEvents arrive. An empty set
	// (process restart) is an explicit "nothing is tracked" declaration. Error = stream
	// broken: bail out, reconnect will re-establish it (like the initial soulprint below).
	if err := sess.SendWardRoster(runner.ActiveSet()); err != nil {
		logger.Warn("ward-roster: send failed (stream broken)", slog.Any("error", err))
		return
	}

	// The first SoulprintReport is sent right when the session is established (onboarding:
	// Keeper gets facts for the freshly-connected host). After that, on the refresh_interval
	// ticker. All Sends happen from this select-loop (via soulprintTick),
	// not from a goroutine — StreamSession isn't concurrent-safe for Send (one writer only).
	if err := sp.pushOnce(ctx, sess); err != nil {
		logger.Warn("soulprint: initial report send failed", slog.Any("error", err))
	}
	soulprintTick := make(chan struct{}, 1)
	stopSoulprint := sp.startTicker(ctx, soulprintTick)
	defer stopSoulprint()

	swapCh := make(chan *soulgrpc.StreamSession)
	startFailback := func(priority int) {
		if !fb.enabled {
			return
		}
		failbackCtx, failbackCancel = context.WithCancel(ctx)
		go failbackLoop(failbackCtx, client, priority, fb, swapCh, logger)
	}
	startFailback(sess.Priority())

	// The Augur client is bound to a specific session (pending-map + Send on its
	// stream, ADR-025). Created per-session: the apply goroutine sends AugurRequest, the
	// reader goroutine delivers the reply straight to the client (Deliver), bypassing the
	// select-loop — otherwise deadlock: the select-loop is blocked in runner.Run while
	// apply runs, and AugurReply would arrive on the same recvCh that the blocked
	// loop reads. On exit/swap the client is closed (pending Fetches get
	// ErrClientClosed). request_id is unique only per-stream — no need for the client
	// to survive a reconnect.
	augurClient := augur.NewClient(sess)

	// The reader goroutine reads the current sess; on swap it's restarted on
	// the new sess. recvCh is unbuffered — a gate through which the
	// reader reports each Recv's outcome; select-loop dispatches on it.
	// The reader delivers AugurReply straight to the Augur client (Deliver) instead of
	// sending it on recvCh — request↔reply correlation goes through the client's pending-map,
	// not the select-loop (which is busy in runner.Run at that point).
	recvCh := make(chan recvResult)
	readerDone := make(chan struct{})
	startReader := func(s *soulgrpc.StreamSession, ac *augur.Client) {
		go func() {
			defer close(readerDone)
			for {
				msg, err := s.Recv()
				if err == nil {
					if reply := msg.GetAugurReply(); reply != nil {
						if !ac.Deliver(reply) {
							logger.Warn("augur: reply with no pending request (timeout/cancel/unknown request_id)",
								slog.String("request_id", reply.GetRequestId()))
						}
						continue
					}
				}
				select {
				case recvCh <- recvResult{msg: msg, err: err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	startReader(sess, augurClient)

	defer func() {
		augurClient.Close()
		_ = sess.Close()
		<-readerDone
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-soulprintTick:
			// Facts-refresh tick: collect and send from this loop (single writer).
			// Send error = stream broken — bail out, reconnect will re-establish it.
			if err := sp.pushOnce(ctx, sess); err != nil {
				logger.Warn("soulprint: report send failed (stream broken)", slog.Any("error", err))
				return
			}
		case portent := <-scheduler.Portents():
			// Beacon scheduler raised a Portent on a state change (ADR-030,
			// edge-triggered). Sent from this select-loop — the session's only
			// writer (StreamSession isn't concurrent-safe for Send). Error = stream
			// broken: bail out, reconnect will re-establish it; the event is already pulled off the
			// channel and is lost on disconnect (like the soulprint tick) — the next State
			// change will raise it again.
			if err := sess.SendFromSoul(&keeperv1.FromSoul{
				Payload: &keeperv1.FromSoul_PortentEvent{PortentEvent: portent},
			}); err != nil {
				logger.Warn("beacon: portent send failed (stream broken)",
					slog.String("vigil", portent.GetBeaconName()),
					slog.Any("error", err))
				return
			}
		case newSess := <-swapCh:
			// Failback found a better priority — switch over: close the
			// old stream first (this unblocks reader-Recv with an err), then set the
			// new one and start a new reader. We already hold the new session
			// open before closing the old one → zero-downtime.
			oldSess := sess
			oldAugur := augurClient
			sess = newSess
			augurClient = augur.NewClient(sess)
			logger.Info("eventstream: failback swap",
				slog.Int("new_priority", newSess.Priority()),
				slog.String("session_id", newSess.SessionID()),
			)
			// Close the old Augur client (pending Fetches on the old session
			// get ErrClientClosed) — the old stream is about to be closed.
			oldAugur.Close()
			_ = oldSess.Close()
			<-readerDone
			readerDone = make(chan struct{})
			startReader(sess, augurClient)
			stopFailback()
			startFailback(sess.Priority())
		case rec := <-recvCh:
			if rec.err != nil {
				if errors.Is(rec.err, io.EOF) {
					logger.Info("soul run: stream closed by Keeper")
				} else {
					logger.Warn("soul run: stream recv error", slog.Any("error", rec.err))
				}
				return
			}
			if ctx.Err() != nil {
				return
			}
			switch payload := rec.msg.GetPayload().(type) {
			case *keeperv1.FromKeeper_ApplyRequest:
				req := payload.ApplyRequest
				logger.Info("apply: received",
					slog.String("apply_id", req.GetApplyId()),
					slog.Int("tasks", len(req.GetTasks())),
					slog.Int("attempt", int(req.GetAttempt())),
				)
				// attempt-fencing guard (ADR-027(g), Phase 2): reject a
				// stale ApplyRequest (attempt < the one already seen for apply_id) — a duplicate
				// from a stale Ward whose original apply has already been accepted. B1: a rejected
				// request is silently dropped (metric + debug log inside AcceptAttempt),
				// no RunResult is sent — Keeper's barrier will close the original apply
				// (higher attempt) with its own RunResult, runTimeout is the backstop.
				if !runner.AcceptAttempt(req.GetApplyId(), req.GetAttempt()) {
					continue
				}
				// Extract the W3C traceparent from ApplyRequest into ctx so apply.run
				// inside runner.Run comes up as a child span of Keeper's
				// grpc.apply_dispatch (end-to-end trace operator → Keeper → Soul, ADR-024).
				// Empty trace_context (old Keeper without the field) → Extract is a no-op,
				// apply.run stays root (forward-compat degradation).
				applyCtx := otel.GetTextMapPropagator().Extract(ctx,
					propagation.MapCarrier{"traceparent": req.GetTraceContext()})
				// Augur client + apply_id in the run's ctx: core.augur.fetch retrieves
				// them via stream.Context() (the generic SoulModule contract of state+params
				// can't express this). delegate=false — data arrives inline through
				// Keeper, the root credential (external system's account) never
				// reaches Soul (ADR-025).
				applyCtx = augur.WithRun(applyCtx, augurClient, req.GetApplyId())
				// FetchModule transport for the current session, for core.module.installed
				// (ADR-065): same ClientConn, separate HTTP/2 stream; mirrors the
				// augur.WithRun pattern.
				applyCtx = installmod.WithFetcher(applyCtx, sess)
				if err := runner.Run(applyCtx, req, sess); err != nil {
					logger.Error("apply: send failed (stream broken)", slog.Any("error", err))
					return
				}
			case *keeperv1.FromKeeper_CancelApply:
				applyID := payload.CancelApply.GetApplyId()
				cancelled := runner.Cancel(applyID)
				logger.Info("apply: cancel received",
					slog.String("apply_id", applyID),
					slog.String("reason", payload.CancelApply.GetReason()),
					slog.Bool("cancelled", cancelled),
				)
			case *keeperv1.FromKeeper_CancelErrand:
				// ADR-033 slice E5: the operator cancelled an in-flight Errand via
				// DELETE /v1/errands/{id} → Keeper sent us CancelErrand.
				// Best-effort: the Errand may have already finished (race with its own
				// terminal state) — Cancel returns false, log + silently ignore.
				cancelReq := payload.CancelErrand
				cancelled := errandRunner.Cancel(cancelReq.GetErrandId())
				logger.Info("errand: cancel received",
					slog.String("errand_id", cancelReq.GetErrandId()),
					slog.Bool("cancelled", cancelled),
				)
			case *keeperv1.FromKeeper_ErrandRequest:
				// ADR-033: pull-based ad-hoc exec of a single module. Run in a separate
				// goroutine so it doesn't block the session's recv-loop (apply currently
				// blocks the loop — that's specific to it, due to ADR-012(a)'s "one
				// in-flight apply" invariant; Errand carries no such invariant, can run
				// alongside apply and alongside other Errands).
				//
				// The same goroutine sends the result. StreamSession is NOT
				// concurrent-safe for Send: sending ErrandResult and
				// TaskEvent/RunResult from the apply goroutine at the same time could split a frame.
				// Protected via writeMu in StreamSession (see send_mutex.go).
				errReq := payload.ErrandRequest
				logger.Info("errand: received",
					slog.String("errand_id", errReq.GetErrandId()),
					slog.String("module", errReq.GetModule()),
					slog.Bool("dry_run", errReq.GetDryRun()),
					slog.Int("timeout_seconds", int(errReq.GetTimeoutSeconds())),
				)
				go func(req *keeperv1.ErrandRequest) {
					result := errandRunner.Run(ctx, req)
					if sendErr := sess.SendErrandResult(result); sendErr != nil {
						logger.Warn("errand: send result failed (stream broken)",
							slog.String("errand_id", req.GetErrandId()),
							slog.Any("error", sendErr))
					}
				}(errReq)
			case *keeperv1.FromKeeper_SigilSnapshot:
				// The full active grant set (ADR-026(h), Variant A): applied
				// as ReplaceAll — replaces the ENTIRE cache with this set. A grant missing from the
				// snapshot is forgotten → near-instant revoke (S6c) without
				// restarting Soul. Empty snapshot = no plugin is granted.
				// Snapshot is the ONLY authoritative source for the set; the cache has a
				// single writer (this recv-loop), verify-phase readers take an RLock.
				snap := payload.SigilSnapshot.GetSigils()
				sigils.ReplaceAll(snap)
				logger.Info("sigil: snapshot applied (ReplaceAll)",
					slog.Int("count", len(snap)),
				)
			case *keeperv1.FromKeeper_SigilTrustAnchors:
				// The full Sigil signing trust-anchor set (ADR-026(h), R3-S6):
				// ReplaceAll on the Host's atomic holder. The set survives reconnect
				// (the holder is created outside the reconnect loop), but a fresh broadcast on every
				// connect/rotation brings it up to date: an anchor missing from the
				// new set is "forgotten" (a retired key stops verifying
				// grants), a new primary becomes trusted near-instantly.
				//
				// fail-closed on a broken set: if even one PEM fails to parse, do NOT
				// swap the holder (keep the previous valid set), warn — otherwise a
				// garbage/incomplete set would open a hole in verify. An empty set
				// (Sigil disabled on Keeper) is a valid state: the holder is cleared,
				// any plugin verify fail-closes on no_trust_anchor.
				if anchors == nil {
					// No holder (e.g. push mode / test harness without a
					// Host) — nowhere to distribute to; verify already fail-closes.
					break
				}
				pems := payload.SigilTrustAnchors.GetPubkeyPem()
				parsed, perr := parseTrustAnchorSet(pems)
				if perr != nil {
					logger.Warn("sigil: trust-anchors broadcast rejected (broken PEM, keeping previous set)",
						slog.Int("count", len(pems)),
						slog.Any("error", perr),
					)
					break
				}
				anchors.SetAnchors(parsed)
				logger.Info("sigil: trust-anchors applied (ReplaceAll)",
					slog.Int("count", len(parsed)),
				)
			case *keeperv1.FromKeeper_PluginSigil:
				// A single PluginSigil (ADR-026(h), Variant A) is a broadcast
				// notification about a new grant, NOT a set mutation. Only SigilSnapshot
				// is authoritative for the set, so we deliberately don't upsert here: otherwise
				// a lost/duplicated single message would corrupt the cache, and revoke
				// wouldn't work (a single message can't "forget" a grant).
				// The set self-heals from the next snapshot.
				sig := payload.PluginSigil
				logger.Debug("sigil: single notification (set unchanged, snapshot is authoritative)",
					slog.String("namespace", sig.GetNamespace()),
					slog.String("name", sig.GetName()),
					slog.String("ref", sig.GetRef()),
				)
			case *keeperv1.FromKeeper_VigilSnapshot:
				// The full active Vigil set (ADR-030, ReplaceAll — same pattern as
				// SigilSnapshot): the scheduler replaces the entire local set. A Vigil missing from the
				// snapshot is stopped and forgotten (disable/removal without
				// restarting Soul), a new one starts from baseline with no Portent. ctx is the
				// parent daemon context: the set survives the current session.
				vigils := payload.VigilSnapshot.GetVigils()
				scheduler.Apply(ctx, vigils)
				logger.Info("beacon: vigil snapshot applied (ReplaceAll)",
					slog.Int("count", len(vigils)),
				)
			case *keeperv1.FromKeeper_SeedRotationReply:
				logger.Info("seed_rotation_reply: ignored (M2.3+)")
			case *keeperv1.FromKeeper_HelloReply:
				logger.Warn("unexpected duplicate HelloReply")
			default:
				logger.Warn("stream: unknown payload", slog.Any("type", fmt.Sprintf("%T", payload)))
			}
		}
	}
}

// failbackLoop periodically tries to establish a new stream at priority <
// currentPriority. On success it sends the new session on swapCh and returns
// (handleSession will restart the loop with the new priority). With currentPriority=1
// or no higher-priority endpoints, it quietly returns (nothing to
// fail back to).
//
// Interval is fb.interval ± fb.spray (uniform jitter), [docs/soul/connection.md
// → Failback]. spray doesn't stretch the interval, it only guards against a thundering
// herd.
func failbackLoop(ctx context.Context, client *soulgrpc.Client, currentPriority int, fb failbackParams, swapCh chan<- *soulgrpc.StreamSession, logger *slog.Logger) {
	if currentPriority <= 1 {
		return
	}
	for {
		d := failbackInterval(fb.interval, fb.spray)
		if !sleepCtx(ctx, d) {
			return
		}
		newSess, err := client.DialPriority(ctx, currentPriority)
		if err != nil {
			if soulgrpc.IsNoHigherPriority(err) {
				// Config changed via hot-reload? Safe to just return —
				// handleSession will restart the loop with the
				// current priority on the next session.
				return
			}
			logger.Debug("failback: attempt failed", slog.Any("error", err))
			continue
		}
		select {
		case swapCh <- newSess:
			return
		case <-ctx.Done():
			_ = newSess.Close()
			return
		}
	}
}

// failbackInterval computes fb.interval ± fb.spray uniformly. spray=0 → exactly
// interval. Guarantees d > 0 (negative interval is clamped to interval/2).
func failbackInterval(interval, spray time.Duration) time.Duration {
	if spray <= 0 {
		return interval
	}
	delta := time.Duration(rand.Int64N(int64(spray*2))) - spray
	d := interval + delta
	if d < interval/2 {
		return interval / 2
	}
	return d
}

// backoffParams is the exponential backoff between reconnect attempts
// (soul.yml::keeper.retry.backoff). Defaults: 1s → 30s, jitter on.
type backoffParams struct {
	initial time.Duration
	max     time.Duration
	jitter  bool
}

// failbackParams are the parameters for proactively returning to a more preferred
// priority (soul.yml::keeper.failback). Defaults: enabled=true, interval=1h,
// spray=10m ([docs/soul/connection.md → Parameters]).
type failbackParams struct {
	enabled  bool
	interval time.Duration
	spray    time.Duration
}

// soulprintReportSink is the narrow StreamSession surface for sending
// SoulprintReport. Kept separate so soulprintPusher is testable without live gRPC.
type soulprintReportSink interface {
	SendSoulprintReport(*keeperv1.SoulprintReport) error
}

// soulprintPusher collects and periodically sends SoulprintReport (M2.3,
// ADR-018). Stateless with respect to the session: pushOnce takes the current
// session's sink, the ticker only signals handleSession's select-loop (which is the writer).
type soulprintPusher struct {
	collector *soulprint.Collector
	sid       string
	interval  time.Duration
}

// pushOnce collects host facts and sends one SoulprintReport to sink.
// Collection is fast (reading /proc / net), runs in the caller's goroutine (select-loop).
func (p soulprintPusher) pushOnce(ctx context.Context, sink soulprintReportSink) error {
	return sink.SendSoulprintReport(p.collector.Collect(ctx, p.sid))
}

// hostFactsFromSoulprint extracts a narrow backend snapshot (pkg-mgr / init-system)
// from a collected SoulprintReport for injection into core modules (Variant A, ADR-018(b)).
// A nil/sparse report (factless host, partial collection) → empty HostFacts: core modules
// fall back to runtime detection. os.pkg_mgr/os.init_system string values match
// the closed-set util.PkgMgr/util.InitSystem (ADR-018 — shared vocabulary); an unknown
// value is treated as unknown by the detectors via ResolvePkgMgr/ResolveInitSystem.
func hostFactsFromSoulprint(rep *keeperv1.SoulprintReport) coremodutil.HostFacts {
	os := rep.GetTypedFacts().GetOs()
	return coremodutil.HostFacts{
		PkgMgr:     coremodutil.PkgMgr(os.GetPkgMgr()),
		InitSystem: coremodutil.InitSystem(os.GetInitSystem()),
	}
}

// startTicker runs a goroutine that signals tick every interval. tick must be
// buffered 1: if the select-loop is busy (apply in-flight), the tick isn't
// lost but doesn't pile up either (coalescing). Returns a stop func.
func (p soulprintPusher) startTicker(ctx context.Context, tick chan<- struct{}) func() {
	tickerCtx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-tickerCtx.Done():
				return
			case <-t.C:
				select {
				case tick <- struct{}{}:
				default: // previous tick not yet consumed — skip (coalescing)
				}
			}
		}
	}()
	return cancel
}

// loadSoulprintInterval — soul.yml::soulprint.refresh_interval. Default 5m
// (docs/soul/config.md). Missing block → default.
func loadSoulprintInterval(cfg *config.SoulConfig) (time.Duration, error) {
	const def = 5 * time.Minute
	if cfg.Soulprint == nil || cfg.Soulprint.RefreshInterval == "" {
		return def, nil
	}
	d, err := config.ParseDuration(cfg.Soulprint.RefreshInterval)
	if err != nil {
		return 0, fmt.Errorf("soulprint.refresh_interval: %w", err)
	}
	return d, nil
}

func loadFailback(cfg *config.SoulConfig) (failbackParams, error) {
	fb := failbackParams{enabled: true, interval: 1 * time.Hour, spray: 10 * time.Minute}
	if cfg.Keeper.Failback == nil {
		return fb, nil
	}
	fb.enabled = cfg.Keeper.Failback.Enabled
	if cfg.Keeper.Failback.Interval != "" {
		d, err := config.ParseDuration(cfg.Keeper.Failback.Interval)
		if err != nil {
			return fb, fmt.Errorf("keeper.failback.interval: %w", err)
		}
		fb.interval = d
	}
	if cfg.Keeper.Failback.Spray != "" {
		d, err := config.ParseDuration(cfg.Keeper.Failback.Spray)
		if err != nil {
			return fb, fmt.Errorf("keeper.failback.spray: %w", err)
		}
		fb.spray = d
	}
	return fb, nil
}

func loadBackoff(cfg *config.SoulConfig) (backoffParams, error) {
	b := backoffParams{initial: 1 * time.Second, max: 30 * time.Second, jitter: true}
	if cfg.Keeper.Retry == nil {
		return b, nil
	}
	bk := cfg.Keeper.Retry.Backoff
	if bk.Initial != "" {
		d, err := config.ParseDuration(bk.Initial)
		if err != nil {
			return b, fmt.Errorf("keeper.retry.backoff.initial: %w", err)
		}
		b.initial = d
	}
	if bk.Max != "" {
		d, err := config.ParseDuration(bk.Max)
		if err != nil {
			return b, fmt.Errorf("keeper.retry.backoff.max: %w", err)
		}
		b.max = d
	}
	b.jitter = bk.Jitter
	return b, nil
}

// resolveMaxAttempts — soul.yml::keeper.retry.max_attempts, resolving 0→2
// (mirrors the default in soulgrpc.NewClient). Missing retry block / zero
// value → defaultClientMaxAttempts. Validation (>=1) already happened at the
// config phase (shared/config/schema.go); this only resolves the default.
func resolveMaxAttempts(cfg *config.SoulConfig) int {
	const defaultClientMaxAttempts = 2
	if cfg.Keeper.Retry == nil || cfg.Keeper.Retry.MaxAttempts <= 0 {
		return defaultClientMaxAttempts
	}
	return cfg.Keeper.Retry.MaxAttempts
}

func parseHandshakeTimeout(cfg *config.SoulConfig) (time.Duration, error) {
	if cfg.Keeper.Retry == nil || cfg.Keeper.Retry.HandshakeTimeout == "" {
		return 10 * time.Second, nil
	}
	d, err := config.ParseDuration(cfg.Keeper.Retry.HandshakeTimeout)
	if err != nil {
		return 0, fmt.Errorf("keeper.retry.handshake_timeout: %w", err)
	}
	return d, nil
}

func nextDelay(cur, max time.Duration) time.Duration {
	n := cur * 2
	if n > max {
		return max
	}
	return n
}

// withJitter adds ±25% random spread, guarding against a reconnect thundering
// herd on a cluster-wide Keeper outage.
func withJitter(d time.Duration, enabled bool) time.Duration {
	if !enabled || d <= 0 {
		return d
	}
	delta := d / 4
	return d + time.Duration(rand.Int64N(int64(delta*2))) - delta
}

// sleepCtx waits for d or ctx cancellation. Returns true on timeout (continue),
// false if ctx was cancelled (caller should exit).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// orderedByPriority returns endpoints sorted by priority (lower first)
// without mutating the input slice. priority=0 normalizes to 1 (default =
// highest), mirroring the EventStream client (internal/grpc). Used by the
// bootstrap phase: failback doesn't apply there (one-shot), but the ordering
// is the same. See docs/soul/connection.md.
func orderedByPriority(in []config.SoulKeeperEndpoint) []config.SoulKeeperEndpoint {
	out := make([]config.SoulKeeperEndpoint, len(in))
	copy(out, in)
	norm := func(p int) int {
		if p == 0 {
			return 1
		}
		return p
	}
	sort.SliceStable(out, func(i, j int) bool { return norm(out[i].Priority) < norm(out[j].Priority) })
	return out
}

// loadSoulConfig reads and validates soul.yml, returning the first error
// phase as a readable message. logging fields (including logging.file /
// logging.rotation.max_age_days) are now normative in the shared schema — no
// separate overlay pass anymore.
func loadSoulConfig(path string) (*config.SoulConfig, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load config %q: %w", path, err)
	}
	cfg, _, diags, _ := config.LoadSoulFromBytes(path, src, config.ValidateOptions{})
	if diag.HasErrors(diags) {
		var sb []byte
		sb = append(sb, []byte(fmt.Sprintf("config %q has errors:", path))...)
		for _, d := range diags {
			if d.Level == diag.LevelError {
				sb = append(sb, []byte(fmt.Sprintf("\n  - %s [%s]: %s", d.Phase, d.Code, d.Message))...)
			}
		}
		return nil, errors.New(string(sb))
	}
	return cfg, nil
}

// soulOTelEndpoint extracts the endpoint from the optional otel block (empty
// string if unset), avoiding a nil-deref in obs.OTelConfig.
func soulOTelEndpoint(o *config.SoulOTel) string {
	if o == nil {
		return ""
	}
	return o.Endpoint
}

// resolveSoulMetricsBasicAuth builds *obs.BasicAuth for soul's `/metrics`.
//
// Returns (nil, nil) if basic-auth is unset/disabled — the listener comes up
// without auth (loopback-bind is the protection). When enabled, reads the
// password from a file on disk (Soul has no vault client, ADR-012): one line,
// trailing whitespace/newline trimmed. Empty file → error (fail-fast: better
// to fail at startup than serve `/metrics` with an empty password). File
// contents never appear in logs/errors.
func resolveSoulMetricsBasicAuth(b *config.SoulMetricsBasicAuth) (*obs.BasicAuth, error) {
	if b == nil || !b.Enabled {
		return nil, nil
	}
	raw, err := os.ReadFile(b.PasswordFile)
	if err != nil {
		// b.PasswordFile is a path (not a secret) — echoed for diagnostics.
		return nil, fmt.Errorf("read metrics.basic_auth.password_file %q: %w", b.PasswordFile, err)
	}
	pass := strings.TrimRight(string(raw), "\r\n")
	if pass == "" {
		return nil, fmt.Errorf("metrics.basic_auth.password_file %q is empty", b.PasswordFile)
	}
	return &obs.BasicAuth{Username: b.Username, Password: pass}, nil
}

// signalContext — a context cancelled on SIGINT/SIGTERM. SIGHUP is handled
// separately via [config.WatchSIGHUP] so reload doesn't get tangled with
// shutdown.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
