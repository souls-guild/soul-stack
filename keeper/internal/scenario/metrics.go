package scenario

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// tracer для in-process span-ов scenario-runner-а. Берёт глобальный
// TracerProvider, поднятый [obs.SetupOTel] в main; при OTel disabled
// провайдер no-op — span-ы бесплатны и код не ветвится (ADR-024 §1.2).
var tracer = otel.Tracer("keeper/scenario")

// ScenarioMetrics — набор Prometheus-collector-ов scenario-runner-а
// (Keeper-side прогон scenario, ADR-009). Регистрируется отдельным helper-ом
// поверх компонент-агностичного [obs.Registry] — тем же паттерном, что
// [grpc.RegisterGRPCMetrics] (пилот ADR-024 §4.0): registry-core не знает про
// конкретные метрики, а keeper_scenario_*-метрики — частность runner-а.
//
// Метрики живут здесь (keeper/internal/scenario), а не в shared/obs, потому
// что привязаны к keeper-внутренней run-goroutine и не переиспользуются
// Soul-ом (ADR-011: shared/ — действительно поперечный код).
//
// Имена — Prometheus convention (snake_case, _total для counter, _seconds для
// histogram длительности; ADR-024 §2.1). Labels — closed enum (result),
// cardinality-safe (ADR-024 §2.2): incarnation/scenario name в labels НЕ
// кладём — это blow-up по числу инкарнаций/сценариев, их разрез — в trace
// (span scenario.run несёт их атрибутами).
type ScenarioMetrics struct {
	// runsTotal — счётчик завершённых прогонов scenario, разрезанный по
	// терминальному результату (`ok` — state закоммичен; `failed` — прогон
	// провалился и incarnation переведена в error_locked; `locked` — прогон
	// отклонён до старта, т.к. incarnation уже applying/error_locked).
	runsTotal *prometheus.CounterVec

	// runDuration — длительность прогона scenario в секундах (от старта
	// run-goroutine до терминала). Histogram отвечает на «сколько длятся
	// прогоны»; разрез по результату не нужен — для p99 хватает общей серии,
	// доменная детализация уходит в trace.
	runDuration prometheus.Histogram
}

// Результаты для keeper_scenario_runs_total. Closed enum в 3 значения,
// отражает терминальные исходы run-goroutine (run.go): commit / abort / отказ
// gate-а до старта.
const (
	runResultOK     = "ok"
	runResultFailed = "failed"
	runResultLocked = "locked"
)

// RegisterScenarioMetrics создаёт keeper_scenario_*-collectors и регистрирует
// их в [obs.Registry]. Возвращает дескриптор для wire-up через [Deps.Metrics].
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на
// одном Registry); падать сразу удобнее, чем носить ленивую инициализацию
// (паттерн идентичен [grpc.RegisterGRPCMetrics]).
func RegisterScenarioMetrics(reg *obs.Registry) *ScenarioMetrics {
	m := &ScenarioMetrics{
		runsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_scenario_runs_total",
				Help: "Количество завершённых прогонов scenario, разрезанное по результату (ok/failed/locked).",
			},
			[]string{"result"},
		),
		runDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_scenario_run_duration_seconds",
			Help:    "Длительность прогона scenario в секундах (от старта run-goroutine до терминала).",
			Buckets: prometheus.DefBuckets,
		}),
	}
	reg.Registerer().MustRegister(m.runsTotal, m.runDuration)
	return m
}

// ObserveRun фиксирует терминал прогона: инкрементирует runs_total по
// результату и (при duration > 0) кладёт длительность в histogram.
// nil-получатель — no-op: runner может подниматься без observability
// (unit-тесты, dev-сборка).
//
// duration <= 0 (прогон отклонён до старта, locked) histogram не наполняет —
// длительность измеряется только для реально стартовавших прогонов.
func (m *ScenarioMetrics) ObserveRun(result string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.runsTotal.WithLabelValues(result).Inc()
	if durationSeconds > 0 {
		m.runDuration.Observe(durationSeconds)
	}
}
