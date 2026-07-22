package serviceregistry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSnapSource — a controllable SnapshotSource for Holder tests without Postgres.
// Snapshot and error change between Refresh calls under a Mutex (Run reads in the background).
type fakeSnapSource struct {
	mu   sync.Mutex
	snap *Snapshot
	err  error

	loads int
}

func (f *fakeSnapSource) Load(_ context.Context) (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

func (f *fakeSnapSource) set(snap *Snapshot, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = snap
	f.err = err
}

// snapWith — helper: a snapshot with the given Services (by name) and the
// default_destiny_source scalar.
func snapWith(dds string, names ...string) *Snapshot {
	s := &Snapshot{
		services:             map[string]ServiceEntry{},
		defaultDestinySource: dds,
	}
	for _, n := range names {
		s.services[n] = ServiceEntry{Name: n, Git: "git@x:" + n + ".git", Ref: "main"}
	}
	return s
}

func TestHolder_InitialSnapshot_FromSource(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("git@x:destiny.git", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	if e, ok := h.Resolve("web"); !ok || e.Name != "web" {
		t.Errorf("Resolve(web) = %+v ok=%v, want web/true", e, ok)
	}
	if _, ok := h.Resolve("missing"); ok {
		t.Errorf("Resolve(missing) ok=true, want false")
	}
	if got := h.DefaultDestinySource(); got != "git@x:destiny.git" {
		t.Errorf("DefaultDestinySource = %q, want git@x:destiny.git", got)
	}
}

func TestHolder_InitialLoadError_Fatal(t *testing.T) {
	src := &fakeSnapSource{err: errors.New("db down")}
	if _, err := NewHolder(context.Background(), src, time.Hour, nil); err == nil {
		t.Fatal("NewHolder should fail when initial Load errors")
	}
}

func TestHolder_Refresh_PicksUpChange(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// Before refresh: api-service is absent, scalar is empty.
	if _, ok := h.Resolve("api"); ok {
		t.Fatalf("Resolve(api) ok=true before refresh, want false")
	}
	if got := h.DefaultDestinySource(); got != "" {
		t.Fatalf("DefaultDestinySource = %q before refresh, want empty", got)
	}

	// Change the snapshot in the DB: api added + default_destiny_source set.
	src.set(snapWith("git@x:destiny.git", "web", "api"), nil)
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if _, ok := h.Resolve("api"); !ok {
		t.Errorf("Resolve(api) ok=false after refresh, want true")
	}
	if got := h.DefaultDestinySource(); got != "git@x:destiny.git" {
		t.Errorf("DefaultDestinySource = %q after refresh, want git@x:destiny.git", got)
	}
}

func TestHolder_Refresh_ErrorKeepsPrevSnapshot(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("git@x:destiny.git", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// Refresh fails — the active snapshot should stay unchanged.
	src.set(nil, errors.New("db blip"))
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh should return error on Load failure")
	}
	if _, ok := h.Resolve("web"); !ok {
		t.Errorf("Resolve(web) ok=false after failed refresh, want prev snapshot kept")
	}
	if got := h.DefaultDestinySource(); got != "git@x:destiny.git" {
		t.Errorf("DefaultDestinySource lost after failed refresh: %q", got)
	}
}

func TestHolder_Run_TTLPolls(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	// Change the snapshot; the background ticker should pick it up within a few intervals.
	src.set(snapWith("", "web", "api"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := h.Resolve("api"); ok {
			break // picked up
		}
		if time.Now().After(deadline) {
			t.Fatal("TTL-poll did not pick up new service within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHolder_NilSource_EmptySnapshot(t *testing.T) {
	h, err := NewHolder(context.Background(), nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder(nil src): %v", err)
	}
	if _, ok := h.Resolve("anything"); ok {
		t.Errorf("nil-source Holder should resolve nothing")
	}
	if got := h.DefaultDestinySource(); got != "" {
		t.Errorf("nil-source DefaultDestinySource = %q, want empty", got)
	}
	// Run with a nil src — no-op, doesn't hang.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.Run(ctx)
}

// fakeInvSource — a controllable InvalidationSource for WatchInvalidations
// tests without Redis. invalidate() simulates an incoming invalidate signal
// from elsewhere; Watch blocks until ctx.Done() or fatalErr.
type fakeInvSource struct {
	signals chan struct{}
	err     error // if set, Watch returns it immediately (simulates a fatal subscribe)
}

func newFakeInvSource() *fakeInvSource {
	return &fakeInvSource{signals: make(chan struct{}, 16)}
}

func (f *fakeInvSource) invalidate() { f.signals <- struct{}{} }

func (f *fakeInvSource) Watch(ctx context.Context, onInvalidate func()) error {
	if f.err != nil {
		return f.err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-f.signals:
			onInvalidate()
		}
	}
}

// TestHolder_WatchInvalidations_NearInstantRefresh — an incoming invalidate
// signal refreshes the snapshot WITHOUT waiting for TTL-poll (interval=1h,
// the tick won't fire within the test window).
func TestHolder_WatchInvalidations_NearInstantRefresh(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := newFakeInvSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.WatchInvalidations(ctx, invSrc)

	if _, ok := h.Resolve("api"); ok {
		t.Fatalf("Resolve(api) ok=true before invalidate, want false")
	}

	// Change the snapshot in the DB and send invalidate — Holder should
	// re-read near-instantly (not waiting for the hourly TTL tick).
	src.set(snapWith("", "web", "api"), nil)
	invSrc.invalidate()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := h.Resolve("api"); ok {
			break // picked up via pub/sub-invalidate
		}
		if time.Now().After(deadline) {
			t.Fatal("invalidate did not trigger near-instant refresh within deadline")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHolder_WatchInvalidations_TTLFallbackStillWorks — even when invalidate
// never arrives, TTL-poll [Run] still refreshes the snapshot (fallback isn't broken).
func TestHolder_WatchInvalidations_TTLFallbackStillWorks(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := newFakeInvSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)                        // TTL-poll
	go h.WatchInvalidations(ctx, invSrc) // pub/sub — but we don't send the signal

	src.set(snapWith("", "web", "api"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := h.Resolve("api"); ok {
			break // picked up by TTL-poll (fallback)
		}
		if time.Now().After(deadline) {
			t.Fatal("TTL-poll fallback did not pick up change without invalidate")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHolder_WatchInvalidations_FatalSubscribeFailSoft — a fatal subscribe
// error does NOT crash: WatchInvalidations returns (logs a warning), the
// daemon continues on TTL-poll.
func TestHolder_WatchInvalidations_FatalSubscribeFailSoft(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := &fakeInvSource{err: errors.New("redis subscribe down")}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.WatchInvalidations(context.Background(), invSrc)
	}()
	select {
	case <-done:
		// OK — returned without panic/hang.
	case <-time.After(2 * time.Second):
		t.Fatal("WatchInvalidations did not return on fatal subscribe error")
	}
}

// TestHolder_WatchInvalidations_NilSourceNoop — with a nil DB source or
// nil invalidator, Watch is a no-op.
func TestHolder_WatchInvalidations_NilSourceNoop(t *testing.T) {
	h, err := NewHolder(context.Background(), nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder(nil src): %v", err)
	}
	h.WatchInvalidations(context.Background(), newFakeInvSource()) // doesn't hang

	h2, err := NewHolder(context.Background(), &fakeSnapSource{snap: snapWith("", "web")}, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h2.WatchInvalidations(context.Background(), nil) // doesn't hang
}

// TestHolder_AtomicSnapshot_Race — concurrent synchronous getters (Resolve/
// DefaultDestinySource) against a background Refresh: the snapshot's atomic
// swap must not produce a data race (-race). Each getter result is internally
// consistent (reads one published snapshot).
func TestHolder_AtomicSnapshot_Race(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("git@x:a.git", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	// Writer: alternates two snapshots via Refresh.
	wg.Add(1)
	go func() {
		defer wg.Done()
		snaps := []*Snapshot{
			snapWith("git@x:a.git", "web"),
			snapWith("git@x:b.git", "web", "api"),
		}
		for i := 0; i < 500; i++ {
			src.set(snaps[i%2], nil)
			_ = h.Refresh(ctx)
		}
	}()

	// Readers: synchronous getters across several goroutines.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_, _ = h.Resolve("web")
				_, _ = h.Resolve("api")
				_ = h.DefaultDestinySource()
			}
		}()
	}

	wg.Wait()
}
