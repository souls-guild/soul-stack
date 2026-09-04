package cel

// The RUNTIME half of the `incarnation.name` → `incarnation.id` window
// ([ADR-0085], NIM-730), plus the detector soul-lint's static half is built on.
//
// The two are complementary and both have to hold: if the alias stops being
// placed, every unmigrated service repository breaks the day this lands; if the
// detector stops answering, the only static catcher for the rename goes quiet
// while everything still passes.

import "testing"

func idVars(m map[string]any) Vars { return Vars{Incarnation: m} }

// TestIncarnationRoot_BothSpellingsEvaluate is the window itself, asked through
// the real engine in the environment a scenario is rendered in.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: delete the
// `out[legacyIncarnationIDKey] = id` line from [Vars.incarnationRoot]. Every
// build stays green, every test that writes the new spelling stays green, and
// this one goes red — which is the shape of the outage it prevents, since a
// stale `incarnation.name` fails at EVALUATION and not at compile.
func TestIncarnationRoot_BothSpellingsEvaluate(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vars := idVars(map[string]any{"id": "redis-prod", "service": "redis"})

	for _, expr := range []string{"incarnation.id", "incarnation.name"} {
		got, err := e.EvalExpression(expr, vars)
		if err != nil {
			t.Fatalf("eval %s: %v", expr, err)
		}
		if s, _ := got.Value().(string); s != "redis-prod" {
			t.Errorf("%s = %q, want %q", expr, s, "redis-prod")
		}
	}
}

// TestIncarnationRoot_AliasReachesFlowControl — the flow-control environment is
// evaluated on the HOST, and it is the one where a stale root fails mid-run after
// earlier tasks have applied. The alias has to be there too, which it is because
// the Soul rebuilds Vars from `flow_context` and goes through the same activation.
func TestIncarnationRoot_AliasReachesFlowControl(t *testing.T) {
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	got, err := e.EvalExpression("incarnation.name == 'redis-prod'", idVars(map[string]any{"id": "redis-prod"}))
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if b, _ := got.Value().(bool); !b {
		t.Error("the retired root did not resolve in the flow-control sandbox")
	}
}

// TestIncarnationRoot_NoAliasWithoutAnID — a context that carries no incarnation
// (push, trial) must keep answering `incarnation.name` with the ordinary
// no-such-key. An unconditional alias would answer with an empty string, and a
// predicate comparing against "" would look like it matched something.
func TestIncarnationRoot_NoAliasWithoutAnID(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, m := range []map[string]any{nil, {"service": "redis"}} {
		if _, err := e.EvalExpression("incarnation.name", idVars(m)); err == nil {
			t.Errorf("incarnation.name resolved with no id in the context (%v)", m)
		}
	}
}

// TestIncarnationRoot_CallerMapIsNotMutated — the activation is built from the
// caller's live map and must not write into it. Rendering one task would
// otherwise leave the alias in a map the next context reuses, which is the class
// of bug registerRoot copies to avoid.
func TestIncarnationRoot_CallerMapIsNotMutated(t *testing.T) {
	m := map[string]any{"id": "redis-prod"}
	_ = idVars(m).activation(false)
	if _, leaked := m["name"]; leaked {
		t.Error("incarnationRoot wrote the alias into the caller's map")
	}
}

// TestIncarnationRoot_ExistingNameKeyIsNotOverwritten — a caller mid-migration
// that already places `name` keeps its own value. The alias fills a gap; it does
// not adjudicate between two spellings a caller supplied deliberately.
func TestIncarnationRoot_ExistingNameKeyIsNotOverwritten(t *testing.T) {
	act := idVars(map[string]any{"id": "redis-prod", "name": "redis-legacy"}).activation(false)
	inc, _ := act["incarnation"].(map[string]any)
	if got := inc["name"]; got != "redis-legacy" {
		t.Errorf("incarnation.name = %v, want the caller's own value", got)
	}
}

// TestReadsLegacyIncarnationID_Detector pins what soul-lint's warning is built
// on: the retired FIELD, in either syntax, and nothing else. The two false
// positives a regex would produce — the word in prose, and inside a CEL string
// literal — are the cases that would teach authors to ignore the warning.
func TestReadsLegacyIncarnationID_Detector(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	expressions := map[string]bool{
		"incarnation.name":        true,
		"incarnation.name == 'x'": true,
		"incarnation['name']":     true,
		"has(incarnation.name)":   true,
		"incarnation.id":          false,
		"incarnation.service":     false,
		"size(incarnation)":       false,
		"'incarnation.name'":      false, // a string constant, not a select
		"input.incarnation.name":  false, // a different root entirely
		// A `.where(…)` predicate is the ONE string literal the engine parses
		// rather than passes through: `incarnation` is in predicateContextRoots, so
		// it resolves from the activation instead of being qualified to the
		// iteration variable. Missing this would leave a real read of the retired
		// root invisible to the platform's only static catcher.
		`soulprint.hosts.where("incarnation.name == 'redis-prod'").size() > 0`: true,
		`soulprint.hosts.where("incarnation.id == 'redis-prod'").size() > 0`:   false,
		// Inside the predicate it is a VALUE here, not a select.
		`soulprint.hosts.where("'incarnation.name' in covens").size()`: false,
		// A `.where` argument that is not a literal at all exercises the other arm
		// of predicateSelectsIncarnationField — there is no source to re-parse, so
		// the answer is no rather than a guess.
		"soulprint.hosts.where('incarnation.name' in covens).size()": false,
		// The concatenated form, which the examples actually use: the reference is
		// outside the predicate and the outer walk sees it directly.
		`soulprint.hosts.where("'" + incarnation.name + "' in covens").size()`: true,
	}
	for expr, want := range expressions {
		if got := e.ExpressionReadsLegacyIncarnationID(expr); got != want {
			t.Errorf("ExpressionReadsLegacyIncarnationID(%q) = %v, want %v", expr, got, want)
		}
	}

	interpolations := map[string]bool{
		"${ incarnation.name }":                true,
		"redis-${ incarnation.name }-primary":  true,
		"${ incarnation.id }":                  false,
		"the incarnation.name is retired":      false, // literal text, no block
		"${ 'incarnation.name' }":              false, // a constant inside a block
		"${ incarnation.id }-${ 'not a ref' }": false,
	}
	for raw, want := range interpolations {
		if got := e.InterpolationReadsLegacyIncarnationID(raw); got != want {
			t.Errorf("InterpolationReadsLegacyIncarnationID(%q) = %v, want %v", raw, got, want)
		}
	}
}
