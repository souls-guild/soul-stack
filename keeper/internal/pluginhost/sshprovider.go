package pluginhost

import (
	"context"
	"fmt"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// SshProviderPlugin — тонкая обёртка над [Plugin], привязывающая базовый handle
// к gRPC-клиенту SshProvider. Создаётся через [NewSshProviderPlugin] после
// успешного [Host.Spawn]: caller проверяет, что manifest.kind == ssh_provider,
// и оборачивает Plugin в SshProviderPlugin.
//
// Apply-цикл keeper.push (docs/keeper/push.md) использует Sign перед каждой
// SSH-сессией и Authorize для проверки прав на хост.
//
// Close проксируется на underlying Plugin.Close (идемпотентен).
type SshProviderPlugin struct {
	*Plugin
	client pluginv1.SshProviderClient
}

// NewSshProviderPlugin оборачивает [Plugin] (из [Host.Spawn]) в kind-specific
// handle. Возвращает ошибку, если manifest.kind != ssh_provider: это защита
// от случайного вызова на soul_module / cloud_driver бинаре.
func NewSshProviderPlugin(p *Plugin) (*SshProviderPlugin, error) {
	if p == nil {
		return nil, fmt.Errorf("pluginhost: nil Plugin")
	}
	if p.Manifest().Kind != KindSSHProvider {
		return nil, fmt.Errorf("pluginhost: expected kind=ssh_provider, manifest %s has kind=%q",
			p.Manifest().Address(), p.Manifest().Kind)
	}
	return &SshProviderPlugin{
		Plugin: p,
		client: pluginv1.NewSshProviderClient(p.Conn()),
	}, nil
}

// Sign — RPC SshProvider.Sign. Выдаёт SSH-сертификат/ключ для текущей сессии
// (Vault SSH CA, Teleport, static-key — всё под единым контрактом).
func (s *SshProviderPlugin) Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return s.client.Sign(ctx, req)
}

// Authorize — RPC SshProvider.Authorize. Подтверждает право Keeper-а ходить
// на конкретный хост (политика провайдера, если она есть).
func (s *SshProviderPlugin) Authorize(ctx context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return s.client.Authorize(ctx, req)
}
