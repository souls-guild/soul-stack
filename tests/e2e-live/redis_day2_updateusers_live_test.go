//go:build e2e_live

// L3b E2E day-2: examples/service/redis::update_users на ЖИВОМ Redis — bulk-replace
// набора operator-extra ACL-юзеров (ACL LOAD). Доказывает: (1) массовое заведение
// alice+bob, (2) bulk-replace удаляет выпавшего из набора (bob), alice остаётся.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisLive_Day2UpdateUsers(t *testing.T) {
	stack, inc, adminPass := setupRedisStandalone(t, "rdb", "volatile-lru", 1024)
	c := plainConn(adminPass)

	// Пароли новых юзеров — в Vault (update_users резолвит keeper-side, вход их не несёт).
	harness.SeedVaultKV(t, stack, "redis/"+inc+"/users/alice", map[string]any{"password": "e2e-alice-secret"})
	harness.SeedVaultKV(t, stack, "redis/"+inc+"/users/bob", map[string]any{"password": "e2e-bob-secret"})

	// Прогон 1: полный набор {alice, bob} — оба заводятся.
	add := stack.RunScenario(t, inc, "update_users", map[string]any{
		"users": []any{
			map[string]any{"name": "alice", "perms": "~app:* +@read +@write", "state": "on"},
			map[string]any{"name": "bob", "perms": "~* +@all", "state": "on"},
		},
	})
	stack.WaitApplySuccess(t, add, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	stack.AssertRedisACLUser(t, 0, "127.0.0.1", 6379, redisDay2AdminUser, adminPass, "alice")
	stack.AssertRedisACLUser(t, 0, "127.0.0.1", 6379, redisDay2AdminUser, adminPass, "bob")
	stack.AssertRedisACLUserPerms(t, c, "alice", "+@read")

	// Прогон 2: набор сузился до {alice} — bulk-replace удаляет bob из живого ACL.
	repl := stack.RunScenario(t, inc, "update_users", map[string]any{
		"users": []any{
			map[string]any{"name": "alice", "perms": "~app:* +@read +@write", "state": "on"},
		},
	})
	stack.WaitApplySuccess(t, repl, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	stack.AssertRedisACLUserAbsent(t, c, "bob")
	stack.AssertRedisACLUser(t, 0, "127.0.0.1", 6379, redisDay2AdminUser, adminPass, "alice")

	// read-model: state.redis_users = ровно [alice] (bulk-replace, bob выпал).
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_users": []any{
			map[string]any{"name": "alice", "perms": "~app:* +@read +@write", "state": "on"},
		},
	})
}
