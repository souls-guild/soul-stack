//go:build integration

// Tempo integration guard tests (ADR-050, S-R4) AT THE middleware LEVEL with
// a real Redis: an HTTP request goes through apimiddleware.RateLimit on top
// of a live *TokenBucket. This is an e2e slice (HTTP → middleware → Redis
// Lua bucket) verifying behavioral invariants that the primitive tests
// (ratelimit_integration_test.go) and the middleware unit tests
// (api/middleware/ratelimit_test.go with a fake limiter) don't cover on
// their own:
//
//   - per-AID coherence through a SHARED Redis (burst exhausted → 429;
//     different AIDs don't share a bucket) — the limit is authoritative in
//     Redis, not multiplied (ADR-050(a));
//   - fail-OPEN when Redis is unavailable (limiter.Allow returns an error →
//     passthrough, NOT 5xx, ADR-050(b));
//   - 429 carries Retry-After (seconds, rounded up) + application/problem+json
//     with type=tempo-exceeded (ADR-050(d)).
//
// The container and integrationAddr are set up by the shared TestMain
// (integration_test.go).
//
// Run:
//
//	cd keeper && TESTCONTAINERS_RYUK_DISABLED=true \
//	    SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/redis/...

package redis

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// tempoMWHandler assembles the chi/net-http chain "RateLimit(middleware) →
// next" on top of a limiter with fixed rate/burst. next responds 202 (like
// the real async-create Voyage). bucket is unique per test (isolation in
// the shared Redis).
func tempoMWHandler(limiter apimiddleware.RateLimiter, bucket string, rate float64, burst int) (http.Handler, *int) {
	nextCalls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusAccepted)
	})
	limits := func() apimiddleware.RateLimitLimits {
		return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
	}
	mw := apimiddleware.RateLimit(limiter, bucket, limits, nil, discardLogger())
	return mw(next), &nextCalls
}

// postAs builds a POST request with claims (AID) in context, as after RequireJWT.
func postAs(aid string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages", nil)
	ctx := apimiddleware.WithClaims(req.Context(), &jwt.Claims{Subject: aid})
	return req.WithContext(ctx)
}

// TestIntegration_TempoMW_PerAIDCoherence — via a SHARED Redis bucket:
// exhausting burst for one AID → 429; ANOTHER AID is unaffected (independent
// bucket).
func TestIntegration_TempoMW_PerAIDCoherence(t *testing.T) {
	tb := newTokenBucketInt(t)
	const rate = 1.0 // slow refill — unnoticeable within the test window
	const burst = 3
	bucket := "voyage_create_" + t.Name()

	h, _ := tempoMWHandler(tb, bucket, rate, burst)

	aidA := uniqueAID(t) + "-a"
	aidB := uniqueAID(t) + "-b"

	// burst requests for AID-A go through (202).
	for i := 0; i < burst; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, postAs(aidA))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("A #%d: expected 202 within burst, got %d", i, rec.Code)
		}
	}

	// burst+1 for AID-A — bucket empty → 429.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAs(aidA))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("A over burst: expected 429, got %d", rec.Code)
	}

	// AID-B — independent bucket in the shared Redis: the first request goes through.
	recB := httptest.NewRecorder()
	h.ServeHTTP(recB, postAs(aidB))
	if recB.Code != http.StatusAccepted {
		t.Fatalf("B first: expected 202 (a different AID does not share the bucket), got %d", recB.Code)
	}
}

// TestIntegration_TempoMW_FailOpenOnRedisDown — Redis is unavailable (client
// closed after constructing the limiter) → limiter.Allow returns an error →
// middleware fail-OPEN passthrough (202, next called), NOT 5xx (ADR-050(b)).
func TestIntegration_TempoMW_FailOpenOnRedisDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tb, err := NewTokenBucket(c)
	if err != nil {
		t.Fatalf("NewTokenBucket: %v", err)
	}
	// Break the connection AFTER construction: the next Allow will hit a
	// closed client → error → fail-open. A deterministic simulation of Redis-down.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	h, nextCalls := tempoMWHandler(tb, "voyage_create_"+t.Name(), 1.0, 1)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAs(uniqueAID(t)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Redis-down → fail-open: expected 202 passthrough, got %d (5xx is not allowed, ADR-050(b))", rec.Code)
	}
	if *nextCalls != 1 {
		t.Fatalf("fail-open → next must be called exactly once, got %d", *nextCalls)
	}
}

// TestIntegration_TempoMW_429Shape — a rejected request carries Retry-After
// (whole seconds, minimum 1) + application/problem+json with
// type=tempo-exceeded and status 429 in the body (ADR-050(d)).
func TestIntegration_TempoMW_429Shape(t *testing.T) {
	tb := newTokenBucketInt(t)
	const rate = 1.0 // 1 rps → retryAfter until the next token is ~1s
	const burst = 1
	h, _ := tempoMWHandler(tb, "voyage_create_"+t.Name(), rate, burst)

	aid := uniqueAID(t)

	// Burn the single token.
	rec0 := httptest.NewRecorder()
	h.ServeHTTP(rec0, postAs(aid))
	if rec0.Code != http.StatusAccepted {
		t.Fatalf("drain: expected 202, got %d", rec0.Code)
	}

	// Second request — 429 with the full shape.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAs(aid))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}

	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("missing Retry-After header")
	}
	secs, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("Retry-After must be a whole number of seconds, got %q", ra)
	}
	if secs < 1 {
		t.Fatalf("Retry-After must be >= 1, got %d", secs)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}

	var body problem.Details
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode problem+json: %v", err)
	}
	if body.Type != problem.TypeTempoExceeded {
		t.Fatalf("problem.type = %q, want %q", body.Type, problem.TypeTempoExceeded)
	}
	if body.Status != http.StatusTooManyRequests {
		t.Fatalf("problem.status = %d, want 429", body.Status)
	}
}
