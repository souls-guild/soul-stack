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

// fakeApplyRunDB — applyrun.ExecQueryRower-stub: настраиваемый QueryRow для
// SelectIncarnationByApplyID и счётчик Exec для UpdateStatus.
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

// Query — grpc-handler-у не нужен (RunResult-correlation ходит только через
// QueryRow/Exec); реализуем no-op-ом ради соответствия applyrun.ExecQueryRower.
func (f *fakeApplyRunDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeApplyRunDB: Query not used")
}

type applyRunErrRow struct{ err error }

func (r applyRunErrRow) Scan(_ ...any) error { return r.err }

// applyRunStaticRow — отдаёт (incarnation_name, scenario, attempt) для резолва
// SelectIncarnationByApplyID. attempt — fencing-epoch строки (gate-1 epoch-check).
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

// recordingAudit — fake [audit.Writer], копит события в порядке записи
// для проверки event_type / payload-полей.
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

// TestHandleTaskEvent_FailedRecordsTaskFailure — BUG-3: упавшая задача пишет
// task_idx + `task <idx> <module>: <message>` в apply_runs.error_summary,
// чтобы оператор видел причину, а не голый RUN_STATUS_FAILED.
func TestHandleTaskEvent_FailedRecordsTaskFailure(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	// GUARD (ADR-056 §S1 fix Variant B): локальный TaskIdx=1 ≠ глобальный
	// PlanIndex=6 (staged/per-host-where). recordTaskFailure ОБЯЗАН писать в
	// failed_plan_index ГЛОБАЛЬНЫЙ ev.PlanIndex (6-й арг), task_idx — локальный.
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
		t.Errorf("SQL = %q, want RecordTaskFailure UPDATE (с failed_plan_index)", ardb.execSQL)
	}
	// S3 (ADR-056): RecordTaskFailure несёт passage (5-й арг) + failed_plan_index
	// (6-й арг) — причина пишется в строку (apply_id, sid, passage). passage из
	// эхо TaskEvent.passage (тут 0).
	if len(ardb.execArgs) != 6 {
		t.Fatalf("execArgs len = %d, want 6", len(ardb.execArgs))
	}
	if ardb.execArgs[2] != 1 {
		t.Errorf("task_idx arg = %v, want 1 (локальный)", ardb.execArgs[2])
	}
	if ardb.execArgs[4] != 0 {
		t.Errorf("passage arg = %v, want 0", ardb.execArgs[4])
	}
	if ardb.execArgs[5] != 6 {
		t.Errorf("★ failed_plan_index arg = %v, want 6 (глобальный ev.PlanIndex, не локальный TaskIdx=1)", ardb.execArgs[5])
	}
	want := "task 1 core.pkg.installed: E: Version '7.2.4' not found"
	if ardb.execArgs[3] != want {
		t.Errorf("summary arg = %q, want %q", ardb.execArgs[3], want)
	}
}

// TestHandleTaskEvent_TimedOutRecordsTaskFailure — TIMED_OUT (частный случай
// failed) тоже фиксирует причину.
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

// TestHandleTaskEvent_OKDoesNotRecordFailure — успешная/changed задача не
// трогает apply_runs.error_summary.
func TestHandleTaskEvent_OKDoesNotRecordFailure(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "01HAPPLY", TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_OK,
	})
	if ardb.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (OK не пишет failure)", ardb.execCalls)
	}
}

// TestHandleTaskEvent_FailedMasksSecretInSummary — vault-ref в message задачи
// маскируется (MaskSecrets-floor) перед записью в error_summary, чтобы секрет
// не утёк в operator-facing причину. Это floor для всех задач (для no_log —
// дополнительное полное подавление в scenario.dispatch).
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

// TestHandleTaskEvent_NoLogSuppressesAudit — [H]-фикс: для no_log-задачи в
// долгоживущий audit НЕ попадают register_data (params/output) и error.message
// (= stderr) — корень утечки произвольного секрета мимо MaskSecrets. Остаются
// несекретные sid/apply_id/task_idx/status + error.code/module и маркер
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
		t.Errorf("несекретные поля статуса потеряны: %v", e.Payload)
	}
}

// TestHandleTaskEvent_NoLogFalseKeepsAudit — регресс: при no_log=false audit
// пишется как раньше (register_data + error.message присутствуют, маркера
// suppressed нет).
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
		{"полный", 0, &keeperv1.TaskError{Module: "core.pkg.installed", Message: "boom"}, "task 0 core.pkg.installed: boom"},
		{"без модуля", 3, &keeperv1.TaskError{Message: "boom"}, "task 3: boom"},
		{"без message", 1, &keeperv1.TaskError{Module: "core.file.present"}, "task 1 core.file.present"},
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
		t.Errorf("SQL = %q, want содержащий %q", ardb.execSQL, want)
	}
	// ADR-056 §S1 fix Variant B: register-upsert несёт ГЛОБАЛЬНЫЙ plan_index как
	// ключ корреляции ($3), локальный task_idx — данными ($4), passage — компонент
	// FK на apply_runs(apply_id, sid, passage) ($6). args: apply_id, sid, plan_index,
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
		t.Errorf("execCalls = %d, want 0 (нет register_data)", ardb.execCalls)
	}
}

func TestHandleTaskEvent_NilApplyRunDB_NoAccumulate(t *testing.T) {
	// ApplyRunDB=nil → accumulateRegister no-op; audit без паники.
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
		t.Errorf("audit events = %d, want 1 (accumulate no-op не должен мешать audit)", len(aw.snapshot()))
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

// newTestHandlerWithApplyRun — handler с подключённым fake apply_runs DB.
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

// newTestHandlerWithBus собирает handler с подключённым ApplyBus, чтобы
// проверить SSE-publish-payload (publishTaskExecuted) на изоляцию stderr.
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

// collectSSE подписывается на applyID и собирает один опубликованный Event.
// Возвращает (event, ok); ok=false при таймауте.
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

// TestPublishTaskExecuted_FailedOmitsRawStderr — BUG-3 floor: упавшая задача
// НЕ кладёт сырой stderr (TaskError.Message) в SSE-payload. Даже если message
// несёт plaintext-секрет no_log-задачи, который MaskSecrets по vault-ref не
// ловит, его в опубликованном фрейме нет. error-блок несёт только code/module.
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

// TestPublishTaskExecuted_OKHasNoError — успешная задача публикуется без
// error-блока (полезные поля статуса не теряются).
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
	// BUG-3: RunResult-handler НЕ перезаписывает error_summary — причина уже
	// записана per-task-ом (recordTaskFailure). UpdateStatus получает nil →
	// COALESCE сохраняет `task <idx> <module>: <message>`, не затирая голым
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
		t.Errorf("error_summary arg = %v, want nil (per-task summary не перезаписывается)", ardb.execArgs[3])
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

// newTestHandlerWithApplyRunMetrics — handler с apply_runs DB и зарегистрированными
// keeper_grpc_*-метриками (нужен epoch-check-тестам, скрейпящим
// keeper_runresult_stale_total). Возвращает handler и registry для scrape.
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
// → актуальный результат, UpdateStatus вызывается (commit), stale-метрика не растёт.
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
// результат от устаревшей попытки, UpdateStatus НЕ вызывается, метрика инкрементнута.
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
	// audit + SSE-publish идут ДО correlate — факт приёма фиксируется даже на stale.
	if len(aw.snapshot()) != 1 {
		t.Errorf("audit events = %d, want 1 (run.completed пишется до correlate)", len(aw.snapshot()))
	}
}

// TestCorrelateRunResult_ZeroAttemptForwardCompat — gate-1: recvAttempt == 0
// (старый Soul без эхо) → forward-compat, проверка актуальности НЕ применяется,
// commit проходит даже при ненулевом rowAttempt.
func TestCorrelateRunResult_ZeroAttemptForwardCompat(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{
		queryRow: func() pgx.Row { return applyRunStaticRow{name: "redis-prod", scenario: "scale", attempt: 7} },
	}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)
	h.handleRunResult(context.Background(), "host.example.com", "session-1", &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, // Attempt не задан → 0
	})
	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (forward-compat commit on attempt=0)", ardb.execCalls)
	}
	if body := obstest.Scrape(t, reg.Gatherer()); strings.Contains(body, "keeper_runresult_stale_total 1") {
		t.Errorf("stale_total must stay 0 on forward-compat attempt=0; got=\n%s", body)
	}
}

// TestCorrelateRunResult_GreaterAttemptCommitsFailSafe — gate-1 defensive: recvAttempt >
// rowAttempt — невозможный инвариант (attempt растёт только вверх при claim). Ветка
// fail-safe: warn-лог + всё равно commit (UpdateStatus вызывается), stale-метрика НЕ
// инкрементнута — результат живого прогона не теряем из-за рассинхрона epoch-а.
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
		t.Fatalf("execCalls = %d, want 1 (fail-safe commit при recvAttempt>rowAttempt)", ardb.execCalls)
	}
	if body := obstest.Scrape(t, reg.Gatherer()); strings.Contains(body, "keeper_runresult_stale_total 1") {
		t.Errorf("stale_total must stay 0 on recvAttempt>rowAttempt (commit, not stale-drop); got=\n%s", body)
	}
}

func TestHandleRunResult_ApplyRunNotFound_LogSkip(t *testing.T) {
	// QueryRow → ErrNoRows → SelectIncarnationByApplyID returns NotFound.
	// Correlation skipped, UpdateStatus НЕ вызывается; audit всё равно есть.
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
	// ApplyRunDB=nil → correlateRunResult no-op; audit + publish без паники.
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

// TestSoulprintFactsMarshaler_SnakeCaseKeys фиксирует E2E BUG-A: проекция
// soulprint в render-контекст использует snake_case composite-ключи
// (pkg_mgr/init_system/primary_ip), канон ADR-018 / templating.md §3.2
// (.self.<path> в text/template ≡ soulprint.self.<path> в CEL — единая точка
// правды). jsonName camelCase (pkgMgr/initSystem/primaryIp) недопустим —
// шаблон `{{ .self.os.pkg_mgr }}` падал бы «map has no entry».
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
		t.Errorf("camelCase key pkgMgr present — рассинхрон с CEL/docs: %s", b)
	}
	if _, ok := os["initSystem"]; ok {
		t.Errorf("camelCase key initSystem present — рассинхрон с CEL/docs: %s", b)
	}

	net, _ := got["network"].(map[string]any)
	if net == nil || net["primary_ip"] != "10.0.0.7" {
		t.Errorf("network.primary_ip = %v, want 10.0.0.7 (snake_case canon): %s", net, b)
	}
	if _, ok := net["primaryIp"]; ok {
		t.Errorf("camelCase key primaryIp present — рассинхрон с CEL/docs: %s", b)
	}
}

func TestHandleTaskEvent_AuditWriterErrorIsNotFatal(t *testing.T) {
	aw := &recordingAudit{err: errors.New("db down")}
	h := newTestHandler(t, aw)
	// Не должно паниковать; warn в логах достаточно (проверяем что не было
	// panic-а вызовом до возврата).
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "apply-x", Status: keeperv1.TaskStatus_TASK_STATUS_OK,
	})
}

// --- staged-render Passage backward-compat (★ guard, ADR-056 S1) ---
//
// S1-инвариант: single-passage прогон (passage=0, в т.ч. старый Soul без поля —
// proto-дефолт 0) проходит correlation/register/audit БИТ-В-БИТ как до
// staged-render. Доказываем: явный passage=0 и опущенный passage дают идентичный
// результат, корреляция по (apply_id, sid) не меняется, а passage=0 эхается в
// observability-payload. Multi-passage цикл (passage>0) — S2/S3.

// TestHandleRunResult_Passage0_CorrelatesIdentically — RunResult с passage=0
// (и опущенным passage) коррелирует со строкой (apply_id, sid, passage=0) —
// тем же UpdateStatus, что N=1-прогон до staged-render (S3 ADR-056): passage=0
// хитит ту же единственную строку хоста (data-level БИТ-В-БИТ). passage=0 едет
// в audit-payload для триажа per-Passage.
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
	// Опущенный passage (старый Soul без поля) — proto-дефолт 0, идентичный путь.
	omitted, _ := run(t, &keeperv1.RunResult{
		ApplyId: "01HAPPLY", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS,
	})

	// Correlation: ровно один UpdateStatus, WHERE по (apply_id, sid, passage) с
	// passage=0 (5-й арг). N=1-прогон шлёт passage=0 → хитит ту же единственную
	// строку хоста, что до staged-render (data-level БИТ-В-БИТ).
	for name, ardb := range map[string]*fakeApplyRunDB{"explicit": explicit, "omitted": omitted} {
		if ardb.execCalls != 1 {
			t.Fatalf("%s: UpdateStatus execCalls = %d, want 1", name, ardb.execCalls)
		}
		if !strings.Contains(ardb.execSQL, "WHERE apply_id = $1 AND sid = $2 AND passage = $5") {
			t.Errorf("%s: correlation WHERE не passage-aware (S3 ADR-056): %q", name, ardb.execSQL)
		}
		if len(ardb.execArgs) != 5 {
			t.Errorf("%s: execArgs len = %d, want 5 (passage в WHERE на S3)", name, len(ardb.execArgs))
		}
		if ardb.execArgs[2] != "success" {
			t.Errorf("%s: status arg = %v, want success", name, ardb.execArgs[2])
		}
		if ardb.execArgs[4] != 0 {
			t.Errorf("%s: passage arg = %v, want 0", name, ardb.execArgs[4])
		}
	}

	// passage=0 эхается в audit run.completed для триажа per-Passage (foundation).
	got := awExplicit.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	if p, ok := got[0].Payload["passage"]; !ok || p != int32(0) {
		t.Errorf("run.completed payload passage = %v (ok=%v), want int32(0)", p, ok)
	}
}

// TestHandleTaskEvent_Passage0_AccumulatesIdentically — TaskEvent с passage=0
// (N=1, plan_index==task_idx) копит register так же, как до staged-render: ключ
// корреляции plan_index ($3) равен task_idx, passage=0 — компонент FK на
// apply_runs (ADR-056 §S1 fix Variant B; миграция 079). passage=0 едет в
// task.executed payload.
func TestHandleTaskEvent_Passage0_AccumulatesIdentically(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{}
	h := newTestHandlerWithApplyRun(t, aw, ardb)

	rd, err := structpb.NewStruct(map[string]any{"stdout": "leader"})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	// N=1: старый Soul без plan_index шлёт 0; новый Soul эхает plan_index==task_idx.
	// Эмулируем эхо нового Soul-а (plan_index=task_idx=2) — поведение идентично N=1.
	h.handleTaskEvent(context.Background(), "host.example.com", "session-1", &keeperv1.TaskEvent{
		ApplyId: "01HABCAPPLY00000000000000", PlanIndex: 2, TaskIdx: 2,
		Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED, RegisterData: rd, Passage: 0,
	})

	// upsert apply_task_register (ADR-056 §S1 fix Variant B): 6 аргументов
	// (apply_id, sid, plan_index, task_idx, register_data, passage). Ключ
	// корреляции — plan_index; passage=0 — компонент FK на apply_runs.
	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (upsert apply_task_register)", ardb.execCalls)
	}
	if !strings.Contains(ardb.execSQL, "INSERT INTO apply_task_register") {
		t.Errorf("SQL = %q, want INSERT INTO apply_task_register", ardb.execSQL)
	}
	if len(ardb.execArgs) != 6 {
		t.Fatalf("execArgs len = %d, want 6 (plan_index + passage в register-upsert)", len(ardb.execArgs))
	}
	if ardb.execArgs[0] != "01HABCAPPLY00000000000000" || ardb.execArgs[1] != "host.example.com" || ardb.execArgs[2] != 2 || ardb.execArgs[3] != 2 {
		t.Errorf("args[0..3] = %v / %v / %v / %v", ardb.execArgs[0], ardb.execArgs[1], ardb.execArgs[2], ardb.execArgs[3])
	}
	if ardb.execArgs[5] != 0 {
		t.Errorf("passage arg = %v, want 0", ardb.execArgs[5])
	}

	// passage=0 эхается в task.executed payload.
	got := aw.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	if p, ok := got[0].Payload["passage"]; !ok || p != 0 {
		t.Errorf("task.executed payload passage = %v (ok=%v), want 0", p, ok)
	}
}
