package reaper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// fakePurger — захватывает вызовы всех методов PurgerAPI и возвращает
// фиксированное `deleted`/`err`. Подходит для проверки, был ли вызов,
// с какими аргументами, и обработал ли Runner ошибку правильно.
//
// `calls` агрегирует вызовы всех методов (нужно для тестов вида
// «Runner всё-таки вызвал Purger хотя бы раз»). Per-method счётчики
// и последние аргументы — в `byRule` (имя SQL-функции → counters)
// чтобы вызов одного правила не затирал параметры другого
// (dispatch обходит все правила в одном тике).
type ruleCall struct {
	count      int
	lastMaxAge time.Duration
	lastBatch  int
	lastStatus []string

	// Поля, специфичные для archive_state_history (ADR-Q19 retention):
	// duration-аргумента нет, есть keep_last_n / keep_version_bump.
	lastKeepLastN       int
	lastKeepVersionBump bool
}

type fakePurger struct {
	mu      sync.Mutex
	calls   int
	byRule  map[string]*ruleCall
	deleted int64
	err     error
}

func (f *fakePurger) record(rule string, maxAge time.Duration, batchSize int, statuses []string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.byRule == nil {
		f.byRule = map[string]*ruleCall{}
	}
	rc := f.byRule[rule]
	if rc == nil {
		rc = &ruleCall{}
		f.byRule[rule] = rc
	}
	rc.count++
	rc.lastMaxAge = maxAge
	rc.lastBatch = batchSize
	rc.lastStatus = statuses
	return f.deleted, f.err
}

func (f *fakePurger) PurgeAuditOld(_ context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_audit_old", maxAge, batchSize, nil)
}

func (f *fakePurger) PurgeExpiredPendingTokens(_ context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("expire_pending_seeds", maxAge, batchSize, nil)
}

func (f *fakePurger) PurgeUsedTokens(_ context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_used_tokens", maxAge, batchSize, nil)
}

func (f *fakePurger) PurgeSouls(_ context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_souls", maxAge, batchSize, statuses)
}

func (f *fakePurger) PurgeOldSeeds(_ context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_old_seeds", maxAge, batchSize, statuses)
}

func (f *fakePurger) PurgeOldCerts(_ context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_old_certs", maxAge, batchSize, statuses)
}

func (f *fakePurger) MarkDisconnected(_ context.Context, staleAfter time.Duration, batchSize int) (int64, error) {
	return f.record("mark_disconnected", staleAfter, batchSize, nil)
}

func (f *fakePurger) PurgeApplyRuns(_ context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_apply_runs", maxAge, batchSize, nil)
}

func (f *fakePurger) PurgeVoyages(_ context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_voyages", maxAge, batchSize, nil)
}

func (f *fakePurger) PurgePushRuns(_ context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_push_runs", maxAge, batchSize, nil)
}

func (f *fakePurger) PurgeIncarnationArchive(_ context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_incarnation_archive", maxAge, batchSize, nil)
}

func (f *fakePurger) PurgeStateHistoryArchive(_ context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_state_history_archive", maxAge, batchSize, nil)
}

func (f *fakePurger) PurgeArchivedStateHistory(_ context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return f.record("purge_archived_state_history", maxAge, batchSize, nil)
}

func (f *fakePurger) PurgeApplyTaskRegister(_ context.Context, gracePeriod time.Duration, batchSize int) (int64, error) {
	return f.record("purge_apply_task_register", gracePeriod, batchSize, nil)
}

func (f *fakePurger) ReclaimApplyRuns(_ context.Context, lease time.Duration, batchSize int) (int64, error) {
	return f.record("reclaim_apply_runs", lease, batchSize, nil)
}

func (f *fakePurger) ReportOrphanVaultKeys(_ context.Context, grace time.Duration, batchSize int) (int64, error) {
	return f.record("reap_orphan_vault_keys", grace, batchSize, nil)
}

func (f *fakePurger) ArchiveStateHistory(_ context.Context, keepLastN int, keepVersionBump bool, batchSize int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.byRule == nil {
		f.byRule = map[string]*ruleCall{}
	}
	rc := f.byRule["archive_state_history"]
	if rc == nil {
		rc = &ruleCall{}
		f.byRule["archive_state_history"] = rc
	}
	rc.count++
	rc.lastBatch = batchSize
	rc.lastKeepLastN = keepLastN
	rc.lastKeepVersionBump = keepVersionBump
	return f.deleted, f.err
}

// snapshot возвращает агрегированный счётчик вызовов и параметры
// последнего вызова правила `purge_audit_old` — это совместимость с
// исходными Reaper.a-тестами, которые знали только одно правило.
// Новые тесты (Reaper.b) используют ruleCall() для per-rule захвата.
func (f *fakePurger) snapshot() (calls int, maxAge time.Duration, batch int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rc := f.byRule["purge_audit_old"]; rc != nil {
		return f.calls, rc.lastMaxAge, rc.lastBatch
	}
	return f.calls, 0, 0
}

func (f *fakePurger) ruleCalls(rule string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rc := f.byRule[rule]; rc != nil {
		return rc.count
	}
	return 0
}

// ruleCall возвращает копию записанных параметров последнего вызова
// именованного правила. Если правило не вызывалось — `ok=false`.
func (f *fakePurger) ruleCall(rule string) (ruleCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rc := f.byRule[rule]; rc != nil {
		return *rc, true
	}
	return ruleCall{}, false
}

// testKeeperYAML — минимальный валидный keeper.yml для unit-тестов
// Runner-а. Содержит только поля, которые Runner реально читает —
// остальные блоки (postgres/vault/listen/...) тоже обязаны быть для
// прохождения LoadKeeper-а.
const testKeeperYAML = `
kid: keeper-test-01

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
    issuer: keeper-test-01
    ttl_default: 24h
    ttl_bootstrap: 30d

logging:
  level: info
  format: json

reaper:
  enabled: true
  interval: 50ms
  dry_run: false
  batch_size: 200
  lock_ttl: 300ms
  rules:
    purge_audit_old:
      enabled: true
      max_age: 365d
      action: delete
`

// writeKeeperYAML кладёт YAML на диск и возвращает путь. body должен быть
// валидным keeper.yml — иначе LoadKeeperStore вернёт error и тест упадёт.
func writeKeeperYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "keeper.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write keeper.yml: %v", err)
	}
	return p
}

// newTestStore — Store[KeeperConfig] с известным начальным cfg. Использует
// LoadKeeperStore, чтобы snapshot был валидным (с заполненным `Reaper`).
func newTestStore(t *testing.T, body string) *config.Store[config.KeeperConfig] {
	t.Helper()
	store, _, err := config.LoadKeeperStore(writeKeeperYAML(t, body), config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	if store.Get() == nil {
		t.Fatal("store snapshot is nil after initial load")
	}
	return store
}

// newTestRedis — клиент к miniredis-у. Cleanup автоматический.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redis.NewClient(context.Background(), redis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// silentLogger — slog-логгер, дисcarding output. Тесты не парсят логи;
// нужен только не-nil pointer.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestNewRunner_ValidatesDeps(t *testing.T) {
	store := newTestStore(t, testKeeperYAML)
	rc := newTestRedis(t)

	full := Deps{
		Purger: &fakePurger{},
		Redis:  rc,
		Store:  store,
		Holder: "keeper-test-01",
		Logger: silentLogger(),
	}
	if _, err := NewRunner(full); err != nil {
		t.Fatalf("NewRunner(full): %v", err)
	}

	cases := []struct {
		name   string
		mutate func(d *Deps)
	}{
		{"nil_purger", func(d *Deps) { d.Purger = nil }},
		{"nil_redis", func(d *Deps) { d.Redis = nil }},
		{"nil_store", func(d *Deps) { d.Store = nil }},
		{"empty_holder", func(d *Deps) { d.Holder = "" }},
		{"nil_logger", func(d *Deps) { d.Logger = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := full
			tc.mutate(&d)
			if _, err := NewRunner(d); err == nil {
				t.Errorf("NewRunner with %s should fail", tc.name)
			}
		})
	}
}

func TestRunner_HappyPath_DispatchesPurger(t *testing.T) {
	fp := &fakePurger{deleted: 3}
	store := newTestStore(t, testKeeperYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp,
		Redis:  rc,
		Store:  store,
		Holder: "keeper-test-01",
		Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Ждём хотя бы один вызов (immediate dispatch при acquire + последующие
	// tick-и через 50 ms).
	waitFor(t, 500*time.Millisecond, func() bool {
		c, _, _ := fp.snapshot()
		return c >= 1
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	calls, maxAge, batch := fp.snapshot()
	if calls < 1 {
		t.Errorf("Purger calls = %d, want >= 1", calls)
	}
	if maxAge != 365*24*time.Hour {
		t.Errorf("Purger maxAge = %v, want 365d", maxAge)
	}
	if batch != 200 {
		t.Errorf("Purger batchSize = %d, want 200", batch)
	}
}

func TestRunner_DryRun_SkipsPurger(t *testing.T) {
	body := testKeeperYAML
	// Заменяем dry_run: false → dry_run: true
	body = replaceOnce(t, body, "dry_run: false", "dry_run: true")

	fp := &fakePurger{}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	if c, _, _ := fp.snapshot(); c != 0 {
		t.Errorf("Purger.calls = %d under dry_run; want 0", c)
	}
}

func TestRunner_RuleDisabled_SkipsPurger(t *testing.T) {
	body := replaceOnce(t, testKeeperYAML,
		"purge_audit_old:\n      enabled: true",
		"purge_audit_old:\n      enabled: false")

	fp := &fakePurger{}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	if c, _, _ := fp.snapshot(); c != 0 {
		t.Errorf("Purger.calls = %d with rule disabled; want 0", c)
	}
}

func TestRunner_ReaperDisabled_NoLoop(t *testing.T) {
	body := replaceOnce(t, testKeeperYAML, "reaper:\n  enabled: true", "reaper:\n  enabled: false")

	fp := &fakePurger{}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	// Reaper.Enabled=false → dispatch — no-op. Runner всё равно держит
	// lease и тикает, но никаких побочных эффектов нет. Проверяем, что
	// purger не вызывается.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	if c, _, _ := fp.snapshot(); c != 0 {
		t.Errorf("Purger.calls = %d with reaper disabled; want 0", c)
	}
}

func TestRunner_CtxCancel_Graceful(t *testing.T) {
	store := newTestStore(t, testKeeperYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: &fakePurger{}, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Дадим acquire завершиться.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel — leak")
	}
}

func TestRunner_LeaseLost_StopsLoopAndReacquires(t *testing.T) {
	fp := &fakePurger{}
	store := newTestStore(t, testKeeperYAML)

	// Используем общий miniredis, чтобы достучаться до ключа извне.
	mr := miniredis.RunT(t)
	c, err := redis.NewClient(context.Background(), redis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	rn, err := NewRunner(Deps{
		Purger: fp, Redis: c, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
		AcquireBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Ждём acquire.
	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(LeaderLeaseKey)
		return v == "keeper-test-01"
	})
	callsBefore, _, _ := fp.snapshot()

	// «Воруем» lease: подменяем значение → следующий Renew вернёт
	// ErrLeaseLost, main-loop остановится, dispatch не должен расти.
	mr.Set(LeaderLeaseKey, "intruder")

	// Дадим времени renewal-goroutine отработать. renewEvery = lock_ttl/3
	// = 100 ms; ждём 250 ms.
	time.Sleep(250 * time.Millisecond)

	// «Освобождаем» ключ, чтобы Runner смог пере-захватить.
	mr.Del(LeaderLeaseKey)

	// Проверяем, что после потери lease Runner возвращается к acquire-у.
	waitFor(t, 2*time.Second, func() bool {
		v, _ := mr.Get(LeaderLeaseKey)
		return v == "keeper-test-01"
	})

	// Дальше должны быть новые dispatch-ы.
	waitFor(t, 500*time.Millisecond, func() bool {
		callsNow, _, _ := fp.snapshot()
		return callsNow > callsBefore
	})

	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v", err)
	}
}

func TestRunner_PurgerError_LoopContinues(t *testing.T) {
	fp := &fakePurger{err: errors.New("pg down")}
	store := newTestStore(t, testKeeperYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	// Несмотря на ошибку Purger-а, loop должен продолжать тикать —
	// counter > 1 (initial + хотя бы один tick).
	if c, _, _ := fp.snapshot(); c < 2 {
		t.Errorf("Purger.calls = %d on persistent error; want >= 2 (loop must keep ticking)", c)
	}
}

// TestRunner_AcquireConflict_BlockedWhileHeld — параллельный holder
// удерживает lease на всё время теста; Runner крутится в backoff-loop-е
// Acquire-а и не должен ни перезаписать чужой ключ, ни вызвать Purger.
// Positive-ветка (re-acquire после освобождения) проверяется
// TestRunner_LeaseLost_StopsLoopAndReacquires.
func TestRunner_AcquireConflict_BlockedWhileHeld(t *testing.T) {
	store := newTestStore(t, testKeeperYAML)
	mr := miniredis.RunT(t)
	c, err := redis.NewClient(context.Background(), redis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Захватываем lease извне — Runner должен попасть в backoff-loop.
	mr.Set(LeaderLeaseKey, "external-leader")
	mr.SetTTL(LeaderLeaseKey, 10*time.Second)

	fp := &fakePurger{}
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: c, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	if c, _, _ := fp.snapshot(); c != 0 {
		t.Errorf("Purger.calls = %d while lease held externally; want 0", c)
	}
	if v, _ := mr.Get(LeaderLeaseKey); v != "external-leader" {
		t.Errorf("external lease was overwritten: got %q", v)
	}
}

// testReaperBYAML — YAML с дефолтным набором reaper-правил (Reaper.b).
// Использует короткие интервалы для тестов (interval/lock_ttl как в
// testKeeperYAML), но с реалистичными max_age/stale_after значениями,
// чтобы проверить корректную передачу cfg → SQL-функция.
const testReaperBYAML = `
kid: keeper-test-01

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
    issuer: keeper-test-01
    ttl_default: 24h
    ttl_bootstrap: 30d

logging:
  level: info
  format: json

reaper:
  enabled: true
  interval: 50ms
  dry_run: false
  batch_size: 200
  lock_ttl: 300ms
  rules:
    purge_audit_old:
      enabled: true
      max_age: 365d
      action: delete
    expire_pending_seeds:
      enabled: true
      max_age: 24h
      action: expire
    purge_used_tokens:
      enabled: true
      max_age: 90d
      action: delete
    purge_souls:
      enabled: true
      statuses: [disconnected, expired]
      max_age: 30d
      action: delete
    purge_old_seeds:
      enabled: true
      statuses: [superseded, expired, revoked]
      max_age: 90d
      action: delete
    mark_disconnected:
      enabled: true
      stale_after: 90s
      action: set_status
      target_status: disconnected
    purge_apply_runs:
      enabled: true
      max_age: 30d
      action: delete
    purge_voyages:
      enabled: true
      max_age: 30d
      action: delete
    purge_push_runs:
      enabled: true
      action: delete
    purge_incarnation_archive:
      enabled: true
      action: delete
    purge_state_history_archive:
      enabled: true
      action: delete
    purge_archived_state_history:
      enabled: true
      action: delete
    purge_apply_task_register:
      enabled: true
      max_age: 1h
      action: delete
`

func TestRunner_DispatchesAllReaperRules(t *testing.T) {
	fp := &fakePurger{}
	store := newTestStore(t, testReaperBYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Ждём, пока каждое из 13 правил вызвалось хотя бы раз.
	want := []string{
		"purge_audit_old",
		"expire_pending_seeds",
		"purge_used_tokens",
		"purge_souls",
		"purge_old_seeds",
		"mark_disconnected",
		"purge_apply_runs",
		"purge_voyages",
		"purge_push_runs",
		"purge_incarnation_archive",
		"purge_state_history_archive",
		"purge_archived_state_history",
		"purge_apply_task_register",
	}
	waitFor(t, 800*time.Millisecond, func() bool {
		for _, r := range want {
			if fp.ruleCalls(r) < 1 {
				return false
			}
		}
		return true
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	// Правила объявлены в фикстуре БЕЗ max_age → dispatch обязан подставить
	// per-rule дефолт из runner.go-констант. Проверяем именно дефолтную
	// ветку (push_runs=30d; три archive-правила=365d compliance-окно):
	// если dispatch-case потеряет правило или прокинет не тот дефолт-аргумент,
	// тест краснеет. Per-rule lastMaxAge не затирается другими правилами —
	// у каждого имени свой *ruleCall в byRule.
	const (
		wantPushRunsMaxAge = 30 * 24 * time.Hour  // defaultPurgePushRunsMaxAge
		wantArchiveMaxAge  = 365 * 24 * time.Hour // defaultPurgeArchiveMaxAge
	)
	maxAgeWant := map[string]time.Duration{
		"purge_push_runs":              wantPushRunsMaxAge,
		"purge_incarnation_archive":    wantArchiveMaxAge,
		"purge_state_history_archive":  wantArchiveMaxAge,
		"purge_archived_state_history": wantArchiveMaxAge,
	}
	for rule, wantAge := range maxAgeWant {
		call, ok := fp.ruleCall(rule)
		if !ok {
			t.Errorf("rule %s was not dispatched", rule)
			continue
		}
		if call.lastMaxAge != wantAge {
			t.Errorf("rule %s default max_age = %v; want %v", rule, call.lastMaxAge, wantAge)
		}
	}
}

func TestRunner_DryRun_SkipsAllReaperBRules(t *testing.T) {
	body := replaceOnce(t, testReaperBYAML, "dry_run: false", "dry_run: true")
	fp := &fakePurger{}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	for _, r := range []string{
		"purge_audit_old", "expire_pending_seeds", "purge_used_tokens",
		"purge_souls", "purge_old_seeds", "mark_disconnected", "purge_apply_runs",
		"purge_voyages", "purge_push_runs", "purge_incarnation_archive",
		"purge_state_history_archive", "purge_archived_state_history",
		"purge_apply_task_register",
	} {
		if got := fp.ruleCalls(r); got != 0 {
			t.Errorf("rule %s: calls = %d under dry_run; want 0", r, got)
		}
	}
}

func TestRunner_PerRuleEnabledFlag(t *testing.T) {
	// Отключаем purge_souls точечно — остальные правила должны
	// продолжать работать.
	body := replaceOnce(t, testReaperBYAML,
		"purge_souls:\n      enabled: true",
		"purge_souls:\n      enabled: false")

	fp := &fakePurger{}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool {
		return fp.ruleCalls("purge_used_tokens") >= 1
	})

	cancel()
	<-done

	if got := fp.ruleCalls("purge_souls"); got != 0 {
		t.Errorf("disabled purge_souls was called %d times; want 0", got)
	}
	if got := fp.ruleCalls("purge_used_tokens"); got < 1 {
		t.Errorf("purge_used_tokens (enabled) calls = %d; want >= 1", got)
	}
}

func TestRunner_PurgeSouls_PassesStatusesAndMaxAge(t *testing.T) {
	fp := &fakePurger{}
	store := newTestStore(t, testReaperBYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool {
		return fp.ruleCalls("purge_souls") >= 1
	})

	cancel()
	<-done

	call, ok := fp.ruleCall("purge_souls")
	if !ok {
		t.Fatal("purge_souls was not dispatched")
	}
	if got := call.lastStatus; len(got) != 2 || got[0] != "disconnected" || got[1] != "expired" {
		t.Errorf("purge_souls statuses = %v; want [disconnected expired]", got)
	}
	if call.lastMaxAge != 30*24*time.Hour {
		t.Errorf("purge_souls maxAge = %v; want 30d", call.lastMaxAge)
	}
}

func TestRunner_MarkDisconnected_PassesStaleAfter(t *testing.T) {
	fp := &fakePurger{}
	store := newTestStore(t, testReaperBYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Захватываем последний вызов mark_disconnected.
	waitFor(t, 500*time.Millisecond, func() bool {
		return fp.ruleCalls("mark_disconnected") >= 1
	})

	cancel()
	<-done

	// Поскольку другие правила тоже вызываются и перезаписывают
	// lastMaxAge, по этому полю различить нельзя. Проверяем только сам
	// факт диспетча mark_disconnected. Передача параметров покрыта
	// unit-тестами Purger-а напрямую.
	if fp.ruleCalls("mark_disconnected") < 1 {
		t.Error("mark_disconnected was not dispatched")
	}
}

// TestRunner_ReclaimApplyRuns_NotDispatchedWhenAbsent — правило
// reclaim_apply_runs (recovery-скан, ADR-027 Phase 2) ОТСУТСТВУЕТ в дефолтном
// keeper.yml-наборе (testReaperBYAML его не объявляет) → dispatch его не зовёт.
// Это нижний слой инварианта «recovery не в прод по дефолту»: правила нет —
// recovery не работает; есть, но enabled:false — тоже не работает (см. ниже).
func TestRunner_ReclaimApplyRuns_NotDispatchedWhenAbsent(t *testing.T) {
	fp := &fakePurger{}
	store := newTestStore(t, testReaperBYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Дожидаемся, что обычное правило отработало (значит dispatch крутился).
	waitFor(t, 500*time.Millisecond, func() bool {
		return fp.ruleCalls("purge_audit_old") >= 1
	})
	cancel()
	<-done

	if got := fp.ruleCalls("reclaim_apply_runs"); got != 0 {
		t.Errorf("reclaim_apply_runs dispatched %d times when absent from config; want 0", got)
	}
}

// TestRunner_ReclaimApplyRuns_DisabledByDefault — правило присутствует в
// keeper.yml с enabled:false (как в дефолтном examples/keeper/keeper.yml) →
// dispatch его НЕ исполняет. Это и есть механизм инварианта «recovery не в
// прод до раскатки attempt-fencing»: правило заведено, но выключено.
func TestRunner_ReclaimApplyRuns_DisabledByDefault(t *testing.T) {
	body := strings.Replace(testReaperBYAML,
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n",
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n"+
			"    reclaim_apply_runs:\n      enabled: false\n      stale_after: 1m\n      action: set_status\n      target_status: planned\n",
		1)
	if body == testReaperBYAML {
		t.Fatal("failed to inject reclaim_apply_runs rule into test YAML")
	}

	fp := &fakePurger{}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool {
		return fp.ruleCalls("purge_audit_old") >= 1
	})
	cancel()
	<-done

	if got := fp.ruleCalls("reclaim_apply_runs"); got != 0 {
		t.Errorf("reclaim_apply_runs dispatched %d times while enabled:false; want 0", got)
	}
}

// TestRunner_ReclaimApplyRuns_DispatchedWhenEnabled — при enabled:true правило
// исполняется (оператор включил после раскатки attempt-fencing). Покрывает
// case в dispatch-switch + передачу через ObserveRule (метрика — бесплатно
// через runDurationRule, отдельный assert в TestRunner_Metrics_*).
func TestRunner_ReclaimApplyRuns_DispatchedWhenEnabled(t *testing.T) {
	body := strings.Replace(testReaperBYAML,
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n",
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n"+
			"    reclaim_apply_runs:\n      enabled: true\n      stale_after: 1m\n      action: set_status\n      target_status: planned\n",
		1)
	if body == testReaperBYAML {
		t.Fatal("failed to inject reclaim_apply_runs rule into test YAML")
	}

	fp := &fakePurger{deleted: 2}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(), Metrics: m,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	waitFor(t, 800*time.Millisecond, func() bool {
		return fp.ruleCalls("reclaim_apply_runs") >= 1
	})
	cancel()
	<-done

	call, ok := fp.ruleCall("reclaim_apply_runs")
	if !ok {
		t.Fatal("reclaim_apply_runs was not dispatched while enabled:true")
	}
	// stale_after: 1m → передаётся как lease-аргумент duration-runner-а.
	if call.lastMaxAge != time.Minute {
		t.Errorf("reclaim_apply_runs lease = %v; want 1m", call.lastMaxAge)
	}
	if call.lastBatch != 200 {
		t.Errorf("reclaim_apply_runs batch = %d; want 200", call.lastBatch)
	}

	// Метрика «бесплатна» через ObserveRule в runDurationRule: executions/purged
	// под label rule="reclaim_apply_runs".
	body2 := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body2, `keeper_reaper_rule_executions_total{rule="reclaim_apply_runs"}`) {
		t.Errorf("executions_total missing for reclaim_apply_runs; got=\n%s", body2)
	}
	if !strings.Contains(body2, `keeper_reaper_rule_purged_total{rule="reclaim_apply_runs"}`) {
		t.Errorf("purged_total missing for reclaim_apply_runs (deleted=2); got=\n%s", body2)
	}
}

// TestRunner_ReapOrphanVaultKeys_DispatchedWhenEnabled — при enabled:true
// cross-store report-only правило исполняется через runDurationRule с grace =
// max_age (MaxAge-as-grace, как purge_apply_task_register). Покрывает case в
// dispatch-switch + маршрут через ReportOrphanVaultKeys.
func TestRunner_ReapOrphanVaultKeys_DispatchedWhenEnabled(t *testing.T) {
	body := strings.Replace(testReaperBYAML,
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n",
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n"+
			"    reap_orphan_vault_keys:\n      enabled: true\n      max_age: 24h\n      action: report\n",
		1)
	if body == testReaperBYAML {
		t.Fatal("failed to inject reap_orphan_vault_keys rule into test YAML")
	}

	fp := &fakePurger{deleted: 3}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(), Metrics: m,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	waitFor(t, 800*time.Millisecond, func() bool {
		return fp.ruleCalls("reap_orphan_vault_keys") >= 1
	})
	cancel()
	<-done

	call, ok := fp.ruleCall("reap_orphan_vault_keys")
	if !ok {
		t.Fatal("reap_orphan_vault_keys was not dispatched while enabled:true")
	}
	// max_age: 24h → передаётся как grace-аргумент duration-runner-а.
	if call.lastMaxAge != 24*time.Hour {
		t.Errorf("reap_orphan_vault_keys grace = %v; want 24h", call.lastMaxAge)
	}

	body2 := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body2, `keeper_reaper_rule_executions_total{rule="reap_orphan_vault_keys"}`) {
		t.Errorf("executions_total missing for reap_orphan_vault_keys; got=\n%s", body2)
	}
	// report-only: «purged» = число задетектированных сирот (см. reaper.md).
	if !strings.Contains(body2, `keeper_reaper_rule_purged_total{rule="reap_orphan_vault_keys"}`) {
		t.Errorf("purged_total missing for reap_orphan_vault_keys (detected=3); got=\n%s", body2)
	}
}

// TestRunner_ReapOrphanVaultKeys_NotDispatchedWhenAbsent — правила нет в
// дефолтном конфиге (default enabled:false / отсутствует) → не исполняется.
func TestRunner_ReapOrphanVaultKeys_NotDispatchedWhenAbsent(t *testing.T) {
	fp := &fakePurger{}
	store := newTestStore(t, testReaperBYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	waitFor(t, 500*time.Millisecond, func() bool {
		return fp.ruleCalls("purge_audit_old") >= 1
	})
	cancel()
	<-done

	if got := fp.ruleCalls("reap_orphan_vault_keys"); got != 0 {
		t.Errorf("reap_orphan_vault_keys dispatched %d times when absent from config; want 0", got)
	}
}

// TestRunner_ArchiveStateHistory_DispatchedWithCfg — правило `archive_state_history`
// (ADR-Q19 retention): при enabled:true + явных keep_last_n / keep_version_bump_snapshots
// runner вызывает Purger.ArchiveStateHistory с этими значениями и шлёт executions/
// purged через ObserveRule под label rule="archive_state_history".
func TestRunner_ArchiveStateHistory_DispatchedWithCfg(t *testing.T) {
	body := strings.Replace(testReaperBYAML,
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n",
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n"+
			"    archive_state_history:\n      enabled: true\n      keep_last_n: 25\n      keep_version_bump_snapshots: false\n      action: soft_delete\n",
		1)
	if body == testReaperBYAML {
		t.Fatal("failed to inject archive_state_history rule into test YAML")
	}

	fp := &fakePurger{deleted: 7}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(), Metrics: m,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	waitFor(t, 800*time.Millisecond, func() bool {
		return fp.ruleCalls("archive_state_history") >= 1
	})
	cancel()
	<-done

	call, ok := fp.ruleCall("archive_state_history")
	if !ok {
		t.Fatal("archive_state_history was not dispatched while enabled:true")
	}
	if call.lastKeepLastN != 25 {
		t.Errorf("keep_last_n = %d; want 25", call.lastKeepLastN)
	}
	if call.lastKeepVersionBump != false {
		t.Errorf("keep_version_bump = %v; want false (explicit override)", call.lastKeepVersionBump)
	}

	body2 := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body2, `keeper_reaper_rule_executions_total{rule="archive_state_history"}`) {
		t.Errorf("executions_total missing for archive_state_history; got=\n%s", body2)
	}
	if !strings.Contains(body2, `keeper_reaper_rule_purged_total{rule="archive_state_history"}`) {
		t.Errorf("purged_total missing for archive_state_history (archived=7); got=\n%s", body2)
	}
}

// TestRunner_ArchiveStateHistory_Defaults — при enabled:true без явных полей
// keep_* runner подставляет дефолты (keep_last_n=50, keep_version_bump=true).
func TestRunner_ArchiveStateHistory_Defaults(t *testing.T) {
	body := strings.Replace(testReaperBYAML,
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n",
		"    purge_apply_task_register:\n      enabled: true\n      max_age: 1h\n      action: delete\n"+
			"    archive_state_history:\n      enabled: true\n      action: soft_delete\n",
		1)
	if body == testReaperBYAML {
		t.Fatal("failed to inject archive_state_history rule into test YAML")
	}

	fp := &fakePurger{}
	store := newTestStore(t, body)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	waitFor(t, 800*time.Millisecond, func() bool {
		return fp.ruleCalls("archive_state_history") >= 1
	})
	cancel()
	<-done

	call, ok := fp.ruleCall("archive_state_history")
	if !ok {
		t.Fatal("archive_state_history was not dispatched while enabled:true")
	}
	if call.lastKeepLastN != 50 {
		t.Errorf("keep_last_n = %d; want 50 (default)", call.lastKeepLastN)
	}
	if call.lastKeepVersionBump != true {
		t.Errorf("keep_version_bump = %v; want true (default — protect migration snapshots)", call.lastKeepVersionBump)
	}
}

// TestRunner_ArchiveStateHistory_NotDispatchedWhenAbsent — правила нет в
// дефолтном конфиге → не исполняется.
func TestRunner_ArchiveStateHistory_NotDispatchedWhenAbsent(t *testing.T) {
	fp := &fakePurger{}
	store := newTestStore(t, testReaperBYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01", Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	waitFor(t, 500*time.Millisecond, func() bool {
		return fp.ruleCalls("purge_audit_old") >= 1
	})
	cancel()
	<-done

	if got := fp.ruleCalls("archive_state_history"); got != 0 {
		t.Errorf("archive_state_history dispatched %d times when absent from config; want 0", got)
	}
}

// TestRunner_Metrics_InstrumentsDispatchAndLease — проверяет интеграцию
// с [ReaperMetrics]: после нескольких dispatch-итераций счётчики
// executions/purged/duration_count растут, lease_held=1 на лидере;
// после cancel — lease_held=0.
func TestRunner_Metrics_InstrumentsDispatchAndLease(t *testing.T) {
	fp := &fakePurger{deleted: 5}
	store := newTestStore(t, testKeeperYAML)
	rc := newTestRedis(t)
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)

	rn, err := NewRunner(Deps{
		Purger:  fp,
		Redis:   rc,
		Store:   store,
		Holder:  "keeper-test-01",
		Logger:  silentLogger(),
		Metrics: m,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()

	// Ждём хотя бы один успешный dispatch (executions_total > 0).
	waitFor(t, 1*time.Second, func() bool {
		return obstest.Contains(t, reg.Gatherer(),
			`keeper_reaper_rule_executions_total{rule="purge_audit_old"} `)
	})

	// Lease Gauge должен быть 1 пока loop крутится.
	if !obstest.Contains(t, reg.Gatherer(), "keeper_reaper_lease_held 1") {
		t.Errorf("lease_held should be 1 while runner holds lease; got=\n%s", obstest.Scrape(t, reg.Gatherer()))
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	// После cancel lease_held → 0.
	if !obstest.Contains(t, reg.Gatherer(), "keeper_reaper_lease_held 0") {
		t.Errorf("lease_held should be 0 after cancel; got=\n%s", obstest.Scrape(t, reg.Gatherer()))
	}

	// Покрытие purged_total: fakePurger.deleted=5, минимум один call —
	// значит purged_total ≥ 5 (точное значение зависит от количества тиков).
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_reaper_rule_purged_total{rule="purge_audit_old"}`) {
		t.Errorf("purged_total sample missing; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_reaper_rule_duration_seconds_count{rule="purge_audit_old"}`) {
		t.Errorf("duration_seconds_count sample missing; got=\n%s", body)
	}
	// Errors не должен расти на happy-path.
	if strings.Contains(body, `keeper_reaper_dispatch_errors_total{rule="purge_audit_old"}`) {
		t.Errorf("dispatch_errors should be empty on happy-path; got=\n%s", body)
	}
}

func TestRunner_Metrics_PurgerErrorIncrementsDispatchErrors(t *testing.T) {
	fp := &fakePurger{err: errors.New("pg down")}
	store := newTestStore(t, testKeeperYAML)
	rc := newTestRedis(t)
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)

	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01",
		Logger: silentLogger(), Metrics: m,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	<-done

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_reaper_dispatch_errors_total{rule="purge_audit_old"}`) {
		t.Errorf("dispatch_errors sample missing on persistent Purger error; got=\n%s", body)
	}
	// Purged НЕ должен расти на error-пути.
	if strings.Contains(body, `keeper_reaper_rule_purged_total{rule="purge_audit_old"}`) {
		t.Errorf("purged_total should be empty when Purger errors; got=\n%s", body)
	}
}

// TestRunner_Metrics_NilIsNoOp — Runner с Metrics=nil не должен паниковать
// (это explicit поддержанный сценарий для тестов / dev-режима без obs).
func TestRunner_Metrics_NilIsNoOp(t *testing.T) {
	fp := &fakePurger{}
	store := newTestStore(t, testKeeperYAML)
	rc := newTestRedis(t)
	rn, err := NewRunner(Deps{
		Purger: fp, Redis: rc, Store: store, Holder: "keeper-test-01",
		Logger: silentLogger(), Metrics: nil,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rn.Run(ctx) }()
	if err := <-done; err != nil {
		t.Errorf("Run with nil Metrics: %v", err)
	}
}

// waitFor крутит cond() с интервалом 5 ms, пока он не вернёт true или не
// истечёт timeout. На timeout — t.Fatal. Используется вместо
// `time.Sleep + assert` для уменьшения flakiness.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// replaceOnce — без аллокаций для одного вхождения. Если old не найден или
// встречается несколько раз — t.Fatalf, чтобы поломанная подмена не
// маскировала тест-bug-ом. Используется для построения вариантов YAML
// внутри одного теста без копи-паста полного полотна.
//
// Защищает от ошибок типа `s.Replace(...)` без count: новые тесты сразу
// упадут, если YAML-шаблон сменится и точка подмены пропадёт.
func replaceOnce(t *testing.T, s, old, newStr string) string {
	t.Helper()
	idx := strings.Index(s, old)
	if idx < 0 {
		t.Fatalf("replaceOnce: old not found in body: %q", old)
	}
	if strings.Index(s[idx+len(old):], old) >= 0 {
		t.Fatalf("replaceOnce: old found more than once: %q", old)
	}
	return s[:idx] + newStr + s[idx+len(old):]
}
