package auth

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
)

// ReconcileRoles is the login's reconciliation handed to the background group
// sync (ADR-058, NIM-1053). What a login can never ask for, the sync does: an
// empty want — every managed role goes, nothing outside the map is touched.
func TestReconcileRoles_EmptyWantDropsEveryManagedRole(t *testing.T) {
	db := &reconcileDB{
		existing:        &operator.Operator{AID: "archon-bob", AuthMethod: operator.AuthMethodLDAP},
		currentRoles:    []string{"operator", "cluster-admin", "manual-extra"},
		rolePerms:       map[string][]string{"cluster-admin": {"*"}},
		survivingAdmins: []string{"archon-root"},
	}
	tx := &reconcileTx{db: db}
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: map[string][]string{"admins": {"cluster-admin"}, "ops": {"operator"}},
		DB:           db, Tx: tx, Audit: &fakeAudit{},
	})

	change, err := m.ReconcileRoles(context.Background(), "archon-bob", nil)
	if err != nil {
		t.Fatalf("ReconcileRoles: %v", err)
	}
	if !slices.Equal(change.Revoked, []string{"cluster-admin", "operator"}) || len(change.Granted) != 0 {
		t.Errorf("change = %+v, want both managed roles revoked, sorted, nothing granted", change)
	}
	if contains(db.revoked, "manual-extra") {
		t.Errorf("manual-extra is outside group_role_map and must survive; revoked=%v", db.revoked)
	}
	if !tx.committed {
		t.Error("the reconciliation must commit")
	}
}

func TestReconcileRoles_ReportsGrants(t *testing.T) {
	db := &reconcileDB{
		existing:     &operator.Operator{AID: "archon-bob", AuthMethod: operator.AuthMethodLDAP},
		currentRoles: []string{"operator"},
	}
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: map[string][]string{"ops": {"operator"}, "ro": {"viewer"}},
		DB:           db, Tx: &reconcileTx{db: db}, Audit: &fakeAudit{},
	})
	change, err := m.ReconcileRoles(context.Background(), "archon-bob", m.RolesForGroups([]string{"ro", "ops"}))
	if err != nil || !slices.Equal(change.Granted, []string{"viewer"}) || len(change.Revoked) != 0 {
		t.Fatalf("change = %+v, %v; want viewer granted only", change, err)
	}
}

func TestReconcileRoles_RefusesTheLastAdmin(t *testing.T) {
	db := &reconcileDB{
		existing:     &operator.Operator{AID: "archon-bob", AuthMethod: operator.AuthMethodLDAP},
		currentRoles: []string{"cluster-admin"},
		rolePerms:    map[string][]string{"cluster-admin": {"*"}},
	}
	tx := &reconcileTx{db: db}
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: map[string][]string{"admins": {"cluster-admin"}},
		DB:           db, Tx: tx, Audit: &fakeAudit{},
	})
	change, err := m.ReconcileRoles(context.Background(), "archon-bob", nil)
	if !errors.Is(err, ErrReconcileWouldLockOutCluster) || !change.Empty() || tx.committed {
		t.Fatalf("err = %v change = %+v committed = %v; want the refusal, nothing reported, nothing committed", err, change, tx.committed)
	}
}

// A mapped role deleted from the registry would fail its grant and roll back
// the revokes beside it; the sync's reconciliation skips it, reports it, and
// the revoke lands.
func TestReconcileRoles_MissingRoleIsSkippedAndRevokesLand(t *testing.T) {
	db := &reconcileDB{
		existing:        &operator.Operator{AID: "archon-bob", AuthMethod: operator.AuthMethodLDAP},
		currentRoles:    []string{"cluster-admin"},
		rolePerms:       map[string][]string{"cluster-admin": {"*"}},
		survivingAdmins: []string{"archon-root"},
		missingRoles:    map[string]bool{"viewer": true},
	}
	tx := &reconcileTx{db: db}
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: map[string][]string{"admins": {"cluster-admin"}, "ro": {"viewer"}},
		DB:           db, Tx: tx, Audit: &fakeAudit{},
	})
	change, err := m.ReconcileRoles(context.Background(), "archon-bob", []string{"viewer"})
	if err != nil {
		t.Fatalf("ReconcileRoles: %v", err)
	}
	if !slices.Equal(change.Revoked, []string{"cluster-admin"}) || len(change.Granted) != 0 || !slices.Equal(change.Missing, []string{"viewer"}) {
		t.Errorf("change = %+v, want cluster-admin revoked, nothing granted, viewer reported missing", change)
	}
	if contains(db.granted, "viewer") || !tx.committed {
		t.Errorf("granted %v committed=%v; the missing role must not be granted and the revoke must commit", db.granted, tx.committed)
	}
}

// The skip is the sync's alone. A login with a mapped role missing from the
// registry still fails and commits nothing — a config error surfaces at the
// door instead of being papered over.
func TestMapper_LoginStillFailsOnAMissingMappedRole(t *testing.T) {
	db := &reconcileDB{
		existing:     &operator.Operator{AID: "archon-bob", AuthMethod: operator.AuthMethodLDAP},
		currentRoles: []string{"operator"},
		missingRoles: map[string]bool{"viewer": true},
	}
	tx := &reconcileTx{db: db}
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: map[string][]string{"ops": {"operator"}, "ro": {"viewer"}},
		DB:           db, Tx: tx, Audit: &fakeAudit{},
	})
	if _, err := m.Map(context.Background(), ExternalIdentity{AID: "archon-bob", Groups: []string{"ops", "ro"}}); err == nil {
		t.Fatal("login with a missing mapped role succeeded")
	}
	if tx.committed {
		t.Error("a failed login committed its reconciliation")
	}
}

func TestManagedRolesOf_OnlyTheMappedDomain(t *testing.T) {
	db := &reconcileDB{currentRoles: []string{"operator", "manual-extra", "cluster-admin"}}
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: map[string][]string{"admins": {"cluster-admin"}, "ops": {"operator"}},
		DB:           db, Audit: &fakeAudit{},
	})
	got, err := m.ManagedRolesOf(context.Background(), "archon-bob")
	if err != nil || !slices.Equal(got, []string{"cluster-admin", "operator"}) {
		t.Fatalf("ManagedRolesOf = %v, %v; want the two mapped roles, sorted", got, err)
	}
}

// Without a transaction a login falls back to grant-only. The sync must not:
// a reconciliation that silently cannot revoke is the defect it closes.
func TestReconcileRoles_WithoutTxIsAnError(t *testing.T) {
	db := &reconcileDB{existing: &operator.Operator{AID: "archon-bob", AuthMethod: operator.AuthMethodLDAP}}
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: map[string][]string{"ops": {"operator"}},
		DB:           db, Audit: &fakeAudit{},
	})
	if _, err := m.ReconcileRoles(context.Background(), "archon-bob", []string{"operator"}); err == nil {
		t.Fatal("ReconcileRoles without Tx succeeded")
	}
	if len(db.granted) != 0 {
		t.Errorf("granted %v without a transaction", db.granted)
	}
}
