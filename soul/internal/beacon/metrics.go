package beacon

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// BeaconMetrics — the set of Prometheus collectors for Soul-daemon's
// beacon-scheduler (ADR-030 S1/S4). Registered by a helper on top of the
// component-agnostic [obs.Registry] — the same pattern as
// [runtime.RegisterApplyMetrics] (docs/observability.md §4.0): the collector
// lives next to the subsystem, registration happens on the soul-registry.
//
// Metrics live here (soul/internal/beacon), not in shared/obs: they're bound
// to Soul agent's per-process beacon-scheduler and aren't reused by Keeper
// (ADR-011: shared/ is for genuinely cross-cutting code).
//
// Names follow ADR-024 §2.1: soul_ prefix (component role), snake_case,
// _total for counters. No labels (cardinality §2.2): breaking down by
// vigil-name is high-cardinality — that belongs in logs/traces, not metrics.
type BeaconMetrics struct {
	// portentsDropped — count of Portents dropped when the channel buffer
	// overflows ([Scheduler.emit] drop branch): the EventStream writer-loop is
	// lagging, or there's been no active session for a while. Dropping an
	// edge-triggered event loses one transition (the next State change raises
	// Portent again); nonzero growth signals "reactions are being lost", an
	// alert candidate.
	portentsDropped prometheus.Counter
}

// RegisterBeaconMetrics creates the soul_beacon_*-collectors and registers
// them on [obs.Registry]. Returns a handle for wiring through
// [SchedulerConfig].
//
// MustRegister: a duplicate registration is a programmer error (called twice
// on the same Registry); failing fast beats lazy init here (identical
// pattern to [runtime.RegisterApplyMetrics]).
func RegisterBeaconMetrics(reg *obs.Registry) *BeaconMetrics {
	m := &BeaconMetrics{
		portentsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_beacon_portents_dropped_total",
			Help: "Number of Portents dropped on channel buffer overflow (writer-loop lagging / no session).",
		}),
	}
	reg.Registerer().MustRegister(m.portentsDropped)
	return m
}

// ObservePortentDropped increments the dropped-Portents counter.
// nil receiver is a no-op: the scheduler can start without an obs stack
// (unit tests, metrics.enabled=false), so the caller doesn't need to check
// nil on every drop.
func (m *BeaconMetrics) ObservePortentDropped() {
	if m == nil {
		return
	}
	m.portentsDropped.Inc()
}
