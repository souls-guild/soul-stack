package runtime

import (
	"github.com/prometheus/client_golang/prometheus"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// ApplyMetrics — набор Prometheus-collector-ов apply-цикла Soul-демона
// (runtime-подсистема, ADR-012/ADR-015). Регистрируется helper-ом поверх
// компонент-агностичного [obs.Registry] — тем же паттерном, что пилот
// [keeper/internal/grpc.RegisterGRPCMetrics] (docs/observability.md §4.0).
//
// Метрики живут здесь (soul/internal/runtime), а не в shared/obs: они привязаны
// к apply-циклу Soul-агента и не переиспользуются Keeper-ом (ADR-011: shared/ —
// действительно поперечный код). По ADR-011 soul НЕ импортирует keeper; обе
// стороны инструментируются через нейтральный shared/obs.
//
// Имена — ADR-024 §2.1: префикс soul_ (роль компонента), snake_case, _total для
// counter, histogram по измеряемой величине + _seconds. Labels — closed enum-ы
// (§2.2): apply_id / sid в labels НЕ кладём — cardinality-blow-up, их разрез — в
// trace (span apply.run).
type ApplyMetrics struct {
	// tasksTotal — счётчик завершённых задач прогона, разрезанный по
	// результату (`ok` / `changed` / `failed`). Closed enum в 3 значения;
	// именно эта метрика приводится примером soul_*-имени в naming-rules.
	tasksTotal *prometheus.CounterVec

	// applyDuration — длительность одного прогона (Run целиком), в секундах.
	// Без label-ов: разрез по apply_id — cardinality (§2.2); распределение
	// длительностей прогонов отвечает на «насколько медленный apply».
	applyDuration prometheus.Histogram

	// taskRetries — счётчик ПОВТОРНЫХ попыток runTask (retry:/until:, DSL-ядро
	// flow-control, destiny/tasks.md §9). Инкрементируется на каждой попытке
	// со второй (первая — не retry). Рост — нестабильные задачи / flaky-хосты.
	// Без label-ов: разрез по task/apply_id — cardinality (§2.2).
	taskRetries prometheus.Counter

	// taskSkipped — счётчик задач, пропущенных gating-ом flow-control (mod.Apply
	// не вызывался), разрезанный по причине. Closed enum reason: `when` (when:
	// false), `requisite` (onchanges/onfail не сработал), `failed_run` (прогон
	// уже провален, не-onfail-задача пропущена fail-stop-ом).
	taskSkipped *prometheus.CounterVec

	// taskTimedOut — счётчик задач, завершившихся таймаутом (TASK_STATUS_TIMED_OUT).
	// Выделен из общего failed-результата soul_apply_tasks_total: таймаут — особый
	// сигнал «висит», полезен отдельной серией для алертов. Считается по финальному
	// исходу задачи (после исчерпания retry-попыток).
	taskTimedOut prometheus.Counter

	// applyFenced — счётчик ApplyRequest, отвергнутых attempt-fencing-guard-ом
	// (ADR-027(g), Phase 2): stale-дубль с attempt < виденного для apply_id.
	// Рост — recovery-скан вернул в очередь ещё-живой Ward (lease короче apply)
	// либо реальное двойное исполнение, которое guard отсёк. Без apply_id-label-а
	// (cardinality, ADR-024 §2.2): разрез по прогону — в trace, не в метрике.
	applyFenced prometheus.Counter
}

// Причины skip для soul_apply_task_skipped_total. Closed enum в 3 значения —
// gating flow-control в Run (when / requisite-onchanges-onfail / fail-stop).
const (
	skipReasonWhen      = "when"
	skipReasonRequisite = "requisite"
	skipReasonFailedRun = "failed_run"
)

// Результаты для soul_apply_tasks_total. Closed enum в 3 значения —
// маппинг из [keeperv1.TaskStatus] (ok/changed/failed; timed_out и cancelled
// сводятся к failed как терминальные не-успехи задачи).
const (
	applyResultOK      = "ok"
	applyResultChanged = "changed"
	applyResultFailed  = "failed"
)

// RegisterApplyMetrics создаёт soul_apply_*-collectors и регистрирует их в
// [obs.Registry]. Возвращает дескриптор для wire-up через [ApplyRunner].
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на
// одном Registry); падать сразу удобнее ленивой инициализации (паттерн
// идентичен пилоту RegisterGRPCMetrics).
func RegisterApplyMetrics(reg *obs.Registry) *ApplyMetrics {
	m := &ApplyMetrics{
		tasksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "soul_apply_tasks_total",
				Help: "Количество завершённых задач прогона, разрезанное по результату (ok/changed/failed).",
			},
			[]string{"result"},
		),
		applyDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "soul_apply_duration_seconds",
			Help: "Длительность одного прогона apply (Run целиком), в секундах.",
			// Apply-прогон тяжелее HTTP-запроса (пакеты/файлы/сервисы): типичный
			// прогон — секунды-десятки секунд, тяжёлый (компиляция/большой
			// архив) — минуты. Сужать верх до 5s (как keeper_http) нельзя —
			// потеряем хвост; расширяем до 300s, сохраняя гранулярность внизу.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}),
		taskRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_apply_task_retries_total",
			Help: "Количество повторных попыток runTask (retry:/until:), без учёта первой попытки.",
		}),
		taskSkipped: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "soul_apply_task_skipped_total",
				Help: "Количество задач, пропущенных gating-ом flow-control, по причине (when/requisite/failed_run).",
			},
			[]string{"reason"},
		),
		taskTimedOut: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_apply_task_timed_out_total",
			Help: "Количество задач, завершившихся таймаутом (по финальному исходу после retry).",
		}),
		applyFenced: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_apply_fenced_total",
			Help: "Количество ApplyRequest, отвергнутых attempt-fencing-guard-ом (stale-дубль, attempt < виденного).",
		}),
	}
	reg.Registerer().MustRegister(
		m.tasksTotal, m.applyDuration,
		m.taskRetries, m.taskSkipped, m.taskTimedOut, m.applyFenced,
	)
	return m
}

// ObserveTask инкрементирует счётчик задач по результату прогона задачи.
// nil-получатель — no-op: ApplyRunner может подниматься без obs-стека
// (unit-тесты, push-режим без metrics-listener-а).
func (m *ApplyMetrics) ObserveTask(result string) {
	if m == nil {
		return
	}
	m.tasksTotal.WithLabelValues(result).Inc()
}

// ObserveApplyDuration записывает длительность прогона в секундах.
// nil-получатель — no-op.
func (m *ApplyMetrics) ObserveApplyDuration(seconds float64) {
	if m == nil {
		return
	}
	m.applyDuration.Observe(seconds)
}

// ObserveRetry инкрементирует счётчик повторных попыток runTask.
// nil-получатель — no-op.
func (m *ApplyMetrics) ObserveRetry() {
	if m == nil {
		return
	}
	m.taskRetries.Inc()
}

// ObserveSkipped инкрементирует счётчик пропущенных задач по причине.
// nil-получатель — no-op.
func (m *ApplyMetrics) ObserveSkipped(reason string) {
	if m == nil {
		return
	}
	m.taskSkipped.WithLabelValues(reason).Inc()
}

// ObserveTimedOut инкрементирует счётчик задач, завершившихся таймаутом.
// nil-получатель — no-op.
func (m *ApplyMetrics) ObserveTimedOut() {
	if m == nil {
		return
	}
	m.taskTimedOut.Inc()
}

// ObserveFenced инкрементирует счётчик ApplyRequest, отвергнутых attempt-fencing-
// guard-ом (stale-дубль). nil-получатель — no-op.
func (m *ApplyMetrics) ObserveFenced() {
	if m == nil {
		return
	}
	m.applyFenced.Inc()
}

// taskResult сводит [keeperv1.TaskStatus] к closed enum result-label-а
// soul_apply_tasks_total. changed → changed; failed/timed_out/cancelled →
// failed (терминальные не-успехи); ok, skipped (onchanges-gating) и всё прочее → ok.
func taskResult(status keeperv1.TaskStatus) string {
	switch status {
	case keeperv1.TaskStatus_TASK_STATUS_CHANGED:
		return applyResultChanged
	case keeperv1.TaskStatus_TASK_STATUS_FAILED,
		keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT,
		keeperv1.TaskStatus_TASK_STATUS_CANCELLED:
		return applyResultFailed
	default:
		return applyResultOK
	}
}
