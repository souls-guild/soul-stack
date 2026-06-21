//go:build integration

package reaper_test

// Integration-тесты Reaper-правила reconcile_orphan_applying (ADR-027 amend (m))
// поверх реальных PG + Redis-presence (Conclave). Дополняют unit-тесты
// (orphan_applying_reconcile_test.go) живой проверкой связки SQL-кандидаты →
// InstanceAlive → ReleaseApplyingOrphan на настоящих таблицах/ключах.

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

// incApplyingByKID читает applying_by_kid (epoch-владелец lock-а). nil — epoch
// обнулён (lock снят) либо строка legacy без epoch.
func incApplyingByKID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) *string {
	t.Helper()
	var kid *string
	if err := pool.QueryRow(ctx, `SELECT applying_by_kid FROM incarnation WHERE name=$1`, name).Scan(&kid); err != nil {
		t.Fatalf("read applying_by_kid %s: %v", name, err)
	}
	return kid
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

// TestIntegration_DualContour_SingleWinner — guard на инвариант A (ADR-027
// amend (l)+(m) на ОДНОЙ инкарнации): оба recovery-контура НЕ дают двойного
// снятия. Voyage-orphan-release (l) и Reaper reconcile_orphan_applying (m) идут
// через ОДИН CAS — incarnation.ReleaseApplyingOrphan (single-winner
// `WHERE status='applying'`). Поэтому второй контур моделируем прямым вызовом
// ReleaseApplyingOrphan (это и есть общий шов обоих путей; полный Voyage-re-run
// в reaper-харнессе несоразмерно дорог — потребовал бы voyageorch.Worker +
// voyage_targets + claim-lease, при том что доказываемый инвариант живёт именно
// в общем CAS, не в надстройке Voyage-оркестратора).
//
// Сценарий: applying-инкарнация с epoch, владелец мёртв в Conclave.
//   - контур (m): Reaper.Run снимает applying→ready (первый победитель);
//   - контур (l): прямой повторный ReleaseApplyingOrphan(тот же apply_id) ВИДИТ
//     уже-ready (single-winner CAS) → ErrOrphanLockNotReleased (no-op), НЕ
//     ошибка-сбой и НЕ второе снятие.
func TestIntegration_DualContour_SingleWinner(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name = "dual-single-winner"
		aid  = "01HRECDUALWINNER00000001"
		kid  = "keeper-dual-dead"
	)
	seedApplyingWithEpoch(t, ctx, pool, name, aid, kid, time.Now().Add(-10*time.Minute))
	// KID НЕ в Conclave → мёртв.

	// Контур (m): Reaper снимает первым.
	rec := reaper.NewOrphanApplyingReconciler(pool, rc, nil, reconcileSilentLogger())
	affected, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run (контур m): %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1 (контур m снял первым)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "ready" {
		t.Fatalf("status после контура m = %q, want ready", got)
	}
	if kidCol := incApplyingByKID(t, ctx, pool, name); kidCol != nil {
		t.Errorf("applying_by_kid после снятия = %q, want NULL (epoch обнулён)", *kidCol)
	}

	// Контур (l): прямой ReleaseApplyingOrphan тем же apply_id видит уже-ready.
	// Это общий CAS обоих путей — Voyage-адаптер (l) и reaper (m) зовут его
	// идентично. Ожидаем no-op (ErrOrphanLockNotReleased), НЕ повторное снятие.
	relErr := incarnation.ReleaseApplyingOrphan(ctx, pool, name, aid, audit.NewULID())
	if !errors.Is(relErr, incarnation.ErrOrphanLockNotReleased) {
		t.Fatalf("второй контур (l) = %v, want ErrOrphanLockNotReleased (no-op, single-winner)", relErr)
	}
	// Состояние не изменилось вторым контуром.
	if got := incStatus(t, ctx, pool, name); got != "ready" {
		t.Errorf("status после второго контура = %q, want ready (без второго снятия)", got)
	}

	// Симметрия: если бы порядок был обратный (первым — Voyage-контур (l)),
	// reaper (m) обязан получить тот же no-op. Reaper.Run после ready-строки
	// просто не берёт её в кандидаты (status='applying'-фильтр SQL) → affected==0,
	// без ошибки.
	affected2, err := rec.Run(ctx, 90*time.Second, 1000)
	if err != nil {
		t.Fatalf("Run (контур m повторно): %v", err)
	}
	if affected2 != 0 {
		t.Errorf("affected повторного контура m = %d, want 0 (строка уже ready, не кандидат)", affected2)
	}
}

// TestIntegration_DualContour_EpochOverwriteSkip — guard на инвариант B (ADR-027
// amend (m)): если Voyage re-run перезахватил инкарнацию и переписал epoch новым
// ЖИВЫМ KID, правило (m) видит presence-alive нового владельца → skip (НЕ снимает
// живой lock). Перезахват моделируем прямой перезаписью epoch-колонок на живой
// KID (имитация lockRun нового прогона) — полный Voyage-re-run в reaper-харнессе
// несоразмерен, а доказываемое поведение зависит ТОЛЬКО от значения
// applying_by_kid + его presence в Conclave, не от пути записи epoch.
func TestIntegration_DualContour_EpochOverwriteSkip(t *testing.T) {
	ctx, pool, rc := newReconcileFixture(t)
	const (
		name    = "dual-epoch-overwrite"
		deadAID = "01HRECDUALDEAD0000000001"
		liveAID = "01HRECDUALLIVE0000000001"
		deadKID = "keeper-dual-old-dead"
		liveKID = "keeper-dual-new-live"
	)
	// Исходный orphan: мёртвый владелец, stale lock.
	seedApplyingWithEpoch(t, ctx, pool, name, deadAID, deadKID, time.Now().Add(-10*time.Minute))

	// Voyage re-run перезахватил: новый ЖИВОЙ владелец перезаписал epoch
	// (apply_id + by_kid + attempt + since) под своим прогоном. applying_since
	// держим stale, чтобы строка прошла age-фильтр SQL и реально дошла до
	// presence-чека (иначе skip был бы по «не stale», а не по «жив» — тест
	// доказывал бы не тот инвариант).
	if _, err := pool.Exec(ctx, `
UPDATE incarnation
SET applying_apply_id = $2, applying_attempt = 1, applying_by_kid = $3,
    applying_since = $4
WHERE name = $1`,
		name, liveAID, liveKID, time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatalf("overwrite epoch (Voyage re-run перезахват): %v", err)
	}
	// apply_run нового прогона под live apply_id (FENCING-1: своя running-строка).
	if _, err := pool.Exec(ctx, `
INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status, started_at)
VALUES ($1, $2, $3, 'deploy', 'running', NOW())`,
		liveAID, name+".host-01", name); err != nil {
		t.Fatalf("seed live apply_run: %v", err)
	}
	// Новый владелец ЖИВ в Conclave.
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
		t.Fatalf("affected = %d, want 0 (перезахваченный epoch — живой владелец, skip)", affected)
	}
	if got := incStatus(t, ctx, pool, name); got != "applying" {
		t.Errorf("status = %q, want applying (живой re-run lock не снят)", got)
	}
	// Epoch остался у НОВОГО (живого) владельца — правило его не тронуло.
	if kidCol := incApplyingByKID(t, ctx, pool, name); kidCol == nil || *kidCol != liveKID {
		got := "<nil>"
		if kidCol != nil {
			got = *kidCol
		}
		t.Errorf("applying_by_kid = %q, want %q (epoch живого владельца не перетёрт)", got, liveKID)
	}
}
