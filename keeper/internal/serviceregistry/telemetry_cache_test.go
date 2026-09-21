package serviceregistry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// fakeTelemetryLister — a programmable TelemetryLister: counts calls, returns a
// given catalog or error, optionally with a delay to test the per-key lock.
type fakeTelemetryLister struct {
	calls   atomic.Int64
	catalog *TelemetryCatalog
	err     error
	delay   time.Duration
}

func (f *fakeTelemetryLister) ListServiceTelemetry(_ context.Context, _, _, _ string) (*TelemetryCatalog, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.catalog, nil
}

func sampleTelemetryCatalog() *TelemetryCatalog {
	return &TelemetryCatalog{
		SHA1: "cafef00d",
		Telemetry: &keeperv1.TelemetryConfig{
			Enabled:     true,
			IntervalSec: 30,
			Collectors:  []string{"cpu", "mem"},
		},
	}
}

func TestTelemetryCache_HitMiss(t *testing.T) {
	lister := &fakeTelemetryLister{catalog: sampleTelemetryCatalog()}
	c := NewTelemetryCache(lister, time.Hour)

	got, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("#1: %v", err)
	}
	if got == nil || got.SHA1 != "cafef00d" || got.Telemetry.GetIntervalSec() != 30 || len(got.Telemetry.GetCollectors()) != 2 {
		t.Fatalf("catalog = %+v", got)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls after miss = %d, want 1", n)
	}

	// Hit: same key — loader is not invoked.
	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls after hit = %d, want 1 (cache did not work)", n)
	}
}

func TestTelemetryCache_ReturnedCatalogIsClone(t *testing.T) {
	// Mutating the catalog returned to the caller doesn't corrupt the cached one
	// (the proto-message + the Collectors slice are cloned on read).
	lister := &fakeTelemetryLister{catalog: sampleTelemetryCatalog()}
	c := NewTelemetryCache(lister, time.Hour)

	first, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("#1: %v", err)
	}
	first.Telemetry.Collectors = nil
	first.Telemetry.IntervalSec = 999

	second, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("#2: %v", err)
	}
	if len(second.Telemetry.GetCollectors()) != 2 || second.Telemetry.GetIntervalSec() != 30 {
		t.Errorf("cache corrupted by caller mutation: %+v", second.Telemetry)
	}
}

func TestTelemetryCache_KeyByNameAndRef(t *testing.T) {
	lister := &fakeTelemetryLister{catalog: sampleTelemetryCatalog()}
	c := NewTelemetryCache(lister, time.Hour)

	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#v1: %v", err)
	}
	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v2"); err != nil {
		t.Fatalf("#v2: %v", err)
	}
	// Same name, different refs → two independent entries.
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (per-(name,ref) keys)", n)
	}
}

func TestTelemetryCache_Expiry(t *testing.T) {
	lister := &fakeTelemetryLister{catalog: sampleTelemetryCatalog()}
	c := NewTelemetryCache(lister, 100*time.Millisecond)

	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#1: %v", err)
	}
	c.now = func() time.Time { return time.Now().Add(200 * time.Millisecond) }
	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls after TTL = %d, want 2", n)
	}
}

func TestTelemetryCache_Invalidate_DropsAllRefs(t *testing.T) {
	lister := &fakeTelemetryLister{catalog: sampleTelemetryCatalog()}
	c := NewTelemetryCache(lister, time.Hour)

	// Warm up three keys: redis@v1, redis@v2, mongo@v1.
	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("warm redis@v1: %v", err)
	}
	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v2"); err != nil {
		t.Fatalf("warm redis@v2: %v", err)
	}
	if _, err := c.ListServiceTelemetry(context.Background(), "mongo", "g", "v1"); err != nil {
		t.Fatalf("warm mongo@v1: %v", err)
	}
	preInvalidate := lister.calls.Load()

	c.Invalidate("redis")

	// Both refs for redis are evicted, mongo stays in the cache.
	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("post-inv redis@v1: %v", err)
	}
	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v2"); err != nil {
		t.Fatalf("post-inv redis@v2: %v", err)
	}
	if _, err := c.ListServiceTelemetry(context.Background(), "mongo", "g", "v1"); err != nil {
		t.Fatalf("post-inv mongo@v1: %v", err)
	}
	// redis@v1 + redis@v2 recomputed (2), mongo@v1 — from cache (0).
	if n := lister.calls.Load() - preInvalidate; n != 2 {
		t.Errorf("calls after Invalidate(\"redis\") = %d, want 2 (mongo should remain cached)", n)
	}
}

func TestTelemetryCache_ErrorNotCached(t *testing.T) {
	listerErr := errors.New("loader busted")
	lister := &fakeTelemetryLister{err: listerErr}
	c := NewTelemetryCache(lister, time.Hour)

	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.ListServiceTelemetry(context.Background(), "redis", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("#2 err = %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (errors are not cached)", n)
	}
}

func TestTelemetryCache_PerKeyLock(t *testing.T) {
	lister := &fakeTelemetryLister{catalog: sampleTelemetryCatalog(), delay: 30 * time.Millisecond}
	c := NewTelemetryCache(lister, time.Hour)

	const N = 5
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.ListServiceTelemetry(context.Background(), "redis", "g", "v1")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1 (per-key lock did not work)", n)
	}
}

func TestTelemetryCache_NilLister_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected a panic on nil lister")
		}
	}()
	_ = NewTelemetryCache(nil, time.Hour)
}

func TestTelemetryCache_ListerFunc_Implements(t *testing.T) {
	var _ TelemetryLister = TelemetryListerFunc(func(_ context.Context, _, _, _ string) (*TelemetryCatalog, error) {
		return nil, nil
	})
}
