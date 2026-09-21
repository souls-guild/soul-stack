package render

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// RenderMetrics — Prometheus collectors for Keeper's render pipeline
// (CEL+text/template scenario rendering, ADR-010). Registered by a separate
// helper on top of the component-agnostic [obs.Registry] — the same pattern as
// [grpc.RegisterGRPCMetrics] / [scenario.RegisterScenarioMetrics] (ADR-024
// §4.0): registry-core doesn't know about specific metrics, and
// keeper_render_* metrics are a Keeper render-facade specific.
//
// Metrics live here (keeper/internal/render), not in shared/obs, because
// they're tied to Keeper's internal [Pipeline] and aren't reused by Soul
// (ADR-011: shared/ is genuinely cross-cutting code; render is Keeper-side per
// ADR-012(d), Soul doesn't pull in cel-go/sprig).
//
// Names follow Prometheus convention (snake_case, _total for counters,
// _seconds for duration histograms; ADR-024 §2.1). Labels carry no secrets and
// no high cardinality (ADR-024 §2.2): incarnation/scenario name is NOT put in
// a label (blow-up by incarnation/scenario count) — that breakdown goes to
// trace instead (the render.pipeline span carries them as attributes,
// pipeline.go). Params / vault values never reach metrics at all.
type RenderMetrics struct {
	// duration — how long one [Pipeline.Render] pass takes, in seconds
	// (vault-resolve → CEL-render → on/where resolve → plan assembly). This is
	// the heaviest Keeper-side phase of a run (same scope as the
	// render.pipeline span). The histogram answers "how long does render
	// take"; no breakdown by outcome needed — the overall series covers p99,
	// errors are counted separately.
	duration prometheus.Histogram

	// errorsTotal — count of failed [Pipeline.Render] passes (any non-nil
	// error: ErrUnsupportedDSL / vault-resolve failure / CEL failure /
	// host-invariant failure). No reason label: detail goes to trace/log (the
	// render.pipeline span sets codes.Error); this counter is for alerting on
	// render error rate without needing the histogram.
	errorsTotal prometheus.Counter
}

// RegisterRenderMetrics creates the keeper_render_* collectors and registers
// them in [obs.Registry]. Returns a handle for wiring up via [NewPipeline].
//
// MustRegister: duplicate registration is a programmer error (called twice on
// the same Registry); failing fast is simpler than carrying lazy init
// (identical pattern to [grpc.RegisterGRPCMetrics]).
func RegisterRenderMetrics(reg *obs.Registry) *RenderMetrics {
	m := &RenderMetrics{
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_render_duration_seconds",
			Help:    "Duration of one scenario render pipeline pass in seconds (vault-resolve -> CEL-render -> on/where resolve).",
			Buckets: prometheus.DefBuckets,
		}),
		errorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_render_errors_total",
			Help: "Count of failed scenario render pipeline passes (any render error).",
		}),
	}
	reg.Registerer().MustRegister(m.duration, m.errorsTotal)
	return m
}

// ObserveRender records the completion of one [Pipeline.Render] pass:
// observes duration and, if err != nil, increments the error counter.
// nil receiver is a no-op: Pipeline can come up without observability
// (unit tests, dev builds, hermetic Trial).
func (m *RenderMetrics) ObserveRender(dur time.Duration, err error) {
	if m == nil {
		return
	}
	m.duration.Observe(dur.Seconds())
	if err != nil {
		m.errorsTotal.Inc()
	}
}
