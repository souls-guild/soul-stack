package shellgate

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// Gate decision outcomes for keeper_rbac_shell_errand_gate_total. Closed
// 4-value enum.
//
// `would_deny` is the whole point of the deprecation window: it is the count an
// operator watches to learn whether flipping to `enforce` will break anything,
// BEFORE it does. A cluster where it stays at zero for a release is a cluster
// the flip cannot break.
const (
	// resultAllow — the operator holds `soul.console` for this target; the gate
	// changed nothing.
	resultAllow = "allow"
	// resultWouldDeny — window mode, `soul.console` missing: allowed now,
	// denied once the window closes.
	resultWouldDeny = "would_deny"
	// resultDeny — enforce mode, `soul.console` missing: refused.
	resultDeny = "deny"
	// resultUnconfigured — the choke-point supplied no console probe; nothing
	// was verified. A wiring bug, surfaced rather than defaulted.
	resultUnconfigured = "unconfigured"
)

// Metrics is the collector set for the console gate over the Errand path
// (NIM-197). Registered on the component-agnostic [obs.Registry], the same
// pattern as [rbac.RegisterRBACMetrics].
//
// Labels carry no aid, role or command line — only the closed `surface` and
// `result` enums (ADR-024 §2.2). Who ran what stays in the audit trail; this
// answers "how much traffic will the flip to enforce break, and on which
// entry point".
// The static side of the inventory — the count of roles that will break —
// derives from the RBAC snapshot, not from traffic, so it lives with the other
// snapshot gauges as `keeper_rbac_shell_errand_legacy_roles`
// ([rbac.RBACMetrics.ObserveShellErrandInventory]).
type Metrics struct {
	decisionsTotal *prometheus.CounterVec
}

// RegisterMetrics creates and registers keeper_rbac_shell_errand_gate_total.
// MustRegister: a duplicate registration is a programmer error.
func RegisterMetrics(reg *obs.Registry) *Metrics {
	m := &Metrics{
		decisionsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_rbac_shell_errand_gate_total",
				Help: "Console-gate decisions on verb-shell Errands, split by surface (rest/mcp/voyage/cadence/cadence_spawn) and result (allow/would_deny/deny/unconfigured).",
			},
			[]string{"surface", "result"},
		),
	}
	reg.Registerer().MustRegister(m.decisionsTotal)
	return m
}

// Observe records one gate decision. nil receiver is a no-op (a handler built
// without observability, as in unit tests).
func (m *Metrics) Observe(surface, result string) {
	if m == nil {
		return
	}
	m.decisionsTotal.WithLabelValues(surface, result).Inc()
}
