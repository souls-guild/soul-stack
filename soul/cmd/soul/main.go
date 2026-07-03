// Package main — entrypoint бинаря `soul` под ADR-004 / ADR-011 / ADR-012.
//
// Subcommand-router на stdlib `flag`:
//
//	soul init  --token=<bootstrap-token> [--config=<path>] [--sid=<sid>]
//	soul run                            [--config=<path>]
//	soul apply                          [--config=<path>]
//	soul help
//
// `init` — единократный bootstrap-цикл (ADR-012(b)): генерация key+CSR →
// unary Bootstrap RPC к Keeper → запись SoulSeed на диск. Bootstrap-токен
// берётся из --token ИЛИ из env SOUL_BOOTSTRAP_TOKEN (флаг побеждает env).
// Env-форма предпочтительнее: флаг светится в `ps` и shell-history, env — нет.
//
// `run` — долгоживущий демон-loop: load SoulSeed → Discover custom-плагинов
// → собрать Registry (core + custom) → dial EventStream к Keeper → recv-loop
// + dispatch ApplyRequest → ApplyRunner → send TaskEvent / RunResult.
// Reconnect при разрыве — внутренний loop с экспоненциальным backoff.
//
// `apply` — push-oneshot режим (ADR-004): читает отрендеренный ApplyRequest
// (protojson) из stdin → собирает Registry (core + custom) → ApplyRunner →
// пишет поток TaskEvent + финальный RunResult в stdout как NDJSON (protojson).
// exit 0 при RunResult.status==success, 1 иначе. SoulSeed/mTLS не требуется —
// доверие обеспечивает аутентифицированный Архонт + SSH-канал от Keeper-а.
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

// envBootstrapToken — env-var с bootstrap-токеном для `soul init`,
// безопасная альтернатива --token (флаг виден в `ps`/shell-history).
const envBootstrapToken = "SOUL_BOOTSTRAP_TOKEN"

const (
	defaultConfigPath = "/etc/soul/soul.yml"
	// defaultSoulMetricsListen — loopback по умолчанию для `/metrics`
	// (docs/soul/config.md → metrics.listen). Loopback, чтобы метрик-порт не
	// торчал наружу — главная защита Soul-метрик в этом slice (auth отложен).
	defaultSoulMetricsListen = "127.0.0.1:9091"
)

// leaseHeldBackoffCap — модест-cap reconnect-backoff для lease-held ветки
// (Dial отвергнут AlreadyExists: SID-lease ещё держит живой holder, см.
// soulgrpc.IsLeaseHeld). Намеренно мал и НЕ конфигурируем: после краха keeper-а
// presence истекает за ~30s, после чего force-release освобождает lease — Soul
// обязан переподключиться в пределах нескольких секунд, а не ждать общий
// transport-cap (keeper.retry.backoff.max, десятки секунд). Значение не доводим
// до конфиг-ключа: это внутренний инвариант recovery-latency, а не оператор-tunable.
const leaseHeldBackoffCap = 3 * time.Second

// soulVersion печатается в Hello.soul_version и BootstrapRequest.soul_version
// для аудита. Значение по умолчанию — для `go run`/IDE-сборок; в релизных
// сборках перезаписывается линкером через `-ldflags "-X ...soulVersion=<ver>"`
// (см. Makefile, переменная VERSION). Именно `var`, а не `const`, потому что
// `-X` умеет инжектить только в package-level string-переменные.
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

// resolveInitToken выбирает bootstrap-токен по precedence: явный --token
// побеждает env SOUL_BOOTSTRAP_TOKEN (флаг = override). Пустой --token → env.
// Оба пусты → ошибка (хотя бы один источник обязателен). Env-форма безопаснее:
// --token виден в `ps`/shell-history, env — нет.
func resolveInitToken(flagToken string) (string, error) {
	if flagToken != "" {
		return flagToken, nil
	}
	if envToken := os.Getenv(envBootstrapToken); envToken != "" {
		return envToken, nil
	}
	return "", fmt.Errorf("soul init: provide token via --token or %s", envBootstrapToken)
}

// runInit парсит флаги, поднимает зависимости (config), вызывает
// bootstrap.Run и печатает итоги.
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

	// Bootstrap-фаза бьёт в bootstrap_port; перебор хостов упорядочен по
	// priority, без in-group shuffle — bootstrap one-shot, порядок
	// детерминирован (spray есть только в EventStream-клиенте). failback
	// к bootstrap неприменим (one-shot). См. docs/soul/connection.md.
	endpoints := make([]string, 0, len(cfg.Keeper.Endpoints))
	for _, ep := range orderedByPriority(cfg.Keeper.Endpoints) {
		endpoints = append(endpoints, ep.BootstrapAddr())
	}

	timeout, err := parseHandshakeTimeout(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul init: %v\n", err)
		return exitError
	}

	// --sid флаг > config.sid > os.Hostname (резолв ниже в bootstrap.Run).
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
// Жизненный цикл:
//  1. Load config + load SoulSeed (cert/key/ca).
//  2. Discover custom-плагинов в paths.modules (warnings logged, не fatal).
//  3. Сборка Registry: coremod.Default() — single source для MVP.
//     (custom-modules через pluginhost — wire-up для discovery'я,
//     dispatch к ним — Plugin.d / M2.3).
//  4. Reconnect-loop: Dial → recv-loop → дисконнект → backoff → retry.
//     SIGINT/SIGTERM прерывают loop.
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

	// Store вместо плоского loadSoulConfig (ADR-021 hot-reload): даёт снимок
	// для стартового wire-up + перечитывается по SIGHUP. reconnect-loop и
	// soulprint-ticker читают store.Get() в точке использования, поэтому
	// next-iteration видит новые keeper.retry/failback + soulprint.refresh_interval.
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

	// Hot-reload `logging.level` (ADR-021): сдвигаем порог логирования по
	// новому снимку на каждый успешный Store-swap. file/format/rotation —
	// restart-required (docs/soul/config.md), их не трогаем.
	store.OnReload(func(_, newCfg *config.SoulConfig) {
		if newCfg != nil {
			logLevel.Set(newCfg.Logging.Level)
		}
	})

	// SIGHUP-reload (ADR-021(b)): отдельный signal-канал внутри WatchSIGHUP,
	// SIGHUP не путается с SIGINT/SIGTERM из signalContext. Запускается только
	// при hot_reload.enable_signal (default true). push-режим (soul apply)
	// hot-reload не касается — это one-shot. Audit на стороне Soul нет
	// (нет audit_log-БД), reload только логируется.
	if cfg.HotReload.SignalEnabled() {
		reloadCh := config.WatchSIGHUP(ctx, store)
		go config.LogReloads(reloadCh, logger)
		logger.Info("soul run: SIGHUP config reload enabled")
	} else {
		logger.Info("soul run: SIGHUP config reload disabled (hot_reload.enable_signal=false)")
	}

	// SoulSeed — load до всего остального: если bootstrap не выполнен,
	// дальше идти бессмысленно. Чёткое сообщение «run soul init».
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

	// Набор trust-anchor-ов verify Sigil (ADR-026(h), R3 multi-anchor): парсим
	// sigil_pubkey.pem из seed-а — он может нести несколько PEM-блоков подряд
	// (multi-anchor для безразрывной ротации ключа подписи). Пусто (Sigil
	// выключен на Keeper) → пустой набор: verify любого custom-плагина
	// fail-closed по no_trust_anchor. Битый PEM → отказ старта (не молчаливое
	// fail-open). Core-модули статические — verify не проходят, не затронуты.
	sigilAnchors, err := seed.ParseSigilPubKeys(seedMaterial.SigilPubKeyPEM)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}

	// Sigil-кеш (ADR-026, S6a) живёт на runtime-уровне Soul — создаётся один
	// раз здесь, вне reconnect-loop, поэтому допуски плагинов переживают разрыв
	// и переустановку EventStream-а. Keeper рассылает PluginSigil broadcast-ом
	// при подключении; recv-loop в handleSession складывает их сюда. Verify
	// против кеша — S6b (через SigilLookupAdapter в pluginhost.Host).
	sigils := sigilcache.New()

	// Custom-modules discovery + lazy-spawn wire-up (ADR-020(d): one-shot per
	// Apply). Warnings — non-fatal: core MVP остаётся работоспособным без
	// custom-плагинов. Сам Host создаётся даже при пустом списке плагинов —
	// это дёшево, упрощает code path при отсутствии paths.modules. Trust-anchor
	// + кеш-адаптер прокидываются в Host для fail-closed Sigil-verify плагинов.
	registry, anchorSet, beaconLookup, err := buildRegistry(cfg, logger, "soul run", sigilAnchors, pluginhost.NewSigilLookupAdapter(sigils))
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}

	// EventStream-фаза бьёт в event_stream_port; priority + spray-shuffle
	// сохраняются (docs/soul/connection.md).
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

	// Per-endpoint retry (keeper.retry.max_attempts) + плоская inter-attempt пауза
	// = reuse backoff.initial/jitter (никаких новых конфиг-ключей). backoff здесь
	// нужен только для статической сборки ClientConfig; reconnectLoop читает свой
	// snapshot из store per-iteration (hot-reload).
	clientBackoff, err := loadBackoff(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soul run: %v\n", err)
		return exitError
	}
	// SID-резолв: config.sid > os.Hostname (lowercased). Lowercase симметричен
	// bootstrap.Run (bootstrap.go) — иначе хост с MixedCase-hostname получит на
	// init и run разный SID. `soul run` не имеет --sid флага (в отличие от init).
	sid := cfg.SID
	if sid == "" {
		host, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "soul run: detect hostname: %v\n", err)
			return exitError
		}
		sid = strings.ToLower(strings.TrimSpace(host))
	}

	// Observability-стек (ADR-024). Registry шарится между exposition-handler-ом
	// `/metrics` и инструментацией подсистем (apply-цикл / EventStream-клиент /
	// soulprint-коллектор). Регистрируем soul_*-collectors сразу — дескрипторы
	// прокидываются в подсистемы ниже. docs/observability.md §4.0: collector
	// живёт рядом с подсистемой, register — на soul-registry.
	reg := obs.NewRegistry()
	applyMetrics := runtime.RegisterApplyMetrics(reg)
	eventStreamMetrics := soulgrpc.RegisterEventStreamMetrics(reg)
	soulprintMetrics := soulprint.RegisterSoulprintMetrics(reg)
	beaconMetrics := beacon.RegisterBeaconMetrics(reg)
	errandMetrics := errandrunner.Register(reg)

	// `/metrics` — listener на cfg.Metrics.Listen (loopback 127.0.0.1:9091
	// по умолчанию). Опц. HTTP Basic-auth через metrics.basic_auth: пароль
	// читается из файла на диске (у Soul нет vault-клиента, ADR-012). Без
	// basic-auth защита эндпоинта — loopback-bind. Опционален: при
	// metrics.enabled=false listener не поднимается.
	if cfg.Metrics != nil && cfg.Metrics.Enabled {
		// Default loopback-адрес (docs/soul/config.md → metrics.listen),
		// если оператор включил метрики, но не задал listen — config-парсер
		// defaults не инжектит, применяем здесь.
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

	// OTel-провайдер (ADR-024): service.name="soul" + кастомный soulstack.sid.
	// Trace-export при otel.enabled+endpoint; иначе no-op. Setup один раз за
	// процесс — otel.* restart-required (hot-reload его не трогает).
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

	// Errand-runner (ADR-033): pull-ad-hoc exec одиночного модуля. Тот же
	// Registry, что у applyrunner — core + plugin через CompositeRegistry. Per-
	// process: stateless, переживает reconnect/swap (как ApplyRunner). Concurrent-
	// safe (Errand-ы не сериализуются на Soul, в отличие от apply, ADR-012(a)).
	errandRunner := errandrunner.New(registry, logger, errandMetrics)

	// Beacon-scheduler (ADR-030 S1) — per-process, как ApplyRunner: набор Vigil
	// и их last-state переживают reconnect/swap. Безвреден без Vigil (ничего не
	// делает до первого VigilSnapshot). Активный набор едет через handleSession
	// (ReplaceAll), поднятые Portent уходят writer-loop-ом той же сессии.
	scheduler := beacon.NewScheduler(beacon.SchedulerConfig{
		Registry: beaconLookup,
		SID:      sid,
		Logger:   logger,
		Metrics:  beaconMetrics,
	})
	defer scheduler.Stop()

	// Стартовая валидация keeper.retry/failback/soulprint.refresh_interval —
	// fail-fast при кривом конфиге на старте. После старта эти значения
	// перечитываются из store.Get() per-iteration (reconnect-loop /
	// soulprint-ticker), поэтому SIGHUP-reload меняет их без рестарта.
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
		// interval не фиксируем здесь: handleSession читает актуальное
		// soulprint.refresh_interval из store на старте каждой сессии (hot-reload).
	}

	// Soulprint-факт → core-модули (Вариант A, ADR-018(b)): собираем снимок хоста
	// один раз на старте и инжектим в ApplyRunner. core.pkg/core.service читают
	// pkg_mgr/init_system из факта (primary), это убирает падение на alpine с
	// openrc-soulprint без openrc-tools (BUG-B) и даёт единый source-of-truth с
	// CEL `soulprint.self.os.*`. Факт периодически пере-собирается для Keeper-а
	// (soulprintPusher), но для backend-выбора достаточно старта: pkg_mgr/
	// init_system хоста за время жизни процесса не меняются.
	runner.SetHostFacts(hostFactsFromSoulprint(sp.collector.Collect(ctx, sid)))

	logger.Info("soul run: ready", slog.String("sid", sid), slog.Int("endpoints", len(endpoints)))
	reconnectLoop(ctx, store, client, runner, errandRunner, sp, eventStreamMetrics, sigils, anchorSet, scheduler, logger)
	logger.Info("soul run: shutdown complete")
	return exitOK
}

// buildRegistry собирает Registry модулей (core + custom) — общий код pull и
// push. core всегда доступен; custom discovery идёт по cfg.Paths.Modules,
// warnings — non-fatal. logPrefix различает источник вызова в логах
// ("soul run" / "soul apply").
//
// anchors + sigils — DI verify Sigil (ADR-026(h), R3 multi-anchor) в
// pluginhost.Host: custom-плагины проходят fail-closed verify перед Spawn. В
// pull (`soul run`) прокидываются НАБОР trust-anchor-ов из seed-а и адаптер
// runtime-кеша; в push (`soul apply`) — пустой набор и nil-lookup (broadcast-кеша
// Sigil в push нет), custom-плагины fail-closed, core MVP работает (статические
// модули verify не проходят).
//
// Вторым результатом возвращает atomic-holder набора якорей ([sharedhost.AnchorSet])
// самого Host-а: recv-loop (`soul run`) подменяет в нём набор по runtime-сообщению
// [keeperv1.SigilTrustAnchors] (R3-S6, ReplaceAll), и тот же holder читается verify-
// фазой при Spawn. push (`soul apply`) holder игнорирует (broadcast-канала нет).
//
// Третьим результатом — composite beacon-Lookup (ADR-030 V5-2): core-beacon
// (статика) + plugin-beacon из того же discovered-набора (kind=soul_beacon
// отдельный реестр поверх pluginhost.Host.SpawnBeacon).
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
	// Конфликты имён `<namespace>.<name>` между core и custom разрешаются в
	// пользу core (защита от подмены core.* кастомным плагином). Имена
	// записываем в лог для аудита; повторяется при каждом hot-register.
	var core *coremod.Registry
	logShadowedByCore := func() {
		for _, name := range pluginReg.Names() {
			if _, clash := core.Lookup(name); clash {
				logger.Warn(logPrefix+": custom module shadowed by core",
					slog.String("module", name))
			}
		}
	}
	// core.module (ADR-065) получает Sigil-набор/якоря/корень кеша — те же,
	// что у verify custom-плагинов; в push-режиме sigils/anchors nil →
	// install-шаг fail-closed module_not_allowed. Rescan — hot-register
	// (ADR-065(d)) после успешной установки; beacon-реестр при этом НЕ
	// пересобирается (MVP-ограничение ADR-065, hot-reload soul_beacon — post-MVP).
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

	// Beacon composite-lookup (ADR-030 V5-2): plugin-beacon — out-of-core
	// одноимённый реестр над тем же pluginhost.Host (SpawnBeacon-метод). discovered
	// фильтруется по kind=soul_beacon внутри NewPluginRegistry.
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

// beaconHostAdapter — узкий мост *pluginhost.Host → beacon.PluginBeaconSpawner.
// Адаптирует тип возврата SpawnBeacon (*pluginhost.BeaconPlugin) к
// beacon.PluginBeaconSession (узкий интерфейс), чтобы beacon-пакет НЕ
// импортировал pluginhost (минимизация сцеплений; параллель
// runtime.PluginHostSpawner для SoulModule).
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
// Жизненный цикл:
//  1. (опц.) Load config — только для путей custom-модулей (paths.modules) и
//     plugin_runtime; SoulSeed/keeper-endpoints в push не нужны.
//  2. Read stdin → protojson.Unmarshal → ApplyRequest (apply_id + RenderedTask[]).
//  3. Сборка Registry: core + custom (как в `run`).
//  4. ApplyRunner.Run с NDJSONSink → stdout: поток TaskEvent + RunResult.
//  5. exit 0 при RunResult.status==SUCCESS, иначе 1.
//
// Та же proto-семантика и тот же ApplyRunner, что и pull — отличается только
// транспорт (stdin/stdout вместо EventStream).
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

	// Config опционален: при ошибке загрузки (нет файла / push-хост без
	// soul.yml) идём с пустым cfg — core-модулей достаточно, custom discovery
	// просто не запустится. Жёсткой ошибки нет, в отличие от run/init.
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

	// Push (`soul apply`) не имеет broadcast-кеша Sigil и не грузит seed —
	// verify Sigil идёт с nil trust-anchor/lookup. Custom-плагины в push
	// fail-closed (no_trust_anchor/no_sigil); core MVP не затронут.
	// push (`soul apply`) не имеет broadcast-канала Sigil — holder якорей не нужен
	// (custom-плагины fail-closed; core MVP не затронут), игнорируем второй результат.
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
	// ApplyRunner шлёт TaskEvent-ы и финальный RunResult в sink. error здесь —
	// только I/O-сбой записи в stdout (бизнес-ошибки задач уезжают через
	// TaskEvent/RunResult). Финальный RunResult в этом случае не записан, но
	// статус мы определить не можем — выходим с ошибкой.
	if err := runner.Run(ctx, req, sink); err != nil {
		fmt.Fprintf(os.Stderr, "soul apply: %v\n", err)
		return exitError
	}

	// exit-код по финальному статусу прогона. Run уже записал RunResult в
	// stdout; статус нужен Keeper-у и как код возврата SSH-сессии.
	if sink.LastStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		logger.Info("soul apply: finished", slog.String("status", sink.LastStatus().String()))
		return exitError
	}
	logger.Info("soul apply: finished", slog.String("status", "success"))
	return exitOK
}

// reconnectLoop — внешний цикл: Dial → handleSession → backoff → repeat.
//
// Каждый Dial-fail увеличивает delay экспоненциально (capped к backoff.max);
// успешный Dial сбрасывает delay. Stop — на отмене ctx (SIGTERM).
//
// keeper.retry.backoff и keeper.failback читаются из store.Get() на каждой
// итерации (hot-reload, ADR-021): SIGHUP-reload меняет их без рестарта.
// store-снимок уже прошёл semantic-валидацию (невалидный duration отвергнут
// на reload-фазе), поэтому resolveBackoff/resolveFailback здесь
// best-effort — на parse-ошибке возвращают дефолты + warn, не падают.
func reconnectLoop(ctx context.Context, store *config.Store[config.SoulConfig], client *soulgrpc.Client, runner *runtime.ApplyRunner, errandRunner *errandrunner.Runner, sp soulprintPusher, metrics *soulgrpc.EventStreamMetrics, sigils *sigilcache.Cache, anchors *sharedhost.AnchorSet, scheduler *beacon.Scheduler, logger *slog.Logger) {
	delay := resolveBackoff(store, logger).initial
	// Первая итерация — initial connect; каждая последующая попытка Dial — это
	// reconnect (после разрыва или после неудачного dial). soul_eventstream_
	// reconnects_total считает именно попытки переустановки, не первичную.
	firstAttempt := true
	for ctx.Err() == nil {
		if !firstAttempt {
			metrics.IncReconnects()
		}
		firstAttempt = false
		b := resolveBackoff(store, logger)
		sess, err := client.Dial(ctx)
		if err != nil {
			// lease-held (все endpoint-ы отдали AlreadyExists, SID-lease ещё держит
			// живой/не-истёкший holder после краха keeper-а) — soft-failure: Dial удался
			// на транспорте, отвергнута только сессия. Капируем backoff модест-значением
			// leaseHeldBackoffCap, чтобы Soul переподключился в пределах секунд после
			// истечения presence (force-release), а не долбил выживших keeper-ов всё окно
			// и не ждал раздутый общий transport-cap (keeper.retry.backoff.max). Общий cap
			// для transport-сбоев НЕ трогаем — там exponential до max уместен.
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
		// Успешный dial — сбрасываем backoff к актуальному initial.
		delay = b.initial
		metrics.SetConnected(true)
		handleSession(ctx, store, client, sess, runner, errandRunner, sp, sigils, anchors, scheduler, logger)
		// handleSession вернулся = сессия закрыта (clean EOF или error).
		// На следующей итерации Dial снова попробует.
		metrics.SetConnected(false)
	}
}

// resolveBackoff читает keeper.retry.backoff из текущего store-снимка.
// На nil-снимке / parse-ошибке — дефолты + warn (см. reconnectLoop).
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

// resolveFailback читает keeper.failback из текущего store-снимка.
// На nil-снимке / parse-ошибке — дефолты + warn.
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

// resolveSoulprintInterval читает soulprint.refresh_interval из текущего
// store-снимка. На nil-снимке / parse-ошибке — дефолт 5m + warn.
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

// parseTrustAnchorSet парсит набор trust-anchor-ов из runtime-сообщения
// [keeperv1.SigilTrustAnchors] (R3-S6): каждый элемент `pubkey_pem` — один SPKI
// PEM-блок "PUBLIC KEY" (как пишет keeper-side sigil.Signer.AnchorSetPEM). Парсинг
// каждого блока — той же [seed.ParseSigilPubKeys], что и набор из seed-а
// (bootstrap, R3-S4): форма byte-идентична, парсер общий.
//
// fail-closed: любой битый блок → ошибка для всего набора (caller НЕ подменяет
// holder, оставляет прежний валидный набор). Пустой вход → пустой набор без
// ошибки (Sigil выключен на Keeper — валидное состояние).
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
		// Каждый элемент = ровно один SPKI-блок; ParseSigilPubKeys допускает
		// конкатенацию (вернёт N), но контракт SigilTrustAnchors — один блок на
		// строку. Принимаем всё, что распарсилось (defensive к конкатенации).
		out = append(out, keys...)
	}
	return out, nil
}

// recvResult — результат одного чтения из stream. Передаётся из reader-горутины
// в select-loop handleSession-а.
type recvResult struct {
	msg *keeperv1.FromKeeper
	err error
}

// handleSession — recv-loop одной сессии. Принимает FromKeeper, диспетчит
// ApplyRequest в ApplyRunner.Run. Возврат — на io.EOF / Recv-error / cancel.
//
// Если включён failback, рядом запускается failbackLoop, который периодически
// пробует подняться на более предпочтительный приоритет. При успехе текущая
// сессия закрывается и заменяется новой (zero-downtime: новая открыта до того,
// как старая закрыта). Failback-горутина останавливается при выходе из
// handleSession.
func handleSession(ctx context.Context, store *config.Store[config.SoulConfig], client *soulgrpc.Client, sess *soulgrpc.StreamSession, runner *runtime.ApplyRunner, errandRunner *errandrunner.Runner, sp soulprintPusher, sigils *sigilcache.Cache, anchors *sharedhost.AnchorSet, scheduler *beacon.Scheduler, logger *slog.Logger) {
	// failback и soulprint.refresh_interval читаются из store на старте каждой
	// сессии (hot-reload, ADR-021): новая сессия после reconnect/swap видит
	// актуальные значения. Внутри сессии они фиксированы — изменение применяется
	// при следующем reconnect (sub-second-латентность не нужна).
	fb := resolveFailback(store, logger)
	sp.interval = resolveSoulprintInterval(store, logger)

	// failbackCtx живёт ровно для одной попытки failback-loop. При swap-е
	// предыдущий cancel вызывается до создания нового — это держит число
	// активных failback-горутин ≤ 1. defer ниже вызывает текущий cancel при
	// выходе.
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

	// WardRoster (Soul-reconcile, ADR-027(g), S6): ПЕРВОЕ app-сообщение после
	// handshake — снимок ведомых apply-прогонов (ReplaceAll). Шлём ДО любого
	// другого app-сообщения, чтобы Keeper-sweep осиротевших dispatched-строк
	// произошёл до прихода RunResult-ов/TaskEvent-ов этого SID-а. Пустой набор
	// (рестарт процесса) — явная декларация «ничего не ведётся». Ошибка = разрыв
	// стрима: выходим, reconnect поднимет заново (как initial soulprint ниже).
	if err := sess.SendWardRoster(runner.ActiveSet()); err != nil {
		logger.Warn("ward-roster: send failed (stream broken)", slog.Any("error", err))
		return
	}

	// Первый SoulprintReport отправляется сразу при установке сессии (онбординг:
	// Keeper получает факты свежеподключённого хоста). Дальше — по тикеру
	// refresh_interval. Все Send идут из этого select-loop-а (через soulprintTick),
	// а не из горутины — StreamSession не concurrent-safe для Send (один writer).
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

	// Augur-клиент привязан к конкретной сессии (pending-map + Send в её stream,
	// ADR-025). Создаётся per-session: AugurRequest шлёт apply-горутина, ответ
	// reader-горутина доставляет напрямую в клиент (Deliver), минуя select-loop —
	// иначе deadlock: select-loop заблокирован в runner.Run, пока идёт apply, а
	// AugurReply пришёл бы через тот же recvCh, который читает заблокированный
	// loop. При выходе/swap клиент закрывается (ожидающие Fetch получают
	// ErrClientClosed). request_id уникален лишь per-stream — переживать
	// reconnect клиенту незачем.
	augurClient := augur.NewClient(sess)

	// reader-горутина читает текущую sess; при swap её перезапускают на
	// новой sess. Канал recvCh не буферизованный — gate, через который
	// reader сообщает результат каждого Recv-а; select-loop по нему диспетчит.
	// AugurReply reader доставляет напрямую в Augur-клиент (Deliver) и НЕ шлёт
	// в recvCh — корреляция request↔reply идёт через pending-map клиента, а не
	// через select-loop (тот в это время занят runner.Run).
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
							logger.Warn("augur: reply без ожидающего запроса (таймаут/отмена/неизвестный request_id)",
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
			// Тик пересборки фактов: собираем и шлём из этого loop-а (writer один).
			// Ошибка отправки = разрыв стрима — выходим, reconnect поднимет заново.
			if err := sp.pushOnce(ctx, sess); err != nil {
				logger.Warn("soulprint: report send failed (stream broken)", slog.Any("error", err))
				return
			}
		case portent := <-scheduler.Portents():
			// Beacon-scheduler поднял Portent на смену состояния (ADR-030,
			// edge-triggered). Шлём из этого select-loop-а — единственного writer-а
			// сессии (StreamSession не concurrent-safe для Send). Ошибка = разрыв
			// стрима: выходим, reconnect поднимет заново; событие уже извлечено из
			// канала и при разрыве теряется (как soulprint-тик) — следующая смена
			// State снова его поднимет.
			if err := sess.SendFromSoul(&keeperv1.FromSoul{
				Payload: &keeperv1.FromSoul_PortentEvent{PortentEvent: portent},
			}); err != nil {
				logger.Warn("beacon: portent send failed (stream broken)",
					slog.String("vigil", portent.GetBeaconName()),
					slog.Any("error", err))
				return
			}
		case newSess := <-swapCh:
			// Failback нашёл better priority — переключаемся: сначала закрываем
			// старый stream (это разморозит reader-Recv с err), потом ставим
			// новый и запускаем нового reader-а. До закрытия старого мы уже
			// держим новую сессию открытой → zero-downtime.
			oldSess := sess
			oldAugur := augurClient
			sess = newSess
			augurClient = augur.NewClient(sess)
			logger.Info("eventstream: failback swap",
				slog.Int("new_priority", newSess.Priority()),
				slog.String("session_id", newSess.SessionID()),
			)
			// Старый Augur-клиент закрываем (ожидающие Fetch на старой сессии
			// получают ErrClientClosed) — старый stream сейчас будет закрыт.
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
				// attempt-fencing-guard (ADR-027(g), Phase 2): отвергаем
				// stale-ApplyRequest (attempt < виденного для apply_id) — дубль
				// протухшего Ward, чей оригинальный apply уже принят. B1: отвергнутый
				// молча дропается (метрика + debug-лог внутри AcceptAttempt),
				// RunResult НЕ шлётся — барьер Keeper-а закроет оригинальный apply
				// (больший attempt) своим RunResult, runTimeout — нижняя страховка.
				if !runner.AcceptAttempt(req.GetApplyId(), req.GetAttempt()) {
					continue
				}
				// Извлекаем W3C traceparent из ApplyRequest в ctx, чтобы apply.run
				// внутри runner.Run поднялся как child span-а grpc.apply_dispatch
				// Keeper-а (сквозная трасса оператор → Keeper → Soul, ADR-024).
				// Пустой trace_context (старый Keeper без поля) → Extract no-op,
				// apply.run остаётся корнем (forward-compat деградация).
				applyCtx := otel.GetTextMapPropagator().Extract(ctx,
					propagation.MapCarrier{"traceparent": req.GetTraceContext()})
				// Augur-клиент + apply_id в ctx прогона: core.augur.fetch достанет
				// их через stream.Context() (общий SoulModule-контракт state+params
				// этого не выражает). delegate=false — данные приходят inline через
				// Keeper, root-credential (учётка внешней системы) на Soul не
				// попадает (ADR-025).
				applyCtx = augur.WithRun(applyCtx, augurClient, req.GetApplyId())
				// FetchModule-транспорт текущей сессии для core.module.installed
				// (ADR-065): та же ClientConn, отдельный HTTP/2-стрим; паттерн
				// augur.WithRun.
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
				// ADR-033 slice E5: оператор отменил in-flight Errand через
				// DELETE /v1/errands/{id} → Keeper отправил CancelErrand нам.
				// Best-effort: Errand мог уже завершиться (race с собственным
				// терминалом) — Cancel вернёт false, лог + молча игнор.
				cancelReq := payload.CancelErrand
				cancelled := errandRunner.Cancel(cancelReq.GetErrandId())
				logger.Info("errand: cancel received",
					slog.String("errand_id", cancelReq.GetErrandId()),
					slog.Bool("cancelled", cancelled),
				)
			case *keeperv1.FromKeeper_ErrandRequest:
				// ADR-033: pull-ad-hoc exec одиночного модуля. Run в отдельной
				// горутине, чтобы не блокировать recv-loop сессии (apply сейчас
				// блокирует loop — это его особенность из-за ADR-012(a) «один
				// in-flight apply»; Errand такой инвариант не несёт, может идти
				// параллельно apply-у и параллельно другим Errand-ам).
				//
				// Send результата делает та же горутина. StreamSession НЕ
				// concurrent-safe для Send: одновременная отправка ErrandResult
				// и TaskEvent/RunResult из apply-горутины может расщепить кадр.
				// Защита — через writeMu в StreamSession (см. send_mutex.go).
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
				// Полный active-набор допусков (ADR-026(h), Вариант A): применяем
				// как ReplaceAll — заменяем ВЕСЬ кеш этим набором. Отсутствующий в
				// snapshot допуск забывается → near-instant revoke (S6c) без
				// перезапуска Soul-а. Пустой snapshot = ни один плагин не допущен.
				// Snapshot — ЕДИНСТВЕННЫЙ авторитетный источник набора; кеш —
				// единый writer (этот recv-loop), читатели verify-фазы берут RLock.
				snap := payload.SigilSnapshot.GetSigils()
				sigils.ReplaceAll(snap)
				logger.Info("sigil: snapshot applied (ReplaceAll)",
					slog.Int("count", len(snap)),
				)
			case *keeperv1.FromKeeper_SigilTrustAnchors:
				// Полный набор trust-anchor-ов подписи Sigil (ADR-026(h), R3-S6):
				// ReplaceAll в atomic-holder Host-а. Набор переживает reconnect
				// (holder создан вне reconnect-loop), но новый broadcast при каждом
				// подключении/ротации приводит его к актуальному состоянию: якорь вне
				// нового набора «забывается» (retired-ключ перестаёт верифицировать
				// допуски), новый primary становится доверенным near-instant.
				//
				// fail-closed на битом наборе: если хоть один PEM не парсится, НЕ
				// подменяем holder (оставляем прежний валидный набор), warn — иначе
				// мусорный/неполный набор открыл бы дыру в verify. Пустой набор
				// (Sigil выключен на Keeper) — валидное состояние: holder стирается,
				// verify любого плагина fail-closed по no_trust_anchor.
				if anchors == nil {
					// holder отсутствует (например push-режим / тестовая обвязка без
					// Host) — раздавать некуда; verify и так fail-closed.
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
				// Одиночный PluginSigil (ADR-026(h), Вариант A) — broadcast-
				// уведомление о новом допуске, НЕ мутация набора. Авторитет набора
				// — только SigilSnapshot, поэтому здесь НЕ делаем upsert: иначе
				// потеря/дубль одиночного сообщения исказила бы кеш, а revoke не
				// сработал бы (одиночное сообщение не умеет «забыть» допуск).
				// Набор восстановится из ближайшего snapshot-а.
				sig := payload.PluginSigil
				logger.Debug("sigil: single notification (set unchanged, snapshot is authoritative)",
					slog.String("namespace", sig.GetNamespace()),
					slog.String("name", sig.GetName()),
					slog.String("ref", sig.GetRef()),
				)
			case *keeperv1.FromKeeper_VigilSnapshot:
				// Полный active-набор Vigil (ADR-030, ReplaceAll — паттерн
				// SigilSnapshot): scheduler заменяет весь локальный набор. Vigil вне
				// snapshot останавливается и забывается (disable/удаление без
				// перезапуска Soul-а), новый — стартует с baseline без Portent. ctx —
				// родительский demon-context: набор переживает текущую сессию.
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

// failbackLoop — периодически пытается установить новый stream на priority <
// currentPriority. При успехе шлёт новую сессию в swapCh и завершается
// (handleSession перезапустит loop с новым priority). При currentPriority=1
// или отсутствии higher-priority endpoint-ов — тихо завершается (некуда
// возвращаться).
//
// Интервал — fb.interval ± fb.spray (равномерный jitter), [docs/soul/connection.md
// → Failback]. spray не растягивает interval, защищает только от стадного
// эффекта.
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
				// Конфиг изменился через hot-reload? Безопасно завершаем —
				// handleSession при следующей сессии перезапустит loop с
				// актуальным priority.
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

// failbackInterval — fb.interval ± fb.spray равномерно. spray=0 → ровно
// interval. Гарантирует d > 0 (отрицательный interval clamping в interval/2).
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

// backoffParams — экспоненциальный backoff между reconnect-попытками
// (soul.yml::keeper.retry.backoff). Defaults: 1s → 30s, jitter on.
type backoffParams struct {
	initial time.Duration
	max     time.Duration
	jitter  bool
}

// failbackParams — параметры proactive-возврата на более предпочтительный
// приоритет (soul.yml::keeper.failback). Defaults: enabled=true, interval=1h,
// spray=10m ([docs/soul/connection.md → Параметры]).
type failbackParams struct {
	enabled  bool
	interval time.Duration
	spray    time.Duration
}

// soulprintReportSink — узкая поверхность StreamSession для отправки
// SoulprintReport. Выделена ради тестируемости soulprintPusher без живого gRPC.
type soulprintReportSink interface {
	SendSoulprintReport(*keeperv1.SoulprintReport) error
}

// soulprintPusher — сбор + периодическая отправка SoulprintReport (M2.3,
// ADR-018). Stateless относительно сессии: pushOnce принимает sink текущей
// сессии, тикер только сигналит select-loop-у handleSession (он же writer).
type soulprintPusher struct {
	collector *soulprint.Collector
	sid       string
	interval  time.Duration
}

// pushOnce собирает факты хоста и отправляет один SoulprintReport в sink.
// Сбор быстрый (чтение /proc / net), идёт в вызывающей горутине (select-loop).
func (p soulprintPusher) pushOnce(ctx context.Context, sink soulprintReportSink) error {
	return sink.SendSoulprintReport(p.collector.Collect(ctx, p.sid))
}

// hostFactsFromSoulprint извлекает узкий backend-снимок (pkg-mgr / init-система)
// из собранного SoulprintReport для инжекта в core-модули (Вариант A, ADR-018(b)).
// nil/sparse-отчёт (factless хост, частичный сбор) → пустой HostFacts: core-модули
// откатятся на runtime-детект. Значения строк os.pkg_mgr/os.init_system совпадают
// с closed-set util.PkgMgr/util.InitSystem (ADR-018 — единый словарь); неизвестное
// значение трактуется детекторами как unknown через ResolvePkgMgr/ResolveInitSystem.
func hostFactsFromSoulprint(rep *keeperv1.SoulprintReport) coremodutil.HostFacts {
	os := rep.GetTypedFacts().GetOs()
	return coremodutil.HostFacts{
		PkgMgr:     coremodutil.PkgMgr(os.GetPkgMgr()),
		InitSystem: coremodutil.InitSystem(os.GetInitSystem()),
	}
}

// startTicker запускает горутину, которая каждые interval шлёт сигнал в tick.
// Канал tick должен быть буферизован на 1 — если select-loop занят (apply
// in-flight), тик не теряется, но и не копится (coalescing: один отложенный
// тик достаточен). Возвращает stop-функцию для остановки горутины.
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
				default: // предыдущий тик ещё не обработан — пропускаем (coalescing)
				}
			}
		}
	}()
	return cancel
}

// loadSoulprintInterval — soul.yml::soulprint.refresh_interval. Default 5m
// (docs/soul/config.md). Отсутствие блока → дефолт.
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

// resolveMaxAttempts — soul.yml::keeper.retry.max_attempts с резолвом 0→2
// (симметрично дефолту в soulgrpc.NewClient). Опущенный блок retry / нулевое
// значение → defaultClientMaxAttempts. Валидация (>=1) уже сделана на
// config-фазе (shared/config/schema.go), здесь только резолв дефолта.
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

// withJitter добавляет ±25% случайного разброса. Используется при отсутствии
// дедупликации reconnect-ов (thundering herd при общем сбое Keeper-cluster-а).
func withJitter(d time.Duration, enabled bool) time.Duration {
	if !enabled || d <= 0 {
		return d
	}
	delta := d / 4
	return d + time.Duration(rand.Int64N(int64(delta*2))) - delta
}

// sleepCtx ждёт d или ctx-cancel. Возвращает true, если время вышло
// (можно продолжать), false — если ctx отменён (выходить).
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

// orderedByPriority возвращает endpoints, упорядоченные по priority (меньше
// → раньше), без мутации исходного slice. priority=0 нормализуется в 1
// (default = высший), как в EventStream-клиенте (internal/grpc). Используется
// bootstrap-фазой: failback к ней неприменим (one-shot), но порядок перебора
// тот же. См. docs/soul/connection.md.
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

// loadSoulConfig читает soul.yml + валидирует. Возвращает первую error-фазу
// в виде понятного сообщения. Поля logging (включая logging.file /
// logging.rotation.max_age_days) теперь нормативны в shared-схеме — отдельного
// overlay-прохода больше нет.
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

// soulOTelEndpoint извлекает endpoint из опц. otel-блока (пустая строка,
// если блок не задан) — для obs.OTelConfig без nil-разыменования.
func soulOTelEndpoint(o *config.SoulOTel) string {
	if o == nil {
		return ""
	}
	return o.Endpoint
}

// resolveSoulMetricsBasicAuth собирает *obs.BasicAuth для soul-`/metrics`.
//
// Возвращает (nil, nil), если basic-auth не настроен/выключен — listener
// поднимается без auth (защита — loopback-bind). При enabled читает пароль
// из файла на диске (у Soul нет vault-клиента, ADR-012): одна строка,
// trailing-whitespace/newline отбрасывается. Пустой файл — ошибка
// (fail-fast: лучше упасть на старте, чем поднять `/metrics` с пустым
// паролем). Содержимое файла в лог/ошибку не попадает.
func resolveSoulMetricsBasicAuth(b *config.SoulMetricsBasicAuth) (*obs.BasicAuth, error) {
	if b == nil || !b.Enabled {
		return nil, nil
	}
	raw, err := os.ReadFile(b.PasswordFile)
	if err != nil {
		// b.PasswordFile — путь (не секрет), эхаем его для диагностики.
		return nil, fmt.Errorf("read metrics.basic_auth.password_file %q: %w", b.PasswordFile, err)
	}
	pass := strings.TrimRight(string(raw), "\r\n")
	if pass == "" {
		return nil, fmt.Errorf("metrics.basic_auth.password_file %q is empty", b.PasswordFile)
	}
	return &obs.BasicAuth{Username: b.Username, Password: pass}, nil
}

// signalContext — context, отменяемый по SIGINT/SIGTERM. SIGHUP сюда НЕ
// входит — он обрабатывается отдельным каналом в [config.WatchSIGHUP], чтобы
// reload не путался с shutdown.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
