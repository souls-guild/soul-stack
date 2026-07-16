package pushprovider

import (
	"context"
)

// TopicPushProvidersChanged is the Redis pub/sub topic for cluster-wide
// notifications of push_providers changes (ADR-032 amendment 2026-05-26, S7-2).
//
// Publisher side: pushprovider.Service publishes to this topic after successful
// Create/Update/Delete commits with provider_name (or empty string for bulk operations).
// Subscriber side: SshDispatcher in setupPushDispatchers marks spawned plugins as stale
// on receipt and respawns them on the next RPC (spawn-on-change semantics).
//
// Redis pub/sub has no persistence: message loss (reconnect, broker flicker) leaves
// the plugin with stale env-payload until the next mutation or keeper restart.
// This is acceptable—mutations are rare, staleness window is milliseconds at steady state.
//
// Convention push-providers:changed is a separate namespace stylistically
// similar to sigil:invalidate / rbac:invalidate (format <subsystem>:<event>);
// plural push-providers reflects the resource name (REST /v1/push-providers).
const TopicPushProvidersChanged = "push-providers:changed"

// RedisPublisher is a narrow interface of the Redis client needed for invalidation.
// Implemented by keeperredis.Client (via wrapper added in S7-2 wire-up)
// and any test mock. We narrow to one method so service does not depend on
// the full redis-client / go-redis package.
type RedisPublisher interface {
	PublishPushProvidersChanged(ctx context.Context, providerName string) error
}

// nopPublisher is a no-op implementation for when Redis is disabled
// (single-instance dev / tests without Redis). Like nilTollDegraded:
// service does not fail, but spawn-on-change does not work in cluster mode
// (changes visible only on the instance receiving the mutation, after restart).
type nopPublisher struct{}

// NopPublisher returns a no-op [RedisPublisher].
func NopPublisher() RedisPublisher { return nopPublisher{} }

func (nopPublisher) PublishPushProvidersChanged(_ context.Context, _ string) error {
	return nil
}
