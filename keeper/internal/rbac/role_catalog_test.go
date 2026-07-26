package rbac

import (
	"errors"
	"strings"
	"testing"
)

// Guards for the READ catalog's resolved form (ADR-078, NIM-181). What is being
// defended: [LoadRoleViews] publishes a derived role's EFFECTIVE rights, so that no
// consumer has to walk parent_role — and so that none of them can arrive at a wider
// answer than the enforcer does.
//
// The chain arithmetic itself is pinned by flatten_test.go against the enforcer;
// these tests pin that the catalog runs it, on the right side of each field.

// catalogOf resolves a catalog and returns its views keyed by role name.
func catalogOf(t *testing.T, views []RoleView) map[string]RoleView {
	t.Helper()
	if err := resolveRoleViews(views); err != nil {
		t.Fatalf("resolveRoleViews: %v", err)
	}
	out := make(map[string]RoleView, len(views))
	for _, v := range views {
		out[v.Name] = v
	}
	return out
}

func joined(s []string) string { return strings.Join(s, ", ") }

// TestResolveRoleViews_PlainRoleResolvesToItself — a plain role's parent side is the
// unrestricted top (ADR-078(b)), so its effective form is its stored one. The UI can
// therefore read the effective fields unconditionally, without a "is it derived?"
// branch.
func TestResolveRoleViews_PlainRoleResolvesToItself(t *testing.T) {
	got := catalogOf(t, []RoleView{{
		Name:         "ops",
		Permissions:  []string{"incarnation.get", "soul.list on coven=prod"},
		DefaultScope: "coven=prod",
	}})["ops"]

	if want := "incarnation.get, soul.list on coven=prod"; joined(got.EffectivePermissions) != want {
		t.Errorf("effective permissions = %q, want %q", joined(got.EffectivePermissions), want)
	}
	if got.EffectiveScope != "coven=prod" {
		t.Errorf("effective scope = %q, want coven=prod", got.EffectiveScope)
	}
}

// TestResolveRoleViews_ChainOfThreeConjoinsEveryScope — the headline case for J3: a
// chain deeper than one hop resolves in the catalog, every hop contributing its
// narrowing. The stored delta stays the delta — publishing the conjunction in
// default_scope would make the next PATCH re-send the parent's predicate and freeze
// the cascade (ADR-078(b)).
func TestResolveRoleViews_ChainOfThreeConjoinsEveryScope(t *testing.T) {
	got := catalogOf(t, []RoleView{
		{Name: "root", Permissions: []string{"incarnation.get"}, DefaultScope: "coven=dba"},
		{Name: "mid", Permissions: []string{"incarnation.get"}, DefaultScope: "service=redis", ParentRole: "root"},
		{Name: "leaf", Permissions: []string{"incarnation.get"}, DefaultScope: "trait.project=aboba", ParentRole: "mid"},
	})

	leaf := got["leaf"]
	if want := "coven=dba AND service=redis AND trait.project=aboba"; leaf.EffectiveScope != want {
		t.Errorf("leaf effective scope = %q, want %q", leaf.EffectiveScope, want)
	}
	if leaf.DefaultScope != "trait.project=aboba" {
		t.Errorf("leaf stored default_scope = %q, want the delta alone", leaf.DefaultScope)
	}
	if leaf.ParentRole != "mid" {
		t.Errorf("leaf parent_role = %q, want mid", leaf.ParentRole)
	}
	// The middle hop resolves too — a catalog that only resolved the leaves would
	// show `mid` as service=redis alone, i.e. wider than it is.
	if want := "coven=dba AND service=redis"; got["mid"].EffectiveScope != want {
		t.Errorf("mid effective scope = %q, want %q", got["mid"].EffectiveScope, want)
	}
}

// TestResolveRoleViews_DropsWhatTheParentDoesNotHold — variant B (ADR-078(c)) in the
// catalog: a stored row the parent does not cover is absent from the effective set.
// Both forms are published side by side, and the difference between them is exactly
// what the operator must see — the row is stored but grants nothing.
func TestResolveRoleViews_DropsWhatTheParentDoesNotHold(t *testing.T) {
	got := catalogOf(t, []RoleView{
		{Name: "dba", Permissions: []string{"incarnation.get"}, DefaultScope: "coven=dba"},
		{Name: "dba-aboba", Permissions: []string{"incarnation.get", "incarnation.destroy"}, ParentRole: "dba"},
	})["dba-aboba"]

	if want := "incarnation.get"; joined(got.EffectivePermissions) != want {
		t.Errorf("effective permissions = %q, want %q — incarnation.destroy is not the parent's to give",
			joined(got.EffectivePermissions), want)
	}
	if len(got.Permissions) != 2 {
		t.Errorf("stored permissions = %v, want both rows as written", got.Permissions)
	}
}

// TestResolveRoleViews_DerivedWildcardCarriesTheCeiling — a `*` on a derived role is a
// SCOPED super-admin (ADR-078(d)). Rendering it as a bare `*` would read as
// unrestricted cluster-admin: the single most misleading string this endpoint could
// emit, and the reason permString keeps a wildcard's scope.
func TestResolveRoleViews_DerivedWildcardCarriesTheCeiling(t *testing.T) {
	got := catalogOf(t, []RoleView{
		{Name: "admin", Permissions: []string{"*"}, DefaultScope: "coven=dba"},
		{Name: "admin-dba", Permissions: []string{"*"}, ParentRole: "admin"},
	})["admin-dba"]

	if want := "* on coven=dba"; joined(got.EffectivePermissions) != want {
		t.Errorf("effective permissions = %q, want %q", joined(got.EffectivePermissions), want)
	}
}

// TestResolveRoleViews_BrokenGraphFailsClosed — a catalog whose graph does not resolve
// fails the read outright. The alternative — serving the stored rows with the
// effective fields left empty — publishes a derived role's UNATTENUATED set, a
// superset of its rights. Per ADR-078(f) such a catalog builds no enforcer either.
func TestResolveRoleViews_BrokenGraphFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		views []RoleView
		want  error
	}{
		{
			name:  "parent outside the catalog",
			views: []RoleView{{Name: "child", ParentRole: "ghost"}},
			want:  ErrRoleParentUnknown,
		},
		{
			name: "cycle",
			views: []RoleView{
				{Name: "a", ParentRole: "b"},
				{Name: "b", ParentRole: "a"},
			},
			want: ErrRoleParentCycle,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := resolveRoleViews(tc.views)
			if !errors.Is(err, tc.want) {
				t.Fatalf("resolveRoleViews = %v, want %v", err, tc.want)
			}
		})
	}
}
