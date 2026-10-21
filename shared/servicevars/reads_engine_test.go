package servicevars

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/cel-go/common/types"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// The static reading of reads.go mirrors two unexported pieces of the engine — the
// `${ … }` scanner and the `.where("…")` rewrite — so each is pinned against the
// engine itself rather than against expectations written by hand.

// TestSplitInterpolation_AgreesWithTheEngine — the blocks SplitInterpolation finds
// are the engine's: evaluating them one by one and joining them with the literal
// text gives what EvalInterpolation gives for the whole string, and a string the
// engine cannot scan is exactly one SplitInterpolation refuses.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in closeBlock close a block
// at its first `}` without asking the parser — every row whose block holds a `}`
// goes red; or drop defaultMacro from macroParser — "default over an expression"
// goes red.
func TestSplitInterpolation_AgreesWithTheEngine(t *testing.T) {
	eng, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	unscannable := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "without a closing }")
	}
	for _, raw := range []string{
		"${ {'a': '}'}.a }",
		"x-${ {'k': {'j': 'v'}}.k.j }-y",
		"${ {'a': 1}.a == 1 ? 'y' : 'n' }",
		"${ '}' + \"}\" }}",
		`${ 'it\'s }' }`,
		`${ "a\"}b" }`,
		"${ r'\\}' }",
		"${ '''}''' }",
		"${ ['}', 'a'][0] }",
		"${ '${' }",
		`\${ literal }-${ 'b' }`,
		`a\${b}${ 'c' }\${`,
		"${ 1 }}",
		"${ \"}\"",
		"${ default(input.x, 'd') }",
		"${ default(size(input), 0) } and ${ 'b' }",
		"${ 'a' } ${",
	} {
		t.Run(raw, func(t *testing.T) {
			want, werr := eng.EvalInterpolation(raw, cel.Vars{})
			segs, ok := SplitInterpolation(raw)
			if !ok || unscannable(werr) {
				if ok != !unscannable(werr) {
					t.Fatalf("SplitInterpolation ok = %v, the engine: %v", ok, werr)
				}
				return
			}
			got, gerr := evalSegments(eng, segs)
			if (gerr != nil) != (werr != nil) {
				t.Fatalf("the blocks give %#v, %v; the engine %#v, %v", got, gerr, want, werr)
			}
			if gerr == nil && got != want {
				t.Fatalf("the blocks give %#v, the engine %#v", got, want)
			}
		})
	}
}

// evalSegments evaluates segs as EvalInterpolation does with its own blocks: one
// whole block keeps its value, anything else is joined as text.
func evalSegments(eng *cel.Engine, segs []Segment) (any, error) {
	if len(segs) == 1 && segs[0].Expr {
		val, err := eng.EvalExpression(segs[0].Text, cel.Vars{})
		if err != nil {
			return nil, err
		}
		return val.Value(), nil
	}
	var b strings.Builder
	for _, s := range segs {
		if !s.Expr {
			b.WriteString(s.Text)
			continue
		}
		val, err := eng.EvalExpression(s.Text, cel.Vars{})
		if err != nil {
			return nil, err
		}
		str, ok := val.ConvertToType(types.StringType).Value().(string)
		if !ok {
			return nil, fmt.Errorf("block %q is not text: %v", s.Text, val)
		}
		b.WriteString(str)
	}
	return b.String(), nil
}

// TestExprReads_WhereAgreesWithTheEngine — the names read inside a
// `soulprint.hosts.where("…")` predicate are the ones the engine's
// InterpolationReads reports, which reads the expression after its rewrite inlines
// the predicate. Where the rewrite refuses the expression nothing is inlined, here
// as there.
//
// The rewrite does not descend into a map or struct literal, so a where-call there
// is neither inlined nor judged.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: make whereInlinable return
// true for every expression — the refused rows go red; or false — the inlined rows
// go red; in readWalk.expr keep inlineWhere inside a literal — the literal rows go
// red; or let whereInlinable judge every where-call through ast.PostOrderVisit — the
// row whose literal holds `.where(vars.p)` goes red.
func TestExprReads_WhereAgreesWithTheEngine(t *testing.T) {
	eng, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, expr := range []string{
		`soulprint.hosts.where("vars.a == role")`,
		`soulprint.where("vars.a == 1").where("has(vars.b)")`,
		`size(soulprint.hosts.where("covens.exists(c, c == vars.zone)")) + vars.n`,
		`soulprint.hosts.where("vars['lit'] != '' && default(vars.d, 1) == 1")`,
		`soulprint.hosts.where(vars.p)`,
		`input.list.where("vars.a == 1")`,
		`soulprint.hosts.where("vars.a", "vars.b")`,
		`soulprint.hosts.where("soulprint.hosts.where('vars.a') == []")`,
		`soulprint.hosts.where("vars.a == 1") + [vars.c]`,
		`{'k': soulprint.hosts.where("vars.q == 1")}`,
		`[{'k': soulprint.hosts.where("vars.q == 1")}]`,
		`google.protobuf.Struct{fields: soulprint.hosts.where("vars.q == 1")}`,
		`soulprint.hosts.where("vars.a == 1") + [{'k': soulprint.hosts.where(vars.p)}]`,
	} {
		t.Run(expr, func(t *testing.T) {
			want := slices.Sorted(maps.Keys(eng.InterpolationReads("${ "+expr+" }", []string{"vars"})["vars"].Fields))
			reads, _ := ExprReads(expr)
			got := []string{}
			for _, r := range reads {
				got = append(got, r.Name)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("ExprReads reads %v, the engine %v", got, want)
			}
		})
	}
}
