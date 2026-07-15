package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// ScenariosTTL — validity window of a cached scenario listing for one Service.
// 60s mirrors the [RefsTTL] choice: same UX balance between "hammering the remote
// repo when opening the Run modal in the UI" vs. listing freshness (a scenario
// added to the repo a minute ago becomes visible to the operator within ≤60s).
const ScenariosTTL = 60 * time.Second

// ScenarioLister — surface for listing scenarios from a locally materialized
// snapshot of the Service repo (`scenario/*/main.yml`). Declared as an interface so
// tests can swap the real loader for a fake, and the handler depends on a minimal
// surface.
//
// Contract: invoked under a per-(name+ref) lock in [ScenariosCache]; the
// production implementation is [ScenariosLister.ListScenarios] over
// [artifact.ServiceLoader].
type ScenarioLister interface {
	ListScenarios(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error)
}

// ScenarioListerFunc — functional implementation of [ScenarioLister] (mirrors
// [artifact.RefsListerFunc] for handler-side wiring without a named type).
type ScenarioListerFunc func(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error)

// ListScenarios makes the function satisfy [ScenarioLister].
func (f ScenarioListerFunc) ListScenarios(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error) {
	return f(ctx, name, gitURL, ref)
}

// ScenariosCache — in-process TTL cache of [ScenarioLister.ListScenarios]
// responses keyed by `(name, ref)` (not `(name)`-only: one service can be queried
// with different refs — the UI dropdown shows scenarios for a specific version).
//
// Per-Keeper, not cluster-wide: scenarios are a read-only view, so lag between
// instances doesn't break registry consistency. A cluster-wide Redis cache is a
// separate slice for later (mirrors the [RefsCache] rationale).
//
// Safe for concurrent use. A per-key Mutex serializes "one in-flight loader per
// key" — parallel "Run scenario" clicks for the same (name,ref) don't hammer
// git-clone N times. The loader itself (`artifact.ServiceLoader`) also carries a
// per-name lock + reuses the snapshot directory by sha1 — but that's a different
// layer: the handler side needs its own short-circuit so it doesn't call the
// loader N times in a row for the same key.
type ScenariosCache struct {
	lister ScenarioLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[scenariosKey]*scenariosEntry
}

// scenariosKey — composite cache key. name+ref are stored separately for correct
// invalidation by name (Update/Deregister of a Service clears all ref variants
// under that name).
type scenariosKey struct {
	name string
	ref  string
}

// scenariosEntry — one cache entry. lock serializes concurrent loader calls for
// the same key; scenarios/expires is the cached response.
type scenariosEntry struct {
	lock      sync.Mutex
	scenarios []artifact.Scenario
	expires   time.Time
}

// NewScenariosCache builds a cache over a lister. lister is required (panics on
// nil — symmetric with [NewRefsCache]); ttl ≤ 0 is normalized to [ScenariosTTL].
func NewScenariosCache(lister ScenarioLister, ttl time.Duration) *ScenariosCache {
	if lister == nil {
		panic("serviceregistry.NewScenariosCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = ScenariosTTL
	}
	return &ScenariosCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[scenariosKey]*scenariosEntry),
	}
}

// ListScenarios returns scenarios for (name, gitURL, ref). Hit — served from the
// cache; miss or expired TTL — one loader call under the per-key lock.
//
// Return: either a successful []Scenario (may be empty — a service with no
// scenarios is valid), or the lister's error as-is — the caller (handler) maps it
// to 502 Bad Gateway. Only a success response is cached: on error, the next
// request retries the loader (best-effort + readable failures in the UI; mirrors
// [RefsCache.ListRefs] semantics).
func (c *ScenariosCache) ListScenarios(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error) {
	entry := c.entryFor(scenariosKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.scenarios != nil {
		return cloneScenarios(entry.scenarios), nil
	}

	scenarios, err := c.lister.ListScenarios(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.scenarios = scenarios
	entry.expires = c.now().Add(c.ttl)
	return cloneScenarios(scenarios), nil
}

// Invalidate clears all cache entries for the given name (all ref variants).
// Mirrors [RefsCache.Invalidate] semantics: after Update/Deregister of a Service,
// stale cached scenarios must disappear so the next request returns a listing from
// the new git source. Idempotent.
func (c *ScenariosCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor returns (creating if needed) the scenariosEntry for key. Doesn't hold
// c.mu during the loader call — that's handled by the per-key lock inside entry.
func (c *ScenariosCache) entryFor(key scenariosKey) *scenariosEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &scenariosEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneScenarios — shallow copy of the slice so the caller can't mutate the
// cached array. Scenario.InputSchema is a map (reference type); deliberately not
// deep-copied: the handler serializes it to JSON right away, and callers outside
// the handler never receive the cache.
func cloneScenarios(in []artifact.Scenario) []artifact.Scenario {
	if in == nil {
		return nil
	}
	out := make([]artifact.Scenario, len(in))
	copy(out, in)
	return out
}
