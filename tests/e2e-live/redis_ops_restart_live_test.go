//go:build e2e_live

// L3b E2E day-2: examples/service/redis::restart on a LIVE Redis - rolling-restart
// of the daemon without a config change. Proves: uptime was reset (restart actually
// happened), role is master (0 replicas), config survived the restart.
package e2e_live_test

import "testing"

func TestL3bRedisLive_Day2Restart(t *testing.T) {
	stack, inc, adminPass := setupRedisStandalone(t, "rdb", "volatile-lru", 1024)
	c := plainConn(adminPass)

	rst := stack.RunScenario(t, inc, "restart", map[string]any{"reason": "e2e restart"})
	stack.WaitApplySuccess(t, rst, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	// Before the restart, redis had been up through the whole create+ready (>180s); a 120s threshold catches the uptime reset.
	stack.AssertRedisUptimeBelow(t, c, 120)
	// 0 replicas -> the single node remains master (INFO replication role probe).
	stack.AssertRedisRole(t, c, "master")
	// The config (eviction policy from create) survived the restart.
	stack.AssertRedisConfigGet(t, c, "maxmemory-policy", "volatile-lru")
}
