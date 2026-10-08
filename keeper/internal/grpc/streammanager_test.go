package grpc

import (
	"context"
	"sync"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func TestStreamManager_Register_LookupSendUnregister(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("host.example.com")

	if e := m.lookup("host.example.com"); e == nil {
		t.Fatal("lookup after Register returned nil")
	}

	msg := &keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_HelloReply{HelloReply: &keeperv1.HelloReply{Kid: "k1"}}}
	if ok := m.lookup("host.example.com").send(msg); !ok {
		t.Fatal("send returned false on empty buffer")
	}

	got, ok := <-out
	if !ok {
		t.Fatal("channel closed before receive")
	}
	if got.GetHelloReply().GetKid() != "k1" {
		t.Errorf("kid = %q, want k1", got.GetHelloReply().GetKid())
	}

	m.Unregister("host.example.com", out)
	if e := m.lookup("host.example.com"); e != nil {
		t.Fatal("entry still present after Unregister")
	}
	if _, ok := <-out; ok {
		t.Fatal("channel still open after Unregister")
	}
}

func TestStreamManager_RegisterReplacesExisting(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out1 := m.Register("sid")
	out2 := m.Register("sid")

	if out1 == out2 {
		t.Fatal("second Register returned same channel")
	}
	// out1 is closed by eviction.
	if _, ok := <-out1; ok {
		t.Fatal("first channel not closed after eviction")
	}
	// out2 is active.
	m.lookup("sid").send(&keeperv1.FromKeeper{})
	if _, ok := <-out2; !ok {
		t.Fatal("second channel closed unexpectedly")
	}
}

func TestStreamManager_Unregister_WrongOwnerSkipped(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out1 := m.Register("sid")
	out2 := m.Register("sid") // evicts out1

	// Unregister with a stale owner handle shouldn't touch the new entry.
	m.Unregister("sid", out1)
	if e := m.lookup("sid"); e == nil {
		t.Fatal("active entry removed by stale Unregister")
	}
	// Cleanup
	m.Unregister("sid", out2)
}

func TestStreamManager_Send_QueueFullReturnsFalse(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	_ = m.Register("sid")
	entry := m.lookup("sid")

	for i := 0; i < outboundBufferSize; i++ {
		if !entry.send(&keeperv1.FromKeeper{}) {
			t.Fatalf("send #%d failed early", i)
		}
	}
	if entry.send(&keeperv1.FromKeeper{}) {
		t.Fatal("send succeeded on full buffer")
	}
}

func TestStreamManager_Send_AfterCloseReturnsFalse(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("sid")
	entry := m.lookup("sid")
	m.Unregister("sid", out)

	if entry.send(&keeperv1.FromKeeper{}) {
		t.Fatal("send succeeded after close")
	}
}

// TestStreamManager_CloseAll_CancelsStreamCtx — Watchman shedding (S2):
// CloseAll must actually cancel the per-stream ctx of every registered
// stream.
func TestStreamManager_CloseAll_CancelsStreamCtx(t *testing.T) {
	m := NewStreamManager(discardLogger(t))

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelA()
	defer cancelB()
	m.RegisterStream("sid-a", cancelA)
	m.RegisterStream("sid-b", cancelB)

	if n := m.CloseAll(); n != 2 {
		t.Fatalf("CloseAll() = %d, want 2", n)
	}

	// Both per-stream ctx values must be cancelled.
	select {
	case <-ctxA.Done():
	default:
		t.Fatal("sid-a stream ctx not cancelled by CloseAll")
	}
	select {
	case <-ctxB.Done():
	default:
		t.Fatal("sid-b stream ctx not cancelled by CloseAll")
	}
}

// TestStreamManager_CloseAll_SkipsNilCancel — CloseAll skips streams
// registered without a cancel (via Register / tests) and doesn't panic.
func TestStreamManager_CloseAll_SkipsNilCancel(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	_ = m.Register("no-cancel") // cancel == nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.RegisterStream("with-cancel", cancel)

	if n := m.CloseAll(); n != 1 {
		t.Fatalf("CloseAll() = %d, want 1 (only the cancelable stream)", n)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancelable stream ctx not cancelled")
	}
}

// TestStreamManager_CloseAll_Idempotent — a repeated CloseAll is safe
// (context.CancelFunc is idempotent) as long as the handler hasn't done
// Unregister yet.
func TestStreamManager_CloseAll_Idempotent(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.RegisterStream("sid", cancel)

	if n := m.CloseAll(); n != 1 {
		t.Fatalf("first CloseAll = %d, want 1", n)
	}
	if n := m.CloseAll(); n != 1 {
		t.Fatalf("second CloseAll = %d, want 1 (entry still present until Unregister)", n)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("stream ctx not cancelled")
	}
}

// The guards below cover [StreamManager.Close] — the only thing that can reach
// the goroutine serving a forgotten host (NIM-386). Erasing the registry row
// stops the host RECONNECTING, but a stream it already holds authenticated at
// open and survives the delete; if Close reports "closed" without cancelling,
// the operator is told a live host was released.

// TestStreamManager_Close_CancelsOnlyTheNamedStream — the release itself, and
// the blast radius. A forget names one host; cancelling a neighbour's ctx would
// drop a Soul that was never mentioned.
func TestStreamManager_Close_CancelsOnlyTheNamedStream(t *testing.T) {
	m := NewStreamManager(discardLogger(t))

	victimCtx, cancelVictim := context.WithCancel(context.Background())
	bystanderCtx, cancelBystander := context.WithCancel(context.Background())
	defer cancelVictim()
	defer cancelBystander()
	m.RegisterStream("victim.example.com", cancelVictim)
	m.RegisterStream("bystander.example.com", cancelBystander)

	if !m.Close("victim.example.com") {
		t.Fatal("Close reported no stream for a SID that has one")
	}
	select {
	case <-victimCtx.Done():
	default:
		t.Fatal("Close returned true but the stream ctx was never cancelled — " +
			"the forgotten host keeps talking to a Keeper that has no record of it, " +
			"and the operator was told the stream was released")
	}
	select {
	case <-bystanderCtx.Done():
		t.Error("forgetting one host cancelled another host's stream")
	default:
	}
}

// TestStreamManager_Close_UnknownSIDIsNotReportedAsClosed — the false half.
// "No stream here" is the normal answer on every instance but the one holding
// it, and a forget of a host that was never connected is legal (NIM-386 has no
// state gate). Returning true would put a release in the operator's record that
// never happened.
func TestStreamManager_Close_UnknownSIDIsNotReportedAsClosed(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	if m.Close("never-connected.example.com") {
		t.Error("Close reported closing a stream that does not exist")
	}
}

// TestStreamManager_Close_StreamWithoutCancelIsNotReportedAsClosed — a stream
// registered through the older [StreamManager.Register] path has no cancel, so
// nothing can force it down. Reporting "closed" for a stream still running is
// the one answer an operator must never be given.
func TestStreamManager_Close_StreamWithoutCancelIsNotReportedAsClosed(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	_ = m.Register("no-cancel.example.com")

	if m.Close("no-cancel.example.com") {
		t.Error("Close reported closing a stream it cannot cancel")
	}
}

// TestStreamManager_Close_IsIdempotent — the forget path can reach the same SID
// twice: the instance closes its own stream synchronously AND the notice it
// broadcast can come back through another route. A second call must be a no-op,
// not a panic.
func TestStreamManager_Close_IsIdempotent(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.RegisterStream("sid.example.com", cancel)

	if !m.Close("sid.example.com") {
		t.Fatal("first Close = false")
	}
	if !m.Close("sid.example.com") {
		t.Error("second Close = false; the entry is still present until the handler's own Unregister")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("stream ctx not cancelled")
	}
}

func applyMsg(applyID string, passage int32) *keeperv1.FromKeeper {
	return &keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_ApplyRequest{
		ApplyRequest: &keeperv1.ApplyRequest{ApplyId: applyID, Passage: passage},
	}}
}

// ★ GUARD (NIM-949). An ApplyRequest another instance published finds the queue
// full and waits for room instead of being dropped: its sender took it as
// delivered, and nothing would tell the step it never reached the Soul. While it
// waits it is declared, so the stream's WardRoster sweep, landing meanwhile, spares
// its row.
//
// Mutations: forward tries once, as deliverLocal does → false → red; record the
// request only once delivered → the sweep does not see it → red.
func TestStreamManager_ForwardWaitsForRoom(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("host.example.com")
	entry := m.lookup("host.example.com")
	for range outboundBufferSize {
		if !entry.send(&keeperv1.FromKeeper{}) {
			t.Fatal("send failed while filling the queue")
		}
	}
	swept := make(chan []routedApply, 1)
	go func() {
		time.Sleep(10 * forwardRetryEvery)
		swept <- m.takeRouted("host.example.com")
		<-out
	}()
	if !m.forward("host.example.com", applyMsg("a1", 1), entry, nil) {
		t.Fatal("forward = false, want the request queued once room appears")
	}
	if got := <-swept; len(got) != 1 || got[0] != (routedApply{applyID: "a1", passage: 1}) {
		t.Errorf("declared while waiting = %v, want [a1/1]", got)
	}
}

// ★ GUARD (NIM-949). The stream's teardown ends the wait: the subscriber is joined
// before the stream is unregistered, so a wait it cannot end would hold the lease
// and the host's presence for forwardApplyWait per buffered request.
//
// Mutation: forward ignores stop → it returns only at forwardApplyWait → red.
func TestStreamManager_ForwardStopsWithTheStream(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	m.Register("host.example.com")
	entry := m.lookup("host.example.com")
	for range outboundBufferSize {
		entry.send(&keeperv1.FromKeeper{})
	}
	stop := make(chan struct{})
	go func() {
		time.Sleep(2 * forwardRetryEvery)
		close(stop)
	}()
	start := time.Now()
	if m.forward("host.example.com", applyMsg("a1", 1), entry, stop) {
		t.Fatal("forward = true on a full queue")
	}
	if waited := time.Since(start); waited > forwardApplyWait/2 {
		t.Errorf("forward returned after %s, want soon after stop", waited)
	}
}

// ★ GUARD (NIM-949). A request the queue refused is not declared for the stream:
// the Soul never gets it, and declared it would keep the WardRoster sweep off a
// row nothing will answer.
//
// Mutation: drop forgetRouted → the refused request is declared → red.
func TestStreamManager_RefusedRequestIsNotDeclared(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	m.Register("host.example.com")
	entry := m.lookup("host.example.com")
	for range outboundBufferSize {
		entry.send(&keeperv1.FromKeeper{})
	}
	if m.deliverLocal("host.example.com", applyMsg("a1", 1), entry) {
		t.Fatal("deliverLocal = true on a full queue")
	}
	if got := m.takeRouted("host.example.com"); len(got) != 0 {
		t.Errorf("routed = %v, want none", got)
	}
}

// ★ GUARD (NIM-949). A send racing the entry's close never panics: forward retries
// a send every 50ms for up to 10s, while a reconnect to this instance evicts — and
// closes — the entry. The window is a few instructions wide, so a plain run rarely
// hits it; under -race (make test-race) the unsynchronised send and close are
// reported every run.
//
// Mutation: release closeMu before the channel send → DATA RACE under -race → red.
func TestStreamEntry_SendRacingCloseDoesNotPanic(t *testing.T) {
	for range 3000 {
		e := &streamEntry{outCh: make(chan *keeperv1.FromKeeper, 1)}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if e.send(&keeperv1.FromKeeper{}) {
					select {
					case <-e.outCh:
					default:
					}
				}
			}
		}()
		e.close()
		wg.Wait()
	}
}

// ★ GUARD (NIM-949). The console stream's entry keeps the same contract: a frame
// sent while Unregister or CloseAllFor closes the entry never panics. Under -race
// the unsynchronised send and close are reported every run.
//
// Mutation: release closeMu before the channel send → DATA RACE under -race → red.
func TestConsoleStreamEntry_SendRacingCloseDoesNotPanic(t *testing.T) {
	for range 3000 {
		e := &consoleStreamEntry{outCh: make(chan *keeperv1.ConsoleToSoul, 1)}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if e.send(&keeperv1.ConsoleToSoul{}) {
					select {
					case <-e.outCh:
					default:
					}
				}
			}
		}()
		e.close()
		wg.Wait()
	}
}
