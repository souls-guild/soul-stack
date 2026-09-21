package serviceregistry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// fakeDirectiveLister is a programmable DirectiveLister: counts calls, returns
// configured catalog or error, optionally with delay for testing per-key lock.
type fakeDirectiveLister struct {
	calls   atomic.Int64
	catalog *artifact.DirectiveCatalog
	err     error
	delay   time.Duration
}

func (f *fakeDirectiveLister) ListDirectives(_ context.Context, _, _, _ string) (*artifact.DirectiveCatalog, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.catalog, nil
}

func sampleCatalog() *artifact.DirectiveCatalog {
	return &artifact.DirectiveCatalog{
		SHA1:       "cafef00d",
		Directives: map[string][]string{"8.2": {"appendonly", "maxmemory"}},
	}
}

func TestDirectivesCache_HitMiss(t *testing.T) {
	lister := &fakeDirectiveLister{catalog: sampleCatalog()}
	c := NewDirectivesCache(lister, time.Hour)

	got, err := c.ListDirectives(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("#1: %v", err)
	}
	if got == nil || got.SHA1 != "cafef00d" || len(got.Directives["8.2"]) != 2 {
		t.Fatalf("catalog = %+v", got)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls after miss = %d, want 1", n)
	}

	// Hit: same key—loader not called.
	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls after hit = %d, want 1 (cache did not work)", n)
	}
}

func TestDirectivesCache_ReturnedCatalogIsClone(t *testing.T) {
	// Mutation of caller-returned catalog does not corrupt cached (external map
	// is cloned on read).
	lister := &fakeDirectiveLister{catalog: sampleCatalog()}
	c := NewDirectivesCache(lister, time.Hour)

	first, err := c.ListDirectives(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("#1: %v", err)
	}
	first.Directives["8.2"] = nil
	delete(first.Directives, "8.2")

	second, err := c.ListDirectives(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("#2: %v", err)
	}
	if len(second.Directives["8.2"]) != 2 {
		t.Errorf("cache corrupted by caller mutation: %+v", second.Directives)
	}
}

func TestDirectivesCache_KeyByNameAndRef(t *testing.T) {
	lister := &fakeDirectiveLister{catalog: sampleCatalog()}
	c := NewDirectivesCache(lister, time.Hour)

	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#v1: %v", err)
	}
	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v2"); err != nil {
		t.Fatalf("#v2: %v", err)
	}
	// Same name, different ref—two independent records.
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (per-(name,ref) keys)", n)
	}
}

func TestDirectivesCache_Expiry(t *testing.T) {
	lister := &fakeDirectiveLister{catalog: sampleCatalog()}
	c := NewDirectivesCache(lister, 100*time.Millisecond)

	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#1: %v", err)
	}
	c.now = func() time.Time { return time.Now().Add(200 * time.Millisecond) }
	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls after TTL = %d, want 2", n)
	}
}

func TestDirectivesCache_Invalidate_DropsAllRefs(t *testing.T) {
	lister := &fakeDirectiveLister{catalog: sampleCatalog()}
	c := NewDirectivesCache(lister, time.Hour)

	// Warm up three keys: redis@v1, redis@v2, mongo@v1.
	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("warm redis@v1: %v", err)
	}
	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v2"); err != nil {
		t.Fatalf("warm redis@v2: %v", err)
	}
	if _, err := c.ListDirectives(context.Background(), "mongo", "g", "v1"); err != nil {
		t.Fatalf("warm mongo@v1: %v", err)
	}
	preInvalidate := lister.calls.Load()

	c.Invalidate("redis")

	// Both ref for redis dropped, mongo stays in cache.
	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("post-inv redis@v1: %v", err)
	}
	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v2"); err != nil {
		t.Fatalf("post-inv redis@v2: %v", err)
	}
	if _, err := c.ListDirectives(context.Background(), "mongo", "g", "v1"); err != nil {
		t.Fatalf("post-inv mongo@v1: %v", err)
	}
	// redis@v1 + redis@v2 recalculated (2), mongo@v1 from cache (0).
	if n := lister.calls.Load() - preInvalidate; n != 2 {
		t.Errorf("calls after Invalidate(\"redis\") = %d, want 2 (mongo should stay in cache)", n)
	}
}

func TestDirectivesCache_ErrorNotCached(t *testing.T) {
	listerErr := errors.New("loader busted")
	lister := &fakeDirectiveLister{err: listerErr}
	c := NewDirectivesCache(lister, time.Hour)

	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.ListDirectives(context.Background(), "redis", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("#2 err = %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (errors not cached)", n)
	}
}

func TestDirectivesCache_PerKeyLock(t *testing.T) {
	lister := &fakeDirectiveLister{catalog: sampleCatalog(), delay: 30 * time.Millisecond}
	c := NewDirectivesCache(lister, time.Hour)

	const N = 5
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.ListDirectives(context.Background(), "redis", "g", "v1")
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

func TestDirectivesCache_NilLister_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil lister")
		}
	}()
	_ = NewDirectivesCache(nil, time.Hour)
}

func TestDirectivesCache_ListerFunc_Implements(t *testing.T) {
	var _ DirectiveLister = DirectiveListerFunc(func(_ context.Context, _, _, _ string) (*artifact.DirectiveCatalog, error) {
		return nil, nil
	})
}
