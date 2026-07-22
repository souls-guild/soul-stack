package toll

import (
	"context"
	"time"
)

// degradedWriter — narrow surface for mutating the cluster:degraded flag, needed
// by the Leader. Narrowing it makes the Leader testable without a live Redis (see
// leader_test.go::fakeDegradedWriter). The daemon wraps
// [keeperredis.TollSetDegraded] / [keeperredis.TollClearDegraded] into an implementation.
type degradedWriter interface {
	// SetDegraded — sets the cluster:degraded key to the holder value (the
	// leader's KID) with a TTL. Uses SET without NX — on every tick the leader
	// refreshes the TTL (re-arm). A returned err means a Redis problem.
	SetDegraded(ctx context.Context, holder string, ttl time.Duration) error
	// ClearDegraded — DEL cluster:degraded. Idempotent (DEL on a missing
	// key is a no-op).
	ClearDegraded(ctx context.Context) error
}
