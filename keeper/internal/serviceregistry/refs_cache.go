package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// RefsTTL — validity window of a cached git-ls-remote response. Matches the spec
// recommendation: 60s balances "hammering" the remote repo when opening the
// Upgrade modal in the UI against acceptable freshness of the ref list (a tag
// added a minute ago becomes visible to the operator within ≤60s of a page
// refresh).
const RefsTTL = 60 * time.Second

// RefsCache — in-process TTL cache of [artifact.ListRefs] responses keyed by
// Service name (not git URL: name is more stable — renaming the git source in the
// registry shouldn't inherit the old repo's refs).
//
// Per-Keeper, not cluster-wide: refs are a read-only view, so lag between
// instances doesn't break registry consistency. If one keeper in the cluster has
// already fetched refs, the others will do their own ls-remote on their first
// request — negligible traffic to the git source (the UI dropdown is opened
// rarely). A cluster-wide Redis cache is a separate slice for later.
//
// Safe for concurrent use. A per-name Mutex serializes "one in-flight ls-remote
// per service" — parallel "open Upgrade" clicks for the same service don't hammer
// the same ls-remote N times.
type RefsCache struct {
	lister artifact.RefsLister
	ttl    time.Duration
	now    func() time.Time // for tests

	mu      sync.Mutex
	entries map[string]*refsEntry
}

// refsEntry — one cache entry. lock serializes concurrent ls-remote calls for the
// same service; refs/expires is the cached response (written under the lock,
// read atomically after Lock/Unlock).
type refsEntry struct {
	lock    sync.Mutex
	refs    []artifact.GitRef
	expires time.Time
}

// NewRefsCache builds a cache over a lister. lister is required (panics on nil —
// the only misconfiguration point); ttl ≤0 is normalized to [RefsTTL].
func NewRefsCache(lister artifact.RefsLister, ttl time.Duration) *RefsCache {
	if lister == nil {
		panic("serviceregistry.NewRefsCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = RefsTTL
	}
	return &RefsCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]*refsEntry),
	}
}

// ListRefs returns refs for (name, gitURL). If there's a valid cache entry,
// return it immediately. Otherwise, one ls-remote through the lister under the
// per-name lock (other goroutines for the same name wait for the result).
//
// name is the cache key; gitURL is the parameter passed to the lister (the
// handler reads both from the registry entry; we don't re-validate them here —
// that's the service layer's job).
//
// Return: either a successful []GitRef (may be empty), or the lister's error
// as-is — the caller (handler) maps it to 502 Bad Gateway. Only a success
// response is cached: on error, the next request retries ls-remote (best-effort +
// readable failures in the UI).
func (c *RefsCache) ListRefs(ctx context.Context, name, gitURL string) ([]artifact.GitRef, error) {
	entry := c.entryFor(name)

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.refs != nil {
		return cloneRefs(entry.refs), nil
	}

	refs, err := c.lister.ListRefs(ctx, gitURL)
	if err != nil {
		return nil, err
	}
	entry.refs = refs
	entry.expires = c.now().Add(c.ttl)
	return cloneRefs(refs), nil
}

// Invalidate clears the cache for name (after a registry Update/Deregister, the
// stale entry should be dropped so the next request returns refs from the new git
// source). Idempotent: no entry — no-op.
func (c *RefsCache) Invalidate(name string) {
	c.mu.Lock()
	delete(c.entries, name)
	c.mu.Unlock()
}

// entryFor returns (creating if needed) the refsEntry for name. Doesn't hold c.mu
// during ls-remote — that's handled by the per-name lock inside entry.
func (c *RefsCache) entryFor(name string) *refsEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[name]
	if !ok {
		e = &refsEntry{}
		c.entries[name] = e
	}
	return e
}

// cloneRefs makes a shallow copy of the slice so the caller can't mutate the
// cached array (GitRef is a value type, deep-copy isn't needed).
func cloneRefs(in []artifact.GitRef) []artifact.GitRef {
	if in == nil {
		return nil
	}
	out := make([]artifact.GitRef, len(in))
	copy(out, in)
	return out
}
