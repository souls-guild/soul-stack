package soulprint

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// SoulprintMetrics — набор Prometheus-collector-ов сбора фактов о хосте
// (Soulprint, ADR-018). Регистрируется helper-ом поверх компонент-агностичного
// [obs.Registry] — тем же паттерном, что пилот
// [keeper/internal/grpc.RegisterGRPCMetrics] (docs/observability.md §4.0).
//
// Метрики живут здесь (soul/internal/soulprint): сбор фактов — Soul-side
// операция, не переиспользуется Keeper-ом (ADR-011). По ADR-011 soul НЕ
// импортирует keeper; инструментация — через нейтральный shared/obs.
//
// Имена — ADR-024 §2.1: префикс soul_, snake_case, _total для counter,
// histogram по величине + _seconds. Labels — closed enum (§2.2): sid в labels
// не кладём (cardinality), разрез по хосту — в OTel resource-attrs (§3).
type SoulprintMetrics struct {
	// collectionsTotal — счётчик снимков фактов, разрезанный по результату
	// (`ok` / `failed`). Collect best-effort и не возвращает error (ADR-018),
	// поэтому в текущей реализации инкрементируется только `ok`; `failed`
	// зарезервирован под будущие fatal-сценарии сбора (closed enum в 2 значения).
	collectionsTotal *prometheus.CounterVec

	// collectDuration — длительность одного снимка фактов (Collect), в секундах.
	// Сбор лёгкий (чтение /proc, /etc/os-release, net.*) — десятки мс; histogram
	// ловит регрессии (например, медленный DNS-lookup в FQDN-резолве).
	collectDuration prometheus.Histogram
}

// Результаты для soul_soulprint_collections_total. Closed enum в 2 значения.
const (
	collectResultOK     = "ok"
	collectResultFailed = "failed"
)

// RegisterSoulprintMetrics создаёт soul_soulprint_*-collectors и регистрирует
// их в [obs.Registry]. Возвращает дескриптор для wire-up через [Collector].
//
// MustRegister: дубликат-регистрация — programmer error; падать сразу удобнее
// ленивой инициализации (паттерн идентичен пилоту RegisterGRPCMetrics).
func RegisterSoulprintMetrics(reg *obs.Registry) *SoulprintMetrics {
	m := &SoulprintMetrics{
		collectionsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "soul_soulprint_collections_total",
				Help: "Количество снимков фактов о хосте, разрезанное по результату (ok/failed).",
			},
			[]string{"result"},
		),
		collectDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "soul_soulprint_collect_duration_seconds",
			Help: "Длительность одного снимка фактов о хосте (Collect), в секундах.",
			// Сбор фактов лёгкий (локальные чтения), цель — миллисекунды. Узкие
			// buckets внизу ловят норму; верх до 5s — на случай медленного
			// FQDN/DNS-резолва на проблемном хосте.
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}),
	}
	reg.Registerer().MustRegister(m.collectionsTotal, m.collectDuration)
	return m
}

// ObserveCollection инкрементирует счётчик снимков по результату.
// nil-получатель — no-op: Collector может подниматься без obs-стека
// (unit-тесты, push-режим без metrics-listener-а).
func (m *SoulprintMetrics) ObserveCollection(result string) {
	if m == nil {
		return
	}
	m.collectionsTotal.WithLabelValues(result).Inc()
}

// ObserveCollectDuration записывает длительность снимка в секундах.
// nil-получатель — no-op.
func (m *SoulprintMetrics) ObserveCollectDuration(seconds float64) {
	if m == nil {
		return
	}
	m.collectDuration.Observe(seconds)
}
