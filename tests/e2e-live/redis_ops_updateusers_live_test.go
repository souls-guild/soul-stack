//go:build e2e_live

// L3b E2E day-2: examples/service/redis::update_users on a LIVE Redis - bulk-replace
// of the operator-extra ACL user set (ACL LOAD). Proves: (1) bulk-creating
// alice+bob, (2) bulk-replace removes the one dropped from the set (bob), alice remains.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisLive_Day2UpdateUsers(t *testing.T) {
	stack, inc, adminPass := setupRedisStandalone(t, "rdb", "volatile-lru", 1024)
	c := plainConn(adminPass)

	// New users' passwords go in Vault (update_users resolves them keeper-side, input doesn't carry them).
	harness.SeedVaultKV(t, stack, "redis/"+inc+"/users/alice", map[string]any{"password": "e2e-alice-secret"})
	harness.SeedVaultKV(t, stack, "redis/"+inc+"/users/bob", map[string]any{"password": "e2e-bob-secret"})

	// Run 1: full set {alice, bob} - both get created.
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

	// Run 2: the set narrows to {alice} - bulk-replace removes bob from the live ACL.
	repl := stack.RunScenario(t, inc, "update_users", map[string]any{
		"users": []any{
			map[string]any{"name": "alice", "perms": "~app:* +@read +@write", "state": "on"},
		},
	})
	stack.WaitApplySuccess(t, repl, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	stack.AssertRedisACLUserAbsent(t, c, "bob")
	stack.AssertRedisACLUser(t, 0, "127.0.0.1", 6379, redisDay2AdminUser, adminPass, "alice")

	// read-model: state.redis_users = exactly [alice] (bulk-replace, bob dropped).
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_users": []any{
			map[string]any{"name": "alice", "perms": "~app:* +@read +@write", "state": "on"},
		},
	})
}
