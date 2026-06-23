package conductor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/redis"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redis.NewClient(context.Background(), redis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

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

// fakeSpawner — мок [Spawner]: считает вызовы и фиксирует батч/интервал,
// с которыми его дёрнул tick. Возвращает заданное число заспавненных Voyage.
type fakeSpawner struct {
	calls     atomic.Int64
	lastBatch atomic.Int64
	spawned   int64
	err       error
}

func (f *fakeSpawner) Run(_ context.Context, _ time.Duration, batchSize int) (int64, error) {
	f.calls.Add(1)
	f.lastBatch.Store(int64(batchSize))
	return f.spawned, f.err
}

func constFn(d time.Duration) func() time.Duration { return func() time.Duration { return d } }

func baseConfig(rc *redis.Client, sp Spawner) Config {
	return Config{
		Holder:     "keeper-test-01",
		Redis:      rc,
		Logger:     silentLogger(),
		Spawner:    sp,
		IntervalFn: constFn(30 * time.Millisecond),
		LockTTLFn:  constFn(300 * time.Millisecond),
	}
}

func TestNew_ValidatesConfig(t *testing.T) {
	rc, _ := newTestRedis(t)
	full := baseConfig(rc, &fakeSpawner{})
	if _, err := New(full); err != nil {
		t.Fatalf("New(full): %v", err)
	}

	cases := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"empty_holder", func(c *Config) { c.Holder = "" }},
		{"nil_redis", func(c *Config) { c.Redis = nil }},
		{"nil_logger", func(c *Config) { c.Logger = nil }},
		{"nil_spawner", func(c *Config) { c.Spawner = nil }},
		{"nil_interval_fn", func(c *Config) { c.IntervalFn = nil }},
		{"nil_lock_ttl_fn", func(c *Config) { c.LockTTLFn = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := full
			tc.mutate(&c)
			if _, err := New(c); err == nil {
				t.Errorf("New with %s should fail", tc.name)
			}
		})
	}
}

// TestRun_AcquiresLeaseAndSpawns — Conductor захватывает свой lease и на тике
// зовёт Spawner (due-cadence спавнятся). Lease лежит под ключом conductor:leader.
func TestRun_AcquiresLeaseAndSpawns(t *testing.T) {
	rc, mr := newTestRedis(t)
	sp := &fakeSpawner{spawned: 2}
	cfg := baseConfig(rc, sp)

	sch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()

	// Свой lease-ключ (НЕ reaper:leader) — захвачен под нашим holder-ом.
	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(LeaseKey)
		return v == cfg.Holder
	})
	if v, _ := mr.Get("reaper:leader"); v != "" {
		t.Errorf("conductor занял reaper:leader=%q; должен быть свой ключ conductor:leader", v)
	}

	// Immediate-spawn при acquire + последующие тики по interval.
	waitFor(t, 500*time.Millisecond, func() bool { return sp.calls.Load() >= 2 })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}
}

// TestLeaseKey_NotReaper — фиксируем инвариант: lease-ключ Conductor отделён
// от reaper-lease (независимое лидерство, ADR-048 §1).
func TestLeaseKey_NotReaper(t *testing.T) {
	if LeaseKey != "conductor:leader" {
		t.Errorf("LeaseKey = %q; want conductor:leader", LeaseKey)
	}
	if LeaseKey == "reaper:leader" {
		t.Fatal("Conductor lease конфликтует с Reaper lease")
	}
}

// TestRun_TwoInstances_OnlyLeaderSpawns — два conductor-инстанса на один lease:
// Spawner зовётся только у держателя (single-executor спавна).
func TestRun_TwoInstances_OnlyLeaderSpawns(t *testing.T) {
	rc, mr := newTestRedis(t)

	winnerSp := &fakeSpawner{}
	loserSp := &fakeSpawner{}

	winnerCfg := baseConfig(rc, winnerSp)
	winnerCfg.Holder = "keeper-winner"
	winnerCfg.AcquireBackoff = 30 * time.Millisecond

	loserCfg := baseConfig(rc, loserSp)
	loserCfg.Holder = "keeper-loser"
	loserCfg.AcquireBackoff = 30 * time.Millisecond

	winner, err := New(winnerCfg)
	if err != nil {
		t.Fatalf("New winner: %v", err)
	}
	loser, err := New(loserCfg)
	if err != nil {
		t.Fatalf("New loser: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wDone := make(chan error, 1)
	go func() { wDone <- winner.Run(ctx) }()
	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(LeaseKey)
		return v == "keeper-winner"
	})
	lDone := make(chan error, 1)
	go func() { lDone <- loser.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool { return winnerSp.calls.Load() >= 3 })
	if got := loserSp.calls.Load(); got != 0 {
		t.Errorf("loser спавнил %d раз без lease; want 0", got)
	}

	cancel()
	<-wDone
	<-lDone
}

// TestRun_PassesBatchSize — tick передаёт Spawner batch из BatchFn (anti-lavina
// потолок числа due-cadence за тик).
func TestRun_PassesBatchSize(t *testing.T) {
	rc, _ := newTestRedis(t)
	sp := &fakeSpawner{}
	cfg := baseConfig(rc, sp)
	cfg.BatchFn = func() int { return 42 }

	sch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool { return sp.calls.Load() >= 1 })
	if got := sp.lastBatch.Load(); got != 42 {
		t.Errorf("Spawner получил batch=%d; want 42", got)
	}

	cancel()
	<-done
}

// TestRun_SpawnerError_DoesNotCrash — ошибка Spawner.Run не роняет loop: тики
// продолжаются (best-effort фоновое правило).
func TestRun_SpawnerError_DoesNotCrash(t *testing.T) {
	rc, _ := newTestRedis(t)
	sp := &fakeSpawner{err: errors.New("boom")}
	cfg := baseConfig(rc, sp)

	sch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()

	// Несколько тиков, несмотря на ошибки на каждом.
	waitFor(t, 1*time.Second, func() bool { return sp.calls.Load() >= 3 })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}
}

// TestRun_GracefulShutdown — ctx.Done → Spawner перестаёт зваться и lease
// освобождён (ключ удалён из Redis).
func TestRun_GracefulShutdown(t *testing.T) {
	rc, mr := newTestRedis(t)
	sp := &fakeSpawner{}
	cfg := baseConfig(rc, sp)

	sch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(LeaseKey)
		return v == cfg.Holder
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run не вернулся за 2s после cancel — leak")
	}

	if v, _ := mr.Get(LeaseKey); v != "" {
		t.Errorf("lease-ключ всё ещё держится после shutdown: %q", v)
	}
}
