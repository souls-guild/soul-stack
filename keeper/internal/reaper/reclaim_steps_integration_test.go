//go:build integration

package reaper_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
)

// Guards for ADR-027 amend (p)(b)4: reclaim addresses the step row, and
// re-checks status on the row it is about to update.

func seedStepClaim(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applyID, sid string, passage int, status string, attempt int, claimExpiresAt time.Time) {
	t.Helper()
	const q = `INSERT INTO apply_runs
		(apply_id, sid, incarnation_name, scenario, status, passage, started_at,
		 claim_by_kid, claim_at, claim_expires_at, attempt)
		VALUES ($1, $2, 'inc-1', 'deploy', $3, $4, $5, 'keeper-dead-01', $5, $6, $7)`
	if _, err := pool.Exec(ctx, q, applyID, sid, status, passage, claimExpiresAt.Add(-time.Hour), claimExpiresAt, attempt); err != nil {
		t.Fatalf("seed %s/%s/%d: %v", applyID, sid, passage, err)
	}
}

func stepStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applyID, sid string, passage int) (string, int) {
	t.Helper()
	var (
		status  string
		attempt int
	)
	const q = `SELECT status, attempt FROM apply_runs WHERE apply_id = $1 AND sid = $2 AND passage = $3`
	if err := pool.QueryRow(ctx, q, applyID, sid, passage).Scan(&status, &attempt); err != nil {
		t.Fatalf("read %s/%s/%d: %v", applyID, sid, passage, err)
	}
	return status, attempt
}

func resetReclaimSteps(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	resetIdentityTables(t, ctx, pool)
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}
	seedIncarnation(t, ctx, pool, "inc-1")
}

// Step 0 finished (its claim columns stay, lease long expired); step 1's
// Ward expired before delivery. Only step 1 goes back to the queue — keyed by
// (apply_id, sid) with no outer status filter, the form before NIM-960, step 0
// went back too and would have run a second time.
func TestIntegration_ReclaimApplyRuns_ReopensOnlyTheExpiredStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := fixturePool(t)
	resetReclaimSteps(t, ctx, pool)

	expired := time.Now().UTC().Add(-time.Minute)
	seedStepClaim(t, ctx, pool, "run-steps", "h1.example.com", 0, "success", 1, expired)
	seedStepClaim(t, ctx, pool, "run-steps", "h1.example.com", 1, "claimed", 2, expired)

	n, err := reaper.NewPurger(pool).ReclaimApplyRuns(ctx, time.Minute, 100)
	if err != nil {
		t.Fatalf("ReclaimApplyRuns: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaimed = %d, want 1 (step 1 only)", n)
	}
	if status, attempt := stepStatus(t, ctx, pool, "run-steps", "h1.example.com", 1); status != "planned" || attempt != 2 {
		t.Errorf("step 1 = %s/attempt %d, want planned/2", status, attempt)
	}
	if status, attempt := stepStatus(t, ctx, pool, "run-steps", "h1.example.com", 0); status != "success" || attempt != 1 {
		t.Errorf("step 0 = %s/attempt %d, want success/1 (untouched)", status, attempt)
	}
}

// The inner SELECT of the reclaim locks nothing. A MarkDispatched that commits
// while the reclaim statement waits on the row's lock is seen by the UPDATE
// only through its outer condition; if that condition ignores status, a
// delivered row returns to `planned` and is sent a second time.
func TestIntegration_ReclaimApplyRuns_RowDispatchedMidStatementStays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := fixturePool(t)
	resetReclaimSteps(t, ctx, pool)

	seedStepClaim(t, ctx, pool, "run-race", "h1.example.com", 0, "claimed", 1, time.Now().UTC().Add(-time.Minute))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE apply_runs SET status = 'dispatched'
		WHERE apply_id = 'run-race' AND sid = 'h1.example.com' AND passage = 0`); err != nil {
		t.Fatalf("mark dispatched in tx: %v", err)
	}

	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := reaper.NewPurger(pool).ReclaimApplyRuns(ctx, time.Minute, 100)
		done <- result{n, err}
	}()

	waitForLockWaiter(t, ctx, pool)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("ReclaimApplyRuns: %v", res.err)
	}
	if res.n != 0 {
		t.Errorf("reclaimed = %d, want 0", res.n)
	}
	if status, _ := stepStatus(t, ctx, pool, "run-race", "h1.example.com", 0); status != "dispatched" {
		t.Errorf("status = %q, want dispatched (a delivered row must not return to the queue)", status)
	}
}

// The same race, with a re-claim instead of a hand-off: a live Acolyte claims the
// row again, with a fresh lease, while the reclaim statement waits on it. The
// re-check must see the fresh lease and leave the new owner's Ward alone.
func TestIntegration_ReclaimApplyRuns_RowReclaimedMidStatementKeepsItsLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := fixturePool(t)
	resetReclaimSteps(t, ctx, pool)

	seedStepClaim(t, ctx, pool, "run-reclaimed", "h1.example.com", 0, "claimed", 1, time.Now().UTC().Add(-time.Minute))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE apply_runs
		SET claim_by_kid = 'keeper-live', claim_at = NOW(), claim_expires_at = NOW() + INTERVAL '30 seconds', attempt = attempt + 1
		WHERE apply_id = 'run-reclaimed' AND sid = 'h1.example.com' AND passage = 0`); err != nil {
		t.Fatalf("re-claim in tx: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := reaper.NewPurger(pool).ReclaimApplyRuns(ctx, time.Minute, 100)
		done <- err
	}()

	waitForLockWaiter(t, ctx, pool)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("ReclaimApplyRuns: %v", err)
	}
	if status, attempt := stepStatus(t, ctx, pool, "run-reclaimed", "h1.example.com", 0); status != "claimed" || attempt != 2 {
		t.Errorf("row = %s/attempt %d, want claimed/2 (the live Ward stands)", status, attempt)
	}
}

// waitForLockWaiter blocks until some backend waits on a row lock, so the
// reclaim statement has taken its snapshot before the competing commit.
func waitForLockWaiter(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%reclaimed AS%'`).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the reclaim statement never waited on the row lock")
}
