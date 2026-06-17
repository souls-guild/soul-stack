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

// fakeScenarioLister — программируемый ScenarioLister: считает вызовы (всего и
// по ключу), отдаёт заданный scenarios или ошибку, опционально с задержкой
// для проверки per-ключ lock-а.
type fakeScenarioLister struct {
	mu        sync.Mutex
	calls     atomic.Int64
	callsKey  map[string]int
	scenarios []artifact.Scenario
	err       error
	delay     time.Duration
}

func newFakeScenarioLister() *fakeScenarioLister {
	return &fakeScenarioLister{callsKey: make(map[string]int)}
}

func (f *fakeScenarioLister) ListScenarios(_ context.Context, name, _, ref string) ([]artifact.Scenario, error) {
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
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]artifact.Scenario, len(f.scenarios))
	copy(out, f.scenarios)
	return out, nil
}

func TestScenariosCache_HitMiss(t *testing.T) {
	lister := newFakeScenarioLister()
	lister.scenarios = []artifact.Scenario{{Name: "create", Path: "scenario/create/main.yml"}}
	c := NewScenariosCache(lister, time.Hour)

	got, err := c.ListScenarios(context.Background(), "web", "g", "v1")
	if err != nil {
		t.Fatalf("#1: %v", err)
	}
	if len(got) != 1 || got[0].Name != "create" {
		t.Fatalf("scenarios = %+v", got)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls после miss = %d, want 1", n)
	}

	// Hit: тот же ключ.
	if _, err := c.ListScenarios(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 1 {
		t.Errorf("calls после hit = %d, want 1 (кеш не сработал)", n)
	}
}

func TestScenariosCache_KeyByNameAndRef(t *testing.T) {
	lister := newFakeScenarioLister()
	lister.scenarios = []artifact.Scenario{{Name: "create"}}
	c := NewScenariosCache(lister, time.Hour)

	if _, err := c.ListScenarios(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#v1: %v", err)
	}
	if _, err := c.ListScenarios(context.Background(), "web", "g", "v2"); err != nil {
		t.Fatalf("#v2: %v", err)
	}
	// Тот же name, разные ref → две независимые записи.
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (per-(name,ref) ключи)", n)
	}
}

func TestScenariosCache_Expiry(t *testing.T) {
	lister := newFakeScenarioLister()
	lister.scenarios = []artifact.Scenario{{Name: "create"}}
	c := NewScenariosCache(lister, 100*time.Millisecond)

	if _, err := c.ListScenarios(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#1: %v", err)
	}
	c.now = func() time.Time { return time.Now().Add(200 * time.Millisecond) }
	if _, err := c.ListScenarios(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("#2: %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls после TTL = %d, want 2", n)
	}
}

func TestScenariosCache_Invalidate_DropsAllRefs(t *testing.T) {
	lister := newFakeScenarioLister()
	lister.scenarios = []artifact.Scenario{{Name: "create"}}
	c := NewScenariosCache(lister, time.Hour)

	// Прогреваем три ключа: web@v1, web@v2, api@v1.
	if _, err := c.ListScenarios(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("warm web@v1: %v", err)
	}
	if _, err := c.ListScenarios(context.Background(), "web", "g", "v2"); err != nil {
		t.Fatalf("warm web@v2: %v", err)
	}
	if _, err := c.ListScenarios(context.Background(), "api", "g", "v1"); err != nil {
		t.Fatalf("warm api@v1: %v", err)
	}
	preInvalidate := lister.calls.Load()

	c.Invalidate("web")

	// Оба ref для web должны быть выкинуты, api — остаться в кеше.
	if _, err := c.ListScenarios(context.Background(), "web", "g", "v1"); err != nil {
		t.Fatalf("post-inv web@v1: %v", err)
	}
	if _, err := c.ListScenarios(context.Background(), "web", "g", "v2"); err != nil {
		t.Fatalf("post-inv web@v2: %v", err)
	}
	if _, err := c.ListScenarios(context.Background(), "api", "g", "v1"); err != nil {
		t.Fatalf("post-inv api@v1: %v", err)
	}
	// web@v1 + web@v2 пересчитаны (2 вызова), api@v1 — из кеша (0).
	if n := lister.calls.Load() - preInvalidate; n != 2 {
		t.Errorf("calls после Invalidate(\"web\") = %d, want 2 (api должен остаться в кеше)", n)
	}
}

func TestScenariosCache_ErrorNotCached(t *testing.T) {
	listerErr := errors.New("loader busted")
	lister := newFakeScenarioLister()
	lister.err = listerErr
	c := NewScenariosCache(lister, time.Hour)

	if _, err := c.ListScenarios(context.Background(), "web", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.ListScenarios(context.Background(), "web", "g", "v1"); !errors.Is(err, listerErr) {
		t.Fatalf("#2 err = %v", err)
	}
	if n := lister.calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (ошибки не кешируются)", n)
	}
}

func TestScenariosCache_PerKeyLock(t *testing.T) {
	lister := newFakeScenarioLister()
	lister.scenarios = []artifact.Scenario{{Name: "create"}}
	lister.delay = 30 * time.Millisecond
	c := NewScenariosCache(lister, time.Hour)

	const N = 5
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.ListScenarios(context.Background(), "web", "g", "v1")
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

func TestScenariosCache_NilLister_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("ожидалась паника при nil lister")
		}
	}()
	_ = NewScenariosCache(nil, time.Hour)
}

func TestScenariosCache_ListerFunc_Implements(t *testing.T) {
	var _ ScenarioLister = ScenarioListerFunc(func(_ context.Context, _, _, _ string) ([]artifact.Scenario, error) {
		return nil, nil
	})
}
