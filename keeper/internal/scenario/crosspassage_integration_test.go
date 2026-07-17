//go:build integration

// Cross-passage requisite-gating guard tests (ADR-056 R3, formerly
// R2-reject). The onchanges/onfail source (register A) lives in an EARLIER
// Passage than the consumer (which moved to Passage>0 via a register
// dependency where:register.X on the probe). Single-Passage Soul gating
// can't see result A → Keeper resolves the link per-host from accumulated
// CHANGED/FAILED facts of previous Passages (auditpg.SelectChangedTaskKeys /
// SelectFailedTaskKeys), crosspassage.go.
//
// CHANGED-set semantics (★): a source only "satisfies" onchanges when CHANGED.
// A skipped/ok source is NOT changed (the register row exists, but it's not
// in the CHANGED set). The dispatcher below writes task.executed audit
// exactly the way the Soul handler does (events_taskevent.go::handleTaskEvent
// → BuildTaskExecutedPayload), so the gate reads a real CHANGED/FAILED fact.
//
// R2-reject is LIFTED: cross-passage is no longer rejected (formerly
// TestIntegration_CrossPassageRequisite_Rejected → FIRES/SKIPS below).

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// crossPassageServiceRepo builds a service repo with a cross-passage
// onchanges scenario:
//
//	#0 Probe role     → Passage 0, register: role (per-host primary/replica)
//	#1 Apply config   → Passage 0, register: cfg  (per-host changed/ok)
//	#2 Restart        → Passage 1, where: register.role=='primary' + onchanges:[cfg]
//
// #2 moved to Passage 1 via a register dependency where:register.role (probe
// #0), but its onchanges source cfg (#1) stayed in Passage 0 → cross-passage onchanges.
func crossPassageServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: cross-passage onchanges service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/crosspassage/main.yml", `name: crosspassage
description: probe role + config (passage 0); restart where primary onchanges cfg (passage 1)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: detect-role }
  - name: Apply config
    module: core.exec.run
    register: cfg
    params: { cmd: write-config }
  - name: Restart on primary after config change
    module: core.exec.run
    where: "register.role.stdout == 'primary'"
    onchanges: [cfg]
    params: { cmd: restart }
`)
	commitRepo(t, repo)
	return "file://" + dir
}

func commitRepo(t *testing.T, repo *git.Repository) {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init cross-passage service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// crossPassageDispatcher simulates Soul under a cross-passage run. In Passage 0:
//   - probe `role` — writes a register (per-host roleBySID) for where: resolution;
//   - source `cfg` — writes a register AND emits task.executed audit with
//     status CHANGED (cfgChangedOn[sid]=true) or OK (otherwise) — exactly the
//     way the Soul handler does, so the cross-passage gate reads a real
//     CHANGED fact.
//
// Passage>0 — records targeting + captures consumer tasks' onchanges_idx (to
// verify keeper stripped the cross-passage idx off the wire). Terminates
// rows as success.
type crossPassageDispatcher struct {
	t            *testing.T
	roleBySID    map[string]string
	cfgChangedOn map[string]bool // sid → cfg finished CHANGED (otherwise OK)

	mu                sync.Mutex
	targetedByPassage map[int][]string
	onchangesByHost   map[string]map[string][]int32 // sid → task-name → onchanges_idx
}

func newCrossPassageDispatcher(t *testing.T, roleBySID map[string]string, cfgChangedOn map[string]bool) *crossPassageDispatcher {
	return &crossPassageDispatcher{
		t: t, roleBySID: roleBySID, cfgChangedOn: cfgChangedOn,
		targetedByPassage: map[int][]string{},
		onchangesByHost:   map[string]map[string][]int32{},
	}
}

func (d *crossPassageDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()

	d.mu.Lock()
	d.targetedByPassage[passage] = append(d.targetedByPassage[passage], sid)
	m := map[string][]int32{}
	for _, task := range req.GetTasks() {
		m[task.GetName()] = task.GetOnchangesIdx()
	}
	d.onchangesByHost[sid] = m
	d.mu.Unlock()

	if passage == 0 {
		// probe role — register for where: resolution.
		if localIdx, planIdx := emitterIndices(req, "role"); localIdx >= 0 {
			d.upsert(ctx, applyID, sid, planIdx, localIdx, map[string]any{"stdout": d.roleBySID[sid], "changed": false, "failed": false})
		}
		// source cfg — register + task.executed audit with status CHANGED/OK.
		if localIdx, planIdx := emitterIndices(req, "cfg"); localIdx >= 0 {
			changed := d.cfgChangedOn[sid]
			d.upsert(ctx, applyID, sid, planIdx, localIdx, map[string]any{"stdout": "cfg", "changed": changed, "failed": false})
			status := keeperv1.TaskStatus_TASK_STATUS_OK
			if changed {
				status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
			}
			d.emitTaskExecuted(ctx, applyID, sid, planIdx, localIdx, 0, status)
		}
	}

	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("crossPassageDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *crossPassageDispatcher) upsert(ctx context.Context, applyID, sid string, planIdx, localIdx int, data map[string]any) {
	if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
		ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx, RegisterData: data, Passage: 0,
	}); err != nil {
		d.t.Errorf("crossPassageDispatcher: UpsertTaskRegister: %v", err)
	}
}

// emitTaskExecuted writes task.executed audit exactly the way the Soul
// handler does (events_taskevent.go::handleTaskEvent): correlation_id=apply_id,
// payload via BuildTaskExecutedPayload (carries plan_index/status/passage) —
// the cross-passage gate reads it via
// SelectChangedTaskKeys/SelectFailedTaskKeys.
func (d *crossPassageDispatcher) emitTaskExecuted(ctx context.Context, applyID, sid string, planIdx, localIdx, passage int, status keeperv1.TaskStatus) {
	payload := audit.BuildTaskExecutedPayload(audit.TaskExecutedInput{
		SID: sid, ApplyID: applyID, TaskIdx: localIdx, PlanIndex: planIdx,
		Status: status.String(), Passage: passage,
	})
	if err := auditpg.NewWriter(integrationPool).Write(ctx, &audit.Event{
		EventType:     audit.EventTaskExecuted,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: applyID,
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		d.t.Errorf("crossPassageDispatcher: audit Write: %v", err)
	}
}

func (d *crossPassageDispatcher) targets(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.targetedByPassage[passage]...)
	return out
}

func (d *crossPassageDispatcher) onchanges(sid, taskName string) ([]int32, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.onchangesByHost[sid]
	if !ok {
		return nil, false
	}
	idx, ok := m[taskName]
	return idx, ok
}

// TestIntegration_CrossPassageOnChanges_Fires — ★ R3 FIRES. cfg CHANGED in
// Passage 0 on both primary hosts → consumer restart (onchanges:[cfg]
// cross-passage, Passage 1) RUNS on hosts where cfg changed AND
// where:role=='primary'. Keeper resolved cross-passage onchanges → restart
// goes on the wire with empty onchanges_idx (unconditional).
func TestIntegration_CrossPassageOnChanges_Fires(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := crossPassageServiceRepo(t)

	disp := newCrossPassageDispatcher(t,
		map[string]string{"host-a.example.com": "primary", "host-b.example.com": "primary"},
		map[string]bool{"host-a.example.com": true, "host-b.example.com": true}) // cfg changed on both
	r := newRunnerWithAuditStaged(t, disp)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "crosspassage",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ Passage 1: restart WENT OUT to both primary hosts (cfg changed → onchanges fired).
	p1 := disp.targets(1)
	if len(p1) != 2 {
		t.Fatalf("* Passage 1 targets = %v, want both hosts (cfg changed cross-passage -> onchanges FIRES)", p1)
	}
	// restart's onchanges_idx on the wire is EMPTY: keeper resolved
	// cross-passage → stripped the idx, Soul runs unconditionally (no
	// same-passage onchanges sources).
	for _, sid := range p1 {
		idx, ok := disp.onchanges(sid, "Restart on primary after config change")
		if !ok {
			t.Errorf("%s: restart did not arrive in the Passage-1 ApplyRequest", sid)
			continue
		}
		if len(idx) != 0 {
			t.Errorf("%s: restart onchanges_idx = %v, want [] (keeper resolved cross-passage -> removed idx, Soul runs unconditionally)", sid, idx)
		}
	}
}

// TestIntegration_CrossPassageOnChanges_Skips — ★ R3 SKIPS. cfg NOT changed (OK)
// in Passage 0 → consumer restart (onchanges:[cfg] cross-passage) is EXCLUDED
// from Passage 1 on those hosts (onchanges didn't fire, no same-passage
// source). Doesn't misfire the other way: restart does NOT run.
func TestIntegration_CrossPassageOnChanges_Skips(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := crossPassageServiceRepo(t)

	disp := newCrossPassageDispatcher(t,
		map[string]string{"host-a.example.com": "primary", "host-b.example.com": "primary"},
		map[string]bool{}) // cfg OK on both (not changed)
	r := newRunnerWithAuditStaged(t, disp)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "crosspassage",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ Passage 1: restart went out to NO host (cfg not changed → onchanges
	// didn't fire → consumer excluded). Doesn't misfire.
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("* Passage 1 targets = %v, want [] (cfg did NOT change cross-passage -> onchanges SKIPS)", p1)
	}
}

// TestIntegration_CrossPassageOnChanges_PerHostDivergent — ★ R3 PER-HOST. cfg
// CHANGED on host-a, OK on host-b (both primary per probe). restart
// (onchanges:[cfg] cross-passage) runs ONLY on host-a. Per-host resolution
// of the cross-passage fact.
func TestIntegration_CrossPassageOnChanges_PerHostDivergent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := crossPassageServiceRepo(t)

	disp := newCrossPassageDispatcher(t,
		map[string]string{"host-a.example.com": "primary", "host-b.example.com": "primary"},
		map[string]bool{"host-a.example.com": true}) // cfg changed ONLY on host-a
	r := newRunnerWithAuditStaged(t, disp)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "crosspassage",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ Passage 1: restart ONLY on host-a (cfg changed only there).
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("* Passage 1 targets = %v, want [host-a.example.com] (cfg changed only on host-a -> per-host)", p1)
	}
}

// TestIntegration_CrossPassageOnChanges_WhereAndChanged — composite: cfg
// changed on both, but where:role=='primary' passes only host-a (host-b is
// replica). restart goes out ONLY to host-a: where filtered out host-b in
// Passage 1 BEFORE the gate, and on host-a cross-passage onchanges fired.
// Proves the cross-passage gate layers on top of where targeting, not
// instead of it.
func TestIntegration_CrossPassageOnChanges_WhereAndChanged(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := crossPassageServiceRepo(t)

	disp := newCrossPassageDispatcher(t,
		map[string]string{"host-a.example.com": "primary", "host-b.example.com": "replica"},
		map[string]bool{"host-a.example.com": true, "host-b.example.com": true}) // cfg changed on both
	r := newRunnerWithAuditStaged(t, disp)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "crosspassage",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ host-b filtered out by where (replica) BEFORE the gate; host-a passed where AND cfg changed.
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("★ Passage 1 targets = %v, want [host-a.example.com] (where:primary + cfg changed)", p1)
	}
}
