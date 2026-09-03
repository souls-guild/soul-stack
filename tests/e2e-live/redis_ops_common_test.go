//go:build e2e_live

// Common setup for L3b redis day-2 tests on a PLAIN incarnation (NIM-54): one live
// Redis in standalone-equivalent mode (sentinel + 0 replicas, provision off = deploy onto a
// ready soul-roster) under update_config / restart / update_users / destroy.
// Repeats the chain from redis_day2_adduser_live_test.go, factored into setupRedisStandalone.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

const (
	redisDay2AdminUser = "default_admin"
	redisDay2AdminPass = "e2e-default-admin-secret"
)

// setupRedisStandalone brings up an L3b stack with a LIVE Redis (sentinel, 0 replicas =
// standalone-equivalent), a plain connection (6379 without TLS), an allowlisted redis,
// and creates the incarnation via a create run through to status=ready. Returns the stack, the
// incarnation name, and the default_admin password (for day-2 assert AUTH). Cleanup is registered
// via t.Cleanup - the caller doesn't need a defer.
func setupRedisStandalone(t *testing.T, persistence, maxmemoryPolicy string, memoryMB int) (stack *harness.Stack, inc, adminPass string) {
	t.Helper()
	return setupRedisStandaloneWith(t, false, persistence, maxmemoryPolicy, memoryMB)
}

// setupRedisStandaloneWith is setupRedisStandalone with the artifact source as a
// parameter. upstream=true takes the release tarballs from the public internet
// instead of the harness's local mirror (NIM-542) — exactly one caller does that,
// and it is deliberately not in E2E_GATE_TESTS.
func setupRedisStandaloneWith(t *testing.T, upstream bool, persistence, maxmemoryPolicy string, memoryMB int) (stack *harness.Stack, inc, adminPass string) {
	t.Helper()

	repoURL := harness.BuildRedisPlugin(t)

	stack = harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       1,
		SoulModules: []harness.SoulModuleEntry{
			{Name: harness.RedisAlias, Source: repoURL, Ref: harness.RedisPluginRef},
		},
		UpstreamArtifacts: upstream,
	})
	t.Cleanup(stack.Cleanup)

	const incName = "redis"

	// Vault seed: incarnation's main password + default_admin (used by redis.* AUTH and
	// by redis-cli asserts). create generates other system users itself.
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "e2e-redis-main"})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/"+redisDay2AdminUser, map[string]any{"password": redisDay2AdminPass})

	stack.MaterializeDestinies(t, "v1.0.0", "redis", "node-exporter", "redis-exporter", "vector")
	stack.AllowSoulModule(t, harness.RedisAlias, repoURL, harness.RedisPluginRef)

	// Seed the incarnation row -> bind the roster -> run create. The order is
	// owned by CreateIncarnationOnRoster (NIM-192): membership carries an FK on
	// incarnation(name), so it cannot precede the row, while a create run with
	// provision off resolves the roster at run start and would abort no_hosts
	// without it. Destinies/Sigil are registry-level and stay ahead of the run.
	inc, createApply := stack.CreateIncarnationOnRoster(t, incName, "redis@main", "create", []int{0}, map[string]any{
		"provision":           map[string]any{"enabled": false},
		"redis_type":          "sentinel",
		"version":             "7.4.1",
		"connection_mode":     "plain",
		"persistence":         persistence,
		"replicas_per_master": 0,
		"memory_mb":           memoryMB,
		"maxmemory_policy":    maxmemoryPolicy,
	})
	stack.WaitApplySuccess(t, createApply, 600)
	stack.WaitIncarnationReady(t, inc, 300)

	return stack, inc, redisDay2AdminPass
}

// plainConn - a plain (no TLS) redis-cli connection to the day-2 tests' standalone instance.
func plainConn(adminPass string) harness.RedisConn {
	return harness.RedisConn{SoulIdx: 0, Host: "127.0.0.1", Port: 6379, User: redisDay2AdminUser, Pass: adminPass}
}
