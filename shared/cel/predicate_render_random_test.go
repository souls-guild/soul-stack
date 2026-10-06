package cel

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// TestRenderPredicate_Randomized widens the differential test past the cases someone
// thought of: a seeded generator writes predicates mixing the open set, the decided
// context, literals, every non-strict operator, has()/default() and the comprehension
// macros — with iteration variables named like roots and types — and each one that
// compiles is rendered and compared over random open values. The seed is fixed, so a
// failure reproduces; the failing predicate is printed.
func TestRenderPredicate_Randomized(t *testing.T) {
	const predicates = 3000
	e := newFlowControlEngine(t)
	ctx := randomHostContext()
	g := &predicateGen{rng: rand.New(rand.NewPCG(928, 907))}

	var compiled, rendered int
	for i := 0; i < predicates; i++ {
		expr := g.expr(4, nil)
		got, err := e.RenderPredicate(expr, ctx, []string{RegisterSelf})
		var ce *ErrCompile
		var ue *ErrUnsupported
		if errors.As(err, &ce) || errors.As(err, &ue) {
			continue
		}
		compiled++
		var pe *ErrPredicateRender
		if errors.As(err, &pe) && (strings.Contains(pe.Reason, "captures its name") || strings.Contains(pe.Reason, "NaN or infinite")) {
			continue
		}
		if err != nil {
			t.Fatalf("RenderPredicate(%q): %v", expr, err)
		}
		rendered++
		for j := 0; j < 6; j++ {
			ov := self(g.openValue())
			full := ctx
			full.Register = mergeRegisters(ctx.Register, ov)
			wantVal, wantErr := e.EvalExpression(expr, full)
			gotVal, gotErr := e.EvalExpression(got.Text, Vars{Register: ov})
			if msg := sameOutcome(wantVal, wantErr, gotVal, gotErr); msg != "" {
				t.Fatalf("open set %v:\n original: %s\n rendered: %s\n %s", ov, expr, got.Text, msg)
			}
		}
	}
	// A generator that stopped producing compilable predicates would pass vacuously.
	if compiled < predicates/4 || rendered < compiled*9/10 {
		t.Fatalf("only %d of %d predicates compiled and %d rendered", compiled, predicates, rendered)
	}
}

func randomHostContext() Vars {
	return Vars{
		Input: map[string]any{
			"n": 1, "d": 2.5, "s": "a", "b": true,
			"l": []any{1, "a"},
			"m": map[string]any{"k": 1},
		},
		Register: map[string]any{
			"probe": map[string]any{"x": 1.0, "l": []any{"a", 2.0}, "m": map[string]any{"k": "a"}},
		},
		Vars:          map[string]any{"v": "a"},
		SoulprintSelf: map[string]any{"os": map[string]any{"family": "debian"}},
	}
}

type predicateGen struct {
	rng *rand.Rand
}

func (g *predicateGen) pick(options ...string) string {
	return options[g.rng.IntN(len(options))]
}

var (
	genOpenLeaves = []string{"register.self.a", "register.self.b", "register.self.l", "register.self.m"}
	genContext    = []string{
		"input.n", "input.d", "input.s", "input.b", "input.l", "input.m", "input.missing", "input.m.k",
		"register.probe.x", "register.probe.l", "register.probe.m", "register.probe.missing",
		"vars.v", "soulprint.self.os.family",
	}
	genLiterals = []string{`0`, `1`, `2.5`, `"a"`, `""`, `true`, `false`, `null`, `[]`, `[1, "a"]`, `{"k": 1}`, `int`, `string`}
	// Iteration variable names, roots and a type among them: scope, not name, decides.
	genIterVars = []string{"x", "y", "input", "register", "int"}
	// Selectable fields for has()/default().
	genFields = []string{"register.self.m.k", "register.self.a", "register.probe.m.k", "register.probe.missing", "input.m.k", "input.missing", "input.m"}
)

func (g *predicateGen) leaf(bound []string) string {
	switch n := g.rng.IntN(10); {
	case n < 3:
		return g.pick(genOpenLeaves...)
	case n < 6:
		return g.pick(genContext...)
	case n < 8 && len(bound) > 0:
		return bound[g.rng.IntN(len(bound))]
	default:
		return g.pick(genLiterals...)
	}
}

func (g *predicateGen) expr(depth int, bound []string) string {
	if depth == 0 {
		return g.leaf(bound)
	}
	sub := func() string { return g.expr(depth-1, bound) }
	switch g.rng.IntN(16) {
	case 0, 1:
		return g.leaf(bound)
	case 2, 3:
		return fmt.Sprintf("(%s %s %s)", sub(), g.pick("==", "!=", "<", ">=", "+", "-", "*", "/", "%", "in"), sub())
	case 4, 5:
		return fmt.Sprintf("(%s %s %s)", sub(), g.pick("&&", "||"), sub())
	case 6:
		switch op := g.pick("!", "-", "!!", "--"); op {
		case "!!", "--":
			// Parenthesized: the parser drops an unparenthesized even run itself.
			return fmt.Sprintf("%c(%c%s)", op[0], op[1], sub())
		default:
			return op + sub()
		}
	case 7:
		return fmt.Sprintf("(%s ? %s : %s)", sub(), sub(), sub())
	case 8:
		return fmt.Sprintf("%s(%s)", g.pick("size", "type", "string", "int"), sub())
	case 9:
		return fmt.Sprintf("has(%s)", g.field(bound))
	case 10:
		return fmt.Sprintf("default(%s, %s)", g.field(bound), sub())
	case 11, 12:
		v := g.pick(genIterVars...)
		inner := append(bound[:len(bound):len(bound)], v)
		body := g.expr(depth-1, inner)
		switch m := g.pick("exists", "all", "exists_one", "filter", "map", "map3"); m {
		case "map3":
			return fmt.Sprintf("%s.map(%s, %s, %s)", g.rangeExpr(depth, bound), v, body, g.expr(depth-1, inner))
		default:
			return fmt.Sprintf("%s.%s(%s, %s)", g.rangeExpr(depth, bound), m, v, body)
		}
	case 13:
		return fmt.Sprintf("[%s, %s]", sub(), sub())
	case 14:
		if g.rng.IntN(2) == 0 {
			return fmt.Sprintf("(%s).k", sub())
		}
		return fmt.Sprintf("%s[%s]", sub(), sub())
	default:
		return fmt.Sprintf("{%q: %s}", g.pick("k", "x"), sub())
	}
}

func (g *predicateGen) rangeExpr(depth int, bound []string) string {
	if g.rng.IntN(3) == 0 {
		return g.expr(depth-1, bound)
	}
	return g.pick("register.self.l", "register.self.m", "input.l", "input.m", "register.probe.l", "[1, 2.5]")
}

func (g *predicateGen) field(bound []string) string {
	if len(bound) > 0 && g.rng.IntN(4) == 0 {
		return bound[g.rng.IntN(len(bound))] + ".k"
	}
	return g.pick(genFields...)
}

// openValue draws register.self: each field is absent or one of a pool mixing the JSON
// types — and an int, which JSON never yields but a group sibling decoded elsewhere might.
// Every map here and in [randomHostContext] has ONE key: cel-go iterates a Go map in Go's
// order, so map()/filter() over two keys answers differently from one evaluation to the
// next — in the original as much as in the rendering.
func (g *predicateGen) openValue() map[string]any {
	pool := []any{
		0.0, 1.0, 2.5, -1.0, 1, "a", "", true, false, nil,
		[]any{}, []any{1.0, "a"}, []any{"a", "b"},
		map[string]any{"k": 1.0}, map[string]any{"x": true},
	}
	out := map[string]any{}
	for _, k := range []string{"a", "b", "l", "m"} {
		if g.rng.IntN(5) == 0 {
			continue
		}
		out[k] = pool[g.rng.IntN(len(pool))]
	}
	return out
}
