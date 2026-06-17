package grpc

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// GRPCMetrics — набор Prometheus-collector-ов EventStream-подсистемы Keeper-а
// (Keeper↔Soul gRPC по ADR-012). Регистрируется отдельным helper-ом поверх
// компонент-агностичного [obs.Registry] — тем же паттерном, что
// [obs.RegisterHTTPMetrics] / [obs.RegisterReaperMetrics]: registry-core не
// знает про конкретные метрики, а keeper_grpc_*-метрики — частность
// gRPC-фасада Keeper-а.
//
// Метрики живут здесь (keeper/internal/grpc), а не в shared/obs, потому что
// в отличие от HTTP-middleware и Reaper-collector-ов они привязаны к
// keeper-внутренним типам EventStream-подсистемы и не переиспользуются
// Soul-ом (ADR-011: shared/ — действительно поперечный код).
//
// Имена — Prometheus convention (snake_case, _total для counters, _active
// для gauge мгновенного состояния; ADR-024 §2.1). Labels — closed enum-ы,
// cardinality-safe (ADR-024 §2.2): sid / apply_id в labels НЕ кладём — это
// blow-up по числу хостов, их разрез — в trace/log (см. dispatch-span в
// outbound.go).
type GRPCMetrics struct {
	// streamsActive — число открытых EventStream-стримов прямо сейчас.
	// Inc при handshake (после Hello→HelloReply), Dec при выходе handler-а.
	// Gauge без label-ов: кардинальность по sid недопустима; разрез «сколько
	// Soul-ов на этом инстансе» виден из метки instance Prometheus-target-а.
	streamsActive prometheus.Gauge

	// messagesTotal — счётчик app-сообщений стрима, разрезанный по
	// направлению (`from_soul` — принятые в receive-loop-е, `to_soul` —
	// отправленные в send-loop-е). Тип payload в label НЕ выносим: oneof-
	// вариантов немного, но разрез по типу даёт histogram-у вопросов, на
	// которые отвечает trace; direction — достаточный минимум для rate/error-
	// budget наблюдения за стримом.
	messagesTotal *prometheus.CounterVec

	// applyDispatchTotal — счётчик попыток отправить ApplyRequest в Soul
	// ([Outbound.SendApply]), разрезанный по результату (`ok` — enqueue/
	// publish успешен, `failed` — ErrSoulNotConnected / ErrOutboundQueueFull).
	// Это keeper→soul dispatch-метрика прогона; рост `failed` — сигнал, что
	// Soul-ы недоступны / очереди переполнены.
	applyDispatchTotal *prometheus.CounterVec

	// bootstrapTotal — счётчик онбординг-попыток Soul-а через unary Bootstrap
	// ([bootstrapHandler.Bootstrap]), разрезанный по результату (`ok` — seed
	// выпущен и soul flip-нут в connected, `failed` — любой не-ok-исход:
	// невалидный токен / CSR / Vault-fail / tx-fail). Bootstrap — отдельный
	// listener (server-only TLS), не EventStream: streams_active его НЕ считает.
	// Рост `failed` — сигнал проблем онбординга (PKI down, мусорные токены).
	bootstrapTotal *prometheus.CounterVec

	// runResultStaleTotal — счётчик отвергнутых при приёме RunResult-ов от
	// устаревших попыток (ADR-027(g), gate-1 epoch-check на приёме). Инкремент в
	// [correlateRunResult], когда RunResult.attempt < apply_runs.attempt: результат
	// от мёртвого Ward-а, чьё задание уже пере-claim-нуто с бОльшим epoch — коммит
	// state НЕ делается (stale-drop). Без label-ов: разрез по sid/apply_id —
	// cardinality blow-up, детализация уходит в Info-лог correlateRunResult.
	// Рост счётчика — нормальный сигнал работы recovery (failback на другой
	// инстанс), не ошибка.
	runResultStaleTotal prometheus.Counter

	// applyOrphanedTotal — счётчик dispatched-строк, терминалённых в `orphaned`
	// по Soul-reconcile (ADR-027(g), S6). Инкремент в [handleWardRoster] на
	// число осиротевших строк, когда Soul на reconnect НЕ объявил их apply_id в
	// WardRoster (RunResult по ним не придёт — «Keeper и Soul оба мертвы после
	// отдачи»). Без label-ов: разрез по sid/apply_id — cardinality blow-up,
	// детализация уходит в Info-лог handleWardRoster. Рост — нормальный сигнал
	// работы recovery после краха пары Keeper+Soul, не ошибка.
	applyOrphanedTotal prometheus.Counter
}

// Направления для keeper_grpc_messages_total. Closed enum в 2 значения.
const (
	directionFromSoul = "from_soul"
	directionToSoul   = "to_soul"
)

// Результаты для keeper_grpc_apply_dispatch_total. Closed enum в 2 значения.
const (
	dispatchResultOK     = "ok"
	dispatchResultFailed = "failed"
)

// Результаты для keeper_grpc_bootstrap_total. Closed enum в 2 значения:
// `failed` агрегирует все не-ok-исходы (анти-enum онбординга — детализация
// причины уходит в trace/log, не в метрику-label).
const (
	bootstrapResultOK     = "ok"
	bootstrapResultFailed = "failed"
)

// RegisterGRPCMetrics создаёт keeper_grpc_*-collectors и регистрирует их в
// [obs.Registry]. Возвращает дескриптор для wire-up через [EventStreamDeps]
// и [OutboundDeps].
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на
// одном Registry); падать сразу удобнее, чем носить ленивую инициализацию
// (паттерн идентичен [obs.RegisterHTTPMetrics] / [obs.RegisterReaperMetrics]).
func RegisterGRPCMetrics(reg *obs.Registry) *GRPCMetrics {
	m := &GRPCMetrics{
		streamsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_grpc_streams_active",
			Help: "Число открытых EventStream-стримов Keeper↔Soul прямо сейчас.",
		}),
		messagesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_grpc_messages_total",
				Help: "Количество app-сообщений EventStream-а, разрезанное по направлению (from_soul/to_soul).",
			},
			[]string{"direction"},
		),
		applyDispatchTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_grpc_apply_dispatch_total",
				Help: "Количество попыток отправить ApplyRequest в Soul, разрезанное по результату (ok/failed).",
			},
			[]string{"result"},
		),
		bootstrapTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_grpc_bootstrap_total",
				Help: "Количество онбординг-попыток Soul-а через Bootstrap-RPC, разрезанное по результату (ok/failed).",
			},
			[]string{"result"},
		),
		runResultStaleTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_runresult_stale_total",
			Help: "Количество отвергнутых RunResult-ов от устаревших попыток (attempt < apply_runs.attempt, gate-1 epoch-check на приёме).",
		}),
		applyOrphanedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_apply_orphaned_total",
			Help: "Количество dispatched-строк, терминалённых в orphaned по Soul-reconcile (Soul на reconnect не объявил apply_id в WardRoster, ADR-027(g)).",
		}),
	}
	reg.Registerer().MustRegister(m.streamsActive, m.messagesTotal, m.applyDispatchTotal, m.bootstrapTotal, m.runResultStaleTotal, m.applyOrphanedTotal)
	return m
}

// IncStreams / DecStreams — nil-safe обёртки над gauge активных стримов.
// Inc после handshake, Dec в defer handler-а. nil-получатель — no-op:
// EventStream может подниматься без observability (unit-тесты, dev-сборка).
func (m *GRPCMetrics) IncStreams() {
	if m == nil {
		return
	}
	m.streamsActive.Inc()
}

func (m *GRPCMetrics) DecStreams() {
	if m == nil {
		return
	}
	m.streamsActive.Dec()
}

// ObserveMessage инкрементирует счётчик сообщений по направлению.
// nil-получатель — no-op.
func (m *GRPCMetrics) ObserveMessage(direction string) {
	if m == nil {
		return
	}
	m.messagesTotal.WithLabelValues(direction).Inc()
}

// ObserveApplyDispatch инкрементирует счётчик dispatch-а ApplyRequest по
// результату (err == nil → ok, иначе failed). nil-получатель — no-op.
func (m *GRPCMetrics) ObserveApplyDispatch(err error) {
	if m == nil {
		return
	}
	result := dispatchResultOK
	if err != nil {
		result = dispatchResultFailed
	}
	m.applyDispatchTotal.WithLabelValues(result).Inc()
}

// ObserveBootstrap инкрементирует счётчик онбординг-попыток по результату
// (err == nil → ok, иначе failed). nil-получатель — no-op: Bootstrap-listener
// может подниматься без obs-стека (unit-тесты, dev-сборка).
func (m *GRPCMetrics) ObserveBootstrap(err error) {
	if m == nil {
		return
	}
	result := bootstrapResultOK
	if err != nil {
		result = bootstrapResultFailed
	}
	m.bootstrapTotal.WithLabelValues(result).Inc()
}

// ObserveRunResultStale инкрементирует счётчик RunResult-ов от устаревших попыток
// (gate-1 epoch-check на приёме, ADR-027(g)). Вызывается из [correlateRunResult]
// при stale-drop-е. nil-получатель — no-op: correlateRunResult исполняется и в
// unit-сборке без obs-стека.
func (m *GRPCMetrics) ObserveRunResultStale() {
	if m == nil {
		return
	}
	m.runResultStaleTotal.Inc()
}

// ObserveApplyOrphaned добавляет n к счётчику осиротевших dispatched-строк
// (Soul-reconcile, ADR-027(g), S6). Вызывается из [handleWardRoster] на число
// строк, переведённых в `orphaned`. nil-получатель — no-op: handleWardRoster
// исполняется и в unit-сборке без obs-стека. n<=0 — no-op (нечего сиротить).
func (m *GRPCMetrics) ObserveApplyOrphaned(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.applyOrphanedTotal.Add(float64(n))
}
