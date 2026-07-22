package pluginhost

import (
	"context"
	"fmt"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"google.golang.org/grpc"
)

// Plugin is the Soul-side handle for a spawned kind=soul_module plugin session.
// Embeds [sharedhost.BasePlugin] (manifest / conn / Close / StderrTail) and adds
// a SoulModule gRPC client for Validate / Plan / Apply.
//
// Lifecycle is one-shot per Spawn (ADR-020(d)): caller calls [Host.Spawn] →
// a series of RPCs → [Plugin.Close]. No connection pooling.
type Plugin struct {
	*sharedhost.BasePlugin
	client pluginv1.SoulModuleClient
}

// newPluginFromBase wraps the generic [sharedhost.BasePlugin] into a Soul-side
// kind-specific [Plugin]. Used only from [Host.Spawn] — no public constructor
// (callers must not spawn a BasePlugin themselves).
func newPluginFromBase(base *sharedhost.BasePlugin) *Plugin {
	return &Plugin{
		BasePlugin: base,
		client:     pluginv1.NewSoulModuleClient(base.Conn()),
	}
}

// errKindMismatch is the Spawn error when wrapping a plugin of the wrong kind
// into a kind-specific wrapper. Indicates a bad Discovered construction (e.g.
// in a test) or drift between the Discover filter and the plugin's manifest.
func errKindMismatch(want, got string) error {
	return fmt.Errorf("pluginhost: expected kind=%s, got kind=%q", want, got)
}

// Validate calls RPC SoulModule.Validate. Errors pass through unwrapped —
// wrapping into TaskError is the apply cycle's job (Core.b / M2.1.b.3).
func (p *Plugin) Validate(ctx context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return p.client.Validate(ctx, req)
}

// Plan calls RPC SoulModule.Plan. Returns a stream — caller reads and
// aggregates PlanEvents itself.
func (p *Plugin) Plan(ctx context.Context, req *pluginv1.PlanRequest) (grpc.ServerStreamingClient[pluginv1.PlanEvent], error) {
	return p.client.Plan(ctx, req)
}

// Apply calls RPC SoulModule.Apply. Returns a stream — caller reads all
// ApplyEvents (the last one is final, carrying changed/failed/output per
// docs/destiny/tasks.md).
func (p *Plugin) Apply(ctx context.Context, req *pluginv1.ApplyRequest) (grpc.ServerStreamingClient[pluginv1.ApplyEvent], error) {
	return p.client.Apply(ctx, req)
}
