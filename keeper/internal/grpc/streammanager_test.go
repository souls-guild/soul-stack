package grpc

import (
	"context"
	"testing"

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
	// out1 закрыт eviction-ом.
	if _, ok := <-out1; ok {
		t.Fatal("first channel not closed after eviction")
	}
	// out2 — активный.
	m.lookup("sid").send(&keeperv1.FromKeeper{})
	if _, ok := <-out2; !ok {
		t.Fatal("second channel closed unexpectedly")
	}
}

func TestStreamManager_Unregister_WrongOwnerSkipped(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out1 := m.Register("sid")
	out2 := m.Register("sid") // вытесняет out1

	// Unregister с устаревшим owner-handle не должен трогать новую запись.
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

// TestStreamManager_CloseAll_CancelsStreamCtx — Watchman-shedding (S2): CloseAll
// должен реально отменить per-stream ctx каждого зарегистрированного стрима.
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

	// Оба per-stream ctx должны быть отменены.
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

// TestStreamManager_CloseAll_SkipsNilCancel — стримы, зарегистрированные без
// cancel-а (через Register / тесты), CloseAll пропускает и не паникует.
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

// TestStreamManager_CloseAll_Idempotent — повторный CloseAll безопасен
// (context.CancelFunc идемпотентна), пока handler ещё не сделал Unregister.
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
