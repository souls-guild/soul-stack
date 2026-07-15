package push

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// Metrics — Prometheus metrics for the push dispatcher (S7-3, ADR-032
// amendment 2026-05-26; extended by amendment 2026-05-27 P2 W-4 Multi-provider
// routing).
type Metrics struct {
	// HostCAUsed — counter of host-CA matches on SSH handshake, broken down
	// by `ca_name` (operator-defined name from `push.host_ca_refs[].name`, or
	// the `default` auto-name under singular auto-adapt). Cardinality-safe:
	// names are pinned in keeper.yml (a closed set of units), kebab-case
	// format is validated by the schema phase.
	HostCAUsed *prometheus.CounterVec
	// ProviderRouted — counter of per-SID routing decisions (P2 W-4): broken
	// down by {provider, decision_source}. decision_source ∈ {soul, coven,
	// cluster}. Cardinality-safe: ~N_providers × 3 decision_source values =
	// a handful of series. Incremented in pushorch.executeAsync after a
	// successful resolve via ProviderRouter (including the α-compat preset
	// path, where source = "soul" by per-job override semantics).
	ProviderRouted *prometheus.CounterVec
}

// RegisterMetrics creates the keeper_push_* collectors and registers them in
// [obs.Registry]. MustRegister: a duplicate registration is a programmer error.
func RegisterMetrics(reg *obs.Registry) *Metrics {
	m := &Metrics{
		HostCAUsed: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_push_host_ca_used_total",
				Help: "Совпадения host-CA на SSH-handshake (S7-3 multi-CA verify); разрез по имени CA из push.host_ca_refs[].name.",
			},
			[]string{"ca_name"},
		),
		ProviderRouted: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_push_provider_routed_total",
				Help: "Routing-decisions per-SID в Multi-provider push (P2 W-4); разрез по имени SshProvider-плагина и уровню резолва (soul/coven/cluster).",
			},
			[]string{"provider", "decision_source"},
		),
	}
	reg.Registerer().MustRegister(m.HostCAUsed, m.ProviderRouted)
	return m
}

// ObserveHostCAUsed increments the counter of CA matches for the given name.
// A nil receiver is a no-op (the dispatcher can run without observability in
// unit tests).
func (m *Metrics) ObserveHostCAUsed(caName string) {
	if m == nil {
		return
	}
	m.HostCAUsed.WithLabelValues(caName).Inc()
}

// ObserveProviderRouted increments the routing-decisions counter. A nil
// receiver is a no-op.
func (m *Metrics) ObserveProviderRouted(providerName, decisionSource string) {
	if m == nil {
		return
	}
	m.ProviderRouted.WithLabelValues(providerName, decisionSource).Inc()
}
