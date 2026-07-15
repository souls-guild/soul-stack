package scenario

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// changedKeys — sugar for building a set of (sid, idx) CHANGED-task keys.
func changedKeys(pairs ...auditpg.ChangedTaskKey) map[auditpg.ChangedTaskKey]struct{} {
	out := make(map[auditpg.ChangedTaskKey]struct{}, len(pairs))
	for _, p := range pairs {
		out[p] = struct{}{}
	}
	return out
}

// findByAddr looks up a ChangedTask by register/id (order-independent).
func findByAddr(tasks []ChangedTask, register, id string) (ChangedTask, bool) {
	for _, t := range tasks {
		if t.Register == register && t.ID == id {
			return t, true
		}
	}
	return ChangedTask{}, false
}

// TestBuildChangedTasks_ChangedHostsByUniqueSID — basic invariant: N hosts
// report CHANGED at an address → ChangedHosts=N (unique sids). Target 3 hosts,
// CHANGED on 2 → ChangedHosts=2, TotalHosts=3.
func TestBuildChangedTasks_ChangedHostsByUniqueSID(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "install", Register: "pkg", Module: "core.pkg.installed"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"a.local", "b.local", "c.local"}},
	}
	keys := changedKeys(
		auditpg.ChangedTaskKey{SID: "a.local", PlanIndex: 0},
		auditpg.ChangedTaskKey{SID: "b.local", PlanIndex: 0},
	)

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("got %d changed tasks, want 1: %+v", len(got), got)
	}
	if got[0].ChangedHosts != 2 {
		t.Errorf("ChangedHosts = %d, want 2 (unique CHANGED sids)", got[0].ChangedHosts)
	}
	if got[0].TotalHosts != 3 {
		t.Errorf("TotalHosts = %d, want 3 (TargetSIDs)", got[0].TotalHosts)
	}
	if got[0].Register != "pkg" {
		t.Errorf("Register = %q, want pkg", got[0].Register)
	}
}

// TestBuildChangedTasks_LoopNoDoubleCount — KEY test (loop double-count):
// one source loop task expands into K=3 RenderedTask (idx 0,1,2) sharing ONE
// address (register="pkg"), targeting M=2 hosts per idx. CHANGED reported on
// both hosts across several idx. Invariant:
//   - TotalHosts = M = 2 (union of unique sids), NOT M×K = 6;
//   - ChangedHosts by unique sid (NOT summed over idx);
//   - one ChangedTask per address (loop fold), idx = first (representative).
func TestBuildChangedTasks_LoopNoDoubleCount(t *testing.T) {
	// 3-iteration loop: one Register shared, idx runs 0,1,2.
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "install_each", Register: "pkg", Module: "core.pkg.installed"},
		{Index: 1, Name: "install_each", Register: "pkg", Module: "core.pkg.installed"},
		{Index: 2, Name: "install_each", Register: "pkg", Module: "core.pkg.installed"},
	}
	// Each iteration targets BOTH hosts (M=2).
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"a.local", "b.local"}},
		{TaskIndex: 1, TargetSIDs: []string{"a.local", "b.local"}},
		{TaskIndex: 2, TargetSIDs: []string{"a.local", "b.local"}},
	}
	// CHANGED: a.local at idx 0,2; b.local at idx 1. Union of sids = {a,b} = 2.
	keys := changedKeys(
		auditpg.ChangedTaskKey{SID: "a.local", PlanIndex: 0},
		auditpg.ChangedTaskKey{SID: "a.local", PlanIndex: 2},
		auditpg.ChangedTaskKey{SID: "b.local", PlanIndex: 1},
	)

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("loop must fold to ONE ChangedTask per address, got %d: %+v", len(got), got)
	}
	if got[0].TotalHosts != 2 {
		t.Errorf("TotalHosts = %d, want 2 (union M, NOT M×K=6)", got[0].TotalHosts)
	}
	if got[0].ChangedHosts != 2 {
		t.Errorf("ChangedHosts = %d, want 2 (union {a,b}, NOT sum-over-idx=3)", got[0].ChangedHosts)
	}
	if got[0].Idx != 0 {
		t.Errorf("Idx = %d, want 0 (first representative iteration)", got[0].Idx)
	}
}

// TestBuildChangedTasks_StagedPlanIndexCorrelation — T3 GUARD (fold side):
// under staged/per-host-where, the GLOBAL RenderedTask.Index (=
// ChangedTaskKey.PlanIndex) ≠ the LOCAL task_idx. buildChangedTasks correlates
// CHANGED by the global Index — a changed-key built from global plan_index
// (as SelectChangedTaskKeys now does) MUST match; a key built from local
// task_idx must NOT.
//
// Model: task with global Index=5 (second Passage, local position would be 2),
// target h.local. Correct CHANGED-key — {h.local, PlanIndex:5}.
func TestBuildChangedTasks_StagedPlanIndexCorrelation(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 5, Name: "restart_staged", Register: "rst", Module: "core.service.running"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 5, TargetSIDs: []string{"h.local"}},
	}
	// Global plan_index=5 (what SelectChangedTaskKeys returns post-T3).
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "h.local", PlanIndex: 5})

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (changed-задача под staged должна корелировать по глобальному plan_index)", len(got))
	}
	if got[0].ChangedHosts != 1 || got[0].Register != "rst" {
		t.Errorf("ChangedHosts=%d Register=%q, want 1/rst", got[0].ChangedHosts, got[0].Register)
	}
}

// TestBuildChangedTasks_StagedLocalIdxMiscorrelates — REVERSE invariant of the
// T3 GUARD: if the fold keyed by LOCAL task_idx (=2), the changed-key
// {h.local,2} would NOT match the task's global Index=5 → the task would
// silently drop out of changed_tasks (mismatching state_changes whitelist +
// audit). This test asserts that such a key indeed does NOT match — a
// regression to local indexing would be caught.
func TestBuildChangedTasks_StagedLocalIdxMiscorrelates(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 5, Name: "restart_staged", Register: "rst", Module: "core.service.running"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 5, TargetSIDs: []string{"h.local"}},
	}
	// Wrong (local) key: task_idx=2 ≠ global Index=5.
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "h.local", PlanIndex: 2})

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 0 {
		t.Fatalf("got %d, want 0 — ключ по ЛОКАЛЬНОМУ task_idx (2) не должен матчить глобальный Index (5): %+v", len(got), got)
	}
}

// TestBuildChangedTasks_NoChangesOmitted — a task with no CHANGED host is
// omitted from the result.
func TestBuildChangedTasks_NoChangesOmitted(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "changed_one", Register: "a", Module: "core.file.present"},
		{Index: 1, Name: "untouched", Register: "b", Module: "core.file.present"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"h.local"}},
		{TaskIndex: 1, TargetSIDs: []string{"h.local"}},
	}
	// CHANGED only at idx 0.
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "h.local", PlanIndex: 0})

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (only changed task)", len(got))
	}
	if got[0].Register != "a" {
		t.Errorf("included Register = %q, want a (untouched 'b' must be absent)", got[0].Register)
	}
	if _, ok := findByAddr(got, "b", ""); ok {
		t.Error("task 'b' without changes leaked into result")
	}
}

// TestBuildChangedTasks_TotalFromDispatchPlan — TotalHosts comes from
// DispatchPlan.TargetSIDs (post on:/where:), NOT the full roster. where:
// filtered out some hosts → denominator = target only.
func TestBuildChangedTasks_TotalFromDispatchPlan(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "on_replicas", Register: "r", Module: "core.service.running"},
	}
	// run roster could be {a,b,c,d}, but where: kept {a,b} — plan carries 2.
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"a.local", "b.local"}},
	}
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "a.local", PlanIndex: 0})

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].TotalHosts != 2 {
		t.Errorf("TotalHosts = %d, want 2 (DispatchPlan.TargetSIDs after where:, not full roster)", got[0].TotalHosts)
	}
	if got[0].ChangedHosts != 1 {
		t.Errorf("ChangedHosts = %d, want 1", got[0].ChangedHosts)
	}
}

// TestBuildChangedTasks_AddressIDFallback — a task without register but with
// id: addressed by id (register∪id). ID lands in ChangedTask, Register empty.
func TestBuildChangedTasks_AddressIDFallback(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "restart", ID: "restart-web", Module: "core.service.running"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"h.local"}},
	}
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "h.local", PlanIndex: 0})

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].ID != "restart-web" || got[0].Register != "" {
		t.Errorf("address fallback: ID=%q Register=%q, want id=restart-web register empty", got[0].ID, got[0].Register)
	}
}

// TestBuildChangedTasks_UnaddressableIncluded — an unaddressable changed task
// (no register, no id) is INCLUDED with an empty address (completeness of
// "what changed"). T3 decision: include all changed tasks.
func TestBuildChangedTasks_UnaddressableIncluded(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "anon", Module: "core.exec.run"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"h.local"}},
	}
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "h.local", PlanIndex: 0})

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (unaddressable changed task included)", len(got))
	}
	if got[0].Register != "" || got[0].ID != "" {
		t.Errorf("unaddressable task must have empty address, got register=%q id=%q", got[0].Register, got[0].ID)
	}
	if got[0].Name != "anon" || got[0].Module != "core.exec.run" {
		t.Errorf("metadata = name=%q module=%q, want anon/core.exec.run", got[0].Name, got[0].Module)
	}
}

// TestBuildChangedTasks_UnaddressableNotFolded — two DIFFERENT unaddressable
// tasks (idx 0 and 1, neither has register/id) do NOT fold into one entry:
// each groups by its own Index. Guards against false-folding distinct tasks
// that share an empty address.
func TestBuildChangedTasks_UnaddressableNotFolded(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "step_a", Module: "core.exec.run"},
		{Index: 1, Name: "step_b", Module: "core.exec.run"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"h.local"}},
		{TaskIndex: 1, TargetSIDs: []string{"h.local"}},
	}
	keys := changedKeys(
		auditpg.ChangedTaskKey{SID: "h.local", PlanIndex: 0},
		auditpg.ChangedTaskKey{SID: "h.local", PlanIndex: 1},
	)

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (distinct unaddressable tasks must not fold)", len(got))
	}
	names := []string{got[0].Name, got[1].Name}
	sort.Strings(names)
	if names[0] != "step_a" || names[1] != "step_b" {
		t.Errorf("names = %v, want [step_a step_b]", names)
	}
}

// TestBuildChangedTasks_SecretHygiene — the changed_tasks payload carries ONLY
// metadata + counts; no register/params VALUES. Verifies changedTasksPayload
// shape: keys strictly {idx,name,register,id,module,changed_hosts,
// total_hosts}, no fields like register_data/params/value.
func TestBuildChangedTasks_SecretHygiene(t *testing.T) {
	changed := []ChangedTask{
		{Idx: 0, Name: "install", Register: "pkg", ID: "", Module: "core.pkg.installed", ChangedHosts: 1, TotalHosts: 2},
	}
	payload := changedTasksPayload(changed)
	if len(payload) != 1 {
		t.Fatalf("payload len = %d, want 1", len(payload))
	}
	allowed := map[string]struct{}{
		"idx": {}, "name": {}, "register": {}, "id": {}, "module": {},
		"changed_hosts": {}, "total_hosts": {},
	}
	for k := range payload[0] {
		if _, ok := allowed[k]; !ok {
			t.Errorf("changed_tasks payload leaked disallowed key %q (secret hygiene)", k)
		}
	}
	// Counts and metadata present.
	if payload[0]["changed_hosts"] != 1 || payload[0]["total_hosts"] != 2 {
		t.Errorf("counts = %v/%v, want 1/2", payload[0]["changed_hosts"], payload[0]["total_hosts"])
	}
	if payload[0]["register"] != "pkg" {
		t.Errorf("register = %v, want pkg", payload[0]["register"])
	}
}

// TestBuildChangedTasks_EmptyInputs — no tasks → nil; tasks exist but none
// CHANGED → empty (not nil) result isn't required at this level,
// buildChangedTasks returns a nil slice (changedTasksPayload yields []).
func TestBuildChangedTasks_EmptyInputs(t *testing.T) {
	if got := buildChangedTasks(nil, nil, nil); got != nil {
		t.Errorf("buildChangedTasks(nil...) = %+v, want nil", got)
	}
	// changedTasksPayload(nil) → empty slice (not nil): payload carries [].
	if p := changedTasksPayload(nil); p == nil {
		t.Error("changedTasksPayload(nil) = nil, want empty slice for JSON []")
	} else if len(p) != 0 {
		t.Errorf("changedTasksPayload(nil) len = %d, want 0", len(p))
	}
}
