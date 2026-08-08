package soulforget

import (
	"context"
	"errors"
	"fmt"
)

// ErrTeardownUnavailable means the cluster-wide teardown notice could not be
// sent, so the operation stopped before touching the database — the host is
// still registered and can be forgotten again once Redis is reachable.
//
// It exists because "forget" has to mean released, not merely deleted. A
// forgotten host cannot reconnect (its allowlist entry goes with its row), but
// a stream it already holds on ANOTHER Keeper instance survives the delete and
// only that instance can close it. If the notice cannot go out, a success
// would be a lie about the one thing the operator asked for. Callers map this
// to 503, not 500: it is a "come back in a moment", and nothing was destroyed.
var ErrTeardownUnavailable = errors.New("soulforget: cluster teardown notice could not be sent, nothing was deleted")

// Teardown releases what the database cannot reach: the live EventStream and
// the per-SID Redis keys. The daemon wires a real implementation over the
// StreamManager and the Redis client; a nil Teardown is the single-instance
// dev / unit-test mode, where there is no second instance to notify and the
// stream dies with the process.
type Teardown interface {
	// CloseLocal closes the EventStream THIS instance holds for the SID and
	// reports whether there was one. Deterministic and local — it cannot fail,
	// which is why it runs after the delete rather than before it.
	CloseLocal(sid string) bool

	// Broadcast asks every other Keeper instance to close its own stream for
	// the SID. Best-effort delivery by nature (pub/sub has no acknowledgement),
	// so the error it returns means "could not even publish", not "nobody
	// listened".
	//
	// sent distinguishes "the notice went out" from "there was no cluster to
	// tell" — a Keeper without Redis is single-instance by construction (Redis
	// IS the coordination layer, ADR-006), so there is no one to notify and no
	// failure either: it returns (false, nil). Reporting that as a broadcast
	// would put a notice in the operator's record that was never sent.
	Broadcast(ctx context.Context, sid string) (sent bool, err error)

	// PurgeCache removes the per-SID Redis keys and returns how many it
	// deleted. The heartbeat hash `soul:<sid>:hb` carries NO TTL by design, so
	// without this it outlives the host forever.
	PurgeCache(ctx context.Context, sid string) (int64, error)
}

// Release records what the teardown actually managed to release. It is
// reported to the operator verbatim; a partial release must never be rendered
// as a plain success.
type Release struct {
	// LocalStreamClosed — this instance held a live EventStream and cancelled
	// it. Cancellation, not a completed teardown: [Teardown.CloseLocal] fires
	// the stream's context and returns, and the gRPC handler unwinds when it
	// observes that. Nothing can keep the stream alive past the cancel, so the
	// distinction never changes the outcome — but a frame already in flight can
	// still land, which is why the Redis purge documents the heartbeat key
	// `soul:<sid>:hb` as resurrectable.
	LocalStreamClosed bool
	// Broadcast — the post-delete teardown notice was PUBLISHED. Not "received":
	// pub/sub has no acknowledgement, and the subscriber count Redis returns is
	// deliberately not consulted (zero subscribers is the normal single-instance
	// case and cannot be told apart from a cluster that missed the notice). See
	// [Teardown.Broadcast].
	Broadcast bool
	// CacheKeysPurged — per-SID Redis keys removed. Zero whenever the purge
	// reported an error: a number beside a failure gets read on its own as a
	// release that happened, and this field is what lands in the audit payload.
	CacheKeysPurged int64
	// Warnings names every resource that could not be released, in words an
	// operator can act on. Empty means fully released.
	Warnings []string
}

// Result is the whole outcome of one forget: what the delete cost, and what
// the teardown released.
type Result struct {
	Counts
	Release
}

// Erase forgets one host: it releases the resources the host holds and erases
// its registry row, in an order chosen so that neither half can quietly fail.
//
//  1. Broadcast the teardown notice. This is a pre-flight as much as an
//     action: if the cluster cannot be reached, the call fails with
//     [ErrTeardownUnavailable] having deleted NOTHING. Deleting first and
//     discovering afterwards that a stream on another instance cannot be
//     closed would leave a forgotten host still talking, with the operator
//     told it succeeded.
//  2. [Forget] — the transaction. Once it commits, the host's allowlist entry
//     is gone and it can no longer authenticate, so no reconnect can slip in
//     behind the teardown.
//  3. Close the local stream, broadcast a second time, purge the Redis keys.
//     The second broadcast is what closes the race the first one opens: a host
//     whose stream was killed in step 1 may have reconnected during step 2,
//     and this one catches that stream. It is idempotent — an instance with
//     nothing to close does nothing.
//
// Failures in step 3 do not undo the delete (it is already committed and the
// host is already unable to come back); they are recorded in
// [Release.Warnings] so the operator sees exactly what is still holding on.
func Erase(ctx context.Context, db TxBeginner, td Teardown, sid, reason string) (Result, error) {
	if td != nil {
		if _, err := td.Broadcast(ctx, sid); err != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrTeardownUnavailable, err)
		}
	}

	counts, err := Forget(ctx, db, sid, reason)
	if err != nil {
		return Result{}, err
	}

	res := Result{Counts: counts}
	if td == nil {
		return res, nil
	}

	res.LocalStreamClosed = td.CloseLocal(sid)

	sent, err := td.Broadcast(ctx, sid)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"the host was forgotten, but the second teardown notice failed (%v); a stream it opened on another Keeper instance in the last moment may still be live until that instance drops it",
			err))
	}
	res.Broadcast = sent

	n, err := td.PurgeCache(ctx, sid)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"the host was forgotten, but its Redis cache keys could not be removed (%v); `soul:%s:hb` has no TTL and will not expire on its own",
			err, sid))
		// Whatever count came back with the error is not something the operator
		// can rely on, and `cache_keys_purged: 2` in the audit trail reads as a
		// release regardless of the warning sitting next to it.
		n = 0
	}
	res.CacheKeysPurged = n

	return res, nil
}
