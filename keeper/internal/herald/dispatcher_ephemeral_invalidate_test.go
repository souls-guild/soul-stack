package herald

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// mutableSource snapshot of enabled rules that can be extended on-the-fly (mimics
// ephemeral-Tiding appearing in DB after voyage-tx commit, bypassing herald.Service
// invalidation). add is concurrent with EnabledTidings under mutex.
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

// TestEphemeralTiding_InvalidateDeliversWithWarmCache guard B (race-fix
// ADR-052(g), integrated with clock injection — deterministic, no TTL=15s wait).
//
// Reproduces bug and fix on warm cache:
//   - cache is "warm" (cachedAt fresh, TTL=1h not expired), ephemeral rules absent
//     from snapshot (inserted later, bypassing herald.Service invalidation);
//   - without explicit invalidation, scenario_run.completed emit of this voyage
//     does not enqueue DeliveryJob (dispatcher holds stale snapshot) —
//     deterministic bug reproduction;
//   - after InvalidateRules (what persist does post-commit), snapshot re-reads,
//     ephemeral rule is picked up → DeliveryJob enqueued.
func TestEphemeralTiding_InvalidateDeliversWithWarmCache(t *testing.T) {
	src := &mutableSource{}
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{Source: src, Queue: q, TTL: time.Hour})

	// Fix clock — TTL does not expire on its own (isolation from 15s).
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

	// 1. Warm cache with event of different voyage — snapshot empty, no job, cacheInit=true.
	d.Dispatch(context.Background(), &audit.Event{
		EventType:     audit.EventScenarioRunCompleted,
		CorrelationID: "vy_other",
		Payload:       map[string]any{"voyage_id": "vy_other"},
	})
	if n := len(q.snapshot()); n != 0 {
		t.Fatalf("warmup: no jobs expected, got %d", n)
	}

	// 2. Ephemeral-Tiding appears in DB (insert in voyage-tx, bypass invalidation).
	src.add(&Tiding{
		Name:       "eph-vy-fast",
		Herald:     "ops-webhook",
		EventTypes: []string{"scenario_run.completed"},
		Ephemeral:  true,
		VoyageID:   strPtr(voyageID),
		Enabled:    true,
	})

	// 3. Fast-run terminal against warm snapshot (no invalidation) →
	//    job not enqueued (deterministically reproduces bug).
	d.Dispatch(context.Background(), completed)
	if n := len(q.snapshot()); n != 0 {
		t.Fatalf("without invalidation, warm snapshot does not see ephemeral — no job expected, got %d (bug not reproduced)", n)
	}

	// 4. persist post-commit invalidates snapshot (fix) → refresh picks up ephemeral.
	d.InvalidateRules()
	d.Dispatch(context.Background(), completed)
	jobs := q.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("after invalidation ephemeral must yield exactly 1 DeliveryJob, got %d", len(jobs))
	}
	if jobs[0].Tiding != "eph-vy-fast" || jobs[0].Herald != "ops-webhook" {
		t.Errorf("job = {tiding:%s herald:%s}, want {eph-vy-fast ops-webhook}", jobs[0].Tiding, jobs[0].Herald)
	}
}
