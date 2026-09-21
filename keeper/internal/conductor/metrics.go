package conductor

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// ConductorMetrics is the set of Conductor Prometheus collectors (ADR-048 §5,
// dashboard "leader alive, spawn on schedule"). Parity with reaper metrics
// (keeper/internal/reaper/metrics.go), names from the Soul Stack dictionary —
// `keeper_conductor_*`.
//
// Registered separately via [RegisterConductorMetrics] only on the branch
// where Conductor is actually started (cardinality-safe: with default-OFF
// without Redis, or explicit `cadence_scheduler.enabled: false`, collectors
// aren't published at all) — wire-up is done by keeper/cmd/keeper/daemon.go
// exactly where [Scheduler] is started.
//
// The collector lives next to its subsystem (docs/observability.md §4.0):
// keeper-specific metrics belong in the conductor package, not shared/obs.
type ConductorMetrics struct {
	// LeaseHeld is 1 if this Keeper instance holds the Redis lease
	// conductor:leader, 0 otherwise. Gauge with no labels: cluster-wide
	// invariant `sum(keeper_conductor_lease_held) == 1` (with exactly one
	// leader). The Conductor leader is independent of the Reaper leader
	// (separate lease, ADR-048 §1) — holders may differ. A per-`kid` label
	// would be redundant: it duplicates the target's Prometheus `instance`
	// label.
	LeaseHeld prometheus.Gauge

	// SpawnExecutions counts spawn ticks (incremented on every leader tick,
	// regardless of whether any due schedules were found). Comparing with
	// [SpawnedTotal] shows "efficiency": many ticks with zero spawns means no
	// schedules are due, or all are skip/queue.
	SpawnExecutions prometheus.Counter

	// SpawnedTotal is the total of spawned Voyages (skip/queue ticks don't
	// count — affected = "how many runs were actually created", parity with
	// CadenceSpawner.Run).
	SpawnedTotal prometheus.Counter

	// SpawnErrors counts spawn-tick errors (Spawner.Run returned an error).
	// Separate from [SpawnExecutions] so it can be alerted on without knowing
	// the histogram.
	SpawnErrors prometheus.Counter

	// SpawnDuration is the duration of a Spawner.Run call in seconds. Buckets
	// are parity with reaper-rule-duration: a typical spawn tick is single-
	// to-tens of ms (SELECT due + per-row insert); the 30s top bucket catches
	// an anomalously long tick separately, instead of `+Inf`.
	SpawnDuration prometheus.Histogram
}

// RegisterConductorMetrics creates the collectors and registers them in the
// Registry. MustRegister: duplicate registration is a programmer error
// (called twice).
func RegisterConductorMetrics(r *obs.Registry) *ConductorMetrics {
	m := &ConductorMetrics{
		LeaseHeld: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_conductor_lease_held",
			Help: "1 if this Keeper instance holds the Redis lease for Conductor leadership (conductor:leader), 0 otherwise.",
		}),
		SpawnExecutions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_conductor_spawn_executions_total",
			Help: "Number of due-Cadence spawn ticks by the Conductor leader.",
		}),
		SpawnedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_conductor_spawned_total",
			Help: "Number of Voyages spawned by the Conductor from due Cadences.",
		}),
		SpawnErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_conductor_spawn_errors_total",
			Help: "Number of Conductor spawn tick errors (Spawner returned an error).",
		}),
		SpawnDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_conductor_spawn_duration_seconds",
			Help:    "Duration of the Conductor due-Cadence spawn tick, in seconds.",
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}),
	}
	r.Registerer().MustRegister(m.LeaseHeld, m.SpawnExecutions, m.SpawnedTotal, m.SpawnErrors, m.SpawnDuration)
	return m
}

// ObserveSpawn is the single helper for recording metrics of one spawn tick.
// nil-safe (Conductor can be initialized without observability — tests). The
// caller doesn't need to check nil on every tick.
func (m *ConductorMetrics) ObserveSpawn(spawned int64, err error, dur time.Duration) {
	if m == nil {
		return
	}
	m.SpawnExecutions.Inc()
	m.SpawnDuration.Observe(dur.Seconds())
	if err != nil {
		m.SpawnErrors.Inc()
		return
	}
	if spawned > 0 {
		m.SpawnedTotal.Add(float64(spawned))
	}
}

// SetLeaseHeld is a nil-safe write to the leadership Gauge. Wired via
// [Config.OnLeaseChange] (parity with reaper's SetLeaseHeld via
// OnLeaseChange).
func (m *ConductorMetrics) SetLeaseHeld(held bool) {
	if m == nil {
		return
	}
	if held {
		m.LeaseHeld.Set(1)
		return
	}
	m.LeaseHeld.Set(0)
}
