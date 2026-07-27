package console

import (
	"context"
	"log/slog"
	"sync"
	"time"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ClusterBridge carries console output across Keeper instances.
//
// The two connections of a console land independently in a stateless cluster:
// the operator's socket on whichever Keeper the load balancer chose, the host's
// EventStream on whichever won the SoulLease. Downstream already routes itself
// (Outbound → `outbound:<sid>`); this is the upstream half.
//
// Both roles live in one type because one instance is usually both at once —
// owner of some sockets, holder of some streams:
//
//   - as OWNER: claims `console:owner:<session>` so holders can find it, and
//     consumes its own `console:<kid>` channel;
//   - as HOLDER: resolves the owner of a session it does not know locally and
//     republishes the frame to that owner's channel.
//
// The owner lookup is cached per session, so the Redis round-trip happens once
// per console rather than once per pty chunk.
type ClusterBridge struct {
	redis  *keeperredis.Client
	kid    string
	logger *slog.Logger

	mu sync.RWMutex
	// ownerOf caches session -> where it is bridged, for sessions whose stream
	// this instance holds but whose socket lives elsewhere. Bounded by the
	// number of bridged sessions and pruned on their terminal frame.
	ownerOf map[string]bridgedSession
	// notFound remembers sessions with no claim, so a late-arriving chunk for a
	// dead session does not re-query Redis on every frame.
	notFound map[string]time.Time
	// reaped remembers orphans this instance already sent a ConsoleClose for,
	// so a stream of trailing output produces one reap, not one per chunk.
	reaped map[string]time.Time
}

// bridgedSession is what the holder remembers about a session it forwards: who
// owns the socket, and which host the pty is on — the latter is what makes a
// reap addressable, since an upstream frame carries only the session id.
type bridgedSession struct {
	ownerKID string
	sid      string
}

// negativeOwnerTTL is how long a "no claim" answer is trusted. Short enough
// that an owner claiming slightly late is still found, long enough that a flood
// of trailing chunks for a dead session costs one lookup, not thousands.
const negativeOwnerTTL = 5 * time.Second

// orphanReapCooldown bounds how often one orphaned session may be reaped again.
// The reap is idempotent Soul-side, but pty output arrives as a stream and one
// ConsoleClose per chunk would be pure noise on the shared EventStream.
const orphanReapCooldown = 30 * time.Second

// NewClusterBridge builds the bridge. A nil redis client yields a nil bridge —
// the caller treats that as single-instance mode.
func NewClusterBridge(rdb *keeperredis.Client, kid string, logger *slog.Logger) *ClusterBridge {
	if rdb == nil || kid == "" || logger == nil {
		return nil
	}
	return &ClusterBridge{
		redis:    rdb,
		kid:      kid,
		logger:   logger,
		ownerOf:  make(map[string]bridgedSession),
		notFound: make(map[string]time.Time),
		reaped:   make(map[string]time.Time),
	}
}

// ClaimSession publishes this instance as the owner of a session's socket.
// Best-effort: a failed claim degrades that one console to same-instance
// operation, which is exactly the pre-bridge behaviour, so it must not fail the
// open.
func (b *ClusterBridge) ClaimSession(ctx context.Context, sessionID string) {
	if b == nil {
		return
	}
	if err := keeperredis.ClaimConsoleSession(ctx, b.redis, sessionID, b.kid); err != nil {
		b.logger.Warn("console: cluster claim failed — session limited to this instance",
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
	}
}

// RefreshClaims re-stamps the TTL of the given sessions' claims. Called on a
// ticker by the socket owner: a console outliving [keeperredis.ConsoleOwnerTTL]
// would otherwise become unroutable to a holder that has not cached it yet.
func (b *ClusterBridge) RefreshClaims(ctx context.Context, sessionIDs []string) {
	if b == nil {
		return
	}
	for _, id := range sessionIDs {
		if err := keeperredis.ClaimConsoleSession(ctx, b.redis, id, b.kid); err != nil {
			b.logger.Debug("console: cluster claim refresh failed",
				slog.String("session_id", id), slog.Any("error", err))
		}
	}
}

// ReleaseSession drops the claim and forgets any cached routing for a session
// that has ended.
func (b *ClusterBridge) ReleaseSession(ctx context.Context, sessionID string) {
	if b == nil {
		return
	}
	b.forget(sessionID)
	if err := keeperredis.ReleaseConsoleSession(ctx, b.redis, sessionID); err != nil {
		b.logger.Debug("console: cluster claim release failed (it expires on its own)",
			slog.String("session_id", sessionID), slog.Any("error", err))
	}
}

// Forward republishes an upstream frame to the Keeper that owns the session's
// socket. Called only for sessions this instance does not hold locally.
//
// Returns ownerGone=true when the session provably has NO live owner — either
// no claim exists, or the claimed instance is not subscribed to its channel.
// That is the orphan signal: this Keeper holds the EventStream, so the pty is
// alive on the host with nobody left watching it, and the caller must reap it.
//
// A transient Redis failure returns false: it is indistinguishable from a
// healthy owner behind a flaky cache, and killing an operator's live shell over
// a blip would be far worse than one dropped chunk.
func (b *ClusterBridge) Forward(ctx context.Context, sid, sessionID string, msg *keeperv1.FromSoul) (ownerGone bool) {
	if b == nil {
		return false
	}
	owner, found, err := b.resolveOwner(ctx, sid, sessionID)
	if err != nil {
		return false // transient — say nothing about the owner
	}
	if !found {
		// No claim at all: the owner released it, or never made one and has
		// since died. Either way nobody is receiving this session.
		return true
	}
	if owner == b.kid {
		// Our own claim, but no local session: the socket closed between the
		// claim and this frame. Dropping is right — there is nobody to show it
		// to, and republishing to ourselves would loop.
		b.forget(sessionID)
		return true
	}

	n, err := keeperredis.PublishConsoleUpstream(ctx, b.redis, owner, b.kid, msg)
	if err != nil {
		b.logger.Warn("console: upstream publish failed",
			slog.String("session_id", sessionID),
			slog.String("owner_kid", owner),
			slog.Any("error", err),
		)
		return false // transient
	}
	if n == 0 {
		// The claim stands but its owner is not subscribed — that instance is
		// gone (a subscription lives as long as the process). Forget the route
		// and report the orphan.
		b.forget(sessionID)
		return true
	}

	// A terminal frame ends the route: keep the cache bounded by session
	// lifetime rather than by instance uptime.
	if _, terminal := msg.GetPayload().(*keeperv1.FromSoul_ConsoleExit); terminal {
		b.forget(sessionID)
	}
	return false
}

// ShouldReap reports whether this instance should send the orphan reap for a
// session, at most once per [orphanReapCooldown]. Output arrives in a stream,
// so without this a dead owner would produce one ConsoleClose per chunk.
func (b *ClusterBridge) ShouldReap(sessionID string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if at, ok := b.reaped[sessionID]; ok && time.Since(at) < orphanReapCooldown {
		return false
	}
	b.reaped[sessionID] = time.Now()
	return true
}

// resolveOwner returns the owning KID for a session, consulting the cache
// first.
//
// found=false means the claim is genuinely absent — an orphan signal. A non-nil
// error means the lookup itself failed, which says NOTHING about the owner and
// must never be read as "gone".
func (b *ClusterBridge) resolveOwner(ctx context.Context, sid, sessionID string) (owner string, found bool, err error) {
	b.mu.RLock()
	cached, isCached := b.ownerOf[sessionID]
	missAt, missed := b.notFound[sessionID]
	b.mu.RUnlock()

	if isCached {
		return cached.ownerKID, true, nil
	}
	if missed && time.Since(missAt) < negativeOwnerTTL {
		return "", false, nil
	}

	owner, err = keeperredis.ReadConsoleSessionOwner(ctx, b.redis, sessionID)
	if err != nil {
		b.logger.Warn("console: owner lookup failed — dropping frame",
			slog.String("session_id", sessionID), slog.Any("error", err))
		return "", false, err
	}
	if owner == "" {
		b.mu.Lock()
		b.notFound[sessionID] = time.Now()
		b.mu.Unlock()
		return "", false, nil
	}

	b.mu.Lock()
	b.ownerOf[sessionID] = bridgedSession{ownerKID: owner, sid: sid}
	delete(b.notFound, sessionID)
	b.mu.Unlock()
	return owner, true, nil
}

// Orphan is a bridged session whose socket owner has vanished.
type Orphan struct {
	SessionID string
	SID       string
}

// SweepOrphans returns the bridged sessions whose owner claim has lapsed.
//
// This is the ACTIVE half of orphan detection, and it is not redundant with the
// check on the forward path: that one only fires when a frame arrives, and the
// dangerous session is precisely the quiet one. An operator who ran `sleep 900`
// (or just left a shell at its prompt) and whose Keeper then died produces no
// output at all — nothing would ever notice, and the pty would outlive everyone.
//
// The claim is refreshed every 30s against a 90s TTL, so a lapse means the
// owning instance is gone rather than merely slow. A Redis failure yields no
// orphans: a blip must never be read as "kill the operator's shell".
func (b *ClusterBridge) SweepOrphans(ctx context.Context) []Orphan {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	tracked := make(map[string]bridgedSession, len(b.ownerOf))
	for id, bs := range b.ownerOf {
		tracked[id] = bs
	}
	b.mu.RUnlock()

	var orphans []Orphan
	for sessionID, bs := range tracked {
		owner, err := keeperredis.ReadConsoleSessionOwner(ctx, b.redis, sessionID)
		if err != nil {
			b.logger.Debug("console: orphan sweep lookup failed — leaving the session alone",
				slog.String("session_id", sessionID), slog.Any("error", err))
			continue
		}
		if owner == bs.ownerKID {
			continue // owner still refreshing its claim
		}
		// Either the claim lapsed (the instance died) or another instance took
		// it over — in both cases our cached route is stale. A takeover cannot
		// happen for a live console (the claim is written once at open and only
		// refreshed by its owner), so a changed owner means the id was reused
		// after our session already ended.
		if owner == "" && b.ShouldReap(sessionID) {
			orphans = append(orphans, Orphan{SessionID: sessionID, SID: bs.sid})
		}
		// Drop the stale route but KEEP the reap mark: a dying pty usually emits
		// a parting chunk, and without the mark the forward path would reap the
		// same session a second time.
		b.forgetRoute(sessionID)
	}
	return orphans
}

// tracks reports whether this bridge is currently routing a session — the
// precondition for the sweep being able to see it at all.
func (b *ClusterBridge) tracks(sessionID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.ownerOf[sessionID]
	return ok
}

func (b *ClusterBridge) forget(sessionID string) {
	b.mu.Lock()
	delete(b.ownerOf, sessionID)
	delete(b.notFound, sessionID)
	delete(b.reaped, sessionID)
	b.mu.Unlock()
}

// forgetRoute drops the cached route but leaves the reap mark in place, so a
// session already reaped is not reaped again by the other detection path.
func (b *ClusterBridge) forgetRoute(sessionID string) {
	b.mu.Lock()
	delete(b.ownerOf, sessionID)
	delete(b.notFound, sessionID)
	b.mu.Unlock()
}

// RunUpstreamConsumer feeds frames arriving on this instance's channel into the
// Hub. Blocks until the subscription channel closes or ctx is done; the caller
// runs it in a goroutine for the process lifetime.
func RunUpstreamConsumer(ctx context.Context, sub *keeperredis.ConsoleUpstreamSubscription, hub *Hub) {
	if sub == nil || hub == nil {
		return
	}
	in := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-in:
			if !ok {
				return
			}
			hub.DeliverLocal(ctx, msg)
		}
	}
}
