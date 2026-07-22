package cel

import (
	"errors"
	"reflect"
	"testing"
)

func newVarRefsEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestVarRefs_SelectForm — `${vars.a}-${vars.b}` → [a b] (case #10): extracts names via
// the select form, in order of appearance during PostOrderVisit.
func TestVarRefs_SelectForm(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${vars.a}-${vars.b}")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("VarRefs = %v, want [a b]", got)
	}
}

// TestVarRefs_LiteralOutsideMarker — `text vars.x` OUTSIDE `${ … }` is not counted as a
// reference (case #10): it is literal text, not a CEL block → empty slice.
func TestVarRefs_LiteralOutsideMarker(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("text vars.x")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("VarRefs = %v, want [] (vars.x outside ${} - literal)", got)
	}
}

// TestVarRefs_StringLiteralInsideExpr — `${ "vars.x" }` (a CEL string literal) is not a
// reference: it is a StringConstant, not a Select node → empty slice.
func TestVarRefs_StringLiteralInsideExpr(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs(`${ "vars.x" }`)
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("VarRefs = %v, want [] (string literal is not a Select)", got)
	}
}

// TestVarRefs_OtherBaseIgnored — references to input/soulprint/register are NOT counted
// (only base=="vars"); a mixed string yields only vars names.
func TestVarRefs_OtherBaseIgnored(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${ input.x }/${ vars.a }/${ soulprint.self.sid }")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("VarRefs = %v, want [a] (vars.* only)", got)
	}
}

// TestVarRefs_Dedup — a repeated reference to the same var is deduplicated.
func TestVarRefs_Dedup(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${ vars.a }-${ vars.a }")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("VarRefs = %v, want [a] (dedup)", got)
	}
}

// TestVarRefs_IndexForm — the index form `vars['k']` → deterministic ErrVarIndexForm
// (case #10, the fixed behavior of the index form).
func TestVarRefs_IndexForm(t *testing.T) {
	e := newVarRefsEngine(t)
	_, err := e.VarRefs(`${ vars['k'] }`)
	if err == nil || !errors.Is(err, ErrVarIndexForm) {
		t.Fatalf("VarRefs index form: expected ErrVarIndexForm, got: %v", err)
	}
}

// TestVarRefs_NestedSelect — a nested select `vars.a.b` extracts the root var key `a`
// (PostOrderVisit visits Select `vars.a` as its own level).
func TestVarRefs_NestedSelect(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${ vars.cfg.path }")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"cfg"}) {
		t.Errorf("VarRefs = %v, want [cfg] (root var key of a nested select)", got)
	}
}

// TestVarRefs_MacroBlockNoVars — a block with a valid macro expression WITHOUT vars refs
// (`[1,2].exists(x, x > 0)`) next to a vars block: VarRefs walks its AST, finds no
// base=="vars", and collects the reference only from the second block, without error.
// Covers the branch where parseNoMacro succeeds but PostOrderVisit yields no vars names.
func TestVarRefs_MacroBlockNoVars(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${ [1,2].exists(x, x > 0) }-${ vars.a }")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("VarRefs = %v, want [a] (macro block without vars skipped, vars.a collected)", got)
	}
}

// TestVarRefs_BrokenBlockRejectedEarly pins the actual boundary: a block with
// syntactically-broken CEL never reaches per-block parseNoMacro — scanInterpolation
// (parseBlock) itself gates the block via env.Parse and fails to find the closing `}`
// for the invalid expression, returning *ErrCompile. So the `continue` on perr in
// varrefs.go (mirror of DetectSealed) is unreachable for entry via VarRefs: VarRefs does
// not "silently skip" a broken block but returns a parse-phase error. This test guards
// that behavior against regression (if parseBlock is relaxed, the mask changes — the test fails).
func TestVarRefs_BrokenBlockRejectedEarly(t *testing.T) {
	e := newVarRefsEngine(t)
	_, err := e.VarRefs("${ vars.a }-${ vars.b + }")
	if err == nil {
		t.Fatal("VarRefs: expected a parse-phase error on the broken block, got nil")
	}
	var compileErr *ErrCompile
	if !errors.As(err, &compileErr) {
		t.Errorf("VarRefs: expected *ErrCompile, got %T: %v", err, err)
	}
}
