// Package pluginhost provides Keeper-side wrapper over `shared/pluginhost` for running
// plugins of kind=cloud_driver and kind=ssh_provider (ADR-020, docs/keeper/plugins.md).
//
// The generic kind-agnostic part (Spawn / handshake / Close / discovery / tailBuffer)
// lives in [sharedhost]. This package adds:
//
//   - kind-specific wrappers [CloudDriverPlugin], [SshProviderPlugin], common private
//     [Plugin] with gRPC-conn;
//   - kind-specific default SocketDir (`/var/run/soul-stack-keeper/plugins`);
//   - Discover-result filter: Keeper-host accepts cloud_driver, ssh_provider and
//     soul_module (the latter is a registry for distribution to Souls, epic core.module.installed;
//     Spawn rejects it);
//   - [FilterByCatalog] for cross-check of discovered plugins against catalog in
//     `keeper.yml::plugins.{cloud_drivers,ssh_providers,soul_modules}`.
package pluginhost

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// DefaultSocketDir is the Keeper-host default Unix socket directory for plugins.
// Differs from Soul-host default: keeper service runs as separate user
// (docs/keeper/plugins.md → Socket Location).
const DefaultSocketDir = "/var/run/soul-stack-keeper/plugins"

// DefaultCacheRoot is the convention for Keeper-side plugin cache directory
// (ADR-020(a), symmetric with [DefaultSocketDir]). Used by main's wire-up when
// `keeper.yml` doesn't specify explicit path (cache field in config schema
// not yet introduced — git-resolve for `plugins.{cloud_drivers,ssh_providers}` is separate task).
const DefaultCacheRoot = "/var/lib/soul-stack-keeper/plugins"

// Defaults re-exported from shared for call-sites convenience.
const (
	DefaultStartupTimeout = sharedhost.DefaultStartupTimeout
	DefaultShutdownGrace  = sharedhost.DefaultShutdownGrace
)

// Re-export types from shared. Aliases intentional: they provide call-sites familiar
// short names and stable contract surface for Keeper-host.
type (
	Discovered = sharedhost.Discovered
	Manifest   = sharedplugin.Manifest
)

// Kind-constants for Keeper-host.
const (
	KindSoulModule  = sharedplugin.KindSoulModule
	KindCloudDriver = sharedplugin.KindCloudDriver
	KindSSHProvider = sharedplugin.KindSSHProvider
)

// SupportedProtocolVersions is plugin-protocol versions understood by Keeper-host.
// Delegated to shared/plugin as single source of truth.
var SupportedProtocolVersions = sharedplugin.SupportedProtocolVersions

// Host is Keeper-side runtime for plugins where kind ∈ {cloud_driver, ssh_provider}.
// Thin wrapper over [sharedhost.Host] with kind-specific Spawn methods.
type Host struct {
	*sharedhost.Host
}

// NewHost constructs Keeper-host. Accepts `keeper.yml::plugin_runtime` and
// substitutes [DefaultSocketDir] if cfg.SocketDir is empty.
//
// anchors is a SET of trust-anchors for Sigil verification (ADR-026(h), R3 multi-anchor):
// public keys of all active keeper-Signer keys ([sigil.Signer.AnchorSet]).
// keeper-host verifies its own plugins against seals it signed itself
// (ADR-026(f)), so anchor set is the active signing set; OR-check enables seamless key rotation.
// Empty set = Sigil not configured on Keeper → verification of any plugin fail-closed (no_trust_anchor):
// operator with cloud/ssh must configure Sigil + allow. sigils is the surface for reading active permissions
// ([SigilLookupAdapter] over plugin_sigils registry); nil = no permissions →
// fail-closed (no_sigil). Both are passed into [sharedhost.Host] as DI;
// set wrapped in atomic [sharedhost.AnchorSet] (S6 will replace it at runtime).
func NewHost(cfg *config.PluginRuntime, anchors []ed25519.PublicKey, sigils sharedhost.SigilLookup) (*Host, error) {
	base, err := sharedhost.NewHost(cfg, DefaultSocketDir)
	if err != nil {
		return nil, err
	}
	base.SigilAnchors = sharedhost.NewAnchorSet(anchors)
	base.Sigils = sigils
	return &Host{Host: base}, nil
}

// SpawnOption is alias to [sharedhost.SpawnOption] for call-site convenience
// (keeper-side caller doesn't pull shared-import for one type).
type SpawnOption = sharedhost.SpawnOption

// WithEnv re-exports [sharedhost.WithEnv] (see doc there). Used by push-S6
// wire-up of SshDispatcher for env-payload params of SshProvider plugin
// (ADR-020 amendment l).
func WithEnv(env []string) SpawnOption { return sharedhost.WithEnv(env) }

// Spawn forks plugin and returns generic [sharedhost.BasePlugin]. Caller
// wraps result in kind-specific [CloudDriverPlugin] / [SshProviderPlugin]
// via [NewCloudDriverPlugin] / [NewSshProviderPlugin] — Keeper-host
// distinguishes two kinds, so intermediate generic Plugin makes choice
// explicit rather than implicit.
//
// Protection from kind-mismatch: if manifest.kind not in {cloud_driver, ssh_provider},
// Spawn returns error before fork.
//
// opts are optional SpawnOptions ([WithEnv] etc.); passed through to
// [sharedhost.Host.Spawn] unchanged.
func (h *Host) Spawn(ctx context.Context, d Discovered, opts ...SpawnOption) (*Plugin, error) {
	if d.Manifest != nil &&
		d.Manifest.Kind != KindCloudDriver &&
		d.Manifest.Kind != KindSSHProvider {
		return nil, fmt.Errorf("pluginhost: expected kind=cloud_driver|ssh_provider, got %q", d.Manifest.Kind)
	}
	base, err := h.Host.Spawn(ctx, d, opts...)
	if err != nil {
		return nil, err
	}
	return &Plugin{BasePlugin: base}, nil
}

// Plugin is Keeper-side generic handle. Doesn't contain kind-specific gRPC client
// (unlike Soul-host where kind is singular): caller wraps Plugin in
// [CloudDriverPlugin] / [SshProviderPlugin] via NewCloudDriverPlugin /
// NewSshProviderPlugin.
type Plugin struct {
	*sharedhost.BasePlugin
}

// Discover performs Keeper-host discovery: searches for plugins in cacheRoot and keeps
// only kind ∈ {cloud_driver, ssh_provider, soul_module}. soul_module Keeper does
// not spawn ([Host.Spawn] rejects it) — keeps in registry for distribution
// to Souls (epic core.module.installed). Other kinds and invalid entries
// go to warnings.
//
// Cache layout (R-nested layout, A1-S1 — git-resolver populates slots):
//
//	<cacheRoot>/
//	  <namespace>-<name>/
//	    current -> <commit_sha>       # symlink to active slot
//	    <commit_sha>/
//	      manifest.yaml
//	      soul-cloud-<name>           # for kind=cloud_driver
//	      soul-ssh-<name>             # for kind=ssh_provider
//	      soul-mod-<name>             # for kind=soul_module
//
// Discovery goes through `current` (one-level symlink resolution): for each
// directory `<ns>-<name>` discovers `<ns>-<name>/current/`. Directories without
// valid `current` (resolver hasn't populated slot yet) go to warnings.
//
// Cache population by git-resolver (`plugins.{cloud_drivers,ssh_providers,
// soul_modules}` → commit_sha-slot) done by [plugingit.Resolver] before Discover
// on Keeper startup; [FilterByCatalog] filters found plugins by registry.
func Discover(cacheRoot string) ([]Discovered, []string, error) {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("pluginhost: read plugin cache root %q: %w", cacheRoot, err)
	}
	var (
		all      []sharedhost.Discovered
		warnings []string
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Active plugin slot — via current-symlink. sharedhost.Discover
		// reads manifest+binary from passed directory; current points to
		// commit_sha-slot with this layout.
		current := filepath.Join(cacheRoot, e.Name(), CurrentLink)
		if _, statErr := os.Stat(current); statErr != nil {
			warnings = append(warnings, fmt.Sprintf("skip %s: no active slot (current): %v",
				filepath.Join(cacheRoot, e.Name()), statErr))
			continue
		}
		found, warns := sharedhost.DiscoverSlot(current)
		all = append(all, found...)
		warnings = append(warnings, warns...)
	}
	keeperOnly, filterWarns := sharedhost.FilterByKinds(all, []string{KindCloudDriver, KindSSHProvider, KindSoulModule})
	return keeperOnly, append(warnings, filterWarns...), nil
}

// FilterByCatalog keeps in `found` only plugins whose `manifest.name`
// is mentioned in catalog `keeper.yml::plugins.{cloud_drivers,ssh_providers,
// soul_modules}`. Comparison by field `name` (PluginCatalogEntry.Name) —
// same kebab-case as `manifest.name`.
//
// Returns filtered list and warnings list:
//
//   - catalog entry without discovered plugin → warning;
//   - discovered plugin without catalog entry → warning.
//
// Catalog `source`/`ref` themselves not used by this filter — git-resolve
// is separate task (see [Discover]).
func FilterByCatalog(found []Discovered, plugins *config.KeeperPlugins) ([]Discovered, []string) {
	if plugins == nil {
		return nil, nil
	}
	// Index declared names by kind to validate both lists in one pass over found.
	// Sets empty if nil-block.
	wantCloud := indexEntries(plugins.CloudDrivers)
	wantSSH := indexEntries(plugins.SSHProviders)
	wantModules := indexEntries(plugins.SoulModules)

	// Catalog key per kind — single point of correspondence kind → yaml-list.
	catalogKey := map[string]string{
		KindCloudDriver: "cloud_drivers",
		KindSSHProvider: "ssh_providers",
		KindSoulModule:  "soul_modules",
	}
	want := map[string]map[string]struct{}{
		KindCloudDriver: wantCloud,
		KindSSHProvider: wantSSH,
		KindSoulModule:  wantModules,
	}

	var (
		out      []Discovered
		warnings []string
	)
	seen := map[string]map[string]bool{
		KindCloudDriver: make(map[string]bool, len(wantCloud)),
		KindSSHProvider: make(map[string]bool, len(wantSSH)),
		KindSoulModule:  make(map[string]bool, len(wantModules)),
	}
	for _, d := range found {
		kind := d.Manifest.Kind
		wantNames, ok := want[kind]
		if !ok {
			continue
		}
		if _, declared := wantNames[d.Manifest.Name]; declared {
			out = append(out, d)
			seen[kind][d.Manifest.Name] = true
		} else {
			warnings = append(warnings, fmt.Sprintf(
				"plugin %s (kind=%s) not declared in keeper.yml::plugins.%s",
				d.Manifest.Address(), kind, catalogKey[kind]))
		}
	}
	for _, kind := range []string{KindCloudDriver, KindSSHProvider, KindSoulModule} {
		for name := range want[kind] {
			if !seen[kind][name] {
				warnings = append(warnings, fmt.Sprintf(
					"keeper.yml::plugins.%s[name=%s] declared but binary not found in cache",
					catalogKey[kind], name))
			}
		}
	}
	return out, warnings
}

func indexEntries(entries []config.PluginCatalogEntry) map[string]struct{} {
	if len(entries) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		m[e.Name] = struct{}{}
	}
	return m
}
