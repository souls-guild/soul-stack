package serviceregistry

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// RegistryMetrics is the set of Prometheus collectors for the Service/keeper_settings
// registry (ADR-029, snapshot-rebuild + cluster invalidation). Mirrors keeper_rbac_snapshot_*
// for the registry's Holder (same SetMetrics/ObserveRebuild*/ObserveInvalidation pattern
// as [rbac.Holder]); registered by a separate helper on top of the component-
// agnostic [obs.Registry] (ADR-024 §4.0): registry-core doesn't know about specific
// metrics, keeper_serviceregistry_*-metrics are this Holder's own business.
//
// Metrics live here (keeper/internal/serviceregistry), not in shared/obs, because
// they're tied to the keeper-internal [Holder] and aren't reused by Soul
// (ADR-011: shared/ is truly cross-cutting code; the Service registry is Keeper-side).
//
// SECURITY + cardinality (ADR-024 §2.2): labels carry NO Service name,
// git/ref, or setting value. The rebuild-error breakdown is only the closed-enum
// `kind` (load/parse). The registry snapshot is a public catalog (names/git-refs),
// not a secret, but cardinality by Service count is still unacceptable in metrics.
//
// Names follow Prometheus convention (snake_case, _total for a counter, _seconds for
// a duration histogram, _timestamp_seconds for absolute time; ADR-024 §2.1).
type RegistryMetrics struct {
	// rebuildDuration — duration of one registry snapshot rebuild in seconds
	// (src.Load from the DB: ListServices + GetSetting). Observed on every
	// [Holder.Refresh] (TTL-poll, pub/sub invalidation, lazy path), regardless
	// of outcome.
	rebuildDuration prometheus.Histogram

	// rebuildErrorsTotal — counter of failed snapshot rebuilds, broken down by
	// failure phase: `load` — src.Load (DB unavailable/SELECT error), `parse` —
	// building the snapshot from rows (reserved: the current PoolSource.Load has
	// no separate parse phase, but symmetry with rbac keeps the branch for a future
	// typed decoder). Failure detail lives in the caller's log/trace.
	rebuildErrorsTotal *prometheus.CounterVec

	// lastSuccessTimestamp — Unix time of the last SUCCESSFUL snapshot rebuild.
	// Snapshot age is computed in PromQL as `time() - <this gauge>`.
	lastSuccessTimestamp prometheus.Gauge

	// services — number of Services in the current snapshot (gauge, updated on
	// every successful Refresh).
	services prometheus.Gauge

	// invalidationsTotal — counter of accepted cluster-wide registry invalidations
	// ([Holder.WatchInvalidations] callback). Incremented on every signal, before
	// the re-read runs. Self-origin is already filtered out by the source.
	invalidationsTotal prometheus.Counter
}

// Snapshot-rebuild failure phases for keeper_serviceregistry_snapshot_rebuild_errors_total.
// Closed enum: `load` — src.Load from the DB failed; `parse` — building the snapshot
// from read rows failed (reserved, see [RegistryMetrics.rebuildErrorsTotal]).
const (
	rebuildErrorLoad  = "load"
	rebuildErrorParse = "parse"
)

// RegisterRegistryMetrics creates the keeper_serviceregistry_*-collectors and
// registers them in [obs.Registry]. Returns a handle for wiring via
// [Holder.SetMetrics].
//
// MustRegister: a duplicate registration is a programmer error (called twice on
// the same Registry); failing fast is simpler than carrying lazy init (pattern
// identical to [rbac.RegisterRBACMetrics]).
func RegisterRegistryMetrics(reg *obs.Registry) *RegistryMetrics {
	m := &RegistryMetrics{
		rebuildDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_serviceregistry_snapshot_rebuild_duration_seconds",
			Help:    "Duration of registry snapshot rebuild in seconds (Load from DB).",
			Buckets: prometheus.DefBuckets,
		}),
		rebuildErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_serviceregistry_snapshot_rebuild_errors_total",
				Help: "Count of failed snapshot rebuilds, broken down by kind (load/parse).",
			},
			[]string{"kind"},
		),
		lastSuccessTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_serviceregistry_snapshot_last_success_timestamp_seconds",
			Help: "Unix time of the last successful snapshot rebuild (age = time() - this gauge).",
		}),
		services: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_serviceregistry_snapshot_services",
			Help: "Number of Services in the current snapshot.",
		}),
		invalidationsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_serviceregistry_invalidations_received_total",
			Help: "Count of accepted cluster-wide registry invalidations (pub/sub signals).",
		}),
	}
	reg.Registerer().MustRegister(
		m.rebuildDuration,
		m.rebuildErrorsTotal,
		m.lastSuccessTimestamp,
		m.services,
		m.invalidationsTotal,
	)
	return m
}

// ObserveRebuildSuccess records a successful snapshot rebuild ([Holder.Refresh]):
// observes the duration, updates last_success_timestamp (Unix-now), and the gauge
// of Service count from the current snapshot. nil receiver — no-op (Holder can
// start without observability: NewHolder in the bootstrap path / unit tests before
// metrics are wired up).
func (m *RegistryMetrics) ObserveRebuildSuccess(dur time.Duration, serviceCount int) {
	if m == nil {
		return
	}
	m.rebuildDuration.Observe(dur.Seconds())
	m.lastSuccessTimestamp.Set(float64(time.Now().Unix()))
	m.services.Set(float64(serviceCount))
}

// ObserveRebuildError records a failed snapshot rebuild: observes the
// duration and increments rebuild_errors_total with an explicit failure phase.
// The caller ([Holder.Refresh]) knows the phase precisely, so it's passed in
// rather than guessed from the error type. nil receiver — no-op.
func (m *RegistryMetrics) ObserveRebuildError(dur time.Duration, kind string) {
	if m == nil {
		return
	}
	m.rebuildDuration.Observe(dur.Seconds())
	m.rebuildErrorsTotal.WithLabelValues(kind).Inc()
}

// ObserveInvalidation increments invalidations_received_total by one
// accepted cluster signal. nil receiver — no-op.
func (m *RegistryMetrics) ObserveInvalidation() {
	if m == nil {
		return
	}
	m.invalidationsTotal.Inc()
}
