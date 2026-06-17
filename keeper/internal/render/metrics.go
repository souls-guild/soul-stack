package render

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// RenderMetrics — набор Prometheus-collector-ов render-пайплайна Keeper-а
// (CEL+text/template-рендер scenario, ADR-010). Регистрируется отдельным
// helper-ом поверх компонент-агностичного [obs.Registry] — тем же паттерном,
// что [grpc.RegisterGRPCMetrics] / [scenario.RegisterScenarioMetrics] (ADR-024
// §4.0): registry-core не знает про конкретные метрики, а keeper_render_*-
// метрики — частность render-фасада Keeper-а.
//
// Метрики живут здесь (keeper/internal/render), а не в shared/obs, потому что
// привязаны к keeper-внутреннему [Pipeline] и не переиспользуются Soul-ом
// (ADR-011: shared/ — действительно поперечный код; рендер — Keeper-side по
// ADR-012(d), Soul cel-go/sprig не тянет).
//
// Имена — Prometheus convention (snake_case, _total для counter, _seconds для
// histogram длительности; ADR-024 §2.1). Labels — без секретов и без
// высокой кардинальности (ADR-024 §2.2): incarnation/scenario name НЕ кладём в
// label (blow-up по числу инкарнаций/сценариев) — их разрез идёт в trace
// (span render.pipeline несёт их атрибутами, pipeline.go). Params / vault-
// значения в метрики не попадают вовсе.
type RenderMetrics struct {
	// duration — длительность одного прохода [Pipeline.Render] в секундах
	// (vault-resolve → CEL-render → резолв on/where → сборка плана). Это
	// самая тяжёлая Keeper-side фаза прогона (тот же горизонт, что у span-а
	// render.pipeline). Histogram отвечает на «сколько длится рендер»; разрез
	// по результату не нужен — для p99 хватает общей серии, ошибка считается
	// отдельным counter-ом.
	duration prometheus.Histogram

	// errorsTotal — счётчик неуспешных проходов [Pipeline.Render] (любой не-nil
	// error: ErrUnsupportedDSL / vault-resolve-fail / CEL-fail / host-инвариант-
	// fail). Без label-а причины: детализация уходит в trace/log (span
	// render.pipeline ставит codes.Error), counter держим для алерта на
	// rate ошибок рендера без знания histogram-а.
	errorsTotal prometheus.Counter
}

// RegisterRenderMetrics создаёт keeper_render_*-collectors и регистрирует их в
// [obs.Registry]. Возвращает дескриптор для wire-up через [NewPipeline].
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на
// одном Registry); падать сразу удобнее, чем носить ленивую инициализацию
// (паттерн идентичен [grpc.RegisterGRPCMetrics]).
func RegisterRenderMetrics(reg *obs.Registry) *RenderMetrics {
	m := &RenderMetrics{
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_render_duration_seconds",
			Help:    "Длительность одного прохода render-пайплайна scenario в секундах (vault-resolve → CEL-render → резолв on/where).",
			Buckets: prometheus.DefBuckets,
		}),
		errorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_render_errors_total",
			Help: "Количество неуспешных проходов render-пайплайна scenario (любой error рендера).",
		}),
	}
	reg.Registerer().MustRegister(m.duration, m.errorsTotal)
	return m
}

// ObserveRender фиксирует завершение одного прохода [Pipeline.Render]:
// наблюдает длительность и, при err != nil, инкрементирует счётчик ошибок.
// nil-получатель — no-op: Pipeline может подниматься без observability
// (unit-тесты, dev-сборка, герметичный Trial).
func (m *RenderMetrics) ObserveRender(dur time.Duration, err error) {
	if m == nil {
		return
	}
	m.duration.Observe(dur.Seconds())
	if err != nil {
		m.errorsTotal.Inc()
	}
}
