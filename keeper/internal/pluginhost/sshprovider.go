package pluginhost

import (
	"context"
	"fmt"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// SshProviderPlugin is thin wrapper over [Plugin], tying base handle
// to SshProvider gRPC client. Created via [NewSshProviderPlugin] after
// successful [Host.Spawn]: caller verifies manifest.kind == ssh_provider,
// and wraps Plugin in SshProviderPlugin.
//
// Apply-cycle keeper.push (docs/keeper/push.md) uses Sign before each
// SSH session and Authorize to check host access permissions.
//
// Close proxied to underlying Plugin.Close (idempotent).
type SshProviderPlugin struct {
	*Plugin
	client pluginv1.SshProviderClient
}

// NewSshProviderPlugin wraps [Plugin] (from [Host.Spawn]) in kind-specific
// handle. Returns error if manifest.kind != ssh_provider: protection
// from accidental call on soul_module / cloud_driver binary.
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

// Sign is RPC SshProvider.Sign. Issues SSH certificate/key for current session
// (Vault SSH CA, Teleport, static-key — all under unified contract).
func (s *SshProviderPlugin) Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return s.client.Sign(ctx, req)
}

// Authorize is RPC SshProvider.Authorize. Confirms Keeper's right to access
// specific host (provider policy if it exists).
func (s *SshProviderPlugin) Authorize(ctx context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return s.client.Authorize(ctx, req)
}
