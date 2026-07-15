package api

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// TempoMetrics — keeper_tempo_* counters of the Tempo per-AID rate-limiter
// (ADR-050(g)). Implements [apimiddleware.RateLimitMetrics]: the middleware calls
// IncTempoAllowed / IncTempoRejected on every allowed / rejected request.
//
// Label `endpoint` = the logical bucket name (`voyage_create`); NO AID label —
// the number of operators is unbounded, and an AID in the label would blow up the
// time-series cardinality (ADR-050(g), rejected alternative (c)). Who exceeds the limit is visible
// in audit/logs by claims.Subject.
//
// nil-safe: the methods check for a nil receiver, so middleware unit tests
// come up without obs.Registry (toll.Metrics / watchmanMetrics pattern).
type TempoMetrics struct {
	// allowedTotal — counter of passed requests (a token was taken).
	allowedTotal *prometheus.CounterVec

	// rejectedTotal — counter of rejected requests (bucket empty → 429).
	rejectedTotal *prometheus.CounterVec
}

// RegisterTempoMetrics creates the keeper_tempo_* collectors and registers them in
// [obs.Registry]. MustRegister — a duplicate registration is a programmer error (toll.RegisterMetrics
// pattern). Registered unconditionally (the registry is always up);
// with Tempo disabled (no Redis / enabled=false) the counters stay at 0 —
// a valid "limiter not active" signal.
func RegisterTempoMetrics(reg *obs.Registry) *TempoMetrics {
	m := &TempoMetrics{
		allowedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_tempo_allowed_total",
				Help: "Запросы, пропущенные Tempo rate-limiter-ом (токен взят). Лейбл endpoint = bucket-имя (voyage_create); AID-лейбла НЕТ (кардинальность, ADR-050).",
			},
			[]string{"endpoint"},
		),
		rejectedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_tempo_rejected_total",
				Help: "Запросы, отклонённые Tempo rate-limiter-ом (бакет пуст → 429 + Retry-After). Лейбл endpoint = bucket-имя (voyage_create); AID-лейбла НЕТ (ADR-050).",
			},
			[]string{"endpoint"},
		),
	}
	reg.Registerer().MustRegister(m.allowedTotal, m.rejectedTotal)
	return m
}

// IncTempoAllowed — +1 to keeper_tempo_allowed_total{endpoint}.
func (m *TempoMetrics) IncTempoAllowed(endpoint string) {
	if m == nil {
		return
	}
	m.allowedTotal.WithLabelValues(endpoint).Inc()
}

// IncTempoRejected — +1 to keeper_tempo_rejected_total{endpoint}.
func (m *TempoMetrics) IncTempoRejected(endpoint string) {
	if m == nil {
		return
	}
	m.rejectedTotal.WithLabelValues(endpoint).Inc()
}
