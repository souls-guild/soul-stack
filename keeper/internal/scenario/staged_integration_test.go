//go:build integration

// 2-passage contract proof for staged-render (ADR-056, S3). Minimal staged
// probe→where scenario on the contract tier: Passage 0 — probe role (register:
// role), emitting a per-host fact ('master' on one host, 'slave' on the other);
// Passage 1 — action `where: register.role.stdout == 'master'`. ASSERT: the
// Passage-1 ApplyRequest goes ONLY to the master host (where resolved via the
// Passage 0 register). Proves staged-render end-to-end on the contract tier —
// closes the "register in where is always empty" drift (ADR-056 §Context).
//
// Full live redis-cluster (cloud/vault-scope) is NOT covered here (S4/S5). Soul
// is simulated by stagedDispatcher (same path as mockDispatcher: SendApply →
// per-task register + terminal apply_runs status, mirroring
// accumulateRegister/correlateRunResult in prod).

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// stagedServiceRepo creates a service repo with a staged `failover` scenario:
// Passage 0 — probe role (register: role), Passage 1 — action on master.
func stagedServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: staged-render proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/failover/main.yml", `name: failover
description: probe role (passage 0) then act on master (passage 1)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Act on master only
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    params:
      cmd: promote
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init staged service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// stagedDispatcher simulates Soul under a staged run (contract tier L3a):
//   - Passage 0 (probe): for each host, writes register `role` with per-host
//     stdout (roleBySID) under (apply_id, sid, passage=0) — mirrors
//     accumulateRegister on TaskEvent.register_data in prod; then terminates the
//     passage=0 row as success — mirrors correlateRunResult on RunResult.passage=0.
//   - Passage>0 (action): terminates this passage's row as success AND records
//     which SIDs received the Passage-N ApplyRequest (targetedByPassage) — the
//     proof of targeting.
//
// passage comes from req.Passage (echoed ApplyRequest.passage) — same contract
// Soul uses to echo passage in TaskEvent/RunResult.
//
// failPassage0On — SIDs where probe (passage 0) ends FAILED instead of success:
// terminates that host's passage-0 row as `failed` (mirrors RunResult.status=
// FAILED in prod). The passage-0 barrier is fail-closed and aborts the ENTIRE
// run → passage-1 is never dispatched. No register is written for these hosts
// (probe failed).
type stagedDispatcher struct {
	t              *testing.T
	roleBySID      map[string]string // sid → probe stdout (per-host probe result)
	failPassage0On map[string]bool   // sid → probe (passage 0) ends FAILED

	mu                sync.Mutex
	targetedByPassage map[int][]string // passage → SIDs that received an ApplyRequest
	// dispatchedPlan — every (passage, plan_index, name) from req.Tasks[] of each
	// ApplyRequest = exactly what Soul would echo in TaskEvent.plan_index → audit
	// task.executed. Ground truth for guard H1 (NIM-37): checked against
	// apply_run_plan (persistRunPlan). dispatchedPlanByIndex dedups by plan_index
	// — passage-1 dispatches to N hosts.
	dispatchedPlan []dispatchedTask
}

// dispatchedTask — one rendered task actually sent in an ApplyRequest (echoed
// plan_index/name of the passage's active render).
type dispatchedTask struct {
	passage   int
	planIndex int
	name      string
}

func newStagedDispatcher(t *testing.T, roleBySID map[string]string) *stagedDispatcher {
	return &stagedDispatcher{t: t, roleBySID: roleBySID, targetedByPassage: map[int][]string{}}
}

// dispatchedPlanByIndex collapses dispatchedPlan into plan_index → (passage,
// name), deduping tasks sent to multiple hosts within the same passage.
func (d *stagedDispatcher) dispatchedPlanByIndex() map[int]dispatchedTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[int]dispatchedTask, len(d.dispatchedPlan))
	for _, dt := range d.dispatchedPlan {
		out[dt.planIndex] = dt
	}
	return out
}

// emitterIndices mirrors the Soul contract (ADR-056 §S1 fix Variant B): for the
// register task named register, finds its LOCAL position in req.Tasks[]
// (= TaskEvent.task_idx, local to this passage/host) and its GLOBAL plan_index
// (= req.Tasks[i].plan_index, echoing TaskEvent.plan_index). Mirrors exactly what
// sendTaskEvent does on Soul: task_idx = loop idx, plan_index = task.GetPlanIndex().
// Register correlation on Keeper keys on plan_index — so the harness must
// distinguish local vs global indices, or it would mask the bug like before.
func emitterIndices(req *keeperv1.ApplyRequest, register string) (localIdx, planIdx int) {
	for i, task := range req.GetTasks() {
		if task.GetRegister() == register {
			return i, int(task.GetPlanIndex())
		}
	}
	return -1, -1
}

func (d *stagedDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()

	d.mu.Lock()
	d.targetedByPassage[passage] = append(d.targetedByPassage[passage], sid)
	for _, task := range req.GetTasks() {
		d.dispatchedPlan = append(d.dispatchedPlan, dispatchedTask{
			passage:   passage,
			planIndex: int(task.GetPlanIndex()),
			name:      task.GetName(),
		})
	}
	d.mu.Unlock()

	if passage == 0 && d.failPassage0On[sid] {
		// probe failed on this host: terminate the passage-0 row as failed
		// (mirrors RunResult.status=FAILED). No register — probe produced no fact.
		summary := "probe failed"
		if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, 0, applyrun.StatusFailed, &summary); err != nil {
			d.t.Errorf("stagedDispatcher: UpdateStatus(%s, passage=0, failed): %v", sid, err)
		}
		return nil
	}

	if passage == 0 {
		// probe task `role`. task_idx is the LOCAL position in req.Tasks[] (as
		// Soul emits TaskEvent.task_idx), plan_index is the GLOBAL echo of
		// req.Tasks[i].plan_index (as Soul echoes TaskEvent.plan_index, ADR-056
		// §S1 fix Variant B). Register keys on plan_index — the fixed path.
		role := d.roleBySID[sid]
		localIdx, planIdx := emitterIndices(req, "role")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID:      applyID,
			SID:          sid,
			PlanIndex:    planIdx,
			TaskIdx:      localIdx,
			RegisterData: map[string]any{"stdout": role, "changed": false, "failed": false},
			Passage:      0,
		}); err != nil {
			d.t.Errorf("stagedDispatcher: UpsertTaskRegister(%s): %v", sid, err)
		}
	}

	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("stagedDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *stagedDispatcher) targets(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.targetedByPassage[passage]...)
	return out
}

// TestIntegration_2Passage_WhereTargetsOnlyMaster — ★ 2-PASSAGE PROOF (ADR-056, S3).
// Two hosts: host-a master, host-b slave (per-host probe in Passage 0). Passage 1
// carries `where: register.role.stdout == 'master'`. ASSERT: the Passage-1
// ApplyRequest targets ONLY host-a (master) — where resolved via the Passage 0
// register end-to-end through the stage loop (render→dispatch→barrier→register→
// render).
func TestIntegration_2Passage_WhereTargetsOnlyMaster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Run succeeds (both passages cleared their barriers, state-commit → ready).
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0 (probe): ApplyRequest to BOTH hosts (probe has no where → whole roster).
	p0 := disp.targets(0)
	if len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want both hosts", p0)
	}

	// ★ Passage 1 (action): ApplyRequest ONLY to the master host. where:
	// register.role.stdout == 'master' resolved via the Passage 0 per-host register.
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("* Passage 1 targets = %v, want [host-a.example.com] (only master) - staged where did not resolve via Passage 0 register", p1)
	}

	// apply_runs: per-host × per-passage. host-a: passage 0 + passage 1 (master).
	// host-b: passage 0 ONLY (slave — Passage 1 never targeted it, no row).
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	got := map[string][]int{}
	for _, st := range statuses {
		if st.Status != applyrun.StatusSuccess {
			t.Errorf("apply_runs[%s,passage=%d] = %s, want success", st.SID, st.Passage, st.Status)
		}
		got[st.SID] = append(got[st.SID], st.Passage)
	}
	if len(got["host-a.example.com"]) != 2 {
		t.Errorf("host-a passages = %v, want [0 1] (probe + master-action)", got["host-a.example.com"])
	}
	if len(got["host-b.example.com"]) != 1 || got["host-b.example.com"][0] != 0 {
		t.Errorf("host-b passages = %v, want [0] (probe only - slave was not targeted by Passage 1)", got["host-b.example.com"])
	}
}

// stagedExpandingServiceRepo — staged scenario where the Passage 1 task EXPANDS
// (loop over 2 items) into N>1 RenderedTask with real plan_index values. At the
// step-5 render (ActivePassage=0) it's ONE collapsed placeholder (index 1); on
// the Passage 1 re-render (ActivePassage=1) it's 2 tasks (indices 1,2). Exactly
// case H1 (NIM-37): plan persistence must pull passage-1 from ITS active render,
// not the step-5 placeholder. where: register.role makes the task
// register-dependent → Passage 1 (Stratify).
func stagedExpandingServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: staged-render expanding-passage proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/expand/main.yml", `name: expand
description: probe role (p0) then fan-out loop on live hosts (p1 expands to N tasks)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Fan out
    module: core.exec.run
    where: "register.role.stdout != ''"
    loop:
      items: "${ ['alpha', 'beta'] }"
      as: item
    params:
      cmd: "configure ${ item }"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init staged expanding service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_StagedExpandingPassage_RunPlanMatchesExecution — ★ GUARD H1
// (NIM-37): in a staged run where the Passage 1 task expands (loop over 2
// items), persistRunPlan must pull apply_run_plan passage-1 from ITS active
// render (real plan_index 1,2), NOT the collapsed step-5 render placeholder
// (single index 1). INVARIANT: the set of plan_index in apply_run_plan
// (+name/passage) == the set of plan_index actually sent in an ApplyRequest
// (echoed TaskEvent.plan_index → audit task.executed). Without the fix,
// passage-1 persists as the placeholder (index 1) and apply_run_plan loses
// index 2 → the assertion fails.
func TestIntegration_StagedExpandingPassage_RunPlanMatchesExecution(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedExpandingServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "expand",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Ground truth: what actually went out in ApplyRequests (echoed plan_index of
	// each passage's active render) = what Soul would record in audit
	// task.executed. probe idx0 (P0) + loop idx1,2 (P1) = 3 unique plan_index.
	exec := disp.dispatchedPlanByIndex()
	if len(exec) != 3 {
		t.Fatalf("dispatched plan_index set = %v, want 3 (probe idx0 + loop idx1,2 unrolled from Passage 1)", exec)
	}

	// apply_run_plan (persistRunPlan) must match execution both in the set of
	// plan_index and in name/passage for each index.
	plan, err := applyrun.SelectRunPlanByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectRunPlanByApplyID: %v", err)
	}
	planByIndex := map[int]applyrun.RunPlanTask{}
	for _, p := range plan {
		planByIndex[p.PlanIndex] = p
	}
	if len(planByIndex) != len(exec) {
		var planIdx, execIdx []int
		for k := range planByIndex {
			planIdx = append(planIdx, k)
		}
		for k := range exec {
			execIdx = append(execIdx, k)
		}
		sort.Ints(planIdx)
		sort.Ints(execIdx)
		t.Fatalf("* H1: apply_run_plan plan_index = %v, want == execution %v (staged passage-1 was recorded with a compressed placeholder instead of the active render - index lost)", planIdx, execIdx)
	}
	for idx, dt := range exec {
		p, ok := planByIndex[idx]
		if !ok {
			t.Fatalf("* H1: plan_index %d executed (passage %d, %q), but missing from apply_run_plan - persistRunPlan recorded the compressed placeholder instead of the unrolled render of Passage 1", idx, dt.passage, dt.name)
		}
		if p.Passage != dt.passage || p.Name != dt.name {
			t.Errorf("plan_index %d: apply_run_plan (passage=%d, name=%q) != execution (passage=%d, name=%q)", idx, p.Passage, p.Name, dt.passage, dt.name)
		}
	}
}

// passagesBySID collects a run's apply_runs into (sid → sorted passages) and
// checks that every row is terminal with the expected wantStatus. Used by guard
// tests to assert "which hosts got an apply_runs row for which passage" —
// targeting proof on the DB side, symmetric to disp.targets() on the dispatch side.
func passagesBySID(t *testing.T, applyID string, wantStatus applyrun.Status) map[string][]int {
	t.Helper()
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	got := map[string][]int{}
	for _, st := range statuses {
		if st.Status != wantStatus {
			t.Errorf("apply_runs[%s,passage=%d] = %s, want %s", st.SID, st.Passage, st.Status, wantStatus)
		}
		got[st.SID] = append(got[st.SID], st.Passage)
	}
	return got
}

// TestIntegration_StagedAllSlave_NoOpReady — ★ TARGETING INVARIANT (ADR-056).
// Both hosts are slave (Passage 0 probe gives role='slave' on both). Passage 1
// carries `where: register.role.stdout == 'master'` → NO host passes the
// filter. ASSERT: the Passage-1 ApplyRequest is sent to NO host (empty
// destructive target), the Passage-1 barrier does NOT start and does NOT hang
// (dispatchPassage: len(perHost)==0 → no-op return nil), incarnation → READY.
// An empty destructive target is a safe no-op, not a hang and not an error.
func TestIntegration_StagedAllSlave_NoOpReady(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "slave",
		"host-b.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// An empty Passage 1 destructive target does NOT hang: the Passage-1 barrier
	// never starts (no-op), the run reaches commitSuccess → ready (if the
	// Passage-1 barrier hung on an empty set, waitRunDone would time out).
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0 (probe): both hosts (probe has no where → whole roster).
	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want both hosts", p0)
	}

	// ★ Passage 1: NO ApplyRequest at all (all slave → where=false on each).
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("* Passage 1 targets = %v, want [] (all-slave: destructive target is empty, no-op)", p1)
	}

	// apply_runs: both hosts have ONLY passage 0 (no Passage-1 row — nobody was
	// targeted). All rows success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		if len(got[sid]) != 1 || got[sid][0] != 0 {
			t.Errorf("%s passages = %v, want [0] (probe only - Passage 1 was not targeted)", sid, got[sid])
		}
	}
}

// TestIntegration_StagedProbeFail_FailStop — ★ LIFECYCLE INVARIANT (ADR-056 §d).
// probe (Passage 0) ends FAILED on a host. ASSERT: the Passage-0 barrier is
// fail-closed and aborts the run BEFORE the stage loop advances to Passage 1 →
// the Passage-1 ApplyRequest is sent to NO host → incarnation → ERROR_LOCKED.
// A failed probe step must not dispatch a dependent passage on an incomplete
// register.
func TestIntegration_StagedProbeFail_FailStop(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master", // role doesn't matter — probe fails before register
	})
	disp.failPassage0On = map[string]bool{"host-a.example.com": true}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (barrier Passage-0 fail-stop)", inc.StatusDetails["reason"])
	}

	// Passage 0 was targeted (probe went out), Passage 1 — NOT (the stage loop
	// broke off at the Passage-0 barrier).
	if p0 := disp.targets(0); len(p0) != 1 {
		t.Errorf("Passage 0 targets = %v, want [host-a.example.com]", p0)
	}
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("* Passage 1 targets = %v, want [] (probe-fail stopped the run before Passage 1)", p1)
	}

	// apply_runs: the only row is passage 0 = failed (no Passage-1 row).
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("apply_runs rows = %d, want 1 (only probe passage 0)", len(statuses))
	}
	if statuses[0].Passage != 0 || statuses[0].Status != applyrun.StatusFailed {
		t.Errorf("apply_runs[0] = passage %d/%s, want passage 0/failed", statuses[0].Passage, statuses[0].Status)
	}
}

// TestIntegration_StagedAllMaster — both hosts role='master' → Passage 1 passes
// where on BOTH → both succeed → ready. Control case mirroring all-slave: proves
// that the empty Passage-1 in all-slave is a consequence of the where filter,
// not a broken staged loop in general.
func TestIntegration_StagedAllMaster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "master",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want both hosts", p0)
	}
	// ★ Passage 1: BOTH hosts (where master is true on each).
	p1 := disp.targets(1)
	if len(p1) != 2 {
		t.Fatalf("* Passage 1 targets = %v, want both hosts (all-master)", p1)
	}

	// apply_runs: both hosts have passage 0 + passage 1, all success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		if len(got[sid]) != 2 {
			t.Errorf("%s passages = %v, want [0 1] (probe + master-action)", sid, got[sid])
		}
	}
}

// TestIntegration_StagedPartialProbeFail_FailClosed — probe fails on ONE of two
// hosts (host-b), the other (host-a) gives role='master'. ASSERT: the Passage-0
// barrier is fail-closed and aborts the ENTIRE run (NOT "dispatch Passage 1 to
// surviving host-a on a partial register") → the Passage-1 ApplyRequest is sent
// to nobody → incarnation → ERROR_LOCKED.
func TestIntegration_StagedPartialProbeFail_FailClosed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master", // probe OK — would give master
		"host-b.example.com": "master",
	})
	disp.failPassage0On = map[string]bool{"host-b.example.com": true} // probe fails on host-b
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (partial probe-fail fails the whole run)", inc.StatusDetails["reason"])
	}

	// Passage 0 went to both hosts; ★ Passage 1 — to nobody (fail-closed:
	// surviving host-a does NOT get Passage-1 on a partial register).
	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want both hosts", p0)
	}
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("* Passage 1 targets = %v, want [] (partial probe-fail -> fail-closed, Passage 1 is not dispatched)", p1)
	}
}

// --- 3-Passage (restart re-probe) ----------------------------------------

// staged3PassageServiceRepo creates a service repo with a 3-passage `restart`
// scenario reproducing the canonical restart re-probe idiom (ADR-056 §"restart
// re-probe"):
//
//	#0 probe role               → Passage 0 (register: role)
//	#1 act where role==master   → Passage 1 (reads register.role — after probe)
//	#2 re-probe role_after      → Passage 1 (program-order edge: emitter AFTER #1)
//	#3 act where role_after==... → Passage 2 (reads register.role_after — after re-probe)
//
// Stratify yields TaskPassage [0,1,1,2], Count=3 (two probe boundaries). The
// Passage-2 task (#3) targets EXCLUSIVELY on register.role_after (Passage 1
// re-probe), NOT on the first probe's register.role (Passage 0) — this is what
// proves the program-order edge S2 + N-loop for N=3.
func staged3PassageServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: staged-render 3-passage proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/restart/main.yml", `name: restart
description: probe role (p0) → failover on master (p1) → re-probe role_after (p1) → act on new master (p2)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Failover on current master
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    params:
      cmd: failover
  - name: Re-probe role after failover
    module: core.exec.run
    register: role_after
    changed_when: "false"
    params:
      cmd: detect-role-again
  - name: Act on the new master
    module: core.exec.run
    where: "register.role_after.stdout == 'master'"
    params:
      cmd: restart-former-master
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init staged 3-passage service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// staged3Dispatcher simulates Soul under a 3-passage restart run (contract
// tier). Register emitters are given as (passage, global plan_index) → per-host
// stdout:
//   - probe `role`         — passage 0, plan_index 0 (roleP0BySID);
//   - re-probe `role_after` — passage 1, plan_index 2 / LOCAL task_idx 1 (roleP1BySID).
//
// Indices come from req.Tasks[] via emitterIndices (as Soul echoes them in
// TaskEvent), never hardcoded — otherwise the harness would mask a
// task_idx-collision bug.
//
// KEY to the proof: roleP1BySID differs from roleP0BySID — master changes after
// failover. If the Passage-2 task (#3) targeted on the OLD register.role (probe
// Passage 0), it would go to host-a; it must go to host-b (master per re-probe).
// targetedByPassage records the targeting fact.
type staged3Dispatcher struct {
	t           *testing.T
	roleP0BySID map[string]string // sid → probe stdout (passage 0, task_idx 0)
	roleP1BySID map[string]string // sid → re-probe stdout (passage 1, task_idx 2)

	mu                sync.Mutex
	targetedByPassage map[int][]string
}

func newStaged3Dispatcher(t *testing.T, p0, p1 map[string]string) *staged3Dispatcher {
	return &staged3Dispatcher{t: t, roleP0BySID: p0, roleP1BySID: p1, targetedByPassage: map[int][]string{}}
}

func (d *staged3Dispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()

	d.mu.Lock()
	d.targetedByPassage[passage] = append(d.targetedByPassage[passage], sid)
	d.mu.Unlock()

	switch passage {
	case 0:
		// probe `role` — global plan_index 0, local task_idx 0 (a single step in
		// Passage 0). Correlation key is plan_index (ADR-056 §S1 fix Variant B).
		localIdx, planIdx := emitterIndices(req, "role")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx,
			RegisterData: map[string]any{"stdout": d.roleP0BySID[sid], "changed": false, "failed": false},
			Passage:      0,
		}); err != nil {
			d.t.Errorf("staged3Dispatcher: UpsertTaskRegister role (%s): %v", sid, err)
		}
	case 1:
		// re-probe `role_after` — GLOBAL plan_index 2 (#2 in the full plan), but
		// LOCAL task_idx 1 (Passage 1 carries #1 failover at local 0 + #2 re-probe
		// at local 1). The harness used to write TaskIdx:2 (global masquerading as
		// local) — that's what hid the bug. Now register keys on the global
		// plan_index, with local task_idx ≠ global — the real path.
		localIdx, planIdx := emitterIndices(req, "role_after")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx,
			RegisterData: map[string]any{"stdout": d.roleP1BySID[sid], "changed": false, "failed": false},
			Passage:      1,
		}); err != nil {
			d.t.Errorf("staged3Dispatcher: UpsertTaskRegister role_after (%s): %v", sid, err)
		}
	}

	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("staged3Dispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *staged3Dispatcher) targets(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.targetedByPassage[passage]...)
	sort.Strings(out)
	return out
}

// TestIntegration_Staged3Passage_ReprobeRetargets — ★ 3-PASSAGE PROOF (ADR-056 §S4).
// Proves the program-order edge S2 + N-loop for N=3 on the restart re-probe idiom.
//
// Setup: before failover host-a is master, host-b is slave (Passage 0 probe).
// AFTER failover (Passage 1 action) roles swap — the Passage 1 re-probe gives
// host-a slave, host-b master. The Passage-2 task `where:
// register.role_after.stdout == 'master'` must target on the FRESH re-probe →
// ONLY host-b. If the program-order edge were broken and the re-probe ended up
// in Passage 0 (or Passage-2 where resolved against the first probe), the task
// would go to host-a (the OLD master) — the test catches that.
func TestIntegration_Staged3Passage_ReprobeRetargets(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := staged3PassageServiceRepo(t)

	disp := newStaged3Dispatcher(t,
		map[string]string{ // probe Passage 0: host-a master
			"host-a.example.com": "master",
			"host-b.example.com": "slave",
		},
		map[string]string{ // re-probe Passage 1: AFTER failover, host-b master
			"host-a.example.com": "slave",
			"host-b.example.com": "master",
		},
	)
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// All three passages (two probe boundaries) cleared their barriers → ready.
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0 (probe): both hosts (probe has no where).
	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want both hosts", p0)
	}
	// Passage 1 (failover where role==master PLUS re-probe with no where):
	// re-probe targets the whole roster, so BOTH hosts get a Passage-1
	// ApplyRequest (re-probe #2 goes to everyone; failover #1 — only to the old
	// master host-a).
	if p1 := disp.targets(1); len(p1) != 2 {
		t.Errorf("Passage 1 targets = %v, want both hosts (re-probe without where -> whole roster)", p1)
	}

	// ★ Passage 2 (act where role_after==master): ONLY host-b — the NEW master
	// per the FRESH Passage 1 re-probe. The old Passage 0 probe would give
	// host-a — if the test sees host-a, the program-order edge/re-probe
	// targeting is BROKEN.
	p2 := disp.targets(2)
	if len(p2) != 1 || p2[0] != "host-b.example.com" {
		t.Fatalf("* Passage 2 targets = %v, want [host-b.example.com] (NEW master from re-probe Passage 1) - re-probe retargeting broken: targeting followed the OLD probe Passage 0", p2)
	}

	// apply_runs: host-a — passage 0,1 (probe + failover-action + re-probe; both
	// failover and re-probe are Passage 1, one row per passage); host-b —
	// passage 0,1,2 (probe + re-probe + new-master-action). All success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	if len(got["host-a.example.com"]) != 2 {
		t.Errorf("host-a passages = %v, want [0 1] (probe + failover/re-probe, not targeted by Passage 2)", got["host-a.example.com"])
	}
	if len(got["host-b.example.com"]) != 3 {
		t.Errorf("host-b passages = %v, want [0 1 2] (probe + re-probe + new-master-action)", got["host-b.example.com"])
	}
}

// TestIntegration_StagedOldSoul_Rejected — ★ FORWARD-COMPAT GUARD (ADR-056 §S5).
// A staged scenario (probe→where, N>1 passages) sends N ApplyRequests to a host;
// each passage's barrier waits for a RunResult echoing that passage. A Soul
// without passage capability (old binary) would return passage=0 for every
// passage → the Passage 1 barrier would wait for a terminal that never comes →
// HANG in applying. The run.go gate MUST reject the run BEFORE dispatch if EVEN
// ONE target host didn't announce passage support. ASSERT: incarnation →
// ERROR_LOCKED, reason = soul_passage_unsupported, NO ApplyRequest at all
// (fail-closed rejection BEFORE dispatch, not a hang). Symmetric to
// StagedNilPassageCap_FailClosed (also fail-closed before dispatch).
func TestIntegration_StagedOldSoul_Rejected(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	// host-b is an "old" Soul without passage capability (doesn't echo passage).
	r := newRunnerWithPassageCap(t, disp, stubPassageCap{lacking: []string{"host-b.example.com"}})

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "soul_passage_unsupported" {
		t.Fatalf("reason = %v, want soul_passage_unsupported (old Soul under staged rejected BEFORE dispatch)", inc.StatusDetails["reason"])
	}

	// ★ Rejected BEFORE dispatch: NO ApplyRequest at all (not even the Passage 0
	// probe went out) — not a hang, not a silent single-pass execution.
	if p0 := disp.targets(0); len(p0) != 0 {
		t.Fatalf("* Passage 0 targets = %v, want [] (old Soul under staged rejected BEFORE any dispatch)", p0)
	}
}

// TestIntegration_StagedNilPassageCap_FailClosed — ★ FAIL-CLOSED without Redis
// (ADR-056 §S5). passageCap=nil (no presence source for capability) → a staged
// run does NOT guess support, it's rejected outright: sending N>1 blind carries
// the same hang risk. N=1 runs don't hit this gate (see other tests). ASSERT:
// ERROR_LOCKED, reason = soul_passage_unsupported, no ApplyRequest at all.
func TestIntegration_StagedNilPassageCap_FailClosed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	r := newRunnerWithPassageCap(t, disp, nil) // no Redis checker.

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "soul_passage_unsupported" {
		t.Fatalf("reason = %v, want soul_passage_unsupported (nil passageCap → fail-closed)", inc.StatusDetails["reason"])
	}
	if p0 := disp.targets(0); len(p0) != 0 {
		t.Fatalf("* Passage 0 targets = %v, want [] (nil passageCap -> reject BEFORE dispatch)", p0)
	}
}

// TestIntegration_RunOnceStaged — run_once+staged are COMPATIBLE (ADR-056 §S4):
// run_once trims a passage task's target to the first-by-SID host from THIS
// passage's RESOLVED target (fresh register). Scenario: probe role (p0) →
// run_once-act where role==master (p1). BOTH hosts are master → where passes
// both → run_once trims to the first by SID (host-a). ASSERT: the Passage-1
// ApplyRequest goes ONLY to host-a (first by SID from the master target), not
// both. Proves run_once applies to the target resolved by the Passage 0
// per-host register (not blindly to the first roster host).
func TestIntegration_RunOnceStaged(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: run_once+staged service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/runoncestaged/main.yml", `name: runoncestaged
description: probe role then run_once-act on master (staged + run_once)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Run-once act on master
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    run_once: true
    params:
      cmd: promote
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init run_once+staged service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	gitURL := "file://" + dir

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "master", // BOTH master — where passes both
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "runoncestaged",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want both hosts", p0)
	}
	// ★ Passage 1: ONLY host-a — run_once trimmed the master target
	// {host-a,host-b} to the first by SID. Proves run_once applied to the
	// RESOLVED Passage 1 where target (both master), not blindly to the first
	// roster host.
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("* Passage 1 targets = %v, want [host-a.example.com] (run_once -> first by SID from the master target)", p1)
	}
}

// TestIntegration_StagedInlineNotAcolyteClaim — ★ staged runs INLINE even under
// AcolyteEnabled (ADR-056 §S4 LIMITATION). The run.go:308 `!staged` gate
// excludes staged runs from the Acolyte path (dispatchPlanned): staged-render
// always runs inline, even when the instance is in work-queue mode. Proof: zero
// planned/claimed apply_runs rows (the Acolyte path writes planned to EVERY
// roster host BEFORE claim); all rows are terminal immediately (inline
// Insert(running)→SendApply→success), passage set correctly. Per-passage
// Acolyte claim is deferred (follow-up).
func TestIntegration_StagedInlineNotAcolyteClaim(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	// AcolyteEnabled=true: BUT a staged run must still go inline (gate !staged).
	r := newRunnerAcolyte(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// The inline path dispatches SendApply immediately (does NOT write planned
	// for Acolyte claim): Passage 0 went to both hosts, Passage 1 — to master.
	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want both hosts (inline SendApply, not Acolyte planned)", p0)
	}
	if p1 := disp.targets(1); len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Errorf("Passage 1 targets = %v, want [host-a.example.com] (inline)", p1)
	}

	// ★ Zero planned/claimed rows: staged did NOT take the Acolyte path
	// (dispatchPlanned would have written planned to EVERY roster host). All
	// rows are terminal success immediately (inline Insert(running)→success).
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	for _, st := range statuses {
		if st.Status == applyrun.StatusPlanned || st.Status == applyrun.StatusClaimed {
			t.Fatalf("* apply_runs[%s,passage=%d] = %s - staged WRONGLY went through the Acolyte path (planned/claimed), but must be inline", st.SID, st.Passage, st.Status)
		}
		if st.Status != applyrun.StatusSuccess {
			t.Errorf("apply_runs[%s,passage=%d] = %s, want success", st.SID, st.Passage, st.Status)
		}
	}
}

// TestIntegration_StagedEmptyRegister — comparison robustness: probe gives EMPTY
// stdout to one host and whitespace to the other → `where: ... == 'master'` is
// false on both → both excluded from Passage 1 (Passage-1 ApplyRequest to
// nobody) → incarnation READY (no-op, like all-slave). Proves an empty/whitespace
// register doesn't "leak" into the master target.
func TestIntegration_StagedEmptyRegister(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "",    // empty stdout
		"host-b.example.com": "   ", // whitespace
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want both hosts", p0)
	}
	// ★ Passage 1: NONE (empty/whitespace stdout != 'master').
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("* Passage 1 targets = %v, want [] (empty/whitespace register != 'master')", p1)
	}

	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		if len(got[sid]) != 1 || got[sid][0] != 0 {
			t.Errorf("%s passages = %v, want [0] (probe only)", sid, got[sid])
		}
	}
}

// multiTaskPassage0ServiceRepo — scenario where Passage 0 carries TWO tasks
// (probe `X` + another task with no register dependency), and Passage 1 reads
// register.X in where. Reproduces a latent task_idx-collision bug (ADR-056 §S1):
//
//	#0 probe X        → Passage 0, register: X, local task_idx 0
//	#1 noop step      → Passage 0, no register, local task_idx 1
//	#2 act where X    → Passage 1, local task_idx 0 (!) — collides with #0 on task_idx
//
// If register keyed on local task_idx, probe-X (Passage0/idx0) and the action
// (Passage1/idx0) would share a key. Correlating on the global plan_index keeps
// them apart (X — plan_index 0, action — plan_index 2).
func multiTaskPassage0ServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: multi-task passage-0 proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/failover/main.yml", `name: failover
description: probe X (p0) + noop (p0) → act where X (p1)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Noop step in passage 0
    module: core.exec.run
    changed_when: "false"
    params:
      cmd: noop
  - name: Act on master only
    module: core.exec.run
    register: action
    where: "register.role.stdout == 'master'"
    changed_when: "false"
    params:
      cmd: promote
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init multi-task passage-0 service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// multiTaskDispatcher simulates Soul for the multi-task-Passage-0 case: writes
// register `role` (Passage 0, local idx 0) AND register `action` (Passage 1,
// local idx 0). Under the OLD PK (apply_id, sid, task_idx) both rows would share
// a key (host-a, 0) → ON CONFLICT would clobber probe-role with action (the
// bug). Under the new PK (apply_id, sid, plan_index) they coexist (plan_index 0
// and 2). Indices come from req.Tasks[] (emitterIndices), as Soul echoes them in
// TaskEvent.
type multiTaskDispatcher struct {
	t         *testing.T
	roleBySID map[string]string

	mu                sync.Mutex
	targetedByPassage map[int][]string
}

func (d *multiTaskDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()

	d.mu.Lock()
	d.targetedByPassage[passage] = append(d.targetedByPassage[passage], sid)
	d.mu.Unlock()

	switch passage {
	case 0:
		localIdx, planIdx := emitterIndices(req, "role")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx, Passage: 0,
			RegisterData: map[string]any{"stdout": d.roleBySID[sid], "changed": false, "failed": false},
		}); err != nil {
			d.t.Errorf("multiTaskDispatcher: UpsertTaskRegister role (%s): %v", sid, err)
		}
	case 1:
		// action — GLOBAL plan_index 2, but LOCAL task_idx 0 (#2 is the first and
		// only step in the Passage 1 slice). task_idx collides with probe role
		// (also local 0 in Passage 0); plan_index does not.
		localIdx, planIdx := emitterIndices(req, "action")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx, Passage: 1,
			RegisterData: map[string]any{"stdout": "promoted", "changed": false, "failed": false},
		}); err != nil {
			d.t.Errorf("multiTaskDispatcher: UpsertTaskRegister action (%s): %v", sid, err)
		}
	}

	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("multiTaskDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *multiTaskDispatcher) targets(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.targetedByPassage[passage]...)
	return out
}

// TestIntegration_MultiTaskPassage0_RegisterNotClobbered — ★ GUARD (ADR-056 §S1
// fix Variant B), integration: Passage 0 carries TWO tasks (probe role + noop),
// Passage 1 carries an action with register `action`. probe role
// (Passage0/local idx 0) and the action (Passage1/local idx 0) collide on
// task_idx — under the old PK (apply_id, sid, task_idx) ON CONFLICT would
// clobber the probe register with the action; correlating on the global
// plan_index keeps them apart (0 and 2).
//
// ASSERT: (1) the probe register role is NOT clobbered (the plan_index 0 row
// carries the probe value, not 'promoted'); (2) Passage-1 targeted ONLY master
// (where resolved to the correct probe value). Proves the fix end-to-end
// through the stage loop with two tasks in Passage 0 and a register task in
// Passage 1.
func TestIntegration_MultiTaskPassage0_RegisterNotClobbered(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := multiTaskPassage0ServiceRepo(t)

	disp := &multiTaskDispatcher{
		t: t,
		roleBySID: map[string]string{
			"host-a.example.com": "master",
			"host-b.example.com": "slave",
		},
		targetedByPassage: map[int][]string{},
	}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// (1) ★ the probe register role is NOT clobbered by the Passage 1 action.
	// The global plan_index of probe role = 0 (first plan step); check that the
	// row with this plan_index carries the probe value, not the action's.
	regs, err := applyrun.SelectTaskRegistersByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectTaskRegistersByApplyID: %v", err)
	}
	var probeFound bool
	for _, reg := range regs {
		if reg.SID == "host-a.example.com" && reg.PlanIndex == 0 {
			probeFound = true
			if reg.RegisterData["stdout"] != "master" {
				t.Errorf("* probe-register (plan_index 0) host-a.stdout = %v, want master (overwritten by an action?)", reg.RegisterData["stdout"])
			}
		}
	}
	if !probeFound {
		t.Fatalf("* probe-register (plan_index 0) host-a missing - overwritten by a task_idx collision (bug)")
	}

	// (2) ★ Passage 1 — ONLY master (where resolved via the non-clobbered probe register).
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("* Passage 1 targets = %v, want [host-a.example.com] - register.role did not resolve (probe overwritten?)", p1)
	}
}

// perHostWhereServiceRepo — scenario where a per-host where inside Passage 0
// gives the register task a DIFFERENT local task_idx on different hosts:
//
//	#0 master-only step  → Passage 0, where: master host only
//	#1 probe role_after  → Passage 0, register: role_after (both hosts)
//	#2 act where after   → Passage 1, where: register.role_after
//
// On the master host the Passage 0 slice = [#0,#1] → probe role_after at local
// 1; on non-master the slice = [#1] (#0 filtered out by where) → probe
// role_after at local 0. probe's task_idx differs (1 vs 0), global plan_index
// is the same (1).
func perHostWhereServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: per-host where in same passage proof service
state_schema:
  type: object
  properties: {}
`)
	// Passage 0 carries #0 (host-A-only prep, where on the STABLE fact
	// soulprint.self.covens — coven 'box-a' only on host-a) + #1 (probe role,
	// both hosts). where #0 relies on coven membership (a registry fact, not a
	// register) → both are in Passage 0, but #0 is filtered per-host. host-a
	// slice = [#0,#1] (probe at local 1); host-b slice = [#1] (probe at local 0).
	// #2 (Passage 1) reads the probe's register role.
	write("scenario/failover/main.yml", `name: failover
description: box-a-only prep (p0) + probe role (p0, both) → act where role (p1)
state_changes: {}
tasks:
  - name: Box-A-only prep
    module: core.exec.run
    where: "'box-a' in soulprint.self.covens"
    changed_when: "false"
    params:
      cmd: prep
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Act on master only
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    params:
      cmd: promote
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init per-host where service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_PerHostDifferentWhere_RegisterResolves — ★ GUARD (ADR-056 §S1
// fix Variant B), integration: a per-host where inside Passage 0 gives the probe
// task `role` a DIFFERENT local task_idx on the two hosts (host-a slice [#0,#1]
// → idx 1; host-b slice [#1] → idx 0), but the global plan_index is the same
// (1). Correlating on plan_index resolves the register correctly on both →
// Passage 1 (`where: register.role == 'master'`) targets correctly.
//
// host-a is master (+coven box-a → passes per-host where #0), host-b is slave.
// ASSERT: Passage 1 → ONLY host-a. If correlation went by task_idx (host-a
// idx 1, host-b idx 0), one host's register would fail to resolve to `role` →
// wrong targeting.
func TestIntegration_PerHostDifferentWhere_RegisterResolves(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	// coven box-a is only on host-a → per-host where #0 ('box-a' in covens)
	// passes only on host-a, giving the probe role a different local task_idx on
	// the two hosts.
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod", "box-a"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := perHostWhereServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// register role on both hosts carries plan_index 1 (global), even though the
	// local task_idx differs (host-a 1, host-b 0). Both resolve to name `role`.
	regs, err := applyrun.SelectTaskRegistersByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectTaskRegistersByApplyID: %v", err)
	}
	taskIdxBySID := map[string]int{}
	for _, reg := range regs {
		if reg.PlanIndex == 1 {
			taskIdxBySID[reg.SID] = reg.TaskIdx
		}
	}
	// host-a probe role at local 1 (slice [#0,#1]); host-b at local 0 (#0
	// filtered out by per-host where). The differing task_idx is the point.
	if taskIdxBySID["host-a.example.com"] != 1 {
		t.Errorf("host-a probe role task_idx = %d, want 1 (slice [#0,#1])", taskIdxBySID["host-a.example.com"])
	}
	if taskIdxBySID["host-b.example.com"] != 0 {
		t.Errorf("host-b probe role task_idx = %d, want 0 (slice [#1], #0 filtered out)", taskIdxBySID["host-b.example.com"])
	}

	// ★ Passage 1 → ONLY master, despite the probe task's differing per-host
	// local task_idx: register resolves via the global plan_index.
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("* Passage 1 targets = %v, want [host-a.example.com] - register.role did not resolve with different per-host task_idx", p1)
	}
}
