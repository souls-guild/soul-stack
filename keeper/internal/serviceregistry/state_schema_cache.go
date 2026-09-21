package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// StateSchemaTTL — validity window of a cached state-schema response for one
// Service. 60s — paired with the [ScenariosTTL] choice: the same UX balance between
// "hammering the remote repo on every UI Schema explorer open" vs. freshness (a new
// migration dropped into the repo a minute ago is visible to the operator within ≤60s).
const StateSchemaTTL = 60 * time.Second

// StateSchemaLister — surface for listing state_schema metadata
// (`state_schema_version` + optional structure declaration + migration chain) from
// a locally materialized snapshot of the Service repo. Declared as an interface
// so it can be swapped for a fake in handler tests; the production implementation
// is a function on top of [artifact.ServiceLoader] + [artifact.ListStateSchema].
//
// Contract: invoked under the per-(name+ref) lock in [StateSchemaCache]; ref is
// explicit because different versions of the same service can have a different
// state_schema_version (the UI Schema explorer shows the version of the selected ref).
type StateSchemaLister interface {
	ListStateSchema(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error)
}

// StateSchemaListerFunc — functional implementation of [StateSchemaLister]
// (paired with [ScenarioListerFunc] for handler-side wire-up without a wrapper
// named type).
type StateSchemaListerFunc func(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error)

// ListStateSchema makes the function implement [StateSchemaLister].
func (f StateSchemaListerFunc) ListStateSchema(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error) {
	return f(ctx, name, gitURL, ref)
}

// StateSchemaCache — in-process TTL cache for
// [StateSchemaLister.ListStateSchema] responses keyed by `(name, ref)`. Per-Keeper, not
// cluster-wide: state-schema is a read-only view, lag between instances doesn't
// break registry consistency (parity with [ScenariosCache]).
//
// Safe for concurrent use. A per-key Mutex serializes "one in-flight loader per
// key" — parallel clicks on "Open Schema explorer" don't hammer git-clone N times.
// At the loader level itself, [artifact.ServiceLoader] also carries a per-name
// lock + reuses the snapshot directory by sha1.
type StateSchemaCache struct {
	lister StateSchemaLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[stateSchemaKey]*stateSchemaEntry
}

// stateSchemaKey — composite cache key. name+ref are stored separately for
// correct invalidation by name (Update/Deregister of a Service resets all
// ref variants under that name; parity with [scenariosKey]).
type stateSchemaKey struct {
	name string
	ref  string
}

// stateSchemaEntry — one cache entry. lock serializes concurrent loader
// calls for one key; info/expires is the cached response.
type stateSchemaEntry struct {
	lock    sync.Mutex
	info    *artifact.StateSchemaInfo
	expires time.Time
}

// NewStateSchemaCache assembles a cache on top of a lister. lister is required
// (panics on nil — symmetric with [NewScenariosCache]); ttl ≤ 0 is normalized to
// [StateSchemaTTL].
func NewStateSchemaCache(lister StateSchemaLister, ttl time.Duration) *StateSchemaCache {
	if lister == nil {
		panic("serviceregistry.NewStateSchemaCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = StateSchemaTTL
	}
	return &StateSchemaCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[stateSchemaKey]*stateSchemaEntry),
	}
}

// ListStateSchema returns state-schema info for (name, gitURL, ref). Hit —
// serve from the cache; miss or expired TTL — one loader call under the
// per-key lock.
//
// ONLY a success response is cached: on error the next request retries the
// loader (best-effort + readable failures in the UI; parity with
// [ScenariosCache.ListScenarios]).
func (c *StateSchemaCache) ListStateSchema(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error) {
	entry := c.entryFor(stateSchemaKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.info != nil {
		return cloneStateSchemaInfo(entry.info), nil
	}

	info, err := c.lister.ListStateSchema(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.info = info
	entry.expires = c.now().Add(c.ttl)
	return cloneStateSchemaInfo(info), nil
}

// Invalidate resets all cache entries for the given name (all ref variants).
// Semantics are paired with [ScenariosCache.Invalidate]: after Update/Deregister
// of a Service, stale cached state-schema entries must disappear so the
// next request returns a listing from the new git source. Idempotent.
func (c *StateSchemaCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor returns (creating if needed) the stateSchemaEntry for key.
// Doesn't hold c.mu during the loader call — that's the job of the per-key
// lock inside entry.
func (c *StateSchemaCache) entryFor(key stateSchemaKey) *stateSchemaEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &stateSchemaEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneStateSchemaInfo — a shallow copy of the struct so the caller can't mutate
// the cached entry. Schema/Migrations are reference types; we copy the migrations
// slice (the handler serializes it to JSON and doesn't pass it beyond the cache);
// we deliberately do NOT deep-copy the Schema map (parity with cloneScenarios:
// the InputSchema map isn't cloned either — the UI only reads it).
func cloneStateSchemaInfo(in *artifact.StateSchemaInfo) *artifact.StateSchemaInfo {
	if in == nil {
		return nil
	}
	out := *in
	if in.Migrations != nil {
		out.Migrations = make([]artifact.Migration, len(in.Migrations))
		copy(out.Migrations, in.Migrations)
	}
	return &out
}
