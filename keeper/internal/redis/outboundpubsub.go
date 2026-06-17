package redis

// Cluster-mode outbound-routing через Redis pub/sub (ADR-002, HA).
//
// Проблема: StreamManager в keeper/internal/grpc — per-Keeper-инстанс
// реестр. Если Outbound.SendApply вызван на Keeper-A для SID, чей
// EventStream висит на Keeper-B, локальный Lookup пуст и без pub/sub
// возвращается ErrSoulNotConnected.
//
// Решение: Keeper-A проверяет SoulLease (`soul:<sid>:lock`), узнаёт
// holder=Keeper-B и публикует FromKeeper-сообщение в канал
// `outbound:<sid>`. Keeper-B при Register-е стрима подписывается на
// этот канал и форвардит входящие в локальный outbound-channel.
//
// Convention `outbound:<sid>` симметрична heartbeat-кэшу
// (`soul:<sid>:hb`) и lease-у (`soul:<sid>:lock`).
//
// Wire-формат — protojson (PM-decision: text-friendly, debuggable
// через `redis-cli SUBSCRIBE`, форвард-compat по proto-полям). Поверх
// — тонкая JSON-envelope с полем `origin_kid`, чтобы subscriber мог
// фильтровать собственные публикации (race-окно: holder поменялся
// между publish и delivery — Keeper, ставший новым holder-ом, мог бы
// получить эхо своей же отправки).
//
// Семантика: fire-and-forget. Redis pub/sub не имеет TTL/persistence
// — если subscriber отключён в момент PUBLISH, сообщение теряется.
// Это приемлемо для apply-commands (caller увидит результат через
// RunResult; для отсутствующего ответа есть таймауты на уровне
// сценария).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protojson"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// OutboundChannelKey формирует Redis-канал outbound-routing для SID.
//
// `outbound:<sid>` — отдельный namespace, не пересекается с
// heartbeat-кэшем (`soul:<sid>:hb`) и lease-ключом
// (`soul:<sid>:lock`); канал pub/sub-only, keyspace отдельный.
func OutboundChannelKey(sid string) string {
	return "outbound:" + sid
}

// outboundEnvelope — JSON-обёртка для одного PUBLISH-сообщения.
//
// `OriginKID` — KID Keeper-инстанса, который опубликовал; subscriber
// игнорирует сообщения с OriginKID == собственный KID (см.
// [OutboundSubscription.run]).
//
// `Payload` — protojson-сериализованный `FromKeeper`. Хранится как
// json.RawMessage, чтобы избежать двойного encode (protojson → byte
// → json-string → byte): сразу подкладываем raw protojson внутрь
// envelope.
type outboundEnvelope struct {
	OriginKID string          `json:"origin_kid"`
	Payload   json.RawMessage `json:"payload"`
}

// PublishOutbound сериализует `FromKeeper` через protojson, заворачивает в
// outbound-envelope с OriginKID = kid Publisher-а и публикует в канал
// [OutboundChannelKey].
//
// Возвращает количество подписчиков, получивших сообщение (Redis
// PUBLISH-результат). Caller (Outbound.SendApply/Cancel/...)
// интерпретирует 0 как «никто не подписан в момент publish — Soul
// возможно реконнектится, либо сообщение пропало». В MVP-семантике
// мы НЕ ретраим автоматически: для apply-команд это правильно (даже
// retransmit-нутая команда может пересечься с тем же apply_id), для
// cancel — Soul ProteinApplyRunner всё равно дочитает run до конца.
func PublishOutbound(ctx context.Context, c *Client, sid, originKID string, msg *keeperv1.FromKeeper) (int64, error) {
	if c == nil {
		return 0, errors.New("redis.PublishOutbound: nil client")
	}
	if sid == "" {
		return 0, errors.New("redis.PublishOutbound: empty sid")
	}
	if originKID == "" {
		return 0, errors.New("redis.PublishOutbound: empty originKID")
	}
	if msg == nil {
		return 0, errors.New("redis.PublishOutbound: nil msg")
	}

	payload, err := protojson.Marshal(msg)
	if err != nil {
		return 0, fmt.Errorf("redis.PublishOutbound: protojson.Marshal: %w", err)
	}
	env, err := json.Marshal(outboundEnvelope{OriginKID: originKID, Payload: payload})
	if err != nil {
		return 0, fmt.Errorf("redis.PublishOutbound: envelope marshal: %w", err)
	}

	n, err := c.underlying().Publish(ctx, OutboundChannelKey(sid), env).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.PublishOutbound: PUBLISH %q: %w", OutboundChannelKey(sid), err)
	}
	return n, nil
}

// OutboundSubscription — handle на активную подписку `outbound:<sid>`.
//
// Создаётся через [SubscribeOutbound]; goroutine читает Redis
// PubSub-канал, парсит envelope, фильтрует по origin_kid (отсекает
// эхо собственных публикаций), кладёт *FromKeeper в Go-канал
// [OutboundSubscription.Channel]. На Close() закрывает Redis-pubsub
// и Go-канал.
//
// Поток жизни subscribe-loop-а: spawn под Register(sid) в
// StreamManager, Close() при Unregister. Не потокобезопасен между
// Close() и read-ом из Channel() — caller обязан остановить чтение
// до Close (типично — handler-defer LIFO: сначала Unregister →
// Close, потом ожидание receive-loop-а).
type OutboundSubscription struct {
	ps        *redis.PubSub
	out       chan *keeperv1.FromKeeper
	selfKID   string
	sid       string
	logger    *slog.Logger
	ready     chan struct{}
	stopped   chan struct{}
	closeOnce func() error
}

// Channel — read-side Go-канала с распакованными FromKeeper-сообщениями.
//
// Закрывается при [OutboundSubscription.Close] или при фатальной
// ошибке Redis (потеря соединения — go-redis сам пытается
// reconnect, но ReceiveMessage может вернуть err после нескольких
// неудач). На закрытие caller завершает forward-loop-у.
func (s *OutboundSubscription) Channel() <-chan *keeperv1.FromKeeper {
	if s == nil {
		return nil
	}
	return s.out
}

// Ready блокируется до первого `subscribe`-acknowledgement от Redis
// (или ctx.Done() / Close()). Гарантирует, что после возврата канал
// действительно зарегистрирован на Redis-стороне; без этого
// PublishOutbound, вызванный сразу после SubscribeOutbound, мог бы
// промахнуться (subscribe еще не дошёл до брокера).
//
// Используется тестами; production-код вызывает однократно после
// [SubscribeOutbound], чтобы гарантировать ordering между Register
// и первой возможной публикацией.
func (s *OutboundSubscription) Ready(ctx context.Context) error {
	if s == nil {
		return errors.New("redis.OutboundSubscription.Ready: nil subscription")
	}
	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		return errors.New("redis.OutboundSubscription.Ready: subscription stopped before ready")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close прерывает subscribe-loop, закрывает Redis-pubsub-handle и Go-
// канал. Идемпотентна — повторный Close — no-op.
func (s *OutboundSubscription) Close() error {
	if s == nil {
		return nil
	}
	return s.closeOnce()
}

// SubscribeOutbound подписывается на `outbound:<sid>` и поднимает
// goroutine-forwarder. selfKID используется для self-фильтрации (см.
// тип-комментарий [OutboundSubscription]).
//
// Goroutine завершается по:
//   - Close() caller-а (Unregister в StreamManager);
//   - ctx.Done() — внешняя отмена;
//   - фатальная ошибка ReceiveMessage (Redis недоступен надолго).
//
// На любую non-fatal ошибку парсинга envelope пишем warn-log и
// продолжаем (битое сообщение — bug сериализатора, не повод ронять
// весь канал).
func SubscribeOutbound(ctx context.Context, c *Client, sid, selfKID string, logger *slog.Logger) (*OutboundSubscription, error) {
	if c == nil {
		return nil, errors.New("redis.SubscribeOutbound: nil client")
	}
	if sid == "" {
		return nil, errors.New("redis.SubscribeOutbound: empty sid")
	}
	if selfKID == "" {
		return nil, errors.New("redis.SubscribeOutbound: empty selfKID")
	}
	if logger == nil {
		return nil, errors.New("redis.SubscribeOutbound: nil logger")
	}

	ps := c.underlying().Subscribe(ctx, OutboundChannelKey(sid))

	s := &OutboundSubscription{
		ps:      ps,
		out:     make(chan *keeperv1.FromKeeper, outboundSubBufferSize),
		selfKID: selfKID,
		sid:     sid,
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
		// ps.Close прерывает ReceiveMessage в forward-loop-е (вернёт ошибку);
		// loop завершается, закрывает out-канал и s.stopped.
		err := ps.Close()
		<-s.stopped
		return err
	}

	go s.run(ctx, closed)
	return s, nil
}

// outboundSubBufferSize — буфер Go-канала между Redis-PubSub-loop-ом и
// caller-ом (StreamManager forward-goroutine). Симметричен
// outboundBufferSize в grpc-пакете: один Soul видит редкие
// FromKeeper-сообщения, 10 элементов покрывают burst.
const outboundSubBufferSize = 10

func (s *OutboundSubscription) run(ctx context.Context, closed <-chan struct{}) {
	defer close(s.out)
	defer close(s.stopped)

	// Дожидаемся subscribe-acknowledgement, потом сигналим Ready. На
	// fatal-ошибку Receive — выходим (closeOnce ниже не вызывает
	// двойную close).
	if _, err := s.ps.Receive(ctx); err != nil {
		s.logger.Warn("redis.SubscribeOutbound: initial Receive failed",
			slog.String("sid", s.sid), slog.Any("error", err))
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
				// Ожидаемое завершение через Close().
				return
			case <-ctx.Done():
				return
			default:
			}
			s.logger.Warn("redis.SubscribeOutbound: ReceiveMessage failed",
				slog.String("sid", s.sid), slog.Any("error", err))
			return
		}

		fromKeeper, originKID, ok := s.decodeMessage(msg.Payload)
		if !ok {
			continue
		}
		if originKID == s.selfKID {
			// Self-echo: эту публикацию сделали мы сами (race-окно при
			// смене lease-holder-а — мы стали holder-ом уже после publish,
			// но до subscribe). Игнорируем, чтобы не зацикливать.
			s.logger.Debug("redis.SubscribeOutbound: ignoring self-origin message",
				slog.String("sid", s.sid), slog.String("origin_kid", originKID))
			continue
		}

		select {
		case s.out <- fromKeeper:
		default:
			// Forward-канал переполнен — local stream-receiver не успевает.
			// Drop+log: гарантии delivery нет уже на уровне pub/sub-а,
			// drop на forward-границе симметричен drop-у в Outbound.send.
			s.logger.Warn("redis.SubscribeOutbound: forward channel full, dropping",
				slog.String("sid", s.sid), slog.String("origin_kid", originKID))
		}
	}
}

func (s *OutboundSubscription) decodeMessage(payload string) (*keeperv1.FromKeeper, string, bool) {
	var env outboundEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		s.logger.Warn("redis.SubscribeOutbound: envelope unmarshal failed",
			slog.String("sid", s.sid), slog.Any("error", err))
		return nil, "", false
	}
	if len(env.Payload) == 0 {
		s.logger.Warn("redis.SubscribeOutbound: empty payload in envelope",
			slog.String("sid", s.sid))
		return nil, env.OriginKID, false
	}
	out := &keeperv1.FromKeeper{}
	if err := protojson.Unmarshal(env.Payload, out); err != nil {
		s.logger.Warn("redis.SubscribeOutbound: protojson.Unmarshal failed",
			slog.String("sid", s.sid), slog.Any("error", err))
		return nil, env.OriginKID, false
	}
	return out, env.OriginKID, true
}

// ReadSoulLeaseHolder возвращает текущий kid-holder lease-а на SID, либо
// "" если ключа нет. Используется Outbound-ом для cluster-mode
// routing-а: holder != self → publish to outbound-канал; holder ==
// self без локального стрима → lease-inconsistency error; holder ==
// "" → ErrSoulNotConnected.
//
// Это голый GET по lease-ключу (SoulLeaseKey) — не lease-acquire, не
// modify. Никакой race-protection поверх: holder может смениться
// между read-ом и последующим Publish-ом, и это нормально (worst
// case — публикация уходит в эфир без подписчика и теряется,
// семантика «fire-and-forget» сохраняется).
func ReadSoulLeaseHolder(ctx context.Context, c *Client, sid string) (string, error) {
	if c == nil {
		return "", errors.New("redis.ReadSoulLeaseHolder: nil client")
	}
	if sid == "" {
		return "", errors.New("redis.ReadSoulLeaseHolder: empty sid")
	}
	v, err := c.underlying().Get(ctx, SoulLeaseKey(sid)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("redis.ReadSoulLeaseHolder: GET %q: %w", SoulLeaseKey(sid), err)
	}
	return v, nil
}
