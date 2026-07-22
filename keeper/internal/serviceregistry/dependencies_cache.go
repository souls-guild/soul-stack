package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// DependenciesTTL is the TTL window for a cached dependencies response of one
// Service. 60s matches [StateSchemaTTL] / [ScenariosTTL] choice: same UX
// balance of "jerking" remote repo on UI Service Detail open vs. freshness
// (operator sees changed `destiny:`/`modules:` block within ≤60s).
const DependenciesTTL = 60 * time.Second

// DependenciesLister is the surface for listing git dependencies (destiny/modules
// from `service.yml`) of one Service repo snapshot. Declared as interface for
// fake substitution in handler tests; production implementation is a function over
// [artifact.ServiceLoader] + [artifact.ListDependencies].
//
// Contract: called under per-(name+ref) lock in [DependenciesCache]; ref is
// explicit because different versions of one service may declare different
// dependencies (UI Service Detail shows dependencies of selected ref).
type DependenciesLister interface {
	ListDependencies(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error)
}

// DependenciesListerFunc is the functional implementation of [DependenciesLister]
// (paired [StateSchemaListerFunc] for handler-side wire-up without wrapper
// named type).
type DependenciesListerFunc func(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error)

// ListDependencies makes the function implement [DependenciesLister].
func (f DependenciesListerFunc) ListDependencies(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error) {
	return f(ctx, name, gitURL, ref)
}

// DependenciesCache is an in-process TTL cache of
// [DependenciesLister.ListDependencies] response by key `(name, ref)`. Per-Keeper, not
// cluster-wide: dependencies are read-only view, lag between
// instances does not break registry consistency (parity with [StateSchemaCache]).
//
// Safe for concurrent use. Per-key Mutex serializes "one
// in-flight loader per key"—parallel Service Detail opens do not
// loop git-clone N times. At the loader level [artifact.ServiceLoader] also
// carries per-name lock and reuses snapshot dir by sha1.
type DependenciesCache struct {
	lister DependenciesLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[dependenciesKey]*dependenciesEntry
}

// dependenciesKey is a composite cache key. name+ref are stored separately for
// correct invalidation by name (Service Update/Deregister drops all
// ref variants under that name; parity with [stateSchemaKey]).
type dependenciesKey struct {
	name string
	ref  string
}

// dependenciesEntry is one cache record. lock serializes concurrent loader
// calls for one key; deps/expires is the cached response.
type dependenciesEntry struct {
	lock    sync.Mutex
	deps    *artifact.ServiceDependencies
	expires time.Time
}

// NewDependenciesCache builds cache over the lister. lister is required (panic
// on nil—symmetrically [NewStateSchemaCache]); ttl ≤ 0 is normalized to
// [DependenciesTTL].
func NewDependenciesCache(lister DependenciesLister, ttl time.Duration) *DependenciesCache {
	if lister == nil {
		panic("serviceregistry.NewDependenciesCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = DependenciesTTL
	}
	return &DependenciesCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[dependenciesKey]*dependenciesEntry),
	}
}

// ListDependencies returns dependencies for (name, gitURL, ref). Hit—
// serve from cache; miss or expired TTL—one loader call under per-key lock.
//
// Only success response is cached: on error, next request tries
// loader again (best-effort + UI failure readability; parity with
// [StateSchemaCache.ListStateSchema]).
func (c *DependenciesCache) ListDependencies(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error) {
	entry := c.entryFor(dependenciesKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.deps != nil {
		return cloneDependencies(entry.deps), nil
	}

	deps, err := c.lister.ListDependencies(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.deps = deps
	entry.expires = c.now().Add(c.ttl)
	return cloneDependencies(deps), nil
}

// Invalidate drops all cache records for the given name (all ref variants).
// Semantics—paired with [StateSchemaCache.Invalidate]: after Service Update/Deregister
// stale cached dependencies must disappear so
// next request returns listing of new git source. Idempotent.
func (c *DependenciesCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor returns (creating if needed) dependenciesEntry for key.
// Does not hold c.mu during loader call—that is per-key lock work inside
// entry.
func (c *DependenciesCache) entryFor(key dependenciesKey) *dependenciesEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &dependenciesEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneDependencies is a shallow copy of the structure so caller cannot modify
// the cached record. Destiny/Modules are value slices (Dependency without
// pointer fields); we copy both (handler serializes to JSON and does not pass
// outside cache; parity with cloneStateSchemaInfo).
func cloneDependencies(in *artifact.ServiceDependencies) *artifact.ServiceDependencies {
	if in == nil {
		return nil
	}
	out := &artifact.ServiceDependencies{}
	if in.Destiny != nil {
		out.Destiny = make([]artifact.Dependency, len(in.Destiny))
		copy(out.Destiny, in.Destiny)
	}
	if in.Modules != nil {
		out.Modules = make([]artifact.Dependency, len(in.Modules))
		copy(out.Modules, in.Modules)
	}
	return out
}
