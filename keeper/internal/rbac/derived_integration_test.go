//go:build integration

// Guard matrix for derived roles (ADR-078, NIM-179) against a real DB: the
// parent-graph guards (self-parent / cycle / depth), the fail-closed orphan
// policy on role.delete, and the fact that a stored parent is carried into the
// snapshot and the catalog view.
//
// These run against the DB rather than a hand-built snapshot on purpose: the
// authoritative gate is the schema (a CHECK, a self-FK ON DELETE RESTRICT and the
// rbac_roles_parent_chain_guard trigger), so it must be exercised through SQL —
// a Go-level test would prove nothing about a write arriving by any other path.
//
// Shares the container / TestMain / resetRBAC / seedOperator / insertRole /
// newService with integration_test.go and crud_integration_test.go.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -race -count=1 ./internal/rbac/

package rbac

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// setParent points a role at a parent through raw SQL — deliberately bypassing
// any Go-side validation, so what is under test is the schema guard itself.
func setParent(name, parent string) error {
	_, err := integrationPool.Exec(context.Background(),
		`UPDATE rbac_roles SET parent_role = $2 WHERE name = $1`, name, parent)
	return err
}

// insertDerivedRole inserts a role that already names a parent (the INSERT path
// of the guard, as opposed to setParent's UPDATE path).
func insertDerivedRole(name, parent string) error {
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO rbac_roles (name, builtin, parent_role) VALUES ($1, false, $2)`, name, parent)
	return err
}

// assertSQLState fails unless err is a PgError carrying the given SQLSTATE.
func assertSQLState(t *testing.T, err error, want, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a %s error, got nil (the guard did not fire)", what, want)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: expected a PgError %s, got %T: %v", what, want, err, err)
	}
	if pgErr.Code != want {
		t.Fatalf("%s: SQLSTATE = %s (%s), want %s", what, pgErr.Code, pgErr.Message, want)
	}
}

// TestIntegration_ParentRole_SelfParentRejected — a role cannot derive from
// itself. The degenerate cycle: no root, so no ceiling to attenuate against.
func TestIntegration_ParentRole_SelfParentRejected(t *testing.T) {
	resetRBAC(t)
	insertRole(t, "dba")

	assertSQLState(t, setParent("dba", "dba"), pgErrCodeRoleParentCycle, "self-parent")

	// The row must be untouched — a rejected write leaves a plain role.
	var parent *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT parent_role FROM rbac_roles WHERE name = 'dba'`).Scan(&parent); err != nil {
		t.Fatalf("read back dba: %v", err)
	}
	if parent != nil {
		t.Errorf("dba.parent_role = %q after a rejected write, want NULL", *parent)
	}
}

// TestIntegration_ParentRole_CycleRejected — closing a chain on itself is
// refused, on the INSERT path and on the UPDATE path alike.
func TestIntegration_ParentRole_CycleRejected(t *testing.T) {
	resetRBAC(t)
	insertRole(t, "a")
	if err := insertDerivedRole("b", "a"); err != nil {
		t.Fatalf("insert b derived from a: %v", err)
	}

	assertSQLState(t, setParent("a", "b"), pgErrCodeRoleParentCycle, "two-role cycle a->b->a")

	if err := insertDerivedRole("c", "b"); err != nil {
		t.Fatalf("insert c derived from b: %v", err)
	}
	assertSQLState(t, setParent("a", "c"), pgErrCodeRoleParentCycle, "three-role cycle a->b->c->a")
}

// TestIntegration_ParentRole_DepthCapEnforced — a chain of exactly
// maxRoleChainDepth roles is legal; one more is refused. Re-parenting is checked
// from BOTH ends: moving a subtree under a deep parent must count the roles that
// already hang below the row being moved, not just the ancestors above it.
func TestIntegration_ParentRole_DepthCapEnforced(t *testing.T) {
	resetRBAC(t)
	insertRole(t, "r1")
	for _, step := range [][2]string{{"r2", "r1"}, {"r3", "r2"}, {"r4", "r3"}} {
		if err := insertDerivedRole(step[0], step[1]); err != nil {
			t.Fatalf("insert %s derived from %s (within the cap): %v", step[0], step[1], err)
		}
	}

	assertSQLState(t, insertDerivedRole("r5", "r4"), pgErrCodeRoleChainTooDeep,
		"a chain one role past the cap")

	// A 3-role subtree x -> y -> z, re-parented under r3 (itself 3 roles deep):
	// upward-only counting would see r3's depth and wave it through.
	insertRole(t, "x")
	if err := insertDerivedRole("y", "x"); err != nil {
		t.Fatalf("insert y derived from x: %v", err)
	}
	if err := insertDerivedRole("z", "y"); err != nil {
		t.Fatalf("insert z derived from y: %v", err)
	}
	assertSQLState(t, setParent("x", "r3"), pgErrCodeRoleChainTooDeep,
		"re-parenting a subtree past the cap")
}

// TestIntegration_ParentRole_UnknownParentRejected — a parent outside the
// catalog is refused by the self-FK, so a child whose ceiling cannot be resolved
// never reaches storage.
func TestIntegration_ParentRole_UnknownParentRejected(t *testing.T) {
	resetRBAC(t)
	insertRole(t, "orphan-to-be")

	err := setParent("orphan-to-be", "no-such-role")
	assertSQLState(t, err, pgErrCodeForeignKeyViolation, "unknown parent")
}

// TestIntegration_ParentRole_DeleteParentFailsClosed — the orphan policy. Deleting
// a role that is still someone's parent is REFUSED ([ErrRoleHasChildren]); the
// operator must re-parent or delete the children explicitly. The alternatives all
// change a child's ceiling implicitly, and clearing the parent would WIDEN it (the
// child's default_scope stops being a delta and becomes absolute) — an escalation.
func TestIntegration_ParentRole_DeleteParentFailsClosed(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-root", nil)
	seedClusterAdmin(t, "archon-root")
	insertRole(t, "dba", "incarnation.get")
	if err := insertDerivedRole("dba-aboba", "dba"); err != nil {
		t.Fatalf("insert the derived role: %v", err)
	}

	svc := newService(t)
	ctx := context.Background()

	if err := svc.DeleteRole(ctx, "dba"); !errors.Is(err, ErrRoleHasChildren) {
		t.Fatalf("DeleteRole(parent with children) = %v, want ErrRoleHasChildren", err)
	}

	// The parent must still be there: a refused delete is a no-op, not a partial one.
	var alive bool
	if err := integrationPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM rbac_roles WHERE name = 'dba')`).Scan(&alive); err != nil {
		t.Fatalf("check the parent survived: %v", err)
	}
	if !alive {
		t.Fatal("the parent role was deleted despite the RESTRICT policy")
	}

	// Once the child is gone the parent deletes normally — RESTRICT blocks the
	// orphaning case only, it does not make a role permanently undeletable.
	if err := svc.DeleteRole(ctx, "dba-aboba"); err != nil {
		t.Fatalf("DeleteRole(child): %v", err)
	}
	if err := svc.DeleteRole(ctx, "dba"); err != nil {
		t.Fatalf("DeleteRole(parent, now childless): %v", err)
	}
}

// TestIntegration_ParentRole_CarriedIntoSnapshotAndView — parent_role reaches
// both read paths: the enforcer snapshot (Role.ParentRole) and the API catalog
// (RoleView.ParentRole). Plain roles stay empty on both.
func TestIntegration_ParentRole_CarriedIntoSnapshotAndView(t *testing.T) {
	resetRBAC(t)
	insertRole(t, "dba", "incarnation.get")
	if err := insertDerivedRole("dba-aboba", "dba"); err != nil {
		t.Fatalf("insert the derived role: %v", err)
	}
	ctx := context.Background()

	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got := snap.RoleParents["dba-aboba"]; got != "dba" {
		t.Errorf("Snapshot.RoleParents[dba-aboba] = %q, want %q", got, "dba")
	}
	if _, ok := snap.RoleParents["dba"]; ok {
		t.Error("a plain role must be absent from Snapshot.RoleParents")
	}

	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	for _, r := range e.roles {
		switch r.Name {
		case "dba-aboba":
			if r.ParentRole != "dba" {
				t.Errorf("Role(dba-aboba).ParentRole = %q, want %q", r.ParentRole, "dba")
			}
		case "dba":
			if r.ParentRole != "" {
				t.Errorf("Role(dba).ParentRole = %q, want empty", r.ParentRole)
			}
		}
	}

	views, err := LoadRoleViews(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadRoleViews: %v", err)
	}
	seen := false
	for _, v := range views {
		if v.Name != "dba-aboba" {
			continue
		}
		seen = true
		if v.ParentRole != "dba" {
			t.Errorf("RoleView(dba-aboba).ParentRole = %q, want %q", v.ParentRole, "dba")
		}
	}
	if !seen {
		t.Error("the derived role is missing from LoadRoleViews")
	}
}

// TestIntegration_ParentRole_MultipleRolesUnion — an operator holding SEVERAL
// roles. Derivation narrows a ROLE, not an OPERATOR: effective rights stay the
// union across the operator's roles, and a derived role joins that union like any
// other one.
//
// The counter-intuitive half is the second case, and it is the reason this test
// exists: granting BOTH the wide parent and the narrow child leaves the operator
// with the parent's full rights — the narrow role adds nothing and restricts
// nothing. Confining an operator means revoking the parent from them, not stacking
// a derived role on top.
//
// Both assertions are PERMANENT invariants, not J1 artifacts: NIM-180 resolves the
// chain, but a child holding a subset of its parent's permissions (variant B) still
// never gains `incarnation.run`, and the union still returns the parent's rights to
// someone who holds the parent.
func TestIntegration_ParentRole_MultipleRolesUnion(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-narrow", nil)
	root := "archon-narrow"
	seedOperator(t, "archon-both", &root)

	insertRole(t, "dba", "incarnation.get", "incarnation.run")
	if err := insertDerivedRole("dba-aboba", "dba"); err != nil {
		t.Fatalf("insert the derived role: %v", err)
	}
	ctx := context.Background()
	// The child keeps a SUBSET of the parent's permissions (variant B): get, not run.
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('dba-aboba', 'incarnation.get')`,
	); err != nil {
		t.Fatalf("seed the child permission: %v", err)
	}
	for _, m := range [][2]string{
		{"dba-aboba", "archon-narrow"},
		{"dba-aboba", "archon-both"},
		{"dba", "archon-both"},
	} {
		if err := GrantOperator(ctx, integrationPool, m[0], m[1], nil); err != nil {
			t.Fatalf("bind %s to %s: %v", m[1], m[0], err)
		}
	}

	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}

	// Only the derived role: holds what the child holds, and nothing the child
	// deliberately left behind.
	if err := e.Check("archon-narrow", "incarnation", "get", nil); err != nil {
		t.Errorf("derived-role holder cannot incarnation.get: %v", err)
	}
	if err := e.Check("archon-narrow", "incarnation", "run", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("derived-role holder got incarnation.run = %v, want denied (the child dropped it)", err)
	}

	// Parent AND child: the union gives the parent's full rights. Adding the narrow
	// role restricts nothing.
	if err := e.Check("archon-both", "incarnation", "run", nil); err != nil {
		t.Errorf("holder of both roles lost incarnation.run: %v — the union must still grant the parent's rights", err)
	}
}

// TestIntegration_ParentRole_InheritsNothingImplicitly — a derived role with no
// rows of its own grants NOTHING, however much its parent holds.
//
// This began life as the J1 boundary marker ("the chain is not resolved yet") and
// survives NIM-180 with the same assertion and a different reason, which is
// exactly why it was inverted rather than deleted. Resolution
// ([flattenRoleGraph]) INTERSECTS the child's own rows with the parent's
// effective set — it does not COPY them down. An empty intersection is empty.
//
// The distinction is the anti-escalation core of ADR-078(c): were inheritance a
// copy, a permission added to a parent would silently appear on every descendant —
// a widening cascade. Only narrowing cascades. The sibling case, where the child
// DOES hold rows and follows its parent's scope, is
// TestIntegration_Derived_CascadeThroughTheSnapshot.
func TestIntegration_ParentRole_InheritsNothingImplicitly(t *testing.T) {
	resetRBAC(t)
	seedOperator(t, "archon-child", nil)
	insertRole(t, "dba", "incarnation.get")
	if err := insertDerivedRole("dba-aboba", "dba"); err != nil {
		t.Fatalf("insert the derived role: %v", err)
	}
	ctx := context.Background()
	if err := GrantOperator(ctx, integrationPool, "dba-aboba", "archon-child", nil); err != nil {
		t.Fatalf("bind the archon to the derived role: %v", err)
	}

	snap, err := LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}

	if err := e.Check("archon-child", "incarnation", "get", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Check via a derived role with no own rows = %v, want ErrPermissionDenied "+
			"(inheritance intersects, it does not copy the parent's permissions down)", err)
	}
}
