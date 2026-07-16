package toll

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// Metrics — keeper_toll_*-collectors + cluster:degraded gauge (ADR-038).
//
// Separate descriptors by Watcher and Leader are not created (like in watchman):
// Watcher and Leader live in one process, shared Registry, single object simplifies
// wire-up in daemon.
//
// nil-safe: all methods check nil receiver so package unit-tests
// can run without obs.Registry wiring (watchmanMetrics pattern).
type Metrics struct {
	// clusterDegraded — cluster-level gauge (set ONLY by leader, ADR-038
	// invariant exclusive setter). Remains 0 on non-leader instances; on
	// leader the leader-loop updates gauge on set/clear events.
	clusterDegraded prometheus.Gauge

	// disconnectsTotal — counter of non-graceful EventStream disconnects
	// (post-filter warmup + graceful-shutdown). Per-coven cardinality is safe
	// for counter — it is not a fanout flag but an observation rate source.
	disconnectsTotal *prometheus.CounterVec

	// warmupSkipped — counter of disconnects rejected by warmup-immunity
	// (first WarmupDelay after instance start). Cheap, helps operator
	// distinguish cold-start wave from actual churn.
	warmupSkipped prometheus.Counter

	// gracefulSkipped — counter of disconnects rejected as graceful-shutdown
	// (planned stream closure by the instance itself). Signal «rolling restart»
	// vs «actual churn».
	gracefulSkipped prometheus.Counter

	// leaderActive — gauge «this instance holds Toll-lease». 1 = leader, 0 =
	// follower. Sum across all instances exactly 1 in healthy cluster
	// (Redis-lease guarantees exclusive).
	leaderActive prometheus.Gauge
}

// RegisterMetrics creates keeper_toll_*-collectors + keeper_cluster_degraded
// gauge and registers them in [obs.Registry]. MustRegister — duplicate registration
// is a programmer error (pattern RegisterGRPCMetrics / registerWatchmanMetrics).
func RegisterMetrics(reg *obs.Registry) *Metrics {
	m := &Metrics{
		clusterDegraded: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_cluster_degraded",
			Help: "1 — Toll-leader raised cluster:degraded flag (disconnect rate > threshold in window); 0 — normal. Set ONLY by leader (ADR-038).",
		}),
		disconnectsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_toll_disconnects_total",
				Help: "Non-graceful EventStream disconnects observed by tollwatcher (post-filter graceful-shutdown / warmup-immunity). Per-coven cardinality safe for counter (ADR-038).",
			},
			[]string{"coven"},
		),
		warmupSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_toll_warmup_skipped_total",
			Help: "Disconnect events rejected by warmup-immunity (first WarmupDelay after instance start).",
		}),
		gracefulSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_toll_graceful_skipped_total",
			Help: "Disconnect events rejected as graceful-shutdown of this instance (rolling restart vs actual churn).",
		}),
		leaderActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_toll_leader_active",
			Help: "1 — this keeper instance holds lease cluster:toll:leader (Toll aggregator); 0 — follower. Sum across cluster exactly 1 with healthy Redis-lease.",
		}),
	}
	reg.Registerer().MustRegister(
		m.clusterDegraded,
		m.disconnectsTotal,
		m.warmupSkipped,
		m.gracefulSkipped,
		m.leaderActive,
	)
	return m
}

// SetClusterDegraded — set 0/1 for keeper_cluster_degraded. Called
// ONLY by leader (invariant ADR-038).
func (m *Metrics) SetClusterDegraded(degraded bool) {
	if m == nil {
		return
	}
	if degraded {
		m.clusterDegraded.Set(1)
		return
	}
	m.clusterDegraded.Set(0)
}

// IncDisconnect — +1 to per-coven counter. Empty coven stored as label
// "" — allowed (cardinality stable, see field doc).
func (m *Metrics) IncDisconnect(coven string) {
	if m == nil {
		return
	}
	m.disconnectsTotal.WithLabelValues(coven).Inc()
}

// IncWarmupSkipped — +1 when disconnect rejected by warmup-immunity.
func (m *Metrics) IncWarmupSkipped() {
	if m == nil {
		return
	}
	m.warmupSkipped.Inc()
}

// IncGracefulSkipped — +1 when disconnect rejected as graceful-shutdown.
func (m *Metrics) IncGracefulSkipped() {
	if m == nil {
		return
	}
	m.gracefulSkipped.Inc()
}

// SetLeaderActive — 0/1 keeper_toll_leader_active. Updated by leader-loop
// on lease acquire/release.
func (m *Metrics) SetLeaderActive(active bool) {
	if m == nil {
		return
	}
	if active {
		m.leaderActive.Set(1)
		return
	}
	m.leaderActive.Set(0)
}
