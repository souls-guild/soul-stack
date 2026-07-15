package scenario

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterScenarioMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterScenarioMetrics(reg)
	if m == nil {
		t.Fatal("RegisterScenarioMetrics returned nil")
	}

	// CounterVec doesn't publish without a first WithLabelValues sample;
	// histogram publishes immediately (count=0). Pull one sample each and check
	// families.
	m.ObserveRun(runResultOK, 1.0)
	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_scenario_runs_total",
		"keeper_scenario_run_duration_seconds",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterScenarioMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterScenarioMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterScenarioMetrics(reg)
}

func TestScenarioMetrics_RunsByResult(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterScenarioMetrics(reg)

	m.ObserveRun(runResultOK, 0.5)
	m.ObserveRun(runResultOK, 1.5)
	m.ObserveRun(runResultFailed, 0.2)
	m.ObserveRun(runResultLocked, 0)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_scenario_runs_total{result="ok"} 2`) {
		t.Errorf("ok count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_scenario_runs_total{result="failed"} 1`) {
		t.Errorf("failed count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_scenario_runs_total{result="locked"} 1`) {
		t.Errorf("locked count mismatch; got=\n%s", body)
	}
}

// TestScenarioMetrics_DurationOnlyForStartedRuns — a locked run never started,
// its duration doesn't go into the histogram (duration=0 → not observed). ok
// and failed (duration>0) are observed. We check the histogram's _count = 2,
// not 3.
func TestScenarioMetrics_DurationOnlyForStartedRuns(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterScenarioMetrics(reg)

	m.ObserveRun(runResultOK, 1.0)
	m.ObserveRun(runResultFailed, 2.0)
	m.ObserveRun(runResultLocked, 0)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_scenario_run_duration_seconds_count 2") {
		t.Errorf("duration histogram count should be 2 (locked excluded); got=\n%s", body)
	}
}

func TestScenarioMetrics_NilReceiver_NoOp(t *testing.T) {
	// Runner can start up without the obs stack (unit tests, dev build). The
	// method on a nil receiver is a no-op, no panic.
	var m *ScenarioMetrics
	m.ObserveRun(runResultOK, 1.0)
	m.ObserveRun(runResultLocked, 0)
}

// TestRun_Span_Created checks that a run produces a scenario.run span with
// incarnation/scenario/apply_id attributes and increments runs_total. The run
// fails at incarnation resolution (lazyPool with no connection) — a failed
// outcome, but the span must be created regardless (it wraps the whole run).
func TestRun_Span_Created(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	reg := obs.NewRegistry()
	metrics := RegisterScenarioMetrics(reg)
	r := newTestRunner(t)
	r.deps.Metrics = metrics

	r.run(context.Background(), RunSpec{
		ApplyID:         "01HRUN0000000000000000000Z",
		IncarnationName: "redis-prod",
		ScenarioName:    "create",
	})

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name() != "scenario.run" {
		t.Errorf("span name = %q, want scenario.run", span.Name())
	}
	attrs := map[string]string{}
	for _, a := range span.Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["incarnation"] != "redis-prod" {
		t.Errorf("span attr incarnation = %q", attrs["incarnation"])
	}
	if attrs["scenario"] != "create" {
		t.Errorf("span attr scenario = %q", attrs["scenario"])
	}
	if attrs["apply_id"] != "01HRUN0000000000000000000Z" {
		t.Errorf("span attr apply_id = %q", attrs["apply_id"])
	}

	// The run failed at incarnation resolution (no connection) → failed.
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_scenario_runs_total{result="failed"} 1`) {
		t.Errorf("expected one failed run from aborted run; got=\n%s", body)
	}
}

// TestRun_NoTracer_NoPanic — with a no-op global provider (OTel disabled) the
// run doesn't fail, the span is free. Metrics are off (nil Metrics) — also a
// no-op. Simulates production without observability.
func TestRun_NoTracer_NoPanic(t *testing.T) {
	r := newTestRunner(t) // Deps.Metrics == nil
	r.run(context.Background(), RunSpec{
		ApplyID:         "01HRUN0000000000000000000Z",
		IncarnationName: "redis-prod",
		ScenarioName:    "create",
	})
}
