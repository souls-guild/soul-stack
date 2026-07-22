// Package runtime — the Soul daemon's apply cycle: receives ApplyRequest from
// Keeper, dispatches via Registry, aggregates ApplyEvent → TaskEvent, and
// produces the final RunResult.
//
// Core modules (ADR-015) run in-process through [inProcApplyStream] — an
// adapter from `grpc.ServerStreamingServer[pluginv1.ApplyEvent]` over a Go
// channel. Custom modules (ADR-020, soul-mod-*) run as a sub-process via
// [soul/internal/pluginhost] (M2.3+: wire-up currently just takes a Registry,
// no distinction yet).
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// tracer for in-process apply-cycle spans. Uses the global TracerProvider set
// up by [obs.SetupOTel] in cmd/soul; when OTel is disabled the provider is a
// no-op, so spans are free and no branching is needed (ADR-024 §1.2).
var tracer = otel.Tracer("soul/runtime")

// Registry is a narrow interface over [coremod.Registry] or any module
// store — only Lookup, which is all the apply cycle needs.
type Registry interface {
	Lookup(name string) (module.SoulModule, bool)
}

// EventSink is where runtime sends messages for Keeper. Implemented by the
// EventStream client ([soul/internal/grpc.StreamSession]); tests use a fake
// to verify the TaskEvent/RunResult sequence.
type EventSink interface {
	SendTaskEvent(*keeperv1.TaskEvent) error
	SendRunResult(*keeperv1.RunResult) error
}

// ApplyRunner holds the apply-cycle state of one Soul daemon.
//
// No concurrent runs on one daemon — ADR-012(a) guarantees Keeper won't send
// a second ApplyRequest before the RunResult for the current one arrives.
// Still, the runner keeps a map of active apply_id → cancel so a Keeper
// CancelApply can target the in-flight run.
type ApplyRunner struct {
	registry Registry
	metrics  *ApplyMetrics

	// flowEngine is the Soul-side sandboxed CEL engine for flow-control
	// predicates (when:/changed_when:/failed_when:, ADR-012(d)). One per
	// runner: env is fixed, compile cache is reused across tasks and runs (no
	// concurrent runs, ADR-012(a)). No vault-client — external access is
	// keeper-only, Soul-side CEL is pure. flowEngineErr is set if engine
	// construction fails (cel-go incompatibility, "should never happen"); if
	// set, [ApplyRunner.Run] fails the run with an internal error instead of
	// silently skipping predicates.
	flowEngine    *cel.Engine
	flowEngineErr error

	// hostFacts is the soulprint snapshot (pkg-mgr / init system) collected by
	// the Soul agent, injected into core modules before Apply (Variant A,
	// ADR-018(b)). Set once at startup via [ApplyRunner.SetHostFacts]; zero
	// value is safe — core modules fall back to runtime detection. Read-only
	// in Run (never changes after startup), no extra sync needed.
	hostFacts util.HostFacts

	mu     sync.Mutex
	active map[string]context.CancelFunc

	// recentlyFinished — short-TTL set of apply_id finished by Run in the last
	// [recentlyFinishedTTL] (Soul-reconcile, ADR-027(g), S6). Closes the race
	// where RunResult was sent but the stream broke before unregister ran: on
	// reconnect, [ApplyRunner.ActiveSet] must still report the apply as owned,
	// or Keeper-sweep would orphan a row whose result is already in
	// flight/delivered. Entries expire after TTL, swept lazily in ActiveSet
	// (set stays tiny — one in-flight apply per Soul, ADR-012(a)).
	//
	// Survives reconnect/failback-swap (per-process cache) but not a process
	// restart — correct, since after a restart nothing is really in flight and
	// its dispatched rows legitimately orphan.
	//
	// Guarded by the same mu as active/lastSeenAttempt (ops are short, no
	// concurrent apply per Soul). nowFn injects time for deterministic TTL
	// tests (time.Now in production).
	recentlyFinished map[string]time.Time
	nowFn            func() time.Time

	// lastSeenAttempt — Soul-guard fencing cache (ADR-027(g), Phase 2): apply_id
	// → highest attempt accepted for execution. [ApplyRunner.AcceptAttempt]
	// rejects an ApplyRequest with attempt < seen, filtering a stale duplicate
	// when a recovery scan re-queues a stale Ward while the original
	// (higher-attempt) apply is still in flight — the re-claim arrives with a
	// higher attempt and wins.
	//
	// Lives per-process rather than per-StreamSession because it must survive
	// stream reconnect/failback-swap; otherwise Soul would forget seen
	// attempts and let a stale duplicate through. A process restart does clear
	// it, but that's safe — nothing is in flight right after a restart, so
	// there's no stale duplicate left to fence.
	//
	// Guarded by the same mu as active — short ops, no concurrent apply per
	// Soul (ADR-012(a)).
	lastSeenAttempt map[string]int32
}

// recentlyFinishedTTL — window a finished Run stays in [ApplyRunner.ActiveSet]
// (Soul-reconcile, ADR-027(g), S6). Must comfortably cover "SendRunResult →
// unregister → reconnect → WardRoster" (really sub-second) so the race
// between an in-flight result and a broken stream doesn't produce a false
// orphan. 30s is a generous ceiling: the apply stays declared even across a
// few reconnect-loop backoff iterations; a longer TTL would just delay
// legitimate orphaning after a real Soul crash (which restarts the process
// and clears the set anyway).
const recentlyFinishedTTL = 30 * time.Second

// NewApplyRunner builds a runner with the registered modules.
//
// metrics is the soul_apply_* collectors (ADR-024); nil disables
// instrumentation ([ApplyMetrics] methods are nil-safe no-ops) — push mode
// (soul apply) and unit tests run without the obs stack.
func NewApplyRunner(reg Registry, metrics *ApplyMetrics) *ApplyRunner {
	engine, err := cel.NewFlowControl()
	return &ApplyRunner{
		registry:         reg,
		metrics:          metrics,
		flowEngine:       engine,
		flowEngineErr:    err,
		active:           make(map[string]context.CancelFunc),
		recentlyFinished: make(map[string]time.Time),
		nowFn:            time.Now,
		lastSeenAttempt:  make(map[string]int32),
	}
}

// SetHostFacts sets the host soulprint snapshot the runner injects into core
// modules implementing [util.SoulprintAware] (core.pkg / core.service) before
// each Apply (Variant A, ADR-018(b)). Called once from cmd/soul at startup
// after the first soulprint collection; before that hostFacts is empty and
// modules fall back to runtime backend detection. No concurrent Run per Soul
// (ADR-012(a)) and the value never changes after startup, so no extra sync.
func (r *ApplyRunner) SetHostFacts(f util.HostFacts) { r.hostFacts = f }

// Cancel attempts to cancel the active apply with the given id. Returns true
// if the apply was registered and cancel was called; false if it already
// finished or doesn't exist. After cancel, the Run goroutine observes
// ctx.Err() and ends the cycle, sending a RunResult with status CANCELLED.
func (r *ApplyRunner) Cancel(applyID string) bool {
	r.mu.Lock()
	cancel, ok := r.active[applyID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// AcceptAttempt is the attempt-fencing guard (ADR-027(g), Phase 2): decides
// whether to accept an ApplyRequest by its (apply_id, attempt). Called BEFORE
// [ApplyRunner.Run] for every incoming ApplyRequest.
//
// Rule:
//   - attempt == 0 → accept, no fencing: 0 means an old Keeper without the
//     fencing field (apply.proto field 4 forward-compat, ADR-012(c) only-add).
//     Cache untouched, so an empty attempt doesn't poison seen for later
//     fencing requests.
//   - attempt < seen[apply_id] → REJECT (false): a stale duplicate — a stale
//     Ward whose original (higher-attempt) apply was already accepted. Cache
//     untouched.
//   - attempt >= seen[apply_id] → accept, seen[apply_id] = attempt. Equality
//     is accepted (a redelivery of the same attempt isn't stale; SID-lease
//     already filters a true same-epoch duplicate — fencing on "==" would
//     falsely reject a valid redeliver).
//
// Returns true to execute (caller invokes Run); false to silently drop
// (ADR-027 barrier-B1: a rejected duplicate sends no RunResult — Keeper's
// barrier closes the original apply with its own RunResult, runTimeout is the
// backstop).
func (r *ApplyRunner) AcceptAttempt(applyID string, attempt int32) bool {
	// attempt=0 (old Keeper) — fencing disabled, execute without caching.
	if attempt == 0 {
		return true
	}
	r.mu.Lock()
	seen := r.lastSeenAttempt[applyID]
	if attempt < seen {
		r.mu.Unlock()
		// B1 (ADR-027): a rejected stale duplicate sends NOTHING to Keeper — just
		// a debug log + metric; no RunResult (the barrier closes the original
		// apply with its own RunResult; runTimeout is the backstop).
		r.metrics.ObserveFenced()
		slog.Default().Debug("runtime: ApplyRequest rejected by the attempt-fencing guard (stale duplicate)",
			slog.String("apply_id", applyID),
			slog.Int("attempt", int(attempt)),
			slog.Int("last_seen", int(seen)))
		return false
	}
	r.lastSeenAttempt[applyID] = attempt
	r.mu.Unlock()
	return true
}

func (r *ApplyRunner) register(applyID string, cancel context.CancelFunc) {
	r.mu.Lock()
	r.active[applyID] = cancel
	r.mu.Unlock()
}

// unregister removes the apply from the in-flight active set and moves it
// into the recently-finished ring (Soul-reconcile, ADR-027(g), S6): the
// apply_id stays declared for [recentlyFinishedTTL] after Run finishes, so a
// reconnect racing "RunResult in flight, stream broke before cleanup" doesn't
// produce a false orphan.
func (r *ApplyRunner) unregister(applyID string) {
	r.mu.Lock()
	delete(r.active, applyID)
	r.recentlyFinished[applyID] = r.nowFn()
	r.mu.Unlock()
}

// ActiveSet snapshots the apply runs Soul currently owns, for [WardRoster]
// (R-B transport, Soul-reconcile ADR-027(g), S6). Union of three sources:
//   - active — in-flight runs (Run still executing);
//   - recentlyFinished — finished in the last [recentlyFinishedTTL] (guards
//     against "result in flight, stream broke before unregister"); stale
//     entries are swept lazily here;
//   - lastSeenAttempt — apply_id with a known fencing epoch (attempt echo).
//     Gives the authoritative attempt for the record; in-flight/finished
//     entries without one report attempt=0 (old Keeper without fencing, or
//     epoch not yet seen).
//
// Returns one [keeperv1.ActiveApply] per apply_id (deduped over the union).
// A nil result explicitly declares "nothing owned": the caller sends a
// WardRoster with empty active[], and Keeper terminates all of the SID's
// dispatched rows on it. Correct both right after a restart (sets empty) and
// after the sole run finishes and ages out of the TTL.
func (r *ApplyRunner) ActiveSet() []*keeperv1.ActiveApply {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.nowFn()
	// Lazy sweep of stale finished entries (set is tiny — one in-flight apply).
	for id, at := range r.recentlyFinished {
		if now.Sub(at) >= recentlyFinishedTTL {
			delete(r.recentlyFinished, id)
		}
	}

	ids := make(map[string]struct{}, len(r.active)+len(r.recentlyFinished))
	for id := range r.active {
		ids[id] = struct{}{}
	}
	for id := range r.recentlyFinished {
		ids[id] = struct{}{}
	}
	if len(ids) == 0 {
		return nil
	}

	out := make([]*keeperv1.ActiveApply, 0, len(ids))
	for id := range ids {
		// attempt echo: authoritative epoch comes from lastSeenAttempt (last
		// accepted attempt). No entry (old Keeper / attempt=0) → 0: Keeper's
		// epoch-guard treats 0 as "no fencing" and won't fence on it.
		out = append(out, &keeperv1.ActiveApply{
			ApplyId: id,
			Attempt: r.lastSeenAttempt[id],
		})
	}
	return out
}

// Run executes every task in req sequentially, sending a TaskEvent for each,
// then a RunResult with the aggregated status.
//
// Gating before Apply (ADR-012(d)): a task executes only when
// `when && onchanges-satisfied && onfail-satisfied`. when: is evaluated by the
// Soul-side sandboxed cel-go engine ([ApplyRunner.evalWhen]) from
// RenderedTask.flow_context plus prior tasks' register (by register name).
// when:false, or onchanges not satisfied, or onfail not satisfied → SKIPPED
// (mod.Apply not called). changed_when/failed_when are evaluated Soul-side
// AFTER Apply ([ApplyRunner.runTask]): they override changed/failed on the
// result (changed_when first, then failed_when).
//
// Fail-stop with rescue (destiny/tasks.md §8): the first FAILED/TIMED_OUT task
// marks RunResult FAILED IRREVERSIBLY, but the loop does not stop. After a
// failure, subsequent ORDINARY (non-onfail) tasks are skipped (SKIPPED, Apply
// not called); ONLY onfail tasks whose source failed (register.failed==true)
// run — rescue/cleanup. onfail tasks never undo the failure: RunResult stays
// FAILED. In a normal (no-failure) run, onfail tasks are always SKIPPED.
// failed_when:false (ignore_errors) makes a task OK — it triggers neither
// fail-stop nor onfail.
//
// Error strategy:
//   - when: evaluation error → TaskEvent.status=FAILED (a runtime-error CEL is
//     expected per templating.md §10; a compile-error is a keeper↔soul
//     internal mismatch, defensive FAILED + warn); the run is marked FAILED,
//     the rescue tail (onfail on this task) runs, remaining tasks skip.
//   - module not found in Registry → TaskEvent.status=FAILED (treated as a
//     module failure: fail-stop with rescue), RunResult.status=FAILED.
//   - SoulModule.Apply returned an error → TaskEvent.status=FAILED.
//   - Apply sent ApplyEvent.failed=true → TaskEvent.status=FAILED.
//   - Apply sent ApplyEvent.changed=true (failed=false) → CHANGED.
//   - ctx was cancelled before/during a task → RunResult.status=CANCELLED, the
//     current task gets TaskEvent.status=CANCELLED, the rest don't run (no
//     TaskEvent sent). CancelApply stops the loop unconditionally — this is
//     not fail-stop, and rescue does not run on cancel.
//   - otherwise → OK.
//
// state_changes isn't aggregated yet (lands in M2.3+); RunResult.state_changes
// stays nil.
//
// Returns an error only on Sink I/O failure (stream broke). All task-level
// business errors travel through TaskEvent.error.
func (r *ApplyRunner) Run(ctx context.Context, req *keeperv1.ApplyRequest, sink EventSink) error {
	if req == nil {
		return fmt.Errorf("runtime: ApplyRequest is nil")
	}
	if sink == nil {
		return fmt.Errorf("runtime: sink is nil")
	}
	// The flow-control CEL engine is mandatory: when: predicates are evaluated
	// Soul-side (ADR-012(d)). A construction failure is a cel-go incompatibility
	// (not runtime data), so we fail the run with an explicit internal error
	// instead of silently ignoring predicates (that would corrupt gating — a
	// when:false task would run).
	if r.flowEngineErr != nil || r.flowEngine == nil {
		return fmt.Errorf("runtime: flow-control CEL engine unavailable: %w", r.flowEngineErr)
	}

	// Local ctx for this run — lets Cancel stop exactly this apply without
	// killing the Soul daemon's parent ctx.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	applyID := req.GetApplyId()
	// passage — the staged-render Passage index (ADR-056). Echoed verbatim in
	// EVERY TaskEvent/RunResult of this run (0 for N=1 — bit-for-bit as before
	// staged rendering): Keeper correlates completion per (apply_id, sid,
	// passage) and accumulates register for rendering the next Passage.
	// Captured once — all events of this ApplyRequest belong to one Passage.
	passage := req.GetPassage()
	if applyID != "" {
		r.register(applyID, cancel)
		defer r.unregister(applyID)
	}

	// One in-process span for the whole run. apply_id is a span attribute for
	// trace filtering (can't be a metric label — cardinality, ADR-024 §2.2); no
	// secrets (params are rendered Keeper-side and never become span
	// attributes here). sid isn't passed in ApplyRequest (authority is the
	// mTLS peer cert, ADR-012), so per-host breakdown lives in Keeper's span.
	// With OTel disabled the tracer is a no-op — Start/End are free.
	runCtx, span := tracer.Start(runCtx, "apply.run",
		trace.WithAttributes(attribute.String("apply_id", applyID)),
	)
	defer span.End()

	start := time.Now()
	defer func() { r.metrics.ObserveApplyDuration(time.Since(start).Seconds()) }()

	// registerByIdx collects register payloads (TaskEvent.register_data) of
	// already-run tasks BY INDEX — needed for requisite gating (`onchanges:`):
	// a task with onchanges_idx runs only if at least one source has
	// register.changed == true. Soul applies tasks strictly sequentially, so
	// by gating time the sources (always earlier in the plan) are already here.
	registerByIdx := make(map[int32]*structpb.Struct, len(req.GetTasks()))

	// registerByName collects register payloads by register NAME
	// (RenderedTask.register) — needed for flow-control predicates (when:/…),
	// which reference `register.<name>.*` (ADR-012(d)). Parallel to
	// registerByIdx (used for index-based onchanges gating). A task without a
	// register name never enters this map (addressable only by its idx).
	registerByName := make(map[string]any, len(req.GetTasks()))

	runStatus := keeperv1.RunStatus_RUN_STATUS_SUCCESS
	// runFailed — the run has already failed (a FAILED/TIMED_OUT task occurred).
	// Fail-stop no longer does an immediate break: the loop continues but
	// SKIPS all subsequent ordinary tasks, running ONLY onfail tasks whose
	// source failed (rescue/cleanup, destiny/tasks.md §8). RunResult still
	// ends up FAILED — onfail compensates, it doesn't undo the failure.
	runFailed := false
	for idx, task := range req.GetTasks() {
		// Cancel may have arrived between tasks — check before running the module.
		if err := runCtx.Err(); err != nil {
			runStatus = keeperv1.RunStatus_RUN_STATUS_CANCELLED
			break
		}

		// After a run failure, ordinary (non-onfail) tasks don't execute: they're
		// skipped WITHOUT evaluating when: (when: on a failed chain could produce
		// a spurious new FAILED). Exception — onfail tasks: gating for them
		// (source failed? + when/onchanges) is computed below. This is the
		// fail-stop semantics: the rescue tail runs, everything else skips.
		//
		// The applier-register terminal (aggregate_of) is ALSO an exception: it
		// runs no module (a synthetic fold of its children, no side effects), but
		// its register.<applier> MUST reflect the real destiny outcome even on
		// failure — otherwise an outer onfail:[<applier>] / when:
		// register.<applier>.failed would break (the failed aggregate would be
		// lost under a generic skipped). Emit the aggregate immediately: children
		// are earlier in the plan and already in registerByIdx (the terminal is
		// last in its group).
		if runFailed && len(task.GetOnfailIdx()) == 0 {
			var ev *keeperv1.TaskEvent
			if agg := task.GetAggregateOf(); len(agg) > 0 {
				ev = &keeperv1.TaskEvent{
					ApplyId:      applyID,
					TaskIdx:      int32(idx),
					Status:       keeperv1.TaskStatus_TASK_STATUS_OK,
					RegisterData: aggregateRegisterData(agg, registerByIdx),
				}
			} else {
				ev = skippedTaskEvent(applyID, int32(idx))
			}
			recordRegister(registerByIdx, registerByName, int32(idx), task.GetRegister(), ev.GetRegisterData())
			if err := sendTaskEvent(sink, ev, task, passage); err != nil {
				return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
			}
			r.metrics.ObserveTask(taskResult(ev.GetStatus()))
			if len(task.GetAggregateOf()) == 0 {
				r.metrics.ObserveSkipped(skipReasonFailedRun)
			}
			continue
		}

		// Gating: a task runs only when `when && onchanges-satisfied &&
		// onfail-satisfied` (ADR-012(d)). Order — when FIRST:
		//   - when:"" → true (unconditional); when:false → SKIPPED, Apply not
		//     called;
		//   - when:true but onchanges/onfail not satisfied → also SKIPPED.
		// Both paths produce the same skipped payload (changed=false — doesn't
		// trigger onchanges downstream, same as an onchanges-skip).
		when, whenErr := r.evalWhen(task, registerByName)
		if whenErr != nil {
			// when: evaluation error: a runtime-error CEL (e.g. register.x missing)
			// → task FAILED per the templating.md §10 error table; a compile-error
			// on Soul is internal (Keeper let an invalid predicate through) →
			// defensive FAILED + warn (the predicate was supposedly validated on
			// Keeper before render).
			r.logFlowControlError("when", task, whenErr)
			ev := &keeperv1.TaskEvent{
				ApplyId: applyID,
				TaskIdx: int32(idx),
				Status:  keeperv1.TaskStatus_TASK_STATUS_FAILED,
				Error: &keeperv1.TaskError{
					Code:    "flowcontrol.when_error",
					Module:  task.GetModule(),
					Message: fmt.Sprintf("when %q: %v", task.GetWhen(), whenErr),
				},
			}
			ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
			recordRegister(registerByIdx, registerByName, int32(idx), task.GetRegister(), ev.GetRegisterData())
			if err := sendTaskEvent(sink, ev, task, passage); err != nil {
				return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
			}
			r.metrics.ObserveTask(applyResultFailed)
			// when-error = task FAILED → fail-stop with rescue (same as a module
			// failure): mark the run failed but don't break the loop — onfail tasks
			// on this task will run, the rest skip (runFailed branch).
			runStatus = keeperv1.RunStatus_RUN_STATUS_FAILED
			runFailed = true
			continue
		}

		// when=false, OR onchanges not satisfied, OR onfail not satisfied →
		// SKIPPED (mod.Apply not called). onchanges prevents restart-flap
		// (restart only when config changed); onfail is rescue-gating: an onfail
		// task in a normal (no-failure) run is always SKIPPED, running only when
		// its source failed (skipOnFail). Multiple requisites combine with AND.
		if !when || skipOnChanges(task.GetOnchangesIdx(), registerByIdx) ||
			skipOnFail(task.GetOnfailIdx(), registerByIdx) {
			ev := skippedTaskEvent(applyID, int32(idx))
			recordRegister(registerByIdx, registerByName, int32(idx), task.GetRegister(), ev.GetRegisterData())
			if err := sendTaskEvent(sink, ev, task, passage); err != nil {
				return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
			}
			// SKIPPED is a terminal, neutral (non-failure) outcome; the closed-enum
			// soul_apply_tasks_total (ok/changed/failed) counts it as ok, not fail.
			r.metrics.ObserveTask(taskResult(ev.GetStatus()))
			// when is first in the gating chain: !when → reason=when, otherwise the
			// skip was caused by a requisite (onchanges/onfail not satisfied).
			if !when {
				r.metrics.ObserveSkipped(skipReasonWhen)
			} else {
				r.metrics.ObserveSkipped(skipReasonRequisite)
			}
			continue
		}

		ev := r.runTaskWithRetry(runCtx, applyID, int32(idx), task, registerByName, req.GetDryRun())
		// If cancel happened inside runTask (module honors ctx), we want a single
		// TaskEvent with status CANCELLED and RunResult=CANCELLED. TaskError.code
		// is kept as `apply.cancelled` for filtering in audit/logs — TaskStatus
		// already carries the cancellation fact, but the string code makes
		// grepping audit_log easier without enum resolution.
		if runCtx.Err() != nil {
			ev.Status = keeperv1.TaskStatus_TASK_STATUS_CANCELLED
			ev.Error = &keeperv1.TaskError{
				Code:    "apply.cancelled",
				Module:  ev.GetError().GetModule(),
				Message: "apply cancelled by Keeper",
			}
			ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
			if err := sendTaskEvent(sink, ev, task, passage); err != nil {
				return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
			}
			// A cancelled task is a terminal failure; the closed-enum
			// soul_apply_tasks_total (ok/changed/failed) counts it as failed.
			r.metrics.ObserveTask(applyResultFailed)
			runStatus = keeperv1.RunStatus_RUN_STATUS_CANCELLED
			break
		}
		// Applier-register materialization (orchestration.md §2.1.1, Variant B): a
		// terminal core.noop.run with a non-empty aggregate_of carries the
		// SUMMARY outcome of the applier's destiny run. Its own ApplyEvent is
		// trivial (noop → changed=false), so register_data is OVERWRITTEN with
		// the aggregate over child tasks (OR of changed/failed/timed_out).
		// Children are earlier in the plan and in this same ApplyRequest (the
		// terminal is last in its group), so they're already in registerByIdx.
		// The override happens AFTER the cancel branch (a cancelled task keeps
		// CANCELLED) and BEFORE sendTaskEvent/recordRegister — both the TaskEvent
		// sent to Keeper and the register used by later gating carry the
		// aggregate.
		if agg := task.GetAggregateOf(); len(agg) > 0 {
			ev.RegisterData = aggregateRegisterData(agg, registerByIdx)
		}
		if err := sendTaskEvent(sink, ev, task, passage); err != nil {
			return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
		}
		// The finished task's register becomes available downstream: to onchanges
		// gating (by index) and to flow-control predicates when:/… (by register
		// name, ADR-012(d)).
		recordRegister(registerByIdx, registerByName, int32(idx), task.GetRegister(), ev.GetRegisterData())
		r.metrics.ObserveTask(taskResult(ev.GetStatus()))
		if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_FAILED ||
			ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
			// Fail-stop with rescue (destiny/tasks.md §8): a failure marks
			// RunResult FAILED irreversibly (onfail tasks are cleanup, not an undo),
			// but the loop doesn't stop. Subsequent ordinary tasks are skipped
			// (runFailed branch at the top of the loop); only onfail tasks whose
			// source failed run. TIMED_OUT is a special case of failed: it also
			// triggers rescue and marks the run failed.
			if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
				// Count the timeout once, on the FINAL outcome (after retries are
				// exhausted), not per attempt — soul_apply_task_timed_out_total.
				r.metrics.ObserveTimedOut()
			}
			runStatus = keeperv1.RunStatus_RUN_STATUS_FAILED
			runFailed = true
		}
	}

	return sink.SendRunResult(&keeperv1.RunResult{
		ApplyId: applyID,
		Status:  runStatus,
		// attempt echoes the request's fencing epoch (ADR-027(g), gate-1): on
		// receipt, Keeper (correlateRunResult) checks it against
		// apply_runs.attempt and rejects a stale attempt's result. Soul returns
		// the value as-is; 0 (old Keeper without fencing) stays 0 and the
		// freshness check degrades gracefully.
		Attempt: req.GetAttempt(),
		// passage echoes ApplyRequest.passage (ADR-056): this Passage's barrier
		// waits for completion by (apply_id, sid, passage). 0 for N=1 — bit-for-bit
		// as before staged rendering.
		Passage: passage,
	})
}

// sendTaskEvent stamps the TaskEvent with an echo of RenderedTask.no_log and
// sends it to sink. The flag travels to Keeper so it can suppress
// register_data/error.message in the long-lived audit log for no_log tasks,
// without needing []RenderedTask (this TaskEvent might land on a different
// Keeper instance, ADR-002). Soul knows no_log from the run plan — it
// executes the task without logging its params/output.
func sendTaskEvent(sink EventSink, ev *keeperv1.TaskEvent, task *keeperv1.RenderedTask, passage int32) error {
	ev.NoLog = task.GetNoLog()
	// Echoes ApplyRequest.passage (ADR-056): Keeper correlates completion per
	// (apply_id, sid, passage) and accumulates register for rendering the next
	// Passage. Single point where every TaskEvent of the run gets this set.
	ev.Passage = passage
	// Echoes RenderedTask.plan_index (ADR-056 §S1 fix Variant B): the GLOBAL
	// task index across the whole plan (all Passages). Keeper correlates
	// register by this (apply_task_register.plan_index), NOT by the local
	// TaskEvent.task_idx (position in ApplyRequest.tasks[], local to
	// passage/host). N=1 run / old Keeper without the field → plan_index=0=
	// task_idx, bit-for-bit behavior. Single point where every TaskEvent of the
	// run gets this set.
	ev.PlanIndex = task.GetPlanIndex()
	return sink.SendTaskEvent(ev)
}

// defaultRetryDelay is the pause between attempts when retry_delay is unset or
// invalid (DSL-core retry.delay default, destiny/tasks.md §9).
const defaultRetryDelay = 5 * time.Second

// runTaskWithRetry wraps runTask to implement the DSL-core retry:/until:
// (destiny/tasks.md §9, Soul-side flow-control). Makes up to retry_count
// attempts at runTask (each is "one attempt → one TaskEvent", runTask's
// contract is unchanged); intermediate attempts are NOT emitted — the caller
// gets the TaskEvent of the LAST attempt only (the "one TaskEvent per
// task_idx" contract holds, no attempts counter is introduced).
//
// Semantics (per architect):
//   - retry_count 0/1/empty → a single attempt (backward compatible: no retry).
//   - WITHOUT until: retries while an attempt is FAILED/TIMED_OUT; the first
//     non-FAILED outcome (OK/CHANGED) exits; once attempts are exhausted, the
//     final status is the LAST attempt's as-is (FAILED or TIMED_OUT — TIMED_OUT
//     is NOT collapsed into FAILED). failed_when:false (ignore_errors) makes an
//     attempt OK → "non-FAILED outcome" → exits on the first attempt
//     (ignore_errors wins over retry).
//   - WITH until: until is evaluated AFTER each attempt (after the
//     changed_when/failed_when override). until-true → exit, final status is
//     the attempt's status AS-IS (until does NOT override failed). until-false
//     → delay → next attempt. All attempts until-false → task FAILED
//     (flowcontrol.until_exhausted), EVEN if the last attempt was OK/CHANGED.
//     On a TIMED_OUT attempt, until is NOT evaluated (a timeout is "failure,
//     retry if attempts remain").
//   - delay (retry_delay, default 5s) applies ONLY between attempts (not
//     before the first, not after the last); interruptible by run
//     cancellation via runCtx.
//   - cancel during delay/attempt → exits the loop; CANCELLED handling is done
//     by the caller (Run checks runCtx.Err() after return).
func (r *ApplyRunner) runTaskWithRetry(runCtx context.Context, applyID string, idx int32, task *keeperv1.RenderedTask, registerByName map[string]any, dryRun bool) *keeperv1.TaskEvent {
	// dry_run (Scry, ADR-031): a pure-read Plan INSTEAD of Apply. Retry/until
	// don't apply — a read is deterministic (the resource either matches the
	// desired state or it doesn't; re-reading is pointless), and Apply is never
	// called on dry_run. So dry_run is handled as a single planTask call,
	// bypassing the retry loop.
	if dryRun {
		return r.planTask(runCtx, applyID, idx, task)
	}

	maxAttempts := int(task.GetRetryCount())
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	until := task.GetUntil()

	// Fast path: a single attempt with no until → behaves exactly as before
	// (calls runTask directly), no delay machinery.
	if maxAttempts == 1 && until == "" {
		ev, _ := r.runTask(runCtx, applyID, idx, task, registerByName)
		return ev
	}

	delay := parseRetryDelay(task)

	var ev *keeperv1.TaskEvent
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Attempt 2+ is a retry (repeat after failure/until-false).
		// soul_apply_task_retries_total counts retries, not the first attempt.
		if attempt > 1 {
			r.metrics.ObserveRetry()
		}
		var self map[string]any
		ev, self = r.runTask(runCtx, applyID, idx, task, registerByName)

		// Cancel during an attempt → exit immediately; CANCELLED handling is in Run.
		if runCtx.Err() != nil {
			return ev
		}

		status := ev.GetStatus()
		timedOut := status == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT

		if until == "" {
			// retry WITHOUT until: the first non-FAILED outcome (OK/CHANGED) exits.
			// FAILED/TIMED_OUT retries if attempts remain. On the last attempt we
			// return the status as-is (TIMED_OUT isn't collapsed).
			if status != keeperv1.TaskStatus_TASK_STATUS_FAILED && !timedOut {
				return ev
			}
		} else if self == nil && !timedOut {
			// A terminal-error, non-timeout branch of runTask (selfRegister==nil):
			// bad address / module not found / flow-control compile-/runtime-error
			// in changed_when/failed_when. ev already carries the precise cause
			// code (flowcontrol.changed_when_error / failed_when_error / …) and
			// status FAILED. until-eval makes no sense here (no register.self) and
			// would overwrite the original code with flowcontrol.until_error — so
			// we return ev as-is, no retry. TIMED_OUT (also self==nil) does NOT
			// land here: a timeout is "failure, retry if attempts remain" and
			// falls through to the general retry branch below.
			return ev
		} else if !timedOut {
			// until (+retry): on a TIMED_OUT attempt until is NOT evaluated — that's
			// "failure, retry if attempts remain". Otherwise evaluate until AFTER
			// the override.
			ok, err := r.evalUntil(until, task, registerByName, self)
			if err != nil {
				// Runtime-/compile-error CEL in until → task FAILED (same as when/
				// changed_when/failed_when, templating.md §10). Terminal, no retry.
				r.logFlowControlError("until", task, err)
				return flowControlErrorEvent(applyID, idx, "flowcontrol.until_error", task, until, err)
			}
			if ok {
				// until-true → exit; final status is the attempt's status AS-IS
				// (until does NOT override failed: failed stays failed).
				return ev
			}
		}

		// The attempt failed (or until-false): delay before the next one, ONLY if
		// there is a next one (not after the last attempt). Delay is
		// interruptible via runCtx (taskCtx already expired via defer inside
		// runTask).
		if attempt < maxAttempts {
			select {
			case <-time.After(delay):
			case <-runCtx.Done():
				return ev
			}
		}
	}

	// Attempts exhausted. With until: it never became truthy → FAILED
	// (until_exhausted), EVEN if the last attempt was OK/CHANGED. Without
	// until: the last attempt's final status is already terminal
	// FAILED/TIMED_OUT — return it as-is (TIMED_OUT isn't collapsed into
	// FAILED).
	if until != "" {
		return untilExhaustedEvent(applyID, idx, task, until, maxAttempts)
	}
	return ev
}

// parseRetryDelay parses retry_delay with the same config.ParseDuration used
// for timeout (the single Soul Stack `duration` convention). Empty/invalid/
// non-positive → defaultRetryDelay (5s): defensive (format is validated by
// validateRetryField when destiny is parsed), never fails on a housekeeping
// error.
func parseRetryDelay(task *keeperv1.RenderedTask) time.Duration {
	rd := task.GetRetryDelay()
	if rd == "" {
		return defaultRetryDelay
	}
	d, err := config.ParseDuration(rd)
	if err != nil || d <= 0 {
		slog.Default().Warn("runtime: invalid/non-positive retry delay, default applied",
			slog.String("task", task.GetName()),
			slog.String("retry_delay", rd),
			slog.Duration("default", defaultRetryDelay))
		return defaultRetryDelay
	}
	return d
}

// evalUntil evaluates the until predicate with the same sandbox and
// activation as failed_when (flow_context + register.* of prior tasks +
// register.self of the fresh attempt with changed_when/failed_when applied).
// self is the selfRegister from runTask (nil on terminal-error branches;
// until never reaches those).
func (r *ApplyRunner) evalUntil(expr string, task *keeperv1.RenderedTask, registerByName map[string]any, self map[string]any) (bool, error) {
	return r.evalFlowPredicate(expr, task, mergeRegisterSelf(registerByName, self))
}

// untilExhaustedEvent builds the TaskEvent for a retry loop that exhausted all
// attempts with until-false: task FAILED (flowcontrol.until_exhausted), even
// if the last attempt was OK/CHANGED (until is a mandatory success condition,
// destiny/tasks.md §9).
func untilExhaustedEvent(applyID string, idx int32, task *keeperv1.RenderedTask, until string, attempts int) *keeperv1.TaskEvent {
	ev := &keeperv1.TaskEvent{
		ApplyId: applyID,
		TaskIdx: idx,
		Status:  keeperv1.TaskStatus_TASK_STATUS_FAILED,
		Error: &keeperv1.TaskError{
			Code:    "flowcontrol.until_exhausted",
			Module:  task.GetModule(),
			Message: fmt.Sprintf("until %q did not become truthy after %d attempts", until, attempts),
		},
	}
	ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
	return ev
}

// runTask dispatches ONE attempt of a task and builds the final TaskEvent.
// Returns the filled event without sending to sink (the caller sends it), and
// selfRegister — register.self of this attempt (DSL-core changed/failed/
// timed_out + output: fields, with changed_when/failed_when override applied).
// selfRegister is needed by [ApplyRunner.runTaskWithRetry] for until-eval (it
// reads register.self after the override). On terminal-error branches (nil
// task / bad address / module not found / timed_out / flow-control
// compile-error) selfRegister is nil — until never reaches those (the retry
// wrapper handles TIMED_OUT/FAILED by status instead).
//
// retry/until are NOT handled here — this is "one attempt → one TaskEvent"
// (contract unchanged); runTaskWithRetry drives the loop.
func (r *ApplyRunner) runTask(ctx context.Context, applyID string, idx int32, task *keeperv1.RenderedTask, registerByName map[string]any) (*keeperv1.TaskEvent, map[string]any) {
	ev := &keeperv1.TaskEvent{
		ApplyId: applyID,
		TaskIdx: idx,
	}
	if task == nil {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "internal.nil_task",
			Message: "RenderedTask is nil",
		}
		return ev, nil
	}

	// `module:` arrives as `<namespace>.<module>.<state>` (e.g.
	// "core.pkg.installed"). The plugin call takes `state` separately from the
	// name, so we split the address.
	modName, state, ok := config.SplitModuleAddr(task.GetModule())
	if !ok {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "module.bad_address",
			Module:  task.GetModule(),
			Message: fmt.Sprintf("expected <namespace>.<module>.<state>, got %q", task.GetModule()),
		}
		return ev, nil
	}

	mod, found := r.registry.Lookup(modName)
	if !found {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "module.not_found",
			Module:  modName,
			Message: fmt.Sprintf("module %q is not registered (task %q)", modName, task.GetName()),
		}
		return ev, nil
	}

	// Per-task timeout (DSL-core timeout:, destiny/tasks.md §9): a context
	// child of ctx with a deadline for one Apply attempt. taskCtx is a CHILD of
	// runCtx — when its deadline expires, runCtx.Err() stays nil, so the cancel
	// branch in Run (if runCtx.Err() != nil → CANCELLED) doesn't fire and the
	// TIMED_OUT status set below sticks. Empty timeout → no limit (only the
	// scenario ceiling applies); we deliberately don't introduce a per-task
	// default — it would break legitimately long-running core.archive/core.url.
	//
	// Parser is config.ParseDuration, the same one Keeper uses when PARSING
	// destiny (validateDurationField) and that core.url/Reaper use — the single
	// Soul Stack `duration` convention: Go forms plus the `<N>d` suffix
	// (docs/keeper/config.md → "Type conventions"). Plain time.ParseDuration
	// doesn't understand `30d` — Keeper would accept such a timeout and Soul
	// would silently drop it.
	taskCtx := ctx
	if to := task.GetTimeout(); to != "" {
		switch d, err := config.ParseDuration(to); {
		case err != nil:
			// Invalid duration string (defensive: Keeper already validated this
			// when parsing destiny, "should never happen"). Treated as "not set" +
			// warn, rather than failing on a housekeeping error.
			slog.Default().Warn("runtime: invalid task timeout, limit not applied",
				slog.String("task", task.GetName()),
				slog.String("module", modName),
				slog.String("timeout", to),
				slog.Any("error", err))
		case d > 0:
			var cancel context.CancelFunc
			taskCtx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		default:
			// d <= 0 (`0s`, `-1s`): treated as "no limit", NOT an instant deadline.
			// WithTimeout(ctx, 0) would expire immediately → a false TIMED_OUT
			// before the module even runs.
			slog.Default().Warn("runtime: non-positive task timeout, limit not applied",
				slog.String("task", task.GetName()),
				slog.String("module", modName),
				slog.String("timeout", to))
		}
	}

	// Soulprint injection (Variant A, ADR-018(b)): an in-process core module
	// implementing util.SoulprintAware (core.pkg / core.service) receives the
	// collected host facts BEFORE Apply — the primary source for backend
	// selection (pkg-mgr / init system), consistent with CEL
	// `soulprint.self.os.*`. Out-of-process plugins don't implement the
	// interface, so they don't get the facts (reserved for Variant B).
	if aware, ok := mod.(util.SoulprintAware); ok {
		aware.SetHostFacts(r.hostFacts)
	}

	stream := newInProcApplyStream(taskCtx)
	pluginReq := &pluginv1.ApplyRequest{
		State:  state,
		Params: task.GetParams(),
	}

	// Apply is a blocking call; the module sends ApplyEvent(s) to the stream and
	// returns nil or an error. The final event carries changed/failed; earlier
	// ones are diagnostic messages (ignored — TaskEvent carries only the final
	// status, MVP doesn't propagate progress).
	applyErr := mod.Apply(pluginReq, stream)
	stream.close()
	last := stream.lastEvent()

	// Per-task timeout expired (taskCtx's own deadline) while the parent ctx is
	// still alive (distinguishing timeout from CancelApply): task is TIMED_OUT.
	// Checked BEFORE inspecting applyErr — the module typically returns
	// ctx.Err() (DeadlineExceeded), and without this branch it would masquerade
	// as module.error.
	if taskCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
		ev.Error = &keeperv1.TaskError{
			Code:    "task.timed_out",
			Module:  modName,
			Message: fmt.Sprintf("task %q timed out after %s", task.GetName(), task.GetTimeout()),
		}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), last)
		return ev, nil
	}

	// The module's base outcome (changed/failed) before flow-control predicate
	// overrides. We keep the module's original error separate — failed_when:
	// false (ignore_errors) can override it, but even then it isn't lost: it's
	// preserved in register.self.ignored_error (see below).
	var (
		baseChanged bool
		baseFailed  bool
		moduleErr   *keeperv1.TaskError
	)
	switch {
	case applyErr != nil:
		baseFailed = true
		moduleErr = &keeperv1.TaskError{
			Code:    "module.error",
			Module:  modName,
			Message: applyErr.Error(),
		}
	case last == nil:
		// Apply returned nil but sent no event at all — a broken module, or a
		// no-op (e.g. Plan-only). Treated as OK with no changes; core MVP
		// modules always send a final event, see util.SendOK / SendChanged /
		// SendFailed.
	case last.GetFailed():
		baseFailed = true
		moduleErr = &keeperv1.TaskError{
			Code:    "module.failed",
			Module:  modName,
			Message: last.GetMessage(),
		}
	case last.GetChanged():
		baseChanged = true
	}

	// Flow-control AFTER Apply (ADR-012(d)): changed_when first (overrides
	// changed), then failed_when (overrides failed). Activation is
	// flow_context + register.* (prior tasks) + register.self.* (the FRESH
	// result: changed/failed/timed_out + output fields). selfRegister is built
	// from the module's BASE outcome — changed_when sees the raw changed,
	// failed_when sees the result with changed_when already applied (order
	// matters). timed_out is always false here — the TIMED_OUT branch was
	// handled above and never reaches this point.
	selfRegister := selfRegisterData(baseChanged, baseFailed, last)

	changed := baseChanged
	if cw := task.GetChangedWhen(); cw != "" {
		res, err := r.evalFlowPredicate(cw, task, mergeRegisterSelf(registerByName, selfRegister))
		if err != nil {
			r.logFlowControlError("changed_when", task, err)
			return flowControlErrorEvent(applyID, idx, "flowcontrol.changed_when_error", task, cw, err), nil
		}
		changed = res
		selfRegister["changed"] = changed
	}

	failed := baseFailed
	if fw := task.GetFailedWhen(); fw != "" {
		res, err := r.evalFlowPredicate(fw, task, mergeRegisterSelf(registerByName, selfRegister))
		if err != nil {
			r.logFlowControlError("failed_when", task, err)
			return flowControlErrorEvent(applyID, idx, "flowcontrol.failed_when_error", task, fw, err), nil
		}
		failed = res
		selfRegister["failed"] = failed
	}

	// Final status: failed takes priority over changed (FAILED is a distinct
	// terminal). changed_when set changed, failed_when set failed.
	switch {
	case failed:
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		// Error source: the module's own TaskError if it failed; a synthetic error
		// if failed_when:true artificially raised failed on an OK module.
		if moduleErr != nil {
			ev.Error = moduleErr
		} else {
			ev.Error = &keeperv1.TaskError{
				Code:    "flowcontrol.failed_when",
				Module:  modName,
				Message: fmt.Sprintf("failed_when %q evaluated to true", task.GetFailedWhen()),
			}
		}
	case changed:
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
	default:
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_OK
	}

	// register_data: the final flow-control outcome (changed/failed after
	// override) + output: fields, taken from selfRegister (already reflects
	// changed_when/failed_when).
	ev.RegisterData = registerStruct(selfRegister)

	// ignore_errors audit (ADR-012(d)): the module failed but failed_when:false
	// overrode it (outcome OK/CHANGED). The original error isn't lost — it goes
	// into register.self.ignored_error (visible to later predicates, reaches
	// audit via register_data). TaskEvent.error stays empty here — apply.proto
	// contract: error is set only on FAILED/TIMED_OUT.
	if !failed && moduleErr != nil {
		ev.RegisterData.GetFields()["ignored_error"] = structpb.NewStringValue(moduleErr.GetMessage())
		// until-eval (retry wrapper) sees register.self in the same shape as the
		// final register_data — set ignored_error on selfRegister too.
		selfRegister["ignored_error"] = moduleErr.GetMessage()
	}
	return ev, selfRegister
}

// selfRegisterData builds the register.self.* map for changed_when/failed_when
// predicates: this task's DSL-core (changed/failed/timed_out) + output: fields
// from the final ApplyEvent. timed_out is always false — TIMED_OUT is handled
// earlier in runTask and never reaches flow-control. This is the cel-activation
// form (map[string]any), like registerByName, not *structpb.Struct.
func selfRegisterData(changed, failed bool, last *pluginv1.ApplyEvent) map[string]any {
	self := map[string]any{
		"changed":   changed,
		"failed":    failed,
		"timed_out": false,
	}
	if last != nil && last.GetOutput() != nil {
		for k, v := range last.GetOutput().AsMap() {
			self[k] = v
		}
	}
	return self
}

// mergeRegisterSelf overlays register.self (this task's fresh result) onto the
// register index of prior tasks, without mutating the source map: changed_when/
// failed_when read both register.<prior>.* AND register.self.*.
func mergeRegisterSelf(prev map[string]any, self map[string]any) map[string]any {
	merged := make(map[string]any, len(prev)+1)
	for k, v := range prev {
		merged[k] = v
	}
	merged["self"] = self
	return merged
}

// registerStruct serializes the self-register map (changed/failed/timed_out +
// output) into *structpb.Struct for TaskEvent.register_data. skipped is always
// false here — a task that reached runTask wasn't skipped. output values are
// already structpb-shaped (from ApplyEvent.Output); re-wrapped via NewValue.
func registerStruct(self map[string]any) *structpb.Struct {
	fields := make(map[string]*structpb.Value, len(self)+1)
	for k, v := range self {
		val, err := structpb.NewValue(v)
		if err != nil {
			// output field isn't structpb-representable (should never happen — it
			// came from a *structpb.Struct already). Skip it rather than fail the run.
			continue
		}
		fields[k] = val
	}
	fields["skipped"] = structpb.NewBoolValue(false)
	return &structpb.Struct{Fields: fields}
}

// evalFlowPredicate evaluates changed_when/failed_when with the sandboxed
// flow-control engine. Activation is flow_context (input/vars/essence/
// incarnation/self) + register (prior tasks + register.self of the fresh
// result). Symmetric to evalWhen.
func (r *ApplyRunner) evalFlowPredicate(expr string, task *keeperv1.RenderedTask, reg map[string]any) (bool, error) {
	return r.flowEngine.EvalPredicate(expr, flowControlVars(task.GetFlowContext(), reg))
}

// flowControlErrorEvent builds the common TaskEvent for a runtime-/compile-error
// CEL in changed_when/failed_when: task FAILED (same as when, templating.md
// §10). Symmetric to the when-branch in Run.
func flowControlErrorEvent(applyID string, idx int32, code string, task *keeperv1.RenderedTask, expr string, err error) *keeperv1.TaskEvent {
	ev := &keeperv1.TaskEvent{
		ApplyId: applyID,
		TaskIdx: idx,
		Status:  keeperv1.TaskStatus_TASK_STATUS_FAILED,
		Error: &keeperv1.TaskError{
			Code:    code,
			Module:  task.GetModule(),
			Message: fmt.Sprintf("%s %q: %v", code, expr, err),
		},
	}
	ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
	return ev
}

// evalWhen evaluates the `when:` flow-control predicate (ADR-012(d)) with the
// Soul-side sandboxed cel-go engine. Empty when → (true, nil) (unconditional).
//
// Activation is built from RenderedTask.flow_context (a literal per-host
// snapshot { input, vars, essence, incarnation, self } assembled by Keeper in
// the CEL phase) + register (registerByName — prior tasks' payload by register
// name, built by Soul itself). soulprint binds to {self: flow_context.self} —
// the canonical soulprint.self.<path> form; soulprint.hosts/where are
// unavailable (sandbox isolation).
//
// changed_when/failed_when are evaluated separately, AFTER Apply, in
// [ApplyRunner.runTask] (override changed/failed by result); evalWhen only
// gates BEFORE Apply.
func (r *ApplyRunner) evalWhen(task *keeperv1.RenderedTask, registerByName map[string]any) (bool, error) {
	when := task.GetWhen()
	if when == "" {
		return true, nil
	}
	return r.flowEngine.EvalPredicate(when, flowControlVars(task.GetFlowContext(), registerByName))
}

// flowControlVars builds cel.Vars from the flow_context snapshot and the
// accumulated registerByName. flow_context is Keeper's data (input/vars/
// essence/incarnation/self); register is prior tasks' results (built by
// Soul). nil/missing sections → empty maps (a normal CEL no-such-key, not a
// panic).
func flowControlVars(flowCtx *structpb.Struct, registerByName map[string]any) cel.Vars {
	fc := map[string]any{}
	if flowCtx != nil {
		fc = flowCtx.AsMap()
	}
	return cel.Vars{
		Input:         flowSection(fc, "input"),
		Vars:          flowSection(fc, "vars"),
		Essence:       flowSection(fc, "essence"),
		Incarnation:   flowSection(fc, "incarnation"),
		SoulprintSelf: flowSection(fc, "self"),
		Register:      registerByName,
		// AllowHosts is intentionally false (zero-value): the flow-control engine
		// already forces soulprint.hosts isolation (NewFlowControl); this just
		// restates the intent.
	}
}

// flowSection extracts a top-level flow_context section as map[string]any.
// Missing/non-object → empty map (field access becomes a normal no-such-key).
func flowSection(fc map[string]any, key string) map[string]any {
	if sec, ok := fc[key].(map[string]any); ok {
		return sec
	}
	return map[string]any{}
}

// logFlowControlError logs a flow-control predicate evaluation error. A
// compile-/unsupported error on Soul is internal (Keeper should have rejected
// an invalid predicate during pre-render validation) — logged at warn so the
// keeper<->soul mismatch is visible in logs/OTel. A runtime-error CEL (e.g.
// missing register.x) is a normal Destiny-author mistake, also warn (the task
// becomes FAILED either way).
func (r *ApplyRunner) logFlowControlError(kind string, task *keeperv1.RenderedTask, err error) {
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(err, &ce) || errors.As(err, &ue) {
		slog.Default().Warn("runtime: flow-control predicate invalid on Soul (Keeper let it through - internal mismatch)",
			slog.String("kind", kind),
			slog.String("task", task.GetName()),
			slog.String("module", task.GetModule()),
			slog.Any("error", err))
		return
	}
	slog.Default().Warn("runtime: flow-control predicate runtime-error",
		slog.String("kind", kind),
		slog.String("task", task.GetName()),
		slog.String("module", task.GetModule()),
		slog.Any("error", err))
}

// recordRegister accumulates a finished/skipped task's register payload into
// both indexes: registerByIdx (by position, for onchanges gating) and
// registerByName (by register name, for when:/… flow-control predicates).
// Empty name → skip registerByName (a task without register: is addressable
// only by its idx). registerByName stores the payload as map[string]any (the
// cel-activation form), not *structpb.Struct — cel reads Go data via adapter.
func recordRegister(byIdx map[int32]*structpb.Struct, byName map[string]any, idx int32, name string, data *structpb.Struct) {
	byIdx[idx] = data
	if name != "" {
		byName[name] = data.AsMap()
	}
}

// skipOnChanges decides whether to skip a task per the DSL-core `onchanges:`
// (destiny/tasks.md §8). onchangesIdx is the source tasks' indexes (register
// names resolved by Keeper, proto onchanges_idx); registerByIdx is the
// register of already-run tasks in this run, by index.
//
// Semantics: empty onchangesIdx → false (unconditional run). Otherwise skip
// (true) UNLESS at least one source has register.changed == true — any
// changed source → false (run). A source missing from registerByIdx is
// treated as changed=false (it didn't run — e.g. was itself skipped): it
// doesn't "rescue" from the skip, consistent with skipped != changed.
func skipOnChanges(onchangesIdx []int32, registerByIdx map[int32]*structpb.Struct) bool {
	if len(onchangesIdx) == 0 {
		return false
	}
	for _, srcIdx := range onchangesIdx {
		rd := registerByIdx[srcIdx]
		if rd.GetFields()["changed"].GetBoolValue() {
			return false
		}
	}
	return true
}

// skipOnFail decides whether to skip an onfail task per the DSL-core `onfail:`
// (destiny/tasks.md §8) — the rescue mirror of skipOnChanges, triggered by
// register.failed instead of register.changed. onfailIdx is the source tasks'
// indexes (register names resolved by Keeper, proto onfail_idx); registerByIdx
// is the register of already-run tasks in this run, by index.
//
// Semantics: empty onfailIdx → false (not an onfail task — the when/onchanges/
// runFailed branch decides execution instead). Otherwise skip (true) UNLESS at
// least one source has register.failed == true — any failed source → false
// (run the rescue). register.failed also covers TIMED_OUT (written to register
// as failed==true by buildRegisterData), so a source's timeout also triggers
// onfail. A source missing from registerByIdx is treated as failed=false (it
// didn't run — e.g. was itself skipped): it doesn't "activate" onfail,
// consistent with skipped != failed.
func skipOnFail(onfailIdx []int32, registerByIdx map[int32]*structpb.Struct) bool {
	if len(onfailIdx) == 0 {
		return false
	}
	for _, srcIdx := range onfailIdx {
		rd := registerByIdx[srcIdx]
		if rd.GetFields()["failed"].GetBoolValue() {
			return false
		}
	}
	return true
}

// skippedTaskEvent builds the common TaskEvent for a skipped task (when/
// onchanges/onfail gating failed, OR the task was skipped after a run
// failure). register_data carries skipped=true, changed=false, failed=false —
// a skipped task triggers neither downstream onchanges nor onfail (skipped !=
// changed/failed).
func skippedTaskEvent(applyID string, idx int32) *keeperv1.TaskEvent {
	return &keeperv1.TaskEvent{
		ApplyId:      applyID,
		TaskIdx:      idx,
		Status:       keeperv1.TaskStatus_TASK_STATUS_SKIPPED,
		RegisterData: buildRegisterData(keeperv1.TaskStatus_TASK_STATUS_SKIPPED, nil),
	}
}

// buildRegisterData assembles the google.protobuf.Struct for
// TaskEvent.register_data from the final ApplyEvent. In MVP scope this is
// {changed, failed, timed_out, skipped, output...}; full schema —
// docs/destiny/tasks.md (register.<task>.*).
//
// skipped=true only for TASK_STATUS_SKIPPED (when/onchanges/onfail gating
// didn't let the task run, mod.Apply wasn't called). A skipped task carries
// changed=false — it does NOT trigger downstream onchanges (skipped != changed,
// destiny/tasks.md §8).
func buildRegisterData(status keeperv1.TaskStatus, last *pluginv1.ApplyEvent) *structpb.Struct {
	changed := status == keeperv1.TaskStatus_TASK_STATUS_CHANGED
	failed := status == keeperv1.TaskStatus_TASK_STATUS_FAILED ||
		status == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
	timedOut := status == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
	skipped := status == keeperv1.TaskStatus_TASK_STATUS_SKIPPED

	fields := map[string]*structpb.Value{
		"changed":   structpb.NewBoolValue(changed),
		"failed":    structpb.NewBoolValue(failed),
		"timed_out": structpb.NewBoolValue(timedOut),
		"skipped":   structpb.NewBoolValue(skipped),
	}
	if last != nil && last.GetOutput() != nil {
		for k, v := range last.GetOutput().GetFields() {
			fields[k] = v
		}
	}
	return &structpb.Struct{Fields: fields}
}

// aggregateRegisterData builds register_data for the TERMINAL synthetic
// applier-register task (core.noop.run with aggregate_of, Variant B
// materialization, orchestration.md §2.1.1) as a summary of the applier's
// child destiny tasks:
//
//	changed   = OR(registerByIdx[i].changed)
//	failed    = OR(registerByIdx[i].failed)
//	timed_out = OR(registerByIdx[i].timed_out)
//
// over aggregateOf's LOCAL indexes (Keeper's ToProtoTasks did the
// global→local remap). This mirrors register.<applier> semantics: an outer
// onchanges:[<applier>] / when: register.<applier>.changed resolves against
// this register_data. skipped is always false (the aggregate is the group's
// real outcome, not the task itself being skipped).
//
// A source missing from registerByIdx (sentinel index -1 from ToProtoTasks: a
// child task filtered out by where: on this host, or routed to a different
// Passage) reads as nil → its changed/failed/timed_out=false (zero
// contribution to the OR), symmetric to skipOnChanges/skipOnFail. An empty
// aggregateOf never reaches here (the caller checks len>0); if it did, all OR
// results would be false (a no-op applier with no children).
//
// Child tasks' output fields are NOT projected (out of scope — propagating a
// declared top-level output: destiny into register.<applier>.<field> is a
// separate slice). Only DSL-core changed/failed/timed_out/skipped.
func aggregateRegisterData(aggregateOf []int32, registerByIdx map[int32]*structpb.Struct) *structpb.Struct {
	var changed, failed, timedOut bool
	for _, idx := range aggregateOf {
		rd := registerByIdx[idx]
		fields := rd.GetFields()
		if fields["changed"].GetBoolValue() {
			changed = true
		}
		if fields["failed"].GetBoolValue() {
			failed = true
		}
		if fields["timed_out"].GetBoolValue() {
			timedOut = true
		}
	}
	return &structpb.Struct{Fields: map[string]*structpb.Value{
		"changed":   structpb.NewBoolValue(changed),
		"failed":    structpb.NewBoolValue(failed),
		"timed_out": structpb.NewBoolValue(timedOut),
		"skipped":   structpb.NewBoolValue(false),
	}}
}

// inProcApplyStream implements `grpc.ServerStreamingServer[pluginv1.ApplyEvent]`
// for in-process core-module calls. Emulates a server-stream via a slice;
// SetTrailer/SendHeader are no-ops (core modules don't use them).
//
// Keeps ALL ApplyEvents, but runtime only looks at the final one. Events in
// stream.events could be dumped in logs/debug mode; in MVP it's just a buffer.
type inProcApplyStream struct {
	grpc.ServerStream
	ctx     context.Context
	events  []*pluginv1.ApplyEvent
	hdr     metadata.MD
	trailer metadata.MD
	closed  bool
}

func newInProcApplyStream(ctx context.Context) *inProcApplyStream {
	return &inProcApplyStream{ctx: ctx, hdr: metadata.MD{}, trailer: metadata.MD{}}
}

func (s *inProcApplyStream) Context() context.Context { return s.ctx }

func (s *inProcApplyStream) Send(ev *pluginv1.ApplyEvent) error {
	if s.closed {
		return fmt.Errorf("inproc stream: Send after close")
	}
	s.events = append(s.events, ev)
	return nil
}

func (s *inProcApplyStream) SetHeader(md metadata.MD) error {
	s.hdr = metadata.Join(s.hdr, md)
	return nil
}
func (s *inProcApplyStream) SendHeader(md metadata.MD) error {
	s.hdr = metadata.Join(s.hdr, md)
	return nil
}
func (s *inProcApplyStream) SetTrailer(md metadata.MD) { s.trailer = metadata.Join(s.trailer, md) }
func (s *inProcApplyStream) SendMsg(m any) error {
	ev, ok := m.(*pluginv1.ApplyEvent)
	if !ok {
		return fmt.Errorf("inproc stream: SendMsg got %T, want *pluginv1.ApplyEvent", m)
	}
	return s.Send(ev)
}
func (s *inProcApplyStream) RecvMsg(any) error {
	return fmt.Errorf("inproc stream: RecvMsg not supported")
}

func (s *inProcApplyStream) close() { s.closed = true }

func (s *inProcApplyStream) lastEvent() *pluginv1.ApplyEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}
