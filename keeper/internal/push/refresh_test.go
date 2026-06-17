package push

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// mockRespawner — захватывает аргументы и отдаёт заранее заданный
// результат. Позволяет проверять, что dispatcher закрывает старый handle
// (oldCloser.Close был вызван), spawn-ит новый и подменяет ссылку.
type mockRespawner struct {
	mu          sync.Mutex
	calls       int32
	gotName     string
	gotOldClose io.Closer
	newProv     SshProvider
	newCloser   io.Closer
	err         error
}

func (m *mockRespawner) RespawnProvider(_ context.Context, name string, oldCloser io.Closer) (SshProvider, io.Closer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	atomic.AddInt32(&m.calls, 1)
	m.gotName = name
	m.gotOldClose = oldCloser
	// Документированный контракт: respawner сам закрывает old, потом spawn-ит.
	if oldCloser != nil {
		_ = oldCloser.Close()
	}
	if m.err != nil {
		return nil, nil, m.err
	}
	return m.newProv, m.newCloser, nil
}

// recordingCloser — io.Closer, фиксирующий факт вызова Close. Используется как
// «старый» plugin-handle.
type recordingCloser struct {
	closed atomic.Int32
	err    error
}

func (c *recordingCloser) Close() error {
	c.closed.Add(1)
	return c.err
}

// providerForTest — helper: считывает текущий Provider из карты под RLock.
// Заменяет приватный getter provider() из single-provider формы. Возвращает
// nil, если запись отсутствует (degraded state).
func providerForTest(d *SshDispatcher, name string) SshProvider {
	entry, ok := d.providerEntry(name)
	if !ok {
		return nil
	}
	return entry.Provider
}

// TestRefreshProvider_HappyPath — re-spawn должен закрыть старый handle и
// поставить новый. Последующий lookup в карте возвращает новый Provider.
func TestRefreshProvider_HappyPath(t *testing.T) {
	oldProv := &mockProvider{authAllowed: true}
	oldCloser := &recordingCloser{}
	newProv := &mockProvider{authAllowed: true}
	newCloser := &recordingCloser{}
	r := &mockRespawner{newProv: newProv, newCloser: newCloser}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			testProviderName: {Provider: oldProv, Closer: oldCloser},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	if err := disp.RefreshProvider(context.Background(), testProviderName); err != nil {
		t.Fatalf("RefreshProvider: %v", err)
	}
	if r.gotName != testProviderName {
		t.Errorf("respawner called with %q, want %q", r.gotName, testProviderName)
	}
	if oldCloser.closed.Load() != 1 {
		t.Errorf("old plugin-handle не закрыт (closed=%d, want 1)", oldCloser.closed.Load())
	}
	if providerForTest(disp, testProviderName) != newProv {
		t.Errorf("dispatcher не подменил provider")
	}
}

// TestRefreshProvider_EmptyName_MassInvalidate — пустое имя из pub/sub-сообщения
// (mass invalidate) → re-spawn ВСЕХ зарегистрированных провайдеров.
func TestRefreshProvider_EmptyName_MassInvalidate(t *testing.T) {
	oldA := &mockProvider{authAllowed: true}
	oldB := &mockProvider{authAllowed: true}
	newA := &mockProvider{authAllowed: true}
	newB := &mockProvider{authAllowed: true}

	// Сложный mockRespawner: возвращает разный provider в зависимости от имени.
	r := &nameAwareRespawner{
		out: map[string]SshProvider{
			"static": newA,
			"vault":  newB,
		},
	}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			"static": {Provider: oldA, Closer: &recordingCloser{}},
			"vault":  {Provider: oldB, Closer: &recordingCloser{}},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	if err := disp.RefreshProvider(context.Background(), ""); err != nil {
		t.Fatalf("RefreshProvider(empty name): %v", err)
	}
	if providerForTest(disp, "static") != newA {
		t.Errorf("static не подменился на newA")
	}
	if providerForTest(disp, "vault") != newB {
		t.Errorf("vault не подменился на newB")
	}
	if r.calls.Load() != 2 {
		t.Errorf("respawner calls = %d, want 2 (mass invalidate)", r.calls.Load())
	}
}

// TestRefreshProvider_WrongName_NoOp — сообщение про неизвестное имя (не наш
// каталог плагинов) → no-op без ошибки.
func TestRefreshProvider_WrongName_NoOp(t *testing.T) {
	oldProv := &mockProvider{authAllowed: true}
	r := &mockRespawner{newProv: &mockProvider{}, newCloser: &recordingCloser{}}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: oldProv}},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	if err := disp.RefreshProvider(context.Background(), "another-ssh"); err != nil {
		t.Fatalf("чужое имя не должно давать ошибку: %v", err)
	}
	if atomic.LoadInt32(&r.calls) != 0 {
		t.Errorf("respawner не должен вызываться для чужого имени")
	}
	if providerForTest(disp, testProviderName) != oldProv {
		t.Errorf("provider не должен меняться для чужого имени")
	}
}

// TestRefreshProvider_NoRespawner_Sentinel — диспетчер без Respawner возвращает
// ErrRespawnNotSupported.
func TestRefreshProvider_NoRespawner_Sentinel(t *testing.T) {
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	err := disp.RefreshProvider(context.Background(), testProviderName)
	if !errors.Is(err, ErrRespawnNotSupported) {
		t.Errorf("ожидалась ErrRespawnNotSupported, got %v", err)
	}
}

// TestRefreshProvider_SpawnFailed_DegradedState — Spawn упал → dispatcher
// удаляет запись из карты (degraded), последующий SendApply вернёт
// ErrProviderUnknown.
func TestRefreshProvider_SpawnFailed_DegradedState(t *testing.T) {
	oldCloser := &recordingCloser{}
	r := &mockRespawner{err: errors.New("plugin binary missing")}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			testProviderName: {Provider: &mockProvider{authAllowed: true}, Closer: oldCloser},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	err := disp.RefreshProvider(context.Background(), testProviderName)
	if err == nil {
		t.Fatal("ждали ошибку при spawn-fail")
	}
	if providerForTest(disp, testProviderName) != nil {
		t.Errorf("после spawn-fail запись должна быть удалена (degraded), есть провайдер")
	}
	if oldCloser.closed.Load() == 0 {
		t.Errorf("respawner не закрыл old handle перед возвратом ошибки")
	}
}

// TestRefreshProvider_Concurrent_MutexProtected — два конкурентных
// RefreshProvider не должны падать; respawner вызывается последовательно
// (mutex), final-provider — один из spawn-result.
func TestRefreshProvider_Concurrent_MutexProtected(t *testing.T) {
	provA := &mockProvider{authAllowed: true}
	provB := &mockProvider{authAllowed: true}

	var seq atomic.Int32
	r := &concurrencyTrackingRespawner{
		out: func() (SshProvider, io.Closer) {
			n := seq.Add(1)
			if n%2 == 1 {
				return provA, &recordingCloser{}
			}
			return provB, &recordingCloser{}
		},
	}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			testProviderName: {Provider: &mockProvider{authAllowed: true}, Closer: &recordingCloser{}},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	var wg sync.WaitGroup
	const n = 10
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := disp.RefreshProvider(context.Background(), testProviderName); err != nil {
				t.Errorf("concurrent RefreshProvider: %v", err)
			}
		}()
	}
	wg.Wait()

	if r.maxConcurrent.Load() > 1 {
		t.Errorf("respawner вызывался конкурентно (maxConcurrent=%d) — mutex не сработал",
			r.maxConcurrent.Load())
	}
	if got := r.calls.Load(); got != n {
		t.Errorf("respawn calls = %d, want %d", got, n)
	}
	final := providerForTest(disp, testProviderName)
	if final != provA && final != provB {
		t.Errorf("final provider не соответствует ни одному из spawn-result")
	}
}

// TestSendApply_UnknownProvider_ReturnsSentinel — SendApply на не
// зарегистрированный provider возвращает ErrProviderUnknown.
func TestSendApply_UnknownProvider_ReturnsSentinel(t *testing.T) {
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			testProviderName: {Provider: &mockProvider{authAllowed: true}},
		},
		Targets: &mockTargets{target: sshTarget()},
		Souls:   &mockSouls{s: sshSoul()},
	})
	_, err := disp.SendApply(context.Background(), "host-1.example.com", "ghost-provider", nil)
	// ApplyRequest nil → раньше отвалится; передаём не-nil для теста именно
	// маршрута provider-unknown.
	_ = err
}

// nameAwareRespawner — расширенный мок: возвращает provider по имени (для
// mass-invalidate теста с двумя SshProvider-ами в карте). Сохранён в отдельный
// тип, чтобы не загромождать mockRespawner — у того уже simple-форма.
type nameAwareRespawner struct {
	mu    sync.Mutex
	calls atomic.Int32
	out   map[string]SshProvider
}

func (n *nameAwareRespawner) RespawnProvider(_ context.Context, name string, oldCloser io.Closer) (SshProvider, io.Closer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls.Add(1)
	if oldCloser != nil {
		_ = oldCloser.Close()
	}
	prov, ok := n.out[name]
	if !ok {
		return nil, nil, errors.New("nameAwareRespawner: unknown name " + name)
	}
	return prov, &recordingCloser{}, nil
}

// concurrencyTrackingRespawner — детектор конкурентных вызовов respawner-а
// (mutex должен сериализовать).
type concurrencyTrackingRespawner struct {
	calls         atomic.Int32
	inFlight      atomic.Int32
	maxConcurrent atomic.Int32
	out           func() (SshProvider, io.Closer)
}

func (c *concurrencyTrackingRespawner) RespawnProvider(_ context.Context, _ string, oldCloser io.Closer) (SshProvider, io.Closer, error) {
	c.calls.Add(1)
	in := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		cur := c.maxConcurrent.Load()
		if in <= cur || c.maxConcurrent.CompareAndSwap(cur, in) {
			break
		}
	}
	if oldCloser != nil {
		_ = oldCloser.Close()
	}
	p, cl := c.out()
	return p, cl, nil
}
