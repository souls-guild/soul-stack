// Package sshprovider — SDK Soul Stack для авторов SshProvider-плагинов
// (kind: ssh_provider, бинари soul-ssh-<provider>).
//
// Минимальный путь автора плагина:
//
//	type VaultSshProvider struct { sshprovider.BaseProvider }
//
//	func (v *VaultSshProvider) Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
//	    // ...
//	}
//
//	func main() {
//	    if err := sshprovider.Serve(&VaultSshProvider{}); err != nil { os.Exit(1) }
//	}
//
// BaseProvider даёт no-op-реализации: Sign возвращает пустой SignReply,
// Authorize отвечает allowed=true (allowlist по умолчанию приемлем только в
// dev/тестах — production-провайдер обязан переопределить хотя бы Authorize).
// Serve открывает Unix-socket, делает gRPC-stdio handshake и обрабатывает
// SIGTERM (см. sdk/handshake).
package sshprovider

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"google.golang.org/grpc"
)

// protocolVersion — версия plugin-протокола MVP (docs/keeper/plugins.md →
// Versioning). Симметрично sdk/module и sdk/clouddriver.
const protocolVersion = 1

// SshProvider — интерфейс, который реализует плагин-автор. Сигнатуры повторяют
// pluginv1.SshProviderServer, но без must-embed-требования к
// pluginv1.UnimplementedSshProviderServer: SDK берёт forward-compat на себя
// через внутренний adapter.
type SshProvider interface {
	Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error)
	Authorize(ctx context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error)
}

// BaseProvider — embeddable default-реализация SshProvider:
// Sign возвращает пустой SignReply, Authorize — allowed=true.
// Дефолт Authorize=true допустим только для тестовых плагинов;
// production-провайдер обязан переопределить хотя бы Authorize.
type BaseProvider struct{}

func (BaseProvider) Sign(context.Context, *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return &pluginv1.SignReply{}, nil
}

func (BaseProvider) Authorize(context.Context, *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}

// Serve — типовой main() SshProvider-плагина: оборачивает sdk/handshake.Serve
// + регистрирует grpc-service pluginv1.SshProvider с автор-impl.
func Serve(impl SshProvider) error {
	return handshake.Serve(handshake.Config{
		ProtocolVersion: protocolVersion,
		Kind:            pluginv1.Kind_KIND_SSH_PROVIDER,
	}, func(s *grpc.Server) {
		pluginv1.RegisterSshProviderServer(s, &serverAdapter{impl: impl})
	})
}

// serverAdapter — мост между SDK-интерфейсом SshProvider и
// pluginv1.SshProviderServer; embed Unimplemented обеспечивает forward-compat
// при добавлении новых RPC в proto/plugin/v2/.
type serverAdapter struct {
	pluginv1.UnimplementedSshProviderServer
	impl SshProvider
}

func (a *serverAdapter) Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return a.impl.Sign(ctx, req)
}

func (a *serverAdapter) Authorize(ctx context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return a.impl.Authorize(ctx, req)
}
