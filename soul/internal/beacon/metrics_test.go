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
	// A counter isn't published before its first Inc — observe, then check the family.
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
	m.ObservePortentDropped() // must not panic
}

// TestEmit_DropIncrementsMetric — when the channel buffer overflows, emit
// drops the Portent and increments soul_beacon_portents_dropped_total.
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

	// The first emit takes the buffer's only slot (not dropped).
	s.emit(context.Background(), def, "changed", nil)
	// The second emit — the buffer is full, select falls into default → drop + metric.
	s.emit(context.Background(), def, "changed", nil)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "soul_beacon_portents_dropped_total 1") {
		t.Errorf("expected one drop; got=\n%s", body)
	}
}

// TestEmit_NoDropNoMetric — with a free buffer, emit doesn't drop and leaves
// the metric untouched (the drop branch didn't fire).
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
