//go:build integration

// Integration guard tests for the onchanges/onfail index remap (ADR-056
// amend, R1). Reuses the integration_test.go / staged_integration_test.go
// harness (TestMain / seed* / newRunner / waitRunDone / writeServiceRepo).
// Proves:
//   - R1 ★ N=1+where reverse guard: a source task filtered out by where: on
//     one host doesn't "misfire" a consumer's onchanges index on THAT host
//     (sentinel), while on a host where the source is present, the index is
//     remapped to its LOCAL position. This was a LATENT bug OUTSIDE staged
//     (N=1, where on a stable fact).
//
// Cross-passage requisite gating (R3, formerly R2-reject) is in crosspassage_integration_test.go.

package scenario

import (
	"context"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// onchangesCaptureDispatcher simulates Soul and captures each ApplyRequest
// task's per-host onchanges_idx (sid → task-name → onchanges_idx). Terminates
// the row success (like mockDispatcher). Lets us verify that the onchanges
// index remap in ToProtoTasks produced the source's LOCAL position in THIS
// host's slice (not the global Index, which Soul would key into a
// registerByIdx miss).
type onchangesCaptureDispatcher struct {
	t  *testing.T
	mu sync.Mutex
	// byHost[sid][taskName] = this task's onchanges_idx in the host's ApplyRequest.
	byHost map[string]map[string][]int32
}

func newOnchangesCaptureDispatcher(t *testing.T) *onchangesCaptureDispatcher {
	return &onchangesCaptureDispatcher{t: t, byHost: map[string]map[string][]int32{}}
}

func (d *onchangesCaptureDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	m := map[string][]int32{}
	for _, task := range req.GetTasks() {
		m[task.GetName()] = task.GetOnchangesIdx()
	}
	d.byHost[sid] = m
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("onchangesCaptureDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

func (d *onchangesCaptureDispatcher) onchanges(sid, taskName string) ([]int32, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.byHost[sid]
	if !ok {
		return nil, false
	}
	idx, ok := m[taskName]
	return idx, ok
}

// TestIntegration_RemapOnChanges_N1Where_NoMisfire — ★ R1 N=1+where REVERSE
// GUARD (ADR-056 amend). LATENT bug OUTSIDE staged: plan [config-change
// (where: only host-a, on the stable fact soulprint.self.sid — N=1, NO
// register dependency), restart onchanges:[config-change] (on both hosts)].
// On host-a the slice = [config-change(local 0), restart(local 1)] →
// onchanges_idx restart = [0] (source's local position). On host-b
// config-change is filtered out → slice = [restart(local 0)] → onchanges_idx
// restart = [-1] (sentinel: source absent, Soul treats it as changed=false →
// restart does NOT misfire). Reverse (WITHOUT remap): on host-b onchanges_idx
// = the source's global Index (0) → Soul registerByIdx[0] = restart ITSELF → misfire.
func TestIntegration_RemapOnChanges_N1Where_NoMisfire(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})

	// where: on a stable fact (soulprint.self.sid) — NOT register → plan N=1
	// (one Passage), but config-change targets per-host ONLY host-a.
	const scn = `name: create
description: n=1 per-host where requisite remap fixture
state_changes: {}
tasks:
  - name: config-change
    module: core.exec.run
    register: cfg
    where: "soulprint.self.sid == 'host-a.example.com'"
    params: { cmd: write-config }
    changed_when: "false"
  - name: restart
    module: core.exec.run
    onchanges: [cfg]
    params: { cmd: restart }
    changed_when: "false"
`
	gitURL := writeServiceRepo(t, scn)

	disp := newOnchangesCaptureDispatcher(t)
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// host-a: config-change is present (local 0), restart (local 1). onchanges_idx
	// restart = [0] — the source's LOCAL position in host-a's slice.
	if idxA, ok := disp.onchanges("host-a.example.com", "restart"); !ok {
		t.Fatalf("host-a: restart не пришёл в ApplyRequest")
	} else if len(idxA) != 1 || idxA[0] != 0 {
		t.Fatalf("host-a: restart onchanges_idx = %v, want [0] (источник config-change на локальной позиции 0)", idxA)
	}

	// ★ host-b: config-change is FILTERED OUT by where: → the slice has ONLY
	// restart (local 0). onchanges_idx restart = [-1] (sentinel for an absent
	// source). Reverse without remap would give [0] → Soul
	// registerByIdx[0]=restart itself → false gating (misfire).
	idxB, ok := disp.onchanges("host-b.example.com", "restart")
	if !ok {
		t.Fatalf("host-b: restart не пришёл в ApplyRequest (должен — onchanges не режет таргет)")
	}
	if len(idxB) != 1 || idxB[0] != -1 {
		t.Fatalf("★ host-b: restart onchanges_idx = %v, want [-1] (источник config-change отфильтрован where → sentinel; реверс без remap дал бы [0] = глобальный Index → registerByIdx-промах → restart мисфайрит)", idxB)
	}

	// host-b must NOT carry the config-change task at all (where filtered it out).
	if _, present := disp.onchanges("host-b.example.com", "config-change"); present {
		t.Errorf("host-b: config-change не должен был попасть в срез (where: только host-a)")
	}
}
