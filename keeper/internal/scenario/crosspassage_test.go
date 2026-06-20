package scenario

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// keySet — множество (sid, planIndex) для CHANGED/FAILED-факта (auditpg-форма).
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

// fullPlan собирает passageByIndex для (Index→Passage). Пары: index, passage, …
func fullPlan(pairs ...int) []*render.RenderedTask {
	out := make([]*render.RenderedTask, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, &render.RenderedTask{Index: pairs[i], Passage: pairs[i+1]})
	}
	return out
}

// taskNames — отсортированные имена задач среза хоста (для ассертов include/exclude).
func taskNames(ts []*render.RenderedTask) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// findTask — задача среза по имени.
func findTask(ts []*render.RenderedTask, name string) *render.RenderedTask {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// TestCrossPassageGate_OnChangesFires — cross-passage onchanges: источник (idx 0,
// Passage 0) CHANGED на хосте → consumer (idx 1, Passage 1) выполняется, cross-
// passage idx убран с wire (OnChangesIdx пуст → Soul безусловно).
func TestCrossPassageGate_OnChangesFires(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan,
		keySet(key("host-a", 0)), // источник 0 changed на host-a
		nil)

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	perHost := map[string][]*render.RenderedTask{"host-a": {consumer}}

	got := g.applyGate(perHost, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 || tasks[0].Name != "restart" {
		t.Fatalf("host-a срез = %v, want [restart] (onchanges-источник changed → выполняется)", taskNames(tasks))
	}
	if len(tasks[0].OnChangesIdx) != 0 {
		t.Errorf("OnChangesIdx = %v, want [] (cross-passage idx убран → безусловно)", tasks[0].OnChangesIdx)
	}
	// Клон, не мутация исходного consumer-а.
	if len(consumer.OnChangesIdx) != 1 {
		t.Errorf("исходный consumer.OnChangesIdx мутирован = %v, want [0]", consumer.OnChangesIdx)
	}
}

// TestCrossPassageGate_OnChangesSkips — cross-passage onchanges: источник НЕ
// changed (ok/skipped) → consumer ИСКЛЮЧАЕТСЯ (нет same-passage источника). Хост
// без других задач выпадает из среза.
func TestCrossPassageGate_OnChangesSkips(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan, keySet(), nil) // ничего не changed

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	perHost := map[string][]*render.RenderedTask{"host-a": {consumer}}

	got := g.applyGate(perHost, 1)
	if _, present := got["host-a"]; present {
		t.Fatalf("host-a остался в срезе = %v, want выпал (onchanges не сработал, нет same-passage)", taskNames(got["host-a"]))
	}
}

// TestCrossPassageGate_OnFailFires — cross-passage onfail (rescue): источник
// FAILED на хосте → rescue выполняется, cross-passage idx убран с wire.
func TestCrossPassageGate_OnFailFires(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan,
		nil,
		keySet(key("host-a", 0))) // источник 0 failed на host-a

	rescue := &render.RenderedTask{Index: 1, Passage: 1, Name: "rescue", OnFailIdx: []int{0}}
	perHost := map[string][]*render.RenderedTask{"host-a": {rescue}}

	got := g.applyGate(perHost, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 || tasks[0].Name != "rescue" {
		t.Fatalf("host-a срез = %v, want [rescue] (onfail-источник failed → rescue выполняется)", taskNames(tasks))
	}
	if len(tasks[0].OnFailIdx) != 0 {
		t.Errorf("OnFailIdx = %v, want [] (cross-passage idx убран → безусловно)", tasks[0].OnFailIdx)
	}
}

// TestCrossPassageGate_OnFailSkips — cross-passage onfail: источник НЕ failed →
// rescue исключается.
func TestCrossPassageGate_OnFailSkips(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan, nil, keySet()) // ничего не failed

	rescue := &render.RenderedTask{Index: 1, Passage: 1, Name: "rescue", OnFailIdx: []int{0}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {rescue}}, 1)
	if _, present := got["host-a"]; present {
		t.Fatalf("host-a остался = %v, want выпал (onfail не сработал)", taskNames(got["host-a"]))
	}
}

// TestCrossPassageGate_PerHostDivergent — источник CHANGED на host-a, НЕ на
// host-b → consumer выполняется ТОЛЬКО на host-a. Per-host разрешение.
func TestCrossPassageGate_PerHostDivergent(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	g := newCrossPassageGate(plan, keySet(key("host-a", 0)), nil) // changed только на host-a

	mk := func() *render.RenderedTask {
		return &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	}
	perHost := map[string][]*render.RenderedTask{"host-a": {mk()}, "host-b": {mk()}}

	got := g.applyGate(perHost, 1)
	if len(got["host-a"]) != 1 {
		t.Errorf("host-a = %v, want [restart] (источник changed)", taskNames(got["host-a"]))
	}
	if _, present := got["host-b"]; present {
		t.Errorf("host-b = %v, want выпал (источник НЕ changed на host-b)", taskNames(got["host-b"]))
	}
}

// TestCrossPassageGate_SkippedSourceNotChanged — источник был SKIPPED (его нет в
// CHANGED-set) → consumer onchanges:[A] SKIPPED. CHANGED-set-семантика: skipped ≠
// changed, даже если register-строка источника существует (register пишется и для
// ok/skipped). Здесь источник в CHANGED-set ОТСУТСТВУЕТ → не сработал.
func TestCrossPassageGate_SkippedSourceNotChanged(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1)
	// CHANGED-set пуст: источник 0 завершился SKIPPED/OK, register-строка могла
	// быть записана, но в CHANGED-set его нет.
	g := newCrossPassageGate(plan, keySet(), nil)

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {consumer}}, 1)
	if _, present := got["host-a"]; present {
		t.Fatalf("host-a остался = %v, want выпал (skipped-источник ≠ changed)", taskNames(got["host-a"]))
	}
}

// TestCrossPassageGate_MixedCrossAndSame_CrossNotChanged — OR-семантика
// cross+same. Consumer (Passage 1) имеет onchanges:[A(cross, idx0, Passage0),
// B(same, idx2, Passage1)]. A НЕ changed, но B — same-passage (Soul гейтит сам по
// нему): consumer ОСТАЁТСЯ на хосте, cross-idx A убран, same-idx B на wire.
func TestCrossPassageGate_MixedCrossAndSame_CrossNotChanged(t *testing.T) {
	// idx0 Passage0 (A, cross); idx2 Passage1 (B, same); idx1 Passage1 (consumer).
	plan := fullPlan(0, 0, 1, 1, 2, 1)
	g := newCrossPassageGate(plan, keySet(), nil) // A НЕ changed

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0, 2}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {consumer}}, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 {
		t.Fatalf("host-a = %v, want [restart] (есть same-passage onchanges B → не исключается, Soul гейтит)", taskNames(tasks))
	}
	// cross-idx 0 (A) убран; same-idx 2 (B) остаётся → Soul гейтит по B.
	if len(tasks[0].OnChangesIdx) != 1 || tasks[0].OnChangesIdx[0] != 2 {
		t.Errorf("OnChangesIdx = %v, want [2] (cross A убран, same B на wire → Soul гейтит по B)", tasks[0].OnChangesIdx)
	}
}

// TestCrossPassageGate_MixedCrossAndSame_CrossChanged — A(cross) CHANGED → OR
// requisite УДОВЛЕТВОРЁН keeper-side → consumer выполняется БЕЗУСЛОВНО, ВЕСЬ
// onchanges снят с wire (и cross A, и same B). same B оставлять НЕЛЬЗЯ: Soul
// пере-гейтил бы по нему и при B-not-changed ЛОЖНО skip-нул бы consumer, хотя cross
// A уже спас (OR). ASSERT: остаётся, OnChangesIdx пуст.
func TestCrossPassageGate_MixedCrossAndSame_CrossChanged(t *testing.T) {
	plan := fullPlan(0, 0, 1, 1, 2, 1)
	g := newCrossPassageGate(plan, keySet(key("host-a", 0)), nil) // A changed

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0, 2}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {consumer}}, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 {
		t.Fatalf("host-a = %v, want [restart] (A changed → выполняется)", taskNames(tasks))
	}
	// ВЕСЬ onchanges снят (OR удовлетворён cross A): Soul выполняет безусловно, не
	// пере-гейтит по same B.
	if len(tasks[0].OnChangesIdx) != 0 {
		t.Errorf("OnChangesIdx = %v, want [] (OR удовлетворён cross A → весь requisite снят, безусловно)", tasks[0].OnChangesIdx)
	}
}

// TestCrossPassageGate_NoCrossPassage_Untouched — задача без cross-passage
// requisite (все источники same-passage) возвращается ТЕМ ЖЕ указателем (keeper не
// трогает, R1-remap на Soul-е).
func TestCrossPassageGate_NoCrossPassage_Untouched(t *testing.T) {
	plan := fullPlan(0, 1, 1, 1) // оба в Passage 1 (same)
	g := newCrossPassageGate(plan, keySet(), nil)

	consumer := &render.RenderedTask{Index: 1, Passage: 1, Name: "restart", OnChangesIdx: []int{0}}
	got := g.applyGate(map[string][]*render.RenderedTask{"host-a": {consumer}}, 1)
	tasks := got["host-a"]
	if len(tasks) != 1 || tasks[0] != consumer {
		t.Fatalf("same-passage requisite: задача должна вернуться тем же указателем без клона")
	}
}

// TestCrossPassageGate_Nil — nil-gate (N=1 / Passage 0) → perHost как есть.
func TestCrossPassageGate_Nil(t *testing.T) {
	var g *crossPassageGate
	consumer := &render.RenderedTask{Index: 0, Passage: 0, Name: "t"}
	in := map[string][]*render.RenderedTask{"host-a": {consumer}}
	got := g.applyGate(in, 0)
	if len(got["host-a"]) != 1 || got["host-a"][0] != consumer {
		t.Fatalf("nil-gate должен вернуть perHost как есть")
	}
}
