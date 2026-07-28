//go:build integration

// Guard matrix for the derived-role cascade report and the scope-mode intent
// (ADR-078(k)/(l), NIM-199 / NIM-200) against a real DB: what a parent's mutation
// does to the roles below it, whether the operator is told, and what `pin` freezes.
//
// Against the DB because the whole subject is a subtree that only exists there:
// the report is computed from live rows inside the mutation's transaction, and the
// scope_mode column is guarded by a CHECK that Go cannot restate.
//
// Shares the container / TestMain / resetRBAC / seedOperator / insertRole /
// insertRoleScoped / insertDerived / newService / scopeOf with integration_test.go
// and attenuate_integration_test.go.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -race -count=1 ./internal/rbac/

package rbac

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// setupParentWithChild is the shape the ticket is about: a parent scoped to one
// coven and a derived role that adds a narrowing of its own, held by an operator.
// alice is cluster-admin, so every mutation below is refused (or not) on the
// cascade rule alone rather than on least-privilege.
func setupParentWithChild(t *testing.T) (admin string) {
	t.Helper()
	ctx := context.Background()
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	a := "archon-alice"
	seedOperator(t, "archon-bob", &a)

	insertRoleScoped(t, "dba", "coven=dba", "incarnation.get", "incarnation.run")
	insertDerived(t, "dba-probe", "dba", "trait.project=probe", "incarnation.get")
	if err := GrantOperator(ctx, integrationPool, "dba-probe", "archon-bob", &a); err != nil {
		t.Fatalf("grant bob→dba-probe: %v", err)
	}
	return a
}

// updateParent replays the mutation the ticket describes — the parent's scope
// being edited — with confirmation on or off.
func updateParent(t *testing.T, admin, scope string, confirm bool) error {
	t.Helper()
	return newService(t).UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:            "dba",
		Permissions:     []string{"incarnation.get", "incarnation.run"},
		CallerAID:       admin,
		SetDefaultScope: true,
		DefaultScope:    &scope,
		ConfirmCascade:  confirm,
	})
}

// ---- the blast radius is reported, not silent (NIM-199) ----

// TestIntegration_Cascade_WideningTheParentIsRefusedUnconfirmed is the headline
// scenario: `coven=dba` becomes `coven in (dba, web)` and every derived role
// silently gains web. Refused until the operator says they meant it — and the
// refusal has to carry what they need in order to decide.
func TestIntegration_Cascade_WideningTheParentIsRefusedUnconfirmed(t *testing.T) {
	admin := setupParentWithChild(t)

	err := updateParent(t, admin, "coven in (dba, web)", false)
	if !errors.Is(err, ErrRoleCascadeNeedsConfirm) {
		t.Fatalf("err = %v, want ErrRoleCascadeNeedsConfirm", err)
	}
	var ce *CascadeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a *CascadeError carrying the report", err)
	}
	if got := ce.Cascade.Widened; len(got) != 1 || got[0] != "dba-probe" {
		t.Errorf("widened = %v, want [dba-probe]", got)
	}
	if len(ce.Cascade.Narrowed) != 0 {
		t.Errorf("narrowed = %v, want none — the parent only gained a coven", ce.Cascade.Narrowed)
	}
	if ce.Cascade.Operators != 1 {
		t.Errorf("operators = %d, want 1 (bob holds dba-probe)", ce.Cascade.Operators)
	}
	// The stored scope is untouched: a refusal is a refusal, not a partial write.
	if got := scopeOf(t, "dba"); got == nil || *got != "coven=dba" {
		t.Errorf("parent default_scope = %v, want the original coven=dba", got)
	}
}

// TestIntegration_Cascade_NarrowingIsReportedToo — the user's requirement in the
// other direction: removing access from derived roles is just as much a surprise
// as adding it, so it is reported on the same terms and names the same numbers.
func TestIntegration_Cascade_NarrowingIsReportedToo(t *testing.T) {
	admin := setupParentWithChild(t)

	err := updateParent(t, admin, "coven=dba AND trait.tier=gold", false)
	if !errors.Is(err, ErrRoleCascadeNeedsConfirm) {
		t.Fatalf("err = %v, want ErrRoleCascadeNeedsConfirm", err)
	}
	var ce *CascadeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a *CascadeError", err)
	}
	if got := ce.Cascade.Narrowed; len(got) != 1 || got[0] != "dba-probe" {
		t.Errorf("narrowed = %v, want [dba-probe]", got)
	}
	if ce.Cascade.Operators != 1 {
		t.Errorf("operators = %d, want 1", ce.Cascade.Operators)
	}
	if msg := err.Error(); !strings.Contains(msg, "dba-probe") || !strings.Contains(msg, "1 operator") {
		t.Errorf("message %q must name the affected role and the operator count", msg)
	}
}

// TestIntegration_Cascade_ConfirmedProceeds — the gate is a question, not a wall.
// The same mutation with the confirmation goes through and actually moves.
func TestIntegration_Cascade_ConfirmedProceeds(t *testing.T) {
	admin := setupParentWithChild(t)

	if err := updateParent(t, admin, "coven in (dba, web)", true); err != nil {
		t.Fatalf("confirmed update: %v", err)
	}
	if got := scopeOf(t, "dba"); got == nil || *got != "coven in (dba, web)" {
		t.Fatalf("parent default_scope = %v, want the widened one", got)
	}
}

// TestIntegration_Cascade_UnaffectedChildrenAskNothing — an edit that leaves the
// subtree's rights exactly as they were passes silently. The report is about
// consequences; being a parent is not itself a consequence.
func TestIntegration_Cascade_UnaffectedChildrenAskNothing(t *testing.T) {
	admin := setupParentWithChild(t)

	// A description-free permission REORDER: the same set, written the other way
	// round. Nothing below moves.
	if err := newService(t).UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:        "dba",
		Permissions: []string{"incarnation.run", "incarnation.get"},
		CallerAID:   admin,
	}); err != nil {
		t.Fatalf("no-op-for-the-subtree update: %v", err)
	}
}

// TestIntegration_Cascade_ChildlessRoleIsNeverGated — the common case. A role with
// no derived roles cannot cascade anywhere, so the widest possible edit is free of
// the question.
func TestIntegration_Cascade_ChildlessRoleIsNeverGated(t *testing.T) {
	admin := setupParentWithChild(t)
	insertRoleScoped(t, "solo", "coven=dba", "incarnation.get")

	scope := "coven in (dba, web, prod)"
	if err := newService(t).UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:            "solo",
		Permissions:     []string{"incarnation.get"},
		CallerAID:       admin,
		SetDefaultScope: true,
		DefaultScope:    &scope,
	}); err != nil {
		t.Fatalf("widening a childless role must not need confirmation: %v", err)
	}
}

// TestIntegration_Cascade_ReachesGrandchildren — the report walks the whole
// subtree, not just the first level. A grandchild is exactly as surprised as a
// child, and the depth cap keeps the walk bounded.
func TestIntegration_Cascade_ReachesGrandchildren(t *testing.T) {
	admin := setupParentWithChild(t)
	insertDerived(t, "dba-probe-eu", "dba-probe", "trait.region=eu", "incarnation.get")

	err := updateParent(t, admin, "coven in (dba, web)", false)
	var ce *CascadeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a *CascadeError", err)
	}
	if got := ce.Cascade.Widened; len(got) != 2 || got[0] != "dba-probe" || got[1] != "dba-probe-eu" {
		t.Errorf("widened = %v, want [dba-probe dba-probe-eu]", got)
	}
}

// ---- scope_mode: the intent is stored, and pin actually holds (NIM-199) ----

// TestIntegration_ScopeMode_DefaultsToTrack — a derived role created without a
// mode is a TRACKING role, which is the ADR-078(b) contract and the only default
// that makes the cascade mean anything.
func TestIntegration_ScopeMode_DefaultsToTrack(t *testing.T) {
	admin := setupParentWithChild(t)

	if err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name: "dba-plain", Permissions: []string{"incarnation.get"},
		DefaultScope: ptr("trait.project=x"), ParentRole: ptr("dba"), CallerAID: admin,
	}); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if got := modeOf(t, "dba-plain"); got != ScopeModeTrack {
		t.Errorf("scope_mode = %q, want track", got)
	}
	if got := scopeOf(t, "dba-plain"); got == nil || *got != "trait.project=x" {
		t.Errorf("default_scope = %v, want the bare delta", got)
	}
}

// TestIntegration_ScopeMode_PinMaterializesTheParentScope — `pin` is a write-time
// act: the parent's scope lands IN the delta, which is what makes the role stop
// following it. Checking the stored string rather than the resolved one is the
// point — a pin that only lived in the resolver would be a convention again.
func TestIntegration_ScopeMode_PinMaterializesTheParentScope(t *testing.T) {
	admin := setupParentWithChild(t)

	if err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name: "dba-pinned", Permissions: []string{"incarnation.get"},
		DefaultScope: ptr("trait.project=x"), ParentRole: ptr("dba"),
		ScopeMode: ScopeModePin, CallerAID: admin,
	}); err != nil {
		t.Fatalf("CreateRole(pin): %v", err)
	}
	if got := modeOf(t, "dba-pinned"); got != ScopeModePin {
		t.Errorf("scope_mode = %q, want pin", got)
	}
	if got := scopeOf(t, "dba-pinned"); got == nil || *got != "coven=dba AND trait.project=x" {
		t.Fatalf("default_scope = %v, want the parent's scope conjoined into the delta", got)
	}
}

// TestIntegration_ScopeMode_PinnedChildDoesNotFollowAWidening — the behaviour the
// whole column exists for, observed where it matters: at the decision layer, after
// the parent has actually moved.
func TestIntegration_ScopeMode_PinnedChildDoesNotFollowAWidening(t *testing.T) {
	ctx := context.Background()
	admin := setupParentWithChild(t)
	s := newService(t)

	if err := s.CreateRole(ctx, CreateRoleInput{
		Name: "dba-pinned", Permissions: []string{"incarnation.get"},
		ParentRole: ptr("dba"), ScopeMode: ScopeModePin, CallerAID: admin,
	}); err != nil {
		t.Fatalf("CreateRole(pin): %v", err)
	}
	// A DEDICATED holder. Bob already has the tracking child, and an operator's
	// rights are the UNION over their roles (ADR-078(j)) — asking about bob would
	// measure the tracking role, not the pin.
	seedOperator(t, "archon-carol", &admin)
	if err := GrantOperator(ctx, integrationPool, "dba-pinned", "archon-carol", &admin); err != nil {
		t.Fatalf("grant carol→dba-pinned: %v", err)
	}
	// The pinned child is not in the report — it cannot gain anything.
	if err := updateParent(t, admin, "coven in (dba, web)", false); err != nil {
		var ce *CascadeError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want a *CascadeError", err)
		}
		for _, name := range ce.Cascade.Widened {
			if name == "dba-pinned" {
				t.Fatal("a PINNED child was reported as widening — the pin did not hold")
			}
		}
		if err := updateParent(t, admin, "coven in (dba, web)", true); err != nil {
			t.Fatalf("confirmed update: %v", err)
		}
	}

	// And the decision layer agrees: carol reaches dba through the pin, never web.
	scope := purviewScope(t, "archon-carol")
	if EvalScope(scope, ScopeInput{Covens: []string{"web"}}) {
		t.Errorf("purview %q admits the NEW coven — the pin did not hold", scope)
	}
	if !EvalScope(scope, ScopeInput{Covens: []string{"dba"}}) {
		t.Errorf("purview %q no longer admits the coven it was pinned to", scope)
	}
}

// TestIntegration_ScopeMode_PinnedChildStillNarrows — the safety half, and the
// reason `pin` materializes instead of freezing: narrowing must ALWAYS cascade, or
// a pin would be an escape from attenuation rather than a defence against drift.
func TestIntegration_ScopeMode_PinnedChildStillNarrows(t *testing.T) {
	ctx := context.Background()
	admin := setupParentWithChild(t)

	if err := newService(t).CreateRole(ctx, CreateRoleInput{
		Name: "dba-pinned", Permissions: []string{"incarnation.get"},
		ParentRole: ptr("dba"), ScopeMode: ScopeModePin, CallerAID: admin,
	}); err != nil {
		t.Fatalf("CreateRole(pin): %v", err)
	}
	// A dedicated holder, for the same union reason as above (ADR-078(j)).
	seedOperator(t, "archon-carol", &admin)
	if err := GrantOperator(ctx, integrationPool, "dba-pinned", "archon-carol", &admin); err != nil {
		t.Fatalf("grant carol→dba-pinned: %v", err)
	}
	if err := updateParent(t, admin, "coven=dba AND trait.tier=gold", true); err != nil {
		t.Fatalf("confirmed narrowing: %v", err)
	}

	scope := purviewScope(t, "archon-carol")
	if EvalScope(scope, ScopeInput{Covens: []string{"dba"}}) {
		t.Errorf("purview %q still admits plain coven=dba — a PINNED child ignored its parent's narrowing", scope)
	}
	if !EvalScope(scope, ScopeInput{Covens: []string{"dba"}, Traits: map[string][]string{"tier": {"gold"}}}) {
		t.Errorf("purview %q dropped the area the parent still allows", scope)
	}
}

// TestIntegration_ScopeMode_RejectedOnAPlainRole — a mode without a parent
// describes a relationship that does not exist. Refused in Go and, underneath, by
// the migration's biconditional CHECK.
func TestIntegration_ScopeMode_RejectedOnAPlainRole(t *testing.T) {
	admin := setupParentWithChild(t)

	err := newService(t).CreateRole(context.Background(), CreateRoleInput{
		Name: "flat", Permissions: []string{"incarnation.get"},
		ScopeMode: ScopeModePin, CallerAID: admin,
	})
	if !errors.Is(err, ErrInvalidScopeMode) {
		t.Fatalf("err = %v, want ErrInvalidScopeMode", err)
	}
	assertRoleAbsent(t, "flat")
}

// TestIntegration_ScopeMode_ClearedWithTheParent — making a role plain again drops
// the intent with the relationship it described, keeping the two columns from
// drifting into a state the CHECK forbids.
func TestIntegration_ScopeMode_ClearedWithTheParent(t *testing.T) {
	admin := setupParentWithChild(t)

	if err := newService(t).UpdateRolePermissions(context.Background(), UpdateRolePermissionsInput{
		Name:          "dba-probe",
		Permissions:   []string{"incarnation.get"},
		CallerAID:     admin,
		SetParentRole: true,
		ParentRole:    nil,
	}); err != nil {
		t.Fatalf("re-root to plain: %v", err)
	}
	if got := modeOf(t, "dba-probe"); got != ScopeModeNone {
		t.Errorf("scope_mode = %q, want NULL once the role is plain", got)
	}
}

// TestIntegration_ScopeMode_UnrelatedEditDoesNotRepin — a pinned role whose
// permissions are edited must NOT re-freeze onto today's parent: that would make an
// ordinary PATCH an authorization change nobody requested.
func TestIntegration_ScopeMode_UnrelatedEditDoesNotRepin(t *testing.T) {
	ctx := context.Background()
	admin := setupParentWithChild(t)
	s := newService(t)

	if err := s.CreateRole(ctx, CreateRoleInput{
		Name: "dba-pinned", Permissions: []string{"incarnation.get", "incarnation.run"},
		ParentRole: ptr("dba"), ScopeMode: ScopeModePin, CallerAID: admin,
	}); err != nil {
		t.Fatalf("CreateRole(pin): %v", err)
	}
	before := scopeOf(t, "dba-pinned")

	if err := s.UpdateRolePermissions(ctx, UpdateRolePermissionsInput{
		Name: "dba-pinned", Permissions: []string{"incarnation.get"}, CallerAID: admin,
	}); err != nil {
		t.Fatalf("trim the pinned role's permissions: %v", err)
	}
	after := scopeOf(t, "dba-pinned")
	if before == nil || after == nil || *before != *after {
		t.Errorf("delta moved from %v to %v on an edit that never mentioned the scope", before, after)
	}
}

// ---- inert rows are visible (NIM-200) ----

// TestIntegration_Inert_NarrowedParentLeavesTheRowVisible is the NIM-200 report: a
// child stores `incarnation.*`, the parent narrows to `incarnation.get`, and the
// child's effective set goes EMPTY rather than shrinking to `get`.
//
// The emptiness is correct and deliberate — clipping `*` down to the parent's set
// would mean a permission later added to the parent silently appears on the child,
// the widening cascade ADR-078(c) exists to prevent. What was wrong is that it was
// invisible: the catalog listed a permission and reported no rights, with nothing
// connecting the two.
func TestIntegration_Inert_NarrowedParentLeavesTheRowVisible(t *testing.T) {
	ctx := context.Background()
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRoleScoped(t, "dba", "coven=dba", "incarnation.get")
	insertDerived(t, "dba-wide", "dba", "", "incarnation.*")

	views, err := LoadRoleViews(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadRoleViews: %v", err)
	}
	var child *RoleView
	for i := range views {
		if views[i].Name == "dba-wide" {
			child = &views[i]
		}
	}
	if child == nil {
		t.Fatal("dba-wide missing from the catalog")
	}
	if len(child.Permissions) != 1 || child.Permissions[0] != "incarnation.*" {
		t.Fatalf("permissions = %v, want the row as stored", child.Permissions)
	}
	if len(child.EffectivePermissions) != 0 {
		t.Fatalf("effective_permissions = %v, want empty — the parent does not cover `incarnation.*`",
			child.EffectivePermissions)
	}
	if len(child.InertPermissions) != 1 {
		t.Fatalf("inert_permissions = %v, want the uncovered row — a role that grants nothing must say so",
			child.InertPermissions)
	}
	if !strings.HasPrefix(child.InertPermissions[0], "incarnation.*") {
		t.Errorf("inert_permissions[0] = %q, want the resolved form of incarnation.*", child.InertPermissions[0])
	}
}

// TestIntegration_Inert_EmptyOnAHealthyRole — the field is a signal, so it has to
// be quiet when there is nothing wrong. A plain role and a fully-covered derived
// role both report nothing inert.
func TestIntegration_Inert_EmptyOnAHealthyRole(t *testing.T) {
	ctx := context.Background()
	setupParentWithChild(t)

	views, err := LoadRoleViews(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadRoleViews: %v", err)
	}
	for _, v := range views {
		if len(v.InertPermissions) != 0 {
			t.Errorf("role %q reports %v inert, want none", v.Name, v.InertPermissions)
		}
	}
}

// TestIntegration_Inert_RepairErrorNamesEveryUncoveredRow — the other half of
// NIM-200: once rows go inert, ANY later edit of the child is refused until they
// are dropped, and an operator told about one offending row at a time cannot
// repair the role in a single PATCH.
func TestIntegration_Inert_RepairErrorNamesEveryUncoveredRow(t *testing.T) {
	ctx := context.Background()
	resetRBAC(t)
	seedOperator(t, "archon-alice", nil)
	seedClusterAdmin(t, "archon-alice")
	insertRoleScoped(t, "dba", "coven=dba", "incarnation.get")
	insertDerived(t, "dba-wide", "dba", "", "incarnation.*")

	err := newService(t).UpdateRolePermissions(ctx, UpdateRolePermissionsInput{
		Name:        "dba-wide",
		Permissions: []string{"incarnation.*", "incarnation.destroy"},
		CallerAID:   "archon-alice",
	})
	if !errors.Is(err, ErrRoleExceedsParent) {
		t.Fatalf("err = %v, want ErrRoleExceedsParent", err)
	}
	for _, want := range []string{"incarnation.*", "incarnation.destroy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q must name every uncovered row, missing %q", err.Error(), want)
		}
	}

	// And the repair itself goes through: drop the rows the parent no longer covers.
	if err := newService(t).UpdateRolePermissions(ctx, UpdateRolePermissionsInput{
		Name: "dba-wide", Permissions: []string{"incarnation.get"}, CallerAID: "archon-alice",
	}); err != nil {
		t.Fatalf("repairing the role by dropping the inert rows: %v", err)
	}
}

// purviewScope resolves an operator's incarnation.get purview off a freshly built
// enforcer and returns its single predicate.
//
// The purview rather than [Enforcer.Check]: a role's narrowing lives in its
// default_scope, and Check reads a BARE permission as unrestricted by design
// (ADR-047 — scope is applied when the purview is resolved, which is the layer
// every scoped decision goes through). Asserting on Check would be asserting on
// the wrong half of the model.
func purviewScope(t *testing.T, aid string) *ScopeExpr {
	t.Helper()
	ctx := context.Background()
	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	p := e.ResolvePurview(aid, "incarnation", "get")
	if p.Unrestricted {
		t.Fatalf("purview of %q is UNRESTRICTED — the ceiling was lost", aid)
	}
	if len(p.Exprs) != 1 {
		t.Fatalf("purview of %q = %d predicates, want 1", aid, len(p.Exprs))
	}
	return p.Exprs[0]
}

// modeOf reads a role's stored scope_mode ([ScopeModeNone] = NULL).
func modeOf(t *testing.T, name string) ScopeMode {
	t.Helper()
	var mode *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT scope_mode FROM rbac_roles WHERE name = $1`, name).Scan(&mode); err != nil {
		t.Fatalf("read scope_mode of %q: %v", name, err)
	}
	if mode == nil {
		return ScopeModeNone
	}
	return ScopeMode(*mode)
}
