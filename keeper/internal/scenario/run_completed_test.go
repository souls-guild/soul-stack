package scenario

import (
	"context"
	"log/slog"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// fakeChangedReader — ChangedTaskReader: отдаёт фикс-множества CHANGED/FAILED-ключей.
type fakeChangedReader struct {
	keys       map[auditpg.ChangedTaskKey]struct{}
	failedKeys map[auditpg.ChangedTaskKey]struct{}
	err        error
}

func (f *fakeChangedReader) SelectChangedTaskKeys(_ context.Context, _ string) (map[auditpg.ChangedTaskKey]struct{}, error) {
	return f.keys, f.err
}

func (f *fakeChangedReader) SelectFailedTaskKeys(_ context.Context, _ string) (map[auditpg.ChangedTaskKey]struct{}, error) {
	return f.failedKeys, f.err
}

func runCompletedRunner(aw audit.Writer, ar ChangedTaskReader) *Runner {
	return &Runner{
		deps:   Deps{Audit: aw, AuditReader: ar},
		logger: slog.New(slog.DiscardHandler),
	}
}

func runCompletedSpec() RunSpec {
	return RunSpec{
		ApplyID:         "apply-xyz",
		IncarnationName: "redis-prod",
		ScenarioName:    "add_user",
	}
}

// TestEmitRunCompleted_SingleEventWithChangedTasks — событие эмитится РОВНО один
// раз (per-incarnation, не per-host), source=keeper_internal, correlation=apply_id,
// payload несёт changed_tasks свёрнутые по адресу.
func TestEmitRunCompleted_SingleEventWithChangedTasks(t *testing.T) {
	aw := &fakeAuditWriter{}
	ar := &fakeChangedReader{keys: changedKeys(
		auditpg.ChangedTaskKey{SID: "a.local", PlanIndex: 0},
		auditpg.ChangedTaskKey{SID: "b.local", PlanIndex: 0},
	)}
	r := runCompletedRunner(aw, ar)

	tasks := []*render.RenderedTask{
		{Index: 0, Name: "install", Register: "pkg", Module: "core.pkg.installed"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"a.local", "b.local", "c.local"}},
	}

	r.emitRunCompleted(context.Background(), runCompletedSpec(), runCompletedStatusSuccess, tasks, plans, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want exactly 1 (per-incarnation, not per-host)", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventIncarnationRunCompleted {
		t.Errorf("event_type = %q, want incarnation.run_completed", ev.EventType)
	}
	if ev.Source != audit.SourceKeeperInternal {
		t.Errorf("source = %q, want keeper_internal", ev.Source)
	}
	if ev.CorrelationID != "apply-xyz" {
		t.Errorf("correlation_id = %q, want apply-xyz (= apply_id)", ev.CorrelationID)
	}
	if ev.ArchonAID != "" {
		t.Errorf("archon_aid = %q, want empty (NULL)", ev.ArchonAID)
	}
	if ev.Payload["incarnation"] != "redis-prod" || ev.Payload["scenario"] != "add_user" {
		t.Errorf("payload incarnation/scenario = %v/%v", ev.Payload["incarnation"], ev.Payload["scenario"])
	}
	if ev.Payload["status"] != "success" {
		t.Errorf("payload status = %v, want success", ev.Payload["status"])
	}

	ct, ok := ev.Payload["changed_tasks"].([]map[string]any)
	if !ok {
		t.Fatalf("changed_tasks type = %T, want []map[string]any", ev.Payload["changed_tasks"])
	}
	if len(ct) != 1 {
		t.Fatalf("changed_tasks len = %d, want 1", len(ct))
	}
	if ct[0]["changed_hosts"] != 2 || ct[0]["total_hosts"] != 3 {
		t.Errorf("changed_hosts/total_hosts = %v/%v, want 2/3", ct[0]["changed_hosts"], ct[0]["total_hosts"])
	}
}

// TestEmitRunCompleted_NilAuditReaderNoChangedTasks — AuditReader=nil:
// событие пишется БЕЗ changed_tasks (пустой массив), факт терминала не теряется.
func TestEmitRunCompleted_NilAuditReaderNoChangedTasks(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := runCompletedRunner(aw, nil)

	tasks := []*render.RenderedTask{{Index: 0, Register: "pkg", Module: "core.pkg.installed"}}
	plans := []render.DispatchPlan{{TaskIndex: 0, TargetSIDs: []string{"a.local"}}}

	r.emitRunCompleted(context.Background(), runCompletedSpec(), runCompletedStatusSuccess, tasks, plans, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1 (terminal fact written even without reader)", len(aw.events))
	}
	ct, ok := aw.events[0].Payload["changed_tasks"].([]map[string]any)
	if !ok {
		t.Fatalf("changed_tasks type = %T", aw.events[0].Payload["changed_tasks"])
	}
	if len(ct) != 0 {
		t.Errorf("changed_tasks len = %d, want 0 (no reader → empty)", len(ct))
	}
}

// TestEmitRunCompleted_NilAuditNoEvent — Audit=nil: событие не пишется вовсе
// (unit-сборка без аудита), вызов не паникует.
func TestEmitRunCompleted_NilAuditNoEvent(t *testing.T) {
	r := runCompletedRunner(nil, &fakeChangedReader{})
	// Не должно паниковать и ничего не писать.
	r.emitRunCompleted(context.Background(), runCompletedSpec(), runCompletedStatusSuccess, nil, nil, slog.New(slog.DiscardHandler))
}

// TestEmitRunCompleted_FailedLateAbortPartialChangedTasks — провал с ПОЗДНИМ
// abort (tasks/plans есть, render успел): status=failed, changed_tasks несёт
// ЧАСТИЧНОЕ изменившееся (что успело CHANGED до падения). Та же форма payload,
// что и на успехе.
func TestEmitRunCompleted_FailedLateAbortPartialChangedTasks(t *testing.T) {
	aw := &fakeAuditWriter{}
	// CHANGED только на одном из двух таргет-хостов (частичный прогресс до падения).
	ar := &fakeChangedReader{keys: changedKeys(
		auditpg.ChangedTaskKey{SID: "a.local", PlanIndex: 0},
	)}
	r := runCompletedRunner(aw, ar)

	tasks := []*render.RenderedTask{
		{Index: 0, Name: "install", Register: "pkg", Module: "core.pkg.installed"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"a.local", "b.local"}},
	}

	r.emitRunCompleted(context.Background(), runCompletedSpec(), runCompletedStatusFailed, tasks, plans, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1 (failed terminal)", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventIncarnationRunCompleted {
		t.Errorf("event_type = %q, want incarnation.run_completed", ev.EventType)
	}
	if ev.Payload["status"] != "failed" {
		t.Errorf("payload status = %v, want failed", ev.Payload["status"])
	}
	ct, ok := ev.Payload["changed_tasks"].([]map[string]any)
	if !ok {
		t.Fatalf("changed_tasks type = %T", ev.Payload["changed_tasks"])
	}
	if len(ct) != 1 {
		t.Fatalf("changed_tasks len = %d, want 1 (partial CHANGED on late abort)", len(ct))
	}
	if ct[0]["changed_hosts"] != 1 || ct[0]["total_hosts"] != 2 {
		t.Errorf("changed_hosts/total_hosts = %v/%v, want 1/2 (partial)", ct[0]["changed_hosts"], ct[0]["total_hosts"])
	}
}

// TestEmitRunCompleted_FailedEarlyAbortEmptyChangedTasks — провал с РАННИМ abort
// (tasks=nil/plans=nil: no_hosts/scenario_load_failed/… до render): status=failed,
// changed_tasks пуст, вызов НЕ паникует на nil-входе (buildChangedTasks(nil,…)
// возвращает nil).
func TestEmitRunCompleted_FailedEarlyAbortEmptyChangedTasks(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := runCompletedRunner(aw, &fakeChangedReader{})

	// Ранний abort: ни tasks, ни plans (render не дошёл) — не должно паниковать.
	r.emitRunCompleted(context.Background(), runCompletedSpec(), runCompletedStatusFailed, nil, nil, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1 (failed terminal even on early abort)", len(aw.events))
	}
	if aw.events[0].Payload["status"] != "failed" {
		t.Errorf("payload status = %v, want failed", aw.events[0].Payload["status"])
	}
	ct, ok := aw.events[0].Payload["changed_tasks"].([]map[string]any)
	if !ok {
		t.Fatalf("changed_tasks type = %T", aw.events[0].Payload["changed_tasks"])
	}
	if len(ct) != 0 {
		t.Errorf("changed_tasks len = %d, want 0 (early abort, nil tasks)", len(ct))
	}
}

// TestEmitRunCompleted_CadenceIDPresentWhenSet — spec.CadenceID != nil → payload
// несёт cadence_id (дочерний Voyage расписания, T4b).
func TestEmitRunCompleted_CadenceIDPresentWhenSet(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := runCompletedRunner(aw, nil)

	spec := runCompletedSpec()
	cadenceID := "cad-01"
	spec.CadenceID = &cadenceID

	r.emitRunCompleted(context.Background(), spec, runCompletedStatusSuccess, nil, nil, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(aw.events))
	}
	if got := aw.events[0].Payload["cadence_id"]; got != "cad-01" {
		t.Errorf("payload cadence_id = %v, want cad-01", got)
	}
}

// TestEmitRunCompleted_CadenceIDAbsentWhenNil — spec.CadenceID == nil (ручной
// прогон) → ключ cadence_id ОТСУТСТВУЕТ в payload (консервативно, как drift-
// payload).
func TestEmitRunCompleted_CadenceIDAbsentWhenNil(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := runCompletedRunner(aw, nil)

	spec := runCompletedSpec() // CadenceID == nil
	r.emitRunCompleted(context.Background(), spec, runCompletedStatusSuccess, nil, nil, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(aw.events))
	}
	if _, present := aw.events[0].Payload["cadence_id"]; present {
		t.Errorf("payload carries cadence_id on manual run; want absent (CadenceID=nil)")
	}
}

// TestEmitRunCompleted_VoyageIDPresentWhenSet — spec.VoyageID != nil (прогон через
// Voyage) → payload несёт voyage_id на УСПЕХЕ (ADR-052 amend §k, visibility-фетч
// Voyage detail).
func TestEmitRunCompleted_VoyageIDPresentWhenSet(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := runCompletedRunner(aw, nil)

	spec := runCompletedSpec()
	voyageID := "voy-77"
	spec.VoyageID = &voyageID

	r.emitRunCompleted(context.Background(), spec, runCompletedStatusSuccess, nil, nil, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(aw.events))
	}
	if got := aw.events[0].Payload["voyage_id"]; got != "voy-77" {
		t.Errorf("payload voyage_id = %v, want voy-77", got)
	}
}

// TestEmitRunCompleted_VoyageIDPresentOnFailed — voyage_id попадает в payload и на
// ПРОВАЛЬНОМ терминале (abort-ветка идёт через ту же emitRunCompleted): Voyage
// detail должен видеть и провалившиеся per-incarnation прогоны вояжа.
func TestEmitRunCompleted_VoyageIDPresentOnFailed(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := runCompletedRunner(aw, nil)

	spec := runCompletedSpec()
	voyageID := "voy-88"
	spec.VoyageID = &voyageID

	r.emitRunCompleted(context.Background(), spec, runCompletedStatusFailed, nil, nil, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(aw.events))
	}
	if aw.events[0].Payload["status"] != "failed" {
		t.Errorf("payload status = %v, want failed", aw.events[0].Payload["status"])
	}
	if got := aw.events[0].Payload["voyage_id"]; got != "voy-88" {
		t.Errorf("payload voyage_id = %v, want voy-88 (carried on failed terminal too)", got)
	}
}

// TestWriteDestroyFailedAudit_CorrelationIDIsApplyID — guard: destroy_failed-
// событие несёт correlation_id = apply_id (как run_completed), а не только
// payload.apply_id. Без correlation_id колонки событие не находится keyset-
// фильтром по correlation_id (только JSONB-сканом payload->>'apply_id'); этот
// guard ловит регресс пропуска CorrelationID в writeDestroyFailedAudit.
func TestWriteDestroyFailedAudit_CorrelationIDIsApplyID(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := runCompletedRunner(aw, nil)

	spec := runCompletedSpec() // ApplyID = "apply-xyz"
	r.writeDestroyFailedAudit(context.Background(), spec, "teardown failed", nil, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventIncarnationDestroyFailed {
		t.Errorf("event_type = %q, want incarnation.destroy_failed", ev.EventType)
	}
	if ev.Source != audit.SourceKeeperInternal {
		t.Errorf("source = %q, want keeper_internal", ev.Source)
	}
	if ev.CorrelationID != "apply-xyz" {
		t.Errorf("correlation_id = %q, want apply-xyz (= apply_id, как run_completed)", ev.CorrelationID)
	}
	if ev.Payload["apply_id"] != "apply-xyz" {
		t.Errorf("payload apply_id = %v, want apply-xyz", ev.Payload["apply_id"])
	}
	if ev.ArchonAID != "" {
		t.Errorf("archon_aid = %q, want empty (NULL, write-path)", ev.ArchonAID)
	}
}

// TestEmitRunCompleted_VoyageIDAbsentWhenNil — spec.VoyageID == nil (прямой путь
// create/rerun/destroy, минующий Voyage) → ключ voyage_id ОТСУТСТВУЕТ в payload
// (симметрия с cadence_id).
func TestEmitRunCompleted_VoyageIDAbsentWhenNil(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := runCompletedRunner(aw, nil)

	spec := runCompletedSpec() // VoyageID == nil
	r.emitRunCompleted(context.Background(), spec, runCompletedStatusSuccess, nil, nil, slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(aw.events))
	}
	if _, present := aw.events[0].Payload["voyage_id"]; present {
		t.Errorf("payload carries voyage_id on direct (non-Voyage) run; want absent (VoyageID=nil)")
	}
}
