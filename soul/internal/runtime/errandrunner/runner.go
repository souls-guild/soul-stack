// Package errandrunner is the Soul-side executor for Errand requests (ADR-033 §6).
//
// Errand is a pull-based ad-hoc exec of a single module on a specific Soul over
// the already-trusted mTLS EventStream channel. Its contract differs from the
// apply cycle:
//
//   - does NOT mutate incarnation.state (`state_changes` are ignored);
//   - one [keeperv1.ErrandRequest] → one [keeperv1.ErrandResult], no
//     intermediate TaskEvent / RunResult;
//   - module whitelist: hardcoded `core.cmd.shell` / `core.exec.run` +
//     marker interface [sdkmodule.ErrandReadSafe] (see [IsAllowed]);
//   - stdout/stderr are captured from the final ApplyEvent.Output, capped at
//     64 KiB per channel + secret-masking (defense-in-depth — Keeper-side
//     does the same when receiving the result, see
//     keeper/internal/errand/mask.go).
//
// Runner calls SoulModule.Apply on the same [Registry] as applyrunner (shared
// core + plugin), but with a synthetic ApplyRequest{state, params}, skipping
// the apply cycle's flow-control/retry/onfail wiring: Errand is a single call.
//
// Concurrency: multiple concurrent Run calls are allowed (Errands aren't
// serialized on Soul, unlike apply, ADR-012(a)). Runner is stateless;
// Registry / Logger / Metrics are read-only after construction.
package errandrunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sdkmodule "github.com/souls-guild/soul-stack/sdk/module"
)

// Registry is a narrow interface over runtime.Registry (`Lookup` by bare
// module name, no state suffix). Declared here so the package doesn't depend
// on `soul/internal/runtime` (avoids cycles: runtime imports sdk/module, so
// does errandrunner — sdk/module is the shared point).
type Registry interface {
	Lookup(name string) (sdkmodule.SoulModule, bool)
}

// Runner is the Soul-side executor of a single Errand. Immutable after
// construction except the active map (errand_id → cancel-fn of the active
// Run goroutine, slice E5).
type Runner struct {
	modules Registry
	logger  *slog.Logger
	metrics *Metrics

	// active tracks running Run goroutines for cancel-flow (ADR-033 slice E5).
	// Populated at the start of Run, cleared via defer before return. Parallel
	// Runs on the same errand_id are impossible (errand_id is a ULID, Keeper
	// guarantees uniqueness); concurrent Runs of different errand_ids are
	// allowed (Errands aren't serialized, ADR-033). mu is short read/write; a
	// separate RWMutex would be overkill.
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// New builds a Runner. logger=nil → slog.Default(); metrics=nil → no-op
// (nil-safe [Metrics] methods); modules is required (panics on nil — a
// wire-up bug, not runtime data).
func New(modules Registry, logger *slog.Logger, metrics *Metrics) *Runner {
	if modules == nil {
		panic("errandrunner: modules registry is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		modules: modules,
		logger:  logger,
		metrics: metrics,
		active:  make(map[string]context.CancelFunc),
	}
}

// Cancel is slice E5: cancels the active Run goroutine by errand_id. Returns
// true if a Run was registered and cancel was invoked; false if the Run has
// already finished or doesn't exist (a race with its own terminal — a safe
// no-op). After cancel, the Run goroutine sees ctx.Err() (Canceled) and
// returns ErrandResult{status: CANCELLED}.
//
// Same pattern as [ApplyRunner.Cancel] — best-effort signal, doesn't block on
// the Run goroutine finishing.
func (r *Runner) Cancel(errandID string) bool {
	r.mu.Lock()
	cancel, ok := r.active[errandID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// registerActive registers a cancel-fn in the active map. Returns an
// unregister-fn (defer-friendly) that removes the entry. If errand_id is
// already in the map (a duplicate ErrandRequest from Keeper — impossible by
// protocol, but defensive), the previous cancel-fn gets overwritten; the old
// Run goroutine already finished registering, the race resolves via the short
// mutex.
func (r *Runner) registerActive(errandID string, cancel context.CancelFunc) func() {
	r.mu.Lock()
	r.active[errandID] = cancel
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.active, errandID)
		r.mu.Unlock()
	}
}

// Run executes a single Errand. Returns a terminal [keeperv1.ErrandResult] —
// the caller (eventstream dispatcher) sends it back to Keeper as one message.
//
// ADR-033 contract:
//  1. Whitelist check BEFORE any action (defense-in-depth, Keeper does too).
//  2. Resolve the module through the same Registry as applyrunner.
//  3. dry_run=true → mod.Plan(...) ONLY for [sdkmodule.PlanReadSafe];
//     otherwise FAILED with `errand_dry_run_unsupported`.
//  4. dry_run=false → mod.Apply(synthetic ApplyRequest).
//  5. Output capped at 64 KiB per stdout/stderr channel + masking via
//     [MaskSecrets].
//  6. state_changes are ignored (Errand doesn't write them).
//
// If ctx expires via timeout_seconds — status is TIMED_OUT (not FAILED).
func (r *Runner) Run(ctx context.Context, req *keeperv1.ErrandRequest) *keeperv1.ErrandResult {
	started := time.Now()
	if req == nil {
		// defensive: dispatcher checks nil before Run, but Run is a public API.
		return &keeperv1.ErrandResult{
			Status:       keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
			ErrorMessage: "errand: request is nil",
		}
	}
	errandID := req.GetErrandId()

	// Address split: `core.cmd.shell` → (`core.cmd`, `shell`). No state suffix
	// means the module isn't called (whitelist relies on the full name).
	// Registry only accepts `<namespace>.<name>` (see coremod.Registry).
	modName, state, ok := splitModuleAddr(req.GetModule())
	if !ok || state == "" {
		return r.terminal(errandID, started,
			keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
			fmt.Sprintf("errand: bad module address %q (expect <namespace>.<name>.<state>)", req.GetModule()),
		)
	}

	mod, found := r.modules.Lookup(modName)
	if !found {
		// Treat a nonexistent module as MODULE_NOT_ALLOWED: from the Errand
		// contour's perspective it's "not allowed" (whitelist implies
		// existence). Same audit mapping (errand.failed), status code
		// distinguishes the reason.
		r.logger.Warn("errand: module not found in Registry",
			slog.String("errand_id", errandID),
			slog.String("module", req.GetModule()))
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED, started)
		return r.terminalNoMetric(errandID, started,
			keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED,
			fmt.Sprintf("errand_module_not_allowed: %s", req.GetModule()),
		)
	}

	if ok, reason := IsAllowed(req.GetModule(), mod); !ok {
		// Whitelist rejection is a security event (attempt to call a
		// non-read-safe module via Errand). Warn level: keeper validate also
		// rejects such requests; they only reach Soul on an early
		// pre-validation client build or a keeper-side bug, worth seeing in
		// logs.
		r.logger.Warn("errand: whitelist reject",
			slog.String("errand_id", errandID),
			slog.String("module", req.GetModule()),
			slog.String("reason", reason))
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED, started)
		return r.terminalNoMetric(errandID, started,
			keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED,
			reason,
		)
	}

	// Dry-run validation: only modules with [sdkmodule.PlanReadSafe]. ADR-031's
	// Plan is pure-read; for verb modules (cmd.shell/exec.run) Plan isn't
	// PlanReadSafe, so dry_run on shell/exec returns
	// `errand_dry_run_unsupported`. Deliberate constraint, not a bug (see
	// core.cmd's doc comment).
	if req.GetDryRun() {
		if _, planReadSafe := mod.(sdkmodule.PlanReadSafe); !planReadSafe {
			r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_FAILED, started)
			return r.terminalNoMetric(errandID, started,
				keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
				"errand_dry_run_unsupported",
			)
		}
	}

	// Timeout applies to the sub-ctx; expiry → TIMED_OUT (distinct from
	// FAILED). 0 → no timeout of its own (parent ctx already carries the
	// dispatcher's ServerCap). Slice E5: cancel-flow wraps via WithCancel
	// regardless of timeout, so Cancel(errandID) can cancel Run even at
	// timeout_seconds=0.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if to := req.GetTimeoutSeconds(); to > 0 {
		var timeoutCancel context.CancelFunc
		runCtx, timeoutCancel = context.WithTimeout(runCtx, time.Duration(to)*time.Second)
		defer timeoutCancel()
	}

	// Slice E5: register the cancel-fn in the active map so an external Cancel
	// (CancelErrand from Keeper) can cancel Run without touching runCtx.
	unregister := r.registerActive(errandID, cancelRun)
	defer unregister()

	collector := newOutputCollector(runCtx, OutputCapBytes)
	pluginReq := &pluginv1.ApplyRequest{
		State:  state,
		Params: req.GetInput(),
	}

	var modErr error
	if req.GetDryRun() {
		// PlanEvent from the Plan stream is collected by a separate collector
		// (a different stream type), but for Errand the final only matters as
		// "didn't fail" — Plan for read-safe modules doesn't write
		// stdout/stderr/output (ADR-031: Plan only sends the final
		// PlanEvent.changed).
		planCollector := newPlanCollector(runCtx)
		modErr = mod.Plan(&pluginv1.PlanRequest{State: state, Params: req.GetInput()}, planCollector)
	} else {
		modErr = mod.Apply(pluginReq, collector)
	}
	durationMs := time.Since(started).Milliseconds()

	// Final status priority: timeout > cancel > module error > module event.
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT, started)
		return r.buildResult(errandID, collector, durationMs,
			keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT,
			"errand_timeout_exceeded")
	case errors.Is(runCtx.Err(), context.Canceled):
		// runCtx.Err()=Canceled means either an external Cancel(errandID)
		// (slice E5 — operator cancelled via DELETE /v1/errands/{id}), or the
		// parent ctx was cancelled (daemon shutdown). Semantically both are
		// CANCELLED: the Errand didn't reach a natural terminal, the
		// operator/process stopped it.
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_CANCELLED, started)
		return r.buildResult(errandID, collector, durationMs,
			keeperv1.ErrandStatus_ERRAND_STATUS_CANCELLED, "errand cancelled by operator")
	case modErr != nil:
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_FAILED, started)
		return r.buildResult(errandID, collector, durationMs,
			keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
			maskedMessage(modErr.Error()))
	}

	last := collector.lastEvent()
	if last != nil && last.GetFailed() {
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_FAILED, started)
		return r.buildResult(errandID, collector, durationMs,
			keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
			maskedMessage(last.GetMessage()))
	}

	r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS, started)
	return r.buildResult(errandID, collector, durationMs,
		keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS, "")
}

// buildResult assembles an ErrandResult from the module's output:
// stdout/stderr/exit_code are extracted from ApplyEvent.Output (core.cmd/exec
// format), the rest of Output stays as structured output (for future
// read-safe modules). Masking + cap happen here, in one place, no
// duplication.
func (r *Runner) buildResult(errandID string, c *outputCollector, durationMs int64, status keeperv1.ErrandStatus, errMsg string) *keeperv1.ErrandResult {
	stdout, stderr, exitCode, structured := c.extractFinal()

	stdoutMasked, stdoutTrunc := MaskAndCapBytes(stdout)
	stderrMasked, stderrTrunc := MaskAndCapBytes(stderr)

	res := &keeperv1.ErrandResult{
		ErrandId:        errandID,
		Status:          status,
		ExitCode:        exitCode,
		Stdout:          stdoutMasked,
		Stderr:          stderrMasked,
		StdoutTruncated: stdoutTrunc,
		StderrTruncated: stderrTrunc,
		DurationMs:      durationMs,
		ErrorMessage:    errMsg,
		Output:          structured,
	}
	if (status == keeperv1.ErrandStatus_ERRAND_STATUS_FAILED ||
		status == keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT) && stdoutTrunc {
		// Echo: both flags are visible via keeper-side mask+cap (defense-in-depth).
	}
	return res
}

// terminal is a terminal result without module output (early-fail on
// whitelist/dry_run/etc). The metric is incremented inside.
func (r *Runner) terminal(errandID string, started time.Time, status keeperv1.ErrandStatus, msg string) *keeperv1.ErrandResult {
	r.recordTerminal("", status, started)
	return r.terminalNoMetric(errandID, started, status, msg)
}

// terminalNoMetric is a terminal result without metrics (caller already
// incremented it). Split out so early-reject (MODULE_NOT_ALLOWED,
// dry_run_unsupported) records the metric with the real module label instead
// of an empty one.
func (r *Runner) terminalNoMetric(errandID string, started time.Time, status keeperv1.ErrandStatus, msg string) *keeperv1.ErrandResult {
	return &keeperv1.ErrandResult{
		ErrandId:     errandID,
		Status:       status,
		DurationMs:   time.Since(started).Milliseconds(),
		ErrorMessage: msg,
	}
}

func (r *Runner) recordTerminal(module string, status keeperv1.ErrandStatus, started time.Time) {
	r.metrics.ObserveErrand(status)
	r.metrics.ObserveDuration(module, time.Since(started).Seconds())
}

// maskedMessage masks an error message (a module might return text with a
// vault ref or a sensitive key). Uses the same mask dictionary as
// stdout/stderr via [MaskAndCapBytes], without capping: error_message is
// short.
func maskedMessage(s string) string {
	if s == "" {
		return ""
	}
	masked, _ := MaskAndCapBytes(s)
	return masked
}

// splitModuleAddr splits `<namespace>.<name>.<state>` into
// (`<namespace>.<name>`, state). Mirrors runtime.splitModuleAddr (unexported
// there — duplicating 12 lines beats a dependency on applyrunner). For
// Errand, state is REQUIRED (whitelist `core.cmd.shell` is the full form):
// empty state → ok=false.
func splitModuleAddr(s string) (name, state string, ok bool) {
	if s == "" {
		return "", "", false
	}
	idx := -1
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			idx = i
			break
		}
	}
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// planCollector is a capture-only PlanEvent server for the dry_run branch.
// ApplyEvent and PlanEvent are different types, the shared outputCollector
// doesn't fit. Plan for read-safe modules only sends the final
// PlanEvent.changed (ADR-031), output isn't populated.
type planCollector struct {
	grpc.ServerStream
	ctx    context.Context
	events []*pluginv1.PlanEvent
}

func newPlanCollector(ctx context.Context) *planCollector {
	return &planCollector{ctx: ctx}
}

func (c *planCollector) Context() context.Context     { return c.ctx }
func (c *planCollector) SetHeader(metadata.MD) error  { return nil }
func (c *planCollector) SendHeader(metadata.MD) error { return nil }
func (c *planCollector) SetTrailer(metadata.MD)       {}
func (c *planCollector) SendMsg(any) error            { return nil }
func (c *planCollector) RecvMsg(any) error {
	return errors.New("plan collector: RecvMsg not supported")
}
func (c *planCollector) Send(ev *pluginv1.PlanEvent) error {
	c.events = append(c.events, ev)
	return nil
}
