//go:build integration

package reaper_test

// Purger integration tests under testcontainers (M0.4.1a + Reaper.b).
// Enabled by build tag `integration`: in the default build this file is not
// compiled, so missing testcontainers wiring during the M0.4.1c merge does not
// break `make build` / `make test`. After the M0.4.1a merge,
// `go test -tags=integration ./keeper/...` will run it together with other
// integration tests.

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

	// integrationVaultClient / integrationVaultAPI are populated in TestMain when
	// testcontainer Vault starts, best-effort like Redis. They are needed only by
	// vaultreconcile_integration_test.go; PG tests work without Vault.
	integrationVaultClient *keepervault.Client
	integrationVaultAPI    *vaultapi.Client
)

// vaultIntegrationImage / vaultIntegrationToken pin the dev Vault version,
// matching keeper/internal/vault/integration_test.go.
const (
	vaultIntegrationImage = "hashicorp/vault:1.18"
	vaultIntegrationToken = "root"
)

// TestMain uses the standard pattern with a separate run() function: defers
// inside run() execute before os.Exit, unlike the inline variant in TestMain;
// see https://pkg.go.dev/testing#hdr-Main. One container is shared by all
// package integration tests; `resetIdentityTables` runs between tests.
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

	// Redis is needed only by runner_integration_test.go; per-rule SQL functions
	// are tested without Redis. In skip scenarios Postgres tests should continue,
	// so log Redis container startup errors (fatal only under REQUIRE_DOCKER) but
	// do not return 1.
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

	// Vault is needed only by vaultreconcile_integration_test.go for rule
	// reap_orphan_vault_keys. Best-effort like Redis: log startup errors (fatal
	// only under REQUIRE_DOCKER), and let Postgres tests continue.
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

// fixturePool returns the shared pgxpool.Pool initialized in TestMain through
// testcontainers, one container for all package integration tests. `defer
// pool.Close()` in callers is a no-op: pool is shared between tests and closed
// in run() through defer.
func fixturePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if integrationPool == nil {
		t.Skip("integrationPool is nil (docker unavailable, REQUIRE_DOCKER not set)")
	}
	return integrationPool
}

// fakeSHA256Hex generates a synthetic hash for bootstrap_tokens.token_hash test
// data. It uses SHA-256 of the seed string, guaranteeing unique hashes for
// different seeds plus the correct 64 hex char format required by CHECK
// constraint in 008.
func fakeSHA256Hex(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// seedSoul inserts a minimal souls row. caller sets registered_at / last_seen_at
// / status. transport defaults to 'agent'.
func seedSoul(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sid, status string, registeredAt time.Time, lastSeenAt *time.Time) {
	t.Helper()
	const q = `INSERT INTO souls (sid, transport, status, registered_at, last_seen_at)
		VALUES ($1, 'agent', $2, $3, $4)`
	if _, err := pool.Exec(ctx, q, sid, status, registeredAt, lastSeenAt); err != nil {
		t.Fatalf("seed soul %s: %v", sid, err)
	}
}

// seedToken inserts a bootstrap_tokens row for an existing sid. If usedAt is
// nil, the token is pending (used_at IS NULL); otherwise it is used.
func seedToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sid string, createdAt, expiresAt time.Time, usedAt *time.Time, seed string) {
	t.Helper()
	const q = `INSERT INTO bootstrap_tokens
		(sid, token_hash, created_at, expires_at, used_at)
		VALUES ($1, $2, $3, $4, $5)`
	if _, err := pool.Exec(ctx, q, sid, fakeSHA256Hex(seed), createdAt, expiresAt, usedAt); err != nil {
		t.Fatalf("seed token sid=%s seed=%s: %v", sid, seed, err)
	}
}

// seedSeed inserts a soul_seeds row for an existing sid.
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

// seedIncarnation inserts a minimal incarnation row for apply_runs FK binding.
// status='ready' is a valid terminal enum.
func seedIncarnation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	const q = `INSERT INTO incarnation (name, service, service_version, status)
		VALUES ($1, 'svc-test', 'v1.0.0', 'ready')`
	if _, err := pool.Exec(ctx, q, name); err != nil {
		t.Fatalf("seed incarnation %s: %v", name, err)
	}
}

// seedApplyRun inserts an apply_runs row for an existing incarnation. finishedAt
// nil means the run is still running and not matched by purge_apply_runs;
// otherwise status is terminal (success/failed/cancelled), and caller sets
// finished_at.
func seedApplyRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applyID, sid, incarnation, status string, startedAt time.Time, finishedAt *time.Time) {
	t.Helper()
	const q = `INSERT INTO apply_runs
		(apply_id, sid, incarnation_name, scenario, status, started_at, finished_at)
		VALUES ($1, $2, $3, 'deploy', $4, $5, $6)`
	if _, err := pool.Exec(ctx, q, applyID, sid, incarnation, status, startedAt, finishedAt); err != nil {
		t.Fatalf("seed apply_run apply_id=%s sid=%s: %v", applyID, sid, err)
	}
}

// seedClaimedApplyRun inserts an apply_runs row under Ward claim (migration
// 025) for recovery tests: any status (claimed/dispatched/running/...) with the
// given claim_by_kid owner, claim_expires_at lease, and attempt fencing epoch.
// claimExpiresAt in the past means the Ward expired; in the future means it is
// alive.
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

// applyRunSnapshot reads fields needed by the recovery test by (apply_id, sid).
func applyRunSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applyID, sid string) (status string, attempt int, claimByKID *string) {
	t.Helper()
	const q = `SELECT status, attempt, claim_by_kid FROM apply_runs WHERE apply_id = $1 AND sid = $2`
	if err := pool.QueryRow(ctx, q, applyID, sid).Scan(&status, &attempt, &claimByKID); err != nil {
		t.Fatalf("snapshot apply_id=%s sid=%s: %v", applyID, sid, err)
	}
	return status, attempt, claimByKID
}

// seedTaskRegister inserts an apply_task_register row for an existing apply_run
// (FK to apply_runs(apply_id, sid)). register_data is minimal non-empty jsonb;
// contents do not matter for purge logic because the criterion is apply_run
// status + finished_at, not the register row itself.
//
// plan_index is part of PK after migration 079, which changed PK from
// (apply_id, sid, task_idx) to (apply_id, sid, plan_index). Seed plan_index =
// task_idx: for a linear plan (N=1) they match, exactly what backfill 079 does.
// Without it, two rows for one apply_id with different task_idx would get
// DEFAULT plan_index 0 and fail on duplicate key.
func seedTaskRegister(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applyID, sid string, taskIdx int) {
	t.Helper()
	const q = `INSERT INTO apply_task_register (apply_id, sid, task_idx, plan_index, register_data)
		VALUES ($1, $2, $3, $3, '{"rc": 0}'::jsonb)`
	if _, err := pool.Exec(ctx, q, applyID, sid, taskIdx); err != nil {
		t.Fatalf("seed task_register apply_id=%s sid=%s task_idx=%d: %v", applyID, sid, taskIdx, err)
	}
}

// resetIdentityTables clears souls / bootstrap_tokens / soul_seeds between
// subtests in one `t.Run` namespace. FK CASCADE handles the rest.
func resetIdentityTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	// TRUNCATE with CASCADE is the cheapest way. Do not touch operators
	// (FK ON DELETE SET NULL, our FK created_by_aid in both tables
	// nullable).
	if _, err := pool.Exec(ctx, "TRUNCATE souls CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// TestIntegration_ExpirePendingSeeds is an end-to-end check of
// `expire_pending_seeds(interval, integer)`:
//
//  1. seed souls for FK binding of tokens.
//  2. inserts 3 pending tokens with expired expires_at + 2 fresh pending +
//     1 used token.
//  3. calls Purger.PurgeExpiredPendingTokens(0, 100).
//  4. asserts 3 deleted; table has 3 left (2 fresh pending + 1 used).
func TestIntegration_ExpirePendingSeeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	// Souls, one per token so the partial unique index by active SID does not fail.
	for i := 0; i < 6; i++ {
		seedSoul(t, ctx, pool, fmt.Sprintf("host%d.example.com", i), "pending", now.Add(-2*time.Hour), nil)
	}

	// 3 pending with expired expires_at (24h ago).
	for i := 0; i < 3; i++ {
		seedToken(t, ctx, pool, fmt.Sprintf("host%d.example.com", i),
			now.Add(-48*time.Hour), now.Add(-24*time.Hour), nil,
			fmt.Sprintf("expired-pending-%d", i))
	}
	// 2 fresh pending with expires_at in the future.
	for i := 3; i < 5; i++ {
		seedToken(t, ctx, pool, fmt.Sprintf("host%d.example.com", i),
			now.Add(-1*time.Hour), now.Add(23*time.Hour), nil,
			fmt.Sprintf("fresh-pending-%d", i))
	}
	// 1 used token that must not match the rule.
	usedAt := now.Add(-12 * time.Hour)
	seedToken(t, ctx, pool, "host5.example.com",
		now.Add(-13*time.Hour), now.Add(-1*time.Hour), &usedAt,
		"used-token")

	p := reaper.NewPurger(pool)
	// maxAge = 1ms means practically everything with expires_at in the past.
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

// TestIntegration_PurgeUsedTokens is an end-to-end check of
// `purge_used_tokens(interval, integer)`: deleting used tokens older than
// max_age by used_at.
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

	// 3 old used tokens (used_at = 120 days ago).
	oldUsedAt := now.Add(-120 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		seedToken(t, ctx, pool, fmt.Sprintf("host%d.example.com", i),
			now.Add(-150*24*time.Hour), now.Add(-149*24*time.Hour), &oldUsedAt,
			fmt.Sprintf("old-used-%d", i))
	}
	// 2 fresh used tokens (used_at = 10 days ago).
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

// TestIntegration_PurgeSouls is an end-to-end check of
// `purge_souls(text[], interval, integer)`: deleting souls in selected statuses
// older than max_age.
func TestIntegration_PurgeSouls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	oldLastSeen := now.Add(-60 * 24 * time.Hour)
	freshLastSeen := now.Add(-1 * 24 * time.Hour)

	// 2 old disconnected, 1 old expired, 1 fresh disconnected, 1 connected
	// (live), 1 revoked (not in the status list).
	seedSoul(t, ctx, pool, "old-disc-1.example.com", "disconnected", now.Add(-200*24*time.Hour), &oldLastSeen)
	seedSoul(t, ctx, pool, "old-disc-2.example.com", "disconnected", now.Add(-200*24*time.Hour), &oldLastSeen)
	seedSoul(t, ctx, pool, "old-exp.example.com", "expired", now.Add(-200*24*time.Hour), &oldLastSeen)
	seedSoul(t, ctx, pool, "fresh-disc.example.com", "disconnected", now.Add(-30*24*time.Hour), &freshLastSeen)
	seedSoul(t, ctx, pool, "live.example.com", "connected", now.Add(-90*24*time.Hour), &now)
	seedSoul(t, ctx, pool, "old-rev.example.com", "revoked", now.Add(-200*24*time.Hour), &oldLastSeen)

	// soul without last_seen_at (never connected) but old registered_at must
	// match the rule.
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
	// Left: fresh-disc, live, old-rev (revoked is not in the status filter).
	if left != 3 {
		t.Errorf("souls left = %d, want 3", left)
	}
}

// TestIntegration_PurgeOldSeeds is an end-to-end check of
// `purge_old_seeds(text[], interval, integer)`: deleting soul_seeds by statuses
// superseded/expired/revoked.
func TestIntegration_PurgeOldSeeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	// One soul with several seeds in different statuses plus one active.
	seedSoul(t, ctx, pool, "h1.example.com", "connected", now.Add(-365*24*time.Hour), &now)

	// 3 old rows (issued_at = 120 days ago) in different terminal statuses.
	oldIssued := now.Add(-120 * 24 * time.Hour)
	seedSeed(t, ctx, pool, "h1.example.com", "superseded", oldIssued, "old-superseded")
	seedSeed(t, ctx, pool, "h1.example.com", "expired", oldIssued, "old-expired")
	seedSeed(t, ctx, pool, "h1.example.com", "revoked", oldIssued, "old-revoked")

	// 1 fresh superseded (issued_at = 30 days ago).
	freshIssued := now.Add(-30 * 24 * time.Hour)
	seedSeed(t, ctx, pool, "h1.example.com", "superseded", freshIssued, "fresh-superseded")

	// 1 active (partial index guarantees exactly one active per sid; age does
	// not matter because status active is not in the filter).
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
	// Left: fresh-superseded + active.
	if left != 2 {
		t.Errorf("rows left = %d, want 2", left)
	}
}

// TestIntegration_MarkDisconnected is an end-to-end check of
// `mark_disconnected(interval, integer)`: connected with stale last_seen_at ->
// disconnected.
func TestIntegration_MarkDisconnected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute)
	freshSeen := now.Add(-10 * time.Second)

	// 3 connected stale (last_seen_at > 90s ago).
	seedSoul(t, ctx, pool, "stale-1.example.com", "connected", now.Add(-1*time.Hour), &staleSeen)
	seedSoul(t, ctx, pool, "stale-2.example.com", "connected", now.Add(-1*time.Hour), &staleSeen)
	seedSoul(t, ctx, pool, "stale-3.example.com", "connected", now.Add(-1*time.Hour), &staleSeen)
	// 2 connected fresh
	seedSoul(t, ctx, pool, "fresh-1.example.com", "connected", now.Add(-1*time.Hour), &freshSeen)
	seedSoul(t, ctx, pool, "fresh-2.example.com", "connected", now.Add(-1*time.Hour), &freshSeen)
	// 1 disconnected stale, which must not be touched because status is already
	// not connected.
	seedSoul(t, ctx, pool, "disc.example.com", "disconnected", now.Add(-1*time.Hour), &staleSeen)
	// 1 connected without last_seen_at (NULL < NOW()-90s -> false, untouched).
	seedSoul(t, ctx, pool, "never-seen.example.com", "connected", now.Add(-1*time.Hour), nil)

	p := reaper.NewPurger(pool)
	updated, err := p.MarkDisconnected(ctx, 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	if updated != 3 {
		t.Errorf("updated = %d, want 3", updated)
	}

	// Recheck: remaining connected are 2 fresh + 1 never-seen.
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
	// 3 stale-* + 1 initially disconnected = 4.
	if disconnected != 4 {
		t.Errorf("disconnected = %d, want 4", disconnected)
	}
}

// redisLeaseChecker wraps Redis client for the reaper check "is EventStream to
// SID alive" and mirrors soulLeaseChecker from cmd/keeper. The external test
// package cannot name unexported interface reaper.soulLeaseChecker, but it
// passes a value that satisfies it through implicit satisfaction.
type redisLeaseChecker struct{ rc *keeperredis.Client }

func (c redisLeaseChecker) SoulStreamAlive(ctx context.Context, sid string) (bool, error) {
	return keeperredis.SoulStreamAlive(ctx, c.rc, sid)
}

// TestIntegration_MarkDisconnected_LeaseAware covers the lease-aware rule
// (ADR-006(a)): a Soul with a live Redis SID lease is NOT marked disconnected
// even with stale PG last_seen_at (idle Soul on a live stream); a truly expired
// one with no lease is marked. Covers the key invariant of part 3.
func TestIntegration_MarkDisconnected_LeaseAware(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if integrationRedisAddr == "" {
		t.Skip("integrationRedisAddr is empty (redis container unavailable, REQUIRE_DOCKER not set)")
	}
	resetIdentityTables(t, ctx, pool)

	rc, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: integrationRedisAddr}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = rc.Close() }()

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute) // > 90s, candidate

	// idle-host: connected, stale last_seen_at, BUT live stream (has lease).
	seedSoul(t, ctx, pool, "idle-live.example.com", "connected", now.Add(-time.Hour), &staleSeen)
	// dead-host: connected, stale last_seen_at, NO lease (truly expired).
	seedSoul(t, ctx, pool, "dead.example.com", "connected", now.Add(-time.Hour), &staleSeen)

	// Acquire lease only for idle-live, simulating a live EventStream.
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
	// Only dead is marked; idle-live is saved by the live lease.
	if updated != 1 {
		t.Errorf("updated = %d, want 1 (only dead)", updated)
	}

	var idleStatus, deadStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'idle-live.example.com'").Scan(&idleStatus); err != nil {
		t.Fatalf("scan idle status: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'dead.example.com'").Scan(&deadStatus); err != nil {
		t.Fatalf("scan dead status: %v", err)
	}
	if idleStatus != "connected" {
		t.Errorf("idle-live status = %q, want connected (live lease prevents false disconnect)", idleStatus)
	}
	if deadStatus != "disconnected" {
		t.Errorf("dead status = %q, want disconnected (no lease, truly expired)", deadStatus)
	}
}

// TestIntegration_MarkDisconnected_LeaseAware_Reconnect covers the reverse
// reconcile direction (snapshot latch fix): a disconnected snapshot with a LIVE
// Redis lease returns to connected. Reconnect of an already onboarded Soul does
// not touch Bootstrap RPC, and eventstream presence does not write to PG; only
// Reaper fixes the snapshot. disconnected WITHOUT lease (truly offline) remains
// disconnected.
func TestIntegration_MarkDisconnected_LeaseAware_Reconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if integrationRedisAddr == "" {
		t.Skip("integrationRedisAddr is empty (redis container unavailable, REQUIRE_DOCKER not set)")
	}
	resetIdentityTables(t, ctx, pool)

	rc, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: integrationRedisAddr}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = rc.Close() }()

	now := time.Now().UTC()
	// Snapshot latched in disconnected, but last_seen_at is fresh. This is an
	// Operator API contradiction fixed by the reverse direction.
	freshSeen := now.Add(-5 * time.Second)

	// back-online: disconnected snapshot, actually online (has lease) -> reconnect.
	seedSoul(t, ctx, pool, "back-online.example.com", "disconnected", now.Add(-time.Hour), &freshSeen)
	// still-offline: disconnected, NO lease (truly offline) -> remains disconnected.
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
	// Only back-online is returned to connected.
	if updated != 1 {
		t.Errorf("updated = %d, want 1 (only back-online)", updated)
	}

	var backStatus, stillStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'back-online.example.com'").Scan(&backStatus); err != nil {
		t.Fatalf("scan back status: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'still-offline.example.com'").Scan(&stillStatus); err != nil {
		t.Fatalf("scan still status: %v", err)
	}
	if backStatus != "connected" {
		t.Errorf("back-online status = %q, want connected (live lease -> reconnect, latch cleared)", backStatus)
	}
	if stillStatus != "disconnected" {
		t.Errorf("still-offline status = %q, want disconnected (no lease -> truly offline)", stillStatus)
	}
}

// TestIntegration_MarkDisconnected_LeaseAware_Bidirectional covers both
// directions in one run: connected+stale+no-lease -> disconnected;
// disconnected+live-lease -> connected. Return value is the sum.
func TestIntegration_MarkDisconnected_LeaseAware_Bidirectional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	if integrationRedisAddr == "" {
		t.Skip("integrationRedisAddr is empty (redis container unavailable, REQUIRE_DOCKER not set)")
	}
	resetIdentityTables(t, ctx, pool)

	rc, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: integrationRedisAddr}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	defer func() { _ = rc.Close() }()

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute)

	// going-down: connected, stale, NO lease -> disconnect.
	seedSoul(t, ctx, pool, "going-down.example.com", "connected", now.Add(-time.Hour), &staleSeen)
	// coming-back: disconnected, live lease -> reconnect.
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

// errLeaseChecker is a soulLeaseChecker that always returns a Redis check error.
// It simulates unavailable Redis for the fail-safe test: neither side of the
// snapshot moves because a live stream is more important than snapshot
// timeliness.
type errLeaseChecker struct{}

func (errLeaseChecker) SoulStreamAlive(_ context.Context, _ string) (bool, error) {
	return false, fmt.Errorf("redis unavailable (fail-safe test)")
}

// TestIntegration_MarkDisconnected_LeaseAware_FailSafeRedisError: on Redis
// check error, reconcile marks neither direction. connected candidate remains
// connected, disconnected candidate remains disconnected. The run itself does
// not fail (returns 0, no error); the next tick will retry.
func TestIntegration_MarkDisconnected_LeaseAware_FailSafeRedisError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute)

	// connected+stale: without Redis response, disconnect is unsafe; fail-safe keep.
	seedSoul(t, ctx, pool, "stale-conn.example.com", "connected", now.Add(-time.Hour), &staleSeen)
	// disconnected: without Redis response, reconnect is unsafe; fail-safe keep.
	seedSoul(t, ctx, pool, "disc.example.com", "disconnected", now.Add(-time.Hour), &staleSeen)

	p := reaper.NewPurgerWithLease(pool, errLeaseChecker{}, nil)
	updated, err := p.MarkDisconnected(ctx, 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected (fail-safe redis error): %v", err)
	}
	if updated != 0 {
		t.Errorf("updated = %d, want 0 (Redis error, mark neither direction)", updated)
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
// With Purger without lease checker: the rule is ONE-WAY pure-SQL
// mark_disconnected (migration 014): connected+stale -> disconnected, while
// disconnected does NOT move back because there is no Redis to determine
// online. Confirms preserved behavior when Redis is not configured.
func TestIntegration_MarkDisconnected_FallbackNoLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	resetIdentityTables(t, ctx, pool)

	now := time.Now().UTC()
	staleSeen := now.Add(-5 * time.Minute)

	seedSoul(t, ctx, pool, "stale-conn.example.com", "connected", now.Add(-time.Hour), &staleSeen)
	seedSoul(t, ctx, pool, "disc.example.com", "disconnected", now.Add(-time.Hour), &staleSeen)

	// NewPurger without lease -> fallback to pure-SQL one-way rule.
	p := reaper.NewPurger(pool)
	updated, err := p.MarkDisconnected(ctx, 90*time.Second, 100)
	if err != nil {
		t.Fatalf("MarkDisconnected (fallback): %v", err)
	}
	// Only connected+stale is marked; disconnected is not brought back.
	if updated != 1 {
		t.Errorf("updated = %d, want 1 (only stale-conn; reconnect unavailable without Redis)", updated)
	}

	var connStatus, discStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'stale-conn.example.com'").Scan(&connStatus); err != nil {
		t.Fatalf("scan conn status: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM souls WHERE sid = 'disc.example.com'").Scan(&discStatus); err != nil {
		t.Fatalf("scan disc status: %v", err)
	}
	if connStatus != "disconnected" {
		t.Errorf("stale-conn status = %q, want disconnected (one-way SQL rule)", connStatus)
	}
	if discStatus != "disconnected" {
		t.Errorf("disc status = %q, want disconnected (fallback does not reconnect)", discStatus)
	}
}

// TestIntegration_PurgeApplyRuns is an end-to-end check of
// `purge_apply_runs(interval, integer)`: deleting finished apply_runs
// (success/failed/cancelled/orphaned/no_match) older than max_age; running and
// fresh rows remain. no_match (FINDING-01 variant (b)) carries finished_at and
// must be purged like other terminals, otherwise rows for non-target hosts would
// accumulate forever.
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

	// 5 old finished rows in different terminal statuses match the rule
	// (success/failed/cancelled/orphaned/no_match all carry finished_at).
	seedApplyRun(t, ctx, pool, "old-success", "h1.example.com", "inc-1", "success", oldFinished.Add(-time.Hour), &oldFinished)
	seedApplyRun(t, ctx, pool, "old-failed", "h1.example.com", "inc-1", "failed", oldFinished.Add(-time.Hour), &oldFinished)
	seedApplyRun(t, ctx, pool, "old-cancelled", "h1.example.com", "inc-1", "cancelled", oldFinished.Add(-time.Hour), &oldFinished)
	seedApplyRun(t, ctx, pool, "old-orphaned", "h1.example.com", "inc-1", "orphaned", oldFinished.Add(-time.Hour), &oldFinished)
	seedApplyRun(t, ctx, pool, "old-no-match", "h1.example.com", "inc-1", "no_match", oldFinished.Add(-time.Hour), &oldFinished)
	// 1 fresh finished row is NOT older than max_age and remains.
	seedApplyRun(t, ctx, pool, "fresh-success", "h1.example.com", "inc-1", "success", freshFinished.Add(-time.Hour), &freshFinished)
	// 1 old running row (finished_at IS NULL) is NEVER deleted.
	seedApplyRun(t, ctx, pool, "old-running", "h1.example.com", "inc-1", "running", oldFinished.Add(-time.Hour), nil)

	p := reaper.NewPurger(pool)
	// max_age = 30d: 5 old finished rows match; fresh (1h) and running do not.
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
	// Left: fresh-success + old-running.
	if left != 2 {
		t.Errorf("rows left = %d, want 2 (fresh finished + running)", left)
	}
	// Running must survive any purge.
	var running int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM apply_runs WHERE status = 'running'").Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != 1 {
		t.Errorf("running left = %d, want 1 (running never purged)", running)
	}
}

// TestIntegration_PurgeApplyTaskRegister is an end-to-end check of
// `purge_apply_task_register(interval, integer)`: deleting register rows for
// terminal runs older than grace; registers for active (running) and fresh
// terminal runs remain.
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

	// Old terminal run (finished 2h ago) with 2 register rows: both match the
	// rule (grace 1h).
	seedApplyRun(t, ctx, pool, "old-term", "h1.example.com", "inc-1", "success", oldFinished.Add(-time.Hour), &oldFinished)
	seedTaskRegister(t, ctx, pool, "old-term", "h1.example.com", 0)
	seedTaskRegister(t, ctx, pool, "old-term", "h1.example.com", 1)

	// Fresh terminal run (finished 1m ago) with register: NOT older than grace
	// 1h, so register remains because scenario-runner might not have read it yet.
	seedApplyRun(t, ctx, pool, "fresh-term", "h2.example.com", "inc-1", "success", freshFinished.Add(-time.Hour), &freshFinished)
	seedTaskRegister(t, ctx, pool, "fresh-term", "h2.example.com", 0)

	// Active (running) run that started long ago (started 2h ago, finished_at IS
	// NULL) with register is NEVER deleted because scenario-runner will still
	// reach the barrier and read it.
	seedApplyRun(t, ctx, pool, "running", "h3.example.com", "inc-1", "running", oldFinished.Add(-time.Hour), nil)
	seedTaskRegister(t, ctx, pool, "running", "h3.example.com", 0)

	p := reaper.NewPurger(pool)
	// grace = 1h: 2 register rows for old-term match; fresh-term (1m) and running
	// without finished_at do not.
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
	// Left: fresh-term (1) + running (1).
	if left != 2 {
		t.Errorf("rows left = %d, want 2 (fresh-term + running)", left)
	}

	// register for an active run must survive any purge.
	var runningReg int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM apply_task_register WHERE apply_id = 'running'").Scan(&runningReg); err != nil {
		t.Fatalf("count running register: %v", err)
	}
	if runningReg != 1 {
		t.Errorf("running register left = %d, want 1 (running never purged)", runningReg)
	}

	// apply_runs are untouched: the rule cleans only register rows, while the run
	// itself remains under purge_apply_runs (30d).
	var runsLeft int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM apply_runs").Scan(&runsLeft); err != nil {
		t.Fatalf("count apply_runs: %v", err)
	}
	if runsLeft != 3 {
		t.Errorf("apply_runs left = %d, want 3 (register purge does not touch apply_runs)", runsLeft)
	}
}

// seedVoyageWithStatus inserts a voyage with the given status/finished_at. For
// running it fills claim fields (voyages_running_claim_consistency); for
// terminals finished_at is required (voyages_terminal_finished_at). cadenceID
// nil means manual run; populated means spawned from Cadence for the guard that
// purging children does not touch active schedule.
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

// seedVoyageTargetRow inserts a Leg row (FK voyage_id -> voyages ON DELETE
// CASCADE) to verify cascading deletion in purge_voyages.
func seedVoyageTargetRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, voyageID, targetID string) {
	t.Helper()
	const q = `INSERT INTO voyage_targets (voyage_id, target_kind, target_id, batch_index, status)
		VALUES ($1, 'sid', $2, 0, 'succeeded')`
	if _, err := pool.Exec(ctx, q, voyageID, targetID); err != nil {
		t.Fatalf("seed voyage_target %s/%s: %v", voyageID, targetID, err)
	}
}

// seedCadenceRow inserts a minimal ACTIVE schedule (interval-kind). It is used
// as a guard: purging run history must NOT touch cadences.
func seedCadenceRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, aid string) {
	t.Helper()
	const q = `INSERT INTO cadences (id, name, schedule_kind, interval_seconds, overlap_policy,
		kind, module, target, created_by_aid)
		VALUES ($1, $1, 'interval', 300, 'skip', 'command', 'core.cmd.shell', '[]'::jsonb, $2)`
	if _, err := pool.Exec(ctx, q, id, aid); err != nil {
		t.Fatalf("seed cadence %s: %v", id, err)
	}
}

// TestIntegration_PurgeVoyages is an end-to-end check of
// `purge_voyages(interval, integer)` (ADR-046 section 79): deleting finished
// voyages (succeeded/failed/partial_failed/cancelled) older than max_age;
// scheduled/pending/running and fresh rows remain. Guard invariants:
//   - voyage_targets are removed by ON DELETE CASCADE, leaving no broken Leg rows;
//   - active Cadence (source schedule) is NOT touched, and its back-link voyage
//     is deleted without deleting the schedule itself. FK ON DELETE SET NULL is
//     directed from voyage to cadence, so purging children does not touch parent;
//   - ephemeral Tiding with voyage_id of the deleted run remains valid for
//     `purge_orphan_ephemeral_tidings` because it is a soft link with no FK, so
//     there is no broken reference.
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

	// Active schedule guard: its child runs are cleaned, but the schedule itself is not.
	seedCadenceRow(t, ctx, pool, "cad-1", "archon-v")

	// 4 old finished rows in different terminal statuses match the rule.
	for _, st := range []string{"succeeded", "failed", "partial_failed", "cancelled"} {
		seedVoyageWithStatus(t, ctx, pool, "old-"+st, "archon-v", st, &oldFinished, nil)
	}
	// Old succeeded row spawned from Cadence (cadence_id populated) plus Leg row:
	// verify voyage_targets cascade and cadences are preserved.
	cadID := "cad-1"
	seedVoyageWithStatus(t, ctx, pool, "old-from-cadence", "archon-v", "succeeded", &oldFinished, &cadID)
	seedVoyageTargetRow(t, ctx, pool, "old-from-cadence", "h1.example.com")
	seedVoyageTargetRow(t, ctx, pool, "old-from-cadence", "h2.example.com")
	// ephemeral Tiding (needs Herald + soft-link voyage_id) for the deleted run.
	seedHeraldRaw(t, ctx, "hook-v", "archon-v")
	seedEphemeralTiding(t, ctx, "eph-old-from-cadence", "hook-v", "old-from-cadence")

	// Fresh finished is NOT older than max_age and remains.
	seedVoyageWithStatus(t, ctx, pool, "fresh-succeeded", "archon-v", "succeeded", &freshFinished, nil)
	// Unfinished rows are NEVER purged.
	seedVoyageWithStatus(t, ctx, pool, "pending-old", "archon-v", "pending", nil, nil)
	seedVoyageWithStatus(t, ctx, pool, "scheduled-old", "archon-v", "scheduled", nil, nil)
	seedVoyageWithStatus(t, ctx, pool, "running-old", "archon-v", "running", nil, nil)

	p := reaper.NewPurger(pool)
	deleted, err := p.PurgeVoyages(ctx, 30*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("PurgeVoyages: %v", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5 (4 terminals + 1 from-cadence)", deleted)
	}

	// Left: fresh-succeeded + pending + scheduled + running.
	var left int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM voyages").Scan(&left); err != nil {
		t.Fatalf("count voyages: %v", err)
	}
	if left != 4 {
		t.Errorf("voyages left = %d, want 4 (fresh + pending + scheduled + running)", left)
	}
	// Unfinished statuses must survive any purge.
	var active int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM voyages WHERE status IN ('pending', 'scheduled', 'running')").Scan(&active); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 3 {
		t.Errorf("active voyages left = %d, want 3 (pending/scheduled/running never purged)", active)
	}

	// CASCADE: voyage_targets for deleted run were removed by ON DELETE CASCADE.
	var targetsLeft int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM voyage_targets WHERE voyage_id = 'old-from-cadence'").Scan(&targetsLeft); err != nil {
		t.Fatalf("count voyage_targets: %v", err)
	}
	if targetsLeft != 0 {
		t.Errorf("voyage_targets left = %d, want 0 (ON DELETE CASCADE)", targetsLeft)
	}

	// GUARD: active schedule is NOT touched because purging children does not touch parent.
	var cadLeft int64
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM cadences WHERE id = 'cad-1'").Scan(&cadLeft); err != nil {
		t.Fatalf("count cadences: %v", err)
	}
	if cadLeft != 1 {
		t.Errorf("cadences left = %d, want 1 (active schedule is not purged)", cadLeft)
	}

	// Soft-link ephemeral Tiding for deleted voyage remains, with no FK to
	// voyages. purge_orphan_ephemeral_tidings will pick it up through NOT EXISTS
	// voyages. Here it matters only that purge_voyages did NOT fail on FK and did
	// not leave a broken reference.
	var ephLeft int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM tidings WHERE name = 'eph-old-from-cadence'").Scan(&ephLeft); err != nil {
		t.Fatalf("count ephemeral tiding: %v", err)
	}
	if ephLeft != 1 {
		t.Errorf("ephemeral tiding left = %d, want 1 (soft link, removed by separate rule)", ephLeft)
	}
}

// TestIntegration_PurgeVoyages_BatchLimit: batch_size limits one DELETE pass
// size, matching purge_apply_runs. With 5 old terminals and batch=2, the first
// pass deletes 2 and leaves 3.
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

// TestIntegration_ReclaimApplyRuns is an end-to-end check of the recovery scan
// (ADR-027 amend, S4): ONLY under-delivered rows are reclaimed, meaning claimed
// with claim_expires_at < NOW() that died BEFORE delivery to Soul. They return
// to planned with claim_by_kid/claim_at/claim_expires_at reset; attempt is
// PRESERVED. KEY invariant: dispatched with expired lease is NOT touched because
// after delivery Soul owns the run and re-claim would be double apply. running
// is also no longer reclaimed. Live Wards (claim_expires_at > NOW) and
// planned/terminal rows are not touched.
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
	expired := now.Add(-1 * time.Minute) // lease expired
	alive := now.Add(10 * time.Minute)   // lease still alive

	// 1 expired Ward that died BEFORE delivery (claimed), the ONLY reclaimed row.
	seedClaimedApplyRun(t, ctx, pool, "zombie-claimed", "h1.example.com", "inc-1", "claimed", 1, expired)
	// KEY invariant: dispatched with expired lease is NOT touched because it was
	// delivered to Soul, and re-claim would be double apply.
	seedClaimedApplyRun(t, ctx, pool, "dispatched-expired", "h2.example.com", "inc-1", "dispatched", 2, expired)
	// running with expired lease (vestigial) is no longer reclaimed.
	seedClaimedApplyRun(t, ctx, pool, "zombie-running", "h6.example.com", "inc-1", "running", 3, expired)
	// 1 live Ward with claim_expires_at in the future is NOT touched.
	seedClaimedApplyRun(t, ctx, pool, "alive-claimed", "h3.example.com", "inc-1", "claimed", 1, alive)
	// 1 planned row (free, claim columns NULL) is NOT touched because it is already queued.
	seedApplyRun(t, ctx, pool, "free-planned", "h4.example.com", "inc-1", "planned", now.Add(-time.Hour), nil)
	// 1 terminal success with expired claim_expires_at is NOT touched because
	// status is not claimed.
	seedClaimedApplyRun(t, ctx, pool, "done-success", "h5.example.com", "inc-1", "success", 1, expired)

	p := reaper.NewPurger(pool)
	reclaimed, err := p.ReclaimApplyRuns(ctx, time.Minute, 100)
	if err != nil {
		t.Fatalf("ReclaimApplyRuns: %v", err)
	}
	if reclaimed != 1 {
		t.Errorf("reclaimed = %d, want 1 (only zombie-claimed)", reclaimed)
	}

	// Under-delivered claimed row returned to planned, owner/lease reset, attempt
	// PRESERVED.
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

	// KEY invariant: dispatched with expired lease is untouched because Soul owns it.
	if status, _, kid := applyRunSnapshot(t, ctx, pool, "dispatched-expired", "h2.example.com"); status != "dispatched" || kid == nil {
		t.Errorf("dispatched-expired: status=%q kid=%v; want dispatched + non-NULL kid (Soul owns after delivery, do NOT reclaim)", status, kid)
	}
	// running with expired lease is untouched (vestigial, no longer reclaimed).
	if status, _, kid := applyRunSnapshot(t, ctx, pool, "zombie-running", "h6.example.com"); status != "running" || kid == nil {
		t.Errorf("zombie-running: status=%q kid=%v; want running + non-NULL kid (running is no longer reclaimed)", status, kid)
	}

	// Live Ward is untouched.
	if status, _, kid := applyRunSnapshot(t, ctx, pool, "alive-claimed", "h3.example.com"); status != "claimed" || kid == nil {
		t.Errorf("alive-claimed: status=%q kid=%v; want claimed + non-NULL kid (alive Ward untouched)", status, kid)
	}
	// planned is untouched.
	if status, _, _ := applyRunSnapshot(t, ctx, pool, "free-planned", "h4.example.com"); status != "planned" {
		t.Errorf("free-planned: status=%q; want planned (already queued)", status)
	}
	// Terminal success is untouched.
	if status, _, _ := applyRunSnapshot(t, ctx, pool, "done-success", "h5.example.com"); status != "success" {
		t.Errorf("done-success: status=%q; want success (terminal untouched)", status)
	}
}

// TestIntegration_ReclaimApplyRuns_BatchLimit: recovery respects batch_size. At
// batch=1, one run returns exactly one row, and the other zombies remain for the
// next run, following the same drain pattern as other rules.
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

// TestIntegration_PurgeAuditOld is an end-to-end check.
// `purge_audit_old(interval, integer)`:
//
//  1. Inserts 5 audit records: 3 older than 365d, 2 fresh.
//  2. Covers Purger.PurgeAuditOld(365d, 100).
//  3. Asserts 3 returned, 2 left in table.
func TestIntegration_PurgeAuditOld(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)

	expiredAt := time.Now().UTC().Add(-400 * 24 * time.Hour)
	freshAt := time.Now().UTC().Add(-1 * time.Hour)

	insert := `INSERT INTO audit_log (audit_id, created_at, event_type, source, payload)
		VALUES ($1, $2, 'config.reload_succeeded', 'signal', '{}'::jsonb)`
	for i, ts := range []time.Time{expiredAt, expiredAt, expiredAt, freshAt, freshAt} {
		// audit.NewULID() is Crockford-base32 26-char ULID, compatible with a
		// future CHECK constraint on audit_id format (M0.6+).
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

// seedStateHistory inserts a state_history snapshot for an existing
// incarnation. scenario is an arbitrary label (`deploy`, `migration`, ...); at
// is set by caller to control ORDER BY at DESC.
func seedStateHistory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, historyID, incarnation, scenario string, at time.Time) {
	t.Helper()
	const q = `INSERT INTO state_history
		(history_id, incarnation_name, scenario, state_before, state_after, apply_id, at)
		VALUES ($1, $2, $3, '{}'::jsonb, '{}'::jsonb, $4, $5)`
	if _, err := pool.Exec(ctx, q, historyID, incarnation, scenario, historyID, at); err != nil {
		t.Fatalf("seed state_history history_id=%s: %v", historyID, err)
	}
}

// countArchivedStateHistory returns (total, archived) snapshots for an
// incarnation. Used as an assert helper in integration tests for the rule.
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
// snapshots -> run the rule with keep_last_n=N, keep_version_bump=true (no
// migration snapshots) -> expect archived_at IS NOT NULL for the 10 oldest.
func TestIntegration_ArchiveStateHistory_KeepsLastN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := fixturePool(t)
	// state_history.incarnation_name → incarnation(name) ON DELETE CASCADE; TRUNCATE
	// CASCADE cleans state_history between subtests.
	if _, err := pool.Exec(ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}

	const incName = "svc-redis-prod"
	const keepLastN = 50
	const total = keepLastN + 10
	seedIncarnation(t, ctx, pool, incName)

	now := time.Now().UTC()
	for i := 0; i < total; i++ {
		// at goes from old to new; (total-1-i) means the oldest has the largest offset.
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
		t.Errorf("total rows = %d, want %d (physical deletion is forbidden)", gotTotal, total)
	}
	if gotArchived != 10 {
		t.Errorf("archived rows = %d, want 10", gotArchived)
	}
}

// TestIntegration_ArchiveStateHistory_ExcludesVersionBump — pre-fill N+5
// snapshots, 2 of them version-bump (scenario='migration') INSIDE the old tail.
// With keep_version_bump=true they are NOT archived: archived count = 3
// non-migration tail rows, and version-bump snapshots remain active.
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
	// Tail of 5 oldest (rn > 50): indexes 0..4 in at ASC order; write 2 of them
	// (positions 1 and 3) as scenario='migration', and they must remain active.
	migrationPositions := map[int]bool{1: true, 3: true}
	wantArchived := int64(0)
	for i := 0; i < total; i++ {
		ts := now.Add(-time.Duration(total-1-i) * time.Minute)
		scenario := "deploy"
		isTail := i < (total - keepLastN) // first 5 = old tail
		if isTail && migrationPositions[i] {
			scenario = "migration"
		} else if isTail {
			wantArchived++
		}
		seedStateHistory(t, ctx, pool, audit.NewULID(), incName, scenario, ts)
	}
	if wantArchived != 3 {
		t.Fatalf("test arithmetic: wantArchived = %d, want 3 (5 tail rows - 2 migration)", wantArchived)
	}

	p := reaper.NewPurger(pool)
	got, err := p.ArchiveStateHistory(ctx, keepLastN, true, 1000)
	if err != nil {
		t.Fatalf("ArchiveStateHistory: %v", err)
	}
	if got != wantArchived {
		t.Errorf("archived = %d, want %d (excluding migration snapshots)", got, wantArchived)
	}

	// version-bump snapshots must remain active even with rn > keepLastN.
	const q = `SELECT COUNT(*) FROM state_history
		WHERE incarnation_name = $1 AND scenario = 'migration' AND archived_at IS NULL`
	var activeMigration int64
	if err := pool.QueryRow(ctx, q, incName).Scan(&activeMigration); err != nil {
		t.Fatalf("count active migration: %v", err)
	}
	if activeMigration != 2 {
		t.Errorf("active migration snapshots = %d, want 2 (protected by keep_version_bump=true)", activeMigration)
	}
}

// TestIntegration_ArchiveStateHistory_KeepVersionBumpFalse: with
// keep_version_bump=false, the rule archives EVERYTHING beyond keep_last_n,
// including migration snapshots. Covers explicit operator opt-out, for example
// for a test stand without recovery requirements.
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
	// 5 snapshots: 0 = oldest (migration), 1..2 = deploy, 3..4 = deploy (newest).
	// keep_last_n=3 -> snapshots 0..1 are in the tail; with keep_version_bump=false both are archived.
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
		t.Errorf("archived = %d, want 2 (including migration with keep_version_bump=false)", got)
	}

	gotTotal, gotArchived := countArchivedStateHistory(t, ctx, pool, incName)
	if gotTotal != 5 {
		t.Errorf("total = %d, want 5 (physical deletion is forbidden)", gotTotal)
	}
	if gotArchived != 2 {
		t.Errorf("archived = %d, want 2", gotArchived)
	}
}

// TestIntegration_ArchiveStateHistory_PerIncarnation: N is counted separately
// for each incarnation. 70 snapshots on A (keep=50 -> archived=20) + 30
// snapshots on B (keep=50 -> archived=0). Covers PARTITION BY in the window
// function.
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

// TestIntegration_ArchiveStateHistory_BatchLimit: batch limits soft-deleted rows
// per run. With batch=5 on a tail of 10 candidates, exactly 5 are archived and
// the rest wait for the next run (drain pattern). The OLDEST candidates are
// archived first (rn DESC in subquery).
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
	// 60 snapshots: 10 tail rows (rn 51..60), 50 live rows (rn 1..50).
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

	// Second run finishes the remainder.
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

	// Third run is empty (drain to 0).
	got3, err := p.ArchiveStateHistory(ctx, keepLastN, true, 5)
	if err != nil {
		t.Fatalf("ArchiveStateHistory (zero): %v", err)
	}
	if got3 != 0 {
		t.Errorf("zero batch archived = %d, want 0", got3)
	}
}
