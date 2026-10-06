package cel

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// The differential test of predicate-rendering.md §1: the original evaluated against
// the host's full context, the rendering against the open set ALONE, over the same open
// values. Every trap of §2 is a case below; each case states the rendered text it
// expects where the text itself is the point.

// predicateCase — one differential case.
type predicateCase struct {
	name string
	expr string
	// ctx — the host context without the open set.
	ctx Vars
	// open — the registers left open; nil means {self}.
	open []string
	// opens — values the open set takes: register name → payload.
	opens []map[string]any
	// want — when set, the exact rendered text.
	want string
	// refuse — when set, the code the rendering is refused with.
	refuse string
	// reason — with refuse, a fragment of the refusal's reason: a refusal for a
	// different cause is a red for the wrong reason.
	reason string
}

func self(v map[string]any) map[string]any { return map[string]any{RegisterSelf: v} }

// hostContext is one host's full context minus the open set. Registers carry numbers as
// float64 — they arrive as JSON — while input from YAML carries ints.
func hostContext() Vars {
	return Vars{
		Input: map[string]any{
			"ok":     "ready",
			"bad":    "failed",
			"n":      3,
			"s":      "str",
			"flag":   true,
			"nullv":  nil,
			"list":   []any{"a", "b"},
			"cfg":    map[string]any{"z": 1, "a": 2, "m": map[string]any{"k": map[string]any{"f": 1}}},
			"tricky": "a\nb\t\"c\\d' é 😀 \x00 end",
			"u":      uint64(3),
			"bytes":  []byte("str"),
		},
		Register: map[string]any{
			"probe": map[string]any{"stdout": "bob", "leader": "a", "min": 3.0, "d": 2.5, "out": "x", "x": "px"},
			"other": map[string]any{"x": 3.0},
		},
		Vars: map[string]any{
			"want":     "a",
			"deadline": time.Date(2026, 10, 6, 12, 0, 0, 5, time.FixedZone("MSK", 3*3600)),
			"timeout":  90 * time.Second,
		},
		Incarnation:   map[string]any{"id": "redis-prod"},
		SoulprintSelf: map[string]any{"os": map[string]any{"family": "debian"}},
	}
}

func withInput(v Vars, key string, val any) Vars {
	in := make(map[string]any, len(v.Input)+1)
	for k, x := range v.Input {
		in[k] = x
	}
	in[key] = val
	v.Input = in
	return v
}

func withVar(v Vars, key string, val any) Vars {
	m := make(map[string]any, len(v.Vars)+1)
	for k, x := range v.Vars {
		m[k] = x
	}
	m[key] = val
	v.Vars = m
	return v
}

func withRegister(v Vars, name string, payload map[string]any) Vars {
	reg := make(map[string]any, len(v.Register)+1)
	for k, x := range v.Register {
		reg[k] = x
	}
	reg[name] = payload
	v.Register = reg
	return v
}

func mergeRegisters(base, open map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(open))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range open {
		out[k] = v
	}
	return out
}

func nestedList(depth int) any {
	var v any = "leaf"
	for i := 0; i < depth; i++ {
		v = []any{v}
	}
	return v
}

func checkPredicateCase(t *testing.T, e *Engine, c predicateCase) {
	t.Helper()
	open := c.open
	if open == nil {
		open = []string{RegisterSelf}
	}
	got, err := e.RenderPredicate(c.expr, c.ctx, open)
	if c.refuse != "" {
		var pe *ErrPredicateRender
		if !errors.As(err, &pe) || pe.Code != c.refuse || !strings.Contains(pe.Reason, c.reason) {
			t.Fatalf("RenderPredicate(%q) = %q, %v; want refusal %s (%q)", c.expr, got.Text, err, c.refuse, c.reason)
		}
		return
	}
	if err != nil {
		t.Fatalf("RenderPredicate(%q): %v", c.expr, err)
	}
	if c.want != "" && got.Text != c.want {
		t.Errorf("RenderPredicate(%q)\n got: %s\nwant: %s", c.expr, got.Text, c.want)
	}
	if len(c.opens) == 0 {
		t.Fatalf("case %q has no open values to compare over", c.name)
	}
	for _, ov := range c.opens {
		full := c.ctx
		full.Register = mergeRegisters(c.ctx.Register, ov)
		wantVal, wantErr := e.EvalExpression(c.expr, full)
		gotVal, gotErr := e.EvalExpression(got.Text, Vars{Register: ov})
		if msg := sameOutcome(wantVal, wantErr, gotVal, gotErr); msg != "" {
			t.Errorf("open set %v:\n original: %s\n rendered: %s\n %s", ov, c.expr, got.Text, msg)
		}
	}
}

// sameOutcome compares the original's outcome with the rendering's. An error matches an
// error at evaluation; a compile error of the rendering never matches, since the Soul
// would refuse the text before the module's result is judged at all.
func sameOutcome(wantVal ref.Val, wantErr error, gotVal ref.Val, gotErr error) string {
	var ce *ErrCompile
	var ue *ErrUnsupported
	if errors.As(gotErr, &ce) || errors.As(gotErr, &ue) {
		return "the rendering does not compile on the Soul: " + gotErr.Error()
	}
	switch {
	case wantErr != nil && gotErr != nil:
		return ""
	case wantErr != nil:
		return "original errs (" + wantErr.Error() + "), rendering gives " + describe(gotVal)
	case gotErr != nil:
		return "original gives " + describe(wantVal) + ", rendering errs (" + gotErr.Error() + ")"
	}
	if !sameVal(wantVal, gotVal) {
		return "original gives " + describe(wantVal) + ", rendering gives " + describe(gotVal)
	}
	return ""
}

func describe(v ref.Val) string {
	return fmt.Sprintf("%s %#v", v.Type().TypeName(), toNative(v.Value()))
}

// sameVal is equality of value AND type, deep: cel-go's == is heterogeneous (1 == 1.0),
// and a rendering that turned an int into a double would pass it.
func sameVal(a, b ref.Val) bool {
	if a.Type().TypeName() != b.Type().TypeName() {
		return false
	}
	switch av := a.(type) {
	case types.Double:
		bv := b.(types.Double)
		return math.Float64bits(float64(av)) == math.Float64bits(float64(bv)) ||
			(math.IsNaN(float64(av)) && math.IsNaN(float64(bv)))
	case types.Timestamp:
		bv := b.(types.Timestamp)
		_, ao := av.Zone()
		_, bo := bv.Zone()
		return av.Time.Equal(bv.Time) && ao == bo
	case traits.Lister:
		bl := b.(traits.Lister)
		if av.Size() != bl.Size() {
			return false
		}
		n := av.Size().(types.Int)
		for i := types.Int(0); i < n; i++ {
			if !sameVal(av.Get(i), bl.Get(i)) {
				return false
			}
		}
		return true
	case traits.Mapper:
		bm := b.(traits.Mapper)
		if av.Size() != bm.Size() {
			return false
		}
		for it := av.Iterator(); it.HasNext() == types.True; {
			k := it.Next()
			bv, found := bm.Find(k)
			if !found || !sameVal(av.Get(k), bv) {
				return false
			}
		}
		return true
	}
	return a.Equal(b) == types.True
}

func runPredicateCases(t *testing.T, cases []predicateCase) {
	t.Helper()
	e := newFlowControlEngine(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { checkPredicateCase(t, e, c) })
	}
}

// TestRenderPredicate_SpecExample — the example predicate-rendering.md opens with.
func TestRenderPredicate_SpecExample(t *testing.T) {
	runPredicateCases(t, []predicateCase{{
		name:  "failed_when against another register",
		expr:  "register.self.stdout != register.probe.stdout",
		ctx:   hostContext(),
		want:  `register.self.stdout != dyn("bob")`,
		opens: []map[string]any{self(map[string]any{"stdout": "bob"}), self(map[string]any{"stdout": "alice"}), self(map[string]any{})},
	}})
}

// TestRenderPredicate_ResidualPrunerTraps — §2 "cel-go's residual pruner is not enough".
// Each shape is one the pruner leaves with its reference: a comprehension's body, both
// arms of an open ternary, `default()`'s fallback, and a bare `register`.
//
// Mutation: render through Env.ResidualAst over PartialVars instead of the own walk →
// the body, both arms and the fallback keep register.probe/input, and the rendering
// against the open set alone errs where the original evaluates.
func TestRenderPredicate_ResidualPrunerTraps(t *testing.T) {
	ctx := hostContext()
	runPredicateCases(t, []predicateCase{
		{
			name: "comprehension body",
			expr: "register.self.members.exists(m, m == register.probe.leader)",
			ctx:  ctx,
			want: `register.self.members.exists(m, m == dyn("a"))`,
			opens: []map[string]any{
				self(map[string]any{"members": []any{"a", "b"}}),
				self(map[string]any{"members": []any{"c"}}),
				self(map[string]any{"members": []any{}}),
				self(map[string]any{"members": "not a list"}),
			},
		},
		{
			name: "both arms of an open ternary",
			expr: "register.self.rc == 0 ? register.self.stdout == input.ok : register.self.stdout == input.bad",
			ctx:  ctx,
			want: `(register.self.rc == 0) ? (register.self.stdout == dyn("ready")) : (register.self.stdout == dyn("failed"))`,
			opens: []map[string]any{
				self(map[string]any{"rc": 0.0, "stdout": "ready"}),
				self(map[string]any{"rc": 0.0, "stdout": "failed"}),
				self(map[string]any{"rc": 1.0, "stdout": "failed"}),
				self(map[string]any{"rc": "x", "stdout": "failed"}),
			},
		},
		{
			name: "default fallback",
			expr: `default(register.self.out, register.probe.out) == "x"`,
			ctx:  ctx,
			want: `default(register.self.out, dyn("x")) == "x"`,
			opens: []map[string]any{
				self(map[string]any{"out": "x"}),
				self(map[string]any{"out": "y"}),
				self(map[string]any{}),
			},
		},
		{
			name: "map, filter, all, exists_one bodies",
			expr: `register.self.l.filter(x, x != vars.want).map(x, x + input.s) == [input.list[1] + input.s] && register.self.l.all(x, x in input.list) && register.self.l.exists_one(x, x == vars.want) && register.self.l.map(x, x != vars.want, x) == ["b"]`,
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"l": []any{"a", "b"}}),
				self(map[string]any{"l": []any{"b", "b"}}),
				self(map[string]any{"l": []any{"a", "c"}}),
				self(map[string]any{"l": []any{}}),
			},
		},
		{
			name: "comprehension over a decided range",
			expr: "input.list.exists(x, x == register.self.v)",
			ctx:  ctx,
			want: `dyn(["a", "b"]).exists(x, x == register.self.v)`,
			opens: []map[string]any{
				self(map[string]any{"v": "a"}),
				self(map[string]any{"v": "z"}),
				self(map[string]any{}),
			},
		},
		{name: "bare register: size", expr: "size(register) > 1", ctx: ctx, refuse: CodePredicateNotRenderable, reason: "without a field"},
		{name: "bare register: in", expr: `"probe" in register && register.self.rc == 0`, ctx: ctx, refuse: CodePredicateNotRenderable, reason: "without a field"},
		{name: "bare register: comprehension range", expr: `register.exists(k, k == "probe")`, ctx: ctx, refuse: CodePredicateNotRenderable, reason: "without a field"},
		{name: "bare register: equality", expr: "register == {}", ctx: ctx, refuse: CodePredicateNotRenderable, reason: "without a field"},
		{
			name: "an iteration variable named register is not the root",
			expr: "register.self.l.exists(register, register == 1)",
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"l": []any{1.0}}),
				self(map[string]any{"l": []any{2.0}}),
			},
		},
	})
}

// TestRenderPredicate_FieldArguments — §2 "Field arguments": the first argument of has()
// and default() is a field, never replaced by its value. has() over a closed read
// becomes its bool; default() over one becomes its own expansion `has(x) ? x : y`.
//
// Mutation: substitute a closed field argument like any other read → `has(dyn("v"))`
// and `default(dyn("px"), …)` do not compile, and the rendering is refused.
func TestRenderPredicate_FieldArguments(t *testing.T) {
	ctx := hostContext()
	present := withInput(ctx, "x", "v")
	runPredicateCases(t, []predicateCase{
		{
			name: "has() over a present closed read",
			expr: "has(input.x) && register.self.stdout != input.x",
			ctx:  present,
			want: `true && register.self.stdout != dyn("v")`,
			opens: []map[string]any{
				self(map[string]any{"stdout": "v"}),
				self(map[string]any{"stdout": "w"}),
				self(map[string]any{}),
			},
		},
		{
			name: "default() over a present closed field",
			expr: `default(register.probe.x, register.self.y) == "px"`,
			ctx:  ctx,
			want: `(true ? dyn("px") : register.self.y) == "px"`,
			opens: []map[string]any{
				self(map[string]any{"y": "px"}),
				self(map[string]any{}),
			},
		},
		{
			name: "default() over an absent closed field",
			expr: `default(register.probe.absent, register.self.y) == "px"`,
			ctx:  ctx,
			want: `(false ? dyn({}).keeper_eval_error : register.self.y) == "px"`,
			opens: []map[string]any{
				self(map[string]any{"y": "px"}),
				self(map[string]any{"y": "py"}),
				self(map[string]any{}),
			},
		},
		{
			name: "default() over an identifier",
			expr: "default(input, register.self.y).s == input.s",
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"y": map[string]any{"s": "other"}}),
			},
		},
		{
			name: "has() over another register's present field",
			expr: "has(register.probe.x) || register.self.rc == 0",
			ctx:  ctx,
			want: "true || register.self.rc == 0",
			opens: []map[string]any{
				self(map[string]any{"rc": 1.0}),
			},
		},
		{
			name: "has() over another register's absent field",
			expr: "has(register.probe.absent) || register.self.rc == 0",
			ctx:  ctx,
			want: "false || register.self.rc == 0",
			opens: []map[string]any{
				self(map[string]any{"rc": 1.0}),
				self(map[string]any{"rc": 0.0}),
			},
		},
		{
			name: "has() over the open register",
			expr: "has(register.self.x) && register.self.x == input.s",
			ctx:  ctx,
			want: `has(register.self.x) && register.self.x == dyn("str")`,
			opens: []map[string]any{
				self(map[string]any{"x": "str"}),
				self(map[string]any{"x": "s"}),
				self(map[string]any{}),
			},
		},
		{
			name: "has() whose operand is half open",
			expr: "has(input.cfg.m[register.self.k].f)",
			ctx:  ctx,
			want: `has(dyn({"k": {"f": 1}})[register.self.k].f)`,
			opens: []map[string]any{
				self(map[string]any{"k": "k"}),
				self(map[string]any{"k": "missing"}),
			},
		},
		{
			name: "default() whose field operand is half open",
			expr: "default(input.cfg.m[register.self.k].f, 0) == 1",
			ctx:  ctx,
			want: `default(dyn({"k": {"f": 1}})[register.self.k].f, 0) == 1`,
			opens: []map[string]any{
				self(map[string]any{"k": "k"}),
				self(map[string]any{"k": "missing"}),
			},
		},
	})
}

// TestRenderPredicate_Types — §2 "Types": a substituted value is wrapped in dyn(), so a
// JSON double against an int, or a string arm against a double arm, still compiles.
//
// Mutation: print a decided number without dyn() → `size(...) >= 3.0` and the mixed
// ternary no longer compile on the Soul.
func TestRenderPredicate_Types(t *testing.T) {
	ctx := hostContext()
	runPredicateCases(t, []predicateCase{
		{
			name: "JSON double against a YAML int",
			expr: "register.self.count == input.n",
			ctx:  ctx,
			want: "register.self.count == dyn(3)",
			opens: []map[string]any{
				self(map[string]any{"count": 3.0}),
				self(map[string]any{"count": 4.0}),
				self(map[string]any{"count": "3"}),
			},
		},
		{
			name: "size against a JSON double",
			expr: "size(register.self.members) >= register.probe.min",
			ctx:  ctx,
			want: "size(register.self.members) >= dyn(3.0)",
			opens: []map[string]any{
				self(map[string]any{"members": []any{"a", "b"}}),
				self(map[string]any{"members": []any{"a", "b", "c"}}),
			},
		},
		{
			// Equality has no cross-type overload at all: int == double is refused.
			name: "size against a JSON double under ==",
			expr: "size(register.self.members) == register.probe.min",
			ctx:  ctx,
			want: "size(register.self.members) == dyn(3.0)",
			opens: []map[string]any{
				self(map[string]any{"members": []any{"a", "b", "c"}}),
				self(map[string]any{"members": []any{"a"}}),
			},
		},
		{
			name: "ternary with a string arm and a double arm",
			expr: "register.self.ok ? input.s : register.probe.d",
			ctx:  ctx,
			want: `register.self.ok ? dyn("str") : dyn(2.5)`,
			opens: []map[string]any{
				self(map[string]any{"ok": true}),
				self(map[string]any{"ok": false}),
				self(map[string]any{"ok": "x"}),
			},
		},
		{
			// cel-go types a cross-type comparison of two literals only after the check
			// has entered a comprehension: the read compiled alone is refused.
			name: "a read whose key the original type-checks after a comprehension",
			expr: "register.self.l.exists(x, x == 1) && input.list[1 < 2.5 ? 0 : 1] == register.self.v",
			ctx:  ctx,
			want: `register.self.l.exists(x, x == 1) && dyn("a") == register.self.v`,
			opens: []map[string]any{
				self(map[string]any{"l": []any{1.0}, "v": "a"}),
				self(map[string]any{"l": []any{}, "v": "b"}),
			},
		},
		{
			// The same checker quirk the other way round: the read takes the
			// comprehension in its key with it, and the comparison after it no longer
			// type-checks unless the text sits inside one.
			name: "a read takes the comprehension the original type-checked after",
			expr: `input.list[input.list.exists(y, y == "a") ? 0 : 1] == register.self.v && size(register.self.l) >= 2.5`,
			ctx:  ctx,
			want: `[0].map(v0, dyn("a") == register.self.v && size(register.self.l) >= 2.5)[0]`,
			opens: []map[string]any{
				self(map[string]any{"l": []any{"a", "b", "c"}, "v": "a"}),
				self(map[string]any{"l": []any{"a"}, "v": "a"}),
			},
		},
		{
			// The original types the body over default()'s ternary; the fallback alone is
			// the range's string element, which || cannot take — so the expansion stays.
			name: "default() over a closed field keeps the ternary",
			expr: `{"x": 1}.filter(y, default(input.missing, y) || register.self.b) == ["x"]`,
			ctx:  ctx,
			want: `{"x": 1}.filter(y, (false ? dyn({}).keeper_eval_error : y) || register.self.b) == ["x"]`,
			opens: []map[string]any{
				self(map[string]any{"b": true}),
				self(map[string]any{"b": false}),
			},
		},
		{
			// An index's type is a free parameter: alone, `/` binds it to double, which is
			// then refused as a range. The expansion's ternary binds it to dyn first.
			name: "default() over a closed field keeps the ternary around an index",
			expr: "(input.n / default(register.probe.missing, register.self.l[false])).map(x, input.flag)",
			ctx:  ctx,
			want: "(dyn(3) / (false ? dyn({}).keeper_eval_error : (register.self.l[false]))).map(x, dyn(true))",
			opens: []map[string]any{
				self(map[string]any{"l": map[string]any{}}),
				self(map[string]any{"l": []any{}}),
			},
		},
		{
			name: "a ternary over a closed condition stays a ternary",
			expr: `(input.flag ? "s" : register.self.a) && register.self.b`,
			ctx:  ctx,
			want: `(dyn(true) ? "s" : register.self.a) && register.self.b`,
			opens: []map[string]any{
				self(map[string]any{"b": false}),
				self(map[string]any{"b": true}),
			},
		},
		{
			// An empty map literal has fresh type parameters, so a bare `{}.f` would type
			// the inner ternary bool and the outer one would have no overload.
			name: "an error term beside a typed arm",
			expr: `register.self.c ? {"k": register.self.b} : (register.self.d ? register.self.e == 1 : input.n.x)`,
			ctx:  ctx,
			want: `register.self.c ? {"k": register.self.b} : (register.self.d ? (register.self.e == 1) : dyn({}).keeper_eval_error)`,
			opens: []map[string]any{
				self(map[string]any{"c": true, "b": 1, "d": true, "e": 1}),
				self(map[string]any{"c": false, "d": true, "e": 1}),
				self(map[string]any{"c": false, "d": false}),
			},
		},
		{
			name: "mixed arithmetic errs in both",
			expr: "register.self.n + input.n > 5",
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"n": 3.0}),
				self(map[string]any{"n": 3}),
			},
		},
		{
			name: "uint, bytes and null",
			expr: "register.self.u == input.u || register.self.b == input.bytes || register.self.u == uint(input.n) || bytes(register.self.s) == bytes(input.s) || register.self.v == input.nullv",
			ctx:  ctx,
			want: `register.self.u == dyn(3u) || register.self.b == dyn(b"\163\164\162") || register.self.u == uint(dyn(3)) || bytes(register.self.s) == bytes(dyn("str")) || register.self.v == dyn(null)`,
			opens: []map[string]any{
				self(map[string]any{"u": uint64(3), "s": "x", "v": 1.0}),
				self(map[string]any{"u": 3.0, "s": "str", "v": 1.0}),
				self(map[string]any{"u": 3.0, "s": "x", "v": nil}),
				self(map[string]any{"u": 4.0, "s": "x", "v": 1.0}),
			},
		},
		{
			name: "a string the parser must read back byte for byte",
			expr: "register.self.s == input.tricky",
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"s": "a\nb\t\"c\\d' é 😀 \x00 end"}),
				self(map[string]any{"s": "a"}),
			},
		},
	})
}

// TestRenderPredicate_ErrorsTravel — §2 "Errors": an error the Keeper meets travels as a
// term and CEL's own logic absorbs it — or not — on the Soul.
//
// Mutation: refuse the task on the first error the Keeper meets → the `has(input.x) &&`
// case, which the original answers false, is refused instead.
func TestRenderPredicate_ErrorsTravel(t *testing.T) {
	ctx := hostContext()
	runPredicateCases(t, []predicateCase{
		{
			name:  "has() guards the absent value",
			expr:  "has(input.x) && register.self.stdout != input.x",
			ctx:   ctx,
			want:  "false && register.self.stdout != dyn({}).keeper_eval_error",
			opens: []map[string]any{self(map[string]any{"stdout": "v"}), self(map[string]any{})},
		},
		{
			name: "&& absorbs the error only when the open side is false",
			expr: "register.self.rc != 0 && register.self.stdout != input.x",
			ctx:  ctx,
			want: "register.self.rc != 0 && register.self.stdout != dyn({}).keeper_eval_error",
			opens: []map[string]any{
				self(map[string]any{"rc": 0.0, "stdout": "v"}),
				self(map[string]any{"rc": 1.0, "stdout": "v"}),
			},
		},
		{
			name: "|| absorbs the error only when the open side is true",
			expr: "register.self.rc == 0 || input.x == 1",
			ctx:  ctx,
			want: "register.self.rc == 0 || dyn({}).keeper_eval_error == 1",
			opens: []map[string]any{
				self(map[string]any{"rc": 0.0}),
				self(map[string]any{"rc": 1.0}),
			},
		},
		{
			name: "?: reaches the error on one arm",
			expr: "register.self.ok ? input.x : true",
			ctx:  ctx,
			want: "register.self.ok ? dyn({}).keeper_eval_error : true",
			opens: []map[string]any{
				self(map[string]any{"ok": true}),
				self(map[string]any{"ok": false}),
			},
		},
		{
			name: "exists() absorbs an element's error when another is true",
			expr: "register.self.l.exists(v, v == 1 || input.x)",
			ctx:  ctx,
			want: "register.self.l.exists(v, v == 1 || dyn({}).keeper_eval_error)",
			opens: []map[string]any{
				self(map[string]any{"l": []any{2.0, 1.0}}),
				self(map[string]any{"l": []any{2.0}}),
				self(map[string]any{"l": []any{}}),
			},
		},
		{
			name: "all() absorbs an element's error when another is false",
			expr: "register.self.l.all(v, v == 1 && input.x)",
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"l": []any{2.0, 1.0}}),
				self(map[string]any{"l": []any{1.0}}),
				self(map[string]any{"l": []any{}}),
			},
		},
		{
			name:  "a range that errs fails the comprehension",
			expr:  "input.x.exists(v, v == register.self.v)",
			ctx:   ctx,
			want:  "dyn({}).keeper_eval_error.exists(v, v == register.self.v)",
			opens: []map[string]any{self(map[string]any{"v": 1.0})},
		},
		{
			name: "default() reaches the erring fallback only when the field is absent",
			expr: "default(register.self.x, input.x) == 1",
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"x": 1.0}),
				self(map[string]any{}),
			},
		},
		{
			name:  "a strict call over an error errs",
			expr:  `register.self.s + input.x == "a"`,
			ctx:   ctx,
			want:  `register.self.s + dyn({}).keeper_eval_error == "a"`,
			opens: []map[string]any{self(map[string]any{"s": "a"})},
		},
		{
			name: "a non-bool operand of && errs unless the other side is false",
			expr: "input.s && register.self.ok",
			ctx:  ctx,
			want: `dyn("str") && register.self.ok`,
			opens: []map[string]any{
				self(map[string]any{"ok": false}),
				self(map[string]any{"ok": true}),
			},
		},
		{
			name: "a bool operand of && where the other need not be a bool",
			expr: "input.flag && register.self.v",
			ctx:  ctx,
			want: "dyn(true) && register.self.v",
			opens: []map[string]any{
				self(map[string]any{"v": "s"}),
				self(map[string]any{"v": true}),
				self(map[string]any{"v": false}),
			},
		},
	})
}

// TestRenderPredicate_ValuesTextCannotCarry — §2 "Values text cannot carry": a timestamp,
// a duration, NaN and the infinities through their constructors, a type by its name,
// and a refusal for what has no text: a timestamp RFC 3339 cannot print, invalid UTF-8,
// a type name an iteration variable captures, and a rendering past the parser's limits.
// A type value reaches a read only through a `loop:` item computed by CEL; YAML carries
// NaN and infinities as `.nan` and `.inf`.
//
// Mutation: print the timestamp without the offset check → the LMT offset's seconds
// are dropped and the comparison answers true where the original answers false.
func TestRenderPredicate_ValuesTextCannotCarry(t *testing.T) {
	ctx := hostContext()
	ctx.Loop = map[string]any{"t": types.IntType}
	nan := withInput(ctx, "a", math.NaN())
	inf := withInput(ctx, "a", math.Inf(1))
	runPredicateCases(t, []predicateCase{
		{
			name: "timestamp keeps its instant and its offset",
			expr: "timestamp(register.self.at) < vars.deadline && string(register.self.ok ? vars.deadline : vars.deadline) == register.self.text",
			ctx:  ctx,
			want: `timestamp(register.self.at) < dyn(timestamp("2026-10-06T12:00:00.000000005+03:00")) && string(register.self.ok ? dyn(timestamp("2026-10-06T12:00:00.000000005+03:00")) : dyn(timestamp("2026-10-06T12:00:00.000000005+03:00"))) == register.self.text`,
			opens: []map[string]any{
				self(map[string]any{"at": "2026-10-06T08:00:00Z", "ok": true, "text": "2026-10-06T12:00:00.000000005+03:00"}),
				self(map[string]any{"at": "2026-10-06T10:00:00Z", "ok": true, "text": "2026-10-06T12:00:00.000000005+03:00"}),
				self(map[string]any{"at": "2026-10-06T08:00:00Z", "ok": false, "text": "2026-10-06T09:00:00.000000005Z"}),
			},
		},
		{
			name: "duration",
			expr: "duration(register.self.elapsed) < vars.timeout",
			ctx:  ctx,
			want: `duration(register.self.elapsed) < dyn(duration("1m30s"))`,
			opens: []map[string]any{
				self(map[string]any{"elapsed": "89s"}),
				self(map[string]any{"elapsed": "91s"}),
			},
		},
		{
			name: "a type by its name",
			expr: "type(register.self.v) == t && type(register.self.v) == type(input.n)",
			ctx:  ctx,
			want: "type(register.self.v) == dyn(int) && type(register.self.v) == type(dyn(3))",
			opens: []map[string]any{
				self(map[string]any{"v": 1}),
				self(map[string]any{"v": 1.0}),
			},
		},
		{
			name: "list, map and null type names",
			expr: "type(register.self.v) == type(input.list) || type(register.self.v) == type(input.cfg) || type(register.self.v) == type(input.nullv)",
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"v": []any{}}),
				self(map[string]any{"v": map[string]any{}}),
				self(map[string]any{"v": nil}),
				self(map[string]any{"v": "s"}),
			},
		},
		{
			name:   "a type name an iteration variable captures",
			expr:   "register.self.l.exists(int, type(int) == t)",
			ctx:    ctx,
			refuse: CodePredicateNotRenderable,
			reason: `the iteration variable "int" captures its name`,
		},
		{
			name:  "NaN",
			expr:  "register.self.v < input.a || register.self.v != input.a",
			ctx:   nan,
			want:  `register.self.v < dyn(double("NaN")) || register.self.v != dyn(double("NaN"))`,
			opens: []map[string]any{self(map[string]any{"v": 1.0}), self(map[string]any{"v": math.NaN()})},
		},
		{
			name:  "infinity",
			expr:  "register.self.v < input.a && [input.a] == register.self.l",
			ctx:   inf,
			want:  `register.self.v < dyn(double("+Inf")) && [dyn(double("+Inf"))] == register.self.l`,
			opens: []map[string]any{self(map[string]any{"v": 1.0, "l": []any{math.Inf(1)}}), self(map[string]any{"v": math.Inf(1)})},
		},
		{
			name:   "a timestamp whose offset has seconds",
			expr:   "timestamp(register.self.at) == vars.lmt",
			ctx:    withVar(ctx, "lmt", time.Date(1850, 1, 1, 0, 0, 0, 0, time.FixedZone("LMT", 2*3600+30*60+17))),
			refuse: CodePredicateNotRenderable,
			reason: "an offset with seconds",
		},
		{
			name:   "a timestamp past year 9999",
			expr:   "string(vars.far) == register.self.s",
			ctx:    withVar(ctx, "far", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)),
			refuse: CodePredicateNotRenderable,
			reason: "outside the years 0001-9999",
		},
		{
			// Year 1 locally, year 0 UTC: timestamp() checks the instant.
			name:   "a timestamp before year 1 in UTC",
			expr:   "string(vars.early) == register.self.s",
			ctx:    withVar(ctx, "early", time.Date(1, 1, 1, 0, 30, 0, 0, time.FixedZone("", 3600))),
			refuse: CodePredicateNotRenderable,
			reason: "outside the years 0001-9999",
		},
		{
			name:   "a timestamp offset the parser rejects",
			expr:   "string(vars.wide) == register.self.s",
			ctx:    withVar(ctx, "wide", time.Date(2026, 3, 1, 12, 0, 0, 0, time.FixedZone("", 25*3600))),
			refuse: CodePredicateNotRenderable,
			reason: "does not read back",
		},
		{
			// Year 0 locally, year 1 UTC: in range, so carried.
			name:  "a timestamp whose UTC instant is in range",
			expr:  "string(vars.edge) == register.self.s",
			ctx:   withVar(ctx, "edge", time.Date(0, 12, 31, 23, 30, 0, 0, time.FixedZone("", -3600))),
			want:  `string(dyn(timestamp("0000-12-31T23:30:00-01:00"))) == register.self.s`,
			opens: []map[string]any{self(map[string]any{"s": "0000-12-31T23:30:00-01:00"}), self(map[string]any{"s": "x"})},
		},
		{
			name:   "a string that is not UTF-8",
			expr:   "register.self.s == input.bad",
			ctx:    withInput(ctx, "bad", "a\xffb"),
			refuse: CodePredicateNotRenderable,
			reason: "not valid UTF-8",
		},
		{
			// Arithmetic stays on the Soul, so a NaN it computes is the original's own.
			name:  "a NaN the Soul computes",
			expr:  "register.self.v < input.z / input.z",
			ctx:   withInput(ctx, "z", 0.0),
			want:  "register.self.v < dyn(0.0) / dyn(0.0)",
			opens: []map[string]any{self(map[string]any{"v": 1.0})},
		},
		{
			name:   "100 000 code points",
			expr:   "register.self.stdout == register.probe.big",
			ctx:    withRegister(ctx, "probe", map[string]any{"big": strings.Repeat("é", 100_000)}),
			refuse: CodePredicateNotRenderable,
			reason: "the Soul's parser stops at 100000",
		},
		{
			name:   "250 levels of nesting",
			expr:   "register.self.v == register.probe.deep",
			ctx:    withRegister(ctx, "probe", map[string]any{"deep": nestedList(260)}),
			refuse: CodePredicateNotRenderable,
			reason: "recursion limit exceeded",
		},
		{
			name: "nesting the parser still reads",
			expr: "register.self.v == register.probe.deep",
			ctx:  withRegister(ctx, "probe", map[string]any{"deep": nestedList(20)}),
			opens: []map[string]any{
				self(map[string]any{"v": nestedList(20)}),
				self(map[string]any{"v": nestedList(19)}),
			},
		},
	})
}

// TestRenderPredicate_HiddenReferences — §2 "Hidden references": an iteration variable
// is open and resolved by scope, so a variable named like a root or a type is the
// variable.
//
// Mutation: look a leftover reference up by NAME in the residual walk → the variable
// `input` reads as the root and the rendering is refused.
func TestRenderPredicate_HiddenReferences(t *testing.T) {
	ctx := hostContext()
	ctx.Loop = map[string]any{"item": map[string]any{"name": "x"}}
	runPredicateCases(t, []predicateCase{
		{
			name: "a variable named input",
			expr: `register.self.members.exists(input, input == "a")`,
			ctx:  ctx,
			want: `register.self.members.exists(input, input == "a")`,
			opens: []map[string]any{
				self(map[string]any{"members": []any{"a"}}),
				self(map[string]any{"members": []any{"b"}}),
			},
		},
		{
			name: "a variable named input beside a resolved root",
			expr: "input.s == register.self.s && register.self.l.exists(input, input == vars.want)",
			ctx:  ctx,
			want: `dyn("str") == register.self.s && register.self.l.exists(input, input == dyn("a"))`,
			opens: []map[string]any{
				self(map[string]any{"s": "str", "l": []any{"a"}}),
				self(map[string]any{"s": "str", "l": []any{"b"}}),
				self(map[string]any{"s": "x", "l": []any{"a"}}),
			},
		},
		{
			name: "a variable named like a type",
			expr: "register.self.l.exists(int, type(int) == int)",
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"l": []any{1.0}}),
				self(map[string]any{"l": []any{1}}),
			},
		},
		{
			name: "a loop variable is context and resolves",
			expr: "register.self.stdout == item.name",
			ctx:  ctx,
			want: `register.self.stdout == dyn("x")`,
			opens: []map[string]any{
				self(map[string]any{"stdout": "x"}),
				self(map[string]any{"stdout": "y"}),
			},
		},
		{
			name: "a variable shadowing the loop variable",
			expr: `register.self.l.exists(item, item == "x")`,
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"l": []any{"x"}}),
				self(map[string]any{"l": []any{"y"}}),
			},
		},
		{
			name: "soulprint.self and incarnation resolve",
			expr: "register.self.family == soulprint.self.os.family && incarnation.id != register.self.name",
			ctx:  ctx,
			want: `register.self.family == dyn("debian") && dyn("redis-prod") != register.self.name`,
			opens: []map[string]any{
				self(map[string]any{"family": "debian", "name": "x"}),
				self(map[string]any{"family": "debian", "name": "redis-prod"}),
			},
		},
	})
}

// TestRenderPredicate_GroupOpenSet — §4: inside a group the group's registers stay open
// beside register.self, and every other register is decided.
func TestRenderPredicate_GroupOpenSet(t *testing.T) {
	runPredicateCases(t, []predicateCase{{
		name: "sibling stays open, another register is decided",
		expr: "register.probe.stdout == register.self.stdout && register.other.x == input.n",
		ctx:  hostContext(),
		open: []string{RegisterSelf, "probe"},
		want: "register.probe.stdout == register.self.stdout && dyn(3.0) == dyn(3)",
		opens: []map[string]any{
			{RegisterSelf: map[string]any{"stdout": "a"}, "probe": map[string]any{"stdout": "a"}},
			{RegisterSelf: map[string]any{"stdout": "a"}, "probe": map[string]any{"stdout": "b"}},
		},
	}})
}

// TestRenderPredicate_UnaryRoundTrip — cel-go prints `!(!x)` as `!!x` and its parser
// drops an even run of `!` or `-`, so a rendering printed naively reads back as `x`:
// a value where the original errs, both in the text sent to the Soul and in a subtree
// the Keeper evaluates.
//
// Mutation: return e unchanged from groupForParser → `!!register.self.b` reads back as
// register.self.b, and `!!dyn("str")` as "str" instead of an error.
func TestRenderPredicate_UnaryRoundTrip(t *testing.T) {
	ctx := hostContext()
	runPredicateCases(t, []predicateCase{
		{
			name: "a negated default() over an absent closed field",
			expr: "!default(input.missing, !register.self.b)",
			ctx:  ctx,
			want: "!(false ? dyn({}).keeper_eval_error : !register.self.b)",
			opens: []map[string]any{
				self(map[string]any{"b": true}),
				self(map[string]any{"b": 1.0}),
			},
		},
		{
			name: "the author's own double negation beside a resolved root",
			expr: "!(!register.self.b) && input.flag",
			ctx:  ctx,
			want: "!dyn(!register.self.b) && dyn(true)",
			opens: []map[string]any{
				self(map[string]any{"b": true}),
				self(map[string]any{"b": "s"}),
			},
		},
		{
			name:  "a double negation over a resolved read",
			expr:  "register.self.a == !(!input.s)",
			ctx:   ctx,
			want:  `register.self.a == !dyn(!dyn("str"))`,
			opens: []map[string]any{self(map[string]any{"a": "str"})},
		},
		{
			// Printed bare, `(-x).f` would read back as `-(x.f)`.
			name: "a negation as a select operand, a member target and an index operand",
			expr: "(-register.self.l).size() == input.n || (-register.self.l)[0] == input.n || (-register.self.m).k == input.n",
			ctx:  ctx,
			want: "dyn(-register.self.l).size() == dyn(3) || dyn(-register.self.l)[0] == dyn(3) || dyn(-register.self.m).k == dyn(3)",
			opens: []map[string]any{
				self(map[string]any{"l": []any{-3, 1, 1}, "m": map[string]any{"k": -3}}),
				self(map[string]any{"l": 2, "m": 3}),
			},
		},
		{
			name: "a double arithmetic negation",
			expr: "register.self.n == -(-input.n) && register.self.n != -(-1)",
			ctx:  ctx,
			opens: []map[string]any{
				self(map[string]any{"n": 3}),
				self(map[string]any{"n": 1}),
			},
		},
	})
}

// TestRenderPredicate_SortedMapKeys — §3: the same values give the same text, whatever
// order Go iterates the map in.
//
// Mutation: drop the sort in mapLiteral → the keys print in iteration order.
func TestRenderPredicate_SortedMapKeys(t *testing.T) {
	ctx := hostContext()
	big := make(map[string]any, 40)
	for i := 0; i < 40; i++ {
		big[string(rune('a'+i%26))+strings.Repeat("k", i/26)] = i
	}
	ctx = withInput(ctx, "big", big)
	e := newFlowControlEngine(t)

	got, err := e.RenderPredicate("register.self.cfg == input.cfg", ctx, []string{RegisterSelf})
	if err != nil {
		t.Fatal(err)
	}
	if want := `register.self.cfg == dyn({"a": 2, "m": {"k": {"f": 1}}, "z": 1})`; got.Text != want {
		t.Fatalf("got  %s\nwant %s", got.Text, want)
	}
	first, err := e.RenderPredicate("register.self.cfg == input.big", ctx, []string{RegisterSelf})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := e.RenderPredicate("register.self.cfg == input.big", ctx, []string{RegisterSelf})
		if err != nil {
			t.Fatal(err)
		}
		if again.Text != first.Text {
			t.Fatalf("rendering differs between runs:\n%s\n%s", first.Text, again.Text)
		}
	}
}

// TestRenderPredicate_NothingResolved — a predicate that reads only the open set comes
// back as written and says so: it needs no new capability on the Soul (§5).
func TestRenderPredicate_NothingResolved(t *testing.T) {
	e := newFlowControlEngine(t)
	got, err := e.RenderPredicate("register.self.rc  != 0", hostContext(), []string{RegisterSelf})
	if err != nil {
		t.Fatal(err)
	}
	if got.Resolved || got.Text != "register.self.rc != 0" {
		t.Fatalf("got %+v, want the normalized original, unresolved", got)
	}
	got, err = e.RenderPredicate("register.self.rc != input.n", hostContext(), []string{RegisterSelf})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Resolved {
		t.Fatalf("got %+v, want resolved", got)
	}
}

// TestRenderPredicate_OriginalRefusals — what the Soul would refuse in the original is
// returned as the Soul's own error, not rendered.
func TestRenderPredicate_OriginalRefusals(t *testing.T) {
	e := newFlowControlEngine(t)
	var ce *ErrCompile
	if _, err := e.RenderPredicate("3.0 == 3 || register.self.ok", hostContext(), []string{RegisterSelf}); !errors.As(err, &ce) {
		t.Errorf("type error in the original: got %v, want ErrCompile", err)
	}
	var ue *ErrUnsupported
	if _, err := e.RenderPredicate(`register.self.x == vault("secret/x")`, hostContext(), []string{RegisterSelf}); !errors.As(err, &ue) {
		t.Errorf("vault() in the original: got %v, want ErrUnsupported", err)
	}
	if got, err := e.RenderPredicate("  ", hostContext(), []string{RegisterSelf}); err != nil || got.Text != "" {
		t.Errorf("empty predicate: got %+v, %v", got, err)
	}
	full, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := full.RenderPredicate("register.self.ok", hostContext(), []string{RegisterSelf}); err == nil {
		t.Error("a non-flow-control Engine rendered a predicate")
	}
}

// TestRenderPredicate_SoulGuardsOnSubstitutedText — the rendering is refused where the
// Soul's guards, as they stand, would reject or rewrite a substituted string: `vault(`
// in a value, and a whitespace run after an escaped quote, which normalize collapses
// (predicate-rendering.md §5, "The Soul's text guards"). The text never reaches a Soul
// that would read it differently.
func TestRenderPredicate_SoulGuardsOnSubstitutedText(t *testing.T) {
	ctx := withInput(withInput(hostContext(), "fn", "vault(x)"), "quoted", "a\"  b")
	runPredicateCases(t, []predicateCase{
		{name: "vault( in a value", expr: "register.self.s == input.fn", ctx: ctx, refuse: CodePredicateNotRenderable, reason: "vault("},
		{name: "escaped quote then two spaces", expr: "register.self.s == input.quoted", ctx: ctx, refuse: CodePredicateNotRenderable, reason: "normalization"},
	})
}

// TestRenderPredicate_RefusalNeverQuotesTheRendering — a refusal names the author's
// predicate and a reason; the rendered text, which can carry a substituted secret, is
// in neither. Every refusal past substitution is exercised with the secret in the value.
//
// Mutation: return err.Error() from soulCompileReason → ErrCompile embeds the rendered
// text, and the secret is in the message.
func TestRenderPredicate_RefusalNeverQuotesTheRendering(t *testing.T) {
	const secret = "s3cr3t-value"
	e := newFlowControlEngine(t)
	ctx := hostContext()
	for _, c := range []struct {
		name string
		expr string
		ctx  Vars
	}{
		{"guard", "register.self.s == input.v", withInput(ctx, "v", secret+" vault(x)")},
		{"normalization", "register.self.s == input.v", withInput(ctx, "v", secret+"\"  b")},
		{"size", "register.self.s == input.v", withInput(ctx, "v", secret+strings.Repeat("x", 100_000))},
		{"nesting", "register.self.s == input.v", withInput(ctx, "v", []any{secret, nestedList(260)})},
	} {
		_, err := e.RenderPredicate(c.expr, c.ctx, []string{RegisterSelf})
		var pe *ErrPredicateRender
		if !errors.As(err, &pe) {
			t.Errorf("%s: want a refusal, got %v", c.name, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the refusal quotes a substituted value: %s", c.name, err)
		}
	}
}

// TestRenderPredicate_RenderingIsNotCached — the rendered text is one host's and one
// run's; checking it through the Engine's compile cache would grow that cache by a text
// per host per run.
//
// Mutation: check the rendering with [Engine.compile] instead of compileUncached.
func TestRenderPredicate_RenderingIsNotCached(t *testing.T) {
	e := newFlowControlEngine(t)
	for i := 0; i < 5; i++ {
		got, err := e.RenderPredicate("register.self.n == input.n", withInput(hostContext(), "n", i), []string{RegisterSelf})
		if err != nil {
			t.Fatal(err)
		}
		e.mu.RLock()
		for key := range e.cache {
			if strings.Contains(key, got.Text) {
				t.Errorf("the rendered text %q is in the compile cache", got.Text)
			}
		}
		e.mu.RUnlock()
	}
}

// TestRenderPredicate_ResidualWalk — the walk that checks the rendered text reads
// nothing outside the open set, by scope.
func TestRenderPredicate_ResidualWalk(t *testing.T) {
	e := newFlowControlEngine(t)
	r := newPredicateRenderer(e, "x", Vars{Loop: map[string]any{"item": 1}}, []string{"item"}, []string{RegisterSelf}, nil)
	for _, text := range []string{
		"register.self.x == input.y",
		"register.probe.x == register.self.y",
		"register == {}",
		"register.self.l.exists(x, x == vars.y)",
		"item == register.self.v",
	} {
		var pe *ErrPredicateRender
		if err := r.checkResidual(text); !errors.As(err, &pe) || pe.Code != CodePredicateResidualReference {
			t.Errorf("checkResidual(%q) = %v, want %s", text, err, CodePredicateResidualReference)
		}
	}
	for _, text := range []string{
		"register.self.x == 1",
		"register.self.l.exists(input, input == 1)",
		"register.self.l.exists(register, register.x == 1)",
		"type(register.self.v) == int",
		"type(register.self.v) == google.protobuf.Timestamp",
		"dyn({}).keeper_eval_error",
	} {
		if err := r.checkResidual(text); err != nil {
			t.Errorf("checkResidual(%q) = %v, want nil", text, err)
		}
	}
}

// TestRenderPredicate_ComprehensionMacrosAreKnown — the walk recognizes a comprehension
// by name; a macro cel-go adds later would otherwise read as a plain call, and its
// variable as a root.
func TestRenderPredicate_ComprehensionMacrosAreKnown(t *testing.T) {
	e := newFlowControlEngine(t)
	for _, m := range e.env.Macros() {
		if !m.IsReceiverStyle() || m.ArgCount() < 2 {
			continue
		}
		if !comprehensionMacros[m.Function()] {
			t.Errorf("receiver macro %s/%d is not in comprehensionMacros", m.Function(), m.ArgCount())
		}
	}
}
