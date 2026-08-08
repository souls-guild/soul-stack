package mcp

// Code↔doc guard for the MCP error catalog (NIM-386).
//
// INVARIANT:
//
//	Every `mcpCode*` constant declared in errors.go appears in
//	docs/keeper/mcp-tools.md § Errors — except the ones named below as
//	known debt, and those must STILL be missing.
//
// Why this is not covered by [TestReservedMCPCodes_PresentInDocs]. That test
// walks [reservedMCPCodes], which by construction lists only the codes declared
// but NOT yet wired to a sentinel — one entry today. A code that is wired and
// returned to real callers has nothing pointing it at the docs at all, which is
// the wrong way round: an undocumented code an agent can actually receive costs
// more than an undocumented one nobody can. NIM-386 added `teardown-unavailable`
// and found nine already in that state.
//
// The debt list is asserted in BOTH directions on purpose. An allowlist that is
// only ever read as "skip these" rots into a list of things somebody documented
// years ago, and then it is silently gating nothing. Documenting one of these
// nine fails this test until its name is removed from the list — which is the
// cheap, correct edit, and the only way the list can shrink honestly.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// undocumentedMCPCodesDebt — codes declared and wired in this package that
// docs/keeper/mcp-tools.md § Errors does not describe. All nine predate NIM-386,
// which found them while documenting its own code and deliberately did not widen
// its scope to write nine error descriptions it had not researched. Tracked in
// NIM-589; the correct fix is to document them and delete them from here, one at
// a time.
var undocumentedMCPCodesDebt = map[string]string{
	"decree-already-exists":        "decree create, UNIQUE violation",
	"herald-already-exists":        "herald create, UNIQUE violation",
	"not-implemented":              "declared surface with no wiring behind it yet",
	"operator-revoked":             "operator revoke on an already-revoked Archon",
	"push-provider-already-exists": "push-provider create, UNIQUE violation",
	"role-cascade-not-confirmed":   "role delete with children and no explicit cascade",
	"role-has-children":            "role delete blocked by derived roles (ADR-0078)",
	"tiding-already-exists":        "tiding create, UNIQUE violation",
	"vigil-already-exists":         "vigil create, UNIQUE violation",
}

// declaredMCPCodes reads the `mcpCode*` constants out of errors.go rather than
// taking a hand-written list. A hand-written list is exactly what this test is
// here to replace: it would be missing the same names the docs are missing,
// since both are updated by the same person in the same sitting.
func declaredMCPCodes(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "errors.go", nil, 0)
	if err != nil {
		t.Fatalf("parse errors.go: %v", err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "mcpCode") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s = %s: %v", name.Name, lit.Value, err)
				}
				out[val] = name.Name
			}
		}
	}
	if len(out) < 20 {
		t.Fatalf("found only %d mcpCode* constants in errors.go — the parse is not seeing the const block", len(out))
	}
	return out
}

// TestDeclaredMCPCodes_DocumentedOrKnownDebt is the guard proper.
func TestDeclaredMCPCodes_DocumentedOrKnownDebt(t *testing.T) {
	raw, err := os.ReadFile(mcpToolsDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", mcpToolsDocPath, err)
	}
	doc := string(raw)
	// Backtick-wrapped, the form the § Errors table uses — a bare substring
	// would match `not-found` inside `rerun-input-unavailable`-style prose and
	// report coverage that is not there.
	documented := func(code string) bool { return strings.Contains(doc, "`"+code+"`") }

	var missing []string
	for code, constName := range declaredMCPCodes(t) {
		if documented(code) {
			continue
		}
		if _, known := undocumentedMCPCodesDebt[code]; known {
			continue
		}
		missing = append(missing, code+" ("+constName+")")
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("MCP code %s is returned to callers but is not in %s § Errors — an agent receiving it "+
			"has nothing to look it up in, and cannot tell a retryable refusal from a defect", m, mcpToolsDocPath)
	}
}

// TestUndocumentedMCPCodesDebt_StillAccurate keeps the allowlist above from
// outliving the debt it records. Without it, documenting a code leaves a
// permanent hole in the guard for that code's name.
func TestUndocumentedMCPCodesDebt_StillAccurate(t *testing.T) {
	raw, err := os.ReadFile(mcpToolsDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", mcpToolsDocPath, err)
	}
	doc := string(raw)
	declared := declaredMCPCodes(t)

	for code, why := range undocumentedMCPCodesDebt {
		if _, ok := declared[code]; !ok {
			t.Errorf("debt entry %q (%s) is no longer a declared mcpCode* constant — drop it from "+
				"undocumentedMCPCodesDebt", code, why)
			continue
		}
		if strings.Contains(doc, "`"+code+"`") {
			t.Errorf("debt entry %q (%s) IS now documented in %s — remove it from undocumentedMCPCodesDebt "+
				"so the guard covers it", code, why, mcpToolsDocPath)
		}
	}
}
