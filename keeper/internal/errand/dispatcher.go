package errand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// DefaultServerCap — sync-window timeout for `POST /v1/souls/{sid}/exec`
// (ADR-033 §3). If ErrandResult doesn't arrive in this time, handler does
// async-escalation (202 + Location), background-goroutine continues waiting
// until full req.TimeoutSec → ErrandStatus.TIMED_OUT.
const DefaultServerCap = 30 * time.Second

// MaxTimeoutSeconds — server-cap for full Errand timeout (ADR-033 §3).
// Any `timeout_seconds` value above this is clamped to this threshold
// at validation stage.
const MaxTimeoutSeconds = 300

// MinTimeoutSeconds — lower bound for timeout; below 1s loses meaning (even
// local shell-exec usually takes >100ms).
const MinTimeoutSeconds = 1

// DefaultTimeoutSeconds — default timeout if operator did not specify explicitly
// (ADR-033 §3, default 30s).
const DefaultTimeoutSeconds = 30

// TTLDefault — lifetime of `errands` row until purge_old_errands (ADR-033,
// reaper.md §purge_old_errands). 7 days. Purge implementation — slice E4.
const TTLDefault = 7 * 24 * time.Hour

// Sentinel errors of Dispatcher. Caller (HTTP/MCP-handler) maps to
// problem+json by type.
var (
	// ErrSIDEmpty — empty SID in DispatchRequest.
	ErrSIDEmpty = errors.New("errand: sid is empty")
	// ErrModuleEmpty — empty module in DispatchRequest.
	ErrModuleEmpty = errors.New("errand: module is empty")
	// ErrTimeoutOutOfRange — timeout outside [1, 300].
	ErrTimeoutOutOfRange = errors.New("errand: timeout out of range")
	// ErrSoulNotConnected — Soul not connected to any keeper instance
	// (empty lease-holder + no local stream). HTTP → 404 (logical
	// "target Soul unavailable"), like Outbound.
	ErrSoulNotConnected = errors.New("errand: soul not connected")
	// ErrErrandTerminal — attempt to cancel Errand already in terminal
	// status (success/failed/timed_out/cancelled/module_not_allowed). HTTP →
	// 409 Conflict (slice E5).
	ErrErrandTerminal = errors.New("errand: cannot cancel terminal errand")
	// ErrEmptyErrandID — empty errand_id in Cancel request (slice E5).
	// HTTP-handler filters this case earlier (path-param is required), but
	// sentinel remains for MCP/SDK calls.
	ErrEmptyErrandID = errors.New("errand: errand_id is empty")
)

// Status-kinds of applybus for Errand family. Registered in
// applybus.EventKind (see applybus/bus.go) — here only short-cut to
// string names, so dispatcher/handler don't depend on applybus-names
// both ways (any name change is visible in one file).
const (
	KindCompleted        = applybus.KindErrandCompleted
	KindFailed           = applybus.KindErrandFailed
	KindTimedOut         = applybus.KindErrandTimedOut
	KindCancelled        = applybus.KindErrandCancelled
	KindModuleNotAllowed = applybus.KindErrandModuleNotAllowed
)

// ResultEvent — payload published in applybus after receiving
// ErrandResult on Soul-side (events_errand.go) and read by Dispatcher
// (wait-loop). JSON-tagged — cluster-bridge serializes through Redis
// pub/sub envelope as json.RawMessage; local-bus passes `any`, and
// same type unpacks without decoding.
//
// Fields match proto ErrandResult (after mask+cap). Output is
// arbitrary map (read-safe modules); for shell/exec — nil.
type ResultEvent struct {
	ErrandID        string         `json:"errand_id"`
	Status          Status         `json:"status"`
	ExitCode        *int32         `json:"exit_code,omitempty"`
	Stdout          string         `json:"stdout,omitempty"`
	Stderr          string         `json:"stderr,omitempty"`
	StdoutTruncated bool           `json:"stdout_truncated,omitempty"`
	StderrTruncated bool           `json:"stderr_truncated,omitempty"`
	DurationMs      *int64         `json:"duration_ms,omitempty"`
	ErrorMessage    string         `json:"error_message,omitempty"`
	Output          map[string]any `json:"output,omitempty"`
}

// DispatchRequest — input for [Dispatcher.Dispatch]. Fields are validated by Dispatcher
// (caller need not duplicate). TimeoutSec=0 → DefaultTimeoutSeconds.
type DispatchRequest struct {
	SID          string
	Module       string
	Input        map[string]any
	TimeoutSec   int
	DryRun       bool
	StartedByAID string
}

// DispatchResult — output from [Dispatcher.Dispatch]. Async=true → caller returns
// HTTP 202 + {errand_id} + Location-header (sync-cap exceeded, result
// will be written by background-goroutine to DB and accessible via
// `GET /v1/errands/{errand_id}`).
type DispatchResult struct {
	ErrandID        string
	Status          Status
	ExitCode        *int32
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	DurationMs      *int64
	ErrorMessage    string
	Output          map[string]any
	Async           bool
	// StartedAt — time of errands-row INSERT (same `now` from Clock(),
	// recorded in Row.StartedAt). Single time source for persistent
	// row and sync-200 response; caller (handler) projects it to wire
	// `started_at` instead of fabricating time.Now().
	StartedAt time.Time
}

// OutboundSender — sending Errand messages to local EventStream of Soul.
// Narrow surface of keeper/internal/grpc.Outbound: dispatcher depends only
// on methods, not full Outbound (faked in tests).
//
// Local-only: returns ErrSoulNotConnected if no local stream.
// Cluster-routing is done by Publisher (see below) as separate call —
// Dispatcher chooses path by holder-KID lease.
type OutboundSender interface {
	SendErrand(ctx context.Context, sid string, req *keeperv1.ErrandRequest) error
	// SendCancelErrand — slice E5: sending CancelErrand message to local-stream
	// of SID. Returns ErrSoulNotConnected like SendErrand.
	SendCancelErrand(ctx context.Context, sid, errandID string) error
}

// RemotePublisher — publishing FromKeeper to `outbound:<sid>` (cluster-mode).
// Used by dispatcher when lease holder is NOT our KID (remote
// keeper). No channel names in this surface — routing-
// layer internals encapsulated in implementation (keeper/internal/grpc::Outbound).
type RemotePublisher interface {
	PublishErrand(ctx context.Context, sid string, req *keeperv1.ErrandRequest) error
	// PublishCancelErrand — slice E5: publishing CancelErrand to outbound:<sid>
	// pub/sub channel (cross-keeper).
	PublishCancelErrand(ctx context.Context, sid, errandID string) error
}

// LeaseLookup — resolves holder-KID lease for SID (`soul:<sid>:lock`).
// Returns "" if lease is absent (Soul not connected to any
// instance). Implementation — wrapper over redis.ReadSoulLeaseHolder.
type LeaseLookup interface {
	ReadHolder(ctx context.Context, sid string) (string, error)
}

// ApplyBus — narrow surface of applybus.EventBus, needed by Dispatcher.
// Sub/Pub by applyID semantics; in our case applyID = errand_id
// (channel name `apply:<id>` carries opaque-id, renaming to `events:<id>`
// is deferred TODO, see applybus/bus.go doc-comment).
type ApplyBus interface {
	Subscribe(ctx context.Context, applyID string) <-chan applybus.Event
	// SubscribeWithBridge — like Subscribe, but with explicit Redis-bridge control
	// (S1, applybus-bottleneck). wantBridge=false → local-only subscription without
	// per-applyID Redis-Subscribe; used when lease-holder of target
	// SID == self-KID (event comes from local publisher of same instance).
	SubscribeWithBridge(ctx context.Context, applyID string, wantBridge bool) <-chan applybus.Event
}

// AuditWriter — narrow surface of shared/audit.Writer (symmetric with pushorch).
type AuditWriter interface {
	Write(ctx context.Context, ev *audit.Event) error
}

// StoreAPI — narrow surface of Store, needed by Dispatcher. Narrowing
// (Insert/MarkTerminal/SweepOrphanRunning without read-methods List/Get) gives
// (a) inject in-memory fake in unit tests without bringing up PG; (b) explicit
// contract "dispatcher = only write-path for errands". Real *Store
// satisfies automatically.
type StoreAPI interface {
	Insert(ctx context.Context, row Row) error
	MarkTerminal(ctx context.Context, id string, upd TerminalUpdate) (bool, error)
	SweepOrphanRunning(ctx context.Context, kid string, grace time.Duration, reason string) ([]string, error)
	// Get — slice E5: read-row for Cancel (lookup SID + check status='running').
	// Returns ErrNotFound if missing.
	Get(ctx context.Context, errandID string) (*Row, error)
}

// Deps — external dependencies of Dispatcher. All fields except Audit/Clock
// are required; nil-Audit → audit-events are not written (diagnostics remain
// in logs). Clock nil → time.Now.
type Deps struct {
	Store       StoreAPI
	Outbound    OutboundSender
	Publisher   RemotePublisher
	LeaseLookup LeaseLookup
	ApplyBus    ApplyBus
	Logger      *slog.Logger
	Audit       AuditWriter
	Clock       func() time.Time
	ServerCap   time.Duration
	KID         string
}

// Dispatcher — synchronous orchestrator of one Errand. One Dispatch =
// one Errand. Concurrent-safe: state in DB + applybus pub/sub, no
// in-memory maps.
type Dispatcher struct {
	deps Deps
}

// NewDispatcher validates deps and returns dispatcher. Return error is
// misconfiguration of caller (wire-up).
func NewDispatcher(deps Deps) (*Dispatcher, error) {
	if deps.Store == nil {
		return nil, errors.New("errand: dispatcher Store is required")
	}
	if deps.Outbound == nil {
		return nil, errors.New("errand: dispatcher Outbound is required")
	}
	// Publisher / LeaseLookup are optional: single-keeper build without Redis
	// degrades to local-only routing (holder unknown → try local
	// Outbound, on missing stream — ErrSoulNotConnected).
	if deps.ApplyBus == nil {
		return nil, errors.New("errand: dispatcher ApplyBus is required")
	}
	if deps.Logger == nil {
		return nil, errors.New("errand: dispatcher Logger is required")
	}
	if deps.KID == "" {
		return nil, errors.New("errand: dispatcher KID is required")
	}
	if deps.ServerCap <= 0 {
		deps.ServerCap = DefaultServerCap
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	return &Dispatcher{deps: deps}, nil
}

// Dispatch — main flow. See package doc-comment (one Errand circuit).
//
// Steps:
//  1. Validate (sid/module/timeout), clamp TimeoutSec to [Min, Max].
//  2. Generate errand_id (ULID).
//  3. INSERT row(status='running', started_by_kid=self).
//  4. Write audit `errand.invoked`.
//  5. Subscribe applybus(`apply:<errand_id>`) — BEFORE sending, to not
//     miss event on quick Soul response.
//  6. Resolve holder → SendErrand local or Publish remote.
//  7. Wait sync until min(TimeoutSec, ServerCap):
//     - ResultEvent received → MarkTerminal → return sync.
//     - timeout = TimeoutSec (≤ServerCap) → MarkTerminal(timed_out) →
//     return sync with status=TIMED_OUT.
//     - timeout = ServerCap (<TimeoutSec) → spawn background goroutine
//     (waitAsync) and return async=true with status=RUNNING.
func (d *Dispatcher) Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	if err := validateDispatch(&req); err != nil {
		return DispatchResult{}, err
	}

	errandID := audit.NewULID()
	now := d.deps.Clock().UTC()
	row := Row{
		ErrandID:     errandID,
		SID:          req.SID,
		Module:       req.Module,
		Input:        req.Input,
		Status:       StatusRunning,
		StartedByAID: req.StartedByAID,
		StartedByKID: d.deps.KID,
		StartedAt:    now,
		TTLAt:        now.Add(TTLDefault),
	}
	if err := d.deps.Store.Insert(ctx, row); err != nil {
		return DispatchResult{}, fmt.Errorf("errand: insert: %w", err)
	}

	d.writeInvoked(ctx, errandID, req)

	// Resolve lease-holder BEFORE Subscribe (S1, applybus-bottleneck): if
	// holder == self-KID, event comes from local publisher of same instance
	// through local-bus → per-applyID Redis-bridge not needed. wantBridge=false
	// skips Redis-Subscribe for locally-connected Souls (common case),
	// eliminating maxclients-cliff. Conservative default wantBridge=true on
	// lookup error or missing LeaseLookup (holder unknown — bridge
	// as before). Event loss on holder-flip ≠ hang: in-process
	// wait-timer finishes Errand as timed_out (see select on timer.C below).
	holder, lookupOK := d.resolveHolder(ctx, req.SID, errandID)
	// `holder != ""` redundant when non-empty KID (holder==KID already non-empty), but
	// kept as guard against misconfig with empty KID: holder=="" && KID=="" should not
	// be treated as self → bridge remains enabled.
	wantBridge := !(lookupOK && holder == d.deps.KID && holder != "")

	// Subscribe BEFORE sending: pub/sub late-subscribe loses events (see
	// applybus.EventBus doc-comment). Subscription lifetime — until sync-wait
	// completion or async-goroutine ends.
	subCtx, subCancel := context.WithCancel(context.Background())
	events := d.deps.ApplyBus.SubscribeWithBridge(subCtx, errandID, wantBridge)

	if err := d.send(ctx, req.SID, errandID, req, holder, lookupOK); err != nil {
		subCancel()
		// Mark row failed: Errand didn't even reach Soul,
		// status=running would remain orphaned until sweep.
		_, mErr := d.deps.Store.MarkTerminal(ctx, errandID, TerminalUpdate{
			Status:       StatusFailed,
			ErrorMessage: err.Error(),
		})
		if mErr != nil {
			d.deps.Logger.Warn("errand: mark terminal after send-fail failed",
				slog.String("errand_id", errandID),
				slog.String("sid", req.SID),
				slog.Any("error", mErr))
		}
		d.writeTerminal(ctx, errandID, req, ResultEvent{
			ErrandID:     errandID,
			Status:       StatusFailed,
			ErrorMessage: err.Error(),
		})
		return DispatchResult{
			ErrandID:     errandID,
			Status:       StatusFailed,
			ErrorMessage: err.Error(),
			StartedAt:    now,
		}, err
	}

	timeoutDur := time.Duration(req.TimeoutSec) * time.Second
	syncCap := d.deps.ServerCap
	syncWait := timeoutDur
	if syncWait > syncCap {
		syncWait = syncCap
	}

	startedAt := d.deps.Clock()
	timer := time.NewTimer(syncWait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		// Caller-ctx cancelled (client disconnected / HTTP-
		// server shutdown). Don't block background-goroutine — it will read event
		// or expire by its timer; DB-row remains running until
		// it works. Subscription handed to goroutine.
		go d.waitAsync(subCtx, subCancel, events, errandID, req, syncWait, timeoutDur)
		return DispatchResult{ErrandID: errandID, Status: StatusRunning, Async: true, StartedAt: now}, nil
	case ev := <-events:
		subCancel()
		res := d.applyResult(ctx, errandID, req, ev, time.Since(startedAt))
		res.StartedAt = now
		return res, nil
	case <-timer.C:
		// Timer-elapsed: either sync hit timeout (TimeoutSec ≤ ServerCap),
		// or ServerCap arrived before TimeoutSec → async.
		if timeoutDur <= syncCap {
			// Sync-timeout = TimeoutSec → terminal timed_out.
			subCancel()
			res := d.markTimedOut(ctx, errandID, req, time.Since(startedAt))
			res.StartedAt = now
			return res, nil
		}
		// ServerCap < TimeoutSec → async-escalation. background-goroutine
		// will continue reading events until full TimeoutSec.
		remaining := timeoutDur - syncCap
		go d.waitAsync(subCtx, subCancel, events, errandID, req, syncCap, syncCap+remaining)
		return DispatchResult{ErrandID: errandID, Status: StatusRunning, Async: true, StartedAt: now}, nil
	}
}

// resolveHolder — best-effort read of lease-holder SID for choosing delivery
// path AND bridge decision (S1). Returns (holder, lookupOK):
//
//   - LeaseLookup nil / Publisher nil → ("", false): single-keeper local-only;
//     routing goes through Outbound without holder-branching.
//   - lookup error → ("", false): fallback to local (Outbound) +
//     conservative bridge (wantBridge=true).
//   - successful lookup → (holder, true): holder=="" means actually empty
//     lease (Soul not connected to any instance) → ErrSoulNotConnected
//     in send. holder=self → local, holder=other → remote.
//
// lookupOK distinguishes "no authoritative answer" (false → fallback to Outbound,
// like old lookup-error path) and "authoritative empty lease" (true,
// holder=="" → NotConnected without trying local). Result used twice:
// for wantBridge in Dispatch and as already-resolved holder in send (no second
// ReadHolder — eliminated double lookup).
func (d *Dispatcher) resolveHolder(ctx context.Context, sid, errandID string) (string, bool) {
	if d.deps.LeaseLookup == nil || d.deps.Publisher == nil {
		return "", false
	}
	holder, err := d.deps.LeaseLookup.ReadHolder(ctx, sid)
	if err != nil {
		d.deps.Logger.Warn("errand: lease lookup failed, fallback to local",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.Any("error", err))
		return "", false
	}
	return holder, true
}

// send chooses delivery path for ErrandRequest: local (Outbound.SendErrand) or
// remote (Publisher.PublishErrand) by already-resolved lease-holder
// (see [Dispatcher.resolveHolder]). Double ReadHolder eliminated — holder and
// lookupOK come as parameters.
//
// Algorithm:
//   - !lookupOK (LeaseLookup/Publisher nil OR lookup error) → local-only
//     fallback through Outbound. No stream → ErrSoulNotConnected.
//   - lookupOK && holder == "" → authoritative empty lease → Soul not connected
//     to any instance → ErrSoulNotConnected.
//   - lookupOK && holder == self → Local.
//   - lookupOK && holder == other → Remote (publisher).
//
// Outbound itself also checks holder inside SendApply/SendCancel — this is not
// race-free: holder could change between resolveHolder and Send, Outbound
// returns ErrSoulNotConnected, caller bubbles up (Errand fail).
func (d *Dispatcher) send(ctx context.Context, sid, errandID string, req DispatchRequest, holder string, lookupOK bool) error {
	pbReq, err := buildProtoRequest(errandID, req)
	if err != nil {
		return fmt.Errorf("errand: build proto: %w", err)
	}

	if !lookupOK {
		// Local-only / lookup-fallback: try directly through Outbound. If
		// no stream, Outbound returns ErrSoulNotConnected (we
		// rewrap it with our sentinel so caller doesn't depend on
		// grpc-package).
		if err := d.deps.Outbound.SendErrand(ctx, sid, pbReq); err != nil {
			d.deps.Logger.Warn("errand: local-only send failed",
				slog.String("sid", sid),
				slog.String("errand_id", errandID),
				slog.Any("error", err))
			return ErrSoulNotConnected
		}
		return nil
	}

	if holder == "" {
		return ErrSoulNotConnected
	}
	if holder == d.deps.KID {
		if err := d.deps.Outbound.SendErrand(ctx, sid, pbReq); err != nil {
			d.deps.Logger.Warn("errand: local send (holder=self) failed",
				slog.String("sid", sid),
				slog.String("errand_id", errandID),
				slog.Any("error", err))
			return ErrSoulNotConnected
		}
		return nil
	}
	if err := d.deps.Publisher.PublishErrand(ctx, sid, pbReq); err != nil {
		d.deps.Logger.Warn("errand: remote publish failed",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.String("holder", holder),
			slog.Any("error", err))
		return ErrSoulNotConnected
	}
	return nil
}

// waitAsync — continuation of wait-loop in background-goroutine after
// sync-escalation. ctxBus — background-ctx of applybus subscription; subCancel —
// cancel this subscription.
//
// elapsedAtSpawn — how much already elapsed at entry (for duration_ms
// at timeout). totalTimeout — full req.TimeoutSec in Duration.
func (d *Dispatcher) waitAsync(
	ctxBus context.Context, subCancel context.CancelFunc,
	events <-chan applybus.Event,
	errandID string, req DispatchRequest,
	elapsedAtSpawn, totalTimeout time.Duration,
) {
	defer subCancel()
	remaining := totalTimeout - elapsedAtSpawn
	if remaining <= 0 {
		// At boundary: full timeout already expired at spawn time. Go straight to TIMED_OUT.
		bg := context.Background()
		d.markTimedOut(bg, errandID, req, elapsedAtSpawn)
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	bg := context.Background()
	select {
	case ev, ok := <-events:
		if !ok {
			// Channel closed (subscription folded) — nothing more to wait for,
			// mark timed_out as defense-in-depth (running-row
			// otherwise hangs until sweep).
			d.markTimedOut(bg, errandID, req, elapsedAtSpawn)
			return
		}
		d.applyResult(bg, errandID, req, ev, elapsedAtSpawn)
	case <-timer.C:
		d.markTimedOut(bg, errandID, req, totalTimeout)
	case <-ctxBus.Done():
		// bus-ctx closed externally (only subCancel — but we schedule it ourselves).
		// Just in case — log+return without MarkTerminal.
		d.deps.Logger.Debug("errand: async wait ctx cancelled (external)",
			slog.String("errand_id", errandID))
	}
}

// applyResult accepts applybus.Event, normalizes payload (local-typed
// or cluster-RawMessage), and transitions errands-row to terminal.
// Returns DispatchResult for sync-caller (async variant ignores
// return — background goroutine).
func (d *Dispatcher) applyResult(
	ctx context.Context,
	errandID string, req DispatchRequest,
	ev applybus.Event, elapsed time.Duration,
) DispatchResult {
	res, ok := decodeResultEvent(ev.Payload)
	if !ok {
		d.deps.Logger.Warn("errand: result event payload decode failed",
			slog.String("errand_id", errandID),
			slog.String("kind", string(ev.Kind)))
		// Failsafe: transition to FAILED (instead of eternal running).
		res = ResultEvent{
			ErrandID:     errandID,
			Status:       StatusFailed,
			ErrorMessage: "errand: malformed result event payload",
		}
	}
	if res.ErrandID == "" {
		res.ErrandID = errandID
	}

	upd := TerminalUpdate{
		Status:          res.Status,
		ExitCode:        res.ExitCode,
		Stdout:          res.Stdout,
		Stderr:          res.Stderr,
		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
		DurationMs:      res.DurationMs,
		ErrorMessage:    res.ErrorMessage,
		Output:          res.Output,
	}
	if upd.DurationMs == nil {
		ms := elapsed.Milliseconds()
		upd.DurationMs = &ms
	}

	changed, err := d.deps.Store.MarkTerminal(ctx, errandID, upd)
	if err != nil {
		d.deps.Logger.Error("errand: mark terminal failed",
			slog.String("errand_id", errandID),
			slog.Any("error", err))
	}
	if changed {
		d.writeTerminal(ctx, errandID, req, res)
	}

	return DispatchResult{
		ErrandID:        errandID,
		Status:          res.Status,
		ExitCode:        res.ExitCode,
		Stdout:          res.Stdout,
		Stderr:          res.Stderr,
		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
		DurationMs:      upd.DurationMs,
		ErrorMessage:    res.ErrorMessage,
		Output:          res.Output,
		Async:           false,
	}
}

// markTimedOut — общий путь для sync-timeout и async-timeout.
func (d *Dispatcher) markTimedOut(ctx context.Context, errandID string, req DispatchRequest, elapsed time.Duration) DispatchResult {
	ms := elapsed.Milliseconds()
	upd := TerminalUpdate{
		Status:       StatusTimedOut,
		DurationMs:   &ms,
		ErrorMessage: fmt.Sprintf("errand timed out after %ds", req.TimeoutSec),
	}
	changed, err := d.deps.Store.MarkTerminal(ctx, errandID, upd)
	if err != nil {
		d.deps.Logger.Error("errand: mark timed_out failed",
			slog.String("errand_id", errandID),
			slog.Any("error", err))
	}
	if changed {
		d.writeTerminal(ctx, errandID, req, ResultEvent{
			ErrandID:     errandID,
			Status:       StatusTimedOut,
			DurationMs:   &ms,
			ErrorMessage: upd.ErrorMessage,
		})
	}
	return DispatchResult{
		ErrandID:     errandID,
		Status:       StatusTimedOut,
		DurationMs:   &ms,
		ErrorMessage: upd.ErrorMessage,
		Async:        false,
	}
}

// writeInvoked writes audit-event `errand.invoked` (ADR-033, event_types.go).
// Payload — sid/module/errand_id/timeout/dry_run; `input` is NOT put
// (may carry vault-resolved secrets).
func (d *Dispatcher) writeInvoked(ctx context.Context, errandID string, req DispatchRequest) {
	if d.deps.Audit == nil {
		return
	}
	ev := &audit.Event{
		EventType:     audit.EventTypeErrandInvoked,
		Source:        audit.SourceAPI,
		ArchonAID:     req.StartedByAID,
		CorrelationID: errandID,
		Payload: map[string]any{
			"sid":             req.SID,
			"module":          req.Module,
			"errand_id":       errandID,
			"timeout_seconds": req.TimeoutSec,
			"dry_run":         req.DryRun,
		},
	}
	if err := d.deps.Audit.Write(ctx, ev); err != nil {
		d.deps.Logger.Warn("errand: audit invoked failed",
			slog.String("errand_id", errandID),
			slog.Any("error", err))
	}
}

// writeTerminal writes audit-event for terminal. Status →
// EventType mapping:
//
//   - success                → errand.completed
//   - failed | module_not_allowed → errand.failed
//   - timed_out             → errand.timed_out
//   - cancelled             → errand.cancelled (slice E5)
//
// Source — soul_grpc (write-path comes from corresponding
// FromSoul.ErrandResult-handler; for send-fail / timed_out — Keeper-
// internal, but source=soul_grpc chosen for uniform event filter).
//
// archon_aid is NOT put in payload (matches contract in
// event_types.go: payload without archon_aid, initiator in colon field).
func (d *Dispatcher) writeTerminal(ctx context.Context, errandID string, req DispatchRequest, res ResultEvent) {
	if d.deps.Audit == nil {
		return
	}
	var eventType audit.EventType
	switch res.Status {
	case StatusSuccess:
		eventType = audit.EventTypeErrandCompleted
	case StatusTimedOut:
		eventType = audit.EventTypeErrandTimedOut
	case StatusCancelled:
		eventType = audit.EventTypeErrandCancelled
	default:
		// failed / module_not_allowed / неизвестное → errand.failed.
		eventType = audit.EventTypeErrandFailed
	}

	payload := map[string]any{
		"sid":       req.SID,
		"module":    req.Module,
		"errand_id": errandID,
	}
	if res.ExitCode != nil {
		payload["exit_code"] = *res.ExitCode
	}
	if res.DurationMs != nil {
		payload["duration_ms"] = *res.DurationMs
	}
	if res.StdoutTruncated {
		payload["stdout_truncated"] = true
	}
	if res.StderrTruncated {
		payload["stderr_truncated"] = true
	}
	if res.ErrorMessage != "" && eventType != audit.EventTypeErrandCompleted {
		payload["error_message"] = res.ErrorMessage
	}

	source := audit.SourceSoulGRPC
	// errand.cancelled initiated by archon via API → source=api with aid.
	// In current slice E2 cancel does not exist — event written only from
	// later slice E5; but if someone brings
	// status=cancelled here, mark source correctly.
	var aid string
	if res.Status == StatusCancelled {
		source = audit.SourceAPI
		aid = req.StartedByAID
	}

	ev := &audit.Event{
		EventType:     eventType,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: errandID,
		Payload:       payload,
	}
	if err := d.deps.Audit.Write(ctx, ev); err != nil {
		d.deps.Logger.Warn("errand: audit terminal failed",
			slog.String("errand_id", errandID),
			slog.String("event_type", string(eventType)),
			slog.Any("error", err))
	}
}

// validateDispatch checks and normalizes DispatchRequest in-place.
// TimeoutSec=0 → DefaultTimeoutSeconds; <Min / >Max → ErrTimeoutOutOfRange
// (caller returns 422). Empty fields → sentinel-errors.
func validateDispatch(req *DispatchRequest) error {
	if req.SID == "" {
		return ErrSIDEmpty
	}
	if req.Module == "" {
		return ErrModuleEmpty
	}
	if req.TimeoutSec == 0 {
		req.TimeoutSec = DefaultTimeoutSeconds
	}
	if req.TimeoutSec < MinTimeoutSeconds || req.TimeoutSec > MaxTimeoutSeconds {
		return ErrTimeoutOutOfRange
	}
	return nil
}

// decodeResultEvent unpacks applybus.Event.Payload into ResultEvent.
// Supports both formats:
//
//   - local Publish: payload is already ResultEvent (or map[string]any,
//     if published by SoulGRPC-handler via json.Marshal on cluster-
//     bridge → degrades to map on re-packing).
//   - cluster Subscribe: payload is json.RawMessage (see
//     keeper/internal/redis/applybus.go::ApplyEvent.Payload). Unmarshal
//     directly to ResultEvent.
//
// On unknown form returns ok=false → caller decides
// (failsafe in applyResult).
func decodeResultEvent(payload any) (ResultEvent, bool) {
	switch p := payload.(type) {
	case ResultEvent:
		return p, true
	case *ResultEvent:
		if p == nil {
			return ResultEvent{}, false
		}
		return *p, true
	case json.RawMessage:
		var out ResultEvent
		if err := json.Unmarshal(p, &out); err != nil {
			return ResultEvent{}, false
		}
		return out, true
	case []byte:
		var out ResultEvent
		if err := json.Unmarshal(p, &out); err != nil {
			return ResultEvent{}, false
		}
		return out, true
	case map[string]any:
		// fallback: payload came as generic-map (cross-keeper-route
		// after json.Marshal/Unmarshal without typed-target). Marshal-
		// unmarshal as ResultEvent.
		b, err := json.Marshal(p)
		if err != nil {
			return ResultEvent{}, false
		}
		var out ResultEvent
		if err := json.Unmarshal(b, &out); err != nil {
			return ResultEvent{}, false
		}
		return out, true
	default:
		return ResultEvent{}, false
	}
}
