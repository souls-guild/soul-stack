package rbac

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSource is a controllable SnapshotSource for testing Holder without
// Postgres. The snapshot and error are changed between Refreshes under a
// Mutex (Run reads in the background).
type fakeSource struct {
	mu   sync.Mutex
	snap *Snapshot
	err  error

	loads int
}

func (f *fakeSource) Load(_ context.Context) (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

func (f *fakeSource) set(snap *Snapshot, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = snap
	f.err = err
}

// snapshotWith is a helper: a snapshot with one cluster-admin role (perm
// `*`) and a given list of bound AIDs.
func adminSnapshot(aids ...string) *Snapshot {
	s := &Snapshot{
		Roles:      map[string][]string{"cluster-admin": {"*"}},
		Membership: map[string][]string{},
	}
	for _, aid := range aids {
		s.Membership[aid] = []string{"cluster-admin"}
	}
	return s
}

func TestHolder_InitialEnforcer_FromSnapshot(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	if err := h.Check("archon-alice", "operator", "create", nil); err != nil {
		t.Errorf("alice (cluster-admin) should pass operator.create: %v", err)
	}
	if err := h.Check("archon-bob", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("bob should be denied: %v", err)
	}
	if got := h.ClusterAdmins(); len(got) != 1 || got[0] != "archon-alice" {
		t.Errorf("ClusterAdmins = %v", got)
	}
}

func TestHolder_InitialLoadError_Fatal(t *testing.T) {
	src := &fakeSource{err: errors.New("db down")}
	if _, err := NewHolder(context.Background(), src, time.Hour, nil); err == nil {
		t.Fatal("NewHolder should fail when initial Load errors")
	}
}

func TestHolder_Refresh_PicksUpNewMembership(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// Before reload, Bob isn't an admin.
	if err := h.Check("archon-bob", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("bob should be denied before refresh: %v", err)
	}

	// Change the snapshot in the DB: Bob becomes cluster-admin too.
	src.set(adminSnapshot("archon-alice", "archon-bob"), nil)
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if err := h.Check("archon-bob", "operator", "create", nil); err != nil {
		t.Errorf("bob should be allowed after refresh: %v", err)
	}
	if got := h.ClusterAdmins(); len(got) != 2 {
		t.Errorf("ClusterAdmins after refresh = %v, want 2 entries", got)
	}
}

func TestHolder_Refresh_ErrorKeepsPrevEnforcer(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// Reload fails — the active enforcer should stay unchanged.
	src.set(nil, errors.New("db blip"))
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh should return error on Load failure")
	}
	if err := h.Check("archon-alice", "operator", "create", nil); err != nil {
		t.Errorf("alice should still pass after failed refresh: %v", err)
	}
}

func TestHolder_Refresh_BadPermissionKeepsPrevEnforcer(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// A snapshot with an invalid permission (outside the catalog) →
	// NewEnforcerFromSnapshot fails; the active enforcer is unchanged.
	src.set(&Snapshot{
		Roles:      map[string][]string{"bad": {"unknown.create"}},
		Membership: map[string][]string{"archon-bob": {"bad"}},
	}, nil)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh should fail on unknown permission")
	}
	if err := h.Check("archon-alice", "operator", "create", nil); err != nil {
		t.Errorf("alice should still pass after bad-snapshot refresh: %v", err)
	}
}

func TestHolder_Run_TTLPolls(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	// Change the snapshot; the background ticker should pick it up within a
	// few intervals.
	src.set(adminSnapshot("archon-alice", "archon-bob"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := h.Check("archon-bob", "operator", "create", nil); err == nil {
			break // picked up
		}
		if time.Now().After(deadline) {
			t.Fatal("TTL-poll did not pick up new membership within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHolder_NilSource_DefaultDeny(t *testing.T) {
	h, err := NewHolder(context.Background(), nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder(nil src): %v", err)
	}
	if err := h.Check("archon-anyone", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("nil-source Holder should deny: %v", err)
	}
	// Run with nil src is a no-op — doesn't hang.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.Run(ctx)
}

// fakeInvalidationSource is a controllable [InvalidationSource] for testing
// WatchInvalidations without Redis. invalidate() simulates a remote
// invalidate signal arriving; Watch blocks until ctx.Done() or fatalErr.
type fakeInvalidationSource struct {
	signals chan struct{}
	err     error // if set, Watch returns it immediately (simulates a fatal subscribe)
}

func newFakeInvalidationSource() *fakeInvalidationSource {
	return &fakeInvalidationSource{signals: make(chan struct{}, 16)}
}

func (f *fakeInvalidationSource) invalidate() { f.signals <- struct{}{} }

func (f *fakeInvalidationSource) Watch(ctx context.Context, onInvalidate func()) error {
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

// TestHolder_WatchInvalidations_NearInstantRefresh — an invalidate signal
// arriving refreshes the snapshot from the DB WITHOUT waiting for the TTL
// poll (interval=1 hour, the tick won't fire within the test window).
func TestHolder_WatchInvalidations_NearInstantRefresh(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := newFakeInvalidationSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.WatchInvalidations(ctx, invSrc)

	// Before invalidation, Bob isn't an admin.
	if err := h.Check("archon-bob", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("bob should be denied before invalidate: %v", err)
	}

	// Change the snapshot in the DB and send invalidate — Holder should
	// reload near-instantly (without waiting for the hourly TTL tick).
	src.set(adminSnapshot("archon-alice", "archon-bob"), nil)
	invSrc.invalidate()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := h.Check("archon-bob", "operator", "create", nil); err == nil {
			break // picked up via pub/sub invalidate
		}
		if time.Now().After(deadline) {
			t.Fatal("invalidate did not trigger near-instant refresh within deadline")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHolder_WatchInvalidations_TTLFallbackStillWorks — even when invalidate
// does NOT arrive (dropped message / no pub/sub), TTL poll [Run] still
// refreshes the snapshot. B2 = B1 + pub/sub, the fallback isn't broken.
func TestHolder_WatchInvalidations_TTLFallbackStillWorks(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := newFakeInvalidationSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)                        // TTL poll
	go h.WatchInvalidations(ctx, invSrc) // pub/sub — but we don't send the signal

	// Change the snapshot, don't send invalidate — only TTL poll should pick
	// it up.
	src.set(adminSnapshot("archon-alice", "archon-bob"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := h.Check("archon-bob", "operator", "create", nil); err == nil {
			break // picked up by TTL poll (fallback)
		}
		if time.Now().After(deadline) {
			t.Fatal("TTL-poll fallback did not pick up change without invalidate")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHolder_WatchInvalidations_FatalSubscribeFailSoft — a fatal subscribe
// error does NOT bring things down: WatchInvalidations returns (logs a
// warning), the daemon continues on TTL polling.
func TestHolder_WatchInvalidations_FatalSubscribeFailSoft(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := &fakeInvalidationSource{err: errors.New("redis subscribe down")}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.WatchInvalidations(context.Background(), invSrc)
	}()
	select {
	case <-done:
		// OK — returned without panicking/hanging.
	case <-time.After(2 * time.Second):
		t.Fatal("WatchInvalidations did not return on fatal subscribe error")
	}
}

// TestHolder_WatchInvalidations_NilSourceNoop — with a nil DB source or a
// nil invalidator, Watch is a no-op (nothing to refresh / nowhere to
// subscribe).
func TestHolder_WatchInvalidations_NilSourceNoop(t *testing.T) {
	// nil DB source: Holder is in default-deny, Watch returns immediately.
	h, err := NewHolder(context.Background(), nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder(nil src): %v", err)
	}
	h.WatchInvalidations(context.Background(), newFakeInvalidationSource()) // doesn't hang

	// nil invalidator with a live DB source: also a no-op.
	h2, err := NewHolder(context.Background(), &fakeSource{snap: adminSnapshot("archon-alice")}, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h2.WatchInvalidations(context.Background(), nil) // doesn't hang
}
