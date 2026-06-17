package scenario

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
)

func TestMergeStateChanges_EmptyNoop(t *testing.T) {
	before := map[string]any{"users": []any{"alice"}, "count": float64(1)}

	// Пустой renderedSets → state не меняется (deep-copy).
	after := mergeStateChanges(before, nil)
	if len(after) != 2 {
		t.Fatalf("after = %+v, want copy of before", after)
	}
	if after["count"] != float64(1) {
		t.Errorf("count = %v", after["count"])
	}

	// Мутация копии не задевает оригинал (deep-copy, не ссылка).
	after["count"] = float64(99)
	if before["count"] != float64(1) {
		t.Errorf("before mutated through copy: %v", before["count"])
	}
}

func TestMergeStateChanges_AppliesSets(t *testing.T) {
	before := map[string]any{"existing": "keep", "count": float64(1)}
	rendered := map[string]any{
		"greeting_file": "/tmp/soul-stack-hello", // новое поле
		"count":         float64(42),             // перезапись существующего
	}
	after := mergeStateChanges(before, rendered)

	if after["greeting_file"] != "/tmp/soul-stack-hello" {
		t.Errorf("greeting_file = %v, want /tmp/soul-stack-hello", after["greeting_file"])
	}
	if after["count"] != float64(42) {
		t.Errorf("count = %v, want 42 (set overrides)", after["count"])
	}
	if after["existing"] != "keep" {
		t.Errorf("existing = %v, want keep (untouched fields preserved)", after["existing"])
	}
	// Оригинал не задет.
	if before["count"] != float64(1) {
		t.Errorf("before mutated: count = %v", before["count"])
	}
	if _, ok := before["greeting_file"]; ok {
		t.Errorf("before mutated: greeting_file leaked into stateBefore")
	}
}

func TestMergeStateChanges_NilBefore(t *testing.T) {
	after := mergeStateChanges(nil, nil)
	if after == nil {
		t.Fatal("after = nil, want empty map")
	}
	if len(after) != 0 {
		t.Errorf("after = %+v, want empty", after)
	}

	// nil before + непустой sets → state из sets.
	after = mergeStateChanges(nil, map[string]any{"x": "y"})
	if after["x"] != "y" {
		t.Errorf("after = %+v, want {x:y}", after)
	}
}

func TestBuildRegisterByHost_ResolvesNamesPerHost(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "probe_a"},
		{Index: 1, Register: ""}, // задача без register: — её строки игнорируются
		{Index: 2, Register: "probe_b"},
	}
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", TaskIdx: 0, RegisterData: map[string]any{"stdout": "1a"}},
		{ApplyID: "a", SID: "host-1", TaskIdx: 2, RegisterData: map[string]any{"stdout": "1b"}},
		{ApplyID: "a", SID: "host-2", TaskIdx: 0, RegisterData: map[string]any{"stdout": "2a"}},
		{ApplyID: "a", SID: "host-2", TaskIdx: 1, RegisterData: map[string]any{"stdout": "ignored"}}, // task без register:
	}

	got := buildRegisterByHost(rows, tasks)

	if len(got) != 2 {
		t.Fatalf("hosts = %d, want 2", len(got))
	}
	if v := got["host-1"]["probe_a"].(map[string]any)["stdout"]; v != "1a" {
		t.Errorf("host-1.probe_a.stdout = %v, want 1a", v)
	}
	if v := got["host-1"]["probe_b"].(map[string]any)["stdout"]; v != "1b" {
		t.Errorf("host-1.probe_b.stdout = %v, want 1b", v)
	}
	if v := got["host-2"]["probe_a"].(map[string]any)["stdout"]; v != "2a" {
		t.Errorf("host-2.probe_a.stdout = %v, want 2a", v)
	}
	// task_idx=1 без register: не должен попасть.
	if _, ok := got["host-2"]["probe_b"]; ok {
		t.Errorf("host-2.probe_b не должен существовать (task без register:)")
	}
	if len(got["host-2"]) != 1 {
		t.Errorf("host-2 register-ключей = %d, want 1", len(got["host-2"]))
	}
}

// Вариант B: register задачи с NoLog=true НЕ аккумулируется в per-host map —
// его register-имя не попадает в nameByIdx, поэтому строка пропускается и
// чувствительное значение не доходит до state-графа (orchestration.md §7).
func TestBuildRegisterByHost_NoLogTaskExcluded(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "plain"},                     // обычная — аккумулируется
		{Index: 1, Register: "secret_probe", NoLog: true}, // no_log — НЕ аккумулируется
	}
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", TaskIdx: 0, RegisterData: map[string]any{"stdout": "ok"}},
		{ApplyID: "a", SID: "host-1", TaskIdx: 1, RegisterData: map[string]any{"stdout": "s3cr3t"}},
	}

	got := buildRegisterByHost(rows, tasks)

	// Обычный register на месте (вариант B не ломает не-no_log задачи).
	if v := got["host-1"]["plain"].(map[string]any)["stdout"]; v != "ok" {
		t.Errorf("host-1.plain.stdout = %v, want ok", v)
	}
	// no_log-register отсутствует → sets, ссылающийся на него, получит no-such-key.
	if _, ok := got["host-1"]["secret_probe"]; ok {
		t.Errorf("host-1.secret_probe не должен существовать (no_log-задача)")
	}
	if len(got["host-1"]) != 1 {
		t.Errorf("host-1 register-ключей = %d, want 1 (только plain)", len(got["host-1"]))
	}
}

func TestBuildRegisterByHost_EmptyRows(t *testing.T) {
	got := buildRegisterByHost(nil, []*render.RenderedTask{{Index: 0, Register: "p"}})
	if got == nil || len(got) != 0 {
		t.Errorf("got = %v, want пустая map", got)
	}
}

func TestDeepCopyMap(t *testing.T) {
	src := map[string]any{
		"nested": map[string]any{"k": "v"},
		"list":   []any{float64(1), float64(2)},
	}
	cp := deepCopyMap(src)
	nested := cp["nested"].(map[string]any)
	nested["k"] = "changed"
	if src["nested"].(map[string]any)["k"] != "v" {
		t.Errorf("deep copy не глубокая: original mutated")
	}
}

func TestOSFamilyOf(t *testing.T) {
	h := &topology.HostFacts{Soulprint: map[string]any{
		"os": map[string]any{"family": "debian"},
	}}
	if got := osFamilyOf(h); got != "debian" {
		t.Errorf("osFamilyOf = %q, want debian", got)
	}

	// Нет фактов → "".
	if got := osFamilyOf(&topology.HostFacts{}); got != "" {
		t.Errorf("osFamilyOf(empty) = %q, want \"\"", got)
	}
	// os есть, family нет.
	h2 := &topology.HostFacts{Soulprint: map[string]any{"os": map[string]any{}}}
	if got := osFamilyOf(h2); got != "" {
		t.Errorf("osFamilyOf(no family) = %q", got)
	}
}

func TestSpecEssence(t *testing.T) {
	inc := &incarnation.Incarnation{Spec: map[string]any{
		"essence": map[string]any{"redis_version": "7.2"},
	}}
	got := specEssence(inc)
	if got["redis_version"] != "7.2" {
		t.Errorf("specEssence = %+v", got)
	}

	// Нет spec.essence → nil.
	if specEssence(&incarnation.Incarnation{}) != nil {
		t.Errorf("specEssence(empty) != nil")
	}
}

func TestStartedByPtr(t *testing.T) {
	if startedByPtr("") != nil {
		t.Errorf("startedByPtr(\"\") != nil")
	}
	p := startedByPtr("archon-alice")
	if p == nil || *p != "archon-alice" {
		t.Errorf("startedByPtr = %v", p)
	}
}
