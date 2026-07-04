package api

// Guard-инварианты ADR-068 §A0 (POST /v1/sse-token) и §A3 (SSE-route live-хода
// прогона). Фокус — БЕЗОПАСНОСТЬ: RBAC/anti-enum 403, секрет-гигиена frame-payload,
// query-token handshake, TTL короткоживущего токена. Интеграция поверх минимального
// /v1-роутера (RequireJWT + huma), без PG (fake access-store) и без /mcp/events.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
)

const (
	sseAPISigningKey = "0123456789abcdef0123456789abcdef"
	sseAPIIssuer     = "keeper.api.sse.unit"
)

// --- fakes ---

type fakeRunAccess struct {
	acc *applyrun.Access
	err error
}

func (f fakeRunAccess) Access(context.Context, string) (*applyrun.Access, error) {
	return f.acc, f.err
}

// stubRBACChecker — PermissionChecker с явным allow-set по "resource.action".
type stubRBACChecker struct{ allow map[string]bool }

func (s stubRBACChecker) Check(_, resource, action string, _ map[string]string) error {
	if s.allow[resource+"."+action] {
		return nil
	}
	return errors.New("denied")
}

func ptrStr(s string) *string { return &s }

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// sseTestHarness — минимальный /v1-роутер (RequireJWT + huma) с SSE-route и sse-token,
// + функция минтинга operator-JWT. access nil → run-events не монтируется.
func sseTestHarness(t *testing.T, bus *applybus.EventBus, access runEventsAccess, rbac apimiddleware.PermissionChecker) (*httptest.Server, func(aid string) string) {
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

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(apimiddleware.RequireJWT(verifier))
		r.Route("/incarnations", func(r chi.Router) {
			if access != nil {
				r.Group(func(r chi.Router) {
					deps := &runEventsDeps{
						Bus:     bus,
						Access:  access,
						RBAC:    rbac,
						Limiter: newSSEConnLimiter(sseMaxConnsGlobal, sseMaxConnsPerAID),
						Logger:  discardLogger(),
					}
					registerHumaIncarnationRunEvents(newHumaCadenceAPI(r), deps)
				})
			}
		})
		r.Group(func(r chi.Router) {
			registerHumaSseToken(newHumaCadenceAPI(r), newSseTokenHandler(issuer, discardLogger()))
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

func sseURL(srv *httptest.Server, name, applyID string) string {
	return srv.URL + "/v1/incarnations/" + name + "/runs/" + applyID + "/events"
}

// getStatus делает GET с Bearer и возвращает статус (тело закрывает сразу — для
// не-стрим-ответов и быстрого 4xx).
func getStatus(t *testing.T, url, bearer string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// === A3: authorizeRunEventsSSE (unit-матрица) ===

func TestAuthorizeRunEventsSSE_Matrix(t *testing.T) {
	const sub, name = "archon-op", "redis-prod"
	owned := &applyrun.Access{IncarnationName: name, StartedByAID: ptrStr(sub)}
	otherOwned := &applyrun.Access{IncarnationName: name, StartedByAID: ptrStr("archon-else")}
	foreign := &applyrun.Access{IncarnationName: "other-inc", StartedByAID: ptrStr("archon-else")}

	cases := []struct {
		name string
		deps *runEventsDeps
		want bool
	}{
		{"owner-allow", &runEventsDeps{Access: fakeRunAccess{acc: owned}, RBAC: stubRBACChecker{}}, true},
		{"get-allow", &runEventsDeps{Access: fakeRunAccess{acc: otherOwned}, RBAC: stubRBACChecker{allow: map[string]bool{"incarnation.get": true}}}, true},
		{"history-allow", &runEventsDeps{Access: fakeRunAccess{acc: otherOwned}, RBAC: stubRBACChecker{allow: map[string]bool{"incarnation.history": true}}}, true},
		{"no-perm-deny", &runEventsDeps{Access: fakeRunAccess{acc: otherOwned}, RBAC: stubRBACChecker{}}, false},
		{"not-found-deny", &runEventsDeps{Access: fakeRunAccess{err: applyrun.ErrApplyRunNotFound}, RBAC: stubRBACChecker{allow: map[string]bool{"incarnation.get": true}}}, false},
		{"foreign-incarnation-deny", &runEventsDeps{Access: fakeRunAccess{acc: foreign}, RBAC: stubRBACChecker{allow: map[string]bool{"incarnation.get": true}}}, false},
		{"nil-access-fail-closed", &runEventsDeps{Access: nil, RBAC: stubRBACChecker{allow: map[string]bool{"incarnation.get": true}}}, false},
		{"nil-rbac-not-owner-deny", &runEventsDeps{Access: fakeRunAccess{acc: otherOwned}, RBAC: nil}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := authorizeRunEventsSSE(context.Background(), c.deps, sub, name, "01APPLY00000000000000000000")
			if got != c.want {
				t.Errorf("authorizeRunEventsSSE = %v, want %v", got, c.want)
			}
		})
	}
}

// === A3: HTTP-интеграция ===

func TestRunEventsSSE_MissingAuth_401(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv, _ := sseTestHarness(t, bus, fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod"}}, stubRBACChecker{})
	if got := getStatus(t, sseURL(srv, "redis-prod", "01APPLY00000000000000000000"), ""); got != http.StatusUnauthorized {
		t.Errorf("no-auth status = %d, want 401 (RequireJWT активен)", got)
	}
}

func TestRunEventsSSE_GarbageToken_401(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv, _ := sseTestHarness(t, bus, fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod"}}, stubRBACChecker{})
	if got := getStatus(t, sseURL(srv, "redis-prod", "01APPLY00000000000000000000"), "not-a-jwt"); got != http.StatusUnauthorized {
		t.Errorf("garbage-token status = %d, want 401 (verifier активен)", got)
	}
}

func TestRunEventsSSE_ForbiddenNotFound_AntiEnum(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv, mint := sseTestHarness(t, bus, fakeRunAccess{err: applyrun.ErrApplyRunNotFound}, stubRBACChecker{allow: map[string]bool{"incarnation.get": true}})
	if got := getStatus(t, sseURL(srv, "redis-prod", "01NONEXISTENT0000000000000"), mint("archon-op")); got != http.StatusForbidden {
		t.Errorf("not-found status = %d, want 403 (anti-enum)", got)
	}
}

func TestRunEventsSSE_ForbiddenForeignIncarnation(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	// apply_id принадлежит other-inc, path — redis-prod → 403 (чужой прогон).
	srv, mint := sseTestHarness(t, bus, fakeRunAccess{acc: &applyrun.Access{IncarnationName: "other-inc", StartedByAID: ptrStr("archon-op")}}, stubRBACChecker{allow: map[string]bool{"incarnation.get": true}})
	if got := getStatus(t, sseURL(srv, "redis-prod", "01APPLY00000000000000000000"), mint("archon-op")); got != http.StatusForbidden {
		t.Errorf("foreign-incarnation status = %d, want 403", got)
	}
}

func TestRunEventsSSE_ForbiddenNoPerm(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv, mint := sseTestHarness(t, bus, fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod", StartedByAID: ptrStr("archon-else")}}, stubRBACChecker{})
	if got := getStatus(t, sseURL(srv, "redis-prod", "01APPLY00000000000000000000"), mint("archon-op")); got != http.StatusForbidden {
		t.Errorf("no-perm status = %d, want 403", got)
	}
}

// TestRunEventsSSE_OwnerStreamsAndMasks — owner открывает поток (200 + text/event-stream),
// опубликованное task.executed приходит frame-ом `event: task.executed`, а секрет-shaped
// поле в payload замаскировано (H1 write-path-барьер).
func TestRunEventsSSE_OwnerStreamsAndMasks(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	const applyID = "01APPLYOWNED00000000000000"
	srv, mint := sseTestHarness(t, bus, fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod", StartedByAID: ptrStr("archon-op")}}, stubRBACChecker{})

	req, _ := http.NewRequest(http.MethodGet, sseURL(srv, "redis-prod", applyID), nil)
	req.Header.Set("Authorization", "Bearer "+mint("archon-op"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Ждём регистрации подписчика, затем публикуем событие с секрет-shaped полем.
	waitSubscribed(t, bus, applyID)
	bus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindTaskExecuted,
		Payload: map[string]any{
			"apply_id":        applyID,
			"kind":            "task.executed",
			"sid":             "keeper",
			"task_idx":        int32(0),
			"task_status":     "TASK_STATUS_CHANGED",
			"bootstrap_token": "s.SUPERSECRETVALUE", // secret-shaped → должно замаскироваться
		},
	})

	ev, data := readFirstSSEFrame(t, resp.Body)
	if ev != "task.executed" {
		t.Errorf("event = %q, want task.executed", ev)
	}
	if strings.Contains(data, "SUPERSECRETVALUE") {
		t.Errorf("frame payload несёт незамаскированный секрет: %s", data)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("frame data не JSON: %v (%s)", err, data)
	}
	if payload["task_status"] != "TASK_STATUS_CHANGED" {
		t.Errorf("task_status в frame = %v, want TASK_STATUS_CHANGED", payload["task_status"])
	}
}

// TestRunEventsSSE_QueryTokenAllowed — доступ по ?access_token= (без Authorization):
// middleware канона берёт токен из query для */events. Доказывает A0→A3 транспорт.
func TestRunEventsSSE_QueryTokenAllowed(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	const applyID = "01APPLYQUERYTOK00000000000"
	srv, mint := sseTestHarness(t, bus, fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod", StartedByAID: ptrStr("archon-op")}}, stubRBACChecker{})

	url := sseURL(srv, "redis-prod", applyID) + "?access_token=" + mint("archon-op")
	resp, err := http.Get(url) // БЕЗ Authorization-header
	if err != nil {
		t.Fatalf("GET query-token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("query-token status = %d, want 200 (middleware */events)", resp.StatusCode)
	}
}

// === A0: POST /v1/sse-token ===

func TestSseToken_IssuesShortToken(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv, mint := sseTestHarness(t, bus, nil, nil)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sse-token", nil)
	req.Header.Set("Authorization", "Bearer "+mint("archon-op"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST sse-token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ExpiresIn != 60 {
		t.Errorf("expires_in = %d, want 60", body.ExpiresIn)
	}
	// Токен верифицируется тем же ключом, sub=оператор, TTL~60s.
	verifier, _ := keeperjwt.NewVerifier([]byte(sseAPISigningKey), sseAPIIssuer)
	claims, err := verifier.Verify(body.AccessToken)
	if err != nil {
		t.Fatalf("минтованный токен не верифицируется: %v", err)
	}
	if claims.Subject != "archon-op" {
		t.Errorf("sub = %q, want archon-op", claims.Subject)
	}
	ttl := claims.ExpiresAt.Sub(claims.IssuedAt)
	if ttl < 55*time.Second || ttl > 65*time.Second {
		t.Errorf("TTL = %s, want ~60s (короткоживущий)", ttl)
	}
}

func TestSseToken_MissingAuth_401(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv, _ := sseTestHarness(t, bus, nil, nil)
	resp, err := http.Post(srv.URL+"/v1/sse-token", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth status = %d, want 401 (Bearer обязателен)", resp.StatusCode)
	}
}

// TestSseToken_EndToEndHandshake — полный A0→A3: минтуем short-token через POST
// /v1/sse-token, открываем SSE по ?access_token=<short-jwt> → 200. Доказывает, что
// short-token валиден на SSE-route (тот же signing key, middleware верифицирует).
func TestSseToken_EndToEndHandshake(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	const applyID = "01APPLYHANDSHAKE0000000000"
	srv, mint := sseTestHarness(t, bus, fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod", StartedByAID: ptrStr("archon-op")}}, stubRBACChecker{})

	// 1) authed POST /v1/sse-token (Bearer) → short-token.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sse-token", nil)
	req.Header.Set("Authorization", "Bearer "+mint("archon-op"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST sse-token: %v", err)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body.AccessToken == "" {
		t.Fatal("short-token пуст")
	}

	// 2) EventSource-эквивалент: GET SSE ?access_token=<short-jwt> → 200.
	stream, err := http.Get(sseURL(srv, "redis-prod", applyID) + "?access_token=" + body.AccessToken)
	if err != nil {
		t.Fatalf("GET SSE handshake: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Errorf("handshake SSE status = %d, want 200", stream.StatusCode)
	}
}

// --- потоковые helper-ы теста ---

func waitSubscribed(t *testing.T, bus *applybus.EventBus, applyID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Subscribers(applyID) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("подписчик SSE не зарегистрировался за 2s")
}

// readFirstSSEFrame читает первый event/data-frame из потока (пропуская `:`-комментарии
// и heartbeat), с таймаутом.
func readFirstSSEFrame(t *testing.T, r io.Reader) (event, data string) {
	t.Helper()
	type frame struct{ event, data string }
	ch := make(chan frame, 1)
	go func() {
		br := bufio.NewReader(r)
		var ev string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- frame{}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ch <- frame{ev, strings.TrimPrefix(line, "data: ")}
				return
			}
		}
	}()
	select {
	case f := <-ch:
		return f.event, f.data
	case <-time.After(3 * time.Second):
		t.Fatal("таймаут чтения SSE-frame")
		return "", ""
	}
}
