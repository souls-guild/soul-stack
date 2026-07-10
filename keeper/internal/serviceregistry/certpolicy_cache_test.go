package serviceregistry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeCertPolicyLister — программируемый CertPolicyLister: считает вызовы, отдаёт
// заданный info/ошибку, опц. с задержкой для проверки per-ключ lock-а (parity с
// fakeStateSchemaLister).
type fakeCertPolicyLister struct {
	mu       sync.Mutex
	calls    atomic.Int64
	callsKey map[string]int
	info     *artifact.CertPolicyInfo
	err      error
	delay    time.Duration
}

func newFakeCertPolicyLister() *fakeCertPolicyLister {
	return &fakeCertPolicyLister{callsKey: make(map[string]int)}
}

func (f *fakeCertPolicyLister) ListCertPolicy(_ context.Context, name, _, ref string) (*artifact.CertPolicyInfo, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.callsKey[name+"|"+ref]++
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.info == nil {
		return nil, nil
	}
	cp := *f.info
	if f.info.Scenarios != nil {
		cp.Scenarios = make([]string, len(f.info.Scenarios))
		copy(cp.Scenarios, f.info.Scenarios)
	}
	return &cp, nil
}

func sampleCertPolicy() *artifact.CertPolicyInfo {
	return &artifact.CertPolicyInfo{
		Rotation:  &config.CertificateRotationConfig{Enable: true, Scenario: "rotate_tls", PKIRole: "redis"},
		Scenarios: []string{"create", "rotate_tls"},
		SHA1:      "abc123",
	}
}

func TestCertPolicyCache_HitMiss(t *testing.T) {
	lister := newFakeCertPolicyLister()
	lister.info = sampleCertPolicy()
	c := NewCertPolicyCache(lister, time.Hour)

	got, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("#1: %v", err)
	}
	if got.SHA1 != "abc123" {
		t.Fatalf("SHA1 = %q", got.SHA1)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls после miss = %d, want 1", n)
	}

	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls после hit = %d, want 1 (кеш не сработал)", n)
	}
}

func TestCertPolicyCache_KeyByNameAndRef(t *testing.T) {
	lister := newFakeCertPolicyLister()
	lister.info = sampleCertPolicy()
	c := NewCertPolicyCache(lister, time.Hour)

	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#v1: %v", err)
	}
	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v2"); err != nil {
		t.Fatalf("#v2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (per-(name,ref) ключи)", n)
	}
}

func TestCertPolicyCache_Expiry(t *testing.T) {
	lister := newFakeCertPolicyLister()
	lister.info = sampleCertPolicy()
	c := NewCertPolicyCache(lister, 100*time.Millisecond)

	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#1: %v", err)
	}
	c.now = func() time.Time { return time.Now().Add(200 * time.Millisecond) }
	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls после TTL = %d, want 2", n)
	}
}

func TestCertPolicyCache_Invalidate_DropsAllRefs(t *testing.T) {
	lister := newFakeCertPolicyLister()
	lister.info = sampleCertPolicy()
	c := NewCertPolicyCache(lister, time.Hour)

	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("warm redis@v1: %v", err)
	}
	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v2"); err != nil {
		t.Fatalf("warm redis@v2: %v", err)
	}
	if _, err := c.ListCertPolicy(context.Background(), "api", "g", "v1"); err != nil {
		t.Fatalf("warm api@v1: %v", err)
	}
	preInvalidate := lister.calls.Load()

	c.Invalidate("redis")

	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1"); err != nil {
		t.Fatalf("post-inv redis@v1: %v", err)
	}
	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v2"); err != nil {
		t.Fatalf("post-inv redis@v2: %v", err)
	}
	if _, err := c.ListCertPolicy(context.Background(), "api", "g", "v1"); err != nil {
		t.Fatalf("post-inv api@v1: %v", err)
	}
	if n := lister.calls.Load() - preInvalidate; n != 2 {
		t.Errorf("calls после Invalidate(\"redis\") = %d, want 2 (api должен остаться в кеше)", n)
	}
}

func TestCertPolicyCache_ErrorNotCached(t *testing.T) {
	listerErr := errors.New("loader busted")
	lister := newFakeCertPolicyLister()
	lister.err = listerErr
	c := NewCertPolicyCache(lister, time.Hour)

	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("#2 err = %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (ошибки не кешируются)", n)
	}
}

func TestCertPolicyCache_PerKeyLock(t *testing.T) {
	lister := newFakeCertPolicyLister()
	lister.info = sampleCertPolicy()
	lister.delay = 30 * time.Millisecond
	c := NewCertPolicyCache(lister, time.Hour)

	const N = 5
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.ListCertPolicy(context.Background(), "redis", "g", "v1")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1 (per-key lock не сработал)", n)
	}
}

func TestCertPolicyCache_ClonesOnReturn(t *testing.T) {
	lister := newFakeCertPolicyLister()
	lister.info = sampleCertPolicy()
	c := NewCertPolicyCache(lister, time.Hour)

	got, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("ListCertPolicy: %v", err)
	}
	// Mutate возвращённый Scenarios slice; повтор должен отдать нетронутый snapshot.
	got.Scenarios[0] = "MUTATED"
	got2, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("ListCertPolicy #2: %v", err)
	}
	if got2.Scenarios[0] == "MUTATED" {
		t.Errorf("кеш не клонирует Scenarios: повтор вернул %q", got2.Scenarios[0])
	}
}

// TestCertPolicyCache_ClonesRotation — review M5: мутация возвращённого .Rotation не
// должна течь в закешированную запись (deep-copy указателя, а не общий *Rotation).
func TestCertPolicyCache_ClonesRotation(t *testing.T) {
	lister := newFakeCertPolicyLister()
	lister.info = sampleCertPolicy()
	c := NewCertPolicyCache(lister, time.Hour)

	got, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("ListCertPolicy: %v", err)
	}
	if got.Rotation == nil {
		t.Fatal("Rotation не должен быть nil в sample")
	}
	got.Rotation.Scenario = "MUTATED"

	got2, err := c.ListCertPolicy(context.Background(), "redis", "g", "v1")
	if err != nil {
		t.Fatalf("ListCertPolicy #2: %v", err)
	}
	if got2.Rotation == got.Rotation {
		t.Error("каждый возврат обязан отдавать отдельный *Rotation")
	}
	if got2.Rotation.Scenario == "MUTATED" {
		t.Errorf("кеш не клонирует Rotation: повтор вернул %q", got2.Rotation.Scenario)
	}
}

func TestCertPolicyCache_NilLister_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("ожидалась паника при nil lister")
		}
	}()
	_ = NewCertPolicyCache(nil, time.Hour)
}

func TestCertPolicyCache_ListerFunc_Implements(t *testing.T) {
	var _ CertPolicyLister = CertPolicyListerFunc(func(_ context.Context, _, _, _ string) (*artifact.CertPolicyInfo, error) {
		return nil, nil
	})
}
