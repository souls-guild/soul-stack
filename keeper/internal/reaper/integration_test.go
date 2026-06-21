//go:build integration

package reaper_test

// Integration-тесты Purger-а под testcontainers (M0.4.1a + Reaper.b).
// Включается build-tag-ом `integration` — в дефолтном build файл не
// компилируется, поэтому отсутствие testcontainers-обвязки в момент
// мерджа M0.4.1c не ломает `make build` / `make test`. После merge
// M0.4.1a `go test -tags=integration ./keeper/...` будет запускать его
// вместе с другими integration-тестами.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

var (
	integrationPool      *pgxpool.Pool
	integrationRedisAddr string

	// integrationVaultClient / integrationVaultAPI заполняются в TestMain,
	// если testcontainer-Vault поднялся (best-effort, как Redis). Нужны только
	// vaultreconcile_integration_test.go; PG-тесты работают и без Vault.
	integrationVaultClient *keepervault.Client
	integrationVaultAPI    *vaultapi.Client
)

// vaultIntegrationImage / vaultIntegrationToken — version-pin dev-Vault-а,
// совпадает с keeper/internal/vault/integration_test.go.
const (
	vaultIntegrationImage = "hashicorp/vault:1.18"
	vaultIntegrationToken = "root"
)

// TestMain — стандартный pattern с отдельной run()-функцией: defer-ы
// внутри run() отрабатывают до os.Exit, в отличие от inline-варианта в
// TestMain (см. https://pkg.go.dev/testing#hdr-Main). Один контейнер на
// все интеграционные тесты пакета — между тестами `resetIdentityTables`.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("reaper integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("reaper integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	// Redis нужен только runner_integration_test.go: per-rule SQL-функции
	// тестируются без Redis. На skip-сценарии Postgres-тесты должны
	// продолжать работать — поэтому ошибку поднятия Redis-контейнера
	// логируем (fatal-only под REQUIRE_DOCKER), но не возвращаем 1.
	redisCtr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		if requireDocker() {
			log.Fatalf("reaper integration: redis setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("reaper integration: redis unavailable, runner tests will skip: %v", err)
	} else {
		defer func() {
			termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer termCancel()
			_ = redisCtr.Terminate(termCtx)
		}()
		host, hErr := redisCtr.Host(ctx)
		port, pErr := redisCtr.MappedPort(ctx, "6379/tcp")
		if hErr != nil || pErr != nil {
			log.Printf("reaper integration: redis endpoint: host=%v port=%v", hErr, pErr)
		} else {
			integrationRedisAddr = fmt.Sprintf("%s:%s", host, port.Port())
		}
	}

	// Vault — нужен только vaultreconcile_integration_test.go (правило
	// reap_orphan_vault_keys). Best-effort, как Redis: ошибку поднятия
	// логируем (fatal-only под REQUIRE_DOCKER), Postgres-тесты продолжают.
	vaultCtr, err := tcvault.Run(ctx, vaultIntegrationImage, tcvault.WithToken(vaultIntegrationToken))
	if err != nil {
		if requireDocker() {
			log.Fatalf("reaper integration: vault setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("reaper integration: vault unavailable, vaultreconcile tests will skip: %v", err)
	} else {
		defer func() {
			termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer termCancel()
			_ = vaultCtr.Terminate(termCtx)
		}()
		vaultAddr, aErr := vaultCtr.HttpHostAddress(ctx)
		if aErr != nil {
			log.Printf("reaper integration: vault HttpHostAddress: %v", aErr)
		} else {
			apiCfg := vaultapi.DefaultConfig()
			apiCfg.Address = vaultAddr
			if api, nErr := vaultapi.NewClient(apiCfg); nErr == nil {
				api.SetToken(vaultIntegrationToken)
				integrationVaultAPI = api
				if vc, cErr := keepervault.NewClient(ctx, config.KeeperVault{
					Addr:    vaultAddr,
					Token:   vaultIntegrationToken,
					KVMount: "secret",
				}); cErr == nil {
					integrationVaultClient = vc
				} else {
					log.Printf("reaper integration: keepervault.NewClient: %v", cErr)
				}
			} else {
				log.Printf("reaper integration: vaultapi.NewClient: %v", nErr)
			}
		}
	}

	return m.Run()
}

// fixturePool возвращает общий pgxpool.Pool, инициализированный в TestMain
// через testcontainers (один контейнер на все интеграционные тесты пакета).
// `defer pool.Close()` в caller-е — no-op: pool разделяется между
// тестами и закрывается в run() через defer.
func fixturePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if integrationPool == nil {
		t.Skip("integrationPool is nil (docker unavailable, REQUIRE_DOCKER not set)")
	}
	return integrationPool
}

// fakeSHA256Hex генерирует синтетический hash для test-данных
// bootstrap_tokens.token_hash. Использует SHA-256 от seed-строки —
// гарантирует уникальность hash-а на разных seed-ах + правильный
// формат (64 hex char), необходимый CHECK constraint-у в 008.
func fakeSHA256Hex(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// seedSoul вставляет минимальную строку souls. Поля registered_at /
// last_seen_at / status задаёт caller. transport='agent' по умолчанию.
func seedSoul(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sid, status string, registeredAt time.Time, lastSeenAt *time.Time) {
	t.Helper()
	const q = `INSERT INTO souls (sid, transport, status, registered_at, last_seen_at)
		VALUES ($1, 'agent', $2, $3, $4)`
	if _, err := pool.Exec(ctx, q, sid, status, registeredAt, lastSeenAt); err != nil {
		t.Fatalf("seed soul %s: %v", sid, err)
	}
}

// seedToken вставляет bootstrap_tokens-строку для уже существующего sid.
// Если usedAt nil — токен «pending» (used_at IS NULL), иначе «used».
func seedToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sid string, createdAt, expiresAt time.Time, usedAt *time.Time, seed string) {
	t.Helper()
	const q = `INSERT INTO bootstrap_tokens
		(sid, token_hash, created_at, expires_at, used_at)
		VALUES ($1, $2, $3, $4, $5)`
	if _, err := pool.Exec(ctx, q, sid, fakeSHA256Hex(seed), createdAt, expiresAt, usedAt); err != nil {
		t.Fatalf("seed token sid=%s seed=%s: %v", sid, seed, err)
	}
}

// seedSeed вставляет soul_seeds-строку для уже существующего sid.
func seedSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sid, status string, issuedAt time.Time, seed string) {
	t.Helper()
	const q = `INSERT INTO soul_seeds
		(sid, fingerprint, serial_number, issued_at, expires_at, status)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := pool.Exec(ctx, q, sid,
		fakeSHA256Hex(seed+":fingerprint"),
		seed+":serial",
		issuedAt,
		issuedAt.Add(365*24*time.Hour),
		status,
	); err != nil {
		t.Fatalf("seed soul_seed sid=%s seed=%s: %v", sid, seed, err)
	}
}

// seedIncarnation вставляет минимальную строку incarnation для FK-привязки
// apply_runs. status='ready' — терминальный валидный enum.
func seedIncarnation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	const q = `INSERT INTO incarnation (name, service, service_version, status)
		VALUES ($1, 'svc-test', 'v1.0.0', 'ready')`
	if _, err := pool.Exec(ctx, q, name); err != nil {
		t.Fatalf("seed incarnation %s: %v", name, err)
	}
}

// seedApplyRun вставляет строку apply_runs для существующей incarnation.
// finishedAt nil — прогон ещё running (под purge_apply_runs не попадает);
// иначе status — терминальный (success/failed/cancelled) и finished_at
// задаёт caller.
func seedApplyRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applyID, sid, incarnation, status string, startedAt time.Time, finishedAt *time.Time) {
	t.Helper()
	const q = `INSERT INTO apply_runs
		(apply_id, sid, incarnation_name, scenario, status, started_at, finished_at)
		VALUES ($1, $2, $3, 'deploy', $4, $5, $6)`
	if _, err := pool.Exec(ctx, q, applyID, sid, incarnation, status, startedAt, finishedAt); err != nil {
		t.Fatalf("seed apply_run apply_id=%s sid=%s: %v", applyID, sid, err)
	}
}

// seedClaimedApplyRun вставляет строку apply_runs под Ward-claim (миграция 025)
// для recovery-теста: произвольный статус (claimed/dispatched/running/…) с
// заданным владельцем claim_by_kid, lease claim_expires_at и fencing-epoch
// attempt. claimExpiresAt в прошлом — Ward протух; в будущем — Ward живой.
// finished_at IS NULL.
func seedClaimedApplyRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applyID, sid, incarnation, status string, attempt int, claimExpiresAt time.Time) {
	t.Helper()
	const q = `INSERT INTO apply_runs
		(apply_id, sid, incarnation_name, scenario, status, started_at,
		 claim_by_kid, claim_at, claim_expires_at, attempt)
		VALUES ($1, $2, $3, 'deploy', $4, $5, $6, $7, $8, $9)`
	startedAt := claimExpiresAt.Add(-time.Hour)
	if _, err := pool.Exec(ctx, q, applyID, sid, incarnation, status, startedAt,
		"keeper-dead-01", startedAt, claimExpiresAt, attempt); err != nil {
		t.Fatalf("seed claimed apply_run apply_id=%s sid=%s: %v", applyID, sid, err)
	}
}

// applyRunSnapshot читает поля, нужные recovery-тесту, по (apply_id, sid).
func applyRunSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applyID, sid string) (status string, attempt int, claimByKID *string) {
	t.Helper()
	const q = `SELECT status, attempt, claim_by_kid FROM apply_runs WHERE apply_id = $1 AND sid = $2`
	if err := pool.QueryRow(ctx, q, applyID, sid).Scan(&status, &attempt, &claimByKID); err != nil {
		t.Fatalf("snapshot apply_id=%s sid=%s: %v", applyID, sid, err)
	}
	return status, attempt, claimByKID
}

// seedTaskRegister вставляет строку apply_task_register под существующий
// apply_run (FK на apply_runs(apply_id, sid)). register_data — минимальный
// непустой jsonb, содержимое для purge-логики не важно (критерий — статус
// apply_run + finished_at, а не сама register-строка).
//
// plan_index — часть PK после миграции 079 (PK сменился с
// (apply_id, sid, task_idx) на (apply_id, sid, plan_index)). Сидим plan_index =
// task_idx: для линейного плана (N=1) они совпадают, ровно это и делает backfill
// 079. Без него две строки одного apply_id с разными task_idx получили бы
// DEFAULT plan_index 0 и упали в duplicate-key.
func seedTaskRegister(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applyID, sid string, taskIdx int) {
	t.Helper()
	const q = `INSERT INTO apply_task_register (apply_id, sid, task_idx, plan_index, register_data)
		VALUES ($1, $2, $3, $3, '{"rc": 0}'::jsonb)`
	if _, err := pool.Exec(ctx, q, applyID, sid, taskIdx); err != nil {
		t.Fatalf("seed task_register apply_id=%s sid=%s task_idx=%d: %v", applyID, sid, taskIdx, err)
	}
}

// resetIdentityTables очищает souls / bootstrap_tokens / soul_seeds между
// под-тестами одного `t.Run`-namespace-а. CASCADE на FK сделает остальное.
func resetIdentityTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	// TRUNCATE с CASCADE — самый дешёвый способ. operators не трогаем
	// (FK ON DELETE SET NULL, наш FK created_by_aid в обоих таблицах
	// nullable).
	if _, err := pool.Exec(ctx, "TRUNCATE souls CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// TestIntegration_ExpirePendingSeeds — end-to-end проверка
// `expire_pending_seeds(interval, integer)`:
//
//  1. seed-souls для FK-привязки токенов.
//  2. inserts 3 pending tokens с истёкшим expires_at + 2 свежих pending +
//     1 used token.
//  3. вызов Purger.PurgeExpiredPendingTokens(0, 100).
//  4. assert: 3 удалено; в таблице — 3 (2 pending свежих + 1 used).
func TestIntegration_ExpirePendingSeeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	// Souls (один на токен, чтобы partial unique index по active SID не сорвался).
	for i := 0; i < 6; i++ {
		seedSoul(t, ctx, pool, fmt.Sprintf("host%d.example.com", i), "pending", now.Add(-2*time.Hour), nil)
	}

	// 3 pending с истёкшим expires_at (24h назад)
	for i := 0; i < 3; i++ {
		seedToken(t, ctx, pool, fmt.Sprintf("host%d.example.com", i),
			now.Add(-48*time.Hour), now.Add(-24*time.Hour), nil,
			fmt.Sprintf("expired-pending-%d", i))
	}
	// 2 pending свежих (expires_at в будущем)
	for i := 3; i < 5; i++ {
		seedToken(t, ctx, pool, fmt.Sprintf("host%d.example.com", i),
			now.Add(-1*time.Hour), now.Add(23*time.Hour), nil,
			fmt.Sprintf("fresh-pending-%d", i))
	}
	// 1 used (не должен попасть под правило)
	usedAt := now.Add(-12 * time.Hour)
	seedToken(t, ctx, pool, "host5.example.com",
		now.Add(-13*time.Hour), now.Add(-1*time.Hour), &usedAt,
		"used-token")

	p := reaper.NewPurger(pool)
	// maxAge = 1ms — практически «всё, у чего expires_at в прошлом».
	deleted, err := p.PurgeExpiredPendingTokens(ctx, time.Millisecond, 100)
	if err != nil {
		t.Fatalf("PurgeExpiredPendingTokens: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}

	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM bootstrap_tokens").Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 3 {
		t.Errorf("rows left = %d, want 3 (2 fresh pending + 1 used)", left)
	}
}

// TestIntegration_PurgeUsedTokens — end-to-end проверка
// `purge_used_tokens(interval, integer)`: удаление used-токенов старше
// max_age по полю used_at.
func TestIntegration_PurgeUsedTokens(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		seedSoul(t, ctx, pool, fmt.Sprintf("host%d.example.com", i), "connected",
			now.Add(-200*24*time.Hour), &now)
	}

	// 3 used старых (used_at = 120 дней назад)
	oldUsedAt := now.Add(-120 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		seedToken(t, ctx, pool, fmt.Sprintf("host%d.example.com", i),
			now.Add(-150*24*time.Hour), now.Add(-149*24*time.Hour), &oldUsedAt,
			fmt.Sprintf("old-used-%d", i))
	}
	// 2 used свежих (used_at = 10 дней назад)
	freshUsedAt := now.Add(-10 * 24 * time.Hour)
	for i := 3; i < 5; i++ {
		seedToken(t, ctx, pool, fmt.Sprintf("host%d.example.com", i),
			now.Add(-11*24*time.Hour), now.Add(-9*24*time.Hour), &freshUsedAt,
			fmt.Sprintf("fresh-used-%d", i))
	}

	p := reaper.NewPurger(pool)
	deleted, err := p.PurgeUsedTokens(ctx, 90*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeUsedTokens: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}

	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM bootstrap_tokens").Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 2 {
		t.Errorf("rows left = %d, want 2 (2 fresh used)", left)
	}
}

// TestIntegration_PurgeSouls — end-to-end проверка
// `purge_souls(text[], interval, integer)`: удаление souls в указанных
// статусах с возрастом старше max_age.
func TestIntegration_PurgeSouls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	oldLastSeen := now.Add(-60 * 24 * time.Hour)
	freshLastSeen := now.Add(-1 * 24 * time.Hour)

	// 2 disconnected старых, 1 expired старый, 1 disconnected свежий,
	// 1 connected (live), 1 revoked (не в списке статусов).
	seedSoul(t, ctx, pool, "old-disc-1.example.com", "disconnected", now.Add(-200*24*time.Hour), &oldLastSeen)
	seedSoul(t, ctx, pool, "old-disc-2.example.com", "disconnected", now.Add(-200*24*time.Hour), &oldLastSeen)
	seedSoul(t, ctx, pool, "old-exp.example.com", "expired", now.Add(-200*24*time.Hour), &oldLastSeen)
	seedSoul(t, ctx, pool, "fresh-disc.example.com", "disconnected", now.Add(-30*24*time.Hour), &freshLastSeen)
	seedSoul(t, ctx, pool, "live.example.com", "connected", now.Add(-90*24*time.Hour), &now)
	seedSoul(t, ctx, pool, "old-rev.example.com", "revoked", now.Add(-200*24*time.Hour), &oldLastSeen)

	// soul без last_seen_at (никогда не подключался), но старый registered_at — должен попасть под правило.
	seedSoul(t, ctx, pool, "never-connected.example.com", "disconnected", now.Add(-90*24*time.Hour), nil)

	p := reaper.NewPurger(pool)
	deleted, err := p.PurgeSouls(ctx, []string{"disconnected", "expired"}, 30*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeSouls: %v", err)
	}
	// old-disc-1, old-disc-2, old-exp, never-connected (COALESCE(NULL, registered_at) > 30d)
	if deleted != 4 {
		t.Errorf("deleted = %d, want 4", deleted)
	}

	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM souls").Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	// Остались: fresh-disc, live, old-rev (revoked не в фильтре статусов).
	if left != 3 {
		t.Errorf("souls left = %d, want 3", left)
	}
}

// TestIntegration_PurgeOldSeeds — end-to-end проверка
// `purge_old_seeds(text[], interval, integer)`: удаление soul_seeds по
// статусам superseded/expired/revoked.
func TestIntegration_PurgeOldSeeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	// Один soul, под него несколько seed-ов в разных статусах + один active.
	seedSoul(t, ctx, pool, "h1.example.com", "connected", now.Add(-365*24*time.Hour), &now)

	// 3 старых (issued_at = 120 дней назад) разных терминальных статусов.
	oldIssued := now.Add(-120 * 24 * time.Hour)
	seedSeed(t, ctx, pool, "h1.example.com", "superseded", oldIssued, "old-superseded")
	seedSeed(t, ctx, pool, "h1.example.com", "expired", oldIssued, "old-expired")
	seedSeed(t, ctx, pool, "h1.example.com", "revoked", oldIssued, "old-revoked")

	// 1 свежий superseded (issued_at = 30 дней назад)
	freshIssued := now.Add(-30 * 24 * time.Hour)
	seedSeed(t, ctx, pool, "h1.example.com", "superseded", freshIssued, "fresh-superseded")

	// 1 active (партишн-индекс гарантирует ровно один active per sid;
	// возраст не важен — статус active не в фильтре).
	seedSeed(t, ctx, pool, "h1.example.com", "active", oldIssued, "active-current")

	p := reaper.NewPurger(pool)
	deleted, err := p.PurgeOldSeeds(ctx, []string{"superseded", "expired", "revoked"}, 90*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeOldSeeds: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}

	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM soul_seeds").Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	// Остались: fresh-superseded + active.
	if left != 2 {
		t.Errorf("rows left = %d, want 2", left)
	}
}

// TestIntegration_MarkDisconnected — end-to-end проверка
// `mark_disconnected(interval, integer)`: connected с stale last_seen_at →
// disconnected.
func TestIntegration_MarkDisconnected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute)
	freshSeen := now.Add(-10 * time.Second)

	// 3 connected stale (last_seen_at > 90s назад)
	seedSoul(t, ctx, pool, "stale-1.example.com", "connected", now.Add(-1*time.Hour), &staleSeen)
	seedSoul(t, ctx, pool, "stale-2.example.com", "connected", now.Add(-1*time.Hour), &staleSeen)
	seedSoul(t, ctx, pool, "stale-3.example.com", "connected", now.Add(-1*time.Hour), &staleSeen)
	// 2 connected fresh
	seedSoul(t, ctx, pool, "fresh-1.example.com", "connected", now.Add(-1*time.Hour), &freshSeen)
	seedSoul(t, ctx, pool, "fresh-2.example.com", "connected", now.Add(-1*time.Hour), &freshSeen)
	// 1 disconnected stale (не должен трогаться — статус уже не connected)
	seedSoul(t, ctx, pool, "disc.example.com", "disconnected", now.Add(-1*time.Hour), &staleSeen)
	// 1 connected без last_seen_at (NULL < NOW()-90s → false, не трогаем)
	seedSoul(t, ctx, pool, "never-seen.example.com", "connected", now.Add(-1*time.Hour), nil)

	p := reaper.NewPurger(pool)
	updated, err := p.MarkDisconnected(ctx, 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	if updated != 3 {
		t.Errorf("updated = %d, want 3", updated)
	}

	// Перепроверим: оставшиеся connected — это 2 fresh + 1 never-seen.
	var connectedLeft int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM souls WHERE status = 'connected'").Scan(&connectedLeft); err != nil {
		t.Fatalf("count connected: %v", err)
	}
	if connectedLeft != 3 {
		t.Errorf("connected left = %d, want 3 (2 fresh + 1 never-seen)", connectedLeft)
	}

	var disconnected int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM souls WHERE status = 'disconnected'").Scan(&disconnected); err != nil {
		t.Fatalf("count disconnected: %v", err)
	}
	// 3 stale-* + 1 изначально disconnected = 4
	if disconnected != 4 {
		t.Errorf("disconnected = %d, want 4", disconnected)
	}
}

// redisLeaseChecker — обёртка над Redis-клиентом под reaper-проверку «жив ли
// EventStream к SID» (зеркалит soulLeaseChecker из cmd/keeper). External
// test-пакет не может назвать unexported-интерфейс reaper.soulLeaseChecker,
// но передаёт значение, удовлетворяющее ему (implicit satisfaction).
type redisLeaseChecker struct{ rc *keeperredis.Client }

func (c redisLeaseChecker) SoulStreamAlive(ctx context.Context, sid string) (bool, error) {
	return keeperredis.SoulStreamAlive(ctx, c.rc, sid)
}

// TestIntegration_MarkDisconnected_LeaseAware — lease-aware правило (ADR-006(a)):
// Soul с живым Redis SID-lease НЕ метится disconnected даже при stale PG
// last_seen_at (idle-Soul на живом стриме); реально протухший (нет lease) —
// метится. Покрывает ключевой инвариант части 3.
func TestIntegration_MarkDisconnected_LeaseAware(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if integrationRedisAddr == "" {
		t.Skip("integrationRedisAddr is empty (redis container unavailable, REQUIRE_DOCKER not set)")
	}
	resetIdentityTables(t, ctx, pool)

	rc, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: integrationRedisAddr})
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = rc.Close() }()

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute) // > 90s → кандидат

	// idle-host: connected, stale last_seen_at, НО живой стрим (есть lease).
	seedSoul(t, ctx, pool, "idle-live.example.com", "connected", now.Add(-time.Hour), &staleSeen)
	// dead-host: connected, stale last_seen_at, lease-а НЕТ (реально протух).
	seedSoul(t, ctx, pool, "dead.example.com", "connected", now.Add(-time.Hour), &staleSeen)

	// Захватываем lease только для idle-live (имитация живого EventStream).
	lease, err := keeperredis.AcquireSoulLease(ctx, rc, "idle-live.example.com", "kid-live", time.Minute)
	if err != nil {
		t.Fatalf("AcquireSoulLease: %v", err)
	}
	defer func() { _ = lease.Release(ctx) }()

	p := reaper.NewPurgerWithLease(pool, redisLeaseChecker{rc: rc}, nil)
	updated, err := p.MarkDisconnected(ctx, 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected (lease-aware): %v", err)
	}
	// Только dead помечен; idle-live спасён живым lease-ом.
	if updated != 1 {
		t.Errorf("updated = %d, want 1 (только dead)", updated)
	}

	var idleStatus, deadStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'idle-live.example.com'").Scan(&idleStatus); err != nil {
		t.Fatalf("scan idle status: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'dead.example.com'").Scan(&deadStatus); err != nil {
		t.Fatalf("scan dead status: %v", err)
	}
	if idleStatus != "connected" {
		t.Errorf("idle-live status = %q, want connected (живой lease спасает от ложного disconnect)", idleStatus)
	}
	if deadStatus != "disconnected" {
		t.Errorf("dead status = %q, want disconnected (нет lease → реально протух)", deadStatus)
	}
}

// TestIntegration_MarkDisconnected_LeaseAware_Reconnect — обратное направление
// reconcile (фикс латча снимка): disconnected-снимок с ЖИВЫМ Redis-lease
// возвращается в connected (реконнект уже-онбордированного Soul-а Bootstrap-RPC
// не трогает, eventstream presence в PG не пишет — снимок чинит только Жнец).
// disconnected БЕЗ lease (реально offline) остаётся disconnected.
func TestIntegration_MarkDisconnected_LeaseAware_Reconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if integrationRedisAddr == "" {
		t.Skip("integrationRedisAddr is empty (redis container unavailable, REQUIRE_DOCKER not set)")
	}
	resetIdentityTables(t, ctx, pool)

	rc, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: integrationRedisAddr})
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = rc.Close() }()

	now := time.Now().UTC()
	// Снимок латчился в disconnected, но last_seen_at свежий (Operator-API
	// противоречие, которое чинит обратное направление).
	freshSeen := now.Add(-5 * time.Second)

	// back-online: disconnected-снимок, реально online (есть lease) → reconnect.
	seedSoul(t, ctx, pool, "back-online.example.com", "disconnected", now.Add(-time.Hour), &freshSeen)
	// still-offline: disconnected, lease-а НЕТ (реально offline) → остаётся disconnected.
	seedSoul(t, ctx, pool, "still-offline.example.com", "disconnected", now.Add(-time.Hour), &freshSeen)

	lease, err := keeperredis.AcquireSoulLease(ctx, rc, "back-online.example.com", "kid-live", time.Minute)
	if err != nil {
		t.Fatalf("AcquireSoulLease: %v", err)
	}
	defer func() { _ = lease.Release(ctx) }()

	p := reaper.NewPurgerWithLease(pool, redisLeaseChecker{rc: rc}, nil)
	updated, err := p.MarkDisconnected(ctx, 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected (reconnect): %v", err)
	}
	// Только back-online возвращён в connected.
	if updated != 1 {
		t.Errorf("updated = %d, want 1 (только back-online)", updated)
	}

	var backStatus, stillStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'back-online.example.com'").Scan(&backStatus); err != nil {
		t.Fatalf("scan back status: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'still-offline.example.com'").Scan(&stillStatus); err != nil {
		t.Fatalf("scan still status: %v", err)
	}
	if backStatus != "connected" {
		t.Errorf("back-online status = %q, want connected (живой lease → reconnect, латч снят)", backStatus)
	}
	if stillStatus != "disconnected" {
		t.Errorf("still-offline status = %q, want disconnected (нет lease → реально offline)", stillStatus)
	}
}

// TestIntegration_MarkDisconnected_LeaseAware_Bidirectional — оба направления за
// один прогон: connected+stale+no-lease → disconnected; disconnected+live-lease →
// connected. Возврат = сумма.
func TestIntegration_MarkDisconnected_LeaseAware_Bidirectional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if integrationRedisAddr == "" {
		t.Skip("integrationRedisAddr is empty (redis container unavailable, REQUIRE_DOCKER not set)")
	}
	resetIdentityTables(t, ctx, pool)

	rc, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: integrationRedisAddr})
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = rc.Close() }()

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute)

	// going-down: connected, stale, lease-а НЕТ → disconnect.
	seedSoul(t, ctx, pool, "going-down.example.com", "connected", now.Add(-time.Hour), &staleSeen)
	// coming-back: disconnected, живой lease → reconnect.
	seedSoul(t, ctx, pool, "coming-back.example.com", "disconnected", now.Add(-time.Hour), &staleSeen)

	lease, err := keeperredis.AcquireSoulLease(ctx, rc, "coming-back.example.com", "kid-live", time.Minute)
	if err != nil {
		t.Fatalf("AcquireSoulLease: %v", err)
	}
	defer func() { _ = lease.Release(ctx) }()

	p := reaper.NewPurgerWithLease(pool, redisLeaseChecker{rc: rc}, nil)
	updated, err := p.MarkDisconnected(ctx, 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected (bidirectional): %v", err)
	}
	if updated != 2 {
		t.Errorf("updated = %d, want 2 (1 disconnect + 1 reconnect)", updated)
	}

	var downStatus, backStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'going-down.example.com'").Scan(&downStatus); err != nil {
		t.Fatalf("scan going-down status: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'coming-back.example.com'").Scan(&backStatus); err != nil {
		t.Fatalf("scan coming-back status: %v", err)
	}
	if downStatus != "disconnected" {
		t.Errorf("going-down status = %q, want disconnected", downStatus)
	}
	if backStatus != "connected" {
		t.Errorf("coming-back status = %q, want connected", backStatus)
	}
}

// errLeaseChecker — soulLeaseChecker, всегда возвращающий ошибку Redis-проверки.
// Имитирует недоступный Redis для fail-safe-теста: ни одна сторона снимка не
// двигается (живой стрим важнее своевременности снимка).
type errLeaseChecker struct{}

func (errLeaseChecker) SoulStreamAlive(_ context.Context, _ string) (bool, error) {
	return false, fmt.Errorf("redis unavailable (fail-safe test)")
}

// TestIntegration_MarkDisconnected_LeaseAware_FailSafeRedisError — при ошибке
// Redis-проверки reconcile НЕ метит ни в одну сторону: connected-кандидат
// остаётся connected, disconnected-кандидат остаётся disconnected. Прогон сам
// не падает (возврат 0, без error) — следующий тик повторит.
func TestIntegration_MarkDisconnected_LeaseAware_FailSafeRedisError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute)

	// connected+stale: без Redis-ответа disconnect нельзя — fail-safe keep.
	seedSoul(t, ctx, pool, "stale-conn.example.com", "connected", now.Add(-time.Hour), &staleSeen)
	// disconnected: без Redis-ответа reconnect нельзя — fail-safe keep.
	seedSoul(t, ctx, pool, "disc.example.com", "disconnected", now.Add(-time.Hour), &staleSeen)

	p := reaper.NewPurgerWithLease(pool, errLeaseChecker{}, nil)
	updated, err := p.MarkDisconnected(ctx, 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected (fail-safe redis error): %v", err)
	}
	if updated != 0 {
		t.Errorf("updated = %d, want 0 (Redis-ошибка → не метить ни одну сторону)", updated)
	}

	var connStatus, discStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'stale-conn.example.com'").Scan(&connStatus); err != nil {
		t.Fatalf("scan conn status: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'disc.example.com'").Scan(&discStatus); err != nil {
		t.Fatalf("scan disc status: %v", err)
	}
	if connStatus != "connected" {
		t.Errorf("stale-conn status = %q, want connected (fail-safe keep)", connStatus)
	}
	if discStatus != "disconnected" {
		t.Errorf("disc status = %q, want disconnected (fail-safe keep)", discStatus)
	}
}

// TestIntegration_MarkDisconnected_FallbackNoLease — fallback lease==nil
// (Purger без lease-checker-а): правило ОДНОСТОРОННЕЕ чисто-SQL mark_disconnected
// (миграция 014) — connected+stale → disconnected, disconnected обратно НЕ
// поднимается (нет Redis для определения online). Подтверждает сохранённое
// поведение при незаконфигуренном Redis.
func TestIntegration_MarkDisconnected_FallbackNoLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute)

	seedSoul(t, ctx, pool, "stale-conn.example.com", "connected", now.Add(-time.Hour), &staleSeen)
	seedSoul(t, ctx, pool, "disc.example.com", "disconnected", now.Add(-time.Hour), &staleSeen)

	// NewPurger без lease → fallback на чисто-SQL одностороннее правило.
	p := reaper.NewPurger(pool)
	updated, err := p.MarkDisconnected(ctx, 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected (fallback): %v", err)
	}
	// Только connected+stale метится; disconnected обратно не поднимается.
	if updated != 1 {
		t.Errorf("updated = %d, want 1 (только stale-conn; reconnect недоступен без Redis)", updated)
	}

	var connStatus, discStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'stale-conn.example.com'").Scan(&connStatus); err != nil {
		t.Fatalf("scan conn status: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'disc.example.com'").Scan(&discStatus); err != nil {
		t.Fatalf("scan disc status: %v", err)
	}
	if connStatus != "disconnected" {
		t.Errorf("stale-conn status = %q, want disconnected (одностороннее SQL-правило)", connStatus)
	}
	if discStatus != "disconnected" {
		t.Errorf("disc status = %q, want disconnected (fallback не поднимает обратно)", discStatus)
	}
}

// TestIntegration_PurgeApplyRuns — end-to-end проверка
// `purge_apply_runs(interval, integer)`: удаление finished apply_runs
// (success/failed/cancelled/orphaned/no_match) старше max_age; running и свежие —
// на месте. no_match (FINDING-01 вариант (б)) несёт finished_at и обязан
// purge-иться наравне с прочими терминалами, иначе строки нецелевых хостов
// копились бы вечно.
func TestIntegration_PurgeApplyRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}

	seedIncarnation(t, ctx, pool, "inc-1")

	now := time.Now().UTC()
	oldFinished := now.Add(-60 * 24 * time.Hour)
	freshFinished := now.Add(-1 * time.Hour)

	// 5 старых finished разных терминальных статусов — попадут под правило
	// (success/failed/cancelled/orphaned/no_match — все несут finished_at).
	seedApplyRun(t, ctx, pool, "old-success", "h1.example.com", "inc-1", "success", oldFinished.Add(-time.Hour), &oldFinished)
	seedApplyRun(t, ctx, pool, "old-failed", "h1.example.com", "inc-1", "failed", oldFinished.Add(-time.Hour), &oldFinished)
	seedApplyRun(t, ctx, pool, "old-cancelled", "h1.example.com", "inc-1", "cancelled", oldFinished.Add(-time.Hour), &oldFinished)
	seedApplyRun(t, ctx, pool, "old-orphaned", "h1.example.com", "inc-1", "orphaned", oldFinished.Add(-time.Hour), &oldFinished)
	seedApplyRun(t, ctx, pool, "old-no-match", "h1.example.com", "inc-1", "no_match", oldFinished.Add(-time.Hour), &oldFinished)
	// 1 свежий finished — НЕ старше max_age, остаётся.
	seedApplyRun(t, ctx, pool, "fresh-success", "h1.example.com", "inc-1", "success", freshFinished.Add(-time.Hour), &freshFinished)
	// 1 старый running (finished_at IS NULL) — НИКОГДА не удаляется.
	seedApplyRun(t, ctx, pool, "old-running", "h1.example.com", "inc-1", "running", oldFinished.Add(-time.Hour), nil)

	p := reaper.NewPurger(pool)
	// max_age = 30d → 5 старых finished попадают, fresh (1h) и running — нет.
	deleted, err := p.PurgeApplyRuns(ctx, 30*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeApplyRuns: %v", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5", deleted)
	}

	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM apply_runs").Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	// Остались: fresh-success + old-running.
	if left != 2 {
		t.Errorf("rows left = %d, want 2 (fresh finished + running)", left)
	}
	// Running должен пережить любой purge.
	var running int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM apply_runs WHERE status = 'running'").Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != 1 {
		t.Errorf("running left = %d, want 1 (running never purged)", running)
	}
}

// TestIntegration_PurgeApplyTaskRegister — end-to-end проверка
// `purge_apply_task_register(interval, integer)`: удаление register-строк
// прогонов в терминальном статусе старше grace; register активных (running)
// и свежих терминальных прогонов остаётся.
func TestIntegration_PurgeApplyTaskRegister(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}

	seedIncarnation(t, ctx, pool, "inc-1")

	now := time.Now().UTC()
	oldFinished := now.Add(-2 * time.Hour)
	freshFinished := now.Add(-1 * time.Minute)

	// Старый терминальный прогон (finished 2h назад) с 2 register-строками —
	// обе попадут под правило (grace 1h).
	seedApplyRun(t, ctx, pool, "old-term", "h1.example.com", "inc-1", "success", oldFinished.Add(-time.Hour), &oldFinished)
	seedTaskRegister(t, ctx, pool, "old-term", "h1.example.com", 0)
	seedTaskRegister(t, ctx, pool, "old-term", "h1.example.com", 1)

	// Свежий терминальный прогон (finished 1m назад) с register — НЕ старше
	// grace 1h, register остаётся (scenario-runner мог ещё не дочитать).
	seedApplyRun(t, ctx, pool, "fresh-term", "h2.example.com", "inc-1", "success", freshFinished.Add(-time.Hour), &freshFinished)
	seedTaskRegister(t, ctx, pool, "fresh-term", "h2.example.com", 0)

	// Активный (running) прогон, стартовавший давно (started 2h назад,
	// finished_at IS NULL) с register — НИКОГДА не удаляется, scenario-runner
	// ещё дойдёт до барьера и будет читать его.
	seedApplyRun(t, ctx, pool, "running", "h3.example.com", "inc-1", "running", oldFinished.Add(-time.Hour), nil)
	seedTaskRegister(t, ctx, pool, "running", "h3.example.com", 0)

	p := reaper.NewPurger(pool)
	// grace = 1h → 2 register-строки old-term попадают; fresh-term (1m) и
	// running (нет finished_at) — нет.
	deleted, err := p.PurgeApplyTaskRegister(ctx, time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeApplyTaskRegister: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (old-term × 2 task_idx)", deleted)
	}

	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM apply_task_register").Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	// Остались: fresh-term (1) + running (1).
	if left != 2 {
		t.Errorf("rows left = %d, want 2 (fresh-term + running)", left)
	}

	// register активного прогона должен пережить любой purge.
	var runningReg int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM apply_task_register WHERE apply_id = 'running'").Scan(&runningReg); err != nil {
		t.Fatalf("count running register: %v", err)
	}
	if runningReg != 1 {
		t.Errorf("running register left = %d, want 1 (running never purged)", runningReg)
	}

	// apply_runs не тронут: правило чистит только register, сам прогон
	// остаётся под purge_apply_runs (30d).
	var runsLeft int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM apply_runs").Scan(&runsLeft); err != nil {
		t.Fatalf("count apply_runs: %v", err)
	}
	if runsLeft != 3 {
		t.Errorf("apply_runs left = %d, want 3 (register-purge не трогает apply_runs)", runsLeft)
	}
}

// seedVoyageWithStatus вставляет voyage в заданном статусе/finished_at.
// Для running проставляет claim-поля (voyages_running_claim_consistency);
// для терминалов finished_at обязателен (voyages_terminal_finished_at).
// cadenceID nil → ручной прогон; populated → спавн от Cadence (для guard
// «purge детей не трогает активное расписание»).
func seedVoyageWithStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, voyageID, aid, status string, finishedAt *time.Time, cadenceID *string) {
	t.Helper()
	switch status {
	case "running":
		const q = `INSERT INTO voyages (voyage_id, kind, module, target_resolved, target_origin,
			total_batches, started_by_aid, status, claimed_by_kid, claim_expires_at, cadence_id)
			VALUES ($1, 'command', 'core.cmd.shell', '[]'::jsonb, '{}'::jsonb, 1, $2, 'running',
			        'kid-test', NOW() + INTERVAL '5 minutes', $3)`
		if _, err := pool.Exec(ctx, q, voyageID, aid, cadenceID); err != nil {
			t.Fatalf("seed voyage %s (running): %v", voyageID, err)
		}
	default:
		const q = `INSERT INTO voyages (voyage_id, kind, module, target_resolved, target_origin,
			total_batches, started_by_aid, status, finished_at, cadence_id)
			VALUES ($1, 'command', 'core.cmd.shell', '[]'::jsonb, '{}'::jsonb, 1, $2, $3, $4, $5)`
		if _, err := pool.Exec(ctx, q, voyageID, aid, status, finishedAt, cadenceID); err != nil {
			t.Fatalf("seed voyage %s (%s): %v", voyageID, status, err)
		}
	}
}

// seedVoyageTargetRow вставляет Leg-строку (FK voyage_id → voyages ON DELETE
// CASCADE) для проверки каскадного сноса при purge_voyages.
func seedVoyageTargetRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, voyageID, targetID string) {
	t.Helper()
	const q = `INSERT INTO voyage_targets (voyage_id, target_kind, target_id, batch_index, status)
		VALUES ($1, 'sid', $2, 0, 'succeeded')`
	if _, err := pool.Exec(ctx, q, voyageID, targetID); err != nil {
		t.Fatalf("seed voyage_target %s/%s: %v", voyageID, targetID, err)
	}
}

// seedCadenceRow вставляет минимальное АКТИВНОЕ расписание (interval-kind).
// Используется как guard: purge истории прогонов НЕ должен трогать cadences.
func seedCadenceRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, aid string) {
	t.Helper()
	const q = `INSERT INTO cadences (id, name, schedule_kind, interval_seconds, overlap_policy,
		kind, module, target, created_by_aid)
		VALUES ($1, $1, 'interval', 300, 'skip', 'command', 'core.cmd.shell', '[]'::jsonb, $2)`
	if _, err := pool.Exec(ctx, q, id, aid); err != nil {
		t.Fatalf("seed cadence %s: %v", id, err)
	}
}

// TestIntegration_PurgeVoyages — end-to-end проверка `purge_voyages(interval,
// integer)` (ADR-046 §79): удаление finished voyages (succeeded/failed/
// partial_failed/cancelled) старше max_age; scheduled/pending/running и свежие
// — на месте. Guard-инварианты:
//   - voyage_targets уносятся ON DELETE CASCADE (нет битых Leg-строк);
//   - активная Cadence (расписание-источник) НЕ тронута, её back-link voyage
//     удалён без сноса самого расписания (FK ON DELETE SET NULL направлен от
//     voyage к cadence — purge детей не трогает родителя);
//   - ephemeral-Tiding с voyage_id удалённого прогона остаётся валидным для
//     `purge_orphan_ephemeral_tidings` (soft-link без FK, битой ссылки нет).
func TestIntegration_PurgeVoyages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if _, err := pool.Exec(ctx,
		"TRUNCATE tidings, heralds, voyage_targets, voyages, cadences, operators RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedOperatorRaw(t, ctx, "archon-v")

	now := time.Now().UTC()
	oldFinished := now.Add(-60 * 24 * time.Hour)
	freshFinished := now.Add(-1 * time.Hour)

	// Активное расписание — guard: его дети-прогоны чистятся, но само оно нет.
	seedCadenceRow(t, ctx, pool, "cad-1", "archon-v")

	// 4 старых finished разных терминальных статусов — попадут под правило.
	for _, st := range []string{"succeeded", "failed", "partial_failed", "cancelled"} {
		seedVoyageWithStatus(t, ctx, pool, "old-"+st, "archon-v", st, &oldFinished, nil)
	}
	// Старый succeeded, спавненный от Cadence (cadence_id populated) + Leg-строка
	// — проверяем каскад voyage_targets и сохранность cadences.
	cadID := "cad-1"
	seedVoyageWithStatus(t, ctx, pool, "old-from-cadence", "archon-v", "succeeded", &oldFinished, &cadID)
	seedVoyageTargetRow(t, ctx, pool, "old-from-cadence", "h1.example.com")
	seedVoyageTargetRow(t, ctx, pool, "old-from-cadence", "h2.example.com")
	// ephemeral-Tiding (нужен Herald + soft-link voyage_id) на удаляемый прогон.
	seedHeraldRaw(t, ctx, "hook-v", "archon-v")
	seedEphemeralTiding(t, ctx, "eph-old-from-cadence", "hook-v", "old-from-cadence")

	// Свежий finished — НЕ старше max_age, остаётся.
	seedVoyageWithStatus(t, ctx, pool, "fresh-succeeded", "archon-v", "succeeded", &freshFinished, nil)
	// Незавершённые — НИКОГДА не purge.
	seedVoyageWithStatus(t, ctx, pool, "pending-old", "archon-v", "pending", nil, nil)
	seedVoyageWithStatus(t, ctx, pool, "scheduled-old", "archon-v", "scheduled", nil, nil)
	seedVoyageWithStatus(t, ctx, pool, "running-old", "archon-v", "running", nil, nil)

	p := reaper.NewPurger(pool)
	deleted, err := p.PurgeVoyages(ctx, 30*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeVoyages: %v", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5 (4 терминала + 1 from-cadence)", deleted)
	}

	// Остались: fresh-succeeded + pending + scheduled + running.
	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM voyages").Scan(&left); err != nil {
		t.Fatalf("count voyages: %v", err)
	}
	if left != 4 {
		t.Errorf("voyages left = %d, want 4 (fresh + pending + scheduled + running)", left)
	}
	// Незавершённые статусы должны пережить любой purge.
	var active int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM voyages WHERE status IN ('pending', 'scheduled', 'running')").Scan(&active); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 3 {
		t.Errorf("active voyages left = %d, want 3 (pending/scheduled/running never purged)", active)
	}

	// КАСКАД: voyage_targets удалённого прогона снесены ON DELETE CASCADE.
	var targetsLeft int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM voyage_targets WHERE voyage_id = 'old-from-cadence'").Scan(&targetsLeft); err != nil {
		t.Fatalf("count voyage_targets: %v", err)
	}
	if targetsLeft != 0 {
		t.Errorf("voyage_targets left = %d, want 0 (ON DELETE CASCADE)", targetsLeft)
	}

	// GUARD: активное расписание НЕ тронуто (purge детей не трогает родителя).
	var cadLeft int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM cadences WHERE id = 'cad-1'").Scan(&cadLeft); err != nil {
		t.Fatalf("count cadences: %v", err)
	}
	if cadLeft != 1 {
		t.Errorf("cadences left = %d, want 1 (активное расписание не purge-ится)", cadLeft)
	}

	// Soft-link ephemeral-Tiding на удалённый voyage остаётся (без FK на voyages);
	// его подберёт purge_orphan_ephemeral_tidings по предикату NOT EXISTS voyages.
	// Здесь важно лишь, что purge_voyages НЕ упал на FK и не оставил битой ссылки.
	var ephLeft int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM tidings WHERE name = 'eph-old-from-cadence'").Scan(&ephLeft); err != nil {
		t.Fatalf("count ephemeral tiding: %v", err)
	}
	if ephLeft != 1 {
		t.Errorf("ephemeral tiding left = %d, want 1 (soft-link, снимается отдельным правилом)", ephLeft)
	}
}

// TestIntegration_PurgeVoyages_BatchLimit — batch_size ограничивает размер
// одного DELETE-прохода (parity purge_apply_runs): 5 старых терминалов, batch=2
// → первый проход удаляет 2, остаются 3.
func TestIntegration_PurgeVoyages_BatchLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if _, err := pool.Exec(ctx,
		"TRUNCATE voyage_targets, voyages, operators RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedOperatorRaw(t, ctx, "archon-v")

	oldFinished := time.Now().UTC().Add(-60 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		seedVoyageWithStatus(t, ctx, pool, fmt.Sprintf("old-%d", i), "archon-v", "succeeded", &oldFinished, nil)
	}

	p := reaper.NewPurger(pool)
	deleted, err := p.PurgeVoyages(ctx, 30*24*time.Hour, 2)
	if err != nil {
		t.Fatalf("PurgeVoyages: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (batch limit)", deleted)
	}

	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM voyages").Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 3 {
		t.Errorf("voyages left = %d, want 3 (5 - batch 2)", left)
	}
}

// TestIntegration_ReclaimApplyRuns — end-to-end проверка recovery-скана
// (ADR-027 amend, S4): реклеймится ТОЛЬКО недо-доставленное (claimed с
// claim_expires_at < NOW() — умер ДО отдачи Soul-у) → planned со сбросом
// claim_by_kid/claim_at/claim_expires_at; attempt СОХРАНЯЕТСЯ. КЛЮЧЕВОЙ
// инвариант: dispatched с протухшим lease НЕ трогается (после отдачи прогоном
// владеет Soul, пере-claim = двойной apply). running тоже больше не реклеймится.
// Живые Ward (claim_expires_at > NOW) и planned/терминальные строки не трогаются.
func TestIntegration_ReclaimApplyRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}
	seedIncarnation(t, ctx, pool, "inc-1")

	now := time.Now().UTC()
	expired := now.Add(-1 * time.Minute) // lease протух
	alive := now.Add(10 * time.Minute)   // lease ещё живой

	// 1 протухший Ward, умерший ДО отдачи (claimed) — ЕДИНСТВЕННЫЙ реклеймится.
	seedClaimedApplyRun(t, ctx, pool, "zombie-claimed", "h1.example.com", "inc-1", "claimed", 1, expired)
	// КЛЮЧЕВОЙ инвариант: dispatched с протухшим lease — НЕ трогать (отдано Soul-у,
	// пере-claim = двойной apply).
	seedClaimedApplyRun(t, ctx, pool, "dispatched-expired", "h2.example.com", "inc-1", "dispatched", 2, expired)
	// running с протухшим lease (vestigial) — больше НЕ реклеймится.
	seedClaimedApplyRun(t, ctx, pool, "zombie-running", "h6.example.com", "inc-1", "running", 3, expired)
	// 1 живой Ward (claim_expires_at в будущем) — НЕ трогать.
	seedClaimedApplyRun(t, ctx, pool, "alive-claimed", "h3.example.com", "inc-1", "claimed", 1, alive)
	// 1 planned (свободно, claim-колонки NULL) — НЕ трогать (уже в очереди).
	seedApplyRun(t, ctx, pool, "free-planned", "h4.example.com", "inc-1", "planned", now.Add(-time.Hour), nil)
	// 1 терминальный success с протухшим claim_expires_at — НЕ трогать
	// (статус не claimed).
	seedClaimedApplyRun(t, ctx, pool, "done-success", "h5.example.com", "inc-1", "success", 1, expired)

	p := reaper.NewPurger(pool)
	reclaimed, err := p.ReclaimApplyRuns(ctx, time.Minute, 100)
	if err != nil {
		t.Fatalf("ReclaimApplyRuns: %v", err)
	}
	if reclaimed != 1 {
		t.Errorf("reclaimed = %d, want 1 (только zombie-claimed)", reclaimed)
	}

	// Недо-доставленный claimed вернулся в planned, владелец/lease сброшены,
	// attempt СОХРАНЁН.
	status, attempt, kid := applyRunSnapshot(t, ctx, pool, "zombie-claimed", "h1.example.com")
	if status != "planned" {
		t.Errorf("zombie-claimed: status = %q, want planned", status)
	}
	if attempt != 1 {
		t.Errorf("zombie-claimed: attempt = %d, want 1 (NOT reset — fencing-epoch)", attempt)
	}
	if kid != nil {
		t.Errorf("zombie-claimed: claim_by_kid = %v, want NULL (claim released)", *kid)
	}

	// КЛЮЧЕВОЙ инвариант: dispatched с протухшим lease НЕ тронут (Soul владеет).
	if status, _, kid := applyRunSnapshot(t, ctx, pool, "dispatched-expired", "h2.example.com"); status != "dispatched" || kid == nil {
		t.Errorf("dispatched-expired: status=%q kid=%v; want dispatched + non-NULL kid (Soul владеет после отдачи, НЕ реклеймить)", status, kid)
	}
	// running с протухшим lease НЕ тронут (vestigial, больше не реклеймится).
	if status, _, kid := applyRunSnapshot(t, ctx, pool, "zombie-running", "h6.example.com"); status != "running" || kid == nil {
		t.Errorf("zombie-running: status=%q kid=%v; want running + non-NULL kid (running больше не реклеймится)", status, kid)
	}

	// Живой Ward не тронут.
	if status, _, kid := applyRunSnapshot(t, ctx, pool, "alive-claimed", "h3.example.com"); status != "claimed" || kid == nil {
		t.Errorf("alive-claimed: status=%q kid=%v; want claimed + non-NULL kid (alive Ward untouched)", status, kid)
	}
	// planned не тронут.
	if status, _, _ := applyRunSnapshot(t, ctx, pool, "free-planned", "h4.example.com"); status != "planned" {
		t.Errorf("free-planned: status=%q; want planned (already queued)", status)
	}
	// Терминальный success не тронут.
	if status, _, _ := applyRunSnapshot(t, ctx, pool, "done-success", "h5.example.com"); status != "success" {
		t.Errorf("done-success: status=%q; want success (terminal untouched)", status)
	}
}

// TestIntegration_ReclaimApplyRuns_BatchLimit — recovery уважает batch_size:
// при batch=1 за один прогон возвращается ровно одна строка, остальные зомби
// остаются для следующего прогона (drain-pattern, как у прочих правил).
func TestIntegration_ReclaimApplyRuns_BatchLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}
	seedIncarnation(t, ctx, pool, "inc-1")

	expired := time.Now().UTC().Add(-1 * time.Minute)
	for i := 0; i < 3; i++ {
		seedClaimedApplyRun(t, ctx, pool, fmt.Sprintf("zombie-%d", i),
			fmt.Sprintf("h%d.example.com", i), "inc-1", "claimed", 1, expired)
	}

	p := reaper.NewPurger(pool)
	reclaimed, err := p.ReclaimApplyRuns(ctx, time.Minute, 1)
	if err != nil {
		t.Fatalf("ReclaimApplyRuns(batch=1): %v", err)
	}
	if reclaimed != 1 {
		t.Errorf("reclaimed = %d under batch=1, want 1", reclaimed)
	}

	var planned int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM apply_runs WHERE status = 'planned'").Scan(&planned); err != nil {
		t.Fatalf("count planned: %v", err)
	}
	if planned != 1 {
		t.Errorf("planned after batch=1 = %d, want 1 (drain-pattern)", planned)
	}
}

// TestIntegration_PurgeAuditOld — end-to-end проверка
// `purge_audit_old(interval, integer)`:
//
//  1. Inserts 5 audit-записей: 3 старше 365d, 2 свежих.
//  2. Покрывает Purger.PurgeAuditOld(365d, 100).
//  3. Assert: возвращено 3, в таблице осталось 2.
func TestIntegration_PurgeAuditOld(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)

	expiredAt := time.Now().UTC().Add(-400 * 24 * time.Hour)
	freshAt := time.Now().UTC().Add(-1 * time.Hour)

	insert := `INSERT INTO audit_log (audit_id, created_at, event_type, source, payload)
		VALUES ($1, $2, 'config.reload_succeeded', 'signal', '{}'::jsonb)`
	for i, ts := range []time.Time{expiredAt, expiredAt, expiredAt, freshAt, freshAt} {
		// audit.NewULID() — Crockford-base32 26-char ULID, совместим с
		// будущим CHECK constraint на формат audit_id (M0.6+).
		auditID := audit.NewULID()
		if _, err := pool.Exec(ctx, insert, auditID, ts); err != nil {
			t.Fatalf("insert[%d]: %v", i, err)
		}
	}

	p := reaper.NewPurger(pool)
	deleted, err := p.PurgeAuditOld(ctx, 365*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeAuditOld: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}

	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log").Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 2 {
		t.Errorf("rows left = %d, want 2", left)
	}
}

// seedStateHistory вставляет state_history-snapshot для существующей incarnation.
// scenario — произвольная метка (`deploy`, `migration`, …); at задаёт caller для
// контроля порядка ORDER BY at DESC.
func seedStateHistory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, historyID, incarnation, scenario string, at time.Time) {
	t.Helper()
	const q = `INSERT INTO state_history
		(history_id, incarnation_name, scenario, state_before, state_after, apply_id, at)
		VALUES ($1, $2, $3, '{}'::jsonb, '{}'::jsonb, $4, $5)`
	if _, err := pool.Exec(ctx, q, historyID, incarnation, scenario, historyID, at); err != nil {
		t.Fatalf("seed state_history history_id=%s: %v", historyID, err)
	}
}

// countArchivedStateHistory возвращает (всего, archived) snapshots для
// incarnation. Используется как assert-helper в integration-тестах правила
// archive_state_history.
func countArchivedStateHistory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, incarnation string) (total, archived int64) {
	t.Helper()
	const q = `SELECT COUNT(*), COUNT(*) FILTER (WHERE archived_at IS NOT NULL)
		FROM state_history WHERE incarnation_name = $1`
	if err := pool.QueryRow(ctx, q, incarnation).Scan(&total, &archived); err != nil {
		t.Fatalf("count state_history %s: %v", incarnation, err)
	}
	return total, archived
}

// TestIntegration_ArchiveStateHistory_KeepsLastN — pre-fill N+10 active
// snapshots → запускаем правило с keep_last_n=N, keep_version_bump=true (нет
// migration-снимков) → ожидаем archived_at IS NOT NULL у 10 старейших.
func TestIntegration_ArchiveStateHistory_KeepsLastN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	// state_history.incarnation_name → incarnation(name) ON DELETE CASCADE; TRUNCATE
	// CASCADE подчищает state_history между подтестами.
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}

	const incName = "svc-redis-prod"
	const keepLastN = 50
	const total = keepLastN + 10
	seedIncarnation(t, ctx, pool, incName)

	now := time.Now().UTC()
	for i := 0; i < total; i++ {
		// at — от старого к новому; (total-1-i) = старейший имеет наибольший offset.
		ts := now.Add(-time.Duration(total-1-i) * time.Minute)
		seedStateHistory(t, ctx, pool, audit.NewULID(), incName, "deploy", ts)
	}

	p := reaper.NewPurger(pool)
	got, err := p.ArchiveStateHistory(ctx, keepLastN, true, 1000)
	if err != nil {
		t.Fatalf("ArchiveStateHistory: %v", err)
	}
	if got != 10 {
		t.Errorf("archived = %d, want 10", got)
	}

	gotTotal, gotArchived := countArchivedStateHistory(t, ctx, pool, incName)
	if gotTotal != total {
		t.Errorf("total rows = %d, want %d (физическое удаление запрещено)", gotTotal, total)
	}
	if gotArchived != 10 {
		t.Errorf("archived rows = %d, want 10", gotArchived)
	}
}

// TestIntegration_ArchiveStateHistory_ExcludesVersionBump — pre-fill N+5
// snapshots, 2 из них version-bump (scenario='migration') ВНУТРИ старого
// хвоста → при keep_version_bump=true они НЕ архивируются: archived count = 3
// (не-migration хвостовые), version-bump-snapshots остаются активными.
func TestIntegration_ArchiveStateHistory_ExcludesVersionBump(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}

	const incName = "svc-postgres-stg"
	const keepLastN = 50
	const total = keepLastN + 5
	seedIncarnation(t, ctx, pool, incName)

	now := time.Now().UTC()
	// Хвост из 5 старейших (rn > 50): индексы 0..4 в порядке by at ASC; 2 из них
	// (положения 1 и 3) пишем как scenario='migration' — должны остаться активны.
	migrationPositions := map[int]bool{1: true, 3: true}
	wantArchived := int64(0)
	for i := 0; i < total; i++ {
		ts := now.Add(-time.Duration(total-1-i) * time.Minute)
		scenario := "deploy"
		isTail := i < (total - keepLastN) // первые 5 = старый хвост
		if isTail && migrationPositions[i] {
			scenario = "migration"
		} else if isTail {
			wantArchived++
		}
		seedStateHistory(t, ctx, pool, audit.NewULID(), incName, scenario, ts)
	}
	if wantArchived != 3 {
		t.Fatalf("test arithmetic: wantArchived = %d, want 3 (5 хвостовых − 2 migration)", wantArchived)
	}

	p := reaper.NewPurger(pool)
	got, err := p.ArchiveStateHistory(ctx, keepLastN, true, 1000)
	if err != nil {
		t.Fatalf("ArchiveStateHistory: %v", err)
	}
	if got != wantArchived {
		t.Errorf("archived = %d, want %d (исключаем migration-snapshots)", got, wantArchived)
	}

	// version-bump-snapshots должны остаться активными даже при rn > keepLastN.
	const q = `SELECT COUNT(*) FROM state_history
		WHERE incarnation_name = $1 AND scenario = 'migration' AND archived_at IS NULL`
	var activeMigration int64
	if err := pool.QueryRow(ctx, q, incName).Scan(&activeMigration); err != nil {
		t.Fatalf("count active migration: %v", err)
	}
	if activeMigration != 2 {
		t.Errorf("active migration snapshots = %d, want 2 (защищены keep_version_bump=true)", activeMigration)
	}
}

// TestIntegration_ArchiveStateHistory_KeepVersionBumpFalse — с keep_version_bump=false
// правило архивирует ВСЁ сверх keep_last_n, включая migration-snapshots. Покрывает
// явный opt-out оператора (например, для тестового стенда без recovery-требований).
func TestIntegration_ArchiveStateHistory_KeepVersionBumpFalse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}

	const incName = "svc-mysql-test"
	const keepLastN = 3
	seedIncarnation(t, ctx, pool, incName)

	now := time.Now().UTC()
	// 5 snapshots: 0 = старейший (migration), 1..2 = deploy, 3..4 = deploy (новейшие).
	// keep_last_n=3 → snapshots 0..1 в хвосте; при keep_version_bump=false оба архивируются.
	seedStateHistory(t, ctx, pool, audit.NewULID(), incName, "migration", now.Add(-5*time.Minute))
	seedStateHistory(t, ctx, pool, audit.NewULID(), incName, "deploy", now.Add(-4*time.Minute))
	seedStateHistory(t, ctx, pool, audit.NewULID(), incName, "deploy", now.Add(-3*time.Minute))
	seedStateHistory(t, ctx, pool, audit.NewULID(), incName, "deploy", now.Add(-2*time.Minute))
	seedStateHistory(t, ctx, pool, audit.NewULID(), incName, "deploy", now.Add(-1*time.Minute))

	p := reaper.NewPurger(pool)
	got, err := p.ArchiveStateHistory(ctx, keepLastN, false, 1000)
	if err != nil {
		t.Fatalf("ArchiveStateHistory: %v", err)
	}
	if got != 2 {
		t.Errorf("archived = %d, want 2 (включая migration при keep_version_bump=false)", got)
	}

	gotTotal, gotArchived := countArchivedStateHistory(t, ctx, pool, incName)
	if gotTotal != 5 {
		t.Errorf("total = %d, want 5 (физическое удаление запрещено)", gotTotal)
	}
	if gotArchived != 2 {
		t.Errorf("archived = %d, want 2", gotArchived)
	}
}

// TestIntegration_ArchiveStateHistory_PerIncarnation — N считается отдельно
// для каждой incarnation. 70 snapshots на A (keep=50 → archived=20) + 30
// snapshots на B (keep=50 → archived=0). Покрывает PARTITION BY в window-функции.
func TestIntegration_ArchiveStateHistory_PerIncarnation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}

	const keepLastN = 50
	seedIncarnation(t, ctx, pool, "svc-a")
	seedIncarnation(t, ctx, pool, "svc-b")

	now := time.Now().UTC()
	for i := 0; i < 70; i++ {
		seedStateHistory(t, ctx, pool, audit.NewULID(), "svc-a", "deploy", now.Add(-time.Duration(70-i)*time.Minute))
	}
	for i := 0; i < 30; i++ {
		seedStateHistory(t, ctx, pool, audit.NewULID(), "svc-b", "deploy", now.Add(-time.Duration(30-i)*time.Minute))
	}

	p := reaper.NewPurger(pool)
	got, err := p.ArchiveStateHistory(ctx, keepLastN, true, 1000)
	if err != nil {
		t.Fatalf("ArchiveStateHistory: %v", err)
	}
	if got != 20 {
		t.Errorf("archived = %d, want 20 (svc-a: 20, svc-b: 0)", got)
	}

	_, archivedA := countArchivedStateHistory(t, ctx, pool, "svc-a")
	if archivedA != 20 {
		t.Errorf("svc-a archived = %d, want 20", archivedA)
	}
	_, archivedB := countArchivedStateHistory(t, ctx, pool, "svc-b")
	if archivedB != 0 {
		t.Errorf("svc-b archived = %d, want 0", archivedB)
	}
}

// TestIntegration_ArchiveStateHistory_BatchLimit — batch ограничивает число
// soft-deleted за один прогон. При batch=5 на «хвосте» из 10 кандидатов
// архивируется ровно 5, остальное ждёт следующего прогона (drain-pattern).
// Архивируются САМЫЕ СТАРЫЕ из кандидатов (rn DESC в подзапросе).
func TestIntegration_ArchiveStateHistory_BatchLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}

	const incName = "svc-redis-batch"
	const keepLastN = 50
	seedIncarnation(t, ctx, pool, incName)

	now := time.Now().UTC()
	// 60 snapshots: 10 хвостовых (rn 51..60), 50 «живых» (rn 1..50).
	for i := 0; i < 60; i++ {
		seedStateHistory(t, ctx, pool, audit.NewULID(), incName, "deploy", now.Add(-time.Duration(60-i)*time.Minute))
	}

	p := reaper.NewPurger(pool)
	got, err := p.ArchiveStateHistory(ctx, keepLastN, true, 5)
	if err != nil {
		t.Fatalf("ArchiveStateHistory: %v", err)
	}
	if got != 5 {
		t.Errorf("first batch archived = %d, want 5", got)
	}

	// Второй прогон добивает остаток.
	got2, err := p.ArchiveStateHistory(ctx, keepLastN, true, 5)
	if err != nil {
		t.Fatalf("ArchiveStateHistory (drain): %v", err)
	}
	if got2 != 5 {
		t.Errorf("drain batch archived = %d, want 5", got2)
	}

	_, archived := countArchivedStateHistory(t, ctx, pool, incName)
	if archived != 10 {
		t.Errorf("total archived = %d, want 10", archived)
	}

	// Третий прогон — пусто (drain до 0).
	got3, err := p.ArchiveStateHistory(ctx, keepLastN, true, 5)
	if err != nil {
		t.Fatalf("ArchiveStateHistory (zero): %v", err)
	}
	if got3 != 0 {
		t.Errorf("zero batch archived = %d, want 0", got3)
	}
}
