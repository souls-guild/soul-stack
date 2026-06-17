package scenario

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// changedKeys — sugar для построения множества (sid, idx) CHANGED-задач.
func changedKeys(pairs ...auditpg.ChangedTaskKey) map[auditpg.ChangedTaskKey]struct{} {
	out := make(map[auditpg.ChangedTaskKey]struct{}, len(pairs))
	for _, p := range pairs {
		out[p] = struct{}{}
	}
	return out
}

// findByAddr ищет ChangedTask по register/id (для устойчивости к порядку).
func findByAddr(tasks []ChangedTask, register, id string) (ChangedTask, bool) {
	for _, t := range tasks {
		if t.Register == register && t.ID == id {
			return t, true
		}
	}
	return ChangedTask{}, false
}

// TestBuildChangedTasks_ChangedHostsByUniqueSID — базовый инвариант: N хостов
// отметились CHANGED по адресу → ChangedHosts=N (уникальные sid). Таргет 3 хоста,
// CHANGED на 2 → ChangedHosts=2, TotalHosts=3.
func TestBuildChangedTasks_ChangedHostsByUniqueSID(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "install", Register: "pkg", Module: "core.pkg.installed"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"a.local", "b.local", "c.local"}},
	}
	keys := changedKeys(
		auditpg.ChangedTaskKey{SID: "a.local", TaskIdx: 0},
		auditpg.ChangedTaskKey{SID: "b.local", TaskIdx: 0},
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

// TestBuildChangedTasks_LoopNoDoubleCount — КЛЮЧЕВОЙ тест (loop double-count):
// одна исходная loop-задача развёрнута в K=3 RenderedTask (idx 0,1,2) с ОДНИМ
// адресом (register="pkg"), таргет — M=2 хоста на каждый idx. CHANGED отметился
// на обоих хостах по нескольким idx. Инвариант:
//   - TotalHosts = M = 2 (union уникальных sid), НЕ M×K = 6;
//   - ChangedHosts по уникальным sid (НЕ сумма по idx);
//   - одна ChangedTask на адрес (loop-свёртка), idx = первый (репрезентативный).
func TestBuildChangedTasks_LoopNoDoubleCount(t *testing.T) {
	// loop из 3 итераций: один Register на всех, idx сквозные 0,1,2.
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "install_each", Register: "pkg", Module: "core.pkg.installed"},
		{Index: 1, Name: "install_each", Register: "pkg", Module: "core.pkg.installed"},
		{Index: 2, Name: "install_each", Register: "pkg", Module: "core.pkg.installed"},
	}
	// Каждая итерация таргетит ОБА хоста (M=2).
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"a.local", "b.local"}},
		{TaskIndex: 1, TargetSIDs: []string{"a.local", "b.local"}},
		{TaskIndex: 2, TargetSIDs: []string{"a.local", "b.local"}},
	}
	// CHANGED: a.local на idx 0 и 2; b.local на idx 1. По union sid это {a,b} = 2.
	keys := changedKeys(
		auditpg.ChangedTaskKey{SID: "a.local", TaskIdx: 0},
		auditpg.ChangedTaskKey{SID: "a.local", TaskIdx: 2},
		auditpg.ChangedTaskKey{SID: "b.local", TaskIdx: 1},
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

// TestBuildChangedTasks_NoChangesOmitted — таска без CHANGED ни на одном хосте
// НЕ попадает в массив.
func TestBuildChangedTasks_NoChangesOmitted(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "changed_one", Register: "a", Module: "core.file.present"},
		{Index: 1, Name: "untouched", Register: "b", Module: "core.file.present"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"h.local"}},
		{TaskIndex: 1, TargetSIDs: []string{"h.local"}},
	}
	// CHANGED только idx 0.
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "h.local", TaskIdx: 0})

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

// TestBuildChangedTasks_TotalFromDispatchPlan — TotalHosts берётся из
// DispatchPlan.TargetSIDs (после on:/where:), НЕ из всего roster-а. where:
// отфильтровал часть хостов → знаменатель = только таргет.
func TestBuildChangedTasks_TotalFromDispatchPlan(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "on_replicas", Register: "r", Module: "core.service.running"},
	}
	// roster прогона мог быть {a,b,c,d}, но where: оставил {a,b} — план несёт 2.
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"a.local", "b.local"}},
	}
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "a.local", TaskIdx: 0})

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

// TestBuildChangedTasks_AddressIDFallback — задача без register, но с id:
// адресуется по id (register∪id). ID попадает в ChangedTask, Register пуст.
func TestBuildChangedTasks_AddressIDFallback(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "restart", ID: "restart-web", Module: "core.service.running"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"h.local"}},
	}
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "h.local", TaskIdx: 0})

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].ID != "restart-web" || got[0].Register != "" {
		t.Errorf("address fallback: ID=%q Register=%q, want id=restart-web register empty", got[0].ID, got[0].Register)
	}
}

// TestBuildChangedTasks_UnaddressableIncluded — неадресуемая изменившаяся задача
// (нет register и нет id) ВКЛЮЧАЕТСЯ в массив с пустым адресом (полнота «что
// изменилось»). Решение T3: включаем все изменившиеся таски.
func TestBuildChangedTasks_UnaddressableIncluded(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "anon", Module: "core.exec.run"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"h.local"}},
	}
	keys := changedKeys(auditpg.ChangedTaskKey{SID: "h.local", TaskIdx: 0})

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

// TestBuildChangedTasks_UnaddressableNotFolded — две РАЗНЫЕ неадресуемые задачи
// (idx 0 и 1, у обеих нет register/id) НЕ схлопываются в одну запись: каждая
// группируется по своему Index. Защита от ложной свёртки разных задач с пустым
// адресом.
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
		auditpg.ChangedTaskKey{SID: "h.local", TaskIdx: 0},
		auditpg.ChangedTaskKey{SID: "h.local", TaskIdx: 1},
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

// TestBuildChangedTasks_SecretHygiene — payload-форма changed_tasks несёт ТОЛЬКО
// метаданные + counts; никаких register/params-ЗНАЧЕНИЙ. Проверяем форму
// changedTasksPayload: ключи строго {idx,name,register,id,module,changed_hosts,
// total_hosts}, без полей вроде register_data/params/value.
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
	// Counts и метаданные присутствуют.
	if payload[0]["changed_hosts"] != 1 || payload[0]["total_hosts"] != 2 {
		t.Errorf("counts = %v/%v, want 1/2", payload[0]["changed_hosts"], payload[0]["total_hosts"])
	}
	if payload[0]["register"] != "pkg" {
		t.Errorf("register = %v, want pkg", payload[0]["register"])
	}
}

// TestBuildChangedTasks_EmptyInputs — нет задач → nil; задачи есть, но ни одна
// не CHANGED → пустой (не nil) результат на уровне эмиссии не требуется здесь,
// buildChangedTasks возвращает nil-slice (changedTasksPayload даёт []).
func TestBuildChangedTasks_EmptyInputs(t *testing.T) {
	if got := buildChangedTasks(nil, nil, nil); got != nil {
		t.Errorf("buildChangedTasks(nil...) = %+v, want nil", got)
	}
	// changedTasksPayload(nil) → пустой slice (не nil): payload несёт [].
	if p := changedTasksPayload(nil); p == nil {
		t.Error("changedTasksPayload(nil) = nil, want empty slice for JSON []")
	} else if len(p) != 0 {
		t.Errorf("changedTasksPayload(nil) len = %d, want 0", len(p))
	}
}
