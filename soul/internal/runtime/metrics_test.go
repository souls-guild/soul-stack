package runtime

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterApplyMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterApplyMetrics(reg)
	if m == nil {
		t.Fatal("RegisterApplyMetrics returned nil")
	}

	// CounterVec/Histogram без первого Observe sample не публикуют — наблюдаем,
	// затем проверяем families.
	m.ObserveTask(applyResultOK)
	m.ObserveApplyDuration(0.5)

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	// Наблюдаем flow-control-метрики, чтобы их families попали в Gather.
	m.ObserveRetry()
	m.ObserveSkipped(skipReasonWhen)
	m.ObserveTimedOut()
	m.ObserveFenced()

	families, err = reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather (flow-control): %v", err)
	}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"soul_apply_tasks_total",
		"soul_apply_duration_seconds",
		"soul_apply_task_retries_total",
		"soul_apply_task_skipped_total",
		"soul_apply_task_timed_out_total",
		"soul_apply_fenced_total",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestApplyMetrics_SkippedByReason(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterApplyMetrics(reg)

	m.ObserveSkipped(skipReasonWhen)
	m.ObserveSkipped(skipReasonRequisite)
	m.ObserveSkipped(skipReasonRequisite)
	m.ObserveSkipped(skipReasonFailedRun)
	m.ObserveRetry()
	m.ObserveRetry()
	m.ObserveTimedOut()

	body := obstest.Scrape(t, reg.Gatherer())
	for substr := range map[string]struct{}{
		`soul_apply_task_skipped_total{reason="when"} 1`:       {},
		`soul_apply_task_skipped_total{reason="requisite"} 2`:  {},
		`soul_apply_task_skipped_total{reason="failed_run"} 1`: {},
		`soul_apply_task_retries_total 2`:                      {},
		`soul_apply_task_timed_out_total 1`:                    {},
	} {
		if !strings.Contains(body, substr) {
			t.Errorf("missing %q; got=\n%s", substr, body)
		}
	}
}

func TestApplyMetrics_FlowControlNilReceiver_NoOp(t *testing.T) {
	var m *ApplyMetrics
	m.ObserveRetry()
	m.ObserveSkipped(skipReasonWhen)
	m.ObserveTimedOut()
	m.ObserveFenced()
}

func TestRegisterApplyMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterApplyMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterApplyMetrics(reg)
}

func TestApplyMetrics_TasksByResult(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterApplyMetrics(reg)

	m.ObserveTask(applyResultOK)
	m.ObserveTask(applyResultChanged)
	m.ObserveTask(applyResultChanged)
	m.ObserveTask(applyResultFailed)

	body := obstest.Scrape(t, reg.Gatherer())
	for substr := range map[string]struct{}{
		`soul_apply_tasks_total{result="ok"} 1`:      {},
		`soul_apply_tasks_total{result="changed"} 2`: {},
		`soul_apply_tasks_total{result="failed"} 1`:  {},
	} {
		if !strings.Contains(body, substr) {
			t.Errorf("missing %q; got=\n%s", substr, body)
		}
	}
}

func TestApplyMetrics_NilReceiver_NoOp(t *testing.T) {
	var m *ApplyMetrics
	m.ObserveTask(applyResultOK)
	m.ObserveApplyDuration(1.0)
}

// TestRun_UpdatesTaskMetrics проверяет, что прогон через ApplyRunner
// инкрементирует soul_apply_tasks_total по фактическому результату задач и
// пишет soul_apply_duration_seconds.
func TestRun_UpdatesTaskMetrics(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterApplyMetrics(reg)

	registry := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
		"core.file": &fakeModule{
			applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{}) // no change → ok
			},
		},
	}
	r := NewApplyRunner(registry, m)
	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-metrics",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "pkg", Module: "core.pkg.installed"},
			{Name: "file", Module: "core.file.present"},
		},
	}, &recordingSink{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `soul_apply_tasks_total{result="changed"} 1`) {
		t.Errorf("expected one changed task; got=\n%s", body)
	}
	if !strings.Contains(body, `soul_apply_tasks_total{result="ok"} 1`) {
		t.Errorf("expected one ok task; got=\n%s", body)
	}
	if !strings.Contains(body, "soul_apply_duration_seconds_count 1") {
		t.Errorf("expected one duration observation; got=\n%s", body)
	}
}

// TestRun_FailedTaskMetric — упавшая задача попадает в result="failed".
func TestRun_FailedTaskMetric(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterApplyMetrics(reg)

	registry := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "boom"})
			},
		},
	}
	r := NewApplyRunner(registry, m)
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-fail",
		Tasks:   []*keeperv1.RenderedTask{{Name: "pkg", Module: "core.pkg.installed"}},
	}, &recordingSink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, `soul_apply_tasks_total{result="failed"} 1`) {
		t.Errorf("expected one failed task; got=\n%s", body)
	}
}

func TestTaskResult_Mapping(t *testing.T) {
	cases := map[keeperv1.TaskStatus]string{
		keeperv1.TaskStatus_TASK_STATUS_OK:        applyResultOK,
		keeperv1.TaskStatus_TASK_STATUS_CHANGED:   applyResultChanged,
		keeperv1.TaskStatus_TASK_STATUS_FAILED:    applyResultFailed,
		keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT: applyResultFailed,
		keeperv1.TaskStatus_TASK_STATUS_CANCELLED: applyResultFailed,
	}
	for status, want := range cases {
		if got := taskResult(status); got != want {
			t.Errorf("taskResult(%v) = %q, want %q", status, got, want)
		}
	}
}

// TestRun_Span_Created — прогон порождает span apply.run с атрибутом apply_id
// при enabled провайдере.
func TestRun_Span_Created(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	r := NewApplyRunner(mapRegistry{}, nil)
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{ApplyId: "apply-span"}, &recordingSink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name() != "apply.run" {
		t.Errorf("span name = %q, want apply.run", span.Name())
	}
	var applyID string
	for _, a := range span.Attributes() {
		if string(a.Key) == "apply_id" {
			applyID = a.Value.AsString()
		}
	}
	if applyID != "apply-span" {
		t.Errorf("span attr apply_id = %q", applyID)
	}
}

// TestRun_NoTracer_NoPanic — без явного провайдера (no-op глобальный) прогон не
// падает. Симулирует OTel disabled.
func TestRun_NoTracer_NoPanic(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{ApplyId: "x"}, &recordingSink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
