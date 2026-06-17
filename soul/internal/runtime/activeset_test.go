package runtime

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// activeIDs извлекает отсортированный список apply_id из набора ActiveSet —
// удобно для проверки множества без зависимости от порядка (map-итерация).
func activeIDs(set []*keeperv1.ActiveApply) []string {
	out := make([]string, 0, len(set))
	for _, a := range set {
		out = append(out, a.GetApplyId())
	}
	sort.Strings(out)
	return out
}

// attemptOf находит attempt записи по apply_id (0, если записи нет).
func attemptOf(set []*keeperv1.ActiveApply, id string) int32 {
	for _, a := range set {
		if a.GetApplyId() == id {
			return a.GetAttempt()
		}
	}
	return 0
}

// TestActiveSet_Empty — свежий runner без прогонов даёт пустой (nil) набор:
// явная декларация «ничего не ведётся» (после рестарта Soul-процесса).
func TestActiveSet_Empty(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	if got := r.ActiveSet(); len(got) != 0 {
		t.Fatalf("ActiveSet() на свежем runner = %v, want пусто", activeIDs(got))
	}
}

// TestActiveSet_InFlight — apply в полёте (register без unregister) присутствует
// в наборе. Эмулируем register напрямую (Run держит cancel в active до конца).
func TestActiveSet_InFlight(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.register("apply-inflight", cancel)

	got := activeIDs(r.ActiveSet())
	if len(got) != 1 || got[0] != "apply-inflight" {
		t.Fatalf("ActiveSet() = %v, want [apply-inflight]", got)
	}
}

// TestActiveSet_RecentlyFinished — завершённый Run (unregister) остаётся в наборе
// в пределах TTL: анти-гонка «RunResult в полёте, стрим порвался до cleanup».
func TestActiveSet_RecentlyFinished(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	r := NewApplyRunner(mapRegistry{}, nil)
	r.nowFn = func() time.Time { return base }

	r.register("apply-done", func() {})
	r.unregister("apply-done") // Run завершился → ушёл в recently-finished ring

	// active пуст, но ring держит apply_id в пределах TTL.
	if _, ok := r.active["apply-done"]; ok {
		t.Fatal("apply-done остался в active после unregister")
	}
	got := activeIDs(r.ActiveSet())
	if len(got) != 1 || got[0] != "apply-done" {
		t.Fatalf("ActiveSet() сразу после unregister = %v, want [apply-done] (ring TTL)", got)
	}
}

// TestActiveSet_RingExpiresAfterTTL — запись ring выбывает из набора после TTL
// (ленивая чистка в ActiveSet по nowFn).
func TestActiveSet_RingExpiresAfterTTL(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	r := NewApplyRunner(mapRegistry{}, nil)
	r.nowFn = func() time.Time { return now }

	r.register("apply-done", func() {})
	r.unregister("apply-done")

	// Внутри TTL — присутствует.
	now = base.Add(recentlyFinishedTTL - time.Second)
	if got := activeIDs(r.ActiveSet()); len(got) != 1 {
		t.Fatalf("ActiveSet() в пределах TTL = %v, want [apply-done]", got)
	}

	// На границе TTL (>=) — вычищен.
	now = base.Add(recentlyFinishedTTL)
	if got := r.ActiveSet(); len(got) != 0 {
		t.Fatalf("ActiveSet() после TTL = %v, want пусто (ring протух)", activeIDs(got))
	}
	// Ленивая чистка реально удалила запись из map.
	if _, ok := r.recentlyFinished["apply-done"]; ok {
		t.Error("протухшая ring-запись не вычищена из recentlyFinished")
	}
}

// TestActiveSet_UnionDedup — apply в полёте И в ring (повторный прогон того же
// apply_id) даёт ОДНУ запись (дедуп по объединению).
func TestActiveSet_UnionDedup(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	r := NewApplyRunner(mapRegistry{}, nil)
	r.nowFn = func() time.Time { return base }

	r.register("apply-x", func() {})
	r.unregister("apply-x")          // в ring
	r.register("apply-x", func() {}) // снова в полёте

	got := activeIDs(r.ActiveSet())
	if len(got) != 1 || got[0] != "apply-x" {
		t.Fatalf("ActiveSet() при union active+ring = %v, want ровно [apply-x]", got)
	}
}

// TestActiveSet_AttemptEcho — attempt в наборе берётся из lastSeenAttempt
// (принятый fencing-epoch); без записи attempt=0 (старый Keeper).
func TestActiveSet_AttemptEcho(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-fenced", 7) // зафиксировал epoch
	r.register("apply-fenced", func() {})
	r.register("apply-plain", func() {}) // без attempt

	set := r.ActiveSet()
	if got := attemptOf(set, "apply-fenced"); got != 7 {
		t.Errorf("attempt[apply-fenced] = %d, want 7 (эхо lastSeenAttempt)", got)
	}
	if got := attemptOf(set, "apply-plain"); got != 0 {
		t.Errorf("attempt[apply-plain] = %d, want 0 (нет fencing-epoch)", got)
	}
}

// TestActiveSet_SurvivesReconnect — ring живёт в ApplyRunner (per-process) и
// переживает reconnect-swap стрима: после Run-а (полный прогон через Run, не
// ручной register) apply_id остаётся в наборе и доступен следующей сессии для
// WardRoster. Эмулируем «reconnect» вторым чтением ActiveSet (sink не меняет
// состояние ring-а).
func TestActiveSet_SurvivesReconnect(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{})
			},
		},
	}
	r := NewApplyRunner(reg, nil)
	r.nowFn = func() time.Time { return base }

	if !r.AcceptAttempt("apply-r", 3) {
		t.Fatal("attempt=3 отвергнут")
	}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-r",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed"}},
		Attempt: 3,
	}, &recordingSink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// «reconnect»: тот же runner, новая сессия читает ActiveSet для WardRoster.
	got := r.ActiveSet()
	if ids := activeIDs(got); len(ids) != 1 || ids[0] != "apply-r" {
		t.Fatalf("ActiveSet() после Run+reconnect = %v, want [apply-r] (ring пережил swap)", ids)
	}
	if a := attemptOf(got, "apply-r"); a != 3 {
		t.Errorf("attempt[apply-r] = %d, want 3 (epoch пережил Run)", a)
	}
}

// TestActiveSet_RestartClearsSet — рестарт процесса = новый ApplyRunner: набор
// пуст, его dispatched законно сиротятся (in-flight физически нет). Эмулируем
// рестарт созданием нового runner-а — старый ring не наследуется.
func TestActiveSet_RestartClearsSet(t *testing.T) {
	r1 := NewApplyRunner(mapRegistry{}, nil)
	r1.register("apply-old", func() {})
	r1.unregister("apply-old")
	if len(r1.ActiveSet()) != 1 {
		t.Fatal("предусловие: apply-old должен быть в ring r1")
	}

	// «Рестарт процесса» — новый runner, ничего не наследует.
	r2 := NewApplyRunner(mapRegistry{}, nil)
	if got := r2.ActiveSet(); len(got) != 0 {
		t.Fatalf("ActiveSet() нового runner после «рестарта» = %v, want пусто", activeIDs(got))
	}
}

// TestActiveSet_RaceWithRunLifecycle — конкурентные register/unregister/ActiveSet
// не дают data race (-race) на active/recentlyFinished/lastSeenAttempt.
func TestActiveSet_RaceWithRunLifecycle(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ids := []string{"a", "b", "c"}
			for i := 0; i < 200; i++ {
				id := ids[i%len(ids)]
				r.AcceptAttempt(id, int32(i))
				r.register(id, func() {})
				_ = r.ActiveSet()
				r.unregister(id)
			}
		}(w)
	}
	wg.Wait()
}
