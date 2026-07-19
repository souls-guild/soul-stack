package rbac

import (
	"strings"
	"testing"
)

func TestParsePermission_FullWildcard(t *testing.T) {
	p, err := ParsePermission("*")
	if err != nil {
		t.Fatalf("ParsePermission(*): %v", err)
	}
	if !p.IsWildcard {
		t.Errorf("IsWildcard = false, want true")
	}
	if p.Resource != "" || p.Action != "" {
		t.Errorf("Resource/Action non-empty: %q/%q", p.Resource, p.Action)
	}
}

func TestParsePermission_BareResourceAction(t *testing.T) {
	p, err := ParsePermission("incarnation.create")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.IsWildcard {
		t.Errorf("IsWildcard = true; want false for bare")
	}
	if p.Resource != "incarnation" {
		t.Errorf("Resource = %q", p.Resource)
	}
	if p.Action != "create" {
		t.Errorf("Action = %q", p.Action)
	}
	if p.Scope != nil {
		t.Errorf("Scope = %v, want nil", p.Scope)
	}
}

func TestParsePermission_ActionWildcard(t *testing.T) {
	p, err := ParsePermission("incarnation.*")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Resource != "incarnation" || p.Action != "*" {
		t.Errorf("Resource/Action = %q/%q", p.Resource, p.Action)
	}
}

func TestParsePermission_KebabCaseAction(t *testing.T) {
	p, err := ParsePermission("operator.issue-token")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Action != "issue-token" {
		t.Errorf("Action = %q", p.Action)
	}
}

// TestParsePermission_RolePermissions — all six role.*-permissions
// (ADR-028(e)) parse without errors. Without a catalog entry (catalog.go),
// a role with these permissions would fail to parse (unknown_permission).
func TestParsePermission_RolePermissions(t *testing.T) {
	names := []string{
		"role.create", "role.delete", "role.list",
		"role.update", "role.grant-operator", "role.revoke-operator",
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			p, err := ParsePermission(n)
			if err != nil {
				t.Fatalf("ParsePermission(%q): %v", n, err)
			}
			if p.Resource != "role" {
				t.Errorf("Resource = %q, want role", p.Resource)
			}
		})
	}
}

func TestParsePermission_WithSelector(t *testing.T) {
	// The old flat form `service=redis,vault` canonicalizes to an in-list.
	p, err := ParsePermission("incarnation.create on service=redis,vault")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Scope == nil {
		t.Fatal("Scope = nil, want a service in-list predicate")
	}
	if got := p.Scope.String(); got != "service in (redis, vault)" {
		t.Errorf("Scope = %q, want %q", got, "service in (redis, vault)")
	}
}

// ADR-047 S1 / NIM-128: ParseDefaultScope reuses the boolean scope grammar —
// the same closed dimension enum and predicate syntax as the per-perm scope.
func TestParseDefaultScope(t *testing.T) {
	t.Run("empty-nil", func(t *testing.T) {
		sel, err := ParseDefaultScope("")
		if err != nil || sel != nil {
			t.Fatalf("ParseDefaultScope(\"\") = (%v, %v), want (nil, nil)", sel, err)
		}
	})
	t.Run("coven-multi", func(t *testing.T) {
		sel, err := ParseDefaultScope("coven=prod,stage")
		if err != nil {
			t.Fatalf("ParseDefaultScope: %v", err)
		}
		if got := sel.String(); got != "coven in (prod, stage)" {
			t.Errorf("sel = %q, want %q", got, "coven in (prod, stage)")
		}
	})
	t.Run("errors", func(t *testing.T) {
		cases := []struct{ in, want string }{
			{"coven", "expected '='"},
			{"namespace=foo", "unknown dimension"},
			{"coven=", "expected a value"},
		}
		for _, c := range cases {
			if _, err := ParseDefaultScope(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("ParseDefaultScope(%q) err = %v, want substring %q", c.in, err, c.want)
			}
		}
	})
}

func TestParsePermission_Errors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring
	}{
		{"empty", "", "empty string"},
		{"three-segments", "keeper.incarnation.create", "exactly two segments"},
		{"no-dot", "incarnation", "expected <resource>.<action>"},
		{"wildcard-resource", "*.create", "wildcard in <resource>"},
		{"upper-resource", "Incarnation.create", "does not match"},
		{"underscore-action", "incarnation.add_user", "does not match"},
		{"unknown-perm", "unknown.create", "unknown_permission"},
		{"unknown-selector-key", "incarnation.create on namespace=foo", "unknown dimension"},
		{"selector-no-eq", "incarnation.create on service", "expected '='"},
		{"selector-empty-value", "incarnation.create on service=", "expected a value"},
		{"selector-empty-mid", "incarnation.create on service=foo,,bar", "expected a value"},
		{"selector-bad-value", "incarnation.create on service=foo bar", "unexpected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParsePermission(c.in)
			if err == nil {
				t.Fatalf("ParsePermission(%q): want error containing %q, got nil", c.in, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v; want substring %q", err, c.want)
			}
		})
	}
}

func TestParsePermission_AllCatalogEntries(t *testing.T) {
	// All catalog names should parse without errors (sanity).
	for name := range AllowedPermissions {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePermission(name); err != nil {
				t.Errorf("catalog entry %q failed to parse: %v", name, err)
			}
		})
	}
}

func TestIsAllowedPermission_CadenceEnableDisable(t *testing.T) {
	// cadence.enable / cadence.disable — granular rights to toggle the
	// schedule (split off from cadence.update; cadence.update remains a
	// backcompat grant).
	for _, action := range []string{"enable", "disable"} {
		if !IsAllowedPermission("cadence", action) {
			t.Errorf("cadence.%s should be in catalog", action)
		}
	}
}

func TestIsAllowedPermission_SynodUpdate(t *testing.T) {
	// synod.update (ADR-049 amend) — editing description. Must be in the
	// catalog: otherwise a role with this right would be rejected as
	// unknown_permission when loading the snapshot.
	if !IsAllowedPermission("synod", "update") {
		t.Errorf("synod.update should be in catalog (ADR-049 amend)")
	}
}

func TestIsAllowedPermission_WildcardAction(t *testing.T) {
	if !IsAllowedPermission("incarnation", "*") {
		t.Errorf("incarnation.* should be valid (catalog has incarnation.create etc)")
	}
	if IsAllowedPermission("unknown", "*") {
		t.Errorf("unknown.* should NOT be valid")
	}
}
