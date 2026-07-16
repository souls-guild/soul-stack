package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/errand"
)

// fakeTerminalSource -- a programmable errandTerminalSource. Returns a
// predefined sequence of statuses by errand_id: each Get advances the
// cursor, simulating the running → terminal transition written by the
// Dispatcher's background goroutine.
type fakeTerminalSource struct {
	mu    sync.Mutex
	rows  map[string][]errand.Status // errand_id → status sequence
	calls map[string]int
}

func newFakeTerminalSource() *fakeTerminalSource {
	return &fakeTerminalSource{rows: map[string][]errand.Status{}, calls: map[string]int{}}
}

func (f *fakeTerminalSource) set(id string, seq ...errand.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[id] = seq
}

func (f *fakeTerminalSource) Get(_ context.Context, id string) (*errand.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.rows[id]
	idx := f.calls[id]
	f.calls[id]++
	if len(seq) == 0 {
		return nil, errand.ErrNotFound
	}
	if idx >= len(seq) {
		idx = len(seq) - 1
	}
	return &errand.Row{ErrandID: id, Status: seq[idx]}, nil
}

// TestAwaitTerminal_SuccessAfterPoll -- after the async-escalation the
// bridge polls the row and returns the real success (NOT async_escalation).
func TestAwaitTerminal_SuccessAfterPoll(t *testing.T) {
	t.Parallel()
	src := newFakeTerminalSource()
	src.set("E1", errand.StatusRunning, errand.StatusRunning, errand.StatusSuccess)
	b := &errandRunSpawnerBridge{
		terminalSource: src,
		pollInterval:   time.Millisecond,
		clock:          time.Now,
	}
	status, errorCode, err := b.awaitTerminal(context.Background(), "E1")
	if err != nil {
		t.Fatalf("awaitTerminal err: %v", err)
	}
	if status != "success" {
		t.Fatalf("status = %q, want success (NOT async_escalation)", status)
	}
	if errorCode != "" {
		t.Fatalf("errorCode = %q, want empty", errorCode)
	}
}

// TestAwaitTerminal_FailedTerminal -- a failed terminal returns as
// failed/errand_failed (the real result, not a hack).
func TestAwaitTerminal_FailedTerminal(t *testing.T) {
	t.Parallel()
	src := newFakeTerminalSource()
	src.set("E2", errand.StatusFailed)
	b := &errandRunSpawnerBridge{terminalSource: src, pollInterval: time.Millisecond, clock: time.Now}
	status, errorCode, err := b.awaitTerminal(context.Background(), "E2")
	if err != nil {
		t.Fatalf("awaitTerminal err: %v", err)
	}
	if status != "failed" || errorCode != "errand_failed" {
		t.Fatalf("got (%q,%q), want (failed,errand_failed)", status, errorCode)
	}
}

// TestAwaitTerminal_DeadlineExceeded -- a row stuck running forever ->
// deadline -> failed/await_timeout (the orchestrator never hangs forever).
func TestAwaitTerminal_DeadlineExceeded(t *testing.T) {
	t.Parallel()
	src := newFakeTerminalSource()
	src.set("E3", errand.StatusRunning)
	// Compressed clock: each now() call jumps a minute forward so the
	// deadline (DefaultTimeoutSeconds+grace) expires within a couple ticks
	// without a real wait.
	var n int64
	clk := func() time.Time {
		n++
		return time.Unix(0, 0).Add(time.Duration(n) * time.Minute)
	}
	b := &errandRunSpawnerBridge{terminalSource: src, pollInterval: time.Millisecond, clock: clk}
	status, errorCode, err := b.awaitTerminal(context.Background(), "E3")
	if err != nil {
		t.Fatalf("awaitTerminal err: %v", err)
	}
	if status != "failed" || errorCode != "await_timeout" {
		t.Fatalf("got (%q,%q), want (failed,await_timeout)", status, errorCode)
	}
}

// TestAwaitTerminal_CtxCancel -- ctx cancellation (abort-policy / shutdown)
// -> cancelled (not counted as a failure in Summary).
func TestAwaitTerminal_CtxCancel(t *testing.T) {
	t.Parallel()
	src := newFakeTerminalSource()
	src.set("E4", errand.StatusRunning)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &errandRunSpawnerBridge{terminalSource: src, pollInterval: 10 * time.Millisecond, clock: time.Now}
	status, errorCode, err := b.awaitTerminal(ctx, "E4")
	if err != nil {
		t.Fatalf("awaitTerminal err: %v", err)
	}
	if status != "cancelled" || errorCode != "cancelled" {
		t.Fatalf("got (%q,%q), want (cancelled,cancelled)", status, errorCode)
	}
}

// TestClassifyErrandStatus -- table of the errand.Status → (status,error_code) projection.
func TestClassifyErrandStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      errand.Status
		status  string
		errCode string
	}{
		{errand.StatusSuccess, "success", ""},
		{errand.StatusFailed, "failed", "errand_failed"},
		{errand.StatusTimedOut, "timed_out", "timed_out"},
		{errand.StatusCancelled, "cancelled", "cancelled"},
		{errand.StatusModuleNotAllowed, "module_not_allowed", "module_not_allowed"},
		{errand.StatusRunning, "", ""},
	}
	for _, tc := range cases {
		st, ec := classifyErrandStatus(tc.in)
		if st != tc.status || ec != tc.errCode {
			t.Errorf("classifyErrandStatus(%q) = (%q,%q), want (%q,%q)", tc.in, st, ec, tc.status, tc.errCode)
		}
	}
}
