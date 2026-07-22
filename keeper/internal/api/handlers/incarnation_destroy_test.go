package handlers

// HANDLER-NATIVE tests for DestroyTyped (T5d-2c-full): direct calls into the domain function
// instead of httptest+(w,r). Status codes are checked via the problem.Type sentinel (wantProblem);
// 202 means err==nil + view.ApplyID. Bind tests (allow_destroy missing/non-bool → 400) are REMOVED:
// huma binds the bool before DestroyTyped, so the domain function already receives a bool.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeDestroyer — a mock of [DestroyStarter]: captures the RunSpec passed to teardown
// and the StartDestroy call count. err simulates a run-start failure.
type fakeDestroyer struct {
	gotSpec scenario.RunSpec
	calls   int
	err     error
}

func (f *fakeDestroyer) StartDestroy(_ context.Context, spec scenario.RunSpec) error {
	f.calls++
	f.gotSpec = spec
	return f.err
}

// fakeAuditWriter — a mock of [audit.Writer]: accumulates written events to check
// destroy_started / destroy_completed (force in the payload).
type fakeAuditWriter struct {
	events []*audit.Event
}

func (f *fakeAuditWriter) Write(_ context.Context, ev *audit.Event) error {
	f.events = append(f.events, ev)
	return nil
}

// newDestroyHandler assembles the handler with all destroy deps. hasScenario controls
// fakeLoader (whether the `destroy` scenario is present in the snapshot).
func newDestroyHandler(db *fakeIncDB, destroyer *fakeDestroyer, aw *fakeAuditWriter, hasScenario bool) *IncarnationHandler {
	loader := &fakeLoader{hasDestroyScenario: hasScenario}
	return NewIncarnationHandler(db, &fakeStarter{}, destroyer, nil, &fakeResolver{ok: true}, loader, aw, nil, nil)
}

// destroyDB builds a fakeIncDB for the full destroy flow: SelectByName
// (prepare/404) returns a row with the given status; Destroy's SELECT FOR UPDATE
// (state, status) returns the same status.
func destroyDB(name, status string) *fakeIncDB {
	return &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, status) },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow(status) },
	}
}

// --- 202 + teardown (scenario present, allow_destroy=false) ---------------

func TestIncarnation_Destroy_Teardown_202(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, destroyer, aw, true)
	view, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false)
	if err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	if len(view.ApplyID) != 26 {
		t.Errorf("apply_id len = %d, want 26", len(view.ApplyID))
	}
	if destroyer.calls != 1 {
		t.Fatalf("StartDestroy calls = %d, want 1 (teardown)", destroyer.calls)
	}
	// handler applyID == teardown-spec applyID (shared correlation).
	if destroyer.gotSpec.ApplyID != view.ApplyID {
		t.Errorf("teardown ApplyID = %q, want %q (handler apply_id)", destroyer.gotSpec.ApplyID, view.ApplyID)
	}
	if destroyer.gotSpec.IncarnationName != "redis-prod" {
		t.Errorf("teardown IncarnationName = %q", destroyer.gotSpec.IncarnationName)
	}
	if destroyer.gotSpec.StartedByAID != "archon-alice" {
		t.Errorf("teardown StartedByAID = %q", destroyer.gotSpec.StartedByAID)
	}
	// Teardown runs against the deployed service version (inc.ServiceVersion = "v1").
	if destroyer.gotSpec.ServiceRef.Ref != "v1" {
		t.Errorf("teardown ServiceRef.Ref = %q, want v1 (deployed version)", destroyer.gotSpec.ServiceRef.Ref)
	}
	// force=false → there must be no direct row DELETE (that's handled by run.go S-D3).
	for _, sql := range db.execCalls {
		if strings.Contains(sql, "DELETE FROM incarnation") {
			t.Errorf("force=false must not perform a direct DELETE; got exec %q", sql)
		}
	}
	// audit destroy_started is written by the Destroy service layer (source=api), force=false.
	if !hasEvent(aw, audit.EventIncarnationDestroyStarted) {
		t.Errorf("expected audit destroy_started")
	}
	if hasEvent(aw, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("destroy_completed must not be written on the teardown path (run.go writes it)")
	}
}

// newDestroyHandlerLifecycle — like newDestroyHandler, but with the snapshot
// manifest's lifecycle block (S3: auto_destroy).
func newDestroyHandlerLifecycle(db *fakeIncDB, destroyer *fakeDestroyer, aw *fakeAuditWriter, hasScenario bool, lc *config.LifecycleConfig) *IncarnationHandler {
	loader := &fakeLoader{hasDestroyScenario: hasScenario, lifecycle: lc}
	return NewIncarnationHandler(db, &fakeStarter{}, destroyer, nil, &fakeResolver{ok: true}, loader, aw, nil, nil)
}

// sawDeleteIncarnation — whether a direct DELETE of the incarnation row occurred among the execs.
func sawDeleteIncarnation(db *fakeIncDB) bool {
	for _, sql := range db.execCalls {
		if strings.Contains(sql, "DELETE FROM incarnation") {
			return true
		}
	}
	return false
}

// TestIncarnation_Destroy_AutoDestroyFalse_DirectDelete — lifecycle.auto_destroy=
// false: deletion is ALWAYS direct (DELETE without teardown), taking priority over
// allow_destroy=false — even when a `destroy` scenario exists.
func TestIncarnation_Destroy_AutoDestroyFalse_DirectDelete(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	// the `destroy` scenario EXISTS, but auto_destroy=false → teardown is skipped.
	h := newDestroyHandlerLifecycle(db, destroyer, aw, true, &config.LifecycleConfig{AutoDestroy: boolPtr(false)})
	// allow_destroy=false — auto_destroy=false must take priority.
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (auto_destroy=false -> no teardown)", destroyer.calls)
	}
	if !sawDeleteIncarnation(db) {
		t.Errorf("auto_destroy=false must perform a direct DELETE; execCalls=%v", db.execCalls)
	}
}

// TestIncarnation_Destroy_AutoDestroyFalse_NoScenario_DirectDelete — auto_destroy=
// false + NO `destroy` scenario + allow_destroy=false: still a direct DELETE
// (takes priority over the scenario-missing gate, which would otherwise return 422).
func TestIncarnation_Destroy_AutoDestroyFalse_NoScenario_DirectDelete(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandlerLifecycle(db, destroyer, aw, false, &config.LifecycleConfig{AutoDestroy: boolPtr(false)})
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false); err != nil {
		t.Fatalf("DestroyTyped err = %v (auto_destroy=false bypasses the scenario gate)", err)
	}
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0", destroyer.calls)
	}
	if !sawDeleteIncarnation(db) {
		t.Errorf("expected a direct DELETE; execCalls=%v", db.execCalls)
	}
}

// TestIncarnation_Destroy_AutoDestroyDefault_Teardown — manifest without lifecycle
// (auto_destroy defaults to true) + allow_destroy=false + scenario present → teardown
// as before (backcompat).
func TestIncarnation_Destroy_AutoDestroyDefault_Teardown(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandlerLifecycle(db, destroyer, aw, true, nil) // lifecycle nil
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	if destroyer.calls != 1 {
		t.Errorf("StartDestroy calls = %d, want 1 (default auto_destroy=true → teardown)", destroyer.calls)
	}
	if sawDeleteIncarnation(db) {
		t.Errorf("the teardown path must not perform a direct DELETE")
	}
}

// --- 422: allow_destroy=false and no `destroy` scenario ----------------

func TestIncarnation_Destroy_NoScenario_NoForce_422(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, destroyer, aw, false) // no scenario
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false)
	wantProblem(t, err, problem.TypeValidationFailed)
	// incarnation is NOT touched: no transition to destroying, no teardown.
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (rejected BEFORE destroying)", destroyer.calls)
	}
	if len(db.execCalls) != 0 {
		t.Errorf("execCalls = %d, want 0 (incarnation must not mutate)", len(db.execCalls))
	}
	if hasEvent(aw, audit.EventIncarnationDestroyStarted) {
		t.Errorf("destroy_started must not be written when the pre-check is rejected")
	}
}

// --- allow_destroy=true without scenario → force-DELETE -------------------

func TestIncarnation_Destroy_Force_NoScenario_202_Delete(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, destroyer, aw, false) // no scenario, but force=true
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", true); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	// force path: teardown is skipped.
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (force skips teardown)", destroyer.calls)
	}
	// force path deletes the row directly (DeleteAfterTeardown).
	if !sawDeleteIncarnation(db) {
		t.Errorf("the force path must perform DELETE FROM incarnation; execCalls=%v", db.execCalls)
	}
	// audit: destroy_started (force=true) + destroy_completed (force=true).
	if !hasEvent(aw, audit.EventIncarnationDestroyStarted) {
		t.Errorf("expected destroy_started")
	}
	if !hasEvent(aw, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("expected destroy_completed (force-DELETE)")
	}
	if got := eventForce(aw, audit.EventIncarnationDestroyStarted); got != true {
		t.Errorf("destroy_started force payload = %v, want true", got)
	}
}

// --- allow_destroy=true with scenario → still force-DELETE -----------
//
// force means "delete without teardown" regardless of whether a scenario exists
// (decisions.md: force path destroying→immediate DELETE).

func TestIncarnation_Destroy_Force_WithScenario_SkipsTeardown(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, destroyer, aw, true) // scenario present, but force=true
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", true); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (force skips teardown even with a scenario)", destroyer.calls)
	}
	if !hasEvent(aw, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("expected destroy_completed (force-DELETE)")
	}
}

// --- allow_destroy maps to force (status_details + audit payload) --

func TestIncarnation_Destroy_AllowDestroyMapsToForce(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, &fakeDestroyer{}, aw, false)
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", true); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	// status_details.force=true is written in the UPDATE-Exec Destroy tx — we check
	// that the transition to destroying happened (history INSERT + status UPDATE).
	var sawStatusUpdate bool
	for _, sql := range db.execCalls {
		if strings.Contains(sql, "UPDATE incarnation") && strings.Contains(sql, "status") {
			sawStatusUpdate = true
		}
	}
	if !sawStatusUpdate {
		t.Errorf("expected UPDATE incarnation status (transition to destroying)")
	}
	if got := eventForce(aw, audit.EventIncarnationDestroyStarted); got != true {
		t.Errorf("allow_destroy=true -> force=true in audit, got %v", got)
	}
}

// --- 404: incarnation does not exist ------------------------------------

func TestIncarnation_Destroy_NotFound_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	h := newDestroyHandler(db, &fakeDestroyer{}, &fakeAuditWriter{}, true)
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "ghost", false)
	wantProblem(t, err, problem.TypeNotFound)
}

// --- 409: status does not allow destroy (applying) ----------------------

func TestIncarnation_Destroy_NotDestroyable_409(t *testing.T) {
	db := destroyDB("redis-prod", "applying")
	destroyer := &fakeDestroyer{}
	h := newDestroyHandler(db, destroyer, &fakeAuditWriter{}, true)
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false)
	wantProblem(t, err, problem.TypeIncarnationLocked)
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (applying does not trigger teardown)", destroyer.calls)
	}
}

// --- 500: destroyer is not configured --------------------------------

func TestIncarnation_Destroy_NotConfigured_500(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	// destroyer/services/loader nil → the endpoint is not configured.
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false)
	wantProblem(t, err, problem.TypeInternalError)
	if len(db.execCalls) != 0 {
		t.Errorf("execCalls = %d, want 0 (not configured -> no mutations)", len(db.execCalls))
	}
}

// --- 422: invalid path name ----------------------------------------

func TestIncarnation_Destroy_InvalidName_422(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	h := newDestroyHandler(db, &fakeDestroyer{}, &fakeAuditWriter{}, true)
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "Bad_Name", false)
	wantProblem(t, err, problem.TypeValidationFailed)
}

// --- force-DELETE no-op (race: row already deleted) → 202 -------------

func TestIncarnation_Destroy_Force_DeleteNoOp_202(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	db.deleteTag = pgconn.NewCommandTag("DELETE 0") // RowsAffected==0 → Deleted=false
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, &fakeDestroyer{}, aw, false)
	// no-op DELETE is not an error (idempotency S-D3): the handler returns 202.
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", true); err != nil {
		t.Fatalf("DestroyTyped err = %v (a no-op DELETE is not an error)", err)
	}
	// destroy_completed is NOT written on no-op (Deleted=false).
	if hasEvent(aw, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("destroy_completed must not be written on a no-op DELETE")
	}
}

// --- helpers ----------------------------------------------------------

func hasEvent(aw *fakeAuditWriter, et audit.EventType) bool {
	for _, ev := range aw.events {
		if ev.EventType == et {
			return true
		}
	}
	return false
}

// eventForce returns the payload["force"] value of the first event of type et
// (false if there is no such event / no such field).
func eventForce(aw *fakeAuditWriter, et audit.EventType) bool {
	for _, ev := range aw.events {
		if ev.EventType == et {
			if v, ok := ev.Payload["force"].(bool); ok {
				return v
			}
		}
	}
	return false
}
