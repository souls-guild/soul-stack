package cel

import (
	"reflect"
	"strings"
	"testing"
)

// TestComprehensionOverAMap_IteratesInKeyOrder — a comprehension over a map walks its
// keys in sorted order, so of several failing elements the first in order is the one
// reported on every evaluation, and `.map(…)` builds the same list every time. Each
// root, and the lazy `vars`, reaches the engine's adapter, and so does a map inside an
// adapted map or list and the result of vault() and merge().
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: return m.Mapper.Iterator() from
// orderedMap.Iterator. Every case goes red. Each of these turns its cases red:
//   - types.DefaultTypeAdapter.NativeToValue(val) returned from lazyVars.Find: the lazy
//     `vars`;
//   - orderedAdapter.NativeToValue building a map[string]any over a.Adapter instead of
//     a: a map in a map, through `vars` and through a list literal; the same for the
//     reflect.Map branch: a map in a typed input map;
//   - its `[]any` case handing the list to a.Adapter: a map in a lazy vars list;
//   - callVault adapting through types.DefaultTypeAdapter: the vault() result for the
//     whole secret, the vault() field for `#field`;
//   - callMergeList adapting through types.DefaultTypeAdapter: merge() of a list.
func TestComprehensionOverAMap_IteratesInKeyOrder(t *testing.T) {
	e := newEngine(t)
	m := func() map[string]any { return map[string]any{"e": 5, "c": 3, "a": 1, "d": 4, "b": 2} }
	view := func() *VarView { return NewVarView(nil, taskLayer(e, map[string]any{"m": m()})) }
	cases := map[string]struct {
		expr string
		vars func() Vars
	}{
		"an input map":           {"input.m.all(k, input[k] > 0)", func() Vars { return Vars{Input: map[string]any{"m": m()}} }},
		"a map in an input list": {"input.l[0].exists(k, input[k] > 0)", func() Vars { return Vars{Input: map[string]any{"l": []any{m()}}} }},
		"a lazy vars map":        {"vars.m.all(k, vars[k] > 0)", func() Vars { return Vars{View: view()} }},
		"a vars map":             {"vars.m.all(k, vars[k] > 0)", func() Vars { return Vars{Vars: map[string]any{"m": m()}} }},
		"a register map":         {"register.p.all(k, input[k] > 0)", func() Vars { return Vars{Register: map[string]any{"p": m()}} }},
		"a merge() result":       {"merge(input.m, {}).all(k, input[k] > 0)", func() Vars { return Vars{Input: map[string]any{"m": m()}} }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for i := range 50 {
				_, err := e.EvalExpression(tc.expr, tc.vars())
				if err == nil || !strings.HasSuffix(err.Error(), "no such key: a") {
					t.Fatalf("evaluation %d: err = %v, want the first key in order, a, to fail", i, err)
				}
			}
		})
	}

	ve := newVaultEngine(t, &stubKV{secrets: map[string]map[string]any{
		"secret/m":   m(),
		"secret/cfg": {"tls": m()},
	}})
	viewOf := func(defs map[string]any) func() Vars {
		return func() Vars { return Vars{View: NewVarView(nil, taskLayer(ve, defs))} }
	}
	for name, tc := range map[string]struct {
		expr string
		vars func() Vars
	}{
		"an input map":             {"input.m.map(k, k)", func() Vars { return Vars{Input: map[string]any{"m": m()}} }},
		"a lazy vars map":          {"vars.m.map(k, k)", viewOf(map[string]any{"m": m()})},
		"a map in a lazy vars map": {"vars.cfg.tls.map(k, k)", viewOf(map[string]any{"cfg": map[string]any{"tls": m()}})},
		"a map in a typed input map": {"input.cfg.tls.map(k, k)", func() Vars {
			return Vars{Input: map[string]any{"cfg": map[string]map[string]int{"tls": {"e": 5, "c": 3, "a": 1, "d": 4, "b": 2}}}}
		}},
		"a map in a lazy vars list":        {"vars.l[0].map(k, k)", viewOf(map[string]any{"l": []any{m()}})},
		"a map in a map in a list literal": {"[input.n][0].z.map(k, k)", func() Vars { return Vars{Input: map[string]any{"n": map[string]any{"z": m()}}} }},
		"a vault() result":                 {"vault('secret/m').map(k, k)", func() Vars { return Vars{} }},
		"a vault() field":                  {"vault('secret/cfg#tls').map(k, k)", func() Vars { return Vars{} }},
		"a merge() of a list of maps": {"merge([input.m, input.n]).map(k, k)", func() Vars {
			return Vars{Input: map[string]any{"m": map[string]any{"e": 5, "c": 3}, "n": map[string]any{"a": 1, "d": 4, "b": 2}}}
		}},
	} {
		t.Run(name+" through .map", func(t *testing.T) {
			want := []any{"a", "b", "c", "d", "e"}
			for i := range 50 {
				got, err := ve.EvalInterpolation("${ "+tc.expr+" }", tc.vars())
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("evaluation %d: %#v, %v; want %v", i, got, err, want)
				}
			}
		})
	}
}

// TestEvalInterpolation_MapKeysThatConvertToOneKeyAreAnError — `1` and `'1'` both become
// the native key "1"; which value it kept was Go's map order. It is an error naming the
// key, the smallest when several collide, on every evaluation.
//
// Of two inner maps that each collide, the one under the first outer key in order is
// converted first and named.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the clashes check from
// nativeMap, so the later key wins. The colliding cases go red. Naming clashes[0]
// instead of slices.Min(clashes) turns the second red. Ranging over byName instead of
// its sorted keys turns the nested one red.
func TestEvalInterpolation_MapKeysThatConvertToOneKeyAreAnError(t *testing.T) {
	e := newEngine(t)
	for expr, key := range map[string]string{
		"${ {1: 'int', '1': 'string'} }":                          `"1"`,
		"${ {2: 'x', '2': 'y', 1: 'p', '1': 'q', 'z': 'z'} }":     `"1"`,
		"${ merge({1: 'int'}, {'1': 'string'}) }":                 `"1"`,
		"${ {'x': {1: 'a', '1': 'b'}, 'y': {2: 'a', '2': 'b'}} }": `"1"`,
	} {
		t.Run(expr, func(t *testing.T) {
			for i := range 50 {
				got, err := e.EvalInterpolation(expr, Vars{})
				if err == nil || !strings.HasSuffix(err.Error(), "two map keys convert to the same key "+key) {
					t.Fatalf("evaluation %d: %#v, %v; want an error naming %s", i, got, err, key)
				}
			}
		})
	}

	t.Run("keys of two kinds that stay apart", func(t *testing.T) {
		got, err := e.EvalInterpolation("${ {1: 'int', 'a': 'string', true: 'bool'} }", Vars{})
		want := map[string]any{"1": "int", "a": "string", "true": "bool"}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%#v, %v; want %v", got, err, want)
		}
	})
}
