package auditotel

import (
	"context"
	"math"
	"regexp"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// newTestWriter возвращает (writer, in-memory-recorder). Recorder
// захватывает закрытые span-ы для проверки attributes и timestamp-а.
func newTestWriter(t *testing.T) (audit.Writer, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return NewWriter(tp.Tracer("audit-test")), rec
}

func findAttr(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestOtelWriter_SmokeSpanShape(t *testing.T) {
	w, rec := newTestWriter(t)

	ts := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	ev := &audit.Event{
		AuditID:       "01HXYZABCDEFGHJKMNPQRSTVWX",
		EventType:     audit.EventConfigReloadSucceeded,
		Source:        audit.SourceSignal,
		ArchonAID:     "archon-alice",
		CorrelationID: "01HCOR0000000000000000000Z",
		Payload:       map[string]any{"path": "/etc/keeper.yml", "retries": 3, "ok": true},
		CreatedAt:     ts,
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "config.reload_succeeded" {
		t.Errorf("span name = %q, want %q", s.Name(), "config.reload_succeeded")
	}
	if !s.EndTime().Equal(ts) {
		t.Errorf("span EndTime = %v, want %v", s.EndTime(), ts)
	}

	attrs := s.Attributes()
	if v, ok := findAttr(attrs, "audit.id"); !ok || v.AsString() != "01HXYZABCDEFGHJKMNPQRSTVWX" {
		t.Errorf("audit.id = %v (ok=%v), want literal id", v.AsString(), ok)
	}
	if v, ok := findAttr(attrs, "audit.source"); !ok || v.AsString() != "signal" {
		t.Errorf("audit.source = %v, want signal", v.AsString())
	}
	if v, ok := findAttr(attrs, "audit.archon_aid"); !ok || v.AsString() != "archon-alice" {
		t.Errorf("audit.archon_aid = %v, want archon-alice", v.AsString())
	}
	if v, ok := findAttr(attrs, "audit.correlation_id"); !ok || v.AsString() != "01HCOR0000000000000000000Z" {
		t.Errorf("audit.correlation_id = %v", v.AsString())
	}
	if v, ok := findAttr(attrs, "audit.payload.path"); !ok || v.AsString() != "/etc/keeper.yml" {
		t.Errorf("audit.payload.path = %v", v.AsString())
	}
	if v, ok := findAttr(attrs, "audit.payload.retries"); !ok || v.AsInt64() != 3 {
		t.Errorf("audit.payload.retries = %v, want 3", v.AsInt64())
	}
	if v, ok := findAttr(attrs, "audit.payload.ok"); !ok || !v.AsBool() {
		t.Errorf("audit.payload.ok = %v, want true", v.AsBool())
	}
}

func TestOtelWriter_MasksSecrets(t *testing.T) {
	w, rec := newTestWriter(t)

	ev := &audit.Event{
		EventType: audit.EventConfigReloadSucceeded,
		Source:    audit.SourceAPI,
		Payload: map[string]any{
			"password":    "p@ssw0rd",
			"path":        "/etc/keeper.yml",
			"signing_key": "abcd",
			"vault_ref":   "vault:secret/keeper#token",
		},
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	attrs := spans[0].Attributes()

	const masked = "***MASKED***"
	if v, ok := findAttr(attrs, "audit.payload.password"); !ok || v.AsString() != masked {
		t.Errorf("password = %v, want masked", v.AsString())
	}
	if v, ok := findAttr(attrs, "audit.payload.signing_key"); !ok || v.AsString() != masked {
		t.Errorf("signing_key = %v, want masked", v.AsString())
	}
	if v, ok := findAttr(attrs, "audit.payload.vault_ref"); !ok || v.AsString() != masked {
		t.Errorf("vault_ref = %v, want masked (vault:-prefix)", v.AsString())
	}
	if v, ok := findAttr(attrs, "audit.payload.path"); !ok || v.AsString() != "/etc/keeper.yml" {
		t.Errorf("path = %v, want passthrough", v.AsString())
	}
}

func TestOtelWriter_AutoFillsAuditIDAndEndTime(t *testing.T) {
	w, rec := newTestWriter(t)

	before := time.Now().UTC()
	ev := &audit.Event{
		EventType: audit.EventConfigReloadFailed,
		Source:    audit.SourceSignal,
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}
	after := time.Now().UTC()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	attrs := spans[0].Attributes()
	v, ok := findAttr(attrs, "audit.id")
	if !ok || !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(v.AsString()) {
		t.Errorf("auto audit.id = %v, want 26-char ULID", v.AsString())
	}
	end := spans[0].EndTime()
	if end.Before(before) || end.After(after.Add(time.Second)) {
		t.Errorf("span EndTime %v out of [%v, %v]", end, before, after)
	}
}

func TestOtelWriter_NilEventReturnsNilNoSpan(t *testing.T) {
	w, rec := newTestWriter(t)
	if err := w.Write(context.Background(), nil); err != nil {
		t.Fatalf("Write(nil) returned %v, want nil", err)
	}
	if got := len(rec.Ended()); got != 0 {
		t.Errorf("ended spans = %d, want 0", got)
	}
}

// TestOtelWriter_EmptyEventTypeSkipped — обсервация obs-7 qa.C.
// Пустой EventType непригоден как span name; writer должен лог-варнить
// и возвращать nil без создания span-а.
func TestOtelWriter_EmptyEventTypeSkipped(t *testing.T) {
	w, rec := newTestWriter(t)
	ev := &audit.Event{
		EventType: "",
		Source:    audit.SourceSignal,
		Payload:   map[string]any{"x": 1},
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := len(rec.Ended()); got != 0 {
		t.Errorf("ended spans = %d, want 0 for empty EventType", got)
	}
}

// TestOtelWriter_NilPayloadValue_AttrOmitted — review.C M-5.
// Значение nil в payload должно приводить к опущенному атрибуту, не к
// пустой строке. Это типовой OTel-pattern и снимает неоднозначность
// «не задано» vs «явно пустая строка».
func TestOtelWriter_NilPayloadValue_AttrOmitted(t *testing.T) {
	w, rec := newTestWriter(t)
	ev := &audit.Event{
		EventType: audit.EventConfigReloadSucceeded,
		Source:    audit.SourceSignal,
		Payload: map[string]any{
			"present": "value",
			"absent":  nil,
		},
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	attrs := spans[0].Attributes()
	if _, ok := findAttr(attrs, "audit.payload.absent"); ok {
		t.Errorf("audit.payload.absent should be omitted for nil value, but present")
	}
	if v, ok := findAttr(attrs, "audit.payload.present"); !ok || v.AsString() != "value" {
		t.Errorf("audit.payload.present = %v (ok=%v), want 'value'", v.AsString(), ok)
	}
}

// TestOtelWriter_IntegerWidths_InPayloadAttribute — review.C M-4.
// Проверяет, что все integer-ширины (int8/int16/int32/int64/uint/uint8/
// uint16/uint32/uint64) попадают как Int64-attribute, кроме uint64 >
// math.MaxInt64 (там — String fallback).
func TestOtelWriter_IntegerWidths_InPayloadAttribute(t *testing.T) {
	w, rec := newTestWriter(t)
	ev := &audit.Event{
		EventType: audit.EventConfigReloadSucceeded,
		Source:    audit.SourceSignal,
		Payload: map[string]any{
			"i":     int(1),
			"i8":    int8(2),
			"i16":   int16(3),
			"i32":   int32(4),
			"i64":   int64(5),
			"u":     uint(6),
			"u8":    uint8(7),
			"u16":   uint16(8),
			"u32":   uint32(9),
			"u64":   uint64(10),
			"u64hi": uint64(math.MaxInt64) + 1,
		},
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	attrs := spans[0].Attributes()

	expectInt := map[string]int64{
		"audit.payload.i":   1,
		"audit.payload.i8":  2,
		"audit.payload.i16": 3,
		"audit.payload.i32": 4,
		"audit.payload.i64": 5,
		"audit.payload.u":   6,
		"audit.payload.u8":  7,
		"audit.payload.u16": 8,
		"audit.payload.u32": 9,
		"audit.payload.u64": 10,
	}
	for k, want := range expectInt {
		v, ok := findAttr(attrs, k)
		if !ok {
			t.Errorf("%s missing", k)
			continue
		}
		if v.Type() != attribute.INT64 {
			t.Errorf("%s type = %v, want INT64", k, v.Type())
			continue
		}
		if got := v.AsInt64(); got != want {
			t.Errorf("%s = %d, want %d", k, got, want)
		}
	}

	// uint64 > MaxInt64 → string-fallback (потеря точности int64
	// неприемлема).
	if v, ok := findAttr(attrs, "audit.payload.u64hi"); !ok || v.Type() != attribute.STRING {
		t.Errorf("audit.payload.u64hi type = %v (ok=%v), want STRING fallback", v.Type(), ok)
	}
}
