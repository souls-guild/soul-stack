package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// CompatTTL is the TTL window for a cached engine-compat window of one Service.
// 60s matches [DependenciesTTL] / [TelemetryTTL]: the same UX balance of "don't
// re-clone the destiny repos on every Service Detail open" vs. freshness (an
// operator sees a changed `compat:` block within <=60s).
const CompatTTL = 60 * time.Second

// CompatCatalog — the engine-compat contributions of one Service snapshot
// (ADR-0076(h)): the service manifest's declared window plus one entry per
// `destiny[]` dependency, each at its own pinned ref. The effective window is
// the intersection ([config.IntersectKeeperWindows]) — computed by the handler,
// not stored, so the view and the render-path gate cannot drift.
//
// Entities is service-first, then destinies in manifest order (a stable display
// order). SHA1 is the service snapshot commit (the ETag source).
type CompatCatalog struct {
	SHA1     string
	Entities []config.CompatEntity
}

// CompatLister — read surface for the compat contributions of one Service repo
// snapshot at `(name, ref)`. Declared as an interface so the handler can be
// tested without git; the production implementation is a function over
// [artifact.ServiceLoader] + the destiny loader (see daemon.setupScenarioDeps).
// nil → `GET /v1/services/{id}/compat` answers 500 "not configured".
type CompatLister interface {
	ListServiceCompat(ctx context.Context, name, gitURL, ref string) (*CompatCatalog, error)
}

// CompatListerFunc — functional implementation of [CompatLister] (parity with
// [TelemetryListerFunc]: wire-up without a named wrapper type).
type CompatListerFunc func(ctx context.Context, name, gitURL, ref string) (*CompatCatalog, error)

// ListServiceCompat makes the function implement [CompatLister].
func (f CompatListerFunc) ListServiceCompat(ctx context.Context, name, gitURL, ref string) (*CompatCatalog, error) {
	return f(ctx, name, gitURL, ref)
}

// CompatCache — in-process TTL cache of [CompatLister.ListServiceCompat] by key
// `(name, ref)`. Per-Keeper, not cluster-wide: the window is a read-only view of
// git artifacts, so lag between instances breaks no registry consistency (parity
// with [DependenciesCache]).
//
// Safe for concurrent use. The per-key Mutex keeps one in-flight loader per key —
// parallel Service Detail opens do not clone the destiny repos N times.
type CompatCache struct {
	lister CompatLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[compatKey]*compatEntry
}

// compatKey — composite cache key. name+ref are stored separately so
// invalidation by name drops every ref variant (parity with [dependenciesKey]).
type compatKey struct {
	name string
	ref  string
}

// compatEntry — one cache record; lock serializes concurrent loader calls for
// the key.
type compatEntry struct {
	lock    sync.Mutex
	catalog *CompatCatalog
	expires time.Time
}

// NewCompatCache builds the cache over the lister. lister is required (panic on
// nil — a wire-up bug, symmetric with [NewDependenciesCache]); ttl <= 0 is
// normalized to [CompatTTL].
func NewCompatCache(lister CompatLister, ttl time.Duration) *CompatCache {
	if lister == nil {
		panic("serviceregistry.NewCompatCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = CompatTTL
	}
	return &CompatCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[compatKey]*compatEntry),
	}
}

// ListServiceCompat returns the compat contributions for (name, gitURL, ref).
// Hit — served from cache; miss or expired TTL — one loader call under the
// per-key lock. Only a success is cached: on error the next request retries
// (parity with [DependenciesCache.ListDependencies]).
func (c *CompatCache) ListServiceCompat(ctx context.Context, name, gitURL, ref string) (*CompatCatalog, error) {
	entry := c.entryFor(compatKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.catalog != nil {
		return cloneCompatCatalog(entry.catalog), nil
	}

	catalog, err := c.lister.ListServiceCompat(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.catalog = catalog
	entry.expires = c.now().Add(c.ttl)
	return cloneCompatCatalog(catalog), nil
}

// Invalidate drops every record for the given name (all ref variants) after a
// Service Update/Deregister. Idempotent (parity with
// [DependenciesCache.Invalidate]).
func (c *CompatCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor returns (creating if needed) the compatEntry for key. c.mu is not
// held across the loader call — that is the per-key lock's job.
func (c *CompatCache) entryFor(key compatKey) *compatEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &compatEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneCompatCatalog — deep-enough copy so a caller cannot mutate the cached
// record: the entity slice is copied and each Window pointer is re-allocated
// (a shared *VersionWindow would let one caller's edit reach every later
// response; parity with cloneDependencies).
func cloneCompatCatalog(in *CompatCatalog) *CompatCatalog {
	if in == nil {
		return nil
	}
	out := &CompatCatalog{SHA1: in.SHA1}
	if in.Entities == nil {
		return out
	}
	out.Entities = make([]config.CompatEntity, len(in.Entities))
	copy(out.Entities, in.Entities)
	for i := range out.Entities {
		if w := out.Entities[i].Window; w != nil {
			out.Entities[i].Window = &config.VersionWindow{Min: w.Min, Max: w.Max}
		}
	}
	return out
}
