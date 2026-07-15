package grpc

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// w3cPropagator — the same composite TraceContext that obs.SetupOTel sets up.
// Propagation tests set it explicitly (the global propagator defaults to a
// no-op provider that injects nothing).
func w3cPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

// TestSendApply_InjectsTraceContext verifies that with an active span in
// ctx, SendApply puts a non-empty W3C traceparent into req.TraceContext
// (ADR-024 cross-process trace propagation). Soul will extract it and raise
// apply.run as a child of grpc.apply_dispatch.
func TestSendApply_InjectsTraceContext(t *testing.T) {
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(w3cPropagator())
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })

	// An active valid parent SpanContext in ctx is enough for injecting the
	// traceparent regardless of the TracerProvider (Inject reads the
	// SpanContext from ctx). grpc.apply_dispatch inside SendApply inherits it.
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, parent := tp.Tracer("test").Start(context.Background(), "parent")
	defer parent.End()

	mgr := NewStreamManager(discardLogger(t))
	ob := newOutboundForTest(t, mgr, nopAudit{})

	// Soul isn't connected → deliver will return ErrSoulNotConnected, but the
	// injection happens BEFORE deliver — req.TraceContext must be populated
	// regardless.
	req := &keeperv1.ApplyRequest{ApplyId: "01HAPPLY00000000000000000Z"}
	if err := ob.SendApply(ctx, "host.example.test", req); !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("SendApply err = %v, want ErrSoulNotConnected", err)
	}

	if req.GetTraceContext() == "" {
		t.Fatal("req.TraceContext пуст, ожидался непустой W3C traceparent")
	}

	// Round-trip: Extract on the injected traceparent gives a valid remote
	// SpanContext with the same trace-id as the parent (= end-to-end trace).
	extracted := otel.GetTextMapPropagator().Extract(context.Background(),
		propagation.MapCarrier{"traceparent": req.GetTraceContext()})
	sc := trace.SpanContextFromContext(extracted)
	if !sc.IsValid() {
		t.Fatal("извлечённый SpanContext невалиден")
	}
	if sc.TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("trace_id = %s, want %s (трасса разорвана)", sc.TraceID(), parent.SpanContext().TraceID())
	}
}

// TestApplyRequest_ExtractTraceContext_RoundTrip models the Soul-side
// extraction: a non-empty traceparent → ctx with a valid remote SpanContext
// (apply.run becomes a child). Duplicates the logic of a line in
// soul/cmd/soul/main.go without spinning up the Soul daemon.
func TestApplyRequest_ExtractTraceContext_RoundTrip(t *testing.T) {
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(w3cPropagator())
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })

	const traceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	req := &keeperv1.ApplyRequest{TraceContext: traceparent}

	ctx := otel.GetTextMapPropagator().Extract(context.Background(),
		propagation.MapCarrier{"traceparent": req.GetTraceContext()})
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatal("SpanContext невалиден при непустом traceparent")
	}
	if !sc.IsRemote() {
		t.Error("SpanContext должен быть remote (пришёл из протокола)")
	}
	if got := sc.TraceID().String(); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("trace_id = %s, want 0123456789abcdef0123456789abcdef", got)
	}
}

// TestApplyRequest_ExtractTraceContext_Empty — forward-compat degradation:
// an empty trace_context (an old Keeper without the field) → Extract is a
// no-op, no panic, apply.run remains the root of its own trace.
func TestApplyRequest_ExtractTraceContext_Empty(t *testing.T) {
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(w3cPropagator())
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })

	req := &keeperv1.ApplyRequest{} // TraceContext == ""

	ctx := otel.GetTextMapPropagator().Extract(context.Background(),
		propagation.MapCarrier{"traceparent": req.GetTraceContext()})
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		t.Errorf("при пустом traceparent SpanContext должен быть невалиден, got %+v", sc)
	}
}
