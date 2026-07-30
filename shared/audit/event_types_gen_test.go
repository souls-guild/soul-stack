package audit

// Generator + drift-guard for the machine-readable event-type catalog.
//
// WHY A CATALOG AT ALL. [EventType] constants are the authoritative list of what
// keeper can write into `audit_log`, but Go cannot enumerate the constants of a
// type at runtime, so nothing downstream could ever ask "what is the full set?".
// The Operator API therefore published `AuditEvent.type` as a bare string, and a
// client rendering a human label per type had no list to check its coverage
// against: a new event type shipped unlabelled and only a user noticed (NIM-337),
// while labels for types that no longer exist sat there just as invisibly.
// [allEventTypes] is that list, and keeper/internal/api turns it into the OpenAPI
// enum of `AuditEvent.type` (NIM-346) so the contract carries it.
//
// WHY GENERATED, NOT WRITTEN. A hand-kept second copy of 138 names is the drift
// class this ticket exists to remove — it would go stale exactly the way the
// locale files did, and just as quietly. So the list is derived from the
// declarations themselves and the derivation is a gate, mirroring the openapi
// snapshot (make gen-openapi / TestCommittedOpenAPI_NoDrift): the same test both
// writes and verifies, so there is no way to regenerate without also proving the
// committed file is what the generator produces.
//
//   - make gen-audit-catalog (GEN_AUDIT_CATALOG=1): overwrite event_types_gen.go.
//   - make test (default): compare; a mismatch = "changed the constants, forgot to
//     regenerate". The failure names the fix.
//
// A constant added to or removed from event_types.go and not regenerated goes red
// here; regenerated but not carried into the spec goes red in
// TestCommittedOpenAPI_NoDrift. `make gen-openapi` runs both writers in order, so one
// command clears both.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"testing"
)

const (
	// eventTypesSource — the declaration file the catalog is derived from.
	eventTypesSource = "event_types.go"

	// eventTypesGenerated — the derived file, committed and compared here.
	eventTypesGenerated = "event_types_gen.go"

	// eventTypesTypeName — only constants of this type belong in the catalog.
	eventTypesTypeName = "EventType"
)

// eventTypeDecl — one constant as declared: Go identifier + wire value.
type eventTypeDecl struct {
	name  string
	value string
}

// parseEventTypeDecls reads the declarations out of event_types.go.
//
// It is deliberately strict about the const forms it accepts. A constant whose
// type is not spelled out (`EventFoo = "foo.bar"`) is untyped and would be
// silently skipped — which is the exact failure mode the catalog exists to
// prevent, so it is an error instead. Same for a value that is not a plain
// string literal: the generator must not have to evaluate expressions to know
// what goes on the wire.
func parseEventTypeDecls(path string) ([]eventTypeDecl, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var decls []eventTypeDecl
	for _, d := range file.Decls {
		gen, ok := d.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, s := range gen.Specs {
			spec, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := spec.Type.(*ast.Ident)
			if !ok || ident.Name != eventTypesTypeName {
				return nil, fmt.Errorf("%s:%d: const %s is not declared as `%s` - every constant in this file must be typed, an untyped one would be dropped from the catalog silently",
					path, fset.Position(s.Pos()).Line, specNames(spec), eventTypesTypeName)
			}
			if len(spec.Names) != len(spec.Values) {
				return nil, fmt.Errorf("%s:%d: const %s has no explicit value - the catalog is built from literals, not from inherited expressions",
					path, fset.Position(s.Pos()).Line, specNames(spec))
			}
			for i, name := range spec.Names {
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return nil, fmt.Errorf("%s:%d: const %s is not a plain string literal - the generator must not have to evaluate expressions to know the wire value",
						path, fset.Position(spec.Values[i].Pos()).Line, name.Name)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					return nil, fmt.Errorf("%s:%d: const %s: unquoting %s: %w", path, fset.Position(lit.Pos()).Line, name.Name, lit.Value, err)
				}
				decls = append(decls, eventTypeDecl{name: name.Name, value: value})
			}
		}
	}
	if len(decls) == 0 {
		return nil, fmt.Errorf("%s declares no %s constant - the parser matched nothing, which is a generator bug rather than an empty catalog", path, eventTypesTypeName)
	}

	sort.Slice(decls, func(i, j int) bool { return decls[i].value < decls[j].value })
	for i := 1; i < len(decls); i++ {
		if decls[i].value == decls[i-1].value {
			return nil, fmt.Errorf("duplicate wire value %q declared by both %s and %s - two constants for one event type make the catalog ambiguous",
				decls[i].value, decls[i-1].name, decls[i].name)
		}
	}
	return decls, nil
}

// specNames renders a ValueSpec's identifiers for an error message.
func specNames(spec *ast.ValueSpec) string {
	names := make([]string, 0, len(spec.Names))
	for _, n := range spec.Names {
		names = append(names, n.Name)
	}
	return fmt.Sprint(names)
}

// renderEventTypesCatalog formats the generated source for the given declarations.
//
// Ordered by wire value (not by declaration order): the OpenAPI enum built from it is
// a committed artifact, so its order has to be a property of the set and not of where
// somebody happened to paste a constant.
//
// Entries are string LITERALS with the constant's name as a comment, not references to
// the constants. Referencing them would tie the catalog to the identifiers at compile
// time, which sounds stricter but deadlocks on removal: deleting a constant would leave
// this file referring to a name that no longer exists, and the generator that fixes it
// is a test in the same package, so it could not run until somebody hand-edited the
// file it is supposed to own. Literals keep the package compiling, so removal is caught
// by a red test with a one-command fix, exactly like addition. Nothing is lost — the
// wire value is what the contract publishes, and the drift guard compares the whole
// file, so a hand-inserted value cannot survive either.
func renderEventTypesCatalog(decls []eventTypeDecl) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by `make gen-audit-catalog`. DO NOT EDIT.\n\n")
	buf.WriteString("package audit\n\n")
	buf.WriteString("// allEventTypes — every " + eventTypesTypeName + " constant declared in " + eventTypesSource + ",\n")
	buf.WriteString("// ordered by wire value. Read through [AllEventTypes].\n")
	buf.WriteString("var allEventTypes = []" + eventTypesTypeName + "{\n")
	for _, d := range decls {
		buf.WriteString("\t" + strconv.Quote(d.value) + ", // " + d.name + "\n")
	}
	buf.WriteString("}\n")

	src, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("gofmt on the generated catalog: %w", err)
	}
	return src, nil
}

// TestGeneratedEventTypes_NoDrift — generator (GEN_AUDIT_CATALOG=1) and drift-guard
// (default). See the file header for why the catalog is derived rather than kept.
func TestGeneratedEventTypes_NoDrift(t *testing.T) {
	decls, err := parseEventTypeDecls(eventTypesSource)
	if err != nil {
		t.Fatalf("reading the event-type declarations: %v", err)
	}
	want, err := renderEventTypesCatalog(decls)
	if err != nil {
		t.Fatalf("rendering the catalog: %v", err)
	}

	if os.Getenv("GEN_AUDIT_CATALOG") != "" {
		if err := os.WriteFile(eventTypesGenerated, want, 0o644); err != nil {
			t.Fatalf("writing %s: %v", eventTypesGenerated, err)
		}
		t.Logf("gen-audit-catalog: wrote %d event types -> %s", len(decls), eventTypesGenerated)
		return
	}

	got, err := os.ReadFile(eventTypesGenerated)
	if err != nil {
		t.Fatalf("reading %s: %v - run `make gen-audit-catalog`", eventTypesGenerated, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is out of date with the %s constants in %s (%d declared): "+
			"run `make gen-audit-catalog` and commit the result. The catalog feeds the OpenAPI enum of "+
			"AuditEvent.type, so a stale one means the contract does not list an event the API can emit.",
			eventTypesGenerated, eventTypesTypeName, eventTypesSource, len(decls))
	}
}

// TestAllEventTypes_MatchesDeclarations — the accessor hands back the whole catalog,
// unsorted-by-accident and unmodifiable by a caller. [AllEventTypes] is what the
// contract enum is built from; a copy that leaks the backing array would let one
// consumer reorder the published enum for every other.
func TestAllEventTypes_MatchesDeclarations(t *testing.T) {
	decls, err := parseEventTypeDecls(eventTypesSource)
	if err != nil {
		t.Fatalf("reading the event-type declarations: %v", err)
	}

	got := AllEventTypes()
	if len(got) != len(decls) {
		t.Fatalf("AllEventTypes returned %d types, %s declares %d", len(got), eventTypesSource, len(decls))
	}
	for i, d := range decls {
		if string(got[i]) != d.value {
			t.Errorf("AllEventTypes()[%d] = %q, declarations have %q (%s) - the catalog must stay ordered by wire value", i, got[i], d.value, d.name)
		}
	}

	first := AllEventTypes()
	first[0] = "mutated.by.caller"
	if AllEventTypes()[0] == "mutated.by.caller" {
		t.Error("AllEventTypes hands out the backing array: a caller mutating the result would change the published contract enum for everyone")
	}
}
