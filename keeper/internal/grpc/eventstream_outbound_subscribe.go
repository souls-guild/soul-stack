package grpc

import (
	"context"
	"log/slog"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// Cluster-mode outbound-subscribe loop (ADR-002 HA).
//
// Когда EventStream-handler регистрирует стрим в StreamManager на
// Keeper-B, параллельно подписывается на Redis pub/sub-канал
// `outbound:<sid>`. Другой Keeper-инстанс (Keeper-A), у которого
// Outbound.SendApply не нашёл локального стрима и увидел в lease
// `holder == kid-B`, публикует туда FromKeeper-сообщение. Subscriber
// здесь форвардит его в локальный outbound-channel того же entry.
//
// Self-фильтрация по `origin_kid` — на стороне
// [keeperredis.SubscribeOutbound]; сюда уже доходят сообщения,
// опубликованные другими Keeper-инстансами.

// startOutboundSubscriber поднимает Redis-подписку и forward-goroutine
// для cluster-mode routing-а. Возвращает cleanup-функцию (Close
// pub/sub) и канал, который закрывается при выходе forward-goroutine.
//
// Если Manager==nil или Redis==nil — cluster-routing выключен:
// возвращаем (nil cleanup, сразу закрытый done). Caller (handler)
// просто ждёт done и игнорирует nil-cleanup.
//
// Ошибка подписки → warn-лог, cluster-routing деградирует до
// per-instance (locally-only); handler продолжает работу — критичных
// инвариантов это не ломает (просто Outbound.SendApply с другого
// Keeper-а вернёт ErrSoulNotConnected, и caller увидит штатный
// "no subscribers" error).
func (h *eventStreamHandler) startOutboundSubscriber(ctx context.Context, sid string, done chan<- struct{}) func() {
	if h.deps.Manager == nil || h.deps.Redis == nil {
		close(done)
		return nil
	}

	sub, err := keeperredis.SubscribeOutbound(ctx, h.deps.Redis, sid, h.deps.KID, h.logger)
	if err != nil {
		h.logger.Warn("eventstream: outbound pub/sub subscribe failed (cluster-routing disabled for this session)",
			slog.String("sid", sid),
			slog.Any("error", err),
		)
		close(done)
		return nil
	}
	// Ждём подтверждения от Redis, что подписка зарегистрирована — иначе
	// PublishOutbound, вызванный сразу после захвата lease на этой
	// стороне, мог бы промахнуться. Не блокируемся бесконечно:
	// разумный таймаут привязан к ctx стрима.
	if err := sub.Ready(ctx); err != nil {
		h.logger.Warn("eventstream: outbound pub/sub Ready failed",
			slog.String("sid", sid),
			slog.Any("error", err),
		)
		_ = sub.Close()
		close(done)
		return nil
	}

	go h.runOutboundSubscriber(sid, sub, done)
	return func() { _ = sub.Close() }
}

func (h *eventStreamHandler) runOutboundSubscriber(sid string, sub *keeperredis.OutboundSubscription, done chan<- struct{}) {
	defer close(done)
	in := sub.Channel()
	for msg := range in {
		entry := h.deps.Manager.lookup(sid)
		if entry == nil {
			// Стрим уже Unregister-ован — это конкурентный shutdown.
			// Сообщение теряется (fire-and-forget семантика pub/sub-а
			// по PM-decision 5).
			h.logger.Debug("eventstream: outbound subscriber dropping message — no local entry",
				slog.String("sid", sid))
			continue
		}
		if !entry.send(msg) {
			h.logger.Warn("eventstream: outbound subscriber drop — queue full or closed",
				slog.String("sid", sid))
		}
	}
}
