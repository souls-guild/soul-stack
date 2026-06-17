package beacon

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// BeaconMetrics — набор Prometheus-collector-ов beacon-scheduler-а Soul-демона
// (ADR-030 S1/S4). Регистрируется helper-ом поверх компонент-агностичного
// [obs.Registry] — тем же паттерном, что [runtime.RegisterApplyMetrics]
// (docs/observability.md §4.0): collector живёт рядом с подсистемой, register —
// на soul-registry.
//
// Метрики живут здесь (soul/internal/beacon), а не в shared/obs: они привязаны к
// per-process beacon-scheduler-у Soul-агента и не переиспользуются Keeper-ом
// (ADR-011: shared/ — действительно поперечный код).
//
// Имена — ADR-024 §2.1: префикс soul_ (роль компонента), snake_case, _total для
// counter. Без label-ов (cardinality §2.2): разрез по vigil-name — high-
// cardinality, его место в логе/trace, не в метрике.
type BeaconMetrics struct {
	// portentsDropped — счётчик Portent-ов, отброшенных при переполнении буфера
	// канала ([Scheduler.emit] drop-ветка): writer-loop EventStream-а отстаёт
	// либо нет активной сессии надолго. Дроп edge-triggered события — потеря
	// одного перехода (следующая смена State снова поднимет Portent); ненулевой
	// рост — сигнал «реакции теряются», alert-кандидат.
	portentsDropped prometheus.Counter
}

// RegisterBeaconMetrics создаёт soul_beacon_*-collectors и регистрирует их в
// [obs.Registry]. Возвращает дескриптор для wire-up через [SchedulerConfig].
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на одном
// Registry); падать сразу удобнее ленивой инициализации (паттерн идентичен
// [runtime.RegisterApplyMetrics]).
func RegisterBeaconMetrics(reg *obs.Registry) *BeaconMetrics {
	m := &BeaconMetrics{
		portentsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soul_beacon_portents_dropped_total",
			Help: "Количество Portent-ов, отброшенных при переполнении буфера канала (writer-loop отстаёт / нет сессии).",
		}),
	}
	reg.Registerer().MustRegister(m.portentsDropped)
	return m
}

// ObservePortentDropped инкрементирует счётчик отброшенных Portent-ов.
// nil-получатель — no-op: scheduler может подниматься без obs-стека (unit-тесты,
// metrics.enabled=false), caller не проверяет nil на каждом дропе.
func (m *BeaconMetrics) ObservePortentDropped() {
	if m == nil {
		return
	}
	m.portentsDropped.Inc()
}
