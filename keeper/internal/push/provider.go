// Package push — S0-пилот keeper.push (agentless SSH-доставка, ADR-004,
// docs/keeper/push.md). Реализует синхронный oneshot-транспорт: Keeper по SSH
// заходит на хост `transport=ssh`, запускает `soul apply`, скармливает
// отрендеренный ApplyRequest (protojson) в stdin и читает NDJSON-поток
// TaskEvent + финальный RunResult из stdout.
//
// Пилот доказывает транспорт end-to-end и задаёт pattern. ВНЕ scope-а пилота
// (отдельные слайсы): SHA-256-кеш доставки soul-бинаря/модулей (S1),
// vault-провайдер SSH CA (S2), интеграция в scenario-runner как
// alt-Outbound-диспетчер (S3), фасады OpenAPI+MCP (S4), host-side cleanup (S5).
package push

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// SshProvider — узкий контракт SSH-аутентификации, который нужен диспетчеру:
// Authorize (право Keeper-а ходить на хост) + Sign (выпуск SSH-credentials на
// сессию). Совпадает по сигнатурам с [pluginhost.SshProviderPlugin] (Sign /
// Authorize), поэтому реальный provider подставляется без адаптера, а в тестах
// мокается struct-ом.
//
// Контракт SshProvider определён в sdk/sshprovider; здесь — host-side
// потребительский интерфейс ровно из двух методов, которыми пользуется
// [SshDispatcher]. Сужение поверхности (а не reuse pluginv1.SshProviderClient
// целиком) держит диспетчер тестируемым без spawn-а плагина.
type SshProvider interface {
	// Authorize подтверждает право Keeper-а открыть SSH-сессию к (host, user).
	// deny → диспетчер прекращает прогон до connect-а (fail-closed).
	Authorize(ctx context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error)
	// Sign выдаёт SSH-credentials на текущую сессию (CA-signed cert или
	// ephemeral keypair — под единым контрактом).
	Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error)
}
