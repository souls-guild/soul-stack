//go:build integration

// Integration tests for Service.Revoke against real Postgres: verify that
// self-lockout invariant holds under concurrent revokes thanks to
// SELECT … FOR UPDATE (architect-verdict M0.6b §1, Slice 3). Unit mocks
// (service_test.go) don't prove serialization — need real row-level lock.
//
// Slice 3: lockout-probe takes admin-set from DB (rbac.LockEffectiveClusterAdmins
// — JOIN rbac_role_operators × rbac_role_permissions × operators under
// FOR UPDATE OF ro,rp,o), NOT from in-memory ClusterAdmins() snapshot. Therefore
// seed creates real membership row (cluster-admin, <aid>), doesn't pass
// admin-set via fakeRBAC. Role cluster-admin with permission `*` already seeded
// by migration 027.
//
// Issuer/RBAC here — fake (defined in service_test.go, shared package);
// fakeRBAC.admins no longer participates in lockout (DB source) — left
// zero to prove invariant independence from snapshot.

package operator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// seedActiveOperator inserts active operator. bootstrap=true → CreatedByAID
// nil (first Archon); otherwise created_by_aid references parent.
func seedActiveOperator(t *testing.T, aid string, parent *string) {
	t.Helper()
	op := &Operator{
		AID:          aid,
		DisplayName:  aid,
		AuthMethod:   AuthMethodJWT,
		CreatedByAID: parent,
	}
	if err := Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seed %q: %v", aid, err)
	}
}

// seedClusterAdmin inserts active operator AND membership row
// (cluster-admin, aid) — makes him effective `*`-admin in DB source
// of lockout-probe (Slice 3). Role cluster-admin (+permission `*`) already
// in schema from migration 027.
func seedClusterAdmin(t *testing.T, aid string, parent *string) {
	t.Helper()
	seedActiveOperator(t, aid, parent)
	grantClusterAdmin(t, aid)
}

// grantClusterAdmin adds membership (cluster-admin, aid). granted_by_aid
// = NULL (seed-membership without initiator, like bootstrap).
func grantClusterAdmin(t *testing.T, aid string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO rbac_role_operators (role_name, aid, granted_by_aid)
		 VALUES ('cluster-admin', $1, NULL)`, aid)
	if err != nil {
		t.Fatalf("grant cluster-admin %q: %v", aid, err)
	}
}

func newIntegrationService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{
		Pool:   integrationPool,
		Issuer: &fakeIssuer{},
		// admins intentionally empty: lockout invariant Slice 3 should not depend
		// on ClusterAdmins() snapshot.
		RBAC:       &fakeRBAC{},
		TTLDefault: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func TestIntegration_ServiceRevoke_HappyPath(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)
	err := s.Revoke(context.Background(), RevokeInput{
		AID: "archon-bob", Reason: "left team", CallerAID: "archon-alice",
	})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	got, err := SelectByAID(context.Background(), integrationPool, "archon-bob")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if !got.IsRevoked() {
		t.Errorf("archon-bob not revoked after Revoke")
	}
	if got.Metadata["revoke_reason"] != "left team" {
		t.Errorf("revoke_reason = %v, want \"left team\"", got.Metadata["revoke_reason"])
	}
}

// TestIntegration_ServiceRevoke_WouldLockOutCluster — only active
// effective `*`-admin removed → lockout (via DB admin-set, not snapshot).
func TestIntegration_ServiceRevoke_WouldLockOutCluster(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)

	s := newIntegrationService(t)
	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-alice", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster", err)
	}

	// Operator remained active — UPDATE rolled back with tx.
	got, err := SelectByAID(context.Background(), integrationPool, "archon-alice")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if got.IsRevoked() {
		t.Errorf("archon-alice revoked, want активен (lockout-инвариант)")
	}
}

// TestIntegration_ServiceRevoke_NotLastAdmin — revoke of non-last admin
// succeeds: second active effective `*`-admin remains.
func TestIntegration_ServiceRevoke_NotLastAdmin(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)
	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-alice", CallerAID: "archon-bob"})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, err := SelectByAID(context.Background(), integrationPool, "archon-alice")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if !got.IsRevoked() {
		t.Errorf("archon-alice not revoked after Revoke")
	}
}

// TestIntegration_ServiceRevoke_RevokedSecondAdminStillLocks — CENTRAL
// case of Slice 3 (closes qa gap in Slice 1). Second cluster-admin already
// revoked (revoked_at != NULL). Remove only active → lockout:
// revoked NOT counted as "survivor". ClusterAdmins() snapshot could "remember"
// second as admin (staleness) and wrongly allow revoke; DB predicate
// `operators.revoked_at IS NULL` cuts it strictly.
func TestIntegration_ServiceRevoke_RevokedSecondAdminStillLocks(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)
	ctx := context.Background()

	// First legally remove bob (alice still active — lockout won't trigger).
	if err := s.Revoke(ctx, RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"}); err != nil {
		t.Fatalf("Revoke bob: %v", err)
	}
	bobGot, err := SelectByAID(ctx, integrationPool, "archon-bob")
	if err != nil {
		t.Fatalf("SelectByAID bob: %v", err)
	}
	if !bobGot.IsRevoked() {
		t.Fatalf("precondition: archon-bob must be revoked")
	}

	// Now alice — only ACTIVE effective `*`-admin (bob revoked,
	// but his membership row still exists). Removing alice → lockout.
	err = s.Revoke(ctx, RevokeInput{AID: "archon-alice", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrWouldLockOutCluster) {
		t.Fatalf("err = %v, want ErrWouldLockOutCluster (revoked bob should not count as survivor)", err)
	}

	got, err := SelectByAID(ctx, integrationPool, "archon-alice")
	if err != nil {
		t.Fatalf("SelectByAID alice: %v", err)
	}
	if got.IsRevoked() {
		t.Errorf("archon-alice revoked, want активен (lockout-инвариант)")
	}
}

func TestIntegration_ServiceRevoke_NotFound(t *testing.T) {
	resetOperators(t)
	s := newIntegrationService(t)
	err := s.Revoke(context.Background(), RevokeInput{AID: "archon-ghost", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want ErrOperatorNotFound", err)
	}
}

func TestIntegration_ServiceRevoke_AlreadyRevoked(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)
	ctx := context.Background()
	if err := s.Revoke(ctx, RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"}); err != nil {
		t.Fatalf("Revoke#1: %v", err)
	}
	err := s.Revoke(ctx, RevokeInput{AID: "archon-bob", CallerAID: "archon-alice"})
	if !errors.Is(err, ErrOperatorAlreadyRevoked) {
		t.Fatalf("Revoke#2: err = %v, want ErrOperatorAlreadyRevoked", err)
	}
}

// TestIntegration_ServiceRevoke_ConcurrentLastAdmins — two active
// cluster-admins, two concurrent Revokes (each revokes the other). Without
// SELECT … FOR UPDATE both could pass probe "admin-set still ≥ 2" and
// commit → self-lockout. With FOR UPDATE OF ro,rp,o serialization exactly one
// succeeds, second sees only last active admin remains, gets
// ErrWouldLockOutCluster. At least one active admin must remain.
//
// Slice 3: admin-set comes from DB, not snapshot, so race revoke ‖
// revoke (and revoke ‖ role-mutation — single FOR UPDATE core) serializes.
func TestIntegration_ServiceRevoke_ConcurrentLastAdmins(t *testing.T) {
	resetOperators(t)
	seedClusterAdmin(t, "archon-alice", nil)
	alice := "archon-alice"
	seedClusterAdmin(t, "archon-bob", &alice)

	s := newIntegrationService(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	targets := []RevokeInput{
		{AID: "archon-alice", CallerAID: "archon-bob"},
		{AID: "archon-bob", CallerAID: "archon-alice"},
	}
	for i := range targets {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			errs[idx] = s.Revoke(context.Background(), targets[idx])
		}(i)
	}
	close(start)
	wg.Wait()

	successes, lockouts := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case errors.Is(e, ErrWouldLockOutCluster):
			lockouts++
		default:
			t.Fatalf("unexpected error from Revoke: %v", e)
		}
	}
	if successes != 1 || lockouts != 1 {
		t.Fatalf("successes=%d lockouts=%d, want 1/1 (FOR UPDATE serialization)", successes, lockouts)
	}

	// Invariant: at least one active effective `*`-admin remains in DB.
	remaining := effectiveAdminCount(t)
	if remaining < 1 {
		t.Fatalf("active admins remaining %d, want >= 1 (cluster must not lock)", remaining)
	}
}

// effectiveAdminCount — count of active operators with effective `*` in DB.
// Count directly (read-only, no lock) for post-check of invariant.
func effectiveAdminCount(t *testing.T) int {
	t.Helper()
	var n int
	err := integrationPool.QueryRow(context.Background(), `
		SELECT COUNT(DISTINCT ro.aid)
		FROM rbac_role_operators ro
		JOIN rbac_role_permissions rp ON rp.role_name = ro.role_name
		JOIN operators o ON o.aid = ro.aid
		WHERE rp.permission = '*' AND o.revoked_at IS NULL`).Scan(&n)
	if err != nil {
		t.Fatalf("effectiveAdminCount: %v", err)
	}
	return n
}
