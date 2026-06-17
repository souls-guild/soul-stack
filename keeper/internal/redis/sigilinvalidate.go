package redis

// Cluster-wide Sigil-инвалидация через Redis pub/sub (ADR-026, S6c).
//
// Проблема: Soul держит in-memory кеш допусков плагинов (PluginSigil),
// наполняемый connect-time broadcast-ом от того Keeper-инстанса, к которому
// подключён стрим. Оператор делает plugin.allow / plugin.revoke — мутация
// коммитится в общую таблицу plugin_sigils на Keeper-A, но Soul может висеть
// на Keeper-B и о новом allow-list-е ничего не узнает: кеш остаётся
// устаревшим (revoked-допуск ещё «доверенный», новый allow ещё не доехал).
//
// Решение симметрично [PublishRBACInvalidate] / [SubscribeRBACInvalidate]:
// мутирующая нода после успешного commit-а Allow/Revoke PUBLISH-ит в канал
// `sigil:invalidate`, КАЖДАЯ нода по SUBSCRIBE перечитывает active-набор из БД
// и re-broadcast-ит его своим подключённым Soul-ам (через тот же путь, что и
// connect-time broadcast). Так Soul на Keeper-B получает свежий набор после
// мутации на Keeper-A.
//
// Отличие от RBAC: здесь self-origin НЕ фильтруется. RBAC-снимок мутирующей
// ноды обновляется её собственным in-process commit-ом, поэтому ноде не нужно
// реагировать на свой publish. Sigil-кеши живут на Soul-ах, а не на Keeper-е;
// мутирующая нода обязана re-broadcast-ить своим Soul-ам ровно так же, как
// чужая. Отдельного in-process re-broadcast после commit-а нет — единый путь
// через pub/sub проще и без двойной раздачи: нода получит собственное
// сообщение по подписке и раздаст его. Поэтому envelope несёт только `at`
// (диагностика), без origin_kid.
//
// Persistence у Redis pub/sub нет: потеря сообщения (reconnect ноды, мигание
// брокера) → допуск доедет на следующем reconnect Soul-а (connect-time
// broadcast). Pub/sub лишь сокращает окно устаревания до миллисекунд, не
// заменяет connect-time-fallback.
//
// Convention `sigil:invalidate` — отдельный namespace (стиль
// `<подсистема>:<событие>`, как `rbac:invalidate`), не пересекается с
// `apply:<id>`/`outbound:<sid>`/`soul:<sid>:*`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// SigilInvalidateChannel — Redis-канал cluster-wide Sigil-инвалидации.
// Фиксированный (не per-SID): любая allow/revoke-мутация на любой ноде шлёт
// сюда, каждая нода re-broadcast-ит active-набор всем своим Soul-ам.
const SigilInvalidateChannel = "sigil:invalidate"

// sigilInvalidateEnvelope — JSON-обёртка одного invalidate-сообщения. Payload-а
// нет: само получение = сигнал «allow-list изменился, re-broadcast active-набор
// своим Soul-ам». `At` — момент публикации (диагностика/лог).
type sigilInvalidateEnvelope struct {
	At time.Time `json:"at"`
}

// SigilInvalidate — распакованное invalidate-сообщение, доставляемое
// подписчику. Без payload-а: получение = сигнал re-broadcast-а.
type SigilInvalidate struct {
	At time.Time
}

// PublishSigilInvalidate публикует invalidate-сигнал в [SigilInvalidateChannel].
// Возвращает количество подписчиков, получивших сообщение.
//
// Best-effort: caller (sigil.Service после commit-а Allow/Revoke) глотает
// ошибку — мутация уже зафиксирована в БД, потеря publish-а компенсируется
// connect-time broadcast-ом при следующем reconnect Soul-а.
func PublishSigilInvalidate(ctx context.Context, c *Client) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishSigilInvalidate: nil client")
	}

	env, err := json.Marshal(sigilInvalidateEnvelope{At: time.Now().UTC()})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSigilInvalidate: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, SigilInvalidateChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSigilInvalidate: PUBLISH %q: %w", SigilInvalidateChannel, err)
	}
	return n, nil
}

// sigilInvalidateSubBufferSize — буфер Go-канала между Redis-PubSub-loop-ом и
// caller-ом. Allow/revoke-мутации редки (десятки в день), небольшой запас
// отсекает drop при burst-е bulk-allow-ов. Совпадает с
// rbacInvalidateSubBufferSize — тот же класс редкого административного потока.
const sigilInvalidateSubBufferSize = 16

// SigilInvalidateSubscription — handle на подписку [SigilInvalidateChannel].
// Lifecycle идентичен [RBACInvalidateSubscription]: spawn goroutine читает
// PubSub-канал → парсит envelope → отдаёт *SigilInvalidate в Channel().
// Close() прерывает goroutine и закрывает канал. Идемпотентен.
type SigilInvalidateSubscription struct {
	ps        *redis.PubSub
	out       chan *SigilInvalidate
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel — read-side Go-канала с распакованными invalidate-сообщениями.
func (s *SigilInvalidateSubscription) Channel() <-chan *SigilInvalidate {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready блокируется до первого subscribe-acknowledgement от Redis (или
// ctx.Done() / Close()). См. doc-comment [RBACInvalidateSubscription.Ready].
func (s *SigilInvalidateSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.SigilInvalidateSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.SigilInvalidateSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close прерывает subscribe-loop, закрывает Redis-pubsub-handle и Go-канал.
// Идемпотентна.
func (s *SigilInvalidateSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeSigilInvalidate подписывается на [SigilInvalidateChannel] и
// поднимает goroutine-forwarder.
func SubscribeSigilInvalidate(ctx context.Context, c *Client, logger *slog.Logger) (*SigilInvalidateSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeSigilInvalidate: nil client")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeSigilInvalidate: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, SigilInvalidateChannel)

	s := &SigilInvalidateSubscription{
		ps:      ps,
		out:     make(chan *SigilInvalidate, sigilInvalidateSubBufferSize),
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

func (s *SigilInvalidateSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeSigilInvalidate: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeSigilInvalidate: ReceiveMessage failed",
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
			// Канал переполнен — подписчик не успел re-broadcast-ить.
			// Drop безопасен: каждое сообщение — лишь «re-broadcast active-набор»,
			// последний re-broadcast всё равно прочтёт актуальный БД-набор целиком.
			s.logger.Warn("redis.SubscribeSigilInvalidate: forward channel full, dropping")
		}
	}
}

func (s *SigilInvalidateSubscription) decodeMessage(payload string) (*SigilInvalidate, bool) {
	var env sigilInvalidateEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeSigilInvalidate: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &SigilInvalidate{At: env.At}, true
}
