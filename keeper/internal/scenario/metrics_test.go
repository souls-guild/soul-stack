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

	// CounterVec без первого WithLabelValues sample не публикует, histogram —
	// публикует сразу (count=0). Дёрнем по одному sample и проверим families.
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

// TestScenarioMetrics_DurationOnlyForStartedRuns — locked-прогон не стартовал,
// его длительность в histogram не попадает (duration=0 → не наблюдается). ok и
// failed (duration>0) — наблюдаются. Проверяем _count histogram-а = 2, не 3.
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
	// Runner может подниматься без obs-стека (unit-тесты, dev-сборка). Метод на
	// nil-получателе — no-op без паники.
	var m *ScenarioMetrics
	m.ObserveRun(runResultOK, 1.0)
	m.ObserveRun(runResultLocked, 0)
}

// TestRun_Span_Created проверяет, что прогон порождает span scenario.run с
// атрибутами incarnation/scenario/apply_id и инкрементит runs_total. Прогон
// падает на резолве incarnation (lazyPool без коннекта) — это failed-исход,
// span обязан создаться в любом случае (он оборачивает весь run).
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

	// Прогон провалился на резолве incarnation (нет коннекта) → failed.
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_scenario_runs_total{result="failed"} 1`) {
		t.Errorf("expected one failed run from aborted run; got=\n%s", body)
	}
}

// TestRun_NoTracer_NoPanic — при no-op глобальном провайдере (OTel disabled)
// прогон не падает, span бесплатен. Метрики выключены (nil Metrics) — тоже
// no-op. Симулирует production без observability.
func TestRun_NoTracer_NoPanic(t *testing.T) {
	r := newTestRunner(t) // Deps.Metrics == nil
	r.run(context.Background(), RunSpec{
		ApplyID:         "01HRUN0000000000000000000Z",
		IncarnationName: "redis-prod",
		ScenarioName:    "create",
	})
}
