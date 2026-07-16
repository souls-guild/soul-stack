package api

// Guard tests for PILOT-1, migrating POST /v1/cadences to huma, FULL-TYPED form
// (code-first, ADR-054 Amendment 2026-06-12). They prove that a huma route over chi
// preserves the cluster invariants while coexisting with oapi-strict routes, and that the FULL-TYPED
// boundary (typed Body + envelope + extracted CreateTyped) is correct:
//
//   - wire: 201 + Location + cadence_id (CreateTyped → typed output);
//   - unknown-field → 400 application/problem+json (now huma-native: huma
//     catches additionalProperties:false, the error-override classifies
//     "unexpected property" → 400, NOT the domain DisallowUnknownFields);
//   - malformed-body → 400 application/problem+json (huma JSON parse);
//   - missing-required (target) → 422 problem+json (huma `required:"true"`);
//   - RBAC-deny on cadence.create → 403 (group wiring, huma inherits it);
//   - AUDIT (CRITICAL, S6 lesson): cadence.create via huma writes an audit event with a non-empty
//     payload (cadence self-audit INSIDE CreateTyped → huma does not touch it);
//   - no-audit-on-reject: 422 writes no audit;
//   - OpenAPI fragment: huma generates the 3.1 spec of POST /v1/cadences from the FULL-TYPED
//     Go types;
//   - GOLDEN-JSON: the 201 reply is byte-for-byte == the pinned reference (wire regression
//     guard for the rollout: omitempty/nullable NextRunAt, []-vs-null).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// humaCadenceRouter assembles a chi router with POST /v1/cadences via huma —
// the production wiring taken verbatim from router.go: RequirePermission(cadence.create) on
// the group + the huma operation inside. installHumaErrorOverride is called explicitly (as in
// buildRouter — a single point). injectClaims replaces RequireJWT (a valid JWT is not
// needed — the subject under test is the huma wiring, not auth). enforcer/auditW are parameterized.
func humaCadenceRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	store := &strictFakeCadenceStore{}
	cadenceH := handlers.NewCadenceHandler(store, nil, nil, enforcer, auditW, nil, 0, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/cadences", func(r chi.Router) {
			r.With(
				injectClaims,
				apimiddleware.RequirePermission(enforcer, "cadence", "create", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaCadence(newHumaCadenceAPI(r), cadenceH)
			})
		})
	})
	return r
}

// TestHumaCadence_Create_WireEquivalent — the FULL-TYPED envelope yields 201 + Location +
// cadence_id (CreateTyped → typed output), like a direct handler call.
func TestHumaCadence_Create_WireEquivalent(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(strictCadenceBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/v1/cadences/") {
		t.Errorf("Location = %q, want /v1/cadences/<id>", loc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var reply struct {
		CadenceID string `json:"cadence_id"`
		Name      string `json:"name"`
		Location  string `json:"location"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal reply: %v; body=%s", err, rec.Body.String())
	}
	if reply.CadenceID == "" || reply.Name != "hourly" || reply.Location == "" {
		t.Errorf("reply неполон: %+v", reply)
	}
}

// TestHumaCadence_Create_UnknownField_400 — the KEY FULL-TYPED invariant: huma
// validates the typed Body (additionalProperties:false is HONEST), an unknown field is caught by
// huma → "unexpected property" → the error-override classifies 400
// application/problem+json (the cluster contract unknown→400, now huma-native).
func TestHumaCadence_Create_UnknownField_400(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	body := `{"name":"x","schedule_kind":"cron","cron_expr":"0 * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]},"bogus_field":1}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown-field via huma additionalProperties:false); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaCadence_Create_MalformedBody_400 — malformed JSON → 400 problem+json
// (huma JSON parse, error-override 400 TypeMalformedRequest).
func TestHumaCadence_Create_MalformedBody_400(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(`{broken`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaCadence_Create_MissingTarget_422 — required validation (target is mandatory)
// is now done by huma (`required:"true"`) → 422 problem+json. Proves that
// missing-required is rejected huma-natively (NOT by domain code), and the status matches
// the former domain classification 422.
func TestHumaCadence_Create_MissingTarget_422(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	body := `{"name":"x","schedule_kind":"cron","cron_expr":"0 * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing target, huma required); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// TestHumaCadence_Create_RBACDeny_403 — RequirePermission(cadence.create) on the group
// rejects the request BEFORE the huma handler (deny-enforcer) → 403. Proves that the group
// wiring is inherited by the huma route.
func TestHumaCadence_Create_RBACDeny_403(t *testing.T) {
	r := humaCadenceRouter(t, strictDenyAll{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(strictCadenceBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (RBAC-deny on cadence.create); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadence_Create_AuditRecorded — CRITICAL guard (S6 lesson): cadence.create via
// huma writes an audit event with a non-empty payload on the successful write path. The cadence
// self-audit (emitWrite INSIDE CreateTyped) is preserved by the FULL-TYPED extraction; the guard
// catches the regression "huma swallowed / did not carry the write path to audit".
func TestHumaCadence_Create_AuditRecorded(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRouter(t, strictAllowAll{}, auditCap)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(strictCadenceBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	evs := auditCap.Events()
	if len(evs) == 0 {
		t.Fatalf("audit NOT записан on успешbutм 201 huma-роуте (huma сломал write-путь cadence.create)")
	}
	ev := evs[0]
	if ev.EventType != audit.EventCadenceCreated {
		t.Errorf("event_type = %q, want %q", ev.EventType, audit.EventCadenceCreated)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("archon_aid = %q, want archon-alice", ev.ArchonAID)
	}
	if len(ev.Payload) == 0 {
		t.Error("audit payload empty — FULL-TYPED fromвлечение потеряло toменный payload")
	}
	if ev.Payload["cadence_id"] == nil || ev.Payload["name"] == nil {
		t.Errorf("audit payload без cadence_id/name: %+v", ev.Payload)
	}
}

// TestHumaCadence_NoAudit_OnReject — negative guard: on a huma reject (missing target
// → 422) audit is NOT written (CreateTyped never reaches emitWrite).
func TestHumaCadence_NoAudit_OnReject(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRouter(t, strictAllowAll{}, auditCap)

	body := `{"name":"x","schedule_kind":"cron","cron_expr":"0 * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан on rejected-запросе (%d withбытий) — write-путь не toлжен писать on 422", len(auditCap.Events()))
	}
}

// TestHumaCadence_OpenAPIFragment_3_1 — huma generates the OpenAPI fragment of POST
// /v1/cadences from the FULL-TYPED Go types (code-first), version 3.1.0 (huma default).
// The fragment carries the body shape (required name/schedule_kind, enum kind) AND the response shape.
func TestHumaCadence_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaCadenceSpecYAML()
	if err != nil {
		t.Fatalf("HumaCadenceSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{"createCadence", "getCadence", "listCadenceRuns", "patchCadence", "deleteCadence", "enableCadence", "disableCadence", "schedule_kind", "overlap_policy", "cadence_id"} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не withдержит %q (form from Go-типов потеряon):\n%s", want, frag)
		}
	}
}

// TestHumaCadence_Create_GoldenWire — GOLDEN-JSON snapshot (wire regression guard
// for the rollout, CRITICAL): the 201 reply huma-output, run through map→sorted-marshal, ==
// the pinned reference. The guard pins the SET of keys, omitempty/nullable
// (NextRunAt is absent when nil — strictFakeCadenceStore does not return next_run_at)
// and the absence of extra fields ($schema). JSON key order is not semantic and is
// normalized by re-marshaling through a map. cadence_id is normalized (the ULID is
// nondeterministic) before comparison.
func TestHumaCadence_Create_GoldenWire(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(strictCadenceBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	// Normalize the nondeterministic ULID (cadence_id + the location tail) and the
	// next_run_at timestamp to placeholders — otherwise the golden is unreachable. Presence/
	// absence of keys (omitempty) and shape are preserved: the cron-schedule body carries
	// next_run_at (resolved next-hour) — the golden pins ITS PRESENCE as a
	// nullable key (if CadenceCreateReply loses omitempty/type — drift).
	got := normalizeCadenceWire(t, rec.Body.Bytes())

	const golden = `{"cadence_id":"_ULID_","enabled":true,"location":"/v1/cadences/_ULID_","name":"hourly","next_run_at":"_TS_"}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф FULL-TYPED reply:\n got  = %s\n want = %s\n(onбор ключей/omitempty/nullable/onличие $schema fromменился — проверь CadenceCreateReply и newHumaCadenceAPI)", got, golden)
	}
}

// normalizeCadenceWire runs the reply through map → canonical marshal (keys are
// sorted) and replaces nondeterministic values (cadence_id + the location tail
// → "<ULID>"; next_run_at timestamp → "<TS>") with placeholders. It preserves
// the presence/absence of keys (omitempty) and any extra fields (e.g. $schema —
// which surfaces immediately in the diff). This way the golden pins the SHAPE, not the values.
func normalizeCadenceWire(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; raw=%s", err, raw)
	}
	if id, ok := m["cadence_id"].(string); ok && id != "" {
		m["cadence_id"] = "_ULID_"
		if loc, ok := m["location"].(string); ok {
			m["location"] = strings.Replace(loc, id, "_ULID_", 1)
		}
	}
	if _, ok := m["next_run_at"]; ok {
		m["next_run_at"] = "_TS_" // pin the presence of the nullable key, not the value
	}
	out, err := json.Marshal(m) // json.Marshal map → keys are sorted (determinism)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

// assertHumaProblem checks Content-Type=application/problem+json and the type URN.
func assertHumaProblem(t *testing.T, rec *httptest.ResponseRecorder, wantType string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q; body=%s", ct, problem.ContentType, rec.Body.String())
	}
	var p problem.Details
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body не problem+json: %v; body=%s", err, rec.Body.String())
	}
	if p.Type != wantType {
		t.Errorf("problem type = %q, want %q", p.Type, wantType)
	}
}

// strictDenyAll — deny-all PermissionChecker (RBAC-deny guard). Any non-nil error
// from Check → RequirePermission returns 403 (see rbac.go).
type strictDenyAll struct{}

func (strictDenyAll) Check(string, string, string, map[string]string) error {
	return rbac.ErrPermissionDenied
}
