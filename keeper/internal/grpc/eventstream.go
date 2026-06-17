package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tlsx"
)

// EventStreamServer — gRPC-сервер EventStream-listener-а (mTLS).
//
// Отдельный listener от Bootstrap (см. [BootstrapServer]) по ADR-012(b):
// разные TLS-режимы (mTLS vs server-only), разные порты, разные
// [grpclib.Server]-ы. Бизнес-логика handler-а — в [eventStreamHandler].
type EventStreamServer struct {
	srv        *grpclib.Server
	configAddr string

	mu     sync.Mutex
	addr   string
	logger *slog.Logger
}

// defaultSoulLeaseTTL — TTL Redis-ключа lease-а Soul-стрима. Выбран так,
// чтобы выдерживать кратковременные GC-паузы / latency-spike-и Renew-а
// (Renew идёт каждые TTL/3 ≈ 20s), но при crash-е Keeper-а конкурент мог
// захватить lease в пределах минуты. Значение в конфиг (`keeper.yml`) не
// выносится: это инвариант координации, а не пользовательская настройка
// (паттерн идентичен Reaper-овскому `lock_ttl`).
const defaultSoulLeaseTTL = 60 * time.Second

// defaultLastSeenFlushInterval — fallback throttle PG-flush-а
// `souls.last_seen_at`, когда [EventStreamDeps.LastSeenFlushInterval] не
// задан (unit-тесты, dev-сборки без reaper-конфига). Выведен из дефолтного
// reaper `mark_disconnected.stale_after` (90s) делением на
// [defaultLastSeenFlushFactor] → 30s. Production wire-up передаёт значение,
// согласованное с фактическим stale_after из keeper.yml.
const defaultLastSeenFlushInterval = 90 * time.Second / defaultLastSeenFlushFactor

// Resource-лимиты EventStream-listener-а (защита от DoS, H3). В отличие
// от Bootstrap, доступ post-auth (mTLS + seed-interceptor), но лимиты всё
// равно нужны: скомпрометированный/баговый Soul не должен валить Keeper.
const (
	// eventStreamMaxRecvMsgSize — максимальный размер входящего FromSoul.
	// Крупнейший payload — SoulprintReport.typed_facts + (legacy) facts-Struct;
	// на multi-homed хосте с длинным interfaces[] и raw-facts это десятки КиБ,
	// RunResult с детальными TaskEvent — сопоставимо. 1 МиБ — комфортный
	// потолок без 4-МиБ-вектора grpc-дефолта.
	eventStreamMaxRecvMsgSize = 1 * 1024 * 1024

	// eventStreamMaxConcurrentStreams — лимит RPC на одно соединение.
	// Легитимный Soul держит ровно один долгоживущий EventStream-стрим;
	// 100 — щедрый запас, отсекающий stream-flood по одному conn.
	eventStreamMaxConcurrentStreams = 100

	// eventStreamKeepaliveMinTime — минимальный интервал client-ping-ов.
	// Soul-клиент (soul/internal/grpc/client.go) пингует каждые 30s с
	// PermitWithoutStream=true; ставим 10s ≤ 30s, чтобы легитимный ping не
	// ловил GOAWAY too_many_pings, но флуд чаще 10s отсекался.
	eventStreamKeepaliveMinTime = 10 * time.Second
)

// EventStreamDeps — wire-up зависимости EventStream-handler-а.
//
// SeedDB — pool / tx-like объект для lookup-а `soul_seeds` (peer-auth
// в interceptor-е). SoulDB — для UPDATE souls.{last_seen_*, soulprint_*}.
// Redis — клиент координации (SoulLease, heartbeat-кэш). KID — идентификатор
// Keeper-инстанса (попадает в HelloReply.kid и в lease-value). Logger —
// общий slog для всех стримов. SoulLeaseTTL — TTL Redis-lease ключа
// (zero → [defaultSoulLeaseTTL]); существует только для тестов с
// miniredis FastForward, в проде остаётся дефолтом.
//
// Manager — реестр активных стримов для outbound-направления (M2.5).
// При nil — handler работает в inbound-only режиме (unit-тесты без
// outbound API); production wire-up (`keeper run`) обязан передать
// non-nil Manager.
//
// SeedRotation — wire-up для `SeedRotationRequest`-handler-а (M2.5).
// При nil — handler логирует warn и игнорирует запрос (минимально-инвазивный
// fallback на случай не-prod сборок без Vault PKI).
type EventStreamDeps struct {
	SeedDB       soulseed.ExecQueryRower
	SoulDB       soul.ExecQueryRower
	Redis        *keeperredis.Client
	AuditWriter  audit.Writer
	KID          string
	SoulLeaseTTL time.Duration

	// LastSeenFlushInterval — throttle PG-flush-а `souls.last_seen_at`
	// (ADR-006(a)). Live-стрим освежает heartbeat в Redis на каждое
	// app-сообщение, но в PG snapshot пишется не чаще раза в этот интервал
	// на каждый SID — иначе Reaper (`mark_disconnected`) ложно помечает
	// живой стрим disconnected (он смотрит на PG-`last_seen_at`).
	//
	// zero → [defaultLastSeenFlushInterval]. Production wire-up (`keeper run`)
	// выводит значение из reaper `mark_disconnected.stale_after` (÷3), чтобы
	// flush заведомо был чаще порога disconnect. SoulDB=nil → flush выключен
	// (heartbeat остаётся только в Redis, dev / unit-режим).
	LastSeenFlushInterval time.Duration

	Manager      *StreamManager
	SeedRotation *SeedRotationDeps

	// ApplyBus — pub/sub шина apply-событий (M0.7.c). nil → publish-операции
	// в handler-ах TaskEvent/RunResult выключены (single-Keeper dev-режим без
	// SSE-консьюмеров). Production wire-up (`keeper run`) обязан передать
	// non-nil bus, общий с mcp.ServerDeps.Bus.
	ApplyBus *applybus.EventBus

	// ApplyRunDB — реестр `apply_runs` (M2.x) для correlation `apply_id` ↔
	// incarnation/scenario. RunResult-handler читает строку по
	// `(apply_id, sid)` и переводит её в терминальный статус. nil →
	// correlation/persist выключены (unit-тесты без PG, ad-hoc push без
	// scenario-runner-а); production wire-up (`keeper run`) передаёт pool.
	ApplyRunDB applyrun.ExecQueryRower

	// Metrics — keeper_grpc_*-collectors (ADR-024). nil → инструментация
	// выключена (nil-safe методы [GRPCMetrics] — no-op): unit-тесты и
	// dev-сборки без observability. Production wire-up (`keeper run`)
	// передаёт зарегистрированный дескриптор поверх общего [obs.Registry].
	Metrics *GRPCMetrics

	// SigilStore — реестр активных печатей доверия плагинов (ADR-026, S6).
	// Connect-time broadcast: сразу после handshake handler читает ListActive
	// и раздаёт каждую запись Soul-у как PluginSigil. nil → broadcast no-op
	// (Sigil выключен / dev / unit-режим). Production wire-up передаёт тот же
	// источник, что и sigil-service (общий pool, см. daemon.setupGRPCEventStream).
	SigilStore SigilStore

	// TrustAnchors — источник ТЕКУЩЕГО набора trust-anchor-ов подписи Sigil
	// (ADR-026(h), R3-S6). Connect-time broadcast: сразу после handshake handler
	// читает набор PEM-якорей и шлёт его Soul-у одним [keeperv1.SigilTrustAnchors]
	// (ReplaceAll). «Живой» источник (а не зафиксированный на старте набор), чтобы
	// после runtime-ротации ключей подписи (S6 hot-reload) свежеподключённый Soul
	// получал актуальный набор. nil → broadcast no-op (Sigil выключен / dev / unit).
	// Production wire-up передаёт holder, обновляемый watcher-ом `sigil:anchors-changed`
	// (см. daemon.setupGRPCEventStream).
	TrustAnchors TrustAnchorSource

	// Augur — wire-up для `AugurRequest`-handler-а (ADR-025, augur.md): резолв
	// доступа + брокер vault/prometheus/elk (delegate=false). nil → handler
	// логирует warn и игнорирует запрос (сборки без Augur). Production wire-up
	// передаёт DB (omens/rites + souls), Vault-клиент, SSRF-guarded Egress-клиент
	// и тот же Outbound, что и SeedRotationDeps.
	Augur *AugurDeps

	// AugurConcurrency — лимит параллельных Augur-обработок (global, все стримы;
	// DoS-guard, см. eventStreamHandler.augurSem). <=0 →
	// [defaultAugurConcurrency]. Игнорируется при Augur==nil.
	AugurConcurrency int

	// Oracle — wire-up для `PortentEvent`-handler-а (ADR-030, beacons reactor,
	// срез S2): match Decree + cooldown + постановка named-scenario в work-queue.
	// nil → handler логирует warn и игнорирует Portent (сборки без Oracle, как
	// Augur). Production wire-up передаёт DB (decrees/oracle_fires + souls),
	// where-CEL evaluator, ScenarioEnqueuer и AuditWriter.
	Oracle *OracleDeps

	// VigilSource — источник active-набора Vigil для SID при connect-time
	// broadcast-е VigilSnapshot (ADR-030, ReplaceAll, паттерн SigilStore). nil →
	// snapshot не шлётся (Oracle выключен / dev / unit). Production wire-up
	// передаёт реестр vigils (тот же pool, что OracleDeps.DB).
	VigilSource VigilSource

	// TollNotifier — Toll cluster-detector hook (ADR-038): handler зовёт его
	// при выходе из receive-loop-а (disconnect-event). nil → Toll не подключён
	// (single-instance/dev без Redis): hook no-op. Production wire-up
	// (`keeper run`) передаёт *toll.Watcher, gate-нутый отсутствием Redis.
	TollNotifier TollNotifier
}

// TollNotifier — узкая поверхность Toll-hook-а disconnect-event-а. Сужение
// до одного метода держит EventStream-handler независимым от полного
// toll.Watcher-а (тот несёт публикацию, метрики, фильтры) и допускает fake
// в unit-тестах handler-а. Реализация — *toll.Watcher (метод NotifyDisconnect).
//
// gracefulShutdown=true означает «инициатор закрытия — сам keeper-инстанс»
// (ctx-cancel: Watchman shedding / graceful keeper shutdown); false — клиент
// (EOF / транспортная ошибка / Recv-error). Toll фильтрует graceful (это не
// отток, это плановое закрытие).
type TollNotifier interface {
	NotifyDisconnect(ctx context.Context, sid, coven string, gracefulShutdown bool)
}

// VigilSource — узкая поверхность резолва active-набора Vigil для хоста, нужная
// connect-time broadcast-у ([broadcastVigils]). Сужение до одного метода
// изолирует EventStream-handler от полного CRUD-а реестра vigils и допускает
// fake в unit-тестах. Реализация резолвит covens хоста из souls-registry
// (авторитетно) и набор Vigil по sid ∪ covens
// ([oracle.SelectActiveVigilsForSubject]), возвращая готовые транспортные
// [keeperv1.VigilDef].
type VigilSource interface {
	ActiveVigilsForSID(ctx context.Context, sid string) ([]*keeperv1.VigilDef, error)
}

// SigilStore — узкая поверхность реестра plugin_sigils, нужная connect-time
// broadcast-у. Сужение до одного метода изолирует EventStream-handler от
// полного CRUD-а [sigil.Store] и позволяет fake-у в unit-тестах. Реальный
// [sigil.Store] (из [sigil.NewPGStore]) удовлетворяет автоматически.
//
// Возвращает сырые [sigil.Sigil] (с byte-exact ManifestRaw + Signature),
// а НЕ projection [sigil.SigilView] — broadcast обязан слать подписанные
// байты, иначе Soul-side verify (fail-closed) отвергнет печать.
type SigilStore interface {
	ListActive(ctx context.Context) ([]*sigil.Sigil, error)
}

// TrustAnchorSource — узкая поверхность чтения ТЕКУЩЕГО набора trust-anchor-ов
// подписи Sigil в PEM-форме (ADR-026(h), R3-S6), нужная connect-time broadcast-у
// ([broadcastTrustAnchors]). Метод возвращает свежий снимок набора: после
// runtime-ротации ключей подписи (S6 hot-reload подменяет Signer) свежеподключённый
// Soul получает актуальный набор, а не зафиксированный на старте. Реализация в
// daemon — atomic-holder, обновляемый watcher-ом `sigil:anchors-changed`.
type TrustAnchorSource interface {
	AnchorSetPEM() []string
}

func (d EventStreamDeps) validate() error {
	if d.SeedDB == nil {
		return errors.New("grpc: EventStreamDeps.SeedDB is required")
	}
	if d.AuditWriter == nil {
		return errors.New("grpc: EventStreamDeps.AuditWriter is required")
	}
	if d.KID == "" {
		return errors.New("grpc: EventStreamDeps.KID is required")
	}
	// SoulDB и Redis опциональны на уровне сборки сервера: unit-тесты
	// поднимают listener без них (handshake-only smoke). EventStream-handler
	// при отсутствии каждого деградирует: SoulDB=nil → UpdateSoulprint
	// пропускается с warn-ом; Redis=nil → lease/heartbeat выключены (single-
	// instance dev-режим). Manager / SeedRotation также опциональны (см.
	// тип-комментарий). Жёсткая обязательность будет в production-wire-up-е
	// (отдельный slice — runDaemon).
	if d.SeedRotation != nil {
		if err := d.SeedRotation.validate(); err != nil {
			return err
		}
	}
	if d.Augur != nil {
		if err := d.Augur.validate(); err != nil {
			return err
		}
	}
	if d.Oracle != nil {
		if err := d.Oracle.validate(); err != nil {
			return err
		}
	}
	return nil
}

// NewEventStreamServer собирает EventStream-listener с mTLS и
// зарегистрированным [eventStreamHandler] поверх stream-interceptor-а
// [streamSeedAuthInterceptor].
//
// Возвращает error на:
//   - пустой cfg.Addr / TLS.Cert / TLS.Key / TLS.CA;
//   - неверные file paths TLS (передаются в [tlsx.LoadMutualTLS]);
//   - nil deps (см. [EventStreamDeps.validate]).
func NewEventStreamServer(cfg config.KeeperListenGRPCEventStream, deps EventStreamDeps, logger *slog.Logger) (*EventStreamServer, error) {
	if cfg.Addr == "" {
		return nil, errors.New("grpc: listen.grpc.event_stream.addr is empty")
	}
	if logger == nil {
		return nil, errors.New("grpc: logger is required")
	}
	if err := deps.validate(); err != nil {
		return nil, err
	}

	tlsCfg, err := tlsx.LoadMutualTLS(tlsx.MutualConfig{
		CertPath: cfg.TLS.Cert,
		KeyPath:  cfg.TLS.Key,
		CAPath:   cfg.TLS.CA,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc: load event_stream mTLS: %w", err)
	}

	auth := NewSeedAuthenticator(deps.SeedDB, logger)

	// send-лимит исходящего FromKeeper (прежде всего ApplyRequest с пачкой
	// RenderedTask). Конфигурируемый, дефолт 8 MiB. Должен быть ≤ Soul-recv-
	// лимиту (`keeper.max_apply_size_mb` в soul.yml); при превышении Keeper
	// падает fail-fast с понятной ResourceExhausted-ошибкой, а не отдаёт Soul-у
	// сообщение, которое тот молча отвергнет. recv-лимит входящих FromSoul —
	// отдельный внутренний инвариант (eventStreamMaxRecvMsgSize, 1 MiB).
	srv := grpclib.NewServer(
		grpclib.Creds(credentials.NewTLS(tlsCfg)),
		grpclib.StreamInterceptor(streamSeedAuthInterceptor(auth)),
		grpclib.MaxRecvMsgSize(eventStreamMaxRecvMsgSize),
		grpclib.MaxSendMsgSize(cfg.ResolvedMaxApplySize()),
		grpclib.MaxConcurrentStreams(eventStreamMaxConcurrentStreams),
		grpclib.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             eventStreamKeepaliveMinTime,
			PermitWithoutStream: true,
		}),
	)
	keeperv1.RegisterKeeperServer(srv, newEventStreamHandler(deps, logger))

	return &EventStreamServer{
		srv:        srv,
		configAddr: cfg.Addr,
		addr:       cfg.Addr,
		logger:     logger,
	}, nil
}

// Start — блокирующий запуск listener-а. Семантика идентична
// [BootstrapServer.Start]: на ctx.Done() — GracefulStop с
// [graceDuration]-timeout-ом, превышение → forced Stop.
func (s *EventStreamServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.configAddr)
	if err != nil {
		return fmt.Errorf("grpc: listen %q: %w", s.configAddr, err)
	}
	actual := ln.Addr().String()
	s.mu.Lock()
	s.addr = actual
	s.mu.Unlock()
	s.logger.Info("gRPC EventStream listener started", slog.String("addr", actual))

	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, grpclib.ErrServerStopped) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("gRPC EventStream listener received shutdown signal")
		stopped := make(chan struct{})
		go func() {
			s.srv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(graceDuration):
			s.logger.Warn("gRPC EventStream GracefulStop did not finish within grace — forcing Stop")
			s.srv.Stop()
		}
		select {
		case serveErr := <-errCh:
			if serveErr != nil && !errors.Is(serveErr, grpclib.ErrServerStopped) {
				s.logger.Warn("gRPC EventStream Serve returned error after shutdown",
					slog.Any("error", serveErr))
			}
		case <-time.After(2 * time.Second):
			s.logger.Warn("gRPC EventStream Serve did not exit within 2s after Stop — leak suspected")
		}
		s.logger.Info("gRPC EventStream listener stopped")
		return nil
	case err := <-errCh:
		return err
	}
}

// Addr возвращает фактический bind-адрес. После Start — actual port
// (важно для тестов с `:0`).
func (s *EventStreamServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// eventStreamHandler — реализация [keeperv1.KeeperServer] для
// EventStream-listener-а.
//
// На этом listener-е Ping/Bootstrap не имеют смысла (server-only TLS-handshake
// невозможен с mTLS-config-ом, у клиента нет SoulSeed до онбординга), но
// embedded [keeperv1.UnimplementedKeeperServer] возвращает Unimplemented
// автоматически — отдельной обработки не требуется.
type eventStreamHandler struct {
	keeperv1.UnimplementedKeeperServer
	deps          EventStreamDeps
	logger        *slog.Logger
	lastSeenFlush *lastSeenFlusher

	// augurSem — global семафор параллельных Augur-обработок поверх ВСЕХ
	// стримов (handler — один экземпляр на сервер). Каждый AugurRequest
	// спавнит горутину (vault/prom/elk-fetch может занять время и не должен
	// блокировать receive-loop); без лимита НЕдоверенный Soul-flood
	// AugurRequest-ов исчерпал бы горутины/соединения Keeper-а — DoS-вектор.
	// Переполнение → AugurReply{ERROR}, без нового спавна (non-blocking
	// acquire). nil → лимит выключен (старое поведение, dev/unit без Augur).
	augurSem chan struct{}
}

// defaultAugurConcurrency — дефолтный лимит параллельных Augur-обработок (global,
// все стримы). Подобран как умеренный: live-fetch — нечастая операция в прогоне,
// но vault+prom+elk вместе под Soul-flood-ом без лимита опасны. Production
// wire-up может переопределить через EventStreamDeps.AugurConcurrency.
const defaultAugurConcurrency = 64

func newEventStreamHandler(deps EventStreamDeps, logger *slog.Logger) *eventStreamHandler {
	flushInterval := deps.LastSeenFlushInterval
	if flushInterval <= 0 {
		flushInterval = defaultLastSeenFlushInterval
	}

	var augurSem chan struct{}
	if deps.Augur != nil {
		limit := deps.AugurConcurrency
		if limit <= 0 {
			limit = defaultAugurConcurrency
		}
		augurSem = make(chan struct{}, limit)
	}

	return &eventStreamHandler{
		deps:          deps,
		logger:        logger,
		lastSeenFlush: newLastSeenFlusher(flushInterval),
		augurSem:      augurSem,
	}
}

// EventStream — bidi-стрим Keeper↔Soul по ADR-012(a).
//
// M2.3 + M2.4 scope:
//   - handshake: Hello → HelloReply (как в M2.2);
//   - Redis SoulLease per SID (`soul:<sid>:lock = <kid>`, TTL renewal в
//     отдельной goroutine). Конфликт → close stream с AlreadyExists;
//   - touch heartbeat-кэша на каждое app-сообщение;
//   - dispatch FromSoul.payload → TaskEvent / RunResult / SoulprintReport
//     handler-ы.
//
// SID берётся из interceptor-context-а ([authenticatedSIDFrom]),
// authoritative-источник — peer cert (mTLS). `Hello.sid_echo` — только
// для логов; рассинхрон логируется как warn, не отвергается (cf.
// docs/naming-rules.md → таблица proto-сообщений).
func (h *eventStreamHandler) EventStream(stream grpclib.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper]) error {
	sid, ok := authenticatedSIDFrom(stream.Context())
	if !ok {
		// Interceptor не отработал — bug wire-up-а. Без него мы не знаем,
		// кто на том конце.
		h.logger.Error("EventStream invoked without authenticated SID — interceptor misconfigured")
		return status.Error(codes.Internal, "authentication context missing")
	}

	// Производный от stream-ctx-а per-stream ctx с собственным cancel-ом. Весь
	// дальнейший handler (lease-renewal, send-loop, subscribe-loop, touchSeen,
	// receive-loop) работает поверх него. cancelStream регистрируется в
	// StreamManager ниже, чтобы Watchman (soul-shedding S2) мог принудительно
	// закрыть стрим при устойчивой изоляции инстанса ([StreamManager.CloseAll]):
	// отмена будит receive/send-loop-ы (`ctx.Err() != nil` / `<-ctx.Done()`),
	// handler делает СВОЙ штатный teardown (Unregister → lease-Release LIFO),
	// gRPC шлёт Soul-у EOF, и Soul по reconnect-loop/failback-list уходит на
	// живой Keeper. Отмена by-Watchman неотличима для teardown-а от естественного
	// обрыва стрима. defer cancelStream() — финальное освобождение ресурса ctx
	// (идемпотентно к уже сработавшей отмене от CloseAll).
	ctx, cancelStream := context.WithCancel(stream.Context())
	defer cancelStream()

	// Первое сообщение обязано быть Hello (handshake-фаза).
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			h.logger.Info("eventstream: client closed before Hello",
				slog.String("sid", sid))
			return nil
		}
		return status.Errorf(codes.Internal, "recv Hello: %v", err)
	}
	h.deps.Metrics.ObserveMessage(directionFromSoul)
	hello := first.GetHello()
	if hello == nil {
		return status.Errorf(codes.FailedPrecondition,
			"first message must be Hello (got %T)", first.GetPayload())
	}
	if echo := hello.GetSidEcho(); echo != "" && echo != sid {
		h.logger.Warn("eventstream: Hello.sid_echo differs from authenticated SID",
			slog.String("sid", sid),
			slog.String("sid_echo", echo),
		)
	}

	// Захват Redis-lease на SID. По PM-decision (1): конфликт → close stream
	// с AlreadyExists. Без Redis (dev-сборка) — пропускаем lease, single-
	// instance режим.
	leaseCleanup, leaseErr := h.acquireSoulLease(ctx, sid)
	if leaseErr != nil {
		return leaseErr
	}
	defer leaseCleanup()

	// Presence (online) теперь = живой Redis SID-lease, захваченный выше через
	// acquireSoulLease: он гаснет на этом teardown-е (Release в leaseCleanup) или
	// по TTL после crash-а. Синхронной PG-записи presence на teardown-е больше
	// нет (ADR-006(a) amend) — таргет-резолвер деривирует online из lease, не из
	// `souls.status`. Ленивое согласование PG-снимка status (для Operator API
	// «последнее известное») делает Reaper-правило `mark_disconnected` (тоже
	// lease-aware), без зависимости штатного закрытия от записи здесь.

	// StreamManager-регистрация и send-loop (M2.5). Manager=nil — режим
	// без outbound (handshake-only smoke / unit-тесты): пропускаем
	// регистрацию и send-loop, FromKeeper-сообщения не отправляются.
	//
	// Unregister обязан выполниться ДО ожидания send-loop-а (см. ниже),
	// потому что send-loop выходит по close(outCh), который происходит
	// внутри Unregister. Defer-цепочка строится так, чтобы:
	//   1) сначала Unregister (закрывает outCh, send-loop выходит);
	//   2) затем <-sendDone (joins goroutine);
	//   3) затем leaseCleanup.
	// LIFO-семантика defer-а инвертирует порядок, поэтому объявляем их
	// в обратном порядке (cleanup → join → unregister).
	//
	// cancelStream регистрируется вместе со стримом — отдаёт Watchman точку
	// принудительного закрытия (см. derive выше / [StreamManager.CloseAll]).
	var outCh <-chan *keeperv1.FromKeeper
	if h.deps.Manager != nil {
		outCh = h.deps.Manager.RegisterStream(sid, cancelStream)
	}

	sessionID := audit.NewULID()
	reply := &keeperv1.HelloReply{
		SessionId:  sessionID,
		Kid:        h.deps.KID,
		ServerTime: timestamppb.Now(),
	}
	if err := stream.Send(&keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_HelloReply{HelloReply: reply},
	}); err != nil {
		return status.Errorf(codes.Unavailable, "send HelloReply: %v", err)
	}
	h.deps.Metrics.ObserveMessage(directionToSoul)

	// Connect-time broadcast печатей доверия плагинов (ADR-026, S6): сразу
	// после HelloReply и ДО старта send-loop-а — отправка идёт напрямую
	// stream.Send в этой же горутине, поэтому порядок гарантирован
	// (HelloReply → все PluginSigil → app-сообщения), без гонки с send-loop-ом
	// и без зависимости от размера outbound-буфера. Best-effort: на любой
	// проблеме (Sigil off / пустой реестр / ошибка чтения / fail отправки)
	// стрим живёт дальше — Soul-side verify fail-closed защитит от
	// неподписанных/неполученных плагинов.
	h.broadcastSigils(ctx, stream, sid, sessionID)

	// Connect-time broadcast набора trust-anchor-ов подписи Sigil (ADR-026(h),
	// R3-S6): сразу после snapshot допусков и в той же горутине — отправка
	// напрямую stream.Send, порядок гарантирован (HelloReply → SigilSnapshot →
	// SigilTrustAnchors → app-сообщения). Так свежеподключённый Soul получает
	// АКТУАЛЬНЫЙ набор якорей (включая после runtime-ротации, S6 hot-reload),
	// против которого верифицирует подписи допусков. Best-effort — см.
	// [broadcastTrustAnchors].
	h.broadcastTrustAnchors(ctx, stream, sid, sessionID)

	// Connect-time broadcast active-набора Vigil (ADR-030, beacons-контур S2):
	// в той же горутине после snapshot-ов Sigil, ДО старта send-loop-а —
	// отправка напрямую stream.Send, порядок гарантирован (HelloReply →
	// SigilSnapshot → SigilTrustAnchors → VigilSnapshot → app-сообщения).
	// ReplaceAll: Soul-scheduler заменяет весь локальный набор Vigil этим
	// списком (S1). Best-effort — см. [broadcastVigils].
	h.broadcastVigils(ctx, stream, sid, sessionID)

	// Стрим считается открытым после успешного handshake (HelloReply
	// доставлен). Inc/Dec — gauge активных стримов; Dec в defer гарантирует
	// баланс на любом пути выхода handler-а.
	h.deps.Metrics.IncStreams()
	defer h.deps.Metrics.DecStreams()

	// При закрытии стрима убираем SID из throttle-стейта flusher-а, чтобы
	// карта не накапливала disconnected-Soul-ов. Следующее подключение того
	// же SID начнёт с немедленного flush-а.
	defer h.lastSeenFlush.forget(sid)

	// Hello — тоже app-сообщение, обновляет heartbeat (Redis) и throttled-flush
	// `last_seen_at`-снимка в PG. Presence (online) НЕ пишется в PG на
	// session-open: авторитет — захваченный выше Redis SID-lease (ADR-006(a)),
	// и таргет-резолвер деривирует online из него. Это снимает прежний
	// HA-blocker (переподключившийся Soul был невидим резолверу из-за
	// disconnected-снимка в `souls`) без синхронной presence-записи в PG —
	// lease уже захвачен, Soul виден сразу.
	h.touchSeen(ctx, sid)

	h.logger.Info("eventstream: session opened",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.String("soul_version", hello.GetSoulVersion()),
	)
	defer h.logger.Info("eventstream: session closed",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
	)

	// Send-loop: вычитывает outCh, отправляет в stream.Send. Существует
	// только если StreamManager wired up (outCh != nil). Завершается по
	// ctx.Done() (graceful stop) либо по close-у outCh (Unregister).
	// stream.Send-ошибка → лог + выход (стрим уже сломан, receive-loop
	// тоже встретит EOF).
	sendDone := make(chan struct{})
	if outCh != nil {
		go h.runSendLoop(ctx, stream, sid, sessionID, outCh, sendDone)
	} else {
		close(sendDone)
	}

	// Cluster-mode subscribe-loop: подписка на Redis pub/sub-канал
	// `outbound:<sid>`, форвард входящих FromKeeper в outCh (Outbound
	// на других Keeper-инстансах публикует туда при routing-е).
	// Включается только когда Manager и Redis оба не-nil — иначе
	// cluster-routing не нужен (single-instance / unit-тесты).
	subDone := make(chan struct{})
	subCleanup := h.startOutboundSubscriber(ctx, sid, subDone)

	// LIFO defer chain: при return-е handler-а сработает в порядке
	//   subCleanup (close pub/sub) → join subDone → Unregister →
	//   join sendDone → leaseCleanup (объявлен ранее).
	// Этот порядок гарантирует:
	//   1) subscriber перестаёт писать в outCh раньше Unregister-а
	//      (иначе entry.send на закрытый канал — false, drop в лог);
	//   2) send-loop успевает дочитать outCh после Unregister-а;
	//   3) lease отдаётся последним — другой Keeper подберёт сразу.
	defer func() {
		if h.deps.Manager != nil {
			h.deps.Manager.Unregister(sid, outCh)
		}
		<-sendDone
	}()
	defer func() {
		if subCleanup != nil {
			subCleanup()
		}
		<-subDone
	}()

	// Receive-loop вынесен в отдельную горутину, потому что `stream.Recv()`
	// блокирует на gRPC-стриме и НЕ реагирует на отмену нашего производного ctx
	// (его контролирует gRPC, а не мы). Watchman-shedding ([StreamManager.CloseAll])
	// отменяет cancelStream → срабатывает ветка `<-ctx.Done()` ниже → handler
	// возвращается → gRPC рвёт RPC и закрывает стрим → осиротевший `stream.Recv()`
	// в этой горутине разблокируется ошибкой и она выходит сама (её НЕ join-им
	// синхронно до return-а: иначе deadlock — Recv разблокируется только ПОСЛЕ
	// закрытия стрима, т.е. после return-а handler-а). recvErr буферизован на 1,
	// чтобы горутина-receiver не повисла на send-е после того, как handler уже
	// ушёл по ctx.Done().
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) || ctx.Err() != nil {
					recvErr <- nil
					return
				}
				recvErr <- status.Errorf(codes.Internal, "recv: %v", err)
				return
			}
			h.dispatch(ctx, sid, sessionID, msg)
		}
	}()

	// Выход по любому из двух событий:
	//   - receive-loop завершился (EOF / транспортная ошибка / отмена ctx,
	//     замеченная самим Recv-ом после возврата) — отдаём его терминальный err;
	//   - ctx отменён извне (Watchman-shedding при устойчивой изоляции) — выходим
	//     БЕЗ ошибки: для Soul-а это EOF, он реконнектится на живой Keeper.
	// В обоих случаях defer-цепочка делает штатный teardown (subscriber →
	// Unregister → send-loop join → lease-Release LIFO).
	//
	// Toll-hook (ADR-038): на КАЖДОМ выходе уведомляем cluster-detector.
	// gracefulShutdown=true для ctx.Done()-ветки (закрытие инициировано самим
	// keeper-инстансом — Watchman shedding или graceful keeper shutdown);
	// false для recvErr-ветки (клиент-side disconnect / транспортная ошибка).
	// Toll сам фильтрует graceful (это не отток). Coven неизвестен в этой точке
	// (резолв стоил бы PG-query на каждом disconnect-е) — передаём пустую
	// строку; per-coven counter в [toll.Metrics] остаётся consistent (label="").
	select {
	case err := <-recvErr:
		h.notifyTollDisconnect(ctx, sid, false)
		return err
	case <-ctx.Done():
		h.logger.Info("eventstream: session shed by isolation (forced close)",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
		)
		h.notifyTollDisconnect(ctx, sid, true)
		return nil
	}
}

// notifyTollDisconnect — best-effort hook в Toll cluster-detector (ADR-038).
// nil-safe: при выключенном Toll (deps.TollNotifier==nil) — no-op. ctx
// освобождаем от cancel (используется как baggage-носитель, hook сам берёт
// короткий timeout внутри Publisher-а).
func (h *eventStreamHandler) notifyTollDisconnect(ctx context.Context, sid string, gracefulShutdown bool) {
	if h.deps.TollNotifier == nil {
		return
	}
	// WithoutCancel: stream-ctx уже отменён (graceful shutdown / shedding),
	// но Toll-publish обязан долететь до Redis. Короткий timeout — внутри
	// Publisher-а (он сам ограничен).
	hookCtx := context.WithoutCancel(ctx)
	h.deps.TollNotifier.NotifyDisconnect(hookCtx, sid, "", gracefulShutdown)
}

// runSendLoop — goroutine, гонит сообщения из per-stream outbound-канала
// в gRPC stream.Send. Запускается из [EventStream], завершается по
// ctx.Done() или close-у outCh (Unregister).
//
// stream.Send блокирует goroutine — это естественный back-pressure от
// клиента. PM-decision M2.5(1) buffered=10: переполнение buffer-а
// сигнализируется в Outbound.send (drop+log), сюда уже доходят только
// принятые сообщения.
func (h *eventStreamHandler) runSendLoop(
	ctx context.Context,
	stream grpclib.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper],
	sid, sessionID string,
	outCh <-chan *keeperv1.FromKeeper,
	done chan<- struct{},
) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-outCh:
			if !ok {
				return
			}
			if err := stream.Send(msg); err != nil {
				h.logger.Warn("eventstream: send-loop stream.Send failed",
					slog.String("sid", sid),
					slog.String("session_id", sessionID),
					slog.Any("error", err),
				)
				return
			}
			h.deps.Metrics.ObserveMessage(directionToSoul)
		}
	}
}

// broadcastSigils раздаёт Soul-у ПОЛНЫЙ active-набор печатей доверия плагинов
// одним [keeperv1.SigilSnapshot] (ADR-026(h), Вариант A: snapshot — единственный
// источник истины active-набора, применяется как ReplaceAll). Вызывается из
// [EventStream] в той же горутине между HelloReply и стартом send-loop-а, поэтому
// шлёт напрямую stream.Send (порядок гарантирован, буфер не задействован).
//
// Connect-time snapshot обязателен и при пустом реестре: пустой sigils[] = «ни
// один плагин не допущен», и Soul ReplaceAll-ом приведёт кеш к этому состоянию
// (важно для near-instant revoke на reconnect-е). Единственное исключение —
// SigilStore=nil (Sigil выключен / dev / unit): тогда snapshot не шлём вовсе,
// чтобы не навязывать пустой набор на хостах с выключенным Sigil.
//
// Best-effort:
//   - SigilStore=nil → no-op без ошибки;
//   - ListActive вернул ошибку → warn, snapshot пропускается, стрим жив
//     (Soul-side verify fail-closed и так защитит);
//   - stream.Send упал → warn (стрим уже сломан, receive-loop встретит EOF).
//
// PluginSigil.Manifest внутри snapshot = rec.ManifestRaw (byte-exact подписанные
// байты, M1) через [sigilRecordToProto]: re-хеш на Soul-е идёт именно над этими
// байтами через NormalizeManifestBytes (S3↔S6-инвариант).
func (h *eventStreamHandler) broadcastSigils(
	ctx context.Context,
	stream grpclib.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper],
	sid, sessionID string,
) {
	if h.deps.SigilStore == nil {
		return
	}
	recs, err := h.deps.SigilStore.ListActive(ctx)
	if err != nil {
		h.logger.Warn("eventstream: sigil snapshot list failed — skipping (verify fail-closed protects)",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_SigilSnapshot{
			SigilSnapshot: &keeperv1.SigilSnapshot{Sigils: SigilRecordsToProto(recs)},
		},
	}
	if err := stream.Send(msg); err != nil {
		h.logger.Warn("eventstream: sigil snapshot send failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	h.deps.Metrics.ObserveMessage(directionToSoul)
	h.logger.Debug("eventstream: sigil snapshot sent (ReplaceAll)",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.Int("count", len(recs)),
	)
}

// broadcastTrustAnchors раздаёт Soul-у ТЕКУЩИЙ набор trust-anchor-ов подписи
// Sigil одним [keeperv1.SigilTrustAnchors] (ADR-026(h), R3-S6, ReplaceAll).
// Вызывается из [EventStream] в той же горутине после [broadcastSigils] и до
// старта send-loop-а, поэтому шлёт напрямую stream.Send (порядок гарантирован,
// буфер не задействован).
//
// Набор берётся «живым» из [TrustAnchorSource], а не из зафиксированного на
// старте слайса: после runtime-ротации ключей подписи (S6 hot-reload подменяет
// Signer и обновляет holder) свежеподключённый Soul получает актуальный набор.
//
// Best-effort:
//   - TrustAnchors=nil → no-op без ошибки (Sigil выключен / dev / unit);
//   - пустой набор всё равно шлётся (пустой = «якорей нет» → Soul стирает holder,
//     fail-closed verify по no_trust_anchor — важно для near-instant retire на
//     reconnect-е, симметрия с пустым SigilSnapshot);
//   - stream.Send упал → warn (стрим уже сломан, receive-loop встретит EOF).
func (h *eventStreamHandler) broadcastTrustAnchors(
	ctx context.Context,
	stream grpclib.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper],
	sid, sessionID string,
) {
	_ = ctx // источник набора синхронный (atomic-holder), ctx сохранён для симметрии сигнатур
	if h.deps.TrustAnchors == nil {
		return
	}
	pems := h.deps.TrustAnchors.AnchorSetPEM()
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_SigilTrustAnchors{
			SigilTrustAnchors: &keeperv1.SigilTrustAnchors{PubkeyPem: pems},
		},
	}
	if err := stream.Send(msg); err != nil {
		h.logger.Warn("eventstream: sigil trust-anchors send failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	h.deps.Metrics.ObserveMessage(directionToSoul)
	h.logger.Debug("eventstream: sigil trust-anchors sent (ReplaceAll)",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.Int("anchors", len(pems)),
	)
}

// broadcastVigils раздаёт Soul-у active-набор Vigil одним [keeperv1.VigilSnapshot]
// (ADR-030, ReplaceAll — паттерн [broadcastSigils]/[broadcastTrustAnchors]).
// Вызывается из [EventStream] в той же горутине после snapshot-ов Sigil и до
// старта send-loop-а, поэтому шлёт напрямую stream.Send (порядок гарантирован,
// буфер не задействован).
//
// Набор резолвится по covens хоста (авторитетно из souls-registry) и его SID.
// Connect-time snapshot обязателен и при пустом наборе: пустой vigils[] = «ни
// одной активной проверки», и Soul ReplaceAll-ом приведёт scheduler к этому
// состоянию (так срабатывает disable/удаление Vigil на reconnect-е). Исключение
// — VigilSource=nil (Oracle выключен / dev / unit): тогда snapshot не шлём вовсе.
//
// Best-effort:
//   - VigilSource=nil → no-op без ошибки;
//   - резолв вернул ошибку → warn, snapshot пропускается, стрим жив;
//   - stream.Send упал → warn (стрим уже сломан, receive-loop встретит EOF).
func (h *eventStreamHandler) broadcastVigils(
	ctx context.Context,
	stream grpclib.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper],
	sid, sessionID string,
) {
	if h.deps.VigilSource == nil {
		return
	}
	vigils, err := h.deps.VigilSource.ActiveVigilsForSID(ctx, sid)
	if err != nil {
		h.logger.Warn("eventstream: vigil snapshot resolve failed — skipping",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_VigilSnapshot{
			VigilSnapshot: &keeperv1.VigilSnapshot{Vigils: vigils},
		},
	}
	if err := stream.Send(msg); err != nil {
		h.logger.Warn("eventstream: vigil snapshot send failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	h.deps.Metrics.ObserveMessage(directionToSoul)
	h.logger.Debug("eventstream: vigil snapshot sent (ReplaceAll)",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.Int("count", len(vigils)),
	)
}

// sigilRecordToProto проецирует запись реестра plugin_sigils в транспортный
// [keeperv1.PluginSigil]. Manifest = rec.ManifestRaw (byte-exact подписанные
// байты, M1), а НЕ rec.Manifest (JSONB-проекция): re-хеш на Soul-е идёт именно
// над этими байтами через NormalizeManifestBytes (S3↔S6-инвариант). Общий для
// connect-time broadcast ([broadcastSigils]) и cluster re-broadcast
// ([Outbound.RebroadcastSigils], S6c) — единая точка маппинга.
func sigilRecordToProto(rec *sigil.Sigil) *keeperv1.PluginSigil {
	return &keeperv1.PluginSigil{
		Namespace:    rec.Namespace,
		Name:         rec.Name,
		Ref:          rec.Ref,
		BinarySha256: rec.SHA256,
		Signature:    rec.Signature,
		Manifest:     rec.ManifestRaw,
	}
}

// SigilRecordsToProto конвертирует active-набор записей реестра plugin_sigils в
// транспортные [keeperv1.PluginSigil] для cluster re-broadcast-а (S6c).
// Экспортирован для wire-up в `keeper run` (daemon собирает набор из
// [SigilStore.ListActive] и отдаёт в [Outbound.RebroadcastSigils]).
func SigilRecordsToProto(recs []*sigil.Sigil) []*keeperv1.PluginSigil {
	out := make([]*keeperv1.PluginSigil, 0, len(recs))
	for _, rec := range recs {
		out = append(out, sigilRecordToProto(rec))
	}
	return out
}

// acquireSoulLease — захват lease и поднятие renewal-goroutine. Возвращает
// cleanup-функцию, которую caller вызывает через defer.
//
// На Redis=nil (dev / unit-сборка) lease выключен — возвращает no-op cleanup.
// На ErrLeaseTaken → AlreadyExists (PM-decision 1).
func (h *eventStreamHandler) acquireSoulLease(ctx context.Context, sid string) (func(), error) {
	if h.deps.Redis == nil {
		return func() {}, nil
	}
	ttl := h.deps.SoulLeaseTTL
	if ttl <= 0 {
		ttl = defaultSoulLeaseTTL
	}

	lease, err := keeperredis.AcquireSoulLease(ctx, h.deps.Redis, sid, h.deps.KID, ttl)
	if err != nil {
		if errors.Is(err, keeperredis.ErrLeaseTaken) {
			h.logger.Warn("eventstream: soul lease held by another keeper",
				slog.String("sid", sid),
				slog.String("kid", h.deps.KID),
			)
			return nil, status.Errorf(codes.AlreadyExists,
				"soul lease held by another keeper for sid=%q", sid)
		}
		h.logger.Warn("eventstream: soul lease acquire failed",
			slog.String("sid", sid), slog.Any("error", err))
		return nil, status.Errorf(codes.Unavailable, "lease acquire failed: %v", err)
	}

	renewEvery := ttl / 3
	if renewEvery < time.Millisecond {
		renewEvery = time.Millisecond
	}
	renewCtx, cancelRenew := context.WithCancel(ctx)
	done := make(chan struct{})
	go h.renewLeaseLoop(renewCtx, sid, lease, renewEvery, done)

	return func() {
		cancelRenew()
		<-done
		// Release c detached-ctx: stream-ctx может быть уже отменён (это и
		// есть нормальный path graceful-stop-а). WithoutCancel: сохраняем
		// trace-baggage, не наследуем cancel teardown-пути.
		relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer relCancel()
		if err := lease.Release(relCtx); err != nil {
			h.logger.Warn("eventstream: soul lease release failed",
				slog.String("sid", sid), slog.Any("error", err))
		}
	}, nil
}

// renewLeaseLoop — periodic Renew-er. На ErrLeaseLost закрывает done; main
// receive-loop выходит сам по EOF/Canceled, потому что lease-loss обычно
// сопровождается timeout-ом стрима либо явной отменой. Мы НЕ форсим
// прерывание receive-loop-а — пусть Soul доест буфер и закроет стрим
// штатно (или другой Keeper, уже принявший этот SID, переотправит ему
// сигнал).
func (h *eventStreamHandler) renewLeaseLoop(ctx context.Context, sid string, lease *keeperredis.Lease, every time.Duration, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := lease.Renew(ctx); err != nil {
				if errors.Is(err, keeperredis.ErrLeaseLost) {
					h.logger.Warn("eventstream: soul lease lost during renewal",
						slog.String("sid", sid))
					return
				}
				h.logger.Warn("eventstream: soul lease renew failed",
					slog.String("sid", sid), slog.Any("error", err))
			}
		}
	}
}

// touchSeen — обновляет heartbeat на каждое app-сообщение. Два слоя:
//
//  1. Redis heartbeat-кэш (быстрый слой, real-time): HSET на каждое
//     сообщение. Ошибки не fatal — heartbeat best-effort, потеря tick-а
//     компенсируется следующим сообщением.
//  2. PG `souls.last_seen_at` (snapshot для Reaper / Operator API):
//     throttled flush не чаще раза в [lastSeenFlusher.interval] на SID.
//     Без него `mark_disconnected` ложно метил бы живой стрим disconnected
//     (он смотрит только на PG-`last_seen_at`, см. ADR-006(a) и миграцию
//     014). UpdateLastSeen — вне stream-ctx-а: stream может отмениться
//     именно в момент flush-а (graceful stop / lease-переезд), а snapshot
//     всё равно полезен — иначе при отмене окно flush-а потерялось бы.
func (h *eventStreamHandler) touchSeen(ctx context.Context, sid string) {
	now := time.Now().UTC()
	if h.deps.Redis != nil {
		if err := keeperredis.TouchHeartbeat(ctx, h.deps.Redis, sid, h.deps.KID, now); err != nil {
			h.logger.Debug("eventstream: touch heartbeat failed",
				slog.String("sid", sid), slog.Any("error", err))
		}
	}
	h.flushLastSeen(ctx, sid, now)
}

// flushLastSeen — throttled snapshot `last_seen_at` в PG. No-op без SoulDB
// (dev / unit-режим, heartbeat живёт только в Redis). Throttle per-SID;
// flush идёт под отдельным background-ctx, чтобы отмена stream-ctx-а в
// момент сброса не потеряла snapshot (короткий 2s-timeout от зависшего PG).
func (h *eventStreamHandler) flushLastSeen(ctx context.Context, sid string, now time.Time) {
	if h.deps.SoulDB == nil {
		return
	}
	if !h.lastSeenFlush.shouldFlush(sid, now) {
		return
	}
	// WithoutCancel: сохраняем trace-baggage PG-write last_seen, не наследуем
	// cancel teardown-пути.
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := soul.UpdateLastSeen(flushCtx, h.deps.SoulDB, sid, h.deps.KID, now); err != nil {
		// ErrSoulNotFound допустим (Soul удалён, стрим ещё дочитывается);
		// прочие — best-effort, следующее окно повторит.
		h.logger.Debug("eventstream: flush last_seen failed",
			slog.String("sid", sid), slog.Any("error", err))
	}
}

// dispatch — диспетчер oneof payload-а FromSoul. Hello после первого
// (handshake) логируется warn-ом и игнорируется; SeedRotationRequest
// оставлен под M2.6.
func (h *eventStreamHandler) dispatch(ctx context.Context, sid, sessionID string, msg *keeperv1.FromSoul) {
	h.deps.Metrics.ObserveMessage(directionFromSoul)
	h.touchSeen(ctx, sid)
	switch p := msg.GetPayload().(type) {
	case *keeperv1.FromSoul_Hello:
		h.logger.Warn("eventstream: duplicate Hello after handshake — ignoring",
			slog.String("sid", sid), slog.String("session_id", sessionID))
	case *keeperv1.FromSoul_TaskEvent:
		h.handleTaskEvent(ctx, sid, sessionID, p.TaskEvent)
	case *keeperv1.FromSoul_RunResult:
		h.handleRunResult(ctx, sid, sessionID, p.RunResult)
	case *keeperv1.FromSoul_SoulprintReport:
		h.handleSoulprintReport(ctx, sid, sessionID, p.SoulprintReport)
	case *keeperv1.FromSoul_SeedRotationRequest:
		h.handleSeedRotationRequest(ctx, sid, sessionID, p.SeedRotationRequest)
	case *keeperv1.FromSoul_AugurRequest:
		h.handleAugurRequest(ctx, sid, sessionID, p.AugurRequest)
	case *keeperv1.FromSoul_PortentEvent:
		h.handlePortentEvent(ctx, sid, sessionID, p.PortentEvent)
	case *keeperv1.FromSoul_WardRoster:
		h.handleWardRoster(ctx, sid, sessionID, p.WardRoster)
	case *keeperv1.FromSoul_ErrandResult:
		h.handleErrandResult(ctx, sid, sessionID, p.ErrandResult)
	default:
		h.logger.Warn("eventstream: unknown payload type",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.String("kind", fmt.Sprintf("%T", p)),
		)
	}
}
