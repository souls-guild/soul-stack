package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// makeIncRowWithState — a pgx.Row stub for SelectByName with a controlled jsonb
// state (for form-prefill tests: the values from which prefill is resolved).
// Mirrors makeIncarnationRow but gives control over the state column (idx 5).
func makeIncRowWithState(name string, state map[string]any) pgx.Row {
	return makeIncRowWithStateVersion(name, "v1", state)
}

// makeIncRowWithStateVersion — like makeIncRowWithState but with an explicit
// service_version (idx 2): version-pin tests verify the snapshot is loaded by the
// incarnation's ServiceVersion, not by the resolver default.
func makeIncRowWithStateVersion(name, version string, state map[string]any) pgx.Row {
	stateBytes, _ := json.Marshal(state)
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", version, int(1),
		[]byte("{}"), stateBytes, "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		any(nil), []byte(nil),
		"create", // created_scenario (migration 089, NOT NULL DEFAULT)
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// formPrefillHandler assembles an IncarnationHandler with db (state row) + loader
// (scenario YAML + state_schema for the secret exclusion) + services.
func formPrefillHandler(state map[string]any, scenarioYAML string, stateSchema map[string]any) *IncarnationHandler {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowWithState(name, state) },
	}
	loader := &fakeLoader{scenarioYAML: scenarioYAML, stateSchema: stateSchema}
	return &IncarnationHandler{
		db:       db,
		loader:   loader,
		services: &fakeResolver{ok: true},
	}
}

// allowAllScope — an inScope predicate "incarnation is always in scope" (the RBAC
// boundary is checked by separate scope tests; here the focus is prefill resolution).
func allowAllScope(*incarnation.Incarnation) bool { return true }

// TestFormPrefill_ResolvesDeclaredPaths — the basic happy path: declared
// prefill_from_state paths resolve from incarnation.state into {values}.
func TestFormPrefill_ResolvesDeclaredPaths(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n" +
		"  redis_version: { type: string, prefill_from_state: state.redis_version }\n" +
		"  max_memory: { type: string, prefill_from_state: state.config.max_memory }\n"
	state := map[string]any{
		"redis_version": "7.2.4",
		"config":        map[string]any{"max_memory": "2gb"},
	}
	h := formPrefillHandler(state, scenarioYAML, nil)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if res.Values["redis_version"] != "7.2.4" {
		t.Errorf("redis_version = %#v, want 7.2.4", res.Values["redis_version"])
	}
	if res.Values["max_memory"] != "2gb" {
		t.Errorf("max_memory = %#v, want 2gb (nested state.config.max_memory)", res.Values["max_memory"])
	}
}

// TestFormPrefill_UncoveredPathOmitted — a field whose prefill path is absent from
// the current state is OMITTED (not a null value, not an error).
func TestFormPrefill_UncoveredPathOmitted(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n" +
		"  redis_version: { type: string, prefill_from_state: state.redis_version }\n" +
		"  missing: { type: string, prefill_from_state: state.not_in_state }\n"
	state := map[string]any{"redis_version": "7.2.4"}
	h := formPrefillHandler(state, scenarioYAML, nil)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if _, present := res.Values["missing"]; present {
		t.Errorf("field with an uncovered state path should not land in values: %#v", res.Values)
	}
	if res.Values["redis_version"] != "7.2.4" {
		t.Errorf("covered field lost: %#v", res.Values)
	}
}

// TestFormPrefill_PathWhitelist — GUARD (invariant b): ONLY the prefill paths
// declared in the schema are resolved. A field WITHOUT prefill_from_state — even
// if its name matches a state key — does NOT land in values (the client does not
// supply the path, arbitrary state access is impossible).
func TestFormPrefill_PathWhitelist(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n" +
		"  redis_version: { type: string, prefill_from_state: state.redis_version }\n" +
		"  secret_token: { type: string }\n" // NO prefill_from_state → not whitelisted
	state := map[string]any{
		"redis_version": "7.2.4",
		"secret_token":  "super-secret", // present in state, but the field did not declare it
	}
	h := formPrefillHandler(state, scenarioYAML, nil)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if _, present := res.Values["secret_token"]; present {
		t.Errorf("field without declared prefill_from_state leaked into values (path-whitelist breached): %#v", res.Values)
	}
	if len(res.Values) != 1 || res.Values["redis_version"] != "7.2.4" {
		t.Errorf("expected only the declared redis_version: %#v", res.Values)
	}
}

// TestFormPrefill_SecretExcluded — GUARD (invariant c): a field whose state path
// is marked secret in state_schema is EXCLUDED from prefill entirely (pre-filling a
// mask is useless). A non-secret field stays.
func TestFormPrefill_SecretExcluded(t *testing.T) {
	scenarioYAML := "name: rotate\nstate_changes: {}\ntasks: []\n" +
		"input:\n" +
		"  admin_token: { type: string, secret: true, prefill_from_state: state.admin_token }\n" +
		"  redis_version: { type: string, prefill_from_state: state.redis_version }\n"
	state := map[string]any{
		"admin_token":   "s3cr3t-token",
		"redis_version": "7.2.4",
	}
	// state_schema marks admin_token secret → secretSchemaForIncarnation.IsSecret("state.admin_token")=true.
	stateSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"admin_token":   map[string]any{"type": "string", "secret": true},
			"redis_version": map[string]any{"type": "string"},
		},
	}
	h := formPrefillHandler(state, scenarioYAML, stateSchema)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "rotate", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if _, present := res.Values["admin_token"]; present {
		t.Errorf("secret field leaked into the prefill response (invariant c breached): %#v", res.Values["admin_token"])
	}
	if res.Values["redis_version"] != "7.2.4" {
		t.Errorf("non-secret field lost: %#v", res.Values)
	}
}

// TestFormPrefill_OutOfScope404 — out of RBAC scope → 404 (do not reveal existence,
// parity with GetTyped).
func TestFormPrefill_OutOfScope404(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n  redis_version: { type: string, prefill_from_state: state.redis_version }\n"
	h := formPrefillHandler(map[string]any{"redis_version": "7.2.4"}, scenarioYAML, nil)

	_, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config",
		func(*incarnation.Incarnation) bool { return false })
	if err == nil {
		t.Fatal("out of scope expected a 404 error")
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("error is not problemError: %v", err)
	}
	if d.Status != 404 {
		t.Errorf("status = %d, want 404", d.Status)
	}
}

// TestFormPrefill_NilScope404 — a nil predicate → fail-closed 404.
func TestFormPrefill_NilScope404(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n  redis_version: { type: string, prefill_from_state: state.redis_version }\n"
	h := formPrefillHandler(map[string]any{"redis_version": "7.2.4"}, scenarioYAML, nil)

	if _, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", nil); err == nil {
		t.Fatal("nil-scope expected a 404 error (fail-closed)")
	}
}

// TestFormPrefill_NoPrefillFields — a schema without prefill_from_state → empty
// values (not an error).
func TestFormPrefill_NoPrefillFields(t *testing.T) {
	scenarioYAML := "name: restart\nstate_changes: {}\ntasks: []\n" +
		"input:\n  reason: { type: string }\n"
	h := formPrefillHandler(map[string]any{"redis_version": "7.2.4"}, scenarioYAML, nil)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "restart", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if len(res.Values) != 0 {
		t.Errorf("expected empty values (no prefill fields): %#v", res.Values)
	}
}

// TestFormPrefill_SchemaPinnedToServiceVersion — GUARD (anti version-craft): the
// prefill schema (BOTH the path-whitelist AND the secret-set) is loaded STRICTLY by
// inc.ServiceVersion, not by an arbitrary client version. The client no longer
// supplies the version; the test pins that both snapshot materializations
// (prefillFieldsForScenario + secretSchemaForIncarnation) requested the SAME
// authoritative version = ServiceVersion. A regression (whitelist/secret drift
// across versions) → a version-craft vector for returning sensitive fields.
func TestFormPrefill_SchemaPinnedToServiceVersion(t *testing.T) {
	const wantVersion = "v2.0.0" // different from the fakeResolver default ("v1")
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n  redis_version: { type: string, prefill_from_state: state.redis_version }\n"
	stateSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"redis_version": map[string]any{"type": "string"}},
	}
	state := map[string]any{"redis_version": "7.2.4"}

	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return makeIncRowWithStateVersion(name, wantVersion, state)
		},
	}
	loader := &fakeLoader{scenarioYAML: scenarioYAML, stateSchema: stateSchema}
	h := &IncarnationHandler{db: db, loader: loader, services: &fakeResolver{ok: true}}

	if _, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", allowAllScope); err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}

	if len(loader.loadedRefs) == 0 {
		t.Fatal("service snapshot was not loaded - version-pin unproven")
	}
	for i, ref := range loader.loadedRefs {
		if ref != wantVersion {
			t.Errorf("Load[%d] ref = %q, want %q (schema must be pinned to ServiceVersion, not the client-supplied/default one)", i, ref, wantVersion)
		}
	}
}
