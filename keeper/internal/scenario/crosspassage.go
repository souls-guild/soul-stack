package scenario

// Cross-passage requisite gating ([ADR-056](../../../docs/adr/0056-staged-render-passage.md),
// R3 — final slice of the global-vs-local class). Keeper-side resolution of
// onchanges/onfail links whose source lives in an EARLIER Passage than the consumer.
//
// Why keeper-side. requisites (`onchanges:`/`onfail:`) are NOT passage-defining
// (excluded from the Stratify graph, passage.go). If a consumer moved to
// Passage>0 via a different register dependency (`where: register.X` from a
// probe), while its requisite's source stayed in Passage 0, they travel in
// DIFFERENT ApplyRequests. Soul's gating for ONE Passage only sees its own
// registerByIdx — another Passage's source result is unavailable to it
// (ToProtoTasks encodes it with sentinel -1 = "doesn't satisfy"). So
// cross-passage requisite links must be resolved by Keeper from accumulated
// per-host CHANGED/FAILED facts (R1+R2 only handled this within a single
// Passage / rejected cross-passage).
//
// CHANGED-set semantics (★). An onchanges source counts as "satisfied" ONLY on
// CHANGED. A skipped source (filtered by its own where: / its own requisite
// didn't fire) is NOT changed — onchanges through it does NOT fire. We rely on
// the auditpg.SelectChangedTaskKeys set (status CHANGED), NOT on "a register
// row exists" (register is written for ok/skipped probes too). onfail mirrors
// this over FAILED∪TIMED_OUT.
//
// Per-host. CHANGED/FAILED is a fact of a SPECIFIC host (source changed on
// host-a, ok on host-b). So the "consumer runs / excluded" decision and the
// wire-requisite rewrite are made per-(sid).

import (
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// crossPassageGate holds everything needed for per-host resolution of
// cross-passage requisites for ONE Passage before its dispatch. Pure data (no
// PG/ctx): run.go's stage loop loads previous Passages' CHANGED/FAILED facts
// once and threads them in here. nil gate (N=1 run / Passage 0) → applyGate
// is a no-op.
type crossPassageGate struct {
	// passageByIndex — global RenderedTask.Index → its Passage across the
	// WHOLE run plan (including sources not in the current Passage slice).
	// Needed to tell a cross-passage source (its Passage < consumer's Passage)
	// apart from same-passage (R1-remap fixes it on Soul, keeper leaves it
	// alone).
	passageByIndex map[int]int

	// changed / failed — per-(sid, planIndex) CHANGED / (FAILED∪TIMED_OUT) facts,
	// accumulated over Passages < current (auditpg). planIndex = global Index.
	changed map[auditpg.ChangedTaskKey]struct{}
	failed  map[auditpg.ChangedTaskKey]struct{}
}

// newCrossPassageGate assembles the gate for dispatching Passage p. tasks is
// the WHOLE resolved plan (all Passages; needed for sources' passageByIndex).
// changed/failed are CHANGED/FAILED facts from Passages < p.
func newCrossPassageGate(tasks []*render.RenderedTask, changed, failed map[auditpg.ChangedTaskKey]struct{}) *crossPassageGate {
	idx := make(map[int]int, len(tasks))
	for _, t := range tasks {
		idx[t.Index] = t.Passage
	}
	return &crossPassageGate{passageByIndex: idx, changed: changed, failed: failed}
}

// applyGate rewrites a Passage's per-host slice by cross-passage requisites:
//
//   - onchanges: ANY cross-passage source CHANGED on the host → requisite
//     SATISFIED (OR semantics) → cross-passage source idx are removed from
//     the wire (consumer runs; same-passage onchanges, if any, remain → Soul
//     gates on them). If NO cross-passage source changed AND there's no
//     same-passage onchanges source → consumer is EXCLUDED from the host
//     slice (onchanges didn't fire — doesn't run). If cross didn't fire but
//     there's same-passage onchanges — consumer stays, cross idx removed,
//     Soul gates by same-passage (R1-remap).
//   - onfail: mirrors this over FAILED∪TIMED_OUT (rescue).
//
// nil gate (N=1 / Passage 0) → returns perHost as-is (zero-cost). Tasks
// without cross-passage requisites are NOT cloned (shared pointer reused).
func (g *crossPassageGate) applyGate(perHost map[string][]*render.RenderedTask, consumerPassage int) map[string][]*render.RenderedTask {
	if g == nil {
		return perHost
	}
	out := make(map[string][]*render.RenderedTask, len(perHost))
	for sid, tasks := range perHost {
		kept := make([]*render.RenderedTask, 0, len(tasks))
		for _, t := range tasks {
			task, include := g.resolveTask(t, sid, consumerPassage)
			if include {
				kept = append(kept, task)
			}
		}
		// A host where ALL of the Passage's tasks are excluded by the
		// cross-passage gate (e.g. the only consumer, whose onchanges didn't
		// fire) drops out of the slice entirely — no apply_runs row/ApplyRequest
		// is created for it (like where: filtering out all of a host's tasks).
		// The barrier doesn't wait on it.
		if len(kept) > 0 {
			out[sid] = kept
		}
	}
	return out
}

// resolveTask decides the fate of one consumer task on one host. Returns
// (possibly cloned, with rewritten requisite idx) the task and whether to
// include it in the host slice. A task without cross-passage requisites is
// returned AS-IS (include=true, no clone) — keeper doesn't touch it.
func (g *crossPassageGate) resolveTask(t *render.RenderedTask, sid string, consumerPassage int) (*render.RenderedTask, bool) {
	onchangesCross, onchangesSame := g.splitRequisite(t.OnChangesIdx, consumerPassage)
	onfailCross, onfailSame := g.splitRequisite(t.OnFailIdx, consumerPassage)

	// No cross-passage requisites at all → keeper doesn't touch it (R1-remap on Soul).
	if len(onchangesCross) == 0 && len(onfailCross) == 0 {
		return t, true
	}

	// Per requisite kind we resolve the OR of the cross part and decide which
	// idx stay on the wire. nextOnchanges/nextOnfail are post-resolve wire idx;
	// include says whether to keep the consumer.
	nextOnchanges, includeOnchanges := g.resolveKind(g.changed, sid, onchangesCross, onchangesSame)
	nextOnfail, includeOnfail := g.resolveKind(g.failed, sid, onfailCross, onfailSame)

	// Requisite kinds combine via AND (like Soul: multiple requisites must all
	// be satisfied together). If the onchanges kind excludes the consumer OR
	// the onfail kind excludes it — the task doesn't run on this host.
	if !includeOnchanges || !includeOnfail {
		return nil, false
	}

	// Consumer runs. Clone it (*RenderedTask is shared across hosts — a
	// per-host decision can't mutate it) and set the rewritten wire idx.
	clone := *t
	clone.OnChangesIdx = nextOnchanges
	clone.OnFailIdx = nextOnfail
	return &clone, true
}

// resolveKind resolves ONE requisite kind (onchanges over the changed set /
// onfail over the failed set) for one host. cross is source idx in an earlier
// Passage, same is in the same one (Soul gates it itself). Returns post-resolve
// wire idx and the include flag:
//
//   - cross empty → keeper leaves this kind alone, same stays as-is (include=true).
//   - ANY cross source is in the set (changed/failed) → OR SATISFIED keeper-side →
//     the WHOLE requisite is stripped from the wire (cross+same), consumer runs
//     unconditionally for this kind (same can't be left in: Soul would re-gate on
//     it and could falsely skip, even though cross already satisfied it).
//   - NO cross is in the set, but same exists → strip cross idx, keep same →
//     Soul gates on the same-passage part (R1-remap).
//   - NO cross is in the set AND no same → requisite not satisfied → consumer
//     excluded (include=false).
func (g *crossPassageGate) resolveKind(set map[auditpg.ChangedTaskKey]struct{}, sid string, cross, same []int) (wire []int, include bool) {
	if len(cross) == 0 {
		return same, true
	}
	if g.anyKey(set, sid, cross) {
		return nil, true // OR satisfied by the cross part → unconditional (strip the whole requisite)
	}
	if len(same) > 0 {
		return same, true // cross didn't satisfy it, but same exists → Soul gates by same
	}
	return nil, false // cross didn't satisfy it and no same → doesn't run
}

// splitRequisite splits requisite source idx into cross-passage (source in an
// earlier Passage than consumerPassage) and same-passage (R1-remap fixes it
// itself). An idx with an unknown source (not in passageByIndex) is treated as
// same-passage — the cross-ref validator/Stratify would already have caught it
// offline; here it's a safe no-op (keeper doesn't invent cross-passage from a
// dangling idx).
func (g *crossPassageGate) splitRequisite(idxs []int, consumerPassage int) (cross, same []int) {
	for _, srcIdx := range idxs {
		if p, ok := g.passageByIndex[srcIdx]; ok && p < consumerPassage {
			cross = append(cross, srcIdx)
		} else {
			same = append(same, srcIdx)
		}
	}
	return cross, same
}

// anyKey — OR across cross-passage sources: is at least one (sid, srcIdx) in
// the facts set (changed / failed). srcIdx is the global plan_index (=
// auditpg key).
func (g *crossPassageGate) anyKey(set map[auditpg.ChangedTaskKey]struct{}, sid string, srcIdxs []int) bool {
	for _, srcIdx := range srcIdxs {
		if _, ok := set[auditpg.ChangedTaskKey{SID: sid, PlanIndex: srcIdx}]; ok {
			return true
		}
	}
	return false
}
