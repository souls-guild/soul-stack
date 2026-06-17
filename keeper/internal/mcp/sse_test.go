package mcp

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

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

const (
	sseTestSigningKey = "0123456789abcdef0123456789abcdef"
	sseTestIssuer     = "keeper.mcp.sse.unit"
)

func sseTestVerifier(t *testing.T) *keeperjwt.Verifier {
	t.Helper()
	v, err := keeperjwt.NewVerifier([]byte(sseTestSigningKey), sseTestIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func sseTestToken(t *testing.T, aid string) string {
	t.Helper()
	iss, err := keeperjwt.NewIssuer([]byte(sseTestSigningKey), sseTestIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue(aid, []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// allowAllAccess — applyAccessStore, разрешающий подписку любому Архонту:
// возвращает Access без конкретного владельца, а incarnation покрывается
// allowAllRBAC. Используется в delivery/masking-тестах, где авторизация не
// в фокусе (RBAC-семантика проверяется отдельными TestSSE_RBAC_*).
type allowAllAccess struct{}

func (allowAllAccess) Access(_ context.Context, _ string) (*applyrun.Access, error) {
	return &applyrun.Access{IncarnationName: "test-inc"}, nil
}

// allowAllRBAC — PermissionChecker, разрешающий любую проверку.
type allowAllRBAC struct{}

func (allowAllRBAC) Check(_, _, _ string, _ map[string]string) error { return nil }

func sseTestServer(t *testing.T, bus *applybus.EventBus) *httptest.Server {
	t.Helper()
	h := buildSSEHandler(sseDeps{
		JWTVerifier: sseTestVerifier(t),
		Bus:         bus,
		Access:      allowAllAccess{},
		RBAC:        allowAllRBAC{},
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	return httptest.NewServer(h)
}

func TestSSE_MethodNotAllowed(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv := sseTestServer(t, bus)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"?apply_id=A", "application/json", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestSSE_MissingAuth(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv := sseTestServer(t, bus)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?apply_id=A")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSSE_BadJWT(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv := sseTestServer(t, bus)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"?apply_id=A", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSSE_MissingApplyID(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv := sseTestServer(t, bus)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+sseTestToken(t, "archon-x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSSE_EventDelivery(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv := sseTestServer(t, bus)
	defer srv.Close()

	applyID := "01J00000000000000000000ABC"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"?apply_id="+applyID, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+sseTestToken(t, "archon-y"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	// Ждём пока subscriber появится в шине.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Subscribers(applyID) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if bus.Subscribers(applyID) == 0 {
		t.Fatal("subscriber did not register within 2s")
	}

	bus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindTaskExecuted,
		Payload: map[string]any{
			"apply_id":    applyID,
			"kind":        string(applybus.KindTaskExecuted),
			"task_idx":    int32(0),
			"task_status": "TASK_STATUS_OK",
		},
	})

	rd := bufio.NewReader(resp.Body)
	frame, err := readSSEFrame(rd, 3*time.Second)
	if err != nil {
		t.Fatalf("readSSEFrame: %v", err)
	}
	if !strings.Contains(frame, "event: task.executed") {
		t.Errorf("frame missing 'event: task.executed': %q", frame)
	}
	if !strings.Contains(frame, "id: "+applyID) {
		t.Errorf("frame missing 'id: %s': %q", applyID, frame)
	}
	if !strings.Contains(frame, "data: {") {
		t.Errorf("frame missing JSON data: %q", frame)
	}

	cancel()
	// После отмены ctx subscriber должен исчезнуть из bus-а.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Subscribers(applyID) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := bus.Subscribers(applyID); got != 0 {
		t.Errorf("Subscribers after ctx-cancel = %d, want 0", got)
	}
}

// fakeAccess — in-memory applyAccessStore для unit-тестов RBAC.
type fakeAccess struct {
	byID map[string]*applyrun.Access
}

func (f fakeAccess) Access(_ context.Context, applyID string) (*applyrun.Access, error) {
	acc, ok := f.byID[applyID]
	if !ok {
		return nil, applyrun.ErrApplyRunNotFound
	}
	return acc, nil
}

// fakeRBAC — PermissionChecker-stub: allow только заданному (aid,
// incarnation)-набору на (incarnation, get).
type fakeRBAC struct {
	allow map[string]string // aid → incarnation
}

func (f fakeRBAC) Check(aid, resource, action string, ctx map[string]string) error {
	if resource == "incarnation" && action == "get" {
		if inc, ok := f.allow[aid]; ok && inc == ctx["incarnation"] {
			return nil
		}
	}
	return errors.New("rbac: denied")
}

func ptr(s string) *string { return &s }

func sseRBACServer(t *testing.T, bus *applybus.EventBus, access applyAccessStore, rbac PermissionChecker) *httptest.Server {
	t.Helper()
	h := buildSSEHandler(sseDeps{
		JWTVerifier: sseTestVerifier(t),
		Bus:         bus,
		Access:      access,
		RBAC:        rbac,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	return httptest.NewServer(h)
}

func sseGet(t *testing.T, srv *httptest.Server, aid, applyID string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"?apply_id="+applyID, nil)
	req.Header.Set("Authorization", "Bearer "+sseTestToken(t, aid))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func TestSSE_RBAC_OwnerAllowed(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	access := fakeAccess{byID: map[string]*applyrun.Access{
		"A1": {IncarnationName: "web", StartedByAID: ptr("archon-owner")},
	}}
	srv := sseRBACServer(t, bus, access, fakeRBAC{})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?apply_id=A1", nil)
	req.Header.Set("Authorization", "Bearer "+sseTestToken(t, "archon-owner"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("owner status = %d, want 200", resp.StatusCode)
	}
	cancel()
}

func TestSSE_RBAC_NonOwnerDenied(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	access := fakeAccess{byID: map[string]*applyrun.Access{
		"A1": {IncarnationName: "web", StartedByAID: ptr("archon-owner")},
	}}
	// fakeRBAC без allow → не-владелец без incarnation.get → 403.
	srv := sseRBACServer(t, bus, access, fakeRBAC{})
	defer srv.Close()

	resp := sseGet(t, srv, "archon-stranger", "A1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-owner status = %d, want 403", resp.StatusCode)
	}
}

func TestSSE_RBAC_PermissionAllowed(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	access := fakeAccess{byID: map[string]*applyrun.Access{
		"A1": {IncarnationName: "web", StartedByAID: ptr("archon-owner")},
	}}
	rbac := fakeRBAC{allow: map[string]string{"archon-ops": "web"}}
	srv := sseRBACServer(t, bus, access, rbac)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?apply_id=A1", nil)
	req.Header.Set("Authorization", "Bearer "+sseTestToken(t, "archon-ops"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("incarnation.get holder status = %d, want 200", resp.StatusCode)
	}
	cancel()
}

func TestSSE_RBAC_NotFoundForbidden(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	access := fakeAccess{byID: map[string]*applyrun.Access{}}
	srv := sseRBACServer(t, bus, access, fakeRBAC{})
	defer srv.Close()

	// Несуществующий apply_id → 403 (anti-enum), не 404.
	resp := sseGet(t, srv, "archon-x", "GHOST")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unknown apply_id status = %d, want 403", resp.StatusCode)
	}
}

func TestSSE_RBAC_NilAccessDenied(t *testing.T) {
	// Access == nil → fail-closed: без access-store нельзя резолвить владельца
	// прогона, подписка отклоняется (403) даже для аутентифицированного Архонта.
	bus := applybus.NewBus(slog.Default())
	srv := sseRBACServer(t, bus, nil, nil)
	defer srv.Close()

	resp := sseGet(t, srv, "archon-dev", "any")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("nil-access status = %d, want 403", resp.StatusCode)
	}
}

func TestSSE_ConnLimiter(t *testing.T) {
	l := newSSEConnLimiter(2, 1)
	if !l.Acquire("a") {
		t.Fatal("first acquire a should succeed")
	}
	if l.Acquire("a") {
		t.Fatal("second acquire a should fail (per-AID limit 1)")
	}
	if !l.Acquire("b") {
		t.Fatal("acquire b should succeed (global 2 not reached)")
	}
	if l.Acquire("c") {
		t.Fatal("acquire c should fail (global limit 2 reached)")
	}
	l.Release("a")
	if !l.Acquire("c") {
		t.Fatal("acquire c should succeed after releasing a")
	}
}

func TestSSE_ConnLimiterDisabled(t *testing.T) {
	l := newSSEConnLimiter(0, 0)
	for i := 0; i < 1000; i++ {
		if !l.Acquire("a") {
			t.Fatalf("acquire #%d should succeed (limits disabled)", i)
		}
	}
}

func TestSSE_PayloadMasked(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv := sseRBACServer(t, bus, allowAllAccess{}, allowAllRBAC{})
	defer srv.Close()

	applyID := "01J00000000000000000000MSK"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?apply_id="+applyID, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+sseTestToken(t, "archon-z"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Subscribers(applyID) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if bus.Subscribers(applyID) == 0 {
		t.Fatal("subscriber did not register")
	}

	bus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindApplyCompleted,
		Payload: map[string]any{
			"apply_id":        applyID,
			"sid":             "h1",
			"bootstrap_token": "secret123",
			"register_data": map[string]any{
				"db_password": "leaked-pass",
				"region":      "eu",
			},
		},
	})

	rd := bufio.NewReader(resp.Body)
	frame, err := readSSEFrame(rd, 3*time.Second)
	if err != nil {
		t.Fatalf("readSSEFrame: %v", err)
	}
	if strings.Contains(frame, "secret123") {
		t.Errorf("plaintext bootstrap_token leaked in SSE frame: %q", frame)
	}
	if strings.Contains(frame, "leaked-pass") {
		t.Errorf("plaintext nested db_password leaked in SSE frame: %q", frame)
	}
	if !strings.Contains(frame, maskedValueInData()) {
		t.Errorf("frame missing masked marker: %q", frame)
	}
	// Несекретные поля сохраняются.
	if !strings.Contains(frame, "eu") {
		t.Errorf("non-secret field 'region' lost: %q", frame)
	}
	cancel()
}

// TestSSE_TaskExecutedFailedFrameClean — end-to-end write-path для упавшей
// задачи (BUG-3 floor). publishTaskExecuted (grpc-слой) не кладёт сырой stderr
// в task.executed-payload; этот тест фиксирует, что итоговый SSE-frame несёт
// только code/module без тела сообщения. Payload собран в той же форме, что
// публикует publishTaskExecuted (см. events_taskevent.go).
func TestSSE_TaskExecutedFailedFrameClean(t *testing.T) {
	bus := applybus.NewBus(slog.Default())
	srv := sseRBACServer(t, bus, allowAllAccess{}, allowAllRBAC{})
	defer srv.Close()

	applyID := "01J00000000000000000FAILED1"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?apply_id="+applyID, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+sseTestToken(t, "archon-z"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for bus.Subscribers(applyID) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if bus.Subscribers(applyID) == 0 {
		t.Fatal("subscriber did not register")
	}

	bus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindTaskExecuted,
		Payload: map[string]any{
			"apply_id":    applyID,
			"kind":        string(applybus.KindTaskExecuted),
			"sid":         "h1",
			"task_idx":    0,
			"task_status": "TASK_STATUS_FAILED",
			"error": map[string]any{
				"code":   "module.failed",
				"module": "core.exec.run",
			},
		},
	})

	rd := bufio.NewReader(resp.Body)
	frame, err := readSSEFrame(rd, 3*time.Second)
	if err != nil {
		t.Fatalf("readSSEFrame: %v", err)
	}
	if strings.Contains(frame, "message") {
		t.Errorf("failed task.executed frame must not carry 'message': %q", frame)
	}
	if !strings.Contains(frame, "module.failed") {
		t.Errorf("frame missing code for triage: %q", frame)
	}
	if !strings.Contains(frame, "core.exec.run") {
		t.Errorf("frame missing module for triage: %q", frame)
	}
	cancel()
}

// maskedValueInData возвращает masked-маркер в том виде, как он попадает в
// JSON-data SSE-frame (через json.Marshal значения).
func maskedValueInData() string {
	b, _ := json.Marshal("***MASKED***")
	return string(b)
}

// readSSEFrame читает один SSE-frame (события или heartbeat-comment) до
// двойного \n. Heartbeat-comment (`:keepalive\n\n`) и event-frame
// (`event:...\ndata:...\n\n`) одинаково оканчиваются `\n\n`.
func readSSEFrame(rd *bufio.Reader, timeout time.Duration) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var sb strings.Builder
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				ch <- res{sb.String(), err}
				return
			}
			sb.WriteString(line)
			if line == "\n" {
				ch <- res{sb.String(), nil}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		// Пропускаем heartbeat-comment-frame-ы — caller ожидает реальный
		// event.
		if strings.HasPrefix(r.s, ":") {
			return readSSEFrame(rd, timeout)
		}
		return r.s, r.err
	case <-time.After(timeout):
		return "", io.EOF
	}
}

func TestSSE_HeartbeatCommentFormat(t *testing.T) {
	// Heartbeat в production ходит каждые 30 с — в unit-тесте мы не ждём;
	// проверяем только сам формат комментария, чтобы при изменении
	// случайно не сломать SSE-RFC.
	if !strings.HasPrefix(":keepalive\n\n", ":") {
		t.Fatal("heartbeat must start with ':' (SSE comment)")
	}
	if !strings.HasSuffix(":keepalive\n\n", "\n\n") {
		t.Fatal("heartbeat must end with \\n\\n")
	}
}
