//go:build e2e_live

// L3b E2E day-2: examples/service/redis::add_user через community-плагин —
// live-guard NIM-8 (auto-deps) + ADR-065 S5. Доказывает ПОЛНУЮ цепочку
// day-2 плагин-канала на ЖИВОМ Redis:
//
//	create (sentinel, 0 реплик = standalone-эквивалент) поднимает redis-server →
//	day-2 add_user: синтез install community.redis (auto-deps ADR-065) → FetchModule
//	(plugingit.Resolver F-fetch из git-source-репо) → Sigil-verify (допуск v1.0.0) →
//	hot-register → community.redis.acl (ACL LOAD) против РЕАЛЬНОГО инстанса → state/audit.
//
// Отличие от L3a (tests/e2e/redis_test.go, soul-stub): реальный soul-in-container
// с реальным redis + реальным плагин-subprocess, а не scripted-success.
//
// ★ Среда прогона: create деплоит redis (install_method=binary, essence-default →
// бинари из WB Nexus) + БЕЗУСЛОВНЫЕ node-exporter/redis-exporter/vector (fetch
// GitHub/Nexus). Контейнеру нужен доступ к этим источникам, иначе create-apply
// падает на fetch.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisLive_Day2AddUser(t *testing.T) {
	// Сборка community-плагина ДО NewStack: source-URL нужен на buildKeeperYAML.
	repoURL := harness.BuildCommunityRedisPlugin(t)

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       1,
		SoulModules: []harness.SoulModuleEntry{
			{Name: "redis", Source: repoURL, Ref: harness.CommunityRedisPluginRef},
		},
	})
	defer stack.Cleanup()

	const (
		incName   = "redis"
		newUser   = "appuser"
		adminUser = "default_admin"
		adminPass = "e2e-default-admin-secret"
	)

	// Vault-seed: главный пароль + default_admin (AUTH плагина community.redis.acl И
	// redis-cli-ассерта — requirepass убран, редизайн default_admin) + новый юзер
	// (add_user резолвит secret/redis/<inc>/users/<name>#password keeper-side, create
	// про него не знает → пред-сеем). Прочие системные (replica/monitoring/…) create
	// генерит сам (core.vault.kv-present generate-if-absent). rel БЕЗ mount/data-префикса.
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "e2e-redis-main"})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/"+adminUser, map[string]any{"password": adminPass})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/"+newUser, map[string]any{"password": "e2e-appuser-secret"})

	// Coven-членство ДО Create (roster по incarnation.name ∈ coven[], ADR-008) + ждём
	// первый SoulprintReport (redis.conf.tmpl биндит на soulprint.self.network.primary_ip).
	stack.AddSoulToCoven(t, 0, incName)
	stack.WaitSoulprintReported(t, 0, 60)

	// Материализуем ВСЕ destiny, которые компонует create: redis + БЕЗУСЛОВНЫЕ
	// node-exporter/redis-exporter/vector (Слайс I / ADR-067). Все ref:v1.0.0 (service.yml).
	stack.MaterializeDestinies(t, "v1.0.0", "redis", "node-exporter", "redis-exporter", "vector")

	// Допуск community.redis@v1.0.0 через operator Sigil-API (AllowSoulModule). Ref-pin
	// обязателен (ADR-065): auto-deps синтезирует install с ref=v1.0.0, допуск на main
	// не сработал бы.
	stack.AllowSoulModule(t, "community", "redis", harness.CommunityRedisPluginRef)

	// Create standalone-эквивалент: sentinel + 0 реплик (режимы standalone/sentinel_only
	// убраны из сервиса). provision:{enabled:false} — деплой на ГОТОВЫЙ soul-roster (иначе
	// provision default-on полез бы в cloud-create). create_scenario явный — у сервиса ТРИ
	// create:true (create/create_from_souls/migrate_cluster) → без него 422. connection_mode
	// plain → redis на 6379 без TLS (add_user коннектится 127.0.0.1:6379). version — enum.
	inc, createApply := stack.CreateIncarnationWithApplyScenario(t, incName, "redis@main", "create", map[string]any{
		"provision":           map[string]any{"enabled": false},
		"redis_type":          "sentinel",
		"version":             "7.4.1",
		"connection_mode":     "plain",
		"persistence":         "rdb",
		"replicas_per_master": 0,
		"memory_mb":           1024,
		"maxmemory_policy":    "volatile-lru",
	})
	// 600s: apt/fetch install + render + systemd start + монолог экспортеров/vector.
	stack.WaitApplySuccess(t, createApply, 600)
	// 300s, не 60 (NIM-45): WaitApplySuccess отдаёт success на keeper-строке ДО
	// появления soul-строки, поэтому ready-wait покрывает весь soul-apply (redis +
	// 3 экспортёра, замерено ~115s) + commit-барьер; ready наступает ~120s.
	stack.WaitIncarnationReady(t, inc, 300)

	// Day-2 add_user: тут синтезируется install community.redis → FetchModule → Sigil-verify
	// → hot-register → community.redis.acl (ACL LOAD). Пароль нового юзера — НЕ во входе (Vault).
	addApply := stack.RunScenario(t, inc, "add_user", map[string]any{
		"user": map[string]any{
			"name":  newUser,
			"perms": "~app:* +@read +@write -@dangerous",
			"state": "on",
		},
	})
	stack.WaitApplySuccess(t, addApply, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	// (а) state.redis_users несёт нового юзера (массив AclUser, wave 1b; пароль НЕ в state).
	// Create прошёл без operator-users → redis_users стартовал []; после add_user = [appuser].
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_users": []any{
			map[string]any{"name": newUser, "perms": "~app:* +@read +@write -@dangerous", "state": "on"},
		},
	})

	// (б) audit-событие запуска add_user (payload несёт scenario+apply_id).
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "add_user",
		"apply_id": addApply,
	})

	// (в) ★ЖИВОЙ эффект: community.redis.acl отработал ACL LOAD → нового юзера видно в
	// РЕАЛЬНОМ инстансе через redis-cli ACL LIST (AUTH default_admin), а не только в state.
	stack.AssertRedisACLUser(t, 0, "127.0.0.1", 6379, adminUser, adminPass, newUser)
}
