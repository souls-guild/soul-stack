package scenario

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// keySet — a set of (sid, planIndex) for a CHANGED/FAILED fact (auditpg form).
func keySet(pairs ...auditpg.ChangedTaskKey) map[auditpg.ChangedTaskKey]struct{} {
	out := make(map[auditpg.ChangedTaskKey]struct{}, len(pairs))
	for _, p := range pairs {
		out[p] = struct{}{}
	}
	return out
}

func key(sid string, idx int) auditpg.ChangedTaskKey {
	return auditpg.ChangedTaskKey{SID: sid, PlanIndex: idx}
}

// fullPlan builds passageByIndex for (Index→Passage). Pairs: index, passage, …
func fullPlan(pairs ...int) []*render.RenderedTask {
	out := make([]*render.RenderedTask, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, &render.RenderedTask{Index: pairs[i], Passage: pairs[i+1]})
	}
	return out
}

// taskNames — sorted task names of a host slice (for include/exclude assertions).
func taskNames(ts []*render.RenderedTask) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// findTask — a slice's task by name.
func findTask(ts []*render.RenderedTask, name string) *render.RenderedTask {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// TestCrossPassageGate_OnChangesFires — cross-passage onchanges: source (idx 0,
// Passage 0) CHANGED on the host → consumer (idx 1, Passage 1) runs,
// cross-passage idx removed from wire (OnChangesIdx empty → Soul unconditional).
func TestCrossPassageGate_OnChangesFires(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan,
		keySet(key("host-a", 0)), // source 0 changed on host-a
		nil)

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	perHost := map[string][]*render.RenderedTask{"host-a": {consumer}}

	got := g.applyGate(perHost, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 || tasks[0].Name != "restart" {
		t.Fatalf("host-a slice = %v, want [restart] (onchanges-source changed -> executes)", taskNames(tasks))
	}
	if len(tasks[0].OnChangesIdx) != 0 {
		t.Errorf("OnChangesIdx = %v, want [] (cross-passage idx dropped -> unconditional)", tasks[0].OnChangesIdx)
	}
	// Clone, not a mutation of the original consumer.
	if len(consumer.OnChangesIdx) != 1 {
		t.Errorf("original consumer.OnChangesIdx mutated = %v, want [0]", consumer.OnChangesIdx)
	}
}

// TestCrossPassageGate_OnChangesSkips — cross-passage onchanges: source is NOT
// changed (ok/skipped) → consumer is EXCLUDED (no same-passage source). A host
// with no other tasks drops out of the slice.
func TestCrossPassageGate_OnChangesSkips(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan, keySet(), nil) // nothing changed

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	perHost := map[string][]*render.RenderedTask{"host-a": {consumer}}

	got := g.applyGate(perHost, 1)
	if _, present := got["host-a"]; present {
		t.Fatalf("host-a stayed in slice = %v, want dropped (onchanges did not fire, no same-passage)", taskNames(got["host-a"]))
	}
}

// TestCrossPassageGate_OnFailFires — cross-passage onfail (rescue): source
// FAILED on the host → rescue runs, cross-passage idx removed from wire.
func TestCrossPassageGate_OnFailFires(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan,
		nil,
		keySet(key("host-a", 0))) // source 0 failed on host-a

	rescue := &render.RenderedTask{Index: 1, Passage: 1, Name: "rescue", OnFailIdx: []int{0}}
	perHost := map[string][]*render.RenderedTask{"host-a": {rescue}}

	got := g.applyGate(perHost, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 || tasks[0].Name != "rescue" {
		t.Fatalf("host-a slice = %v, want [rescue] (onfail-source failed -> rescue executes)", taskNames(tasks))
	}
	if len(tasks[0].OnFailIdx) != 0 {
		t.Errorf("OnFailIdx = %v, want [] (cross-passage idx dropped -> unconditional)", tasks[0].OnFailIdx)
	}
}

// TestCrossPassageGate_OnFailSkips — cross-passage onfail: source is NOT
// failed → rescue is excluded.
func TestCrossPassageGate_OnFailSkips(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan, nil, keySet()) // nothing failed

	rescue := &render.RenderedTask{Index: 1, Passage: 1, Name: "rescue", OnFailIdx: []int{0}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {rescue}}, 1)
	if _, present := got["host-a"]; present {
		t.Fatalf("host-a stayed = %v, want dropped (onfail did not fire)", taskNames(got["host-a"]))
	}
}

// TestCrossPassageGate_PerHostDivergent — source CHANGED on host-a, NOT on
// host-b → consumer runs ONLY on host-a. Per-host resolution.
func TestCrossPassageGate_PerHostDivergent(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan, keySet(key("host-a", 0)), nil) // changed only on host-a

	mk := func() *render.RenderedTask {
		return &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	}
	perHost := map[string][]*render.RenderedTask{"host-a": {mk()}, "host-b": {mk()}}

	got := g.applyGate(perHost, 1)
	if len(got["host-a"]) != 1 {
		t.Errorf("host-a = %v, want [restart] (source changed)", taskNames(got["host-a"]))
	}
	if _, present := got["host-b"]; present {
		t.Errorf("host-b = %v, want dropped (source NOT changed on host-b)", taskNames(got["host-b"]))
	}
}

// TestCrossPassageGate_SkippedSourceNotChanged — source was SKIPPED (not in the
// CHANGED set) → consumer onchanges:[A] SKIPPED. CHANGED-set semantics: skipped
// ≠ changed, even if the source's register row exists (register is written for
// ok/skipped too). Here the source is ABSENT from the CHANGED set → didn't fire.
func TestCrossPassageGate_SkippedSourceNotChanged(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	// CHANGED set is empty: source 0 finished SKIPPED/OK, a register row may
	// have been written, but it's not in the CHANGED set.
	g := newCrossPassageGate(plan, keySet(), nil)

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {consumer}}, 1)
	if _, present := got["host-a"]; present {
		t.Fatalf("host-a stayed = %v, want dropped (skipped-source != changed)", taskNames(got["host-a"]))
	}
}

// TestCrossPassageGate_MixedCrossAndSame_CrossNotChanged — OR semantics for
// cross+same. Consumer (Passage 1) has onchanges:[A(cross, idx0, Passage0),
// B(same, idx2, Passage1)]. A is NOT changed, but B is same-passage (Soul
// gates on it itself): consumer STAYS on the host, cross idx A removed,
// same idx B stays on wire.
func TestCrossPassageGate_MixedCrossAndSame_CrossNotChanged(t *testing.T) {
	// idx0 Passage0 (A, cross); idx2 Passage1 (B, same); idx1 Passage1 (consumer).
	plan := fullPlan(0, 0, 1, 1, 2, 1)
	g := newCrossPassageGate(plan, keySet(), nil) // A is NOT changed

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0, 2}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {consumer}}, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 {
		t.Fatalf("host-a = %v, want [restart] (has same-passage onchanges B -> not excluded, Soul gates)", taskNames(tasks))
	}
	// cross idx 0 (A) removed; same idx 2 (B) stays → Soul gates by B.
	if len(tasks[0].OnChangesIdx) != 1 || tasks[0].OnChangesIdx[0] != 2 {
		t.Errorf("OnChangesIdx = %v, want [2] (cross A dropped, same B on wire -> Soul gates by B)", tasks[0].OnChangesIdx)
	}
}

// TestCrossPassageGate_MixedCrossAndSame_CrossChanged — A(cross) CHANGED → OR
// requisite SATISFIED keeper-side → consumer runs UNCONDITIONALLY, the WHOLE
// onchanges is stripped from the wire (both cross A and same B). same B can't
// be left in: Soul would re-gate on it and, if B-not-changed, would FALSELY
// skip the consumer even though cross A already satisfied it (OR). ASSERT:
// stays, OnChangesIdx empty.
func TestCrossPassageGate_MixedCrossAndSame_CrossChanged(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1, 2, 1)
	g := newCrossPassageGate(plan, keySet(key("host-a", 0)), nil) // A changed

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0, 2}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {consumer}}, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 {
		t.Fatalf("host-a = %v, want [restart] (A changed -> executes)", taskNames(tasks))
	}
	// The WHOLE onchanges is stripped (OR satisfied by cross A): Soul runs
	// unconditionally, doesn't re-gate on same B.
	if len(tasks[0].OnChangesIdx) != 0 {
		t.Errorf("OnChangesIdx = %v, want [] (OR satisfied by cross A -> entire requisite dropped, unconditional)", tasks[0].OnChangesIdx)
	}
}

// TestCrossPassageGate_NoCrossPassage_Untouched — a task with no cross-passage
// requisite (all sources same-passage) is returned by the SAME pointer (keeper
// doesn't touch it, R1-remap on Soul).
func TestCrossPassageGate_NoCrossPassage_Untouched(t *testing.T) {
	plan := fullPlan(0, 1, 1, 1) // both in Passage 1 (same)
	g := newCrossPassageGate(plan, keySet(), nil)

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {consumer}}, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 || tasks[0] != consumer {
		t.Fatalf("same-passage requisite: task must be returned as the same pointer without a clone")
	}
}

// TestCrossPassageGate_Nil — nil gate (N=1 / Passage 0) → perHost as-is.
func TestCrossPassageGate_Nil(t *testing.T) {
	var g *crossPassageGate
	consumer := &render.RenderedTask{Index: 0, Passage: 0, Name: "t"}
	in := map[string][]*render.RenderedTask{"host-a": {consumer}}
	got := g.applyGate(in, 0)
	if len(got["host-a"]) != 1 || got["host-a"][0] != consumer {
		t.Fatalf("nil-gate must return perHost as-is")
	}
}
