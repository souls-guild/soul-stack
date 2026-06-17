package toll

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// Metrics — keeper_toll_*-collectors + cluster:degraded gauge (ADR-038).
//
// Раздельные дескрипторы Watcher- и Leader-сторонами не делаем (как в watchman):
// Watcher и Leader живут в одном процессе, общий Registry, единый объект упрощает
// wire-up в daemon.
//
// nil-safe: все методы проверяют nil-получатель, чтобы unit-тесты пакета
// поднимались без обвязки obs.Registry (паттерн watchmanMetrics).
type Metrics struct {
	// clusterDegraded — gauge cluster-уровня (set ТОЛЬКО leader-ом, ADR-038
	// инвариант exclusive setter). На non-leader инстансах остаётся 0; на
	// leader-е leader-loop обновляет gauge по факту set/clear.
	clusterDegraded prometheus.Gauge

	// disconnectsTotal — counter не-graceful EventStream-disconnect-ов
	// (post-filter warmup + graceful-shutdown). Per-coven cardinality безопасна
	// на counter — это не fanout-флага, а наблюдательный rate-источник.
	disconnectsTotal *prometheus.CounterVec

	// warmupSkipped — счётчик disconnect-ов, отброшенных warmup-immunity
	// (первые WarmupDelay после старта инстанса). Дёшево, помогает оператору
	// отделить cold-start-волну от реального оттока.
	warmupSkipped prometheus.Counter

	// gracefulSkipped — счётчик disconnect-ов, отброшенных как graceful-shutdown
	// (плановое закрытие стрима этим же инстансом). Сигнал «rolling restart»
	// vs «реальный отток».
	gracefulSkipped prometheus.Counter

	// leaderActive — gauge «этот инстанс держит Toll-lease». 1 = leader, 0 =
	// follower. Сумма по всем инстансам ровно 1 при здоровом кластере
	// (Redis-lease гарантирует exclusive).
	leaderActive prometheus.Gauge
}

// RegisterMetrics создаёт keeper_toll_*-collectors + keeper_cluster_degraded
// gauge и регистрирует их в [obs.Registry]. MustRegister — дубликат-регистрация
// programmer error (паттерн RegisterGRPCMetrics / registerWatchmanMetrics).
func RegisterMetrics(reg *obs.Registry) *Metrics {
	m := &Metrics{
		clusterDegraded: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_cluster_degraded",
			Help: "1 — Toll-leader взвёл флаг cluster:degraded (rate disconnect > threshold за окно); 0 — нормальное состояние. Set ТОЛЬКО leader-ом (ADR-038).",
		}),
		disconnectsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_toll_disconnects_total",
				Help: "Не-graceful EventStream-disconnect-ы, наблюдённые tollwatcher-ом (post-filter graceful-shutdown / warmup-immunity). Per-coven cardinality безопасна на counter (ADR-038).",
			},
			[]string{"coven"},
		),
		warmupSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_toll_warmup_skipped_total",
			Help: "Disconnect-события, отброшенные warmup-immunity (первые WarmupDelay после старта инстанса).",
		}),
		gracefulSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_toll_graceful_skipped_total",
			Help: "Disconnect-события, отброшенные как graceful-shutdown этого инстанса (rolling restart vs реальный отток).",
		}),
		leaderActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_toll_leader_active",
			Help: "1 — этот keeper-инстанс держит lease cluster:toll:leader (агрегатор Toll); 0 — follower. Сумма по кластеру ровно 1 при здоровом Redis-lease.",
		}),
	}
	reg.Registerer().MustRegister(
		m.clusterDegraded,
		m.disconnectsTotal,
		m.warmupSkipped,
		m.gracefulSkipped,
		m.leaderActive,
	)
	return m
}

// SetClusterDegraded — set 0/1 для keeper_cluster_degraded. Вызывается
// ТОЛЬКО leader-ом (инвариант ADR-038).
func (m *Metrics) SetClusterDegraded(degraded bool) {
	if m == nil {
		return
	}
	if degraded {
		m.clusterDegraded.Set(1)
		return
	}
	m.clusterDegraded.Set(0)
}

// IncDisconnect — +1 к counter-у per-coven. Пустой coven кладётся как label
// "" — допустимо (cardinality стабильная, см. doc к полю).
func (m *Metrics) IncDisconnect(coven string) {
	if m == nil {
		return
	}
	m.disconnectsTotal.WithLabelValues(coven).Inc()
}

// IncWarmupSkipped — +1 при отбросе disconnect-а warmup-immunity.
func (m *Metrics) IncWarmupSkipped() {
	if m == nil {
		return
	}
	m.warmupSkipped.Inc()
}

// IncGracefulSkipped — +1 при отбросе disconnect-а как graceful-shutdown.
func (m *Metrics) IncGracefulSkipped() {
	if m == nil {
		return
	}
	m.gracefulSkipped.Inc()
}

// SetLeaderActive — 0/1 keeper_toll_leader_active. Обновляется leader-loop-ом
// по факту acquire/release lease.
func (m *Metrics) SetLeaderActive(active bool) {
	if m == nil {
		return
	}
	if active {
		m.leaderActive.Set(1)
		return
	}
	m.leaderActive.Set(0)
}
