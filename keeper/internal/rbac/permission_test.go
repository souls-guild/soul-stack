package rbac

import "testing"

// TestMatches_ResourceWildcard_NoCrossResourceLeak — guard (NIM-79): the action
// wildcard `<resource>.*` covers ALL actions of its own resource (including
// sensitive ones), but does NOT leak to other resources. Matches compares
// Resource exactly (permission.go), so `incarnation.*` grants no rights on
// service/role/operator even for a sensitive action.
func TestMatches_ResourceWildcard_NoCrossResourceLeak(t *testing.T) {
	p := mustParse(t, "incarnation.*")[0]

	// Same resource — any action matches, including destroy / view-secrets.
	for _, action := range []string{"run", "get", "destroy", "view-secrets"} {
		if !p.Matches("incarnation", action, nil) {
			t.Errorf("incarnation.* should match incarnation.%s", action)
		}
	}

	// Different resource — does not match for any action (including sensitive ones).
	for _, tc := range []struct{ resource, action string }{
		{"service", "register"},
		{"service", "deregister"},
		{"role", "create"},
		{"role", "grant-operator"},
		{"operator", "revoke"},
	} {
		if p.Matches(tc.resource, tc.action, nil) {
			t.Errorf("incarnation.* should NOT match %s.%s (cross-resource leak)", tc.resource, tc.action)
		}
	}
}

// TestMatches_FullWildcard_MatchesEverything — guard (NIM-79): full `*` =
// cluster-admin, matches any resource/action/context. Distinguishes full-`*`
// from the action-wildcard `<resource>.*` (which is scoped to its resource).
func TestMatches_FullWildcard_MatchesEverything(t *testing.T) {
	star := mustParse(t, "*")[0]
	if !star.IsWildcard {
		t.Fatal("`*` should parse to IsWildcard=true")
	}
	for _, tc := range []struct{ resource, action string }{
		{"incarnation", "run"},
		{"incarnation", "view-secrets"},
		{"service", "register"},
		{"role", "grant-operator"},
		{"operator", "revoke"},
	} {
		if !star.Matches(tc.resource, tc.action, map[string]string{"coven": "prod"}) {
			t.Errorf("`*` should match %s.%s", tc.resource, tc.action)
		}
	}
}
