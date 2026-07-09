//go:build e2e_live

// Общий setup L3b операционных тестов redis на PLAIN-инкарнации (NIM-54): один живой
// Redis в standalone-эквиваленте (sentinel + 0 реплик, provision off = деплой на
// готовый soul-roster) под update_config / restart / update_users / destroy.
// Повторяет цепочку redis_ops_adduser_live_test.go, вынесенную в setupRedisStandalone.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

const (
	redisOpsAdminUser = "default_admin"
	redisOpsAdminPass = "e2e-default-admin-secret"
)

// setupRedisStandalone поднимает L3b-стек с ЖИВЫМ Redis (sentinel, 0 реплик =
// standalone-эквивалент), plain-коннект (6379 без TLS), допущенным community.redis
// и создаёт инкарнацию create-прогоном до status=ready. Возвращает стек, имя
// инкарнации и пароль default_admin (AUTH ассертов). Cleanup регистрируется
// через t.Cleanup — вызывающему defer не нужен.
func setupRedisStandalone(t *testing.T, persistence, maxmemoryPolicy string, memoryMB int) (stack *harness.Stack, inc, adminPass string) {
	t.Helper()

	repoURL := harness.BuildCommunityRedisPlugin(t)

	stack = harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       1,
		SoulModules: []harness.SoulModuleEntry{
			{Name: "redis", Source: repoURL, Ref: harness.CommunityRedisPluginRef},
		},
	})
	t.Cleanup(stack.Cleanup)

	const incName = "redis"

	// Vault-seed: главный пароль инкарнации + default_admin (AUTH community.redis.* и
	// redis-cli-ассертов). Прочие системные юзеры create генерит сам.
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "e2e-redis-main"})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/"+redisOpsAdminUser, map[string]any{"password": redisOpsAdminPass})

	stack.AddSoulToCoven(t, 0, incName)
	stack.WaitSoulprintReported(t, 0, 60)

	stack.MaterializeDestinies(t, "v1.0.0", "redis", "node-exporter", "redis-exporter", "vector")
	stack.AllowSoulModule(t, "community", "redis", harness.CommunityRedisPluginRef)

	inc, createApply := stack.CreateIncarnationWithApplyScenario(t, incName, "redis@main", "create", map[string]any{
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

	return stack, inc, redisOpsAdminPass
}

// plainConn — plain (без TLS) redis-cli-коннект к standalone-инстансу операционных тестов.
func plainConn(adminPass string) harness.RedisConn {
	return harness.RedisConn{SoulIdx: 0, Host: "127.0.0.1", Port: 6379, User: redisOpsAdminUser, Pass: adminPass}
}
