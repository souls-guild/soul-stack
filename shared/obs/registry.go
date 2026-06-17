// Package obs — сквозная observability-инфраструктура Soul Stack: метрики
// Prometheus, HTTP-инструментация и bootstrap OTel-провайдера. Используется
// keeper-ом и soul-ом (keeper-side wired; Soul wire-up — Slice 1).
//
// Под требование «публикация метрик / OpenTelemetry / Hot-reload / …
// из коробки» (docs/requirements.md «Архитектурные требования»):
// shared/obs живёт в shared/ модуле, чтобы оба бинаря собирались с
// одним и тем же стеком метрик/трейсов без дублирования. По ADR-024
// канал метрик — Prometheus-primary (pull `/metrics`), OTel — мост для
// трейсов + опц. push метрик (см. docs/observability.md).
//
// Registry — dedicated [prometheus.Registry], не глобальный
// [prometheus.DefaultRegisterer]. Без default-registry:
//   - тесты не делят состояние через global (race / re-register panic);
//   - core-collectors (go-runtime, process) подключаются явно — не
//     навязываются библиотечному коду;
//   - две независимых инстанции (например, для unit-теста в одном
//     processe) сосуществуют без конфликта имён.
//
// Registry компонент-агностичен: и keeper_*, и soul_*-метрики
// регистрируются в нём через component-specific helper-ы. Сквозные
// (нужные обоим бинарям, нейтральные к их internal-типам) живут здесь —
// например [RegisterHTTPMetrics]. Collector-ы подсистем живут РЯДОМ с
// подсистемой и регистрируются поверх этого Registry извне — например
// keeper-only RegisterReaperMetrics в keeper/internal/reaper (правило
// размещения docs/observability.md §4.0). Сам registry-core не знает про
// конкретные метрики — это держит границу между сквозным фундаментом и
// инструментацией подсистем (ADR-024 §2: различение keeper_*/soul_*
// префиксом метрики, не структурой registry).
package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry — компонент-агностичный дескриптор Prometheus-стека
// (keeper или soul). Owns dedicated registry с базовыми go/process-
// collectors; конкретные метрики подсистем регистрируются отдельными
// helper-ами поверх [Registry.Registerer] — сквозные здесь (см. http.go),
// subsystem-local рядом с подсистемой (см. docs/observability.md §4.0).
//
// Создаётся один раз в main, передаётся в подсистемы для регистрации их
// метрик и в handler `/metrics` для exposition того же registry.
type Registry struct {
	reg *prometheus.Registry
}

// NewRegistry собирает компонент-агностичный observability-стек:
// dedicated [prometheus.NewRegistry] + базовые collectors. Никаких
// component-specific (keeper_*/soul_*) метрик здесь — они вешаются
// отдельными helper-ами поверх готового Registry.
//
// Регистрирует go-runtime и process-collectors (memory, goroutines, fds,
// gc) — без них Prometheus-scrape бесполезен в production: одни
// application-метрики не дают ответа «кто течёт» (ADR-024 §1.1).
func NewRegistry() *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return &Registry{reg: reg}
}

// Gatherer возвращает [prometheus.Gatherer] для exposition-handler-а
// `/metrics`. Используется meta/metrics.go через [promhttp.HandlerFor].
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// Registerer возвращает [prometheus.Registerer] для подсистем, которые
// регистрируют собственные метрики: сквозные ([RegisterHTTPMetrics]) и
// subsystem-local рядом с подсистемой (Reaper / gRPC / scenario / RBAC /
// apply-цикл, docs/observability.md §4.0).
func (r *Registry) Registerer() prometheus.Registerer { return r.reg }

// MetricsHandler возвращает HTTP-handler для `/metrics` под этот
// Registry. По умолчанию использует exposition-format `text/plain;
// version=0.0.4` (Prometheus 2.x scrape-compatible).
//
// `EnableOpenMetrics: false` — намеренно. OpenMetrics-format
// нестандартизирован в Prometheus exporter spec (выбирается через
// `Accept`-header), и стандартные scraper-ы (Prometheus, VictoriaMetrics,
// Grafana Agent) понимают text-format одинаково. Включим, когда
// появится понятный triple-test от пользователя.
func (r *Registry) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		// Любые сбои сериализации (metrics-bug) — 500 без body. В /metrics
		// body отдаём только при успехе, чтобы не сбивать scraper-а
		// partial-payload-ом.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}
