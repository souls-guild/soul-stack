package render

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// TestIncarnationVars_StateProjected — RenderInput.State проецируется в CEL как
// `incarnation.state` (Вариант A, ADR-009/010). incarnationVars кладёт ключ
// `state` ← in.State, чтобы scenario-render видел read-only снимок incarnation.state.
func TestIncarnationVars_StateProjected(t *testing.T) {
	state := map[string]any{"redis_users": map[string]any{"alice": map[string]any{"acl": "+@all"}}}
	in := RenderInput{
		Incarnation: IncarnationMeta{Name: "redis", Service: "redis-cluster"},
		State:       state,
	}
	got := incarnationVars(in, 3)
	if got["state"] == nil {
		t.Fatalf("incarnation.state не спроецирован: %v", got)
	}
	if !reflect.DeepEqual(got["state"], state) {
		t.Fatalf("incarnation.state = %v, want %v", got["state"], state)
	}
}

// TestIncarnationVars_NilStateNoKey — nil-State не кладёт ключ `state`
// (backward-compat: push/trial без State видят incarnation.state.x как
// no-such-key, не compile-error — incarnation DynType).
func TestIncarnationVars_NilStateNoKey(t *testing.T) {
	in := RenderInput{Incarnation: IncarnationMeta{Name: "x"}}
	got := incarnationVars(in, 1)
	if _, ok := got["state"]; ok {
		t.Fatalf("nil-State не должен класть ключ state, got: %v", got)
	}
}

// TestRenderState_ReadOnly — ★ read-only инвариант: рендер params/where,
// читающих incarnation.state.*, НЕ мутирует RenderInput.State (CEL читает, не
// пишет — нет пути мутации state через CEL). Снимок до и после eval идентичен.
func TestRenderState_ReadOnly(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	state := map[string]any{
		"redis_users": map[string]any{"alice": map[string]any{"acl": "+@read"}},
		"count":       2,
	}
	in := RenderInput{
		Incarnation: IncarnationMeta{Name: "redis"},
		Input:       map[string]any{"new_acl": "+@all"},
		State:       state,
	}
	host := &topology.HostFacts{SID: "redis-0.example.com", Coven: []string{"redis"}}
	vars := hostVars(in, host, 1)

	// where читает incarnation.state.count.
	if _, err := evalWhere(e, "incarnation.state.count > 0", vars); err != nil {
		t.Fatalf("evalWhere incarnation.state: %v", err)
	}
	// params интерполируют incarnation.state.redis_users (current-for-diff).
	params := map[string]any{
		"current": "${ incarnation.state.redis_users }",
		"new":     "${ input.new_acl }",
	}
	if _, err := renderParams(e, params, vars); err != nil {
		t.Fatalf("renderParams incarnation.state: %v", err)
	}

	want := map[string]any{
		"redis_users": map[string]any{"alice": map[string]any{"acl": "+@read"}},
		"count":       2,
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("★ incarnation.state мутирован eval-ом: got %v, want %v", state, want)
	}
}

// TestRenderState_StagedSnapshotInvariant — ★ staged-snapshot: на staged-прогоне
// (renderIn переиспользуется на P0 и P1+) incarnation.state.* идентичен в обоих
// passages — это pre-run stateBefore, а НЕ промежуточный результат state_changes.
// Симулируем staged-render: тот же RenderInput.State, разный ActivePassage; eval
// incarnation.state.* должен дать один и тот же результат.
func TestRenderState_StagedSnapshotInvariant(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	state := map[string]any{"redis_users": map[string]any{"alice": map[string]any{"acl": "+@read"}}}
	in := RenderInput{
		Incarnation:    IncarnationMeta{Name: "redis"},
		State:          state,
		TaskPassage:    []int{0, 1},
		RegisterByHost: map[string]map[string]any{},
	}
	host := &topology.HostFacts{SID: "redis-0.example.com", Coven: []string{"redis"}}

	eval := func(passage int) any {
		in.ActivePassage = passage
		vars := hostVars(in, host, 1)
		out, err := e.EvalInterpolation("${ incarnation.state.redis_users }", vars)
		if err != nil {
			t.Fatalf("passage %d: EvalInterpolation: %v", passage, err)
		}
		return out
	}

	p0 := eval(0)
	p1 := eval(1)
	if !reflect.DeepEqual(p0, p1) {
		t.Fatalf("★ incarnation.state разошёлся между passages: P0=%v P1=%v (снимок обязан быть инвариантен)", p0, p1)
	}
	// Снимок == исходный pre-run state (не накопление между passages).
	want := map[string]any{"alice": map[string]any{"acl": "+@read"}}
	if !reflect.DeepEqual(p0, want) {
		t.Fatalf("incarnation.state.redis_users = %v, want pre-run %v", p0, want)
	}
}

// TestRenderState_BackwardCompatNoState — backward-compat: без RenderInput.State
// (nil) обращение incarnation.state.x не падает compile-error, а даёт штатный
// no-such-key (incarnation — DynType). where с incarnation.state.x должен
// вернуть eval-ошибку «no such key», НЕ compile-ошибку.
func TestRenderState_BackwardCompatNoState(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	in := RenderInput{Incarnation: IncarnationMeta{Name: "redis"}} // State == nil
	host := &topology.HostFacts{SID: "redis-0.example.com", Coven: []string{"redis"}}
	vars := hostVars(in, host, 1)

	// has() guard на отсутствующий state — корректный no-such-key путь (false),
	// без State это не compile-error.
	out, err := e.EvalExpression("has(incarnation.state) && size(incarnation.state.redis_users) > 0", vars)
	if err != nil {
		t.Fatalf("backward-compat has(incarnation.state): %v (должно резолвиться без State)", err)
	}
	if b, _ := out.Value().(bool); b {
		t.Fatalf("has(incarnation.state) без State = true, want false")
	}
}

// TestRenderState_StateChangesSeesState — incarnation.state доступен и в
// state_changes-контексте: stateChangesVars зовёт тот же incarnationVars, поэтому
// одна точка правки (Вариант A) даёт incarnation.state и в sets/modify-патчах.
func TestRenderState_StateChangesSeesState(t *testing.T) {
	state := map[string]any{"redis_users": map[string]any{"alice": map[string]any{"acl": "+@read"}}}
	in := RenderInput{
		Incarnation: IncarnationMeta{Name: "redis"},
		State:       state,
		Hosts:       []*topology.HostFacts{{SID: "redis-0.example.com"}},
	}
	vars := stateChangesVars(in, in.Hosts[0])
	if vars.Incarnation["state"] == nil {
		t.Fatalf("state_changes-контекст не видит incarnation.state: %v", vars.Incarnation)
	}
	if !reflect.DeepEqual(vars.Incarnation["state"], state) {
		t.Fatalf("state_changes incarnation.state = %v, want %v", vars.Incarnation["state"], state)
	}
}
