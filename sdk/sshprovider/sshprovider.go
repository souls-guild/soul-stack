// Package sshprovider is the Soul Stack SDK for SshProvider plugin authors
// (kind: ssh_provider, binaries soul-ssh-<provider>).
//
// Minimal plugin author path:
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
// BaseProvider provides no-op implementations: Sign returns an empty
// SignReply, Authorize responds allowed=true (an allow-by-default is
// acceptable only in dev/tests — a production provider must override at
// least Authorize).
// Serve opens a Unix socket, performs the gRPC-stdio handshake, and handles
// SIGTERM (see sdk/handshake).
package sshprovider

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"google.golang.org/grpc"
)

// protocolVersion is the MVP plugin protocol version (docs/keeper/plugins.md →
// Versioning). Symmetric with sdk/module and sdk/clouddriver.
const protocolVersion = 1

// SshProvider is the interface implemented by the plugin author. Signatures
// mirror pluginv1.SshProviderServer, but without the must-embed requirement
// for pluginv1.UnimplementedSshProviderServer: the SDK takes forward-compat
// on itself via an internal adapter.
type SshProvider interface {
	Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error)
	Authorize(ctx context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error)
}

// BaseProvider is an embeddable default implementation of SshProvider:
// Sign returns an empty SignReply, Authorize returns allowed=true.
// The Authorize=true default is acceptable only for test plugins;
// a production provider must override at least Authorize.
type BaseProvider struct{}

func (BaseProvider) Sign(context.Context, *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return &pluginv1.SignReply{}, nil
}

func (BaseProvider) Authorize(context.Context, *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}

// Serve is the typical main() of an SshProvider plugin: wraps
// sdk/handshake.Serve + registers the pluginv1.SshProvider grpc-service with
// the author's impl.
func Serve(impl SshProvider) error {
	return handshake.Serve(handshake.Config{
		ProtocolVersion: protocolVersion,
		Kind:            pluginv1.Kind_KIND_SSH_PROVIDER,
	}, func(s *grpc.Server) {
		pluginv1.RegisterSshProviderServer(s, &serverAdapter{impl: impl})
	})
}

// serverAdapter is the bridge between the SDK's SshProvider interface and
// pluginv1.SshProviderServer; embedding Unimplemented provides forward-compat
// when new RPCs are added in proto/plugin/v2/.
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
