package serviceregistry

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// RegistryMetrics — набор Prometheus-collector-ов реестра Service-ов/keeper_settings
// (ADR-029, snapshot-rebuild + cluster-инвалидация). Зеркало keeper_rbac_snapshot_*
// для Holder-а реестра (тот же паттерн SetMetrics/ObserveRebuild*/ObserveInvalidation,
// что у [rbac.Holder]); регистрируется отдельным helper-ом поверх компонент-
// агностичного [obs.Registry] (ADR-024 §4.0): registry-core не знает про конкретные
// метрики, а keeper_serviceregistry_*-метрики — частность этого Holder-а.
//
// Метрики живут здесь (keeper/internal/serviceregistry), а не в shared/obs, потому
// что привязаны к keeper-внутреннему [Holder] и не переиспользуются Soul-ом
// (ADR-011: shared/ — действительно поперечный код; реестр Service-ов — Keeper-side).
//
// БЕЗОПАСНОСТЬ + кардинальность (ADR-024 §2.2): в label-ы НЕ кладём имя Service-а,
// git/ref, значение настройки. Разрез rebuild-ошибки — только closed-enum `kind`
// (load/parse). Снимок реестра — это публичный каталог (имена/git-refs), не
// секрет, но cardinality по числу Service-ов всё равно недопустима в метриках.
//
// Имена — Prometheus convention (snake_case, _total для counter, _seconds для
// histogram длительности, _timestamp_seconds для абсолютного времени; ADR-024 §2.1).
type RegistryMetrics struct {
	// rebuildDuration — длительность одной пересборки снимка реестра в секундах
	// (src.Load из БД: ListServices + GetSetting). Наблюдается на каждом
	// [Holder.Refresh] (TTL-poll, pub/sub-инвалидация, lazy-путь), независимо от
	// исхода.
	rebuildDuration prometheus.Histogram

	// rebuildErrorsTotal — счётчик неуспешных пересборок снимка, разрезанный по
	// фазе отказа: `load` — src.Load (БД недоступна/ошибка SELECT-ов), `parse` —
	// построение снимка из строк (зарезервировано: текущий PoolSource.Load не
	// имеет отдельной parse-фазы, но симметрия с rbac оставляет ветку под будущий
	// типизированный декодер). Деталь причины — в log/trace caller-а.
	rebuildErrorsTotal *prometheus.CounterVec

	// lastSuccessTimestamp — Unix-time последней УСПЕШНОЙ пересборки снимка.
	// Возраст снимка считается в PromQL как `time() - <этот gauge>`.
	lastSuccessTimestamp prometheus.Gauge

	// services — число Service-ов в актуальном снимке (gauge, обновляется на
	// каждом успешном Refresh).
	services prometheus.Gauge

	// invalidationsTotal — счётчик принятых cluster-wide инвалидаций реестра
	// ([Holder.WatchInvalidations] callback). Инкремент на каждый сигнал, до
	// запуска перечита. Self-origin уже отфильтрован источником.
	invalidationsTotal prometheus.Counter
}

// Фазы отказа пересборки снимка для keeper_serviceregistry_snapshot_rebuild_errors_total.
// Closed enum: `load` — отказ src.Load из БД; `parse` — отказ построения снимка из
// прочитанных строк (зарезервировано, см. [RegistryMetrics.rebuildErrorsTotal]).
const (
	rebuildErrorLoad  = "load"
	rebuildErrorParse = "parse"
)

// RegisterRegistryMetrics создаёт keeper_serviceregistry_*-collectors и
// регистрирует их в [obs.Registry]. Возвращает дескриптор для wire-up через
// [Holder.SetMetrics].
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на одном
// Registry); падать сразу удобнее, чем носить ленивую инициализацию (паттерн
// идентичен [rbac.RegisterRBACMetrics]).
func RegisterRegistryMetrics(reg *obs.Registry) *RegistryMetrics {
	m := &RegistryMetrics{
		rebuildDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_serviceregistry_snapshot_rebuild_duration_seconds",
			Help:    "Длительность пересборки снимка реестра Service-ов в секундах (Load из БД).",
			Buckets: prometheus.DefBuckets,
		}),
		rebuildErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_serviceregistry_snapshot_rebuild_errors_total",
				Help: "Количество неуспешных пересборок снимка реестра, разрезанное по kind (load/parse).",
			},
			[]string{"kind"},
		),
		lastSuccessTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_serviceregistry_snapshot_last_success_timestamp_seconds",
			Help: "Unix-время последней успешной пересборки снимка реестра (возраст = time() - этот gauge).",
		}),
		services: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_serviceregistry_snapshot_services",
			Help: "Число Service-ов в актуальном снимке реестра.",
		}),
		invalidationsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_serviceregistry_invalidations_received_total",
			Help: "Количество принятых cluster-wide инвалидаций реестра Service-ов (pub/sub-сигналов).",
		}),
	}
	reg.Registerer().MustRegister(
		m.rebuildDuration,
		m.rebuildErrorsTotal,
		m.lastSuccessTimestamp,
		m.services,
		m.invalidationsTotal,
	)
	return m
}

// ObserveRebuildSuccess фиксирует успешную пересборку снимка ([Holder.Refresh]):
// наблюдает длительность, обновляет last_success_timestamp (Unix-now) и gauge
// числа Service-ов по актуальному снимку. nil-получатель — no-op (Holder может
// подниматься без observability: NewHolder в bootstrap-пути / unit-тестах до
// wire-up метрик).
func (m *RegistryMetrics) ObserveRebuildSuccess(dur time.Duration, serviceCount int) {
	if m == nil {
		return
	}
	m.rebuildDuration.Observe(dur.Seconds())
	m.lastSuccessTimestamp.Set(float64(time.Now().Unix()))
	m.services.Set(float64(serviceCount))
}

// ObserveRebuildError фиксирует неуспешную пересборку снимка: наблюдает
// длительность и инкрементирует rebuild_errors_total с явной фазой отказа.
// Caller ([Holder.Refresh]) знает фазу точно, поэтому она передаётся, а не
// угадывается по типу ошибки. nil-получатель — no-op.
func (m *RegistryMetrics) ObserveRebuildError(dur time.Duration, kind string) {
	if m == nil {
		return
	}
	m.rebuildDuration.Observe(dur.Seconds())
	m.rebuildErrorsTotal.WithLabelValues(kind).Inc()
}

// ObserveInvalidation инкрементирует invalidations_received_total на один
// принятый cluster-сигнал. nil-получатель — no-op.
func (m *RegistryMetrics) ObserveInvalidation() {
	if m == nil {
		return
	}
	m.invalidationsTotal.Inc()
}
