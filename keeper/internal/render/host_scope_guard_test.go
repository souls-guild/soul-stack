package render

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// host-scope guards (NIM-908), the direct-reference half. `soulprint.self` in a
// context that renders once per run used to come back as `no such key: sid` — the
// contexts omit cel.Vars.SoulprintSelf and the activation substitutes an empty map
// for it, so the message named a field that is spelled correctly and exists
// everywhere else. It would have failed identically on ANY field.
//
// The indirect half — an `input.<name>` the destiny renders per host — is in
// perhost_apply_input_test.go. Both come out of the same guard, and that is
// deliberate: they are one mistake reached by two routes.

// hostScopeStance — the expected [cel.HostScope] of every `cel.Vars` composite
// literal in this package, keyed by the function that builds it. The table IS the
// guard, exactly as computeScopeStance is for NIM-619: the AST test below fails when
// the package grows a literal that is not listed, so adding a context forces an
// answer to "does this stand on a host".
//
// `want` is the SOURCE TEXT, not a "was it set" flag. Presence alone would let a
// builder be flipped from host-free to host-bound and still pass, and that is a real
// mutation: HostBound on loopInvariantVars restores the silence NIM-908 was filed
// about, and nothing behavioural notices — the loop axis carries no soulprint map
// either way, so the eval error stays "no such key".
var hostScopeStance = map[string]struct {
	want   string
	reason string
}{
	"hostVars":          {want: "cel.HostBound"},          // the per-host context itself
	"resolveCompute":    {want: "cel.HostFreeCompute"},    // OUT: host-invariance is what the namespace rests on
	"keeperVars":        {want: "cel.HostFreeKeeper"},     // OUT: the keeper is not a host
	"resolveCovenList":  {want: "cel.HostFreeCovenList"},  // OUT: on: chooses the roster, so it cannot read it
	"loopInvariantVars": {want: "cel.HostFreeLoopAxis"},   // OUT: the loop expands into tasks every host runs
	"StateOpEvaluators": {want: "cel.HostFreeStateMatch"}, // OUT: merge sees one element, no run context

	"flowControlVarsFromStruct": {
		want: "cel.HostBound",
		reason: "SoulprintSelf in that literal IS a real host's facts — it is the shape Soul binds on the receiving host, and Keeper " +
			"reuses it for a static when: only to fail bit-for-bit the way Soul would (ADR-012(d)). The keeper-side hazard is that the " +
			"DECISION is taken once for the whole roster, which is refused at the decision site (staticWhenSkips / evalIncludeWhen), " +
			"not by this literal",
	},
	"resolveTaskVars": {
		reason: "the zero Vars is returned next to a non-nil error and is never evaluated; on the success path the caller's base carries the stance",
	},
}

// TestHostScope_EveryVarsBuilderDeclaresItsStance walks this package's own source
// and checks every `cel.Vars{…}` literal against the table above.
//
// Fail-open is the hazard the design carries: the zero HostScope means "a host is
// bound", chosen for the same reason ComputeScope's zero means "available". The cost
// is that a NEW context would inherit host-boundness in silence, which is how
// apply.input inherited `targeted[0]` for as long as it did. This test is the
// compensation.
func TestHostScope_EveryVarsBuilderDeclaresItsStance(t *testing.T) {
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
				want, known := hostScopeStance[fn.Name.Name]
				if !known {
					t.Errorf("%s: %s builds a cel.Vars and is not in hostScopeStance — decide whether that "+
						"context stands on a host, say so (shared/cel.HostScope), then list it here",
						fset.Position(lit.Pos()), fn.Name.Name)
					return true
				}
				seen[fn.Name.Name] = true
				if got := litFieldValue(fset, lit, "HostScope"); got != want.want {
					t.Errorf("%s: %s sets HostScope=%q, the table says %q (%s)",
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
	for _, name := range sortedKeys(hostScopeStance) {
		if !seen[name] {
			t.Errorf("hostScopeStance lists %q, but the package has no cel.Vars literal there — "+
				"stale entry; a stale allowlist hides the next omission", name)
		}
	}
}

func hostScopeRenderInput(task config.Task) RenderInput {
	return RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "create", Tasks: []config.Task{task}},
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{ID: "svc", Service: "svc", ServiceVersion: "v1.0.0"},
		Hosts: []*topology.HostFacts{
			hostWithRole("h1.example.com", "master", []string{"web"},
				map[string]any{"primary_ip": "10.0.0.1"}, map[string]any{"family": "debian"}),
			hostWithRole("h2.example.com", "replica", []string{"web"},
				map[string]any{"primary_ip": "10.0.0.2"}, map[string]any{"family": "debian"}),
		},
	}
}

// TestHostScope_SoulprintSelfNamesTheContext — the four host-free contexts reached
// through Render. Each must say WHICH context has no host, rather than blaming a
// field name.
func TestHostScope_SoulprintSelfNamesTheContext(t *testing.T) {
	cases := []struct {
		name    string
		task    config.Task
		context string
	}{
		{
			name: "loop.items",
			task: config.Task{
				Name:   "fan",
				Loop:   &config.LoopSpec{Items: "${ [soulprint.self.sid] }", As: "item"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ item }"}},
			},
			context: "loop.items:/loop.when:",
		},
		{
			name: "on covens",
			task: config.Task{
				Name:   "roll",
				On:     []any{"web-${ soulprint.self.os.family }"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			context: "on: [covens]",
		},
		{
			name: "keeper task",
			task: config.Task{
				Name:   "seed",
				On:     config.KeeperTarget,
				Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{"sid": "${ soulprint.self.sid }"}},
			},
			context: "an on: keeper task",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), hostScopeRenderInput(tc.task))
			if !errors.Is(err, cel.ErrNoHostBound) {
				t.Fatalf("expected ErrNoHostBound, got: %v", err)
			}
			if !strings.Contains(err.Error(), tc.context) {
				t.Errorf("the message does not name the %s context: %v", tc.context, err)
			}
			if strings.Contains(err.Error(), "no such key") {
				t.Errorf("the message blames a key that is spelled correctly: %v", err)
			}
		})
	}
}

// The compute: block is the fifth, and it does not go through a task — the block is
// resolved once at the start of Render.
func TestHostScope_SoulprintSelfInComputeNamesTheContext(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostScopeRenderInput(config.Task{
		Name:   "use",
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
	})
	in.Scenario.Compute = config.ComputeBlock{{Name: "me", Value: "${ soulprint.self.sid }"}}

	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, cel.ErrNoHostBound) {
		t.Fatalf("expected ErrNoHostBound, got: %v", err)
	}
	if !strings.Contains(err.Error(), "the compute: block") {
		t.Errorf("the message does not name the compute: context: %v", err)
	}
}

// ★ The counterweight, and the one this guard could most easily get wrong:
// `soulprint.hosts` is the run's ROSTER — the same list on every host — and is a
// documented, legitimate loop source (orchestration.md §4.1). A guard that keyed on
// the `soulprint` identifier rather than on the `self` selection would refuse it,
// taking away the one way a host-free context reaches host facts at all.
func TestHostScope_SoulprintHostsStillDrivesTheLoop(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostScopeRenderInput(config.Task{
		Name:   "fan",
		Loop:   &config.LoopSpec{Items: `${ soulprint.hosts.where("role == 'replica'") }`, As: "h"},
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ h.sid }"}},
	})

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("soulprint.hosts on the loop axis must still resolve: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected one iteration (one replica), got %d", len(tasks))
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo h2.example.com" {
		t.Fatalf("params.cmd = %q, want %q", got, "echo h2.example.com")
	}
}

// ★ A per-host root hidden inside a `.where("…")` argument. The predicate is a
// string LITERAL until [cel.Engine.rewriteHostsWhere] inlines it, so a guard reading
// the author's text sees `soulprint.hosts` and nothing else, while at eval the
// inlined predicate reads THIS context's `soulprint.self`. Running the guard on the
// rewritten text is what closes it (the NIM-909 lesson, applied here).
func TestHostScope_SoulprintSelfHiddenInsideWherePredicate(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostScopeRenderInput(config.Task{
		Name:   "fan",
		Loop:   &config.LoopSpec{Items: `${ soulprint.hosts.where("sid != soulprint.self.sid") }`, As: "h"},
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ h.sid }"}},
	})

	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, cel.ErrNoHostBound) {
		t.Fatalf("expected ErrNoHostBound for soulprint.self inlined from a .where predicate, got: %v", err)
	}
}

// And the whole point of the stance: a per-host context is untouched. `soulprint.self`
// in a task's params is what the language is for.
func TestHostScope_PerHostContextStillReadsSelf(t *testing.T) {
	const tmplPath = "templates/host.conf.tmpl"
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostScopeRenderInput(config.Task{
		Name: "write",
		Module: &config.ModuleTask{
			Module: moduleFileRendered,
			Params: map[string]any{"path": "/etc/host.conf", "template": tmplPath},
		},
	})
	in.Templates = fakeReader{files: map[string][]byte{tmplPath: []byte("ip {{ .self.network.primary_ip }}\n")}}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("soulprint.self in a per-host context: %v", err)
	}
	rc, ok := tasks[0].RenderContextBySID["h2.example.com"]
	if !ok {
		t.Fatal("no per-host render_context for h2")
	}
	got := rc.AsMap()["self"].(map[string]any)["network"].(map[string]any)["primary_ip"]
	if got != "10.0.0.2" {
		t.Fatalf("h2 render_context.self.network.primary_ip = %v, want 10.0.0.2", got)
	}
}
