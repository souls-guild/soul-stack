package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubPinger struct {
	err error
}

func (s stubPinger) Ping(ctx context.Context) error { return s.err }

// slowPinger never returns ok on its own; it waits for ctx.Done. Used
// to exercise the per-check timeout in Readyz.
type slowPinger struct{}

func (slowPinger) Ping(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestHealthz_AlwaysOK(t *testing.T) {
	h := NewHandler(Deps{PG: stubPinger{err: errors.New("ignored: liveness != readiness")}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.Healthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func TestReadyz_AllUp(t *testing.T) {
	h := NewHandler(Deps{PG: stubPinger{}, Redis: stubPinger{}, Vault: stubPinger{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
	var resp readyResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if resp.Checks["postgres"] != "ok" {
		t.Errorf("postgres = %q, want ok", resp.Checks["postgres"])
	}
	if resp.Checks["redis"] != "ok" {
		t.Errorf("redis = %q, want ok", resp.Checks["redis"])
	}
	if resp.Checks["vault"] != "ok" {
		t.Errorf("vault = %q, want ok", resp.Checks["vault"])
	}
}

// TestReadyz_RedisDown — Redis is a required dependency: its unavailability
// fails readiness (503) so the LB drains traffic from an instance without cluster state.
func TestReadyz_RedisDown(t *testing.T) {
	h := NewHandler(Deps{PG: stubPinger{}, Redis: stubPinger{err: errors.New("connection refused")}, Vault: stubPinger{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Code = %d, want 503", rec.Code)
	}
	var resp readyResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "not_ready" {
		t.Errorf("status = %q, want not_ready", resp.Status)
	}
	if got := resp.Checks["redis"]; got == "" || got == "ok" {
		t.Errorf("redis = %q, want unreachable", got)
	}
	if resp.Checks["postgres"] != "ok" {
		t.Errorf("postgres = %q, want ok", resp.Checks["postgres"])
	}
}

// TestReadyz_RedisSkippedWhenNil — Redis Pinger is nil (dev fallback without Redis):
// the check is skipped, not mentioned in the response, and does not fail readiness.
func TestReadyz_RedisSkippedWhenNil(t *testing.T) {
	h := NewHandler(Deps{PG: stubPinger{}, Vault: stubPinger{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
	var resp readyResp
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if _, ok := resp.Checks["redis"]; ok {
		t.Errorf("redis check present %q, want absent (nil pinger skipped)", resp.Checks["redis"])
	}
}

func TestReadyz_PGDown(t *testing.T) {
	h := NewHandler(Deps{PG: stubPinger{err: errors.New("connection refused")}, Redis: stubPinger{}, Vault: stubPinger{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Code = %d, want 503", rec.Code)
	}
	var resp readyResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "not_ready" {
		t.Errorf("status = %q, want not_ready", resp.Status)
	}
	if got := resp.Checks["postgres"]; got == "" || got == "ok" {
		t.Errorf("postgres = %q, want unreachable", got)
	}
	if resp.Checks["vault"] != "ok" {
		t.Errorf("vault = %q, want ok", resp.Checks["vault"])
	}
}

func TestReadyz_VaultDown(t *testing.T) {
	h := NewHandler(Deps{PG: stubPinger{}, Redis: stubPinger{}, Vault: stubPinger{err: errors.New("sealed")}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Code = %d, want 503", rec.Code)
	}
	var resp readyResp
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Status != "not_ready" {
		t.Errorf("status = %q, want not_ready", resp.Status)
	}
}

func TestReadyz_NilPingerSkipped(t *testing.T) {
	h := NewHandler(Deps{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rec, req)

	// No dependencies → checks empty → ok=true → 200.
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
	var resp readyResp
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Checks) != 0 {
		t.Errorf("checks = %v, want empty", resp.Checks)
	}
}

// TestReadyz_BothDown — both dependent services return an error. `checks{}`
// must carry both statuses (no early-return on the first error),
// overall — 503.
func TestReadyz_BothDown(t *testing.T) {
	h := NewHandler(Deps{
		PG:    stubPinger{err: errors.New("pg refused")},
		Redis: stubPinger{},
		Vault: stubPinger{err: errors.New("vault sealed")},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Code = %d, want 503", rec.Code)
	}
	var resp readyResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "not_ready" {
		t.Errorf("status = %q, want not_ready", resp.Status)
	}
	if pg := resp.Checks["postgres"]; pg == "" || pg == "ok" {
		t.Errorf("postgres = %q, want unreachable", pg)
	}
	if v := resp.Checks["vault"]; v == "" || v == "ok" {
		t.Errorf("vault = %q, want unreachable", v)
	}
}

// TestReadyz_PerCheckTimeout — a slow pinger must not exceed the
// per-check timeout. We verify that overall Readyz latency stays within
// (timeout + small margin) and the check is marked timeout, not
// "unreachable".
func TestReadyz_PerCheckTimeout(t *testing.T) {
	h := NewHandler(Deps{PG: slowPinger{}, Vault: stubPinger{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	start := time.Now()
	h.Readyz(rec, req)
	elapsed := time.Since(start)

	if elapsed > perCheckTimeout+500*time.Millisecond {
		t.Errorf("Readyz elapsed %s, want <= %s+500ms", elapsed, perCheckTimeout)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Code = %d, want 503", rec.Code)
	}
	var resp readyResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	pg := resp.Checks["postgres"]
	if !strings.Contains(pg, "timeout") {
		t.Errorf("postgres = %q, want contains 'timeout'", pg)
	}
	if resp.Checks["vault"] != "ok" {
		t.Errorf("vault = %q, want ok (independent of pg slow path)", resp.Checks["vault"])
	}
}

// TestReadyz_Parallel — two slow checks run in parallel
// (overall = max, not sum). If they don't run in parallel, the test fails on
// elapsed.
func TestReadyz_Parallel(t *testing.T) {
	h := NewHandler(Deps{PG: slowPinger{}, Vault: slowPinger{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	start := time.Now()
	h.Readyz(rec, req)
	elapsed := time.Since(start)

	// Sequential: ~2*timeout = 4s. Parallel: ~timeout = 2s. Take a
	// margin bound, catch an obvious sequential regression.
	if elapsed > perCheckTimeout+1*time.Second {
		t.Errorf("Readyz elapsed %s — looks sequential, want parallel (~%s)", elapsed, perCheckTimeout)
	}
}
