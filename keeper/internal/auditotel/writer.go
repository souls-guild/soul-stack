// Package auditotel is an OTel implementation of [audit.Writer] that
// publishes the event as a standalone span.
//
// Used by the multi-writer ([keeper/internal/auditmulti]) as the
// **secondary** writer in the dual-write pipeline: PG is the source of
// truth (sync, required), OTel is transient debugging (async,
// best-effort). That's why Write always returns nil — exporter errors are
// handled by the OTel SDK (BatchSpanProcessor) asynchronously, they don't
// block the write path or affect audit_log consistency.
//
// The span is created standalone (no parent) — an audit event records the
// fact "something happened", it's not part of the current request's
// distributed trace. A trace in an OTel viewer will show one span with
// ~0 duration and a set of attributes. This is a deliberate trade-off
// against ad-hoc queries over `audit_log` (Grafana → Tempo / Jaeger / any
// other OTel backend).
//
// ADR-022(f) fixes the dual-write multi-writer; this package is the
// secondary half (the Postgres half is `keeper/internal/auditpg`).
// Exporter and TracerProvider lifecycle live in `cmd/keeper` (M0.4.2); the
// tracer is passed into [NewWriter] already configured.
package auditotel

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// otelWriter is an [audit.Writer] implementation on top of [trace.Tracer].
// One instance per Keeper process; safe for concurrent use — the tracer
// is thread-safe per the OTel contract.
type otelWriter struct {
	tracer trace.Tracer
}

// NewWriter wraps a [trace.Tracer] into an [audit.Writer]. TracerProvider
// ownership stays with the caller: the writer doesn't shut down the
// provider — lifecycle is `cmd/keeper`.
func NewWriter(tracer trace.Tracer) audit.Writer {
	return &otelWriter{tracer: tracer}
}

// Write publishes event as a standalone span. Contract:
//
//   - event == nil → no-op, returns nil without a span.
//   - event.EventType == "" → early return with slog.Warn (creating a
//     span with an empty name is pointless — it's unusable in the OTel
//     UI).
//   - event.AuditID empty → generated via [audit.NewULID] (symmetric with
//     pgxWriter; audit_id matches between PG and OTel when used inside
//     the multi-writer).
//   - Span name = string(event.EventType) (`<area>.<action>`).
//   - Attributes follow the `audit.*` schema: source, correlation_id,
//     archon_aid, id; payload is flattened via [audit.MaskSecrets] into
//     `audit.payload.<key>` (top-level only — nested maps/slices are
//     stringified via `fmt.Sprintf("%v", …)`).
//   - End the span with an explicit timestamp = event.CreatedAt (now if
//     zero).
//   - Returns nil **always** — a secondary writer in dual-write must not
//     block the primary; export errors are async via BatchSpanProcessor,
//     visible through OTel's internal log handler.
func (w *otelWriter) Write(_ context.Context, event *audit.Event) error {
	if event == nil {
		return nil
	}
	if event.EventType == "" {
		slog.Warn(
			"audit otel writer: empty EventType, skipping span",
			slog.String("audit_id", event.AuditID),
			slog.String("source", string(event.Source)),
		)
		return nil
	}

	auditID := event.AuditID
	if auditID == "" {
		auditID = audit.NewULID()
	}

	endTime := event.CreatedAt
	if endTime.IsZero() {
		endTime = time.Now().UTC()
	}

	// context.Background — the span is standalone, not tied to a request.
	_, span := w.tracer.Start(context.Background(), string(event.EventType))

	attrs := make([]attribute.KeyValue, 0, 4+len(event.Payload))
	attrs = append(attrs,
		attribute.String("audit.id", auditID),
		attribute.String("audit.source", string(event.Source)),
	)
	if event.CorrelationID != "" {
		attrs = append(attrs, attribute.String("audit.correlation_id", event.CorrelationID))
	}
	if event.ArchonAID != "" {
		attrs = append(attrs, attribute.String("audit.archon_aid", event.ArchonAID))
	}

	masked := audit.MaskSecrets(event.Payload)
	for k, v := range masked {
		if kv, ok := payloadAttribute(k, v); ok {
			attrs = append(attrs, kv)
		}
	}

	span.SetAttributes(attrs...)
	span.End(trace.WithTimestamp(endTime))
	return nil
}

// payloadAttribute builds an OTel attribute from a top-level payload key.
// Scalar value types are preserved (string/bool/intN/uintN/floatN);
// complex types (map / slice) are serialized to a string via
// `fmt.Sprintf("%v", …)` to avoid crashing the exporter on an unsupported
// attribute value. payload in OTel is a debugging aid — the normative
// data channel is the JSONB column in PG.
//
// The second return value flags whether to emit the attribute at all: for
// a `nil` value it returns `(_, false)` so the caller skips it in
// SetAttributes (a typical OTel pattern — absence beats an empty string,
// which could be confused with a deliberately set empty value).
//
// uint64 > math.MaxInt64 is serialized to a string: an OTel int attribute
// is int64, there's no painless downcast.
func payloadAttribute(key string, value any) (attribute.KeyValue, bool) {
	akey := "audit.payload." + key
	switch x := value.(type) {
	case nil:
		return attribute.KeyValue{}, false
	case string:
		return attribute.String(akey, x), true
	case bool:
		return attribute.Bool(akey, x), true
	case int:
		return attribute.Int64(akey, int64(x)), true
	case int8:
		return attribute.Int64(akey, int64(x)), true
	case int16:
		return attribute.Int64(akey, int64(x)), true
	case int32:
		return attribute.Int64(akey, int64(x)), true
	case int64:
		return attribute.Int64(akey, x), true
	case uint:
		if uint64(x) > math.MaxInt64 {
			return attribute.String(akey, fmt.Sprintf("%d", x)), true
		}
		return attribute.Int64(akey, int64(x)), true
	case uint8:
		return attribute.Int64(akey, int64(x)), true
	case uint16:
		return attribute.Int64(akey, int64(x)), true
	case uint32:
		return attribute.Int64(akey, int64(x)), true
	case uint64:
		if x > math.MaxInt64 {
			return attribute.String(akey, fmt.Sprintf("%d", x)), true
		}
		return attribute.Int64(akey, int64(x)), true
	case float32:
		return attribute.Float64(akey, float64(x)), true
	case float64:
		return attribute.Float64(akey, x), true
	default:
		return attribute.String(akey, fmt.Sprintf("%v", x)), true
	}
}
