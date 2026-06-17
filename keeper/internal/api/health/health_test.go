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

// slowPinger — никогда не возвращает ok сам, ждёт ctx.Done. Используется
// для проверки per-check timeout-а в Readyz.
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

// TestReadyz_RedisDown — Redis обязательная зависимость: её недоступность
// валит readiness (503), чтобы LB увёл трафик с инстанса без cluster-state.
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

// TestReadyz_RedisSkippedWhenNil — Redis-Pinger nil (dev-fallback без Redis):
// check пропускается, в response не упоминается, readiness не валится из-за него.
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

	// Без зависимостей checks пустой → ok=true → 200.
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", rec.Code)
	}
	var resp readyResp
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Checks) != 0 {
		t.Errorf("checks = %v, want empty", resp.Checks)
	}
}

// TestReadyz_BothDown — оба зависимых сервиса возвращают ошибку. В
// `checks{}` должны быть оба статуса (не early-return на первой ошибке),
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

// TestReadyz_PerCheckTimeout — медленный pinger не должен превышать
// per-check timeout. Проверяем, что overall-latency Readyz укладывается
// в (timeout + небольшой запас) и check помечен как timeout, не
// «unreachable».
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

// TestReadyz_Parallel — две slow-проверки выполняются параллельно
// (overall = max, а не sum). Если параллели нет, тест провалится по
// elapsed.
func TestReadyz_Parallel(t *testing.T) {
	h := NewHandler(Deps{PG: slowPinger{}, Vault: slowPinger{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	start := time.Now()
	h.Readyz(rec, req)
	elapsed := time.Since(start)

	// Sequential: ~2*timeout = 4s. Parallel: ~timeout = 2s. Берём
	// границу с запасом, ловим явный sequential-regress.
	if elapsed > perCheckTimeout+1*time.Second {
		t.Errorf("Readyz elapsed %s — looks sequential, want parallel (~%s)", elapsed, perCheckTimeout)
	}
}
