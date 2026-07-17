package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
)

// fakeClusterRegistry — mock Conclave registry (LiveKIDs + per-KID meta).
type fakeClusterRegistry struct {
	kids    []string
	meta    map[string]string
	kidsErr error
}

func (f *fakeClusterRegistry) LiveKIDs(_ context.Context) ([]string, error) {
	if f.kidsErr != nil {
		return nil, f.kidsErr
	}
	return f.kids, nil
}

func (f *fakeClusterRegistry) InstanceMeta(_ context.Context, kid string) (string, bool, error) {
	v, ok := f.meta[kid]
	return v, ok, nil
}

// fakeLeaderReader — mock read of the reaper:leader key holder.
type fakeLeaderReader struct {
	holder string
	ok     bool
	err    error
}

func (f *fakeLeaderReader) ReaperLeaderHolder(_ context.Context) (string, bool, error) {
	return f.holder, f.ok, f.err
}

// okPinger / failPinger — health.Pingers for self_health.
type okPinger struct{}

func (okPinger) Ping(_ context.Context) error { return nil }

type failPinger struct{}

func (failPinger) Ping(_ context.Context) error { return errors.New("down") }

func metaJSON(startedAt string) string {
	return `{"started_at":"` + startedAt + `","kid":"x"}`
}

// TestClusterGetTyped_ListsInstancesFromConclave — MAIN guard: the instance list is
// built from Conclave (mock LiveKIDs), started_at is parsed from meta, self_kid and
// self_health are present. Instances are sorted by KID.
func TestClusterGetTyped_ListsInstancesFromConclave(t *testing.T) {
	reg := &fakeClusterRegistry{
		kids: []string{"keeper-c", "keeper-a", "keeper-b"},
		meta: map[string]string{
			"keeper-a": metaJSON("2026-07-01T10:00:00Z"),
			"keeper-b": metaJSON("2026-07-01T11:00:00Z"),
			"keeper-c": metaJSON("2026-07-01T12:00:00Z"),
		},
	}
	h := NewClusterHandler(reg, &fakeLeaderReader{}, health.Deps{PG: okPinger{}, Redis: okPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if reply.Body.SelfKID != "keeper-a" {
		t.Errorf("SelfKID = %q, want keeper-a", reply.Body.SelfKID)
	}
	if len(reply.Body.Instances) != 3 {
		t.Fatalf("Instances len = %d, want 3", len(reply.Body.Instances))
	}
	// Sorted by KID.
	wantOrder := []string{"keeper-a", "keeper-b", "keeper-c"}
	for i, inst := range reply.Body.Instances {
		if inst.KID != wantOrder[i] {
			t.Errorf("Instances[%d].KID = %q, want %q (stable order by KID)", i, inst.KID, wantOrder[i])
		}
		if !inst.Alive {
			t.Errorf("Instances[%d].Alive = false, want true (KID from LiveKIDs)", i)
		}
		if inst.StartedAt == nil {
			t.Errorf("Instances[%d].StartedAt = nil, want parsed from meta", i)
		}
	}
	// self_health reuses health.Check.
	if reply.Body.SelfHealth["postgres"] != "ok" || reply.Body.SelfHealth["redis"] != "ok" {
		t.Errorf("SelfHealth = %v, want postgres:ok redis:ok", reply.Body.SelfHealth)
	}
	if _, hasVault := reply.Body.SelfHealth["vault"]; hasVault {
		t.Errorf("SelfHealth contains vault, but the Vault-pinger was nil (check should have been skipped): %v", reply.Body.SelfHealth)
	}
}

// TestClusterGetTyped_ReaperLeaderHolder — guard: is_reaper_leader is set on exactly
// the KID whose identifier = holder of the reaper:leader key.
func TestClusterGetTyped_ReaperLeaderHolder(t *testing.T) {
	reg := &fakeClusterRegistry{kids: []string{"keeper-a", "keeper-b"}, meta: map[string]string{}}
	h := NewClusterHandler(reg, &fakeLeaderReader{holder: "keeper-b", ok: true}, health.Deps{PG: okPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	byKID := map[string]ClusterInstanceView{}
	for _, inst := range reply.Body.Instances {
		byKID[inst.KID] = inst
	}
	if !byKID["keeper-b"].IsReaperLeader {
		t.Error("keeper-b (holder of the reaper:leader key) IsReaperLeader=false, want true")
	}
	if byKID["keeper-a"].IsReaperLeader {
		t.Error("keeper-a (not the holder) IsReaperLeader=true, want false")
	}
}

// TestClusterGetTyped_NoReaperLeader — no leader (lease free) → is_reaper_leader is set
// on nobody.
func TestClusterGetTyped_NoReaperLeader(t *testing.T) {
	reg := &fakeClusterRegistry{kids: []string{"keeper-a", "keeper-b"}, meta: map[string]string{}}
	h := NewClusterHandler(reg, &fakeLeaderReader{ok: false}, health.Deps{PG: okPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	for _, inst := range reply.Body.Instances {
		if inst.IsReaperLeader {
			t.Errorf("%s IsReaperLeader=true with no leader present, want false", inst.KID)
		}
	}
}

// TestClusterGetTyped_LeaderReadError_FailSafe — a holder-read error does not crash the
// view: is_reaper_leader=false for all, the instance list stays intact.
func TestClusterGetTyped_LeaderReadError_FailSafe(t *testing.T) {
	reg := &fakeClusterRegistry{kids: []string{"keeper-a"}, meta: map[string]string{}}
	h := NewClusterHandler(reg, &fakeLeaderReader{err: errors.New("redis down")}, health.Deps{PG: okPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v (leader-read-error should be fail-safe, not 500)", err)
	}
	if len(reply.Body.Instances) != 1 || reply.Body.Instances[0].IsReaperLeader {
		t.Errorf("view = %+v, want 1 instance without reaper-leader (fail-safe)", reply.Body.Instances)
	}
}

// TestClusterGetTyped_ConclaveDown_SelfOnly — Conclave unavailable (LiveKIDs error):
// not 500 but a self-only view (the current instance is always visible). self_health
// shows the cause (redis down).
func TestClusterGetTyped_ConclaveDown_SelfOnly(t *testing.T) {
	reg := &fakeClusterRegistry{kidsErr: errors.New("redis unreachable")}
	h := NewClusterHandler(reg, &fakeLeaderReader{}, health.Deps{PG: okPinger{}, Redis: failPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v (Conclave-down should be fail-safe, not 500)", err)
	}
	if len(reply.Body.Instances) != 1 || reply.Body.Instances[0].KID != "keeper-a" {
		t.Fatalf("Instances = %+v, want self-only [keeper-a]", reply.Body.Instances)
	}
	if !reply.Body.Instances[0].Alive {
		t.Error("self-instance Alive=false, want true")
	}
	// self_health reflects the Redis failure.
	if reply.Body.SelfHealth["redis"] == "ok" {
		t.Errorf("SelfHealth[redis] = ok, want the failure reason: %v", reply.Body.SelfHealth)
	}
}

// TestClusterGetTyped_SelfMissingFromLiveKIDs — self absent from LiveKIDs (registration
// race at startup) → appended to the list.
func TestClusterGetTyped_SelfMissingFromLiveKIDs(t *testing.T) {
	reg := &fakeClusterRegistry{kids: []string{"keeper-b"}, meta: map[string]string{}}
	h := NewClusterHandler(reg, &fakeLeaderReader{}, health.Deps{PG: okPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	found := map[string]bool{}
	for _, inst := range reply.Body.Instances {
		found[inst.KID] = true
	}
	if !found["keeper-a"] || !found["keeper-b"] {
		t.Errorf("Instances = %+v, want contains both keeper-a (self, appended) and keeper-b", reply.Body.Instances)
	}
}

// TestClusterGetTyped_BadMeta_StartedAtOmitted — non-JSON / corrupt meta → instance in
// the list, but StartedAt=nil (fail-safe, not a crash).
func TestClusterGetTyped_BadMeta_StartedAtOmitted(t *testing.T) {
	reg := &fakeClusterRegistry{
		kids: []string{"keeper-a"},
		meta: map[string]string{"keeper-a": "keeper-a"}, // bare KID (fail-safe RegisterInstance), not JSON
	}
	h := NewClusterHandler(reg, &fakeLeaderReader{}, health.Deps{PG: okPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if len(reply.Body.Instances) != 1 {
		t.Fatalf("Instances len = %d, want 1", len(reply.Body.Instances))
	}
	if reply.Body.Instances[0].StartedAt != nil {
		t.Errorf("StartedAt = %v, want nil (non-JSON meta -> omitted)", reply.Body.Instances[0].StartedAt)
	}
	if !reply.Body.Instances[0].Alive {
		t.Error("Alive=false with corrupt meta, want true (meta doesn't affect alive)")
	}
}
