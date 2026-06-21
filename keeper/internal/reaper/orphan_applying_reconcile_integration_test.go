//go:build integration

package reaper_test

// Integration-тесты Reaper-правила reconcile_orphan_applying (ADR-027 amend (m))
// поверх реальных PG + Redis-presence (Conclave). Дополняют unit-тесты
// (orphan_applying_reconcile_test.go) живой проверкой связки SQL-кандидаты →
// InstanceAlive → ReleaseApplyingOrphan на настоящих таблицах/ключах.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
)

func reconcileSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// seedApplyingWithEpoch создаёт incarnation в applying с заполненным epoch
// (имитирует scenario.lockApplyingWithEpoch) + одну running apply_runs-строку
// её apply_id (без этого FENCING-1 не отличить orphan от собственного прогона —
// строка с ТЕМ ЖЕ apply_id не блокирует снятие).
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

// reconcileFixture поднимает PG + Redis (skip без контейнеров), чистит incarnation
// и Conclave-ключи перед тестом.
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

	rc, err := redis.NewClient(ctx, redis.Config{Addr: integrationRedisAddr})
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	if _, err := integrationPool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}
	return ctx, integrationPool, rc
}

// TestIntegration_ReconcileOrphanApplying_DeadOwner_Released — (f) presence-мёртвый
// (KID НЕ зарегистрирован в Conclave), без чужого rival → applying снят, epoch
// обнулён в NULL.
func TestIntegration_ReconcileOrphanApplying_DeadOwner_Released(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name = "orphan-dead"
		aid  = "01HRECDEADAPPLY000000001"
		kid  = "keeper-dead-int"
	)
	// applying_since далеко в прошлом → проходит stale-фильтр (cutoff=NOW()-90s).
	seedApplyingWithEpoch(t, ctx, pool, name, aid, kid, time.Now().Add(-10*time.Minute))
	// KID НЕ регистрируем в Conclave → InstanceAlive(kid)=false (мёртв).

	rec := reaper.NewOrphanApplyingReconciler(pool, rc, nil, reconcileSilentLogger())
	affected, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1 (мёртвый владелец, lock снят)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "ready" {
		t.Errorf("status = %q, want ready", got)
	}
	if !incEpochNull(t, ctx, pool, name) {
		t.Error("epoch не обнулён после снятия")
	}
}

// TestIntegration_ReconcileOrphanApplying_AliveOwner_NotReclaimed — (c) presence-
// живой владелец (KID зарегистрирован в Conclave) → НЕ снят (split-brain guard).
func TestIntegration_ReconcileOrphanApplying_AliveOwner_NotReclaimed(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name = "orphan-alive"
		aid  = "01HRECALIVEAPPLY00000001"
		kid  = "keeper-alive-int"
	)
	seedApplyingWithEpoch(t, ctx, pool, name, aid, kid, time.Now().Add(-10*time.Minute))
	// KID ЖИВ в Conclave.
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
		t.Fatalf("affected = %d, want 0 (живой владелец — прогон идёт)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "applying" {
		t.Errorf("status = %q, want applying (живой lock не тронут)", got)
	}
}

// TestIntegration_ReconcileOrphanApplying_NullEpoch_NotReclaimed — (b) applying БЕЗ
// epoch (applying_by_kid NULL, legacy/pre-082) → SQL-фильтр НЕ берёт в кандидаты,
// правило НЕ реклеймит. Имитируем UPDATE-ом applying без epoch (минуя lockRun).
func TestIntegration_ReconcileOrphanApplying_NullEpoch_NotReclaimed(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const name = "orphan-null-epoch"
	// applying БЕЗ epoch-колонок (NULL) — legacy-строка.
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
		t.Fatalf("affected = %d, want 0 (NULL-epoch не реклеймится — known-gap)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "applying" {
		t.Errorf("status = %q, want applying (NULL-epoch не тронут)", got)
	}
}

// TestIntegration_ReconcileOrphanApplying_FreshLock_NotStale — applying свежий
// (applying_since недавно) → НЕ кандидат (cutoff не пройден), не снят даже при
// мёртвом владельце.
func TestIntegration_ReconcileOrphanApplying_FreshLock_NotStale(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name = "orphan-fresh"
		aid  = "01HRECFRESHAPPLY00000001"
		kid  = "keeper-dead-fresh"
	)
	// applying_since прямо сейчас → НЕ stale (cutoff = NOW()-90s).
	seedApplyingWithEpoch(t, ctx, pool, name, aid, kid, time.Now())

	rec := reaper.NewOrphanApplyingReconciler(pool, rc, nil, reconcileSilentLogger())
	affected, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if affected != 0 {
		t.Fatalf("affected = %d, want 0 (свежий lock не stale)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "applying" {
		t.Errorf("status = %q, want applying", got)
	}
}
