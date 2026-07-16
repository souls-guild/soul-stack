//go:build integration

package reaper_test

// End-to-end Runner integration tests (Reaper.c) verify dispatch, lease,
// dry_run, and per-rule scheduling through the real Runner.Run loop over
// testcontainers PG + Redis.
//
// Per-rule SQL functions are covered separately by [integration_test.go]
// (Reaper.b). This file tests the next layer: Runner.Run() correctly connects
// cfg (`reaper.rules.*`) to Purger calls, and the lease is actually acquired in
// Redis and keeps a competing Runner in the acquire backoff loop.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/reaper/...

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/reaper"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// reaperLeaderKey is a copy of [reaper.leaseKey], fixed in
// docs/keeper/reaper.md as a cluster invariant. Duplicate it as a local
// constant in the external test package because unexported access is
// unavailable; docs/keeper/reaper.md and [reaper.leaseKey] track value changes
// synchronously.
const reaperLeaderKey = "reaper:leader"

// runnerExecutor mirrors the composite cmd/keeper executor: Purger plus nil-Vault
// VaultReconciler. These E2E tests do not enable reap_orphan_vault_keys in YAML,
// so Vault is not needed (vault=nil degrades, but the rule is not called).
type runnerExecutor struct {
	*reaper.Purger
	*reaper.VaultReconciler
}

// newRunnerExecutor builds a PurgerAPI-compatible executor over pool.
func newRunnerExecutor(pool *pgxpool.Pool) *runnerExecutor {
	return &runnerExecutor{
		Purger:          reaper.NewPurger(pool),
		VaultReconciler: reaper.NewVaultReconciler(nil, nil, slog.Default(), nil),
	}
}

// runnerIntegrationFixture is shared setup: PG + Redis + clean tables. It skips
// when containers are unavailable, see TestMain. Between tests it idempotently
// clears souls/audit_log plus the lease key.
type runnerIntegrationFixture struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	redis *redis.Client
}

func newRunnerIntegrationFixture(t *testing.T) *runnerIntegrationFixture {
	t.Helper()
	if integrationPool == nil {
		t.Skip("integrationPool is nil (docker unavailable, REQUIRE_DOCKER not set)")
	}
	if integrationRedisAddr == "" {
		t.Skip("integrationRedisAddr is empty (redis container unavailable, REQUIRE_DOCKER not set)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	rc, err := redis.NewClient(ctx, redis.Config{Addr: integrationRedisAddr}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	f := &runnerIntegrationFixture{ctx: ctx, pool: integrationPool, redis: rc}
	f.reset(t)
	return f
}

// reset clears state between subtests. It covers all tables touched by Reaper
// plus the Redis lease key.
func (f *runnerIntegrationFixture) reset(t *testing.T) {
	t.Helper()
	resetIdentityTables(t, f.ctx, f.pool)
	if _, err := f.pool.Exec(f.ctx, "TRUNCATE audit_log"); err != nil {
		t.Fatalf("truncate audit_log: %v", err)
	}
	// incarnation CASCADE removes related apply_runs (FK ON DELETE CASCADE).
	if _, err := f.pool.Exec(f.ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}
	delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flushLeaderKey(t, delCtx, f.redis)
}

// flushLeaderKey deletes the lease key through ephemeral Acquire+Release.
// Without unexported underlying(), the only way to remove a foreign key is to
// wait for TTL expiration. For isolated test Redis it is more convenient to
// overwrite with a short TTL and immediately Release.
//
// If the key is free, Acquire succeeds and Release deletes it. If it is held by
// a foreign holder, which should not happen because reset runs between tests,
// Release CAS will not work, and we just wait for the previous test's TTL to
// expire because lock_ttl values are short.
func flushLeaderKey(t *testing.T, ctx context.Context, rc *redis.Client) {
	t.Helper()
	l, err := redis.Acquire(ctx, rc, reaperLeaderKey, "test-reset", 100*time.Millisecond)
	if err != nil {
		// If ErrLeaseTaken, the previous test holds the key; wait for expiration.
		// lock_ttl in test YAML is 300ms, so 500ms is enough.
		time.Sleep(500 * time.Millisecond)
		return
	}
	_ = l.Release(ctx)
}

// waitLeaderKeyFree spins Acquire+Release ephemeral lease until success. It
// guarantees reaperLeaderKey is free BEFORE taking the external lease in
// [TestIntegration_Runner_LeaseConflictBlocks]. Under ./... load, finishing
// Runners from neighboring tests may hold the key; wait up to 5s because test
// lock_ttl values are short. On failure, call t.Fatal so a stuck key is not
// masked.
func waitLeaderKeyFree(t *testing.T, rc *redis.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		l, err := redis.Acquire(ctx, rc, reaperLeaderKey, "test-free-probe", 100*time.Millisecond)
		if err == nil {
			_ = l.Release(ctx)
			cancel()
			return
		}
		cancel()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("reaperLeaderKey %q did not become free within 5s (stuck from neighboring test)", reaperLeaderKey)
}

// runnerIntegrationYAML is minimum keeper.yml with all 6 reaper rules enabled.
// Differences from runner_test.go::testReaperBYAML:
//   - max_age/stale_after are very short (1ms / 1s), so real SQL functions find
//     seeded expired records under any timestamp shift between insert and
//     dispatch.
//   - batch_size is large (1000), so one tick sweeps the whole seed.
const runnerIntegrationYAML = `
kid: keeper-int-01

listen:
  grpc:
    bootstrap:    { addr: "127.0.0.1:19442", tls: { cert: /tmp/c, key: /tmp/k } }
    event_stream: { addr: "127.0.0.1:18443", tls: { cert: /tmp/c, key: /tmp/k, ca: /tmp/ca } }
  openapi: { addr: "127.0.0.1:18080" }
  mcp:     { addr: "127.0.0.1:18081" }
  metrics: { addr: "127.0.0.1:19090" }

postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }

redis:
  addr: "127.0.0.1:6379"

vault:
  addr: "http://127.0.0.1:8200"
  auth: { method: token }
  pki_mount: "pki/soulstack"

auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-int-01
    ttl_default: 24h
    ttl_bootstrap: 30d

logging:
  level: info
  format: json

reaper:
  enabled: true
  interval: 200ms
  dry_run: false
  batch_size: 1000
  lock_ttl: 2s
  rules:
    purge_audit_old:
      enabled: true
      max_age: 1ms
      action: delete
    expire_pending_seeds:
      enabled: true
      max_age: 1ms
      action: expire
    purge_used_tokens:
      enabled: true
      max_age: 1ms
      action: delete
    purge_souls:
      enabled: true
      statuses: [disconnected, expired]
      max_age: 1h
      action: delete
    purge_old_seeds:
      enabled: true
      statuses: [superseded, expired, revoked]
      max_age: 1ms
      action: delete
    mark_disconnected:
      enabled: true
      stale_after: 1s
      action: set_status
      target_status: disconnected
    purge_apply_runs:
      enabled: true
      max_age: 1ms
      action: delete
`

// writeYAMLAndLoadStore writes body to a tempfile and returns Store through
// LoadKeeperStore. Validation errors call t.Fatalf because they are test bugs,
// not runtime behavior.
func writeYAMLAndLoadStore(t *testing.T, body string) *config.Store[config.KeeperConfig] {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "keeper.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write keeper.yml: %v", err)
	}
	store, _, err := config.LoadKeeperStore(p, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	if store.Get() == nil {
		t.Fatal("store snapshot is nil after initial load")
	}
	return store
}

// replaceOnceStr is a copy of [reaper.replaceOnce] for the external package.
// Duplicate it to avoid moving a test helper into production code.
func replaceOnceStr(t *testing.T, s, old, newStr string) string {
	t.Helper()
	idx := indexOnce(t, s, old)
	return s[:idx] + newStr + s[idx+len(old):]
}

func indexOnce(t *testing.T, s, sub string) int {
	t.Helper()
	first := -1
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			if first >= 0 {
				t.Fatalf("substring %q found more than once", sub)
			}
			first = i
		}
	}
	if first < 0 {
		t.Fatalf("substring %q not found", sub)
	}
	return first
}

// silentSlog is a discarding logger for integration tests. Loop logs are noisy
// on a 200ms interval and are not validated by asserts.
func silentSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// seedAllRuleData fills PG with records matched by all 6 rules when max_age=1ms
// / stale_after=1s. Returned counters are used for "affected > 0" asserts.
type seededCounts struct {
	audit, pendingTokens, usedTokens, souls, seeds, stale int
	// applyFinished are old finished apply_runs matched by purge; applyRunning
	// are running apply_runs that are never purged.
	applyFinished, applyRunning int
}

func seedAllRuleData(t *testing.T, f *runnerIntegrationFixture) seededCounts {
	t.Helper()
	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)
	stale := now.Add(-10 * time.Minute)

	// audit_log: 2 old records matched by purge_audit_old with max_age=1ms.
	for i := 0; i < 2; i++ {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO audit_log (audit_id, created_at, event_type, source, payload)
			 VALUES ($1, $2, 'config.reload_succeeded', 'signal', '{}'::jsonb)`,
			audit.NewULID(), old); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}

	// souls for FK binding of tokens / seeds.
	//   - 2 old disconnected rows are matched by purge_souls.
	//   - 2 connected stale rows are matched by mark_disconnected.
	//   - 2 connected pending-token hosts + 2 used-token hosts + 1 seed host for
	//     FK from bootstrap_tokens / soul_seeds. Partial unique index
	//     `bootstrap_tokens_active_by_sid_idx` requires <=1 pending token per
	//     sid, so each pending token gets its own sid.
	seedSoul(t, f.ctx, f.pool, "disc-1.example.com", "disconnected", old, &old)
	seedSoul(t, f.ctx, f.pool, "disc-2.example.com", "disconnected", old, &old)
	seedSoul(t, f.ctx, f.pool, "stale-1.example.com", "connected", old, &stale)
	seedSoul(t, f.ctx, f.pool, "stale-2.example.com", "connected", old, &stale)
	seedSoul(t, f.ctx, f.pool, "pend-1.example.com", "connected", old, &now)
	seedSoul(t, f.ctx, f.pool, "pend-2.example.com", "connected", old, &now)
	seedSoul(t, f.ctx, f.pool, "used-1.example.com", "connected", old, &now)
	seedSoul(t, f.ctx, f.pool, "used-2.example.com", "connected", old, &now)
	seedSoul(t, f.ctx, f.pool, "seed-host.example.com", "connected", old, &now)

	// bootstrap_tokens: 2 expired-pending, one per sid, plus 2 old used tokens.
	// Used tokens may sit on the same sids as used-host-1/2; after used_at IS NOT
	// NULL, the partial index does not apply and there is no restriction.
	for i := 0; i < 2; i++ {
		seedToken(t, f.ctx, f.pool, fmt.Sprintf("pend-%d.example.com", i+1),
			now.Add(-48*time.Hour), now.Add(-24*time.Hour), nil,
			fmt.Sprintf("exp-pend-%d", i))
	}
	usedAt := now.Add(-120 * 24 * time.Hour)
	for i := 0; i < 2; i++ {
		seedToken(t, f.ctx, f.pool, fmt.Sprintf("used-%d.example.com", i+1),
			now.Add(-150*24*time.Hour), now.Add(-149*24*time.Hour), &usedAt,
			fmt.Sprintf("used-old-%d", i))
	}

	// soul_seeds: 2 superseded plus 1 old expired certificate.
	seedSeed(t, f.ctx, f.pool, "seed-host.example.com", "superseded", old, "s-old-1")
	seedSeed(t, f.ctx, f.pool, "seed-host.example.com", "superseded", old, "s-old-2")
	seedSeed(t, f.ctx, f.pool, "seed-host.example.com", "expired", old, "s-old-3")

	// apply_runs: incarnation + 2 old finished rows matched by purge_apply_runs
	// with max_age=1ms + 1 running row that is never purged.
	seedIncarnation(t, f.ctx, f.pool, "inc-1")
	seedApplyRun(t, f.ctx, f.pool, "ar-old-1", "seed-host.example.com", "inc-1", "success", old, &old)
	seedApplyRun(t, f.ctx, f.pool, "ar-old-2", "seed-host.example.com", "inc-1", "failed", old, &old)
	seedApplyRun(t, f.ctx, f.pool, "ar-running", "seed-host.example.com", "inc-1", "running", old, nil)

	return seededCounts{
		audit: 2, pendingTokens: 2, usedTokens: 2, souls: 2, seeds: 3, stale: 2,
		applyFinished: 2, applyRunning: 1,
	}
}

// countRow wraps COUNT(*) queries for asserts.
func countRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// waitForCond runs cond() with a small step until timeout; on failure, it calls
// t.Fatal. Local copy of [reaper.waitFor] because the internal helper is not
// visible from the external test package.
func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// TestIntegration_Runner_E2E_AllRules is the main happy path: start Runner.Run,
// seed data for all 6 rules, wait one tick, and verify each table was reduced or
// updated after execution.
func TestIntegration_Runner_E2E_AllRules(t *testing.T) {
	f := newRunnerIntegrationFixture(t)
	seeded := seedAllRuleData(t, f)

	store := writeYAMLAndLoadStore(t, runnerIntegrationYAML)
	rn, err := reaper.NewRunner(reaper.Deps{
		Purger: newRunnerExecutor(f.pool),
		Redis:  f.redis,
		Store:  store,
		Holder: "keeper-int-01",
		Logger: silentSlog(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Wait until each rule has run at least once. The signal is row counts
	// dropping to expected values.
	waitForCond(t, 8*time.Second, func() bool {
		auditLeft := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log")
		pendingLeft := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM bootstrap_tokens WHERE used_at IS NULL")
		usedLeft := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM bootstrap_tokens WHERE used_at IS NOT NULL")
		seedsLeft := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM soul_seeds WHERE status IN ('superseded','expired','revoked')")
		discLeft := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM souls WHERE status = 'disconnected' AND sid LIKE 'disc-%'")
		// mark_disconnected: stale-1/2 must become disconnected.
		stale := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM souls WHERE status = 'disconnected' AND sid LIKE 'stale-%'")
		// purge_apply_runs: finished rows are deleted, running remains.
		applyFinishedLeft := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM apply_runs WHERE status IN ('success','failed','cancelled')")

		return auditLeft == 0 && pendingLeft == 0 && usedLeft == 0 &&
			seedsLeft == 0 && discLeft == 0 && stale == int64(seeded.stale) &&
			applyFinishedLeft == 0
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	// Final asserts are the formal guarantee. Even if waitForCond passed, record
	// exact numbers in the test log for bisection.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log"); got != 0 {
		t.Errorf("audit_log left = %d, want 0", got)
	}
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM bootstrap_tokens"); got != 0 {
		t.Errorf("bootstrap_tokens left = %d, want 0", got)
	}
	// soul_seeds: only active remain; none are in this seed, so 0 here.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM soul_seeds"); got != 0 {
		t.Errorf("soul_seeds left = %d, want 0", got)
	}
	// souls: 9 initially - 2 disc removed by purge_souls = 7. stale-1/2 became
	// disconnected after mark_disconnected, but last_seen=10m < max_age=1h, so
	// purge_souls does NOT delete them.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM souls"); got != 7 {
		t.Errorf("souls left = %d, want 7", got)
	}
	if got := countRow(t, f.ctx, f.pool,
		"SELECT COUNT(*) FROM souls WHERE sid LIKE 'stale-%' AND status = 'disconnected'"); got != int64(seeded.stale) {
		t.Errorf("stale-* now disconnected = %d, want %d", got, seeded.stale)
	}
	// apply_runs: 2 finished rows deleted by purge_apply_runs, 1 running survived.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM apply_runs"); got != int64(seeded.applyRunning) {
		t.Errorf("apply_runs left = %d, want %d (only running survives)", got, seeded.applyRunning)
	}
	if got := countRow(t, f.ctx, f.pool,
		"SELECT COUNT(*) FROM apply_runs WHERE status = 'running'"); got != int64(seeded.applyRunning) {
		t.Errorf("apply_runs running left = %d, want %d (running never purged)", got, seeded.applyRunning)
	}
}

// TestIntegration_Runner_DryRunSkipsAll: dry_run: true means no row should be
// deleted or updated by any of the 6 rules.
func TestIntegration_Runner_DryRunSkipsAll(t *testing.T) {
	f := newRunnerIntegrationFixture(t)
	seeded := seedAllRuleData(t, f)

	body := replaceOnceStr(t, runnerIntegrationYAML, "dry_run: false", "dry_run: true")
	store := writeYAMLAndLoadStore(t, body)
	rn, err := reaper.NewRunner(reaper.Deps{
		Purger: newRunnerExecutor(f.pool),
		Redis:  f.redis,
		Store:  store,
		Holder: "keeper-int-01",
		Logger: silentSlog(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	// Give the loop several ticks (interval=200ms) to run. Under dry_run nothing
	// should change, so wait a fixed time and then check counts.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	// audit_log: original 2 remain.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log"); got != int64(seeded.audit) {
		t.Errorf("audit_log = %d, want %d (dry_run should skip)", got, seeded.audit)
	}
	// bootstrap_tokens: original 4 remain.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM bootstrap_tokens"); got != 4 {
		t.Errorf("bootstrap_tokens = %d, want 4 (dry_run)", got)
	}
	// soul_seeds: 3 remain.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM soul_seeds"); got != int64(seeded.seeds) {
		t.Errorf("soul_seeds = %d, want %d (dry_run)", got, seeded.seeds)
	}
	// souls: 9 (2 disc + 2 stale + 2 pend + 2 used + 1 seed-host), all in
	// original statuses.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM souls"); got != 9 {
		t.Errorf("souls = %d, want 9 (dry_run)", got)
	}
	// Check mark_disconnected separately: stale-* are still connected.
	if got := countRow(t, f.ctx, f.pool,
		"SELECT COUNT(*) FROM souls WHERE sid LIKE 'stale-%' AND status = 'connected'"); got != int64(seeded.stale) {
		t.Errorf("stale-souls still connected = %d, want %d (dry_run skipped mark_disconnected)", got, seeded.stale)
	}
	// apply_runs: 3 (2 finished + 1 running) remain.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM apply_runs"); got != int64(seeded.applyFinished+seeded.applyRunning) {
		t.Errorf("apply_runs = %d, want %d (dry_run)", got, seeded.applyFinished+seeded.applyRunning)
	}
}

// TestIntegration_Runner_PerRuleEnabled: disabling one rule specifically
// (purge_audit_old) must not affect the others. Verify audit_log remains
// untouched, while bootstrap_tokens / souls are processed.
func TestIntegration_Runner_PerRuleEnabled(t *testing.T) {
	f := newRunnerIntegrationFixture(t)
	seeded := seedAllRuleData(t, f)

	body := replaceOnceStr(t, runnerIntegrationYAML,
		"purge_audit_old:\n      enabled: true",
		"purge_audit_old:\n      enabled: false")
	store := writeYAMLAndLoadStore(t, body)
	rn, err := reaper.NewRunner(reaper.Deps{
		Purger: newRunnerExecutor(f.pool),
		Redis:  f.redis,
		Store:  store,
		Holder: "keeper-int-01",
		Logger: silentSlog(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Wait until all ENABLED rules have run.
	waitForCond(t, 8*time.Second, func() bool {
		pending := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM bootstrap_tokens WHERE used_at IS NULL")
		seeds := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM soul_seeds WHERE status IN ('superseded','expired','revoked')")
		disc := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM souls WHERE status = 'disconnected' AND sid LIKE 'disc-%'")
		return pending == 0 && seeds == 0 && disc == 0
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	// purge_audit_old is disabled, so audit_log is untouched.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log"); got != int64(seeded.audit) {
		t.Errorf("audit_log = %d, want %d (purge_audit_old disabled)", got, seeded.audit)
	}
}

// TestIntegration_Runner_LeaseConflictBlocks: an external holder keeps the lease
// in Redis. Runner spins in the acquire backoff loop and must neither overwrite
// the foreign key nor call any rule.
func TestIntegration_Runner_LeaseConflictBlocks(t *testing.T) {
	f := newRunnerIntegrationFixture(t)
	seeded := seedAllRuleData(t, f)

	// reaperLeaderKey is fixed as a production invariant, so neighboring reaper
	// tests share it in one per-package Redis container. Before taking the
	// external lease, wait until the key is free from the previous test;
	// otherwise under parallel ./... load, external Acquire can catch a foreign
	// stale key and the test flakes. waitLeaderKeyFree spins Acquire+Release
	// until success.
	waitLeaderKeyFree(t, f.redis)

	// Take the lease externally with a generous TTL. Under parallel test load,
	// wall-clock time from Acquire to the final probe can stretch, and expiration
	// of the external lease before the probe would give a false competitor
	// success. 60s surely outlives test ctx 1.5s plus load slack. Holder !=
	// Runner holder, so the competitor's Renew CAS will not work.
	external, err := redis.Acquire(f.ctx, f.redis, reaperLeaderKey, "external-leader", 60*time.Second)
	if err != nil {
		t.Fatalf("external Acquire: %v", err)
	}
	t.Cleanup(func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = external.Release(relCtx)
	})

	store := writeYAMLAndLoadStore(t, runnerIntegrationYAML)
	rn, err := reaper.NewRunner(reaper.Deps{
		Purger:         newRunnerExecutor(f.pool),
		Redis:          f.redis,
		Store:          store,
		Holder:         "keeper-int-blocked",
		Logger:         silentSlog(),
		AcquireBackoff: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	// No rule must run because the leader is foreign.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log"); got != int64(seeded.audit) {
		t.Errorf("audit_log = %d, want %d (lease blocked)", got, seeded.audit)
	}
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM bootstrap_tokens"); got != 4 {
		t.Errorf("bootstrap_tokens = %d, want 4 (lease blocked)", got)
	}
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM soul_seeds"); got != int64(seeded.seeds) {
		t.Errorf("soul_seeds = %d, want %d (lease blocked)", got, seeded.seeds)
	}
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM souls"); got != 9 {
		t.Errorf("souls = %d, want 9 (lease blocked)", got)
	}
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM apply_runs"); got != int64(seeded.applyFinished+seeded.applyRunning) {
		t.Errorf("apply_runs = %d, want %d (lease blocked)", got, seeded.applyFinished+seeded.applyRunning)
	}
	// The external lease is still held by the competitor. Indirect check:
	// repeated Acquire from another holder returns ErrLeaseTaken.
	if _, err := redis.Acquire(f.ctx, f.redis, reaperLeaderKey, "probe", time.Second); err == nil {
		t.Errorf("competing Acquire succeeded; expected ErrLeaseTaken")
	}
}

// reclaimApplyRunsRuleYAML is the reclaim_apply_runs rule block inserted into
// runnerIntegrationYAML after purge_apply_runs. The rule is DISABLED by default,
// enabled only after attempt-fencing rollout; see purger.go. Therefore it is
// not in the common YAML, and tests insert it explicitly with the desired
// enabled value.
const reclaimApplyRunsRuleYAML = `    purge_apply_runs:
      enabled: true
      max_age: 1ms
      action: delete
    reclaim_apply_runs:
      enabled: %t
      stale_after: 1m
      action: set_status
      target_status: planned
`

// withReclaimRule inserts the reclaim_apply_runs block into runnerIntegrationYAML
// immediately after purge_apply_runs, setting enabled to the requested value.
func withReclaimRule(t *testing.T, enabled bool) string {
	t.Helper()
	const purgeApplyRunsBlock = `    purge_apply_runs:
      enabled: true
      max_age: 1ms
      action: delete
`
	return replaceOnceStr(t, runnerIntegrationYAML, purgeApplyRunsBlock,
		fmt.Sprintf(reclaimApplyRunsRuleYAML, enabled))
}

// seedReclaimScenario seeds real PG with the full apply_runs set for the
// recovery scan: one expired claimed row that died BEFORE delivery to Soul, the
// ONLY candidate, plus rows the rule must NOT touch (GATE-1 dispatched/running
// with expired lease + live claimed). zombie-claimed has attempt=1; verify
// reclaim does NOT reset it (fencing epoch). All rows use the shared
// seedClaimedApplyRun helper from integration_test.go.
func seedReclaimScenario(t *testing.T, f *runnerIntegrationFixture) {
	t.Helper()
	now := time.Now().UTC()
	expired := now.Add(-1 * time.Minute) // lease expired, holder died
	alive := now.Add(10 * time.Minute)   // lease is still alive

	seedIncarnation(t, f.ctx, f.pool, "inc-reclaim")
	// Expired claimed row died BEFORE dispatch and is the ONLY reclaimed row. attempt=1.
	seedClaimedApplyRun(t, f.ctx, f.pool, "zombie-claimed", "h1.example.com", "inc-reclaim", "claimed", 1, expired)
	// GATE-1: dispatched with expired lease is NOT touched; Soul owns after delivery.
	seedClaimedApplyRun(t, f.ctx, f.pool, "dispatched-expired", "h2.example.com", "inc-reclaim", "dispatched", 2, expired)
	// GATE-1: running with expired lease (vestigial) is NOT reclaimed.
	seedClaimedApplyRun(t, f.ctx, f.pool, "zombie-running", "h3.example.com", "inc-reclaim", "running", 3, expired)
	// Live claimed row with future lease is NOT reclaimed.
	seedClaimedApplyRun(t, f.ctx, f.pool, "alive-claimed", "h4.example.com", "inc-reclaim", "claimed", 1, alive)
}

// TestIntegration_Runner_ReclaimApplyRuns_Enabled is the happy path for the
// recovery-lease mechanism THROUGH A REAL Runner tick, not a direct Purger call:
// a real Runner over testcontainers PG+Redis with reclaim_apply_runs enabled
// acquires the lease, dispatches the rule, and the only expired claimed Ward
// returns to planned with claim_by_kid/claim_at/claim_expires_at reset. attempt
// is PRESERVED as the fencing epoch. It also verifies
// keeper_reaper_rule_purged_total{rule="reclaim_apply_runs"} grows to 1.
// GATE-1 rows (dispatched/running expired + live claimed) are untouched, proving
// the Runner path does not differ from direct SQL.
func TestIntegration_Runner_ReclaimApplyRuns_Enabled(t *testing.T) {
	f := newRunnerIntegrationFixture(t)
	seedReclaimScenario(t, f)

	store := writeYAMLAndLoadStore(t, withReclaimRule(t, true))
	reg := obs.NewRegistry()
	metrics := reaper.RegisterReaperMetrics(reg)
	rn, err := reaper.NewRunner(reaper.Deps{
		Purger:  newRunnerExecutor(f.pool),
		Redis:   f.redis,
		Store:   store,
		Holder:  "keeper-int-01",
		Logger:  silentSlog(),
		Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Wait until the rule has run at least once: the signal is zombie-claimed
	// moving to planned.
	waitForCond(t, 8*time.Second, func() bool {
		return countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM apply_runs WHERE apply_id = 'zombie-claimed' AND status = 'planned'") == 1
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	// Expired claimed row is reclaimed: planned, owner/lease reset, attempt is
	// PRESERVED. The next claim, not reclaim, increments the fencing epoch.
	status, attempt, kid := applyRunSnapshot(t, f.ctx, f.pool, "zombie-claimed", "h1.example.com")
	if status != "planned" {
		t.Errorf("zombie-claimed: status = %q, want planned", status)
	}
	if attempt != 1 {
		t.Errorf("zombie-claimed: attempt = %d, want 1 (NOT reset — fencing-epoch)", attempt)
	}
	if kid != nil {
		t.Errorf("zombie-claimed: claim_by_kid = %v, want NULL (claim released)", *kid)
	}

	// GATE-1 through Runner path: dispatched / running expired are untouched.
	if status, _, kid := applyRunSnapshot(t, f.ctx, f.pool, "dispatched-expired", "h2.example.com"); status != "dispatched" || kid == nil {
		t.Errorf("dispatched-expired: status=%q kid=%v; want dispatched + non-NULL kid (Soul owns, do NOT reclaim)", status, kid)
	}
	if status, _, kid := applyRunSnapshot(t, f.ctx, f.pool, "zombie-running", "h3.example.com"); status != "running" || kid == nil {
		t.Errorf("zombie-running: status=%q kid=%v; want running + non-NULL kid (running is no longer reclaimed)", status, kid)
	}
	// Live claimed row is untouched.
	if status, _, kid := applyRunSnapshot(t, f.ctx, f.pool, "alive-claimed", "h4.example.com"); status != "claimed" || kid == nil {
		t.Errorf("alive-claimed: status=%q kid=%v; want claimed + non-NULL kid (alive Ward untouched)", status, kid)
	}

	// purged_total under label rule="reclaim_apply_runs" grew to 1 because
	// exactly one row was reclaimed. Exposition format prints the value after the
	// space: `...{rule="reclaim_apply_runs"} 1`.
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_reaper_rule_purged_total{rule="reclaim_apply_runs"} 1`) {
		t.Errorf("purged_total for reclaim_apply_runs != 1 (1 row reclaimed); got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_reaper_rule_executions_total{rule="reclaim_apply_runs"}`) {
		t.Errorf("executions_total missing for reclaim_apply_runs; got=\n%s", body)
	}
}

// TestIntegration_Runner_ReclaimApplyRuns_Disabled — ★ negative (disabled)
// THROUGH A REAL Runner tick: when reclaim_apply_runs enabled:false, the rule is
// not dispatched, so an expired claimed Ward remains claimed, does NOT return to
// planned, and its owner/lease are preserved. This proves default-OFF really
// protects from recovery before attempt-fencing rollout at the SQL level, not
// only in fakePurger. Other YAML-enabled rules run, proving Runner spun and a
// tick happened; missing reclaim is not caused by the loop failing to start.
func TestIntegration_Runner_ReclaimApplyRuns_Disabled(t *testing.T) {
	f := newRunnerIntegrationFixture(t)
	seedReclaimScenario(t, f)

	// Marker audit record: purge_audit_old with max_age=1ms will delete it,
	// proving Runner actually completed at least one tick.
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO audit_log (audit_id, created_at, event_type, source, payload)
		 VALUES ($1, $2, 'config.reload_succeeded', 'signal', '{}'::jsonb)`,
		audit.NewULID(), time.Now().UTC().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("seed audit marker: %v", err)
	}

	store := writeYAMLAndLoadStore(t, withReclaimRule(t, false))
	rn, err := reaper.NewRunner(reaper.Deps{
		Purger: newRunnerExecutor(f.pool),
		Redis:  f.redis,
		Store:  store,
		Holder: "keeper-int-01",
		Logger: silentSlog(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Wait for proof that a tick happened: marker audit record was deleted by
	// enabled purge_audit_old.
	waitForCond(t, 8*time.Second, func() bool {
		return countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log") == 0
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	// reclaim_apply_runs disabled -> expired claimed row is NOT touched even
	// after a tick: it remains claimed with the previous owner and attempt.
	status, attempt, kid := applyRunSnapshot(t, f.ctx, f.pool, "zombie-claimed", "h1.example.com")
	if status != "claimed" {
		t.Errorf("zombie-claimed: status = %q, want claimed (rule disabled, do NOT reclaim)", status)
	}
	if attempt != 1 {
		t.Errorf("zombie-claimed: attempt = %d, want 1 (untouched)", attempt)
	}
	if kid == nil {
		t.Errorf("zombie-claimed: claim_by_kid = NULL, want non-NULL (rule disabled, claim preserved)")
	}
	// No row must become planned due to reclaim.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM apply_runs WHERE status = 'planned'"); got != 0 {
		t.Errorf("planned rows = %d, want 0 (reclaim disabled, claimed does not move)", got)
	}
}
