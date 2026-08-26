package stateop

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

func task(t *testing.T, name, module string, params map[string]any) *render.RenderedTask {
	t.Helper()
	p, err := structpb.NewStruct(params)
	if err != nil {
		t.Fatalf("params %v: %v", params, err)
	}
	return &render.RenderedTask{Name: name, Module: module, Params: p}
}

// TestOpsFromPlan_CaptureStepsOnlyInPlanOrder — the fold is the whole point: a
// plan is mostly host work, and the capture steps standing between it are the
// only ones that touch the state. Order is plan order because the merge is
// last-wins, so a fold that reordered would report a different final state than
// the run.
func TestOpsFromPlan_CaptureStepsOnlyInPlanOrder(t *testing.T) {
	ops, err := OpsFromPlan([]*render.RenderedTask{
		task(t, "install", "core.pkg.present", map[string]any{"name": "redis"}),
		task(t, "record the owner", "core.state.set", map[string]any{"field": "owner", "value": "alice"}),
		task(t, "restart", "core.service.restarted", map[string]any{"name": "redis"}),
		task(t, "overwrite the owner", "core.state.set", map[string]any{"field": "owner", "value": "bob"}),
		task(t, "drop the flag", "core.state.unset", map[string]any{"field": "migrating"}),
	})
	if err != nil {
		t.Fatalf("OpsFromPlan: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("ops = %d, want 3 (the host tasks are not captures)", len(ops))
	}
	if ops[0].Verb != config.VerbSet || ops[0].Field != "owner" || ops[0].Value != "alice" {
		t.Errorf("ops[0] = %+v, want set owner=alice", ops[0])
	}
	if ops[1].Value != "bob" {
		t.Errorf("ops[1].Value = %v, want bob: plan order decides which write is last", ops[1].Value)
	}
	if ops[2].Verb != config.VerbUnset || ops[2].Field != "migrating" {
		t.Errorf("ops[2] = %+v, want unset migrating", ops[2])
	}
}

// TestOpsFromPlan_SkipPlaceholderIsNotAnOp — a task the render skipped (static
// when:false, block/loop skip, future-Passage stub) reaches the fold as a
// placeholder: the module address of the task it stands for, and no params. It
// wrote nothing, so it is not an op — and selecting on the address alone would not
// merely add a phantom write, it would hand [CheckParams] an empty map and fail the
// whole plan on the params of a task that never ran.
func TestOpsFromPlan_SkipPlaceholderIsNotAnOp(t *testing.T) {
	ops, err := OpsFromPlan([]*render.RenderedTask{
		{Name: "record the owner", Module: "core.state.set"}, // Params nil — the skip marker
		task(t, "record the last user", "core.state.set", map[string]any{"field": "last_user", "value": "bob"}),
	})
	if err != nil {
		t.Fatalf("OpsFromPlan: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("ops = %d, want 1 (the placeholder is not a write)", len(ops))
	}
	if ops[0].Field != "last_user" {
		t.Errorf("ops[0].Field = %q, want last_user", ops[0].Field)
	}
}

// TestOpsFromPlan_UnsetTakesNoValue — `unset` drops the key, `set` with an empty
// string blanks it, and the two are different states. The distinction is held one
// level down, by [CheckParams] refusing `value:` on a verb that does not take one:
// were it merely ignored, an author who wrote both would get the drop and read the
// task as the blanking.
func TestOpsFromPlan_UnsetTakesNoValue(t *testing.T) {
	_, err := OpsFromPlan([]*render.RenderedTask{
		task(t, "drop with a value", "core.state.unset", map[string]any{"field": "migrating", "value": ""}),
	})
	if err == nil {
		t.Fatal("expected an error: unset takes no `value`, and ignoring it would read as a blanking")
	}
	if !strings.Contains(err.Error(), ParamValue) {
		t.Errorf("the error must name `value`, got: %v", err)
	}
}

// TestOpsFromPlan_UnknownStateIsAnError — an unrecognised suffix is NOT a task of
// another module, it is a `core.state` step nobody can execute. Skipping it would
// predict a state the run never reaches, and the case would go green on a plan
// the keeper aborts.
func TestOpsFromPlan_UnknownStateIsAnError(t *testing.T) {
	_, err := OpsFromPlan([]*render.RenderedTask{
		task(t, "typo", "core.state.sett", map[string]any{"field": "owner", "value": "alice"}),
	})
	if err == nil {
		t.Fatal("expected an error: `sett` is not a state of core.state")
	}
	if !strings.Contains(err.Error(), "typo") || !strings.Contains(err.Error(), KnownStates()) {
		t.Errorf("the error must name the task AND list the known states, got: %v", err)
	}
}

// TestOpsFromPlan_RejectsParamsTheRunWouldRefuse is why the fold is shared rather
// than copied. `core.state.set` given a `match:` reads as a filtered write and is
// a wholesale overwrite; [CheckParams] refuses it at dispatch. A caller that
// predicts the run's state with its own loop and skips that check reports a state
// for a plan that never runs.
func TestOpsFromPlan_RejectsParamsTheRunWouldRefuse(t *testing.T) {
	_, err := OpsFromPlan([]*render.RenderedTask{
		task(t, "narrow the write", "core.state.set", map[string]any{
			"field": "hosts", "value": "alice", "match": "element.sid == 'a'",
		}),
	})
	if err == nil {
		t.Fatal("expected an error: `match` is not a param of set, and the run refuses it")
	}
	if !strings.Contains(err.Error(), ParamMatch) {
		t.Errorf("the error must name the offending param, got: %v", err)
	}

	// The same shape without the stray param passes — the guard is the param, not
	// the verb.
	if _, err := OpsFromPlan([]*render.RenderedTask{
		task(t, "write", "core.state.set", map[string]any{"field": "hosts", "value": "alice"}),
	}); err != nil {
		t.Fatalf("set without match must build: %v", err)
	}
}

// TestOpsFromPlan_MissingRequiredParam — `field:` is missing rather than empty,
// which is the shape a bad interpolation leaves behind. It has to fail here, not
// silently build an op writing the "" field.
func TestOpsFromPlan_MissingRequiredParam(t *testing.T) {
	_, err := OpsFromPlan([]*render.RenderedTask{
		task(t, "no field", "core.state.set", map[string]any{"value": "alice"}),
	})
	if err == nil {
		t.Fatal("expected an error: set without `field` has nothing to write to")
	}
	if !strings.Contains(err.Error(), ParamField) {
		t.Errorf("the error must name `field`, got: %v", err)
	}
}

// TestOpsFromPlan_NoCaptureStepsIsNotAnError — a scenario that writes no state is
// ordinary ([ADR-0084] retired the `state_changes: []` that used to say so), and
// an empty fold must read as "nothing to apply", never as a failure.
func TestOpsFromPlan_NoCaptureStepsIsNotAnError(t *testing.T) {
	ops, err := OpsFromPlan([]*render.RenderedTask{
		task(t, "install", "core.pkg.present", map[string]any{"name": "redis"}),
	})
	if err != nil {
		t.Fatalf("OpsFromPlan: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// TestOpsFromPlan_AddRefusesBothIdentities — `key:` and `match:` are the two
// spellings of element identity, one per collection kind, and the manifest calls
// them mutually exclusive. Both on one task means the engine honours one and drops
// the other, decided by the field's kind rather than by the author — so the pair is
// refused where every caller passes, [BuildOp], not by each caller in turn.
func TestOpsFromPlan_AddRefusesBothIdentities(t *testing.T) {
	_, err := OpsFromPlan([]*render.RenderedTask{
		task(t, "add the user", "core.state.add", map[string]any{
			"field": "users", "value": "alice", "key": "name", "match": "elem.name == value.name",
		}),
	})
	if err == nil {
		t.Fatal("* add with both key: and match: should fail")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error does not say why: %v", err)
	}
}
