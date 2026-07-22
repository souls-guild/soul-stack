package voyage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestClaimNext_ValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := ClaimNext(ctx, &fakeDB{}, "", time.Minute); err == nil || !strings.Contains(err.Error(), "empty kid") {
		t.Errorf("empty kid: %v", err)
	}
	if _, err := ClaimNext(ctx, &fakeDB{}, "kid", 0); err == nil || !strings.Contains(err.Error(), "non-positive lease") {
		t.Errorf("zero lease: %v", err)
	}
}

func TestClaimNext_NoPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	v, err := ClaimNext(ctx, fdb, "kid-1", time.Minute)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if v != nil {
		t.Errorf("v = %v, want nil (no pending)", v)
	}
}

func TestClaimNext_PassesArgs_AndSQL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	if _, err := ClaimNext(ctx, fdb, "kid-7", 90*time.Second); err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if !strings.Contains(fdb.queryRowSQL, "FOR UPDATE SKIP LOCKED") {
		t.Errorf("claim SQL missing FOR UPDATE SKIP LOCKED: %.300s", fdb.queryRowSQL)
	}
	if !strings.Contains(fdb.queryRowSQL, "attempt          = v.attempt + 1") {
		t.Errorf("claim SQL missing attempt++ (fencing): %.300s", fdb.queryRowSQL)
	}
	if !strings.Contains(fdb.queryRowSQL, "ORDER BY c.created_at ASC") {
		t.Errorf("claim SQL missing FIFO created_at: %.300s", fdb.queryRowSQL)
	}
	if len(fdb.queryRowArgs) != 2 {
		t.Fatalf("queryRowArgs = %d, want 2", len(fdb.queryRowArgs))
	}
	if fdb.queryRowArgs[0] != "kid-7" {
		t.Errorf("arg[0] = %v, want kid-7", fdb.queryRowArgs[0])
	}
}

// TestClaimNext_ScheduledGatingSQL — the claimable predicate covers pending AND
// a due scheduled, but not a future scheduled. The gating lives in SQL
// (fakeDB doesn't simulate PG row-matching, so we check the predicate's shape).
func TestClaimNext_ScheduledGatingSQL(t *testing.T) {
	t.Parallel()
	for _, frag := range []string{
		"c.status = 'pending'",
		"c.status = 'scheduled' AND c.schedule_at <= NOW()",
		"ORDER BY c.created_at ASC",
		"FOR UPDATE SKIP LOCKED",
		"status           = 'running'",
	} {
		if !strings.Contains(claimNextSQL, frag) {
			t.Errorf("claimNextSQL missing %q\nSQL: %s", frag, claimNextSQL)
		}
	}
	// scheduled-gating must be an OR-branch to pending (not an AND-narrowing).
	if !strings.Contains(claimNextSQL, "WHERE c.status = 'pending'\n       OR (c.status = 'scheduled'") {
		t.Errorf("claimNextSQL: scheduled must be OR-branch to pending\nSQL: %s", claimNextSQL)
	}
}

func TestRenewLease_ValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := RenewLease(ctx, &fakeDB{}, "", "kid", time.Minute); err == nil || !strings.Contains(err.Error(), "empty voyage_id") {
		t.Errorf("empty id: %v", err)
	}
	if err := RenewLease(ctx, &fakeDB{}, "v1", "", time.Minute); err == nil || !strings.Contains(err.Error(), "empty kid") {
		t.Errorf("empty kid: %v", err)
	}
	if err := RenewLease(ctx, &fakeDB{}, "v1", "kid", 0); err == nil || !strings.Contains(err.Error(), "non-positive lease") {
		t.Errorf("zero lease: %v", err)
	}
}

func TestRenewLease_LeaseLost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	err := RenewLease(ctx, fdb, "v1", "kid", time.Minute)
	if !errors.Is(err, ErrLeaseLost) {
		t.Errorf("err = %v, want ErrLeaseLost", err)
	}
}

func TestRenewLease_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return stringRow{v: "v1"} }}
	if err := RenewLease(ctx, fdb, "v1", "kid", time.Minute); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if !strings.Contains(fdb.queryRowSQL, "claim_expires_at > NOW()") {
		t.Errorf("renew SQL missing not-expired-guard: %.200s", fdb.queryRowSQL)
	}
}

func TestReleaseLease_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{}
	if err := ReleaseLease(ctx, fdb, "v1", "kid"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if !strings.Contains(fdb.execSQL, "status           = 'pending'") {
		t.Errorf("release SQL does not return to pending: %.200s", fdb.execSQL)
	}
}

func TestReleaseLease_ValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := ReleaseLease(ctx, &fakeDB{}, "", "kid"); err == nil || !strings.Contains(err.Error(), "empty voyage_id") {
		t.Errorf("empty id: %v", err)
	}
	if err := ReleaseLease(ctx, &fakeDB{}, "v1", ""); err == nil || !strings.Contains(err.Error(), "empty kid") {
		t.Errorf("empty kid: %v", err)
	}
}

// stringRow — Scan(*string) for RETURNING voyage_id (RenewLease).
type stringRow struct{ v string }

func (r stringRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("stringRow: expected 1 dest")
	}
	sp, ok := dest[0].(*string)
	if !ok {
		return errors.New("stringRow: dest is not *string")
	}
	*sp = r.v
	return nil
}
