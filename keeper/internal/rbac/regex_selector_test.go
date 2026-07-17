package rbac

import (
	"strings"
	"testing"
)

// ADR-047 S2a — the regex selector key: matches SID/hostname (RE2).
// TDD-first: tests pin down the contract BEFORE the implementation (red),
// then go green.
//
// S2a boundary: regex is added to the selector grammar + Purview.Regexes +
// accounted for in Matches (host-context) and the least-privilege subset
// (string-equality fail-closed). REAL application to list-visibility/target
// intersection is S3/S4.

// --- Parsing a quoted regex value ---

// regex='^web-' parses into Selector{regex:[^web-]} (value without quotes).
func TestParseSelector_Regex_Simple(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^web-'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["regex"]
	if len(got) != 1 || got[0] != "^web-" {
		t.Errorf("Selector[regex] = %v, want [^web-]", got)
	}
}

// A comma INSIDE the regex ({1,3}) doesn't split the value — the quoted form
// protects against the `,` value-list separator.
func TestParseSelector_Regex_CommaInsideQuotes(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^a{1,3}$'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["regex"]
	if len(got) != 1 || got[0] != "^a{1,3}$" {
		t.Errorf("Selector[regex] = %v, want [^a{1,3}$] (comma inside regex does not split)", got)
	}
}

// A broken regex → load error (parseSelector validates via regexp.Compile).
func TestParseSelector_Regex_BrokenRejected(t *testing.T) {
	cases := []string{
		"incarnation.run on regex='^web-['",    // unclosed character class
		"incarnation.run on regex='(unclosed'", // unclosed group
		"incarnation.run on regex='*'",         // no operand for the quantifier
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParsePermission(in)
			if err == nil {
				t.Fatalf("ParsePermission(%q): want compile error, got nil", in)
			}
			if !strings.Contains(err.Error(), "regex") {
				t.Errorf("err = %v, want substring \"regex\"", err)
			}
		})
	}
}

// An unquoted regex value is forbidden: an unquoted regex is indistinguishable
// from an exact value, and special characters won't pass reSelValue — we
// require the quoted form explicitly.
func TestParseSelector_Regex_RequiresQuotes(t *testing.T) {
	_, err := ParsePermission("incarnation.run on regex=^web-")
	if err == nil {
		t.Fatal("ParsePermission(regex=^web-): want error (regex must be quoted), got nil")
	}
}

// An overly long regex is rejected at load (a ReDoS length cap).
func TestParseSelector_Regex_LengthCapped(t *testing.T) {
	long := strings.Repeat("a", maxRegexLen+1)
	_, err := ParsePermission("incarnation.run on regex='" + long + "'")
	if err == nil {
		t.Fatal("ParsePermission(over-long regex): want length-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too long") && !strings.Contains(err.Error(), "length") {
		t.Errorf("err = %v, want length-cap message", err)
	}
}

// An empty regex value (regex set to an empty string) is rejected.
func TestParseSelector_Regex_EmptyRejected(t *testing.T) {
	_, err := ParsePermission("incarnation.run on regex=''")
	if err == nil {
		t.Fatal("ParsePermission(regex=''): want error for empty regex, got nil")
	}
}

// --- Matches with host/sid context ---

// permission incarnation.run on regex='^web-' + context{host: web-01} → match.
func TestMatches_Regex_HostContext(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^web-'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if !p.Matches("incarnation", "run", map[string]string{"host": "web-01"}) {
		t.Errorf("regex=^web- should match host=web-01")
	}
	if p.Matches("incarnation", "run", map[string]string{"host": "db-01"}) {
		t.Errorf("regex=^web- should NOT match host=db-01")
	}
}

// regex also matches via the sid key in the context (some endpoints put sid there).
func TestMatches_Regex_SidContext(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^web-'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if !p.Matches("incarnation", "run", map[string]string{"sid": "web-01.example.com"}) {
		t.Errorf("regex=^web- should match sid=web-01.example.com")
	}
}

// A regex key without host/sid in the context → no match (like an exact key
// missing its own key).
func TestMatches_Regex_NoHostKeyDeny(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^web-'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Matches("incarnation", "run", map[string]string{"coven": "prod"}) {
		t.Errorf("regex-perm without host/sid in context should give deny")
	}
	if p.Matches("incarnation", "run", nil) {
		t.Errorf("regex-perm with nil context should give deny")
	}
}

// --- Purview.Regexes ---

// ResolvePurview with a regex permission populates Purview.Regexes.
func TestResolvePurview_Regex(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "web-ops", operators: []string{"archon-a"},
		permissions: []string{"incarnation.run on regex='^web-'"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (regex-scoped)")
	}
	if len(p.Regexes) != 1 || p.Regexes[0] != "^web-" {
		t.Errorf("Regexes = %v, want [^web-]", p.Regexes)
	}
}

// default_scope=regex is inherited by a bare permission (S1+S2a together).
func TestResolvePurview_Regex_DefaultScopeInherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "web-ops", operators: []string{"archon-a"},
		defaultScope: "regex='^web-'",
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (bare inherits regex default_scope)")
	}
	if len(p.Regexes) != 1 || p.Regexes[0] != "^web-" {
		t.Errorf("Regexes = %v, want [^web-] (default_scope inheritance)", p.Regexes)
	}
}

// --- subset: regex = string-equality fail-closed ---

func TestSubset_Regex_StringEquality(t *testing.T) {
	tests := []struct {
		name        string
		callerRaws  []string
		grantedRaws []string
		wantHeld    bool // true → ErrPermissionNotHeld (grant forbidden)
	}{
		{
			name:        "identical regex -> grant ok",
			callerRaws:  []string{"incarnation.run on regex='^web-'"},
			grantedRaws: []string{"incarnation.run on regex='^web-'"},
			wantHeld:    false,
		},
		{
			name:        "different regex -> DENY (fail-closed, not string-equal)",
			callerRaws:  []string{"incarnation.run on regex='^web-'"},
			grantedRaws: []string{"incarnation.run on regex='^db-'"},
			wantHeld:    true,
		},
		{
			name:        "regex narrowing is not statically provable -> DENY (^web- does not cover ^web-prod-)",
			callerRaws:  []string{"incarnation.run on regex='^web-'"},
			grantedRaws: []string{"incarnation.run on regex='^web-prod-'"},
			wantHeld:    true,
		},
		{
			name:        "caller with * grants any regex",
			callerRaws:  []string{"*"},
			grantedRaws: []string{"incarnation.run on regex='^web-'"},
			wantHeld:    false,
		},
		{
			name:        "caller without regex-scope (bare) grants regex -> ok (bare covers)",
			callerRaws:  []string{"incarnation.run"},
			grantedRaws: []string{"incarnation.run on regex='^web-'"},
			wantHeld:    false,
		},
		{
			name:        "caller with regex grants bare -> DENY (bare is wider than caller regex-scope)",
			callerRaws:  []string{"incarnation.run on regex='^web-'"},
			grantedRaws: []string{"incarnation.run"},
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

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
