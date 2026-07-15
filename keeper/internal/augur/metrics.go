package augur

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// tracer for the Augur broker's in-process spans. Takes the global
// TracerProvider set up by [obs.SetupOTel] in main; when OTel is disabled the
// provider is a no-op — spans are free and the code doesn't branch (ADR-024
// §1.2). Used by the gRPC handler (events_augur.go) around resolving + fetching
// AugurRequest via [Tracer].
var tracer = otel.Tracer("keeper/augur")

// Tracer hands out the Augur broker's package-level OTel tracer to a caller
// in another package (grpc handler), so the AugurRequest-handling span is
// tied to a single instrumentation scope `keeper/augur`. The handler itself
// lives in keeper/internal/grpc, but the span is about Augur semantics, so
// the tracer is declared here.
func Tracer() trace.Tracer { return tracer }

// SpanName — the name of the in-process span for handling AugurRequest
// (resolve + fetch). Pulled out as a constant: the handler starts the span
// with this name, and the test checks it.
const SpanName = "augur.request"

// BrokerMetrics — the Augur broker's set of Prometheus collectors (ADR-025,
// handling AugurRequest from Soul over the EventStream). Registered by a
// separate helper on top of the component-agnostic [obs.Registry] (ADR-024
// §4.0): the registry core knows nothing about specific metrics, and
// keeper_augur_*-metrics are the broker's own concern.
//
// The metrics live here (keeper/internal/augur), not in shared/obs, because
// they're tied to the keeper-internal AugurRequest broker and aren't reused
// by Soul (ADR-011: shared/ is genuinely cross-cutting code; Augur is
// Keeper-side).
//
// SECURITY + cardinality (ADR-024 §2.2, invariant augur.md §8): labels never
// carry omen_name, query, sid, apply_id, request_id, let alone a secret
// value. The only split is a closed enum: `source` (external system type
// vault/prometheus/elk) and `decision` (outcome ok/denied/error). Who
// requested what goes to the audit log/trace, not the metric.
//
// Names follow Prometheus convention (snake_case, _total for a counter,
// _seconds for a duration histogram; ADR-024 §2.1).
type BrokerMetrics struct {
	// fetchTotal — a counter of handled AugurRequests, split by `source`
	// (Omen type) and `decision` (outcome):
	//   - ok     — access allowed AND fetch succeeded (AugurReply{OK});
	//   - denied — resolve rejected access (AugurReply{DENIED});
	//   - error  — infrastructure failure (resolve failed / fetch failed /
	//     concurrency limit), AugurReply{ERROR}.
	// source for denied/error can be unknown when the type isn't determined
	// yet (Omen not found / semaphore full before resolve) — see [SourceUnknown].
	fetchTotal *prometheus.CounterVec

	// fetchDuration — duration of handling one AugurRequest in seconds
	// (resolve + fetch), split by `source`. The histogram answers "how long
	// does brokering take per system type" (vault-KV is cheap, prom/elk is
	// external HTTP); no split by decision — a per-source series is enough
	// for p99.
	fetchDuration *prometheus.HistogramVec
}

// Outcomes of handling an AugurRequest for keeper_augur_fetch_total{decision}.
// A closed 3-value enum; parallels the denied/error/ok branches of processAugurRequest.
const (
	DecisionOK     = "ok"
	DecisionDenied = "denied"
	DecisionError  = "error"
)

// SourceUnknown — the value of the `source` label when the Omen's type isn't
// determined yet at the point of accounting (Omen not found during resolve /
// concurrency limit rejected the request before resolve). Not to be confused
// with the closed enum [SourceType] vault/prometheus/elk — SourceUnknown
// doesn't exist in the registry, it's a "source unknown" marker.
const SourceUnknown = "unknown"

// RegisterBrokerMetrics creates the keeper_augur_*-collectors and registers
// them in [obs.Registry]. Returns a handle for wire-up through the gRPC
// handler (AugurDeps.Metrics).
//
// MustRegister: a duplicate registration is a programmer error (called twice
// on the same Registry); failing fast is more convenient than carrying lazy
// initialization (the pattern is identical to [rbac.RegisterRBACMetrics] /
// [scenario.RegisterScenarioMetrics]).
func RegisterBrokerMetrics(reg *obs.Registry) *BrokerMetrics {
	m := &BrokerMetrics{
		fetchTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_augur_fetch_total",
				Help: "Количество обработанных AugurRequest-ов, разрезанное по source (vault/prometheus/elk/unknown) и decision (ok/denied/error).",
			},
			[]string{"source", "decision"},
		),
		fetchDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "keeper_augur_fetch_duration_seconds",
				Help:    "Длительность обработки AugurRequest в секундах (резолв + fetch), по source.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"source"},
		),
	}
	reg.Registerer().MustRegister(m.fetchTotal, m.fetchDuration)
	return m
}

// ObserveFetch records the terminal outcome of one AugurRequest handling: it
// increments fetch_total{source,decision} and puts the duration into
// fetch_duration{source}. source is the Omen's type ([SourceType]) or
// [SourceUnknown] if the type isn't determined yet; decision is one of
// [DecisionOK]/[DecisionDenied]/[DecisionError].
//
// nil receiver is a no-op: the broker can come up without observability
// (unit tests, builds without Augur wire-up), so the caller doesn't need to
// check for nil on every request.
func (m *BrokerMetrics) ObserveFetch(source, decision string, dur time.Duration) {
	if m == nil {
		return
	}
	m.fetchTotal.WithLabelValues(source, decision).Inc()
	m.fetchDuration.WithLabelValues(source).Observe(dur.Seconds())
}
