package pluginhost

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// BeaconPlugin is the Soul-side handle for a kind=soul_beacon plugin spawn
// session (ADR-030 V5-2). Embeds [sharedhost.BasePlugin] (manifest / conn /
// Close / StderrTail) and adds a SoulBeacon gRPC client for Validate / Check.
//
// Lifecycle is one-shot per Spawn (ADR-020(d)): the caller invokes
// [Host.SpawnBeacon] → a series of RPCs (usually one Check) → [BeaconPlugin.Close].
// The beacon scheduler wraps each per-tick Check in its own Spawn, so no
// connection pool is needed; optimizing for a long-lived process on frequent
// ticks is a separate task (see ADR-030 V5-2 spec, secondary item).
type BeaconPlugin struct {
	*sharedhost.BasePlugin
	client pluginv1.SoulBeaconClient
}

// newBeaconFromBase wraps the generic [sharedhost.BasePlugin] into the
// Soul-side kind-specific [BeaconPlugin]. Only used from [Host.SpawnBeacon] —
// there's no public constructor (callers shouldn't spawn a BasePlugin directly).
func newBeaconFromBase(base *sharedhost.BasePlugin) *BeaconPlugin {
	return &BeaconPlugin{
		BasePlugin: base,
		client:     pluginv1.NewSoulBeaconClient(base.Conn()),
	}
}

// Validate is the SoulBeacon.Validate RPC. Passed through to the caller
// without wrapping in TaskError — that's the scheduler's job (on a validate
// error the scheduler logs and doesn't start the Vigil, no baseline is set).
func (p *BeaconPlugin) Validate(ctx context.Context, req *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error) {
	return p.client.Validate(ctx, req)
}

// Check is the SoulBeacon.Check RPC. Returns state + payload + state_cookie.
// The scheduler compares state with the last per-Vigil value; on a change it
// raises a Portent.
func (p *BeaconPlugin) Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
	return p.client.Check(ctx, req)
}
