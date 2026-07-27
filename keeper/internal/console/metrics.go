package console

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// Metrics — the keeper_console_* collectors (ADR-024). The Soul side exposes
// the mirror set (soul_console_*), so an operator can tell a Keeper-side drop
// from a host-side one — which is the whole diagnostic question when a terminal
// stutters.
//
// All methods are nil-safe no-ops, so unit tests and dev builds can leave
// Metrics unset (the pattern of [GRPCMetrics] in keeper/internal/grpc).
//
// Cardinality (ADR-024 §2.2): sid / aid / session_id are NEVER labels — they
// scale with host and operator count. The only label is the closed
// ConsoleExitReason enum.
type Metrics struct {
	sessionsActive prometheus.Gauge
	sessionsTotal  *prometheus.CounterVec
	outputBytes    prometheus.Counter
	droppedBytes   prometheus.Counter
	socketsActive  prometheus.Gauge
}

// RegisterMetrics creates the collectors and registers them on the shared
// registry. MustRegister: a duplicate registration is a programmer error.
func RegisterMetrics(r *obs.Registry) *Metrics {
	m := &Metrics{
		sessionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_console_sessions_active",
			Help: "Interactive console (PTY) sessions currently held by this Keeper instance.",
		}),
		sessionsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_console_sessions_total",
				Help: "Console sessions that reached a terminal state, by reason.",
			},
			[]string{"reason"},
		),
		outputBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_console_output_bytes_total",
			Help: "PTY output bytes received from Souls and forwarded to operator sockets.",
		}),
		droppedBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_console_dropped_bytes_total",
			Help: "PTY output bytes dropped by Keeper-side backpressure (a socket that stopped reading).",
		}),
		socketsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_console_sockets_active",
			Help: "Operator WebSocket connections currently open on /v1/console.",
		}),
	}
	r.Registerer().MustRegister(m.sessionsActive, m.sessionsTotal, m.outputBytes, m.droppedBytes, m.socketsActive)
	return m
}

// IncSessionsActive / DecSessionsActive track live sessions. The gauge is the
// leak signal: it must return to zero once every operator disconnects.
func (m *Metrics) IncSessionsActive() {
	if m != nil {
		m.sessionsActive.Inc()
	}
}

func (m *Metrics) DecSessionsActive() {
	if m != nil {
		m.sessionsActive.Dec()
	}
}

// IncSessionTerminal counts a session that reached its terminal state. An empty
// reason (a Keeper-side close with no ConsoleExit) is recorded as `unknown`
// rather than skipped — the totals must reconcile against the gauge.
func (m *Metrics) IncSessionTerminal(reason string) {
	if m == nil {
		return
	}
	if reason == "" {
		reason = "unknown"
	}
	m.sessionsTotal.WithLabelValues(reason).Inc()
}

func (m *Metrics) AddOutputBytes(n int) {
	if m != nil && n > 0 {
		m.outputBytes.Add(float64(n))
	}
}

// AddDroppedBytes counts output discarded by Keeper-side backpressure. A rising
// counter means an operator socket is not draining; combined with
// soul_console_dropped_bytes_total it says which side is the bottleneck.
func (m *Metrics) AddDroppedBytes(n int) {
	if m != nil && n > 0 {
		m.droppedBytes.Add(float64(n))
	}
}

func (m *Metrics) IncSocketsActive() {
	if m != nil {
		m.socketsActive.Inc()
	}
}

func (m *Metrics) DecSocketsActive() {
	if m != nil {
		m.socketsActive.Dec()
	}
}
