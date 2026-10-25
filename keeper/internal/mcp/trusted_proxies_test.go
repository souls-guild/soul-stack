package mcp

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/shared/config"
)

type ipRecorder struct{ ips []string }

func (r *ipRecorder) RecordUse(_, ip, _ string) { r.ips = append(r.ips, ip) }

// The MCP listener resolves the client behind listen.trusted_proxies the same
// way the OpenAPI one does (ADR-058 amendment NIM-1050): the token's
// last_used_ip is the client a listed proxy names, never what an unlisted peer
// claims.
func TestServer_ResolvesTheClientBehindATrustedProxy(t *testing.T) {
	for _, c := range []struct {
		name, peer, want string
	}{
		{"a listed proxy", "10.0.0.5:4431", "203.0.113.9"},
		{"an unlisted peer", "198.51.100.7:4431", "198.51.100.7"},
	} {
		t.Run(c.name, func(t *testing.T) {
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			rec := &ipRecorder{}
			s, err := NewServer(config.KeeperListenSimple{Addr: "127.0.0.1:0"}, ServerDeps{
				JWTVerifier:    sseTestVerifier(t),
				Handler:        &Handler{},
				Bus:            applybus.NewBus(logger),
				RBAC:           allowAllRBAC{},
				Logger:         logger,
				JWTUse:         rec,
				TrustedProxies: middleware.TrustedProxies{netip.MustParsePrefix("10.0.0.0/24")},
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			req := httptest.NewRequest(http.MethodGet, "/mcp/events", nil)
			req.RemoteAddr = c.peer
			req.Header.Set("Authorization", "Bearer "+sseTestToken(t, "archon-y"))
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
			s.srv.Handler.ServeHTTP(httptest.NewRecorder(), req)
			if len(rec.ips) != 1 || rec.ips[0] != c.want {
				t.Fatalf("recorded use from %v, want [%s]", rec.ips, c.want)
			}
		})
	}
}
