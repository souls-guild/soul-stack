package errandrunner

import (
	"github.com/prometheus/client_golang/prometheus"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// Metrics — soul_errand_* collectors for the Errand runner (ADR-033).
// Registered by the [Register] helper on top of the component-agnostic
// [obs.Registry] — same pattern as [soul/internal/runtime.RegisterApplyMetrics]
// / keeper-side errand metrics. Labels are closed enums (ADR-024 §2.2):
// cardinality is safe.
//
// nil-receiver Observe* is a no-op: Runner can come up without an obs stack
// (push mode doesn't use Errand, unit tests run without obs).
type Metrics struct {
	// errandsTotal — count of completed Errands, sliced by terminal status.
	// Closed enum status: success / failed / timed_out / cancelled /
	// module_not_allowed. Mirrors keeper-side ResultEvent.Status.
	errandsTotal *prometheus.CounterVec

	// errandDuration — duration of a single Errand (from Run to return) in
	// seconds. Label module is a closed enum by core namespace (`core.cmd` /
	// `core.exec` / `core.http`). For a custom plugin it's `<namespace>.<name>`
	// without the state suffix: cardinality is bounded by the set of plugins
	// installed on a given host (tens at most, ADR-020).
	errandDuration *prometheus.HistogramVec
}

// Status label values for soul_errand_total. See [observeErrandLabel] for the mapping.
const (
	labelStatusSuccess          = "success"
	labelStatusFailed           = "failed"
	labelStatusTimedOut         = "timed_out"
	labelStatusCancelled        = "cancelled"
	labelStatusModuleNotAllowed = "module_not_allowed"
)

// Register creates the soul_errand_* collectors and registers them in
// [obs.Registry]. MustRegister: duplicate registration is a programmer error
// (same wiring pattern as RegisterApplyMetrics).
func Register(reg *obs.Registry) *Metrics {
	m := &Metrics{
		errandsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "soul_errand_total",
				Help: "Количество завершённых Errand-ов, разрезанное по терминалу.",
			},
			[]string{"status"},
		),
		errandDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "soul_errand_duration_seconds",
				Help: "Длительность одного Errand-а в секундах, по модулю (без state-суффикса).",
				// Errand is a single shell/exec/probe, typically < a second;
				// server-side dispatch cap is 30s, hard cap 300s. Buckets cover
				// a fast shell (50ms), a typical probe (250ms), a slow exec
				// (a few seconds), and the timed_out upper bound (300s).
				Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
			},
			[]string{"module"},
		),
	}
	reg.Registerer().MustRegister(m.errandsTotal, m.errandDuration)
	return m
}

// ObserveErrand increments the terminal-status counter.
// nil receiver is a no-op.
func (m *Metrics) ObserveErrand(status keeperv1.ErrandStatus) {
	if m == nil {
		return
	}
	m.errandsTotal.WithLabelValues(statusLabel(status)).Inc()
}

// ObserveDuration records an Errand's duration in seconds. module is the
// fully-qualified `<namespace>.<name>.<state>` (as in the request), but the
// label gets `<namespace>.<name>` — closed over the core set (the state
// suffix varies and would blow up cardinality). An empty module (early
// reject before resolve) → `unknown`.
func (m *Metrics) ObserveDuration(module string, seconds float64) {
	if m == nil {
		return
	}
	m.errandDuration.WithLabelValues(moduleLabel(module)).Observe(seconds)
}

// statusLabel — closed mapping ErrandStatus → label value.
// UNSPECIFIED / RUNNING aren't terminal and shouldn't reach here; defensive →
// "failed" (terminal bucket-by-default).
func statusLabel(s keeperv1.ErrandStatus) string {
	switch s {
	case keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS:
		return labelStatusSuccess
	case keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT:
		return labelStatusTimedOut
	case keeperv1.ErrandStatus_ERRAND_STATUS_CANCELLED:
		return labelStatusCancelled
	case keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED:
		return labelStatusModuleNotAllowed
	default:
		return labelStatusFailed
	}
}

// moduleLabel strips the state suffix (`core.cmd.shell` → `core.cmd`). Empty
// input → "unknown" (early reject before address parsing).
func moduleLabel(full string) string {
	if full == "" {
		return "unknown"
	}
	// This duplicates splitModuleAddr (12 lines), but here we only need
	// `<ns>.<name>` without the ok flag — a simplified rfind.
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '.' {
			if i > 0 {
				return full[:i]
			}
			return "unknown"
		}
	}
	return full
}
