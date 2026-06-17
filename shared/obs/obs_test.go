package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestNewRegistry_HasGoCollector(t *testing.T) {
	r := NewRegistry()
	body := obstest.Scrape(t, r.Gatherer())

	// go_goroutines — кросс-платформенный go-collector sample.
	// registry-core компонент-агностичен: keeper_http_*-метрик здесь нет,
	// пока не вызван RegisterHTTPMetrics (см. тест ниже).
	//
	// process_* collectors не проверяем: на macOS process_open_fds
	// отсутствует (linux-only метрика).
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("metrics output missing %q", "go_goroutines")
	}
}

func TestRegisterHTTPMetrics_AddsInFlightGauge(t *testing.T) {
	reg := NewRegistry()
	_ = RegisterHTTPMetrics(reg)
	body := obstest.Scrape(t, reg.Gatherer())

	// in_flight_requests — единственная HTTP-метрика с гарантированным
	// sample без вызова handler-а (Gauge с 0). Counter/Histogram видны
	// только после первой WithLabelValues — покрывается middleware-тестом.
	if !strings.Contains(body, "keeper_http_in_flight_requests") {
		t.Errorf("metrics output missing %q after RegisterHTTPMetrics", "keeper_http_in_flight_requests")
	}
}

func TestMiddlewareForPath_RecordsRequest(t *testing.T) {
	reg := NewRegistry()
	httpM := RegisterHTTPMetrics(reg)

	// Симулируем chi-эффект: pathExtractor возвращает фиксированный
	// pattern, как если бы chi.RouteContext.RoutePattern() уже
	// записал /v1/operators.
	mw := httpM.MiddlewareForPath(func(r *http.Request) string {
		return "/v1/operators"
	})

	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", nil)
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("downstream handler не был вызван")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_http_requests_total{method="POST",path="/v1/operators",status="201"} 1`) {
		t.Errorf("requests_total sample не найден; got=\n%s", body)
	}
	// duration_seconds_count{...} 1 — гарантия, что Observe() вызвался.
	if !strings.Contains(body, `keeper_http_request_duration_seconds_count{method="POST",path="/v1/operators"} 1`) {
		t.Errorf("duration_seconds_count sample не найден; got=\n%s", body)
	}
}

func TestMiddlewareForPath_DefaultStatusOK(t *testing.T) {
	reg := NewRegistry()
	mw := RegisterHTTPMetrics(reg).MiddlewareForPath(func(r *http.Request) string { return "/v1/x" })
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Намеренно НЕ вызываем WriteHeader — stdlib подразумевает 200.
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	handler.ServeHTTP(rec, req)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `status="200"`) {
		t.Errorf("default status должен быть 200; got=\n%s", body)
	}
}

func TestMiddlewareForPath_NilExtractor(t *testing.T) {
	reg := NewRegistry()
	mw := RegisterHTTPMetrics(reg).MiddlewareForPath(nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	handler.ServeHTTP(rec, req)
	// path=""; собрать не должно паниковать
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `path=""`) {
		t.Errorf("expected empty path label; got=\n%s", body)
	}
}

func TestMetricsHandler_ServesPrometheusFormat(t *testing.T) {
	reg := NewRegistry()
	srv := httptest.NewServer(reg.MetricsHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", ct)
	}
}
