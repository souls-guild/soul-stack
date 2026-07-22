// Package obs — the cross-cutting observability infrastructure of Soul Stack:
// Prometheus metrics, HTTP instrumentation, and OTel provider bootstrap. Used by keeper
// and soul (keeper-side wired; Soul wire-up — Slice 1).
//
// Under the "metrics publication / OpenTelemetry / Hot-reload / … out of the box"
// requirement (docs/requirements.md "Architectural requirements"): shared/obs lives in
// the shared/ module so both binaries build with the same metrics/traces stack without
// duplication. Per ADR-024 the metrics channel is Prometheus-primary (pull `/metrics`),
// OTel is a bridge for traces + optional metrics push (see docs/observability.md).
//
// Registry is a dedicated [prometheus.Registry], not the global
// [prometheus.DefaultRegisterer]. Without a default registry:
//   - tests don't share state through a global (race / re-register panic);
//   - core collectors (go-runtime, process) are wired explicitly — not forced on
//     library code;
//   - two independent instances (e.g. for a unit test in one process) coexist without
//     name conflicts.
//
// Registry is component-agnostic: both keeper_* and soul_* metrics are registered in it
// via component-specific helpers. Cross-cutting ones (needed by both binaries, neutral
// to their internal types) live here — e.g. [RegisterHTTPMetrics]. Subsystem collectors
// live NEXT TO the subsystem and are registered on top of this Registry from outside —
// e.g. keeper-only RegisterReaperMetrics in keeper/internal/reaper (placement rule
// docs/observability.md §4.0). The registry core itself knows nothing about concrete
// metrics — this keeps the boundary between the cross-cutting foundation and subsystem
// instrumentation (ADR-024 §2: distinguishing keeper_*/soul_* by metric prefix, not by
// registry structure).
package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry — a component-agnostic descriptor of the Prometheus stack (keeper or soul).
// Owns a dedicated registry with the base go/process collectors; concrete subsystem
// metrics are registered by separate helpers on top of [Registry.Registerer] —
// cross-cutting ones here (see http.go), subsystem-local next to the subsystem (see
// docs/observability.md §4.0).
//
// Created once in main, passed to subsystems to register their metrics and to the
// `/metrics` handler to expose the same registry.
type Registry struct {
	reg *prometheus.Registry
}

// NewRegistry assembles the component-agnostic observability stack: a dedicated
// [prometheus.NewRegistry] + base collectors. No component-specific (keeper_*/soul_*)
// metrics here — they're attached by separate helpers on top of the ready Registry.
//
// Registers go-runtime and process collectors (memory, goroutines, fds, gc) — without
// them a Prometheus scrape is useless in production: application metrics alone don't
// answer "who's leaking" (ADR-024 §1.1).
func NewRegistry() *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return &Registry{reg: reg}
}

// Gatherer returns [prometheus.Gatherer] for the `/metrics` exposition handler. Used by
// meta/metrics.go via [promhttp.HandlerFor].
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// Registerer returns [prometheus.Registerer] for subsystems that register their own
// metrics: cross-cutting ([RegisterHTTPMetrics]) and subsystem-local next to the
// subsystem (Reaper / gRPC / scenario / RBAC / apply cycle, docs/observability.md §4.0).
func (r *Registry) Registerer() prometheus.Registerer { return r.reg }

// MetricsHandler returns the HTTP handler for `/metrics` on this Registry. Defaults to
// the exposition format `text/plain; version=0.0.4` (Prometheus 2.x scrape-compatible).
//
// `EnableOpenMetrics: false` is deliberate. The OpenMetrics format is not standardized
// in the Prometheus exporter spec (selected via the `Accept` header), and standard
// scrapers (Prometheus, VictoriaMetrics, Grafana Agent) understand the text format the
// same way. We'll enable it when there's a clear triple-test from the user.
func (r *Registry) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		// Any serialization failure (metrics bug) — 500 with no body. On /metrics we
		// return a body only on success, so as not to confuse a scraper with a partial
		// payload.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}
