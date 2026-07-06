package obs

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTPMetrics — набор Prometheus-collector-ов HTTP-инструментации
// (Operator API под `/v1/*`). Регистрируется отдельно от registry-core
// через [RegisterHTTPMetrics]: registry-core компонент-агностичен, а
// собственные метрики вешаются helper-ами поверх него. HTTPMetrics
// остаётся в shared/obs — middleware нейтральна к internal-типам обоих
// бинарей (параметризуется injected path-extractor-ом), поэтому это
// сквозной фундамент, а НЕ subsystem-local collector. Контраст:
// keeper-only collector-ы (Reaper и пр.) тянут internal-типы и потому
// живут рядом с подсистемой в keeper/internal/* (docs/observability.md §4.0).
//
// Имена метрик — Prometheus convention (snake_case, _total для counters,
// _seconds для durations; ADR-024 §2.1). Labels подобраны под наблюдаемые
// вопросы оператора:
//   - method/path/status — стандартный split request-rate;
//   - path берётся из chi-RouteContext (route pattern, не raw URL) — без
//     него /v1/operators/{aid}/revoke даст cardinality-blow-up по AID
//     (ADR-024 §2.2).
type HTTPMetrics struct {
	requestsTotal *prometheus.CounterVec
	requestDur    *prometheus.HistogramVec
	inFlight      prometheus.Gauge
}

// RegisterHTTPMetrics создаёт keeper_http_*-collectors и регистрирует их в
// Registry-е. Возвращает дескриптор для wire-up в HTTP-роутер Keeper-а.
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды
// на одном Registry); падать сразу удобнее, чем носить ленивую init.
func RegisterHTTPMetrics(r *Registry) *HTTPMetrics {
	m := &HTTPMetrics{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_http_requests_total",
				Help: "Количество HTTP-запросов под /v1/*, разрезанное по method/path/status.",
			},
			[]string{"method", "path", "status"},
		),
		requestDur: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "keeper_http_request_duration_seconds",
				Help: "Latency HTTP-запросов под /v1/*, в секундах.",
				// Buckets под Operator API: типичные запросы — 1-50ms,
				// PG-write-path — 10-200ms, slow — 1s+. Default-buckets
				// Prometheus (0.005..10) рассчитаны на web-traffic с длинным
				// хвостом; Keeper-API короче — сужаем верхнюю границу до 5s,
				// добавляем гранулярность в зоне 5-100ms.
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
			},
			[]string{"method", "path"},
		),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_http_in_flight_requests",
			Help: "Число HTTP-запросов под /v1/* в обработке прямо сейчас.",
		}),
	}
	r.reg.MustRegister(m.requestsTotal, m.requestDur, m.inFlight)
	return m
}

// MiddlewareForPath возвращает middleware, инструментирующий HTTP-handler:
// считает requests_total / duration / in_flight, лейбля по результату
// pathExtractor(r). pathExtractor вытягивает chi-pattern; каллер обычно
// использует [chi.RouteContext](r).RoutePattern().
//
// `path` — pattern маршрута (`/v1/operators/{aid}/revoke`), не raw URL.
// Для не-chi handler-ов / fallback-ов / nil-extractor-а path-параметр
// будет пустым — это допустимо (метрика собирается с label `path=""`,
// нелитеральные пути не размывают cardinality).
//
// Подход «через injected extractor», а не через прямой import chi, чтобы
// shared/obs не тянул chi в зависимости (по ADR-011 shared/ — поперечный
// код без привязки к конкретному роутеру; роутер выбирает keeper-сторона).
//
// Middleware применяется ВНУТРИ chi.Route("/v1") после того, как
// chi-router вычислил RoutePattern; снаружи (на root r.Use(...)) chi ещё
// не знает pattern-а, label получится пустым.
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

// statusRecorder — wrap для http.ResponseWriter, запоминающий фактический
// status (WriteHeader). Лёгкий, без буферизации body — нам нужен только code.
//
// Дублирует приватный recorder middleware/audit.go; не выношу в общий
// helper, так как audit-middleware живёт в keeper/, а obs/ — в shared/,
// и тянуть зависимость shared→keeper нельзя. Три строки лучше
// преждевременной абстракции (CLAUDE.md «без over-engineering»).
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

// Write обеспечивает корректный учёт статуса, если handler пишет body
// без явного WriteHeader (stdlib подразумевает 200).
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true // status уже 200 из конструктора
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
