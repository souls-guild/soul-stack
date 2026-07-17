package handlers

// Guard tests for the Trait relocation per-soul → per-incarnation at the handler layer (ADR-060
// amend R1):
//   - create with top-level `traits` → spec.traits → INSERT (the source of truth that the
//     sync hook projects into souls.traits);
//   - PUT .../traits (SetTraitsTyped) → wholesale replacement of incarnation.traits;
//   - domain validation of trait values (422 on nested) and name (422).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// TestIncarnation_Create_TraitsProjectedToSpec — top-level `traits` on create
// reaches spec.traits (and from there TraitsFromSpec → column incarnation.traits →
// sync hook into souls.traits). We check the jsonb spec arg ($5) of the INSERT.
func TestIncarnation_Create_TraitsProjectedToSpec(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","traits":{"team":"dba","owners":["alice","bob"]}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(db.insertArgs) < 5 {
		t.Fatalf("insertArgs len = %d, want ≥5", len(db.insertArgs))
	}
	specBytes, ok := db.insertArgs[4].([]byte)
	if !ok {
		t.Fatalf("insertArgs[4] spec = %T, want []byte", db.insertArgs[4])
	}
	var spec map[string]any
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("spec not JSON: %v", err)
	}
	traits, ok := spec["traits"].(map[string]any)
	if !ok {
		t.Fatalf("spec.traits = %v (%T), want object", spec["traits"], spec["traits"])
	}
	if traits["team"] != "dba" {
		t.Errorf("spec.traits.team = %v, want dba", traits["team"])
	}
}

// TestIncarnation_Create_NoTraits_NoSpecKey — without `traits` the spec.traits key does NOT
// appear (distinguishable for CEL from "traits set to empty").
func TestIncarnation_Create_NoTraits_NoSpecKey(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	specBytes, _ := db.insertArgs[4].([]byte)
	var spec map[string]any
	_ = json.Unmarshal(specBytes, &spec)
	if _, has := spec["traits"]; has {
		t.Errorf("spec.traits present without traits in request: %v", spec)
	}
}

// TestIncarnation_Create_InvalidTraitValue_422 — a nested trait value is rejected
// by the domain (TraitsFromSpec → ValidateTraitDelta) BEFORE the insert.
func TestIncarnation_Create_InvalidTraitValue_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","traits":{"bad":{"nested":1}}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (422 before insert)", db.insertCalls)
	}
}

// --- PUT /v1/incarnations/{name}/traits (SetTraitsTyped) ---

// TestIncarnation_SetTraits_200_Replaces — successful wholesale replacement: 200 +
// incarnation.traits written with the given set (jsonb arg of the UPDATE).
func TestIncarnation_SetTraits_200_Replaces(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := withClaims(newChiRequest(http.MethodPut, "/v1/incarnations/redis-prod/traits",
		bytes.NewReader([]byte(`{"traits":{"team":"dba","env":"prod"}}`)), "name", "redis-prod"), "archon-alice")
	rec := incSetTraits(h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if db.updateTraitsArg == nil {
		t.Fatal("UPDATE incarnation SET traits was not executed")
	}
	var got map[string]any
	if err := json.Unmarshal(db.updateTraitsArg, &got); err != nil {
		t.Fatalf("traits arg not JSON: %v", err)
	}
	if got["team"] != "dba" || got["env"] != "prod" {
		t.Errorf("persisted traits = %v, want team=dba env=prod", got)
	}
}

// TestIncarnation_SetTraits_EmptyClears — empty/omitted traits → `{}` (clears).
func TestIncarnation_SetTraits_EmptyClears(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := withClaims(newChiRequest(http.MethodPut, "/v1/incarnations/redis-prod/traits",
		bytes.NewReader([]byte(`{}`)), "name", "redis-prod"), "archon-alice")
	rec := incSetTraits(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if string(db.updateTraitsArg) != "{}" {
		t.Errorf("traits arg = %s, want \"{}\" (cleared)", db.updateTraitsArg)
	}
}

// TestIncarnation_SetTraits_InvalidValue_422 — a nested value is rejected by the domain
// (ValidateTraitDelta) BEFORE the UPDATE.
func TestIncarnation_SetTraits_InvalidValue_422(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncarnationRow(name) },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := withClaims(newChiRequest(http.MethodPut, "/v1/incarnations/redis-prod/traits",
		bytes.NewReader([]byte(`{"traits":{"bad":{"nested":1}}}`)), "name", "redis-prod"), "archon-alice")
	rec := incSetTraits(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if db.updateTraitsArg != nil {
		t.Error("UPDATE traits was executed on an invalid value - should 422 before writing")
	}
}

// TestIncarnation_SetTraits_InvalidName_422 — invalid incarnation name → 422.
func TestIncarnation_SetTraits_InvalidName_422(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := withClaims(newChiRequest(http.MethodPut, "/v1/incarnations/Bad_Name/traits",
		bytes.NewReader([]byte(`{"traits":{"team":"dba"}}`)), "name", "Bad_Name"), "archon-alice")
	rec := incSetTraits(h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", rec.Code)
	}
}

// TestIncarnation_SetTraits_404 — non-existent incarnation → 404.
func TestIncarnation_SetTraits_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := withClaims(newChiRequest(http.MethodPut, "/v1/incarnations/ghost/traits",
		bytes.NewReader([]byte(`{"traits":{"team":"dba"}}`)), "name", "ghost"), "archon-alice")
	rec := incSetTraits(h, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Code = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeNotFound {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeNotFound)
	}
}
