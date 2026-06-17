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

// w3cPropagator — тот же composite TraceContext, что ставит obs.SetupOTel.
// Тесты propagation ставят его явно (глобальный propagator у no-op-провайдера
// по умолчанию ничего не инжектит).
func w3cPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

// TestSendApply_InjectsTraceContext проверяет, что при активном span-е в ctx
// SendApply кладёт непустой W3C traceparent в req.TraceContext (ADR-024
// cross-process trace-propagation). Soul извлечёт его и поднимет apply.run как
// child grpc.apply_dispatch.
func TestSendApply_InjectsTraceContext(t *testing.T) {
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(w3cPropagator())
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })

	// Активный валидный родительский SpanContext в ctx — достаточно для
	// инжекта traceparent независимо от TracerProvider-а (Inject читает
	// SpanContext из ctx). grpc.apply_dispatch внутри SendApply наследует его.
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, parent := tp.Tracer("test").Start(context.Background(), "parent")
	defer parent.End()

	mgr := NewStreamManager(discardLogger(t))
	ob := newOutboundForTest(t, mgr, nopAudit{})

	// Soul не подключён → deliver вернёт ErrSoulNotConnected, но инжект делается
	// ДО deliver — req.TraceContext должен быть заполнен в любом случае.
	req := &keeperv1.ApplyRequest{ApplyId: "01HAPPLY00000000000000000Z"}
	if err := ob.SendApply(ctx, "host.example.test", req); !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("SendApply err = %v, want ErrSoulNotConnected", err)
	}

	if req.GetTraceContext() == "" {
		t.Fatal("req.TraceContext пуст, ожидался непустой W3C traceparent")
	}

	// Round-trip: Extract из инжектнутого traceparent даёт валидный remote
	// SpanContext c тем же trace-id, что у родителя (= сквозная трасса).
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

// TestApplyRequest_ExtractTraceContext_RoundTrip моделирует Soul-side извлечение:
// непустой traceparent → ctx с валидным remote SpanContext (apply.run станет
// child). Дублирует логику строки soul/cmd/soul/main.go без подъёма Soul-демона.
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

// TestApplyRequest_ExtractTraceContext_Empty — forward-compat деградация: пустой
// trace_context (старый Keeper без поля) → Extract noop, без паники, apply.run
// останется корнем собственной трассы.
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
