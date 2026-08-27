package render

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// compute-scope guards (NIM-619). The bug was never a wrong value: a context that
// does NOT have the `compute` namespace still handed the evaluator an empty map for
// it, so the failure came back as `no such key: topology_node_count` — a sentence
// about a key that existed, was spelled correctly, and had just been computed. These
// tests pin both halves: the message an author actually receives, and the structural
// reason a future context builder cannot repeat the omission.

// computeScopeStance — the expected stance of every `cel.Vars` composite literal in
// this package, keyed by the function that builds it. The table IS the guard: the
// AST test below fails when the package grows a literal that is not listed here, so
// adding a context forces an answer to the question this ticket was about.
//
// `want` is the SOURCE TEXT of the ComputeScope value, not a "was it set" flag.
// Presence alone would let a builder be flipped from out-of-scope to available and
// still pass, which is a real mutation: hostComputeScope returning ComputeAvailable
// unconditionally deletes the ADR-009 V2 destiny isolation, and the destiny pass
// carries no compute map either way, so the eval error stays "no such key" and
// nothing behavioural notices. An empty `want` means the literal must NOT set the
// field, and owes a reason.
var computeScopeStance = map[string]struct {
	want   string
	reason string
}{
	"hostVars":          {want: "hostComputeScope(in)"},            // in scope per-host; the destiny pass opts out inside that helper
	"resolveCompute":    {want: "cel.ComputeAvailable"},            // in scope: the block resolves inside its own namespace
	"keeperVars":        {want: "cel.ComputeAvailable"},            // IN: on: keeper IS the run-level context compute resolved in
	"resolveCovenList":  {want: "cel.ComputeOutOfScopeCovenList"},  // OUT: on: [covens] resolves once per run
	"loopInvariantVars": {want: "cel.ComputeOutOfScopeLoopAxis"},   // OUT: the host-invariant loop axis
	"StateOpEvaluators": {want: "cel.ComputeOutOfScopeStateMatch"}, // OUT: a capture's match:/patch: sees one element and nothing else

	"flowControlVarsFromStruct": {
		want: "cel.ComputeAvailable",
		reason: "the flow-control env does not declare `compute` at all, so cel-go's own undeclared-reference error already names the namespace. " +
			"cel.ComputeOutOfScopeFlowControl exists for that context but is deliberately NOT set here: Soul builds its own Vars for the same " +
			"predicate, and ADR-012(d) wants keeper-side static-when to fail bit-for-bit the way Soul fails. soul-lint uses the stance offline, " +
			"where there is no env to do the refusing",
	},
	"resolveTaskVars": {
		reason: "the zero Vars is returned next to a non-nil error and is never evaluated; on the success path the caller's base carries the stance",
	},
}

// TestComputeScope_EveryVarsBuilderDeclaresItsStance walks this package's own source
// and checks every `cel.Vars{…}` literal against the table above.
//
// Fail-open is the hazard the design carries: the zero ComputeScope means
// "available", chosen so the restricted engines keep reporting their own honest
// undeclared-reference errors and ad-hoc Vars outside this package keep working. The
// cost is that a NEW keeper-side context would inherit "available" in silence —
// exactly how keeperVars inherited an empty compute map for as long as it did. This
// test is the compensation: silence here is a failure, not a latent bug.
func TestComputeScope_EveryVarsBuilderDeclaresItsStance(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	literals := 0

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, isFn := n.(*ast.FuncDecl)
			if !isFn || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				lit, isLit := inner.(*ast.CompositeLit)
				if !isLit || !isCelVarsLit(lit.Type) {
					return true
				}
				literals++
				want, known := computeScopeStance[fn.Name.Name]
				if !known {
					t.Errorf("%s: %s builds a cel.Vars and is not in computeScopeStance — decide whether "+
						"the compute namespace exists in that context, say so (shared/cel.ComputeScope), "+
						"then list it here",
						fset.Position(lit.Pos()), fn.Name.Name)
					return true
				}
				seen[fn.Name.Name] = true
				if got := celVarsLitValue(fset, lit, "ComputeScope"); got != want.want {
					t.Errorf("%s: %s sets ComputeScope=%q, the table says %q (%s)",
						fset.Position(lit.Pos()), fn.Name.Name, got, want.want, want.reason)
				}
				return true
			})
			return false
		})
	}

	if literals == 0 {
		t.Fatal("found no cel.Vars literals at all — the walk is broken, and a broken walk gates nothing")
	}
	for _, name := range sortedKeys(computeScopeStance) {
		if !seen[name] {
			t.Errorf("computeScopeStance lists %q, but the package has no cel.Vars literal there — "+
				"stale entry; a stale allowlist hides the next omission", name)
		}
	}
}

func isCelVarsLit(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Vars" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "cel"
}

// celVarsLitValue returns the source text of a field's value in a composite
// literal, or "" when the literal does not set the field.
func celVarsLitValue(fset *token.FileSet, lit *ast.CompositeLit, field string) string {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != field {
			continue
		}
		var buf strings.Builder
		if err := printer.Fprint(&buf, fset, kv.Value); err != nil {
			return "<unprintable>"
		}
		return buf.String()
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// computeScopeScenario — a scenario whose compute: block DOES define the name the
// task reads. That is the whole point: under the old behaviour this reported
// `no such key: topology_node_count` for a name the run had just computed.
func computeScopeScenario(task config.Task) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "create",
		Compute: config.ComputeBlock{
			{Name: "topology_node_count", Value: "${ 1 + 2 }"},
		},
		Tasks: []config.Task{task},
	}
}

func computeScopeInput(manifest *config.ScenarioManifest) RenderInput {
	return RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "svc", Service: "svc", ServiceVersion: "v1.0.0"},
		Hosts: []*topology.HostFacts{
			host("h1.example.com", []string{"web"}, nil),
			host("h2.example.com", []string{"web"}, nil),
		},
	}
}

// TestComputeScope_KeeperTaskReadsCompute is the ticket's own reproduction, end to
// end through Render, with the answer this ticket settled on: the task renders and
// SEES the value. `on: keeper` renders in the run-level context — the very one
// [Pipeline.resolveCompute] resolves the block in, before the task loop — so there
// was never anything per-host to import, only a namespace the builder left out.
//
// NON-VACUITY (the mutation this test exists to catch): delete `Compute: in.Compute`
// from render.keeperVars. The activation substitutes an empty map for the absent
// namespace, so this goes red at EVAL with `no such key: topology_node_count` — the
// exact sentence the ticket was filed about — and not at compile.
func TestComputeScope_KeeperTaskReadsCompute(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := computeScopeInput(computeScopeScenario(config.Task{
		Name: "seed",
		On:   config.KeeperTarget,
		Module: &config.ModuleTask{
			Module: "core.soul.registered",
			Params: map[string]any{
				"count": "${ compute.topology_node_count }",
				"sid":   "node-${ compute.topology_node_count }.example.com",
			},
		},
	}))

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("compute.* in an on: keeper task: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("no rendered tasks")
	}
	fields := tasks[0].Params.GetFields()
	// A whole cell that is one `${ … }` keeps its native type (ADR-010).
	if got := fields["count"].GetNumberValue(); got != 3 {
		t.Fatalf("params.count = %v, want 3 -- the keeper context did not see the computed value", got)
	}
	if got := fields["sid"].GetStringValue(); got != "node-3.example.com" {
		t.Fatalf("params.sid = %q, want %q", got, "node-3.example.com")
	}
}

// TestComputeScope_KeeperTaskVarsReadCompute — task-level vars: layer onto that very
// same keeper context (resolveTaskVars takes it as its base), so the value reaches
// params: through them too. Under the old omission this was the "obvious workaround
// that silently isn't"; it is now simply the same context, twice.
func TestComputeScope_KeeperTaskVarsReadCompute(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := computeScopeInput(computeScopeScenario(config.Task{
		Name: "seed",
		Vars: map[string]any{"n": "${ compute.topology_node_count }"},
		On:   config.KeeperTarget,
		Module: &config.ModuleTask{
			Module: "core.soul.registered",
			Params: map[string]any{"count": "${ vars.n }"},
		},
	}))

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("compute.* through a keeper task's vars:: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("no rendered tasks")
	}
	if got := tasks[0].Params.GetFields()["count"].GetNumberValue(); got != 3 {
		t.Fatalf("params.count = %v, want 3", got)
	}
}

// TestComputeScope_KeeperTaskMisspellingSaysNoSuchKey — the counterweight to the two
// above. With the namespace present, a wrong name is an ordinary no-such-key, and
// that message is now TRUE: the namespace is there, this name is not. soul-lint
// catches the same mistake offline (compute_unknown_name).
func TestComputeScope_KeeperTaskMisspellingSaysNoSuchKey(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := computeScopeInput(computeScopeScenario(config.Task{
		Name: "seed",
		On:   config.KeeperTarget,
		Module: &config.ModuleTask{
			Module: "core.soul.registered",
			Params: map[string]any{"count": "${ compute.topology_node_conut }"},
		},
	}))

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("a misspelled compute name in a keeper task: expected an error")
	}
	if errors.Is(err, cel.ErrNamespaceOutOfScope) {
		t.Fatalf("a real typo reported as an absent namespace: %v", err)
	}
	if !strings.Contains(err.Error(), "no such key") {
		t.Fatalf("expected the ordinary no-such-key, got: %v", err)
	}
}

// TestComputeScope_LoopAxisAndCovenList — the other two keeper-side contexts of the
// ticket's table, each reached through Render.
func TestComputeScope_LoopAxisAndCovenList(t *testing.T) {
	cases := []struct {
		name    string
		task    config.Task
		context string
	}{
		{
			name: "loop.items",
			task: config.Task{
				Name:   "fan",
				Loop:   &config.LoopSpec{Items: "${ compute.topology_node_count }", As: "item"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			context: "loop.items",
		},
		{
			name: "loop.when",
			task: config.Task{
				Name: "fan",
				Loop: &config.LoopSpec{
					Items: []any{"a", "b"},
					As:    "item",
					When:  "compute.topology_node_count > 1",
				},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			context: "loop.when",
		},
		{
			name: "on covens",
			task: config.Task{
				Name:   "roll",
				On:     []any{"web-${ compute.topology_node_count }"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			context: "on: [covens]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), computeScopeInput(computeScopeScenario(tc.task)))
			if !errors.Is(err, cel.ErrNamespaceOutOfScope) {
				t.Fatalf("expected ErrNamespaceOutOfScope, got %v", err)
			}
			if strings.Contains(err.Error(), "no such key") {
				t.Fatalf("the message blames a key: %v", err)
			}
			if !strings.Contains(err.Error(), tc.context) {
				t.Fatalf("the message does not name the %s context: %v", tc.context, err)
			}
		})
	}
}

// TestComputeScope_SoulSideStillReads — the guard must cost the contexts that DO
// have the namespace nothing. An ordinary per-host task reads it exactly as before.
func TestComputeScope_SoulSideStillReads(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := computeScopeInput(computeScopeScenario(config.Task{
		Name: "use",
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{"cmd": "echo ${ compute.topology_node_count }"},
		},
	}))

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Soul-side read of compute.*: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("no rendered tasks")
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo 3" {
		t.Fatalf("params.cmd = %q, want %q", got, "echo 3")
	}
}

// TestComputeScope_InScopeMisspellingStillSaysNoSuchKey — the counterweight: where
// the namespace IS available, a wrong name must stay an ordinary no-such-key. A
// guard that swallowed that would trade one misleading message for another.
func TestComputeScope_InScopeMisspellingStillSaysNoSuchKey(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := computeScopeInput(computeScopeScenario(config.Task{
		Name: "use",
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{"cmd": "echo ${ compute.topology_node_conut }"},
		},
	}))

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("a misspelled compute name in a Soul-side task: expected an error")
	}
	if errors.Is(err, cel.ErrNamespaceOutOfScope) {
		t.Fatalf("a real typo reported as an absent namespace: %v", err)
	}
	if !strings.Contains(err.Error(), "no such key") {
		t.Fatalf("expected the ordinary no-such-key, got: %v", err)
	}
}

// TestComputeScope_StateMatchNamesTheNamespace — the fifth context: a capture's
// `match:` is evaluated once per collection element over elem/key/value, so a
// compute reference there has to say so rather than quietly compare against
// nothing. Both closures are checked: modify/remove reach merge through opEval, not
// through the add-identity one.
func TestComputeScope_StateMatchNamesTheNamespace(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	match, opEval := p.StateOpEvaluators(context.Background(), "svc")

	_, err := match("elem.id == compute.topology_node_count",
		map[string]any{"id": 1}, map[string]any{"id": 1})
	if !errors.Is(err, cel.ErrNamespaceOutOfScope) {
		t.Fatalf("add match: expected ErrNamespaceOutOfScope, got %v", err)
	}

	_, err = opEval("elem.id == compute.topology_node_count", map[string]any{"elem": map[string]any{"id": 1}}, true)
	if !errors.Is(err, cel.ErrNamespaceOutOfScope) {
		t.Fatalf("modify/remove match: expected ErrNamespaceOutOfScope, got %v", err)
	}

	// And the context still works on what it does have.
	ok, err := match("elem.id == value.id", map[string]any{"id": 1}, map[string]any{"id": 1})
	if err != nil || !ok {
		t.Fatalf("elem/value match: ok=%v err=%v, want true/nil", ok, err)
	}
}
