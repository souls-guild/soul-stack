package redis

// Cluster-wide инвалидация реестра Service-ов и keeper-settings через Redis
// pub/sub (S2 переноса реестра в Postgres, паттерн ADR-028(d) RBAC).
//
// Проблема: каждый Keeper-инстанс держит собственный in-memory снимок реестра
// service_registry + keeper_settings (`serviceregistry.Holder`), построенный из
// Postgres. CRUD-мутация (Insert/Update/Delete service / SetSetting) коммитится
// в общую БД на Keeper-A, но снимки остальных нод видят её только через
// TTL-poll (до `serviceregistry.DefaultRefreshInterval`). Окно устаревания —
// секунды.
//
// Решение симметрично [PublishRBACInvalidate] / [SubscribeRBACInvalidate]:
// мутирующая нода после успешного commit-а PUBLISH-ит в канал
// `service:invalidate`, остальные ноды по SUBSCRIBE near-instant перечитывают
// снимок из БД. Self-filter по `origin_kid` отсекает эхо собственной публикации
// — мутирующая нода уже зафиксировала изменение в БД и полагается на свой
// TTL-poll (тот же self-filter, что в RBAC; у Sigil его нет — там кеши живут на
// Soul-ах, здесь снимок локален Keeper-у).
//
// S2 = TTL-poll + pub/sub: TTL-poll НЕ убирается. Redis pub/sub не имеет
// persistence — если сообщение потеряно (реконнект ноды, мигание брокера), его
// подхватит следующий TTL-poll. Pub/sub лишь сокращает типичную задержку до
// миллисекунд, не заменяет fallback.
//
// Convention `service:invalidate` — отдельный namespace (стиль
// `<подсистема>:<событие>`, как `rbac:invalidate`/`sigil:invalidate`), не
// пересекается с `apply:<id>`/`outbound:<sid>`/`soul:<sid>:*`. Wire-формат —
// компактный JSON-envelope: `origin_kid` (self-filter) + `at` (диагностика).
// Payload-а нет — сообщение «снимок устарел, перечитай из БД».

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// ServiceInvalidateChannel — Redis-канал cluster-wide инвалидации реестра
// Service-ов. Фиксированный (не per-name): любая CRUD-мутация service/setting на
// любой ноде шлёт сюда.
const ServiceInvalidateChannel = "service:invalidate"

// serviceInvalidateEnvelope — JSON-обёртка одного invalidate-сообщения.
//
// `OriginKID` — KID Keeper-инстанса, опубликовавшего инвалидацию; подписчик
// отбрасывает сообщения с OriginKID == собственный KID (self-filter,
// симметрично [rbacInvalidateEnvelope.OriginKID]).
//
// `At` — момент публикации (диагностика); семантической нагрузки на
// rebuild-снимка не несёт.
type serviceInvalidateEnvelope struct {
	OriginKID string    `json:"origin_kid"`
	At        time.Time `json:"at"`
}

// ServiceInvalidate — распакованное invalidate-сообщение, доставляемое
// подписчику. Payload-а нет: само получение = сигнал «перечитай снимок реестра».
type ServiceInvalidate struct {
	OriginKID string
	At        time.Time
}

// PublishServiceInvalidate публикует invalidate-сигнал в
// [ServiceInvalidateChannel]. Возвращает количество подписчиков, получивших
// сообщение.
//
// Best-effort: caller (serviceregistry.Service после commit-а) глотает ошибку —
// мутация уже зафиксирована в БД, потеря publish-а не критична (TTL-poll
// подхватит).
func PublishServiceInvalidate(ctx context.Context, c *Client, originKID string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishServiceInvalidate: nil client")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishServiceInvalidate: empty originKID")
	}

	env, err := json.Marshal(serviceInvalidateEnvelope{
		OriginKID: originKID,
		At:        time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishServiceInvalidate: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, ServiceInvalidateChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishServiceInvalidate: PUBLISH %q: %w", ServiceInvalidateChannel, err)
	}
	return n, nil
}

// serviceInvalidateSubBufferSize — буфер Go-канала между Redis-PubSub-loop-ом и
// caller-ом (Holder). Инвалидации редки (CRUD-мутации реестра — десятки в день),
// небольшой запас отсекает drop при burst-е bulk-импорта. Совпадает с
// rbacInvalidateSubBufferSize — тот же класс редкого административного потока.
const serviceInvalidateSubBufferSize = 16

// ServiceInvalidateSubscription — handle на подписку [ServiceInvalidateChannel].
//
// Идентична по lifecycle [RBACInvalidateSubscription]: spawn goroutine читает
// PubSub-канал → парсит envelope → filter self-origin → отдаёт
// *ServiceInvalidate в Channel(). Close() прерывает goroutine и закрывает канал.
type ServiceInvalidateSubscription struct {
	ps        *redis.PubSub
	out       chan *ServiceInvalidate
	selfKID   string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel — read-side Go-канала с распакованными invalidate-сообщениями.
func (s *ServiceInvalidateSubscription) Channel() <-chan *ServiceInvalidate {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready блокируется до первого subscribe-acknowledgement от Redis (или
// ctx.Done() / Close()). См. doc-comment [RBACInvalidateSubscription.Ready].
func (s *ServiceInvalidateSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.ServiceInvalidateSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.ServiceInvalidateSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close прерывает subscribe-loop, закрывает Redis-pubsub-handle и Go-канал.
// Идемпотентна.
func (s *ServiceInvalidateSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeServiceInvalidate подписывается на [ServiceInvalidateChannel] и
// поднимает goroutine-forwarder. selfKID используется для self-фильтрации (как в
// [SubscribeRBACInvalidate]).
func SubscribeServiceInvalidate(ctx context.Context, c *Client, selfKID string, logger *slog.Logger) (*ServiceInvalidateSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeServiceInvalidate: nil client")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeServiceInvalidate: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeServiceInvalidate: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, ServiceInvalidateChannel)

	s := &ServiceInvalidateSubscription{
		ps:      ps,
		out:     make(chan *ServiceInvalidate, serviceInvalidateSubBufferSize),
		selfKID: selfKID,
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

func (s *ServiceInvalidateSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeServiceInvalidate: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeServiceInvalidate: ReceiveMessage failed",
				slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if ev.OriginKID == s.selfKID {
			// Self-echo: мутацию сделали мы сами. Не рефрешимся по своему
			// publish-у — собственный TTL-poll подхватит. Тот же self-filter,
			// что в SubscribeRBACInvalidate.
			s.logger.Debug("redis.SubscribeServiceInvalidate: ignoring self-origin invalidate",
				slog.String("origin_kid", ev.OriginKID))
			continue
		}

		select {
		case s.out <- ev:
		default:
			// Канал переполнен — подписчик (Holder) не успел перечитать снимок.
			// Drop безопасен: каждое сообщение — лишь «перечитай», последний
			// rebuild всё равно прочтёт актуальный БД-снимок целиком.
			s.logger.Warn("redis.SubscribeServiceInvalidate: forward channel full, dropping",
				slog.String("origin_kid", ev.OriginKID))
		}
	}
}

func (s *ServiceInvalidateSubscription) decodeMessage(payload string) (*ServiceInvalidate, bool) {
	var env serviceInvalidateEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeServiceInvalidate: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &ServiceInvalidate{
		OriginKID: env.OriginKID,
		At:        env.At,
	}, true
}
