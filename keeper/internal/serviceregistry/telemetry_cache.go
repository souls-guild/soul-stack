package serviceregistry

import (
	"context"
	"sync"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TelemetryTTL - validity window of the cached telemetry config for one
// `(name, ref)`. The config is immutable at a git-ref snapshot, but the service's git URL can
// change under the same ref name (Update); 60s balances the paired DirectivesTTL.
const TelemetryTTL = 60 * time.Second

// TelemetryCatalog - snapshot result of the /telemetry lister: SHA1 of the materialized
// snapshot (serves as an ETag) + the effective per-service telemetry config (manifest
// defaults, without essence). Shape of the /telemetry lister result (parity DirectiveCatalog).
type TelemetryCatalog struct {
	SHA1      string
	Telemetry *keeperv1.TelemetryConfig
}

// TelemetryLister - a read surface for the default (per-service, without essence)
// telemetry config of a service + a snapshot SHA1 (for ETag) from the materialized
// Service repo snapshot for `(name, ref)`. Parity [DirectiveLister]. When nil,
// `GET /v1/services/{name}/telemetry` responds 500 "not configured".
type TelemetryLister interface {
	ListServiceTelemetry(ctx context.Context, name, gitURL, ref string) (*TelemetryCatalog, error)
}

// TelemetryListerFunc - a functional implementation of [TelemetryLister] (parity
// DirectiveListerFunc for wire-up without a named type).
type TelemetryListerFunc func(ctx context.Context, name, gitURL, ref string) (*TelemetryCatalog, error)

// ListServiceTelemetry makes the function implement [TelemetryLister].
func (f TelemetryListerFunc) ListServiceTelemetry(ctx context.Context, name, gitURL, ref string) (*TelemetryCatalog, error) {
	return f(ctx, name, gitURL, ref)
}

// TelemetryCache - an in-process TTL cache of the [TelemetryLister.ListServiceTelemetry]
// response keyed by `(name, ref)`. Per-Keeper, not cluster-wide (read-only catalog). Safe
// for concurrent use; a per-key Mutex serializes "one in-flight loader
// per key" (parity DirectivesCache).
type TelemetryCache struct {
	lister TelemetryLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[telemetryKey]*telemetryEntry
}

// telemetryKey - a composite cache key (name+ref separate for invalidation by name).
type telemetryKey struct {
	name string
	ref  string
}

// telemetryEntry - one cache entry: lock serializes the concurrent loader for one
// key; catalog/expires is the cached response.
type telemetryEntry struct {
	lock    sync.Mutex
	catalog *TelemetryCatalog
	expires time.Time
}

// NewTelemetryCache assembles a cache on top of a lister. lister is required (panics on
// nil - symmetric with NewDirectivesCache); ttl <= 0 is normalized to [TelemetryTTL].
func NewTelemetryCache(lister TelemetryLister, ttl time.Duration) *TelemetryCache {
	if lister == nil {
		panic("serviceregistry.NewTelemetryCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = TelemetryTTL
	}
	return &TelemetryCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[telemetryKey]*telemetryEntry),
	}
}

// ListServiceTelemetry returns the telemetry config for (name, gitURL, ref). A hit comes from
// the cache; a miss/expired TTL triggers one loader call under the per-key lock. Only
// success is cached (errors are not cached; parity DirectivesCache).
func (c *TelemetryCache) ListServiceTelemetry(ctx context.Context, name, gitURL, ref string) (*TelemetryCatalog, error) {
	entry := c.entryFor(telemetryKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.catalog != nil {
		return cloneTelemetryCatalog(entry.catalog), nil
	}

	catalog, err := c.lister.ListServiceTelemetry(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.catalog = catalog
	entry.expires = c.now().Add(c.ttl)
	return cloneTelemetryCatalog(catalog), nil
}

// Invalidate clears all cache entries for name (all ref variants). Paired
// semantics with DirectivesCache.Invalidate: after a Service Update/Deregister,
// the stale config disappears and the next request returns the config from the new git source.
func (c *TelemetryCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor returns (creating if needed) the telemetryEntry for key.
func (c *TelemetryCache) entryFor(key telemetryKey) *telemetryEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &telemetryEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneTelemetryCatalog - a deep copy of the catalog (new proto-message + copy of the
// Collectors slice): protects the cache from caller mutation. Scalar fields are by value.
func cloneTelemetryCatalog(in *TelemetryCatalog) *TelemetryCatalog {
	if in == nil {
		return nil
	}
	out := &TelemetryCatalog{SHA1: in.SHA1}
	if in.Telemetry != nil {
		out.Telemetry = &keeperv1.TelemetryConfig{
			Enabled:     in.Telemetry.Enabled,
			IntervalSec: in.Telemetry.IntervalSec,
			Collectors:  append([]string(nil), in.Telemetry.Collectors...),
		}
	}
	return out
}
