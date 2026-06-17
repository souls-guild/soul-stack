package rbac

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// RBACMetrics — набор Prometheus-collector-ов RBAC-подсистемы Keeper-а
// (snapshot-rebuild + permission-checks + cluster-инвалидация, ADR-028).
// Регистрируется отдельным helper-ом поверх компонент-агностичного
// [obs.Registry] — тем же паттерном, что [scenario.RegisterScenarioMetrics] /
// [vault.RegisterVaultMetrics] (ADR-024 §4.0): registry-core не знает про
// конкретные метрики, а keeper_rbac_*-метрики — частность RBAC-фасада.
//
// Метрики живут здесь (keeper/internal/rbac), а не в shared/obs, потому что
// привязаны к keeper-внутреннему [Holder]/[Enforcer] и не переиспользуются
// Soul-ом (ADR-011: shared/ — действительно поперечный код; RBAC — Keeper-side).
//
// БЕЗОПАСНОСТЬ + кардинальность (ADR-024 §2.2, инвариант ADR-028): в label-ы
// НЕ кладём ни aid, ни permission, ни role_name, ни resource/action. Разрез —
// только closed-enum: `kind` ошибки rebuild (load/parse) и `result` проверки
// (allow/deny). Кто именно и что проверял — уходит в audit-log/trace, не в
// метрику.
//
// Имена — Prometheus convention (snake_case, _total для counter, _seconds для
// histogram длительности, _timestamp_seconds для абсолютного времени;
// ADR-024 §2.1).
type RBACMetrics struct {
	// rebuildDuration — длительность одной пересборки RBAC-снимка в секундах
	// (src.Load из БД → NewEnforcerFromSnapshot). Наблюдается на каждом
	// [Holder.Refresh] (TTL-poll, pub/sub-инвалидация, lazy-путь), независимо
	// от исхода.
	rebuildDuration prometheus.Histogram

	// rebuildErrorsTotal — счётчик неуспешных пересборок снимка, разрезанный
	// по фазе отказа: `load` — src.Load (БД недоступна/ошибка SELECT-ов),
	// `parse` — NewEnforcerFromSnapshot (невалидная permission в БД после
	// рассинхрона версий каталога). Деталь причины — в log/trace caller-а.
	rebuildErrorsTotal *prometheus.CounterVec

	// lastSuccessTimestamp — Unix-time последней УСПЕШНОЙ пересборки снимка.
	// Возраст снимка считается в PromQL как `time() - <этот gauge>` — отдельную
	// _age_seconds-метрику сознательно не заводим (gauge-возраст «протух» бы
	// между scrape-ами).
	lastSuccessTimestamp prometheus.Gauge

	// roles — число ролей в актуальном снимке (gauge, обновляется на каждом
	// успешном Refresh).
	roles prometheus.Gauge

	// operators — число операторов с ≥1 ролевой привязкой в актуальном снимке
	// (gauge). AID без привязок в снимок-enforcer не попадает — это default-deny,
	// в метрику не считается.
	operators prometheus.Gauge

	// checksTotal — счётчик permission-проверок ([Holder.Check]), разрезанный
	// по исходу: `allow` (err==nil) / `deny` (любой не-nil error, включая
	// misconfigured-call). Это горячий путь admin-API/MCP до tool-execution.
	checksTotal *prometheus.CounterVec

	// invalidationsTotal — счётчик принятых cluster-wide RBAC-инвалидаций
	// ([Holder.WatchInvalidations] callback). Инкремент на каждый сигнал,
	// до запуска перечита. Self-origin уже отфильтрован источником.
	invalidationsTotal prometheus.Counter
}

// Фазы отказа пересборки снимка для keeper_rbac_snapshot_rebuild_errors_total.
// Closed enum в 2 значения — разрезает «БД недоступна» (load) и «битый
// каталог» (parse): алертить на них надо по-разному.
const (
	rebuildErrorLoad  = "load"
	rebuildErrorParse = "parse"
)

// Исходы permission-проверки для keeper_rbac_checks_total. Closed enum в 2
// значения; deny агрегирует и явный ErrPermissionDenied, и misconfigured-call
// (пустой resource/action) — наружу оба = 403, разрез не нужен.
const (
	checkResultAllow = "allow"
	checkResultDeny  = "deny"
)

// RegisterRBACMetrics создаёт keeper_rbac_*-collectors и регистрирует их в
// [obs.Registry]. Возвращает дескриптор для wire-up через [Holder.SetMetrics].
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на
// одном Registry); падать сразу удобнее, чем носить ленивую инициализацию
// (паттерн идентичен [scenario.RegisterScenarioMetrics]).
func RegisterRBACMetrics(reg *obs.Registry) *RBACMetrics {
	m := &RBACMetrics{
		rebuildDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_rbac_snapshot_rebuild_duration_seconds",
			Help:    "Длительность пересборки RBAC-снимка в секундах (Load из БД → NewEnforcerFromSnapshot).",
			Buckets: prometheus.DefBuckets,
		}),
		rebuildErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_rbac_snapshot_rebuild_errors_total",
				Help: "Количество неуспешных пересборок RBAC-снимка, разрезанное по kind (load/parse).",
			},
			[]string{"kind"},
		),
		lastSuccessTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_rbac_snapshot_last_success_timestamp_seconds",
			Help: "Unix-время последней успешной пересборки RBAC-снимка (возраст = time() - этот gauge).",
		}),
		roles: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_rbac_snapshot_roles",
			Help: "Число ролей в актуальном RBAC-снимке.",
		}),
		operators: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_rbac_snapshot_operators",
			Help: "Число операторов с ≥1 ролевой привязкой в актуальном RBAC-снимке.",
		}),
		checksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_rbac_checks_total",
				Help: "Количество permission-проверок RBAC, разрезанное по результату (allow/deny).",
			},
			[]string{"result"},
		),
		invalidationsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_rbac_invalidations_received_total",
			Help: "Количество принятых cluster-wide RBAC-инвалидаций (pub/sub-сигналов).",
		}),
	}
	reg.Registerer().MustRegister(
		m.rebuildDuration,
		m.rebuildErrorsTotal,
		m.lastSuccessTimestamp,
		m.roles,
		m.operators,
		m.checksTotal,
		m.invalidationsTotal,
	)
	return m
}

// ObserveRebuildSuccess фиксирует успешную пересборку снимка ([Holder.Refresh]):
// наблюдает длительность, обновляет last_success_timestamp (Unix-now) и gauge-и
// roles/operators по актуальному снимку.
//
// nil-получатель — no-op: Holder может подниматься без observability
// (NewHolder в bootstrap-пути / unit-тестах до wire-up метрик).
func (m *RBACMetrics) ObserveRebuildSuccess(dur time.Duration, roleCount, operatorCount int) {
	if m == nil {
		return
	}
	m.rebuildDuration.Observe(dur.Seconds())
	m.lastSuccessTimestamp.Set(float64(time.Now().Unix()))
	m.roles.Set(float64(roleCount))
	m.operators.Set(float64(operatorCount))
}

// ObserveRebuildError фиксирует неуспешную пересборку снимка: наблюдает
// длительность и инкрементирует rebuild_errors_total с явной фазой отказа
// (`load` — src.Load из БД, `parse` — NewEnforcerFromSnapshot). Caller
// ([Holder.Refresh]) знает фазу точно, поэтому она передаётся, а не
// угадывается по типу ошибки. nil-получатель — no-op.
func (m *RBACMetrics) ObserveRebuildError(dur time.Duration, kind string) {
	if m == nil {
		return
	}
	m.rebuildDuration.Observe(dur.Seconds())
	m.rebuildErrorsTotal.WithLabelValues(kind).Inc()
}

// ObserveCheck инкрементирует checks_total по исходу одной [Holder.Check]:
// err==nil → allow, иначе → deny. nil-получатель — no-op.
func (m *RBACMetrics) ObserveCheck(err error) {
	if m == nil {
		return
	}
	result := checkResultAllow
	if err != nil {
		result = checkResultDeny
	}
	m.checksTotal.WithLabelValues(result).Inc()
}

// ObserveInvalidation инкрементирует invalidations_received_total на один
// принятый cluster-сигнал. nil-получатель — no-op.
func (m *RBACMetrics) ObserveInvalidation() {
	if m == nil {
		return
	}
	m.invalidationsTotal.Inc()
}
