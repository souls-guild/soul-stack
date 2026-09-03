package handlers

// Guard tests for server-side incarnation-name composition (ADR-0079, NIM-177) at
// the REST handler layer: POST /v1/incarnations against a create scenario that
// declares `name_template`. The scenario package covers the composition itself;
// here the seam that matters is the HANDLER — that the composed name is what gets
// inserted, run, audited and echoed, and that nothing still reads req.Name.

import (
	"bytes"
	"encoding/json"
	"errors"
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
	h := NewIncarnationHandler(db, starter, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	// Gate (b) re-measures the COMPOSED name against the caller's scope (NIM-333),
	// and refuses when no checker is wired — so these tests, which are about
	// composition rather than about scope, must say who may create. An unrestricted
	// checker keeps them testing what they are named after; the scope behaviour
	// itself is TestIncarnation_Create_Templated_Scoped* below.
	h.SetPermissionChecker(allowAllChecker{})
	return h
}

// allowAllChecker — a PermissionChecker standing in for an unrestricted role.
type allowAllChecker struct{}

func (allowAllChecker) Check(_, _, _ string, _ map[string]string) error { return nil }

// scopedChecker admits `incarnation.create` only for contexts whose dimensions all
// fall inside the allowed sets — a hand-rolled stand-in for a scoped role, so these
// handler unit tests do not need a real enforcer. A dimension absent from the
// context is a MISS, mirroring `evalCond`: a scope predicate over a dimension the
// request does not carry cannot be satisfied.
type scopedChecker struct {
	covens   []string
	services []string
	// incarnations, when non-empty, additionally requires the `incarnation`
	// dimension to be present and listed — the shape of a role scoped
	// `incarnation=`, which gate (a) can never satisfy on a templated create.
	incarnations []string
}

func (s scopedChecker) Check(_, resource, action string, ctx map[string]string) error {
	if resource != "incarnation" || action != "create" {
		return errScopedCheckerDenied
	}
	inSet := func(set []string, key string) bool {
		if len(set) == 0 {
			return true // this dimension is not restricted
		}
		v, ok := ctx[key]
		if !ok {
			return false
		}
		for _, allowed := range set {
			if allowed == v {
				return true
			}
		}
		return false
	}
	if inSet(s.covens, "coven") && inSet(s.services, "service") && inSet(s.incarnations, "incarnation") {
		return nil
	}
	return errScopedCheckerDenied
}

var errScopedCheckerDenied = errors.New("scopedChecker: denied")

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
	if got := reply.AuditPayload["id"]; got != "cache-billing-inv-redis-sentinel" {
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

	rec := postCreate(t, h, `{"id":"my-own","service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
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
	if !strings.Contains(p.Detail, "field 'id' is required") {
		t.Errorf("Detail = %q, want \"field 'id' is required\"", p.Detail)
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

	rec := postCreate(t, h, `{"id":"redis-prod","service":"redis","create_scenario":"create"}`)
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

	rec := postCreate(t, h, `{"id":"NOT-Kebab","service":"redis"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if !strings.Contains(p.Detail, "field 'id' must match") {
		t.Errorf("Detail = %q, want the name-format message", p.Detail)
	}
}

// --- NIM-333: a scoped operator creates a templated incarnation ---

// The demo scenario at the handler seam: a coven-scoped operator creates an
// incarnation whose name is composed server-side. This proves gate (b) admits it;
// the end-to-end proof, through gate (a) and a real enforcer, is
// TestToolsCall_IncarnationCreate_Templated_ScopedOperatorAllowed on MCP.
func TestIncarnation_Create_Templated_ScopedOperatorAllowed(t *testing.T) {
	db := &fakeIncDB{}
	h := newNameTemplateHandler(t, db, &fakeStarter{})
	// A role scoped `coven=billing`, holding nothing unrestricted.
	h.SetPermissionChecker(scopedChecker{covens: []string{"billing"}})

	rec := postCreate(t, h, `{"service":"redis","covens":["billing"],"create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202 — a coven-scoped operator must be able to create inside its own scope; body=%s",
			rec.Code, rec.Body.String())
	}
}

// The other direction, and the one that must not regress into self-service:
// declaring a coven OUTSIDE the caller's scope is refused. Nothing is trimmed —
// there is no "create it without the coven you may not have" fallback.
func TestIncarnation_Create_Templated_CovenOutsideCeilingRefused(t *testing.T) {
	db := &fakeIncDB{}
	h := newNameTemplateHandler(t, db, &fakeStarter{})
	h.SetPermissionChecker(scopedChecker{covens: []string{"billing"}})

	rec := postCreate(t, h, `{"service":"redis","covens":["prod"],"create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Code = %d, want 403 — coven=prod is outside a coven=billing ceiling; body=%s",
			rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("nothing may be inserted on a refusal; insertCalls=%d", db.insertCalls)
	}
}

// Declaring BOTH an in-scope and an out-of-scope coven is refused as a whole. The
// gate ORs over the declared covens, so a partial match would have created the
// incarnation carrying the label the caller may not use — the silent-trim failure
// NIM-209/NIM-232 closed on membership, asked here at create time.
func TestIncarnation_Create_Templated_MixedCovensRefusedWhole(t *testing.T) {
	db := &fakeIncDB{}
	h := newNameTemplateHandler(t, db, &fakeStarter{})
	h.SetPermissionChecker(scopedChecker{covens: []string{"billing"}})

	rec := postCreate(t, h, `{"service":"redis","covens":["billing","prod"],"create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if rec.Code == http.StatusAccepted {
		t.Fatalf("mixed covens must NOT create: one declared label is outside the ceiling; body=%s", rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("nothing may be inserted; insertCalls=%d", db.insertCalls)
	}
}

// A service-scoped role works on the same terms — `service` is required by the
// schema, so it is always available to gate (a).
func TestIncarnation_Create_Templated_ServiceScopedAllowed(t *testing.T) {
	db := &fakeIncDB{}
	h := newNameTemplateHandler(t, db, &fakeStarter{})
	h.SetPermissionChecker(scopedChecker{services: []string{"redis"}})

	rec := postCreate(t, h, `{"service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202 for a service-scoped role; body=%s", rec.Code, rec.Body.String())
	}
}

// Gate (b) in isolation: this test calls the handler directly, so gate (a) does NOT
// run (the middleware is not in the path) and what is exercised is the second gate
// alone. The composed name is outside the allowed set, so the create is refused
// after the template rendered — the check that keeps a caller from choosing, through
// `input`, a name outside their reach.
//
// A role scoped ONLY by `incarnation=` is refused end-to-end at gate (a) instead;
// that is TestToolsCall_IncarnationCreate_Templated_IncarnationScopedRoleStillDenied
// on the MCP surface, where both gates run against a real enforcer.
func TestIncarnation_Create_Templated_ComposedNameOutsideScopeRefused(t *testing.T) {
	db := &fakeIncDB{}
	h := newNameTemplateHandler(t, db, &fakeStarter{})
	h.SetPermissionChecker(scopedChecker{
		services:     []string{"redis"},
		incarnations: []string{"some-other-name"},
	})

	rec := postCreate(t, h, `{"service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Code = %d, want 403 — the composed name is outside the caller's incarnation= scope; body=%s",
			rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("nothing may be inserted; insertCalls=%d", db.insertCalls)
	}
}

// And the same role passes when the composed name IS inside its scope — gate (b) is
// a boundary, not a blanket refusal. Gate (a) is again not in the path here.
func TestIncarnation_Create_Templated_ComposedNameInsideScopeAllowed(t *testing.T) {
	db := &fakeIncDB{}
	h := newNameTemplateHandler(t, db, &fakeStarter{})
	h.SetPermissionChecker(scopedChecker{
		services:     []string{"redis"},
		incarnations: []string{"cache-billing-inv-redis-sentinel"},
	})

	rec := postCreate(t, h, `{"service":"redis","create_scenario":"create","input":{"name":"cache","project":"billing","subproject":"inv"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202 — the composed name is inside the caller's scope; body=%s",
			rec.Code, rec.Body.String())
	}
}

// Fail-closed: no checker wired → the create is refused, not waved through. Calls
// CreateTyped directly rather than through the incCreate shim, because that shim
// defaults an unrestricted checker (it stands for a request that already passed the
// route's RBAC). The same choice the per-host bind gate makes: without the checker
// the boundary cannot be evaluated, and a handler assembled without it is a
// misconfiguration, not a permission.
func TestIncarnation_Create_Templated_NoCheckerRefuses(t *testing.T) {
	db := &fakeIncDB{}
	loader := &fakeLoader{localDir: nameTemplateSnapshot(t)}
	h := NewIncarnationHandler(db, &fakeStarter{}, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	// deliberately no SetPermissionChecker

	r := withClaims(httptest.NewRequest(http.MethodPost, "/v1/incarnations", nil), "archon-alice")
	claims, _ := shimClaims(r)
	_, err := h.CreateTyped(r.Context(), claims, IncarnationCreateRequestInput{
		Service: "redis", CreateScenario: "create",
		Input: map[string]any{"name": "cache", "project": "billing", "subproject": "inv"},
	})
	if err == nil {
		t.Fatal("a create must be refused when no permission checker is wired")
	}
	if db.insertCalls != 0 {
		t.Errorf("nothing may be inserted; insertCalls=%d", db.insertCalls)
	}
}

// A NAMED create goes through gate (b) too, and this is the escalation it closes
// (NIM-338): gate (a) ORs over the declared covens, so a request naming one coven
// the caller holds and one it does not matched on the first and was created carrying
// BOTH. A declared coven is a label on the incarnation itself: every role scoped to
// it now reads and runs that incarnation, and its service vars resolve through that
// overlay (ADR-0082), so the caller wrote into a scope it does not hold. Never
// specific to templating.
func TestIncarnation_Create_Named_MixedCovensRefusedWhole(t *testing.T) {
	db := &fakeIncDB{}
	// A create scenario with no required input, so the request reaches the gate
	// rather than stopping at input validation.
	h := newCreateScenarioHandler(t, db, &fakeStarter{})
	h.SetPermissionChecker(scopedChecker{covens: []string{"billing"}})

	rec := postCreate(t, h, `{"id":"redis-prod","service":"redis","covens":["billing","prod"],"create_scenario":"create"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Code = %d, want 403 — a named create declaring an out-of-scope coven must be refused whole; body=%s",
			rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("nothing may be inserted; insertCalls=%d", db.insertCalls)
	}
}
