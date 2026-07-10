package rbac

import "testing"

// TestMatches_ResourceWildcard_NoCrossResourceLeak — guard (NIM-79): action-
// wildcard `<resource>.*` покрывает ВСЕ действия своего ресурса (в т.ч.
// чувствительные), но НЕ течёт на другие ресурсы. Matches точно сравнивает
// Resource (permission.go), поэтому `incarnation.*` не даёт прав на
// service/role/operator даже при чувствительном action.
func TestMatches_ResourceWildcard_NoCrossResourceLeak(t *testing.T) {
	p := mustParse(t, "incarnation.*")[0]

	// Тот же resource — любой action матчит, включая destroy / view-secrets.
	for _, action := range []string{"run", "get", "destroy", "view-secrets"} {
		if !p.Matches("incarnation", action, nil) {
			t.Errorf("incarnation.* должна матчить incarnation.%s", action)
		}
	}

	// Другой resource — не матчит ни при каком (в т.ч. чувствительном) action.
	for _, tc := range []struct{ resource, action string }{
		{"service", "register"},
		{"service", "deregister"},
		{"role", "create"},
		{"role", "grant-operator"},
		{"operator", "revoke"},
	} {
		if p.Matches(tc.resource, tc.action, nil) {
			t.Errorf("incarnation.* НЕ должна матчить %s.%s (cross-resource leak)", tc.resource, tc.action)
		}
	}
}

// TestMatches_FullWildcard_MatchesEverything — guard (NIM-79): полный `*` =
// cluster-admin, матчит любой resource/action/context. Отграничивает full-`*`
// от action-wildcard `<resource>.*` (тот скоупится своим ресурсом).
func TestMatches_FullWildcard_MatchesEverything(t *testing.T) {
	star := mustParse(t, "*")[0]
	if !star.IsWildcard {
		t.Fatal("`*` должна парситься в IsWildcard=true")
	}
	for _, tc := range []struct{ resource, action string }{
		{"incarnation", "run"},
		{"incarnation", "view-secrets"},
		{"service", "register"},
		{"role", "grant-operator"},
		{"operator", "revoke"},
	} {
		if !star.Matches(tc.resource, tc.action, map[string]string{"coven": "prod"}) {
			t.Errorf("`*` должна матчить %s.%s", tc.resource, tc.action)
		}
	}
}
