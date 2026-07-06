//go:build e2e_live

// L3b E2E day-2: examples/service/redis teardown — force-DELETE инкарнации
// (allow_destroy) при provision off (нет VM). Доказывает каскад ON DELETE CASCADE:
// GET → 404 и apply_runs/state_history по инкарнации = 0 живых строк.
package e2e_live_test

import "testing"

func TestL3bRedisLive_Day2Destroy(t *testing.T) {
	stack, inc, _ := setupRedisStandalone(t, "rdb", "volatile-lru", 1024)

	// provision off → нет VM; force-DELETE = синхронный каскад keeper-реестров.
	stack.DeleteIncarnation(t, inc, true)
	stack.AssertIncarnationAbsent(t, inc)
}
