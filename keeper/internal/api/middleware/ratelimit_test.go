package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

func tempoTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRateLimiter — controllable [RateLimiter] for middleware tests.
type fakeRateLimiter struct {
	allowed    bool
	retryAfter time.Duration
	err        error

	// arguments recorded from the last Allow call — for verifying AID/bucket pass-through.
	gotAID    string
	gotBucket string
	gotRate   float64
	gotBurst  int
	calls     int
}

func (f *fakeRateLimiter) Allow(_ context.Context, aid, bucket string, rate float64, burst int) (bool, time.Duration, error) {
	f.calls++
	f.gotAID, f.gotBucket, f.gotRate, f.gotBurst = aid, bucket, rate, burst
	return f.allowed, f.retryAfter, f.err
}

func withTestClaims(r *http.Request, aid string) *http.Request {
	ctx := WithClaims(r.Context(), &jwt.Claims{Subject: aid})
	return r.WithContext(ctx)
}

// staticLimits — a fixed rate/burst provider for tests (simulates a
// config.Store snapshot without hot-reload).
func staticLimits(rate float64, burst int) func() RateLimitLimits {
	return func() RateLimitLimits { return RateLimitLimits{Rate: rate, Burst: burst} }
}

// fakeTempoMetrics — allowed/rejected counters per endpoint for verifying emit.
type fakeTempoMetrics struct {
	allowed  map[string]int
	rejected map[string]int
}

func newFakeTempoMetrics() *fakeTempoMetrics {
	return &fakeTempoMetrics{allowed: map[string]int{}, rejected: map[string]int{}}
}

func (m *fakeTempoMetrics) IncTempoAllowed(endpoint string)  { m.allowed[endpoint]++ }
func (m *fakeTempoMetrics) IncTempoRejected(endpoint string) { m.rejected[endpoint]++ }

func TestRateLimit_NilLimiter_Passthrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	})
	mw := RateLimit(nil, "voyage_create", staticLimits(10, 20), nil, tempoTestLogger())(next)

	rec := httptest.NewRecorder()
	req := withTestClaims(httptest.NewRequest(http.MethodPost, "/v1/voyages", nil), "archon-alice")
	mw.ServeHTTP(rec, req)

	if !called {
		t.Fatal("nil-limiter → middleware должен быть no-op (next вызван)")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ожидался 202, got %d", rec.Code)
	}
}

func TestRateLimit_Allowed_Passthrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	})
	lim := &fakeRateLimiter{allowed: true}
	metrics := newFakeTempoMetrics()
	mw := RateLimit(lim, "voyage_create", staticLimits(10, 20), metrics, tempoTestLogger())(next)

	rec := httptest.NewRecorder()
	req := withTestClaims(httptest.NewRequest(http.MethodPost, "/v1/voyages", nil), "archon-alice")
	mw.ServeHTTP(rec, req)

	if !called {
		t.Fatal("allowed=true → next должен вызваться")
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("ожидался 201, got %d", rec.Code)
	}
	if lim.gotAID != "archon-alice" {
		t.Errorf("AID прокинут неверно: got %q, want archon-alice", lim.gotAID)
	}
	if lim.gotBucket != "voyage_create" || lim.gotRate != 10 || lim.gotBurst != 20 {
		t.Errorf("bucket/rate/burst прокинуты неверно: %q/%v/%d", lim.gotBucket, lim.gotRate, lim.gotBurst)
	}
	if metrics.allowed["voyage_create"] != 1 || metrics.rejected["voyage_create"] != 0 {
		t.Errorf("allowed=true → allowed{voyage_create}+1, rejected 0; got allowed=%d rejected=%d",
			metrics.allowed["voyage_create"], metrics.rejected["voyage_create"])
	}
}

func TestRateLimit_Exceeded_429_RetryAfter_Problem(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	lim := &fakeRateLimiter{allowed: false, retryAfter: 1500 * time.Millisecond}
	metrics := newFakeTempoMetrics()
	mw := RateLimit(lim, "voyage_create", staticLimits(10, 20), metrics, tempoTestLogger())(next)

	rec := httptest.NewRecorder()
	req := withTestClaims(httptest.NewRequest(http.MethodPost, "/v1/voyages", nil), "archon-alice")
	mw.ServeHTTP(rec, req)

	if called {
		t.Fatal("allowed=false → next вызываться не должен")
	}
	if metrics.rejected["voyage_create"] != 1 || metrics.allowed["voyage_create"] != 0 {
		t.Errorf("allowed=false → rejected{voyage_create}+1, allowed 0; got allowed=%d rejected=%d",
			metrics.allowed["voyage_create"], metrics.rejected["voyage_create"])
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("ожидался 429, got %d", rec.Code)
	}

	// Retry-After rounds UP: 1500ms → 2s.
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("ожидался заголовок Retry-After")
	}
	n, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("Retry-After должен быть целым числом секунд, got %q", ra)
	}
	if n != 2 {
		t.Fatalf("Retry-After: 1500ms должно округлиться вверх до 2с, got %d", n)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("ожидался Content-Type application/problem+json, got %q", ct)
	}

	var body problem.Details
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if body.Type != problem.TypeTempoExceeded {
		t.Fatalf("ожидался type %q, got %q", problem.TypeTempoExceeded, body.Type)
	}
	if body.Status != http.StatusTooManyRequests {
		t.Fatalf("ожидался status 429 в теле, got %d", body.Status)
	}
}

// TestRateLimit_RetryAfter_Floor1s — retryAfter < 1s rounds up to the minimum of 1.
func TestRateLimit_RetryAfter_Floor1s(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	lim := &fakeRateLimiter{allowed: false, retryAfter: 50 * time.Millisecond}
	mw := RateLimit(lim, "voyage_create", staticLimits(10, 20), nil, tempoTestLogger())(next)

	rec := httptest.NewRecorder()
	req := withTestClaims(httptest.NewRequest(http.MethodPost, "/v1/voyages", nil), "archon-alice")
	mw.ServeHTTP(rec, req)

	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After для 50ms должен быть минимумом 1, got %q", got)
	}
}

func TestRateLimit_LimiterError_FailOpen(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	lim := &fakeRateLimiter{err: errors.New("redis down")}
	mw := RateLimit(lim, "voyage_create", staticLimits(10, 20), nil, tempoTestLogger())(next)

	rec := httptest.NewRecorder()
	req := withTestClaims(httptest.NewRequest(http.MethodPost, "/v1/voyages", nil), "archon-alice")
	mw.ServeHTTP(rec, req)

	if !called {
		t.Fatal("limiter-error → fail-open (next вызван), ADR-050(b)")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался 200 fail-open, got %d", rec.Code)
	}
}

func TestRateLimit_NoClaims_FailOpen(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	lim := &fakeRateLimiter{allowed: false, retryAfter: time.Second}
	mw := RateLimit(lim, "voyage_create", staticLimits(10, 20), nil, tempoTestLogger())(next)

	rec := httptest.NewRecorder()
	// Without WithClaims — no claims in the context.
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages", nil)
	mw.ServeHTTP(rec, req)

	if !called {
		t.Fatal("нет AID в context → fail-open passthrough (wire-up без RequireJWT)")
	}
	if lim.calls != 0 {
		t.Fatalf("без AID limiter дёргаться не должен, calls=%d", lim.calls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался 200, got %d", rec.Code)
	}
}

// bucketingLimiter — a fake [RateLimiter] that keeps INDEPENDENT remaining-token
// counters per-(aid, bucket). Simulates the Redis key `tempo:<aid>:<bucket>`: different
// bucket names → different keys → independent quotas (no refill — sufficient
// for the window test). Needed to prove the invariant "voyage_create and voyage_preview
// don't share a quota" at the middleware level (without testcontainers, ADR-050 amendment
// 2026-06-17).
type bucketingLimiter struct {
	burst     map[string]int // (aid|bucket) → starting depth (capacity)
	remaining map[string]int // (aid|bucket) → remaining tokens
}

func newBucketingLimiter() *bucketingLimiter {
	return &bucketingLimiter{burst: map[string]int{}, remaining: map[string]int{}}
}

func (l *bucketingLimiter) Allow(_ context.Context, aid, bucket string, _ float64, burst int) (bool, time.Duration, error) {
	key := aid + "|" + bucket
	if _, seen := l.burst[key]; !seen {
		l.burst[key] = burst
		l.remaining[key] = burst
	}
	if l.remaining[key] <= 0 {
		return false, time.Second, nil
	}
	l.remaining[key]--
	return true, 0, nil
}

// TestRateLimit_PreviewAndCreate_SeparateBuckets — INVARIANT of ADR-050 amendment
// 2026-06-17: voyage_create and voyage_preview are DIFFERENT bucket keys per-AID, they
// don't share a quota. Exhausting the create bucket does NOT 429 preview, and vice versa.
//
// Burst=1 on each bucket: one request passes, the second gets 429. We prove that
// after create is exhausted, preview still has its own full burst (and
// symmetrically). Same limiter (a shared Redis analog), one AID.
func TestRateLimit_PreviewAndCreate_SeparateBuckets(t *testing.T) {
	const aid = "archon-alice"
	const burst = 1
	lim := newBucketingLimiter()
	logger := tempoTestLogger()

	createMW := RateLimit(lim, "voyage_create", staticLimits(10, burst), nil, logger)
	previewMW := RateLimit(lim, "voyage_preview", staticLimits(30, burst), nil, logger)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	do := func(mw func(http.Handler) http.Handler, path string) int {
		rec := httptest.NewRecorder()
		req := withTestClaims(httptest.NewRequest(http.MethodPost, path, nil), aid)
		mw(next).ServeHTTP(rec, req)
		return rec.Code
	}

	// 1. Exhaust the create bucket: the first — 202, the second — 429.
	if code := do(createMW, "/v1/voyages"); code != http.StatusAccepted {
		t.Fatalf("create #1: ожидался 202 в пределах burst, got %d", code)
	}
	if code := do(createMW, "/v1/voyages"); code != http.StatusTooManyRequests {
		t.Fatalf("create #2: ожидался 429 (бакет create исчерпан), got %d", code)
	}

	// 2. preview is NOT affected by exhausting create: the first preview passes (202).
	if code := do(previewMW, "/v1/voyages/preview"); code != http.StatusAccepted {
		t.Fatalf("preview #1: ожидался 202 — preview не делит квоту с create, got %d", code)
	}
	// preview is exhausted by its own burst → the second preview gets 429.
	if code := do(previewMW, "/v1/voyages/preview"); code != http.StatusTooManyRequests {
		t.Fatalf("preview #2: ожидался 429 (собственный preview-бакет исчерпан), got %d", code)
	}

	// 3. Symmetry: exhausting preview does not affect create. create was already exhausted in
	//    step 1; we check the reverse direction on a fresh AID.
	const aid2 = "archon-bob"
	doAs := func(mw func(http.Handler) http.Handler, path, who string) int {
		rec := httptest.NewRecorder()
		req := withTestClaims(httptest.NewRequest(http.MethodPost, path, nil), who)
		mw(next).ServeHTTP(rec, req)
		return rec.Code
	}
	if code := doAs(previewMW, "/v1/voyages/preview", aid2); code != http.StatusAccepted {
		t.Fatalf("bob preview #1: ожидался 202, got %d", code)
	}
	if code := doAs(previewMW, "/v1/voyages/preview", aid2); code != http.StatusTooManyRequests {
		t.Fatalf("bob preview #2: ожидался 429 (preview-бакет исчерпан), got %d", code)
	}
	// bob's create bucket is untouched by exhausting his preview → it passes.
	if code := doAs(createMW, "/v1/voyages", aid2); code != http.StatusAccepted {
		t.Fatalf("bob create #1: ожидался 202 — create не делит квоту с preview, got %d", code)
	}
}

// TestRateLimit_NonPositiveLimits_FailOpen — the provider returned a zero/broken
// rate/burst (hot-reload slipped in an invalid value) → fail-OPEN passthrough, the limiter is
// not invoked (Allow would have rejected them with an error anyway).
func TestRateLimit_NonPositiveLimits_FailOpen(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	lim := &fakeRateLimiter{allowed: false, retryAfter: time.Second}
	mw := RateLimit(lim, "voyage_create", staticLimits(0, 0), nil, tempoTestLogger())(next)

	rec := httptest.NewRecorder()
	req := withTestClaims(httptest.NewRequest(http.MethodPost, "/v1/voyages", nil), "archon-alice")
	mw.ServeHTTP(rec, req)

	if !called {
		t.Fatal("нулевой rate/burst → fail-open passthrough")
	}
	if lim.calls != 0 {
		t.Fatalf("при невалидном лимите limiter дёргаться не должен, calls=%d", lim.calls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался 200, got %d", rec.Code)
	}
}
