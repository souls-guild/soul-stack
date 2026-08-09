package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
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

// TestClassifyDispatchErr -- the refusal → reason-token projection, wrapped the way the
// dispatcher returns it (the bridge never sees a bare sentinel).
//
// Scope honestly: this asserts the function, and nothing downstream observes its
// output yet — voyageCommandSpawner drops it and `voyage_targets` has no column for it
// (see the classifyDispatchErr doc). So a green run here does NOT mean an operator can
// tell these cases apart; it means the mapping is right for whoever wires it up.
//
// The three dry_run rows became reachable at all only when NIM-559 put the flag on the
// per-host request; each names a different fix, so collapsing any of them into the
// `spawn_error` default would point at the wrong thing. The verb-shell row is included
// even though Voyage creation now answers 400: a run parked on schedule_at, or created
// before that check existed, arrives here without passing it.
func TestClassifyDispatchErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"soul not connected", fmt.Errorf("dispatch host-a: %w", errand.ErrSoulNotConnected), "soul_not_connected"},
		{"verb-shell under dry_run", fmt.Errorf("dispatch host-a: %w",
			&errand.DryRunVerbShellError{Module: "core.cmd.shell"}), coremanifest.ReasonDryRunUnsupported},
		{"capability not announced", fmt.Errorf("dispatch host-a: %w", errand.ErrDryRunNotAnnounced), "soul_capability_unsupported"},
		{"capability unverifiable", fmt.Errorf("dispatch host-a: %w", errand.ErrDryRunUnverifiable), "soul_capability_unverifiable"},
		{"empty module", fmt.Errorf("dispatch host-a: %w", errand.ErrModuleEmpty), "invalid_request"},
		{"anything else", errors.New("pg is down"), "spawn_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyDispatchErr(tc.err); got != tc.want {
				t.Errorf("classifyDispatchErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// recordingDispatcher captures the DispatchRequest the bridge builds and answers a
// synchronous success. Recording the REQUEST rather than returning a canned "ok" is
// the point: a fake that only answers cannot show which fields the caller dropped,
// and dry_run was dropped here for the whole life of Voyage kind=command (NIM-559).
type recordingDispatcher struct {
	mu   sync.Mutex
	reqs []errand.DispatchRequest
}

func (d *recordingDispatcher) Dispatch(_ context.Context, req errand.DispatchRequest) (errand.DispatchResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reqs = append(d.reqs, req)
	return errand.DispatchResult{ErrandID: "E-" + req.SID, Status: errand.StatusSuccess}, nil
}

func (d *recordingDispatcher) Cancel(_ context.Context, _ errand.CancelRequest) error { return nil }

func (d *recordingDispatcher) last(t *testing.T) errand.DispatchRequest {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.reqs) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(d.reqs))
	}
	return d.reqs[0]
}

// TestVoyageCommandSpawner_ForwardsDryRun guards the production hop
// Voyage executor → voyageCommandSpawner → errandRunSpawnerBridge → DispatchRequest.
// Both values are asserted: the flag must be FORWARDED, not hardcoded either way. A
// composite literal that omits the field compiles to false, which is why the defect
// needed no wrong line to exist — only a missing one.
func TestVoyageCommandSpawner_ForwardsDryRun(t *testing.T) {
	t.Parallel()
	for _, dryRun := range []bool{true, false} {
		dryRun := dryRun
		t.Run(map[bool]string{true: "dry_run", false: "apply"}[dryRun], func(t *testing.T) {
			t.Parallel()
			disp := &recordingDispatcher{}
			sp := &voyageCommandSpawner{bridge: &errandRunSpawnerBridge{
				dispatcher:   disp,
				pollInterval: time.Millisecond,
				clock:        time.Now,
			}}

			errandID, status, err := sp.SpawnCommand(context.Background(),
				"V1", "s1.example", "core.file.present", "archon-alice", []byte(`{"path":"/etc/motd"}`), dryRun)
			if err != nil {
				t.Fatalf("SpawnCommand: %v", err)
			}
			if status != "success" || errandID != "E-s1.example" {
				t.Fatalf("got (%q,%q), want (E-s1.example,success)", errandID, status)
			}

			req := disp.last(t)
			if req.DryRun != dryRun {
				t.Errorf("DispatchRequest.DryRun = %v, want %v — the Soul picks Plan over Apply from this field alone", req.DryRun, dryRun)
			}
			// The rest of the request must still arrive intact: a "fix" that rebuilds
			// the request from scratch would satisfy the assertion above alone.
			if req.SID != "s1.example" || req.Module != "core.file.present" || req.StartedByAID != "archon-alice" {
				t.Errorf("request = %+v, want sid/module/aid preserved", req)
			}
			if got, ok := req.Input["path"].(string); !ok || got != "/etc/motd" {
				t.Errorf("request input = %v, want path=/etc/motd", req.Input)
			}
		})
	}
}
