package servicevars

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// snapshot is a service snapshot whose files live under `vars/`.
func snapshot(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for rel, body := range files {
		fsys["vars/"+rel] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func view(t *testing.T, fsys fstest.MapFS, inc IncarnationContext) *cel.VarView {
	t.Helper()
	layers, err := Layers(fsys, inc, nil)
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	return cel.NewVarView(nil, layers...)
}

func lookup(t *testing.T, v *cel.VarView, name string) any {
	t.Helper()
	got, found, err := v.Lookup(name)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", name, err)
	}
	if !found {
		t.Fatalf("Lookup(%q): not found", name)
	}
	return got
}

// TestParseStack_SelectorReadingVarsIsRefused — point 14: a step chooses files,
// never values. A selector reading `vars.*` reads a value before the final merge:
// `when: "vars.tls"` evaluated while 00-base says `tls: false` leaves the TLS
// layer out, and a later file saying `tls: true` does not bring it back. Every
// selector, every spelling of the read, refused at parse with the step and the
// selector named.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the
// selectorsReadNoVars call from parseStack. Every case below goes red. Dropping
// `.vars` from selectorRoots turns the leading-dot cases red alone.
func TestParseStack_SelectorReadingVarsIsRefused(t *testing.T) {
	cases := []struct {
		name, stack, want string
	}{
		{"when", "stack:\n  - file: 00-base.yaml\n  - file: 50-tls.yaml\n    when: \"vars.tls\"\n",
			"step 2: service vars: invalid _stack.yaml step: when: reads vars.tls"},
		{"when through has()", "stack:\n  - file: 50-tls.yaml\n    when: \"has(vars.tls)\"\n",
			"step 1: service vars: invalid _stack.yaml step: when: reads vars.tls"},
		{"when by index", "stack:\n  - file: 50-tls.yaml\n    when: \"vars['tls'] == true\"\n",
			"step 1: service vars: invalid _stack.yaml step: when: reads vars"},
		{"foreach", "stack:\n  - foreach: \"${ vars.tiers }\"\n    as: t\n    file: \"${ t }.yaml\"\n",
			"step 1: service vars: invalid _stack.yaml step: foreach: reads vars.tiers"},
		{"file", "stack:\n  - file: 00-base.yaml\n  - file: \"tier-${ vars.tier }.yaml\"\n",
			"step 2: service vars: invalid _stack.yaml step: file: reads vars.tier"},
		{"file whole", "stack:\n  - file: \"${ size(vars) }.yaml\"\n",
			"step 1: service vars: invalid _stack.yaml step: file: reads vars"},
		// A leading dot names the same root: the parser keeps it in the identifier,
		// so a check keyed on `vars` alone read these as no read at all, and the
		// selector then evaluated against an empty map.
		{"when, leading dot", "stack:\n  - file: 50-tls.yaml\n    when: \"!has(.vars.tls)\"\n",
			"step 1: service vars: invalid _stack.yaml step: when: reads vars.tls"},
		{"foreach, leading dot", "stack:\n  - foreach: \"${ .vars.tiers }\"\n    as: t\n    file: \"${ t }.yaml\"\n",
			"step 1: service vars: invalid _stack.yaml step: foreach: reads vars.tiers"},
		{"file, leading dot", "stack:\n  - file: \"tier-${ .vars.tier }.yaml\"\n",
			"step 1: service vars: invalid _stack.yaml step: file: reads vars.tier"},
		{"file whole, leading dot", "stack:\n  - file: \"${ size(.vars) }.yaml\"\n",
			"step 1: service vars: invalid _stack.yaml step: file: reads vars"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseStack([]byte(tc.stack))
			if !errors.Is(err, config.ErrStackStepInvalid) {
				t.Fatalf("want ErrStackStepInvalid, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error must name the step and the selector:\n got %v\nwant …%s…", err, tc.want)
			}
			// The assembly parses through the same check, so the keeper refuses the
			// stack before any selector is evaluated.
			if _, err := Layers(snapshot(map[string]string{"_stack.yaml": tc.stack}), IncarnationContext{}, nil); !errors.Is(err, config.ErrStackStepInvalid) {
				t.Fatalf("Layers: want ErrStackStepInvalid, got %v", err)
			}
		})
	}
}

// TestParseStack_IncarnationSelectorsPass — the other half: the selectors a stack
// is for — over `incarnation.*` and the binding — parse. An unparsable selector
// is not reported as a vars read: it fails at assembly, which names the syntax
// error instead of a read that is not there.
func TestParseStack_IncarnationSelectorsPass(t *testing.T) {
	const ok = `stack:
  - file: 00-base.yaml
  - foreach: "${ incarnation.covens }"
    as: coven
    when: "coven != 'skip' && incarnation.traits.env == 'prd'"
    file: "coven/${ coven }.yaml"
    optional: true
`
	if _, err := ParseStack([]byte(ok)); err != nil {
		t.Fatalf("a stack over incarnation.* must parse: %v", err)
	}

	const broken = "stack:\n  - file: 00-base.yaml\n    when: \"incarnation.covens.(\"\n"
	if _, err := ParseStack([]byte(broken)); err != nil {
		t.Fatalf("an unparsable selector is not a vars read: %v", err)
	}
	_, err := Layers(snapshot(map[string]string{"00-base.yaml": "a: 1\n", "_stack.yaml": broken}), IncarnationContext{}, nil)
	if err == nil || strings.Contains(err.Error(), "reads vars") {
		t.Fatalf("an unparsable selector must fail at assembly with its own error, got %v", err)
	}
}

// TestLayers_ForeachBindingIsNotVisibleInTheStackedFile — the binding chooses the
// file and stops there. The stacked file's contents are definitions, evaluated
// after the merge on request, where no step is in scope: a `${ coven }` in it
// fails when it is read, while its plain values and its reads of `vars.*` work.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: give the stacked layer
// the selector's activation — `layer.Base = sel` in stackLayers — and `who`
// resolves.
func TestLayers_ForeachBindingIsNotVisibleInTheStackedFile(t *testing.T) {
	v := view(t, snapshot(map[string]string{
		"00-base.yaml":    "prefix: svc\n",
		"coven/prod.yaml": "tier: gold\nlabel: \"${ vars.prefix }-gold\"\nwho: \"${ coven }\"\n",
		"_stack.yaml": `stack:
  - file: 00-base.yaml
  - foreach: "${ incarnation.covens }"
    as: coven
    file: "coven/${ coven }.yaml"
`,
	}), IncarnationContext{Covens: []string{"prod"}})

	if got := lookup(t, v, "tier"); got != "gold" {
		t.Fatalf("the binding must resolve in file:, tier = %#v", got)
	}
	if got := lookup(t, v, "label"); got != "svc-gold" {
		t.Fatalf("a stacked file's definition reads vars.*: label = %#v", got)
	}
	_, _, err := v.Lookup("who")
	if err == nil {
		t.Fatal("the foreach binding leaked into the stacked file's contents")
	}
	if !strings.Contains(err.Error(), "coven") || !strings.Contains(err.Error(), "vars.who") {
		t.Fatalf("the error must name the binding and the var: %v", err)
	}
}

// TestLayers_LayerOriginsNameTheirStep — a stacked layer says which step put it
// there, a lexical one which file, so an evaluation error points at the source.
func TestLayers_LayerOriginsNameTheirStep(t *testing.T) {
	stacked, err := Layers(snapshot(map[string]string{
		"00-base.yaml":    "a: 1\n",
		"coven/prod.yaml": "b: 2\n",
		"_stack.yaml":     "stack:\n  - file: 00-base.yaml\n  - foreach: \"${ incarnation.covens }\"\n    as: c\n    file: \"coven/${ c }.yaml\"\n",
	}), IncarnationContext{Covens: []string{"prod"}}, nil)
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	lexical, err := Layers(snapshot(map[string]string{"20-host-layout.yaml": "a: 1\n"}), IncarnationContext{}, nil)
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}

	want := []string{
		"vars/_stack.yaml step 1: vars/00-base.yaml",
		"vars/_stack.yaml step 2: vars/coven/prod.yaml",
		"vars/20-host-layout.yaml",
	}
	var got []string
	for _, l := range append(stacked, lexical...) {
		got = append(got, l.Origin)
		if l.Rung != cel.RungService || l.Engine == nil || len(l.Base.Loop) != 0 {
			t.Errorf("%s: want a service-rung layer with an engine and no loop binding, got %+v", l.Origin, l)
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("origins:\n got %q\nwant %q", got, want)
	}
}

// TestStackLayers_ASelectorReadingVarsFailsAtEvaluation — point 14, the evaluation
// half: a selector is evaluated in an environment without `vars`, so one that got
// past ParseStack still fails, at compile, instead of reading an empty map. A
// `has()` or `default()` over that map would otherwise answer quietly and decide the
// step. The stack is handed to the evaluation directly, as no parse would let it be.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: evaluate the selectors with
// the definitions' engine (`selector, err := serviceVarsEngine()` in newAssembly)
// and every case assembles a layer.
func TestStackLayers_ASelectorReadingVarsFailsAtEvaluation(t *testing.T) {
	fsys := snapshot(map[string]string{"00-base.yaml": "a: 1\n", "tier-x.yaml": "b: 2\n"})
	for name, step := range map[string]config.ServiceVarsStackStep{
		"when":                 {File: "00-base.yaml", When: "!has(vars.tls)"},
		"when, leading dot":    {File: "00-base.yaml", When: "default(.vars.tls, true)"},
		"foreach":              {Foreach: "${ default(vars.tiers, ['x']) }", As: "t", File: "tier-${ t }.yaml"},
		"file":                 {File: "tier-${ has(vars.tier) ? vars.tier : 'x' }.yaml"},
		"file, whole":          {File: "tier-${ size(vars) == 0 ? 'x' : 'y' }.yaml"},
		"foreach, leading dot": {Foreach: "${ has(.vars.tiers) ? [] : ['x'] }", As: "t", File: "tier-${ t }.yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := newAssembly(fsys, IncarnationContext{ID: "inc-1"}, nil)
			if err != nil {
				t.Fatalf("newAssembly: %v", err)
			}
			layers, err := a.stackLayers(&config.ServiceVarsStack{Stack: []config.ServiceVarsStackStep{step}})
			var compileErr *cel.ErrCompile
			undeclared := err != nil && (strings.Contains(err.Error(), "undeclared reference to 'vars'") ||
				strings.Contains(err.Error(), "undeclared reference to '.vars'"))
			if !errors.As(err, &compileErr) || !undeclared {
				t.Fatalf("stackLayers = %d layers, err %v; want an undeclared reference to vars", len(layers), err)
			}
			if !strings.Contains(err.Error(), "step 1") {
				t.Fatalf("the error must name the step: %v", err)
			}
		})
	}
}

// TestStubLayers_SkipsOnlyAPathTheIncarnationNames — the stub's id is made up, so a
// missing file is skipped only where its path depends on the incarnation: a composed
// `file:` that reads `incarnation.*` itself, or the binding of a `foreach:` that reads
// it. A literal `file:` names one path for every incarnation, so the Keeper refuses
// every incarnation that stacks it, whatever its `foreach:`, and so does the stub;
// a composed one over a constant list or reading no binding is one path too.
// EveryLayer lists the literal one, once per step, whatever its `when:` or
// `foreach:`, and not an optional one.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in stackLayers skip a missing
// file when the step's `foreach:` reads `incarnation.*` again
// (`a.readsIncarnation(step.Foreach) ||` in perIncarnationPath, ahead of the literal
// test) — "literal file over the id" goes red; drop the readsRoot test of the
// binding — "composed file reading no binding" goes red; or in EveryLayer list a
// missing file only for an unconditional step (`&& step.When == ""`) — the `when:`
// row of missing goes red.
func TestStubLayers_SkipsOnlyAPathTheIncarnationNames(t *testing.T) {
	inc := IncarnationContext{ID: "stub", Service: "redis", Covens: []string{"prod"}}
	for name, tc := range map[string]struct {
		step    string
		skipped bool
	}{
		"literal file over the id":         {"  - foreach: \"${ [incarnation.id] }\"\n    as: i\n    file: 10-missing.yaml\n", false},
		"literal file over the covens":     {"  - foreach: \"${ incarnation.covens }\"\n    as: c\n    file: 10-missing.yaml\n", false},
		"literal file":                     {"  - file: 10-missing.yaml\n", false},
		"composed file over a constant":    {"  - foreach: \"${ ['missing'] }\"\n    as: f\n    file: \"10-${ f }.yaml\"\n", false},
		"composed file reading no binding": {"  - foreach: \"${ incarnation.covens }\"\n    as: c\n    file: \"10-${ 'missing' }.yaml\"\n", false},
		"composed file over the id":        {"  - file: \"inc/${ incarnation.id }.yaml\"\n", true},
		"composed file over the covens":    {"  - foreach: \"${ incarnation.covens + ['zz'] }\"\n    as: c\n    file: \"coven/${ c }.yaml\"\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			fsys := snapshot(map[string]string{"_stack.yaml": "stack:\n  - file: 00-base.yaml\n" + tc.step, "00-base.yaml": "a: 1\n"})
			layers, err := StubLayers(fsys, inc)
			var missing *StepFileMissingError
			switch {
			case tc.skipped && (err != nil || len(layers) != 1):
				t.Fatalf("want the base alone, got %d layers, %v", len(layers), err)
			case !tc.skipped && (!errors.As(err, &missing) || missing.Step != 2):
				t.Fatalf("want step 2's file refused, got %d layers, %v", len(layers), err)
			}
			if _, err := Layers(fsys, inc, nil); !tc.skipped && !errors.As(err, &missing) {
				t.Fatalf("the Keeper refuses this incarnation too, got %v", err)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		_, missing, err := EveryLayer(snapshot(map[string]string{
			"_stack.yaml": "stack:\n  - file: 00-base.yaml\n" +
				"  - file: 10-gated.yaml\n    when: \"'a' in incarnation.covens\"\n" +
				"  - foreach: \"${ incarnation.covens }\"\n    as: c\n    file: 20-each.yaml\n" +
				"  - foreach: \"${ ['x', 'y'] }\"\n    as: c\n    file: 30-twice.yaml\n" +
				"  - file: 40-optional.yaml\n    optional: true\n" +
				"  - file: \"inc/${ incarnation.id }.yaml\"\n" +
				"  - foreach: \"${ ['missing'] }\"\n    as: f\n    file: \"50-${ f }.yaml\"\n",
			"00-base.yaml": "a: 1\n",
		}))
		if err != nil {
			t.Fatal(err)
		}
		want := []StepFileMissingError{{Step: 2, File: "10-gated.yaml"}, {Step: 3, File: "20-each.yaml"}, {Step: 4, File: "30-twice.yaml"}}
		if !slices.Equal(missing, want) {
			t.Errorf("missing = %v, want %v", missing, want)
		}
	})
}
