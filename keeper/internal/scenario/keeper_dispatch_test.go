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

// fakeKeeperModule — keeper-side core-модуль-заглушка: отдаёт заранее заданный
// финальный ApplyEvent (changed/failed/output). nil eventsErr → штатный stream.
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

// fakeKeeperRegistry — KeeperModuleRegistry поверх map.
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
	tasks := []*render.RenderedTask{
		{Index: 0, Module: "core.soul.registered"},
		{Index: 1, Module: "core.exec.run"},
		{Index: 2, Module: "core.vault.kv-read"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}},
		{TaskIndex: 1, TargetSIDs: []string{"host-a"}},
		{TaskIndex: 2, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}},
	}
	got := keeperTasksOf(tasks, plans)
	if len(got) != 2 {
		t.Fatalf("keeperTasksOf: len=%d, want 2", len(got))
	}
	if got[0].Index != 0 || got[1].Index != 2 {
		t.Fatalf("keeperTasksOf order = [%d %d], want [0 2]", got[0].Index, got[1].Index)
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

// TestApplyKeeperTask_NoFinalEvent — модуль вернул nil без единого ApplyEvent
// (final==nil, applyErr==nil): отсутствие финала = аномалия контракта (как
// Soul-side), applyKeeperTask → failed с message "no final event"
// (keeper_dispatch.go ветка last==nil). Закрывает QA-пробел №4.
func TestApplyKeeperTask_NoFinalEvent(t *testing.T) {
	mod := &fakeKeeperModule{} // final=nil, applyErr=nil → Apply ничего не шлёт
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

// TestSyncTraitsOnRegistered_Gating — bind-хук релокации Trait (ADR-060 amend
// R1) фильтрует точку врезки: ТОЛЬКО успешный core.soul.registered триггерит
// проекцию. Для прочих модулей / пустого incName / nil-DB — no-op без обращения
// к БД (Runner без Deps.DB не паникует). Полная проекция доказана в integration
// (incarnation/traits_integration_test.go).
func TestSyncTraitsOnRegistered_Gating(t *testing.T) {
	r := &Runner{} // Deps.DB == nil
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Не registered-модуль → ранний выход по адресу (DB не нужна).
	r.syncTraitsOnRegistered(context.Background(), "redis-prod",
		&render.RenderedTask{Module: "core.vault.kv-read"}, log)
	r.syncTraitsOnRegistered(context.Background(), "redis-prod",
		&render.RenderedTask{Module: "core.cloud.provisioned"}, log)

	// registered, но incName пуст (прямой keeper-test без инкарнации) → no-op.
	r.syncTraitsOnRegistered(context.Background(), "",
		&render.RenderedTask{Module: "core.soul.registered"}, log)

	// registered + incName, но Deps.DB == nil → no-op (не паникует, не лезет в БД).
	r.syncTraitsOnRegistered(context.Background(), "redis-prod",
		&render.RenderedTask{Module: "core.soul.registered"}, log)

	// Бракованный адрес модуля → ранний выход (SplitModuleAddr !ok).
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

// TestKeeperTaskStatus_Mapping — маппинг исхода keeper-задачи в keeperv1-enum:
// changed→CHANGED, failed→FAILED (failed имеет приоритет над changed),
// иначе→OK. Свёртка changed (auditpg) фильтрует по строке "TASK_STATUS_CHANGED",
// рассинхрон молча обнулил бы её для keeper-задач.
func TestKeeperTaskStatus_Mapping(t *testing.T) {
	cases := []struct {
		changed, failed bool
		want            string
	}{
		{changed: true, failed: false, want: "TASK_STATUS_CHANGED"},
		{changed: false, failed: true, want: "TASK_STATUS_FAILED"},
		{changed: true, failed: true, want: "TASK_STATUS_FAILED"}, // failed побеждает
		{changed: false, failed: false, want: "TASK_STATUS_OK"},
	}
	for _, c := range cases {
		if got := keeperTaskStatus(c.changed, c.failed).String(); got != c.want {
			t.Errorf("keeperTaskStatus(changed=%v failed=%v) = %q, want %q", c.changed, c.failed, got, c.want)
		}
	}
}

// TestEmitKeeperTaskExecuted_ChangedEmits — keeper-задача с changed → task.executed
// эмитится с sid=KeeperTargetSID ("keeper"), status=TASK_STATUS_CHANGED,
// correlation_id=apply_id, source=keeper_internal. Это адрес, по которому свёртка
// changed_tasks (auditpg) и task:-подписка Tiding видят keeper-side задачи.
func TestEmitKeeperTaskExecuted_ChangedEmits(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := &Runner{deps: Deps{Audit: aw}}
	rt := &render.RenderedTask{Index: 2, Name: "provision", Register: "vm", Module: "core.cloud.created"}

	r.emitKeeperTaskExecuted(context.Background(), "apply-k1", rt, true /*changed*/, false /*failed*/, "", slog.New(slog.DiscardHandler))

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
}

// TestEmitKeeperTaskExecuted_FailedStatus — failed keeper-задача → task.executed
// status=TASK_STATUS_FAILED (НЕ CHANGED): такая задача НЕ попадёт в changed_tasks.
// error.message присутствует для не-no_log (маскинг — на write-path auditpg).
func TestEmitKeeperTaskExecuted_FailedStatus(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := &Runner{deps: Deps{Audit: aw}}
	rt := &render.RenderedTask{Index: 0, Module: "core.cloud.created"}

	r.emitKeeperTaskExecuted(context.Background(), "apply-k2", rt, false, true /*failed*/, "boom from driver", slog.New(slog.DiscardHandler))

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

// TestEmitKeeperTaskExecuted_SecretHygiene — payload task.executed keeper-задачи
// НЕ содержит register_data/output (keeper-задачи могут нести vault-резолвленный
// output). no_log failed-задача НЕ утекает message — подавляется маркером
// suppressed:"no_log".
func TestEmitKeeperTaskExecuted_SecretHygiene(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := &Runner{deps: Deps{Audit: aw}}

	// changed keeper-задача с register: — register_data всё равно НЕ в payload.
	rtChanged := &render.RenderedTask{Index: 0, Register: "secret_out", Module: "core.vault.kv-read"}
	r.emitKeeperTaskExecuted(context.Background(), "apply-k3", rtChanged, true, false, "", slog.New(slog.DiscardHandler))

	// no_log failed keeper-задача — message подавлен.
	rtNoLog := &render.RenderedTask{Index: 1, Module: "core.vault.kv-read", NoLog: true}
	r.emitKeeperTaskExecuted(context.Background(), "apply-k3", rtNoLog, false, true, "vault:secret/db plaintext", slog.New(slog.DiscardHandler))

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

// TestEmitKeeperTaskExecuted_NilAuditNoOp — Audit=nil (unit-сборка без аудита) →
// эмиссия no-op, не паникует.
func TestEmitKeeperTaskExecuted_NilAuditNoOp(t *testing.T) {
	r := &Runner{deps: Deps{Audit: nil}}
	r.emitKeeperTaskExecuted(context.Background(), "apply-k4",
		&render.RenderedTask{Index: 0, Module: "core.cloud.created"}, true, false, "", slog.New(slog.DiscardHandler))
}

// TestKeeperTaskExecuted_NoRegisterButIDFoldsToChangedTask — КЛЮЧЕВОЙ кейс бага:
// keeper-задача БЕЗ register, но с id: (типичный provision_vm) изменилась →
// task.executed (sid=keeper, CHANGED) эмитится И эта пара (sid, task_idx) через
// свёртку changed_tasks даёт changed_hosts=1, total_hosts=1. До фикса keeper-
// dispatch не эмитил task.executed → задача молча выпадала из run_completed.
func TestKeeperTaskExecuted_NoRegisterButIDFoldsToChangedTask(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := &Runner{deps: Deps{Audit: aw}}
	rt := &render.RenderedTask{Index: 0, Name: "provision_vm", ID: "vm-web", Module: "core.cloud.created"}

	r.emitKeeperTaskExecuted(context.Background(), "apply-k5", rt, true /*changed*/, false, "", slog.New(slog.DiscardHandler))

	if len(aw.events) != 1 {
		t.Fatalf("emitted %d events, want 1 (задача без register, но с id адресуется)", len(aw.events))
	}
	if aw.events[0].Payload["status"] != "TASK_STATUS_CHANGED" {
		t.Fatalf("status = %v, want TASK_STATUS_CHANGED", aw.events[0].Payload["status"])
	}

	// Свёртка: ключ (keeper, idx=0) из журнала + DispatchPlan keeper-target →
	// одна ChangedTask с адресом по id, changed_hosts=1, total_hosts=1.
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
