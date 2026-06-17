package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- bulk fake pool ---
//
// Узкий fake под bulk coven-assign: COUNT(*) FROM souls (Matched/dry_run) и
// CTE-чанк (WITH chunk … UPDATE souls). Бизнес-инварианты bulk-слоя (keyset,
// idempotent-отсев, partial) покрыты unit-тестами soul-пакета; здесь
// проверяется ТРАНСПОРТ MCP-tool-а: scope-проверка, маппинг ошибок, audit.

type covenBulkFakePool struct {
	matched int // что вернёт COUNT(*).
	changed int // RETURNING-строки в первом (и единственном) чанке.

	countErr error // ошибка COUNT (до любых записей).
	chunkErr error // ошибка чанк-UPDATE.

	// gotCountWhere / gotChunkSQL — записанные SQL для проверки scope-предиката.
	gotCountArgs []any
	gotChunkArgs []any
}

func (p *covenBulkFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errFakeUnexpected{sql: sql}
}

func (p *covenBulkFakePool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SELECT COUNT(*) FROM souls"):
		p.gotCountArgs = args
		if p.countErr != nil {
			return errRow{err: p.countErr}
		}
		return covenCountRow{n: p.matched}
	case strings.Contains(sql, "WITH chunk AS"):
		p.gotChunkArgs = args
		if p.chunkErr != nil {
			return errRow{err: p.chunkErr}
		}
		// scanned, changed, maxSID. scanned < bulkChunkSize → последний чанк
		// (итерация завершится после одного round-trip-а).
		return covenChunkRow{scanned: p.matched, changed: int64(p.changed)}
	}
	return errRow{err: errFakeUnexpected{sql: sql}}
}

func (p *covenBulkFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	return nil, errFakeUnexpected{sql: sql}
}

func (p *covenBulkFakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &covenBulkFakeTx{pool: p}, nil
}

type covenBulkFakeTx struct{ pool *covenBulkFakePool }

func (t *covenBulkFakeTx) Exec(ctx context.Context, sql string, a ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, a...)
}
func (t *covenBulkFakeTx) Query(ctx context.Context, sql string, a ...any) (pgx.Rows, error) {
	return t.pool.Query(ctx, sql, a...)
}
func (t *covenBulkFakeTx) QueryRow(ctx context.Context, sql string, a ...any) pgx.Row {
	return t.pool.QueryRow(ctx, sql, a...)
}
func (t *covenBulkFakeTx) Begin(ctx context.Context) (pgx.Tx, error) { return t, nil }
func (t *covenBulkFakeTx) Commit(_ context.Context) error            { return nil }
func (t *covenBulkFakeTx) Rollback(_ context.Context) error          { return nil }
func (t *covenBulkFakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("covenBulkFakeTx.CopyFrom: unexpected")
}
func (t *covenBulkFakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("covenBulkFakeTx.SendBatch: unexpected")
}
func (t *covenBulkFakeTx) LargeObjects() pgx.LargeObjects {
	panic("covenBulkFakeTx.LargeObjects: unexpected")
}
func (t *covenBulkFakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("covenBulkFakeTx.Prepare: unexpected")
}
func (t *covenBulkFakeTx) Conn() *pgx.Conn { return nil }

type covenCountRow struct{ n int }

func (r covenCountRow) Scan(dest ...any) error {
	*dest[0].(*int) = r.n
	return nil
}

// covenChunkRow — RETURNING-scan CTE: (scanned int, changed int64, maxSID *string).
type covenChunkRow struct {
	scanned int
	changed int64
}

func (r covenChunkRow) Scan(dest ...any) error {
	*dest[0].(*int) = r.scanned
	*dest[1].(*int64) = r.changed
	// maxSID: пусто — итерация по-любому завершится (scanned < bulkChunkSize).
	*dest[2].(**string) = nil
	return nil
}

// --- harness ---

func newCovenAssignHandler(t *testing.T, rbacCfg *rbactest.Config, pool *covenBulkFakePool) (*Handler, *recordingAudit) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	opSvc, err := operator.NewService(operator.ServiceDeps{
		Pool:       &fakePool{},
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("operator.NewService: %v", err)
	}

	rec := &recordingAudit{}
	deps := HandlerDeps{
		OperatorSvc:   opSvc,
		RBAC:          enf,
		AuditWriter:   rec,
		Logger:        logger,
		IncarnationDB: &fakePool{},
		// enf реализует и Check, и ResolvePurview — single source of truth для
		// обоих гейтов (как production rbac.Holder).
		PurviewResolver: enf,
	}
	if pool != nil {
		deps.SoulDB = pool
	}

	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// covenAssignAdminCfg — оператор без селекторных ограничений (bare grant,
// unrestricted scope).
func covenAssignAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "coven-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"soul.coven-assign",
			}},
		},
	}
}

// covenAssignDevScopedCfg — оператор, ограниченный coven=dev: вправе менять
// метки только хостов в dev и навешивать только метку dev.
func covenAssignDevScopedCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "dev-op", Operators: []string{"archon-dev"}, Permissions: []string{
				"soul.coven-assign on coven=dev",
			}},
		},
	}
}

// --- tests: manifest ---

func TestSoulCovenAssign_InManifest(t *testing.T) {
	e, ok := toolByName("keeper.soul.coven-assign")
	if !ok {
		t.Fatal("keeper.soul.coven-assign missing from catalogManifest")
	}
	if e.status != toolStatusImplemented {
		t.Errorf("status = %d, want Implemented", e.status)
	}
	var schema map[string]any
	if err := json.Unmarshal(e.decl.InputSchema, &schema); err != nil {
		t.Fatalf("inputSchema not valid JSON: %v", err)
	}
	if e.decl.OutputSchema == nil {
		t.Error("outputSchema missing")
	}
}

// --- tests: nil-guard ---

func TestSoulCovenAssign_NilSoulDB(t *testing.T) {
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), nil) // SoulDB == nil
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{"all":true}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("code = %q, want internal-error", data.Code)
	}
}

func TestSoulCovenAssign_NilScoper(t *testing.T) {
	// PurviewResolver не сконфигурирован → internal-error (паритет REST nil scoper → 500).
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	enf, err := rbactest.NewEnforcer(covenAssignAdminCfg())
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	opSvc, err := operator.NewService(operator.ServiceDeps{
		Pool: &fakePool{}, Issuer: &fakeIssuer{}, RBAC: enf, TTLDefault: time.Hour, Logger: logger,
	})
	if err != nil {
		t.Fatalf("operator.NewService: %v", err)
	}
	h, err := NewHandler(HandlerDeps{
		OperatorSvc: opSvc, RBAC: enf, AuditWriter: &recordingAudit{}, Logger: logger,
		IncarnationDB: &fakePool{}, SoulDB: &covenBulkFakePool{},
		// PurviewResolver намеренно nil.
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{"all":true}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("code = %q, want internal-error", data.Code)
	}
}

// --- tests: happy path ---

func TestSoulCovenAssign_AppendSuccess(t *testing.T) {
	pool := &covenBulkFakePool{matched: 5, changed: 5}
	h, rec := newCovenAssignHandler(t, covenAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{"all":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeCovenAssignOutput(t, resp)
	if out.Mode != "append" || out.Label != "prod" {
		t.Errorf("mode/label = %q/%q", out.Mode, out.Label)
	}
	if out.Matched != 5 || out.Changed != 5 {
		t.Errorf("matched/changed = %d/%d, want 5/5", out.Matched, out.Changed)
	}
	if out.Status != string(soul.BulkCompleted) {
		t.Errorf("status = %q, want completed", out.Status)
	}
	if out.DryRun {
		t.Error("dry_run = true, want false")
	}

	ev := requireSingleAudit(t, rec, string(audit.EventSoulCovenChanged))
	if ev.Source != audit.SourceMCP {
		t.Errorf("audit source = %q, want mcp", ev.Source)
	}
	if ev.Payload["source"] != string(audit.SourceMCP) {
		t.Errorf("audit payload source = %v, want mcp", ev.Payload["source"])
	}
	if ev.Payload["label"] != "prod" {
		t.Errorf("audit label = %v", ev.Payload["label"])
	}
	if ev.Payload["scope_applied"] != false {
		t.Errorf("audit scope_applied = %v, want false (unrestricted admin)", ev.Payload["scope_applied"])
	}
	if ev.Payload["dry_run"] != false {
		t.Errorf("audit dry_run = %v, want false", ev.Payload["dry_run"])
	}
}

func TestSoulCovenAssign_RemoveSuccess(t *testing.T) {
	pool := &covenBulkFakePool{matched: 3, changed: 2}
	h, rec := newCovenAssignHandler(t, covenAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"remove","label":"prod","selector":{"coven":"prod"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeCovenAssignOutput(t, resp)
	if out.Mode != "remove" {
		t.Errorf("mode = %q, want remove", out.Mode)
	}
	if out.Matched != 3 || out.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 3/2", out.Matched, out.Changed)
	}
	requireSingleAudit(t, rec, string(audit.EventSoulCovenChanged))
}

func TestSoulCovenAssign_DryRun(t *testing.T) {
	// dry_run: COUNT(*) only, без UPDATE. changed=0, chunk-SQL не зовётся.
	pool := &covenBulkFakePool{
		matched:  7,
		chunkErr: errFakeUnexpected{sql: "dry_run must NOT run chunk UPDATE"},
	}
	h, rec := newCovenAssignHandler(t, covenAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{"all":true},"dry_run":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeCovenAssignOutput(t, resp)
	if !out.DryRun {
		t.Error("dry_run = false, want true")
	}
	if out.Matched != 7 {
		t.Errorf("matched = %d, want 7", out.Matched)
	}
	if out.Changed != 0 {
		t.Errorf("changed = %d, want 0 (dry_run)", out.Changed)
	}
	ev := requireSingleAudit(t, rec, string(audit.EventSoulCovenChanged))
	if ev.Payload["dry_run"] != true {
		t.Errorf("audit dry_run = %v, want true", ev.Payload["dry_run"])
	}
}

// --- tests: validation ---

func TestSoulCovenAssign_InvalidMode(t *testing.T) {
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"merge","label":"prod","selector":{"all":true}}`)
	requireValidationFailed(t, resp)
}

func TestSoulCovenAssign_InvalidLabel(t *testing.T) {
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"BAD LABEL","selector":{"all":true}}`)
	requireValidationFailed(t, resp)
}

func TestSoulCovenAssign_EmptySelector(t *testing.T) {
	// all=false и пусто всё остальное → ErrBulkEmptySelector → validation-failed.
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{}}`)
	requireValidationFailed(t, resp)
}

func TestSoulCovenAssign_InvalidSelectorStatus(t *testing.T) {
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{"status":"zombie"}}`)
	requireValidationFailed(t, resp)
}

func TestSoulCovenAssign_InvalidSelectorSID(t *testing.T) {
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{"sids":["BAD_SID"]}}`)
	requireValidationFailed(t, resp)
}

func TestSoulCovenAssign_UnknownArg(t *testing.T) {
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{"all":true},"extra":1}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeMalformedRequest {
		t.Errorf("code = %q, want malformed-request", data.Code)
	}
}

// --- tests: RBAC / scope (security) ---

func TestSoulCovenAssign_RBACForbidden(t *testing.T) {
	// archon-alice без soul.coven-assign → deny на RBAC.Check, DB не трогается.
	h, rec := newCovenAssignHandler(t, nil, &covenBulkFakePool{
		countErr: errFakeUnexpected{sql: "coven-assign must NOT query when RBAC denies"},
	})
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{"all":true}}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("denied coven-assign must not write audit")
	}
}

// TestSoulCovenAssign_ScopedOperatorCannotAssignProdLabel — КЛЮЧЕВОЙ
// security-кейс (ТЗ): coven-scoped (dev) оператор НЕ может через MCP навесить
// метку prod. Гейт (b) permission-слой: RBAC.Check с {coven: "prod"} не
// матчит selector `coven=dev` → forbidden, ДО любого DB-запроса.
func TestSoulCovenAssign_ScopedOperatorCannotAssignProdLabel(t *testing.T) {
	pool := &covenBulkFakePool{
		countErr: errFakeUnexpected{sql: "out-of-scope label must NOT query"},
	}
	h, rec := newCovenAssignHandler(t, covenAssignDevScopedCfg(), pool)
	resp := callTool(t, h, "archon-dev", "keeper.soul.coven-assign",
		`{"mode":"append","label":"prod","selector":{"all":true}}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error for out-of-scope label")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("forbidden coven-assign must not write audit")
	}
}

// TestSoulCovenAssign_ScopedOperatorTargetsOutOfScopeHosts — гейт (a):
// dev-оператор навешивает свою метку dev, но targeting не ограничивает scope —
// service-слой режет целевые хосты до coven-scope (predicate coven && ARRAY[dev]).
// Permission-гейт (b) проходит (label=dev ∈ scope); проверяем, что scope
// доезжает до service-слоя (scope_applied=true в audit) и операция не падает.
func TestSoulCovenAssign_ScopedOperatorOwnLabelPasses(t *testing.T) {
	pool := &covenBulkFakePool{matched: 2, changed: 2}
	h, rec := newCovenAssignHandler(t, covenAssignDevScopedCfg(), pool)
	resp := callTool(t, h, "archon-dev", "keeper.soul.coven-assign",
		`{"mode":"append","label":"dev","selector":{"all":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeCovenAssignOutput(t, resp)
	if out.Matched != 2 || out.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 2/2", out.Matched, out.Changed)
	}
	// archon-dev, не archon-alice → requireSingleAudit (hardcode alice) не годится.
	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventSoulCovenChanged || ev.Source != audit.SourceMCP {
		t.Errorf("event = %q / source %q", ev.EventType, ev.Source)
	}
	if ev.ArchonAID != "archon-dev" {
		t.Errorf("ArchonAID = %q, want archon-dev", ev.ArchonAID)
	}
	if ev.Payload["scope_applied"] != true {
		t.Errorf("audit scope_applied = %v, want true (restricted dev-op)", ev.Payload["scope_applied"])
	}
	// scope-предикат уехал в COUNT-аргументы (coven && ARRAY[dev] → последний arg
	// — []string{"dev"}); подтверждает, что service-слой получил ненулевой scope.
	if len(pool.gotCountArgs) == 0 {
		t.Fatal("COUNT received no args; scope predicate not applied")
	}
}

// TestSoulCovenAssign_RemoveOutOfScopeLabelRejectedByCheck — даже remove
// out-of-scope метки отсекается permission-гейтом (Check с {coven:label}).
// REST симметрично: SoulCovenLabelSelector ставит {coven:label} для любого mode.
func TestSoulCovenAssign_RemoveOutOfScopeLabelRejected(t *testing.T) {
	pool := &covenBulkFakePool{
		countErr: errFakeUnexpected{sql: "out-of-scope remove must NOT query"},
	}
	h, rec := newCovenAssignHandler(t, covenAssignDevScopedCfg(), pool)
	resp := callTool(t, h, "archon-dev", "keeper.soul.coven-assign",
		`{"mode":"remove","label":"prod","selector":{"all":true}}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("forbidden remove must not write audit")
	}
}

// --- decode helper ---

func decodeCovenAssignOutput(t *testing.T, resp jsonRPCResponse) soulCovenAssignOutput {
	t.Helper()
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out soulCovenAssignOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

func requireValidationFailed(t *testing.T, resp jsonRPCResponse) {
	t.Helper()
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
}

// --- mode=replace MCP (3 кейса: успех, label-out-of-scope, host-out-of-scope) ---

// TestSoulCovenAssign_Replace_Success — admin (unrestricted) делает replace,
// тело содержит labels (не label), ответ содержит labels.
func TestSoulCovenAssign_Replace_Success(t *testing.T) {
	pool := &covenBulkFakePool{matched: 2, changed: 2}
	h, rec := newCovenAssignHandler(t, covenAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"replace","labels":["prod","edge"],"selector":{"all":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeCovenAssignOutput(t, resp)
	if out.Mode != "replace" {
		t.Errorf("mode = %q, want replace", out.Mode)
	}
	if out.Label != "" {
		t.Errorf("label = %q, want пусто для replace", out.Label)
	}
	if len(out.Labels) != 2 {
		t.Errorf("labels = %v, want 2-элементный набор", out.Labels)
	}
	if out.Matched != 2 || out.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 2/2", out.Matched, out.Changed)
	}
	ev := requireSingleAudit(t, rec, string(audit.EventSoulCovenChanged))
	if _, hasLabel := ev.Payload["label"]; hasLabel {
		t.Errorf("audit replace содержит label: %v", ev.Payload)
	}
	auditLabels, ok := ev.Payload["labels"].([]string)
	if !ok || len(auditLabels) != 2 {
		t.Errorf("audit labels = %v, want 2 элемента", ev.Payload["labels"])
	}
}

// TestSoulCovenAssign_Replace_LabelOutOfScopeRejected — гейт (b) на replace:
// набор `[dev, prod]` с scope=dev → forbidden ДО любого DB-запроса (RBAC.Check
// последовательно вернёт deny на `prod`).
func TestSoulCovenAssign_Replace_LabelOutOfScopeRejected(t *testing.T) {
	pool := &covenBulkFakePool{
		countErr: errFakeUnexpected{sql: "out-of-scope replace must NOT query"},
	}
	h, rec := newCovenAssignHandler(t, covenAssignDevScopedCfg(), pool)
	resp := callTool(t, h, "archon-dev", "keeper.soul.coven-assign",
		`{"mode":"replace","labels":["dev","prod"],"selector":{"all":true}}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error for out-of-scope label in set")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("forbidden replace must not write audit")
	}
}

// TestSoulCovenAssign_Replace_HostOutOfScope — dev-оператор делает replace
// `[dev]` под все хосты; scope (a) ограничит фактический UPDATE до dev-хостов.
// Здесь fake matched=0 (как симулируем «нет dev-хостов в БД»), но ключевое —
// что service-слой получил scope-args (covens=[dev]).
func TestSoulCovenAssign_Replace_HostOutOfScope_ScopeApplied(t *testing.T) {
	pool := &covenBulkFakePool{matched: 0, changed: 0}
	h, rec := newCovenAssignHandler(t, covenAssignDevScopedCfg(), pool)
	resp := callTool(t, h, "archon-dev", "keeper.soul.coven-assign",
		`{"mode":"replace","labels":["dev"],"selector":{"all":true}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeCovenAssignOutput(t, resp)
	if out.Matched != 0 || out.Changed != 0 {
		t.Errorf("matched/changed = %d/%d, want 0/0", out.Matched, out.Changed)
	}
	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	if rec.events[0].Payload["scope_applied"] != true {
		t.Errorf("audit scope_applied = %v, want true (restricted dev-op)", rec.events[0].Payload["scope_applied"])
	}
	// scope-предикат уехал в COUNT-args.
	if len(pool.gotCountArgs) == 0 {
		t.Fatal("COUNT received no args; scope predicate not applied for replace")
	}
}

// TestSoulCovenAssign_Replace_RejectsLabelField — XOR: label+replace.
func TestSoulCovenAssign_Replace_RejectsLabelField(t *testing.T) {
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"replace","label":"prod","selector":{"all":true}}`)
	requireValidationFailed(t, resp)
}

// TestSoulCovenAssign_Append_RejectsLabelsField — XOR: labels+append.
func TestSoulCovenAssign_Append_RejectsLabelsField(t *testing.T) {
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), &covenBulkFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","labels":["prod"],"selector":{"all":true}}`)
	requireValidationFailed(t, resp)
}

// --- selector.incarnation MCP (3 кейса: матч, no-match, scope-intersection) ---

// TestSoulCovenAssign_Incarnation_Match — incarnation-селектор доходит до
// service-слоя, args содержат имя incarnation.
func TestSoulCovenAssign_Incarnation_Match(t *testing.T) {
	pool := &covenBulkFakePool{matched: 2, changed: 2}
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"patched","selector":{"incarnation":"redis"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeCovenAssignOutput(t, resp)
	if out.Matched != 2 || out.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 2/2", out.Matched, out.Changed)
	}
	foundIncarnation := false
	for _, a := range pool.gotCountArgs {
		if s, ok := a.(string); ok && s == "redis" {
			foundIncarnation = true
		}
	}
	if !foundIncarnation {
		t.Errorf("incarnation `redis` не дошёл до COUNT-args: %v", pool.gotCountArgs)
	}
}

// TestSoulCovenAssign_Incarnation_NoMatch — incarnation без матча → 0/0,
// chunk-UPDATE не зовётся.
func TestSoulCovenAssign_Incarnation_NoMatch(t *testing.T) {
	pool := &covenBulkFakePool{
		matched:  0,
		chunkErr: errFakeUnexpected{sql: "matched=0 must NOT run chunk UPDATE"},
	}
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"patched","selector":{"incarnation":"ghost"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeCovenAssignOutput(t, resp)
	if out.Matched != 0 || out.Changed != 0 {
		t.Errorf("matched/changed = %d/%d, want 0/0", out.Matched, out.Changed)
	}
}

// TestSoulCovenAssign_Incarnation_ScopeIntersection — incarnation + dev-scope:
// в COUNT-args дойдут И incarnation, И scope-массив.
func TestSoulCovenAssign_Incarnation_ScopeIntersection(t *testing.T) {
	pool := &covenBulkFakePool{matched: 1, changed: 1}
	h, _ := newCovenAssignHandler(t, covenAssignDevScopedCfg(), pool)
	resp := callTool(t, h, "archon-dev", "keeper.soul.coven-assign",
		`{"mode":"append","label":"dev","selector":{"incarnation":"redis"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	foundIncarnation := false
	foundScope := false
	for _, a := range pool.gotCountArgs {
		if s, ok := a.(string); ok && s == "redis" {
			foundIncarnation = true
		}
		if arr, ok := a.([]string); ok && len(arr) == 1 && arr[0] == "dev" {
			foundScope = true
		}
	}
	if !foundIncarnation || !foundScope {
		t.Errorf("count-args не содержат incarnation+scope: %v", pool.gotCountArgs)
	}
}

// TestSoulCovenAssign_Incarnation_InvalidName — невалидное имя incarnation →
// validation-failed ДО БД.
func TestSoulCovenAssign_Incarnation_InvalidName(t *testing.T) {
	pool := &covenBulkFakePool{
		countErr: errFakeUnexpected{sql: "invalid incarnation must NOT query"},
	}
	h, _ := newCovenAssignHandler(t, covenAssignAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.coven-assign",
		`{"mode":"append","label":"patched","selector":{"incarnation":"BAD_NAME"}}`)
	requireValidationFailed(t, resp)
}
