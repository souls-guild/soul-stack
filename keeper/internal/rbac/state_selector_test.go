package rbac

import (
	"strings"
	"testing"
)

// ADR-047 S2c — the state selector key: a CEL predicate over
// incarnation.state. TDD-first: tests fix the contract BEFORE the
// implementation (red), then go green.
//
// Parallels regex (S2a) / soulprint (S2b), but validation/eval is delegated
// to keeper/internal/statepredicate (Compile/Matches already exist, sandbox
// migration-CEL root `state`) — RBAC does NOT duplicate the state-predicate
// CEL engine.
//
// S2c scope (like soulprint in S2b): state is added to the selector grammar
// + Purview.StateExprs + the least-privilege subset check (string-equality,
// fail-closed). Matches is active once incarnation.state is present in
// context (S3b will supply it); until then, context (map[string]string)
// carries no nested state — fail-closed deny.

// --- Parsing a quoted state value ---

// state='state.redis_version == "8.0"' parses into Selector{state:[…]}
// (the CEL predicate without its outer quotes).
func TestParseSelector_State_Simple(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on state='state.redis_version == "8.0"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["state"]
	want := `state.redis_version == "8.0"`
	if len(got) != 1 || got[0] != want {
		t.Errorf("Selector[state] = %v, want [%q]", got, want)
	}
}

// Inner double quotes and spaces in the CEL predicate don't break the
// value-list: the outer single quotes protect against the `,` separator and
// spaces.
func TestParseSelector_State_QuotesAndSpaces(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on state='state.redis_version == "8.0" && state.replicas == 3'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["state"]
	want := `state.redis_version == "8.0" && state.replicas == 3`
	if len(got) != 1 || got[0] != want {
		t.Errorf("Selector[state] = %v, want [%q]", got, want)
	}
}

// Malformed CEL → load error (parseSelector validates compilation through
// statepredicate.Compile).
func TestParseSelector_State_BrokenRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on state='state.redis_version =='`, // unterminated expression
		`incarnation.run on state='state.redis_version && '`,
		`incarnation.run on state='('`, // unbalanced parenthesis
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParsePermission(in)
			if err == nil {
				t.Fatalf("ParsePermission(%q): want compile error, got nil", in)
			}
			if !strings.Contains(err.Error(), "state") {
				t.Errorf("err = %v, want substring \"state\"", err)
			}
		})
	}
}

// A CEL predicate that touches a root/function forbidden in the
// migration-sandbox is rejected at load — state scope is a pure function of
// state (vault/now/register/soulprint/input/essence are forbidden; only
// `state.*` is declared).
func TestParseSelector_State_SandboxRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on state='vault("secret/x") == "y"'`,
		`incarnation.run on state='now() > timestamp("2020-01-01T00:00:00Z")'`,
		`incarnation.run on state='soulprint.self.os.family == "debian"'`,
		`incarnation.run on state='register.self.rc == 0'`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePermission(in); err == nil {
				t.Fatalf("ParsePermission(%q): want sandbox/compile error, got nil", in)
			}
		})
	}
}

// An unquoted state value is forbidden: a predicate with spaces/quotes is
// indistinguishable from a value-list without the quoted form.
func TestParseSelector_State_RequiresQuotes(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on state=state.redis_version`)
	if err == nil {
		t.Fatal("ParsePermission(unquoted state): want error (must be quoted), got nil")
	}
}

// An empty state (state=”) is rejected.
func TestParseSelector_State_EmptyRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on state=''`)
	if err == nil {
		t.Fatal("ParsePermission(state=''): want error for empty predicate, got nil")
	}
}

// An overly long predicate is rejected at load (length cap).
func TestParseSelector_State_LengthCapped(t *testing.T) {
	long := `state.redis_version == "` + strings.Repeat("a", maxStateExprLen) + `"`
	_, err := ParsePermission(`incarnation.run on state='` + long + `'`)
	if err == nil {
		t.Fatal("ParsePermission(over-long state): want length-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too long") && !strings.Contains(err.Error(), "length") {
		t.Errorf("err = %v, want length-cap message", err)
	}
}

// --- Matches: active once incarnation.state is present in context, else fail-closed ---

// Without state in context — a soulprint-like fail-closed deny: the current
// map[string]string context carries no nested state (S3b will supply it).
func TestMatches_State_FailClosedWithoutState(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on state='state.redis_version == "8.0"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Matches("incarnation", "run", map[string]string{"incarnation": "redis-prod", "coven": "prod"}) {
		t.Errorf("state-perm без state-в-context должна давать deny (S2c fail-closed)")
	}
	if p.Matches("incarnation", "run", nil) {
		t.Errorf("state-perm с nil-context должна давать deny")
	}
}

// --- Standalone CEL eval against incarnation.state (via statepredicate, ready for S3b) ---

func TestEvalStateExpr(t *testing.T) {
	v80 := map[string]any{"redis_version": "8.0", "replicas": int64(3)}
	v81 := map[string]any{"redis_version": "8.1", "replicas": int64(3)}

	if ok, err := EvalStateExpr(`state.redis_version == "8.0"`, v80); err != nil || !ok {
		t.Errorf("state 8.0: ok=%v err=%v, want true,nil", ok, err)
	}
	if ok, err := EvalStateExpr(`state.redis_version == "8.0"`, v81); err != nil || ok {
		t.Errorf("state 8.1: ok=%v err=%v, want false,nil", ok, err)
	}
	// A missing key in state → no-match (fail-closed), NOT a function error.
	if ok, err := EvalStateExpr(`state.redis_version == "8.0"`, map[string]any{}); err != nil || ok {
		t.Errorf("пустой state: ok=%v err=%v, want false,nil (no-such-key → no-match)", ok, err)
	}
	// nil state → no-match.
	if ok, err := EvalStateExpr(`state.redis_version == "8.0"`, nil); err != nil || ok {
		t.Errorf("nil-state: ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- Purview.StateExprs ---

// ResolvePurview with a state permission populates Purview.StateExprs.
func TestResolvePurview_State(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "redis8-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.run on state='state.redis_version == "8.0"'`},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (state-scoped)")
	}
	want := `state.redis_version == "8.0"`
	if len(p.StateExprs) != 1 || p.StateExprs[0] != want {
		t.Errorf("StateExprs = %v, want [%q]", p.StateExprs, want)
	}
}

// default_scope=state is inherited by a bare permission (S1+S2c together).
func TestResolvePurview_State_DefaultScopeInherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "redis8-ops", operators: []string{"archon-a"},
		defaultScope: `state='state.redis_version == "8.0"'`,
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (bare наследует state default_scope)")
	}
	want := `state.redis_version == "8.0"`
	if len(p.StateExprs) != 1 || p.StateExprs[0] != want {
		t.Errorf("StateExprs = %v, want [%q] (наследование default_scope)", p.StateExprs, want)
	}
}

// --- subset: state = string-equality fail-closed ---

func TestSubset_State_StringEquality(t *testing.T) {
	v80 := `incarnation.run on state='state.redis_version == "8.0"'`
	v81 := `incarnation.run on state='state.redis_version == "8.1"'`
	// Logically narrower than the v80 predicate (8.0 AND replicas==3), but
	// static CEL containment is undecidable → string-inequal → DENY.
	v80repl := `incarnation.run on state='state.redis_version == "8.0" && state.replicas == 3'`

	tests := []struct {
		name        string
		callerRaws  []string
		grantedRaws []string
		wantHeld    bool // true → ErrPermissionNotHeld (grant denied)
	}{
		{
			name:        "идентичный state → выдача ок",
			callerRaws:  []string{v80},
			grantedRaws: []string{v80},
			wantHeld:    false,
		},
		{
			name:        "иной state → DENY (fail-closed, не string-equal)",
			callerRaws:  []string{v80},
			grantedRaws: []string{v81},
			wantHeld:    true,
		},
		{
			name:        "state-сужение недостижимо статически → DENY",
			callerRaws:  []string{v80},
			grantedRaws: []string{v80repl},
			wantHeld:    true,
		},
		{
			name:        "caller с * выдаёт любой state",
			callerRaws:  []string{"*"},
			grantedRaws: []string{v80},
			wantHeld:    false,
		},
		{
			name:        "caller без state-scope (bare) выдаёт state → ок (bare покрывает)",
			callerRaws:  []string{"incarnation.run"},
			grantedRaws: []string{v80},
			wantHeld:    false,
		},
		{
			name:        "caller со state выдаёт bare → DENY (bare шире state-scope caller-а)",
			callerRaws:  []string{v80},
			grantedRaws: []string{"incarnation.run"},
			wantHeld:    true,
		},
		{
			name:        "caller со state выдаёт coven (иное измерение) → DENY",
			callerRaws:  []string{v80},
			grantedRaws: []string{"incarnation.run on coven=prod"},
			wantHeld:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caller := mustParse(t, tc.callerRaws...)
			required := mustParse(t, tc.grantedRaws...)
			err := assertCallerCovers(caller, required)
			gotHeld := strings.Contains(errString(err), "least-privilege")
			if gotHeld != tc.wantHeld {
				t.Fatalf("assertCallerCovers err = %v; held=%v, want %v", err, gotHeld, tc.wantHeld)
			}
		})
	}
}
