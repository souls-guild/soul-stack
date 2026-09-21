package rbac

import (
	"errors"
	"testing"
	"time"
)

// NIM-219 — a role's default_scope bounds its bare permissions at the GATE, not
// only in the resolver.
//
// [Enforcer.ResolvePurview] has always inherited default_scope onto a bare
// permission (ADR-047(a)); [Enforcer.Check] matched permissions role-blind and
// so read the same bare permission as unrestricted. The two paths disagreed, and
// the gate was the wider of the two — a role confined to `coven=dba` on every
// read ran against ANY coven on the write endpoints, which is where it matters
// more. These tests pin the agreement and the three edges that must survive it:
// bare `*` is never bounded, a role with no default_scope stays unrestricted,
// and a per-permission scope still overrides.

// The bug itself: default_scope=coven=dba + a BARE incarnation.run. In scope →
// allow; out of scope → deny; no context at all → deny (nothing satisfies a
// predicate). Before NIM-219 all three allowed.
func TestCheck_DefaultScope_BoundsBarePermission(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "dba", operators: []string{"archon-bob"},
		defaultScope: "coven=dba",
		permissions:  []string{"incarnation.run"},
	})

	if err := e.Check("archon-bob", "incarnation", "run", map[string]string{"coven": "dba"}); err != nil {
		t.Errorf("in-scope coven=dba denied: %v", err)
	}
	if err := e.Check("archon-bob", "incarnation", "run", map[string]string{"coven": "web"}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("coven=web = %v, want ErrPermissionDenied — the role is confined to coven=dba", err)
	}
	if err := e.Check("archon-bob", "incarnation", "run", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("nil context = %v, want ErrPermissionDenied (a scoped role needs a context, "+
			"ADR-047 §d: a gate that cannot know the scope asks HoldsAction instead)", err)
	}

	// The existence gate is NOT what changed: a route guarded by RequireAction
	// still lets this operator through, and the handler narrows by purview.
	// Without this, the fix would cut a scoped role off from its own reads —
	// the NIM-144 failure mode.
	if !e.HoldsAction("archon-bob", "incarnation", "run") {
		t.Error("HoldsAction=false — the existence gate must still hold; only the scope-aware gate narrowed")
	}
}

// ADR-047(b) exception #1 — a BARE `*` is cluster-admin and default_scope never
// touches it, in either path. Binding it would let a bootstrap admin lock itself
// out of RBAC (ADR-013/ADR-014).
func TestCheck_DefaultScope_DoesNotBindBareWildcard(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "admin", operators: []string{"archon-root"},
		defaultScope: "coven=dba",
		permissions:  []string{"*"},
	})

	for _, ctx := range []map[string]string{nil, {"coven": "dba"}, {"coven": "web"}} {
		if err := e.Check("archon-root", "role", "update", ctx); err != nil {
			t.Errorf("bare `*` denied with context %v: %v — default_scope must not bind cluster-admin", ctx, err)
		}
	}
	// The context-less cluster ops are the ones that would lock the admin out.
	if err := e.Check("archon-root", "operator", "revoke", nil); err != nil {
		t.Errorf("bare `*` denied operator.revoke: %v", err)
	}
	if !e.ResolvePurview("archon-root", "role", "update").Unrestricted {
		t.Error("ResolvePurview lost Unrestricted for a bare `*` under a scoped role")
	}
	// Self-lockout still counts this operator: cluster-admin is bare-`*` only,
	// and a default_scope on the role does not demote it.
	if !e.HasWildcard("archon-root") {
		t.Error("HasWildcard=false — a bare `*` in a default_scope'd role is still a lockout survivor")
	}
}

// ADR-047(b) exception #2, backcompat — a role that introduces NO scope leaves
// its bare permissions unrestricted. This is the majority of existing roles;
// the fix must not touch them.
func TestCheck_NoDefaultScope_BarePermissionStaysUnrestricted(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "ops", operators: []string{"archon-a"},
		permissions: []string{"incarnation.run", "operator.create"},
	})

	for _, ctx := range []map[string]string{nil, {}, {"coven": "web"}, {"coven": "dba", "service": "redis"}} {
		if err := e.Check("archon-a", "incarnation", "run", ctx); err != nil {
			t.Errorf("bare permission on an unscoped role denied with context %v: %v", ctx, err)
		}
	}
	if err := e.Check("archon-a", "operator", "create", nil); err != nil {
		t.Errorf("context-less op denied on an unscoped role: %v", err)
	}
}

// A per-permission `on <expr>` FULLY overrides the role's default_scope at the
// gate too — staging only, not a prod+staging merge, and not prod.
func TestCheck_PerPermScopeOverridesDefaultScope(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "prod-ops", operators: []string{"archon-a"},
		defaultScope: "coven=prod",
		permissions:  []string{"incarnation.run on coven=staging"},
	})

	if err := e.Check("archon-a", "incarnation", "run", map[string]string{"coven": "staging"}); err != nil {
		t.Errorf("per-perm coven=staging denied: %v", err)
	}
	if err := e.Check("archon-a", "incarnation", "run", map[string]string{"coven": "prod"}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("coven=prod = %v, want denied — the per-perm scope REPLACES default_scope, it does not merge", err)
	}
}

// A scoped `* on X` (NIM-128) carries its own predicate, so default_scope is
// moot for it: the boundary is X, whatever the role's default says.
func TestCheck_ScopedWildcard_KeepsItsOwnScope(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "web-super", operators: []string{"archon-a"},
		defaultScope: "coven=dba",
		permissions:  []string{"* on coven=web"},
	})

	if err := e.Check("archon-a", "incarnation", "run", map[string]string{"coven": "web"}); err != nil {
		t.Errorf("scoped `*` denied inside its own scope: %v", err)
	}
	if err := e.Check("archon-a", "incarnation", "run", map[string]string{"coven": "dba"}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("coven=dba = %v, want denied — a scoped `*` is bounded by ITS predicate, not the role default", err)
	}
	// A scoped `*` is not a cluster-admin and never was (NIM-128).
	if e.HasWildcard("archon-a") {
		t.Error("HasWildcard counted a SCOPED `*` — only a bare `*` is a lockout survivor")
	}
}

// Union across roles: an unrestricted role wins over a scoped one, the same way
// ResolvePurview reports Unrestricted. Narrowing one role must not narrow the
// operator.
func TestCheck_UnionAcrossRoles_UnrestrictedWins(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{
			name: "dba", operators: []string{"archon-a"},
			defaultScope: "coven=dba",
			permissions:  []string{"incarnation.run"},
		},
		fixtureRole{
			name: "free-ops", operators: []string{"archon-a"},
			permissions: []string{"incarnation.run"},
		},
	)

	for _, ctx := range []map[string]string{nil, {"coven": "web"}} {
		if err := e.Check("archon-a", "incarnation", "run", ctx); err != nil {
			t.Errorf("context %v denied: %v — the second, unscoped role grants this unconditionally", ctx, err)
		}
	}
}

// Union across roles, both scoped: the boundary is the OR of the two, and
// neither role's scope leaks onto the other's permissions.
func TestCheck_UnionAcrossRoles_BothScoped(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{
			name: "dba", operators: []string{"archon-a"},
			defaultScope: "coven=dba",
			permissions:  []string{"incarnation.run"},
		},
		fixtureRole{
			name: "web", operators: []string{"archon-a"},
			defaultScope: "coven=web",
			permissions:  []string{"incarnation.get"},
		},
	)

	if err := e.Check("archon-a", "incarnation", "run", map[string]string{"coven": "dba"}); err != nil {
		t.Errorf("incarnation.run in coven=dba denied: %v", err)
	}
	if err := e.Check("archon-a", "incarnation", "run", map[string]string{"coven": "web"}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("incarnation.run in coven=web = %v, want denied — the web role does not grant run", err)
	}
	if err := e.Check("archon-a", "incarnation", "get", map[string]string{"coven": "web"}); err != nil {
		t.Errorf("incarnation.get in coven=web denied: %v", err)
	}
	if err := e.Check("archon-a", "incarnation", "get", map[string]string{"coven": "dba"}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("incarnation.get in coven=dba = %v, want denied — the dba role does not grant get", err)
	}
}

// A derived role's EFFECTIVE default_scope is `parent's effective AND own delta`
// (ADR-078(b)), resolved once at snapshot build. The gate must read the resolved
// value, so the chain's ceiling binds a bare permission here exactly as it binds
// the purview — the NIM-179/NIM-180 attenuation reaching the write path.
func TestCheck_DerivedRole_ChainCeilingBindsBarePermission(t *testing.T) {
	e := mustEnforcer(t,
		fixtureRole{
			name: "dba", operators: []string{"archon-parent"},
			defaultScope: "coven=dba",
			permissions:  []string{"incarnation.run"},
		},
		fixtureRole{
			name: "dba-redis", operators: []string{"archon-child"}, parent: "dba",
			defaultScope: "service=redis",
			permissions:  []string{"incarnation.run"},
		},
	)

	// The child is confined to the conjunction; one half of it is not enough.
	if err := e.Check("archon-child", "incarnation", "run",
		map[string]string{"coven": "dba", "service": "redis"}); err != nil {
		t.Errorf("child denied inside `coven=dba AND service=redis`: %v", err)
	}
	for _, ctx := range []map[string]string{
		{"coven": "dba"},
		{"service": "redis"},
		{"coven": "web", "service": "redis"},
		{"coven": "dba", "service": "vault"},
	} {
		if err := e.Check("archon-child", "incarnation", "run", ctx); !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("child allowed with context %v (= %v), want denied — the chain ceiling is a conjunction", ctx, err)
		}
	}

	// The parent keeps its own, wider boundary: attenuation runs downwards only.
	if err := e.Check("archon-parent", "incarnation", "run", map[string]string{"coven": "dba"}); err != nil {
		t.Errorf("parent denied inside its own scope: %v", err)
	}
}

// The two spellings of one role are now the same role. `default_scope: X` with a
// bare permission and no default_scope with `<perm> on X` say the same thing
// (ADR-047(a): the role sets a base, a permission may override it), and after
// NIM-219 they DECIDE the same on both paths.
//
// This is also the measure of what the fix changed: every request the scoped
// role is newly refused, the per-perm spelling of the same role was already
// refused before. The fix removes a discrepancy between two ways of writing one
// thing — it does not invent a new denial.
func TestCheck_DefaultScope_EquivalentToPerPermSpelling(t *testing.T) {
	viaDefault := mustEnforcer(t, fixtureRole{
		name: "dba", operators: []string{"archon-a"},
		defaultScope: "coven=dba",
		permissions:  []string{"incarnation.run", "operator.create"},
	})
	viaPerPerm := mustEnforcer(t, fixtureRole{
		name: "dba", operators: []string{"archon-a"},
		permissions: []string{"incarnation.run on coven=dba", "operator.create on coven=dba"},
	})

	pairs := [][2]string{{"incarnation", "run"}, {"operator", "create"}, {"incarnation", "get"}}
	contexts := []map[string]string{nil, {}, {"coven": "dba"}, {"coven": "web"}, {"service": "redis"}}

	for _, pair := range pairs {
		for _, ctx := range contexts {
			a := viaDefault.Check("archon-a", pair[0], pair[1], ctx) == nil
			b := viaPerPerm.Check("archon-a", pair[0], pair[1], ctx) == nil
			if a != b {
				t.Errorf("%s.%s ctx=%v: default_scope allowed=%v, per-perm allowed=%v — "+
					"the same role written two ways must decide the same", pair[0], pair[1], ctx, a, b)
			}
			pa := viaDefault.ResolvePurview("archon-a", pair[0], pair[1])
			pb := viaPerPerm.ResolvePurview("archon-a", pair[0], pair[1])
			if pa.Unrestricted != pb.Unrestricted || len(pa.Exprs) != len(pb.Exprs) {
				t.Errorf("%s.%s: purview differs between the two spellings: %+v vs %+v", pair[0], pair[1], pa, pb)
			}
		}
	}
}

// The anti-drift guard, and the reason NIM-219 existed at all: the gate and the
// resolver must answer the SAME question for a flat request context.
//
//	Check(aid, r, a, ctx) == nil  ⟺  ResolvePurview(aid, r, a).Match(scopeInputFromContext(ctx))
//
// Both sides read [effectiveScope], so this holds by construction — the test is
// here to fail the moment someone re-introduces a second reading of a role's
// scope on one path only. It ranges over every branch of the rule: bare `*`,
// scoped `*`, bare permission with and without default_scope, per-perm override,
// a derived chain, a resource wildcard, revoked, and an unknown AID.
func TestCheck_AgreesWithResolvePurview(t *testing.T) {
	snap := snapshotOf(
		fixtureRole{name: "admin", operators: []string{"archon-root"}, defaultScope: "coven=dba", permissions: []string{"*"}},
		fixtureRole{name: "web-super", operators: []string{"archon-super"}, defaultScope: "coven=dba", permissions: []string{"* on coven=web"}},
		fixtureRole{name: "dba", operators: []string{"archon-dba", "archon-multi"}, defaultScope: "coven=dba", permissions: []string{"incarnation.run", "incarnation.get"}},
		fixtureRole{name: "free", operators: []string{"archon-free"}, permissions: []string{"incarnation.run"}},
		fixtureRole{name: "override", operators: []string{"archon-ovr"}, defaultScope: "coven=prod", permissions: []string{"incarnation.run on coven=staging"}},
		fixtureRole{name: "star-action", operators: []string{"archon-star"}, defaultScope: "service=redis", permissions: []string{"incarnation.*"}},
		fixtureRole{name: "web", operators: []string{"archon-multi"}, defaultScope: "coven=web", permissions: []string{"incarnation.get"}},
		fixtureRole{name: "dba-redis", operators: []string{"archon-derived"}, parent: "dba", defaultScope: "service=redis", permissions: []string{"incarnation.run"}},
		fixtureRole{name: "revoked-role", operators: []string{"archon-gone"}, permissions: []string{"incarnation.run"}},
	)
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	e.revoked = map[string]time.Time{"archon-gone": time.Unix(0, 0).UTC()}

	aids := []string{
		"archon-root", "archon-super", "archon-dba", "archon-free", "archon-ovr",
		"archon-star", "archon-multi", "archon-derived", "archon-gone", "archon-ghost",
	}
	pairs := [][2]string{
		{"incarnation", "run"}, {"incarnation", "get"}, {"incarnation", "destroy"}, {"operator", "create"},
	}
	contexts := []map[string]string{
		nil,
		{},
		{"coven": "dba"},
		{"coven": "web"},
		{"coven": "staging"},
		{"coven": "prod"},
		{"service": "redis"},
		{"coven": "dba", "service": "redis"},
		{"coven": "web", "service": "vault"},
	}

	for _, aid := range aids {
		for _, pair := range pairs {
			purview := e.ResolvePurview(aid, pair[0], pair[1])
			for _, ctx := range contexts {
				gate := e.Check(aid, pair[0], pair[1], ctx) == nil
				resolved := purview.Match(scopeInputFromContext(ctx))
				if gate != resolved {
					t.Errorf("aid=%s %s.%s ctx=%v: Check allowed=%v, Purview.Match=%v — "+
						"the gate and the resolver read a role's scope differently (NIM-219)",
						aid, pair[0], pair[1], ctx, gate, resolved)
				}
			}
		}
	}
}
