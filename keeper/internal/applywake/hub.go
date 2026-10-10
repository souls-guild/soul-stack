// Package applywake wakes a scenario run's barrier the moment a row of the run
// moves, instead of at its next poll of `apply_runs` (NIM-984).
//
// Under linear dispatch a run waits at a barrier after every step, so the poll
// period is paid per step: the linear-dispatch benchmark (NIM-950) found ~190 of the
// ~200 ms between two steps spent waiting for the barrier's tick. A wake makes the
// barrier re-read its rows now. It carries no status — Postgres stays the only
// truth — and a lost wake costs one fallback poll, never a wrong answer.
//
// A writer of a row a barrier waits on calls [Hub.Wake] once the write has
// committed. The barrier may run on another Keeper instance, so the wake also
// goes out on [keeperredis.ApplyWakeChannel], and [Hub.Listen] hands what other
// instances publish to the waiters here.
package applywake

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// publishTimeout bounds one PUBLISH. The writer never waits for it — the publish
// runs in its own goroutine — so this only keeps a hung Redis from piling them up.
const publishTimeout = time.Second

// resubscribeDelay paces [Hub.Listen]'s attempts after the subscription dies.
const resubscribeDelay = time.Second

// Hub routes wakes to the barriers waiting on this instance. A nil *Hub is
// usable and wakes nobody.
type Hub struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}

	redis  *keeperredis.Client
	kid    string
	logger *slog.Logger

	// publishFailing makes a Redis outage one log line, not one per wake.
	publishFailing atomic.Bool
}

// New builds a hub. With a nil redis or an empty kid it wakes this instance
// only, which is all a single Keeper needs: it writes every row itself.
func New(redis *keeperredis.Client, kid string, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	h := &Hub{waiters: map[string]map[chan struct{}]struct{}{}, logger: logger}
	if redis != nil && kid != "" {
		h.redis, h.kid = redis, kid
	}
	return h
}

// Watch registers a waiter for applyID and returns its channel and the function
// that removes it. The channel keeps one pending wake, so a wake that lands
// between the caller's read of the rows and its next wait is not lost — provided
// Watch is called before the first read. A nil hub returns a nil channel, which
// never fires: the caller is left with its poll.
func (h *Hub) Watch(applyID string) (<-chan struct{}, func()) {
	if h == nil {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.waiters[applyID] == nil {
		h.waiters[applyID] = map[chan struct{}]struct{}{}
	}
	h.waiters[applyID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.waiters[applyID], ch)
		if len(h.waiters[applyID]) == 0 {
			delete(h.waiters, applyID)
		}
		h.mu.Unlock()
	}
}

// Wake tells every barrier of applyID that a row of the run moved: on this
// instance at once, on the others through Redis without waiting for the publish.
// Call it after the write has committed — a barrier woken before re-reads the
// old row and then waits a full fallback poll.
func (h *Hub) Wake(applyID string) {
	if h == nil || applyID == "" {
		return
	}
	h.deliver(applyID)
	if h.redis == nil {
		return
	}
	go h.publish(applyID)
}

func (h *Hub) publish(applyID string) {
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if err := keeperredis.PublishApplyWake(ctx, h.redis, applyID, h.kid); err != nil {
		if !h.publishFailing.Swap(true) {
			h.logger.Warn("applywake: publishing barrier wakes failed; a barrier on another Keeper instance sees rows written here only at its fallback poll",
				slog.Any("error", err))
		}
		return
	}
	if h.publishFailing.Swap(false) {
		h.logger.Info("applywake: publishing barrier wakes recovered")
	}
}

func (h *Hub) deliver(applyID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.waiters[applyID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Listen hands the wakes other instances publish to this instance's waiters
// until ctx ends, subscribing again after a failure. Pub/sub has no replay: a
// wake published while the subscription is down is lost, and its barrier sees
// the row at its fallback poll. A hub without Redis returns at once.
func (h *Hub) Listen(ctx context.Context) {
	if h == nil || h.redis == nil {
		return
	}
	interrupted := false
	for ctx.Err() == nil {
		h.listenOnce(ctx, &interrupted)
		select {
		case <-ctx.Done():
			return
		case <-time.After(resubscribeDelay):
		}
	}
}

func (h *Hub) listenOnce(ctx context.Context, interrupted *bool) {
	fail := func(err error) {
		if *interrupted {
			return
		}
		*interrupted = true
		h.logger.Warn("applywake: barrier wake subscription down; barriers here see rows written on other Keeper instances only at their fallback poll",
			slog.Any("error", err))
	}
	sub, err := keeperredis.SubscribeApplyWake(ctx, h.redis, h.kid, h.logger)
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = sub.Close() }()
	if err := sub.Ready(ctx); err != nil {
		if ctx.Err() == nil {
			fail(err)
		}
		return
	}
	if *interrupted {
		*interrupted = false
		h.logger.Info("applywake: barrier wake subscription re-established")
	}
	for {
		select {
		case <-ctx.Done():
			return
		case applyID, ok := <-sub.Channel():
			if !ok {
				fail(errors.New("subscription closed"))
				return
			}
			h.deliver(applyID)
		}
	}
}
