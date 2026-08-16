package cel

import (
	"errors"
	"strings"
	"testing"
)

// keeperScopeVars — the shape of render.keeperVars: a run-level context with a
// resolved compute: block that this context nonetheless cannot read. Compute is
// deliberately NON-empty: the bug being guarded here reported "no such key" for a
// name the run had actually computed.
func keeperScopeVars() Vars {
	return Vars{
		Input:        map[string]any{"cluster": "prod"},
		Compute:      map[string]any{"topology_node_count": 3},
		ComputeScope: ComputeOutOfScopeKeeperTask,
	}
}

// TestComputeScope_NamesTheNamespaceNotTheKey is the ticket's premise as a test:
// the message must say the NAMESPACE is absent in this context, and must not say
// "no such key" — that wording sent the author looking for a typo in a name that
// was spelled correctly and had a value.
func TestComputeScope_NamesTheNamespaceNotTheKey(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalExpression("compute.topology_node_count", keeperScopeVars())
	if err == nil {
		t.Fatal("compute.topology_node_count in an on: keeper task: expected an error, got none")
	}
	if !errors.Is(err, ErrNamespaceOutOfScope) {
		t.Fatalf("expected ErrNamespaceOutOfScope, got %T: %v", err, err)
	}

	msg := err.Error()
	if strings.Contains(msg, "no such key") {
		t.Fatalf("message still blames a key: %s", msg)
	}
	for _, want := range []string{"compute", "namespace", "on: keeper"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message does not mention %q: %s", want, msg)
		}
	}

	var oos *ErrOutOfScope
	if !errors.As(err, &oos) {
		t.Fatalf("expected *ErrOutOfScope, got %T", err)
	}
	if oos.Namespace != "compute" {
		t.Fatalf("Namespace = %q, want %q", oos.Namespace, "compute")
	}
	if oos.Hint == "" {
		t.Fatal("ErrOutOfScope carries no hint — the author is told what is wrong but not what to write instead")
	}
}

// TestComputeScope_AnyNameFailsTheSameWay — the old error varied with the name and
// so read as a statement about the name. The new one must not: two different names
// produce the same diagnosis, because the namespace is what is missing.
func TestComputeScope_AnyNameFailsTheSameWay(t *testing.T) {
	e := newEngine(t)
	for _, expr := range []string{"compute.topology_node_count", "compute.nothing_like_it"} {
		_, err := e.EvalExpression(expr, keeperScopeVars())
		if !errors.Is(err, ErrNamespaceOutOfScope) {
			t.Fatalf("%s: expected ErrNamespaceOutOfScope, got %v", expr, err)
		}
	}
}

// TestComputeScope_SilentFormsNowSpeak — the forms that used to produce no error at
// all. `has(compute.x)` evaluated false and `size(compute)` returned 0 against the
// substituted empty map, so a step gated on either silently did the wrong thing.
// This is the behaviour change the ticket asks for, pinned.
func TestComputeScope_SilentFormsNowSpeak(t *testing.T) {
	e := newEngine(t)
	for _, expr := range []string{
		"has(compute.topology_node_count)",
		"size(compute) > 0",
		"compute['topology_node_count']",
		"false && compute.topology_node_count > 0", // an untaken branch is still refused
	} {
		_, err := e.EvalExpression(expr, keeperScopeVars())
		if !errors.Is(err, ErrNamespaceOutOfScope) {
			t.Fatalf("%s: expected ErrNamespaceOutOfScope, got %v", expr, err)
		}
	}
}

// TestComputeScope_EveryOutOfScopeContextNamesItself — a context that refuses must
// say WHICH context, or the author learns only that they cannot do it.
func TestComputeScope_EveryOutOfScopeContextNamesItself(t *testing.T) {
	e := newEngine(t)
	cases := map[ComputeScope]string{
		ComputeOutOfScopeKeeperTask: "on: keeper",
		ComputeOutOfScopeLoopAxis:   "loop.items",
		ComputeOutOfScopeCovenList:  "on: [covens]",
		ComputeOutOfScopeDestiny:    "destiny",
	}
	for scope, want := range cases {
		_, err := e.EvalExpression("compute.x", Vars{ComputeScope: scope})
		if !errors.Is(err, ErrNamespaceOutOfScope) {
			t.Fatalf("scope %d: expected ErrNamespaceOutOfScope, got %v", scope, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("scope %d: message does not name the context (%q): %s", scope, want, err.Error())
		}
		ctx, hint := scope.Describe()
		if ctx == "" || hint == "" {
			t.Fatalf("scope %d: Describe() returned an empty half (%q, %q)", scope, ctx, hint)
		}
	}
}

// TestComputeScope_InScopeStillGivesNoSuchKey — the guard must not swallow the
// ordinary mistake. Where the namespace IS available, a name that is not in the
// block is a plain no-such-key, exactly as before.
func TestComputeScope_InScopeStillGivesNoSuchKey(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalExpression("compute.topology_node_count", Vars{
		Compute: map[string]any{"topology_node_count": 3},
	})
	if err != nil {
		t.Fatalf("in-scope read: %v", err)
	}
	if got := out.Value(); got != int64(3) {
		t.Fatalf("in-scope read: got %v, want 3", got)
	}

	_, err = e.EvalExpression("compute.absent", Vars{Compute: map[string]any{"x": 1}})
	if err == nil {
		t.Fatal("absent key in an in-scope context: expected an error")
	}
	if errors.Is(err, ErrNamespaceOutOfScope) {
		t.Fatalf("absent key reported as an absent namespace: %v", err)
	}
	if !strings.Contains(err.Error(), "no such key") {
		t.Fatalf("expected the ordinary no-such-key, got: %v", err)
	}

	// A nil Compute is "this run has no compute: block", not "no namespace here":
	// still an ordinary no-such-key wherever the namespace is in scope.
	_, err = e.EvalExpression("compute.anything", Vars{})
	if errors.Is(err, ErrNamespaceOutOfScope) {
		t.Fatalf("nil Compute in an in-scope context reported as out of scope: %v", err)
	}
	if err == nil {
		t.Fatal("nil Compute: expected the ordinary no-such-key, got no error")
	}
}

// TestComputeScope_CachePoisoning — [Engine.compile] consults its cache BEFORE any
// guard, so the scope has to be part of the cache key. Compile the expression first
// where it is legal, then ask for it where it is not: without the tag the second
// call is served the cached program and the guard never runs.
func TestComputeScope_CachePoisoning(t *testing.T) {
	e := newEngine(t)

	if _, err := e.EvalExpression("compute.topology_node_count", Vars{
		Compute: map[string]any{"topology_node_count": 3},
	}); err != nil {
		t.Fatalf("priming the cache in an in-scope context: %v", err)
	}

	_, err := e.EvalExpression("compute.topology_node_count", keeperScopeVars())
	if !errors.Is(err, ErrNamespaceOutOfScope) {
		t.Fatalf("cached in-scope program served to an out-of-scope context: %v", err)
	}

	// And the reverse order: a refusal must not poison the in-scope path either.
	if _, err := e.EvalExpression("compute.topology_node_count", Vars{
		Compute: map[string]any{"topology_node_count": 7},
	}); err != nil {
		t.Fatalf("in-scope read after an out-of-scope refusal: %v", err)
	}
}

// TestComputeScope_NoFalsePositives — everything that merely LOOKS like the
// namespace. A rule that fired on any of these would push authors to rename
// unrelated things; the AST walk is what keeps them apart from a real reference.
func TestComputeScope_NoFalsePositives(t *testing.T) {
	e := newEngine(t)
	vars := Vars{
		Input:          map[string]any{"computed_at": "yesterday", "compute_kind": "x"},
		Vars:           map[string]any{"compute": "a var that happens to be called that"},
		SoulprintHosts: hostsFixture(),
		AllowHosts:     true,
		ComputeScope:   ComputeOutOfScopeKeeperTask,
	}
	for _, expr := range []string{
		"input.computed_at",
		"input.compute_kind",
		"vars.compute",
		`"compute.x is a namespace"`,
		`soulprint.hosts.where("compute == 4").size()`, // a host field inside a string literal
	} {
		if _, err := e.EvalExpression(expr, vars); errors.Is(err, ErrNamespaceOutOfScope) {
			t.Fatalf("%s: flagged as a compute reference, it is not", expr)
		}
	}
}

// TestComputeScope_Interpolation — params: values are interpolations, and each
// `${ … }` block compiles on its own. The word in the literal text around a block
// is not a reference; the one inside it is.
func TestComputeScope_Interpolation(t *testing.T) {
	e := newEngine(t)

	if _, err := e.EvalInterpolation("nodes=${ compute.topology_node_count }", keeperScopeVars()); !errors.Is(err, ErrNamespaceOutOfScope) {
		t.Fatalf("interpolated reference: expected ErrNamespaceOutOfScope, got %v", err)
	}
	if _, err := e.EvalInterpolation("we compute this later: ${ input.cluster }", keeperScopeVars()); err != nil {
		t.Fatalf("the word in literal text is not a reference: %v", err)
	}
}

// TestComputeScope_LoopVariableShadows — a loop variable named `compute` is bound at
// the top level of the activation and shadows the namespace, so the guard must step
// aside. Unreachable from YAML (shared/config.loopReservedNames forbids the name);
// this keeps a programmatic caller honest.
func TestComputeScope_LoopVariableShadows(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalExpression("compute.n", Vars{
		Loop:         map[string]any{"compute": map[string]any{"n": 42}},
		ComputeScope: ComputeOutOfScopeKeeperTask,
	})
	if err != nil {
		t.Fatalf("shadowed by a loop variable: %v", err)
	}
	if got := out.Value(); got != int64(42) {
		t.Fatalf("got %v, want 42", got)
	}
}

// TestComputeScope_ReferenceHelpersMatchTheGuard — soul-lint's offline rule asks
// these two helpers the question the guard answers, so a disagreement would be a
// linter that flags what render accepts (or stays quiet on what it refuses).
func TestComputeScope_ReferenceHelpersMatchTheGuard(t *testing.T) {
	e := newEngine(t)

	exprs := []string{
		"compute.topology_node_count",
		"has(compute.x)",
		"size(compute)",
		"compute['x']",
		"input.computed_at",
		"vars.compute",
		`soulprint.hosts.where("compute == 4").size()`,
		"input.cluster",
	}
	for _, expr := range exprs {
		_, err := e.EvalExpression(expr, Vars{
			SoulprintHosts: hostsFixture(),
			AllowHosts:     true,
			ComputeScope:   ComputeOutOfScopeKeeperTask,
		})
		guardRefused := errors.Is(err, ErrNamespaceOutOfScope)
		if got := e.ExpressionReferencesCompute(expr); got != guardRefused {
			t.Fatalf("%s: ExpressionReferencesCompute=%v, guard refused=%v", expr, got, guardRefused)
		}
		if got := e.InterpolationReferencesCompute("${ " + expr + " }"); got != guardRefused {
			t.Fatalf("${ %s }: InterpolationReferencesCompute=%v, guard refused=%v", expr, got, guardRefused)
		}
	}

	// Whole-string form vs interpolated form: only the latter reads `${ }` blocks.
	if e.ExpressionReferencesCompute("nodes=${ compute.x }") {
		t.Fatal("ExpressionReferencesCompute must read its argument as one CEL expression, not as an interpolation")
	}
	if !e.InterpolationReferencesCompute("nodes=${ compute.x }") {
		t.Fatal("InterpolationReferencesCompute missed a reference inside a block")
	}
	if e.InterpolationReferencesCompute("we compute this later") {
		t.Fatal("the bare word outside a block is not a reference")
	}
}
