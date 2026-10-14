package render

import (
	"context"
	"strings"
	"testing"

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
			m.Tasks[0].Vars = map[string]any{"k": fails}
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
			if f := FailureOf(err); f == nil || f.Host == "" || f.Cause != "no such key: nope" {
				t.Fatalf("failure = %+v (err %v), want it on its host", f, err)
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
