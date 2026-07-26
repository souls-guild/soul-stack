package rbac

import (
	"errors"
	"strings"
	"testing"
)

// Guard matrix for chain resolution (ADR-078(c)/(d), NIM-180). Every case here is
// a security invariant of the derived-role model, checked at the layer that
// decides: the enforcer built from a snapshot.
//
// The tests deliberately go through [NewEnforcerFromSnapshot] rather than calling
// [attenuate] directly — the claim being defended is "a permission check reads the
// attenuated form", and only the full build proves that.

// derivedEnforcer builds an enforcer from a compact catalog description and binds
// every role to archon-alice, so a purview can be resolved for any of them.
func derivedEnforcer(t *testing.T, roles map[string][]string, scopes, parents map[string]string) *Enforcer {
	t.Helper()
	member := make([]string, 0, len(roles))
	for name := range roles {
		member = append(member, name)
	}
	e, err := NewEnforcerFromSnapshot(&Snapshot{
		Roles:       roles,
		RoleScopes:  scopes,
		RoleParents: parents,
		Membership:  map[string][]string{"archon-alice": member},
	})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	return e
}

// roleByName returns a role from a built enforcer — i.e. in its EFFECTIVE form.
func roleByName(t *testing.T, e *Enforcer, name string) *Role {
	t.Helper()
	for _, r := range e.roles {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("role %q not in the enforcer", name)
	return nil
}

// permStrings renders a role's effective permissions for comparison.
func permStrings(r *Role) []string {
	out := make([]string, 0, len(r.Permissions))
	for _, p := range r.Permissions {
		out = append(out, permString(p))
	}
	return out
}

// TestFlatten_ParentScopeCascadesIntoChild — the headline behaviour: the child
// stores only its added narrowing, and its effective scope is the parent's AND
// that delta. Moving the parent moves the child, with no role rewritten by hand.
//
// The second half is the invariant that makes the cascade safe: the child's scope
// after the move contains the NEW parent predicate and no trace of the old one. A
// child that had materialized `coven=dba` would keep pointing at the coven the
// operator just left.
func TestFlatten_ParentScopeCascadesIntoChild(t *testing.T) {
	build := func(parentScope string) *Role {
		e := derivedEnforcer(t,
			map[string][]string{"dba": {"incarnation.get"}, "dba-aboba": {"incarnation.get"}},
			map[string]string{"dba": parentScope, "dba-aboba": "trait.project=aboba"},
			map[string]string{"dba-aboba": "dba"})
		return roleByName(t, e, "dba-aboba")
	}

	before := build("coven=dba")
	if got, want := before.DefaultScope.String(), "coven=dba AND trait.project=aboba"; got != want {
		t.Fatalf("effective scope = %q, want %q", got, want)
	}

	after := build("coven=dbaas")
	if got, want := after.DefaultScope.String(), "coven=dbaas AND trait.project=aboba"; got != want {
		t.Fatalf("after the parent moved, effective scope = %q, want %q", got, want)
	}
	if strings.Contains(after.DefaultScope.String(), "coven=dba ") {
		t.Error("the child still carries the parent's OLD predicate — the delta was materialized")
	}
}

// TestFlatten_ChainNarrowsAtEveryHop — a three-role chain conjoins all three
// scopes. Depth is capped, not forbidden, and each hop must contribute.
func TestFlatten_ChainNarrowsAtEveryHop(t *testing.T) {
	e := derivedEnforcer(t,
		map[string][]string{"root": {"incarnation.get"}, "mid": {"incarnation.get"}, "leaf": {"incarnation.get"}},
		map[string]string{"root": "coven=dba", "mid": "service=redis", "leaf": "trait.project=aboba"},
		map[string]string{"mid": "root", "leaf": "mid"})

	got := roleByName(t, e, "leaf").DefaultScope.String()
	want := "coven=dba AND service=redis AND trait.project=aboba"
	if got != want {
		t.Errorf("leaf effective scope = %q, want %q", got, want)
	}
}

// TestFlatten_ChildCannotAddAPermission — variant B (ADR-078(c)): the child's own
// rows are its effective set, INTERSECTED with the parent's. A permission the
// parent does not hold is dropped, so the child cannot mint rights sideways.
func TestFlatten_ChildCannotAddAPermission(t *testing.T) {
	e := derivedEnforcer(t,
		map[string][]string{
			"dba":       {"incarnation.get"},
			"dba-aboba": {"incarnation.get", "incarnation.destroy"},
		},
		map[string]string{"dba": "coven=dba"},
		map[string]string{"dba-aboba": "dba"})

	child := roleByName(t, e, "dba-aboba")
	if got, want := permStrings(child), []string{"incarnation.get"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("child effective permissions = %v, want %v", got, want)
	}
	if err := e.Check("archon-alice", "incarnation", "destroy", map[string]string{"coven": "dba"}); err == nil {
		t.Error("incarnation.destroy is not the parent's to give — Check must deny it")
	}
}

// TestFlatten_ChildCannotWidenScope — a child whose per-permission scope reaches
// outside its parent's is dropped, not clipped. Two shapes, and the second is the
// one that matters most: a BARE permission on the child would otherwise resolve
// to unrestricted and silently escape a parent whose narrowing lives in a
// per-permission scope rather than in default_scope.
func TestFlatten_ChildCannotWidenScope(t *testing.T) {
	tests := []struct {
		name       string
		parentPerm string
		childPerm  string
	}{
		{
			name:       "wider value set",
			parentPerm: "incarnation.get on coven=dba",
			childPerm:  "incarnation.get on coven in (dba, prod)",
		},
		{
			name:       "bare child against a scoped parent",
			parentPerm: "incarnation.get on coven=dba",
			childPerm:  "incarnation.get",
		},
		{
			name:       "sideways into another coven",
			parentPerm: "incarnation.get on coven=dba",
			childPerm:  "incarnation.get on coven=prod",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := derivedEnforcer(t,
				map[string][]string{"dba": {tc.parentPerm}, "child": {tc.childPerm}},
				nil,
				map[string]string{"child": "dba"})
			if got := permStrings(roleByName(t, e, "child")); len(got) != 0 {
				t.Errorf("child kept %v, want nothing (the parent does not cover it)", got)
			}
		})
	}
}

// TestFlatten_ChildNarrowsWithinParentIsKept — the mirror of the case above: a
// per-permission scope that stays inside the parent survives, and comes out
// conjoined with the parent's ceiling rather than replacing it.
func TestFlatten_ChildNarrowsWithinParentIsKept(t *testing.T) {
	e := derivedEnforcer(t,
		map[string][]string{"dba": {"incarnation.get"}, "child": {"incarnation.get on trait.project=aboba"}},
		map[string]string{"dba": "coven=dba"},
		map[string]string{"child": "dba"})

	got := permStrings(roleByName(t, e, "child"))
	want := "incarnation.get on coven=dba AND trait.project=aboba"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("child effective permissions = %v, want [%q]", got, want)
	}
}

// TestFlatten_WildcardIsBoundedByTheParentCeiling — ADR-047(b) #1 says a role's
// own default_scope never touches `*`. A PARENT's ceiling is a different thing
// and must: otherwise a derived role carrying `*` would resolve to an
// unrestricted cluster-admin under a scoped parent — the escalation this design
// exists to prevent.
func TestFlatten_WildcardIsBoundedByTheParentCeiling(t *testing.T) {
	// archon-alice holds ONLY the derived role: the parent's own `*` is
	// unrestricted by ADR-047(b) #1 and would mask the child's bound.
	e, err := NewEnforcerFromSnapshot(&Snapshot{
		Roles:       map[string][]string{"admin": {"*"}, "admin-dba": {"*"}},
		RoleScopes:  map[string]string{"admin": "coven=dba"},
		RoleParents: map[string]string{"admin-dba": "admin"},
		Membership:  map[string][]string{"archon-alice": {"admin-dba"}},
	})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}

	child := roleByName(t, e, "admin-dba")
	if len(child.Permissions) != 1 {
		t.Fatalf("child effective permissions = %v, want one wildcard", permStrings(child))
	}
	if child.Permissions[0].Scope == nil {
		t.Fatal("the derived `*` came out UNRESTRICTED — the parent's ceiling was not applied")
	}
	if got, want := child.Permissions[0].Scope.String(), "coven=dba"; got != want {
		t.Errorf("derived `*` scope = %q, want %q", got, want)
	}
	// And the bound is real at check time, not just in the stored shape.
	if err := e.Check("archon-alice", "incarnation", "destroy", map[string]string{"coven": "prod"}); err == nil {
		t.Error("a `*` derived from a coven=dba parent must not reach coven=prod")
	}
}

// TestFlatten_WildcardWithoutAParentWildcardIsDropped — a child cannot invent a
// `*` its parent never had. There is nothing to attenuate, so it grants nothing.
func TestFlatten_WildcardWithoutAParentWildcardIsDropped(t *testing.T) {
	e := derivedEnforcer(t,
		map[string][]string{"dba": {"incarnation.get"}, "sneaky": {"*"}},
		map[string]string{"dba": "coven=dba"},
		map[string]string{"sneaky": "dba"})

	if got := permStrings(roleByName(t, e, "sneaky")); len(got) != 0 {
		t.Errorf("child kept %v, want nothing — the parent holds no `*`", got)
	}
}

// TestFlatten_ParentIsOneRoleNotTheUnion — ADR-078(e). An operator holding a wide
// role AND a role derived from a narrow one gets the union of the two resolved
// roles; the derived role itself is still bounded by the ONE role it names. If
// derivation resolved against "the operator's rights", the narrow child would
// silently inherit the wide role's ceiling.
func TestFlatten_ParentIsOneRoleNotTheUnion(t *testing.T) {
	e := derivedEnforcer(t,
		map[string][]string{
			"wide":   {"incarnation.get", "incarnation.destroy"},
			"narrow": {"incarnation.get"},
			"child":  {"incarnation.get", "incarnation.destroy"},
		},
		map[string]string{"narrow": "coven=dba"},
		map[string]string{"child": "narrow"})

	got := permStrings(roleByName(t, e, "child"))
	if len(got) != 1 || got[0] != "incarnation.get" {
		t.Errorf("child effective permissions = %v, want only incarnation.get — "+
			"the ceiling is `narrow`, not the union with `wide`", got)
	}
}

// TestFlatten_UndecidableGlobFailsClosed — glob subsumption is not decided
// (NIM-128 §C.5 keeps [atomSubset] conservative), so a child glob that is not
// provably inside the parent's is DROPPED rather than assumed safe. `web-0?`
// really is a subset of `web-*`, and it is still refused: fail-closed means the
// undecidable case costs an operator a role, not the cluster a boundary.
func TestFlatten_UndecidableGlobFailsClosed(t *testing.T) {
	e := derivedEnforcer(t,
		map[string][]string{
			"fleet": {"incarnation.get on host matches web-*"},
			"child": {"incarnation.get on host matches web-0?"},
		},
		nil,
		map[string]string{"child": "fleet"})

	if got := permStrings(roleByName(t, e, "child")); len(got) != 0 {
		t.Errorf("child kept %v, want nothing (glob containment is not decided)", got)
	}
}

// TestFlatten_BrokenChainNeverBecomesUnrestricted — the fail-closed direction of
// the orphan policy, at the read side. A child whose parent is missing from the
// catalog must not resolve to "no ceiling": the whole build fails instead, and
// [Holder] keeps the previous enforcer.
func TestFlatten_BrokenChainNeverBecomesUnrestricted(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(&Snapshot{
		Roles:       map[string][]string{"orphan": {"*"}},
		RoleParents: map[string]string{"orphan": "deleted-parent"},
		Membership:  map[string][]string{"archon-alice": {"orphan"}},
	})
	if !errors.Is(err, ErrRoleParentUnknown) {
		t.Fatalf("err = %v, want ErrRoleParentUnknown", err)
	}
	if e != nil {
		t.Fatal("an orphaned child must not yield an enforcer that grants its `*` unbounded")
	}
}

// TestFlatten_ScopeTooComplexIsRefused — the conjunction of a chain's scopes is
// normalized to DNF by the containment check. A chain whose product blows past
// [maxDNFConjuncts] is refused, not approximated: a scope nothing can normalize
// is a scope nothing can bound.
func TestFlatten_ScopeTooComplexIsRefused(t *testing.T) {
	// Each level is a 7-way OR; three levels multiply to 343 disjuncts, past the
	// 256 cap, while every individual predicate parses fine on its own.
	wide := func(prefix string) string {
		var sb strings.Builder
		for i := 0; i < 7; i++ {
			if i > 0 {
				sb.WriteString(" OR ")
			}
			sb.WriteString("coven=" + prefix + string(rune('a'+i)))
		}
		return sb.String()
	}
	_, err := NewEnforcerFromSnapshot(&Snapshot{
		Roles: map[string][]string{"r1": {"incarnation.get"}, "r2": {"incarnation.get"}, "r3": {"incarnation.get"}},
		RoleScopes: map[string]string{
			"r1": wide("x"), "r2": wide("y"), "r3": wide("z"),
		},
		RoleParents: map[string]string{"r2": "r1", "r3": "r2"},
	})
	if !errors.Is(err, ErrRoleScopeTooComplex) {
		t.Fatalf("err = %v, want ErrRoleScopeTooComplex", err)
	}
}

// TestFlatten_PlainRolesAreUntouched — backcompat. Every role that existed before
// derivation reads back bit-for-bit: same permissions, same ADR-047 scope
// semantics, no ceiling conjoined onto anything.
func TestFlatten_PlainRolesAreUntouched(t *testing.T) {
	e := derivedEnforcer(t,
		map[string][]string{"ops": {"incarnation.get", "incarnation.run on coven=prod", "*"}},
		map[string]string{"ops": "coven=dba"},
		nil)

	role := roleByName(t, e, "ops")
	if got, want := role.DefaultScope.String(), "coven=dba"; got != want {
		t.Errorf("plain role scope = %q, want %q", got, want)
	}
	want := []string{"incarnation.get", "incarnation.run on coven=prod", "*"}
	got := permStrings(role)
	if len(got) != len(want) {
		t.Fatalf("plain role permissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("permission %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestHasWildcard_DerivedRoleIsNeverClusterAdmin — the self-lockout half of
// ADR-078(i), at the enforcer. A derived role is skipped even when its chain
// currently resolves to an unrestricted `*` (an unscoped parent): its
// admin-ness depends on a parent that any other mutation may narrow, so counting
// it would let the real last admin be removed.
func TestHasWildcard_DerivedRoleIsNeverClusterAdmin(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(&Snapshot{
		Roles:       map[string][]string{"cluster-admin": {"*"}, "admin-copy": {"*"}},
		RoleParents: map[string]string{"admin-copy": "cluster-admin"},
		Membership: map[string][]string{
			"archon-alice": {"cluster-admin"},
			"archon-bob":   {"admin-copy"},
		},
	})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}

	if !e.HasWildcard("archon-alice") {
		t.Error("archon-alice holds `*` through a PLAIN role — must count as cluster-admin")
	}
	if e.HasWildcard("archon-bob") {
		t.Error("archon-bob holds `*` only through a DERIVED role — must NOT count (ADR-078(i))")
	}
	admins := e.ClusterAdmins()
	if len(admins) != 1 || admins[0] != "archon-alice" {
		t.Errorf("ClusterAdmins() = %v, want [archon-alice]", admins)
	}
	// Skipping the count does not take the rights away — bob still resolves to
	// `*` through his chain; he is simply not a lockout survivor.
	if err := e.Check("archon-bob", "incarnation", "destroy", nil); err != nil {
		t.Errorf("archon-bob lost the rights his chain grants: %v", err)
	}
}

// TestAndScopes — the conjunction helper. nil is the unrestricted top, nested AND
// nodes flatten, and a term restated by a child collapses to one: same predicate,
// smaller DNF, stable canonical string.
func TestAndScopes(t *testing.T) {
	parse := func(s string) *ScopeExpr {
		t.Helper()
		e, err := ParseScopeExpr(s)
		if err != nil {
			t.Fatalf("ParseScopeExpr(%q): %v", s, err)
		}
		return e
	}

	if got := andScopes(nil, nil); got != nil {
		t.Errorf("andScopes(nil, nil) = %v, want nil (top AND top)", got)
	}
	if got := andScopes(nil, parse("coven=dba")).String(); got != "coven=dba" {
		t.Errorf("andScopes(nil, x) = %q, want %q", got, "coven=dba")
	}
	if got := andScopes(parse("coven=dba"), nil).String(); got != "coven=dba" {
		t.Errorf("andScopes(x, nil) = %q, want %q", got, "coven=dba")
	}
	if got, want := andScopes(parse("coven=dba"), parse("coven=dba")).String(), "coven=dba"; got != want {
		t.Errorf("restated term: %q, want %q (deduped)", got, want)
	}
	if got, want := andScopes(
		parse("coven=dba AND service=redis"),
		parse("trait.project=aboba"),
	).String(), "coven=dba AND service=redis AND trait.project=aboba"; got != want {
		t.Errorf("nested AND: %q, want %q (flattened)", got, want)
	}
	// An OR operand keeps its grouping — flattening only ever applies to AND.
	if got, want := andScopes(
		parse("coven=dba OR coven=dbaas"),
		parse("trait.project=aboba"),
	).String(), "(coven=dba OR coven=dbaas) AND trait.project=aboba"; got != want {
		t.Errorf("OR operand: %q, want %q", got, want)
	}
}
