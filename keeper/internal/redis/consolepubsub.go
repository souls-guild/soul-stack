package redis

// Cross-Keeper routing of console output (ADR-002 HA, NIM-143).
//
// A console spans two connections that land independently in a stateless
// cluster: the operator's WebSocket (`/v1/console`) terminates on whichever
// Keeper the load balancer picked, while the host's EventStream is held by
// whichever Keeper won the SoulLease. With N instances they coincide about 1/N
// of the time.
//
// The DOWNSTREAM half needs nothing new — Outbound already routes a FromKeeper
// to the lease holder over `outbound:<sid>`. This file is the UPSTREAM half:
// pty output arriving on the holder has to reach the instance that owns the
// socket.
//
// Addressing is by OWNER, not by session. The socket owner publishes a claim
// `console:owner:<session_id> -> <kid>|<sid>`; the holder resolves it ONCE per
// session and caches it, then publishes every frame to `console:<owner-kid>` —
// one channel per Keeper instance, subscribed once at startup. The alternative
// (a channel per session) would cost one Redis subscription per open terminal,
// i.e. hundreds on a busy cluster, to carry the same bytes.
//
// The claim carries the SID as well as the KID (NIM-196) so the publisher can
// check that an authenticated frame names a session belonging to the host it
// arrived from. That check has to live here, on the claim, because the pub/sub
// envelope is deliberately left alone: adding a field to it would make a new
// instance publish what an old one cannot read, breaking every cross-instance
// console for the length of a rolling upgrade.
//
// The claim TTL is deliberately short: it only has to survive the window
// between ConsoleOpen and the first upstream frame (milliseconds in practice).
// After that the holder's in-memory cache answers, and if the holder restarts
// its EventStreams break — which kills every pty behind them anyway
// (kill-on-disconnect), so a stale claim can never resurrect a dead session.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protojson"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ConsoleOwnerTTL is how long a session's owner claim lives in Redis, refreshed
// by the socket owner while the session is alive.
//
// Short on purpose. The claim is not just an address — its ABSENCE is how the
// stream holder learns that an operator's Keeper died, so it can reap a pty
// that would otherwise keep running with nobody watching it. The TTL is
// therefore the detection window for that orphan, and 90s (refreshed every 30s)
// keeps it inside a couple of minutes at a cost of one SET per session per
// refresh — nothing next to what an abandoned root shell costs.
const ConsoleOwnerTTL = 90 * time.Second

// consoleUpstreamBufferSize is the Go channel buffer between the Redis PubSub
// loop and the Hub. Deeper than the outbound mirror (10): this channel carries
// pty output for EVERY bridged session on the instance, not the sparse command
// traffic of one Soul.
const consoleUpstreamBufferSize = 256

// ConsoleOwnerKey is the claim key mapping a console session to the Keeper
// instance holding its operator socket.
func ConsoleOwnerKey(sessionID string) string {
	return "console:owner:" + sessionID
}

// ConsoleUpstreamChannelKey is the per-instance channel carrying upstream
// console frames addressed to that Keeper.
func ConsoleUpstreamChannelKey(kid string) string {
	return "console:" + kid
}

// ConsoleSessionOwner is a resolved claim: the Keeper holding the operator's
// socket, and the host the session was opened against.
//
// SID carries the binding a publisher checks a frame against (NIM-196). It is
// empty for a claim written before that binding existed — mid-upgrade the
// reader must read that as "unknown", never as a mismatch.
type ConsoleSessionOwner struct {
	KID string
	SID string
}

// consoleClaimSep joins the halves of a claim value. Split on its LAST
// occurrence: a SID is an FQDN and cannot contain it, an operator-set KID can.
const consoleClaimSep = "|"

// ClaimConsoleSession records this Keeper as the owner of a session's socket,
// bound to the host the session was authorised against.
func ClaimConsoleSession(ctx context.Context, c *Client, sessionID, kid, sid string) error {
	if c == nil {
		return errors.New("redis.ClaimConsoleSession: nil client")
	}
	if sessionID == "" || kid == "" {
		return errors.New("redis.ClaimConsoleSession: empty sessionID or kid")
	}
	if sid == "" {
		return errors.New("redis.ClaimConsoleSession: empty sid")
	}
	value := kid + consoleClaimSep + sid
	if err := c.underlying().Set(ctx, ConsoleOwnerKey(sessionID), value, ConsoleOwnerTTL).Err(); err != nil {
		return fmt.Errorf("redis.ClaimConsoleSession: SET %q: %w", ConsoleOwnerKey(sessionID), err)
	}
	return nil
}

// ReleaseConsoleSession drops the claim when a session ends.
func ReleaseConsoleSession(ctx context.Context, c *Client, sessionID string) error {
	if c == nil {
		return errors.New("redis.ReleaseConsoleSession: nil client")
	}
	if sessionID == "" {
		return errors.New("redis.ReleaseConsoleSession: empty sessionID")
	}
	if err := c.underlying().Del(ctx, ConsoleOwnerKey(sessionID)).Err(); err != nil {
		return fmt.Errorf("redis.ReleaseConsoleSession: DEL %q: %w", ConsoleOwnerKey(sessionID), err)
	}
	return nil
}

// ReadConsoleSessionOwner returns the claim on a session's socket. A zero KID
// means no claim exists (the session is gone, or was never bridged); a zero SID
// means the claim predates the binding and says nothing about the host.
func ReadConsoleSessionOwner(ctx context.Context, c *Client, sessionID string) (ConsoleSessionOwner, error) {
	if c == nil {
		return ConsoleSessionOwner{}, errors.New("redis.ReadConsoleSessionOwner: nil client")
	}
	if sessionID == "" {
		return ConsoleSessionOwner{}, errors.New("redis.ReadConsoleSessionOwner: empty sessionID")
	}
	v, err := c.underlying().Get(ctx, ConsoleOwnerKey(sessionID)).Result()
	if errors.Is(err, redis.Nil) {
		return ConsoleSessionOwner{}, nil
	}
	if err != nil {
		return ConsoleSessionOwner{}, fmt.Errorf("redis.ReadConsoleSessionOwner: GET %q: %w", ConsoleOwnerKey(sessionID), err)
	}
	i := strings.LastIndex(v, consoleClaimSep)
	if i < 0 {
		return ConsoleSessionOwner{KID: v}, nil // pre-NIM-196 claim
	}
	return ConsoleSessionOwner{KID: v[:i], SID: v[i+len(consoleClaimSep):]}, nil
}

// consoleUpstreamEnvelope wraps one published frame. `OriginKID` lets a
// subscriber drop the echo of its own publication — the same race guard as
// [outboundEnvelope] (a session may migrate back to this instance between the
// owner lookup and delivery).
type consoleUpstreamEnvelope struct {
	OriginKID string          `json:"origin_kid"`
	Payload   json.RawMessage `json:"payload"`
}

// PublishConsoleUpstream sends a FromSoul console frame to the Keeper instance
// that owns the session's socket. Returns the number of subscribers that got
// it; 0 means the owner is gone (its socket closed, or the instance died) and
// the frame is dropped — pub/sub is fire-and-forget, and a console has no
// replay semantics (a terminal shows what arrives, live).
func PublishConsoleUpstream(ctx context.Context, c *Client, ownerKID, originKID string, msg *keeperv1.FromSoul) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishConsoleUpstream: nil client")
	}
	if ownerKID == "" {
		return 0, errors.New("redis.PublishConsoleUpstream: empty ownerKID")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishConsoleUpstream: empty originKID")
	}
	if msg == nil {
		return 0, errors.New("redis.PublishConsoleUpstream: nil msg")
	}

	payload, err := protojson.Marshal(msg)
	if err != nil {
		return 0, fmt.Errorf("redis.PublishConsoleUpstream: protojson.Marshal: %w", err)
	}
	env, err := json.Marshal(consoleUpstreamEnvelope{OriginKID: originKID, Payload: payload})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishConsoleUpstream: envelope marshal: %w", err)
	}

	ch := ConsoleUpstreamChannelKey(ownerKID)
	n, err := c.underlying().Publish(ctx, ch, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishConsoleUpstream: PUBLISH %q: %w", ch, err)
	}
	return n, nil
}

// ConsoleUpstreamSubscription is a handle to this instance's
// `console:<kid>` subscription.
type ConsoleUpstreamSubscription struct {
	ps        *redis.PubSub
	out       chan *keeperv1.FromSoul
	selfKID   string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel is the read side of the unpacked FromSoul messages.
func (s *ConsoleUpstreamSubscription) Channel() <-chan *keeperv1.FromSoul {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready blocks until Redis acknowledges the subscription.
func (s *ConsoleUpstreamSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.ConsoleUpstreamSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.ConsoleUpstreamSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the loop and releases the Redis handle. Idempotent.
func (s *ConsoleUpstreamSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeConsoleUpstream subscribes to this instance's console channel. One
// subscription per Keeper, opened at startup — not one per session.
func SubscribeConsoleUpstream(ctx context.Context, c *Client, selfKID string, logger *slog.Logger) (*ConsoleUpstreamSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeConsoleUpstream: nil client")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeConsoleUpstream: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeConsoleUpstream: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, ConsoleUpstreamChannelKey(selfKID))
	s := &ConsoleUpstreamSubscription{
		ps:      ps,
		out:     make(chan *keeperv1.FromSoul, consoleUpstreamBufferSize),
		selfKID: selfKID,
		logger:  logger,
		ready:   make(chan struct{}),
		stopped: make(chan struct{}),
	}

	closed := make(chan struct{})
	s.closeOnce = func() error {
		select {
		case <-closed:
			return nil
		default:
		}
		close(closed)
		err := ps.Close()
		<-s.stopped
		return err
	}

	go s.run(ctx, closed)
	return s, nil
}

func (s *ConsoleUpstreamSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeConsoleUpstream: initial Receive failed",
			slog.String("kid", s.selfKID), slog.Any("error", err))
		return
	}
	close(s.ready)

	for {
		select {
		case <-closed:
			return
		case <-ctx.Done():
			return
		default:
		}

		msg, err := s.ps.ReceiveMessage(ctx)
		if err != nil {
			select {
			case <-closed:
				return
			case <-ctx.Done():
				return
			default:
			}
			s.logger.Warn("redis.SubscribeConsoleUpstream: ReceiveMessage failed",
				slog.String("kid", s.selfKID), slog.Any("error", err))
			return
		}

		fromSoul, originKID, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if originKID == s.selfKID {
			continue // self-echo, see consoleUpstreamEnvelope
		}

		select {
		case s.out <- fromSoul:
		default:
			// The Hub is not draining — the same drop-with-a-log the whole
			// console path uses. Output is lossy under pressure by design; the
			// operator sees a gap, not a stalled terminal.
			s.logger.Warn("redis.SubscribeConsoleUpstream: forward channel full, dropping frame",
				slog.String("kid", s.selfKID))
		}
	}
}

func (s *ConsoleUpstreamSubscription) decodeMessage(payload string) (*keeperv1.FromSoul, string, bool) {
	var env consoleUpstreamEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeConsoleUpstream: envelope unmarshal failed",
			slog.String("kid", s.selfKID), slog.Any("error", err))
		return nil, "", false
	}
	if len(env.Payload) == 0 {
		return nil, env.OriginKID, false
	}
	out := &keeperv1.FromSoul{}
	if err := protojson.Unmarshal(env.Payload, out); err != nil {
		s.logger.Warn("redis.SubscribeConsoleUpstream: protojson.Unmarshal failed",
			slog.String("kid", s.selfKID), slog.Any("error", err))
		return nil, env.OriginKID, false
	}
	return out, env.OriginKID, true
}
