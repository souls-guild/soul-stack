package rbac

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/migrations"
)

// roleSet builds the catalog-name set [validateRoleGraph] takes.
func roleSet(names ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

// TestValidateRoleGraph — the model-level parent-graph guards (ADR-078). Each
// case is a security invariant, not a nicety: a cycle has no root to attenuate
// against, an unbounded chain makes the ceiling unreadable, and a dangling parent
// is a child whose ceiling cannot be resolved at all.
func TestValidateRoleGraph(t *testing.T) {
	tests := []struct {
		name    string
		roles   map[string]struct{}
		parents map[string]string
		wantErr error
	}{
		{
			name:  "no parents at all is the plain catalog of today",
			roles: roleSet("cluster-admin", "ops"),
		},
		{
			name:    "single hop",
			roles:   roleSet("dba", "dba-aboba"),
			parents: map[string]string{"dba-aboba": "dba"},
		},
		{
			name:  "chain of exactly maxRoleChainDepth roles is allowed",
			roles: roleSet("r1", "r2", "r3", "r4"),
			parents: map[string]string{
				"r2": "r1", "r3": "r2", "r4": "r3",
			},
		},
		{
			name:    "self-parent",
			roles:   roleSet("dba"),
			parents: map[string]string{"dba": "dba"},
			wantErr: ErrRoleParentCycle,
		},
		{
			name:    "two-role cycle",
			roles:   roleSet("a", "b"),
			parents: map[string]string{"a": "b", "b": "a"},
			wantErr: ErrRoleParentCycle,
		},
		{
			name:    "three-role cycle",
			roles:   roleSet("a", "b", "c"),
			parents: map[string]string{"a": "b", "b": "c", "c": "a"},
			wantErr: ErrRoleParentCycle,
		},
		{
			// Walking up from `tail` never returns to `tail`, only into the a→b→a
			// loop. Reported as a cycle, not as an over-deep chain — the hop count
			// alone could not tell these apart.
			name:    "role feeding into a cycle it is not part of",
			roles:   roleSet("tail", "a", "b"),
			parents: map[string]string{"tail": "a", "a": "b", "b": "a"},
			wantErr: ErrRoleParentCycle,
		},
		{
			name:  "chain one role past the cap",
			roles: roleSet("r1", "r2", "r3", "r4", "r5"),
			parents: map[string]string{
				"r2": "r1", "r3": "r2", "r4": "r3", "r5": "r4",
			},
			wantErr: ErrRoleChainTooDeep,
		},
		{
			name:    "parent missing from the catalog",
			roles:   roleSet("child"),
			parents: map[string]string{"child": "ghost"},
			wantErr: ErrRoleParentUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRoleGraph(tc.roles, tc.parents)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("validateRoleGraph: unexpected error: %v", err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("validateRoleGraph = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestRoleChainDepthCapMatchesMigration pins [maxRoleChainDepth] to max_depth in
// migration 102. The cap is enforced twice — in plpgsql on the write path and in
// Go on the snapshot path — and the two cannot be expressed once. If they drift,
// a chain accepted by the DB would brick every enforcer build, or vice versa.
func TestRoleChainDepthCapMatchesMigration(t *testing.T) {
	b, err := migrations.FS.ReadFile("102_rbac_roles_parent_role.up.sql")
	if err != nil {
		t.Fatalf("read migration 102: %v", err)
	}
	m := regexp.MustCompile(`max_depth\s+CONSTANT\s+int\s*:=\s*(\d+)`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("migration 102 no longer declares `max_depth CONSTANT int := N`")
	}
	sqlDepth, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse max_depth: %v", err)
	}
	if sqlDepth != maxRoleChainDepth {
		t.Errorf("migration 102 max_depth = %d, maxRoleChainDepth = %d — the two guards disagree",
			sqlDepth, maxRoleChainDepth)
	}
}

// TestLoadSnapshot_RoleParents — ADR-078: parent_role is read into
// Snapshot.RoleParents. NULL (a plain role) does NOT end up in the projection —
// a missing key means "no parent", mirroring RoleScopes.
func TestLoadSnapshot_RoleParents(t *testing.T) {
	pool := &snapPool{
		roles:       []string{"dba", "dba-aboba"},
		roleParents: map[string]string{"dba-aboba": "dba"},
		perms: []rolePermRow{
			{"dba", "incarnation.run"},
			{"dba-aboba", "incarnation.run"},
		},
	}
	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got := snap.RoleParents["dba-aboba"]; got != "dba" {
		t.Errorf("RoleParents[dba-aboba] = %q, want %q", got, "dba")
	}
	if _, ok := snap.RoleParents["dba"]; ok {
		t.Error("RoleParents[dba] present, want absent (NULL parent_role)")
	}
}

// TestNewEnforcerFromSnapshot_CarriesParentRole — a derived role reaches the
// enforcer with its parent attached, a plain one with an empty parent.
func TestNewEnforcerFromSnapshot_CarriesParentRole(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(&Snapshot{
		Roles:       map[string][]string{"dba": {"incarnation.get"}, "dba-aboba": {"incarnation.get"}},
		RoleParents: map[string]string{"dba-aboba": "dba"},
		Membership:  map[string][]string{"archon-alice": {"dba-aboba"}},
	})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	got := make(map[string]string, len(e.roles))
	for _, r := range e.roles {
		got[r.Name] = r.ParentRole
	}
	if got["dba-aboba"] != "dba" {
		t.Errorf("dba-aboba.ParentRole = %q, want %q", got["dba-aboba"], "dba")
	}
	if got["dba"] != "" {
		t.Errorf("dba.ParentRole = %q, want empty (plain role)", got["dba"])
	}
}

// TestNewEnforcerFromSnapshot_BrokenGraphFailsClosed — a snapshot whose parent
// graph is broken must NOT build an enforcer. Reaching this means the DB guards
// were bypassed (a hand-edited row, a restore, an older binary); rather than
// serving a catalog whose ceilings cannot be resolved, [Holder] degrades exactly
// as it already does for an unparseable permission — a TTL refresh keeps the
// previous enforcer and warns, startup refuses to come up.
func TestNewEnforcerFromSnapshot_BrokenGraphFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		snap    *Snapshot
		wantErr error
	}{
		{
			name: "cycle",
			snap: &Snapshot{
				Roles:       map[string][]string{"a": nil, "b": nil},
				RoleParents: map[string]string{"a": "b", "b": "a"},
			},
			wantErr: ErrRoleParentCycle,
		},
		{
			name: "chain past the cap",
			snap: &Snapshot{
				Roles: map[string][]string{"r1": nil, "r2": nil, "r3": nil, "r4": nil, "r5": nil},
				RoleParents: map[string]string{
					"r2": "r1", "r3": "r2", "r4": "r3", "r5": "r4",
				},
			},
			wantErr: ErrRoleChainTooDeep,
		},
		{
			name: "parent outside the catalog",
			snap: &Snapshot{
				Roles:       map[string][]string{"child": nil},
				RoleParents: map[string]string{"child": "ghost"},
			},
			wantErr: ErrRoleParentUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewEnforcerFromSnapshot(tc.snap)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewEnforcerFromSnapshot err = %v, want %v", err, tc.wantErr)
			}
			if e != nil {
				t.Error("a rejected snapshot must not yield an enforcer")
			}
		})
	}
}

// TestMapRoleError_ParentChainSQLSTATE — the custom SQLSTATEs raised by the
// rbac_roles_parent_chain_guard trigger are translated into the package
// sentinels, so a write path that sets parent_role reports the model rule
// instead of a raw PG code.
func TestMapRoleError_ParentChainSQLSTATE(t *testing.T) {
	for _, tc := range []struct {
		code    string
		wantErr error
	}{
		{pgErrCodeRoleParentCycle, ErrRoleParentCycle},
		{pgErrCodeRoleChainTooDeep, ErrRoleChainTooDeep},
	} {
		t.Run(tc.code, func(t *testing.T) {
			err := mapRoleError(&pgconn.PgError{Code: tc.code, Message: "guard fired"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("mapRoleError(%s) = %v, want %v", tc.code, err, tc.wantErr)
			}
		})
	}
}

// TestMapRoleDeleteError — the orphan policy surfaces as [ErrRoleHasChildren]
// only for the parent self-FK; other FK violations on the delete path keep their
// previous shape (they mean something else entirely).
func TestMapRoleDeleteError(t *testing.T) {
	parentFK := &pgconn.PgError{
		Code:           pgErrCodeForeignKeyViolation,
		ConstraintName: roleParentFKConstraint,
		Message:        "still referenced",
	}
	if err := mapRoleDeleteError(parentFK); !errors.Is(err, ErrRoleHasChildren) {
		t.Errorf("parent-FK violation = %v, want ErrRoleHasChildren", err)
	}

	otherFK := &pgconn.PgError{
		Code:           pgErrCodeForeignKeyViolation,
		ConstraintName: "rbac_role_operators_aid_fk",
		Message:        "some other FK",
	}
	if err := mapRoleDeleteError(otherFK); errors.Is(err, ErrRoleHasChildren) {
		t.Errorf("unrelated FK violation must not map to ErrRoleHasChildren, got %v", err)
	}
}
