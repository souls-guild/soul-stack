package render

import (
	"context"
	"strings"
	"testing"
)

// TestStateOpEvaluators_BindingsOnly — ★ the fence [ADR-0084] put around a
// merge-time predicate: it sees the element bindings and NOTHING else. A capture
// step's `match:`/`patch:` is an ordinary module param, so anything it needs from
// `input.*`/`register.*` was already interpolated render-side and arrives as a
// literal; leaving the run context reachable here would give one expression two
// resolution points that can disagree — the old block's failure mode.
//
// Mutation: add `Input: …` to the cel.Vars built in StateOpEvaluators and the
// negative half below stops erroring.
func TestStateOpEvaluators_BindingsOnly(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, opEval := p.StateOpEvaluators(context.Background(), "")

	res, err := opEval("key == 'alice'", map[string]any{"key": "alice"}, true)
	if err != nil {
		t.Fatalf("opEval: %v", err)
	}
	if res != true {
		t.Errorf("match (key == 'alice', key=alice) = %v, want true", res)
	}

	res2, err := opEval("key == 'alice'", map[string]any{"key": "bob"}, true)
	if err != nil {
		t.Fatalf("opEval: %v", err)
	}
	if res2 != false {
		t.Errorf("match (key=bob) = %v, want false", res2)
	}

	// patch value (boolOut=false) — interpolation, native type.
	val, err := opEval("${ value.acl }", map[string]any{"value": map[string]any{"acl": "+@all"}}, false)
	if err != nil {
		t.Fatalf("opEval patch: %v", err)
	}
	if val != "+@all" {
		t.Errorf("patch value = %v, want +@all", val)
	}

	// ★ The negative half: the run context is not reachable from here.
	if _, err := opEval("key == input.username", map[string]any{"key": "alice"}, true); err == nil {
		t.Fatal("a merge-time predicate reached input.* — the run context must not be in scope")
	} else if !strings.Contains(err.Error(), "input") {
		t.Errorf("error = %v, want it to name the unresolved `input`", err)
	}
}
