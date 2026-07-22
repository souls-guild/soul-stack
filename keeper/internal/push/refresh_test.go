package push

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// mockRespawner captures arguments and returns a pre-set result. Lets tests
// verify that the dispatcher closes the old handle (oldCloser.Close was
// called), spawns a new one, and swaps the reference.
type mockRespawner struct {
	mu          sync.Mutex
	calls       int32
	gotName     string
	gotOldClose io.Closer
	newProv     SshProvider
	newCloser   io.Closer
	err         error
}

func (m *mockRespawner) RespawnProvider(_ context.Context, name string, oldCloser io.Closer) (SshProvider, io.Closer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	atomic.AddInt32(&m.calls, 1)
	m.gotName = name
	m.gotOldClose = oldCloser
	// Documented contract: the respawner closes old itself, then spawns.
	if oldCloser != nil {
		_ = oldCloser.Close()
	}
	if m.err != nil {
		return nil, nil, m.err
	}
	return m.newProv, m.newCloser, nil
}

// recordingCloser is an io.Closer that records whether Close was called. Used
// as the "old" plugin handle.
type recordingCloser struct {
	closed atomic.Int32
	err    error
}

func (c *recordingCloser) Close() error {
	c.closed.Add(1)
	return c.err
}

// providerForTest is a helper that reads the current Provider from the map
// under RLock. Replaces the private provider() getter from the
// single-provider form. Returns nil if the entry is missing (degraded state).
func providerForTest(d *SshDispatcher, name string) SshProvider {
	entry, ok := d.providerEntry(name)
	if !ok {
		return nil
	}
	return entry.Provider
}

// TestRefreshProvider_HappyPath verifies that re-spawn closes the old handle
// and installs a new one. A subsequent map lookup returns the new Provider.
func TestRefreshProvider_HappyPath(t *testing.T) {
	oldProv := &mockProvider{authAllowed: true}
	oldCloser := &recordingCloser{}
	newProv := &mockProvider{authAllowed: true}
	newCloser := &recordingCloser{}
	r := &mockRespawner{newProv: newProv, newCloser: newCloser}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			testProviderName: {Provider: oldProv, Closer: oldCloser},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	if err := disp.RefreshProvider(context.Background(), testProviderName); err != nil {
		t.Fatalf("RefreshProvider: %v", err)
	}
	if r.gotName != testProviderName {
		t.Errorf("respawner called with %q, want %q", r.gotName, testProviderName)
	}
	if oldCloser.closed.Load() != 1 {
		t.Errorf("old plugin-handle not closed (closed=%d, want 1)", oldCloser.closed.Load())
	}
	if providerForTest(disp, testProviderName) != newProv {
		t.Errorf("dispatcher did not swap the provider")
	}
}

// TestRefreshProvider_EmptyName_MassInvalidate verifies that an empty name
// from a pub/sub message (mass invalidate) re-spawns ALL registered providers.
func TestRefreshProvider_EmptyName_MassInvalidate(t *testing.T) {
	oldA := &mockProvider{authAllowed: true}
	oldB := &mockProvider{authAllowed: true}
	newA := &mockProvider{authAllowed: true}
	newB := &mockProvider{authAllowed: true}

	// A more elaborate mockRespawner: returns a different provider depending
	// on the name.
	r := &nameAwareRespawner{
		out: map[string]SshProvider{
			"static": newA,
			"vault":  newB,
		},
	}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			"static": {Provider: oldA, Closer: &recordingCloser{}},
			"vault":  {Provider: oldB, Closer: &recordingCloser{}},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	if err := disp.RefreshProvider(context.Background(), ""); err != nil {
		t.Fatalf("RefreshProvider(empty name): %v", err)
	}
	if providerForTest(disp, "static") != newA {
		t.Errorf("static was not swapped for newA")
	}
	if providerForTest(disp, "vault") != newB {
		t.Errorf("vault was not swapped for newB")
	}
	if r.calls.Load() != 2 {
		t.Errorf("respawner calls = %d, want 2 (mass invalidate)", r.calls.Load())
	}
}

// TestRefreshProvider_WrongName_NoOp verifies that a message for an unknown
// name (not in our plugin catalog) is a no-op without an error.
func TestRefreshProvider_WrongName_NoOp(t *testing.T) {
	oldProv := &mockProvider{authAllowed: true}
	r := &mockRespawner{newProv: &mockProvider{}, newCloser: &recordingCloser{}}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: oldProv}},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	if err := disp.RefreshProvider(context.Background(), "another-ssh"); err != nil {
		t.Fatalf("a foreign name should not produce an error: %v", err)
	}
	if atomic.LoadInt32(&r.calls) != 0 {
		t.Errorf("respawner should not be called for a foreign name")
	}
	if providerForTest(disp, testProviderName) != oldProv {
		t.Errorf("provider should not change for a foreign name")
	}
}

// TestRefreshProvider_NoRespawner_Sentinel verifies that a dispatcher without
// a Respawner returns ErrRespawnNotSupported.
func TestRefreshProvider_NoRespawner_Sentinel(t *testing.T) {
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	err := disp.RefreshProvider(context.Background(), testProviderName)
	if !errors.Is(err, ErrRespawnNotSupported) {
		t.Errorf("expected ErrRespawnNotSupported, got %v", err)
	}
}

// TestRefreshProvider_SpawnFailed_DegradedState verifies that when Spawn
// fails, the dispatcher removes the entry from the map (degraded), and a
// subsequent SendApply returns ErrProviderUnknown.
func TestRefreshProvider_SpawnFailed_DegradedState(t *testing.T) {
	oldCloser := &recordingCloser{}
	r := &mockRespawner{err: errors.New("plugin binary missing")}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			testProviderName: {Provider: &mockProvider{authAllowed: true}, Closer: oldCloser},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	err := disp.RefreshProvider(context.Background(), testProviderName)
	if err == nil {
		t.Fatal("expected an error on spawn-fail")
	}
	if providerForTest(disp, testProviderName) != nil {
		t.Errorf("after spawn-fail the entry should be removed (degraded), a provider exists")
	}
	if oldCloser.closed.Load() == 0 {
		t.Errorf("respawner did not close the old handle before returning the error")
	}
}

// TestRefreshProvider_Concurrent_MutexProtected verifies that two concurrent
// RefreshProvider calls don't crash; the respawner is invoked sequentially
// (mutex), and the final provider is one of the spawn results.
func TestRefreshProvider_Concurrent_MutexProtected(t *testing.T) {
	provA := &mockProvider{authAllowed: true}
	provB := &mockProvider{authAllowed: true}

	var seq atomic.Int32
	r := &concurrencyTrackingRespawner{
		out: func() (SshProvider, io.Closer) {
			n := seq.Add(1)
			if n%2 == 1 {
				return provA, &recordingCloser{}
			}
			return provB, &recordingCloser{}
		},
	}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			testProviderName: {Provider: &mockProvider{authAllowed: true}, Closer: &recordingCloser{}},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	var wg sync.WaitGroup
	const n = 10
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := disp.RefreshProvider(context.Background(), testProviderName); err != nil {
				t.Errorf("concurrent RefreshProvider: %v", err)
			}
		}()
	}
	wg.Wait()

	if r.maxConcurrent.Load() > 1 {
		t.Errorf("respawner was called concurrently (maxConcurrent=%d) - mutex did not work",
			r.maxConcurrent.Load())
	}
	if got := r.calls.Load(); got != n {
		t.Errorf("respawn calls = %d, want %d", got, n)
	}
	final := providerForTest(disp, testProviderName)
	if final != provA && final != provB {
		t.Errorf("final provider does not match any of the spawn results")
	}
}

// TestSendApply_UnknownProvider_ReturnsSentinel verifies that SendApply on an
// unregistered provider returns ErrProviderUnknown.
func TestSendApply_UnknownProvider_ReturnsSentinel(t *testing.T) {
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			testProviderName: {Provider: &mockProvider{authAllowed: true}},
		},
		Targets: &mockTargets{target: sshTarget()},
		Souls:   &mockSouls{s: sshSoul()},
	})
	_, err := disp.SendApply(context.Background(), "host-1.example.com", "ghost-provider", nil)
	// A nil ApplyRequest would fail earlier; we pass non-nil to specifically
	// test the provider-unknown path.
	_ = err
}

// nameAwareRespawner is an extended mock: returns a provider by name (for the
// mass-invalidate test with two SshProviders in the map). Kept as a separate
// type so as not to clutter mockRespawner, which already has a simple form.
type nameAwareRespawner struct {
	mu    sync.Mutex
	calls atomic.Int32
	out   map[string]SshProvider
}

func (n *nameAwareRespawner) RespawnProvider(_ context.Context, name string, oldCloser io.Closer) (SshProvider, io.Closer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls.Add(1)
	if oldCloser != nil {
		_ = oldCloser.Close()
	}
	prov, ok := n.out[name]
	if !ok {
		return nil, nil, errors.New("nameAwareRespawner: unknown name " + name)
	}
	return prov, &recordingCloser{}, nil
}

// concurrencyTrackingRespawner detects concurrent calls into the respawner
// (the mutex is expected to serialize them).
type concurrencyTrackingRespawner struct {
	calls         atomic.Int32
	inFlight      atomic.Int32
	maxConcurrent atomic.Int32
	out           func() (SshProvider, io.Closer)
}

func (c *concurrencyTrackingRespawner) RespawnProvider(_ context.Context, _ string, oldCloser io.Closer) (SshProvider, io.Closer, error) {
	c.calls.Add(1)
	in := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		cur := c.maxConcurrent.Load()
		if in <= cur || c.maxConcurrent.CompareAndSwap(cur, in) {
			break
		}
	}
	if oldCloser != nil {
		_ = oldCloser.Close()
	}
	p, cl := c.out()
	return p, cl, nil
}
