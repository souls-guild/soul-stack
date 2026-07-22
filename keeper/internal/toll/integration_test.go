//go:build integration

// Integration test for Toll extensions (ADR-038 amendment 2026-05-27): per-coven
// trigger end-to-end through a REAL Redis (testcontainers redis:7) + mock
// webhook-server (httptest). Covers:
//
//   - Watcher → Publisher → ZADD → Leader → ZRANGEBYSCORE → per-coven group →
//     SetDegraded + Notify;
//   - WebhookNotifier POST to mock-server (generic payload).
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/toll/...

package toll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

const integrationRedisImage = "redis:7-alpine"

var integrationAddr string

func TestMain(m *testing.M) { os.Exit(integrationRun(m)) }

func integrationRun(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        integrationRedisImage,
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		if requireDocker() {
			log.Fatalf("toll integration: container setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("toll integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		tctx, tc := context.WithTimeout(context.Background(), 30*time.Second)
		defer tc()
		_ = ctr.Terminate(tctx)
	}()

	host, err := ctr.Host(ctx)
	if err != nil {
		log.Printf("toll integration: container host: %v", err)
		return 1
	}
	port, err := ctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		log.Printf("toll integration: mapped port: %v", err)
		return 1
	}
	integrationAddr = fmt.Sprintf("%s:%s", host, port.Port())
	return m.Run()
}

// requireDocker — opt-in strict mode: in CI
// SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 is set so that docker unavailability
// causes a fail instead of a skip (as in the existing
// keeper/internal/redis/require_docker_test.go).
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}

// integrationLogger — discard-logger, shared by all integration tests.
func integrationLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// publishAdapter — thin ZADD publisher on top of *keeperredis.Client. Duplicates
// the daemon adapter for self-contained tests (the daemon pulls in a lot of dependencies).
type publishAdapter struct{ c *keeperredis.Client }

func (p *publishAdapter) PublishDisconnect(ctx context.Context, sid, kid, coven string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	member := EncodeDisconnect(sid, kid, coven, at)
	return keeperredis.PublishTollDisconnect(ctx, p.c, member, at.Unix())
}

// sortedSetAdapter — implements SortedSetReader + CovenAwareReader.
type sortedSetAdapter struct{ c *keeperredis.Client }

func (a *sortedSetAdapter) CountInWindow(ctx context.Context, fromUnix, toUnix int64) (int64, error) {
	return keeperredis.TollCountInWindow(ctx, a.c, fromUnix, toUnix)
}
func (a *sortedSetAdapter) TrimBelow(ctx context.Context, beforeUnix int64) error {
	return keeperredis.TollTrimBelow(ctx, a.c, beforeUnix)
}
func (a *sortedSetAdapter) CountByCovenInWindow(ctx context.Context, fromUnix, toUnix int64) (map[string]int64, error) {
	return keeperredis.TollCountByCovenInWindow(ctx, a.c, fromUnix, toUnix)
}

type degradedAdapter struct{ c *keeperredis.Client }

func (a *degradedAdapter) SetDegraded(ctx context.Context, holder string, ttl time.Duration) error {
	return keeperredis.TollSetDegraded(ctx, a.c, holder, ttl)
}
func (a *degradedAdapter) ClearDegraded(ctx context.Context) error {
	return keeperredis.TollClearDegraded(ctx, a.c)
}

type leaseAdapter struct{ c *keeperredis.Client }

func (a *leaseAdapter) Acquire(ctx context.Context, key, holder string, ttl time.Duration) (Lease, error) {
	lease, err := keeperredis.Acquire(ctx, a.c, key, holder, ttl)
	if err != nil {
		if errors.Is(err, keeperredis.ErrLeaseTaken) {
			return nil, ErrLeaseTaken
		}
		return nil, err
	}
	return &leaseWrapper{lease: lease}, nil
}

type leaseWrapper struct{ lease *keeperredis.Lease }

func (l *leaseWrapper) Renew(ctx context.Context) error {
	if err := l.lease.Renew(ctx); err != nil {
		if errors.Is(err, keeperredis.ErrLeaseLost) {
			return ErrLeaseLost
		}
		return err
	}
	return nil
}
func (l *leaseWrapper) Release(ctx context.Context) error { return l.lease.Release(ctx) }

// fixedBaseline — fixed snapshot of connected-souls for the test.
type fixedBaseline struct{ value int64 }

func (b *fixedBaseline) BaselineConnected(context.Context) (int64, error) { return b.value, nil }

// TestIntegration_PerCovenTrigger_WithWebhook — end-to-end:
//  1. Start a mock webhook-server (httptest).
//  2. Connect to a real Redis.
//  3. Watcher publishes 15 disconnects into coven=production-eu (rate 0.15 > 0.10).
//  4. Leader ticks, groups by coven via ZRANGEBYSCORE, sees the threshold exceeded,
//     sets cluster:degraded + notifies the webhook.
//  5. Verify: the key is in Redis (TollIsDegraded=true), the webhook got a POST,
//     the payload contains coven_name=production-eu.
func TestIntegration_PerCovenTrigger_WithWebhook(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Mock webhook receiver.
	var (
		mu           sync.Mutex
		webhookCalls []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		webhookCalls = append(webhookCalls, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 2. Redis client.
	rc, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer rc.Close()

	// Clean slate (in case the test is re-run).
	_ = keeperredis.TollTrimBelow(ctx, rc, time.Now().Unix()+1)
	_ = keeperredis.TollClearDegraded(ctx, rc)

	// 3. Watcher → publish 15 disconnects under coven=production-eu.
	logger := integrationLogger()
	watcher, err := NewWatcher(Config{KID: "kid-int", WarmupDelay: time.Nanosecond},
		&publishAdapter{c: rc}, nil, logger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	// Push startedAt into the past — warmup has expired.
	watcher.setStartedAt(time.Now().Add(-time.Hour))
	for i := 0; i < 15; i++ {
		watcher.NotifyDisconnect(ctx, fmt.Sprintf("soul-%d", i), "production-eu", false)
	}

	// 4. WebhookNotifier pointed at the mock-server.
	notifier, err := NewWebhookNotifier(WebhookConfig{
		URLRef:  srv.URL,
		Format:  "generic",
		Timeout: 5 * time.Second,
	}, nil, logger)
	if err != nil {
		t.Fatalf("NewWebhookNotifier: %v", err)
	}

	// 5. Leader with per-coven threshold 0.10. Baseline=100 → 15/100=0.15 > 0.10.
	leader, err := NewLeader(LeaderConfig{
		KID:                "kid-int",
		LeaseTTL:           5 * time.Second,
		AcquireRetry:       100 * time.Millisecond,
		TickInterval:       100 * time.Millisecond,
		WindowSize:         60 * time.Second,
		Threshold:          0.50, // global won't trigger (15/100 = 0.15 < 0.50)
		DegradedTTL:        10 * time.Second,
		ClearGrace:         5 * time.Second,
		BaselineCacheTTL:   60 * time.Second,
		PerCovenThresholds: map[string]float64{"production-eu": 0.10},
		Notifier:           notifier,
	}, LeaderDeps{
		Lease:          &leaseAdapter{c: rc},
		SortedSet:      &sortedSetAdapter{c: rc},
		DegradedWriter: &degradedAdapter{c: rc},
		Baseline:       &fixedBaseline{value: 100},
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	leaderCtx, leaderCancel := context.WithCancel(ctx)
	defer leaderCancel()
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		leader.Run(leaderCtx)
	}()

	// Wait for the degraded flag (up to 5s — 50 ticks of 100ms).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok, err := keeperredis.TollIsDegraded(ctx, rc)
		if err == nil && ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	ok, err := keeperredis.TollIsDegraded(ctx, rc)
	if err != nil || !ok {
		t.Fatalf("expected cluster:degraded set, ok=%v err=%v", ok, err)
	}

	// Wait for the webhook call.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(webhookCalls)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	leaderCancel()
	<-leaderDone

	mu.Lock()
	defer mu.Unlock()
	if len(webhookCalls) == 0 {
		t.Fatalf("expected ≥1 webhook POST, got 0")
	}
	first := webhookCalls[0]
	if first["event_type"] != EventTypeDegradedSet {
		t.Fatalf("event_type mismatch: %v", first["event_type"])
	}
	if first["coven_name"] != "production-eu" {
		t.Fatalf("expected coven_name=production-eu (per-coven trigger), got %v", first["coven_name"])
	}
	// threshold should be per-coven (0.10), not global (0.50).
	if first["threshold"].(float64) != 0.10 {
		t.Fatalf("expected threshold=0.10 (per-coven), got %v", first["threshold"])
	}
}
