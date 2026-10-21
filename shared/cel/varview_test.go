package cel

import (
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newServiceVarsEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewServiceVars()
	if err != nil {
		t.Fatalf("NewServiceVars: %v", err)
	}
	return e
}

func newScenarioVarsEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewScenarioVars()
	if err != nil {
		t.Fatalf("NewScenarioVars: %v", err)
	}
	return e
}

var testIncarnation = map[string]any{"id": "inc-1", "service": "redis"}

func serviceLayer(e *Engine, origin string, defs map[string]any) VarLayer {
	return VarLayer{Rung: RungService, Origin: origin, Defs: defs, Engine: e, Base: Vars{Incarnation: testIncarnation}}
}

func taskLayer(e *Engine, defs map[string]any) VarLayer {
	return VarLayer{Rung: RungTask, Origin: "task vars:", Defs: defs, Engine: e, Base: Vars{Incarnation: testIncarnation}}
}

func mustLookup(t *testing.T, v *VarView, name string) any {
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

// TestVarView_CycleTextIsTheSameForEveryRequest — point 10: the cycle is rotated to
// its smallest name, so the text does not depend on which var was asked for first,
// and the marker survives prg.Eval and ErrEval for a cell that reads the cycle.
func TestVarView_CycleTextIsTheSameForEveryRequest(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	const want = "var_cycle: a → b → c → a"

	cases := []struct {
		name  string
		build func() *VarView
	}{
		{"one layer", func() *VarView {
			return NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
				"a": "${ vars.b }", "b": "${ vars.c }", "c": "${ vars.a }", "x": "${ vars.b }-x",
			}))
		}},
		{"across rungs", func() *VarView {
			parent := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
				"a": "${ vars.b }", "x": "${ vars.b }-x",
			}))
			return NewVarView(parent, taskLayer(task, map[string]any{
				"b": "${ vars.c }", "c": "${ vars.a }",
			}))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, req := range []string{"a", "b", "c", "x"} {
				_, _, err := tc.build().Lookup(req)
				if !errors.Is(err, ErrVarCycle) {
					t.Fatalf("Lookup(%q) = %v, want ErrVarCycle", req, err)
				}
				if err.Error() != want {
					t.Errorf("Lookup(%q) text = %q, want %q", req, err.Error(), want)
				}
			}

			_, err := task.EvalExpression("vars.b", Vars{View: tc.build()})
			if !errors.Is(err, ErrVarCycle) {
				t.Fatalf("EvalExpression(vars.b) = %v, want ErrVarCycle through ErrEval", err)
			}
			var cyc *VarCycleError
			if !errors.As(err, &cyc) || cyc.Error() != want {
				t.Fatalf("EvalExpression(vars.b): VarCycleError = %v, want %q", cyc, want)
			}
		})
	}

	// a@1 (90-wb) reads a@0 (00-base) by self-reference, a@0 reads c, c reads a@1: the
	// ring holds a twice and starts at a@0, whichever name was asked for.
	t.Run("a name on the ring twice starts at its lowest bound", func(t *testing.T) {
		const want = "var_cycle: a → c → a → a"
		for _, req := range []string{"a", "c"} {
			v := NewVarView(nil,
				serviceLayer(svc, "vars/00-base.yaml", map[string]any{"a": "${ vars.c }", "c": "${ vars.a }"}),
				serviceLayer(svc, "vars/90-wb.yaml", map[string]any{"a": "${ vars.a }-x"}),
			)
			_, _, err := v.Lookup(req)
			if !errors.Is(err, ErrVarCycle) || err.Error() != want {
				t.Fatalf("Lookup(%q) = %v, want %q", req, err, want)
			}
		}
	})
}

// TestVarView_DeterministicUnderRandomRequestOrder — point 10: the same values for any
// order of requests, and whether or not the parent view read a name first; and here,
// where each failing name can fail only one way, the same error texts.
func TestVarView_DeterministicUnderRandomRequestOrder(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	build := func() (*VarView, *VarView) {
		parent := NewVarView(nil,
			serviceLayer(svc, "vars/00-base.yaml", map[string]any{
				"base":        "/srv",
				"conf_dir":    "${ vars.base }/etc",
				"acl_file":    "${ vars.conf_dir }/users.acl",
				"port":        6379,
				"endpoint":    "${ incarnation.id }:${ vars.port }",
				"broken":      "${ vars.missing }",
				"uses_broken": "${ vars.broken }-x",
				"cyc_a":       "${ vars.cyc_b }",
				"cyc_b":       "${ vars.cyc_a }",
			}),
			serviceLayer(svc, "vars/10-wb.yaml", map[string]any{
				"conf_dir": "${ vars.conf_dir }/wb",
				"tls":      map[string]any{"enabled": true, "cert": "${ vars.conf_dir }/tls.crt"},
			}),
		)
		child := NewVarView(parent, taskLayer(task, map[string]any{
			"port":    7000,
			"summary": "${ vars.endpoint } ${ vars.acl_file }",
			"list":    []any{"${ vars.port }", "${ vars.base }"},
			"count":   "${ size(vars) }",
		}))
		return parent, child
	}

	type outcome struct {
		value any
		err   string
	}
	run := func(order []string, rng *rand.Rand) map[string]outcome {
		parent, child := build()
		out := make(map[string]outcome, len(order))
		for _, name := range order {
			if rng.Intn(3) == 0 {
				_, _, _ = parent.Lookup(name)
			}
			val, _, err := child.Lookup(name)
			o := outcome{value: val}
			if err != nil {
				o.err = err.Error()
			}
			out[name] = o
		}
		return out
	}

	_, child := build()
	names := child.Names()
	if len(names) != 13 {
		t.Fatalf("Names() = %v, want 13 names", names)
	}
	want := run(names, rand.New(rand.NewSource(0)))

	for name, val := range map[string]any{
		"acl_file": "/srv/etc/wb/users.acl",
		"endpoint": "inc-1:7000",
		"summary":  "inc-1:7000 /srv/etc/wb/users.acl",
		"list":     []any{int64(7000), "/srv"},
		"count":    int64(13),
		"tls":      map[string]any{"enabled": true, "cert": "/srv/etc/wb/tls.crt"},
	} {
		if got := want[name]; got.err != "" || !reflect.DeepEqual(got.value, val) {
			t.Fatalf("%s = %#v (err %q), want %#v", name, got.value, got.err, val)
		}
	}
	if want["broken"].err == "" || want["uses_broken"].err != want["broken"].err {
		t.Fatalf("uses_broken must report broken's own error: broken %q, uses_broken %q", want["broken"].err, want["uses_broken"].err)
	}
	if got := want["cyc_b"].err; got != "var_cycle: cyc_a → cyc_b → cyc_a" {
		t.Fatalf("cyc_b error = %q", got)
	}

	rng := rand.New(rand.NewSource(1014))
	for i := range 50 {
		order := slices.Clone(names)
		rng.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
		got := run(order, rng)
		for _, name := range names {
			if !reflect.DeepEqual(got[name], want[name]) {
				t.Fatalf("order #%d %v: %s = %#v, want %#v", i, order, name, got[name], want[name])
			}
		}
	}
}

// TestVarView_UnrequestedDefinitionIsNotEvaluated — point 4: a name nobody reads is
// never evaluated, a broken one included, and the counter proves the skip.
func TestVarView_UnrequestedDefinitionIsNotEvaluated(t *testing.T) {
	v := NewVarView(nil, serviceLayer(newServiceVarsEngine(t), "vars/00-base.yaml", map[string]any{
		"a":          "${ vars.b }-a",
		"b":          "b",
		"c":          "c",
		"broken":     "${ vars.missing }",
		"bad_syntax": "${ 1 + }",
	}))
	var evaluated []string
	v.onEvaluate = func(name string, _ int) { evaluated = append(evaluated, name) }

	if got := mustLookup(t, v, "a"); got != "b-a" {
		t.Fatalf("a = %#v, want b-a", got)
	}
	slices.Sort(evaluated)
	if v.Evaluations() != 2 || !slices.Equal(evaluated, []string{"a", "b"}) {
		t.Fatalf("Evaluations() = %d, evaluated %v; want 2, [a b]", v.Evaluations(), evaluated)
	}

	if !v.Has("broken") || len(v.Names()) != 5 {
		t.Fatalf("Has(broken) = %v, Names() = %v", v.Has("broken"), v.Names())
	}
	if _, found, err := v.Lookup("absent"); found || err != nil {
		t.Fatalf("Lookup(absent) = found %v, err %v; want not found, no error", found, err)
	}
	if v.Evaluations() != 2 {
		t.Fatalf("Has/Names/absent Lookup evaluated something: Evaluations() = %d", v.Evaluations())
	}

	_, _, err := v.Lookup("broken")
	var ve *VarError
	if !errors.As(err, &ve) || ve.Name != "broken" || ve.Origin != "vars/00-base.yaml" {
		t.Fatalf("Lookup(broken) = %v, want a VarError for broken from vars/00-base.yaml", err)
	}
	if v.Evaluations() != 3 {
		t.Fatalf("Evaluations() = %d, want 3", v.Evaluations())
	}
}

// TestVarView_LateBindingReadsTheHighestDefinition — point 5: every definition reads
// `vars.<other>` through the reading view, so a lower definition sees a higher
// override, whichever of the two is declared lower.
func TestVarView_LateBindingReadsTheHighestDefinition(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	acl := map[string]any{"acl_file": "${ vars.conf_dir }/users.acl"}
	cases := []struct {
		name string
		view func() *VarView
	}{
		{"a service var reads a task override", func() *VarView {
			parent := NewVarView(nil, serviceLayer(svc, "vars/20-host-layout.yaml", map[string]any{
				"conf_dir": "/etc/redis", "acl_file": "${ vars.conf_dir }/users.acl",
			}))
			return NewVarView(parent, taskLayer(task, map[string]any{"conf_dir": "/opt/redis"}))
		}},
		{"a task var reads a service var", func() *VarView {
			parent := NewVarView(nil, serviceLayer(svc, "vars/20-host-layout.yaml", map[string]any{"conf_dir": "/opt/redis"}))
			return NewVarView(parent, taskLayer(task, acl))
		}},
		{"an earlier file reads a later one", func() *VarView {
			return NewVarView(nil,
				serviceLayer(svc, "vars/00-base.yaml", map[string]any{"conf_dir": "/etc/redis", "acl_file": "${ vars.conf_dir }/users.acl"}),
				serviceLayer(svc, "vars/90-wb.yaml", map[string]any{"conf_dir": "/opt/redis"}),
			)
		}},
		{"a later file reads an earlier one", func() *VarView {
			return NewVarView(nil,
				serviceLayer(svc, "vars/00-base.yaml", map[string]any{"conf_dir": "/opt/redis"}),
				serviceLayer(svc, "vars/90-wb.yaml", acl),
			)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustLookup(t, tc.view(), "acl_file"); got != "/opt/redis/users.acl" {
				t.Fatalf("acl_file = %#v, want /opt/redis/users.acl", got)
			}
		})
	}

	parent := NewVarView(nil, serviceLayer(svc, "vars/20-host-layout.yaml", map[string]any{
		"conf_dir": "/etc/redis", "acl_file": "${ vars.conf_dir }/users.acl",
	}))
	_ = NewVarView(parent, taskLayer(task, map[string]any{"conf_dir": "/opt/redis"}))
	if got := mustLookup(t, parent, "acl_file"); got != "/etc/redis/users.acl" {
		t.Fatalf("the service view itself: acl_file = %#v, want /etc/redis/users.acl", got)
	}
}

// TestVarView_SelfReferenceReadsTheLayerBelow — point 9: `vars.N` inside N's own
// definition reads N as the layers strictly below the contributing one define it,
// within a rung and across rungs; nothing below is var_cycle N → N.
func TestVarView_SelfReferenceReadsTheLayerBelow(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	base := func() VarLayer {
		return serviceLayer(svc, "vars/00-base.yaml", map[string]any{"conf_dir": "/etc/redis"})
	}
	wb := func() VarLayer {
		return serviceLayer(svc, "vars/90-wb.yaml", map[string]any{"conf_dir": "${ vars.conf_dir }/wb"})
	}
	cases := []struct {
		name string
		view func() *VarView
		want string
	}{
		{"two files of one rung", func() *VarView { return NewVarView(nil, base(), wb()) }, "/etc/redis/wb"},
		{"a task over the service", func() *VarView {
			return NewVarView(NewVarView(nil, base()), taskLayer(task, map[string]any{"conf_dir": "${ vars.conf_dir }/wb"}))
		}, "/etc/redis/wb"},
		{"three layers", func() *VarView {
			return NewVarView(NewVarView(nil, base(), wb()), taskLayer(task, map[string]any{"conf_dir": "${ vars.conf_dir }/x"}))
		}, "/etc/redis/wb/x"},
		{"through default()", func() *VarView {
			return NewVarView(NewVarView(nil, base()), taskLayer(task, map[string]any{"conf_dir": "${ default(vars.conf_dir, '/d') }/wb"}))
		}, "/etc/redis/wb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustLookup(t, tc.view(), "conf_dir"); got != tc.want {
				t.Fatalf("conf_dir = %#v, want %q", got, tc.want)
			}
		})
	}

	t.Run("the service view keeps its own value", func(t *testing.T) {
		parent := NewVarView(nil, base())
		child := NewVarView(parent, taskLayer(task, map[string]any{"conf_dir": "${ vars.conf_dir }/wb"}))
		_ = mustLookup(t, child, "conf_dir")
		if got := mustLookup(t, parent, "conf_dir"); got != "/etc/redis" {
			t.Fatalf("parent conf_dir = %#v, want /etc/redis", got)
		}
	})

	for _, tc := range []struct {
		name    string
		varName string
		defs    map[string]any
	}{
		{"nothing below", "conf_dir", map[string]any{"conf_dir": "${ vars.conf_dir }/wb"}},
		{"a sibling key of the same file", "creds", map[string]any{"creds": map[string]any{"name": "a", "user": "${ vars.creds.name }"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", tc.defs)).Lookup(tc.varName)
			want := "var_cycle: " + tc.varName + " → " + tc.varName
			if !errors.Is(err, ErrVarCycle) || err.Error() != want {
				t.Fatalf("Lookup = %v, want %q", err, want)
			}
		})
	}
}

// TestVarView_ParentValueIsReusedUnlessAnOverrideTouchesIt — point 8: a service var
// no override touches is evaluated once however many child views read it; a child
// that overrides a name the var read evaluates it again, with its own value.
func TestVarView_ParentValueIsReusedUnlessAnOverrideTouchesIt(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	service := func() *VarView {
		return NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
			"conf_dir": "/etc/redis",
			"acl_file": "${ vars.conf_dir }/users.acl",
			"port":     6379,
			"endpoint": "${ incarnation.id }:${ vars.port }",
		}))
	}
	child := func(parent *VarView) *VarView {
		return NewVarView(parent, taskLayer(task, map[string]any{"unrelated": "x", "port": 7000}))
	}
	const n = 5

	t.Run("the service view read it first", func(t *testing.T) {
		parent := service()
		_ = mustLookup(t, parent, "acl_file")
		if parent.Evaluations() != 2 {
			t.Fatalf("parent Evaluations() = %d, want 2", parent.Evaluations())
		}
		for i := range n {
			c := child(parent)
			if got := mustLookup(t, c, "acl_file"); got != "/etc/redis/users.acl" {
				t.Fatalf("child %d: acl_file = %#v", i, got)
			}
			if c.Evaluations() != 0 {
				t.Fatalf("child %d evaluated %d definitions, want 0", i, c.Evaluations())
			}
		}
		if parent.Evaluations() != 2 {
			t.Fatalf("parent Evaluations() = %d after %d child reads, want 2", parent.Evaluations(), n)
		}
	})

	t.Run("the children read it first", func(t *testing.T) {
		parent := service()
		var total int64
		for range n {
			c := child(parent)
			_ = mustLookup(t, c, "acl_file")
			total += c.Evaluations()
		}
		_ = mustLookup(t, parent, "acl_file")
		if total += parent.Evaluations(); total != 2 {
			t.Fatalf("acl_file and conf_dir evaluated %d times across %d children and the parent, want 2", total, n)
		}
	})

	t.Run("an override re-evaluates", func(t *testing.T) {
		parent := service()
		if got := mustLookup(t, parent, "endpoint"); got != "inc-1:6379" {
			t.Fatalf("parent endpoint = %#v", got)
		}
		c := child(parent)
		var evaluated []string
		c.onEvaluate = func(name string, _ int) { evaluated = append(evaluated, name) }
		if got := mustLookup(t, c, "endpoint"); got != "inc-1:7000" {
			t.Fatalf("child endpoint = %#v, want inc-1:7000", got)
		}
		if !slices.Contains(evaluated, "endpoint") {
			t.Fatalf("the child reused the parent's endpoint: evaluated %v", evaluated)
		}
		before := parent.Evaluations()
		if got := mustLookup(t, parent, "endpoint"); got != "inc-1:6379" || parent.Evaluations() != before {
			t.Fatalf("parent endpoint = %#v after the child (evaluations %d → %d)", got, before, parent.Evaluations())
		}
	})
}

// TestVarView_AnAbsentReadOrAnInTestBlocksParentReuse — point 8: a read records its
// name found or absent, and so does `'x' in vars`, so a parent value that saw o absent
// is not this child's value once the child defines o.
func TestVarView_AnAbsentReadOrAnInTestBlocksParentReuse(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	names := []string{"tls", "p"}
	for _, parentFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("parent reads first %v", parentFirst), func(t *testing.T) {
			parent := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
				"tls": "${ default(vars.o, false) }",
				"p":   "${ 'o' in vars }",
			}))
			child := NewVarView(parent, taskLayer(task, map[string]any{"o": true}))
			if parentFirst {
				for _, name := range names {
					if got := mustLookup(t, parent, name); got != false {
						t.Fatalf("parent %s = %#v, want false", name, got)
					}
				}
			}
			before := parent.Evaluations()

			var evaluated []string
			child.onEvaluate = func(name string, _ int) { evaluated = append(evaluated, name) }
			for _, name := range names {
				if got := mustLookup(t, child, name); got != true {
					t.Fatalf("child %s = %#v, want true", name, got)
				}
			}
			slices.Sort(evaluated)
			if !slices.Equal(evaluated, []string{"o", "p", "tls"}) {
				t.Fatalf("child evaluated %v, want [o p tls]: a parent value was reused", evaluated)
			}

			for _, name := range names {
				if got := mustLookup(t, parent, name); got != false {
					t.Fatalf("parent %s = %#v after the child, want false", name, got)
				}
			}
			if parentFirst && parent.Evaluations() != before {
				t.Fatalf("parent re-evaluated after the child: %d → %d", before, parent.Evaluations())
			}
		})
	}
}

// TestVarView_AWholeReadIsReusedOnlyWhereUpperRungsDefineNothing — point 8: a value that
// read the whole namespace changes with any name a child adds, so it is neither reused
// by such a child nor handed up from it.
func TestVarView_AWholeReadIsReusedOnlyWhereUpperRungsDefineNothing(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	service := func() *VarView {
		return NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{"a": 1, "n": "${ size(vars) }"}))
	}
	for _, parentFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("parent reads first %v", parentFirst), func(t *testing.T) {
			parent := service()
			child := NewVarView(parent, taskLayer(task, map[string]any{"extra": "x"}))
			if parentFirst {
				if got := mustLookup(t, parent, "n"); got != int64(2) {
					t.Fatalf("parent n = %#v, want 2", got)
				}
			}
			if got := mustLookup(t, child, "n"); got != int64(3) {
				t.Fatalf("child n = %#v, want the parent's 2 + 1", got)
			}
			if got := mustLookup(t, parent, "n"); got != int64(2) {
				t.Fatalf("parent n = %#v after the child, want 2", got)
			}
		})
	}

	t.Run("a child that defines nothing reuses it", func(t *testing.T) {
		parent := service()
		_ = mustLookup(t, parent, "n")
		child := NewVarView(parent, taskLayer(task, map[string]any{}))
		if got := mustLookup(t, child, "n"); got != int64(2) || child.Evaluations() != 0 {
			t.Fatalf("child n = %#v after %d evaluations, want the parent's 2 after 0", got, child.Evaluations())
		}
	})
}

// TestVarView_PresenceEvaluatesKeyOnlyOperationsDoNot — point 4: has(), default() and
// both index forms evaluate their operand and raise its error, at the top level and
// inside a definition; size(vars), `in` and an iteration over the keys evaluate nothing,
// and neither does a comparison of something other than the whole `vars`.
func TestVarView_PresenceEvaluatesKeyOnlyOperationsDoNot(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	view := func() *VarView {
		return NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
			"ok":         "v",
			"broken":     "${ vars.missing }",
			"idx":        "${ vars['ok'] }-i",
			"dyn_idx":    "${ vars['o' + 'k'] }-e",
			"idx_broken": "${ vars['bro' + 'ken'] }",
		}))
	}

	for _, expr := range []string{
		"has(vars.broken)",
		"default(vars.broken, 'd')",
		"vars['broken']",
		"vars['bro' + 'ken']",
		"vars.idx_broken",
	} {
		t.Run(expr, func(t *testing.T) {
			_, err := task.EvalExpression(expr, Vars{View: view()})
			var ve *VarError
			if !errors.As(err, &ve) || ve.Name != "broken" {
				t.Fatalf("%s = %v, want broken's VarError", expr, err)
			}
		})
	}

	for _, tc := range []struct {
		expr string
		want any
		// evals is what the cell evaluates: zero for the key-only operations.
		evals int64
	}{
		{"has(vars.absent)", false, 0},
		{"default(vars.absent, 'd')", "d", 0},
		{"default(vars.ok, 'd')", "v", 1},
		{"vars['ok']", "v", 1},
		{"vars['o' + 'k']", "v", 1},
		{"vars.idx", "v-i", 2},
		{"vars.dyn_idx", "v-e", 2},
		{"size(vars)", int64(5), 0},
		{"'broken' in vars", true, 0},
		{"'absent' in vars", false, 0},
		{"vars.exists(k, k == 'broken')", true, 0},
		{"vars.ok == 'v'", true, 1},
		{"size(vars) != 0", true, 0},
		{"[vars][0].ok == 'v'", true, 1},
		{"[vars].exists(m, m.ok == 'v')", true, 1},
		{"[vars].exists(m, m.exists(k, k == 'broken'))", true, 0},
		{"[1].exists(vars, vars == 1)", true, 0},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			v := view()
			got, err := task.EvalExpression(tc.expr, Vars{View: v})
			if err != nil {
				t.Fatalf("%s: %v", tc.expr, err)
			}
			if got.Value() != tc.want {
				t.Fatalf("%s = %#v, want %#v", tc.expr, got.Value(), tc.want)
			}
			if v.Evaluations() != tc.evals {
				t.Fatalf("%s evaluated %d definitions, want %d", tc.expr, v.Evaluations(), tc.evals)
			}
		})
	}
}

// TestVarView_WholeReadPropagatesABrokenVar — point 13: a whole read materializes
// every name, so a broken or cyclic one fails the cell instead of landing in it as an
// error value. An equality over `vars` is one, on either side and at any depth, though
// cel-go drops the lazy map's error in all but `vars == m` — and so is one over a value
// the root reaches: an element of a literal list or map, an index or a select into it,
// a list concatenation, a comprehension's variable or result, and what merge() returns,
// since it copies its arguments' values shallowly. Calls whose result holds no
// argument's value — size(), type(), `in`, has() — leave the view lazy, so a broken var
// nobody reads fails none of the innocent cells.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code, any of:
//   - in varsScope.comprehension, `iter := s.with(cp.IterVar(), s.flow(cp.IterRange()).element())`
//     → `iter := s.with(cp.IterVar(), 0)`. Every case through a comprehension's
//     variable goes red;
//   - in varsScope.flow, drop the `operators.Index` case. Every case that indexes goes
//     red;
//   - in varsScope.flow, let a call carry flow only through dyn() and `+` again:
//     `if fn := c.FunctionName(); fn != overloads.TypeConvertDyn && fn != operators.Add { return 0 }`
//     ahead of the loop over the arguments. The merge() cases go red;
//   - in varsScope.flow, drop the `scalarResult` case. `type(vars) == map` and
//     `size(merge({'x': vars})) == 1` go red.
func TestVarView_WholeReadPropagatesABrokenVar(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	comparisons := []string{
		"${ vars == {'ok': 'v'} }",
		"${ vars != {'ok': 'v'} }",
		"${ {'ok': 'v'} == vars }",
		"${ {'ok': 'v'} != vars }",
		"${ vars == null }",
		"${ true && vars != {'ok': 'v'} }",
		"${ [vars] == [{'ok': 'v'}] }",
		"${ [{'ok': 'v'}] != [vars] }",
		"${ {'a': vars} == {'a': {'ok': 'v'}} }",
		"${ (true ? vars : {}) != {'ok': 'v'} }",
		"${ dyn(vars) != {'ok': 'v'} }",
		"${ default(vars, {}) != {'ok': 'v'} }",
		"${ vars in [{'ok': 'v'}] }",
		"${ {'ok': 'v'} in [vars] }",
		"${ [vars].exists(m, m != {'ok': 'v'}) }",
		"${ [vars].exists(m, {'ok': 'v'} != m) }",
		"${ [[vars]].exists(l, l[0] != {'ok': 'v'}) }",
		"${ [vars][0] != {'ok': 'v'} }",
		"${ {'a': vars}.a != {'ok': 'v'} }",
		"${ {'a': vars}['a'] != {'ok': 'v'} }",
		"${ [vars].map(m, m)[0] != {'ok': 'v'} }",
		"${ [vars].filter(m, true) != [{'ok': 'v'}] }",
		"${ ([vars] + [])[0] != {'ok': 'v'} }",
		"${ merge({'x': vars}).x != {'ok': 'v'} }",
		"${ {'ok': 'v'} == merge({'x': vars}).x }",
		"${ merge([{'x': vars}])['x'] != {'ok': 'v'} }",
		"${ [merge({'x': vars})].exists(m, m.x != {'ok': 'v'}) }",
	}
	broken := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
		"ok": "v", "broken": "${ vars.missing }",
	}))
	for _, cell := range append([]string{
		"${ vars }",
		"${ merge(vars, {}) }",
		"${ merge([vars]) }",
		"${ merge(vars, {}).ok }",
		"${ size(merge(vars, {})) }",
		"${ {'a': vars} }",
		"${ [vars] }",
	}, comparisons...) {
		t.Run(cell, func(t *testing.T) {
			got, err := task.EvalInterpolation(cell, Vars{View: broken})
			var ve *VarError
			if !errors.As(err, &ve) || ve.Name != "broken" {
				t.Fatalf("%s = %#v, %v; want broken's VarError", cell, got, err)
			}
		})
	}

	cyclic := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
		"ok": "v", "cyc": "${ vars.cyc }",
	}))
	for _, cell := range comparisons {
		t.Run("cyclic "+cell, func(t *testing.T) {
			got, err := task.EvalInterpolation(cell, Vars{View: cyclic})
			if !errors.Is(err, ErrVarCycle) {
				t.Fatalf("%s = %#v, %v; want ErrVarCycle", cell, got, err)
			}
		})
	}

	valid := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
		"ok": "v", "n": "${ 1 + 1 }",
	}))
	for cell, want := range map[string]any{
		"${ vars }":                                        map[string]any{"ok": "v", "n": int64(2)},
		"${ merge(vars, {'z': 1}) }":                       map[string]any{"ok": "v", "n": int64(2), "z": int64(1)},
		"${ [vars] }":                                      []any{map[string]any{"ok": "v", "n": int64(2)}},
		"${ vars == {'ok': 'v', 'n': 2} }":                 true,
		"${ vars != {'ok': 'v', 'n': 2} }":                 false,
		"${ {'ok': 'v', 'n': 2} == vars }":                 true,
		"${ {'ok': 'v', 'n': 2} != vars }":                 false,
		"${ {'ok': 'v'} == vars }":                         false,
		"${ [vars] == [{'ok': 'v', 'n': 2}] }":             true,
		"${ vars in [{'ok': 'v', 'n': 2}] }":               true,
		"${ [vars].exists(m, m != {'ok': 'v', 'n': 2}) }":  false,
		"${ [vars].exists(m, m != {'ok': 'v'}) }":          true,
		"${ merge({'x': vars}).x == {'ok': 'v', 'n': 2} }": true,
		"${ merge({'x': vars}).x != {'ok': 'v'} }":         true,
	} {
		got, err := task.EvalInterpolation(cell, Vars{View: valid})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %#v, %v; want %#v", cell, got, err, want)
		}
	}
	if m, err := valid.Materialize(); err != nil || !reflect.DeepEqual(m, map[string]any{"ok": "v", "n": int64(2)}) {
		t.Fatalf("Materialize() = %#v, %v", m, err)
	}

	innocent := map[string]any{
		"${ size(vars) }":                          int64(5),
		"${ 'd' in vars }":                         true,
		"${ vars.d }":                              int64(1),
		"${ has(vars.d) }":                         true,
		"${ vars.m.exists(k, k == 'k') }":          true,
		"${ [vars.d].exists(x, x == 1) }":          true,
		"${ {'k': vars.d} }":                       map[string]any{"k": int64(1)},
		"${ {'k': vars.d} == {'k': 1} }":           true,
		"${ vars.l == [1, 2] }":                    true,
		"${ vars.m == {'k': 1} }":                  true,
		"${ vars.d in [1] }":                       true,
		"${ vars.exists(k, k == 'd') }":            true,
		"${ vars.map(k, k).size() }":               int64(5),
		"${ type(vars) == map }":                   true,
		"${ size([vars][0]) }":                     int64(5),
		"${ [vars].exists(m, 'd' in m) }":          true,
		"${ [vars].exists(m, m.d == 1) }":          true,
		"${ [vars][0].d }":                         int64(1),
		"${ {'x': vars}.x.d }":                     int64(1),
		"${ default(vars.zz, 1) }":                 int64(1),
		"${ vars.filter(k, k == 'd').size() }":     int64(1),
		"${ dyn(vars).d }":                         int64(1),
		"${ [vars, vars].size() }":                 int64(2),
		"${ vars.d == 1 && vars.m.k == 1 }":        true,
		"${ merge({'x': vars}).x.d }":              int64(1),
		"${ size(merge({'x': vars})) == 1 }":       true,
		"${ string(vars.d) + string(size(vars)) }": "15",
	}
	for cell, want := range innocent {
		t.Run("innocent "+cell, func(t *testing.T) {
			v := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
				"c": "${ 1 / 0 }", "d": 1, "m": map[string]any{"k": 1}, "l": []any{1, 2}, "s": "${ vars.s }",
			}))
			got, err := task.EvalInterpolation(cell, Vars{View: v})
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s = %#v, %v; want %#v", cell, got, err, want)
			}
		})
	}
}

// TestVarView_AComparedCopyOfVarsIsTheSameInEveryOrder — point 10: CEL turns an error
// into a value only where any value gives the same result, except in an equality that
// meets the lazy `vars`, so such an equality is a whole read wherever the root reaches
// it. In each case y compares a copy of `vars` and a absorbs the cycle through y: in
// "exists" the copy is a comprehension's variable, in "merge" what merge() returns for
// a map holding the root. Were the copy not seen, y read inside a would turn that cycle
// into the value of a failed comparison, the memo would keep it, and y would be one
// value when read first and another when read after a.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code, either of:
//   - in varsScope.comprehension, `iter := s.with(cp.IterVar(), s.flow(cp.IterRange()).element())`
//     → `iter := s.with(cp.IterVar(), 0)`. "exists" goes red on every read that
//     evaluates a before y;
//   - in varsScope.flow, let a call carry flow only through dyn() and `+` again:
//     `if fn := c.FunctionName(); fn != overloads.TypeConvertDyn && fn != operators.Add { return 0 }`
//     ahead of the loop over the arguments. "merge" goes red the same way.
func TestVarView_AComparedCopyOfVarsIsTheSameInEveryOrder(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	for name, tc := range map[string]struct {
		below, top map[string]any
		want       map[string]string
	}{
		"exists": {
			below: map[string]any{"y": true},
			top: map[string]any{
				"y": "${ [vars].exists(m, m != {'a': true, 'y': true}) }",
				"a": "${ vars.y || true }",
			},
			want: map[string]string{
				"Lookup(a)":   "true",
				"Lookup(y)":   "false",
				"Materialize": `map[string]interface {}{"a":true, "y":false}`,
				"Check":       "<nil>",
				"a cell":      "false",
			},
		},
		"merge": {
			below: map[string]any{"y": 2},
			top: map[string]any{
				"y": "${ {'a': 1, 'y': 2} == merge({'x': vars}).x ? 'eq' : 'ne' }",
				"a": "${ vars.y == 'eq' || true ? 1 : 0 }",
			},
			want: map[string]string{
				"Lookup(a)":   "1",
				"Lookup(y)":   `"eq"`,
				"Materialize": `map[string]interface {}{"a":1, "y":"eq"}`,
				"Check":       "<nil>",
				"a cell":      `"eq"`,
			},
		},
	} {
		view := func() *VarView {
			return NewVarView(nil,
				serviceLayer(svc, "vars/00-base.yaml", tc.below),
				serviceLayer(svc, "vars/90-wb.yaml", tc.top),
			)
		}
		for read, do := range readsOf(task, []string{"a", "y"}, "${ vars.y }") {
			for _, after := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/after Lookup(a) %v", name, read, after), func(t *testing.T) {
					v := view()
					if after {
						val, _, err := v.Lookup("a")
						if got := outcome(val, err); got != tc.want["Lookup(a)"] {
							t.Fatalf("Lookup(a) = %s, want %s", got, tc.want["Lookup(a)"])
						}
					}
					if got := outcome(do(v)); got != tc.want[read] {
						t.Fatalf("%s = %s, want %s", read, got, tc.want[read])
					}
				})
			}
		}
	}
}

// TestVarView_NestedInterpolationIsEvaluated — point 1: a `${ … }` at any depth of a
// map or list definition is interpolated, and a nested self-reference reads the
// layer below.
func TestVarView_NestedInterpolationIsEvaluated(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	v := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
		"conf_dir": "/etc/redis",
		"tls": map[string]any{
			"cert": "${ vars.conf_dir }/tls.crt",
			"port": 6380,
			"opts": []any{"${ vars.conf_dir }", 1, map[string]any{"k": "${ vars.tls_port }"}},
		},
		"tls_port": "${ 6000 + 380 }",
	}))
	want := map[string]any{
		"cert": "/etc/redis/tls.crt",
		"port": 6380,
		"opts": []any{"/etc/redis", 1, map[string]any{"k": int64(6380)}},
	}
	if got := mustLookup(t, v, "tls"); !reflect.DeepEqual(got, want) {
		t.Fatalf("tls = %#v, want %#v", got, want)
	}

	t.Run("a nested self-reference reads the file below", func(t *testing.T) {
		v := NewVarView(nil,
			serviceLayer(svc, "vars/00-base.yaml", map[string]any{"creds": map[string]any{"user": "admin"}}),
			serviceLayer(svc, "vars/90-wb.yaml", map[string]any{"creds": map[string]any{"label": "${ vars.creds.user }-ro"}}),
		)
		want := map[string]any{"user": "admin", "label": "admin-ro"}
		if got := mustLookup(t, v, "creds"); !reflect.DeepEqual(got, want) {
			t.Fatalf("creds = %#v, want %#v", got, want)
		}
	})

	t.Run("a nested self-reference reads the rung below", func(t *testing.T) {
		parent := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{"paths": map[string]any{"root": "/srv"}}))
		v := NewVarView(parent, taskLayer(task, map[string]any{"paths": []any{map[string]any{"data": "${ vars.paths.root }/data"}}}))
		want := []any{map[string]any{"data": "/srv/data"}}
		if got := mustLookup(t, v, "paths"); !reflect.DeepEqual(got, want) {
			t.Fatalf("paths = %#v, want %#v", got, want)
		}
	})
}

// TestVarView_MergeDeepWithinARungReplaceAcrossRungs — point 3: the files of one rung
// deep-merge raw (a map recurses, anything else replaces, `replace` replaces the name
// whole); a higher rung replaces the name whole.
func TestVarView_MergeDeepWithinARungReplaceAcrossRungs(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	lower := func() VarLayer {
		return serviceLayer(svc, "vars/00-base.yaml", map[string]any{
			"db":     map[string]any{"host": "a", "port": 1},
			"xs":     []any{1, 2},
			"scalar": "s",
			"expr":   "${ {'host': 'a'} }",
		})
	}
	upperDefs := map[string]any{
		"db":     map[string]any{"port": 2},
		"xs":     []any{3},
		"scalar": map[string]any{"k": "v"},
		"expr":   map[string]any{"port": 2},
	}
	cases := []struct {
		name string
		view func() *VarView
		want map[string]any
	}{
		{"one rung deep-merges", func() *VarView {
			return NewVarView(nil, lower(), serviceLayer(svc, "vars/90-wb.yaml", upperDefs))
		}, map[string]any{
			"db":     map[string]any{"host": "a", "port": 2},
			"xs":     []any{3},
			"scalar": map[string]any{"k": "v"},
			"expr":   map[string]any{"port": 2},
		}},
		{"replace strategy", func() *VarView {
			upper := serviceLayer(svc, "vars/90-wb.yaml", upperDefs)
			upper.Strategy = map[string]string{"db": "replace"}
			return NewVarView(nil, lower(), upper)
		}, map[string]any{"db": map[string]any{"port": 2}}},
		{"a higher rung replaces", func() *VarView {
			return NewVarView(NewVarView(nil, lower()), taskLayer(task, upperDefs))
		}, map[string]any{"db": map[string]any{"port": 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.view()
			for name, want := range tc.want {
				if got := mustLookup(t, v, name); !reflect.DeepEqual(got, want) {
					t.Fatalf("%s = %#v, want %#v", name, got, want)
				}
			}
			if defs := v.Definitions()["db"]; len(defs) != 2 || !reflect.DeepEqual(defs[0], map[string]any{"host": "a", "port": 1}) {
				t.Fatalf("Definitions()[db] = %#v, want both layers' definitions, lower first", defs)
			}
		})
	}
}

// TestVarView_ErrorsAreNotMemoized — point 8: a failing definition is evaluated again
// on the next read and fails the same way.
func TestVarView_ErrorsAreNotMemoized(t *testing.T) {
	v := NewVarView(nil, serviceLayer(newServiceVarsEngine(t), "vars/00-base.yaml", map[string]any{"broken": "${ vars.missing }"}))
	_, _, first := v.Lookup("broken")
	_, _, second := v.Lookup("broken")
	if first == nil || second == nil || first.Error() != second.Error() {
		t.Fatalf("errors %v / %v, want the same error twice", first, second)
	}
	if v.Evaluations() != 2 {
		t.Fatalf("Evaluations() = %d, want 2", v.Evaluations())
	}
}

// cyclicGroup is n definitions on one ring of reads three wide: v_i reads v_{i+1},
// v_{i+2} and v_{i+3}, modulo n. Every one fails on the cycle and is reached by several
// paths.
func cyclicGroup(n int) map[string]any {
	defs := make(map[string]any, n)
	for i := range n {
		defs[fmt.Sprintf("v%02d", i)] = fmt.Sprintf("${ string(vars.v%02d) + string(vars.v%02d) + string(vars.v%02d) }", (i+1)%n, (i+2)%n, (i+3)%n)
	}
	return defs
}

// failingLattice is n acyclic definitions over one that fails: v_i reads v_{i+1} and
// v_{i+2}, and the last divides by zero, so the paths to it from v00 number about
// 1.6^n.
func failingLattice(n int) map[string]any {
	defs := make(map[string]any, n)
	for i := range n - 1 {
		defs[fmt.Sprintf("v%02d", i)] = fmt.Sprintf("${ vars.v%02d + vars.v%02d }", i+1, min(i+2, n-1))
	}
	defs[fmt.Sprintf("v%02d", n-1)] = "${ 1 / 0 }"
	return defs
}

// within returns read's error, failing t once read has run for limit. An evaluation
// cannot be stopped, so on a timeout it is left running.
func within(t *testing.T, limit time.Duration, read func() error) error {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- read() }()
	select {
	case err := <-done:
		t.Logf("returned in %v", time.Since(start))
		return err
	case <-time.After(limit):
		t.Fatalf("still evaluating after %v", limit)
		return nil
	}
}

// TestVarView_AFailedDefinitionIsEvaluatedOncePerRead — point 8: within one read a
// definition whose failure is kept is not evaluated again, and its first error is what
// every later path to it gets. CEL evaluates both operands of `+`, so without that a
// read walked every path to a failing definition: one Lookup of a 20-var cyclic group
// took 33 s. A failure is kept while the definitions it depends on are in progress or
// have failed too, which in a cyclic group is the whole read, and one over a division
// by zero depends on none. A 24-var cyclic group and a 30-var failing lattice fail with
// the error they always reported, each definition evaluated once, through every kind
// of read; a failed evaluation counts once and Check counts none. Settling the kept
// failures is linear too: a return touches only the failures that depend on it, and in
// a ring three wide a failure depends on nothing deeper than path index 2, so it is
// touched at most three times. Scanning every kept failure on every return touched
// n(n-1)/2.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code, any of:
//   - drop the failure-memo lookup, `if f, ok := read.failed[key]; ok { return nil, false, f }`,
//     from resolve;
//   - drop every kept failure whenever an evaluation returns: `clear(r.failed)` and
//     `return 0` at the top of varRead.settle;
//   - settle by scanning every kept failure: varRead.settle's body →
//     `touched := 0; for key, f := range r.failed { touched++; switch { case f.dep != d:
//     case ok: delete(r.failed, key); default: f.dep = d - 1 } }; return touched`.
//
// Every case goes red, on its deadline or on its count.
func TestVarView_AFailedDefinitionIsEvaluatedOncePerRead(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	ring := make([]string, 0, 25)
	for i := range 24 {
		ring = append(ring, fmt.Sprintf("v%02d", i))
	}
	for name, tc := range map[string]struct {
		defs map[string]any
		want string
	}{
		"a 24-var cyclic group":    {cyclicGroup(24), "var_cycle: " + strings.Join(append(ring, "v00"), " → ")},
		"a 30-var failing lattice": {failingLattice(30), `vars.v29 (vars/00-base.yaml): CEL eval "1 / 0": division by zero`},
	} {
		for read, tr := range map[string]struct {
			read  func(*VarView) error
			count bool
		}{
			"Lookup":      {func(v *VarView) error { _, _, err := v.Lookup("v00"); return err }, true},
			"Materialize": {func(v *VarView) error { _, err := v.Materialize(); return err }, true},
			"Check":       {(*VarView).Check, false},
			"a cell": {func(v *VarView) error {
				_, err := task.EvalInterpolation("${ vars.v00 }", Vars{View: v})
				return err
			}, true},
		} {
			t.Run(name+"/"+read, func(t *testing.T) {
				v := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", tc.defs))
				evaluated, touched := map[string]int{}, 0
				v.onEvaluate = func(name string, _ int) { evaluated[name]++ }
				v.onSettle = func(n int) { touched += n }
				err := within(t, time.Second, func() error { return tr.read(v) })
				if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
					t.Fatalf("err = %v, want it to end in %q", err, tc.want)
				}
				for _, name := range v.Names() {
					if evaluated[name] != 1 {
						t.Fatalf("%s evaluated %d times, want once: %v", name, evaluated[name], evaluated)
					}
				}
				want := int64(0)
				if tr.count {
					want = int64(len(tc.defs))
				}
				if got := v.Evaluations(); got != want {
					t.Fatalf("Evaluations() = %d, want %d", got, want)
				}
				if touched > 3*len(tc.defs) {
					t.Fatalf("settling touched %d kept failures, want at most %d", touched, 3*len(tc.defs))
				}
			})
		}
	}
}

// outcome is a read's value, or its error's text.
func outcome(val any, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("%#v", val)
}

// readsOf is every kind of read of v: a Lookup of each name, Materialize, Check and a
// cell.
func readsOf(task *Engine, names []string, cell string) map[string]func(*VarView) (any, error) {
	out := map[string]func(*VarView) (any, error){
		"Materialize": func(v *VarView) (any, error) { return v.Materialize() },
		"Check":       func(v *VarView) (any, error) { return nil, v.Check() },
		"a cell":      func(v *VarView) (any, error) { return task.EvalInterpolation(cell, Vars{View: v}) },
	}
	for _, n := range names {
		out["Lookup("+n+")"] = func(v *VarView) (any, error) {
			val, _, err := v.Lookup(n)
			return val, err
		}
	}
	return out
}

// TestVarView_AFailureIsKeptOnlyWhileItsPathHolds — point 8: a cycle error depends on
// the definitions in progress, so a read keeps a failure only while the ones it may
// depend on are in progress or have failed themselves. In each case a definition fails
// on a cycle that `||` absorbs further up, and is read again on a path without it:
//
//   - A: b fails on the cycle through a, which a absorbs; read again, b reads a's value;
//   - B: likewise, and b read again fails on the missing c instead;
//   - C: c fails on the cycles through a and b; b absorbs it, and a, reading c again
//     once b is done, gets its value.
//
// Each case has the outcomes it had when no failure was kept at all, in a fresh read
// and after a Lookup(b), through Lookup, Materialize, Check and a cell.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code, either of:
//   - never drop a kept failure: in varRead.settle, `if ok {` → `if false {`, so a
//     definition that returned a value counts as one that failed. A, B and C go red;
//   - root a failure at the SHALLOWEST definition it depends on, and drop it when that
//     one returns: in varFrame.saw,
//     `if d := len(f.path) - 1; dep >= 0 && dep < d && (f.dep < 0 || dep < f.dep) { f.dep = dep }`,
//     and `ok = true` at the top of varRead.settle. C goes red: c's failure outlives b.
func TestVarView_AFailureIsKeptOnlyWhileItsPathHolds(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	const noSuchC = `error: vars.b (vars/00-base.yaml): CEL eval "vars.a ? vars.c : 0": no such key: c`
	for name, tc := range map[string]struct {
		defs map[string]any
		cell string
		want map[string]string
	}{
		"A": {
			defs: map[string]any{"a": "${ vars['b' + ''] || true }", "b": "${ vars.a }"},
			cell: "${ string(vars.a) + string(vars.b) }",
			want: map[string]string{
				"Lookup(a)":   "true",
				"Lookup(b)":   "true",
				"Materialize": `map[string]interface {}{"a":true, "b":true}`,
				"Check":       "<nil>",
				"a cell":      `"truetrue"`,
			},
		},
		"B": {
			defs: map[string]any{"a": "${ vars.b == 1 || true }", "b": "${ vars.a ? vars.c : 0 }"},
			cell: "${ string(vars.a) + string(vars.b) }",
			want: map[string]string{
				"Lookup(a)":   "true",
				"Lookup(b)":   noSuchC,
				"Materialize": noSuchC,
				"Check":       noSuchC,
				"a cell":      `error: CEL eval "string(vars.a) + string(vars.b)": ` + noSuchC[len("error: "):],
			},
		},
		"C": {
			defs: map[string]any{
				"a": "${ string(vars.b) + string(vars.c) }",
				"b": "${ vars.c || true }",
				"c": "${ (vars.a || true) && vars.b }",
			},
			cell: "${ vars.a }",
			want: map[string]string{
				"Lookup(a)":   `"truetrue"`,
				"Lookup(b)":   "true",
				"Lookup(c)":   "true",
				"Materialize": `map[string]interface {}{"a":"truetrue", "b":true, "c":true}`,
				"Check":       "<nil>",
				"a cell":      `"truetrue"`,
			},
		},
	} {
		view := func() *VarView { return NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", tc.defs)) }
		for read, do := range readsOf(task, slices.Sorted(maps.Keys(tc.defs)), tc.cell) {
			for _, after := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/after Lookup(b) %v", name, read, after), func(t *testing.T) {
					v := view()
					if after {
						val, _, err := v.Lookup("b")
						if got := outcome(val, err); got != tc.want["Lookup(b)"] {
							t.Fatalf("Lookup(b) = %s, want %s", got, tc.want["Lookup(b)"])
						}
					}
					if got := outcome(do(v)); got != tc.want[read] {
						t.Fatalf("%s = %s, want %s", read, got, tc.want[read])
					}
				})
			}
		}
	}
}

// TestVarView_AFailureIsKeptPerLayerBound — point 8: a kept failure is keyed as a value
// is, by name and layer bound. x fails in 00-base, and 90-wb's x absorbs that failure
// through its self-reference, so one read meets x failing at one bound and succeeding
// at the other.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: key varRead.failed by name,
// `map[string]*varFailure`, with `read.failed[key.name]` in resolve. Materialize, Check
// and the cell go red: y reads x's lower failure.
func TestVarView_AFailureIsKeptPerLayerBound(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	view := func() *VarView {
		return NewVarView(nil,
			serviceLayer(svc, "vars/00-base.yaml", map[string]any{"x": "${ 1 / 0 }"}),
			serviceLayer(svc, "vars/90-wb.yaml", map[string]any{"x": "${ vars.x == 1 || true }", "y": "${ vars.x }"}),
		)
	}
	want := map[string]string{
		"Lookup(x)":   "true",
		"Lookup(y)":   "true",
		"Materialize": `map[string]interface {}{"x":true, "y":true}`,
		"Check":       "<nil>",
		"a cell":      `"truetrue"`,
	}
	for read, do := range readsOf(task, []string{"x", "y"}, "${ string(vars.x) + string(vars.y) }") {
		t.Run(read, func(t *testing.T) {
			if got := outcome(do(view())); got != want[read] {
				t.Fatalf("%s = %s, want %s", read, got, want[read])
			}
		})
	}
}

// TestVarView_AFailureOutlivesAFailedReturnAndNotItsParent — point 8: a kept failure
// that depends on a definition in progress depends, once that definition returns
// failed, on its parent alone. In one cell, t is at path index 0 and reads a, c and e
// at index 1. b fails on the ring through a; a fails too, so b's failure now depends on
// t alone. r, under c, reads that failure: its own depends on t alone as well, so it
// survives c's return and e reads it without evaluating r again. When t returns, both
// are dropped, and g evaluates r afresh. So r is evaluated twice, against three times
// with no failure kept, and the outcome is the same.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code, either of:
//   - in varRead.settle, delete `f.dep = d - 1`. b's failure keeps index 1 as its
//     dependency, r's failure is dropped when c returns, and r is evaluated three
//     times;
//   - in varRead.settle, `if ok {` → `if false {`, so no kept failure is dropped. r is
//     evaluated once.
func TestVarView_AFailureOutlivesAFailedReturnAndNotItsParent(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	defs := map[string]any{
		"a": "${ vars.b }",
		"b": "${ vars.a }",
		"r": "${ vars.b }",
		"c": "${ has(vars.r) || true }",
		"e": "${ has(vars.r) || true }",
		"g": "${ has(vars.r) || true }",
		"t": "${ (has(vars.a) || true) && vars.c && vars.e }",
	}
	for _, off := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure memo off %v", off), func(t *testing.T) {
			failureMemoOff = off
			defer func() { failureMemoOff = false }()
			v := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", defs))
			evaluated := map[string]int{}
			v.onEvaluate = func(name string, _ int) { evaluated[name]++ }
			got, err := task.EvalInterpolation("${ vars.t && vars.g }", Vars{View: v})
			if err != nil || got != true {
				t.Fatalf("cell = %#v, %v; want true", got, err)
			}
			want := 2
			if off {
				want = 3
			}
			if evaluated["r"] != want {
				t.Fatalf("r evaluated %d times, want %d: %v", evaluated["r"], want, evaluated)
			}
		})
	}
}

// TestVarView_OutcomesDoNotDependOnThePathOrTheOrder — point 10, over random views of
// five definitions that read each other, through a literal and a non-literal key,
// under `||`, `&&`, `?:` and `exists`, now and then over a missing name or a division
// by zero. What a fresh Lookup of a name gives, its value or a failure, it gives after
// any other Lookups too; Materialize, Check and a cell over every name succeed exactly
// when every fresh Lookup does, with those values. The error text is not compared: a
// kept failure's text is the one its first path rendered (point 10).
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: never drop a kept failure — in
// varRead.settle, `if ok {` → `if false {`. It goes red.
func TestVarView_OutcomesDoNotDependOnThePathOrTheOrder(t *testing.T) {
	const views = 60
	svc, task := newServiceVarsEngine(t), newEngine(t)
	names := []string{"a", "b", "c", "d", "e"}
	rng := rand.New(rand.NewSource(1014))
	var expr func(self string, depth int) string
	expr = func(self string, depth int) string {
		n := self
		for n == self {
			n = names[rng.Intn(len(names))]
		}
		if depth == 0 || rng.Intn(3) == 0 {
			switch r := rng.Intn(16); r {
			case 0:
				return "vars.missing"
			case 1:
				return "1 / 0 == 0"
			default:
				return []string{"true", "false", "vars." + n, "vars." + n, "vars." + n, "vars['" + n + "' + '']", "vars." + n + " == 1"}[r%7]
			}
		}
		x, y := expr(self, depth-1), expr(self, depth-1)
		switch rng.Intn(6) {
		case 0, 1:
			return "(" + x + " || " + y + ")"
		case 2, 3:
			return "(" + x + " && " + y + ")"
		case 4:
			return "(" + x + " ? " + y + " : " + expr(self, depth-1) + ")"
		default:
			return "[1, 2].exists(i, i == 1 ? " + x + " : " + y + ")"
		}
	}
	status := func(val any, err error) string {
		if err != nil {
			return "a failure"
		}
		return fmt.Sprintf("%#v", val)
	}
	whole := 0
	for g := range views {
		defs := make(map[string]any, len(names))
		for _, n := range names {
			defs[n] = "${ " + expr(n, 2) + " }"
		}
		view := func() *VarView { return NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", defs)) }
		want := make(map[string]string, len(names))
		values := make(map[string]any, len(names))
		for _, n := range names {
			val, _, err := view().Lookup(n)
			want[n] = status(val, err)
			if err == nil {
				values[n] = val
			}
		}
		ok := len(values) == len(names)
		wantAll, wantCell := "a failure", "a failure"
		if ok {
			whole++
			wantAll = status(values, nil)
			wantCell = status([]any{values["a"], values["b"], values["c"], values["d"], values["e"]}, nil)
		}
		cell, err := task.EvalInterpolation("${ [vars.a, vars.b, vars.c, vars.d, vars.e] }", Vars{View: view()})
		if got := status(cell, err); got != wantCell {
			t.Fatalf("view #%d %v: the cell = %s, want %s", g, defs, got, wantCell)
		}
		for range 4 {
			order := slices.Clone(names)
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			order = order[:rng.Intn(len(order)+1)]
			v := view()
			for _, n := range order {
				val, _, err := v.Lookup(n)
				if got := status(val, err); got != want[n] {
					t.Fatalf("view #%d %v: after Lookups %v, Lookup(%s) = %s, want %s", g, defs, order, n, got, want[n])
				}
			}
			m, err := v.Materialize()
			if got := status(m, err); got != wantAll {
				t.Fatalf("view #%d %v: after Lookups %v, Materialize = %s, want %s", g, defs, order, got, wantAll)
			}
			if err := v.Check(); (err == nil) != ok {
				t.Fatalf("view #%d %v: after Lookups %v, Check = %v, want success %v", g, defs, order, err, ok)
			}
		}
	}
	t.Logf("%d of %d views evaluate whole", whole, views)
}

// memoLayer is one layer of a random view, before its Engine is attached.
type memoLayer struct {
	rung     VarRung
	defs     map[string]any
	strategy map[string]string
}

// memoGraph is a random view and the reads made on it, in order. split > 0 stacks the
// layers from split on as a child view over a parent view of the ones below.
type memoGraph struct {
	layers []memoLayer
	split  int
	reads  []memoRead
}

// memoRead is one read, on the child view or the parent: "lookup" of name,
// "materialize", "check", or cell.
type memoRead struct {
	kind  string
	child bool
	name  string
	cell  string
}

type memoMode int

const (
	memoMixed  memoMode = iota // every operator, over up to 8 names
	memoCyclic                 // mostly reads of other names
	memoTight                  // three names, each a pair of reads of the others
)

// memoGen writes random definitions. Values stay bounded: a map cell and each half of
// a two-part string hold a bool, so no definition nests or concatenates another's
// value.
type memoGen struct {
	rng   *rand.Rand
	mode  memoMode
	names []string
}

func (g *memoGen) name(self string) string {
	if self != "" && g.rng.Intn(4) == 0 {
		return self
	}
	return g.names[g.rng.Intn(len(g.names))]
}

func (g *memoGen) leaf(self string) string {
	if g.mode == memoTight {
		n := self
		for n == self {
			n = g.names[g.rng.Intn(len(g.names))]
		}
		return []string{"vars." + n, "vars." + n, "(vars." + n + " || true)", "vars['" + n + "' + '']"}[g.rng.Intn(4)]
	}
	n := g.name(self)
	if g.mode == memoCyclic && g.rng.Intn(3) > 0 {
		return []string{"vars." + n, "vars['" + n + "' + '']", "(vars." + n + " || true)", "(vars." + n + " && false)", "has(vars." + n + ")", "default(vars." + n + ", true)"}[g.rng.Intn(6)]
	}
	switch g.rng.Intn(26) {
	case 0:
		return "vars.missing"
	case 1:
		return "1 / 0 == 0"
	case 2:
		return "true"
	case 3:
		return "false"
	case 4:
		return "vars['" + n + "' + '']"
	case 5:
		return "vars." + n + " == true"
	case 6:
		return "has(vars." + n + ")"
	case 7:
		return "default(vars." + n + ", true)"
	case 8:
		return "vars." + n + ".k == true"
	case 9:
		return "has(vars." + n + ".k)"
	case 10:
		return "default(vars." + n + ".k, false)"
	case 11:
		return "'" + n + "' in vars"
	case 12:
		return "size(vars) > 1"
	case 13:
		return "vars.exists(q, q == '" + n + "' && vars[q] == true)"
	case 14:
		return "size(merge(vars)) > 0"
	case 15:
		return "vars != {}"
	case 16:
		return "[vars].exists(m, m != {})"
	case 17:
		return "vars." + n + " == 1"
	case 18:
		return "string(vars." + n + ") == 'true'"
	case 19:
		return "type(vars." + n + ") == bool"
	default:
		return "vars." + n
	}
}

func (g *memoGen) expr(self string, depth int) string {
	if depth == 0 || g.rng.Intn(3) == 0 {
		return g.leaf(self)
	}
	x, y := g.expr(self, depth-1), g.expr(self, depth-1)
	switch g.rng.Intn(8) {
	case 0, 1:
		return "(" + x + " || " + y + ")"
	case 2, 3:
		return "(" + x + " && " + y + ")"
	case 4:
		return "(" + x + " ? " + y + " : " + g.expr(self, depth-1) + ")"
	case 5:
		return "[1, 2].exists(i, i == 1 ? " + x + " : " + y + ")"
	case 6:
		return "[1, 2].all(i, i == 1 ? " + x + " : " + y + ")"
	default:
		return "!" + x
	}
}

func (g *memoGen) boolean(self string) string {
	return "(" + g.expr(self, 1) + ") == true"
}

func (g *memoGen) def(self string) any {
	if g.mode == memoTight {
		return "${ " + g.leaf(self) + []string{" || ", " && "}[g.rng.Intn(2)] + g.leaf(self) + " }"
	}
	switch r := g.rng.Intn(20); {
	case r < 13 || g.mode == memoCyclic && r < 18:
		return "${ " + g.expr(self, 2) + " }"
	case r < 16:
		m := map[string]any{"k": "${ " + g.boolean(self) + " }", "m": true}
		if g.rng.Intn(2) == 0 {
			m["m"] = "${ " + g.boolean(self) + " }"
		}
		return m
	case r < 17:
		return "${ string(" + g.boolean(self) + ") }-${ string(" + g.boolean(self) + ") }"
	case r < 18:
		return g.rng.Intn(2) == 0
	default:
		return map[string]any{"k": true}
	}
}

func newMemoGraph(rng *rand.Rand, mode memoMode) memoGraph {
	all := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	g := &memoGen{rng: rng, mode: mode, names: all[:2+rng.Intn(len(all)-1)]}
	if mode == memoTight {
		g.names = all[:3]
	}
	var gr memoGraph
	rung := RungService
	for i := range 1 + rng.Intn(3) {
		if i > 0 && rng.Intn(2) == 0 {
			rung = min(rung+1+VarRung(rng.Intn(2)), RungTask)
		}
		l := memoLayer{rung: rung, defs: map[string]any{}, strategy: map[string]string{}}
		for _, n := range g.names {
			if rng.Intn(10) < 6 || i == 0 && rng.Intn(3) == 0 {
				l.defs[n] = g.def(n)
				if rung == RungService && rng.Intn(5) == 0 {
					l.strategy[n] = strategyReplace
				}
			}
		}
		gr.layers = append(gr.layers, l)
	}
	if len(gr.layers) > 1 && rng.Intn(5) < 2 {
		gr.split = 1 + rng.Intn(len(gr.layers)-1)
	}
	for range 3 + rng.Intn(6) {
		r := memoRead{child: gr.split == 0 || rng.Intn(2) == 0}
		switch k := rng.Intn(10); {
		case k < 5:
			r.kind, r.name = "lookup", g.name("")
			if rng.Intn(15) == 0 {
				r.name = "absent"
			}
		case k < 6:
			r.kind = "materialize"
		case k < 7:
			r.kind = "check"
		case k < 8:
			r.kind, r.cell = "cell", "${ "+g.expr("", 2)+" }"
		default:
			order := slices.Clone(g.names)
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			r.kind, r.cell = "cell", "${ [vars."+strings.Join(order, ", vars.")+"] }"
		}
		gr.reads = append(gr.reads, r)
	}
	return gr
}

// memoEngines are the Engines of the rungs: a layer compiles with its rung's.
type memoEngines struct{ service, scenario, task *Engine }

func (m memoEngines) of(r VarRung) *Engine {
	switch r {
	case RungService:
		return m.service
	case RungScenario:
		return m.scenario
	}
	return m.task
}

// memoResult is each read's outcome and the most times it evaluated one definition;
// ok is false when the reads ran over their budget of evaluations and were stopped.
type memoResult struct {
	outcomes []string
	most     []int
	ok       bool
}

// run makes gr's reads in order on fresh views, with the failure memo as
// failureMemoOff has it.
func (m memoEngines) run(gr memoGraph, budget int) memoResult {
	var res memoResult
	done := make(chan bool, 1)
	go func() {
		completed := false
		defer func() { done <- completed }()
		total, per := 0, map[varKey]int{}
		count := func(name string, bound int) {
			per[varKey{name, bound}]++
			if total++; total > budget {
				runtime.Goexit()
			}
		}
		layers := make([]VarLayer, len(gr.layers))
		for i, l := range gr.layers {
			layers[i] = VarLayer{Rung: l.rung, Origin: "L" + strconv.Itoa(i), Defs: l.defs, Strategy: l.strategy, Engine: m.of(l.rung), Base: Vars{Incarnation: testIncarnation}}
		}
		parent := NewVarView(nil, layers...)
		child := parent
		if gr.split > 0 {
			parent = NewVarView(nil, layers[:gr.split]...)
			child = NewVarView(parent, layers[gr.split:]...)
		}
		parent.onEvaluate, child.onEvaluate = count, count
		for _, r := range gr.reads {
			v := parent
			if r.child {
				v = child
			}
			var val any
			found, err := true, error(nil)
			switch r.kind {
			case "lookup":
				val, found, err = v.Lookup(r.name)
			case "materialize":
				val, err = v.Materialize()
			case "check":
				err = v.Check()
			default:
				val, err = m.task.EvalInterpolation(r.cell, Vars{View: v})
			}
			switch {
			case err != nil:
				res.outcomes = append(res.outcomes, "a failure")
			case !found:
				res.outcomes = append(res.outcomes, "absent")
			default:
				res.outcomes = append(res.outcomes, fmt.Sprintf("%#v", val))
			}
			res.most = append(res.most, slices.Max(append(slices.Collect(maps.Values(per)), 0)))
			clear(per)
		}
		completed = true
	}()
	res.ok = <-done
	return res
}

// runAll runs every graph, GOMAXPROCS at a time.
func (m memoEngines) runAll(graphs []memoGraph, budget int) []memoResult {
	out := make([]memoResult, len(graphs))
	next := make(chan int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for i := range next {
				out[i] = m.run(graphs[i], budget)
			}
		})
	}
	for i := range graphs {
		next <- i
	}
	close(next)
	wg.Wait()
	return out
}

// memoViews is how many views TestVarView_TheFailureMemoChangesNoOutcome draws; fewer
// under the race detector (varview_race_test.go).
var memoViews = 2400

// TestVarView_TheFailureMemoChangesNoOutcome — points 8 and 10, against the reference:
// the same reads with the failure memo off (failureMemoOff), where a read evaluates a
// failed definition again on every path to it. Over seeded random views — up to 8 names
// on up to 3 layers, with self-reads and deep-merged or replaced maps, as one view or a
// parent and a child — a sequence of Lookups, Materializes, Checks and cells on the
// child and the parent succeeds or fails, and with which value, read for read as the
// reference does. A third of the views mix `||`, `&&`, `?:`, `exists`, `all`, `!`,
// `has()`, `default()`, `'x' in vars`, `size(vars)`, `merge(vars)`, `vars != {}`, a
// missing name and a division by zero; a third are cycle-heavy, reading mostly other
// names; a third are rings of three names that read each other under `||` and `&&`,
// where failures are absorbed and so dropped. And the memo saves the work it is for:
// with it on, no read evaluates one definition more than three times.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code, any of:
//   - let a failure depend on its own definition: in varFrame.saw,
//     `min(dep, len(f.path)-2)` → `dep`;
//   - never drop a kept failure: in varRead.settle, `if ok {` → `if false {`;
//   - drop every kept failure whenever an evaluation returns: `clear(r.failed)` and
//     `return 0` at the top of varRead.settle. The outcomes hold and the count goes red.
func TestVarView_TheFailureMemoChangesNoOutcome(t *testing.T) {
	const (
		budget = 20000
		most   = 3
	)
	m := memoEngines{newServiceVarsEngine(t), newScenarioVarsEngine(t), newEngine(t)}
	start := time.Now()
	graphs := make([]memoGraph, memoViews)
	for i := range graphs {
		graphs[i] = newMemoGraph(rand.New(rand.NewSource(int64(1014_000+i))), memoMode(i%3))
	}
	got := m.runAll(graphs, budget)
	failureMemoOff = true
	want := m.runAll(graphs, budget)
	failureMemoOff = false
	reads, refMost := 0, 0
	for i, gr := range graphs {
		if !got[i].ok || !want[i].ok {
			t.Fatalf("view #%d %+v: over %d evaluations, memo on %v, memo off %v", i, gr, budget, !got[i].ok, !want[i].ok)
		}
		for j, r := range gr.reads {
			reads++
			if got[i].outcomes[j] != want[i].outcomes[j] {
				t.Fatalf("view #%d %+v: read %d %+v = %s, want %s as with the memo off", i, gr, j, r, got[i].outcomes[j], want[i].outcomes[j])
			}
			if got[i].most[j] > most {
				t.Fatalf("view #%d %+v: read %d %+v evaluated one definition %d times, want at most %d", i, gr, j, r, got[i].most[j], most)
			}
			refMost = max(refMost, want[i].most[j])
		}
	}
	t.Logf("%d views, %d reads in %v; with the memo off a read evaluated one definition up to %d times", memoViews, reads, time.Since(start), refMost)
}

// TestVarView_AnExpressionInALayerWithoutAnEngineIsAVarError — a layer built without an
// Engine still serves its literal definitions; one carrying `${ … }`, at any depth,
// fails as that definition instead of panicking.
func TestVarView_AnExpressionInALayerWithoutAnEngineIsAVarError(t *testing.T) {
	v := NewVarView(nil, VarLayer{Rung: RungService, Origin: "vars/00-base.yaml", Defs: map[string]any{
		"plain":  "p",
		"expr":   "${ 1 + 1 }",
		"nested": map[string]any{"k": []any{"${ vars.plain }"}},
	}})
	if got := mustLookup(t, v, "plain"); got != "p" {
		t.Fatalf("plain = %#v, want p", got)
	}
	for _, name := range []string{"expr", "nested"} {
		_, _, err := v.Lookup(name)
		var ve *VarError
		if !errors.As(err, &ve) || ve.Name != name || ve.Origin != "vars/00-base.yaml" {
			t.Fatalf("Lookup(%q) = %v, want a VarError for %s from vars/00-base.yaml", name, err, name)
		}
	}
}

// TestNewScenarioVars_RefusesEveryRootOutsideRunData — point 6: a scenario `vars:`
// definition names run data only. Every entry of the closed list is refused at
// compile, top-level and nested, whatever host flags the reader's Vars carry; this is
// what keeps the Passage cut's axes complete (point 16).
func TestNewScenarioVars_RefusesEveryRootOutsideRunData(t *testing.T) {
	e := newScenarioVarsEngine(t)
	scenario := func(base Vars, defs map[string]any) *VarView {
		return NewVarView(nil, VarLayer{Rung: RungScenario, Origin: "scenario vars:", Defs: defs, Engine: e, Base: base})
	}
	refused := []string{
		"soulprint.self.x",
		"soulprint.hosts",
		"soulprint.where('true')",
		"register.p.x",
		"register.hosts.p",
		"register.self.x",
		"vault('secret/x#y')",
		"generate_secret({})",
	}
	bases := map[string]Vars{
		"no host flags":  {},
		"all host flags": {AllowHosts: true, AllowRegisterHosts: true},
	}
	for baseName, base := range bases {
		for _, expr := range refused {
			for shape, def := range map[string]any{
				"top-level": "${ " + expr + " }",
				"nested":    map[string]any{"a": "${ " + expr + " }"},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", baseName, shape, expr), func(t *testing.T) {
					_, _, err := scenario(base, map[string]any{"x": def}).Lookup("x")
					var (
						ve          *VarError
						compileErr  *ErrCompile
						unsupported *ErrUnsupported
					)
					if !errors.As(err, &ve) || ve.Name != "x" {
						t.Fatalf("Lookup(x) = %v, want a VarError for x", err)
					}
					if !errors.As(err, &compileErr) && !errors.As(err, &unsupported) {
						t.Fatalf("Lookup(x) = %v, want a compile-time refusal", err)
					}
				})
			}
		}
	}

	t.Run("run data is accepted", func(t *testing.T) {
		v := scenario(Vars{
			Input:       map[string]any{"a": "A"},
			Incarnation: testIncarnation,
			Compute:     map[string]any{"c": "C"},
		}, map[string]any{
			"x": map[string]any{"s": "${ input.a }-${ incarnation.id }-${ compute.c }-${ vars.y }"},
			"y": "Y",
		})
		if got := mustLookup(t, v, "x"); !reflect.DeepEqual(got, map[string]any{"s": "A-inc-1-C-Y"}) {
			t.Fatalf("x = %#v", got)
		}
	})

	t.Run("WithVault is refused", func(t *testing.T) {
		if _, err := NewScenarioVars(WithVault(&stubKV{})); err == nil {
			t.Fatal("NewScenarioVars(WithVault) = nil error, want a refusal")
		}
	})
}

// TestVarView_ConcurrentReadsAgree — point 8: the memo is shared across goroutines;
// duplicate evaluation is harmless and every reader sees the same values.
func TestVarView_ConcurrentReadsAgree(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	parent := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
		"conf_dir": "/etc/redis",
		"acl_file": "${ vars.conf_dir }/users.acl",
		"port":     6379,
		"endpoint": "${ incarnation.id }:${ vars.port }",
	}))
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Go(func() {
			c := NewVarView(parent, taskLayer(task, map[string]any{"port": 7000 + i%2}))
			want := map[string]any{"acl_file": "/etc/redis/users.acl", "endpoint": fmt.Sprintf("inc-1:%d", 7000+i%2)}
			for name, w := range want {
				got, _, err := c.Lookup(name)
				if err != nil || got != w {
					errs <- fmt.Errorf("goroutine %d: %s = %#v, %v; want %q", i, name, got, err, w)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestVarView_CheckOnAChildStoresInNeitherMemo — point 12b: [VarView.Check] on a task
// view over the service view keeps nothing it evaluated, in the child's memo or in the
// parent's, where a value of the parent's own layers would otherwise wait for the next
// reader. A read after the check evaluates as if it had not run.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in resolve, after the scratch
// store, `if read.scratch != nil && key.bound < v.base { v.parent.store(key, e) }`. It goes
// red.
func TestVarView_CheckOnAChildStoresInNeitherMemo(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	parent := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
		"conf_dir": "/etc/redis",
		"acl_file": "${ vars.conf_dir }/users.acl",
	}))
	child := NewVarView(parent, taskLayer(task, map[string]any{"own": "${ 'o' + 'k' }"}))
	if err := child.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if n, m := len(parent.memo), len(child.memo); n != 0 || m != 0 {
		t.Fatalf("Check left %d entries in the parent's memo and %d in the child's, want none", n, m)
	}

	var evaluated []string
	parent.onEvaluate = func(name string, _ int) { evaluated = append(evaluated, name) }
	if got := mustLookup(t, parent, "acl_file"); got != "/etc/redis/users.acl" {
		t.Fatalf("acl_file = %#v", got)
	}
	if !slices.Equal(evaluated, []string{"acl_file", "conf_dir"}) {
		t.Fatalf("the read after the check evaluated %v, want [acl_file conf_dir]", evaluated)
	}
	if p, c := parent.Evaluations(), child.Evaluations(); p != 2 || c != 0 {
		t.Fatalf("Evaluations() = parent %d, child %d; want 2 and 0", p, c)
	}
}

// TestVarView_AFailedCheckCountsNoEvaluation — Check adds nothing to Evaluations, and a
// definition that fails under it is no exception, on a view alone or on a child over
// the view that defines it. A read still counts its failure.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the `if read.scratch == nil`
// around the count of a failed evaluation in evaluate. Both cases go red.
func TestVarView_AFailedCheckCountsNoEvaluation(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	service := func() *VarView {
		return NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{"ok": 1, "broken": "${ vars.missing }"}))
	}
	for name, build := range map[string]func() (checked *VarView, all []*VarView){
		"a view alone": func() (*VarView, []*VarView) {
			v := service()
			return v, []*VarView{v}
		},
		"a child over it": func() (*VarView, []*VarView) {
			parent := service()
			child := NewVarView(parent, taskLayer(task, map[string]any{"own": 2}))
			return child, []*VarView{parent, child}
		},
	} {
		t.Run(name, func(t *testing.T) {
			checked, all := build()
			if err := checked.Check(); err == nil || !strings.Contains(err.Error(), "vars.broken") {
				t.Fatalf("Check = %v, want vars.broken's error", err)
			}
			for i, v := range all {
				if n := v.Evaluations(); n != 0 {
					t.Fatalf("view %d: Evaluations() = %d after the check, want 0", i, n)
				}
			}
			if _, _, err := checked.Lookup("broken"); err == nil {
				t.Fatal("Lookup(broken) succeeded")
			}
			var total int64
			for _, v := range all {
				total += v.Evaluations()
			}
			if total != 1 {
				t.Fatalf("a failed read counted %d evaluations, want 1", total)
			}
		})
	}
}

// TestVarView_CheckBesideReadsAgrees — Check runs concurrently with Lookup and
// Materialize on a parent view and a child over it, and every caller sees the values
// it would alone. Its worth is under `go test -race`; there is no mutation to make.
func TestVarView_CheckBesideReadsAgrees(t *testing.T) {
	svc, task := newServiceVarsEngine(t), newEngine(t)
	parent := NewVarView(nil, serviceLayer(svc, "vars/00-base.yaml", map[string]any{
		"conf_dir": "/etc/redis",
		"acl_file": "${ vars.conf_dir }/users.acl",
		"port":     6379,
		"endpoint": "${ incarnation.id }:${ vars.port }",
	}))
	child := NewVarView(parent, taskLayer(task, map[string]any{"port": 7000, "own": "${ vars.endpoint }/x"}))
	views := []struct {
		view *VarView
		want map[string]any
	}{
		{parent, map[string]any{"conf_dir": "/etc/redis", "acl_file": "/etc/redis/users.acl", "port": 6379, "endpoint": "inc-1:6379"}},
		{child, map[string]any{"conf_dir": "/etc/redis", "acl_file": "/etc/redis/users.acl", "port": 7000, "endpoint": "inc-1:7000", "own": "inc-1:7000/x"}},
	}
	var wg sync.WaitGroup
	errs := make(chan error, 256)
	for range 16 {
		for i, vw := range views {
			wg.Go(func() {
				if err := vw.view.Check(); err != nil {
					errs <- fmt.Errorf("view %d: Check: %w", i, err)
				}
			})
			wg.Go(func() {
				if got, err := vw.view.Materialize(); err != nil || !reflect.DeepEqual(got, vw.want) {
					errs <- fmt.Errorf("view %d: Materialize = %#v, %v; want %#v", i, got, err, vw.want)
				}
			})
			wg.Go(func() {
				for _, name := range slices.Sorted(maps.Keys(vw.want)) {
					if got, _, err := vw.view.Lookup(name); err != nil || got != vw.want[name] {
						errs <- fmt.Errorf("view %d: %s = %#v, %v; want %#v", i, name, got, err, vw.want[name])
					}
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
