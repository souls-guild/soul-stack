package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// DirectivesTTL — validity window of a cached directive catalog for one
// `(name, ref)`. The catalog is immutable at a given git-ref snapshot, but a
// service's git URL can change under the same ref name (Update); 60s mirrors the
// ScenariosTTL/RefsTTL "freshness vs. hammering remote" balance.
const DirectivesTTL = 60 * time.Second

// DirectiveLister — surface for reading the FULL directive catalog (all series)
// from a materialized snapshot of the Service repo + the snapshot's SHA1 (for
// ETag). Version narrowing is done by the handler over the result
// (artifact.FilterDirectivesByVersion), so the cache is version-agnostic (keyed by
// (name,ref), like sibling caches).
type DirectiveLister interface {
	ListDirectives(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error)
}

// DirectiveListerFunc — functional implementation of [DirectiveLister] (mirrors
// ScenarioListerFunc for wiring without a named type).
type DirectiveListerFunc func(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error)

// ListDirectives makes the function satisfy [DirectiveLister].
func (f DirectiveListerFunc) ListDirectives(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error) {
	return f(ctx, name, gitURL, ref)
}

// DirectivesCache — in-process TTL cache of [DirectiveLister.ListDirectives]
// responses keyed by `(name, ref)`. Per-Keeper, not cluster-wide (read-only
// catalog, lag between instances doesn't break registry consistency). Safe for
// concurrent use; a per-key Mutex serializes "one in-flight loader per key"
// (parity with ScenariosCache).
type DirectivesCache struct {
	lister DirectiveLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[directivesKey]*directivesEntry
}

// directivesKey — composite cache key (name+ref kept separate for invalidation by name).
type directivesKey struct {
	name string
	ref  string
}

// directivesEntry — one cache entry: lock serializes concurrent loader calls for
// the same key; catalog/expires is the cached response.
type directivesEntry struct {
	lock    sync.Mutex
	catalog *artifact.DirectiveCatalog
	expires time.Time
}

// NewDirectivesCache builds a cache over a lister. lister is required (panics on
// nil — symmetric with NewScenariosCache); ttl ≤ 0 is normalized to
// [DirectivesTTL].
func NewDirectivesCache(lister DirectiveLister, ttl time.Duration) *DirectivesCache {
	if lister == nil {
		panic("serviceregistry.NewDirectivesCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = DirectivesTTL
	}
	return &DirectivesCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[directivesKey]*directivesEntry),
	}
}

// ListDirectives returns the full catalog for (name, gitURL, ref). Hit — served
// from the cache; miss/expired TTL — one loader call under the per-key lock. Only
// success is cached (errors aren't cached — the next request retries; parity with
// ScenariosCache).
func (c *DirectivesCache) ListDirectives(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error) {
	entry := c.entryFor(directivesKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.catalog != nil {
		return cloneDirectiveCatalog(entry.catalog), nil
	}

	catalog, err := c.lister.ListDirectives(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.catalog = catalog
	entry.expires = c.now().Add(c.ttl)
	return cloneDirectiveCatalog(catalog), nil
}

// Invalidate clears all cache entries for name (all ref variants). Mirrors
// ScenariosCache.Invalidate semantics: after Update/Deregister of a Service, the
// stale catalog disappears, and the next request returns the catalog from the new
// git source.
func (c *DirectivesCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor returns (creating if needed) the directivesEntry for key.
func (c *DirectivesCache) entryFor(key directivesKey) *directivesEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &directivesEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneDirectiveCatalog — shallow copy of the catalog (new outer map, name slices
// are shared): slices are read-only after loading (sorted, never mutated), so
// sharing is safe; copying the outer map protects the cache from mutation by the
// caller.
func cloneDirectiveCatalog(in *artifact.DirectiveCatalog) *artifact.DirectiveCatalog {
	if in == nil {
		return nil
	}
	out := &artifact.DirectiveCatalog{SHA1: in.SHA1}
	if in.Directives != nil {
		m := make(map[string][]string, len(in.Directives))
		for k, v := range in.Directives {
			m[k] = v
		}
		out.Directives = m
	}
	return out
}
