// Package watchman — изоляция-детект Keeper-инстанса и активное закрытие
// (shedding) его локальных EventStream-стримов (soul-shedding S2, ADR-002 HA-
// кластер Keeper).
//
// Проблема: когда Keeper-инстанс изолирован (потерял PG/Redis) или нездоров,
// его уже-установленные долгоживущие EventStream-стримы к Souls сами по себе НЕ
// закрываются. `/readyz` уводит только НОВЫЕ подключения (LB смотрит на
// readiness), но существующие gRPC bidi-стримы от HTTP-health не зависят —
// Souls продолжают висеть на нездоровом инстансе и не уходят на живой Keeper.
//
// Watchman — фоновая goroutine на инстансе: периодически пингует те же
// зависимости, что `/readyz` (PG + Redis), и при УСТОЙЧИВОЙ изоляции активно
// закрывает ВСЕ локальные стримы (hard-close, без drain): отменяет per-stream
// ctx каждого ([StreamCloser.CloseAll]) → EventStream-handler возвращается →
// gRPC шлёт Soul-у EOF → Soul по reconnect-loop/failback-list уходит на живой
// Keeper. При восстановлении зависимостей — возобновляет нормальную работу
// (новые стримы принимаются listener-ом, lease-renewal / Acolyte оживают сами).
//
// Централизация: решение «я изолирован» принимается ТОЛЬКО здесь, не дублируется
// в каждом per-stream renewal-loop-е (после CloseAll их renewal-горутины умирают
// вместе со стримами). Источник истины об изоляции — один.
//
// Debounce/flap-guard: изоляция объявляется только после [Config.FailThreshold]
// последовательных провалов probe, а не на первом же. Единичный сетевой spike
// не должен сбросить весь флот стримов разом (thundering-herd reconnect по всему
// кластеру). Один успешный probe сбрасывает счётчик. Если инстанс уже в
// состоянии «изолирован» и зависимости вернулись — Watchman логирует recovery и
// снова считает с нуля; повторного CloseAll в изоляции НЕ делает (стримы уже
// закрыты, новых на изолированном инстансе быть не должно).
package watchman

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	// DefaultInterval — период probe-тика Watchman. 5s — баланс между скоростью
	// реакции на изоляцию (= Interval × FailThreshold ≈ 15s до shedding-а) и
	// нагрузкой ping-ов на PG/Redis. Тот же порядок, что DefaultConclaveRenewInterval.
	DefaultInterval = 5 * time.Second

	// DefaultFailThreshold — число подряд идущих провалов probe до объявления
	// изоляции. 3 — debounce от единичных spike-ов: кратковременный обрыв
	// (один-два тика) переживается без shedding-а, устойчивая потеря (>=3 тика)
	// триггерит. Меньшее значение — агрессивный shedding на дрожащей сети;
	// большее — медленная реакция на реальную изоляцию.
	DefaultFailThreshold = 3

	// defaultProbeTimeout — жёсткий timeout одного probe-вызова. Без него
	// зависший PG/Redis-ping подвесил бы probe-тик дольше Interval-а. 2s —
	// симметрично health.perCheckTimeout (`/readyz` использует тот же порядок).
	defaultProbeTimeout = 2 * time.Second
)

// ErrNoProbeDeps — конструктор не получил ни одной зависимости для probe.
// Watchman без зависимостей бессмыслен: ему нечего пинговать, изоляцию он не
// обнаружит. Caller (daemon) обязан передать хотя бы один Pinger.
var ErrNoProbeDeps = errors.New("watchman: at least one health probe is required")

// HealthProbe — узкая поверхность проверки доступности зависимостей инстанса.
// Probe возвращает nil, если все зависимости здоровы, и не-nil — если хотя бы
// одна недоступна (= признак изоляции). Реализация по умолчанию ([NewDepsProbe])
// композирует те же `health.Pinger`-ы, что и `/readyz` (PG + Redis), под
// per-check timeout-ом. Интерфейс сужает Watchman до одного метода — это
// позволяет fake-у в unit-тестах и держит probe-логику отделимой от lifecycle-а.
type HealthProbe interface {
	Probe(ctx context.Context) error
}

// StreamCloser — узкая поверхность принудительного закрытия всех локальных
// EventStream-стримов. Реализуется [keepergrpc.StreamManager] (CloseAll отменяет
// per-stream ctx каждого зарегистрированного стрима). Сужение до одного метода
// изолирует Watchman от полного реестра стримов и допускает fake в unit-тестах.
type StreamCloser interface {
	// CloseAll отменяет ctx всех локальных стримов и возвращает их число (для
	// лога/метрики). Идемпотентен (context.CancelFunc безопасна к повтору).
	CloseAll() int
}

// Metrics — наблюдаемость Watchman (опциональна). Передаётся nil → весь учёт
// выключен (unit-тесты / dev-сборка без observability); Watchman проверяет
// `w.metrics != nil` перед каждым вызовом.
type Metrics interface {
	// SetIsolated выставляет gauge keeper_watchman_isolated (1 = инстанс
	// объявлен изолированным, 0 = здоров).
	SetIsolated(isolated bool)
	// AddStreamsShed добавляет n к счётчику keeper_watchman_streams_shed_total
	// (сколько стримов закрыто shedding-ом за всё время).
	AddStreamsShed(n int)
}

// Config — параметры Watchman.
type Config struct {
	// Interval — период probe-тика. <=0 → [DefaultInterval].
	Interval time.Duration
	// FailThreshold — число подряд идущих провалов probe до shedding-а. <=0 →
	// [DefaultFailThreshold].
	FailThreshold int
	// ProbeTimeout — timeout одного probe-вызова. <=0 → [defaultProbeTimeout].
	ProbeTimeout time.Duration
}

// Watchman — изоляция-детект + shedding. Один экземпляр на keeper-инстанс,
// поднимается daemon-ом после EventStream-listener-а (нужен StreamManager) и
// Redis/PG (нужны зависимости probe).
//
// consecutiveFails / isolated — состояние debounce-машины. Мьютекса нет: они
// читаются/пишутся ТОЛЬКО из probe-loop-а (одна goroutine [Run]); тесты дёргают
// [Watchman.tick] так же из одной goroutine.
type Watchman struct {
	probe   HealthProbe
	closer  StreamCloser
	cfg     Config
	logger  *slog.Logger
	metrics Metrics
	probeTO time.Duration

	consecutiveFails int
	isolated         bool
}

// New собирает Watchman. probe / closer / logger обязательны; metrics
// опционален (nil → наблюдаемость выключена). Пустые поля Config дефолтятся.
func New(probe HealthProbe, closer StreamCloser, cfg Config, metrics Metrics, logger *slog.Logger) (*Watchman, error) {
	if probe == nil {
		return nil, errors.New("watchman: HealthProbe is required")
	}
	if closer == nil {
		return nil, errors.New("watchman: StreamCloser is required")
	}
	if logger == nil {
		return nil, errors.New("watchman: logger is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = DefaultFailThreshold
	}
	probeTO := cfg.ProbeTimeout
	if probeTO <= 0 {
		probeTO = defaultProbeTimeout
	}
	return &Watchman{
		probe:   probe,
		closer:  closer,
		cfg:     cfg,
		logger:  logger,
		metrics: metrics,
		probeTO: probeTO,
	}, nil
}

// Run — блокирующий probe-loop. Завершается по ctx.Done() (graceful shutdown).
// Caller (daemon) запускает его в goroutine и снимает через отмену ctx +
// cleanup-стек LIFO (как conclave/reaper).
//
// На каждом тике: probe под timeout-ом. Провал → инкремент счётчика подряд-
// провалов; достижение FailThreshold (и пока не в изоляции) → shedding
// (CloseAll). Успех → если был провальный счётчик / изоляция — лог recovery и
// сброс счётчика и флага изоляции. Повторного CloseAll в уже-объявленной
// изоляции НЕ делаем (стримы закрыты; новых на изолированном инстансе быть не
// должно — listener-у нужны живые PG/Redis для lease/seed-auth).
func (w *Watchman) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := w.runProbe(ctx)
			// Отмена самого probe-ctx из-за shutdown-а (ctx.Done между тиками) —
			// не считаем за провал зависимости: на следующей итерации цикл выйдет
			// по ctx.Done() выше.
			if err != nil && ctx.Err() != nil {
				return
			}
			w.tick(err)
		}
	}
}

// tick — одна итерация debounce-машины: учёт результата probe (nil = здоров),
// решение о shedding-е / recovery. Выделен из [Run] (тот лишь крутит ticker),
// чтобы декларативно тестировать debounce/изоляцию/recovery без таймеров.
// Исполняется в одной goroutine с Run (state без мьютекса).
func (w *Watchman) tick(err error) {
	if err != nil {
		w.consecutiveFails++
		w.logger.Warn("watchman: probe failed",
			slog.Int("consecutive_fails", w.consecutiveFails),
			slog.Int("threshold", w.cfg.FailThreshold),
			slog.Bool("isolated", w.isolated),
			slog.Any("error", err),
		)
		if w.consecutiveFails >= w.cfg.FailThreshold && !w.isolated {
			w.isolated = true
			w.setIsolated(true)
			n := w.closer.CloseAll()
			w.addStreamsShed(n)
			w.logger.Error("watchman: instance isolated — shedding all local EventStream streams",
				slog.Int("consecutive_fails", w.consecutiveFails),
				slog.Int("streams_shed", n),
			)
		}
		return
	}
	// Успешный probe.
	if w.isolated {
		w.logger.Info("watchman: dependencies recovered — resuming normal operation",
			slog.Int("prior_consecutive_fails", w.consecutiveFails),
		)
		w.isolated = false
		w.setIsolated(false)
	} else if w.consecutiveFails > 0 {
		// Spike пережит без объявления изоляции (debounce сработал).
		w.logger.Info("watchman: probe recovered before isolation threshold",
			slog.Int("prior_consecutive_fails", w.consecutiveFails),
		)
	}
	w.consecutiveFails = 0
}

// runProbe вызывает probe под per-tick timeout-ом.
func (w *Watchman) runProbe(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, w.probeTO)
	defer cancel()
	return w.probe.Probe(pctx)
}

func (w *Watchman) setIsolated(isolated bool) {
	if w.metrics != nil {
		w.metrics.SetIsolated(isolated)
	}
}

func (w *Watchman) addStreamsShed(n int) {
	if w.metrics != nil && n > 0 {
		w.metrics.AddStreamsShed(n)
	}
}
