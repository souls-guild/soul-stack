package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// renewMux is a fake Vault that adds handling of auth/token/lookup-self and
// auth/token/renew-self on top of the health endpoint, for TokenRenewer's needs.
//
// renewable — what to return in lookup-self.Data.renewable.
// leaseSeconds — the token's TTL (ttl in lookup-self, lease_duration in renew).
// renewStatus — the HTTP status of renew-self (200 = success, otherwise renew fails).
type renewMux struct {
	renewable    bool
	leaseSeconds int
	renewStatus  int

	renewCalls atomic.Int32
}

func (m *renewMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/sys/health":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"initialized":true,"sealed":false,"standby":false,"version":"test"}`))
		return

	case r.URL.Path == "/v1/auth/token/lookup-self":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"renewable": m.renewable,
				"ttl":       m.leaseSeconds,
				"id":        "test-token",
			},
		})
		return

	case r.URL.Path == "/v1/auth/token/renew-self":
		m.renewCalls.Add(1)
		status := m.renewStatus
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errors":["renew failed"]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "test-token",
				"renewable":      true,
				"lease_duration": m.leaseSeconds,
			},
		})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func startRenewVault(t *testing.T, m *renewMux) string {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return srv.URL
}

// captureLogger is an slog.Logger writing to a thread-safe buffer. Lets us
// check that the expected message made it into the log and that the token
// isn't in it.
func captureLogger() (*slog.Logger, *syncBuf) {
	buf := &syncBuf{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newRenewClient(t *testing.T, addr string) *Client {
	t.Helper()
	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "test-token", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cl
}

func TestStartTokenRenewer_RenewableStarts(t *testing.T) {
	m := &renewMux{renewable: true, leaseSeconds: 30, renewStatus: http.StatusOK}
	addr := startRenewVault(t, m)
	cl := newRenewClient(t, addr)
	logger, logs := captureLogger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := cl.StartTokenRenewer(ctx, logger)
	if err != nil {
		t.Fatalf("StartTokenRenewer: %v", err)
	}
	if r == nil {
		t.Fatalf("StartTokenRenewer: renewer is nil for renewable token")
	}

	// The watcher renews the token right on the first loop iteration.
	deadline := time.After(3 * time.Second)
	for m.renewCalls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("renew-self was not called within 3s (renewable watcher did not run)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	r.Stop()

	out := logs.String()
	if !strings.Contains(out, "token auto-renew enabled") {
		t.Errorf("expected 'token auto-renew enabled' log, got:\n%s", out)
	}
	if strings.Contains(out, "test-token") {
		t.Errorf("token value leaked into logs:\n%s", out)
	}
}

func TestStartTokenRenewer_NonRenewableDegrades(t *testing.T) {
	m := &renewMux{renewable: false, leaseSeconds: 0}
	addr := startRenewVault(t, m)
	cl := newRenewClient(t, addr)
	logger, logs := captureLogger()

	r, err := cl.StartTokenRenewer(context.Background(), logger)
	if err != nil {
		t.Fatalf("StartTokenRenewer non-renewable: unexpected error %v", err)
	}
	if r != nil {
		t.Fatalf("StartTokenRenewer non-renewable: expected nil renewer, got %v", r)
	}

	// Give it time to confirm renew isn't triggered (the watcher didn't start).
	time.Sleep(100 * time.Millisecond)
	if n := m.renewCalls.Load(); n != 0 {
		t.Errorf("non-renewable token: renew-self called %d times, want 0", n)
	}

	out := logs.String()
	if !strings.Contains(out, "token not renewable, auto-renew disabled") {
		t.Errorf("expected degradation warn, got:\n%s", out)
	}

	// Stop on a nil renewer must not panic.
	r.Stop()
}

func TestStartTokenRenewer_RenewFailLogged(t *testing.T) {
	// A renewable token with a very short lease and a failing renew-self.
	// RenewBehaviorIgnoreErrors keeps backing off until the lease is
	// exhausted, then DoneCh returns an error → the code logs Error.
	m := &renewMux{renewable: true, leaseSeconds: 1, renewStatus: http.StatusInternalServerError}
	addr := startRenewVault(t, m)
	cl := newRenewClient(t, addr)
	logger, logs := captureLogger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := cl.StartTokenRenewer(ctx, logger)
	if err != nil {
		t.Fatalf("StartTokenRenewer: %v", err)
	}
	if r == nil {
		t.Fatalf("StartTokenRenewer: nil renewer")
	}

	// Wait for the watcher to exhaust its retries and exit via DoneCh.
	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatalf("watcher did not exit via DoneCh within 10s")
	}

	if m.renewCalls.Load() == 0 {
		t.Errorf("renew-self was never attempted")
	}
	out := logs.String()
	if !strings.Contains(out, "token will expire") {
		t.Errorf("expected renew-fail/expire log, got:\n%s", out)
	}
	if strings.Contains(out, "test-token") {
		t.Errorf("token value leaked into logs:\n%s", out)
	}
}

func TestStartTokenRenewer_GracefulStopOnCtx(t *testing.T) {
	m := &renewMux{renewable: true, leaseSeconds: 3600, renewStatus: http.StatusOK}
	addr := startRenewVault(t, m)
	cl := newRenewClient(t, addr)
	logger, logs := captureLogger()

	ctx, cancel := context.WithCancel(context.Background())
	r, err := cl.StartTokenRenewer(ctx, logger)
	if err != nil {
		t.Fatalf("StartTokenRenewer: %v", err)
	}
	if r == nil {
		t.Fatalf("StartTokenRenewer: nil renewer")
	}

	// A long lease (3600s) → the watcher sleeps, doesn't exit on its own.
	// Canceling ctx should stop the goroutine; Stop should return quickly.
	cancel()

	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Stop did not return within 5s after ctx cancel (graceful stop broken)")
	}

	out := logs.String()
	if !strings.Contains(out, "token auto-renew stopping (shutdown)") {
		t.Errorf("expected graceful-stop log, got:\n%s", out)
	}
}
