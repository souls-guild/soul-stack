//go:build e2e_live

// L3b E2E day-2: examples/service/redis::restart на ЖИВОМ Redis — rolling-restart
// демона без изменения конфига. Доказывает: uptime сброшен (рестарт реально
// произошёл), роль master (0 реплик), конфиг пережил рестарт.
package e2e_live_test

import "testing"

func TestL3bRedisLive_Day2Restart(t *testing.T) {
	stack, inc, adminPass := setupRedisStandalone(t, "rdb", "volatile-lru", 1024)
	c := plainConn(adminPass)

	rst := stack.RunScenario(t, inc, "restart", map[string]any{"reason": "e2e restart"})
	stack.WaitApplySuccess(t, rst, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	// До рестарта redis жил весь create+ready (>180с); порог 120с ловит сброс uptime.
	stack.AssertRedisUptimeBelow(t, c, 120)
	// 0 реплик → одиночный узел остаётся master (INFO replication role probe).
	stack.AssertRedisRole(t, c, "master")
	// Конфиг (политика вытеснения из create) пережил рестарт.
	stack.AssertRedisConfigGet(t, c, "maxmemory-policy", "volatile-lru")
}
