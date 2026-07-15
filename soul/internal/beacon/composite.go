package beacon

import "log/slog"

// CompositeRegistry joins the core (static) and plugin (resolver-over-
// pluginhost) beacon sets for the scheduler. Lookup checks core first (so a
// plugin can't shadow `core.beacon.*` names), then plugin. Mirrors
// [soul/internal/runtime.CompositeRegistry] for SoulModule.
//
// PluginLookup is a separate interface so composite doesn't depend on
// pluginhost; wiring happens in cmd/soul.
type CompositeRegistry struct {
	core   BeaconLookup
	plugin BeaconLookup
	logger *slog.Logger
}

// NewCompositeRegistry builds the registry. Either branch may be nil (e.g. in
// push mode there's no plugin discovery — pass nil); Lookup skips a nil
// branch.
//
// A core ↔ plugin name clash shouldn't normally happen: plugin names resolve
// as `<namespace>.<name>` (e.g. `community.zfs-degraded`), core as
// `core.beacon.<name>`. But the check order still guards against it: core is
// always checked first, even if a name is manually duplicated.
func NewCompositeRegistry(core, plugin BeaconLookup, logger *slog.Logger) *CompositeRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &CompositeRegistry{core: core, plugin: plugin, logger: logger}
}

// Lookup searches layers sequentially. Returns the first match.
func (c *CompositeRegistry) Lookup(name string) (Beacon, bool) {
	if c.core != nil {
		if b, ok := c.core.Lookup(name); ok {
			return b, true
		}
	}
	if c.plugin != nil {
		if b, ok := c.plugin.Lookup(name); ok {
			return b, true
		}
	}
	return nil, false
}
