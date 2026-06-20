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

// TestToProtoTasks_RemapOnChangesGlobalToLocal — ★ R1 REMAP (ADR-056 amend).
// Срез, где Index ≠ локальная позиция (задача-источник Index=2 на локальной поз.1
// из-за отфильтрованной where: Index=1): onchanges_idx источника РЕМАПИТСЯ global→
// local. Реверс (без remap) → onchanges_idx==2 (глобальный), Soul ключует
// registerByIdx[2]=nil → restart молча SKIPPED. Тест ловит этот реверс.
func TestToProtoTasks_RemapOnChangesGlobalToLocal(t *testing.T) {
	// Локальный per-host срез: Index 1 отфильтрован where: → в срез не попал.
	// Локальные позиции: [0]=Index0, [1]=Index2, [2]=Index3.
	tasks := []*RenderedTask{
		{Index: 0, Name: "config-change", Module: "core.file.present", Register: "cfg"},
		{Index: 2, Name: "other-change", Module: "core.file.present", Register: "other"},
		{Index: 3, Name: "restart", Module: "core.service.restarted", OnChangesIdx: []int{2}},
	}
	got := ToProtoTasks(tasks)
	oc := got[2].GetOnchangesIdx()
	if len(oc) != 1 || oc[0] != 1 {
		t.Fatalf("onchanges_idx = %v, want [1] (global Index 2 → локальная позиция 1; реверс без remap дал бы [2] → registerByIdx промах → restart молча SKIPPED)", oc)
	}
}

// TestToProtoTasks_RemapOnFailGlobalToLocal — onfail-зеркало remap-теста: onfail_idx
// источника РЕМАПИТСЯ global→local тем же конвертером. Реверс → onfail_idx==2 →
// onfail-rescue молча НЕ запускается.
func TestToProtoTasks_RemapOnFailGlobalToLocal(t *testing.T) {
	tasks := []*RenderedTask{
		{Index: 0, Name: "deploy", Module: "core.exec.run", Register: "deploy"},
		{Index: 2, Name: "migrate", Module: "core.exec.run", Register: "migrate"},
		{Index: 3, Name: "rollback", Module: "core.exec.run", OnFailIdx: []int{2}},
	}
	got := ToProtoTasks(tasks)
	of := got[2].GetOnfailIdx()
	if len(of) != 1 || of[0] != 1 {
		t.Fatalf("onfail_idx = %v, want [1] (global Index 2 → локальная позиция 1; реверс дал бы [2] → onfail-rescue молча НЕ запускается)", of)
	}
}

// TestToProtoTasks_RemapMissingSourceSentinel — источник onchanges/onfail
// ОТСУТСТВУЕТ во входном срезе (cross-passage ИЛИ отфильтрован where: на этом
// хосте) → кодируется sentinel-ом outOfRangeRequisite (-1), а НЕ выкидывается.
// Soul трактует registerByIdx[-1]=nil → changed/failed=false. Выкидывание сместило
// бы остальные индексы и сломало бы AND-семантику нескольких источников.
func TestToProtoTasks_RemapMissingSourceSentinel(t *testing.T) {
	// Срез несёт consumer, но НЕ несёт источник Index=5 (отфильтрован/cross-passage).
	// onchanges:[0,5] — первый источник в срезе (локальная 0), второй отсутствует.
	tasks := []*RenderedTask{
		{Index: 0, Name: "present", Module: "core.file.present", Register: "present"},
		{Index: 3, Name: "restart", Module: "core.service.restarted", OnChangesIdx: []int{0, 5}},
	}
	got := ToProtoTasks(tasks)
	oc := got[1].GetOnchangesIdx()
	if len(oc) != 2 {
		t.Fatalf("onchanges_idx len = %d (%v), want 2 (отсутствующий источник кодируется sentinel-ом, НЕ выкидывается)", len(oc), oc)
	}
	if oc[0] != 0 {
		t.Errorf("onchanges_idx[0] = %d, want 0 (присутствующий источник Index 0 → локальная 0)", oc[0])
	}
	if oc[1] != outOfRangeRequisite {
		t.Errorf("onchanges_idx[1] = %d, want %d (sentinel отсутствующего источника)", oc[1], outOfRangeRequisite)
	}
}

// TestToProtoTasks_RemapIdentityBackwardCompat — ★ BACKWARD-COMPAT: N=1 без where
// (срез = полный план, Index==localPos для всех) → remap=identity, onchanges_idx
// БИТ-В-БИТ совпадает с глобальным Index. Гарантирует, что remap НЕ ломает
// не-staged / без-where прогон.
func TestToProtoTasks_RemapIdentityBackwardCompat(t *testing.T) {
	tasks := []*RenderedTask{
		{Index: 0, Name: "a", Module: "core.file.present", Register: "a"},
		{Index: 1, Name: "b", Module: "core.file.present", Register: "b"},
		{Index: 2, Name: "c", Module: "core.service.restarted", OnChangesIdx: []int{0, 1}},
	}
	got := ToProtoTasks(tasks)
	oc := got[2].GetOnchangesIdx()
	if len(oc) != 2 || oc[0] != 0 || oc[1] != 1 {
		t.Fatalf("onchanges_idx = %v, want [0 1] (identity: global==local при Index==позиция)", oc)
	}
	for i, pt := range got {
		if pt.GetPlanIndex() != int32(i) {
			t.Errorf("task[%d] plan_index = %d, want %d (identity backward-compat)", i, pt.GetPlanIndex(), i)
		}
	}
}
