package api

// Guard tests of ROLLOUT BATCH 2a moving the SIGIL domain (plugins/sigils) ENTIRELY onto huma
// full-typed (ADR-054 §Pattern, role patterns). allow/revoke — WRITE+AUDIT (variant B,
// huma-audit-middleware; events plugin.allowed/plugin.revoked); list — read-bare
// (no audit). They prove the cluster invariants over chi:
//
//   - wire/golden: allow 201 {namespace,name,ref,sha256}; list 200 items[]; revoke 204
//     empty (byte-exact);
//   - unknown-field → 400; missing-required → 422; bad ref segment → 422; RBAC-deny → 403;
//   - S6-GUARD on EVERY write route (allow/revoke): full huma wiring writes an audit
//     event with a NON-EMPTY payload + the CORRECT event-type on 2xx and does NOT write on 4xx/403.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// sigilFixtureSHA — the deterministic sha256 of the fixture binary (golden allow).
var sigilFixtureSHA = func() string {
	d := sha256.Sum256([]byte("cloud-binary"))
	return hex.EncodeToString(d[:])
}()

// hsigilStore — a mock [sigil.Store] for the huma test (Insert/Revoke/ListActive).
type hsigilStore struct {
	revokeErr  error
	listResult []*sigil.Sigil
}

func (s *hsigilStore) Insert(context.Context, *sigil.Sigil) error { return nil }
func (s *hsigilStore) Revoke(context.Context, string, string, string, string) error {
	return s.revokeErr
}
func (s *hsigilStore) ListActive(context.Context) ([]*sigil.Sigil, error) { return s.listResult, nil }

// hsigilSlots — a mock [sigil.SlotReader]: a successful slot (cloud/hetzner) + commit_sha.
type hsigilSlots struct{}

func (hsigilSlots) ReadSlot(string, string) (*pluginhost.SlotContents, error) {
	return &pluginhost.SlotContents{
		BinaryPath:    "/cache/cloud-hetzner/soul-cloud-hetzner",
		ManifestBytes: []byte("kind: cloud_driver\nprotocol_version: 1\nnamespace: cloud\nname: hetzner\nspec:\n  profile_schema:\n    type: object\n"),
		BinarySHA256:  sigilFixtureSHA,
	}, nil
}
func (hsigilSlots) SlotCommitSHA(string, string) (string, error) {
	return "0123456789abcdef0123456789abcdef01234567", nil
}

// humaSigilRouter assembles a chi router with ALL sigil routes through huma — the production
// wiring from router.go: RequirePermission(plugin.<action>) on each group + (for
// write) huma-audit-middleware variant B + the huma operation. injectClaims replaces
// RequireJWT. store is parameterized (revoke list-result).
func humaSigilRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, store *hsigilStore) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer, err := sigil.NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	svc, err := sigil.NewService(sigil.ServiceDeps{Signer: signer, Store: store, Slots: hsigilSlots{}})
	if err != nil {
		t.Fatalf("sigil.NewService: %v", err)
	}
	sigilH := handlers.NewSigilHandler(svc, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/plugins/sigils", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "plugin", "allow", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSigilAllow(newHumaSigilAPI(r, auditW, audit.EventPluginAllowed, nil), sigilH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "plugin", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSigilList(newHumaCadenceAPI(r), sigilH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "plugin", "revoke", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaSigilRevoke(newHumaSigilAPI(r, auditW, audit.EventPluginRevoked, nil), sigilH)
			})
		})
	})
	return r
}

// === ALLOW (WRITE+AUDIT plugin.allowed) ===

func TestHumaSigil_Allow_GoldenWire(t *testing.T) {
	r := humaSigilRouter(t, strictAllowAll{}, nil, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/sigils", strings.NewReader(`{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	golden := `{"name":"hetzner","namespace":"cloud","ref":"v1.0.0","sha256":"` + sigilFixtureSHA + `"}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire drift sigil.allow:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaSigil_Allow_UnknownField_400(t *testing.T) {
	r := humaSigilRouter(t, strictAllowAll{}, nil, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/sigils", strings.NewReader(`{"namespace":"cloud","name":"hetzner","ref":"v1.0.0","bogus":1}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaSigil_Allow_MissingRef_422(t *testing.T) {
	r := humaSigilRouter(t, strictAllowAll{}, nil, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/sigils", strings.NewReader(`{"namespace":"cloud","name":"hetzner"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaSigil_Allow_RBACDeny_403(t *testing.T) {
	r := humaSigilRouter(t, strictDenyAll{}, nil, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/sigils", strings.NewReader(`{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_SigilAllow_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilRouter(t, strictAllowAll{}, auditCap, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/sigils", strings.NewReader(`{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventPluginAllowed, map[string]any{
		"namespace": "cloud", "name": "hetzner", "ref": "v1.0.0",
		"sha256": sigilFixtureSHA, "allowed_by_aid": "archon-alice",
	})
}

func TestHumaAudit_SigilAllow_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilRouter(t, strictDenyAll{}, auditCap, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/sigils", strings.NewReader(`{"namespace":"cloud","name":"hetzner","ref":"v1.0.0"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on RBAC-deny sigil.allow (%d events)", len(auditCap.Events()))
	}
}

// === LIST (READ-bare, no audit) ===

func TestHumaSigil_List_GoldenWire(t *testing.T) {
	allowedAt := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	store := &hsigilStore{listResult: []*sigil.Sigil{{
		Namespace:    "cloud",
		Name:         "hetzner",
		Ref:          "v1.0.0",
		SHA256:       sigilFixtureSHA,
		AllowedByAID: "archon-alice",
		AllowedAt:    allowedAt,
	}}}
	r := humaSigilRouter(t, strictAllowAll{}, nil, store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins/sigils", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	golden := `{"items":[{"allowed_at":"2026-06-13T10:00:00Z","allowed_by_aid":"archon-alice","name":"hetzner","namespace":"cloud","ref":"v1.0.0","sha256":"` + sigilFixtureSHA + `"}]}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire drift sigil.list:\n got  = %s\n want = %s", got, golden)
	}
}

func TestHumaSigil_List_GoldenEmpty(t *testing.T) {
	r := humaSigilRouter(t, strictAllowAll{}, nil, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins/sigils", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	const golden = `{"items":[]}`
	if got := strings.TrimSpace(rec.Body.String()); got != golden {
		t.Errorf("GOLDEN wire drift sigil.list (empty): got=%q want=%q", got, golden)
	}
}

func TestHumaSigil_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilRouter(t, strictAllowAll{}, auditCap, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins/sigils", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ route sigil.list recorded audit (%d events)", len(auditCap.Events()))
	}
}

func TestHumaSigil_List_RBACDeny_403(t *testing.T) {
	r := humaSigilRouter(t, strictDenyAll{}, nil, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins/sigils", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === REVOKE (WRITE+AUDIT plugin.revoked) ===

func TestHumaSigil_Revoke_204(t *testing.T) {
	r := humaSigilRouter(t, strictAllowAll{}, nil, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/plugins/sigils/cloud/hetzner/v1.0.0", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-body sigil.revoke must be empty, got %q", body)
	}
}

func TestHumaAudit_SigilRevoke_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilRouter(t, strictAllowAll{}, auditCap, &hsigilStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/plugins/sigils/cloud/hetzner/v1.0.0", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventPluginRevoked, map[string]any{
		"namespace": "cloud", "name": "hetzner", "ref": "v1.0.0",
	})
}

func TestHumaAudit_SigilRevoke_NoAudit_OnBadRef(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaSigilRouter(t, strictAllowAll{}, auditCap, &hsigilStore{})
	rec := httptest.NewRecorder()
	// ref with a space → invalid path segment → 422 (domain validateSigilTriple).
	req := httptest.NewRequest(http.MethodDelete, "/v1/plugins/sigils/cloud/hetzner/bad%20ref", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad ref segment); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit recorded on bad-ref revoke (%d events)", len(auditCap.Events()))
	}
}

// === OpenAPI fragment: ALL sigil operations from FULL-TYPED Go types ===

func TestHumaSigil_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaSigilSpecYAML()
	if err != nil {
		t.Fatalf("HumaSigilSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma fragment does not carry `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"allowPluginSigil", "listPluginSigils", "revokePluginSigil",
		"namespace", "sha256",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI fragment does not contain %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI fragment carries application/octet-stream:\n%s", frag)
	}
}
