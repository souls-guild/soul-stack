package api

// Guard invariants for ADR-068 §A3 (the SSE route for a run's live progress). Focus — SECURITY:
// RBAC/anti-enum 403, secret hygiene of the frame payload, auth via the `*/events` chain
// (fetch-streaming Authorization: Bearer, §A0). Integration over a minimal
// /v1 router (RequireJWT + huma), without PG (fake access-store) and without /mcp/events.

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

// stubRBACChecker — a PermissionChecker with an explicit allow-set keyed by "resource.action".
type stubRBACChecker struct{ allow map[string]bool }

func (s stubRBACChecker) Check(_, resource, action string, _ map[string]string) error {
	if s.allow[resource+"."+action] {
		return nil
	}
	return errors.New("denied")
}

func ptrStr(s string) *string { return &s }

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// sseTestHarness — a minimal /v1 router (RequireJWT + huma) with the SSE route +
// a function that mints an operator JWT. access nil → run-events is not mounted.
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

// getStatus does a GET with Bearer and returns the status (closes the body immediately — for
// non-stream responses and a fast 4xx).
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

// === A3: authorizeRunEventsSSE (unit matrix) ===

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

// === A3: HTTP integration ===

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
	// apply_id belongs to other-inc, path is redis-prod → 403 (someone else's run).
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

// TestRunEventsSSE_OwnerStreamsAndMasks — the owner opens the stream (200 + text/event-stream),
// a published task.executed arrives as a frame `event: task.executed`, and a secret-shaped
// field in the payload is masked (H1 write-path barrier).
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

	// Wait for the subscriber to register, then publish an event with a secret-shaped field.
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
			"bootstrap_token": "s.SUPERSECRETVALUE", // secret-shaped → must be masked
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

// TestRunEventsSSE_QueryTokenAllowed — the SSE route also accepts ?access_token= (the canon
// middleware for */events), besides the main Authorization: Bearer (fetch-streaming
// §A0). Guard: the canonical query-token path is not broken by this slice.
func TestRunEventsSSE_QueryTokenAllowed(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	const applyID = "01APPLYQUERYTOK00000000000"
	srv, mint := sseTestHarness(t, bus, fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod", StartedByAID: ptrStr("archon-op")}}, stubRBACChecker{})

	url := sseURL(srv, "redis-prod", applyID) + "?access_token=" + mint("archon-op")
	resp, err := http.Get(url) // without an Authorization header
	if err != nil {
		t.Fatalf("GET query-token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("query-token status = %d, want 200 (middleware */events)", resp.StatusCode)
	}
}

// --- streaming test helpers ---

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

// readFirstSSEFrame reads the first event/data frame from the stream (skipping `:` comments
// and heartbeat), with a timeout.
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
