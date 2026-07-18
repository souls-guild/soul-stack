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
//	where = Keeper-side targeting. A host with where:false gets NO task at all:
//	        at the apply_runs level it either has no passage row,
//	        or no_match. The task doesn't exist on it.
//	when  = Soul-side per-task gating. A task WITHOUT where: goes to ALL hosts
//	        of the run; Soul evaluates when via the sandboxed-cel engine and on
//	        when:false emits skippedTaskEvent -> TASK_STATUS_SKIPPED (mod.Apply is not called),
//	        as a separate task.executed row in audit_log.
//
// This is exactly the observable difference the test proves on real-apply:
// register-gated when produces a per-task SKIPPED EVENT (audit_log), NOT no_match at
// the apply_runs level (like where).
//
// SCENARIO (tests/e2e-live/when-gate-live, a local fixture in the test's own scope, does NOT
// touch committed examples/):
//
//	probe   - `redis-cli role | head -1 | tr -d '\n'` -> register: redis_role
//	          (LIVE redis: host-0 master, host-1/2 REPLICAOF host-0 -> role
//	          master vs slave). tr -d '\n' - register.stdout without a trailing \n.
//	action  - a task WITHOUT where:, `when: register.redis_role.stdout == 'master'`,
//	          core.cmd.shell writes a marker file, changed_when: false. BOTH steps in
//	          ONE Passage 0: register-dependent `when:` does NOT split the Passage
//	          (FC-5 narrow-fix, ADR-056:85 - flow-control is NOT passage-defining;
//	          soul-lint passage_plan = single-passage, NOT [1 1] as before the fix).
//	          keeper carries when AS A CEL STRING in RenderedTask.when (does NOT
//	          evaluate it - register is known only to Soul), Soul sees the register
//	          same-passage in its ApplyRequest. Soul: master -> OK, replicas -> SKIPPED.
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

	// Membership BEFORE running the scenario: the roster resolves members via
	// incarnation_membership (ADR-008 amendment/NIM-124). Without it no_hosts -> zero apply_runs rows.
	for i := range stack.SoulContainers {
		stack.AddMember(t, i, incName)
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

	// (2) FC-5 key check: per-task Soul-side when-gating by real register.
	// Action WITHOUT where reached ALL three hosts (apply_runs success for each;
	// WaitApplySuccess already proved this). Soul-side when:
	//   master   -> when:true  -> task executed -> TASK_STATUS_OK (changed_when: false).
	//   replicas -> when:false -> Soul emits TASK_STATUS_SKIPPED BEFORE Apply.
	// AssertTaskStatus reads per-task task.executed audit_log row (FC-0):
	// SKIPPED event is PERSISTED as a separate row; this is the difference from
	// where (where the replica has no such task at all).
	stack.AssertTaskStatus(t, applyID, masterSID, wgActionPlanIdx, wgActionPassage, "TASK_STATUS_OK")
	stack.AssertTaskStatus(t, applyID, replica1SID, wgActionPlanIdx, wgActionPassage, "TASK_STATUS_SKIPPED")
	stack.AssertTaskStatus(t, applyID, replica2SID, wgActionPlanIdx, wgActionPassage, "TASK_STATUS_SKIPPED")

	// (3) when vs where difference is proven by persistence.
	// when produces a SKIPPED EVENT (task.executed row exists) on replicas, not
	// no_match or missing apply_runs row. Direct counter-assert: with where (FC-1
	// update_acl), a replica on a where task has apply_runs(passage=1)=no_match OR
	// no row; with when, the replica has apply_runs success (task was delivered),
	// and per-task SKIPPED lives in audit_log. Prove both sides:
	//   3a. replica apply_runs = success (NOT no_match) - task reached the host.
	assertWhenReplicaApplyRunSucceeded(t, stack, applyID, replica1SID, replica2SID)
	//   3b. task.executed row for replica SKIPPED action really exists (persistence).
	assertTaskEventPersisted(t, stack, applyID, replica1SID, wgActionPlanIdx, wgActionPassage)
	assertTaskEventPersisted(t, stack, applyID, replica2SID, wgActionPlanIdx, wgActionPassage)

	// (4) Independent verify on LIVE containers: marker file.
	// when-gated cmd wrote /tmp/when-gated-acted. master executed -> file exists;
	// replicas were SKIPPED (mod.Apply not called) -> file is ABSENT. Proves
	// SKIPPED is a real Apply skip, not "executed but status was downgraded".
	stack.AssertHostFileExists(t, 0, wgActionFilePath)
	assertHostFileAbsent(t, stack, 1, wgActionFilePath)
	assertHostFileAbsent(t, stack, 2, wgActionFilePath)

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "when_role_gate",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// assertWhenReplicaApplyRunSucceeded checks that replicas received apply_runs
// with status success (NOT no_match), meaning the when-gated task reached the
// host (unlike where, which would filter the host out of target). per-task
// SKIPPED does not change host apply_runs terminal: SKIPPED is a neutral
// outcome (not fail, not no_match), and the host completes the run as success.
func assertWhenReplicaApplyRunSucceeded(t *testing.T, stack *harness.Stack, applyID string, replicaSIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, sid := range replicaSIDs {
		var status string
		err := stack.DB().QueryRow(ctx,
			`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid).Scan(&status)
		if err != nil {
			t.Fatalf("when vs where: replica %s has NO apply_runs row - task did not reach the host (where behavior, not when): %v", sid, err)
		}
		if status == "no_match" {
			t.Fatalf("when vs where: replica %s apply_runs status=no_match - host was filtered from target (where semantics); when must DELIVER the task and skip it Soul-side, not filter out the host", sid)
		}
		if status != "success" {
			t.Fatalf("replica %s apply_runs status=%q, expected success (per-task SKIPPED is a neutral outcome, host must finish the run as success)", sid, status)
		}
	}
}

// assertTaskEventPersisted checks that the per-task task.executed row for task
// (apply_id, sid, plan_index, passage) really exists in audit_log. This is the
// material difference between when and where: SKIPPED event is persisted as a
// separate row (Soul emits skippedTaskEvent), while a where-filtered host has
// no task.executed row for this task at all.
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
		t.Fatalf("when vs where: replica %s has NO task.executed row for when-gated task (plan_index=%d passage=%d) - SKIPPED event was not persisted; when must emit per-task SKIPPED event, not silently drop the task like where", sid, planIdx, passage)
	}
}
