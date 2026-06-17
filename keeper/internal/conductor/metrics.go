package conductor

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// ConductorMetrics — набор Prometheus-collector-ов Conductor (ADR-048 §5,
// dashboard «лидер жив, спавн идёт по графику»). Parity reaper-метрик
// (keeper/internal/reaper/metrics.go), имена из словаря Soul Stack —
// `keeper_conductor_*`.
//
// Регистрируется отдельно через [RegisterConductorMetrics] только в ветке
// поднятого Conductor (cardinality-safe: при default-OFF без Redis или явном
// `cadence_scheduler.enabled: false` collectors не публикуются вовсе) — wire-up
// делает keeper/cmd/keeper/daemon.go строго там, где поднимается [Scheduler].
//
// Collector живёт рядом с подсистемой (docs/observability.md §4.0): метрики
// keeper-specific, их место в conductor-пакете, а не в shared/obs.
type ConductorMetrics struct {
	// LeaseHeld — 1 если этот Keeper-инстанс держит Redis-lease conductor:leader,
	// 0 иначе. Gauge без label-ов: cluster-wide invariant
	// `sum(keeper_conductor_lease_held) == 1` (при ровно одном лидере). Лидер
	// Conductor независим от лидера Reaper (отдельный lease, ADR-048 §1) —
	// держатели могут различаться. Per-`kid` label избыточен: дублирует
	// Prometheus-метку `instance` target-а.
	LeaseHeld prometheus.Gauge

	// SpawnExecutions — счётчик тиков спавна (увеличивается на каждый тик лидера,
	// независимо от того, нашлись ли due-расписания). Сравнение со
	// [SpawnedTotal] показывает «эффективность»: много тиков при нулевом спавне =
	// расписаний нет или все skip/queue.
	SpawnExecutions prometheus.Counter

	// SpawnedTotal — total заспавненных Voyage (skip/queue-тики не считаются —
	// affected = «сколько прогонов реально создано», parity CadenceSpawner.Run).
	SpawnedTotal prometheus.Counter

	// SpawnErrors — счётчик ошибок тика спавна (Spawner.Run вернул error).
	// Отдельно от [SpawnExecutions], чтобы алертилось без знания histogram-а.
	SpawnErrors prometheus.Counter

	// SpawnDuration — длительность вызова Spawner.Run в секундах. Buckets parity
	// reaper-rule-duration: типичный спавн-тик — единицы-десятки ms (SELECT due +
	// per-row insert), верх 30s отсекает аномальный долгий тик в отдельный
	// bucket, а не в `+Inf`.
	SpawnDuration prometheus.Histogram
}

// RegisterConductorMetrics создаёт collectors и регистрирует их в Registry-е.
// MustRegister: дубликат-регистрация — programmer error (вызывали дважды).
func RegisterConductorMetrics(r *obs.Registry) *ConductorMetrics {
	m := &ConductorMetrics{
		LeaseHeld: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_conductor_lease_held",
			Help: "1 если этот Keeper-инстанс держит Redis-lease лидерства Conductor (conductor:leader), 0 иначе.",
		}),
		SpawnExecutions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_conductor_spawn_executions_total",
			Help: "Количество тиков спавна due-Cadence Conductor-лидера.",
		}),
		SpawnedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_conductor_spawned_total",
			Help: "Количество Voyage, заспавненных Conductor-ом из созревших Cadence.",
		}),
		SpawnErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_conductor_spawn_errors_total",
			Help: "Количество ошибок тика спавна Conductor (Spawner вернул error).",
		}),
		SpawnDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "keeper_conductor_spawn_duration_seconds",
			Help:    "Длительность тика спавна due-Cadence Conductor в секундах.",
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}),
	}
	r.Registerer().MustRegister(m.LeaseHeld, m.SpawnExecutions, m.SpawnedTotal, m.SpawnErrors, m.SpawnDuration)
	return m
}

// ObserveSpawn — единый helper для записи метрик одного тика спавна. nil-safe
// (Conductor может быть инициализирован без observability — тесты). Caller не
// проверяет nil на каждом тике.
func (m *ConductorMetrics) ObserveSpawn(spawned int64, err error, dur time.Duration) {
	if m == nil {
		return
	}
	m.SpawnExecutions.Inc()
	m.SpawnDuration.Observe(dur.Seconds())
	if err != nil {
		m.SpawnErrors.Inc()
		return
	}
	if spawned > 0 {
		m.SpawnedTotal.Add(float64(spawned))
	}
}

// SetLeaseHeld — nil-safe запись Gauge лидерства. Подключается через
// [Config.OnLeaseChange] (parity reaper SetLeaseHeld через OnLeaseChange).
func (m *ConductorMetrics) SetLeaseHeld(held bool) {
	if m == nil {
		return
	}
	if held {
		m.LeaseHeld.Set(1)
		return
	}
	m.LeaseHeld.Set(0)
}
