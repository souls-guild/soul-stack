package render

import (
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ToProtoTasks converts a render plan ([]*RenderedTask) into the
// ApplyRequest.tasks wire form ([]*keeperv1.RenderedTask). Index (the global
// index of a task across the whole run plan, including all Passages) goes
// into the proto field plan_index — the stable register-correlation key on
// Keeper (Soul echoes it back in TaskEvent.plan_index, ADR-056 §S1 fix Variant
// B). A task's position in ApplyRequest.tasks[] (= TaskEvent.task_idx) is
// LOCAL to passage/host — register resolution can NOT rely on it (Soul builds
// registerByName for flow-control predicates, ADR-012(d)).
//
// When/ChangedWhen/FailedWhen — flow-control CEL strings, passed through as-is
// (Soul evaluates them, ADR-012(d): when applies to this slice;
// changed_when/failed_when to the next). FlowContext is a per-host snapshot of
// the non-register CEL context. Until/RetryCount/RetryDelay are the DSL retry
// core (destiny/tasks.md §9), the retry loop is enforced Soul-side
// (applyrunner.runTaskWithRetry); until is a CEL string as-is.
//
// The single render→proto converter: both the scenario-orchestrator
// (dispatch) and trial-L2 (l2_run) call it, so adding a RenderedTask field
// can't drift into two copies of the wire form.
//
// ★ onchanges/onfail indexes are REMAPPED global→local (ADR-056 amend). On
// Keeper, resolveOnChanges/resolveOnFail resolve requisite register names into
// the GLOBAL RenderedTask.Index (spanning the whole run plan). But Soul keys
// registerByIdx by a task's LOCAL position in ApplyRequest.tasks[]
// (applyrunner.go), and that slice is per-host (groupByHost: only tasks that
// passed where: on the host) and/or a per-Passage subset of the plan. So a
// source's global Index doesn't match its local position in the slice, and
// the registerByIdx[onchanges_idx] lookup would miss → an onchanges task
// silently SKIPPED, an onfail-rescue silently NOT triggered. remapRequisites
// translates each global Index into a local position against the INPUT slice
// `tasks` (position = local index, exactly like Soul). N=1 without where
// (Index==localPos for all) → remap=identity, bit-for-bit behavior.
//
// A thin wrapper over [ToProtoTasksForHost] with an empty sid (golden path:
// params pass through as-is, render_context of the first-by-SID host).
//
// ★ PRECONDITION since NIM-908: the plan must be single-host or host-invariant.
// With an empty sid this converter CANNOT express a per-host value — it answers
// with [RenderedTask.Params] and [RenderedTask.RenderContextBySID] is not consulted
// — so a plan carrying [RenderedTask.ParamsBySID] would be flattened onto the first
// host by SID for everyone, which is the defect NIM-908 removes. Callers are held
// to it: pushorch converts per SID inside its fan-out, and trial L2 refuses such a
// plan outright (it has one stand and cannot honour it). A new caller is caught by
// TestProtoTasks_EveryMultiHostDispatchPathPassesASID.
func ToProtoTasks(tasks []*RenderedTask) []*keeperv1.RenderedTask {
	return ToProtoTasksForHost(tasks, "")
}

// PlanIsPerHost reports whether any task of the plan renders different params on
// different hosts, i.e. whether the plan can only be dispatched through
// [ToProtoTasksForHost] with a real SID.
//
// Exported for the consumers that must REFUSE such a plan rather than flatten it:
// trial L2, which applies one ApplyRequest to a single stand. A caller that can
// dispatch per host does not need to ask — it simply passes its SID.
func PlanIsPerHost(tasks []*RenderedTask) bool {
	for _, t := range tasks {
		if t != nil && len(t.ParamsBySID) > 0 {
			return true
		}
	}
	return false
}

// ToProtoTasksForHost is the main render→proto converter for a specific host.
// Identical to the golden path, but it answers with THIS host's params where they
// differ from the plan's golden-path struct — [RenderedTask.ParamsBySID] whole
// (NIM-908), or the narrower per-host render_context overlay
// (RenderedTask.RenderContextBySID[sid]) onto Params: otherwise every host would get the first-by-SID host's
// render_context, and a self-variant template (`{{ .self.network.primary_ip }}`)
// would render with the first host's facts (CORE bug, partial closure of open
// Q #25). sid=="" OR no SID key OR host-invariant render_context (nil map —
// only populated for multi-host) → no overlay, Params pass through as-is
// (bit-for-bit with prior behavior).
//
// The overlay is data-safe: t.Params is NOT mutated — a new *structpb.Struct
// is assembled with the same Fields, only the render_context key is replaced
// (see paramsForHost). The same *RenderedTask remains reusable for other SIDs
// (groupByHost puts one pointer into perHost for each host).
func ToProtoTasksForHost(tasks []*RenderedTask, sid string) []*keeperv1.RenderedTask {
	globalToLocal := localIndexMap(tasks)
	out := make([]*keeperv1.RenderedTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, &keeperv1.RenderedTask{
			Name:         t.Name,
			Module:       t.Module,
			Params:       paramsForHost(t, sid),
			SecretOutput: t.SecretOutput,
			Timeout:      t.Timeout,
			OnchangesIdx: remapRequisites(t.OnChangesIdx, globalToLocal),
			OnfailIdx:    remapRequisites(t.OnFailIdx, globalToLocal),
			// AggregateOf carries the GLOBAL Index of the applier's child destiny
			// tasks (applier-register materialization, Variant B) — remap global→local
			// with the same remapRequisites as onchanges/onfail: Soul aggregates by
			// LOCAL position in registerByIdx (applyrunner). A child task filtered out
			// by where: on this host / that landed in another Passage is encoded as
			// the sentinel (-1) → its contribution to the OR is zero (changed=false).
			AggregateOf: remapRequisites(t.AggregateOf, globalToLocal),
			// Intra-host concurrency (ADR-0075). RequireIdx is remapped
			// global→local with the same remapRequisites, but the sentinel (-1)
			// means "nothing to wait for" here, not "contributes false to a gate":
			// a source absent from this slice was filtered out by where: on this
			// host (it never ran) or lives in an EARLIER Passage (already closed on
			// every host). A source in a LATER Passage never reaches this point —
			// resolveRequire rejects that plan.
			Async:       t.Async,
			RequireIdx:  remapRequisites(t.RequireIdx, globalToLocal),
			RequireAll:  t.RequireAll,
			When:        t.When,
			ChangedWhen: t.ChangedWhen,
			FailedWhen:  t.FailedWhen,
			FlowContext: t.FlowContext,
			Register:    t.Register,
			Until:       t.Until,
			RetryCount:  int32(t.RetryCount),
			RetryDelay:  t.RetryDelay,
			// PlanIndex is the task's global index (across all Passages), the
			// register-correlation key on Keeper (ADR-056 §S1 fix Variant B). Soul
			// echoes it back in TaskEvent.plan_index. Indexes are small and
			// non-negative → the int32 narrowing is safe.
			PlanIndex: int32(t.Index),
		})
	}
	return out
}

// paramsForHost returns a task's wire params for a specific sid. Golden path
// (sid=="" / no per-host render_context for this SID) → t.Params as-is.
// Otherwise — a NEW *structpb.Struct with the same Fields, where the
// render_context key is replaced by the per-host variant
// t.RenderContextBySID[sid] (single-key overlay). t.Params isn't mutated: the
// same *RenderedTask dispatches to multiple SIDs, so the shared Params must
// stay unchanged (other Fields' values are shared read-only — sufficient for
// wire marshaling).
func paramsForHost(t *RenderedTask, sid string) *structpb.Struct {
	if sid == "" {
		return t.Params
	}
	// The WHOLE-struct case first (NIM-908): a per-host struct was rendered in this
	// host's own environment, so it already carries this host's render_context — the
	// single-key overlay below would only put the same value back. Returned directly,
	// and it is not mutated: the same *RenderedTask dispatches to every SID.
	if st, ok := t.ParamsBySID[sid]; ok && st != nil {
		return st
	}
	if t.RenderContextBySID == nil {
		return t.Params
	}
	rc, ok := t.RenderContextBySID[sid]
	if !ok || rc == nil {
		return t.Params
	}
	fields := make(map[string]*structpb.Value, len(t.Params.GetFields()))
	for k, v := range t.Params.GetFields() {
		fields[k] = v
	}
	fields[paramRenderContext] = structpb.NewStructValue(rc)
	return &structpb.Struct{Fields: fields}
}

// localIndexMap builds a map from global RenderedTask.Index to local position
// in the slice (0-based, order = tasks order). The slice is what actually
// goes into ApplyRequest.tasks[] for a specific host/Passage, so its position
// is exactly the key Soul uses to index registerByIdx (applyrunner.go). The
// map is used to remap onchanges/onfail indexes from global to local.
func localIndexMap(tasks []*RenderedTask) map[int]int32 {
	m := make(map[int]int32, len(tasks))
	for pos, t := range tasks {
		m[t.Index] = int32(pos)
	}
	return m
}

// outOfRangeRequisite is the sentinel index for an onchanges/onfail source
// ABSENT from ToProtoTasks's input slice (source in another Passage OR
// filtered out by where: on this host). Soul treats registerByIdx[neg]=nil →
// changed/failed=false (applyrunner.go skipOnChanges/skipOnFail: an absent
// source doesn't "save" from skip / doesn't "trigger" onfail). Absence is
// encoded as an explicit sentinel rather than dropping the element: dropping
// would shift the remaining indexes and break the AND semantics of multiple
// sources (at least one changed → run).
const outOfRangeRequisite int32 = -1

// remapRequisites translates onchanges/onfail indexes from the global
// RenderedTask.Index into a local position via the globalToLocal map (see
// localIndexMap). A source missing from the map (cross-passage / filtered by
// where:) is encoded as outOfRangeRequisite — Soul treats it as
// changed/failed=false (see the constant). nil/empty → nil (unconditional run
// / not an onfail task, omitempty field).
func remapRequisites(globalIdx []int, globalToLocal map[int]int32) []int32 {
	if len(globalIdx) == 0 {
		return nil
	}
	out := make([]int32, len(globalIdx))
	for i, g := range globalIdx {
		if local, ok := globalToLocal[g]; ok {
			out[i] = local
		} else {
			out[i] = outOfRangeRequisite
		}
	}
	return out
}

// PerHostParamValue is what the run-plan view shows in place of a params cell whose
// value differs between hosts (NIM-908). It is a DISPLAY marker, not a mask: the
// value is not secret, it is plural, and there is one column to show it in.
const PerHostParamValue = "<per-host>"

// PlanParamsForDisplay is a task's params as the operator-facing run plan should
// show them: the golden-path struct, with every cell that differs between hosts
// replaced by [PerHostParamValue].
//
// The plan row (`apply_run_plan`, one row per task) and `GET …/runs/{id}/tasks`
// carry ONE params map per task beside per-host RESULTS. Before NIM-908 that was
// always honest, because params were host-invariant or the render refused. Now a
// task can legitimately render nine different values, and showing the first host's
// as if it were everyone's is the same silent substitution this ticket removed from
// the run itself — in the one place an operator goes to see what ran.
//
// Deliberately NOT a schema change: the differing cells are marked, the rest stay
// visible, and no API field moves. Showing the real per-host values needs a per-host
// params column in the reply, which is a companion-UI change and its own ticket.
//
// nil task / nil Params → nil. No ParamsBySID → the map unchanged (golden path).
func PlanParamsForDisplay(t *RenderedTask) map[string]any {
	return MarkPerHostCells(t, PlanParamsBase(t))
}

// PlanParamsBase is the golden-path params map the display starts from.
func PlanParamsBase(t *RenderedTask) map[string]any {
	if t == nil || t.Params == nil {
		return nil
	}
	return t.Params.AsMap()
}

// MarkPerHostCells replaces every cell of m that differs between hosts with
// [PerHostParamValue].
//
// ★ Split from [PlanParamsBase] so the caller can MASK first and mark second. The
// order matters for a channel nobody would miss until it was needed: the run-plan
// masker has a regex last resort that raises an ALARM when it finds something
// secret-shaped at a path the seal did not cover ([audit.SealOpts.RegexFallback]).
// Substituting the marker first would delete the value before that layer ever saw
// it — no leak, but the alarm that says "the seal missed a cell" would stop firing
// for exactly the cells this ticket made plural.
//
// A cell the masker already replaced is left alone: the seal's verdict outranks a
// display marker, and "masked" is the more important thing for a reader to see.
func MarkPerHostCells(t *RenderedTask, m map[string]any) map[string]any {
	if t == nil || m == nil || len(t.ParamsBySID) == 0 {
		return m
	}
	for key, val := range m {
		if s, isStr := val.(string); isStr && s == audit.MaskedValue {
			continue
		}
		if paramCellVaries(t, key) {
			m[key] = PerHostParamValue
		}
	}
	return m
}

// paramCellVaries reports whether one top-level params key holds different values on
// different hosts. Top-level only: the marker replaces a whole cell, because a
// half-marked nested map reads as data rather than as a notice.
func paramCellVaries(t *RenderedTask, key string) bool {
	// seen, not `first != nil`: a key ABSENT on the first host by SID and present on
	// another is variance, and a nil-vs-nil comparison would read it as agreement.
	var first *structpb.Value
	seen := false
	for _, sid := range sortedParamSIDs(t.ParamsBySID) {
		v := t.ParamsBySID[sid].GetFields()[key]
		if !seen {
			first, seen = v, true
			continue
		}
		if !proto.Equal(first, v) {
			return true
		}
	}
	return false
}

func sortedParamSIDs(byHost map[string]*structpb.Struct) []string {
	out := make([]string, 0, len(byHost))
	for sid := range byHost {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out
}
