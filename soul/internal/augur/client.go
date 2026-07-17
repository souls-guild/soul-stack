// Package augur — Soul-side client for the Augur broker (ADR-025, docs/keeper/augur.md).
//
// Augur gives Soul live (during apply) access to external systems through
// Keeper: the core.augur.fetch module sends an AugurRequest over EventStream
// and waits for a correlated AugurReply. Transport is only-add messages on
// the existing EventStream (FromSoul.augur_request / FromKeeper.augur_reply),
// no new RPC (ADR-012(c)).
//
// The client lives exactly one EventStream session: the pending map
// correlates in-flight requests by request_id, the session's recv-loop
// delivers AugurReply. On session break/close all waiters are cancelled
// (Close) — request_id is only unique per-stream (§5.1 augur.md), so there's
// no need to survive a reconnect.
//
// MVP-1 (delegate=false): Soul gets the value inline through Keeper
// (AugurReply.inline_data). Delegation (delegate=true, scoped_*) is MVP-2,
// not handled here.
package augur

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"

	"github.com/oklog/ulid/v2"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ErrClientClosed — client closed (EventStream session ended) before an
// AugurReply arrived. Returned by Fetch to waiting callers on Close.
var ErrClientClosed = errors.New("augur: client closed (EventStream session ended)")

// ErrDenied — Augur denied access (AugurStatus DENIED, or UNSPECIFIED
// defensively treated as deny, §5.1 augur.md). Carries the reason from
// Keeper without any secret material.
var ErrDenied = errors.New("augur: access denied")

// ErrRemote — execution failure on the Keeper/Omen side (AugurStatus ERROR).
var ErrRemote = errors.New("augur: execution failure on the Keeper side")

// requestSender — the narrow EventStream-session surface the client needs:
// just sending FromSoul. Kept separate for testability without a live gRPC
// connection, and so the client doesn't depend on soul/internal/grpc (which
// depends on runtime — avoids an import cycle).
//
// Not concurrent-safe on the real session (one writer per bidi-stream); the
// client serializes Send under sendMu.
type requestSender interface {
	SendFromSoul(*keeperv1.FromSoul) error
}

// Client — Augur client for a single EventStream session.
//
// One writer (sendMu serializes Send on the stream — bidi-stream doesn't
// allow concurrent Send). AugurReply delivery happens via the session's
// recv-loop calling Deliver: it does NOT block (buffered channel of size 1 +
// non-blocking write).
type Client struct {
	sender requestSender
	// entropy — monotonic source for ULID request_id. Covered by sendMu
	// (generation happens at send time) — no separate mutex needed.
	entropy *ulid.MonotonicEntropy

	sendMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan *keeperv1.AugurReply
	closed  bool
}

// NewClient builds a client on top of an EventStream session. The entropy
// source for request_id is monotonic (lexically sortable, collision-free
// within a session).
func NewClient(sender requestSender) *Client {
	return &Client{
		sender:  sender,
		entropy: ulid.Monotonic(rand.Reader, 0),
		pending: make(map[string]chan *keeperv1.AugurReply),
	}
}

// Fetch sends an AugurRequest and blocks until the correlated AugurReply, ctx
// cancellation, or client close. Returns inline_data (delegate=false, §5.3)
// on OK; ErrDenied/ErrRemote/ErrClientClosed otherwise.
//
// request_id is generated here (Soul-side ULID, unique per-stream, §5.1). The
// pending channel is registered BEFORE Send — otherwise a fast AugurReply
// could arrive at the recv-loop before registration and get lost.
func (c *Client) Fetch(ctx context.Context, applyID, omen, query string) (*keeperv1.AugurReply, error) {
	reqID, replyCh, err := c.register()
	if err != nil {
		return nil, err
	}
	// Unregister on any outcome — timeout/cancel/delivery. Without this the
	// pending map would leak on cancelled requests.
	defer c.discard(reqID)

	req := &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_AugurRequest{
			AugurRequest: &keeperv1.AugurRequest{
				RequestId: reqID,
				ApplyId:   applyID,
				OmenName:  omen,
				Query:     query,
			},
		},
	}
	if err := c.send(req); err != nil {
		return nil, fmt.Errorf("augur: sending request: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply, ok := <-replyCh:
		if !ok {
			// Channel closed by Close — session broke, no reply is coming.
			return nil, ErrClientClosed
		}
		return classify(reply)
	}
}

// Deliver is called by the session's recv-loop on FromKeeper.augur_reply. It
// does NOT block: looks up the pending channel by request_id and writes to it
// non-blocking (channel buffered to 1; the sole consumer Fetch is either
// already waiting or has timed out — in the latter case default drops the
// late reply). Returns true if the reply was delivered to someone (for
// "orphaned reply" diagnostics).
func (c *Client) Deliver(reply *keeperv1.AugurReply) bool {
	if reply == nil {
		return false
	}
	c.mu.Lock()
	ch, ok := c.pending[reply.GetRequestId()]
	c.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- reply:
		return true
	default:
		// Consumer already gone (timeout/cancel) — drop the late reply,
		// recv-loop doesn't block.
		return false
	}
}

// Close closes the client: future Fetch calls are rejected, all waiters get
// ErrClientClosed (by closing their channels). Called when the EventStream
// session ends. Idempotent.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// register generates a request_id and registers the pending channel. The
// channel is buffered to 1 — Deliver writes non-blocking even if Fetch
// hasn't reached select yet. Errors only if the client is closed.
func (c *Client) register() (string, <-chan *keeperv1.AugurReply, error) {
	ch := make(chan *keeperv1.AugurReply, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", nil, ErrClientClosed
	}
	// ULID generated under mu (entropy is mutated by the monotonic generator).
	// Collisions within a session are ruled out by monotonicity; regenerating
	// on an unlikely map dup is defensive.
	var id string
	for {
		id = ulid.MustNew(ulid.Now(), c.entropy).String()
		if _, dup := c.pending[id]; !dup {
			break
		}
	}
	c.pending[id] = ch
	return id, ch, nil
}

// discard removes the pending channel (Fetch finished via timeout/cancel/
// delivery). Safe if the client is already closed (Close may have removed
// the entry).
func (c *Client) discard(reqID string) {
	c.mu.Lock()
	delete(c.pending, reqID)
	c.mu.Unlock()
}

// send serializes FromSoul sends (bidi-stream — single writer).
func (c *Client) send(msg *keeperv1.FromSoul) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.sender.SendFromSoul(msg)
}

// classify interprets an AugurReply. UNSPECIFIED is treated as DENIED
// (default-deny, §5.1 augur.md): no explicit OK means denied. delegate=true
// results (scoped_*) are MVP-2; MVP-1 expects inline_data on OK.
func classify(reply *keeperv1.AugurReply) (*keeperv1.AugurReply, error) {
	switch reply.GetStatus() {
	case keeperv1.AugurStatus_AUGUR_STATUS_OK:
		if reply.GetInlineData() == nil {
			// OK without inline_data in MVP-1 means either delegate=true (not
			// supported here) or a Keeper mismatch. Fail loud, not silent.
			return nil, fmt.Errorf("augur: OK without inline_data (delegate=true not supported in MVP-1)")
		}
		return reply, nil
	case keeperv1.AugurStatus_AUGUR_STATUS_DENIED:
		return nil, wrapReason(ErrDenied, reply.GetError())
	case keeperv1.AugurStatus_AUGUR_STATUS_ERROR:
		return nil, wrapReason(ErrRemote, reply.GetError())
	default:
		// UNSPECIFIED and any unknown status — deny (defensive).
		return nil, wrapReason(ErrDenied, reply.GetError())
	}
}

// wrapReason appends the Keeper's reason to the sentinel error, if present.
// The reason comes from Keeper without any secret (§8 augur.md: values/
// tokens are never written to diagnostics).
func wrapReason(base error, reason string) error {
	if reason == "" {
		return base
	}
	return fmt.Errorf("%w: %s", base, reason)
}
