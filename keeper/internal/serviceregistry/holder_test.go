package serviceregistry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSnapSource — управляемый SnapshotSource для тестов Holder без Postgres-а.
// Снимок и ошибка меняются между Refresh-ами под Mutex-ом (Run читает в фоне).
type fakeSnapSource struct {
	mu   sync.Mutex
	snap *Snapshot
	err  error

	loads int
}

func (f *fakeSnapSource) Load(_ context.Context) (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

func (f *fakeSnapSource) set(snap *Snapshot, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = snap
	f.err = err
}

// snapWith — хелпер: снимок с заданными Service-ами (по name) и скаляром
// default_destiny_source.
func snapWith(dds string, names ...string) *Snapshot {
	s := &Snapshot{
		services:             map[string]ServiceEntry{},
		defaultDestinySource: dds,
	}
	for _, n := range names {
		s.services[n] = ServiceEntry{Name: n, Git: "git@x:" + n + ".git", Ref: "main"}
	}
	return s
}

func TestHolder_InitialSnapshot_FromSource(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("git@x:destiny.git", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	if e, ok := h.Resolve("web"); !ok || e.Name != "web" {
		t.Errorf("Resolve(web) = %+v ok=%v, want web/true", e, ok)
	}
	if _, ok := h.Resolve("missing"); ok {
		t.Errorf("Resolve(missing) ok=true, want false")
	}
	if got := h.DefaultDestinySource(); got != "git@x:destiny.git" {
		t.Errorf("DefaultDestinySource = %q, want git@x:destiny.git", got)
	}
}

func TestHolder_InitialLoadError_Fatal(t *testing.T) {
	src := &fakeSnapSource{err: errors.New("db down")}
	if _, err := NewHolder(context.Background(), src, time.Hour, nil); err == nil {
		t.Fatal("NewHolder should fail when initial Load errors")
	}
}

func TestHolder_Refresh_PicksUpChange(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// До перечита api-service отсутствует, скаляр пуст.
	if _, ok := h.Resolve("api"); ok {
		t.Fatalf("Resolve(api) ok=true before refresh, want false")
	}
	if got := h.DefaultDestinySource(); got != "" {
		t.Fatalf("DefaultDestinySource = %q before refresh, want empty", got)
	}

	// Меняем снимок в БД: добавился api + задан default_destiny_source.
	src.set(snapWith("git@x:destiny.git", "web", "api"), nil)
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if _, ok := h.Resolve("api"); !ok {
		t.Errorf("Resolve(api) ok=false after refresh, want true")
	}
	if got := h.DefaultDestinySource(); got != "git@x:destiny.git" {
		t.Errorf("DefaultDestinySource = %q after refresh, want git@x:destiny.git", got)
	}
}

func TestHolder_Refresh_ErrorKeepsPrevSnapshot(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("git@x:destiny.git", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	// Перечит падает — активный снимок должен остаться прежним.
	src.set(nil, errors.New("db blip"))
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh should return error on Load failure")
	}
	if _, ok := h.Resolve("web"); !ok {
		t.Errorf("Resolve(web) ok=false after failed refresh, want prev snapshot kept")
	}
	if got := h.DefaultDestinySource(); got != "git@x:destiny.git" {
		t.Errorf("DefaultDestinySource lost after failed refresh: %q", got)
	}
}

func TestHolder_Run_TTLPolls(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	// Меняем снимок; фоновый ticker должен подхватить за несколько интервалов.
	src.set(snapWith("", "web", "api"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := h.Resolve("api"); ok {
			break // подхвачено
		}
		if time.Now().After(deadline) {
			t.Fatal("TTL-poll did not pick up new service within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHolder_NilSource_EmptySnapshot(t *testing.T) {
	h, err := NewHolder(context.Background(), nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder(nil src): %v", err)
	}
	if _, ok := h.Resolve("anything"); ok {
		t.Errorf("nil-source Holder should resolve nothing")
	}
	if got := h.DefaultDestinySource(); got != "" {
		t.Errorf("nil-source DefaultDestinySource = %q, want empty", got)
	}
	// Run при nil-src — no-op, не виснет.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.Run(ctx)
}

// fakeInvSource — управляемый InvalidationSource для тестов WatchInvalidations
// без Redis-а. invalidate() имитирует приход чужого invalidate-сигнала; Watch
// блокируется до ctx.Done() или fatalErr.
type fakeInvSource struct {
	signals chan struct{}
	err     error // если задан — Watch возвращает её сразу (имитация фатальной подписки)
}

func newFakeInvSource() *fakeInvSource {
	return &fakeInvSource{signals: make(chan struct{}, 16)}
}

func (f *fakeInvSource) invalidate() { f.signals <- struct{}{} }

func (f *fakeInvSource) Watch(ctx context.Context, onInvalidate func()) error {
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
// рефрешит снимок БЕЗ ожидания TTL-poll-а (interval=час, тик не сработает в окне
// теста).
func TestHolder_WatchInvalidations_NearInstantRefresh(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := newFakeInvSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.WatchInvalidations(ctx, invSrc)

	if _, ok := h.Resolve("api"); ok {
		t.Fatalf("Resolve(api) ok=true before invalidate, want false")
	}

	// Меняем снимок в БД и шлём invalidate — Holder должен near-instant
	// перечитать (не дожидаясь часового TTL-тика).
	src.set(snapWith("", "web", "api"), nil)
	invSrc.invalidate()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := h.Resolve("api"); ok {
			break // подхвачено через pub/sub-invalidate
		}
		if time.Now().After(deadline) {
			t.Fatal("invalidate did not trigger near-instant refresh within deadline")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHolder_WatchInvalidations_TTLFallbackStillWorks — даже когда invalidate НЕ
// приходит, TTL-poll [Run] всё равно рефрешит снимок (fallback не сломан).
func TestHolder_WatchInvalidations_TTLFallbackStillWorks(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := newFakeInvSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)                        // TTL-poll
	go h.WatchInvalidations(ctx, invSrc) // pub/sub — но сигнал НЕ шлём

	src.set(snapWith("", "web", "api"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := h.Resolve("api"); ok {
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
	src := &fakeSnapSource{snap: snapWith("", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	invSrc := &fakeInvSource{err: errors.New("redis subscribe down")}

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
// nil-инвалидаторе Watch — no-op.
func TestHolder_WatchInvalidations_NilSourceNoop(t *testing.T) {
	h, err := NewHolder(context.Background(), nil, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder(nil src): %v", err)
	}
	h.WatchInvalidations(context.Background(), newFakeInvSource()) // не виснет

	h2, err := NewHolder(context.Background(), &fakeSnapSource{snap: snapWith("", "web")}, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h2.WatchInvalidations(context.Background(), nil) // не виснет
}

// TestHolder_AtomicSnapshot_Race — конкурентные синхронные геттеры (Resolve/
// DefaultDestinySource) против фонового Refresh-а: atomic-swap снимка не должен
// давать data race (-race). Каждый итог геттера сам по себе согласован
// (читает один опубликованный снимок).
func TestHolder_AtomicSnapshot_Race(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("git@x:a.git", "web")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	// Писатель: чередует два снимка через Refresh.
	wg.Add(1)
	go func() {
		defer wg.Done()
		snaps := []*Snapshot{
			snapWith("git@x:a.git", "web"),
			snapWith("git@x:b.git", "web", "api"),
		}
		for i := 0; i < 500; i++ {
			src.set(snaps[i%2], nil)
			_ = h.Refresh(ctx)
		}
	}()

	// Читатели: синхронные геттеры в несколько goroutine.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_, _ = h.Resolve("web")
				_, _ = h.Resolve("api")
				_ = h.DefaultDestinySource()
			}
		}()
	}

	wg.Wait()
}
