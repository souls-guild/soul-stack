package render

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// TestIncarnationVars_StateProjected proves RenderInput.State projects into
// CEL as `incarnation.state` (Variant A, ADR-009/010). incarnationVars sets the
// `state` key from in.State, so scenario-render sees a read-only snapshot of
// incarnation.state.
func TestIncarnationVars_StateProjected(t *testing.T) {
	state := map[string]any{"redis_users": map[string]any{"alice": map[string]any{"acl": "+@all"}}}
	in := RenderInput{
		Incarnation: IncarnationMeta{Name: "redis", Service: "redis-cluster"},
		State:       state,
	}
	got := incarnationVars(in, 3)
	if got["state"] == nil {
		t.Fatalf("incarnation.state is not projected: %v", got)
	}
	if !reflect.DeepEqual(got["state"], state) {
		t.Fatalf("incarnation.state = %v, want %v", got["state"], state)
	}
}

// TestIncarnationVars_NilStateNoKey proves a nil State sets no `state` key
// (backward-compat: push/trial without State see incarnation.state.x as
// no-such-key, not a compile error — incarnation is DynType).
func TestIncarnationVars_NilStateNoKey(t *testing.T) {
	in := RenderInput{Incarnation: IncarnationMeta{Name: "x"}}
	got := incarnationVars(in, 1)
	if _, ok := got["state"]; ok {
		t.Fatalf("nil-State must not set the state key, got: %v", got)
	}
}

// TestRenderState_ReadOnly proves the ★ read-only invariant: rendering
// params/where that read incarnation.state.* does NOT mutate
// RenderInput.State (CEL reads, never writes — there's no mutation path for
// state via CEL). The snapshot before and after eval is identical.
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

	// where reads incarnation.state.count.
	if _, err := evalWhere(e, "incarnation.state.count > 0", vars); err != nil {
		t.Fatalf("evalWhere incarnation.state: %v", err)
	}
	// params interpolate incarnation.state.redis_users (current-for-diff).
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
		t.Fatalf("★ incarnation.state was mutated by eval: got %v, want %v", state, want)
	}
}

// TestRenderState_StagedSnapshotInvariant proves the ★ staged-snapshot
// invariant: on a staged run (renderIn reused across P0 and P1+),
// incarnation.state.* is identical in both passages — it's the pre-run
// stateBefore, NOT an intermediate state_changes result. Simulates a staged
// render: same RenderInput.State, different ActivePassage; eval of
// incarnation.state.* must give the same result both times.
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
		t.Fatalf("★ incarnation.state diverged between passages: P0=%v P1=%v (the snapshot must be invariant)", p0, p1)
	}
	// Snapshot == the original pre-run state (no accumulation across passages).
	want := map[string]any{"alice": map[string]any{"acl": "+@read"}}
	if !reflect.DeepEqual(p0, want) {
		t.Fatalf("incarnation.state.redis_users = %v, want pre-run %v", p0, want)
	}
}

// TestRenderState_BackwardCompatNoState proves backward-compat: without
// RenderInput.State (nil), accessing incarnation.state.x doesn't fail with a
// compile error, but yields the normal no-such-key (incarnation is DynType). A
// where clause with incarnation.state.x must return an eval error "no such
// key", NOT a compile error.
func TestRenderState_BackwardCompatNoState(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	in := RenderInput{Incarnation: IncarnationMeta{Name: "redis"}} // State == nil
	host := &topology.HostFacts{SID: "redis-0.example.com", Coven: []string{"redis"}}
	vars := hostVars(in, host, 1)

	// has() guard on a missing state — the correct no-such-key path (false),
	// not a compile error, even without State.
	out, err := e.EvalExpression("has(incarnation.state) && size(incarnation.state.redis_users) > 0", vars)
	if err != nil {
		t.Fatalf("backward-compat has(incarnation.state): %v (must resolve without State)", err)
	}
	if b, _ := out.Value().(bool); b {
		t.Fatalf("has(incarnation.state) without State = true, want false")
	}
}

// TestRenderState_StateChangesSeesState proves incarnation.state is also
// available in the state_changes context: stateChangesVars calls the same
// incarnationVars, so a single edit point (Variant A) gives incarnation.state
// in sets/modify patches too.
func TestRenderState_StateChangesSeesState(t *testing.T) {
	state := map[string]any{"redis_users": map[string]any{"alice": map[string]any{"acl": "+@read"}}}
	in := RenderInput{
		Incarnation: IncarnationMeta{Name: "redis"},
		State:       state,
		Hosts:       []*topology.HostFacts{{SID: "redis-0.example.com"}},
	}
	vars := stateChangesVars(in, in.Hosts[0])
	if vars.Incarnation["state"] == nil {
		t.Fatalf("state_changes context does not see incarnation.state: %v", vars.Incarnation)
	}
	if !reflect.DeepEqual(vars.Incarnation["state"], state) {
		t.Fatalf("state_changes incarnation.state = %v, want %v", vars.Incarnation["state"], state)
	}
}
