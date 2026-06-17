package augur

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// tracer для in-process span-ов Augur-брокера. Берёт глобальный TracerProvider,
// поднятый [obs.SetupOTel] в main; при OTel disabled провайдер no-op — span-ы
// бесплатны и код не ветвится (ADR-024 §1.2). Используется gRPC-handler-ом
// (events_augur.go) вокруг резолва + fetch-а AugurRequest через [Tracer].
var tracer = otel.Tracer("keeper/augur")

// Tracer отдаёт пакетный OTel-tracer Augur-брокера для caller-а в другом пакете
// (grpc-handler), чтобы span обработки AugurRequest был привязан к одному
// instrumentation scope `keeper/augur`. Сам handler живёт в keeper/internal/grpc,
// но span — про Augur-семантику, поэтому tracer объявлен здесь.
func Tracer() trace.Tracer { return tracer }

// SpanName — имя in-process span-а обработки AugurRequest (резолв + fetch).
// Вынесено константой: handler стартует span этим именем, тест сверяет его.
const SpanName = "augur.request"

// BrokerMetrics — набор Prometheus-collector-ов Augur-брокера (ADR-025,
// обработка AugurRequest от Soul-а в EventStream-е). Регистрируется отдельным
// helper-ом поверх компонент-агностичного [obs.Registry] (ADR-024 §4.0):
// registry-core не знает про конкретные метрики, а keeper_augur_*-метрики —
// частность брокера.
//
// Метрики живут здесь (keeper/internal/augur), а не в shared/obs, потому что
// привязаны к keeper-внутреннему брокеру AugurRequest и не переиспользуются
// Soul-ом (ADR-011: shared/ — действительно поперечный код; Augur — Keeper-side).
//
// БЕЗОПАСНОСТЬ + кардинальность (ADR-024 §2.2, инвариант augur.md §8): в label-ы
// НЕ кладём omen_name, query, sid, apply_id, request_id, ни тем более значение
// секрета. Разрез — только closed-enum: `source` (тип внешней системы
// vault/prometheus/elk) и `decision` (исход ok/denied/error). Кто именно и что
// запрашивал — уходит в audit-log/trace, не в метрику.
//
// Имена — Prometheus convention (snake_case, _total для counter, _seconds для
// histogram длительности; ADR-024 §2.1).
type BrokerMetrics struct {
	// fetchTotal — счётчик обработанных AugurRequest-ов, разрезанный по
	// `source` (тип Omen-а) и `decision` (исход):
	//   - ok     — доступ разрешён И fetch успешен (AugurReply{OK});
	//   - denied — резолв отклонил доступ (AugurReply{DENIED});
	//   - error  — инфраструктурный сбой (резолв упал / fetch упал /
	//     concurrency-limit), AugurReply{ERROR}.
	// source для denied/error может быть unknown, когда тип ещё не определён
	// (Omen не найден / семафор переполнен до резолва) — см. [SourceUnknown].
	fetchTotal *prometheus.CounterVec

	// fetchDuration — длительность обработки одного AugurRequest в секундах
	// (резолв + fetch), разрезанная по `source`. Histogram отвечает на «сколько
	// длится брокинг по типу системы» (vault-KV дёшев, prom/elk — внешний HTTP);
	// разрез по decision не нужен — для p99 хватает per-source серии.
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
