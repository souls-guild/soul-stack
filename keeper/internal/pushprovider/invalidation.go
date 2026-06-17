package pushprovider

import (
	"context"
)

// TopicPushProvidersChanged — Redis pub/sub topic для cluster-wide
// уведомления об изменении push_providers (ADR-032 amendment 2026-05-26, S7-2).
//
// Publish-сторона: pushprovider.Service после успешного commit-а
// Create/Update/Delete шлёт сюда `provider_name` (или пустую строку для
// массовых операций). Subscribe-сторона: SshDispatcher в setupPushDispatchers
// при получении сообщения помечает spawned-плагин «stale» и пере-spawn-ит его
// на ближайшем RPC (spawn-on-change semantics).
//
// Persistence у Redis pub/sub нет: потеря сообщения (reconnect, мигание
// брокера) → плагин остаётся с прежним env-payload до следующей мутации
// либо рестарта keeper-а. Это допустимо — мутации редкие, окно устаревания
// миллисекунды при штатной работе.
//
// Convention `push-providers:changed` — отдельный namespace стилистически
// близкий к `sigil:invalidate` / `rbac:invalidate` (формат `<подсистема>:
// <событие>`); множественное `push-providers` отражает имя ресурса (REST
// `/v1/push-providers`).
const TopicPushProvidersChanged = "push-providers:changed"

// RedisPublisher — узкая поверхность Redis-клиента, нужная invalidation-у.
// Реализуется keeperredis.Client (через метод-обёртку, добавляемый в S7-2
// wire-up) и любым моком для unit-тестов. Сужаем до одного метода, чтобы
// service не тянул весь redis-client / go-redis в зависимости.
type RedisPublisher interface {
	PublishPushProvidersChanged(ctx context.Context, providerName string) error
}

// nopPublisher — no-op реализация для случаев, когда Redis выключен
// (single-instance dev / unit-тесты без Redis). Симметрично nilTollDegraded:
// service не падает, просто spawn-on-change не работает в кластерном режиме
// (изменения видны только на инстансе, принявшем мутацию, после рестарта).
type nopPublisher struct{}

// NopPublisher возвращает no-op [RedisPublisher].
func NopPublisher() RedisPublisher { return nopPublisher{} }

func (nopPublisher) PublishPushProvidersChanged(_ context.Context, _ string) error {
	return nil
}
