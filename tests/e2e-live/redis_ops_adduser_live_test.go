//go:build e2e_live

// L3b E2E day-2: examples/service/redis::add_user via the community plugin -
// live-guard NIM-8 (auto-deps) + ADR-065 S5. Proves the FULL chain of the
// day-2 plugin channel on a LIVE Redis:
//
//	create (sentinel, 0 replicas = standalone-equivalent) brings up redis-server ->
//	day-2 add_user: synthesizes install community.redis (auto-deps ADR-065) -> FetchModule
//	(plugingit.Resolver F-fetch from the git source repo) -> Sigil-verify (allowlist v1.0.0) ->
//	hot-register -> community.redis.acl (ACL LOAD) against the REAL instance -> state/audit.
//
// Difference from L3a (tests/e2e/redis_test.go, soul-stub): a real soul-in-container
// with a real redis + a real plugin-subprocess, not scripted-success.
//
// * Run environment: create deploys redis (install_method=binary, essence-default ->
// binaries from an internal Nexus) + UNCONDITIONAL node-exporter/redis-exporter/vector (fetch
// GitHub/Nexus). The container needs access to these sources, otherwise create-apply
// fails on fetch.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisLive_Day2AddUser(t *testing.T) {
	// Build the community plugin BEFORE NewStack: the source URL is needed for buildKeeperYAML.
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

	// Vault seed: main password + default_admin (used both by the community.redis.acl plugin
	// AUTH and by the redis-cli assert - requirepass is dropped, so default_admin is re-designated) +
	// the new user (add_user resolves secret/redis/<inc>/users/<name>#password keeper-side,
	// create doesn't know about it -> pre-seed it). Other system users (replica/monitoring/...)
	// create generates itself (core.vault.kv-present generate-if-absent). rel WITHOUT mount/data prefix.
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "e2e-redis-main"})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/"+adminUser, map[string]any{"password": adminPass})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/"+newUser, map[string]any{"password": "e2e-appuser-secret"})

	// Membership BEFORE Create (roster via incarnation_membership, NIM-124) + wait
	// for the first SoulprintReport (redis.conf.tmpl binds to soulprint.self.network.primary_ip).
	stack.AddMember(t, 0, incName)
	stack.WaitSoulprintReported(t, 0, 60)

	// Materialize ALL destinies that create composes: redis + UNCONDITIONAL
	// node-exporter/redis-exporter/vector (Slice I / ADR-067). All ref:v1.0.0 (service.yml).
	stack.MaterializeDestinies(t, "v1.0.0", "redis", "node-exporter", "redis-exporter", "vector")

	// Allowlist community.redis@v1.0.0 via the operator Sigil API (AllowSoulModule). Ref-pin
	// is mandatory (ADR-065): auto-deps synthesizes install with ref=v1.0.0, allowing main
	// would not work.
	stack.AllowSoulModule(t, "community", "redis", harness.CommunityRedisPluginRef)

	// Create the standalone-equivalent: sentinel + 0 replicas (standalone/sentinel_only modes
	// have been removed from the service). provision:{enabled:false} - deploy onto a READY soul roster
	// (otherwise provision default-on would try to go to cloud-create). create_scenario is explicit - the service has
	// THREE create:true (create/create_from_souls/migrate_cluster) -> without it, 422. connection_mode
	// plain -> redis on 6379 without TLS (add_user connects to 127.0.0.1:6379). version - enum.
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
	// 600s: apt/fetch install + render + systemd start + exporters/vector rollout.
	stack.WaitApplySuccess(t, createApply, 600)
	// 300s, not 60 (NIM-45): WaitApplySuccess returns success on the keeper row BEFORE
	// the soul row appears, so ready-wait covers the entire soul-apply (redis +
	// 3 exporters, measured ~115s) + commit barrier; ready arrives ~120s.
	stack.WaitIncarnationReady(t, inc, 300)

	// Day-2 add_user: this is where install community.redis -> FetchModule -> Sigil-verify
	// -> hot-register -> community.redis.acl (ACL LOAD) gets synthesized. The new user's password is NOT in the input (Vault).
	addApply := stack.RunScenario(t, inc, "add_user", map[string]any{
		"user": map[string]any{
			"name":  newUser,
			"perms": "~app:* +@read +@write -@dangerous",
			"state": "on",
		},
	})
	stack.WaitApplySuccess(t, addApply, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	// (a) state.redis_users carries the new user (AclUser array, wave 1b; password NOT in state).
	// Create ran without operator-users -> redis_users started as []; after add_user = [appuser].
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_users": []any{
			map[string]any{"name": newUser, "perms": "~app:* +@read +@write -@dangerous", "state": "on"},
		},
	})

	// (b) audit event for add_user launch (payload carries scenario+apply_id).
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "add_user",
		"apply_id": addApply,
	})

	// (c) *LIVE effect: community.redis.acl ran ACL LOAD -> the new user is visible on the
	// REAL instance via redis-cli ACL LIST (AUTH default_admin), not just in state.
	stack.AssertRedisACLUser(t, 0, "127.0.0.1", 6379, adminUser, adminPass, newUser)
}
