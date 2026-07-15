package redis

// Conclave is the registry of live cluster Keeper instances in Redis
// (ADR-006 amend, soul-shedding S1). Each instance registers its own
// presence record `keeper:instance:<kid>` with a TTL on startup and renews it
// via a renewal goroutine; on graceful shutdown it deletes the record, on
// crash the record expires by TTL. Enumerating live instances ([LiveKIDs] /
// [CountLive]) is a SCAN over the prefix.
//
// Difference from [Lease]/[SoulLease]: this is NOT an exclusive lock. Each
// instance holds its OWN key (by its own KID), so there's no contention over
// a single key — registration happens without NX. The optional NX check
// ([RegisterInstance]) catches a KID collision (two keeper processes with the
// same `kid` in their config = an operator error) and is logged as a warning,
// not a blocking error: presence for one's own KID is an invariant, not a
// fight for leadership.
//
// Presence is authoritative in Redis (the presence→Redis invariant): a PG
// variant was rejected (presence is volatile, TTL+renew is the natural
// model). The TTL keys themselves are the single source of truth, with no
// parallel Redis Set (mirroring how [SoulsStreamAlive] rejected a separate
// Set of live SIDs): a few dozen instances per cluster makes SCAN cheap.
//
// Feeds (S2/S3, separate slices): the "I'm not alone" refuse-guard
// (CountLive > 1) and soul-shedding (is there somewhere to go — LiveKIDs
// minus one's own KID).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// conclaveKeyPrefix is the prefix for Conclave presence keys. The key
	// name is technical (like `soul:<sid>:lock`), the dictionary entity is
	// Conclave.
	conclaveKeyPrefix = "keeper:instance:"

	// DefaultConclaveTTL / DefaultConclaveRenewInterval — the presence key's
	// TTL and its renewal period. TTL ≈ 3×renew to ride out brief GC pauses /
	// Renew latency spikes (the same headroom as SoulLease).
	DefaultConclaveTTL           = 30 * time.Second
	DefaultConclaveRenewInterval = 10 * time.Second
)

// ErrConclaveKIDTaken is returned when [RegisterInstance] with
// requireUnique=true finds that the `keeper:instance:<kid>` key already
// exists. Means a KID collision (two keeper processes with the same `kid` in
// their config) — an operator configuration error. The caller logs a warning
// and proceeds with registration (presence for one's own KID is an
// invariant).
var ErrConclaveKIDTaken = errors.New("redis: conclave instance key already exists (KID collision)")

// ConclaveKey builds the Redis presence key for a specific KID.
func ConclaveKey(kid string) string {
	return conclaveKeyPrefix + kid
}

// RegisterInstance writes the keeper instance's presence record
// `keeper:instance:<kid>` with TTL `ttl` and value `meta` (lightweight
// diagnostic metadata — JSON / KID, the caller builds it).
//
// requireUnique=true first checks the key is absent (NX semantics via
// `SET NX`): on a KID collision it returns [ErrConclaveKIDTaken] WITHOUT
// overwriting — so the caller can log a warning. requireUnique=false (the
// normal restart path: the same KID after a crash, its own stale TTL key
// hasn't expired yet) does an unconditional SET, overwriting any leftover of
// its own.
//
// `ttl` must be > 0 (like [Acquire]): a zero/negative TTL is a caller error.
func RegisterInstance(ctx context.Context, c *Client, kid, meta string, ttl time.Duration, requireUnique bool) error {
	if c == nil {
		return errors.New("redis.RegisterInstance: nil client")
	}
	if kid == "" {
		return errors.New("redis.RegisterInstance: empty kid")
	}
	if ttl <= 0 {
		return fmt.Errorf("redis.RegisterInstance: ttl must be > 0, got %v", ttl)
	}
	key := ConclaveKey(kid)

	if requireUnique {
		ok, err := c.underlying().SetNX(ctx, key, meta, ttl).Result()
		if err != nil {
			return fmt.Errorf("redis.RegisterInstance: SETNX %q: %w", key, err)
		}
		if !ok {
			return ErrConclaveKIDTaken
		}
		return nil
	}

	if err := c.underlying().Set(ctx, key, meta, ttl).Err(); err != nil {
		return fmt.Errorf("redis.RegisterInstance: SET %q: %w", key, err)
	}
	return nil
}

// RenewInstance extends the TTL of the presence key `keeper:instance:<kid>`
// to `ttl` (PEXPIRE on an existing key). Unlike [Lease.Renew] there's no
// holder CAS here: the key belongs to this instance by construction (one KID
// — one process), so there's no contention over it.
//
// If the key is gone (expired from missed renewals / was deleted), PEXPIRE
// returns 0; the renewal goroutine re-creates presence from scratch
// ([RegisterInstance]) to self-heal, rather than silently becoming invisible
// to the cluster (restart-safe semantics). "Key exists, TTL extended" means
// ok=true.
func RenewInstance(ctx context.Context, c *Client, kid string, ttl time.Duration) (ok bool, err error) {
	if c == nil {
		return false, errors.New("redis.RenewInstance: nil client")
	}
	if kid == "" {
		return false, errors.New("redis.RenewInstance: empty kid")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("redis.RenewInstance: ttl must be > 0, got %v", ttl)
	}
	res, err := c.underlying().PExpire(ctx, ConclaveKey(kid), ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis.RenewInstance: PEXPIRE %q: %w", ConclaveKey(kid), err)
	}
	return res, nil
}

// DeregisterInstance removes the presence key `keeper:instance:<kid>`
// (graceful shutdown). Idempotent: a missing key is a no-op (DEL returns 0).
// A network error is propagated, but the caller usually ignores it — the
// call happens during shutdown cleanup, where Redis may already be
// unreachable (falls back to crash/TTL-expiry).
func DeregisterInstance(ctx context.Context, c *Client, kid string) error {
	if c == nil {
		return errors.New("redis.DeregisterInstance: nil client")
	}
	if kid == "" {
		return errors.New("redis.DeregisterInstance: empty kid")
	}
	if err := c.underlying().Del(ctx, ConclaveKey(kid)).Err(); err != nil {
		return fmt.Errorf("redis.DeregisterInstance: DEL %q: %w", ConclaveKey(kid), err)
	}
	return nil
}

// LiveKIDs enumerates the KIDs of live keeper instances — a SCAN over the
// `keeper:instance:*` prefix with the prefix trimmed off. A dead instance
// (crashed without Deregister) drops out of the result once its key hits
// TTL-expiry.
//
// SCAN (not KEYS) is a non-blocking cursor: KEYS in production blocks Redis
// for the entire keyspace walk. A few dozen instances means one or two
// cursor passes, cheap. count=100 is a per-iteration batch-size hint (Redis
// is free to return more or less). Duplicate KIDs across SCAN batches
// (possible during rehash) are collapsed via a set.
//
// Cluster mode (ADR-006 amendment): presence keys for different KIDs land in
// DIFFERENT slots (no shared hash-tag — intentional: otherwise the whole
// presence keyspace would pile onto one node). A plain SCAN on
// `*redis.ClusterClient` only covers ONE node → undercounts presence →
// silently breaks the "I'm not alone" refuse-guard (ADR-027) and
// soul-shedding. So in cluster mode SCAN runs per-master via
// [ClusterClient.ForEachMaster] and results are merged (dedup via the same
// seen-set). The type-switch is on underlying()'s concrete type.
func LiveKIDs(ctx context.Context, c *Client) ([]string, error) {
	if c == nil {
		return nil, errors.New("redis.LiveKIDs: nil client")
	}
	// seen deduplicates KIDs: across SCAN batches (rehash) and across cluster
	// master nodes. mu guards the update under concurrent ForEachMaster.
	var mu sync.Mutex
	seen := make(map[string]struct{})
	var kids []string
	collect := func(kid string) {
		if kid == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, dup := seen[kid]; dup {
			return
		}
		seen[kid] = struct{}{}
		kids = append(kids, kid)
	}

	if cc, ok := c.underlying().(*redis.ClusterClient); ok {
		// SCAN on every master node in the cluster: keys for different KIDs
		// are sharded across slots, and one node sees only its share.
		// ForEachMaster calls fn concurrently across nodes — collect is
		// synchronized via mu.
		err := cc.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			kidsOnNode, err := scanKIDs(ctx, node)
			if err != nil {
				return err
			}
			for _, kid := range kidsOnNode {
				collect(kid)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("redis.LiveKIDs: ForEachMaster SCAN: %w", err)
		}
		return kids, nil
	}

	nodeKIDs, err := scanKIDs(ctx, c.underlying())
	if err != nil {
		return nil, fmt.Errorf("redis.LiveKIDs: SCAN: %w", err)
	}
	for _, kid := range nodeKIDs {
		collect(kid)
	}
	return kids, nil
}

// scanKIDs runs a cursor SCAN over `keeper:instance:*` on ONE node and
// returns the trimmed KIDs (possibly with duplicates across batches — dedup
// is the caller's job). `s` is either a UniversalClient (the whole cluster)
// or a `*redis.Client` (a single master node from ForEachMaster).
func scanKIDs(ctx context.Context, s redis.Cmdable) ([]string, error) {
	var out []string
	var cursor uint64
	for {
		keys, next, err := s.Scan(ctx, cursor, conclaveKeyPrefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			out = append(out, k[len(conclaveKeyPrefix):])
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

// ReadInstanceMeta reads the value of the presence key `keeper:instance:<kid>`
// — lightweight instance metadata (a `{started_at, kid}` JSON written by the
// caller in [RegisterInstance]). Returns (meta, true) if the key is alive;
// (_, false) if the instance is dead / the key has expired (`redis.Nil`).
// Feeds `GET /v1/cluster`: the list of live KIDs ([LiveKIDs]) plus each one's
// started_at.
//
// meta is returned as a raw string — parsing (JSON→started_at) is the
// caller's job (the handler); the storage layer doesn't impose a value
// shape (a fail-safe RegisterInstance could have written a bare KID instead
// of JSON — the caller must be ready for that).
func ReadInstanceMeta(ctx context.Context, c *Client, kid string) (string, bool, error) {
	if c == nil {
		return "", false, errors.New("redis.ReadInstanceMeta: nil client")
	}
	if kid == "" {
		return "", false, errors.New("redis.ReadInstanceMeta: empty kid")
	}
	v, err := c.underlying().Get(ctx, ConclaveKey(kid)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("redis.ReadInstanceMeta: GET %q: %w", ConclaveKey(kid), err)
	}
	return v, true, nil
}

// CountLive returns the number of live keeper instances (= len([LiveKIDs])).
// Feeds the "I'm not alone" refuse-guard (CountLive > 1, S3) — a separate
// helper so a caller that only needs the count doesn't allocate a KID slice.
func CountLive(ctx context.Context, c *Client) (int, error) {
	kids, err := LiveKIDs(ctx, c)
	if err != nil {
		return 0, err
	}
	return len(kids), nil
}

// InstanceAlive is a point presence check for a single KID: is the keeper
// instance alive right now (EXISTS on its presence key
// `keeper:instance:<kid>`). The record exists only as long as the instance's
// renewal goroutine keeps extending it; it disappears on graceful shutdown
// ([DeregisterInstance]) or by TTL-expiry after a crash.
//
// Unlike [LiveKIDs]/[CountLive] (SCAN over the whole registry), this is a
// single EXISTS: the caller knows the specific KID and only asks about that
// one. Feeds recovery detection of "the run/stream owner is provably dead"
// (ADR-027 amend (m) — reconcile_orphan_applying presence-gate; amend (n) —
// force-release the SID-lease from a dead prev-holder). Symmetric with
// [SoulStreamAlive] (EXISTS on the SID-lease key), but a different keyspace —
// keeper-instance presence, not Souls.
//
// The caller treats an error return (network/protocol failure of EXISTS)
// fail-safe: unknown means do NOT declare it dead (don't reclaim the lock /
// don't release the lease), so a live run isn't disrupted by a Redis blip.
func InstanceAlive(ctx context.Context, c *Client, kid string) (bool, error) {
	if c == nil {
		return false, errors.New("redis.InstanceAlive: nil client")
	}
	if kid == "" {
		return false, errors.New("redis.InstanceAlive: empty kid")
	}
	n, err := c.underlying().Exists(ctx, ConclaveKey(kid)).Result()
	if err != nil {
		return false, fmt.Errorf("redis.InstanceAlive: EXISTS %q: %w", ConclaveKey(kid), err)
	}
	return n > 0, nil
}
