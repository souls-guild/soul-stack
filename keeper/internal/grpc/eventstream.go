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

// EventStreamServer is the gRPC server for the EventStream listener (mTLS).
//
// Separate listener from Bootstrap (see [BootstrapServer]) per ADR-012(b):
// different TLS modes (mTLS vs server-only), different ports, different
// [grpclib.Server] instances. Handler business logic lives in
// [eventStreamHandler].
type EventStreamServer struct {
	srv        *grpclib.Server
	configAddr string

	mu     sync.Mutex
	addr   string
	logger *slog.Logger
}

// defaultSoulLeaseTTL is the TTL of the Soul stream's lease key in Redis.
// Chosen to tolerate brief GC pauses / Renew latency spikes (Renew fires
// every TTL/3 ≈ 20s), while still letting a competitor grab the lease
// within a minute of a Keeper crash. Not exposed via config (`keeper.yml`):
// this is a coordination invariant, not a user setting (same pattern as
// Reaper's `lock_ttl`).
const defaultSoulLeaseTTL = 60 * time.Second

// defaultLastSeenFlushInterval is the fallback throttle for the
// `souls.last_seen_at` PG flush when [EventStreamDeps.LastSeenFlushInterval]
// is unset (unit tests, dev builds without reaper config). Derived from the
// default reaper `mark_disconnected.stale_after` (90s) divided by
// [defaultLastSeenFlushFactor] → 30s. Production wire-up passes a value
// consistent with the actual stale_after from keeper.yml.
const defaultLastSeenFlushInterval = 90 * time.Second / defaultLastSeenFlushFactor

// Resource limits for the EventStream listener (DoS protection, H3). Unlike
// Bootstrap, access here is post-auth (mTLS + seed interceptor), but limits
// are still needed: a compromised or buggy Soul must not be able to take
// down Keeper.
const (
	// eventStreamMaxRecvMsgSize is the max size of an incoming FromSoul.
	// The largest payload is SoulprintReport.typed_facts + (legacy)
	// facts-Struct; on a multi-homed host with a long interfaces[] and raw
	// facts that's tens of KiB, and a RunResult with detailed TaskEvents is
	// comparable. 1 MiB is a comfortable ceiling, avoiding the attack
	// surface of grpc's 4-MiB default.
	eventStreamMaxRecvMsgSize = 1 * 1024 * 1024

	// eventStreamMaxConcurrentStreams is the RPC limit per connection. A
	// legitimate Soul holds exactly one long-lived EventStream stream; 100
	// is a generous margin that cuts off stream flooding on a single conn.
	eventStreamMaxConcurrentStreams = 100

	// eventStreamKeepaliveMinTime is the minimum interval between client
	// pings. The Soul client (soul/internal/grpc/client.go) pings every 30s
	// with PermitWithoutStream=true; we set 10s ≤ 30s so a legitimate ping
	// never triggers GOAWAY too_many_pings, while flooding faster than 10s
	// still gets cut off.
	eventStreamKeepaliveMinTime = 10 * time.Second
)

// EventStreamDeps holds the wire-up dependencies for the EventStream handler.
//
// SeedDB is a pool / tx-like object for `soul_seeds` lookups (peer auth in
// the interceptor). SoulDB is for UPDATE souls.{last_seen_*, soulprint_*}.
// Redis is the coordination client (SoulLease, heartbeat cache). KID is the
// Keeper instance identifier (goes into HelloReply.kid and the lease value).
// Logger is the shared slog used by all streams. SoulLeaseTTL is the Redis
// lease key TTL (zero → [defaultSoulLeaseTTL]); exists only for tests using
// miniredis FastForward, production always uses the default.
//
// Manager is the registry of active streams for the outbound direction
// (M2.5). nil → the handler runs in inbound-only mode (unit tests without
// the outbound API); production wire-up (`keeper run`) must pass a non-nil
// Manager.
//
// SeedRotation is the wire-up for the `SeedRotationRequest` handler (M2.5).
// nil → the handler logs a warning and ignores the request (a minimally
// invasive fallback for non-prod builds without Vault PKI).
type EventStreamDeps struct {
	SeedDB       soulseed.ExecQueryRower
	SoulDB       soul.ExecQueryRower
	Redis        *keeperredis.Client
	AuditWriter  audit.Writer
	KID          string
	SoulLeaseTTL time.Duration

	// LastSeenFlushInterval throttles the `souls.last_seen_at` PG flush
	// (ADR-006(a)). The live stream refreshes the heartbeat in Redis on
	// every app message, but the PG snapshot is written no more than once
	// per this interval per SID — otherwise Reaper (`mark_disconnected`)
	// would falsely mark a live stream disconnected (it looks at
	// PG-`last_seen_at`).
	//
	// zero → [defaultLastSeenFlushInterval]. Production wire-up (`keeper run`)
	// derives the value from reaper `mark_disconnected.stale_after` (÷3) so
	// the flush is reliably more frequent than the disconnect threshold.
	// SoulDB=nil → flush disabled (heartbeat stays Redis-only, dev / unit
	// mode).
	LastSeenFlushInterval time.Duration

	Manager      *StreamManager
	SeedRotation *SeedRotationDeps

	// ApplyBus is the pub/sub bus for apply events (M0.7.c). nil → publish
	// operations in the TaskEvent/RunResult handlers are disabled
	// (single-Keeper dev mode without SSE consumers). Production wire-up
	// (`keeper run`) must pass a non-nil bus, shared with mcp.ServerDeps.Bus.
	ApplyBus *applybus.EventBus

	// ApplyRunDB is the `apply_runs` registry (M2.x) for correlating
	// `apply_id` with an incarnation/scenario. The RunResult handler reads
	// the row by `(apply_id, sid)` and moves it to a terminal status. nil →
	// correlation/persist disabled (unit tests without PG, ad-hoc push
	// without a scenario runner); production wire-up (`keeper run`) passes
	// the pool.
	ApplyRunDB applyrun.ExecQueryRower

	// Metrics holds the keeper_grpc_* collectors (ADR-024). nil →
	// instrumentation disabled (nil-safe [GRPCMetrics] methods are no-ops):
	// unit tests and dev builds without observability. Production wire-up
	// (`keeper run`) passes a descriptor registered against the shared
	// [obs.Registry].
	Metrics *GRPCMetrics

	// SigilStore is the registry of active plugin trust sigils (ADR-026, S6).
	// Connect-time broadcast: right after the handshake the handler calls
	// ListActive and hands each record to the Soul as a PluginSigil. nil →
	// broadcast is a no-op (Sigil disabled / dev / unit mode). Production
	// wire-up passes the same source as the sigil service (shared pool, see
	// daemon.setupGRPCEventStream).
	SigilStore SigilStore

	// TrustAnchors is the source of the CURRENT set of Sigil-signing trust
	// anchors (ADR-026(h), R3-S6). Connect-time broadcast: right after the
	// handshake the handler reads the set of PEM anchors and sends it to the
	// Soul as a single [keeperv1.SigilTrustAnchors] (ReplaceAll). A "live"
	// source (not a set fixed at startup), so that after a runtime
	// signing-key rotation (S6 hot reload) a freshly connected Soul gets the
	// current set. nil → broadcast is a no-op (Sigil disabled / dev / unit).
	// Production wire-up passes a holder updated by the
	// `sigil:anchors-changed` watcher (see daemon.setupGRPCEventStream).
	TrustAnchors TrustAnchorSource

	// Augur is the wire-up for the `AugurRequest` handler (ADR-025,
	// augur.md): access resolution + vault/prometheus/elk broker
	// (delegate=false). nil → the handler logs a warning and ignores the
	// request (builds without Augur). Production wire-up passes the DB
	// (omens/rites + souls), a Vault client, an SSRF-guarded Egress client,
	// and the same Outbound as SeedRotationDeps.
	Augur *AugurDeps

	// AugurConcurrency is the limit on parallel Augur processing (global,
	// across all streams; DoS guard, see eventStreamHandler.augurSem). <=0 →
	// [defaultAugurConcurrency]. Ignored when Augur==nil.
	AugurConcurrency int

	// Oracle is the wire-up for the `PortentEvent` handler (ADR-030, beacons
	// reactor, slice S2): Decree matching + cooldown + enqueueing a
	// named scenario onto the work queue. nil → the handler logs a warning
	// and ignores the Portent (builds without Oracle, same as Augur).
	// Production wire-up passes the DB (decrees/oracle_fires + souls), a
	// where-CEL evaluator, a ScenarioEnqueuer, and an AuditWriter.
	Oracle *OracleDeps

	// VigilSource is the source of the active Vigil set for a SID, used by
	// the connect-time VigilSnapshot broadcast (ADR-030, ReplaceAll, same
	// pattern as SigilStore). nil → snapshot is not sent (Oracle disabled /
	// dev / unit). Production wire-up passes the vigil registry (the same
	// pool as OracleDeps.DB).
	VigilSource VigilSource

	// TollNotifier is the Toll cluster-detector hook (ADR-038): the handler
	// calls it whenever the receive loop exits (disconnect event). nil →
	// Toll is not wired up (single-instance/dev without Redis): the hook is
	// a no-op. Production wire-up (`keeper run`) passes a *toll.Watcher,
	// gated on Redis being absent.
	TollNotifier TollNotifier

	// ModuleBinaries resolves "sha256 → path to the sigil-allowed SoulModule
	// binary" for FetchModule (epic core.module.installed, S2). nil →
	// FetchModule responds Unavailable (Sigil disabled / dev / unit).
	// Production wire-up passes sigil.Service.
	ModuleBinaries ModuleBinarySource

	// ModuleFetchMaxBytes caps the size of the binary served
	// (plugins.max_artifact_size_mb). <=0 → default from config.
	ModuleFetchMaxBytes int64

	// ModuleFetchPerSID limits parallel FetchModule calls per SID (protects
	// the control plane from flooding). <=0 → [defaultModuleFetchPerSID].
	ModuleFetchPerSID int
}

// TollNotifier is the narrow surface of the Toll disconnect-event hook.
// Narrowing to one method keeps the EventStream handler independent of the
// full toll.Watcher (which also carries publication, metrics, filters) and
// allows a fake in the handler's unit tests. Implementation: *toll.Watcher
// (method NotifyDisconnect).
//
// gracefulShutdown=true means "the closing party is the keeper instance
// itself" (ctx-cancel: Watchman shedding / graceful keeper shutdown); false
// means the client (EOF / transport error / Recv error). Toll filters out
// graceful closes (that's not churn, it's a planned shutdown).
type TollNotifier interface {
	NotifyDisconnect(ctx context.Context, sid, coven string, gracefulShutdown bool)
}

// VigilSource is the narrow surface for resolving a host's active Vigil set,
// needed by the connect-time broadcast ([broadcastVigils]). Narrowing to one
// method isolates the EventStream handler from the full vigil-registry CRUD
// and allows a fake in unit tests. The implementation resolves the host's
// covens from the souls registry (authoritative) and the Vigil set by
// sid ∪ covens ([oracle.SelectActiveVigilsForSubject]), returning ready
// transport [keeperv1.VigilDef] values.
type VigilSource interface {
	ActiveVigilsForSID(ctx context.Context, sid string) ([]*keeperv1.VigilDef, error)
}

// SigilStore is the narrow surface of the plugin_sigils registry needed by
// the connect-time broadcast. Narrowing to one method isolates the
// EventStream handler from the full [sigil.Store] CRUD and allows a fake in
// unit tests. The real [sigil.Store] (from [sigil.NewPGStore]) satisfies it
// automatically.
//
// Returns raw [sigil.Sigil] records (with byte-exact ManifestRaw +
// Signature), NOT the [sigil.SigilView] projection — the broadcast must send
// the signed bytes, or Soul-side verify (fail-closed) will reject the sigil.
type SigilStore interface {
	ListActive(ctx context.Context) ([]*sigil.Sigil, error)
}

// ModuleBinarySource is the narrow surface for resolving sha256 → path to a
// sigil-allowed SoulModule binary ([sigil.Service.LookupModuleBinary]): it
// isolates FetchModule from the full sigil.Service and allows a fake in unit
// tests. A non-allowed sha returns [sigil.ErrModuleNotAllowed].
type ModuleBinarySource interface {
	LookupModuleBinary(ctx context.Context, sha256Hex string) (string, error)
}

// TrustAnchorSource is the narrow surface for reading the CURRENT set of
// Sigil-signing trust anchors in PEM form (ADR-026(h), R3-S6), needed by the
// connect-time broadcast ([broadcastTrustAnchors]). The method returns a
// fresh snapshot of the set: after a runtime signing-key rotation (S6 hot
// reload swaps the Signer), a freshly connected Soul gets the current set
// rather than one fixed at startup. The daemon implementation is an
// atomic holder updated by the `sigil:anchors-changed` watcher.
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
	// SoulDB and Redis are optional at server-build level: unit tests bring
	// up the listener without them (handshake-only smoke). The
	// EventStream handler degrades gracefully when either is absent:
	// SoulDB=nil → UpdateSoulprint is skipped with a warning; Redis=nil →
	// lease/heartbeat are disabled (single-instance dev mode). Manager /
	// SeedRotation are likewise optional (see the type comment). Strict
	// requiredness will live in the production wire-up (a separate
	// slice — runDaemon).
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

// NewEventStreamServer assembles the EventStream listener with mTLS and a
// registered [eventStreamHandler] behind the [streamSeedAuthInterceptor]
// stream interceptor.
//
// Returns an error on:
//   - empty cfg.Addr / TLS.Cert / TLS.Key / TLS.CA;
//   - invalid TLS file paths (passed to [tlsx.LoadMutualTLS]);
//   - nil deps (see [EventStreamDeps.validate]).
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

	// Send limit for outgoing FromKeeper (chiefly an ApplyRequest carrying a
	// batch of RenderedTask). Configurable, default 8 MiB. Must be ≤ the
	// Soul recv limit (`keeper.max_apply_size_mb` in soul.yml); on overflow
	// Keeper fails fast with a clear ResourceExhausted error instead of
	// sending the Soul a message it would silently reject. The recv limit
	// for incoming FromSoul is a separate internal invariant
	// (eventStreamMaxRecvMsgSize, 1 MiB).
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

// Start is a blocking listener startup. Semantics identical to
// [BootstrapServer.Start]: on ctx.Done() — GracefulStop with a
// [graceDuration] timeout, exceeding it → forced Stop.
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

// Addr returns the actual bind address. After Start, this is the actual
// port (important for tests using `:0`).
func (s *EventStreamServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// eventStreamHandler is the [keeperv1.KeeperServer] implementation for the
// EventStream listener.
//
// Ping/Bootstrap don't make sense on this listener (server-only TLS
// handshake is impossible with an mTLS config, and the client has no
// SoulSeed before onboarding), but the embedded
// [keeperv1.UnimplementedKeeperServer] returns Unimplemented automatically —
// no separate handling needed.
type eventStreamHandler struct {
	keeperv1.UnimplementedKeeperServer
	deps          EventStreamDeps
	logger        *slog.Logger
	lastSeenFlush *lastSeenFlusher

	// augurSem is a global semaphore limiting parallel Augur processing
	// across ALL streams (the handler is a single instance per server).
	// Each AugurRequest spawns a goroutine (vault/prom/elk fetch can take
	// time and must not block the receive loop); without a limit, an
	// untrusted Soul flooding AugurRequests would exhaust Keeper's
	// goroutines/connections — a DoS vector. Overflow → AugurReply{ERROR},
	// with no new spawn (non-blocking acquire). nil → limit disabled (old
	// behavior, dev/unit without Augur).
	augurSem chan struct{}

	// fetchInflight is the per-SID inflight limit for FetchModule (protects
	// the control plane from a flood of fetch streams). Zero-value usable
	// (lazy map).
	fetchInflight sidInflight

	// soulLeaseOwner / instanceAlive are seams for the presence-gated
	// force-release of a SID lease (ADR-027 amend (n)) in
	// [acquireSoulLease]. By default they are the direct
	// [keeperredis.SoulLeaseOwner] / [keeperredis.InstanceAlive]; swapped
	// out in guard tests that need to reproduce an owner-change race and a
	// Redis flap specifically on the presence check (miniredis has no way
	// to inject an error into a single command). Not a public surface —
	// exists to let a test pin down the force-release security invariants.
	soulLeaseOwner func(context.Context, *keeperredis.Client, string) (string, bool, error)
	instanceAlive  func(context.Context, *keeperredis.Client, string) (bool, error)
}

// defaultAugurConcurrency is the default limit on parallel Augur processing
// (global, across all streams). Chosen as a moderate value: live fetch is an
// infrequent operation in a run, but vault+prom+elk together under a Soul
// flood are dangerous without a limit. Production wire-up can override via
// EventStreamDeps.AugurConcurrency.
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
		deps:           deps,
		logger:         logger,
		lastSeenFlush:  newLastSeenFlusher(flushInterval),
		augurSem:       augurSem,
		soulLeaseOwner: keeperredis.SoulLeaseOwner,
		instanceAlive:  keeperredis.InstanceAlive,
	}
}

// EventStream is the bidi Keeper↔Soul stream per ADR-012(a).
//
// M2.3 + M2.4 scope:
//   - handshake: Hello → HelloReply (as in M2.2);
//   - a Redis SoulLease per SID (`soul:<sid>:lock = <kid>`, TTL renewal in
//     a separate goroutine). Conflict → close stream with AlreadyExists;
//   - touch the heartbeat cache on every app message;
//   - dispatch FromSoul.payload → the TaskEvent / RunResult / SoulprintReport
//     handlers.
//
// SID comes from the interceptor context ([authenticatedSIDFrom]); the
// authoritative source is the peer cert (mTLS). `Hello.sid_echo` is for logs
// only; a mismatch is logged as a warning, not rejected (cf.
// docs/naming-rules.md → proto message table).
func (h *eventStreamHandler) EventStream(stream grpclib.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper]) error {
	sid, ok := authenticatedSIDFrom(stream.Context())
	if !ok {
		// The interceptor didn't run — a wire-up bug. Without it we don't
		// know who's on the other end.
		h.logger.Error("EventStream invoked without authenticated SID — interceptor misconfigured")
		return status.Error(codes.Internal, "authentication context missing")
	}

	// A per-stream ctx derived from the stream ctx, with its own cancel. The
	// rest of the handler (lease renewal, send loop, subscribe loop,
	// touchSeen, receive loop) runs on top of it. cancelStream is registered
	// with the StreamManager below so that Watchman (soul shedding S2) can
	// forcibly close the stream on sustained instance isolation
	// ([StreamManager.CloseAll]): the cancellation wakes the receive/send
	// loops (`ctx.Err() != nil` / `<-ctx.Done()`), the handler runs its OWN
	// regular teardown (Unregister → lease-Release LIFO), gRPC sends the
	// Soul an EOF, and the Soul moves to a live Keeper via its
	// reconnect-loop/failback list. A Watchman-driven cancellation is
	// indistinguishable, from the teardown's point of view, from a natural
	// stream break. defer cancelStream() is the final release of the ctx
	// resource (idempotent against a cancellation already triggered by
	// CloseAll).
	ctx, cancelStream := context.WithCancel(stream.Context())
	defer cancelStream()

	// The first message must be Hello (handshake phase).
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

	// Acquire the Redis lease on the SID. Per PM-decision (1): conflict →
	// close stream with AlreadyExists. Without Redis (dev build) — skip the
	// lease, single-instance mode.
	leaseCleanup, leaseErr := h.acquireSoulLease(ctx, sid)
	if leaseErr != nil {
		return leaseErr
	}
	defer leaseCleanup()

	// Presence (online) is now = a live Redis SID lease acquired above via
	// acquireSoulLease: it goes away on this teardown (Release in
	// leaseCleanup) or by TTL after a crash. There's no more synchronous PG
	// presence write on teardown (ADR-006(a) amend) — the target resolver
	// derives online from the lease, not from `souls.status`. Lazy
	// reconciliation of the PG status snapshot (for the Operator API's
	// "last known") is handled by the Reaper `mark_disconnected` rule (also
	// lease-aware), without regular closure depending on a write here.

	// StreamManager registration and the send loop (M2.5). Manager=nil →
	// no-outbound mode (handshake-only smoke / unit tests): skip
	// registration and the send loop, FromKeeper messages are not sent.
	//
	// Unregister must run BEFORE waiting on the send loop (see below),
	// because the send loop exits via close(outCh), which happens inside
	// Unregister. The defer chain is built so that:
	//   1) Unregister runs first (closes outCh, send loop exits);
	//   2) then <-sendDone (joins the goroutine);
	//   3) then leaseCleanup.
	// defer's LIFO semantics invert the order, so we declare them in
	// reverse (cleanup → join → unregister).
	//
	// cancelStream is registered together with the stream — it gives
	// Watchman a point for forced closure (see the derive above /
	// [StreamManager.CloseAll]).
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

	// Connect-time broadcast of plugin trust sigils (ADR-026, S6): right
	// after HelloReply and BEFORE the send loop starts — sent directly via
	// stream.Send in this same goroutine, so ordering is guaranteed
	// (HelloReply → all PluginSigil → app messages), with no race against
	// the send loop and no dependency on the outbound buffer size.
	// Best-effort: on any problem (Sigil off / empty registry / read error /
	// send failure) the stream keeps living — Soul-side verify (fail-closed)
	// protects against unsigned/unreceived plugins.
	h.broadcastSigils(ctx, stream, sid, sessionID)

	// Connect-time broadcast of the Sigil-signing trust-anchor set
	// (ADR-026(h), R3-S6): right after the sigil snapshot and in the same
	// goroutine — sent directly via stream.Send, ordering guaranteed
	// (HelloReply → SigilSnapshot → SigilTrustAnchors → app messages). This
	// way a freshly connected Soul gets the CURRENT anchor set (including
	// after a runtime rotation, S6 hot reload) to verify sigil signatures
	// against. Best-effort — see [broadcastTrustAnchors].
	h.broadcastTrustAnchors(ctx, stream, sid, sessionID)

	// Connect-time broadcast of the active Vigil set (ADR-030, beacons
	// circuit S2): in the same goroutine after the Sigil snapshots, BEFORE
	// the send loop starts — sent directly via stream.Send, ordering
	// guaranteed (HelloReply → SigilSnapshot → SigilTrustAnchors →
	// VigilSnapshot → app messages). ReplaceAll: the Soul scheduler replaces
	// its entire local Vigil set with this list (S1). Best-effort — see
	// [broadcastVigils].
	h.broadcastVigils(ctx, stream, sid, sessionID)

	// The stream counts as open after a successful handshake (HelloReply
	// delivered). Inc/Dec is the active-streams gauge; Dec in a defer
	// guarantees balance on any exit path from the handler.
	h.deps.Metrics.IncStreams()
	defer h.deps.Metrics.DecStreams()

	// On stream close, remove the SID from the flusher's throttle state so
	// the map doesn't accumulate disconnected Souls. The next connection of
	// the same SID will start with an immediate flush.
	defer h.lastSeenFlush.forget(sid)

	// Hello is also an app message: it refreshes the heartbeat (Redis) and
	// the throttled `last_seen_at` snapshot flush to PG. Presence (online)
	// is NOT written to PG on session-open: the authority is the Redis SID
	// lease acquired above (ADR-006(a)), and the target resolver derives
	// online from it. This removes the old HA blocker (a reconnected Soul
	// was invisible to the resolver because of a disconnected snapshot in
	// `souls`) without a synchronous presence write to PG — the lease is
	// already held, the Soul is visible immediately.
	h.touchSeen(ctx, sid)

	// Persist the announced capabilities (ADR-056 §S5) alongside presence —
	// the staged gate in run.go checks them BEFORE dispatch. Always written
	// (including an empty set from an old binary), by overwrite: otherwise
	// an old Soul reconnecting after a newer one would inherit a stale
	// "passage" flag. Best-effort (like the heartbeat): without Redis
	// (dev/unit) the gate degrades fail-closed to staged.
	if h.deps.Redis != nil {
		if err := keeperredis.SetSoulCapabilities(ctx, h.deps.Redis, sid, hello.GetCapabilities()); err != nil {
			h.logger.Debug("eventstream: persist soul capabilities failed",
				slog.String("sid", sid), slog.Any("error", err))
		}
	}

	h.logger.Info("eventstream: session opened",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.String("soul_version", hello.GetSoulVersion()),
		slog.Any("capabilities", hello.GetCapabilities()),
	)
	defer h.logger.Info("eventstream: session closed",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
	)

	// Send loop: drains outCh, sends via stream.Send. Only exists if a
	// StreamManager is wired up (outCh != nil). Exits on ctx.Done()
	// (graceful stop) or on outCh being closed (Unregister). A stream.Send
	// error → log + exit (the stream is already broken, the receive loop
	// will also hit EOF).
	sendDone := make(chan struct{})
	if outCh != nil {
		go h.runSendLoop(ctx, stream, sid, sessionID, outCh, sendDone)
	} else {
		close(sendDone)
	}

	// Cluster-mode subscribe loop: subscribes to the Redis pub/sub channel
	// `outbound:<sid>`, forwarding incoming FromKeeper messages to outCh
	// (Outbound on other Keeper instances publishes there when routing).
	// Only enabled when Manager and Redis are both non-nil — otherwise
	// cluster routing isn't needed (single-instance / unit tests).
	subDone := make(chan struct{})
	subCleanup := h.startOutboundSubscriber(ctx, sid, subDone)

	// LIFO defer chain: on handler return this fires in the order
	//   subCleanup (close pub/sub) → join subDone → Unregister →
	//   join sendDone → leaseCleanup (declared earlier).
	// This order guarantees:
	//   1) the subscriber stops writing to outCh before Unregister
	//      (otherwise entry.send on a closed channel returns false, dropped
	//      to the log);
	//   2) the send loop manages to drain outCh after Unregister;
	//   3) the lease is released last — another Keeper can pick it up right
	//      away.
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

	// The receive loop runs in a separate goroutine because `stream.Recv()`
	// blocks on the gRPC stream and does NOT react to our derived ctx being
	// cancelled (gRPC controls it, not us). Watchman shedding
	// ([StreamManager.CloseAll]) cancels cancelStream → the `<-ctx.Done()`
	// branch below fires → the handler returns → gRPC tears down the RPC
	// and closes the stream → the orphaned `stream.Recv()` in this goroutine
	// unblocks with an error and exits on its own (we do NOT join it
	// synchronously before returning: that would deadlock — Recv only
	// unblocks AFTER the stream closes, i.e. after the handler returns).
	// recvErr is buffered with size 1 so the receiver goroutine doesn't hang
	// on a send after the handler has already left via ctx.Done().
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

	// Exit on either of two events:
	//   - the receive loop finished (EOF / transport error / ctx
	//     cancellation noticed by Recv itself after it returns) — we return
	//     its terminal err;
	//   - ctx was cancelled externally (Watchman shedding on sustained
	//     isolation) — we exit WITHOUT an error: to the Soul this looks like
	//     EOF, and it reconnects to a live Keeper.
	// In both cases the defer chain performs the regular teardown
	// (subscriber → Unregister → send-loop join → lease-Release LIFO).
	//
	// Toll hook (ADR-038): notify the cluster detector on EVERY exit.
	// gracefulShutdown=true for the ctx.Done() branch (closure initiated by
	// the keeper instance itself — Watchman shedding or a graceful keeper
	// shutdown); false for the recvErr branch (client-side disconnect /
	// transport error). Toll itself filters out graceful closes (that's not
	// churn). Coven is unknown at this point (resolving it would cost a
	// PG query on every disconnect) — we pass an empty string; the
	// per-coven counter in [toll.Metrics] stays consistent (label="").
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

// notifyTollDisconnect is a best-effort hook into the Toll cluster detector
// (ADR-038). nil-safe: a no-op when Toll is disabled (deps.TollNotifier==nil).
// We strip ctx of its cancel (used only as a baggage carrier; the hook takes
// its own short timeout inside the Publisher).
func (h *eventStreamHandler) notifyTollDisconnect(ctx context.Context, sid string, gracefulShutdown bool) {
	if h.deps.TollNotifier == nil {
		return
	}
	// WithoutCancel: the stream ctx may already be cancelled (graceful
	// shutdown / shedding), but the Toll publish must still reach Redis. The
	// short timeout lives inside the Publisher (it's self-limited).
	hookCtx := context.WithoutCancel(ctx)
	h.deps.TollNotifier.NotifyDisconnect(hookCtx, sid, "", gracefulShutdown)
}

// runSendLoop is a goroutine that drives messages from the per-stream
// outbound channel into gRPC stream.Send. Started from [EventStream], exits
// on ctx.Done() or on outCh being closed (Unregister).
//
// stream.Send blocks the goroutine — this is natural back-pressure from the
// client. PM-decision M2.5(1) buffered=10: buffer overflow is signaled in
// Outbound.send (drop+log), so only already-accepted messages reach here.
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

// broadcastSigils hands the Soul the FULL active set of plugin trust sigils
// as a single [keeperv1.SigilSnapshot] (ADR-026(h), Option A: the snapshot
// is the sole source of truth for the active set, applied as ReplaceAll).
// Called from [EventStream] in the same goroutine between HelloReply and the
// start of the send loop, so it sends directly via stream.Send (ordering
// guaranteed, the buffer isn't involved).
//
// The connect-time snapshot is mandatory even for an empty registry: an
// empty sigils[] means "no plugin is allowed", and the Soul's ReplaceAll
// brings its cache to that state (important for near-instant revoke on
// reconnect). The one exception is SigilStore=nil (Sigil disabled / dev /
// unit): then no snapshot is sent at all, so as not to force an empty set
// onto hosts with Sigil disabled.
//
// Best-effort:
//   - SigilStore=nil → no-op, no error;
//   - ListActive returned an error → warn, snapshot skipped, stream stays
//     alive (Soul-side verify fail-closed protects anyway);
//   - stream.Send failed → warn (the stream is already broken, the receive
//     loop will hit EOF too).
//
// PluginSigil.Manifest inside the snapshot is rec.ManifestRaw (the
// byte-exact signed bytes, M1) via [sigilRecordToProto]: the re-hash on the
// Soul side runs over exactly these bytes via NormalizeManifestBytes
// (S3↔S6 invariant).
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

// broadcastTrustAnchors hands the Soul the CURRENT set of Sigil-signing
// trust anchors as a single [keeperv1.SigilTrustAnchors] (ADR-026(h), R3-S6,
// ReplaceAll). Called from [EventStream] in the same goroutine after
// [broadcastSigils] and before the send loop starts, so it sends directly
// via stream.Send (ordering guaranteed, the buffer isn't involved).
//
// The set is taken "live" from [TrustAnchorSource] rather than from a slice
// fixed at startup: after a runtime signing-key rotation (S6 hot reload
// swaps the Signer and updates the holder), a freshly connected Soul gets
// the current set.
//
// Best-effort:
//   - TrustAnchors=nil → no-op, no error (Sigil disabled / dev / unit);
//   - an empty set is still sent (empty = "no anchors" → the Soul clears its
//     holder, fail-closed verify via no_trust_anchor — important for
//     near-instant retire on reconnect, symmetric with an empty
//     SigilSnapshot);
//   - stream.Send failed → warn (the stream is already broken, the receive
//     loop will hit EOF too).
func (h *eventStreamHandler) broadcastTrustAnchors(
	ctx context.Context,
	stream grpclib.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper],
	sid, sessionID string,
) {
	_ = ctx // the set's source is synchronous (atomic holder); ctx is kept for signature symmetry
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

// broadcastVigils hands the Soul the active Vigil set as a single
// [keeperv1.VigilSnapshot] (ADR-030, ReplaceAll — same pattern as
// [broadcastSigils]/[broadcastTrustAnchors]). Called from [EventStream] in
// the same goroutine after the Sigil snapshots and before the send loop
// starts, so it sends directly via stream.Send (ordering guaranteed, the
// buffer isn't involved).
//
// The set is resolved from the host's covens (authoritative, from the souls
// registry) and its SID. The connect-time snapshot is mandatory even for an
// empty set: an empty vigils[] means "no active checks", and the Soul's
// ReplaceAll brings the scheduler to that state (this is how a Vigil
// disable/removal takes effect on reconnect). Exception — VigilSource=nil
// (Oracle disabled / dev / unit): then no snapshot is sent at all.
//
// Best-effort:
//   - VigilSource=nil → no-op, no error;
//   - the resolve returned an error → warn, snapshot skipped, stream stays
//     alive;
//   - stream.Send failed → warn (the stream is already broken, the receive
//     loop will hit EOF too).
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

// sigilRecordToProto projects a plugin_sigils registry record into the
// transport [keeperv1.PluginSigil]. Manifest = rec.ManifestRaw (the
// byte-exact signed bytes, M1), NOT rec.Manifest (the JSONB projection): the
// re-hash on the Soul side runs over exactly these bytes via
// NormalizeManifestBytes (S3↔S6 invariant). Shared by the connect-time
// broadcast ([broadcastSigils]) and the cluster re-broadcast
// ([Outbound.RebroadcastSigils], S6c) — a single mapping point.
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

// SigilRecordsToProto converts an active set of plugin_sigils registry
// records into transport [keeperv1.PluginSigil] values for the cluster
// re-broadcast (S6c). Exported for wire-up in `keeper run` (the daemon
// gathers the set from [SigilStore.ListActive] and hands it to
// [Outbound.RebroadcastSigils]).
func SigilRecordsToProto(recs []*sigil.Sigil) []*keeperv1.PluginSigil {
	out := make([]*keeperv1.PluginSigil, 0, len(recs))
	for _, rec := range recs {
		out = append(out, sigilRecordToProto(rec))
	}
	return out
}

// acquireSoulLease acquires the lease and starts the renewal goroutine.
// Returns a cleanup function that the caller invokes via defer.
//
// With Redis=nil (dev / unit build) the lease is disabled — returns a no-op
// cleanup. On ErrLeaseTaken → AlreadyExists (PM-decision 1).
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
			// A competitor holds the lease. Before handing the Soul
			// AlreadyExists (it reconnects after ~TTL), try a
			// presence-gated force-release against a PROVABLY-DEAD
			// prev-holder (ADR-027 amend (n)): after a holder is SIGKILLed,
			// its lease hangs around until TTL (~60s), blocking a reconnect
			// of the same SID to another keeper and blocking
			// dispatched-orphan reconciliation. This is NOT a blind DEL —
			// we only take over once Conclave confirms the owner's death;
			// otherwise (alive / indeterminate / our own lease) — the old
			// AlreadyExists behavior (split-brain guard / fail-safe).
			forced, ferr := h.tryForceAcquireDeadLease(ctx, sid, ttl)
			if ferr != nil {
				return nil, ferr
			}
			if forced != nil {
				lease = forced
			} else {
				return nil, status.Errorf(codes.AlreadyExists,
					"soul lease held by another keeper for sid=%q", sid)
			}
		} else {
			h.logger.Warn("eventstream: soul lease acquire failed",
				slog.String("sid", sid), slog.Any("error", err))
			return nil, status.Errorf(codes.Unavailable, "lease acquire failed: %v", err)
		}
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
		// Release with a detached ctx: the stream ctx may already be
		// cancelled (that's the normal graceful-stop path). WithoutCancel:
		// keep the trace baggage, don't inherit the teardown path's cancel.
		relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer relCancel()
		if err := lease.Release(relCtx); err != nil {
			h.logger.Warn("eventstream: soul lease release failed",
				slog.String("sid", sid), slog.Any("error", err))
		}
	}, nil
}

// tryForceAcquireDeadLease is a presence-gated takeover of a SID lease from
// a provably-dead prev-holder (ADR-027 amend (n), recovery backstop S2).
// Called from [acquireSoulLease] ONLY after a regular AcquireSoulLease
// returned ErrLeaseTaken.
//
// Addresses the "stale SID-lease ~60s" finding: after a holder is
// SIGKILLed, its lease hangs around until TTL, and a reconnect of the same
// SID to another keeper would get AlreadyExists (the stream closed before
// Hello → dispatched-orphan reconciliation unreachable). This is a targeted
// takeover, NOT a blind DEL: ownership is only pulled from whichever holder
// Conclave presence confirmed dead.
//
// Returns:
//   - (lease, nil) — force-release succeeded, lease is on our own KID, audit
//     emitted;
//   - (nil, nil)   — force NOT applied (prev is alive / it's ourselves /
//     indeterminate / a race changed the key): the caller hands the Soul
//     AlreadyExists (split-brain guard);
//   - (nil, err)   — a terminal gRPC error (doesn't occur on current
//     branches, reserved for future fatal conditions; currently always
//     nil-err).
func (h *eventStreamHandler) tryForceAcquireDeadLease(ctx context.Context, sid string, ttl time.Duration) (*keeperredis.Lease, error) {
	prevKID, ok, err := h.soulLeaseOwner(ctx, h.deps.Redis, sid)
	if err != nil {
		// Can't determine the owner — fail-safe: don't take over.
		h.logger.Warn("eventstream: soul lease owner lookup failed — yielding AlreadyExists",
			slog.String("sid", sid), slog.Any("error", err))
		return nil, nil
	}
	if !ok || prevKID == "" {
		// The key disappeared between Acquire and GET (TTL just expired).
		// Don't force it: the caller returns AlreadyExists, the Soul
		// retries, and a regular Acquire will succeed. No retry-Acquire
		// here — avoids any risk of a loop.
		return nil, nil
	}
	if prevKID == h.deps.KID {
		// Reconnect to the same keeper / our own lease — not a false takeover.
		h.logger.Warn("eventstream: soul lease still held by self — yielding AlreadyExists (reconnect race / own lease)",
			slog.String("sid", sid), slog.String("kid", h.deps.KID))
		return nil, nil
	}

	alive, err := h.instanceAlive(ctx, h.deps.Redis, prevKID)
	if err != nil {
		// The presence check failed — fail-safe: do NOT declare it dead, do
		// NOT take over.
		h.logger.Warn("eventstream: prev-holder presence check failed — yielding AlreadyExists (fail-safe)",
			slog.String("sid", sid), slog.String("prev_kid", prevKID), slog.Any("error", err))
		return nil, nil
	}
	if alive {
		// A live holder, or a partition with a live Conclave — let the Soul
		// retry.
		h.logger.Warn("eventstream: soul lease held by live keeper — yielding AlreadyExists",
			slog.String("sid", sid), slog.String("prev_kid", prevKID))
		return nil, nil
	}

	lease, ferr := keeperredis.ForceAcquireSoulLease(ctx, h.deps.Redis, sid, prevKID, h.deps.KID, ttl)
	if ferr != nil {
		if errors.Is(ferr, keeperredis.ErrLeaseTaken) {
			// Race: between the presence check and the CAS, the key changed
			// to a third party (TTL expired / another keeper got there
			// first). We do NOT take over someone else's fresh lease —
			// fall back to AlreadyExists, no retry (no risk of a loop).
			h.logger.Warn("eventstream: force-release lost race — yielding AlreadyExists",
				slog.String("sid", sid), slog.String("prev_kid", prevKID))
			return nil, nil
		}
		// Some other Redis error from the force-CAS — fail-safe: don't take
		// over.
		h.logger.Warn("eventstream: force-release failed — yielding AlreadyExists",
			slog.String("sid", sid), slog.String("prev_kid", prevKID), slog.Any("error", ferr))
		return nil, nil
	}

	h.logger.Info("eventstream: SID-lease force-released from dead prev-holder",
		slog.String("sid", sid),
		slog.String("prev_kid", prevKID),
		slog.String("new_kid", h.deps.KID),
	)
	h.auditLeaseForceReleased(ctx, sid, prevKID)
	return lease, nil
}

// auditLeaseForceReleased writes a security event for a SID-lease ownership
// takeover (ADR-027 amend (n)) via the same [audit.Writer] path as the
// Outbound forwarder (`source: soul_grpc`). Best-effort: the lease is
// already re-acquired, an audit failure doesn't roll back the recovery
// (same pattern as Outbound apply.dispatched).
func (h *eventStreamHandler) auditLeaseForceReleased(ctx context.Context, sid, prevKID string) {
	if err := h.deps.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventLeaseForceReleased,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: sid,
		Payload: map[string]any{
			"sid":      sid,
			"prev_kid": prevKID,
			"new_kid":  h.deps.KID,
		},
	}); err != nil {
		h.logger.Warn("eventstream: audit lease_force_released failed (lease already re-acquired)",
			slog.String("sid", sid),
			slog.String("prev_kid", prevKID),
			slog.Any("error", err))
	}
}

// renewLeaseLoop is the periodic renewer. On ErrLeaseLost it closes done;
// the main receive loop exits on its own via EOF/Canceled, because lease
// loss is usually accompanied by a stream timeout or an explicit
// cancellation. We do NOT force-interrupt the receive loop — let the Soul
// drain its buffer and close the stream normally (or let another Keeper,
// having already taken over this SID, signal it directly).
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

// touchSeen refreshes the heartbeat on every app message. Two layers:
//
//  1. The Redis heartbeat cache (fast layer, real-time): HSET on every
//     message. Errors aren't fatal — the heartbeat is best-effort, a missed
//     tick is made up for by the next message.
//  2. PG `souls.last_seen_at` (a snapshot for Reaper / the Operator API):
//     a throttled flush, no more than once per [lastSeenFlusher.interval]
//     per SID. Without it, `mark_disconnected` would falsely mark a live
//     stream disconnected (it only looks at PG-`last_seen_at`, see
//     ADR-006(a) and migration 014). UpdateLastSeen runs outside the stream
//     ctx: the stream can be cancelled at the exact moment of the flush
//     (graceful stop / lease handoff), and the snapshot is still useful —
//     otherwise the flush window would be lost on cancellation.
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

// flushLastSeen is a throttled `last_seen_at` snapshot to PG. No-op without
// SoulDB (dev / unit mode, heartbeat lives in Redis only). Throttled
// per-SID; the flush runs under its own background ctx so a stream-ctx
// cancellation at the moment of the flush doesn't lose the snapshot (a
// short 2s timeout guards against a hung PG).
func (h *eventStreamHandler) flushLastSeen(ctx context.Context, sid string, now time.Time) {
	if h.deps.SoulDB == nil {
		return
	}
	if !h.lastSeenFlush.shouldFlush(sid, now) {
		return
	}
	// WithoutCancel: keep the trace baggage for the last_seen PG write,
	// don't inherit the teardown path's cancel.
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := soul.UpdateLastSeen(flushCtx, h.deps.SoulDB, sid, h.deps.KID, now); err != nil {
		// ErrSoulNotFound is expected (the Soul was deleted, the stream is
		// still draining); other errors are best-effort, the next window
		// will retry.
		h.logger.Debug("eventstream: flush last_seen failed",
			slog.String("sid", sid), slog.Any("error", err))
	}
}

// dispatch is the dispatcher for FromSoul's oneof payload. A Hello after the
// first one (handshake) is logged as a warning and ignored;
// SeedRotationRequest is reserved for M2.6.
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
