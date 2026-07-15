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

// activeIDs extracts a sorted list of apply_ids from an ActiveSet — useful for
// order-independent set comparisons (map iteration).
func activeIDs(set []*keeperv1.ActiveApply) []string {
	out := make([]string, 0, len(set))
	for _, a := range set {
		out = append(out, a.GetApplyId())
	}
	sort.Strings(out)
	return out
}

// attemptOf finds the attempt for an apply_id (0 if there's no record).
func attemptOf(set []*keeperv1.ActiveApply, id string) int32 {
	for _, a := range set {
		if a.GetApplyId() == id {
			return a.GetAttempt()
		}
	}
	return 0
}

// TestActiveSet_Empty — a fresh runner with no runs yields an empty (nil) set:
// an explicit "nothing in flight" (e.g. after a Soul process restart).
func TestActiveSet_Empty(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	if got := r.ActiveSet(); len(got) != 0 {
		t.Fatalf("ActiveSet() на свежем runner = %v, want пусто", activeIDs(got))
	}
}

// TestActiveSet_InFlight — an in-flight apply (register without unregister) is
// present in the set. Emulates register directly (Run holds cancel in active
// until it's done).
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

// TestActiveSet_RecentlyFinished — a finished Run (unregister) stays in the
// set within TTL: anti-race for "RunResult in flight, stream broke before
// cleanup".
func TestActiveSet_RecentlyFinished(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	r := NewApplyRunner(mapRegistry{}, nil)
	r.nowFn = func() time.Time { return base }

	r.register("apply-done", func() {})
	r.unregister("apply-done") // Run finished → moved into the recently-finished ring

	// active is empty, but the ring holds apply_id within TTL.
	if _, ok := r.active["apply-done"]; ok {
		t.Fatal("apply-done остался в active после unregister")
	}
	got := activeIDs(r.ActiveSet())
	if len(got) != 1 || got[0] != "apply-done" {
		t.Fatalf("ActiveSet() сразу после unregister = %v, want [apply-done] (ring TTL)", got)
	}
}

// TestActiveSet_RingExpiresAfterTTL — a ring entry drops out of the set after
// TTL (lazy cleanup in ActiveSet via nowFn).
func TestActiveSet_RingExpiresAfterTTL(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	r := NewApplyRunner(mapRegistry{}, nil)
	r.nowFn = func() time.Time { return now }

	r.register("apply-done", func() {})
	r.unregister("apply-done")

	// Within TTL — present.
	now = base.Add(recentlyFinishedTTL - time.Second)
	if got := activeIDs(r.ActiveSet()); len(got) != 1 {
		t.Fatalf("ActiveSet() в пределах TTL = %v, want [apply-done]", got)
	}

	// At the TTL boundary (>=) — evicted.
	now = base.Add(recentlyFinishedTTL)
	if got := r.ActiveSet(); len(got) != 0 {
		t.Fatalf("ActiveSet() после TTL = %v, want пусто (ring протух)", activeIDs(got))
	}
	// Lazy cleanup actually removed the entry from the map.
	if _, ok := r.recentlyFinished["apply-done"]; ok {
		t.Error("протухшая ring-запись не вычищена из recentlyFinished")
	}
}

// TestActiveSet_UnionDedup — an apply both in-flight AND in the ring (rerun of
// the same apply_id) yields exactly ONE entry (dedup on union).
func TestActiveSet_UnionDedup(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	r := NewApplyRunner(mapRegistry{}, nil)
	r.nowFn = func() time.Time { return base }

	r.register("apply-x", func() {})
	r.unregister("apply-x")          // in the ring
	r.register("apply-x", func() {}) // in flight again

	got := activeIDs(r.ActiveSet())
	if len(got) != 1 || got[0] != "apply-x" {
		t.Fatalf("ActiveSet() при union active+ring = %v, want ровно [apply-x]", got)
	}
}

// TestActiveSet_AttemptEcho — attempt in the set comes from lastSeenAttempt
// (the accepted fencing epoch); no record means attempt=0 (older Keeper).
func TestActiveSet_AttemptEcho(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-fenced", 7) // recorded the epoch
	r.register("apply-fenced", func() {})
	r.register("apply-plain", func() {}) // no attempt

	set := r.ActiveSet()
	if got := attemptOf(set, "apply-fenced"); got != 7 {
		t.Errorf("attempt[apply-fenced] = %d, want 7 (эхо lastSeenAttempt)", got)
	}
	if got := attemptOf(set, "apply-plain"); got != 0 {
		t.Errorf("attempt[apply-plain] = %d, want 0 (нет fencing-epoch)", got)
	}
}

// TestActiveSet_SurvivesReconnect — the ring lives in ApplyRunner (per
// process) and survives a stream reconnect-swap: after a full Run (not a
// manual register), apply_id stays in the set and is available to the next
// session's WardRoster. Emulates "reconnect" via a second ActiveSet read (the
// sink doesn't change the ring's state).
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

	// "reconnect": same runner, a new session reads ActiveSet for WardRoster.
	got := r.ActiveSet()
	if ids := activeIDs(got); len(ids) != 1 || ids[0] != "apply-r" {
		t.Fatalf("ActiveSet() после Run+reconnect = %v, want [apply-r] (ring пережил swap)", ids)
	}
	if a := attemptOf(got, "apply-r"); a != 3 {
		t.Errorf("attempt[apply-r] = %d, want 3 (epoch пережил Run)", a)
	}
}

// TestActiveSet_RestartClearsSet — a process restart means a new ApplyRunner:
// the set is empty, its dispatched work is legitimately orphaned (nothing is
// physically in flight). Emulates restart by creating a new runner — the old
// ring isn't inherited.
func TestActiveSet_RestartClearsSet(t *testing.T) {
	r1 := NewApplyRunner(mapRegistry{}, nil)
	r1.register("apply-old", func() {})
	r1.unregister("apply-old")
	if len(r1.ActiveSet()) != 1 {
		t.Fatal("предусловие: apply-old должен быть в ring r1")
	}

	// "Process restart" — new runner, nothing inherited.
	r2 := NewApplyRunner(mapRegistry{}, nil)
	if got := r2.ActiveSet(); len(got) != 0 {
		t.Fatalf("ActiveSet() нового runner после «рестарта» = %v, want пусто", activeIDs(got))
	}
}

// TestActiveSet_RaceWithRunLifecycle — concurrent register/unregister/ActiveSet
// produce no data race (-race) on active/recentlyFinished/lastSeenAttempt.
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
