//go:build integration

package reaper_test

// End-to-end integration-тесты Runner-а (Reaper.c) — verify dispatch +
// lease + dry_run + per-rule scheduling через реальный Runner.Run-loop
// поверх testcontainers PG + Redis.
//
// Per-rule SQL-функции отдельно покрыты [integration_test.go] (Reaper.b).
// Здесь — следующий уровень: что Runner.Run() корректно соединяет cfg
// (`reaper.rules.*`) с Purger-вызовами, что lease реально захватывается
// в Redis и держит конкурентного Runner-а в acquire-backoff-loop.
//
// Запуск:
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

// reaperLeaderKey — копия [reaper.leaseKey], зафиксированного в
// docs/keeper/reaper.md как инвариант кластера. Дублируем как локальную
// константу external-test-пакета (доступа к unexported нет), а смену
// значения отслеживает docs/keeper/reaper.md + [reaper.leaseKey] синхронно.
const reaperLeaderKey = "reaper:leader"

// runnerExecutor зеркалит составной исполнитель cmd/keeper: Purger + nil-Vault
// VaultReconciler. Эти E2E-тесты не включают reap_orphan_vault_keys в YAML,
// поэтому Vault не нужен (vault=nil → degrade, но правило не вызывается).
type runnerExecutor struct {
	*reaper.Purger
	*reaper.VaultReconciler
}

// newRunnerExecutor собирает PurgerAPI-совместимый исполнитель над pool.
func newRunnerExecutor(pool *pgxpool.Pool) *runnerExecutor {
	return &runnerExecutor{
		Purger:          reaper.NewPurger(pool),
		VaultReconciler: reaper.NewVaultReconciler(nil, nil, slog.Default(), nil),
	}
}

// runnerIntegrationFixture — общий setup: PG + Redis + чистые таблицы.
// Skip-ит, если контейнеры недоступны (см. TestMain). Между тестами
// идемпотентно очищает souls/audit_log + ключ lease.
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

// reset — очищает state между подтестами. Покрывает все таблицы, которые
// трогает Reaper, плюс Redis-ключ lease.
func (f *runnerIntegrationFixture) reset(t *testing.T) {
	t.Helper()
	resetIdentityTables(t, f.ctx, f.pool)
	if _, err := f.pool.Exec(f.ctx, "TRUNCATE audit_log"); err != nil {
		t.Fatalf("truncate audit_log: %v", err)
	}
	// incarnation CASCADE снимает связанные apply_runs (FK ON DELETE CASCADE).
	if _, err := f.pool.Exec(f.ctx, "TRUNCATE incarnation CASCADE"); err != nil {
		t.Fatalf("truncate incarnation: %v", err)
	}
	delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flushLeaderKey(t, delCtx, f.redis)
}

// flushLeaderKey — удаляет ключ lease через ephemeral Acquire+Release.
// Без unexported underlying() единственный способ снять чужой ключ —
// дождаться истечения TTL; для тестового изолированного Redis-а удобнее
// перезаписать через короткий TTL и тут же Release-нуть.
//
// Если ключ свободен — Acquire успешен, Release удаляет; если занят чужим
// holder-ом (быть не должно — между тестами reset), Release-CAS не сработает,
// и мы просто подождём истечения TTL предыдущего теста (lock_ttl коротки).
func flushLeaderKey(t *testing.T, ctx context.Context, rc *redis.Client) {
	t.Helper()
	l, err := redis.Acquire(ctx, rc, reaperLeaderKey, "test-reset", 100*time.Millisecond)
	if err != nil {
		// Если ErrLeaseTaken — ключ держит предыдущий тест; ждём истечения.
		// lock_ttl в тестовом YAML = 300ms, поэтому 500ms достаточно.
		time.Sleep(500 * time.Millisecond)
		return
	}
	_ = l.Release(ctx)
}

// waitLeaderKeyFree крутит Acquire+Release ephemeral-lease до успеха: гарантирует,
// что reaperLeaderKey свободен ПЕРЕД захватом внешнего lease в
// [TestIntegration_Runner_LeaseConflictBlocks]. Под нагрузкой ./... ключ могут
// удерживать догорающие Runner-ы соседних тестов; ждём до 5s (lock_ttl тестов
// короткие), на провал — t.Fatal (не маскируем застрявший ключ).
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
	t.Fatalf("reaperLeaderKey %q не освободился за 5s (застрял от соседнего теста)", reaperLeaderKey)
}

// runnerIntegrationYAML — minimum keeper.yml с включёнными всеми 6 reaper-rules.
// Отличия от runner_test.go::testReaperBYAML:
//   - max_age/stale_after выставлены очень коротко (1ms / 1s), чтобы
//     real SQL-функции находили seeded "просроченные" записи на любом
//     timestamp-сдвиге между insert и dispatch.
//   - batch_size — большой (1000), чтобы за один tick подмести весь seed.
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

// writeYAMLAndLoadStore кладёт body в tempfile и возвращает Store через
// LoadKeeperStore. Validate-ошибки → t.Fatalf (тест-bug, не runtime).
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

// replaceOnceStr — копия [reaper.replaceOnce] для external-пакета.
// Дублируем, чтобы не выносить test-helper в production-код.
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

// silentSlog — discarding-logger для интеграционных тестов. Логи loop-а
// шумные на 200ms-интервале; они не валидируются ассертами.
func silentSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// seedAllRuleData заполняет PG записями, которые попадают под все 6 правил
// при max_age=1ms / stale_after=1s. Возвращаемые счётчики используются
// для assert-ов «affected > 0».
type seededCounts struct {
	audit, pendingTokens, usedTokens, souls, seeds, stale int
	// applyFinished — старые finished apply_runs (попадают под purge);
	// applyRunning — running apply_runs (никогда не purge).
	applyFinished, applyRunning int
}

func seedAllRuleData(t *testing.T, f *runnerIntegrationFixture) seededCounts {
	t.Helper()
	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)
	stale := now.Add(-10 * time.Minute)

	// audit_log: 2 старые записи (попадают под purge_audit_old с max_age=1ms).
	for i := 0; i < 2; i++ {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO audit_log (audit_id, created_at, event_type, source, payload)
			 VALUES ($1, $2, 'config.reload_succeeded', 'signal', '{}'::jsonb)`,
			audit.NewULID(), old); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}

	// souls для FK-привязки токенов / seed-ов.
	//   - 2 disconnected старых → попадут под purge_souls.
	//   - 2 connected stale → попадут под mark_disconnected.
	//   - 2 connected pending-токен-хоста + 2 used-токен-хоста + 1 seed-host —
	//     для FK от bootstrap_tokens / soul_seeds. Partial unique index
	//     `bootstrap_tokens_active_by_sid_idx` требует ≤1 pending токена
	//     на sid, поэтому под каждый pending-токен — свой sid.
	seedSoul(t, f.ctx, f.pool, "disc-1.example.com", "disconnected", old, &old)
	seedSoul(t, f.ctx, f.pool, "disc-2.example.com", "disconnected", old, &old)
	seedSoul(t, f.ctx, f.pool, "stale-1.example.com", "connected", old, &stale)
	seedSoul(t, f.ctx, f.pool, "stale-2.example.com", "connected", old, &stale)
	seedSoul(t, f.ctx, f.pool, "pend-1.example.com", "connected", old, &now)
	seedSoul(t, f.ctx, f.pool, "pend-2.example.com", "connected", old, &now)
	seedSoul(t, f.ctx, f.pool, "used-1.example.com", "connected", old, &now)
	seedSoul(t, f.ctx, f.pool, "used-2.example.com", "connected", old, &now)
	seedSoul(t, f.ctx, f.pool, "seed-host.example.com", "connected", old, &now)

	// bootstrap_tokens: 2 expired-pending (по одному на sid) + 2 used-старых.
	// Used-токены могут лежать на тех же sid, что и used-host-1/2 — после
	// used_at IS NOT NULL partial-index не действует, ограничения нет.
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

	// soul_seeds: 2 superseded + 1 expired-сертификат старых.
	seedSeed(t, f.ctx, f.pool, "seed-host.example.com", "superseded", old, "s-old-1")
	seedSeed(t, f.ctx, f.pool, "seed-host.example.com", "superseded", old, "s-old-2")
	seedSeed(t, f.ctx, f.pool, "seed-host.example.com", "expired", old, "s-old-3")

	// apply_runs: incarnation + 2 старых finished (попадают под
	// purge_apply_runs с max_age=1ms) + 1 running (никогда не purge).
	seedIncarnation(t, f.ctx, f.pool, "inc-1")
	seedApplyRun(t, f.ctx, f.pool, "ar-old-1", "seed-host.example.com", "inc-1", "success", old, &old)
	seedApplyRun(t, f.ctx, f.pool, "ar-old-2", "seed-host.example.com", "inc-1", "failed", old, &old)
	seedApplyRun(t, f.ctx, f.pool, "ar-running", "seed-host.example.com", "inc-1", "running", old, nil)

	return seededCounts{
		audit: 2, pendingTokens: 2, usedTokens: 2, souls: 2, seeds: 3, stale: 2,
		applyFinished: 2, applyRunning: 1,
	}
}

// countRow — обёртка для COUNT(*)-запросов в assert-ах.
func countRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// waitForCond крутит cond() с маленьким шагом до timeout-а; на провал — t.Fatal.
// Локальная копия [reaper.waitFor] — internal-helper не виден из external test pkg.
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

// TestIntegration_Runner_E2E_AllRules — главный happy-path:
// поднимаем Runner.Run, seed-data на все 6 rule-ов, ждём один tick;
// verify, что после выполнения каждая таблица сократилась/обновилась.
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

	// Ждём, пока каждое правило отработало хотя бы один раз. Признак —
	// падение row-count-ов до ожидаемых значений.
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
		// mark_disconnected: stale-1/2 должны стать disconnected.
		stale := countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM souls WHERE status = 'disconnected' AND sid LIKE 'stale-%'")
		// purge_apply_runs: finished удалены, running остаётся.
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

	// Финальные ассерты — формальная гарантия (даже если waitForCond
	// прошёл, фиксируем точные числа в логе теста для bisect-а).
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log"); got != 0 {
		t.Errorf("audit_log left = %d, want 0", got)
	}
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM bootstrap_tokens"); got != 0 {
		t.Errorf("bootstrap_tokens left = %d, want 0", got)
	}
	// soul_seeds: остаются только active (их в seed нет → 0 в этом seed-е).
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM soul_seeds"); got != 0 {
		t.Errorf("soul_seeds left = %d, want 0", got)
	}
	// souls: 9 изначально − 2 disc (удалены purge_souls) = 7.
	// stale-1/2 после mark_disconnected стали disconnected, но
	// last_seen=10мин < max_age=1h → НЕ удаляются purge_souls.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM souls"); got != 7 {
		t.Errorf("souls left = %d, want 7", got)
	}
	if got := countRow(t, f.ctx, f.pool,
		"SELECT COUNT(*) FROM souls WHERE sid LIKE 'stale-%' AND status = 'disconnected'"); got != int64(seeded.stale) {
		t.Errorf("stale-* now disconnected = %d, want %d", got, seeded.stale)
	}
	// apply_runs: 2 finished удалены purge_apply_runs, 1 running выжил.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM apply_runs"); got != int64(seeded.applyRunning) {
		t.Errorf("apply_runs left = %d, want %d (only running survives)", got, seeded.applyRunning)
	}
	if got := countRow(t, f.ctx, f.pool,
		"SELECT COUNT(*) FROM apply_runs WHERE status = 'running'"); got != int64(seeded.applyRunning) {
		t.Errorf("apply_runs running left = %d, want %d (running never purged)", got, seeded.applyRunning)
	}
}

// TestIntegration_Runner_DryRunSkipsAll — dry_run: true → ни одна
// строка не должна быть удалена / обновлена ни одним из 6 rule-ов.
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

	// Даём loop-у несколько тиков (interval=200ms) на отработку. На
	// dry_run ничего меняться не должно, поэтому ждём фиксированное
	// время и затем проверяем counts.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	// audit_log: исходные 2 на месте.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log"); got != int64(seeded.audit) {
		t.Errorf("audit_log = %d, want %d (dry_run should skip)", got, seeded.audit)
	}
	// bootstrap_tokens: исходные 4 на месте.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM bootstrap_tokens"); got != 4 {
		t.Errorf("bootstrap_tokens = %d, want 4 (dry_run)", got)
	}
	// soul_seeds: 3 на месте.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM soul_seeds"); got != int64(seeded.seeds) {
		t.Errorf("soul_seeds = %d, want %d (dry_run)", got, seeded.seeds)
	}
	// souls: 9 (2 disc + 2 stale + 2 pend + 2 used + 1 seed-host), все в
	// исходных статусах.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM souls"); got != 9 {
		t.Errorf("souls = %d, want 9 (dry_run)", got)
	}
	// mark_disconnected проверяем отдельно: stale-* всё ещё connected.
	if got := countRow(t, f.ctx, f.pool,
		"SELECT COUNT(*) FROM souls WHERE sid LIKE 'stale-%' AND status = 'connected'"); got != int64(seeded.stale) {
		t.Errorf("stale-souls still connected = %d, want %d (dry_run skipped mark_disconnected)", got, seeded.stale)
	}
	// apply_runs: 3 (2 finished + 1 running) на месте.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM apply_runs"); got != int64(seeded.applyFinished+seeded.applyRunning) {
		t.Errorf("apply_runs = %d, want %d (dry_run)", got, seeded.applyFinished+seeded.applyRunning)
	}
}

// TestIntegration_Runner_PerRuleEnabled — точечное отключение одного
// правила (purge_audit_old) не должно влиять на остальные. Проверяем,
// что audit_log остаётся нетронутым, а bootstrap_tokens / souls
// обработаны.
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

	// Ждём, пока все ВКЛЮЧЁННЫЕ правила отработают.
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

	// purge_audit_old отключён → audit_log нетронут.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log"); got != int64(seeded.audit) {
		t.Errorf("audit_log = %d, want %d (purge_audit_old disabled)", got, seeded.audit)
	}
}

// TestIntegration_Runner_LeaseConflictBlocks — внешний holder удерживает
// lease в Redis; Runner крутится в acquire-backoff-loop и не должен
// ни перезаписать чужой ключ, ни вызвать ни одного правила.
func TestIntegration_Runner_LeaseConflictBlocks(t *testing.T) {
	f := newRunnerIntegrationFixture(t)
	seeded := seedAllRuleData(t, f)

	// reaperLeaderKey фиксирован (= prod-инвариант), поэтому соседние reaper-тесты
	// делят его в одном per-package Redis-контейнере. Перед захватом внешнего
	// lease дожидаемся, пока ключ освободится от предыдущего теста — иначе под
	// нагрузкой параллельного ./... external Acquire ловит чужой stale-ключ и тест
	// флапает. waitLeaderKeyFree крутит Acquire+Release до успеха.
	waitLeaderKeyFree(t, f.redis)

	// Захватываем lease извне с ЩЕДРЫМ TTL: под нагрузкой параллельного прогона
	// wall-clock от Acquire до финального probe может растянуться, а истечение
	// внешнего lease раньше probe дало бы ложный успех конкурента. 60s заведомо
	// переживает (тест-ctx 1.5s + load-slack). Holder ≠ Runner-holder → Renew-CAS
	// конкурента не сработает.
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

	// Никакое правило не должно отработать — лидер чужой.
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
	// Внешний lease всё ещё держится конкурентом (косвенная проверка —
	// повторный Acquire от другого holder-а возвращает ErrLeaseTaken).
	if _, err := redis.Acquire(f.ctx, f.redis, reaperLeaderKey, "probe", time.Second); err == nil {
		t.Errorf("competing Acquire succeeded; expected ErrLeaseTaken")
	}
}

// reclaimApplyRunsRuleYAML — блок правила reclaim_apply_runs, вставляемый в
// runnerIntegrationYAML после purge_apply_runs. По дефолту правило ВЫКЛЮЧЕНО
// (включается только при раскатанном attempt-fencing, см. purger.go), поэтому
// в общий YAML оно не входит — тесты подставляют его явно с нужным enabled.
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

// withReclaimRule подставляет блок reclaim_apply_runs в runnerIntegrationYAML
// сразу после purge_apply_runs, выставляя его enabled в нужное значение.
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

// seedReclaimScenario засевает на реальный PG полный набор apply_runs под
// recovery-скан: один протухший claimed (умер ДО отдачи Soul-у — ЕДИНСТВЕННЫЙ
// кандидат), плюс строки, которые правило НЕ должно трогать (GATE-1
// dispatched/running с истёкшим lease + живой claimed). zombie-claimed несёт
// attempt=1 — проверяем, что reclaim его НЕ сбрасывает (fencing-epoch). Все —
// через общий helper seedClaimedApplyRun (integration_test.go).
func seedReclaimScenario(t *testing.T, f *runnerIntegrationFixture) {
	t.Helper()
	now := time.Now().UTC()
	expired := now.Add(-1 * time.Minute) // lease протух (holder умер)
	alive := now.Add(10 * time.Minute)   // lease ещё живой

	seedIncarnation(t, f.ctx, f.pool, "inc-reclaim")
	// Протухший claimed — умер ДО dispatch, ЕДИНСТВЕННЫЙ реклеймится. attempt=1.
	seedClaimedApplyRun(t, f.ctx, f.pool, "zombie-claimed", "h1.example.com", "inc-reclaim", "claimed", 1, expired)
	// GATE-1: dispatched с протухшим lease — НЕ трогать (Soul владеет после отдачи).
	seedClaimedApplyRun(t, f.ctx, f.pool, "dispatched-expired", "h2.example.com", "inc-reclaim", "dispatched", 2, expired)
	// GATE-1: running с протухшим lease (vestigial) — НЕ реклеймится.
	seedClaimedApplyRun(t, f.ctx, f.pool, "zombie-running", "h3.example.com", "inc-reclaim", "running", 3, expired)
	// Живой claimed (lease в будущем) — НЕ реклеймится.
	seedClaimedApplyRun(t, f.ctx, f.pool, "alive-claimed", "h4.example.com", "inc-reclaim", "claimed", 1, alive)
}

// TestIntegration_Runner_ReclaimApplyRuns_Enabled — ★ happy-path механизма
// recovery-lease ЧЕРЕЗ РЕАЛЬНЫЙ Runner-tick (не прямой Purger-вызов): реальный
// Runner поверх testcontainers PG+Redis с включённым reclaim_apply_runs
// захватывает lease, диспетчит правило, и единственный протухший claimed-Ward
// возвращается в planned со сбросом claim_by_kid/claim_at/claim_expires_at;
// attempt СОХРАНЯЕТСЯ (fencing-epoch). Параллельно проверяет, что метрика
// keeper_reaper_rule_purged_total{rule="reclaim_apply_runs"} выросла до 1.
// GATE-1-строки (dispatched/running expired + живой claimed) не тронуты —
// доказывает, что Runner-путь не отличается от прямого SQL.
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

	// Ждём, пока правило отработает хотя бы раз: признак — zombie-claimed
	// перешёл в planned.
	waitForCond(t, 8*time.Second, func() bool {
		return countRow(t, f.ctx, f.pool,
			"SELECT COUNT(*) FROM apply_runs WHERE apply_id = 'zombie-claimed' AND status = 'planned'") == 1
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	// Протухший claimed реклеймнут: planned, владелец/lease сброшены, attempt
	// СОХРАНЁН (fencing-epoch инкрементит следующий claim, не reclaim).
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

	// GATE-1 через Runner-путь: dispatched / running expired не тронуты.
	if status, _, kid := applyRunSnapshot(t, f.ctx, f.pool, "dispatched-expired", "h2.example.com"); status != "dispatched" || kid == nil {
		t.Errorf("dispatched-expired: status=%q kid=%v; want dispatched + non-NULL kid (Soul владеет, НЕ реклеймить)", status, kid)
	}
	if status, _, kid := applyRunSnapshot(t, f.ctx, f.pool, "zombie-running", "h3.example.com"); status != "running" || kid == nil {
		t.Errorf("zombie-running: status=%q kid=%v; want running + non-NULL kid (running больше не реклеймится)", status, kid)
	}
	// Живой claimed не тронут.
	if status, _, kid := applyRunSnapshot(t, f.ctx, f.pool, "alive-claimed", "h4.example.com"); status != "claimed" || kid == nil {
		t.Errorf("alive-claimed: status=%q kid=%v; want claimed + non-NULL kid (alive Ward untouched)", status, kid)
	}

	// Метрика purged_total под label rule="reclaim_apply_runs" доросла до 1
	// (реклеймнута ровно одна строка). Exposition-формат печатает значение
	// после пробела: `...{rule="reclaim_apply_runs"} 1`.
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_reaper_rule_purged_total{rule="reclaim_apply_runs"} 1`) {
		t.Errorf("purged_total for reclaim_apply_runs != 1 (реклеймнута 1 строка); got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_reaper_rule_executions_total{rule="reclaim_apply_runs"}`) {
		t.Errorf("executions_total missing for reclaim_apply_runs; got=\n%s", body)
	}
}

// TestIntegration_Runner_ReclaimApplyRuns_Disabled — ★ negative (disabled)
// ЧЕРЕЗ РЕАЛЬНЫЙ Runner-tick: при reclaim_apply_runs enabled:false правило не
// диспетчится, поэтому протухший claimed-Ward остаётся claimed (НЕ возвращается
// в planned) и его владелец/lease сохранены. Доказывает, что default-OFF
// реально защищает от recovery до раскатки attempt-fencing на SQL-уровне (а не
// только на fakePurger). Прочие правила (включённые в YAML) отрабатывают, что
// гарантирует: Runner крутился и tick случился — отсутствие реклейма не из-за
// того, что loop не запустился.
func TestIntegration_Runner_ReclaimApplyRuns_Disabled(t *testing.T) {
	f := newRunnerIntegrationFixture(t)
	seedReclaimScenario(t, f)

	// Маркерная audit-запись: правило purge_audit_old (max_age=1ms) её снесёт —
	// признак, что Runner реально провёл хотя бы один tick.
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

	// Ждём доказательства, что tick случился: маркерная audit-запись снесена
	// включённым purge_audit_old.
	waitForCond(t, 8*time.Second, func() bool {
		return countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM audit_log") == 0
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	// reclaim_apply_runs disabled → протухший claimed НЕ тронут даже после
	// прошедшего tick-а: остаётся claimed с прежним владельцем и attempt.
	status, attempt, kid := applyRunSnapshot(t, f.ctx, f.pool, "zombie-claimed", "h1.example.com")
	if status != "claimed" {
		t.Errorf("zombie-claimed: status = %q, want claimed (правило disabled — НЕ реклеймить)", status)
	}
	if attempt != 1 {
		t.Errorf("zombie-claimed: attempt = %d, want 1 (не тронут)", attempt)
	}
	if kid == nil {
		t.Errorf("zombie-claimed: claim_by_kid = NULL, want non-NULL (правило disabled — claim сохранён)")
	}
	// Ни одна строка не должна стать planned из-за reclaim.
	if got := countRow(t, f.ctx, f.pool, "SELECT COUNT(*) FROM apply_runs WHERE status = 'planned'"); got != 0 {
		t.Errorf("planned rows = %d, want 0 (reclaim disabled — claimed не двигается)", got)
	}
}
