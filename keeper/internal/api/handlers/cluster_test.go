package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
)

// fakeClusterRegistry — mock Conclave-реестра (LiveKIDs + per-KID meta).
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

// fakeLeaderReader — mock чтения holder-а ключа reaper:leader.
type fakeLeaderReader struct {
	holder string
	ok     bool
	err    error
}

func (f *fakeLeaderReader) ReaperLeaderHolder(_ context.Context) (string, bool, error) {
	return f.holder, f.ok, f.err
}

// okPinger / failPinger — health.Pinger-ы для self_health.
type okPinger struct{}

func (okPinger) Ping(_ context.Context) error { return nil }

type failPinger struct{}

func (failPinger) Ping(_ context.Context) error { return errors.New("down") }

func metaJSON(startedAt string) string {
	return `{"started_at":"` + startedAt + `","kid":"x"}`
}

// TestClusterGetTyped_ListsInstancesFromConclave — ГЛАВНЫЙ guard: список инстансов
// строится из Conclave (mock LiveKIDs), started_at парсится из meta, self_kid и
// self_health присутствуют. Инстансы отсортированы по KID.
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
	// Отсортированы по KID.
	wantOrder := []string{"keeper-a", "keeper-b", "keeper-c"}
	for i, inst := range reply.Body.Instances {
		if inst.KID != wantOrder[i] {
			t.Errorf("Instances[%d].KID = %q, want %q (стабильный порядок по KID)", i, inst.KID, wantOrder[i])
		}
		if !inst.Alive {
			t.Errorf("Instances[%d].Alive = false, want true (KID из LiveKIDs)", i)
		}
		if inst.StartedAt == nil {
			t.Errorf("Instances[%d].StartedAt = nil, want распарсенный из meta", i)
		}
	}
	// self_health переиспользует health.Check.
	if reply.Body.SelfHealth["postgres"] != "ok" || reply.Body.SelfHealth["redis"] != "ok" {
		t.Errorf("SelfHealth = %v, want postgres:ok redis:ok", reply.Body.SelfHealth)
	}
	if _, hasVault := reply.Body.SelfHealth["vault"]; hasVault {
		t.Errorf("SelfHealth содержит vault, а Vault-pinger был nil (check должен пропуститься): %v", reply.Body.SelfHealth)
	}
}

// TestClusterGetTyped_ReaperLeaderHolder — guard: is_reaper_leader выставлен ровно
// у того KID, чей идентификатор = holder ключа reaper:leader.
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
		t.Error("keeper-b (holder ключа reaper:leader) IsReaperLeader=false, want true")
	}
	if byKID["keeper-a"].IsReaperLeader {
		t.Error("keeper-a (не holder) IsReaperLeader=true, want false")
	}
}

// TestClusterGetTyped_NoReaperLeader — нет лидера (lease свободен) → ни у кого не
// выставлен is_reaper_leader.
func TestClusterGetTyped_NoReaperLeader(t *testing.T) {
	reg := &fakeClusterRegistry{kids: []string{"keeper-a", "keeper-b"}, meta: map[string]string{}}
	h := NewClusterHandler(reg, &fakeLeaderReader{ok: false}, health.Deps{PG: okPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	for _, inst := range reply.Body.Instances {
		if inst.IsReaperLeader {
			t.Errorf("%s IsReaperLeader=true при отсутствии лидера, want false", inst.KID)
		}
	}
}

// TestClusterGetTyped_LeaderReadError_FailSafe — ошибка чтения holder-а не роняет
// view: is_reaper_leader=false у всех, список инстансов цел.
func TestClusterGetTyped_LeaderReadError_FailSafe(t *testing.T) {
	reg := &fakeClusterRegistry{kids: []string{"keeper-a"}, meta: map[string]string{}}
	h := NewClusterHandler(reg, &fakeLeaderReader{err: errors.New("redis down")}, health.Deps{PG: okPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v (leader-read-error должен быть fail-safe, не 500)", err)
	}
	if len(reply.Body.Instances) != 1 || reply.Body.Instances[0].IsReaperLeader {
		t.Errorf("view = %+v, want 1 инстанс без reaper-leader (fail-safe)", reply.Body.Instances)
	}
}

// TestClusterGetTyped_ConclaveDown_SelfOnly — Conclave недоступен (LiveKIDs error):
// НЕ 500, а self-only view (текущий инстанс всегда виден). self_health покажет
// причину (redis down).
func TestClusterGetTyped_ConclaveDown_SelfOnly(t *testing.T) {
	reg := &fakeClusterRegistry{kidsErr: errors.New("redis unreachable")}
	h := NewClusterHandler(reg, &fakeLeaderReader{}, health.Deps{PG: okPinger{}, Redis: failPinger{}}, "keeper-a", nil)

	reply, err := h.GetTyped(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTyped: %v (Conclave-down должен быть fail-safe, не 500)", err)
	}
	if len(reply.Body.Instances) != 1 || reply.Body.Instances[0].KID != "keeper-a" {
		t.Fatalf("Instances = %+v, want self-only [keeper-a]", reply.Body.Instances)
	}
	if !reply.Body.Instances[0].Alive {
		t.Error("self-инстанс Alive=false, want true")
	}
	// self_health отражает падение Redis.
	if reply.Body.SelfHealth["redis"] == "ok" {
		t.Errorf("SelfHealth[redis] = ok, want причину падения: %v", reply.Body.SelfHealth)
	}
}

// TestClusterGetTyped_SelfMissingFromLiveKIDs — self отсутствует в LiveKIDs (гонка
// регистрации на старте) → дописывается в список.
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
		t.Errorf("Instances = %+v, want содержит и keeper-a (self, дописан), и keeper-b", reply.Body.Instances)
	}
}

// TestClusterGetTyped_BadMeta_StartedAtOmitted — не-JSON / битая meta → инстанс в
// списке, но StartedAt=nil (fail-safe, не падение).
func TestClusterGetTyped_BadMeta_StartedAtOmitted(t *testing.T) {
	reg := &fakeClusterRegistry{
		kids: []string{"keeper-a"},
		meta: map[string]string{"keeper-a": "keeper-a"}, // голый KID (fail-safe RegisterInstance), не JSON
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
		t.Errorf("StartedAt = %v, want nil (не-JSON meta → опущен)", reply.Body.Instances[0].StartedAt)
	}
	if !reply.Body.Instances[0].Alive {
		t.Error("Alive=false при битой meta, want true (meta не влияет на alive)")
	}
}
