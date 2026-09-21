package runtime

import (
	"github.com/prometheus/client_golang/prometheus"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// ApplyMetrics is the set of Prometheus collectors for the Soul daemon's
// apply cycle (runtime subsystem, ADR-012/ADR-015). Registered by a helper on
// top of the component-agnostic [obs.Registry] — the same pattern as
// [keeper/internal/grpc.RegisterGRPCMetrics] (docs/observability.md §4.0).
//
// Metrics live here (soul/internal/runtime), not in shared/obs: they're tied
// to the Soul agent's apply cycle and aren't reused by Keeper (ADR-011:
// shared/ is genuinely cross-cutting code only). Per ADR-011, soul doesn't
// import keeper; both sides are instrumented via the neutral shared/obs.
//
// Naming follows ADR-024 §2.1: soul_ prefix (component role), snake_case,
// _total for counters, histograms named by the measured quantity + _seconds.
// Labels are closed enums (§2.2): apply_id / sid are never labels —
// cardinality blow-up; that breakdown belongs in trace (span apply.run).
type ApplyMetrics struct {
	// tasksTotal counts completed run tasks, broken down by result
	// (`ok` / `changed` / `failed`). Closed 3-value enum; this is the metric
	// used as the soul_*-name example in naming-rules.
	tasksTotal *prometheus.CounterVec

	// applyDuration is the duration of one run (the whole Run), in seconds.
	// No labels: breaking down by apply_id is a cardinality risk (§2.2); the
	// distribution of run durations answers "how slow is apply".
	applyDuration prometheus.Histogram

	// taskRetries counts RETRY attempts of runTask (retry:/until:, DSL
	// flow-control core, destiny/tasks.md §9). Incremented on every attempt
	// from the second onward (the first isn't a retry). Growth signals
	// unstable tasks / flaky hosts. No labels: task/apply_id is a
	// cardinality risk (§2.2).
	taskRetries prometheus.Counter

	// taskSkipped counts tasks skipped by flow-control gating (mod.Apply never
	// called), broken down by reason. Closed enum: `when` (when: false),
	// `requisite` (onchanges/onfail didn't fire), `failed_run` (run already
	// failed, non-onfail task skipped by fail-stop).
	taskSkipped *prometheus.CounterVec

	// taskTimedOut counts tasks that ended in a timeout (TASK_STATUS_TIMED_OUT).
	// Broken out from the general failed result in soul_apply_tasks_total:
	// timeout is a distinct "stuck" signal, useful as its own series for
	// alerts. Counted by the task's final outcome (after retries are exhausted).
	taskTimedOut prometheus.Counter

	// applyFenced counts ApplyRequests rejected by the attempt-fencing guard
	// (ADR-027(g), Phase 2): a stale duplicate with attempt < the one already
	// seen for apply_id. Growth means either a recovery scan requeued a
	// still-alive Ward (lease shorter than apply) or a real double execution
	// that the guard caught. No apply_id label (cardinality, ADR-024 §2.2):
	// per-run breakdown belongs in trace, not the metric.
	applyFenced prometheus.Counter
}

// Skip reasons for soul_apply_task_skipped_total. Closed 3-value enum —
// flow-control gating in Run (when / requisite-onchanges-onfail / fail-stop).
const (
	skipReasonWhen      = "when"
	skipReasonRequisite = "requisite"
	skipReasonFailedRun = "failed_run"
)

// Results for soul_apply_tasks_total. Closed 3-value enum — mapped from
// [keeperv1.TaskStatus] (ok/changed/failed; timed_out and cancelled collapse
// into failed as terminal non-success outcomes).
const (
	applyResultOK      = "ok"
	applyResultChanged = "changed"
	applyResultFailed  = "failed"
)

// RegisterApplyMetrics creates the soul_apply_* collectors and registers them
// on [obs.Registry]. Returns the descriptor for wire-up via [ApplyRunner].
//
// MustRegister: a duplicate registration is a programmer error (Register
// called twice on the same Registry); failing fast beats lazy init here
// (same pattern as the RegisterGRPCMetrics pilot).
func RegisterApplyMetrics(reg *obs.Registry) *ApplyMetrics {
	m := &ApplyMetrics{
		tasksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "soul_apply_tasks_total",
				Help: "Number of completed run tasks, broken down by result (ok/changed/failed).",
			},
			[]string{"result"},
		),
		applyDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "soul_apply_duration_seconds",
			Help: "Duration of one apply run (the whole Run), in seconds.",
			// An apply run is heavier than an HTTP request (packages/files/
			// services): typical runs are seconds to tens of seconds, heavy
			// ones (compiling/large archives) run minutes. Can't narrow the
			// top to 5s like keeper_http — we'd lose the tail; extended to
			// 300s while keeping granularity at the low end.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}),
		taskRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_apply_task_retries_total",
			Help: "Number of runTask retry attempts (retry:/until:), excluding the first attempt.",
		}),
		taskSkipped: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "soul_apply_task_skipped_total",
				Help: "Number of tasks skipped by flow-control gating, by reason (when/requisite/failed_run).",
			},
			[]string{"reason"},
		),
		taskTimedOut: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_apply_task_timed_out_total",
			Help: "Number of tasks that ended in a timeout (by final outcome after retry).",
		}),
		applyFenced: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_apply_fenced_total",
			Help: "Number of ApplyRequests rejected by the attempt-fencing guard (stale duplicate, attempt < last seen).",
		}),
	}
	reg.Registerer().MustRegister(
		m.tasksTotal, m.applyDuration,
		m.taskRetries, m.taskSkipped, m.taskTimedOut, m.applyFenced,
	)
	return m
}

// ObserveTask increments the task counter by task run result.
// nil-receiver-safe no-op: ApplyRunner can run without an obs stack
// (unit tests, push mode without a metrics listener).
func (m *ApplyMetrics) ObserveTask(result string) {
	if m == nil {
		return
	}
	m.tasksTotal.WithLabelValues(result).Inc()
}

// ObserveApplyDuration records a run's duration in seconds.
// nil-receiver-safe no-op.
func (m *ApplyMetrics) ObserveApplyDuration(seconds float64) {
	if m == nil {
		return
	}
	m.applyDuration.Observe(seconds)
}

// ObserveRetry increments the runTask retry counter.
// nil-receiver-safe no-op.
func (m *ApplyMetrics) ObserveRetry() {
	if m == nil {
		return
	}
	m.taskRetries.Inc()
}

// ObserveSkipped increments the skipped-tasks counter by reason.
// nil-receiver-safe no-op.
func (m *ApplyMetrics) ObserveSkipped(reason string) {
	if m == nil {
		return
	}
	m.taskSkipped.WithLabelValues(reason).Inc()
}

// ObserveTimedOut increments the counter of tasks that ended in a timeout.
// nil-receiver-safe no-op.
func (m *ApplyMetrics) ObserveTimedOut() {
	if m == nil {
		return
	}
	m.taskTimedOut.Inc()
}

// ObserveFenced increments the counter of ApplyRequests rejected by the
// attempt-fencing guard (stale duplicate). nil-receiver-safe no-op.
func (m *ApplyMetrics) ObserveFenced() {
	if m == nil {
		return
	}
	m.applyFenced.Inc()
}

// taskResult collapses [keeperv1.TaskStatus] into the closed result label
// enum for soul_apply_tasks_total. changed → changed; failed/timed_out/cancelled
// → failed (terminal non-success); ok, skipped (onchanges gating), and
// everything else → ok.
func taskResult(status keeperv1.TaskStatus) string {
	switch status {
	case keeperv1.TaskStatus_TASK_STATUS_CHANGED:
		return applyResultChanged
	case keeperv1.TaskStatus_TASK_STATUS_FAILED,
		keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT,
		keeperv1.TaskStatus_TASK_STATUS_CANCELLED:
		return applyResultFailed
	default:
		return applyResultOK
	}
}
