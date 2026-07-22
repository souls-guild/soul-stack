package grpc

import (
	"context"
	"log/slog"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// Cluster-mode outbound-subscribe loop (ADR-002 HA).
//
// When the EventStream handler registers a stream in StreamManager on
// Keeper-B, it also subscribes to the Redis pub/sub channel
// `outbound:<sid>`. Another Keeper instance (Keeper-A), whose
// Outbound.SendApply found no local stream and saw `holder == kid-B` in
// the lease, publishes a FromKeeper message there. The subscriber here
// forwards it to the local outbound channel of the same entry.
//
// Self-filtering by `origin_kid` happens on the
// [keeperredis.SubscribeOutbound] side; only messages published by other
// Keeper instances reach this point.

// startOutboundSubscriber sets up the Redis subscription and the forward
// goroutine for cluster-mode routing. Returns a cleanup function (closes
// the pub/sub) and a channel that closes when the forward goroutine exits.
//
// If Manager==nil or Redis==nil, cluster routing is disabled: returns
// (nil cleanup, an already-closed done). The caller (handler) just waits
// on done and ignores the nil cleanup.
//
// A subscribe error logs a warning and cluster routing degrades to
// per-instance (locally-only); the handler keeps running — this breaks no
// critical invariant (Outbound.SendApply from another Keeper will simply
// return ErrSoulNotConnected, and the caller sees the regular
// "no subscribers" error).
func (h *eventStreamHandler) startOutboundSubscriber(ctx context.Context, sid string, done chan<- struct{}) func() {
	if h.deps.Manager == nil || h.deps.Redis == nil {
		close(done)
		return nil
	}

	sub, err := keeperredis.SubscribeOutbound(ctx, h.deps.Redis, sid, h.deps.KID, h.logger)
	if err != nil {
		h.logger.Warn("eventstream: outbound pub/sub subscribe failed (cluster-routing disabled for this session)",
			slog.String("sid", sid),
			slog.Any("error", err),
		)
		close(done)
		return nil
	}
	// Wait for Redis to confirm the subscription is registered — otherwise
	// a PublishOutbound called right after this side acquires the lease
	// could miss it. Don't block forever: a sane timeout is tied to the
	// stream ctx.
	if err := sub.Ready(ctx); err != nil {
		h.logger.Warn("eventstream: outbound pub/sub Ready failed",
			slog.String("sid", sid),
			slog.Any("error", err),
		)
		_ = sub.Close()
		close(done)
		return nil
	}

	go h.runOutboundSubscriber(sid, sub, done)
	return func() { _ = sub.Close() }
}

func (h *eventStreamHandler) runOutboundSubscriber(sid string, sub *keeperredis.OutboundSubscription, done chan<- struct{}) {
	defer close(done)
	in := sub.Channel()
	for msg := range in {
		entry := h.deps.Manager.lookup(sid)
		if entry == nil {
			// The stream was already Unregistered — this is a concurrent
			// shutdown. The message is dropped (fire-and-forget pub/sub
			// semantics per PM-decision 5).
			h.logger.Debug("eventstream: outbound subscriber dropping message — no local entry",
				slog.String("sid", sid))
			continue
		}
		if !entry.send(msg) {
			h.logger.Warn("eventstream: outbound subscriber drop — queue full or closed",
				slog.String("sid", sid))
		}
	}
}
