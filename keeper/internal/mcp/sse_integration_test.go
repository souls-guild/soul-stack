//go:build integration

// Integration-тест SSE-маршрута `/mcp/events`: реальный MCP-сервер на
// ephemeral port (общий harness из integration_test.go), apply-bus
// разделён с тестовым кодом — мы публикуем события напрямую и проверяем,
// что SSE-клиент получает их через HTTP.
//
// Запуск:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/mcp/...

package mcp

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/config"
)

// seedApplyRunForSSE кладёт минимальную цепочку operator → incarnation →
// apply_runs так, чтобы authorizeSSE (sse.go) разрешил подписку владельцу:
// started_by_aid = ownerAID == JWT.sub. ApplyAccessPG резолвит apply_id в эту
// строку, owner-ветка authorizeSSE даёт allow без RBAC-проверки. Без apply_run
// SSE fail-closed (apply_id не найден → 403, anti-enum).
func seedApplyRunForSSE(t *testing.T, applyID, ownerAID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE incarnation, operators, audit_log RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("seedApplyRunForSSE truncate: %v", err)
	}
	op := &operator.Operator{AID: ownerAID, DisplayName: ownerAID, AuthMethod: operator.AuthMethodJWT}
	if err := operator.Insert(ctx, integrationPool, op); err != nil {
		t.Fatalf("seedApplyRunForSSE operator: %v", err)
	}
	owner := ownerAID
	inc := &incarnation.Incarnation{
		Name: "sse-inc", Service: "noop", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: incarnation.StatusReady, CreatedByAID: &owner,
	}
	if err := incarnation.Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("seedApplyRunForSSE incarnation: %v", err)
	}
	if err := applyrun.Insert(ctx, integrationPool, &applyrun.ApplyRun{
		ApplyID: applyID, SID: "sse-host.example", IncarnationName: "sse-inc",
		Scenario: "create", Status: applyrun.StatusRunning, StartedByAID: &owner,
	}); err != nil {
		t.Fatalf("seedApplyRunForSSE apply_run: %v", err)
	}
}

// startMCPServerWithBus поднимает MCP-listener с пользовательской bus-шиной.
// Возвращает baseURL и саму шину, чтобы тест мог публиковать события
// напрямую (имитация publisher-а из events_taskevent.go/events_runresult.go,
// без gRPC-обвязки — отдельный slice integration-сценарий).
func startMCPServerWithBus(t *testing.T) (baseURL string, bus *applybus.EventBus, shutdown func()) {
	t.Helper()
	verifier, err := keeperjwt.NewVerifier([]byte(integrationSigningKey), integrationIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	issuer, err := keeperjwt.NewIssuer([]byte(integrationSigningKey), integrationIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	enforcer, err := rbactest.NewEnforcer(nil)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}

	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       integrationPool,
		Issuer:     issuer,
		RBAC:       enforcer,
		TTLDefault: time.Hour,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("operator.NewService: %v", err)
	}

	handler, err := NewHandler(HandlerDeps{
		OperatorSvc:   svc,
		RBAC:          enforcer,
		AuditWriter:   auditpg.NewWriter(integrationPool),
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		IncarnationDB: integrationPool,
	})
	if err != nil {
		t.Fatalf("mcp.NewHandler: %v", err)
	}

	bus = applybus.NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	// ApplyAccess + RBAC включают RBAC-проверку SSE-подписки (M1, authorizeSSE):
	// без ApplyAccess подписка fail-closed (deny → 403). Прод всегда прокидывает
	// ApplyAccessPG; harness — тот же путь поверх тест-пула. RBAC=enforcer для
	// ветки «не владелец» (тесты используют ветку владельца через started_by_aid).
	srv, err := NewServer(config.KeeperListenSimple{Addr: "127.0.0.1:0"}, ServerDeps{
		JWTVerifier: verifier,
		Handler:     handler,
		Bus:         bus,
		ApplyAccess: NewApplyAccessPG(integrationPool),
		RBAC:        enforcer,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("mcp.NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if addr := srv.Addr(); addr != "" && addr != "127.0.0.1:0" {
			baseURL = "http://" + addr
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("mcp server did not bind within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	shutdown = func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(15 * time.Second):
			t.Fatal("mcp server did not stop within 15s")
		}
	}
	return baseURL, bus, shutdown
}

func TestIntegration_SSE_EndToEnd(t *testing.T) {
	base, bus, stop := startMCPServerWithBus(t)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	applyID := "01J0SSE00000000000000ABCDE"
	// SSE-подписка авторизуется как владелец прогона (started_by_aid == sub).
	seedApplyRunForSSE(t, applyID, "archon-alice")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/mcp/events?apply_id="+applyID, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Ждём пока SSE-handler подписался в bus-е.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Subscribers(applyID) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if bus.Subscribers(applyID) == 0 {
		t.Fatal("SSE handler did not subscribe within 3s")
	}

	// Имитируем последовательность publisher-а events_taskevent.go +
	// events_runresult.go: одна задача + финальный SUCCESS.
	bus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindTaskExecuted,
		Payload: map[string]any{
			"apply_id":    applyID,
			"kind":        string(applybus.KindTaskExecuted),
			"sid":         "test-soul.example",
			"task_idx":    int32(0),
			"task_status": "TASK_STATUS_OK",
		},
	})
	bus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindApplyCompleted,
		Payload: map[string]any{
			"apply_id":   applyID,
			"kind":       string(applybus.KindApplyCompleted),
			"sid":        "test-soul.example",
			"run_status": "RUN_STATUS_SUCCESS",
		},
	})

	rd := bufio.NewReader(resp.Body)
	got1, err := readSSEFrame(rd, 3*time.Second)
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if !strings.Contains(got1, "event: task.executed") {
		t.Errorf("first frame: %q", got1)
	}
	got2, err := readSSEFrame(rd, 3*time.Second)
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if !strings.Contains(got2, "event: apply.completed") {
		t.Errorf("second frame: %q", got2)
	}

	cancel()
	// После disconnect bus должен оссвободить subscriber-а.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Subscribers(applyID) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := bus.Subscribers(applyID); got != 0 {
		t.Errorf("subscriber count after disconnect = %d, want 0", got)
	}
}

func TestIntegration_SSE_AuthRequired(t *testing.T) {
	base, _, stop := startMCPServerWithBus(t)
	defer stop()

	resp, err := http.Get(base + "/mcp/events?apply_id=foo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestIntegration_SSE_MissingApplyID(t *testing.T) {
	base, _, stop := startMCPServerWithBus(t)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	req, _ := http.NewRequest(http.MethodGet, base+"/mcp/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
