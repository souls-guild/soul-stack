package redis

// Summons — Redis pub/sub-сигнал «появились planned-задания» (ADR-027(a)).
//
// Проблема: scenario-runner на dispatch-е пишет строки `apply_runs` в статус
// `planned` (slice 1.4 cutover) — но на каком Keeper-инстансе они появились,
// заранее неизвестно. Acolyte-пулы всех инстансов и так периодически сканируют
// `planned` (poll-tick, см. acolyte/pool.go), однако poll-интервал — это
// задержка до подхвата. Summons сокращает её до миллисекунд: записавшая
// `planned`-строку нода PUBLISH-ит сигнал, любой Acolyte по SUBSCRIBE
// просыпается и сразу клеймит.
//
// Отличие от [PublishApplyEvent] / [PublishRBACInvalidate]: self-filter здесь
// НЕ нужен. planned-задание, записанное на инициаторе, должен подхватить любой
// Acolyte любой ноды — включая саму инициирующую (её пул такой же кандидат на
// claim). Поэтому envelope несёт `origin_kid` только для диагностики/логов, а
// подписчик НЕ отбрасывает собственное эхо.
//
// Best-effort + poll-fallback: Redis pub/sub не имеет persistence. Потерянный
// сигнал (нода реконнектится, брокер мигнул, ни один Acolyte ещё не
// subscribe-нулся) не теряет задание — его подхватит ближайший poll-tick
// (acolyte/pool.go::defaultPollInterval). Summons лишь УСКОРЯЕТ пробуждение.
//
// Convention `apply:summons` (зафиксировано, propose-and-wait пройден,
// naming-rules.md) — стиль `<подсистема>:<событие>`, как `rbac:invalidate`.
// Фиксированный канал (не per-ID): любое появление planned на любой ноде шлёт
// сюда; payload-а нет — сигнал «проверь очередь», конкретику Acolyte берёт из
// Postgres сам (claim через `FOR UPDATE SKIP LOCKED`).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// SummonsChannel — Redis-канал Summons-сигнала planned-заданий.
// Фиксированный (не per-ID): любая planned-запись на любой ноде шлёт сюда.
const SummonsChannel = "apply:summons"

// summonsEnvelope — JSON-обёртка одного Summons-сигнала.
//
// `OriginKID` — KID Keeper-инстанса, опубликовавшего сигнал; в отличие от
// applybus/rbacinvalidate, для self-filter НЕ используется (см. doc-comment
// файла) — только диагностика. `At` — момент публикации (лог/диагностика).
type summonsEnvelope struct {
	OriginKID string    `json:"origin_kid"`
	At        time.Time `json:"at"`
}

// PublishSummons публикует Summons-сигнал в [SummonsChannel]. Возвращает
// количество подписчиков, получивших сообщение.
//
// Best-effort: caller (scenario-runner после записи planned-строки, slice 1.4)
// глотает ошибку — задание уже персистентно в `apply_runs`, потеря publish-а
// не критична (poll-fallback Acolyte подхватит). Call-site публикации в этом
// слайсе НЕ вводится: здесь только готовый механизм.
func PublishSummons(ctx context.Context, c *Client, originKID string) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishSummons: nil client")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishSummons: empty originKID")
	}

	env, err := json.Marshal(summonsEnvelope{
		OriginKID: originKID,
		At:        time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSummons: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, SummonsChannel, env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishSummons: PUBLISH %q: %w", SummonsChannel, err)
	}
	return n, nil
}

// SummonsSubscription — handle на подписку [SummonsChannel].
//
// Lifecycle как у [RBACInvalidateSubscription], но без read-side Go-канала:
// Summons не несёт payload, подписчику нужно лишь «проснуться». Поэтому на
// каждый валидный сигнал goroutine дёргает callback `onSignal` (для Acolyte —
// pool.Notify). Close() прерывает goroutine и закрывает Redis-pubsub-handle.
type SummonsSubscription struct {
	ps        *redis.PubSub
	onSignal  func()
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Ready блокируется до первого subscribe-acknowledgement от Redis (или
// ctx.Done() / Close()). См. doc-comment [RBACInvalidateSubscription.Ready].
func (s *SummonsSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.SummonsSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.SummonsSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close прерывает subscribe-loop и закрывает Redis-pubsub-handle. Идемпотентна.
func (s *SummonsSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeSummons подписывается на [SummonsChannel] и поднимает
// goroutine-forwarder: на каждый валидный сигнал вызывает onSignal. Self-filter
// нет (см. doc-comment файла) — origin_kid игнорируется при доставке.
func SubscribeSummons(ctx context.Context, c *Client, onSignal func(), logger *slog.Logger) (*SummonsSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeSummons: nil client")
	}
	if onSignal == nil {
		return nil, errors.New("redis.SubscribeSummons: nil onSignal")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeSummons: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, SummonsChannel)

	s := &SummonsSubscription{
		ps:       ps,
		onSignal: onSignal,
		logger:   logger,
		ready:    make(chan struct{}),
		stopped:  make(chan struct{}),
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

func (s *SummonsSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeSummons: initial Receive failed",
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
			s.logger.Warn("redis.SubscribeSummons: ReceiveMessage failed",
				slog.Any("error", err))
			return
		}

		// Envelope парсим только ради валидации/диагностики origin_kid; payload
		// семантики не несёт. Битый envelope не должен глушить wake — лучше
		// лишний раз проверить очередь, чем пропустить planned-задание.
		if env, ok := s.decodeMessage(msg.Payload); ok {
			s.logger.Debug("redis.SubscribeSummons: summons received",
				slog.String("origin_kid", env.OriginKID))
		}
		s.onSignal()
	}
}

func (s *SummonsSubscription) decodeMessage(payload string) (summonsEnvelope, bool) {
	var env summonsEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeSummons: envelope unmarshal failed",
			slog.Any("error", err))
		return summonsEnvelope{}, false
	}
	return env, true
}
