package push

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// fakeRouterReader реализует PGRouterReader для unit-тестов. ssh_target
// возвращается из соответствующего поля; covens — из map[sid][]string.
type fakeRouterReader struct {
	target map[string]*soul.SSHTarget
	covens map[string][]string
	err    error
}

func (f *fakeRouterReader) SelectSshTarget(_ context.Context, sid string) (*soul.SSHTarget, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.target[sid], nil
}

func (f *fakeRouterReader) SelectCovens(_ context.Context, sid string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.covens[sid], nil
}

func sshTargetWithProvider(p string) *soul.SSHTarget {
	return &soul.SSHTarget{SSHPort: 22, SSHUser: "root", SoulPath: "/usr/local/bin/soul", SSHProvider: &p}
}

// TestPGRouter_Level1_SoulExplicit — souls.ssh_target.ssh_provider непустой →
// SourceSoul, провайдер из поля.
func TestPGRouter_Level1_SoulExplicit(t *testing.T) {
	r := &fakeRouterReader{
		target: map[string]*soul.SSHTarget{"sid-1": sshTargetWithProvider("vault-bastion")},
	}
	cfg := NewStaticRouterConfigSource(RouterConfig{
		ClusterDefaultProvider: "static-fallback",
	})
	router, err := NewPGRouter(r, cfg)
	if err != nil {
		t.Fatalf("NewPGRouter: %v", err)
	}
	name, src, err := router.RouteFor(context.Background(), "sid-1")
	if err != nil {
		t.Fatalf("RouteFor: %v", err)
	}
	if name != "vault-bastion" || src != SourceSoul {
		t.Errorf("got (%q, %v), want (vault-bastion, SourceSoul)", name, src)
	}
}

// TestPGRouter_Level2_CovenDefault — ssh_provider не задан, но Coven Soul-а
// есть в карте → SourceCoven.
func TestPGRouter_Level2_CovenDefault(t *testing.T) {
	r := &fakeRouterReader{
		target: map[string]*soul.SSHTarget{"sid-1": {SSHPort: 22, SSHUser: "root", SoulPath: "/usr/local/bin/soul"}},
		covens: map[string][]string{"sid-1": {"prod", "eu-west"}},
	}
	cfg := NewStaticRouterConfigSource(RouterConfig{
		CovenDefaultProviders: map[string]string{
			"prod":    "vault-bastion",
			"eu-west": "static-eu",
		},
	})
	router, _ := NewPGRouter(r, cfg)
	name, src, err := router.RouteFor(context.Background(), "sid-1")
	if err != nil {
		t.Fatalf("RouteFor: %v", err)
	}
	// Tiebreak — алфавитный порядок ковенов: eu-west < prod → static-eu.
	if name != "static-eu" || src != SourceCoven {
		t.Errorf("got (%q, %v), want (static-eu, SourceCoven)", name, src)
	}
}

// TestPGRouter_Level3_ClusterDefault — ни Level 1, ни Level 2 не дали match →
// SourceCluster.
func TestPGRouter_Level3_ClusterDefault(t *testing.T) {
	r := &fakeRouterReader{
		target: map[string]*soul.SSHTarget{"sid-1": {SSHPort: 22, SSHUser: "root", SoulPath: "/usr/local/bin/soul"}},
		covens: map[string][]string{"sid-1": {"prod"}},
	}
	cfg := NewStaticRouterConfigSource(RouterConfig{
		CovenDefaultProviders:  map[string]string{"stage": "static-stage"},
		ClusterDefaultProvider: "vault-bastion",
	})
	router, _ := NewPGRouter(r, cfg)
	name, src, err := router.RouteFor(context.Background(), "sid-1")
	if err != nil {
		t.Fatalf("RouteFor: %v", err)
	}
	if name != "vault-bastion" || src != SourceCluster {
		t.Errorf("got (%q, %v), want (vault-bastion, SourceCluster)", name, src)
	}
}

// TestPGRouter_NotRouted — все три уровня пусты → ErrProviderNotRouted.
func TestPGRouter_NotRouted(t *testing.T) {
	r := &fakeRouterReader{}
	cfg := NewStaticRouterConfigSource(RouterConfig{})
	router, _ := NewPGRouter(r, cfg)
	_, _, err := router.RouteFor(context.Background(), "sid-1")
	if !errors.Is(err, ErrProviderNotRouted) {
		t.Errorf("err = %v, want ErrProviderNotRouted", err)
	}
}

// TestPGRouter_PGError_PropagatesUnknown — реальная PG-ошибка читается как
// SourceUnknown + wrapped error (не ErrProviderNotRouted).
func TestPGRouter_PGError_PropagatesUnknown(t *testing.T) {
	r := &fakeRouterReader{err: errors.New("conn refused")}
	cfg := NewStaticRouterConfigSource(RouterConfig{})
	router, _ := NewPGRouter(r, cfg)
	_, src, err := router.RouteFor(context.Background(), "sid-1")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrProviderNotRouted) {
		t.Error("PG transport error не должен маскироваться под ErrProviderNotRouted")
	}
	if src != SourceUnknown {
		t.Errorf("source = %v, want SourceUnknown", src)
	}
}

// TestPGRouter_CovenAlphabeticalTiebreak — детерминизм tiebreak: если Soul в
// нескольких ковенах, каждый со своим default, побеждает алфабетически первый.
func TestPGRouter_CovenAlphabeticalTiebreak(t *testing.T) {
	r := &fakeRouterReader{
		target: map[string]*soul.SSHTarget{"sid-1": {SSHPort: 22, SSHUser: "root", SoulPath: "/usr/local/bin/soul"}},
		covens: map[string][]string{"sid-1": {"zeta", "alpha", "mu"}},
	}
	cfg := NewStaticRouterConfigSource(RouterConfig{
		CovenDefaultProviders: map[string]string{
			"alpha": "a-provider",
			"mu":    "m-provider",
			"zeta":  "z-provider",
		},
	})
	router, _ := NewPGRouter(r, cfg)
	// alpha < mu < zeta → выигрывает alpha.
	name, _, err := router.RouteFor(context.Background(), "sid-1")
	if err != nil {
		t.Fatalf("RouteFor: %v", err)
	}
	if name != "a-provider" {
		t.Errorf("got %q, want a-provider (alphabetical tiebreak)", name)
	}
}

// --- Multi-provider dispatcher tests (W-2 map + concurrent-safety) ---

// TestDispatcher_MultiProvider_LookupByName — карта несёт двух провайдеров,
// SendApply каждому уходит на свой mock (косвенно через successful resolve).
func TestDispatcher_MultiProvider_LookupByName(t *testing.T) {
	provA := &mockProvider{authAllowed: true}
	provB := &mockProvider{authAllowed: true}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			"static": {Provider: provA},
			"vault":  {Provider: provB},
		},
		Targets: &mockTargets{target: sshTarget()},
		Souls:   &mockSouls{s: sshSoul()},
	})

	if !disp.HasProvider("static") || !disp.HasProvider("vault") {
		t.Error("HasProvider не видит обоих провайдеров")
	}
	if disp.HasProvider("ghost") {
		t.Error("HasProvider даёт true для незарегистрированного имени")
	}
	names := disp.ProviderNames()
	if len(names) != 2 {
		t.Errorf("ProviderNames len = %d, want 2", len(names))
	}
}

// TestDispatcher_SendApply_UnknownProvider — SendApply на не зарегистрированный
// provider возвращает ErrProviderUnknown.
func TestDispatcher_SendApply_UnknownProvider(t *testing.T) {
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			"static": {Provider: &mockProvider{authAllowed: true}},
		},
		Targets: &mockTargets{target: sshTarget()},
		Souls:   &mockSouls{s: sshSoul()},
	})
	// non-nil request требуется для прохождения первой проверки SendApply.
	req := &keeperv1.ApplyRequest{ApplyId: "ap-ghost"}
	_, err := disp.SendApply(context.Background(), "host-1.example.com", "ghost", req)
	if !errors.Is(err, ErrProviderUnknown) {
		t.Errorf("err = %v, want ErrProviderUnknown", err)
	}
}

// TestDispatcher_RefreshProvider_DoesNotBlockOtherProviders — concurrent
// RefreshProvider("static") не блокирует HasProvider("vault") долго: refresh
// держит Lock, но HasProvider/ProviderNames — RLock → конкурентны для read-pure.
// Тест проверяет, что новое имя не появляется параллельно (refresh для другого
// имени работает изолированно).
func TestDispatcher_RefreshProvider_PerNameIsolated(t *testing.T) {
	provA := &mockProvider{authAllowed: true}
	provB := &mockProvider{authAllowed: true}
	newA := &mockProvider{authAllowed: true}
	r := &nameAwareRespawner{out: map[string]SshProvider{"static": newA}}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			"static": {Provider: provA, Closer: &recordingCloser{}},
			"vault":  {Provider: provB, Closer: &recordingCloser{}},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	if err := disp.RefreshProvider(context.Background(), "static"); err != nil {
		t.Fatalf("RefreshProvider(static): %v", err)
	}
	// static подменён, vault остался прежним.
	if providerForTest(disp, "static") != newA {
		t.Errorf("static не подменился")
	}
	if providerForTest(disp, "vault") != provB {
		t.Errorf("vault затронут чужим refresh: должен остаться provB")
	}
}

// TestDispatcher_Concurrent_ReadWrite — параллельные HasProvider/ProviderNames
// и RefreshProvider не приводят к panic-у/гонкам (race-detector).
func TestDispatcher_Concurrent_ReadWrite(t *testing.T) {
	provA := &mockProvider{authAllowed: true}
	newA := &mockProvider{authAllowed: true}
	r := &nameAwareRespawner{out: map[string]SshProvider{"static": newA}}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{
			"static": {Provider: provA, Closer: &recordingCloser{}},
		},
		Respawner: r,
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
	})

	var wg sync.WaitGroup
	var refreshCount atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			disp.HasProvider("static")
			_ = disp.ProviderNames()
		}()
		go func() {
			defer wg.Done()
			if err := disp.RefreshProvider(context.Background(), "static"); err == nil {
				refreshCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if refreshCount.Load() == 0 {
		t.Error("ни один RefreshProvider не прошёл")
	}
}
