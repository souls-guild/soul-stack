package beacon

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterBeaconMetrics_RegistersFamily(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterBeaconMetrics(reg)
	if m == nil {
		t.Fatal("RegisterBeaconMetrics returned nil")
	}
	// Counter без первого Inc не публикуется — наблюдаем, затем проверяем family.
	m.ObservePortentDropped()

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "soul_beacon_portents_dropped_total 1") {
		t.Errorf("missing soul_beacon_portents_dropped_total; got=\n%s", body)
	}
}

func TestRegisterBeaconMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterBeaconMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterBeaconMetrics(reg)
}

func TestBeaconMetrics_NilReceiver_NoOp(t *testing.T) {
	var m *BeaconMetrics
	m.ObservePortentDropped() // не должно паниковать
}

// TestEmit_DropIncrementsMetric — при переполнении буфера канала emit дропает
// Portent и инкрементирует soul_beacon_portents_dropped_total.
func TestEmit_DropIncrementsMetric(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterBeaconMetrics(reg)

	s := NewScheduler(SchedulerConfig{
		Registry:      &Registry{beacons: map[string]Beacon{}},
		SID:           "host.example",
		PortentBuffer: 1,
		Metrics:       m,
	})

	def := vigil("v1", "core.beacon.file_changed", "1s")

	// Первый emit занимает единственный слот буфера (не дропается).
	s.emit(context.Background(), def, "changed", nil)
	// Второй emit — буфер полон, select уходит в default → дроп + метрика.
	s.emit(context.Background(), def, "changed", nil)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "soul_beacon_portents_dropped_total 1") {
		t.Errorf("expected one drop; got=\n%s", body)
	}
}

// TestEmit_NoDropNoMetric — при свободном буфере emit не дропает и метрику не
// трогает (drop-ветка не сработала).
func TestEmit_NoDropNoMetric(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterBeaconMetrics(reg)

	s := NewScheduler(SchedulerConfig{
		Registry:      &Registry{beacons: map[string]Beacon{}},
		SID:           "host.example",
		PortentBuffer: 4,
		Metrics:       m,
	})

	s.emit(context.Background(), vigil("v1", "core.beacon.file_changed", "1s"), "changed", nil)

	body := obstest.Scrape(t, reg.Gatherer())
	if strings.Contains(body, "soul_beacon_portents_dropped_total 1") {
		t.Errorf("unexpected drop on free buffer; got=\n%s", body)
	}
}
