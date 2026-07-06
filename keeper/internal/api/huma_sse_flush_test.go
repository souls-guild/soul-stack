package api

// РЕГРЕСС NIM-37: live-ход прогона (SSE) за ПОЛНЫМ прод-middleware /v1. Существующий
// huma_sse_test.go гоняет урезанный роутер (RequireJWT + huma), где huma BodyWriter
// разворачивается прямо в chi *http.response (сам http.Flusher) — потому флаш «работает»
// и баг невидим. В проде events-роут обёрнут obs-metrics-рекордером (router.go
// MiddlewareForPath) — он встраивает http.ResponseWriter без Unwrap/Flush, рвёт цепочку
// unwrapFlusher → flush=no-op → huma StreamResponse не коммитит ответ → клиент 0 байт.
// Здесь роутер несёт ту же обёртку (obs-metrics + audit-StatusRecorder), и тест требует,
// чтобы стартовый `:ok`-preamble долетел ДО публикации любого события.

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

// sseHarnessProdMiddleware — /v1-роутер с events-роутом за ПРОД-обёртками ResponseWriter:
// obs-metrics-рекордер (как router.go) + audit StatusRecorder. Оба встраивают
// http.ResponseWriter; проверяем, что флаш SSE проходит сквозь них до сокета.
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
	// Оборачивает writer в общий audit StatusRecorder (как делает apimiddleware.Audit на
	// write-роутах): второе звено, которое обязано быть прозрачным для flush/unwrap.
	auditRecorderWrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(apimiddleware.NewStatusRecorder(w), r)
		})
	}

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(metrics.MiddlewareForPath(nil)) // прод-обёртка events-роута (router.go:261)
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

// TestRunEventsSSE_PreambleFlushesThroughV1Middleware — стартовый `:ok`-preamble обязан
// долететь до клиента, пока хендлер ещё стримит (событий не публикуем). Без Unwrap/Flush
// в obs/audit-рекордерах huma StreamResponse не коммитит ответ → Do висит до дедлайна →
// тест краснеет. С фиксом флаш проходит → 200 + preamble приходят сразу.
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
		t.Fatalf("SSE-preamble не долетел за 3s (флаш не прошёл сквозь /v1-обёртки?): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if line := readSSEPreambleLine(t, resp.Body); !strings.HasPrefix(line, ":ok") {
		t.Fatalf("первый flush-нутый кусок = %q, want :ok-preamble", line)
	}
}

// readSSEPreambleLine читает первую строку SSE-потока (стартовый `:ok`-комментарий) с
// таймаутом. В отличие от readFirstSSEFrame НЕ пропускает `:`-комментарии — их и проверяем.
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
		t.Fatal("таймаут чтения SSE-preamble")
		return ""
	}
}
