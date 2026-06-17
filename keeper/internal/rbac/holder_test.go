package rbac

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSource — управляемый SnapshotSource для тестов Holder без Postgres-а.
// Снимок и ошибка меняются между Refresh-ами под Mutex-ом (Run читает в
// фоне).
type fakeSource struct {
	mu   sync.Mutex
	snap *Snapshot
	err  error

	loads int
}

func (f *fakeSource) Load(_ context.Context) (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

func (f *fakeSource) set(snap *Snapshot, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = snap
	f.err = err
}

// snapshotWith — хелпер: снимок с одной ролью cluster-admin (perm `*`) и
// заданным списком привязанных AID.
func adminSnapshot(aids ...string) *Snapshot {
	s := &Snapshot{
		Roles:      map[string][]string{"cluster-admin": {"*"}},
		Membership: map[string][]string{},
	}
	for _, aid := range aids {
		s.Membership[aid] = []string{"cluster-admin"}
	}
	return s
}

func TestHolder_InitialEnforcer_FromSnapshot(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	if err := h.Check("archon-alice", "operator", "create", nil); err != nil {
		t.Errorf("alice (cluster-admin) should pass operator.create: %v", err)
	}
	if err := h.Check("archon-bob", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("bob should be denied: %v", err)
	}
	if got := h.ClusterAdmins(); len(got) != 1 || got[0] != "archon-alice" {
		t.Errorf("ClusterAdmins = %v", got)
	}
}

func TestHolder_InitialLoadError_Fatal(t *testing.T) {
	src := &fakeSource{err: errors.New("db down")}
	if _, err := NewHolder(context.Background(), src, time.Hour, nil); err == nil {
		t.Fatal("NewHolder should fail when initial Load errors")
	}
}

func TestHolder_Refresh_PicksUpNewMembership(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// До перечита Bob не админ.
	if err := h.Check("archon-bob", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("bob should be denied before refresh: %v", err)
	}

	// Меняем снимок в БД: Bob тоже cluster-admin.
	src.set(adminSnapshot("archon-alice", "archon-bob"), nil)
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if err := h.Check("archon-bob", "operator", "create", nil); err != nil {
		t.Errorf("bob should be allowed after refresh: %v", err)
	}
	if got := h.ClusterAdmins(); len(got) != 2 {
		t.Errorf("ClusterAdmins after refresh = %v, want 2 entries", got)
	}
}

func TestHolder_Refresh_ErrorKeepsPrevEnforcer(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// Перечит падает — активный enforcer должен остаться прежним.
	src.set(nil, errors.New("db blip"))
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh should return error on Load failure")
	}
	if err := h.Check("archon-alice", "operator", "create", nil); err != nil {
		t.Errorf("alice should still pass after failed refresh: %v", err)
	}
}

func TestHolder_Refresh_BadPermissionKeepsPrevEnforcer(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// Снимок с невалидной permission (вне каталога) → NewEnforcerFromSnapshot
	// падает; активный enforcer не меняется.
	src.set(&Snapshot{
		Roles:      map[string][]string{"bad": {"unknown.create"}},
		Membership: map[string][]string{"archon-bob": {"bad"}},
	}, nil)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh should fail on unknown permission")
	}
	if err := h.Check("archon-alice", "operator", "create", nil); err != nil {
		t.Errorf("alice should still pass after bad-snapshot refresh: %v", err)
	}
}

func TestHolder_Run_TTLPolls(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	// Меняем снимок; фоновый ticker должен подхватить за несколько интервалов.
	src.set(adminSnapshot("archon-alice", "archon-bob"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := h.Check("archon-bob", "operator", "create", nil); err == nil {
			break // подхвачено
		}
		if time.Now().After(deadline) {
			t.Fatal("TTL-poll did not pick up new membership within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHolder_NilSource_DefaultDeny(t *testing.T) {
	h, err := NewHolder(context.Background(), nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder(nil src): %v", err)
	}
	if err := h.Check("archon-anyone", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("nil-source Holder should deny: %v", err)
	}
	// Run при nil-src — no-op, не виснет.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.Run(ctx)
}

// fakeInvalidationSource — управляемый [InvalidationSource] для тестов
// WatchInvalidations без Redis-а. invalidate() имитирует приход чужого
// invalidate-сигнала; Watch блокируется до ctx.Done() или fatalErr.
type fakeInvalidationSource struct {
	signals chan struct{}
	err     error // если задан — Watch возвращает её сразу (имитация фатальной подписки)
}

func newFakeInvalidationSource() *fakeInvalidationSource {
	return &fakeInvalidationSource{signals: make(chan struct{}, 16)}
}

func (f *fakeInvalidationSource) invalidate() { f.signals <- struct{}{} }

func (f *fakeInvalidationSource) Watch(ctx context.Context, onInvalidate func()) error {
	if f.err != nil {
		return f.err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-f.signals:
			onInvalidate()
		}
	}
}

// TestHolder_WatchInvalidations_NearInstantRefresh — приход invalidate-сигнала
// рефрешит снимок из БД БЕЗ ожидания TTL-poll-а (interval=час, тик не сработает
// в окне теста).
func TestHolder_WatchInvalidations_NearInstantRefresh(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := newFakeInvalidationSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.WatchInvalidations(ctx, invSrc)

	// До инвалидации Bob не админ.
	if err := h.Check("archon-bob", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("bob should be denied before invalidate: %v", err)
	}

	// Меняем снимок в БД и шлём invalidate — Holder должен near-instant
	// перечитать (не дожидаясь часового TTL-тика).
	src.set(adminSnapshot("archon-alice", "archon-bob"), nil)
	invSrc.invalidate()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := h.Check("archon-bob", "operator", "create", nil); err == nil {
			break // подхвачено через pub/sub-invalidate
		}
		if time.Now().After(deadline) {
			t.Fatal("invalidate did not trigger near-instant refresh within deadline")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHolder_WatchInvalidations_TTLFallbackStillWorks — даже когда invalidate
// НЕ приходит (потеря сообщения / нет pub/sub), TTL-poll [Run] всё равно
// рефрешит снимок. B2 = B1 + pub/sub, fallback не сломан.
func TestHolder_WatchInvalidations_TTLFallbackStillWorks(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := newFakeInvalidationSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)                        // TTL-poll
	go h.WatchInvalidations(ctx, invSrc) // pub/sub — но сигнал НЕ шлём

	// Меняем снимок, invalidate НЕ шлём — подхватить должен только TTL-poll.
	src.set(adminSnapshot("archon-alice", "archon-bob"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := h.Check("archon-bob", "operator", "create", nil); err == nil {
			break // подхвачено TTL-poll-ом (fallback)
		}
		if time.Now().After(deadline) {
			t.Fatal("TTL-poll fallback did not pick up change without invalidate")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHolder_WatchInvalidations_FatalSubscribeFailSoft — фатальная ошибка
// подписки НЕ роняет: WatchInvalidations возвращается (логирует warn), daemon
// продолжает на TTL-poll.
func TestHolder_WatchInvalidations_FatalSubscribeFailSoft(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := &fakeInvalidationSource{err: errors.New("redis subscribe down")}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.WatchInvalidations(context.Background(), invSrc)
	}()
	select {
	case <-done:
		// OK — вернулся без паники/виса.
	case <-time.After(2 * time.Second):
		t.Fatal("WatchInvalidations did not return on fatal subscribe error")
	}
}

// TestHolder_WatchInvalidations_NilSourceNoop — при nil-БД-источнике или
// nil-инвалидаторе Watch — no-op (нечего рефрешить / некуда подписываться).
func TestHolder_WatchInvalidations_NilSourceNoop(t *testing.T) {
	// nil-БД-источник: Holder в default-deny, Watch сразу выходит.
	h, err := NewHolder(context.Background(), nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder(nil src): %v", err)
	}
	h.WatchInvalidations(context.Background(), newFakeInvalidationSource()) // не виснет

	// nil-инвалидатор при живом БД-источнике: тоже no-op.
	h2, err := NewHolder(context.Background(), &fakeSource{snap: adminSnapshot("archon-alice")}, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h2.WatchInvalidations(context.Background(), nil) // не виснет
}
