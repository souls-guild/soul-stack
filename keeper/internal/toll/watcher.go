package toll

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Watcher — per-Keeper-инстанс наблюдатель disconnect-событий gRPC EventStream-а
// (ADR-038(b)). НЕ goroutine: пассивный объект, методы которого зовут
// EventStream-handler-ы при выходе из receive-loop-а. Состояния не держит
// (`StartedAt` для warmup-окна — единственное поле); один экземпляр на keeper
// process разделяется между всеми стримами.
//
// Фильтрация (ADR-038(c)):
//  1. Warmup-immunity: первые [Config.WarmupDelay] после старта инстанса
//     disconnect-ы НЕ публикуются (cluster cold-start защита). Метрика
//     [Metrics.IncWarmupSkipped] всё равно растёт — оператор видит факт.
//  2. Graceful-shutdown: если caller указывает gracefulShutdown=true (отмена
//     ctx по shutdown-у самого инстанса), disconnect отбрасывается. Метрика
//     [Metrics.IncGracefulSkipped].
//  3. Live disconnect: после фильтров — ZADD в общий Redis sorted-set
//     ([Publisher]) + [Metrics.IncDisconnect].
//
// Per-coven counter инкрементируется ВСЕГДА (включая отбракованные), чтобы
// counter был наблюдательным rate-источником, не фильтрованным leader-ом.
// Wait — поправка: ADR-038(g) описывает counter как «не-graceful disconnect-ы»;
// держим counter ПОСЛЕ фильтров (graceful + warmup отброшены, см. NotifyDisconnect).
type Watcher struct {
	kid       string
	publisher Publisher
	logger    *slog.Logger
	metrics   *Metrics
	startedAt time.Time
	warmup    time.Duration
}

// Config — параметры Watcher-а.
type Config struct {
	// KID — идентификатор keeper-инстанса (пишется в member-value sorted-set-а
	// для логирования / диагностики leader-агрегатора).
	KID string
	// WarmupDelay — окно immunity после старта инстанса. <=0 → дефолт (60s),
	// чтобы fake-watcher в тестах не зависел от config-резолва.
	WarmupDelay time.Duration
}

// defaultWarmupDelay — резерв на случай WarmupDelay<=0 (unit-тесты могут
// передать 0 и явно сбросить StartedAt). 60s соответствует ADR-038.
const defaultWarmupDelay = 60 * time.Second

// NewWatcher собирает Watcher. publisher / logger обязательны; metrics
// опционален (nil → счётчики выключены, nil-safe методы [Metrics]).
//
// startedAt = NOW: warmup отсчитывается от construction-а, не от первого
// disconnect-а. Для unit-тестов есть [Watcher.setStartedAt] (test-helper).
func NewWatcher(cfg Config, publisher Publisher, metrics *Metrics, logger *slog.Logger) (*Watcher, error) {
	if cfg.KID == "" {
		return nil, errors.New("toll.NewWatcher: empty KID")
	}
	if publisher == nil {
		return nil, errors.New("toll.NewWatcher: nil publisher")
	}
	if logger == nil {
		return nil, errors.New("toll.NewWatcher: nil logger")
	}
	warmup := cfg.WarmupDelay
	if warmup <= 0 {
		warmup = defaultWarmupDelay
	}
	return &Watcher{
		kid:       cfg.KID,
		publisher: publisher,
		logger:    logger,
		metrics:   metrics,
		startedAt: time.Now(),
		warmup:    warmup,
	}, nil
}

// NotifyDisconnect — hook для gRPC EventStream-cleanup-а. Caller передаёт
// SID отвалившегося Soul-а, его covens (если известны; пустая строка
// допустима) и флаг gracefulShutdown (true → инициированное самим инстансом
// закрытие, например Watchman-shedding или graceful keeper-shutdown).
//
// Метод non-blocking: на любой проблеме (Publisher-error, ctx.Done) логирует
// debug и продолжает — disconnect-флоу EventStream-handler-а не должен
// зависеть от живости Toll-инфраструктуры. Publisher-error не fatal
// (Redis temporarily down — Leader всё равно проигнорирует пустое окно и
// не сбросит false-positive).
func (w *Watcher) NotifyDisconnect(ctx context.Context, sid, coven string, gracefulShutdown bool) {
	if w == nil {
		return
	}
	// Warmup-immunity: первые WarmupDelay disconnect-ы не публикуем.
	if time.Since(w.startedAt) < w.warmup {
		w.metrics.IncWarmupSkipped()
		w.logger.Debug("toll: disconnect skipped (warmup immunity)",
			slog.String("sid", sid),
			slog.Duration("since_start", time.Since(w.startedAt)),
		)
		return
	}
	// Graceful-shutdown: не считаем плановое закрытие за отток.
	if gracefulShutdown {
		w.metrics.IncGracefulSkipped()
		w.logger.Debug("toll: disconnect skipped (graceful shutdown)",
			slog.String("sid", sid),
		)
		return
	}
	// Post-filter: counter растёт + публикация в общий sorted-set.
	w.metrics.IncDisconnect(coven)
	if err := w.publisher.PublishDisconnect(ctx, sid, w.kid, coven, time.Now()); err != nil {
		// Не fatal: Leader на следующем тике проигнорирует пустое окно. Лог
		// уровня debug, потому что Redis-флапы могут быть частыми, а сам
		// disconnect уже отражён в counter.
		w.logger.Debug("toll: publish disconnect failed",
			slog.String("sid", sid),
			slog.Any("error", err),
		)
	}
}

// setStartedAt — test-helper для unit-тестов: позволяет сдвинуть startedAt
// в прошлое (warmup истёк) либо в будущее (warmup ещё активен) без sleep-ов
// и без зависимости от реального clock-а.
func (w *Watcher) setStartedAt(t time.Time) { w.startedAt = t }
