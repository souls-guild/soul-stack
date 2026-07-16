//go:build integration

package reaper_test

// Integration tests for Reaper rule reconcile_orphan_applying (ADR-027 amend
// (m)) over real PG + Redis presence (Conclave). They complement unit tests
// (orphan_applying_reconcile_test.go) with a live check of the path SQL
// candidates -> InstanceAlive -> ReleaseApplyingOrphan on real tables/keys.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/audit"
)

func reconcileSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// seedApplyingWithEpoch creates an incarnation in applying with a populated
// epoch, imitating scenario.lockApplyingWithEpoch, plus one running apply_runs
// row for its apply_id. Without this, FENCING-1 cannot distinguish an orphan
// from its own run; a row with the SAME apply_id does not block release.
func seedApplyingWithEpoch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, applyID, kid string, applyingSince time.Time) {
	t.Helper()
	const incSQL = `
INSERT INTO incarnation
    (name, service, service_version, state_schema_version, state, status,
     applying_apply_id, applying_attempt, applying_by_kid, applying_since)
VALUES ($1, 'redis', 'v1', 1, '{"primary":"p"}'::jsonb, 'applying',
        $2, 0, $3, $4)`
	if _, err := pool.Exec(ctx, incSQL, name, applyID, kid, applyingSince); err != nil {
		t.Fatalf("seed applying incarnation %s: %v", name, err)
	}
	const arSQL = `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_at)
VALUES ($1, $2, $3, 'deploy', 'running', NOW())`
	if _, err := pool.Exec(ctx, arSQL, applyID, name+".host-01", name); err != nil {
		t.Fatalf("seed apply_run %s: %v", applyID, err)
	}
}

func incStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM incarnation WHERE name=$1`, name).Scan(&s); err != nil {
		t.Fatalf("read status %s: %v", name, err)
	}
	return s
}

func incEpochNull(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var n int
	const q = `SELECT count(*) FROM incarnation WHERE name=$1
        AND applying_apply_id IS NULL AND applying_attempt IS NULL
        AND applying_by_kid IS NULL AND applying_since IS NULL`
	if err := pool.QueryRow(ctx, q, name).Scan(&n); err != nil {
		t.Fatalf("read epoch %s: %v", name, err)
	}
	return n == 1
}

// incApplyingByKID reads applying_by_kid, the epoch owner of the lock. nil means
// epoch was cleared because the lock was released, or the row is legacy without
// epoch.
func incApplyingByKID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) *string {
	t.Helper()
	var kid *string
	if err := pool.QueryRow(ctx, `SELECT applying_by_kid FROM incarnation WHERE name=$1`, name).Scan(&kid); err != nil {
		t.Fatalf("read applying_by_kid %s: %v", name, err)
	}
	return kid
}

// reconcileFixture starts PG + Redis, skipping without containers, and cleans
// incarnation plus Conclave keys before the test.
func newReconcileFixture(t *testing.T) (context.Context, *pgxpool.Pool, *redis.Client) {
	t.Helper()
	if integrationPool == nil {
		t.Skip("integrationPool is nil (docker unavailable)")
	}
	if integrationRedisAddr == "" {
		t.Skip("integrationRedisAddr is empty (redis unavailable)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	rc, err := redis.NewClient(ctx, redis.Config{Addr: integrationRedisAddr}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	if _, err := integrationPool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}
	return ctx, integrationPool, rc
}

// TestIntegration_ReconcileOrphanApplying_DeadOwner_Released: (f) dead presence
// (KID is NOT registered in Conclave), without foreign rival -> applying is
// released and epoch is cleared to NULL.
func TestIntegration_ReconcileOrphanApplying_DeadOwner_Released(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name = "orphan-dead"
		aid  = "01HRECDEADAPPLY000000001"
		kid  = "keeper-dead-int"
	)
	// applying_since is far in the past, so it passes the stale filter (cutoff=NOW()-90s).
	seedApplyingWithEpoch(t, ctx, pool, name, aid, kid, time.Now().Add(-10*time.Minute))
	// Do NOT register KID in Conclave, so InstanceAlive(kid)=false (dead).

	rec := reaper.NewOrphanApplyingReconciler(pool, rc, nil, reconcileSilentLogger())
	affected, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1 (dead owner, lock released)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "ready" {
		t.Errorf("status = %q, want ready", got)
	}
	if !incEpochNull(t, ctx, pool, name) {
		t.Error("epoch was not cleared after release")
	}
}

// TestIntegration_ReconcileOrphanApplying_AliveOwner_NotReclaimed — (c) presence-
// Live owner (KID registered in Conclave) is NOT released (split-brain guard).
func TestIntegration_ReconcileOrphanApplying_AliveOwner_NotReclaimed(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name = "orphan-alive"
		aid  = "01HRECALIVEAPPLY00000001"
		kid  = "keeper-alive-int"
	)
	seedApplyingWithEpoch(t, ctx, pool, name, aid, kid, time.Now().Add(-10*time.Minute))
	// KID is LIVE in Conclave.
	if err := redis.RegisterInstance(ctx, rc, kid, "{}", 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	t.Cleanup(func() { _ = redis.DeregisterInstance(context.Background(), rc, kid) })

	rec := reaper.NewOrphanApplyingReconciler(pool, rc, nil, reconcileSilentLogger())
	affected, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 0 {
		t.Fatalf("affected = %d, want 0 (live owner, run is in progress)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "applying" {
		t.Errorf("status = %q, want applying (live lock untouched)", got)
	}
}

// TestIntegration_ReconcileOrphanApplying_NullEpoch_NotReclaimed: (b) applying
// WITHOUT epoch (applying_by_kid NULL, legacy/pre-082) is NOT selected by the
// SQL filter, and the rule does NOT reclaim it. Simulate applying without epoch
// by UPDATE, bypassing lockRun.
func TestIntegration_ReconcileOrphanApplying_NullEpoch_NotReclaimed(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const name = "orphan-null-epoch"
	// applying WITHOUT epoch columns (NULL), a legacy row.
	if _, err := pool.Exec(ctx, `
INSERT INTO incarnation (name, service, service_version, state_schema_version, state, status, updated_at)
VALUES ($1, 'redis', 'v1', 1, '{}'::jsonb, 'applying', $2)`,
		name, time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatalf("seed null-epoch applying: %v", err)
	}

	rec := reaper.NewOrphanApplyingReconciler(pool, rc, nil, reconcileSilentLogger())
	affected, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 0 {
		t.Fatalf("affected = %d, want 0 (NULL epoch is not reclaimed, known gap)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "applying" {
		t.Errorf("status = %q, want applying (NULL epoch untouched)", got)
	}
}

// TestIntegration_ReconcileOrphanApplying_FreshLock_NotStale: fresh applying
// (recent applying_since) is NOT a candidate because cutoff is not passed, so it
// is not released even with a dead owner.
func TestIntegration_ReconcileOrphanApplying_FreshLock_NotStale(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name = "orphan-fresh"
		aid  = "01HRECFRESHAPPLY00000001"
		kid  = "keeper-dead-fresh"
	)
	// applying_since right now is NOT stale (cutoff = NOW()-90s).
	seedApplyingWithEpoch(t, ctx, pool, name, aid, kid, time.Now())

	rec := reaper.NewOrphanApplyingReconciler(pool, rc, nil, reconcileSilentLogger())
	affected, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 0 {
		t.Fatalf("affected = %d, want 0 (fresh lock is not stale)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "applying" {
		t.Errorf("status = %q, want applying", got)
	}
}

// TestIntegration_DualContour_SingleWinner guards invariant A (ADR-027 amend
// (l)+(m) on ONE incarnation): both recovery contours must NOT double-release.
// Voyage orphan release (l) and Reaper reconcile_orphan_applying (m) go through
// ONE CAS: incarnation.ReleaseApplyingOrphan (single-winner
// `WHERE status='applying'`). Therefore model the second contour by directly
// calling ReleaseApplyingOrphan. This is the shared joint of both paths; a full
// Voyage re-run in the reaper harness is disproportionally expensive because it
// would require voyageorch.Worker + voyage_targets + claim lease, while the
// proven invariant lives in the shared CAS, not in the Voyage orchestrator layer.
//
// Scenario: applying incarnation with epoch, owner dead in Conclave.
//   - contour (m): Reaper.Run releases applying->ready as the first winner;
//   - contour (l): direct repeated ReleaseApplyingOrphan with the same apply_id
//     SEES already-ready (single-winner CAS) -> ErrOrphanLockNotReleased
//     (no-op), NOT a failure error and NOT a second release.
func TestIntegration_DualContour_SingleWinner(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name = "dual-single-winner"
		aid  = "01HRECDUALWINNER00000001"
		kid  = "keeper-dual-dead"
	)
	seedApplyingWithEpoch(t, ctx, pool, name, aid, kid, time.Now().Add(-10*time.Minute))
	// KID is NOT in Conclave, so it is dead.

	// Contour (m): Reaper releases first.
	rec := reaper.NewOrphanApplyingReconciler(pool, rc, nil, reconcileSilentLogger())
	affected, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run (contour m): %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1 (contour m released first)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "ready" {
		t.Fatalf("status after contour m = %q, want ready", got)
	}
	if kidCol := incApplyingByKID(t, ctx, pool, name); kidCol != nil {
		t.Errorf("applying_by_kid after release = %q, want NULL (epoch cleared)", *kidCol)
	}

	// Contour (l): direct ReleaseApplyingOrphan with the same apply_id sees
	// already-ready. This is the shared CAS of both paths: Voyage adapter (l) and
	// reaper (m) call it identically. Expect no-op (ErrOrphanLockNotReleased),
	// NOT a repeated release.
	relErr := incarnation.ReleaseApplyingOrphan(ctx, pool, name, aid, audit.NewULID())
	if !errors.Is(relErr, incarnation.ErrOrphanLockNotReleased) {
		t.Fatalf("second contour (l) = %v, want ErrOrphanLockNotReleased (no-op, single-winner)", relErr)
	}
	// State was not changed by the second contour.
	if got := incStatus(t, ctx, pool, name); got != "ready" {
		t.Errorf("status after second contour = %q, want ready (no second release)", got)
	}

	// Symmetry: if the order were reversed, with Voyage contour (l) first, reaper
	// (m) must get the same no-op. After the row is ready, Reaper.Run simply does
	// not select it as a candidate (status='applying' SQL filter), so
	// affected==0 without error.
	affected2, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run (contour m repeated): %v", err)
	}
	if affected2 != 0 {
		t.Errorf("affected of repeated contour m = %d, want 0 (row already ready, not candidate)", affected2)
	}
}

// TestIntegration_DualContour_EpochOverwriteSkip guards invariant B (ADR-027
// amend (m)): if a Voyage re-run re-captured the incarnation and overwrote epoch
// with a new LIVE KID, rule (m) sees presence-alive for the new owner and skips,
// NOT releasing a live lock. Re-capture is modeled by directly overwriting epoch
// columns with a live KID, imitating lockRun for the new run. A full Voyage
// re-run in the reaper harness is disproportionate, and the proven behavior
// depends ONLY on applying_by_kid plus its presence in Conclave, not on the epoch
// write path.
func TestIntegration_DualContour_EpochOverwriteSkip(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name    = "dual-epoch-overwrite"
		deadAID = "01HRECDUALDEAD0000000001"
		liveAID = "01HRECDUALLIVE0000000001"
		deadKID = "keeper-dual-old-dead"
		liveKID = "keeper-dual-new-live"
	)
	// Initial orphan: dead owner, stale lock.
	seedApplyingWithEpoch(t, ctx, pool, name, deadAID, deadKID, time.Now().Add(-10*time.Minute))

	// Voyage re-run re-captured: a new LIVE owner overwrote epoch (apply_id +
	// by_kid + attempt + since) for its run. Keep applying_since stale so the row
	// passes the SQL age filter and really reaches the presence check. Otherwise
	// skip would be by "not stale", not by "live", and the test would prove the
	// wrong invariant.
	if _, err := pool.Exec(ctx, `
UPDATE incarnation
SET applying_apply_id = $2, applying_attempt = 1, applying_by_kid = $3,
    applying_since = $4
WHERE name = $1`,
		name, liveAID, liveKID, time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatalf("overwrite epoch (Voyage re-run re-capture): %v", err)
	}
	// apply_run for the new run under live apply_id (FENCING-1: its own running row).
	if _, err := pool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_at)
VALUES ($1, $2, $3, 'deploy', 'running', NOW())`,
		liveAID, name+".host-01", name); err != nil {
		t.Fatalf("seed live apply_run: %v", err)
	}
	// New owner is LIVE in Conclave.
	if err := redis.RegisterInstance(ctx, rc, liveKID, "{}", 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance(liveKID): %v", err)
	}
	t.Cleanup(func() { _ = redis.DeregisterInstance(context.Background(), rc, liveKID) })

	rec := reaper.NewOrphanApplyingReconciler(pool, rc, nil, reconcileSilentLogger())
	affected, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 0 {
		t.Fatalf("affected = %d, want 0 (re-captured epoch, live owner, skip)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "applying" {
		t.Errorf("status = %q, want applying (live re-run lock not released)", got)
	}
	// Epoch remains with the NEW live owner; the rule did not touch it.
	if kidCol := incApplyingByKID(t, ctx, pool, name); kidCol == nil || *kidCol != liveKID {
		got := "<nil>"
		if kidCol != nil {
			got = *kidCol
		}
		t.Errorf("applying_by_kid = %q, want %q (live owner's epoch was not overwritten)", got, liveKID)
	}
}
