package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/runtime"
)

// holdModule applies once release is closed, or fails when its task is cancelled.
type holdModule struct {
	module.BaseModule
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (m *holdModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.once.Do(func() { close(m.started) })
	select {
	case <-m.release:
		return stream.Send(&pluginv1.ApplyEvent{Changed: true})
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

type holdRegistry struct{ mod *holdModule }

func (r holdRegistry) Lookup(name string) (module.SoulModule, bool) {
	return r.mod, name == "test.hold"
}

// keeperStream is the Keeper end of one stream.
type keeperStream struct {
	mu      sync.Mutex
	results []*keeperv1.RunResult
	events  int
}

func (s *keeperStream) SendTaskEvent(*keeperv1.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events++
	return nil
}

func (s *keeperStream) SendRunResult(rr *keeperv1.RunResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, rr)
	return nil
}

func (s *keeperStream) waitResult(t *testing.T) *keeperv1.RunResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.results) > 0 {
			rr := s.results[0]
			s.mu.Unlock()
			return rr
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no RunResult within 5s")
	return nil
}

func holdRequest(applyID string) *keeperv1.ApplyRequest {
	return &keeperv1.ApplyRequest{
		ApplyId: applyID,
		Tasks:   []*keeperv1.RenderedTask{{Name: "hold", Module: "test.hold.run"}},
	}
}

func newHoldWorker(t *testing.T) (*applyWorker, *holdModule) {
	t.Helper()
	mod := &holdModule{started: make(chan struct{}), release: make(chan struct{})}
	w := newApplyWorker(runtime.NewApplyRunner(holdRegistry{mod}, nil), slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.run(ctx)
	return w, mod
}

// ★ GUARD (NIM-949, acceptance 3, Soul side). An apply runs on past the stream it
// arrived on: the stream drops mid-task, the task finishes, and its result reaches
// the Keeper on the next stream. Meanwhile the apply stays declared, so the
// WardRoster of the new stream keeps the Keeper's row.
//
// Mutation: the relay drops what it is given with no stream attached → the next
// stream gets no result → red.
func TestApplyWorker_RunOutlivesItsStream(t *testing.T) {
	w, mod := newHoldWorker(t)
	first := &keeperStream{}
	w.relay.Attach(first)
	w.submit(holdRequest("a1"))
	<-mod.started

	w.relay.Detach(first)
	close(mod.release)
	deadline := time.Now().Add(5 * time.Second)
	for w.inFlight() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.ContainsFunc(w.runner.ActiveSet(), func(a *keeperv1.ActiveApply) bool { return a.GetApplyId() == "a1" }) {
		t.Errorf("ActiveSet = %v, want a1 declared while its result is undelivered", w.runner.ActiveSet())
	}

	next := &keeperStream{}
	w.relay.Attach(next)
	if rr := next.waitResult(t); rr.GetApplyId() != "a1" || rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("result on the next stream = %v, want a1 SUCCESS", rr)
	}
}

// ★ GUARD (NIM-949). The WardRoster declares what the runner itself does not
// know: an apply received and not started, and one whose result waits for a
// stream — or the Keeper's sweep orphans a step this Soul still answers for.
//
// Mutations: drop the SetAlsoOwned wiring → all missing; drop w.queued() or
// w.relay.Undelivered() from it → that one missing; leave the request taken off
// the queue out of queued() → popped missing → red.
func TestApplyWorker_DeclaresQueuedAndUndelivered(t *testing.T) {
	mod := &holdModule{started: make(chan struct{}), release: make(chan struct{})}
	w := newApplyWorker(runtime.NewApplyRunner(holdRegistry{mod}, nil), slog.New(slog.DiscardHandler))
	w.submit(holdRequest("queued"))
	_ = w.relay.SendRunResult(&keeperv1.RunResult{ApplyId: "undelivered"})
	w.current = holdRequest("popped")

	declared := map[string]bool{}
	for _, a := range w.runner.ActiveSet() {
		declared[a.GetApplyId()] = true
	}
	for _, id := range []string{"queued", "undelivered", "popped"} {
		if !declared[id] {
			t.Errorf("ActiveSet = %v, want %s declared", w.runner.ActiveSet(), id)
		}
	}
}

// ★ GUARD (NIM-949). The session loop no longer waits on the apply, so a
// CancelApply reaches a running apply, and a queued one is reported cancelled
// without starting.
//
// Mutation: cancel ignores the queue → the queued a2 runs → its result is not
// CANCELLED → red.
func TestApplyWorker_CancelReachesRunningAndQueued(t *testing.T) {
	w, mod := newHoldWorker(t)
	s := &keeperStream{}
	w.relay.Attach(s)
	w.submit(holdRequest("a1"))
	<-mod.started
	w.submit(holdRequest("a2"))

	if !w.cancel("a2") {
		t.Fatal("cancel(a2) = false, want the queued apply dropped")
	}
	if !w.cancel("a1") {
		t.Fatal("cancel(a1) = false, want the running apply cancelled")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.results)
		s.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	got := map[string]keeperv1.RunStatus{}
	for _, rr := range s.results {
		got[rr.GetApplyId()] = rr.GetStatus()
	}
	for _, id := range []string{"a1", "a2"} {
		if got[id] != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
			t.Errorf("%s result = %v, want CANCELLED (all: %v)", id, got[id], got)
		}
	}
}

// ★ GUARD (NIM-949). Only the Keeper refusing a message the Soul sent counts
// against the relay's events, with the refused size; a stream that ends any other
// way — the Keeper's own message over its send limit included, which ends it with
// the same code — does not.
//
// Mutations: the code alone is a refusal → the send-limit case counts → red; any
// error is a refusal → the transport error counts → red.
func TestKeeperRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		size int
		want bool
	}{
		{"oversize", status.Error(codes.ResourceExhausted, "grpc: received message larger than max (1048612 vs. 1048576)"), 1048612, true},
		{"keeper send limit", status.Error(codes.ResourceExhausted, "grpc: trying to send message larger than max (9000000 vs. 8388608)"), 0, false},
		{"transport", status.Error(codes.Unavailable, "transport is closing"), 0, false},
		{"internal", status.Error(codes.Internal, "recv: stream reset"), 0, false},
		{"eof", io.EOF, 0, false},
	} {
		if size, ok := keeperRefused(tc.err); ok != tc.want || size != tc.size {
			t.Errorf("%s: keeperRefused(%v) = %d, %v, want %d, %v", tc.name, tc.err, size, ok, tc.size, tc.want)
		}
	}
}

// ★ GUARD (NIM-949). A Run that refuses its request still answers the step: the
// row would otherwise stay dispatched, its stream up, until scenario_timeout.
//
// Mutation: only log the Run error → no RunResult → red.
func TestApplyWorker_RefusedRunReportsFailed(t *testing.T) {
	w, _ := newHoldWorker(t)
	s := &keeperStream{}
	w.relay.Attach(s)
	w.submit(nil)
	if rr := s.waitResult(t); rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("result = %v, want FAILED", rr)
	}
}

// ★ GUARD (NIM-949). Retiring a closed stream's reader takes what the reader still
// hands over — it blocks until taken, so a wait that does not take never ends —
// and a run's request among it is honoured: its Keeper sent it before the stream
// closed and holds the step's row dispatched for it.
//
// Mutations: wait on done without taking → no return within 2s → red; drop the
// handleRun call → the request is lost → red.
func TestRetireReader_HonoursARunsLastMessage(t *testing.T) {
	recvCh := make(chan recvResult)
	done := make(chan struct{})
	go func() {
		recvCh <- recvResult{msg: &keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_ApplyRequest{
			ApplyRequest: &keeperv1.ApplyRequest{ApplyId: "last"},
		}}}
		recvCh <- recvResult{err: io.EOF}
		close(done)
	}()
	var handled []string
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		retireReader(recvCh, done, func(msg *keeperv1.FromKeeper) bool {
			handled = append(handled, msg.GetApplyRequest().GetApplyId())
			return true
		})
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("retireReader did not return — it waits without taking what the reader hands over")
	}
	if !slices.Equal(handled, []string{"last"}) {
		t.Errorf("handled %v, want [last]", handled)
	}
}
