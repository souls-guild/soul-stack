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

// Исходы обработки AugurRequest для keeper_augur_fetch_total{decision}. Closed
// enum в 3 значения; параллель с denied/error/ok-ветвями processAugurRequest.
const (
	DecisionOK     = "ok"
	DecisionDenied = "denied"
	DecisionError  = "error"
)

// SourceUnknown — значение label-а `source`, когда тип Omen-а ещё не определён в
// момент учёта (Omen не найден при резолве / concurrency-limit отбил запрос до
// резолва). Не путать с closed-enum [SourceType] vault/prometheus/elk —
// SourceUnknown в реестре не существует, это метка «source неизвестен».
const SourceUnknown = "unknown"

// RegisterBrokerMetrics создаёт keeper_augur_*-collectors и регистрирует их в
// [obs.Registry]. Возвращает дескриптор для wire-up через gRPC-handler
// (AugurDeps.Metrics).
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на одном
// Registry); падать сразу удобнее, чем носить ленивую инициализацию (паттерн
// идентичен [rbac.RegisterRBACMetrics] / [scenario.RegisterScenarioMetrics]).
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

// ObserveFetch фиксирует терминал одной обработки AugurRequest: инкрементирует
// fetch_total{source,decision} и кладёт длительность в fetch_duration{source}.
// source — тип Omen-а ([SourceType]) или [SourceUnknown], если тип ещё не
// определён; decision — один из [DecisionOK]/[DecisionDenied]/[DecisionError].
//
// nil-получатель — no-op: брокер может подниматься без observability (unit-тесты,
// сборки без Augur-wire-up), caller не проверяет nil на каждом запросе.
func (m *BrokerMetrics) ObserveFetch(source, decision string, dur time.Duration) {
	if m == nil {
		return
	}
	m.fetchTotal.WithLabelValues(source, decision).Inc()
	m.fetchDuration.WithLabelValues(source).Observe(dur.Seconds())
}
