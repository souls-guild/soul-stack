package handlers

// Guard tests for server-side incarnation-name composition (ADR-0079, NIM-177) at
// the REST handler layer: POST /v1/incarnations against a create scenario that
// declares `name_template`. The scenario package covers the composition itself;
// here the seam that matters is the HANDLER — that the composed name is what gets
// inserted, run, audited and echoed, and that nothing still reads req.Name.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// nameTemplateSnapshot writes a service snapshot with ONE create scenario whose
// name is composed from four input components (the NIM-177 shape), and returns its
// root for fakeLoader.localDir.
func nameTemplateSnapshot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "scenario", "create")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	yaml := `name: create
create: true
name_template: "${input.name}-${input.project}-${input.subproject}-redis-${input.service_type}"
input:
  name:
    type: string
    required: true
  project:
    type: string
    required: true
  subproject:
    type: string
    required: true
  service_type:
    type: string
    default: sentinel
tasks: []
`
	if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write create/main.yml: %v", err)
	}
	return root
}

func newNameTemplateHandler(t *testing.T, db *fakeIncDB, starter *fakeStarter) *IncarnationHandler {
	t.Helper()
	loader := &fakeLoader{localDir: nameTemplateSnapshot(t)}
	return NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
}

// postCreate is the shared request shape of these tests: no `name` field at all.
func postCreate(t *testing.T, h *IncarnationHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations", bytes.NewReader([]byte(body)))
	req = withClaims(req, "archon-alice")
	return incCreate(h, req)
}

// TestIncarnation_Create_NameComposed — the primary handler guard: a request
// WITHOUT `name` succeeds, and the composed name reaches every consumer (insert
// $1, the bootstrap RunSpec, the 202 body).
func TestIncarnation_Create_NameComposed(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newNameTemplateHandler(t, db, starter)

	rec := postCreate(t, h, `{"service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}

	const want = "cache-billing-inv-redis-sentinel"
	if got, _ := db.insertArgs[0].(string); got != want {
		t.Errorf("INSERT name ($1) = %q, want %q", got, want)
	}
	if starter.gotSpec.IncarnationName != want {
		t.Errorf("RunSpec.IncarnationName = %q, want %q", starter.gotSpec.IncarnationName, want)
	}
	var body struct {
		Incarnation string `json:"incarnation"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Incarnation != want {
		t.Errorf("202 body incarnation = %q, want %q", body.Incarnation, want)
	}
}

// TestIncarnation_Create_ComposedName_InAudit — the audit payload must carry the
// name that was actually created; auditing the (absent) request name would make
// incarnation.created untraceable.
func TestIncarnation_Create_ComposedName_InAudit(t *testing.T) {
	db := &fakeIncDB{}
	h := newNameTemplateHandler(t, db, &fakeStarter{})

	r := withClaims(httptest.NewRequest(http.MethodPost, "/v1/incarnations", nil), "archon-alice")
	claims, _ := shimClaims(r)
	reply, err := h.CreateTyped(r.Context(), claims,
		IncarnationCreateRequestInput{
			Service: "redis", CreateScenario: "create",
			Input: map[string]any{"name": "cache", "project": "billing", "subproject": "inv"},
		})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
	if got := reply.AuditPayload["name"]; got != "cache-billing-inv-redis-sentinel" {
		t.Errorf("audit name = %v, want the composed name", got)
	}
}

// TestIncarnation_Create_ComposedNameTooLong_422 — the 63-character ceiling: an
// explicit 422 naming the overflow, and NOTHING is created. A regression that
// truncates instead would create an incarnation under a name the operator never
// asked for — and the name is the immutable primary key.
func TestIncarnation_Create_ComposedNameTooLong_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newNameTemplateHandler(t, db, starter)

	long := strings.Repeat("x", 30)
	rec := postCreate(t, h, `{"service":"redis","create_scenario":"create","input":{"name":"`+long+`","project":"`+long+`","subproject":"`+long+`"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}
	if !strings.Contains(p.Detail, "composed_name_invalid") {
		t.Errorf("Detail = %q, want the composed_name_invalid prefix", p.Detail)
	}
	if !strings.Contains(p.Detail, "63") {
		t.Errorf("Detail = %q, must state the ceiling so the operator knows what to shorten", p.Detail)
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (nothing is created on an invalid composed name)", db.insertCalls)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0", starter.calls)
	}
}

// TestIncarnation_Create_ExplicitNameWithTemplate_422 — sending `name` against a
// composing scenario is refused, not silently ignored: otherwise the RBAC
// `incarnation=<name>` dimension would be checked on one name while another is
// inserted.
func TestIncarnation_Create_ExplicitNameWithTemplate_422(t *testing.T) {
	db := &fakeIncDB{}
	h := newNameTemplateHandler(t, db, &fakeStarter{})

	rec := postCreate(t, h, `{"name":"my-own","service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if !strings.Contains(p.Detail, "name_not_composable") {
		t.Errorf("Detail = %q, want the name_not_composable prefix", p.Detail)
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", db.insertCalls)
	}
}

// TestIncarnation_Create_NoTemplate_NameStillRequired — back-compat: against a
// service whose create scenario declares NO template, an omitted name is still the
// same 422 as before. The `required` check moved past the plan resolve; it did not
// disappear.
func TestIncarnation_Create_NoTemplate_NameStillRequired(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	rec := postCreate(t, h, `{"service":"redis","create_scenario":"create"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if !strings.Contains(p.Detail, "field 'name' is required") {
		t.Errorf("Detail = %q, want \"field 'name' is required\"", p.Detail)
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", db.insertCalls)
	}
}

// TestIncarnation_Create_NoTemplate_OperatorNameStands — the opt-in half of
// back-compat: a scenario without a template creates under exactly the name the
// operator sent, unchanged.
func TestIncarnation_Create_NoTemplate_OperatorNameStands(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	rec := postCreate(t, h, `{"name":"redis-prod","service":"redis","create_scenario":"create"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := db.insertArgs[0].(string); got != "redis-prod" {
		t.Errorf("INSERT name ($1) = %q, want redis-prod", got)
	}
	if starter.gotSpec.IncarnationName != "redis-prod" {
		t.Errorf("RunSpec.IncarnationName = %q, want redis-prod", starter.gotSpec.IncarnationName)
	}
}

// TestIncarnation_Create_MalformedNameStillRejectedEarly — making `name` optional
// must not make a MALFORMED name acceptable: a non-empty value is still
// format-checked before anything else happens.
func TestIncarnation_Create_MalformedNameStillRejectedEarly(t *testing.T) {
	db := &fakeIncDB{}
	h := newCreateScenarioHandler(t, db, &fakeStarter{})

	rec := postCreate(t, h, `{"name":"NOT-Kebab","service":"redis"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if !strings.Contains(p.Detail, "field 'name' must match") {
		t.Errorf("Detail = %q, want the name-format message", p.Detail)
	}
}
