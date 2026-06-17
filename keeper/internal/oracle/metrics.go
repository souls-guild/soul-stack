package oracle

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// OracleMetrics — набор Prometheus-collector-ов Oracle reactor-роутера (ADR-030
// S4: safety + observability). Регистрируется helper-ом поверх компонент-
// агностичного [obs.Registry] (ADR-024 §4.0) — тем же паттерном, что
// [augur.RegisterBrokerMetrics] / [reaper.RegisterReaperMetrics]: registry-core
// не знает про конкретные метрики, keeper_oracle_*-метрики — частность Oracle.
//
// Метрики живут здесь (keeper/internal/oracle), а не в shared/obs: они привязаны
// к keeper-внутреннему reactor-у Portent→Decree и не переиспользуются Soul-ом
// (ADR-011: shared/ — действительно поперечный код; Oracle — Keeper-side).
//
// БЕЗОПАСНОСТЬ + кардинальность (ADR-024 §2.2): в label-ы НЕ кладём decree-name,
// sid, apply_id, beacon-name, payload — это high-cardinality (десятки тысяч
// хостов × правил) и/или недоверенный вход. Кто именно сработал — в audit-log
// (`oracle.fired` / `decree.circuit_tripped`) и trace, не в метрику. Все
// collector-ы — без label-ов (closed-enum-разреза тут нет: один Oracle-поток).
//
// Имена — Prometheus convention (snake_case, _total для counter; ADR-024 §2.1),
// префикс keeper_ (роль компонента).
type OracleMetrics struct {
	// portentsReceived — счётчик принятых PortentEvent-ов (на вход
	// handlePortentEvent с непустым beacon_name). Знаменатель для остальных:
	// сколько beacon-событий вообще дошло до reactor-а.
	portentsReceived prometheus.Counter

	// decreesMatched — счётчик Decree-срабатываний, прошедших весь фильтр
	// (subject-match + membership + where-CEL + НЕ в cooldown) и дошедших до
	// постановки. Инкрементируется per-Decree (один Portent может сматчить
	// несколько Decree-ов). Меньше portentsReceived из-за default-deny.
	decreesMatched prometheus.Counter

	// scenariosEnqueued — счётчик named-scenario, успешно поставленных в
	// work-queue (ADR-027) Oracle-реакцией. Равно числу записанных fire-ов;
	// расхождение с decreesMatched — сбои enqueue (см. лог).
	scenariosEnqueued prometheus.Counter

	// cooldownBlocked — счётчик Decree-срабатываний, отсечённых cooldown-ом
	// per-(decree, subject) (loop-prevention, ADR-030(a)). Рост — частые
	// edge-события на одном правиле; первый барьер против шторма работает.
	cooldownBlocked prometheus.Counter

	// circuitTripped — счётчик авто-disable Decree circuit-breaker-ом (ADR-030(a):
	// N срабатываний за окно → enabled=false + alert). Любой ненулевой прирост —
	// нештатная ситуация (правило сорвалось в петлю и было заглушено),
	// alert-кандидат.
	circuitTripped prometheus.Counter
}

// RegisterOracleMetrics создаёт keeper_oracle_*-collectors и регистрирует их в
// [obs.Registry]. Возвращает дескриптор для wire-up через [grpc.OracleDeps].
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на одном
// Registry); падать сразу удобнее ленивой инициализации (паттерн идентичен
// [augur.RegisterBrokerMetrics] / [reaper.RegisterReaperMetrics]).
func RegisterOracleMetrics(reg *obs.Registry) *OracleMetrics {
	m := &OracleMetrics{
		portentsReceived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_portents_received_total",
			Help: "Количество принятых PortentEvent-ов reactor-ом Oracle (с непустым beacon_name).",
		}),
		decreesMatched: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_decrees_matched_total",
			Help: "Количество Decree-срабатываний, прошедших весь фильтр (subject/membership/where/cooldown) и дошедших до постановки.",
		}),
		scenariosEnqueued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_scenarios_enqueued_total",
			Help: "Количество named-scenario, успешно поставленных в work-queue Oracle-реакцией.",
		}),
		cooldownBlocked: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_cooldown_blocked_total",
			Help: "Количество Decree-срабатываний, отсечённых cooldown-ом per-(decree, subject) (loop-prevention).",
		}),
		circuitTripped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_oracle_circuit_tripped_total",
			Help: "Количество авто-disable Decree circuit-breaker-ом (N срабатываний за окно → enabled=false).",
		}),
	}
	reg.Registerer().MustRegister(
		m.portentsReceived, m.decreesMatched,
		m.scenariosEnqueued, m.cooldownBlocked, m.circuitTripped,
	)
	return m
}

// ObservePortentReceived инкрементирует счётчик принятых Portent-ов.
// nil-получатель — no-op: Oracle-handler может подниматься без obs-стека
// (unit-тесты, сборки без metrics-listener-а), caller не проверяет nil.
func (m *OracleMetrics) ObservePortentReceived() {
	if m == nil {
		return
	}
	m.portentsReceived.Inc()
}

// ObserveDecreeMatched инкрементирует счётчик Decree, дошедших до постановки.
// nil-получатель — no-op.
func (m *OracleMetrics) ObserveDecreeMatched() {
	if m == nil {
		return
	}
	m.decreesMatched.Inc()
}

// ObserveScenarioEnqueued инкрементирует счётчик успешно поставленных scenario.
// nil-получатель — no-op.
func (m *OracleMetrics) ObserveScenarioEnqueued() {
	if m == nil {
		return
	}
	m.scenariosEnqueued.Inc()
}

// ObserveCooldownBlocked инкрементирует счётчик срабатываний, отсечённых
// cooldown-ом. nil-получатель — no-op.
func (m *OracleMetrics) ObserveCooldownBlocked() {
	if m == nil {
		return
	}
	m.cooldownBlocked.Inc()
}

// ObserveCircuitTripped инкрементирует счётчик авто-disable Decree circuit-
// breaker-ом. nil-получатель — no-op.
func (m *OracleMetrics) ObserveCircuitTripped() {
	if m == nil {
		return
	}
	m.circuitTripped.Inc()
}
