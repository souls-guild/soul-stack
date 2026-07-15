package clouddriver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClassify_CtxAndNil(t *testing.T) {
	if got := Classify(nil, nil); got != FailUnknown {
		t.Errorf("nil err class=%v, want unknown", got)
	}
	if got := Classify(nil, context.Canceled); got != FailTransient {
		t.Errorf("ctx.Canceled class=%v, want transient", got)
	}
	if got := Classify(nil, context.DeadlineExceeded); got != FailTransient {
		t.Errorf("ctx.DeadlineExceeded class=%v, want transient", got)
	}
	// fn=nil + arbitrary error → unknown (driver didn't set a classifier).
	if got := Classify(nil, errors.New("x")); got != FailUnknown {
		t.Errorf("nil fn class=%v, want unknown", got)
	}
}

func TestClassify_DelegatesToFn(t *testing.T) {
	fn := func(err error) FailClass {
		if err.Error() == "Throttling" {
			return FailTransient
		}
		return FailAuth
	}
	if got := Classify(fn, errors.New("Throttling")); got != FailTransient {
		t.Errorf("class=%v, want transient", got)
	}
	if got := Classify(fn, errors.New("AccessDenied")); got != FailAuth {
		t.Errorf("class=%v, want auth", got)
	}
}

func TestFailClass_TransientAndString(t *testing.T) {
	cases := map[FailClass]struct {
		str   string
		trans bool
	}{
		FailUnknown:       {"unknown", false},
		FailNotFound:      {"not_found", false},
		FailQuota:         {"quota_exceeded", false},
		FailAuth:          {"auth", false},
		FailInvalidParams: {"invalid_params", false},
		FailTransient:     {"transient", true},
	}
	for c, want := range cases {
		if c.String() != want.str {
			t.Errorf("%d.String()=%q, want %q", c, c.String(), want.str)
		}
		if c.Transient() != want.trans {
			t.Errorf("%d.Transient()=%v, want %v", c, c.Transient(), want.trans)
		}
	}
}

func TestFailMessage(t *testing.T) {
	msg := FailMessage(FailQuota, "RunInstances", errors.New("limit reached"))
	want := "quota_exceeded: RunInstances: limit reached"
	if msg != want {
		t.Errorf("msg=%q, want %q", msg, want)
	}
}

func TestRetry_SucceedsAfterTransient(t *testing.T) {
	classify := func(err error) FailClass {
		if err.Error() == "throttle" {
			return FailTransient
		}
		return FailAuth
	}
	calls := 0
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 5}
	err := Retry(context.Background(), cfg, classify, func() error {
		calls++
		if calls < 3 {
			return errors.New("throttle")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls=%d, want 3", calls)
	}
}

func TestRetry_NonTransientReturnsImmediately(t *testing.T) {
	classify := func(error) FailClass { return FailAuth }
	calls := 0
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 5}
	wantErr := errors.New("denied")
	err := Retry(context.Background(), cfg, classify, func() error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Errorf("calls=%d, want 1 (non-transient must not retry)", calls)
	}
}

func TestRetry_ExhaustsAttempts(t *testing.T) {
	classify := func(error) FailClass { return FailTransient }
	calls := 0
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 3}
	err := Retry(context.Background(), cfg, classify, func() error {
		calls++
		return errors.New("throttle")
	})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if calls != 3 {
		t.Errorf("calls=%d, want 3 (MaxAttempts)", calls)
	}
}

func TestRetry_CtxCancelDuringBackoff(t *testing.T) {
	classify := func(error) FailClass { return FailTransient }
	ctx, cancel := context.WithCancel(context.Background())
	cfg := BackoffConfig{Initial: 50 * time.Millisecond, Max: time.Second, Factor: 2, MaxAttempts: 10}
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	err := Retry(ctx, cfg, classify, func() error { return errors.New("throttle") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

func TestWaitUntilReady_AllBecomeReady(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 20}
	polls := map[string]int{}
	probe := func(_ context.Context, vmID string) ProbeResult {
		polls[vmID]++
		return ProbeResult{Ready: polls[vmID] >= 2}
	}
	res, err := WaitUntilReady(context.Background(), cfg, []string{"i-1", "i-2"}, probe, nil)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	for _, r := range res {
		if !r.Ready || r.Err != nil {
			t.Errorf("vm %s: ready=%v err=%v, want ready", r.VMID, r.Ready, r.Err)
		}
	}
}

func TestWaitUntilReady_TerminalErr(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 20}
	probeErr := errors.New("instance went to error state")
	probe := func(_ context.Context, vmID string) ProbeResult {
		if vmID == "i-bad" {
			return ProbeResult{Err: probeErr}
		}
		return ProbeResult{Ready: true}
	}
	res, err := WaitUntilReady(context.Background(), cfg, []string{"i-ok", "i-bad"}, probe, nil)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	byID := map[string]WaitResult{}
	for _, r := range res {
		byID[r.VMID] = r
	}
	if !byID["i-ok"].Ready {
		t.Error("i-ok should be ready")
	}
	if byID["i-bad"].Err == nil || byID["i-bad"].Ready {
		t.Errorf("i-bad should carry terminal err, got %+v", byID["i-bad"])
	}
}

// TestWaitUntilReady_CtxCancel_AntiOrphan covers the key reference technique:
// on ctx-cancel, per-VM results are returned for VMs already polled (with
// VMID filled in), so the driver can mark the ones that didn't make it as
// failed → Keeper can Destroy them.
func TestWaitUntilReady_CtxCancel_AntiOrphan(t *testing.T) {
	cfg := BackoffConfig{Initial: 50 * time.Millisecond, Max: time.Second, Factor: 2, MaxAttempts: 50}
	ctx, cancel := context.WithCancel(context.Background())
	probe := func(_ context.Context, _ string) ProbeResult {
		return ProbeResult{Ready: false} // never ready
	}
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	res, err := WaitUntilReady(ctx, cfg, []string{"i-1", "i-2"}, probe, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if len(res) != 2 {
		t.Fatalf("res len=%d, want 2 (anti-orphan: all vm_ids returned)", len(res))
	}
	for _, r := range res {
		if r.VMID == "" {
			t.Error("anti-orphan: WaitResult must carry vm_id even when cancelled")
		}
		if r.Ready {
			t.Errorf("vm %s reported ready unexpectedly", r.VMID)
		}
	}
}

func TestWaitUntilReady_MaxAttemptsDeadline(t *testing.T) {
	cfg := BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 2, MaxAttempts: 3}
	probe := func(_ context.Context, _ string) ProbeResult { return ProbeResult{Ready: false} }
	res, err := WaitUntilReady(context.Background(), cfg, []string{"i-1"}, probe, nil)
	if !errors.Is(err, ErrWaitDeadline) {
		t.Fatalf("err=%v, want ErrWaitDeadline", err)
	}
	if len(res) != 1 || res[0].Ready {
		t.Errorf("res=%+v, want one not-ready entry", res)
	}
}
