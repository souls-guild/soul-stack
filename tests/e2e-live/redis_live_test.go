//go:build e2e_live

// L3b E2E pilot: examples/service/redis::create (режим standalone) —
// real-soul-in-container (redis-консолидация концепции Ansible-роли, ADR-039 /
// .pm/tasks/2026-06-22-redis-consolidation).
//
// Параллель с tests/e2e/redis_test.go (L3a, soul-stub отвечает scripted), но
// идущая через РЕАЛЬНЫЙ apt-install redis-server + render redis.conf внутри
// Debian-12-systemd-soul-container. Диспетчер create инклудит ветку standalone
// (cluster/sentinel/sentinel_only гасятся static-when), которая раскрывает
// режим-агностичный destiny `redis`.
//
// Экспортеры (redis_exporter/node_exporter) ВЫНЕСЕНЫ из service/redis: мониторинг —
// отдельная сущность. Прежний exporter-coupled pilot заменён на чистый
// standalone-redis live-create.
//
// Покрытие, которого L3a не даёт: Keeper render → ApplyRequest на wire → реальный
// soul Apply (core.pkg / core.file.rendered / core.service) → RunResult →
// apply_runs success + redis.conf реально отрендерен с merged-директивами.
//
// Harness-механики (см. tests/e2e-live/harness):
//   - SeedVaultKV: redis-пароль keeper-side через vault('secret/redis/<inc>#password');
//   - MaterializeDestinies + default_destiny_source: apply:destiny resolve git-URL;
//   - WaitSoulprintReported: redis.conf.tmpl биндит на soulprint.self.network.primary_ip;
//   - AssertExpectations: apply_runs / incarnation_state / host_state (redis.conf
//     отрендерен, redis-server active).
//
// Timeout 300s — apt-update + apt-get install redis-server + render + systemctl
// start. На холодном CI медленно.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisLive_CreateStandalone(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("ожидался 1 soul-контейнер, получено %d", got)
	}
	const wantSID = "soul-live-a.example.com"
	if sc := stack.SoulContainers[0]; sc.SID != wantSID {
		t.Errorf("SoulContainers[0].SID = %q, ожидалось %q", sc.SID, wantSID)
	}

	const incName = "redis"

	// Vault-seed redis-пароля: standalone create читает его keeper-side через
	// vault('secret/redis/'+incarnation.name+'#password'); per-user пароль —
	// vault('secret/redis/'+incarnation.name+'/users/<name>#password'). rel БЕЗ
	// mount/`data/`-префикса (SeedVaultKV добавляет их, KV v2).
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-redis-secret",
	})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/app", map[string]any{
		"password": "e2e-app-user-secret",
	})

	// Coven-членство ДО Create: roster резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Bootstrap-flow выставил status='connected', но coven пуст.
	stack.AddSoulToCoven(t, 0, incName)

	// Ждём первый SoulprintReport реального soul-а: redis.conf.tmpl биндит на
	// soulprint.self.network.primary_ip keeper-side при рендере.
	stack.WaitSoulprintReported(t, 0, 60)

	// Материализуем режим-агностичный destiny `redis` под git-тегом v1.0.0 (ref из
	// service.yml::destiny[]) + ставим default_destiny_source (SeedDefaultDestinySource
	// блокируется на holderRefreshGrace — Holder подхватит значение ДО create-render).
	stack.MaterializeDestinies(t, "v1.0.0", "redis")

	// Простой типизированный ввод: version (distro-native пин), memory_mb +
	// persistence + maxmemory_policy → merge-трансляция в redis.conf; users — typed-map.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"version":          "5:7.0.15-1~deb12u7",
		"memory_mb":        1024,
		"persistence":      "rdb",
		"maxmemory_policy": "volatile-lru",
		"users": map[string]any{
			"app": map[string]any{
				"perms": "~app:* +@read +@write -@dangerous",
				"state": "on",
			},
		},
	})

	stack.WaitApplySuccess(t, applyID, 300)
	// apply_runs success ≠ state закоммичен — ждём ready перед чтением state.
	stack.WaitIncarnationReady(t, inc, 30)

	exp := harness.LoadExpectations(t, "redis/expectations/after-create.yaml")
	stack.AssertExpectations(t, exp, applyID, inc)

	// apply_id в payload audit-event-а — runtime-значение, отдельно от YAML-fixture.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
}
