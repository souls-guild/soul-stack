package handlers

// Guard-тесты релокации Trait per-soul → per-incarnation на handler-слое (ADR-060
// amend R1):
//   - create с top-level `traits` → spec.traits → INSERT (источник истины, который
//     sync-hook проецирует в souls.traits);
//   - PUT .../traits (SetTraitsTyped) → целостная замена incarnation.traits;
//   - доменная валидация trait-значений (422 на nested) и имени (422).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// TestIncarnation_Create_TraitsProjectedToSpec — top-level `traits` на create
// доезжает до spec.traits (а оттуда TraitsFromSpec → колонка incarnation.traits →
// sync-hook в souls.traits). Проверяем jsonb-арг spec ($5) INSERT-а.
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

// TestIncarnation_Create_NoTraits_NoSpecKey — без `traits` ключ spec.traits НЕ
// появляется (отличимо для CEL от «traits заданы пустыми»).
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
		t.Errorf("spec.traits present без traits в запросе: %v", spec)
	}
}

// TestIncarnation_Create_InvalidTraitValue_422 — nested-значение trait отбивается
// доменом (TraitsFromSpec → ValidateTraitDelta) ДО insert.
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
		t.Errorf("insertCalls = %d, want 0 (422 ДО insert)", db.insertCalls)
	}
}

// --- PUT /v1/incarnations/{name}/traits (SetTraitsTyped) ---

// TestIncarnation_SetTraits_200_Replaces — успешная целостная замена: 200 +
// incarnation.traits записан переданным набором (jsonb-арг UPDATE).
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
		t.Fatal("UPDATE incarnation SET traits не выполнен")
	}
	var got map[string]any
	if err := json.Unmarshal(db.updateTraitsArg, &got); err != nil {
		t.Fatalf("traits arg not JSON: %v", err)
	}
	if got["team"] != "dba" || got["env"] != "prod" {
		t.Errorf("persisted traits = %v, want team=dba env=prod", got)
	}
}

// TestIncarnation_SetTraits_EmptyClears — пустой/опущенный traits → `{}` (очистка).
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
		t.Errorf("traits arg = %s, want \"{}\" (очистка)", db.updateTraitsArg)
	}
}

// TestIncarnation_SetTraits_InvalidValue_422 — nested-значение отбивается доменом
// (ValidateTraitDelta) ДО UPDATE.
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
		t.Error("UPDATE traits выполнен на невалидном значении — должен 422 ДО записи")
	}
}

// TestIncarnation_SetTraits_InvalidName_422 — невалидное имя инкарнации → 422.
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

// TestIncarnation_SetTraits_404 — несуществующая инкарнация → 404.
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
