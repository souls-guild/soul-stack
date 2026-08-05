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

// TestIncarnation_Create_TraitsGoToTheColumn — the request's traits are validated
// and go straight into the `incarnation.traits` column ($10), which has been their
// source of truth since migration 088. The detour through a freeform `spec` map
// existed only because `spec` was where the request used to be persisted; that
// copy went stale the moment a day-2 `PUT .../traits` edited the column, and the
// column itself is gone (NIM-408).
func TestIncarnation_Create_TraitsGoToTheColumn(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","traits":{"team":"dba","owners":["alice","bob"]}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(db.insertArgs) < 10 {
		t.Fatalf("insertArgs len = %d, want ≥10", len(db.insertArgs))
	}

	traitsBytes, ok := db.insertArgs[9].([]byte)
	if !ok {
		t.Fatalf("insertArgs[9] traits = %T, want []byte", db.insertArgs[9])
	}
	var traits map[string]any
	if err := json.Unmarshal(traitsBytes, &traits); err != nil {
		t.Fatalf("traits not JSON: %v", err)
	}
	if traits["team"] != "dba" {
		t.Errorf("traits.team = %v, want dba (the column is the source of truth)", traits["team"])
	}
	owners, ok := traits["owners"].([]any)
	if !ok || len(owners) != 2 {
		t.Errorf("traits.owners = %v, want a two-element list", traits["owners"])
	}
}

// TestIncarnation_Create_NoTraits_WritesEmptyTraits — a create without `traits`
// writes an EMPTY traits map, not a populated one.
//
// This used to read `insertArgs[4]`, call it spec and look for a `traits` key in
// it. NIM-410 dropped the spec column, so $5 became `state` and the assertion
// went on passing against the wrong argument — and it swallowed the unmarshal
// error, so it would also have passed against no argument at all. Two ways to be
// green regardless of what the handler did.
//
// The real argument is $10 (index 9), the same one the populated case above
// reads, and its emptiness is a claim that can fail.
func TestIncarnation_Create_NoTraits_WritesEmptyTraits(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(db.insertArgs) < 10 {
		t.Fatalf("insertArgs len = %d, want ≥10", len(db.insertArgs))
	}
	traitsBytes, ok := db.insertArgs[9].([]byte)
	if !ok {
		t.Fatalf("insertArgs[9] traits = %T, want []byte", db.insertArgs[9])
	}
	var traits map[string]any
	if err := json.Unmarshal(traitsBytes, &traits); err != nil {
		t.Fatalf("traits not JSON: %v", err)
	}
	if len(traits) != 0 {
		t.Errorf("traits = %v, want {} without traits in the request", traits)
	}
}

// TestIncarnation_Create_InvalidTraitValue_422 — a nested trait value is rejected
// by the domain (ValidateCreateTraits) BEFORE the insert.
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
