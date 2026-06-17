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
	// timeout: должен дойти до wire-формы (ловит регресс обрыва протяжки —
	// MAJOR #2: поле молча отбрасывалось до появления RenderedTask.Timeout).
	if pt.GetTimeout() != "30s" {
		t.Errorf("timeout = %q, want 30s (протяжка config.Task → render → proto оборвана)", pt.GetTimeout())
	}
}

// TestToProtoTasks_OnChangesIdx — render.OnChangesIdx ([]int) доезжает до wire
// proto onchanges_idx ([]int32). Ловит регресс обрыва протяжки gating-индексов
// (без неё restart-flap не лечится: Soul не получит индексы, выполнит всегда).
func TestToProtoTasks_OnChangesIdx(t *testing.T) {
	tasks := []*RenderedTask{
		{Index: 0, Name: "redis_conf", Module: "core.file.present"},
		{Index: 1, Name: "restart", Module: "core.service.restarted", OnChangesIdx: []int{0}},
	}
	got := ToProtoTasks(tasks)
	if got[0].GetOnchangesIdx() != nil {
		t.Errorf("источник: onchanges_idx = %v, want nil", got[0].GetOnchangesIdx())
	}
	oc := got[1].GetOnchangesIdx()
	if len(oc) != 1 || oc[0] != 0 {
		t.Errorf("потребитель: onchanges_idx = %v, want [0]", oc)
	}
}

func TestInt32Slice(t *testing.T) {
	if got := int32Slice(nil); got != nil {
		t.Errorf("int32Slice(nil) = %v, want nil", got)
	}
	if got := int32Slice([]int{}); got != nil {
		t.Errorf("int32Slice([]) = %v, want nil", got)
	}
	got := int32Slice([]int{0, 3, 7})
	if len(got) != 3 || got[0] != 0 || got[1] != 3 || got[2] != 7 {
		t.Errorf("int32Slice([0 3 7]) = %v", got)
	}
}
