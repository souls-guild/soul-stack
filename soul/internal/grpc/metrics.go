package grpc

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// EventStreamMetrics — набор Prometheus-collector-ов Soul-side EventStream-
// клиента (Keeper↔Soul gRPC по ADR-002/ADR-012). Регистрируется helper-ом
// поверх компонент-агностичного [obs.Registry] — тем же паттерном, что пилот
// [keeper/internal/grpc.RegisterGRPCMetrics] (docs/observability.md §4.0).
//
// Метрики живут здесь (soul/internal/grpc), а не в shared/obs: это connection-
// state Soul-агента, не переиспользуется Keeper-ом (ADR-011: shared/ — поперечный
// код). По ADR-011 soul НЕ импортирует keeper; инструментация — через
// нейтральный shared/obs.
//
// Имена — ADR-024 §2.1: префикс soul_, snake_case, gauge мгновенного состояния
// без _total, counter с _total. Labels — нет: connection-state Soul-агента
// одномерен (один Keeper-стрим за раз), разрез по KID/session — в trace/log.
type EventStreamMetrics struct {
	// connected — 1, когда EventStream-сессия установлена (handshake завершён),
	// 0 — при разрыве/реконнекте. Gauge мгновенного состояния: «есть ли у Soul
	// живой канал к Keeper-у прямо сейчас».
	connected prometheus.Gauge

	// reconnects — счётчик попыток реконнекта (каждый Dial reconnect-loop-а
	// после первого подключения). Рост — сигнал нестабильного канала /
	// недоступности Keeper-кластера.
	reconnects prometheus.Counter
}

// RegisterEventStreamMetrics создаёт soul_eventstream_*-collectors и
// регистрирует их в [obs.Registry]. Возвращает дескриптор для wire-up в
// reconnect-loop cmd/soul.
//
// MustRegister: дубликат-регистрация — programmer error; падать сразу удобнее
// ленивой инициализации (паттерн идентичен пилоту RegisterGRPCMetrics).
func RegisterEventStreamMetrics(reg *obs.Registry) *EventStreamMetrics {
	m := &EventStreamMetrics{
		connected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "soul_eventstream_connected",
			Help: "1, когда EventStream-сессия Soul↔Keeper установлена; 0 — при разрыве/реконнекте.",
		}),
		reconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_eventstream_reconnects_total",
			Help: "Количество попыток реконнекта EventStream-клиента к Keeper-у.",
		}),
	}
	reg.Registerer().MustRegister(m.connected, m.reconnects)
	return m
}

// SetConnected проставляет gauge connected (true → 1, false → 0).
// nil-получатель — no-op: reconnect-loop может подниматься без obs-стека
// (unit-тесты, metrics.enabled=false).
func (m *EventStreamMetrics) SetConnected(connected bool) {
	if m == nil {
		return
	}
	if connected {
		m.connected.Set(1)
		return
	}
	m.connected.Set(0)
}

// IncReconnects инкрементирует счётчик попыток реконнекта. nil-получатель — no-op.
func (m *EventStreamMetrics) IncReconnects() {
	if m == nil {
		return
	}
	m.reconnects.Inc()
}
