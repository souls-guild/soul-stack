package render

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterRenderMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRenderMetrics(reg)
	if m == nil {
		t.Fatal("RegisterRenderMetrics returned nil")
	}

	// A Histogram/Counter with no Observe/Inc yet doesn't publish its family in
	// the exposition — observe both outcomes once, then check
	// the families are present.
	m.ObserveRender(5*time.Millisecond, nil)
	m.ObserveRender(time.Millisecond, errors.New("boom"))

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_render_duration_seconds",
		"keeper_render_errors_total",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterRenderMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterRenderMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterRenderMetrics(reg)
}

func TestRenderMetrics_ErrorsCounter(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRenderMetrics(reg)

	// err == nil observes duration, does NOT increment errors_total.
	m.ObserveRender(time.Millisecond, nil)
	m.ObserveRender(time.Millisecond, nil)
	m.ObserveRender(time.Millisecond, errors.New("x"))

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_render_errors_total 1") {
		t.Errorf("errors_total should be 1; got=\n%s", body)
	}
	// All three passes landed in the histogram (count=3).
	if !strings.Contains(body, "keeper_render_duration_seconds_count 3") {
		t.Errorf("duration count should be 3; got=\n%s", body)
	}
}

func TestRenderMetrics_NilReceiver_NoOp(t *testing.T) {
	// Pipeline may come up without the obs stack (unit tests, dev build, Trial).
	// The method on a nil receiver is a no-op, no panic.
	var m *RenderMetrics
	m.ObserveRender(time.Second, nil)
	m.ObserveRender(time.Second, errors.New("x"))
}
