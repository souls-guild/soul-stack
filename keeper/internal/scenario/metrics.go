package scenario

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// tracer for in-process spans of the scenario runner. Uses the global
// TracerProvider set up by [obs.SetupOTel] in main; when OTel is disabled the
// provider is no-op — spans are free and the code doesn't branch (ADR-024 §1.2).
var tracer = otel.Tracer("keeper/scenario")

// ScenarioMetrics — Prometheus collectors for the scenario runner
// (Keeper-side scenario run, ADR-009). Registered by a dedicated helper on
// top of the component-agnostic [obs.Registry] — same pattern as
// [grpc.RegisterGRPCMetrics] (pilot, ADR-024 §4.0): the registry core doesn't
// know about concrete metrics, keeper_scenario_* metrics are the runner's own.
//
// Metrics live here (keeper/internal/scenario) rather than shared/obs because
// they're tied to the keeper-internal run goroutine and not reused by Soul
// (ADR-011: shared/ is for truly cross-cutting code).
//
// Names follow Prometheus convention (snake_case, _total for counters,
// _seconds for duration histograms; ADR-024 §2.1). Labels are a closed enum
// (result), cardinality-safe (ADR-024 §2.2): incarnation/scenario name is NOT
// a label — that would blow up cardinality by incarnation/scenario count;
// that breakdown belongs in traces (the scenario.run span carries them as
// attributes).
type ScenarioMetrics struct {
	// runsTotal — count of finished scenario runs, split by terminal result
	// (`ok` — state committed; `failed` — run failed, incarnation moved to
	// error_locked; `locked` — run rejected before start because the
	// incarnation was already applying/error_locked).
	runsTotal *prometheus.CounterVec

	// runDuration — scenario run duration in seconds (from run-goroutine
	// start to terminal). Answers "how long do runs take"; no split by result
	// needed — the overall series is enough for p99, domain detail goes to trace.
	runDuration prometheus.Histogram
}

// Results for keeper_scenario_runs_total. Closed 3-value enum reflecting
// run-goroutine terminal outcomes (run.go): commit / abort / gate rejection
// before start.
const (
	runResultOK     = "ok"
	runResultFailed = "failed"
	runResultLocked = "locked"
)

// RegisterScenarioMetrics creates the keeper_scenario_* collectors and
// registers them in [obs.Registry]. Returns the handle for wiring via
// [Deps.Metrics].
//
// MustRegister: duplicate registration is a programmer error (called twice on
// the same Registry); failing fast is simpler than lazy init (same pattern as
// [grpc.RegisterGRPCMetrics]).
func RegisterScenarioMetrics(reg *obs.Registry) *ScenarioMetrics {
	m := &ScenarioMetrics{
		runsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_scenario_runs_total",
				Help: "Number of finished scenario runs, broken down by outcome (ok/failed/locked).",
			},
			[]string{"result"},
		),
		runDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_scenario_run_duration_seconds",
			Help:    "Duration of a scenario run in seconds (from run-goroutine start to terminal state).",
			Buckets: prometheus.DefBuckets,
		}),
	}
	reg.Registerer().MustRegister(m.runsTotal, m.runDuration)
	return m
}

// ObserveRun records a run's terminal: increments runs_total by result and
// (when duration > 0) records the duration in the histogram. nil receiver is
// a no-op: the runner may run without observability (unit tests, dev builds).
//
// duration <= 0 (run rejected before start, locked) doesn't feed the
// histogram — duration is only measured for runs that actually started.
func (m *ScenarioMetrics) ObserveRun(result string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.runsTotal.WithLabelValues(result).Inc()
	if durationSeconds > 0 {
		m.runDuration.Observe(durationSeconds)
	}
}
