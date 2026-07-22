package beacon

import (
	"context"
	"fmt"
	"log/slog"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"google.golang.org/protobuf/types/known/structpb"
)

// PluginBeaconSpawner — narrow contract over pluginhost.Host for a one-shot
// per-tick beacon-plugin spawn. In production it wraps *pluginhost.Host
// (Soul-side, kind=soul_beacon); in tests it's a fake. Decoupling keeps the
// beacon package independent of the pluginhost import (avoids an import
// cycle and host deps in the scheduler's unit tests).
type PluginBeaconSpawner interface {
	SpawnBeacon(ctx context.Context, d sharedhost.Discovered) (PluginBeaconSession, error)
}

// PluginBeaconSession — narrow contract over *pluginhost.BeaconPlugin for a
// single Check call. Parallels [soul/internal/runtime.PluginSession] for
// SoulModule.
type PluginBeaconSession interface {
	Validate(ctx context.Context, req *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error)
	Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error)
	Close() error
}

// PluginRegistry — the [BeaconLookup] implementation over custom beacon
// plugins found by pluginhost.Discover (kind=soul_beacon). Lookup returns a
// [pluginBeacon] wrapper that does Spawn → Check → Close on every Check.
//
// Concurrency: read-only after construction, safe for concurrent Lookups.
// Spawn sessions are independent — Host serializes socket creation via an
// atomic counter (shared/pluginhost).
type PluginRegistry struct {
	spawner PluginBeaconSpawner
	beacons map[string]sharedhost.Discovered
	logger  *slog.Logger
}

// NewPluginRegistry builds the registry. discovered is the list of
// kind=soul_beacon plugins (caller already filtered by `d.Manifest.Kind`).
// The key name is `<namespace>.<name>` (manifest.Address()), matching
// VigilDef.check for plugin-beacon addresses.
func NewPluginRegistry(spawner PluginBeaconSpawner, discovered []sharedhost.Discovered, logger *slog.Logger) *PluginRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	beacons := make(map[string]sharedhost.Discovered, len(discovered))
	for _, d := range discovered {
		if d.Manifest == nil || d.Manifest.Kind != sharedplugin.KindSoulBeacon {
			continue
		}
		beacons[d.Manifest.Address()] = d
	}
	return &PluginRegistry{spawner: spawner, beacons: beacons, logger: logger}
}

// Names — the list of registered plugin-beacon addresses.
func (r *PluginRegistry) Names() []string {
	out := make([]string, 0, len(r.beacons))
	for k := range r.beacons {
		out = append(out, k)
	}
	return out
}

// Lookup returns a per-Vigil wrapper implementing [Beacon]. Each Check inside
// the wrapper does a one-shot Spawn → Check → Close (ADR-020(d), parity with
// pluginSoulModule in runtime/pluginregistry.go).
func (r *PluginRegistry) Lookup(name string) (Beacon, bool) {
	d, ok := r.beacons[name]
	if !ok {
		return nil, false
	}
	return &pluginBeacon{
		discovered: d,
		spawner:    r.spawner,
		logger:     r.logger,
	}, true
}

// pluginBeacon — a one-shot spawn adapter implementing [Beacon]. Implements
// only Check; the scheduler never calls Validate directly (manifest
// validation happens when the operator creates the Vigil via OpenAPI).
type pluginBeacon struct {
	discovered sharedhost.Discovered
	spawner    PluginBeaconSpawner
	logger     *slog.Logger
}

// Check does a one-shot Spawn → SoulBeacon.Check → Close. The returned
// state/payload/error are passed through to the scheduler; a non-fatal
// CheckReply.error (plugin-side soft error) is translated into a Go error so
// the scheduler skips the tick (baseline untouched, parity with a built-in
// [Beacon].Check err).
func (p *pluginBeacon) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	sess, err := p.spawner.SpawnBeacon(ctx, p.discovered)
	if err != nil {
		return "", nil, fmt.Errorf("plugin_spawn: %w", err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			p.logger.Warn("beacon: plugin close error",
				slog.String("beacon", p.discovered.Manifest.Address()),
				slog.Any("error", cerr),
			)
		}
	}()
	reply, err := sess.Check(ctx, &pluginv1.CheckRequest{Params: params})
	if err != nil {
		return "", nil, fmt.Errorf("plugin_check_rpc: %w", err)
	}
	if reply.GetError() != "" {
		// Soft error from the plugin — scheduler will skip the tick. Raise it
		// as a Go error (not as state) so baseline doesn't move.
		return "", nil, fmt.Errorf("plugin_check_soft: %s", reply.GetError())
	}
	return reply.GetState(), reply.GetPayload(), nil
}
