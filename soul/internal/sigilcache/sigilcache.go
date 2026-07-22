// Package sigilcache — runtime-level in-memory cache of Sigil trust seals
// (ADR-026) on the Soul side, slice S6a / S6c.
//
// The authoritative source for the active set is SigilSnapshot (ADR-026(h),
// Variant A): Keeper sends the full grant set over EventStream on connect and
// on every cluster-wide invalidate. Soul applies it as ReplaceAll
// ([Cache.ReplaceAll]), swapping out the ENTIRE local set. A grant missing
// from the snapshot is forgotten — this is how near-instant revoke works
// (S6c): after an Archon revokes access, it's cut off without restarting
// Soul. An empty snapshot means no plugin is granted. Verify against the
// cache is S6b (shared/pluginhost).
//
// Key is the pair (namespace, name), NOT ref: exactly one active Sigil is
// granted per pair (single-slot), its ref is stored inside the value.
//
// The cache lives at Soul's runtime level (created at daemon startup, outside
// the reconnect loop) and is NOT recreated on stream reconnect — grants
// survive an EventStream break (the next session gets a fresh snapshot first
// thing and applies ReplaceAll). NOT persisted to disk: a stale on-disk grant
// would be a hole in the trust model.
package sigilcache

import (
	"sync"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// key — the cache's composite key. namespace+name uniquely address one
// active Sigil (single-slot per pair, see package doc).
type key struct {
	namespace string
	name      string
}

// Cache — a thread-safe cache of Sigils. Writes come from the recv-loop
// (single writer), reads come from verify (S6b, not yet wired up); RWMutex
// covers both. The zero value is not ready for use — construct via New.
type Cache struct {
	mu    sync.RWMutex
	items map[key]*keeperv1.PluginSigil
}

// New creates an empty cache.
func New() *Cache {
	return &Cache{items: make(map[key]*keeperv1.PluginSigil)}
}

// ReplaceAll atomically replaces the ENTIRE grant set with the given
// snapshot (ADR-026(h), Variant A: SigilSnapshot is the single source of
// truth, applied as ReplaceAll). A grant missing from the snapshot is
// forgotten — this is near-instant revoke (S6c). An empty/nil snapshot →
// empty cache (no plugin granted).
//
// nil elements inside the snapshot are skipped (a corrupt payload shouldn't
// break the swap). Done under a single Lock: verify-phase readers (S6b) see
// either the entire old set or the entire new one, never an in-between state.
func (c *Cache) ReplaceAll(snapshot []*keeperv1.PluginSigil) {
	next := make(map[key]*keeperv1.PluginSigil, len(snapshot))
	for _, sig := range snapshot {
		if sig == nil {
			continue
		}
		next[key{namespace: sig.GetNamespace(), name: sig.GetName()}] = sig
	}
	c.mu.Lock()
	c.items = next
	c.mu.Unlock()
}

// Get returns the active Sigil for the (namespace, name) pair, or nil if
// there's no grant. Returns the stored pointer — callers must not mutate the
// PluginSigil (proto message is read-only after receipt).
func (c *Cache) Get(namespace, name string) *keeperv1.PluginSigil {
	k := key{namespace: namespace, name: name}
	c.mu.RLock()
	sig := c.items[k]
	c.mu.RUnlock()
	return sig
}
