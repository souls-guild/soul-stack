package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// The router resolves the client once, and the two readers that matter agree
// on it: the IP a minted session stores and the IP the login limiter keys on.
// X-Forwarded-For counts only from a peer in listen.trusted_proxies (ADR-058
// amendment NIM-1050) — from anyone else it is the client's own claim.
func TestRouter_XForwardedForOnlyFromATrustedProxy(t *testing.T) {
	ingress := apimiddleware.TrustedProxies{netip.MustParsePrefix("10.0.0.0/24")}
	for _, c := range []struct {
		name    string
		proxies apimiddleware.TrustedProxies
		peer    string
		want    string
	}{
		{"behind a trusted proxy", ingress, "10.0.0.5:4431", "203.0.113.9"},
		{"a peer outside the list", ingress, "198.51.100.7:4431", "198.51.100.7"},
		{"no list", nil, "10.0.0.5:4431", "10.0.0.5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			authn := &stubLocalAuthenticator{op: auth.MappedOperator{AID: "alice"}, token: "x"}
			guard := &lockingGuard{}
			h := authRouterWithGuard(t, nil, emptyEnforcer(t), nil, AuthMethodsDeps{Password: true, Local: true}, guard,
				withLocalAuth(&LocalAuthDeps{Authenticator: authn, TTL: time.Hour}), withTrustedProxies(c.proxies))

			body, _ := json.Marshal(LocalLoginRequest{AID: "alice", Password: "correct horse battery"})
			req := httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Forwarded-For", "198.18.0.1, 203.0.113.9")
			req.RemoteAddr = c.peer
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("login: %d; body=%s", rec.Code, rec.Body.String())
			}
			if authn.client.IP != c.want {
				t.Errorf("the session records IP %q, want %q", authn.client.IP, c.want)
			}
			if !slices.Contains(guard.asked, "ip:"+c.want) {
				t.Errorf("the login limiter keyed on %v, want ip:%s", guard.asked, c.want)
			}
		})
	}
}
