package ldapsync

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// Metrics are the `keeper_ldapsync_*` collectors (docs/observability.md). All
// label values are closed enums of this package. A nil *Metrics is a no-op.
type Metrics struct {
	// Runs — passes by [Result]; idle ticks are not counted.
	Runs *prometheus.CounterVec
	// Operators — attempts by [Outcome], one per operator per pass.
	Operators *prometheus.CounterVec
	// RoleChanges — roles granted or revoked, by [Reason].
	RoleChanges *prometheus.CounterVec
	// LockoutRefusals — reconciliations refused because they would have
	// emptied the cluster's admin set (NIM-320).
	LockoutRefusals prometheus.Counter
	// LastSuccess — unix time of the last `ok` pass: the directory answered for
	// every operator and every answer was applied. Only the leader sets it.
	LastSuccess prometheus.Gauge
	// LeaseHeld — 1 while this instance leads the sync.
	LeaseHeld prometheus.Gauge
}

// RegisterMetrics creates and registers the collectors.
func RegisterMetrics(r *obs.Registry) *Metrics {
	m := &Metrics{
		Runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_ldapsync_runs_total",
			Help: "LDAP group sync passes by result (ok, partial, unreachable, guarded, error).",
		}, []string{"result"}),
		Operators: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_ldapsync_operators_total",
			Help: "LDAP group sync attempts by per-operator outcome.",
		}, []string{"outcome"}),
		RoleChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_ldapsync_role_changes_total",
			Help: "Roles granted or revoked by the LDAP group sync, by reason.",
		}, []string{"change", "reason"}),
		LockoutRefusals: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_ldapsync_lockout_refusals_total",
			Help: "LDAP group sync reconciliations refused because they would have emptied the cluster admin set.",
		}),
		LastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_ldapsync_last_success_timestamp_seconds",
			Help: "Unix time of the last LDAP group sync pass that read and applied every operator.",
		}),
		LeaseHeld: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_ldapsync_lease_held",
			Help: "1 if this Keeper instance holds the LDAP group sync leadership Redis lease, 0 otherwise.",
		}),
	}
	r.Registerer().MustRegister(m.Runs, m.Operators, m.RoleChanges, m.LockoutRefusals, m.LastSuccess, m.LeaseHeld)
	return m
}

// SetLeaseHeld is the leaderloop OnLeaseChange hook.
func (m *Metrics) SetLeaseHeld(held bool) {
	if m == nil {
		return
	}
	if held {
		m.LeaseHeld.Set(1)
		return
	}
	m.LeaseHeld.Set(0)
}

func (m *Metrics) run(r Result) {
	if m == nil || r == ResultIdle {
		return
	}
	m.Runs.WithLabelValues(string(r)).Inc()
	if r == ResultOK {
		m.LastSuccess.Set(float64(time.Now().Unix()))
	}
}

func (m *Metrics) outcome(o Outcome) {
	if m == nil {
		return
	}
	m.Operators.WithLabelValues(string(o)).Inc()
}

func (m *Metrics) roleChanges(c auth.RoleChange, reason Reason) {
	if m == nil {
		return
	}
	if n := len(c.Granted); n > 0 {
		m.RoleChanges.WithLabelValues("granted", string(reason)).Add(float64(n))
	}
	if n := len(c.Revoked); n > 0 {
		m.RoleChanges.WithLabelValues("revoked", string(reason)).Add(float64(n))
	}
}

func (m *Metrics) lockoutRefused() {
	if m == nil {
		return
	}
	m.LockoutRefusals.Inc()
}
