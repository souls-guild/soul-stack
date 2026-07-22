package sigilcache

import (
	"sync"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func sig(ns, name, ref string) *keeperv1.PluginSigil {
	return &keeperv1.PluginSigil{Namespace: ns, Name: name, Ref: ref}
}

func TestReplaceAllGet(t *testing.T) {
	c := New()

	if got := c.Get("core", "pkg"); got != nil {
		t.Fatalf("Get on an empty cache: expected nil, got %v", got)
	}

	c.ReplaceAll([]*keeperv1.PluginSigil{sig("core", "pkg", "v1")})
	got := c.Get("core", "pkg")
	if got == nil {
		t.Fatal("Get after ReplaceAll: expected Sigil, got nil")
	}
	if got.GetRef() != "v1" {
		t.Fatalf("ref: expected v1, got %q", got.GetRef())
	}
}

// TestKeyIsNamespaceAndName: the (namespace, name) pair addresses distinct
// slots — the same name in different namespaces doesn't collide.
func TestKeyIsNamespaceAndName(t *testing.T) {
	c := New()
	c.ReplaceAll([]*keeperv1.PluginSigil{
		sig("core", "pkg", "v1"),
		sig("community", "pkg", "v9"),
	})

	core := c.Get("core", "pkg")
	if core == nil || core.GetRef() != "v1" {
		t.Fatalf("expected core/pkg ref=v1, got %v", core)
	}
	comm := c.Get("community", "pkg")
	if comm == nil || comm.GetRef() != "v9" {
		t.Fatalf("the same name in different namespaces must be different slots: %v", comm)
	}
}

// TestSurvivesReconnect is a structural guarantee: the cache isn't tied to a
// stream. The same *Cache survives multiple notional "sessions" (in prod
// each session is a new StreamSession; the cache is created in runDaemon
// outside the reconnect loop).
func TestSurvivesReconnect(t *testing.T) {
	c := New() // created once at the Soul runtime level

	// "Session 1" applied a snapshot and closed.
	c.ReplaceAll([]*keeperv1.PluginSigil{sig("core", "file", "v1")})

	// "Session 2" (after reconnect) sees the grant applied in session 1 —
	// the same *Cache isn't recreated.
	if got := c.Get("core", "file"); got == nil {
		t.Fatal("cache lost admission after a conditional reconnect - it must not be re-created per session")
	}
}

// TestReplaceAllAddsAndRevokes: ReplaceAll atomically adds new grants and
// forgets missing ones (near-instant revoke, ADR-026(h) S6c).
func TestReplaceAllAddsAndRevokes(t *testing.T) {
	c := New()

	// First snapshot: two grants.
	c.ReplaceAll([]*keeperv1.PluginSigil{
		sig("core", "pkg", "v1"),
		sig("core", "file", "v1"),
	})
	if c.Get("core", "pkg") == nil || c.Get("core", "file") == nil {
		t.Fatal("ReplaceAll should have added both admissions")
	}

	// Second snapshot: core/pkg revoked (missing), core/service added,
	// core/file updated to v2.
	c.ReplaceAll([]*keeperv1.PluginSigil{
		sig("core", "file", "v2"),
		sig("core", "service", "v1"),
	})

	if c.Get("core", "pkg") != nil {
		t.Fatal("revoke: admission missing from the snapshot must be forgotten")
	}
	if got := c.Get("core", "file"); got == nil || got.GetRef() != "v2" {
		t.Fatalf("update: core/file expected ref=v2, got %v", got)
	}
	if c.Get("core", "service") == nil {
		t.Fatal("allow: a new admission should appear")
	}
}

// TestReplaceAllEmptyClears: an empty snapshot clears the whole cache (no
// plugin remains granted).
func TestReplaceAllEmptyClears(t *testing.T) {
	c := New()
	c.ReplaceAll([]*keeperv1.PluginSigil{sig("core", "pkg", "v1")})
	if c.Get("core", "pkg") == nil {
		t.Fatal("precondition: the admission must be in the cache")
	}

	c.ReplaceAll(nil)
	if c.Get("core", "pkg") != nil {
		t.Fatal("empty/nil snapshot should clear the cache")
	}
}

// TestReplaceAllSkipsNilElements: nil elements inside a snapshot don't crash
// the replace and don't create garbage keys.
func TestReplaceAllSkipsNilElements(t *testing.T) {
	c := New()
	c.ReplaceAll([]*keeperv1.PluginSigil{
		sig("core", "pkg", "v1"),
		nil,
		sig("core", "file", "v1"),
	})
	if c.Get("core", "pkg") == nil || c.Get("core", "file") == nil {
		t.Fatal("ReplaceAll should skip nil and keep valid admissions")
	}
	if c.Get("", "") != nil {
		t.Fatal("a nil element must not create an entry under an empty key")
	}
}

// TestConcurrentReplaceAllGet: concurrent ReplaceAll (writer) + Get (readers)
// under -race: the set swap is atomic, readers see a consistent set, no
// races.
func TestConcurrentReplaceAllGet(t *testing.T) {
	c := New()
	const writers = 8
	const readers = 8
	const iters = 500

	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c.ReplaceAll([]*keeperv1.PluginSigil{
					sig("core", "pkg", "v1"),
					sig("core", "file", "v1"),
				})
			}
		}()
	}
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = c.Get("core", "pkg")
				_ = c.Get("core", "file")
			}
		}()
	}
	wg.Wait()
}
