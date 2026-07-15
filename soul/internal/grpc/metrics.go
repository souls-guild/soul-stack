package grpc

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// EventStreamMetrics — Prometheus collectors for the Soul-side EventStream
// client (Keeper↔Soul gRPC per ADR-002/ADR-012). Registered by a helper on
// top of the component-agnostic [obs.Registry] — the same pattern as the
// [keeper/internal/grpc.RegisterGRPCMetrics] pilot (docs/observability.md §4.0).
//
// Metrics live here (soul/internal/grpc), not in shared/obs: this is Soul
// agent connection state, not reused by the Keeper (ADR-011: shared/ is
// cross-cutting code). Per ADR-011, soul does NOT import keeper;
// instrumentation goes through the neutral shared/obs.
//
// Names — ADR-024 §2.1: soul_ prefix, snake_case, gauge for instantaneous
// state without _total, counter with _total. No labels: the Soul agent's
// connection state is one-dimensional (one Keeper stream at a time); breakdown
// by KID/session belongs in trace/log.
type EventStreamMetrics struct {
	// connected — 1 when the EventStream session is established (handshake
	// done), 0 on disconnect/reconnect. Instantaneous-state gauge: "does Soul
	// have a live channel to the Keeper right now".
	connected prometheus.Gauge

	// reconnects — counter of reconnect attempts (each Dial in the
	// reconnect loop after the first connection). Growth signals an unstable
	// channel or an unreachable Keeper cluster.
	reconnects prometheus.Counter
}

// RegisterEventStreamMetrics creates the soul_eventstream_* collectors and
// registers them in [obs.Registry]. Returns a handle for wiring into the
// cmd/soul reconnect loop.
//
// MustRegister: a duplicate registration is a programmer error; failing fast
// is more convenient than lazy init (pattern identical to the
// RegisterGRPCMetrics pilot).
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

// SetConnected sets the connected gauge (true → 1, false → 0).
// nil receiver — no-op: the reconnect loop may run without the obs stack
// (unit tests, metrics.enabled=false).
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

// IncReconnects increments the reconnect-attempt counter. nil receiver — no-op.
func (m *EventStreamMetrics) IncReconnects() {
	if m == nil {
		return
	}
	m.reconnects.Inc()
}
