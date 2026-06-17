package leaderloop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/redis"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newTestRedis — клиент к свежему miniredis-у. Cleanup автоматический.
func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redis.NewClient(context.Background(), redis.Config{Addr: mr.Addr()})
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// tickCounter — атомарный счётчик вызовов tick-callback-а.
type tickCounter struct{ n atomic.Int64 }

func (c *tickCounter) tick(context.Context) { c.n.Add(1) }
func (c *tickCounter) count() int64         { return c.n.Load() }

// waitFor крутит cond() до timeout-а; на провал — t.Fatal.
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

func constFn(d time.Duration) func() time.Duration { return func() time.Duration { return d } }

func baseConfig(rc *redis.Client) Config {
	return Config{
		LeaseKey:   "test:leader",
		Holder:     "keeper-test-01",
		Redis:      rc,
		Logger:     silentLogger(),
		IntervalFn: constFn(30 * time.Millisecond),
		LockTTLFn:  constFn(300 * time.Millisecond),
		Tick:       func(context.Context) {},
	}
}

func TestNew_ValidatesConfig(t *testing.T) {
	rc, _ := newTestRedis(t)
	full := baseConfig(rc)
	if _, err := New(full); err != nil {
		t.Fatalf("New(full): %v", err)
	}

	cases := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"empty_lease_key", func(c *Config) { c.LeaseKey = "" }},
		{"empty_holder", func(c *Config) { c.Holder = "" }},
		{"nil_redis", func(c *Config) { c.Redis = nil }},
		{"nil_logger", func(c *Config) { c.Logger = nil }},
		{"nil_interval_fn", func(c *Config) { c.IntervalFn = nil }},
		{"nil_lock_ttl_fn", func(c *Config) { c.LockTTLFn = nil }},
		{"nil_tick", func(c *Config) { c.Tick = nil }},
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

// TestRun_AcquiresLeaseAndTicks — acquire lease → tick вызывается по interval,
// lease реально захвачен в Redis под Holder-ом.
func TestRun_AcquiresLeaseAndTicks(t *testing.T) {
	rc, mr := newTestRedis(t)
	var tc tickCounter
	cfg := baseConfig(rc)
	cfg.Tick = tc.tick

	loop, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	// Ключ захвачен под нашим holder-ом.
	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(cfg.LeaseKey)
		return v == cfg.Holder
	})
	// Immediate-tick при acquire + последующие тики по interval.
	waitFor(t, 500*time.Millisecond, func() bool { return tc.count() >= 2 })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}
}

// TestRun_LeaseLost_StopsTickAndReacquires — потеря lease прекращает tick,
// после освобождения ключа loop пере-захватывает и tick возобновляется.
func TestRun_LeaseLost_StopsTickAndReacquires(t *testing.T) {
	rc, mr := newTestRedis(t)
	var tc tickCounter
	cfg := baseConfig(rc)
	cfg.Tick = tc.tick
	cfg.AcquireBackoff = 30 * time.Millisecond

	loop, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(cfg.LeaseKey)
		return v == cfg.Holder
	})
	callsBefore := tc.count()

	// «Воруем» lease: подменяем значение → следующий Renew вернёт ErrLeaseLost,
	// tick-loop остановится.
	mr.Set(cfg.LeaseKey, "intruder")
	// renewEvery = lock_ttl/3 = 100ms; ждём, чтобы Renew поймал потерю.
	time.Sleep(250 * time.Millisecond)

	// Освобождаем ключ — loop должен пере-захватить.
	mr.Del(cfg.LeaseKey)
	waitFor(t, 2*time.Second, func() bool {
		v, _ := mr.Get(cfg.LeaseKey)
		return v == cfg.Holder
	})
	// Новые тики после re-acquire.
	waitFor(t, 500*time.Millisecond, func() bool { return tc.count() > callsBefore })

	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v", err)
	}
}

// TestRun_TwoInstances_OnlyHolderTicks — один lease на двоих: tick зовётся
// только у держателя, второй крутится в acquire-backoff и не тикает.
func TestRun_TwoInstances_OnlyHolderTicks(t *testing.T) {
	rc, mr := newTestRedis(t)

	var winnerTicks, loserTicks tickCounter

	winnerCfg := baseConfig(rc)
	winnerCfg.Holder = "keeper-winner"
	winnerCfg.Tick = winnerTicks.tick
	winnerCfg.AcquireBackoff = 30 * time.Millisecond

	loserCfg := baseConfig(rc)
	loserCfg.Holder = "keeper-loser"
	loserCfg.Tick = loserTicks.tick
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

	// Стартуем winner первым, даём захватить lease, затем loser.
	wDone := make(chan error, 1)
	go func() { wDone <- winner.Run(ctx) }()
	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(winnerCfg.LeaseKey)
		return v == "keeper-winner"
	})
	lDone := make(chan error, 1)
	go func() { lDone <- loser.Run(ctx) }()

	// Winner накапливает тики; loser должен оставаться на нуле.
	waitFor(t, 500*time.Millisecond, func() bool { return winnerTicks.count() >= 3 })
	if got := loserTicks.count(); got != 0 {
		t.Errorf("loser ticked %d times while not holding lease; want 0", got)
	}
	// Ключ всё ещё у winner-а.
	if v, _ := mr.Get(winnerCfg.LeaseKey); v != "keeper-winner" {
		t.Errorf("lease holder = %q; want keeper-winner", v)
	}

	cancel()
	<-wDone
	<-lDone
}

// TestRun_HotReloadInterval — IntervalFn возвращает новое значение → следующий
// тик планируется по новому интервалу. Проверяем учащение темпа тиков.
func TestRun_HotReloadInterval(t *testing.T) {
	rc, _ := newTestRedis(t)
	var tc tickCounter

	var mu sync.Mutex
	interval := 200 * time.Millisecond

	cfg := baseConfig(rc)
	cfg.Tick = tc.tick
	cfg.IntervalFn = func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return interval
	}

	loop, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	// На медленном интервале (200ms) за ~250ms ожидаем мало тиков
	// (immediate + ~1).
	time.Sleep(250 * time.Millisecond)
	slow := tc.count()

	// Ускоряем интервал.
	mu.Lock()
	interval = 20 * time.Millisecond
	mu.Unlock()

	// Через следующий (старый) тик loop перечитает IntervalFn и пересоздаст
	// ticker — после этого тики пойдут часто. За 400ms на 20ms-интервале их
	// должно прибавиться заметно больше, чем на медленном.
	waitFor(t, 1500*time.Millisecond, func() bool {
		return tc.count()-slow >= 5
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned: %v", err)
	}
}

// TestRun_GracefulShutdown_ReleasesLease — ctx.Done → tick прекращается и
// lease освобождается (ключ удалён из Redis).
func TestRun_GracefulShutdown_ReleasesLease(t *testing.T) {
	rc, mr := newTestRedis(t)
	var tc tickCounter
	cfg := baseConfig(rc)
	cfg.Tick = tc.tick

	loop, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(cfg.LeaseKey)
		return v == cfg.Holder
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel — leak")
	}

	// После graceful-shutdown ключ освобождён (lease Release на ctx-выходе).
	if v, _ := mr.Get(cfg.LeaseKey); v != "" {
		t.Errorf("lease key still held after graceful shutdown: %q", v)
	}
}

// TestRun_LeaseFailover_OtherInstanceAcquires — держатель «умирает» (его ctx
// отменён, lease освобождён) → другой инстанс захватывает lease и начинает тикать.
func TestRun_LeaseFailover_OtherInstanceAcquires(t *testing.T) {
	rc, mr := newTestRedis(t)

	var aTicks, bTicks tickCounter

	aCfg := baseConfig(rc)
	aCfg.Holder = "keeper-a"
	aCfg.Tick = aTicks.tick
	aCfg.AcquireBackoff = 30 * time.Millisecond

	bCfg := baseConfig(rc)
	bCfg.Holder = "keeper-b"
	bCfg.Tick = bTicks.tick
	bCfg.AcquireBackoff = 30 * time.Millisecond

	a, err := New(aCfg)
	if err != nil {
		t.Fatalf("New a: %v", err)
	}
	b, err := New(bCfg)
	if err != nil {
		t.Fatalf("New b: %v", err)
	}

	aCtx, aCancel := context.WithCancel(context.Background())
	bCtx, bCancel := context.WithCancel(context.Background())
	defer bCancel()

	aDone := make(chan error, 1)
	go func() { aDone <- a.Run(aCtx) }()
	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(aCfg.LeaseKey)
		return v == "keeper-a"
	})

	// b стартует, упирается в backoff (a держит lease).
	bDone := make(chan error, 1)
	go func() { bDone <- b.Run(bCtx) }()
	time.Sleep(100 * time.Millisecond)
	if got := bTicks.count(); got != 0 {
		t.Errorf("b ticked %d times while a is leader; want 0", got)
	}

	// «Убиваем» a: graceful-shutdown освобождает lease.
	aCancel()
	<-aDone

	// b должен подхватить lease и начать тикать.
	waitFor(t, 2*time.Second, func() bool {
		v, _ := mr.Get(bCfg.LeaseKey)
		return v == "keeper-b"
	})
	waitFor(t, 500*time.Millisecond, func() bool { return bTicks.count() >= 1 })

	bCancel()
	<-bDone
}

// TestRun_OnLeaseChange_Callback — опциональный callback вызывается true на
// захвате lease и false на выходе (graceful shutdown).
func TestRun_OnLeaseChange_Callback(t *testing.T) {
	rc, mr := newTestRedis(t)

	var mu sync.Mutex
	var changes []bool
	cfg := baseConfig(rc)
	cfg.OnLeaseChange = func(held bool) {
		mu.Lock()
		changes = append(changes, held)
		mu.Unlock()
	}

	loop, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool {
		v, _ := mr.Get(cfg.LeaseKey)
		return v == cfg.Holder
	})
	waitFor(t, 500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(changes) >= 1 && changes[0]
	})

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(changes) < 2 {
		t.Fatalf("OnLeaseChange called %d times; want >= 2 (held then released)", len(changes))
	}
	if !changes[0] {
		t.Errorf("first OnLeaseChange = %v; want true (acquired)", changes[0])
	}
	if changes[len(changes)-1] {
		t.Errorf("last OnLeaseChange = %v; want false (released)", changes[len(changes)-1])
	}
}

// TestRun_OnLeaseChange_NilIsSafe — nil callback не должен паниковать.
func TestRun_OnLeaseChange_NilIsSafe(t *testing.T) {
	rc, _ := newTestRedis(t)
	cfg := baseConfig(rc)
	cfg.OnLeaseChange = nil

	loop, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := loop.Run(ctx); err != nil {
		t.Errorf("Run with nil OnLeaseChange: %v", err)
	}
}

// TestRun_AcquireConflict_DoesNotOverwrite — внешний holder держит lease;
// loop крутится в backoff и не перезаписывает чужой ключ, tick не зовётся.
func TestRun_AcquireConflict_DoesNotOverwrite(t *testing.T) {
	rc, mr := newTestRedis(t)
	mr.Set("test:leader", "external-leader")
	mr.SetTTL("test:leader", 10*time.Second)

	var tc tickCounter
	cfg := baseConfig(rc)
	cfg.Tick = tc.tick
	cfg.AcquireBackoff = 30 * time.Millisecond

	loop, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := loop.Run(ctx); err != nil {
		t.Errorf("Run: %v", err)
	}

	if got := tc.count(); got != 0 {
		t.Errorf("tick called %d times while lease held externally; want 0", got)
	}
	if v, _ := mr.Get("test:leader"); v != "external-leader" {
		t.Errorf("external lease was overwritten: got %q", v)
	}
}
