package main

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/keeper/internal/watchman"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// watchmanMetrics -- keeper_watchman_* collectors (isolation detection +
// soul-shedding S2). Implements [watchman.Metrics]. Lives in the daemon
// wiring (not in the watchman package): the same approach as the
// conclaveInstances gauge -- the package keeps pure domain logic, while the
// concrete prometheus descriptors are wired up in main over the shared
// [obs.Registry].
type watchmanMetrics struct {
	// isolated -- instance-state gauge: 1 = declared isolated (streams
	// shed), 0 = healthy. No breakdown by reason (PG vs Redis) -- the detail
	// goes into the Watchman warn/error log.
	isolated prometheus.Gauge
	// streamsShed -- counter of EventStream streams closed by shedding over
	// the process's whole lifetime (grows on each isolation declaration by
	// the number of local streams at the moment of CloseAll).
	streamsShed prometheus.Counter
}

var _ watchman.Metrics = (*watchmanMetrics)(nil)

// registerWatchmanMetrics creates the keeper_watchman_* collectors and
// registers them in [obs.Registry]. MustRegister -- a duplicate is a
// programmer error (same pattern as RegisterGRPCMetrics / conclaveInstances).
func registerWatchmanMetrics(reg *obs.Registry) *watchmanMetrics {
	m := &watchmanMetrics{
		isolated: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_watchman_isolated",
			Help: "1 = keeper instance declared isolated by Watchman (local EventStream streams shed), 0 = healthy.",
		}),
		streamsShed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_watchman_streams_shed_total",
			Help: "Total number of local EventStream streams forcibly closed by Watchman shedding on instance isolation.",
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
