package pluginhost

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/schema"
)

// SoulModulePlugin is the Keeper-side handle for a spawned kind=soul_module
// module — the SAME contract the Soul host runs (ADR-009 / ADR-017: one
// SoulModule interface for both sides), started inside the Keeper's own process
// tree instead of on a host. Embeds the generic [Plugin] and adds the SoulModule
// gRPC client, exactly as [CloudDriverPlugin] / [SshProviderPlugin] do for their
// own contracts.
//
// Lifecycle is one-shot per spawn (ADR-020(d)), like every other kind here:
// [Host.SpawnSoulModule] → Apply → Close. No connection pooling.
type SoulModulePlugin struct {
	*Plugin
	client pluginv1.SoulModuleClient
}

// SpawnSoulModule forks the artifact for one kind=soul_module module and wraps
// it in a SoulModule client. Separate from [Host.Spawn] (cloud_driver /
// ssh_provider) because each kind gets its own wrap function on this host, and
// because this one carries a gate the other two have no analogue of.
//
// ★ A plugin on the Keeper is the most privileged way to execute anything the
// platform has — a foreign binary inside the Keeper process tree rather than on
// a host — so this door is the neighbouring one plus a bolt, never less. Two
// refusals happen BEFORE the fork, both fail-closed:
//
//   - kind != soul_module — the [Discovered] was built wrong (by hand in a test,
//     or by a Discover filter that drifted from the artifact's document);
//   - the module does not declare `side: keeper` ([ADR-0087]) — running a
//     Soul-side module inside the Keeper is precisely the confusion this path
//     must make impossible, and the check sits here as well as in the dispatcher
//     so a wrong caller cannot reach the fork either.
//
// The side declaration is trustworthy for the same reason the capabilities are:
// it lives INSIDE the schema document, and the Sigil seal signs the document
// together with the binary's sha256 (ADR-026(c)) — it cannot be flipped without
// breaking the signature [sharedhost.Host.Spawn] verifies a moment later. That
// verification, the capability check and the handshake validation are unchanged
// and shared with every other kind: this adds a gate, it removes none.
func (h *Host) SpawnSoulModule(ctx context.Context, d Discovered, opts ...SpawnOption) (*SoulModulePlugin, error) {
	if d.Kind() != KindSoulModule {
		return nil, fmt.Errorf("pluginhost: expected kind=soul_module, got %q", d.Kind())
	}
	if side := DeclaredSide(d); side != schema.SideKeeper {
		return nil, fmt.Errorf("pluginhost: %s: module declares side=%s, only side=keeper executes on the keeper", d.Address(), side)
	}
	base, err := h.Host.Spawn(ctx, d, opts...)
	if err != nil {
		return nil, err
	}
	return &SoulModulePlugin{
		Plugin: &Plugin{BasePlugin: base},
		client: pluginv1.NewSoulModuleClient(base.Conn()),
	}, nil
}

// Apply calls RPC SoulModule.Apply and returns the event stream — the caller
// reads it and folds the events itself (the last one is final, carrying
// changed/failed/output per docs/destiny/tasks.md), same as the Soul host.
func (p *SoulModulePlugin) Apply(ctx context.Context, req *pluginv1.ApplyRequest) (grpc.ServerStreamingClient[pluginv1.ApplyEvent], error) {
	return p.client.Apply(ctx, req)
}

// DeclaredSide is the side d's module declares, with the zero value read as
// [schema.SideSoul] — the absent key and `side: soul` are indistinguishable by
// design, permanently, because documents are signed and stamping the default in
// later would change every artifact's sha256 ([ADR-0087]).
//
// An entry with no module declaration at all (a single-endpoint kind, or a
// module the document does not describe) also answers `soul`: the question this
// answers is "may the Keeper run it", and everything that has not said `keeper`
// must be told no.
func DeclaredSide(d Discovered) schema.Side {
	m, ok := d.ModuleDef()
	if !ok || m.Side == "" {
		return schema.SideSoul
	}
	return m.Side
}
