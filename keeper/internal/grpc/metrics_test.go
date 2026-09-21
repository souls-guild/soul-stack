package grpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterGRPCMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterGRPCMetrics(reg)
	if m == nil {
		t.Fatal("RegisterGRPCMetrics returned nil")
	}

	// streams_active is a gauge, visible right away at value 0. A
	// CounterVec doesn't publish without a first WithLabelValues sample —
	// we'll check those after Observe.
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_grpc_streams_active 0") {
		t.Errorf("missing streams_active=0 sample; got=\n%s", body)
	}

	m.ObserveMessage(directionFromSoul)
	m.ObserveApplyDispatch(nil)
	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	m.ObserveBootstrap(nil)
	m.ObserveRunResultStale()
	families, err = reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_grpc_streams_active",
		"keeper_grpc_messages_total",
		"keeper_grpc_apply_dispatch_total",
		"keeper_grpc_bootstrap_total",
		"keeper_runresult_stale_total",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterGRPCMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterGRPCMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterGRPCMetrics(reg)
}

func TestGRPCMetrics_StreamsActive_IncDec(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterGRPCMetrics(reg)

	m.IncStreams()
	m.IncStreams()
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "keeper_grpc_streams_active 2") {
		t.Errorf("streams_active should be 2 after two Inc; got=\n%s", body)
	}
	m.DecStreams()
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "keeper_grpc_streams_active 1") {
		t.Errorf("streams_active should be 1 after one Dec; got=\n%s", body)
	}
}

func TestGRPCMetrics_MessagesByDirection(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterGRPCMetrics(reg)

	m.ObserveMessage(directionFromSoul)
	m.ObserveMessage(directionFromSoul)
	m.ObserveMessage(directionToSoul)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_grpc_messages_total{direction="from_soul"} 2`) {
		t.Errorf("from_soul count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_grpc_messages_total{direction="to_soul"} 1`) {
		t.Errorf("to_soul count mismatch; got=\n%s", body)
	}
}

func TestGRPCMetrics_ApplyDispatchByResult(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterGRPCMetrics(reg)

	m.ObserveApplyDispatch(nil)
	m.ObserveApplyDispatch(nil)
	m.ObserveApplyDispatch(errors.New("soul not connected"))

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_grpc_apply_dispatch_total{result="ok"} 2`) {
		t.Errorf("ok count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_grpc_apply_dispatch_total{result="failed"} 1`) {
		t.Errorf("failed count mismatch; got=\n%s", body)
	}
}

func TestGRPCMetrics_BootstrapByResult(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterGRPCMetrics(reg)

	m.ObserveBootstrap(nil)
	m.ObserveBootstrap(nil)
	m.ObserveBootstrap(errors.New("token rejected"))

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_grpc_bootstrap_total{result="ok"} 2`) {
		t.Errorf("ok count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_grpc_bootstrap_total{result="failed"} 1`) {
		t.Errorf("failed count mismatch; got=\n%s", body)
	}
}

func TestGRPCMetrics_NilReceiver_NoOp(t *testing.T) {
	// EventStream/Outbound/Bootstrap can come up without the obs stack
	// (unit tests, dev build). All methods on a nil receiver are a no-op,
	// no panic.
	var m *GRPCMetrics
	m.IncStreams()
	m.DecStreams()
	m.ObserveMessage(directionFromSoul)
	m.ObserveApplyDispatch(nil)
	m.ObserveApplyDispatch(errors.New("x"))
	m.ObserveBootstrap(nil)
	m.ObserveBootstrap(errors.New("x"))
	m.ObserveRunResultStale()
}

// bootstrapSpanProbe — helper check for the grpc.bootstrap span, called
// from the shared span test in the package (see below). Bootstrap and
// dispatch share one package-level tracer "keeper/grpc"; the otel-global
// delegate binds to the first provider set (internal/global), so multiple
// tests each with their own SetTracerProvider on the same tracer name are
// incompatible — both spans are checked under the SAME recorder.
func bootstrapSpanProbe(t *testing.T, rec *tracetest.SpanRecorder) {
	t.Helper()
	reg := obs.NewRegistry()
	metrics := RegisterGRPCMetrics(reg)
	h := newBootstrapHandler(BootstrapDeps{
		Pool: fakeTxBeginner{}, VaultClient: fakeSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed", Metrics: metrics,
	}, discardLogger(t))

	before := len(rec.Ended())
	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "BAD_SID",
		BootstrapToken: "tok",
		CsrPem:         []byte("x"),
	})
	if err == nil {
		t.Fatal("expected error on invalid SID")
	}

	spans := rec.Ended()
	if len(spans) != before+1 {
		t.Fatalf("expected 1 new span from bootstrap, got %d", len(spans)-before)
	}
	span := spans[len(spans)-1]
	if span.Name() != "grpc.bootstrap" {
		t.Errorf("span name = %q, want grpc.bootstrap", span.Name())
	}
	attrs := map[string]string{}
	for _, a := range span.Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["sid"] != "BAD_SID" {
		t.Errorf("span attr sid = %q", attrs["sid"])
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_grpc_bootstrap_total{result="failed"} 1`) {
		t.Errorf("expected one failed bootstrap; got=\n%s", body)
	}
}

// TestBootstrap_NoTracer_NoPanic — with a no-op global provider (OTel
// disabled), Bootstrap doesn't crash and the span is free; nil Metrics is
// a no-op.
func TestBootstrap_NoTracer_NoPanic(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool: fakeTxBeginner{}, VaultClient: fakeSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed", // Metrics nil
	}, discardLogger(t))
	if _, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid: "BAD_SID", BootstrapToken: "tok", CsrPem: []byte("x"),
	}); err == nil {
		t.Fatal("expected error on invalid SID")
	}
}

// TestSendApply_DispatchSpan_Created checks that dispatch produces a span
// with sid/apply_id attributes when the provider is enabled. The
// SpanRecorder is set as the global TracerProvider — the package-level
// `tracer` (otel.Tracer) delegates to it lazily.
func TestSendApply_DispatchSpan_Created(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	mgr := NewStreamManager(discardLogger(t))
	out, err := NewOutbound(OutboundDeps{
		Manager:     mgr,
		AuditWriter: &captureAudit{},
		Logger:      discardLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}

	// The Soul is not connected — deliver will return ErrSoulNotConnected.
	// The span must still be created either way (it wraps the dispatch
	// attempt itself).
	req := &keeperv1.ApplyRequest{ApplyId: "01HAPPLY00000000000000000Z"}
	derr := out.SendApply(context.Background(), "host.example.test", req)
	if !errors.Is(derr, ErrSoulNotConnected) {
		t.Fatalf("expected ErrSoulNotConnected, got %v", derr)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name() != "grpc.apply_dispatch" {
		t.Errorf("span name = %q, want grpc.apply_dispatch", span.Name())
	}
	attrs := map[string]string{}
	for _, a := range span.Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["sid"] != "host.example.test" {
		t.Errorf("span attr sid = %q", attrs["sid"])
	}
	if attrs["apply_id"] != "01HAPPLY00000000000000000Z" {
		t.Errorf("span attr apply_id = %q", attrs["apply_id"])
	}

	// grpc.bootstrap shares the same package-level tracer "keeper/grpc";
	// the otel-global delegate binds to the first provider set, so the
	// Bootstrap RPC span is checked right here, under this same recorder
	// (a separate test with its own SetTracerProvider would poison the
	// delegate — see bootstrapSpanProbe).
	bootstrapSpanProbe(t, rec)
}

// TestSendApply_NoTracer_NoPanic — with no explicit provider (a no-op
// global one), dispatch doesn't crash and the span is free. Simulates OTel
// disabled.
func TestSendApply_NoTracer_NoPanic(t *testing.T) {
	mgr := NewStreamManager(discardLogger(t))
	out, err := NewOutbound(OutboundDeps{
		Manager:     mgr,
		AuditWriter: &captureAudit{},
		Logger:      discardLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	req := &keeperv1.ApplyRequest{ApplyId: "01HAPPLY00000000000000000Z"}
	if derr := out.SendApply(context.Background(), "host.example.test", req); !errors.Is(derr, ErrSoulNotConnected) {
		t.Fatalf("expected ErrSoulNotConnected, got %v", derr)
	}
}
