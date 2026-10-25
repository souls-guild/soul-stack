//go:build integration

// Integration tests for per-token accounting (migration 127, NIM-1045) against
// real Postgres: the SQL of mint / list / touch / revoke, the revoked-token
// projection the RBAC snapshot loads, the CHECK constraints and the Reaper's
// purge function. The unit tests drive the same code through a fake pool, which
// proves the order of calls and not one line of the SQL.

package operator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

func mintFor(t *testing.T, aid string, kind JWTKind, by, label string) MintResult {
	t.Helper()
	res, err := MintJWT(context.Background(), integrationPool, &fakeIssuer{}, MintInput{
		AID: aid, TTL: time.Hour, Kind: kind, CreatedByAID: by, Label: label,
		Client: ClientMeta{IP: "10.0.0.1", UserAgent: "itest/1"},
	})
	if err != nil {
		t.Fatalf("MintJWT(%s, %s): %v", aid, kind, err)
	}
	return res
}

func TestIntegration_JWT_MintListTouchRevoke(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "archon-alice", nil)
	alice := "archon-alice"
	seedActiveOperator(t, "sd-integration", &alice)

	issued := mintFor(t, "sd-integration", JWTKindIssued, "archon-alice", "Service Desk prod")
	time.Sleep(10 * time.Millisecond)
	session := mintFor(t, "sd-integration", JWTKindSession, "", "")

	recs, total, err := ListJWTs(ctx, integrationPool, "sd-integration", 0, 50)
	if err != nil {
		t.Fatalf("ListJWTs: %v", err)
	}
	if total != 2 || len(recs) != 2 {
		t.Fatalf("total=%d len=%d, want 2", total, len(recs))
	}
	if recs[0].JWTID != session.JWTID || recs[1].JWTID != issued.JWTID {
		t.Errorf("order = %s, %s; want newest first", recs[0].JWTID, recs[1].JWTID)
	}
	got := recs[1]
	if got.Kind != JWTKindIssued || got.Label != "Service Desk prod" || got.CreatedByAID == nil || *got.CreatedByAID != "archon-alice" ||
		got.CreatedIP != "10.0.0.1" || got.CreatedUserAgent != "itest/1" || got.RevokedAt != nil || got.LastUsedAt != nil {
		t.Errorf("issued row = %+v", got)
	}
	if recs[0].CreatedByAID != nil {
		t.Errorf("session row created_by_aid = %v, want NULL", *recs[0].CreatedByAID)
	}

	used := time.Now().UTC().Truncate(time.Microsecond)
	if err := TouchJWT(ctx, integrationPool, issued.JWTID, used, ClientMeta{IP: "10.0.0.9", UserAgent: "sd/2"}); err != nil {
		t.Fatalf("TouchJWT: %v", err)
	}
	if err := TouchJWT(ctx, integrationPool, issued.JWTID, used.Add(-time.Hour), ClientMeta{IP: "stale", UserAgent: "stale"}); err != nil {
		t.Fatalf("TouchJWT (older): %v", err)
	}
	recs, _, _ = ListJWTs(ctx, integrationPool, "sd-integration", 0, 50)
	if lu := recs[1].LastUsedAt; lu == nil || !lu.Equal(used) || recs[1].LastUsedIP != "10.0.0.9" {
		t.Errorf("last used = %v %q, want %v from 10.0.0.9 — a delayed older write must not move it back", lu, recs[1].LastUsedIP, used)
	}

	if _, err := RevokeJWT(ctx, integrationPool, "archon-alice", issued.JWTID, "archon-alice"); !errors.Is(err, ErrJWTNotFound) {
		t.Errorf("revoke under another operator's AID = %v, want ErrJWTNotFound", err)
	}
	rec, err := RevokeJWT(ctx, integrationPool, "sd-integration", issued.JWTID, "archon-alice")
	if err != nil {
		t.Fatalf("RevokeJWT: %v", err)
	}
	if rec.RevokedAt == nil || rec.RevokedByAID == nil || *rec.RevokedByAID != "archon-alice" {
		t.Errorf("revoked row = %+v", rec)
	}
	if _, err := RevokeJWT(ctx, integrationPool, "sd-integration", issued.JWTID, "archon-alice"); !errors.Is(err, ErrJWTAlreadyRevoked) {
		t.Errorf("second revoke = %v, want ErrJWTAlreadyRevoked", err)
	}

	snap, err := rbac.LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if _, ok := snap.RevokedJWTs[issued.JWTID]; !ok {
		t.Error("the revoked token is missing from the RBAC snapshot")
	}
	if _, ok := snap.RevokedJWTs[session.JWTID]; ok {
		t.Error("the session token is in the revoked projection without being revoked")
	}
	if _, ok := snap.Revoked["sd-integration"]; ok {
		t.Error("revoking one token revoked its operator")
	}
}

// The snapshot keeps a revoked id for five minutes past its expiry (the clock
// margin), then lets it go; the Reaper deletes rows only past max_age.
func TestIntegration_JWT_RevokedWindowAndPurge(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "archon-alice", nil)

	insert := func(id string, expiredAgo time.Duration) {
		t.Helper()
		_, err := integrationPool.Exec(ctx, `
INSERT INTO operator_jwts (jwt_id, aid, kind, created_at, expires_at, revoked_at)
VALUES ($1, 'archon-alice', 'issued', NOW() - $2::interval - INTERVAL '1 hour', NOW() - $2::interval, NOW() - $2::interval - INTERVAL '30 minutes')`,
			id, expiredAgo.String())
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("01JEXP1RED1M1N000000000000", time.Minute)
	insert("01JEXP1RED10M1N00000000000", 10*time.Minute)
	insert("01JEXP1RED31D0000000000000", 31*24*time.Hour)

	snap, err := rbac.LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if _, ok := snap.RevokedJWTs["01JEXP1RED1M1N000000000000"]; !ok {
		t.Error("a token expired a minute ago left the projection inside the clock margin")
	}
	if _, ok := snap.RevokedJWTs["01JEXP1RED10M1N00000000000"]; ok {
		t.Error("a token expired ten minutes ago is still loaded")
	}

	var deleted int64
	if err := integrationPool.QueryRow(ctx, `SELECT purge_operator_jwts('30 days'::interval, 100)`).Scan(&deleted); err != nil {
		t.Fatalf("purge_operator_jwts: %v", err)
	}
	if deleted != 1 {
		t.Errorf("purged %d rows, want only the one expired 31 days ago", deleted)
	}
	var left int
	_ = integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operator_jwts`).Scan(&left)
	if left != 2 {
		t.Errorf("%d rows left, want 2", left)
	}

	// A max_age shorter than the snapshot's clock margin is floored at an hour:
	// the row expired ten minutes ago stays.
	if err := integrationPool.QueryRow(ctx, `SELECT purge_operator_jwts('1 second'::interval, 100)`).Scan(&deleted); err != nil {
		t.Fatalf("purge_operator_jwts (1s): %v", err)
	}
	if deleted != 0 {
		t.Errorf("a 1s max_age purged %d rows inside the one-hour floor", deleted)
	}
}

func TestIntegration_JWT_Constraints(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "archon-alice", nil)

	for name, sql := range map[string]string{
		"unknown kind":     `INSERT INTO operator_jwts (jwt_id, aid, kind, created_at, expires_at) VALUES ('01JKC0NSTRA1NT000000000000', 'archon-alice', 'personal', NOW(), NOW() + INTERVAL '1 hour')`,
		"malformed id":     `INSERT INTO operator_jwts (jwt_id, aid, kind, created_at, expires_at) VALUES ('not-a-ulid', 'archon-alice', 'issued', NOW(), NOW() + INTERVAL '1 hour')`,
		"label too long":   `INSERT INTO operator_jwts (jwt_id, aid, kind, label, created_at, expires_at) VALUES ('01JKC0NSTRA1NT000000000001', 'archon-alice', 'issued', '` + strings.Repeat("x", 201) + `', NOW(), NOW() + INTERVAL '1 hour')`,
		"expiry not after": `INSERT INTO operator_jwts (jwt_id, aid, kind, created_at, expires_at) VALUES ('01JKC0NSTRA1NT000000000002', 'archon-alice', 'issued', NOW(), NOW())`,
		"unknown operator": `INSERT INTO operator_jwts (jwt_id, aid, kind, created_at, expires_at) VALUES ('01JKC0NSTRA1NT000000000003', 'archon-ghost', 'issued', NOW(), NOW() + INTERVAL '1 hour')`,
	} {
		if _, err := integrationPool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: insert accepted", name)
		}
	}
}
