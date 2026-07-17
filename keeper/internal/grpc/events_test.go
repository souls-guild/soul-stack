package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// fakeApplyRunDB — an applyrun.ExecQueryRower stub: a configurable QueryRow for
// SelectIncarnationByApplyID and an Exec counter for UpdateStatus.
type fakeApplyRunDB struct {
	queryRow  func() pgx.Row
	execCalls int
	execSQL   string
	execArgs  []any
	execTag   pgconn.CommandTag
}

func (f *fakeApplyRunDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls++
	f.execSQL = sql
	f.execArgs = args
	if f.execTag.String() == "" {
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return f.execTag, nil
}

func (f *fakeApplyRunDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	if f.queryRow != nil {
		return f.queryRow()
	}
	return applyRunErrRow{err: pgx.ErrNoRows}
}

// Query isn't needed by the grpc handler (RunResult correlation only goes through
// QueryRow/Exec); implemented as a no-op to satisfy applyrun.ExecQueryRower.
func (f *fakeApplyRunDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeApplyRunDB: Query not used")
}

type applyRunErrRow struct{ err error }

func (r applyRunErrRow) Scan(_ ...any) error { return r.err }

// applyRunStaticRow — returns (incarnation_name, scenario, attempt) to resolve
// SelectIncarnationByApplyID. attempt — the row's fencing epoch (gate-1 epoch-check).
type applyRunStaticRow struct {
	name, scenario string
	attempt        int32
}

func (r applyRunStaticRow) Scan(dest ...any) error {
	if len(dest) != 3 {
		return errors.New("applyRunStaticRow: want 3 dest")
	}
	*(dest[0].(*string)) = r.name
	*(dest[1].(*string)) = r.scenario
	*(dest[2].(*int32)) = r.attempt
	return nil
}

// recordingAudit — a fake [audit.Writer] that accumulates events in write order
// for checking event_type / payload fields.
type recordingAudit struct {
	mu     sync.Mutex
	events []*audit.Event
	err    error
}

func (r *recordingAudit) Write(_ context.Context, e *audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	cp := *e
	r.events = append(r.events, &cp)
	return nil
}

func (r *recordingAudit) snapshot() []*audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*audit.Event, len(r.events))
	copy(out, r.events)
	return out
}

func newTestHandler(t *testing.T, aw audit.Writer) *eventStreamHandler {
	t.Helper()
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t))
}

func TestHandleTaskEvent_WritesAuditWithStatusAndApplyID(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	ev := &keeperv1.TaskEvent{
		ApplyId: "01HABCAPPLY00000000000000",
		TaskIdx: 3,
		Status:  keeperv1.TaskStatus_TASK_STATUS_CHANGED,
		Error:   nil,
	}
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", ev)

	got := aw.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	e := got[0]
	if e.EventType != audit.EventTaskExecuted {
		t.Errorf("event_type = %q, want %q", e.EventType, audit.EventTaskExecuted)
	}
	if e.Source != audit.SourceSoulGRPC {
		t.Errorf("source = %q, want %q", e.Source, audit.SourceSoulGRPC)
	}
	if e.CorrelationID != "01HABCAPPLY00000000000000" {
		t.Errorf("correlation_id = %q, want apply_id echo", e.CorrelationID)
	}
	if e.Payload["status"] != "TASK_STATUS_CHANGED" {
		t.Errorf("payload.status = %v, want TASK_STATUS_CHANGED", e.Payload["status"])
	}
	if e.Payload["sid"] != "host.example.com" {
		t.Errorf("payload.sid = %v, want host.example.com", e.Payload["sid"])
	}
}

func TestHandleTaskEvent_FailedIncludesError(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	ev := &keeperv1.TaskEvent{
		ApplyId: "apply-1",
		TaskIdx: 0,
		Status:  keeperv1.TaskStatus_TASK_STATUS_FAILED,
		Error: &keeperv1.TaskError{
			Code: "policy_violation", Message: "blocked by side_effects", Module: "core.pkg",
		},
	}
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", ev)
	got := aw.snapshot()
	e := got[0]
	errMap, ok := e.Payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("payload.error type = %T, want map", e.Payload["error"])
	}
	if errMap["code"] != "policy_violation" {
		t.Errorf("error.code = %v, want policy_violation", errMap["code"])
	}
}

func TestHandleTaskEvent_NilPayloadDoesNothing(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", nil)
	if len(aw.snapshot()) != 0 {
		t.Error("nil event produced audit write")
	}
}

// TestHandleTaskEvent_FailedRecordsTaskFailure — BUG-3: a failed task writes
// task_idx + `task <idx> <module>: <message>` into apply_runs.error_summary,
// so the operator sees the reason instead of a bare RUN_STATUS_FAILED.
func TestHandleTaskEvent_FailedRecordsTaskFailure(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	// GUARD (ADR-056 §S1 fix Variant B): local TaskIdx=1 ≠ the global
	// PlanIndex=6 (staged/per-host-where). recordTaskFailure MUST write the
	// GLOBAL ev.PlanIndex into failed_plan_index (6th arg); task_idx is local.
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId:   "01HAPPLY",
		TaskIdx:   1,
		PlanIndex: 6,
		Status:    keeperv1.TaskStatus_TASK_STATUS_FAILED,
		Error: &keeperv1.TaskError{
			Code: "module.failed", Module: "core.pkg.installed", Message: "E: Version '7.2.4' not found",
		},
	})
	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (RecordTaskFailure)", ardb.execCalls)
	}
	if !strings.Contains(ardb.execSQL, "UPDATE apply_runs") || !strings.Contains(ardb.execSQL, "COALESCE(error_summary") ||
		!strings.Contains(ardb.execSQL, "COALESCE(failed_plan_index") {
		t.Errorf("SQL = %q, want RecordTaskFailure UPDATE (with failed_plan_index)", ardb.execSQL)
	}
	// S3 (ADR-056): RecordTaskFailure carries passage (5th arg) + failed_plan_index
	// (6th arg) — the reason is written into the (apply_id, sid, passage) row. passage comes from
	// the echoed TaskEvent.passage (0 here).
	if len(ardb.execArgs) != 6 {
		t.Fatalf("execArgs len = %d, want 6", len(ardb.execArgs))
	}
	if ardb.execArgs[2] != 1 {
		t.Errorf("task_idx arg = %v, want 1 (local)", ardb.execArgs[2])
	}
	if ardb.execArgs[4] != 0 {
		t.Errorf("passage arg = %v, want 0", ardb.execArgs[4])
	}
	if ardb.execArgs[5] != 6 {
		t.Errorf("* failed_plan_index arg = %v, want 6 (global ev.PlanIndex, not local TaskIdx=1)", ardb.execArgs[5])
	}
	want := "task 1 core.pkg.installed: E: Version '7.2.4' not found"
	if ardb.execArgs[3] != want {
		t.Errorf("summary arg = %q, want %q", ardb.execArgs[3], want)
	}
}

// TestHandleTaskEvent_TimedOutRecordsTaskFailure — TIMED_OUT (a special case of
// failed) also records the reason.
func TestHandleTaskEvent_TimedOutRecordsTaskFailure(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "01HAPPLY", TaskIdx: 2,
		Status: keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT,
		Error:  &keeperv1.TaskError{Code: "timeout", Module: "core.exec.run", Message: "deadline exceeded"},
	})
	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1", ardb.execCalls)
	}
	if ardb.execArgs[3] != "task 2 core.exec.run: deadline exceeded" {
		t.Errorf("summary = %q", ardb.execArgs[3])
	}
}

// TestHandleTaskEvent_OKDoesNotRecordFailure — a successful/changed task doesn't
// touch apply_runs.error_summary.
func TestHandleTaskEvent_OKDoesNotRecordFailure(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "01HAPPLY", TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_OK,
	})
	if ardb.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (OK does not write a failure)", ardb.execCalls)
	}
}

// TestHandleTaskEvent_FailedMasksSecretInSummary — a vault-ref in a task's message
// is masked (MaskSecrets floor) before being written to error_summary, so the secret
// doesn't leak into the operator-facing reason. This is a floor for all tasks (for no_log —
// there's additional full suppression in scenario.dispatch).
func TestHandleTaskEvent_FailedMasksSecretInSummary(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "01HAPPLY", TaskIdx: 1,
		Status: keeperv1.TaskStatus_TASK_STATUS_FAILED,
		Error: &keeperv1.TaskError{
			Code: "module.failed", Module: "core.git.cloned",
			Message: "auth failed using token vault:secret/git/deploy-key",
		},
	})
	summary, _ := ardb.execArgs[3].(string)
	if strings.Contains(summary, "vault:secret/") {
		t.Errorf("summary leaks vault-ref: %q", summary)
	}
	if !strings.Contains(summary, "***MASKED***") {
		t.Errorf("summary not masked: %q", summary)
	}
}

// TestHandleTaskEvent_NoLogSuppressesAudit — [H]-fix: for a no_log task,
// register_data (params/output) and error.message (= stderr) do NOT reach the
// long-lived audit — the root of an arbitrary secret leak past MaskSecrets. What remains
// is the non-secret sid/apply_id/task_idx/status + error.code/module and the
// suppressed:"no_log".
func TestHandleTaskEvent_NoLogSuppressesAudit(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	const secret = "S3cr3t-PlainText-Password"

	rd, err := structpb.NewStruct(map[string]any{"stdout": secret, "rc": float64(1)})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId:      "01HAPPLY",
		TaskIdx:      0,
		Status:       keeperv1.TaskStatus_TASK_STATUS_FAILED,
		NoLog:        true,
		RegisterData: rd,
		Error: &keeperv1.TaskError{
			Code: "module.failed", Module: "core.exec.run",
			Message: "command printed password=" + secret,
		},
	})

	got := aw.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	e := got[0]

	blob, _ := json.Marshal(e.Payload)
	if strings.Contains(string(blob), secret) {
		t.Errorf("no_log secret leaked into audit payload: %s", blob)
	}
	if _, present := e.Payload["register_data"]; present {
		t.Errorf("no_log task must not write register_data: %v", e.Payload)
	}
	if e.Payload["suppressed"] != "no_log" {
		t.Errorf("payload.suppressed = %v, want \"no_log\"", e.Payload["suppressed"])
	}

	errMap, ok := e.Payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("payload.error type = %T, want map", e.Payload["error"])
	}
	if _, present := errMap["message"]; present {
		t.Errorf("no_log error must not carry 'message' (stderr): %v", errMap)
	}
	if errMap["code"] != "module.failed" || errMap["module"] != "core.exec.run" {
		t.Errorf("error.code/module dropped: %v", errMap)
	}
	if e.Payload["status"] != "TASK_STATUS_FAILED" || e.Payload["apply_id"] != "01HAPPLY" {
		t.Errorf("non-secret status fields lost: %v", e.Payload)
	}
}

// TestHandleTaskEvent_NoLogFalseKeepsAudit — regression: with no_log=false, audit
// is written as before (register_data + error.message are present, no
// suppressed marker).
func TestHandleTaskEvent_NoLogFalseKeepsAudit(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)

	rd, err := structpb.NewStruct(map[string]any{"stdout": "leader"})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId:      "01HAPPLY",
		TaskIdx:      0,
		Status:       keeperv1.TaskStatus_TASK_STATUS_FAILED,
		NoLog:        false,
		RegisterData: rd,
		Error: &keeperv1.TaskError{
			Code: "module.failed", Module: "core.exec.run", Message: "boom",
		},
	})

	e := aw.snapshot()[0]
	if _, present := e.Payload["suppressed"]; present {
		t.Errorf("no_log=false must not set suppressed: %v", e.Payload)
	}
	if _, present := e.Payload["register_data"]; !present {
		t.Errorf("no_log=false must keep register_data: %v", e.Payload)
	}
	errMap, ok := e.Payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("payload.error type = %T, want map", e.Payload["error"])
	}
	if errMap["message"] != "boom" {
		t.Errorf("no_log=false must keep error.message: %v", errMap)
	}
}

func TestComposeTaskErrorSummary(t *testing.T) {
	tests := []struct {
		name string
		idx  int
		te   *keeperv1.TaskError
		want string
	}{
		{"full", 0, &keeperv1.TaskError{Module: "core.pkg.installed", Message: "boom"}, "task 0 core.pkg.installed: boom"},
		{"no module", 3, &keeperv1.TaskError{Message: "boom"}, "task 3: boom"},
		{"no message", 1, &keeperv1.TaskError{Module: "core.file.present"}, "task 1 core.file.present"},
		{"nil error", 2, nil, "task 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := composeTaskErrorSummary(tt.idx, tt.te); got != tt.want {
				t.Errorf("composeTaskErrorSummary = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleTaskEvent_AccumulatesRegister(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)

	rd, err := structpb.NewStruct(map[string]any{"stdout": "leader", "rc": float64(0)})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	ev := &keeperv1.TaskEvent{
		ApplyId:      "01HABCAPPLY00000000000000",
		PlanIndex:    5,
		TaskIdx:      2,
		Status:       keeperv1.TaskStatus_TASK_STATUS_CHANGED,
		RegisterData: rd,
	}
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", ev)

	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (upsert apply_task_register)", ardb.execCalls)
	}
	if want := "INSERT INTO apply_task_register"; !strings.Contains(ardb.execSQL, want) {
		t.Errorf("SQL = %q, want containing %q", ardb.execSQL, want)
	}
	// ADR-056 §S1 fix Variant B: the register-upsert carries the GLOBAL plan_index as the
	// correlation key ($3), local task_idx as data ($4), passage as a component of the
	// FK on apply_runs(apply_id, sid, passage) ($6). args: apply_id, sid, plan_index,
	// task_idx, register_data, passage.
	if len(ardb.execArgs) != 6 {
		t.Fatalf("execArgs len = %d, want 6", len(ardb.execArgs))
	}
	if ardb.execArgs[0] != "01HABCAPPLY00000000000000" || ardb.execArgs[1] != "host.example.com" || ardb.execArgs[2] != 5 || ardb.execArgs[3] != 2 {
		t.Errorf("args[0..3] = %v / %v / %v / %v", ardb.execArgs[0], ardb.execArgs[1], ardb.execArgs[2], ardb.execArgs[3])
	}
}

func TestHandleTaskEvent_NoRegisterData_NoAccumulate(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "a", TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_OK,
	})
	if ardb.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (no register_data)", ardb.execCalls)
	}
}

func TestHandleTaskEvent_NilApplyRunDB_NoAccumulate(t *testing.T) {
	// ApplyRunDB=nil → accumulateRegister no-op; audit without a panic.
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	rd, err := structpb.NewStruct(map[string]any{"stdout": "x"})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "a", TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED, RegisterData: rd,
	})
	if len(aw.snapshot()) != 1 {
		t.Errorf("audit events = %d, want 1 (accumulate no-op must not interfere with audit)", len(aw.snapshot()))
	}
}

func TestHandleRunResult_WritesAuditWithStatus(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	ev := &keeperv1.RunResult{
		ApplyId: "01HABCAPPLY00000000000000",
		Status:  keeperv1.RunStatus_RUN_STATUS_SUCCESS,
	}
	h.handleRunResult(context.Background(), "host.example.com", "session-1", ev)

	got := aw.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	e := got[0]
	if e.EventType != audit.EventRunCompleted {
		t.Errorf("event_type = %q, want %q", e.EventType, audit.EventRunCompleted)
	}
	if e.CorrelationID != "01HABCAPPLY00000000000000" {
		t.Errorf("correlation_id = %q, want apply_id echo", e.CorrelationID)
	}
	if e.Payload["status"] != "RUN_STATUS_SUCCESS" {
		t.Errorf("status = %v, want RUN_STATUS_SUCCESS", e.Payload["status"])
	}
}

func TestHandleRunResult_NilPayloadDoesNothing(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", nil)
	if len(aw.snapshot()) != 0 {
		t.Error("nil event produced audit write")
	}
}

// newTestHandlerWithApplyRun — a handler with a fake apply_runs DB wired up.
func newTestHandlerWithApplyRun(t *testing.T, aw audit.Writer, ardb applyrun.ExecQueryRower) *eventStreamHandler {
	t.Helper()
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		ApplyRunDB:  ardb,
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t))
}

// newTestHandlerWithBus builds a handler with ApplyBus wired up, to
// verify the SSE publish payload (publishTaskExecuted) isolates stderr.
func newTestHandlerWithBus(t *testing.T, aw audit.Writer, bus *applybus.EventBus) *eventStreamHandler {
	t.Helper()
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		ApplyBus:    bus,
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t))
}

// collectSSE subscribes to applyID and collects one published Event.
// Returns (event, ok); ok=false on timeout.
func collectSSE(t *testing.T, bus *applybus.EventBus, applyID string, publish func()) (applybus.Event, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := bus.Subscribe(ctx, applyID)

	deadline := time.Now().Add(2 * time.Second)
	for bus.Subscribers(applyID) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	publish()

	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(2 * time.Second):
		return applybus.Event{}, false
	}
}

// TestPublishTaskExecuted_FailedOmitsRawStderr — BUG-3 floor: a failed task
// does NOT place raw stderr (TaskError.Message) into the SSE payload. Even if message
// carries a no_log task's plaintext secret that MaskSecrets can't catch by
// vault-ref, it's absent from the published frame. The error block only carries code/module.
func TestPublishTaskExecuted_FailedOmitsRawStderr(t *testing.T) {
	bus := applybus.NewBus(discardLogger(t))
	h := newTestHandlerWithBus(t, &recordingAudit{}, bus)
	const secret = "S3cr3t-PlainText-Password"

	ev, ok := collectSSE(t, bus, "01HAPPLY", func() {
		h.publishTaskExecuted("host.example.com", &keeperv1.TaskEvent{
			ApplyId: "01HAPPLY", TaskIdx: 0,
			Status: keeperv1.TaskStatus_TASK_STATUS_FAILED,
			Error: &keeperv1.TaskError{
				Code: "module.failed", Module: "core.exec.run",
				Message: "command printed password=" + secret,
			},
		})
	})
	if !ok {
		t.Fatal("no SSE event published")
	}
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", ev.Payload)
	}

	blob, _ := json.Marshal(payload)
	if strings.Contains(string(blob), secret) {
		t.Errorf("raw stderr secret leaked into SSE payload: %s", blob)
	}
	if strings.Contains(string(blob), "password=") {
		t.Errorf("raw stderr body leaked into SSE payload: %s", blob)
	}

	errMap, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("payload.error type = %T, want map", payload["error"])
	}
	if _, present := errMap["message"]; present {
		t.Errorf("SSE error must not carry 'message' (stderr floor): %v", errMap)
	}
	if errMap["code"] != "module.failed" {
		t.Errorf("error.code = %v, want module.failed", errMap["code"])
	}
	if errMap["module"] != "core.exec.run" {
		t.Errorf("error.module = %v, want core.exec.run", errMap["module"])
	}
	if payload["task_status"] != "TASK_STATUS_FAILED" {
		t.Errorf("task_status = %v, want TASK_STATUS_FAILED", payload["task_status"])
	}
}

// TestPublishTaskExecuted_OKHasNoError — a successful task is published without
// an error block (useful status fields aren't lost).
func TestPublishTaskExecuted_OKHasNoError(t *testing.T) {
	bus := applybus.NewBus(discardLogger(t))
	h := newTestHandlerWithBus(t, &recordingAudit{}, bus)

	ev, ok := collectSSE(t, bus, "01HAPPLY", func() {
		h.publishTaskExecuted("host.example.com", &keeperv1.TaskEvent{
			ApplyId: "01HAPPLY", TaskIdx: 2,
			Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED,
		})
	})
	if !ok {
		t.Fatal("no SSE event published")
	}
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", ev.Payload)
	}
	if _, present := payload["error"]; present {
		t.Errorf("OK/changed task must not carry error: %v", payload)
	}
	if payload["task_status"] != "TASK_STATUS_CHANGED" {
		t.Errorf("task_status = %v, want TASK_STATUS_CHANGED", payload["task_status"])
	}
	if payload["sid"] != "host.example.com" {
		t.Errorf("sid = %v, want host.example.com", payload["sid"])
	}
}

func TestHandleRunResult_CorrelatesAndUpdatesApplyRun_Success(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{
		queryRow: func() pgx.Row { return applyRunStaticRow{name: "redis-prod", scenario: "scale"} },
	}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS,
	})
	if ardb.execCalls != 1 {
		t.Fatalf("apply_runs Exec calls = %d, want 1 (UpdateStatus)", ardb.execCalls)
	}
	// args: [apply_id, sid, status, error_summary].
	if ardb.execArgs[2] != "success" {
		t.Errorf("status arg = %v, want success", ardb.execArgs[2])
	}
	if ardb.execArgs[3] != nil {
		t.Errorf("error_summary arg = %v, want nil on success", ardb.execArgs[3])
	}
}

func TestHandleRunResult_FailedPreservesPerTaskSummary(t *testing.T) {
	// BUG-3: the RunResult handler does NOT overwrite error_summary — the reason is already
	// recorded per-task (recordTaskFailure). UpdateStatus receives nil →
	// COALESCE preserves `task <idx> <module>: <message>`, without overwriting it with a bare
	// `run_status=...`.
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{
		queryRow: func() pgx.Row { return applyRunStaticRow{name: "redis-prod", scenario: "scale"} },
	}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_FAILED,
	})
	if ardb.execArgs[2] != "failed" {
		t.Errorf("status arg = %v, want failed", ardb.execArgs[2])
	}
	if ardb.execArgs[3] != nil {
		t.Errorf("error_summary arg = %v, want nil (per-task summary is not overwritten)", ardb.execArgs[3])
	}
}

func TestHandleRunResult_CancelledMapsToCancelled(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{
		queryRow: func() pgx.Row { return applyRunStaticRow{name: "redis-prod", scenario: "scale"} },
	}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_CANCELLED,
	})
	if ardb.execArgs[2] != "cancelled" {
		t.Errorf("status arg = %v, want cancelled", ardb.execArgs[2])
	}
}

// newTestHandlerWithApplyRunMetrics — a handler with an apply_runs DB and registered
// keeper_grpc_* metrics (needed by epoch-check tests that scrape
// keeper_runresult_stale_total). Returns the handler and registry for scraping.
func newTestHandlerWithApplyRunMetrics(t *testing.T, aw audit.Writer, ardb applyrun.ExecQueryRower) (*eventStreamHandler, *obs.Registry) {
	t.Helper()
	reg := obs.NewRegistry()
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		ApplyRunDB:  ardb,
		Metrics:     RegisterGRPCMetrics(reg),
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t)), reg
}

// TestCorrelateRunResult_EqualAttemptCommits — gate-1: recvAttempt == rowAttempt
// → a current result, UpdateStatus is called (commit), the stale metric doesn't grow.
func TestCorrelateRunResult_EqualAttemptCommits(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{
		queryRow: func() pgx.Row { return applyRunStaticRow{name: "redis-prod", scenario: "scale", attempt: 3} },
	}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, Attempt: 3,
	})
	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (UpdateStatus on equal attempt)", ardb.execCalls)
	}
	if body := obstest.Scrape(t, reg.Gatherer()); strings.Contains(body, "keeper_runresult_stale_total 1") {
		t.Errorf("stale_total must stay 0 on equal attempt; got=\n%s", body)
	}
}

// TestCorrelateRunResult_StaleAttemptDropped — gate-1: recvAttempt < rowAttempt →
// a result from a stale attempt, UpdateStatus is NOT called, the metric is incremented.
func TestCorrelateRunResult_StaleAttemptDropped(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{
		queryRow: func() pgx.Row { return applyRunStaticRow{name: "redis-prod", scenario: "scale", attempt: 5} },
	}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, Attempt: 2,
	})
	if ardb.execCalls != 0 {
		t.Fatalf("execCalls = %d, want 0 (commit dropped on stale attempt)", ardb.execCalls)
	}
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "keeper_runresult_stale_total 1") {
		t.Errorf("stale_total must be 1 on stale attempt; got=\n%s", body)
	}
	// audit + SSE-publish happen BEFORE correlate — receipt is recorded even on stale.
	if len(aw.snapshot()) != 1 {
		t.Errorf("audit events = %d, want 1 (run.completed is written before correlate)", len(aw.snapshot()))
	}
}

// TestCorrelateRunResult_ZeroAttemptForwardCompat — gate-1: recvAttempt == 0
// (an old Soul without echo) → forward-compat, the currency check is NOT applied,
// commit goes through even with a nonzero rowAttempt.
func TestCorrelateRunResult_ZeroAttemptForwardCompat(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{
		queryRow: func() pgx.Row { return applyRunStaticRow{name: "redis-prod", scenario: "scale", attempt: 7} },
	}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, // Attempt not set → 0
	})
	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (forward-compat commit on attempt=0)", ardb.execCalls)
	}
	if body := obstest.Scrape(t, reg.Gatherer()); strings.Contains(body, "keeper_runresult_stale_total 1") {
		t.Errorf("stale_total must stay 0 on forward-compat attempt=0; got=\n%s", body)
	}
}

// TestCorrelateRunResult_GreaterAttemptCommitsFailSafe — gate-1 defensive: recvAttempt >
// rowAttempt — an impossible invariant (attempt only grows on claim). The
// fail-safe branch: warn log + commit anyway (UpdateStatus is called), the stale metric is NOT
// incremented — we don't lose the result of a live run due to an epoch desync.
func TestCorrelateRunResult_GreaterAttemptCommitsFailSafe(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{
		queryRow: func() pgx.Row { return applyRunStaticRow{name: "redis-prod", scenario: "scale", attempt: 3} },
	}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, Attempt: 5,
	})
	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (fail-safe commit on recvAttempt>rowAttempt)", ardb.execCalls)
	}
	if body := obstest.Scrape(t, reg.Gatherer()); strings.Contains(body, "keeper_runresult_stale_total 1") {
		t.Errorf("stale_total must stay 0 on recvAttempt>rowAttempt (commit, not stale-drop); got=\n%s", body)
	}
}

func TestHandleRunResult_ApplyRunNotFound_LogSkip(t *testing.T) {
	// QueryRow → ErrNoRows → SelectIncarnationByApplyID returns NotFound.
	// Correlation skipped, UpdateStatus is NOT called; audit still happens.
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{} // default → ErrNoRows
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HORPHAN", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS,
	})
	if ardb.execCalls != 0 {
		t.Errorf("Exec calls = %d, want 0 (UpdateStatus skipped on not-found)", ardb.execCalls)
	}
	if len(aw.snapshot()) != 1 {
		t.Errorf("audit events = %d, want 1 (run.completed still written)", len(aw.snapshot()))
	}
}

func TestHandleRunResult_NilApplyRunDB_NoCorrelation(t *testing.T) {
	// ApplyRunDB=nil → correlateRunResult no-op; audit + publish without a panic.
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS,
	})
	if len(aw.snapshot()) != 1 {
		t.Errorf("audit events = %d, want 1", len(aw.snapshot()))
	}
}

func TestHandleRunResult_StateChangesSerialized(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	sc, _ := structpb.NewStruct(map[string]any{"replicas": 3.0})
	ev := &keeperv1.RunResult{
		ApplyId:      "apply-1",
		Status:       keeperv1.RunStatus_RUN_STATUS_SUCCESS,
		StateChanges: sc,
	}
	h.handleRunResult(context.Background(), "host.example.com", "session-1", ev)
	e := aw.snapshot()[0]
	scStr, ok := e.Payload["state_changes"].(string)
	if !ok || scStr == "" {
		t.Fatalf("state_changes payload = %v, want non-empty JSON string", e.Payload["state_changes"])
	}
}

func TestHandleSoulprintReport_WritesAuditOnly_WhenSoulDBNil(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	collected := time.Now().UTC().Add(-30 * time.Second)
	ev := &keeperv1.SoulprintReport{
		CollectedAt: timestamppb.New(collected),
		TypedFacts: &keeperv1.SoulprintFacts{
			Sid: "host.example.com", Hostname: "host",
			Os: &keeperv1.OsFacts{Family: "debian", Distro: "ubuntu", Version: "22.04"},
		},
	}
	h.handleSoulprintReport(context.Background(), "host.example.com", "session-1", ev)

	got := aw.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	e := got[0]
	if e.EventType != audit.EventSoulprintReceived {
		t.Errorf("event_type = %q, want %q", e.EventType, audit.EventSoulprintReceived)
	}
	if e.Payload["has_typed_facts"] != true {
		t.Errorf("has_typed_facts = %v, want true", e.Payload["has_typed_facts"])
	}
	if e.Payload["sid"] != "host.example.com" {
		t.Errorf("sid = %v, want host.example.com", e.Payload["sid"])
	}
}

func TestHandleSoulprintReport_NilPayloadDoesNothing(t *testing.T) {
	aw := &recordingAudit{}
	h := newTestHandler(t, aw)
	h.handleSoulprintReport(context.Background(), "host.example.com", "session-1", nil)
	if len(aw.snapshot()) != 0 {
		t.Error("nil event produced audit write")
	}
}

// TestSoulprintFactsMarshaler_SnakeCaseKeys pins down E2E BUG-A: the soulprint
// projection into the render context uses snake_case composite keys
// (pkg_mgr/init_system/primary_ip), the canon per ADR-018 / templating.md §3.2
// (.self.<path> in text/template ≡ soulprint.self.<path> in CEL — a single source
// of truth). jsonName camelCase (pkgMgr/initSystem/primaryIp) is not acceptable —
// the template `{{ .self.os.pkg_mgr }}` would fail with "map has no entry".
func TestSoulprintFactsMarshaler_SnakeCaseKeys(t *testing.T) {
	tf := &keeperv1.SoulprintFacts{
		Sid:      "host.example.com",
		Hostname: "host",
		Os:       &keeperv1.OsFacts{Family: "debian", Distro: "ubuntu", PkgMgr: "apt", InitSystem: "systemd"},
		Network:  &keeperv1.NetworkFacts{PrimaryIp: "10.0.0.7"},
	}
	b, err := soulprintFactsMarshaler.Marshal(tf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	os, _ := got["os"].(map[string]any)
	if os == nil {
		t.Fatalf("os section missing: %s", b)
	}
	if os["pkg_mgr"] != "apt" {
		t.Errorf("os.pkg_mgr = %v, want apt (snake_case canon)", os["pkg_mgr"])
	}
	if os["init_system"] != "systemd" {
		t.Errorf("os.init_system = %v, want systemd (snake_case canon)", os["init_system"])
	}
	if _, ok := os["pkgMgr"]; ok {
		t.Errorf("camelCase key pkgMgr present -- out of sync with CEL/docs: %s", b)
	}
	if _, ok := os["initSystem"]; ok {
		t.Errorf("camelCase key initSystem present -- out of sync with CEL/docs: %s", b)
	}

	net, _ := got["network"].(map[string]any)
	if net == nil || net["primary_ip"] != "10.0.0.7" {
		t.Errorf("network.primary_ip = %v, want 10.0.0.7 (snake_case canon): %s", net, b)
	}
	if _, ok := net["primaryIp"]; ok {
		t.Errorf("camelCase key primaryIp present -- out of sync with CEL/docs: %s", b)
	}
}

func TestHandleTaskEvent_AuditWriterErrorIsNotFatal(t *testing.T) {
	aw := &recordingAudit{err: errors.New("db down")}
	h := newTestHandler(t, aw)
	// Must not panic; a warning in the logs is enough (we're checking there was no
	// panic by calling through to return).
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "apply-x", Status: keeperv1.TaskStatus_TASK_STATUS_OK,
	})
}

// --- staged-render Passage backward-compat (★ guard, ADR-056 S1) ---
//
// S1 invariant: a single-passage run (passage=0, including an old Soul without the field —
// proto default 0) goes through correlation/register/audit BIT-FOR-BIT as before
// staged-render. We prove: an explicit passage=0 and an omitted passage give an identical
// result, correlation by (apply_id, sid) doesn't change, and passage=0 is echoed into the
// observability payload. The multi-passage cycle (passage>0) is S2/S3.

// TestHandleRunResult_Passage0_CorrelatesIdentically — a RunResult with passage=0
// (and with passage omitted) correlates with the (apply_id, sid, passage=0) row —
// the same UpdateStatus as an N=1 run before staged-render (S3 ADR-056): passage=0
// hits the same single host row (data-level BIT-FOR-BIT). passage=0 goes into the
// audit payload for per-Passage triage.
func TestHandleRunResult_Passage0_CorrelatesIdentically(t *testing.T) {
	run := func(t *testing.T, ev *keeperv1.RunResult) (*fakeApplyRunDB, *recordingAudit) {
		t.Helper()
		aw := &recordingAudit{}
		ardb := &fakeApplyRunDB{
			queryRow: func() pgx.Row { return applyRunStaticRow{name: "redis-prod", scenario: "scale"} },
		}
		h := newTestHandlerWithApplyRun(t, aw, ardb)
		h.handleRunResult(context.Background(), "host.example.com", "session-1", ev)
		return ardb, aw
	}

	explicit, awExplicit := run(t, &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, Passage: 0,
	})
	// Omitted passage (an old Soul without the field) — proto default 0, the identical path.
	omitted, _ := run(t, &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS,
	})

	// Correlation: exactly one UpdateStatus, WHERE by (apply_id, sid, passage) with
	// passage=0 (5th arg). An N=1 run sends passage=0 → hits the same single
	// host row as before staged-render (data-level BIT-FOR-BIT).
	for name, ardb := range map[string]*fakeApplyRunDB{"explicit": explicit, "omitted": omitted} {
		if ardb.execCalls != 1 {
			t.Fatalf("%s: UpdateStatus execCalls = %d, want 1", name, ardb.execCalls)
		}
		if !strings.Contains(ardb.execSQL, "WHERE apply_id = $1 AND sid = $2 AND passage = $5") {
			t.Errorf("%s: correlation WHERE is not passage-aware (S3 ADR-056): %q", name, ardb.execSQL)
		}
		if len(ardb.execArgs) != 5 {
			t.Errorf("%s: execArgs len = %d, want 5 (passage in WHERE on S3)", name, len(ardb.execArgs))
		}
		if ardb.execArgs[2] != "success" {
			t.Errorf("%s: status arg = %v, want success", name, ardb.execArgs[2])
		}
		if ardb.execArgs[4] != 0 {
			t.Errorf("%s: passage arg = %v, want 0", name, ardb.execArgs[4])
		}
	}

	// passage=0 is echoed into audit run.completed for per-Passage triage (foundation).
	got := awExplicit.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	if p, ok := got[0].Payload["passage"]; !ok || p != int32(0) {
		t.Errorf("run.completed payload passage = %v (ok=%v), want int32(0)", p, ok)
	}
}

// TestHandleTaskEvent_Passage0_AccumulatesIdentically — a TaskEvent with passage=0
// (N=1, plan_index==task_idx) accumulates register the same as before staged-render: the
// correlation key plan_index ($3) equals task_idx, passage=0 is a component of the FK on
// apply_runs (ADR-056 §S1 fix Variant B; migration 079). passage=0 goes into the
// task.executed payload.
func TestHandleTaskEvent_Passage0_AccumulatesIdentically(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)

	rd, err := structpb.NewStruct(map[string]any{"stdout": "leader"})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	// N=1: an old Soul without plan_index sends 0; a new Soul echoes plan_index==task_idx.
	// We emulate a new Soul's echo (plan_index=task_idx=2) — behavior identical to N=1.
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "01HABCAPPLY00000000000000", PlanIndex: 2, TaskIdx: 2,
		Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED, RegisterData: rd, Passage: 0,
	})

	// upsert apply_task_register (ADR-056 §S1 fix Variant B): 6 arguments
	// (apply_id, sid, plan_index, task_idx, register_data, passage). The
	// correlation key is plan_index; passage=0 is a component of the FK on apply_runs.
	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (upsert apply_task_register)", ardb.execCalls)
	}
	if !strings.Contains(ardb.execSQL, "INSERT INTO apply_task_register") {
		t.Errorf("SQL = %q, want INSERT INTO apply_task_register", ardb.execSQL)
	}
	if len(ardb.execArgs) != 6 {
		t.Fatalf("execArgs len = %d, want 6 (plan_index + passage in register-upsert)", len(ardb.execArgs))
	}
	if ardb.execArgs[0] != "01HABCAPPLY00000000000000" || ardb.execArgs[1] != "host.example.com" || ardb.execArgs[2] != 2 || ardb.execArgs[3] != 2 {
		t.Errorf("args[0..3] = %v / %v / %v / %v", ardb.execArgs[0], ardb.execArgs[1], ardb.execArgs[2], ardb.execArgs[3])
	}
	if ardb.execArgs[5] != 0 {
		t.Errorf("passage arg = %v, want 0", ardb.execArgs[5])
	}

	// passage=0 is echoed into the task.executed payload.
	got := aw.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	if p, ok := got[0].Payload["passage"]; !ok || p != 0 {
		t.Errorf("task.executed payload passage = %v (ok=%v), want 0", p, ok)
	}
}
