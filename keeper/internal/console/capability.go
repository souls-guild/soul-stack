package console

import (
	"context"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/config"
)

// CapabilityConsole is the capability a Soul announces in Hello when its binary
// can host a pty session. The literal lives in shared/config so Keeper and Soul
// reference the same string — a desync here is a silent fail-closed.
const CapabilityConsole = config.CapabilityConsole

// redisCapabilities reads the capability set a Soul announced at connect time
// (stored next to its heartbeat, ADR-056 §S5).
type redisCapabilities struct {
	client *keeperredis.Client
}

// NewRedisCapabilityChecker builds the production capability gate. A nil client
// yields nil, which the Hub reads as "skip the check" — that is correct for a
// single-node dev build with no Redis, where there is no capability record to
// consult in the first place.
func NewRedisCapabilityChecker(c *keeperredis.Client) CapabilityChecker {
	if c == nil {
		return nil
	}
	return &redisCapabilities{client: c}
}

// HasCapability reports whether the SID's active stream announced the given
// capability. A lookup failure returns the error rather than a guess: the Hub
// refuses the open, because minting a session for a Soul that may not
// understand ConsoleOpen leaves the operator staring at a terminal that never
// answers.
func (r *redisCapabilities) HasCapability(ctx context.Context, sid, capability string) (bool, error) {
	return keeperredis.SoulHasCapability(ctx, r.client, sid, capability)
}
