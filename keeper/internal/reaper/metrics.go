package reaper

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// ReaperMetrics — collection of Prometheus collectors for the Reaper
// (docs/keeper/reaper.md → section "Metrics per rule").
//
// Registered separately from core HTTP metrics via [RegisterReaperMetrics],
// because Reaper is an optional subsystem (`reaper.enabled: false`
// is valid; in that case metrics are not needed and not published). Wire-up
// is done by `keeper/cmd/keeper/main.go::runDaemon` strictly in the same
// branch where [Runner] is started.
//
// Collector lives alongside the subsystem (docs/observability.md §4.0): metrics
// `keeper_reaper_*` are keeper-specific, pull from per-rule semantics of Reaper —
// their place is in `keeper/internal/reaper`, not in `shared/obs`.
//
// Labels:
//   - `rule` — canonical rule name from docs/keeper/reaper.md. Closed enum,
//     cardinality-safe: values = exactly the set of rules dispatched via
//     runDurationRule / runStatusesRule / runArchiveStateHistory in
//     runner.go::dispatch (enum closed by runner-dispatch, not by this list —
//     docstring is synchronized manually). Full set:
//     `purge_audit_old`, `expire_pending_seeds`, `purge_used_tokens`,
//     `purge_souls`, `purge_old_seeds`, `mark_disconnected`,
//     `purge_apply_runs`, `purge_voyages`, `purge_push_runs`,
//     `purge_incarnation_archive`, `purge_state_history_archive`,
//     `purge_archived_state_history`, `purge_apply_task_register`,
//     `reclaim_apply_runs`, `archive_state_history`,
//     `purge_orphan_push_runs`, `reap_orphan_vault_keys`, `purge_old_errands`,
//     `purge_orphan_ephemeral_tidings`, `reclaim_voyages`,
//     `reconcile_orphan_applying`.
//
// `LeaseHeld` — without labels: lease is an exclusive resource cluster-wide,
// each Keeper instance publishes its own value (0/1). Scraper
// sees who is the leader from the `instance` label of the Prometheus target.
// Per-`kid` label is redundant — it duplicates `instance`.
type ReaperMetrics struct {
	// RuleExecutions — counter of rule executions (incremented on each
	// dispatch, regardless of whether rows were found for deletion).
	// Comparison with `RulePurgedTotal` gives the rule's "effectiveness":
	// many executions with zero purged — candidate for increasing max_age.
	RuleExecutions *prometheus.CounterVec

	// RulePurgedTotal — total rows affected by the rule. `set_status` rules
	// (`mark_disconnected`) also increment this — these are affected rows,
	// not strictly "deleted".
	RulePurgedTotal *prometheus.CounterVec

	// RuleDuration — duration of the rule's SQL function call in seconds.
	// Buckets are tuned to Reaper-prod profile: typical batch is 10-100ms
	// (DELETE FROM ... WHERE ... LIMIT 1000), long audit-purge with big
	// retention takes seconds. Default Prometheus buckets (0.005..10) almost work,
	// but we shift the upper bound to 30s — long-purge on cold DB
	// can take 5-15s, cutting it at `+Inf` masks regression.
	RuleDuration *prometheus.HistogramVec

	// DispatchErrors — counter of dispatch-loop errors (Purger returned error).
	// Separate from `RuleExecutions` so it can alert independently without histogram knowledge.
	DispatchErrors *prometheus.CounterVec

	// LeaseHeld — 1 if this Keeper instance holds the Redis leadership lease,
	// 0 otherwise. Gauge without labels: cluster-wide invariant —
	// `sum(keeper_reaper_lease_held) == 1` (with exactly one leader),
	// imbalance is visible to the operator without alerting.
	LeaseHeld prometheus.Gauge
}

// RegisterReaperMetrics creates collectors and registers them in the Registry.
// Returns a descriptor for wire-up via [Deps].
//
// MustRegister: duplicate registration is a programmer error (called twice);
// failing immediately is better than carrying lazy initialization.
func RegisterReaperMetrics(r *obs.Registry) *ReaperMetrics {
	m := &ReaperMetrics{
		RuleExecutions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_reaper_rule_executions_total",
				Help: "Number of Reaper rule executions, split by rule.",
			},
			[]string{"rule"},
		),
		RulePurgedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_reaper_rule_purged_total",
				Help: "Affected rows from Reaper rules (delete or set_status), by rule.",
			},
			[]string{"rule"},
		),
		RuleDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "keeper_reaper_rule_duration_seconds",
				Help:    "Duration of Reaper rule SQL function call in seconds, by rule.",
				Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
			},
			[]string{"rule"},
		),
		DispatchErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_reaper_dispatch_errors_total",
				Help: "Number of Reaper dispatch loop errors (Purger returned error), by rule.",
			},
			[]string{"rule"},
		),
		LeaseHeld: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_reaper_lease_held",
			Help: "1 if this Keeper instance holds the Reaper leadership Redis lease, 0 otherwise.",
		}),
	}
	r.Registerer().MustRegister(m.RuleExecutions, m.RulePurgedTotal, m.RuleDuration, m.DispatchErrors, m.LeaseHeld)
	return m
}

// ObserveRule — single helper for recording all per-rule metrics of one
// dispatch iteration. Usage on the Reaper side:
//
//	start := time.Now()
//	affected, err := call(ctx, ...)
//	m.ObserveRule(rule, affected, err, time.Since(start))
//
// Metrics on nil receiver is no-op (Reaper can be initialized without
// observability — tests, ranched mode). This allows the caller to not
// check for nil on each iteration.
func (m *ReaperMetrics) ObserveRule(rule string, affected int64, err error, dur time.Duration) {
	if m == nil {
		return
	}
	m.RuleExecutions.WithLabelValues(rule).Inc()
	m.RuleDuration.WithLabelValues(rule).Observe(dur.Seconds())
	if err != nil {
		m.DispatchErrors.WithLabelValues(rule).Inc()
		return
	}
	if affected > 0 {
		m.RulePurgedTotal.WithLabelValues(rule).Add(float64(affected))
	}
}

// SetLeaseHeld — sugar for nil-safe Gauge write.
func (m *ReaperMetrics) SetLeaseHeld(held bool) {
	if m == nil {
		return
	}
	if held {
		m.LeaseHeld.Set(1)
		return
	}
	m.LeaseHeld.Set(0)
}
