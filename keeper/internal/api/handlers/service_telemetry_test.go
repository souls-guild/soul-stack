package handlers

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeSvcTelemetryLister — stub ServiceTelemetryLister that returns a fixed
// catalog or a git-loader error.
type fakeSvcTelemetryLister struct {
	catalog *serviceregistry.TelemetryCatalog
	err     error
}

func (f fakeSvcTelemetryLister) ListServiceTelemetry(context.Context, string, string, string) (*serviceregistry.TelemetryCatalog, error) {
	return f.catalog, f.err
}

func telCatalog(sha string, enabled bool, interval int32, collectors []string) *serviceregistry.TelemetryCatalog {
	return &serviceregistry.TelemetryCatalog{
		SHA1:      sha,
		Telemetry: &keeperv1.TelemetryConfig{Enabled: enabled, IntervalSec: interval, Collectors: collectors},
	}
}

// TestServiceTelemetry_Defaults — happy path: default config + ref from the registry +
// full known_collectors (== config.KnownCollectors).
func TestServiceTelemetry_Defaults(t *testing.T) {
	lister := fakeSvcTelemetryLister{catalog: telCatalog("sha-tel", true, 30, []string{"cpu", "mem", "disk", "load", "uptime"})}
	h := newServiceHandlerWithTelemetry(t, &svcFakePool{getValues: serviceRow("redis", "g", "v1")}, lister)

	reply, err := h.ListServiceTelemetryTyped(context.Background(), "redis", "")
	if err != nil {
		t.Fatalf("ListServiceTelemetryTyped: %v", err)
	}
	if reply.Service != "redis" || reply.Ref != "v1" || reply.SHA1 != "sha-tel" {
		t.Errorf("meta = %+v", reply)
	}
	if !reply.Enabled || reply.IntervalSec != 30 || len(reply.Collectors) != 5 {
		t.Errorf("config = %+v", reply)
	}
	if len(reply.KnownCollectors) != len(config.KnownCollectors) {
		t.Fatalf("known_collectors = %v, want %v", reply.KnownCollectors, config.KnownCollectors)
	}
	for i, c := range config.KnownCollectors {
		if reply.KnownCollectors[i] != c {
			t.Errorf("known_collectors[%d] = %q, want %q", i, reply.KnownCollectors[i], c)
		}
	}
}

// TestServiceTelemetry_EmptyCollectors_NonNil — collectors empty → `[]` (not nil);
// known_collectors stays the full set.
func TestServiceTelemetry_EmptyCollectors_NonNil(t *testing.T) {
	lister := fakeSvcTelemetryLister{catalog: telCatalog("sha", false, 60, nil)}
	h := newServiceHandlerWithTelemetry(t, &svcFakePool{getValues: serviceRow("redis", "g", "v1")}, lister)

	reply, err := h.ListServiceTelemetryTyped(context.Background(), "redis", "")
	if err != nil {
		t.Fatalf("ListServiceTelemetryTyped: %v", err)
	}
	if reply.Collectors == nil {
		t.Errorf("collectors = nil, want [] (non-nil)")
	}
	if len(reply.Collectors) != 0 {
		t.Errorf("collectors = %v, want empty", reply.Collectors)
	}
	if reply.Enabled {
		t.Errorf("enabled = true, want false (explicit enabled=false)")
	}
	if len(reply.KnownCollectors) != len(config.KnownCollectors) {
		t.Errorf("known_collectors = %v, want the full set", reply.KnownCollectors)
	}
}

// TestServiceTelemetry_RefOverride — ?ref override is forwarded into the reply.
func TestServiceTelemetry_RefOverride(t *testing.T) {
	lister := fakeSvcTelemetryLister{catalog: telCatalog("sha", true, 30, []string{"cpu"})}
	h := newServiceHandlerWithTelemetry(t, &svcFakePool{getValues: serviceRow("redis", "g", "v1")}, lister)

	reply, err := h.ListServiceTelemetryTyped(context.Background(), "redis", "main")
	if err != nil {
		t.Fatalf("ListServiceTelemetryTyped: %v", err)
	}
	if reply.Ref != "main" {
		t.Errorf("ref = %q, want main (override)", reply.Ref)
	}
}

// TestServiceTelemetry_NotFound_404 — service not in the registry → 404.
func TestServiceTelemetry_NotFound_404(t *testing.T) {
	lister := fakeSvcTelemetryLister{catalog: telCatalog("sha", true, 30, nil)}
	h := newServiceHandlerWithTelemetry(t, &svcFakePool{getValues: nil}, lister)
	_, err := h.ListServiceTelemetryTyped(context.Background(), "ghost", "")
	wantProblem(t, err, problem.TypeNotFound)
}

// TestServiceTelemetry_NilLister_500 — lister not configured → 500.
func TestServiceTelemetry_NilLister_500(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{getValues: serviceRow("redis", "g", "v1")})
	_, err := h.ListServiceTelemetryTyped(context.Background(), "redis", "")
	wantProblem(t, err, problem.TypeInternalError)
}

// TestServiceTelemetry_LoaderError_502 — git-loader error → 502.
func TestServiceTelemetry_LoaderError_502(t *testing.T) {
	lister := fakeSvcTelemetryLister{err: &svcErr{"git clone failed"}}
	h := newServiceHandlerWithTelemetry(t, &svcFakePool{getValues: serviceRow("redis", "g", "v1")}, lister)
	_, err := h.ListServiceTelemetryTyped(context.Background(), "redis", "")
	wantProblem(t, err, problem.TypeBadGateway)
}
