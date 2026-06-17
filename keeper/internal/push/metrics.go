package push

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// Metrics — Prometheus-метрики push-диспетчера (S7-3, ADR-032 amendment
// 2026-05-26; расширено amendment 2026-05-27 P2 W-4 Multi-provider routing).
type Metrics struct {
	// HostCAUsed — счётчик матчей host-CA на SSH-handshake, разрез по `ca_name`
	// (operator-defined имя из `push.host_ca_refs[].name` либо auto-name
	// `default` при auto-adapt singular). Cardinality-safe: имена закрепляются
	// в keeper.yml (closed-set единиц), kebab-case-формат валидируется
	// schema-фазой.
	HostCAUsed *prometheus.CounterVec
	// ProviderRouted — счётчик routing-decisions per-SID (P2 W-4): разрез
	// {provider, decision_source}. decision_source ∈ {soul, coven, cluster}.
	// Cardinality-safe: ~N_providers × 3 значения decision_source = единицы
	// серий. Инкрементируется в pushorch.executeAsync после успешного
	// resolve через ProviderRouter (включая α-compat preset-путь, где source
	// = «soul» по семантике per-job override).
	ProviderRouted *prometheus.CounterVec
}

// RegisterMetrics создаёт keeper_push_*-collectors и регистрирует их в
// [obs.Registry]. MustRegister: дубликат-регистрация — programmer error.
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

// ObserveHostCAUsed инкрементирует счётчик матчей CA с заданным именем. nil-
// получатель — no-op (dispatcher может работать без observability в unit-тестах).
func (m *Metrics) ObserveHostCAUsed(caName string) {
	if m == nil {
		return
	}
	m.HostCAUsed.WithLabelValues(caName).Inc()
}

// ObserveProviderRouted инкрементирует счётчик routing-decisions. nil-
// получатель — no-op.
func (m *Metrics) ObserveProviderRouted(providerName, decisionSource string) {
	if m == nil {
		return
	}
	m.ProviderRouted.WithLabelValues(providerName, decisionSource).Inc()
}
