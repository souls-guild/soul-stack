package render

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

const failureSecret = "S3CRET-NIM1000"

// renderFailure renders a scenario written as YAML — so its tasks carry the
// positions the parser stamps — and returns the Failure its error carries.
func renderFailure(t *testing.T, scenario string) (*Failure, error) {
	t.Helper()
	return renderFailureWith(t, newEngine(t), scenario)
}

func renderFailureWith(t *testing.T, engine *cel.Engine, scenario string) (*Failure, error) {
	t.Helper()
	scn, _, diags, err := config.LoadScenarioManifestFromBytes("scenario/demo/main.yml", []byte(scenario), config.ValidateOptions{})
	if err != nil || diag.HasErrors(diags) {
		t.Fatalf("load: err=%v diags=%v", err, diags)
	}
	p := NewPipeline(nil, engine, nil, nil)
	_, _, err = p.Render(context.Background(), RenderInput{
		Scenario:    scn,
		Input:       map[string]any{"pw": failureSecret},
		Incarnation: IncarnationMeta{ID: "demo", Service: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", nil, nil)},
		Sealed:      NewSealedSet(),
	})
	if err == nil {
		t.Fatal("Render succeeded, want a render failure")
	}
	f := FailureOf(err)
	if f == nil {
		t.Fatalf("no Failure in %T: %v", err, err)
	}
	return f, err
}

const failureInput = `name: demo
input:
  pw: { type: string, secret: true }
tasks:
`

// The address of a failed cell, and the chain's text left as render always
// printed it (NIM-1000).
func TestFailure_AddressOfAFailedCell(t *testing.T) {
	f, err := renderFailure(t, failureInput+`  - name: reads a missing key
    module: core.exec.run
    vars: { m: { a: 1 } }
    params:
      cmd: echo
      args: ["${ vars.m['nope'] }"]
`)
	want := `render: task "reads a missing key" (host a.example.com): render: cell "args[0]": CEL eval "vars.m['nope']": no such key: nope`
	if err.Error() != want {
		t.Errorf("text = %q\nwant   %q", err.Error(), want)
	}
	if len(f.Tasks) != 1 {
		t.Fatalf("tasks = %+v, want one", f.Tasks)
	}
	got := f.Tasks[0]
	if got.Name != "reads a missing key" || got.Index != 0 || got.Module != "core.exec.run" ||
		got.Source != (config.SourcePos{File: "scenario/demo/main.yml", Line: 5}) {
		t.Errorf("task = %+v", got)
	}
	if f.Host != "a.example.com" || f.Cell != "args[0]" || f.Expr != "vars.m['nope']" || f.Cause != "no such key: nope" || f.Withheld {
		t.Errorf("failure = host %q cell %q expr %q cause %q withheld %v", f.Host, f.Cell, f.Expr, f.Cause, f.Withheld)
	}
}

// A lookup keyed by a secret quotes the secret — directly, and through a task
// var that reads it. Neither the error text nor the Failure may carry it.
func TestFailure_SecretCauseWithheld(t *testing.T) {
	for name, task := range map[string]string{
		"direct": `  - name: lookup
    module: core.exec.run
    vars: { m: { a: 1 } }
    params: { cmd: "${ vars.m[input.pw] }" }
`,
		"through task vars": `  - name: lookup
    module: core.exec.run
    vars: { m: { a: 1 }, k: "${ input.pw }" }
    params: { cmd: "${ vars.m[vars.k] }" }
`,
	} {
		t.Run(name, func(t *testing.T) {
			f, err := renderFailure(t, failureInput+task)
			if strings.Contains(err.Error(), failureSecret) {
				t.Fatalf("the secret reached the error text: %v", err)
			}
			if !f.Withheld || f.Cause != withheldCause {
				t.Errorf("withheld = %v, cause = %q", f.Withheld, f.Cause)
			}
			if strings.Contains(f.Cause+f.Expr+f.Cell, failureSecret) {
				t.Errorf("the secret reached a field: %+v", f)
			}
			if !strings.HasSuffix(err.Error(), withheldCause) || !strings.Contains(err.Error(), `cell "cmd"`) {
				t.Errorf("text = %q, want the chain with the cause withheld", err.Error())
			}
		})
	}
}

// Inside a destiny the secret is the DESTINY's input: the scenario passes it a
// plain-looking value and only the destiny's own schema says it is secret. The
// withholding has to be decided in the destiny pass, with the destiny's sources —
// and the chain still names the applier above the destiny task.
func TestFailure_SecretCauseWithheldInsideADestiny(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name: "lookup",
			Vars: map[string]any{"m": map[string]any{"a": "1"}},
			Module: &config.ModuleTask{
				Module: "core.exec.run",
				Params: map[string]any{"cmd": "${ vars.m[input.pw] }"},
			},
		}},
		config.InputSchemaMap{"pw": {Type: "string", Required: true, Secret: true}},
		map[string]any{"pw": failureSecret},
	)
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := perHostRenderInput(manifest, res, perHostHosts())
	in.Sealed = NewSealedSet()
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render succeeded, want a render failure")
	}
	if strings.Contains(err.Error(), failureSecret) {
		t.Fatalf("the secret reached the error text: %v", err)
	}
	f := FailureOf(err)
	if f == nil || !f.Withheld {
		t.Fatalf("failure = %+v, want the cause withheld", f)
	}
	if len(f.Tasks) != 2 || f.Tasks[1].Name != "lookup" {
		t.Errorf("tasks = %+v, want the applier and then the destiny task", f.Tasks)
	}
	// The applier expands into the destiny's tasks: no index of its own.
	if f.Tasks[0].Index != -1 || f.Tasks[0].Destiny != "redis" || f.Tasks[1].Index != 0 {
		t.Errorf("tasks = %+v, want the applier unindexed and naming its destiny", f.Tasks)
	}
}

// A destiny local that fails over another one's secret: the taint of the file
// layer has to exist before the layer resolves, or the pass that withholds by
// it never knows `vars.a` is secret.
func TestFailure_SecretCauseWithheldInDestinyVars(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{Name: "noop", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}}},
		config.InputSchemaMap{"pw": {Type: "string", Required: true, Secret: true}},
		map[string]any{"pw": failureSecret},
	)
	res.resolved.Vars = map[string]any{"a": "${ input.pw }", "b": "${ {'x': 1}[vars.a] }"}
	// A local is evaluated when a cell reads it (ADR-0082 amendment 2026-10-15).
	res.resolved.Tasks[0].Module.Params["cmd"] = "echo ${ vars.b }"
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := perHostRenderInput(manifest, res, perHostHosts())
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render succeeded, want a render failure")
	}
	if strings.Contains(err.Error(), failureSecret) {
		t.Fatalf("the secret reached the error text: %v", err)
	}
	if f := FailureOf(err); f == nil || !f.Withheld || f.Host == "" {
		t.Fatalf("failure = %+v, want the cause withheld on its host", f)
	}
}

// The applier's own values resolve per host before the destiny pass exists; a
// failure there still names the host it was evaluated for.
func TestFailure_ApplierFailuresNameTheHost(t *testing.T) {
	const fails = "${ {'a': 1}['nope'] }"
	for name, set := range map[string]func(*config.ScenarioManifest){
		"apply.input": func(m *config.ScenarioManifest) { m.Tasks[0].Apply.Input["pw"] = fails },
		"applier vars": func(m *config.ScenarioManifest) {
			// A var is evaluated when apply.input reads it (ADR-0082 amendment
			// 2026-10-15).
			m.Tasks[0].Vars = map[string]any{"k": fails}
			m.Tasks[0].Apply.Input["pw"] = "${ vars.k }"
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, res := perHostApplyInput(
				[]config.Task{{Name: "noop", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}}},
				config.InputSchemaMap{"pw": {Type: "string", Required: true}},
				map[string]any{"pw": "plain"},
			)
			set(manifest)
			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
			f := FailureOf(err)
			if f == nil || f.Host == "" || f.Cause != "no such key: nope" {
				t.Fatalf("failure = %+v (err %v), want it on its host", f, err)
			}
			if name == "applier vars" && (f.Expr != "{'a': 1}['nope']" || f.Cell != "vars.k") {
				t.Errorf("failure = expr %q cell %q, want the definition's own: {'a': 1}['nope'] at vars.k", f.Expr, f.Cell)
			}
		})
	}
}

// A destiny `validate:` rule fails over the destiny's secret input: its error is
// cel-go's own, and the destiny's schema is what says the input is secret.
func TestFailure_SecretCauseWithheldInDestinyValidate(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{Name: "noop", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}}},
		config.InputSchemaMap{"pw": {Type: "string", Required: true, Secret: true}},
		map[string]any{"pw": failureSecret},
	)
	res.resolved.Validate = []config.ValidateRule{{That: "{'a': true}[input.pw]", Message: "pw is known"}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if err == nil {
		t.Fatal("Render succeeded, want a render failure")
	}
	if strings.Contains(err.Error(), failureSecret) {
		t.Fatalf("the secret reached the error text: %v", err)
	}
	// The rule is judged per host (NIM-908), and the failure names the host.
	if f := FailureOf(err); f == nil || !f.Withheld || f.Expr != "{'a': true}[input.pw]" || f.Host == "" {
		t.Fatalf("failure = %+v, want the rule's cause withheld on its host", f)
	}
}

// A failing `assert:` renders into no task of the plan, so it has no index: the
// one it is handed is the next task's.
func TestFailure_AssertHasNoIndex(t *testing.T) {
	f, _ := renderFailure(t, failureInput+`  - name: precondition
    assert:
      that: ["1 == 2"]
      message: never
  - name: next
    module: core.exec.run
    params: { cmd: echo }
`)
	if len(f.Tasks) != 1 || f.Tasks[0].Name != "precondition" || f.Tasks[0].Index != -1 {
		t.Errorf("tasks = %+v, want the assert, unindexed", f.Tasks)
	}
}

// A destiny's own input is not secret because the scenario has a secret input of
// the same name: the destiny's expression reads the destiny's scope only.
func TestFailure_DestinyCauseNotWithheldByTheScenariosNames(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name: "lookup",
			Vars: map[string]any{"m": map[string]any{"a": "1"}},
			Module: &config.ModuleTask{
				Module: "core.exec.run",
				Params: map[string]any{"cmd": "${ vars.m[input.region] }"},
			},
		}},
		config.InputSchemaMap{"region": {Type: "string", Required: true}},
		map[string]any{"region": "eu-west"},
	)
	manifest.Input = config.InputSchemaMap{"region": {Type: "string", Secret: true}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := perHostRenderInput(manifest, res, perHostHosts())
	in.Input = map[string]any{"region": "scenario-secret"}
	_, _, err := p.Render(context.Background(), in)
	f := FailureOf(err)
	if f == nil || f.Withheld || f.Cause != "no such key: eu-west" {
		t.Fatalf("failure = %+v (err %v), want the destiny's plain cause kept", f, err)
	}
}

// A loop over a secret list binds the secret to `item`; a `loop.when` that fails
// on it is decided inside the loop, where that binding is known.
func TestFailure_SecretCauseWithheldInLoopWhen(t *testing.T) {
	f, err := renderFailure(t, failureInput+`  - name: each
    module: core.exec.run
    loop:
      items: "${ [input.pw] }"
      when: "{'a': 1}[item] == 1"
    params: { cmd: echo }
`)
	if strings.Contains(err.Error(), failureSecret) || !f.Withheld {
		t.Fatalf("failure = %+v, err %v; want the cause withheld", f, err)
	}
}

// A `vault()` error names its path, and is kept — unless the path was built from
// a secret, which the path then quotes.
func TestFailure_VaultErrorWithheldWhenItsPathReadsASecret(t *testing.T) {
	// The stored value is the secret, so a path built from it would quote it.
	engine := vaultEngine(t, stubKVRender{secrets: map[string]map[string]any{"secret/app": {"user": failureSecret}}})
	for name, tc := range map[string]struct {
		cell     string
		withheld bool
	}{
		"literal path":            {`${ vault('secret/app#missing') }`, false},
		"field from the secret":   {`${ vault('secret/app#' + input.pw) }`, true},
		"field from vault()":      {`${ vault('secret/app#' + vault('secret/app#user')) }`, true},
		"through a comprehension": {`${ [vault('secret/app#user')].map(x, vault('secret/app#' + x)) }`, true},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := renderFailureWith(t, engine, failureInput+`  - name: read
    module: core.exec.run
    params: { cmd: "`+tc.cell+`" }
`)
			if f.Withheld != tc.withheld {
				t.Errorf("withheld = %v, want %v (err %v)", f.Withheld, tc.withheld, err)
			}
			if strings.Contains(err.Error(), failureSecret) {
				t.Errorf("the secret reached the error text: %v", err)
			}
		})
	}
}

// A `where:` fails while the task's hosts are chosen, before anything of the task
// renders: the failure is still addressed to the task.
func TestFailure_WhereFailureAddressed(t *testing.T) {
	f, _ := renderFailure(t, failureInput+`  - name: chooses hosts
    module: core.exec.run
    vars: { m: { a: 1 } }
    where: "vars.m['nope'] == 1"
    params: { cmd: echo }
`)
	if len(f.Tasks) != 1 || f.Tasks[0].Name != "chooses hosts" || f.Tasks[0].Source.Line != 5 {
		t.Errorf("tasks = %+v, want the task at line 5", f.Tasks)
	}
	if f.Expr != "vars.m['nope'] == 1" || f.Cell != "" || f.Host != "a.example.com" {
		t.Errorf("failure = expr %q cell %q host %q", f.Expr, f.Cell, f.Host)
	}
}

// A loop's failing iteration is addressed by ITS plan index — the one the run's
// task list would have shown — not the loop's first.
func TestFailure_LoopIterationIndex(t *testing.T) {
	f, _ := renderFailure(t, failureInput+`  - name: each
    module: core.exec.run
    vars: { m: { a: "1" } }
    loop: { items: ["a", "nope"] }
    params: { cmd: "${ vars.m[item] }" }
`)
	if len(f.Tasks) != 1 || f.Tasks[0].Name != "each" || f.Tasks[0].Index != 1 {
		t.Errorf("tasks = %+v, want the loop's second iteration, index 1", f.Tasks)
	}
}

// A block's child failing is addressed as the block, then the child, each at
// its own line.
func TestFailure_BlockChildChain(t *testing.T) {
	f, _ := renderFailure(t, failureInput+`  - name: group
    block:
      - name: inner
        module: core.exec.run
        vars: { m: { a: 1 } }
        params: { cmd: "${ vars.m['nope'] }" }
`)
	if len(f.Tasks) != 2 {
		t.Fatalf("tasks = %+v, want the block and its child", f.Tasks)
	}
	if f.Tasks[0].Name != "group" || f.Tasks[0].Source.Line != 5 || f.Tasks[0].Index != -1 {
		t.Errorf("outer = %+v, want group at line 5, unindexed", f.Tasks[0])
	}
	if f.Tasks[1].Name != "inner" || f.Tasks[1].Source.Line != 7 || f.Tasks[1].Index != 0 {
		t.Errorf("inner = %+v, want inner #0 at line 7", f.Tasks[1])
	}
}

// An error raised while a definition is evaluated is the definition's (ADR-0082
// amendment 2026-10-15, point 11): its own expression, `vars.<name>` as where it failed,
// and its own cause — not the cell that asked for it, however many reads away.
func TestFailure_AddressOfAFailedDefinition(t *testing.T) {
	for name, scenario := range map[string]string{
		"task rung": failureInput + `  - name: reads a broken var
    module: core.exec.run
    vars: { k: "${ {'a': 1}['nope'] }", outer: "${ vars.k }-x" }
    params: { cmd: "echo ${ vars.outer }" }
`,
		"scenario rung": `name: demo
vars:
  k: "${ {'a': 1}['nope'] }"
tasks:
  - name: reads a broken var
    module: core.exec.run
    vars: { outer: "${ vars.k }-x" }
    params: { cmd: "echo ${ vars.outer }" }
`,
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := renderFailure(t, scenario)
			if f.Cell != "vars.k" || f.Expr != "{'a': 1}['nope']" || f.Cause != "no such key: nope" || f.Withheld {
				t.Errorf("failure = cell %q expr %q cause %q withheld %v, want vars.k, the definition's expression and its cause",
					f.Cell, f.Expr, f.Cause, f.Withheld)
			}
			if f.Host != "a.example.com" || len(f.Tasks) != 1 || f.Tasks[0].Name != "reads a broken var" {
				t.Errorf("failure = host %q tasks %+v, want the reading task on its host", f.Host, f.Tasks)
			}
		})
	}
}

// ★ GUARD (point 10). Two cells read two broken vars, and the one reported is the same
// on every render: the cells of a map are visited in key order, so which definition
// fails first does not depend on Go's map order — what the eager topological resolve
// gave for free.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: range over the map directly in
// renderValue, or over apply.Input in resolveApplyInput. The matching case goes red.
func TestFailure_TheReportedVarDoesNotDependOnMapOrder(t *testing.T) {
	broken := func() map[string]any { return map[string]any{"a": "${ input.nope1 }", "b": "${ input.nope2 }"} }
	reads := func() map[string]any { return map[string]any{"x": "${ vars.a }", "y": "${ vars.b }"} }
	for name, in := range map[string]func() RenderInput{
		"params": func() RenderInput {
			return RenderInput{Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
				Name: "t", Vars: broken(), Module: &config.ModuleTask{Module: "core.exec.run", Params: reads()},
			}}}}
		},
		"apply: input:": func() RenderInput {
			manifest, res := perHostApplyInput(
				[]config.Task{{Name: "echo", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.x }"}}}},
				config.InputSchemaMap{"x": {Type: "string"}, "y": {Type: "string"}},
				reads(),
			)
			manifest.Tasks[0].Vars = broken()
			return RenderInput{Scenario: manifest, Destiny: res}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var first string
			var firstCell string
			for i := range 50 {
				_, err := renderScenario(t, in())
				f := FailureOf(err)
				if err == nil || f == nil {
					t.Fatalf("render %d: err = %v, want a Failure", i, err)
				}
				if i == 0 {
					first, firstCell = err.Error(), f.Cell
					if firstCell != "vars.a" || !strings.Contains(first, "nope1") {
						t.Fatalf("failure = cell %q, %v; want vars.a, the var the first cell in key order reads", firstCell, err)
					}
					continue
				}
				if err.Error() != first || f.Cell != firstCell {
					t.Fatalf("render %d reported cell %q: %v\nrender 0 reported cell %q: %s", i, f.Cell, err, firstCell, first)
				}
			}
		})
	}
}

// An error inside a tainted definition is withheld however many hops away the asking
// cell is (point 11): CEL quotes a missing key by its value, and the definition's text
// is what is judged, by the taint of the task's whole view.
func TestFailure_ErrorInsideATaintedDefinitionIsWithheld(t *testing.T) {
	check := func(t *testing.T, f *Failure, err error, cell, expr string) {
		t.Helper()
		if strings.Contains(err.Error(), failureSecret) {
			t.Fatalf("the secret reached the error text: %v", err)
		}
		if !f.Withheld || f.Cause != withheldCause {
			t.Errorf("withheld = %v, cause = %q", f.Withheld, f.Cause)
		}
		if f.Cell != cell || f.Expr != expr {
			t.Errorf("failure = cell %q expr %q, want %q %q", f.Cell, f.Expr, cell, expr)
		}
		if strings.Contains(f.Cause+f.Expr+f.Cell, failureSecret) {
			t.Errorf("the secret reached a field: %+v", f)
		}
	}

	for name, tc := range map[string]struct{ scenario, cell, expr string }{
		"a scenario var keyed by a secret input": {`name: demo
input:
  pw: { type: string, secret: true }
vars:
  pick: "${ {'a': 1}[input.pw] }"
tasks:
  - name: reads it
    module: core.exec.run
    params: { cmd: "echo ${ vars.pick }" }
`, "vars.pick", "{'a': 1}[input.pw]"},
		"two hops from the cell, through a scenario var": {`name: demo
input:
  pw: { type: string, secret: true }
vars:
  k: "${ input.pw }"
  pick: "${ {'a': 1}[vars.k] }"
tasks:
  - name: reads it
    module: core.exec.run
    vars: { outer: "${ vars.pick }-x" }
    params: { cmd: "echo ${ vars.outer }" }
`, "vars.pick", "{'a': 1}[vars.k]"},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := renderFailure(t, tc.scenario)
			check(t, f, err, tc.cell, tc.expr)
		})
	}

	t.Run("a service var keyed by a task override", func(t *testing.T) {
		scn := &config.ScenarioManifest{
			Name:  "demo",
			Input: config.InputSchemaMap{"pw": {Type: "string", Secret: true}},
			Tasks: []config.Task{execTask("reads it", map[string]any{"k": "${ input.pw }"}, "echo ${ vars.pick }")},
		}
		_, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), RenderInput{
			Scenario:    scn,
			Input:       map[string]any{"pw": failureSecret},
			Incarnation: IncarnationMeta{ID: "demo", Service: "svc"},
			Hosts:       []*topology.HostFacts{host("a.example.com", nil, nil)},
			ServiceVars: serviceView(t, map[string]any{"pick": "${ {'a': 1}[vars.k] }"}),
		})
		f := FailureOf(err)
		if f == nil {
			t.Fatalf("no Failure in %v", err)
		}
		check(t, f, err, "vars.pick", "{'a': 1}[vars.k]")
	})

	t.Run("a vars.yml local keyed by a destiny task var", func(t *testing.T) {
		manifest, res := perHostApplyInput(
			[]config.Task{execTask("reads it", map[string]any{"p": "${ input.pw }"}, "echo ${ vars.b }")},
			config.InputSchemaMap{"pw": {Type: "string", Required: true, Secret: true}},
			map[string]any{"pw": failureSecret},
		)
		res.resolved.Vars = map[string]any{"b": "${ {'x': 1}[vars.p] }"}
		_, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
		f := FailureOf(err)
		if f == nil {
			t.Fatalf("no Failure in %v", err)
		}
		check(t, f, err, "vars.b", "{'x': 1}[vars.p]")
	})
}

// ★ GUARD. A `template:` path built from a secret is resolved before Keeper reads the
// file, and a read error is no CEL error, so withhold never judges it: the error has
// to quote the path as the author wrote it. The reader is the production one over an
// empty snapshot, whose own errors name the path they tried at three levels.
//
// A `vault:` reference is the same path built from a secret, resolved one phase
// earlier: the text written is the reference, and what Keeper tries to read is the
// secret itself.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: quote the resolved `rel`
// instead of the raw text in resolveTemplateUsesInput's two errors. Every case goes
// red. Taking `raw` from the vault-resolved params instead of the task's own turns
// the two `vault:` cases red.
func TestFailure_TemplatePathBuiltFromASecretIsQuotedAsWritten(t *testing.T) {
	root := t.TempDir()
	snapshot := func(scenarioPrefix string) TemplateReader {
		return NewSnapshotTemplateReader(func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(root, rel) }, scenarioPrefix)
	}
	rendered := func(tmpl string) config.Task {
		return config.Task{Name: "render", Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
			"path": "/etc/redis/redis.conf", "template": tmpl,
		}}}
	}
	const vaultRef = "vault:secret/tmpl/redis#path"
	kv := resolveVaultStubKV{secrets: map[string]map[string]any{
		"secret/tmpl/redis": {"path": "templates/" + hopSecret + ".tmpl"},
	}}
	cases := map[string]struct {
		in    func() RenderInput
		raw   string
		vault KVReader
	}{
		"a vault: reference": {func() RenderInput {
			in := hopRenderInput(hopScenario(rendered(vaultRef)))
			in.Templates = snapshot("scenario/create")
			return in
		}, vaultRef, kv},
		"a vault: reference, no TemplateReader": {func() RenderInput {
			return hopRenderInput(hopScenario(rendered(vaultRef)))
		}, vaultRef, kv},
		"a scenario secret input": {func() RenderInput {
			in := hopRenderInput(hopScenario(rendered("templates/${ input.admin_password }.tmpl")))
			in.Templates = snapshot("scenario/create")
			return in
		}, "templates/${ input.admin_password }.tmpl", nil},
		"a scenario-rung var over a secret input": {func() RenderInput {
			scn := hopScenario(rendered("templates/${ vars.tmpl }.tmpl"))
			scn.Vars = map[string]any{"tmpl": "${ input.admin_password }"}
			in := hopRenderInput(scn)
			in.Templates = snapshot("scenario/create")
			return in
		}, "templates/${ vars.tmpl }.tmpl", nil},
		"a destiny secret input": {func() RenderInput {
			dst := destinyWith(nil, rendered("templates/${ input.password }.tmpl"))
			dst.Templates = snapshot("")
			in := hopRenderInput(applyHopScenario())
			in.Destiny = &stubDestinyResolver{resolved: dst}
			return in
		}, "templates/${ input.password }.tmpl", nil},
		"no TemplateReader": {func() RenderInput {
			return hopRenderInput(hopScenario(rendered("templates/${ input.admin_password }.tmpl")))
		}, "templates/${ input.admin_password }.tmpl", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := NewPipeline(tc.vault, newEngine(t), nil, nil).Render(context.Background(), tc.in())
			if err == nil {
				t.Fatal("Render succeeded, want a missing template")
			}
			if strings.Contains(err.Error(), hopSecret) {
				t.Fatalf("the secret reached the error text: %v", err)
			}
			if !strings.Contains(err.Error(), tc.raw) {
				t.Errorf("the error does not quote the template: as written, %q: %v", tc.raw, err)
			}
			if f := FailureOf(err); f == nil || strings.Contains(f.Cause+f.Expr+f.Cell, hopSecret) {
				t.Errorf("failure = %+v, want one carrying no secret", f)
			}
		})
	}
}

// ★ GUARD. A comprehension over a map fails on the same element on every render, so the
// failure text is one text: a map iterates its keys in sorted order (shared/cel
// ordered.go), not in Go's. A task var's map and an input map alike.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: return m.Mapper.Iterator() from
// shared/cel orderedMap.Iterator. Both cases go red.
func TestFailure_AComprehensionOverAMapFailsOnTheSameKey(t *testing.T) {
	m := func() map[string]any { return map[string]any{"e": 5, "c": 3, "a": 1, "d": 4, "b": 2} }
	for name, in := range map[string]func() RenderInput{
		"a vars map": func() RenderInput {
			return RenderInput{Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{
				execTask("t", map[string]any{"m": m()}, "${ vars.m.all(k, vars[k] > 0) }"),
			}}}
		},
		"an input map": func() RenderInput {
			return RenderInput{
				Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{execTask("t", nil, "${ input.m.all(k, input[k] > 0) }")}},
				Input:    map[string]any{"m": m()},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			for i := range 50 {
				_, err := renderScenario(t, in())
				if err == nil || FailureOf(err) == nil || !strings.HasSuffix(err.Error(), "no such key: a") {
					t.Fatalf("render %d: %v; want a Failure on the first key in order, a", i, err)
				}
			}
		})
	}
}

// unsafePathReader refuses every read as artifact.ReadSnapshotFile refuses a path that
// escapes the snapshot: with no *fs.PathError, and the path it was given quoted.
type unsafePathReader struct{ asked []string }

func (r *unsafePathReader) Read(rel string) ([]byte, error) {
	r.asked = append(r.asked, rel)
	return nil, fmt.Errorf("artifact: unsafe path %q: %w", rel, errors.New("path escapes from parent"))
}

// ★ GUARD. A read error that is no *fs.PathError, but quotes the resolved path all the
// same, reaches neither the error text nor the Failure: the path is quoted as written,
// over a cause that says only that the read failed.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: `return err` in the default
// branch of pathFreeCause. It goes red.
func TestFailure_TemplateReadErrorWithoutAPathErrorNamesNoResolvedPath(t *testing.T) {
	const raw = "templates/${ input.admin_password }.tmpl"
	reader := &unsafePathReader{}
	in := hopRenderInput(hopScenario(config.Task{Name: "render", Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
		"path": "/etc/redis/redis.conf", "template": raw,
	}}}))
	in.Templates = reader
	_, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render succeeded, want the read refused")
	}
	if len(reader.asked) == 0 || !strings.Contains(reader.asked[0], hopSecret) {
		t.Fatalf("the reader was asked %q, want the path resolved from the secret", reader.asked)
	}
	if strings.Contains(err.Error(), hopSecret) {
		t.Fatalf("the secret reached the error text: %v", err)
	}
	if !strings.Contains(err.Error(), raw) || !strings.HasSuffix(err.Error(), "read failed") {
		t.Errorf("text = %v, want the template as written over a path-free cause", err)
	}
	if f := FailureOf(err); f == nil || strings.Contains(f.Cause+f.Expr+f.Cell, hopSecret) {
		t.Errorf("failure = %+v, want one carrying no secret", f)
	}
}
