package redis

// Cluster-wide push-providers инвалидация через Redis pub/sub (ADR-032
// amendment 2026-05-26, S7-2).
//
// Проблема: SshDispatcher держит spawned SshProvider-плагин с env-payload
// `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS`, наполненным при старте keeper-а
// из push_providers. Оператор делает POST/PUT/DELETE /v1/push-providers
// — мутация коммитится в общую таблицу на Keeper-A, но Keeper-B держит
// плагин со старым env-payload-ом и узнает только при следующем рестарте.
//
// Решение симметрично [PublishSigilInvalidate] / [SubscribeSigilInvalidate]:
// мутирующая нода после успешного commit-а Create/Update/Delete PUBLISH-ит
// в канал `push-providers:changed`, каждая нода по SUBSCRIBE помечает свой
// SshProvider-handle stale и пере-spawn-ит его на ближайшем RPC
// (spawn-on-change, PM-decision S7-2 #6).
//
// Persistence у Redis pub/sub нет: потеря сообщения (reconnect ноды,
// мигание брокера) → пере-spawn произойдёт только при следующем рестарте
// keeper-а либо при следующей мутации. Допустимо: мутации редкие
// (operator-driven), окно устаревания миллисекунды в штатной работе.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// PushProvidersChangedChannel — Redis pub/sub topic. Совпадает с
// pushprovider.TopicPushProvidersChanged (constant дублирован, чтобы redis-
// пакет не тянул pushprovider в импорты — direction-инвариант).
const PushProvidersChangedChannel = "push-providers:changed"

// pushProvidersChangedEnvelope — JSON-обёртка одного invalidate-сообщения.
// `Name` — имя изменённого provider-а (или пустая строка для массовой
// инвалидации); `At` — момент публикации (диагностика).
type pushProvidersChangedEnvelope struct {
	Name string    `json:"name,omitempty"`
	At   time.Time `json:"at"`
}

// PushProvidersChanged — распакованное invalidate-сообщение для подписчика.
type PushProvidersChanged struct {
	Name string
	At   time.Time
}

// PublishPushProvidersChanged публикует invalidate-сигнал.
// Возвращает количество подписчиков, получивших сообщение.
//
// Best-effort: caller (pushprovider.Service после commit-а CRUD-операции)
// глотает ошибку — мутация уже зафиксирована в БД, потеря publish-а
// компенсируется ленивым re-spawn-ом при следующей мутации/рестарте.
func PublishPushProvidersChanged(ctx context.Context, c *Client, providerName string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishPushProvidersChanged: nil client")
	}
	env, err := json.Marshal(pushProvidersChangedEnvelope{
		Name: providerName,
		At:   time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishPushProvidersChanged: envelope marshal: %w", err)
	}
	n, err := c.underlying().Publish(ctx, PushProvidersChangedChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishPushProvidersChanged: PUBLISH %q: %w",
			PushProvidersChangedChannel, err)
	}
	return n, nil
}

// pushProvidersChangedSubBufferSize — буфер Go-канала между PubSub-loop-ом и
// caller-ом. push-providers-мутации редки (десятки в день максимум), небольшой
// запас отсекает drop при burst-е bulk-операций. Совпадает с
// sigilInvalidateSubBufferSize.
const pushProvidersChangedSubBufferSize = 16

// PushProvidersChangedSubscription — handle на подписку.
// Lifecycle идентичен [SigilInvalidateSubscription].
type PushProvidersChangedSubscription struct {
	ps        *redis.PubSub
	out       chan *PushProvidersChanged
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel — read-side Go-канала с распакованными invalidate-сообщениями.
func (s *PushProvidersChangedSubscription) Channel() <-chan *PushProvidersChanged {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready блокируется до первого subscribe-acknowledgement от Redis.
func (s *PushProvidersChangedSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.PushProvidersChangedSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.PushProvidersChangedSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close прерывает subscribe-loop. Идемпотентен.
func (s *PushProvidersChangedSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribePushProvidersChanged подписывается на [PushProvidersChangedChannel].
func SubscribePushProvidersChanged(ctx context.Context, c *Client, logger *slog.Logger) (*PushProvidersChangedSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribePushProvidersChanged: nil client")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribePushProvidersChanged: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, PushProvidersChangedChannel)

	s := &PushProvidersChangedSubscription{
		ps:      ps,
		out:     make(chan *PushProvidersChanged, pushProvidersChangedSubBufferSize),
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

func (s *PushProvidersChangedSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribePushProvidersChanged: initial Receive failed",
			slog.Any("error", err))
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
			s.logger.Warn("redis.SubscribePushProvidersChanged: ReceiveMessage failed",
				slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}

		select {
		case s.out <- ev:
		default:
			// Канал переполнен — подписчик не успел перечитать. Drop
			// безопасен: каждое сообщение — лишь «re-spawn at next RPC»,
			// следующее перечитает актуальный набор params целиком.
			s.logger.Warn("redis.SubscribePushProvidersChanged: forward channel full, dropping")
		}
	}
}

func (s *PushProvidersChangedSubscription) decodeMessage(payload string) (*PushProvidersChanged, bool) {
	var env pushProvidersChangedEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribePushProvidersChanged: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &PushProvidersChanged{Name: env.Name, At: env.At}, true
}
