package redis

// Cluster-mode SSE-routing apply-событий через Redis pub/sub (ADR-006(c)).
//
// Проблема: in-memory [applybus.EventBus] (M0.7.c) живёт внутри одного
// Keeper-инстанса. Если SSE-подписчик пришёл на Keeper-A
// (`GET /mcp/events?apply_id=X` повисел там), а Soul, который льёт
// TaskEvent/RunResult по EventStream-у, подключился к Keeper-B —
// publisher на Keeper-B запишет события только в свой local-bus, и
// SSE-клиент на Keeper-A никогда их не увидит.
//
// Решение симметрично outbound-routing-у (см. [PublishOutbound] /
// [SubscribeOutbound]): publisher дополнительно PUBLISH-ит в шардированный
// Redis-канал `events:shard:<n>`, все Keeper-инстансы подписаны на нужные
// шарды и форвардят события в свои local-bus-ы. Self-filter по `origin_kid`
// отсекает эхо собственных публикаций.
//
// Шардирование (ADR-006(c) amendment, S2 applybus-bottleneck): per-applyID
// канал `apply:<apply_id>` давал на больших флотах столько же Redis-подписок,
// сколько одновременных прогонов, и упирался в `maxclients`. Канал сведён к
// фиксированному множеству из [ApplyBusShardCount] шардов; applyID
// детерминированно отображается в шард через [shardIndex] (fnv32a % K).
// Несколько applyID шарят одну shard-подписку; forward-loop на стороне
// applybus отсеивает чужие события по `envelope.ApplyID` (см.
// `keeper/internal/applybus/bus.go`).
//
// Convention `events:shard:<n>` — отдельный namespace от
// `outbound:<sid>`/`soul:<sid>:hb`/`soul:<sid>:lock`, ключевое слово
// «events» симметрично EventKind-ам (`apply.started`/`apply.completed`/
// `errand.completed`/…) и не привязано к одному семейству opaque-id.
//
// Wire-формат — JSON-envelope `applyEventEnvelope`. Payload остаётся
// `json.RawMessage`, чтобы избежать двойной перекодировки: publisher
// уже отдаёт payload как json-сериализуемый `map[string]any` (см.
// events_taskevent.go / events_runresult.go).
//
// Семантика: fire-and-forget. Redis pub/sub не имеет persistence — если
// подписчик ещё не subscribe-нулся в момент PUBLISH, сообщение теряется.
// Это приемлемо: SSE-клиент по контракту подписывается ДО старта apply
// (порядок «subscribe → tools/call → ждать SSE-events», см. doc-comment
// [applybus.EventBus.Publish] про late-subscriber semantics).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// applyBusChannelPrefix — namespace шардированных apply-events-каналов.
// Слово «events» симметрично EventKind-ам и не привязано к одному семейству
// opaque-id (apply.* / errand.* / …), в отличие от прежнего `apply:<id>`.
const applyBusChannelPrefix = "events"

// ApplyBusShardCount — фиксированное число shard-каналов. applyID
// детерминированно отображается в один из них через [ApplyBusShardIndex].
// Значение подобрано как компромисс: достаточно велико, чтобы события разных
// прогонов почти не делили один forward-loop (collision-частота ≈ 1/K), и
// достаточно мало, чтобы каждый Keeper-инстанс держал ограниченное число
// Redis-подписок независимо от размера флота (snapshot выбирался architect-ом).
const ApplyBusShardCount = 256

// ApplyBusShardIndex отображает applyID в индекс shard-канала
// [0, ApplyBusShardCount). fnv32a — быстрый неаллоцирующий хеш;
// криптостойкость не нужна (это распределение нагрузки, не безопасность).
//
// Экспортирован, чтобы applybus ключевал per-shard bridge-refs тем же индексом,
// что и канал (единый источник shard-резолва, без рассинхрона).
func ApplyBusShardIndex(applyID string) uint32 {
	h := fnv.New32a()
	// Hash.Write по контракту never-error.
	_, _ = h.Write([]byte(applyID))
	return h.Sum32() % ApplyBusShardCount
}

// ApplyBusChannel формирует шардированный Redis-канал для applyID.
// Детерминирован: один applyID всегда даёт один канал (см. [ApplyBusShardIndex]).
func ApplyBusChannel(applyID string) string {
	return fmt.Sprintf("%s:shard:%d", applyBusChannelPrefix, ApplyBusShardIndex(applyID))
}

// applyEventEnvelope — JSON-обёртка одного PUBLISH-сообщения.
//
// `OriginKID` — KID Keeper-инстанса, который опубликовал; подписчик
// фильтрует сообщения с OriginKID == собственный KID (предотвращает
// дубль local-publish + Redis-echo на том же Keeper-е).
//
// `Payload` — `json.RawMessage`: publisher (events_taskevent.go /
// events_runresult.go) собирает `map[string]any` и так уже подаёт его в
// `applybus.Event.Payload`. Чтобы не делать `json.Marshal` дважды
// (один раз в SSE-frame, второй — внутрь envelope-string), payload
// маршалим один раз на стороне publisher-а и кладём байтами в RawMessage.
type applyEventEnvelope struct {
	OriginKID string          `json:"origin_kid"`
	Kind      string          `json:"kind"`
	ApplyID   string          `json:"apply_id"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload"`
}

// ApplyEvent — распакованное cluster-bus-сообщение, доставляемое
// подписчику. Симметрично `applybus.Event`, но Payload здесь —
// `json.RawMessage`: cluster-bridge не знает typed-структуру payload-а,
// он лишь передаёт байты, а SSE-handler сериализует их обратно в frame
// (см. mcp/sse.go::writeSSEEvent).
type ApplyEvent struct {
	OriginKID string
	Kind      string
	ApplyID   string
	At        time.Time
	Payload   json.RawMessage
}

// PublishApplyEvent сериализует event и публикует в канал
// [ApplyBusChannel]. payload — уже сериализованный JSON-объект
// (см. doc-comment [applyEventEnvelope]). Возвращает количество
// подписчиков, получивших сообщение.
//
// Если at.IsZero — подставляется time.Now().UTC() (симметрично
// `applybus.EventBus.Publish`).
func PublishApplyEvent(ctx context.Context, c *Client, applyID, originKID, kind string, at time.Time, payload json.RawMessage) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishApplyEvent: nil client")
	}
	if applyID == "" {
		return 0, errors.New("redis.PublishApplyEvent: empty applyID")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishApplyEvent: empty originKID")
	}
	if kind == "" {
		return 0, errors.New("redis.PublishApplyEvent: empty kind")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}

	env, err := json.Marshal(applyEventEnvelope{
		OriginKID: originKID,
		Kind:      kind,
		ApplyID:   applyID,
		At:        at,
		Payload:   payload,
	})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishApplyEvent: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, ApplyBusChannel(applyID), env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishApplyEvent: PUBLISH %q: %w", ApplyBusChannel(applyID), err)
	}
	return n, nil
}

// applyEventSubBufferSize — буфер Go-канала между Redis-PubSub-loop-ом и
// caller-ом (applybus-bridge). Симметричен outboundSubBufferSize. 64
// события — типичный apply (10–30 задач + старт/финал) + запас на burst;
// на один shard-канал может приходить несколько applyID (collision ≈ 1/K),
// но forward-loop разгребает в local-subs синхронно и быстро, переполнение
// маловероятно.
const applyEventSubBufferSize = 64

// ApplyEventSubscription — handle на подписку одного shard-канала
// `events:shard:<n>` (см. [ApplyBusChannel]). Через неё проходят события
// всех applyID, отображённых в этот shard; фильтрацию по конкретному applyID
// делает forward-loop в applybus.
//
// Идентичен по lifecycle [OutboundSubscription]:
// spawn goroutine читает PubSub-канал → парсит envelope → filter
// self-origin → отдаёт *ApplyEvent в Channel(). Close() прерывает
// goroutine и закрывает канал.
type ApplyEventSubscription struct {
	ps        *redis.PubSub
	out       chan *ApplyEvent
	selfKID   string
	applyID   string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel — read-side Go-канала с распакованными ApplyEvent-сообщениями.
func (s *ApplyEventSubscription) Channel() <-chan *ApplyEvent {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready блокируется до первого subscribe-acknowledgement от Redis (или
// ctx.Done() / Close()). См. doc-comment [OutboundSubscription.Ready].
func (s *ApplyEventSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.ApplyEventSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.ApplyEventSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close прерывает subscribe-loop, закрывает Redis-pubsub-handle и
// Go-канал. Идемпотентна.
func (s *ApplyEventSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeApplyEvent подписывается на shard-канал [ApplyBusChannel] (applyID)
// и поднимает goroutine-forwarder. selfKID используется для self-фильтрации
// (как в [SubscribeOutbound]). applyID здесь задаёт лишь shard и используется
// в логах; на канал приходят события всех applyID того же shard-а — отсев по
// конкретному applyID делает caller (applybus forward-loop).
func SubscribeApplyEvent(ctx context.Context, c *Client, applyID, selfKID string, logger *slog.Logger) (*ApplyEventSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeApplyEvent: nil client")
	}
	if applyID == "" {
		return nil, errors.New("redis.SubscribeApplyEvent: empty applyID")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeApplyEvent: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeApplyEvent: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, ApplyBusChannel(applyID))

	s := &ApplyEventSubscription{
		ps:      ps,
		out:     make(chan *ApplyEvent, applyEventSubBufferSize),
		selfKID: selfKID,
		applyID: applyID,
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

func (s *ApplyEventSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeApplyEvent: initial Receive failed",
			slog.String("apply_id", s.applyID), slog.Any("error", err))
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
			s.logger.Warn("redis.SubscribeApplyEvent: ReceiveMessage failed",
				slog.String("apply_id", s.applyID), slog.Any("error", err))
			return
		}

		ev, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if ev.OriginKID == s.selfKID {
			// Self-echo: эту публикацию сделали мы сами. Не пересылаем
			// дальше — local-bus уже доставил её local-subscriber-ам.
			s.logger.Debug("redis.SubscribeApplyEvent: ignoring self-origin event",
				slog.String("apply_id", s.applyID),
				slog.String("origin_kid", ev.OriginKID))
			continue
		}

		s.forward(ev)
	}
}

// forward кладёт ev в s.out. При полном буфере дропает САМОЕ СТАРОЕ событие
// (вычитывает один элемент и пишет новый в освободившийся слот), симметрично
// local-буферу applybus (drop-oldest, см. [applybus.EventBus.deliver]).
//
// Почему keep-freshest, а не keep-oldest: на cross-keeper-пути самое свежее
// событие — терминал прогона (`apply.completed`/`apply.failed`), и дропать его
// в пользу старого «task_idx=0 OK» хуже для SSE-клиента. Сам applybus
// неавторитетен (источник правды — PG, недоставленный терминал добивает
// dispatcher-timer), но keep-freshest — правильная политика по умолчанию.
//
// Forward-loop единственный писатель в s.out, поэтому read+write здесь без
// гонки за слот: между select-ами никто другой в канал не пишет.
func (s *ApplyEventSubscription) forward(ev *ApplyEvent) {
	select {
	case s.out <- ev:
		return
	default:
	}
	// Полный буфер — освобождаем слот, вытолкнув самое старое.
	select {
	case <-s.out:
		s.logger.Warn("redis.SubscribeApplyEvent: forward channel full — dropped oldest event",
			slog.String("apply_id", s.applyID),
			slog.String("origin_kid", ev.OriginKID),
			slog.String("kind", ev.Kind))
	default:
		// Буфер опустошён читателем между select-ами — кладём новое ниже.
	}
	select {
	case s.out <- ev:
	default:
		// Маловероятно (читатель не успел вычитать освобождённый слот) — но
		// не блокируемся: гарантия «forward-loop не зависает» важнее одного
		// события. Symmetric с applybus.deliver final-branch.
		s.logger.Warn("redis.SubscribeApplyEvent: forward channel still full after drop — event lost",
			slog.String("apply_id", s.applyID),
			slog.String("origin_kid", ev.OriginKID),
			slog.String("kind", ev.Kind))
	}
}

func (s *ApplyEventSubscription) decodeMessage(payload string) (*ApplyEvent, bool) {
	var env applyEventEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeApplyEvent: envelope unmarshal failed",
			slog.String("apply_id", s.applyID), slog.Any("error", err))
		return nil, false
	}
	return &ApplyEvent{
		OriginKID: env.OriginKID,
		Kind:      env.Kind,
		ApplyID:   env.ApplyID,
		At:        env.At,
		Payload:   env.Payload,
	}, true
}
