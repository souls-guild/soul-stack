package grpc

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterEventStreamMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterEventStreamMetrics(reg)
	if m == nil {
		t.Fatal("RegisterEventStreamMetrics returned nil")
	}

	// connected is a gauge, visible immediately at 0. reconnects (a counter
	// with no labels) is also published immediately at 0.
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "soul_eventstream_connected 0") {
		t.Errorf("missing connected=0 sample; got=\n%s", body)
	}
	if !strings.Contains(body, "soul_eventstream_reconnects_total 0") {
		t.Errorf("missing reconnects_total=0 sample; got=\n%s", body)
	}
}

func TestRegisterEventStreamMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterEventStreamMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterEventStreamMetrics(reg)
}

func TestEventStreamMetrics_Connected_Toggle(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterEventStreamMetrics(reg)

	m.SetConnected(true)
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "soul_eventstream_connected 1") {
		t.Errorf("connected should be 1 after SetConnected(true); got=\n%s", body)
	}
	m.SetConnected(false)
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "soul_eventstream_connected 0") {
		t.Errorf("connected should be 0 after SetConnected(false); got=\n%s", body)
	}
}

func TestEventStreamMetrics_ReconnectsGrows(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterEventStreamMetrics(reg)

	m.IncReconnects()
	m.IncReconnects()
	m.IncReconnects()

	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "soul_eventstream_reconnects_total 3") {
		t.Errorf("reconnects_total should be 3; got=\n%s", body)
	}
}

func TestEventStreamMetrics_NilReceiver_NoOp(t *testing.T) {
	// reconnect-loop can start without the obs stack (unit tests,
	// metrics.enabled=false). All methods on a nil receiver are a no-op, no panic.
	var m *EventStreamMetrics
	m.SetConnected(true)
	m.SetConnected(false)
	m.IncReconnects()
}
