//go:build e2e_live

// FC-5 L3b real-apply: per-task `when:` register-gating on GENUINE register,
// executed by a real soul against a LIVE redis in three Debian-12 containers.
//
// WHY (the gap we're closing). L3a-stub does NOT evaluate when (soul-stub returns
// a predetermined status without running the flow-control engine), so
// register-dependent `when:` gating - a Soul-side per-task decision BEFORE Apply - is
// not proven on real register. FC-1 (redis_cluster_update_acl_test.go) proved the
// SYMMETRIC but DIFFERENT path - `where:` targeting:
//
//   where = Keeper-side targeting. A host with where:false gets NO task at all:
//           at the apply_runs level it either has no passage row,
//           or no_match. The task doesn't exist on it.
//   when  = Soul-side per-task gating. A task WITHOUT where: goes to ALL hosts
//           of the run; Soul evaluates when via the sandboxed-cel engine and on
//           when:false emits skippedTaskEvent -> TASK_STATUS_SKIPPED (mod.Apply is not called),
//           as a separate task.executed row in audit_log.
//
// This is exactly the observable difference the test proves on real-apply:
// register-gated when produces a per-task SKIPPED EVENT (audit_log), NOT no_match at
// the apply_runs level (like where).
//
// SCENARIO (tests/e2e-live/when-gate-live, a local fixture in the test's own scope, does NOT
// touch committed examples/):
//   probe   - `redis-cli role | head -1 | tr -d '\n'` -> register: redis_role
//             (LIVE redis: host-0 master, host-1/2 REPLICAOF host-0 -> role
//             master vs slave). tr -d '\n' - register.stdout without a trailing \n.
//   action  - a task WITHOUT where:, `when: register.redis_role.stdout == 'master'`,
//             core.cmd.shell writes a marker file, changed_when: false. BOTH steps in
//             ONE Passage 0: register-dependent `when:` does NOT split the Passage
//             (FC-5 narrow-fix, ADR-056:85 - flow-control is NOT passage-defining;
//             soul-lint passage_plan = single-passage, NOT [1 1] as before the fix).
//             keeper carries when AS A CEL STRING in RenderedTask.when (does NOT
//             evaluate it - register is known only to Soul), Soul sees the register
//             same-passage in its ApplyRequest. Soul: master -> OK, replicas -> SKIPPED.
//
// * trim: the probe-cmd carries `tr -d '\n'` (the cmd module does NOT trim stdout - the same
// finding as FC-1 update_acl). Without it register.redis_role.stdout = "master\n"
// and when:=='master' would not match -> master would be skipped as a replica (a false
// "gating works inverted"). The regression guard below fails on \n in register-stdout.
//
// Live-bootstrap of live redis (bootstrapLiveRedis) is reused from
// redis_cluster_update_acl_test.go (one package e2e_live_test) - stop-rule
// "chain -> map": don't proliferate a second redis-bootstrap.
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

const (
	wgService     = "when-gate-live"
	wgExamplePath = "tests/e2e-live/when-gate-live"

	// Plan when_role_gate: probe + the when-gated action - BOTH in Passage 0 (FC-5
	// narrow-fix, ADR-056:85): register-dependent `when:` does NOT split the Passage
	// (flow-control is NOT passage-defining), the action without its own where: stays
	// same-passage with probe -> Soul sees register same-passage -> when works.
	// plan_index - a GLOBAL cross-plan index over the whole plan (migration 079, §S1):
	// probe = 0, action = 1; BOTH passage = 0 (one Passage, soul-lint passage_plan
	// = single-passage, NOT [1 1] as before the narrow-fix).
	wgProbePlanIdx   = 0
	wgProbePassage   = 0
	wgActionPlanIdx  = 1
	wgActionPassage  = 0
	wgActionFilePath = "/tmp/when-gated-acted"
)

func TestFC5WhenGating_LiveRegister(t *testing.T) {
	// FC-5 narrow-fix (ADR-056:85, variant c): register-dependent `when:` NO LONGER
	// splits the Passage (flow-control removed from collectTaskReads). probe + the when-gated
	// action (WITHOUT its own where:) is now SAME-passage (Passage 0) -> Soul sees
	// the register same-passage in its ApplyRequest -> when works: master -> OK, replicas
	// -> SKIPPED. Genuinely cross-passage when (probe in an earlier Passage for a DIFFERENT
	// reason) - UNSUPPORTED, caught by soul-lint/keeper `cross_passage_when_unsupported`
	// (a unit-guard in shared/config + soul-lint testdata, NOT this live scenario).
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: wgExamplePath,
		ServiceName: wgService,
		Souls:       3,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 3 {
		t.Fatalf("expected 3 soul containers, got %d", got)
	}
	const (
		masterSID   = "soul-live-a.example.com"
		replica1SID = "soul-live-b.example.com"
		replica2SID = "soul-live-c.example.com"
	)
	wantSIDs := []string{masterSID, replica1SID, replica2SID}
	for i, want := range wantSIDs {
		if got := stack.SoulContainers[i].SID; got != want {
			t.Fatalf("SoulContainers[%d].SID = %q, expected %q", i, got, want)
		}
	}

	const incName = "when-gate-live-run"

	// Coven membership BEFORE running the scenario: the roster resolves via incarnation.name ∈
	// coven[] (ADR-008). Without it no_hosts -> zero apply_runs rows.
	for i := range stack.SoulContainers {
		stack.AddSoulToCoven(t, i, incName)
	}

	// Direct seed of a ready incarnation (state_schema is empty, the focus is gating, not state).
	stack.SeedIncarnationReady(t, incName, wgService, "main", map[string]any{})

	// ── Bootstrap LIVE redis: host-0 master, host-1/2 REPLICAOF host-0 ────────
	// Makes the probe `redis-cli role` distinguishable (master vs slave) on a real
	// apply. Reused from redis_cluster_update_acl_test.go (one package).
	bootstrapLiveRedis(t, stack)

	applyID := stack.RunScenario(t, incName, "when_role_gate", nil)

	stack.WaitApplySuccess(t, applyID, 180)
	stack.WaitIncarnationReady(t, incName, 30)

	// ── (1) probe-register: the real soul returned each host's role ────────────
	// Passage 0, plan_index 0. register stdout - what the real soul printed
	// from `redis-cli role | head -1 | tr -d '\n'`.
	masterRole := registerStdout(t, stack, applyID, masterSID)
	r1Role := registerStdout(t, stack, applyID, replica1SID)
	r2Role := registerStdout(t, stack, applyID, replica2SID)
	t.Logf("probe register: master=%q replica1=%q replica2=%q", masterRole, r1Role, r2Role)

	// * trim regression guard: register.redis_role.stdout with a trailing \n would break
	// when:=='master' (master would be skipped as a replica - a false "gating is inverted").
	if strings.Contains(masterRole, "\n") {
		t.Fatalf("* probe-newline regression: master register stdout = %q contains \\n - probe `redis-cli role | head -1 | tr -d '\\n'` lost the trim; when:=='master' won't match on real register (restore tr -d in tests/e2e-live/when-gate-live/scenario/when_role_gate/main.yml)", masterRole)
	}
	if strings.TrimSpace(masterRole) != "master" {
		t.Fatalf("probe: master host returned role=%q, expected master (redis-bootstrap incorrect?)", masterRole)
	}
	if strings.TrimSpace(r1Role) != "slave" || strings.TrimSpace(r2Role) != "slave" {
		t.Fatalf("probe: replicas returned role r1=%q r2=%q, expected slave (REPLICAOF didn't converge?)", r1Role, r2Role)
	}

	// FC-0 canonical key: probe register stdout at plan_index 0 == 'master'/'slave'.
	// This is exactly the input Soul used to evaluate when: register.redis_role.stdout.
	stack.AssertTaskRegisterField(t, applyID, masterSID, wgProbePlanIdx, "stdout", "master")
	stack.AssertTaskRegisterField(t, applyID, replica1SID, wgProbePlanIdx, "stdout", "slave")
	stack.AssertTaskRegisterField(t, applyID, replica2SID, wgProbePlanIdx, "stdout", "slave")

	// ── (2) ★ КЛЮЧ FC-5: per-task when-gating Soul-side по реальному register ───
	// Действие БЕЗ where: дошло до ВСЕХ трёх хостов (apply_runs success у каждого —
	// WaitApplySuccess это уже доказал). Soul-side when:
	//   master  → when:true  → задача исполнилась → TASK_STATUS_OK (changed_when: false).
	//   реплики → when:false → Soul эмитит TASK_STATUS_SKIPPED ДО Apply.
	// AssertTaskStatus читает per-task task.executed-строку audit_log (FC-0):
	// SKIPPED-событие ПЕРСИСТИТСЯ как отдельная строка — это и есть отличие от
	// where (где на реплике задачи нет вовсе).
	stack.AssertTaskStatus(t, applyID, masterSID, wgActionPlanIdx, wgActionPassage, "TASK_STATUS_OK")
	stack.AssertTaskStatus(t, applyID, replica1SID, wgActionPlanIdx, wgActionPassage, "TASK_STATUS_SKIPPED")
	stack.AssertTaskStatus(t, applyID, replica2SID, wgActionPlanIdx, wgActionPassage, "TASK_STATUS_SKIPPED")

	// ── (3) ★ Отличие when от where ДОКАЗАНО на персистентности ────────────────
	// when даёт SKIPPED-СОБЫТИЕ (task.executed-строка существует) на репликах, а НЕ
	// no_match/отсутствие apply_runs-строки. Прямой контр-ассерт: у where (FC-1
	// update_acl) реплика на where-задаче имеет apply_runs(passage=1)=no_match ЛИБО
	// строки нет; у when реплика имеет apply_runs success (задача дошла), а per-task
	// SKIPPED живёт в audit_log. Доказываем обе грани:
	//   3a. apply_runs реплики = success (НЕ no_match) — задача дошла до хоста.
	assertWhenReplicaApplyRunSucceeded(t, stack, applyID, replica1SID, replica2SID)
	//   3b. task.executed-строка для SKIPPED-действия реплики РЕАЛЬНО есть (персист).
	assertTaskEventPersisted(t, stack, applyID, replica1SID, wgActionPlanIdx, wgActionPassage)
	assertTaskEventPersisted(t, stack, applyID, replica2SID, wgActionPlanIdx, wgActionPassage)

	// ── (4) Independent verify на ЖИВЫХ контейнерах: маркер-файл ───────────────
	// when-gated cmd писал /tmp/when-gated-acted. master выполнил → файл есть;
	// реплики SKIPPED (mod.Apply не зван) → файла НЕТ. Доказывает, что SKIPPED —
	// настоящий skip Apply, а не «выполнилась, но статус занижен».
	stack.AssertHostFileExists(t, 0, wgActionFilePath)
	assertHostFileAbsent(t, stack, 1, wgActionFilePath)
	assertHostFileAbsent(t, stack, 2, wgActionFilePath)

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "when_role_gate",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// assertWhenReplicaApplyRunSucceeded проверяет, что реплики получили apply_runs со
// статусом success (НЕ no_match) — то есть when-gated задача ДОШЛА до хоста (в
// отличие от where, отфильтровавшего бы хост из таргета). per-task SKIPPED не
// меняет терминал apply_runs хоста: SKIPPED — нейтральный исход (не fail, не
// no_match), хост штатно завершает прогон success.
func assertWhenReplicaApplyRunSucceeded(t *testing.T, stack *harness.Stack, applyID string, replicaSIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, sid := range replicaSIDs {
		var status string
		err := stack.DB().QueryRow(ctx,
			`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid).Scan(&status)
		if err != nil {
			t.Fatalf("★ when≠where: реплика %s НЕ имеет apply_runs-строки — задача не дошла до хоста (это поведение where, не when): %v", sid, err)
		}
		if status == "no_match" {
			t.Fatalf("★ when≠where: реплика %s apply_runs status=no_match — хост отфильтрован из таргета (where-семантика); when обязан ДОСТАВИТЬ задачу и скипнуть её Soul-side, а не отсеять хост", sid)
		}
		if status != "success" {
			t.Fatalf("★ реплика %s apply_runs status=%q, ожидался success (per-task SKIPPED — нейтральный исход, хост должен завершить прогон success)", sid, status)
		}
	}
}

// assertTaskEventPersisted проверяет, что per-task task.executed-строка для задачи
// (apply_id, sid, plan_index, passage) РЕАЛЬНО есть в audit_log — это и есть
// материальное отличие when от where: SKIPPED-событие персистится отдельной
// строкой (соул эмитит skippedTaskEvent), тогда как where-отфильтрованный хост
// task.executed-строки этой задачи не имеет вовсе.
func assertTaskEventPersisted(t *testing.T, stack *harness.Stack, applyID, sid string, planIdx, passage int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	err := stack.DB().QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1
		  AND payload->>'sid' = $2
		  AND (payload->>'plan_index')::int = $3
		  AND (payload->>'passage')::int = $4
	`, applyID, sid, planIdx, passage).Scan(&n)
	if err != nil {
		t.Fatalf("assertTaskEventPersisted(sid=%s plan_index=%d passage=%d): query: %v", sid, planIdx, passage, err)
	}
	if n == 0 {
		t.Fatalf("★ when≠where: для реплики %s НЕТ task.executed-строки when-gated задачи (plan_index=%d passage=%d) — SKIPPED-событие не персистнуто; when обязан эмитить per-task SKIPPED-событие, а не молча выкинуть задачу как where", sid, planIdx, passage)
	}
}
