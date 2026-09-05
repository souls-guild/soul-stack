package render

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// ★ The producer guard for the incarnation-state seal (NIM-826).
//
// [RenderInput.SecretStateFields] is supplied by the caller, and a caller that
// forgets it gets no error: the render succeeds, the params carry the state
// secret in plaintext, and the seal is simply empty. That is the same fail-open
// shape as every hop in this family — NIM-811's fields were declared and never
// populated, and nothing said so for as long as it lasted.
//
// So the omission is made unrepresentable the way [activationRoots] made an
// unclassified activation root unrepresentable: every builder of a RenderInput
// under keeper/internal is listed below with its stance, and a new one fails
// this test until somebody decides. A stance is the SOURCE TEXT of the value,
// not a was-it-set flag: a builder switched from the manifest's answer to a nil
// literal must redden.
var secretStateFieldsStance = map[string]struct {
	want   string
	reason string
}{
	"scenario.run": {
		want: "incarnation.StateSchemaSecretFields(art)",
		reason: "THE run path: the only builder that collects a seal (Sealed non-nil) and reads state, " +
			"so it is the one this ticket exists for",
	},
	"render.renderApplyDestiny": {
		want: "parentIn.SecretStateFields",
		reason: "carried over although State is not, so that forwarding State one day is one edit rather " +
			"than two",
	},
	"scenario.RenderForHost": {
		reason: "the Acolyte re-renders a claimed run and leaves Sealed nil — nothing collects, so an " +
			"address set would feed nothing. It DOES read State, so this is the first line to change if " +
			"the Acolyte ever collects a seal",
	},
	"scenario.PreflightAssert": {
		reason: "reads State but goes to EvalAsserts, which evaluates predicates and emits no params: " +
			"Sealed is nil and there is no cell to mask",
	},
	"pushorch.executeAsync": {
		reason: "a push run is not tied to an incarnation — State is nil, so `incarnation.state.<x>` is " +
			"a no-such-key and there is no manifest to ask either",
	},
	"trial.renderCase": {
		reason: "State comes from the case's own fixtures and Sealed is nil: a trial's output is the " +
			"operator's dry-run, not a durable run-plan row",
	},
}

// TestSecretStateFields_EveryRenderInputBuilderDeclaresItsStance walks every
// non-test source file in the keeper module and checks each RenderInput literal
// against the table.
//
// Wider than one package on purpose: the builders are spread across scenario,
// pushorch, trial and this package, and a guard covering only its own package
// would have watched exactly the literal that was already right. The whole module
// rather than internal/ alone, because cmd/ imports this package too.
//
// ⚠ WHAT IT DOES NOT SEE, stated rather than left to be discovered — this is a
// tripwire on the shape the code is written in today, not a proof:
//
//   - a builder that is not a composite literal. `var in RenderInput` plus field
//     assignments is invisible, and so is `in := parentIn` plus assignments — the
//     copy-then-mutate shape [Pipeline.renderApplyDestiny] warns about by name. The
//     copy inherits whatever the parent had and is the safe direction; the
//     zero-value one is not;
//   - a literal outside a function body (a package-level `var`): the scan runs over
//     `fn.Body`;
//   - `_test.go` files, which is where the `//go:build integration` lane lives;
//   - two same-named methods on different types in one package, which collapse into
//     one row — the key is `<pkg>.<func>` and drops the receiver.
//
// An UNKEYED literal is caught below rather than silently read as an unset field.
func TestSecretStateFields_EveryRenderInputBuilderDeclaresItsStance(t *testing.T) {
	fset := token.NewFileSet()
	seen := map[string]bool{}
	literals := 0

	root := filepath.Join("..", "..") // the keeper module
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		pkgDir := filepath.Base(filepath.Dir(path))
		ast.Inspect(file, func(n ast.Node) bool {
			fn, isFn := n.(*ast.FuncDecl)
			if !isFn || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				lit, isLit := inner.(*ast.CompositeLit)
				if !isLit || !isRenderInputLit(lit.Type) {
					return true
				}
				literals++
				key := pkgDir + "." + fn.Name.Name
				// An unkeyed literal reads as "sets no field" for every field,
				// which would pass silently for the rows whose stance is empty.
				if unkeyedLiteral(lit) {
					t.Errorf("%s: %s builds a RenderInput POSITIONALLY. This guard reads fields by name,\n"+
						"so a positional literal answers \"unset\" for every one of them — write it keyed.",
						fset.Position(lit.Pos()), key)
					return true
				}
				want, known := secretStateFieldsStance[key]
				if !known {
					t.Errorf("%s: %s builds a render.RenderInput and is not in secretStateFieldsStance.\n"+
						"Decide whether this caller must seal `${ incarnation.state.<field> }`: it must when it\n"+
						"collects a seal (Sealed non-nil) and reads State, and then the value comes from the\n"+
						"service manifest (incarnation.StateSchemaSecretFields). Then list it here WITH THE\n"+
						"REASON — leaving it out is the NIM-826 failure: the render succeeds and the seal is\n"+
						"quietly empty.", fset.Position(lit.Pos()), key)
					return true
				}
				seen[key] = true
				got := litFieldValue(fset, lit, "SecretStateFields")
				if got != want.want {
					t.Errorf("%s: %s sets SecretStateFields=%q, the table says %q (%s)",
						fset.Position(lit.Pos()), key, got, want.want, want.reason)
				}
				// ★ The premise under every empty stance, enforced rather than
				// trusted. Each of those rows is justified by "this builder collects
				// no seal", and a table is only worth what its reasons are worth: the
				// day one of them starts collecting, nothing else would say so and
				// the guard would stay green over the exact hole it was written for.
				if got == "" && litFieldValue(fset, lit, "Sealed") != "" {
					t.Errorf("%s: %s now collects a seal (it sets Sealed) and supplies no SecretStateFields.\n"+
						"Its row says %q — that reason has just expired. A cell reading\n"+
						"`${ incarnation.state.<field> }` here is unsealed: wire the manifest's answer in\n"+
						"(incarnation.StateSchemaSecretFields) and move the row's stance.",
						fset.Position(lit.Pos()), key, want.reason)
				}
				return true
			})
			return false
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk keeper/internal: %v", err)
	}

	if literals == 0 {
		t.Fatal("found no render.RenderInput literals at all — the walk is broken, and a broken walk gates nothing")
	}
	for _, key := range sortedKeys(secretStateFieldsStance) {
		if !seen[key] {
			t.Errorf("secretStateFieldsStance lists %q, which builds no RenderInput any more — stale entry; "+
				"a stale allowlist hides the next omission", key)
		}
	}
}

// unkeyedLiteral reports a composite literal written positionally. An empty
// literal (`RenderInput{}`) is keyed-vacuously and fine — it sets nothing and the
// table can say so.
func unkeyedLiteral(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		if _, keyed := el.(*ast.KeyValueExpr); !keyed {
			return true
		}
	}
	return false
}

// isRenderInputLit matches both spellings: `RenderInput{…}` inside this package
// and `render.RenderInput{…}` outside it.
func isRenderInputLit(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "RenderInput"
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && pkg.Name == "render" && t.Sel.Name == "RenderInput"
	}
	return false
}
