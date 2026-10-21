package servicevars

import (
	"reflect"
	"testing"
	"testing/fstest"
)

// TestSplitInterpolation_ScansAsTheEngine — a block closes at the first `}` its
// content parses at, so a map literal inside it does not end it; `\${` is literal
// text; a block that never closes is refused.
func TestSplitInterpolation_ScansAsTheEngine(t *testing.T) {
	for raw, want := range map[string][]Segment{
		"plain":                    {{Text: "plain"}},
		"${ vars.a }":              {{Text: "vars.a", Expr: true}},
		"a-${ {'k': 1}.k }-${ x }": {{Text: "a-"}, {Text: "{'k': 1}.k", Expr: true}, {Text: "-"}, {Text: "x", Expr: true}},
		`\${ vars.a }`:             {{Text: "${ vars.a }"}},
	} {
		got, ok := SplitInterpolation(raw)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("SplitInterpolation(%q) = %#v, %v; want %#v", raw, got, ok, want)
		}
	}
	if _, ok := SplitInterpolation("${ vars.a"); ok {
		t.Error("an unclosed block must be refused")
	}
}

// TestExprReads_NamesAndGuards — which names an expression reads off `vars`, which
// of them it tests has() or `in` on, which it reads only where a key test on them
// holds, and whether it reads the namespace whole. A read whose result a bool
// literal decides is no read, unless it is a whole comparison, which the Keeper
// materializes the view for before it evaluates.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in readWalk.expr drop the
// `_in_` case — "key test" reports a whole read and "key-tested read" loses its
// guard; or drop the IdentKind case — every "whole" row goes red; in readWalk.call
// walk both branches of a key-tested `?:` with w.held — the rows reading vars.a in
// the branch the test fails go red; drop the LogicalAnd/LogicalOr case — the `&&`
// and `||` rows go red; in readWalk.read record a read in a vacuous place again
// (`w.reads[name] = true` ahead of the vacuous check) — the literal rows reading a
// name go red; or drop `&& w.vacuous == 0` from the IdentKind case — "true ? 1 :
// vars" goes red.
func TestExprReads_NamesAndGuards(t *testing.T) {
	keyed := []VarRead{{Name: "a", Guarded: true, KeyTested: true}}
	open := []VarRead{{Name: "a", Guarded: true}}
	for expr, want := range map[string]struct {
		reads []VarRead
		whole bool
	}{
		"vars.a + vars['b']":                                     {reads: []VarRead{{Name: "a"}, {Name: "b"}}},
		"has(vars.a) ? vars.a : 1":                               {reads: []VarRead{{Name: "a", Guarded: true}}},
		"default(vars.a, 1)":                                     {reads: []VarRead{{Name: "a", Guarded: true}}},
		"default(vars.a.b, 1)":                                   {reads: []VarRead{{Name: "a"}}},
		"has(vars.a.b)":                                          {reads: []VarRead{{Name: "a"}}},
		".vars.a":                                                {reads: []VarRead{{Name: "a"}}},
		"'a' in vars ? vars.a : 1":                               {reads: keyed},
		"'a' in vars && vars.a > 0":                              {reads: keyed},
		"vars.a > 0 && 'a' in vars":                              {reads: keyed},
		"'a' in vars && (vars.a > 0 && vars.a < 9)":              {reads: keyed},
		"!('a' in vars) || vars.a > 0":                           {reads: keyed},
		"!('a' in vars) ? 1 : vars.a":                            {reads: keyed},
		"'a' in vars ? 1 : vars.a":                               {reads: open},
		"'a' in vars ? vars.a : vars.a":                          {reads: open},
		"'a' in vars || vars.a > 0":                              {reads: open},
		"!('a' in vars) && vars.a > 0":                           {reads: open},
		"has(vars.a) && vars.a > 0":                              {reads: []VarRead{{Name: "a", Guarded: true}}},
		"'a' in vars":                                            {},
		"vars[k] + size(vars) + vars.size()":                     {},
		"vars.exists(k, k == 'a')":                               {},
		"[1].map(vars, vars.x) + [vars.y]":                       {reads: []VarRead{{Name: "y"}}},
		"'vars.a' + string(vars.c)":                              {reads: []VarRead{{Name: "c"}}},
		"vars":                                                   {whole: true},
		"merge(vars, {'a': 1})":                                  {whole: true},
		"vars == {}":                                             {whole: true},
		"vars in [{}]":                                           {whole: true},
		"'x' in [vars]":                                          {whole: true},
		"soulprint.hosts.where('vars.a == 1 && has(vars.b)')":    {reads: []VarRead{{Name: "a"}, {Name: "b", Guarded: true}}},
		"not ( valid":                                            {},
		"true ? 1 : vars.a":                                      {},
		"false ? vars.a : vars.b":                                {reads: []VarRead{{Name: "b"}}},
		"false && vars.a":                                        {},
		"vars.a || true":                                         {},
		"(vars.a && false) ? vars.b : 1":                         {},
		"true ? 1 : vars":                                        {},
		"true ? vars.a : 1":                                      {reads: []VarRead{{Name: "a"}}},
		"false && vars != {}":                                    {whole: true},
		"true || [vars].exists(m, m != {})":                      {whole: true},
		"false && .vars == {}":                                   {whole: true},
		"true ? 1 : merge({'x': vars}).x != {}":                  {whole: true},
		"true ? 1 : [vars][0].a == 1":                            {},
		"false && [1].exists(vars, vars == 1)":                   {},
		"false && size(soulprint.hosts.where('vars != {}')) > 0": {whole: true},
	} {
		reads, whole := ExprReads(expr)
		if (len(reads) != 0 || len(want.reads) != 0) && !reflect.DeepEqual(reads, want.reads) || whole != want.whole {
			t.Errorf("ExprReads(%q) = %#v, %v; want %#v, %v", expr, reads, whole, want.reads, want.whole)
		}
	}
}

// TestEveryLayer_TheFilesTheKeeperCanRead — without `_stack.yaml` the files
// directly inside vars/, lexically; with one, each step's files in step order and
// with the step's strategy, a composed `file:` contributing every file its skeleton
// matches, lexically, at its step's position. A file no step can name is not a
// layer.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in EveryLayer take the
// lexical branch whatever the stack — "stack" goes red; or walk every file under
// vars/ sorted by path, as before NIM-1014's fix round — "no stack" goes red.
func TestEveryLayer_TheFilesTheKeeperCanRead(t *testing.T) {
	origins := func(t *testing.T, fsys fstest.MapFS) []string {
		t.Helper()
		layers, _, err := EveryLayer(fsys)
		if err != nil {
			t.Fatalf("EveryLayer: %v", err)
		}
		var out []string
		for _, l := range layers {
			out = append(out, l.Origin)
		}
		return out
	}
	t.Run("no stack", func(t *testing.T) {
		got := origins(t, fstest.MapFS{
			"vars/00-base.yaml":    {Data: []byte("a: 1\n")},
			"vars/coven/prod.yaml": {Data: []byte("b: 2\n")},
			"vars/coven.yaml":      {Data: []byte("c: 3\n")},
			"vars/README.md":       {Data: []byte("prose\n")},
		})
		if want := []string{"vars/00-base.yaml", "vars/coven.yaml"}; !reflect.DeepEqual(got, want) {
			t.Errorf("origins = %v, want %v", got, want)
		}
	})
	t.Run("stack", func(t *testing.T) {
		fsys := fstest.MapFS{
			"vars/_stack.yaml": {Data: []byte("stack:\n  - file: defaults.yaml\n" +
				"  - foreach: \"${ incarnation.covens }\"\n    as: coven\n    file: \"./coven/${ coven }.yaml\"\n    optional: true\n" +
				"  - file: 00-late.yaml\n    strategy: replace\n")},
			"vars/defaults.yaml":       {Data: []byte("a: 1\n")},
			"vars/coven/stage.yaml":    {Data: []byte("b: 2\n")},
			"vars/coven/prod.yaml":     {Data: []byte("b: 3\n")},
			"vars/coven/deep/x.yaml":   {Data: []byte("b: 4\n")},
			"vars/00-late.yaml":        {Data: []byte("c: 5\n")},
			"vars/99-unstacked.yaml":   {Data: []byte("d: 6\n")},
			"vars/elsewhere/dead.yaml": {Data: []byte("e: 7\n")},
		}
		want := []string{
			"vars/_stack.yaml step 1: vars/defaults.yaml",
			"vars/_stack.yaml step 2: vars/coven/prod.yaml",
			"vars/_stack.yaml step 2: vars/coven/stage.yaml",
			"vars/_stack.yaml step 3: vars/00-late.yaml",
		}
		if got := origins(t, fsys); !reflect.DeepEqual(got, want) {
			t.Fatalf("origins = %v, want %v", got, want)
		}
		layers, _, _ := EveryLayer(fsys)
		if s := layers[3].Strategy["c"]; s != "replace" {
			t.Errorf("a step's strategy is its layers', got %q", s)
		}
	})
	if none, _, err := EveryLayer(fstest.MapFS{}); err != nil || len(none) != 0 {
		t.Errorf("no vars/ is no layers, got %v, %v", none, err)
	}
	if _, _, err := EveryLayer(fstest.MapFS{"vars/00-base.yaml": {Data: []byte("a: [\n")}}); err == nil {
		t.Error("a layer that does not parse is an error, not a layer dropped")
	}
}

// TestEveryLayer_OrderAndConditionality — a step whose `foreach:` and `file:` read
// nothing of `incarnation.*` is evaluated, so its files are stacked in binding
// order, as the Keeper stacks them, and for every incarnation; one that reads
// `incarnation.*` contributes its files lexically and conditionally. A `when:` makes
// a step conditional; `optional:` alone does not, since the file is in the snapshot.
// A conditional step whose composed `file:` has no `foreach:` names one of its files
// per incarnation, so they are OneOf; a `foreach:` step's are not, nor is a literal
// `file:`'s.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in EveryLayer skip
// fixedFiles (`fixed := false`) — "binding order" goes red; mark every stack layer
// Conditional — the unconditional rows go red; add `|| step.Optional` to
// Conditional — the optional row goes red; or drop `step.Foreach == ""` from oneOf
// — the coven rows go red.
func TestEveryLayer_OrderAndConditionality(t *testing.T) {
	layers, _, err := EveryLayer(snapshot(map[string]string{
		"_stack.yaml": "stack:\n" +
			"  - file: 00-base.yaml\n    optional: true\n" +
			"  - foreach: \"${ ['b', 'a'] }\"\n    as: f\n    file: \"l/${ f }.yaml\"\n" +
			"  - foreach: \"${ incarnation.covens }\"\n    as: c\n    file: \"c/${ c }.yaml\"\n    optional: true\n" +
			"  - file: 90-wb.yaml\n    when: \"'wb' in incarnation.covens\"\n" +
			"  - file: \"inc/${ incarnation.id }.yaml\"\n    optional: true\n",
		"00-base.yaml": "a: 1\n",
		"l/a.yaml":     "x: 1\n",
		"l/b.yaml":     "x: 2\n",
		"c/stage.yaml": "y: 1\n",
		"c/prod.yaml":  "y: 2\n",
		"90-wb.yaml":   "z: 1\n",
		"inc/a.yaml":   "w: 1\n",
		"inc/b.yaml":   "w: 2\n",
	}))
	if err != nil {
		t.Fatalf("EveryLayer: %v", err)
	}
	type row struct {
		origin             string
		conditional, oneOf bool
	}
	var got []row
	for _, l := range layers {
		got = append(got, row{l.Origin, l.Conditional, l.OneOf})
	}
	want := []row{
		{"vars/_stack.yaml step 1: vars/00-base.yaml", false, false},
		{"vars/_stack.yaml step 2: vars/l/b.yaml", false, false},
		{"vars/_stack.yaml step 2: vars/l/a.yaml", false, false},
		{"vars/_stack.yaml step 3: vars/c/prod.yaml", true, false},
		{"vars/_stack.yaml step 3: vars/c/stage.yaml", true, false},
		{"vars/_stack.yaml step 4: vars/90-wb.yaml", true, false},
		{"vars/_stack.yaml step 5: vars/inc/a.yaml", true, true},
		{"vars/_stack.yaml step 5: vars/inc/b.yaml", true, true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("binding order: layers = %v\nwant %v", got, want)
	}
}

// TestStepFileMatcher_OneSegmentPerBlock — a literal `file:` names its cleaned path;
// a composed one matches its cleaned skeleton, each `${ … }` block standing for any
// text inside ONE path segment, the skeleton's own text matched literally and to
// the end of the path.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: map a block to `.*` — the
// "crosses a directory" rows go red; drop the path.Clean of the skeleton — the
// "./" rows go red; drop regexp.QuoteMeta — "tier.${ t }.yaml" matches
// tier/gold.yaml and the `(` row panics; or drop the `$` anchor — the row of a
// directory named prod.yaml goes red.
func TestStepFileMatcher_OneSegmentPerBlock(t *testing.T) {
	for _, tc := range []struct {
		file, rel string
		want      bool
	}{
		{"coven/prod.yaml", "coven/prod.yaml", true},
		{"./coven/prod.yaml", "coven/prod.yaml", true},
		{"./coven/${ c }.yaml", "coven/prod.yaml", true},
		{"coven/${ c }.yaml", "coven/prod.yaml", true},
		{"tier-${ t }-${ e }.yaml", "tier-gold-prd.yaml", true},
		{"${ c }.yaml", "essence/coven/x.yaml", false},
		{"${ c }.yaml", "coven/x.yaml", false},
		{"coven/${ c }.yaml", "coven/deep/x.yaml", false},
		{"coven/prod.yaml", "coven/stage.yaml", false},
		{"tier.${ t }.yaml", "tier.gold.yaml", true},
		{"tier.${ t }.yaml", "tier/gold.yaml", false},
		{"a+(${ x }.yaml", "a+(b.yaml", true},
		{"a+(${ x }.yaml", "aa(b.yaml", false},
		{"coven/${ c }.yaml", "coven/prod.yaml/x.yaml", false},
	} {
		if got := StepFileMatcher(tc.file)(tc.rel); got != tc.want {
			t.Errorf("StepFileMatcher(%q)(%q) = %v, want %v", tc.file, tc.rel, got, tc.want)
		}
	}
}
