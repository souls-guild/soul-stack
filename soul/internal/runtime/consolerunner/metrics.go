package consolerunner

import (
	"github.com/prometheus/client_golang/prometheus"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// Metrics — soul_console_* collectors for the console runner. Registered through
// [Register] on the component-agnostic [obs.Registry], the same pattern as the
// Errand runner's metrics. Labels are closed enums (ADR-024 §2.2), so cardinality
// is bounded.
//
// A nil receiver makes every Observe* a no-op: the Runner comes up without an obs
// stack in push mode and in unit tests.
type Metrics struct {
	// sessionsActive — live pty sessions on this host. The leak signal: it must
	// return to zero after an operator disconnects.
	sessionsActive prometheus.Gauge

	// sessionsTotal — sessions that reached a terminal, by reason. Splits an
	// orderly `exit` from a killed session, an open failure and a limit
	// rejection.
	sessionsTotal *prometheus.CounterVec

	// outputBytes — pty output actually forwarded to Keeper.
	outputBytes prometheus.Counter

	// droppedBytes — pty output discarded by flow control. Non-zero means a
	// console out-produced its budget; the operator sees a gap marker.
	droppedBytes prometheus.Counter
}

// Reason label values for soul_console_sessions_total.
const (
	labelReasonExited   = "process_exited"
	labelReasonClosed   = "closed_by_keeper"
	labelReasonOpenFail = "open_failed"
	labelReasonShutdown = "soul_shutdown"
	labelReasonLimit    = "limit_exceeded"
	labelReasonUnknown  = "unknown"
)

// Register creates the soul_console_* collectors and registers them in
// [obs.Registry]. MustRegister: a duplicate registration is a wiring bug.
func Register(reg *obs.Registry) *Metrics {
	m := &Metrics{
		sessionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "soul_console_sessions_active",
			Help: "Live interactive console (PTY) sessions on this host.",
		}),
		sessionsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "soul_console_sessions_total",
				Help: "Console sessions that reached a terminal, sliced by reason.",
			},
			[]string{"reason"},
		),
		outputBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_console_output_bytes_total",
			Help: "Console output bytes forwarded to Keeper.",
		}),
		droppedBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_console_dropped_bytes_total",
			Help: "Console output bytes discarded by Soul-side flow control.",
		}),
	}
	reg.Registerer().MustRegister(m.sessionsActive, m.sessionsTotal, m.outputBytes, m.droppedBytes)
	return m
}

// ObserveSessionStart records a session that came up, with the resulting live
// count.
func (m *Metrics) ObserveSessionStart(active int) {
	if m == nil {
		return
	}
	m.sessionsActive.Set(float64(active))
}

// ObserveSessionActive republishes the live-session gauge after a session ends.
func (m *Metrics) ObserveSessionActive(active int) {
	if m == nil {
		return
	}
	m.sessionsActive.Set(float64(active))
}

// ObserveSessionEnd counts a terminal session by reason.
func (m *Metrics) ObserveSessionEnd(reason keeperv1.ConsoleExitReason) {
	if m == nil {
		return
	}
	m.sessionsTotal.WithLabelValues(reasonLabel(reason)).Inc()
}

// ObserveOutput records forwarded and dropped console bytes.
func (m *Metrics) ObserveOutput(sent int, dropped uint64) {
	if m == nil {
		return
	}
	if sent > 0 {
		m.outputBytes.Add(float64(sent))
	}
	if dropped > 0 {
		m.droppedBytes.Add(float64(dropped))
	}
}

// reasonLabel — closed mapping ConsoleExitReason → label value.
func reasonLabel(r keeperv1.ConsoleExitReason) string {
	switch r {
	case keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED:
		return labelReasonExited
	case keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_CLOSED_BY_KEEPER:
		return labelReasonClosed
	case keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_OPEN_FAILED:
		return labelReasonOpenFail
	case keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN:
		return labelReasonShutdown
	case keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_LIMIT_EXCEEDED:
		return labelReasonLimit
	default:
		return labelReasonUnknown
	}
}
