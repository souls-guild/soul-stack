package rbac

import (
	"strings"
	"testing"
)

// ADR-047 S2b — the soulprint selector key: a CEL predicate over host facts
// (`soulprint.self.*`, ADR-018 canonical form). TDD-first: tests fix the
// contract BEFORE the implementation (red), then go green.
//
// S2b scope (like regex in S2a): soulprint is added to the selector grammar
// + Purview.SoulprintExprs + the least-privilege subset check
// (string-equality, fail-closed). Matches in S2b is fail-closed: the current
// context (map[string]string) carries no nested soulprint facts, so a
// soulprint predicate always denies; REAL CEL eval against host facts is
// slices S3/S4 (list visibility/target), where the list/target resolver
// supplies the facts. Standalone eval (EvalSoulprintExpr) is ready and
// tested for S3.

// --- Parsing a quoted soulprint value ---

// soulprint='soulprint.self.os.family == "debian"' parses into
// Selector{soulprint:[…]} (the CEL predicate without its outer quotes).
func TestParseSelector_Soulprint_Simple(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on soulprint='soulprint.self.os.family == "debian"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["soulprint"]
	want := `soulprint.self.os.family == "debian"`
	if len(got) != 1 || got[0] != want {
		t.Errorf("Selector[soulprint] = %v, want [%q]", got, want)
	}
}

// Inner double quotes and spaces in the CEL predicate don't break the
// value-list: the outer single quotes protect against the `,` separator and
// spaces.
func TestParseSelector_Soulprint_QuotesAndSpaces(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on soulprint='soulprint.self.os.family == "debian" && soulprint.self.os.arch == "amd64"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["soulprint"]
	want := `soulprint.self.os.family == "debian" && soulprint.self.os.arch == "amd64"`
	if len(got) != 1 || got[0] != want {
		t.Errorf("Selector[soulprint] = %v, want [%q]", got, want)
	}
}

// Malformed CEL → load error (parseSelector validates compilation through
// shared/cel).
func TestParseSelector_Soulprint_BrokenRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on soulprint='soulprint.self.os.family =='`, // unterminated expression
		`incarnation.run on soulprint='soulprint.self.os.family && '`,
		`incarnation.run on soulprint='('`, // unbalanced parenthesis
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParsePermission(in)
			if err == nil {
				t.Fatalf("ParsePermission(%q): want compile error, got nil", in)
			}
			if !strings.Contains(err.Error(), "soulprint") {
				t.Errorf("err = %v, want substring \"soulprint\"", err)
			}
		})
	}
}

// A CEL predicate that touches a root/function forbidden in the sandbox is
// rejected at load — soulprint scope is a pure function of host facts
// (FlowControl sandbox: vault()/now() guarded, state undeclared).
// register/input/essence ARE declared in FlowControl (the shared context
// set) and compile fine, but are meaningless for a scope predicate (always
// no-such-key → deny); that's a harmless footgun (deny + string-equality
// subset), not a load failure (see observations).
func TestParseSelector_Soulprint_SandboxRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on soulprint='vault("secret/x") == "y"'`,
		`incarnation.run on soulprint='now() > timestamp("2020-01-01T00:00:00Z")'`,
		`incarnation.run on soulprint='state.x == 1'`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePermission(in); err == nil {
				t.Fatalf("ParsePermission(%q): want sandbox/compile error, got nil", in)
			}
		})
	}
}

// soulprint.hosts/soulprint.where is a run-level host accessor, unavailable
// in a scope predicate (allowHosts=false isolation in the FlowControl
// sandbox) → load failure.
func TestParseSelector_Soulprint_HostsAccessorRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on soulprint='soulprint.hosts.exists(h, h.sid == "x")'`)
	if err == nil {
		t.Fatal("ParsePermission(soulprint.hosts ...): want isolation error, got nil")
	}
}

// An unquoted soulprint value is forbidden: a predicate with spaces/quotes
// is indistinguishable from a value-list without the quoted form.
func TestParseSelector_Soulprint_RequiresQuotes(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on soulprint=soulprint.self.os.family`)
	if err == nil {
		t.Fatal("ParsePermission(unquoted soulprint): want error (must be quoted), got nil")
	}
}

// An empty soulprint (soulprint=") is rejected.
func TestParseSelector_Soulprint_EmptyRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on soulprint=''`)
	if err == nil {
		t.Fatal("ParsePermission(soulprint=''): want error for empty predicate, got nil")
	}
}

// An overly long predicate is rejected at load (length cap).
func TestParseSelector_Soulprint_LengthCapped(t *testing.T) {
	long := `soulprint.self.os.family == "` + strings.Repeat("a", maxSoulprintExprLen) + `"`
	_, err := ParsePermission(`incarnation.run on soulprint='` + long + `'`)
	if err == nil {
		t.Fatal("ParsePermission(over-long soulprint): want length-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too long") && !strings.Contains(err.Error(), "length") {
		t.Errorf("err = %v, want length-cap message", err)
	}
}

// --- Matches: fail-closed in S2b (context carries no soulprint facts) ---

// A soulprint predicate in S2b always denies through Matches: the current
// map[string]string context carries no nested facts. Real eval is S3/S4.
func TestMatches_Soulprint_FailClosed(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on soulprint='soulprint.self.os.family == "debian"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	// No context (including coven/host/sid) activates a soulprint predicate
	// in S2b — there are no facts in map[string]string.
	if p.Matches("incarnation", "run", map[string]string{"host": "web-01", "coven": "prod"}) {
		t.Errorf("soulprint-perm should deny in Matches (S2b fail-closed)")
	}
	if p.Matches("incarnation", "run", nil) {
		t.Errorf("soulprint-perm with nil-context should deny")
	}
}

// --- Standalone CEL eval against host facts (ready for S3) ---

func TestEvalSoulprintExpr(t *testing.T) {
	debian := map[string]any{"os": map[string]any{"family": "debian", "arch": "amd64"}}
	rhel := map[string]any{"os": map[string]any{"family": "rhel", "arch": "amd64"}}

	if ok, err := EvalSoulprintExpr(`soulprint.self.os.family == "debian"`, debian); err != nil || !ok {
		t.Errorf("debian host: ok=%v err=%v, want true,nil", ok, err)
	}
	if ok, err := EvalSoulprintExpr(`soulprint.self.os.family == "debian"`, rhel); err != nil || ok {
		t.Errorf("rhel host: ok=%v err=%v, want false,nil", ok, err)
	}
	// A missing key in the facts → no-match (default-deny), NOT a function error.
	if ok, err := EvalSoulprintExpr(`soulprint.self.os.family == "debian"`, map[string]any{}); err != nil || ok {
		t.Errorf("empty facts: ok=%v err=%v, want false,nil (no-such-key -> no-match)", ok, err)
	}
	// nil facts → no-match.
	if ok, err := EvalSoulprintExpr(`soulprint.self.os.family == "debian"`, nil); err != nil || ok {
		t.Errorf("nil facts: ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- Purview.SoulprintExprs ---

// ResolvePurview with a soulprint permission populates Purview.SoulprintExprs.
func TestResolvePurview_Soulprint(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "deb-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.run on soulprint='soulprint.self.os.family == "debian"'`},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (soulprint-scoped)")
	}
	want := `soulprint.self.os.family == "debian"`
	if len(p.SoulprintExprs) != 1 || p.SoulprintExprs[0] != want {
		t.Errorf("SoulprintExprs = %v, want [%q]", p.SoulprintExprs, want)
	}
}

// default_scope=soulprint is inherited by a bare permission (S1+S2b
// together).
func TestResolvePurview_Soulprint_DefaultScopeInherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "deb-ops", operators: []string{"archon-a"},
		defaultScope: `soulprint='soulprint.self.os.family == "debian"'`,
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (bare inherits soulprint default_scope)")
	}
	want := `soulprint.self.os.family == "debian"`
	if len(p.SoulprintExprs) != 1 || p.SoulprintExprs[0] != want {
		t.Errorf("SoulprintExprs = %v, want [%q] (default_scope inheritance)", p.SoulprintExprs, want)
	}
}

// --- subset: soulprint = string-equality fail-closed ---

func TestSubset_Soulprint_StringEquality(t *testing.T) {
	deb := `incarnation.run on soulprint='soulprint.self.os.family == "debian"'`
	rhel := `incarnation.run on soulprint='soulprint.self.os.family == "rhel"'`
	// Logically narrower than the deb predicate (debian AND amd64), but
	// static CEL containment is undecidable → string-inequal → DENY.
	debArch := `incarnation.run on soulprint='soulprint.self.os.family == "debian" && soulprint.self.os.arch == "amd64"'`

	tests := []struct {
		name        string
		callerRaws  []string
		grantedRaws []string
		wantHeld    bool // true → ErrPermissionNotHeld (grant denied)
	}{
		{
			name:        "identical soulprint -> grant ok",
			callerRaws:  []string{deb},
			grantedRaws: []string{deb},
			wantHeld:    false,
		},
		{
			name:        "different soulprint -> DENY (fail-closed, not string-equal)",
			callerRaws:  []string{deb},
			grantedRaws: []string{rhel},
			wantHeld:    true,
		},
		{
			name:        "soulprint narrowing statically undecidable -> DENY",
			callerRaws:  []string{deb},
			grantedRaws: []string{debArch},
			wantHeld:    true,
		},
		{
			name:        "caller with * grants any soulprint",
			callerRaws:  []string{"*"},
			grantedRaws: []string{deb},
			wantHeld:    false,
		},
		{
			name:        "caller without soulprint-scope (bare) grants soulprint -> ok (bare covers)",
			callerRaws:  []string{"incarnation.run"},
			grantedRaws: []string{deb},
			wantHeld:    false,
		},
		{
			name:        "caller with soulprint grants bare -> DENY (bare wider than caller soulprint-scope)",
			callerRaws:  []string{deb},
			grantedRaws: []string{"incarnation.run"},
			wantHeld:    true,
		},
		{
			name:        "caller with soulprint grants coven (different dimension) -> DENY",
			callerRaws:  []string{deb},
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
