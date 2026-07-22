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

// fakeLister — a programmable artifact.RefsLister: counts calls and returns
// preset refs / error.
type fakeLister struct {
	mu    sync.Mutex
	calls atomic.Int64
	refs  []artifact.GitRef
	err   error
	delay time.Duration
}

func (f *fakeLister) ListRefs(_ context.Context, _ string) ([]artifact.GitRef, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]artifact.GitRef, len(f.refs))
	copy(out, f.refs)
	return out, nil
}

func TestRefsCache_HitMiss(t *testing.T) {
	lister := &fakeLister{refs: []artifact.GitRef{
		{Name: "v1.0.0", Type: artifact.GitRefTypeTag, Commit: "abc"},
	}}
	c := NewRefsCache(lister, time.Hour)

	// First call — miss → lister invoked.
	got, err := c.ListRefs(context.Background(), "web", "file:///tmp/web")
	if err != nil {
		t.Fatalf("ListRefs #1: %v", err)
	}
	if len(got) != 1 || got[0].Name != "v1.0.0" {
		t.Fatalf("refs = %+v", got)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls after miss = %d, want 1", n)
	}

	// Second call on the same name — hit.
	got2, err := c.ListRefs(context.Background(), "web", "file:///tmp/web")
	if err != nil {
		t.Fatalf("ListRefs #2: %v", err)
	}
	if len(got2) != 1 || got2[0].Name != "v1.0.0" {
		t.Fatalf("refs hit = %+v", got2)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls after hit = %d, want 1 (cache did not work)", n)
	}
}

func TestRefsCache_Expiry(t *testing.T) {
	lister := &fakeLister{refs: []artifact.GitRef{{Name: "v1", Type: artifact.GitRefTypeTag, Commit: "a"}}}
	c := NewRefsCache(lister, 100*time.Millisecond)

	if _, err := c.ListRefs(context.Background(), "web", "g"); err != nil {
		t.Fatalf("#1: %v", err)
	}
	// Swap the clock to "200ms later".
	c.now = func() time.Time { return time.Now().Add(200 * time.Millisecond) }
	if _, err := c.ListRefs(context.Background(), "web", "g"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls after TTL expiry = %d, want 2", n)
	}
}

func TestRefsCache_Invalidate(t *testing.T) {
	lister := &fakeLister{refs: []artifact.GitRef{{Name: "v1", Type: artifact.GitRefTypeTag, Commit: "a"}}}
	c := NewRefsCache(lister, time.Hour)

	if _, err := c.ListRefs(context.Background(), "web", "g"); err != nil {
		t.Fatalf("#1: %v", err)
	}
	c.Invalidate("web")
	if _, err := c.ListRefs(context.Background(), "web", "g"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls after Invalidate = %d, want 2", n)
	}
}

func TestRefsCache_ErrorNotCached(t *testing.T) {
	listerErr := errors.New("git unreachable")
	lister := &fakeLister{err: listerErr}
	c := NewRefsCache(lister, time.Hour)

	if _, err := c.ListRefs(context.Background(), "web", "g"); !errors.Is(err, listerErr) {
		t.Fatalf("err = %v, want %v", err, listerErr)
	}
	// Second call — hits the lister again (errors aren't cached).
	if _, err := c.ListRefs(context.Background(), "web", "g"); !errors.Is(err, listerErr) {
		t.Fatalf("#2 err = %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls after two errors = %d, want 2", n)
	}
}

// TestRefsCache_PerNameLock — concurrent requests for one service are
// serialized: the lister is invoked ONCE, the rest get the result from cache.
func TestRefsCache_PerNameLock(t *testing.T) {
	lister := &fakeLister{
		refs:  []artifact.GitRef{{Name: "v1", Type: artifact.GitRefTypeTag, Commit: "a"}},
		delay: 30 * time.Millisecond,
	}
	c := NewRefsCache(lister, time.Hour)

	const N = 5
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.ListRefs(context.Background(), "web", "g")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls = %d, expected 1 (per-name lock did not work)", n)
	}
}

// TestRefsCache_PerNameIsolation — different services are cached independently.
func TestRefsCache_PerNameIsolation(t *testing.T) {
	lister := &fakeLister{refs: []artifact.GitRef{{Name: "v1", Type: artifact.GitRefTypeTag, Commit: "a"}}}
	c := NewRefsCache(lister, time.Hour)

	if _, err := c.ListRefs(context.Background(), "web", "g1"); err != nil {
		t.Fatalf("web: %v", err)
	}
	if _, err := c.ListRefs(context.Background(), "api", "g2"); err != nil {
		t.Fatalf("api: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, expected 2 (two independent cache entries)", n)
	}
}

func TestRefsCache_NilLister_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil lister")
		}
	}()
	_ = NewRefsCache(nil, time.Hour)
}
