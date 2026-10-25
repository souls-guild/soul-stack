package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

type revokedJTIs map[string]bool

func (r revokedJTIs) IsJWTRevoked(id string) bool { return r[id] }

// A revoked token is answered by its own problem type, not the generic one:
// the holder needs a new token, not a new identity (NIM-1045).
func TestRequireJWT_RevokedJTIIsTyped(t *testing.T) {
	v, err := keeperjwt.NewVerifier(testSigningKey, testIssuer, revokedJTIs{"01JTESTJT1000000000000000A": true})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	h := RequireJWT(v)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a revoked token reached the handler")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+newToken(t, 3600e9))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), problem.TypeJWTRevoked) {
		t.Errorf("revoked token = %d %s, want 401 %s", rec.Code, rec.Body.String(), problem.TypeJWTRevoked)
	}
}

type useSpy struct{ id, ip, ua string }

func (s *useSpy) RecordUse(id, ip, ua string) { s.id, s.ip, s.ua = id, ip, ua }

func TestRecordJWTUse_HandsOnTokenAndClient(t *testing.T) {
	spy := &useSpy{}
	v, _ := keeperjwt.NewVerifier(testSigningKey, testIssuer, keeperjwt.NoRevocation)
	h := RequireJWT(v)(RecordJWTUse(spy)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.RemoteAddr = "10.9.8.7:51234"
	req.Header.Set("User-Agent", "sd-dictionary/1.0")
	req.Header.Set("Authorization", "Bearer "+newToken(t, 3600e9))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if spy.id != "01JTESTJT1000000000000000A" || spy.ip != "10.9.8.7" || spy.ua != "sd-dictionary/1.0" {
		t.Errorf("recorded %+v, want the token id, the peer IP without port and the User-Agent", spy)
	}
}

func TestWithClientMeta(t *testing.T) {
	var got ClientMeta
	h := WithClientMeta(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = ClientMetaFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/auth/ldap/login", nil)
	req.RemoteAddr = "192.0.2.1:4000"
	req.Header.Set("User-Agent", "Mozilla/5.0")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.IP != "192.0.2.1" || got.UserAgent != "Mozilla/5.0" {
		t.Errorf("ClientMeta = %+v", got)
	}
}

type credRBAC struct {
	revokedAID, revokedJTI string
}

func (c credRBAC) IsRevoked(aid string) bool   { return aid == c.revokedAID }
func (c credRBAC) IsJWTRevoked(id string) bool { return id == c.revokedJTI }

func TestCredentialRevoked(t *testing.T) {
	rc := credRBAC{revokedAID: "archon-gone", revokedJTI: "01JREV0KEDJWT0000000000000"}
	cases := []struct {
		name   string
		claims *keeperjwt.Claims
		want   bool
	}{
		{"live operator, live token", &keeperjwt.Claims{Subject: "archon-alice", ID: "01JG00DJWT0000000000000000"}, false},
		{"revoked operator", &keeperjwt.Claims{Subject: "archon-gone", ID: "01JG00DJWT0000000000000000"}, true},
		{"revoked token", &keeperjwt.Claims{Subject: "archon-alice", ID: "01JREV0KEDJWT0000000000000"}, true},
		{"legacy token without jti", &keeperjwt.Claims{Subject: "archon-alice"}, false},
		{"no claims", nil, true},
	}
	for _, c := range cases {
		if got := CredentialRevoked(rc, c.claims); got != c.want {
			t.Errorf("%s: CredentialRevoked = %v, want %v", c.name, got, c.want)
		}
	}
}

// The audit record names the token the action was taken with, so it can be
// traced past the operator to one token and where that token came from.
func TestAudit_RecordsTheActorToken(t *testing.T) {
	w := &captureWriter{}
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) })
	h := Audit(w, audit.EventOperatorCreated, nil, discardLogger())(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/operators", nil)
	req = req.WithContext(context.WithValue(req.Context(), claimsCtxKey{},
		&keeperjwt.Claims{Subject: "archon-alice", ID: "01JTESTJT1000000000000000A"}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	evs := w.Events()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if got := evs[0].Payload[audit.PayloadActorJWTID]; got != "01JTESTJT1000000000000000A" {
		t.Errorf("payload[%s] = %v, want the caller's jti", audit.PayloadActorJWTID, got)
	}
}
