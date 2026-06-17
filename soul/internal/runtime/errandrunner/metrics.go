package errandrunner

import (
	"github.com/prometheus/client_golang/prometheus"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// Metrics — soul_errand_*-collectors Errand-runner-а (ADR-033). Регистрируется
// helper-ом [Register] поверх компонент-агностичного [obs.Registry] — паттерн
// идентичен [soul/internal/runtime.RegisterApplyMetrics] / Keeper-side errand
// metrics. Labels — closed enum-ы (ADR-024 §2.2): cardinality безопасна.
//
// nil-получатель Observe* — no-op: Runner поднимается без obs-стека (push-
// режим не использует Errand, unit-тесты без obs).
type Metrics struct {
	// errandsTotal — счётчик завершённых Errand-ов, разрезанный по терминалу.
	// Closed enum status: success / failed / timed_out / cancelled /
	// module_not_allowed. Симметрично с keeper-side ResultEvent.Status.
	errandsTotal *prometheus.CounterVec

	// errandDuration — длительность одного Errand-а (от Run до возврата) в
	// секундах. Label module — closed enum по core-namespace (`core.cmd` /
	// `core.exec` / `core.http`). Для custom-плагина — `<namespace>.<name>`
	// без state-суффикса: cardinality ограничена набором установленных
	// плагинов на конкретном хосте (десятки максимум, ADR-020).
	errandDuration *prometheus.HistogramVec
}

// Status-label-значения для soul_errand_total. Маппинг см. [observeErrandLabel].
const (
	labelStatusSuccess          = "success"
	labelStatusFailed           = "failed"
	labelStatusTimedOut         = "timed_out"
	labelStatusCancelled        = "cancelled"
	labelStatusModuleNotAllowed = "module_not_allowed"
)

// Register создаёт soul_errand_*-collectors и регистрирует их в [obs.Registry].
// MustRegister: дубликат-регистрация — programmer error (паттерн обвязки
// идентичен RegisterApplyMetrics).
func Register(reg *obs.Registry) *Metrics {
	m := &Metrics{
		errandsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "soul_errand_total",
				Help: "Количество завершённых Errand-ов, разрезанное по терминалу.",
			},
			[]string{"status"},
		),
		errandDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "soul_errand_duration_seconds",
				Help: "Длительность одного Errand-а в секундах, по модулю (без state-суффикса).",
				// Errand — одиночный shell/exec/probe, типично < секунды;
				// server-cap dispatch-а 30s, hard-cap 300s. Bucket-ы покрывают
				// быстрый shell (50ms), типичный probe (250ms), долгий exec
				// (несколько секунд) и upper-bound timed_out (300s).
				Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
			},
			[]string{"module"},
		),
	}
	reg.Registerer().MustRegister(m.errandsTotal, m.errandDuration)
	return m
}

// ObserveErrand инкрементирует счётчик терминалов по статусу.
// nil-получатель — no-op.
func (m *Metrics) ObserveErrand(status keeperv1.ErrandStatus) {
	if m == nil {
		return
	}
	m.errandsTotal.WithLabelValues(statusLabel(status)).Inc()
}

// ObserveDuration записывает длительность Errand-а в секундах. module — это
// fully-qualified `<namespace>.<name>.<state>` (как в запросе), но в label
// кладётся `<namespace>.<name>` — закрытый по core-set (state-суффикс
// варьируется и взорвал бы cardinality). Пустой module (early-reject до
// resolve) → `unknown`.
func (m *Metrics) ObserveDuration(module string, seconds float64) {
	if m == nil {
		return
	}
	m.errandDuration.WithLabelValues(moduleLabel(module)).Observe(seconds)
}

// statusLabel — закрытый маппинг ErrandStatus → label-value.
// UNSPECIFIED / RUNNING не терминальны и сюда попасть не должны; defensive →
// "failed" (терминальный bucket-by-default).
func statusLabel(s keeperv1.ErrandStatus) string {
	switch s {
	case keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS:
		return labelStatusSuccess
	case keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT:
		return labelStatusTimedOut
	case keeperv1.ErrandStatus_ERRAND_STATUS_CANCELLED:
		return labelStatusCancelled
	case keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED:
		return labelStatusModuleNotAllowed
	default:
		return labelStatusFailed
	}
}

// moduleLabel срезает state-суффикс (`core.cmd.shell` → `core.cmd`). Пустой
// вход → "unknown" (early-reject до address-parse).
func moduleLabel(full string) string {
	if full == "" {
		return "unknown"
	}
	// Реализация дублирует splitModuleAddr (12 строк), но здесь нужен только
	// `<ns>.<name>` без флага ok — упрощённый rfind.
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '.' {
			if i > 0 {
				return full[:i]
			}
			return "unknown"
		}
	}
	return full
}
