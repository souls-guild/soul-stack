//go:build integration

// 2-passage contract proof для staged-render (ADR-056, S3). Минимальный staged-
// сценарий probe→where на контракт-тире: Passage 0 — probe роли (register: role),
// эмитящий per-host факт ('master' на одном хосте, 'slave' на другом); Passage 1 —
// действие `where: register.role.stdout == 'master'`. ASSERT: Passage-1
// ApplyRequest уходит ТОЛЬКО на master-хост (where резолвнулся register-ом из
// Passage 0). Это доказывает staged-render end-to-end на контракт-тире —
// закрытие drift-а «register в where всегда пуст» (ADR-056 §Контекст).
//
// Полный live redis-cluster (cloud/vault-scope) — НЕ здесь (S4/S5). Soul
// симулируется stagedDispatcher-ом (тем же путём, что mockDispatcher: SendApply →
// per-task register + терминальный apply_runs-статус, как accumulateRegister/
// correlateRunResult в проде).

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// stagedServiceRepo создаёт service-репо со staged-сценарием `failover`:
// Passage 0 — probe роли (register: role), Passage 1 — действие на master.
func stagedServiceRepo(t *testing.T) string {
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
description: staged-render proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/failover/main.yml", `name: failover
description: probe role (passage 0) then act on master (passage 1)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Act on master only
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    params:
      cmd: promote
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init staged service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// stagedDispatcher симулирует Soul под staged-прогоном (контракт-тир L3a):
//   - Passage 0 (probe): для каждого хоста пишет register `role` с per-host
//     stdout (roleBySID) под (apply_id, sid, passage=0) — как accumulateRegister
//     на TaskEvent.register_data в проде; затем терминалит строку passage=0
//     success — как correlateRunResult на RunResult.passage=0.
//   - Passage>0 (действие): терминалит строку этого Passage success И фиксирует
//     SID-ы, на которые пришёл Passage-N ApplyRequest (targetedByPassage) — это
//     и есть доказательство targeting-а.
//
// passage берётся из req.Passage (эхо ApplyRequest.passage) — тем же контрактом,
// что Soul эхает passage в TaskEvent/RunResult.
//
// failPassage0On — SID-ы, на которых probe (passage 0) завершается FAILED (вместо
// success): probe-fail терминалит строку passage-0 этого хоста в `failed` (как
// RunResult.status=FAILED в проде). barrier passage-0 fail-closed валит ВЕСЬ
// прогон → passage-1 не диспатчится. Для них register НЕ пишется (probe упал).
type stagedDispatcher struct {
	t              *testing.T
	roleBySID      map[string]string // sid → probe-stdout (per-host probe-результат)
	failPassage0On map[string]bool   // sid → probe (passage 0) завершается FAILED

	mu                sync.Mutex
	targetedByPassage map[int][]string // passage → SID-ы, получившие ApplyRequest
	// dispatchedPlan — все (passage, plan_index, name) из req.Tasks[] каждого
	// ApplyRequest = ровно то, что Soul эхнул бы в TaskEvent.plan_index → audit
	// task.executed. Ground-truth «реально исполненного плана» для guard-а H1
	// (NIM-37): сверяется с apply_run_plan (persistRunPlan). Дедуп по plan_index
	// делает reader (dispatchedPlanByIndex) — passage-1 диспатчится на N хостов.
	dispatchedPlan []dispatchedTask
}

// dispatchedTask — одна отрендеренная задача, реально ушедшая в ApplyRequest
// (эхо plan_index/name активного render'а Passage).
type dispatchedTask struct {
	passage   int
	planIndex int
	name      string
}

func newStagedDispatcher(t *testing.T, roleBySID map[string]string) *stagedDispatcher {
	return &stagedDispatcher{t: t, roleBySID: roleBySID, targetedByPassage: map[int][]string{}}
}

// dispatchedPlanByIndex сворачивает dispatchedPlan в plan_index → (passage, name),
// дедуплицируя одинаковые задачи, ушедшие на несколько хостов одного Passage.
func (d *stagedDispatcher) dispatchedPlanByIndex() map[int]dispatchedTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[int]dispatchedTask, len(d.dispatchedPlan))
	for _, dt := range d.dispatchedPlan {
		out[dt.planIndex] = dt
	}
	return out
}

// emitterIndices воспроизводит контракт Soul-а (ADR-056 §S1 fix Variant B): для
// register-задачи с именем register находит её ЛОКАЛЬНУЮ позицию в req.Tasks[]
// (= TaskEvent.task_idx, локален для Passage/host) и её ГЛОБАЛЬНЫЙ plan_index
// (= req.Tasks[i].plan_index, эхо TaskEvent.plan_index). Симулирует ровно то, что
// делает sendTaskEvent на Soul-е: task_idx = idx цикла, plan_index = task.GetPlanIndex().
// register-корреляция на Keeper-е ключуется по plan_index — поэтому harness обязан
// различать локальный и глобальный индексы (иначе он, как раньше, маскировал бы баг).
func emitterIndices(req *keeperv1.ApplyRequest, register string) (localIdx, planIdx int) {
	for i, task := range req.GetTasks() {
		if task.GetRegister() == register {
			return i, int(task.GetPlanIndex())
		}
	}
	return -1, -1
}

func (d *stagedDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()

	d.mu.Lock()
	d.targetedByPassage[passage] = append(d.targetedByPassage[passage], sid)
	for _, task := range req.GetTasks() {
		d.dispatchedPlan = append(d.dispatchedPlan, dispatchedTask{
			passage:   passage,
			planIndex: int(task.GetPlanIndex()),
			name:      task.GetName(),
		})
	}
	d.mu.Unlock()

	if passage == 0 && d.failPassage0On[sid] {
		// probe упал на этом хосте: терминалим passage-0 строку в failed (как
		// RunResult.status=FAILED). register НЕ пишем — probe не дал факта.
		summary := "probe failed"
		if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, 0, applyrun.StatusFailed, &summary); err != nil {
			d.t.Errorf("stagedDispatcher: UpdateStatus(%s, passage=0, failed): %v", sid, err)
		}
		return nil
	}

	if passage == 0 {
		// probe-задача `role`. task_idx — ЛОКАЛЬНАЯ позиция в req.Tasks[] (как Soul
		// эмитит TaskEvent.task_idx), plan_index — ГЛОБАЛЬНЫЙ эхо req.Tasks[i].plan_index
		// (как Soul эхает TaskEvent.plan_index, ADR-056 §S1 fix Variant B). Register
		// ключуется по plan_index — это и есть исправленный путь корреляции.
		role := d.roleBySID[sid]
		localIdx, planIdx := emitterIndices(req, "role")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID:      applyID,
			SID:          sid,
			PlanIndex:    planIdx,
			TaskIdx:      localIdx,
			RegisterData: map[string]any{"stdout": role, "changed": false, "failed": false},
			Passage:      0,
		}); err != nil {
			d.t.Errorf("stagedDispatcher: UpsertTaskRegister(%s): %v", sid, err)
		}
	}

	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("stagedDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *stagedDispatcher) targets(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.targetedByPassage[passage]...)
	return out
}

// TestIntegration_2Passage_WhereTargetsOnlyMaster — ★ 2-PASSAGE PROOF (ADR-056, S3).
// Два хоста: host-a — master, host-b — slave (per-host probe в Passage 0). Passage
// 1 несёт `where: register.role.stdout == 'master'`. ASSERT: Passage-1 ApplyRequest
// затаргетился ТОЛЬКО на host-a (master) — where резолвнулся register-ом из
// Passage 0 end-to-end через stage-loop (render→dispatch→barrier→register→render).
func TestIntegration_2Passage_WhereTargetsOnlyMaster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Прогон успешен (оба Passage прошли барьеры, state-commit → ready).
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0 (probe): ApplyRequest на ОБА хоста (probe без where → весь roster).
	p0 := disp.targets(0)
	if len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want оба хоста", p0)
	}

	// ★ Passage 1 (действие): ApplyRequest ТОЛЬКО на master-хост. where:
	// register.role.stdout == 'master' резолвнулся per-host register-ом Passage 0.
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("★ Passage 1 targets = %v, want [host-a.example.com] (only master) — staged where не резолвнулся register-ом Passage 0", p1)
	}

	// apply_runs: per-host × per-passage. host-a: passage 0 + passage 1 (master).
	// host-b: passage 0 ТОЛЬКО (slave — Passage 1 на него не таргетился, строки нет).
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	got := map[string][]int{}
	for _, st := range statuses {
		if st.Status != applyrun.StatusSuccess {
			t.Errorf("apply_runs[%s,passage=%d] = %s, want success", st.SID, st.Passage, st.Status)
		}
		got[st.SID] = append(got[st.SID], st.Passage)
	}
	if len(got["host-a.example.com"]) != 2 {
		t.Errorf("host-a passages = %v, want [0 1] (probe + master-action)", got["host-a.example.com"])
	}
	if len(got["host-b.example.com"]) != 1 || got["host-b.example.com"][0] != 0 {
		t.Errorf("host-b passages = %v, want [0] (probe только — slave не таргетился Passage 1)", got["host-b.example.com"])
	}
}

// stagedExpandingServiceRepo — staged-сценарий, где задача Passage 1 РАСКРЫВАЕТСЯ
// (loop с 2 items) в N>1 RenderedTask с реальными plan_index. На render шага 5
// (ActivePassage=0) она — ОДИН сжатый placeholder (индекс 1); на пере-рендере
// Passage 1 (ActivePassage=1) — 2 задачи (индексы 1,2). Ровно кейс H1 (NIM-37):
// персист плана обязан снять passage-1 из ЕГО активного render, а не из placeholder-а
// шага 5. where: register.role делает задачу register-зависимой → Passage 1 (Stratify).
func stagedExpandingServiceRepo(t *testing.T) string {
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
description: staged-render expanding-passage proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/expand/main.yml", `name: expand
description: probe role (p0) then fan-out loop on live hosts (p1 expands to N tasks)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Fan out
    module: core.exec.run
    where: "register.role.stdout != ''"
    loop:
      items: "${ ['alpha', 'beta'] }"
      as: item
    params:
      cmd: "configure ${ item }"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init staged expanding service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_StagedExpandingPassage_RunPlanMatchesExecution — ★ GUARD H1
// (NIM-37): при staged-прогоне, где задача Passage 1 раскрывается (loop 2 items),
// persistRunPlan обязан снять apply_run_plan passage-1 из ЕГО активного render
// (реальные plan_index 1,2), а НЕ из сжатого placeholder-а render шага 5 (один
// индекс 1). ИНВАРИАНТ: множество plan_index в apply_run_plan (+name/passage) ==
// множество plan_index, реально ушедшее в ApplyRequest (эхо TaskEvent.plan_index →
// audit task.executed). Без фикса passage-1 персистится placeholder-ом (index 1),
// apply_run_plan теряет index 2 → ассерт падает.
func TestIntegration_StagedExpandingPassage_RunPlanMatchesExecution(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedExpandingServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "expand",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Ground-truth: что реально ушло в ApplyRequest (эхо plan_index активного
	// render'а каждого Passage) = что Soul записал бы в audit task.executed.
	// probe idx0 (P0) + loop idx1,2 (P1) = 3 уникальных plan_index.
	exec := disp.dispatchedPlanByIndex()
	if len(exec) != 3 {
		t.Fatalf("dispatched plan_index set = %v, want 3 (probe idx0 + loop idx1,2 раскрытого Passage 1)", exec)
	}

	// apply_run_plan (persistRunPlan) обязан совпасть с исполнением по множеству
	// plan_index и по name/passage на каждый индекс.
	plan, err := applyrun.SelectRunPlanByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectRunPlanByApplyID: %v", err)
	}
	planByIndex := map[int]applyrun.RunPlanTask{}
	for _, p := range plan {
		planByIndex[p.PlanIndex] = p
	}
	if len(planByIndex) != len(exec) {
		var planIdx, execIdx []int
		for k := range planByIndex {
			planIdx = append(planIdx, k)
		}
		for k := range exec {
			execIdx = append(execIdx, k)
		}
		sort.Ints(planIdx)
		sort.Ints(execIdx)
		t.Fatalf("★ H1: apply_run_plan plan_index = %v, want == исполнение %v (staged passage-1 снят placeholder-ом вместо активного render'а — index потерян)", planIdx, execIdx)
	}
	for idx, dt := range exec {
		p, ok := planByIndex[idx]
		if !ok {
			t.Fatalf("★ H1: plan_index %d исполнен (passage %d, %q), но отсутствует в apply_run_plan — persistRunPlan снял сжатый placeholder вместо раскрытого render'а Passage 1", idx, dt.passage, dt.name)
		}
		if p.Passage != dt.passage || p.Name != dt.name {
			t.Errorf("plan_index %d: apply_run_plan (passage=%d, name=%q) != исполнение (passage=%d, name=%q)", idx, p.Passage, p.Name, dt.passage, dt.name)
		}
	}
}

// passagesBySID собирает apply_runs прогона в (sid → отсортированные passage-ы) +
// проверяет, что каждая строка терминальна с ожидаемым статусом wantStatus.
// Используется guard-тестами для ассерта «какие хосты получили apply_runs-строку
// какого Passage» (targeting-доказательство на стороне БД, симметрично
// disp.targets() на стороне dispatch-а).
func passagesBySID(t *testing.T, applyID string, wantStatus applyrun.Status) map[string][]int {
	t.Helper()
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	got := map[string][]int{}
	for _, st := range statuses {
		if st.Status != wantStatus {
			t.Errorf("apply_runs[%s,passage=%d] = %s, want %s", st.SID, st.Passage, st.Status, wantStatus)
		}
		got[st.SID] = append(got[st.SID], st.Passage)
	}
	return got
}

// TestIntegration_StagedAllSlave_NoOpReady — ★ TARGETING-ИНВАРИАНТ (ADR-056).
// Оба хоста — slave (probe в Passage 0 отдаёт role='slave' обоим). Passage 1
// несёт `where: register.role.stdout == 'master'` → НИ ОДИН хост не проходит
// фильтр. ASSERT: Passage-1 ApplyRequest НЕ ушёл НИ ОДНОМУ хосту (пустой
// destructive-таргет), barrier Passage-1 НЕ запускается и НЕ виснет
// (dispatchPassage: len(perHost)==0 → no-op return nil), incarnation → READY.
// Пустой destructive-таргет — безопасный no-op, не hang и не error.
func TestIntegration_StagedAllSlave_NoOpReady(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "slave",
		"host-b.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Пустой destructive-таргет Passage 1 — НЕ hang: barrier Passage-1 не
	// запускается (no-op), прогон доходит до commitSuccess → ready (если бы
	// barrier Passage-1 зависал на пустом наборе, waitRunDone упал бы по timeout).
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0 (probe): оба хоста (probe без where → весь roster).
	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want оба хоста", p0)
	}

	// ★ Passage 1: НИ ОДНОГО ApplyRequest (все slave → where=false на каждом).
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("★ Passage 1 targets = %v, want [] (all-slave: destructive-таргет пуст, no-op)", p1)
	}

	// apply_runs: у обоих хостов ТОЛЬКО passage 0 (Passage-1 строки нет — на него
	// никто не таргетился). Все строки success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		if len(got[sid]) != 1 || got[sid][0] != 0 {
			t.Errorf("%s passages = %v, want [0] (probe только — Passage 1 не таргетился)", sid, got[sid])
		}
	}
}

// TestIntegration_StagedProbeFail_FailStop — ★ LIFECYCLE-ИНВАРИАНТ (ADR-056 §г).
// probe (Passage 0) завершается FAILED на хосте. ASSERT: barrier Passage-0
// fail-closed валит прогон ДО stage-loop-перехода на Passage 1 →
// Passage-1 ApplyRequest НЕ отправлен НИ ОДНОМУ хосту → incarnation →
// ERROR_LOCKED. Провал probe-шага НЕ диспатчит зависимый Passage по неполному
// register.
func TestIntegration_StagedProbeFail_FailStop(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master", // role не важен — probe упадёт раньше register
	})
	disp.failPassage0On = map[string]bool{"host-a.example.com": true}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (barrier Passage-0 fail-stop)", inc.StatusDetails["reason"])
	}

	// Passage 0 затаргетился (probe ушёл), Passage 1 — НЕТ (stage-loop оборвался
	// на barrier Passage-0).
	if p0 := disp.targets(0); len(p0) != 1 {
		t.Errorf("Passage 0 targets = %v, want [host-a.example.com]", p0)
	}
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("★ Passage 1 targets = %v, want [] (probe-fail остановил прогон до Passage 1)", p1)
	}

	// apply_runs: единственная строка passage 0 = failed (Passage-1 строки нет).
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("apply_runs rows = %d, want 1 (только probe passage 0)", len(statuses))
	}
	if statuses[0].Passage != 0 || statuses[0].Status != applyrun.StatusFailed {
		t.Errorf("apply_runs[0] = passage %d/%s, want passage 0/failed", statuses[0].Passage, statuses[0].Status)
	}
}

// TestIntegration_StagedAllMaster — оба хоста role='master' → Passage 1 проходит
// where на ОБОИХ → оба success → ready. Контрольный «всё совпало» к all-slave:
// доказывает, что пустой Passage-1 в all-slave — следствие where-фильтра, а не
// поломки staged-loop-а вообще.
func TestIntegration_StagedAllMaster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "master",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want оба хоста", p0)
	}
	// ★ Passage 1: ОБА хоста (where master истинен на каждом).
	p1 := disp.targets(1)
	if len(p1) != 2 {
		t.Fatalf("★ Passage 1 targets = %v, want оба хоста (all-master)", p1)
	}

	// apply_runs: у обоих хостов passage 0 + passage 1, все success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		if len(got[sid]) != 2 {
			t.Errorf("%s passages = %v, want [0 1] (probe + master-action)", sid, got[sid])
		}
	}
}

// TestIntegration_StagedPartialProbeFail_FailClosed — probe упал на ОДНОМ из двух
// хостов (host-b), на другом (host-a) дал role='master'. ASSERT: barrier
// Passage-0 fail-closed валит ВЕСЬ прогон (НЕ «диспатчить Passage 1 по
// частичному register на выжившего host-a») → Passage-1 ApplyRequest НЕ
// отправлен никому → incarnation → ERROR_LOCKED.
func TestIntegration_StagedPartialProbeFail_FailClosed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master", // probe ОК — дал бы master
		"host-b.example.com": "master",
	})
	disp.failPassage0On = map[string]bool{"host-b.example.com": true} // probe упал на host-b
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (частичный probe-fail валит весь прогон)", inc.StatusDetails["reason"])
	}

	// Passage 0 ушёл на оба хоста; ★ Passage 1 — никому (fail-closed: выживший
	// host-a НЕ получает Passage-1 по частичному register).
	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want оба хоста", p0)
	}
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("★ Passage 1 targets = %v, want [] (частичный probe-fail → fail-closed, Passage 1 не диспатчится)", p1)
	}
}

// --- 3-Passage (restart re-probe) ----------------------------------------

// staged3PassageServiceRepo создаёт service-репо с 3-Passage сценарием `restart`,
// воспроизводящим каноническую restart re-probe идиому (ADR-056 §«restart re-probe»):
//
//	#0 probe role               → Passage 0 (register: role)
//	#1 act where role==master   → Passage 1 (читает register.role — после probe)
//	#2 re-probe role_after       → Passage 1 (program-order ребро: эмиттер ПОСЛЕ #1)
//	#3 act where role_after==... → Passage 2 (читает register.role_after — после re-probe)
//
// Stratify даёт TaskPassage [0,1,1,2], Count=3 (две probe-границы). Passage-2
// задача (#3) таргетится ИСКЛЮЧИТЕЛЬНО по register.role_after (re-probe Passage 1),
// а НЕ по первому probe register.role (Passage 0) — это и доказывает program-order
// ребро S2 + N-loop для N=3.
func staged3PassageServiceRepo(t *testing.T) string {
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
description: staged-render 3-passage proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/restart/main.yml", `name: restart
description: probe role (p0) → failover on master (p1) → re-probe role_after (p1) → act on new master (p2)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Failover on current master
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    params:
      cmd: failover
  - name: Re-probe role after failover
    module: core.exec.run
    register: role_after
    changed_when: "false"
    params:
      cmd: detect-role-again
  - name: Act on the new master
    module: core.exec.run
    where: "register.role_after.stdout == 'master'"
    params:
      cmd: restart-former-master
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init staged 3-passage service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// staged3Dispatcher симулирует Soul под 3-Passage restart-прогоном (контракт-тир).
// Эмиттеры register заданы как (passage, глобальный plan_index) → per-host stdout:
//   - probe `role`        — passage 0, plan_index 0 (roleP0BySID);
//   - re-probe `role_after`— passage 1, plan_index 2 / ЛОКАЛЬНЫЙ task_idx 1 (roleP1BySID).
//
// Индексы берутся из req.Tasks[] через emitterIndices (как Soul эхает их в
// TaskEvent), а НЕ хардкодятся — иначе harness замаскировал бы баг task_idx-коллизии.
//
// КЛЮЧЕВОЕ для доказательства: roleP1BySID отличается от roleP0BySID — после
// failover master меняется. Если бы Passage-2 задача (#3) таргетилась по СТАРОМУ
// register.role (probe Passage 0), она ушла бы на host-a; она обязана уйти на
// host-b (master по re-probe). targetedByPassage фиксирует факт targeting-а.
type staged3Dispatcher struct {
	t           *testing.T
	roleP0BySID map[string]string // sid → probe-stdout (passage 0, task_idx 0)
	roleP1BySID map[string]string // sid → re-probe-stdout (passage 1, task_idx 2)

	mu                sync.Mutex
	targetedByPassage map[int][]string
}

func newStaged3Dispatcher(t *testing.T, p0, p1 map[string]string) *staged3Dispatcher {
	return &staged3Dispatcher{t: t, roleP0BySID: p0, roleP1BySID: p1, targetedByPassage: map[int][]string{}}
}

func (d *staged3Dispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()

	d.mu.Lock()
	d.targetedByPassage[passage] = append(d.targetedByPassage[passage], sid)
	d.mu.Unlock()

	switch passage {
	case 0:
		// probe `role` — глобальный plan_index 0, локальный task_idx 0 (один шаг
		// в Passage 0). Ключ корреляции — plan_index (ADR-056 §S1 fix Variant B).
		localIdx, planIdx := emitterIndices(req, "role")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx,
			RegisterData: map[string]any{"stdout": d.roleP0BySID[sid], "changed": false, "failed": false},
			Passage:      0,
		}); err != nil {
			d.t.Errorf("staged3Dispatcher: UpsertTaskRegister role (%s): %v", sid, err)
		}
	case 1:
		// re-probe `role_after` — ГЛОБАЛЬНЫЙ plan_index 2 (#2 в полном плане), но
		// ЛОКАЛЬНЫЙ task_idx 1 (Passage 1 несёт #1 failover на локальной 0 + #2
		// re-probe на локальной 1). Раньше harness писал TaskIdx:2 (глобальный под
		// именем локального) — это и маскировало баг. Теперь register ключуется по
		// глобальному plan_index, а локальный task_idx ≠ глобальному — реальный путь.
		localIdx, planIdx := emitterIndices(req, "role_after")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx,
			RegisterData: map[string]any{"stdout": d.roleP1BySID[sid], "changed": false, "failed": false},
			Passage:      1,
		}); err != nil {
			d.t.Errorf("staged3Dispatcher: UpsertTaskRegister role_after (%s): %v", sid, err)
		}
	}

	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("staged3Dispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *staged3Dispatcher) targets(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.targetedByPassage[passage]...)
	sort.Strings(out)
	return out
}

// TestIntegration_Staged3Passage_ReprobeRetargets — ★ 3-PASSAGE PROOF (ADR-056 §S4).
// Доказывает program-order ребро S2 + N-loop для N=3 на restart re-probe идиоме.
//
// Расклад: до failover host-a — master, host-b — slave (probe Passage 0). ПОСЛЕ
// failover (Passage 1 действие) роли меняются — re-probe Passage 1 отдаёт host-a
// slave, host-b master. Passage-2 задача `where: register.role_after.stdout ==
// 'master'` обязана таргетиться по СВЕЖЕМУ re-probe → ТОЛЬКО host-b. Если бы
// program-order ребро не работало и re-probe оказался в Passage 0 (или Passage-2
// where резолвился по первому probe), задача ушла бы на host-a (СТАРЫЙ master) —
// тест это поймает.
func TestIntegration_Staged3Passage_ReprobeRetargets(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := staged3PassageServiceRepo(t)

	disp := newStaged3Dispatcher(t,
		map[string]string{ // probe Passage 0: host-a master
			"host-a.example.com": "master",
			"host-b.example.com": "slave",
		},
		map[string]string{ // re-probe Passage 1: ПОСЛЕ failover host-b master
			"host-a.example.com": "slave",
			"host-b.example.com": "master",
		},
	)
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Три Passage (две probe-границы) прошли барьеры → ready.
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0 (probe): оба хоста (probe без where).
	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want оба хоста", p0)
	}
	// Passage 1 (failover where role==master ПЛЮС re-probe без where): re-probe
	// таргетит весь roster, поэтому ОБА хоста получают Passage-1 ApplyRequest
	// (re-probe #2 идёт на всех; failover #1 — только на старый master host-a).
	if p1 := disp.targets(1); len(p1) != 2 {
		t.Errorf("Passage 1 targets = %v, want оба хоста (re-probe без where → весь roster)", p1)
	}

	// ★ Passage 2 (act where role_after==master): ТОЛЬКО host-b — НОВЫЙ master по
	// СВЕЖЕМУ re-probe Passage 1. Старый probe Passage 0 дал бы host-a — если тест
	// видит host-a, program-order ребро/re-probe-таргетинг СЛОМАН.
	p2 := disp.targets(2)
	if len(p2) != 1 || p2[0] != "host-b.example.com" {
		t.Fatalf("★ Passage 2 targets = %v, want [host-b.example.com] (НОВЫЙ master по re-probe Passage 1) — re-probe retargeting сломан: targeting пошёл по СТАРОМУ probe Passage 0", p2)
	}

	// apply_runs: host-a — passage 0,1 (probe + failover-action + re-probe; failover
	// и re-probe оба Passage 1, одна строка на passage); host-b — passage 0,1,2
	// (probe + re-probe + new-master-action). Все success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	if len(got["host-a.example.com"]) != 2 {
		t.Errorf("host-a passages = %v, want [0 1] (probe + failover/re-probe, не таргетился Passage 2)", got["host-a.example.com"])
	}
	if len(got["host-b.example.com"]) != 3 {
		t.Errorf("host-b passages = %v, want [0 1 2] (probe + re-probe + new-master-action)", got["host-b.example.com"])
	}
}

// TestIntegration_StagedOldSoul_Rejected — ★ FORWARD-COMPAT GUARD (ADR-056 §S5).
// Staged-сценарий (probe→where, N>1 Passage) шлёт N ApplyRequest на хост; barrier
// каждого Passage ждёт RunResult с echo passage. Soul без passage-capability
// (старый бинарь) вернул бы passage=0 на все Passage → barrier Passage 1 ждал бы
// терминал, которого нет → ЗАВИСАНИЕ в applying. Гейт run.go ОБЯЗАН отвергнуть
// прогон ДО dispatch, если ХОТЬ ОДИН таргет-хост не анонсировал passage. ASSERT:
// incarnation → ERROR_LOCKED, reason = soul_passage_unsupported, НИ ОДНОГО
// ApplyRequest (отказ fail-closed ДО dispatch, не hang). Симметрия с
// StagedNilPassageCap_FailClosed (тоже fail-closed ДО dispatch).
func TestIntegration_StagedOldSoul_Rejected(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	// host-b — «старый» Soul без passage-capability (не эхает passage).
	r := newRunnerWithPassageCap(t, disp, stubPassageCap{lacking: []string{"host-b.example.com"}})

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "soul_passage_unsupported" {
		t.Fatalf("reason = %v, want soul_passage_unsupported (старый Soul под staged отвергнут ДО dispatch)", inc.StatusDetails["reason"])
	}

	// ★ Отказ ДО dispatch: НИ ОДНОГО ApplyRequest (даже probe Passage 0 не ушёл) —
	// не hang, не silent одно-проходное исполнение.
	if p0 := disp.targets(0); len(p0) != 0 {
		t.Fatalf("★ Passage 0 targets = %v, want [] (старый Soul под staged отвергнут ДО любого dispatch)", p0)
	}
}

// TestIntegration_StagedNilPassageCap_FailClosed — ★ FAIL-CLOSED без Redis (ADR-056
// §S5). passageCap=nil (нет presence-источника capability) → staged-прогон НЕ
// угадывает поддержку, отвергается целиком: слать N>1 вслепую = тот же риск
// зависания. N=1-прогоны гейт не проходят (см. остальные тесты). ASSERT:
// ERROR_LOCKED, reason = soul_passage_unsupported, ни одного ApplyRequest.
func TestIntegration_StagedNilPassageCap_FailClosed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	r := newRunnerWithPassageCap(t, disp, nil) // нет Redis-чекера.

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "soul_passage_unsupported" {
		t.Fatalf("reason = %v, want soul_passage_unsupported (nil passageCap → fail-closed)", inc.StatusDetails["reason"])
	}
	if p0 := disp.targets(0); len(p0) != 0 {
		t.Fatalf("★ Passage 0 targets = %v, want [] (nil passageCap → отказ ДО dispatch)", p0)
	}
}

// TestIntegration_RunOnceStaged — run_once+staged СОВМЕСТИМЫ (ADR-056 §S4): run_once
// режет таргет Passage-задачи до первого по SID из РЕЗОЛВНУТОГО (свежим register)
// таргета ЭТОГО Passage. Сценарий: probe role (p0) → run_once-act where role==master
// (p1). ОБА хоста master → where пропускает обоих → run_once срезает до первого по
// SID (host-a). ASSERT: Passage-1 ApplyRequest ТОЛЬКО на host-a (первый по SID из
// master-таргета), НЕ на оба. Доказывает, что run_once применяется к таргету,
// резолвнутому per-host register Passage 0 (а не к первому хосту roster вслепую).
func TestIntegration_RunOnceStaged(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})

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
description: run_once+staged service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/runoncestaged/main.yml", `name: runoncestaged
description: probe role then run_once-act on master (staged + run_once)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Run-once act on master
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    run_once: true
    params:
      cmd: promote
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init run_once+staged service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	gitURL := "file://" + dir

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "master", // ОБА master — where пропустит обоих
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "runoncestaged",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want оба хоста", p0)
	}
	// ★ Passage 1: ТОЛЬКО host-a — run_once срезал master-таргет {host-a,host-b}
	// до первого по SID. Это доказывает, что run_once применился к РЕЗОЛВНУТОМУ
	// where-таргету Passage 1 (оба master), а не к слепому первому хосту roster.
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("★ Passage 1 targets = %v, want [host-a.example.com] (run_once → первый по SID из master-таргета)", p1)
	}
}

// TestIntegration_StagedInlineNotAcolyteClaim — ★ staged идёт INLINE при
// AcolyteEnabled (ADR-056 §S4 ЛИМИТ). run.go:308 гейт `!staged` исключает staged-
// прогон из Acolyte-пути (dispatchPlanned): staged-render крутится inline даже когда
// инстанс в work-queue-режиме. Доказательство: ни одной planned/claimed строки
// apply_runs (Acolyte-путь пишет planned на КАЖДЫЙ roster-хост ДО claim); все
// строки сразу терминальны (inline Insert(running)→SendApply→success), passage
// корректно проставлен. Per-passage Acolyte-claim отложен (follow-up).
func TestIntegration_StagedInlineNotAcolyteClaim(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	// AcolyteEnabled=true: НО staged-прогон обязан идти inline (гейт !staged).
	r := newRunnerAcolyte(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Inline-путь сразу диспатчит SendApply (НЕ пишет planned для Acolyte-claim):
	// Passage 0 ушёл на оба хоста, Passage 1 — на master.
	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want оба хоста (inline SendApply, не Acolyte planned)", p0)
	}
	if p1 := disp.targets(1); len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Errorf("Passage 1 targets = %v, want [host-a.example.com] (inline)", p1)
	}

	// ★ Ни одной planned/claimed строки: staged НЕ пошёл Acolyte-путём
	// (dispatchPlanned написал бы planned на КАЖДЫЙ roster-хост). Все строки
	// сразу терминальны success (inline Insert(running)→success).
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	for _, st := range statuses {
		if st.Status == applyrun.StatusPlanned || st.Status == applyrun.StatusClaimed {
			t.Fatalf("★ apply_runs[%s,passage=%d] = %s — staged ОШИБОЧНО пошёл Acolyte-путём (planned/claimed), а обязан inline", st.SID, st.Passage, st.Status)
		}
		if st.Status != applyrun.StatusSuccess {
			t.Errorf("apply_runs[%s,passage=%d] = %s, want success", st.SID, st.Passage, st.Status)
		}
	}
}

// TestIntegration_StagedEmptyRegister — устойчивость сравнения: probe отдаёт
// ПУСТОЙ stdout одному хосту и whitespace другому → `where: ... == 'master'`
// ложно на обоих → оба исключены из Passage 1 (Passage-1 ApplyRequest никому) →
// incarnation READY (no-op, как all-slave). Доказывает, что пустой/whitespace
// register не «протекает» в master-таргет.
func TestIntegration_StagedEmptyRegister(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := stagedServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "",    // пустой stdout
		"host-b.example.com": "   ", // whitespace
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	if p0 := disp.targets(0); len(p0) != 2 {
		t.Errorf("Passage 0 targets = %v, want оба хоста", p0)
	}
	// ★ Passage 1: НИ ОДНОГО (empty/whitespace stdout != 'master').
	if p1 := disp.targets(1); len(p1) != 0 {
		t.Fatalf("★ Passage 1 targets = %v, want [] (empty/whitespace register не == 'master')", p1)
	}

	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		if len(got[sid]) != 1 || got[sid][0] != 0 {
			t.Errorf("%s passages = %v, want [0] (probe только)", sid, got[sid])
		}
	}
}

// multiTaskPassage0ServiceRepo — сценарий, где Passage 0 несёт ДВЕ задачи (probe
// `X` + ещё одна задача без register-зависимости), а Passage 1 читает register.X
// в where. Воспроизводит латентный баг task_idx-коллизии (ADR-056 §S1):
//
//	#0 probe X        → Passage 0, register: X, локальный task_idx 0
//	#1 noop step      → Passage 0, без register, локальный task_idx 1
//	#2 act where X    → Passage 1, локальный task_idx 0 (!) — коллизия с #0 по task_idx
//
// Если бы register ключевался по локальному task_idx, probe-X (Passage0/idx0) и
// действие (Passage1/idx0) делили бы ключ. Корреляция по глобальному plan_index
// разводит их (X — plan_index 0, действие — plan_index 2).
func multiTaskPassage0ServiceRepo(t *testing.T) string {
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
description: multi-task passage-0 proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/failover/main.yml", `name: failover
description: probe X (p0) + noop (p0) → act where X (p1)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Noop step in passage 0
    module: core.exec.run
    changed_when: "false"
    params:
      cmd: noop
  - name: Act on master only
    module: core.exec.run
    register: action
    where: "register.role.stdout == 'master'"
    changed_when: "false"
    params:
      cmd: promote
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init multi-task passage-0 service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// multiTaskDispatcher симулирует Soul для multi-task-Passage-0 кейса: пишет
// register `role` (Passage 0, локальный idx 0) И register `action` (Passage 1,
// локальный idx 0). Под СТАРЫМ PK (apply_id, sid, task_idx) обе строки делили бы
// ключ (host-a, 0) → ON CONFLICT затёр бы probe-role действием (баг). Под новым
// PK (apply_id, sid, plan_index) они сосуществуют (plan_index 0 и 2). Индексы
// берутся из req.Tasks[] (emitterIndices), как Soul эхает их в TaskEvent.
type multiTaskDispatcher struct {
	t         *testing.T
	roleBySID map[string]string

	mu                sync.Mutex
	targetedByPassage map[int][]string
}

func (d *multiTaskDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()

	d.mu.Lock()
	d.targetedByPassage[passage] = append(d.targetedByPassage[passage], sid)
	d.mu.Unlock()

	switch passage {
	case 0:
		localIdx, planIdx := emitterIndices(req, "role")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx, Passage: 0,
			RegisterData: map[string]any{"stdout": d.roleBySID[sid], "changed": false, "failed": false},
		}); err != nil {
			d.t.Errorf("multiTaskDispatcher: UpsertTaskRegister role (%s): %v", sid, err)
		}
	case 1:
		// action — ГЛОБАЛЬНЫЙ plan_index 2, но ЛОКАЛЬНЫЙ task_idx 0 (#2 — первый и
		// единственный шаг среза Passage 1). task_idx коллидирует с probe role
		// (тоже локальный 0 в Passage 0); plan_index — нет.
		localIdx, planIdx := emitterIndices(req, "action")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx, Passage: 1,
			RegisterData: map[string]any{"stdout": "promoted", "changed": false, "failed": false},
		}); err != nil {
			d.t.Errorf("multiTaskDispatcher: UpsertTaskRegister action (%s): %v", sid, err)
		}
	}

	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("multiTaskDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *multiTaskDispatcher) targets(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.targetedByPassage[passage]...)
	return out
}

// TestIntegration_MultiTaskPassage0_RegisterNotClobbered — ★ GUARD (ADR-056 §S1
// fix Variant B), интеграция: Passage 0 несёт ДВЕ задачи (probe role + noop),
// Passage 1 несёт действие с register `action`. probe role (Passage0/локальный
// idx 0) и действие (Passage1/локальный idx 0) коллидируют по task_idx — под
// старым PK (apply_id, sid, task_idx) ON CONFLICT затёр бы probe-register
// действием; корреляция по глобальному plan_index разводит их (0 и 2).
//
// ASSERT: (1) probe-register role НЕ затёрт (строка plan_index 0 несёт probe-
// значение, не 'promoted'); (2) Passage-1 затаргетился ТОЛЬКО на master (where
// резолвнулся правильным значением probe). Доказывает фикс end-to-end через
// stage-loop с двумя задачами в Passage 0 и register-задачей в Passage 1.
func TestIntegration_MultiTaskPassage0_RegisterNotClobbered(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := multiTaskPassage0ServiceRepo(t)

	disp := &multiTaskDispatcher{
		t: t,
		roleBySID: map[string]string{
			"host-a.example.com": "master",
			"host-b.example.com": "slave",
		},
		targetedByPassage: map[int][]string{},
	}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// (1) ★ probe-register role НЕ затёрт действием Passage 1. Глобальный
	// plan_index probe role = 0 (первый шаг плана); проверяем, что строка с этим
	// plan_index несёт probe-значение, а не значение действия.
	regs, err := applyrun.SelectTaskRegistersByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectTaskRegistersByApplyID: %v", err)
	}
	var probeFound bool
	for _, reg := range regs {
		if reg.SID == "host-a.example.com" && reg.PlanIndex == 0 {
			probeFound = true
			if reg.RegisterData["stdout"] != "master" {
				t.Errorf("★ probe-register (plan_index 0) host-a.stdout = %v, want master (затёрт действием?)", reg.RegisterData["stdout"])
			}
		}
	}
	if !probeFound {
		t.Fatalf("★ probe-register (plan_index 0) host-a отсутствует — затёрт коллизией task_idx (баг)")
	}

	// (2) ★ Passage 1 — ТОЛЬКО master (where резолвнулся непустым probe-register).
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("★ Passage 1 targets = %v, want [host-a.example.com] — register.role не резолвнулся (probe затёрт?)", p1)
	}
}

// perHostWhereServiceRepo — сценарий, где per-host where внутри Passage 0 даёт
// register-задаче РАЗНЫЙ локальный task_idx на разных хостах:
//
//	#0 master-only step  → Passage 0, where: только master-хост
//	#1 probe role_after  → Passage 0, register: role_after (оба хоста)
//	#2 act where after   → Passage 1, where: register.role_after
//
// На master-хосте срез Passage 0 = [#0,#1] → probe role_after на локальной 1;
// на не-master срез = [#1] (#0 отфильтрован where) → probe role_after на локальной
// 0. task_idx у probe разный (1 vs 0), глобальный plan_index одинаковый (1).
func perHostWhereServiceRepo(t *testing.T) string {
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
description: per-host where in same passage proof service
state_schema:
  type: object
  properties: {}
`)
	// Passage 0 несёт #0 (host-A-only prep, where по СТАБИЛЬНОМУ факту
	// soulprint.self.covens — coven 'box-a' только у host-a) + #1 (probe role, оба
	// хоста). where #0 опирается на coven-членство (registry-факт, не register) →
	// оба в Passage 0, но #0 фильтруется per-host. Срез host-a = [#0,#1] (probe на
	// локальной 1); срез host-b = [#1] (probe на локальной 0). #2 (Passage 1)
	// читает register role probe.
	write("scenario/failover/main.yml", `name: failover
description: box-a-only prep (p0) + probe role (p0, both) → act where role (p1)
state_changes: {}
tasks:
  - name: Box-A-only prep
    module: core.exec.run
    where: "'box-a' in soulprint.self.covens"
    changed_when: "false"
    params:
      cmd: prep
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Act on master only
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    params:
      cmd: promote
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init per-host where service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_PerHostDifferentWhere_RegisterResolves — ★ GUARD (ADR-056 §S1
// fix Variant B), интеграция: per-host where внутри Passage 0 даёт probe-задаче
// `role` РАЗНЫЙ локальный task_idx на двух хостах (host-a срез [#0,#1] → idx 1;
// host-b срез [#1] → idx 0), но глобальный plan_index одинаков (1). Корреляция по
// plan_index резолвит register обоих верно → Passage 1 (`where: register.role ==
// 'master'`) таргетится корректно.
//
// host-a — master (+coven box-a → проходит per-host where #0), host-b — slave.
// ASSERT: Passage 1 → ТОЛЬКО host-a. Если бы корреляция шла по task_idx (host-a
// idx 1, host-b idx 0), register одного из хостов не резолвнулся бы в `role` →
// неверный таргетинг.
func TestIntegration_PerHostDifferentWhere_RegisterResolves(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	// coven box-a только у host-a → per-host where #0 ('box-a' in covens) проходит
	// лишь на host-a, давая probe role разный локальный task_idx на двух хостах.
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod", "box-a"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	gitURL := perHostWhereServiceRepo(t)

	disp := newStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master",
		"host-b.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "failover",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// register role обоих хостов несёт plan_index 1 (глобальный), хоть локальный
	// task_idx разный (host-a 1, host-b 0). Оба резолвятся в имя `role`.
	regs, err := applyrun.SelectTaskRegistersByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectTaskRegistersByApplyID: %v", err)
	}
	taskIdxBySID := map[string]int{}
	for _, reg := range regs {
		if reg.PlanIndex == 1 {
			taskIdxBySID[reg.SID] = reg.TaskIdx
		}
	}
	// host-a probe role на локальной 1 (срез [#0,#1]); host-b на локальной 0 (#0
	// отфильтрован per-host where). Разный task_idx — суть кейса.
	if taskIdxBySID["host-a.example.com"] != 1 {
		t.Errorf("host-a probe role task_idx = %d, want 1 (срез [#0,#1])", taskIdxBySID["host-a.example.com"])
	}
	if taskIdxBySID["host-b.example.com"] != 0 {
		t.Errorf("host-b probe role task_idx = %d, want 0 (срез [#1], #0 отфильтрован)", taskIdxBySID["host-b.example.com"])
	}

	// ★ Passage 1 → ТОЛЬКО master, несмотря на разный per-host локальный task_idx
	// probe-задачи: register резолвится по глобальному plan_index.
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("★ Passage 1 targets = %v, want [host-a.example.com] — register.role не резолвнулся при разном per-host task_idx", p1)
	}
}
