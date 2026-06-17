package serviceregistry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// fakeDependenciesLister — программируемый DependenciesLister: считает вызовы,
// отдаёт заданный deps или ошибку, опционально с задержкой для проверки
// per-ключ lock-а (parity с fakeStateSchemaLister).
type fakeDependenciesLister struct {
	calls atomic.Int64
	deps  *artifact.ServiceDependencies
	err   error
	delay time.Duration
}

func (f *fakeDependenciesLister) ListDependencies(_ context.Context, _, _, _ string) (*artifact.ServiceDependencies, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.deps == nil {
		return nil, nil
	}
	return cloneDependencies(f.deps), nil
}

func sampleDeps() *artifact.ServiceDependencies {
	return &artifact.ServiceDependencies{
		Destiny: []artifact.Dependency{{Name: "redis", Ref: "v2.0.0"}},
		Modules: []artifact.Dependency{{Name: "wb.redis-failover", Ref: "v1.2.0"}},
	}
}

func TestDependenciesCache_HitMiss(t *testing.T) {
	lister := &fakeDependenciesLister{deps: sampleDeps()}
	c := NewDependenciesCache(lister, time.Hour)

	got, err := c.ListDependencies(context.Background(), "web", "g", "v1")
	if err != nil {
		t.Fatalf("#1: %v", err)
	}
	if len(got.Destiny) != 1 || got.Destiny[0].Name != "redis" {
		t.Fatalf("Destiny = %+v", got.Destiny)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls после miss = %d, want 1", n)
	}
	if _, err := c.ListDependencies(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls после hit = %d, want 1 (кеш не сработал)", n)
	}
}

func TestDependenciesCache_KeyByNameAndRef(t *testing.T) {
	lister := &fakeDependenciesLister{deps: sampleDeps()}
	c := NewDependenciesCache(lister, time.Hour)

	if _, err := c.ListDependencies(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#v1: %v", err)
	}
	if _, err := c.ListDependencies(context.Background(), "web", "g", "v2"); err != nil {
		t.Fatalf("#v2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (per-(name,ref) ключи)", n)
	}
}

func TestDependenciesCache_Invalidate_DropsAllRefs(t *testing.T) {
	lister := &fakeDependenciesLister{deps: sampleDeps()}
	c := NewDependenciesCache(lister, time.Hour)

	for _, k := range [][2]string{{"web", "v1"}, {"web", "v2"}, {"api", "v1"}} {
		if _, err := c.ListDependencies(context.Background(), k[0], "g", k[1]); err != nil {
			t.Fatalf("warm %v: %v", k, err)
		}
	}
	pre := lister.calls.Load()

	c.Invalidate("web")

	for _, k := range [][2]string{{"web", "v1"}, {"web", "v2"}, {"api", "v1"}} {
		if _, err := c.ListDependencies(context.Background(), k[0], "g", k[1]); err != nil {
			t.Fatalf("post-inv %v: %v", k, err)
		}
	}
	if n := lister.calls.Load() - pre; n != 2 {
		t.Errorf("calls после Invalidate(\"web\") = %d, want 2 (api остаётся в кеше)", n)
	}
}

func TestDependenciesCache_ErrorNotCached(t *testing.T) {
	listerErr := errors.New("loader busted")
	lister := &fakeDependenciesLister{err: listerErr}
	c := NewDependenciesCache(lister, time.Hour)

	if _, err := c.ListDependencies(context.Background(), "web", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.ListDependencies(context.Background(), "web", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("#2 err = %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (ошибки не кешируются)", n)
	}
}

func TestDependenciesCache_PerKeyLock(t *testing.T) {
	lister := &fakeDependenciesLister{deps: sampleDeps(), delay: 30 * time.Millisecond}
	c := NewDependenciesCache(lister, time.Hour)

	const N = 5
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.ListDependencies(context.Background(), "web", "g", "v1")
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

func TestDependenciesCache_ClonesOnReturn(t *testing.T) {
	lister := &fakeDependenciesLister{deps: sampleDeps()}
	c := NewDependenciesCache(lister, time.Hour)

	got, err := c.ListDependencies(context.Background(), "web", "g", "v1")
	if err != nil {
		t.Fatalf("ListDependencies: %v", err)
	}
	got.Destiny[0].Ref = "MUTATED"
	got2, err := c.ListDependencies(context.Background(), "web", "g", "v1")
	if err != nil {
		t.Fatalf("ListDependencies #2: %v", err)
	}
	if got2.Destiny[0].Ref == "MUTATED" {
		t.Errorf("кеш не клонирует Destiny: повтор вернул %q", got2.Destiny[0].Ref)
	}
}

func TestDependenciesCache_NilLister_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("ожидалась паника при nil lister")
		}
	}()
	_ = NewDependenciesCache(nil, time.Hour)
}

func TestDependenciesCache_ListerFunc_Implements(t *testing.T) {
	var _ DependenciesLister = DependenciesListerFunc(func(_ context.Context, _, _, _ string) (*artifact.ServiceDependencies, error) {
		return nil, nil
	})
}
