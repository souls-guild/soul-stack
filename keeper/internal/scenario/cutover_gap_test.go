//go:build integration

// Integration gap-тесты cutover-а на стратегию Y (ADR-027, Phase 1 cutover):
// full-roster render при claim + groupByHost-фильтр SID-а. Доказывают, что Y
// закрыл BUG-1 (run_once на ВСЕ хосты вместо одного) и BUG-2 (soulprint.hosts
// видит roster из одного хоста), а также minor-фиксы Cancel-в-planned-окне и
// barrier-timeout planned-хоста без claim. Reuse harness-а integration_test.go +
// cutover_test.go (TestMain/seed*/newAcolyteRunner/newClaimRunner/driveClaims/...).

package scenario

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// perHostCountDispatcher симулирует Soul и фиксирует ЧИСЛО задач каждого
// ApplyRequest по SID (sid → tasks) + порядок SID-ов. Завершает barrier
// success-статусом (как mockDispatcher). Конкурентно-безопасен (Acolyte-пул
// дёргает SendApply из нескольких воркеров).
type perHostCountDispatcher struct {
	t      *testing.T
	mu     sync.Mutex
	counts map[string]int
}

func newPerHostCountDispatcher(t *testing.T) *perHostCountDispatcher {
	return &perHostCountDispatcher{t: t, counts: map[string]int{}}
}

func (d *perHostCountDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	d.counts[sid] = len(req.GetTasks())
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("perHostCountDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

func (d *perHostCountDispatcher) snapshot() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int, len(d.counts))
	for k, v := range d.counts {
		out[k] = v
	}
	return out
}

// newAcolyteRunnerWith — newAcolyteRunner с заданным Outbound (а не fakeDispatcher).
// Нужен parity-тесту: старый путь зовёт SendApply на dispatch-е, но новый путь
// его НЕ зовёт (claim делает Acolyte) — Outbound тут безвреден, передаём counting.
func newAcolyteRunnerWith(t *testing.T, summons SummonsPublisher, disp ApplyDispatcher) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		Essence:        essence.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       disp,
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-test",
		Summons:        summons,
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     20 * time.Second,
	})
}

// runOnceScenario — scenario/create с run_once-задачей (исполняется на ОДНОМ
// хосте) + обычной задачей (на всех). До Y single-host render обходил
// applyRunOnce (len(targeted)≤1) → run_once-задача попадала на каждый хост.
const runOnceScenario = `name: create
description: run_once + per-host fixture
state_changes: {}
tasks:
  - name: Run once on first host
    module: core.exec.run
    run_once: true
    params:
      cmd: echo
      args: ["once"]
    changed_when: "false"
  - name: Run on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["all"]
    changed_when: "false"
`

// soulprintHostsScenario — scenario/create, params которой ссылаются на
// soulprint.hosts.size() (число хостов roster-а прогона). До Y single-host render
// схлопывал размер до 1 на каждом хосте.
const soulprintHostsScenario = `name: create
description: soulprint.hosts.size fixture
state_changes: {}
tasks:
  - name: Echo roster size
    module: core.exec.run
    params:
      cmd: "echo ${ soulprint.hosts.size() }"
    changed_when: "false"
`

// perHostWhereScenario — scenario/create с задачей where: по SID. Пропускает на
// host-a, режет на host-b в одном прогоне (per-host резолв where в RenderForHost).
const perHostWhereScenario = `name: create
description: per-host where fixture
state_changes: {}
tasks:
  - name: Only on host-a
    module: core.exec.run
    where: "soulprint.self.sid == 'host-a.example.com'"
    params:
      cmd: echo
      args: ["a-only"]
    changed_when: "false"
`

// driveAcolyteRun стартует Runner в новом пути и доводит все planned-задания
// Acolyte-ом до терминала, возвращая incarnation. n — ожидаемое число
// planned-хостов.
func driveAcolyteRun(t *testing.T, r *Runner, spec RunSpec, disp ApplyDispatcher, nHosts int) *incarnation.Incarnation {
	t.Helper()
	if err := r.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForPlanned(t, spec.ApplyID, nHosts)
	cr := newClaimRunner(t, disp)
	driveClaims(t, cr, spec.ApplyID, nHosts)
	return waitRunDone(t, spec.IncarnationName, spec.ApplyID, incarnation.StatusReady)
}

// TestIntegration_TargetingParity_AcolyteVsOldPath — КЛЮЧЕВОЙ gap-тест: набор
// задач на хост в НОВОМ пути (Acolyte full-roster render + groupByHost) ==
// старый путь, для scenario с run_once И soulprint.hosts. Доказывает, что Y
// закрыл BUG-1 и BUG-2: single-host render давал бы run_once на оба хоста и
// soulprint.hosts.size()==1 — full-roster даёт идентичную старому пути картину.
func TestIntegration_TargetingParity_AcolyteVsOldPath(t *testing.T) {
	const parityScenario = `name: create
description: run_once + soulprint.hosts parity fixture
state_changes: {}
tasks:
  - name: Run once on first host
    module: core.exec.run
    run_once: true
    params:
      cmd: echo
      args: ["once"]
    changed_when: "false"
  - name: Echo roster size on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["${ soulprint.hosts.size() }"]
    changed_when: "false"
`

	// --- старый путь (run-goroutine, прямой render всего roster-а) ---
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, parityScenario)

	oldDisp := newPerHostCountDispatcher(t)
	oldRunner := newRunner(t, oldDisp, gitURL)
	oldApplyID := audit.NewULID()
	if err := oldRunner.Start(context.Background(), RunSpec{
		ApplyID:         oldApplyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("old-path Start: %v", err)
	}
	waitRunDone(t, "noop-prod", oldApplyID, incarnation.StatusReady)
	oldCounts := oldDisp.snapshot()

	// --- новый путь (Acolyte full-roster render + groupByHost) ---
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	// gitURL переиспользуем — тот же снапшот сервиса.

	newDisp := newPerHostCountDispatcher(t)
	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
	driveAcolyteRun(t, r, RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}, newDisp, 2)
	newCounts := newDisp.snapshot()

	// Parity: одинаковый набор задач на каждый хост.
	// Старый путь: host-a (run_once-первый по SID) → 2 задачи; host-b → 1 (только roster-size).
	if oldCounts["host-a.example.com"] != 2 {
		t.Errorf("old-path host-a tasks = %d, want 2 (run_once-первый + roster-size)", oldCounts["host-a.example.com"])
	}
	if oldCounts["host-b.example.com"] != 1 {
		t.Errorf("old-path host-b tasks = %d, want 1 (только roster-size, run_once срезан)", oldCounts["host-b.example.com"])
	}
	for sid, want := range oldCounts {
		if newCounts[sid] != want {
			t.Errorf("PARITY break на %s: новый путь %d задач, старый %d (Y не закрыл BUG-1/2)",
				sid, newCounts[sid], want)
		}
	}
	if len(newCounts) != len(oldCounts) {
		t.Errorf("PARITY break: новый путь покрыл %d хостов, старый %d", len(newCounts), len(oldCounts))
	}
}

// TestIntegration_RunOnce_SingleHostUnderAcolyte — BUG-1 регрессия: run_once при
// acolytes>0 и roster≥2 исполняется на ОДНОМ хосте (первом по SID), не на всех.
// До Y single-host render возвращал run_once-задачу на каждый хост.
func TestIntegration_RunOnce_SingleHostUnderAcolyte(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, runOnceScenario)

	disp := newPerHostCountDispatcher(t)
	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
	driveAcolyteRun(t, r, RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}, disp, 2)

	counts := disp.snapshot()
	// host-a (первый по SID): run_once-задача + общая = 2. host-b: только общая = 1.
	if counts["host-a.example.com"] != 2 {
		t.Errorf("host-a tasks = %d, want 2 (run_once-первый + общая)", counts["host-a.example.com"])
	}
	if counts["host-b.example.com"] != 1 {
		t.Errorf("host-b tasks = %d, want 1 (BUG-1: run_once НЕ должен попасть на host-b)", counts["host-b.example.com"])
	}
}

// TestIntegration_SoulprintHosts_FullRosterUnderAcolyte — BUG-2 регрессия:
// soulprint.hosts под новым путём видит ПОЛНЫЙ roster. Доказательство —
// soulprint.hosts.size() рендерится в "2" на двухhost-овом roster-е (а не в "1",
// как было бы при single-host render-е).
func TestIntegration_SoulprintHosts_FullRosterUnderAcolyte(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, soulprintHostsScenario)

	// Захват отрендеренной команды per-SID: должно быть "echo 2" на обоих хостах.
	disp := newPerHostCommandDispatcher(t)
	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
	driveAcolyteRun(t, r, RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}, disp, 2)

	cmds := disp.snapshot()
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		if cmds[sid] != "echo 2" {
			t.Errorf("%s rendered cmd = %q, want \"echo 2\" (BUG-2: soulprint.hosts.size() видит полный roster)", sid, cmds[sid])
		}
	}
}

// TestIntegration_PerHostWhere_TwoHosts — per-host where в новом пути: задача с
// where по SID пропускается на host-a, режется на host-b в одном прогоне.
// host-b → no-op no_match (нет задач, FINDING-01 (б)), host-a → 1 задача.
func TestIntegration_PerHostWhere_TwoHosts(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, perHostWhereScenario)

	disp := newPerHostCountDispatcher(t)
	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
	applyID := audit.NewULID()
	driveAcolyteRun(t, r, RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}, disp, 2)

	counts := disp.snapshot()
	if counts["host-a.example.com"] != 1 {
		t.Errorf("host-a tasks = %d, want 1 (where пропустил)", counts["host-a.example.com"])
	}
	// host-b: where срезал всё → no-op success, SendApply не звался.
	if _, ok := counts["host-b.example.com"]; ok {
		t.Errorf("host-b получил SendApply, want no-op success (where срезал всё)")
	}
	// Но строка host-b обязана быть success (барьер её засчитал no-op-ом).
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	byStatus := map[string]applyrun.Status{}
	for _, hs := range st {
		byStatus[hs.SID] = hs.Status
	}
	if byStatus["host-b.example.com"] != applyrun.StatusNoMatch {
		t.Errorf("host-b status = %q, want no_match (FINDING-01 (б): where срезал всё на хосте)", byStatus["host-b.example.com"])
	}
}

// TestIntegration_CancelInPlannedWindow_Cancels — minor-фикс (а): Cancel,
// поставленный в planned-окне (между dispatch-ем и claim-ом), реально отменяет.
// RequestCancel теперь бьёт по planned/claimed; Acolyte при claim видит флаг до
// SendApply и переводит задание в cancelled (apply на Soul НЕ уходит). Прогон →
// error_locked (барьер засчитал cancelled как не-success).
func TestIntegration_CancelInPlannedWindow_Cancels(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
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

	// Дожидаемся planned-строки и СТАВИМ Cancel ДО claim-а (planned-окно).
	waitForPlanned(t, applyID, 1)
	affected, err := applyrun.RequestCancel(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected == 0 {
		t.Fatal("RequestCancel affected = 0, want >=1 (planned-строка) — фильтр не покрыл planned")
	}

	// Acolyte клеймит planned, видит cancel_requested до SendApply → cancelled.
	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	driveClaims(t, cr, applyID, 1)

	if disp.calls.Load() != 0 {
		t.Errorf("SendApply calls = %d, want 0 (Cancel до отправки apply)", disp.calls.Load())
	}
	got, err := applyrun.SelectByApplyID(context.Background(), integrationPool, applyID, "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusCancelled {
		t.Errorf("status = %q, want cancelled (Cancel в planned-окне)", got.Status)
	}
	// Прогон отменён → incarnation error_locked (барьер увидел не-success терминал).
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
}

// TestIntegration_NoClaim_BarrierTimeout — planned-хост, которого НИКТО не
// клеймит (Acolyte-пул не запущен), не виснет навсегда: barrier завершается по
// короткому RunTimeout → error_locked (timeout). Так dispatch нового пути не
// блокирует прогон навечно при отсутствии живого Acolyte.
func TestIntegration_NoClaim_BarrierTimeout(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	// Короткий RunTimeout: barrier завершится по нему (ClaimRunner НЕ запускаем —
	// planned-задание никто не подхватит).
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	r := NewRunner(Deps{
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		Essence:        essence.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       fakeDispatcher{},
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-test",
		Summons:        &countingSummons{},
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     2 * time.Second,
	})

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

	// Никакого claim-а: planned-задание висит, barrier упрётся в RunTimeout.
	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("status_details = %+v, want reason=dispatch_failed (barrier timeout)", inc.StatusDetails)
	}
}

// perHostCommandDispatcher — как perHostCountDispatcher, но фиксирует
// отрендеренную команду первой задачи per-SID (для проверки soulprint.hosts.size()).
type perHostCommandDispatcher struct {
	t    *testing.T
	mu   sync.Mutex
	cmds map[string]string
}

func newPerHostCommandDispatcher(t *testing.T) *perHostCommandDispatcher {
	return &perHostCommandDispatcher{t: t, cmds: map[string]string{}}
}

func (d *perHostCommandDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	if tasks := req.GetTasks(); len(tasks) > 0 {
		d.cmds[sid] = renderedExecCommand(tasks[0].GetParams())
	}
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("perHostCommandDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

func (d *perHostCommandDispatcher) snapshot() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]string, len(d.cmds))
	for k, v := range d.cmds {
		out[k] = v
	}
	return out
}
