package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// in_maintenance_window in insertSQL/updateSQL args: recipeArgs' last value,
// after id at args[0].
const cadenceArgInMaintenanceWindow = 24

func TestCadenceCreate_InMaintenanceWindow_Stored(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"nightly","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},"in_maintenance_window":true}`))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got := store.insertArgs[cadenceArgInMaintenanceWindow]; got != true {
		t.Fatalf("in_maintenance_window column = %v, want true", got)
	}
}

func TestCadenceCreate_InMaintenanceWindow_OnCommand422(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"x","schedule_kind":"interval","interval_seconds":60,"overlap_policy":"skip","kind":"command","module":"core.pkg.present","target":{"coven":["prod"]},"in_maintenance_window":true}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 {
		t.Fatalf("insertCalls = %d, want 0", store.insertCalls)
	}
}

func TestCadencePatch_InMaintenanceWindow_SetAndKept(t *testing.T) {
	id := audit.NewULID()

	store := scenarioStore()
	h := newCadenceHandler(store, allowAll())
	rec := httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"in_maintenance_window":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := store.updateArgs[cadenceArgInMaintenanceWindow]; got != true {
		t.Fatalf("PATCH in_maintenance_window column = %v, want true", got)
	}

	store = scenarioStoreWith(cadenceFullRow{inMaintenanceWindow: true})
	h = newCadenceHandler(store, allowAll())
	rec = httptest.NewRecorder()
	h.Patch(rec, cadenceReqID(http.MethodPatch, "/v1/cadences/"+id, id, `{"enabled":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := store.updateArgs[cadenceArgInMaintenanceWindow]; got != true {
		t.Fatalf("a PATCH without the field reset in_maintenance_window to %v", got)
	}
}

func TestCadenceGet_InMaintenanceWindow_Reported(t *testing.T) {
	store := scenarioStoreWith(cadenceFullRow{inMaintenanceWindow: true})
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Get(rec, cadenceReqID(http.MethodGet, "/v1/cadences/"+id, id, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto cadenceDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !dto.InMaintenanceWindow {
		t.Fatalf("dto.in_maintenance_window = false, want true; body=%s", rec.Body.String())
	}
}
