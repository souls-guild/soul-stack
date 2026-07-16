package toll

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
)

// retryAfterSeconds — Retry-After header for 503 response from degraded-middleware.
// Matches DegradedTTL by default (60s): client can retry after
// estimated max-window. ADR-038 does not fix exact value — we choose symmetrically
// to the window.
const retryAfterSeconds = 60

// DegradedMiddleware returns chi/net-http-compatible middleware. On each
// blocked request (determined via [BlockedRoute]) middleware reads
// cluster:degraded via [DegradedReader] and on set flag responds
// 503 + Retry-After + application/problem+json, otherwise passes through.
//
// reader=nil → middleware no-op (just passthrough). Convenient for wire-up
// in daemon: without Redis (single-instance/dev) middleware blocks nothing
// and router tests that don't need Toll don't get surprises.
//
// Blocking logic:
//  1. If route NOT blocked (read-API, RBAC, destroy, Errand) → passthrough.
//     Cheap check FIRST to avoid hitting Redis on every GET.
//  2. Read cluster:degraded via DegradedReader. Error → fail-OPEN (pass
//     request): availability more important than safety, flag expires via DegradedTTL
//     if leader dies, blocking on Redis flap is worse than false-negative.
//  3. degraded=true → 503 + Retry-After 60 + problem+json.
//
// Middleware decides blocked-route by path+method itself. More precise (route-pattern
// matcher) CANNOT be done at middleware level — chi RouteContext accessible
// only under `r.Route(...)`. So we use explicit wrapping of exact
// blocked routes in router (see wire-up in api/router.go).
func DegradedMiddleware(reader DegradedReader, logger *slog.Logger) func(http.Handler) http.Handler {
	if reader == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			degraded, err := reader.IsDegraded(r.Context())
			if err != nil {
				// Fail-OPEN: debug log (Redis flap — common phenomenon), pass through.
				logger.Debug("toll: degraded check failed — fail-open",
					slog.Any("error", err),
					slog.String("path", r.URL.Path),
				)
				next.ServeHTTP(w, r)
				return
			}
			if !degraded {
				next.ServeHTTP(w, r)
				return
			}
			writeDegraded503(w, r)
		})
	}
}

// writeDegraded503 writes RFC 7807 problem+json with Retry-After. Format strictly
// aligned with keeper/internal/api/problem/TypeClusterDegraded (status 503 and title
// fixed there too). Local assembly without dependency on problem package —
// middleware lives in `keeper/internal/toll/` and should not pull
// `keeper/internal/api/problem/` (no cycles, but dependency direction
// `api → toll`, not reversed).
func writeDegraded503(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusServiceUnavailable)
	body := map[string]any{
		"type":     "https://soul-stack.io/errors/cluster-degraded",
		"title":    "Cluster is in degraded mode",
		"status":   http.StatusServiceUnavailable,
		"detail":   "Too many Souls disconnected recently; write-API blocked. Retry after 60s.",
		"instance": r.URL.Path,
	}
	_ = json.NewEncoder(w).Encode(body)
}
