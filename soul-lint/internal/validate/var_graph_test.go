package validate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/servicevars"
)

// lintJSON runs one validate-* command and decodes its diagnostics.
func lintJSON(t *testing.T, opts Options) []diag.Diagnostic {
	t.Helper()
	opts.JSON = true
	var out, errOut bytes.Buffer
	if code := Run(opts, &out, &errOut); code == ExitIOFatal {
		t.Fatalf("soul-lint: fatal: %s", errOut.String())
	}
	var ds []diag.Diagnostic
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var d diag.Diagnostic
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			t.Fatalf("decode %q: %v", sc.Text(), err)
		}
		ds = append(ds, d)
	}
	return ds
}

func withCode(ds []diag.Diagnostic, code string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, d := range ds {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

func serviceDiags(t *testing.T, root, serviceName string) []diag.Diagnostic {
	t.Helper()
	return lintJSON(t, Options{Path: filepath.Join(root, "service.yml"), Kind: KindService, ServiceName: serviceName})
}

func scenarioDiags(t *testing.T, root, scenario string) []diag.Diagnostic {
	t.Helper()
	return lintJSON(t, Options{Path: filepath.Join(root, "scenario", scenario, "main.yml"), Kind: KindScenario, ServiceName: "redis"})
}

// scenarioWithTaskVars is a one-task scenario whose task carries vars (a YAML
// mapping body, indented under `vars:`) and reads them from params.
func scenarioWithTaskVars(scenarioVars, taskVars string) string {
	s := "name: create\ninput: {}\n"
	if scenarioVars != "" {
		s += "vars:\n" + scenarioVars
	}
	s += "tasks:\n  - name: write config\n    module: core.file.present\n"
	if taskVars != "" {
		s += "    vars:\n" + taskVars
	}
	return s + "    params:\n      path: /etc/redis/redis.conf\n      content: \"x\"\n"
}

// keeperCycleText is the text the Keeper raises for the first var of names whose
// evaluation fails with a cycle in view.
func keeperCycleText(t *testing.T, view *cel.VarView, names ...string) string {
	t.Helper()
	for _, n := range names {
		_, _, err := view.Lookup(n)
		var cyc *cel.VarCycleError
		if errors.As(err, &cyc) {
			return cyc.Error()
		}
	}
	t.Fatalf("the Keeper's view raises no cycle for %v", names)
	return ""
}

// keeperServiceView is the service view the Keeper builds for root.
func keeperServiceView(t *testing.T, root string) *cel.VarView {
	t.Helper()
	layers, err := servicevars.Layers(os.DirFS(root), servicevars.IncarnationContext{ID: "x", Service: "redis"}, nil)
	if err != nil {
		t.Fatalf("servicevars.Layers: %v", err)
	}
	return cel.NewVarView(nil, layers...)
}

// TestVarUnknownRef_HasGuardExemptsOnlyTheGuardedRoot — a read is a need unless the
// same expression tests has() or `in` on that same root name; has() is what
// default(vars.x, y) expands to. has(vars.x.y) and default(vars.x.y, d) guard y,
// so x is still a need.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in servicevars.ExprReads
// leave a test-only select out of hasTests — "has guard" goes red; drop the `_in_`
// case of readWalk.call — "key-test guard" goes red; or let a presence test on a
// field guard the field's root — "default over a field" and "has over a field" go
// red.
func TestVarUnknownRef_HasGuardExemptsOnlyTheGuardedRoot(t *testing.T) {
	for name, tc := range map[string]struct {
		def  string
		want string // the unknown name, "" for none
	}{
		"default guard":          {`"${ default(vars.tls_override, false) }"`, ""},
		"has guard":              {`"${ has(vars.o) ? vars.o : 1 }"`, ""},
		"key-test guard":         {`"${ 'o' in vars ? vars.o : 1 }"`, ""},
		"unguarded read":         {`"${ vars.missing }"`, "missing"},
		"literal index read":     {`"${ vars['missing'] }"`, "missing"},
		"default over a field":   {`"${ default(vars.cfg.port, 1) }"`, "cfg"},
		"has over a field":       {`"${ has(vars.cfg.port) ? vars.cfg.port : 1 }"`, "cfg"},
		"guard in another block": {`"${ has(vars.o) }-${ vars.o }"`, "o"},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeServiceTree(t, map[string]string{"vars/00-base.yaml": "x: " + tc.def + "\n"})
			got := withCode(serviceDiags(t, root, "redis"), codeVarUnknownRef)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("a guarded read is not a need, got %+v", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0].Message, "reads vars."+tc.want+",") {
				t.Fatalf("want one var_unknown_ref naming vars.%s, got %+v", tc.want, got)
			}
			if got[0].Level != diag.LevelError || got[0].Line != 1 {
				t.Errorf("want an ERROR at line 1 of the layer file, got %+v", got[0])
			}
		})
	}
}

// TestVarCycle_TextIsTheKeepers — soul-lint renders a cycle in the exact text the
// Keeper raises for the same definitions: rotated to its smallest name, closed by
// it. A self-reference with nothing below is the one-node cycle, and a cycle a
// task closes through a service var is visible only in the task's view.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in reportCycle start the
// ring where the walk found it (start := 0, no rotation) — "entered off the ring"
// goes red; or drop the self-cycle report of varWalk.expand — "self-reference in a
// task" goes red. (In the service view the stub evaluation reports a self-reference
// the walk missed, so only a view without a stub isolates the walk.)
func TestVarCycle_TextIsTheKeepers(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"three files": {
			"vars/00-a.yaml": "a: \"${ vars.b }\"\n",
			"vars/10-b.yaml": "b: \"${ vars.c }\"\n",
			"vars/20-c.yaml": "c: \"${ vars.a }\"\n",
		},
		"entered off the ring": {
			"vars/00-base.yaml": "a: \"${ vars.c }\"\nc: \"${ vars.b }\"\nb: \"${ vars.c }-x\"\n",
		},
		"self-reference, nothing below": {
			"vars/00-base.yaml": "conf_dir: \"${ vars.conf_dir }/wb\"\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeServiceTree(t, files)
			want := keeperCycleText(t, keeperServiceView(t, root), "a", "b", "c", "conf_dir")
			got := withCode(serviceDiags(t, root, "redis"), codeVarCycle)
			if len(got) != 1 || got[0].Message != want {
				t.Fatalf("want one var_cycle %q, got %+v", want, got)
			}
		})
	}

	t.Run("across rungs", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/00-base.yaml":        "dsn: \"redis://${ vars.pw }@localhost\"\n",
			"scenario/create/main.yml": scenarioWithTaskVars("", "      pw: \"${ vars.dsn }\"\n"),
		})
		eng, err := cel.New()
		if err != nil {
			t.Fatal(err)
		}
		task := cel.NewVarView(keeperServiceView(t, root), cel.VarLayer{
			Rung: cel.RungTask, Origin: taskRungOrigin, Defs: map[string]any{"pw": "${ vars.dsn }"}, Engine: eng,
		})
		want := keeperCycleText(t, task, "pw", "dsn")
		got := withCode(scenarioDiags(t, root, "create"), codeVarCycle)
		if len(got) != 1 || got[0].Message != want {
			t.Fatalf("want one var_cycle %q, got %+v", want, got)
		}
		if !strings.HasSuffix(got[0].File, filepath.Join("scenario", "create", "main.yml")) {
			t.Errorf("a cycle a task closes is the scenario's to report, got %s", got[0].File)
		}
		// The service view alone has no cycle: dsn's read of pw is a need there.
		if c := withCode(serviceDiags(t, root, "redis"), codeVarCycle); len(c) != 0 {
			t.Errorf("no cycle in the service view, got %+v", c)
		}
	})

	t.Run("self-reference in a task", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/00-base.yaml":        "port: 6379\n",
			"scenario/create/main.yml": scenarioWithTaskVars("", "      conf_dir: \"${ vars.conf_dir }/wb\"\n"),
		})
		got := withCode(scenarioDiags(t, root, "create"), codeVarCycle)
		if len(got) != 1 || got[0].Message != "var_cycle: conf_dir → conf_dir" {
			t.Fatalf("want one var_cycle conf_dir → conf_dir, got %+v", got)
		}
	})
}

// destinyVarsKeeperView is the view the Keeper's destiny pass builds over a
// vars.yml alone.
func destinyVarsKeeperView(t *testing.T, varsYml string) *cel.VarView {
	t.Helper()
	defs, err := config.LoadDestinyVarsFromBytes("vars.yml", []byte(varsYml))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	return cel.NewVarView(nil, cel.VarLayer{Rung: cel.RungDestinyFile, Origin: destinyRungOrigin, Defs: defs, Engine: eng})
}

// destinyVarsDiags lints a destiny whose vars.yml is varsYml.
func destinyVarsDiags(t *testing.T, varsYml string) []diag.Diagnostic {
	t.Helper()
	path := writeDestinyDir(t, "d", varsYml,
		"- name: t\n  module: core.file.present\n  params:\n    path: /tmp/x\n    content: x\n")
	return lintJSON(t, Options{Path: path, Kind: KindDestiny})
}

// TestVarCycle_SelfReferenceOverALowerDefinitionIsNot — a self-reference reads the
// definition strictly below the layer that wrote it, in a later file of the rung or
// in a higher rung alike, so deriving a value from the layer below is no cycle.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in varWalk.expand resolve a
// self-read at the top of the stack (`w.sel.winner(n.name, w.v.top())` for
// `layer-1`) — both cases go red.
func TestVarCycle_SelfReferenceOverALowerDefinitionIsNot(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml":        "conf_dir: /etc/redis\n",
		"vars/90-wb.yaml":          "conf_dir: \"${ vars.conf_dir }/wb\"\n",
		"scenario/create/main.yml": scenarioWithTaskVars("", "      conf_dir: \"${ vars.conf_dir }/conf.d\"\n"),
	})
	if got := withCode(serviceDiags(t, root, "redis"), codeVarCycle); len(got) != 0 {
		t.Errorf("a later file over an earlier one is no cycle, got %+v", got)
	}
	if got := withCode(scenarioDiags(t, root, "create"), codeVarCycle); len(got) != 0 {
		t.Errorf("a task over the service rung is no cycle, got %+v", got)
	}
}

// TestVarCycle_AgreesWithTheKeeperOnWhatAReadEvaluates — a definition reaching
// `vars` whole (`${ vars }`, merge(), an equality, a list membership) reads every
// name of the view, its own below it included, so with nothing below it is the
// Keeper's `x → x`. A key-only read — size(), `'k' in vars`, an iteration over the
// keys — evaluates nothing, and neither does a read evaluated only where a key test
// on its name holds, which with nothing below it never does: the branch of `?:` the
// test selects, a term beside the test in `&&` or beside its negation in `||`. A
// read in the branch the test fails is the cycle. Each row is checked against the
// Keeper's own view first, so the table cannot drift from the runtime. A destiny's
// vars.yml is the view: validate-destiny has no stub evaluation behind the static
// walk.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in varWalk.expand ignore
// stringReads.whole — the whole-read rows go red; report a self-read with nothing
// below whatever it reads (drop `(r.whole || r.unkeyed[n.name])`) — the key-tested
// rows that raise no cycle go red; in servicevars readWalk.call walk both branches of a
// key-tested `?:` with w.held — "inverted key test" goes red; or drop its
// LogicalAnd/LogicalOr case — the `&&` and `||` rows that raise no cycle go red.
func TestVarCycle_AgreesWithTheKeeperOnWhatAReadEvaluates(t *testing.T) {
	for name, tc := range map[string]struct {
		def   string
		cycle bool
	}{
		"whole cell":                 {`"${ vars }"`, true},
		"merge":                      {`"${ merge(vars, {'k': 1}) }"`, true},
		"equality":                   {`"${ vars == {} }"`, true},
		"list membership":            {`"${ vars in [{}] }"`, true},
		"nested in a map":            {`{inner: "${ vars }"}`, true},
		"size":                       {`"${ size(vars) }"`, false},
		"key test":                   {`"${ 'other' in vars }"`, false},
		"iteration":                  {`"${ vars.exists(k, k == 'other') }"`, false},
		"key-tested self-read":       {`"${ 'x' in vars ? vars.x : 'd' }"`, false},
		"has-tested self-read":       {`"${ has(vars.x) ? vars.x : 'd' }"`, true},
		"inverted key test":          {`"${ 'x' in vars ? 'd' : vars.x }"`, true},
		"negated key test":           {`"${ !('x' in vars) ? 'd' : vars.x }"`, false},
		"key test before it in &&":   {`"${ 'x' in vars && vars.x > 0 }"`, false},
		"key test after it in &&":    {`"${ vars.x > 0 && 'x' in vars }"`, false},
		"negated key test in ||":     {`"${ !('x' in vars) || vars.x > 0 }"`, false},
		"key test in ||":             {`"${ 'x' in vars || vars.x > 0 }"`, true},
		"negated key test in &&":     {`"${ !('x' in vars) && vars.x > 0 }"`, true},
		"key test in another branch": {`"${ 'x' in vars ? vars.x : vars.x }"`, true},
	} {
		t.Run(name, func(t *testing.T) {
			varsYml := "other: 1\nx: " + tc.def + "\n"
			_, _, err := destinyVarsKeeperView(t, varsYml).Lookup("x")
			if keeper := errors.Is(err, cel.ErrVarCycle); keeper != tc.cycle {
				t.Fatalf("the Keeper raises a cycle: %v (%v); the row says %v", keeper, err, tc.cycle)
			}
			got := withCode(destinyVarsDiags(t, varsYml), codeVarCycle)
			switch {
			case tc.cycle && (len(got) != 1 || got[0].Message != err.Error()):
				t.Fatalf("want one var_cycle %q, got %+v", err, got)
			case !tc.cycle && len(got) != 0:
				t.Fatalf("no cycle, got %+v", got)
			}
		})
	}
}

// TestVarCycle_KeyTestConditionsAgreeWithTheKeeper — a key test protects a read
// through any condition built of it with `!`, `&&`, `||` and a comparison with a
// bool literal. The idiom `'x' in vars && <a test of vars.x> ? vars.x + '/x' :
// '/etc/x'` with nothing below x never reads x, nor does any condition that implies
// the key on the branch the read is in, or a term of `&&` (`||`) beside one that
// implies it when true (false). A condition that implies it only on the other side
// reads x where it is absent, and is the Keeper's `x → x`. Each row is checked
// against the Keeper's own view first, as x of a destiny's vars.yml and of a
// scenario task's vars over a service rung, both with y = 1 and a map z, and
// soul-lint must report exactly what the Keeper raises. A condition built of
// anything else proves nothing: `('x' in vars ? true : false) && vars.x == 1` is
// still a var_cycle here, though the Keeper raises none.
//
// A bool literal proves every key on the side it never takes, so `'x' in vars ||
// false` implies x where it is true, as the Keeper's evaluation does.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in servicevars.impliedKeys
// let `&&` take the union for both polarities — "&& false branch" goes red; let `||`
// take the union where it is true — "|| true branch" goes red; or drop its
// boolLiteral case — "|| false" and the other literal rows that raise no cycle go
// red.
func TestVarCycle_KeyTestConditionsAgreeWithTheKeeper(t *testing.T) {
	type row struct {
		name, expr string
		cycle      bool
	}
	rows := []row{
		{"conf_dir idiom", `'x' in vars && vars.x != '' ? vars.x + '/x' : '/etc/x'`, false},
		{"&& test after a read", `vars.y == 1 && 'x' in vars ? vars.x : 0`, false},
		{"&& test before a read", `'x' in vars && vars.y == 1 ? vars.x : 'd'`, false},
		{"|| negated test", `!('x' in vars) || vars.x == '' ? 'd' : vars.x`, false},
		{"negated &&", `!('x' in vars && true) ? 0 : vars.x`, false},
		{"double negation", `!(!('x' in vars)) ? vars.x : 0`, false},
		{"== true", `('x' in vars) == true ? vars.x : 0`, false},
		{"true ==", `true == ('x' in vars) ? vars.x : 0`, false},
		{"!= false", `('x' in vars) != false ? vars.x : 0`, false},
		{"== false", `('x' in vars) == false ? 0 : vars.x`, false},
		{"!= true", `('x' in vars) != true ? 0 : vars.x`, false},
		{"size beside the test", `size(vars) > 0 && 'x' in vars ? vars.x : 0`, false},
		{"|| of && terms", `'x' in vars && vars.y == 1 || 'x' in vars && vars.x == 1 ? vars.x : 0`, false},
		{"|| of negated tests", `!('x' in vars) || !('y' in vars) || vars.x == 1`, false},
		{"&& under ||", `vars.y == 0 || 'x' in vars && vars.x == 1`, false},
		{"&& in a list", `['x' in vars && vars.x == 1][0]`, false},
		{"&& in a map", `{'k': 'x' in vars && vars.x == 1}.k`, false},
		{"nested in the true branch", `'x' in vars ? (vars.x == 1 && 'z' in vars) : false`, false},
		{"nested in the false branch", `!('x' in vars) ? 0 : ('x' in vars ? vars.x : 1)`, false},
		{"has beside the test", `has(vars.x) && 'x' in vars`, false},
		{"comprehension beside the test", `'x' in vars && [vars.x].exists(v, v == 1)`, false},
		{"default beside the test", `default(vars.x, 0) == 0 && 'x' in vars`, false},
		{"index beside the test", `vars['x'] == 1 && 'x' in vars`, false},
		{"test on a field of z", `'x' in vars.z`, false},
		{"inverted", `'x' in vars ? 1 : vars.x`, true},
		{"&& false branch", `!('x' in vars) && vars.y == 2 ? 0 : vars.x`, true},
		{"&& false branch, test first", `'x' in vars && vars.y == 1 ? 0 : vars.x`, true},
		{"|| true branch", `'x' in vars || vars.y == 1 ? vars.x : 0`, true},
		{"negated test, true branch", `!('x' in vars) ? vars.x : 0`, true},
		{"== false, true branch", `('x' in vars) == false ? vars.x : 0`, true},
		{"another name's test", `'y' in vars ? vars.x : 0`, true},
		{"not a key test", `'x' in vars ? 1 : 2 == 2 ? vars.x : 0`, true},
		{"has", `has(vars.x) ? vars.x : 0`, true},
		{"negated has || test", `!has(vars.x) || 'x' in vars`, true},
		{"|| test", `'x' in vars || vars.x == 1`, true},
		{"&& negated test", `!('x' in vars) && vars.x == 1`, true},
		{"whole read beside the test", `'x' in vars && vars == {}`, true},
		{"", `'x' in vars ? vars.x : 0`, false},
		{"", `!('x' in vars) ? 0 : vars.x`, false},
		{"", `'x' in vars && vars.x != '' ? vars.x : 'd'`, false},
		{"", `'x' in vars && (vars.y == 1 ? vars.x : 0) == 1`, false},
		{"", `'x' in vars && has(vars.x)`, false},
		{"", `'x' in vars ? vars.y : vars.x`, true},
		{"", `vars.x == 1 && 'x' in vars`, false},
		{"", `vars.x == 1 || !('x' in vars)`, false},
		{"", `('x' in vars && vars.x == 1) || false`, false},
		{"", `('x' in vars && vars.x == 1) || vars.y == 1`, false},
		{"", `'x' in vars && vars.y == 1 && vars.x == 1`, false},
		{"", `vars.y == 1 && vars.x == 1 && 'x' in vars`, false},
		{"", `[vars.x].size() == 1 && 'x' in vars`, false},
		{"", `'x' in vars ? vars.x : ('x' in vars ? vars.x : 0)`, false},
		{"", `'x' in vars && vars.x`, false},
		{"", `vars.x && 'x' in vars`, false},
		{"", `(vars.x || true) && 'x' in vars`, false},
		{"", `'x' in vars && vars.x.a == 1`, false},
		{"", `'x' in vars && default(vars.x, 0) == 0`, false},
		{"", `'x' in vars ? default(vars.x, 0) : 0`, false},
		{"", `int(vars.x) == 1 && 'x' in vars`, false},
		{"", `vars.x + 1 == 2 && 'x' in vars`, false},
		{"", `dyn(vars.x) == 1 && 'x' in vars`, false},
		{"", `'x' in vars && vars.y == 1 || 'x' in vars && vars.x == 1`, false},
		{"", `!('x' in vars) ? 'd' : vars.x == 1 ? 'a' : 'b'`, false},
		{"", `'x' in vars ? vars.x : vars.y`, false},
		{"", `'x' in vars && vars.x == 1 ? 1 : 0`, false},
		{"|| false", `'x' in vars || false ? vars.x : 0`, false},
		{"false ||", `false || 'x' in vars ? vars.x : 0`, false},
		{"&& true", `'x' in vars && true ? vars.x : 0`, false},
		{"false condition", `false ? vars.x : 0`, false},
		{"true condition", `true ? 0 : vars.x`, false},
		{"negated true", `!true ? vars.x : 0`, false},
		{"false &&", `false && vars.x == 1`, false},
		{"true ||", `true || vars.x == 1`, false},
		{"|| false == true", `('x' in vars || false) == true ? vars.x : 0`, false},
		{"false == true", `false == true ? vars.x : 0`, false},
		{"true condition, read", `true ? vars.x : 0`, true},
		{"false condition, read", `false ? 0 : vars.x`, true},
		{"|| true", `'x' in vars || true ? vars.x : 0`, true},
		{"true &&", `true && vars.x == 1`, true},
	}
	const below = "y: 1\nz:\n  q: 1\n"
	eng, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, keeper error, got []diag.Diagnostic, cycle bool) {
		t.Helper()
		if errors.Is(keeper, cel.ErrVarCycle) != cycle {
			t.Fatalf("the Keeper raises %v; the row says cycle = %v", keeper, cycle)
		}
		switch {
		case cycle && (len(got) != 1 || got[0].Message != keeper.Error()):
			t.Fatalf("want one var_cycle %q, got %+v", keeper, got)
		case !cycle && len(got) != 0:
			t.Fatalf("no cycle, got %+v", got)
		}
	}
	for _, r := range rows {
		def := `"${ ` + r.expr + ` }"`
		if r.name == "" {
			r.name = r.expr
		}
		t.Run(r.name+"/vars.yml", func(t *testing.T) {
			varsYml := below + "x: " + def + "\n"
			_, _, keeper := destinyVarsKeeperView(t, varsYml).Lookup("x")
			check(t, keeper, withCode(destinyVarsDiags(t, varsYml), codeVarCycle), r.cycle)
		})
		t.Run(r.name+"/task", func(t *testing.T) {
			root := writeServiceTree(t, map[string]string{
				"vars/00-base.yaml":        below,
				"scenario/create/main.yml": scenarioWithTaskVars("", "      x: "+def+"\n"),
			})
			task := cel.NewVarView(keeperServiceView(t, root), cel.VarLayer{
				Rung: cel.RungTask, Origin: taskRungOrigin, Defs: map[string]any{"x": "${ " + r.expr + " }"}, Engine: eng,
			})
			_, _, keeper := task.Lookup("x")
			check(t, keeper, withCode(scenarioDiags(t, root, "create"), codeVarCycle), r.cycle)
		})
	}
}

// TestVarCycle_WholeReadInATaskAndAVarsYml — the same whole read in a scenario
// task's own vars and in a destiny's vars.yml: nothing below the definition defines
// its name, so it is the one-node cycle in both, cited where it is written.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make ExprReads never report
// whole (drop the IdentKind case of readWalk.expr) — both go red.
func TestVarCycle_WholeReadInATaskAndAVarsYml(t *testing.T) {
	const want = "var_cycle: snapshot → snapshot"
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml":        "port: 6379\n",
		"scenario/create/main.yml": scenarioWithTaskVars("", "      snapshot: \"${ vars }\"\n"),
	})
	got := withCode(scenarioDiags(t, root, "create"), codeVarCycle)
	if len(got) != 1 || got[0].Message != want || got[0].YAMLPath != "$.tasks[0].vars.snapshot" {
		t.Errorf("scenario task: want one %q at $.tasks[0].vars.snapshot, got %+v", want, got)
	}

	path := writeDestinyDir(t, "d", "port: 6379\nsnapshot: \"${ vars }\"\n",
		"- name: t\n  module: core.file.present\n  params:\n    path: /tmp/x\n    content: \"${ vars.port }\"\n")
	got = withCode(lintJSON(t, Options{Path: path, Kind: KindDestiny}), codeVarCycle)
	if len(got) != 1 || got[0].Message != want || got[0].Line != 2 {
		t.Errorf("vars.yml: want one %q at line 2, got %+v", want, got)
	}
}

// TestVarCycle_StubEvaluationCycleIsAVarCycle — a cycle only the stub evaluation
// finds, through a `vars[expr]` the static walk cannot follow, is a var_cycle in
// the Keeper's text at the file and line of its first var, not a var_invalid at
// vars/, and its hint says what found it without guessing why the walk did not.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in serviceStubDiags let a
// VarCycleError fall through to the var_invalid branch — this goes red.
func TestVarCycle_StubEvaluationCycleIsAVarCycle(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"vars/10-pair.yaml": "a: \"${ vars['b' + ''] }\"\nb: \"${ vars.a }\"\n",
	})
	want := keeperCycleText(t, keeperServiceView(t, root), "a")
	ds := serviceDiags(t, root, "redis")
	got := withCode(ds, codeVarCycle)
	if len(got) != 1 || got[0].Message != want {
		t.Fatalf("want one var_cycle %q, got %+v", want, got)
	}
	if d := got[0]; d.File != filepath.Join(root, "vars", "10-pair.yaml") || d.Line != 1 || d.YAMLPath != "$.a" {
		t.Errorf("want vars/10-pair.yaml:1 $.a, got %s:%d %s", d.File, d.Line, d.YAMLPath)
	}
	if h := got[0].Hint; !strings.HasPrefix(h, "evaluating the service view on the stub incarnation found this ring;") || strings.Contains(h, "not spelled") {
		t.Errorf("the hint names what found the ring and nothing else: %q", h)
	}
	if inv := withCode(ds, codeVarInvalid); len(inv) != 0 {
		t.Errorf("a cycle is not var_invalid, got %+v", inv)
	}
}

// TestVarCycle_EveryCycleOfAGroup — definitions that read each other in more than
// one ring form one group, and the Keeper names whichever ring its evaluation order
// meets first. Every distinct ring the search walks is reported, none twice: here
// both rings of the group, and each text the Keeper raises from a, b or z is one of
// them. The walk is judged in a destiny's vars.yml, which no stub evaluation backs;
// in the service view the stub's ring must not repeat one the walk reported.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in findCycles pass
// elementaryCycles a limit of 1 — "the walk" goes red; or in serviceStubDiags drop
// the static.rings test — "walk and stub" goes red.
func TestVarCycle_EveryCycleOfAGroup(t *testing.T) {
	const defs = "a: \"${ vars.z }${ vars.b }\"\nb: \"${ vars.z }\"\nz: \"${ vars.a }\"\n"
	want := []string{"var_cycle: a → b → z → a", "var_cycle: a → z → a"}
	once := func(t *testing.T, got []diag.Diagnostic) map[string]bool {
		t.Helper()
		seen := map[string]bool{}
		for _, d := range got {
			if seen[d.Message] {
				t.Errorf("reported twice: %q", d.Message)
			}
			seen[d.Message] = true
		}
		if len(seen) != len(want) || !seen[want[0]] || !seen[want[1]] {
			t.Errorf("want exactly %v, got %+v", want, got)
		}
		return seen
	}
	t.Run("the walk", func(t *testing.T) {
		view := destinyVarsKeeperView(t, defs)
		seen := once(t, withCode(destinyVarsDiags(t, defs), codeVarCycle))
		for _, n := range []string{"a", "b", "z"} {
			if text := keeperCycleText(t, view, n); !seen[text] {
				t.Errorf("the Keeper raises %q for %s, which is not reported", text, n)
			}
		}
	})
	t.Run("walk and stub", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{"vars/00-base.yaml": defs})
		once(t, withCode(serviceDiags(t, root, "redis"), codeVarCycle))
	})
}

// pairLayersTree is a base `a: 1, b: 1` under k `when:`-gated files that each define
// a over b and b over a. Every a reads every b and the other way round, so the group
// holds every alternating ring, a number that grows factorially with k, and almost
// all of them repeat the text of a shorter one at the same definition.
func pairLayersTree(t *testing.T, k int) string {
	t.Helper()
	files := map[string]string{"vars/00-base.yaml": "a: 1\nb: 1\n"}
	stack := "stack:\n  - file: 00-base.yaml\n"
	for i := 1; i <= k; i++ {
		file := fmt.Sprintf("%02d-l.yaml", i)
		stack += fmt.Sprintf("  - file: %s\n    when: \"'c%d' in incarnation.covens\"\n", file, i)
		files["vars/"+file] = "a: \"${ vars.b }\"\nb: \"${ vars.a }\"\n"
	}
	files["vars/_stack.yaml"] = stack
	return writeServiceTree(t, files)
}

// randomLayersTree is n vars over a base that reads lower names only, under layers
// `when:`-gated files whose every var reads three others, drawn from a fixed seed.
func randomLayersTree(t *testing.T, n, layers int) string {
	t.Helper()
	rng := rand.New(rand.NewPCG(1014, uint64(n)))
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("v%03d", i)
	}
	reads := func(from []string) string {
		pool := slices.Clone(from)
		rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		var terms []string
		for _, name := range pool[:3] {
			terms = append(terms, "string(vars."+name+")")
		}
		return `"${ ` + strings.Join(terms, " + ") + ` }"`
	}
	var base strings.Builder
	for i, name := range names {
		def := "lit"
		if i >= 3 {
			def = reads(names[:i])
		}
		fmt.Fprintf(&base, "%s: %s\n", name, def)
	}
	files := map[string]string{"vars/00-base.yaml": base.String()}
	stack := "stack:\n  - file: 00-base.yaml\n"
	for l := 1; l <= layers; l++ {
		file := fmt.Sprintf("%02d-l.yaml", l)
		stack += fmt.Sprintf("  - file: %s\n    when: \"'c%d' in incarnation.covens\"\n", file, l)
		var body strings.Builder
		for _, name := range names {
			others := slices.DeleteFunc(slices.Clone(names), func(o string) bool { return o == name })
			fmt.Fprintf(&body, "%s: %s\n", name, reads(others))
		}
		files["vars/"+file] = body.String()
	}
	files["vars/_stack.yaml"] = stack
	return writeServiceTree(t, files)
}

// serviceWork walks the service view of root as validate-service does, within a 10s
// backstop that only stops a search that does not end, and returns what it did.
func serviceWork(t *testing.T, root string) varWork {
	t.Helper()
	done := make(chan varWork, 1)
	start := time.Now()
	go func() { done <- serviceVarChecks(root, "redis", false).work }()
	select {
	case w := <-done:
		t.Logf("selections %d, nodes %d, edges %d, cells %d, rings per group %v, %s",
			w.selections, w.nodes, w.edges, w.cells, w.rings, time.Since(start).Round(time.Millisecond))
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("the cycle search has not ended after 10s")
		return varWork{}
	}
}

// serviceJSON is validate-service's JSON output for root, decoded: a decoder, not
// lintJSON's line scanner, since a ring through many names makes a line longer than
// the scanner's buffer. Two runs must print the same bytes.
func serviceJSON(t *testing.T, root string) []diag.Diagnostic {
	t.Helper()
	var outs [2]string
	for i := range outs {
		var out, errOut bytes.Buffer
		Run(Options{Path: filepath.Join(root, "service.yml"), Kind: KindService, ServiceName: "redis", JSON: true}, &out, &errOut)
		outs[i] = out.String()
	}
	if outs[0] != outs[1] {
		t.Errorf("a second run differs:\n%s\nfirst:\n%s", outs[1], outs[0])
	}
	var ds []diag.Diagnostic
	for dec := json.NewDecoder(strings.NewReader(outs[0])); dec.More(); {
		var d diag.Diagnostic
		if err := dec.Decode(&d); err != nil {
			t.Fatal(err)
		}
		ds = append(ds, d)
	}
	return ds
}

// TestVarCycle_RingSearchIsBounded — the search walks at most maxCyclesPerGroup
// rings in a group of one selection, whether a ring is new, repeats a reported text
// or cites nothing the command owns, so a group with factorially many rings costs
// what a small one does. Inside a selection a read has one winner, so k layers that
// each define a over b and b over a hold one ring per selection, the `a → b → a` the
// Keeper raises for any incarnation in one of the k covens, and it is reported once.
// The work counters are the assertion; the wall-clock bound only stops a search that
// does not end. Two runs print the same bytes.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: let a read resolve to every
// definer of the stack (range w.sel.definers(name, w.v.top()) in varWalk.expand, an
// edge to each) — the pair rows go red on their one ring.
func TestVarCycle_RingSearchIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		root func(t *testing.T) string
		want string // the one text the Keeper raises, "" to skip
	}{
		{"5 pair layers", func(t *testing.T) string { return pairLayersTree(t, 5) }, "var_cycle: a → b → a"},
		{"8 pair layers", func(t *testing.T) string { return pairLayersTree(t, 8) }, "var_cycle: a → b → a"},
		{"16 pair layers", func(t *testing.T) string { return pairLayersTree(t, 16) }, "var_cycle: a → b → a"},
		{"200 random vars over 10 layers", func(t *testing.T) string { return randomLayersTree(t, 200, 10) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.root(t)
			if tc.want != "" {
				if got := keeperCycleText(t, keeperCovenView(t, root, "c1"), "a"); got != tc.want {
					t.Fatalf("the Keeper raises %q, the row says %q", got, tc.want)
				}
			}
			w := serviceWork(t, root)
			if len(w.rings) == 0 {
				t.Fatal("no group was searched")
			}
			if m := slices.Max(w.rings); m > maxCyclesPerGroup {
				t.Fatalf("a group walked %d rings, the bound is %d", m, maxCyclesPerGroup)
			}
			cycles := withCode(serviceJSON(t, root), codeVarCycle)
			if len(cycles) == 0 {
				t.Fatal("no var_cycle reported")
			}
			if tc.want != "" {
				if got := messages(cycles); !slices.Equal(got, []string{tc.want}) {
					t.Errorf("want the Keeper's %q once, got %q", tc.want, got)
				}
				if slices.Max(w.rings) != 1 {
					t.Errorf("each selection holds one ring, walked %v", w.rings)
				}
			}
		})
	}
}

// TestVarCycle_WorkIsLinearInTheLayers — the cost of the walk is a pass over each
// selection, and a selection costs what its definitions do: a name is assembled
// once per prefix of its definers, and a scenario's task views walk the service
// rung in S_all and S_base only. Three trees that cost seconds to minutes and
// gigabytes before (NIM-1014): 1500 per-incarnation files under validate-service,
// 1000 of them under a scenario of 20 tasks, and 500 vars over 20 conditional layers
// that each read `vars` whole. The per-incarnation files are one step's
// alternatives, so S_all and S_base are the base alone and walked once. The counters
// are the assertion; the wall-clock bound only stops a search that does not end.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in scenarioVarChecks give the
// task views perLayer: true — "scenario over 1000 files" goes red on its selections;
// drop the memo of varWalk.cell, assembling every node from its first definer —
// "500 whole-read vars" goes red on its cells; count only the rings that add a
// diagnostic (reportCycle returns whether it emitted, and elementaryCycles counts a
// ring only then) — "500 whole-read vars" goes red on its rings, every stack of one
// layer repeating the texts of the first; or make eachSelection walk S_base whatever
// S_all stacks (drop `if stacked > 0`) — the two file rows go red on their
// selections.
func TestVarCycle_WorkIsLinearInTheLayers(t *testing.T) {
	fleet := func(t *testing.T, n int, scenario string) string {
		files := map[string]string{
			"vars/_stack.yaml":  "stack:\n  - file: 00-base.yaml\n  - file: \"inc/${ incarnation.id }.yaml\"\n    optional: true\n",
			"vars/00-base.yaml": "maxmemory: 1gb\nport: 6379\nconf: \"port ${ vars.port } maxmemory ${ vars.maxmemory }\"\nreplicas: 1\n",
		}
		for i := range n {
			files[fmt.Sprintf("vars/inc/redis-%04d.yaml", i)] = fmt.Sprintf("maxmemory: %dmb\nport: %d\nreplicas: 2\n", i+100, 6379+i%3)
		}
		if scenario != "" {
			files["scenario/create/main.yml"] = scenario
		}
		return writeServiceTree(t, files)
	}
	// defs is every definition each selection stacks: what a pass over each costs.
	defs := func(t *testing.T, root string, perLayer bool) (selections, total int) {
		layers, _, err := serviceVarLayers(root, true)
		if err != nil {
			t.Fatal(err)
		}
		(&varView{layers: layers, perLayer: perLayer}).eachSelection(func(sel *varSelection) {
			selections++
			for _, name := range sel.names {
				total += len(sel.definers(name, len(layers)-1))
			}
		})
		return selections, total
	}

	t.Run("1500 per-incarnation files", func(t *testing.T) {
		root := fleet(t, 1500, "")
		w := serviceWork(t, root)
		selections, total := defs(t, root, true)
		if w.selections != 1501 || selections != 1501 {
			t.Errorf("want the base, then each of the 1500 files alone over it: %d selections walked, %d listed", w.selections, selections)
		}
		if w.cells > total {
			t.Errorf("%d cells assembled, more than the %d definitions the selections stack", w.cells, total)
		}
		if ds := varCodes(serviceJSON(t, root)); len(ds) != 0 {
			t.Errorf("no var finding, got %+v", ds)
		}
	})

	t.Run("scenario over 1000 files", func(t *testing.T) {
		var tasks strings.Builder
		for i := range 20 {
			fmt.Fprintf(&tasks, "  - name: t%d\n    module: core.file.present\n    vars:\n      local%d: \"${ vars.maxmemory }\"\n"+
				"    params:\n      path: /tmp/x%d\n      content: x\n", i, i, i)
		}
		root := fleet(t, 1000, scenarioTasks("", tasks.String()))
		path := filepath.Join(root, "scenario", "create", "main.yml")
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		scn, _, _, _ := config.LoadScenarioManifestFromBytes(path, src, config.ValidateOptions{})
		if scn == nil {
			t.Fatal("the scenario does not parse")
		}
		done := make(chan varWork, 1)
		go func() { done <- scenarioVarChecks(path, src, scn).work }()
		var w varWork
		select {
		case w = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the scenario's walk has not ended after 10s")
		}
		_, total := defs(t, root, false)
		t.Logf("selections %d, nodes %d, cells %d", w.selections, w.nodes, w.cells)
		if w.selections != 21 {
			t.Errorf("want one stack per view, the run's and 20 tasks', S_all being S_base: %d selections", w.selections)
		}
		if w.cells > 21*(total+1) {
			t.Errorf("%d cells assembled, more than 21 views × %d definitions", w.cells, total+1)
		}
		if ds := varCodes(scenarioDiags(t, root, "create")); len(ds) != 0 {
			t.Errorf("no var finding, got %+v", ds)
		}
	})

	t.Run("500 whole-read vars over 20 layers", func(t *testing.T) {
		const n, k = 500, 20
		var base, whole strings.Builder
		for i := range n {
			fmt.Fprintf(&base, "v%03d: lit\n", i)
			fmt.Fprintf(&whole, "v%03d: \"${ size(string(vars)) }\"\n", i)
		}
		files := map[string]string{"vars/00-base.yaml": base.String()}
		stack := "stack:\n  - file: 00-base.yaml\n"
		for l := 1; l <= k; l++ {
			stack += fmt.Sprintf("  - file: %02d-l.yaml\n    when: \"'c%d' in incarnation.covens\"\n", l, l)
			files[fmt.Sprintf("vars/%02d-l.yaml", l)] = whole.String()
		}
		files["vars/_stack.yaml"] = stack
		root := writeServiceTree(t, files)
		w := serviceWork(t, root)
		_, total := defs(t, root, true)
		// S_all: each name at each of its 21 definers. S_base: each name at the base.
		// Each S_L: each name at the base and at that layer.
		if want := n*(k+1) + n + k*n*2; w.selections != k+2 || w.nodes != want {
			t.Errorf("want %d selections and %d nodes, got %d and %d", k+2, want, w.selections, w.nodes)
		}
		if w.cells > total {
			t.Errorf("%d cells assembled, more than the %d definitions the selections stack", w.cells, total)
		}
		if m := slices.Max(w.rings); m > maxCyclesPerGroup {
			t.Errorf("a group walked %d rings, the bound is %d", m, maxCyclesPerGroup)
		}
		if len(withCode(serviceJSON(t, root), codeVarCycle)) == 0 {
			t.Error("no var_cycle reported")
		}
	})
}

// TestVarChecks_WherePredicateIsRead — a `soulprint.hosts.where("…")` predicate is a
// string the engine inlines before compiling, so a `vars.*` inside it is a read like
// any other: a self-reference through it is a cycle, and a name nothing defines is an
// unknown reference.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make
// servicevars.whereInlinable return false — both go red.
func TestVarChecks_WherePredicateIsRead(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"scenario/create/main.yml": scenarioWithTaskVars("",
			"      peers: \"${ soulprint.hosts.where('vars.peers == 1') }\"\n"+
				"      zone: \"${ soulprint.hosts.where('vars.missing == 1') }\"\n"),
	})
	ds := scenarioDiags(t, root, "create")
	if got := withCode(ds, codeVarCycle); len(got) != 1 || got[0].Message != "var_cycle: peers → peers" {
		t.Errorf("want var_cycle peers → peers, got %+v", got)
	}
	if got := withCode(ds, codeVarUnknownRef); len(got) != 1 || !strings.Contains(got[0].Message, "vars.zone (task vars:) reads vars.missing,") {
		t.Errorf("want var_unknown_ref on zone reading missing, got %+v", got)
	}
	if got := withCode(ds, codeVarInvalid); len(got) != 0 {
		t.Errorf("both compile in a host task, got %+v", got)
	}
}

// TestVarUnknownRef_NeedingAHigherRung — a definition must resolve from its own
// rung and the rungs below: a service var that only works once a task supplies a
// name fails the service view, while a task overriding a service name is fine. The
// scenario rung is held to the same rule against the task rung above it.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: build the service view's
// checker with newVarChecker(false) — "service needs a task name" goes red; or drop
// `l.rung <= rung` from definedAtOrBelow — "scenario judged in a task view" goes red.
func TestVarUnknownRef_NeedingAHigherRung(t *testing.T) {
	t.Run("service needs a task name", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/00-base.yaml":        "acl_file: \"${ vars.conf_dir }/users.acl\"\n",
			"scenario/create/main.yml": scenarioWithTaskVars("", "      conf_dir: /opt/redis\n"),
		})
		got := withCode(serviceDiags(t, root, "redis"), codeVarUnknownRef)
		if len(got) != 1 || !strings.Contains(got[0].Message, "vars.acl_file") || !strings.Contains(got[0].Message, "reads vars.conf_dir,") {
			t.Fatalf("want var_unknown_ref on acl_file reading conf_dir, got %+v", got)
		}
	})
	t.Run("scenario needs a task name", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/00-base.yaml":        "port: 6379\n",
			"scenario/create/main.yml": scenarioWithTaskVars("  acl: \"${ vars.conf_dir }/users.acl\"\n", "      conf_dir: /opt/redis\n"),
		})
		got := withCode(scenarioDiags(t, root, "create"), codeVarUnknownRef)
		if len(got) != 1 || !strings.Contains(got[0].Message, "vars.acl (scenario vars:) reads vars.conf_dir,") {
			t.Fatalf("want var_unknown_ref on the scenario's acl, got %+v", got)
		}
	})
	// The rung decides, not which view a layer is first checked in: judged inside a
	// task's view, the scenario definition still may not need the task's name.
	t.Run("scenario judged in a task view", func(t *testing.T) {
		site := func(string) varSite { return varSite{file: "main.yml", yamlPath: "$.x"} }
		c := newVarChecker(true)
		c.check(&varView{layers: []*varLayer{
			{rung: cel.RungScenario, origin: scenarioRungOrigin, defs: map[string]any{"acl": "${ vars.conf_dir }"}, owned: true, site: site},
			{rung: cel.RungTask, origin: taskRungOrigin, defs: map[string]any{"conf_dir": "/opt"}, owned: true, site: site},
		}})
		if got := withCode(c.out, codeVarUnknownRef); len(got) != 1 {
			t.Fatalf("want var_unknown_ref on the scenario's acl, got %+v", got)
		}
	})
	t.Run("task override is fine", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/00-base.yaml":        "conf_dir: /etc/redis\nacl_file: \"${ vars.conf_dir }/users.acl\"\n",
			"scenario/create/main.yml": scenarioWithTaskVars("  acl: \"${ vars.acl_file }\"\n", "      conf_dir: /opt/redis\n      path: \"${ vars.acl }\"\n"),
		})
		for _, ds := range [][]diag.Diagnostic{serviceDiags(t, root, "redis"), scenarioDiags(t, root, "create")} {
			if got := withCode(ds, codeVarUnknownRef); len(got) != 0 {
				t.Errorf("an override from above is not a need, got %+v", got)
			}
		}
	})
}

// TestVarInvalid_ScenarioRungSeesRunDataOnly — a scenario `vars:` definition
// compiles in the scenario environment, which declares no soulprint and no
// register, so naming either is var_invalid; run data compiles. The same text in a
// host task's own vars compiles: that rung keeps its task's environment.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: give the scenario layer
// engine: taskVarsEngine() in scenarioVarViewDiags — both refusals go red.
func TestVarInvalid_ScenarioRungSeesRunDataOnly(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"scenario/create/main.yml": scenarioWithTaskVars(
			"  host: \"${ soulprint.self.hostname }\"\n"+
				"  probe: \"${ register.p.stdout }\"\n"+
				"  run: \"${ input.size }-${ incarnation.id }-${ compute.n }\"\n",
			"      own_host: \"${ soulprint.self.hostname }\"\n"),
	})
	got := withCode(scenarioDiags(t, root, "create"), codeVarInvalid)
	byName := map[string]diag.Diagnostic{}
	for _, d := range got {
		byName[strings.Fields(d.Message)[0]] = d
	}
	for _, name := range []string{"vars.host", "vars.probe"} {
		d, ok := byName[name]
		if !ok {
			t.Errorf("want var_invalid on %s, got %+v", name, got)
			continue
		}
		if !strings.Contains(d.Message, "(scenario vars:)") || d.YAMLPath != "$."+name {
			t.Errorf("%s: want the scenario rung and $.%s, got %+v", name, name, d)
		}
	}
	if len(got) != 2 {
		t.Errorf("only the two host/probe definitions are invalid, got %+v", got)
	}
}

// TestVarInvalid_StubEvaluationNamesVarAndFile — validate-service evaluates the
// whole service view on a stub incarnation with no traits, so a definition reading
// a trait without has() fails there, and the finding names the var, the file it is
// written in and the cause. --service-name is the stub's service.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in serviceStubDiags cite
// every failure at varsDir instead of resolving the VarError's file — the File
// assertion goes red.
func TestVarInvalid_StubEvaluationNamesVarAndFile(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml":   "port: 6379\nowner: \"${ incarnation.service == 'redis' ? 'redis' : 'nobody' }\"\n",
		"vars/10-traits.yaml": "port: 6380\nenv: \"${ incarnation.traits.env }\"\nsafe_env: \"${ default(incarnation.traits.env, 'dev') }\"\n",
	})
	got := withCode(serviceDiags(t, root, "redis"), codeVarInvalid)
	if len(got) != 1 {
		t.Fatalf("want one var_invalid (env), got %+v", got)
	}
	d := got[0]
	if d.File != filepath.Join(root, "vars", "10-traits.yaml") || d.YAMLPath != "$.env" || d.Line != 2 {
		t.Errorf("want vars/10-traits.yaml:2 $.env, got %s:%d %s", d.File, d.Line, d.YAMLPath)
	}
	for _, part := range []string{"vars.env (vars/10-traits.yaml)", "no such key", `service "redis"`} {
		if !strings.Contains(d.Message, part) {
			t.Errorf("message must carry %q: %s", part, d.Message)
		}
	}
}

// TestVarsDirNested_QuietForAStackNamedFile — a nested file a `_stack.yaml` step can
// include is read, by a literal `file:` or through a `${ … }` wildcard, a leading
// `./` included; one no step can name is still never read and still warned about,
// and a block never stands for a directory.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make stackReach return
// func(string) bool { return false } unconditionally — "literal step" and
// "wildcard step" go red; map a block to `.*` in servicevars.StepFileMatcher —
// "top-level wildcard" goes red; drop its path.Clean — both "dot-slash" rows go red.
func TestVarsDirNested_QuietForAStackNamedFile(t *testing.T) {
	for name, tc := range map[string]struct {
		stack string
		warn  bool
	}{
		"literal step":       {"stack:\n  - file: 00-base.yaml\n  - file: coven/prod.yaml\n", false},
		"wildcard step":      {"stack:\n  - file: 00-base.yaml\n  - foreach: \"${ incarnation.covens }\"\n    as: coven\n    file: \"coven/${ coven }.yaml\"\n    optional: true\n", false},
		"dot-slash wildcard": {"stack:\n  - file: 00-base.yaml\n  - foreach: \"${ incarnation.covens }\"\n    as: coven\n    file: \"./coven/${ coven }.yaml\"\n    optional: true\n", false},
		"dot-slash literal":  {"stack:\n  - file: 00-base.yaml\n  - file: ./coven/prod.yaml\n", false},
		"top-level wildcard": {"stack:\n  - file: 00-base.yaml\n  - foreach: \"${ incarnation.covens }\"\n    as: c\n    file: \"${ c }.yaml\"\n    optional: true\n", true},
		"other step":         {"stack:\n  - file: 00-base.yaml\n  - file: tier/gold.yaml\n", true},
		"no stack":           {"", true},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				"vars/00-base.yaml":    "port: 6379\n",
				"vars/coven/prod.yaml": "tier: gold\n",
				"vars/tier/gold.yaml":  "maxmemory: 1gb\n",
			}
			if tc.stack != "" {
				files["vars/_stack.yaml"] = tc.stack
			}
			got := withCode(serviceDiags(t, writeServiceTree(t, files), "redis"), "vars_dir_nested")
			var coven bool
			for _, d := range got {
				coven = coven || strings.HasSuffix(d.File, filepath.Join("coven", "prod.yaml"))
			}
			if coven != tc.warn {
				t.Errorf("vars/coven/prod.yaml warned = %v, want %v: %+v", coven, tc.warn, got)
			}
		})
	}
}

// TestVarsDirNested_ADeeperFileIsWarnedToo — a layer file two directories down is as
// unread as one a level down when no step names it, and a top-level `${ … }` step
// does not name it.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in nestedVarsDiags list only
// the files directly inside each subdirectory — this goes red.
func TestVarsDirNested_ADeeperFileIsWarnedToo(t *testing.T) {
	for name, stack := range map[string]string{
		"no stack":           "",
		"top-level wildcard": "stack:\n  - foreach: \"${ incarnation.covens }\"\n    as: c\n    file: \"${ c }.yaml\"\n    optional: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				"vars/00-base.yaml":            "port: 6379\n",
				"vars/essence/coven/prod.yaml": "tier: gold\n",
			}
			if stack != "" {
				files["vars/_stack.yaml"] = stack
			}
			got := withCode(serviceDiags(t, writeServiceTree(t, files), "redis"), "vars_dir_nested")
			if len(got) != 1 || !strings.HasSuffix(got[0].File, filepath.Join("essence", "coven", "prod.yaml")) ||
				!strings.Contains(got[0].Message, "vars/essence/coven/prod.yaml is never read") {
				t.Errorf("want one vars_dir_nested on vars/essence/coven/prod.yaml, got %+v", got)
			}
		})
	}
}

// keeperCovenView is the service view the Keeper builds for an incarnation in covens.
func keeperCovenView(t *testing.T, root string, covens ...string) *cel.VarView {
	t.Helper()
	inc := servicevars.IncarnationContext{ID: "redis-prod", Service: "redis", Covens: covens, Traits: map[string]any{}}
	layers, err := servicevars.Layers(os.DirFS(root), inc, nil)
	if err != nil {
		t.Fatalf("servicevars.Layers: %v", err)
	}
	return cel.NewVarView(nil, layers...)
}

// messages lists the messages of ds, in order.
func messages(ds []diag.Diagnostic) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Message)
	}
	return out
}

// TestVarCycle_NothingKnownBelowIsNoCycle — a self-reference finds nothing below only
// where the rungs below are known. A scenario linted without its service, a service
// rung that does not assemble and a vars.yml that does not parse leave them unknown,
// so `port: "${ vars.port + 1 }"` there is no var_cycle, as no read there is judged
// var_unknown_ref. The vars.yml that does not parse is the yaml_parse_error every
// destiny file that does not parse gets.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in varWalk.expand drop
// `!w.v.lowerUnknown` — the three unknown rows go red; in destinyVarViewDiags drop
// the YAMLParseDiagnostic — "vars.yml does not parse" goes red; or in
// config.yamlParseDiag read the position off *yaml.SyntaxError alone again — the
// list and scalar rows go red.
func TestVarCycle_NothingKnownBelowIsNoCycle(t *testing.T) {
	scenario := scenarioWithTaskVars("  conf_dir: \"${ vars.conf_dir }/scn\"\n", "      port: \"${ vars.port + 1 }\"\n")
	t.Run("scenario without its service", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "main.yml")
		if err := os.WriteFile(path, []byte(scenario), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := varCodes(lintJSON(t, Options{Path: path, Kind: KindScenario, ServiceName: "redis"})); len(got) != 0 {
			t.Errorf("no var finding, got %+v", got)
		}
	})
	t.Run("service rung does not assemble", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/00-base.yaml":        "port: 6379\nconf_dir: /etc/redis\n",
			"vars/50-broken.yaml":      "bad: [\n",
			"scenario/create/main.yml": scenario,
		})
		ds := scenarioDiags(t, root, "create")
		if got := withCode(ds, codeVarCycle); len(got) != 0 {
			t.Errorf("no var_cycle, got %+v", got)
		}
		got := withCode(ds, codeVarInvalid)
		if len(got) != 1 || !strings.Contains(got[0].Message, "vars/50-broken.yaml does not parse: ") ||
			strings.Contains(got[0].Message, "\n") || got[0].File != filepath.Join(root, "vars", "50-broken.yaml") || got[0].Line != 1 {
			t.Fatalf("the rung's own finding stays, on one line at vars/50-broken.yaml:1, got %+v", got)
		}
		// A scenario linted by a relative path shows the layer by one, as
		// validate-service does.
		t.Chdir(root)
		rel := lintJSON(t, Options{Path: filepath.Join("scenario", "create", "main.yml"), Kind: KindScenario, ServiceName: "redis"})
		if got := withCode(rel, codeVarInvalid); len(got) != 1 || got[0].File != filepath.Join("vars", "50-broken.yaml") {
			t.Errorf("want the layer at vars/50-broken.yaml, relative, got %+v", got)
		}
		svc := withCode(lintJSON(t, Options{Path: "service.yml", Kind: KindService, ServiceName: "redis"}), codeVarInvalid)
		if len(svc) != 1 || svc[0].File != filepath.Join("vars", "50-broken.yaml") || svc[0].Line != got[0].Line || svc[0].Message != strings.TrimSuffix(got[0].Message,
			"; this scenario's vars are checked without it, and no read of theirs is judged unknown") {
			t.Errorf("validate-service reports the same layer at the same place, got %+v", svc)
		}
	})
	for name, tc := range map[string]struct {
		varsYml, message string
		line, column     int
	}{
		"vars.yml does not parse":  {"port: 6379\nbad: {\n", "", 2, 0},
		"vars.yml is a list":       {"- port\n- conf_dir\n", "sequence was used where mapping is expected", 1, 1},
		"vars.yml is a scalar":     {"just a string\n", "string was used where mapping is expected", 1, 1},
		"vars.yml is a number too": {"42\n", "int was used where mapping is expected", 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeDestinyDir(t, "d", tc.varsYml,
				"- name: t\n  module: core.file.present\n  vars:\n    port: \"${ vars.port + 1 }\"\n"+
					"  params:\n    path: /tmp/x\n    content: \"${ vars.port }\"\n")
			ds := lintJSON(t, Options{Path: path, Kind: KindDestiny})
			if got := varCodes(ds); len(got) != 0 {
				t.Errorf("no var finding, got %+v", got)
			}
			got := withCode(ds, "yaml_parse_error")
			if len(got) != 1 || filepath.Base(got[0].File) != "vars.yml" || got[0].Line != tc.line || got[0].Level != diag.LevelError ||
				(tc.column > 0 && got[0].Column != tc.column) || strings.Contains(got[0].Message, "\n") ||
				(tc.message != "" && got[0].Message != tc.message) {
				t.Errorf("want one yaml_parse_error ERROR at vars.yml:%d:%d on one line %q, got %+v", tc.line, tc.column, tc.message, got)
			}
		})
	}
	t.Run("known, and nothing below", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/00-base.yaml":        "host: localhost\n",
			"scenario/create/main.yml": scenario,
		})
		got := messages(withCode(scenarioDiags(t, root, "create"), codeVarCycle))
		if want := []string{"var_cycle: conf_dir → conf_dir", "var_cycle: port → port"}; !slices.Equal(got, want) {
			t.Errorf("var_cycle = %v, want %v", got, want)
		}
	})
}

// TestVarCycle_ShadowedConditionalLayer — a definition in a conditional layer is
// evaluated by an incarnation that stacks it without the layer that shadows it in
// S_all, so each conditional layer is walked alone over the always-stacked ones too,
// in its S_L. "covenmiss": coven-prod.yaml's self-reference with
// nothing below is shadowed by coven-staging.yaml, and an incarnation in prod alone
// raises it. "supermiss": a `when:`-gated x, shadowed by another one, closes a ring
// through the base's y. Each is checked against the Keeper's view of the incarnation
// that raises it. A definition every incarnation stacks still shadows, and so does a
// present `optional:` file.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make eachSelection return
// after S_all and S_base — "covenmiss" and "supermiss" go red; mark every
// layer conditional there — "an unconditional shadow" goes red; or in
// servicevars.EveryLayer add `|| step.Optional` to Conditional — "a present optional
// file" goes red.
func TestVarCycle_ShadowedConditionalLayer(t *testing.T) {
	for name, tc := range map[string]struct {
		files  map[string]string
		covens []string
		names  []string
		file   string
	}{
		"covenmiss": {
			files: map[string]string{
				"vars/_stack.yaml": "stack:\n  - file: 00-base.yaml\n" +
					"  - foreach: \"${ incarnation.covens }\"\n    as: coven_name\n    file: \"coven-${ coven_name }.yaml\"\n    optional: true\n",
				"vars/00-base.yaml":       "port: 6379\n",
				"vars/coven-prod.yaml":    "tuning: \"${ vars.tuning }-prod\"\n",
				"vars/coven-staging.yaml": "tuning: staging\n",
			},
			covens: []string{"prod"}, names: []string{"tuning"}, file: "coven-prod.yaml",
		},
		"supermiss": {
			files: map[string]string{
				"vars/_stack.yaml": "stack:\n  - file: 00-base.yaml\n" +
					"  - file: 10-a.yaml\n    when: \"'a' in incarnation.covens\"\n" +
					"  - file: 20-b.yaml\n    when: \"'b' in incarnation.covens\"\n",
				"vars/00-base.yaml": "x: 1\ny: \"${ vars.x }\"\n",
				"vars/10-a.yaml":    "x: \"${ vars.y }\"\n",
				"vars/20-b.yaml":    "x: 2\n",
			},
			covens: []string{"a"}, names: []string{"x", "y"}, file: "10-a.yaml",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeServiceTree(t, tc.files)
			want := keeperCycleText(t, keeperCovenView(t, root, tc.covens...), tc.names...)
			got := varCodes(serviceDiags(t, root, "redis"))
			if len(got) != 1 || got[0].Code != codeVarCycle || got[0].Message != want ||
				got[0].File != filepath.Join(root, "vars", tc.file) || got[0].Line != 1 {
				t.Fatalf("want one %q at vars/%s:1, got %+v", want, tc.file, got)
			}
		})
	}
	for name, files := range map[string]map[string]string{
		"an unconditional shadow": {
			"vars/00-base.yaml":  "x: \"${ vars.y }\"\ny: \"${ vars.x }\"\n",
			"vars/10-final.yaml": "x: 1\n",
		},
		// The `when:`-gated step makes a second conditional layer, so the base
		// would be walked without 90-wb.yaml's floor if it were conditional.
		"a present optional file": {
			"vars/_stack.yaml":  "stack:\n  - file: 00-base.yaml\n    optional: true\n  - file: 90-wb.yaml\n  - file: 95-x.yaml\n    when: \"'x' in incarnation.covens\"\n",
			"vars/00-base.yaml": "conf_dir: /etc/redis\n",
			"vars/90-wb.yaml":   "conf_dir: \"${ vars.conf_dir }/wb\"\n",
			"vars/95-x.yaml":    "z: 1\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeServiceTree(t, files)
			for _, covens := range [][]string{nil, {"x"}} {
				view := keeperCovenView(t, root, covens...)
				for _, n := range view.Names() {
					if _, _, err := view.Lookup(n); err != nil {
						t.Fatalf("the Keeper evaluates %s for covens %v: %v", n, covens, err)
					}
				}
			}
			if got := varCodes(serviceDiags(t, root, "redis")); len(got) != 0 {
				t.Errorf("no var finding, got %+v", got)
			}
		})
	}
}

// keeperCycles is every var_cycle text the Keeper raises for some name, for each set
// of covens.
func keeperCycles(t *testing.T, root string, covenSets ...[]string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, covens := range covenSets {
		view := keeperCovenView(t, root, covens...)
		for _, n := range view.Names() {
			var cyc *cel.VarCycleError
			if _, _, err := view.Lookup(n); errors.As(err, &cyc) {
				out[cyc.Error()] = true
			}
		}
	}
	return out
}

// TestVarCycle_ContradictoryLayerChoicesAreNoCycle — inside one stack a read has one
// winner, so a ring that needs x from one conditional layer and x from another at
// once is no ring. "cosel": 10-a.yaml's x reads y, and the only y that reads x is
// 20-b.yaml's, which also sets x, so whichever covens an incarnation is in, x and y
// resolve. "replace" (c5_replace_cond): a `strategy: replace` layer cuts the base's
// x.a, the read that would close the ring; without that layer y is undefined, which
// is the stub's var_invalid, not a cycle. The Keeper raises no cycle for covens [],
// [a], [b] or [a, b].
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in varWalk.expand resolve a
// read to every definer of the stack (range w.sel.definers(name, w.v.top()), an edge
// to each) — both go red.
func TestVarCycle_ContradictoryLayerChoicesAreNoCycle(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"cosel": {
			"vars/_stack.yaml": "stack:\n  - file: 00-base.yaml\n  - file: 10-a.yaml\n    when: \"'a' in incarnation.covens\"\n" +
				"  - file: 20-b.yaml\n    when: \"'b' in incarnation.covens\"\n",
			"vars/00-base.yaml": "x: 1\ny: 1\n",
			"vars/10-a.yaml":    "x: \"${ vars.y }\"\n",
			"vars/20-b.yaml":    "x: 2\ny: \"${ vars.x }\"\n",
		},
		"replace": {
			"vars/_stack.yaml":  "stack:\n  - file: 00-base.yaml\n  - file: 10-r.yaml\n    strategy: replace\n    when: \"'a' in incarnation.covens\"\n",
			"vars/00-base.yaml": "x:\n  a: \"${ vars.y }\"\n",
			"vars/10-r.yaml":    "x:\n  b: 1\ny: \"${ vars.x.b }\"\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeServiceTree(t, files)
			if got := keeperCycles(t, root, nil, []string{"a"}, []string{"b"}, []string{"a", "b"}); len(got) != 0 {
				t.Fatalf("the Keeper raises %v", got)
			}
			if got := withCode(serviceDiags(t, root, "redis"), codeVarCycle); len(got) != 0 {
				t.Errorf("no var_cycle, got %+v", got)
			}
		})
	}
}

// TestVarCycle_EachConditionalLayerAlone — "starve": four conditional layers each
// redefine a, and three of them a and b over each other. S_all holds
// `a → b → a`; only 01-l.yaml alone over the base holds `a → c → a`, which an
// incarnation in c1 raises. Both are reported, once each, in that order, each at
// its first selection, and the rings of the other layers' stacks, which repeat
// `a → b → a`, take nothing from them.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make eachSelection return
// after S_all and S_base — this goes red.
func TestVarCycle_EachConditionalLayerAlone(t *testing.T) {
	files := map[string]string{
		"vars/00-base.yaml": "a: 1\nb: 1\nc: \"${ vars.a }\"\n",
		"vars/01-l.yaml":    "a: \"${ vars.b + vars.c }\"\n",
	}
	stack := "stack:\n  - file: 00-base.yaml\n  - file: 01-l.yaml\n    when: \"'c1' in incarnation.covens\"\n"
	for l := 2; l <= 4; l++ {
		files[fmt.Sprintf("vars/0%d-l.yaml", l)] = "a: \"${ vars.b }\"\nb: \"${ vars.a }\"\n"
		stack += fmt.Sprintf("  - file: 0%d-l.yaml\n    when: \"'c%d' in incarnation.covens\"\n", l, l)
	}
	files["vars/_stack.yaml"] = stack
	root := writeServiceTree(t, files)
	want := []string{"var_cycle: a → b → a", "var_cycle: a → c → a"}
	keeper := keeperCycles(t, root, []string{"c1"}, []string{"c2"}, []string{"c1", "c2", "c3", "c4"})
	for _, w := range want {
		if !keeper[w] {
			t.Fatalf("the Keeper does not raise %q, it raises %v", w, keeper)
		}
	}
	got := withCode(serviceDiags(t, root, "redis"), codeVarCycle)
	if !slices.Equal(messages(got), want) {
		t.Fatalf("var_cycle = %q, want %q", messages(got), want)
	}
	if got[1].File != filepath.Join(root, "vars", "01-l.yaml") || got[1].Line != 1 {
		t.Errorf("want a → c → a at vars/01-l.yaml:1, got %s:%d", got[1].File, got[1].Line)
	}
}

// TestVarInvalid_StubFailureIsSuppressedPerDefinition — the stub evaluation leaves
// out a failure the walk already reported for the same definition, its name and its
// layer, and only that one: a `when:`-gated x the walk flags does not hide the
// base's x, which the stub evaluates and which fails for every incarnation without
// the trait.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make flagKey key a definition
// by its name alone (`return name`) — this goes red.
func TestVarInvalid_StubFailureIsSuppressedPerDefinition(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/_stack.yaml":  "stack:\n  - file: 00-base.yaml\n  - file: 10-c.yaml\n    when: \"'a' in incarnation.covens\"\n",
		"vars/00-base.yaml": "x: \"${ incarnation.traits.env }\"\n",
		"vars/10-c.yaml":    "x: \"${ vars.nope }\"\n",
	})
	got := varCodes(serviceDiags(t, root, "redis"))
	if len(got) != 2 {
		t.Fatalf("want the walk's var_unknown_ref and the stub's var_invalid, got %+v", got)
	}
	if got[0].Code != codeVarUnknownRef || got[0].File != filepath.Join(root, "vars", "10-c.yaml") {
		t.Errorf("want var_unknown_ref in vars/10-c.yaml, got %+v", got[0])
	}
	if got[1].Code != codeVarInvalid || got[1].File != filepath.Join(root, "vars", "00-base.yaml") || got[1].Line != 1 ||
		!strings.Contains(got[1].Message, "no such key") {
		t.Errorf("want the stub's var_invalid at vars/00-base.yaml:1, got %+v", got[1])
	}
}

// TestVarCycle_InvertedKeyTestInATask — a scenario task's `kx: "${ 'kx' in vars ? 1 :
// vars.kx }"` reads kx where the test fails, and with nothing below the Keeper's
// view raises `kx → kx`; the test's own branch stays quiet.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in servicevars readWalk.call
// walk both branches of a key-tested `?:` with w.held — this goes red.
func TestVarCycle_InvertedKeyTestInATask(t *testing.T) {
	defs := map[string]any{"kx": "${ 'kx' in vars ? 1 : vars.kx }", "ko": "${ 'ko' in vars ? vars.ko : 'd' }"}
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 6379\n",
		"scenario/create/main.yml": scenarioWithTaskVars("",
			"      kx: \""+defs["kx"].(string)+"\"\n      ko: \""+defs["ko"].(string)+"\"\n"),
	})
	eng, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	task := cel.NewVarView(keeperServiceView(t, root), cel.VarLayer{Rung: cel.RungTask, Origin: taskRungOrigin, Defs: defs, Engine: eng})
	want := keeperCycleText(t, task, "kx")
	if _, _, err := task.Lookup("ko"); err != nil {
		t.Fatalf("the Keeper evaluates ko: %v", err)
	}
	got := withCode(scenarioDiags(t, root, "create"), codeVarCycle)
	if len(got) != 1 || got[0].Message != want || got[0].YAMLPath != "$.tasks[0].vars.kx" {
		t.Errorf("want one %q at $.tasks[0].vars.kx, got %+v", want, got)
	}
}

// TestVarCycle_StubRingIsNotReportedTwice — a ring the walk and the stub evaluation
// both find is one diagnostic, whatever layer of a name it passes twice the text
// starts at. "both find it": the stub's stack is the walk's, and the ring passes both
// layers of `a`, which the Keeper names once per layer. "det": a ring of the base,
// and a coven file over `a` that reads the layer below. S_all stacks the coven file
// and finds `a → b → c → a → a`, which a prod incarnation raises; S_base, the base
// alone, finds `a → b → c → a`, which an incarnation in no coven raises and the stub
// raises too, so the walk alone reports both and the stub adds nothing. "det through
// a scenario": the same two rings closed by the scenario's `c`, which only the
// scenario reports: validate-scenario walks S_base too. Every text is checked
// against the Keeper's.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in serviceStubDiags look the
// stub's ring up by its text (`static.rings[cyc.Error()]`) — "both find it" goes red;
// or drop S_base from eachSelection — "det" and "det through a scenario" go red.
func TestVarCycle_StubRingIsNotReportedTwice(t *testing.T) {
	t.Run("both find it", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/00-base.yaml": "a: \"${ vars.b }\"\nb: \"${ vars.a }\"\n",
			"vars/10-wb.yaml":   "a: \"${ vars.a }-x\"\n",
		})
		want := []string{keeperCycleText(t, keeperServiceView(t, root), "a")}
		if want[0] != "var_cycle: a → b → a → a" {
			t.Fatalf("the Keeper's text changed: %q", want[0])
		}
		if got := messages(varCodes(serviceDiags(t, root, "redis"))); !slices.Equal(got, want) {
			t.Errorf("var findings = %q, want %q", got, want)
		}
	})
	t.Run("det", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/_stack.yaml": "stack:\n  - file: 00-base.yaml\n" +
				"  - foreach: \"${ incarnation.covens }\"\n    as: cv\n    file: \"c/${ cv }.yaml\"\n    optional: true\n",
			"vars/00-base.yaml": "a: \"${ vars.b }\"\nb: \"${ vars.c }\"\nc: \"${ vars.a }\"\n",
			"vars/c/prod.yaml":  "a: \"${ vars.a }-x\"\n",
		})
		want := []string{
			keeperCycleText(t, keeperCovenView(t, root, "prod"), "a"),
			keeperCycleText(t, keeperCovenView(t, root), "a"),
		}
		if want[0] != "var_cycle: a → b → c → a → a" {
			t.Fatalf("the Keeper's prod text changed: %q", want[0])
		}
		if got := messages(varCodes(serviceVarChecks(root, "redis", false).out)); !slices.Equal(got, want) {
			t.Errorf("the walk's var findings = %q, want %q", got, want)
		}
		if got := messages(varCodes(serviceDiags(t, root, "redis"))); !slices.Equal(got, want) {
			t.Errorf("var findings = %q, want %q", got, want)
		}
	})
	t.Run("det through a scenario", func(t *testing.T) {
		scenarioVars := map[string]any{"c": "${ vars.a }"}
		root := writeServiceTree(t, map[string]string{
			"vars/_stack.yaml": "stack:\n  - file: 00-base.yaml\n" +
				"  - foreach: \"${ incarnation.covens }\"\n    as: cv\n    file: \"c/${ cv }.yaml\"\n    optional: true\n",
			"vars/00-base.yaml":        "a: \"${ vars.b }\"\nb: \"${ default(vars.c, 0) }\"\n",
			"vars/c/prod.yaml":         "a: \"${ vars.a }-x\"\n",
			"scenario/create/main.yml": scenarioWithTaskVars("  c: \""+scenarioVars["c"].(string)+"\"\n", ""),
		})
		eng, err := cel.NewScenarioVars()
		if err != nil {
			t.Fatal(err)
		}
		run := func(covens ...string) *cel.VarView {
			return cel.NewVarView(keeperCovenView(t, root, covens...), cel.VarLayer{
				Rung: cel.RungScenario, Origin: scenarioRungOrigin, Defs: scenarioVars, Engine: eng, Base: scenarioRungScope,
			})
		}
		want := []string{keeperCycleText(t, run("prod"), "c"), keeperCycleText(t, run(), "c")}
		if want[0] != "var_cycle: a → b → c → a → a" || want[1] != "var_cycle: a → b → c → a" {
			t.Fatalf("the Keeper's texts changed: %q", want)
		}
		got := withCode(scenarioDiags(t, root, "create"), codeVarCycle)
		if !slices.Equal(messages(got), want) {
			t.Fatalf("var_cycle = %q, want %q", messages(got), want)
		}
		for _, d := range got {
			if d.YAMLPath != "$.vars.c" {
				t.Errorf("want %q at the scenario's $.vars.c, got %s", d.Message, d.YAMLPath)
			}
		}
		if got := varCodes(serviceDiags(t, root, "redis")); len(got) != 0 {
			t.Errorf("the service alone holds no ring, got %+v", got)
		}
	})
}

// TestVarCycle_OneOfStepFilesAreAlternatives — a step whose composed `file:` has no
// `foreach:` stacks at most one file per incarnation, so its files are alternatives:
// S_all stacks none of them, and each is walked in its own S_L. "idreal":
// inc/a.yaml's maxmemory reads mem and inc/b.yaml's mem reads maxmemory, a ring no
// incarnation builds; "idpair" is the same through default(). The Keeper raises
// nothing for ids a, b or another. A ring inside one per-id file is still reported,
// at that file, as the Keeper raises it for that id.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in eachSelection let S_all
// stack the files of one-of steps again (`every[name] = append(…)` for every layer)
// — "idreal" and "idpair" go red.
func TestVarCycle_OneOfStepFilesAreAlternatives(t *testing.T) {
	const stack = "stack:\n  - file: 00-base.yaml\n  - file: \"inc/${ incarnation.id }.yaml\"\n    optional: true\n"
	idView := func(t *testing.T, root, id string) *cel.VarView {
		t.Helper()
		inc := servicevars.IncarnationContext{ID: id, Service: "redis", Traits: map[string]any{}}
		layers, err := servicevars.Layers(os.DirFS(root), inc, nil)
		if err != nil {
			t.Fatalf("servicevars.Layers: %v", err)
		}
		return cel.NewVarView(nil, layers...)
	}
	for name, files := range map[string]map[string]string{
		"idreal": {
			"vars/00-base.yaml": "mem: 1gb\nmaxmemory: 800mb\n",
			"vars/inc/a.yaml":   "maxmemory: \"${ vars.mem }-headroom\"\n",
			"vars/inc/b.yaml":   "mem: \"${ vars.maxmemory }+headroom\"\n",
		},
		"idpair": {
			"vars/00-base.yaml": "port: 1\n",
			"vars/inc/a.yaml":   "x: \"${ default(vars.y, 1) }\"\n",
			"vars/inc/b.yaml":   "y: \"${ default(vars.x, 2) }\"\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			files["vars/_stack.yaml"] = stack
			root := writeServiceTree(t, files)
			for _, id := range []string{"a", "b", "redis-prod"} {
				view := idView(t, root, id)
				for _, n := range view.Names() {
					if _, _, err := view.Lookup(n); err != nil {
						t.Fatalf("the Keeper fails %s for id %s: %v", n, id, err)
					}
				}
			}
			if got := varCodes(serviceDiags(t, root, "redis")); len(got) != 0 {
				t.Errorf("no var finding, got %+v", got)
			}
		})
	}
	t.Run("a ring inside one file", func(t *testing.T) {
		root := writeServiceTree(t, map[string]string{
			"vars/_stack.yaml":  stack,
			"vars/00-base.yaml": "port: 1\n",
			"vars/inc/a.yaml":   "p: 1\n",
			"vars/inc/c.yaml":   "p: \"${ vars.q }\"\nq: \"${ vars.p }\"\n",
		})
		want := keeperCycleText(t, idView(t, root, "c"), "p")
		got := varCodes(serviceDiags(t, root, "redis"))
		if len(got) != 1 || got[0].Message != want || got[0].File != filepath.Join(root, "vars", "inc", "c.yaml") || got[0].Line != 1 {
			t.Errorf("want one %q at vars/inc/c.yaml:1, got %+v", want, got)
		}
	})
}

// TestVarCycle_ADeadReadIsNoEdge — a read whose result a bool literal decides is no
// edge: in the branch of `?:` a literal condition never takes, CEL never evaluates
// it, and in a term of `&&` (`||`) beside a literal `false` (`true`) it drops its
// error, a cycle's included. x reads y in each row and y reads x; each row is checked
// against the Keeper's view first, which raises the ring only where the read is
// live. A dead read of a name nothing defines is no var_unknown_ref either. A whole
// comparison is the exception: the Keeper materializes the view before it evaluates an
// expression that may compare the root, whatever decides the result, so it is a whole
// read in a dead position too, and so is one through merge(). Each row is a service
// file's x and y, and a scenario's: validate-service's stub evaluation would report a
// ring the walk misses, and validate-scenario has none.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code, any of: in servicevars
// readWalk.read record a read in a vacuous place again (`w.reads[name] = true` ahead
// of the vacuous check) — the dead rows reading a name go red; drop `&& w.vacuous ==
// 0` from readWalk.expr's IdentKind case — "true ? 1 : vars" goes red; or return
// `w.whole` alone from ExprReads — the scenario's dead comparison rows go red.
func TestVarCycle_ADeadReadIsNoEdge(t *testing.T) {
	scenarioEngine, err := cel.NewScenarioVars()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		def   string
		cycle bool
	}{
		{"true ? 1 : vars.y", false},
		{"false ? vars.y : 1", false},
		{"!true ? vars.y : 1", false},
		{"false && vars.y", false},
		{"true || vars.y", false},
		{"vars.y && false", false},
		{"vars.y || true", false},
		{"(false && vars.y) ? 1 : 2", false},
		{"true ? 1 : vars", false},
		{"true ? 1 : vars.nope", false},
		{"true ? 1 : merge({'k': vars}).k", false},
		{"true || [vars][0].y == 1", false},
		{"false && vars != {}", true},
		{"true || [vars].exists(m, m != {})", true},
		{"true ? 1 : (merge({'k': vars}).k == {} ? 2 : 3)", true},
		{"merge({'k': vars}).k != {}", true},
		{"false ? 1 : vars.y", true},
		{"true ? vars.y : 1", true},
		{"true && vars.y", true},
		{"false || vars.y", true},
		{"false ? 1 : vars", true},
	} {
		check := func(t *testing.T, view *cel.VarView, got []diag.Diagnostic) {
			t.Helper()
			var keeper error
			for _, n := range view.Names() {
				if _, _, err := view.Lookup(n); err != nil && keeper == nil {
					keeper = err
				}
			}
			if errors.Is(keeper, cel.ErrVarCycle) != tc.cycle || (!tc.cycle && keeper != nil) {
				t.Fatalf("the Keeper raises %v; the row says cycle = %v", keeper, tc.cycle)
			}
			switch {
			case tc.cycle && (len(got) == 0 || got[0].Code != codeVarCycle || got[0].Message != keeper.Error()):
				t.Fatalf("want the Keeper's %q first, got %+v", keeper, got)
			case !tc.cycle && len(got) != 0:
				t.Fatalf("no var finding, got %+v", got)
			}
		}
		x := "${ " + tc.def + " }"
		t.Run("service/"+tc.def, func(t *testing.T) {
			root := writeServiceTree(t, map[string]string{"vars/00-base.yaml": "x: \"" + x + "\"\ny: \"${ vars.x }\"\n"})
			check(t, keeperServiceView(t, root), varCodes(serviceDiags(t, root, "redis")))
		})
		t.Run("scenario/"+tc.def, func(t *testing.T) {
			root := writeServiceTree(t, map[string]string{
				"vars/00-base.yaml":        "port: 1\n",
				"scenario/create/main.yml": scenarioWithTaskVars("  x: \""+x+"\"\n  y: \"${ vars.x }\"\n", ""),
			})
			view := cel.NewVarView(keeperServiceView(t, root), cel.VarLayer{
				Rung: cel.RungScenario, Origin: scenarioRungOrigin, Defs: map[string]any{"x": x, "y": "${ vars.x }"},
				Engine: scenarioEngine, Base: scenarioRungScope,
			})
			check(t, view, varCodes(scenarioDiags(t, root, "create")))
		})
	}
}

// TestVarCycle_CitesTheCellThatReads — a ring through a map merged deep is cited at
// the string that reads the next name, in the layer that wrote it: deep3's m reads p
// through m.a of 00-base.yaml, under 10-mid.yaml's m.b and 20-top.yaml's m.c, which
// read nothing of the ring. The hint names that layer too.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in reportCycle cite each node
// at its bound (`at[i] = varLeaf{layer: rotated[i].bound}`) — this goes red.
func TestVarCycle_CitesTheCellThatReads(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "m:\n  a: \"${ vars.p }\"\n  b: 1\np: \"${ vars.m.c }\"\n",
		"vars/10-mid.yaml":  "m:\n  b: \"${ vars.q }\"\nq: 1\n",
		"vars/20-top.yaml":  "m:\n  c: 3\n",
	})
	want := keeperCycleText(t, keeperServiceView(t, root), "m")
	got := varCodes(serviceDiags(t, root, "redis"))
	if len(got) != 1 || got[0].Message != want || got[0].File != filepath.Join(root, "vars", "00-base.yaml") ||
		got[0].YAMLPath != "$.m.a" || got[0].Line != 2 {
		t.Fatalf("want one %q at vars/00-base.yaml:2 $.m.a, got %+v", want, got)
	}
	if !strings.Contains(got[0].Hint, "vars.m (vars/00-base.yaml)") {
		t.Errorf("want the hint to name m in vars/00-base.yaml, got %q", got[0].Hint)
	}
}

// TestRingKey_IsTheRingWhicheverRotation — two renderings of one ring that start at
// different layers of a name it passes twice are one key; another ring is another.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make ringKey join names as
// given — this goes red.
func TestRingKey_IsTheRingWhicheverRotation(t *testing.T) {
	if ringKey([]string{"x", "y", "x", "z"}) != ringKey([]string{"x", "z", "x", "y"}) {
		t.Error("one ring, two keys")
	}
	if ringKey([]string{"x", "y", "z"}) == ringKey([]string{"x", "z", "y"}) {
		t.Error("two rings, one key")
	}
}

// TestVarCycle_StubSelfCycleCitesTheLowestDefinition — a self-reference with nothing
// below only the stub evaluation finds, through an index with a non-literal key, is
// cited at the definition that finds nothing below: the lowest, not the one
// shadowing it.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in stubCycleSite take a
// one-name path for the self-reference (`len(cyc.Path) == 1`, forgetting that the
// path is closed), so the top definition is cited — this goes red.
func TestVarCycle_StubSelfCycleCitesTheLowestDefinition(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "port: 1\nx: \"${ vars['x' + ''] }\"\n",
		"vars/10-wb.yaml":   "x: \"${ vars.x }-wb\"\n",
	})
	want := keeperCycleText(t, keeperServiceView(t, root), "x")
	got := varCodes(serviceDiags(t, root, "redis"))
	if len(got) != 1 || got[0].Message != want || got[0].File != filepath.Join(root, "vars", "00-base.yaml") || got[0].Line != 2 {
		t.Errorf("want one %q at vars/00-base.yaml:2, got %+v", want, got)
	}
}

// TestVarChecks_OrderIsFixed — a tree with findings of every var code, from the walk
// and from the stub evaluation, gives the same list in the same order on every run,
// byte for byte.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in compile() range l.defs
// instead of its sorted names — this goes red within the repeats.
func TestVarChecks_OrderIsFixed(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "a: \"${ vars.b }\"\nb: \"${ vars.a }\"\ns: \"${ vars.s }\"\n" +
			"t: \"${ vars.nope }\"\nu: \"${ vars.nope2 }\"\n" +
			"h: \"${ soulprint.self.hostname }\"\nh2: \"${ register.p.x }\"\n" +
			"env: \"${ incarnation.traits.env }\"\ntier: \"${ incarnation.traits.tier }\"\n",
		"vars/10-more.yaml": "p: \"${ vars.q }\"\nq: \"${ vars.p }\"\n",
	})
	run := func() string {
		var out, errOut bytes.Buffer
		Run(Options{Path: filepath.Join(root, "service.yml"), Kind: KindService, ServiceName: "redis", JSON: true}, &out, &errOut)
		return out.String()
	}
	want := []string{
		"var_cycle 00-base.yaml:3 var_cycle: s → s",
		"var_cycle 00-base.yaml:1 var_cycle: a → b → a",
		"var_cycle 10-more.yaml:1 var_cycle: p → q → p",
		"var_unknown_ref 00-base.yaml:4 vars.t (vars/00-base.yaml) reads vars.nope, which neither its own rung nor a rung below it defines",
		"var_unknown_ref 00-base.yaml:5 vars.u (vars/00-base.yaml) reads vars.nope2, which neither its own rung nor a rung below it defines",
		"var_invalid 00-base.yaml:6",
		"var_invalid 00-base.yaml:7",
		"var_invalid 00-base.yaml:8",
		"var_invalid 00-base.yaml:9",
	}
	var got []string
	for _, d := range varCodes(lintJSON(t, Options{Path: filepath.Join(root, "service.yml"), Kind: KindService, ServiceName: "redis"})) {
		line := fmt.Sprintf("%s %s:%d", d.Code, filepath.Base(d.File), d.Line)
		if d.Code != codeVarInvalid {
			line += " " + d.Message
		}
		got = append(got, line)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("findings, in order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	first := run()
	for range 20 {
		if again := run(); again != first {
			t.Fatalf("a second run differs:\n%s\nfirst:\n%s", again, first)
		}
	}
}
