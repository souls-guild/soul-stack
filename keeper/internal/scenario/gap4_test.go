//go:build integration

// Integration-тесты GAP#4: закрытие пробелов покрытия scenario-run + unlock
// по qa-находкам. Дополняют integration_test.go (тот же стенд: testcontainers
// PG, local-fs git service-noop, mock Outbound):
//
//  1. scenario-run против НЕПУСТОГО state — merge, а не перезапись (реальный
//     DB round-trip incarnation.state).
//  2. Цельный lifecycle одним тестом: create → error_locked → run (lock-gate
//     отказ) → unlock → run снова → success.
//  3. Concurrency lock-gate: параллельные прогоны против ОДНОЙ incarnation —
//     ровно один стартует apply, остальные отбиваются (lockRun под FOR UPDATE).

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// seedIncarnationWithState создаёт ready-incarnation с заданным начальным
// state (seedIncarnation из integration_test.go стартует от пустого state —
// merge-into-existing им не покрывается).
func seedIncarnationWithState(t *testing.T, name string, state map[string]any) {
	t.Helper()
	inc := &incarnation.Incarnation{
		Name: name, Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusReady,
		State: state,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnationWithState: %v", err)
	}
}

// setBServiceRepo — service-noop, чей scenario `bump` несёт state_changes.sets,
// задающий ТОЛЬКО поле b (статический литерал). Остальные поля state не
// упомянуты — merge должен их сохранить.
func setBServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepoScenario(t, "bump", `name: bump
description: set only field b
state_changes:
  sets:
    b: "20"
tasks:
  - name: No-op step
    module: core.exec.run
    params:
      cmd: echo
      args: ["bump"]
    changed_when: "false"
`)
}

// writeServiceRepoScenario — конструктор local-fs git-репо service-noop с
// scenario/<name>/main.yml = scenarioMain (обобщение writeServiceRepo, который
// жёстко пишет scenario/create/). Вынесено в этот файл, чтобы не трогать
// integration_test.go.
func writeServiceRepoScenario(t *testing.T, scenarioName, scenarioMain string) string {
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
description: noop service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/"+scenarioName+"/main.yml", scenarioMain)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_RunMergesIntoExistingState — GAP#4 #1: incarnation со
// state {a:1, b:2}; scenario bump задаёт ТОЛЬКО b. После прогона (реальный
// DB round-trip): a сохранён (merge не перезаписал весь state), b обновлён на 20.
func TestIntegration_RunMergesIntoExistingState(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithState(t, "noop-prod", map[string]any{"a": 1, "b": 2})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := setBServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "bump",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// a не упомянут в sets → должен сохраниться (JSON round-trip из JSONB → float64).
	if inc.State["a"] != float64(1) {
		t.Errorf("state.a = %v (%T), want 1 (merge должен сохранить непереписанные поля)", inc.State["a"], inc.State["a"])
	}
	// b упомянут в sets → перезаписан литералом "20".
	if inc.State["b"] != "20" {
		t.Errorf("state.b = %v (%T), want \"20\" (sets обновил поле)", inc.State["b"], inc.State["b"])
	}

	// Реальный DB round-trip: читаем state ещё раз напрямую из БД, чтобы
	// подтвердить, что merge закоммичен, а не виден лишь в in-memory снапшоте.
	fromDB, err := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if fromDB.State["a"] != float64(1) {
		t.Errorf("DB state.a = %v, want 1", fromDB.State["a"])
	}
	if fromDB.State["b"] != "20" {
		t.Errorf("DB state.b = %v, want \"20\"", fromDB.State["b"])
	}

	// state_history snapshot: state_before несёт исходный b=2, state_after — b="20".
	hist, total, err := incarnation.HistorySelectByName(context.Background(), integrationPool,
		"noop-prod", incarnation.HistoryFilter{ApplyID: applyID}, 0, 10)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 || len(hist) != 1 {
		t.Fatalf("history entries = %d, want 1", total)
	}
	if hist[0].StateBefore["b"] != float64(2) {
		t.Errorf("history state_before.b = %v, want 2", hist[0].StateBefore["b"])
	}
	if hist[0].StateAfter["a"] != float64(1) {
		t.Errorf("history state_after.a = %v, want 1 (a сохранён в snapshot)", hist[0].StateAfter["a"])
	}
	if hist[0].StateAfter["b"] != "20" {
		t.Errorf("history state_after.b = %v, want \"20\"", hist[0].StateAfter["b"])
	}
}

// TestIntegration_Lifecycle_LockUnlockRerun — GAP#4 #2: цельный жизненный цикл
// одним тестом.
//
//	create (ready, state {seeded:true})
//	  → прогон #1 fail на хосте → error_locked (state НЕ тронут)
//	  → прогон #2 против error_locked → lock-gate отбивает (ErrLocked,
//	    dispatch не происходит, статус остаётся error_locked) — это бэкенд 409
//	  → Unlock(reason) → ready, status_details сброшены, state сохранён
//	  → прогон #3 → проходит до success.
func TestIntegration_Lifecycle_LockUnlockRerun(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithState(t, "noop-prod", map[string]any{"seeded": true})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ref := artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"}

	// --- прогон #1: fail → error_locked --------------------------------
	summary := "module failed"
	failDisp := &mockDispatcher{t: t, result: applyrun.StatusFailed, summary: &summary}
	rFail := newRunner(t, failDisp, gitURL)

	apply1 := audit.NewULID()
	if err := rFail.Start(context.Background(), RunSpec{
		ApplyID: apply1, IncarnationName: "noop-prod", ServiceRef: ref,
		ScenarioName: "create", StartedByAID: "archon-alice",
	}); err != nil {
		t.Fatalf("Start #1: %v", err)
	}
	locked := waitRunDone(t, "noop-prod", apply1, incarnation.StatusErrorLocked)
	if locked.StatusDetails == nil || locked.StatusDetails["reason"] != "dispatch_failed" {
		t.Fatalf("после fail: status_details = %v, want reason dispatch_failed", locked.StatusDetails)
	}
	if locked.State["seeded"] != true {
		t.Errorf("state.seeded = %v, want true (fail не трогает state)", locked.State["seeded"])
	}

	// --- прогон #2: против error_locked → lock-gate отбивает (бэкенд 409) ---
	gateDisp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	rGate := newRunner(t, gateDisp, gitURL)
	apply2 := audit.NewULID()
	if err := rGate.Start(context.Background(), RunSpec{
		ApplyID: apply2, IncarnationName: "noop-prod", ServiceRef: ref,
		ScenarioName: "create", StartedByAID: "archon-alice",
	}); err != nil {
		t.Fatalf("Start #2: %v", err)
	}
	// lockRun → ErrLocked внутри goroutine: dispatch не происходит, статус
	// остаётся error_locked. Это та же проверка под FOR UPDATE, что handler
	// отдаёт наружу как 409 incarnation-locked.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gateDisp.calls > 0 {
			t.Fatalf("прогон #2: SendApply вызван при error_locked-incarnation (lock-gate не сработал)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stillLocked, err := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByName после #2: %v", err)
	}
	if stillLocked.Status != incarnation.StatusErrorLocked {
		t.Errorf("после отбитого #2: status = %q, want error_locked", stillLocked.Status)
	}

	// --- unlock с reason → ready --------------------------------------
	unlockRes, err := incarnation.Unlock(context.Background(), integrationPool,
		"noop-prod", "triaged: host fixed manually", "archon-alice", audit.NewULID())
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if unlockRes.PreviousStatus != incarnation.StatusErrorLocked {
		t.Errorf("unlock previous_status = %q, want error_locked", unlockRes.PreviousStatus)
	}
	unlocked, err := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByName после unlock: %v", err)
	}
	if unlocked.Status != incarnation.StatusReady {
		t.Fatalf("после unlock: status = %q, want ready", unlocked.Status)
	}
	if unlocked.StatusDetails != nil {
		t.Errorf("после unlock: status_details = %v, want nil (сброшены)", unlocked.StatusDetails)
	}
	if unlocked.State["seeded"] != true {
		t.Errorf("после unlock: state.seeded = %v, want true (unlock не трогает state)", unlocked.State["seeded"])
	}

	// --- прогон #3: ready снова → проходит до success ------------------
	okDisp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	rOK := newRunner(t, okDisp, gitURL)
	apply3 := audit.NewULID()
	if err := rOK.Start(context.Background(), RunSpec{
		ApplyID: apply3, IncarnationName: "noop-prod", ServiceRef: ref,
		ScenarioName: "create", StartedByAID: "archon-alice",
	}); err != nil {
		t.Fatalf("Start #3: %v", err)
	}
	final := waitRunDone(t, "noop-prod", apply3, incarnation.StatusReady)
	if final.StatusDetails != nil {
		t.Errorf("после #3: status_details = %v, want nil on success", final.StatusDetails)
	}
	if okDisp.calls != 1 {
		t.Errorf("прогон #3: SendApply calls = %d, want 1 (после unlock прогон проходит)", okDisp.calls)
	}
	if final.State["seeded"] != true {
		t.Errorf("после #3: state.seeded = %v, want true", final.State["seeded"])
	}
}

// blockingDispatcher симулирует Soul, который держит apply открытым до сигнала
// release (чтобы прогон-победитель удерживал incarnation в applying, пока
// конкуренты пытаются стартовать). На SendApply считает вызовы, ждёт release,
// затем пишет терминальный success-статус.
type blockingDispatcher struct {
	t       *testing.T
	calls   atomic.Int32
	release chan struct{}
}

func (d *blockingDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.calls.Add(1)
	select {
	case <-d.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("blockingDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

// TestIntegration_Concurrency_LockGate_RunVsRun — GAP#4 #3: N параллельных
// прогонов (разные apply_id) против ОДНОЙ incarnation. lockRun под FOR UPDATE
// сериализует run-vs-run: РОВНО ОДИН переводит incarnation в applying и доходит
// до dispatch (SendApply), остальные на selectForUpdate видят applying →
// ErrAlreadyRunning и отбиваются без dispatch.
//
// Прогоняется под go test -race. Победитель держится в applying через
// blockingDispatcher, пока все конкуренты не отстрелялись на lock-gate; затем
// release завершает прогон-победитель.
func TestIntegration_Concurrency_LockGate_RunVsRun(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ref := artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"}

	disp := &blockingDispatcher{t: t, release: make(chan struct{})}
	r := newRunner(t, disp, gitURL)

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			// Разные apply_id: in-memory active-gate (по apply_id) НЕ участвует —
			// сериализацию обеспечивает только lockRun под FOR UPDATE.
			if err := r.Start(context.Background(), RunSpec{
				ApplyID: audit.NewULID(), IncarnationName: "noop-prod", ServiceRef: ref,
				ScenarioName: "create", StartedByAID: "archon-alice",
			}); err != nil {
				t.Errorf("Start: %v", err)
			}
		}()
	}
	wg.Wait()

	// Ждём, пока ровно один прогон дойдёт до SendApply (победитель залочил
	// incarnation в applying и заблокировался на release). Остальные за это
	// время должны отбиться на lock-gate.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if disp.calls.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := disp.calls.Load(); got != 1 {
		t.Fatalf("SendApply calls = %d, want ровно 1 (lock-gate сериализует run-vs-run)", got)
	}

	// Даём проигравшим время убедиться, что они не дёрнут dispatch позже.
	time.Sleep(300 * time.Millisecond)
	if got := disp.calls.Load(); got != 1 {
		t.Fatalf("SendApply calls = %d после паузы, want 1 (конкуренты не должны стартовать apply)", got)
	}

	// apply_runs: только победитель завёл running-row. Releaseим победителя и
	// ждём, пока incarnation вернётся в ready (success-терминал прогона).
	close(disp.release)
	waitStatus(t, "noop-prod", incarnation.StatusReady)

	var applyRunRows int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM apply_runs`).Scan(&applyRunRows); err != nil {
		t.Fatalf("count apply_runs: %v", err)
	}
	if applyRunRows != 1 {
		t.Errorf("apply_runs rows = %d, want 1 (только победитель завёл строку прогона)", applyRunRows)
	}
}

// waitStatus поллит статус incarnation до достижения want (или timeout).
// Отдельно от waitRunDone: в concurrency-тесте apply_id победителя заранее не
// известен (генерится в каждой goroutine), поэтому ждём по статусу.
func waitStatus(t *testing.T, name string, want incarnation.Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inc, err := incarnation.SelectByName(context.Background(), integrationPool, name)
		if err != nil {
			t.Fatalf("waitStatus SelectByName: %v", err)
		}
		if inc.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("incarnation %s не достиг статуса %q за 10s", name, want)
}
