// Package leaderloop — generic Redis-lease leader-loop для фоновых
// singleton-задач Keeper-кластера (ADR-006(d)).
//
// Это вынесенный из Reaper-runner-а универсальный каркас лидерства: захват
// Redis-lease → renewal-goroutine → периодический tick-callback у держателя →
// re-acquire при потере lease. Один и тот же loop переиспользуется HA-задачами
// (Reaper, Conductor-cadence-scheduler), которые отличаются только tick-логикой.
//
// Алгоритм (lease-семантика — точная копия исходного Reaper-runner-а):
//
//   - A: периодический tick через `time.Ticker` с интервалом из [Config.IntervalFn]
//     (hot-reload: интервал перечитывается на каждом тике).
//   - B: Redis-lease держится только пока крутится [Loop.Run]. На graceful-shutdown
//     (ctx.Done) lease освобождается через Release.
//   - C: TTL продлевается отдельной goroutine с шагом `lock_ttl/3`. Долгий tick
//     не даёт ключу истечь — Renew не блокируется tick-callback-ом.
//   - D: потеря lease (ErrLeaseLost) — сигнал tick-loop-у остановиться; затем loop
//     возвращается к acquire-фазе (re-acquire лидерства).
//   - E: ctx.Done — Release, выход с nil.
//
// lease-примитив (Acquire / Renew / Release / CAS-fencing) — в
// [keeper/internal/redis]; этот пакет лишь оркестрирует его жизненный цикл.
package leaderloop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// defaultAcquireBackoff — пауза между попытками Acquire при конфликте лидерства,
// если [Config.AcquireBackoff] не задан. Совпадает с историческим дефолтом
// Reaper-а: крупно достаточно, чтобы не флудить Redis, мелко достаточно, чтобы
// failover при падении лидера происходил в пределах нескольких секунд.
const defaultAcquireBackoff = 5 * time.Second

// Config — параметры leader-loop-а. Все поля, кроме [Config.AcquireBackoff] и
// [Config.OnLeaseChange], обязательны: отсутствие — программная ошибка caller-а,
// [New] возвращает error.
type Config struct {
	// LeaseKey — Redis-ключ лидерства. Один singleton-loop = один уникальный
	// ключ в кластере (например, "reaper:leader").
	LeaseKey string

	// Holder — идентификатор инстанса (KID), записывается в lease-ключ для
	// человекочитаемых логов и различения смен лидерства.
	Holder string

	// Redis — клиент, через который захватывается lease.
	Redis *redis.Client

	// Logger — slog-логгер. Структурированные поля: `key`, `holder`.
	Logger *slog.Logger

	// IntervalFn — интервал между тиками. Вызывается на каждом тике, что даёт
	// hot-reload: вернул новое значение → следующий тик планируется по нему.
	IntervalFn func() time.Duration

	// LockTTLFn — TTL Redis-lease-ключа. Вызывается на каждой acquire-итерации
	// (hot-reload между re-acquire). renew-интервал выводится как `lock_ttl/3`.
	LockTTLFn func() time.Duration

	// Tick — callback, исполняемый у держателя lease по interval (и один раз
	// сразу после acquire, не дожидаясь первого тика). Выполняется синхронно
	// в loop-goroutine: следующий тик не начнётся, пока текущий не вернётся.
	Tick func(ctx context.Context)

	// AcquireBackoff — пауза между попытками Acquire при конфликте лидерства.
	// Zero-value → [defaultAcquireBackoff].
	AcquireBackoff time.Duration

	// OnLeaseChange — опциональный callback смены статуса лидерства: true при
	// захвате lease, false при выходе из tick-loop-а (ctx.Done или lease-loss).
	// Nil — допустимо (no-op). Используется для метрики lease_held.
	OnLeaseChange func(held bool)
}

// Loop — корневая структура leader-loop-а. Один экземпляр на одну фоновую
// задачу. Создаётся через [New], запускается через [Loop.Run].
type Loop struct {
	cfg            Config
	acquireBackoff time.Duration
}

// New валидирует конфиг и возвращает Loop. На отсутствующие обязательные поля —
// error: программная ошибка caller-а (wire-up), не runtime-условие.
func New(cfg Config) (*Loop, error) {
	if cfg.LeaseKey == "" {
		return nil, errors.New("leaderloop.New: LeaseKey is required")
	}
	if cfg.Holder == "" {
		return nil, errors.New("leaderloop.New: Holder is required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("leaderloop.New: Redis is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("leaderloop.New: Logger is required")
	}
	if cfg.IntervalFn == nil {
		return nil, errors.New("leaderloop.New: IntervalFn is required")
	}
	if cfg.LockTTLFn == nil {
		return nil, errors.New("leaderloop.New: LockTTLFn is required")
	}
	if cfg.Tick == nil {
		return nil, errors.New("leaderloop.New: Tick is required")
	}
	backoff := cfg.AcquireBackoff
	if backoff <= 0 {
		backoff = defaultAcquireBackoff
	}
	return &Loop{cfg: cfg, acquireBackoff: backoff}, nil
}

// Run крутит loop до отмены ctx. Возвращает nil на graceful-stop (ctx.Done) и
// обёрнутую error на fatal-условия acquire-фазы.
//
// Алгоритм:
//
//  1. Каждые `acquireBackoff` пытаемся захватить lease, пока не получится или ctx.Done.
//  2. На успехе поднимаем renewal-goroutine (Renew каждый `lock_ttl/3`).
//  3. tick-loop по `time.Ticker(interval)`; первый Tick — сразу при acquire.
//  4. На ErrLeaseLost от renewal — gracefully выходим из tick-loop, делаем
//     Release только если вышли не через lease-loss (при loss ключ уже не наш),
//     возвращаемся к шагу 1 (re-acquire).
//  5. На ctx.Done — Release, выход.
func (l *Loop) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		lease, lockTTL, err := l.acquireWithBackoff(ctx)
		if err != nil {
			// acquireWithBackoff возвращает не-nil error только на отмену ctx
			// или программную ошибку Acquire-вызова (последние отлажены New-ом).
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("leaderloop.Run: acquire: %w", err)
		}

		l.cfg.Logger.Info("leaderloop: acquired lease",
			slog.String("key", lease.Key()),
			slog.String("holder", lease.Holder()),
			slog.Duration("lock_ttl", lockTTL),
		)
		l.setLeaseHeld(true)

		renewEvery := lockTTL / 3
		// Защита от panic в time.NewTicker при невменяемо коротком lockTTL
		// (<3ms). Дешёвая, закрывает класс ошибок.
		if renewEvery < time.Millisecond {
			renewEvery = time.Millisecond
		}
		lostCh := l.startRenewal(ctx, lease, renewEvery)
		viaLost := l.tickLoop(ctx, lostCh)
		// Любой выход из tickLoop — мы больше не лидер. Сбрасываем статус
		// немедленно, не дожидаясь Release-call-а.
		l.setLeaseHeld(false)

		// Cleanup. renewal-goroutine закрывает lostCh сама (на ctx.Done либо
		// ErrLeaseLost). Release делаем только если вышли НЕ через lostCh:
		// при ErrLeaseLost ключ заведомо уже не наш — Release был бы лишним
		// round-trip-ом с гарантированным CAS-0.
		if !viaLost {
			// WithoutCancel: сохраняем trace-baggage, не наследуем cancel
			// teardown-пути (Release должен пройти и при отменённом parent-ctx).
			relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			if relErr := lease.Release(relCtx); relErr != nil {
				l.cfg.Logger.Warn("leaderloop: lease release failed",
					slog.String("key", lease.Key()),
					slog.Any("error", relErr),
				)
			}
			relCancel()
		}

		if ctx.Err() != nil {
			return nil
		}
		l.cfg.Logger.Info("leaderloop: lease lost, re-acquire pending",
			slog.String("key", lease.Key()),
		)
	}
}

// acquireWithBackoff пытается захватить lease, ретраит на ErrLeaseTaken с
// постоянной паузой `acquireBackoff`. На отмену ctx возвращает (nil, 0, ctx.Err()).
//
// Возвращает также effective `lock_ttl` (через LockTTLFn) — caller использует
// его для renew-интервала. Чтение именно здесь, чтобы между re-acquire-итерациями
// hot-reload TTL подхватывался.
func (l *Loop) acquireWithBackoff(ctx context.Context) (*redis.Lease, time.Duration, error) {
	for {
		lockTTL := l.cfg.LockTTLFn()

		lease, err := redis.Acquire(ctx, l.cfg.Redis, l.cfg.LeaseKey, l.cfg.Holder, lockTTL)
		if err == nil {
			return lease, lockTTL, nil
		}
		if errors.Is(err, redis.ErrLeaseTaken) {
			// Нормальный сценарий: ждём backoff, пробуем снова.
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(l.acquireBackoff):
				continue
			}
		}
		// Сетевая/валидационная ошибка Acquire — логируем и тоже ждём.
		// Недоступность Redis не должна ронять процесс: фоновая задача
		// отрабатывает best-effort.
		l.cfg.Logger.Warn("leaderloop: acquire failed, will retry",
			slog.String("key", l.cfg.LeaseKey),
			slog.Duration("backoff", l.acquireBackoff),
			slog.Any("error", err),
		)
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(l.acquireBackoff):
		}
	}
}

// startRenewal поднимает goroutine, продлевающую TTL ключа каждые `renewEvery`.
// На ErrLeaseLost закрывает возвращаемый канал — tick-loop читает его, чтобы
// выйти gracefully. На ctx.Done — тоже закрывает (без различий: tick должен
// прекратиться вне зависимости от причины).
func (l *Loop) startRenewal(ctx context.Context, lease *redis.Lease, renewEvery time.Duration) <-chan struct{} {
	lost := make(chan struct{})
	go func() {
		defer close(lost)
		t := time.NewTicker(renewEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := lease.Renew(ctx); err != nil {
					if errors.Is(err, redis.ErrLeaseLost) {
						l.cfg.Logger.Warn("leaderloop: lease lost during renewal",
							slog.String("key", lease.Key()),
						)
						return
					}
					// Сетевая ошибка Renew — продолжаем (следующий tick попробует
					// снова). Если Redis-down достаточно долго, ключ истечёт и
					// Renew вернёт ErrLeaseLost на одном из последующих tick-ов.
					l.cfg.Logger.Warn("leaderloop: renew failed",
						slog.String("key", lease.Key()),
						slog.Any("error", err),
					)
				}
			}
		}
	}()
	return lost
}

// tickLoop крутит главный Ticker, пока ctx не отменится или lostCh не закроется
// (lease потерян). Первый Tick — сразу при acquire (не дожидаясь первого тика).
//
// Возвращает true, если вышли через lostCh (lease-loss), false на ctx.Done.
// Caller использует это, чтобы решить, делать ли Release.
func (l *Loop) tickLoop(ctx context.Context, lostCh <-chan struct{}) bool {
	// Hot-reload интервала: каждый тик перечитываем IntervalFn и пересоздаём
	// Ticker при изменении. На стабильном интервале Reset не вызывается.
	interval := l.cfg.IntervalFn()
	t := time.NewTicker(interval)
	defer t.Stop()

	// Первый прогон — сразу при acquire (smoke-видимость работы + «подмести
	// накопившееся сразу после рестарта/failover-а»).
	l.cfg.Tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return false
		case <-lostCh:
			return true
		case <-t.C:
			newInterval := l.cfg.IntervalFn()
			if newInterval != interval {
				interval = newInterval
				t.Reset(interval)
				l.cfg.Logger.Info("leaderloop: interval updated",
					slog.Duration("interval", interval),
				)
			}
			l.cfg.Tick(ctx)
		}
	}
}

// setLeaseHeld — nil-safe вызов OnLeaseChange.
func (l *Loop) setLeaseHeld(held bool) {
	if l.cfg.OnLeaseChange != nil {
		l.cfg.OnLeaseChange(held)
	}
}
