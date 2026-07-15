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

// fakeStateSchemaLister — a programmable StateSchemaLister: counts calls
// (total and per key), returns a configured info or error, optionally with
// a delay for testing the per-key lock (parity with fakeScenarioLister).
type fakeStateSchemaLister struct {
	mu       sync.Mutex
	calls    atomic.Int64
	callsKey map[string]int
	info     *artifact.StateSchemaInfo
	err      error
	delay    time.Duration
}

func newFakeStateSchemaLister() *fakeStateSchemaLister {
	return &fakeStateSchemaLister{callsKey: make(map[string]int)}
}

func (f *fakeStateSchemaLister) ListStateSchema(_ context.Context, name, _, ref string) (*artifact.StateSchemaInfo, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.callsKey[name+"|"+ref]++
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.info == nil {
		return nil, nil
	}
	cp := *f.info
	if f.info.Migrations != nil {
		cp.Migrations = make([]artifact.Migration, len(f.info.Migrations))
		copy(cp.Migrations, f.info.Migrations)
	}
	return &cp, nil
}

func sampleInfo() *artifact.StateSchemaInfo {
	return &artifact.StateSchemaInfo{
		Version: 2,
		Schema:  map[string]any{"type": "object"},
		Migrations: []artifact.Migration{
			{From: 1, To: 2, Path: "migrations/001_to_002.yml"},
		},
	}
}

func TestStateSchemaCache_HitMiss(t *testing.T) {
	lister := newFakeStateSchemaLister()
	lister.info = sampleInfo()
	c := NewStateSchemaCache(lister, time.Hour)

	got, err := c.ListStateSchema(context.Background(), "web", "g", "v1")
	if err != nil {
		t.Fatalf("#1: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("Version = %d", got.Version)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls after miss = %d, want 1", n)
	}

	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls after hit = %d, want 1 (cache did not work)", n)
	}
}

func TestStateSchemaCache_KeyByNameAndRef(t *testing.T) {
	lister := newFakeStateSchemaLister()
	lister.info = sampleInfo()
	c := NewStateSchemaCache(lister, time.Hour)

	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#v1: %v", err)
	}
	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v2"); err != nil {
		t.Fatalf("#v2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (per-(name,ref) keys)", n)
	}
}

func TestStateSchemaCache_Expiry(t *testing.T) {
	lister := newFakeStateSchemaLister()
	lister.info = sampleInfo()
	c := NewStateSchemaCache(lister, 100*time.Millisecond)

	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#1: %v", err)
	}
	c.now = func() time.Time { return time.Now().Add(200 * time.Millisecond) }
	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls after TTL = %d, want 2", n)
	}
}

func TestStateSchemaCache_Invalidate_DropsAllRefs(t *testing.T) {
	lister := newFakeStateSchemaLister()
	lister.info = sampleInfo()
	c := NewStateSchemaCache(lister, time.Hour)

	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("warm web@v1: %v", err)
	}
	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v2"); err != nil {
		t.Fatalf("warm web@v2: %v", err)
	}
	if _, err := c.ListStateSchema(context.Background(), "api", "g", "v1"); err != nil {
		t.Fatalf("warm api@v1: %v", err)
	}
	preInvalidate := lister.calls.Load()

	c.Invalidate("web")

	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("post-inv web@v1: %v", err)
	}
	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v2"); err != nil {
		t.Fatalf("post-inv web@v2: %v", err)
	}
	if _, err := c.ListStateSchema(context.Background(), "api", "g", "v1"); err != nil {
		t.Fatalf("post-inv api@v1: %v", err)
	}
	if n := lister.calls.Load() - preInvalidate; n != 2 {
		t.Errorf("calls after Invalidate(\"web\") = %d, want 2 (api should stay in cache)", n)
	}
}

func TestStateSchemaCache_ErrorNotCached(t *testing.T) {
	listerErr := errors.New("loader busted")
	lister := newFakeStateSchemaLister()
	lister.err = listerErr
	c := NewStateSchemaCache(lister, time.Hour)

	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.ListStateSchema(context.Background(), "web", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("#2 err = %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (errors not cached)", n)
	}
}

func TestStateSchemaCache_PerKeyLock(t *testing.T) {
	lister := newFakeStateSchemaLister()
	lister.info = sampleInfo()
	lister.delay = 30 * time.Millisecond
	c := NewStateSchemaCache(lister, time.Hour)

	const N = 5
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.ListStateSchema(context.Background(), "web", "g", "v1")
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

func TestStateSchemaCache_ClonesOnReturn(t *testing.T) {
	lister := newFakeStateSchemaLister()
	lister.info = sampleInfo()
	c := NewStateSchemaCache(lister, time.Hour)

	got, err := c.ListStateSchema(context.Background(), "web", "g", "v1")
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	// Mutate returned Migrations slice; next call should return
	// untouched cached snapshot.
	got.Migrations[0].Path = "MUTATED"
	got2, err := c.ListStateSchema(context.Background(), "web", "g", "v1")
	if err != nil {
		t.Fatalf("ListStateSchema #2: %v", err)
	}
	if got2.Migrations[0].Path == "MUTATED" {
		t.Errorf("cache does not clone Migrations: repeat returned %q", got2.Migrations[0].Path)
	}
}

func TestStateSchemaCache_NilLister_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil lister")
		}
	}()
	_ = NewStateSchemaCache(nil, time.Hour)
}

func TestStateSchemaCache_ListerFunc_Implements(t *testing.T) {
	var _ StateSchemaLister = StateSchemaListerFunc(func(_ context.Context, _, _, _ string) (*artifact.StateSchemaInfo, error) {
		return nil, nil
	})
}
