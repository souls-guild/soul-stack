package herald

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// DispatcherMetrics — Prometheus-collector-ы notification-dispatcher-а и tap-а
// (ADR-052(c)). Регистрируется поверх компонент-агностичного [obs.Registry]
// тем же паттерном, что [scenario.RegisterScenarioMetrics] (ADR-024 §4.0).
//
// Наблюдаемость S2-точки: сколько событий обработано/сматчено, сколько
// дропнуто из-за полного буфера tap-а (сигнал «consumer не успевает»),
// сколько ошибок матча/постановки.
type DispatcherMetrics struct {
	// keeper_herald_tap_dropped_total — событий дропнуто tap-ом из-за полного
	// буфера (drop-counter, ADR-052(c) «неблокирующая передача в bounded-
	// буфер»). Рост → consumer/PG-кэш правил не успевает за write-path-ом.
	tapDropped prometheus.Counter
	// keeper_herald_dispatch_total — событий обработано dispatcher-ом,
	// разрезано по факту матча (matched/unmatched). Без cardinality по
	// event_type — держим низкую размерность.
	dispatchTotal *prometheus.CounterVec
	// keeper_herald_matches_total — сматченных (event×Tiding) пар суммарно
	// (одно событие может сматчить несколько правил → несколько jobs).
	matchesTotal prometheus.Counter
	// keeper_herald_dispatch_errors_total — ошибок загрузки правил / постановки
	// job-а в очередь (best-effort, не валят write-path).
	errorsTotal prometheus.Counter
}

// RegisterDispatcherMetrics создаёт herald-dispatcher-collector-ы и
// регистрирует их в [obs.Registry]. MustRegister: дубликат-регистрация —
// programmer error (паттерн RegisterScenarioMetrics).
func RegisterDispatcherMetrics(reg *obs.Registry) *DispatcherMetrics {
	m := &DispatcherMetrics{
		tapDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_herald_tap_dropped_total",
			Help: "Количество audit-событий, дропнутых notification-tap-ом из-за полного буфера (consumer не успевает).",
		}),
		dispatchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_dispatch_total",
			Help: "Количество событий, обработанных notification-dispatcher-ом, разрезанное по факту матча (matched/unmatched).",
		}, []string{"result"}),
		matchesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_herald_matches_total",
			Help: "Количество сматченных пар (событие × Tiding-правило); каждая порождает задание на доставку.",
		}),
		errorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_herald_dispatch_errors_total",
			Help: "Количество ошибок загрузки Tiding-правил / постановки задания в очередь доставки (best-effort).",
		}),
	}
	reg.Registerer().MustRegister(m.tapDropped, m.dispatchTotal, m.matchesTotal, m.errorsTotal)
	return m
}

// observeDrop инкрементирует drop-счётчик. nil-receiver → no-op (tap может
// подниматься без observability — unit-тесты).
func (m *DispatcherMetrics) observeDrop() {
	if m == nil {
		return
	}
	m.tapDropped.Inc()
}

// observeDispatch фиксирует обработку одного события: matched-bucket при
// matched>0, иначе unmatched; плюс число сматченных правил в matchesTotal.
func (m *DispatcherMetrics) observeDispatch(matched int) {
	if m == nil {
		return
	}
	if matched > 0 {
		m.dispatchTotal.WithLabelValues("matched").Inc()
		m.matchesTotal.Add(float64(matched))
	} else {
		m.dispatchTotal.WithLabelValues("unmatched").Inc()
	}
}

// observeError инкрементирует счётчик ошибок dispatch-а.
func (m *DispatcherMetrics) observeError() {
	if m == nil {
		return
	}
	m.errorsTotal.Inc()
}

// DeliveryMetrics — Prometheus-collector-ы claim-queue worker-а доставки
// (ADR-052(d), S3). Наблюдаемость: сколько попыток/успехов/провалов/retry.
//
// Кардинальность: лейбл `herald` — имя канала из реестра `heralds`. Реестр
// оператор-управляемый и мал (единицы–десятки каналов), это НЕ unbounded
// user-input (в отличие от `event_type`, которого нет в лейблах). 4 серии ×
// число каналов — контролируемо. Если число каналов когда-нибудь станет
// большим, лейбл можно будет снять без смены имён метрик.
type DeliveryMetrics struct {
	// keeper_herald_delivery_attempts_total — попыток доставки (каждый claim+POST).
	attempts *prometheus.CounterVec
	// keeper_herald_delivery_succeeded_total — успешных терминалов (2xx).
	succeeded *prometheus.CounterVec
	// keeper_herald_delivery_failed_total — терминальных провалов (исчерпан retry
	// / no-retry-ошибка).
	failed *prometheus.CounterVec
	// keeper_herald_delivery_retries_total — перепостановок на повтор (backoff).
	retries *prometheus.CounterVec
}

// RegisterDeliveryMetrics создаёт delivery-collector-ы и регистрирует их в
// [obs.Registry]. MustRegister: дубликат — programmer error (паттерн
// RegisterDispatcherMetrics).
func RegisterDeliveryMetrics(reg *obs.Registry) *DeliveryMetrics {
	m := &DeliveryMetrics{
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_delivery_attempts_total",
			Help: "Количество попыток webhook-доставки уведомления (claim + POST), по herald-каналу.",
		}, []string{"herald"}),
		succeeded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_delivery_succeeded_total",
			Help: "Количество успешных доставок уведомления (2xx-терминал), по herald-каналу.",
		}, []string{"herald"}),
		failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_delivery_failed_total",
			Help: "Количество терминальных провалов доставки (исчерпан retry / no-retry-ошибка), по herald-каналу.",
		}, []string{"herald"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_delivery_retries_total",
			Help: "Количество перепостановок уведомления на повтор (retry с backoff), по herald-каналу.",
		}, []string{"herald"}),
	}
	reg.Registerer().MustRegister(m.attempts, m.succeeded, m.failed, m.retries)
	return m
}

func (m *DeliveryMetrics) observeAttempt(herald string) {
	if m == nil {
		return
	}
	m.attempts.WithLabelValues(herald).Inc()
}

func (m *DeliveryMetrics) observeSucceeded(herald string) {
	if m == nil {
		return
	}
	m.succeeded.WithLabelValues(herald).Inc()
}

func (m *DeliveryMetrics) observeFailed(herald string) {
	if m == nil {
		return
	}
	m.failed.WithLabelValues(herald).Inc()
}

func (m *DeliveryMetrics) observeRetry(herald string) {
	if m == nil {
		return
	}
	m.retries.WithLabelValues(herald).Inc()
}
