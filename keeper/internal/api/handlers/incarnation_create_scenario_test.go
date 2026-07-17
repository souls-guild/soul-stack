package handlers

// Guard tests for the multiple-create-scenarios mechanism (Variant A) at the handler
// layer: POST /v1/incarnations with a `create_scenario` field (CreateTyped). They cover
// the seam that was missing in *_test.go (the component pieces — ResolveCreateScenarios /
// ValidateCreateScenarioChoice / ValidateInput — are covered in the scenario package;
// an e2e through the handler with a real snapshot was missing):
//
//   (a) a valid create-kind choice → starts EXACTLY the chosen scenario
//       (RunSpec.ScenarioName) + writes incarnation.created_scenario (INSERT $12);
//   (b) an invalid / non-create name → 422 create_scenario_invalid
//       (ErrCreateScenarioNotEligible), the incarnation is NOT created, no run starts;
//   (c) input is validated against the CHOSEN scenario (its required fields), not
//       against the default `create`.
//
// The service snapshot is materialized to disk (temp): ResolveCreateScenarios scans
// art.LocalDir (artifact.ListScenarios), and ValidateInput reads scenario/<chosen>/
// main.yml — both phases see one snapshot via fakeLoader{localDir}.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// createScenarioSnapshot writes a temp service snapshot with three scenarios and
// returns its root (for fakeLoader.localDir):
//
//   - scenario/create/main.yml   — create: true, input without required fields;
//   - scenario/restore/main.yml  — create: true, input with a required `backup_id`
//     (no default) — a selectable start scenario with a schema DIFFERENT from create;
//   - scenario/add_user/main.yml — operational (no create:) — NOT eligible.
//
// Phase 2: `create` is also marked `create: true` — the name is no longer privileged;
// EXACTLY the `create: true` scenarios end up in the set (here {create, restore}).
//
// t.TempDir auto-cleans. Files are minimal (input + tasks: []) so that
// config.LoadScenarioManifestFromBytes parses without diag errors.
func createScenarioSnapshot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name, yaml string) {
		dir := filepath.Join(root, "scenario", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte(yaml), 0o644); err != nil {
			t.Fatalf("write %s/main.yml: %v", name, err)
		}
	}
	write("create", `name: create
create: true
input:
  replicas:
    type: integer
    default: 1
tasks: []
`)
	write("restore", `name: restore
create: true
input:
  backup_id:
    type: string
    required: true
tasks: []
`)
	write("add_user", `name: add_user
input:
  user:
    type: string
    required: true
tasks: []
`)
	return root
}

// newCreateScenarioHandler builds the handler with runner+services+loader (full
// create path: runScenario=true, loader!=nil). starter captures the RunSpec.
func newCreateScenarioHandler(t *testing.T, db *fakeIncDB, starter *fakeStarter) *IncarnationHandler {
	t.Helper()
	loader := &fakeLoader{localDir: createScenarioSnapshot(t)}
	return NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
}

// bareScenarioSnapshot writes a service snapshot WITHOUT a single create scenario (only
// operational restart) and returns its root. The create-scenario set is empty →
// bare incarnation.
func bareScenarioSnapshot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "scenario", "restart")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte("name: restart\ntasks: []\n"), 0o644); err != nil {
		t.Fatalf("write restart/main.yml: %v", err)
	}
	return root
}

// TestIncarnation_Create_BareNoScenario_ReadyNoRun — GUARD Phase 2: a service WITHOUT
// create scenarios + empty create_scenario → bare incarnation: 202, incarnation
// created (insert), no run starts, apply_id is ABSENT, created_scenario col ($12) =
// NULL. The main invariant of the bare branch.
func TestIncarnation_Create_BareNoScenario_ReadyNoRun(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	loader := &fakeLoader{localDir: bareScenarioSnapshot(t)}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-bare","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1 (bare is created)", db.insertCalls)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 (bare without a run)", starter.calls)
	}
	// created_scenario col ($12) = NULL (nil) — bare carries NULL, not 'create'.
	if db.insertArgs[11] != nil {
		t.Errorf("INSERT created_scenario ($12) = %v, want nil (NULL for bare)", db.insertArgs[11])
	}
	// apply_id is absent from JSON (bare without a run).
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, has := raw["apply_id"]; has {
		t.Errorf("apply_id present for bare incarnation: %v", raw)
	}
}

// (a) valid choice of a non-default create scenario --------------------------

// TestIncarnation_Create_ChosenScenario_Starts_AndPersisted — create_scenario:
// restore (create:true) → starts restore (NOT the default create) + INSERT carries
// created_scenario=restore ($12). Regression = operator choice is ignored, create
// always runs.
func TestIncarnation_Create_ChosenScenario_Starts_AndPersisted(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"restore","input":{"backup_id":"b-001"}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Fatalf("starter.calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "restore" {
		t.Errorf("RunSpec.ScenarioName = %q, want restore (chosen, NOT the default create)", starter.gotSpec.ScenarioName)
	}
	// created_scenario — $12 of the INSERT (insertArgs[11]); see insertSQL crud.go.
	if len(db.insertArgs) < 12 {
		t.Fatalf("insertArgs len = %d, want ≥12", len(db.insertArgs))
	}
	if got, _ := db.insertArgs[11].(string); got != "restore" {
		t.Errorf("INSERT created_scenario ($12) = %q, want restore", got)
	}
}

// TestIncarnation_Create_EmptyChoice_HasScenarios_422 — Phase 2: the service OFFERS
// create scenarios ({create, restore}), but the choice is empty → 422
// create_scenario_required listing the eligible ones; the incarnation is NOT created,
// no run starts. Regression = the back-compat default `create` returned on empty choice.
func TestIncarnation_Create_EmptyChoice_HasScenarios_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}
	if !bytes.Contains([]byte(p.Detail), []byte("create_scenario_required")) {
		t.Errorf("detail = %q, want contains create_scenario_required", p.Detail)
	}
	// Listing the eligible scenarios — the operator sees what to choose.
	if !bytes.Contains([]byte(p.Detail), []byte("create")) || !bytes.Contains([]byte(p.Detail), []byte("restore")) {
		t.Errorf("detail = %q, want listing {create, restore}", p.Detail)
	}
	if db.insertCalls != 0 || starter.calls != 0 {
		t.Errorf("insertCalls/starter.calls = %d/%d, want 0/0", db.insertCalls, starter.calls)
	}
}

// TestIncarnation_Create_ExplicitCreate_Starts — explicit create_scenario=create
// (marked create:true) → starts create, created_scenario=create. Contrast to
// _EmptyChoice_HasScenarios_422: the name `create` is valid as an EXPLICIT choice.
func TestIncarnation_Create_ExplicitCreate_Starts(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"create"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if starter.gotSpec.ScenarioName != "create" {
		t.Errorf("RunSpec.ScenarioName = %q, want create", starter.gotSpec.ScenarioName)
	}
	if got, _ := db.insertArgs[11].(string); got != "create" {
		t.Errorf("INSERT created_scenario ($12) = %q, want create", got)
	}
}

// (b) invalid / non-create name → 422 create_scenario_invalid ---------------

// TestIncarnation_Create_NonCreateScenario_422 — an operational scenario (add_user,
// no create:true) as create_scenario → 422 create_scenario_invalid; the incarnation is
// NOT created, no run starts. Regression = one could "create" an incarnation with an
// operational scenario, bypassing bootstrap.
func TestIncarnation_Create_NonCreateScenario_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"add_user","input":{"user":"bob"}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}
	// detail carries the create_scenario_invalid marker (handler mapping of ErrCreateScenarioNotEligible).
	if !bytes.Contains([]byte(p.Detail), []byte("create_scenario_invalid")) {
		t.Errorf("detail = %q, want contains create_scenario_invalid", p.Detail)
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (incarnation is NOT created)", db.insertCalls)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 (run does NOT start)", starter.calls)
	}
}

// TestIncarnation_Create_UnknownScenario_422 — a nonexistent create_scenario →
// 422 (not in the set): the same rejection class as operational.
func TestIncarnation_Create_UnknownScenario_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"nonexistent"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 || starter.calls != 0 {
		t.Errorf("insertCalls/starter.calls = %d/%d, want 0/0", db.insertCalls, starter.calls)
	}
}

// TestIncarnation_Create_TraversalScenario_422 — a garbage name (path-traversal per
// ScenarioNamePattern) is rejected BEFORE resolving the set (not substituted into a
// path) → 422.
func TestIncarnation_Create_TraversalScenario_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"../../etc/passwd"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 || starter.calls != 0 {
		t.Errorf("insertCalls/starter.calls = %d/%d, want 0/0", db.insertCalls, starter.calls)
	}
}

// (c) input is validated against the CHOSEN scenario --------------------------

// TestIncarnation_Create_InputValidatedAgainstChosen_Missing_422 — restore is chosen
// (required backup_id), but input is empty → 422 input_invalid. Proves the schema is
// taken from the CHOSEN scenario (the default create has no such required field).
func TestIncarnation_Create_InputValidatedAgainstChosen_Missing_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"restore"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422 (restore requires backup_id), body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if !bytes.Contains([]byte(p.Detail), []byte("input_invalid")) {
		t.Errorf("detail = %q, want contains input_invalid", p.Detail)
	}
	if db.insertCalls != 0 || starter.calls != 0 {
		t.Errorf("insertCalls/starter.calls = %d/%d, want 0/0 (rejection BEFORE mutation)", db.insertCalls, starter.calls)
	}
}

// TestIncarnation_Create_ChosenCreate_EmptyInputOK — CONTRAST to
// _InputValidatedAgainstChosen_Missing_422: the same empty input, but create is chosen
// (its schema does NOT require backup_id) → 202. The test pair pins: the schema is
// resolved from the CHOSEN scenario, not statically.
func TestIncarnation_Create_ChosenCreate_EmptyInputOK(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"create"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202 (create does NOT require backup_id), body=%s", rec.Code, rec.Body.String())
	}
}

// TestIncarnation_Create_ChosenScenario_BadInputType_422 — restore is chosen,
// backup_id passed as a number (schema: string) → 422 input_invalid (type mismatch
// against the chosen scenario's schema).
func TestIncarnation_Create_ChosenScenario_BadInputType_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"restore","input":{"backup_id":123}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", db.insertCalls)
	}
}
