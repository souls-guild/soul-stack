package handlers

// HANDLER-NATIVE tests for RerunLastTyped: a direct call to the domain function instead of
// httptest+(w,r). 202 → err==nil + view.{ApplyID,Incarnation,Scenario}; 404/409/422 → wantProblem.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// newRerunHandler assembles a handler with all deps for rerun-last
// (runner + resolver + auditWriter). loader is not needed (rerun does not validate input).
func newRerunHandler(db *fakeIncDB, starter *fakeStarter, aw *fakeAuditWriter) *IncarnationHandler {
	return NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, nil, aw, nil, nil)
}

// rerunDB constructs a fakeIncDB for the rerun flow: SelectByName (status) +
// UnlockForRerun SELECT FOR UPDATE (state, status) the same status. The default
// last-run probe → create (create path: last failed == created).
func rerunDB(status string) *fakeIncDB {
	return &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, status) },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow(status) },
	}
}

// TestRerunLast_202_FromErrorLocked — happy path: the lock is removed from error_locked
// (UnlockForRerun: applying) and exactly one scenario `create` is started
// with a shared apply_id; response 202 {apply_id, incarnation, scenario}; audit rerun_last
// with reason + previous_status + scenario.
func TestRerunLast_202_FromErrorLocked(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	aw := &fakeAuditWriter{}
	h := newRerunHandler(db, starter, aw)

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun bootstrap verified")
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v", err)
	}
	if out.Incarnation != "redis-prod" {
		t.Errorf("incarnation = %q", out.Incarnation)
	}
	if out.Scenario != "create" {
		t.Errorf("reply scenario = %q, want create", out.Scenario)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	// Exactly one new create run with the same apply_id.
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "create" {
		t.Errorf("ScenarioName = %q, want create", starter.gotSpec.ScenarioName)
	}
	if starter.gotSpec.ApplyID != out.ApplyID {
		t.Errorf("run apply_id = %q, want %q", starter.gotSpec.ApplyID, out.ApplyID)
	}
	if starter.gotSpec.ServiceRef.Ref != "v1" {
		t.Errorf("ServiceRef.Ref = %q, want v1 (expanded version)", starter.gotSpec.ServiceRef.Ref)
	}
	// audit rerun_last with reason + previous_status=error_locked + scenario.
	if !hasEvent(aw, audit.EventIncarnationRerunLast) {
		t.Fatalf("expected audit incarnation.rerun_last")
	}
	var ev *audit.Event
	for _, e := range aw.events {
		if e.EventType == audit.EventIncarnationRerunLast {
			ev = e
		}
	}
	if ev.Payload["reason"] != "rerun bootstrap verified" {
		t.Errorf("audit reason = %v", ev.Payload["reason"])
	}
	if ev.Payload["previous_status"] != "error_locked" {
		t.Errorf("audit previous_status = %v, want error_locked", ev.Payload["previous_status"])
	}
	if ev.Payload["scenario"] != "create" {
		t.Errorf("audit scenario = %v, want create", ev.Payload["scenario"])
	}
	if ev.Payload["apply_id"] != out.ApplyID {
		t.Errorf("audit apply_id = %v, want %q", ev.Payload["apply_id"], out.ApplyID)
	}
	// Does NOT reuse incarnation.unlocked.
	if hasEvent(aw, audit.EventIncarnationUnlocked) {
		t.Errorf("rerun must not write incarnation.unlocked")
	}
}

// TestRerunLast_ReusesStoredInput_202 — create-path GUARD: rerun-last of an incarnation
// with operator input stored in spec.input (redis cluster: version + shards) →
// that input is passed into RunSpec.Input of the restarted bootstrap run (NOT nil,
// NOT defaults). Regression: RunSpec without Input → nil → the restart fails on required
// validation (version/shards) or applies defaults.
func TestRerunLast_ReusesStoredInput_202(t *testing.T) {
	specJSON := []byte(`{"input":{"version":"8.6.1","shards":3,"connection_mode":"cluster"}}`)
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRowSpec("error_locked", specJSON) },
	}
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-cluster-prod", "rerun cluster bootstrap")
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v", err)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	gotInput := starter.gotSpec.Input
	if gotInput == nil {
		t.Fatal("RunSpec.Input = nil - stored spec.input NOT threaded through (create-path regression)")
	}
	if gotInput["version"] != "8.6.1" {
		t.Errorf("RunSpec.Input[version] = %v, want 8.6.1 (stored)", gotInput["version"])
	}
	// jsonb numbers deserialize as float64 — we compare by value.
	if shards, ok := gotInput["shards"].(float64); !ok || shards != 3 {
		t.Errorf("RunSpec.Input[shards] = %v (%T), want 3", gotInput["shards"], gotInput["shards"])
	}
	if gotInput["connection_mode"] != "cluster" {
		t.Errorf("RunSpec.Input[connection_mode] = %v, want cluster (stored)", gotInput["connection_mode"])
	}
}

// TestRerunLast_NoStoredInput_NilInput_202 — create-path contrast: an incarnation WITHOUT
// stored input (spec.input absent) → RunSpec.Input nil (input was not
// set), the run starts normally. Regression = an empty spec yields `{}` input or
// panics on extraction.
func TestRerunLast_NoStoredInput_NilInput_202(t *testing.T) {
	db := rerunDB("error_locked") // makeUnlockSelectRow → spec=`{}` (no input)
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun no-input bootstrap")
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.Input != nil {
		t.Errorf("RunSpec.Input = %v, want nil (spec without input)", starter.gotSpec.Input)
	}
}

// TestRerunLast_Day2_ReusesRecipeInput_202 — day-2 happy-path: the last failed one
// — add_user (≠ created `create`), its input is taken from the recipe apply_run → 202,
// RunSpec.ScenarioName=="add_user", RunSpec.Input=={user:alice} (not spec.input),
// reply.Scenario=="add_user", audit scenario=="add_user".
func TestRerunLast_Day2_ReusesRecipeInput_202(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		// spec.input carries version — it must NOT leak onto the day-2 path.
		unlockSelectRow: func(_ string) pgx.Row {
			return makeUnlockSelectRowSpec("error_locked", []byte(`{"input":{"version":"8.6.1"}}`))
		},
		lastScenarioRow: func(_ string) pgx.Row {
			return staticRow{values: []any{"add_user", "01HFAILEDADDUSER0000000000"}}
		},
		recipeRow: func(_ string) pgx.Row {
			return staticRow{values: []any{[]byte(`{"scenario_name":"add_user","input":{"user":"alice"}}`)}}
		},
	}
	starter := &fakeStarter{}
	aw := &fakeAuditWriter{}
	h := newRerunHandler(db, starter, aw)

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun add_user verified")
	if err != nil {
		t.Fatalf("RerunLastTyped day-2 err = %v", err)
	}
	if out.Scenario != "add_user" {
		t.Errorf("reply scenario = %q, want add_user", out.Scenario)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "add_user" {
		t.Errorf("ScenarioName = %q, want add_user (last failed operation)", starter.gotSpec.ScenarioName)
	}
	gotInput := starter.gotSpec.Input
	if gotInput == nil || gotInput["user"] != "alice" {
		t.Fatalf("RunSpec.Input = %v, want {user:alice} (recipe.input)", gotInput)
	}
	if _, leaked := gotInput["version"]; leaked {
		t.Error("RunSpec.Input carries spec.input[version] - operation must take recipe.input")
	}
	var ev *audit.Event
	for _, e := range aw.events {
		if e.EventType == audit.EventIncarnationRerunLast {
			ev = e
		}
	}
	if ev == nil || ev.Payload["scenario"] != "add_user" {
		t.Errorf("audit scenario = %v, want add_user", ev)
	}
	// day-2 recipe without from_upgrade → RunSpec.FromUpgrade=false (restart from scenario/).
	if starter.gotSpec.FromUpgrade {
		t.Error("RunSpec.FromUpgrade = true, want false (recipe without from_upgrade)")
	}
}

// TestRerunLast_Day2_FromUpgradeRecipe_202 — MAJOR-guard (ADR-0068): rerun-last of
// a run whose recipe.from_upgrade=true (a failed auto-started upgrade scenario)
// must pass FromUpgrade=true into RunSpec — otherwise the restart looks for scenario/<slug>/
// (which does not exist, §3) and fails with 500. Checks the wiring UnlockResult.FromUpgrade →
// RunSpec.FromUpgrade at the HANDLER level (the DB layer does not catch it).
func TestRerunLast_Day2_FromUpgradeRecipe_202(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		unlockSelectRow: func(_ string) pgx.Row {
			return makeUnlockSelectRowSpec("error_locked", []byte(`{}`))
		},
		lastScenarioRow: func(_ string) pgx.Row {
			return staticRow{values: []any{"to_v2", "01HFAILEDUPGRADE0000000000"}}
		},
		recipeRow: func(_ string) pgx.Row {
			return staticRow{values: []any{[]byte(`{"scenario_name":"to_v2","from_upgrade":true,"input":{}}`)}}
		},
	}
	starter := &fakeStarter{}
	aw := &fakeAuditWriter{}
	h := newRerunHandler(db, starter, aw)

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun upgrade verified")
	if err != nil {
		t.Fatalf("RerunLastTyped day-2 upgrade err = %v", err)
	}
	if out.Scenario != "to_v2" {
		t.Errorf("reply scenario = %q, want to_v2", out.Scenario)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if !starter.gotSpec.FromUpgrade {
		t.Error("RunSpec.FromUpgrade = false, want true (recipe.from_upgrade -> rerun from upgrade/)")
	}
	if !starter.gotSpec.FromLocked {
		t.Error("RunSpec.FromLocked = false, want true (applying reserved)")
	}
}

// TestRerunLast_Day2_BareIncarnation_202 — a bare incarnation (created_scenario IS
// NULL) locked by a day-2 scenario → rerun-last is applicable via the recipe path (was:
// 409). ScenarioName from last-run, Input from recipe.
func TestRerunLast_Day2_BareIncarnation_202(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRowBare("error_locked") },
		lastScenarioRow: func(_ string) pgx.Row {
			return staticRow{values: []any{"update_acl", "01HFAILEDACL00000000000000"}}
		},
		recipeRow: func(_ string) pgx.Row {
			return staticRow{values: []any{[]byte(`{"input":{"acl":"readonly"}}`)}}
		},
	}
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-bare", "rerun bare day-2")
	if err != nil {
		t.Fatalf("RerunLastTyped bare day-2 err = %v", err)
	}
	if out.Scenario != "update_acl" {
		t.Errorf("reply scenario = %q, want update_acl", out.Scenario)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.Input["acl"] != "readonly" {
		t.Errorf("RunSpec.Input[acl] = %v, want readonly (recipe.input)", starter.gotSpec.Input["acl"])
	}
}

// TestRerunLast_Day2_RecipeUnavailable_409 — day-2, but the recipe is absent (recipe
// IS NULL / apply_run purged → ErrNoRows): fail-closed 409 rerun-input-unavailable
// (a distinct problem-type from incarnation-locked — a machine-readable difference from
// "status is not error_locked"), the run does NOT start (no silent bootstrap-input).
func TestRerunLast_Day2_RecipeUnavailable_409(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow("error_locked") },
		lastScenarioRow: func(_ string) pgx.Row {
			return staticRow{values: []any{"add_user", "01HFAILEDADDUSER0000000000"}}
		},
		// recipeRow nil → the recipe probe returns ErrNoRows (fail-closed).
	}
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun add_user")
	wantProblem(t, err, problem.TypeRerunInputUnavailable)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (fail-closed recipe unavailable)", starter.calls)
	}
}

// TestRerunLast_RejectNonErrorLocked — from ready/applying/migration_failed
// rerun is rejected (409 incarnation-locked), the run does NOT start.
func TestRerunLast_RejectNonErrorLocked(t *testing.T) {
	for _, status := range []string{"ready", "applying", "migration_failed", "destroy_failed", "drift"} {
		t.Run(status, func(t *testing.T) {
			db := rerunDB(status)
			starter := &fakeStarter{}
			h := newRerunHandler(db, starter, &fakeAuditWriter{})

			_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "x")
			wantProblem(t, err, problem.TypeIncarnationLocked)
			if starter.calls != 0 {
				t.Errorf("status=%s: scenario start calls = %d, want 0", status, starter.calls)
			}
		})
	}
}

// TestRerunLast_NotFound_404 — non-existent incarnation → 404.
func TestRerunLast_NotFound_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "ghost", "x")
	wantProblem(t, err, problem.TypeNotFound)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0", starter.calls)
	}
}

// TestRerunLast_EmptyReason_422 — empty reason → 422 (explicit confirmation).
func TestRerunLast_EmptyReason_422(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "")
	wantProblem(t, err, problem.TypeValidationFailed)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (rejected before start)", starter.calls)
	}
}

// TestRerunLast_InvalidName_422 — invalid name in path → 422.
func TestRerunLast_InvalidName_422(t *testing.T) {
	h := newRerunHandler(rerunDB("error_locked"), &fakeStarter{}, &fakeAuditWriter{})
	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "Bad_Name", "x")
	wantProblem(t, err, problem.TypeValidationFailed)
}

// TestRerunLast_ReasonAtMax_202 — reason of exactly ReasonMaxLen characters passes
// (inclusive boundary): rerun-last starts, scenario start is called.
func TestRerunLast_ReasonAtMax_202(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("a", incarnation.ReasonMaxLen)
	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v (reason exactly %d is allowed)", err, incarnation.ReasonMaxLen)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// TestRerunLast_ReasonOverMax_422 — reason longer than ReasonMaxLen → 422 BEFORE start
// (upper reason boundary, behavioral invariant).
func TestRerunLast_ReasonOverMax_422(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("a", incarnation.ReasonMaxLen+1)
	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	wantProblem(t, err, problem.TypeValidationFailed)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (reason over max -> rejected before start)", starter.calls)
	}
}

// TestRerunLast_ReasonMultibyteAtMax_202 — LOCK of rune semantics (spec↔runtime):
// a reason of ReasonMaxLen Cyrillic runes is 2*ReasonMaxLen BYTES, but exactly
// ReasonMaxLen runes. JSON-Schema maxLength counts runes, so it MUST pass, even though
// by bytes it is >maxLen. Catches the len(reason)↔utf8.RuneCountInString regression.
func TestRerunLast_ReasonMultibyteAtMax_202(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("я", incarnation.ReasonMaxLen) // ReasonMaxLen runes, 2*ReasonMaxLen bytes
	if len(reason) <= incarnation.ReasonMaxLen {
		t.Fatalf("test precondition violated: %d bytes does not exceed limit %d - case does not distinguish bytes/runes",
			len(reason), incarnation.ReasonMaxLen)
	}
	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v (ReasonMaxLen runes in Cyrillic is allowed - we count runes, not bytes)", err)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// TestRerunLast_ReasonMultibyteOverMax_422 — the reverse boundary of rune semantics:
// ReasonMaxLen+1 Cyrillic runes → 422 BEFORE start (exceeded by runes).
func TestRerunLast_ReasonMultibyteOverMax_422(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("я", incarnation.ReasonMaxLen+1)
	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	wantProblem(t, err, problem.TypeValidationFailed)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (reason over max in runes -> rejected before start)", starter.calls)
	}
}
