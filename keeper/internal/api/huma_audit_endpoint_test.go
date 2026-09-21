package api

// The guard set for the FOURTH tier of ADR-054 (read-with-typed-query) for GET
// /v1/audit on huma full-typed. Proves the CONTRACT invariant 400/422 (decision A
// 2026-06-13, continuation of the ADR-051 Amendment), previously held by the
// strict bind phase:
//
//   - bad date-time (started_after) → 400 problem+json (huma parseInto → error-
//     override hasQueryParseError);
//   - bad int (offset/limit) → 400 (the same parse detection);
//   - KEY: bad source-enum → 422 (schema-validate enum mismatch — a different
//     Message, not in the parse set → falls to the default 422 branch). Proves
//     that the detection distinguishes a parse error from an enum mismatch on
//     the same query.-Location;
//   - valid date-time/enum/pagination → 200 + golden envelope {items,offset,limit,total}
//     byte-exact (PagedResponse equivalence preserved);
//   - OpenAPI fragment: typed query-params (date-time/int/enum), no requestBody on GET.
//
// The router is assembled with the literal production wiring from router.go:
// RequirePermission(audit.read) on the group + the huma operation, WITHOUT the
// huma-audit-middleware (READ). pool — auditpg.Reader over q400ListPool
// (COUNT→0, SELECT→empty). cfg.CreateHooks=nil keeps newHumaCadenceAPI (no
// $schema body decoration).

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
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// humaAuditRouter assembles a chi router with GET /v1/audit via huma — the
// wiring from router.go: RequirePermission(audit.read) on the group + the huma
// operation (READ, without the audit middleware). installHumaErrorOverride is
// explicit. injectClaims replaces RequireJWT.
func humaAuditRouter(t *testing.T, enforcer apimiddleware.PermissionChecker) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	auditH := handlers.NewAuditHandler(auditpg.NewReader(q400ListPool{}), nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "audit", "read", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaAuditList(newHumaCadenceAPI(r), auditH)
		})
	})
	return r
}

func auditGet(t *testing.T, r *chi.Mux, rawurl string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, rawurl, http.NoBody)
	r.ServeHTTP(rec, req)
	return rec
}

// TestHumaAudit_BadStartedAfter_400 — a bad date-time typed-query → 400 problem+json
// (huma parseInto "invalid date/time …" → hasQueryParseError).
func TestHumaAudit_BadStartedAfter_400(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?started_after=yesterday")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad started_after -> parse-detect 400); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaAudit_BadOffset_400 — bad int offset → 400 (parseInto "invalid integer").
func TestHumaAudit_BadOffset_400(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?offset=notanint")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad offset -> parse-detect 400); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaAudit_BadLimit_400 — bad int limit → 400 (parseInto "invalid integer").
func TestHumaAudit_BadLimit_400(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?limit=abc")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad limit -> parse-detect 400); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaAudit_OutOfRangePagination_400 — the CONTRACT invariant for bounds
// (decision A): out-of-range offset/limit → 400 (api.CheckPageBounds in
// ListTyped), NOT 422 (huma carries no schema-minimum/maximum). Must match the
// legacy/strict ParsePage (limit=0/1001/offset<0 → 400), otherwise a wire change.
func TestHumaAudit_OutOfRangePagination_400(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	for _, c := range []string{
		"/v1/audit?limit=0",
		"/v1/audit?limit=1001",
		"/v1/audit?offset=-1",
	} {
		rec := auditGet(t, r, c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (out-of-range → 400, parity ParsePage, NOT huma-422); body=%s", c, rec.Code, rec.Body.String())
			continue
		}
		assertHumaProblem(t, rec, problem.TypeMalformedRequest)
	}
}

// TestHumaAudit_BadSource_422 — the KEY contract guard: bad source-enum → 422
// (schema-validate enum mismatch — Message "expected value to be one of …", NOT
// in the parse set → falls to the default 422 branch). Proves that the parse
// detection does NOT catch an enum mismatch: the same query.-Location, the
// discriminator is the Message literal.
func TestHumaAudit_BadSource_422(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?source=hax0r")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad source-enum -> 422, parse-detect does NOT catch it); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// TestHumaAudit_ConfigBootstrapSource_200 — a MAJOR REGRESSION guard (adverse
// verification 2026-06-13): config_bootstrap is a valid source (audit.Source.Valid()
// accepts it, it is actually emitted by push/auto_import.go). The reference
// enum tag omitted it → huma would have rejected a working filter with 422
// (a wire regression 200→422 vs. the legacy strict, which did NOT validate the
// enum on bind). Fix: the enum tag = the FULL domain valid set. The guard
// asserts 200 (NOT 422) — config_bootstrap passes both the huma enum AND the
// domain Source.Valid().
func TestHumaAudit_ConfigBootstrapSource_200(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?source=config_bootstrap")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (config_bootstrap - valid source, NOT 422; enum tag = domain-valid-set); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaAudit_ValidFilters_200 — valid date-time/enum/pagination pass the bind →
// ListTyped (an empty reader) → 200 + golden envelope {items,offset,limit,total}
// byte-exact. items=[] (non-nil), pagination = the values passed in.
func TestHumaAudit_ValidFilters_200(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?source=api&source=mcp&started_after=2026-05-25T00:00:00Z&offset=10&limit=20")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// Re-marshal through a map → deterministic key order; the golden pins
	// the envelope's set/shape (items=[] non-nil, offset/limit echo, total=0).
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[],"limit":20,"offset":10,"total":0}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-drift audit-envelope:\n got  = %s\n want = %s", got, golden)
	}
}

// TestHumaAudit_DefaultPagination_200 — omitted offset/limit → default (0/50),
// matching shared/api.ParsePage (otherwise a wire change). Confirms the bounds.
func TestHumaAudit_DefaultPagination_200(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN drift default-pagination (must match ParsePage offset=0 limit=50):\n got  = %s\n want = %s", got, golden)
	}
}

// TestHumaAudit_RBACDeny_403 — an RBAC deny on the group is inherited by huma (403 BEFORE bind).
func TestHumaAudit_RBACDeny_403(t *testing.T) {
	r := humaAuditRouter(t, strictDenyAll{})
	rec := auditGet(t, r, "/v1/audit")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaAudit_OpenAPIFragment_3_1 — the fragment from FULL-TYPED Go types: typed
// query-params (date-time/int/enum), no requestBody on GET.
func TestHumaAudit_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaAuditSpecYAML()
	if err != nil {
		t.Fatalf("HumaAuditSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma fragment does not carry `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"listAuditEvents",
		"in: query",
		"started_after",
		"date-time",
		"keeper_internal",  // a source-enum value
		"config_bootstrap", // MAJOR fix: enum tag = domain valid set (includes config_bootstrap)
		"offset",
		"limit",
		"explode: true", // MINOR fix: multi-value []string query params (type/source) carry explode:true
		"int32",         // MINOR fix: offset/limit are int32 (match committed OffsetQuery/LimitQuery), NOT int64
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI fragment does not contain %q:\n%s", want, frag)
		}
	}
	// MINOR-3 scope — SPECIFICALLY the query params offset/limit must be int32
	// (match committed OffsetQuery/LimitQuery). The int64 negative check is
	// ONLY over the operation's `parameters:` block (between `parameters:` and
	// `responses:`); the body envelope AuditEventListReply (items/offset/limit/
	// total) and shared HumaProblemError.status carry int64 as the Go-int
	// response schema — that is NOT the subject of the query-tier fix.
	_, afterParams, hasParams := strings.Cut(frag, "parameters:")
	paramsBlock, _, hasResponses := strings.Cut(afterParams, "responses:")
	if !hasParams || !hasResponses {
		t.Fatalf("fragment has no `parameters:`...`responses:` block - operation structure changed, int64 negative check is invalid:\n%s", frag)
	}
	if strings.Contains(paramsBlock, "int64") {
		t.Errorf("operation query params carry int64 (offset/limit must be int32):\n%s", paramsBlock)
	}
	// GET has no body: requestBody must not be present on the operation.
	if strings.Contains(frag, "requestBody") {
		t.Errorf("GET /v1/audit fragment carries requestBody (GET must not have a body):\n%s", frag)
	}
	// the fourth tier does not carry a RawBody octet-stream artifact.
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI fragment carries application/octet-stream:\n%s", frag)
	}
}
