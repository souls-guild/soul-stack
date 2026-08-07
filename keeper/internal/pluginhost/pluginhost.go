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
//
// [Document] replaced the manifest in NIM-377: an artifact carries a generated schema
// document in its trailer and no self-name at all. A Discovered entry is therefore one
// addressable MODULE, keyed by the registration alias, not one file.
type (
	Discovered = sharedhost.Discovered
	Document   = sharedplugin.Document
	Kind       = sharedplugin.Kind
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
// Protection from kind-mismatch: if the artifact's kind is not in {cloud_driver,
// ssh_provider}, Spawn returns an error before the fork.
//
// opts are optional SpawnOptions ([WithEnv] etc.); passed through to
// [sharedhost.Host.Spawn] unchanged.
func (h *Host) Spawn(ctx context.Context, d Discovered, opts ...SpawnOption) (*Plugin, error) {
	if d.Doc != nil &&
		d.Kind() != KindCloudDriver &&
		d.Kind() != KindSSHProvider {
		return nil, fmt.Errorf("pluginhost: expected kind=cloud_driver|ssh_provider, got %q", d.Kind())
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
//	  <alias>/                        # the registration alias, address level 1
//	    current -> <commit_sha>       # symlink to the active slot
//	    <commit_sha>/
//	      <artifact>                  # exactly one executable, schema in its trailer
//
// The slot directory's name is the REGISTRATION ALIAS the operator chose, and the
// artifact inside has no name convention at all: it carries no self-name since
// NIM-377, so the host takes the slot's single executable and reads what it offers
// from the trailer. Two publishers of the same subject cannot collide — the operator
// picks both aliases.
//
// Discovery goes through `current` (one-level symlink resolution): for each directory
// `<alias>` it reads `<alias>/current/`, passing the alias down so address level 1
// comes from the registration and never from the symlink's basename (which is a commit
// sha and says nothing about the registration). Directories without a valid `current`
// (the resolver has not populated the slot yet) go to warnings.
//
// Cache population by the git-resolver (`plugins.{cloud_drivers,ssh_providers,
// soul_modules}` → commit_sha-slot) happens in [plugingit.Resolver] before Discover on
// Keeper startup; [FilterByCatalog] then filters the result against the catalog.
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
		alias := e.Name()
		current := filepath.Join(cacheRoot, alias, CurrentLink)
		if _, statErr := os.Stat(current); statErr != nil {
			warnings = append(warnings, fmt.Sprintf("skip %s: no active slot (current): %v",
				filepath.Join(cacheRoot, alias), statErr))
			continue
		}
		found, warns := sharedhost.DiscoverSlot(alias, current)
		all = append(all, found...)
		warnings = append(warnings, warns...)
	}
	keeperOnly, filterWarns := sharedhost.FilterByKinds(all, []Kind{KindCloudDriver, KindSSHProvider, KindSoulModule})
	return keeperOnly, append(warnings, filterWarns...), nil
}

// FilterByCatalog keeps in `found` only entries whose registration ALIAS is declared in
// `keeper.yml::plugins.{cloud_drivers,ssh_providers,soul_modules}`. The comparison is
// against `PluginCatalogEntry.Name`, which IS the alias (the field kept its name until
// the real registry lands in NIM-437).
//
// The alias is now the only thing to compare: the artifact declares no name of its own,
// so there is nothing else that could be matched against a catalog entry, and matching
// on the artifact's contents would mean the catalog no longer says which registration
// it authorized.
//
// Returns the filtered list and a warning list:
//
//   - catalog entry with no discovered slot → warning;
//   - discovered slot with no catalog entry → warning.
//
// Because one artifact yields one entry PER MODULE, several entries may share an alias;
// they are accepted or rejected together, and a catalog entry counts as satisfied by
// the first of them.
func FilterByCatalog(found []Discovered, plugins *config.KeeperPlugins) ([]Discovered, []string) {
	if plugins == nil {
		return nil, nil
	}
	// Index declared aliases by kind to validate both lists in one pass over found.
	// Sets are empty for a nil block.
	wantCloud := indexEntries(plugins.CloudDrivers)
	wantSSH := indexEntries(plugins.SSHProviders)
	wantModules := indexEntries(plugins.SoulModules)

	// Catalog key per kind — the single point of correspondence kind → yaml-list.
	catalogKey := map[Kind]string{
		KindCloudDriver: "cloud_drivers",
		KindSSHProvider: "ssh_providers",
		KindSoulModule:  "soul_modules",
	}
	want := map[Kind]map[string]struct{}{
		KindCloudDriver: wantCloud,
		KindSSHProvider: wantSSH,
		KindSoulModule:  wantModules,
	}

	var (
		out      []Discovered
		warnings []string
	)
	seen := map[Kind]map[string]bool{
		KindCloudDriver: make(map[string]bool, len(wantCloud)),
		KindSSHProvider: make(map[string]bool, len(wantSSH)),
		KindSoulModule:  make(map[string]bool, len(wantModules)),
	}
	warned := make(map[string]bool, len(found))
	for _, d := range found {
		kind := d.Kind()
		wantAliases, ok := want[kind]
		if !ok {
			continue
		}
		if _, declared := wantAliases[d.Alias]; declared {
			out = append(out, d)
			seen[kind][d.Alias] = true
			continue
		}
		// One warning per undeclared ALIAS, not per module: an artifact serving
		// three modules is one registration the operator forgot to declare.
		key := string(kind) + "/" + d.Alias
		if !warned[key] {
			warned[key] = true
			warnings = append(warnings, fmt.Sprintf(
				"plugin %s (kind=%s) not declared in keeper.yml::plugins.%s",
				d.Alias, kind, catalogKey[kind]))
		}
	}
	for _, kind := range []Kind{KindCloudDriver, KindSSHProvider, KindSoulModule} {
		for alias := range want[kind] {
			if !seen[kind][alias] {
				warnings = append(warnings, fmt.Sprintf(
					"keeper.yml::plugins.%s[name=%s] declared but no artifact found in cache",
					catalogKey[kind], alias))
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
