package rbac

// Guard on the cost of a glob scope decision (NIM-845).
//
// INVARIANT:
//
//	A `host matches` / `incarnation matches` predicate compiles its pattern ONCE,
//	when the scope is parsed — never while a decision is being taken.
//
// The distinction matters because the decision is not taken once per request.
// `soulpurview.InScope` is asked per resolved host (voyage_resolver.go,
// incarnation_members.go), and the resolved set is bounded only by
// `voyage.max_scope` — so a role with two `host matches` predicates against a
// 5000-host command Voyage used to perform 10 000 regexp compiles inside one
// `POST /v1/voyages`, on the two paths already singled out as resolver-heavy and
// given their own Tempo buckets.
//
// The guard counts compiles rather than allocations or time: an allocation
// threshold is a proxy that moves with the regexp package's internals, and "the
// count did not change over 5000 elements" is the actual claim. [globCompiles]
// exists for this.
//
// Single-threaded by construction — the counter is process-wide, so a parallel
// test in this package would make it meaningless. These cases do not call
// t.Parallel() for that reason.

import (
	"fmt"
	"testing"
	"time"
)

// TestGlobScope_CompilesAtParseTimeOnly — the case this file exists for. Put the
// compile back on the match path and the count goes up by one per host.
func TestGlobScope_CompilesAtParseTimeOnly(t *testing.T) {
	before := globCompiles.Load()
	expr, err := ParseScopeExpr("host matches redis-* OR host matches cache-*")
	if err != nil {
		t.Fatalf("ParseScopeExpr: %v", err)
	}
	atParse := globCompiles.Load() - before
	if atParse != 2 {
		t.Fatalf("parsing two glob predicates compiled %d patterns, want exactly 2 — one per glob, at parse time", atParse)
	}

	// 5000 hosts, the order of a max_scope-bounded command Voyage.
	hosts := make([]string, 5000)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("redis-%04d.example.com", i)
	}

	before = globCompiles.Load()
	matched := 0
	for _, h := range hosts {
		if evalScope(expr, ScopeInput{Hosts: []string{h}}) {
			matched++
		}
	}
	if got := globCompiles.Load() - before; got != 0 {
		t.Errorf("deciding visibility for %d hosts compiled %d patterns, want 0 — the per-element cost of a scope decision is a regexp compile again",
			len(hosts), got)
	}

	// The decision still has to be the right one: a guard that only counted
	// compiles would pass on a predicate that matched nothing.
	if matched != len(hosts) {
		t.Errorf("matched %d of %d hosts against `host matches redis-*`, want all", matched, len(hosts))
	}
}

// TestGlobScope_IncarnationDimensionCompilesNothingEither — `matches` is valid on
// two dimensions (scope_ast.go parseCondition), and they are separate arms of
// evalCond. A guard that drove only `host matches` would leave the incarnation arm
// free to compile per element, which is the same defect on the path
// `soulpurview`-style per-incarnation visibility takes.
func TestGlobScope_IncarnationDimensionCompilesNothingEither(t *testing.T) {
	expr, err := ParseScopeExpr("incarnation matches redis-*")
	if err != nil {
		t.Fatalf("ParseScopeExpr: %v", err)
	}
	names := make([]string, 2000)
	for i := range names {
		names[i] = fmt.Sprintf("redis-%04d", i)
	}

	before := globCompiles.Load()
	matched := 0
	for _, n := range names {
		if evalScope(expr, ScopeInput{Incarnations: []string{n}}) {
			matched++
		}
	}
	if got := globCompiles.Load() - before; got != 0 {
		t.Errorf("deciding visibility for %d incarnations compiled %d patterns, want 0", len(names), got)
	}
	if matched != len(names) {
		t.Errorf("matched %d of %d incarnations, want all", matched, len(names))
	}
}

// TestGlobScope_EnforcerCheckCompilesNothing — the same claim through the surface
// an operator reaches: building an Enforcer from a snapshot compiles each glob
// once, and every Check afterwards compiles none.
func TestGlobScope_EnforcerCheckCompilesNothing(t *testing.T) {
	before := globCompiles.Load()
	enf, err := NewEnforcerFromSnapshot(&Snapshot{
		Roles:      map[string][]string{"fleet": {"soul.list on host matches redis-*"}},
		Membership: map[string][]string{"archon-op": {"fleet"}},
		Revoked:    map[string]time.Time{},
	})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if atBuild := globCompiles.Load() - before; atBuild == 0 {
		t.Fatal("building the enforcer compiled nothing — the glob is being compiled somewhere later, which is the defect")
	}

	before = globCompiles.Load()
	for i := 0; i < 2000; i++ {
		sid := fmt.Sprintf("redis-%04d.example.com", i)
		if err := enf.Check("archon-op", "soul", "list", map[string]string{"host": sid}); err != nil {
			t.Fatalf("Check(%s) = %v, want allow", sid, err)
		}
	}
	if got := globCompiles.Load() - before; got != 0 {
		t.Errorf("2000 Checks compiled %d patterns, want 0", got)
	}
}

// TestGlobCond_WithoutCompiledPatternFailsClosed pins the direction the missing
// pattern breaks in. A [ScopeCond] can only acquire its pattern through
// [newGlobCond]; one built without it must match NOTHING, because the other
// outcome is a grant nobody wrote.
func TestGlobCond_WithoutCompiledPatternFailsClosed(t *testing.T) {
	bare := &ScopeCond{Dim: dimHost, Match: MatchGlob, Values: []string{"*"}}
	if bare.anyGlobMatch([]string{"redis-01"}) {
		t.Error("a glob condition with no compiled pattern matched a host — `*` without a compile must deny, not admit everything")
	}
	if bare.matchGlob("redis-01") {
		t.Error("matchGlob admitted a host on a condition with no compiled pattern")
	}
	if evalCond(bare, ScopeInput{Hosts: []string{"redis-01"}}) {
		t.Error("evalCond admitted a host on a condition with no compiled pattern")
	}
}

// TestGlobScope_ParserAlwaysCompiles — the reverse direction of the fail-closed
// rule: every glob predicate the parser produces carries its pattern, so the
// fail-closed branch above is unreachable from parsed input. Without this, the
// two together would be satisfied by a parser that compiled nothing and denied
// everything.
func TestGlobScope_ParserAlwaysCompiles(t *testing.T) {
	for _, s := range []string{
		"host matches redis-*",
		"incarnation matches redis-*",
		`host matches "redis-*"`,
		"coven=prod AND (host matches redis-* OR host matches cache-?)",
	} {
		expr, err := ParseScopeExpr(s)
		if err != nil {
			t.Fatalf("ParseScopeExpr(%q): %v", s, err)
		}
		forEachGlobCond(expr, func(c *ScopeCond) {
			if c.glob == nil {
				t.Errorf("%q: glob predicate %q came out of the parser with no compiled pattern — it would deny every host", s, c.Values[0])
			}
		})
	}
}

func forEachGlobCond(e *ScopeExpr, fn func(*ScopeCond)) {
	if e == nil {
		return
	}
	if e.Op == OpLeaf {
		if e.Cond != nil && e.Cond.Match == MatchGlob {
			fn(e.Cond)
		}
		return
	}
	for _, c := range e.Children {
		forEachGlobCond(c, fn)
	}
}
