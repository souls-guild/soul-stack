package grpc

import (
	"context"
	"log/slog"
	"time"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// handleHostUtilization — handler for the [keeperv1.HostUtilization] payload
// (NIM-86). Stores a host-vitals snapshot into Redis (latest Hash + a window of points);
// does not touch Postgres — telemetry is volatile and lives under TTL.
//
// SID — the authenticated peer (a dispatch parameter from mTLS), NEVER from the
// payload (HostUtilization has no sid field — a structural invariant).
//
// Redis-level errors are logged as warn and don't kill the stream: Soul sends
// utilization periodically, the next tick overwrites the snapshot.
func (h *eventStreamHandler) handleHostUtilization(ctx context.Context, sid, sessionID string, ev *keeperv1.HostUtilization) {
	if ev == nil {
		h.logger.Debug("eventstream: HostUtilization payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if h.deps.Redis == nil {
		// dev / unit build without Redis — nowhere to write vitals, quietly return.
		return
	}
	if err := keeperredis.WriteUtilization(ctx, h.deps.Redis, sid, ev, time.Now()); err != nil {
		h.logger.Warn("eventstream: WriteUtilization failed",
			slog.String("sid", sid), slog.Any("error", err))
	}
}
