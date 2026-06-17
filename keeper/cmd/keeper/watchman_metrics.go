package main

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/keeper/internal/watchman"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// watchmanMetrics — keeper_watchman_*-collectors (изоляция-детект +
// soul-shedding S2). Реализует [watchman.Metrics]. Живут в daemon-обвязке (а не
// в пакете watchman): тот же приём, что conclaveInstances-gauge — пакет держит
// чистую domain-логику, а конкретные prometheus-дескрипторы wired-up в main
// поверх общего [obs.Registry].
type watchmanMetrics struct {
	// isolated — gauge состояния инстанса: 1 = объявлен изолированным (стримы
	// отшеддены), 0 = здоров. Разреза по причине нет (PG vs Redis) — детализация
	// уходит в warn/error-лог Watchman-а.
	isolated prometheus.Gauge
	// streamsShed — счётчик закрытых shedding-ом EventStream-стримов за всё время
	// жизни процесса (растёт на каждом объявлении изоляции на число локальных
	// стримов в момент CloseAll).
	streamsShed prometheus.Counter
}

var _ watchman.Metrics = (*watchmanMetrics)(nil)

// registerWatchmanMetrics создаёт keeper_watchman_*-collectors и регистрирует их
// в [obs.Registry]. MustRegister — дубликат = programmer error (паттерн
// RegisterGRPCMetrics / conclaveInstances).
func registerWatchmanMetrics(reg *obs.Registry) *watchmanMetrics {
	m := &watchmanMetrics{
		isolated: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_watchman_isolated",
			Help: "1 = keeper-инстанс объявлен изолированным Watchman-ом (локальные EventStream-стримы отшеддены), 0 = здоров.",
		}),
		streamsShed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_watchman_streams_shed_total",
			Help: "Суммарное число локальных EventStream-стримов, принудительно закрытых Watchman-shedding-ом при изоляции инстанса.",
		}),
	}
	reg.Registerer().MustRegister(m.isolated, m.streamsShed)
	return m
}

func (m *watchmanMetrics) SetIsolated(isolated bool) {
	if m == nil {
		return
	}
	if isolated {
		m.isolated.Set(1)
		return
	}
	m.isolated.Set(0)
}

func (m *watchmanMetrics) AddStreamsShed(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.streamsShed.Add(float64(n))
}
