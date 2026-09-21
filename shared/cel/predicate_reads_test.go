package cel

import (
	"sort"
	"strings"
	"testing"
)

// predicateRoots — the three roots keeper narrows flow_context by; using the same
// set here keeps the unit test on the shape the one caller actually asks for.
var predicateRoots = []string{"input", "vars", "incarnation"}

func fieldNames(t *testing.T, r RootReads) string {
	t.Helper()
	names := make([]string, 0, len(r.Fields))
	for f := range r.Fields {
		names = append(names, f)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// TestPredicateReads_SelectForm — the ordinary shape: each `<root>.<field>` is
// reported under its own root, and nothing is reported whole.
func TestPredicateReads_SelectForm(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reads := e.PredicateReads("input.action == 'apply' && vars.enabled", predicateRoots)

	if got := fieldNames(t, reads["input"]); got != "action" {
		t.Errorf("input fields = %q, want \"action\"", got)
	}
	if got := fieldNames(t, reads["vars"]); got != "enabled" {
		t.Errorf("vars fields = %q, want \"enabled\"", got)
	}
	for _, root := range predicateRoots {
		if reads[root].Whole {
			t.Errorf("%s reported Whole for a plain select-form predicate", root)
		}
	}
	if got := fieldNames(t, reads["incarnation"]); got != "" {
		t.Errorf("incarnation fields = %q, want none (never named)", got)
	}
}

// TestPredicateReads_NestedSelect — `input.db.port` names the field ONE level
// below the root; the deeper hop is not a root read and must not widen anything.
func TestPredicateReads_NestedSelect(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reads := e.PredicateReads("input.db.port == 6379", predicateRoots)

	if got := fieldNames(t, reads["input"]); got != "db" {
		t.Errorf("input fields = %q, want \"db\"", got)
	}
	if reads["input"].Whole {
		t.Error("a nested select must not read the root whole")
	}
}

// TestPredicateReads_HasMacro — `has(input.x)` names x. This is the case the
// macro-free parse exists for: under the evaluation env the same text is a
// test-only Select carrying no recoverable field, and a caller that dropped x
// would get has(...)==false — a different answer with no error to show for it.
func TestPredicateReads_HasMacro(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reads := e.PredicateReads("has(input.optional_flag)", predicateRoots)

	if got := fieldNames(t, reads["input"]); got != "optional_flag" {
		t.Errorf("input fields = %q, want \"optional_flag\" (macro-free parse)", got)
	}
	if reads["input"].Whole {
		t.Error("has() over a named field is not a whole-root read")
	}
}

// TestPredicateReads_WholeRootShapes — every read whose field name is not in the
// AST reports Whole: the bare identifier and the index form. Fail-closed is the
// point — a narrowing caller must widen here, never silently drop.
func TestPredicateReads_WholeRootShapes(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, expr := range []string{
		"size(input) > 0",
		"input == {}",
		"input['action'] == 'apply'",
		"input[vars.key] == 'x'",
	} {
		reads := e.PredicateReads(expr, predicateRoots)
		if !reads["input"].Whole {
			t.Errorf("%q: input.Whole = false, want true (no field name in the AST)", expr)
		}
	}
}

// TestPredicateReads_WholeAndNamed — a predicate doing both keeps Whole: the
// named half must not mask the unnamed one.
func TestPredicateReads_WholeAndNamed(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reads := e.PredicateReads("input.action == 'apply' && size(input) > 1", predicateRoots)
	if !reads["input"].Whole {
		t.Error("input.Whole = false, want true - the bare read is still there")
	}
}

// TestPredicateReads_Unparseable — broken CEL reports every root whole. It fails
// at eval on one side of the wire or the other regardless; calling it "reads
// nothing" would turn that failure into a quietly different result.
func TestPredicateReads_Unparseable(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reads := e.PredicateReads("input.action ==", predicateRoots)
	for _, root := range predicateRoots {
		if !reads[root].Whole {
			t.Errorf("%s.Whole = false on unparseable text, want true (fail-closed)", root)
		}
	}
}

// TestPredicateReads_Empty — no predicate reads nothing, and is NOT whole: that
// is what lets a caller ship an empty section for a task with no flow control.
func TestPredicateReads_Empty(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reads := e.PredicateReads("", predicateRoots)
	for _, root := range predicateRoots {
		if reads[root].Whole || len(reads[root].Fields) != 0 {
			t.Errorf("%s = %+v on an empty predicate, want no fields and not whole", root, reads[root])
		}
	}
}

// TestPredicateReads_StringConstantIsNotARead — `input` inside a CEL string
// literal is text, not a reference (the AST walk is what buys this over a regex).
func TestPredicateReads_StringConstantIsNotARead(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reads := e.PredicateReads("vars.note == 'input.password'", predicateRoots)
	if got := fieldNames(t, reads["input"]); got != "" {
		t.Errorf("input fields = %q, want none - it is inside a string constant", got)
	}
	if reads["input"].Whole {
		t.Error("input.Whole = true for a mention inside a string constant")
	}
}

// TestPredicateReads_ComprehensionOverField — a macro over a named field stays a
// named read; the iteration variable is not a root.
func TestPredicateReads_ComprehensionOverField(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reads := e.PredicateReads("input.roles.exists(r, r == 'primary')", predicateRoots)
	if got := fieldNames(t, reads["input"]); got != "roles" {
		t.Errorf("input fields = %q, want \"roles\"", got)
	}
	if reads["input"].Whole {
		t.Error("a comprehension over a named field is not a whole-root read")
	}
}
