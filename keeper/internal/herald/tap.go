package herald

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// DefaultTapBuffer — ёмкость bounded-буфера notification-tap-а. Tap получает
// КАЖДОЕ audit-событие keeper-а (горячий write-path); обработка (матч правил)
// вынесена в отдельную горутину, чтобы Observe не блокировал write-path.
// Буфер сглаживает всплески; при переполнении событие ДРОПается (drop-счётчик
// + warn) — tap best-effort по ADR-052(c), потеря уведомления при шторме
// приемлема, блокировка audit-записи — нет.
//
// 1024 — заметно больше типичного burst-а финализаций (батч Voyage-ов), но
// ограничивает память при затыке consumer-а (медленный PG-кэш правил).
const DefaultTapBuffer = 1024

// NotificationTap — keeper-side [audit.Tap]: на каждое успешно-записанное
// audit-событие неблокирующе кладёт его в bounded-канал; фоновая горутина
// разбирает канал и зовёт [Dispatcher.Dispatch]. ADR-052(c) tap-точка.
//
// Неблокируемость: Observe делает select-with-default — если буфер полон,
// событие дропается (drop-счётчик + debug-лог). Это изолирует audit write-path
// от любого затыка в матче/доставке.
//
// Shutdown через done-канал, НЕ через close(ch): ch никогда не закрывается,
// поэтому Observe (горячий write-path) не может попасть в send-after-close-панику
// при гонке с Close. consume в select слушает done-ветку — по сигналу дочитывает
// остаток буфера и выходит; Observe в select сначала проверяет done (после Close —
// no-op), затем неблокирующую постановку в ch.
type NotificationTap struct {
	ch         chan *audit.Event
	done       chan struct{}
	dispatcher *Dispatcher
	logger     *slog.Logger
	metrics    *DispatcherMetrics

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewNotificationTap собирает tap поверх dispatcher-а с буфером bufferSize
// (<=0 → [DefaultTapBuffer]) и запускает consumer-горутину. Возвращает tap
// (для NewMultiWriter) — его обязательно остановить через Close при shutdown.
func NewNotificationTap(dispatcher *Dispatcher, logger *slog.Logger, bufferSize int) *NotificationTap {
	if bufferSize <= 0 {
		bufferSize = DefaultTapBuffer
	}
	t := &NotificationTap{
		ch:         make(chan *audit.Event, bufferSize),
		done:       make(chan struct{}),
		dispatcher: dispatcher,
		logger:     logger,
	}
	t.wg.Add(1)
	go t.consume()
	return t
}

// SetMetrics late-binding метрик (drop-counter / dispatch). Прокидывается
// после metrics-registry (см. [Dispatcher.SetMetrics]). nil-safe.
func (t *NotificationTap) SetMetrics(m *DispatcherMetrics) {
	if t == nil {
		return
	}
	t.metrics = m
	t.dispatcher.SetMetrics(m)
}

// Observe — реализация [audit.Tap]. Неблокирующе ставит в буфер указатель из
// write-path (копия уже снята multi-writer-ом до tap-fan-out); consumer только
// читает, а dispatcher делает копию payload при постановке job-а, поэтому
// дополнительного копирования здесь не требуется. При полном буфере — drop.
//
// done-ветка идёт первой: после Close (или во время него) Observe — no-op, в ch
// ничего не кладётся. Поскольку ch не закрывается, send-after-close-паника
// невозможна by construction даже при гонке Observe ∥ Close.
func (t *NotificationTap) Observe(_ context.Context, event *audit.Event) {
	if t == nil || event == nil {
		return
	}
	select {
	case <-t.done:
		return
	default:
	}
	select {
	case <-t.done:
		return
	case t.ch <- event:
	default:
		t.metrics.observeDrop()
		// Drop логируем на debug, не warn: при шторме затыка warn сработал бы на
		// КАЖДОЕ событие (а не сэмплированно) и залил бы лог. Главный сигнал —
		// метрика keeper_herald_tap_dropped (counter с производной/алертом);
		// debug-строка нужна лишь для точечного разбора в дев-сборке.
		if t.logger != nil {
			t.logger.Debug("herald: notification tap buffer full, event dropped",
				slog.String("event_type", string(event.EventType)))
		}
	}
}

// dispatchEnqueueDeadline — потолок на один Dispatch (включая Enqueue в
// Redis-очередь). Dispatch отвязан от исходного write-ctx (тот мог отмениться),
// но с реальной RedisDeliveryQueue (S3) Enqueue делает сетевой LPUSH — без
// deadline зависший Redis подвесил бы consumer-горутину и Close на wg.Wait().
// Deadline короткий: Enqueue должен быть быстрым (LPUSH), затык → drop job-а
// (логируется dispatcher-ом как enqueue-ошибка), а не блокировка shutdown-а.
const dispatchEnqueueDeadline = 5 * time.Second

// consume разбирает буфер до сигнала done, синхронно вызывая Dispatch на каждом
// событии (одна consumer-горутина — матч правил сериализован, кэш правил под
// RWMutex). Каждый Dispatch — под ctx с [dispatchEnqueueDeadline] (S3): tap
// отвязан от исходного write-ctx, но deadline защищает от зависания в Enqueue на
// неотвечающем Redis-е (иначе Close завис бы на wg.Wait).
//
// По сигналу done дочитывает остаток буфера неблокирующим drain-ом и выходит:
// ch не закрывается (см. NotificationTap), поэтому остаток выгребается select-ом
// с default-выходом, а не range-ом по закрытому каналу.
func (t *NotificationTap) consume() {
	defer t.wg.Done()
	for {
		select {
		case <-t.done:
			// Дочитываем то, что уже легло в буфер до сигнала, и выходим.
			for {
				select {
				case event := <-t.ch:
					t.dispatch(event)
				default:
					return
				}
			}
		case event := <-t.ch:
			t.dispatch(event)
		}
	}
}

// dispatch вызывает Dispatcher под ctx с deadline на Enqueue (см.
// dispatchEnqueueDeadline). Отдельный ctx на каждое событие — отмена не утекает
// между событиями.
func (t *NotificationTap) dispatch(event *audit.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), dispatchEnqueueDeadline)
	defer cancel()
	t.dispatcher.Dispatch(ctx, event)
}

// Close сигналит consumer-у через done и дожидается слива остатка. Идемпотентен
// и безопасен при конкурентных вызовах: закрытие done под sync.Once (без него
// гонка double-close паникует), wg.Wait — вне Once (сам идемпотентен), поэтому
// все параллельные Close дождутся завершения consumer-а, а не вернутся раньше.
// Consumer по done дочитывает остаток буфера и выходит.
//
// Закрывается именно done, НЕ ch: Observe во время и после Close не паникует —
// канал событий не закрывается вообще, shutdown идёт через done-сигнал, поэтому
// конкурентный Observe ∥ Close безопасен by construction (см. NotificationTap).
// Вызывается при graceful-shutdown keeper-а (cleanup-стек).
func (t *NotificationTap) Close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(func() { close(t.done) })
	t.wg.Wait()
}
