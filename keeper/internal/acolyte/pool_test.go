package acolyte

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCloser — io.Closer-заглушка, фиксирующая факт Close (для проверки
// graceful-стопа Summons-подписки в Shutdown).
type fakeCloser struct{ closed atomic.Bool }

func (f *fakeCloser) Close() error {
	f.closed.Store(true)
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewPool_Validation(t *testing.T) {
	if _, err := NewPool(Config{Workers: 0}, Deps{Logger: testLogger()}); err == nil {
		t.Fatal("Workers=0 must be rejected")
	}
	if _, err := NewPool(Config{Workers: -1}, Deps{Logger: testLogger()}); err == nil {
		t.Fatal("negative Workers must be rejected")
	}
	if _, err := NewPool(Config{Workers: 1}, Deps{Logger: nil}); err == nil {
		t.Fatal("nil Logger must be rejected")
	}
	if _, err := NewPool(Config{Workers: 2}, Deps{Logger: testLogger()}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestNewPool_DefaultPollInterval(t *testing.T) {
	p, err := NewPool(Config{Workers: 1}, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.PollInterval != defaultPollInterval {
		t.Fatalf("expected default poll interval %v, got %v", defaultPollInterval, p.cfg.PollInterval)
	}
}

// TestPool_StartsNWorkers — пул поднимает ровно N воркеров (по приросту
// goroutine-count относительно baseline).
func TestPool_StartsNWorkers(t *testing.T) {
	const n = 4
	p, err := NewPool(Config{Workers: n, PollInterval: time.Hour}, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	before := runtime.NumGoroutine()
	p.Start(ctx)

	// Дать воркерам встать на select.
	waitFor(t, func() bool { return runtime.NumGoroutine() >= before+n })

	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// После Shutdown прирост goroutine от пула должен схлопнуться.
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before })
}

// TestPool_ClaimCalledOnTick — claim-callback вызывается на poll-tick-е.
func TestPool_ClaimCalledOnTick(t *testing.T) {
	p, err := NewPool(Config{Workers: 1, PollInterval: 5 * time.Millisecond}, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	p.SetClaim(func(context.Context) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	waitFor(t, func() bool { return calls.Load() >= 3 })

	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestPool_ClaimCalledOnNotify — Summons-wake будит воркер раньше poll-tick-а.
func TestPool_ClaimCalledOnNotify(t *testing.T) {
	// Длинный poll-interval: если claim вызвался, это из-за Notify, не из-за тика.
	p, err := NewPool(Config{Workers: 1, PollInterval: time.Hour}, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	p.SetClaim(func(context.Context) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	p.Notify()
	waitFor(t, func() bool { return calls.Load() >= 1 })

	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestPool_SummonsWakesPool — Summons-сигнал от подписчика дёргает Notify,
// что будит воркер и приводит к claim раньше poll-tick-а.
func TestPool_SummonsWakesPool(t *testing.T) {
	// Длинный poll-interval: если claim вызвался — это от Summons-wake, не тика.
	var (
		mu     sync.Mutex
		onSig  func()
		closer = &fakeCloser{}
	)
	subscribe := func(_ context.Context, signal func()) (io.Closer, error) {
		mu.Lock()
		onSig = signal
		mu.Unlock()
		return closer, nil
	}

	p, err := NewPool(
		Config{Workers: 1, PollInterval: time.Hour},
		Deps{Logger: testLogger(), Summons: subscribe},
	)
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	p.SetClaim(func(context.Context) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	// Эмулируем Redis-сигнал: подписчик дёргает переданный ему callback.
	mu.Lock()
	sig := onSig
	mu.Unlock()
	if sig == nil {
		t.Fatal("Summons subscriber was not invoked on Start")
	}
	sig()

	waitFor(t, func() bool { return calls.Load() >= 1 })

	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if !closer.closed.Load() {
		t.Error("Summons subscription was not closed on Shutdown")
	}
}

// TestPool_SummonsSubscribeFailure — сбой подписки не роняет старт пула:
// пул работает на poll-fallback-е.
func TestPool_SummonsSubscribeFailure(t *testing.T) {
	subscribe := func(context.Context, func()) (io.Closer, error) {
		return nil, errors.New("redis down")
	}

	p, err := NewPool(
		Config{Workers: 1, PollInterval: 5 * time.Millisecond},
		Deps{Logger: testLogger(), Summons: subscribe},
	)
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	p.SetClaim(func(context.Context) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	// Несмотря на провал подписки, poll-fallback крутит claim.
	waitFor(t, func() bool { return calls.Load() >= 2 })

	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestPool_NoSummonsSubscriber — без Deps.Summons пул стартует и крутит claim
// на poll-fallback-е; Shutdown без подписки чистый.
func TestPool_NoSummonsSubscriber(t *testing.T) {
	p, err := NewPool(Config{Workers: 1, PollInterval: 5 * time.Millisecond}, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	p.SetClaim(func(context.Context) error {
		calls.Add(1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	waitFor(t, func() bool { return calls.Load() >= 1 })
	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestPool_DefaultClaimNoop — без SetClaim пул крутится без паники (no-op claim).
func TestPool_DefaultClaimNoop(t *testing.T) {
	p, err := NewPool(Config{Workers: 2, PollInterval: time.Millisecond}, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestPool_ClaimErrorDoesNotStop — ошибка claim-callback-а не роняет воркер.
func TestPool_ClaimErrorDoesNotStop(t *testing.T) {
	p, err := NewPool(Config{Workers: 1, PollInterval: 5 * time.Millisecond}, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	p.SetClaim(func(context.Context) error {
		calls.Add(1)
		return errors.New("boom")
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	// Несколько тиков несмотря на постоянную ошибку — воркер жив.
	waitFor(t, func() bool { return calls.Load() >= 3 })

	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestPool_ShutdownGraceful — Shutdown завершается без leak-warn на чистом
// ctx-cancel (воркеры выходят быстро).
func TestPool_ShutdownGraceful(t *testing.T) {
	p, err := NewPool(Config{Workers: 3, PollInterval: 10 * time.Millisecond}, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	cancel()

	done := make(chan error, 1)
	go func() { done <- p.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return within 2s — workers leaked")
	}
}

// TestPool_DrainStopsNewClaims — после drain-сигнала (Shutdown) пул перестаёт
// входить в новые claim-тики. Считаем claim-и: фиксируем число на момент
// Shutdown, ждём и убеждаемся, что новых не прибавилось.
func TestPool_DrainStopsNewClaims(t *testing.T) {
	p, err := NewPool(
		Config{Workers: 2, PollInterval: 5 * time.Millisecond, DrainGrace: time.Second},
		Deps{Logger: testLogger()},
	)
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	p.SetClaim(func(context.Context) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// Дать пулу натикать claim-ов.
	waitFor(t, func() bool { return calls.Load() >= 3 })

	// Drain через Shutdown: воркеры должны выйти из loop-а, новых тиков нет.
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	after := calls.Load()

	// После drain — никаких новых claim-ов, сколько бы ни прошло poll-интервалов.
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != after {
		t.Fatalf("claims continued after drain: %d → %d", after, got)
	}
}

// TestPool_DrainWaitsInflightWithinGrace — уже идущий in-flight claim
// доводится до конца в пределах grace (его ctx НЕ отменяется), Shutdown
// возвращает nil.
func TestPool_DrainWaitsInflightWithinGrace(t *testing.T) {
	p, err := NewPool(
		Config{Workers: 1, PollInterval: time.Millisecond, DrainGrace: 2 * time.Second},
		Deps{Logger: testLogger()},
	)
	if err != nil {
		t.Fatal(err)
	}

	var (
		entered   = make(chan struct{}, 1)
		finished  atomic.Bool
		ctxCancel atomic.Bool
	)
	p.SetClaim(func(cctx context.Context) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		// In-flight держим ~100ms — короче grace. Сигнализируем, если ctx
		// внезапно отменили (drain не должен этого делать в пределах grace).
		select {
		case <-time.After(100 * time.Millisecond):
			finished.Store(true)
		case <-cctx.Done():
			ctxCancel.Store(true)
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	<-entered // дождались, что воркер вошёл в claim (in-flight)

	start := time.Now()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}
	if ctxCancel.Load() {
		t.Fatal("in-flight claim ctx was cancelled within grace — drain rubbed it out")
	}
	if !finished.Load() {
		t.Fatal("in-flight claim did not finish naturally")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("shutdown took too long (%v) — grace not honoured as expected", elapsed)
	}
}

// TestPool_DrainGraceExceededAbortsInflight — in-flight, не уложившийся в grace,
// прерывается отменой claim-ctx. Имитация «claim, который не завершит работу
// сам»: callback возвращается ТОЛЬКО по ctx.Done. Проверяем, что claim получил
// отменённый ctx (= не «доеден» силой: callback сам решает, что делать с Ward —
// здесь он просто выходит, ничего не коммитя).
func TestPool_DrainGraceExceededAbortsInflight(t *testing.T) {
	p, err := NewPool(
		Config{Workers: 1, PollInterval: time.Millisecond, DrainGrace: 50 * time.Millisecond},
		Deps{Logger: testLogger()},
	)
	if err != nil {
		t.Fatal(err)
	}

	var (
		entered   = make(chan struct{}, 1)
		sawCancel atomic.Bool
	)
	p.SetClaim(func(cctx context.Context) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		// Бесконечный in-flight: завершаемся ТОЛЬКО по отмене ctx (grace-abort).
		// Если бы пул не отменил ctx, callback не вышел бы и Shutdown завис бы —
		// сам факт возврата Shutdown доказывает, что claimCtx отменён.
		<-cctx.Done()
		sawCancel.Store(true)
		return cctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	<-entered

	start := time.Now()
	// Shutdown-ctx с запасом: даём grace истечь и claimCancel сработать.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = p.Shutdown(shutCtx)

	if !sawCancel.Load() {
		t.Fatal("in-flight claim was not aborted by ctx after grace exceeded")
	}
	// Shutdown не вернулся раньше grace-окна.
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("shutdown returned before grace window elapsed (%v)", elapsed)
	}
}

// waitFor поллит cond до true или фейлит по таймауту.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
