// Package grpc is the Soul-side gRPC client for Keeper.
//
// Contents:
//   - [Client]: dial loop over prioritized endpoints with spray-shuffle
//     ([keeper.endpoints] in soul.yml). One Client makes one attempt to
//     establish a long-lived EventStream (ADR-002, ADR-012(a)).
//   - [StreamSession]: handshake (Hello/HelloReply) + send/recv loop over an
//     established bidi stream. Implements runtime.EventSink.
//
// Reconnect is the caller's job (the `soul run` loop in cmd/soul): on a Dial
// or Recv-loop error it retries with backoff. Keeps each step independently
// testable and responsibilities clear.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tlsx"
)

// Endpoint describes a Keeper instance from soul.yml. Addr is the
// EventStream address (`host:event_stream_port`, from
// SoulKeeperEndpoint.EventStreamAddr()). Priority drives failover order
// ([docs/soul/connection.md]): try priority 1 first, then 2; within a
// priority, random shuffle (spray).
type Endpoint struct {
	Addr     string
	Priority int
}

// ClientConfig — Client parameters.
type ClientConfig struct {
	// Endpoints — Keeper instances. Must not be empty.
	Endpoints []Endpoint
	// SeedCert / SeedKey — client SoulSeed (PEM paths on disk).
	SeedCert string
	SeedKey  string
	// CAPath — CA chain to verify Keeper's server cert.
	CAPath string
	// HandshakeTimeout — window for one dial + Hello/HelloReply.
	HandshakeTimeout time.Duration
	// SoulVersion — put into Hello.soul_version for audit.
	SoulVersion string
	// SID — put into Hello.sid_echo (authoritative source is the mTLS peer cert).
	SID string
	// MaxRecvMsgSize — cap on incoming FromKeeper size in bytes (mainly
	// ApplyRequest with a batch of RenderedTask). Applied as
	// grpc.MaxCallRecvMsgSize, replacing the small gRPC default (4 MiB). 0 →
	// [config.DefaultMaxApplySizeMB] (8 MiB). Source — soul.yml
	// `keeper.max_apply_size_mb` (config.SoulKeeper.ResolvedMaxApplySize).
	MaxRecvMsgSize int
	// MaxAttempts — dialOne retries against a SINGLE endpoint on a retriable
	// error (Unavailable/DeadlineExceeded/Internal/… see isRetriablePerEndpoint)
	// before spraying to the next endpoint. Source — soul.yml
	// `keeper.retry.max_attempts`. 0 → [defaultMaxAttempts] (resolved in NewClient).
	// The outer reconnectLoop is a separate retry layer between FULL passes
	// over the fallback list (ADR-002).
	MaxAttempts int
	// InterAttemptDelay — pause between attempts against one endpoint. FLAT
	// (no exponential growth — growth stays with the outer reconnectLoop).
	// Source — `keeper.retry.backoff.initial` (reused, no new config keys).
	// restart-required: fixed at Client construction and NOT re-read on
	// hot-reload (unlike reconnect backoff, which reconnectLoop reads from the
	// store per-iteration) — changing `backoff.initial` for inter-attempt
	// delay requires a soul restart.
	InterAttemptDelay time.Duration
	// InterAttemptJitter — whether to add ±25% jitter to InterAttemptDelay.
	// Source — `keeper.retry.backoff.jitter`.
	InterAttemptJitter bool
}

// defaultMaxAttempts — dialOne retries per endpoint when
// keeper.retry.max_attempts is omitted/zero. Conservative: one retry after
// the first retriable failure, then spray + the outer reconnectLoop.
const defaultMaxAttempts = 2

// Client — EventStream session manager.
type Client struct {
	cfg    ClientConfig
	logger *slog.Logger
}

// NewClient builds a Client and validates required fields.
func NewClient(cfg ClientConfig, logger *slog.Logger) (*Client, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("grpc: client: endpoints is empty")
	}
	if cfg.SeedCert == "" || cfg.SeedKey == "" || cfg.CAPath == "" {
		return nil, errors.New("grpc: client: SoulSeed (cert/key/ca) is incomplete — run `soul init` first")
	}
	if cfg.SID == "" {
		return nil, errors.New("grpc: client: SID is empty")
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.MaxRecvMsgSize <= 0 {
		cfg.MaxRecvMsgSize = config.DefaultMaxApplySizeMB * 1024 * 1024
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{cfg: cfg, logger: logger}, nil
}

// Dial establishes a gRPC connection and opens EventStream + handshake.
// Returns a StreamSession ready for send/recv. One attempt per endpoint;
// first success wins.
//
// On total failure returns an aggregated error detailing each endpoint —
// the caller (run-loop) backs off and retries.
func (c *Client) Dial(ctx context.Context) (*StreamSession, error) {
	return c.DialPriority(ctx, 0)
}

// DialPriority attempts a connection restricted to endpoints with priority
// strictly less than maxPriority. maxPriority=0 means "unrestricted"
// (equivalent to Dial). Used by the failback loop to try returning to a more
// preferred priority ([docs/soul/connection.md → Failback]).
//
// Returns the session and the chosen endpoint's actual priority via
// StreamSession.Priority(); with maxPriority>0, success means
// priority < maxPriority, otherwise error.
func (c *Client) DialPriority(ctx context.Context, maxPriority int) (*StreamSession, error) {
	tlsCfg, err := tlsx.LoadClientTLS(tlsx.ClientConfig{
		CertPath: c.cfg.SeedCert,
		KeyPath:  c.cfg.SeedKey,
		CAPath:   c.cfg.CAPath,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc: client: load mTLS: %w", err)
	}

	// gRPC keepalive: ping the server every 30s after 10s idle, keep the
	// connection alive even with no active stream (PermitWithoutStream).
	// Covers ADR-012 "no app-level heartbeat" — gRPC itself detects a dead
	// connection and closes the stream.
	kp := keepalive.ClientParameters{
		Time:                30 * time.Second,
		Timeout:             10 * time.Second,
		PermitWithoutStream: true,
	}

	var dialErrs []string
	// allLeaseHeld — every failed endpoint returned AlreadyExists (SID lease
	// still held by a live/unexpired holder, keeper/internal/grpc/eventstream.go).
	// Only meaningful once at least one endpoint was actually tried: with
	// dialErrs empty (no matching endpoints) the flag stays false.
	allLeaseHeld := true
	tried := false
	for _, ep := range orderedEndpoints(c.cfg.Endpoints) {
		if maxPriority > 0 && normalizedPriority(ep.Priority) >= maxPriority {
			continue
		}
		cfgForAddr := tlsCfg.Clone()
		if h, ok := hostFromAddr(ep.Addr); ok {
			cfgForAddr.ServerName = h
		}
		creds := credentials.NewTLS(cfgForAddr)

		tried = true
		// Per-endpoint retry (keeper.retry.max_attempts): retry dialOne against the SAME
		// endpoint on a retriable error (transport flake) before spraying to the
		// next one. Non-retriable (lease-held / auth / invalid) breaks immediately:
		// retrying the same endpoint is pointless, we need a different one. This is
		// the level between a single dialOne and the outer reconnectLoop (ADR-002, docs/soul/connection.md).
		var err error
		var sess *StreamSession
		for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
			sess, err = c.dialOne(ctx, ep.Addr, creds, kp)
			if err == nil {
				sess.priority = normalizedPriority(ep.Priority)
				c.logger.Info("eventstream: connected",
					slog.String("addr", ep.Addr),
					slog.Int("priority", sess.priority),
					slog.String("kid", sess.KID()),
					slog.String("session_id", sess.SessionID()),
				)
				return sess, nil
			}
			if !isRetriablePerEndpoint(err) {
				// ★ Regression guard (7af8e95): lease-held (AlreadyExists) is non-retriable,
				// so each lease-held endpoint gets EXACTLY one dialOne; allLeaseHeld
				// accumulates one attempt per endpoint.
				break
			}
			if attempt < c.cfg.MaxAttempts {
				// Debug, not Warn: an intermediate retry is expected noise; with a large
				// fallback list plus a network storm this shouldn't flood the Warn log.
				// The final "dial failed" (once attempts are exhausted) below is Warn.
				c.logger.Debug("eventstream: dial failed, retrying same endpoint",
					slog.String("addr", ep.Addr),
					slog.Int("attempt", attempt),
					slog.Int("max_attempts", c.cfg.MaxAttempts),
					slog.Any("error", err),
				)
				if !c.sleepInterAttempt(ctx) {
					// ctx canceled during the pause — bail out, result returned below.
					dialErrs = append(dialErrs, fmt.Sprintf("%s: %v", ep.Addr, ctx.Err()))
					return nil, fmt.Errorf("grpc: client: all endpoints failed:\n  - %s", strings.Join(dialErrs, "\n  - "))
				}
			}
		}
		// spray: AlreadyExists on one endpoint does NOT stop iteration — the next
		// endpoint may already have grabbed the lease after force-release. Clear
		// the flag as soon as any failure is NOT AlreadyExists (then it's a
		// transport failure — the general backoff cap, not the lease-held modest
		// cap).
		if !isAlreadyExists(err) {
			allLeaseHeld = false
		}
		c.logger.Warn("eventstream: dial failed", slog.String("addr", ep.Addr), slog.Any("error", err))
		dialErrs = append(dialErrs, fmt.Sprintf("%s: %v", ep.Addr, err))
	}
	if maxPriority > 0 && len(dialErrs) == 0 {
		// No endpoints with priority < maxPriority found — that's normal, not a
		// failback-loop error; distinguish it from "some exist but all failed".
		return nil, errNoHigherPriority
	}
	aggregated := fmt.Errorf("grpc: client: all endpoints failed:\n  - %s", strings.Join(dialErrs, "\n  - "))
	if tried && allLeaseHeld {
		// Every tried endpoint returned AlreadyExists — wrap with the errLeaseHeld
		// sentinel so reconnect-loop applies the modest cap (fast return after
		// presence expiry) without losing diagnostics in aggregated. status in the
		// per-endpoint wrappers is already erased (%v), so the signal travels via the sentinel.
		return nil, fmt.Errorf("%w: %w", errLeaseHeld, aggregated)
	}
	return nil, aggregated
}

// errNoHigherPriority — DialPriority sentinel error: no endpoints with
// priority < maxPriority. Used by the failback loop to distinguish "nothing
// to return to" from "all attempts failed".
var errNoHigherPriority = errors.New("grpc: client: no higher-priority endpoint")

// IsNoHigherPriority is the public selector for DialPriority's sentinel error.
func IsNoHigherPriority(err error) bool {
	return errors.Is(err, errNoHigherPriority)
}

// errLeaseHeld — Dial/DialPriority sentinel error: ALL tried endpoints
// rejected the handshake with gRPC AlreadyExists (SID lease still held by a
// live/unexpired holder, keeper/internal/grpc/eventstream.go::acquireSoulLease).
// Dial succeeded at the transport level but the session was rejected — a
// soft failure for backoff purposes: reconnect-loop applies a modest cap
// instead of the general transport cap, so Soul reconnects within seconds of
// presence expiry (force-release) instead of hammering surviving keepers or
// waiting out an inflated exponential cap.
var errLeaseHeld = errors.New("grpc: client: soul lease held by live keeper")

// IsLeaseHeld is the public selector for the errLeaseHeld sentinel error.
func IsLeaseHeld(err error) bool {
	return errors.Is(err, errLeaseHeld)
}

// isAlreadyExists reports whether err carries gRPC status codes.AlreadyExists
// anywhere in its chain (dialOne wraps the recv error via %w, status survives).
func isAlreadyExists(err error) bool {
	return status.Code(err) == codes.AlreadyExists
}

// isRetriablePerEndpoint decides whether to retry dialOne against the SAME
// endpoint (per-endpoint retry, keeper.retry.max_attempts) or spray to the
// next one right away. The matrix is normative (architect, docs/soul/connection.md):
//
//   - NOT retriable (retrying the same endpoint is pointless → break, spray on):
//     AlreadyExists (lease-held — another keeper holds the SID lease),
//     Unauthenticated / PermissionDenied (an auth problem won't self-heal),
//     InvalidArgument / FailedPrecondition / Unimplemented (contract rejection).
//   - retriable (transient transport flake — a retry may succeed):
//     Unavailable / DeadlineExceeded / Internal / Unknown / Aborted, plus a
//     local handshake timeout (not a gRPC status: dialOne returns a plain
//     fmt.Errorf, status.Code → codes.Unknown, falls into default → retriable).
//   - default (unclassified code) → retriable, conservatively.
func isRetriablePerEndpoint(err error) bool {
	switch status.Code(err) {
	case codes.AlreadyExists,
		codes.Unauthenticated,
		codes.PermissionDenied,
		codes.InvalidArgument,
		codes.FailedPrecondition,
		codes.Unimplemented:
		return false
	default:
		// Unavailable / DeadlineExceeded / Internal / Unknown / Aborted +
		// handshake timeout (codes.Unknown) + everything unclassified.
		return true
	}
}

// sleepInterAttempt waits out a flat InterAttemptDelay (±jitter) between
// attempts against one endpoint, interruptible via ctx. Returns true if the
// pause completed, false if ctx was canceled. Cap growth stays with the outer
// reconnectLoop — here the pause is flat (reuses backoff.initial).
func (c *Client) sleepInterAttempt(ctx context.Context) bool {
	d := c.cfg.InterAttemptDelay
	if c.cfg.InterAttemptJitter && d > 0 {
		delta := d / 4
		d = d + time.Duration(rand.Int64N(int64(delta*2))) - delta
	}
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// dialOne is a single attempt against one endpoint. The stream lives under
// its own session ctx (canceled in StreamSession.Close); handshake timeout is
// enforced locally via select-on-channel, otherwise a timeout-cancel would
// also kill the long-lived stream.
func (c *Client) dialOne(ctx context.Context, addr string, creds credentials.TransportCredentials, kp keepalive.ClientParameters) (*StreamSession, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(kp),
		// recv limit for incoming ApplyRequest (config.keeper.max_apply_size_mb).
		// The gRPC recv default (4 MiB) is too small for a large Destiny; leave
		// the client's send limit alone — Soul only sends small FromSoul
		// (TaskEvent/RunResult).
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(c.cfg.MaxRecvMsgSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	sessCtx, sessCancel := context.WithCancel(ctx)
	stream, err := keeperv1.NewKeeperClient(conn).EventStream(sessCtx)
	if err != nil {
		sessCancel()
		_ = conn.Close()
		return nil, fmt.Errorf("EventStream: %w", err)
	}

	hsDone := make(chan error, 1)
	var reply *keeperv1.HelloReply
	go func() {
		hello := &keeperv1.Hello{
			SidEcho:     c.cfg.SID,
			SoulVersion: c.cfg.SoulVersion,
			// Protocol feature announcement (ADR-056 §S5): keeper persists the set
			// alongside presence and checks it BEFORE dispatching a staged scenario.
			// Without "passage", this Soul is rejected fail-closed under N>1 Passage
			// instead of hanging.
			Capabilities: config.SoulCapabilities(),
		}
		if err := stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_Hello{Hello: hello}}); err != nil {
			hsDone <- fmt.Errorf("send Hello: %w", err)
			return
		}
		first, err := stream.Recv()
		if err != nil {
			hsDone <- fmt.Errorf("recv HelloReply: %w", err)
			return
		}
		r := first.GetHelloReply()
		if r == nil {
			hsDone <- fmt.Errorf("expected HelloReply, got %T", first.GetPayload())
			return
		}
		reply = r
		hsDone <- nil
	}()
	select {
	case err := <-hsDone:
		if err != nil {
			sessCancel()
			_ = conn.Close()
			return nil, err
		}
	case <-time.After(c.cfg.HandshakeTimeout):
		sessCancel()
		_ = conn.Close()
		return nil, fmt.Errorf("handshake timeout %s", c.cfg.HandshakeTimeout)
	}

	return &StreamSession{
		conn:       conn,
		stream:     stream,
		cancel:     sessCancel,
		kid:        reply.GetKid(),
		sessionID:  reply.GetSessionId(),
		serverTime: reply.GetServerTime().AsTime(),
		logger:     c.logger,
	}, nil
}

// orderedEndpoints returns endpoints sorted by priority (lower → earlier),
// with in-priority shuffle (spray, ADR-002). Does not mutate the input slice.
func orderedEndpoints(in []Endpoint) []Endpoint {
	out := make([]Endpoint, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		return normalizedPriority(out[i].Priority) < normalizedPriority(out[j].Priority)
	})
	// Spray: shuffle within each priority group.
	for i := 0; i < len(out); {
		j := i + 1
		pi := normalizedPriority(out[i].Priority)
		for j < len(out) && normalizedPriority(out[j].Priority) == pi {
			j++
		}
		if j-i > 1 {
			group := out[i:j]
			rand.Shuffle(len(group), func(a, b int) { group[a], group[b] = group[b], group[a] })
		}
		i = j
	}
	return out
}

func normalizedPriority(p int) int {
	if p == 0 {
		return 1
	}
	return p
}

// hostFromAddr extracts host from `host:port` for SNI / hostname verify.
// Duplicates a sibling helper in internal/bootstrap; not worth hoisting into
// shared for 6 lines.
func hostFromAddr(s string) (string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", false
	}
	host := s[:i]
	if strings.Contains(host, ":") {
		return "", false
	}
	return host, true
}

// StreamSession is an open Keeper↔Soul bidi stream with the handshake
// already complete. Implements runtime.EventSink (SendTaskEvent / SendRunResult).
//
// Concurrent Sends are serialized by the internal writeMu: a gRPC bidi
// stream doesn't allow concurrent Send without external synchronization. In
// the MVP most Sends come from a single writer (the handleSession select
// loop), but the Errand goroutine (ADR-033) is a parallel writer: apply may
// still be running while an Errand arrives. writeMu closes that race without
// restructuring the "one writer" architecture.
type StreamSession struct {
	conn       *grpc.ClientConn
	stream     grpc.BidiStreamingClient[keeperv1.FromSoul, keeperv1.FromKeeper]
	cancel     context.CancelFunc
	kid        string
	sessionID  string
	serverTime time.Time
	priority   int
	logger     *slog.Logger

	// writeMu serializes Send (ApplyRunner.SendTaskEvent / SendRunResult,
	// SendWardRoster, SendSoulprintReport, SendFromSoul, SendErrandResult).
	// The read loop (Recv) is a separate goroutine, no mutex needed. Recv and
	// Send on a gRPC bidi stream run concurrently by contract
	// (google.golang.org/grpc).
	writeMu sync.Mutex
}

func (s *StreamSession) KID() string       { return s.kid }
func (s *StreamSession) SessionID() string { return s.sessionID }
func (s *StreamSession) ServerTime() time.Time {
	return s.serverTime
}

// Priority is the endpoint priority the session was established on (after
// zero→1 normalization). 0 means the session was opened without priority
// tracking (legacy Dial).
func (s *StreamSession) Priority() int { return s.priority }

// Recv returns the next FromKeeper message. io.EOF is a normal close; any
// other error means the caller loop should reconnect.
func (s *StreamSession) Recv() (*keeperv1.FromKeeper, error) {
	msg, err := s.stream.Recv()
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	return msg, err
}

// SendTaskEvent sends a TaskEvent to Keeper. Satisfies runtime.EventSink.
func (s *StreamSession) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_TaskEvent{TaskEvent: ev}})
}

// SendRunResult sends a RunResult. Satisfies runtime.EventSink.
func (s *StreamSession) SendRunResult(r *keeperv1.RunResult) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_RunResult{RunResult: r}})
}

// SendFromSoul sends an arbitrary FromSoul on the stream. Needed by the
// Augur client (soul/internal/augur) to send AugurRequest — its payload isn't
// covered by the narrow Send* helpers. Serialized via writeMu (concurrent
// Send from the apply goroutine and the Errand goroutine is safe).
func (s *StreamSession) SendFromSoul(msg *keeperv1.FromSoul) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(msg)
}

// SendWardRoster sends a snapshot of tracked apply runs (ReplaceAll,
// Soul-reconcile ADR-027(g), S6). Called by the caller (handleSession) RIGHT
// AFTER handshake and BEFORE the first app message on every (re)connect: Keeper
// uses it to terminate orphaned dispatched rows for this SID. active=nil →
// WardRoster with an empty set (an explicit "nothing is tracked" declaration).
func (s *StreamSession) SendWardRoster(active []*keeperv1.ActiveApply) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_WardRoster{
			WardRoster: &keeperv1.WardRoster{Active: active},
		},
	})
}

// SendSoulprintReport is for the future soulprint collector (M2.3+).
// Added symmetrically with TaskEvent; cmd/soul doesn't call it yet.
// `received_at` is Keeper-only (ADR-018), not set here.
func (s *StreamSession) SendSoulprintReport(rep *keeperv1.SoulprintReport) error {
	if rep.GetCollectedAt() == nil {
		rep.CollectedAt = timestamppb.Now()
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_SoulprintReport{SoulprintReport: rep}})
}

// SendHostUtilization — снимок живой утилизации хоста (ADR-072). Зеркало
// SendSoulprintReport: `received_at` — Keeper-only, здесь не выставляется.
func (s *StreamSession) SendHostUtilization(u *keeperv1.HostUtilization) error {
	if u.GetCollectedAt() == nil {
		u.CollectedAt = timestamppb.Now()
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_HostUtilization{HostUtilization: u}})
}

// SendErrandResult sends the final ErrandResult to Keeper (ADR-033, slice E3).
// The Errand goroutine (recv-handler in cmd/soul) calls this in parallel with
// the apply goroutine, so writeMu is mandatory — concurrent Send on a gRPC
// bidi stream would desync the protocol. One ErrandRequest → one ErrandResult.
func (s *StreamSession) SendErrandResult(r *keeperv1.ErrandResult) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_ErrandResult{ErrandResult: r}})
}

// FetchModule opens a server-streaming fetch of a SoulModule plugin's bytes
// over the same mTLS ClientConn as EventStream (ADR-065(a)): the artifact
// travels on a separate HTTP/2 stream and doesn't choke the control plane.
// writeMu isn't needed — this is an independent RPC, not a Send on the bidi
// stream. Implements coremod/module.Fetcher.
func (s *StreamSession) FetchModule(ctx context.Context, req *keeperv1.PluginFetchRequest) (grpc.ServerStreamingClient[keeperv1.PluginChunk], error) {
	return keeperv1.NewKeeperClient(s.conn).FetchModule(ctx, req)
}

// Close cleanly ends the session: CloseSend → cancel ctx → conn.Close.
// Idempotent.
func (s *StreamSession) Close() error {
	if s.stream != nil {
		_ = s.stream.CloseSend()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}
