package reaper

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// ReaperMetrics — набор Prometheus-collector-ов для Жнеца
// (docs/keeper/reaper.md → раздел «Метрики на каждое правило»).
//
// Регистрируется отдельно от core HTTP-метрик через [RegisterReaperMetrics],
// потому что Reaper — опциональная подсистема (`reaper.enabled: false`
// валиден, в этом случае метрики тоже не нужны и не публикуются). Wire-up
// делает `keeper/cmd/keeper/main.go::runDaemon` строго в той же ветке,
// где поднимается [Runner].
//
// Collector живёт рядом с подсистемой (docs/observability.md §4.0): метрики
// `keeper_reaper_*` keeper-specific, тянут per-rule-семантику Reaper-а —
// их место в `keeper/internal/reaper`, а не в `shared/obs`.
//
// Labels:
//   - `rule` — canonical имя правила из docs/keeper/reaper.md
//     (`purge_audit_old`, `expire_pending_seeds`, `purge_used_tokens`,
//     `purge_souls`, `purge_old_seeds`, `mark_disconnected`,
//     `purge_apply_runs`, `purge_voyages`, `purge_apply_task_register`,
//     `reclaim_apply_runs`, `reap_orphan_vault_keys`, `archive_state_history`).
//     Closed enum, cardinality-safe.
//
// `LeaseHeld` — без label-ов: lease — exclusive resource cluster-wide,
// каждый Keeper-инстанс публикует собственное значение (0/1). Scraper
// видит из метки `instance` Prometheus-target-а, кто именно лидер.
// Per-`kid` label избыточен — он дублирует `instance`.
type ReaperMetrics struct {
	// RuleExecutions — счётчик прогонов правила (увеличивается на каждый
	// dispatch, независимо от того, нашлись ли строки под удаление).
	// Сравнение с `RulePurgedTotal` даёт «эффективность» правила:
	// много executions при нулевом purged — кандидат на increase max_age.
	RuleExecutions *prometheus.CounterVec

	// RulePurgedTotal — total rows affected правилом. `set_status`-правила
	// (`mark_disconnected`) тоже инкрементируют — это affected rows,
	// а не строго «удалено».
	RulePurgedTotal *prometheus.CounterVec

	// RuleDuration — длительность вызова SQL-функции правила в секундах.
	// Buckets подобраны под профиль Reaper-prod: типичный batch — 10-100ms
	// (DELETE FROM ... WHERE ... LIMIT 1000), длинный аудит-purge с big
	// retention — секунды. Default Prometheus buckets (0.005..10) почти
	// подходят, но переключаем верх на 30s — long-purge на холодной БД
	// бывает 5-15s, отсечь его в `+Inf` маскирует регрессию.
	RuleDuration *prometheus.HistogramVec

	// DispatchErrors — счётчик ошибок dispatch-цикла (Purger вернул error).
	// Отдельно от `RuleExecutions`, чтобы алертилось без знания histogram-а.
	DispatchErrors *prometheus.CounterVec

	// LeaseHeld — 1 если этот Keeper-инстанс держит Redis-lease лидерства,
	// 0 иначе. Gauge без label-ов: cluster-wide invariant —
	// `sum(keeper_reaper_lease_held) == 1` (при ровно одном leader-е),
	// несбалансированность видна оператору без алертинга.
	LeaseHeld prometheus.Gauge
}

// RegisterReaperMetrics создаёт collectors и регистрирует их в Registry-е.
// Возвращает дескриптор для wire-up через [Deps].
//
// MustRegister: дубликат-регистрация — programmer error (вызывали дважды);
// падать сразу удобнее, чем носить ленивую инициализацию.
func RegisterReaperMetrics(r *obs.Registry) *ReaperMetrics {
	m := &ReaperMetrics{
		RuleExecutions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_reaper_rule_executions_total",
				Help: "Количество прогонов правил Reaper-а, разрезанное по rule.",
			},
			[]string{"rule"},
		),
		RulePurgedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_reaper_rule_purged_total",
				Help: "Affected rows правил Reaper-а (delete или set_status), по rule.",
			},
			[]string{"rule"},
		),
		RuleDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "keeper_reaper_rule_duration_seconds",
				Help:    "Длительность вызова SQL-функции правила Reaper-а в секундах, по rule.",
				Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
			},
			[]string{"rule"},
		),
		DispatchErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_reaper_dispatch_errors_total",
				Help: "Количество ошибок dispatch-цикла Reaper-а (Purger вернул error), по rule.",
			},
			[]string{"rule"},
		),
		LeaseHeld: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_reaper_lease_held",
			Help: "1 если этот Keeper-инстанс держит Redis-lease лидерства Reaper-а, 0 иначе.",
		}),
	}
	r.Registerer().MustRegister(m.RuleExecutions, m.RulePurgedTotal, m.RuleDuration, m.DispatchErrors, m.LeaseHeld)
	return m
}

// ObserveRule — единый helper для записи всех per-rule метрик одной
// dispatch-итерации. Использование на стороне Reaper-а:
//
//	start := time.Now()
//	affected, err := call(ctx, ...)
//	m.ObserveRule(rule, affected, err, time.Since(start))
//
// Метрики на nil-получателе — no-op (Reaper может быть инициализирован без
// observability — тесты, ranchard-режим). Это позволяет caller-у не
// проверять nil на каждой итерации.
func (m *ReaperMetrics) ObserveRule(rule string, affected int64, err error, dur time.Duration) {
	if m == nil {
		return
	}
	m.RuleExecutions.WithLabelValues(rule).Inc()
	m.RuleDuration.WithLabelValues(rule).Observe(dur.Seconds())
	if err != nil {
		m.DispatchErrors.WithLabelValues(rule).Inc()
		return
	}
	if affected > 0 {
		m.RulePurgedTotal.WithLabelValues(rule).Add(float64(affected))
	}
}

// SetLeaseHeld — sugar для nil-safe записи Gauge.
func (m *ReaperMetrics) SetLeaseHeld(held bool) {
	if m == nil {
		return
	}
	if held {
		m.LeaseHeld.Set(1)
		return
	}
	m.LeaseHeld.Set(0)
}
