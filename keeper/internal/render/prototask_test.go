package render

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

func TestToProtoTasks(t *testing.T) {
	params, _ := structpb.NewStruct(map[string]any{"cmd": "echo hi"})
	tasks := []*RenderedTask{
		{Index: 0, Name: "echo", Module: "core.exec.run", Params: params, Register: "r", NoLog: true, Timeout: "30s"},
	}
	got := ToProtoTasks(tasks)
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	pt := got[0]
	if pt.GetName() != "echo" || pt.GetModule() != "core.exec.run" {
		t.Errorf("name/module = %q/%q", pt.GetName(), pt.GetModule())
	}
	if pt.GetParams().GetFields()["cmd"].GetStringValue() != "echo hi" {
		t.Errorf("params not propagated: %v", pt.GetParams())
	}
	if !pt.GetNoLog() {
		t.Errorf("no_log not propagated")
	}
	// timeout: must reach the wire form (catches a threading regression —
	// MAJOR #2: the field was silently dropped before RenderedTask.Timeout).
	if pt.GetTimeout() != "30s" {
		t.Errorf("timeout = %q, want 30s (config.Task -> render -> proto propagation broken)", pt.GetTimeout())
	}
}

// TestToProtoTasks_OnChangesIdx — render.OnChangesIdx ([]int) reaches the wire
// proto onchanges_idx ([]int32). Catches a threading regression of the gating
// indices (without it restart-flap isn't fixed: Soul won't get the indices and
// will always run).
func TestToProtoTasks_OnChangesIdx(t *testing.T) {
	tasks := []*RenderedTask{
		{Index: 0, Name: "redis_conf", Module: "core.file.present"},
		{Index: 1, Name: "restart", Module: "core.service.restarted", OnChangesIdx: []int{0}},
	}
	got := ToProtoTasks(tasks)
	if got[0].GetOnchangesIdx() != nil {
		t.Errorf("source: onchanges_idx = %v, want nil", got[0].GetOnchangesIdx())
	}
	oc := got[1].GetOnchangesIdx()
	if len(oc) != 1 || oc[0] != 0 {
		t.Errorf("consumer: onchanges_idx = %v, want [0]", oc)
	}
}

// TestToProtoTasks_RemapOnChangesGlobalToLocal — ★ R1 REMAP (ADR-056 amend).
// A slice where Index ≠ local position (source task Index=2 at local pos.1
// because where: filtered out Index=1): the source's onchanges_idx is REMAPPED
// global→local. A reversion (no remap) → onchanges_idx==2 (global), Soul keys
// registerByIdx[2]=nil → restart silently SKIPPED. This test catches that
// reversion.
func TestToProtoTasks_RemapOnChangesGlobalToLocal(t *testing.T) {
	// Local per-host slice: Index 1 filtered out by where: → didn't make the slice.
	// Local positions: [0]=Index0, [1]=Index2, [2]=Index3.
	tasks := []*RenderedTask{
		{Index: 0, Name: "config-change", Module: "core.file.present", Register: "cfg"},
		{Index: 2, Name: "other-change", Module: "core.file.present", Register: "other"},
		{Index: 3, Name: "restart", Module: "core.service.restarted", OnChangesIdx: []int{2}},
	}
	got := ToProtoTasks(tasks)
	oc := got[2].GetOnchangesIdx()
	if len(oc) != 1 || oc[0] != 1 {
		t.Fatalf("onchanges_idx = %v, want [1] (global Index 2 -> local position 1; reverse without remap would give [2] -> registerByIdx miss -> restart silently SKIPPED)", oc)
	}
}

// TestToProtoTasks_RemapOnFailGlobalToLocal — onfail mirror of the remap test:
// the source's onfail_idx is REMAPPED global→local by the same converter. A
// reversion → onfail_idx==2 → onfail-rescue silently does NOT run.
func TestToProtoTasks_RemapOnFailGlobalToLocal(t *testing.T) {
	tasks := []*RenderedTask{
		{Index: 0, Name: "deploy", Module: "core.exec.run", Register: "deploy"},
		{Index: 2, Name: "migrate", Module: "core.exec.run", Register: "migrate"},
		{Index: 3, Name: "rollback", Module: "core.exec.run", OnFailIdx: []int{2}},
	}
	got := ToProtoTasks(tasks)
	of := got[2].GetOnfailIdx()
	if len(of) != 1 || of[0] != 1 {
		t.Fatalf("onfail_idx = %v, want [1] (global Index 2 -> local position 1; reverse would give [2] -> onfail-rescue silently does NOT run)", of)
	}
}

// TestToProtoTasks_RemapMissingSourceSentinel — an onchanges/onfail source is
// ABSENT from the input slice (cross-passage OR filtered by where: on this
// host) → encoded with sentinel outOfRangeRequisite (-1), NOT dropped. Soul
// treats registerByIdx[-1]=nil → changed/failed=false. Dropping it would shift
// the remaining indices and break the AND semantics of multiple sources.
func TestToProtoTasks_RemapMissingSourceSentinel(t *testing.T) {
	// The slice carries the consumer but NOT the source Index=5 (filtered/cross-passage).
	// onchanges:[0,5] — the first source is in the slice (local 0), the second is absent.
	tasks := []*RenderedTask{
		{Index: 0, Name: "present", Module: "core.file.present", Register: "present"},
		{Index: 3, Name: "restart", Module: "core.service.restarted", OnChangesIdx: []int{0, 5}},
	}
	got := ToProtoTasks(tasks)
	oc := got[1].GetOnchangesIdx()
	if len(oc) != 2 {
		t.Fatalf("onchanges_idx len = %d (%v), want 2 (a missing source is encoded with a sentinel, NOT dropped)", len(oc), oc)
	}
	if oc[0] != 0 {
		t.Errorf("onchanges_idx[0] = %d, want 0 (a present source Index 0 -> local 0)", oc[0])
	}
	if oc[1] != outOfRangeRequisite {
		t.Errorf("onchanges_idx[1] = %d, want %d (sentinel of a missing source)", oc[1], outOfRangeRequisite)
	}
}

// TestToProtoTasks_RemapIdentityBackwardCompat — ★ BACKWARD-COMPAT: N=1 without
// where (slice = full plan, Index==localPos for all) → remap=identity,
// onchanges_idx matches the global Index BIT-FOR-BIT. Guarantees remap doesn't
// break a non-staged / where-less run.
func TestToProtoTasks_RemapIdentityBackwardCompat(t *testing.T) {
	tasks := []*RenderedTask{
		{Index: 0, Name: "a", Module: "core.file.present", Register: "a"},
		{Index: 1, Name: "b", Module: "core.file.present", Register: "b"},
		{Index: 2, Name: "c", Module: "core.service.restarted", OnChangesIdx: []int{0, 1}},
	}
	got := ToProtoTasks(tasks)
	oc := got[2].GetOnchangesIdx()
	if len(oc) != 2 || oc[0] != 0 || oc[1] != 1 {
		t.Fatalf("onchanges_idx = %v, want [0 1] (identity: global==local when Index==position)", oc)
	}
	for i, pt := range got {
		if pt.GetPlanIndex() != int32(i) {
			t.Errorf("task[%d] plan_index = %d, want %d (identity backward-compat)", i, pt.GetPlanIndex(), i)
		}
	}
}
