package beacon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Scheduler is the per-process scheduler for the active Vigil set (ADR-030 S1).
//
// One Scheduler per soul daemon, living outside the reconnect loop (like
// ApplyRunner): the Vigil set and their last-state survive EventStream
// disconnects. Harmless with no Vigils — a no-op until the first VigilSnapshot.
//
// Per-Vigil lifecycle:
//   - on (re)start the scheduler runs Check every VigilDef.interval;
//   - the first check sets a baseline WITHOUT emitting a Portent (avoids a
//     storm on Soul restart / new Vigil);
//   - a later State change vs last → emits a Portent (edge-triggered);
//   - State unchanged → no-op;
//   - a Vigil removed from a new VigilSnapshot is stopped and its last-state
//     forgotten (disable/removal without restarting Soul).
//
// Ready Portents go on [Scheduler.Portents] — a buffered channel drained by
// the single EventStream writer (handleSession's select loop), which sends
// them on the session. The scheduler itself never holds a session or calls
// Send: StreamSession isn't concurrent-safe for Send (ADR-012), so all
// FromSoul messages come from one writer loop (like soulprintTick).
type Scheduler struct {
	registry BeaconLookup
	sid      string
	logger   *slog.Logger
	metrics  *BeaconMetrics

	// now / newTicker inject time for deterministic tests; production uses
	// time.Now / a real ticker.
	now       func() time.Time
	newTicker func(time.Duration) ticker

	portents chan *keeperv1.PortentEvent

	mu      sync.Mutex
	running map[string]*vigilRun // name → active Vigil run
	stopped bool
}

// vigilRun is one running Vigil: its goroutine + cancel.
type vigilRun struct {
	def    *keeperv1.VigilDef
	cancel context.CancelFunc
	done   chan struct{}
}

// ticker is a narrow abstraction over time.Ticker for test substitution.
type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

// SchedulerConfig holds [NewScheduler] parameters.
type SchedulerConfig struct {
	// Registry resolves beacons by name. Must not be nil. Production uses
	// [CompositeRegistry] (core + plugin); tests use a static [Registry] or a
	// fake Lookup.
	Registry BeaconLookup
	// SID is stamped into PortentEvent.sid (echo, ADR-012(i)).
	SID string
	// Logger — nil defaults to slog.Default.
	Logger *slog.Logger
	// PortentBuffer is the Portents channel capacity. 0 → defaultPortentBuffer.
	PortentBuffer int
	// Metrics is the soul_beacon_* descriptor (ADR-024 S4). nil disables
	// instrumentation (nil-safe Observe* methods are no-ops), same as
	// Metrics in ApplyRunner.
	Metrics *BeaconMetrics
}

const defaultPortentBuffer = 64

// NewScheduler builds a scheduler. Starts no Vigils — the set arrives via
// [Scheduler.Apply].
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	buf := cfg.PortentBuffer
	if buf <= 0 {
		buf = defaultPortentBuffer
	}
	return &Scheduler{
		registry:  cfg.Registry,
		sid:       cfg.SID,
		logger:    logger,
		metrics:   cfg.Metrics,
		now:       time.Now,
		newTicker: func(d time.Duration) ticker { return realTicker{t: time.NewTicker(d)} },
		portents:  make(chan *keeperv1.PortentEvent, buf),
		running:   make(map[string]*vigilRun),
	}
}

// Portents is the channel of ready Portent events, drained by the single
// EventStream writer (handleSession's select loop), which sends them on the
// current session. Per-process channel: survives reconnects, so an event
// raised between sessions isn't lost (buffered) and ships on the next session.
//
// nil-receiver-safe: a nil scheduler (test harness without a beacon setup)
// returns a nil channel — select blocks on it forever, meaning handleSession
// never sees a Portent.
func (s *Scheduler) Portents() <-chan *keeperv1.PortentEvent {
	if s == nil {
		return nil
	}
	return s.portents
}

// Apply applies a VigilSnapshot as ReplaceAll (ADR-030): the snapshot's set
// becomes the only active one.
//   - new (not in running) — started (baseline, no Portent);
//   - existing with the same check+interval+params — left as-is (last-state
//     kept, no restart needed);
//   - existing with a changed definition — restarted (new baseline);
//   - missing from the snapshot — stopped and forgotten.
//
// ctx is the daemon's parent context: its cancellation stops all Vigil goroutines.
func (s *Scheduler) Apply(ctx context.Context, defs []*keeperv1.VigilDef) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}

	wanted := make(map[string]*keeperv1.VigilDef, len(defs))
	for _, d := range defs {
		if d.GetName() == "" {
			s.logger.Warn("beacon: vigil без имени в snapshot пропущен")
			continue
		}
		wanted[d.GetName()] = d
	}

	// Stop Vigils absent from the new set or whose definition changed
	// (needs a restart with a new baseline).
	for name, run := range s.running {
		newDef, keep := wanted[name]
		if keep && sameVigil(run.def, newDef) {
			delete(wanted, name) // already running with current definition — leave it
			continue
		}
		s.stopRun(name, run)
	}

	// Start the rest (new + changed).
	for name, def := range wanted {
		s.startRun(ctx, name, def)
	}
}

// Stop stops all Vigils and moves the scheduler to a terminal state:
// subsequent Apply calls are no-ops. Idempotent. The Portents channel is NOT
// closed — its reader (handleSession) exits via its own ctx, and the producer
// closing the channel while a Send is in flight would risk a panic/race.
func (s *Scheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	for name, run := range s.running {
		s.stopRun(name, run)
	}
}

// stopRun (under s.mu) cancels the Vigil goroutine, waits for it to exit,
// then forgets the entry (and its last-state).
func (s *Scheduler) stopRun(name string, run *vigilRun) {
	run.cancel()
	<-run.done
	delete(s.running, name)
}

// startRun (under s.mu) starts the Vigil goroutine. An unknown check is
// logged and the Vigil isn't started — a bad registry on Keeper must not
// crash the scheduler.
func (s *Scheduler) startRun(ctx context.Context, name string, def *keeperv1.VigilDef) {
	b, ok := s.registry.Lookup(def.GetCheck())
	if !ok {
		s.logger.Warn("beacon: неизвестный check, vigil не запущен",
			slog.String("vigil", name),
			slog.String("check", def.GetCheck()),
		)
		return
	}
	interval, err := config.ParseDuration(def.GetInterval())
	if err != nil || interval <= 0 {
		s.logger.Warn("beacon: невалидный interval, vigil не запущен",
			slog.String("vigil", name),
			slog.String("interval", def.GetInterval()),
			slog.Any("error", err),
		)
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	run := &vigilRun{def: def, cancel: cancel, done: make(chan struct{})}
	s.running[name] = run
	go s.loop(runCtx, run, b, interval)
}

// loop is one Vigil's goroutine. Runs Check every interval; baseline on the
// first check without a Portent, edge-triggered after that.
func (s *Scheduler) loop(ctx context.Context, run *vigilRun, b Beacon, interval time.Duration) {
	defer close(run.done)

	tk := s.newTicker(interval)
	defer tk.Stop()

	var (
		last         State
		haveBaseline bool
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			state, data, err := b.Check(ctx, run.def.GetParams())
			if err != nil {
				// A check error != a host state change: leave baseline/last
				// alone, don't emit a Portent. Logged for diagnostics.
				s.logger.Warn("beacon: проверка завершилась ошибкой",
					slog.String("vigil", run.def.GetName()),
					slog.String("check", run.def.GetCheck()),
					slog.Any("error", err),
				)
				continue
			}
			if !haveBaseline {
				last = state
				haveBaseline = true
				s.logger.Debug("beacon: baseline установлен (без Portent)",
					slog.String("vigil", run.def.GetName()),
					slog.String("state", state),
				)
				continue
			}
			if state == last {
				continue // no-change
			}
			last = state
			s.emit(ctx, run.def, state, data)
		}
	}
}

// emit builds a Portent and enqueues it. Non-blocking send: if the buffer is
// full (writer loop lagging / no active session for a while), the event is
// dropped with a warn — otherwise the Vigil goroutine would stall and miss
// later transitions. Dropping an edge-triggered event loses one transition;
// the next State change raises a Portent again.
//
// V5-1 (ADR-030 amendment 2026-05-26): fills BOTH branches — Data (deprecated)
// and Payload (typed oneof, keyed by check address). Data is hard-cut after a
// 1-release deprecation period (S5-final). WARNs about dual-write once per process.
func (s *Scheduler) emit(ctx context.Context, def *keeperv1.VigilDef, state State, data *structpb.Struct) {
	ev := &keeperv1.PortentEvent{
		BeaconName:  def.GetName(),
		Data:        data,
		CollectedAt: timestamppb.New(s.now()),
		Sid:         s.sid,
	}
	fillTypedPayload(ev, def.GetCheck(), data)
	if ev.GetPayload() != nil {
		emitDeprecationWarnOnce(s.logger)
	}
	select {
	case s.portents <- ev:
		s.logger.Info("beacon: portent поднят (смена состояния)",
			slog.String("vigil", def.GetName()),
			slog.String("state", state),
		)
	case <-ctx.Done():
	default:
		s.metrics.ObservePortentDropped()
		s.logger.Warn("beacon: канал portent переполнен, событие отброшено",
			slog.String("vigil", def.GetName()),
			slog.String("state", state),
		)
	}
}

// sameVigil reports whether two Vigil definitions are equivalent (no restart
// needed): same check, interval, and params. Params comparison uses proto
// equivalence of Struct (see structEqual).
func sameVigil(a, b *keeperv1.VigilDef) bool {
	return a.GetCheck() == b.GetCheck() &&
		a.GetInterval() == b.GetInterval() &&
		structEqual(a.GetParams(), b.GetParams())
}

// structEqual reports proto equivalence of two Struct values (both nil →
// equal). Ensures a repeated VigilSnapshot with the same definition doesn't
// restart the Vigil (which would reset baseline and suppress a real event).
func structEqual(a, b *structpb.Struct) bool {
	return proto.Equal(a, b)
}
