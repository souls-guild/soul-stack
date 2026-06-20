//go:build e2e

// L3a контракт-e2e: examples/service/redis-cluster — мутирующий scenario
// `update_acl` на контракт-тире staged-render (probe→where, ADR-056).
//
// В отличие от redis-cluster-live (целиком на core-модулях, один host) этот
// сервис — оригинальный redis-cluster с `apply: destiny: redis`, vault-scoped
// `redis_password` и declared-ролями. Его `create` в L3a недоступен (cloud-spawn /
// declared-primary / probe на ещё-не-запущенном redis), поэтому incarnation
// засевается напрямую в Postgres (SeedIncarnationReady) с baseline state, а
// проверяется именно `update_acl`:
//
//   - Passage 0 — probe "Detect actual redis role per host" (on: incarnation,
//     без where): три хоста, host-0 эмитит redis_role.stdout='master',
//     host-1/host-2 — 'replica' (per-host register через SetTaskRegister);
//     failed_when: size(register.redis_role) < host_count → нужны ВСЕ три ответа.
//   - Passage 1 — "Diff and apply ACL changes on the current master"
//     (where: register.redis_role.stdout=='master', run_once: true): ACL-apply
//     должен затаргетиться ТОЛЬКО на host-0.
//
// ★ ASSERT: Passage-1 ApplyRequest пришёл ТОЛЬКО master-хосту (host-0) — это
// доказывает redis-cluster probe→where на контракт-тире (drift «register в where
// всегда пуст» закрыт staged-render-ом, теперь на реальном сервисе).
//
// ★ state-read: Passage-1 `apply: input: { current: ${ incarnation.state.redis_users } }`
// читает read-only снимок incarnation.state в scenario-render CEL (ADR-009/010
// amendment 2026-06-20, Вариант A). state_changes (ADR-057 CRUD `modify`) патчит
// redis_users по input.changes — assert ниже фиксирует обновлённый ACL.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

const (
	rcService = "redis-cluster"
	rcExample = "examples/service/redis-cluster"
)

// rcSoulprintOS — минимальный os-факт хоста redis-cluster. pkg_mgr/init_system
// нужны core.pkg/core.service внутри destiny redis; primary_ip — стабильный
// soulprint-факт (на L3a `where:` идёт по register-у probe, не по нему, но факт
// присутствует как у настоящего хоста).
func rcSoulprintOS(ip string) map[string]any {
	return map[string]any{
		"os": map[string]any{
			"family":      "debian",
			"distro":      "debian",
			"version":     "12",
			"arch":        "amd64",
			"pkg_mgr":     "apt",
			"init_system": "systemd",
		},
		"network": map[string]any{"primary_ip": ip},
	}
}

// rcBaselineState — incarnation.state до update_acl (один предсуществующий юзер
// app, чтобы redis_users было непустым и видна неизменность после `modifies`).
func rcBaselineState() map[string]any {
	return map[string]any{
		"redis_version": "7.2.4",
		"redis_users": map[string]any{
			"app": map[string]any{"acl": "on >old-pass ~app:* +@read", "state": "on"},
		},
		"redis_config": map[string]any{"appendonly": "yes"},
		"redis_hosts":  []any{},
	}
}

func TestE2EServiceRedisCluster_UpdateAcl(t *testing.T) {
	const incName = "redis-cluster-update-acl"

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: rcExample,
		Souls:       3,
	})
	defer stack.Cleanup()

	// soulprint + Coven-членство (roster по incarnation.name, ADR-008) для всех трёх.
	ips := []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	for i := 0; i < 3; i++ {
		stack.SeedSoulprint(t, i, rcSoulprintOS(ips[i]))
		stack.AddSoulToCoven(t, i, incName)
	}

	// apply: destiny: redis (update_acl Passage 1) требует резолва destiny-репо.
	// Материализуем `redis` под git-тегом v2.0.0 (service.yml::destiny[].ref) и
	// ставим default_destiny_source — ДО RegisterService (invalidate подтянет снимок).
	stack.MaterializeDestinies(t, "v2.0.0", "redis")
	stack.RegisterService(t, rcService, rcExample)

	// Пароль доступен scoped vault:-ref (vault_scope: secret/redis/* на redis_password).
	// update_acl его НЕ использует, но seed-им путь, чтобы доказать достижимость
	// vault-scope-канала, разблокированного STEP 1 (create/add_replica его читают).
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "s3cr3t-redis-pass"})

	// Прямой seed ready-incarnation с baseline state (create в L3a недоступен).
	stack.SeedIncarnationReady(t, incName, rcService, "main", rcBaselineState())

	// Три live-стрима. host-0 — master, host-1/host-2 — replica.
	master := stack.ConnectSoulStub(t, 0)
	replica1 := stack.ConnectSoulStub(t, 1)
	replica2 := stack.ConnectSoulStub(t, 2)
	master.SetApplyDefaultSuccess(true)
	replica1.SetApplyDefaultSuccess(true)
	replica2.SetApplyDefaultSuccess(true)

	// Per-host probe-register на задаче probe (Passage 0). Все три ОБЯЗАНЫ ответить
	// (failed_when: size < host_count), но only-master матчит where: Passage 1.
	const probeTask = "Detect actual redis role per host"
	master.SetTaskRegister(probeTask, map[string]any{"stdout": "master", "changed": false, "failed": false})
	replica1.SetTaskRegister(probeTask, map[string]any{"stdout": "replica", "changed": false, "failed": false})
	replica2.SetTaskRegister(probeTask, map[string]any{"stdout": "replica", "changed": false, "failed": false})

	// changes — map username → {acl,state}. update_acl применяет их к мастеру.
	applyID := stack.RunScenario(t, incName, "update_acl", map[string]any{
		"changes": map[string]any{
			"app": map[string]any{"acl": "on >new-pass ~app:* +@all", "state": "on"},
		},
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)

	// ★ Passage 0 (probe): ApplyRequest пришёл ВСЕМ трём хостам (probe без where).
	mP0 := applyRequestsForPassage(master, 0)
	r1P0 := applyRequestsForPassage(replica1, 0)
	r2P0 := applyRequestsForPassage(replica2, 0)
	if mP0 == 0 || r1P0 == 0 || r2P0 == 0 {
		t.Fatalf("Passage 0: probe должен прийти всем — master=%d replica1=%d replica2=%d", mP0, r1P0, r2P0)
	}

	// ★ Passage 1 (ACL-apply): ApplyRequest ТОЛЬКО master-хосту. where:
	// register.redis_role.stdout=='master' + run_once резолвнулись per-host
	// register-ом Passage 0. Это и есть доказательство redis-cluster probe→where.
	mP1 := applyRequestsForPassage(master, 1)
	r1P1 := applyRequestsForPassage(replica1, 1)
	r2P1 := applyRequestsForPassage(replica2, 1)
	if mP1 == 0 {
		t.Fatalf("★ Passage 1: master НЕ получил ApplyRequest — staged where не затаргетил master (probe→where drift)")
	}
	if r1P1 != 0 || r2P1 != 0 {
		t.Fatalf("★ Passage 1: replica получили ApplyRequest (r1=%d r2=%d) — where:'master' НЕ должен таргетить replica", r1P1, r2P1)
	}

	// ★ state.redis_users ПАТЧИТСЯ по input.changes: state_changes foreach по
	// input.changes + modify: redis_users match key==change.key патчит acl/state
	// существующего юзера `app` (baseline acl `…+@read` → новый `…+@all`).
	// Коммит — один раз после последнего Passage (ADR-009 §7).
	stack.AssertIncarnationState(t, incName, map[string]any{
		"redis_users": map[string]any{
			"app": map[string]any{"acl": "on >new-pass ~app:* +@all", "state": "on"},
		},
	})

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "update_acl",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}
