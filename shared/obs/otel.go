package obs

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// OTelConfig holds the OTel provider bootstrap parameters. Component-agnostic:
// keeper and soul pass their own ServiceName ("keeper" / "soul") and per-instance
// domain identity via ResourceAttrs (keeper — soulstack.kid, soul — soulstack.sid;
// ADR-024 §3). The provider itself knows nothing about these keys.
type OTelConfig struct {
	// Enabled turns on export. When false, [SetupOTel] returns a no-op provider
	// (not nil): main does not branch, the defer chain stays uniform.
	Enabled bool

	// Endpoint is the OTLP collector address (host:port). Used only when Enabled;
	// an empty Endpoint skips the exporter even with Enabled (traces go to a no-op
	// span processor).
	Endpoint string

	// ServiceName is the standard OTel semconv attribute service.name
	// ("keeper" | "soul" per the Soul Stack vocabulary).
	ServiceName string

	// ResourceAttrs holds custom resource attributes of the source
	// (soulstack.kid / soulstack.sid, etc.). Layered over service.name; an empty
	// map is allowed.
	ResourceAttrs map[string]string
}

// OTelProvider is a handle to the running OTel stack. Holds the TracerProvider and
// (optionally) the OTLP exporter for a correct flush+close on shutdown.
//
// In Slice 0 there is only the TracerProvider; no instrumentation (spans) yet
// (Slice 2). The provider is registered globally ([otel.SetTracerProvider] +
// propagator) so future instrumentation picks it up without explicit passing.
type OTelProvider struct {
	tp *sdktrace.TracerProvider
}

// SetupOTel brings up the OTel provider from config. Always returns a non-nil
// [OTelProvider] (even with Enabled=false) so main can defer Shutdown without a
// nil check.
//
// Call ONCE per process. SetupOTel sets the global [otel.SetTracerProvider] +
// propagator — a second call without [Shutdown] of the previous provider leaks
// (the old TracerProvider with its batch-export pipeline stays alive but
// unreachable). The otel.* config block is restart-required: hot-reload does NOT
// re-read it (changing endpoint/enabled live means recreating the provider with the
// risk of losing in-flight spans).
//
// Resource is built from semconv.ServiceName(cfg.ServiceName) + custom
// ResourceAttrs, over [resource.Default] (host/sdk attributes). Sampler is
// ParentBased(AlwaysSample): root spans are always sampled, children inherit the
// parent's decision (a sampler config field is deferred).
//
// With Enabled+Endpoint an OTLP-gRPC batch exporter is started; otherwise the
// TracerProvider runs without an exporter (spans go nowhere but the API still
// works — handy for dev without a collector).
func SetupOTel(ctx context.Context, cfg OTelConfig) (*OTelProvider, error) {
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	}

	if cfg.Enabled && cfg.Endpoint != "" {
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
			// Insecure: the dev collector runs locally via docker-compose without
			// TLS (project_local_dev_docker). TLS to the collector is a separate task
			// once a prod install appears.
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, fmt.Errorf("obs: build OTLP trace-exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagator())

	return &OTelProvider{tp: tp}, nil
}

// Shutdown flushes pending spans and closes the exporter. Safe on any returned
// provider (including Enabled=false — the no-op TracerProvider just completes).
// Deferred into main's chain with a timeout (Slice 1).
func (p *OTelProvider) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	if err := p.tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("obs: shutdown OTel tracer-provider: %w", err)
	}
	return nil
}

// buildResource assembles the OTel resource from service.name + custom
// ResourceAttrs (soulstack.kid / soulstack.sid), over resource.Default()
// (host/process/sdk attributes). Split out of SetupOTel to test the resource layer
// directly without starting a TracerProvider.
//
// resource.New without WithSchemaURL: our layer stays schema-less so the merge with
// resource.Default() (which carries the SDK's semconv schema URL) does not fail on a
// conflicting Schema URL.
func buildResource(ctx context.Context, cfg OTelConfig) (*resource.Resource, error) {
	attrs := make([]attribute.KeyValue, 0, len(cfg.ResourceAttrs)+1)
	attrs = append(attrs, semconv.ServiceName(cfg.ServiceName))
	for k, v := range cfg.ResourceAttrs {
		attrs = append(attrs, attribute.String(k, v))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, fmt.Errorf("obs: build OTel resource: %w", err)
	}
	merged, err := resource.Merge(resource.Default(), res)
	if err != nil {
		return nil, fmt.Errorf("obs: merge OTel resource: %w", err)
	}
	return merged, nil
}

// propagator — W3C TraceContext + Baggage. TraceContext carries trace-id / span-id
// through the EventStream gRPC metadata (end-to-end trace operator → Keeper → Soul,
// ADR-024 §1.2); Baggage carries domain context. The global propagator is set in
// [SetupOTel] so instrumentation (Slice 2) can inject/extract without explicit
// passing.
func propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}
