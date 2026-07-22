package soulprint

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterSoulprintMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterSoulprintMetrics(reg)
	if m == nil {
		t.Fatal("RegisterSoulprintMetrics returned nil")
	}

	m.ObserveCollection(collectResultOK)
	m.ObserveCollectDuration(0.01)

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"soul_soulprint_collections_total",
		"soul_soulprint_collect_duration_seconds",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterSoulprintMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterSoulprintMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterSoulprintMetrics(reg)
}

func TestSoulprintMetrics_CollectionsByResult(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterSoulprintMetrics(reg)

	m.ObserveCollection(collectResultOK)
	m.ObserveCollection(collectResultOK)
	m.ObserveCollection(collectResultFailed)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `soul_soulprint_collections_total{result="ok"} 2`) {
		t.Errorf("ok count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `soul_soulprint_collections_total{result="failed"} 1`) {
		t.Errorf("failed count mismatch; got=\n%s", body)
	}
}

func TestSoulprintMetrics_NilReceiver_NoOp(t *testing.T) {
	var m *SoulprintMetrics
	m.ObserveCollection(collectResultOK)
	m.ObserveCollectDuration(0.5)
}

// TestCollect_UpdatesMetrics — each Collect increments collections_total
// (result=ok, since collection is best-effort) and records
// collect_duration_seconds.
func TestCollect_UpdatesMetrics(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterSoulprintMetrics(reg)

	c := NewCollector(fakeSource{}, m)
	c.Collect(context.Background(), "host-metrics")
	c.Collect(context.Background(), "host-metrics")

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `soul_soulprint_collections_total{result="ok"} 2`) {
		t.Errorf("expected 2 ok collections; got=\n%s", body)
	}
	if !strings.Contains(body, "soul_soulprint_collect_duration_seconds_count 2") {
		t.Errorf("expected 2 duration observations; got=\n%s", body)
	}
}
