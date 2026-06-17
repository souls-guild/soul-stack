// Package obstest — test-helper для скрейпа Prometheus-метрик в unit-тестах.
//
// Зависит только от [prometheus] + testing, НЕ от shared/obs ([ADR-011]):
// сигнатура принимает [prometheus.Gatherer] (caller передаёт reg.Gatherer()),
// поэтому helper используется и из white-box тестов `package obs`, и из
// external-пакетов (keeper/internal/...) без import-cycle.
//
// Технический suffix-пакет в духе stdlib `httptest`/`iotest`: не сущность
// словаря Soul Stack, а тест-инструментарий рядом с инструментируемым кодом.
//
// [ADR-011]: ../../../docs/architecture.md
package obstest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Scrape рендерит текущий Prometheus-snapshot переданного Gatherer-а в
// exposition-формате (text/plain; version=0.0.4) и возвращает тело как
// строку. На любую ошибку рендера/скрейпа — t.Fatal.
//
// Caller передаёт g = reg.Gatherer() (obs.Registry.Gatherer); за счёт работы
// через интерфейс [prometheus.Gatherer] пакет не тянет shared/obs.
func Scrape(t testing.TB, g prometheus.Gatherer) string {
	t.Helper()
	h := promhttp.HandlerFor(g, promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics scrape status = %d", rec.Code)
	}
	return rec.Body.String()
}

// Contains — удобная обёртка: true, если скрейп Gatherer-а содержит substr.
func Contains(t testing.TB, g prometheus.Gatherer, substr string) bool {
	t.Helper()
	return strings.Contains(Scrape(t, g), substr)
}
