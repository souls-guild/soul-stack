//go:build integration

// Integration-guard-ы терминального события incarnation.run_completed на ПРОВАЛЕ
// прогона (T4-фундамент, ADR-052 §k). Проверяют ветвление abort()-пути run():
//
//   - поздний abort (dispatch_failed: tasks/plans уже отрендерены) → событие
//     status=failed с частичным changed_tasks;
//   - ранний abort (no_hosts: render не дошёл, tasks/plans nil) → событие
//     status=failed с пустым changed_tasks (не паникует);
//   - TerminalDestroy-провал (teardown упал) → НЕ эмитит run_completed (свой
//     терминал destroy_failed);
//   - single-winner-сигнал lockIncarnation (finalized) на уже-финализированной
//     incarnation.
//
// Инфраструктура (testcontainers PG, mock Outbound, local-fs git) общая с
// integration_test.go. Runner собирается с реальным auditpg.Writer/Reader (в
// отличие от newRunner, который Audit не подключает) — событие действительно
// долетает в audit_log.

package scenario

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// newRunnerWithAudit собирает Runner с реальным auditpg.Writer/Reader — иначе
// emitRunCompleted (Audit==nil) молча no-op-ит и событие в audit_log не долетает.
func newRunnerWithAudit(t *testing.T, disp ApplyDispatcher) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:       artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:     topology.NewResolver(integrationPool, nil, nil),
		Essence:      essence.NewResolver(nil),
		Render:       render.NewPipeline(nil, engine, nil, nil),
		Outbound:     disp,
		DB:           integrationPool,
		Audit:        auditpg.NewWriter(integrationPool),
		AuditReader:  auditpg.NewReader(integrationPool),
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
	})
}

// runCompletedEvents читает из audit_log все incarnation.run_completed-события
// прогона applyID (correlation_id = apply_id) и возвращает их payload-ы.
func runCompletedEvents(t *testing.T, applyID string) []map[string]any {
	t.Helper()
	rows, err := integrationPool.Query(context.Background(),
		`SELECT payload FROM audit_log
		   WHERE event_type = $1 AND correlation_id = $2
		   ORDER BY created_at`,
		string(audit.EventIncarnationRunCompleted), applyID)
	if err != nil {
		t.Fatalf("query run_completed events: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var p map[string]any
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// waitRunCompletedEvents поллит до появления ровно одного incarnation.run_completed
// события прогона applyID и возвращает его payload. Событие в abort()/success-ветке
// пишется под detached-ctx ПОСЛЕ финализации статуса incarnation, поэтому ждать
// статус (waitRunDone) недостаточно — нужно дождаться самой записи в audit_log.
func waitRunCompletedEvents(t *testing.T, applyID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if evs := runCompletedEvents(t, applyID); len(evs) > 0 {
			if len(evs) > 1 {
				t.Fatalf("run_completed events = %d, want ровно 1 (одно событие на прогон)", len(evs))
			}
			return evs[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("incarnation.run_completed для apply_id=%s не появилось за 10s", applyID)
	return nil
}

// TestIntegration_RunCompletedFailed_LateAbort — поздний abort (dispatch_failed:
// SendApply вернул терминал failed после render) → incarnation.run_completed
// status=failed эмитится ровно один раз, changed_tasks — частичный (тут изменений
// нет, changed_when:false, поэтому пустой, но СОБЫТИЕ есть). Симметрия с
// TestIntegration_FailPath, дополненная проверкой события.
func TestIntegration_RunCompletedFailed_LateAbort(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	summary := "module failed"
	disp := &mockDispatcher{t: t, result: applyrun.StatusFailed, summary: &summary}
	r := newRunnerWithAudit(t, disp)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

	ev := waitRunCompletedEvents(t, applyID)
	if ev["status"] != "failed" {
		t.Errorf("event status = %v, want failed", ev["status"])
	}
	if ev["incarnation"] != "noop-prod" {
		t.Errorf("event incarnation = %v, want noop-prod", ev["incarnation"])
	}
	if _, hasCadence := ev["cadence_id"]; hasCadence {
		t.Errorf("ручной прогон не должен нести cadence_id, got %v", ev["cadence_id"])
	}
}

// TestIntegration_RunCompletedFailed_EarlyAbort — ранний abort (no_hosts: render
// не дошёл, tasks/plans nil) → событие status=failed с пустым changed_tasks, без
// паники на nil-входе buildChangedTasks.
func TestIntegration_RunCompletedFailed_EarlyAbort(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// Ни одного connected-хоста → no_hosts (abort до render).
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithAudit(t, disp)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "no_hosts" {
		t.Fatalf("reason = %v, want no_hosts", inc.StatusDetails["reason"])
	}

	ev := waitRunCompletedEvents(t, applyID)
	if ev["status"] != "failed" {
		t.Errorf("event status = %v, want failed", ev["status"])
	}
	ct, ok := ev["changed_tasks"].([]any)
	if !ok {
		t.Fatalf("changed_tasks type = %T, want []any (JSONB array)", ev["changed_tasks"])
	}
	if len(ct) != 0 {
		t.Errorf("changed_tasks len = %d, want 0 (ранний abort, tasks=nil)", len(ct))
	}
}

// TestIntegration_RunCompletedFailed_CadenceIDPresent — прогон с CadenceID != nil
// (имитация дочернего Voyage расписания) → провальное событие несёт cadence_id.
func TestIntegration_RunCompletedFailed_CadenceIDPresent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithAudit(t, disp)

	cadenceID := "cad-77"
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		CadenceID:       &cadenceID,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// no_hosts → failed-терминал; нас интересует cadence_id в payload.
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

	ev := waitRunCompletedEvents(t, applyID)
	if ev["cadence_id"] != "cad-77" {
		t.Errorf("event cadence_id = %v, want cad-77 (дочерний Voyage расписания)", ev["cadence_id"])
	}
}

// TestIntegration_RunCompleted_VoyageIDPresent — прогон с VoyageID != nil
// (имитация спавна через Voyage-orchestrator) → событие incarnation.run_completed
// несёт voyage_id в payload (ADR-052 amend §k, visibility-фетч Voyage detail).
// Проверяем сквозь реальный run() (failed-путь, симметрично CadenceID-тесту) и
// что то же событие находится фильтром по payload->>'voyage_id' (Voyage detail).
func TestIntegration_RunCompleted_VoyageIDPresent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithAudit(t, disp)

	voyageID := "voy-77"
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		VoyageID:        &voyageID,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// no_hosts → failed-терминал; нас интересует voyage_id в payload.
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

	ev := waitRunCompletedEvents(t, applyID)
	if ev["voyage_id"] != "voy-77" {
		t.Errorf("event voyage_id = %v, want voy-77 (прогон через Voyage)", ev["voyage_id"])
	}
	if _, hasCadence := ev["cadence_id"]; hasCadence {
		t.Errorf("voyage без cadence не должен нести cadence_id, got %v", ev["cadence_id"])
	}

	// Voyage detail фетчит run-события вояжа фильтром payload->>'voyage_id'
	// (correlation_id у per-incarnation события = apply_id, не voyage_id).
	var byVoyage int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE event_type = $1 AND payload->>'voyage_id' = $2`,
		string(audit.EventIncarnationRunCompleted), voyageID).Scan(&byVoyage); err != nil {
		t.Fatalf("count by voyage_id: %v", err)
	}
	if byVoyage != 1 {
		t.Errorf("событий вояжа по payload->>'voyage_id' = %d, want 1", byVoyage)
	}
}

// TestIntegration_DestroyFailed_NoRunCompleted — провал teardown (TerminalDestroy)
// уходит в свой терминал destroy_failed и НЕ эмитит incarnation.run_completed
// (гейт TerminalMode != TerminalDestroy в abort).
func TestIntegration_DestroyFailed_NoRunCompleted(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedDestroyingIncarnation(t, "noop-prod", map[string]any{"leader": "host-a"})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := destroyServiceRepo(t)

	summary := "teardown failed"
	disp := &mockDispatcher{t: t, result: applyrun.StatusFailed, summary: &summary}
	r := newRunnerWithAudit(t, disp)

	applyID := audit.NewULID()
	if err := r.StartDestroy(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("StartDestroy: %v", err)
	}

	waitStatusInc(t, "noop-prod", incarnation.StatusDestroyFailed)

	// destroy_failed-событие — терминал destroy, должно появиться. Фильтр по
	// correlation_id штатно (writeDestroyFailedAudit ставит correlation_id=apply_id,
	// как run_completed). Поллим до него — событие пишется под detached-ctx после
	// смены статуса.
	deadline := time.Now().Add(10 * time.Second)
	var destroyFailed int
	for time.Now().Before(deadline) {
		if err := integrationPool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_log WHERE event_type = $1 AND correlation_id = $2`,
			string(audit.EventIncarnationDestroyFailed), applyID).Scan(&destroyFailed); err != nil {
			t.Fatalf("count destroy_failed: %v", err)
		}
		if destroyFailed > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if destroyFailed != 1 {
		t.Errorf("destroy_failed events = %d, want 1 (свой терминал destroy)", destroyFailed)
	}

	// run_completed для destroy-провала НЕ эмитится (гейт TerminalDestroy в abort).
	// destroy_failed уже записан выше → если бы run_completed эмитился, он бы тоже
	// успел; проверяем после его появления.
	if evs := runCompletedEvents(t, applyID); len(evs) != 0 {
		t.Errorf("run_completed events = %d, want 0 (destroy-провал эмитит destroy_failed, НЕ run_completed)", len(evs))
	}
}

// TestIntegration_LockIncarnation_SingleWinnerSignal — single-winner-сигнал
// lockIncarnation: на incarnation, которую ДРУГОЙ коммиттер уже вывел из applying
// (UpdateStateFromRun → ErrAlreadyFinalized), lockIncarnation возвращает
// finalized=false. abort() при finalized=false НЕ эмитит провальное событие
// (защита от дубля при recovery-перехвате). Тут проверяется сам сигнал — гейт
// `&& finalized` в abort читается явно и покрыт этим инвариантом.
func TestIntegration_LockIncarnation_SingleWinnerSignal(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// incarnation в applying — состояние, из которого lockIncarnation финализирует.
	inc := &incarnation.Incarnation{
		Name: "noop-prod", Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusApplying,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("Create applying incarnation: %v", err)
	}

	r := newRunnerWithAudit(t, &mockDispatcher{t: t, result: applyrun.StatusSuccess})
	log := slog.New(slog.DiscardHandler)
	spec := RunSpec{ApplyID: audit.NewULID(), IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}

	// Первый коммиттер (наш) — реально финализирует applying → error_locked.
	if finalized := r.lockIncarnation(context.Background(), spec, nil, incarnation.StatusErrorLocked, "dispatch_failed", nil, log); !finalized {
		t.Fatalf("первый lockIncarnation: finalized=false, want true (реальный финализатор)")
	}

	// Второй вызов (имитация recovery-проигравшего: incarnation уже не в applying)
	// → ErrAlreadyFinalized внутри → finalized=false: провальное событие отдаёт
	// победитель, не этот инстанс.
	spec2 := RunSpec{ApplyID: audit.NewULID(), IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}
	if finalized := r.lockIncarnation(context.Background(), spec2, nil, incarnation.StatusErrorLocked, "dispatch_failed", nil, log); finalized {
		t.Errorf("второй lockIncarnation на уже-финализированной incarnation: finalized=true, want false (single-winner-проигравший)")
	}
}
