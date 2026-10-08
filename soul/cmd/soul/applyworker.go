package main

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/soul/internal/augur"
	installmod "github.com/souls-guild/soul-stack/soul/internal/coremod/module"
	soulgrpc "github.com/souls-guild/soul-stack/soul/internal/grpc"
	"github.com/souls-guild/soul-stack/soul/internal/runtime"
)

var errNoStream = errors.New("soul: no stream to the Keeper")

// sessionLink is the stream an apply reaches the Keeper through mid-run — an
// Augur fetch, a module download. The apply may outlive the stream it arrived
// on, so it asks for the current one on every call.
type sessionLink struct {
	mu    sync.Mutex
	sess  *soulgrpc.StreamSession
	augur *augur.Client
}

func (l *sessionLink) set(sess *soulgrpc.StreamSession, ac *augur.Client) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sess, l.augur = sess, ac
}

// clear drops sess if it is still the current stream.
func (l *sessionLink) clear(sess *soulgrpc.StreamSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sess == sess {
		l.sess, l.augur = nil, nil
	}
}

func (l *sessionLink) Fetch(ctx context.Context, applyID, omen, query string) (*keeperv1.AugurReply, error) {
	l.mu.Lock()
	ac := l.augur
	l.mu.Unlock()
	if ac == nil {
		return nil, errNoStream
	}
	return ac.Fetch(ctx, applyID, omen, query)
}

func (l *sessionLink) FetchModule(ctx context.Context, req *keeperv1.PluginFetchRequest) (grpc.ServerStreamingClient[keeperv1.PluginChunk], error) {
	l.mu.Lock()
	sess := l.sess
	l.mu.Unlock()
	if sess == nil {
		return nil, errNoStream
	}
	return sess.FetchModule(ctx, req)
}

// applyWorker runs ApplyRequests one at a time (ADR-012(a)), off the session loop.
// A run is not tied to the stream it arrived on: when the stream drops, the loop
// goes back to reconnecting while the step goes on, and its events reach the
// Keeper through the relay on whichever stream comes next — the Keeper waits
// soul_timeout for exactly that (docs/scenario/settings.md §2.2). The session loop
// stays free to read, so a CancelApply reaches a running apply.
type applyWorker struct {
	runner *runtime.ApplyRunner
	relay  *runtime.Relay
	link   *sessionLink
	logger *slog.Logger

	mu    sync.Mutex
	queue []*keeperv1.ApplyRequest
	// current is the request taken off the queue, from before its Run registers
	// it until after the Run returns.
	current *keeperv1.ApplyRequest
	busy    bool
	wake    chan struct{}
	idle    chan struct{}
}

func newApplyWorker(runner *runtime.ApplyRunner, logger *slog.Logger) *applyWorker {
	w := &applyWorker{
		runner: runner,
		relay:  runtime.NewRelay(),
		link:   &sessionLink{},
		logger: logger,
		wake:   make(chan struct{}, 1),
		idle:   make(chan struct{}, 1),
	}
	runner.SetAlsoOwned(func() []runtime.OwnedApply {
		return append(w.queued(), w.relay.Undelivered()...)
	})
	return w
}

// attach makes sess the stream applies report through and reach the Keeper by,
// replaying what an earlier stream may have lost. Called after the stream's
// WardRoster, which declares those applies.
func (w *applyWorker) attach(sess *soulgrpc.StreamSession, ac *augur.Client) {
	w.link.set(sess, ac)
	w.relay.Attach(sess)
}

func (w *applyWorker) detach(sess *soulgrpc.StreamSession) {
	w.relay.Detach(sess)
	w.link.clear(sess)
}

// ended tells the relay how sess ended: a refusal counts against the events it
// carried.
func (w *applyWorker) ended(sess *soulgrpc.StreamSession, err error) {
	if size, ok := keeperRefused(err); ok {
		w.relay.Refused(sess, size)
	}
}

// refusedSize is grpc's refusal of a message over the receiving side's limit; the
// first number is the refused message's size.
var refusedSize = regexp.MustCompile(`received message larger than max \((\d+) vs\. \d+\)`)

// keeperRefused reports the size of a message too large for the Keeper to take,
// whose refusal ended the stream: grpc ends it with ResourceExhausted on its own,
// quoting the size. The code alone would not do — a Keeper message over its own
// send limit, or over the Soul's recv limit, ends the stream the same way — and the
// size names the event the refusal was for.
func keeperRefused(err error) (int, bool) {
	st := status.Convert(err)
	if st.Code() != codes.ResourceExhausted {
		return 0, false
	}
	m := refusedSize.FindStringSubmatch(st.Message())
	if m == nil {
		return 0, false
	}
	size, perr := strconv.Atoi(m[1])
	return size, perr == nil
}

// submit queues an accepted ApplyRequest.
func (w *applyWorker) submit(req *keeperv1.ApplyRequest) {
	w.relay.Settle(req.GetApplyId(), req.GetPassage())
	w.mu.Lock()
	w.queue = append(w.queue, req)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// cancel stops apply applyID: the running one through the runner, a queued one by
// reporting it cancelled without starting it.
func (w *applyWorker) cancel(applyID string) bool {
	w.mu.Lock()
	var dropped []*keeperv1.ApplyRequest
	kept := w.queue[:0]
	for _, req := range w.queue {
		if req.GetApplyId() == applyID {
			dropped = append(dropped, req)
			continue
		}
		kept = append(kept, req)
	}
	w.queue = kept
	w.mu.Unlock()
	for _, req := range dropped {
		_ = w.relay.SendRunResult(&keeperv1.RunResult{
			ApplyId: req.GetApplyId(),
			Status:  keeperv1.RunStatus_RUN_STATUS_CANCELLED,
			Attempt: req.GetAttempt(),
			Passage: req.GetPassage(),
		})
	}
	return w.runner.Cancel(applyID) || len(dropped) > 0
}

// queued lists the messages received whose Run has not registered them yet.
func (w *applyWorker) queued() []runtime.OwnedApply {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]runtime.OwnedApply, 0, len(w.queue)+1)
	if w.current != nil {
		out = append(out, runtime.OwnedApply{ApplyID: w.current.GetApplyId(), Passage: w.current.GetPassage()})
	}
	for _, req := range w.queue {
		out = append(out, runtime.OwnedApply{ApplyID: req.GetApplyId(), Passage: req.GetPassage()})
	}
	return out
}

// inFlight reports whether an apply runs or waits to.
func (w *applyWorker) inFlight() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busy || len(w.queue) > 0
}

// idleC fires once the worker has nothing left to run.
func (w *applyWorker) idleC() <-chan struct{} { return w.idle }

// applyDrainGrace bounds how long a shutting-down session waits for the running
// apply, already cancelled by the same signal, to report on it.
const applyDrainGrace = 10 * time.Second

// drain waits up to grace for the worker to have nothing in flight.
func (w *applyWorker) drain(grace time.Duration) {
	deadline := time.Now().Add(grace)
	for w.inFlight() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

// run executes queued applies until ctx ends.
func (w *applyWorker) run(ctx context.Context) {
	for {
		req, ok := w.next(ctx)
		if !ok {
			return
		}
		w.apply(ctx, req)
		w.mu.Lock()
		w.current = nil
		w.mu.Unlock()
	}
}

func (w *applyWorker) next(ctx context.Context) (*keeperv1.ApplyRequest, bool) {
	for {
		w.mu.Lock()
		if len(w.queue) > 0 {
			req := w.queue[0]
			w.queue = w.queue[1:]
			w.current = req
			w.busy = true
			w.mu.Unlock()
			return req, true
		}
		wasBusy := w.busy
		w.busy = false
		w.mu.Unlock()
		if wasBusy {
			select {
			case w.idle <- struct{}{}:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-w.wake:
		}
	}
}

func (w *applyWorker) apply(ctx context.Context, req *keeperv1.ApplyRequest) {
	// apply.run comes up as a child of the Keeper's grpc.apply_dispatch span
	// (ADR-024); an empty trace_context leaves it a root.
	applyCtx := otel.GetTextMapPropagator().Extract(ctx,
		propagation.MapCarrier{"traceparent": req.GetTraceContext()})
	// core.augur.fetch and core.module.installed reach the Keeper through the run's
	// context (ADR-025, ADR-065) — through whichever stream is current.
	applyCtx = augur.WithRun(applyCtx, w.link, req.GetApplyId())
	applyCtx = installmod.WithFetcher(applyCtx, w.link)
	if err := w.runner.Run(applyCtx, req, w.relay); err != nil {
		// Run refused the request before reporting anything; a step left without a
		// result would hold its barrier until scenario_timeout.
		w.logger.Error("apply: run failed", slog.String("apply_id", req.GetApplyId()), slog.Any("error", err))
		_ = w.relay.SendRunResult(&keeperv1.RunResult{
			ApplyId: req.GetApplyId(),
			Status:  keeperv1.RunStatus_RUN_STATUS_FAILED,
			Attempt: req.GetAttempt(),
			Passage: req.GetPassage(),
		})
	}
}
