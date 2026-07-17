package rbac

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// RBACMetrics is the set of Prometheus collectors for Keeper's RBAC
// subsystem (snapshot rebuild + permission checks + cluster invalidation,
// ADR-028). Registered via a dedicated helper on top of the component-agnostic
// [obs.Registry] — same pattern as [scenario.RegisterScenarioMetrics] /
// [vault.RegisterVaultMetrics] (ADR-024 §4.0): the registry core knows
// nothing about specific metrics; keeper_rbac_* metrics are an RBAC-facade
// concern.
//
// Metrics live here (keeper/internal/rbac), not in shared/obs, because they're
// tied to Keeper-internal [Holder]/[Enforcer] and aren't reused by Soul
// (ADR-011: shared/ is truly cross-cutting code; RBAC is Keeper-side).
//
// SECURITY + cardinality (ADR-024 §2.2, ADR-028 invariant): labels never
// carry aid, permission, role_name, or resource/action. The only cut is a
// closed enum: rebuild error `kind` (load/parse) and check `result`
// (allow/deny). Who checked what goes to audit-log/trace, not metrics.
//
// Names follow Prometheus convention (snake_case, _total for counters,
// _seconds for duration histograms, _timestamp_seconds for absolute time;
// ADR-024 §2.1).
type RBACMetrics struct {
	// rebuildDuration is the duration of one RBAC snapshot rebuild in seconds
	// (src.Load from DB → NewEnforcerFromSnapshot). Observed on every
	// [Holder.Refresh] (TTL poll, pub/sub invalidation, lazy path), regardless
	// of outcome.
	rebuildDuration prometheus.Histogram

	// rebuildErrorsTotal counts failed snapshot rebuilds, cut by failure
	// phase: `load` — src.Load (DB unreachable / SELECT error), `parse` —
	// NewEnforcerFromSnapshot (invalid permission in DB after a catalog
	// version desync). Failure detail goes to the caller's log/trace.
	rebuildErrorsTotal *prometheus.CounterVec

	// lastSuccessTimestamp is the Unix time of the last SUCCESSFUL snapshot
	// rebuild. Snapshot age is computed in PromQL as `time() - <this gauge>`;
	// we deliberately skip a separate _age_seconds metric since a gauge-based
	// age would go stale between scrapes.
	lastSuccessTimestamp prometheus.Gauge

	// roles is the number of roles in the current snapshot (gauge, updated on
	// every successful Refresh).
	roles prometheus.Gauge

	// operators is the number of operators with >=1 role binding in the
	// current snapshot (gauge). An AID with no bindings never enters the
	// snapshot enforcer — that's default-deny and isn't counted here.
	operators prometheus.Gauge

	// checksTotal counts permission checks ([Holder.Check]), cut by outcome:
	// `allow` (err==nil) / `deny` (any non-nil error, including a
	// misconfigured call). This is the hot path for admin-API/MCP before
	// tool execution.
	checksTotal *prometheus.CounterVec

	// invalidationsTotal counts received cluster-wide RBAC invalidations
	// ([Holder.WatchInvalidations] callback). Incremented on every signal,
	// before the reload runs. Self-origin signals are already filtered out
	// by the source.
	invalidationsTotal prometheus.Counter
}

// Snapshot rebuild failure phases for keeper_rbac_snapshot_rebuild_errors_total.
// Closed 2-value enum — splits "DB unreachable" (load) from "corrupt catalog"
// (parse): these need different alerting.
const (
	rebuildErrorLoad  = "load"
	rebuildErrorParse = "parse"
)

// Permission check outcomes for keeper_rbac_checks_total. Closed 2-value
// enum; deny aggregates both an explicit ErrPermissionDenied and a
// misconfigured call (empty resource/action) — both surface as 403, so no
// further split is needed.
const (
	checkResultAllow = "allow"
	checkResultDeny  = "deny"
)

// RegisterRBACMetrics creates the keeper_rbac_* collectors and registers them
// on [obs.Registry]. Returns the descriptor for wire-up via [Holder.SetMetrics].
//
// MustRegister: a duplicate registration is a programmer error (called twice
// on the same Registry); failing fast is simpler than carrying lazy init
// (same pattern as [scenario.RegisterScenarioMetrics]).
func RegisterRBACMetrics(reg *obs.Registry) *RBACMetrics {
	m := &RBACMetrics{
		rebuildDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_rbac_snapshot_rebuild_duration_seconds",
			Help:    "Duration of RBAC snapshot rebuild in seconds (Load from DB -> NewEnforcerFromSnapshot).",
			Buckets: prometheus.DefBuckets,
		}),
		rebuildErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_rbac_snapshot_rebuild_errors_total",
				Help: "Number of failed RBAC snapshot rebuilds, split by kind (load/parse).",
			},
			[]string{"kind"},
		),
		lastSuccessTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_rbac_snapshot_last_success_timestamp_seconds",
			Help: "Unix time of the last successful RBAC snapshot rebuild (age = time() - this gauge).",
		}),
		roles: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_rbac_snapshot_roles",
			Help: "Number of roles in the current RBAC snapshot.",
		}),
		operators: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_rbac_snapshot_operators",
			Help: "Number of operators with >=1 role binding in the current RBAC snapshot.",
		}),
		checksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_rbac_checks_total",
				Help: "Number of RBAC permission checks, split by result (allow/deny).",
			},
			[]string{"result"},
		),
		invalidationsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_rbac_invalidations_received_total",
			Help: "Number of accepted cluster-wide RBAC invalidations (pub/sub signals).",
		}),
	}
	reg.Registerer().MustRegister(
		m.rebuildDuration,
		m.rebuildErrorsTotal,
		m.lastSuccessTimestamp,
		m.roles,
		m.operators,
		m.checksTotal,
		m.invalidationsTotal,
	)
	return m
}

// ObserveRebuildSuccess records a successful snapshot rebuild ([Holder.Refresh]):
// observes duration, updates last_success_timestamp (Unix now), and sets the
// roles/operators gauges from the current snapshot.
//
// nil receiver is a no-op: Holder can come up without observability
// (NewHolder on the bootstrap path / unit tests before metrics wire-up).
func (m *RBACMetrics) ObserveRebuildSuccess(dur time.Duration, roleCount, operatorCount int) {
	if m == nil {
		return
	}
	m.rebuildDuration.Observe(dur.Seconds())
	m.lastSuccessTimestamp.Set(float64(time.Now().Unix()))
	m.roles.Set(float64(roleCount))
	m.operators.Set(float64(operatorCount))
}

// ObserveRebuildError records a failed snapshot rebuild: observes duration
// and increments rebuild_errors_total with the explicit failure phase
// (`load` — src.Load from DB, `parse` — NewEnforcerFromSnapshot). The caller
// ([Holder.Refresh]) knows the phase precisely, so it's passed in rather than
// guessed from the error type. nil receiver is a no-op.
func (m *RBACMetrics) ObserveRebuildError(dur time.Duration, kind string) {
	if m == nil {
		return
	}
	m.rebuildDuration.Observe(dur.Seconds())
	m.rebuildErrorsTotal.WithLabelValues(kind).Inc()
}

// ObserveCheck increments checks_total by the outcome of one [Holder.Check]:
// err==nil → allow, otherwise → deny. nil receiver is a no-op.
func (m *RBACMetrics) ObserveCheck(err error) {
	if m == nil {
		return
	}
	result := checkResultAllow
	if err != nil {
		result = checkResultDeny
	}
	m.checksTotal.WithLabelValues(result).Inc()
}

// ObserveInvalidation increments invalidations_received_total for one
// received cluster signal. nil receiver is a no-op.
func (m *RBACMetrics) ObserveInvalidation() {
	if m == nil {
		return
	}
	m.invalidationsTotal.Inc()
}
