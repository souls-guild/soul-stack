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

// TestRenderState_PassageDoesNotAlterProjection proves ActivePassage is not an
// input to the projection: for one RenderInput.State, `incarnation.state.*`
// evaluates identically in P0 and P1. Under [ADR-0084] state DOES accumulate
// across Passages, but that refresh is the runner's move — it re-reads the row
// at the barrier and assigns renderIn.State (scenario/run.go:687-693). Render
// itself stays a pure function of the State it was handed, which is what makes
// the re-read the single place the accumulation can be reasoned about.
func TestRenderState_PassageDoesNotAlterProjection(t *testing.T) {
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
		t.Fatalf("★ incarnation.state diverged between passages: P0=%v P1=%v (only the runner may move it)", p0, p1)
	}
	// The projection is the State it was handed, unchanged.
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

// TestRenderState_CaptureSeesState proves a `core.state.<verb>` capture sees
// incarnation.state like any other task: [ADR-0084] made the capture an ordinary
// task, so its params render through hostVars and there is no second context to
// keep in step (there used to be one, and it was its own edit point).
func TestRenderState_CaptureSeesState(t *testing.T) {
	state := map[string]any{"redis_users": map[string]any{"alice": map[string]any{"acl": "+@read"}}}
	in := RenderInput{
		Incarnation: IncarnationMeta{Name: "redis"},
		State:       state,
		Hosts:       []*topology.HostFacts{{SID: "redis-0.example.com"}},
	}
	vars := hostVars(in, in.Hosts[0], 1)
	if vars.Incarnation["state"] == nil {
		t.Fatalf("a capture does not see incarnation.state: %v", vars.Incarnation)
	}
	if !reflect.DeepEqual(vars.Incarnation["state"], state) {
		t.Fatalf("capture incarnation.state = %v, want %v", vars.Incarnation["state"], state)
	}
}
