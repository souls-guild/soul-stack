package api

// REGRESSION NIM-37: live run progress (SSE) behind the FULL prod /v1 middleware. The
// existing huma_sse_test.go runs a trimmed router (RequireJWT + huma), where the huma
// BodyWriter unwraps directly to the chi *http.response (itself an http.Flusher) — so the
// flush "works" and the bug is invisible. In prod the events route is wrapped by the
// obs-metrics recorder (router.go MiddlewareForPath) — it embeds http.ResponseWriter without
// Unwrap/Flush, breaking the unwrapFlusher chain → flush=no-op → huma StreamResponse does not
// commit the response → the client gets 0 bytes. Here the router carries the same wrapper
// (obs-metrics + audit StatusRecorder), and the test requires the starting `:ok` preamble to
// reach the client BEFORE any event is published.

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// sseHarnessProdMiddleware — a /v1 router with the events route behind the PROD ResponseWriter
// wrappers: the obs-metrics recorder (like router.go) + the audit StatusRecorder. Both embed
// http.ResponseWriter; we check that the SSE flush passes through them to the socket.
func sseHarnessProdMiddleware(t *testing.T, bus *applybus.EventBus, access runEventsAccess, rbac apimiddleware.PermissionChecker) (*httptest.Server, func(aid string) string) {
	t.Helper()
	installHumaErrorOverride()
	verifier, err := keeperjwt.NewVerifier([]byte(sseAPISigningKey), sseAPIIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	issuer, err := keeperjwt.NewIssuer([]byte(sseAPISigningKey), sseAPIIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	metrics := obs.RegisterHTTPMetrics(obs.NewRegistry())
	// Wraps the writer in the shared audit StatusRecorder (as apimiddleware.Audit does on
	// write routes): the second link that must be transparent to flush/unwrap.
	auditRecorderWrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(apimiddleware.NewStatusRecorder(w), r)
		})
	}

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(metrics.MiddlewareForPath(nil)) // prod wrapper of the events route (router.go:261)
		r.Use(apimiddleware.RequireJWT(verifier))
		r.Route("/incarnations", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(auditRecorderWrap)
				deps := &runEventsDeps{
					Bus:     bus,
					Access:  access,
					RBAC:    rbac,
					Limiter: newSSEConnLimiter(sseMaxConnsGlobal, sseMaxConnsPerAID),
					Logger:  discardLogger(),
				}
				registerHumaIncarnationRunEvents(newHumaCadenceAPI(r), deps)
			})
		})
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	mint := func(aid string) string {
		tok, err := issuer.Issue(aid, []string{"cluster-admin"}, time.Hour, false)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		return tok
	}
	return srv, mint
}

// TestRunEventsSSE_PreambleFlushesThroughV1Middleware — the starting `:ok` preamble must
// reach the client while the handler is still streaming (we publish no events). Without
// Unwrap/Flush in the obs/audit recorders, huma StreamResponse does not commit the response →
// Do hangs until the deadline → the test reddens. With the fix the flush passes → 200 +
// preamble arrive immediately.
func TestRunEventsSSE_PreambleFlushesThroughV1Middleware(t *testing.T) {
	bus := applybus.NewBus(discardLogger())
	const applyID = "01APPLYFLUSHGUARD0000000000"
	srv, mint := sseHarnessProdMiddleware(t, bus,
		fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod", StartedByAID: ptrStr("archon-op")}},
		stubRBACChecker{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, sseURL(srv, "redis-prod", applyID), nil)
	req.Header.Set("Authorization", "Bearer "+mint("archon-op"))
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE-preamble не toлетел за 3s (флаш не прошёл сквозь /v1-обёртки?): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if line := readSSEPreambleLine(t, resp.Body); !strings.HasPrefix(line, ":ok") {
		t.Fatalf("первый flush-нутый куwithк = %q, want :ok-preamble", line)
	}
}

// readSSEPreambleLine reads the first line of the SSE stream (the starting `:ok` comment)
// with a timeout. Unlike readFirstSSEFrame it does NOT skip `:` comments — those are exactly
// what we check.
func readSSEPreambleLine(t *testing.T, r io.Reader) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(r).ReadString('\n')
		ch <- strings.TrimRight(line, "\r\n")
	}()
	select {
	case s := <-ch:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("таймаут reading SSE-preamble")
		return ""
	}
}
