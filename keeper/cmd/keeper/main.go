// Package main — entrypoint бинаря `keeper` под ADR-004 / ADR-011 / ADR-013.
//
// Subcommand-router на stdlib `flag`:
//
//	keeper init    --archon=<aid> [--config=<path>] [--credential-out=<path>] [--display-name=<name>]
//	keeper run     [--config=<path>] [--initialize]
//	keeper version
//	keeper help
//
// `init` — bootstrap первого Архонта (ADR-013), вызывает `internal/bootstrap.Init`.
// `run` — daemon-loop; M0.5c-stub: только load config, apply migrations,
// check `operators` registry, wait-for-signal. Реальный gRPC/HTTP server
// добавится в M0.6+.
//
// Архитектурно команды независимые: каждая делает свой context+зависимости,
// не делит globals — keeper-process всегда выполняет ровно одну
// subcommand-у и завершается.
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

// version — версия бинаря `keeper`, печатается командой `keeper version`, чтобы
// оператор видел версию центрального узла (DoD беты). Значение по умолчанию —
// для `go run`/IDE-сборок; в релизных сборках перезаписывается линкером через
// `-ldflags "-X ...version=<ver>"` (см. Makefile, переменная VERSION; симметрия
// с soulVersion/soulctlVersion). Именно `var`, а не `const`, потому что `-X`
// умеет инжектить только в package-level string-переменные.
var version = "0.0.0-dev"

// Exit-коды:
//
//	0 — успех.
//	1 — runtime-error (PG/Vault недоступны, уже initialized, и т.п.).
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

// printVersion печатает версию keeper-бинаря и Go-runtime. Формат:
//
//	keeper <version> (go<goversion>)
//
// `version` инжектится линкером через `-ldflags -X` (см. Makefile, VERSION);
// в `git describe`-варианте уже содержит ближайший тег + commit + -dirty,
// поэтому отдельный git-commit ldflag не вводим (симметрия с soul/soulctl).
func printVersion(w *os.File) {
	fmt.Fprintf(w, "keeper %s (%s)\n", version, runtime.Version())
}

// runInit парсит флаги, поднимает зависимости (PG pool, Vault client,
// audit writer), вызывает bootstrap.Init и печатает финальное сообщение.
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
		// flag сам печатает usage в ContinueOnError-режиме.
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
	// Logger строится после успешной загрузки/валидации cfg, чтобы читать
	// logging-ротацию из keeper.yml (ранние config-ошибки выше уходят в
	// stderr напрямую, не через logger).
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

	// Vault-client поднимается до pg.NewPool — `postgres.dsn_ref`
	// может быть vault-ref (`vault:secret/keeper/postgres`), резолв
	// которого требует non-nil vc.
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

	// Apply migrations — идемпотентно (см. migrate.Apply).
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
			// Insert операторa уже закоммичен, но audit-event не
			// записан. БД консистентна, audit_log — нет.
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
			// Insert + audit committed; JWT-файл не сохранён. Печатаем
			// токен в stderr с предупреждением — secrecy уже нарушена
			// (логи могут уехать в OTel/файл/journald), оператор обязан
			// ротировать токен. Альтернативы (lose token = lockout)
			// хуже: без cluster-admin JWT operator API недоступен.
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
// Делает: load config, NewPool + Ping, Apply migrations, Count operators,
// Build JWT Verifier/Issuer (signing key из Vault), Build RBAC enforcer
// из БД-снимка через rbac.NewHolder (ADR-028, config-RBAC удалён), Start
// Operator API HTTP-сервер с RBAC + audit middleware на /v1/operators.
//
// Без --initialize и пустого реестра → exit 1 с подсказкой запустить
// `keeper init`. С --initialize и пустым реестром / непустым реестром →
// поднимает HTTP listener и блокируется до SIGINT/SIGTERM.
//
// gRPC (Keeper↔Soul) и MCP listenerы — M0.7+.
func runDaemon(args []string) int {
	ctx, cancel := signalContext()
	defer cancel()

	d := &daemon{cleanups: &cleanupStack{}}
	// ЕДИНСТВЕННАЯ точка graceful shutdown: LIFO-drain cleanup-стека
	// воспроизводит порядок прежних defer-ов runDaemon один-в-один (см.
	// daemon.go). Каждый setupX регистрирует свои teardown-ы через
	// d.cleanups.push в том же порядке, в каком прежде шли defer-ы.
	defer d.cleanups.runLIFO()

	// setupConfig — особая exit-семантика (флаг-парс → exitUsage), поэтому
	// вызывается отдельно от единого паттерна остальных шагов.
	if code, ok := d.setupConfig(args); !ok {
		return code
	}

	// Строгий порядок init (см. §4 инвариантов в daemon.go). Каждый шаг при
	// ошибке уже напечатал stderr и вернул errSetupFailed — оркестратор лишь
	// маппит в exitError, не печатая повторно. Порядок шагов критичен и
	// менять его нельзя без архитектурного аудита.
	steps := []func(context.Context) error{
		d.setupObservabilityEarly,
		d.setupVault,
		d.setupStorage,
		// setupSigil переставлен сюда (выше setupGRPCBootstrap): bootstrap-сервер
		// читает d.sigilPubKeyPEM (trust-anchor Soul-у, ADR-026 S6). Его deps —
		// d.vc (setupVault) + d.pool (setupStorage) — уже готовы; ничего, что
		// создаётся между storage и bootstrap, ему не нужно.
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
		// setupPushOrchestrator — prepare-фаза Variant C orchestrator-а
		// (docs/keeper/push.md): создание pushDestinyLoader. Финал —
		// finalizePushOrchestrator после setupGRPCEventStream (topologyResolver).
		d.setupPushOrchestrator,
		// setupPushDispatchers — pilot wire-up SshDispatcher (S6, 2026-05-26):
		// host-CA из Vault + ConfigTargetResolver + Spawn первого SshProvider-
		// плагина + SshDispatcher. Зависит от setupCoreModules (pushPluginHost +
		// pushDiscoveredSsh) и setupVault (d.vc). При отсутствии push-блока /
		// ssh_providers — no-op, push выключен. Lifecycle plugin-handle —
		// cleanup-стек (LIFO ДО Redis/Pool).
		d.setupPushDispatchers,
		d.setupGRPCBootstrap,
		d.setupRedis,
		// setupHeraldDelivery — claim-queue worker-ы webhook-доставки уведомлений
		// (ADR-052(d), S3). Late-binding подменяет fallback-LogDeliveryQueue
		// dispatcher-а (собран в setupAudit ДО Redis) на RedisDeliveryQueue +
		// поднимает worker-ы + mini-reaper. После setupRedis (redisClient),
		// setupAudit (dispatcher/auditWriter), setupMetricsRegistry
		// (heraldDeliveryMetrics), setupVault (d.vc для secret_ref). Fail-open: без
		// Redis доставка деградирует, keeper не падает.
		d.setupHeraldDelivery,
		// setupPushProviderSvc — поднимает CRUD-фасад push_providers
		// (ADR-032 amendment 2026-05-26, S7-2) + Redis-publisher для cluster-
		// wide invalidate. После setupRedis (нужен d.redisClient для
		// publisher); ДО setupAPIServer / setupMCPServer (api.Deps и
		// HandlerDeps читают d.pushProviderSvc).
		d.setupPushProviderSvc,
		// setupHeraldSvc — CRUD-фасад реестров heralds/tidings (ADR-052, S4) +
		// двухуровневая инвалидация снимка Tiding-правил dispatcher-а (in-process
		// heraldDispatcher + cross-keeper Redis `herald:invalidate`). ПОСЛЕ
		// setupAudit (d.heraldDispatcher) и setupRedis (publisher); ДО
		// setupAPIServer / setupMCPServer (api.Deps и HandlerDeps читают d.heraldSvc).
		d.setupHeraldSvc,
		// runLegacyAutoImport — opt-in one-shot миграция inline
		// `keeper.yml::push.targets[]` / `push.providers[]` в PG-источники
		// (ADR-032 amendment 2026-05-26, S7-4). При оба флага false → no-op.
		// После setupPushProviderSvc (нужен подтверждённый PG-state +
		// d.auditWriter); ДО setupAPIServer (REST `/v1/push-providers` сразу
		// видит импортированные строки).
		d.runLegacyAutoImport,
		d.setupConclave,
		// Refuse-guard soul-shedding (Finding-A, ADR-027(h)): СРАЗУ после
		// setupConclave (нужна собственная presence-запись в CountLive) и ДО
		// EventStream/Acolyte — отказ старта при multi-keeper + acolytes=0.
		d.setupConclaveRefuseGuard,
		d.setupRBACInvalidation,
		d.setupServiceRegistryInvalidation,
		// setupToll — cluster-wide detector массового оттока (ADR-038). ДО
		// setupGRPCEventStream, чтобы EventStreamDeps мог получить TollNotifier
		// (d.tollWatcher); ПОСЛЕ setupRedis (нужен d.redisClient). При выключенном
		// Toll (нет Redis или `toll.enabled: false`) — no-op-watcher / noop-reader,
		// EventStream-hook остаётся no-op (nil TollNotifier).
		d.setupToll,
		// setupTempo — per-AID rate-limiter write-API (Tempo, ADR-050). ПОСЛЕ
		// setupRedis (limiter живёт в Redis; без Redis → nil → middleware
		// passthrough), ДО setupAPIServer (api.Deps читает d.tempoLimiter).
		d.setupTempo,
		// setupLoginGuard — anti-bruteforce-лимитер публичных login-эндпоинтов
		// (ADR-058(g), HIGH-3). ПОСЛЕ setupRedis (lockout cluster-shared в Redis;
		// без Redis → nil → login без throttle), ДО setupAPIServer (api.Deps
		// читает d.loginGuard).
		d.setupLoginGuard,
		d.setupGRPCEventStream,
		// finalizePushOrchestrator — собирает *pushorch.PushRun после поднятия
		// topologyResolver (setupGRPCEventStream). Идёт ДО setupAPIServer/
		// setupMCPServer, чтобы они увидели d.pushRun != nil.
		d.finalizePushOrchestrator,
		// setupErrandDispatcher — pull-ad-hoc Errand contour (ADR-033). Зависит от
		// d.outbound + d.applyBus + d.pool (setupGRPCEventStream + setupRedis уже
		// отработали); ДО setupAPIServer (api.Deps читает errandDispatcher/Store).
		// Включает однократный Replay осиротевших running-Errand-ов этого KID.
		d.setupErrandDispatcher,
		d.setupWatchman,
		d.setupSigilInvalidation,
		d.setupAPIServer,
		// setupOperatorInvalidation — JWT immediate revoke (ADR-014 Amendment
		// 2026-05-27): подключает operator.Service к тому же `rbac:invalidate`
		// pub/sub, что и role-мутации. Идёт ПОСЛЕ setupAPIServer (operator.Service
		// создаётся внутри NewServer → доступен через apiServer.OperatorService()).
		// При redisClient==nil — no-op (single-Keeper/dev: чистый TTL-poll).
		d.setupOperatorInvalidation,
		d.setupMCPServer,
		d.setupAcolyte,
		// setupVoyageWorker — pool VoyageWorker-ов (ADR-043, S1). Зависит от
		// d.pool + scenarioRunner/serviceRegistry/errandDispatcher (production
		// DI-адаптеры). config-gated OFF по умолчанию: поднимается лишь при
		// voyage.workers > 0; dev-конфиг без блока voyage не меняет поведение.
		// До setupReaper (Reaper-правило reclaim_voyages — пост-S1).
		d.setupVoyageWorker,
		d.setupReaper,
		// setupConductor — leader-elected исполнитель Cadence (ADR-048). Свой
		// lease conductor:leader, независимый от reaper. Default-ON при наличии
		// Redis (footgun-guard ADR-048 §5). После setupReaper: в C4 Reaper
		// перестал спавнить Cadence, Conductor начал — atomic switchover.
		d.setupConductor,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return exitError
		}
	}

	// srv.Start блокируется до ctx.Done() (signal) или fatal Serve-ошибки.
	// На signal делает graceful shutdown внутри. Все ошибки маппятся в
	// exit-code 1. Самый последний шаг — вне «setup» (блокирующий main loop).
	if err := d.apiServer.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: HTTP server: %v\n", err)
		return exitError
	}

	d.logger.Info("keeper run: shutdown complete")
	return exitOK
}

// poolPinger — адаптер pgxpool.Pool к интерфейсу health.Pinger.
// Декларируется в main, чтобы api-пакет не тянул pgx как direct dep
// (health.Pinger — общий интерфейс с `Ping(ctx) error`).
type poolPinger struct{ pool pgxPool }

func (p poolPinger) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// pgxPool — узкий интерфейс над `*pgxpool.Pool` (только Ping). Позволяет
// держать api-пакет независимым от pgxpool, а main-пакет — от api-impl.
type pgxPool interface {
	Ping(ctx context.Context) error
}

// parseTTL парсит duration-строку из `keeper.yml::auth.jwt.<field>`
// (Go-syntax: "720h", "24h", "30m"). Пустая строка → def. fieldName
// используется в сообщении об ошибке для контекста.
//
// Convention `<N>d` (через [config.ParseDuration]) здесь сознательно НЕ
// используется — auth.jwt.ttl_* в config-schema объявлены как Go-duration
// (не «duration с днями»). Если convention расширится — заменить на
// config.ParseDuration в одной точке.
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

// envTruthy читает env-переменную как boolean-флаг через [strconv.ParseBool]
// (принимает 1/t/T/true/TRUE и т.п.). Пустая или невалидная строка → false:
// env-override не должен «случайно» включать режим из-за опечатки/мусора.
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

// guardOperatorsRegistry — чистое РЕШЕНИЕ restart-семантики ADR-013(d) по
// состоянию реестра `operators`: пустой реестр без явного `initialize` →
// отказ старта; пустой с `initialize` → bootstrap-pending; непустой → ready.
//
// Без I/O (БД/логгер/os) — вся приёмка зависимостей и печать остаются в
// runDaemon. refuseMsg возвращается только при proceed=false.
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

// conclaveSinglePathDecision — исход refuse-guard-а soul-shedding (Finding-A,
// ADR-027(h)): что делать при `acolytes == 0` в окружении с числом живых
// Keeper-инстансов `liveCount`.
type conclaveSinglePathDecision int

const (
	// conclaveSinglePathOK — конфигурация безопасна (acolytes>0 ЛИБО единственный
	// живой инстанс): старт без замечаний.
	conclaveSinglePathOK conclaveSinglePathDecision = iota
	// conclaveSinglePathRefuse — multi-keeper + acolytes=0 без opt-out: отказ старта.
	conclaveSinglePathRefuse
	// conclaveSinglePathWarn — то же опасное сочетание, но с явным opt-out: громкий
	// WARN, старт продолжается (осознанный выбор оператора).
	conclaveSinglePathWarn
)

// decideConclaveSinglePath — чистое РЕШЕНИЕ refuse-guard-а soul-shedding
// (Finding-A, ADR-027(h)) по числу живых Keeper-инстансов в Conclave.
//
// Опасна ровно одна конфигурация: `acolytes == 0` (run-goroutine-путь,
// single-keeper-only) ПРИ наличии ДРУГИХ живых инстансов (`liveCount > 1` —
// в счёт входит и собственная только что зарегистрированная presence-запись,
// поэтому порог именно «> 1», а не «>= 1»). В ней apply на Keeper-A c Soul-ом
// на стриме Keeper-B навсегда зависает в `applying` (cross-keeper barrier).
//
//   - acolytes > 0           → OK (work-queue ADR-027, cross-keeper-зависания нет);
//   - liveCount <= 1         → OK (единственный инстанс — run-goroutine-путь штатен);
//   - иначе без opt-out      → Refuse (дефолт, безопасно);
//   - иначе с allowUnsafe    → Warn (явный opt-out оператора).
//
// Без I/O (Conclave-count и печать остаются в setupX). liveCount резолвится
// caller-ом из Conclave.CountLive (см. setupConclaveRefuseGuard).
func decideConclaveSinglePath(acolytes, liveCount int, allowUnsafe bool) conclaveSinglePathDecision {
	if acolytes > 0 || liveCount <= 1 {
		return conclaveSinglePathOK
	}
	if allowUnsafe {
		return conclaveSinglePathWarn
	}
	return conclaveSinglePathRefuse
}

// conclaveRefuseMessage формирует operator-facing stderr-сообщение refuse-а
// (Finding-A): что обнаружено, почему опасно, как починить. liveCount — число
// живых инстансов в Conclave (включая собственный).
func conclaveRefuseMessage(liveCount int) string {
	return fmt.Sprintf("keeper run: multi-keeper обнаружен (%d живых Keeper-инстансов в Conclave) при keeper.acolytes=0 — refusing to start.\n"+
		"        Run-goroutine-путь (acolytes: 0) — single-keeper-only: apply на одном Keeper-е c Soul-ом\n"+
		"        на стриме другого навсегда зависнет в applying (ADR-027). Выставьте keeper.acolytes>0\n"+
		"        для HA-кластера, либо keeper.allow_unsafe_single_path_multi_keeper: true\n"+
		"        (env KEEPER_ALLOW_UNSAFE_MULTI_KEEPER=true) — осознанный single-keeper-за-LB opt-out.", liveCount)
}

// metricsPasswordField — имя поля в Vault KV, из которого
// [resolveMetricsBasicAuth] достаёт пароль basic-auth (симметрично
// `signing_key` для JWT signing-key, bootstrap.extractSigningKey).
const metricsPasswordField = "password"

// resolveMetricsBasicAuth собирает *obs.BasicAuth для metrics-listener-а.
//
// Возвращает (nil, nil), если basic-auth не настроен/выключен — listener
// поднимается без auth. При enabled резолвит пароль из vault по password_ref
// тем же keeper-vault-клиентом, что читает signing-key (ADR-011: shared/obs
// не тянет vault; резолв — на keeper-стороне). Ни password_ref, ни сам
// пароль не логируются (см. PM-decision Slice 1 #5).
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
		// b.PasswordRef — vault-ref (не секрет), но в сообщение его не кладём,
		// чтобы случайный plaintext-ref не утёк в лог; ParseRef уже эхает форму.
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

// otelEndpoint извлекает endpoint из опц. otel-блока (пустая строка, если
// блок не задан) — для obs.OTelConfig без nil-разыменования.
func otelEndpoint(o *config.KeeperOTel) string {
	if o == nil {
		return ""
	}
	return o.Endpoint
}

// issuerFactory возвращает фабрику для bootstrap.Config.IssuerFactory.
// Тесты mock-ают саму фабрику (без подгрузки signing-key).
func issuerFactory(issuerName string) func(signingKey []byte) (bootstrap.JWTIssuer, error) {
	return func(signingKey []byte) (bootstrap.JWTIssuer, error) {
		return keeperjwt.NewIssuer(signingKey, issuerName)
	}
}

// pluginCacheRoot — путь к директории-кешу Keeper-side плагинов.
//
// Приоритет источников (от высшего к низшему):
//  1. `keeper.yml::plugins.cache_root` (нормальный, прод-источник).
//  2. env `KEEPER_PLUGIN_CACHE_DIR` — dev/CI-override, оставлен для удобства
//     локальных прогонов без правки YAML.
//  3. [pluginhost.DefaultCacheRoot] — встроенный default.
//
// Абсолютность пути в (1) гарантируется schema-фазой ([config.schemaValidateKeeper]);
// env-override (2) — ответственность оператора (dev-only).
func pluginCacheRoot(p *config.KeeperPlugins) string {
	if p != nil && p.CacheRoot != "" {
		return p.CacheRoot
	}
	if v := os.Getenv("KEEPER_PLUGIN_CACHE_DIR"); v != "" {
		return v
	}
	return pluginhost.DefaultCacheRoot
}

// defaultPluginWorkRoot — корень рабочих git-клонов резолвера плагинов
// (plugingit.Resolver, ADR-026 F-fetch, A1-S1). СТРОГО вне cache-root: .git и
// checkout не должны попадать в кеш-слоты, читаемые Discover/ReadSlot.
const defaultPluginWorkRoot = "/var/lib/soul-stack-keeper/plugin-src"

// pluginWorkRoot — путь к корню рабочих клонов резолвера. Приоритет:
//  1. `keeper.yml::plugins.work_root` (абсолютность гарантируется schema-фазой);
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

// defaultServiceCacheRoot — корень кеша git-снапшотов service-репозиториев
// (artifact.ServiceLoader). Параллель с pluginhost.DefaultCacheRoot.
const defaultServiceCacheRoot = "/var/lib/soul-stack-keeper/services"

// serviceCacheRoot — путь к кешу git-артефактов service-репо.
//
// Приоритет: env `KEEPER_SERVICE_CACHE_DIR` (dev/CI-override) →
// [defaultServiceCacheRoot]. Отдельного `keeper.yml`-поля пока нет — это
// runtime-путь, не часть конфиг-контракта (добавить config-field — отдельная
// задача, см. observations в delegation).
func serviceCacheRoot(_ *config.KeeperConfig) string {
	if v := os.Getenv("KEEPER_SERVICE_CACHE_DIR"); v != "" {
		return v
	}
	return defaultServiceCacheRoot
}

// defaultDestinyCacheRoot — корень кеша git-снапшотов destiny-репозиториев
// (artifact.DestinyLoader). Параллель с [defaultServiceCacheRoot].
const defaultDestinyCacheRoot = "/var/lib/soul-stack-keeper/destiny"

// destinyCacheRoot — путь к кешу git-артефактов destiny-репо. См.
// [serviceCacheRoot]: env `KEEPER_DESTINY_CACHE_DIR` → [defaultDestinyCacheRoot].
func destinyCacheRoot(_ *config.KeeperConfig) string {
	if v := os.Getenv("KEEPER_DESTINY_CACHE_DIR"); v != "" {
		return v
	}
	return defaultDestinyCacheRoot
}

// acolyteLease — TTL Ward-захвата planned-задания Acolyte-ом (ADR-027,
// claim_expires_at = NOW()+lease). Просроченный Ward переклеймит recovery-скан
// (Phase 2). Берётся из `keeper.acolyte_lease`; пусто/некорректно →
// [config.DefaultAcolyteLease]. Формат строки уже провалидирован semantic-фазой
// (checkDuration), но дефолтуем на любой не-положительный результат на всякий.
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

// acolyteBatch — максимум planned-заданий, захватываемых одним claim-тиком
// (LIMIT claim-запроса). Воркеры разных инстансов делят очередь через
// FOR UPDATE SKIP LOCKED — батч лишь ограничивает аппетит одного тика. Берётся
// из `keeper.acolyte_batch`; 0/опущено → [config.DefaultAcolyteBatch].
func acolyteBatch(cfg *config.KeeperConfig) int {
	if cfg.AcolyteBatch <= 0 {
		return config.DefaultAcolyteBatch
	}
	return cfg.AcolyteBatch
}

// acolytePollInterval — период poll-fallback-а воркера (ADR-027(a)). Берётся из
// `keeper.acolyte_poll_interval`; пусто/некорректно → [config.DefaultAcolytePollInterval].
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

// acolyteDrainGrace — окно graceful-drain пула при остановке Keeper
// (graceful-drain пула Acolyte, ADR-027 Phase 2). Берётся из `keeper.acolyte_drain_grace`;
// пусто/некорректно → [config.DefaultAcolyteDrainGrace].
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

// voyageWorkers — число воркеров VoyageWorker-пула (ADR-043, S1). Берётся из
// `keeper.voyage.workers`. Config-gated OFF по умолчанию: отсутствие блока ИЛИ
// workers ≤ 0 → 0 (pool НЕ поднимается). Воркер стартует только при явном
// `voyage.workers: N > 0`.
func voyageWorkers(cfg *config.KeeperConfig) int {
	if cfg.Voyage == nil || cfg.Voyage.Workers <= 0 {
		return 0
	}
	return cfg.Voyage.Workers
}

// voyageLeaseTTL — TTL PG-claim-lease для строки в `voyages` (ADR-043). Берётся
// из `keeper.voyage.lease_ttl`; пусто/некорректно → [config.DefaultVoyageLeaseTTL]
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

// voyageLeaseRenewInterval — период renewal-CAS-UPDATE-а текущего lease-а.
// Берётся из `keeper.voyage.lease_renew_interval`; пусто/некорректно →
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

// voyagePollInterval — период idle-poll claim-loop-а. Берётся из
// `keeper.voyage.poll_interval`; пусто/некорректно →
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

// sigilAnchorsReloadInterval — период TTL-fallback-перечита набора trust-anchor-
// ключей подписи Sigil (ADR-026(h), R3 known-gap). Берётся из
// `keeper.sigil_anchors_reload_interval`; пусто/некорректно →
// [config.DefaultSigilAnchorsReloadInterval] (30s). Формат уже провалидирован
// semantic-фазой (checkDuration); дефолтуем на любой не-положительный результат
// на всякий случай (симметрия с резолверами acolyte_*).
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

// oracleCircuitMaxFires — эффективный порог circuit-breaker-а Oracle (ADR-030(a),
// beacons S4): сколько срабатываний Decree за окно допустимо до авто-disable.
// Берётся из `keeper.oracle_circuit_max_fires` (*int): nil (поле опущено) →
// дефолт [config.DefaultOracleCircuitMaxFires] (5); явный 0 → breaker OFF
// (escape-hatch), возвращаем 0 как есть. Отрицательное отсечено schema-фазой.
// *int нужен именно чтобы различить «пусто → дефолт 5» и «явный 0 → off».
func oracleCircuitMaxFires(cfg *config.KeeperConfig) int {
	if cfg.OracleCircuitMaxFires == nil {
		return config.DefaultOracleCircuitMaxFires
	}
	return *cfg.OracleCircuitMaxFires
}

// oracleCircuitWindow — длина fixed-window circuit-breaker-а Oracle (ADR-030(a)).
// Берётся из `keeper.oracle_circuit_window`; пусто/некорректно →
// [config.DefaultOracleCircuitWindow] (10m). Формат уже провалидирован
// semantic-фазой (checkDuration); дефолтуем на любой не-положительный результат
// (симметрия с резолверами acolyte_*).
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

// watchmanInterval — период probe-тика Watchman (изоляция-детект +
// soul-shedding S2). Берётся из `keeper.watchman_interval`; пусто/некорректно →
// [config.DefaultWatchmanInterval] (5s). Формат уже провалидирован semantic-фазой
// (checkDuration); дефолтуем на любой не-положительный результат (симметрия с
// резолверами acolyte_* / oracle_circuit_window).
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

// watchmanFailThreshold — число подряд идущих провалов probe Watchman до
// shedding-а (debounce/flap-guard). Берётся из `keeper.watchman_fail_threshold`;
// 0/опущено → [config.DefaultWatchmanFailThreshold] (3). Отрицательное отсечено
// schema-фазой (симметрия с acolyteBatch).
func watchmanFailThreshold(cfg *config.KeeperConfig) int {
	if cfg.WatchmanFailThreshold <= 0 {
		return config.DefaultWatchmanFailThreshold
	}
	return cfg.WatchmanFailThreshold
}

// summonsPublisher — адаптер [scenario.SummonsPublisher] поверх
// [keeperredis.PublishSummons]. Best-effort: scenario-runner на новом пути
// dispatch-а зовёт его после записи planned-строк; ошибку публикации он
// логирует и глотает (задания персистентны, poll-fallback Acolyte подхватит).
type summonsPublisher struct {
	redis *keeperredis.Client
	kid   string
}

func (p summonsPublisher) PublishSummons(ctx context.Context) error {
	_, err := keeperredis.PublishSummons(ctx, p.redis, p.kid)
	return err
}

// rbacInvalidatePublishTimeout — deadline на сетевой Redis PUBLISH
// invalidate-сигнала (B2): если Redis недоступен, role-мутация не блокируется
// дольше этого. Симметрично applybus.clusterPublishTimeout.
const rbacInvalidatePublishTimeout = time.Second

// rbacInvalidator — адаптер [rbac.Invalidator] поверх
// [keeperredis.PublishRBACInvalidate]. Best-effort: ошибку публикации
// логирует и глотает (мутация уже зафиксирована в БД, TTL-poll подхватит).
type rbacInvalidator struct {
	redis  *keeperredis.Client
	kid    string
	logger *slog.Logger
}

func (i rbacInvalidator) Invalidate(_ context.Context) {
	// Свой короткий deadline вместо ctx caller-а: PUBLISH не должен
	// блокировать ответ на role-мутацию при недоступном Redis.
	ctx, cancel := context.WithTimeout(context.Background(), rbacInvalidatePublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishRBACInvalidate(ctx, i.redis, i.kid); err != nil {
		i.logger.Warn("rbac: cluster-invalidate publish failed", slog.Any("error", err))
	}
}

// rbacInvalidationSource — адаптер [rbac.InvalidationSource] поверх
// [keeperredis.SubscribeRBACInvalidate]. Watch держит подписку до ctx.Done()
// и вызывает onInvalidate на каждое чужое invalidate-сообщение (self-origin
// уже отфильтрован подпиской по KID).
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
				// Канал закрыт (фатальная ошибка подписки) — выходим, Holder
				// деградирует на TTL-poll.
				return nil
			}
			onInvalidate()
		}
	}
}

// serviceInvalidatePublishTimeout — deadline на сетевой Redis PUBLISH
// invalidate-сигнала реестра (S2): если Redis недоступен, CRUD-мутация не
// блокируется дольше этого. Симметрично rbacInvalidatePublishTimeout.
const serviceInvalidatePublishTimeout = time.Second

// serviceInvalidator — адаптер serviceregistry.Invalidator поверх
// [keeperredis.PublishServiceInvalidate]. Best-effort: ошибку публикации
// логирует и глотает (мутация уже зафиксирована в БД, TTL-poll подхватит).
type serviceInvalidator struct {
	redis  *keeperredis.Client
	kid    string
	logger *slog.Logger
}

func (i serviceInvalidator) Invalidate(_ context.Context) {
	// Свой короткий deadline вместо ctx caller-а: PUBLISH не должен блокировать
	// ответ на CRUD-мутацию при недоступном Redis.
	ctx, cancel := context.WithTimeout(context.Background(), serviceInvalidatePublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishServiceInvalidate(ctx, i.redis, i.kid); err != nil {
		i.logger.Warn("serviceregistry: cluster-invalidate publish failed", slog.Any("error", err))
	}
}

// serviceInvalidationSource — адаптер serviceregistry.InvalidationSource поверх
// [keeperredis.SubscribeServiceInvalidate]. Watch держит подписку до ctx.Done()
// и вызывает onInvalidate на каждое чужое invalidate-сообщение (self-origin уже
// отфильтрован подпиской по KID).
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
				// Канал закрыт (фатальная ошибка подписки) — выходим, Holder
				// деградирует на TTL-poll.
				return nil
			}
			onInvalidate()
		}
	}
}

// sigilInvalidatePublishTimeout — deadline на сетевой Redis PUBLISH
// invalidate-сигнала (S6c): если Redis недоступен, allow/revoke не блокируется
// дольше этого. Симметрично rbacInvalidatePublishTimeout.
const sigilInvalidatePublishTimeout = time.Second

// sigilInvalidator — адаптер [sigil.Invalidator] поверх
// [keeperredis.PublishSigilInvalidate] (ADR-026, S6c). Best-effort: ошибку
// публикации логирует и глотает (мутация уже зафиксирована в БД, connect-time
// broadcast подхватит на следующем reconnect Soul-а).
type sigilInvalidator struct {
	redis  *keeperredis.Client
	logger *slog.Logger
}

func (i sigilInvalidator) Invalidate(_ context.Context) {
	// Свой короткий deadline вместо ctx caller-а: PUBLISH не должен блокировать
	// ответ на allow/revoke при недоступном Redis.
	ctx, cancel := context.WithTimeout(context.Background(), sigilInvalidatePublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishSigilInvalidate(ctx, i.redis); err != nil {
		i.logger.Warn("sigil: cluster-invalidate publish failed", slog.Any("error", err))
	}
}

// sigilAnchorsPublisher — адаптер [sigil.AnchorsPublisher] поверх
// [keeperredis.PublishAnchorsChanged] (ADR-026(h), R3-S7). После мутации реестра
// ключей подписи (Introduce/SetPrimary/Retire) шлёт в `sigil:anchors-changed`,
// по которому каждая нода re-load-ит Signer/набор и re-broadcast-ит SigilTrustAnchors.
// Best-effort: ошибку публикации логирует и глотает (мутация уже в БД, набор
// доедет на рестарте).
type sigilAnchorsPublisher struct {
	redis  *keeperredis.Client
	logger *slog.Logger
}

func (p sigilAnchorsPublisher) Publish(_ context.Context) {
	// Свой короткий deadline: PUBLISH не должен блокировать ответ на ротацию.
	ctx, cancel := context.WithTimeout(context.Background(), sigilInvalidatePublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishAnchorsChanged(ctx, p.redis); err != nil {
		p.logger.Warn("sigil: anchors-changed publish failed", slog.Any("error", err))
	}
}

// signalContext возвращает context, отменяемый при получении SIGINT/SIGTERM.
// Используется обеими командами: init — чтобы Ctrl-C прервал долгий
// docker-pull/PG-handshake; run — чтобы daemon корректно завершался.
//
// SIGHUP сюда НЕ входит — он обрабатывается отдельным каналом внутри
// [config.WatchSIGHUP], чтобы reload не путался с shutdown.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
