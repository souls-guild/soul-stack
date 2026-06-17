package herald

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// mutableSource — снимок enabled-правил, который можно дополнить «на лету»
// (имитирует появление ephemeral-Tiding в БД после commit voyage-tx, в обход
// herald.Service-инвалидации). Под mutex — add конкурентен с EnabledTidings.
type mutableSource struct {
	mu    sync.Mutex
	rules []*Tiding
}

func (s *mutableSource) EnabledTidings(_ context.Context) ([]*Tiding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Tiding, len(s.rules))
	copy(out, s.rules)
	return out, nil
}

func (s *mutableSource) add(t *Tiding) {
	s.mu.Lock()
	s.rules = append(s.rules, t)
	s.mu.Unlock()
}

// TestEphemeralTiding_InvalidateDeliversWithWarmCache — guard B (race-fix
// ADR-052(g), integration с инъекцией clock — детерминированно, без ожидания
// TTL=15s).
//
// Воспроизводит баг и фикс на горячем кэше:
//   - кэш «тёплый» (cachedAt свежий, TTL=1h не истёк), ephemeral-правила в
//     снимке НЕТ (вставлено позже, в обход herald.Service-инвалидации);
//   - без явной инвалидации эмит scenario_run.completed этого voyage НЕ ставит
//     DeliveryJob (dispatcher держит устаревший снимок) — детерминированный
//     повтор бага;
//   - после InvalidateRules (то, что persist делает после commit) снимок
//     перечитывается, ephemeral-правило подхватывается → DeliveryJob ставится.
func TestEphemeralTiding_InvalidateDeliversWithWarmCache(t *testing.T) {
	src := &mutableSource{}
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{Source: src, Queue: q, TTL: time.Hour})

	// Фиксируем часы — TTL заведомо не истекает сам по себе (изоляция от 15s).
	now := time.Now()
	d.clock = func() time.Time { return now }

	const voyageID = "vy_fast"
	completed := &audit.Event{
		EventType:     audit.EventScenarioRunCompleted,
		CorrelationID: voyageID,
		Payload: map[string]any{
			"voyage_id": voyageID, "kind": "scenario", "total_batches": 1,
			"summary": map[string]any{"total": 3, "succeeded": 3, "failed": 0, "cancelled": 0},
		},
	}

	// 1. Прогреваем кэш событием другого voyage — снимок пуст, job нет, cacheInit=true.
	d.Dispatch(context.Background(), &audit.Event{
		EventType:     audit.EventScenarioRunCompleted,
		CorrelationID: "vy_other",
		Payload:       map[string]any{"voyage_id": "vy_other"},
	})
	if n := len(q.snapshot()); n != 0 {
		t.Fatalf("прогрев: job-ов быть не должно, got %d", n)
	}

	// 2. Ephemeral-Tiding появляется в БД (insert в voyage-tx, обход инвалидации).
	src.add(&Tiding{
		Name:       "eph-vy-fast",
		Herald:     "ops-webhook",
		EventTypes: []string{"scenario_run.completed"},
		Ephemeral:  true,
		VoyageID:   strPtr(voyageID),
		Enabled:    true,
	})

	// 3. Терминал быстрого прогона ПРОТИВ ТЁПЛОГО снимка (без инвалидации) →
	//    job НЕ ставится (воспроизводит баг детерминированно).
	d.Dispatch(context.Background(), completed)
	if n := len(q.snapshot()); n != 0 {
		t.Fatalf("без инвалидации тёплый снимок не видит ephemeral — job не должно быть, got %d (баг не воспроизведён)", n)
	}

	// 4. persist после commit инвалидирует снимок (фикс) → refresh подхватывает eph.
	d.InvalidateRules()
	d.Dispatch(context.Background(), completed)
	jobs := q.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("после инвалидации ephemeral должен дать ровно 1 DeliveryJob, got %d", len(jobs))
	}
	if jobs[0].Tiding != "eph-vy-fast" || jobs[0].Herald != "ops-webhook" {
		t.Errorf("job = {tiding:%s herald:%s}, want {eph-vy-fast ops-webhook}", jobs[0].Tiding, jobs[0].Herald)
	}
}
