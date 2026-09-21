// Package obstest is a test helper for scraping Prometheus metrics in unit tests.
//
// It depends only on [prometheus] + testing, NOT on shared/obs ([ADR-011]):
// the signature takes a [prometheus.Gatherer] (the caller passes reg.Gatherer()),
// so the helper is usable both from white-box tests in `package obs` and from
// external packages (keeper/internal/...) without an import cycle.
//
// A technical suffix package in the spirit of stdlib `httptest`/`iotest`: not a
// Soul Stack vocabulary entity, but test tooling next to the instrumented code.
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

// Scrape renders the current Prometheus snapshot of the given Gatherer in
// exposition format (text/plain; version=0.0.4) and returns the body as a
// string. Any render/scrape error → t.Fatal.
//
// The caller passes g = reg.Gatherer() (obs.Registry.Gatherer); by working
// through the [prometheus.Gatherer] interface the package does not pull in shared/obs.
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

// Contains is a convenience wrapper: true if the Gatherer's scrape contains substr.
func Contains(t testing.TB, g prometheus.Gatherer, substr string) bool {
	t.Helper()
	return strings.Contains(Scrape(t, g), substr)
}
