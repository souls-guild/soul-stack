package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// TestAcceptAttempt_FirstAttemptAccepted — the first ApplyRequest for an
// apply_id is accepted and records seen (ADR-027(g)).
func TestAcceptAttempt_FirstAttemptAccepted(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	if !r.AcceptAttempt("apply-1", 1) {
		t.Fatalf("AcceptAttempt(apply-1, 1) = false, want true (первый attempt)")
	}
	if got := r.lastSeenAttempt["apply-1"]; got != 1 {
		t.Errorf("seen[apply-1] = %d, want 1", got)
	}
}

// TestAcceptAttempt_HigherAttemptAccepted — a re-claim with a higher attempt
// (recovery returned the Ward → ClaimNext incremented it) is accepted and
// advances seen.
func TestAcceptAttempt_HigherAttemptAccepted(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-1", 1)
	if !r.AcceptAttempt("apply-1", 2) {
		t.Fatalf("AcceptAttempt(apply-1, 2) = false, want true (больший attempt)")
	}
	if got := r.lastSeenAttempt["apply-1"]; got != 2 {
		t.Errorf("seen[apply-1] = %d, want 2", got)
	}
}

// TestAcceptAttempt_EqualAttemptAccepted — an equal attempt is accepted
// (redelivery of the same epoch isn't stale; "==" can't be fenced, the
// SID lease rejects true duplicates of the same attempt).
func TestAcceptAttempt_EqualAttemptAccepted(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-1", 2)
	if !r.AcceptAttempt("apply-1", 2) {
		t.Errorf("AcceptAttempt(apply-1, 2) повторно = false, want true (==seen не stale)")
	}
}

// TestAcceptAttempt_StaleRejected — attempt < seen is rejected (stale
// duplicate: an expired Ward whose apply is still in flight, while a
// re-claim with a higher attempt has already been accepted).
func TestAcceptAttempt_StaleRejected(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-1", 3) // original (higher) already accepted
	if r.AcceptAttempt("apply-1", 1) {
		t.Fatalf("AcceptAttempt(apply-1, 1) = true, want false (stale < seen=3)")
	}
	// seen did NOT roll back.
	if got := r.lastSeenAttempt["apply-1"]; got != 3 {
		t.Errorf("seen[apply-1] = %d, want 3 (stale не должен сдвигать seen)", got)
	}
}

// TestAcceptAttempt_ZeroNeverFenced — attempt=0 (an old Keeper without the
// fencing field, forward-compat) is always accepted and NOT recorded in the
// cache, so it doesn't "poison" seen for later fencing requests.
func TestAcceptAttempt_ZeroNeverFenced(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	// Even after seeing attempt=5, zero is still accepted (old Keeper).
	r.AcceptAttempt("apply-1", 5)
	if !r.AcceptAttempt("apply-1", 0) {
		t.Errorf("AcceptAttempt(apply-1, 0) = false, want true (старый Keeper не фенсится)")
	}
	// attempt=0 didn't move seen down.
	if got := r.lastSeenAttempt["apply-1"]; got != 5 {
		t.Errorf("seen[apply-1] = %d, want 5 (0 не пишется в кеш)", got)
	}

	// Fresh apply with nothing seen yet: 0 is accepted, cache stays empty (0 isn't written).
	if !r.AcceptAttempt("apply-fresh", 0) {
		t.Errorf("AcceptAttempt(apply-fresh, 0) = false, want true")
	}
	if _, ok := r.lastSeenAttempt["apply-fresh"]; ok {
		t.Errorf("seen[apply-fresh] записан для attempt=0, ожидалось отсутствие")
	}
}

// TestAcceptAttempt_PerApplyIDIsolation — the cache is kept per apply_id:
// staleness on one run doesn't affect another.
func TestAcceptAttempt_PerApplyIDIsolation(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-a", 5)
	// apply-b sees attempt=1 for the first time — accepted (isolated from apply-a).
	if !r.AcceptAttempt("apply-b", 1) {
		t.Errorf("AcceptAttempt(apply-b, 1) = false, want true (другой apply_id)")
	}
}

// TestAcceptAttempt_RejectedIncrementsMetric — a rejected stale duplicate
// increments soul_apply_fenced_total (B1: the metric is the only external
// trace of a rejection).
func TestAcceptAttempt_RejectedIncrementsMetric(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterApplyMetrics(reg)
	r := NewApplyRunner(mapRegistry{}, m)

	r.AcceptAttempt("apply-1", 2)
	// Two stale duplicates in a row → counter reaches 2.
	if r.AcceptAttempt("apply-1", 1) {
		t.Fatal("первый stale принят, want отвергнут")
	}
	if r.AcceptAttempt("apply-1", 0+1) { // attempt=1 < seen=2
		t.Fatal("второй stale принят, want отвергнут")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "soul_apply_fenced_total 2") {
		t.Errorf("expected soul_apply_fenced_total 2; got=\n%s", body)
	}
}

// TestAcceptAttempt_AcceptedDoesNotIncrementMetric — an accepted (non-stale)
// request does NOT touch the fenced counter.
func TestAcceptAttempt_AcceptedDoesNotIncrementMetric(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterApplyMetrics(reg)
	r := NewApplyRunner(mapRegistry{}, m)

	r.AcceptAttempt("apply-1", 1)
	r.AcceptAttempt("apply-1", 2) // higher — accepted
	r.AcceptAttempt("apply-2", 0) // old Keeper — accepted

	body := obstest.Scrape(t, reg.Gatherer())
	// A CounterVec/Counter without Inc isn't published — there should be no
	// fenced series in body (or it's 0). Check for absence of a positive value.
	if strings.Contains(body, "soul_apply_fenced_total 1") ||
		strings.Contains(body, "soul_apply_fenced_total 2") {
		t.Errorf("fenced-счётчик инкрементирован на принятых запросах; got=\n%s", body)
	}
}

// TestAcceptAttempt_CachePersistsAcrossRunnerLifetime — the cache lives in
// ApplyRunner (per-process) and survives a stream reconnect-swap: in
// cmd/soul, failback/reconnect recreates the StreamSession, but there's ONE
// runner per process. We emulate a swap by running two different sinks
// (≈ two sessions) on one runner — seen persists between them, so a stale
// attempt after a "swap" is rejected.
func TestAcceptAttempt_CachePersistsAcrossRunnerLifetime(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{})
			},
		},
	}
	r := NewApplyRunner(reg, nil)

	// Session #1: accept attempt=2 and execute.
	if !r.AcceptAttempt("apply-x", 2) {
		t.Fatal("attempt=2 на сессии №1 отвергнут")
	}
	sink1 := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-x",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed"}},
		Attempt: 2,
	}, sink1); err != nil {
		t.Fatalf("Run сессии №1: %v", err)
	}

	// "reconnect-swap": same runner, new session (sink2). A stale attempt=1
	// arrives (recovery re-queued a still-alive Ward; the original with
	// attempt=2 already ran). The per-process cache remembers seen=2 → reject.
	if r.AcceptAttempt("apply-x", 1) {
		t.Fatal("stale attempt=1 после swap принят — кеш не пережил swap (баг)")
	}
}

// TestAcceptAttempt_RaceOnGuardMap — concurrent AcceptAttempt calls across
// different and identical apply_ids produce no data race on
// lastSeenAttempt (-race).
func TestAcceptAttempt_RaceOnGuardMap(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ids := []string{"a", "b", "c"}
			for i := int32(1); i <= 100; i++ {
				_ = r.AcceptAttempt(ids[int(i)%len(ids)], i)
				_ = r.AcceptAttempt("shared", i)
			}
		}(w)
	}
	wg.Wait()
}

// TestAcceptAttempt_B1_NoSideChannelOnReject — the B1 invariant at the guard
// level: a rejected stale attempt does nothing besides the metric/log —
// RunResult/TaskEvent are NOT produced here (only Run sends those, and the
// caller doesn't invoke Run on rejection). Verifies rejection is a pure bool
// with no write to the active registry (no registered cancel that would hint
// Run was started).
func TestAcceptAttempt_B1_NoSideChannelOnReject(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-1", 2)
	_ = r.AcceptAttempt("apply-1", 1) // stale, rejected

	// A rejected request never started Run → never registered a cancel in active.
	if r.Cancel("apply-1") {
		t.Errorf("Cancel(apply-1) = true: отвергнутый stale не должен регистрировать active-apply")
	}
}
