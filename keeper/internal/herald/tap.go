package herald

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// DefaultTapBuffer is the capacity of the notification tap's bounded buffer. Tap
// receives every Keeper audit event on the hot write path; processing (rule
// matching) is moved to a separate goroutine so Observe does not block the
// write-path. The buffer smooths bursts; when full, the event is dropped
// (drop counter + warn). Tap is best-effort per ADR-052(c): losing a notification
// during a storm is acceptable, blocking audit writes is not.
//
// 1024 is noticeably larger than a typical finalization burst (batch of Voyages)
// while bounding memory if the consumer stalls on a slow PG rule cache.
const DefaultTapBuffer = 1024

// NotificationTap is the Keeper-side [audit.Tap]: for every successfully written
// audit event, it non-blockingly puts the event into a bounded channel; a
// background goroutine consumes the channel and calls [Dispatcher.Dispatch].
// This is the ADR-052(c) tap point.
//
// Non-blocking behavior: Observe uses select-with-default. If the buffer is full,
// the event is dropped (drop counter + debug log). This isolates the audit
// write-path from any rule-match or delivery stall.
//
// Shutdown uses the done channel, not close(ch): ch is never closed, so Observe
// on the hot write-path cannot hit a send-after-close panic while racing Close.
// consume listens to the done branch, drains the remaining buffer on signal, and
// exits. Observe checks done first (after Close, no-op), then tries a non-blocking
// send to ch.
type NotificationTap struct {
	ch         chan *audit.Event
	done       chan struct{}
	dispatcher *Dispatcher
	logger     *slog.Logger
	metrics    *DispatcherMetrics

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewNotificationTap builds a tap on top of dispatcher with bufferSize
// (<=0 uses [DefaultTapBuffer]) and starts the consumer goroutine. It returns the
// tap for NewMultiWriter; callers must stop it through Close during shutdown.
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

// SetMetrics late-binds metrics (drop counter / dispatch). It is wired after the
// metrics registry, see [Dispatcher.SetMetrics]. nil-safe.
func (t *NotificationTap) SetMetrics(m *DispatcherMetrics) {
	if t == nil {
		return
	}
	t.metrics = m
	t.dispatcher.SetMetrics(m)
}

// Observe implements [audit.Tap]. It non-blockingly puts the write-path pointer
// into the buffer (multi-writer already copied it before tap fan-out); the
// consumer only reads, and dispatcher copies payload when enqueuing the job, so no
// extra copy is needed here. Full buffer means drop.
//
// The done branch comes first: after or during Close, Observe is no-op and puts
// nothing into ch. Because ch is never closed, send-after-close panic is
// impossible by construction even when Observe races Close.
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
		// Log drops at debug, not warn: during a stall storm, warn would fire for
		// every event and flood logs. The main signal is the
		// keeper_herald_tap_dropped metric (counter with derivative/alert); the
		// debug line is only for targeted investigation in a dev build.
		if t.logger != nil {
			t.logger.Debug("herald: notification tap buffer full, event dropped",
				slog.String("event_type", string(event.EventType)))
		}
	}
}

// dispatchEnqueueDeadline caps one Dispatch, including Enqueue into the Redis
// queue. Dispatch is detached from the original write ctx, which may have been
// cancelled, but with a real RedisDeliveryQueue (S3), Enqueue does network
// LPUSH. Without a deadline, a hung Redis would stall the consumer goroutine and
// Close on wg.Wait. Deadline is short: Enqueue should be quick (LPUSH); a stall
// drops the job (logged by dispatcher as an enqueue error) instead of blocking
// shutdown.
const dispatchEnqueueDeadline = 5 * time.Second

// consume drains the buffer until done is signalled, synchronously calling Dispatch
// for each event (one consumer goroutine: rule matching is serialized, rule cache
// is under RWMutex). Each Dispatch gets a ctx with [dispatchEnqueueDeadline] (S3):
// tap is detached from the original write ctx, but the deadline protects against a
// stuck Enqueue on an unresponsive Redis; otherwise Close would hang on wg.Wait.
//
// On done, it drains the remaining buffer non-blockingly and exits. ch is not
// closed (see NotificationTap), so the remainder is drained with select and a
// default exit, not range over a closed channel.
func (t *NotificationTap) consume() {
	defer t.wg.Done()
	for {
		select {
		case <-t.done:
			// Drain what was already buffered before the signal, then exit.
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

// dispatch calls Dispatcher under a ctx with an Enqueue deadline, see
// dispatchEnqueueDeadline. A separate ctx per event prevents cancellation from
// leaking between events.
func (t *NotificationTap) dispatch(event *audit.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), dispatchEnqueueDeadline)
	defer cancel()
	t.dispatcher.Dispatch(ctx, event)
}

// Close signals the consumer through done and waits for the remaining buffer to
// drain. It is idempotent and safe under concurrent calls: done is closed under
// sync.Once (without it, double-close races would panic), while wg.Wait is outside
// Once and is itself idempotent, so all parallel Close calls wait for the consumer
// to finish instead of returning early. On done, consumer drains the remaining
// buffer and exits.
//
// done is closed, not ch: Observe during and after Close does not panic. The event
// channel is never closed; shutdown goes through the done signal, so concurrent
// Observe and Close are safe by construction (see NotificationTap). Called during
// Keeper graceful shutdown by the cleanup stack.
func (t *NotificationTap) Close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(func() { close(t.done) })
	t.wg.Wait()
}
