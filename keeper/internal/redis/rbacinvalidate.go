package redis

// Cluster-wide RBAC-инвалидация через Redis pub/sub (ADR-028(d), Фаза 3 = B2).
//
// Проблема: каждый Keeper-инстанс держит собственный RBAC-снимок
// (`rbac.Holder`/`rbac.Enforcer`), построенный из Postgres. Мутация роли
// (CreateRole/DeleteRole/UpdateRolePermissions/GrantOperator/RevokeOperator)
// коммитится в общую БД на Keeper-A, но снимки остальных нод видят её только
// через TTL-poll (B1, до `rbac.DefaultRefreshInterval`). Окно устаревания —
// секунды.
//
// Решение симметрично apply-events-routing-у (см. [PublishApplyEvent] /
// [SubscribeApplyEvent]): мутирующая нода после успешного commit-а PUBLISH-ит
// в канал `rbac:invalidate`, остальные ноды по SUBSCRIBE перечитывают снимок
// из БД (near-instant). Self-filter по `origin_kid` отсекает эхо собственной
// публикации — мутирующая нода полагается на свой TTL-poll (рефрешить снимок
// прямо в момент publish здесь не нужно, образец applybus так же отбрасывает
// self-origin).
//
// B2 = B1 + pub/sub: TTL-poll НЕ убирается. Redis pub/sub не имеет
// persistence — если сообщение потеряно (нода реконнектится, брокер мигнул),
// его подхватит следующий TTL-poll. Pub/sub лишь сокращает типичную задержку
// до миллисекунд, а не заменяет fallback.
//
// Convention `rbac:invalidate` — отдельный namespace (PM-decision,
// подтверждённый topic), не пересекается с `apply:<id>`/`outbound:<sid>`/
// `soul:<sid>:*`. Wire-формат — компактный JSON-envelope: только
// `origin_kid` (для self-filter) + `at` (диагностика/лог). Payload-а нет —
// сообщение «снимок устарел, перечитай из БД», конкретику подписчик берёт
// из Postgres сам.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// RBACInvalidateChannel — Redis-канал cluster-wide RBAC-инвалидации.
// Фиксированный (не per-ID): любая role-мутация на любой ноде шлёт сюда.
const RBACInvalidateChannel = "rbac:invalidate"

// rbacInvalidateEnvelope — JSON-обёртка одного invalidate-сообщения.
//
// `OriginKID` — KID Keeper-инстанса, опубликовавшего инвалидацию; подписчик
// отбрасывает сообщения с OriginKID == собственный KID (self-filter,
// симметрично [applyEventEnvelope.OriginKID]).
//
// `At` — момент публикации (диагностика); семантической нагрузки на
// rebuild-снимка не несёт.
type rbacInvalidateEnvelope struct {
	OriginKID string    `json:"origin_kid"`
	At        time.Time `json:"at"`
}

// RBACInvalidate — распакованное invalidate-сообщение, доставляемое
// подписчику. Payload-а нет: само получение = сигнал «перечитай RBAC-снимок».
type RBACInvalidate struct {
	OriginKID string
	At        time.Time
}

// PublishRBACInvalidate публикует invalidate-сигнал в [RBACInvalidateChannel].
// Возвращает количество подписчиков, получивших сообщение.
//
// Best-effort: caller (rbac.Service после commit-а) глотает ошибку — мутация
// уже зафиксирована в БД, потеря publish-а не критична (TTL-poll подхватит).
func PublishRBACInvalidate(ctx context.Context, c *Client, originKID string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishRBACInvalidate: nil client")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishRBACInvalidate: empty originKID")
	}

	env, err := json.Marshal(rbacInvalidateEnvelope{
		OriginKID: originKID,
		At:        time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishRBACInvalidate: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, RBACInvalidateChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishRBACInvalidate: PUBLISH %q: %w", RBACInvalidateChannel, err)
	}
	return n, nil
}

// rbacInvalidateSubBufferSize — буфер Go-канала между Redis-PubSub-loop-ом и
// caller-ом (Holder). Инвалидации редки (role-мутации — десятки в день), но
// небольшой запас отсекает drop при burst-е bulk-grant-ов. Меньше
// applyEventSubBufferSize — поток на порядки реже.
const rbacInvalidateSubBufferSize = 16

// RBACInvalidateSubscription — handle на подписку [RBACInvalidateChannel].
//
// Идентична по lifecycle [ApplyEventSubscription]: spawn goroutine читает
// PubSub-канал → парсит envelope → filter self-origin → отдаёт
// *RBACInvalidate в Channel(). Close() прерывает goroutine и закрывает канал.
type RBACInvalidateSubscription struct {
	ps        *redis.PubSub
	out       chan *RBACInvalidate
	selfKID   string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel — read-side Go-канала с распакованными invalidate-сообщениями.
func (s *RBACInvalidateSubscription) Channel() <-chan *RBACInvalidate {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready блокируется до первого subscribe-acknowledgement от Redis (или
// ctx.Done() / Close()). См. doc-comment [ApplyEventSubscription.Ready].
func (s *RBACInvalidateSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.RBACInvalidateSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.RBACInvalidateSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close прерывает subscribe-loop, закрывает Redis-pubsub-handle и Go-канал.
// Идемпотентна.
func (s *RBACInvalidateSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeRBACInvalidate подписывается на [RBACInvalidateChannel] и поднимает
// goroutine-forwarder. selfKID используется для self-фильтрации (как в
// [SubscribeApplyEvent]).
func SubscribeRBACInvalidate(ctx context.Context, c *Client, selfKID string, logger *slog.Logger) (*RBACInvalidateSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeRBACInvalidate: nil client")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeRBACInvalidate: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeRBACInvalidate: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, RBACInvalidateChannel)

	s := &RBACInvalidateSubscription{
		ps:      ps,
		out:     make(chan *RBACInvalidate, rbacInvalidateSubBufferSize),
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

func (s *RBACInvalidateSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeRBACInvalidate: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeRBACInvalidate: ReceiveMessage failed",
				slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if ev.OriginKID == s.selfKID {
			// Self-echo: мутацию сделали мы сами. Не рефрешимся по своему
			// publish-у — собственный TTL-poll подхватит (B1-fallback). Тот же
			// self-filter, что в SubscribeApplyEvent.
			s.logger.Debug("redis.SubscribeRBACInvalidate: ignoring self-origin invalidate",
				slog.String("origin_kid", ev.OriginKID))
			continue
		}

		select {
		case s.out <- ev:
		default:
			// Канал переполнен — подписчик (Holder) не успел перечитать снимок.
			// Drop безопасен: каждое сообщение — лишь «перечитай», последний
			// Refresh всё равно прочтёт актуальный БД-снимок целиком.
			s.logger.Warn("redis.SubscribeRBACInvalidate: forward channel full, dropping",
				slog.String("origin_kid", ev.OriginKID))
		}
	}
}

func (s *RBACInvalidateSubscription) decodeMessage(payload string) (*RBACInvalidate, bool) {
	var env rbacInvalidateEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeRBACInvalidate: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &RBACInvalidate{
		OriginKID: env.OriginKID,
		At:        env.At,
	}, true
}
