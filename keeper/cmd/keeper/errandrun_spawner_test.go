package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/errand"
)

// fakeTerminalSource — программируемый errandTerminalSource. Возвращает
// заранее заданную последовательность статусов по errand_id: каждый Get
// продвигает курсор, имитируя running → terminal-переход, который пишет
// background-горутина Dispatcher-а.
type fakeTerminalSource struct {
	mu    sync.Mutex
	rows  map[string][]errand.Status // errand_id → последовательность статусов
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

// TestAwaitTerminal_SuccessAfterPoll — после async-escalation bridge поллит
// строку и возвращает реальный success (НЕ async_escalation).
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
		t.Fatalf("status = %q, want success (НЕ async_escalation)", status)
	}
	if errorCode != "" {
		t.Fatalf("errorCode = %q, want empty", errorCode)
	}
}

// TestAwaitTerminal_FailedTerminal — терминал failed возвращается как failed/
// errand_failed (реальный результат, не хак).
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

// TestAwaitTerminal_DeadlineExceeded — строка вечно running → дедлайн →
// failed/await_timeout (orchestrator не виснет навсегда).
func TestAwaitTerminal_DeadlineExceeded(t *testing.T) {
	t.Parallel()
	src := newFakeTerminalSource()
	src.set("E3", errand.StatusRunning)
	// Сжатые часы: каждый вызов now() прыгает на минуту, чтобы дедлайн
	// (DefaultTimeoutSeconds+grace) истёк за пару тиков без реального ожидания.
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

// TestAwaitTerminal_CtxCancel — отмена ctx (abort-policy / shutdown) →
// cancelled (не считается провалом в Summary).
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

// TestClassifyErrandStatus — таблица проекции errand.Status → (status,error_code).
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
