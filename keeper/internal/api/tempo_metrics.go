package api

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// TempoMetrics — keeper_tempo_*-counters Tempo per-AID rate-limiter-а
// (ADR-050(g)). Реализует [apimiddleware.RateLimitMetrics]: middleware зовёт
// IncTempoAllowed / IncTempoRejected на каждом разрешённом / отклонённом запросе.
//
// Лейбл `endpoint` = логическое bucket-имя (`voyage_create`); AID-лейбла НЕТ —
// число операторов не ограничено, AID в лейбле взорвал бы кардинальность
// time-series (ADR-050(g), отвергнутая альтернатива (в)). Кто превышает — видно
// в audit/логах по claims.Subject.
//
// nil-safe: методы проверяют nil-получатель, чтобы unit-тесты middleware
// поднимались без obs.Registry (паттерн toll.Metrics / watchmanMetrics).
type TempoMetrics struct {
	// allowedTotal — counter пропущенных запросов (токен взят).
	allowedTotal *prometheus.CounterVec

	// rejectedTotal — counter отклонённых запросов (бакет пуст → 429).
	rejectedTotal *prometheus.CounterVec
}

// RegisterTempoMetrics создаёт keeper_tempo_*-collectors и регистрирует их в
// [obs.Registry]. MustRegister — дубликат-регистрация programmer error (паттерн
// toll.RegisterMetrics). Регистрируется безусловно (registry всегда поднят);
// при выключенном Tempo (нет Redis / enabled=false) counters остаются на 0 —
// валидный сигнал «лимитер не активен».
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

// IncTempoAllowed — +1 к keeper_tempo_allowed_total{endpoint}.
func (m *TempoMetrics) IncTempoAllowed(endpoint string) {
	if m == nil {
		return
	}
	m.allowedTotal.WithLabelValues(endpoint).Inc()
}

// IncTempoRejected — +1 к keeper_tempo_rejected_total{endpoint}.
func (m *TempoMetrics) IncTempoRejected(endpoint string) {
	if m == nil {
		return
	}
	m.rejectedTotal.WithLabelValues(endpoint).Inc()
}
