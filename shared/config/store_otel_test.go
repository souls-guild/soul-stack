package config

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// spanAttrs collects a span's string attributes into a map for checks.
func spanAttrs(span sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, a := range span.Attributes() {
		out[string(a.Key)] = a.Value.AsString()
	}
	return out
}

// findReloadSpan looks for a config.reload span with the given source+path among
// the recorded ones. The global TracerProvider is shared across the package's
// tests (background reload-loop WatchSIGHUP / concurrent tests may leave stray
// config.reload spans), so we select ours by source+path (path is unique per test
// — t.TempDir), not by position/count.
func findReloadSpan(t *testing.T, rec *tracetest.SpanRecorder, source ReloadSource, path string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range rec.Ended() {
		if span.Name() != "config.reload" {
			continue
		}
		attrs := spanAttrs(span)
		if attrs["source"] == string(source) && attrs["path"] == path {
			return span
		}
	}
	t.Fatalf("no config.reload span with source=%q path=%q among %d ended spans",
		source, path, len(rec.Ended()))
	return nil
}

// TestReload_Span checks hot-reload instrumentation with a single SpanRecorder.
// success and failure are subtests under a shared provider: the global delegating
// tracer (var tracer = otel.Tracer(...)) pins its delegate to the first real
// TracerProvider installed, so swapping the provider per subtest is pointless —
// we set one recorder and distinguish spans by path.
func TestReload_Span(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	// success: golden Reload → span config.reload with outcome=ok, status non-Error.
	t.Run("success_outcome_ok", func(t *testing.T) {
		path := fixtureKeeperPath(t)
		store, _, err := LoadKeeperStore(path, ValidateOptions{})
		if err != nil {
			t.Fatalf("LoadKeeperStore: %v", err)
		}

		res := store.Reload(context.Background(), ReloadSourceMCP)
		if !res.Swapped {
			t.Fatalf("Swapped=false on golden Reload, diags=%+v", res.Diagnostics)
		}

		span := findReloadSpan(t, rec, ReloadSourceMCP, path)
		attrs := spanAttrs(span)
		if attrs["outcome"] != "ok" {
			t.Errorf("span attr outcome = %q, want ok", attrs["outcome"])
		}
		if span.Status().Code == codes.Error {
			t.Errorf("span status = Error on success, want unset")
		}
	})

	// failure: broken YAML → span with status Error, outcome=failed, phase and a
	// recorded span-error (RecordError).
	t.Run("failure_status_error", func(t *testing.T) {
		path := fixtureKeeperPath(t)
		store, _, err := LoadKeeperStore(path, ValidateOptions{})
		if err != nil {
			t.Fatalf("LoadKeeperStore: %v", err)
		}

		bad := []byte("kid: [\n  unterminated\n")
		if err := os.WriteFile(path, bad, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		res := store.Reload(context.Background(), ReloadSourceSignal)
		if res.Swapped {
			t.Fatalf("Swapped=true on broken YAML")
		}

		span := findReloadSpan(t, rec, ReloadSourceSignal, path)
		attrs := spanAttrs(span)
		if attrs["outcome"] != "failed" {
			t.Errorf("span attr outcome = %q, want failed", attrs["outcome"])
		}
		if attrs["phase"] != string(res.Phase) {
			t.Errorf("span attr phase = %q, want %q", attrs["phase"], res.Phase)
		}
		if span.Status().Code != codes.Error {
			t.Errorf("span status = %v, want Error", span.Status().Code)
		}
		if len(span.Events()) == 0 {
			t.Errorf("span has no recorded error event on failure")
		}
	})
}

// TestReload_Span_NoTracer_NoPanic — with a no-op/unavailable provider Reload does
// not fail and the span is free. Simulates production without observability.
func TestReload_Span_NoTracer_NoPanic(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	if res := store.Reload(context.Background(), ReloadSourceAPI); !res.Swapped {
		t.Fatalf("Swapped=false on golden Reload without dedicated recorder")
	}
}
