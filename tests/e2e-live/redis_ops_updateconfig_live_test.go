//go:build e2e_live

// L3b E2E day-2: examples/service/redis::update_config on a LIVE Redis - hot-reload
// of directives without a process restart. Closes NIM-53 findings: #1 persistence-enum
// aof_1sec - a valid essence.persistence_presets key (render doesn't crash on "no such
// key"); #2 io-threads - a startup-only directive (CONFIG SET skips it, plugin denylist)
// -> lands ON DISK, rewrite:false doesn't erase it.
package e2e_live_test

import "testing"

func TestL3bRedisLive_Day2UpdateConfig(t *testing.T) {
	stack, inc, adminPass := setupRedisStandalone(t, "rdb", "volatile-lru", 1024)
	c := plainConn(adminPass)

	// Change memory budget, eviction policy, persistence mode and io-threads in one
	// run (CONFIG SET hot-settable + CONFIG REWRITE, io-threads skipped).
	upd := stack.RunScenario(t, inc, "update_config", map[string]any{
		"memory_mb":        512,
		"maxmemory_policy": "allkeys-lru",
		"persistence":      "aof_1sec",
		"io_threads":       2,
	})
	stack.WaitApplySuccess(t, upd, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	// (a) hot-settable directives came alive on the instance (CONFIG GET).
	stack.AssertRedisConfigGet(t, c, "maxmemory-policy", "allkeys-lru")
	// aof_1sec -> appendonly yes: finding #1 - aof_1sec is a valid enum preset, render doesn't crash.
	stack.AssertRedisConfigGet(t, c, "appendonly", "yes")

	// (b) startup-only io-threads is visible ONLY on disk (CONFIG SET skips it):
	// finding #2 - rewrite:false doesn't erase the directive in redis.conf.
	stack.AssertRedisConfFileDirective(t, 0, "/etc/redis/redis.conf", "io-threads", "2")
	// memory_mb=512 translates to maxmemory 384mb (reserve 75%, essence) on disk -
	// CONFIG GET maxmemory would return bytes, so we check the deterministic conf-file render.
	stack.AssertRedisConfFileDirective(t, 0, "/etc/redis/redis.conf", "maxmemory", "384mb")

	// (c) read-model: state carries the new simple values (update_config state_changes).
	stack.AssertIncarnationState(t, inc, map[string]any{
		"persistence":      "aof_1sec",
		"maxmemory_policy": "allkeys-lru",
		"memory_mb":        512,
		"io_threads":       2,
	})
}
