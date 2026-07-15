package rbac

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestMapRoleError — mapping of pgx errors to the package's sentinels.
func TestMapRoleError(t *testing.T) {
	t.Run("unique→ErrRoleAlreadyExists", func(t *testing.T) {
		pgErr := &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "rbac_roles_pkey"}
		got := mapRoleError(pgErr)
		if !errors.Is(got, ErrRoleAlreadyExists) {
			t.Fatalf("err = %v, want errors.Is ErrRoleAlreadyExists", got)
		}
		// The original is reachable via errors.Is (multi-wrap).
		if !errors.Is(got, pgErr) {
			t.Errorf("original PgError lost in wrap: %v", got)
		}
	})

	t.Run("fk→wrapped, not AlreadyExists", func(t *testing.T) {
		pgErr := &pgconn.PgError{Code: pgErrCodeForeignKeyViolation, ConstraintName: "rbac_roles_created_by_aid_fk"}
		got := mapRoleError(pgErr)
		if errors.Is(got, ErrRoleAlreadyExists) {
			t.Errorf("FK violation wrongly mapped to ErrRoleAlreadyExists: %v", got)
		}
		if !errors.Is(got, pgErr) {
			t.Errorf("original PgError lost: %v", got)
		}
		// The constraint name is in the message for diagnostics.
		if want := "rbac_roles_created_by_aid_fk"; !strings.Contains(got.Error(), want) {
			t.Errorf("err = %q, want substring %q", got.Error(), want)
		}
	})

	t.Run("other→generic wrap", func(t *testing.T) {
		base := errors.New("connection reset")
		got := mapRoleError(base)
		if errors.Is(got, ErrRoleAlreadyExists) {
			t.Errorf("generic error wrongly mapped to sentinel: %v", got)
		}
		if !errors.Is(got, base) {
			t.Errorf("base error lost in wrap: %v", got)
		}
	})
}

// TestMapGrantError — grant-membership maps an FK violation to
// ErrOperatorNotFound, everything else to a generic wrap.
func TestMapGrantError(t *testing.T) {
	t.Run("fk→ErrOperatorNotFound", func(t *testing.T) {
		pgErr := &pgconn.PgError{Code: pgErrCodeForeignKeyViolation, ConstraintName: "rbac_role_operators_aid_fk"}
		got := mapGrantError(pgErr)
		if !errors.Is(got, ErrOperatorNotFound) {
			t.Fatalf("err = %v, want errors.Is ErrOperatorNotFound", got)
		}
		if !errors.Is(got, pgErr) {
			t.Errorf("original PgError lost in wrap: %v", got)
		}
		if want := "rbac_role_operators_aid_fk"; !strings.Contains(got.Error(), want) {
			t.Errorf("err = %q, want substring %q", got.Error(), want)
		}
	})

	t.Run("unique→not ErrOperatorNotFound", func(t *testing.T) {
		// 23505 is unreachable on the grant path (ON CONFLICT DO NOTHING),
		// but the mapper must not mistakenly map it to ErrOperatorNotFound.
		pgErr := &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "rbac_role_operators_pkey"}
		got := mapGrantError(pgErr)
		if errors.Is(got, ErrOperatorNotFound) {
			t.Errorf("unique violation wrongly mapped to ErrOperatorNotFound: %v", got)
		}
	})

	t.Run("other→generic wrap", func(t *testing.T) {
		base := errors.New("connection reset")
		got := mapGrantError(base)
		if errors.Is(got, ErrOperatorNotFound) {
			t.Errorf("generic error wrongly mapped to sentinel: %v", got)
		}
		if !errors.Is(got, base) {
			t.Errorf("base error lost in wrap: %v", got)
		}
	})
}

// TestRoleGivesWildcard — exclusion logic over a set of permission strings
// (fake source, no DB): whether the role grants `*`.
func TestRoleGivesWildcard(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"empty", nil, false},
		{"no-wildcard", []string{"soul.list", "incarnation.get"}, false},
		{"only-wildcard", []string{"*"}, true},
		{"wildcard-among-others", []string{"soul.list", "*", "push.apply"}, true},
		{"action-wildcard-is-not-full", []string{"incarnation.*"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := roleGivesWildcard(c.in); got != c.want {
				t.Errorf("roleGivesWildcard(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestCreateRole_ValidationBeforeTx — a bad name / bad permission is caught
// by validation BEFORE any DB access (db is never called). We use a nil db:
// if validation let it through, nil.Exec would panic.
func TestCreateRole_Validation(t *testing.T) {
	t.Run("bad-name", func(t *testing.T) {
		err := CreateRole(t.Context(), nil, "Bad_Name", "", nil, nil, nil)
		if err == nil {
			t.Fatal("CreateRole with bad name: want error, got nil")
		}
		if !strings.Contains(err.Error(), "invalid role name") {
			t.Errorf("err = %v, want 'invalid role name'", err)
		}
	})
	t.Run("bad-permission", func(t *testing.T) {
		err := CreateRole(t.Context(), nil, "ok-role", "", []string{"not-a-valid-perm-three.seg.ments"}, nil, nil)
		if err == nil {
			t.Fatal("CreateRole with bad permission: want error, got nil")
		}
		if !strings.Contains(err.Error(), "invalid permission") {
			t.Errorf("err = %v, want 'invalid permission'", err)
		}
	})
	// ADR-047 S1: a bad default_scope is caught by validation BEFORE the DB (db=nil).
	t.Run("bad-default-scope", func(t *testing.T) {
		bad := "coven" // no '=' → parseSelector error
		err := CreateRole(t.Context(), nil, "ok-role", "", nil, nil, &bad)
		if err == nil {
			t.Fatal("CreateRole with bad default_scope: want error, got nil")
		}
		if !strings.Contains(err.Error(), "default_scope") {
			t.Errorf("err = %v, want 'default_scope'", err)
		}
	})
	// An empty default_scope is valid (= NULL = role with no scope
	// restriction): ParseDefaultScope("") → (nil, nil), not an error.
	t.Run("empty-default-scope-ok", func(t *testing.T) {
		sel, err := ParseDefaultScope("")
		if err != nil {
			t.Errorf("ParseDefaultScope(\"\") = err %v, want nil", err)
		}
		if sel != nil {
			t.Errorf("ParseDefaultScope(\"\") = %v, want nil selector", sel)
		}
	})
}
