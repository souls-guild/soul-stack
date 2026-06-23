//go:build integration

// Cluster-mode SSE-routing integration: два EventBus-инстанса с разными
// KID, общий Redis. Publish на Bus-A должен дойти до SSE-подписчика на
// Bus-B через `apply:<applyID>` pub/sub (ADR-006(c), M2.6).
//
// Запуск:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/mcp/...

package mcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	clusterRedisImage = "redis:7-alpine"
)

// startRedisContainer поднимает одноразовый Redis-контейнер для одного
// теста. Возвращает addr (host:port) и shutdown-функцию. Контейнер
// держится с TestMain-life-cycle общего PG/Vault — добавлять Redis в
// общий TestMain имело бы смысл, если бы тестов > 1; пока единичный.
func startRedisContainer(t *testing.T) (addr string, shutdown func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        clusterRedisImage,
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(30 * time.Second),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		if requireDocker() {
			t.Fatalf("redis container start: %v", err)
		}
		t.Skipf("redis container unavailable: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("MappedPort: %v", err)
	}
	addr = fmt.Sprintf("%s:%s", host, port.Port())
	shutdown = func() {
		tctx, tc := context.WithTimeout(context.Background(), 15*time.Second)
		defer tc()
		if err := ctr.Terminate(tctx); err != nil {
			log.Printf("redis container terminate: %v", err)
		}
	}
	return addr, shutdown
}

// startMCPServerWithCustomBus поднимает MCP-listener с заранее собранной
// шиной (cluster-mode). Симметрично [startMCPServerWithBus], но bus
// строится в caller-е, чтобы передать Redis-клиент и KID.
func startMCPServerWithCustomBus(t *testing.T, bus *applybus.EventBus) (baseURL string, shutdown func()) {
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
		if a := srv.Addr(); a != "" && a != "127.0.0.1:0" {
			baseURL = "http://" + a
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
	return baseURL, shutdown
}

// TestIntegration_SSE_ClusterMode_EndToEnd — два EventBus-инстанса с
// разными KID на одном Redis. Publish на Bus-A должен прийти SSE-клиенту
// Keeper-B (cross-Keeper routing). И наоборот local-publish на Bus-B
// доходит local-SSE-клиенту (без удвоения от собственного Redis-echo).
func TestIntegration_SSE_ClusterMode_EndToEnd(t *testing.T) {
	redisAddr, shutdownRedis := startRedisContainer(t)
	defer shutdownRedis()

	ctxClient, cancelClient := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelClient()

	cA, err := keeperredis.NewClient(ctxClient, keeperredis.Config{Addr: redisAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(A): %v", err)
	}
	defer cA.Close()
	cB, err := keeperredis.NewClient(ctxClient, keeperredis.Config{Addr: redisAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(B): %v", err)
	}
	defer cB.Close()

	lg := slog.New(slog.NewJSONHandler(io.Discard, nil))
	busA := applybus.NewBusWithRedis(lg, cA, "keeper-A")
	busB := applybus.NewBusWithRedis(lg, cB, "keeper-B")

	// Поднимаем второй MCP-listener (Keeper-B) c SSE поверх busB.
	baseB, stopB := startMCPServerWithCustomBus(t, busB)
	defer stopB()

	token := newToken(t, "archon-alice", []string{"cluster-admin"})
	applyID := "01J0CLUSTER0000000000000XYZ"
	// SSE-подписка авторизуется как владелец прогона (started_by_aid == sub).
	seedApplyRunForSSE(t, applyID, "archon-alice")

	// SSE-подписчик на Keeper-B.
	req, err := http.NewRequestWithContext(ctxClient, http.MethodGet,
		baseB+"/mcp/events?apply_id="+applyID, nil)
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

	// Ждём пока local-subscribe (busB.Subscribers) и Redis-bridge.Ready
	// зарегистрировались. На startMCPServerWithCustomBus + Subscribe это
	// синхронно дожидается Ready в applybus, но мы не контролируем
	// HTTP-handler-ctx-side — проверяем через busB.Subscribers.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if busB.Subscribers(applyID) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if busB.Subscribers(applyID) == 0 {
		t.Fatal("SSE handler on Keeper-B did not subscribe within 3s")
	}

	// Дополнительный sleep — Redis-bridge может ещё не пропинговать
	// первое сообщение. Subscribe в applybus дожидается Ready, но
	// между Ready и handle-в-Keeper-A может быть пара миллисекунд.
	time.Sleep(100 * time.Millisecond)

	// Publish на Keeper-A — событие должно прийти SSE-клиенту через
	// Redis pub/sub.
	busA.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindTaskExecuted,
		Payload: map[string]any{
			"apply_id":    applyID,
			"kind":        string(applybus.KindTaskExecuted),
			"sid":         "host.example",
			"task_idx":    int32(0),
			"task_status": "TASK_STATUS_OK",
		},
	})

	rd := bufio.NewReader(resp.Body)
	frame, err := readSSEFrame(rd, 5*time.Second)
	if err != nil {
		t.Fatalf("readSSEFrame: %v", err)
	}
	if !strings.Contains(frame, "event: task.executed") {
		t.Errorf("cross-Keeper frame = %q, want event: task.executed", frame)
	}
	if !strings.Contains(frame, "host.example") {
		t.Errorf("frame missing payload data: %q", frame)
	}

	// Self-publish на Keeper-B — должно прийти ровно один раз
	// (local-доставка), не дублироваться от Redis-echo (self-filter).
	busB.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindApplyCompleted,
		Payload: map[string]any{
			"apply_id":   applyID,
			"kind":       string(applybus.KindApplyCompleted),
			"sid":        "host.example",
			"run_status": "RUN_STATUS_SUCCESS",
		},
	})

	frame2, err := readSSEFrame(rd, 5*time.Second)
	if err != nil {
		t.Fatalf("readSSEFrame(self): %v", err)
	}
	if !strings.Contains(frame2, "event: apply.completed") {
		t.Errorf("self frame = %q, want event: apply.completed", frame2)
	}

	// Дополнительный draft: ещё одного apply.completed быть не должно
	// (если бы self-filter не работал, второй раз пришёл бы echo от
	// Redis-bridge-а).
	deadline = time.Now().Add(500 * time.Millisecond)
	type readRes struct {
		frame string
		err   error
	}
	extraCh := make(chan readRes, 1)
	go func() {
		f, e := readSSEFrame(rd, 400*time.Millisecond)
		extraCh <- readRes{f, e}
	}()
	select {
	case r := <-extraCh:
		// Допустимо: heartbeat (`:keepalive`) или таймаут (io.EOF). НЕ
		// допустимо: ещё один data-frame.
		if r.err == nil && strings.Contains(r.frame, "event:") {
			t.Errorf("unexpected duplicate frame after self-publish: %q", r.frame)
		}
	case <-time.After(time.Until(deadline) + 200*time.Millisecond):
		// OK — ничего лишнего не пришло.
	}
}
