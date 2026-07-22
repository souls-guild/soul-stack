package statepredicate

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// Slice S1 (filter -> Run + RBAC Purview S2c + Cadence state): unified
// statepredicate.Resolver, a CEL predicate over incarnation.state. TDD-first:
// tests pin the contract BEFORE implementation (red), then go green.
//
// S1 scope is Compile (validation + program cache) + Matches
// (single-incarnation check against state map). ResolveIncarnations (list + SQL
// pushdown) is the next slice (requires incarnation repository + DB access), so
// do not pull it here.

func newResolver(t *testing.T) Resolver {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// --- Matches: equality by string state field ---

func TestMatches_StringEquality(t *testing.T) {
	r := newResolver(t)
	state := map[string]any{"redis_version": "8.0"}

	if ok, err := r.Matches(`state.redis_version == "8.0"`, state); err != nil || !ok {
		t.Errorf(`redis_version=="8.0" on {8.0}: ok=%v err=%v, want true,nil`, ok, err)
	}
	if ok, err := r.Matches(`state.redis_version == "8.1"`, state); err != nil || ok {
		t.Errorf(`redis_version=="8.1" on {8.0}: ok=%v err=%v, want false,nil`, ok, err)
	}
}

// --- numeric: jsonb numbers (float64 after decode) compare correctly ---

func TestMatches_Numeric(t *testing.T) {
	r := newResolver(t)

	// int (as from CEL literal / Go int).
	if ok, err := r.Matches(`state.memory_mb > 1000`, map[string]any{"memory_mb": 2000}); err != nil || !ok {
		t.Errorf("memory_mb>1000 on int(2000): ok=%v err=%v, want true,nil", ok, err)
	}
	// float64 is number shape after json/jsonb decode.
	if ok, err := r.Matches(`state.memory_mb > 1000`, map[string]any{"memory_mb": float64(2000)}); err != nil || !ok {
		t.Errorf("memory_mb>1000 on float64(2000): ok=%v err=%v, want true,nil (jsonb coercion)", ok, err)
	}
	if ok, err := r.Matches(`state.memory_mb > 1000`, map[string]any{"memory_mb": float64(500)}); err != nil || ok {
		t.Errorf("memory_mb>1000 on float64(500): ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- in/list ---

func TestMatches_InList(t *testing.T) {
	r := newResolver(t)
	if ok, err := r.Matches(`state.redis_version in ["8.0","8.1"]`, map[string]any{"redis_version": "8.0"}); err != nil || !ok {
		t.Errorf("in-list on 8.0: ok=%v err=%v, want true,nil", ok, err)
	}
	if ok, err := r.Matches(`state.redis_version in ["8.0","8.1"]`, map[string]any{"redis_version": "7.4"}); err != nil || ok {
		t.Errorf("in-list on 7.4: ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- nested field ---

func TestMatches_Nested(t *testing.T) {
	r := newResolver(t)
	state := map[string]any{"cluster": map[string]any{"replicas": 3}}
	if ok, err := r.Matches(`state.cluster.replicas == 3`, state); err != nil || !ok {
		t.Errorf("nested replicas==3: ok=%v err=%v, want true,nil", ok, err)
	}
}

// --- no-such-key: predicate by missing state field -> (false, nil) ---
//
// Fail-closed semantics, consistent with rbac.EvalSoulprintExpr (S2b) and
// oracle.WhereEvaluator: untrusted/incomplete state snapshot must not break
// resolver; absence of required fact = did not match.
func TestMatches_NoSuchKey(t *testing.T) {
	r := newResolver(t)
	if ok, err := r.Matches(`state.absent == "x"`, map[string]any{"redis_version": "8.0"}); err != nil || ok {
		t.Errorf("missing field: ok=%v err=%v, want false,nil (no-such-key -> no-match)", ok, err)
	}
	if ok, err := r.Matches(`state.absent > 1`, map[string]any{}); err != nil || ok {
		t.Errorf("missing numeric field: ok=%v err=%v, want false,nil", ok, err)
	}
	// nil state -> no-match (not panic).
	if ok, err := r.Matches(`state.redis_version == "8.0"`, nil); err != nil || ok {
		t.Errorf("nil-state: ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- sandbox: predicate with vault()/now()/register/... -> Compile error ---
//
// State predicate is a pure function of state (like migration-CEL ADR-019):
// vault()/now() are cut by guard, other roots by env undeclared-ness.
func TestCompile_SandboxRejected(t *testing.T) {
	r := newResolver(t)
	cases := []string{
		`vault("secret/x") == "y"`,
		`now() > timestamp("2020-01-01T00:00:00Z")`,
		`register.foo == 1`,
		`soulprint.self.os.family == "debian"`,
		`input.bar == 1`,
		`incarnation.name == "x"`,
		`essence.baz == 1`,
	}
	for _, expr := range cases {
		t.Run(expr, func(t *testing.T) {
			if err := r.Compile(expr); err == nil {
				t.Fatalf("Compile(%q): want sandbox/compile error, got nil", expr)
			}
		})
	}
}

// --- broken CEL -> Compile error ---

func TestCompile_BrokenRejected(t *testing.T) {
	r := newResolver(t)
	cases := []string{
		`state.redis_version ==`,    // incomplete expression
		`state.redis_version && `,   // dangling operator
		`(`,                         // unbalanced parenthesis
		`state.redis_version + "x"`, // non-bool result is cut on Matches, but syntax is ok; see separate test
	}
	// Only syntactically broken ones (first three) fail Compile.
	for _, expr := range cases[:3] {
		t.Run(expr, func(t *testing.T) {
			if err := r.Compile(expr); err == nil {
				t.Fatalf("Compile(%q): want compile error, got nil", expr)
			}
		})
	}
}

// Non-bool predicate result -> Matches error (predicate must be boolean), NOT
// fail-closed (false, nil). Distinguish non-bool from runtime no-such-key
// through typed sentinel [cel.ErrPredicateNotBool], not by text.
func TestMatches_NonBoolRejected(t *testing.T) {
	r := newResolver(t)
	ok, err := r.Matches(`state.redis_version`, map[string]any{"redis_version": "8.0"})
	if err == nil {
		t.Fatalf("non-bool predicate: ok=%v err=nil, want error (not fail-closed)", ok)
	}
	if !errors.Is(err, cel.ErrPredicateNotBool) {
		t.Fatalf("non-bool predicate: errors.Is(err, cel.ErrPredicateNotBool)=false, err=%v", err)
	}
}

// --- blank predicate -> Compile error (fail-closed, no accidental match-all) ---
//
// Blank state predicate is ambiguous (match-all is dangerous for filter/RBAC
// selector), so it is rejected explicitly: caller needing "all incarnations"
// simply does not call resolver. Symmetric to rbac rejecting blank soulprint
// selector.
func TestCompile_EmptyRejected(t *testing.T) {
	r := newResolver(t)
	if err := r.Compile(""); err == nil {
		t.Fatal("Compile(\"\"): want error for empty predicate, got nil")
	}
	if err := r.Compile("   "); err == nil {
		t.Fatal("Compile(spaces): want error for blank predicate, got nil")
	}
	if _, err := r.Matches("", map[string]any{}); err == nil {
		t.Fatal("Matches(\"\", …): want error for empty predicate, got nil")
	}
}

// --- cache: repeated compile of one expression reuses program ---
//
// Indirect check: Matches does not fail across repeated calls and gives stable
// result (program is cached in shared/cel.Engine; direct compilation counter is
// not inspected here, that is Engine detail). Guarantee "do not recompile on
// every Matches" is provided by reusing one Engine under sync.Once.
func TestMatches_Repeatable(t *testing.T) {
	r := newResolver(t)
	state := map[string]any{"redis_version": "8.0"}
	for i := 0; i < 100; i++ {
		ok, err := r.Matches(`state.redis_version == "8.0"`, state)
		if err != nil || !ok {
			t.Fatalf("iter %d: ok=%v err=%v, want true,nil", i, ok, err)
		}
	}
}

// Compile of valid expression returns no error (normal caller validation path
// on load: filter/RBAC selector/Cadence target compile predicate in advance).
func TestCompile_ValidOK(t *testing.T) {
	r := newResolver(t)
	if err := r.Compile(`state.redis_version == "8.0" && state.memory_mb > 1000`); err != nil {
		t.Fatalf("Compile(valid): %v", err)
	}
}

// Sandbox Compile error mentions state predicate/sandbox (clear diagnostics for
// operator).
func TestCompile_SandboxErrorMessage(t *testing.T) {
	r := newResolver(t)
	err := r.Compile(`register.foo == 1`)
	if err == nil {
		t.Fatal("want error")
	}
	// Message should distinguish sandbox/compile from blank/other; do not bind
	// to exact cel-go text, only check non-emptiness.
	if strings.TrimSpace(err.Error()) == "" {
		t.Fatal("empty error message")
	}
}
