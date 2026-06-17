package redis

// Cluster-wide инвалидация набора trust-anchor-ов подписи Sigil через Redis
// pub/sub (ADR-026(h), R3-S6).
//
// Проблема: набор trust-anchor-ов (sigil_signing_keys) меняется ротацией ключей
// подписи (Introduce / SetPrimary / Retire, S7). Мутация коммитится в общую
// таблицу на Keeper-A, но КАЖДАЯ нода держит собственный in-memory snapshot
// набора (sigil.Signer + keeper-host AnchorSet) и раздаёт его своим подключённым
// Soul-ам connect-time broadcast-ом. После ротации ноды должны: (1) перечитать
// набор из БД, (2) пересобрать Signer (новый primary + якоря), (3) обновить
// keeper-host verify-набор, (4) re-broadcast-ить свежий набор своим Soul-ам —
// иначе Soul на другой ноде проверяет подписи против устаревшего набора якорей
// (новый primary ещё «недоверенный»; retired-якорь ещё «доверенный»).
//
// Решение симметрично [PublishSigilInvalidate] / [SubscribeSigilInvalidate]:
// мутирующая нода после успешного commit-а ротации PUBLISH-ит в канал
// `sigil:anchors-changed`, КАЖДАЯ нода по SUBSCRIBE re-load-ит Signer/набор и
// re-broadcast-ит SigilTrustAnchors своим Soul-ам.
//
// Self-origin НЕ фильтруется (как у sigil:invalidate, в отличие от rbac:invalidate).
// Набор живёт на Soul-ах и в keeper-host-е КАЖДОЙ ноды; мутирующая нода обязана
// re-load-ить и re-broadcast-ить ровно так же, как чужая (её собственный
// in-memory Signer ещё хранит старый набор до re-load-а). Единый путь через
// pub/sub без отдельного in-process re-load после commit-а проще и без двойной
// обработки: нода получит собственное сообщение по подписке и обработает его.
// Поэтому envelope несёт только `at` (диагностика), без origin_kid.
//
// Persistence у Redis pub/sub нет: потеря сообщения (reconnect ноды, мигание
// брокера) оставляет ноду со старым набором до её следующего рестарта
// (load-at-start в setupSigil). Это окно осознанное — connect-time broadcast и
// fail-closed verify на Soul-е защищают целостность; pub/sub лишь сокращает
// окно рассинхрона набора до миллисекунд.
//
// Convention `sigil:anchors-changed` — отдельный namespace (стиль
// `<подсистема>:<событие>`, как `sigil:invalidate`), не пересекается с
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

// AnchorsChangedChannel — Redis-канал cluster-wide инвалидации набора
// trust-anchor-ов подписи Sigil. Фиксированный (не per-key): любая ротация
// ключей подписи на любой ноде шлёт сюда, каждая нода re-load-ит набор и
// re-broadcast-ит его всем своим Soul-ам.
const AnchorsChangedChannel = "sigil:anchors-changed"

// anchorsChangedEnvelope — JSON-обёртка одного anchors-changed-сообщения.
// Payload-а нет: само получение = сигнал «набор якорей изменился, re-load набор
// и re-broadcast своим Soul-ам». `At` — момент публикации (диагностика/лог).
type anchorsChangedEnvelope struct {
	At time.Time `json:"at"`
}

// AnchorsChanged — распакованное anchors-changed-сообщение, доставляемое
// подписчику. Без payload-а: получение = сигнал re-load + re-broadcast.
type AnchorsChanged struct {
	At time.Time
}

// PublishAnchorsChanged публикует anchors-changed-сигнал в [AnchorsChangedChannel].
// Возвращает количество подписчиков, получивших сообщение.
//
// Best-effort: caller (rotation-handler S7 после commit-а Introduce/SetPrimary/
// Retire) глотает ошибку — мутация уже зафиксирована в БД, потеря publish-а
// оставит ноды со старым набором до рестарта (осознанное окно, см. doc-comment
// файла).
func PublishAnchorsChanged(ctx context.Context, c *Client) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishAnchorsChanged: nil client")
	}

	env, err := json.Marshal(anchorsChangedEnvelope{At: time.Now().UTC()})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishAnchorsChanged: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, AnchorsChangedChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishAnchorsChanged: PUBLISH %q: %w", AnchorsChangedChannel, err)
	}
	return n, nil
}

// anchorsChangedSubBufferSize — буфер Go-канала между Redis-PubSub-loop-ом и
// caller-ом. Ротации ключей подписи редки (единицы за всю жизнь кластера),
// небольшой запас отсекает drop при burst-е (например, Introduce+SetPrimary
// подряд). Совпадает с sigilInvalidateSubBufferSize — тот же класс редкого
// административного потока.
const anchorsChangedSubBufferSize = 16

// AnchorsChangedSubscription — handle на подписку [AnchorsChangedChannel].
// Lifecycle идентичен [SigilInvalidateSubscription]: spawn goroutine читает
// PubSub-канал → парсит envelope → отдаёт *AnchorsChanged в Channel().
// Close() прерывает goroutine и закрывает канал. Идемпотентен.
type AnchorsChangedSubscription struct {
	ps        *redis.PubSub
	out       chan *AnchorsChanged
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel — read-side Go-канала с распакованными anchors-changed-сообщениями.
func (s *AnchorsChangedSubscription) Channel() <-chan *AnchorsChanged {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready блокируется до первого subscribe-acknowledgement от Redis (или
// ctx.Done() / Close()). См. doc-comment [SigilInvalidateSubscription.Ready].
func (s *AnchorsChangedSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.AnchorsChangedSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.AnchorsChangedSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close прерывает subscribe-loop, закрывает Redis-pubsub-handle и Go-канал.
// Идемпотентна.
func (s *AnchorsChangedSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeAnchorsChanged подписывается на [AnchorsChangedChannel] и поднимает
// goroutine-forwarder.
func SubscribeAnchorsChanged(ctx context.Context, c *Client, logger *slog.Logger) (*AnchorsChangedSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeAnchorsChanged: nil client")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeAnchorsChanged: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, AnchorsChangedChannel)

	s := &AnchorsChangedSubscription{
		ps:      ps,
		out:     make(chan *AnchorsChanged, anchorsChangedSubBufferSize),
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

func (s *AnchorsChangedSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeAnchorsChanged: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeAnchorsChanged: ReceiveMessage failed",
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
			// Канал переполнен — подписчик не успел re-load + re-broadcast.
			// Drop безопасен: каждое сообщение — лишь «re-load набор», последний
			// re-load всё равно прочтёт актуальный БД-набор целиком.
			s.logger.Warn("redis.SubscribeAnchorsChanged: forward channel full, dropping")
		}
	}
}

func (s *AnchorsChangedSubscription) decodeMessage(payload string) (*AnchorsChanged, bool) {
	var env anchorsChangedEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeAnchorsChanged: envelope unmarshal failed",
			slog.Any("error", err))
		return nil, false
	}
	return &AnchorsChanged{At: env.At}, true
}
