//go:build e2e_live

// L3b E2E real-apply: examples/service/redis-cluster scenario `update_acl` на
// ПОДЛИННОМ redis в трёх Debian-12-контейнерах (RC-1, закрывает главный e2e
// trust-gap — staged probe→where + apply:destiny + vault-scope + state-modify на
// реальном apply, не stub).
//
// L3a-прецедент TestE2EServiceRedisCluster_UpdateAcl (tests/e2e/redis_cluster_test.go)
// доказывает тот же сценарий на soul-STUB: probe-register инъецируется
// SetTaskRegister("master"/"replica"), apply:destiny update_acls — SetApplyDefaultSuccess.
// L3b заменяет stub живыми контейнерами:
//   - probe `redis-cli role | head -1` исполняется реальным soul против ЖИВОГО redis
//     (host-0 standalone master, host-1/2 — REPLICAOF host-0 → `redis-cli role` = slave);
//   - apply:destiny redis action=update_acls реально зовёт `redis-cli ACL SETUSER`
//     ТОЛЬКО на master (where: role=='master' + run_once);
//   - state.redis_users патчится state_changes (ADR-057 modify) по input.changes.
//
// ★ Bootstrap живого redis (host-0 master + host-1/2 replica) делается ПРЯМЫМ
// Exec-ом в контейнерах (install redis-server → start → REPLICAOF), а НЕ через
// create-сценарий: create у examples/service/redis-cluster офлайн-неприменим
// (cloud-spawn / declared-primary / probe на ещё-не-запущенном redis — см.
// L3a-комментарий). Это standalone replication (REPLICAOF), НЕ cluster-mode:
// нам нужен лишь дискриминатор `redis-cli role` master vs slave для probe→where.
//
// ★★ НАХОДКА (probe-newline, см. отчёт): committed-probe `redis-cli role | head -1`
// эмитит stdout "master\n" (register stdout НЕ триммится — ни soul-side
// util/exec.go, ни keeper-side applyrun/taskregister.go), а where:
// register.redis_role.stdout=='master' сравнивает с "master" без \n → на реальном
// apply where НЕ матчит master. L3a-stub маскировал это (инъецировал чистое
// "master"). Тест ассертит фактическое поведение register-сбора и явно фейлит на
// несовпадении — пилот вскрывает стоимость drift-а stub↔real.
package e2e_live_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

const (
	rcuService = "redis-cluster"
	rcuExample = "examples/service/redis-cluster"

	// update_acl: probe (Detect actual redis role per host) — первая top-level
	// задача плана, Passage 0, ГЛОБАЛЬНЫЙ plan_index 0. ACL-apply (apply:destiny)
	// — следующий Passage; его plan_index НЕ константа (зависит от разворота
	// destiny redis tasks/main.yml + loop), поэтому per-task ACL-статус assert-ится
	// passage-разрезом (assertOnlyMasterAppliedPassage1), а не plan_index-ключом.
	updateAclProbePlanIdx = 0
)

// rcuBaselineState — incarnation.state до update_acl: один предсуществующий юзер
// `app`, чтобы redis_users было непустым и видна мутация ACL после modify.
// Симметрично L3a rcBaselineState (tests/e2e/redis_cluster_test.go).
func rcuBaselineState() map[string]any {
	return map[string]any{
		"redis_version": "7.2.4",
		"redis_users": map[string]any{
			"app": map[string]any{"acl": "on >old-pass ~app:* +@read", "state": "on"},
		},
		"redis_config": map[string]any{"appendonly": "yes"},
		"redis_hosts":  []any{},
	}
}

func TestL3bRedisClusterUpdateAcl_LiveMaster(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: rcuExample,
		ServiceName: rcuService,
		Souls:       3,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 3 {
		t.Fatalf("ожидалось 3 soul-контейнера, получено %d", got)
	}
	const (
		masterSID   = "soul-live-a.example.com"
		replica1SID = "soul-live-b.example.com"
		replica2SID = "soul-live-c.example.com"
	)
	wantSIDs := []string{masterSID, replica1SID, replica2SID}
	for i, want := range wantSIDs {
		if got := stack.SoulContainers[i].SID; got != want {
			t.Fatalf("SoulContainers[%d].SID = %q, ожидалось %q", i, got, want)
		}
	}

	const incName = "redis-cluster-update-acl"

	// Coven-членство ДО запуска scenario: roster резолвится по incarnation.name ∈
	// coven[] (ADR-008). Без него no_hosts → ноль строк apply_runs.
	for i := range stack.SoulContainers {
		stack.AddSoulToCoven(t, i, incName)
	}

	// apply:destiny redis (update_acls) требует резолва standalone-destiny `redis`
	// под git-тегом service.yml::destiny[].ref (v2.0.0). Материализуем ПОСЛЕ
	// NewStack (service уже зарегистрирован NewStack-ом); SeedDefaultDestinySource
	// блокируется на holderRefreshGrace, поэтому к первому RunScenario снимок свеж.
	stack.MaterializeDestinies(t, "v2.0.0", "redis")

	// vault-scope-канал: scoped vault:-ref (vault_scope: secret/redis/*) на
	// redis_password. update_acl его НЕ читает, но seed-им путь, чтобы доказать
	// достижимость scope-канала (create/add_replica его читают). Симметрия с L3a.
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "s3cr3t-redis-pass"})

	// Прямой seed ready-incarnation с baseline state (create офлайн-неприменим).
	stack.SeedIncarnationReady(t, incName, rcuService, "main", rcuBaselineState())

	// ── Bootstrap ЖИВОГО redis: host-0 master, host-1/2 REPLICAOF host-0 ───────
	// Делает probe `redis-cli role` различимым (master vs slave) на реальном apply.
	masterIP := bootstrapLiveRedis(t, stack)

	// changes — map username → {acl,state}. update_acls патчит их на мастере.
	applyID := stack.RunScenario(t, incName, "update_acl", map[string]any{
		"changes": map[string]any{
			"app": map[string]any{"acl": "on >new-pass ~app:* +@all", "state": "on"},
		},
	})

	stack.WaitApplySuccess(t, applyID, 180)
	stack.WaitIncarnationReady(t, incName, 30)

	// ── (1) probe-register: реальный soul вернул роль каждого хоста ────────────
	// Passage 0 (probe), plan_index 0 в плане прогона. register stdout — то, что
	// реальный soul напечатал на `redis-cli role | head -1`.
	masterRole := registerStdout(t, stack, applyID, masterSID)
	r1Role := registerStdout(t, stack, applyID, replica1SID)
	r2Role := registerStdout(t, stack, applyID, replica2SID)
	t.Logf("probe register: master=%q replica1=%q replica2=%q", masterRole, r1Role, r2Role)

	// ★ probe-newline guard (RC-1, починено 7afc60c): committed-probe несёт
	// `tr -d '\n'`, поэтому register stdout = чистое "master" без trailing \n.
	// Регресс-guard: если \n вернулся (кто-то убрал tr), where:=='master' на
	// реальном apply снова перестанет матчить master, Passage 1 не затаргетит
	// никого — фейлим тут немедленно с указанием на trim-fix.
	if strings.Contains(masterRole, "\n") {
		t.Fatalf("★ probe-newline-регресс: master register stdout = %q содержит \\n — probe `redis-cli role | head -1 | tr -d '\\n'` потерял trim; where:=='master' не матчит на реальном apply (восстановить tr -d в examples/service/redis-cluster/scenario/update_acl/main.yml)", masterRole)
	}
	if strings.TrimSpace(masterRole) != "master" {
		t.Fatalf("probe: master-хост вернул role=%q, ожидалось master (redis-bootstrap некорректен?)", masterRole)
	}
	if strings.TrimSpace(r1Role) != "slave" || strings.TrimSpace(r2Role) != "slave" {
		t.Fatalf("probe: реплики вернули role r1=%q r2=%q, ожидалось slave (REPLICAOF не сошёлся?)", r1Role, r2Role)
	}

	// ── FC-0 helper: probe register stdout == 'master' через apply_task_register ─
	// AssertTaskRegisterField читает register_data->>'stdout' probe-задачи по
	// ГЛОБАЛЬНОМУ plan_index 0 (probe — первая задача плана, Passage 0). Параллель
	// с registerStdout выше (passage-разрез), но через канонический FC-0-ключ
	// plan_index — это вход, по которому where: register.redis_role.stdout=='master'
	// затаргетил master. tr -d '\n' (7afc60c) гарантирует точное 'master'.
	stack.AssertTaskRegisterField(t, applyID, masterSID, updateAclProbePlanIdx, "stdout", "master")
	stack.AssertTaskRegisterField(t, applyID, replica1SID, updateAclProbePlanIdx, "stdout", "slave")
	stack.AssertTaskRegisterField(t, applyID, replica2SID, updateAclProbePlanIdx, "stdout", "slave")

	// ── (2) Passage-1 targeting: ACL-apply пришёл ТОЛЬКО master-хосту ──────────
	// where: register.redis_role.stdout=='master' + run_once резолвнулись per-host
	// register-ом Passage 0. Реплики не должны нести passage=1-строку (или no_match).
	assertOnlyMasterAppliedPassage1(t, stack, applyID, masterSID, []string{replica1SID, replica2SID})

	// ── (3) ACL РЕАЛЬНО применён на master-контейнере ─────────────────────────
	// Independent verify: `redis-cli ACL GETUSER app` на host-0 содержит новые
	// права (+@all). baseline был `+@read` → доказывает, что update_acls реально
	// выполнил `redis-cli ACL SETUSER app ...` на живом redis (не только state-patch).
	assertACLGetUser(t, stack, 0, "app", []string{"~app:*", "+@all"})
	_ = masterIP // (диагностика выше через t.Logf)

	// ── (4) state.redis_users ПАТЧИТСЯ по input.changes (ADR-057 modify) ───────
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

// bootstrapLiveRedis ставит redis-server на все три контейнера и поднимает
// standalone-репликацию: host-0 — master, host-1/2 — REPLICAOF host-0. Возвращает
// IP host-0 (master). Идемпотентно по шагам (повторный nohup/REPLICAOF безвреден).
//
// Прямой Exec, а НЕ scenario-apply: офлайн-create неприменим (см. шапку файла), а
// поднятие redis через отдельный полноценный сценарий открыло бы свой apply-chain
// (нора). Replication, НЕ cluster-mode — нужен лишь дискриминатор `redis-cli role`.
func bootstrapLiveRedis(t *testing.T, stack *harness.Stack) (masterIP string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// apt install на всех трёх (redis-server + redis-tools/redis-cli).
	for i := range stack.SoulContainers {
		execOK(t, ctx, stack, i, []string{"sh", "-c",
			"apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq redis-server"},
			"apt install redis-server")
	}

	// Запуск redis-server в фоне на каждой ноде (bind 0.0.0.0, protected-mode no
	// для cross-container replication). Idempotent: unless ping PONG.
	for i := range stack.SoulContainers {
		execOK(t, ctx, stack, i, []string{"sh", "-c",
			"redis-cli -p 6379 ping 2>/dev/null | grep -q PONG || " +
				"(mkdir -p /var/lib/redis && nohup redis-server --bind 0.0.0.0 --protected-mode no --dir /var/lib/redis >/var/log/redis.log 2>&1 & sleep 1; true)"},
			"start redis-server")
	}

	// Health-gate: redis отвечает PONG на каждой ноде.
	for i := range stack.SoulContainers {
		waitRedisPong(t, ctx, stack, i)
	}

	// IP мастера (host-0) — REPLICAOF целевой адрес для реплик.
	out, code, err := stack.SoulContainers[0].Exec(ctx, []string{"sh", "-c", "hostname -i | awk '{print $1}'"})
	if err != nil || code != 0 {
		t.Fatalf("bootstrapLiveRedis: hostname -i на master: code=%d err=%v out=%s", code, err, out)
	}
	masterIP = strings.TrimSpace(out)
	if masterIP == "" {
		t.Fatalf("bootstrapLiveRedis: пустой master IP (hostname -i = %q)", out)
	}

	// host-1/2 → REPLICAOF host-0. Дожидаемся master_link_status:up.
	for i := 1; i < len(stack.SoulContainers); i++ {
		execOK(t, ctx, stack, i, []string{"sh", "-c",
			fmt.Sprintf("redis-cli -p 6379 replicaof %s 6379", masterIP)},
			"REPLICAOF master")
		waitReplicaLinkUp(t, ctx, stack, i)
	}

	return masterIP
}

// execOK выполняет команду в i-м контейнере и фейлит при exit!=0.
func execOK(t *testing.T, ctx context.Context, stack *harness.Stack, soulIdx int, cmd []string, desc string) {
	t.Helper()
	out, code, err := stack.SoulContainers[soulIdx].Exec(ctx, cmd)
	if err != nil {
		t.Fatalf("bootstrapLiveRedis[%d] %s: exec: %v\nout=%s", soulIdx, desc, err, out)
	}
	if code != 0 {
		t.Fatalf("bootstrapLiveRedis[%d] %s: exit=%d\nout=%s", soulIdx, desc, code, out)
	}
}

// waitRedisPong поллит redis-cli ping до PONG (cold-start redis-server ~1-3s).
func waitRedisPong(t *testing.T, ctx context.Context, stack *harness.Stack, soulIdx int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, code, err := stack.SoulContainers[soulIdx].Exec(ctx, []string{"redis-cli", "-p", "6379", "ping"})
		if err == nil && code == 0 && strings.Contains(out, "PONG") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("waitRedisPong[%d]: redis не ответил PONG за 30s", soulIdx)
}

// waitReplicaLinkUp поллит master_link_status:up в `redis-cli info replication`
// на реплике (REPLICAOF-handshake + initial sync).
func waitReplicaLinkUp(t *testing.T, ctx context.Context, stack *harness.Stack, soulIdx int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, code, err := stack.SoulContainers[soulIdx].Exec(ctx, []string{"redis-cli", "-p", "6379", "info", "replication"})
		if err == nil && code == 0 {
			last = out
			if strings.Contains(out, "master_link_status:up") {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("waitReplicaLinkUp[%d]: master_link_status:up не достигнут за 30s\nlast info:\n%s", soulIdx, last)
}

// registerStdout читает register_data->>'stdout' probe-задачи (Passage 0,
// plan_index 0) хоста sid. Доказывает, что реальный soul вернул TaskEvent с
// register-данными probe-роли, а keeper их персистил в apply_task_register.
func registerStdout(t *testing.T, stack *harness.Stack, applyID, sid string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout string
	err := stack.DB().QueryRow(ctx,
		`SELECT COALESCE(register_data->>'stdout','<null>') FROM apply_task_register
		 WHERE apply_id = $1 AND sid = $2 AND passage = 0
		 ORDER BY plan_index ASC LIMIT 1`, applyID, sid).Scan(&stdout)
	if err != nil {
		t.Fatalf("registerStdout(%s): нет register probe-задачи passage=0 (реальный soul не вернул register?): %v", sid, err)
	}
	return stdout
}

// assertOnlyMasterAppliedPassage1 проверяет, что ACL-apply (Passage 1) исполнился
// ТОЛЬКО на master-хосте: master несёт passage=1-строку success/changed, реплики —
// либо без passage=1, либо no_match (where: 'master' отфильтровал).
func assertOnlyMasterAppliedPassage1(t *testing.T, stack *harness.Stack, applyID, masterSID string, replicaSIDs []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var masterP1 string
	err := stack.DB().QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2 AND passage = 1`, applyID, masterSID).Scan(&masterP1)
	if err != nil {
		t.Fatalf("★ Passage 1: master НЕ получил apply_runs(passage=1) — staged where не затаргетил master (probe→where drift): %v", err)
	}
	if masterP1 != "success" && masterP1 != "changed" {
		t.Fatalf("★ Passage 1: master passage=1 status=%q, ожидалось success/changed", masterP1)
	}

	for _, sid := range replicaSIDs {
		var p1 string
		err := stack.DB().QueryRow(ctx,
			`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2 AND passage = 1`, applyID, sid).Scan(&p1)
		if err != nil {
			// нет строки passage=1 — допустимо (where отфильтровал реплику из таргета).
			continue
		}
		if p1 != "no_match" {
			t.Fatalf("★ Passage 1: реплика %s passage=1 status=%q — ACL-apply исполнился на реплике (where:'master' не отфильтровал, silent-wrong-target)", sid, p1)
		}
	}
}

// assertACLGetUser выполняет `redis-cli ACL GETUSER <user>` на i-м контейнере и
// фейлит, если вывод не содержит каждую ожидаемую подстроку. Independent verify,
// что update_acls реально применил ACL на живом redis (не только state-patch).
func assertACLGetUser(t *testing.T, stack *harness.Stack, soulIdx int, user string, wantSubstrings []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := stack.SoulContainers[soulIdx].Exec(ctx,
		[]string{"redis-cli", "-p", "6379", "ACL", "GETUSER", user})
	if err != nil {
		t.Fatalf("assertACLGetUser[%d] %s: exec: %v", soulIdx, user, err)
	}
	if code != 0 {
		t.Fatalf("assertACLGetUser[%d] %s: exit=%d out=%s", soulIdx, user, code, out)
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Fatalf("★ assertACLGetUser[%d] %s: вывод не содержит %q (ACL не применён на живом redis?)\nACL GETUSER:\n%s",
				soulIdx, user, want, out)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// FC-1 split-brain guard: failed_when по реальному модульному stdout.
//
// remove_replica содержит защиту «нельзя вывести текущий primary»
// (examples/service/redis-cluster/scenario/remove_replica/main.yml task #1):
//
//   failed_when: register.self.stdout == 'master'   # на trim-safe probe-stdout
//
// Тест таргетит remove_replica на host-0 (живой MASTER) → probe роли host-0 →
// failed_when видит 'master' → задача FAILED с error.code flowcontrol.failed_when
// → fail-stop ДО шага «Stop redis» → redis на host-0 НЕ остановлен → state_changes
// (remove из redis_hosts) НЕ коммитятся → incarnation error_locked.
//
// Это первое ЖИВОЕ доказательство failed_when-валидации на реальном модульном
// stdout (не completeness-идиома — её убрали 7afc60c). Контр-кейс (failed_when НЕ
// срабатывает на slave) доказан транзитивно success-апплаем update_acl на тех же
// репликах: там register.self != 'master' и flow-control пропускает.
// ──────────────────────────────────────────────────────────────────────────

// remove_replica = единственный Passage (passage 0): три top-level задачи, ни
// одна не читает register ДРУГОЙ задачи (task #1 — register.self, исключён из
// passage-стратификации, shared/config/task_refs.go) → plan_index = позиция.
const (
	removeReplicaProbePlanIdx  = 0 // Detect role of the replica being removed
	removeReplicaGuardPlanIdx  = 1 // Refuse to remove the current primary (failed_when)
	removeReplicaStopPlanIdx   = 2 // Stop redis on the replica being removed
	removeReplicaSinglePassage = 0
)

// sbgBaselineState — incarnation.state до remove_replica: redis_hosts несёт запись
// удаляемого хоста (host-0 master). Если split-brain guard НЕ сработает и remove
// дойдёт до коммита — запись исчезнет; assert «state unchanged» это поймал бы.
func sbgBaselineState(masterSID string) map[string]any {
	return map[string]any{
		"redis_version": "7.2.4",
		"redis_users":   map[string]any{},
		"redis_config":  map[string]any{"appendonly": "yes"},
		"redis_hosts": []any{
			map[string]any{"sid": masterSID, "role": "master"},
		},
	}
}

func TestL3bRedisCluster_SplitBrainGuard(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: rcuExample,
		ServiceName: rcuService,
		Souls:       3,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 3 {
		t.Fatalf("ожидалось 3 soul-контейнера, получено %d", got)
	}
	const (
		masterSID   = "soul-live-a.example.com"
		replica1SID = "soul-live-b.example.com"
		replica2SID = "soul-live-c.example.com"
	)
	wantSIDs := []string{masterSID, replica1SID, replica2SID}
	for i, want := range wantSIDs {
		if got := stack.SoulContainers[i].SID; got != want {
			t.Fatalf("SoulContainers[%d].SID = %q, ожидалось %q", i, got, want)
		}
	}

	const incName = "redis-cluster-split-brain"

	for i := range stack.SoulContainers {
		stack.AddSoulToCoven(t, i, incName)
	}

	// Прямой seed ready-incarnation с baseline, несущим запись host-0 в redis_hosts.
	stack.SeedIncarnationReady(t, incName, rcuService, "main", sbgBaselineState(masterSID))

	// ── Bootstrap живого redis: host-0 master, host-1/2 REPLICAOF host-0 ───────
	bootstrapLiveRedis(t, stack)

	// remove_replica таргетит ровно host-0 (where: soulprint.self.sid == input.sid).
	// host-0 — MASTER → split-brain guard ДОЛЖЕН сработать.
	applyID := stack.RunScenario(t, incName, "remove_replica", map[string]any{
		"sid": masterSID,
	})

	// ── (1) прогон упал (fail-stop), incarnation осталась error_locked ─────────
	// WaitApplySuccess здесь НЕЛЬЗЯ — он fatal-ит на terminal-failed. Ждём именно
	// error_locked: достижение ready означало бы, что guard НЕ сработал (регресс).
	stack.WaitIncarnationStatus(t, incName, "error_locked", 120)
	// host-0 (целевой MASTER) = failed (guard); реплики filtered (все задачи
	// where: sid==input.sid) → 0 задач → no_match (не failed). AssertApplyRunsStatus
	// (единый статус ВСЕХ строк) тут неприменим — нужен per-host разрез.
	stack.AssertApplyHostStatus(t, applyID, masterSID, "failed")
	stack.AssertApplyHostStatus(t, applyID, replica1SID, "no_match")
	stack.AssertApplyHostStatus(t, applyID, replica2SID, "no_match")

	// ── (2) ★ split-brain guard FAILED с error.code flowcontrol.failed_when ────
	// FC-0 helper читает audit_log task.executed (correlation_id=apply_id) хоста
	// host-0, plan_index=1 (Refuse-задача), passage 0. error.code доказывает КЛАСС
	// падения — не модульная ошибка, а именно flow-control failed_when по stdout.
	stack.AssertTaskStatus(t, applyID, masterSID,
		removeReplicaGuardPlanIdx, removeReplicaSinglePassage, "TASK_STATUS_FAILED")
	stack.AssertTaskErrorCode(t, applyID, masterSID,
		removeReplicaGuardPlanIdx, removeReplicaSinglePassage, "flowcontrol.failed_when")

	// probe (plan_index 0) на host-0 вернул чистый 'master' (trim-safe) — это и
	// есть вход, по которому failed_when сравнил == 'master'. register.self у
	// guard-задачи — её собственный stdout того же probe-командного шага.
	stack.AssertTaskRegisterField(t, applyID, masterSID, removeReplicaProbePlanIdx, "stdout", "master")

	// ── (3) fail-stop ДО «Stop redis»: redis на host-0 ЖИВ ─────────────────────
	// task #2 (Stop redis, plan_index 2) НЕ должна была исполниться — guard оборвал
	// прогон. Independent verify: redis-cli ping на host-0 = PONG (primary защищён).
	assertRedisAlive(t, stack, 0)

	// ── (4) state НЕ изменён: запись host-0 всё ещё в redis_hosts ──────────────
	// remove из redis_hosts НЕ закоммичен (fail-stop до барьера). Запись host-0 на
	// месте — защита primary доказана и на уровне persisted state.
	stack.AssertIncarnationState(t, incName, map[string]any{
		"redis_hosts": []any{
			map[string]any{"sid": masterSID, "role": "master"},
		},
	})

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "remove_replica",
		"apply_id": applyID,
	})
}

// assertRedisAlive фейлит, если redis на i-м контейнере не отвечает PONG.
// Independent verify, что fail-stop оборвал прогон ДО разрушительного шага
// «Stop redis» (task #2 не исполнилась).
func assertRedisAlive(t *testing.T, stack *harness.Stack, soulIdx int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := stack.SoulContainers[soulIdx].Exec(ctx,
		[]string{"redis-cli", "-p", "6379", "ping"})
	if err != nil {
		t.Fatalf("assertRedisAlive[%d]: exec: %v", soulIdx, err)
	}
	if code != 0 || !strings.Contains(out, "PONG") {
		t.Fatalf("★ assertRedisAlive[%d]: redis НЕ отвечает PONG (exit=%d out=%q) — fail-stop НЕ защитил primary, «Stop redis» исполнилась несмотря на guard",
			soulIdx, code, out)
	}
}
