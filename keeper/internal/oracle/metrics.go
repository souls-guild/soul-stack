package oracle

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// OracleMetrics is the set of Prometheus collectors for the Oracle reactor router (ADR-030
// S4: safety + observability). Registered by a helper on top of the component-
// agnostic [obs.Registry] (ADR-024 §4.0) — the same pattern as
// [augur.RegisterBrokerMetrics] / [reaper.RegisterReaperMetrics]: the registry core
// doesn't know about specific metrics, keeper_oracle_* metrics are Oracle's own concern.
//
// The metrics live here (keeper/internal/oracle), not in shared/obs: they're tied
// to the keeper-internal Portent→Decree reactor and aren't reused by the Soul
// (ADR-011: shared/ is truly cross-cutting code; Oracle is Keeper-side).
//
// SECURITY + cardinality (ADR-024 §2.2): we do NOT put decree-name,
// sid, apply_id, beacon-name, or payload in labels — that's high-cardinality (tens of
// thousands of hosts × rules) and/or untrusted input. Who exactly fired is in the audit log
// (`oracle.fired` / `decree.circuit_tripped`) and trace, not in the metric. All
// collectors are label-free (there's no closed-enum split here: a single Oracle stream).
//
// Names follow Prometheus convention (snake_case, _total for counters; ADR-024 §2.1),
// prefix keeper_ (component role).
type OracleMetrics struct {
	// portentsReceived is the counter of accepted PortentEvents (input to
	// handlePortentEvent with a non-empty beacon_name). The denominator for the rest:
	// how many beacon events reached the reactor at all.
	portentsReceived prometheus.Counter

	// decreesMatched is the counter of Decree fires that passed the entire filter
	// (subject-match + membership + where-CEL + NOT in cooldown) and reached
	// enqueuing. Incremented per-Decree (one Portent can match
	// multiple Decrees). Lower than portentsReceived due to default-deny.
	decreesMatched prometheus.Counter

	// scenariosEnqueued is the counter of named scenarios successfully enqueued
	// to the work-queue (ADR-027) by the Oracle reaction. Equal to the number of recorded fires;
	// a discrepancy with decreesMatched means enqueue failures (see the log).
	scenariosEnqueued prometheus.Counter

	// cooldownBlocked is the counter of Decree fires cut off by the cooldown
	// per-(decree, subject) (loop-prevention, ADR-030(a)). Growth means frequent
	// edge events on one rule; the first barrier against a storm is working.
	cooldownBlocked prometheus.Counter

	// circuitTripped is the counter of auto-disables of a Decree by the circuit breaker (ADR-030(a):
	// N fires within a window → enabled=false + alert). Any nonzero increase is
	// an abnormal situation (a rule went into a loop and was suppressed),
	// an alert candidate.
	circuitTripped prometheus.Counter
}

// RegisterOracleMetrics creates the keeper_oracle_* collectors and registers them in
// [obs.Registry]. Returns the descriptor for wire-up through [grpc.OracleDeps].
//
// MustRegister: duplicate registration is a programmer error (called twice on the same
// Registry); failing fast is more convenient than lazy initialization (the pattern is identical to
// [augur.RegisterBrokerMetrics] / [reaper.RegisterReaperMetrics]).
func RegisterOracleMetrics(reg *obs.Registry) *OracleMetrics {
	m := &OracleMetrics{
		portentsReceived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_portents_received_total",
			Help: "Number of PortentEvents accepted by the Oracle reactor (with non-empty beacon_name).",
		}),
		decreesMatched: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_decrees_matched_total",
			Help: "Number of Decree triggers that passed the full filter (subject/membership/where/cooldown) and reached dispatch.",
		}),
		scenariosEnqueued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_scenarios_enqueued_total",
			Help: "Number of named-scenarios successfully queued to the work-queue by an Oracle reaction.",
		}),
		cooldownBlocked: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_cooldown_blocked_total",
			Help: "Number of Decree triggers cut off by per-(decree, subject) cooldown (loop-prevention).",
		}),
		circuitTripped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_circuit_tripped_total",
			Help: "Number of Decree auto-disables by the circuit-breaker (N triggers within a window -> enabled=false).",
		}),
	}
	reg.Registerer().MustRegister(
		m.portentsReceived, m.decreesMatched,
		m.scenariosEnqueued, m.cooldownBlocked, m.circuitTripped,
	)
	return m
}

// ObservePortentReceived increments the counter of accepted Portents.
// A nil receiver is a no-op: the Oracle handler can come up without the obs stack
// (unit tests, builds without a metrics listener), the caller doesn't check for nil.
func (m *OracleMetrics) ObservePortentReceived() {
	if m == nil {
		return
	}
	m.portentsReceived.Inc()
}

// ObserveDecreeMatched increments the counter of Decrees that reached enqueuing.
// A nil receiver is a no-op.
func (m *OracleMetrics) ObserveDecreeMatched() {
	if m == nil {
		return
	}
	m.decreesMatched.Inc()
}

// ObserveScenarioEnqueued increments the counter of successfully enqueued scenarios.
// A nil receiver is a no-op.
func (m *OracleMetrics) ObserveScenarioEnqueued() {
	if m == nil {
		return
	}
	m.scenariosEnqueued.Inc()
}

// ObserveCooldownBlocked increments the counter of fires cut off by the
// cooldown. A nil receiver is a no-op.
func (m *OracleMetrics) ObserveCooldownBlocked() {
	if m == nil {
		return
	}
	m.cooldownBlocked.Inc()
}

// ObserveCircuitTripped increments the counter of auto-disables of a Decree by the circuit
// breaker. A nil receiver is a no-op.
func (m *OracleMetrics) ObserveCircuitTripped() {
	if m == nil {
		return
	}
	m.circuitTripped.Inc()
}
