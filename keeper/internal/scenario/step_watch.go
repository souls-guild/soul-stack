package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// soulUnavailable is the error_summary code of a host failed for unavailability
// (docs/scenario/settings.md §2.2): its agent stream stayed lost — or never came to
// send a step on — for longer than the step's soul_timeout. A failure tied to no task.
const soulUnavailable = "soul_unavailable"

// SoulPresenceChecker reports the SIDs whose agent stream some Keeper instance
// holds: the SID lease, which the holder releases when the stream ends and which
// expires when the holder dies — so the instance running a step learns of a drop on
// another instance without a message of its own. Implemented over
// [keeperredis.SoulsStreamAlive].
type SoulPresenceChecker interface {
	SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error)
}

// stepWatch holds a linear step to its hosts' agents: a host is sent the step only
// over a stream, waiting up to soul_timeout for one, and a host whose stream stays
// lost for longer while the step is out on it fails for unavailability. The wait
// and the loss are the connection timeout the user rule separates from a task's
// (settings.md §2.2); neither bounds how long the step itself runs.
//
// One per step, made by [Runner.newStepWatch]; nil under staged dispatch, whose
// dead host the run ceiling still catches.
type stepWatch struct {
	r           *Runner
	spec        RunSpec
	passage     int
	soulTimeout time.Duration
	log         *slog.Logger

	// mu orders a waiting host's send against stop: once stop returns, nothing of
	// this step goes out, so a step a failure ended reaches no host late.
	mu      sync.Mutex
	stopped bool
	done    chan struct{}
	waiters sync.WaitGroup
	// waiting counts the hosts whose wait has left them no row yet; under mu.
	waiting int

	// lostSince is when the barrier first saw a host's stream gone; read and
	// written by the barrier goroutine only.
	lostSince map[string]time.Time
}

func (r *Runner) newStepWatch(spec RunSpec, passage int, log *slog.Logger) *stepWatch {
	if spec.executionModel != ExecutionLinear {
		return nil
	}
	if r.deps.SoulPresence == nil {
		log.Warn("scenario: no agent presence source (Redis) - a host whose stream is lost is bounded by scenario_timeout only",
			slog.Int("passage", passage))
	}
	return &stepWatch{
		r:           r,
		spec:        spec,
		passage:     passage,
		soulTimeout: stepSoulTimeout(spec, passage),
		log:         log,
		done:        make(chan struct{}),
		lostSince:   make(map[string]time.Time),
	}
}

type stepDoneKey struct{}

// pushContext carries the step's end to a push host's goroutine: its SSH connect
// retries stop with the step, as a stream host's wait does. The context itself is
// not cancelled — a push session already under way runs to its end. nil-safe.
func (w *stepWatch) pushContext(ctx context.Context) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, stepDoneKey{}, (<-chan struct{})(w.done))
}

// stepDone is the end of the step ctx was made for by [stepWatch.pushContext];
// nil — never.
func stepDone(ctx context.Context) <-chan struct{} {
	done, _ := ctx.Value(stepDoneKey{}).(<-chan struct{})
	return done
}

// stop withdraws the step from every host still waiting for a stream and returns
// once none of them can send. nil-safe.
func (w *stepWatch) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		close(w.done)
	}
	w.mu.Unlock()
	w.waiters.Wait()
}

// withdraw stops the step and returns how many of its hosts were still waiting
// for a stream, which now get no row: a barrier counting the step's hosts must
// not wait for them. nil-safe.
func (w *stepWatch) withdraw() int {
	if w == nil {
		return 0
	}
	w.stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.waiting
}

// wait runs attempt from its own goroutine until the host is done ([stepWatch.await]).
func (w *stepWatch) wait(ctx context.Context, run *applyrun.ApplyRun, attempt func() (bool, error), onCancel func() error) {
	w.mu.Lock()
	w.waiting++
	w.mu.Unlock()
	w.waiters.Add(1)
	go func() {
		defer w.waiters.Done()
		if w.await(ctx, run, attempt, onCancel) {
			w.mu.Lock()
			w.waiting--
			w.mu.Unlock()
		}
	}()
}

// send hands sid its step over its agent stream (the stream branch of
// dispatchWave). The row is written `dispatched` together with the send, never
// ahead of a stream to carry it: on reconnect the WardRoster sweep orphans every
// dispatched row its Soul does not declare, which is how a step the agent lost —
// to a restart, or in transit — is noticed at all, and a row waiting for a stream
// is one no Soul could declare. A host with no stream waits in its own goroutine,
// so the hosts behind it in the wave are not held up; it counts toward the barrier,
// and the wait leaves it a row unless the step ended first — the barrier does not
// wait for it then: a Cancel ends the run — a row's flag at once, the lock's when
// [onlyDispatchedOpen] lets it — and a step a dispatch error ended counts it out
// ([stepWatch.withdraw]). A host known to have
// no stream goes straight to the wait: a row inserted to be withdrawn could meet the
// sweep of a Soul reconnecting meanwhile. records are written once the step is out.
func (w *stepWatch) send(ctx context.Context, run *applyrun.ApplyRun, req *keeperv1.ApplyRequest, records func()) error {
	if w.r.deps.SoulPresence == nil || w.present(ctx, run.SID) {
		done, err := w.trySend(ctx, run, req, records)
		if err != nil || done {
			return err
		}
	}
	w.wait(ctx, run, func() (bool, error) {
		// The version the agent announced on the connection this request travels.
		run.SoulVersion = w.r.announcedSoulVersion(ctx, run.SID, w.log)
		return w.trySend(ctx, run, req, records)
	}, func() error {
		// A row an earlier try failed to withdraw: left dispatched, with nothing on
		// the host to finish it, it would hold the deferred Cancel open.
		_, err := applyrun.WithdrawUnsent(ctx, w.r.deps.DB, run.ApplyID, run.SID, run.Passage)
		return err
	})
	return nil
}

// queue writes the planned row of a step the work-queue carries (NIM-961) for
// run's host — at once over a stream, or else once the host has one, as send does
// for an inline step: the claim that takes the row sends at once and fails a host
// with no stream. write inserts the row — late, from the wait, when the host had no
// stream at first; a host whose stream does not come within soul_timeout fails for
// unavailability with no row queued. nil-safe.
func (w *stepWatch) queue(ctx context.Context, run *applyrun.ApplyRun, write func(late bool) error) error {
	if w == nil || w.r.deps.SoulPresence == nil || w.present(ctx, run.SID) {
		return write(false)
	}
	w.log.Info("scenario: no agent stream to queue the step for - waiting for one",
		slog.String("sid", run.SID), slog.Int("passage", w.passage), slog.Duration("soul_timeout", w.soulTimeout))
	w.wait(ctx, run, func() (bool, error) { return true, write(true) }, nil)
	return nil
}

// trySend inserts the row and sends; true — the host needs nothing more. A send
// that found no stream withdraws the row and reports false, unless the row was
// closed meanwhile — by a sweep, as orphaned — which settles the host as it is.
func (w *stepWatch) trySend(ctx context.Context, run *applyrun.ApplyRun, req *keeperv1.ApplyRequest, records func()) (bool, error) {
	w.r.warnCrossKeeperDispatch(ctx, run.SID, w.log)
	row := *run
	row.Status = applyrun.StatusDispatched
	if err := applyrun.Insert(ctx, w.r.deps.DB, &row); err != nil {
		if !errors.Is(err, applyrun.ErrApplyRunAlreadyExists) {
			return false, fmt.Errorf("scenario: insert apply_run (%s): %w", run.SID, err)
		}
		// The row an earlier try failed to withdraw, the step unsent: take it back
		// and write it again with the send. Closed since — by a sweep — it settles
		// the host as it is.
		withdrawn, derr := applyrun.WithdrawUnsent(ctx, w.r.deps.DB, run.ApplyID, run.SID, run.Passage)
		if derr != nil {
			return false, derr
		}
		if !withdrawn {
			return true, nil
		}
		if err := applyrun.Insert(ctx, w.r.deps.DB, &row); err != nil {
			return false, fmt.Errorf("scenario: insert apply_run (%s): %w", run.SID, err)
		}
	}
	if serr := w.r.deps.Outbound.SendApply(ctx, run.SID, req); serr != nil {
		// Every SendApply failure is a missing or saturated stream — the request is
		// only queued here — so none of them is a reason to fail the host before
		// soul_timeout.
		w.log.Info("scenario: no agent stream to send the step on - waiting for one",
			slog.String("sid", run.SID), slog.Int("passage", w.passage), slog.Duration("soul_timeout", w.soulTimeout), slog.Any("error", serr))
		withdrawn, derr := applyrun.WithdrawUnsent(ctx, w.r.deps.DB, run.ApplyID, run.SID, run.Passage)
		if derr != nil {
			return false, derr
		}
		return !withdrawn, nil
	}
	records()
	w.log.Info("scenario: ApplyRequest sent", slog.String("sid", run.SID), slog.Int("tasks", len(req.GetTasks())))
	return true, nil
}

// await retries attempt until it reports the host done or soul_timeout runs out,
// which fails the host for unavailability. With a presence source it tries only
// over a stream that has been there for [presenceSettle]. A reconnected Soul's
// first message is its WardRoster, swept within milliseconds; the holder counts a
// request queued before the sweep as declared, but a row inserted before the sweep
// for a request queued after it would be orphaned while the Soul applies the step —
// waiting out the sweep is what keeps the insert after it. attempt runs under mu,
// so nothing of the step goes out once stop has returned, and not at all once the
// run is cancelled: a Cancel read from the lock waits while another host's row is
// dispatched, and a host sent the step meanwhile would apply it after the Cancel.
// It reports whether the host was left a row.
//
// onCancel, when set, runs before a Cancel ends the wait; if it fails, the wait tries
// again at its next attempt — with the stream gone meanwhile, a row it leaves is
// closed by [stepWatch.check] or failUnsent at soul_timeout.
func (w *stepWatch) await(ctx context.Context, run *applyrun.ApplyRun, attempt func() (bool, error), onCancel func() error) bool {
	deadline := time.Now().Add(w.soulTimeout)
	tick := time.NewTicker(w.r.pollInterval)
	defer tick.Stop()
	var presentSince time.Time
	for {
		select {
		case <-ctx.Done():
			return false
		case <-w.done:
			return false
		case <-tick.C:
		}
		now := time.Now()
		if !now.Before(deadline) {
			return w.failUnsent(ctx, run)
		}
		if w.r.deps.SoulPresence != nil {
			if !w.present(ctx, run.SID) {
				presentSince = time.Time{}
				continue
			}
			if presentSince.IsZero() {
				presentSince = now
			}
			if now.Sub(presentSince) < w.r.presenceSettle() {
				continue
			}
		}
		cancelled, err := w.r.cancelFlagged(ctx, w.spec.ApplyID)
		if err != nil {
			// Unknown is not "not cancelled": sending now could apply the step after a
			// Cancel. The next poll asks again.
			continue
		}
		if cancelled {
			if onCancel != nil {
				if err := onCancel(); err != nil {
					continue
				}
			}
			w.log.Info("scenario: run cancelled - the step is not sent to a host that waited for its stream",
				slog.String("sid", run.SID), slog.Int("passage", w.passage))
			return false
		}
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			return false
		}
		done, err := attempt()
		w.mu.Unlock()
		if err != nil {
			// The database's error, not the stream's: the next poll tries again, and
			// soul_timeout still bounds the wait.
			w.log.Warn("scenario: sending a step to a host that waited for its stream failed - retrying",
				slog.String("sid", run.SID), slog.Any("error", err))
			continue
		}
		if done {
			return true
		}
		presentSince = time.Time{}
	}
}

// cancelFlagged reports a cluster-wide Cancel of run applyID: on the applying lock,
// which [Runner.RequestCancel] flags first, or on a row, the one carrier for a run
// that holds no lock (`destroying`). A local Cancel ends the run through ctx.
func (r *Runner) cancelFlagged(ctx context.Context, applyID string) (bool, error) {
	c, err := incarnation.RunCancelRequested(ctx, r.deps.DB, applyID)
	if err != nil || c {
		return c, err
	}
	return applyrun.RunCancelFlagged(ctx, r.deps.DB, applyID)
}

// failUnsent leaves a failed row for a host the step never reached, so the
// barrier counting it ends — retrying a write that fails, since without the row
// the barrier waits for the host until scenario_timeout. It reports whether the
// row was left.
func (w *stepWatch) failUnsent(ctx context.Context, run *applyrun.ApplyRun) bool {
	summary := fmt.Sprintf("%s: no agent stream to send the step on within soul_timeout (%s)", soulUnavailable, w.soulTimeout)
	tick := time.NewTicker(w.r.pollInterval)
	defer tick.Stop()
	for logged := false; ; logged = true {
		wrote, err := w.recordUnsent(ctx, run, summary)
		if err == nil {
			return wrote
		}
		if !logged {
			w.log.Error("scenario: recording a host the step never reached failed - retrying",
				slog.String("sid", run.SID), slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// recordUnsent writes the row of failUnsent; true — written, false with a nil
// error — the step ended first.
func (w *stepWatch) recordUnsent(ctx context.Context, run *applyrun.ApplyRun, summary string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false, nil
	}
	// Written failed in one statement: a row left non-terminal here is one nothing
	// closes but scenario_timeout.
	row := *run
	row.Status = applyrun.StatusFailed
	row.ErrorSummary = &summary
	err := applyrun.Insert(ctx, w.r.deps.DB, &row)
	switch {
	case err == nil:
		if serr := applyrun.StampFinished(ctx, w.r.deps.DB, run.ApplyID, run.SID, run.Passage); serr != nil {
			w.log.Warn("scenario: stamping a failed host's finish time failed", slog.String("sid", run.SID), slog.Any("error", serr))
		}
	case errors.Is(err, applyrun.ErrApplyRunAlreadyExists):
		// The row a failed withdraw left behind.
		if uerr := applyrun.UpdateStatus(ctx, w.r.deps.DB, run.ApplyID, run.SID, run.Passage, applyrun.StatusFailed, &summary); uerr != nil && !errors.Is(uerr, applyrun.ErrApplyRunAlreadyTerminal) {
			return false, uerr
		}
	default:
		return false, err
	}
	w.log.Warn("scenario: host failed for unavailability - no stream to send the step on",
		slog.String("sid", run.SID), slog.Int("passage", w.passage), slog.Duration("soul_timeout", w.soulTimeout))
	return true, nil
}

// present reports whether sid's stream is there; a presence error answers yes, so
// the send itself decides.
func (w *stepWatch) present(ctx context.Context, sid string) bool {
	alive, err := w.r.deps.SoulPresence.SoulsStreamAlive(ctx, []string{sid})
	if err != nil {
		return true
	}
	_, ok := alive[sid]
	return ok
}

// check is the barrier's dead-host detector: every host holding this step's
// `dispatched` row whose stream has been gone for soul_timeout is failed for
// unavailability. Absence is counted from the first poll that saw it and forgiven
// by any poll that sees the stream again — a host that reconnected keeps its row,
// and its Soul relays a result finished while the stream was down. A presence
// error proves nothing either way and changes nothing. nil-safe.
func (w *stepWatch) check(ctx context.Context, statuses []applyrun.HostStatus) {
	if w == nil || w.r.deps.SoulPresence == nil {
		return
	}
	var out []string
	for _, hs := range statuses {
		if hs.Passage == w.passage && hs.Status == applyrun.StatusDispatched && hs.SID != render.KeeperTargetSID {
			out = append(out, hs.SID)
		}
	}
	if len(out) == 0 {
		return
	}
	alive, err := w.r.deps.SoulPresence.SoulsStreamAlive(ctx, out)
	if err != nil {
		w.log.Debug("scenario: agent presence unavailable - no host judged lost this poll", slog.Any("error", err))
		return
	}
	now := time.Now()
	for _, sid := range out {
		if _, ok := alive[sid]; ok {
			delete(w.lostSince, sid)
			continue
		}
		since, seen := w.lostSince[sid]
		if !seen {
			w.lostSince[sid] = now
			w.log.Info("scenario: agent stream lost while the step is out - waiting soul_timeout",
				slog.String("sid", sid), slog.Int("passage", w.passage), slog.Duration("soul_timeout", w.soulTimeout))
			continue
		}
		if now.Sub(since) < w.soulTimeout {
			continue
		}
		summary := fmt.Sprintf("%s: agent stream lost for longer than soul_timeout (%s)", soulUnavailable, w.soulTimeout)
		failed, ferr := applyrun.FailLost(ctx, w.r.deps.DB, w.spec.ApplyID, sid, w.passage, summary)
		if ferr != nil {
			w.log.Error("scenario: failing a host whose stream stayed lost failed", slog.String("sid", sid), slog.Any("error", ferr))
			continue
		}
		delete(w.lostSince, sid)
		if failed {
			w.log.Warn("scenario: host failed for unavailability - its stream stayed lost",
				slog.String("sid", sid), slog.Int("passage", w.passage), slog.Duration("soul_timeout", w.soulTimeout))
		}
	}
}
