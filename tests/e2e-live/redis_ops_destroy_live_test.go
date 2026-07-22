//go:build e2e_live

// L3b E2E day-2: examples/service/redis teardown - force-DELETE of the incarnation
// (allow_destroy) with provision off (no VM). Proves the ON DELETE CASCADE:
// GET -> 404 and apply_runs/state_history for the incarnation = 0 live rows.
package e2e_live_test

import "testing"

func TestL3bRedisLive_Day2Destroy(t *testing.T) {
	stack, inc, _ := setupRedisStandalone(t, "rdb", "volatile-lru", 1024)

	// provision off -> no VM; force-DELETE = synchronous cascade of keeper registries.
	stack.DeleteIncarnation(t, inc, true)
	stack.AssertIncarnationAbsent(t, inc)
}
