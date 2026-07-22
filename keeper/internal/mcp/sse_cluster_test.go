//go:build integration

// Cluster-mode SSE-routing integration: two EventBus instances with
// different KIDs sharing one Redis. A publish on Bus-A must reach the
// SSE subscriber on Bus-B via `apply:<applyID>` pub/sub (ADR-006(c), M2.6).
//
// Run:
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

// startRedisContainer spins up a one-off Redis container for a single
// test. Returns addr (host:port) and a shutdown function. Kept outside the
// shared TestMain life-cycle for PG/Vault — adding Redis there would make
// sense with >1 test using it; for now there's just one.
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

// startMCPServerWithCustomBus starts an MCP listener with a pre-built bus
// (cluster-mode). Mirrors [startMCPServerWithBus], but the bus is built by
// the caller so it can pass in a Redis client and KID.
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

// TestIntegration_SSE_ClusterMode_EndToEnd — two EventBus instances with
// different KIDs on one Redis. A publish on Bus-A must reach the SSE
// client on Keeper-B (cross-Keeper routing). Conversely, a local publish on
// Bus-B reaches its local SSE client (without doubling via its own
// Redis echo).
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

	// Start a second MCP listener (Keeper-B) with SSE on top of busB.
	baseB, stopB := startMCPServerWithCustomBus(t, busB)
	defer stopB()

	token := newToken(t, "archon-alice", []string{"cluster-admin"})
	applyID := "01J0CLUSTER0000000000000XYZ"
	// The SSE subscription authorizes as the run's owner (started_by_aid == sub).
	seedApplyRunForSSE(t, applyID, "archon-alice")

	// SSE subscriber on Keeper-B.
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

	// Wait for local-subscribe (busB.Subscribers) and Redis-bridge.Ready to
	// register. startMCPServerWithCustomBus + Subscribe synchronously waits
	// for Ready inside applybus, but we don't control the HTTP-handler-ctx
	// side — check via busB.Subscribers instead.
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

	// Extra sleep — the Redis bridge may not have flushed the first message
	// yet. Subscribe in applybus waits for Ready, but there can be a couple
	// milliseconds between Ready and the handler on Keeper-A.
	time.Sleep(100 * time.Millisecond)

	// Publish on Keeper-A — the event must reach the SSE client via
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

	// Self-publish on Keeper-B — must arrive exactly once (local delivery),
	// not duplicated by the Redis echo (self-filter).
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

	// Extra check: no additional apply.completed should arrive (if the
	// self-filter were broken, a second copy would arrive as an echo from
	// the Redis bridge).
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
		// Acceptable: heartbeat (`:keepalive`) or timeout (io.EOF). NOT
		// acceptable: another data frame.
		if r.err == nil && strings.Contains(r.frame, "event:") {
			t.Errorf("unexpected duplicate frame after self-publish: %q", r.frame)
		}
	case <-time.After(time.Until(deadline) + 200*time.Millisecond):
		// OK — nothing extra arrived.
	}
}
