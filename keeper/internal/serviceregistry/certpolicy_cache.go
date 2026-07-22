package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// CertPolicyTTL — the validity window of a cached cert-policy response for one
// Service (paired with the [StateSchemaTTL] choice: the same balance between
// "hammering the remote repo" vs. freshness of a certificate_rotation section
// that landed in the manifest a minute ago).
const CertPolicyTTL = 60 * time.Second

// CertPolicyLister — the listing surface for cert-rotation policy
// (`certificate_rotation:` + scenario/ names) from a locally materialized
// snapshot of the Service repo. Interface — for swapping with a fake in tests;
// production — [artifact.ServiceLoader.LoadCertPolicy].
//
// Contract: invoked under a per-(name+ref) lock; ref is explicit because
// different service versions can have different rotation policies.
type CertPolicyLister interface {
	ListCertPolicy(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error)
}

// CertPolicyListerFunc — a functional implementation of [CertPolicyLister]
// (paired with [StateSchemaListerFunc] for handler-side wire-up).
type CertPolicyListerFunc func(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error)

// ListCertPolicy makes the function satisfy [CertPolicyLister].
func (f CertPolicyListerFunc) ListCertPolicy(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error) {
	return f(ctx, name, gitURL, ref)
}

// CertPolicyCache — an in-process TTL cache of [CertPolicyLister.ListCertPolicy]
// responses keyed by `(name, ref)`. Per-Keeper (parity with [StateSchemaCache]):
// cert-policy is a read-only view, lag between instances doesn't break consistency.
//
// Safe for concurrent use. A per-key Mutex serializes "one in-flight loader per key".
type CertPolicyCache struct {
	lister CertPolicyLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[certPolicyKey]*certPolicyEntry
}

// certPolicyKey — a composite cache key (name+ref kept separate to allow
// invalidation by name; parity with [stateSchemaKey]).
type certPolicyKey struct {
	name string
	ref  string
}

// certPolicyEntry — one cache entry: the lock serializes concurrent loader
// calls for the same key; info/expires — the cached response.
type certPolicyEntry struct {
	lock    sync.Mutex
	info    *artifact.CertPolicyInfo
	expires time.Time
}

// NewCertPolicyCache builds a cache on top of a lister. lister is required
// (panics on nil; parity with [NewStateSchemaCache]); ttl <= 0 is normalized to
// [CertPolicyTTL].
func NewCertPolicyCache(lister CertPolicyLister, ttl time.Duration) *CertPolicyCache {
	if lister == nil {
		panic("serviceregistry.NewCertPolicyCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = CertPolicyTTL
	}
	return &CertPolicyCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[certPolicyKey]*certPolicyEntry),
	}
}

// ListCertPolicy returns cert-policy info for (name, gitURL, ref). Hit — served
// from the cache; miss/expired TTL — a single loader call under the per-key lock.
//
// Only a success response is cached (parity with [StateSchemaCache.ListStateSchema]).
func (c *CertPolicyCache) ListCertPolicy(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error) {
	entry := c.entryFor(certPolicyKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.info != nil {
		return cloneCertPolicyInfo(entry.info), nil
	}

	info, err := c.lister.ListCertPolicy(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.info = info
	entry.expires = c.now().Add(c.ttl)
	return cloneCertPolicyInfo(info), nil
}

// Invalidate drops all cache entries for the given name (all ref variants).
// Idempotent; parity with [StateSchemaCache.Invalidate].
func (c *CertPolicyCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor returns (creating if necessary) the certPolicyEntry for key.
func (c *CertPolicyCache) entryFor(key certPolicyKey) *certPolicyEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &certPolicyEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneCertPolicyInfo — a copy so the caller can't mutate the cached entry:
// Scenarios — a slice copy, Rotation — a deep copy of the pointer (otherwise a
// shared *Rotation is a latent race if a writer ever appears).
func cloneCertPolicyInfo(in *artifact.CertPolicyInfo) *artifact.CertPolicyInfo {
	if in == nil {
		return nil
	}
	out := *in
	if in.Scenarios != nil {
		out.Scenarios = make([]string, len(in.Scenarios))
		copy(out.Scenarios, in.Scenarios)
	}
	if in.Rotation != nil {
		r := *in.Rotation
		out.Rotation = &r
	}
	return &out
}
