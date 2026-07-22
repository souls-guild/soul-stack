package obs

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTPMetrics is the set of Prometheus collectors for HTTP instrumentation
// (Operator API under `/v1/*`). Registered separately from the registry core
// via [RegisterHTTPMetrics]: the registry core is component-agnostic, and
// dedicated metrics are attached by helpers on top of it. HTTPMetrics stays in
// shared/obs — the middleware is neutral to both binaries' internal types (it
// is parameterized by an injected path-extractor), so it is a cross-cutting
// foundation, NOT a subsystem-local collector. Contrast: keeper-only collectors
// (Reaper etc.) pull internal types and therefore live next to their subsystem
// in keeper/internal/* (docs/observability.md §4.0).
//
// Metric names follow Prometheus convention (snake_case, _total for counters,
// _seconds for durations; ADR-024 §2.1). Labels are chosen for the operator's
// observable questions:
//   - method/path/status — standard request-rate split;
//   - path comes from chi-RouteContext (route pattern, not raw URL) — without
//     it /v1/operators/{aid}/revoke would blow up cardinality by AID
//     (ADR-024 §2.2).
type HTTPMetrics struct {
	requestsTotal *prometheus.CounterVec
	requestDur    *prometheus.HistogramVec
	inFlight      prometheus.Gauge
}

// RegisterHTTPMetrics creates the keeper_http_* collectors and registers them
// in the Registry. Returns a descriptor for wire-up into the Keeper HTTP router.
//
// MustRegister: duplicate registration is a programmer error (called twice on
// one Registry); failing immediately is simpler than carrying lazy init.
func RegisterHTTPMetrics(r *Registry) *HTTPMetrics {
	m := &HTTPMetrics{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_http_requests_total",
				Help: "Number of HTTP requests under /v1/*, sliced by method/path/status.",
			},
			[]string{"method", "path", "status"},
		),
		requestDur: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "keeper_http_request_duration_seconds",
				Help: "Latency of HTTP requests under /v1/*, in seconds.",
				// Buckets tuned for the Operator API: typical requests 1-50ms,
				// PG-write-path 10-200ms, slow 1s+. Prometheus default buckets
				// (0.005..10) target web traffic with a long tail; the Keeper
				// API is shorter — narrow the upper bound to 5s and add
				// granularity in the 5-100ms zone.
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
			},
			[]string{"method", "path"},
		),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_http_in_flight_requests",
			Help: "Number of HTTP requests under /v1/* currently in flight.",
		}),
	}
	r.reg.MustRegister(m.requestsTotal, m.requestDur, m.inFlight)
	return m
}

// MiddlewareForPath returns middleware that instruments an HTTP handler: it
// counts requests_total / duration / in_flight, labeling by the result of
// pathExtractor(r). pathExtractor pulls the chi pattern; the caller usually
// uses [chi.RouteContext](r).RoutePattern().
//
// `path` is the route pattern (`/v1/operators/{aid}/revoke`), not the raw URL.
// For non-chi handlers / fallbacks / a nil extractor the path label will be
// empty — that is acceptable (the metric is collected with label `path=""`;
// non-literal paths do not smear cardinality).
//
// The "injected extractor" approach, rather than a direct chi import, keeps
// shared/obs from depending on chi (per ADR-011 shared/ is cross-cutting code
// with no tie to a specific router; the keeper side picks the router).
//
// The middleware applies INSIDE chi.Route("/v1") after the chi router has
// computed RoutePattern; outside (at root r.Use(...)) chi does not yet know the
// pattern and the label would be empty.
func (m *HTTPMetrics) MiddlewareForPath(pathExtractor func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			m.inFlight.Inc()
			defer m.inFlight.Dec()

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, req)
			elapsed := time.Since(start).Seconds()

			path := ""
			if pathExtractor != nil {
				path = pathExtractor(req)
			}

			m.requestsTotal.WithLabelValues(req.Method, path, strconv.Itoa(rec.status)).Inc()
			m.requestDur.WithLabelValues(req.Method, path).Observe(elapsed)
		})
	}
}

// statusRecorder wraps http.ResponseWriter to remember the actual status
// (WriteHeader). Lightweight, no body buffering — we only need the code.
//
// Duplicates the private recorder in middleware/audit.go; not extracted into a
// shared helper because the audit middleware lives in keeper/ while obs/ is in
// shared/, and a shared→keeper dependency is not allowed. Three lines beat a
// premature abstraction (CLAUDE.md "no over-engineering").
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

// Write records the status correctly when a handler writes the body without an
// explicit WriteHeader (stdlib implies 200).
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true // status already 200 from the constructor
	}
	return s.ResponseWriter.Write(b)
}

// NIM-37: SSE flush passthrough
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
