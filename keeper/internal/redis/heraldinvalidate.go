package redis

// Cluster-wide инвалидация снимка Tiding-правил dispatcher-а через Redis
// pub/sub (ADR-052, S4).
//
// Проблема: notification-dispatcher держит TTL-кэш ВКЛЮЧЁННЫХ Tiding-правил
// (DefaultRuleCacheTTL=15s). Оператор делает CRUD Herald/Tiding — мутация
// коммитится в общую таблицу на Keeper-A и in-process там сбрасывает кэш, но
// Keeper-B держит устаревший снимок до истечения TTL (новое правило не
// сработает / удалённое продолжит слать ≤15s).
//
// Решение симметрично [PublishPushProvidersChanged] / [PublishSigilInvalidate]:
// мутирующая нода после успешного commit-а CRUD PUBLISH-ит в канал
// `herald:invalidate`, каждая нода по SUBSCRIBE дёргает свой
// Dispatcher.InvalidateRules (следующий матч перечитает enabled-снимок из PG).
//
// Persistence у Redis pub/sub нет: потеря сообщения (reconnect ноды, мигание
// брокера) → сходимость наступит через TTL-poll (DefaultRuleCacheTTL).
// Допустимо: CRUD-мутации редкие (operator-driven), окно устаревания ≤15s.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// HeraldInvalidateChannel — Redis pub/sub topic. Совпадает с техническим
// именем канала в naming-rules (`herald:invalidate`, стиль
// `<подсистема>:<событие>` как `rbac:invalidate` / `push-providers:changed`).
const HeraldInvalidateChannel = "herald:invalidate"

// heraldInvalidateEnvelope — JSON-обёртка одного invalidate-сообщения. `Name` —
// имя изменённого Herald/Tiding (диагностика; инвалидация всегда полная — снимок
// перечитывается целиком, имя не таргетирует сброс); `At` — момент публикации.
type heraldInvalidateEnvelope struct {
	Name string    `json:"name,omitempty"`
	At   time.Time `json:"at"`
}

// HeraldInvalidate — распакованное invalidate-сообщение для подписчика.
type HeraldInvalidate struct {
	Name string
	At   time.Time
}

// PublishHeraldInvalidate публикует invalidate-сигнал. Возвращает количество
// подписчиков, получивших сообщение.
//
// Best-effort: caller (herald.Service после commit-а CRUD-операции) глотает
// ошибку — мутация уже зафиксирована в БД, потеря publish-а компенсируется
// TTL-сходимостью dispatcher-кэша (DefaultRuleCacheTTL).
func PublishHeraldInvalidate(ctx context.Context, c *Client, name string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishHeraldInvalidate: nil client")
	}
	env, err := json.Marshal(heraldInvalidateEnvelope{Name: name, At: time.Now().UTC()})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishHeraldInvalidate: envelope marshal: %w", err)
	}
	n, err := c.underlying().Publish(ctx, HeraldInvalidateChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishHeraldInvalidate: PUBLISH %q: %w", HeraldInvalidateChannel, err)
	}
	return n, nil
}

// heraldInvalidateSubBufferSize — буфер Go-канала между PubSub-loop-ом и
// caller-ом. CRUD-мутации Herald/Tiding редки; небольшой запас отсекает drop
// при burst-е. Совпадает с pushProvidersChangedSubBufferSize.
const heraldInvalidateSubBufferSize = 16

// HeraldInvalidateSubscription — handle на подписку. Lifecycle идентичен
// [PushProvidersChangedSubscription].
type HeraldInvalidateSubscription struct {
	ps        *redis.PubSub
	out       chan *HeraldInvalidate
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel — read-side Go-канала с распакованными invalidate-сообщениями.
func (s *HeraldInvalidateSubscription) Channel() <-chan *HeraldInvalidate {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready блокируется до первого subscribe-acknowledgement от Redis.
func (s *HeraldInvalidateSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.HeraldInvalidateSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.HeraldInvalidateSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close прерывает subscribe-loop. Идемпотентен.
func (s *HeraldInvalidateSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeHeraldInvalidate подписывается на [HeraldInvalidateChannel].
func SubscribeHeraldInvalidate(ctx context.Context, c *Client, logger *slog.Logger) (*HeraldInvalidateSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeHeraldInvalidate: nil client")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeHeraldInvalidate: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, HeraldInvalidateChannel)

	s := &HeraldInvalidateSubscription{
		ps:      ps,
		out:     make(chan *HeraldInvalidate, heraldInvalidateSubBufferSize),
		logger:  logger,
		ready:   make(chan struct{}),
		stopped: make(chan struct{}),
	}

	closed := make(chan struct{})
	s.closeOnce = func() error {
		select {
		case <-closed:
			return nil
		default:
		}
		close(closed)
		err := ps.Close()
		<-s.stopped
		return err
	}

	go s.run(ctx, closed)
	return s, nil
}

func (s *HeraldInvalidateSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeHeraldInvalidate: initial Receive failed", slog.Any("error", err))
		return
	}
	close(s.ready)

	for {
		select {
		case <-closed:
			return
		case <-ctx.Done():
			return
		default:
		}

		msg, err := s.ps.ReceiveMessage(ctx)
		if err != nil {
			select {
			case <-closed:
				return
			case <-ctx.Done():
				return
			default:
			}
			s.logger.Warn("redis.SubscribeHeraldInvalidate: ReceiveMessage failed", slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}

		select {
		case s.out <- ev:
		default:
			// Канал переполнен — подписчик не успел перечитать. Drop безопасен:
			// каждое сообщение — лишь «InvalidateRules», следующее перечитает
			// актуальный снимок целиком.
			s.logger.Warn("redis.SubscribeHeraldInvalidate: forward channel full, dropping")
		}
	}
}

func (s *HeraldInvalidateSubscription) decodeMessage(payload string) (*HeraldInvalidate, bool) {
	var env heraldInvalidateEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeHeraldInvalidate: envelope unmarshal failed", slog.Any("error", err))
		return nil, false
	}
	return &HeraldInvalidate{Name: env.Name, At: env.At}, true
}
