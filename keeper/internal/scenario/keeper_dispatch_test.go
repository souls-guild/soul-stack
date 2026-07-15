package scenario

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// fakeKeeperModule is a keeper-side core-module stub: returns a pre-set
// final ApplyEvent (changed/failed/output). nil eventsErr → normal stream.
type fakeKeeperModule struct {
	module.BaseModule
	final    *pluginv1.ApplyEvent
	applyErr error
	gotState string
}

func (m *fakeKeeperModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.gotState = req.GetState()
	if m.applyErr != nil {
		return m.applyErr
	}
	if m.final != nil {
		return stream.Send(m.final)
	}
	return nil
}

// fakeKeeperRegistry is a KeeperModuleRegistry backed by a map.
type fakeKeeperRegistry map[string]module.SoulModule

func (r fakeKeeperRegistry) Lookup(name string) (module.SoulModule, bool) {
	m, ok := r[name]
	return m, ok
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return s
}

func TestKeeperTasksOf(t *testing.T) {
	// Passage 0: two keeper tasks (idx 0,2) + one host task (idx 1). Passage 1:
	// one keeper task (idx 3) — stratified later (e.g. core.bootstrap.delivered
	// reads register core.cloud.created). keeperTasksOf(passage) selects
	// keeper tasks for EXACTLY the requested Passage.
	tasks := []*render.RenderedTask{
		{Index: 0, Module: "core.cloud.created", Passage: 0},
		{Index: 1, Module: "core.exec.run", Passage: 0},
		{Index: 2, Module: "core.vault.kv-read", Passage: 0},
		{Index: 3, Module: "core.bootstrap.delivered", Passage: 1},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}},
		{TaskIndex: 1, TargetSIDs: []string{"host-a"}},
		{TaskIndex: 2, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}},
		{TaskIndex: 3, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}},
	}

	got0 := keeperTasksOf(tasks, plans, 0)
	if len(got0) != 2 {
		t.Fatalf("keeperTasksOf(0): len=%d, want 2 (keeper-задачи Passage 0)", len(got0))
	}
	if got0[0].Index != 0 || got0[1].Index != 2 {
		t.Fatalf("keeperTasksOf(0) order = [%d %d], want [0 2]", got0[0].Index, got0[1].Index)
	}

	got1 := keeperTasksOf(tasks, plans, 1)
	if len(got1) != 1 || got1[0].Index != 3 {
		t.Fatalf("keeperTasksOf(1) = %v, want ровно [idx 3] (keeper-задача Passage 1)", got1)
	}

	// A Passage with no keeper tasks → empty (host-only Passage / out of range).
	if got := keeperTasksOf(tasks, plans, 2); len(got) != 0 {
		t.Fatalf("keeperTasksOf(2) = %v, want пусто", got)
	}
}

func TestApplyKeeperTask_Success(t *testing.T) {
	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{
		Changed: true,
		Output:  mustStruct(t, map[string]any{"created": true, "coven": []any{"svc"}}),
	}}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.soul": mod}}

	rt := &render.RenderedTask{Index: 0, Module: "core.soul.registered", Params: mustStruct(t, map[string]any{"sid": "n1"})}
	changed, failed, output, _ := r.applyKeeperTask(context.Background(), rt)
	if !changed || failed {
		t.Fatalf("changed=%v failed=%v, want true/false", changed, failed)
	}
	if mod.gotState != "registered" {
		t.Fatalf("module got state %q, want registered (state-суффикс адреса core.soul.registered)", mod.gotState)
	}
	if output["created"] != true {
		t.Fatalf("output[created] = %v, want true", output["created"])
	}
}

func TestApplyKeeperTask_FailedEvent(t *testing.T) {
	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{Failed: true, Message: "invalid coven"}}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.soul": mod}}

	_, failed, _, msg := r.applyKeeperTask(context.Background(), &render.RenderedTask{Module: "core.soul.registered"})
	if !failed {
		t.Fatalf("failed=false, want true")
	}
	if msg != "invalid coven" {
		t.Fatalf("message = %q, want 'invalid coven'", msg)
	}
}

func TestApplyKeeperTask_UnknownModule(t *testing.T) {
	r := &Runner{keeperModules: fakeKeeperRegistry{}}
	_, failed, _, msg := r.applyKeeperTask(context.Background(), &render.RenderedTask{Module: "core.soul.registered"})
	if !failed {
		t.Fatalf("failed=false, want true (модуль не найден в Registry)")
	}
	if msg == "" {
		t.Fatalf("message пуст, ожидалось упоминание unknown module")
	}
}

func TestApplyKeeperTask_ApplyError(t *testing.T) {
	mod := &fakeKeeperModule{applyErr: fmt.Errorf("ctx canceled")}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.soul": mod}}
	_, failed, _, msg := r.applyKeeperTask(context.Background(), &render.RenderedTask{Module: "core.soul.registered"})
	if !failed || msg != "ctx canceled" {
		t.Fatalf("failed=%v msg=%q, want true/'ctx canceled'", failed, msg)
	}
}

// TestApplyKeeperTask_NoFinalEvent — the module returned nil without ever
// sending an ApplyEvent (final==nil, applyErr==nil): a missing final event is
// a contract anomaly (same as Soul-side); applyKeeperTask → failed with
// message "no final event" (keeper_dispatch.go's last==nil branch). Closes
// QA gap #4.
func TestApplyKeeperTask_NoFinalEvent(t *testing.T) {
	mod := &fakeKeeperModule{} // final=nil, applyErr=nil → Apply sends nothing
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.soul": mod}}

	_, failed, output, msg := r.applyKeeperTask(context.Background(), &render.RenderedTask{Module: "core.soul.registered"})
	if !failed {
		t.Fatalf("failed=false, want true (модуль не прислал финального события)")
	}
	if output != nil {
		t.Errorf("output = %v, want nil", output)
	}
	if !strings.Contains(msg, "no final event") {
		t.Fatalf("message = %q, want содержащее 'no final event'", msg)
	}
}

// TestSyncTraitsOnRegistered_Gating — the Trait relocation bind hook
// (ADR-060 amend R1) filters its injection point: ONLY a successful
// core.soul.registered triggers the projection. For other modules / empty
// incName / nil DB — a no-op with no DB access (a Runner without Deps.DB
// doesn't panic). The full projection is proven in integration
// (incarnation/traits_integration_test.go).
func TestSyncTraitsOnRegistered_Gating(t *testing.T) {
	r := &Runner{} // Deps.DB == nil
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Not a registered module → early exit by address (no DB needed).
	r.syncTraitsOnRegistered(context.Background(), "redis-prod",
		&render.RenderedTask{Module: "core.vault.kv-read"}, log)
	r.syncTraitsOnRegistered(context.Background(), "redis-prod",
		&render.RenderedTask{Module: "core.cloud.provisioned"}, log)

	// registered, but incName is empty (direct keeper test without an incarnation) → no-op.
	r.syncTraitsOnRegistered(context.Background(), "",
		&render.RenderedTask{Module: "core.soul.registered"}, log)

	// registered + incName, but Deps.DB == nil → no-op (doesn't panic, doesn't touch the DB).
	r.syncTraitsOnRegistered(context.Background(), "redis-prod",
		&render.RenderedTask{Module: "core.soul.registered"}, log)

	// Malformed module address → early exit (SplitModuleAddr !ok).
	r.syncTraitsOnRegistered(context.Background(), "redis-prod",
		&render.RenderedTask{Module: "bogus"}, log)
}

func TestComposeKeeperFailure(t *testing.T) {
	rt := &render.RenderedTask{Index: 3, Module: "core.soul.registered"}
	if got := composeKeeperFailure(rt, "boom"); got != "task 3 core.soul.registered: boom" {
		t.Fatalf("composeKeeperFailure = %q", got)
	}
	rtNoLog := &render.RenderedTask{Index: 1, Module: "core.vault.kv-read", NoLog: true}
	if got := composeKeeperFailure(rtNoLog, "secret leaked"); got != "task 1 core.vault.kv-read: (no_log task failed)" {
		t.Fatalf("composeKeeperFailure no_log = %q", got)
	}
}

// TestKeeperTaskStatus_Mapping — maps a keeper task outcome to the keeperv1
// enum: changed→CHANGED, failed→FAILED (failed takes priority over changed),
// else→OK. The changed fold (auditpg) filters on the literal string
// "TASK_STATUS_CHANGED"; a mismatch would silently zero it out for keeper
// tasks.
func TestKeeperTaskStatus_Mapping(t *testing.T) {
	cases := []struct {
		changed, failed bool
		want            string
	}{
		{changed: true, failed: false, want: "TASK_STATUS_CHANGED"},
		{changed: false, failed: true, want: "TASK_STATUS_FAILED"},
		{changed: true, failed: true, want: "TASK_STATUS_FAILED"}, // failed wins
		{changed: false, failed: false, want: "TASK_STATUS_OK"},
	}
	for _, c := range cases {
		if got := keeperTaskStatus(c.changed, c.failed).String(); got != c.want {
			t.Errorf("keeperTaskStatus(changed=%v failed=%v) = %q, want %q", c.changed, c.failed, got, c.want)
		}
	}
}

// TestEmitKeeperTaskExecuted_ChangedEmits — a keeper task with changed →
// task.executed is emitted with sid=KeeperTargetSID ("keeper"),
// status=TASK_STATUS_CHANGED, correlation_id=apply_id,
// source=keeper_internal. This is the address the changed_tasks fold
// (auditpg) and Tiding's task: subscription use to see keeper-side tasks.
func TestEmitKeeperTaskExecuted_ChangedEmits(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := &Runner{deps: Deps{Audit: aw}}
	rt := &render.RenderedTask{Index: 2, Name: "provision", Register: "vm", Module: "core.cloud.created"}

	// passage 1 — the keeper task is stratified (Slice 2): the payload echoes
	// passage for per-Passage triage; changed_tasks correlation is unaffected
	// (keyed by sid/plan_index).
	r.emitKeeperTaskExecuted(context.Background(), "apply-k1", 1 /*passage*/, rt, true /*changed*/, false /*failed*/, "", slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventTaskExecuted {
		t.Errorf("event_type = %q, want task.executed", ev.EventType)
	}
	if ev.Source != audit.SourceKeeperInternal {
		t.Errorf("source = %q, want keeper_internal", ev.Source)
	}
	if ev.CorrelationID != "apply-k1" {
		t.Errorf("correlation_id = %q, want apply-k1 (= apply_id, фильтр SelectChangedTaskKeys)", ev.CorrelationID)
	}
	if ev.Payload["sid"] != render.KeeperTargetSID {
		t.Errorf("payload sid = %v, want %q", ev.Payload["sid"], render.KeeperTargetSID)
	}
	if ev.Payload["status"] != "TASK_STATUS_CHANGED" {
		t.Errorf("payload status = %v, want TASK_STATUS_CHANGED (свёртка фильтрует по литералу)", ev.Payload["status"])
	}
	if ev.Payload["task_idx"] != 2 {
		t.Errorf("payload task_idx = %v, want 2", ev.Payload["task_idx"])
	}
	if ev.Payload["passage"] != 1 {
		t.Errorf("payload passage = %v, want 1 (эхо Passage keeper-задачи, Слайс 2)", ev.Payload["passage"])
	}
}

// TestEmitKeeperTaskExecuted_FailedStatus — a failed keeper task →
// task.executed status=TASK_STATUS_FAILED (NOT CHANGED): such a task will
// NOT land in changed_tasks. error.message is present for non-no_log
// (masking happens on auditpg's write path).
func TestEmitKeeperTaskExecuted_FailedStatus(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := &Runner{deps: Deps{Audit: aw}}
	rt := &render.RenderedTask{Index: 0, Module: "core.cloud.created"}

	r.emitKeeperTaskExecuted(context.Background(), "apply-k2", 0 /*passage*/, rt, false, true /*failed*/, "boom from driver", slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(aw.events))
	}
	ev := aw.events[0]
	if ev.Payload["status"] != "TASK_STATUS_FAILED" {
		t.Errorf("payload status = %v, want TASK_STATUS_FAILED (не CHANGED → не в changed_tasks)", ev.Payload["status"])
	}
	errMap, ok := ev.Payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("payload error type = %T, want map", ev.Payload["error"])
	}
	if errMap["message"] != "boom from driver" {
		t.Errorf("error.message = %v, want 'boom from driver' (не-no_log → message кладётся)", errMap["message"])
	}
	if errMap["module"] != "core.cloud.created" {
		t.Errorf("error.module = %v, want core.cloud.created", errMap["module"])
	}
}

// TestEmitKeeperTaskExecuted_SecretHygiene — a keeper task's task.executed
// payload does NOT contain register_data/output (keeper tasks may carry a
// vault-resolved output). A no_log failed task does NOT leak message — it's
// suppressed with the suppressed:"no_log" marker.
func TestEmitKeeperTaskExecuted_SecretHygiene(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := &Runner{deps: Deps{Audit: aw}}

	// a changed keeper task with register: — register_data still stays out of the payload.
	rtChanged := &render.RenderedTask{Index: 0, Register: "secret_out", Module: "core.vault.kv-read"}
	r.emitKeeperTaskExecuted(context.Background(), "apply-k3", 0 /*passage*/, rtChanged, true, false, "", slog.New(slog.DiscardHandler))

	// no_log failed keeper task — message is suppressed.
	rtNoLog := &render.RenderedTask{Index: 1, Module: "core.vault.kv-read", NoLog: true}
	r.emitKeeperTaskExecuted(context.Background(), "apply-k3", 0 /*passage*/, rtNoLog, false, true, "vault:secret/db plaintext", slog.New(slog.DiscardHandler))

	if len(aw.events) != 2 {
		t.Fatalf("emitted %d events, want 2", len(aw.events))
	}
	changedPayload := aw.events[0].Payload
	for _, forbidden := range []string{"register_data", "output", "params"} {
		if _, present := changedPayload[forbidden]; present {
			t.Errorf("changed keeper task.executed payload leaked %q (secret hygiene)", forbidden)
		}
	}

	noLogPayload := aw.events[1].Payload
	if noLogPayload["suppressed"] != "no_log" {
		t.Errorf("no_log payload suppressed = %v, want no_log", noLogPayload["suppressed"])
	}
	errMap, ok := noLogPayload["error"].(map[string]any)
	if !ok {
		t.Fatalf("no_log payload error type = %T, want map", noLogPayload["error"])
	}
	if _, present := errMap["message"]; present {
		t.Errorf("no_log keeper task leaked error.message = %v (must be suppressed)", errMap["message"])
	}
}

// TestEmitKeeperTaskExecuted_NilAuditNoOp — Audit=nil (unit build without
// audit) → emission is a no-op, doesn't panic.
func TestEmitKeeperTaskExecuted_NilAuditNoOp(t *testing.T) {
	r := &Runner{deps: Deps{Audit: nil}}
	r.emitKeeperTaskExecuted(context.Background(), "apply-k4", 0, /*passage*/
		&render.RenderedTask{Index: 0, Module: "core.cloud.created"}, true, false, "", slog.New(slog.DiscardHandler))
}

// TestKeeperTaskExecuted_NoRegisterButIDFoldsToChangedTask — the KEY bug
// case: a keeper task WITHOUT register but WITH id: (a typical
// provision_vm) changed → task.executed (sid=keeper, CHANGED) is emitted,
// and that (sid, task_idx) pair folds through changed_tasks into
// changed_hosts=1, total_hosts=1. Before the fix, keeper dispatch never
// emitted task.executed → the task silently dropped out of run_completed.
func TestKeeperTaskExecuted_NoRegisterButIDFoldsToChangedTask(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := &Runner{deps: Deps{Audit: aw}}
	rt := &render.RenderedTask{Index: 0, Name: "provision_vm", ID: "vm-web", Module: "core.cloud.created"}

	r.emitKeeperTaskExecuted(context.Background(), "apply-k5", 0 /*passage*/, rt, true /*changed*/, false, "", slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1 (задача без register, но с id адресуется)", len(aw.events))
	}
	if aw.events[0].Payload["status"] != "TASK_STATUS_CHANGED" {
		t.Fatalf("status = %v, want TASK_STATUS_CHANGED", aw.events[0].Payload["status"])
	}

	// Fold: the (keeper, idx=0) key from the journal + a DispatchPlan
	// keeper-target → one ChangedTask addressed by id, changed_hosts=1,
	// total_hosts=1.
	tasks := []*render.RenderedTask{rt}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}},
	}
	keys := changedKeys(auditpg.ChangedTaskKey{SID: render.KeeperTargetSID, PlanIndex: 0})

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("buildChangedTasks: got %d, want 1 (keeper changed task должна попасть)", len(got))
	}
	if got[0].ChangedHosts != 1 || got[0].TotalHosts != 1 {
		t.Errorf("changed_hosts/total_hosts = %d/%d, want 1/1", got[0].ChangedHosts, got[0].TotalHosts)
	}
	if got[0].ID != "vm-web" || got[0].Register != "" {
		t.Errorf("address = id=%q register=%q, want id=vm-web register empty", got[0].ID, got[0].Register)
	}
}
