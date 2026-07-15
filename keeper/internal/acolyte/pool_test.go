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

// fakeCloser is an io.Closer stub that records whether Close was called (to
// verify graceful stop of the Summons subscription in Shutdown).
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

// TestPool_StartsNWorkers checks that the pool starts exactly N workers (by
// the goroutine-count increase relative to baseline).
func TestPool_StartsNWorkers(t *testing.T) {
	const n = 4
	p, err := NewPool(Config{Workers: n, PollInterval: time.Hour}, Deps{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	before := runtime.NumGoroutine()
	p.Start(ctx)

	// Let the workers settle into select.
	waitFor(t, func() bool { return runtime.NumGoroutine() >= before+n })

	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// After Shutdown, the pool's goroutine increase should collapse.
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before })
}

// TestPool_ClaimCalledOnTick checks that the claim-callback is invoked on a
// poll-tick.
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

// TestPool_ClaimCalledOnNotify checks that Summons-wake wakes the worker
// ahead of the poll-tick.
func TestPool_ClaimCalledOnNotify(t *testing.T) {
	// Long poll-interval: if claim fired, it's because of Notify, not the tick.
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

// TestPool_SummonsWakesPool checks that a Summons signal from the subscriber
// triggers Notify, waking the worker and causing a claim ahead of the
// poll-tick.
func TestPool_SummonsWakesPool(t *testing.T) {
	// Long poll-interval: if claim fired, it's from Summons-wake, not the tick.
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

	// Emulate a Redis signal: the subscriber invokes the callback it was given.
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

// TestPool_SummonsSubscribeFailure checks that a subscription failure doesn't
// break pool startup: the pool runs on poll-fallback.
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

	// Despite the subscription failing, poll-fallback keeps claiming.
	waitFor(t, func() bool { return calls.Load() >= 2 })

	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestPool_NoSummonsSubscriber checks that without Deps.Summons the pool
// starts and claims on poll-fallback; Shutdown without a subscription is
// clean.
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

// TestPool_DefaultClaimNoop checks that without SetClaim the pool runs
// without panicking (no-op claim).
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

// TestPool_ClaimErrorDoesNotStop checks that a claim-callback error doesn't
// bring down the worker.
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

	// Several ticks despite the persistent error — the worker is alive.
	waitFor(t, func() bool { return calls.Load() >= 3 })

	cancel()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestPool_ShutdownGraceful checks that Shutdown completes without a
// leak-warning on a clean ctx-cancel (workers exit fast).
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

// TestPool_DrainStopsNewClaims checks that after the drain signal (Shutdown)
// the pool stops entering new claim ticks. Counts claims: records the count
// at Shutdown time, waits, and verifies no new ones were added.
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

	// Let the pool tick out a few claims.
	waitFor(t, func() bool { return calls.Load() >= 3 })

	// Drain via Shutdown: workers should exit the loop, no new ticks.
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	after := calls.Load()

	// After drain — no new claims, no matter how many poll intervals pass.
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != after {
		t.Fatalf("claims continued after drain: %d → %d", after, got)
	}
}

// TestPool_DrainWaitsInflightWithinGrace checks that an already in-flight
// claim runs to completion within grace (its ctx is NOT cancelled), and
// Shutdown returns nil.
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
		// Hold in-flight for ~100ms — shorter than grace. Signal if ctx got
		// cancelled unexpectedly (drain shouldn't do that within grace).
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

	<-entered // wait until the worker entered claim (in-flight)

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

// TestPool_DrainGraceExceededAbortsInflight checks that an in-flight claim
// that doesn't finish within grace is aborted by cancelling claim-ctx.
// Simulates a "claim that won't finish on its own": the callback returns
// ONLY on ctx.Done. Verifies that claim received a cancelled ctx (it isn't
// forcibly killed — the callback decides what to do with the Ward itself;
// here it just returns, without committing anything).
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
		// Infinite in-flight: only returns on ctx cancel (grace-abort). If the
		// pool didn't cancel ctx, the callback would never return and Shutdown
		// would hang — Shutdown returning at all proves claimCtx was cancelled.
		<-cctx.Done()
		sawCancel.Store(true)
		return cctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	<-entered

	start := time.Now()
	// Shutdown-ctx with headroom: let grace expire and claimCancel fire.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = p.Shutdown(shutCtx)

	if !sawCancel.Load() {
		t.Fatal("in-flight claim was not aborted by ctx after grace exceeded")
	}
	// Shutdown didn't return before the grace window elapsed.
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("shutdown returned before grace window elapsed (%v)", elapsed)
	}
}

// waitFor polls cond until true or fails on timeout.
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
