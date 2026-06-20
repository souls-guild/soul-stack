//go:build integration

// Cross-passage requisite-gating guard-тесты (ADR-056 R3, бывший R2-reject).
// Источник onchanges/onfail (register A) лежит в БОЛЕЕ РАННЕМ Passage, чем
// потребитель (consumer уехал в Passage>0 register-зависимостью where:register.X
// от probe). Soul gating одного Passage результат A не видит → связь резолвит
// Keeper per-host по накопленным CHANGED/FAILED-фактам предыдущих Passage
// (auditpg.SelectChangedTaskKeys / SelectFailedTaskKeys), crosspassage.go.
//
// CHANGED-set-семантика (★): источник «спас» onchanges ТОЛЬКО при CHANGED.
// skipped/ok-источник = НЕ changed (register-строка существует, но в CHANGED-set
// его нет). Дисп. ниже пишет task.executed-аудит ровно тем путём, что Soul-handler
// (events_taskevent.go::handleTaskEvent → BuildTaskExecutedPayload), чтобы гейт
// читал реальный CHANGED/FAILED-факт.
//
// R2-reject СНЯТ: cross-passage больше НЕ отвергается (бывший
// TestIntegration_CrossPassageRequisite_Rejected → FIRES/SKIPS ниже).

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

// crossPassageServiceRepo создаёт service-репо с cross-passage onchanges-сценарием:
//
//	#0 Probe role     → Passage 0, register: role (per-host primary/replica)
//	#1 Apply config   → Passage 0, register: cfg  (per-host changed/ok)
//	#2 Restart        → Passage 1, where: register.role=='primary' + onchanges:[cfg]
//
// #2 уехал в Passage 1 register-зависимостью where:register.role (probe #0), но его
// onchanges-источник cfg (#1) остался в Passage 0 → cross-passage onchanges.
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

// crossPassageDispatcher симулирует Soul под cross-passage-прогоном. В Passage 0:
//   - probe `role` — пишет register (per-host roleBySID) для where:-резолва;
//   - source `cfg` — пишет register И эмитит task.executed-аудит со статусом
//     CHANGED (cfgChangedOn[sid]=true) либо OK (иначе) — ровно тем путём, что Soul-
//     handler, чтобы cross-passage-гейт прочитал реальный CHANGED-факт.
//
// Passage>0 — фиксирует targeting + захватывает onchanges_idx consumer-задач (для
// проверки, что keeper убрал cross-passage idx с wire). Терминалит строки success.
type crossPassageDispatcher struct {
	t            *testing.T
	roleBySID    map[string]string
	cfgChangedOn map[string]bool // sid → cfg завершился CHANGED (иначе OK)

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
		// probe role — register для where:-резолва.
		if localIdx, planIdx := emitterIndices(req, "role"); localIdx >= 0 {
			d.upsert(ctx, applyID, sid, planIdx, localIdx, map[string]any{"stdout": d.roleBySID[sid], "changed": false, "failed": false})
		}
		// source cfg — register + task.executed-аудит со статусом CHANGED/OK.
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

// emitTaskExecuted пишет task.executed-аудит ровно тем путём, что Soul-handler
// (events_taskevent.go::handleTaskEvent): correlation_id=apply_id, payload через
// BuildTaskExecutedPayload (несёт plan_index/status/passage) — cross-passage-гейт
// читает его через SelectChangedTaskKeys/SelectFailedTaskKeys.
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

// TestIntegration_CrossPassageOnChanges_Fires — ★ R3 FIRES. cfg CHANGED в Passage 0
// на обоих primary-хостах → consumer restart (onchanges:[cfg] cross-passage, Passage
// 1) ВЫПОЛНЯЕТСЯ на хостах, где cfg changed И where:role=='primary'. Keeper резолвил
// cross-passage onchanges → restart на wire с пустым onchanges_idx (безусловно).
func TestIntegration_CrossPassageOnChanges_Fires(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := crossPassageServiceRepo(t)

	disp := newCrossPassageDispatcher(t,
		map[string]string{"host-a.example.com": "primary", "host-b.example.com": "primary"},
		map[string]bool{"host-a.example.com": true, "host-b.example.com": true}) // cfg changed на обоих
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

	// ★ Passage 1: restart УШЁЛ на оба primary-хоста (cfg changed → onchanges сработал).
	p1 := disp.targets(1)
	if len(p1) != 2 {
		t.Fatalf("★ Passage 1 targets = %v, want оба хоста (cfg changed cross-passage → onchanges FIRES)", p1)
	}
	// onchanges_idx restart на wire ПУСТ: keeper резолвил cross-passage → убрал idx,
	// Soul выполняет безусловно (нет same-passage onchanges-источников).
	for _, sid := range p1 {
		idx, ok := disp.onchanges(sid, "Restart on primary after config change")
		if !ok {
			t.Errorf("%s: restart не пришёл в Passage-1 ApplyRequest", sid)
			continue
		}
		if len(idx) != 0 {
			t.Errorf("%s: restart onchanges_idx = %v, want [] (keeper резолвил cross-passage → убрал idx, Soul безусловно)", sid, idx)
		}
	}
}

// TestIntegration_CrossPassageOnChanges_Skips — ★ R3 SKIPS. cfg НЕ changed (OK) в
// Passage 0 → consumer restart (onchanges:[cfg] cross-passage) ИСКЛЮЧАЕТСЯ из
// Passage 1 на этих хостах (onchanges не сработал, нет same-passage источника).
// Не мисфайрит обратно: restart НЕ выполняется.
func TestIntegration_CrossPassageOnChanges_Skips(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := crossPassageServiceRepo(t)

	disp := newCrossPassageDispatcher(t,
		map[string]string{"host-a.example.com": "primary", "host-b.example.com": "primary"},
		map[string]bool{}) // cfg OK на обоих (не changed)
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

	// ★ Passage 1: restart НЕ ушёл НИ ОДНОМУ хосту (cfg не changed → onchanges не
	// сработал → consumer исключён). Не мисфайрит.
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("★ Passage 1 targets = %v, want [] (cfg НЕ changed cross-passage → onchanges SKIPS)", p1)
	}
}

// TestIntegration_CrossPassageOnChanges_PerHostDivergent — ★ R3 PER-HOST. cfg
// CHANGED на host-a, OK на host-b (оба primary по probe). restart (onchanges:[cfg]
// cross-passage) выполняется ТОЛЬКО на host-a. Per-host резолв cross-passage факта.
func TestIntegration_CrossPassageOnChanges_PerHostDivergent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := crossPassageServiceRepo(t)

	disp := newCrossPassageDispatcher(t,
		map[string]string{"host-a.example.com": "primary", "host-b.example.com": "primary"},
		map[string]bool{"host-a.example.com": true}) // cfg changed ТОЛЬКО на host-a
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

	// ★ Passage 1: restart ТОЛЬКО на host-a (cfg changed только там).
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("★ Passage 1 targets = %v, want [host-a.example.com] (cfg changed только на host-a → per-host)", p1)
	}
}

// TestIntegration_CrossPassageOnChanges_WhereAndChanged — composите: cfg changed на
// обоих, но where:role=='primary' пропускает только host-a (host-b replica). restart
// уходит ТОЛЬКО на host-a: where отфильтровал host-b в Passage 1 ДО гейта, на host-a
// cross-passage onchanges сработал. Доказывает, что cross-passage-гейт работает
// поверх where-таргетинга, не вместо него.
func TestIntegration_CrossPassageOnChanges_WhereAndChanged(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := crossPassageServiceRepo(t)

	disp := newCrossPassageDispatcher(t,
		map[string]string{"host-a.example.com": "primary", "host-b.example.com": "replica"},
		map[string]bool{"host-a.example.com": true, "host-b.example.com": true}) // cfg changed на обоих
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

	// ★ host-b отфильтрован where (replica) ДО гейта; host-a прошёл where И cfg changed.
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("★ Passage 1 targets = %v, want [host-a.example.com] (where:primary + cfg changed)", p1)
	}
}
