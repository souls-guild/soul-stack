package grpc

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// GRPCMetrics — the set of Prometheus collectors for the Keeper's
// EventStream subsystem (Keeper↔Soul gRPC per ADR-012). Registered by a
// dedicated helper on top of the component-agnostic [obs.Registry] — the
// same pattern as [obs.RegisterHTTPMetrics] / [obs.RegisterReaperMetrics]:
// the registry core knows nothing about specific metrics, and
// keeper_grpc_*-metrics are a detail of the Keeper's gRPC facade.
//
// The metrics live here (keeper/internal/grpc), not in shared/obs, because
// unlike the HTTP middleware and Reaper collectors they're tied to
// keeper-internal EventStream types and aren't reused by the Soul
// (ADR-011: shared/ is genuinely cross-cutting code).
//
// Names follow Prometheus convention (snake_case, _total for counters,
// _active for an instantaneous-state gauge; ADR-024 §2.1). Labels are
// closed enums, cardinality-safe (ADR-024 §2.2): sid / apply_id are NOT
// put in labels — that would blow up with host count; that breakdown
// belongs in trace/log (see the dispatch span in outbound.go).
type GRPCMetrics struct {
	// streamsActive — number of EventStream streams open right now.
	// Inc on handshake (after Hello→HelloReply), Dec when the handler
	// exits. A gauge with no labels: cardinality by sid is not acceptable;
	// "how many Souls on this instance" is visible from the Prometheus
	// target's instance label.
	streamsActive prometheus.Gauge

	// messagesTotal — counter of stream app messages, broken down by
	// direction (`from_soul` — received in the receive loop, `to_soul` —
	// sent in the send loop). The payload type is NOT put in a label:
	// there aren't many oneof variants, but breaking down by type gives a
	// histogram the kind of questions that trace answers better; direction
	// is the minimal breakdown needed for rate/error-budget observation of
	// the stream.
	messagesTotal *prometheus.CounterVec

	// applyDispatchTotal — counter of attempts to send an ApplyRequest to
	// a Soul ([Outbound.SendApply]), broken down by result (`ok` —
	// enqueue/publish succeeded, `failed` — ErrSoulNotConnected /
	// ErrOutboundQueueFull). This is the keeper→soul dispatch metric; a
	// rise in `failed` signals that Souls are unreachable or queues are
	// full.
	applyDispatchTotal *prometheus.CounterVec

	// bootstrapTotal — counter of a Soul's onboarding attempts via the
	// unary Bootstrap RPC ([bootstrapHandler.Bootstrap]), broken down by
	// result (`ok` — seed issued and the soul flipped to connected,
	// `failed` — any non-ok outcome: invalid token / CSR / Vault failure /
	// tx failure). Bootstrap is a separate listener (server-only TLS), not
	// EventStream: streams_active does NOT count it. A rise in `failed`
	// signals onboarding trouble (PKI down, garbage tokens).
	bootstrapTotal *prometheus.CounterVec

	// runResultStaleTotal — counter of RunResults rejected on receipt as
	// coming from a stale attempt (ADR-027(g), gate-1 epoch check on
	// receipt). Incremented in [correlateRunResult] when
	// RunResult.attempt < apply_runs.attempt: a result from a dead Ward
	// whose task has already been re-claimed with a higher epoch — the
	// state commit is skipped (stale-drop). No labels: breaking down by
	// sid/apply_id would blow up cardinality; details go to
	// correlateRunResult's Info log. A rising counter is a normal sign of
	// recovery at work (failback to another instance), not an error.
	runResultStaleTotal prometheus.Counter

	// applyOrphanedTotal — counter of dispatched rows terminalized to
	// `orphaned` by Soul reconcile (ADR-027(g), S6). Incremented in
	// [handleWardRoster] by the number of orphaned rows, when a Soul on
	// reconnect did NOT declare their apply_id in the WardRoster (no
	// RunResult will ever arrive for them — "both Keeper and Soul died
	// after handoff"). No labels: breaking down by sid/apply_id would blow
	// up cardinality; details go to handleWardRoster's Info log. A rise is
	// a normal sign of recovery after a Keeper+Soul pair crash, not an
	// error.
	applyOrphanedTotal prometheus.Counter
}

// Directions for keeper_grpc_messages_total. Closed enum of 2 values.
const (
	directionFromSoul = "from_soul"
	directionToSoul   = "to_soul"
)

// Results for keeper_grpc_apply_dispatch_total. Closed enum of 2 values.
const (
	dispatchResultOK     = "ok"
	dispatchResultFailed = "failed"
)

// Results for keeper_grpc_bootstrap_total. Closed enum of 2 values:
// `failed` aggregates every non-ok outcome (anti-enum for onboarding —
// cause detail goes to trace/log, not the metric label).
const (
	bootstrapResultOK     = "ok"
	bootstrapResultFailed = "failed"
)

// RegisterGRPCMetrics creates the keeper_grpc_* collectors and registers
// them in [obs.Registry]. Returns a handle for wiring through
// [EventStreamDeps] and [OutboundDeps].
//
// MustRegister: a duplicate registration is a programmer error (called
// twice on the same Registry); failing fast is more convenient than
// carrying lazy initialization (pattern identical to
// [obs.RegisterHTTPMetrics] / [obs.RegisterReaperMetrics]).
func RegisterGRPCMetrics(reg *obs.Registry) *GRPCMetrics {
	m := &GRPCMetrics{
		streamsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_grpc_streams_active",
			Help: "Число открытых EventStream-стримов Keeper↔Soul прямо сейчас.",
		}),
		messagesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_grpc_messages_total",
				Help: "Количество app-сообщений EventStream-а, разрезанное по направлению (from_soul/to_soul).",
			},
			[]string{"direction"},
		),
		applyDispatchTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_grpc_apply_dispatch_total",
				Help: "Количество попыток отправить ApplyRequest в Soul, разрезанное по результату (ok/failed).",
			},
			[]string{"result"},
		),
		bootstrapTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_grpc_bootstrap_total",
				Help: "Количество онбординг-попыток Soul-а через Bootstrap-RPC, разрезанное по результату (ok/failed).",
			},
			[]string{"result"},
		),
		runResultStaleTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_runresult_stale_total",
			Help: "Количество отвергнутых RunResult-ов от устаревших попыток (attempt < apply_runs.attempt, gate-1 epoch-check на приёме).",
		}),
		applyOrphanedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_apply_orphaned_total",
			Help: "Количество dispatched-строк, терминалённых в orphaned по Soul-reconcile (Soul на reconnect не объявил apply_id в WardRoster, ADR-027(g)).",
		}),
	}
	reg.Registerer().MustRegister(m.streamsActive, m.messagesTotal, m.applyDispatchTotal, m.bootstrapTotal, m.runResultStaleTotal, m.applyOrphanedTotal)
	return m
}

// IncStreams / DecStreams — nil-safe wrappers over the active-streams
// gauge. Inc after handshake, Dec in the handler's defer. A nil receiver
// is a no-op: EventStream can come up without observability (unit tests,
// dev build).
func (m *GRPCMetrics) IncStreams() {
	if m == nil {
		return
	}
	m.streamsActive.Inc()
}

func (m *GRPCMetrics) DecStreams() {
	if m == nil {
		return
	}
	m.streamsActive.Dec()
}

// ObserveMessage increments the message counter for the given direction.
// A nil receiver is a no-op.
func (m *GRPCMetrics) ObserveMessage(direction string) {
	if m == nil {
		return
	}
	m.messagesTotal.WithLabelValues(direction).Inc()
}

// ObserveApplyDispatch increments the ApplyRequest dispatch counter by
// result (err == nil → ok, otherwise failed). A nil receiver is a no-op.
func (m *GRPCMetrics) ObserveApplyDispatch(err error) {
	if m == nil {
		return
	}
	result := dispatchResultOK
	if err != nil {
		result = dispatchResultFailed
	}
	m.applyDispatchTotal.WithLabelValues(result).Inc()
}

// ObserveBootstrap increments the onboarding-attempts counter by result
// (err == nil → ok, otherwise failed). A nil receiver is a no-op: the
// Bootstrap listener can come up without the obs stack (unit tests, dev
// build).
func (m *GRPCMetrics) ObserveBootstrap(err error) {
	if m == nil {
		return
	}
	result := bootstrapResultOK
	if err != nil {
		result = bootstrapResultFailed
	}
	m.bootstrapTotal.WithLabelValues(result).Inc()
}

// ObserveRunResultStale increments the counter of RunResults from stale
// attempts (gate-1 epoch check on receipt, ADR-027(g)). Called from
// [correlateRunResult] on a stale-drop. A nil receiver is a no-op:
// correlateRunResult also runs in a unit build without the obs stack.
func (m *GRPCMetrics) ObserveRunResultStale() {
	if m == nil {
		return
	}
	m.runResultStaleTotal.Inc()
}

// ObserveApplyOrphaned adds n to the counter of orphaned dispatched rows
// (Soul reconcile, ADR-027(g), S6). Called from [handleWardRoster] with the
// number of rows moved to `orphaned`. A nil receiver is a no-op:
// handleWardRoster also runs in a unit build without the obs stack. n<=0 is
// a no-op (nothing to orphan).
func (m *GRPCMetrics) ObserveApplyOrphaned(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.applyOrphanedTotal.Add(float64(n))
}
