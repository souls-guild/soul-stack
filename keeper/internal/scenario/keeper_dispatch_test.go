package scenario

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	coremodutil "github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
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
		t.Fatalf("keeperTasksOf(0): len=%d, want 2 (keeper tasks, Passage 0)", len(got0))
	}
	if got0[0].Index != 0 || got0[1].Index != 2 {
		t.Fatalf("keeperTasksOf(0) order = [%d %d], want [0 2]", got0[0].Index, got0[1].Index)
	}

	got1 := keeperTasksOf(tasks, plans, 1)
	if len(got1) != 1 || got1[0].Index != 3 {
		t.Fatalf("keeperTasksOf(1) = %v, want exactly [idx 3] (keeper task, Passage 1)", got1)
	}

	// A Passage with no keeper tasks → empty (host-only Passage / out of range).
	if got := keeperTasksOf(tasks, plans, 2); len(got) != 0 {
		t.Fatalf("keeperTasksOf(2) = %v, want empty", got)
	}
}

func TestApplyKeeperTask_Success(t *testing.T) {
	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{
		Changed: true,
		Output:  mustStruct(t, map[string]any{"created": true, "coven": []any{"svc"}}),
	}}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.soul": mod}}

	rt := &render.RenderedTask{Index: 0, Module: "core.soul.registered", Params: mustStruct(t, map[string]any{"sid": "n1"})}
	changed, failed, output, _ := r.applyKeeperTask(context.Background(), RunSpec{}, nil, rt, nil)
	if !changed || failed {
		t.Fatalf("changed=%v failed=%v, want true/false", changed, failed)
	}
	if mod.gotState != "registered" {
		t.Fatalf("module got state %q, want registered (state suffix of the core.soul.registered address)", mod.gotState)
	}
	if output["created"] != true {
		t.Fatalf("output[created] = %v, want true", output["created"])
	}
}

// ★ GUARD (NIM-849). The run's seal must reach the MODULE, not only the masker
// on the way out. `core.ssh.run` refuses a secret in a command line rather than
// masking the aftermath — argv is visible in `ps`, `audit.log` and journald on
// the host itself, so by the time a message is masked the command has run — and
// [coremodutil.SealedPathsFrom] is the only way it can know which cell holds
// one.
//
// This is the last hop, and it is one line: drop the WithSealedPaths call in
// runKeeperTask and every guard test in the ssh package still passes, because
// those inject the context themselves. Then the refusal silently stops existing
// in production.
func TestApplyKeeperTask_SealReachesTheModule(t *testing.T) {
	sealed := map[string]bool{"steps[0].run": true}
	var got map[string]bool
	mod := &ctxProbeModule{onApply: func(ctx context.Context) {
		got = coremodutil.SealedPathsFrom(ctx)
	}}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.ssh": mod}}

	rt := &render.RenderedTask{Module: "core.ssh.run", Params: mustStruct(t, map[string]any{"hosts": []any{}})}
	r.applyKeeperTask(context.Background(), RunSpec{}, nil, rt, sealed)

	if len(got) != 1 || !got["steps[0].run"] {
		t.Fatalf("the module saw sealed paths %v, want the run's set %v", got, sealed)
	}
}

// A run that sealed nothing hands the module nothing — and that must stay
// distinguishable from "the wiring is gone", which is why the test above asserts
// the positive case rather than this one.
func TestApplyKeeperTask_NoSealIsAnEmptySet(t *testing.T) {
	var got map[string]bool
	mod := &ctxProbeModule{onApply: func(ctx context.Context) {
		got = coremodutil.SealedPathsFrom(ctx)
	}}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.ssh": mod}}

	rt := &render.RenderedTask{Module: "core.ssh.run"}
	r.applyKeeperTask(context.Background(), RunSpec{}, nil, rt, nil)

	if got != nil {
		t.Fatalf("the module saw %v, want nil", got)
	}
}

// ctxProbeModule reports what the runner put on the module context.
type ctxProbeModule struct {
	module.BaseModule
	onApply func(context.Context)
}

func (m *ctxProbeModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.onApply(stream.Context())
	return stream.Send(&pluginv1.ApplyEvent{Changed: true})
}

func TestApplyKeeperTask_FailedEvent(t *testing.T) {
	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{Failed: true, Message: "invalid coven"}}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.soul": mod}}

	_, failed, _, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, &render.RenderedTask{Module: "core.soul.registered"}, nil)
	if !failed {
		t.Fatalf("failed=false, want true")
	}
	if msg != "invalid coven" {
		t.Fatalf("message = %q, want 'invalid coven'", msg)
	}
}

func TestApplyKeeperTask_UnknownModule(t *testing.T) {
	r := &Runner{keeperModules: fakeKeeperRegistry{}}
	_, failed, _, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, &render.RenderedTask{Module: "core.soul.registered"}, nil)
	if !failed {
		t.Fatalf("failed=false, want true (module not found in Registry)")
	}
	if msg == "" {
		t.Fatalf("message empty, expected mention of unknown module")
	}
}

func TestApplyKeeperTask_ApplyError(t *testing.T) {
	mod := &fakeKeeperModule{applyErr: fmt.Errorf("ctx canceled")}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.soul": mod}}
	_, failed, _, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, &render.RenderedTask{Module: "core.soul.registered"}, nil)
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

	_, failed, output, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, &render.RenderedTask{Module: "core.soul.registered"}, nil)
	if !failed {
		t.Fatalf("failed=false, want true (module sent no final event)")
	}
	if output != nil {
		t.Errorf("output = %v, want nil", output)
	}
	if !strings.Contains(msg, "no final event") {
		t.Fatalf("message = %q, want containing 'no final event'", msg)
	}
}

func TestComposeKeeperFailure(t *testing.T) {
	rt := &render.RenderedTask{Index: 3, Module: "core.soul.registered"}
	if got := composeKeeperFailure(rt, "boom"); got != "task 3 core.soul.registered: boom" {
		t.Fatalf("composeKeeperFailure = %q", got)
	}
	// [ADR-0083] §8: no per-task suppression left — the summary is composed
	// from whatever the module reported, on every task alike.
	rtSecret := &render.RenderedTask{Index: 1, Module: "core.vault.kv-read", SecretOutput: []string{"data"}}
	if got := composeKeeperFailure(rtSecret, "boom"); got != "task 1 core.vault.kv-read: boom" {
		t.Fatalf("composeKeeperFailure with secret_output = %q", got)
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
		t.Errorf("correlation_id = %q, want apply-k1 (= apply_id, SelectChangedTaskKeys filter)", ev.CorrelationID)
	}
	if ev.Payload["sid"] != render.KeeperTargetSID {
		t.Errorf("payload sid = %v, want %q", ev.Payload["sid"], render.KeeperTargetSID)
	}
	if ev.Payload["status"] != "TASK_STATUS_CHANGED" {
		t.Errorf("payload status = %v, want TASK_STATUS_CHANGED (the fold filters by literal)", ev.Payload["status"])
	}
	if ev.Payload["task_idx"] != 2 {
		t.Errorf("payload task_idx = %v, want 2", ev.Payload["task_idx"])
	}
	if ev.Payload["passage"] != 1 {
		t.Errorf("payload passage = %v, want 1 (echo of the keeper-task Passage, Slice 2)", ev.Payload["passage"])
	}
}

// TestEmitKeeperTaskExecuted_FailedStatus — a failed keeper task →
// task.executed status=TASK_STATUS_FAILED (NOT CHANGED): such a task will
// NOT land in changed_tasks. error.message is present (masking happens on
// auditpg's write path).
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
		t.Errorf("payload status = %v, want TASK_STATUS_FAILED (not CHANGED -> not in changed_tasks)", ev.Payload["status"])
	}
	errMap, ok := ev.Payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("payload error type = %T, want map", ev.Payload["error"])
	}
	if errMap["message"] != "boom from driver" {
		t.Errorf("error.message = %v, want 'boom from driver' (message is always set)", errMap["message"])
	}
	if errMap["module"] != "core.cloud.created" {
		t.Errorf("error.module = %v, want core.cloud.created", errMap["module"])
	}
}

// TestEmitKeeperTaskExecuted_SecretHygiene — a keeper task's task.executed
// payload does NOT contain register_data/output (keeper tasks may carry a
// vault-resolved output). The exclusion is unconditional: it does not depend on
// the task declaring anything, which is what [ADR-0083] §8 replaced `no_log:`
// with.
func TestEmitKeeperTaskExecuted_SecretHygiene(t *testing.T) {
	aw := &fakeAuditWriter{}
	r := &Runner{deps: Deps{Audit: aw}}

	// a changed keeper task with register: — register_data still stays out of the payload.
	rtChanged := &render.RenderedTask{Index: 0, Register: "secret_out", Module: "core.vault.kv-read"}
	r.emitKeeperTaskExecuted(context.Background(), "apply-k3", 0 /*passage*/, rtChanged, true, false, "", slog.New(slog.DiscardHandler))

	// a failed keeper task whose module declares secret output — register_data
	// still stays out, and the summary is written as the module reported it.
	rtSecret := &render.RenderedTask{Index: 1, Module: "core.vault.kv-read", SecretOutput: []string{"data"}}
	r.emitKeeperTaskExecuted(context.Background(), "apply-k3", 0 /*passage*/, rtSecret, false, true, "boom", slog.New(slog.DiscardHandler))

	if len(aw.events) != 2 {
		t.Fatalf("emitted %d events, want 2", len(aw.events))
	}
	changedPayload := aw.events[0].Payload
	for _, forbidden := range []string{"register_data", "output", "params"} {
		if _, present := changedPayload[forbidden]; present {
			t.Errorf("changed keeper task.executed payload leaked %q (secret hygiene)", forbidden)
		}
	}

	failedPayload := aw.events[1].Payload
	if _, present := failedPayload["suppressed"]; present {
		t.Errorf("payload suppressed = %v, want the marker gone with no_log", failedPayload["suppressed"])
	}
	for _, forbidden := range []string{"register_data", "output", "params"} {
		if _, present := failedPayload[forbidden]; present {
			t.Errorf("failed keeper task.executed payload leaked %q (secret hygiene)", forbidden)
		}
	}
	errMap, ok := failedPayload["error"].(map[string]any)
	if !ok {
		t.Fatalf("failed payload error type = %T, want map", failedPayload["error"])
	}
	if errMap["message"] != "boom" {
		t.Errorf("error.message = %v, want 'boom' (no per-task suppression)", errMap["message"])
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
		t.Fatalf("emitted %d events, want 1 (task without register, but addressable by id)", len(aw.events))
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
		t.Fatalf("buildChangedTasks: got %d, want 1 (keeper changed task should land)", len(got))
	}
	if got[0].ChangedHosts != 1 || got[0].TotalHosts != 1 {
		t.Errorf("changed_hosts/total_hosts = %d/%d, want 1/1", got[0].ChangedHosts, got[0].TotalHosts)
	}
	if got[0].ID != "vm-web" || got[0].Register != "" {
		t.Errorf("address = id=%q register=%q, want id=vm-web register empty", got[0].ID, got[0].Register)
	}
}

// The §7 fence, post-render half ([ADR-0083]). `core.vault.*` reaches Vault through
// the module, so neither the CEL guard nor the authoring-time scan sees it when the
// path arrived as `${ vars.p }`. The module must not be invoked at all.
func TestApplyKeeperTask_OwnNamespaceVaultParamFenced(t *testing.T) {
	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{Changed: true}}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.vault": mod}}
	spec := RunSpec{ServiceRef: artifact.ServiceRef{Name: "redis"}}

	rt := &render.RenderedTask{Module: "core.vault.kv-read",
		Params: mustStruct(t, map[string]any{"path": "secret/redis/prod/redis_users/app"})}
	_, failed, _, msg := r.applyKeeperTask(context.Background(), spec, nil, rt, nil)
	if !failed {
		t.Fatal("the task succeeded on a rendered path in the service's own namespace")
	}
	if !strings.Contains(msg, config.VaultOwnNamespaceCode) {
		t.Fatalf("message = %q, want %s", msg, config.VaultOwnNamespaceCode)
	}
	if mod.gotState != "" {
		t.Fatalf("the module was invoked despite the fence (state %q)", mod.gotState)
	}
}

// The negative twin: a path outside the prefix reaches the module unchanged.
func TestApplyKeeperTask_CrossNamespaceVaultParamPasses(t *testing.T) {
	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{Changed: true}}
	r := &Runner{keeperModules: fakeKeeperRegistry{"core.vault": mod}}
	spec := RunSpec{ServiceRef: artifact.ServiceRef{Name: "redis"}}

	rt := &render.RenderedTask{Module: "core.vault.kv-read",
		Params: mustStruct(t, map[string]any{"path": "secret/services/shared/tls"})}
	_, failed, _, msg := r.applyKeeperTask(context.Background(), spec, nil, rt, nil)
	if failed {
		t.Fatalf("the fence fired on a cross-namespace path: %s", msg)
	}
	if mod.gotState != "kv-read" {
		t.Fatalf("module got state %q, want kv-read", mod.gotState)
	}
}
