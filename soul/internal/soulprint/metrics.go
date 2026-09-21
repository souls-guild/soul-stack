package soulprint

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// SoulprintMetrics is the set of Prometheus collectors for host-fact
// collection (Soulprint, ADR-018). Registered via a helper over the
// component-agnostic [obs.Registry] — the same pattern as the
// [keeper/internal/grpc.RegisterGRPCMetrics] pilot (docs/observability.md §4.0).
//
// Metrics live here (soul/internal/soulprint): fact collection is a Soul-side
// operation, not reused by Keeper (ADR-011). Per ADR-011 soul does NOT import
// keeper; instrumentation goes through the neutral shared/obs.
//
// Naming per ADR-024 §2.1: soul_ prefix, snake_case, _total for counters,
// histogram by magnitude + _seconds. Labels are a closed enum (§2.2): sid is
// not a label (cardinality) — per-host breakdown goes through OTel
// resource-attrs (§3).
type SoulprintMetrics struct {
	// collectionsTotal counts fact-collection snapshots, split by result
	// (`ok` / `failed`). Collect is best-effort and never returns an error
	// (ADR-018), so only `ok` is currently incremented; `failed` is reserved
	// for future fatal collection scenarios (closed 2-value enum).
	collectionsTotal *prometheus.CounterVec

	// collectDuration is the duration of one fact snapshot (Collect), in
	// seconds. Collection is lightweight (reads /proc, /etc/os-release,
	// net.*) — tens of ms; the histogram catches regressions (e.g. a slow
	// DNS lookup during FQDN resolution).
	collectDuration prometheus.Histogram
}

// Results for soul_soulprint_collections_total. Closed 2-value enum.
const (
	collectResultOK     = "ok"
	collectResultFailed = "failed"
)

// RegisterSoulprintMetrics creates the soul_soulprint_* collectors and
// registers them in [obs.Registry]. Returns the handle for wiring via
// [Collector].
//
// MustRegister: a duplicate registration is a programmer error — failing
// fast beats lazy init (same pattern as the RegisterGRPCMetrics pilot).
func RegisterSoulprintMetrics(reg *obs.Registry) *SoulprintMetrics {
	m := &SoulprintMetrics{
		collectionsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "soul_soulprint_collections_total",
				Help: "Number of host fact snapshots, broken down by result (ok/failed).",
			},
			[]string{"result"},
		),
		collectDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "soul_soulprint_collect_duration_seconds",
			Help: "Duration of one host fact snapshot (Collect), in seconds.",
			// Fact collection is lightweight (local reads), target is
			// milliseconds. Narrow low buckets catch the norm; up to 5s at
			// the top covers a slow FQDN/DNS resolve on a bad host.
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}),
	}
	reg.Registerer().MustRegister(m.collectionsTotal, m.collectDuration)
	return m
}

// ObserveCollection increments the snapshot counter by result.
// nil receiver is a no-op: Collector may run without an obs stack
// (unit tests, push mode with no metrics listener).
func (m *SoulprintMetrics) ObserveCollection(result string) {
	if m == nil {
		return
	}
	m.collectionsTotal.WithLabelValues(result).Inc()
}

// ObserveCollectDuration records the snapshot duration in seconds.
// nil receiver is a no-op.
func (m *SoulprintMetrics) ObserveCollectDuration(seconds float64) {
	if m == nil {
		return
	}
	m.collectDuration.Observe(seconds)
}
