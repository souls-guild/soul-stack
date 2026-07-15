package scenario

import (
	"context"
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
)

// Start registers a run and spawns the run-goroutine, returning immediately
// (async).
//
// runCtx is detached from the request-ctx's cancel/deadline
// (context.WithoutCancel): the run survives the HTTP 202 response return, BUT
// inherits the parent's values — mainly OTel SpanContext/baggage, so the
// `scenario.run` span stitches to the request span (ADR-024). Cancellation
// works via [Runner.Cancel] (by applyID) or [Runner.Shutdown] (all active).
// No per-scenario timeout is set here — the plan isn't parsed yet; the
// provision-aware effective run-timeout is applied by run() as a separate
// WithTimeout over runCtx (inherits cancel from the active-map).
//
// Returns:
//   - [ErrShuttingDown] — Runner is shutting down.
//   - [ErrAlreadyRunning] — applyID is already active (duplicate call).
//   - a validation error for empty spec fields.
func (r *Runner) Start(parent context.Context, spec RunSpec) error {
	if spec.ApplyID == "" {
		return fmt.Errorf("scenario: empty apply_id")
	}
	if spec.IncarnationName == "" {
		return fmt.Errorf("scenario: empty incarnation_name")
	}
	if spec.ScenarioName == "" {
		return fmt.Errorf("scenario: empty scenario_name")
	}

	r.mu.Lock()
	if r.shuttingDown {
		r.mu.Unlock()
		return ErrShuttingDown
	}
	if _, exists := r.active[spec.ApplyID]; exists {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	// runCtx lives independently of the request-ctx — the run must not die
	// when the 202 response returns. WithoutCancel: keep trace baggage
	// (parent's SpanContext for trace stitching), don't inherit the request's
	// cancel/deadline. cancel goes into the active-map — the basis for
	// Cancel/Shutdown (tears down the parent runCtx). Deadline
	// (provision-aware effective run-timeout) is NOT set here: the plan isn't
	// parsed yet (whether it's a provision run is unknown). It's applied in
	// run() AFTER ExpandIncludes as a separate WithTimeout over this runCtx
	// (the sub-context inherits cancellation from the active-map — Cancel/
	// Shutdown keep working).
	runCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	r.active[spec.ApplyID] = cancel
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		defer cancel()
		defer r.unregister(spec.ApplyID)
		r.run(runCtx, spec)
	}()

	return nil
}

// StartDestroy initiates incarnation teardown: runs scenario `destroy`
// against its hosts in [TerminalDestroy] mode (S-D2b). A thin wrapper over
// [Runner.Start] — sets ScenarioName=`destroy` and TerminalMode=Destroy; the
// rest (async, lockRun gate, dispatch, barrier) goes through the common
// run() path.
//
// Finalization semantics (run.go): teardown success → incarnation stays in
// `destroying` (state untouched, ready is NOT committed; row DELETE is
// S-D3); teardown failure → `destroy_failed` (NOT error_locked). Only starts
// from `destroying` (lockRun rejects any other status as ErrNotRunnable) —
// destroy is already initiated by S-D1, and the scenario `destroy`
// existence pre-check (PrepareDestroy) passed BEFORE the transition to
// destroying.
//
// Caller (handler S-D4) does Destroy() → StartDestroy(), passing the same
// applyID as in the initiation's state_history snapshot. ScenarioName/Input
// in spec are ignored — StartDestroy sets them itself.
//
// Returns the same errors as [Runner.Start] (ErrShuttingDown /
// ErrAlreadyRunning / validation of empty spec fields).
func (r *Runner) StartDestroy(parent context.Context, spec RunSpec) error {
	spec.ScenarioName = DestroyScenarioName
	spec.TerminalMode = TerminalDestroy
	return r.Start(parent, spec)
}

// Cancel cancels the active run for applyID ON THIS instance. Returns true if
// the run was active locally (cancel invoked), false if applyID isn't in the
// active-map (finished, never started, or the run-goroutine lives on ANOTHER
// Keeper instance — use [RequestCancel] for cross-Keeper).
//
// Cancel only cancels runCtx: the dispatch loop breaks, but Keeper does NOT
// send CancelApply to already-dispatched Souls in the pilot (best-effort
// cancel — an admin-API layer via Outbound.SendCancel, separate). The
// incarnation stays in whatever status run.go records for the interrupted
// run (error_locked).
func (r *Runner) Cancel(applyID string) bool {
	r.mu.Lock()
	cancel, ok := r.active[applyID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// RequestCancel is cluster-wide run cancellation (G1): works regardless of
// which Keeper instance the run-goroutine lives on (ADR-002, stateless
// cluster). Entry point for the admin API (HTTP/MCP Cancel).
//
// Two paths, both idempotent:
//   - PG flag (cross-Keeper): [applyrun.RequestCancel] sets cancel_requested
//     on all still-running rows of the run. The owning instance's goroutine
//     sees the flag in barrier polling (waitBarrier) on the next tick and
//     cancels the run the same way as a local Cancel (abort →
//     error_locked). A terminal run is untouched (status='running' filter) —
//     cancelling a finished run is a no-op.
//   - local Cancel (fast path): if the goroutine is here — [Runner.Cancel]
//     cancels runCtx immediately, without waiting for a barrier tick. If on
//     another instance — false, the flag delivers the cancellation.
//
// Returns:
//   - found — the run was active (at least one running PG row was affected OR
//     the local Cancel fired). false → no such run, or it's already terminal
//     (caller treats it as no-op / 404).
//   - error — only a PG flag-update failure (local Cancel never errors).
func (r *Runner) RequestCancel(ctx context.Context, applyID string) (found bool, err error) {
	if applyID == "" {
		return false, fmt.Errorf("scenario: empty apply_id")
	}
	// PG flag first — the cluster-wide path is authoritative (survives
	// cross-Keeper).
	affected, perr := applyrun.RequestCancel(ctx, r.deps.DB, applyID)
	if perr != nil {
		return false, fmt.Errorf("scenario: request cancel: %w", perr)
	}
	// Local fast path: if the goroutine is on this instance, cancel
	// immediately instead of waiting for the barrier to pick up the
	// just-set flag.
	local := r.Cancel(applyID)
	return affected > 0 || local, nil
}

// Shutdown stops accepting new runs and waits for active ones to finish
// (graceful). If ctx cancels first — cancels all active runCtx (force) and
// waits for them to exit without a time limit (run-goroutines react to
// cancel quickly — the dispatch loop checks ctx).
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.shuttingDown = true
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		r.cancelAll()
		// Wait for the goroutines to actually exit: after cancelAll the
		// dispatch loop breaks on its next ctx check.
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			r.logger.Warn("scenario: run goroutines did not exit within 15s after shutdown — leak suspected")
		}
		return ctx.Err()
	}
}

// unregister removes applyID from the active-map (called by the
// run-goroutine on completion).
func (r *Runner) unregister(applyID string) {
	r.mu.Lock()
	delete(r.active, applyID)
	r.mu.Unlock()
}

// cancelAll cancels runCtx for all active runs (force-shutdown).
func (r *Runner) cancelAll() {
	r.mu.Lock()
	for _, cancel := range r.active {
		cancel()
	}
	r.mu.Unlock()
}
