package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/pluginhost"
	"google.golang.org/grpc"
)

// PluginSpawner is a narrow contract over pluginhost.Host, used by Registry
// to lazily spawn sub-process plugins. Implemented by *Host in production;
// a fake in tests to avoid launching real binaries.
type PluginSpawner interface {
	Spawn(ctx context.Context, d pluginhost.Discovered) (PluginSession, error)
}

// PluginSession is a narrow contract over *pluginhost.Plugin for one Apply
// call. Declared here so tests can substitute it without depending on a
// network connection or a subprocess.
type PluginSession interface {
	Apply(ctx context.Context, req *pluginv1.ApplyRequest) (grpc.ServerStreamingClient[pluginv1.ApplyEvent], error)
	Close() error
}

// PluginHostSpawner wraps *pluginhost.Host to satisfy PluginSpawner. Exists
// to adapt the type: Host.Spawn returns *pluginhost.Plugin, while Registry
// operates on the PluginSession interface.
type PluginHostSpawner struct {
	Host *pluginhost.Host
}

func (s PluginHostSpawner) Spawn(ctx context.Context, d pluginhost.Discovered) (PluginSession, error) {
	p, err := s.Host.Spawn(ctx, d)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// PluginRegistry implements Registry over custom modules found by
// pluginhost.Discover. Lookup returns a pluginSoulModule wrapper that does
// Spawn → Apply → Close on every Apply call (one-shot, ADR-020(d)).
//
// Concurrency: mods is guarded by an RWMutex — Rescan (hot-register from
// core.module.installed, ADR-065(d)) runs concurrently with Lookup from an
// in-flight run. Spawn sessions are independent of each other — Host itself
// serializes socket creation via an atomic counter.
type PluginRegistry struct {
	spawner PluginSpawner
	logger  *slog.Logger

	mu   sync.RWMutex
	mods map[string]pluginhost.Discovered
}

// NewPluginRegistry builds the registry. The key is `<namespace>.<name>`
// (manifest.Address()) — matches what arrives in RenderedTask.module before
// the state suffix. Discovered entries with kind != soul_module are skipped
// (defensive: the Soul-host Discover can also return soul_beacon, ADR-030
// V5-2 — those get registered by a separate beacon-PluginRegistry).
func NewPluginRegistry(spawner PluginSpawner, discovered []pluginhost.Discovered, logger *slog.Logger) *PluginRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &PluginRegistry{spawner: spawner, mods: indexSoulModules(discovered), logger: logger}
}

func indexSoulModules(discovered []pluginhost.Discovered) map[string]pluginhost.Discovered {
	mods := make(map[string]pluginhost.Discovered, len(discovered))
	for _, d := range discovered {
		if d.Manifest == nil || d.Manifest.Kind != pluginhost.KindSoulModule {
			continue
		}
		mods[d.Manifest.Address()] = d
	}
	return mods
}

// Rescan is hot-register (ADR-065(d)): a full re-discover of the module
// directory and an atomic swap of the custom-module set without restarting
// the daemon. Returns discovery warnings for the caller to log (same style
// as startup). The beacon registry is NOT rebuilt on Rescan — MVP
// limitation per ADR-065; hot-reload for soul_beacon is post-MVP.
func (r *PluginRegistry) Rescan(modulesRoot string) ([]string, error) {
	discovered, warnings, err := pluginhost.Discover(modulesRoot)
	if err != nil {
		return warnings, err
	}
	mods := indexSoulModules(discovered)
	r.mu.Lock()
	r.mods = mods
	r.mu.Unlock()
	return warnings, nil
}

// Names returns the list of registered custom modules.
func (r *PluginRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.mods))
	for k := range r.mods {
		out = append(out, k)
	}
	return out
}

// Lookup returns a SoulModule wrapper that does a one-shot spawn on every
// Apply. The returned module.SoulModule implements only Apply; Validate/Plan
// return BaseModule defaults (the MVP apply loop doesn't call them).
func (r *PluginRegistry) Lookup(name string) (module.SoulModule, bool) {
	r.mu.RLock()
	d, ok := r.mods[name]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return &pluginSoulModule{
		discovered: d,
		spawner:    r.spawner,
		logger:     r.logger,
	}, true
}

// pluginSoulModule adapts one-shot spawning to sdk/module.SoulModule. An
// Apply call does spawn → Apply (stream) → forwards ApplyEvents into the
// caller's stream → Close. Any stage error becomes an error for the runner
// (which turns it into TaskEvent.failed=true).
type pluginSoulModule struct {
	module.BaseModule
	discovered pluginhost.Discovered
	spawner    PluginSpawner
	logger     *slog.Logger
}

func (m *pluginSoulModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	sess, err := m.spawner.Spawn(ctx, m.discovered)
	if err != nil {
		return fmt.Errorf("plugin_spawn: %w", err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			m.logger.Warn("plugin: close error",
				slog.String("module", m.discovered.Manifest.Address()),
				slog.Any("error", cerr),
			)
		}
	}()

	rpcStream, err := sess.Apply(ctx, req)
	if err != nil {
		return fmt.Errorf("plugin_apply_rpc: %w", err)
	}
	for {
		ev, err := rpcStream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("plugin_apply_stream: %w", err)
		}
		if sendErr := stream.Send(ev); sendErr != nil {
			return fmt.Errorf("plugin_apply_forward: %w", sendErr)
		}
	}
}

// CompositeRegistry is a Registry that checks lookups sequentially across a
// list. Used to combine core + plugin: core takes priority so a custom
// module with a conflicting name (e.g. `core.pkg`) can't shadow static core.
// Conflicts are logged in Names() via a log call from the cmd/soul
// constructor.
type CompositeRegistry struct {
	layers []Registry
}

// NewCompositeRegistry is order-dependent: the first layer is checked first.
func NewCompositeRegistry(layers ...Registry) *CompositeRegistry {
	return &CompositeRegistry{layers: layers}
}

func (c *CompositeRegistry) Lookup(name string) (module.SoulModule, bool) {
	for _, l := range c.layers {
		if m, ok := l.Lookup(name); ok {
			return m, true
		}
	}
	return nil, false
}
