package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// newSynodHandler assembles a SynodHandler over a fake pool (shares rbacFakePool with
// the role tests). Covers the DOMAIN layer (*Typed: validation / sentinel→problem
// mapping); SQL-logic consistency is covered in rbac/synod_crud_integration_test.go.
// bind/decode/400 cases are covered by huma-integration (handler-native T5d).
func newSynodHandler(t *testing.T, pool *rbacFakePool) *SynodHandler {
	t.Helper()
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("rbac.NewService: %v", err)
	}
	return NewSynodHandler(svc, nil)
}

// --- Create ---

func TestSynodHandler_Create_201(t *testing.T) {
	h := newSynodHandler(t, &rbacFakePool{})
	desc := "ops"
	if _, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		SynodCreateInput{Name: "ops-team", Description: &desc}); err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
}

func TestSynodHandler_Create_EmptyName_422(t *testing.T) {
	h := newSynodHandler(t, &rbacFakePool{})
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"), SynodCreateInput{Name: ""})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestSynodHandler_Create_Duplicate_409(t *testing.T) {
	// UNIQUE-violation INSERT synods → ErrSynodAlreadyExists → 409.
	pool := &rbacFakePool{insertSynodErr: &pgconn.PgError{Code: "23505", ConstraintName: "synods_pkey"}}
	h := newSynodHandler(t, pool)
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"), SynodCreateInput{Name: "ops"})
	wantProblem(t, err, problem.TypeSynodExists)
}

func TestSynodHandler_Create_InvalidName_422(t *testing.T) {
	// An invalid name is rejected by the Service BEFORE tx (ErrInvalidSynodName).
	h := newSynodHandler(t, &rbacFakePool{})
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"), SynodCreateInput{Name: "Bad_Name"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

// Guard tests for the description cap on CREATE (parity with Update). Boundary pair:
//   - exactly MaxLen → 201 (NOT 422): catches `>=` instead of `>` and off-by-one;
//   - MaxLen+1 → 422 validation-failed: catches the cap being dropped altogether.
func TestSynodHandler_Create_DescriptionAtLimit_201(t *testing.T) {
	long := strings.Repeat("a", rbac.SynodDescriptionMaxLen)
	h := newSynodHandler(t, &rbacFakePool{})
	if _, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		SynodCreateInput{Name: "ops-team", Description: &long}); err != nil {
		t.Fatalf("CreateTyped (description at MaxLen must pass): %v", err)
	}
}

func TestSynodHandler_Create_TooLongDescription_422(t *testing.T) {
	long := strings.Repeat("a", rbac.SynodDescriptionMaxLen+1)
	h := newSynodHandler(t, &rbacFakePool{})
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		SynodCreateInput{Name: "ops-team", Description: &long})
	wantProblem(t, err, problem.TypeValidationFailed)
}

// --- Delete ---

func TestSynodHandler_Delete_204(t *testing.T) {
	// The group exists, isn't builtin, `*` doesn't bundle it → deletion goes through.
	pool := &rbacFakePool{lockSynodFound: true, lockSynodValue: false}
	h := newSynodHandler(t, pool)
	if _, err := h.DeleteTyped(context.Background(), "team"); err != nil {
		t.Fatalf("DeleteTyped: %v", err)
	}
}

func TestSynodHandler_Delete_NotFound_404(t *testing.T) {
	pool := &rbacFakePool{lockSynodFound: false}
	h := newSynodHandler(t, pool)
	_, err := h.DeleteTyped(context.Background(), "ghost")
	wantProblem(t, err, problem.TypeSynodNotFound)
}

func TestSynodHandler_Delete_Builtin_409(t *testing.T) {
	pool := &rbacFakePool{lockSynodFound: true, lockSynodValue: true}
	h := newSynodHandler(t, pool)
	_, err := h.DeleteTyped(context.Background(), "protected")
	wantProblem(t, err, problem.TypeSynodBuiltin)
}

// --- Update (description) --- ADR-049 amend

func TestSynodHandler_Update_204(t *testing.T) {
	// The group exists → UPDATE 1 row → 204. Only description changes.
	pool := &rbacFakePool{updateSynodFound: true}
	h := newSynodHandler(t, pool)
	if _, err := h.UpdateTyped(context.Background(), claimsFor("archon-alice"),
		"team", SynodUpdateInput{Description: "new desc"}); err != nil {
		t.Fatalf("UpdateTyped: %v", err)
	}
}

func TestSynodHandler_Update_NotFound_404(t *testing.T) {
	// The group doesn't exist → UPDATE 0 rows → ErrSynodNotFound → 404.
	pool := &rbacFakePool{updateSynodFound: false}
	h := newSynodHandler(t, pool)
	_, err := h.UpdateTyped(context.Background(), claimsFor("archon-alice"),
		"ghost", SynodUpdateInput{Description: "x"})
	wantProblem(t, err, problem.TypeSynodNotFound)
}

func TestSynodHandler_Update_Builtin_204(t *testing.T) {
	// builtin IS ALLOWED to have its description edited (cosmetic, not behavior —
	// ADR-049 amend). The Service doesn't check builtin: UPDATE 1 row → 204.
	pool := &rbacFakePool{updateSynodFound: true}
	h := newSynodHandler(t, pool)
	if _, err := h.UpdateTyped(context.Background(), claimsFor("archon-alice"),
		"cluster-admins", SynodUpdateInput{Description: "edited builtin"}); err != nil {
		t.Fatalf("UpdateTyped (builtin edit must be allowed): %v", err)
	}
}

func TestSynodHandler_Update_EmptyDescription_422(t *testing.T) {
	h := newSynodHandler(t, &rbacFakePool{updateSynodFound: true})
	_, err := h.UpdateTyped(context.Background(), claimsFor("archon-alice"),
		"team", SynodUpdateInput{Description: ""})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestSynodHandler_Update_TooLongDescription_422(t *testing.T) {
	long := strings.Repeat("a", rbac.SynodDescriptionMaxLen+1)
	h := newSynodHandler(t, &rbacFakePool{updateSynodFound: true})
	_, err := h.UpdateTyped(context.Background(), claimsFor("archon-alice"),
		"team", SynodUpdateInput{Description: long})
	wantProblem(t, err, problem.TypeValidationFailed)
}

// --- AddOperator ---

func TestSynodHandler_AddOperator_204(t *testing.T) {
	// The group exists, the bundle is empty (synodRolesValue nil) → subset no-op → ok.
	pool := &rbacFakePool{lockSynodFound: true}
	h := newSynodHandler(t, pool)
	if _, err := h.AddOperatorTyped(context.Background(), claimsFor("archon-alice"),
		"team", "archon-bob"); err != nil {
		t.Fatalf("AddOperatorTyped: %v", err)
	}
}

func TestSynodHandler_AddOperator_InvalidAID_422(t *testing.T) {
	h := newSynodHandler(t, &rbacFakePool{lockSynodFound: true})
	_, err := h.AddOperatorTyped(context.Background(), claimsFor("archon-alice"), "team", "BAD AID")
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestSynodHandler_AddOperator_SynodNotFound_404(t *testing.T) {
	pool := &rbacFakePool{lockSynodFound: false}
	h := newSynodHandler(t, pool)
	_, err := h.AddOperatorTyped(context.Background(), claimsFor("archon-alice"), "ghost", "archon-bob")
	wantProblem(t, err, problem.TypeSynodNotFound)
}

// --- RemoveOperator ---

func TestSynodHandler_RemoveOperator_204(t *testing.T) {
	// Membership exists (lockRoleOperatorFound), the group exists, `*` doesn't bundle it.
	pool := &rbacFakePool{lockSynodFound: true, lockRoleOperatorFound: true}
	h := newSynodHandler(t, pool)
	if _, err := h.RemoveOperatorTyped(context.Background(), "team", "archon-bob"); err != nil {
		t.Fatalf("RemoveOperatorTyped: %v", err)
	}
}

func TestSynodHandler_RemoveOperator_NotFound_404(t *testing.T) {
	pool := &rbacFakePool{lockSynodFound: true, lockRoleOperatorFound: false}
	h := newSynodHandler(t, pool)
	_, err := h.RemoveOperatorTyped(context.Background(), "team", "archon-bob")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestSynodHandler_RemoveOperator_InvalidAID_422(t *testing.T) {
	h := newSynodHandler(t, &rbacFakePool{})
	_, err := h.RemoveOperatorTyped(context.Background(), "team", "BAD AID")
	wantProblem(t, err, problem.TypeValidationFailed)
}

// --- GrantRole ---

func TestSynodHandler_GrantRole_204(t *testing.T) {
	// The group exists; the role carries no permissions (rolePerms nil) → subset no-op.
	pool := &rbacFakePool{lockSynodFound: true}
	h := newSynodHandler(t, pool)
	if _, err := h.GrantRoleTyped(context.Background(), claimsFor("archon-alice"), "team", "viewer"); err != nil {
		t.Fatalf("GrantRoleTyped: %v", err)
	}
}

func TestSynodHandler_GrantRole_EmptyRole_422(t *testing.T) {
	h := newSynodHandler(t, &rbacFakePool{lockSynodFound: true})
	_, err := h.GrantRoleTyped(context.Background(), claimsFor("archon-alice"), "team", "")
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestSynodHandler_GrantRole_RoleNotFound_404(t *testing.T) {
	// FK violation on role_name (synod_roles_role_fk) → ErrRoleNotFound → 404.
	pool := &rbacFakePool{
		lockSynodFound:      true,
		insertMembershipErr: &pgconn.PgError{Code: "23503", ConstraintName: "synod_roles_role_fk"},
	}
	h := newSynodHandler(t, pool)
	_, err := h.GrantRoleTyped(context.Background(), claimsFor("archon-alice"), "team", "ghost")
	wantProblem(t, err, problem.TypeRoleNotFound)
}

// --- RevokeRole ---

func TestSynodHandler_RevokeRole_204(t *testing.T) {
	// The bundle pair exists (lockRoleOperatorFound), the role doesn't grant `*` (rolePerms nil).
	pool := &rbacFakePool{lockRoleOperatorFound: true}
	h := newSynodHandler(t, pool)
	if _, err := h.RevokeRoleTyped(context.Background(), "team", "viewer"); err != nil {
		t.Fatalf("RevokeRoleTyped: %v", err)
	}
}

func TestSynodHandler_RevokeRole_NotFound_404(t *testing.T) {
	pool := &rbacFakePool{lockRoleOperatorFound: false}
	h := newSynodHandler(t, pool)
	_, err := h.RevokeRoleTyped(context.Background(), "team", "viewer")
	wantProblem(t, err, problem.TypeNotFound)
}
