// Package pluginhost is the Soul-side wrapper over `shared/pluginhost` for
// running plugins of kind ∈ {soul_module, soul_beacon} (ADR-020 + ADR-030
// V5-2, docs/keeper/plugins.md).
//
// The kind-agnostic parts (Spawn / handshake / Close / discovery /
// tailBuffer) live in [sharedhost]. This package adds:
//
//   - kind-specific wrappers: [Plugin] (SoulModule gRPC client) and
//     [BeaconPlugin] (SoulBeacon gRPC client);
//   - kind-specific default SocketDir (`/var/run/soul-stack/plugins`);
//   - a Discover filter: the Soul host accepts kind=soul_module and
//     kind=soul_beacon (cloud/ssh are Keeper-side, filtered into warnings).
//
// manifest.yaml parsing lives in `shared/plugin`; this package only
// re-exports type aliases to avoid breaking existing call sites.
package pluginhost

import (
	"context"
	"crypto/ed25519"

	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// DefaultSocketDir is the Soul host's default plugin Unix-socket directory.
// Differs from the Keeper-host default since the keeper service runs under a
// separate user (docs/keeper/plugins.md → socket location).
const DefaultSocketDir = "/var/run/soul-stack/plugins"

// Defaults re-exported from shared for call-site convenience (legacy tests
// referenced `pluginhost.DefaultStartupTimeout`).
const (
	DefaultStartupTimeout = sharedhost.DefaultStartupTimeout
	DefaultShutdownGrace  = sharedhost.DefaultShutdownGrace
)

// Re-export of shared types. Deliberate aliases: they give call sites the
// familiar short names (`pluginhost.Discovered`, `pluginhost.Manifest`) and a
// stable Soul-host contract surface.
type (
	Discovered = sharedhost.Discovered
	Manifest   = sharedplugin.Manifest
)

// Host is the Soul-side runtime for kind=soul_module plugins. A thin wrapper
// over [sharedhost.Host]: overrides Spawn to return [Plugin] instead of the
// generic [sharedhost.BasePlugin], and rejects kind != soul_module.
type Host struct {
	*sharedhost.Host
}

// Spawn forks a kind=soul_module plugin and wraps [sharedhost.BasePlugin] in
// the kind-specific [Plugin] (SoulModule client). Errors if
// Discovered.Manifest.Kind != soul_module (guards against kind mismatch when
// Discovered is hand-built in tests). See [Host.SpawnBeacon] for kind=soul_beacon.
func (h *Host) Spawn(ctx context.Context, d Discovered) (*Plugin, error) {
	if d.Manifest != nil && d.Manifest.Kind != KindSoulModule {
		return nil, errKindMismatch(KindSoulModule, d.Manifest.Kind)
	}
	base, err := h.Host.Spawn(ctx, d)
	if err != nil {
		return nil, err
	}
	return newPluginFromBase(base), nil
}

// SpawnBeacon forks a kind=soul_beacon plugin and wraps [sharedhost.BasePlugin]
// in [BeaconPlugin] (SoulBeacon client). Mirrors [Host.Spawn] for the Soul
// host's second kind (ADR-030 V5-2).
//
// Kind-mismatch guard: errors before forking if manifest.kind != soul_beacon
// (symmetric with the Keeper host, where each kind gets its own wrap function).
func (h *Host) SpawnBeacon(ctx context.Context, d Discovered) (*BeaconPlugin, error) {
	if d.Manifest != nil && d.Manifest.Kind != KindSoulBeacon {
		return nil, errKindMismatch(KindSoulBeacon, d.Manifest.Kind)
	}
	base, err := h.Host.Spawn(ctx, d)
	if err != nil {
		return nil, err
	}
	return newBeaconFromBase(base), nil
}

// Kind constants for the Soul host. Soul accepts kind=soul_module and
// kind=soul_beacon (ADR-030 V5-2); the rest are re-exported for use in tests
// and validation logic.
const (
	KindSoulModule  = sharedplugin.KindSoulModule
	KindCloudDriver = sharedplugin.KindCloudDriver
	KindSSHProvider = sharedplugin.KindSSHProvider
	KindSoulBeacon  = sharedplugin.KindSoulBeacon
)

// SupportedProtocolVersions lists the plugin-protocol versions the Soul host
// understands. Delegates to shared/plugin as the single source of truth.
var SupportedProtocolVersions = sharedplugin.SupportedProtocolVersions

// NewHost builds the Soul host. Takes `soul.yml::plugin_runtime` and falls
// back to [DefaultSocketDir] when cfg.SocketDir is empty.
//
// anchors is the SET of Sigil verification trust anchors (ADR-026(h), R3
// multi-anchor) from the SoulSeed (a parsed sigil_pubkey.pem may carry
// several PEM blocks, see [seed.ParseSigilPubKeys]). An empty set means Sigil
// isn't configured on the Keeper, so verifying any custom plugin fails closed
// (no_trust_anchor). OR-matching over the set allows seamless signing-key
// rotation. sigils is the read surface for the runtime allowlist cache
// ([SigilLookupAdapter]); nil means no allowlist, failing closed (no_sigil).
// Both are injected into [sharedhost.Host]; the anchor set is wrapped in the
// atomic [sharedhost.AnchorSet] (S6 will swap it at runtime via a
// SigilTrustAnchors message, without restarting Soul).
func NewHost(cfg *config.PluginRuntime, anchors []ed25519.PublicKey, sigils sharedhost.SigilLookup) (*Host, error) {
	base, err := sharedhost.NewHost(cfg, DefaultSocketDir)
	if err != nil {
		return nil, err
	}
	base.SigilAnchors = sharedhost.NewAnchorSet(anchors)
	base.Sigils = sigils
	return &Host{Host: base}, nil
}

// Discover is Soul-host discovery: scans modulesRoot and keeps only
// kind=soul_module and kind=soul_beacon (ADR-030 V5-2). Cloud/SSH plugins and
// invalid entries go into warnings (caller logs them).
//
// Cache layout (docs/soul/modules.md):
//
//	/var/lib/soul-stack/modules/
//	  <namespace>-<name>/
//	    manifest.yaml
//	    soul-mod-<name>        # for kind=soul_module
//	    soul-beacon-<name>     # for kind=soul_beacon
//
// External contract is unchanged: `[]Discovered, []string, error`. The caller
// splits the result by kind (via `d.Manifest.Kind`) and registers into the
// appropriate registry (module-registry / beacon-registry).
func Discover(modulesRoot string) ([]Discovered, []string, error) {
	all, warnings, err := sharedhost.Discover(modulesRoot)
	if err != nil {
		return nil, nil, err
	}
	soulOnly, filterWarns := sharedhost.FilterByKinds(all, []string{KindSoulModule, KindSoulBeacon})
	return soulOnly, append(warnings, filterWarns...), nil
}
