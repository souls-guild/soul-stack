package soulpurview

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// TestResolve_RegexOnly_NotPartialNotEmpty verifies that regex dimension S3b-2a
// IS computed by pilot: Resolve no longer marks pure regex scope as Partial (as
// in S3b-0) and does not collapse to Empty. Scope.Regexes carries patterns for
// keyset eval. soulprint/state remain Partial: this slice pilot does not compute
// them.
func TestResolve_RegexOnly_NotPartialNotEmpty(t *testing.T) {
	sc := Resolve(rbac.Purview{Regexes: []string{"^web-"}})
	if sc.Empty {
		t.Fatalf("regex-only purview -> Empty=true (fail-closed on available regex dimension)")
	}
	if sc.Partial {
		t.Fatalf("regex-only purview -> Partial=true; want false (S3b-2a computes regex)")
	}
	if !reflect.DeepEqual(sc.Regexes, []string{"^web-"}) {
		t.Fatalf("Regexes = %v, want [^web-]", sc.Regexes)
	}
}

// TestResolve_CovenPlusRegex_NotPartial verifies coven+regex: both dimensions
// are computable by pilot (coven OR regex) -> NOT Partial. Both fields are set.
func TestResolve_CovenPlusRegex_NotPartial(t *testing.T) {
	sc := Resolve(rbac.Purview{Covens: []string{"prod"}, Regexes: []string{"^db-"}})
	if sc.Empty || sc.Partial {
		t.Fatalf("coven+regex -> Empty=%v Partial=%v; want both false", sc.Empty, sc.Partial)
	}
	if !reflect.DeepEqual(sc.Covens, []string{"prod"}) {
		t.Fatalf("Covens = %v, want [prod]", sc.Covens)
	}
	if !reflect.DeepEqual(sc.Regexes, []string{"^db-"}) {
		t.Fatalf("Regexes = %v, want [^db-]", sc.Regexes)
	}
}

// TestResolve_SoulprintStillPartial verifies soulprint/state remain Partial in
// this slice (S3b-2b not implemented yet). Slice boundary: regex removed from
// Partial, soulprint/state not.
func TestResolve_SoulprintStillPartial(t *testing.T) {
	for name, p := range map[string]rbac.Purview{
		"soulprint":       {SoulprintExprs: []string{`soulprint.self.os.family == "debian"`}},
		"state":           {StateExprs: []string{`state.role == "primary"`}},
		"coven+soulprint": {Covens: []string{"prod"}, SoulprintExprs: []string{`x`}},
		"regex+soulprint": {Regexes: []string{"^web-"}, SoulprintExprs: []string{`x`}},
	} {
		sc := Resolve(p)
		if !sc.Partial {
			t.Errorf("%s -> Partial=false; want true (soulprint/state are not computed in S3b-2a)", name)
		}
		if sc.Empty {
			t.Errorf("%s -> Empty=true (access exists)", name)
		}
	}
}

// TestResolve_Keyset_RegexPresent verifies mode flag: scope with regex requires
// keyset mode (handler selects path by it). coven-only / unrestricted do not.
func TestResolve_Keyset_RegexPresent(t *testing.T) {
	if !Resolve(rbac.Purview{Regexes: []string{"^web-"}}).NeedsKeyset() {
		t.Error("regex-scope → NeedsKeyset()=false; want true")
	}
	if !Resolve(rbac.Purview{Covens: []string{"prod"}, Regexes: []string{"^db-"}}).NeedsKeyset() {
		t.Error("coven+regex-scope → NeedsKeyset()=false; want true")
	}
	if Resolve(rbac.Purview{Covens: []string{"prod"}}).NeedsKeyset() {
		t.Error("coven-only → NeedsKeyset()=true; want false (offset fast-path)")
	}
	if Resolve(rbac.Purview{Unrestricted: true}).NeedsKeyset() {
		t.Error("unrestricted → NeedsKeyset()=true; want false")
	}
}

// TestCompileScope_BadRegex_FailClosed verifies broken pattern in Purview ->
// compilation error (handler hides/empty, NOT 500). NOT silent ignore
// (otherwise operator would see more/less than allowed).
func TestCompileScope_BadRegex_FailClosed(t *testing.T) {
	sc := Scope{Regexes: []string{"^web-", "([unclosed"}}
	if _, err := CompileScope(sc); err == nil {
		t.Fatal("CompileScope with broken pattern -> nil err; want error (fail-closed)")
	}
}

// TestCompileScope_TooLong_FailClosed verifies pattern over length limit (ReDoS
// guard: RE2 is time-safe, but pathologically long pattern is rejected) -> error.
func TestCompileScope_TooLong_FailClosed(t *testing.T) {
	long := make([]byte, MaxRegexLen+1)
	for i := range long {
		long[i] = 'a'
	}
	sc := Scope{Regexes: []string{string(long)}}
	if _, err := CompileScope(sc); err == nil {
		t.Fatalf("CompileScope with pattern length %d (>%d) -> nil err; want error", len(long), MaxRegexLen)
	}
}

// TestVisible_OR_Union is the MAIN union invariant: visibility = covenMatch OR
// regexMatch. Host matching ONLY ONE dimension is visible. This is OR, not AND.
func TestVisible_OR_Union(t *testing.T) {
	sc := Scope{Covens: []string{"prod"}, Regexes: []string{"^db-"}}
	cs, err := CompileScope(sc)
	if err != nil {
		t.Fatalf("CompileScope: %v", err)
	}

	// Host in prod but NOT db-* -> visible by coven dimension.
	if !cs.Visible("web-01.example.com", []string{"prod"}) {
		t.Error("host [prod]/web-* -> invisible; want visible (coven dimension)")
	}
	// Host db-* but NOT in prod -> visible by regex dimension.
	if !cs.Visible("db-07.example.com", []string{"staging"}) {
		t.Error("host [staging]/db-* -> invisible; want visible (regex dimension)")
	}
	// Host in both -> visible.
	if !cs.Visible("db-09.example.com", []string{"prod"}) {
		t.Error("host [prod]/db-* -> invisible; want visible (both dimensions)")
	}
}

// TestVisible_UnionSubset_Negative verifies union subset of Purview: host
// matching NEITHER coven NOR regex is hidden. Regression = over-show beyond
// Purview boundary.
func TestVisible_UnionSubset_Negative(t *testing.T) {
	sc := Scope{Covens: []string{"prod"}, Regexes: []string{"^db-"}}
	cs, _ := CompileScope(sc)
	if cs.Visible("web-01.example.com", []string{"staging"}) {
		t.Error("host [staging]/web-* (neither coven nor regex) -> visible; want hidden (union subset of Purview)")
	}
	if cs.Visible("app-01.example.com", nil) {
		t.Error("host without covens / non-db -> visible; want hidden")
	}
}

// TestVisible_Unrestricted verifies unrestricted scope sees any host.
func TestVisible_Unrestricted(t *testing.T) {
	cs, _ := CompileScope(Scope{Unrestricted: true})
	if !cs.Visible("anything.example.com", nil) {
		t.Error("unrestricted -> host invisible")
	}
}

// TestVisible_Empty_FailClosed verifies Empty scope allows no host.
func TestVisible_Empty_FailClosed(t *testing.T) {
	cs, _ := CompileScope(Scope{Empty: true})
	if cs.Visible("prod-01.example.com", []string{"prod"}) {
		t.Error("Empty scope -> host visible (fail-OPEN!); want hidden")
	}
}

// TestInScope_RegexOnly_NowVisible verifies list/get CONSISTENCY (gate fix):
// regex-only operator sees host in List (keyset eval [CompiledScope.Visible]),
// and now InScope gives THE SAME result: regex-matching sid is visible by
// GET /{sid} (200, not 404). Regression of this test = list/get divergence
// returned (host in list, but 404 by direct SID). Flipped from old CurrentlyFalse
// (S3b-2a coven-only).
func TestInScope_RegexOnly_NowVisible(t *testing.T) {
	sc := Resolve(rbac.Purview{Regexes: []string{"^web-"}})
	// Regex-matching host is visible regardless of covens (regex dimension).
	if !InScope(sc, "web-01.example.com", []string{"any-coven"}) {
		t.Fatal("regex-only scope, sid=web-01 (matches ^web-) -> InScope=false; want true (list/get consistent)")
	}
	if !InScope(sc, "web-02.example.com", nil) {
		t.Fatal("regex-only scope, sid=web-02 without covens -> InScope=false; want true (regex dimension)")
	}
	// Non-matching regex host is hidden (union subset of Purview, not over-show).
	if InScope(sc, "db-01.example.com", []string{"any-coven"}) {
		t.Fatal("regex-only scope, sid=db-01 (does NOT match ^web-) -> InScope=true; want false (outside Purview)")
	}
}

// TestInScope_CovenRegexUnion verifies single-read OR union (same predicate as
// [CompiledScope.Visible]): host visible by coven OR regex, hidden if neither.
// Symmetric to List union (see TestVisible_OR_Union).
func TestInScope_CovenRegexUnion(t *testing.T) {
	sc := Scope{Covens: []string{"prod"}, Regexes: []string{"^db-"}}
	// in prod, not db-* -> visible by coven.
	if !InScope(sc, "web-01.example.com", []string{"prod"}) {
		t.Error("[prod]/web-* -> InScope=false; want true (coven dimension)")
	}
	// db-*, not in prod -> visible by regex.
	if !InScope(sc, "db-07.example.com", []string{"staging"}) {
		t.Error("[staging]/db-* -> InScope=false; want true (regex dimension)")
	}
	// neither prod nor db-* -> hidden.
	if InScope(sc, "web-01.example.com", []string{"staging"}) {
		t.Error("[staging]/web-* (neither coven nor regex) -> InScope=true; want false")
	}
}

// TestInScope_BadRegex_FailClosed verifies eval-error single-read: broken regex
// in Purview hides host (false), does not reveal existence and does not panic.
// Fail-closed, symmetric to listKeyset CompileScope-error branch.
func TestInScope_BadRegex_FailClosed(t *testing.T) {
	sc := Scope{Regexes: []string{"(unclosed"}}
	if InScope(sc, "web-01.example.com", []string{"prod"}) {
		t.Fatal("broken regex in scope -> InScope=true (fail-OPEN!); want false (eval-error hides)")
	}
}

// TestVisible_RegexAnchoring verifies RE2 without auto anchor: `^web-` matches
// prefix but NOT substring in the middle. Documents semantics for operator.
func TestVisible_RegexAnchoring(t *testing.T) {
	cs, _ := CompileScope(Scope{Regexes: []string{"^web-"}})
	if !cs.Visible("web-01.example.com", nil) {
		t.Error("^web- did not match web-01")
	}
	if cs.Visible("api-web-01.example.com", nil) {
		t.Error("^web- matched api-web-01 (pattern is anchored to start, not substring)")
	}
}
