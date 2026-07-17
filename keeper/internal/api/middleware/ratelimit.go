package middleware

// Tempo rate-limit middleware (ADR-050) — a per-AID limiter on the rate of an
// operator's calls to resolver-heavy write endpoints.
//
// Wired AFTER [RequireJWT]: the AID is taken from claims in the context
// ([ClaimsFromContext], `claims.Subject`). Before the handler (before the
// resolvers run) one token is taken from the per-AID bucket in Redis; on
// exhaustion — 429 + Retry-After + application/problem+json
// ([problem.TypeTempoExceeded]), without calling next.
//
// Reference — toll.DegradedMiddleware (keeper/internal/toll/middleware.go):
// the same factory shape, the same nil → passthrough, the same fail-OPEN on a
// Redis error. Difference: Tempo — per-AID 429 by rate, Toll — cluster-wide
// 503 by health. Wiring (specifically on `POST /v1/voyages`) and config
// (the `tempo:` block, rate/burst, hot-reload) — wire-up in keeper/cmd/keeper +
// keeper/internal/api (S-R3/S-R4).

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// RateLimiter — the Tempo token-bucket surface the middleware needs.
// Implemented by *redis.TokenBucket (keeper/internal/redis/ratelimit.go).
//
// An interface (not a concrete type) — so the middleware lives in `api/middleware`
// without an import dependency on `internal/redis`, and so unit tests can swap the
// limiter with a fake (like DegradedReader in Toll). allowed/retryAfter/err — per the
// TokenBucket.Allow contract.
type RateLimiter interface {
	Allow(ctx context.Context, aid, bucket string, rate float64, burst int) (allowed bool, retryAfter time.Duration, err error)
}

// RateLimitMetrics — a narrow Tempo metrics surface (ADR-050(g)).
// Implemented by keeper/internal/api server metrics; nil-safe (nil hook → emit
// no-op). Label endpoint = bucket name; NO AID label (cardinality).
type RateLimitMetrics interface {
	IncTempoAllowed(endpoint string)
	IncTempoRejected(endpoint string)
}

// RateLimitLimits — the current rate/burst of a bucket. Returned by a provider
// that reads the live config.Store snapshot (ADR-021 hot-reload): a new limit
// applies from the next request without a restart, and the current buckets in Redis
// live out their own PEXPIRE.
type RateLimitLimits struct {
	Rate  float64
	Burst int
}

// RateLimit returns a chi/net-http-compatible Tempo-limit middleware for a
// specific logical bucket (`bucket`, e.g. "voyage_create").
//
// rate/burst are read NOT at assembly but on every request via the `limits` provider
// (hot-reload, ADR-050(f)/ADR-021): the provider returns the live config.Store snapshot.
// Invalid (≤0) provider values are treated as fail-OPEN passthrough —
// limiter.Allow would reject them with an error anyway, and blocking on a config failure
// is not allowed (availability > caution, as on a Redis flap).
//
// limiter=nil → middleware no-op (passthrough). Symmetric to the toll middleware
// at reader=nil: dev/single-instance without Redis blocks nothing, and router
// tests that don't need Tempo get no surprises.
//
// metrics=nil → emit no-op (unit tests without obs.Registry).
//
// Logic:
//  1. limiter=nil → passthrough (decided at the factory stage, no per-request
//     overhead).
//  2. No claims in context (the middleware is attached without the RequireJWT chain) →
//     fail-OPEN passthrough + warn. This is a wire-up error, but blocking on
//     "no AID" is not allowed — the bucket is per-AID, and without an AID the limit is meaningless.
//  3. limiter.Allow returned an error (Redis failure / script crashed) → fail-OPEN
//     passthrough + debug (ADR-050(b): availability > caution, like Toll).
//  4. allowed=false → 429 + Retry-After + problem+json, next is NOT called,
//     keeper_tempo_rejected_total{endpoint} +1.
//  5. allowed=true → passthrough, keeper_tempo_allowed_total{endpoint} +1.
func RateLimit(limiter RateLimiter, bucket string, limits func() RateLimitLimits, metrics RateLimitMetrics, logger *slog.Logger) func(http.Handler) http.Handler {
	if limiter == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok || claims.Subject == "" {
				// Wire-up without RequireJWT before Tempo is a server-configuration
				// programming error. Fail-OPEN: a per-AID limit without an AID
				// is uncomputable, blocking is not allowed.
				logger.Warn("tempo: no AID in context - middleware wired without RequireJWT? fail-open",
					slog.String("path", r.URL.Path),
				)
				next.ServeHTTP(w, r)
				return
			}

			lim := limits()
			if lim.Rate <= 0 || lim.Burst <= 0 {
				// Broken/zero config (hot-reload supplied an invalid value) → fail-OPEN.
				logger.Debug("tempo: non-positive rate/burst from config — fail-open",
					slog.Float64("rate", lim.Rate),
					slog.Int("burst", lim.Burst),
					slog.String("bucket", bucket),
				)
				next.ServeHTTP(w, r)
				return
			}

			allowed, retryAfter, err := limiter.Allow(r.Context(), claims.Subject, bucket, lim.Rate, lim.Burst)
			if err != nil {
				// Fail-OPEN: a Redis flap is common, availability matters more than
				// caution (ADR-050(b)).
				logger.Debug("tempo: limiter check failed — fail-open",
					slog.Any("error", err),
					slog.String("aid", claims.Subject),
					slog.String("bucket", bucket),
					slog.String("path", r.URL.Path),
				)
				next.ServeHTTP(w, r)
				return
			}
			if allowed {
				if metrics != nil {
					metrics.IncTempoAllowed(bucket)
				}
				next.ServeHTTP(w, r)
				return
			}
			if metrics != nil {
				metrics.IncTempoRejected(bucket)
			}
			writeTempo429(w, r, retryAfter)
		})
	}
}

// writeTempo429 writes 429 + Retry-After + RFC 7807 problem+json
// ([problem.TypeTempoExceeded]). Retry-After — the seconds until at least one token
// refills, rounded UP (a retry before that would hit an empty bucket again). Minimum
// 1s: the Retry-After header is in seconds, and 0 would send the client into
// busy-retry.
func writeTempo429(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	secs := int(retryAfter / time.Second)
	if retryAfter%time.Second > 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	problem.Write(w, problem.New(
		problem.TypeTempoExceeded,
		r.URL.Path,
		"rate limit exceeded for this operator; retry after the Retry-After interval",
	))
}
