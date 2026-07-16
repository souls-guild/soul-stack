package herald

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// DispatcherMetrics are Prometheus collectors for notification dispatcher and tap
// (ADR-052(c)). Registered over component-agnostic [obs.Registry]
// with same pattern as [scenario.RegisterScenarioMetrics] (ADR-024 §4.0).
//
// Observability of S2 point: how many events processed/matched, how many
// dropped due to full tap buffer (signal "consumer can't keep up"),
// how many match/enqueue errors.
type DispatcherMetrics struct {
	// keeper_herald_tap_dropped_total is events dropped by tap due to full
	// buffer (drop-counter, ADR-052(c) "non-blocking transfer to bounded
	// buffer"). Growth → consumer/PG rule cache can't keep up with write-path.
	tapDropped prometheus.Counter
	// keeper_herald_dispatch_total is events processed by dispatcher,
	// sliced by match result (matched/unmatched). No cardinality by
	// event_type — keep low dimensionality.
	dispatchTotal *prometheus.CounterVec
	// keeper_herald_matches_total is matched (event×Tiding) pairs total
	// (one event can match multiple rules → multiple jobs).
	matchesTotal prometheus.Counter
	// keeper_herald_dispatch_errors_total is errors loading rules / enqueuing
	// job (best-effort, doesn't kill write-path).
	errorsTotal prometheus.Counter
}

// RegisterDispatcherMetrics creates herald-dispatcher collectors and
// registers them in [obs.Registry]. MustRegister: duplicate registration is
// programmer error (pattern RegisterScenarioMetrics).
func RegisterDispatcherMetrics(reg *obs.Registry) *DispatcherMetrics {
	m := &DispatcherMetrics{
		tapDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_herald_tap_dropped_total",
			Help: "Audit events dropped by notification tap because the buffer is full (consumer cannot keep up).",
		}),
		dispatchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_dispatch_total",
			Help: "Events processed by notification dispatcher, split by match result (matched/unmatched).",
		}, []string{"result"}),
		matchesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_herald_matches_total",
			Help: "Matched pairs (event x Tiding rule); each creates one delivery job.",
		}),
		errorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_herald_dispatch_errors_total",
			Help: "Errors loading Tiding rules or enqueuing delivery jobs (best-effort).",
		}),
	}
	reg.Registerer().MustRegister(m.tapDropped, m.dispatchTotal, m.matchesTotal, m.errorsTotal)
	return m
}

// observeDrop increments drop counter. nil-receiver → no-op (tap can
// start without observability — unit-tests).
func (m *DispatcherMetrics) observeDrop() {
	if m == nil {
		return
	}
	m.tapDropped.Inc()
}

// observeDispatch records processing of one event: matched-bucket if
// matched>0, else unmatched; plus number of matched rules to matchesTotal.
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

// observeError increments dispatch error counter.
func (m *DispatcherMetrics) observeError() {
	if m == nil {
		return
	}
	m.errorsTotal.Inc()
}

// DeliveryMetrics are Prometheus collectors for delivery claim-queue worker
// (ADR-052(d), S3). Observability: how many attempts/successes/failures/retries.
//
// Cardinality: label `herald` is channel name from `heralds` registry. Registry
// is operator-managed and small (units–tens of channels), NOT unbounded
// user-input (unlike `event_type` which isn't in labels). 4 series ×
// number of channels is manageable. If channel count ever grows large,
// label can be removed without changing metric names.
type DeliveryMetrics struct {
	// keeper_herald_delivery_attempts_total is delivery attempts (each claim+POST).
	attempts *prometheus.CounterVec
	// keeper_herald_delivery_succeeded_total is successful terminals (2xx).
	succeeded *prometheus.CounterVec
	// keeper_herald_delivery_failed_total is terminal failures (retry exhausted
	// / no-retry error).
	failed *prometheus.CounterVec
	// keeper_herald_delivery_retries_total is requeues for retry (backoff).
	retries *prometheus.CounterVec
}

// RegisterDeliveryMetrics creates delivery collectors and registers them in
// [obs.Registry]. MustRegister: duplicate is programmer error (pattern
// RegisterDispatcherMetrics).
func RegisterDeliveryMetrics(reg *obs.Registry) *DeliveryMetrics {
	m := &DeliveryMetrics{
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_delivery_attempts_total",
			Help: "Webhook notification delivery attempts (claim + POST), by herald channel.",
		}, []string{"herald"}),
		succeeded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_delivery_succeeded_total",
			Help: "Successful notification deliveries (2xx terminal), by herald channel.",
		}, []string{"herald"}),
		failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_delivery_failed_total",
			Help: "Terminal delivery failures (retry exhausted / no-retry error), by herald channel.",
		}, []string{"herald"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "keeper_herald_delivery_retries_total",
			Help: "Notification requeues for retry with backoff, by herald channel.",
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
